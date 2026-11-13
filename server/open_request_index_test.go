package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestUnionStringKeys(t *testing.T) {
	require.Equal(t, []string{"a", "b", "c"}, unionStringKeys([]string{"a", "b"}, []string{"b", "c", ""}))
	require.Empty(t, unionStringKeys(nil, nil))
}

func TestPersistOpenRequestIndexFromScan_CASSuccess(t *testing.T) {
	api := &plugintest.API{}
	// Register before stubLogLevels — that helper also Maybe-stubs this CAS.
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	require.NoError(t, p.persistOpenRequestIndexFromScan(nil, []string{kvBotRequestPrefix + "r1"}))
}

func TestPersistOpenRequestIndexFromScan_ConflictMergesConcurrentTrack(t *testing.T) {
	api := &plugintest.API{}
	scanned := []string{kvBotRequestPrefix + "old"}
	liveJSON, _ := json.Marshal([]string{kvBotRequestPrefix + "new"})

	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).Return(false, nil).Once()
	api.On("KVGet", kvOpenRequestIndex).Return(liveJSON, nil).Once()
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			var got []string
			require.NoError(t, json.Unmarshal(args.Get(2).([]byte), &got))
			require.ElementsMatch(t, []string{kvBotRequestPrefix + "old", kvBotRequestPrefix + "new"}, got)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	require.NoError(t, p.persistOpenRequestIndexFromScan(nil, scanned))
}

func TestRebuildOpenRequestIndex_UsesCASAgainstPreScanValue(t *testing.T) {
	api := &plugintest.API{}
	key := kvBotRequestPrefix + "r1"
	partial, err := marshalPartialKeyIndex([]string{key})
	require.NoError(t, err)
	api.On("KVGet", kvOpenRequestIndex).Return(nil, nil).Once()
	api.On("KVList", 0, 200).Return([]string{key, "noise"}, nil).Twice()
	api.On("KVCompareAndSet", kvOpenRequestIndex, []byte(nil), mock.MatchedBy(func(b []byte) bool {
		keys, complete, ok := parseKeyIndex(b)
		return ok && !complete && len(keys) == 1 && keys[0] == key
	})).Return(true, nil).Once()
	api.On("KVGet", kvOpenRequestIndex).Return(partial, nil).Once()
	api.On("KVCompareAndSet", kvOpenRequestIndex, partial, mock.MatchedBy(func(b []byte) bool {
		keys, complete, ok := parseKeyIndex(b)
		return ok && complete && len(keys) == 1 && keys[0] == key
	})).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	p.rebuildOpenRequestIndex()
}

// A key present before the scan but skipped by offset paging must survive
// rebuild: the scan only adds.
func TestRebuildOpenRequestIndex_UnionsPreScanIndex(t *testing.T) {
	api := &plugintest.API{}
	tracked := kvBotRequestPrefix + "tracked"
	scanned := kvTeamRequestPrefix + "scanned"
	prev, err := json.Marshal([]string{tracked})
	require.NoError(t, err)
	api.On("KVGet", kvOpenRequestIndex).Return(prev, nil).Once()
	api.On("KVList", 0, 200).Return([]string{scanned}, nil)
	api.On("KVCompareAndSet", kvOpenRequestIndex, prev, mock.Anything).
		Run(func(args mock.Arguments) {
			keys, complete, ok := parseKeyIndex(args.Get(2).([]byte))
			require.True(t, ok)
			require.True(t, complete)
			require.ElementsMatch(t, []string{scanned, tracked}, keys)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	p.rebuildOpenRequestIndex()
}

func TestListRequestKeys_RebuildDoesNotKVSet(t *testing.T) {
	api := &plugintest.API{}
	api.On("KVGet", kvOpenRequestIndex).Return(nil, nil)
	api.On("KVList", 0, 200).Return([]string{kvTeamRequestPrefix + "a"}, nil)
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).Return(true, nil)
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	keys, err := p.listRequestKeys()
	require.NoError(t, err)
	require.Equal(t, []string{kvTeamRequestPrefix + "a"}, keys)
	api.AssertNotCalled(t, "KVSet", kvOpenRequestIndex, mock.Anything)
	// Missing-index bootstrap: partial publish + complete persist.
	api.AssertNumberOfCalls(t, "KVCompareAndSet", 2)
	api.AssertNumberOfCalls(t, "KVList", 2)
}

// A key tracked in a partial index but skipped by the paged scan must stay
// in both the listing and the first complete index written from it.
func TestListRequestKeys_UnionsPartialPreScanIndex(t *testing.T) {
	api := &plugintest.API{}
	tracked := kvBotRequestPrefix + "tracked"
	scanned := kvTeamRequestPrefix + "scanned"
	partial, err := marshalPartialKeyIndex([]string{tracked})
	require.NoError(t, err)
	api.On("KVGet", kvOpenRequestIndex).Return(partial, nil)
	api.On("KVList", 0, 200).Return([]string{scanned}, nil)
	api.On("KVCompareAndSet", kvOpenRequestIndex, partial, mock.Anything).
		Run(func(args mock.Arguments) {
			keys, complete, ok := parseKeyIndex(args.Get(2).([]byte))
			require.True(t, ok)
			require.True(t, complete)
			require.ElementsMatch(t, []string{scanned, tracked}, keys)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	keys, err := p.listRequestKeys()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{scanned, tracked}, keys)
}

func TestTrackOpenRequestKey_MissingIndexWritesPartialNotComplete(t *testing.T) {
	api := &plugintest.API{}
	key := kvBotRequestPrefix + "r1"
	api.On("KVGet", kvOpenRequestIndex).Return(nil, nil).Once()
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			keys, complete, ok := parseKeyIndex(args.Get(2).([]byte))
			require.True(t, ok)
			require.False(t, complete, "must not invent a complete one-key index")
			require.Equal(t, []string{key}, keys)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	p.trackOpenRequestKey(key)
	api.AssertNotCalled(t, "KVSet", kvOpenRequestIndex, mock.Anything)
	api.AssertNotCalled(t, "KVList", mock.Anything, mock.Anything)
}

func TestPersistOpenRequestIndexFromScan_ConflictMergesPartialTrack(t *testing.T) {
	api := &plugintest.API{}
	scanned := []string{kvBotRequestPrefix + "old"}
	partial, err := marshalPartialKeyIndex([]string{kvBotRequestPrefix + "new"})
	require.NoError(t, err)

	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).Return(false, nil).Once()
	api.On("KVGet", kvOpenRequestIndex).Return(partial, nil).Once()
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			got, complete, ok := parseKeyIndex(args.Get(2).([]byte))
			require.True(t, ok)
			require.True(t, complete)
			require.ElementsMatch(t, []string{kvBotRequestPrefix + "old", kvBotRequestPrefix + "new"}, got)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	require.NoError(t, p.persistOpenRequestIndexFromScan(nil, scanned))
}

func TestLoadOpenRequestIndex_PartialIsNotReadable(t *testing.T) {
	api := &plugintest.API{}
	partial, err := marshalPartialKeyIndex([]string{kvBotRequestPrefix + "r1"})
	require.NoError(t, err)
	api.On("KVGet", kvOpenRequestIndex).Return(partial, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	keys, ok := p.loadOpenRequestIndex()
	require.False(t, ok)
	require.Nil(t, keys)
}

func TestTrackOpenRequestKey_CASGiveUpInvalidates(t *testing.T) {
	api := &plugintest.API{}
	key := kvBotRequestPrefix + "r1"
	existing, _ := json.Marshal([]string{kvBotRequestPrefix + "other"})
	for range openRequestIndexCASAttempts {
		api.On("KVGet", kvOpenRequestIndex).Return(existing, nil).Once()
		api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).Return(false, nil).Once()
	}
	// Invalidate writes a partial blob (with the failed key) instead of deleting.
	api.On("KVGet", kvOpenRequestIndex).Return(existing, nil).Once()
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			keys, complete, ok := parseKeyIndex(args.Get(2).([]byte))
			require.True(t, ok)
			require.False(t, complete)
			require.Contains(t, keys, key)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	p.trackOpenRequestKey(key)
	api.AssertNotCalled(t, "KVDelete", kvOpenRequestIndex)
}

func TestUntrackOpenRequestKey_CompletePartialAndMissing(t *testing.T) {
	t.Run("complete index drops key", func(t *testing.T) {
		api := &plugintest.API{}
		key := kvBotRequestPrefix + "r1"
		existing, _ := json.Marshal([]string{key, kvBotRequestPrefix + "r2"})
		api.On("KVGet", kvOpenRequestIndex).Return(existing, nil).Once()
		api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				got, complete, ok := parseKeyIndex(args.Get(2).([]byte))
				require.True(t, ok)
				require.True(t, complete)
				require.Equal(t, []string{kvBotRequestPrefix + "r2"}, got)
			}).Return(true, nil).Once()
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		newTestPlugin(api).untrackOpenRequestKey(key)
	})
	t.Run("partial index drops key", func(t *testing.T) {
		api := &plugintest.API{}
		key := kvBotRequestPrefix + "r1"
		partial, err := marshalPartialKeyIndex([]string{key})
		require.NoError(t, err)
		api.On("KVGet", kvOpenRequestIndex).Return(partial, nil).Once()
		api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				got, complete, ok := parseKeyIndex(args.Get(2).([]byte))
				require.True(t, ok)
				require.False(t, complete)
				require.Empty(t, got)
			}).Return(true, nil).Once()
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		newTestPlugin(api).untrackOpenRequestKey(key)
	})
	t.Run("missing index is a no-op", func(t *testing.T) {
		api := &plugintest.API{}
		api.On("KVGet", kvOpenRequestIndex).Return(nil, nil).Once()
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		newTestPlugin(api).untrackOpenRequestKey(kvBotRequestPrefix + "r1")
		api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
	})
}

func TestPutPendingRequestKV_TracksKeyInCompleteIndex(t *testing.T) {
	api := &plugintest.API{}
	key := kvBotRequestPrefix + "r1"
	existing, _ := json.Marshal([]string{kvBotRequestPrefix + "other"})
	api.On("KVSet", key, []byte(`{"id":"r1"}`)).Return(nil).Once()
	api.On("KVGet", kvOpenRequestIndex).Return(existing, nil).Once()
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			got, complete, ok := parseKeyIndex(args.Get(2).([]byte))
			require.True(t, ok)
			require.True(t, complete)
			require.ElementsMatch(t, []string{kvBotRequestPrefix + "other", key}, got)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	require.NoError(t, p.putPendingRequestKV(key, []byte(`{"id":"r1"}`)))
}

func TestClaimRecord_UntracksOpenRequestKey(t *testing.T) {
	api := &plugintest.API{}
	key := kvBotRequestPrefix + "r1"
	raw := []byte(`{}`)
	existing, _ := json.Marshal([]string{key, kvBotRequestPrefix + "r2"})
	api.On("KVCompareAndDelete", key, raw).Return(true, nil).Once()
	api.On("KVGet", kvOpenRequestIndex).Return(existing, nil).Once()
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			got, complete, ok := parseKeyIndex(args.Get(2).([]byte))
			require.True(t, ok)
			require.True(t, complete)
			require.Equal(t, []string{kvBotRequestPrefix + "r2"}, got)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	_, ok := p.claimRecord(key, raw, decisionCards{}, "test")
	require.True(t, ok)
}
