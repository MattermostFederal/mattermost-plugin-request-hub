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
	api.On("KVGet", kvOpenRequestIndex).Return(nil, nil).Once()
	api.On("KVList", 0, 200).Return([]string{kvBotRequestPrefix + "r1", "noise"}, nil)
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			var got []string
			require.NoError(t, json.Unmarshal(args.Get(2).([]byte), &got))
			require.Equal(t, []string{kvBotRequestPrefix + "r1"}, got)
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
	api.On("KVCompareAndSet", kvOpenRequestIndex, mock.Anything, mock.Anything).Return(true, nil).Once()
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	keys, err := p.listRequestKeys()
	require.NoError(t, err)
	require.Equal(t, []string{kvTeamRequestPrefix + "a"}, keys)
	api.AssertNotCalled(t, "KVSet", kvOpenRequestIndex, mock.Anything)
}
