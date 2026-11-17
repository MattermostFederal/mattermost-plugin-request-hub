package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func bugReportPlugin(t *testing.T) (*Plugin, *plugintest.API, map[string][]byte) {
	t.Helper()
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.auditSink = nil
	p.setConfiguration(&configuration{
		AllowBugReportRequests: true,
		AuditChannelDetail:     "decisions",
	})
	store := fakeKV(api)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil).Maybe()
	api.On("GetUser", "u_plain").Return(&model.User{Id: "u_plain", Username: "plain"}, nil).Maybe()
	return p, api, store
}

func storedBugReport(t *testing.T, store map[string][]byte) *bugReportRequest {
	t.Helper()
	req := &bugReportRequest{
		ID: model.NewId(), RequesterID: "u_req", Category: bugReportCategoryBug,
		AboutKind: pendingKindChannel, AboutOptions: "prefix ops-", Description: "mobile buttons noop",
		CreatedAt:      model.GetMillis(),
		ApprovalPostID: "card1", ApprovalChannelID: "auditch", DMRootPostID: "dmroot", DMChannelID: "dm1",
	}
	store[kvBugReportRequestPrefix+req.ID] = recordJSON(t, req)
	store[pendingListKey(pendingKindBugReport, "u_req")] = recordJSON(t, []pendingEntry{newPendingEntry(req)})
	store[kvHistoryPrefix+req.ID] = recordJSON(t, requestHistory{
		RequestID: req.ID, Kind: pendingKindBugReport, RequesterID: "u_req", Status: historyPending,
		Events: []auditEvent{{At: 1, Event: eventSubmitted}},
	})
	return req
}

func TestSubmitBugReportRequest_PostsToAuditChannel(t *testing.T) {
	p, api, store := bugReportPlugin(t)
	cfg := *p.getConfiguration()
	cfg.ApprovalTeam = "appteam"
	p.setConfiguration(&cfg)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", Name: "appteam"}, nil)
	api.On("GetChannelByName", "appteam-id", "mattermost-audit-requests", true).
		Return(pluginChannel("auditch", "mattermost-audit-requests"), nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{sysadmin("admin1")}, nil).Maybe()
	api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(&model.Post{Id: "card1", ChannelId: "auditch"}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("GetConfig").Return(&model.Config{}).Maybe()

	msg, err := p.submitBugReportRequest("u_req", "bug", "channel", "prefix ops-", "", "Buttons do nothing on mobile")
	require.NoError(t, err)
	require.Contains(t, msg, "audit channel")

	var id string
	for key := range store {
		if rest, ok := strings.CutPrefix(key, kvBugReportRequestPrefix); ok {
			id = rest
			break
		}
	}
	require.NotEmpty(t, id)
	req, _, err := p.loadBugReportRequest(id)
	require.NoError(t, err)
	require.Equal(t, bugReportCategoryBug, req.Category)
	require.Equal(t, pendingKindChannel, req.AboutKind)
	require.Equal(t, "prefix ops-", req.AboutOptions)

	h := storedHistory(t, store, id)
	require.Equal(t, historyPending, h.Status)
}

func TestSubmitBugReportRequest_Refusals(t *testing.T) {
	p, _, _ := bugReportPlugin(t)
	_, err := p.submitBugReportRequest("u_req", "nope", "general", "", "", "x")
	require.ErrorContains(t, err, "Bug or Feedback")
	_, err = p.submitBugReportRequest("u_req", "bug", "nope", "", "", "x")
	require.ErrorContains(t, err, "Request Hub area")
	_, err = p.submitBugReportRequest("u_req", "bug", "general", "", "", "  ")
	require.ErrorContains(t, err, "describe")

	p.setConfiguration(&configuration{AllowBugReportRequests: false})
	_, err = p.submitBugReportRequest("u_req", "bug", "general", "", "", "x")
	require.EqualError(t, err, bugReportRequestsDisabledMsg)
}

func TestSubmitBugReport_RelatedOwnOnly(t *testing.T) {
	p, _, store := bugReportPlugin(t)
	otherID := model.NewId()
	store[kvHistoryPrefix+otherID] = recordJSON(t, requestHistory{
		RequestID: otherID, Kind: pendingKindChannel, RequesterID: "u_other", Status: historyApproved,
	})

	_, err := p.submitBugReportRequest("u_req", "bug", "general", "", otherID, "linked someone else")
	require.EqualError(t, err, "you can only link one of your own requests")

	require.NoError(t, p.relatedRequestVisible("admin1", otherID, true))
}

func TestDecideAcknowledgeBugReport(t *testing.T) {
	p, api, store := bugReportPlugin(t)
	req := storedBugReport(t, store)
	store[kvApprovalCardsPrefix+req.ID] = recordJSON(t, []string{"card1"})
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil).Maybe()
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
	api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(&model.Post{Id: "reply"}, nil).Maybe()
	updatedPosts(api)

	res := p.decideAcknowledgeBugReport("admin1", req.ID, "")

	require.Contains(t, res.Outcome, "Acknowledged by @admin")
	require.NotContains(t, store, kvBugReportRequestPrefix+req.ID)
	h := storedHistory(t, store, req.ID)
	require.Equal(t, historyAcknowledged, h.Status)
	last := h.Events[len(h.Events)-1]
	require.Equal(t, eventAcknowledged, last.Event)
	require.Equal(t, "admin1", last.ActorID)
}

func TestDecideBugReport_DenyRefused(t *testing.T) {
	p, _, store := bugReportPlugin(t)
	req := storedBugReport(t, store)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+req.ID)

	require.Contains(t, resp.Text, "can't be denied")
	require.Contains(t, store, kvBugReportRequestPrefix+req.ID)
}

func TestDecideAcknowledgeBugReport_OnlySystemAdmin(t *testing.T) {
	p, _, store := bugReportPlugin(t)
	req := storedBugReport(t, store)

	resp := runCommandAs(p, "u_plain", "/mattermost-request-hub approve "+req.ID)

	require.Contains(t, resp.Text, "Only System Admins")
	require.Contains(t, store, kvBugReportRequestPrefix+req.ID)
}

func TestHandleSubmitBugReportWebapp_Disabled(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowBugReportRequests: false})

	body := `{"category":"bug","about_kind":"general","description":"x"}`
	r := httptest.NewRequest(http.MethodPost, routeSubmitBugReportWebapp, strings.NewReader(body))
	r.Header.Set("Mattermost-User-Id", "u1")
	w := httptest.NewRecorder()
	p.ServeHTTP(nil, w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), bugReportRequestsDisabledMsg)
}

func TestBugReportAttachment_AcknowledgeOnly(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	req := &bugReportRequest{ID: "rid", Category: bugReportCategoryFeedback, AboutKind: bugReportAboutGeneral, Description: "nice"}
	att := p.bugReportAttachment(req, &model.User{Username: "req"})
	require.Len(t, att.Actions, 1)
	require.Equal(t, "acknowledge", att.Actions[0].Id)
	require.Equal(t, "/plugins/"+manifest.Id+routeAcknowledgeBugReport, att.Actions[0].Integration.URL)
}
