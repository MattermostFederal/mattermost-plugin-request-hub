package main

import (
	"fmt"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
)

// Approve re-checks what submit checked, since a request may have been
// pending a while. Each blocker returns why a request can't be approved as it
// stands, or "" when it can. The handler answers a blocker with a private
// reply and leaves the request pending, so it can still be denied. A lookup
// that fails for any reason other than "not found" asks the approver to try
// again rather than deny.

// approvalRetryText is the private reply when the requester can't be checked.
const approvalRetryText = "Could not check the requester right now. Try again in a moment."

// approvalCheckRetryText is the private reply when anything else about the
// request can't be checked.
const approvalCheckRetryText = "Could not check this request right now. Try again in a moment."

// requesterInactiveText is the private reply when the requester's account is
// gone or deactivated.
const requesterInactiveText = "The requester's account is no longer active. Deny this request instead."

// lookupFailed reports whether appErr is an error other than "not found".
func lookupFailed(appErr *model.AppError) bool {
	return appErr != nil && appErr.StatusCode != http.StatusNotFound
}

// requesterBlocker checks that the requester, already loaded by the caller,
// still has an active account.
func (p *Plugin) requesterBlocker(requesterID string, requester *model.User, requesterErr *model.AppError) string {
	if lookupFailed(requesterErr) {
		p.API.LogWarn("failed to load requester for approval", "user_id", requesterID, "error", requesterErr.Error())
		return approvalRetryText
	}
	if requester == nil || requester.DeleteAt != 0 {
		return requesterInactiveText
	}
	return ""
}

// requesterTeamBlocker checks that a requester who isn't a System Admin is
// still on the team.
func (p *Plugin) requesterTeamBlocker(teamID string, requester *model.User) string {
	if requester.IsSystemAdmin() {
		return ""
	}
	member, appErr := p.API.GetTeamMember(teamID, requester.Id)
	if lookupFailed(appErr) {
		p.API.LogWarn("failed to check requester's team membership for approval", "user_id", requester.Id, "team_id", teamID, "error", appErr.Error())
		return approvalRetryText
	}
	if appErr != nil || member == nil || member.DeleteAt != 0 {
		return "The requester is no longer a member of the team. Deny this request instead."
	}
	return ""
}

// requesterChannelBlocker checks that a requester who isn't a System Admin is
// still in the channel.
func (p *Plugin) requesterChannelBlocker(channelID string, requester *model.User) string {
	if requester.IsSystemAdmin() {
		return ""
	}
	// Channel memberships are hard-deleted on leave, so a member that loads
	// is current.
	member, appErr := p.API.GetChannelMember(channelID, requester.Id)
	if lookupFailed(appErr) {
		p.API.LogWarn("failed to check requester's channel membership for approval", "user_id", requester.Id, "channel_id", channelID, "error", appErr.Error())
		return approvalRetryText
	}
	if appErr != nil || member == nil {
		return "The requester is no longer a member of the channel. Deny this request instead."
	}
	return ""
}

// teamBlocker checks that the team, already loaded by the caller, still
// exists. GetTeam returns archived teams too, so it checks DeleteAt.
func (p *Plugin) teamBlocker(teamID string, team *model.Team, teamErr *model.AppError) string {
	if lookupFailed(teamErr) {
		p.API.LogWarn("failed to load team for approval", "team_id", teamID, "error", teamErr.Error())
		return approvalCheckRetryText
	}
	if teamErr != nil || team == nil {
		return "The team no longer exists. Deny this request instead."
	}
	if team.DeleteAt != 0 {
		return "The team has been archived. Deny this request instead."
	}
	return ""
}

// channelBlocker checks that a channel the request acts on, already loaded
// by the caller, still exists. GetChannel returns archived channels too, so
// it checks DeleteAt.
func (p *Plugin) channelBlocker(channelID string, channel *model.Channel, appErr *model.AppError) string {
	if lookupFailed(appErr) {
		p.API.LogWarn("failed to load channel for approval", "channel_id", channelID, "error", appErr.Error())
		return approvalCheckRetryText
	}
	if appErr != nil || channel == nil {
		return "A channel in this request no longer exists. Deny this request instead."
	}
	if channel.DeleteAt != 0 {
		return fmt.Sprintf("~%s has been archived. Deny this request instead.", channel.Name)
	}
	return ""
}

// webhookChannelBlocker re-checks what requireWebhookTargetChannel checked at
// submit: the channel still exists and isn't archived, an outgoing webhook's
// channel is still public, and the requester is still in it.
func (p *Plugin) webhookChannelBlocker(channelID, webhookType string, channel *model.Channel, appErr *model.AppError, requester *model.User) string {
	if msg := p.channelBlocker(channelID, channel, appErr); msg != "" {
		return msg
	}
	if webhookType == webhookTypeOutgoing && channel.Type != model.ChannelTypeOpen {
		return fmt.Sprintf("Outgoing webhooks need a public channel, and ~%s is no longer public. Deny this request instead.", channel.Name)
	}
	return p.requesterChannelBlocker(channelID, requester)
}

// nomineeBlocker is checkNominees for approvers: the same rules, with
// approver wording and a retry when a lookup fails.
func (p *Plugin) nomineeBlocker(ids []string, rules nomineeRules) string {
	for _, id := range ids {
		user, appErr := p.API.GetUser(id)
		if lookupFailed(appErr) {
			p.API.LogWarn("failed to load nominee for approval", "user_id", id, "error", appErr.Error())
			return approvalCheckRetryText
		}
		if appErr != nil || user == nil {
			return "One of the nominees could not be found. Deny this request instead."
		}
		if user.DeleteAt != 0 {
			return fmt.Sprintf("@%s is deactivated. Deny this request instead.", user.Username)
		}
		if user.IsBot && !rules.allowBots {
			return fmt.Sprintf("@%s is a bot and can't be made %s. Deny this request instead.", user.Username, rules.role)
		}
		if user.IsGuest() && !rules.allowGuests {
			return fmt.Sprintf("@%s is a guest and can't be made %s. Deny this request instead.", user.Username, rules.role)
		}
		if rules.teamID != "" {
			member, appErr := p.API.GetTeamMember(rules.teamID, id)
			if lookupFailed(appErr) {
				p.API.LogWarn("failed to check nominee's team membership for approval", "user_id", id, "team_id", rules.teamID, "error", appErr.Error())
				return approvalCheckRetryText
			}
			if appErr != nil || member == nil || member.DeleteAt != 0 {
				return fmt.Sprintf("@%s is no longer a member of the team. Deny this request instead.", user.Username)
			}
		}
		if rules.channelID != "" {
			member, appErr := p.API.GetChannelMember(rules.channelID, id)
			if lookupFailed(appErr) {
				p.API.LogWarn("failed to check nominee's channel membership for approval", "user_id", id, "channel_id", rules.channelID, "error", appErr.Error())
				return approvalCheckRetryText
			}
			if appErr != nil || member == nil {
				return fmt.Sprintf("@%s is no longer a member of the channel. Deny this request instead.", user.Username)
			}
		}
	}
	return ""
}

// approvalBlocker re-checks a channel request.
func (p *Plugin) approvalBlocker(config *configuration, req *channelRequest, requester *model.User, requesterErr *model.AppError) string {
	if msg := p.requesterBlocker(req.RequesterID, requester, requesterErr); msg != "" {
		return msg
	}
	team, teamErr := p.API.GetTeam(req.TeamID)
	if msg := p.teamBlocker(req.TeamID, team, teamErr); msg != "" {
		return msg
	}
	if msg := p.requesterTeamBlocker(req.TeamID, requester); msg != "" {
		return msg
	}
	if !config.allowsChannelName(req.Name) {
		return "The channel name no longer matches the configured prefixes. Deny this request and ask the requester to submit it again."
	}
	return ""
}

// adminApprovalBlocker re-checks a Channel Admin request. Submit only
// requires nominees to be in the channel, so bots and guests stay allowed.
func (p *Plugin) adminApprovalBlocker(req *adminRequest, channel *model.Channel, channelErr *model.AppError, requester *model.User, requesterErr *model.AppError) string {
	if msg := p.channelBlocker(req.ChannelID, channel, channelErr); msg != "" {
		return msg
	}
	if msg := p.requesterBlocker(req.RequesterID, requester, requesterErr); msg != "" {
		return msg
	}
	if msg := p.requesterChannelBlocker(req.ChannelID, requester); msg != "" {
		return msg
	}
	return p.nomineeBlocker(req.NomineeIDs, nomineeRules{role: "a Channel Admin", allowBots: true, allowGuests: true, channelID: req.ChannelID})
}

// teamAdminApprovalBlocker re-checks a Team Admin request.
func (p *Plugin) teamAdminApprovalBlocker(req *teamAdminRequest, team *model.Team, teamErr *model.AppError) string {
	if msg := p.teamBlocker(req.TeamID, team, teamErr); msg != "" {
		return msg
	}
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	if msg := p.requesterBlocker(req.RequesterID, requester, requesterErr); msg != "" {
		return msg
	}
	if msg := p.requesterTeamBlocker(req.TeamID, requester); msg != "" {
		return msg
	}
	return p.nomineeBlocker(req.NomineeIDs, nomineeRules{role: "a Team Admin", teamID: req.TeamID})
}

// teamMemberApprovalBlocker re-checks a team member request. Submit doesn't
// require the requester to be on the team, so that isn't re-checked.
func (p *Plugin) teamMemberApprovalBlocker(req *teamMemberRequest, team *model.Team, teamErr *model.AppError) string {
	if msg := p.teamBlocker(req.TeamID, team, teamErr); msg != "" {
		return msg
	}
	if team.IsGroupConstrained() {
		return "The team's members are now managed by groups. Deny this request instead."
	}
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	if msg := p.requesterBlocker(req.RequesterID, requester, requesterErr); msg != "" {
		return msg
	}
	return p.nomineeBlocker(req.NomineeIDs, nomineeRules{role: "a team member", allowBots: true, allowGuests: true})
}

// teamGuestApprovalBlocker re-checks a guest invite request.
func (p *Plugin) teamGuestApprovalBlocker(req *teamGuestRequest, team *model.Team, teamErr *model.AppError) string {
	if msg := p.teamBlocker(req.TeamID, team, teamErr); msg != "" {
		return msg
	}
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	if msg := p.requesterBlocker(req.RequesterID, requester, requesterErr); msg != "" {
		return msg
	}
	if msg := p.requesterTeamBlocker(req.TeamID, requester); msg != "" {
		return msg
	}
	for _, chID := range req.ChannelIDs {
		channel, appErr := p.API.GetChannel(chID)
		if msg := p.channelBlocker(chID, channel, appErr); msg != "" {
			return msg
		}
		if channel.TeamId != req.TeamID {
			return fmt.Sprintf("~%s is no longer in this team. Deny this request instead.", channel.Name)
		}
		if msg := p.requesterChannelBlocker(chID, requester); msg != "" {
			return msg
		}
	}
	return ""
}

// teamApprovalBlocker re-checks a team creation request: the requester
// becomes the team's first admin, so their account must still be active.
func (p *Plugin) teamApprovalBlocker(req *teamCreationRequest) string {
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	return p.requesterBlocker(req.RequesterID, requester, requesterErr)
}

// botApprovalBlocker re-checks a bot request before either step. The owner
// follows checkBotOwner's rules, and webhook channels the rules
// requireWebhookTargetChannel applied at submit.
func (p *Plugin) botApprovalBlocker(req *botRequest, requester *model.User, requesterErr *model.AppError) string {
	if msg := p.requesterBlocker(req.RequesterID, requester, requesterErr); msg != "" {
		return msg
	}
	if req.OwnerUserID != "" && req.OwnerUserID != req.RequesterID {
		owner, appErr := p.API.GetUser(req.OwnerUserID)
		if lookupFailed(appErr) {
			p.API.LogWarn("failed to load bot owner for approval", "user_id", req.OwnerUserID, "error", appErr.Error())
			return approvalCheckRetryText
		}
		if appErr != nil || owner == nil || owner.DeleteAt != 0 || owner.IsBot {
			return "The bot owner is no longer an active user. Deny this request instead."
		}
	}
	for _, hook := range []struct{ channelID, webhookType string }{
		{req.IncomingWebhookChannelID, webhookTypeIncoming},
		{req.OutgoingWebhookChannelID, webhookTypeOutgoing},
	} {
		if hook.channelID == "" {
			continue
		}
		channel, appErr := p.API.GetChannel(hook.channelID)
		if msg := p.webhookChannelBlocker(hook.channelID, hook.webhookType, channel, appErr, requester); msg != "" {
			return msg
		}
	}
	return ""
}

// webhookApprovalBlocker re-checks a webhook request before either step.
func (p *Plugin) webhookApprovalBlocker(req *webhookRequest, requester *model.User, requesterErr *model.AppError, channel *model.Channel, channelErr *model.AppError) string {
	if msg := p.requesterBlocker(req.RequesterID, requester, requesterErr); msg != "" {
		return msg
	}
	return p.webhookChannelBlocker(req.ChannelID, req.WebhookType, channel, channelErr, requester)
}
