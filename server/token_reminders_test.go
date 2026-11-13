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

// stubTokenReminderJob lets cluster jobs started by OnActivate (token
// reminders + two-step channel reconcile) take their locks and find nothing
// to do.
func stubTokenReminderJob(api *plugintest.API) {
	isJobKey := mock.MatchedBy(func(key string) bool {
		return strings.HasSuffix(key, tokenReminderJobKey) || strings.HasSuffix(key, twoStepChannelReconcileJobKey)
	})
	api.On("KVSetWithOptions", isJobKey, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	api.On("KVSet", isJobKey, mock.Anything).Return(nil).Maybe()
	api.On("KVList", mock.Anything, mock.Anything).Return([]string{}, nil).Maybe()
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVSet", kvTokenExpiryIndex, mock.Anything).Return(nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	api.On("KVGet", kvOpenRequestIndex).Return(nil, nil).Maybe()
	api.On("KVSet", kvOpenRequestIndex, mock.Anything).Return(nil).Maybe()
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()
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
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()

	before := model.GetMillis()
	p.trackBotTokenExpiry(&model.UserAccessToken{Id: "tok1", Token: "secret-value", UserId: "bot1", ExpiresAt: 12345}, "mybot", "req1", "")

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

	p.trackBotTokenExpiry(&model.UserAccessToken{Id: "tok1", UserId: "bot1"}, "mybot", "req1", "")
	p.trackBotTokenExpiry(nil, "mybot", "req1", "")

	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestTrackBotTokenExpiry_StoreFailureIsLogged(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("KVSet", botTokenExpiryKey("tok1"), mock.Anything).Return(testAppErr("kv down"))

	p.trackBotTokenExpiry(&model.UserAccessToken{Id: "tok1", UserId: "bot1", ExpiresAt: 1}, "mybot", "req1", "")

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
		// ~1h slack: daily cadence may fire slightly early, but not after
		// only a few hours (that would spam).
		"last week, a day since the last": {expiresIn: 5 * day, lastReminded: day - 30*time.Minute, want: "in 5 days"},
		"last week, hours since the last": {expiresIn: 5 * day, lastReminded: 10 * time.Hour},
		"weekly turns daily":              {expiresIn: 6 * day, lastReminded: 2 * day, want: "in 6 days"},
		"last day":                        {expiresIn: 3 * time.Hour, lastReminded: day, want: "within a day"},
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
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1", RequestID: "req-id",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(), LastRemindedAt: now.Add(-48 * time.Hour).UnixMilli(),
	})
	events := captureAudit(f.p)

	f.p.remindTokenExpiry(botTokenExpiryKey("tok1"), now)

	require.Len(t, f.sent["req1"], 1)
	require.Contains(t, f.sent["req1"][0], "expired on")
	require.NotNil(t, f.saved)
	require.Equal(t, now.UnixMilli(), f.saved.ExpiredNotifiedAt)
	f.api.AssertCalled(t, "KVDelete", botTokenExpiryKey("tok1"))
	require.Equal(t, []string{eventTokenReminder, eventTokenTrackingEnded}, eventNames(*events))
	require.Equal(t, "req-id", (*events)[0].RequestID)
	require.Equal(t, true, (*events)[0].Detail["expired"])
	require.Equal(t, "the token expired", (*events)[1].Detail["reason"])
}

// A short-lived token still gets the expired notice: LastRemindedAt at issue
// time must not stand in for "already told them it expired".
func TestRemindTokenExpiry_ShortLivedTokenStillGetsExpiredNotice(t *testing.T) {
	now := time.Now()
	f := newReminderFixture(t, botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-time.Minute).UnixMilli(), LastRemindedAt: now.Add(-30 * time.Minute).UnixMilli(),
	})

	f.p.remindTokenExpiry(botTokenExpiryKey("tok1"), now)

	require.Len(t, f.sent["req1"], 1)
	require.Contains(t, f.sent["req1"][0], "expired on")
	require.Equal(t, now.UnixMilli(), f.saved.ExpiredNotifiedAt)
}

// Once ExpiredNotifiedAt is set, a stuck delete only retries cleanup — no
// second expired DM, even a day later.
func TestRemindTokenExpiry_StuckDeleteDoesNotResendExpiredNotice(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	rec := botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(), LastRemindedAt: now.Add(-48 * time.Hour).UnixMilli(),
	}
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	sent := map[string][]string{}
	var store botTokenExpiry
	api.On("KVGet", key).Return(func(string) ([]byte, *model.AppError) {
		return mustMarshal(t, store), nil
	})
	api.On("KVSet", key, mock.Anything).Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &store))
	}).Return(nil)
	api.On("KVDelete", key).Return(testAppErr("kv down"))
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("GetUser", "req1").Return(&model.User{Id: "req1", Username: "alice"}, nil)
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(&model.Channel{Id: "dm-req1"}, nil)
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		post := args.Get(0).(*model.Post)
		sent["req1"] = append(sent["req1"], post.Message)
	}).Return(&model.Post{}, nil)

	store = rec
	p.remindTokenExpiry(key, now)
	require.Len(t, sent["req1"], 1)
	require.Greater(t, store.ExpiredNotifiedAt, int64(0))

	// Next daily run: marker set, delete still stuck — no second DM.
	later := now.Add(25 * time.Hour)
	p.remindTokenExpiry(key, later)
	require.Len(t, sent["req1"], 1)
	api.AssertNumberOfCalls(t, "KVDelete", 2)
}

func TestRemindTokenExpiry_AlreadyNotifiedOnlyRetriesDelete(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	rec := botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(), ExpiredNotifiedAt: now.Add(-24 * time.Hour).UnixMilli(),
	}
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVGet", key).Return(mustMarshal(t, rec), nil)
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("KVDelete", key).Return(nil)
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()

	p.remindTokenExpiry(key, now)

	api.AssertCalled(t, "KVDelete", key)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "KVSet", key, mock.Anything)
}

func TestRemindTokenExpiry_ExpiredMarkerSaveFailureStillEndsTracking(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	rec := botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(), LastRemindedAt: now.Add(-48 * time.Hour).UnixMilli(),
	}
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVGet", key).Return(mustMarshal(t, rec), nil)
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("GetUser", "req1").Return(&model.User{Id: "req1", Username: "alice"}, nil)
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(&model.Channel{Id: "dm-req1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("KVSet", key, mock.Anything).Return(testAppErr("kv down"))
	api.On("KVDelete", key).Return(nil)
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()

	p.remindTokenExpiry(key, now)

	api.AssertCalled(t, "CreatePost", mock.Anything)
	api.AssertCalled(t, "KVDelete", key)
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
			events := captureAudit(p)
			rec := botTokenExpiry{TokenID: "tok1", BotUserID: "bot1", RequesterID: "req1", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
			api.On("KVGet", botTokenExpiryKey("tok1")).Return(mustMarshal(t, rec), nil)
			getBot(api)
			api.On("KVDelete", botTokenExpiryKey("tok1")).Return(nil)
			api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
			api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()

			p.remindTokenExpiry(botTokenExpiryKey("tok1"), time.Now())

			api.AssertNotCalled(t, "CreatePost", mock.Anything)
			require.Equal(t, []string{eventTokenTrackingEnded}, eventNames(*events))
			require.Equal(t, "tok1", (*events)[0].Detail["token_id"])
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

// A failed send leaves no ExpiredNotifiedAt, so the next run retries the notice.
func TestRemindTokenExpiry_FailedSendRetriesNotice(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	rec := botTokenExpiry{TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1", ExpiresAt: now.Add(-time.Hour).UnixMilli()}
	var store botTokenExpiry
	var sent int
	api.On("KVGet", key).Return(func(string) ([]byte, *model.AppError) {
		return mustMarshal(t, store), nil
	})
	api.On("KVSet", key, mock.Anything).Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &store))
	}).Return(nil).Maybe()
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("GetUser", "req1").Return(&model.User{Id: "req1"}, nil)
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(nil, testAppErr("no dm")).Once()
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(&model.Channel{Id: "dm-req1"}, nil)
	api.On("CreatePost", mock.Anything).Run(func(mock.Arguments) { sent++ }).Return(&model.Post{}, nil)
	api.On("KVDelete", key).Return(nil)
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	store = rec

	p.remindTokenExpiry(key, now)
	require.Equal(t, 0, sent)
	require.Equal(t, int64(0), store.ExpiredNotifiedAt)
	api.AssertNotCalled(t, "KVDelete", key)

	later := now.Add(25 * time.Hour)
	p.remindTokenExpiry(key, later)
	require.Equal(t, 1, sent)
	require.Greater(t, store.ExpiredNotifiedAt, int64(0))
	api.AssertCalled(t, "KVDelete", key)
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
	// Missing index → rebuild from KVList (CAS persist), then remind.
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil)
	firstPage := make([]string, 200)
	for i := range firstPage {
		firstPage[i] = "pending_x"
	}
	firstPage[7] = botTokenExpiryKey("tok1")
	api.On("KVList", 0, 200).Return(firstPage, nil)
	api.On("KVList", 1, 200).Return([]string{botTokenExpiryKey("tok2"), "other", kvTokenExpiryIndex}, nil)
	api.On("KVGet", botTokenExpiryKey("tok1")).Return(nil, nil)
	api.On("KVGet", botTokenExpiryKey("tok2")).Return(nil, nil)

	p.sendTokenExpiryReminders(time.Now())

	api.AssertCalled(t, "KVGet", botTokenExpiryKey("tok1"))
	api.AssertCalled(t, "KVGet", botTokenExpiryKey("tok2"))
}

func TestSendTokenExpiryReminders_ListFailureIsLogged(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil)
	api.On("KVList", 0, 200).Return(nil, testAppErr("kv down"))

	p.sendTokenExpiryReminders(time.Now())

	api.AssertCalled(t, "LogError", "failed to list bot token expiries", "error", mock.Anything)
	api.AssertNotCalled(t, "KVGet", mock.MatchedBy(func(key string) bool {
		return key != kvTokenExpiryIndex && strings.HasPrefix(key, botTokenExpiryKeyPrefix)
	}))
}
