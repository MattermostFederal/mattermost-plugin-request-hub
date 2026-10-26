package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// expectDocsBookmarkAttempt expects a new request channel to try the docs
// bookmark. There is no SiteURL, so the attempt stops at the URL and logs.
func expectDocsBookmarkAttempt(api *plugintest.API) {
	api.On("GetConfig").Return(&model.Config{})
}

// bookmarkServer answers the channel bookmark REST calls. It lists existing
// and records each bookmark created.
type bookmarkServer struct {
	mu       sync.Mutex
	existing []*model.ChannelBookmarkWithFileInfo
	listErr  bool
	created  []model.ChannelBookmark
}

func (s *bookmarkServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		require.Equal(t, "/api/v4/channels/ch1/bookmarks", r.URL.Path)
		require.Equal(t, "tok1", strings.Fields(r.Header.Get("Authorization"))[1])
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if s.listErr {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"id":"x","message":"no","status_code":403}`))
				return
			}
			_ = json.NewEncoder(w).Encode(s.existing)
		case http.MethodPost:
			var b model.ChannelBookmark
			require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
			s.created = append(s.created, b)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(model.ChannelBookmarkWithFileInfo{ChannelBookmark: &b})
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// stubBotSession lets createUserSession run for the bot and the deferred
// revoke follow.
func stubBotSession(api *plugintest.API) {
	api.On("GetUser", "bot-user-id").Return(&model.User{Id: "bot-user-id", Roles: "system_user"}, nil)
	api.On("CreateSession", mock.MatchedBy(func(s *model.Session) bool { return s.UserId == "bot-user-id" })).
		Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
	api.On("RevokeSession", "sess1").Return(nil)
}

func TestDocsURL(t *testing.T) {
	for name, tc := range map[string]struct {
		siteURL string
		want    string
	}{
		"no SiteURL":     {"", ""},
		"SiteURL":        {"https://chat.example.com", "https://chat.example.com" + docsPath()},
		"trailing slash": {"https://chat.example.com/", "https://chat.example.com" + docsPath()},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			stubSiteURL(api, tc.siteURL)
			require.Equal(t, tc.want, p.docsURL())
		})
	}
}

func TestBookmarkDocs_JoinsBotAndCreatesBookmark(t *testing.T) {
	srv := &bookmarkServer{}
	ts := srv.start(t)
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	stubBotSession(api)
	api.On("GetChannelMember", "ch1", "bot-user-id").Return(nil, testAppErr("not a member"))
	api.On("GetTeamMember", "t1", "bot-user-id").Return(&model.TeamMember{TeamId: "t1", UserId: "bot-user-id"}, nil)
	api.On("AddChannelMember", "ch1", "bot-user-id").Return(&model.ChannelMember{}, nil)

	p.bookmarkDocs(&model.Channel{Id: "ch1", TeamId: "t1"})

	require.Len(t, srv.created, 1)
	b := srv.created[0]
	require.Equal(t, "ch1", b.ChannelId)
	require.Equal(t, docsBookmarkDisplayName, b.DisplayName)
	require.Equal(t, ts.URL+docsPath(), b.LinkUrl)
	require.Equal(t, model.ChannelBookmarkLink, b.Type)
}

func TestBookmarkDocs_SkipsChannelAlreadyBookmarked(t *testing.T) {
	srv := &bookmarkServer{}
	ts := srv.start(t)
	srv.existing = []*model.ChannelBookmarkWithFileInfo{{ChannelBookmark: &model.ChannelBookmark{LinkUrl: ts.URL + docsPath()}}}
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	stubBotSession(api)
	api.On("GetChannelMember", "ch1", "bot-user-id").Return(&model.ChannelMember{}, nil)

	p.bookmarkDocs(&model.Channel{Id: "ch1", TeamId: "t1"})

	require.Empty(t, srv.created)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestBookmarkDocs_FailuresAreLogged(t *testing.T) {
	t.Run("no SiteURL", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetConfig").Return(&model.Config{})
		api.On("LogWarn", "failed to bookmark the docs in a request channel",
			"channel_id", "ch1", "error", "SiteURL is not configured").Once()

		p.bookmarkDocs(&model.Channel{Id: "ch1", TeamId: "t1"})
	})

	t.Run("bot can't join the channel", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		stubSiteURL(api, "https://chat.example.com")
		api.On("GetChannelMember", "ch1", "bot-user-id").Return(nil, testAppErr("not a member"))
		api.On("GetTeamMember", "t1", "bot-user-id").Return(&model.TeamMember{}, nil)
		api.On("AddChannelMember", "ch1", "bot-user-id").Return(nil, testAppErr("denied"))
		api.On("LogWarn", "failed to bookmark the docs in a request channel",
			"channel_id", "ch1", "error", mock.MatchedBy(func(e string) bool { return e != "" })).Once()

		p.bookmarkDocs(&model.Channel{Id: "ch1", TeamId: "t1"})

		api.AssertNotCalled(t, "CreateSession", mock.Anything)
	})

	t.Run("REST failure still revokes the session", func(t *testing.T) {
		srv := &bookmarkServer{listErr: true}
		ts := srv.start(t)
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		stubSiteURL(api, ts.URL)
		stubBotSession(api)
		api.On("GetChannelMember", "ch1", "bot-user-id").Return(&model.ChannelMember{}, nil)
		api.On("LogWarn", "failed to bookmark the docs in a request channel",
			"channel_id", "ch1", "error", mock.Anything).Once()

		p.bookmarkDocs(&model.Channel{Id: "ch1", TeamId: "t1"})

		require.Empty(t, srv.created)
	})

	t.Run("nil channel", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.bookmarkDocs(nil)
		api.AssertNotCalled(t, "GetConfig")
	})
}

func TestBookmarkDocsInExistingChannels(t *testing.T) {
	t.Run("already done", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("KVGet", kvDocsBookmarksMigrated).Return([]byte("1"), nil)

		p.bookmarkDocsInExistingChannels()

		api.AssertNotCalled(t, "GetConfig")
		api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
	})

	t.Run("waits for SiteURL", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("KVGet", kvDocsBookmarksMigrated).Return(nil, nil)
		api.On("GetConfig").Return(&model.Config{})

		p.bookmarkDocsInExistingChannels()

		api.AssertNotCalled(t, "GetTeams")
		api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
	})

	t.Run("bookmarks every request channel and marks done", func(t *testing.T) {
		srv := &bookmarkServer{}
		ts := srv.start(t)
		p, api := logOnlyPlugin(&configuration{})
		defer api.AssertExpectations(t)
		api.On("KVGet", kvDocsBookmarksMigrated).Return(nil, nil)
		stubSiteURL(api, ts.URL)
		stubBotSession(api)
		// No approval team, so only the per-team channel exists.
		api.On("GetTeams").Return([]*model.Team{{Id: "t1"}}, nil)
		api.On("GetChannelByName", "t1", "mattermost-channel-requests", false).
			Return(pluginChannelIn("ch1", "mattermost-channel-requests", "t1"), nil)
		api.On("GetChannelMember", "ch1", "bot-user-id").Return(&model.ChannelMember{}, nil)
		api.On("KVSet", kvDocsBookmarksMigrated, []byte("1")).Return(nil)

		p.bookmarkDocsInExistingChannels()

		require.Len(t, srv.created, 1)
	})
}
