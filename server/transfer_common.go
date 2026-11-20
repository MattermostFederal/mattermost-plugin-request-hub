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
// webhook's original creator through an update, which would make Reassign
// report success while ownership never moved. Both the 9.11 federal build
// and Mattermost 11 restore the stored creator, so every major from 9 up is
// treated as unsupported rather than guessing per patch release.
func (p *Plugin) webhookReassignUnsupported() bool {
	major, ok := serverMajorVersion(p.API.GetServerVersion())
	return ok && major >= 9
}

// serverRefused reports an error Mattermost clearly turned down (a 4xx), so
// the call it names cannot have taken effect. Anything else — a timeout, a
// 5xx, a dropped connection — may have landed with its response lost, and
// callers have to treat the outcome as unknown rather than as a no-op.
func serverRefused(err error) bool {
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr == nil {
		return false
	}
	return appErr.StatusCode >= http.StatusBadRequest && appErr.StatusCode < http.StatusInternalServerError
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

// transferReads bounds a batch of live Mattermost reads: one admin session
// for the whole batch, and one deadline covering all of it. The transfer
// picker reads once per indexed asset, so a session (and an unbounded
// context) per asset turns a long inventory into a request that keeps
// working long after its caller has gone.
type transferReads struct {
	p        *Plugin
	ctx      context.Context
	cancel   context.CancelFunc
	adminID  string
	client   *model.Client4
	release  func()
	failed   bool
	outgoing map[string]outgoingSnapshotResult
}

// outgoingSnapshotResult remembers one outgoing-hook read, failures
// included: a read that could not be made stays unknown for the batch
// instead of being retried against a session that is already in trouble.
type outgoingSnapshotResult struct {
	snap *outgoingWebhookRecreate
	err  error
}

// newTransferReads opens a read batch run as adminUserID. parent is the
// caller's context — an HTTP request's, so a client that walks away stops
// the reads — and budget caps the batch however long the caller waits.
func (p *Plugin) newTransferReads(parent context.Context, adminUserID string, budget time.Duration) *transferReads {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	return &transferReads{p: p, ctx: ctx, cancel: cancel, adminID: strings.TrimSpace(adminUserID)}
}

// oneShotReads is a read batch for a single lookup off the request path.
func (p *Plugin) oneShotReads(adminUserID string) *transferReads {
	return p.newTransferReads(context.Background(), adminUserID, transferRESTTimeout)
}

func (t *transferReads) close() {
	if t == nil {
		return
	}
	t.cancel()
	if t.release != nil {
		t.release()
		t.release = nil
	}
}

// live hands out the shared client. ok is false once the batch has no
// admin to act as, has run out of time, or could not open a session —
// callers then report the read as unknown rather than waiting longer.
func (t *transferReads) live() (*model.Client4, context.Context, bool) {
	if t == nil || t.failed || t.adminID == "" || t.ctx.Err() != nil {
		return nil, nil, false
	}
	if t.client == nil {
		client, cleanup, err := t.p.transferClient(t.adminID)
		if err != nil {
			t.failed = true
			return nil, nil, false
		}
		t.client, t.release = client, cleanup
	}
	return t.client, t.ctx, true
}

// liveWebhookOwnerID reads a webhook's current Mattermost owner. The RH
// index falls behind whenever a reassign changed the hook but could not
// write the index row, so ownership checks must prefer the live value.
//
// known is false when the hook could not be read at all — reading someone
// else's hook needs Manage*Webhooks, which a plain requester does not have
// — and callers fall back to the index owner rather than refusing them.
func (p *Plugin) liveWebhookOwnerID(webhookType, hookID, adminUserID string) (owner string, known bool) {
	reads := p.oneShotReads(adminUserID)
	defer reads.close()
	return reads.webhookOwnerID(webhookType, hookID)
}

func (t *transferReads) webhookOwnerID(webhookType, hookID string) (owner string, known bool) {
	hookID = strings.TrimSpace(hookID)
	if t == nil || hookID == "" || t.adminID == "" {
		return "", false
	}
	if t.p.liveWebhookOwnerFn != nil {
		live, err := t.p.liveWebhookOwnerFn(webhookType, hookID, t.adminID)
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(live), strings.TrimSpace(live) != ""
	}
	switch webhookType {
	case webhookTypeOutgoing:
		// Reuse the recreate read so an outgoing hook is fetched one way.
		snap, snapErr := t.outgoingSnapshot(hookID)
		if snapErr != nil || snap == nil {
			return "", false
		}
		creator := strings.TrimSpace(snap.CreatorID)
		return creator, creator != ""
	case webhookTypeIncoming:
		client, ctx, ok := t.live()
		if !ok {
			return "", false
		}
		hook, _, apiErr := client.GetIncomingWebhook(ctx, hookID, "")
		if apiErr != nil || hook == nil || strings.TrimSpace(hook.UserId) == "" {
			return "", false
		}
		return strings.TrimSpace(hook.UserId), true
	}
	return "", false
}

// liveIncomingWebhookChannelID reads where an incoming hook posts now. A
// retire+recreate rebuilds the hook from the ticket, so a hook that has
// been moved since must not be rebuilt in the channel the reviewers saw.
func (p *Plugin) liveIncomingWebhookChannelID(hookID, adminUserID string) (string, error) {
	reads := p.oneShotReads(adminUserID)
	defer reads.close()
	return reads.incomingWebhookChannelID(hookID)
}

func (t *transferReads) incomingWebhookChannelID(hookID string) (string, error) {
	hookID = strings.TrimSpace(hookID)
	if hookID == "" {
		return "", errors.New("the webhook's ID is unknown")
	}
	if t.p.incomingWebhookChannelFn != nil {
		return t.p.incomingWebhookChannelFn(hookID, t.adminID)
	}
	client, ctx, ok := t.live()
	if !ok {
		return "", errors.New("the incoming webhook could not be read")
	}
	hook, _, apiErr := client.GetIncomingWebhook(ctx, hookID, "")
	if apiErr != nil {
		return "", errors.Wrap(apiErr, "failed to load incoming webhook")
	}
	if hook == nil {
		return "", errors.New("incoming webhook not found")
	}
	return strings.TrimSpace(hook.ChannelId), nil
}

// liveWebhookChannelID reads where a hook of either type is scoped now.
// known is false when the read failed or the seam in use carries no
// channel at all — an unverifiable scope must never pass for the channel a
// ticket was approved against.
func (p *Plugin) liveWebhookChannelID(webhookType, hookID, adminUserID string) (channelID string, known bool) {
	reads := p.oneShotReads(adminUserID)
	defer reads.close()
	return reads.webhookChannelID(webhookType, hookID)
}

func (t *transferReads) webhookChannelID(webhookType, hookID string) (channelID string, known bool) {
	if strings.TrimSpace(hookID) == "" {
		return "", false
	}
	switch webhookType {
	case webhookTypeIncoming:
		live, err := t.incomingWebhookChannelID(hookID)
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(live), true
	case webhookTypeOutgoing:
		snap, err := t.outgoingSnapshot(hookID)
		if err != nil || snap == nil || snap.channelUnknown {
			return "", false
		}
		// An outgoing hook with no channel fires team-wide. That is a real
		// scope, not a failed read.
		return strings.TrimSpace(snap.ChannelID), true
	}
	return "", false
}

// outgoingSnapshot reads the live outgoing hook through the shared session.
// The result is held for the life of the batch, so a caller that wants the
// owner and the channel costs one read rather than two.
func (t *transferReads) outgoingSnapshot(hookID string) (*outgoingWebhookRecreate, error) {
	if hit, ok := t.outgoing[hookID]; ok {
		return hit.snap, hit.err
	}
	snap, err := t.readOutgoing(hookID)
	if t.outgoing == nil {
		t.outgoing = map[string]outgoingSnapshotResult{}
	}
	t.outgoing[hookID] = outgoingSnapshotResult{snap: snap, err: err}
	return snap, err
}

func (t *transferReads) readOutgoing(hookID string) (*outgoingWebhookRecreate, error) {
	if t.p.getOutgoingWebhookCallbackFn != nil || t.p.getOutgoingWebhookForRecreateFn != nil {
		return t.p.loadOutgoingWebhookForRecreate(hookID, t.adminID)
	}
	client, ctx, ok := t.live()
	if !ok {
		return nil, errors.New("the outgoing webhook could not be read")
	}
	hook, _, apiErr := client.GetOutgoingWebhook(ctx, hookID)
	if apiErr != nil {
		return nil, errors.Wrap(apiErr, "failed to load outgoing webhook for recreate")
	}
	return outgoingRecreateFromHook(hook)
}

// liveWebhookScope reads a hook's current owner and channel together, for
// callers that have to write both down. known is false unless Mattermost
// supplied them.
func (p *Plugin) liveWebhookScope(webhookType, hookID, adminUserID string) (owner, channelID string, known bool) {
	owner, ownerKnown := p.liveWebhookOwnerID(webhookType, hookID, adminUserID)
	if !ownerKnown {
		return "", "", false
	}
	channelID, channelKnown := p.liveWebhookChannelID(webhookType, hookID, adminUserID)
	if !channelKnown {
		return "", "", false
	}
	return owner, channelID, true
}

// webhookScopeForChecks is the owner and the channel a submit runs its
// checks against: Mattermost's own values when they can be read, otherwise
// the index row. A live value that disagrees with the index is written
// back, so a ticket filed after the hook moved is filed against the channel
// it sits in now. Approval re-checks the same place, so "submit a new
// request for the channel it is in now" is advice a requester can act on
// rather than one that files the same refusal again. index is updated in
// place to match what was adopted.
func (p *Plugin) webhookScopeForChecks(index *webhookIndexEntry, readerUserID string) (owner, channelID string) {
	if index == nil {
		return "", ""
	}
	reads := p.oneShotReads(readerUserID)
	defer reads.close()
	owner, _ = p.webhookOwnerForChecksWith(index, reads)
	channelID = index.ChannelID
	live, known := reads.webhookChannelID(index.WebhookType, index.HookID)
	if !known || live == channelID {
		return owner, channelID
	}
	if err := p.updateWebhookIndexChannel(index.HookID, live); err != nil {
		p.API.LogWarn("failed to repair a stale webhook index channel", "request_id", index.RequestID, "error", err.Error())
	}
	index.ChannelID = live
	return owner, live
}

// webhookOwnerForChecksWith is the owner the nominate ACL and successor
// checks run against: the live Mattermost owner when it can be read,
// otherwise the index row, which is written back when the two disagree so a
// reassign whose index write failed stops handing transfer rights to the
// previous owner. liveKnown is false when Mattermost could not be asked, so
// callers that list assets can tell a verified owner from the index's last
// word.
func (p *Plugin) webhookOwnerForChecksWith(index *webhookIndexEntry, reads *transferReads) (owner string, liveKnown bool) {
	if index == nil {
		return "", false
	}
	indexOwner := webhookIndexOwner(*index)
	live, known := reads.webhookOwnerID(index.WebhookType, index.HookID)
	if !known {
		return indexOwner, false
	}
	if live == indexOwner {
		return indexOwner, true
	}
	if err := p.updateWebhookIndexOwner(index.HookID, live); err != nil {
		p.API.LogWarn("failed to repair a stale webhook index owner", "request_id", index.RequestID, "error", err.Error())
	}
	return live, true
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
//
// Two agreeing walks are a guard against a shifting window, not proof the
// list is complete: on a replica-backed server both walks can be served by
// the same lagging replica, so a token created (or one this process has not
// seen) is absent from both. Completeness is only ever established by
// confirmBotTokensRevoked reading an empty set after the revokes.
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
	// adminUserID is who the capture ran as, so the confirmation read
	// after the revokes can use the same session rights.
	adminUserID string
}

// collectBotTokens captures the bot's current tokens without revoking any of
// them: the live API list plus RH's tracked expiry rows.
func (p *Plugin) collectBotTokens(botUserID, adminUserID string) botTokenTargets {
	targets := botTokenTargets{tracked: map[string]string{}, adminUserID: strings.TrimSpace(adminUserID)}
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
// on transient failures. The lease, when set, is checked before the first
// revoke and renewed as the loop runs; losing it stops the walk before the
// next revoke, so an approval that no longer owns the asset never touches a
// token.
//
// keep names tokens that are allowed to survive — a replacement issued
// before the revoke. Everything else has to be gone by the time this
// returns nil: the captured set is only what two reads happened to show,
// so a clean run is confirmed against a fresh read rather than assumed.
func (p *Plugin) revokeBotTokenTargets(botUserID string, lease transferLease, targets botTokenTargets, keep ...string) (int, error) {
	revoked := 0
	seen := map[string]struct{}{}
	fails := tokenRevokeFailures{general: append([]string(nil), targets.listFailure...)}
	attempts := 0
	renew := func() error {
		at := attempts
		attempts++
		if at%botTokenRevokeRenewEvery != 0 {
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
	if err := report(); err != nil {
		return revoked, err
	}
	return revoked, p.confirmBotTokensRevoked(botUserID, targets.adminUserID, keep)
}

// confirmBotTokensRevoked re-reads the bot's tokens and fails closed unless
// the only ones left are in keep.
//
// The captured target set comes from two agreeing walks of a paged list
// with no stable order. On a replica-backed server both walks can be
// answered by the same lagging replica, so "the list looked the same
// twice" says nothing about tokens the replica has not caught up on, and
// the plugin API has no revoke-every-token-for-this-user call to lean on
// instead. A read after the revokes that comes back empty is the only
// evidence available that the bot has nothing live left; anything else is
// reported as incomplete so no approval ever claims the tokens are gone.
func (p *Plugin) confirmBotTokensRevoked(botUserID, adminUserID string, keep []string) error {
	if strings.TrimSpace(adminUserID) == "" {
		return errors.New("whether the bot has any tokens left could not be checked without an admin session; revoke anything remaining under Integrations → Bot Accounts")
	}
	tokens, err := p.listBotAccessTokens(botUserID, adminUserID)
	if err != nil {
		return errors.Wrap(err, "the bot's tokens could not be re-read to confirm the revoke finished, so some may still be live; retry Approve")
	}
	allowed := make(map[string]struct{}, len(keep))
	for _, id := range keep {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = struct{}{}
		}
	}
	left := 0
	for _, tok := range tokens {
		if tok == nil || tok.Id == "" {
			continue
		}
		if _, ok := allowed[tok.Id]; ok {
			continue
		}
		left++
	}
	if left > 0 {
		return errors.Errorf("%d token(s) are still live on the bot after the revoke; revoke them under Integrations → Bot Accounts and try again", left)
	}
	return nil
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
// can never land in the revoke set. The captured targets come back even on
// a failed create, so a caller whose ownership change already landed can
// still cut the previous owner off.
func (p *Plugin) issueReplacementBotToken(botUserID, botUsername, newOwnerID, adminUserID, requestID string) (issuedBotToken, error) {
	targets := p.collectBotTokens(botUserID, adminUserID)
	tok, tokErr := p.createBotToken(botUserID, "Rotated API token for @"+botUsername)
	if tokErr != nil {
		tokErr = errors.Wrap(tokErr, "failed to create replacement token")
		if len(targets.listFailure) > 0 {
			tokErr = errors.Errorf("%s; %s", strings.Join(targets.listFailure, "; "), tokErr.Error())
		}
		return issuedBotToken{targets: targets}, tokErr
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
	revoked, revokeErr := p.revokeBotTokenTargets(botUserID, lease, issued.targets, issued.tokenID)
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
