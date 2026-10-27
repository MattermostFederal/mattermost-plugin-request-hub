package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	kvWebhookRequestPrefix = "webhook_request_"

	webhookTypeIncoming = "incoming"
	webhookTypeOutgoing = "outgoing"

	fieldCallbackURL = "callback_url"
)

// webhookRequest is a pending request to create an incoming or outgoing webhook.
// Webhooks cross the network boundary, so all requests — including those from
// System Admins — go through two-step approval. Creation uses the final
// approver's session (no stored credentials).
type webhookRequest struct {
	ID          string `json:"id"`
	RequesterID string `json:"requester_id"`
	WebhookType string `json:"webhook_type"` // "incoming" or "outgoing"
	ChannelID   string `json:"channel_id"`
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	CallbackURL string `json:"callback_url,omitempty"` // outgoing only

	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`

	twoStepState
}

// requireWebhookTargetChannel loads channelID and ensures it is a channel the
// requester may target with a webhookType hook: a public or private team
// channel for incoming, a public one for outgoing. System Admins don't need
// to be members. Used by standalone webhook requests and bot webhook extras.
func (p *Plugin) requireWebhookTargetChannel(requester *model.User, channelID, webhookType string) (*model.Channel, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return nil, errors.New("a target channel is required")
	}
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil {
		p.API.LogWarn("failed to load webhook target channel", "channel_id", channelID, "error", appErr.Error())
		return nil, errors.New("could not find the target channel")
	}
	if channel.Type != model.ChannelTypeOpen && channel.Type != model.ChannelTypePrivate {
		return nil, errors.New("webhooks can only target public or private channels")
	}
	if channel.DeleteAt != 0 {
		return nil, errors.New("webhooks can't target an archived channel")
	}
	if webhookType == webhookTypeOutgoing && channel.Type != model.ChannelTypeOpen {
		return nil, errors.New("outgoing webhooks can only use public channels")
	}
	if requester != nil && !requester.IsSystemAdmin() {
		if _, memErr := p.API.GetChannelMember(channelID, requester.Id); memErr != nil {
			return nil, errors.New("you must be a member of the channel to request a webhook for it")
		}
	}
	return channel, nil
}

// submitWebhookRequest validates input and queues a webhook request in the
// dedicated webhook approval channel. The webhook is created at approve time
// with the approving admin's session token.
func (p *Plugin) submitWebhookRequest(requesterID, webhookType, channelID, displayName, description, callbackURL string) (string, error) {
	if !p.getConfiguration().RequestEnabled(requestTypeWebhook) {
		return "", errors.New(webhookRequestsDisabledMsg)
	}
	if webhookType != webhookTypeIncoming && webhookType != webhookTypeOutgoing {
		return "", errors.Errorf("unknown webhook type %q", webhookType)
	}
	if strings.TrimSpace(channelID) == "" {
		return "", errors.New("a target channel is required")
	}
	if strings.TrimSpace(displayName) == "" {
		return "", errors.New("a display name is required")
	}
	if err := checkByteLen("display name", displayName, maxWebhookNameBytes); err != nil {
		return "", err
	}
	if err := checkByteLen("description", description, maxWebhookDescriptionBytes); err != nil {
		return "", err
	}
	if webhookType == webhookTypeOutgoing {
		if strings.TrimSpace(callbackURL) == "" {
			return "", errors.New("a callback URL is required for outgoing webhooks")
		}
		if err := checkCallbackURL(callbackURL); err != nil {
			return "", err
		}
	}

	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil {
		p.API.LogWarn("failed to load requesting user", "user_id", requesterID, "error", appErr.Error())
		return "", errors.New("could not load your account; try again")
	}

	channel, err := p.requireWebhookTargetChannel(requester, channelID, webhookType)
	if err != nil {
		return "", err
	}

	req := &webhookRequest{
		ID:          model.NewId(),
		RequesterID: requesterID,
		WebhookType: webhookType,
		ChannelID:   channelID,
		TeamID:      channel.TeamId,
		DisplayName: strings.TrimSpace(displayName),
		Description: strings.TrimSpace(description),
		CallbackURL: strings.TrimSpace(callbackURL),
	}

	if storeErr := p.storeWebhookRequest(req); storeErr != nil {
		return "", storeErr
	}
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		_ = p.API.KVDelete(kvWebhookRequestPrefix + req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot webhook request")
	}

	approvalPostID, approvalChannelID, err := p.postWebhookApprovalRequest(req, requester, channel)
	if err != nil {
		_ = p.API.KVDelete(kvWebhookRequestPrefix + req.ID)
		return "", err
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	dmRootPostID, dmChannelID := p.sendWebhookTicketCreatedDM(req, requester, channel)
	req.DMRootPostID = dmRootPostID
	req.DMChannelID = dmChannelID

	p.persistTicketPosts(ticketPersist{
		KVKey: kvWebhookRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "webhook request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
			Kind: requestTypeWebhook,
		},
	})

	msg := "Your webhook request has been submitted for approval. It needs a security approval and a system approval."
	if requester.IsSystemAdmin() {
		msg = "Your webhook request has been posted for two-step approval. A security approver must sign off first; then you (or another System Admin) can fill the system step."
	}
	return msg, nil
}

func (p *Plugin) sendWebhookTicketCreatedDM(req *webhookRequest, requester *model.User, channel *model.Channel) (dmRootPostID, dmChannelID string) {
	return p.postTicketCreatedDM(
		req.RequesterID,
		p.webhookApprovalAttachment(req, requester, channel),
		ticketSubmittedIntro("webhook request"),
	)
}

// createWebhookForRequest creates the webhook via the Mattermost REST API using
// a short-lived session for the approving admin. Returns a user-facing DM
// message and the created hook ID (empty for outgoing) so callers can delete
// an undelivered incoming-hook secret.
func (p *Plugin) createWebhookForRequest(req *webhookRequest, adminUserID string) (dmMsg, hookID string, err error) {
	if strings.TrimSpace(adminUserID) == "" {
		return "", "", errors.New("admin session token is missing — cannot create webhook")
	}

	internalURL := p.internalBaseURL()
	publicURL := p.siteURL()
	if internalURL == "" || publicURL == "" {
		return "", "", errors.New("site URL is not configured — cannot create webhook")
	}

	if p.createWebhookFn != nil {
		return p.createWebhookFn(req, adminUserID)
	}

	session, sessErr := p.createUserSession(adminUserID)
	if sessErr != nil {
		return "", "", errors.Wrap(sessErr, "failed to create admin session for webhook creation")
	}
	defer func() {
		if revErr := p.API.RevokeSession(session.Id); revErr != nil {
			p.API.LogWarn("failed to revoke admin session after webhook op", "error", revErr.Error())
		}
	}()

	// Use the internal localhost URL to avoid the loopback routing issue in
	// cloud deployments where the public SiteURL routes through a load balancer
	// that may block traffic originating from inside the server process.
	client := model.NewAPIv4Client(internalURL)
	client.SetToken(session.Token)
	ctx := context.Background()

	switch req.WebhookType {
	case webhookTypeIncoming:
		// Owned by the requester and locked to the requested channel, so
		// the hook posts as them and only where they asked.
		hook, _, apiErr := client.CreateIncomingWebhook(ctx, &model.IncomingWebhook{
			ChannelId:     req.ChannelID,
			TeamId:        req.TeamID,
			UserId:        req.RequesterID,
			ChannelLocked: true,
			DisplayName:   req.DisplayName,
			Description:   req.Description,
		})
		if apiErr != nil {
			return "", "", errors.Wrap(apiErr, "failed to create incoming webhook")
		}
		return fmt.Sprintf("Your incoming webhook **%s** was approved. Your webhook URL:\n`%s/hooks/%s`",
			escapeMarkdown(hook.DisplayName), publicURL, hook.Id), hook.Id, nil

	case webhookTypeOutgoing:
		hook, _, apiErr := client.CreateOutgoingWebhook(ctx, &model.OutgoingWebhook{
			TeamId:       req.TeamID,
			ChannelId:    req.ChannelID,
			CreatorId:    req.RequesterID,
			DisplayName:  req.DisplayName,
			Description:  req.Description,
			CallbackURLs: []string{req.CallbackURL},
			ContentType:  "application/json",
		})
		if apiErr != nil {
			return "", "", errors.Wrap(apiErr, "failed to create outgoing webhook")
		}
		return fmt.Sprintf("Your outgoing webhook **%s** was approved and is now active.", escapeMarkdown(hook.DisplayName)), hook.Id, nil

	default:
		return "", "", errors.Errorf("unknown webhook type: %s", req.WebhookType)
	}
}

// deleteWebhook removes a hook created by createWebhookForRequest. Used when
// the requester DM fails after creation so we don't leave an unowned secret.
// adminUserID is the approver whose session is used for the REST delete.
// hookIDFromPublicURL extracts the hook id from a SiteURL/hooks/<id> string.
func hookIDFromPublicURL(publicURL string) string {
	const marker = "/hooks/"
	i := strings.LastIndex(publicURL, marker)
	if i < 0 {
		return ""
	}
	id := strings.TrimSpace(publicURL[i+len(marker):])
	if id == "" || strings.ContainsAny(id, "/?#") {
		return ""
	}
	return id
}

func (p *Plugin) deleteWebhook(webhookType, hookID, adminUserID string) error {
	if strings.TrimSpace(hookID) == "" {
		return nil
	}
	if p.deleteWebhookFn != nil {
		return p.deleteWebhookFn(webhookType, hookID)
	}

	internalURL := p.internalBaseURL()
	if internalURL == "" {
		return errors.New("site URL is not configured — cannot delete webhook")
	}
	session, sessErr := p.createUserSession(adminUserID)
	if sessErr != nil {
		return errors.Wrap(sessErr, "failed to create admin session for webhook deletion")
	}
	defer func() {
		if revErr := p.API.RevokeSession(session.Id); revErr != nil {
			p.API.LogWarn("failed to revoke admin session after webhook op", "error", revErr.Error())
		}
	}()

	client := model.NewAPIv4Client(internalURL)
	client.SetToken(session.Token)
	ctx := context.Background()

	switch webhookType {
	case webhookTypeIncoming:
		if _, err := client.DeleteIncomingWebhook(ctx, hookID); err != nil {
			return errors.Wrap(err, "failed to delete incoming webhook")
		}
	case webhookTypeOutgoing:
		if _, err := client.DeleteOutgoingWebhook(ctx, hookID); err != nil {
			return errors.Wrap(err, "failed to delete outgoing webhook")
		}
	}
	return nil
}

// siteURL returns the Mattermost instance's public site URL, used for
// user-facing links (e.g. webhook URLs sent in DMs).
func (p *Plugin) siteURL() string {
	cfg := p.API.GetConfig()
	if cfg == nil || cfg.ServiceSettings.SiteURL == nil {
		return ""
	}
	return *cfg.ServiceSettings.SiteURL
}

// internalBaseURL returns an HTTP base URL that reaches the Mattermost server
// on its listen address, bypassing the public SiteURL. Cloud deployments route
// the public hostname through a load balancer; a plugin calling its own public
// IP from inside the server process often times out due to egress filtering.
// Falls back to the public SiteURL when the listen address cannot be parsed,
// or when TLS is enabled on a loopback host (local cert verification fails).
func (p *Plugin) internalBaseURL() string {
	cfg := p.API.GetConfig()
	if cfg == nil || cfg.ServiceSettings.ListenAddress == nil {
		return p.siteURL()
	}
	host, port, err := net.SplitHostPort(*cfg.ServiceSettings.ListenAddress)
	if err != nil || port == "" {
		return p.siteURL()
	}
	scheme := "http"
	if cfg.ServiceSettings.ConnectionSecurity != nil &&
		*cfg.ServiceSettings.ConnectionSecurity == model.ConnSecurityTLS {
		scheme = "https"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "localhost"
	}
	// TLS certs usually match SiteURL's hostname, not a raw listen IP / loopback.
	// Prefer SiteURL so webhook/guest/bot-extra API clients pass cert checks.
	if scheme == "https" {
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || net.ParseIP(host) != nil {
			return p.siteURL()
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func (p *Plugin) postWebhookApprovalRequest(req *webhookRequest, requester *model.User, channel *model.Channel) (postID, channelID string, err error) {
	webhookCh := p.ensureWebhookApprovalChannel()
	go p.reconcileTwoStepApproverChannels()
	return p.postApprovalAttachment(
		req.ID,
		p.webhookApprovalAttachment(req, requester, channel),
		p.webhookApprovalHeader(),
		webhookCh,
	)
}

func (p *Plugin) webhookApprovalHeader() string {
	return "@channel — a webhook request needs review (requires a security approval and a system approval)."
}

func (p *Plugin) webhookApprovalAttachment(req *webhookRequest, requester *model.User, channel *model.Channel) *model.MessageAttachment {
	typeLabel := "Incoming webhook"
	if req.WebhookType == webhookTypeOutgoing {
		typeLabel = "Outgoing webhook"
	}

	fields := []*model.MessageAttachmentField{
		{Title: "Requested by", Value: fmt.Sprintf("@%s", requester.Username), Short: true},
		{Title: "Type", Value: typeLabel, Short: true},
		{Title: "Target channel", Value: fmt.Sprintf("~%s", channel.Name), Short: true},
		{Title: "Display name", Value: escapeMarkdown(req.DisplayName), Short: true},
		{Title: "Approvals", Value: p.twoStepStatusValue(req.twoStepState), Short: false},
	}

	var details []string
	if req.Description != "" {
		details = append(details, fmt.Sprintf("**Description:** %s", escapeMarkdown(req.Description)))
	}
	if req.CallbackURL != "" {
		details = append(details, fmt.Sprintf("**Callback URL:** %s", escapeMarkdown(req.CallbackURL)))
	}

	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   "Webhook request",
		Color:   "#FF6B35",
		Fields:  fields,
		Text:    strings.Join(details, "\n"),
		Actions: p.webhookApprovalActions(req.ID, siteURL),
	}
}

func (p *Plugin) webhookApprovalAttachmentWithNotice(req *webhookRequest, requester *model.User, channel *model.Channel, notice string) *model.MessageAttachment {
	return p.webhookApprovalAttachmentWithTitledNotice(req, requester, channel, "⚠️ Action needed", notice)
}

func (p *Plugin) webhookApprovalAttachmentWithTitledNotice(req *webhookRequest, requester *model.User, channel *model.Channel, title, notice string) *model.MessageAttachment {
	att := p.webhookApprovalAttachment(req, requester, channel)
	att.Color = "#D24B4E"
	if title == "" {
		title = "⚠️ Action needed"
	}
	att.Fields = append([]*model.MessageAttachmentField{
		{Title: title, Value: notice, Short: false},
	}, att.Fields...)
	return att
}

func (p *Plugin) webhookApprovalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id: "approve", Name: approveButtonLabel, Type: model.PostActionTypeButton, Style: "primary",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeApproveWebhook,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
		{
			Id: "deny", Name: "Deny", Type: model.PostActionTypeButton, Style: "danger",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeDenyWebhook,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
	}
}

func (p *Plugin) storeWebhookRequest(req *webhookRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal webhook request")
	}
	if appErr := p.API.KVSet(kvWebhookRequestPrefix+req.ID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to store webhook request")
	}
	return nil
}

func (p *Plugin) loadWebhookRequest(id string) (*webhookRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvWebhookRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load webhook request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req webhookRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal webhook request")
	}
	return &req, data, nil
}

// openIncomingWebhookDialog opens the request dialog for an incoming webhook.
func (p *Plugin) openIncomingWebhookDialog(triggerID string) error {
	dialog := model.Dialog{
		CallbackId:       "incoming_webhook_request",
		Title:            "Request an Incoming Webhook",
		IntroductionText: "Incoming webhooks let external services post messages into a Mattermost channel. Requires a security approval and a system approval.",
		SubmitLabel:      "Submit request",
		Elements: []model.DialogElement{
			{
				DisplayName: "Target channel",
				Name:        fieldChannelID,
				Type:        "select",
				DataSource:  "channels",
				HelpText:    "The channel this webhook will post messages into.",
			},
			{
				DisplayName: "Display name",
				Name:        fieldDisplayName,
				Type:        "text",
				Placeholder: "e.g. Monitoring Alerts",
				MaxLength:   maxWebhookNameBytes,
			},
			{
				DisplayName: "Description / purpose",
				Name:        fieldPurpose,
				Type:        "textarea",
				Optional:    true,
				Placeholder: "What service is this for? What events does it send?",
				MaxLength:   maxWebhookDescriptionBytes,
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeIncomingWebhookDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}

// openOutgoingWebhookDialog opens the request dialog for an outgoing webhook.
func (p *Plugin) openOutgoingWebhookDialog(triggerID string) error {
	dialog := model.Dialog{
		CallbackId:       "outgoing_webhook_request",
		Title:            "Request an Outgoing Webhook",
		IntroductionText: "Outgoing webhooks send channel messages to an external service. Requires a security approval and a system approval.",
		SubmitLabel:      "Submit request",
		Elements: []model.DialogElement{
			{
				DisplayName: "Target channel",
				Name:        fieldChannelID,
				Type:        "select",
				DataSource:  "channels",
				HelpText:    "The channel whose messages trigger this webhook.",
			},
			{
				DisplayName: "Display name",
				Name:        fieldDisplayName,
				Type:        "text",
				Placeholder: "e.g. Incident Relay",
				MaxLength:   maxWebhookNameBytes,
			},
			{
				DisplayName: "Description / purpose",
				Name:        fieldPurpose,
				Type:        "textarea",
				Optional:    true,
				Placeholder: "What does this webhook do? What service does it relay to?",
				MaxLength:   maxWebhookDescriptionBytes,
			},
			{
				DisplayName: "Callback URL",
				Name:        fieldCallbackURL,
				Type:        "text",
				Placeholder: "https://example.com/mattermost-hook",
				HelpText:    "The external URL Mattermost will POST to when messages are sent in the target channel.",
				MaxLength:   maxCallbackURLLen,
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeOutgoingWebhookDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}
