package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// handleSubmitBotTransferWebapp takes the hub tile's bot transfer/retire form.
func (p *Plugin) handleSubmitBotTransferWebapp(w http.ResponseWriter, r *http.Request) {
	// Identity first: an unauthenticated caller must not learn which
	// request types this instance has turned on.
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeBotTransfer) {
		writeJSON(w, map[string]string{"error": botTransferRequestsDisabledMsg})
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
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeWebhookTransfer) {
		writeJSON(w, map[string]string{"error": webhookTransferRequestsDisabledMsg})
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

// transferPickerBudget caps the live Mattermost reads handleTransferableAssets
// makes across the whole inventory. Past it the picker answers from the
// index alone rather than holding the caller's request open.
const transferPickerBudget = 15 * time.Second

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
	caller, appErr := p.API.GetUser(userID)
	if appErr != nil || caller == nil {
		http.Error(w, "failed to list transferable assets", http.StatusInternalServerError)
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
		// Same owner submit and approval use, so who may nominate and the
		// orphaned (rotate default) flag agree with what the server decides.
		owner, ownerErr := p.liveBotOwnerID(e.BotUserID, botIndexOwner(e))
		if ownerErr != nil {
			if !errors.Is(ownerErr, errBotGone) {
				p.API.LogWarn("failed to load live bot owner for transfer picker", "bot_user_id", e.BotUserID, "error", ownerErr.Error())
			}
			continue
		}
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
	// One admin session and one deadline for every live hook read below:
	// the list is as long as the inventory, and the caller's context ends
	// the batch if they give up on the request.
	reads := p.newTransferReads(r.Context(), userID, transferPickerBudget)
	defer reads.close()
	noReassign := p.webhookReassignUnsupported()
	for _, e := range hooks {
		if e.RetiredAt != 0 {
			continue
		}
		entry := e
		// Same owner resolution submit and approval use, so the picker
		// never offers a hook the submit check will turn down — or hide one
		// whose index row has fallen behind a reassign.
		owner, liveKnown := p.webhookOwnerForChecksWith(&entry, reads)
		if !liveKnown && !caller.IsSystemAdmin() && userID != owner && userID != e.RequesterID {
			// The index row is the last thing Request Hub was told, not
			// who owns the hook now. A stale "owner deactivated" widens
			// canNominateTransfer to everyone who can see the channel, so
			// an unverified row must not be what hands a hook — and an
			// incoming hook's handle — to a channel member. The indexed
			// owner and the original requester are named on the row
			// itself and submit accepts a nomination from either on the
			// same unverified row, so hiding the hook from them only
			// leaves the picker disagreeing with the form behind it.
			continue
		}
		asset := transferAsset{assetType: "webhook", assetID: e.HookID, ownerUserID: owner, requesterID: e.RequesterID, channelID: e.ChannelID}
		if p.canNominateTransfer(userID, asset) != nil {
			continue
		}
		active := p.userActive(owner)
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
		// Servers that preserve the creator can't reassign anything.
		if noReassign || (e.WebhookType == webhookTypeIncoming && !active) {
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
