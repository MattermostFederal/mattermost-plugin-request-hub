package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	kvTeamAdminRequestPrefix  = "team_admin_request_"
	teamAdminDialogCallbackID = "team_admin_request"
	// teamAdminRoles is the roles string granting Team Admin on a team.
	teamAdminRoles = "team_user team_admin"
)

// teamAdminRequest is a pending request to promote one or more users to Team
// Admin on a specific team. Because Team Admin grants broad permissions across
// the whole team, these requests go to the team requests channel in the root
// approval team (or to every System Admin by direct message when that channel
// isn't available) and can only be approved by System Admins.
type teamAdminRequest struct {
	ID          string   `json:"id"`
	RequesterID string   `json:"requester_id"`
	TeamID      string   `json:"team_id"`
	NomineeIDs  []string `json:"nominee_ids"`
	// CreatedAt is when the request was submitted, in milliseconds.
	// Requests saved by older versions get it from migratePendingRequests.
	CreatedAt int64 `json:"created_at,omitempty"`

	// Ticket messaging fields — set after the approval post and DM are created.
	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`
}

// submitTeamAdminRequest validates the input, stores a pending request and
// posts it to the team-requests approval channel. Requests from System Admins
// queue too.
func (p *Plugin) submitTeamAdminRequest(requesterID, teamID string, nomineeIDs []string) (string, error) {
	if !p.getConfiguration().RequestEnabled(requestTypeTeamAdmin) {
		return "", errors.New(teamAdminRequestsDisabledMsg)
	}
	if strings.TrimSpace(teamID) == "" {
		return "", errors.New("a team is required")
	}
	nominees := dedupeNonEmpty(nomineeIDs)
	if len(nominees) == 0 {
		return "", errors.New("pick at least one person to make a Team Admin")
	}
	if err := checkIDList(nominees, "Team Admin nominees"); err != nil {
		return "", err
	}

	team, appErr := p.API.GetTeam(teamID)
	if appErr != nil {
		p.API.LogWarn("failed to load team for Team Admin request", "team_id", teamID, "error", appErr.Error())
		return "", errors.New("could not find that team")
	}

	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil {
		p.API.LogWarn("failed to load requesting user", "user_id", requesterID, "error", appErr.Error())
		return "", errors.New("could not load your account; try again")
	}

	// Requester must be an active member of the team they're nominating into.
	// System Admins are exempt — they govern all teams.
	if !requester.IsSystemAdmin() {
		member, appErr := p.API.GetTeamMember(teamID, requesterID)
		if appErr != nil || member == nil || member.DeleteAt != 0 {
			return "", errors.New("you must be a member of the team to request Team Admin access")
		}
	}
	if err := p.checkNominees(nominees, nomineeRules{role: "a Team Admin", teamID: teamID}); err != nil {
		return "", err
	}

	req := &teamAdminRequest{
		ID:          model.NewId(),
		RequesterID: requesterID,
		TeamID:      teamID,
		NomineeIDs:  nominees,
		CreatedAt:   model.GetMillis(),
	}

	entry := newPendingEntry(req)
	if err := p.reservePending(pendingKindTeamAdmin, req.RequesterID, entry, p.getConfiguration()); err != nil {
		return "", err
	}

	if err := p.storeTeamAdminRequest(req); err != nil {
		p.releasePending(pendingKindTeamAdmin, req.RequesterID, req.ID)
		p.API.LogError("failed to store team admin request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		_ = p.API.KVDelete(kvTeamAdminRequestPrefix + req.ID)
		p.releasePending(pendingKindTeamAdmin, req.RequesterID, req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot team admin request")
	}

	approvalPostID, approvalChannelID, err := p.postTeamAdminApprovalRequest(req, requester, team)
	if err != nil {
		_ = p.API.KVDelete(kvTeamAdminRequestPrefix + req.ID)
		p.releasePending(pendingKindTeamAdmin, req.RequesterID, req.ID)
		p.API.LogError("failed to post team admin request for approval", "error", err.Error())
		return "", errors.New("could not send your request to the approvers; try again or contact a System Admin")
	}
	if approvalPostID == "" {
		// The card went to System Admins as direct-message copies.
		p.resolveCardsIfHandled(pendingKindTeamAdmin, req.ID)
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	dmRootPostID, dmChannelID := p.sendTeamAdminTicketCreatedDM(req, requester, team)
	req.DMRootPostID = dmRootPostID
	req.DMChannelID = dmChannelID

	p.persistTicketPosts(ticketPersist{
		KVKey: kvTeamAdminRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "team admin request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
			Kind: requestTypeTeamAdmin,
		},
	})

	return "Your Team Admin request has been submitted for approval. You'll be notified once an approver responds.", nil
}

func (p *Plugin) sendTeamAdminTicketCreatedDM(req *teamAdminRequest, requester *model.User, team *model.Team) (dmRootPostID, dmChannelID string) {
	return p.postTicketCreatedDM(
		req.RequesterID,
		p.teamAdminApprovalAttachment(req, requester, team),
		ticketSubmittedIntro("Team Admin request"),
	)
}

// promoteTeamAdmins adds each nominee to the team (ensuring membership) then
// grants the team_admin scheme role. After promotion it reseeds the per-team
// channel-requests channel: UserHasJoinedTeam only runs on join, not when an
// existing member is promoted, so newly minted Team Admins would otherwise
// never see that channel.
func (p *Plugin) promoteTeamAdmins(req *teamAdminRequest) {
	for _, userID := range req.NomineeIDs {
		if userID == "" {
			continue
		}
		if _, appErr := p.API.CreateTeamMember(req.TeamID, userID); appErr != nil {
			p.API.LogWarn("failed to add nominee to team", "team_id", req.TeamID, "user_id", userID, "error", appErr.Error())
		}
		if _, appErr := p.API.UpdateTeamMemberRoles(req.TeamID, userID, teamAdminRoles); appErr != nil {
			p.API.LogWarn("failed to promote nominee to team admin", "team_id", req.TeamID, "user_id", userID, "error", appErr.Error())
		}
	}
	p.syncTeamApprovalChannelAfterAdminChange(req.TeamID)
}

// postTeamAdminApprovalRequest posts the card where the team's approvers can
// act on it: the team's own approval channel when PerTeamApprovalChannels is
// on and the team has an approver, otherwise the team requests channel next
// to team creation requests. System Admins and the team's Team Admins (see
// canApproveTeamAdminRequest) are mentioned.
func (p *Plugin) postTeamAdminApprovalRequest(req *teamAdminRequest, requester *model.User, team *model.Team) (postID, channelID string, err error) {
	var ch *model.Channel
	if p.getConfiguration().PerTeamApprovalChannels && p.hasActiveTeamApprovers(team.Id) {
		teamCh, chErr := p.ensureTeamApprovalChannel(team.Id)
		if chErr != nil {
			p.API.LogWarn("failed to resolve per-team approval channel for Team Admin request; using team requests channel",
				"team_id", team.Id, "error", chErr.Error())
		}
		ch = teamCh
	}
	if ch == nil {
		ch = p.ensureTeamCreationApprovalChannel()
	}
	header := "A Team Admin request needs your review."
	if mentions := p.mentionList(p.adminApproverIDs(team.Id)); mentions != "" {
		header = mentions + " — a Team Admin request needs your review."
	}
	return p.postApprovalAttachment(
		req.ID,
		p.teamAdminApprovalAttachment(req, requester, team),
		header,
		ch,
	)
}

func (p *Plugin) teamAdminApprovalAttachment(req *teamAdminRequest, requester *model.User, team *model.Team) *model.MessageAttachment {
	nominees := p.mentionList(req.NomineeIDs)
	fields := []*model.MessageAttachmentField{
		{Title: "Requested by", Value: fmt.Sprintf("@%s", requester.Username), Short: true},
		{Title: "Team", Value: escapeMarkdown(team.DisplayName), Short: true},
		{Title: "Proposed Team Admins", Value: nominees, Short: false},
		{
			Title: "⚠️ Privilege Escalation — Team Admin",
			Value: "Team Admin grants broad permissions across the entire team: " +
				"manage members, create and archive channels, edit team settings, " +
				"and view all private channels. Only approve if this level of access is appropriate.",
			Short: false,
		},
	}
	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   "Team Admin request",
		Color:   "#F4A221",
		Fields:  fields,
		Footer:  requestIDFooter(req.ID),
		Actions: p.teamAdminApprovalActions(req.ID, siteURL),
	}
}

func (p *Plugin) teamAdminApprovalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id: "approve", Name: "Approve", Type: model.PostActionTypeButton, Style: "primary",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeApproveTeamAdmin,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
		{
			Id: "deny", Name: "Deny", Type: model.PostActionTypeButton, Style: "danger",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeDenyTeamAdmin,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
	}
}

// systemAdminMentions returns a mention string for all System Admins,
// used in root-channel approval post headers so the right people are notified.
func (p *Plugin) systemAdminMentions() string {
	admins, appErr := p.API.GetUsers(&model.UserGetOptions{Role: model.SystemAdminRoleId, Page: 0, PerPage: 100})
	if appErr != nil || len(admins) == 0 {
		return "@channel"
	}
	ids := make([]string, 0, len(admins))
	for _, a := range admins {
		if a.Id != p.botUserID {
			ids = append(ids, a.Id)
		}
	}
	mentions := p.mentionList(ids)
	if mentions == "" {
		return "@channel"
	}
	return mentions
}

func (p *Plugin) storeTeamAdminRequest(req *teamAdminRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal team admin request")
	}
	if appErr := p.API.KVSet(kvTeamAdminRequestPrefix+req.ID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to store team admin request")
	}
	return nil
}

func (p *Plugin) loadTeamAdminRequest(id string) (*teamAdminRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvTeamAdminRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load team admin request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req teamAdminRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal team admin request")
	}
	return &req, data, nil
}

// openTeamAdminRequestDialog opens the interactive dialog for submitting a
// Team Admin promotion request for the user's current team.
func (p *Plugin) openTeamAdminRequestDialog(triggerID, teamID string) error {
	teamName := teamID
	if t, appErr := p.API.GetTeam(teamID); appErr == nil {
		teamName = t.DisplayName
	}

	dialog := model.Dialog{
		CallbackId: teamAdminDialogCallbackID,
		Title:      "Request Team Admin Promotion",
		IntroductionText: fmt.Sprintf(
			"Nominate users to become Team Admins in **%s**. System Admin approval is required because Team Admin grants broad permissions across the whole team.",
			escapeMarkdown(teamName),
		),
		SubmitLabel: "Submit request",
		State:       teamID,
		Elements: []model.DialogElement{
			{
				DisplayName: "Users to promote",
				Name:        fieldMembers,
				Type:        "select",
				DataSource:  "users",
				MultiSelect: true,
				HelpText:    "Select one or more users to nominate for Team Admin. Nominees must already be members of the team.",
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeTeamAdminDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}
