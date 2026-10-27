package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mmAPIStub builds a minimal httptest.Server that responds to the Mattermost
// REST API endpoints used by createWebhookForRequest. The server writes back
// a synthetic webhook object so callers can verify the success message.
func mmAPIStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/hooks/incoming":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var hook model.IncomingWebhook
			_ = json.NewDecoder(r.Body).Decode(&hook)
			hook.Id = "stub-incoming-id"
			if hook.DisplayName == "" {
				hook.DisplayName = "Test Incoming"
			}
			_ = json.NewEncoder(w).Encode(hook)
		case "/api/v4/hooks/outgoing":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var hook model.OutgoingWebhook
			_ = json.NewDecoder(r.Body).Decode(&hook)
			hook.Id = "stub-outgoing-id"
			if hook.DisplayName == "" {
				hook.DisplayName = "Test Outgoing"
			}
			_ = json.NewEncoder(w).Encode(hook)
		default:
			http.NotFound(w, r)
		}
	}))
}

// stubWebhookChannel sets up the API mock expectations for
// ensureWebhookApprovalChannel (called by submitWebhookRequest).
func stubWebhookChannel(api *plugintest.API, teamName, channelID string) {
	api.On("GetTeamByName", teamName).Return(&model.Team{Id: "team-id", DisplayName: "App Team"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "team-id", "mattermost-webhook-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: channelID}, nil)
}

// --- submitWebhookRequest tests -------------------------------------------------

func TestSubmitWebhookRequest_RequiresChannel(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	_, err := p.submitWebhookRequest("u1", webhookTypeIncoming, "", "My Hook", "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "target channel is required")
}

func TestSubmitWebhookRequest_RequiresDisplayName(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	_, err := p.submitWebhookRequest("u1", webhookTypeIncoming, "ch1", "  ", "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "display name is required")
}

func TestSubmitWebhookRequest_OutgoingRequiresCallbackURL(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	_, err := p.submitWebhookRequest("u1", webhookTypeOutgoing, "ch1", "Hook", "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "callback URL")
}

func TestSubmitWebhookRequest_RejectsUnknownType(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	_, err := p.submitWebhookRequest("u1", "magic", "ch1", "Hook", "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown webhook type")
}

// TestSubmitWebhookRequest_SystemAdminQueuesNotBypasses verifies that, as
// for every request type, a System Admin's webhook request queues for
// approval instead of creating the webhook.
func TestSubmitWebhookRequest_SystemAdminQueuesNotBypasses(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowWebhookRequests: true})

	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team-id", Name: "alerts", Type: model.ChannelTypeOpen}, nil)
	api.On("GetUser", "u_admin").Return(&model.User{Id: "u_admin", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	stubWebhookChannel(api, "appteam", "wh-ch")

	api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool { return p.ChannelId == "wh-ch" })).
		Return(&model.Post{Id: "approval-1"}, nil)
	api.On("GetDirectChannel", "u_admin", "bot-user-id").Return(&model.Channel{Id: "dm-ch"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dm-1"}, nil)

	msg, err := p.submitWebhookRequest("u_admin", webhookTypeIncoming, "ch1", "Monitoring Alerts", "", "")

	require.NoError(t, err)
	// System Admins still queue for two-step approval (no create bypass).
	require.Contains(t, msg, "two-step approval")
	// No REST API call made — webhook not created yet.
	api.AssertNotCalled(t, "CreateIncomingWebhook", mock.Anything)
}

func TestSubmitWebhookRequest_RegularUserQueuesInWebhookChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowWebhookRequests: true})

	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team-id", Name: "alerts", Type: model.ChannelTypeOpen}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	stubWebhookChannel(api, "appteam", "wh-ch")

	var approvalPost *model.Post
	api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool { return p.ChannelId == "wh-ch" })).
		Run(func(args mock.Arguments) { approvalPost = args.Get(0).(*model.Post) }).
		Return(&model.Post{Id: "approval-1"}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm-ch"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dm-1"}, nil)

	msg, err := p.submitWebhookRequest("u_req", webhookTypeIncoming, "ch1", "Monitoring Alerts", "watch for alerts", "")

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	require.NotNil(t, approvalPost)
	require.Equal(t, "wh-ch", approvalPost.ChannelId)
	api.AssertCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestSubmitWebhookRequest_OutgoingQueuesWithCallbackURL(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowWebhookRequests: true})

	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team-id", Name: "ops", Type: model.ChannelTypeOpen}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	stubWebhookChannel(api, "appteam", "wh-ch")

	api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool { return p.ChannelId == "wh-ch" })).
		Return(&model.Post{Id: "approval-1"}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm-ch"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dm-1"}, nil)

	msg, err := p.submitWebhookRequest("u_req", webhookTypeOutgoing, "ch1", "Incident Relay", "sends to PagerDuty", "https://pd.example.com/hook")

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
}

// --- createWebhookForRequest tests ---------------------------------------------

func TestCreateWebhookForRequest_IncomingReturnsURL(t *testing.T) {
	ts := mmAPIStub(t)
	defer ts.Close()

	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetConfig").Return(&model.Config{
		ServiceSettings: model.ServiceSettings{SiteURL: new(ts.URL)},
	})
	// createWebhookForRequest creates a short-lived session for the admin and
	// revokes it immediately after the REST API call.
	api.On("GetUser", "test-admin-user-id").Return(&model.User{Id: "test-admin-user-id", Roles: model.SystemAdminRoleId}, nil)
	api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
	api.On("RevokeSession", "sess1").Return(nil)

	msg, hookID, err := p.createWebhookForRequest(&webhookRequest{
		WebhookType: webhookTypeIncoming,
		ChannelID:   "ch1",
		TeamID:      "team1",
		DisplayName: "Monitoring Alerts",
	}, "test-admin-user-id")

	require.NoError(t, err)
	require.Equal(t, "stub-incoming-id", hookID)
	require.Contains(t, msg, "stub-incoming-id")
	require.Contains(t, msg, "/hooks/")
}

func TestCreateWebhookForRequest_OutgoingReturnsConfirmation(t *testing.T) {
	ts := mmAPIStub(t)
	defer ts.Close()

	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetConfig").Return(&model.Config{
		ServiceSettings: model.ServiceSettings{SiteURL: new(ts.URL)},
	})
	api.On("GetUser", "test-admin-user-id").Return(&model.User{Id: "test-admin-user-id", Roles: model.SystemAdminRoleId}, nil)
	api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess2", Token: "tok2"}, nil)
	api.On("RevokeSession", "sess2").Return(nil)

	msg, _, err := p.createWebhookForRequest(&webhookRequest{
		WebhookType: webhookTypeOutgoing,
		ChannelID:   "ch1",
		TeamID:      "team1",
		DisplayName: "Incident Relay",
		CallbackURL: "https://pd.example.com/hook",
	}, "test-admin-user-id")

	require.NoError(t, err)
	require.Contains(t, msg, "approved and is now active")
}

func TestCreateWebhookForRequest_FailsWithEmptyToken(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	_, _, err := p.createWebhookForRequest(&webhookRequest{WebhookType: webhookTypeIncoming}, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "admin session token is missing")
}

func TestCreateWebhookForRequest_FailsWithNoSiteURL(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetConfig").Return(&model.Config{
		ServiceSettings: model.ServiceSettings{SiteURL: nil},
	})

	_, _, err := p.createWebhookForRequest(&webhookRequest{WebhookType: webhookTypeIncoming}, "tok")
	require.Error(t, err)
	require.Contains(t, err.Error(), "site URL is not configured")
}

func TestCreateWebhookForRequest_APIErrorPropagates(t *testing.T) {
	// Server that always returns 403 — simulates the admin not having
	// the required webhook creation permission.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Forbidden","status_code":403}`, http.StatusForbidden)
	}))
	defer ts.Close()

	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetConfig").Return(&model.Config{
		ServiceSettings: model.ServiceSettings{SiteURL: new(ts.URL)},
	})
	api.On("GetUser", "bad-user-id").Return(&model.User{Id: "bad-user-id", Roles: model.SystemAdminRoleId}, nil)
	api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess3", Token: "bad-token"}, nil)
	api.On("RevokeSession", "sess3").Return(nil)

	_, _, err := p.createWebhookForRequest(&webhookRequest{
		WebhookType: webhookTypeIncoming,
		ChannelID:   "ch1",
		TeamID:      "team1",
		DisplayName: "Alerts",
	}, "bad-user-id")

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create incoming webhook")
}

func TestSubmitWebhookRequest_RejectsNonMember(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team-id", Name: "secret", Type: model.ChannelTypePrivate}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(nil, testAppErr("not a member"))

	_, err := p.submitWebhookRequest("u_req", webhookTypeIncoming, "ch1", "Hook", "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "member of the channel")
}

func TestSubmitWebhookRequest_RejectsDirectMessageChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetChannel", "dm1").Return(&model.Channel{Id: "dm1", Type: model.ChannelTypeDirect}, nil)

	_, err := p.submitWebhookRequest("u_req", webhookTypeIncoming, "dm1", "Hook", "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "public or private")
}

func TestIsTeamAdminOfTeamID_RejectsSoftDeleted(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{
		TeamId: "t1", UserId: "u1", SchemeAdmin: true, DeleteAt: 99,
	}, nil)
	require.False(t, p.isTeamAdminOfTeamID("u1", "t1"))
}
