package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// auditExportPlugin is a plugin in usergroup mode, where u_sec is in the
// security approver group and u_plain isn't, backed by fakeKV with the real
// request history.
func auditExportPlugin(t *testing.T) (*Plugin, *plugintest.API, map[string][]byte) {
	t.Helper()
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	p.setConfiguration(&configuration{
		AllowChannelRequests:  true,
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
		AuditChannelDetail:    "decisions",
	})
	store := fakeKV(api)
	stubSyncedGroup(api, "security-approvers", "g-sec")
	api.On("GetGroupsForUser", "u_sec").Return([]*model.Group{{Id: "g-sec"}}, nil).Maybe()
	api.On("GetGroupsForUser", mock.Anything).Return([]*model.Group{}, nil).Maybe()
	api.On("GetUser", "u_sec").Return(&model.User{Id: "u_sec", Username: "sec", Roles: model.SystemUserRoleId}, nil).Maybe()
	api.On("GetUser", "u_plain").Return(&model.User{Id: "u_plain", Username: "plain", Roles: model.SystemUserRoleId}, nil).Maybe()
	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil).Maybe()
	return p, api, store
}

func TestAuditAccess(t *testing.T) {
	p, _, _ := auditExportPlugin(t)

	view, request := p.auditAccess(sysadmin("admin1"))
	require.True(t, view)
	require.False(t, request, "System Admins download it themselves")

	view, request = p.auditAccess(&model.User{Id: "u_sec", Roles: model.SystemUserRoleId})
	require.False(t, view)
	require.True(t, request)

	view, request = p.auditAccess(&model.User{Id: "u_plain", Roles: model.SystemUserRoleId})
	require.False(t, view)
	require.False(t, request)

	view, request = p.auditAccess(&model.User{Id: "u_sec", Roles: model.SystemUserRoleId, DeleteAt: 1})
	require.False(t, view || request)
	view, request = p.auditAccess(nil)
	require.False(t, view || request)
}

func TestSubmitAuditExportRequest_Refusals(t *testing.T) {
	p, _, store := auditExportPlugin(t)
	events := captureAudit(p)

	for name, tc := range map[string]struct {
		user   string
		filter auditExportFilter
		want   string
	}{
		"a System Admin":        {"admin1", auditExportFilter{Format: "csv"}, "System Admins can download the audit history directly"},
		"not a security member": {"u_plain", auditExportFilter{Format: "csv"}, auditExportNotApproverText},
		"bad format":            {"u_sec", auditExportFilter{Format: "xml"}, "pick CSV or JSON"},
		"unknown kind":          {"u_sec", auditExportFilter{Format: "csv", Kind: "nope"}, "that request type"},
		"dates backwards":       {"u_sec", auditExportFilter{Format: "csv", Since: 20, Until: 10}, "the start date must be before the end date"},
		"bad requester":         {"u_sec", auditExportFilter{Format: "csv", Requester: "x"}, "the requester isn't valid"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.submitAuditExportRequest(tc.user, tc.filter, "")
			require.ErrorContains(t, err, tc.want)
		})
	}
	for key := range store {
		require.False(t, strings.HasPrefix(key, kvAuditExportRequestPrefix), "nothing is stored")
	}
	require.Equal(t, 6, countEvents(*events, eventSubmitRefused))
}

// storedExportRequest stores a pending audit export request from u_sec,
// with its ticket thread.
func storedExportRequest(t *testing.T, store map[string][]byte, f auditExportFilter) *auditExportRequest {
	t.Helper()
	req := &auditExportRequest{
		ID: model.NewId(), RequesterID: "u_sec", Filter: f, Reason: "quarterly review", CreatedAt: model.GetMillis(),
		ApprovalPostID: "card1", ApprovalChannelID: "auditch", DMRootPostID: "dmroot", DMChannelID: "dm1",
	}
	store[kvAuditExportRequestPrefix+req.ID] = recordJSON(t, req)
	store[pendingListKey(pendingKindAuditExport, "u_sec")] = recordJSON(t, []pendingEntry{newPendingEntry(req)})
	return req
}

func TestDecideAuditExport_ApproveSendsTheFile(t *testing.T) {
	p, api, store := auditExportPlugin(t)
	store[kvHistoryPrefix+"r1"] = recordJSON(t, requestHistory{
		RequestID: "r1", Kind: pendingKindBot, RequesterID: "u_req", Status: historyApproved,
		Events: []auditEvent{{At: 1, Event: eventMessageRelayed, Outcome: "success", Note: "wrote", Detail: map[string]any{"message": "hello", "direction": "x"}}},
	})
	req := storedExportRequest(t, store, auditExportFilter{Format: "csv", Kind: pendingKindBot})
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil).Maybe()
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
	updatedPosts(api)
	var uploaded []byte
	api.On("UploadFile", mock.Anything, "dm1", mock.MatchedBy(func(name string) bool { return strings.HasSuffix(name, ".csv") })).
		Run(func(args mock.Arguments) { uploaded = args.Get(0).([]byte) }).Return(&model.FileInfo{Id: "file1"}, nil)
	var delivered *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		if post := args.Get(0).(*model.Post); len(post.FileIds) > 0 {
			delivered = post
		}
	}).Return(&model.Post{Id: "p"}, nil)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+req.ID)

	require.Contains(t, resp.Text, "✅ Approved by @admin for @sec. Audit export (CSV, 1 requests) sent.")
	require.NotNil(t, delivered)
	require.Equal(t, "dmroot", delivered.RootId, "the file goes in the requester's ticket thread")
	require.Equal(t, model.StringArray{"file1"}, delivered.FileIds)
	rows, err := csv.NewReader(bytes.NewReader(uploaded)).ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "hello", rows[1][10], "the message text is in the file")
	require.NotContains(t, store, kvAuditExportRequestPrefix+req.ID)

	h := storedHistory(t, store, req.ID)
	require.Equal(t, historyApproved, h.Status)
	last := h.Events[len(h.Events)-1]
	require.Equal(t, eventApproved, last.Event)
	require.Equal(t, "admin1", last.ActorID)
	require.Equal(t, "file1", last.Detail["file_id"])
	require.Equal(t, pendingKindBot, last.Detail["kind"])
}

func TestDecideAuditExport_FailedDeliveryKeepsTheRequest(t *testing.T) {
	p, api, store := auditExportPlugin(t)
	req := storedExportRequest(t, store, auditExportFilter{Format: "json"})
	api.On("UploadFile", mock.Anything, "dm1", mock.Anything).Return(nil, testAppErr("files are off"))

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+req.ID)

	require.Contains(t, resp.Text, "Could not send the export")
	require.Contains(t, resp.Text, "still pending")
	require.Contains(t, store, kvAuditExportRequestPrefix+req.ID)
	h := storedHistory(t, store, req.ID)
	require.Equal(t, eventApproveFailed, h.Events[len(h.Events)-1].Event)
}

func TestDecideAuditExport_Refusals(t *testing.T) {
	t.Run("only System Admins decide", func(t *testing.T) {
		p, _, store := auditExportPlugin(t)
		req := storedExportRequest(t, store, auditExportFilter{Format: "csv"})
		resp := runCommandAs(p, "u_sec", "/mattermost-request-hub approve "+req.ID)
		require.Equal(t, "Only System Admins can approve or deny audit export requests.", resp.Text)
		require.Contains(t, store, kvAuditExportRequestPrefix+req.ID)
	})
	t.Run("a requester who left the security pool", func(t *testing.T) {
		p, api, store := auditExportPlugin(t)
		req := storedExportRequest(t, store, auditExportFilter{Format: "csv"})
		req.RequesterID = "u_plain"
		store[kvAuditExportRequestPrefix+req.ID] = recordJSON(t, req)
		api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
		resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+req.ID)
		require.Equal(t, "The requester is no longer a security approver. Deny this request instead.", resp.Text)
		api.AssertNotCalled(t, "UploadFile", mock.Anything, mock.Anything, mock.Anything)
	})
}

func TestDecideAuditExport_Deny(t *testing.T) {
	p, api, store := auditExportPlugin(t)
	req := storedExportRequest(t, store, auditExportFilter{Format: "csv"})
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil).Maybe()
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
	updatedPosts(api)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p"}, nil)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+req.ID)

	require.Equal(t, "❌ Denied by @admin for @sec. Audit export (CSV).", resp.Text)
	api.AssertNotCalled(t, "UploadFile", mock.Anything, mock.Anything, mock.Anything)
	require.Equal(t, historyDenied, storedHistory(t, store, req.ID).Status)
}

func TestExecuteAudit_SecurityApproverRequestsTheExport(t *testing.T) {
	p, api, store := auditExportPlugin(t)
	p.setConfiguration(&configuration{
		AllowChannelRequests: true, TwoStepApproverMode: twoStepModeUserGroup, SecurityApproverGroup: "security-approvers",
		ApprovalTeam: "appteam", AuditChannelDetail: "decisions",
	})
	mockAuditCards(api)
	api.On("GetUsers", mock.Anything).Return([]*model.User{sysadmin("admin1")}, nil)
	api.On("GetDirectChannel", "u_sec", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: model.NewId()}, nil)

	resp := runCommandAs(p, "u_sec", "/mattermost-request-hub audit export json")

	require.Contains(t, resp.Text, "has been sent to the System Admins")
	var stored *auditExportRequest
	for key, raw := range store {
		if strings.HasPrefix(key, kvAuditExportRequestPrefix) {
			stored = &auditExportRequest{}
			require.NoError(t, json.Unmarshal(raw, stored))
		}
	}
	require.NotNil(t, stored)
	require.Equal(t, "json", stored.Filter.Format)
	require.Equal(t, "u_sec", stored.RequesterID)

	// Everyone else outside the pool is still refused.
	require.Contains(t, runCommandAs(p, "u_plain", "/mattermost-request-hub audit export").Text, "Only System Admins")
	require.Contains(t, runCommandAs(p, "u_sec", "/mattermost-request-hub audit recent").Text, "Only System Admins")
}

func TestExecuteAudit_ExportLinksForAdmins(t *testing.T) {
	p, _, store := auditExportPlugin(t)
	id := model.NewId()
	store[kvHistoryPrefix+id] = recordJSON(t, requestHistory{RequestID: id, Kind: pendingKindTeam, Status: historyPending, Events: []auditEvent{{At: 1, Event: eventSubmitted}}})

	text := runCommandAs(p, "admin1", "/mattermost-request-hub audit export").Text
	require.Contains(t, text, "Download the whole audit history")
	require.Contains(t, text, routeAuditExport+"?format=csv")
	require.Contains(t, text, routeAuditExport+"?format=json")

	text = runCommandAs(p, "admin1", "/mattermost-request-hub audit export json").Text
	require.Contains(t, text, "[JSON]("+"/plugins/"+manifest.Id+routeAuditExport+"?format=json)")
	require.NotContains(t, text, "format=csv")

	require.Contains(t, runCommandAs(p, "admin1", "/mattermost-request-hub audit export xml").Text, "to download the whole audit history")

	text = runCommandAs(p, "admin1", "/mattermost-request-hub audit "+id).Text
	require.Contains(t, text, "This request in full: [CSV]("+"/plugins/"+manifest.Id+routeAuditExport+"?request="+id+"&format=csv)")
}

func getAs(t *testing.T, p *Plugin, userID, route string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, route, nil)
	r.Header.Set(headerUserID, userID)
	w := httptest.NewRecorder()
	p.ServeHTTP(nil, w, r)
	return w
}

func TestHandleAuditAccess(t *testing.T) {
	p, _, _ := auditExportPlugin(t)
	for user, want := range map[string]string{
		"admin1":  `{"can_request":false,"can_view":true}`,
		"u_sec":   `{"can_request":true,"can_view":false}`,
		"u_plain": `{"can_request":false,"can_view":false}`,
	} {
		require.JSONEq(t, want, getAs(t, p, user, routeAuditAccess).Body.String(), user)
	}
}

func TestHandleAuditRequestsAndRequest(t *testing.T) {
	p, api, store := auditExportPlugin(t)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "alice"}, nil).Maybe()
	ids := []string{model.NewId(), model.NewId(), model.NewId()}
	store[kvHistoryPrefix+ids[0]] = recordJSON(t, requestHistory{
		RequestID: ids[0], Kind: pendingKindBot, RequesterID: "u_req", Status: historyApproved, UpdatedAt: 30,
		Request: json.RawMessage(`{"username":"svcbot","display_name":"Service Bot"}`), Events: []auditEvent{{At: 30, Event: eventApproved}},
	})
	store[kvHistoryPrefix+ids[1]] = recordJSON(t, requestHistory{
		RequestID: ids[1], Kind: pendingKindChannel, RequesterID: "u_other", Status: historyPending, UpdatedAt: 20,
		Request: json.RawMessage(`{"display_name":"Ops Room","name":"team-ops"}`), Events: []auditEvent{{At: 20, Event: eventSubmitted}},
	})
	store[kvHistoryPrefix+ids[2]] = recordJSON(t, requestHistory{RequestID: ids[2], Kind: pendingKindChannel, RequesterID: "u_other", Status: historyDenied, UpdatedAt: 10})
	api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Username: "bob"}, nil).Maybe()

	list := func(query string) (int, []auditRequestSummary) {
		w := getAs(t, p, "admin1", routeAuditRequests+query)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body struct {
			Total    int                   `json:"total"`
			Requests []auditRequestSummary `json:"requests"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return body.Total, body.Requests
	}

	total, rows := list("")
	require.Equal(t, 3, total)
	require.Equal(t, ids[0], rows[0].RequestID, "newest activity first")
	require.Equal(t, "Service Bot", rows[0].Title)
	require.Equal(t, "alice", rows[0].RequesterUsername)
	require.Equal(t, "bot account", rows[0].KindLabel)
	require.Equal(t, "Ops Room", rows[1].Title)

	total, rows = list("?kind=channel&status=pending")
	require.Equal(t, 1, total)
	require.Equal(t, ids[1], rows[0].RequestID)

	total, _ = list("?q=ALICE")
	require.Equal(t, 1, total, "search matches the requester's username")
	total, _ = list("?q=ops")
	require.Equal(t, 1, total, "and the title")
	total, _ = list("?q=svcbot")
	require.Equal(t, 1, total, "and every other name, such as the bot username")

	total, rows = list("?per_page=2&page=1")
	require.Equal(t, 3, total)
	require.Len(t, rows, 1)
	require.Equal(t, http.StatusBadRequest, getAs(t, p, "admin1", routeAuditRequests+"?page=x").Code)

	w := getAs(t, p, "admin1", routeAuditRequest+"?id="+ids[0])
	require.Equal(t, http.StatusOK, w.Code)
	var one struct {
		Summary           auditRequestSummary `json:"summary"`
		RequesterUsername string              `json:"requester_username"`
		History           requestHistory      `json:"history"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &one))
	require.Equal(t, "alice", one.RequesterUsername)
	require.Equal(t, ids[0], one.History.RequestID)
	require.Equal(t, http.StatusNotFound, getAs(t, p, "admin1", routeAuditRequest+"?id="+model.NewId()).Code)
	require.Equal(t, http.StatusBadRequest, getAs(t, p, "admin1", routeAuditRequest+"?id=nope").Code)

	for _, route := range []string{routeAuditRequests, routeAuditRequest + "?id=" + ids[0], routeAuditExport} {
		require.Equal(t, http.StatusForbidden, getAs(t, p, "u_sec", route).Code, "security approvers ask instead: "+route)
	}
}

func TestHandleSubmitAuditExportWebapp(t *testing.T) {
	p, api, store := auditExportPlugin(t)
	api.On("GetUserByUsername", "nobody").Return(nil, testAppErr("not found"))
	post := func(userID, body string) map[string]string {
		r := httptest.NewRequest(http.MethodPost, routeSubmitAuditExportWebapp, strings.NewReader(body))
		r.Header.Set(headerUserID, userID)
		w := httptest.NewRecorder()
		p.ServeHTTP(nil, w, r)
		var out map[string]string
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		return out
	}

	require.Contains(t, post("u_plain", `{"format":"csv"}`)["error"], auditExportNotApproverText)
	require.Contains(t, post("u_sec", `{"format":"csv","requester":"@nobody"}`)["error"], "could not find a user named @nobody")
	for key := range store {
		require.False(t, strings.HasPrefix(key, kvAuditExportRequestPrefix))
	}
}

func TestExportCSV_CarriesEveryDetail(t *testing.T) {
	p, _, store := auditExportPlugin(t)
	store[kvHistoryPrefix+"r1"] = recordJSON(t, requestHistory{RequestID: "r1", Kind: pendingKindBot, Status: historyApproved, Events: []auditEvent{
		{At: 1, Event: eventMessageRelayed, Outcome: "success", Detail: map[string]any{"message": "=SUM(A1)", "direction": "approvers_to_requester"}},
		{At: 2, Event: eventApproved, Outcome: "success", Detail: map[string]any{"bot_user_id": "b1"}},
	}})
	store[kvHistoryPrefix+"r2"] = recordJSON(t, requestHistory{RequestID: "r2", Kind: pendingKindTeam, Status: historyPending})

	w := getAs(t, p, "admin1", routeAuditExport+"?format=csv&request=r1")
	rows, err := csv.NewReader(w.Body).ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 3, "only the one request")
	require.Equal(t, []string{"message", "details"}, rows[0][10:])
	require.Equal(t, "'=SUM(A1)", rows[1][10])
	require.JSONEq(t, `{"direction":"approvers_to_requester"}`, rows[1][11])
	require.JSONEq(t, `{"bot_user_id":"b1"}`, rows[2][11])
}
