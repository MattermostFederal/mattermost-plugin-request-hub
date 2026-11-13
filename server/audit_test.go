package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// captureAudit replaces the test plugin's audit sink with one that keeps
// every event, and returns them.
func captureAudit(p *Plugin) *[]auditEntry {
	var events []auditEntry
	p.auditSink = func(e auditEntry, _ auditEventSpec) { events = append(events, e) }
	return &events
}

func countEvents(events []auditEntry, event string) int {
	n := 0
	for _, e := range events {
		if e.Event == event {
			n++
		}
	}
	return n
}

func eventNames(events []auditEntry) []string {
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.Event)
	}
	return names
}

func storedHistory(t *testing.T, store map[string][]byte, requestID string) requestHistory {
	t.Helper()
	raw, ok := store[kvHistoryPrefix+requestID]
	require.True(t, ok, "no history for %s", requestID)
	var h requestHistory
	require.NoError(t, json.Unmarshal(raw, &h))
	return h
}

func TestKVHistoryPrefix_IsNoRequestPrefix(t *testing.T) {
	_, isRequest := pendingKindForKey(kvHistoryPrefix + "abc")
	require.False(t, isRequest)
}

func TestEveryAuditEventHasASpec(t *testing.T) {
	for _, event := range []string{
		eventSubmitted, eventSubmitFailed, eventSubmitRefused, eventStepApproved, eventApproved,
		eventApprovedPartial, eventDenied, eventExpired, eventApproveFailed, eventLost, eventUndelivered,
		eventClosedAfterTeardown, eventDecisionRefused, eventMessageRelayed, eventMessageDropped,
		eventTokenReminder, eventTokenTrackingEnded, eventTokenPostDeleted, eventExerciseSeeded,
		eventExerciseRemoved, eventMigrated, eventAuditExported,
	} {
		_, ok := auditEventSpecs[event]
		require.True(t, ok, event)
	}
}

func TestRecordEvent_AppendsHistoryAndMovesStatus(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	p.setConfiguration(&configuration{AuditChannelDetail: "decisions"})
	store := fakeKV(api)

	req := &botRequest{ID: "r1", RequesterID: "u_req", Username: "svcbot", Exercise: true}
	p.recordEvent(auditEntry{Event: eventSubmitted, Kind: pendingKindBot, RequestID: "r1", RequesterID: "u_req", ActorID: "u_req", ActorName: "req", Snapshot: req, Exercise: true, Note: "submitted"})
	p.recordEvent(auditEntry{Event: eventStepApproved, Kind: pendingKindBot, RequestID: "r1", ActorID: "sec1", Note: "step"})
	p.recordEvent(auditEntry{Event: eventApproved, Kind: pendingKindBot, RequestID: "r1", ActorID: "sys1", Detail: map[string]any{"bot_user_id": "b1"}, Note: "approved"})

	h := storedHistory(t, store, "r1")
	require.Equal(t, "r1", h.RequestID)
	require.Equal(t, pendingKindBot, h.Kind)
	require.Equal(t, "u_req", h.RequesterID)
	require.Equal(t, historyApproved, h.Status)
	require.True(t, h.Exercise)
	require.Contains(t, string(h.Request), `"username":"svcbot"`)
	require.Len(t, h.Events, 3)
	require.Equal(t, []string{eventSubmitted, eventStepApproved, eventApproved}, []string{h.Events[0].Event, h.Events[1].Event, h.Events[2].Event})
	require.Equal(t, "req", h.Events[0].ActorName)
	require.Equal(t, "b1", h.Events[2].Detail["bot_user_id"])
	require.Equal(t, model.AuditStatusSuccess, h.Events[2].Outcome)
	api.AssertNumberOfCalls(t, "LogAuditRec", 3)
}

func TestRecordEvent_FailedEventsAreMarkedFailed(t *testing.T) {
	api := &plugintest.API{}
	var rec *model.AuditRecord
	api.On("LogAuditRec", mock.Anything).Run(func(args mock.Arguments) { rec = args.Get(0).(*model.AuditRecord) }).Once()
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	store := fakeKV(api)

	p.recordEvent(auditEntry{Event: eventLost, Kind: pendingKindTeam, RequestID: "r2", RequesterID: "u_req", ActorID: "admin1", Note: "lost it"})

	h := storedHistory(t, store, "r2")
	require.Equal(t, historyLost, h.Status)
	require.Equal(t, model.AuditStatusFail, h.Events[0].Outcome)
	require.NotNil(t, rec)
	require.Equal(t, "requesthub_lost", rec.EventName)
	require.Equal(t, model.AuditStatusFail, rec.Status)
	require.Equal(t, "admin1", rec.Actor.UserId)
	require.Equal(t, "r2", rec.Meta["request_id"])
	require.Equal(t, pendingKindTeam, rec.Meta["kind"])
	require.Equal(t, "lost it", rec.Error.Description)
}

func TestRecordEvent_ServerRecordCarriesSnapshotAndDetail(t *testing.T) {
	api := &plugintest.API{}
	var rec *model.AuditRecord
	api.On("LogAuditRec", mock.Anything).Run(func(args mock.Arguments) { rec = args.Get(0).(*model.AuditRecord) }).Once()
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	fakeKV(api)

	p.recordEvent(auditEntry{
		Event: eventSubmitted, Kind: pendingKindChannel, RequestID: "r3", RequesterID: "u_req", ActorID: "u_req", ActorName: "req",
		Snapshot: &channelRequest{ID: "r3", Name: "team-x"}, Detail: map[string]any{"via": "card"}, Note: "hello",
	})

	require.NotNil(t, rec)
	require.Equal(t, model.AuditStatusSuccess, rec.Status)
	require.Equal(t, "team-x", rec.EventData.ResultState["name"])
	require.Equal(t, "card", rec.EventData.Parameters["via"])
	require.Equal(t, "hello", rec.EventData.Parameters["note"])
	require.Equal(t, "req", rec.EventData.Parameters["actor_name"])
}

func TestRecordEvent_RetriesALostCompareAndSet(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	existing := recordJSON(t, requestHistory{RequestID: "r4", Kind: pendingKindChannel, Status: historyPending, Events: []auditEvent{{Event: eventSubmitted}}})
	api.On("KVGet", kvHistoryPrefix+"r4").Return(existing, nil)
	api.On("KVCompareAndSet", kvHistoryPrefix+"r4", existing, mock.Anything).Return(false, nil).Once()
	var saved []byte
	api.On("KVCompareAndSet", kvHistoryPrefix+"r4", existing, mock.Anything).Run(func(args mock.Arguments) {
		saved = args.Get(2).([]byte)
	}).Return(true, nil).Once()

	p.recordEvent(auditEntry{Event: eventDenied, Kind: pendingKindChannel, RequestID: "r4", Note: "no"})

	var h requestHistory
	require.NoError(t, json.Unmarshal(saved, &h))
	require.Len(t, h.Events, 2)
	require.Equal(t, historyDenied, h.Status)
	api.AssertNumberOfCalls(t, "KVCompareAndSet", 2)
}

func TestRecordEvent_LogsTheEventWhenHistoryCantBeSaved(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	api.On("KVGet", kvHistoryPrefix+"r5").Return(nil, testAppErr("db down"))

	p.recordEvent(auditEntry{Event: eventApproved, Kind: pendingKindChannel, RequestID: "r5", Note: "approved it"})

	api.AssertCalled(t, "LogError", "failed to save request audit history", "request_id", "r5", "kind", pendingKindChannel,
		"event", mock.MatchedBy(func(ev string) bool { return strings.Contains(ev, "approved it") }), "error", mock.Anything)
	// The server audit log still gets it.
	api.AssertCalled(t, "LogAuditRec", mock.Anything)
}

func TestRecordEvent_NeverOverwritesAnUnreadableHistory(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	api.On("KVGet", kvHistoryPrefix+"r6").Return([]byte("{not json"), nil)

	p.recordEvent(auditEntry{Event: eventDenied, Kind: pendingKindChannel, RequestID: "r6", Note: "no"})

	api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
	api.AssertCalled(t, "LogError", "failed to save request audit history", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestRecordEvent_RefusalStartsNoHistory(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	store := fakeKV(api)

	p.recordEvent(auditEntry{Event: eventDecisionRefused, Kind: pendingKindChannel, RequestID: "nope", ActorID: "u1", Note: "refused"})
	p.recordEvent(auditEntry{Event: eventSubmitRefused, Kind: pendingKindChannel, ActorID: "u1", Note: "refused"})

	require.Empty(t, store)
	api.AssertNumberOfCalls(t, "LogAuditRec", 2)

	store[kvHistoryPrefix+"r7"] = recordJSON(t, requestHistory{RequestID: "r7", Status: historyPending})
	p.recordEvent(auditEntry{Event: eventDecisionRefused, Kind: pendingKindChannel, RequestID: "r7", ActorID: "u1", Note: "refused"})
	h := storedHistory(t, store, "r7")
	require.Len(t, h.Events, 1)
	require.Equal(t, historyPending, h.Status)
}

func TestRecordEvent_AuditChannelDetailLevels(t *testing.T) {
	events := []string{eventSubmitted, eventStepApproved, eventApproved, eventDecisionRefused, eventMessageRelayed, eventMigrated}
	for level, want := range map[string][]string{
		"decisions": {"✅ Approved"},
		"lifecycle": {"📨 Submitted", "☑️ Step approved", "✅ Approved"},
		"":          {"📨 Submitted", "☑️ Step approved", "✅ Approved"},
		"all":       {"📨 Submitted", "☑️ Step approved", "✅ Approved", "🚫 Refused", "💬 Message"},
	} {
		t.Run(level, func(t *testing.T) {
			api := &plugintest.API{}
			var titles []string
			api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
				titles = append(titles, extractFirstAttachment(args.Get(0).(*model.Post)).Title)
			}).Return(&model.Post{}, nil)
			stubLogLines(api)
			p := newTestPlugin(api)
			p.auditSink = nil
			p.setConfiguration(&configuration{ApprovalTeam: "appteam", AuditChannelDetail: level})
			fakeKV(api)
			mockAuditCards(api)

			for _, ev := range events {
				p.recordEvent(auditEntry{Event: ev, Kind: pendingKindChannel, RequestID: "r8", Note: "note for " + ev})
			}

			require.Equal(t, want, titles)
		})
	}
}

func TestAuditedDecider_RecordsWhatTheDecisionDidNotAudit(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	events := captureAudit(p)
	refused := p.auditedDecider(pendingKindBot, func(_, _, _ string, _ bool) decisionResult {
		return decisionResult{Reply: "You don't have permission to deny this request."}
	})
	recorded := p.auditedDecider(pendingKindBot, func(_, _, _ string, _ bool) decisionResult {
		return decisionResult{Outcome: "done", audited: true}
	})

	refused("u1", "r1", "card1", false)
	recorded("u1", "r1", "", true)

	require.Len(t, *events, 1)
	e := (*events)[0]
	require.Equal(t, eventDecisionRefused, e.Event)
	require.Equal(t, pendingKindBot, e.Kind)
	require.Equal(t, "r1", e.RequestID)
	require.Equal(t, "u1", e.ActorID)
	require.Equal(t, "deny", e.Detail["action"])
	require.Equal(t, "card", e.Detail["via"])
	require.Equal(t, "You don't have permission to deny this request.", e.Detail["reason"])
}

// Every Approve or Deny leaves an event, whichever kind and however it ends:
// here, an acting user who can't be loaded.
func TestDeciderFor_EveryKindAuditsEveryDecision(t *testing.T) {
	for _, kind := range allPendingKinds {
		for _, approve := range []bool{true, false} {
			api := &plugintest.API{}
			stubLogLines(api)
			p := newTestPlugin(api)
			events := captureAudit(p)
			api.On("GetUser", "u1").Return(nil, testAppErr("gone"))

			p.deciderFor(kind)("u1", "r1", "", approve)

			require.Equal(t, []string{eventDecisionRefused}, eventNames(*events), kind)
			require.Equal(t, kind, (*events)[0].Kind)
		}
	}
}

func TestExecuteDecision_ApproveLeavesTheFullHistory(t *testing.T) {
	api, p, store := decisionFixture(t, "card1")
	p.auditSink = nil
	mockChannelCreate(api)
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil)
	updatedPosts(api)
	api.On("GetChannelMembers", mock.Anything, mock.Anything, mock.Anything).Return(model.ChannelMembers{}, nil).Maybe()
	api.On("GetChannelSidebarCategories", mock.Anything, mock.Anything).Return(&model.OrderedSidebarCategories{}, nil).Maybe()

	runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

	h := storedHistory(t, store, testChannelRequestID)
	require.Equal(t, historyApproved, h.Status)
	require.Equal(t, pendingKindChannel, h.Kind)
	require.Equal(t, "u_req", h.RequesterID)
	require.Len(t, h.Events, 1, "one approval, and no refusal recorded on top of it")
	last := h.Events[len(h.Events)-1]
	require.Equal(t, eventApproved, last.Event)
	require.Equal(t, "admin1", last.ActorID)
	require.Equal(t, "slash_command", last.Detail["via"])
	require.Contains(t, string(h.Request), `"name":"team-x"`)
	// The request record is gone; its history isn't.
	require.NotContains(t, store, kvRequestPrefix+testChannelRequestID)
}

func TestExecuteDecision_RefusedApproveIsAudited(t *testing.T) {
	api, p, store := decisionFixture(t, "card1")
	p.auditSink = nil
	store[kvHistoryPrefix+testChannelRequestID] = recordJSON(t, requestHistory{RequestID: testChannelRequestID, Kind: pendingKindChannel, Status: historyPending})
	api.On("GetUser", "u_plain").Return(&model.User{Id: "u_plain", Username: "plain", Roles: model.SystemUserRoleId}, nil)
	api.On("GetTeamMember", "team1", "u_plain").Return(&model.TeamMember{TeamId: "team1", UserId: "u_plain", Roles: model.TeamUserRoleId}, nil).Maybe()

	resp := runCommandAs(p, "u_plain", "/mattermost-request-hub approve "+testChannelRequestID)

	require.Contains(t, resp.Text, "permission")
	h := storedHistory(t, store, testChannelRequestID)
	require.Equal(t, historyPending, h.Status)
	require.Len(t, h.Events, 1)
	require.Equal(t, eventDecisionRefused, h.Events[0].Event)
	require.Equal(t, "u_plain", h.Events[0].ActorID)
	require.Equal(t, resp.Text, h.Events[0].Detail["reason"])
}

func TestSubmitAudit_RecordsHowTheSubmissionEnded(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	events := captureAudit(p)
	req := &teamCreationRequest{ID: "r1", RequesterID: "u_req", Name: "ops", ApprovalPostID: "card1", Exercise: true}

	refused := p.startSubmitAudit(pendingKindTeam, "u_req")
	refused.req = req
	refused.finish(errors.New("that team name is taken"))

	failed := p.startSubmitAudit(pendingKindTeam, "u_req")
	failed.req, failed.stored, failed.actorName = req, true, "req"
	failed.finish(errors.New("could not post"))

	ok := p.startSubmitAudit(pendingKindTeam, "u_req")
	ok.req, ok.stored, ok.actorName = req, true, "req"
	ok.finish(nil)

	require.Equal(t, []string{eventSubmitRefused, eventSubmitFailed, eventSubmitted}, eventNames(*events))
	got := *events
	require.Empty(t, got[0].RequestID, "a refused submission stored no request")
	require.Nil(t, got[0].Snapshot)
	require.Equal(t, "that team name is taken", got[0].Detail["reason"])
	require.Contains(t, got[0].Note, "user u_req")
	require.Equal(t, "r1", got[1].RequestID)
	require.Equal(t, "could not post", got[1].Detail["error"])
	require.Equal(t, "r1", got[2].RequestID)
	require.Equal(t, req, got[2].Snapshot)
	require.True(t, got[2].Exercise)
	require.Equal(t, "card1", got[2].ThreadPostID)
	require.Equal(t, "@req submitted a team creation request", got[2].Note)
}

func TestAuditRelay_RecordsTheMessageText(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	events := captureAudit(p)
	anchor := threadAnchor{ApprovalPostID: "card1", Kind: requestTypeChannelAdmin, RequestID: "r1"}
	poster := &model.User{Id: "u1", Username: "alice"}

	p.auditRelay(anchor, &model.Post{Id: "p1", RootId: "dmroot", Message: "why? @channel"}, poster, "approvers_to_requester", nil, "")
	p.auditRelay(anchor, &model.Post{Id: "p2", RootId: "card1", Message: "hi"}, poster, "approvers_to_requester", nil, "not an approver")
	p.auditRelay(anchor, &model.Post{Id: "p3", RootId: "card1", Message: "hi"}, poster, "requester_to_approvers", testAppErr("down"), "")

	got := *events
	require.Equal(t, []string{eventMessageRelayed, eventMessageDropped, eventMessageDropped}, eventNames(got))
	require.Equal(t, pendingKindAdmin, got[0].Kind)
	require.Equal(t, "r1", got[0].RequestID)
	require.Equal(t, "why? @channel", got[0].Detail["message"])
	require.NotContains(t, got[0].Note, "why?", "the text is kept once, in Detail")
	require.Contains(t, got[0].CardText, "> why? @\u200bchannel", "the card quotes it, with mentions defused")
	require.Equal(t, "p1", got[0].Detail["post_id"])
	require.Equal(t, "not an approver", got[1].Detail["reason"])
	require.Contains(t, got[2].Detail["reason"], "could not be relayed")
}

func TestBotFinalApproval_AuditNeverHoldsTheToken(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubBotExtras(api)
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	events := captureAudit(p)
	stubSiteURL(api, ts.URL)
	finalBot(t, api, finalBotApproval(botRequest{RequestToken: true, DMChannelID: "dm1", DMRootPostID: "dm-root"}))
	api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
	api.On("RevokeSession", "sess1").Return(nil)
	api.On("KVGet", tokenPostRegistryKey).Return(nil, nil)
	api.On("KVSet", botTokenExpiryKey("tok-id"), mock.Anything).Return(nil)
	api.On("KVSet", kvBotIndexPrefix+"new-bot-id", mock.Anything).Return(nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "created"}, nil)

	runAsAdmin(t, p, botActionHandler, true)

	require.Equal(t, []string{eventApproved}, eventNames(*events))
	e := (*events)[0]
	require.Equal(t, true, e.Detail["token_created"])
	require.Equal(t, "new-bot-id", e.Detail["bot_user_id"])
	data, err := json.Marshal(e)
	require.NoError(t, err)
	require.NotContains(t, string(data), "secret-token")
}

func TestExecuteAudit_SystemAdminsOnly(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemUserRoleId}, nil)

	resp := runCommandAs(p, "u1", "/mattermost-request-hub audit recent")

	require.Contains(t, resp.Text, "Only System Admins")
}

func TestExecuteAudit_ShowsATimeline(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	store := fakeKV(api)
	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	id := model.NewId()
	store[kvHistoryPrefix+id] = recordJSON(t, requestHistory{
		RequestID: id, Kind: pendingKindWebhook, RequesterID: "u_req", Status: historyDenied, UpdatedAt: 2,
		Events: []auditEvent{
			{At: 1, Event: eventSubmitted, ActorID: "u_req", ActorName: "req", Outcome: model.AuditStatusSuccess, Note: "submitted it"},
			{At: 2, Event: eventDenied, ActorID: "admin1", ActorName: "admin", Outcome: model.AuditStatusSuccess, Note: "denied\nit"},
		},
	})

	withDirectExport(p)
	resp := runCommandAs(p, "admin1", "/mattermost-request-hub audit "+id)

	require.Contains(t, resp.Text, "Audit history for webhook request `"+id+"`")
	require.Contains(t, resp.Text, "Status: **denied**")
	require.Contains(t, resp.Text, "`submitted` · @req: submitted it")
	require.Contains(t, resp.Text, "`denied` · @admin: denied it")

	resp = runCommandAs(p, "admin1", "/mattermost-request-hub audit recent 5")
	require.Contains(t, resp.Text, "Latest 1 of 1 requests")
	require.Contains(t, resp.Text, "`"+id+"` · webhook · **denied**")
	require.Contains(t, resp.Text, routeAuditExport+"?format=csv")

	resp = runCommandAs(p, "admin1", "/mattermost-request-hub audit "+model.NewId())
	require.Equal(t, "No history is recorded for that request ID.", resp.Text)

	resp = runCommandAs(p, "admin1", "/mattermost-request-hub audit")
	require.Contains(t, resp.Text, "for a request's history")
}

func exportRequest(t *testing.T, p *Plugin, userID, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, routeAuditExport+query, nil)
	r.Header.Set(headerUserID, userID)
	w := httptest.NewRecorder()
	p.ServeHTTP(nil, w, r)
	return w
}

func TestHandleAuditExport(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	events := captureAudit(p)
	store := fakeKV(api)
	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemUserRoleId}, nil)
	store[kvHistoryPrefix+"r1"] = recordJSON(t, requestHistory{
		RequestID: "r1", Kind: pendingKindBot, RequesterID: "u_req", Status: historyApproved, CreatedAt: 10, UpdatedAt: 30,
		Events: []auditEvent{{At: 10, Event: eventSubmitted, Outcome: "success", Note: "=cmd()"}, {At: 30, Event: eventApproved, ActorID: "admin1", Outcome: "success", Note: "ok"}},
	})
	store[kvHistoryPrefix+"r2"] = recordJSON(t, requestHistory{
		RequestID: "r2", Kind: pendingKindTeam, RequesterID: "u_other", Status: historyPending, CreatedAt: 20, UpdatedAt: 20,
		Events: []auditEvent{{At: 20, Event: eventSubmitted, Outcome: "success"}},
	})
	store[kvRequestPrefix+"x"] = []byte(`{}`)

	t.Run("refused while direct export is off", func(t *testing.T) {
		w := exportRequest(t, p, "admin1", "")
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Contains(t, w.Body.String(), "Direct export is off. Request an export instead.")
	})
	withDirectExport(p)
	t.Run("refuses non-admins", func(t *testing.T) {
		require.Equal(t, http.StatusForbidden, exportRequest(t, p, "u1", "").Code)
	})
	t.Run("refuses an unknown type, and a format the type doesn't come in", func(t *testing.T) {
		require.Equal(t, http.StatusBadRequest, exportRequest(t, p, "admin1", "?type=nope").Code)
		require.Equal(t, http.StatusBadRequest, exportRequest(t, p, "admin1", "?type=full_histories&format=pdf").Code)
	})
	t.Run("refuses a bad format", func(t *testing.T) {
		require.Equal(t, http.StatusBadRequest, exportRequest(t, p, "admin1", "?format=xml").Code)
	})
	t.Run("json", func(t *testing.T) {
		w := exportRequest(t, p, "admin1", "")
		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Header().Get("Content-Disposition"), `attachment; filename="request-hub-full-histories-`)
		var got []requestHistory
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Len(t, got, 2)
		require.Equal(t, "r1", got[0].RequestID)
	})
	t.Run("csv filtered by kind", func(t *testing.T) {
		w := exportRequest(t, p, "admin1", "?format=csv&kind=bot")
		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Header().Get("Content-Type"), "text/csv")
		rows, err := csv.NewReader(w.Body).ReadAll()
		require.NoError(t, err)
		require.Len(t, rows, 3)
		require.Equal(t, "request_id", rows[0][0])
		require.Equal(t, []string{"r1", "bot", "u_req", "approved"}, rows[1][:4])
		require.Equal(t, "'=cmd()", rows[1][9], "formulas are neutralised")
		require.Equal(t, "admin1", rows[2][7])
	})
	t.Run("filtered by requester", func(t *testing.T) {
		w := exportRequest(t, p, "admin1", "?requester=u_other")
		var got []requestHistory
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Len(t, got, 1)
		require.Equal(t, "r2", got[0].RequestID)
	})
	t.Run("filtered by time", func(t *testing.T) {
		ids := func(query string) []string {
			var got []requestHistory
			require.NoError(t, json.Unmarshal(exportRequest(t, p, "admin1", query).Body.Bytes(), &got))
			var out []string
			for _, h := range got {
				out = append(out, h.RequestID)
			}
			return out
		}
		// r1 has events at 10 and 30, r2 at 20. A history needs an event in
		// the window: r1 spans 15–25 but has nothing inside it.
		require.Equal(t, []string{"r1"}, ids("?since=25"))
		require.Equal(t, []string{"r1"}, ids("?until=15"))
		require.Equal(t, []string{"r2"}, ids("?since=15&until=25"))
		// CSV rows are each event in the range.
		rows, err := csv.NewReader(exportRequest(t, p, "admin1", "?format=csv&kind=bot&since=25").Body).ReadAll()
		require.NoError(t, err)
		require.Len(t, rows, 2)
		require.Equal(t, eventApproved, rows[1][5])
		require.Equal(t, http.StatusBadRequest, exportRequest(t, p, "admin1", "?since=yesterday").Code)
	})

	exported := 0
	for _, e := range *events {
		if e.Event == eventAuditExported {
			exported++
			require.Equal(t, "admin1", e.ActorID)
		}
	}
	require.Equal(t, 7, exported)
}

func TestAuditChannelLevel(t *testing.T) {
	require.Equal(t, auditLevelDecisions, (&configuration{AuditChannelDetail: "decisions"}).AuditChannelLevel())
	require.Equal(t, auditLevelLifecycle, (&configuration{AuditChannelDetail: "lifecycle"}).AuditChannelLevel())
	require.Equal(t, auditLevelLifecycle, (&configuration{}).AuditChannelLevel())
	require.Equal(t, auditLevelLifecycle, (&configuration{AuditChannelDetail: "bogus"}).AuditChannelLevel())
	require.Equal(t, auditLevelAll, (&configuration{AuditChannelDetail: " all "}).AuditChannelLevel())
}

// A submission is recorded once it is posted, which can be after an
// approver decided it: the history keeps the decision.
func TestRecordEvent_LateSubmissionNeverReopensADecision(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	store := fakeKV(api)

	decided := &channelRequest{ID: "r1", Name: "team-x", ApprovalPostID: "card1"}
	p.recordEvent(auditEntry{Event: eventApproved, Kind: pendingKindChannel, RequestID: "r1", At: 200, Snapshot: decided, Note: "approved"})
	p.recordEvent(auditEntry{Event: eventSubmitted, Kind: pendingKindChannel, RequestID: "r1", At: 100, Snapshot: &channelRequest{ID: "r1", Name: "team-x"}, Note: "submitted"})

	h := storedHistory(t, store, "r1")
	require.Equal(t, historyApproved, h.Status)
	require.Equal(t, []string{eventSubmitted, eventApproved}, []string{h.Events[0].Event, h.Events[1].Event}, "events read in the order they happened")
	require.Contains(t, string(h.Request), `"approval_post_id":"card1"`, "the decided copy is kept")
	require.Equal(t, int64(100), h.CreatedAt)
	require.Equal(t, int64(200), h.UpdatedAt)
}

func TestSubmitAudit_PanicIsRecordedAsAFailure(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	events := captureAudit(p)
	submit := func() (submitErr error) {
		sa := p.startSubmitAudit(pendingKindBot, "u_req")
		defer sa.done(&submitErr)
		panic("boom")
	}

	require.PanicsWithValue(t, "boom", func() { _ = submit() })

	require.Equal(t, []string{eventSubmitRefused}, eventNames(*events))
	require.Contains(t, (*events)[0].Detail["reason"], "crashed: boom")
}

func TestSubmitAudit_RecordsTheSubmissionTime(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	events := captureAudit(p)
	sa := p.startSubmitAudit(pendingKindTeam, "u_req")
	sa.built(&teamCreationRequest{ID: "r1", RequesterID: "u_req"}, &model.User{Username: "req"})
	sa.markStored()
	storedAt := sa.storedAt

	sa.finish(nil)

	require.Equal(t, storedAt, (*events)[0].At)
	require.Equal(t, "req", (*events)[0].ActorName)
}

func TestBoundedEvent_CutsMessagesOnceAHistoryIsLarge(t *testing.T) {
	long := strings.Repeat("é", historyMessageBytes)
	ev := auditEvent{Event: eventMessageRelayed, Detail: map[string]any{"message": long}}

	require.Equal(t, long, boundedEvent(ev, historySoftLimitBytes-1).Detail["message"])

	cut := boundedEvent(ev, historySoftLimitBytes)
	msg := cut.Detail["message"].(string)
	require.LessOrEqual(t, len(msg), historyMessageBytes)
	require.True(t, utf8.ValidString(msg))
	require.Equal(t, true, cut.Detail["message_truncated"])
	require.Equal(t, long, ev.Detail["message"], "the event itself, sent to the server audit log, keeps it all")
}

func TestExecuteDecision_ExpiryIsRecordedAsThePlugins(t *testing.T) {
	api, p, store := decisionFixture(t, "")
	events := captureAudit(p)
	config := approvableConfig()
	config.expiryDays = 30
	p.setConfiguration(config)
	req := &channelRequest{ID: testChannelRequestID, RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", CreatedAt: expiredMillis(), DMRootPostID: "dmroot", DMChannelID: "dm1"}
	store[kvRequestPrefix+req.ID] = recordJSON(t, req)
	api.On("GetPost", "post1").Return(approvalCard("post1"), nil)
	api.On("GetPost", "post2").Return(approvalCard("post2"), nil)
	updatedPosts(api)

	runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

	require.Equal(t, []string{eventExpired}, eventNames(*events))
	e := (*events)[0]
	require.Empty(t, e.ActorID, "expiry is the plugin's doing")
	require.Equal(t, "admin1", e.Detail["noticed_by_id"])
	require.Equal(t, 30, e.Detail["expiry_days"])
}

func TestDeleteTokenPost_RecordsOnlyADeletedPost(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	events := captureAudit(p)
	api.On("DeletePost", "p1").Return(nil)
	api.On("DeletePost", "p2").Return(testAppErr("gone"))

	p.deleteTokenPost(tokenPostEntry{PostID: "p1", RequestID: "r1"}, "failed")
	p.deleteTokenPost(tokenPostEntry{PostID: "p2", RequestID: "r1"}, "failed")

	require.Equal(t, []string{eventTokenPostDeleted}, eventNames(*events))
	require.Equal(t, "r1", (*events)[0].RequestID)
	require.Equal(t, "p1", (*events)[0].Detail["post_id"])
}
