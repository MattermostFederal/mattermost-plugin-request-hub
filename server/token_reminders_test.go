package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// stubTokenReminderJob lets the reminder job started by OnActivate take its
// cluster lock and find nothing to do.
func stubTokenReminderJob(api *plugintest.API) {
	isJobKey := mock.MatchedBy(func(key string) bool { return strings.HasSuffix(key, tokenReminderJobKey) })
	api.On("KVSetWithOptions", isJobKey, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	api.On("KVSet", isJobKey, mock.Anything).Return(nil).Maybe()
	api.On("KVList", mock.Anything, mock.Anything).Return([]string{}, nil).Maybe()
}

func TestTrackBotTokenExpiry_StoresDetailsButNotTheToken(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	var stored botTokenExpiry
	var raw []byte
	api.On("KVSet", botTokenExpiryKey("tok1"), mock.Anything).Run(func(args mock.Arguments) {
		raw = args.Get(1).([]byte)
		require.NoError(t, json.Unmarshal(raw, &stored))
	}).Return(nil)

	before := model.GetMillis()
	p.trackBotTokenExpiry(&model.UserAccessToken{Id: "tok1", Token: "secret-value", UserId: "bot1", ExpiresAt: 12345}, "mybot", "req1")

	require.Equal(t, botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: 12345, LastRemindedAt: stored.LastRemindedAt,
	}, stored)
	require.GreaterOrEqual(t, stored.LastRemindedAt, before)
	require.NotContains(t, string(raw), "secret-value")
}

func TestTrackBotTokenExpiry_SkipsTokensThatNeverExpire(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	p.trackBotTokenExpiry(&model.UserAccessToken{Id: "tok1", UserId: "bot1"}, "mybot", "req1")
	p.trackBotTokenExpiry(nil, "mybot", "req1")

	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestTrackBotTokenExpiry_StoreFailureIsLogged(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("KVSet", botTokenExpiryKey("tok1"), mock.Anything).Return(testAppErr("kv down"))

	p.trackBotTokenExpiry(&model.UserAccessToken{Id: "tok1", UserId: "bot1", ExpiresAt: 1}, "mybot", "req1")

	api.AssertCalled(t, "LogWarn", "failed to store bot token expiry; no reminders will be sent", "bot_user_id", "bot1", "error", mock.Anything)
}

// reminderFixture stores rec and an active bot, plus an active requester
// when rec's requester is req1, and records
// the DMs the plugin sends and the reminder state it saves.
type reminderFixture struct {
	api   *plugintest.API
	p     *Plugin
	sent  map[string][]string // user ID -> messages
	saved *botTokenExpiry
}

func newReminderFixture(t *testing.T, rec botTokenExpiry) *reminderFixture {
	t.Helper()
	api := &plugintest.API{}
	stubLogs(api)
	f := &reminderFixture{api: api, p: newTestPlugin(api), sent: map[string][]string{}}
	key := botTokenExpiryKey(rec.TokenID)
	api.On("KVGet", key).Return(mustMarshal(t, rec), nil)
	api.On("KVSet", key, mock.Anything).Run(func(args mock.Arguments) {
		var saved botTokenExpiry
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &saved))
		f.saved = &saved
	}).Return(nil).Maybe()
	api.On("KVDelete", key).Return(nil).Maybe()
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil).Maybe()
	api.On("GetUser", "req1").Return(&model.User{Id: "req1", Username: "alice"}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, "bot-user-id").Return(func(userID, _ string) (*model.Channel, *model.AppError) {
		return &model.Channel{Id: "dm-" + userID}, nil
	}).Maybe()
	api.On("CreatePost", mock.Anything).Return(func(post *model.Post) (*model.Post, *model.AppError) {
		userID := strings.TrimPrefix(post.ChannelId, "dm-")
		f.sent[userID] = append(f.sent[userID], post.Message)
		return post, nil
	}).Maybe()
	return f
}

func TestRemindTokenExpiry_Schedule(t *testing.T) {
	now := time.Date(2027, time.March, 1, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for name, tc := range map[string]struct {
		expiresIn    time.Duration
		lastReminded time.Duration // before now
		want         string        // message substring; "" means no reminder
	}{
		"more than a month out":            {expiresIn: 40 * day, lastReminded: 60 * day},
		"month out, a week since the last": {expiresIn: 20 * day, lastReminded: 7 * day, want: "in 20 days"},
		"month out, days since the last":   {expiresIn: 20 * day, lastReminded: 2 * day},
		"last week, a day since the last":  {expiresIn: 5 * day, lastReminded: day - 30*time.Minute, want: "in 5 days"},
		"last week, hours since the last":  {expiresIn: 5 * day, lastReminded: 10 * time.Hour},
		"weekly turns daily":               {expiresIn: 6 * day, lastReminded: 2 * day, want: "in 6 days"},
		"last day":                         {expiresIn: 3 * time.Hour, lastReminded: day, want: "within a day"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := botTokenExpiry{
				TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
				ExpiresAt:      now.Add(tc.expiresIn).UnixMilli(),
				LastRemindedAt: now.Add(-tc.lastReminded).UnixMilli(),
			}
			f := newReminderFixture(t, rec)

			f.p.remindTokenExpiry(botTokenExpiryKey("tok1"), now)

			if tc.want == "" {
				require.Empty(t, f.sent)
				require.Nil(t, f.saved)
				return
			}
			require.Len(t, f.sent["req1"], 1)
			msg := f.sent["req1"][0]
			require.Contains(t, msg, "**@mybot**")
			require.Contains(t, msg, tc.want)
			require.Contains(t, msg, "expires on **"+tokenExpiryDate(rec.ExpiresAt)+"**")
			require.Contains(t, msg, "/"+commandTrigger+" bot")
			require.NotNil(t, f.saved)
			require.Equal(t, now.UnixMilli(), f.saved.LastRemindedAt)
		})
	}
}

func TestRemindTokenExpiry_ExpiredSendsOneNoticeThenStops(t *testing.T) {
	now := time.Now()
	f := newReminderFixture(t, botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(), LastRemindedAt: now.Add(-2 * time.Hour).UnixMilli(),
	})

	f.p.remindTokenExpiry(botTokenExpiryKey("tok1"), now)

	require.Len(t, f.sent["req1"], 1)
	require.Contains(t, f.sent["req1"][0], "expired on")
	f.api.AssertCalled(t, "KVDelete", botTokenExpiryKey("tok1"))
}

func TestRemindTokenExpiry_UsesTheBotsCurrentUsername(t *testing.T) {
	now := time.Now()
	f := newReminderFixture(t, botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "oldname", RequesterID: "req1",
		ExpiresAt: now.Add(2 * 24 * time.Hour).UnixMilli(),
	})

	f.p.remindTokenExpiry(botTokenExpiryKey("tok1"), now)

	require.Contains(t, f.sent["req1"][0], "**@mybot**")
	require.Equal(t, "mybot", f.saved.BotUsername)
}

func TestRemindTokenExpiry_GoneBotStopsTracking(t *testing.T) {
	for name, getBot := range map[string]func(api *plugintest.API){
		"deleted": func(api *plugintest.API) {
			api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", DeleteAt: 1}, nil)
		},
		"not found": func(api *plugintest.API) {
			api.On("GetBot", "bot1", true).Return(nil, model.NewAppError("GetBot", "not_found", nil, "", http.StatusNotFound))
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			rec := botTokenExpiry{TokenID: "tok1", BotUserID: "bot1", RequesterID: "req1", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
			api.On("KVGet", botTokenExpiryKey("tok1")).Return(mustMarshal(t, rec), nil)
			getBot(api)
			api.On("KVDelete", botTokenExpiryKey("tok1")).Return(nil)

			p.remindTokenExpiry(botTokenExpiryKey("tok1"), time.Now())

			api.AssertNotCalled(t, "CreatePost", mock.Anything)
		})
	}
}

func TestRemindTokenExpiry_BotLookupFailureKeepsTracking(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	rec := botTokenExpiry{TokenID: "tok1", BotUserID: "bot1", RequesterID: "req1", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	api.On("KVGet", botTokenExpiryKey("tok1")).Return(mustMarshal(t, rec), nil)
	api.On("GetBot", "bot1", true).Return(nil, testAppErr("db down"))

	p.remindTokenExpiry(botTokenExpiryKey("tok1"), time.Now())

	api.AssertNotCalled(t, "KVDelete", mock.Anything)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestRemindTokenExpiry_GoneRequesterRemindsSystemAdmins(t *testing.T) {
	for name, getUser := range map[string]func(api *plugintest.API){
		"deactivated": func(api *plugintest.API) {
			api.On("GetUser", "gone1").Return(&model.User{Id: "gone1", Username: "alice", DeleteAt: 1}, nil)
		},
		"deleted": func(api *plugintest.API) {
			api.On("GetUser", "gone1").Return(nil, testAppErr("not found"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			rec := botTokenExpiry{
				TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "gone1",
				ExpiresAt: now.Add(3 * 24 * time.Hour).UnixMilli(),
			}
			f := newReminderFixture(t, rec)
			getUser(f.api)
			f.api.On("GetUsers", mock.MatchedBy(func(o *model.UserGetOptions) bool {
				return o.Role == model.SystemAdminRoleId && o.Active
			})).Return([]*model.User{{Id: "sa1"}, {Id: "sa2"}}, nil)

			f.p.remindTokenExpiry(botTokenExpiryKey("tok1"), now)

			require.Empty(t, f.sent["gone1"])
			for _, admin := range []string{"sa1", "sa2"} {
				require.Len(t, f.sent[admin], 1)
				require.Contains(t, f.sent[admin][0], "**@mybot**")
				require.Contains(t, f.sent[admin][0], "who asked for the bot, no longer has an active account")
			}
			require.NotNil(t, f.saved)
		})
	}
}

func TestRemindTokenExpiry_FailedSendRetriesNextRun(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	now := time.Now()
	rec := botTokenExpiry{TokenID: "tok1", BotUserID: "bot1", RequesterID: "req1", ExpiresAt: now.Add(-time.Hour).UnixMilli()}
	api.On("KVGet", botTokenExpiryKey("tok1")).Return(mustMarshal(t, rec), nil)
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("GetUser", "req1").Return(&model.User{Id: "req1"}, nil)
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(nil, testAppErr("no dm"))

	p.remindTokenExpiry(botTokenExpiryKey("tok1"), now)

	api.AssertNotCalled(t, "KVDelete", mock.Anything)
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestRemindTokenExpiry_UnreadableRecordIsDropped(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVGet", botTokenExpiryKey("tok1")).Return([]byte("{"), nil)
	api.On("KVDelete", botTokenExpiryKey("tok1")).Return(nil)

	p.remindTokenExpiry(botTokenExpiryKey("tok1"), time.Now())
}

func TestSendTokenExpiryReminders_ChecksOnlyTrackedTokensOnEveryPage(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	firstPage := make([]string, 200)
	for i := range firstPage {
		firstPage[i] = "pending_x"
	}
	firstPage[7] = botTokenExpiryKey("tok1")
	api.On("KVList", 0, 200).Return(firstPage, nil)
	api.On("KVList", 1, 200).Return([]string{botTokenExpiryKey("tok2"), "other"}, nil)
	api.On("KVGet", botTokenExpiryKey("tok1")).Return(nil, nil)
	api.On("KVGet", botTokenExpiryKey("tok2")).Return(nil, nil)

	p.sendTokenExpiryReminders(time.Now())

	api.AssertNumberOfCalls(t, "KVGet", 2)
}

func TestSendTokenExpiryReminders_ListFailureIsLogged(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("KVList", 0, 200).Return(nil, testAppErr("kv down"))

	p.sendTokenExpiryReminders(time.Now())

	api.AssertCalled(t, "LogError", "failed to list bot token expiries", "error", mock.Anything)
	api.AssertNotCalled(t, "KVGet", mock.Anything)
}
