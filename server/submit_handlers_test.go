package main

import (
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

// serve sends a request through ServeHTTP so the route switch is exercised too.
func serve(p *Plugin, method, path, userID, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if userID != "" {
		r.Header.Set(headerUserID, userID)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(nil, w, r)
	return w
}

func dialogError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code)
	var resp model.SubmitDialogResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Error
}

// bodyUserID is a different user placed in the request body; handlers must
// file the request under the session user instead.
const bodyUserID = "u_other"

// withBodyUser adds a user_id field to a JSON object body.
func withBodyUser(body string) string {
	return strings.Replace(body, "{", `{"user_id":"`+bodyUserID+`",`, 1)
}

// storedRequesterID returns requester_id from the request stored under kvPrefix.
func storedRequesterID(t *testing.T, api *plugintest.API, kvPrefix string) string {
	t.Helper()
	for _, call := range api.Calls {
		if call.Method != "KVSet" || !strings.HasPrefix(call.Arguments.String(0), kvPrefix) {
			continue
		}
		var stored struct {
			RequesterID string `json:"requester_id"`
		}
		require.NoError(t, json.Unmarshal(call.Arguments.Get(1).([]byte), &stored))
		return stored.RequesterID
	}
	require.Fail(t, "no request stored under "+kvPrefix)
	return ""
}

func stubChannelAdminQueue(api *plugintest.API) {
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "ops", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetChannelMember", "ch1", testNomineeID).Return(&model.ChannelMember{}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil).Maybe()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Maybe()
}

func stubTeamAdminQueue(nomineeID string) func(api *plugintest.API) {
	return func(api *plugintest.API) {
		stubTeam1(api)
		api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil).Maybe()
		api.On("GetTeamMember", "team1", nomineeID).Return(&model.TeamMember{TeamId: "team1", UserId: nomineeID}, nil)
	}
}

func stubTeamMemberQueue(api *plugintest.API) {
	stubTeam1(api)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
}

func stubTeamGuestQueue(api *plugintest.API) {
	stubTeam1(api)
	api.On("GetChannel", testChannelID).Return(&model.Channel{Id: testChannelID, Name: "guests", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
}

func stubWebhookQueue(api *plugintest.API) {
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "alerts", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Maybe()
}

// dialogRoute describes one interactive dialog endpoint: a submission that
// queues a request and one the submit function refuses.
type dialogRoute struct {
	route    string
	kvPrefix string
	stub     func(api *plugintest.API)
	ok       model.SubmitDialogRequest
	bad      model.SubmitDialogRequest
	wantErr  string
}

func dialogRoutes() map[string]dialogRoute {
	return map[string]dialogRoute{
		"channel admin": {
			route:    routeChannelAdminDialog,
			kvPrefix: kvAdminRequestPrefix,
			stub:     stubChannelAdminQueue,
			ok:       model.SubmitDialogRequest{Submission: map[string]any{fieldChannelID: "ch1", fieldNominees: testNomineeID}},
			bad:      model.SubmitDialogRequest{Submission: map[string]any{fieldNominees: testNomineeID}},
			wantErr:  "a channel is required",
		},
		"team admin": {
			route:    routeTeamAdminDialog,
			kvPrefix: kvTeamAdminRequestPrefix,
			stub:     stubTeamAdminQueue(testNomineeID),
			ok:       model.SubmitDialogRequest{TeamId: "team1", Submission: map[string]any{fieldMembers: testNomineeID}},
			bad:      model.SubmitDialogRequest{Submission: map[string]any{fieldMembers: testNomineeID}},
			wantErr:  "a team is required",
		},
		"team creation": {
			route:    routeTeamDialog,
			kvPrefix: kvTeamRequestPrefix,
			ok: model.SubmitDialogRequest{Submission: map[string]any{
				fieldDisplayName: "Marketing", fieldType: "O", fieldRequestTeamAdmin: true,
			}},
			bad:     model.SubmitDialogRequest{Submission: map[string]any{fieldDisplayName: strings.Repeat("x", model.TeamDisplayNameMaxRunes+1)}},
			wantErr: "team name must be",
		},
		"team member": {
			route:    routeTeamMemberDialog,
			kvPrefix: kvTeamMemberRequestPrefix,
			stub:     stubTeamMemberQueue,
			ok:       model.SubmitDialogRequest{TeamId: "team1", Submission: map[string]any{fieldMembers: []any{testNomineeID}, "reason": "onboarding"}},
			bad:      model.SubmitDialogRequest{State: "team1", Submission: map[string]any{}},
			wantErr:  "pick at least one person",
		},
		"team guest": {
			route:    routeTeamGuestDialog,
			kvPrefix: kvTeamGuestRequestPrefix,
			stub:     stubTeamGuestQueue,
			ok: model.SubmitDialogRequest{TeamId: "team1", Submission: map[string]any{
				"email": "guest@example.com", "guest_channels": []any{testChannelID},
			}},
			bad: model.SubmitDialogRequest{State: "team1", Submission: map[string]any{
				"email": "not-an-email", "guest_channels": testChannelID,
			}},
			wantErr: "valid guest email",
		},
		"bot": {
			route:    routeBotDialog,
			kvPrefix: kvBotRequestPrefix,
			ok:       model.SubmitDialogRequest{Submission: map[string]any{fieldBotUsername: "svcbot", fieldBotDisplayName: "Service Bot"}},
			bad:      model.SubmitDialogRequest{Submission: map[string]any{fieldBotUsername: "@ab"}},
			wantErr:  "3-22 characters",
		},
		"incoming webhook": {
			route:    routeIncomingWebhookDialog,
			kvPrefix: kvWebhookRequestPrefix,
			stub:     stubWebhookQueue,
			ok:       model.SubmitDialogRequest{Submission: map[string]any{fieldChannelID: "ch1", fieldDisplayName: "Alerts"}},
			bad:      model.SubmitDialogRequest{Submission: map[string]any{fieldDisplayName: "Alerts"}},
			wantErr:  "a target channel is required",
		},
		"outgoing webhook": {
			route:    routeOutgoingWebhookDialog,
			kvPrefix: kvWebhookRequestPrefix,
			stub:     stubWebhookQueue,
			ok: model.SubmitDialogRequest{Submission: map[string]any{
				fieldChannelID: "ch1", fieldDisplayName: "Relay", fieldCallbackURL: "https://hooks.example.com/out",
			}},
			bad:     model.SubmitDialogRequest{Submission: map[string]any{fieldChannelID: "ch1", fieldDisplayName: "Relay"}},
			wantErr: "callback URL is required",
		},
	}
}

func TestDialogSubmitHandlers_InvalidBody(t *testing.T) {
	for name, rt := range dialogRoutes() {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)

			w := serve(p, http.MethodPost, rt.route, "u_req", `{`)

			require.Equal(t, http.StatusBadRequest, w.Code)
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}

func TestDialogSubmitHandlers_CancelledDoesNothing(t *testing.T) {
	for name, rt := range dialogRoutes() {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)

			w := serve(p, http.MethodPost, rt.route, "u_req", string(mustMarshal(t, model.SubmitDialogRequest{Cancelled: true})))

			require.Equal(t, http.StatusOK, w.Code)
			require.Empty(t, w.Body.String())
			// Only log calls are registered, so any other API call fails the test.
		})
	}
}

func TestDialogSubmitHandlers_SubmitErrorShownInDialog(t *testing.T) {
	for name, rt := range dialogRoutes() {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)

			w := serve(p, http.MethodPost, rt.route, "u_req", string(mustMarshal(t, rt.bad)))

			require.Contains(t, dialogError(t, w), rt.wantErr)
			api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}

func TestDialogSubmitHandlers_QueuesAndConfirms(t *testing.T) {
	for name, rt := range dialogRoutes() {
		t.Run(name, func(t *testing.T) {
			p, api := newQueuePlugin(t)
			stubSystemAdminQueue(api)
			if rt.stub != nil {
				rt.stub(api)
			}
			var confirmation *model.Post
			api.On("SendEphemeralPost", testSysAdminID, mock.Anything).Run(func(args mock.Arguments) {
				confirmation = args.Get(1).(*model.Post)
			}).Return(&model.Post{})

			req := rt.ok
			req.ChannelId = "here"
			req.UserId = bodyUserID
			w := serve(p, http.MethodPost, rt.route, testSysAdminID, string(mustMarshal(t, req)))

			require.Equal(t, http.StatusOK, w.Code)
			require.Empty(t, w.Body.String())
			requireQueued(t, api, rt.kvPrefix)
			require.NotNil(t, confirmation)
			require.Equal(t, "here", confirmation.ChannelId)
			require.Contains(t, confirmation.Message, "approval")
			require.Equal(t, testSysAdminID, storedRequesterID(t, api, rt.kvPrefix))
		})
	}
}

func TestTeamCreationDialogSubmit_RecordsTeamAdminChoice(t *testing.T) {
	for name, value := range map[string]any{"checked": true, "unchecked": false, "not a bool": "true"} {
		t.Run(name, func(t *testing.T) {
			p, api := newQueuePlugin(t)
			stubSystemAdminQueue(api)
			api.On("SendEphemeralPost", testSysAdminID, mock.Anything).Return(&model.Post{})

			w := serve(p, http.MethodPost, routeTeamDialog, testSysAdminID, string(mustMarshal(t, model.SubmitDialogRequest{
				Submission: map[string]any{fieldDisplayName: "Marketing", fieldType: "O", fieldRequestTeamAdmin: value},
			})))

			require.Equal(t, http.StatusOK, w.Code)
			var stored *teamCreationRequest
			for _, call := range api.Calls {
				if call.Method == "KVSet" && strings.HasPrefix(call.Arguments.String(0), kvTeamRequestPrefix) {
					require.NoError(t, json.Unmarshal(call.Arguments.Get(1).([]byte), &stored))
					break
				}
			}
			require.NotNil(t, stored)
			require.Equal(t, value == true, stored.RequestTeamAdmin)
		})
	}
}

// webappRoute describes one webapp modal endpoint.
type webappRoute struct {
	route    string
	kvPrefix string
	stub     func(api *plugintest.API)
	ok       string
	refusals map[string]webappRefusal
}

type webappRefusal struct {
	userID  string // defaults to u_req
	body    string
	stub    func(api *plugintest.API)
	wantErr string
}

func webappRoutes() map[string]webappRoute {
	longTeamName := strings.Repeat("x", model.TeamDisplayNameMaxRunes+1)
	return map[string]webappRoute{
		"team creation": {
			route:    routeSubmitTeamCreationWebapp,
			kvPrefix: kvTeamRequestPrefix,
			ok:       `{"display_name":"Marketing","type":"O","description":"Campaigns","request_team_admin":true}`,
			refusals: map[string]webappRefusal{
				"name too long": {body: `{"display_name":"` + longTeamName + `"}`, wantErr: "team name must be"},
			},
		},
		"team admin": {
			route:    routeSubmitTeamAdminWebapp,
			kvPrefix: kvTeamAdminRequestPrefix,
			stub:     stubTeamAdminQueue(testSysAdminID),
			ok:       `{"team_id":"team1"}`,
			refusals: map[string]webappRefusal{
				"no team": {body: `{"team_id":" "}`, wantErr: "team_id is required"},
				"team not found": {userID: testNomineeID, body: `{"team_id":"team1"}`, stub: func(api *plugintest.API) {
					api.On("GetTeam", "team1").Return(nil, testAppErr("no team"))
				}, wantErr: "could not find that team"},
			},
		},
		"team member": {
			route:    routeSubmitTeamMemberWebapp,
			kvPrefix: kvTeamMemberRequestPrefix,
			stub: func(api *plugintest.API) {
				stubTeamMemberQueue(api)
				api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
			},
			ok: `{"team_id":"team1","nominees":["@nommy"],"reason":"onboarding"}`,
			refusals: map[string]webappRefusal{
				"no team": {body: `{"nominees":["nommy"]}`, wantErr: "team_id is required"},
				"unknown nominee": {body: `{"team_id":"team1","nominees":["ghost"]}`, stub: func(api *plugintest.API) {
					api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))
				}, wantErr: "unknown user: ghost"},
				"no nominees": {body: `{"team_id":"team1","nominees":[]}`, wantErr: "pick at least one person"},
			},
		},
		"team guest": {
			route:    routeSubmitTeamGuestWebapp,
			kvPrefix: kvTeamGuestRequestPrefix,
			stub:     stubTeamGuestQueue,
			ok:       `{"team_id":"team1","email":"guest@example.com","channel_ids":["` + testChannelID + `"],"reason":"vendor"}`,
			refusals: map[string]webappRefusal{
				"no team":   {body: `{"email":"guest@example.com"}`, wantErr: "team_id is required"},
				"bad email": {body: `{"team_id":"team1","email":"nope","channel_ids":["` + testChannelID + `"]}`, wantErr: "valid guest email"},
			},
		},
		"bot": {
			route:    routeSubmitBotWebapp,
			kvPrefix: kvBotRequestPrefix,
			ok:       `{"username":"svcbot","display_name":"Service Bot","description":"Posts alerts","request_token":true}`,
			refusals: map[string]webappRefusal{
				"username too short": {body: `{"username":"ab"}`, wantErr: "3-22 characters"},
			},
		},
		"incoming webhook": {
			route:    routeSubmitIncomingWebhookWebapp,
			kvPrefix: kvWebhookRequestPrefix,
			stub:     stubWebhookQueue,
			ok:       `{"channel_id":"ch1","display_name":"Alerts","description":"Pager"}`,
			refusals: map[string]webappRefusal{
				"no channel": {body: `{"display_name":"Alerts"}`, wantErr: "a target channel is required"},
			},
		},
		"outgoing webhook": {
			route:    routeSubmitOutgoingWebhookWebapp,
			kvPrefix: kvWebhookRequestPrefix,
			stub:     stubWebhookQueue,
			ok:       `{"channel_id":"ch1","display_name":"Relay","callback_url":"https://hooks.example.com/out"}`,
			refusals: map[string]webappRefusal{
				"no callback": {body: `{"channel_id":"ch1","display_name":"Relay"}`, wantErr: "callback URL is required"},
			},
		},
	}
}

func TestSubmitWebappHandlers_InvalidBody(t *testing.T) {
	for name, rt := range webappRoutes() {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)

			w := serve(p, http.MethodPost, rt.route, "u_req", `not json`)

			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Contains(t, w.Body.String(), "invalid request body")
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}

func TestSubmitWebappHandlers_Refusals(t *testing.T) {
	for name, rt := range webappRoutes() {
		for refusal, tc := range rt.refusals {
			t.Run(name+"/"+refusal, func(t *testing.T) {
				api := &plugintest.API{}
				stubLogLevels(api)
				p := newTestPlugin(api)
				api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
				if tc.stub != nil {
					tc.stub(api)
				}

				userID := tc.userID
				if userID == "" {
					userID = "u_req"
				}
				w := serve(p, http.MethodPost, rt.route, userID, tc.body)

				out := jsonMap(t, w)
				require.Contains(t, out["error"], tc.wantErr)
				require.Empty(t, out["message"])
				api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
			})
		}
	}
}

func TestSubmitWebappHandlers_QueuesAndReplies(t *testing.T) {
	for name, rt := range webappRoutes() {
		t.Run(name, func(t *testing.T) {
			p, api := newQueuePlugin(t)
			stubSystemAdminQueue(api)
			if rt.stub != nil {
				rt.stub(api)
			}

			w := serve(p, http.MethodPost, rt.route, testSysAdminID, withBodyUser(rt.ok))

			out := jsonMap(t, w)
			require.Empty(t, out["error"])
			require.Contains(t, out["message"], "approval")
			requireQueued(t, api, rt.kvPrefix)
			require.Equal(t, testSysAdminID, storedRequesterID(t, api, rt.kvPrefix))
		})
	}
}

func TestHandleWebappCreate_QueuesAndReplies(t *testing.T) {
	p, api := newQueuePlugin(t)
	cfg := *p.getConfiguration()
	cfg.prefixes = []channelPrefix{{Prefix: "team-"}}
	p.setConfiguration(&cfg)
	stubSystemAdminQueue(api)
	stubTeam1(api)
	api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil).Maybe()

	w := serve(p, http.MethodPost, routeCreate, testSysAdminID,
		withBodyUser(`{"team_id":"team1","display_name":"Ops Room","prefix":"team-","channel_type":"O","members":["nommy"],"admin_members":["@nommy"]}`))

	out := jsonMap(t, w)
	require.Empty(t, out["error"])
	require.Contains(t, out["message"], "submitted for approval")
	requireQueued(t, api, kvRequestPrefix)
	require.Equal(t, testSysAdminID, storedRequesterID(t, api, kvRequestPrefix))
}

func TestHandleRequestAdmin_QueuesAndReplies(t *testing.T) {
	p, api := newQueuePlugin(t)
	stubSystemAdminQueue(api)
	stubChannelAdminQueue(api)
	api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)

	w := serve(p, http.MethodPost, routeRequestAdmin, testSysAdminID, withBodyUser(`{"channel_id":"ch1","nominees":["@nommy"]}`))

	out := jsonMap(t, w)
	require.Empty(t, out["error"])
	require.Contains(t, out["message"], "submitted")
	requireQueued(t, api, kvAdminRequestPrefix)
	require.Equal(t, testSysAdminID, storedRequesterID(t, api, kvAdminRequestPrefix))
}

func TestHandleListSidebarCategories_MissingTeamID(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)

	w := serve(p, http.MethodGet, routeSidebarCategories, "u1", "")

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "team_id required")
	api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
}

func TestHandleListSidebarCategories_LookupFailure(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(nil, testAppErr("boom"))

	w := serve(p, http.MethodGet, routeSidebarCategories+"?team_id=t1", "u1", "")

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "failed to list categories")
}

func TestHandleListSidebarCategories_NoCategoriesReturned(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(nil, nil)

	w := serve(p, http.MethodGet, routeSidebarCategories+"?team_id=t1", "u1", "")

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, "[]", w.Body.String())
}

func TestHandleListSidebarCategories_SkipsEmptyEntries(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{
			nil,
			{SidebarCategory: model.SidebarCategory{Id: "c1", DisplayName: "Work", Type: model.SidebarCategoryCustom}},
		},
	}, nil)

	w := serve(p, http.MethodGet, routeSidebarCategories+"?team_id=t1", "u1", "")

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `[{"id":"c1","display_name":"Work","type":"custom"}]`, w.Body.String())
}

func TestHandleConfig_ReportsEachRequestType(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowBotTokenRequests: true, AllowWebhookRequests: true})

	w := serve(p, http.MethodGet, routeConfig, "u1", "")

	require.Equal(t, http.StatusOK, w.Code)
	var out map[string]bool
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Equal(t, map[string]bool{
		requestTypeChannel:      true,
		requestTypeChannelAdmin: false,
		requestTypeTeam:         false,
		requestTypeTeamAdmin:    false,
		requestTypeTeamMember:   false,
		requestTypeTeamGuest:    false,
		requestTypeWebhook:      true,
		requestTypeBot:          true,
	}, out)
}

func TestServeHTTP_RecoversHandlerPanic(t *testing.T) {
	panicOnGetUser := func(api *plugintest.API) {
		api.On("GetUser", "u1").Run(func(mock.Arguments) { panic("lookup blew up") }).Return(nil, nil)
	}

	t.Run("action route replies with JSON", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		panicOnGetUser(api)

		w := serve(p, http.MethodPost, routeApprove, "u1", actionBody(t, "req1"))

		resp := actionResponse(t, w)
		require.Contains(t, resp.EphemeralText, "Something went wrong processing that action")
		api.AssertCalled(t, "LogError", "channel-requests: ServeHTTP panic recovered",
			"path", routeApprove, "panic", "lookup blew up", "stack", mock.Anything)
	})

	t.Run("other route returns a server error", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		panicOnGetUser(api)

		w := serve(p, http.MethodPost, routeChannelAdminDialog, "u1", string(mustMarshal(t, model.SubmitDialogRequest{
			Submission: map[string]any{fieldChannelID: "ch1", fieldNominees: testNomineeID},
		})))

		require.Equal(t, http.StatusInternalServerError, w.Code)
		require.Contains(t, w.Body.String(), "internal plugin error")
		api.AssertCalled(t, "LogError", "channel-requests: ServeHTTP panic recovered",
			"path", routeChannelAdminDialog, "panic", "lookup blew up", "stack", mock.Anything)
	})
}

type stringerID string

func (s stringerID) String() string { return "str:" + string(s) }

func TestContextString(t *testing.T) {
	cases := []struct {
		name string
		ctx  map[string]any
		want string
	}{
		{name: "nil context", ctx: nil, want: ""},
		{name: "missing key", ctx: map[string]any{}, want: ""},
		{name: "string", ctx: map[string]any{"k": "abc"}, want: "abc"},
		{name: "stringer", ctx: map[string]any{"k": stringerID("abc")}, want: "str:abc"},
		{name: "other type", ctx: map[string]any{"k": 42}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, contextString(tc.ctx, "k"))
		})
	}
}

func TestSubmissionIDList(t *testing.T) {
	cases := []struct {
		name       string
		submission map[string]any
		want       []string
	}{
		{name: "nil submission", submission: nil, want: nil},
		{name: "missing key", submission: map[string]any{}, want: nil},
		{name: "comma string", submission: map[string]any{"k": " a, ,b "}, want: []string{"a", "b"}},
		{name: "blank string", submission: map[string]any{"k": "  "}, want: nil},
		{name: "multi-select values", submission: map[string]any{"k": []any{" a ", "", 7, "b"}}, want: []string{"a", "b"}},
		{name: "string slice", submission: map[string]any{"k": []string{"a", " a", "", "b"}}, want: []string{"a", "b"}},
		{name: "other type", submission: map[string]any{"k": true}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := submissionIDList(tc.submission, "k")
			if tc.want == nil {
				require.Empty(t, got)
				return
			}
			require.Equal(t, tc.want, got)
		})
	}
}
