package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// hookCapture records the webhook bodies sent to the REST API and answers
// like mmAPIStub does. status overrides the reply code when non-zero.
type hookCapture struct {
	incoming *model.IncomingWebhook
	outgoing *model.OutgoingWebhook
	status   int
	// onIncoming, when set, runs as the incoming webhook request arrives.
	onIncoming func()
}

func (c *hookCapture) server(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		if c.status != 0 {
			http.Error(w, `{"message":"no","status_code":403}`, c.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/hooks/incoming":
			if c.onIncoming != nil {
				c.onIncoming()
			}
			var hook model.IncomingWebhook
			require.NoError(t, json.Unmarshal(body, &hook))
			c.incoming = &hook
			hook.Id = "in-hook-id"
			_ = json.NewEncoder(w).Encode(hook)
		case "/api/v4/hooks/outgoing":
			var hook model.OutgoingWebhook
			require.NoError(t, json.Unmarshal(body, &hook))
			c.outgoing = &hook
			hook.Id = "out-hook-id"
			_ = json.NewEncoder(w).Encode(hook)
		case "/api/v4/users/new-bot-id/tokens":
			_ = json.NewEncoder(w).Encode(model.UserAccessToken{Id: "tok-id", Token: "tok-value"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// stubSiteURL points the plugin's REST calls at url.
func stubSiteURL(api *plugintest.API, url string) {
	api.On("GetConfig").Return(&model.Config{ServiceSettings: model.ServiceSettings{SiteURL: new(url)}})
}

// stubApproverSession lets createUserSession and the deferred revoke run.
func stubApproverSession(api *plugintest.API) {
	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Roles: "system_user system_admin"}, nil)
	api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
	api.On("RevokeSession", "sess1").Return(nil)
}

func TestCreateWebhookForRequest_IncomingOwnedByRequesterAndLocked(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	stubApproverSession(api)

	_, hookID, err := p.createWebhookForRequest(&webhookRequest{
		RequesterID: "u_req", WebhookType: webhookTypeIncoming, ChannelID: "ch1", TeamID: "team1", DisplayName: "Alerts",
	}, "admin1")

	require.NoError(t, err)
	require.Equal(t, "in-hook-id", hookID)
	require.NotNil(t, capture.incoming)
	require.Equal(t, "u_req", capture.incoming.UserId)
	require.True(t, capture.incoming.ChannelLocked)
	require.Equal(t, "ch1", capture.incoming.ChannelId)
	require.Equal(t, "team1", capture.incoming.TeamId)
}

func TestCreateWebhookForRequest_OutgoingOwnedByRequester(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	stubApproverSession(api)

	_, hookID, err := p.createWebhookForRequest(&webhookRequest{
		RequesterID: "u_req", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", TeamID: "team1",
		DisplayName: "Relay", CallbackURL: "https://hooks.example.com/in",
	}, "admin1")

	require.NoError(t, err)
	require.Equal(t, "out-hook-id", hookID)
	require.NotNil(t, capture.outgoing)
	require.Equal(t, "u_req", capture.outgoing.CreatorId)
	require.Equal(t, model.StringArray{"https://hooks.example.com/in"}, capture.outgoing.CallbackURLs)
}

func TestCreateWebhookForRequest_OutgoingAPIErrorPropagates(t *testing.T) {
	capture := &hookCapture{status: http.StatusForbidden}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	stubApproverSession(api)

	_, _, err := p.createWebhookForRequest(&webhookRequest{
		RequesterID: "u_req", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", TeamID: "team1",
		DisplayName: "Relay", CallbackURL: "https://hooks.example.com/in",
	}, "admin1")

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create outgoing webhook")
}

func TestCreateWebhookForRequest_UnknownTypeIsRefused(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, "http://localhost:8065")
	stubApproverSession(api)

	_, _, err := p.createWebhookForRequest(&webhookRequest{WebhookType: "sideways"}, "admin1")

	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown webhook type")
}

func TestCreateWebhookForRequest_SessionFailureStopsBeforeCreate(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, "http://localhost:8065")
	api.On("GetUser", "admin1").Return(nil, testAppErr("no user"))

	_, _, err := p.createWebhookForRequest(&webhookRequest{WebhookType: webhookTypeIncoming}, "admin1")

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create admin session")
	api.AssertNotCalled(t, "RevokeSession", mock.Anything)
}

func TestCreateWebhookForRequest_RevokeFailureIsLogged(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1"}, nil)
	api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
	api.On("RevokeSession", "sess1").Return(testAppErr("revoke failed"))

	_, hookID, err := p.createWebhookForRequest(&webhookRequest{
		RequesterID: "u_req", WebhookType: webhookTypeIncoming, ChannelID: "ch1", TeamID: "team1",
	}, "admin1")

	require.NoError(t, err)
	require.Equal(t, "in-hook-id", hookID)
	api.AssertCalled(t, "LogWarn", "failed to revoke admin session after webhook op", "error", mock.Anything)
}

func TestCreateWebhookForRequest_UsesOverrideWhenSet(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, "http://localhost:8065")
	p.createWebhookFn = func(req *webhookRequest, adminUserID string) (string, string, error) {
		return "msg", "hook-" + adminUserID, nil
	}

	msg, hookID, err := p.createWebhookForRequest(&webhookRequest{WebhookType: webhookTypeIncoming}, "admin1")

	require.NoError(t, err)
	require.Equal(t, "msg", msg)
	require.Equal(t, "hook-admin1", hookID)
	api.AssertNotCalled(t, "CreateSession", mock.Anything)
}

func TestCreateIncomingWebhookForBot_OwnedByBotAndLocked(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)

	url, err := p.createIncomingWebhookForBot("tok1", "new-bot-id", &botRequest{
		RequesterID: "u_req", Username: "svcbot", IncomingWebhookChannelID: "ch1",
	})

	require.NoError(t, err)
	require.Equal(t, ts.URL+"/hooks/in-hook-id", url)
	require.NotNil(t, capture.incoming)
	require.Equal(t, "new-bot-id", capture.incoming.UserId)
	require.True(t, capture.incoming.ChannelLocked)
	require.Equal(t, "team1", capture.incoming.TeamId)
	require.Equal(t, "Incoming webhook for @svcbot", capture.incoming.DisplayName)
}

func TestCreateIncomingWebhookForBot_KeepsRequestedName(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)

	_, err := p.createIncomingWebhookForBot("tok1", "new-bot-id", &botRequest{
		Username: "svcbot", IncomingWebhookChannelID: "ch1", IncomingWebhookDisplayName: "Build alerts",
	})

	require.NoError(t, err)
	require.Equal(t, "Build alerts", capture.incoming.DisplayName)
}

func TestCreateIncomingWebhookForBot_ChannelLoadFailure(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	stubSiteURL(api, "http://localhost:8065")
	api.On("GetChannel", "ch1").Return(nil, testAppErr("gone"))

	_, err := p.createIncomingWebhookForBot("tok1", "new-bot-id", &botRequest{IncomingWebhookChannelID: "ch1"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "load channel")
}

func TestCreateIncomingWebhookForBot_APIErrorPropagates(t *testing.T) {
	capture := &hookCapture{status: http.StatusForbidden}
	ts := capture.server(t)
	api := &plugintest.API{}
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)

	_, err := p.createIncomingWebhookForBot("tok1", "new-bot-id", &botRequest{IncomingWebhookChannelID: "ch1"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "create incoming webhook")
}

func TestCreateOutgoingWebhookForBot_OwnedByBot(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)

	err := p.createOutgoingWebhookForBot("tok1", "new-bot-id", &botRequest{
		RequesterID: "u_req", Username: "svcbot", OutgoingWebhookChannelID: "ch1",
		OutgoingWebhookCallbackURL: "https://hooks.example.com/out",
	})

	require.NoError(t, err)
	require.NotNil(t, capture.outgoing)
	require.Equal(t, "new-bot-id", capture.outgoing.CreatorId)
	require.Equal(t, "team1", capture.outgoing.TeamId)
	require.Equal(t, "Outgoing webhook for @svcbot", capture.outgoing.DisplayName)
	require.Equal(t, model.StringArray{"https://hooks.example.com/out"}, capture.outgoing.CallbackURLs)
}

func TestCreateOutgoingWebhookForBot_KeepsRequestedName(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)

	err := p.createOutgoingWebhookForBot("tok1", "new-bot-id", &botRequest{
		Username: "svcbot", OutgoingWebhookChannelID: "ch1", OutgoingWebhookDisplayName: "Deploy relay",
		OutgoingWebhookCallbackURL: "https://hooks.example.com/out",
	})

	require.NoError(t, err)
	require.Equal(t, "Deploy relay", capture.outgoing.DisplayName)
}

func TestCreateOutgoingWebhookForBot_ChannelLoadFailure(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	stubSiteURL(api, "http://localhost:8065")
	api.On("GetChannel", "ch1").Return(nil, testAppErr("gone"))

	err := p.createOutgoingWebhookForBot("tok1", "new-bot-id", &botRequest{OutgoingWebhookChannelID: "ch1"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "load channel")
}

func TestCreateOutgoingWebhookForBot_APIErrorPropagates(t *testing.T) {
	capture := &hookCapture{status: http.StatusForbidden}
	ts := capture.server(t)
	api := &plugintest.API{}
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)

	err := p.createOutgoingWebhookForBot("tok1", "new-bot-id", &botRequest{
		OutgoingWebhookChannelID: "ch1", OutgoingWebhookCallbackURL: "https://hooks.example.com/out",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "create outgoing webhook")
}

// calledMethods lists the API methods called so far, in order.
func calledMethods(api *plugintest.API) []string {
	var methods []string
	for _, call := range api.Calls {
		methods = append(methods, call.Method)
	}
	return methods
}

// finalBotApproval returns a bot request whose security step is filled, so
// admin1's Approve creates the bot.
func finalBotApproval(req botRequest) *botRequest {
	req.ID = "r1"
	req.RequesterID = "u_req"
	req.Username = "svcbot"
	req.ApprovalPostID = "post1"
	req.twoStepState = twoStepState{SecurityApproverID: "sec1", SecurityApprovedAt: 1}
	return &req
}

// stubFinalBotApproval registers the calls admin1's final Approve makes up
// to and including creating the bot.
func stubFinalBotApproval(t *testing.T, api *plugintest.API, req *botRequest) {
	t.Helper()
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	stubApproverSession(api)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvBotRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvBotRequestPrefix+"r1", raw).Return(true, nil)
	api.On("CreateBot", mock.Anything).Return(&model.Bot{UserId: "new-bot-id", Username: "svcbot"}, nil)
}

func TestHandleBotAction_BotJoinsChannelBeforeOwningIncomingWebhook(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)

	req := finalBotApproval(botRequest{
		DMChannelID: "dm1", DMRootPostID: "dm-root",
		IncomingWebhookChannelID: "ch1",
		OutgoingWebhookChannelID: "ch2", OutgoingWebhookCallbackURL: "https://hooks.example.com/out",
	})
	stubFinalBotApproval(t, api, req)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1", Type: model.ChannelTypePrivate}, nil)
	api.On("GetChannel", "ch2").Return(&model.Channel{Id: "ch2", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetTeamMember", "team1", "new-bot-id").Return(nil, testAppErr("not a member"))
	api.On("CreateTeamMember", "team1", "new-bot-id").Return(&model.TeamMember{}, nil)
	api.On("AddChannelMember", "ch1", "new-bot-id").Return(&model.ChannelMember{}, nil)
	// Closing the ticket and replying with the webhook URL.
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("no post"))
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "reply1"}, nil)
	api.On("KVGet", tokenPostRegistryKey).Return(nil, nil)

	var before []string
	capture.onIncoming = func() { before = calledMethods(api) }

	resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleBotAction }}, true)

	require.NotNil(t, resp.Update)
	require.NotNil(t, capture.incoming)
	require.Equal(t, "new-bot-id", capture.incoming.UserId)
	require.NotNil(t, capture.outgoing)
	require.Equal(t, "new-bot-id", capture.outgoing.CreatorId)
	require.Contains(t, before, "CreateTeamMember")
	require.Contains(t, before, "AddChannelMember")
}

func TestHandleBotAction_BotAlreadyOnTeamOnlyJoinsChannel(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)

	req := finalBotApproval(botRequest{DMChannelID: "dm1", DMRootPostID: "dm-root", IncomingWebhookChannelID: "ch1"})
	stubFinalBotApproval(t, api, req)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetTeamMember", "team1", "new-bot-id").Return(&model.TeamMember{TeamId: "team1", UserId: "new-bot-id"}, nil)
	api.On("AddChannelMember", "ch1", "new-bot-id").Return(&model.ChannelMember{}, nil)
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("no post"))
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "reply1"}, nil)
	api.On("KVGet", tokenPostRegistryKey).Return(nil, nil)

	resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleBotAction }}, true)

	require.NotNil(t, resp.Update)
	require.Equal(t, "new-bot-id", capture.incoming.UserId)
	api.AssertNotCalled(t, "CreateTeamMember", mock.Anything, mock.Anything)
}

func TestHandleBotAction_ChannelJoinFailureSkipsIncomingWebhook(t *testing.T) {
	cases := []struct {
		name  string
		stub  func(api *plugintest.API)
		error string
	}{
		{
			name: "channel can't be loaded",
			stub: func(api *plugintest.API) {
				api.On("GetChannel", "ch1").Return(nil, testAppErr("gone"))
			},
			error: "load channel",
		},
		{
			name: "team join fails",
			stub: func(api *plugintest.API) {
				api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
				api.On("GetTeamMember", "team1", "new-bot-id").Return(nil, testAppErr("not a member"))
				api.On("CreateTeamMember", "team1", "new-bot-id").Return(nil, testAppErr("join failed"))
			},
			error: "add bot to team",
		},
		{
			name: "channel join fails",
			stub: func(api *plugintest.API) {
				api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
				api.On("GetTeamMember", "team1", "new-bot-id").Return(&model.TeamMember{}, nil)
				api.On("AddChannelMember", "ch1", "new-bot-id").Return(nil, testAppErr("join failed"))
			},
			error: "add bot to channel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &hookCapture{}
			ts := capture.server(t)
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			stubSiteURL(api, ts.URL)

			req := finalBotApproval(botRequest{DMChannelID: "dm1", DMRootPostID: "dm-root", IncomingWebhookChannelID: "ch1"})
			stubFinalBotApproval(t, api, req)
			tc.stub(api)
			api.On("GetPost", mock.Anything).Return(nil, testAppErr("no post"))
			var reply string
			api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
				if post := args.Get(0).(*model.Post); post.ChannelId == "dm1" {
					reply = post.Message
				}
			}).Return(&model.Post{Id: "reply1"}, nil)

			resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleBotAction }}, true)

			require.NotNil(t, resp.Update)
			require.Nil(t, capture.incoming)
			require.Contains(t, reply, "incoming webhook creation failed: "+tc.error)
		})
	}
}

func TestHandleBotAction_UndeliverableTokenClearsCards(t *testing.T) {
	for _, deleteFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("bot delete fails %v", deleteFails), func(t *testing.T) {
			capture := &hookCapture{}
			ts := capture.server(t)
			api := &plugintest.API{}
			stubLogLevels(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			stubSiteURL(api, ts.URL)

			req := finalBotApproval(botRequest{RequestToken: true})
			stubFinalBotApproval(t, api, req)
			api.On("KVSet", mock.Anything, mock.Anything).Return(nil) // ticket relay lookups
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(nil, testAppErr("no dm"))
			if deleteFails {
				api.On("PermanentDeleteBot", "new-bot-id").Return(testAppErr("delete failed"))
			} else {
				api.On("PermanentDeleteBot", "new-bot-id").Return(nil)
			}
			api.On("KVDelete", kvApprovalCardsPrefix+"r1").Return(nil).Once()
			api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

			resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleBotAction }}, true)

			require.NotNil(t, resp.Update)
			require.Contains(t, resp.Update.Message, "token could not be delivered")
		})
	}
}

func TestHandleWebhookAction_UndeliverableURLClearsCards(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, "http://localhost:8065")
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) { return "msg", "hook1", nil }
	p.deleteWebhookFn = func(string, string) error { return nil }

	req := &webhookRequest{
		ID: "r1", RequesterID: "u_req", WebhookType: webhookTypeIncoming, ChannelID: "ch1", DisplayName: "Alerts",
		ApprovalPostID: "post1", twoStepState: twoStepState{SecurityApproverID: "sec1", SecurityApprovedAt: 1},
	}
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	stubSystemAdmin(api)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil)
	api.On("KVGet", kvWebhookRequestPrefix+"r1").Return(raw, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil) // ticket relay lookups
	api.On("KVCompareAndDelete", kvWebhookRequestPrefix+"r1", raw).Return(true, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(nil, testAppErr("no dm"))
	api.On("KVDelete", kvApprovalCardsPrefix+"r1").Return(nil).Once()
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleWebhookAction }}, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "URL could not be delivered")
}

func TestTwoStepActionHandlers_LostRequestClearsCards(t *testing.T) {
	t.Run("bot", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)

		req := finalBotApproval(botRequest{})
		raw, err := json.Marshal(req)
		require.NoError(t, err)
		stubSystemAdmin(api)
		api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
		api.On("KVGet", kvBotRequestPrefix+"r1").Return(raw, nil)
		api.On("KVSet", mock.MatchedBy(func(key string) bool { return key != kvBotRequestPrefix+"r1" }), mock.Anything).Return(nil)
		api.On("KVCompareAndDelete", kvBotRequestPrefix+"r1", raw).Return(true, nil)
		api.On("CreateBot", mock.Anything).Return(nil, testAppErr("name taken"))
		// The pending request can't be written back.
		api.On("KVSet", kvBotRequestPrefix+"r1", raw).Return(testAppErr("kv down"))
		api.On("KVDelete", kvApprovalCardsPrefix+"r1").Return(nil).Once()

		resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleBotAction }}, true)

		require.Contains(t, resp.EphemeralText, "could not be saved")
	})
	t.Run("webhook", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		stubSiteURL(api, "http://localhost:8065")
		p.createWebhookFn = func(*webhookRequest, string) (string, string, error) { return "", "", errors.New("hooks are off") }

		req := &webhookRequest{
			ID: "r1", RequesterID: "u_req", WebhookType: webhookTypeIncoming, ChannelID: "ch1", DisplayName: "Alerts",
			ApprovalPostID: "post1", twoStepState: twoStepState{SecurityApproverID: "sec1", SecurityApprovedAt: 1},
		}
		raw, err := json.Marshal(req)
		require.NoError(t, err)
		stubSystemAdmin(api)
		api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
		api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil)
		api.On("KVGet", kvWebhookRequestPrefix+"r1").Return(raw, nil)
		api.On("KVSet", mock.MatchedBy(func(key string) bool { return key != kvWebhookRequestPrefix+"r1" }), mock.Anything).Return(nil)
		api.On("KVCompareAndDelete", kvWebhookRequestPrefix+"r1", raw).Return(true, nil)
		api.On("KVSet", kvWebhookRequestPrefix+"r1", raw).Return(testAppErr("kv down"))
		api.On("KVDelete", kvApprovalCardsPrefix+"r1").Return(nil).Once()

		resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleWebhookAction }}, true)

		require.Contains(t, resp.EphemeralText, "could not be saved")
	})
}
