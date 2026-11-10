package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/require"
)

func TestHistoryMatchesExportFilter(t *testing.T) {
	h := requestHistory{RequestID: "r1", Kind: "bot", RequesterID: "u1", CreatedAt: 100, UpdatedAt: 200}
	require.True(t, historyMatchesExportFilter(h, auditExportFilter{}))
	require.False(t, historyMatchesExportFilter(h, auditExportFilter{RequestID: "other"}))
	require.False(t, historyMatchesExportFilter(h, auditExportFilter{Kind: "team"}))
	require.False(t, historyMatchesExportFilter(h, auditExportFilter{Requester: "u2"}))
	require.False(t, historyMatchesExportFilter(h, auditExportFilter{Since: 201}))
	require.False(t, historyMatchesExportFilter(h, auditExportFilter{Until: 99}))
}

func TestCapHistoriesEvents_StopsAtLimitAndMarksTruncation(t *testing.T) {
	in := []requestHistory{
		{RequestID: "a", Events: []auditEvent{{At: 1}, {At: 2}, {At: 3}}},
		{RequestID: "b", Events: []auditEvent{{At: 4}, {At: 5}}},
	}
	out, truncated := capHistoriesEvents(in, auditExportFilter{}, 3)
	require.True(t, truncated)
	require.Len(t, out, 1)
	require.Equal(t, "a", out[0].RequestID)
	require.Len(t, out[0].Events, 3)

	exact, truncatedExact := capHistoriesEvents(in, auditExportFilter{}, 5)
	require.False(t, truncatedExact)
	require.Len(t, exact, 2)
	require.Len(t, exact[0].Events, 3)
	require.Len(t, exact[1].Events, 2)
}

func TestCapHistoriesEvents_MarksTruncationAtHistoryBoundary(t *testing.T) {
	// Cap lands on the last event of history a; history b still has eligible events.
	in := []requestHistory{
		{RequestID: "a", Events: []auditEvent{{At: 1}, {At: 2}}},
		{RequestID: "b", Events: []auditEvent{{At: 3}}},
	}
	out, truncated := capHistoriesEvents(in, auditExportFilter{}, 2)
	require.True(t, truncated)
	require.Len(t, out, 1)
	require.Equal(t, "a", out[0].RequestID)
	require.Len(t, out[0].Events, 2)
}

func TestBuildHistoryExport_CSVMarksTruncationAtHistoryBoundary(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	prevH, prevE := historyExportMaxHistories, historyExportMaxEvents
	historyExportMaxHistories = 10
	historyExportMaxEvents = 2
	t.Cleanup(func() {
		historyExportMaxHistories = prevH
		historyExportMaxEvents = prevE
	})

	h1, _ := json.Marshal(requestHistory{RequestID: "r1", Kind: "bot", CreatedAt: 1, UpdatedAt: 2, Events: []auditEvent{{At: 1}, {At: 2}}})
	h2, _ := json.Marshal(requestHistory{RequestID: "r2", Kind: "bot", CreatedAt: 3, UpdatedAt: 4, Events: []auditEvent{{At: 3}}})
	api.On("KVList", 0, historyListPageSize).Return([]string{kvHistoryPrefix + "r1", kvHistoryPrefix + "r2"}, nil)
	api.On("KVGet", kvHistoryPrefix+"r1").Return(h1, nil)
	api.On("KVGet", kvHistoryPrefix+"r2").Return(h2, nil)

	exp, err := p.buildHistoryExport(auditExportFilter{Format: "csv", Kind: "bot"}, "test")
	require.NoError(t, err)
	require.Equal(t, true, exp.detail["truncated"])
	require.Contains(t, string(exp.data), "TRUNCATED")
}

func TestListHistoriesForExport_KeepsNewestAndMarksTruncation(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	prev := historyExportMaxHistories
	historyExportMaxHistories = 2
	t.Cleanup(func() { historyExportMaxHistories = prev })

	h1, _ := json.Marshal(requestHistory{RequestID: "r1", Kind: "bot", CreatedAt: 1, UpdatedAt: 10})
	h2, _ := json.Marshal(requestHistory{RequestID: "r2", Kind: "bot", CreatedAt: 2, UpdatedAt: 30})
	h3, _ := json.Marshal(requestHistory{RequestID: "r3", Kind: "bot", CreatedAt: 3, UpdatedAt: 20})
	api.On("KVList", 0, historyListPageSize).Return([]string{
		kvHistoryPrefix + "r1", kvHistoryPrefix + "r2", kvHistoryPrefix + "r3",
	}, nil)
	api.On("KVGet", kvHistoryPrefix+"r1").Return(h1, nil)
	api.On("KVGet", kvHistoryPrefix+"r2").Return(h2, nil)
	api.On("KVGet", kvHistoryPrefix+"r3").Return(h3, nil)

	got, truncated, err := p.listHistoriesForExport(auditExportFilter{Kind: "bot"})
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, got, 2)
	require.Equal(t, "r2", got[0].RequestID) // newest UpdatedAt
	require.Equal(t, "r3", got[1].RequestID)
}

func TestBuildHistoryExport_JSONStaysArrayWhenTruncated(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	prevH, prevE := historyExportMaxHistories, historyExportMaxEvents
	historyExportMaxHistories = 1
	historyExportMaxEvents = 100
	t.Cleanup(func() {
		historyExportMaxHistories = prevH
		historyExportMaxEvents = prevE
	})

	h1, _ := json.Marshal(requestHistory{RequestID: "r1", Kind: "bot", CreatedAt: 1, UpdatedAt: 1, Events: []auditEvent{{At: 1}}})
	h2, _ := json.Marshal(requestHistory{RequestID: "r2", Kind: "bot", CreatedAt: 2, UpdatedAt: 2, Events: []auditEvent{{At: 2}}})
	api.On("KVList", 0, historyListPageSize).Return([]string{kvHistoryPrefix + "r1", kvHistoryPrefix + "r2"}, nil)
	api.On("KVGet", kvHistoryPrefix+"r1").Return(h1, nil)
	api.On("KVGet", kvHistoryPrefix+"r2").Return(h2, nil)

	exp, err := p.buildHistoryExport(auditExportFilter{Format: "json", Kind: "bot"}, "test")
	require.NoError(t, err)
	require.Equal(t, true, exp.detail["truncated"])

	var decoded []requestHistory
	require.NoError(t, json.Unmarshal(exp.data, &decoded), "truncated JSON must remain a histories array")
	require.Len(t, decoded, 1)
}
