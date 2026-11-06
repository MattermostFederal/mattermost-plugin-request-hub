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
	fieldBotOwner       = "bot_owner"
	fieldBotCompliance  = "bot_compliance"

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
	// Compliance is the requester's acceptance of the compliance
	// statement; nil when none was required.
	Compliance *botCompliance `json:"compliance,omitempty"`

	// What the requester declared about the bot (see bot_declaration.go),
	// and its home team and channel, which the bot joins on approval.
	// Requests saved by older versions have none of them.
	Purpose            string `json:"purpose,omitempty"`
	ArchitectureURL    string `json:"architecture_url,omitempty"`
	IntegrationURL     string `json:"integration_endpoint,omitempty"`
	PrivilegeLevel     string `json:"privilege_level,omitempty"`
	MMPrivilege        string `json:"mm_privilege,omitempty"`
	DataIngestion      string `json:"data_ingestion,omitempty"`
	HostingEnv         string `json:"hosting_env,omitempty"`
	HostingOtherDetail string `json:"hosting_other_detail,omitempty"`
	HomeTeamID         string `json:"home_team_id,omitempty"`
	HomeChannelID      string `json:"home_channel_id,omitempty"`

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
// Every path sets the account fields, the declaration and the home team and
// channel; only the webapp path sets the token/webhook extras.
type botRequestInput struct {
	Username                   string
	DisplayName                string
	OwnerUserID                string
	Purpose                    string
	ArchitectureURL            string
	IntegrationURL             string
	HostingEnv                 string
	HostingOtherDetail         string
	HomeTeamID                 string
	HomeChannelID              string
	RequestToken               bool
	IncomingWebhookChannelID   string
	IncomingWebhookDisplayName string
	OutgoingWebhookChannelID   string
	OutgoingWebhookDisplayName string
	OutgoingWebhookCallbackURL string
	// ComplianceAccepted is the requester's tick on the compliance
	// statement; ComplianceTextHash the hash of the statement they were
	// shown, required while the statement is.
	ComplianceAccepted bool
	ComplianceTextHash string
}

// submitBotRequest validates the input and stores a pending request for
// two-step approval.
func (p *Plugin) submitBotRequest(requesterID string, input botRequestInput) (submitted string, submitErr error) {
	sa := p.startSubmitAudit(pendingKindBot, requesterID)
	defer sa.done(&submitErr)

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
	_, appErr := p.API.GetUserByUsername(username)
	if err := p.nameFree(appErr, fmt.Sprintf("The username @%s is taken.", username), "@"+username); err != nil {
		return "", err
	}
	for _, check := range []error{
		checkTextLen("Display name", input.DisplayName, maxDisplayNameLen),
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
	declared, declErr := checkBotDeclaration(p.getConfiguration(), botDeclaration{
		Purpose: input.Purpose, ArchitectureURL: input.ArchitectureURL,
		IntegrationURL: input.IntegrationURL, HostingEnv: input.HostingEnv,
		HostingOtherDetail: input.HostingOtherDetail,
	})
	if declErr != nil {
		return "", declErr
	}
	compliance, complianceErr := checkBotCompliance(p.getConfiguration(), input.ComplianceAccepted, input.ComplianceTextHash)
	if complianceErr != nil {
		return "", complianceErr
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
	homeTeamID := strings.TrimSpace(input.HomeTeamID)
	homeChannelID := strings.TrimSpace(input.HomeChannelID)
	if err := p.requireBotHome(requester, homeTeamID, homeChannelID); err != nil {
		return "", err
	}

	// Purpose is the required declaration text and also becomes the Mattermost
	// bot profile description.
	req := &botRequest{
		ID:                         model.NewId(),
		RequesterID:                requesterID,
		Username:                   username,
		DisplayName:                strings.TrimSpace(input.DisplayName),
		Description:                declared.Purpose,
		OwnerUserID:                strings.TrimSpace(input.OwnerUserID),
		Compliance:                 compliance,
		Purpose:                    declared.Purpose,
		ArchitectureURL:            declared.ArchitectureURL,
		IntegrationURL:             declared.IntegrationURL,
		PrivilegeLevel:             declared.PrivilegeLevel,
		MMPrivilege:                declared.MMPrivilege,
		DataIngestion:              declared.DataIngestion,
		HostingEnv:                 declared.HostingEnv,
		HostingOtherDetail:         declared.HostingOtherDetail,
		HomeTeamID:                 homeTeamID,
		HomeChannelID:              homeChannelID,
		RequestToken:               input.RequestToken,
		IncomingWebhookChannelID:   incomingChID,
		IncomingWebhookDisplayName: strings.TrimSpace(input.IncomingWebhookDisplayName),
		OutgoingWebhookChannelID:   outgoingChID,
		OutgoingWebhookDisplayName: strings.TrimSpace(input.OutgoingWebhookDisplayName),
		OutgoingWebhookCallbackURL: strings.TrimSpace(input.OutgoingWebhookCallbackURL),
		CreatedAt:                  model.GetMillis(),
	}

	entry := newPendingEntry(req)
	sa.built(req, requester)
	if compliance != nil {
		sa.addDetail(complianceAuditDetail(compliance), complianceNote(compliance))
	}
	if err := p.reservePending(pendingKindBot, req.RequesterID, entry, p.getConfiguration()); err != nil {
		return "", err
	}

	if err := p.storeBotRequest(req); err != nil {
		p.releasePending(pendingKindBot, req.RequesterID, req.ID)
		p.API.LogError("failed to store bot request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	sa.markStored()
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		p.deletePendingRequestKV(kvBotRequestPrefix + req.ID)
		p.releasePending(pendingKindBot, req.RequesterID, req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot bot request")
	}

	approvalPostID, approvalChannelID, err := p.postBotApprovalRequest(req, requester)
	if err != nil {
		p.deletePendingRequestKV(kvBotRequestPrefix + req.ID)
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
	go func() {
		defer p.logRecoveredPanic("two-step approver channel reconcile")
		p.reconcileTwoStepApproverChannels()
	}()
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
	if req.OwnerUserID != "" {
		details = append(details, fmt.Sprintf("**Owner:** %s", p.mentionList([]string{req.OwnerUserID})))
	}
	if req.Purpose != "" {
		details = append(details, fmt.Sprintf("**Purpose:** %s", escapeMarkdown(req.Purpose)))
	}
	if req.ArchitectureURL != "" {
		details = append(details, fmt.Sprintf("**Architecture link:** %s", escapeMarkdown(req.ArchitectureURL)))
	}
	if req.IntegrationURL != "" {
		details = append(details, fmt.Sprintf("**Integration endpoint:** %s", escapeMarkdown(req.IntegrationURL)))
	}
	details = append(details, fmt.Sprintf("**Mattermost role:** %s", mmPrivilegeLabel(req.MMPrivilege)))
	if req.DataIngestion != "" {
		details = append(details, fmt.Sprintf("**Channel access:** %s", escapeMarkdown(req.DataIngestion)))
	}
	if req.HostingEnv != "" {
		hostLabel := hostingEnvLabel(req.HostingEnv)
		if req.HostingEnv == hostingOther && req.HostingOtherDetail != "" {
			hostLabel = fmt.Sprintf("%s (%s)", hostLabel, escapeMarkdown(req.HostingOtherDetail))
		}
		details = append(details, fmt.Sprintf("**Script / service runs from:** %s", hostLabel))
	}
	if req.IntegrationURL != "" {
		details = append(details, "**Transit security:** the integration endpoint uses https (checked on submit)")
	}
	details = append(details, "**Activity logs:** Request Hub audit history; forensic pull by bot user ID after approval")
	if req.HomeChannelID != "" {
		details = append(details, fmt.Sprintf("**Home:** %s", p.channelDisplayRef(req.HomeChannelID)))
	}
	if req.Compliance != nil {
		details = append(details, fmt.Sprintf("**Compliance:** accepted (sha256 %s)", shortHash(req.Compliance.TextSHA256)))
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
	if err := p.putPendingRequestKV(kvBotRequestPrefix+req.ID, data); err != nil {
		return errors.Wrap(err, "failed to store bot request")
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
// account request. The channel it is opened from becomes the bot's home.
func (p *Plugin) openBotRequestDialog(triggerID string, home *model.Channel) error {
	dialog := p.botRequestDialog(p.channelDisplayRef(home.Id))
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeBotDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}

// botRequestDialog is the bot request form. homeRef names the channel it
// was opened from, the bot's home. While the compliance statement is
// required it is shown in the introduction with a box to accept it, and its
// hash rides in the dialog state so a statement changed meanwhile is caught
// on submit.
func (p *Plugin) botRequestDialog(homeRef string) model.Dialog {
	c := p.getConfiguration()
	envs := make([]*model.PostActionOptions, 0, len(c.HostingEnvs()))
	for _, code := range c.HostingEnvs() {
		envs = append(envs, &model.PostActionOptions{Text: hostingEnvLabel(code), Value: code})
	}
	dialog := model.Dialog{
		CallbackId: botDialogCallbackID,
		Title:      "Request a Bot Account",
		IntroductionText: "Bot account requests need a security approval and a system approval. The bot is always a regular user (never Team Admin or System Admin).\n\n" +
			"**Home:** " + homeRef + ". The bot joins this channel when it is approved. " +
			"Open the form from another channel to change it. A home doesn't limit what the bot's token can reach.",
		SubmitLabel: "Submit request",
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
				DisplayName: "Purpose",
				Name:        fieldBotPurpose,
				Type:        "textarea",
				Placeholder: "What the bot does and why",
				HelpText:    "Required. This also becomes the bot's Mattermost profile description.",
				MaxLength:   maxBotPurposeLen,
			},
			{
				DisplayName: "Architecture link",
				Name:        fieldBotArchitectureURL,
				Type:        "text",
				Optional:    true,
				Placeholder: "https://…",
				HelpText:    "Optional https link to an architecture or design review.",
				MaxLength:   maxBotURLLen,
			},
			{
				DisplayName: "Integration endpoint",
				Name:        fieldBotIntegrationURL,
				Type:        "text",
				Placeholder: "https://ci.example/hooks/mm",
				HelpText:    "Required https URL of the integration that drives this bot.",
				MaxLength:   maxBotURLLen,
			},
			{
				DisplayName: "Where the script / service runs",
				Name:        fieldBotHostingEnv,
				Type:        "select",
				Options:     envs,
				HelpText:    "Where the integration that drives this bot runs (not which Mattermost environment).",
			},
			{
				DisplayName: "Specify hosting (if Other)",
				Name:        fieldBotHostingOtherDetail,
				Type:        "text",
				Optional:    true,
				Placeholder: "e.g. air-gapped appliance",
				MaxLength:   maxBotHostingOtherLen,
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
	if c.BotComplianceRequired() {
		text := c.BotComplianceText()
		dialog.IntroductionText += "\n\n**Compliance statement**\n\n> " + strings.ReplaceAll(text, "\n", "\n> ")
		dialog.Elements = append(dialog.Elements, model.DialogElement{
			DisplayName: "Compliance",
			Name:        fieldBotCompliance,
			Type:        "bool",
			Placeholder: "I accept the compliance statement above",
		})
		dialog.State = complianceHash(text)
	}
	return dialog
}
