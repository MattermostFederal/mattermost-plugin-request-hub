package main

import (
	"slices"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

// UserHasLoggedIn adds or evicts the user from bot/webhook approval channels
// based on current pool / admin status. Mattermost has no plugin hook for
// custom-group membership changes, so login + periodic reconcile cover joins
// and demotions.
func (p *Plugin) UserHasLoggedIn(_ *plugin.Context, user *model.User) {
	if user == nil || user.Id == "" || user.Id == p.botUserID {
		return
	}
	go p.reconcileTwoStepAccessForUser(user)
}

// startTwoStepChannelReconciler periodically adds current pool members to the
// bot/webhook channels. Cheap when groups resolve (only lists ~6 members).
func (p *Plugin) startTwoStepChannelReconciler() {
	p.stopTwoStepChannelReconciler()
	stop := make(chan struct{})
	p.configurationLock.Lock()
	p.twoStepSyncStop = stop
	p.configurationLock.Unlock()

	go func() {
		// Run once immediately — NewTicker only fires after the first interval.
		p.reconcileTwoStepApproverChannels()

		ticker := time.NewTicker(twoStepChannelReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p.reconcileTwoStepApproverChannels()
			}
		}
	}()
}

func (p *Plugin) stopTwoStepChannelReconciler() {
	p.configurationLock.Lock()
	stop := p.twoStepSyncStop
	p.twoStepSyncStop = nil
	p.configurationLock.Unlock()
	if stop != nil {
		close(stop)
	}
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
	for _, userID := range ids {
		if userID == "" || userID == p.botUserID {
			continue
		}
		p.addUserToTwoStepApprovalChannels(userID)
	}
	p.evictStaleTwoStepApproverChannelMembers()
}

// linkTwoStepGroupsToChannel attaches syncable (LDAP/plugin) security/system
// groups to the channel with AutoAdd. Custom user groups are skipped — Mattermost
// only syncs LDAP/plugin group sources.
func (p *Plugin) linkTwoStepGroupsToChannel(ch *model.Channel) {
	if ch == nil || p.getConfiguration().twoStepMode() != twoStepModeUserGroup {
		return
	}
	cfg := p.getConfiguration()
	for _, step := range []approverStep{stepSecurity, stepSystem} {
		name := cfg.approverGroupName(step)
		if name == "" {
			continue
		}
		group, err := p.resolveApproverGroup(name)
		if err != nil || !group.IsSyncable() {
			continue
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
			continue
		}
		if syncErr := p.API.CreateDefaultSyncableMemberships(model.CreateDefaultMembershipParams{
			ReAddRemovedMembers: true,
			ScopedChannelID:     &ch.Id,
		}); syncErr != nil {
			p.API.LogDebug("two-step channel access: CreateDefaultSyncableMemberships skipped",
				"channel_id", ch.Id, "error", syncErr.Error())
		}
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
		for _, step := range []approverStep{stepSecurity, stepSystem} {
			name := cfg.approverGroupName(step)
			if name == "" {
				continue
			}
			in, err := p.userInApproverGroup(user.Id, name)
			if err != nil {
				return false, false
			}
			if in {
				return true, true
			}
		}
		return false, true
	default:
		for _, step := range []approverStep{stepSecurity, stepSystem} {
			attr := cfg.approverAttributeName(step)
			if attr == "" {
				continue
			}
			ok, err := p.userIsApprover(user, attr)
			if err != nil {
				return false, false
			}
			if ok {
				return true, true
			}
		}
		return false, true
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
	return m.SchemeAdmin || slices.Contains(strings.Fields(m.Roles), model.TeamAdminRoleId)
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
		for _, step := range []approverStep{stepSecurity, stepSystem} {
			for _, id := range p.listApproverGroupMemberIDs(cfg.approverGroupName(step)) {
				add(id)
			}
		}
	default:
		for _, step := range []approverStep{stepSecurity, stepSystem} {
			for _, id := range p.listApproverAttributeUserIDs(cfg.approverAttributeName(step)) {
				add(id)
			}
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

// resolveApproverGroup finds the LDAP or plugin-synced group configured as an
// approver pool, by name or display name. Custom groups are never used: any
// user can create one or add themselves to it. Returns an error when no such
// group can be found; it never returns (nil, nil).
func (p *Plugin) resolveApproverGroup(groupName string) (*model.Group, error) {
	groupName = normalizeApproverGroupName(groupName)
	if groupName == "" {
		return nil, errors.New("no approver group is configured")
	}

	want := strings.ToLower(groupName)
	customMatch := false
	match := func(g *model.Group) bool {
		if g == nil || g.DeleteAt != 0 {
			return false
		}
		if (g.Name == nil || strings.ToLower(*g.Name) != want) && strings.ToLower(g.DisplayName) != want {
			return false
		}
		if g.Source == model.GroupSourceCustom {
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
		p.API.LogWarn("two-step channel access: the approver group is a custom group; only LDAP or plugin-synced groups can approve",
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
			if u != nil {
				ids = append(ids, u.Id)
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
