package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/require"
)

// backfillHistory is an approved standalone outgoing webhook request filed
// by requesterID, with hookID recorded on its approval event.
func backfillHistory(t *testing.T, requestID, requesterID, hookID string) []byte {
	t.Helper()
	snap, err := json.Marshal(&webhookRequest{
		ID: requestID, RequesterID: requesterID, WebhookType: webhookTypeOutgoing,
		ChannelID: "ch1", TeamID: "t1", DisplayName: "Relay", CallbackURL: "https://hooks.example.com/out",
	})
	require.NoError(t, err)
	return recordJSON(t, &requestHistory{
		RequestID: requestID, Kind: pendingKindWebhook, RequesterID: requesterID, Status: historyApproved,
		Request: snap,
		Events:  []auditEvent{{Event: eventApproved, Detail: map[string]any{"hook_id": hookID}}},
	})
}

func TestBackfillWebhookIndex_UsesTheLiveOwner(t *testing.T) {
	requesterID := model.NewId()
	liveOwnerID := model.NewId()
	readable, unreadable := model.NewId(), model.NewId()

	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)
	store[kvHistoryPrefix+"r-ok"] = backfillHistory(t, "r-ok", requesterID, readable)
	store[kvHistoryPrefix+"r-blind"] = backfillHistory(t, "r-blind", requesterID, unreadable)

	adminID := model.NewId()
	api.On("GetUsers", &model.UserGetOptions{Role: model.SystemAdminRoleId, Active: true, Page: 0, PerPage: 1}).
		Return([]*model.User{{Id: adminID}}, nil).Once()
	p.liveWebhookOwnerFn = func(_, hookID, actingID string) (string, error) {
		require.Equal(t, adminID, actingID)
		if hookID == readable {
			return liveOwnerID, nil
		}
		return "", testAppErr("cannot read hook")
	}

	p.backfillWebhookIndexFromHistory()

	indexed, err := p.loadWebhookIndex(readable)
	require.NoError(t, err)
	require.NotNil(t, indexed)
	// The hook changed hands before the index existed; handing transfer
	// rights back to the original requester would be a privilege regression.
	require.Equal(t, liveOwnerID, indexed.OwnerUserID)
	require.Equal(t, requesterID, indexed.RequesterID)

	skipped, err := p.loadWebhookIndex(unreadable)
	require.NoError(t, err)
	require.Nil(t, skipped, "a hook whose owner cannot be read waits for a later run")
	api.AssertExpectations(t)
}
