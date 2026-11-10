package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

// resolveApprovalChannel returns the channel where approval requests for the
// given teamID should be posted, plus an optional header prefix to include
// when the request is routed to the root channel on behalf of a specific team.
//
// Priority order:
//
//  1. Per-team auto-channel (when PerTeamApprovalChannels is enabled AND the
//     team has at least one Team Admin or System Admin as a member).
//  2. Root approval channel with a "For Team: X — " prefix label, used when
//     PerTeamApprovalChannels is enabled but the team has no local approvers
//     — routing to a per-team channel nobody monitors would silently bury requests.
//  3. Root approval channel with no label (PerTeamApprovalChannels is disabled).
//  4. nil — caller falls back to DMing every System Admin.
//
// Failures at each tier are logged and the next tier is tried so a
// misconfiguration at one level doesn't silently drop requests.
func (p *Plugin) resolveApprovalChannel(teamID string) (*model.Channel, string) {
	config := p.getConfiguration()

	if config.PerTeamApprovalChannels && teamID != "" {
		if !p.hasActiveTeamApprovers(teamID) {
			// No Team Admin and no System Admin on this team — fall back to root
			// and label the card so System Admins know which team it belongs to.
			teamName := teamID
			if team, appErr := p.API.GetTeam(teamID); appErr == nil {
				teamName = team.DisplayName
			}
			p.API.LogInfo("channel-requests: no team/system admins on team; routing to root channel",
				"team_id", teamID, "team_name", teamName)
			ch := p.ensureRootApprovalChannel()
			return ch, fmt.Sprintf("For Team: %s — ", escapeMarkdown(teamName))
		}

		ch, err := p.ensureTeamApprovalChannel(teamID)
		if err == nil {
			return ch, ""
		}
		p.API.LogWarn("failed to resolve per-team approval channel; falling back to root channel",
			"team_id", teamID, "error", err.Error())
	}

	ch := p.ensureRootApprovalChannel()
	if ch == nil {
		p.API.LogWarn("channel-requests: no approval channel available — falling back to System Admin DMs. " +
			"Configure ApprovalTeam in System Console, or enable PerTeamApprovalChannels.")
	}
	return ch, ""
}

// hasActiveTeamApprovers returns true when teamID has at least one member who
// can monitor a per-team approval channel: a Team Admin (SchemeAdmin /
// team_admin) or a System Admin who belongs to the team.
//
// Returns true on team-member lookup failure so the caller defaults to trying
// the per-team channel rather than silently bypassing it.
func (p *Plugin) hasActiveTeamApprovers(teamID string) bool {
	sysAdmins := p.systemAdminIDSet()

	const perPage = 200
	for page := range 10 {
		members, appErr := p.API.GetTeamMembers(teamID, page, perPage)
		if appErr != nil {
			return true // safe default: assume monitored
		}
		for _, m := range members {
			if isTeamAdminMember(m) {
				return true
			}
			if sysAdmins[m.UserId] {
				return true
			}
		}
		if len(members) < perPage {
			return false
		}
	}
	return false
}

// systemAdminIDSet returns the set of System Admin user IDs (best-effort).
func (p *Plugin) systemAdminIDSet() map[string]bool {
	out := map[string]bool{}
	admins, appErr := p.API.GetUsers(&model.UserGetOptions{Role: model.SystemAdminRoleId, Page: 0, PerPage: 200})
	if appErr != nil {
		p.API.LogWarn("failed to list System Admins for approver check", "error", appErr.Error())
		return out
	}
	for _, admin := range admins {
		if admin != nil && admin.Id != "" && admin.Id != p.botUserID {
			out[admin.Id] = true
		}
	}
	return out
}

// ensureRootApprovalChannel returns the root approval channel for channel and
// admin promotion requests, creating it if it doesn't already exist. The channel
// is always named "mattermost-channel-requests" in the configured ApprovalTeam.
// Returns nil when ApprovalTeam is blank (plugin not yet configured), so callers
// can fall back to DMing System Admins.
func (p *Plugin) ensureRootApprovalChannel() *model.Channel {
	team, err := p.approvalTeam()
	if err != nil {
		p.API.LogWarn("root approval team not found; falling back to System Admin DMs", "error", err.Error())
		return nil
	}
	if team == nil {
		return nil
	}
	teamName := team.Name
	channelName := "mattermost-channel-requests"

	// Fast path: channel already exists.
	if ch, taken := p.lookupApprovalChannel(team.Id, channelName); ch != nil || taken {
		return ch
	}

	ch, appErr := p.API.CreateChannel(&model.Channel{
		TeamId:      team.Id,
		Name:        channelName,
		DisplayName: "Mattermost: Channel Requests",
		Type:        model.ChannelTypePrivate,
		Purpose: fmt.Sprintf(
			"Global channel and governance requests for %s. Reviewed by System and Team Admins.",
			team.DisplayName,
		),
		CreatorId: p.botUserID,
	})
	if appErr != nil {
		// A concurrent call may have beaten us — retry the lookup before surfacing the error.
		if existing, _ := p.lookupApprovalChannel(team.Id, channelName); existing != nil {
			return existing
		}
		p.API.LogWarn("failed to create root approval channel; falling back to System Admin DMs",
			"team", teamName, "channel", channelName, "error", appErr.Error())
		return nil
	}

	if _, postErr := p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: ch.Id,
		Message: "This channel was created automatically by the Mattermost Request Hub plugin. " +
			"It is the global approval channel for channel creation and Channel Admin requests. " +
			"Team Admin and team creation requests go to mattermost-team-requests. " +
			"Configure team-specific routing in System Console → Plugins → Mattermost Request Hub.",
	}); postErr != nil {
		p.API.LogWarn("failed to post welcome message in root approval channel",
			"channel_id", ch.Id, "error", postErr.Error())
	}

	p.addSystemAdminsToChannel(ch.Id)
	p.addPrimaryTeamAdminsToChannel(ch.Id)
	p.bookmarkDocs(ch)

	return ch
}

// Managed sidebar category for every plugin ticket / approval channel.
// Prior labels are renamed in-place so upgrades don't leave duplicate empty categories.
const (
	sidebarCategoryName       = "Mattermost: Request Hub"
	kvSidebarCategoryMigrated = "migration_sidebar_category_v3"
)

var sidebarCategoryNameLegacy = []string{
	"Mattermost: Permissions",
	"Mattermost Requests",
}

func isManagedSidebarCategory(name string) bool {
	return name == sidebarCategoryName || slices.Contains(sidebarCategoryNameLegacy, name)
}

// addChannelToPermissionsCategory ensures the given channel appears at the top
// of the "Mattermost: Request Hub" sidebar category for userID in teamID.
// Renames a legacy category label if found. The category is created on first
// use. Failures are logged but never fatal.
func (p *Plugin) addChannelToPermissionsCategory(userID, teamID, channelID string) {
	if userID == "" || teamID == "" || channelID == "" {
		return
	}
	cats, appErr := p.API.GetChannelSidebarCategories(userID, teamID)
	if appErr != nil {
		p.API.LogWarn("sidebar: failed to get categories",
			"user_id", userID, "team_id", teamID, "error", appErr.Error())
		return
	}
	// GetChannelSidebarCategories can return (nil, nil) on some builds.
	if cats != nil {
		for _, cat := range cats.Categories {
			if cat == nil || !isManagedSidebarCategory(cat.DisplayName) {
				continue
			}
			changed := false
			if cat.DisplayName != sidebarCategoryName {
				cat.DisplayName = sidebarCategoryName
				changed = true
			}
			already := slices.Contains(cat.Channels, channelID)
			if !already {
				cat.Channels = append([]string{channelID}, cat.Channels...)
				changed = true
			}
			if !changed {
				return
			}
			if _, updateErr := p.API.UpdateChannelSidebarCategories(userID, teamID,
				[]*model.SidebarCategoryWithChannels{cat}); updateErr != nil {
				p.API.LogWarn("sidebar: failed to update category",
					"user_id", userID, "category_id", cat.Id, "error", updateErr.Error())
			}
			return
		}
	}

	// Category doesn't exist yet — create it.
	if _, createErr := p.API.CreateChannelSidebarCategory(userID, teamID, &model.SidebarCategoryWithChannels{
		SidebarCategory: model.SidebarCategory{
			UserId:      userID,
			TeamId:      teamID,
			DisplayName: sidebarCategoryName,
			Type:        model.SidebarCategoryCustom,
		},
		Channels: []string{channelID},
	}); createErr != nil {
		p.API.LogWarn("sidebar: failed to create category",
			"user_id", userID, "team_id", teamID, "error", createErr.Error())
	}
}

// addSystemAdminsToChannel adds all System Admins to the given channel so they
// can see and act on root-channel approval requests immediately after creation.
// Per-user failures are logged but don't abort — a partial add is better than none.
func (p *Plugin) addSystemAdminsToChannel(channelID string) {
	// Resolve the channel's team so we can place it in the sidebar category.
	channel, chErr := p.API.GetChannel(channelID)
	var teamID string
	if chErr == nil {
		teamID = channel.TeamId
	} else {
		p.API.LogWarn("sidebar: failed to resolve channel team; sidebar categorization skipped",
			"channel_id", channelID, "error", chErr.Error())
	}

	admins, appErr := p.API.GetUsers(&model.UserGetOptions{Role: model.SystemAdminRoleId, Page: 0, PerPage: 100})
	if appErr != nil {
		p.API.LogWarn("failed to list System Admins for root channel seeding", "channel_id", channelID, "error", appErr.Error())
		return
	}
	for _, admin := range admins {
		if admin.Id == p.botUserID {
			continue
		}
		if _, addErr := p.API.AddChannelMember(channelID, admin.Id); addErr != nil {
			p.API.LogWarn("failed to add System Admin to root approval channel",
				"channel_id", channelID, "user_id", admin.Id, "error", addErr.Error())
			continue
		}
		if teamID != "" {
			p.addChannelToPermissionsCategory(admin.Id, teamID, channelID)
		}
	}
}

// ensureNamedApprovalChannel looks up or creates a private approval channel
// with the given name in the globally-configured approval team. It seeds all
// System Admins and posts a one-time orientation message on first creation.
// Returns nil when ApprovalTeam is not configured (plugin not yet set up).
func (p *Plugin) ensureNamedApprovalChannel(name, displayName, purpose string) *model.Channel {
	team, err := p.approvalTeam()
	if err != nil {
		p.API.LogWarn("cannot ensure named approval channel — team not found",
			"channel", name, "error", err.Error())
		return nil
	}
	if team == nil {
		return nil
	}

	if existing, taken := p.lookupApprovalChannel(team.Id, name); existing != nil || taken {
		return existing
	}

	ch, appErr := p.API.CreateChannel(&model.Channel{
		TeamId:      team.Id,
		Name:        name,
		DisplayName: displayName,
		Purpose:     purpose,
		Type:        model.ChannelTypePrivate,
		CreatorId:   p.botUserID,
	})
	if appErr != nil {
		// Concurrent activation may have beaten us — retry lookup before failing.
		if existing, _ := p.lookupApprovalChannel(team.Id, name); existing != nil {
			return existing
		}
		p.API.LogWarn("failed to create dedicated approval channel",
			"name", name, "error", appErr.Error())
		return nil
	}

	_, _ = p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: ch.Id,
		Message: fmt.Sprintf(
			"This channel was created automatically by the Mattermost Request Hub plugin. "+
				"**%s** requests are posted here for System Admin review. "+
				"Reply to any request thread to ask the requester for more information.",
			displayName,
		),
	})

	// Seed all System Admins immediately so they can see and act on requests
	// without waiting for the next activation cycle. Security/system pool
	// members are seeded separately via syncTwoStepApproverChannels (activate /
	// config change / login) to avoid an init cycle with ensure* helpers.
	// Primary-team Team Admins get watch-only membership (approve rights stay
	// gated by existing toggles / two-step rules).
	p.addSystemAdminsToChannel(ch.Id)
	p.addPrimaryTeamAdminsToChannel(ch.Id)
	p.bookmarkDocs(ch)

	return ch
}

// ensureBotApprovalChannel returns the dedicated channel for bot account
// creation requests. Created automatically in the root approval team.
func (p *Plugin) ensureBotApprovalChannel() *model.Channel {
	return p.ensureNamedApprovalChannel(
		"mattermost-bot-requests",
		"Mattermost: Bot Requests",
		"Bot account creation requests — System Admin review required.",
	)
}

// ensureWebhookApprovalChannel returns the dedicated channel for incoming and
// outgoing webhook requests. Created automatically in the root approval team.
func (p *Plugin) ensureWebhookApprovalChannel() *model.Channel {
	return p.ensureNamedApprovalChannel(
		"mattermost-webhook-requests",
		"Mattermost: Webhook Requests",
		"Incoming and outgoing webhook requests — System Admin review required.",
	)
}

// ensureTeamCreationApprovalChannel returns the dedicated channel for team
// creation requests. Created automatically in the root approval team. Always
// forces the channel into the managed "Mattermost: Request Hub" sidebar
// category for System Admins (same treatment as other ticket channels),
// including when the channel already existed from an older install.
func (p *Plugin) ensureTeamCreationApprovalChannel() *model.Channel {
	ch := p.ensureNamedApprovalChannel(
		"mattermost-team-requests",
		"Mattermost: Team Requests",
		"Team creation requests — System Admin review required.",
	)
	if ch != nil {
		p.forceTicketChannelCategory(ch)
	}
	return ch
}

// forceTicketChannelCategory places ch at the top of each System Admin's
// "Mattermost: Request Hub" sidebar category for ch's team. Does not add
// membership — callers that need seeding should use addSystemAdminsToChannel.
func (p *Plugin) forceTicketChannelCategory(ch *model.Channel) {
	if ch == nil || ch.TeamId == "" {
		return
	}
	for adminID := range p.systemAdminIDSet() {
		p.addChannelToPermissionsCategory(adminID, ch.TeamId, ch.Id)
	}
}

// ensureAuditChannel returns the dedicated channel for audit log entries.
// Created automatically in the root approval team. All approve/deny actions
// post a line here so admins have a chronological record outside of request threads.
func (p *Plugin) ensureAuditChannel() *model.Channel {
	return p.ensureNamedApprovalChannel(
		"mattermost-audit-requests",
		"Mattermost: Audit Requests",
		"Audit log for all approval actions — channel creation, bot, webhook, team, and admin requests.",
	)
}

// ensureTeamApprovalChannel returns the per-team approval channel, creating it
// (and adding all current Team Admins as members) if it doesn't already exist.
// Safe to call concurrently — if two calls race to create the same channel, the
// loser retries the lookup and returns the already-created channel.
func (p *Plugin) ensureTeamApprovalChannel(teamID string) (*model.Channel, error) {
	channelName := p.getConfiguration().TeamChannelName()

	// Fast path: channel already exists.
	if ch, taken := p.lookupApprovalChannel(teamID, channelName); ch != nil {
		return ch, nil
	} else if taken {
		return nil, errors.New("the team approval channel name is taken by another channel")
	}

	team, appErr := p.API.GetTeam(teamID)
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to load team")
	}

	// Ensure the bot is a team member before creating the channel. The plugin
	// API skips most permission checks, but the bot must be in the team to
	// reliably post messages in team channels later. An existing membership
	// is left alone: adding the bot again fails on current servers.
	if addErr := p.ensureTeamMember(teamID, p.botUserID); addErr != nil {
		p.API.LogWarn("failed to add bot to team before creating approval channel",
			"team_id", teamID, "bot_user_id", p.botUserID, "error", addErr.Error())
	}

	ch, appErr := p.API.CreateChannel(&model.Channel{
		TeamId:      teamID,
		Name:        channelName,
		DisplayName: "Mattermost: Channel Requests",
		Type:        model.ChannelTypePrivate,
		Purpose: fmt.Sprintf(
			"Channel and admin promotion requests for %s. Team Admins review and approve requests here.",
			team.DisplayName,
		),
		CreatorId: p.botUserID,
	})
	if appErr != nil {
		// A concurrent call may have beaten us to creation — retry the lookup
		// before surfacing the error.
		if existing, _ := p.lookupApprovalChannel(teamID, channelName); existing != nil {
			return existing, nil
		}
		return nil, errors.Wrap(appErr, "failed to create team approval channel")
	}

	p.addTeamApproversToChannel(teamID, ch.Id)
	p.postChannelWelcomeMessage(ch, team)
	p.bookmarkDocs(ch)

	return ch, nil
}

// addTeamApproversToChannel adds every Team Admin of teamID, plus every System
// Admin who is a member of that team, to channelID. Places each in the managed
// "Mattermost: Request Hub" sidebar category. Called when the per-team channel
// is first created (and from the one-time sidebar migration). Per-member
// failures are logged but don't abort the rest.
func (p *Plugin) addTeamApproversToChannel(teamID, channelID string) {
	sysAdmins := p.systemAdminIDSet()
	const perPage = 200
	for page := range 50 {
		members, appErr := p.API.GetTeamMembers(teamID, page, perPage)
		if appErr != nil {
			p.API.LogWarn("failed to list team members when seeding approval channel",
				"team_id", teamID, "channel_id", channelID, "error", appErr.Error())
			return
		}
		for _, m := range members {
			isTeamAdmin := isTeamAdminMember(m)
			if !isTeamAdmin && !sysAdmins[m.UserId] {
				continue
			}
			if m.UserId == p.botUserID {
				continue
			}
			if _, appErr := p.API.AddChannelMember(channelID, m.UserId); appErr != nil {
				p.API.LogWarn("failed to add approver to team approval channel",
					"channel_id", channelID, "user_id", m.UserId, "error", appErr.Error())
				continue
			}
			if isTeamAdmin {
				p.markTAVisibilitySeeded(channelID, m.UserId)
			}
			p.addChannelToPermissionsCategory(m.UserId, teamID, channelID)
		}
		if len(members) < perPage {
			return
		}
	}
}

// postChannelWelcomeMessage posts an orientation message in the newly-created
// per-team approval channel so Team Admins know what it's for.
func (p *Plugin) postChannelWelcomeMessage(ch *model.Channel, team *model.Team) {
	msg := fmt.Sprintf(
		"This channel was created automatically by the Mattermost Request Hub plugin. "+
			"Channel creation and Channel Admin promotion requests for the **%s** team are posted here for review.\n\n"+
			"Team Admins will receive new-request notifications and can use the **Approve** / **Deny** buttons. "+
			"Reply to any request thread to ask the requester for more information.",
		escapeMarkdown(team.DisplayName),
	)
	if _, appErr := p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: ch.Id,
		Message:   msg,
	}); appErr != nil {
		p.API.LogWarn("failed to post welcome message in team approval channel",
			"channel_id", ch.Id, "error", appErr.Error())
	}
}

// ensureAllTeamApprovalChannels creates per-team approval channels for every
// team that has at least one Team Admin or System Admin member. Teams with no
// local approvers are skipped (their channel requests fall back to the root
// approval channel). Called from OnActivate / config enable when
// PerTeamApprovalChannels is on. Best-effort: failures for individual teams
// are logged but don't abort the sweep.
func (p *Plugin) ensureAllTeamApprovalChannels() {
	teams, appErr := p.API.GetTeams()
	if appErr != nil {
		p.API.LogWarn("failed to list teams for approval-channel sweep", "error", appErr.Error())
		return
	}
	approvalTeam := strings.TrimSpace(p.getConfiguration().ApprovalTeam)
	channelName := p.getConfiguration().TeamChannelName()
	created := 0
	skipped := 0
	for _, team := range teams {
		// Fast path: channel already exists — skip the Team Admin / SysAdmin
		// membership scan for every other team. Only the Approval team reseeds.
		if existing, _ := p.lookupApprovalChannel(team.Id, channelName); existing != nil {
			if team.Name == approvalTeam {
				p.addTeamApproversToChannel(team.Id, existing.Id)
			}
			created++
			continue
		}
		if !p.hasActiveTeamApprovers(team.Id) {
			skipped++
			continue
		}
		ch, err := p.ensureTeamApprovalChannel(team.Id)
		if err != nil {
			p.API.LogWarn("failed to ensure approval channel for team",
				"team_id", team.Id, "team_name", team.Name, "error", err.Error())
			continue
		}
		// In the approval team this is also the root approval channel, which
		// ensureRootApprovalChannel creates with System Admins only, so the
		// fast path above never seeds the team's Team Admins. Re-sync them,
		// the same way OnActivate re-syncs System Admins.
		if team.Name == approvalTeam {
			p.addTeamApproversToChannel(team.Id, ch.Id)
		}
		created++
	}
	p.API.LogInfo("per-team approval channel sweep complete",
		"teams", len(teams), "ensured", created, "skipped_no_approvers", skipped)
}

// migrateTicketChannelsToManagedCategory is a one-shot sweep: for every plugin
// ticket/approval channel (including mattermost-team-requests), ensure each
// member has that channel in the managed "Mattermost: Request Hub" sidebar
// category (renaming a short-lived "Mattermost Requests" category if present).
// Idempotent via KV flag (v2 re-runs after the temporary rename).
func (p *Plugin) migrateTicketChannelsToManagedCategory() {
	if migrated, _ := p.API.KVGet(kvSidebarCategoryMigrated); migrated != nil {
		return
	}

	channels := p.requestChannels()
	placed := 0
	for _, ch := range channels {
		placed += p.placeChannelMembersInManagedCategory(ch)
	}

	_ = p.API.KVSet(kvSidebarCategoryMigrated, []byte("1"))
	p.API.LogInfo("sidebar migration: ticket channels forced into Mattermost: Request Hub category",
		"channels", len(channels), "member_placements", placed)
}

// requestChannels returns every plugin ticket/approval channel: root, bot,
// webhook, team-creation, audit and each team's own channel, without
// duplicates (in the approval team the per-team channel is the root one).
// The shared channels are created if missing.
func (p *Plugin) requestChannels() []*model.Channel {
	channelName := p.getConfiguration().TeamChannelName()
	var channels []*model.Channel

	for _, ensure := range []func() *model.Channel{
		p.ensureRootApprovalChannel,
		p.ensureBotApprovalChannel,
		p.ensureWebhookApprovalChannel,
		p.ensureTeamCreationApprovalChannel,
		p.ensureAuditChannel,
	} {
		if ch := ensure(); ch != nil {
			channels = append(channels, ch)
		}
	}

	if teams, appErr := p.API.GetTeams(); appErr == nil {
		for _, team := range teams {
			if ch, getErr := p.API.GetChannelByName(team.Id, channelName, false); getErr == nil && ch != nil {
				channels = append(channels, ch)
			}
		}
	} else {
		p.API.LogWarn("failed to list teams for the request channel sweep", "error", appErr.Error())
	}

	seen := map[string]bool{}
	out := channels[:0]
	for _, ch := range channels {
		if ch == nil || ch.Id == "" || seen[ch.Id] {
			continue
		}
		seen[ch.Id] = true
		out = append(out, ch)
	}
	return out
}

// placeChannelMembersInManagedCategory walks channel members and ensures each
// is in the managed sidebar category for that channel's team. Returns how many
// members were processed (best-effort).
func (p *Plugin) placeChannelMembersInManagedCategory(ch *model.Channel) int {
	if ch == nil || ch.TeamId == "" {
		return 0
	}
	count := 0
	const perPage = 200
	for page := range 50 {
		members, appErr := p.API.GetUsersInChannel(ch.Id, "username", page, perPage)
		if appErr != nil {
			p.API.LogWarn("sidebar migration: failed to list channel members",
				"channel_id", ch.Id, "error", appErr.Error())
			return count
		}
		for _, u := range members {
			if u.Id == p.botUserID {
				continue
			}
			p.addChannelToPermissionsCategory(u.Id, ch.TeamId, ch.Id)
			count++
		}
		if len(members) < perPage {
			return count
		}
	}
	return count
}

// syncTeamApprovalChannelAfterAdminChange creates (if needed) and reseeds the
// per-team channel-requests channel when PerTeamApprovalChannels is on and the
// team has at least one local approver. Used after team create and after Team
// Admin promotion — both paths where membership/roles change without a join hook.
// When the team is the primary Approval team, also reseeds Team Admin visibility
// across all primary ticket channels (bot/webhook/team/audit included).
func (p *Plugin) syncTeamApprovalChannelAfterAdminChange(teamID string) {
	if teamID == "" {
		return
	}
	if approval, err := p.approvalTeam(); err == nil && approval != nil && approval.Id == teamID {
		p.syncPrimaryTeamAdminVisibility()
	}
	if !p.getConfiguration().PerTeamApprovalChannels {
		return
	}
	if !p.hasActiveTeamApprovers(teamID) {
		return
	}
	ch, err := p.ensureTeamApprovalChannel(teamID)
	if err != nil {
		p.API.LogWarn("failed to ensure per-team approval channel after admin change",
			"team_id", teamID, "error", err.Error())
		return
	}
	if ch == nil {
		return
	}
	p.addTeamApproversToChannel(teamID, ch.Id)
	// Demotions via the plugin path land here; console demotions are covered
	// by login + the periodic reconciler.
	p.evictStaleApproversFromApprovalChannel(ch, teamID)
	p.placeChannelMembersInManagedCategory(ch)
}

// primaryTicketChannels returns the shared ticket channels on the primary
// Approval team (root, bot, webhook, team-creation, audit), creating them if
// needed. Used for setup-time ensure and Team Admin visibility seeding.
func (p *Plugin) primaryTicketChannels() []*model.Channel {
	var out []*model.Channel
	for _, ensure := range []func() *model.Channel{
		p.ensureRootApprovalChannel,
		p.ensureBotApprovalChannel,
		p.ensureWebhookApprovalChannel,
		p.ensureTeamCreationApprovalChannel,
		p.ensureAuditChannel,
	} {
		if ch := ensure(); ch != nil {
			out = append(out, ch)
		}
	}
	return out
}

// listTeamAdminIDs returns Team Admin user IDs for teamID (best-effort).
func (p *Plugin) listTeamAdminIDs(teamID string) []string {
	var out []string
	if teamID == "" {
		return out
	}
	const perPage = 200
	for page := range 50 {
		members, appErr := p.API.GetTeamMembers(teamID, page, perPage)
		if appErr != nil {
			p.API.LogWarn("failed to list team members for Team Admin visibility seeding",
				"team_id", teamID, "error", appErr.Error())
			return out
		}
		for _, m := range members {
			if m == nil || m.UserId == "" || m.UserId == p.botUserID {
				continue
			}
			if isTeamAdminMember(m) {
				out = append(out, m.UserId)
			}
		}
		if len(members) < perPage {
			return out
		}
	}
	return out
}

// addPrimaryTeamAdminsToChannel adds every Team Admin of the configured
// Approval team to channelID for watch-only visibility, and places each in the
// managed sidebar category. No-op when ApprovalTeam is unset.
func (p *Plugin) addPrimaryTeamAdminsToChannel(channelID string) {
	if channelID == "" {
		return
	}
	team, err := p.approvalTeam()
	if err != nil || team == nil {
		return
	}
	for _, userID := range p.listTeamAdminIDs(team.Id) {
		if _, appErr := p.API.AddChannelMember(channelID, userID); appErr != nil {
			p.API.LogWarn("failed to add primary Team Admin to ticket channel",
				"channel_id", channelID, "user_id", userID, "error", appErr.Error())
			continue
		}
		p.markTAVisibilitySeeded(channelID, userID)
		p.addChannelToPermissionsCategory(userID, team.Id, channelID)
	}
}

// primaryVisibilityChannels are primary ticket channels where demoted
// plugin-seeded Team Admins are evicted (root / team-creation / audit).
// Bot/webhook are excluded: security-pool members also belong there.
func (p *Plugin) primaryVisibilityChannels() []*model.Channel {
	var out []*model.Channel
	for _, ensure := range []func() *model.Channel{
		p.ensureRootApprovalChannel,
		p.ensureTeamCreationApprovalChannel,
		p.ensureAuditChannel,
	} {
		if ch := ensure(); ch != nil {
			out = append(out, ch)
		}
	}
	return out
}

// primaryTAVisibilityChannels returns the primary channels Approval-team Team
// Admins are seeded into for watch-only visibility. Gated by the same flags
// that control whether those TAs may approve the related request types.
func (p *Plugin) primaryTAVisibilityChannels() []*model.Channel {
	cfg := p.getConfiguration()
	var out []*model.Channel
	if cfg.AllowTeamAdminApprovers {
		if ch := p.ensureRootApprovalChannel(); ch != nil {
			out = append(out, ch)
		}
	}
	if cfg.AllowApprovalTeamAdminsForTeamRequests {
		if ch := p.ensureTeamCreationApprovalChannel(); ch != nil {
			out = append(out, ch)
		}
	}
	if cfg.AllowTeamAdminApprovers || cfg.AllowApprovalTeamAdminsForTeamRequests {
		if ch := p.ensureAuditChannel(); ch != nil {
			out = append(out, ch)
		}
	}
	return out
}

// seedTAVisibility adds userID to ch and records that the plugin seeded them
// so demotion eviction can leave hand-added members alone.
func (p *Plugin) seedTAVisibility(ch *model.Channel, userID string) {
	if ch == nil || userID == "" {
		return
	}
	p.addUserToApprovalChannel(ch, userID)
	p.markTAVisibilitySeeded(ch.Id, userID)
}

// syncPrimaryTeamAdminVisibility ensures primary ticket channels exist and
// reseeds Approval-team Team Admins into the gated visibility channels, then
// evicts plugin-seeded members who no longer qualify.
func (p *Plugin) syncPrimaryTeamAdminVisibility() {
	team, err := p.approvalTeam()
	if err != nil || team == nil {
		return
	}
	channels := p.primaryTAVisibilityChannels()
	if len(channels) == 0 {
		return
	}
	adminIDs := p.listTeamAdminIDs(team.Id)
	p.API.LogInfo("primary team admin visibility: reseeding into ticket channels",
		"team_id", team.Id, "admins", len(adminIDs), "channels", len(channels))
	for _, userID := range adminIDs {
		for _, ch := range channels {
			p.seedTAVisibility(ch, userID)
		}
	}
	p.evictStalePrimaryTeamAdminChannelMembers()
}

// ensurePrimaryTeamAdminVisibilityForUser adds one Approval-team Team Admin to
// gated primary visibility channels. No-op when the user is not a Team Admin.
func (p *Plugin) ensurePrimaryTeamAdminVisibilityForUser(userID string) {
	if userID == "" || userID == p.botUserID || !p.isApprovalTeamAdmin(userID) {
		return
	}
	for _, ch := range p.primaryTAVisibilityChannels() {
		p.seedTAVisibility(ch, userID)
	}
}

// reconcilePrimaryTeamAdminVisibilityForUser seeds the user into primary
// ticket channels when they are a SysAdmin or Approval-team Team Admin.
// Demotion eviction is the ~2m periodic sweep (evictStalePrimaryTeamAdminChannelMembers),
// not login: login also runs for non-TA members who were added manually
// (observers, e2e bystanders), and kicking them mid-request turns Approve into
// a Mattermost 403 before the plugin can reply. Bot/webhook membership stays
// with two-step reconcile so security-pool members are not removed.
func (p *Plugin) reconcilePrimaryTeamAdminVisibilityForUser(user *model.User) {
	if user == nil || user.Id == "" || user.Id == p.botUserID {
		return
	}
	team, err := p.approvalTeam()
	if err != nil || team == nil {
		return
	}

	if user.IsSystemAdmin() {
		for _, ch := range p.primaryTicketChannels() {
			p.addUserToApprovalChannel(ch, user.Id)
		}
		return
	}

	member, appErr := p.API.GetTeamMember(team.Id, user.Id)
	if appErr != nil || member == nil || member.DeleteAt != 0 {
		return
	}
	if !isTeamAdminMember(member) {
		return
	}
	for _, ch := range p.primaryTAVisibilityChannels() {
		p.seedTAVisibility(ch, user.Id)
	}
}

// evictStalePrimaryTeamAdminChannelMembers removes members of root /
// team-creation / audit who are not the bot, not a System Admin, and not an
// Approval-team Team Admin. Fail-closed on user/role lookup errors (skip).
func (p *Plugin) evictStalePrimaryTeamAdminChannelMembers() {
	team, err := p.approvalTeam()
	if err != nil || team == nil {
		return
	}
	for _, ch := range p.primaryVisibilityChannels() {
		if ch == nil {
			continue
		}
		p.evictStaleApproversFromApprovalChannel(ch, team.Id)
	}
}

// evictStalePerTeamApprovalChannelMembers removes demoted Team Admins from
// each team's mattermost-channel-requests channel (keep bot, System Admins,
// and current Team Admins of that team). Console demotions have no plugin
// hook; this runs on the same ~2m ticker as primary visibility eviction.
func (p *Plugin) evictStalePerTeamApprovalChannelMembers() {
	if !p.getConfiguration().PerTeamApprovalChannels {
		return
	}
	teams, appErr := p.API.GetTeams()
	if appErr != nil {
		p.API.LogWarn("per-team approval: failed to list teams for demotion eviction",
			"error", appErr.Error())
		return
	}
	channelName := p.getConfiguration().TeamChannelName()
	for _, team := range teams {
		if team == nil || team.Id == "" || team.DeleteAt != 0 {
			continue
		}
		ch, _ := p.lookupApprovalChannel(team.Id, channelName)
		if ch == nil {
			continue
		}
		p.evictStaleApproversFromApprovalChannel(ch, team.Id)
	}
}

// reconcilePerTeamApprovalAccessForUser removes the user from any per-team
// channel-requests channel where they are no longer a Team Admin (and not a
// System Admin). Used on login so demotions take effect without waiting for
// the periodic sweep.
func (p *Plugin) reconcilePerTeamApprovalAccessForUser(user *model.User) {
	if user == nil || user.Id == "" || user.Id == p.botUserID {
		return
	}
	if !p.getConfiguration().PerTeamApprovalChannels {
		return
	}
	if user.IsSystemAdmin() {
		return
	}
	teams, appErr := p.API.GetTeamsForUser(user.Id)
	if appErr != nil {
		return
	}
	channelName := p.getConfiguration().TeamChannelName()
	for _, team := range teams {
		if team == nil || team.Id == "" {
			continue
		}
		ch, _ := p.lookupApprovalChannel(team.Id, channelName)
		if ch == nil {
			continue
		}
		tm, tmErr := p.API.GetTeamMember(team.Id, user.Id)
		if tmErr == nil && tm != nil && tm.DeleteAt == 0 && isTeamAdminMember(tm) {
			continue
		}
		if tmErr != nil && !isNotFound(tmErr) {
			continue
		}
		// Only remove members the plugin seeded — leave hand-added observers.
		if !p.isTAVisibilitySeeded(ch.Id, user.Id) {
			continue
		}
		if _, getErr := p.API.GetChannelMember(ch.Id, user.Id); getErr != nil {
			continue
		}
		if delErr := p.API.DeleteChannelMember(ch.Id, user.Id); delErr != nil {
			p.API.LogDebug("per-team approval: evict on login",
				"channel_id", ch.Id, "user_id", user.Id, "error", delErr.Error())
			continue
		}
		p.clearTAVisibilitySeeded(ch.Id, user.Id)
		p.API.LogInfo("per-team approval: evicted demoted Team Admin",
			"user_id", user.Id, "username", user.Username, "team_id", team.Id, "channel_id", ch.Id)
	}
}

// evictStaleApproversFromApprovalChannel removes plugin-seeded members who are
// no longer a System Admin or Team Admin of teamID. Hand-added observers and
// cross-team approvers are left alone. Collects member IDs first so deletes
// do not skip rows while paging.
func (p *Plugin) evictStaleApproversFromApprovalChannel(ch *model.Channel, teamID string) {
	if ch == nil || teamID == "" {
		return
	}
	const perPage = 100
	var memberIDs []string
	for page := range 50 {
		members, appErr := p.API.GetChannelMembers(ch.Id, page, perPage)
		if appErr != nil {
			p.API.LogWarn("approval channel: GetChannelMembers failed during demotion eviction",
				"channel_id", ch.Id, "error", appErr.Error())
			return
		}
		for i := range members {
			if id := members[i].UserId; id != "" && id != p.botUserID {
				memberIDs = append(memberIDs, id)
			}
		}
		if len(members) < perPage {
			break
		}
	}
	seeded := p.loadTAVisibilitySeededSet(ch.Id)
	if len(seeded) == 0 {
		return
	}
	for _, userID := range memberIDs {
		if _, ok := seeded[userID]; !ok {
			continue
		}
		user, uErr := p.API.GetUser(userID)
		if uErr != nil || user == nil {
			continue
		}
		if user.IsSystemAdmin() {
			continue
		}
		tm, tmErr := p.API.GetTeamMember(teamID, userID)
		if tmErr != nil {
			if !isNotFound(tmErr) {
				continue
			}
		} else if tm != nil && tm.DeleteAt == 0 && isTeamAdminMember(tm) {
			continue
		}
		if delErr := p.API.DeleteChannelMember(ch.Id, userID); delErr != nil {
			p.API.LogWarn("approval channel: failed to evict demoted member",
				"channel_id", ch.Id, "user_id", userID, "error", delErr.Error())
			continue
		}
		p.clearTAVisibilitySeeded(ch.Id, userID)
		p.API.LogInfo("approval channel: evicted demoted Team Admin",
			"user_id", userID, "username", user.Username, "team_id", teamID, "channel_id", ch.Id)
	}
}

// UserHasJoinedTeam ensures that a Team Admin or System Admin who joins (or is
// added to) a team is automatically added to the team's approval channel. This
// handles new team creation and explicit admin additions. Promotion of an
// existing member to Team Admin is handled by syncTeamApprovalChannelAfterAdminChange
// (called from promoteTeamAdmins / createTeamForRequest).
//
// When the join is onto the primary Approval team as Team Admin, the user is
// also seeded into all primary ticket channels (bot/webhook/team/audit) for
// visibility — even when PerTeamApprovalChannels is off.
func (p *Plugin) UserHasJoinedTeam(_ *plugin.Context, teamMember *model.TeamMember, actor *model.User) {
	if teamMember == nil || teamMember.UserId == "" {
		return
	}

	isTeamAdmin := isTeamAdminMember(teamMember)
	if isTeamAdmin {
		if approval, err := p.approvalTeam(); err == nil && approval != nil && approval.Id == teamMember.TeamId {
			p.ensurePrimaryTeamAdminVisibilityForUser(teamMember.UserId)
		}
	}

	if !p.getConfiguration().PerTeamApprovalChannels {
		return
	}

	isSysAdmin := false
	if actor != nil && actor.Id == teamMember.UserId {
		isSysAdmin = actor.IsSystemAdmin()
	} else if u, appErr := p.API.GetUser(teamMember.UserId); appErr == nil {
		isSysAdmin = u.IsSystemAdmin()
	}
	if !isTeamAdmin && !isSysAdmin {
		return
	}

	ch, err := p.ensureTeamApprovalChannel(teamMember.TeamId)
	if err != nil {
		p.API.LogWarn("failed to ensure team approval channel on team join",
			"team_id", teamMember.TeamId, "user_id", teamMember.UserId, "error", err.Error())
		return
	}

	if _, appErr := p.API.AddChannelMember(ch.Id, teamMember.UserId); appErr != nil {
		p.API.LogWarn("failed to add joining approver to approval channel",
			"channel_id", ch.Id, "user_id", teamMember.UserId, "error", appErr.Error())
		return
	}
	// Team Admins must be recorded as plugin-seeded so demotion eviction can
	// remove them later (System Admins are not swept by that path).
	if isTeamAdmin {
		p.markTAVisibilitySeeded(ch.Id, teamMember.UserId)
	}
	p.addChannelToPermissionsCategory(teamMember.UserId, teamMember.TeamId, ch.Id)
}

// kvApprovalTeamKey records which team the Approval team setting named when
// it was first used, so a different team that later takes that name isn't
// treated as the approval team.
const kvApprovalTeamKey = "approval_team"

type approvalTeamRecord struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// approvalTeamResetHint tells an admin how to clear the recorded Approval
// team: the record is forgotten only when the setting changes.
const approvalTeamResetHint = "to use it, set Approval team to a different team, save, then set it back"

// approvalTeam returns the team named by the Approval team setting, or nil
// when the setting is blank. The first team found for a name is recorded;
// if that name later belongs to a different team (the original was renamed
// or deleted), approvalTeam returns an error until the setting is changed
// and changed back. Archived teams are refused.
func (p *Plugin) approvalTeam() (*model.Team, error) {
	name := strings.TrimSpace(p.getConfiguration().ApprovalTeam)
	if name == "" {
		return nil, nil
	}
	team, appErr := p.API.GetTeamByName(name)
	if appErr != nil || team == nil {
		return nil, errors.Errorf("approval team %q not found", name)
	}
	if team.DeleteAt != 0 {
		return nil, errors.Errorf("approval team %q is archived", name)
	}

	record, _ := json.Marshal(approvalTeamRecord{Name: name, ID: team.Id}) // always marshals
	// Two tries: if another server records a team between our read and
	// write, the second pass checks what it recorded.
	for range 2 {
		data, appErr := p.API.KVGet(kvApprovalTeamKey)
		if appErr != nil {
			return nil, errors.Wrap(appErr, "failed to load the recorded approval team")
		}
		if data != nil {
			var recorded approvalTeamRecord
			if err := json.Unmarshal(data, &recorded); err != nil {
				p.API.LogError("The recorded Approval team can't be read, so the Approval team isn't used. In the plugin settings, set Approval team to a different team, save, then set it back.",
					"error", err.Error())
				return nil, errors.Wrap(err, "failed to read the recorded approval team")
			}
			if recorded.Name == name {
				if recorded.ID != team.Id {
					p.API.LogError("The team named in the Approval team setting is not the team that was selected. In the plugin settings, set Approval team to a different team, save, then set it back.",
						"team", name, "recorded_team_id", recorded.ID, "team_id", team.Id)
					return nil, errors.Errorf("the team named %q is not the team that was selected as the Approval team; %s", name, approvalTeamResetHint)
				}
				return team, nil
			}
		}

		saved, appErr := p.API.KVCompareAndSet(kvApprovalTeamKey, data, record)
		if appErr != nil {
			p.API.LogWarn("failed to record the approval team", "team", name, "error", appErr.Error())
			return team, nil
		}
		if saved {
			return team, nil
		}
	}
	return nil, errors.New("the recorded approval team kept changing; try again")
}

// forgetApprovalTeam clears the recorded approval team so the next lookup
// records whichever team the setting names.
func (p *Plugin) forgetApprovalTeam() {
	if appErr := p.API.KVDelete(kvApprovalTeamKey); appErr != nil {
		p.API.LogWarn("failed to clear the recorded approval team", "error", appErr.Error())
	}
}

// isPluginApprovalChannel reports whether ch is a live private channel this
// plugin's bot created, the only kind used for approval cards without
// further checks.
func (p *Plugin) isPluginApprovalChannel(ch *model.Channel) bool {
	return ch != nil && ch.Type == model.ChannelTypePrivate && ch.CreatorId == p.botUserID && ch.DeleteAt == 0
}

// lookupApprovalChannel finds the approval channel with the given name in a
// team. A live private channel the plugin's bot didn't create is adopted
// when a System Admin or an earlier bot of this plugin created it (an
// upgraded server, or a plugin bot account that was recreated). taken is true when a channel with that name
// exists but can't be used; approval cards are then not posted there, and an
// admin is asked to rename that channel.
func (p *Plugin) lookupApprovalChannel(teamID, name string) (ch *model.Channel, taken bool) {
	existing, appErr := p.API.GetChannelByName(teamID, name, true)
	if appErr != nil || existing == nil {
		return nil, false
	}
	if p.isPluginApprovalChannel(existing) {
		return existing, false
	}
	if p.adoptApprovalChannel(existing) {
		return existing, false
	}
	p.API.LogError("An approval channel name is used by a channel the plugin can't use (archived, public, or created by someone who is not a System Admin or this plugin's bot), so requests are sent elsewhere. Rename that channel so the plugin can create its own.",
		"team_id", teamID, "channel_id", existing.Id, "channel_name", name, "creator_id", existing.CreatorId)
	return nil, true
}

// adoptApprovalChannel reports whether ch, a channel with an approval
// channel's name that the plugin's bot didn't create, can be used anyway: it
// must be live (private, or public that we can convert), and either (a) its
// creator is a System Admin / Request Hub bot, or (b) this plugin's bot is
// already a member *and* the creator is gone (re-upload / purged bot). Bot
// membership alone is not enough while an untrusted creator still exists —
// that would let a random user pre-create the name, get the bot added, and
// capture tickets. Legacy public mattermost-channel-requests channels created
// by the pre-rename bot must become private before cards post, or tickets
// fall back to System Admin DMs.
func (p *Plugin) adoptApprovalChannel(ch *model.Channel) bool {
	if ch.DeleteAt != 0 {
		return false
	}
	if ch.Type != model.ChannelTypePrivate && ch.Type != model.ChannelTypeOpen {
		return false
	}
	if _, adopted := p.adoptedApprovalChannels.Load(ch.Id); adopted {
		return true
	}
	trustedCreator := ch.CreatorId != "" && p.trustedApprovalChannelCreator(ch.CreatorId)
	if !trustedCreator {
		// Membership path only when the creator is gone (re-upload). Avoid
		// calling GetUser when the creator is already trusted.
		if !p.botIsChannelMember(ch.Id) || !p.approvalChannelCreatorGone(ch.CreatorId) {
			return false
		}
	}
	if ch.Type == model.ChannelTypeOpen {
		if !p.makeApprovalChannelPrivate(ch) {
			return false
		}
	}
	if err := p.ensureTeamMember(ch.TeamId, p.botUserID); err != nil {
		p.API.LogWarn("could not add the bot to the team of an existing approval channel",
			"channel_id", ch.Id, "channel_name", ch.Name, "error", err.Error())
		return false
	}
	if _, appErr := p.API.AddChannelMember(ch.Id, p.botUserID); appErr != nil {
		p.API.LogWarn("could not add the bot to an existing approval channel",
			"channel_id", ch.Id, "channel_name", ch.Name, "error", appErr.Error())
		return false
	}
	p.adoptedApprovalChannels.Store(ch.Id, true)
	if trustedCreator {
		p.API.LogInfo("adopted an existing approval channel: the name matched and a System Admin or this plugin's bot created it",
			"channel_id", ch.Id, "channel_name", ch.Name, "creator_id", ch.CreatorId)
	} else {
		p.API.LogInfo("adopted an existing approval channel: the bot was already a member (creator no longer trusted — e.g. after a plugin re-upload)",
			"channel_id", ch.Id, "channel_name", ch.Name, "creator_id", ch.CreatorId)
	}
	return true
}

// makeApprovalChannelPrivate converts a public approval channel to private so
// tickets are not visible to the whole team. Updates ch.Type on success.
func (p *Plugin) makeApprovalChannelPrivate(ch *model.Channel) bool {
	if ch == nil || ch.Type == model.ChannelTypePrivate {
		return true
	}
	private := ch.DeepCopy()
	private.Type = model.ChannelTypePrivate
	updated, appErr := p.API.UpdateChannel(private)
	if appErr != nil {
		p.API.LogWarn("could not convert a public approval channel to private",
			"channel_id", ch.Id, "channel_name", ch.Name, "error", appErr.Error())
		return false
	}
	if updated != nil {
		ch.Type = updated.Type
	} else {
		ch.Type = model.ChannelTypePrivate
	}
	p.API.LogInfo("converted a public approval channel to private",
		"channel_id", ch.Id, "channel_name", ch.Name)
	return true
}

// botIsChannelMember reports whether the plugin bot is an active member of
// channelID. Used to re-adopt approval channels after the creating bot user
// was replaced or purged.
func (p *Plugin) botIsChannelMember(channelID string) bool {
	if channelID == "" || p.botUserID == "" {
		return false
	}
	m, appErr := p.API.GetChannelMember(channelID, p.botUserID)
	return appErr == nil && m != nil
}

// requestHubBotUsername is the EnsureBotUser username.
const requestHubBotUsername = "mattermost-request-hub"

// legacyRequestHubPluginID is the plugin ID before the 0.5.0 rename. Bots
// that still list this as OwnerId created today's approval channels on
// upgraded servers; refuse every other owner. Username alone is never
// trusted — anyone who can create bots could register a legacy name.
const legacyRequestHubPluginID = "com.mattermostfederal.channel-requests"

// approvalChannelCreatorGone reports whether creatorID no longer resolves to
// a live user (missing or deactivated). Transient GetUser failures must not
// count as gone — that would adopt a channel while an untrusted creator still
// exists. Only not-found (and deactivated) are treated as gone.
func (p *Plugin) approvalChannelCreatorGone(creatorID string) bool {
	if creatorID == "" {
		return true
	}
	creator, appErr := p.API.GetUser(creatorID)
	if appErr != nil {
		return isNotFound(appErr)
	}
	if creator == nil {
		return true
	}
	return creator.DeleteAt != 0
}

// trustedApprovalChannelCreator reports whether userID may have created an
// approval channel the plugin adopts: a System Admin, or a Request Hub bot
// (current or legacy plugin OwnerId). Other bots are refused — never by
// username alone.
func (p *Plugin) trustedApprovalChannelCreator(userID string) bool {
	if userID == "" {
		return false
	}
	// Check bot ownership first so a deactivated / renamed-plugin creator
	// still counts — GetUser alone is not enough after a re-upload.
	if bot, appErr := p.API.GetBot(userID, true); appErr == nil && bot != nil {
		return isRequestHubBotOwner(bot.OwnerId)
	}
	creator, appErr := p.API.GetUser(userID)
	if appErr != nil || creator == nil {
		return false
	}
	return creator.IsSystemAdmin()
}

func isRequestHubBotOwner(ownerID string) bool {
	return ownerID == manifest.Id || ownerID == legacyRequestHubPluginID
}

// ensureTeamMember adds userID to the team unless they're already an active
// member.
func (p *Plugin) ensureTeamMember(teamID, userID string) error {
	if member, appErr := p.API.GetTeamMember(teamID, userID); appErr == nil && member != nil && member.DeleteAt == 0 {
		return nil
	}
	if _, appErr := p.API.CreateTeamMember(teamID, userID); appErr != nil {
		return appErr
	}
	return nil
}
