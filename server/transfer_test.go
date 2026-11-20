package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	}).Maybe()
	api.On("KVGet", mock.Anything).Return(func(key string) ([]byte, *model.AppError) {
		return store[key], nil
	}).Maybe()
	api.On("KVDelete", mock.Anything).Return(func(key string) *model.AppError {
		delete(store, key)
		return nil
	}).Maybe()
	api.On("KVList", mock.Anything, mock.Anything).Return(func(page, perPage int) ([]string, *model.AppError) {
		if page > 0 {
			return nil, nil
		}
		keys := make([]string, 0, len(store))
		for k := range store {
			keys = append(keys, k)
		}
		return keys, nil
	}).Maybe()
	api.On("KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything).Return(
		func(key string, old, value []byte) (bool, *model.AppError) {
			if !bytes.Equal(store[key], old) {
				return false, nil
			}
			store[key] = value
			return true, nil
		},
	).Maybe()
	api.On("KVCompareAndDelete", mock.Anything, mock.Anything).Return(
		func(key string, old []byte) (bool, *model.AppError) {
			if !bytes.Equal(store[key], old) {
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

func TestParseTransferClaim_Compat(t *testing.T) {
	bare := parseTransferClaim([]byte("req-a"))
	require.Equal(t, "req-a", bare.requestID)
	require.Equal(t, transferPhaseHeld, bare.phase)
	require.True(t, bare.leaseExpired(model.GetMillis()), "a value written before leases must not hold the asset")

	versioned := parseTransferClaim([]byte("req-a|3"))
	require.Equal(t, int64(3), versioned.generation)
	require.Equal(t, transferPhaseHeld, versioned.phase)

	full := parseTransferClaim(transferClaim{requestID: "req-a", generation: 2, phase: transferPhaseFulfilling, leaseAt: 1700}.encode())
	require.Equal(t, transferClaim{requestID: "req-a", generation: 2, phase: transferPhaseFulfilling, leaseAt: 1700}, full)
}

func TestClaimTransferAsset_CAS(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := fakeKV(api)
	p := newTestPlugin(api)

	claimKey := transferAssetClaimKey(pendingKindBotTransfer, "bot1")
	claim := func() transferClaim { return parseTransferClaim(store[claimKey]) }

	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-a"))
	require.Equal(t, "req-a", claim().requestID)
	require.Equal(t, transferPhaseHeld, claim().phase)

	// Live holder blocks a second claim.
	store[kvBotTransferRequestPrefix+"req-a"] = []byte(`{"id":"req-a"}`)
	err := p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already")

	// Holder record gone while its final approval is being carried out: the
	// lease is fresh, so the asset stays with it.
	delete(store, kvBotTransferRequestPrefix+"req-a")
	created, _, err := p.beginTransferFulfill(pendingKindBotTransfer, "bot1", "req-a")
	require.NoError(t, err)
	require.False(t, created, "the claim row was already there")
	require.Equal(t, transferPhaseFulfilling, claim().phase)
	err = p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already")

	// A lease older than the fulfill window (crash mid-fulfill) no longer
	// protects the claim.
	expire := func(holder string, phase string) {
		store[claimKey] = transferClaim{
			requestID: holder, phase: phase,
			leaseAt: model.GetMillis() - transferFulfillWindow.Milliseconds() - 1,
		}.encode()
	}
	expire("req-a", transferPhaseFulfilling)
	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b"))
	require.Equal(t, "req-b", claim().requestID)

	// A live restoring holder renews its lease as it republishes, so a fresh
	// restoring lease still protects the asset.
	store[claimKey] = transferClaim{requestID: "req-b", phase: transferPhaseRestoring, leaseAt: model.GetMillis()}.encode()
	err = p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-c")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already")
	require.Equal(t, "req-b", claim().requestID)

	// A restoring holder whose lease aged out died mid-restore: without a
	// takeover the asset would be locked for good.
	expire("req-b", transferPhaseRestoring)
	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-c"))
	require.Equal(t, "req-c", claim().requestID)

	// A bare (pre-lease) claim value reads as its holder with no lease.
	store[claimKey] = []byte("req-gone")
	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-d"))
	require.Equal(t, "req-d", claim().requestID)

	p.releaseTransferAssetClaim(pendingKindBotTransfer, "bot1", "req-d")
	_, held := store[claimKey]
	require.False(t, held)
}

func TestRenewTransferLease(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := fakeKV(api)
	p := newTestPlugin(api)

	claimKey := transferAssetClaimKey(pendingKindBotTransfer, "bot1")
	lease := transferLease{kind: pendingKindBotTransfer, assetKey: "bot1", requestID: "req-a"}
	require.NoError(t, p.renewTransferLease(transferLease{}), "an unset lease is a no-op")

	store[claimKey] = transferClaim{requestID: "req-a", phase: transferPhaseFulfilling, leaseAt: 1}.encode()
	require.NoError(t, p.renewTransferLease(lease))
	require.Greater(t, parseTransferClaim(store[claimKey]).leaseAt, int64(1))
	require.Equal(t, transferPhaseFulfilling, parseTransferClaim(store[claimKey]).phase)

	// Another ticket owns the asset: the caller has to stop.
	store[claimKey] = transferClaim{requestID: "req-b", phase: transferPhaseHeld, leaseAt: model.GetMillis()}.encode()
	require.ErrorIs(t, p.renewTransferLease(lease), errTransferClaimLost)

	delete(store, claimKey)
	require.ErrorIs(t, p.renewTransferLease(lease), errTransferClaimLost)
}

func TestRevokeBotTokenTargets_StopsWhenTheLeaseIsLost(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := fakeKV(api)
	p := newTestPlugin(api)

	// Another ticket holds the claim, so the first renewal fails.
	store[transferAssetClaimKey(pendingKindBotTransfer, "bot1")] = transferClaim{requestID: "req-b", phase: transferPhaseHeld, leaseAt: model.GetMillis()}.encode()

	var revoked []string
	api.On("RevokeUserAccessToken", mock.Anything).Return(func(id string) *model.AppError {
		revoked = append(revoked, id)
		return nil
	}).Maybe()

	targets := botTokenTargets{tracked: map[string]string{}}
	for i := range botTokenRevokeRenewEvery + 5 {
		targets.listed = append(targets.listed, fmt.Sprintf("tok-%02d", i))
	}
	lease := transferLease{kind: pendingKindBotTransfer, assetKey: "bot1", requestID: "req-a"}

	n, err := p.revokeBotTokenTargets("bot1", lease, targets)
	require.Error(t, err)
	require.Contains(t, err.Error(), "took over")
	require.Zero(t, n, "the claim is checked before the first revoke, not after a batch of them")
	require.Empty(t, revoked, "an approval that already lost the asset must not revoke a single token")
}

func TestRevokeBotTokenTargets_RenewsPartWayThroughALongWalk(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := fakeKV(api)
	p := newTestPlugin(api)

	claimKey := transferAssetClaimKey(pendingKindBotTransfer, "bot1")
	store[claimKey] = transferClaim{requestID: "req-a", phase: transferPhaseFulfilling, leaseAt: 1}.encode()
	api.On("RevokeUserAccessToken", mock.Anything).Return(nil).Maybe()

	targets := botTokenTargets{tracked: map[string]string{}}
	for i := range botTokenRevokeRenewEvery + 5 {
		targets.listed = append(targets.listed, fmt.Sprintf("tok-%02d", i))
	}
	lease := transferLease{kind: pendingKindBotTransfer, assetKey: "bot1", requestID: "req-a"}

	n, err := p.revokeBotTokenTargets("bot1", lease, targets)
	require.NoError(t, err)
	require.Equal(t, botTokenRevokeRenewEvery+5, n)
	require.Greater(t, parseTransferClaim(store[claimKey]).leaseAt, int64(1), "the lease is renewed as the walk runs")
}

func TestWebhookIndexRoundTrip(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	memKV(api)
	p := newTestPlugin(api)

	req := &webhookRequest{
		ID: "r1", RequesterID: "u_req", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", TeamID: "t1",
		DisplayName: "Relay", CallbackURL: "https://hooks.example.com/out",
	}
	require.NoError(t, p.writeWebhookIndex(req, "hook1", "admin1"))

	got, err := p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "u_req", got.OwnerUserID)
	require.Equal(t, "u_req", got.RequesterID)
	require.Equal(t, webhookTypeOutgoing, got.WebhookType)
	require.Equal(t, "https://hooks.example.com/out", got.CallbackURL)
	require.Zero(t, got.RetiredAt)

	require.NoError(t, p.updateWebhookIndexOwner("hook1", "u_new"))
	got, err = p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.Equal(t, "u_new", got.OwnerUserID)

	all, err := p.listWebhookIndex()
	require.NoError(t, err)
	require.Len(t, all, 1)

	require.NoError(t, p.retireWebhookIndex("hook1"))
	got, err = p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.NotZero(t, got.RetiredAt)
}

func TestWriteWebhookIndexSkipsExerciseAndEmptyHook(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	store := memKV(api)
	p := newTestPlugin(api)

	require.NoError(t, p.writeWebhookIndex(&webhookRequest{ID: "r1", Exercise: true, WebhookType: webhookTypeIncoming}, "hook1", "a"))
	require.NoError(t, p.writeWebhookIndex(&webhookRequest{ID: "r2", WebhookType: webhookTypeIncoming}, "", "a"))
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
		api.On("GetBot", "b1", true).Return(&model.Bot{UserId: "b1", OwnerId: "u_req"}, nil)
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

func TestWebhookIndexOwnerFallsBackToRequester(t *testing.T) {
	require.Equal(t, "owner", webhookIndexOwner(webhookIndexEntry{OwnerUserID: "owner", RequesterID: "req"}))
	require.Equal(t, "req", webhookIndexOwner(webhookIndexEntry{RequesterID: "req"}))
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

func TestCreateOutgoingWebhookFromRecreate_AlwaysStages(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubSiteURL(api, "http://localhost:8065")
	api.On("GetUser", "admin1").Return(nil, testAppErr("no admin")).Maybe()
	p := newTestPlugin(api)

	req := &webhookRequest{
		ID: "r1", RequesterID: "u_new", WebhookType: webhookTypeOutgoing,
		ChannelID: "ch1", TeamID: "t1", DisplayName: "Relay",
	}

	// Even a plain JSON single-callback hook has to be staged on a temporary
	// trigger, or it would fire alongside the hook it replaces.
	for _, contentType := range []string{"application/json", "application/x-www-form-urlencoded"} {
		simpleCreate := false
		p.createWebhookFn = func(*webhookRequest, string) (string, string, error) {
			simpleCreate = true
			return "ok", "new-hook", nil
		}
		_, _, _, err := p.createOutgoingWebhookFromRecreate(req, &outgoingWebhookRecreate{
			CallbackURLs: []string{"https://hooks.example.com/out"},
			ContentType:  contentType,
		}, "admin1")
		require.False(t, simpleCreate, contentType)
		require.Error(t, err) // transfer REST client needs a usable admin session
	}
}

// cutoverFixture is the retire & recreate fixture with a store that honours
// compare-and-set, so checkpoint writes are visible to the test.
// Stubs in preKV are registered ahead of the store, so they take precedence.
func cutoverFixture(t *testing.T, preKV ...func(*plugintest.API)) (*plugintest.API, *Plugin, map[string][]byte) {
	t.Helper()
	api := &plugintest.API{}
	for _, stub := range preKV {
		stub(api)
	}
	store := memKV(api) // before stubLogs, whose blanket CAS stub discards writes
	stubLogs(api)
	stubSiteURL(api, "http://localhost:8065")
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.auditSink = func(auditEntry, auditEventSpec) {}
	t.Cleanup(func() {
		p.getOutgoingWebhookForRecreateFn = nil
		p.createOutgoingWebhookFromRecreateFn = nil
		p.restoreOutgoingWebhookTriggersFn = nil
		p.deleteWebhookFn = nil
	})

	api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_new", Username: "newowner"}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
	p.restoreOutgoingWebhookTriggersFn = func(string, string, *outgoingWebhookRecreate) error { return nil }
	p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
		return &outgoingWebhookRecreate{
			ChannelID:    "ch1",
			CallbackURLs: []string{"https://hooks.example.com/out"},
			TriggerWords: []string{"alert"},
			ContentType:  "application/json",
		}, nil
	}
	return api, p, store
}

// stageCutover stores a retire & recreate ticket and its asset claim.
func stageCutover(t *testing.T, store map[string][]byte, id string) (*webhookTransferRequest, string, []byte) {
	t.Helper()
	req := &webhookTransferRequest{
		ID: id, RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
		DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
		Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
	}
	key := kvWebhookTransferRequestPrefix + id
	raw := recordJSON(t, req)
	store[key] = raw
	store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = transferClaim{requestID: id, leaseAt: model.GetMillis()}.encode()
	return req, key, raw
}

func TestFulfillWebhookRetireRecreate_Checkpoints(t *testing.T) {
	fulfill := func(p *Plugin, req *webhookTransferRequest, raw []byte, key string) decisionResult {
		return p.fulfillWebhookRetireRecreate(req,
			&model.User{Id: "admin1", Username: "admin"},
			&model.User{Id: "u_req", Username: "req"},
			newDecisionCards(req.ID, "", ""), raw, key)
	}
	okCreate := func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
		return "Outgoing webhook created.", "new-hook", "tok-new", nil
	}

	t.Run("records the create intent before asking Mattermost for a hook", func(t *testing.T) {
		_, p, store := cutoverFixture(t)
		req, key, raw := stageCutover(t, store, "t-intent")

		var atCreate webhookTransferRequest
		p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
			// A lost response here leaves a hook only this stage records.
			require.NoError(t, json.Unmarshal(store[key], &atCreate))
			return okCreate(nil, nil, "")
		}
		p.deleteWebhookFn = func(string, string) error { return nil }

		res := fulfill(p, req, raw, key)
		require.Equal(t, cutoverStageCreating, atCreate.CutoverStage)
		require.Empty(t, atCreate.ResumeNewHookID)
		require.True(t, atCreate.cutoverInProgress())
		require.Contains(t, res.Outcome, "recreated")
	})

	t.Run("a create the server turned down clears the intent so the ticket is plain again", func(t *testing.T) {
		_, p, store := cutoverFixture(t)
		req, key, raw := stageCutover(t, store, "t-intent-fail")
		p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
			return "", "", "", model.NewAppError("create", "refused", nil, "", http.StatusBadRequest)
		}

		fulfill(p, req, raw, key)

		var stored webhookTransferRequest
		require.NoError(t, json.Unmarshal(store[key], &stored))
		require.False(t, stored.cutoverInProgress())
	})

	t.Run("a create whose outcome is unknown keeps the creating checkpoint", func(t *testing.T) {
		_, p, store := cutoverFixture(t)
		req, key, raw := stageCutover(t, store, "t-intent-unknown")
		p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
			return "", "", "", errors.New("connection reset by peer")
		}

		fulfill(p, req, raw, key)

		var stored webhookTransferRequest
		require.NoError(t, json.Unmarshal(store[key], &stored))
		// A lost response may still have made a hook that only this stage
		// records, so the next attempt has to be told to look for it.
		require.Equal(t, cutoverStageCreating, stored.CutoverStage)
		require.True(t, stored.cutoverInProgress())
	})

	t.Run("a replacement that cannot be rolled back keeps its checkpoint", func(t *testing.T) {
		_, p, store := cutoverFixture(t)
		req, key, raw := stageCutover(t, store, "t-rollback-fail")
		p.createOutgoingWebhookFromRecreateFn = okCreate
		var deleted []string
		p.deleteWebhookFn = func(_, hookID string) error {
			deleted = append(deleted, hookID)
			return errors.New("delete refused")
		}

		res := fulfill(p, req, raw, key)
		require.Equal(t, []string{"old-hook", "new-hook"}, deleted)
		require.NotContains(t, res.Outcome, "recreated")

		var stored webhookTransferRequest
		require.NoError(t, json.Unmarshal(store[key], &stored))
		// Both hooks are live; dropping the pointer would leave the parked
		// replacement with nothing to clean it up.
		require.Equal(t, "new-hook", stored.ResumeNewHookID)
	})

	t.Run("a checkpoint that will not clear keeps the ticket open", func(t *testing.T) {
		// The ticket moved under this approval, so clearing the checkpoint misses.
		_, p, store := cutoverFixture(t, func(api *plugintest.API) {
			api.On("KVCompareAndDelete", kvWebhookTransferRequestPrefix+"t-cp-stuck", mock.Anything).Return(false, nil)
		})
		req, key, raw := stageCutover(t, store, "t-cp-stuck")
		p.createOutgoingWebhookFromRecreateFn = okCreate
		p.deleteWebhookFn = func(string, string) error { return nil }

		res := fulfill(p, req, raw, key)
		require.NotContains(t, res.Outcome, "recreated", "a ticket still on disk is not a closed ticket")
		require.NotEmpty(t, store[key])
	})
}

func TestWebhookCutover_DenyClaimsBeforeRemovingTheReplacement(t *testing.T) {
	_, p, store := cutoverFixture(t)
	req, key, _ := stageCutover(t, store, model.NewId())
	req.stageCutover(cutoverStageReplacementReady, webhookCutover{newHookID: "new-hook", channelID: "ch1"})
	store[key] = recordJSON(t, req)
	// Another ticket owns the hook, so this deny may not touch either hook.
	store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = transferClaim{requestID: model.NewId(), leaseAt: model.GetMillis()}.encode()

	deleted := false
	p.deleteWebhookFn = func(string, string) error {
		deleted = true
		return nil
	}

	res, ok := p.denyStagedCutover(req, &model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"}, newDecisionCards(req.ID, "", ""), store[key], key)

	require.False(t, ok)
	require.False(t, deleted, "the replacement must survive a deny that never held the claim")
	require.Contains(t, res.Reply, "Another transfer ticket now owns this asset")
	require.NotEmpty(t, store[key], "the ticket stays so the cleanup can be retried")
}

func TestResumeWebhookRetireRecreate_RechecksTheSuccessorBeforeDeleting(t *testing.T) {
	_, p, store := cutoverFixture(t)
	req, key, _ := stageCutover(t, store, model.NewId())
	req.NewOwnerUserID = model.NewId()
	req.stageCutover(cutoverStageReplacementReady, webhookCutover{newHookID: "new-hook", channelID: "ch1"})
	raw := recordJSON(t, req)
	store[key] = raw

	// The successor was deactivated while the cutover sat half-finished.
	p.API.(*plugintest.API).On("GetUser", req.NewOwnerUserID).
		Return(&model.User{Id: req.NewOwnerUserID, Username: "newowner", DeleteAt: model.GetMillis()}, nil)
	deleted := false
	p.deleteWebhookFn = func(string, string) error {
		deleted = true
		return nil
	}

	res := p.fulfillWebhookRetireRecreate(req, &model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"}, newDecisionCards(req.ID, "", ""), raw, key)

	require.False(t, deleted, "an ineligible successor must not cost the requester a live webhook")
	require.NotContains(t, res.Outcome, "recreated")

	var stored webhookTransferRequest
	require.NoError(t, json.Unmarshal(store[key], &stored))
	require.Equal(t, "new-hook", stored.ResumeNewHookID, "the parked replacement is still tracked")
}

func TestFulfillWebhookRetireRecreate_Outgoing(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	store := memKV(api)
	stubSiteURL(api, "http://localhost:8065")
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.auditSink = func(auditEntry, auditEventSpec) {}

	api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_new", Username: "newowner"}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()

	// resetRecreateSeams clears the shared seams when a subtest ends, even
	// when one of its assertions fails first.
	resetRecreateSeams := func(t *testing.T) {
		t.Cleanup(func() {
			p.getOutgoingWebhookForRecreateFn = nil
			p.getOutgoingWebhookCallbackFn = nil
			p.createOutgoingWebhookFromRecreateFn = nil
			p.restoreOutgoingWebhookTriggersFn = nil
			p.outgoingWebhookTokenFn = nil
			p.createWebhookFn = nil
			p.deleteWebhookFn = nil
		})
	}
	okTriggers := func(string, string, *outgoingWebhookRecreate) error { return nil }
	okCreate := func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
		return "Outgoing webhook created.", "new-hook", "tok-new", nil
	}
	triggerSnap := func() *outgoingWebhookRecreate {
		return &outgoingWebhookRecreate{
			ChannelID:    "ch1",
			CallbackURLs: []string{"https://hooks.example.com/out"},
			TriggerWords: []string{"alert", "page"},
			TriggerWhen:  1,
			ContentType:  "application/json",
		}
	}
	newReq := func(id string) *webhookTransferRequest {
		req := &webhookTransferRequest{
			ID: id, RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
			DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
			Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
		}
		store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = []byte(req.ID)
		return req
	}
	fulfill := func(req *webhookTransferRequest, raw []byte) decisionResult {
		return p.fulfillWebhookRetireRecreate(req,
			&model.User{Id: "admin1", Username: "admin"},
			&model.User{Id: "u_req", Username: "req"},
			newDecisionCards(req.ID, "", ""), raw, kvWebhookTransferRequestPrefix+req.ID)
	}

	t.Run("refuses when live fetch fails even if snapshot has a callback", func(t *testing.T) {
		// Snapshot alone cannot prove triggers / extra callbacks were absent.
		resetRecreateSeams(t)
		deleted, created := false, false
		p.deleteWebhookFn = func(string, string) error {
			deleted = true
			return nil
		}
		p.getOutgoingWebhookCallbackFn = func(string, string) (string, error) {
			return "", errors.New("live unavailable")
		}
		p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
			created = true
			return "", "", "", nil
		}
		req := newReq("t1")
		req.CallbackURL = "https://hooks.example.com/from-index"
		res := fulfill(req, []byte(`{"id":"t1"}`))
		require.False(t, deleted, "must not delete when live fields cannot be confirmed")
		require.False(t, created)
		require.True(t, res.audited)
	})

	t.Run("prefers live callback over stale snapshot", func(t *testing.T) {
		resetRecreateSeams(t)
		var gotCallback string
		createBeforeDelete, deleted := false, false
		p.getOutgoingWebhookCallbackFn = func(hookID, _ string) (string, error) {
			require.Equal(t, "old-hook", hookID)
			return "https://hooks.example.com/live", nil
		}
		p.createOutgoingWebhookFromRecreateFn = func(_ *webhookRequest, snap *outgoingWebhookRecreate, _ string) (string, string, string, error) {
			require.False(t, deleted, "must create replacement before deleting the old hook")
			createBeforeDelete = true
			gotCallback = snap.CallbackURLs[0]
			return "Outgoing webhook created.", "new-hook", "tok-new", nil
		}
		p.deleteWebhookFn = func(_, hookID string) error {
			require.True(t, createBeforeDelete)
			require.Equal(t, "old-hook", hookID)
			deleted = true
			return nil
		}
		p.restoreOutgoingWebhookTriggersFn = okTriggers
		req := newReq("t2")
		req.CallbackURL = "https://hooks.example.com/stale"
		res := fulfill(req, nil)
		require.Equal(t, "https://hooks.example.com/live", gotCallback)
		require.True(t, createBeforeDelete && deleted)
		require.Contains(t, res.Outcome, "recreated")
	})

	t.Run("create failure leaves old hook intact", func(t *testing.T) {
		resetRecreateSeams(t)
		deleted := false
		p.deleteWebhookFn = func(string, string) error {
			deleted = true
			return nil
		}
		p.getOutgoingWebhookCallbackFn = func(string, string) (string, error) {
			return "https://hooks.example.com/live", nil
		}
		p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
			return "", "", "", errors.New("create failed")
		}
		res := fulfill(newReq("t-fail"), []byte(`{"id":"t-fail"}`))
		require.False(t, deleted)
		require.True(t, res.audited)
	})

	t.Run("refuses when the hook is no longer scoped to the approved channel", func(t *testing.T) {
		resetRecreateSeams(t)
		created := false
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
			snap := triggerSnap()
			snap.ChannelID = "" // fires team-wide now
			return snap, nil
		}
		p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
			created = true
			return "", "", "", nil
		}
		p.deleteWebhookFn = func(string, string) error { return nil }
		res := fulfill(newReq("t-teamwide"), []byte(`{"id":"t-teamwide"}`))
		require.False(t, created, "recreating a team-wide hook in one channel changes its scope")
		require.True(t, res.audited)
	})

	t.Run("refuses when the hook moved to another channel", func(t *testing.T) {
		resetRecreateSeams(t)
		created := false
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
			snap := triggerSnap()
			snap.ChannelID = "ch-moved"
			return snap, nil
		}
		p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
			created = true
			return "", "", "", nil
		}
		p.deleteWebhookFn = func(string, string) error { return nil }
		res := fulfill(newReq("t-moved"), []byte(`{"id":"t-moved"}`))
		require.False(t, created, "must not recreate the hook in a channel the reviewers never saw")
		require.True(t, res.audited)
	})

	t.Run("preserves form-urlencoded content type via recreate seam", func(t *testing.T) {
		resetRecreateSeams(t)
		var gotSnap *outgoingWebhookRecreate
		p.getOutgoingWebhookForRecreateFn = func(hookID, _ string) (*outgoingWebhookRecreate, error) {
			require.Equal(t, "old-hook", hookID)
			return &outgoingWebhookRecreate{
				ChannelID:    "ch1",
				CallbackURLs: []string{"https://hooks.example.com/form"},
				ContentType:  "application/x-www-form-urlencoded",
			}, nil
		}
		p.createOutgoingWebhookFromRecreateFn = func(_ *webhookRequest, snap *outgoingWebhookRecreate, _ string) (string, string, string, error) {
			gotSnap = snap
			return "Outgoing webhook created.", "new-hook", "tok-new", nil
		}
		p.restoreOutgoingWebhookTriggersFn = okTriggers
		p.deleteWebhookFn = func(string, string) error { return nil }
		res := fulfill(newReq("t-form"), nil)
		require.NotNil(t, gotSnap)
		require.Equal(t, "application/x-www-form-urlencoded", gotSnap.ContentType)
		require.Contains(t, res.Outcome, "recreated")
	})

	t.Run("restores trigger words after the old hook is deleted", func(t *testing.T) {
		resetRecreateSeams(t)
		var gotSnap, restoredSnap *outgoingWebhookRecreate
		var restoredHook string
		deleted := false
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) { return triggerSnap(), nil }
		p.createOutgoingWebhookFromRecreateFn = func(_ *webhookRequest, snap *outgoingWebhookRecreate, _ string) (string, string, string, error) {
			gotSnap = snap
			return "Outgoing webhook created.", "new-hook", "tok-new", nil
		}
		p.deleteWebhookFn = func(string, string) error {
			deleted = true
			return nil
		}
		p.restoreOutgoingWebhookTriggersFn = func(hookID, _ string, snap *outgoingWebhookRecreate) error {
			require.True(t, deleted, "triggers go back only once the old hook is gone")
			restoredHook, restoredSnap = hookID, snap
			return nil
		}
		res := fulfill(newReq("t-trig"), nil)
		require.NotNil(t, gotSnap)
		require.Equal(t, []string{"alert", "page"}, gotSnap.TriggerWords)
		require.Equal(t, "new-hook", restoredHook)
		require.NotNil(t, restoredSnap)
		require.Equal(t, []string{"alert", "page"}, restoredSnap.TriggerWords)
		require.Equal(t, 1, restoredSnap.TriggerWhen)
		require.Contains(t, res.Outcome, "recreated")
	})

	t.Run("a failed trigger restore keeps the ticket and records resume state", func(t *testing.T) {
		resetRecreateSeams(t)
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) { return triggerSnap(), nil }
		p.createOutgoingWebhookFromRecreateFn = okCreate
		p.deleteWebhookFn = func(string, string) error { return nil }
		p.restoreOutgoingWebhookTriggersFn = func(string, string, *outgoingWebhookRecreate) error {
			return errors.New("update refused")
		}
		req := newReq("t-trig-fail")
		key := kvWebhookTransferRequestPrefix + req.ID
		res := fulfill(req, []byte(`{"id":"t-trig-fail"}`))
		require.NotContains(t, res.Outcome, "with issues", "the handover is not finished, so the ticket stays open")
		require.True(t, res.audited)

		var stored webhookTransferRequest
		require.NoError(t, json.Unmarshal(store[key], &stored))
		require.Equal(t, "new-hook", stored.ResumeNewHookID)
		require.True(t, stored.ResumeOldDeleted)
		require.Equal(t, []string{"alert", "page"}, stored.ResumeTriggerWords)
		require.Equal(t, 1, stored.ResumeTriggerWhen)
	})

	t.Run("old hook still present rolls the replacement back", func(t *testing.T) {
		resetRecreateSeams(t)
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) { return triggerSnap(), nil }
		p.createOutgoingWebhookFromRecreateFn = okCreate
		restored := false
		p.restoreOutgoingWebhookTriggersFn = func(string, string, *outgoingWebhookRecreate) error {
			restored = true
			return nil
		}
		var deleted []string
		p.deleteWebhookFn = func(_, hookID string) error {
			deleted = append(deleted, hookID)
			if hookID == "old-hook" {
				return errors.New("delete refused")
			}
			return nil
		}
		res := fulfill(newReq("t-olddel"), []byte(`{"id":"t-olddel"}`))
		require.Equal(t, []string{"old-hook", "new-hook"}, deleted)
		require.False(t, restored, "triggers must not be restored on a hook that was taken down")
		require.NotContains(t, res.Outcome, "recreated")
	})

	t.Run("old hook already gone carries the cutover through", func(t *testing.T) {
		resetRecreateSeams(t)
		lookups := 0
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
			lookups++
			if lookups == 1 {
				return triggerSnap(), nil
			}
			return nil, errors.New("not found")
		}
		p.createOutgoingWebhookFromRecreateFn = okCreate
		p.restoreOutgoingWebhookTriggersFn = okTriggers
		var deleted []string
		p.deleteWebhookFn = func(_, hookID string) error {
			deleted = append(deleted, hookID)
			if hookID == "old-hook" {
				return errors.New("delete refused")
			}
			return nil
		}
		res := fulfill(newReq("t-oldgone"), []byte(`{"id":"t-oldgone"}`))
		require.Equal(t, []string{"old-hook"}, deleted, "the replacement must survive a delete whose response was lost")
		require.Contains(t, res.Outcome, "recreated")
	})

	t.Run("ambiguous old hook lookup parks the ticket and keeps the replacement", func(t *testing.T) {
		resetRecreateSeams(t)
		lookups := 0
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
			lookups++
			if lookups == 1 {
				return triggerSnap(), nil
			}
			return nil, errors.New("server unavailable")
		}
		p.createOutgoingWebhookFromRecreateFn = okCreate
		p.restoreOutgoingWebhookTriggersFn = okTriggers
		var deleted []string
		p.deleteWebhookFn = func(_, hookID string) error {
			deleted = append(deleted, hookID)
			if hookID == "old-hook" {
				return errors.New("delete refused")
			}
			return nil
		}
		req := newReq("t-ambiguous")
		key := kvWebhookTransferRequestPrefix + req.ID
		res := fulfill(req, []byte(`{"id":"t-ambiguous"}`))
		require.Equal(t, []string{"old-hook"}, deleted, "an unknown old-hook state must not delete the replacement")
		require.NotContains(t, res.Outcome, "recreated")

		var stored webhookTransferRequest
		require.NoError(t, json.Unmarshal(store[key], &stored))
		require.Equal(t, "new-hook", stored.ResumeNewHookID)
		require.False(t, stored.ResumeOldDeleted)
	})

	t.Run("checkpoints the replacement before deleting the old hook", func(t *testing.T) {
		resetRecreateSeams(t)
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) { return triggerSnap(), nil }
		p.createOutgoingWebhookFromRecreateFn = okCreate
		p.restoreOutgoingWebhookTriggersFn = okTriggers
		req := newReq("t-checkpoint")
		key := kvWebhookTransferRequestPrefix + req.ID

		var atDelete webhookTransferRequest
		p.deleteWebhookFn = func(_, hookID string) error {
			require.Equal(t, "old-hook", hookID)
			// Final approval deleted the record, so only a checkpoint
			// written before this call can survive a crash here.
			require.NoError(t, json.Unmarshal(store[key], &atDelete))
			return nil
		}
		res := fulfill(req, []byte(`{"id":"t-checkpoint"}`))
		require.Equal(t, "new-hook", atDelete.ResumeNewHookID)
		require.Equal(t, cutoverStageReplacementReady, atDelete.CutoverStage)
		require.Equal(t, "ch1", atDelete.ResumeChannelID)
		require.Equal(t, []string{"alert", "page"}, atDelete.ResumeTriggerWords)
		require.Contains(t, res.Outcome, "recreated")

		_, stillOpen := store[key]
		require.False(t, stillOpen, "a finished cutover clears its checkpoint")
	})

	t.Run("blocks delete when callback cannot be resolved", func(t *testing.T) {
		resetRecreateSeams(t)
		deleted := false
		p.deleteWebhookFn = func(string, string) error {
			deleted = true
			return nil
		}
		p.getOutgoingWebhookCallbackFn = func(string, string) (string, error) {
			return "", errors.New("hook gone")
		}
		req := newReq("t3")
		req.ChannelID = ""
		res := fulfill(req, []byte(`{"id":"t3"}`))
		require.False(t, deleted, "must not delete before callback is known")
		require.True(t, res.audited)
		require.True(t, strings.Contains(res.Reply, "Couldn't") || strings.Contains(res.Reply, "hook gone") || res.Banner != nil)
	})
}

func TestResumeWebhookRetireRecreate_FinishesCutover(t *testing.T) {
	newOwnerID := model.NewId()

	setup := func(t *testing.T) (*Plugin, *plugintest.API, map[string][]byte) {
		api := &plugintest.API{}
		stubLogs(api)
		store := memKV(api)
		stubSiteURL(api, "http://localhost:8065")
		p := newTestPlugin(api)
		p.botUserID = "bot"
		p.auditSink = func(auditEntry, auditEventSpec) {}

		api.On("GetUser", newOwnerID).Return(&model.User{Id: newOwnerID, Username: "newowner", Roles: "system_user"}, nil).Maybe()
		api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
		api.On("GetChannelMember", "ch1", newOwnerID).Return(&model.ChannelMember{}, nil).Maybe()
		api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Type: model.ChannelTypeOpen}, nil).Maybe()
		api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
		api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
		api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
		api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
		return p, api, store
	}
	newReq := func(store map[string][]byte) *webhookTransferRequest {
		req := &webhookTransferRequest{
			ID: "t-resume", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
			DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
			Action: transferActionRetireRecreate, NewOwnerUserID: newOwnerID,
			CallbackURL:        "https://hooks.example.com/out",
			CutoverStage:       cutoverStageOldDeleted,
			ResumeNewHookID:    "new-hook",
			ResumeOldDeleted:   true,
			ResumeTriggerWords: []string{"alert", "page"},
			ResumeTriggerWhen:  1,
			ResumeChannelID:    "ch1",
		}
		store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = transferClaim{requestID: req.ID}.encode()
		return req
	}
	resume := func(p *Plugin, req *webhookTransferRequest) decisionResult {
		return p.fulfillWebhookRetireRecreate(req,
			&model.User{Id: "admin1", Username: "admin"},
			&model.User{Id: "u_req", Username: "req"},
			newDecisionCards(req.ID, "", ""), nil, kvWebhookTransferRequestPrefix+req.ID)
	}

	t.Run("finishes without making a second hook", func(t *testing.T) {
		p, _, store := setup(t)
		created := false
		p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
			created = true
			return "", "", "", nil
		}
		var deleted []string
		p.deleteWebhookFn = func(_, hookID string) error {
			deleted = append(deleted, hookID)
			return nil
		}
		var restoredSnap *outgoingWebhookRecreate
		p.restoreOutgoingWebhookTriggersFn = func(hookID, _ string, snap *outgoingWebhookRecreate) error {
			require.Equal(t, "new-hook", hookID)
			restoredSnap = snap
			return nil
		}
		p.outgoingWebhookTokenFn = func(hookID, _ string) (string, error) {
			require.Equal(t, "new-hook", hookID)
			return "tok-resumed", nil
		}

		req := newReq(store)
		key := kvWebhookTransferRequestPrefix + req.ID
		res := resume(p, req)

		require.False(t, created, "a resumed cutover must not make a second hook")
		require.Empty(t, deleted, "the old hook was already deleted")
		require.NotNil(t, restoredSnap)
		require.Equal(t, []string{"alert", "page"}, restoredSnap.TriggerWords)
		require.Contains(t, res.Outcome, "recreated")

		_, stillOpen := store[key]
		require.False(t, stillOpen, "the finished cutover clears its checkpoint")
		indexed, err := p.loadWebhookIndex("new-hook")
		require.NoError(t, err)
		require.NotNil(t, indexed, "the replacement is listed before the ticket closes")
		require.Equal(t, newOwnerID, indexed.OwnerUserID)
	})

	t.Run("pauses when the successor is no longer eligible", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		store := memKV(api)
		stubSiteURL(api, "http://localhost:8065")
		p := newTestPlugin(api)
		p.botUserID = "bot"
		p.auditSink = func(auditEntry, auditEventSpec) {}
		// The successor was deactivated while the cutover was parked.
		api.On("GetUser", newOwnerID).Return(&model.User{Id: newOwnerID, Username: "newowner", DeleteAt: 1}, nil).Maybe()
		api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
		api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
		api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
		api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
		api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()

		restored, dmd := false, false
		p.restoreOutgoingWebhookTriggersFn = func(string, string, *outgoingWebhookRecreate) error {
			restored = true
			return nil
		}
		p.outgoingWebhookTokenFn = func(string, string) (string, error) {
			dmd = true
			return "tok-resumed", nil
		}
		req := newReq(store)
		key := kvWebhookTransferRequestPrefix + req.ID
		res := resume(p, req)

		require.False(t, restored, "an ineligible successor must not get a live hook")
		require.False(t, dmd, "an ineligible successor must not be sent the token")
		require.NotContains(t, res.Outcome, "recreated")

		var stored webhookTransferRequest
		require.NoError(t, json.Unmarshal(store[key], &stored))
		require.Equal(t, "new-hook", stored.ResumeNewHookID, "the ticket keeps its cutover state for another retry")
	})
}

func TestWebhookTransferCutover_BlocksClosingTheTicket(t *testing.T) {
	staged := func(oldDeleted bool) *webhookTransferRequest {
		req := &webhookTransferRequest{
			ID: "t-staged", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
			DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
			Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
			CreatedAt: model.GetMillis(), ResumeNewHookID: "new-hook",
		}
		if oldDeleted {
			req.CutoverStage = cutoverStageOldDeleted
			req.ResumeOldDeleted = true
		} else {
			req.CutoverStage = cutoverStageReplacementReady
		}
		return req
	}
	setup := func(t *testing.T, req *webhookTransferRequest) (*Plugin, *plugintest.API, map[string][]byte) {
		api := &plugintest.API{}
		stubLogs(api)
		store := memKV(api)
		p := newTestPlugin(api)
		p.auditSink = func(auditEntry, auditEventSpec) {}
		api.On("GetUser", testSysAdminID).Return(systemAdminUser(), nil).Maybe()
		api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
		api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
		api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
		api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
		api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
		store[kvWebhookTransferRequestPrefix+req.ID] = recordJSON(t, req)
		return p, api, store
	}

	t.Run("deny refused once the old hook is gone", func(t *testing.T) {
		req := staged(true)
		p, _, store := setup(t, req)
		deleted := false
		p.deleteWebhookFn = func(string, string) error {
			deleted = true
			return nil
		}
		res := p.decideWebhookTransferRequest(testSysAdminID, req.ID, "", false)
		require.Contains(t, res.Reply, "can't be denied")
		require.False(t, deleted)
		require.NotEmpty(t, store[kvWebhookTransferRequestPrefix+req.ID], "the ticket stays open so Approve can finish")
	})

	t.Run("deny removes the replacement while the old hook is live", func(t *testing.T) {
		req := staged(false)
		p, _, store := setup(t, req)
		p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
			return &outgoingWebhookRecreate{CallbackURLs: []string{"https://hooks.example.com/out"}}, nil
		}
		var deleted []string
		p.deleteWebhookFn = func(_, hookID string) error {
			deleted = append(deleted, hookID)
			return nil
		}
		res := p.decideWebhookTransferRequest(testSysAdminID, req.ID, "", false)
		require.Equal(t, []string{"new-hook"}, deleted, "only the staged replacement is removed")
		require.Contains(t, res.Outcome, "Denied")
		_, stillOpen := store[kvWebhookTransferRequestPrefix+req.ID]
		require.False(t, stillOpen)
	})

	t.Run("deny refused while a staged cutover is past the review window", func(t *testing.T) {
		req := staged(true)
		req.CreatedAt = model.GetMillis() - 365*24*60*60*1000
		p, _, store := setup(t, req)
		config := *p.getConfiguration()
		config.expiryDays = 30
		p.setConfiguration(&config)
		res := p.decideWebhookTransferRequest(testSysAdminID, req.ID, "", false)
		require.Equal(t, cutoverExpiryBlockedMsg, res.Reply)
		require.NotEmpty(t, store[kvWebhookTransferRequestPrefix+req.ID])
	})
}

func TestWebhookTransferCutover_ApproveFinishesAnExpiredHandover(t *testing.T) {
	newOwnerID := model.NewId()
	api := &plugintest.API{}
	stubLogs(api)
	store := memKV(api)
	p := newTestPlugin(api)
	config := *p.getConfiguration()
	// One System Admin can stamp both steps, so a single Approve reaches
	// the fulfill path the expiry check used to block.
	config.TwoStepApproverMode = twoStepModeSystemAdmin
	config.expiryDays = 30
	p.setConfiguration(&config)
	p.auditSink = func(auditEntry, auditEventSpec) {}

	api.On("GetUser", testSysAdminID).Return(systemAdminUser(), nil).Maybe()
	api.On("GetUser", newOwnerID).Return(&model.User{Id: newOwnerID, Username: "newowner", Roles: "system_user"}, nil).Maybe()
	api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetChannelMember", "ch1", newOwnerID).Return(&model.ChannelMember{}, nil).Maybe()
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Type: model.ChannelTypeOpen}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
	p.restoreOutgoingWebhookTriggersFn = func(string, string, *outgoingWebhookRecreate) error { return nil }
	p.outgoingWebhookTokenFn = func(string, string) (string, error) { return "tok-resumed", nil }

	req := &webhookTransferRequest{
		ID: "t-expired", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
		DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
		Action: transferActionRetireRecreate, NewOwnerUserID: newOwnerID,
		CallbackURL: "https://hooks.example.com/out", CreatedAt: model.GetMillis() - 365*24*60*60*1000,
		CutoverStage: cutoverStageOldDeleted, ResumeNewHookID: "new-hook", ResumeOldDeleted: true,
		ResumeChannelID: "ch1",
	}
	store[kvWebhookTransferRequestPrefix+req.ID] = recordJSON(t, req)
	store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = transferClaim{requestID: req.ID, leaseAt: model.GetMillis()}.encode()

	res := p.decideWebhookTransferRequest(testSysAdminID, req.ID, "", true)

	// Expiring deletes the only pointer to the replacement hook, so Approve
	// has to stay the way out of a staged cutover however old the ticket is.
	require.NotEqual(t, cutoverExpiryBlockedMsg, res.Reply)
	require.Contains(t, res.Outcome, "recreated")
	_, stillOpen := store[kvWebhookTransferRequestPrefix+req.ID]
	require.False(t, stillOpen, "the finished handover closes the ticket")
}

func TestWebhookTransferApprovalBlocker_RetireOnlyAcceptsAMissingHook(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	memKV(api)
	p := newTestPlugin(api)

	src := &webhookRequest{ID: "orig", RequesterID: "u_req", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", TeamID: "t1", DisplayName: "Relay"}
	require.NoError(t, p.writeWebhookIndex(src, "hook1", "admin1"))
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req", Roles: "system_user"}, nil).Maybe()
	p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
		return nil, errors.New("404 not found")
	}
	acting := &model.User{Id: "sec1", Username: "sec", Roles: "system_user"}

	retire := &webhookTransferRequest{
		ID: "t-retire", RequesterID: "u_req", HookID: "hook1", WebhookType: webhookTypeOutgoing,
		ChannelID: "ch1", Action: transferActionRetireOnly,
	}
	require.Empty(t, p.webhookTransferApprovalBlockerFor(retire, acting, true),
		"a hook that is already gone is what Retire wants; fulfill retires the index row")

	recreate := *retire
	recreate.Action = transferActionRetireRecreate
	recreate.NewOwnerUserID = model.NewId()
	require.Contains(t, p.webhookTransferApprovalBlockerFor(&recreate, acting, true), "no longer exists")
}

func TestFulfillWebhookTransfer_RetireOnlySucceedsWhenTheHookIsGone(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	store := memKV(api)
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.auditSink = func(auditEntry, auditEventSpec) {}
	api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()

	src := &webhookRequest{ID: "orig", RequesterID: "u_req", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", TeamID: "t1", DisplayName: "Relay"}
	require.NoError(t, p.writeWebhookIndex(src, "hook1", "admin1"))
	p.deleteWebhookFn = func(string, string) error { return &model.AppError{StatusCode: 404, Message: "not found"} }

	req := &webhookTransferRequest{
		ID: "t-retire", RequesterID: "u_req", HookID: "hook1", WebhookType: webhookTypeOutgoing,
		DisplayName: "Relay", ChannelID: "ch1", Action: transferActionRetireOnly,
	}
	store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = transferClaim{requestID: req.ID}.encode()

	res := p.fulfillWebhookTransferRequest(req,
		&model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"},
		newDecisionCards(req.ID, "", ""), nil, kvWebhookTransferRequestPrefix+req.ID)

	require.Contains(t, res.Outcome, "retired")
	require.NotContains(t, res.Outcome, "with issues")
	index, err := p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.NotZero(t, index.RetiredAt)
}

func TestWebhookTransferApprovalBlocker_SkipsLiveLookupOnFirstStep(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	memKV(api)
	p := newTestPlugin(api)

	newOwnerID := model.NewId()
	src := &webhookRequest{ID: "orig", RequesterID: "u_req", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", TeamID: "t1", DisplayName: "Relay"}
	require.NoError(t, p.writeWebhookIndex(src, "hook1", "admin1"))
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetUser", newOwnerID).Return(&model.User{Id: newOwnerID, Username: "newowner"}, nil).Maybe()
	api.On("GetChannelMember", mock.Anything, mock.Anything).Return(&model.ChannelMember{}, nil).Maybe()
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Type: model.ChannelTypeOpen}, nil).Maybe()

	looked := 0
	p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
		looked++
		return nil, errors.New("403 forbidden")
	}

	req := &webhookTransferRequest{
		ID: "t1", RequesterID: "u_req", HookID: "hook1", WebhookType: webhookTypeOutgoing,
		ChannelID: "ch1", Action: transferActionRetireRecreate, NewOwnerUserID: newOwnerID,
	}
	acting := &model.User{Id: "sec1", Username: "sec", Roles: "system_user"}
	require.Empty(t, p.webhookTransferApprovalBlockerFor(req, acting, false))
	require.Zero(t, looked, "the first step must not need webhook read permission")

	require.Contains(t, p.webhookTransferApprovalBlockerFor(req, acting, true), "still exists")
	require.Equal(t, 1, looked)
}

// Both the 9.11 federal build and Mattermost 11 restore a webhook's stored
// creator, so Reassign is refused from major 9 up.
func TestSubmitWebhookTransferRejectsReassignOnServersThatKeepTheCreator(t *testing.T) {
	for _, version := range []string{"9.11.0", "10.5.1", "11.0.0"} {
		t.Run(version, func(t *testing.T) {
			api := &plugintest.API{}
			// Registered before the shared stubs so this version wins.
			api.On("GetServerVersion").Return(version).Maybe()
			stubLogs(api)
			store := memKV(api)
			api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
			p := newTestPlugin(api)

			store[webhookIndexKey("hook1")] = recordJSON(t, webhookIndexEntry{
				HookID: "hook1", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", RequesterID: "u_req", OwnerUserID: "u_req",
			})
			_, err := p.submitWebhookTransferRequest("u_req", webhookTransferInput{HookID: "hook1", Action: transferActionReassign})
			require.ErrorContains(t, err, "Retire & recreate")
		})
	}
}

func TestWebhookReassignUnsupported(t *testing.T) {
	for version, unsupported := range map[string]bool{
		"8.1.0":   false,
		"9.0.0":   true,
		"9.11.0":  true,
		"10.11.2": true,
		"11.0.0":  true,
		"unknown": false,
	} {
		api := &plugintest.API{}
		api.On("GetServerVersion").Return(version).Maybe()
		stubLogLevels(api)
		require.Equal(t, unsupported, newTestPlugin(api).webhookReassignUnsupported(), version)
	}
}

func TestServerMajorVersion(t *testing.T) {
	major, ok := serverMajorVersion("11.2.3")
	require.True(t, ok)
	require.Equal(t, 11, major)

	major, ok = serverMajorVersion(" 9.11.0 ")
	require.True(t, ok)
	require.Equal(t, 9, major)

	_, ok = serverMajorVersion("")
	require.False(t, ok)
	_, ok = serverMajorVersion("unknown")
	require.False(t, ok)
}

func TestFulfillWebhookRetireRecreate_IncomingUndeliveredKeepsOldHook(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		name := "replacement cleaned up"
		if cleanupFails {
			name = "replacement cleanup fails"
		}
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			store := memKV(api)
			stubSiteURL(api, "http://localhost:8065")
			p := newTestPlugin(api)
			p.botUserID = "bot"
			p.auditSink = func(auditEntry, auditEventSpec) {}

			var deleted []string
			p.incomingWebhookChannelFn = func(string, string) (string, error) { return "ch1", nil }
			p.createWebhookFn = func(*webhookRequest, string) (string, string, error) {
				return "Incoming webhook URL: http://localhost:8065/hooks/new-hook", "new-hook", nil
			}
			p.deleteWebhookFn = func(_, hookID string) error {
				deleted = append(deleted, hookID)
				if cleanupFails {
					return errors.New("server unavailable")
				}
				return nil
			}
			// The new owner's DM can't be opened, so the new URL never arrives.
			api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(nil, testAppErr("no dm")).Maybe()
			api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_new", Username: "newowner"}, nil).Maybe()
			api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
			api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
			api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()

			req := &webhookTransferRequest{
				ID: "t-in", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeIncoming,
				DisplayName: "Alerts", ChannelID: "ch1", TeamID: "t1",
				Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
			}
			store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = []byte(req.ID)
			raw := []byte(`{"id":"t-in"}`)
			key := kvWebhookTransferRequestPrefix + req.ID

			res := p.fulfillWebhookRetireRecreate(req,
				&model.User{Id: "admin1", Username: "admin"},
				&model.User{Id: "u_req", Username: "req"},
				newDecisionCards(req.ID, "", ""), raw, key)

			require.Equal(t, []string{"new-hook"}, deleted, "only the undelivered replacement may be deleted, never the old hook")
			require.True(t, res.audited)

			var stored webhookTransferRequest
			require.NoError(t, json.Unmarshal(store[key], &stored))
			if cleanupFails {
				// The replacement is still live and nobody holds its URL,
				// so the ticket has to stay the only pointer to it.
				require.Equal(t, cutoverStageCleanupPending, stored.CutoverStage)
				require.Equal(t, "new-hook", stored.ResumeNewHookID)
				return
			}
			require.Equal(t, raw, store[key], "the ticket is put back for a retry")
		})
	}
}

func TestFulfillWebhookTransfer_ReassignKeepsTheTicketWhenTheIndexWriteFails(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	store := memKV(api)
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.auditSink = func(auditEntry, auditEventSpec) {}
	api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_new", Username: "newowner"}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()

	req := &webhookTransferRequest{
		ID: model.NewId(), RequesterID: "u_req", HookID: "hook1", WebhookType: webhookTypeOutgoing,
		DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
		Action: transferActionReassign, NewOwnerUserID: "u_new",
	}
	key := kvWebhookTransferRequestPrefix + req.ID
	raw := recordJSON(t, req)
	store[key] = raw
	store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = transferClaim{requestID: req.ID, leaseAt: model.GetMillis()}.encode()

	reassigned := false
	p.updateWebhookOwnerFn = func(string, string, string, string) error {
		reassigned = true
		return nil
	}
	t.Cleanup(func() { p.updateWebhookOwnerFn = nil })
	// No index row, so recording the new owner fails after Mattermost moved
	// the hook — the previous owner would otherwise keep transfer rights.
	res := p.fulfillWebhookTransferRequest(req, &model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"}, newDecisionCards(req.ID, "", ""), raw, key)

	require.True(t, reassigned)
	require.NotContains(t, res.Outcome, "Fully approved")
	require.NotEmpty(t, store[key], "the ticket stays open so Approve can finish the handover")
}

func TestBotTransferApprovalBlocker_LetsAHalfDoneTransferFinish(t *testing.T) {
	newOwner := model.NewId()
	requester := model.NewId()
	previous := model.NewId()

	setup := func(t *testing.T) (*plugintest.API, *Plugin, *botTransferRequest) {
		t.Helper()
		api := &plugintest.API{}
		stubLogLevels(api)
		store := memKV(api)
		p := newTestPlugin(api)
		req := &botTransferRequest{
			ID: model.NewId(), RequesterID: requester, BotUserID: "bot1", BotUsername: "svcbot",
			Action: transferActionTransfer, NewOwnerUserID: newOwner, Reason: transferReasonVoluntary,
		}
		store[kvBotIndexPrefix+req.BotUserID] = recordJSON(t, &botIndexEntry{
			BotUserID: req.BotUserID, Username: req.BotUsername, RequesterID: requester, OwnerUserID: previous,
		})
		return api, p, req
	}

	t.Run("the bot already belongs to the successor", func(t *testing.T) {
		api, p, req := setup(t)
		api.On("GetBot", req.BotUserID, true).Return(&model.Bot{UserId: req.BotUserID, OwnerId: newOwner}, nil)

		require.Empty(t, p.botTransferApprovalBlocker(req),
			"the requester no longer owns the bot, but the retry still has the index row and tokens to finish")
	})

	t.Run("a resume finishes once the bot belongs to the successor", func(t *testing.T) {
		api, p, req := setup(t)
		req.ResumeAfterAssign = true
		api.On("GetBot", req.BotUserID, true).Return(&model.Bot{UserId: req.BotUserID, OwnerId: newOwner}, nil)

		require.Empty(t, p.botTransferApprovalBlocker(req))
	})

	t.Run("a resume whose ownership never moved is checked again", func(t *testing.T) {
		api, p, req := setup(t)
		req.ResumeAfterAssign = true
		api.On("GetBot", req.BotUserID, true).Return(&model.Bot{UserId: req.BotUserID, OwnerId: previous}, nil)
		api.On("GetUser", mock.Anything).Return(func(id string) (*model.User, *model.AppError) {
			return &model.User{Id: id, Username: "u"}, nil
		}).Maybe()

		// The bot never moved, so this retry is about to call AssignBot —
		// the nominate check has to run as it would on a first approval.
		require.NotEmpty(t, p.botTransferApprovalBlocker(req))
	})

	t.Run("an untouched ticket is still checked against its owner", func(t *testing.T) {
		api, p, req := setup(t)
		api.On("GetBot", req.BotUserID, true).Return(&model.Bot{UserId: req.BotUserID, OwnerId: previous}, nil)
		api.On("GetUser", mock.Anything).Return(func(id string) (*model.User, *model.AppError) {
			return &model.User{Id: id, Username: "u"}, nil
		}).Maybe()

		require.NotEmpty(t, p.botTransferApprovalBlocker(req),
			"the requester never owned this bot, so a first approval must refuse")
	})
}

func TestWebhookTransferAuditSnapshot_HidesIncomingHookID(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	var got []auditEntry
	p.auditSink = func(e auditEntry, _ auditEventSpec) { got = append(got, e) }

	incoming := &webhookTransferRequest{ID: "r1", HookID: "secret-hook-id", TransferHandle: "handle1", WebhookType: webhookTypeIncoming, ResumeNewHookID: "secret-new-hook-id"}
	outgoing := &webhookTransferRequest{ID: "r2", HookID: "out-hook-id", WebhookType: webhookTypeOutgoing}
	p.recordEvent(auditEntry{RequestID: "r1", Kind: pendingKindWebhookTransfer, Event: eventApproved, Snapshot: incoming})
	p.recordEvent(auditEntry{RequestID: "r2", Kind: pendingKindWebhookTransfer, Event: eventApproved, Snapshot: outgoing})
	require.Len(t, got, 2)

	in, err := json.Marshal(got[0].Snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(in), "secret-hook-id")
	require.NotContains(t, string(in), "secret-new-hook-id", "a staged replacement ID is as good as the original")
	require.Contains(t, string(in), `"transfer_handle":"handle1"`)
	require.Equal(t, "secret-hook-id", incoming.HookID, "the live request keeps its hook ID")
	require.Equal(t, "secret-new-hook-id", incoming.ResumeNewHookID)

	out, err := json.Marshal(got[1].Snapshot)
	require.NoError(t, err)
	require.Contains(t, string(out), "out-hook-id", "outgoing hook IDs aren't secret")
}

func TestWebhookTransferApprovalBlocker_RefusesOrphanedIncomingReassign(t *testing.T) {
	api := &plugintest.API{}
	// A server old enough to still move a webhook's creator, so the orphan
	// check is what refuses rather than the version gate.
	api.On("GetServerVersion").Return("8.1.0").Maybe()
	stubLogLevels(api)
	memKV(api)
	p := newTestPlugin(api)

	src := &webhookRequest{ID: "orig", RequesterID: "u_req", WebhookType: webhookTypeIncoming, ChannelID: "ch1", TeamID: "t1", DisplayName: "Alerts"}
	require.NoError(t, p.writeWebhookIndex(src, "hook1", "admin1"))

	// The owner was deactivated after the ticket was filed.
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", DeleteAt: 1}, nil)
	api.On("GetUser", "u_new").Return(&model.User{Id: "u_new", Username: "newowner"}, nil).Maybe()
	api.On("GetChannelMember", mock.Anything, mock.Anything).Return(&model.ChannelMember{}, nil).Maybe()

	req := &webhookTransferRequest{
		ID: "t1", RequesterID: "u_req", HookID: "hook1", WebhookType: webhookTypeIncoming,
		Action: transferActionReassign, NewOwnerUserID: "u_new",
	}
	msg := p.webhookTransferApprovalBlockerFor(req, nil, true)
	require.Contains(t, msg, "Retire & recreate")
}

func TestRestoreTransferRecord(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)
	claimKey := transferAssetClaimKey(pendingKindBotTransfer, "bot1")
	key := kvBotTransferRequestPrefix + "req-a"
	raw := []byte(`{"id":"req-a"}`)

	store[claimKey] = []byte("req-a")
	restored, err := p.restoreTransferRecord(pendingKindBotTransfer, "bot1", "req-a", key, raw)
	require.NoError(t, err)
	require.True(t, restored)
	require.Equal(t, raw, store[key])

	// Another ticket took the claim while the record was missing: the
	// record must not come back, or the asset would have two open tickets.
	delete(store, key)
	store[claimKey] = []byte("req-b")
	restored, err = p.restoreTransferRecord(pendingKindBotTransfer, "bot1", "req-a", key, raw)
	require.NoError(t, err)
	require.False(t, restored)
	_, present := store[key]
	require.False(t, present)
}

func TestRestoreTransferRecord_ClaimCheckErrorPublishesNothing(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := map[string][]byte{}
	api.On("KVSet", mock.Anything, mock.Anything).Return(func(key string, value []byte) *model.AppError {
		store[key] = value
		return nil
	})
	api.On("KVGet", mock.Anything).Return(
		func(key string) []byte {
			if strings.HasPrefix(key, kvTransferAssetClaimPrefix) {
				return nil
			}
			return store[key]
		},
		func(key string) *model.AppError {
			if strings.HasPrefix(key, kvTransferAssetClaimPrefix) {
				return testAppErr("kv down")
			}
			return nil
		},
	)
	api.On("KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	api.On("KVCompareAndDelete", mock.Anything, mock.Anything).Return(true, nil).Maybe()
	p := newTestPlugin(api)
	key := kvBotTransferRequestPrefix + "req-a"
	raw := []byte(`{"id":"req-a"}`)

	restored, err := p.restoreTransferRecord(pendingKindBotTransfer, "bot1", "req-a", key, raw)
	require.Error(t, err)
	require.False(t, restored)
	// Ownership could not be confirmed. Publishing anyway is how one asset
	// ends up with two open tickets, so nothing is written.
	require.NotContains(t, store, key)
}

func TestResolveWebhookTransferHookID_FallsBackToIndex(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)

	src := &webhookRequest{ID: "orig", RequesterID: "u_req", WebhookType: webhookTypeIncoming, ChannelID: "ch1", TeamID: "t1", DisplayName: "Alerts"}
	require.NoError(t, p.writeWebhookIndex(src, "hook1", "admin1"))
	entry, err := p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.NotEmpty(t, entry.TransferHandle)

	// The best-effort handle map write was lost.
	delete(store, kvWebhookTransferHandlePrefix+entry.TransferHandle)

	got, err := p.resolveWebhookTransferHookID(entry.TransferHandle)
	require.NoError(t, err)
	require.Equal(t, "hook1", got)

	_, err = p.resolveWebhookTransferHookID(model.NewId())
	require.Error(t, err)
}

func TestRevokeBotTokens_PagesAndKeepsExpiryOnFail(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)

	listed := []*model.UserAccessToken{
		{Id: "tok-a"}, {Id: "tok-b"}, {Id: "tok-c"},
	}
	p.listBotAccessTokensFn = func(botUserID, adminUserID string) ([]*model.UserAccessToken, error) {
		require.Equal(t, "bot1", botUserID)
		return listed, nil
	}
	var revoked []string
	api.On("RevokeUserAccessToken", mock.Anything).Return(func(id string) *model.AppError {
		revoked = append(revoked, id)
		if id == "tok-b" {
			return testAppErr("transient")
		}
		return nil
	})

	expiryKey := botTokenExpiryKey("tok-b")
	store[expiryKey] = recordJSON(t, botTokenExpiry{TokenID: "tok-b", BotUserID: "bot1"})
	store[botTokenExpiryKey("tok-a")] = recordJSON(t, botTokenExpiry{TokenID: "tok-a", BotUserID: "bot1"})

	n, err := p.revokeBotTokens("bot1", "admin1", transferLease{})
	require.Error(t, err)
	require.Equal(t, 2, n) // tok-a + tok-c
	// tok-b is attempted from the API list and again via its expiry row.
	require.Equal(t, []string{"tok-a", "tok-b", "tok-c", "tok-b"}, revoked)
	_, kept := store[expiryKey]
	require.True(t, kept, "failed revoke must keep expiry tracking")
	_, dropped := store[botTokenExpiryKey("tok-a")]
	require.False(t, dropped, "confirmed revoke drops expiry")
}

func TestRevokeNewlyIssuedBotToken_RevokesByID(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)

	// A stale row for an old token whose revoke failed earlier must not be
	// picked instead of the new token.
	store[botTokenExpiryKey("tok-old")] = recordJSON(t, botTokenExpiry{TokenID: "tok-old", BotUserID: "bot1"})
	store[botTokenExpiryKey("tok-new")] = recordJSON(t, botTokenExpiry{TokenID: "tok-new", BotUserID: "bot1"})
	var revoked []string
	api.On("RevokeUserAccessToken", mock.Anything).Return(func(id string) *model.AppError {
		revoked = append(revoked, id)
		return nil
	})

	require.NoError(t, p.revokeNewlyIssuedBotToken("tok-new"))
	require.Equal(t, []string{"tok-new"}, revoked)
	_, newKept := store[botTokenExpiryKey("tok-new")]
	require.False(t, newKept)
	_, oldKept := store[botTokenExpiryKey("tok-old")]
	require.True(t, oldKept)

	// A token with no expiry has no row; it is still revoked by ID.
	require.NoError(t, p.revokeNewlyIssuedBotToken("tok-noexpiry"))
	require.Equal(t, []string{"tok-new", "tok-noexpiry"}, revoked)

	require.Error(t, p.revokeNewlyIssuedBotToken(""))
}

func TestEnsureIncomingTransferHandle_StaleCopyKeepsRetire(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)

	// The picker listed this legacy incoming row (no handle) before a
	// retire landed; its copy still says live and names the old owner.
	stale := webhookIndexEntry{HookID: "hook1", WebhookType: webhookTypeIncoming, RequesterID: "u_req", OwnerUserID: "u_old"}
	current := stale
	current.RetiredAt = 1234
	current.OwnerUserID = "u_new"
	store[webhookIndexKey("hook1")] = recordJSON(t, current)

	handle := p.ensureIncomingTransferHandle(&stale)
	require.NotEmpty(t, handle)
	require.NotEqual(t, "hook1", handle)

	got, err := p.loadWebhookIndex("hook1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, int64(1234), got.RetiredAt, "a stale listing copy must not un-retire the hook")
	require.Equal(t, "u_new", got.OwnerUserID, "a stale listing copy must not restore the old owner")
	require.Equal(t, handle, got.TransferHandle)
	// stubLogLevels answers handle-map writes itself, so check the call.
	api.AssertCalled(t, "KVSet", kvWebhookTransferHandlePrefix+handle, []byte("hook1"))
}

func TestRotateBotToken_CreatesBeforeRevoking(t *testing.T) {
	setup := func(t *testing.T) (*Plugin, *plugintest.API, *[]string) {
		api := &plugintest.API{}
		stubLogLevels(api)
		memKV(api)
		p := newTestPlugin(api)
		var revoked []string
		p.listBotAccessTokensFn = func(string, string) ([]*model.UserAccessToken, error) {
			return []*model.UserAccessToken{{Id: "tok-old-a"}, {Id: "tok-old-b"}}, nil
		}
		api.On("RevokeUserAccessToken", mock.Anything).Return(func(id string) *model.AppError {
			revoked = append(revoked, id)
			return nil
		}).Maybe()
		return p, api, &revoked
	}

	t.Run("old tokens die only after the replacement exists", func(t *testing.T) {
		p, api, revoked := setup(t)
		created := false
		api.On("CreateUserAccessToken", mock.Anything).Return(func(tok *model.UserAccessToken) (*model.UserAccessToken, *model.AppError) {
			require.Empty(t, *revoked, "nothing may be revoked before the replacement exists")
			created = true
			return &model.UserAccessToken{Id: "tok-new", Token: "secret", UserId: tok.UserId}, nil
		})

		n, tokenID, tokenValue, _, err := p.rotateBotToken("bot1", "svcbot", "u_new", "admin1", transferLease{requestID: "req-1"})
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, 2, n)
		require.Equal(t, "tok-new", tokenID)
		require.Equal(t, "secret", tokenValue)
		require.Equal(t, []string{"tok-old-a", "tok-old-b"}, *revoked)
		require.NotContains(t, *revoked, "tok-new")
	})

	t.Run("a failed create leaves the old tokens working", func(t *testing.T) {
		p, api, revoked := setup(t)
		api.On("CreateUserAccessToken", mock.Anything).Return(nil, testAppErr("token service down"))

		n, _, tokenValue, _, err := p.rotateBotToken("bot1", "svcbot", "u_new", "admin1", transferLease{requestID: "req-1"})
		require.Error(t, err)
		require.Zero(t, n)
		require.Empty(t, tokenValue)
		require.Empty(t, *revoked, "a bot with no new token must keep the ones it has")
	})
}

func TestListBotAccessTokens_FailsClosedOnAnOverlappingPage(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)

	// A full page whose last row repeats on the next page: the window
	// shifted mid-walk, so some other token was missed.
	page := make([]*model.UserAccessToken, 0, botAccessTokenListPerPage)
	for i := range botAccessTokenListPerPage {
		page = append(page, &model.UserAccessToken{Id: fmt.Sprintf("tok-%03d", i)})
	}
	calls := 0
	p.listBotAccessTokensPageFn = func(botUserID, adminUserID string, pageNum int) ([]*model.UserAccessToken, error) {
		calls++
		if pageNum == 0 {
			return page, nil
		}
		return []*model.UserAccessToken{page[len(page)-1]}, nil
	}

	_, err := p.listBotAccessTokens("bot1", "admin1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "shifted")
	require.Equal(t, 2, calls, "the walk stops as soon as the overlap shows up")
}

func TestRotateBotTokenForTransfer_VoluntaryDeliversBeforeRevoking(t *testing.T) {
	// beforePost runs inside CreatePost, so a subtest can pin down what had
	// already been revoked at the moment the secret message went out.
	setup := func(t *testing.T, beforePost func(revoked []string)) (*Plugin, *plugintest.API, *[]string, *[]string) {
		api := &plugintest.API{}
		stubLogLevels(api)
		memKV(api)
		p := newTestPlugin(api)
		p.botUserID = "bot"
		var revoked, posted []string
		p.listBotAccessTokensFn = func(string, string) ([]*model.UserAccessToken, error) {
			return []*model.UserAccessToken{{Id: "tok-old"}}, nil
		}
		api.On("CreateUserAccessToken", mock.Anything).Return(&model.UserAccessToken{Id: "tok-new", Token: "secret"}, nil).Maybe()
		api.On("RevokeUserAccessToken", mock.Anything).Return(func(id string) *model.AppError {
			revoked = append(revoked, id)
			return nil
		}).Maybe()
		api.On("CreatePost", mock.Anything).Return(func(post *model.Post) (*model.Post, *model.AppError) {
			if beforePost != nil {
				beforePost(revoked)
			}
			posted = append(posted, post.Message)
			return &model.Post{Id: "p1"}, nil
		}).Maybe()
		api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
		return p, api, &revoked, &posted
	}
	req := func(reason string) *botTransferRequest {
		return &botTransferRequest{
			ID: "req-1", RequesterID: "u_req", BotUserID: "bot1", BotUsername: "svcbot",
			Action: transferActionTransfer, NewOwnerUserID: "u_new", Reason: reason, RotateToken: true,
		}
	}

	t.Run("voluntary: the DM lands before any revoke", func(t *testing.T) {
		// The old token must still work while the secret message is being
		// written, not merely while the DM channel is being opened.
		p, api, revoked, posted := setup(t, func(revokedSoFar []string) {
			require.Empty(t, revokedSoFar, "the old tokens must still work until the secret message is posted")
		})
		api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(func(string, string) (*model.Channel, *model.AppError) {
			require.Empty(t, *revoked, "the old tokens must still work until the new one is delivered")
			return &model.Channel{Id: "dm1"}, nil
		}).Maybe()

		out := p.rotateBotTokenForTransfer(req(transferReasonVoluntary), "admin1", transferLease{})
		require.True(t, out.rotated)
		require.Empty(t, out.partialErrors)
		require.Equal(t, []string{"tok-old"}, *revoked, "the old token goes once the new one is in the owner's hands")
		require.Len(t, *posted, 1)
	})

	t.Run("voluntary: an undelivered token leaves the old ones working", func(t *testing.T) {
		p, api, revoked, _ := setup(t, nil)
		api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(nil, testAppErr("no dm")).Maybe()

		out := p.rotateBotTokenForTransfer(req(transferReasonVoluntary), "admin1", transferLease{})
		require.False(t, out.rotated)
		require.Equal(t, []string{"tok-new"}, *revoked, "only the undelivered replacement is revoked")
		require.Len(t, out.partialErrors, 1)
		require.Contains(t, out.partialErrors[0], "old tokens were left working")
	})

	t.Run("offboarding: the old tokens go first", func(t *testing.T) {
		p, api, revoked, _ := setup(t, nil)
		api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(func(string, string) (*model.Channel, *model.AppError) {
			require.Equal(t, []string{"tok-old"}, *revoked, "an offboarding handover cuts the previous owner off first")
			return &model.Channel{Id: "dm1"}, nil
		}).Maybe()

		out := p.rotateBotTokenForTransfer(req(transferReasonOffboarding), "admin1", transferLease{})
		require.True(t, out.rotated)
		require.Equal(t, 1, out.revoked)
	})
}

func TestRotateBotTokenForTransfer_FailedReplacement(t *testing.T) {
	setup := func(t *testing.T) (*Plugin, *[]string, *[]string) {
		api := &plugintest.API{}
		stubLogLevels(api)
		memKV(api)
		p := newTestPlugin(api)
		p.botUserID = "bot"

		var revoked, posted []string
		p.listBotAccessTokensFn = func(string, string) ([]*model.UserAccessToken, error) {
			return []*model.UserAccessToken{{Id: "tok-old"}}, nil
		}
		api.On("CreateUserAccessToken", mock.Anything).Return(nil, testAppErr("token service down")).Maybe()
		api.On("RevokeUserAccessToken", mock.Anything).Return(func(id string) *model.AppError {
			revoked = append(revoked, id)
			return nil
		}).Maybe()
		api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
		api.On("CreatePost", mock.Anything).Return(func(post *model.Post) (*model.Post, *model.AppError) {
			posted = append(posted, post.Message)
			return &model.Post{Id: "p1"}, nil
		}).Maybe()
		return p, &revoked, &posted
	}
	req := func(reason string) *botTransferRequest {
		return &botTransferRequest{
			ID: "req-1", RequesterID: "u_req", BotUserID: "bot1", BotUsername: "svcbot",
			Action: transferActionTransfer, NewOwnerUserID: "u_new", Reason: reason, RotateToken: true,
		}
	}

	t.Run("offboarding still revokes the old tokens", func(t *testing.T) {
		p, revoked, posted := setup(t)
		out := p.rotateBotTokenForTransfer(req(transferReasonOffboarding), "admin1", transferLease{})

		require.False(t, out.rotated)
		require.Equal(t, 1, out.revoked)
		// Cutting the previous owner off is the point of the rotation, and
		// ownership has already moved.
		require.Equal(t, []string{"tok-old"}, *revoked)
		require.Empty(t, *posted, "there is no new secret to hand over")
		require.Len(t, out.partialErrors, 1)
		require.Contains(t, out.partialErrors[0], "previous owner's tokens were revoked")
	})

	t.Run("voluntary leaves the old tokens working", func(t *testing.T) {
		p, revoked, posted := setup(t)
		out := p.rotateBotTokenForTransfer(req(transferReasonVoluntary), "admin1", transferLease{})

		require.False(t, out.rotated)
		require.Zero(t, out.revoked)
		// Nobody holds a working token for the bot yet, so revoking would
		// leave it unreachable.
		require.Empty(t, *revoked)
		require.Empty(t, *posted)
		require.Len(t, out.partialErrors, 1)
		require.Contains(t, out.partialErrors[0], "left working")
	})
}

func TestRestoreTransferRecord_BumpsClaimGenerationBeforePublishing(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := memKV(api)
	p := newTestPlugin(api)

	claimKey := transferAssetClaimKey(pendingKindBotTransfer, "bot1")
	key := kvBotTransferRequestPrefix + "req-a"
	raw := []byte(`{"id":"req-a"}`)
	store[claimKey] = []byte("req-a|3")

	restored, err := p.restoreTransferRecord(pendingKindBotTransfer, "bot1", "req-a", key, raw)
	require.NoError(t, err)
	require.True(t, restored)
	require.Equal(t, raw, store[key])

	claim := parseTransferClaim(store[claimKey])
	require.Equal(t, "req-a", claim.requestID)
	require.Equal(t, int64(4), claim.generation, "a takeover holding the old value must lose its CAS")
	require.Equal(t, transferPhaseHeld, claim.phase, "the restore is finished, so the asset is a plain pending ticket again")
	require.False(t, claim.leaseExpired(model.GetMillis()))
}

func TestRestoreTransferRecord_HoldsTheRestoringPhaseWhilePublishing(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)

	claimKey := transferAssetClaimKey(pendingKindBotTransfer, "bot1")
	key := kvBotTransferRequestPrefix + "req-a"
	raw := []byte(`{"id":"req-a"}`)
	store := map[string][]byte{
		claimKey: transferClaim{requestID: "req-a", phase: transferPhaseFulfilling, leaseAt: model.GetMillis()}.encode(),
	}
	var p *Plugin
	var duringRestore error

	api.On("KVGet", mock.Anything).Return(func(k string) ([]byte, *model.AppError) {
		return store[k], nil
	}).Maybe()
	api.On("KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything).Return(
		func(k string, old, value []byte) (bool, *model.AppError) {
			if !bytes.Equal(store[k], old) {
				return false, nil
			}
			store[k] = value
			return true, nil
		}).Maybe()
	api.On("KVSet", mock.Anything, mock.Anything).Return(func(k string, value []byte) *model.AppError {
		// Another submitter racing the restore must be refused at the
		// moment the record goes back, not only before it.
		if k == key {
			require.Equal(t, transferPhaseRestoring, parseTransferClaim(store[claimKey]).phase)
			duringRestore = p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b")
		}
		store[k] = value
		return nil
	}).Maybe()
	p = newTestPlugin(api)

	restored, err := p.restoreTransferRecord(pendingKindBotTransfer, "bot1", "req-a", key, raw)
	require.NoError(t, err)
	require.True(t, restored)
	require.Error(t, duringRestore)
	require.Equal(t, "req-a", parseTransferClaim(store[claimKey]).requestID)
}

func TestReleaseTransferAssetClaim_NeverLogsTheKey(t *testing.T) {
	api := &plugintest.API{}
	store := map[string][]byte{}
	api.On("KVGet", mock.Anything).Return(func(key string) ([]byte, *model.AppError) {
		return store[key], nil
	}).Maybe()
	api.On("KVCompareAndDelete", mock.Anything, mock.Anything).Return(false, testAppErr("kv down")).Maybe()
	var logged []any
	for arity := 1; arity <= 16; arity++ {
		args := make([]any, arity)
		for i := range args {
			args[i] = mock.Anything
		}
		api.On("LogWarn", args...).Run(func(a mock.Arguments) { logged = append(logged, a...) }).Maybe()
	}
	stubLogLevels(api)
	p := newTestPlugin(api)

	assetKey := webhookTypeIncoming + "/secret-hook-id"
	store[transferAssetClaimKey(pendingKindWebhookTransfer, assetKey)] = transferClaim{requestID: "req-a"}.encode()
	p.releaseTransferAssetClaim(pendingKindWebhookTransfer, assetKey, "req-a")

	require.NotEmpty(t, logged)
	require.NotContains(t, fmt.Sprintf("%v", logged), "secret-hook-id")
}

func TestBeginTransferFulfill_RefusesASecondApprovalUnderALiveLease(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := fakeKV(api)
	p := newTestPlugin(api)

	claimKey := transferAssetClaimKey(pendingKindBotTransfer, "bot1")
	store[claimKey] = transferClaim{requestID: "req-a", phase: transferPhaseHeld, leaseAt: model.GetMillis()}.encode()

	created, first, err := p.beginTransferFulfill(pendingKindBotTransfer, "bot1", "req-a")
	require.NoError(t, err)
	require.False(t, created)
	require.NotEmpty(t, first)

	// A second approver clicking while the first run is still going must
	// not start the same irreversible side effects twice.
	_, _, err = p.beginTransferFulfill(pendingKindBotTransfer, "bot1", "req-a")
	require.ErrorIs(t, err, errTransferFulfillInFlight)

	res, ok := p.claimTransferForFulfill(pendingKindBotTransfer, "bot1", "req-a",
		kvBotTransferRequestPrefix+"req-a", nil, newDecisionCards("req-a", "", ""), "bot transfer request")
	require.False(t, ok)
	require.Contains(t, res.Reply, "already being carried out")

	// Once the lease ages out the checkpoint is what lets a retry pick the
	// work up again.
	stale := parseTransferClaim(store[claimKey])
	stale.leaseAt = model.GetMillis() - transferFulfillWindow.Milliseconds() - 1
	store[claimKey] = stale.encode()
	_, second, err := p.beginTransferFulfill(pendingKindBotTransfer, "bot1", "req-a")
	require.NoError(t, err)
	require.NotEqual(t, first, second, "each attempt writes bytes only it can take back")
}

func TestDeleteTransferClaimIfUnchanged_OnlyDropsItsOwnAttempt(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := fakeKV(api)
	p := newTestPlugin(api)

	claimKey := transferAssetClaimKey(pendingKindBotTransfer, "bot1")
	created, mine, err := p.beginTransferFulfill(pendingKindBotTransfer, "bot1", "req-a")
	require.NoError(t, err)
	require.True(t, created)

	stale := parseTransferClaim(store[claimKey])
	stale.leaseAt = model.GetMillis() - transferFulfillWindow.Milliseconds() - 1
	store[claimKey] = stale.encode()
	_, theirs, err := p.beginTransferFulfill(pendingKindBotTransfer, "bot1", "req-a")
	require.NoError(t, err)
	require.NotEqual(t, mine, theirs)

	p.deleteTransferClaimIfUnchanged(pendingKindBotTransfer, "bot1", "req-a", mine)
	require.NotEmpty(t, store[claimKey], "an earlier attempt must not drop the row a later one owns")
}

func TestResumeWebhookRetireRecreate_RefusesWhenTheReplacementIsGone(t *testing.T) {
	api, p, store := cutoverFixture(t)
	req, key, _ := stageCutover(t, store, model.NewId())
	req.NewOwnerUserID = model.NewId()
	req.stageCutover(cutoverStageReplacementReady, webhookCutover{newHookID: "new-hook", channelID: "ch1"})
	raw := recordJSON(t, req)
	store[key] = raw
	api.On("GetChannelMember", "ch1", req.NewOwnerUserID).Return(&model.ChannelMember{}, nil).Maybe()
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Type: model.ChannelTypeOpen}, nil).Maybe()

	// The staged replacement was removed by hand while the cutover sat.
	p.getOutgoingWebhookForRecreateFn = func(hookID, _ string) (*outgoingWebhookRecreate, error) {
		if hookID == "new-hook" {
			return nil, errors.New("404 not found")
		}
		return &outgoingWebhookRecreate{ChannelID: "ch1", CallbackURLs: []string{"https://hooks.example.com/out"}}, nil
	}
	var deleted []string
	p.deleteWebhookFn = func(_, hookID string) error {
		deleted = append(deleted, hookID)
		return nil
	}

	res := p.fulfillWebhookRetireRecreate(req, &model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"}, newDecisionCards(req.ID, "", ""), raw, key)

	require.Empty(t, deleted, "a checkpoint that outlived its replacement must not cost the old hook too")
	require.NotContains(t, res.Outcome, "recreated")

	var stored webhookTransferRequest
	require.NoError(t, json.Unmarshal(store[key], &stored))
	require.False(t, stored.cutoverInProgress(), "the ticket is plain again, so a retry builds a fresh replacement")
}

func TestFulfillWebhookRetireRecreate_ChecksTheSuccessorAgainstTheLiveChannel(t *testing.T) {
	api, p, store := cutoverFixture(t)
	req, key, _ := stageCutover(t, store, "t-live-channel")
	// The ticket was filed against a team-wide hook, so nobody vetted the
	// successor for the channel the hook sits in now.
	req.ChannelID = ""
	req.NewOwnerUserID = model.NewId()
	raw := recordJSON(t, req)
	store[key] = raw
	api.On("GetChannelMember", "ch-live", req.NewOwnerUserID).Return(nil, testAppErr("not a member")).Maybe()
	p.getOutgoingWebhookForRecreateFn = func(string, string) (*outgoingWebhookRecreate, error) {
		return &outgoingWebhookRecreate{ChannelID: "ch-live", CallbackURLs: []string{"https://hooks.example.com/out"}}, nil
	}
	created := false
	p.createOutgoingWebhookFromRecreateFn = func(*webhookRequest, *outgoingWebhookRecreate, string) (string, string, string, error) {
		created = true
		return "", "", "", nil
	}
	p.deleteWebhookFn = func(string, string) error { return nil }

	res := p.fulfillWebhookRetireRecreate(req, &model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"}, newDecisionCards(req.ID, "", ""), raw, key)

	require.False(t, created, "the successor was never vetted for the channel the hook lives in now")
	require.True(t, res.audited)
}

// incomingRecreateFixture is the retire & recreate fixture for incoming
// hooks, whose live channel is read before anything is rebuilt.
func incomingRecreateFixture(t *testing.T) (*plugintest.API, *Plugin, map[string][]byte) {
	t.Helper()
	api := &plugintest.API{}
	stubLogs(api)
	store := memKV(api)
	stubSiteURL(api, "http://localhost:8065")
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.auditSink = func(auditEntry, auditEventSpec) {}
	api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_new", Username: "newowner"}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
	return api, p, store
}

func newIncomingRecreate(store map[string][]byte, id string) *webhookTransferRequest {
	req := &webhookTransferRequest{
		ID: id, RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeIncoming,
		DisplayName: "Alerts", ChannelID: "ch1", TeamID: "t1",
		Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
	}
	claim := transferClaim{requestID: id, leaseAt: model.GetMillis()}
	store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = claim.encode()
	return req
}

func TestFulfillWebhookRetireRecreate_IncomingRefusesAHookThatMovedChannel(t *testing.T) {
	_, p, store := incomingRecreateFixture(t)
	created, deleted := false, false
	p.incomingWebhookChannelFn = func(string, string) (string, error) { return "ch-moved", nil }
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) {
		created = true
		return "", "new-hook", nil
	}
	p.deleteWebhookFn = func(string, string) error {
		deleted = true
		return nil
	}

	req := newIncomingRecreate(store, "t-in-moved")
	key := kvWebhookTransferRequestPrefix + req.ID
	res := p.fulfillWebhookRetireRecreate(req, &model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"}, newDecisionCards(req.ID, "", ""), recordJSON(t, req), key)

	require.False(t, created, "a hook that moved must not be rebuilt in the channel the reviewers approved")
	require.False(t, deleted)
	require.True(t, res.audited)
}

func TestFulfillWebhookRetireRecreate_RemovesACleanupPendingReplacementFirst(t *testing.T) {
	_, p, store := incomingRecreateFixture(t)
	p.incomingWebhookChannelFn = func(string, string) (string, error) { return "ch1", nil }
	var deleted []string
	p.deleteWebhookFn = func(_, hookID string) error {
		deleted = append(deleted, hookID)
		return nil
	}
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) {
		return "Incoming webhook URL: http://localhost:8065/hooks/new-hook-2", "new-hook-2", nil
	}

	req := newIncomingRecreate(store, "t-in-cleanup")
	req.stageCutover(cutoverStageCleanupPending, webhookCutover{newHookID: "stale-hook", channelID: "ch1"})
	key := kvWebhookTransferRequestPrefix + req.ID
	raw := recordJSON(t, req)
	store[key] = raw

	res := p.fulfillWebhookRetireRecreate(req, &model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"}, newDecisionCards(req.ID, "", ""), raw, key)

	require.Equal(t, []string{"stale-hook", "old-hook"}, deleted,
		"the unusable leftover goes before another replacement is made")
	require.Contains(t, res.Outcome, "recreated")
}

func TestBotTransferResumeAfterAssign_SurvivesTheReviewWindow(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	store := memKV(api)
	p := newTestPlugin(api)
	config := *p.getConfiguration()
	config.expiryDays = 30
	p.setConfiguration(&config)
	p.auditSink = func(auditEntry, auditEventSpec) {}
	api.On("GetUser", testSysAdminID).Return(systemAdminUser(), nil).Maybe()
	api.On("GetUser", mock.Anything).Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "p1"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()

	req := &botTransferRequest{
		ID: "t-resume-expired", RequesterID: "u_req", BotUserID: "bot1", BotUsername: "svcbot",
		Action: transferActionTransfer, NewOwnerUserID: "u_new", Reason: transferReasonVoluntary,
		CreatedAt: model.GetMillis() - 365*24*60*60*1000, ResumeAfterAssign: true,
	}
	key := kvBotTransferRequestPrefix + req.ID
	store[key] = recordJSON(t, req)

	res := p.decideBotTransferRequest(testSysAdminID, req.ID, "", false)

	require.Equal(t, resumeAfterAssignExpiryBlockedMsg, res.Reply)
	require.NotEmpty(t, store[key], "expiring deletes the only note that the bot may already have moved")
}

func TestTransferSubmitHandlers_RequireIdentityBeforeTheFeatureFlag(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{}) // every request type off

	for name, handler := range map[string]http.HandlerFunc{
		"bot":     p.handleSubmitBotTransferWebapp,
		"webhook": p.handleSubmitWebhookTransferWebapp,
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
			require.Equal(t, http.StatusUnauthorized, w.Code,
				"an anonymous caller must not learn which request types are on")
		})
	}
}
