package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	kvTeamMemberRequestPrefix  = "team_member_request_"
	teamMemberDialogCallbackID = "team_member_request"
)

// teamMemberRequest is a pending request to add one or more existing users to a
// team. Routes to mattermost-team-requests; only System Admins may approve.
type teamMemberRequest struct {
	ID          string   `json:"id"`
	RequesterID string   `json:"requester_id"`
	TeamID      string   `json:"team_id"`
	NomineeIDs  []string `json:"nominee_ids"`
	Reason      string   `json:"reason,omitempty"`

	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`
}

func (p *Plugin) submitTeamMemberRequest(requesterID, teamID string, nomineeIDs []string, reason string) (string, error) {
	if !p.getConfiguration().RequestEnabled(requestTypeTeamMember) {
		return "", errors.New(teamMemberRequestsDisabledMsg)
	}
	if strings.TrimSpace(teamID) == "" {
		return "", errors.New("a team is required")
	}
	nominees := dedupeNonEmpty(nomineeIDs)
	if len(nominees) == 0 {
		return "", errors.New("pick at least one person to add to the team")
	}
	if err := checkIDList(nominees, "people to add"); err != nil {
		return "", err
	}
	if err := checkTextLen("reason", reason, maxReasonLen); err != nil {
		return "", err
	}

	team, appErr := p.API.GetTeam(teamID)
	if appErr != nil {
		p.API.LogWarn("failed to load team for team member request", "team_id", teamID, "error", appErr.Error())
		return "", errors.New("could not find that team")
	}
	if team.IsGroupConstrained() {
		return "", errors.New("this team's members are managed by groups, so people can't be added by request")
	}

	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil {
		p.API.LogWarn("failed to load requesting user", "user_id", requesterID, "error", appErr.Error())
		return "", errors.New("could not load your account; try again")
	}
	if err := p.checkNominees(nominees, nomineeRules{role: "a team member", allowBots: true, allowGuests: true}); err != nil {
		return "", err
	}

	req := &teamMemberRequest{
		ID:          model.NewId(),
		RequesterID: requesterID,
		TeamID:      teamID,
		NomineeIDs:  nominees,
		Reason:      strings.TrimSpace(reason),
	}

	// Any authenticated user may nominate people onto any team; System Admins
	// approve. Membership of the requester on the target team is intentionally
	// not required — governance is the approval card, not submit-side RBAC.
	if err := p.storeTeamMemberRequest(req); err != nil {
		return "", err
	}
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		_ = p.API.KVDelete(kvTeamMemberRequestPrefix + req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot team member request")
	}

	approvalPostID, approvalChannelID, err := p.postTeamMemberApprovalRequest(req, requester, team)
	if err != nil {
		_ = p.API.KVDelete(kvTeamMemberRequestPrefix + req.ID)
		return "", err
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	dmRootPostID, dmChannelID := p.sendTeamMemberTicketCreatedDM(req, requester, team)
	req.DMRootPostID = dmRootPostID
	req.DMChannelID = dmChannelID

	p.persistTicketPosts(ticketPersist{
		KVKey: kvTeamMemberRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "team member request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
			Kind: requestTypeTeamMember,
		},
	})

	return "Your team member request has been submitted for approval. You'll be notified once a System Admin responds.", nil
}

func (p *Plugin) sendTeamMemberTicketCreatedDM(req *teamMemberRequest, requester *model.User, team *model.Team) (dmRootPostID, dmChannelID string) {
	return p.postTicketCreatedDM(
		req.RequesterID,
		p.teamMemberApprovalAttachment(req, requester, team),
		ticketSubmittedIntro("team member request"),
	)
}

// addTeamMembers adds each nominee to the team. Returns user IDs that failed
// so callers can report partial success instead of always announcing success.
func (p *Plugin) addTeamMembers(req *teamMemberRequest) []string {
	var failed []string
	for _, userID := range req.NomineeIDs {
		if userID == "" {
			continue
		}
		if _, appErr := p.API.CreateTeamMember(req.TeamID, userID); appErr != nil {
			p.API.LogWarn("failed to add user to team", "team_id", req.TeamID, "user_id", userID, "error", appErr.Error())
			failed = append(failed, userID)
		}
	}
	return failed
}

// exceptIDs returns members of all that are not in exclude.
func exceptIDs(all, exclude []string) []string {
	skip := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		skip[id] = true
	}
	out := make([]string, 0, len(all))
	for _, id := range all {
		if id == "" || skip[id] {
			continue
		}
		out = append(out, id)
	}
	return out
}

func (p *Plugin) postTeamMemberApprovalRequest(req *teamMemberRequest, requester *model.User, team *model.Team) (postID, channelID string, err error) {
	ch := p.ensureTeamCreationApprovalChannel()
	header := p.systemAdminMentions() + " — a team member request needs System Admin review."
	return p.postApprovalAttachment(
		req.ID,
		p.teamMemberApprovalAttachment(req, requester, team),
		header,
		ch,
	)
}

func (p *Plugin) teamMemberApprovalAttachment(req *teamMemberRequest, requester *model.User, team *model.Team) *model.MessageAttachment {
	fields := []*model.MessageAttachmentField{
		{Title: "Requested by", Value: fmt.Sprintf("@%s", requester.Username), Short: true},
		{Title: "Team", Value: escapeMarkdown(team.DisplayName), Short: true},
		{Title: "Users to add", Value: p.mentionList(req.NomineeIDs), Short: false},
	}
	if req.Reason != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Reason", Value: escapeMarkdown(req.Reason), Short: false})
	}
	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   "Team member request",
		Color:   "#F4A221",
		Fields:  fields,
		Actions: p.teamMemberApprovalActions(req.ID, siteURL),
	}
}

func (p *Plugin) teamMemberApprovalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id: "approve", Name: "Approve", Type: model.PostActionTypeButton, Style: "primary",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeApproveTeamMember,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
		{
			Id: "deny", Name: "Deny", Type: model.PostActionTypeButton, Style: "danger",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeDenyTeamMember,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
	}
}

func (p *Plugin) storeTeamMemberRequest(req *teamMemberRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal team member request")
	}
	if appErr := p.API.KVSet(kvTeamMemberRequestPrefix+req.ID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to store team member request")
	}
	return nil
}

func (p *Plugin) loadTeamMemberRequest(id string) (*teamMemberRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvTeamMemberRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load team member request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req teamMemberRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal team member request")
	}
	return &req, data, nil
}

func (p *Plugin) openTeamMemberRequestDialog(triggerID, teamID string) error {
	teamName := teamID
	if t, appErr := p.API.GetTeam(teamID); appErr == nil {
		teamName = t.DisplayName
	}

	dialog := model.Dialog{
		CallbackId: teamMemberDialogCallbackID,
		Title:      "Request Team Members",
		IntroductionText: fmt.Sprintf(
			"Nominate existing Mattermost users **or bots** to be added to **%s**. System Admin approval is required.\n\n"+
				"**Adding a bot:** search for the bot username below (create the bot first via Request a Bot if needed). Same approval process as people.",
			escapeMarkdown(teamName),
		),
		SubmitLabel: "Submit request",
		State:       teamID,
		Elements: []model.DialogElement{
			{
				DisplayName: "Users or bots to add",
				Name:        fieldMembers,
				Type:        "select",
				DataSource:  "users",
				MultiSelect: true,
				Optional:    false,
				HelpText:    "Select existing users or bots not already on this team. Bots use the same flow — pick the bot account by username.",
			},
			{
				DisplayName: "Reason",
				Name:        "reason",
				Type:        "textarea",
				Optional:    true,
				HelpText:    "Optional context for reviewers.",
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeTeamMemberDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}
