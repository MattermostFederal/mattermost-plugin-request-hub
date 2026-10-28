package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	kvBotRequestPrefix  = "bot_request_"
	botDialogCallbackID = "bot_request"

	fieldBotUsername    = "bot_username"
	fieldBotDisplayName = "bot_display_name"
	fieldBotDescription = "bot_description"
	fieldBotOwner       = "bot_owner"

	// Bounds on a requested bot username.
	minBotUsernameLen = 3
	maxBotUsernameLen = 22
)

// botRequest is a pending request to create a new bot account. Because bots
// carry integration credentials and system-wide access, these requests route
// to the bot approval channel and require two-step approval (security + system).
type botRequest struct {
	ID          string `json:"id"`
	RequesterID string `json:"requester_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	OwnerUserID string `json:"owner_user_id,omitempty"`

	// Optional extras — requested alongside the bot account in the same ticket.
	// All three are created at approve-time using the approver's session token.
	RequestToken               bool   `json:"request_token,omitempty"`
	IncomingWebhookChannelID   string `json:"incoming_webhook_channel_id,omitempty"`
	IncomingWebhookDisplayName string `json:"incoming_webhook_display_name,omitempty"`
	OutgoingWebhookChannelID   string `json:"outgoing_webhook_channel_id,omitempty"`
	OutgoingWebhookDisplayName string `json:"outgoing_webhook_display_name,omitempty"`
	OutgoingWebhookCallbackURL string `json:"outgoing_webhook_callback_url,omitempty"`
	// CreatedAt is when the request was submitted, in milliseconds.
	// Requests saved by older versions get it from migratePendingRequests.
	CreatedAt int64 `json:"created_at,omitempty"`
	// Exercise marks a request seeded by exercise mode.
	Exercise bool `json:"exercise,omitempty"`
	// ExerciseTeamID is the team whose exercise data records what
	// approving this request creates.
	ExerciseTeamID string `json:"exercise_team_id,omitempty"`

	// Ticket messaging fields — set after the approval post and DM are created.
	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`

	// twoStepState tracks security + system approvals. Missing fields on
	// older KV records unmarshal as empty (both steps still required).
	twoStepState
}

// botRequestInput carries the caller-supplied fields for a new bot request.
// The slash-command dialog path populates only the base fields; the webapp
// path additionally sets the optional token/webhook fields.
type botRequestInput struct {
	Username                   string
	DisplayName                string
	Description                string
	OwnerUserID                string
	RequestToken               bool
	IncomingWebhookChannelID   string
	IncomingWebhookDisplayName string
	OutgoingWebhookChannelID   string
	OutgoingWebhookDisplayName string
	OutgoingWebhookCallbackURL string
}

// submitBotRequest validates the input and stores a pending request for
// two-step approval.
func (p *Plugin) submitBotRequest(requesterID string, input botRequestInput) (string, error) {
	if !p.getConfiguration().RequestEnabled(requestTypeBot) {
		return "", errors.New(botRequestsDisabledMsg)
	}
	username := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(input.Username, "@")))
	if username == "" {
		return "", errors.New("A bot username is required.")
	}
	if len(username) < minBotUsernameLen || len(username) > maxBotUsernameLen {
		return "", errors.Errorf("Bot username must be %d-%d characters.", minBotUsernameLen, maxBotUsernameLen)
	}
	if !model.IsValidUsername(username) {
		return "", errors.New("Bot username must be lowercase letters, numbers, and . - _ (3-22 characters).")
	}
	if strings.TrimSpace(input.DisplayName) == "" {
		return "", errors.New("A display name is required.")
	}
	for _, check := range []error{
		checkTextLen("Display name", input.DisplayName, maxDisplayNameLen),
		checkTextLen("Description", input.Description, maxDescriptionLen),
		checkByteLen("Incoming webhook name", input.IncomingWebhookDisplayName, maxWebhookNameBytes),
		checkByteLen("Outgoing webhook name", input.OutgoingWebhookDisplayName, maxWebhookNameBytes),
	} {
		if check != nil {
			return "", check
		}
	}
	if strings.TrimSpace(input.OutgoingWebhookChannelID) != "" {
		if err := checkCallbackURL(input.OutgoingWebhookCallbackURL); err != nil {
			return "", err
		}
	}

	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil {
		p.API.LogWarn("failed to load requesting user", "user_id", requesterID, "error", appErr.Error())
		return "", errors.New("could not load your account; try again")
	}
	if err := p.checkBotOwner(requesterID, strings.TrimSpace(input.OwnerUserID)); err != nil {
		return "", err
	}

	incomingChID := strings.TrimSpace(input.IncomingWebhookChannelID)
	outgoingChID := strings.TrimSpace(input.OutgoingWebhookChannelID)
	if incomingChID != "" {
		if _, err := p.requireWebhookTargetChannel(requester, incomingChID, webhookTypeIncoming); err != nil {
			return "", errors.Wrap(err, "incoming webhook channel")
		}
	}
	if outgoingChID != "" {
		if _, err := p.requireWebhookTargetChannel(requester, outgoingChID, webhookTypeOutgoing); err != nil {
			return "", errors.Wrap(err, "outgoing webhook channel")
		}
	}

	req := &botRequest{
		ID:                         model.NewId(),
		RequesterID:                requesterID,
		Username:                   username,
		DisplayName:                strings.TrimSpace(input.DisplayName),
		Description:                strings.TrimSpace(input.Description),
		OwnerUserID:                strings.TrimSpace(input.OwnerUserID),
		RequestToken:               input.RequestToken,
		IncomingWebhookChannelID:   incomingChID,
		IncomingWebhookDisplayName: strings.TrimSpace(input.IncomingWebhookDisplayName),
		OutgoingWebhookChannelID:   outgoingChID,
		OutgoingWebhookDisplayName: strings.TrimSpace(input.OutgoingWebhookDisplayName),
		OutgoingWebhookCallbackURL: strings.TrimSpace(input.OutgoingWebhookCallbackURL),
		CreatedAt:                  model.GetMillis(),
	}

	entry := newPendingEntry(req)
	if err := p.reservePending(pendingKindBot, req.RequesterID, entry, p.getConfiguration()); err != nil {
		return "", err
	}

	if err := p.storeBotRequest(req); err != nil {
		p.releasePending(pendingKindBot, req.RequesterID, req.ID)
		p.API.LogError("failed to store bot request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		_ = p.API.KVDelete(kvBotRequestPrefix + req.ID)
		p.releasePending(pendingKindBot, req.RequesterID, req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot bot request")
	}

	approvalPostID, approvalChannelID, err := p.postBotApprovalRequest(req, requester)
	if err != nil {
		_ = p.API.KVDelete(kvBotRequestPrefix + req.ID)
		p.releasePending(pendingKindBot, req.RequesterID, req.ID)
		p.API.LogError("failed to post bot request for approval", "error", err.Error())
		return "", errors.New("could not send your request to the approvers; try again or contact a System Admin")
	}
	if approvalPostID == "" {
		// The card went to System Admins as direct-message copies.
		p.resolveCardsIfHandled(pendingKindBot, req.ID)
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	dmRootPostID, dmChannelID := p.sendBotTicketCreatedDM(req, requester)
	req.DMRootPostID = dmRootPostID
	req.DMChannelID = dmChannelID

	p.persistTicketPosts(ticketPersist{
		KVKey: kvBotRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "bot request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
			Kind: requestTypeBot,
		},
	})

	return "Your bot account request has been submitted for approval. It needs a security approval and a system approval. You'll be notified as each step completes.", nil
}

func (p *Plugin) sendBotTicketCreatedDM(req *botRequest, requester *model.User) (dmRootPostID, dmChannelID string) {
	return p.postTicketCreatedDM(
		req.RequesterID,
		p.botApprovalAttachment(req, requester),
		ticketSubmittedIntro("bot account request"),
	)
}

// createBotForRequest creates the Mattermost bot account. OwnerId is set to
// the selected owner (if provided) or the requester.
func (p *Plugin) createBotForRequest(req *botRequest) (*model.Bot, error) {
	ownerID := req.OwnerUserID
	if ownerID == "" {
		ownerID = req.RequesterID
	}
	bot, appErr := p.API.CreateBot(&model.Bot{
		Username:    req.Username,
		DisplayName: req.DisplayName,
		Description: req.Description,
		OwnerId:     ownerID,
	})
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to create bot")
	}
	return bot, nil
}

// postBotApprovalRequest posts to the dedicated bot-requests approval channel.
func (p *Plugin) postBotApprovalRequest(req *botRequest, requester *model.User) (postID, channelID string, err error) {
	ch := p.ensureBotApprovalChannel()
	// Light reconcile (group member list only) so new/re-added security
	// approvers see this card without waiting for the 2m ticker.
	go p.reconcileTwoStepApproverChannels()
	return p.postApprovalAttachment(
		req.ID,
		p.botApprovalAttachment(req, requester),
		p.botApprovalHeader(req.Exercise),
		ch,
	)
}

// botApprovalHeader is the bot card's header. Exercise cards mention
// nobody, so seeding doesn't notify the channel.
func (p *Plugin) botApprovalHeader(exercise bool) string {
	return cardMention(exercise) + "a bot account request needs review (requires a security approval and a system approval)."
}

// botRetryHeader is the bot card's header after a failed final approval.
func botRetryHeader(exercise bool) string {
	return cardMention(exercise) + "a bot account request needs review (a previous approval attempt failed)."
}

func (p *Plugin) botApprovalAttachment(req *botRequest, requester *model.User) *model.MessageAttachment {
	fields := []*model.MessageAttachmentField{
		{Title: "Requested by", Value: fmt.Sprintf("@%s", requester.Username), Short: true},
		{Title: "Bot username", Value: fmt.Sprintf("@%s", req.Username), Short: true},
	}

	var details []string
	details = append(details, fmt.Sprintf("**Display name:** %s", escapeMarkdown(req.DisplayName)))
	if req.Description != "" {
		details = append(details, fmt.Sprintf("**Description:** %s", escapeMarkdown(req.Description)))
	}
	if req.OwnerUserID != "" {
		details = append(details, fmt.Sprintf("**Owner:** %s", p.mentionList([]string{req.OwnerUserID})))
	}
	if req.RequestToken {
		details = append(details, "**Token:** requested (generated at approve-time, sent in DM)")
	}
	if req.IncomingWebhookChannelID != "" {
		name := req.IncomingWebhookDisplayName
		if name == "" {
			name = "incoming webhook"
		}
		details = append(details, fmt.Sprintf("**Incoming webhook:** %s → %s", escapeMarkdown(name), p.channelDisplayRef(req.IncomingWebhookChannelID)))
	}
	if req.OutgoingWebhookChannelID != "" {
		name := req.OutgoingWebhookDisplayName
		if name == "" {
			name = "outgoing webhook"
		}
		details = append(details, fmt.Sprintf("**Outgoing webhook:** %s → %s (callback: %s)", escapeMarkdown(name), p.channelDisplayRef(req.OutgoingWebhookChannelID), escapeMarkdown(req.OutgoingWebhookCallbackURL)))
	}

	fields = append(fields, &model.MessageAttachmentField{
		Title: "Approvals", Value: p.twoStepStatusValue(req.twoStepState), Short: false,
	})

	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   exerciseLabel(req.Exercise) + "Bot account request",
		Color:   "#8B008B",
		Fields:  fields,
		Footer:  requestIDFooter(req.ID),
		Text:    strings.Join(details, "\n"),
		Actions: p.botApprovalActions(req.ID, siteURL),
	}
}

func (p *Plugin) botApprovalAttachmentWithNotice(req *botRequest, requester *model.User, notice string) *model.MessageAttachment {
	return p.botApprovalAttachmentWithTitledNotice(req, requester, "⚠️ Action needed", notice)
}

func (p *Plugin) botApprovalAttachmentWithTitledNotice(req *botRequest, requester *model.User, title, notice string) *model.MessageAttachment {
	att := p.botApprovalAttachment(req, requester)
	att.Color = "#D24B4E"
	if title == "" {
		title = "⚠️ Action needed"
	}
	att.Fields = append([]*model.MessageAttachmentField{
		{Title: title, Value: notice, Short: false},
	}, att.Fields...)
	return att
}

func (p *Plugin) botApprovalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id: "approve", Name: approveButtonLabel, Type: model.PostActionTypeButton, Style: "primary",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeApproveBot,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
		{
			Id: "deny", Name: "Deny", Type: model.PostActionTypeButton, Style: "danger",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeDenyBot,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
	}
}

// channelDisplayRef returns "#channel-name (Team: DisplayName)" for display
// in approval cards. Falls back gracefully when either lookup fails.
func (p *Plugin) channelDisplayRef(channelID string) string {
	ch, appErr := p.API.GetChannel(channelID)
	if appErr != nil {
		return channelID
	}
	team, appErr := p.API.GetTeam(ch.TeamId)
	if appErr != nil {
		return "#" + ch.Name
	}
	return fmt.Sprintf("#%s (Team: %s)", ch.Name, escapeMarkdown(team.DisplayName))
}

// checkBotOwner accepts an empty owner (the requester owns the bot), the
// requester, or another active user who isn't a bot.
func (p *Plugin) checkBotOwner(requesterID, ownerID string) error {
	if ownerID == "" || ownerID == requesterID {
		return nil
	}
	if !model.IsValidId(ownerID) {
		return errors.New("The bot owner must be picked from the list.")
	}
	owner, appErr := p.API.GetUser(ownerID)
	if appErr != nil || owner == nil || owner.DeleteAt != 0 || owner.IsBot {
		return errors.New("The bot owner must be an active user.")
	}
	return nil
}

// channelNamesList resolves channel IDs to ~name for approval cards, marking
// private channels. Uses the raw ID when a lookup fails so reviewers still
// see something actionable.
func (p *Plugin) channelNamesList(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		ch, appErr := p.API.GetChannel(id)
		if appErr != nil || ch == nil {
			parts = append(parts, id)
			continue
		}
		name := "~" + ch.Name
		if ch.Type == model.ChannelTypePrivate {
			name += " (private)"
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

func (p *Plugin) storeBotRequest(req *botRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal bot request")
	}
	if appErr := p.API.KVSet(kvBotRequestPrefix+req.ID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to store bot request")
	}
	return nil
}

func (p *Plugin) loadBotRequest(id string) (*botRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvBotRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load bot request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req botRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal bot request")
	}
	return &req, data, nil
}

// openBotRequestDialog opens the interactive dialog for submitting a bot
// account request.
func (p *Plugin) openBotRequestDialog(triggerID string) error {
	dialog := model.Dialog{
		CallbackId:       botDialogCallbackID,
		Title:            "Request a Bot Account",
		IntroductionText: "Bot account requests need a security approval and a system approval.",
		SubmitLabel:      "Submit request",
		Elements: []model.DialogElement{
			{
				DisplayName: "Bot username",
				Name:        fieldBotUsername,
				Type:        "text",
				Placeholder: "my-integration-bot",
				HelpText:    "The bot's @handle. Lowercase letters, numbers, and . - _ (3-22 characters).",
				MaxLength:   maxBotUsernameLen,
			},
			{
				DisplayName: "Display name",
				Name:        fieldBotDisplayName,
				Type:        "text",
				Placeholder: "My Integration Bot",
				MaxLength:   maxDisplayNameLen,
			},
			{
				DisplayName: "Description",
				Name:        fieldBotDescription,
				Type:        "textarea",
				Optional:    true,
				Placeholder: "What does this bot do? What systems will it integrate with?",
				MaxLength:   maxDescriptionLen,
			},
			{
				DisplayName: "Bot owner",
				Name:        fieldBotOwner,
				Type:        "select",
				DataSource:  "users",
				Optional:    true,
				HelpText:    "The user responsible for this bot. Defaults to you if blank.",
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeBotDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}
