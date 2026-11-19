package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	transferRESTTimeout       = 30 * time.Second
	botAccessTokenListPerPage = 200
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

// errBotGone is returned by liveBotOwnerID when the bot no longer exists.
var errBotGone = errors.New("the bot no longer exists")

// liveBotOwnerID prefers Mattermost's bot OwnerId when set, falling back to
// the Request Hub index owner. Lookup failures fail closed; a deleted bot
// returns errBotGone.
func (p *Plugin) liveBotOwnerID(botUserID, indexOwner string) (string, error) {
	bot, appErr := p.API.GetBot(botUserID, true)
	if appErr != nil {
		if isNotFound(appErr) {
			return "", errBotGone
		}
		return "", errors.Wrap(appErr, "failed to load bot")
	}
	if bot != nil {
		if owner := strings.TrimSpace(bot.OwnerId); owner != "" {
			return owner, nil
		}
	}
	return indexOwner, nil
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

// userActiveStatus reports whether a user is active. known is false when
// GetUser failed for a reason other than not-found (transient errors must
// not widen orphan filing rights).
func (p *Plugin) userActiveStatus(userID string) (active bool, known bool) {
	if strings.TrimSpace(userID) == "" {
		return false, true
	}
	user, appErr := p.API.GetUser(userID)
	if appErr != nil {
		if isNotFound(appErr) {
			return false, true
		}
		return false, false
	}
	return user != nil && user.DeleteAt == 0, true
}

// userActive reports whether a user exists and is not deactivated. Lookup
// failures other than not-found count as inactive for display paths that
// cannot surface an error; ACL paths must use userActiveStatus instead.
func (p *Plugin) userActive(userID string) bool {
	active, _ := p.userActiveStatus(userID)
	return active
}

// normalizeTransferReason accepts only the known reason enum (or empty →
// default from owner activity). Free text belongs in Note.
func normalizeTransferReason(reason string, ownerActive bool) (string, error) {
	reason = strings.TrimSpace(strings.ToLower(reason))
	if reason == "" {
		if !ownerActive {
			return transferReasonOwnerDeactivated, nil
		}
		return transferReasonVoluntary, nil
	}
	switch reason {
	case transferReasonVoluntary, transferReasonOwnerDeactivated, transferReasonOffboarding:
		return reason, nil
	default:
		return "", errors.New("pick a valid reason (voluntary, owner_deactivated, or offboarding)")
	}
}

// transferNewOwnerOpts carries extra checks for the successor on a transfer.
type transferNewOwnerOpts struct {
	currentOwnerID string
	actingUserID   string // when set (approve time), blocks approving a transfer to yourself
	channelID      string // webhook target: successor must be a member (unless sysadmin)
	webhookType    string
	noun           string // "bot" / "webhook" for error text
}

// checkTransferNewOwner requires an active, non-bot, non-guest user who is
// not the current owner. Optional channel membership and "not yourself"
// checks apply when those fields are set.
func (p *Plugin) checkTransferNewOwner(ownerID string, opts transferNewOwnerOpts) error {
	noun := opts.noun
	if noun == "" {
		noun = "asset"
	}
	if ownerID == "" {
		return errors.Errorf("pick the new owner for the %s", noun)
	}
	if !model.IsValidId(ownerID) {
		return errors.New("the new owner must be picked from the list")
	}
	if opts.currentOwnerID != "" && ownerID == opts.currentOwnerID {
		return errors.Errorf("pick someone other than the current %s owner", noun)
	}
	if opts.actingUserID != "" && ownerID == opts.actingUserID {
		return errors.New("you can't approve a transfer that names you as the new owner — ask another approver")
	}
	owner, appErr := p.API.GetUser(ownerID)
	if appErr != nil || owner == nil || owner.DeleteAt != 0 || owner.IsBot {
		return errors.New("the new owner must be an active user")
	}
	if owner.IsGuest() {
		return errors.New("the new owner can't be a guest account")
	}
	if opts.channelID != "" && !owner.IsSystemAdmin() {
		if _, memErr := p.API.GetChannelMember(opts.channelID, ownerID); memErr != nil {
			return errors.New("the new owner must be a member of the webhook's channel")
		}
		if opts.webhookType == webhookTypeOutgoing {
			ch, chErr := p.API.GetChannel(opts.channelID)
			if chErr != nil || ch == nil {
				return errors.New("could not verify the webhook's channel for the new owner")
			}
			if ch.Type != model.ChannelTypeOpen {
				return errors.New("outgoing webhooks can only use public channels")
			}
		}
	}
	return nil
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
	ownerActive, ownerKnown := p.userActiveStatus(asset.ownerUserID)
	if !ownerKnown {
		return errors.New("could not verify the current owner's account; try again")
	}
	if ownerActive {
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
	client.HTTPClient = &http.Client{Timeout: transferRESTTimeout}
	cleanup := func() {
		if revErr := p.API.RevokeSession(session.Id); revErr != nil {
			p.API.LogWarn("failed to revoke admin session after transfer op", "error", revErr.Error())
		}
	}
	return client, cleanup, nil
}

// listBotAccessTokens returns every access token on the bot, paging the
// Mattermost API until a short page. Failures mid-pagination abort so
// revoke-all never pretends to have finished.
func (p *Plugin) listBotAccessTokens(botUserID, adminUserID string) ([]*model.UserAccessToken, error) {
	if p.listBotAccessTokensFn != nil {
		return p.listBotAccessTokensFn(botUserID, adminUserID)
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx := context.Background()
	var out []*model.UserAccessToken
	for page := 0; ; page++ {
		tokens, _, listErr := client.GetUserAccessTokensForUser(ctx, botUserID, page, botAccessTokenListPerPage)
		if listErr != nil {
			return nil, errors.Wrap(listErr, "failed to list bot access tokens")
		}
		out = append(out, tokens...)
		if len(tokens) < botAccessTokenListPerPage {
			return out, nil
		}
	}
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

// revokeBotTokens revokes every access token on the bot (including ones the
// owner created by hand) via an admin session, then cleans tracked reminder
// rows. Already-revoked / not-found tokens count as success. Expiry rows are
// dropped only after a confirmed revoke so reminders keep retrying on
// transient failures.
func (p *Plugin) revokeBotTokens(botUserID, adminUserID string) (int, error) {
	revoked := 0
	var failMsgs []string
	seen := map[string]struct{}{}

	if adminUserID != "" {
		tokens, listErr := p.listBotAccessTokens(botUserID, adminUserID)
		if listErr != nil {
			failMsgs = append(failMsgs, "list tokens: "+listErr.Error())
		} else {
			for _, tok := range tokens {
				if tok == nil || tok.Id == "" {
					continue
				}
				if appErr := p.API.RevokeUserAccessToken(tok.Id); appErr != nil {
					if isNotFound(appErr) {
						seen[tok.Id] = struct{}{}
						revoked++
						continue
					}
					p.API.LogWarn("failed to revoke bot token on transfer/retire", "bot_user_id", botUserID, "token_id", tok.Id, "error", appErr.Error())
					failMsgs = append(failMsgs, "revoke "+tok.Id+": "+appErr.Error())
					continue
				}
				seen[tok.Id] = struct{}{}
				revoked++
			}
		}
	}

	// Drop reminder rows for RH-tracked tokens (and revoke any the list missed).
	keys, err := p.listTokenExpiryKeys()
	if err != nil {
		failMsgs = append(failMsgs, "list tracked: "+err.Error())
	} else {
		for _, key := range keys {
			rec, _, loadErr := p.loadBotTokenExpiry(key)
			if loadErr != nil {
				failMsgs = append(failMsgs, "load "+key+": "+loadErr.Error())
				continue
			}
			if rec == nil || rec.BotUserID != botUserID {
				continue
			}
			if _, already := seen[rec.TokenID]; already {
				p.deleteBotTokenExpiry(key)
				continue
			}
			if appErr := p.API.RevokeUserAccessToken(rec.TokenID); appErr != nil && !isNotFound(appErr) {
				p.API.LogWarn("failed to revoke tracked bot token on transfer/retire", "bot_user_id", botUserID, "token_id", rec.TokenID, "error", appErr.Error())
				failMsgs = append(failMsgs, "revoke "+rec.TokenID+": "+appErr.Error())
				continue
			}
			seen[rec.TokenID] = struct{}{}
			revoked++
			p.deleteBotTokenExpiry(key)
		}
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

// revokeNewlyIssuedBotToken revokes a just-created token whose secret could
// not be delivered, by its ID, and drops its expiry reminder row.
func (p *Plugin) revokeNewlyIssuedBotToken(tokenID string) error {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return errors.New("the new token's ID is unknown")
	}
	if appErr := p.API.RevokeUserAccessToken(tokenID); appErr != nil && !isNotFound(appErr) {
		return errors.Wrap(appErr, "failed to revoke undelivered bot token")
	}
	p.deleteBotTokenExpiry(botTokenExpiryKey(tokenID))
	return nil
}

// rotateBotToken revokes every token on the bot and issues a fresh one,
// tracked for the new owner so expiry reminders reach them. Returns the new
// token (ID, value and expiry) for the DM; an empty value means none was
// created.
// A partial revoke still issues the replacement, so the bot is never left
// with its working tokens gone and nothing new; the revoke error is
// returned alongside the token.
func (p *Plugin) rotateBotToken(botUserID, botUsername, newOwnerID, adminUserID, requestID string) (revoked int, tokenID, tokenValue string, expiresAt int64, err error) {
	revoked, revokeErr := p.revokeBotTokens(botUserID, adminUserID)
	tok, tokErr := p.createBotToken(botUserID, "Rotated API token for @"+botUsername)
	if tokErr != nil {
		tokErr = errors.Wrap(tokErr, "failed to create replacement token")
		if revokeErr != nil {
			tokErr = errors.Errorf("%s; %s", revokeErr.Error(), tokErr.Error())
		}
		return revoked, "", "", 0, tokErr
	}
	p.trackBotTokenExpiry(tok, botUsername, newOwnerID, requestID)
	return revoked, tok.Id, tok.Token, tok.ExpiresAt, revokeErr
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
