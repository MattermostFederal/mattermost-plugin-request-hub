package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// Shared pieces for the bot and webhook transfer/retire flows: the REST side
// effects (bot ownership and state, webhook owner), the token rotation, the
// nominate ACL (policy 5), and the delivery of rotated secrets to a new owner.

const (
	// Bot transfer actions.
	transferActionTransfer = "transfer"
	transferActionRetire   = "retire"

	// Webhook transfer actions.
	transferActionReassign       = "reassign"
	transferActionRetireRecreate = "retire_recreate"
	transferActionRetireOnly     = "retire_only"

	// Transfer reasons. Free text rides in Note; offboarding /
	// owner_deactivated drive the token-rotate default.
	transferReasonVoluntary        = "voluntary"
	transferReasonOwnerDeactivated = "owner_deactivated"
	transferReasonOffboarding      = "offboarding"

	maxTransferNoteLen = 500
)

// transferAsset is the minimum an ACL check needs about a bot or webhook.
type transferAsset struct {
	assetType   string // "bot" | "webhook"
	assetID     string
	ownerUserID string
	requesterID string
	channelID   string // bot home channel, or the webhook's target channel
}

// rotateDefaultOn reports whether token rotation defaults on for a reason:
// offboarding and owner-deactivated handoffs rotate by default; a voluntary
// handoff leaves it to the requester.
func rotateDefaultOn(reason string) bool {
	switch strings.TrimSpace(strings.ToLower(reason)) {
	case transferReasonOffboarding, transferReasonOwnerDeactivated:
		return true
	default:
		return false
	}
}

// userActive reports whether a user exists and is not deactivated. A lookup
// failure (including not-found) counts as inactive, which is what drives the
// orphan path.
func (p *Plugin) userActive(userID string) bool {
	if strings.TrimSpace(userID) == "" {
		return false
	}
	user, appErr := p.API.GetUser(userID)
	return appErr == nil && user != nil && user.DeleteAt == 0
}

// canNominateTransfer enforces policy 5: while the asset's owner is active,
// only that owner (or a System Admin) may open a transfer/retire. Once the
// owner is deactivated the asset is orphaned, so anyone who was on the
// original request, or who can see the asset's channel (a stand-in for
// "sees it in inventory"), may step in. System Admins always may.
func (p *Plugin) canNominateTransfer(actingUserID string, asset transferAsset) error {
	actor, appErr := p.API.GetUser(actingUserID)
	if appErr != nil || actor == nil {
		return errors.New("could not verify your account; try again")
	}
	if actor.IsSystemAdmin() {
		return nil
	}
	noun := asset.assetType
	if noun == "" {
		noun = "asset"
	}
	if p.userActive(asset.ownerUserID) {
		if actingUserID == asset.ownerUserID {
			return nil
		}
		return errors.Errorf("only the current owner or a System Admin can transfer or retire this %s while its owner is active", noun)
	}
	// Owner is deactivated (orphaned): widen to the original requester and
	// anyone who can see the asset's channel.
	if actingUserID == asset.requesterID {
		return nil
	}
	if asset.channelID != "" {
		if _, memErr := p.API.GetChannelMember(asset.channelID, actingUserID); memErr == nil {
			return nil
		}
	}
	return errors.Errorf("you can't transfer or retire this %s: you must be its owner, on the original request, or able to see it where it lives", noun)
}

// transferClient builds an admin REST client for a transfer side effect and
// returns it with a cleanup that revokes the session. On any error the
// cleanup is a no-op.
func (p *Plugin) transferClient(adminUserID string) (*model.Client4, func(), error) {
	internalURL := p.internalBaseURL()
	if internalURL == "" {
		return nil, func() {}, errors.New("site URL is not configured — cannot reach the server")
	}
	session, sessErr := p.createUserSession(adminUserID)
	if sessErr != nil {
		return nil, func() {}, errors.Wrap(sessErr, "failed to create admin session")
	}
	client := model.NewAPIv4Client(internalURL)
	client.SetToken(session.Token)
	cleanup := func() {
		if revErr := p.API.RevokeSession(session.Id); revErr != nil {
			p.API.LogWarn("failed to revoke admin session after transfer op", "error", revErr.Error())
		}
	}
	return client, cleanup, nil
}

// assignBot reassigns a bot's Mattermost owner. model.BotPatch has no
// OwnerId, so this goes through REST AssignBot with the approver's session.
func (p *Plugin) assignBot(botUserID, newOwnerID, adminUserID string) error {
	if p.assignBotFn != nil {
		return p.assignBotFn(botUserID, newOwnerID, adminUserID)
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, _, apiErr := client.AssignBot(context.Background(), botUserID, newOwnerID); apiErr != nil {
		return errors.Wrap(apiErr, "failed to assign bot to new owner")
	}
	return nil
}

// disableBot deactivates a bot via REST (not on the plugin API). Retire
// prefers disable over permanent delete so the bot can be re-enabled.
func (p *Plugin) disableBot(botUserID, adminUserID string) error {
	if p.disableBotFn != nil {
		return p.disableBotFn(botUserID, adminUserID)
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, _, apiErr := client.DisableBot(context.Background(), botUserID); apiErr != nil {
		return errors.Wrap(apiErr, "failed to disable bot")
	}
	return nil
}

// updateWebhookOwner reassigns a standalone webhook's creator/owner via REST.
// Mattermost may ignore the creator change on some builds; callers still
// update the RH index and note the outcome in the audit trail.
func (p *Plugin) updateWebhookOwner(webhookType, hookID, newOwnerID, adminUserID string) error {
	if p.updateWebhookOwnerFn != nil {
		return p.updateWebhookOwnerFn(webhookType, hookID, newOwnerID, adminUserID)
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx := context.Background()
	switch webhookType {
	case webhookTypeIncoming:
		hook, _, getErr := client.GetIncomingWebhook(ctx, hookID, "")
		if getErr != nil {
			return errors.Wrap(getErr, "failed to load incoming webhook")
		}
		hook.UserId = newOwnerID
		if _, _, upErr := client.UpdateIncomingWebhook(ctx, hook); upErr != nil {
			return errors.Wrap(upErr, "failed to update incoming webhook owner")
		}
		// Some Mattermost builds keep the stored UserId on update. Re-read
		// and refuse a silent no-op so callers can fall back to recreate.
		after, _, afterErr := client.GetIncomingWebhook(ctx, hookID, "")
		if afterErr != nil {
			return errors.Wrap(afterErr, "failed to verify incoming webhook owner after update")
		}
		if after == nil || after.UserId != newOwnerID {
			return errors.New("Mattermost did not change the incoming webhook creator (server keeps the original owner); use Retire & recreate instead")
		}
	case webhookTypeOutgoing:
		hook, _, getErr := client.GetOutgoingWebhook(ctx, hookID)
		if getErr != nil {
			return errors.Wrap(getErr, "failed to load outgoing webhook")
		}
		hook.CreatorId = newOwnerID
		if _, _, upErr := client.UpdateOutgoingWebhook(ctx, hook); upErr != nil {
			return errors.Wrap(upErr, "failed to update outgoing webhook owner")
		}
		after, _, afterErr := client.GetOutgoingWebhook(ctx, hookID)
		if afterErr != nil {
			return errors.Wrap(afterErr, "failed to verify outgoing webhook owner after update")
		}
		if after == nil || after.CreatorId != newOwnerID {
			return errors.New("Mattermost did not change the outgoing webhook creator (server keeps the original owner); use Retire & recreate instead")
		}
	default:
		return errors.Errorf("unknown webhook type %q", webhookType)
	}
	return nil
}

// revokeBotTokens revokes every tracked access token for a bot and stops
// their reminders. Returns how many were revoked. The plugin API can't list
// tokens, so this scans the bot_token_expiry_* index for matching BotUserID
// (see token_reminders.go).
func (p *Plugin) revokeBotTokens(botUserID string) (int, error) {
	keys, err := p.listTokenExpiryKeys()
	if err != nil {
		return 0, errors.Wrap(err, "failed to list tracked bot tokens")
	}
	revoked := 0
	var failMsgs []string
	for _, key := range keys {
		rec, _, loadErr := p.loadBotTokenExpiry(key)
		if loadErr != nil {
			failMsgs = append(failMsgs, "load "+key+": "+loadErr.Error())
			continue
		}
		if rec == nil || rec.BotUserID != botUserID {
			continue
		}
		if appErr := p.API.RevokeUserAccessToken(rec.TokenID); appErr != nil {
			p.API.LogWarn("failed to revoke bot token on transfer/retire", "bot_user_id", botUserID, "token_id", rec.TokenID, "error", appErr.Error())
			failMsgs = append(failMsgs, "revoke "+rec.TokenID+": "+appErr.Error())
			continue
		}
		p.deleteBotTokenExpiry(key)
		revoked++
	}
	if len(failMsgs) > 0 {
		return revoked, errors.Errorf("revoked %d token(s); failures: %s", revoked, strings.Join(failMsgs, "; "))
	}
	return revoked, nil
}

// loadBotTokenExpiry reads one tracked-token record by its KV key.
func (p *Plugin) loadBotTokenExpiry(key string) (*botTokenExpiry, []byte, error) {
	raw, appErr := p.API.KVGet(key)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load bot token expiry")
	}
	if raw == nil {
		return nil, nil, nil
	}
	var rec botTokenExpiry
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, nil, errors.Wrap(err, "failed to read bot token expiry")
	}
	return &rec, raw, nil
}

// rotateBotToken revokes the bot's tracked tokens and issues a fresh one,
// tracked for the new owner so expiry reminders reach them. Returns the new
// token (value + expiry) for the DM; an empty value means none was created.
func (p *Plugin) rotateBotToken(botUserID, botUsername, newOwnerID, requestID string) (revoked int, tokenValue string, expiresAt int64, err error) {
	revoked, err = p.revokeBotTokens(botUserID)
	if err != nil {
		return revoked, "", 0, err
	}
	tok, tokErr := p.createBotToken(botUserID, "Rotated API token for @"+botUsername)
	if tokErr != nil {
		return revoked, "", 0, errors.Wrap(tokErr, "failed to create replacement token")
	}
	p.trackBotTokenExpiry(tok, botUsername, newOwnerID, requestID)
	return revoked, tok.Token, tok.ExpiresAt, nil
}

// sendTransferSecretsDM DMs the new owner the rotated secrets (token or
// webhook URL). It is the only place a transfer's secret is delivered, and
// only after final approval. Returns whether the DM was sent. Token/URL
// posts are scheduled for the usual 72-hour auto-delete.
func (p *Plugin) sendTransferSecretsDM(newOwnerID, requestID string, lines []string, hasSecret bool) bool {
	if newOwnerID == "" || len(lines) == 0 {
		return false
	}
	dm, appErr := p.API.GetDirectChannel(newOwnerID, p.botUserID)
	if appErr != nil {
		p.API.LogError("failed to open DM for transferred secrets", "user_id", newOwnerID, "error", appErr.Error())
		return false
	}
	post, appErr := p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: dm.Id,
		Message:   strings.Join(lines, "\n"),
	})
	if appErr != nil {
		p.API.LogError("failed to DM transferred secrets", "user_id", newOwnerID, "error", appErr.Error())
		return false
	}
	if hasSecret {
		p.scheduleTokenPostDeletion(post.Id, requestID)
	}
	return true
}
