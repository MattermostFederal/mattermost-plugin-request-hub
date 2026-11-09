package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestScanTokenExpiryKeys_ExcludesIndexKey(t *testing.T) {
	api := &plugintest.API{}
	api.On("KVList", 0, 200).Return([]string{
		botTokenExpiryKey("tok1"),
		kvTokenExpiryIndex, // shares bot_token_expiry_ prefix; must be skipped
		"other",
		botTokenExpiryKey("tok2"),
	}, nil)
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	keys, err := p.scanTokenExpiryKeys()
	require.NoError(t, err)
	require.Equal(t, []string{botTokenExpiryKey("tok1"), botTokenExpiryKey("tok2")}, keys)
}

func TestPersistTokenExpiryIndexFromScan_ConflictMergesConcurrentTrack(t *testing.T) {
	api := &plugintest.API{}
	scanned := []string{botTokenExpiryKey("old")}
	liveJSON, _ := json.Marshal([]string{botTokenExpiryKey("new")})

	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).Return(false, nil).Once()
	api.On("KVGet", kvTokenExpiryIndex).Return(liveJSON, nil).Once()
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			var got []string
			require.NoError(t, json.Unmarshal(args.Get(2).([]byte), &got))
			require.ElementsMatch(t, []string{botTokenExpiryKey("old"), botTokenExpiryKey("new")}, got)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	require.NoError(t, p.persistTokenExpiryIndexFromScan(nil, scanned))
}

func TestRebuildTokenExpiryIndex_UsesCAS(t *testing.T) {
	api := &plugintest.API{}
	api.On("KVGet", kvTokenExpiryIndex).Return(nil, nil).Once()
	api.On("KVList", 0, 200).Return([]string{botTokenExpiryKey("tok1"), kvTokenExpiryIndex}, nil)
	api.On("KVCompareAndSet", kvTokenExpiryIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			var got []string
			require.NoError(t, json.Unmarshal(args.Get(2).([]byte), &got))
			require.Equal(t, []string{botTokenExpiryKey("tok1")}, got)
		}).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	p.rebuildTokenExpiryIndex()
}
