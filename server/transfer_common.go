package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
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

	// webhookReassignUnsupportedMsg is shown when the server will not change
	// a webhook's creator, which makes Reassign a silent no-op.
	webhookReassignUnsupportedMsg = "This Mattermost version keeps the original creator on a webhook, so Reassign can't move ownership — use Retire & recreate instead."
)

// serverMajorVersion parses the leading major number of a Mattermost version
// string. ok is false when the version is missing or unparseable.
func serverMajorVersion(version string) (major int, ok bool) {
	majorPart, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	parsed, err := strconv.Atoi(strings.TrimSpace(majorPart))
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// webhookReassignUnsupported reports whether this server preserves a
// webhook's original creator through an update. Mattermost 11 does, so
// Reassign would report success while ownership never moved.
func (p *Plugin) webhookReassignUnsupported() bool {
	major, ok := serverMajorVersion(p.API.GetServerVersion())
	return ok && major >= 11
}

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

// listBotAccessTokens returns every access token on the bot. Mattermost's
// page/offset token API has no ORDER BY, so rows can shift between pages and
// a single walk can miss one. Two full walks must agree on the token-ID set;
// anything else fails closed so revoke-all never pretends to have finished.
func (p *Plugin) listBotAccessTokens(botUserID, adminUserID string) ([]*model.UserAccessToken, error) {
	if p.listBotAccessTokensFn != nil {
		return p.listBotAccessTokensFn(botUserID, adminUserID)
	}
	fetch := p.listBotAccessTokensPageFn
	if fetch == nil {
		client, cleanup, err := p.transferClient(adminUserID)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		ctx := context.Background()
		fetch = func(botUserID, _ string, page int) ([]*model.UserAccessToken, error) {
			tokens, _, listErr := client.GetUserAccessTokensForUser(ctx, botUserID, page, botAccessTokenListPerPage)
			return tokens, listErr
		}
	}
	walk := func() (map[string]*model.UserAccessToken, []string, error) {
		byID := map[string]*model.UserAccessToken{}
		var order []string
		for page := 0; ; page++ {
			tokens, listErr := fetch(botUserID, adminUserID, page)
			if listErr != nil {
				return nil, nil, errors.Wrap(listErr, "failed to list bot access tokens")
			}
			for _, tok := range tokens {
				if tok == nil || tok.Id == "" {
					continue
				}
				if _, dup := byID[tok.Id]; dup {
					// The same row on two pages means the window shifted
					// mid-walk, so some other token was skipped. Fail
					// closed instead of reporting a short list as complete.
					return nil, nil, errors.New("the bot's token list shifted while it was being read; try again")
				}
				byID[tok.Id] = tok
				order = append(order, tok.Id)
			}
			if len(tokens) < botAccessTokenListPerPage {
				return byID, order, nil
			}
		}
	}

	first, order, err := walk()
	if err != nil {
		return nil, err
	}
	second, _, err := walk()
	if err != nil {
		return nil, err
	}
	if len(first) != len(second) {
		return nil, errors.Errorf("the bot's token list changed while it was being read (%d then %d tokens); try again", len(first), len(second))
	}
	out := make([]*model.UserAccessToken, 0, len(order))
	for _, id := range order {
		if _, ok := second[id]; !ok {
			return nil, errors.New("the bot's token list changed while it was being read; try again")
		}
		out = append(out, first[id])
	}
	return out, nil
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

// botTokenTargets is every token to revoke on a bot, captured before any
// revoke or replacement-create runs so rotation can issue the new token
// first and still know exactly which old ones to kill.
type botTokenTargets struct {
	listed      []string          // token IDs from the live API list
	tracked     map[string]string // RH-tracked token ID → expiry KV key
	trackedIDs  []string          // tracked IDs in listing order
	listFailure []string
}

// collectBotTokens captures the bot's current tokens without revoking any of
// them: the live API list plus RH's tracked expiry rows.
func (p *Plugin) collectBotTokens(botUserID, adminUserID string) botTokenTargets {
	targets := botTokenTargets{tracked: map[string]string{}}
	if adminUserID != "" {
		tokens, listErr := p.listBotAccessTokens(botUserID, adminUserID)
		if listErr != nil {
			targets.listFailure = append(targets.listFailure, "list tokens: "+listErr.Error())
		} else {
			for _, tok := range tokens {
				if tok == nil || tok.Id == "" {
					continue
				}
				targets.listed = append(targets.listed, tok.Id)
			}
		}
	}
	keys, err := p.listTokenExpiryKeys()
	if err != nil {
		targets.listFailure = append(targets.listFailure, "list tracked: "+err.Error())
		return targets
	}
	for _, key := range keys {
		rec, _, loadErr := p.loadBotTokenExpiry(key)
		if loadErr != nil {
			targets.listFailure = append(targets.listFailure, "load "+key+": "+loadErr.Error())
			continue
		}
		if rec == nil || rec.BotUserID != botUserID {
			continue
		}
		if _, dup := targets.tracked[rec.TokenID]; dup {
			continue
		}
		targets.tracked[rec.TokenID] = key
		targets.trackedIDs = append(targets.trackedIDs, rec.TokenID)
	}
	return targets
}

// botTokenRevokeRenewEvery is how many revoke attempts pass between claim
// lease renewals during a long revoke-all.
const botTokenRevokeRenewEvery = 25

// tokenRevokeFailures collects revoke problems by token ID, so a token that
// fails on the API list and then succeeds (or reads as already gone) from
// its tracked row is not reported as a failure.
type tokenRevokeFailures struct {
	general []string
	order   []string
	byID    map[string]string
}

func (f *tokenRevokeFailures) failed(id, msg string) {
	if f.byID == nil {
		f.byID = map[string]string{}
	}
	if _, seen := f.byID[id]; !seen {
		f.order = append(f.order, id)
	}
	f.byID[id] = msg
}

func (f *tokenRevokeFailures) cleared(id string) { delete(f.byID, id) }

func (f *tokenRevokeFailures) messages() []string {
	out := append([]string(nil), f.general...)
	for _, id := range f.order {
		if msg, ok := f.byID[id]; ok {
			out = append(out, msg)
		}
	}
	return out
}

// revokeBotTokenTargets revokes a captured token set and cleans the tracked
// reminder rows. Already-revoked / not-found tokens count as success. Expiry
// rows are dropped only after a confirmed revoke so reminders keep retrying
// on transient failures. The lease, when set, is renewed as the loop runs;
// losing it stops the walk before the next revoke.
func (p *Plugin) revokeBotTokenTargets(botUserID string, lease transferLease, targets botTokenTargets) (int, error) {
	revoked := 0
	seen := map[string]struct{}{}
	fails := tokenRevokeFailures{general: append([]string(nil), targets.listFailure...)}
	attempts := 0
	renew := func() error {
		attempts++
		if attempts%botTokenRevokeRenewEvery != 0 {
			return nil
		}
		return p.renewTransferLease(lease)
	}
	report := func() error {
		if msgs := fails.messages(); len(msgs) > 0 {
			return errors.Errorf("revoked %d token(s); failures: %s", revoked, strings.Join(msgs, "; "))
		}
		return nil
	}

	for _, id := range targets.listed {
		if renewErr := renew(); renewErr != nil {
			fails.general = append(fails.general, "stopped part-way through: "+renewErr.Error())
			return revoked, report()
		}
		if appErr := p.API.RevokeUserAccessToken(id); appErr != nil {
			if isNotFound(appErr) {
				seen[id] = struct{}{}
				fails.cleared(id)
				revoked++
				continue
			}
			p.API.LogWarn("failed to revoke bot token on transfer/retire", "bot_user_id", botUserID, "token_id", id, "error", appErr.Error())
			fails.failed(id, "revoke "+id+": "+appErr.Error())
			continue
		}
		seen[id] = struct{}{}
		fails.cleared(id)
		revoked++
	}

	// Drop reminder rows for RH-tracked tokens (and revoke any the list missed).
	for _, id := range targets.trackedIDs {
		key := targets.tracked[id]
		if _, already := seen[id]; already {
			p.deleteBotTokenExpiry(key)
			continue
		}
		if renewErr := renew(); renewErr != nil {
			fails.general = append(fails.general, "stopped part-way through: "+renewErr.Error())
			return revoked, report()
		}
		if appErr := p.API.RevokeUserAccessToken(id); appErr != nil && !isNotFound(appErr) {
			p.API.LogWarn("failed to revoke tracked bot token on transfer/retire", "bot_user_id", botUserID, "token_id", id, "error", appErr.Error())
			fails.failed(id, "revoke "+id+": "+appErr.Error())
			continue
		}
		seen[id] = struct{}{}
		fails.cleared(id)
		revoked++
		p.deleteBotTokenExpiry(key)
	}
	return revoked, report()
}

// revokeBotTokens revokes every access token on the bot (including ones the
// owner created by hand) via an admin session.
func (p *Plugin) revokeBotTokens(botUserID, adminUserID string, lease transferLease) (int, error) {
	return p.revokeBotTokenTargets(botUserID, lease, p.collectBotTokens(botUserID, adminUserID))
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

// issuedBotToken is a freshly created replacement token together with the
// tokens the bot had before it existed.
type issuedBotToken struct {
	targets   botTokenTargets
	tokenID   string
	token     string
	expiresAt int64
}

// issueReplacementBotToken captures the bot's current tokens and creates
// the successor's replacement, tracked so expiry reminders reach them.
// Nothing is revoked: listing first and creating before revoking means a
// failed create leaves the bot's working tokens alone, and the replacement
// can never land in the revoke set.
func (p *Plugin) issueReplacementBotToken(botUserID, botUsername, newOwnerID, adminUserID, requestID string) (issuedBotToken, error) {
	targets := p.collectBotTokens(botUserID, adminUserID)
	tok, tokErr := p.createBotToken(botUserID, "Rotated API token for @"+botUsername)
	if tokErr != nil {
		tokErr = errors.Wrap(tokErr, "failed to create replacement token")
		if len(targets.listFailure) > 0 {
			tokErr = errors.Errorf("%s; %s", strings.Join(targets.listFailure, "; "), tokErr.Error())
		}
		return issuedBotToken{}, tokErr
	}
	p.trackBotTokenExpiry(tok, botUsername, newOwnerID, requestID)
	return issuedBotToken{targets: targets, tokenID: tok.Id, token: tok.Token, expiresAt: tok.ExpiresAt}, nil
}

// rotateBotToken issues a fresh token for the new owner and then revokes
// the tokens the bot had before it. Callers that must hand the secret over
// before anything is revoked (a voluntary handover) use
// issueReplacementBotToken and revokeBotTokenTargets directly. Returns the
// new token (ID, value and expiry) for the DM; a revoke failure is returned
// alongside it.
func (p *Plugin) rotateBotToken(botUserID, botUsername, newOwnerID, adminUserID string, lease transferLease) (revoked int, tokenID, tokenValue string, expiresAt int64, err error) {
	issued, issueErr := p.issueReplacementBotToken(botUserID, botUsername, newOwnerID, adminUserID, lease.requestID)
	if issueErr != nil {
		return 0, "", "", 0, issueErr
	}
	revoked, revokeErr := p.revokeBotTokenTargets(botUserID, lease, issued.targets)
	return revoked, issued.tokenID, issued.token, issued.expiresAt, revokeErr
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
