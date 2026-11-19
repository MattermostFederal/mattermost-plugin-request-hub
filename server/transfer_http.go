package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

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

// resolveOwnerID prefers an explicit user ID; otherwise it resolves a
// webapp-supplied @username. An empty or unresolvable username yields "",
// which the submit validator rejects.
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
	// AllowedActions, when set, limits the transfer modal's action list
	// (orphaned incoming hooks must not offer Reassign — same secret URL).
	AllowedActions []string `json:"allowed_actions,omitempty"`
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
		p.API.LogError("failed to list bot index for transfer picker", "error", err.Error())
		http.Error(w, "failed to list transferable assets", http.StatusInternalServerError)
		return
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
		p.API.LogError("failed to list webhook index for transfer picker", "error", err.Error())
		http.Error(w, "failed to list transferable assets", http.StatusInternalServerError)
		return
	}
	for _, e := range hooks {
		if e.RetiredAt != 0 {
			continue
		}
		owner := webhookIndexOwner(e)
		asset := transferAsset{assetType: "webhook", assetID: e.HookID, ownerUserID: owner, requesterID: e.RequesterID, channelID: e.ChannelID}
		if p.canNominateTransfer(userID, asset) != nil {
			continue
		}
		active := p.userActive(owner)
		entry := e
		assetID := p.ensureIncomingTransferHandle(&entry)
		if e.WebhookType == webhookTypeIncoming && assetID == "" {
			continue
		}
		item := transferableAsset{
			AssetType: "webhook", AssetID: assetID, WebhookType: e.WebhookType, DisplayName: e.DisplayName, OwnerID: owner,
			OwnerActive: active, Orphaned: !active,
		}
		// Incoming hook IDs are the secret URL. Orphaned incoming hooks must
		// not be reassigned (same URL stays live); only retire / recreate.
		if e.WebhookType == webhookTypeIncoming && !active {
			item.AllowedActions = []string{transferActionRetireRecreate, transferActionRetireOnly}
		}
		out["webhooks"] = append(out["webhooks"], item)
	}
	writeJSON(w, out)
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
