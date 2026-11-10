package main

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// kvHistoryPrefix namespaces each request's audit history:
// history_{requestID} → requestHistory JSON. It must not start with any
// request prefix (request_, bot_request_, …): pendingKindForKey matches
// those by prefix. History is never deleted.
const kvHistoryPrefix = "history_"

// historyUpdateAttempts bounds the compare-and-set retries when two events
// for the same request land at once. More than the pending list's, since
// ticket thread messages can arrive in bursts from several nodes.
const historyUpdateAttempts = 8

// historyListPageSize is how many KV keys listHistories reads at a time.
const historyListPageSize = 200

// historyExportMaxHistories / historyExportMaxEvents bound a single histories
// export so an unbounded KV store cannot OOM the node on download/approve.
// Prefer filtering by date/request in the UI; these are hard ceilings only.
// Vars (not consts) so unit tests can override the caps.
var (
	historyExportMaxHistories = 500
	historyExportMaxEvents    = 20000
)

// Audit events. Every event that touches a request is recorded under one
// of these names, in the request history, the server audit log (as
// requesthub_<event>) and, depending on AuditChannelDetail, the audit
// channel.
const (
	eventSubmitted           = "submitted"
	eventSubmitFailed        = "submit_failed"
	eventSubmitRefused       = "submit_refused"
	eventStepApproved        = "step_approved"
	eventApproved            = "approved"
	eventApprovedPartial     = "approved_partial"
	eventDenied              = "denied"
	eventExpired             = "expired"
	eventApproveFailed       = "approve_failed"
	eventLost                = "lost"
	eventUndelivered         = "undelivered"
	eventClosedAfterTeardown = "closed_after_teardown"
	eventDecisionRefused     = "decision_refused"
	eventMessageRelayed      = "message_relayed"
	eventMessageDropped      = "message_dropped"
	eventTokenReminder       = "token_reminder"
	eventTokenTrackingEnded  = "token_tracking_ended"
	eventTokenPostDeleted    = "token_post_deleted" //nolint:gosec // G101: an event name, not a credential
	eventExerciseSeeded      = "exercise_seeded"
	eventExerciseRemoved     = "exercise_removed"
	eventMigrated            = "migrated"
	eventAuditExported       = "audit_exported"
)

// History statuses: where a request ended up, or pending while it is open.
const (
	historyPending  = "pending"
	historyApproved = "approved"
	historyDenied   = "denied"
	historyExpired  = "expired"
	historyFailed   = "failed"
	historyLost     = "lost"
	historyClosed   = "closed"
)

// auditNever marks an event that is never posted to the audit channel.
const auditNever = -1

// auditEventSpec is how an event is recorded.
type auditEventSpec struct {
	// status is the history status the event moves the request to; ""
	// leaves it.
	status string
	// card is the audit channel card style; level the lowest
	// AuditChannelDetail level that posts it (auditNever for none).
	card  auditOutcome
	level int
	// failed records the event with a failed outcome.
	failed bool
	// existingOnly appends the event only to a history that already exists,
	// so a click on a card for an unknown request starts no history.
	existingOnly bool
}

var auditEventSpecs = map[string]auditEventSpec{
	eventSubmitted:           {status: historyPending, card: auditSubmitted, level: auditLevelLifecycle},
	eventSubmitFailed:        {status: historyFailed, card: auditFailed, level: auditLevelLifecycle, failed: true},
	eventSubmitRefused:       {level: auditNever, failed: true},
	eventStepApproved:        {card: auditStepApproved, level: auditLevelLifecycle},
	eventApproved:            {status: historyApproved, card: auditApproved, level: auditLevelDecisions},
	eventApprovedPartial:     {status: historyApproved, card: auditPartial, level: auditLevelDecisions},
	eventDenied:              {status: historyDenied, card: auditDenied, level: auditLevelDecisions},
	eventExpired:             {status: historyExpired, card: auditExpired, level: auditLevelDecisions},
	eventApproveFailed:       {card: auditFailed, level: auditLevelLifecycle, failed: true},
	eventLost:                {status: historyLost, card: auditFailed, level: auditLevelLifecycle, failed: true},
	eventUndelivered:         {status: historyFailed, card: auditFailed, level: auditLevelLifecycle, failed: true},
	eventClosedAfterTeardown: {status: historyClosed, card: auditNote, level: auditLevelLifecycle},
	eventDecisionRefused:     {card: auditRefused, level: auditLevelAll, failed: true, existingOnly: true},
	eventMessageRelayed:      {card: auditMessage, level: auditLevelAll},
	eventMessageDropped:      {card: auditRefused, level: auditLevelAll, failed: true},
	eventTokenReminder:       {card: auditNote, level: auditLevelLifecycle},
	eventTokenTrackingEnded:  {card: auditNote, level: auditLevelLifecycle},
	eventTokenPostDeleted:    {card: auditNote, level: auditLevelLifecycle},
	eventExerciseSeeded:      {status: historyPending, level: auditNever},
	eventExerciseRemoved:     {status: historyClosed, level: auditNever},
	eventMigrated:            {level: auditNever},
	eventAuditExported:       {level: auditNever},
}

// auditEntry is one event as the code that saw it reports it.
type auditEntry struct {
	Event       string
	Kind        string // pendingKind*
	RequestID   string
	RequesterID string
	// ActorID is the user who did it; "" for the plugin itself.
	ActorID   string
	ActorName string
	// Note is the human-readable line, also the audit channel card text.
	Note string
	// Detail holds the event's particulars. JSON-friendly values only, and
	// never a secret (token, incoming webhook URL).
	Detail map[string]any
	// Snapshot is the request record, kept as the history's latest copy.
	Snapshot any
	Exercise bool
	// ThreadPostID is the card the audit channel card links to.
	ThreadPostID string
	// CardText, when set, is the audit channel card text instead of Note.
	CardText string
	// At is when it happened, in milliseconds; 0 means now.
	At int64
}

// auditEvent is one event in a request's history.
type auditEvent struct {
	At        int64          `json:"at"`
	Event     string         `json:"event"`
	ActorID   string         `json:"actor_id,omitempty"`
	ActorName string         `json:"actor_name,omitempty"`
	Outcome   string         `json:"outcome"`
	Note      string         `json:"note,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// requestHistory is everything that happened to one request. It outlives
// the request record, which is deleted once the request is decided.
type requestHistory struct {
	RequestID   string          `json:"request_id"`
	Kind        string          `json:"kind"`
	RequesterID string          `json:"requester_id,omitempty"`
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
	Status      string          `json:"status"`
	Exercise    bool            `json:"exercise,omitempty"`
	Request     json.RawMessage `json:"request,omitempty"`
	Events      []auditEvent    `json:"events"`
}

// recordEvent records e in the request history and the server audit log,
// and posts it to the audit channel when AuditChannelDetail asks for it.
// It never fails the action being audited: a history that can't be saved
// is logged as an error with the whole event.
func (p *Plugin) recordEvent(e auditEntry) {
	spec, ok := auditEventSpecs[e.Event]
	if !ok {
		spec = auditEventSpec{card: auditNote, level: auditLevelLifecycle}
	}
	if e.At == 0 {
		e.At = model.GetMillis()
	}
	ev := auditEvent{
		At:        e.At,
		Event:     e.Event,
		ActorID:   e.ActorID,
		ActorName: e.ActorName,
		Outcome:   model.AuditStatusSuccess,
		Note:      strings.TrimSpace(e.Note),
		Detail:    e.Detail,
	}
	if spec.failed {
		ev.Outcome = model.AuditStatusFail
	}

	if p.auditSink != nil {
		p.auditSink(e, spec)
		return
	}
	// Events about no stored request (a refused submit, an export) go to
	// the server audit log only.
	if e.RequestID != "" {
		if err := p.appendHistory(e, spec, ev); err != nil {
			data, _ := json.Marshal(ev)
			p.API.LogError("failed to save request audit history", "request_id", e.RequestID, "kind", e.Kind, "event", string(data), "error", err.Error())
		}
	}
	p.logAuditRecord(e, ev)

	text := ev.Note
	if e.CardText != "" {
		text = strings.TrimSpace(e.CardText)
	}
	if spec.level != auditNever && spec.level <= p.getConfiguration().AuditChannelLevel() && text != "" {
		p.postAuditCard(spec.card, exerciseLabel(e.Exercise)+text, e.ThreadPostID)
	}
}

// appendHistory adds ev to the request's history with compare-and-set,
// creating the history on its first event.
func (p *Plugin) appendHistory(e auditEntry, spec auditEventSpec, ev auditEvent) error {
	key := kvHistoryPrefix + e.RequestID
	var snapshot json.RawMessage
	if e.Snapshot != nil {
		data, err := json.Marshal(e.Snapshot)
		if err != nil {
			return fmt.Errorf("failed to encode request snapshot: %w", err)
		}
		snapshot = data
	}
	for attempt := range historyUpdateAttempts {
		if attempt > 0 {
			// Back off a little, with jitter, so events racing for the same
			// history (on one node or several) stop colliding.
			time.Sleep(time.Duration(attempt*attempt*5+rand.IntN(10)) * time.Millisecond) //nolint:gosec // G404: retry jitter, not security
		}
		raw, appErr := p.API.KVGet(key)
		if appErr != nil {
			return appErr
		}
		var h requestHistory
		if raw == nil {
			if spec.existingOnly {
				return nil
			}
			h = requestHistory{RequestID: e.RequestID, CreatedAt: ev.At, Status: historyPending}
		} else if err := json.Unmarshal(raw, &h); err != nil {
			// Never overwrite a history that can't be read.
			return fmt.Errorf("failed to read request history: %w", err)
		}
		if h.Kind == "" {
			h.Kind = e.Kind
		}
		if h.RequesterID == "" {
			h.RequesterID = e.RequesterID
		}
		if e.Exercise {
			h.Exercise = true
		}
		// A submission is recorded once it is fully posted, which can be
		// after an approver already decided it. A pending status (and its
		// older copy of the request) never replaces a final one.
		reopens := spec.status == historyPending && h.Status != historyPending && h.Status != ""
		if spec.status != "" && !reopens {
			h.Status = spec.status
		}
		if snapshot != nil && (!reopens || h.Request == nil) {
			h.Request = snapshot
		}
		h.CreatedAt = min(h.CreatedAt, ev.At)
		h.UpdatedAt = max(h.UpdatedAt, ev.At)
		h.Events = insertByTime(h.Events, boundedEvent(ev, len(raw)))

		data, err := json.Marshal(h)
		if err != nil {
			return fmt.Errorf("failed to encode request history: %w", err)
		}
		saved, appErr := p.API.KVCompareAndSet(key, raw, data)
		if appErr != nil {
			return appErr
		}
		if saved {
			return nil
		}
	}
	return fmt.Errorf("request history changed %d times while saving", historyUpdateAttempts)
}

// insertByTime adds ev after every event that happened at or before it,
// so the history reads in the order things happened.
func insertByTime(events []auditEvent, ev auditEvent) []auditEvent {
	i := len(events)
	for i > 0 && events[i-1].At > ev.At {
		i--
	}
	return slices.Insert(events, i, ev)
}

// History size guard: once a history passes historySoftLimitBytes, relayed
// message text kept in it is cut to historyMessageBytes, so a long ticket
// thread can't grow it past what the KV store accepts and lose the events
// that matter most. The server audit log always gets the full text.
const (
	historySoftLimitBytes = 2 << 20
	historyMessageBytes   = 2048
)

func boundedEvent(ev auditEvent, historyBytes int) auditEvent {
	msg, ok := ev.Detail["message"].(string)
	if historyBytes < historySoftLimitBytes || !ok || len(msg) <= historyMessageBytes {
		return ev
	}
	detail := maps.Clone(ev.Detail)
	detail["message"] = strings.ToValidUTF8(msg[:historyMessageBytes], "")
	detail["message_truncated"] = true
	ev.Detail = detail
	return ev
}

// logAuditRecord sends ev to the server audit log. Servers without audit
// logging turned on drop it; the request history doesn't depend on it.
func (p *Plugin) logAuditRecord(e auditEntry, ev auditEvent) {
	rec := plugin.MakeAuditRecord("requesthub_"+ev.Event, ev.Outcome)
	rec.Actor.UserId = ev.ActorID
	rec.AddEventObjectType("requesthub_request")
	rec.AddMeta("request_id", e.RequestID)
	rec.AddMeta("kind", e.Kind)
	rec.AddMeta("requester_id", e.RequesterID)
	if e.Exercise {
		rec.AddMeta("exercise", true)
	}
	rec.EventData.Parameters = jsonObject(ev.Detail)
	if ev.Note != "" {
		rec.EventData.Parameters["note"] = ev.Note
	}
	if ev.ActorName != "" {
		rec.EventData.Parameters["actor_name"] = ev.ActorName
	}
	if e.Snapshot != nil {
		rec.EventData.ResultState = jsonObject(e.Snapshot)
	}
	if ev.Outcome == model.AuditStatusFail {
		rec.AddErrorDesc(ev.Note)
	}
	p.API.LogAuditRec(rec)
}

// jsonObject turns v into a plain JSON object, the only kind of value an
// audit record survives the RPC round trip with.
func jsonObject(v any) map[string]any {
	out := map[string]any{}
	if v == nil {
		return out
	}
	data, err := json.Marshal(v)
	if err != nil {
		return out
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{"value": string(data)}
	}
	if out == nil { // v was a nil map or pointer: JSON null
		return map[string]any{}
	}
	return out
}

// decisionEvent records an event about the request a decision is acting on.
func (p *Plugin) decisionEvent(c decisionCards, event, note string, detail map[string]any) {
	e := auditEntry{
		Event:        event,
		Kind:         c.subject.kind,
		RequestID:    c.requestID,
		RequesterID:  c.subject.requesterID,
		Note:         note,
		Detail:       detail,
		Snapshot:     c.subject.snapshot,
		Exercise:     c.subject.exercise,
		ThreadPostID: c.cardPostID,
	}
	if c.subject.actor != nil {
		e.ActorID = c.subject.actor.Id
		e.ActorName = c.subject.actor.Username
	}
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	e.Detail["via"] = decisionVia(c.clickedPostID)
	p.recordEvent(e)
}

// approveFailed records an approval that couldn't be carried out and left
// the request pending. what says who approved what and what went wrong;
// err's text is added.
func (p *Plugin) approveFailed(c decisionCards, what string, err error) {
	p.decisionEvent(c, eventApproveFailed, fmt.Sprintf("%s: %s. The request is still pending.", what, escapeMarkdown(strings.TrimRight(err.Error(), "."))),
		map[string]any{"error": err.Error()})
}

// closeAfterTeardown closes an exercise request approved after teardown
// removed it, recording that.
func (p *Plugin) closeAfterTeardown(c decisionCards, requesterID, dmRootPostID, dmChannelID string) decisionResult {
	p.decisionEvent(c, eventClosedAfterTeardown, "An exercise request was approved after exercise teardown removed it, so what it made was removed and the request closed", nil)
	return p.closeRemovedExerciseRequest(c, requesterID, dmRootPostID, dmChannelID)
}

func decisionVia(clickedPostID string) string {
	if clickedPostID == "" {
		return "slash_command"
	}
	return "card"
}

// auditSubject is the request a decision is acting on, for its audit
// events.
type auditSubject struct {
	kind        string
	requesterID string
	actor       *model.User
	snapshot    any
	exercise    bool
}

// withSubject returns c carrying the request for its audit events.
func (c decisionCards) withSubject(kind, requesterID string, actor *model.User, snapshot any, exercise bool) decisionCards {
	c.subject = auditSubject{kind: kind, requesterID: requesterID, actor: actor, snapshot: snapshot, exercise: exercise}
	return c
}

// auditedDecider records a refusal for every decision d answers without
// recording an event itself (no permission, own request, already handled,
// a failed re-check, a lost race…), so every Approve or Deny leaves a trace.
func (p *Plugin) auditedDecider(kind string, d decider) decider {
	return func(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
		res := d(actingUserID, requestID, clickedPostID, approve)
		if res.audited {
			return res
		}
		action := "deny"
		if approve {
			action = "approve"
		}
		reason := res.replyText()
		e := auditEntry{
			Event:        eventDecisionRefused,
			Kind:         kind,
			RequestID:    requestID,
			ActorID:      actingUserID,
			Detail:       map[string]any{"action": action, "reason": reason, "via": decisionVia(clickedPostID)},
			ThreadPostID: clickedPostID,
		}
		e.Note = fmt.Sprintf("User %s could not %s %s request: %s", actingUserID, action, aKind(kind), reason)
		// Only the "all" level posts refusals; name the user on that card.
		// The history keeps the ID, whatever the setting.
		if p.getConfiguration().AuditChannelLevel() >= auditLevelAll {
			if user, appErr := p.API.GetUser(actingUserID); appErr == nil && user != nil {
				e.CardText = fmt.Sprintf("@%s could not %s %s request: %s", user.Username, action, aKind(kind), reason)
			}
		}
		p.recordEvent(e)
		return res
	}
}

// aKind is kindLabel with its article: "a bot account", "an audit export".
func aKind(kind string) string {
	label := kindLabel(kind)
	if label != "" && strings.ContainsRune("aeiouAEIOU", rune(label[0])) {
		return "an " + label
	}
	return "a " + label
}

// kindLabel names a pending kind in audit lines.
func kindLabel(kind string) string {
	switch kind {
	case pendingKindChannel:
		return "channel"
	case pendingKindAdmin:
		return "Channel Admin"
	case pendingKindTeam:
		return "team creation"
	case pendingKindTeamAdmin:
		return "Team Admin"
	case pendingKindTeamMember:
		return "team member"
	case pendingKindTeamGuest:
		return "guest invite"
	case pendingKindBot:
		return "bot account"
	case pendingKindWebhook:
		return "webhook"
	case pendingKindAuditExport:
		return "audit export"
	default:
		return kind
	}
}

// submitAudit follows one submission so that whichever way it ends, it is
// recorded: submitted, failed after it was stored (and rolled back), or
// refused before anything was stored. Use it with a deferred finish.
type submitAudit struct {
	p       *Plugin
	kind    string
	actorID string
	// req is the request once it is built; stored is set once it is saved.
	req    storedRequest
	stored bool
	// storedAt is when the request was saved: the submission's time, even
	// though it is recorded later, once it has been posted.
	storedAt int64
	// actorName is the submitter's username once it is loaded.
	actorName string
	// detail and noteSuffix are added to the submitted event.
	detail     map[string]any
	noteSuffix string
}

func (p *Plugin) startSubmitAudit(kind, actorID string) *submitAudit {
	return &submitAudit{p: p, kind: kind, actorID: actorID}
}

// built notes the request once it is built, and who submitted it.
func (s *submitAudit) built(req storedRequest, requester *model.User) {
	s.req = req
	if requester != nil {
		s.actorName = requester.Username
	}
}

// addDetail adds particulars to the submitted event, and a short phrase to
// its note.
func (s *submitAudit) addDetail(detail map[string]any, note string) {
	if s.detail == nil {
		s.detail = map[string]any{}
	}
	maps.Copy(s.detail, detail)
	s.noteSuffix = note
}

// markStored notes that the request was saved.
func (s *submitAudit) markStored() {
	s.stored = true
	s.storedAt = model.GetMillis()
}

// done is deferred by each submit function with a pointer to its error. A
// panic is recorded as a failed submission, then carries on.
func (s *submitAudit) done(err *error) {
	if r := recover(); r != nil {
		s.finish(fmt.Errorf("the submission crashed: %v", r))
		panic(r)
	}
	s.finish(*err)
}

// finish records how the submission ended. err is the submit's error.
func (s *submitAudit) finish(err error) {
	e := auditEntry{Kind: s.kind, ActorID: s.actorID, ActorName: s.actorName, RequesterID: s.actorID, At: s.storedAt}
	approvalPostID := ""
	if s.req != nil {
		id, requesterID, _ := s.req.pendingFields()
		e.RequestID = id
		e.RequesterID = requesterID
		e.Snapshot = s.req
		e.Exercise, approvalPostID = requestMarkers(s.req)
	}
	label := aKind(s.kind)
	who := userRef(s.actorID, s.actorName)
	switch {
	case err == nil:
		e.Event = eventSubmitted
		e.Note = fmt.Sprintf("%s submitted %s request", who, label)
		if s.noteSuffix != "" {
			e.Note += " and " + s.noteSuffix
		}
		e.Detail = s.detail
		e.ThreadPostID = approvalPostID
	case s.stored:
		e.Event = eventSubmitFailed
		e.Note = fmt.Sprintf("%s request from %s could not be sent to the approvers and was withdrawn: %s", capitalize(label), who, escapeMarkdown(err.Error()))
		e.At = 0 // when it was withdrawn
		e.Detail = map[string]any{"error": err.Error()}
	default:
		e.Event = eventSubmitRefused
		e.Note = fmt.Sprintf("%s request from %s was refused: %s", capitalize(label), who, escapeMarkdown(err.Error()))
		e.Detail = map[string]any{"reason": err.Error()}
		// Nothing was stored, so there is no request to keep a copy of.
		e.Snapshot = nil
		e.RequestID = ""
	}
	s.p.recordEvent(e)
}

// userRef names a user for an audit line, falling back to the ID when the
// name isn't known.
func userRef(userID, username string) string {
	if username != "" {
		return "@" + username
	}
	return "user " + userID
}

// requestMarkers reads the exercise flag and approval card every request
// record carries under the same JSON names.
func requestMarkers(r storedRequest) (exercise bool, approvalPostID string) {
	if r == nil {
		return false, ""
	}
	var m struct {
		Exercise       bool   `json:"exercise"`
		ApprovalPostID string `json:"approval_post_id"`
	}
	data, err := json.Marshal(r)
	if err != nil {
		return false, ""
	}
	_ = json.Unmarshal(data, &m)
	return m.Exercise, m.ApprovalPostID
}

// loadHistory returns a request's history, nil when there is none.
func (p *Plugin) loadHistory(requestID string) (*requestHistory, error) {
	raw, appErr := p.API.KVGet(kvHistoryPrefix + requestID)
	if appErr != nil {
		return nil, appErr
	}
	if raw == nil {
		return nil, nil
	}
	var h requestHistory
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("failed to read request history: %w", err)
	}
	return &h, nil
}

// listHistories returns every request history, paging through the KV store.
func (p *Plugin) listHistories() ([]requestHistory, error) {
	var out []requestHistory
	for page := 0; ; page++ {
		keys, appErr := p.API.KVList(page, historyListPageSize)
		if appErr != nil {
			return nil, appErr
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, kvHistoryPrefix) {
				continue
			}
			h, err := p.loadHistory(strings.TrimPrefix(key, kvHistoryPrefix))
			if err != nil {
				p.API.LogWarn("skipping unreadable request history", "key", key, "error", err.Error())
				continue
			}
			if h != nil {
				out = append(out, *h)
			}
		}
		if len(keys) < historyListPageSize {
			return out, nil
		}
	}
}

// historyMatchesExportFilter reports whether h belongs in a histories export
// for filter f (request/kind/requester/time window on the history record).
func historyMatchesExportFilter(h requestHistory, f auditExportFilter) bool {
	if f.RequestID != "" && h.RequestID != f.RequestID {
		return false
	}
	if f.Kind != "" && h.Kind != f.Kind {
		return false
	}
	if f.Requester != "" && h.RequesterID != f.Requester {
		return false
	}
	if f.Since > 0 && h.UpdatedAt < f.Since {
		return false
	}
	if f.Until > 0 && h.CreatedAt > f.Until {
		return false
	}
	return true
}

// listHistoriesForExport pages history keys and keeps at most
// historyExportMaxHistories newest matches (by UpdatedAt, then CreatedAt) in
// a bounded min-heap so peak memory stays near the cap, not the full match
// set. truncated is true when more matches existed than the cap.
func (p *Plugin) listHistoriesForExport(f auditExportFilter) (filtered []requestHistory, truncated bool, err error) {
	capN := historyExportMaxHistories
	kept := &historyExportHeap{}
	heap.Init(kept)
	for page := 0; ; page++ {
		keys, appErr := p.API.KVList(page, historyListPageSize)
		if appErr != nil {
			return nil, false, appErr
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, kvHistoryPrefix) {
				continue
			}
			h, loadErr := p.loadHistory(strings.TrimPrefix(key, kvHistoryPrefix))
			if loadErr != nil {
				p.API.LogWarn("skipping unreadable request history", "key", key, "error", loadErr.Error())
				continue
			}
			if h == nil || !historyMatchesExportFilter(*h, f) {
				continue
			}
			if capN <= 0 {
				truncated = true
				continue
			}
			if kept.Len() < capN {
				heap.Push(kept, *h)
				continue
			}
			oldest := (*kept)[0]
			if historyExportNewer(*h, oldest) {
				heap.Pop(kept)
				heap.Push(kept, *h)
			}
			truncated = true
		}
		if len(keys) < historyListPageSize {
			break
		}
	}
	out := make([]requestHistory, kept.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(kept).(requestHistory)
	}
	return out, truncated, nil
}

// historyExportHeap is a min-heap of requestHistory by UpdatedAt, then
// CreatedAt — oldest at index 0 so the export can drop it when a newer
// match arrives past the cap.
type historyExportHeap []requestHistory

func (h historyExportHeap) Len() int { return len(h) }
func (h historyExportHeap) Less(i, j int) bool {
	if h[i].UpdatedAt != h[j].UpdatedAt {
		return h[i].UpdatedAt < h[j].UpdatedAt
	}
	return h[i].CreatedAt < h[j].CreatedAt
}
func (h historyExportHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *historyExportHeap) Push(x any) {
	*h = append(*h, x.(requestHistory))
}

func (h *historyExportHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func historyExportNewer(a, b requestHistory) bool {
	if a.UpdatedAt != b.UpdatedAt {
		return a.UpdatedAt > b.UpdatedAt
	}
	return a.CreatedAt > b.CreatedAt
}
