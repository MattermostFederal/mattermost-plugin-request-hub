package main

import (
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// memKV wires the KV methods the transfer/index code uses against an
// in-memory map so round-trips (store → load → list → retire) behave like a
// real store without hand-rolling every expectation.
func memKV(api *plugintest.API) map[string][]byte {
	store := map[string][]byte{}
	api.On("KVSet", mock.Anything, mock.Anything).Return(func(key string, value []byte) *model.AppError {
		store[key] = value
		return nil
	})
	api.On("KVGet", mock.Anything).Return(func(key string) []byte { return store[key] }, func(string) *model.AppError { return nil })
	api.On("KVDelete", mock.Anything).Return(func(key string) *model.AppError {
		delete(store, key)
		return nil
	})
	api.On("KVList", mock.Anything, mock.Anything).Return(func(page, perPage int) []string {
		if page > 0 {
			return nil
		}
		keys := make([]string, 0, len(store))
		for k := range store {
			keys = append(keys, k)
		}
		return keys
	}, func(int, int) *model.AppError { return nil })
	api.On("KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything).Return(
		func(key string, old, value []byte) (bool, *model.AppError) {
			cur, ok := store[key]
			if old == nil {
				if ok && len(cur) > 0 {
					return false, nil
				}
				store[key] = value
				return true, nil
			}
			if !ok || string(cur) != string(old) {
				return false, nil
			}
			store[key] = value
			return true, nil
		},
	).Maybe()
	api.On("KVCompareAndDelete", mock.Anything, mock.Anything).Return(
		func(key string, old []byte) (bool, *model.AppError) {
			cur, ok := store[key]
			if !ok || string(cur) != string(old) {
				return false, nil
			}
			delete(store, key)
			return true, nil
		},
	).Maybe()
	return store
}

func TestRotateDefaultOn(t *testing.T) {
	require.True(t, rotateDefaultOn(transferReasonOffboarding))
	require.True(t, rotateDefaultOn(transferReasonOwnerDeactivated))
	require.True(t, rotateDefaultOn("  Offboarding  "))
	require.False(t, rotateDefaultOn(transferReasonVoluntary))
	require.False(t, rotateDefaultOn(""))
}

func TestClaimTransferAsset_CAS(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := fakeKV(api)
	p := newTestPlugin(api)

	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-a"))
	require.Equal(t, "req-a", string(store[transferAssetClaimKey(pendingKindBotTransfer, "bot1")]))

	// Live holder blocks a second claim.
	store[kvBotTransferRequestPrefix+"req-a"] = []byte(`{"id":"req-a"}`)
	err := p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already")

	// Stale holder (request gone) is overwritten.
	delete(store, kvBotTransferRequestPrefix+"req-a")
	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b"))
	require.Equal(t, "req-b", string(store[transferAssetClaimKey(pendingKindBotTransfer, "bot1")]))

	p.releaseTransferAssetClaim(pendingKindBotTransfer, "bot1", "req-b")
	_, held := store[transferAssetClaimKey(pendingKindBotTransfer, "bot1")]
	require.False(t, held)
}

func TestWebhookIndexRoundTrip(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	memKV(api)
	p := newTestPlugin(api)

	req := &webhookRequest{
		ID: "r1", RequesterID: "u_req", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", TeamID: "t1",
		DisplayName: "Relay", CallbackURL: "https://hooks.example.com/out",
	}
	p.writeWebhookIndex(req, "hook1", "admin1")

	got, err := p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "u_req", got.OwnerUserID)
	require.Equal(t, "u_req", got.RequesterID)
	require.Equal(t, webhookTypeOutgoing, got.WebhookType)
	require.Equal(t, "https://hooks.example.com/out", got.CallbackURL)
	require.Zero(t, got.RetiredAt)

	p.updateWebhookIndexOwner("hook1", "u_new")
	got, err = p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.Equal(t, "u_new", got.OwnerUserID)

	all, err := p.listWebhookIndex()
	require.NoError(t, err)
	require.Len(t, all, 1)

	p.retireWebhookIndex("hook1")
	got, err = p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.NotZero(t, got.RetiredAt)
}

func TestWriteWebhookIndexSkipsExerciseAndEmptyHook(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	store := memKV(api)
	p := newTestPlugin(api)

	p.writeWebhookIndex(&webhookRequest{ID: "r1", Exercise: true, WebhookType: webhookTypeIncoming}, "hook1", "a")
	p.writeWebhookIndex(&webhookRequest{ID: "r2", WebhookType: webhookTypeIncoming}, "", "a")
	require.Empty(t, store)
}

func TestCanNominateTransfer(t *testing.T) {
	const owner, requester, other, hookOwner = "owner_id", "requester_id", "other_id", "owner_id"

	setup := func(ownerActive bool) *Plugin {
		api := &plugintest.API{}
		stubLogs(api)
		api.On("GetUser", testSysAdminID).Return(systemAdminUser(), nil).Maybe()
		api.On("GetUser", other).Return(&model.User{Id: other, Roles: "system_user"}, nil).Maybe()
		api.On("GetUser", requester).Return(&model.User{Id: requester, Roles: "system_user"}, nil).Maybe()
		if ownerActive {
			api.On("GetUser", owner).Return(&model.User{Id: owner, Roles: "system_user"}, nil).Maybe()
		} else {
			api.On("GetUser", owner).Return(&model.User{Id: owner, DeleteAt: 1}, nil).Maybe()
		}
		api.On("GetChannelMember", "ch1", mock.Anything).Return(nil, testAppErr("not a member")).Maybe()
		p := newTestPlugin(api)
		return p
	}

	asset := transferAsset{assetType: "bot", assetID: "b1", ownerUserID: owner, requesterID: requester, channelID: ""}

	t.Run("sysadmin always may", func(t *testing.T) {
		require.NoError(t, setup(true).canNominateTransfer(testSysAdminID, asset))
	})
	t.Run("active owner may", func(t *testing.T) {
		require.NoError(t, setup(true).canNominateTransfer(owner, asset))
	})
	t.Run("stranger blocked while owner active", func(t *testing.T) {
		require.ErrorContains(t, setup(true).canNominateTransfer(other, asset), "only the current owner")
	})
	t.Run("original requester may once orphaned", func(t *testing.T) {
		require.NoError(t, setup(false).canNominateTransfer(requester, asset))
	})
	t.Run("stranger without channel still blocked when orphaned", func(t *testing.T) {
		require.ErrorContains(t, setup(false).canNominateTransfer(other, asset), "you must be its owner")
	})
	t.Run("channel member may once orphaned", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		api.On("GetUser", other).Return(&model.User{Id: other, Roles: "system_user"}, nil)
		api.On("GetUser", owner).Return(&model.User{Id: owner, DeleteAt: 1}, nil)
		api.On("GetChannelMember", "ch1", other).Return(&model.ChannelMember{}, nil)
		p := newTestPlugin(api)
		inChannel := transferAsset{assetType: "bot", assetID: "b1", ownerUserID: owner, requesterID: requester, channelID: "ch1"}
		require.NoError(t, p.canNominateTransfer(other, inChannel))
	})
	_ = hookOwner
}

func TestSubmitBotTransferRejects(t *testing.T) {
	t.Run("disabled by admin", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{})
		_, err := p.submitBotTransferRequest("u_req", botTransferInput{BotUserID: "b1", Action: transferActionTransfer})
		require.EqualError(t, err, botTransferRequestsDisabledMsg)
	})

	t.Run("bot not managed by RH", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		memKV(api) // empty index → not found
		api.On("GetUser", "u_req").Return(regularUser(), nil)
		p := newTestPlugin(api)
		_, err := p.submitBotTransferRequest("u_req", botTransferInput{BotUserID: "b1", Action: transferActionTransfer})
		require.ErrorContains(t, err, "isn't managed by Request Hub")
	})

	t.Run("unknown action", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		store := memKV(api)
		store[kvBotIndexPrefix+"b1"] = mustMarshal(t, botIndexEntry{BotUserID: "b1", Username: "svcbot", RequesterID: "u_req", OwnerUserID: "u_req"})
		api.On("GetUser", "u_req").Return(regularUser(), nil)
		p := newTestPlugin(api)
		_, err := p.submitBotTransferRequest("u_req", botTransferInput{BotUserID: "b1", Action: "frobnicate"})
		require.EqualError(t, err, "pick Transfer or Retire")
	})

	t.Run("transfer needs a new owner", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		store := memKV(api)
		store[kvBotIndexPrefix+"b1"] = mustMarshal(t, botIndexEntry{BotUserID: "b1", Username: "svcbot", RequesterID: "u_req", OwnerUserID: "u_req"})
		api.On("GetUser", "u_req").Return(regularUser(), nil)
		p := newTestPlugin(api)
		_, err := p.submitBotTransferRequest("u_req", botTransferInput{BotUserID: "b1", Action: transferActionTransfer})
		require.ErrorContains(t, err, "pick the new owner")
	})
}

func TestSubmitWebhookTransferRejects(t *testing.T) {
	t.Run("disabled by admin", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{})
		_, err := p.submitWebhookTransferRequest("u_req", webhookTransferInput{HookID: "h1", Action: transferActionReassign})
		require.EqualError(t, err, webhookTransferRequestsDisabledMsg)
	})

	t.Run("hook not tracked", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		memKV(api)
		api.On("GetUser", "u_req").Return(regularUser(), nil)
		p := newTestPlugin(api)
		_, err := p.submitWebhookTransferRequest("u_req", webhookTransferInput{HookID: "h1", Action: transferActionReassign})
		require.ErrorContains(t, err, "Request Hub")
	})
}

func TestBotIndexOwnerFallsBackToRequester(t *testing.T) {
	require.Equal(t, "owner", botIndexOwner(botIndexEntry{OwnerUserID: "owner", RequesterID: "req"}))
	require.Equal(t, "req", botIndexOwner(botIndexEntry{RequesterID: "req"}))
}

func TestResolveOutgoingWebhookCallback_Seam(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.getOutgoingWebhookCallbackFn = func(hookID, adminUserID string) (string, error) {
		require.Equal(t, "h1", hookID)
		require.Equal(t, "admin1", adminUserID)
		return "https://cb.example/out", nil
	}
	got, err := p.resolveOutgoingWebhookCallback("h1", "admin1")
	require.NoError(t, err)
	require.Equal(t, "https://cb.example/out", got)
}

func TestFulfillWebhookRetireRecreate_OutgoingUsesCallback(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	memKV(api)
	stubSiteURL(api, "http://localhost:8065")
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.auditSink = func(auditEntry, auditEventSpec) {}

	var gotCallback string
	p.deleteWebhookFn = func(webhookType, hookID string) error {
		require.Equal(t, webhookTypeOutgoing, webhookType)
		require.Equal(t, "old-hook", hookID)
		return nil
	}
	p.createWebhookFn = func(req *webhookRequest, adminUserID string) (string, string, error) {
		gotCallback = req.CallbackURL
		require.Equal(t, "u_new", req.RequesterID)
		require.Equal(t, webhookTypeOutgoing, req.WebhookType)
		return "Outgoing webhook created.", "new-hook", nil
	}
	api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_new", Username: "newowner"}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
	api.On("KVDelete", mock.Anything).Return(nil).Maybe()

	defaultDelete := p.deleteWebhookFn
	t.Run("refuses when live fetch fails even if snapshot has a callback", func(t *testing.T) {
		// Snapshot alone cannot prove triggers / extra callbacks were absent.
		gotCallback = ""
		deleted := false
		p.deleteWebhookFn = func(string, string) error {
			deleted = true
			return nil
		}
		p.getOutgoingWebhookCallbackFn = func(string, string) (string, error) {
			return "", errors.New("live unavailable")
		}
		raw := []byte(`{"id":"t1"}`)
		req := &webhookTransferRequest{
			ID: "t1", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
			DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
			CallbackURL: "https://hooks.example.com/from-index",
			Action:      transferActionRetireRecreate, NewOwnerUserID: "u_new",
		}
		res := p.fulfillWebhookRetireRecreate(req,
			&model.User{Id: "admin1", Username: "admin"},
			&model.User{Id: "u_req", Username: "req"},
			newDecisionCards("t1", "", ""), raw, kvWebhookTransferRequestPrefix+"t1")
		require.False(t, deleted, "must not delete when live fields cannot be confirmed")
		require.Empty(t, gotCallback)
		require.True(t, res.audited)
		p.deleteWebhookFn = defaultDelete
	})

	t.Run("prefers live callback over stale snapshot", func(t *testing.T) {
		gotCallback = ""
		createBeforeDelete := false
		deleted := false
		p.createWebhookFn = func(req *webhookRequest, adminUserID string) (string, string, error) {
			require.False(t, deleted, "must create replacement before deleting the old hook")
			createBeforeDelete = true
			gotCallback = req.CallbackURL
			return "Outgoing webhook created.", "new-hook", nil
		}
		p.deleteWebhookFn = func(string, string) error {
			require.True(t, createBeforeDelete)
			deleted = true
			return nil
		}
		p.getOutgoingWebhookCallbackFn = func(hookID, _ string) (string, error) {
			require.Equal(t, "old-hook", hookID)
			return "https://hooks.example.com/live", nil
		}
		req := &webhookTransferRequest{
			ID: "t2", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
			DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
			CallbackURL: "https://hooks.example.com/stale",
			Action:      transferActionRetireRecreate, NewOwnerUserID: "u_new",
		}
		res := p.fulfillWebhookRetireRecreate(req,
			&model.User{Id: "admin1", Username: "admin"},
			&model.User{Id: "u_req", Username: "req"},
			newDecisionCards("t2", "", ""), nil, kvWebhookTransferRequestPrefix+"t2")
		require.Equal(t, "https://hooks.example.com/live", gotCallback)
		require.True(t, createBeforeDelete && deleted)
		require.Contains(t, res.Outcome, "recreated")
		p.deleteWebhookFn = defaultDelete
		p.createWebhookFn = func(req *webhookRequest, adminUserID string) (string, string, error) {
			gotCallback = req.CallbackURL
			return "Outgoing webhook created.", "new-hook", nil
		}
	})

	t.Run("create failure leaves old hook intact", func(t *testing.T) {
		deleted := false
		p.deleteWebhookFn = func(string, string) error {
			deleted = true
			return nil
		}
		p.createWebhookFn = func(*webhookRequest, string) (string, string, error) {
			return "", "", errors.New("create failed")
		}
		p.getOutgoingWebhookCallbackFn = func(string, string) (string, error) {
			return "https://hooks.example.com/live", nil
		}
		raw := []byte(`{"id":"t-fail"}`)
		req := &webhookTransferRequest{
			ID: "t-fail", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
			DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
			Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
		}
		res := p.fulfillWebhookRetireRecreate(req,
			&model.User{Id: "admin1", Username: "admin"},
			&model.User{Id: "u_req", Username: "req"},
			newDecisionCards("t-fail", "", ""), raw, kvWebhookTransferRequestPrefix+"t-fail")
		require.False(t, deleted)
		require.True(t, res.audited)
		p.deleteWebhookFn = defaultDelete
	})

	t.Run("live fetch when snapshot empty", func(t *testing.T) {
		gotCallback = ""
		p.createWebhookFn = func(req *webhookRequest, adminUserID string) (string, string, error) {
			gotCallback = req.CallbackURL
			return "Outgoing webhook created.", "new-hook", nil
		}
		p.deleteWebhookFn = defaultDelete
		p.getOutgoingWebhookCallbackFn = func(hookID, _ string) (string, error) {
			require.Equal(t, "old-hook", hookID)
			return "https://hooks.example.com/live-only", nil
		}
		req := &webhookTransferRequest{
			ID: "t2b", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
			DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
			Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
		}
		res := p.fulfillWebhookRetireRecreate(req,
			&model.User{Id: "admin1", Username: "admin"},
			&model.User{Id: "u_req", Username: "req"},
			newDecisionCards("t2b", "", ""), nil, kvWebhookTransferRequestPrefix+"t2b")
		require.Equal(t, "https://hooks.example.com/live-only", gotCallback)
		require.Contains(t, res.Outcome, "recreated")
	})

	t.Run("blocks delete when callback cannot be resolved", func(t *testing.T) {
		deleted := false
		p.deleteWebhookFn = func(string, string) error {
			deleted = true
			return nil
		}
		p.getOutgoingWebhookCallbackFn = func(string, string) (string, error) {
			return "", errors.New("hook gone")
		}
		raw := []byte(`{"id":"t3"}`)
		req := &webhookTransferRequest{
			ID: "t3", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
			DisplayName: "Relay", Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
		}
		res := p.fulfillWebhookRetireRecreate(req,
			&model.User{Id: "admin1", Username: "admin"},
			&model.User{Id: "u_req", Username: "req"},
			newDecisionCards("t3", "", ""), raw, kvWebhookTransferRequestPrefix+"t3")
		require.False(t, deleted, "must not delete before callback is known")
		require.True(t, res.audited)
		require.True(t, strings.Contains(res.Reply, "Couldn't") || strings.Contains(res.Reply, "hook gone") || res.Banner != nil)
	})
}
