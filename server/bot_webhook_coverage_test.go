package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// restServer records the REST calls it receives and answers each with
// status (and body, when set).
type restServer struct {
	mu     sync.Mutex
	calls  []string
	status int
	body   string
}

func (s *restServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		body := s.body
		if body == "" {
			body = `{"status":"OK"}`
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (s *restServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// registryHolds matches a registry write whose entries are exactly postIDs.
func registryHolds(postIDs ...string) any {
	return mock.MatchedBy(func(data []byte) bool {
		var entries []tokenPostEntry
		if err := json.Unmarshal(data, &entries); err != nil || len(entries) != len(postIDs) {
			return false
		}
		for i, e := range entries {
			if e.PostID != postIDs[i] {
				return false
			}
		}
		return true
	})
}

func TestSubmitBotRequest_StoreFailure(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	stubRequester(api)
	api.On("KVSet", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvBotRequestPrefix) }), mock.Anything).
		Return(testAppErr("kv down"))

	_, err := p.submitBotRequest("u_req", botRequestInput{Username: "ci-bot", DisplayName: "CI Bot"})

	require.EqualError(t, err, "could not save your request; try again")
	api.AssertCalled(t, "LogError", "failed to store bot request", "error", mock.Anything)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestSubmitBotRequest_ApprovalPostFailureRemovesRequest(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	cfg := *p.getConfiguration()
	cfg.ApprovalTeam = "appteam"
	p.setConfiguration(&cfg)
	stubRequester(api)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id"}, nil)
	api.On("GetChannelByName", "appteam-id", "mattermost-bot-requests", false).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "bots-ch"}, nil)
	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))
	var deleted string
	api.On("KVDelete", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvBotRequestPrefix) })).
		Run(func(args mock.Arguments) { deleted = args.String(0) }).Return(nil)

	_, err := p.submitBotRequest("u_req", botRequestInput{Username: "ci-bot", DisplayName: "CI Bot"})

	require.EqualError(t, err, "could not send your request to the approvers; try again or contact a System Admin")
	require.NotEmpty(t, deleted)
	api.AssertNotCalled(t, "GetDirectChannel", mock.Anything, mock.Anything)
}

func TestSubmitWebhookRequest_StoreFailure(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("GetUser", "u_req").Return(regularUser(), nil)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team-id", Name: "alerts", Type: model.ChannelTypeOpen}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("KVSet", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvWebhookRequestPrefix) }), mock.Anything).
		Return(testAppErr("kv down"))

	_, err := p.submitWebhookRequest("u_req", webhookTypeIncoming, "ch1", "Alerts", "", "")

	require.EqualError(t, err, "could not save your request; try again")
	api.AssertCalled(t, "LogError", "failed to store webhook request", "error", mock.Anything)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestSubmitWebhookRequest_ApprovalPostFailureRemovesRequest(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	cfg := *p.getConfiguration()
	cfg.ApprovalTeam = "appteam"
	p.setConfiguration(&cfg)
	api.On("GetUser", "u_req").Return(regularUser(), nil)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team-id", Name: "alerts", Type: model.ChannelTypeOpen}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	stubWebhookChannel(api, "appteam", "wh-ch")
	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))
	var deleted string
	api.On("KVDelete", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvWebhookRequestPrefix) })).
		Run(func(args mock.Arguments) { deleted = args.String(0) }).Return(nil)

	_, err := p.submitWebhookRequest("u_req", webhookTypeIncoming, "ch1", "Alerts", "", "")

	require.EqualError(t, err, "could not send your request to the approvers; try again or contact a System Admin")
	require.NotEmpty(t, deleted)
}

func TestStoreBotRequest_KVFailure(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVSet", kvBotRequestPrefix+"r1", mock.Anything).Return(testAppErr("kv down"))
	err := p.storeBotRequest(&botRequest{ID: "r1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to store bot request")
}

func TestLoadBotRequest_Failures(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvBotRequestPrefix+"err").Return(nil, testAppErr("kv down"))
	api.On("KVGet", kvBotRequestPrefix+"bad").Return([]byte("{not json"), nil)

	req, raw, err := p.loadBotRequest("err")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to load bot request")
	require.Nil(t, req)
	require.Nil(t, raw)

	req, raw, err = p.loadBotRequest("bad")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to unmarshal bot request")
	require.Nil(t, req)
	require.Nil(t, raw)
}

func TestStoreWebhookRequest_KVFailure(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVSet", kvWebhookRequestPrefix+"r1", mock.Anything).Return(testAppErr("kv down"))
	err := p.storeWebhookRequest(&webhookRequest{ID: "r1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to store webhook request")
}

func TestLoadWebhookRequest_Failures(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvWebhookRequestPrefix+"err").Return(nil, testAppErr("kv down"))
	api.On("KVGet", kvWebhookRequestPrefix+"bad").Return([]byte("{not json"), nil)

	_, _, err := p.loadWebhookRequest("err")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to load webhook request")

	_, _, err = p.loadWebhookRequest("bad")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to unmarshal webhook request")
}

func TestBotApprovalAttachment_OwnerAndDefaultWebhookNames(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetUser", "owner1").Return(&model.User{Id: "owner1", Username: "owner"}, nil)
	api.On("GetChannel", mock.Anything).Return(nil, testAppErr("missing"))

	att := p.botApprovalAttachment(&botRequest{
		ID: "r1", Username: "ci-bot", DisplayName: "CI Bot", OwnerUserID: "owner1",
		IncomingWebhookChannelID: "in-ch", OutgoingWebhookChannelID: "out-ch",
		OutgoingWebhookCallbackURL: "https://example.com/hook",
	}, &model.User{Username: "reqer"})

	require.Contains(t, att.Text, "**Owner:** @owner")
	require.Contains(t, att.Text, "**Incoming webhook:** incoming webhook → in-ch")
	require.Contains(t, att.Text, "**Outgoing webhook:** outgoing webhook → out-ch")
}

func TestBotApprovalAttachmentWithNotice_PrependsNotice(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	req := &botRequest{ID: "r1", Username: "ci-bot", DisplayName: "CI Bot"}
	requester := &model.User{Username: "reqer"}

	att := p.botApprovalAttachmentWithNotice(req, requester, "token could not be sent")
	require.GreaterOrEqual(t, len(att.Fields), 2)
	require.Equal(t, "#D24B4E", att.Color)
	require.Equal(t, "⚠️ Action needed", att.Fields[0].Title)
	require.Equal(t, "token could not be sent", att.Fields[0].Value)
	require.Equal(t, "Requested by", att.Fields[1].Title)

	att = p.botApprovalAttachmentWithTitledNotice(req, requester, "", "blank title")
	require.NotEmpty(t, att.Fields)
	require.Equal(t, "⚠️ Action needed", att.Fields[0].Title)

	att = p.botApprovalAttachmentWithTitledNotice(req, requester, "Heads up", "custom")
	require.NotEmpty(t, att.Fields)
	require.Equal(t, "Heads up", att.Fields[0].Title)
	require.Equal(t, "custom", att.Fields[0].Value)
}

func TestWebhookApprovalAttachmentWithNotice_PrependsNotice(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	req := &webhookRequest{ID: "r1", WebhookType: webhookTypeIncoming, DisplayName: "Alerts"}
	requester := &model.User{Username: "reqer"}
	channel := &model.Channel{Name: "alerts"}

	att := p.webhookApprovalAttachmentWithNotice(req, requester, channel, "URL could not be sent")
	require.GreaterOrEqual(t, len(att.Fields), 2)
	require.Equal(t, "#D24B4E", att.Color)
	require.Equal(t, "⚠️ Action needed", att.Fields[0].Title)
	require.Equal(t, "URL could not be sent", att.Fields[0].Value)
	require.Equal(t, "Requested by", att.Fields[1].Title)

	att = p.webhookApprovalAttachmentWithTitledNotice(req, requester, channel, "", "blank title")
	require.NotEmpty(t, att.Fields)
	require.Equal(t, "⚠️ Action needed", att.Fields[0].Title)

	att = p.webhookApprovalAttachmentWithTitledNotice(req, requester, channel, "Heads up", "custom")
	require.NotEmpty(t, att.Fields)
	require.Equal(t, "Heads up", att.Fields[0].Title)
}

func TestHookIDFromPublicURL_Cases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://chat.example.com/hooks/abc123", "abc123"},
		{"https://chat.example.com/hooks/abc123 ", "abc123"},
		{"https://chat.example.com/hooks/", ""},
		{"https://chat.example.com/hooks/abc/extra", ""},
		{"https://chat.example.com/hooks/abc?x=1", ""},
		{"https://chat.example.com/hooks/abc#frag", ""},
		{"https://chat.example.com/plugins/whatever", ""},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, hookIDFromPublicURL(tc.in), tc.in)
	}
}

func TestInternalBaseURL_ListenAddressVariants(t *testing.T) {
	const site = "https://chat.example.com"
	tls := model.ConnSecurityTLS
	cases := []struct {
		name   string
		listen string
		tls    bool
		want   string
	}{
		{"unparseable address uses site URL", "not-an-address", false, site},
		{"blank port uses site URL", "localhost:", false, site},
		{"blank host becomes localhost", ":8065", false, "http://localhost:8065"},
		{"wildcard IPv4 becomes localhost", "0.0.0.0:8065", false, "http://localhost:8065"},
		{"wildcard IPv6 becomes localhost", "[::]:8065", false, "http://localhost:8065"},
		{"named host kept", "mm.internal:8065", false, "http://mm.internal:8065"},
		{"TLS on loopback uses site URL", ":8443", true, site},
		{"TLS on raw IP uses site URL", "10.0.0.5:8443", true, site},
		{"TLS on named host kept", "mm.internal:8443", true, "https://mm.internal:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			cfg := &model.Config{}
			cfg.ServiceSettings.SiteURL = model.NewPointer(site)
			cfg.ServiceSettings.ListenAddress = model.NewPointer(tc.listen)
			if tc.tls {
				cfg.ServiceSettings.ConnectionSecurity = &tls
			}
			api.On("GetConfig").Return(cfg)
			require.Equal(t, tc.want, p.internalBaseURL())
		})
	}
}

func TestDeleteWebhook_Paths(t *testing.T) {
	t.Run("blank hook ID is a no-op", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		require.NoError(t, p.deleteWebhook(webhookTypeIncoming, "  ", "admin1"))
		require.Empty(t, api.Calls)
	})

	t.Run("override is used when set", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		var gotType, gotID string
		p.deleteWebhookFn = func(webhookType, hookID string) error {
			gotType, gotID = webhookType, hookID
			return nil
		}
		require.NoError(t, p.deleteWebhook(webhookTypeOutgoing, "hid", "admin1"))
		require.Equal(t, webhookTypeOutgoing, gotType)
		require.Equal(t, "hid", gotID)
		require.Empty(t, api.Calls)
	})

	t.Run("no site URL", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetConfig").Return(&model.Config{})
		err := p.deleteWebhook(webhookTypeIncoming, "hid", "admin1")
		require.Error(t, err)
		require.Contains(t, err.Error(), "site URL is not configured")
		api.AssertNotCalled(t, "CreateSession", mock.Anything)
	})

	t.Run("session failure", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubSiteURL(api, "http://localhost:8065")
		api.On("GetUser", "admin1").Return(nil, testAppErr("no user"))
		err := p.deleteWebhook(webhookTypeIncoming, "hid", "admin1")
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to create admin session for webhook deletion")
	})

	t.Run("incoming hook deleted and session revoked", func(t *testing.T) {
		srv := &restServer{status: http.StatusOK}
		ts := srv.start(t)
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubSiteURL(api, ts.URL)
		stubApproverSession(api)
		require.NoError(t, p.deleteWebhook(webhookTypeIncoming, "hid", "admin1"))
		require.Equal(t, []string{"DELETE /api/v4/hooks/incoming/hid"}, srv.seen())
		api.AssertCalled(t, "RevokeSession", "sess1")
	})

	t.Run("outgoing hook deleted", func(t *testing.T) {
		srv := &restServer{status: http.StatusOK}
		ts := srv.start(t)
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubSiteURL(api, ts.URL)
		stubApproverSession(api)
		require.NoError(t, p.deleteWebhook(webhookTypeOutgoing, "hid", "admin1"))
		require.Equal(t, []string{"DELETE /api/v4/hooks/outgoing/hid"}, srv.seen())
	})

	t.Run("unknown type makes no REST call", func(t *testing.T) {
		srv := &restServer{status: http.StatusOK}
		ts := srv.start(t)
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubSiteURL(api, ts.URL)
		stubApproverSession(api)
		require.NoError(t, p.deleteWebhook("sideways", "hid", "admin1"))
		require.Empty(t, srv.seen())
		api.AssertCalled(t, "RevokeSession", "sess1")
	})

	for _, webhookType := range []string{webhookTypeIncoming, webhookTypeOutgoing} {
		t.Run(webhookType+" REST failure is returned and revoke failure logged", func(t *testing.T) {
			srv := &restServer{status: http.StatusForbidden, body: `{"id":"api.context.permissions.app_error","message":"no","status_code":403}`}
			ts := srv.start(t)
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)
			stubSiteURL(api, ts.URL)
			api.On("GetUser", "admin1").Return(&model.User{Id: "admin1"}, nil)
			api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
			api.On("RevokeSession", "sess1").Return(testAppErr("revoke failed"))
			err := p.deleteWebhook(webhookType, "hid", "admin1")
			require.Error(t, err)
			require.Contains(t, err.Error(), "failed to delete "+webhookType+" webhook")
			api.AssertCalled(t, "LogWarn", "failed to revoke admin session after webhook op", "error", mock.Anything)
		})
	}
}

func TestCreateBotToken_Paths(t *testing.T) {
	t.Run("no site URL", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetConfig").Return(&model.Config{})
		_, _, err := p.createBotToken("tok", "bot1", "desc")
		require.EqualError(t, err, "SiteURL not configured")
	})

	t.Run("token returned", func(t *testing.T) {
		srv := &restServer{status: http.StatusOK, body: `{"id":"t1","token":"secret-value","user_id":"bot1"}`}
		ts := srv.start(t)
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubSiteURL(api, ts.URL)
		token, _, err := p.createBotToken("tok", "bot1", "desc")
		require.NoError(t, err)
		require.Equal(t, "secret-value", token)
		require.Equal(t, []string{"POST /api/v4/users/bot1/tokens"}, srv.seen())
	})

	t.Run("REST failure", func(t *testing.T) {
		srv := &restServer{status: http.StatusForbidden, body: `{"id":"x","message":"no","status_code":403}`}
		ts := srv.start(t)
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubSiteURL(api, ts.URL)
		_, _, err := p.createBotToken("tok", "bot1", "desc")
		require.Error(t, err)
		require.Contains(t, err.Error(), "create token")
	})
}

func TestCreateBotToken_Lifetime(t *testing.T) {
	for name, tc := range map[string]struct {
		days     int
		wantDays int
	}{
		"default of one year": {days: 365, wantDays: 365},
		"custom lifetime":     {days: 30, wantDays: 30},
		"never expires":       {days: 0},
	} {
		t.Run(name, func(t *testing.T) {
			var sent model.UserAccessToken
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(mustMarshal(t, model.UserAccessToken{Id: "t1", Token: "secret-value", ExpiresAt: sent.ExpiresAt}))
			}))
			t.Cleanup(ts.Close)
			api := &plugintest.API{}
			p := newTestPlugin(api)
			p.getConfiguration().tokenLifetimeDays = tc.days
			stubSiteURL(api, ts.URL)

			before := time.Now()
			_, expiresAt, err := p.createBotToken("tok", "bot1", "desc")

			require.NoError(t, err)
			require.Equal(t, sent.ExpiresAt, expiresAt)
			if tc.wantDays == 0 {
				require.Zero(t, sent.ExpiresAt)
				return
			}
			want := before.AddDate(0, 0, tc.wantDays)
			require.WithinDuration(t, want, time.UnixMilli(sent.ExpiresAt), time.Minute)
		})
	}
}

func TestScheduleTokenPostDeletion_AppendsAfterRetry(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	existing := mustMarshal(t, []tokenPostEntry{{PostID: "old", ExpiresAt: time.Now().Add(time.Hour).Unix()}})
	api.On("KVGet", tokenPostRegistryKey).Return(existing, nil)
	api.On("KVCompareAndSet", tokenPostRegistryKey, existing, registryHolds("old", "new")).
		Return(false, testAppErr("busy")).Once()
	api.On("KVCompareAndSet", tokenPostRegistryKey, existing, registryHolds("old", "new")).
		Return(true, nil).Once()

	p.scheduleTokenPostDeletion("new")

	api.AssertNumberOfCalls(t, "KVCompareAndSet", 2)
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
}

func TestCleanupExpiredTokenPosts_NothingToDo(t *testing.T) {
	cases := map[string]func(api *plugintest.API){
		"read failure":     func(api *plugintest.API) { api.On("KVGet", tokenPostRegistryKey).Return(nil, testAppErr("kv down")) },
		"empty registry":   func(api *plugintest.API) { api.On("KVGet", tokenPostRegistryKey).Return(nil, nil) },
		"unreadable entry": func(api *plugintest.API) { api.On("KVGet", tokenPostRegistryKey).Return([]byte("{bad"), nil) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			setup(api)
			p.cleanupExpiredTokenPosts()
			api.AssertNotCalled(t, "DeletePost", mock.Anything)
			api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestCleanupExpiredTokenPosts_DeletesExpiredAndKeepsLive(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	now := time.Now()
	raw := mustMarshal(t, []tokenPostEntry{
		{PostID: "expired", ExpiresAt: now.Add(-time.Hour).Unix()},
		{PostID: "live", ExpiresAt: now.Add(time.Hour).Unix()},
	})
	api.On("KVGet", tokenPostRegistryKey).Return(raw, nil)
	api.On("DeletePost", "expired").Return(testAppErr("already gone"))
	// A write error, then a lost race, then success: three passes in total.
	api.On("KVCompareAndSet", tokenPostRegistryKey, raw, registryHolds("live")).Return(false, testAppErr("busy")).Once()
	api.On("KVCompareAndSet", tokenPostRegistryKey, raw, registryHolds("live")).Return(false, nil).Once()
	api.On("KVCompareAndSet", tokenPostRegistryKey, raw, registryHolds("live")).Return(true, nil).Once()

	p.cleanupExpiredTokenPosts()

	api.AssertNumberOfCalls(t, "KVCompareAndSet", 3)
	api.AssertCalled(t, "DeletePost", "expired")
	api.AssertNotCalled(t, "DeletePost", "live")
	api.AssertCalled(t, "LogWarn", "cleanup: failed to delete expired token post", "post_id", "expired", "error", mock.Anything)
}

func TestCleanupExpiredTokenPosts_TimerDeletesAndUnregisters(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	raw := mustMarshal(t, []tokenPostEntry{{PostID: "soon", ExpiresAt: time.Now().Unix() + 2}})
	api.On("KVGet", tokenPostRegistryKey).Return(raw, nil)
	api.On("KVCompareAndSet", tokenPostRegistryKey, raw, registryHolds("soon")).Return(true, nil).Once()
	api.On("DeletePost", "soon").Return(testAppErr("already gone"))
	removed := make(chan struct{})
	api.On("KVCompareAndSet", tokenPostRegistryKey, raw, registryHolds()).
		Run(func(mock.Arguments) { close(removed) }).Return(true, nil).Once()

	p.cleanupExpiredTokenPosts()

	waitFor(t, removed, "timer to remove the post from the registry")
	api.AssertCalled(t, "DeletePost", "soon")
}

func TestRemoveTokenPostFromRegistry_Paths(t *testing.T) {
	t.Run("filters the post out", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		raw := mustMarshal(t, []tokenPostEntry{{PostID: "a"}, {PostID: "b"}})
		api.On("KVGet", tokenPostRegistryKey).Return(raw, nil)
		api.On("KVCompareAndSet", tokenPostRegistryKey, raw, registryHolds("b")).Return(true, nil).Once()
		p.removeTokenPostFromRegistry("a")
		api.AssertNumberOfCalls(t, "KVCompareAndSet", 1)
	})

	t.Run("gives up after three failed writes", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("KVGet", tokenPostRegistryKey).Return(nil, nil)
		api.On("KVCompareAndSet", tokenPostRegistryKey, []byte(nil), mock.MatchedBy(func(data []byte) bool {
			return bytes.Equal(data, []byte("[]"))
		})).Return(false, testAppErr("busy")).Once()
		api.On("KVCompareAndSet", tokenPostRegistryKey, []byte(nil), mock.Anything).Return(false, nil)
		p.removeTokenPostFromRegistry("a")
		api.AssertNumberOfCalls(t, "KVCompareAndSet", 3)
	})
}
