package main

import (
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi/cluster"
	"github.com/pkg/errors"
)

// twoStepChannelReconcileJobKey is the cluster job name for the periodic
// two-step / Team Admin visibility reconcile.
const twoStepChannelReconcileJobKey = "two_step_channel_reconcile"

// securityPoolMaxMembers caps usergroup / attribute security-pool listing so
// a misconfigured LDAP sync cannot expand into a runaway scan. Members past
// this cap can still approve (authz uses pool checks), but are not seeded into
// bot/webhook channels until login. Documented in docs/admin-guide.md.
const securityPoolMaxMembers = 75

// UserHasLoggedIn adds or evicts the user from bot/webhook approval channels
// based on current pool / admin status. Mattermost has no plugin hook for
// custom-group membership changes, so login + periodic reconcile cover joins
// and demotions. Also reconciles primary-team Team Admin visibility (seed on
// promote, evict from root/team/audit on demote) and per-team channel-requests
// membership after a secondary-team demotion.
func (p *Plugin) UserHasLoggedIn(_ *plugin.Context, user *model.User) {
	if user == nil || user.Id == "" || user.Id == p.botUserID {
		return
	}
	go func() {
		defer p.logRecoveredPanic("login access reconcile")
		p.reconcileTwoStepAccessForUser(user)
		p.reconcilePrimaryTeamAdminVisibilityForUser(user)
		p.reconcilePerTeamApprovalAccessForUser(user)
	}()
}

// startTwoStepChannelReconciler periodically adds current pool members to the
// bot/webhook channels and evicts demoted Team Admins from primary visibility
// channels and per-team channel-requests. Runs on one cluster node at a time.
func (p *Plugin) startTwoStepChannelReconciler() {
	p.stopTwoStepChannelReconciler()
	run := func() {
		defer p.logRecoveredPanic("two-step channel reconcile")
		p.reconcileTwoStepApproverChannels()
		p.evictStalePrimaryTeamAdminChannelMembers()
		p.evictStalePerTeamApprovalChannelMembers()
	}
	// Immediate pass on every node at activation (Schedule waits for the first
	// interval). Overlap with the cluster job and OnActivate sync is intentional:
	// those paths are idempotent (CAS seed ledger) and we prefer a fresh pass
	// over delaying until the first interval on a single node.
	go run()
	job, err := cluster.Schedule(p.API, twoStepChannelReconcileJobKey, cluster.MakeWaitForInterval(twoStepChannelReconcileInterval), run)
	if err != nil {
		p.API.LogError("failed to schedule two-step channel reconcile", "error", err.Error())
		return
	}
	p.twoStepReconcileJob = job
}

// warnSecurityPoolCapOnce logs when listing hits securityPoolMaxMembers.
// Keyed by pool label (not count): at the cap the member count is always the
// max, so a count fingerprint would never change. Cleared when a later listing
// is under the cap so a new overrun warns again. sync.Map-safe across
// reconcile / config / request goroutines.
func (p *Plugin) warnSecurityPoolCapOnce(pool string, members int) {
	if pool == "" {
		return
	}
	if members < securityPoolMaxMembers {
		p.securityPoolCapWarned.Delete(pool)
		return
	}
	if _, loaded := p.securityPoolCapWarned.LoadOrStore(pool, struct{}{}); loaded {
		return
	}
	p.API.LogWarn("two-step channel access: security pool listing hit member cap; extra members can still approve but are not seeded into bot/webhook until login",
		"pool", pool, "members", members, "max_members", securityPoolMaxMembers)
}

func (p *Plugin) stopTwoStepChannelReconciler() {
	if p.twoStepReconcileJob == nil {
		return
	}
	if err := p.twoStepReconcileJob.Close(); err != nil {
		p.API.LogWarn("failed to stop two-step channel reconcile", "error", err.Error())
	}
	p.twoStepReconcileJob = nil
}

// reconcileTwoStepApproverChannels is the periodic path: group/attribute member
// listing only.
func (p *Plugin) reconcileTwoStepApproverChannels() {
	cfg := p.getConfiguration()
	switch cfg.twoStepMode() {
	case twoStepModeSystemAdmin:
		return
	case twoStepModeUserGroup, twoStepModeAttribute:
		// continue
	default:
		return
	}
	ids := p.listTwoStepApproverUserIDs()
	if len(ids) == 0 {
		return
	}
	p.API.LogInfo("two-step channel access: periodic reconcile", "count", len(ids))
	for _, userID := range ids {
		if userID == "" || userID == p.botUserID {
			continue
		}
		p.addUserToTwoStepApprovalChannels(userID)
	}
	p.evictStaleTwoStepApproverChannelMembers()
}

// syncTwoStepApproverChannels reseeds every current security/system pool
// member into bot + webhook approval channels (idempotent). Also links
// LDAP/plugin syncable groups as channel AutoAdd syncables when supported.
// Custom user groups are not syncable in Mattermost — those members are
// added via explicit AddChannelMember below (and on login / periodic reconcile).
func (p *Plugin) syncTwoStepApproverChannels() {
	for _, ch := range []*model.Channel{p.ensureBotApprovalChannel(), p.ensureWebhookApprovalChannel()} {
		if ch != nil {
			p.linkTwoStepGroupsToChannel(ch)
		}
	}
	ids := p.listTwoStepApproverUserIDs()
	p.API.LogInfo("two-step channel access: reseeding approvers into bot/webhook channels",
		"count", len(ids))
	if len(ids) == 0 {
		p.warnNoSecurityApprovers()
	}
	for _, userID := range ids {
		if userID == "" || userID == p.botUserID {
			continue
		}
		p.addUserToTwoStepApprovalChannels(userID)
	}
	p.evictStaleTwoStepApproverChannelMembers()
}

// linkTwoStepGroupsToChannel attaches a syncable (LDAP/plugin) security
// group to the channel with AutoAdd. Custom user groups are skipped —
// Mattermost only syncs LDAP/plugin group sources (custom members are
// added individually via listTwoStepApproverUserIDs).
// warnNoSecurityApprovers logs, once per sync (activation and each change
// to the pool settings), that the security pool has nobody in it. Bot and
// webhook requests then wait at the security step: System Admins don't
// cover it in attribute or usergroup mode.
func (p *Plugin) warnNoSecurityApprovers() {
	cfg := p.getConfiguration()
	switch cfg.twoStepMode() {
	case twoStepModeUserGroup:
		p.API.LogWarn("two-step approvals: the security approver group has no usable members, so bot and webhook requests can't get a security approval. "+
			"Create the group and add members, or, for a custom group, turn on Allow custom approver groups.",
			"group", cfg.approverGroupName(stepSecurity))
	case twoStepModeAttribute:
		p.API.LogWarn("two-step approvals: nobody holds the security approver attribute, so bot and webhook requests can't get a security approval. "+
			"Create the attribute (admin-managed or synced) and set it on the security reviewers.",
			"attribute", cfg.approverAttributeName(stepSecurity))
	}
}

func (p *Plugin) linkTwoStepGroupsToChannel(ch *model.Channel) {
	if ch == nil || p.getConfiguration().twoStepMode() != twoStepModeUserGroup {
		return
	}
	name := p.getConfiguration().approverGroupName(stepSecurity)
	if name == "" {
		return
	}
	group, err := p.resolveApproverGroup(name)
	if err != nil || !group.IsSyncable() {
		return
	}
	if ch.TeamId != "" {
		if _, err := p.API.UpsertGroupSyncable(model.NewGroupTeam(group.Id, ch.TeamId, true)); err != nil {
			p.API.LogDebug("two-step channel access: group→team syncable skipped",
				"group", name, "team_id", ch.TeamId, "error", err.Error())
		}
	}
	if _, err := p.API.UpsertGroupSyncable(model.NewGroupChannel(group.Id, ch.Id, true)); err != nil {
		p.API.LogDebug("two-step channel access: group→channel syncable skipped",
			"group", name, "channel_id", ch.Id, "error", err.Error())
		return
	}
	if syncErr := p.API.CreateDefaultSyncableMemberships(model.CreateDefaultMembershipParams{
		ReAddRemovedMembers: true,
		ScopedChannelID:     &ch.Id,
	}); syncErr != nil {
		p.API.LogDebug("two-step channel access: CreateDefaultSyncableMemberships skipped",
			"channel_id", ch.Id, "error", syncErr.Error())
	}
}

// ensureTwoStepApproverChannelAccess adds user to bot/webhook channels when
// they qualify for either two-step pool (security or system).
func (p *Plugin) ensureTwoStepApproverChannelAccess(user *model.User) {
	p.reconcileTwoStepAccessForUser(user)
}

// reconcileTwoStepAccessForUser adds the user when they still qualify, or
// removes them from bot/webhook channels when demoted (unless System Admin or
// Team Admin on the approval team).
func (p *Plugin) reconcileTwoStepAccessForUser(user *model.User) {
	if user == nil || user.Id == "" || user.Id == p.botUserID {
		return
	}
	for _, ch := range []*model.Channel{p.ensureBotApprovalChannel(), p.ensureWebhookApprovalChannel()} {
		if ch == nil {
			continue
		}
		retain, confident := p.shouldRetainTwoStepApprovalChannelAccess(user, ch.TeamId)
		if !confident {
			continue
		}
		if retain {
			p.addUserToApprovalChannel(ch, user.Id)
			continue
		}
		if delErr := p.API.DeleteChannelMember(ch.Id, user.Id); delErr != nil {
			// Already gone is fine; other errors are worth a warn.
			p.API.LogDebug("two-step channel access: evict on login/reconcile",
				"channel_id", ch.Id, "user_id", user.Id, "error", delErr.Error())
			continue
		}
		p.API.LogInfo("two-step channel access: evicted demoted approver",
			"user_id", user.Id, "username", user.Username, "channel_id", ch.Id)
	}
}

// evictStaleTwoStepApproverChannelMembers removes channel members who are no
// longer in a two-step pool and are not System Admin / Team Admin.
func (p *Plugin) evictStaleTwoStepApproverChannelMembers() {
	cfg := p.getConfiguration()
	switch cfg.twoStepMode() {
	case twoStepModeSystemAdmin:
		return
	case twoStepModeUserGroup, twoStepModeAttribute:
		// continue
	default:
		return
	}
	for _, ch := range []*model.Channel{p.ensureBotApprovalChannel(), p.ensureWebhookApprovalChannel()} {
		if ch != nil {
			p.evictStaleMembersFromTwoStepChannel(ch)
		}
	}
}

func (p *Plugin) evictStaleMembersFromTwoStepChannel(ch *model.Channel) {
	if ch == nil {
		return
	}
	const perPage = 100
	for page := 0; ; page++ {
		members, appErr := p.API.GetChannelMembers(ch.Id, page, perPage)
		if appErr != nil {
			p.API.LogWarn("two-step channel access: GetChannelMembers failed",
				"channel_id", ch.Id, "error", appErr.Error())
			return
		}
		for i := range members {
			m := &members[i]
			if m.UserId == "" || m.UserId == p.botUserID {
				continue
			}
			user, uErr := p.API.GetUser(m.UserId)
			if uErr != nil || user == nil {
				continue
			}
			retain, confident := p.shouldRetainTwoStepApprovalChannelAccess(user, ch.TeamId)
			if !confident || retain {
				continue
			}
			if delErr := p.API.DeleteChannelMember(ch.Id, m.UserId); delErr != nil {
				p.API.LogWarn("two-step channel access: failed to evict demoted approver",
					"channel_id", ch.Id, "user_id", m.UserId, "error", delErr.Error())
				continue
			}
			p.API.LogInfo("two-step channel access: evicted demoted approver",
				"user_id", m.UserId, "username", user.Username, "channel_id", ch.Id)
		}
		if len(members) < perPage {
			return
		}
	}
}

// shouldRetainTwoStepApprovalChannelAccess reports whether user should stay in
// bot/webhook channels. confident=false means a pool lookup failed — caller
// must not evict (fail closed).
func (p *Plugin) shouldRetainTwoStepApprovalChannelAccess(user *model.User, teamID string) (retain, confident bool) {
	if user == nil || user.Id == "" || user.Id == p.botUserID {
		return true, true
	}
	if user.IsSystemAdmin() {
		return true, true
	}
	if p.userIsTeamAdmin(teamID, user.Id) {
		return true, true
	}

	cfg := p.getConfiguration()
	switch cfg.twoStepMode() {
	case twoStepModeSystemAdmin:
		return false, true
	case twoStepModeUserGroup:
		name := cfg.approverGroupName(stepSecurity)
		if name == "" {
			return false, true
		}
		in, err := p.userInApproverGroup(user.Id, name)
		if err != nil {
			return false, false
		}
		return in, true
	default:
		attr := cfg.approverAttributeName(stepSecurity)
		if attr == "" {
			return false, true
		}
		ok, err := p.userIsApprover(user, attr)
		if err != nil {
			return false, false
		}
		return ok, true
	}
}

func (p *Plugin) userIsTeamAdmin(teamID, userID string) bool {
	if teamID == "" || userID == "" {
		return false
	}
	m, appErr := p.API.GetTeamMember(teamID, userID)
	if appErr != nil || m == nil || m.DeleteAt != 0 {
		return false
	}
	return isTeamAdminMember(m)
}

// addUserToTwoStepApprovalChannels ensures the user is on the approval team and
// a member of the bot + webhook private channels.
func (p *Plugin) addUserToTwoStepApprovalChannels(userID string) {
	if userID == "" || userID == p.botUserID {
		return
	}
	for _, ch := range []*model.Channel{p.ensureBotApprovalChannel(), p.ensureWebhookApprovalChannel()} {
		if ch == nil {
			continue
		}
		p.addUserToApprovalChannel(ch, userID)
	}
}

func (p *Plugin) addUserToApprovalChannel(ch *model.Channel, userID string) {
	if ch == nil || userID == "" {
		return
	}
	if ch.TeamId != "" {
		if _, appErr := p.API.CreateTeamMember(ch.TeamId, userID); appErr != nil {
			p.API.LogDebug("two-step channel access: CreateTeamMember",
				"team_id", ch.TeamId, "user_id", userID, "error", appErr.Error())
		}
	}
	p.addChannelMemberAndCategory(ch, userID)
}

// addUserToApprovalChannelIfTeamMember adds userID to ch only when they are
// already an active member of ch's team. Used for SysAdmin visibility on login
// so we do not silently join them to the Approval team.
func (p *Plugin) addUserToApprovalChannelIfTeamMember(ch *model.Channel, userID string) {
	if ch == nil || userID == "" {
		return
	}
	if ch.TeamId != "" {
		m, appErr := p.API.GetTeamMember(ch.TeamId, userID)
		if appErr != nil || m == nil || m.DeleteAt != 0 {
			return
		}
	}
	p.addChannelMemberAndCategory(ch, userID)
}

func (p *Plugin) addChannelMemberAndCategory(ch *model.Channel, userID string) {
	if _, appErr := p.API.AddChannelMember(ch.Id, userID); appErr != nil {
		p.API.LogWarn("two-step channel access: failed to add channel member",
			"channel_id", ch.Id, "user_id", userID, "error", appErr.Error())
		return
	}
	if ch.TeamId != "" {
		p.addChannelToPermissionsCategory(userID, ch.TeamId, ch.Id)
	}
}

// listTwoStepApproverUserIDs returns pool member IDs.
func (p *Plugin) listTwoStepApproverUserIDs() []string {
	cfg := p.getConfiguration()
	seen := map[string]struct{}{}
	var out []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}

	switch cfg.twoStepMode() {
	case twoStepModeSystemAdmin:
		return nil
	case twoStepModeUserGroup:
		for _, id := range p.listApproverGroupMemberIDs(cfg.approverGroupName(stepSecurity)) {
			add(id)
		}
	default:
		for _, id := range p.listApproverAttributeUserIDs(cfg.approverAttributeName(stepSecurity)) {
			add(id)
		}
	}
	return out
}

// normalizeApproverGroupName trims whitespace and a leading @ so admins can
// paste either "security-approvers" or "@security-approvers".
func normalizeApproverGroupName(groupName string) string {
	groupName = strings.TrimSpace(groupName)
	return strings.TrimPrefix(groupName, "@")
}

// resolveApproverGroup finds the group configured as the security approver
// pool, by name or display name. LDAP and plugin-synced groups are accepted;
// a custom group is accepted only when AllowCustomApproverGroups is on,
// because a custom group's managers choose its members. Returns an error
// when no usable group can be found; it never returns (nil, nil).
func (p *Plugin) resolveApproverGroup(groupName string) (*model.Group, error) {
	groupName = normalizeApproverGroupName(groupName)
	if groupName == "" {
		return nil, errors.New("no approver group is configured")
	}

	want := strings.ToLower(groupName)
	allowCustom := p.getConfiguration().AllowCustomApproverGroups
	customMatch := false
	match := func(g *model.Group) bool {
		if g == nil || g.DeleteAt != 0 {
			return false
		}
		if (g.Name == nil || strings.ToLower(*g.Name) != want) && strings.ToLower(g.DisplayName) != want {
			return false
		}
		if g.Source == model.GroupSourceCustom && !allowCustom {
			customMatch = true
			return false
		}
		return true
	}

	if group, appErr := p.API.GetGroupByName(groupName); appErr == nil && match(group) {
		return group, nil
	}

	groups, appErr := p.API.GetGroups(0, 100, model.GroupSearchOpts{Q: groupName, FilterAllowReference: true}, nil)
	if appErr != nil {
		p.API.LogDebug("two-step channel access: GetGroups failed", "group", groupName, "error", appErr.Error())
	}
	for _, g := range groups {
		if match(g) {
			return g, nil
		}
	}

	bySource, appErr := p.API.GetGroupsBySource(model.GroupSourceLdap)
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to list LDAP groups")
	}
	for _, g := range bySource {
		if match(g) {
			return g, nil
		}
	}

	if customMatch {
		p.API.LogWarn("two-step channel access: the approver group is a custom group; turn on Allow custom approver groups to use it, or name an LDAP or plugin-synced group",
			"group", groupName)
	} else {
		p.API.LogWarn("two-step channel access: could not resolve approver group", "group", groupName)
	}
	return nil, errors.Errorf("approver group %q not found", groupName)
}

func (p *Plugin) listApproverGroupMemberIDs(groupName string) []string {
	group, err := p.resolveApproverGroup(groupName)
	if err != nil {
		return nil
	}
	// Group exists — trust GetGroupMemberUsers even when empty.
	return p.listGroupMemberIDsByGroupID(group.Id, groupName)
}

func (p *Plugin) listGroupMemberIDsByGroupID(groupID, label string) []string {
	const perPage = 100
	var ids []string
	for page := 0; ; page++ {
		users, listErr := p.API.GetGroupMemberUsers(groupID, page, perPage)
		if listErr != nil {
			p.API.LogWarn("two-step channel access: GetGroupMemberUsers failed",
				"group", label, "error", listErr.Error())
			return ids
		}
		for _, u := range users {
			if u == nil {
				continue
			}
			ids = append(ids, u.Id)
			if len(ids) >= securityPoolMaxMembers {
				p.warnSecurityPoolCapOnce("group:"+label, len(ids))
				return ids[:securityPoolMaxMembers]
			}
		}
		if len(users) < perPage {
			return ids
		}
	}
}

func (p *Plugin) listApproverAttributeUserIDs(attrName string) []string {
	attrName = strings.TrimSpace(attrName)
	if attrName == "" {
		return nil
	}
	field, err := p.loadApproverAttributeField(attrName)
	if err != nil || field == nil {
		if err != nil {
			p.API.LogWarn("two-step channel access: load attribute field failed",
				"attr", attrName, "error", err.Error())
		}
		return nil
	}
	groupID := field.GroupID
	if groupID == "" {
		group, gErr := p.API.GetPropertyGroup(model.AccessControlPropertyGroupName)
		if gErr != nil || group == nil {
			return nil
		}
		groupID = group.ID
	}

	const perPage = 100
	var ids []string
	var cursor model.PropertyValueSearchCursor
	for {
		values, searchErr := p.API.SearchPropertyValues(groupID, model.PropertyValueSearchOpts{
			GroupID:    groupID,
			TargetType: model.PropertyValueTargetTypeUser,
			FieldID:    field.ID,
			Cursor:     cursor,
			PerPage:    perPage,
		})
		if searchErr != nil {
			p.API.LogWarn("two-step channel access: SearchPropertyValues failed",
				"attr", attrName, "error", searchErr.Error())
			return ids
		}
		for _, v := range values {
			if v == nil || v.TargetID == "" {
				continue
			}
			if attributeValueIsSet(v.Value, field) {
				ids = append(ids, v.TargetID)
				if len(ids) >= securityPoolMaxMembers {
					p.warnSecurityPoolCapOnce("attr:"+attrName, len(ids))
					return ids[:securityPoolMaxMembers]
				}
			}
			cursor = model.PropertyValueSearchCursor{
				PropertyValueID: v.ID,
				CreateAt:        v.CreateAt,
			}
		}
		if len(values) < perPage {
			return ids
		}
	}
}
