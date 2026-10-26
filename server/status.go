package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// errNoVisibleRequest is the user-facing answer when a request doesn't
// exist or the user may not see it. Both read the same, so the answer
// doesn't say which.
var errNoVisibleRequest = errors.New("no pending request with that ID. It may have been approved, denied or closed; the bot sends the requester the outcome")

// listRequestsDisabledMsg is the list reply when every request type is
// turned off.
const listRequestsDisabledMsg = "Every request type is currently disabled by an administrator."

// maxListedRequests caps how many requests the list views load and show.
const maxListedRequests = 50

// requestSummary describes one pending request for the list and status
// views.
type requestSummary struct {
	ID      string
	Kind    string // a pendingKind* value
	Summary string // Markdown: what was requested, and where
	Expired bool
	// CreatedAt and ExpiresAt are in milliseconds. ExpiresAt is 0 when
	// requests never expire, or for a request saved by an older version
	// that has no CreatedAt yet.
	CreatedAt int64
	ExpiresAt int64
}

// allPendingKinds is every request kind, in the order the views try them.
var allPendingKinds = []string{
	pendingKindChannel, pendingKindAdmin, pendingKindTeam, pendingKindTeamAdmin,
	pendingKindTeamMember, pendingKindTeamGuest, pendingKindBot, pendingKindWebhook,
}

// pendingKindRequestType returns the request type (requestType*) of a
// pending kind, for the per-type toggles.
func pendingKindRequestType(kind string) string {
	switch kind {
	case pendingKindChannel:
		return requestTypeChannel
	case pendingKindAdmin:
		return requestTypeChannelAdmin
	default:
		return kind
	}
}

// enabledPendingKinds returns the request kinds the list and status views
// cover, leaving out any type an administrator turned off.
func enabledPendingKinds(config *configuration) []string {
	var kinds []string
	for _, kind := range allPendingKinds {
		if config.RequestEnabled(pendingKindRequestType(kind)) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// loadPendingRecord loads a stored request of the given kind, or returns
// nil when it isn't stored.
func (p *Plugin) loadPendingRecord(kind, requestID string) (storedRequest, error) {
	info, ok := pendingKinds[kind]
	if !ok {
		return nil, errors.Errorf("unknown request kind %q", kind)
	}
	data, appErr := p.API.KVGet(info.kvPrefix + requestID)
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to load request")
	}
	if data == nil {
		return nil, nil
	}
	rec := info.newRecord()
	if err := json.Unmarshal(data, rec); err != nil {
		return nil, errors.Wrap(err, "failed to read request")
	}
	return rec, nil
}

// recordRequesterID returns who submitted a stored request.
func recordRequesterID(rec storedRequest) string {
	_, requesterID, _ := rec.pendingFields()
	return requesterID
}

// listOwnRequests returns up to limit of the user's pending requests of
// every kind, oldest first, and how many more there are. A kind whose
// request type is turned off is left out. It reads the pending lists
// without changing them. Entries whose request no longer
// exists are skipped, and so are unreadable records, which are logged.
func (p *Plugin) listOwnRequests(userID string, limit int) ([]requestSummary, int, error) {
	type listed struct {
		kind  string
		entry pendingEntry
	}
	config := p.getConfiguration()
	var entries []listed
	for _, kind := range enabledPendingKinds(config) {
		raw, appErr := p.API.KVGet(pendingListKey(kind, userID))
		if appErr != nil {
			return nil, 0, errors.Wrap(appErr, "failed to load pending requests")
		}
		if raw == nil {
			continue
		}
		var list []pendingEntry
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, 0, errors.Wrap(err, "failed to read pending requests")
		}
		for _, e := range list {
			entries = append(entries, listed{kind: kind, entry: e})
		}
	}
	slices.SortStableFunc(entries, func(a, b listed) int {
		return cmp.Compare(a.entry.CreatedAt, b.entry.CreatedAt)
	})

	var out []requestSummary
	for i, e := range entries {
		if len(out) == limit {
			// Not loaded; some may since have been decided.
			return out, len(entries) - i, nil
		}
		rec, err := p.loadPendingRecord(e.kind, e.entry.ID)
		if err != nil {
			if isUnmarshalError(err) {
				p.API.LogWarn("skipping unreadable stored request", "request_id", e.entry.ID, "error", err.Error())
				continue
			}
			return nil, 0, err
		}
		// A request decided after its entry failed to release is gone, and
		// its entry is dropped on the next submit.
		if rec != nil && recordRequesterID(rec) == userID {
			out = append(out, p.summarize(config, e.kind, rec))
		}
	}
	return out, 0, nil
}

// isUnmarshalError reports whether err came from decoding a stored record
// rather than from reading it.
func isUnmarshalError(err error) bool {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
}

// requestStatus returns one pending request, if the user may see it: its
// requester, or someone who can decide it. A request whose type is turned
// off reads as not found.
func (p *Plugin) requestStatus(userID, requestID string) (*requestSummary, error) {
	if !model.IsValidId(requestID) {
		return nil, errNoVisibleRequest
	}
	config := p.getConfiguration()
	for _, kind := range enabledPendingKinds(config) {
		rec, err := p.loadPendingRecord(kind, requestID)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			continue
		}
		if recordRequesterID(rec) != userID {
			user, appErr := p.API.GetUser(userID)
			if appErr != nil {
				p.API.LogWarn("failed to load user for request status", "user_id", userID, "error", appErr.Error())
				return nil, errNoVisibleRequest
			}
			if !p.canDecide(user, rec) {
				return nil, errNoVisibleRequest
			}
		}
		summary := p.summarize(config, kind, rec)
		return &summary, nil
	}
	return nil, errNoVisibleRequest
}

// listApprovableRequests returns up to limit of the pending requests the
// user can approve now, oldest first, and how many more there are. It
// leaves out the user's own requests, types that are turned off, and bot
// and webhook requests where the user has no step to fill. Finding them
// reads every stored request, so it runs only when asked.
func (p *Plugin) listApprovableRequests(userID string, limit int) ([]requestSummary, int, error) {
	user, appErr := p.API.GetUser(userID)
	if appErr != nil {
		return nil, 0, errors.Wrap(appErr, "failed to load user")
	}
	keys, err := p.listRequestKeys()
	if err != nil {
		return nil, 0, err
	}
	config := p.getConfiguration()
	// The same for every request, so looked up once.
	canSec, canSys := p.twoStepPools(user, "approval queue")
	var out []requestSummary
	for _, key := range keys {
		kind, _ := pendingKindForKey(key)
		if !config.RequestEnabled(pendingKindRequestType(kind)) {
			continue
		}
		rec, err := p.loadPendingRecord(kind, strings.TrimPrefix(key, pendingKinds[kind].kvPrefix))
		if err != nil {
			if isUnmarshalError(err) {
				p.API.LogWarn("skipping unreadable stored request", "key", key, "error", err.Error())
				continue
			}
			return nil, 0, err
		}
		if rec == nil || recordRequesterID(rec) == user.Id || !p.canApproveNow(user, rec, canSec, canSys) {
			continue
		}
		out = append(out, p.summarize(config, kind, rec))
	}
	slices.SortStableFunc(out, func(a, b requestSummary) int {
		return cmp.Compare(a.CreatedAt, b.CreatedAt)
	})
	if len(out) > limit {
		return out[:limit], len(out) - limit, nil
	}
	return out, 0, nil
}

// canDecide reports whether the user can approve or deny the request, by
// the same rules the decisions use: for bot and webhook requests, being
// in either approver pool.
func (p *Plugin) canDecide(user *model.User, rec storedRequest) bool {
	switch req := rec.(type) {
	case *channelRequest:
		return p.canApproveChannelRequest(user, req.TeamID)
	case *adminRequest:
		return p.canApproveAdminRequest(user, req.ChannelID)
	case *teamCreationRequest, *teamAdminRequest:
		return p.canApproveTeamRequest(user)
	case *botRequest, *webhookRequest:
		canSec, canSys := p.twoStepPools(user, "request status")
		return canSec || canSys
	default:
		return user.IsSystemAdmin()
	}
}

// canApproveNow reports whether Approve by the user would go ahead as far
// as permissions go: for bot and webhook requests, whether the user, in
// the approval steps canSec and canSys, has an outstanding step to fill.
func (p *Plugin) canApproveNow(user *model.User, rec storedRequest, canSec, canSys bool) bool {
	var state twoStepState
	switch req := rec.(type) {
	case *botRequest:
		state = req.twoStepState
	case *webhookRequest:
		state = req.twoStepState
	default:
		return p.canDecide(user, rec)
	}
	_, fill, _ := planApproval(state, canSec, canSys, user.Id, recordRequesterID(rec))
	return fill
}

// summarize describes a stored request for the list and status views.
func (p *Plugin) summarize(config *configuration, kind string, rec storedRequest) requestSummary {
	id, _, createdAt := rec.pendingFields()
	s := requestSummary{ID: id, Kind: kind, CreatedAt: *createdAt}
	switch req := rec.(type) {
	case *channelRequest:
		s.Summary = fmt.Sprintf("Channel **%s** (`%s`) in %s", escapeMarkdown(req.DisplayName), req.Name, escapeMarkdown(p.teamDisplayName(req.TeamID)))
	case *adminRequest:
		s.Summary = fmt.Sprintf("Channel Admin for %s in %s", p.mentionList(req.NomineeIDs), p.channelLabel(req.ChannelID))
	case *teamCreationRequest:
		s.Summary = fmt.Sprintf("Team **%s** (`/%s`)", escapeMarkdown(req.DisplayName), req.Name)
	case *teamAdminRequest:
		s.Summary = fmt.Sprintf("Team Admin for %s in %s", p.mentionList(req.NomineeIDs), escapeMarkdown(p.teamDisplayName(req.TeamID)))
	case *teamMemberRequest:
		s.Summary = fmt.Sprintf("Add %s to %s", p.mentionList(req.NomineeIDs), escapeMarkdown(p.teamDisplayName(req.TeamID)))
	case *teamGuestRequest:
		s.Summary = fmt.Sprintf("Guest invite for **%s** to %s", escapeMarkdown(req.Email), escapeMarkdown(p.teamDisplayName(req.TeamID)))
	case *botRequest:
		s.Summary = fmt.Sprintf("Bot **@%s**%s", escapeMarkdown(req.Username), awaitingStep(req.twoStepState))
	case *webhookRequest:
		s.Summary = fmt.Sprintf("%s webhook **%s** in %s%s", capitalize(req.WebhookType), escapeMarkdown(req.DisplayName), p.channelLabel(req.ChannelID), awaitingStep(req.twoStepState))
	}
	s.Expired = requestExpired(s.CreatedAt, config.ExpiryDays(), model.GetMillis())
	if days := config.ExpiryDays(); days > 0 && s.CreatedAt > 0 {
		s.ExpiresAt = s.CreatedAt + int64(days)*millisPerDay
	}
	return s
}

// awaitingStep names the approval a two-step request waits for.
func awaitingStep(state twoStepState) string {
	if !state.securityDone() {
		return ", awaiting security approval"
	}
	return ", awaiting system approval"
}

// capitalize upper-cases the first letter of an ASCII word.
func capitalize(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}

// channelLabel renders a channel as ~name for summaries, falling back to
// "a channel that no longer exists".
func (p *Plugin) channelLabel(channelID string) string {
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil || channel == nil {
		return "a channel that no longer exists"
	}
	return "~" + channel.Name
}

// summaryLine renders a request as one Markdown line for the slash
// command.
func (s requestSummary) summaryLine() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, submitted %s", s.Summary, formatDay(s.CreatedAt))
	switch {
	case s.Expired:
		b.WriteString(", expired (an approver can close it)")
	case s.ExpiresAt > 0:
		fmt.Fprintf(&b, ", expires %s", formatDay(s.ExpiresAt))
	}
	fmt.Fprintf(&b, ". ID `%s`", s.ID)
	return b.String()
}

// formatDay renders a millisecond time as a UTC date, or "before the
// upgrade" for a request saved without one.
func formatDay(millis int64) string {
	if millis <= 0 {
		return "before the upgrade"
	}
	return model.GetTimeForMillis(millis).UTC().Format(time.DateOnly)
}
