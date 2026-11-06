package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	forensicBot   = model.NewId()
	forensicOther = model.NewId()
)

const (
	forensicSince = int64(1_000_000)
	forensicUntil = int64(2_000_000)
)

// forensicPlugin is auditExportPlugin with an indexed, active bot that is
// in team t1's channels c1 (and a DM, which is skipped), whose home is c1
// and whose incoming webhook posts to c2, a channel it left.
func forensicPlugin(t *testing.T) (*Plugin, *plugintest.API, map[string][]byte) {
	t.Helper()
	p, api, store := auditExportPlugin(t)
	store[kvBotIndexPrefix+forensicBot] = recordJSON(t, botIndexEntry{
		BotUserID: forensicBot, RequestID: "r1", Username: "alpha", RequesterID: "u_req", CreatedAt: 100,
		HostingEnv: "mattermost-prod", HomeTeamID: "t1", HomeChannelID: "c1",
	})
	store[kvHistoryPrefix+"r1"] = recordJSON(t, requestHistory{
		RequestID: "r1", Kind: pendingKindBot, RequesterID: "u_req", Status: historyApproved,
		Request: recordJSON(t, botRequest{ID: "r1", HomeChannelID: "c1", IncomingWebhookChannelID: "c2"}),
		Events:  []auditEvent{{At: 1, Event: eventSubmitted, Outcome: "success"}},
	})
	api.On("GetBot", forensicBot, true).Return(&model.Bot{UserId: forensicBot, Username: "alpha"}, nil).Maybe()
	api.On("GetUser", forensicBot).Return(&model.User{Id: forensicBot, Username: "alpha", IsBot: true}, nil).Maybe()
	api.On("GetUserByUsername", "alpha").Return(&model.User{Id: forensicBot, Username: "alpha", IsBot: true}, nil).Maybe()
	api.On("GetUserByUsername", "sec").Return(&model.User{Id: "u_sec", Username: "sec"}, nil).Maybe()
	api.On("GetTeamsForUser", forensicBot).Return([]*model.Team{{Id: "t1", Name: "ops", DisplayName: "Ops"}}, nil).Maybe()
	api.On("GetChannelsForTeamForUser", "t1", forensicBot, true).Return([]*model.Channel{
		{Id: "c1", TeamId: "t1", Name: "alerts", DisplayName: "Alerts", Type: model.ChannelTypeOpen},
		{Id: "dm", Name: "a__b", Type: model.ChannelTypeDirect},
	}, nil).Maybe()
	api.On("GetChannel", "c2").Return(&model.Channel{Id: "c2", TeamId: "t1", Name: "hooks", Type: model.ChannelTypePrivate, DeleteAt: 5}, nil).Maybe()
	api.On("GetFileInfo", "f1").Return(&model.FileInfo{Id: "f1", Name: "log.txt", Size: 12, MimeType: "text/plain"}, nil).Maybe()
	api.On("GetFileInfo", "f2").Return(nil, testAppErr("gone")).Maybe()
	return p, api, store
}

// postsPage is a page of GetPostsForChannel, newest first.
func postsPage(posts ...*model.Post) *model.PostList {
	list := model.NewPostList()
	for _, post := range posts {
		list.AddPost(post)
		list.AddOrder(post.Id)
	}
	return list
}

func TestBuildBotForensic(t *testing.T) {
	p, api, _ := forensicPlugin(t)
	api.On("GetPostsForChannel", "c1", 0, forensicPageSize).Return(postsPage(
		&model.Post{Id: "late", UserId: forensicBot, ChannelId: "c1", CreateAt: forensicUntil + 1},
		&model.Post{Id: "p2", UserId: forensicBot, ChannelId: "c1", CreateAt: 1_500_000, Message: "second", FileIds: []string{"f1", "f2"}},
		&model.Post{Id: "human", UserId: "u_req", ChannelId: "c1", CreateAt: 1_400_000},
		&model.Post{Id: "p1", UserId: forensicBot, ChannelId: "c1", CreateAt: 1_100_000, Message: "first"},
		&model.Post{Id: "early", UserId: forensicBot, ChannelId: "c1", CreateAt: forensicSince - 1},
	), nil).Once()
	api.On("GetPostsForChannel", "c2", 0, forensicPageSize).Return(postsPage(
		&model.Post{Id: "hook", UserId: forensicBot, ChannelId: "c2", CreateAt: 1_200_000, Message: "from the webhook"},
	), nil).Once()

	fx, err := p.buildBotForensic(forensicBot, forensicSince, forensicUntil, 42)

	require.NoError(t, err)
	require.True(t, fx.RHManaged)
	require.Equal(t, "r1", fx.Index.RequestID)
	require.Equal(t, "r1", fx.History.RequestID)
	require.Equal(t, botStatusActive, fx.BotStatus)
	require.Len(t, fx.Teams, 1)
	require.Equal(t, []forensicChannel{{ID: "c1", TeamID: "t1", Name: "alerts", DisplayName: "Alerts", Type: "O", Why: "member"}}, fx.Teams[0].Channels, "a DM isn't listed")
	require.Equal(t, []forensicChannel{
		{ID: "c1", TeamID: "t1", Name: "alerts", DisplayName: "Alerts", Type: "O", Why: "member"},
		{ID: "c2", TeamID: "t1", Name: "hooks", Type: "P", Archived: true, Why: "webhook"},
	}, fx.Scanned, "the home channel is scanned once, and a webhook channel the bot left is scanned too")
	ids := make([]string, len(fx.Posts))
	for i, post := range fx.Posts {
		ids[i] = post.ID
	}
	require.Equal(t, []string{"p1", "hook", "p2"}, ids, "the bot's posts in the range, oldest first")
	require.Equal(t, 3, fx.PostCount)
	require.Equal(t, []forensicFile{
		{ID: "f1", PostID: "p2", Name: "log.txt", Size: 12, MimeType: "text/plain"},
		{ID: "f2", PostID: "p2", Missing: true},
	}, fx.Files)
	require.False(t, fx.Truncated)
	require.Equal(t, forensicLimitations, fx.Limitations)
}

func TestBuildBotForensic_Pages(t *testing.T) {
	p, api, _ := forensicPlugin(t)
	full := make([]*model.Post, forensicPageSize)
	for i := range full {
		full[i] = &model.Post{Id: model.NewId(), UserId: "u_req", ChannelId: "c1", CreateAt: forensicUntil - int64(i)}
	}
	api.On("GetPostsForChannel", "c1", 0, forensicPageSize).Return(postsPage(full...), nil).Once()
	api.On("GetPostsForChannel", "c1", 1, forensicPageSize).Return(postsPage(
		&model.Post{Id: "p1", UserId: forensicBot, ChannelId: "c1", CreateAt: 1_500_000},
	), nil).Once()
	api.On("GetPostsForChannel", "c2", 0, forensicPageSize).Return(postsPage(), nil).Once()

	fx, err := p.buildBotForensic(forensicBot, forensicSince, forensicUntil, 42)

	require.NoError(t, err)
	require.Equal(t, 1, fx.PostCount, "a full page reads the next one")
}

func TestBuildBotForensic_PostCap(t *testing.T) {
	p, api, _ := forensicPlugin(t)
	page := func(n int) *model.PostList {
		posts := make([]*model.Post, forensicPageSize)
		for i := range posts {
			posts[i] = &model.Post{Id: model.NewId(), UserId: forensicBot, ChannelId: "c1", CreateAt: forensicUntil - int64(n*forensicPageSize+i)}
		}
		return postsPage(posts...)
	}
	for n := range forensicMaxPosts / forensicPageSize {
		api.On("GetPostsForChannel", "c1", n, forensicPageSize).Return(page(n), nil).Once()
	}

	fx, err := p.buildBotForensic(forensicBot, forensicSince, forensicUntil, 42)

	require.NoError(t, err)
	require.Equal(t, forensicMaxPosts, fx.PostCount)
	require.True(t, fx.Truncated)
	require.Equal(t, "reached 5000 posts", fx.TruncatedReason)
	api.AssertNotCalled(t, "GetPostsForChannel", "c2", 0, forensicPageSize)
}

func TestBuildBotForensic_SizeCap(t *testing.T) {
	p, api, _ := forensicPlugin(t)
	big := strings.Repeat("x", 8<<20)
	api.On("GetPostsForChannel", "c1", 0, forensicPageSize).Return(postsPage(
		&model.Post{Id: "a", UserId: forensicBot, ChannelId: "c1", CreateAt: 1_900_000, Message: big},
		&model.Post{Id: "b", UserId: forensicBot, ChannelId: "c1", CreateAt: 1_800_000, Message: big},
		&model.Post{Id: "c", UserId: forensicBot, ChannelId: "c1", CreateAt: 1_700_000, Message: big},
	), nil).Once()

	fx, err := p.buildBotForensic(forensicBot, forensicSince, forensicUntil, 42)

	require.NoError(t, err)
	require.Equal(t, 2, fx.PostCount)
	require.True(t, fx.Truncated)
	require.Equal(t, "reached 20 MB", fx.TruncatedReason)
}

func TestBuildBotForensic_NotRHManaged(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	fakeKV(api)
	api.On("GetBot", forensicOther, true).Return(&model.Bot{UserId: forensicOther, Username: "outside", DeleteAt: 7}, nil)
	api.On("GetTeamsForUser", forensicOther).Return([]*model.Team{}, nil)

	fx, err := p.buildBotForensic(forensicOther, forensicSince, forensicUntil, 42)

	require.NoError(t, err)
	require.False(t, fx.RHManaged)
	require.Nil(t, fx.Index)
	require.Nil(t, fx.History)
	require.Equal(t, botStatusDeactivated, fx.BotStatus)
	require.Empty(t, fx.Scanned)
	require.NotNil(t, fx.Posts, "posts is an empty list, not null")
}

func TestBuildBotForensic_GoneBot(t *testing.T) {
	notFound := model.NewAppError("GetBot", "not_found", nil, "", http.StatusNotFound)

	t.Run("indexed", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLines(api)
		p := newTestPlugin(api)
		store := fakeKV(api)
		store[kvBotIndexPrefix+forensicBot] = recordJSON(t, botIndexEntry{BotUserID: forensicBot, Username: "alpha"})
		api.On("GetBot", forensicBot, true).Return(nil, notFound)

		fx, err := p.buildBotForensic(forensicBot, forensicSince, forensicUntil, 42)

		require.NoError(t, err)
		require.Equal(t, botStatusDeleted, fx.BotStatus)
		require.Nil(t, fx.Bot)
		api.AssertNotCalled(t, "GetTeamsForUser", mock.Anything)
	})

	t.Run("unknown", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		fakeKV(api)
		api.On("GetBot", forensicOther, true).Return(nil, notFound)

		_, err := p.buildBotForensic(forensicOther, forensicSince, forensicUntil, 42)

		require.ErrorContains(t, err, "there is no bot with that ID")
	})
}

func TestBuildBotForensic_Window(t *testing.T) {
	p, _, _ := forensicPlugin(t)
	_, err := p.buildBotForensic(forensicBot, 0, forensicUntil, 1)
	require.ErrorContains(t, err, "needs a start date before its end date")
	_, err = p.buildBotForensic(forensicBot, forensicUntil, forensicSince, 1)
	require.ErrorContains(t, err, "needs a start date before its end date")
	_, err = p.buildBotForensic(forensicBot, 1, 1+forensicMaxWindow.Milliseconds()+1, 1)
	require.ErrorContains(t, err, "covers at most 90 days")
}

func TestCheckExportScope_Forensic(t *testing.T) {
	day := (24 * time.Hour).Milliseconds()
	ok := auditExportFilter{Type: exportBotForensic, Format: "json", BotID: forensicBot, Since: day, Until: 91 * day}
	require.NoError(t, checkExportFilter(ok))

	for name, tc := range map[string]struct {
		edit func(f *auditExportFilter)
		want string
	}{
		"no bot":         {func(f *auditExportFilter) { f.BotID = "" }, "name the bot"},
		"bad bot":        {func(f *auditExportFilter) { f.BotID = "nope" }, "the bot isn't valid"},
		"no start":       {func(f *auditExportFilter) { f.Since = 0 }, "needs a start and an end date"},
		"no end":         {func(f *auditExportFilter) { f.Until = 0 }, "needs a start and an end date"},
		"too long":       {func(f *auditExportFilter) { f.Until = 92 * day }, "covers at most 90 days"},
		"env":            {func(f *auditExportFilter) { f.HostingEnv = "prod" }, "narrowed only by the bot and dates"},
		"requester":      {func(f *auditExportFilter) { f.Requester = forensicOther }, "narrowed only by the bot and dates"},
		"kind":           {func(f *auditExportFilter) { f.Kind = pendingKindBot }, "narrowed only by the bot and dates"},
		"pdf":            {func(f *auditExportFilter) { f.Format = "pdf" }, "pick JSON for bot forensics"},
		"bot, histories": {func(f *auditExportFilter) { *f = auditExportFilter{Format: "json", BotID: forensicBot} }, "only a bot forensic export names a bot"},
		"bot, inventory": {func(f *auditExportFilter) {
			*f = auditExportFilter{Type: exportBotInventory, Format: "pdf", BotID: forensicBot}
		}, "only a bot forensic export names a bot"},
	} {
		t.Run(name, func(t *testing.T) {
			f := ok
			tc.edit(&f)
			require.ErrorContains(t, checkExportFilter(f), tc.want)
		})
	}

	a := &auditExportRequest{Filter: ok}
	b := &auditExportRequest{Filter: ok}
	b.Filter.BotID = forensicOther
	require.NotEqual(t, a.pendingKey(), b.pendingKey(), "forensics of two bots are two requests")
}

func TestDescribeExportFilter_Forensic(t *testing.T) {
	p, _, _ := forensicPlugin(t)
	day := (24 * time.Hour).Milliseconds()
	require.Equal(t, "Only posts by @alpha, made from January 2, 1970 to January 4, 1970",
		p.describeExportFilter(auditExportFilter{Type: exportBotForensic, BotID: forensicBot, Since: day, Until: 3 * day}))
}

func TestResolveExportBot(t *testing.T) {
	p, api, _ := forensicPlugin(t)
	api.On("GetUserByUsername", "nobody").Return(nil, testAppErr("not found"))

	id, err := p.resolveExportBot(" @alpha ")
	require.NoError(t, err)
	require.Equal(t, forensicBot, id)
	id, err = p.resolveExportBot("")
	require.NoError(t, err)
	require.Empty(t, id)
	_, err = p.resolveExportBot("sec")
	require.ErrorContains(t, err, "@sec is not a bot")
	_, err = p.resolveExportBot("nobody")
	require.ErrorContains(t, err, "could not find a bot named @nobody")
}

func TestDecideAuditExport_ApproveSendsTheForensics(t *testing.T) {
	p, api, store := forensicPlugin(t)
	events := captureAudit(p)
	req := storedExportRequest(t, store, auditExportFilter{Type: exportBotForensic, Format: "json", BotID: forensicBot, Since: forensicSince, Until: forensicUntil})
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil).Maybe()
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
	updatedPosts(api)
	api.On("GetPostsForChannel", "c1", 0, forensicPageSize).Return(postsPage(
		&model.Post{Id: "p1", UserId: forensicBot, ChannelId: "c1", CreateAt: 1_100_000, Message: "secret text"},
	), nil)
	api.On("GetPostsForChannel", "c2", 0, forensicPageSize).Return(postsPage(), nil)
	var uploaded []byte
	api.On("UploadFile", mock.Anything, "dm1", mock.MatchedBy(func(name string) bool {
		return strings.HasPrefix(name, "request-hub-bot-forensic-") && strings.HasSuffix(name, ".json")
	})).Run(func(args mock.Arguments) { uploaded = args.Get(0).([]byte) }).Return(&model.FileInfo{Id: "file1"}, nil)
	var delivered *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		if post := args.Get(0).(*model.Post); len(post.FileIds) > 0 {
			delivered = post
		}
	}).Return(&model.Post{Id: "p"}, nil)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+req.ID)

	require.Contains(t, resp.Text, "Audit export (JSON, 1 post) sent.")
	require.NotNil(t, delivered)
	require.Contains(t, delivered.Message, "**Audit export: Bot forensics** (JSON, 1 post). Only posts by @alpha")
	var fx forensicExport
	require.NoError(t, json.Unmarshal(uploaded, &fx))
	require.Equal(t, forensicBot, fx.BotID)
	require.Equal(t, "secret text", fx.Posts[0].Message)

	var approved *auditEntry
	for i := range *events {
		if (*events)[i].Event == eventApproved {
			approved = &(*events)[i]
		}
	}
	require.NotNil(t, approved)
	require.Equal(t, 1, approved.Detail["posts"])
	require.Equal(t, false, approved.Detail["truncated"])
	require.Equal(t, forensicBot, approved.Detail["bot_id"])
	require.Equal(t, exportBotForensic, approved.Detail["type"])
	raw, err := json.Marshal(approved)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret text", "the audit event never holds post content")
}

func TestAuditExportSlash_Forensic(t *testing.T) {
	p, api, store := forensicPlugin(t)
	c := *p.getConfiguration()
	c.ApprovalTeam = "appteam"
	p.setConfiguration(&c)
	mockAuditCards(api)
	api.On("GetUsers", mock.Anything).Return([]*model.User{sysadmin("admin1")}, nil)
	api.On("GetDirectChannel", "u_sec", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: model.NewId()}, nil)

	resp := runCommandAs(p, "u_sec", "/mattermost-request-hub audit export bot-forensic --bot @alpha --since 2026-01-01 --until 2026-03-01 --reason incident")

	require.Contains(t, resp.Text, "has been sent to the System Admins")
	var stored *auditExportRequest
	for key, value := range store {
		if strings.HasPrefix(key, kvAuditExportRequestPrefix) {
			stored = &auditExportRequest{}
			require.NoError(t, json.Unmarshal(value, stored))
		}
	}
	require.NotNil(t, stored)
	require.Equal(t, exportBotForensic, stored.Filter.Type)
	require.Equal(t, "json", stored.Filter.Format)
	require.Equal(t, forensicBot, stored.Filter.BotID)

	resp = runCommandAs(p, "u_sec", "/mattermost-request-hub audit export bot-forensic --bot @alpha --reason incident")
	require.Contains(t, resp.Text, "needs a start and an end date")
	resp = runCommandAs(p, "u_sec", "/mattermost-request-hub audit export bot-forensic --bot @sec --since 2026-01-01 --until 2026-01-02 --reason x")
	require.Contains(t, resp.Text, "@sec is not a bot")

	withDirectExport(p)
	resp = runCommandAs(p, "admin1", "/mattermost-request-hub audit export bot_forensic --bot @alpha --since 2026-01-01 --until 2026-01-31")
	require.Contains(t, resp.Text, "**Download the audit export: Bot forensics**")
	require.Contains(t, resp.Text, "type=bot_forensic&bot="+forensicBot)
	require.Contains(t, resp.Text, "format=json")
}

func TestHandleAuditExport_Forensic(t *testing.T) {
	p, api, _ := forensicPlugin(t)
	events := captureAudit(p)
	withDirectExport(p)
	api.On("GetPostsForChannel", mock.Anything, 0, forensicPageSize).Return(postsPage(), nil)

	w := exportRequest(t, p, "admin1", "?type=bot_forensic&bot="+forensicBot+"&since=1000000&until=2000000")

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	var fx forensicExport
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fx))
	require.Equal(t, forensicBot, fx.BotID)
	require.Equal(t, 1, countEvents(*events, eventAuditExported))
	last := (*events)[len(*events)-1]
	require.Equal(t, 0, last.Detail["posts"])
	require.Equal(t, 2, last.Detail["channels_scanned"])

	require.Equal(t, http.StatusBadRequest, exportRequest(t, p, "admin1", "?type=bot_forensic&bot="+forensicBot).Code)
	require.Equal(t, http.StatusBadRequest, exportRequest(t, p, "admin1", "?bot="+forensicBot).Code)
}

func TestSubmitAuditExportWebapp_Forensic(t *testing.T) {
	p, api, store := forensicPlugin(t)
	c := *p.getConfiguration()
	c.ApprovalTeam = "appteam"
	p.setConfiguration(&c)
	mockAuditCards(api)
	api.On("GetUsers", mock.Anything).Return([]*model.User{sysadmin("admin1")}, nil)
	api.On("GetDirectChannel", "u_sec", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: model.NewId()}, nil)

	post := func(body map[string]any) map[string]string {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPost, routeSubmitAuditExportWebapp, bytes.NewReader(raw))
		r.Header.Set(headerUserID, "u_sec")
		w := httptest.NewRecorder()
		p.ServeHTTP(nil, w, r)
		var out map[string]string
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		return out
	}

	body := map[string]any{"type": "bot_forensic", "format": "json", "bot": "@alpha", "since": forensicSince, "until": forensicUntil, "reason": "incident"}
	require.Contains(t, post(body)["message"], "has been sent to the System Admins")
	var stored auditExportRequest
	for key, value := range store {
		if strings.HasPrefix(key, kvAuditExportRequestPrefix) {
			require.NoError(t, json.Unmarshal(value, &stored))
		}
	}
	require.Equal(t, forensicBot, stored.Filter.BotID)

	body["bot"] = "sec"
	require.Contains(t, post(body)["error"], "@sec is not a bot")
}
