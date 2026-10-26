package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	kvTeamRequestPrefix  = "team_request_"
	teamDialogCallbackID = "team_request"

	// fieldRequestTeamAdmin is the bool dialog element for "also request Team Admin".
	fieldRequestTeamAdmin = "request_team_admin"
)

// teamCreationRequest is a pending request to create a new team. Routes to the
// root approval channel and can only be approved by System Admins.
type teamCreationRequest struct {
	ID               string `json:"id"`
	RequesterID      string `json:"requester_id"`
	DisplayName      string `json:"display_name"`
	Name             string `json:"name"` // resolved URL slug
	Description      string `json:"description"`
	Type             string `json:"type"` // "O" (open) or "I" (invite)
	RequestTeamAdmin bool   `json:"request_team_admin"`
	// CreatedAt is when the request was submitted, in milliseconds.
	// Requests saved by older versions get it from migratePendingRequests.
	CreatedAt int64 `json:"created_at,omitempty"`

	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`
}

// teamCreationInput is the normalized input from the dialog or webapp form.
type teamCreationInput struct {
	RequesterID      string
	DisplayName      string
	Name             string // optional URL suffix — slugified from DisplayName if blank
	Description      string
	Type             string // "O" or "I"
	RequestTeamAdmin bool
}

// submitTeamCreationRequest validates the input and stores a pending request
// for approval. AllowTeamRequests is enforced here so every entry point is gated.
func (p *Plugin) submitTeamCreationRequest(in teamCreationInput) (string, error) {
	config := p.getConfiguration()
	if !config.RequestEnabled(requestTypeTeam) {
		return "", errors.New(teamRequestsDisabledMsg)
	}

	if strings.TrimSpace(in.DisplayName) == "" {
		return "", errors.New("a team name is required")
	}
	if err := checkTextLen("team name", in.DisplayName, model.TeamDisplayNameMaxRunes); err != nil {
		return "", err
	}
	if err := checkByteLen("description", in.Description, maxTeamDescriptionBytes); err != nil {
		return "", err
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = slugify(in.DisplayName)
	} else {
		name = slugify(name)
	}
	if len(name) > model.TeamNameMaxLength || !model.IsValidTeamName(name) {
		return "", errors.Errorf("%q is not a valid team URL name; use 2-64 lowercase letters, numbers, and hyphens", name)
	}
	if model.IsReservedTeamName(name) {
		return "", errors.Errorf("%q is reserved by Mattermost; choose a different team URL name", name)
	}

	teamType := in.Type
	if teamType != "O" && teamType != "I" {
		teamType = "O"
	}

	requester, appErr := p.API.GetUser(in.RequesterID)
	if appErr != nil {
		p.API.LogWarn("failed to load requesting user", "user_id", in.RequesterID, "error", appErr.Error())
		return "", errors.New("could not load your account; try again")
	}

	req := &teamCreationRequest{
		ID:               model.NewId(),
		RequesterID:      in.RequesterID,
		DisplayName:      strings.TrimSpace(in.DisplayName),
		Name:             name,
		Description:      strings.TrimSpace(in.Description),
		Type:             teamType,
		RequestTeamAdmin: in.RequestTeamAdmin,
		CreatedAt:        model.GetMillis(),
	}

	entry := newPendingEntry(req)
	if err := p.reservePending(pendingKindTeam, req.RequesterID, entry, p.getConfiguration()); err != nil {
		return "", err
	}

	if err := p.storeTeamRequest(req); err != nil {
		p.releasePending(pendingKindTeam, req.RequesterID, req.ID)
		p.API.LogError("failed to store team request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		_ = p.API.KVDelete(kvTeamRequestPrefix + req.ID)
		p.releasePending(pendingKindTeam, req.RequesterID, req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot team request")
	}

	approvalPostID, approvalChannelID, err := p.postTeamCreationApprovalRequest(req, requester)
	if err != nil {
		_ = p.API.KVDelete(kvTeamRequestPrefix + req.ID)
		p.releasePending(pendingKindTeam, req.RequesterID, req.ID)
		p.API.LogError("failed to post team request for approval", "error", err.Error())
		return "", errors.New("could not send your request to the approvers; try again or contact a System Admin")
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	dmRootPostID, dmChannelID := p.sendTeamCreationTicketCreatedDM(req, requester)
	req.DMRootPostID = dmRootPostID
	req.DMChannelID = dmChannelID

	p.persistTicketPosts(ticketPersist{
		KVKey: kvTeamRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "team request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
			Kind: requestTypeTeam,
		},
	})

	return "Your team creation request has been submitted for approval. You'll be notified once an approver responds.", nil
}

func (p *Plugin) sendTeamCreationTicketCreatedDM(req *teamCreationRequest, requester *model.User) (dmRootPostID, dmChannelID string) {
	return p.postTicketCreatedDM(
		req.RequesterID,
		p.teamCreationApprovalAttachment(req, requester),
		ticketSubmittedIntro("team creation request"),
	)
}

// createTeamForRequest creates the team, adds the requester as a member, and
// optionally promotes them to Team Admin if RequestTeamAdmin is true.
func (p *Plugin) createTeamForRequest(req *teamCreationRequest) (*model.Team, error) {
	team, appErr := p.API.CreateTeam(&model.Team{
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Description: req.Description,
		Type:        req.Type,
	})
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to create team")
	}
	if _, appErr := p.API.CreateTeamMember(team.Id, req.RequesterID); appErr != nil {
		p.API.LogWarn("failed to add requester to new team", "team_id", team.Id, "user_id", req.RequesterID, "error", appErr.Error())
	}
	if req.RequestTeamAdmin {
		if _, appErr := p.API.UpdateTeamMemberRoles(team.Id, req.RequesterID, teamAdminRoles); appErr != nil {
			p.API.LogWarn("failed to promote requester to team admin in new team", "team_id", team.Id, "user_id", req.RequesterID, "error", appErr.Error())
		}
	}
	// Stand up the per-team channel-requests channel only when someone can
	// monitor it (Team Admin or System Admin on the team). Creating it with no
	// local approvers leaves an empty private channel; promoteTeamAdmins /
	// UserHasJoinedTeam create and seed it once the first approver exists.
	p.syncTeamApprovalChannelAfterAdminChange(team.Id)
	return team, nil
}

func (p *Plugin) postTeamCreationApprovalRequest(req *teamCreationRequest, requester *model.User) (postID, channelID string, err error) {
	ch := p.ensureTeamCreationApprovalChannel()
	header := p.systemAdminMentions() + " — a team creation request needs System Admin review."
	if ch != nil && p.getConfiguration().AllowTeamAdminTeamApprovers {
		// Approval-team Team Admins can act on this card, so they need to be
		// in the channel and notified.
		p.addTeamApproversToChannel(ch.TeamId, ch.Id)
		if mentions := p.mentionList(p.adminApproverIDs(ch.TeamId)); mentions != "" {
			header = mentions + " — a team creation request needs your review."
		}
	}
	return p.postApprovalAttachment(
		req.ID,
		p.teamCreationApprovalAttachment(req, requester),
		header,
		ch,
	)
}

func (p *Plugin) teamCreationApprovalAttachment(req *teamCreationRequest, requester *model.User) *model.MessageAttachment {
	visibility := "Open"
	if req.Type == "I" {
		visibility = "Invite only"
	}
	fields := []*model.MessageAttachmentField{
		{Title: "Requested by", Value: fmt.Sprintf("@%s", requester.Username), Short: true},
		{Title: "Visibility", Value: visibility, Short: true},
		{Title: "Team name", Value: escapeMarkdown(req.DisplayName), Short: true},
		{Title: "URL", Value: req.Name, Short: true},
	}

	// Team Admin escalation goes in its own always-visible field so approvers
	// can't miss it — Team Admin grants broad permissions across the whole team
	// and requires conscious review, unlike a basic team creation.
	if req.RequestTeamAdmin {
		fields = append(fields, &model.MessageAttachmentField{
			Title: "⚠️ Privilege Escalation — Team Admin Requested",
			Value: fmt.Sprintf(
				"@%s is also requesting **Team Admin** access for this team. "+
					"Team Admin grants broad permissions across the entire team — "+
					"ability to manage members, channels, and team settings. "+
					"Approve this only if the requester should hold administrative control.",
				p.usernameOrID(req.RequesterID),
			),
			Short: false,
		})
	}

	var details []string
	if req.Description != "" {
		details = append(details, fmt.Sprintf("**Description:** %s", escapeMarkdown(req.Description)))
	}

	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   "Team creation request",
		Color:   "#6A33C2",
		Fields:  fields,
		Text:    strings.Join(details, "\n"),
		Actions: p.teamCreationApprovalActions(req.ID, siteURL),
	}
}

// usernameOrID returns the username for display, falling back to the raw user
// ID when the lookup fails (used in non-critical display text only).
func (p *Plugin) usernameOrID(userID string) string {
	user, appErr := p.API.GetUser(userID)
	if appErr != nil || user == nil {
		return userID
	}
	return user.Username
}

func (p *Plugin) teamCreationApprovalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id: "approve", Name: "Approve", Type: model.PostActionTypeButton, Style: "primary",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeApproveTeam,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
		{
			Id: "deny", Name: "Deny", Type: model.PostActionTypeButton, Style: "danger",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeDenyTeam,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
	}
}

func (p *Plugin) storeTeamRequest(req *teamCreationRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal team request")
	}
	if appErr := p.API.KVSet(kvTeamRequestPrefix+req.ID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to store team request")
	}
	return nil
}

func (p *Plugin) loadTeamRequest(id string) (*teamCreationRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvTeamRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load team request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req teamCreationRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal team request")
	}
	return &req, data, nil
}

// openTeamCreationRequestDialog opens the interactive dialog for submitting a
// team creation request.
func (p *Plugin) openTeamCreationRequestDialog(triggerID string) error {
	dialog := model.Dialog{
		CallbackId:       teamDialogCallbackID,
		Title:            "Request a New Team",
		IntroductionText: "Team creation requests are reviewed by System Admins.",
		SubmitLabel:      "Submit request",
		Elements: []model.DialogElement{
			{
				DisplayName: "Team name",
				Name:        fieldDisplayName,
				Type:        "text",
				Placeholder: "e.g. Marketing",
				MaxLength:   model.TeamDisplayNameMaxRunes,
			},
			{
				DisplayName: "URL name",
				Name:        fieldName,
				Type:        "text",
				Optional:    true,
				HelpText:    "Lowercase letters, numbers, and hyphens. Generated from team name if blank.",
				MaxLength:   model.TeamNameMaxLength,
			},
			{
				DisplayName: "Description",
				Name:        fieldPurpose,
				Type:        "textarea",
				Optional:    true,
				MaxLength:   maxTeamDescriptionBytes,
			},
			{
				DisplayName: "Type",
				Name:        fieldType,
				Type:        "radio",
				Default:     "O",
				Options: []*model.PostActionOptions{
					{Text: "Open (anyone can join)", Value: "O"},
					{Text: "Invite only", Value: "I"},
				},
			},
			{
				DisplayName: "Request Team Admin role",
				Name:        fieldRequestTeamAdmin,
				Type:        "bool",
				Optional:    true,
				HelpText:    "Also request Team Admin access for this new team. Requires separate approval.",
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeTeamDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}
