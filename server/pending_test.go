package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// isPendingListKey matches the KV key of any user's pending list.
var isPendingListKey = mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvPendingPrefix) })

// isNotPendingListKey matches every other KV key.
var isNotPendingListKey = mock.MatchedBy(func(key string) bool { return !strings.HasPrefix(key, kvPendingPrefix) })

// isApprovalCardsKey matches the KV key of any request's recorded
// approval cards.
var isApprovalCardsKey = mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvApprovalCardsPrefix) })

// stubApprovalPosts lets production code record approval cards freely:
// none are recorded to start with and every write succeeds.
func stubApprovalPosts(api *plugintest.API) {
	api.On("KVGet", isApprovalCardsKey).Return(nil, nil).Maybe()
	api.On("KVSet", isApprovalCardsKey, mock.Anything).Return(nil).Maybe()
	api.On("KVDelete", isApprovalCardsKey).Return(nil).Maybe()
}

// stubPendingLists lets production code read and update pending lists
// freely: every list starts empty and every write succeeds. Recorded
// approval posts are stubbed the same way, since the same paths save and
// resolve them.
func stubPendingLists(api *plugintest.API) {
	stubApprovalPosts(api)
	api.On("KVGet", isPendingListKey).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", isPendingListKey, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	api.On("KVCompareAndDelete", isPendingListKey, mock.Anything).Return(true, nil).Maybe()
}

// stubSubmittedRequestExists makes any request stored under prefix read
// back as present, for the check submit makes after recording the
// approval posts.
func stubSubmittedRequestExists(api *plugintest.API, prefix string) {
	api.On("KVGet", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, prefix) })).Return([]byte("{}"), nil).Maybe()
}

// stubNewerSubmittedRequestsExist is stubSubmittedRequestExists for every
// request type other than channel and Channel Admin.
func stubNewerSubmittedRequestsExist(api *plugintest.API) {
	for _, prefix := range []string{kvTeamRequestPrefix, kvTeamAdminRequestPrefix, kvTeamMemberRequestPrefix, kvTeamGuestRequestPrefix, kvBotRequestPrefix, kvWebhookRequestPrefix} {
		stubSubmittedRequestExists(api, prefix)
	}
}

// storedRequests makes the given request IDs exist (and only those) under
// prefix.
func storedRequests(api *plugintest.API, prefix string, ids ...string) {
	api.On("KVGet", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, prefix) })).Return(func(key string) ([]byte, *model.AppError) {
		for _, id := range ids {
			if key == prefix+id {
				return []byte("{}"), nil
			}
		}
		return nil, nil
	})
}

// stubPendingMigrated marks the stored-request migration as already done.
func stubPendingMigrated(api *plugintest.API) {
	api.On("KVGet", kvPendingMigrated).Return([]byte("1"), nil).Maybe()
}

// approvableConfig is a configuration whose prefix list allows the
// "team-" channel names the approval tests use.
func approvableConfig() *configuration {
	return &configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, prefixes: []channelPrefix{{Prefix: "team-"}}}
}

// stubActiveTeamMember makes userID an active member of teamID.
func stubActiveTeamMember(api *plugintest.API, teamID, userID string) {
	api.On("GetTeamMember", teamID, userID).Return(&model.TeamMember{TeamId: teamID, UserId: userID}, nil).Maybe()
}

// stubLiveTeam lets approve-time checks find teamID live.
func stubLiveTeam(api *plugintest.API, teamID string) {
	api.On("GetTeam", teamID).Return(&model.Team{Id: teamID, Name: teamID, DisplayName: "Team " + teamID}, nil).Maybe()
}

const day = millisPerDay

// entriesJSON encodes a pending list the way updatePending stores it.
func entriesJSON(t *testing.T, entries ...pendingEntry) []byte {
	t.Helper()
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	return data
}

// decodeEntries decodes a stored pending list.
func decodeEntries(t *testing.T, data []byte) []pendingEntry {
	t.Helper()
	var entries []pendingEntry
	require.NoError(t, json.Unmarshal(data, &entries))
	return entries
}

func TestRequestExpired(t *testing.T) {
	now := 100 * day
	require.False(t, requestExpired(now-90*day, 0, now), "0 days never expires")
	require.False(t, requestExpired(now-30*day, 30, now))
	require.True(t, requestExpired(now-30*day-1, 30, now))
	require.False(t, requestExpired(0, 30, now), "a missing timestamp counts as new")
}

func TestPendingRequestKeys(t *testing.T) {
	require.Equal(t, "pending_channel_u1", pendingListKey(pendingKindChannel, "u1"))
	require.Equal(t, "team1/team-x", (&channelRequest{TeamID: "team1", Name: "team-x"}).pendingKey())

	req := &adminRequest{ChannelID: "ch1", NomineeIDs: []string{"b", "a"}}
	require.Equal(t, "ch1/a,b", req.pendingKey())
	require.Equal(t, []string{"b", "a"}, req.NomineeIDs, "the request's own order is left alone")
}

func TestReservePending(t *testing.T) {
	now := model.GetMillis()
	key := pendingListKey(pendingKindChannel, "u1")
	newEntry := pendingEntry{ID: "new", CreatedAt: now, Key: "team1/team-new"}
	config := &configuration{maxPending: 2, expiryDays: 30}

	t.Run("first request creates the list", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", key).Return(nil, nil)
		api.On("KVCompareAndSet", key, []byte(nil), entriesJSON(t, newEntry)).Return(true, nil)

		require.NoError(t, p.reservePending(pendingKindChannel, "u1", newEntry, config))
	})

	t.Run("under the limit appends", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		old := entriesJSON(t, pendingEntry{ID: "a", CreatedAt: now, Key: "team1/team-a"})
		api.On("KVGet", key).Return(old, nil)
		var saved []byte
		api.On("KVCompareAndSet", key, old, mock.Anything).Run(func(args mock.Arguments) { saved = args.Get(2).([]byte) }).Return(true, nil)

		require.NoError(t, p.reservePending(pendingKindChannel, "u1", newEntry, config))
		require.Len(t, decodeEntries(t, saved), 2)
	})

	t.Run("at the limit refuses", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", key).Return(entriesJSON(t,
			pendingEntry{ID: "a", CreatedAt: now, Key: "team1/team-a"},
			pendingEntry{ID: "b", CreatedAt: now, Key: "team1/team-b"},
		), nil)
		storedRequests(api, kvRequestPrefix, "a", "b")

		err := p.reservePending(pendingKindChannel, "u1", newEntry, config)
		require.EqualError(t, err, "you have 2 pending requests waiting for review; you can submit more once some are approved, denied or expire")
		api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("limit 0 means no limit", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		old := entriesJSON(t,
			pendingEntry{ID: "a", CreatedAt: now, Key: "team1/team-a"},
			pendingEntry{ID: "b", CreatedAt: now, Key: "team1/team-b"},
		)
		api.On("KVGet", key).Return(old, nil)
		api.On("KVCompareAndSet", key, old, mock.Anything).Return(true, nil)

		require.NoError(t, p.reservePending(pendingKindChannel, "u1", newEntry, &configuration{expiryDays: 30}))
	})

	t.Run("expired entries don't count and don't block the same request", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		expiredSame := pendingEntry{ID: "a", CreatedAt: now - 31*day, Key: "team1/team-new"}
		expiredOther := pendingEntry{ID: "b", CreatedAt: now - 40*day, Key: "team1/team-b"}
		old := entriesJSON(t, expiredSame, expiredOther)
		api.On("KVGet", key).Return(old, nil)
		var saved []byte
		api.On("KVCompareAndSet", key, old, mock.Anything).Run(func(args mock.Arguments) { saved = args.Get(2).([]byte) }).Return(true, nil)

		require.NoError(t, p.reservePending(pendingKindChannel, "u1", newEntry, config))
		// They stay listed, so they count again if expiry is raised.
		require.Equal(t, []pendingEntry{expiredSame, expiredOther, newEntry}, decodeEntries(t, saved))
	})

	t.Run("an entry whose request is gone doesn't block the same request", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		other := pendingEntry{ID: "b", CreatedAt: now, Key: "team1/team-b"}
		old := entriesJSON(t, pendingEntry{ID: "gone", CreatedAt: now, Key: newEntry.Key}, other)
		api.On("KVGet", key).Return(old, nil)
		storedRequests(api, kvRequestPrefix, "b")
		var saved []byte
		api.On("KVCompareAndSet", key, old, mock.Anything).Run(func(args mock.Arguments) { saved = args.Get(2).([]byte) }).Return(true, nil)

		require.NoError(t, p.reservePending(pendingKindChannel, "u1", newEntry, config))
		require.Equal(t, []pendingEntry{other, newEntry}, decodeEntries(t, saved))
	})

	t.Run("entries whose requests are gone don't count toward the limit", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		kept := pendingEntry{ID: "a", CreatedAt: now, Key: "team1/team-a"}
		old := entriesJSON(t, kept, pendingEntry{ID: "gone", CreatedAt: now, Key: "team1/team-b"})
		api.On("KVGet", key).Return(old, nil)
		storedRequests(api, kvRequestPrefix, "a")
		var saved []byte
		api.On("KVCompareAndSet", key, old, mock.Anything).Run(func(args mock.Arguments) { saved = args.Get(2).([]byte) }).Return(true, nil)

		require.NoError(t, p.reservePending(pendingKindChannel, "u1", newEntry, config))
		require.Equal(t, []pendingEntry{kept, newEntry}, decodeEntries(t, saved))
	})

	t.Run("a failed request lookup refuses nothing and saves nothing", func(t *testing.T) {
		for name, list := range map[string][]byte{
			"duplicate": entriesJSON(t, pendingEntry{ID: "a", CreatedAt: now, Key: newEntry.Key}),
			"at limit": entriesJSON(t,
				pendingEntry{ID: "a", CreatedAt: now, Key: "team1/team-a"},
				pendingEntry{ID: "b", CreatedAt: now, Key: "team1/team-b"},
			),
		} {
			t.Run(name, func(t *testing.T) {
				api := &plugintest.API{}
				stubLogs(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)
				api.On("KVGet", key).Return(list, nil)
				api.On("KVGet", kvRequestPrefix+"a").Return(nil, testAppErr("kv down"))

				err := p.reservePending(pendingKindChannel, "u1", newEntry, config)
				require.EqualError(t, err, "could not save your request; try again")
				api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
			})
		}
	})

	t.Run("reserving the same request again replaces its entry", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		old := entriesJSON(t, pendingEntry{ID: "new", CreatedAt: 1, Key: newEntry.Key})
		api.On("KVGet", key).Return(old, nil)
		api.On("KVCompareAndSet", key, old, entriesJSON(t, newEntry)).Return(true, nil)

		require.NoError(t, p.reservePending(pendingKindChannel, "u1", newEntry, config))
	})

	t.Run("the same channel request refuses", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", key).Return(entriesJSON(t, pendingEntry{ID: "a", CreatedAt: now, Key: newEntry.Key}), nil)
		storedRequests(api, kvRequestPrefix, "a")

		err := p.reservePending(pendingKindChannel, "u1", newEntry, config)
		require.EqualError(t, err, "you already have a pending request for ~team-new in this team")
	})

	t.Run("the same Channel Admin request refuses", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		adminKey := pendingListKey(pendingKindAdmin, "u1")
		entry := pendingEntry{ID: "new", CreatedAt: now, Key: "ch1/a,b"}
		api.On("KVGet", adminKey).Return(entriesJSON(t, pendingEntry{ID: "a", CreatedAt: now, Key: entry.Key}), nil)
		storedRequests(api, kvAdminRequestPrefix, "a")

		err := p.reservePending(pendingKindAdmin, "u1", entry, config)
		require.EqualError(t, err, "you already have a pending Channel Admin request for these people in this channel")
	})

	t.Run("a concurrent update is retried", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		first := entriesJSON(t, pendingEntry{ID: "a", CreatedAt: now, Key: "team1/team-a"})
		second := entriesJSON(t, pendingEntry{ID: "a", CreatedAt: now, Key: "team1/team-a"}, pendingEntry{ID: "b", CreatedAt: now, Key: "team1/team-b"})
		api.On("KVGet", key).Return(first, nil).Once()
		api.On("KVCompareAndSet", key, first, mock.Anything).Return(false, nil).Once()
		api.On("KVGet", key).Return(second, nil).Once()
		storedRequests(api, kvRequestPrefix, "a", "b")

		// The retry sees the other request and is now at the limit.
		err := p.reservePending(pendingKindChannel, "u1", newEntry, config)
		require.ErrorContains(t, err, "you have 2 pending requests")
	})

	t.Run("persistent conflicts give up", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", key).Return(nil, nil).Times(pendingUpdateAttempts)
		api.On("KVCompareAndSet", key, []byte(nil), mock.Anything).Return(false, nil).Times(pendingUpdateAttempts)

		err := p.reservePending(pendingKindChannel, "u1", newEntry, config)
		require.EqualError(t, err, "could not save your request; try again")
		api.AssertCalled(t, "LogError", "failed to reserve pending request", "user_id", "u1", "error", "pending requests changed while saving")
	})

	t.Run("store errors", func(t *testing.T) {
		for name, setup := range map[string]func(api *plugintest.API){
			"read fails":   func(api *plugintest.API) { api.On("KVGet", key).Return(nil, testAppErr("kv down")) },
			"corrupt list": func(api *plugintest.API) { api.On("KVGet", key).Return([]byte("{"), nil) },
			"write fails": func(api *plugintest.API) {
				api.On("KVGet", key).Return(nil, nil)
				api.On("KVCompareAndSet", key, []byte(nil), mock.Anything).Return(false, testAppErr("kv down"))
			},
		} {
			t.Run(name, func(t *testing.T) {
				api := &plugintest.API{}
				stubLogs(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)
				setup(api)

				require.EqualError(t, p.reservePending(pendingKindChannel, "u1", newEntry, config), "could not save your request; try again")
			})
		}
	})
}

func TestReleasePending(t *testing.T) {
	key := pendingListKey(pendingKindAdmin, "u1")
	a := pendingEntry{ID: "a", CreatedAt: 1, Key: "ch1/x"}
	b := pendingEntry{ID: "b", CreatedAt: 2, Key: "ch1/y"}

	t.Run("removes the request", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		old := entriesJSON(t, a, b)
		api.On("KVGet", key).Return(old, nil)
		api.On("KVCompareAndSet", key, old, entriesJSON(t, b)).Return(true, nil)

		p.releasePending(pendingKindAdmin, "u1", "a")
	})

	t.Run("removing the last request deletes the list", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		old := entriesJSON(t, a)
		api.On("KVGet", key).Return(old, nil)
		api.On("KVCompareAndDelete", key, old).Return(true, nil)

		p.releasePending(pendingKindAdmin, "u1", "a")
	})

	t.Run("unknown request or no list writes nothing", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", key).Return(entriesJSON(t, a), nil).Once()
		api.On("KVGet", key).Return(nil, nil).Once()

		p.releasePending(pendingKindAdmin, "u1", "zzz")
		p.releasePending(pendingKindAdmin, "u1", "a")
		api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
		api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	})

	t.Run("failures are logged", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", key).Return(nil, testAppErr("kv down"))

		p.releasePending(pendingKindAdmin, "u1", "a")
		api.AssertCalled(t, "LogWarn", "failed to update pending requests", "user_id", "u1", "request_id", "a", "error", mock.Anything)
	})
}

func TestMigratePendingRequests(t *testing.T) {
	r1 := model.NewId()
	r2 := model.NewId()
	r3 := model.NewId()
	a1 := model.NewId()
	u1 := model.NewId()
	u2 := model.NewId()

	seed := func(store map[string][]byte) {
		store[kvRequestPrefix+r1] = recordJSON(t, channelRequest{ID: r1, RequesterID: u1, TeamID: "team1", Name: "team-x"})
		store[kvRequestPrefix+r2] = recordJSON(t, channelRequest{ID: r2, RequesterID: u1, TeamID: "team1", Name: "team-y", CreatedAt: 5})
		store[kvAdminRequestPrefix+a1] = recordJSON(t, adminRequest{ID: a1, RequesterID: u2, ChannelID: "ch1", NomineeIDs: []string{"n1"}})
		// u1 already has r3 listed; the migration must keep it.
		store[kvRequestPrefix+r3] = recordJSON(t, channelRequest{ID: r3, RequesterID: u1, TeamID: "team1", Name: "team-z", CreatedAt: 7})
		store[pendingListKey(pendingKindChannel, u1)] = entriesJSON(t, pendingEntry{ID: r3, CreatedAt: 7, Key: "team1/team-z"})
		store[kvRequestPrefix+"bad"] = []byte("{")
		store[kvRequestPrefix+model.NewId()] = recordJSON(t, channelRequest{ID: "mismatch", RequesterID: u1})
		store["other_key"] = []byte("x")
	}

	t.Run("backfills stored requests and records the migration", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLines(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		store := fakeKV(api)
		seed(store)

		require.NoError(t, p.migratePendingRequests())

		var got channelRequest
		require.NoError(t, json.Unmarshal(store[kvRequestPrefix+r1], &got))
		require.Positive(t, got.CreatedAt)
		require.Equal(t, "team-x", got.Name)
		require.NoError(t, json.Unmarshal(store[kvRequestPrefix+r2], &got))
		require.Equal(t, int64(5), got.CreatedAt, "an existing timestamp is kept")

		ids := func(kind, userID string) []string {
			var out []string
			for _, e := range decodeEntries(t, store[pendingListKey(kind, userID)]) {
				out = append(out, e.ID)
			}
			return out
		}
		require.ElementsMatch(t, []string{r1, r2, r3}, ids(pendingKindChannel, u1))
		require.Equal(t, []string{a1}, ids(pendingKindAdmin, u2))
		require.Equal(t, migrationDone, string(store[kvPendingMigrated]))
		api.AssertCalled(t, "LogWarn", "skipping unreadable stored request", "key", kvRequestPrefix+"bad", "error", mock.Anything)
	})

	t.Run("already done or running elsewhere", func(t *testing.T) {
		for _, state := range []string{migrationDone, migrationRunning} {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			store := fakeKV(api)
			store[kvPendingMigrated] = []byte(state)

			require.NoError(t, p.migratePendingRequests())
			api.AssertNotCalled(t, "KVList", mock.Anything, mock.Anything)
		}
	})

	t.Run("lists every page before changing anything", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		store := fakeKV(api)
		for range 250 {
			id := model.NewId()
			store[kvRequestPrefix+id] = recordJSON(t, channelRequest{ID: id, RequesterID: u1, TeamID: "team1", Name: "team-" + id})
		}

		require.NoError(t, p.migratePendingRequests())
		require.Len(t, decodeEntries(t, store[pendingListKey(pendingKindChannel, u1)]), 250)
	})

	t.Run("a request approved while its entry is added is taken back out", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		record := recordJSON(t, channelRequest{ID: r1, RequesterID: u1, TeamID: "team1", Name: "team-x", CreatedAt: 5})
		// The record is read once, then claimed by an approver before the
		// existence check.
		reads := 0
		api.On("KVGet", kvRequestPrefix+r1).Return(func(string) ([]byte, *model.AppError) {
			reads++
			if reads == 1 {
				return record, nil
			}
			return nil, nil
		})
		store := fakeKV(api)

		require.NoError(t, p.migratePendingRequest(kvRequestPrefix+r1, 10))
		require.Equal(t, 2, reads)
		require.Empty(t, store, "the entry was added, then taken back out")
		api.AssertCalled(t, "KVCompareAndSet", pendingListKey(pendingKindChannel, u1), []byte(nil), mock.Anything)
	})

	t.Run("a record changed meanwhile is read again", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		legacy := recordJSON(t, channelRequest{ID: r1, RequesterID: u1, TeamID: "team1", Name: "team-x"})
		api.On("KVCompareAndSet", kvRequestPrefix+r1, legacy, mock.Anything).Return(false, nil).Once()
		store := fakeKV(api)
		store[kvRequestPrefix+r1] = legacy

		require.NoError(t, p.migratePendingRequest(kvRequestPrefix+r1, 10))
		require.Len(t, decodeEntries(t, store[pendingListKey(pendingKindChannel, u1)]), 1)
	})

	t.Run("a record that keeps changing is reported", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		legacy := recordJSON(t, channelRequest{ID: r1, RequesterID: u1, TeamID: "team1", Name: "team-x"})
		api.On("KVGet", kvRequestPrefix+r1).Return(legacy, nil)
		api.On("KVCompareAndSet", kvRequestPrefix+r1, legacy, mock.Anything).Return(false, nil)

		require.EqualError(t, p.migratePendingRequest(kvRequestPrefix+r1, 10), "stored request kept changing")
	})

	t.Run("another server claims it first", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVSetWithOptions", kvPendingMigrated, []byte(migrationRunning), mock.Anything).Return(false, nil)
		fakeKV(api)

		require.NoError(t, p.migratePendingRequests())
		api.AssertNotCalled(t, "KVList", mock.Anything, mock.Anything)
	})

	t.Run("a request handled before it's read is skipped", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		store := fakeKV(api)

		require.NoError(t, p.migratePendingRequest(kvRequestPrefix+r1, 10))
		require.Empty(t, store)
	})

	t.Run("an unreadable Channel Admin request is skipped", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLines(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		store := fakeKV(api)
		store[kvAdminRequestPrefix+a1] = []byte("{")

		require.NoError(t, p.migratePendingRequest(kvAdminRequestPrefix+a1, 10))
		api.AssertCalled(t, "LogWarn", "skipping unreadable stored request", "key", kvAdminRequestPrefix+a1, "error", mock.Anything)
	})

	t.Run("a claim that can't be cleared is logged", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLines(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVList", 0, 200).Return(nil, testAppErr("kv down"))
		api.On("KVDelete", kvPendingMigrated).Return(testAppErr("kv down"))
		fakeKV(api)

		require.Error(t, p.migratePendingRequests())
		api.AssertCalled(t, "LogWarn", "failed to clear pending-request migration claim", "error", mock.Anything)
	})

	t.Run("failures leave the migration to run again", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLines(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVCompareAndSet", isPendingListKey, mock.Anything, mock.Anything).Return(false, testAppErr("kv down"))
		store := fakeKV(api)
		store[kvRequestPrefix+r1] = recordJSON(t, channelRequest{ID: r1, RequesterID: u1, TeamID: "team1", Name: "team-x", CreatedAt: 5})

		require.EqualError(t, p.migratePendingRequests(), "1 stored requests could not be updated")
		require.NotContains(t, store, kvPendingMigrated)
		api.AssertCalled(t, "LogWarn", "failed to update stored request; will retry on a later activation", "key", kvRequestPrefix+r1, "error", mock.Anything)
	})

	t.Run("store errors", func(t *testing.T) {
		for name, tc := range map[string]struct {
			setup func(api *plugintest.API)
			want  string
		}{
			"flag read fails": {
				setup: func(api *plugintest.API) { api.On("KVGet", kvPendingMigrated).Return(nil, testAppErr("kv down")) },
				want:  "failed to check pending-request migration",
			},
			"claim fails": {
				setup: func(api *plugintest.API) {
					api.On("KVSetWithOptions", kvPendingMigrated, mock.Anything, mock.Anything).Return(false, testAppErr("kv down"))
				},
				want: "failed to start pending-request migration",
			},
			"list fails": {
				setup: func(api *plugintest.API) { api.On("KVList", 0, 200).Return(nil, testAppErr("kv down")) },
				want:  "failed to list stored requests",
			},
			"flag write fails": {
				setup: func(api *plugintest.API) {
					api.On("KVSet", kvPendingMigrated, []byte(migrationDone)).Return(testAppErr("kv down"))
				},
				want: "failed to record pending-request migration",
			},
		} {
			t.Run(name, func(t *testing.T) {
				api := &plugintest.API{}
				stubLogLines(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)
				tc.setup(api)
				fakeKV(api)

				require.ErrorContains(t, p.migratePendingRequests(), tc.want)
			})
		}
	})

	t.Run("record errors are returned", func(t *testing.T) {
		for name, setup := range map[string]func(api *plugintest.API){
			"read fails": func(api *plugintest.API) {
				api.On("KVGet", kvRequestPrefix+r1).Return(nil, testAppErr("kv down"))
			},
			"update fails": func(api *plugintest.API) {
				api.On("KVCompareAndSet", kvRequestPrefix+r1, mock.Anything, mock.Anything).Return(false, testAppErr("kv down"))
			},
			"existence check fails": func(api *plugintest.API) {
				reads := 0
				api.On("KVGet", kvRequestPrefix+r1).Return(func(string) ([]byte, *model.AppError) {
					reads++
					if reads == 1 {
						return recordJSON(t, channelRequest{ID: r1, RequesterID: u1, TeamID: "team1", Name: "team-x"}), nil
					}
					return nil, testAppErr("kv down")
				})
			},
		} {
			t.Run(name, func(t *testing.T) {
				api := &plugintest.API{}
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)
				setup(api)
				store := fakeKV(api)
				store[kvRequestPrefix+r1] = recordJSON(t, channelRequest{ID: r1, RequesterID: u1, TeamID: "team1", Name: "team-x"})

				require.Error(t, p.migratePendingRequest(kvRequestPrefix+r1, 10))
			})
		}
	})
}

// fakeKV backs the KV calls the migration makes with a map. Expectations
// registered on api before calling it take precedence.
func fakeKV(api *plugintest.API) map[string][]byte {
	store := map[string][]byte{}
	api.On("KVGet", mock.Anything).Return(func(key string) ([]byte, *model.AppError) {
		return store[key], nil
	}).Maybe()
	api.On("KVSet", mock.Anything, mock.Anything).Return(func(key string, value []byte) *model.AppError {
		store[key] = value
		return nil
	}).Maybe()
	api.On("KVDelete", mock.Anything).Return(func(key string) *model.AppError {
		delete(store, key)
		return nil
	}).Maybe()
	api.On("KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything).Return(func(key string, oldValue, newValue []byte) (bool, *model.AppError) {
		if !bytes.Equal(store[key], oldValue) {
			return false, nil
		}
		store[key] = newValue
		return true, nil
	}).Maybe()
	api.On("KVCompareAndDelete", mock.Anything, mock.Anything).Return(func(key string, oldValue []byte) (bool, *model.AppError) {
		if !bytes.Equal(store[key], oldValue) {
			return false, nil
		}
		delete(store, key)
		return true, nil
	}).Maybe()
	api.On("KVSetWithOptions", mock.Anything, mock.Anything, mock.Anything).Return(func(key string, value []byte, opts model.PluginKVSetOptions) (bool, *model.AppError) {
		if opts.Atomic && !bytes.Equal(store[key], opts.OldValue) {
			return false, nil
		}
		store[key] = value
		return true, nil
	}).Maybe()
	api.On("KVList", mock.Anything, mock.Anything).Return(func(page, perPage int) ([]string, *model.AppError) {
		keys := make([]string, 0, len(store))
		for k := range store {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		start := min(page*perPage, len(keys))
		return keys[start:min(start+perPage, len(keys))], nil
	}).Maybe()
	return store
}

// recordJSON encodes a stored request record.
func recordJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

// fakePendingLists backs the pending-list KV calls with a map, so tests
// can check what ends up stored.
func fakePendingLists(api *plugintest.API) map[string][]byte {
	stubApprovalPosts(api)
	store := map[string][]byte{}
	api.On("KVGet", isPendingListKey).Return(func(key string) ([]byte, *model.AppError) {
		return store[key], nil
	}).Maybe()
	api.On("KVCompareAndSet", isPendingListKey, mock.Anything, mock.Anything).Return(func(key string, oldValue, newValue []byte) (bool, *model.AppError) {
		if !bytes.Equal(store[key], oldValue) {
			return false, nil
		}
		store[key] = newValue
		return true, nil
	}).Maybe()
	api.On("KVCompareAndDelete", isPendingListKey, mock.Anything).Return(func(key string, oldValue []byte) (bool, *model.AppError) {
		if !bytes.Equal(store[key], oldValue) {
			return false, nil
		}
		delete(store, key)
		return true, nil
	}).Maybe()
	return store
}
