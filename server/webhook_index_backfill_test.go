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

// backfillIncomingHistory is an approved standalone incoming webhook
// request. Incoming approvals record a transfer handle rather than a hook
// ID — the ID is the secret URL — so there is nothing in history to index
// from.
func backfillIncomingHistory(t *testing.T, requestID, requesterID, displayName string) []byte {
	t.Helper()
	snap, err := json.Marshal(&webhookRequest{
		ID: requestID, RequesterID: requesterID, WebhookType: webhookTypeIncoming,
		ChannelID: "ch1", TeamID: "t1", DisplayName: displayName,
	})
	require.NoError(t, err)
	return recordJSON(t, &requestHistory{
		RequestID: requestID, Kind: pendingKindWebhook, RequesterID: requesterID, Status: historyApproved,
		Request: snap,
		Events:  []auditEvent{{Event: eventApproved, Detail: map[string]any{"transfer_handle": model.NewId()}}},
	})
}

// An incoming hook is identified against the live hooks, by the fields the
// create call took from the request. No secret goes into the index beyond
// the hook ID the index is keyed on everywhere else.
func TestBackfillWebhookIndex_IdentifiesIncomingHooksFromTheLiveList(t *testing.T) {
	requesterID := model.NewId()
	hookID := model.NewId()

	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)
	store[kvHistoryPrefix+"r-in"] = backfillIncomingHistory(t, "r-in", requesterID, "Alerts")

	adminID := model.NewId()
	api.On("GetUsers", &model.UserGetOptions{Role: model.SystemAdminRoleId, Active: true, Page: 0, PerPage: 1}).
		Return([]*model.User{{Id: adminID}}, nil).Once()
	listed := 0
	p.listIncomingWebhooksFn = func(teamID, actingID string) ([]*model.IncomingWebhook, error) {
		listed++
		require.Equal(t, "t1", teamID)
		require.Equal(t, adminID, actingID)
		return []*model.IncomingWebhook{
			{Id: hookID, ChannelId: "ch1", TeamId: "t1", UserId: requesterID, DisplayName: "Alerts"},
			// Same channel, somebody else's hook.
			{Id: model.NewId(), ChannelId: "ch1", TeamId: "t1", UserId: model.NewId(), DisplayName: "Alerts"},
		}, nil
	}
	p.incomingWebhookChannelFn = func(string, string) (string, error) { return "ch1", nil }
	p.liveWebhookOwnerFn = func(string, string, string) (string, error) { return requesterID, nil }

	p.backfillWebhookIndexFromHistory()

	indexed, err := p.loadWebhookIndex(hookID)
	require.NoError(t, err)
	require.NotNil(t, indexed)
	require.Equal(t, webhookTypeIncoming, indexed.WebhookType)
	require.Equal(t, requesterID, indexed.OwnerUserID)
	require.NotEmpty(t, indexed.TransferHandle, "the picker hands out the handle, never the hook ID")
	require.Equal(t, 1, listed, "a team's hooks are listed once per run")
}

// Two hooks that both fit the request is not a hook anyone can name.
// Guessing would hand one hook's transfer rights to the other's owner.
func TestBackfillWebhookIndex_SkipsAmbiguousIncomingMatches(t *testing.T) {
	requesterID := model.NewId()
	first, second := model.NewId(), model.NewId()

	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)
	store[kvHistoryPrefix+"r-in"] = backfillIncomingHistory(t, "r-in", requesterID, "Alerts")

	api.On("GetUsers", &model.UserGetOptions{Role: model.SystemAdminRoleId, Active: true, Page: 0, PerPage: 1}).
		Return([]*model.User{{Id: model.NewId()}}, nil).Once()
	p.listIncomingWebhooksFn = func(string, string) ([]*model.IncomingWebhook, error) {
		return []*model.IncomingWebhook{
			{Id: first, ChannelId: "ch1", TeamId: "t1", UserId: requesterID, DisplayName: "Alerts"},
			{Id: second, ChannelId: "ch1", TeamId: "t1", UserId: requesterID, DisplayName: "Alerts"},
		}, nil
	}

	p.backfillWebhookIndexFromHistory()

	for _, id := range []string{first, second} {
		indexed, err := p.loadWebhookIndex(id)
		require.NoError(t, err)
		require.Nil(t, indexed)
	}
}

func TestBackfillWebhookIndex_UsesTheLiveOwnerAndChannel(t *testing.T) {
	requesterID := model.NewId()
	liveOwnerID := model.NewId()
	readable, unreadable, movedBlind := model.NewId(), model.NewId(), model.NewId()

	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)
	store[kvHistoryPrefix+"r-ok"] = backfillHistory(t, "r-ok", requesterID, readable)
	store[kvHistoryPrefix+"r-blind"] = backfillHistory(t, "r-blind", requesterID, unreadable)
	store[kvHistoryPrefix+"r-nochannel"] = backfillHistory(t, "r-nochannel", requesterID, movedBlind)

	adminID := model.NewId()
	api.On("GetUsers", &model.UserGetOptions{Role: model.SystemAdminRoleId, Active: true, Page: 0, PerPage: 1}).
		Return([]*model.User{{Id: adminID}}, nil).Once()
	p.liveWebhookOwnerFn = func(_, hookID, actingID string) (string, error) {
		require.Equal(t, adminID, actingID)
		if hookID == unreadable {
			return "", testAppErr("cannot read hook")
		}
		return liveOwnerID, nil
	}
	// The readable hook was moved to another channel since it was approved.
	p.getOutgoingWebhookForRecreateFn = func(hookID, _ string) (*outgoingWebhookRecreate, error) {
		if hookID != readable {
			return nil, testAppErr("cannot read hook")
		}
		return &outgoingWebhookRecreate{ChannelID: "ch-live", CallbackURLs: []string{"https://hooks.example.com/out"}}, nil
	}

	p.backfillWebhookIndexFromHistory()

	indexed, err := p.loadWebhookIndex(readable)
	require.NoError(t, err)
	require.NotNil(t, indexed)
	// The hook changed hands before the index existed; handing transfer
	// rights back to the original requester would be a privilege regression.
	require.Equal(t, liveOwnerID, indexed.OwnerUserID)
	require.Equal(t, requesterID, indexed.RequesterID)
	// The channel grants the same rights once the hook is orphaned, so the
	// row records where the hook lives now rather than where it was filed.
	require.Equal(t, "ch-live", indexed.ChannelID)

	skipped, err := p.loadWebhookIndex(unreadable)
	require.NoError(t, err)
	require.Nil(t, skipped, "a hook whose owner cannot be read waits for a later run")

	noChannel, err := p.loadWebhookIndex(movedBlind)
	require.NoError(t, err)
	require.Nil(t, noChannel, "an unverifiable channel must not fall back to the historical one")
	api.AssertExpectations(t)
}
