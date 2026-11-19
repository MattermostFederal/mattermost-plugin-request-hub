package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
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

func TestClaimTransferAsset_CAS(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	store := fakeKV(api)
	p := newTestPlugin(api)

	claimHolder := func() string {
		holder, _ := parseTransferClaim(store[transferAssetClaimKey(pendingKindBotTransfer, "bot1")])
		return holder
	}

	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-a"))
	require.Equal(t, "req-a", claimHolder())

	// Live holder blocks a second claim.
	store[kvBotTransferRequestPrefix+"req-a"] = []byte(`{"id":"req-a"}`)
	err := p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already")

	// Holder record gone while its final approval is being carried out: refuse.
	delete(store, kvBotTransferRequestPrefix+"req-a")
	require.NoError(t, p.markTransferFulfilling("req-a"))
	err = p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already")

	// A marker older than the fulfill window (crash mid-fulfill) no longer
	// protects the claim.
	stale := model.GetMillis() - transferFulfillWindow.Milliseconds() - 1
	store[transferFulfillingKey("req-a")] = []byte(strconv.FormatInt(stale, 10))
	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-b"))
	require.Equal(t, "req-b", claimHolder())

	// A bare (pre-versioning) claim value still reads as its holder.
	store[transferAssetClaimKey(pendingKindBotTransfer, "bot1")] = []byte("req-gone")
	require.NoError(t, p.claimTransferAsset(pendingKindBotTransfer, "bot1", "req-c"))
	require.Equal(t, "req-c", claimHolder())

	require.NoError(t, p.markTransferFulfilling("req-c"))
	p.clearTransferFulfilling("req-c")
	_, marked := store[transferFulfillingKey("req-c")]
	require.False(t, marked)

	p.releaseTransferAssetClaim(pendingKindBotTransfer, "bot1", "req-c")
	_, held := store[transferAssetClaimKey(pendingKindBotTransfer, "bot1")]
	require.False(t, held)
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

	req := &webhookTransferRequest{
		ID: "t-resume", RequesterID: "u_req", HookID: "old-hook", WebhookType: webhookTypeOutgoing,
		DisplayName: "Relay", ChannelID: "ch1", TeamID: "t1",
		Action: transferActionRetireRecreate, NewOwnerUserID: "u_new",
		CallbackURL:        "https://hooks.example.com/out",
		ResumeNewHookID:    "new-hook",
		ResumeOldDeleted:   true,
		ResumeTriggerWords: []string{"alert", "page"},
		ResumeTriggerWhen:  1,
	}
	store[transferAssetClaimKey(pendingKindWebhookTransfer, req.pendingKey())] = []byte(req.ID)

	res := p.fulfillWebhookRetireRecreate(req,
		&model.User{Id: "admin1", Username: "admin"},
		&model.User{Id: "u_req", Username: "req"},
		newDecisionCards(req.ID, "", ""), nil, kvWebhookTransferRequestPrefix+req.ID)

	require.False(t, created, "a resumed cutover must not make a second hook")
	require.Empty(t, deleted, "the old hook was already deleted")
	require.NotNil(t, restoredSnap)
	require.Equal(t, []string{"alert", "page"}, restoredSnap.TriggerWords)
	require.Contains(t, res.Outcome, "recreated")
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

func TestSubmitWebhookTransferRejectsReassignOnV11(t *testing.T) {
	api := &plugintest.API{}
	// Registered before the shared stubs so this version wins.
	api.On("GetServerVersion").Return("11.0.0").Maybe()
	stubLogs(api)
	store := memKV(api)
	api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
	p := newTestPlugin(api)

	store[webhookIndexKey("hook1")] = recordJSON(t, webhookIndexEntry{
		HookID: "hook1", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", RequesterID: "u_req", OwnerUserID: "u_req",
	})
	_, err := p.submitWebhookTransferRequest("u_req", webhookTransferInput{HookID: "hook1", Action: transferActionReassign})
	require.ErrorContains(t, err, "Retire & recreate")
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
			require.Equal(t, raw, store[key], "the ticket is put back for a retry")
			require.True(t, res.audited)
		})
	}
}

func TestWebhookTransferAuditSnapshot_HidesIncomingHookID(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	var got []auditEntry
	p.auditSink = func(e auditEntry, _ auditEventSpec) { got = append(got, e) }

	incoming := &webhookTransferRequest{ID: "r1", HookID: "secret-hook-id", TransferHandle: "handle1", WebhookType: webhookTypeIncoming}
	outgoing := &webhookTransferRequest{ID: "r2", HookID: "out-hook-id", WebhookType: webhookTypeOutgoing}
	p.recordEvent(auditEntry{RequestID: "r1", Kind: pendingKindWebhookTransfer, Event: eventApproved, Snapshot: incoming})
	p.recordEvent(auditEntry{RequestID: "r2", Kind: pendingKindWebhookTransfer, Event: eventApproved, Snapshot: outgoing})
	require.Len(t, got, 2)

	in, err := json.Marshal(got[0].Snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(in), "secret-hook-id")
	require.Contains(t, string(in), `"transfer_handle":"handle1"`)
	require.Equal(t, "secret-hook-id", incoming.HookID, "the live request keeps its hook ID")

	out, err := json.Marshal(got[1].Snapshot)
	require.NoError(t, err)
	require.Contains(t, string(out), "out-hook-id", "outgoing hook IDs aren't secret")
}

func TestWebhookTransferApprovalBlocker_RefusesOrphanedIncomingReassign(t *testing.T) {
	api := &plugintest.API{}
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

func TestRestoreTransferRecord_ClaimCheckErrorKeepsRecord(t *testing.T) {
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
	require.True(t, restored, "record stays so Approve can retry")
	require.Equal(t, raw, store[key])
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

	n, err := p.revokeBotTokens("bot1", "admin1", "")
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

		n, tokenID, tokenValue, _, err := p.rotateBotToken("bot1", "svcbot", "u_new", "admin1", "req-1")
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

		n, _, tokenValue, _, err := p.rotateBotToken("bot1", "svcbot", "u_new", "admin1", "req-1")
		require.Error(t, err)
		require.Zero(t, n)
		require.Empty(t, tokenValue)
		require.Empty(t, *revoked, "a bot with no new token must keep the ones it has")
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
	store[claimKey] = []byte(formatTransferClaim("req-a", 3))

	restored, err := p.restoreTransferRecord(pendingKindBotTransfer, "bot1", "req-a", key, raw)
	require.NoError(t, err)
	require.True(t, restored)
	require.Equal(t, raw, store[key])

	holder, gen := parseTransferClaim(store[claimKey])
	require.Equal(t, "req-a", holder)
	require.Equal(t, int64(4), gen, "a takeover holding the old value must lose its CAS")
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
	store[transferAssetClaimKey(pendingKindWebhookTransfer, assetKey)] = []byte(formatTransferClaim("req-a", 0))
	p.releaseTransferAssetClaim(pendingKindWebhookTransfer, assetKey, "req-a")

	require.NotEmpty(t, logged)
	require.NotContains(t, fmt.Sprintf("%v", logged), "secret-hook-id")
}
