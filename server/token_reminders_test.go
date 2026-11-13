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

func TestExpiredNotifyState(t *testing.T) {
	require.Equal(t, expiredNotifyNone, botTokenExpiry{}.expiredNotifyState())
	require.Equal(t, expiredNotifyPending, botTokenExpiry{ExpiredNotifyAttemptAt: 1}.expiredNotifyState())
	require.Equal(t, expiredNotifySent, botTokenExpiry{ExpiredNotifiedAt: 2}.expiredNotifyState())
	// Sent wins if both are set (recovery after a partial write).
	require.Equal(t, expiredNotifySent, botTokenExpiry{ExpiredNotifyAttemptAt: 1, ExpiredNotifiedAt: 2}.expiredNotifyState())
}

// none → pending (saved before send) → sent → delete.
func TestRemindTokenExpiry_NoneToPendingToSent(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	rec := botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1", RequestID: "req-id",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(), LastRemindedAt: now.Add(-48 * time.Hour).UnixMilli(),
	}
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	events := captureAudit(p)
	var store botTokenExpiry
	var saves []botTokenExpiry
	api.On("KVGet", key).Return(func(string) ([]byte, *model.AppError) {
		return mustMarshal(t, store), nil
	})
	api.On("KVSet", key, mock.Anything).Run(func(args mock.Arguments) {
		var saved botTokenExpiry
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &saved))
		saves = append(saves, saved)
		store = saved
	}).Return(nil)
	api.On("KVDelete", key).Return(nil)
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("GetUser", "req1").Return(&model.User{Id: "req1", Username: "alice"}, nil)
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(&model.Channel{Id: "dm-req1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	store = rec

	p.remindTokenExpiry(key, now)

	require.Len(t, saves, 2)
	require.Equal(t, expiredNotifyPending, saves[0].expiredNotifyState())
	require.Equal(t, now.UnixMilli(), saves[0].ExpiredNotifyAttemptAt)
	require.Equal(t, int64(0), saves[0].ExpiredNotifiedAt)
	require.Equal(t, expiredNotifySent, saves[1].expiredNotifyState())
	require.Equal(t, now.UnixMilli(), saves[1].ExpiredNotifiedAt)
	api.AssertCalled(t, "KVDelete", key)
	require.Equal(t, []string{eventTokenReminder, eventTokenTrackingEnded}, eventNames(*events))
	require.Equal(t, true, (*events)[0].Detail["expired"])
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
	require.Equal(t, now.UnixMilli(), f.saved.ExpiredNotifyAttemptAt)
	require.Equal(t, now.UnixMilli(), f.saved.ExpiredNotifiedAt)
}

// pending + send failure stays pending; next run retries the DM without
// rewriting pending, then advances to sent.
func TestRemindTokenExpiry_PendingSendFailureRetriesThenSends(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	var store botTokenExpiry
	var sent int
	var pendingSaves int
	api.On("KVGet", key).Return(func(string) ([]byte, *model.AppError) {
		return mustMarshal(t, store), nil
	})
	api.On("KVSet", key, mock.Anything).Run(func(args mock.Arguments) {
		var saved botTokenExpiry
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &saved))
		if saved.expiredNotifyState() == expiredNotifyPending && saved.ExpiredNotifiedAt == 0 {
			pendingSaves++
		}
		store = saved
	}).Return(nil)
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("GetUser", "req1").Return(&model.User{Id: "req1"}, nil)
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(nil, testAppErr("no dm")).Once()
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(&model.Channel{Id: "dm-req1"}, nil)
	api.On("CreatePost", mock.Anything).Run(func(mock.Arguments) { sent++ }).Return(&model.Post{}, nil)
	api.On("KVDelete", key).Return(nil)
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	store = botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(),
	}

	p.remindTokenExpiry(key, now)
	require.Equal(t, 0, sent)
	require.Equal(t, 1, pendingSaves)
	require.Equal(t, expiredNotifyPending, store.expiredNotifyState())
	api.AssertNotCalled(t, "KVDelete", key)

	later := now.Add(25 * time.Hour)
	p.remindTokenExpiry(key, later)
	require.Equal(t, 1, sent)
	require.Equal(t, 1, pendingSaves, "pending must not be rewritten on retry")
	require.Equal(t, expiredNotifySent, store.expiredNotifyState())
	api.AssertCalled(t, "KVDelete", key)
}

// Starting already pending (crash after pending save) must not require a
// second pending write before send.
func TestRemindTokenExpiry_AlreadyPendingSendsWithoutRewritingPending(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	attemptAt := now.Add(-time.Hour).UnixMilli()
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	store := botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-2 * time.Hour).UnixMilli(), ExpiredNotifyAttemptAt: attemptAt,
	}
	var saves []botTokenExpiry
	api.On("KVGet", key).Return(func(string) ([]byte, *model.AppError) {
		return mustMarshal(t, store), nil
	})
	api.On("KVSet", key, mock.Anything).Run(func(args mock.Arguments) {
		var saved botTokenExpiry
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &saved))
		saves = append(saves, saved)
		store = saved
	}).Return(nil)
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("GetUser", "req1").Return(&model.User{Id: "req1"}, nil)
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(&model.Channel{Id: "dm-req1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("KVDelete", key).Return(nil)
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()

	p.remindTokenExpiry(key, now)

	require.Len(t, saves, 1)
	require.Equal(t, attemptAt, saves[0].ExpiredNotifyAttemptAt)
	require.Equal(t, expiredNotifySent, saves[0].expiredNotifyState())
}

// sent → only retry delete; no DM even a day later with a stuck delete.
func TestRemindTokenExpiry_SentOnlyRetriesDelete(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	rec := botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt:              now.Add(-time.Hour).UnixMilli(),
		ExpiredNotifyAttemptAt: now.Add(-26 * time.Hour).UnixMilli(),
		ExpiredNotifiedAt:      now.Add(-25 * time.Hour).UnixMilli(),
	}
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVGet", key).Return(mustMarshal(t, rec), nil)
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("KVDelete", key).Return(testAppErr("kv down")).Once()
	api.On("KVDelete", key).Return(nil).Once()
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(true, nil).Maybe()

	p.remindTokenExpiry(key, now)
	p.remindTokenExpiry(key, now.Add(25*time.Hour))

	api.AssertNumberOfCalls(t, "KVDelete", 2)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "KVSet", key, mock.Anything)
}

// none + pending-save failure must not send (no durable pending marker).
func TestRemindTokenExpiry_PendingSaveFailureSkipsDM(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	rec := botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(),
	}
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVGet", key).Return(mustMarshal(t, rec), nil)
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("KVSet", key, mock.Anything).Return(testAppErr("kv down"))

	p.remindTokenExpiry(key, now)

	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "KVDelete", key)
}

// DM succeeded but "sent" save failed: still try delete. Next run sees
// pending and may re-send once if delete also failed.
func TestRemindTokenExpiry_SentSaveFailureStillEndsTracking(t *testing.T) {
	now := time.Now()
	key := botTokenExpiryKey("tok1")
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	store := botTokenExpiry{
		TokenID: "tok1", BotUserID: "bot1", BotUsername: "mybot", RequesterID: "req1",
		ExpiresAt: now.Add(-time.Hour).UnixMilli(), ExpiredNotifyAttemptAt: now.UnixMilli(),
	}
	api.On("KVGet", key).Return(func(string) ([]byte, *model.AppError) {
		return mustMarshal(t, store), nil
	})
	api.On("KVSet", key, mock.Anything).Return(testAppErr("kv down"))
	api.On("GetBot", "bot1", true).Return(&model.Bot{UserId: "bot1", Username: "mybot"}, nil)
	api.On("GetUser", "req1").Return(&model.User{Id: "req1", Username: "alice"}, nil)
	api.On("GetDirectChannel", "req1", "bot-user-id").Return(&model.Channel{Id: "dm-req1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
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
