package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// transferDialogState rides in a dialog's State so the submit knows which
// asset it is acting on and whether it came from an orphan card.
type transferDialogState struct {
	AssetID    string `json:"asset_id"`
	FromOrphan bool   `json:"from_orphan"`
}

func encodeTransferState(assetID string, fromOrphan bool) string {
	data, _ := json.Marshal(transferDialogState{AssetID: assetID, FromOrphan: fromOrphan})
	return string(data)
}

func decodeTransferState(state string) transferDialogState {
	var s transferDialogState
	_ = json.Unmarshal([]byte(state), &s)
	return s
}

// openBotTransferDialog opens the bot transfer/retire dialog with the bot
// locked. defaultAction preselects Transfer or Retire (orphan Retire button).
func (p *Plugin) openBotTransferDialog(triggerID, botUserID, botUsername, defaultAction string, fromOrphan bool) error {
	if defaultAction == "" {
		defaultAction = transferActionTransfer
	}
	dialog := model.Dialog{
		CallbackId:       botTransferDialogCallbackID,
		Title:            "Transfer or retire a bot",
		IntroductionText: "Bot **@" + botUsername + "**. Transfer or retire requests need a security approval and a system approval. Bot-owned webhooks move with the bot.",
		SubmitLabel:      "Submit request",
		State:            encodeTransferState(botUserID, fromOrphan),
		Elements: []model.DialogElement{
			{
				DisplayName: "Action", Name: fieldTransferAction, Type: "select", Default: defaultAction,
				Options: []*model.PostActionOptions{
					{Text: "Transfer to a new owner", Value: transferActionTransfer},
					{Text: "Retire (disable + revoke tokens)", Value: transferActionRetire},
				},
			},
			{
				DisplayName: "New owner", Name: fieldTransferNewOwner, Type: "select", DataSource: "users", Optional: true,
				HelpText: "Required for Transfer. The user who will own the bot.",
			},
			{
				DisplayName: "Rotate access token", Name: fieldTransferRotate, Type: "bool", Optional: true,
				Placeholder: "Revoke the old token and issue a fresh one to the new owner",
				HelpText:    "Defaults on for offboarding / deactivated-owner handoffs.",
			},
			{
				DisplayName: "Note", Name: fieldTransferNote, Type: "textarea", Optional: true,
				MaxLength: maxTransferNoteLen, HelpText: "Optional reason or context for the approvers.",
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID, URL: "/plugins/" + manifest.Id + routeBotTransferDialog, Dialog: dialog,
	}); appErr != nil {
		return errors.Wrap(appErr, "failed to open bot transfer dialog")
	}
	return nil
}

// openWebhookTransferDialog opens the webhook transfer/retire dialog.
func (p *Plugin) openWebhookTransferDialog(triggerID, hookID, whType, displayName, defaultAction string, fromOrphan bool) error {
	if defaultAction == "" {
		defaultAction = transferActionReassign
	}
	dialog := model.Dialog{
		CallbackId:       webhookTransferDialogCallbackID,
		Title:            "Transfer or retire a webhook",
		IntroductionText: "Webhook **" + displayName + "** (" + whType + "). These requests need a security approval and a system approval.",
		SubmitLabel:      "Submit request",
		State:            encodeTransferState(hookID, fromOrphan),
		Elements: []model.DialogElement{
			{
				DisplayName: "Action", Name: fieldTransferAction, Type: "select", Default: defaultAction,
				Options: []*model.PostActionOptions{
					{Text: "Reassign to a new owner", Value: transferActionReassign},
					{Text: "Retire & recreate under a new owner", Value: transferActionRetireRecreate},
					{Text: "Retire (delete)", Value: transferActionRetireOnly},
				},
			},
			{
				DisplayName: "New owner", Name: fieldTransferNewOwner, Type: "select", DataSource: "users", Optional: true,
				HelpText: "Required for Reassign and Retire & recreate.",
			},
			{
				DisplayName: "Note", Name: fieldTransferNote, Type: "textarea", Optional: true,
				MaxLength: maxTransferNoteLen, HelpText: "Optional reason or context for the approvers.",
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID, URL: "/plugins/" + manifest.Id + routeWebhookTransferDialog, Dialog: dialog,
	}); appErr != nil {
		return errors.Wrap(appErr, "failed to open webhook transfer dialog")
	}
	return nil
}

func (p *Plugin) handleBotTransferDialogSubmit(w http.ResponseWriter, r *http.Request) {
	if !p.getConfiguration().RequestEnabled(requestTypeBotTransfer) {
		writeJSON(w, model.SubmitDialogResponse{Error: botTransferRequestsDisabledMsg})
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}
	state := decodeTransferState(submission.State)
	_, rotateSet := submission.Submission[fieldTransferRotate]
	msg, err := p.submitBotTransferRequest(userID, botTransferInput{
		BotUserID:      state.AssetID,
		Action:         submissionString(submission.Submission, fieldTransferAction),
		NewOwnerUserID: submissionString(submission.Submission, fieldTransferNewOwner),
		Note:           submissionString(submission.Submission, fieldTransferNote),
		RotateToken:    submissionBool(submission.Submission, fieldTransferRotate),
		RotateSet:      rotateSet,
		FromOrphanCard: state.FromOrphan,
	})
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	writeJSON(w, model.SubmitDialogResponse{})
	_ = p.API.SendEphemeralPost(userID, &model.Post{ChannelId: submission.ChannelId, Message: msg})
}

func (p *Plugin) handleWebhookTransferDialogSubmit(w http.ResponseWriter, r *http.Request) {
	if !p.getConfiguration().RequestEnabled(requestTypeWebhookTransfer) {
		writeJSON(w, model.SubmitDialogResponse{Error: webhookTransferRequestsDisabledMsg})
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}
	state := decodeTransferState(submission.State)
	msg, err := p.submitWebhookTransferRequest(userID, webhookTransferInput{
		HookID:         state.AssetID,
		Action:         submissionString(submission.Submission, fieldTransferAction),
		NewOwnerUserID: submissionString(submission.Submission, fieldTransferNewOwner),
		Note:           submissionString(submission.Submission, fieldTransferNote),
		FromOrphanCard: state.FromOrphan,
	})
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	writeJSON(w, model.SubmitDialogResponse{})
	_ = p.API.SendEphemeralPost(userID, &model.Post{ChannelId: submission.ChannelId, Message: msg})
}

// handleSubmitBotTransferWebapp takes the hub tile's bot transfer/retire form.
func (p *Plugin) handleSubmitBotTransferWebapp(w http.ResponseWriter, r *http.Request) {
	if !p.getConfiguration().RequestEnabled(requestTypeBotTransfer) {
		writeJSON(w, map[string]string{"error": botTransferRequestsDisabledMsg})
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var payload struct {
		BotUserID        string `json:"bot_user_id"`
		Action           string `json:"action"`
		NewOwnerUserID   string `json:"new_owner_user_id"`
		NewOwnerUsername string `json:"new_owner_username"`
		Reason           string `json:"reason"`
		Note             string `json:"note"`
		RotateToken      *bool  `json:"rotate_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	in := botTransferInput{
		BotUserID: payload.BotUserID, Action: payload.Action,
		NewOwnerUserID: p.resolveOwnerID(payload.NewOwnerUserID, payload.NewOwnerUsername),
		Reason:         payload.Reason, Note: payload.Note,
	}
	if payload.RotateToken != nil {
		in.RotateToken, in.RotateSet = *payload.RotateToken, true
	}
	msg, err := p.submitBotTransferRequest(userID, in)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// handleSubmitWebhookTransferWebapp takes the hub tile's webhook transfer form.
func (p *Plugin) handleSubmitWebhookTransferWebapp(w http.ResponseWriter, r *http.Request) {
	if !p.getConfiguration().RequestEnabled(requestTypeWebhookTransfer) {
		writeJSON(w, map[string]string{"error": webhookTransferRequestsDisabledMsg})
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var payload struct {
		HookID           string `json:"hook_id"`
		Action           string `json:"action"`
		NewOwnerUserID   string `json:"new_owner_user_id"`
		NewOwnerUsername string `json:"new_owner_username"`
		Reason           string `json:"reason"`
		Note             string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	msg, err := p.submitWebhookTransferRequest(userID, webhookTransferInput{
		HookID: payload.HookID, Action: payload.Action,
		NewOwnerUserID: p.resolveOwnerID(payload.NewOwnerUserID, payload.NewOwnerUsername),
		Reason:         payload.Reason, Note: payload.Note,
	})
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// resolveOwnerID prefers an explicit user ID (the dialog "users" picker
// returns one); otherwise it resolves a webapp-supplied @username. An empty
// or unresolvable username yields "", which the submit validator rejects.
func (p *Plugin) resolveOwnerID(userID, username string) string {
	if strings.TrimSpace(userID) != "" {
		return strings.TrimSpace(userID)
	}
	username = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(username), "@"))
	if username == "" {
		return ""
	}
	user, appErr := p.API.GetUserByUsername(username)
	if appErr != nil || user == nil {
		return ""
	}
	return user.Id
}

// transferableAsset is one RH-managed asset the caller may nominate.
type transferableAsset struct {
	AssetType   string `json:"asset_type"`
	AssetID     string `json:"asset_id"`
	WebhookType string `json:"webhook_type,omitempty"`
	DisplayName string `json:"display_name"`
	OwnerID     string `json:"owner_id"`
	OwnerActive bool   `json:"owner_active"`
	Orphaned    bool   `json:"orphaned"`
}

// handleTransferableAssets lists RH-managed bots and webhooks the caller may
// transfer or retire (all of them for a System Admin). Used by the hub
// pickers; read access alone is never enough (canNominateTransfer decides).
func (p *Plugin) handleTransferableAssets(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	out := map[string][]transferableAsset{"bots": {}, "webhooks": {}}

	bots, err := p.listBotIndex()
	if err != nil {
		p.API.LogWarn("failed to list bot index for transfer picker", "error", err.Error())
	}
	for _, e := range bots {
		if e.RetiredAt != 0 {
			continue
		}
		owner := botIndexOwner(e)
		asset := transferAsset{assetType: "bot", assetID: e.BotUserID, ownerUserID: owner, requesterID: e.RequesterID, channelID: e.HomeChannelID}
		if p.canNominateTransfer(userID, asset) != nil {
			continue
		}
		active := p.userActive(owner)
		out["bots"] = append(out["bots"], transferableAsset{
			AssetType: "bot", AssetID: e.BotUserID, DisplayName: e.Username, OwnerID: owner,
			OwnerActive: active, Orphaned: !active,
		})
	}

	hooks, err := p.listWebhookIndex()
	if err != nil {
		p.API.LogWarn("failed to list webhook index for transfer picker", "error", err.Error())
	}
	for _, e := range hooks {
		if e.RetiredAt != 0 {
			continue
		}
		asset := transferAsset{assetType: "webhook", assetID: e.HookID, ownerUserID: e.OwnerUserID, requesterID: e.RequesterID, channelID: e.ChannelID}
		if p.canNominateTransfer(userID, asset) != nil {
			continue
		}
		active := p.userActive(e.OwnerUserID)
		out["webhooks"] = append(out["webhooks"], transferableAsset{
			AssetType: "webhook", AssetID: e.HookID, WebhookType: e.WebhookType, DisplayName: e.DisplayName, OwnerID: e.OwnerUserID,
			OwnerActive: active, Orphaned: !active,
		})
	}
	writeJSON(w, out)
}

// handleOrphanTransferClick opens the transfer/retire dialog from an orphan
// audit card. The clicker must pass canNominateTransfer — reading the audit
// channel is not enough.
func (p *Plugin) handleOrphanCardClick(w http.ResponseWriter, r *http.Request, retire bool) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}
	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}
	assetType := contextString(request.Context, "asset_type")
	assetID := contextString(request.Context, "asset_id")
	if assetID == "" {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not identify that asset. Refresh the channel and try again."})
		return
	}
	if msg := p.openOrphanDialog(userID, request.TriggerId, assetType, assetID, retire); msg != "" {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
		return
	}
	writeJSON(w, model.PostActionIntegrationResponse{})
}

// openOrphanDialog checks the ACL and opens the right dialog. Returns a
// user-facing message when it can't (the caller shows it ephemerally).
func (p *Plugin) openOrphanDialog(userID, triggerID, assetType, assetID string, retire bool) string {
	switch assetType {
	case "bot":
		index, err := p.loadBotIndex(assetID)
		if err != nil || index == nil {
			return "That bot is no longer tracked by Request Hub."
		}
		asset := transferAsset{assetType: "bot", assetID: assetID, ownerUserID: index.OwnerUserID, requesterID: index.RequesterID, channelID: index.HomeChannelID}
		if aclErr := p.canNominateTransfer(userID, asset); aclErr != nil {
			return aclErr.Error()
		}
		action := transferActionTransfer
		if retire {
			action = transferActionRetire
		}
		if err := p.openBotTransferDialog(triggerID, assetID, index.Username, action, true); err != nil {
			p.API.LogWarn("failed to open orphan bot dialog", "error", err.Error())
			return "Could not open the form. Try again."
		}
	case "webhook":
		index, err := p.loadWebhookIndex(assetID)
		if err != nil || index == nil {
			return "That webhook is no longer tracked by Request Hub."
		}
		asset := transferAsset{assetType: "webhook", assetID: assetID, ownerUserID: index.OwnerUserID, requesterID: index.RequesterID, channelID: index.ChannelID}
		if aclErr := p.canNominateTransfer(userID, asset); aclErr != nil {
			return aclErr.Error()
		}
		action := transferActionReassign
		if retire {
			action = transferActionRetireOnly
		}
		if err := p.openWebhookTransferDialog(triggerID, assetID, index.WebhookType, index.DisplayName, action, true); err != nil {
			p.API.LogWarn("failed to open orphan webhook dialog", "error", err.Error())
			return "Could not open the form. Try again."
		}
	default:
		return "Unknown asset type."
	}
	return ""
}

func (p *Plugin) handleApproveBotTransfer(w http.ResponseWriter, r *http.Request) {
	p.handleBotTransferAction(w, r, true)
}
func (p *Plugin) handleDenyBotTransfer(w http.ResponseWriter, r *http.Request) {
	p.handleBotTransferAction(w, r, false)
}
func (p *Plugin) handleApproveWebhookTransfer(w http.ResponseWriter, r *http.Request) {
	p.handleWebhookTransferAction(w, r, true)
}
func (p *Plugin) handleDenyWebhookTransfer(w http.ResponseWriter, r *http.Request) {
	p.handleWebhookTransferAction(w, r, false)
}
