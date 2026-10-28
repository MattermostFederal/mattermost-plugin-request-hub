package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestPendingKinds(t *testing.T) {
	t.Run("no request prefix starts another", func(t *testing.T) {
		for kind, info := range pendingKinds {
			for other, otherInfo := range pendingKinds {
				if kind != other {
					require.False(t, strings.HasPrefix(otherInfo.kvPrefix, info.kvPrefix), "%s prefix starts %s's", kind, other)
				}
			}
		}
	})

	t.Run("each kind owns its stored requests", func(t *testing.T) {
		for kind, info := range pendingKinds {
			got, ok := pendingKindForKey(info.kvPrefix + "r1")
			require.True(t, ok, kind)
			require.Equal(t, kind, got)

			record := info.newRecord()
			id, requesterID, createdAt := record.pendingFields()
			require.Empty(t, id)
			require.Empty(t, requesterID)
			require.NotNil(t, createdAt, kind)
		}
	})

	t.Run("other keys belong to no kind", func(t *testing.T) {
		for _, key := range []string{pendingListKey(pendingKindTeam, "u1"), kvApprovalCardsPrefix + "r1", kvPendingMigrated} {
			_, ok := pendingKindForKey(key)
			require.False(t, ok, key)
		}
	})
}

func TestPendingKeysForEveryType(t *testing.T) {
	for name, tc := range map[string]struct {
		record storedRequest
		want   string
	}{
		"team":        {&teamCreationRequest{Name: "ops"}, "ops"},
		"team admin":  {&teamAdminRequest{TeamID: "t1", NomineeIDs: []string{"b", "a"}}, "t1/a,b"},
		"team member": {&teamMemberRequest{TeamID: "t1", NomineeIDs: []string{"b", "a"}}, "t1/a,b"},
		"team guest":  {&teamGuestRequest{TeamID: "t1", Email: "Guest@Example.com"}, "t1/guest@example.com"},
		"bot":         {&botRequest{Username: "svcbot"}, "svcbot"},
		"webhook":     {&webhookRequest{WebhookType: webhookTypeIncoming, ChannelID: "ch1", DisplayName: "Build Alerts"}, "incoming/ch1/build alerts"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.record.pendingKey())
		})
	}
}

func TestDuplicateRequestErrorForEveryType(t *testing.T) {
	for kind, tc := range map[string]struct{ key, want string }{
		pendingKindChannel:    {"team1/team-x", "you already have a pending request for ~team-x in this team"},
		pendingKindAdmin:      {"ch1/a", "you already have a pending Channel Admin request for these people in this channel"},
		pendingKindTeam:       {"ops", "you already have a pending request for a team named ops"},
		pendingKindTeamAdmin:  {"t1/a", "you already have a pending Team Admin request for these people in this team"},
		pendingKindTeamMember: {"t1/a", "you already have a pending request to add these people to this team"},
		pendingKindTeamGuest:  {"t1/guest@example.com", "you already have a pending request to invite guest@example.com to this team"},
		pendingKindBot:        {"svcbot", "you already have a pending request for bot @svcbot"},
		pendingKindWebhook:    {"outgoing/ch1/alerts", "you already have a pending outgoing webhook request with this name in this channel"},
	} {
		t.Run(kind, func(t *testing.T) {
			require.EqualError(t, duplicateRequestError(kind, tc.key), tc.want)
		})
	}
	require.Len(t, pendingKinds, 8, "every request type has a duplicate message above")
}

func TestPendingRequestExistsUnknownKind(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	_, err := p.pendingRequestExists("nope", "r1")

	require.EqualError(t, err, `unknown pending request kind "nope"`)
}

// typedSubmit is one of the newer request types' submit calls, with the
// stubs that let its input checks pass.
type typedSubmit struct {
	name        string
	kind        string
	prefix      string
	requesterID string
	key         string // the pending key the submit produces
	setup       func(api *plugintest.API)
	submit      func(p *Plugin) (string, error)
}

func typedSubmits() []typedSubmit {
	var out []typedSubmit
	teamKinds := map[string]struct{ kind, requesterID, key string }{
		"team member":   {pendingKindTeamMember, "u_req", "team1/" + testNomineeID},
		"team guest":    {pendingKindTeamGuest, testSysAdminID, "team1/guest@example.com"},
		"team admin":    {pendingKindTeamAdmin, testSysAdminID, "team1/" + testNomineeID},
		"team creation": {pendingKindTeam, "u_req", "marketing"},
	}
	for _, tc := range teamSubmits() {
		k := teamKinds[tc.name]
		out = append(out, typedSubmit{
			name: tc.name, kind: k.kind, prefix: tc.prefix, requesterID: k.requesterID, key: k.key,
			setup: tc.setup, submit: tc.submit,
		})
	}
	return append(out,
		typedSubmit{
			name: "bot", kind: pendingKindBot, prefix: kvBotRequestPrefix, requesterID: "u_req", key: "ci-bot",
			setup: stubRequester,
			submit: func(p *Plugin) (string, error) {
				return p.submitBotRequest("u_req", botRequestInput{Username: "ci-bot", DisplayName: "CI Bot"})
			},
		},
		typedSubmit{
			name: "webhook", kind: pendingKindWebhook, prefix: kvWebhookRequestPrefix, requesterID: "u_req", key: "incoming/ch1/alerts",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "u_req").Return(regularUser(), nil)
				api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team-id", Name: "alerts", Type: model.ChannelTypeOpen}, nil)
				api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
			},
			submit: func(p *Plugin) (string, error) {
				return p.submitWebhookRequest("u_req", webhookTypeIncoming, " ch1 ", "Alerts", "", "")
			},
		},
	)
}

func TestEveryRequestTypeSubmitChecksPendingList(t *testing.T) {
	require.Len(t, typedSubmits(), 6)
	for _, tc := range typedSubmits() {
		// newSubmit returns a plugin limited to one pending request per type,
		// whose requester already has old pending under key.
		newSubmit := func(t *testing.T, key string) (*plugintest.API, *Plugin, map[string][]byte) {
			api := &plugintest.API{}
			lists := fakePendingLists(api)
			stubLogs(api)
			p := newTestPlugin(api)
			config := *p.getConfiguration()
			config.maxPending = 1
			config.expiryDays = 30
			p.setConfiguration(&config)
			tc.setup(api)
			lists[pendingListKey(tc.kind, tc.requesterID)] = entriesJSON(t, pendingEntry{ID: "old", CreatedAt: model.GetMillis(), Key: key})
			return api, p, lists
		}

		t.Run(tc.name+"/duplicate", func(t *testing.T) {
			api, p, _ := newSubmit(t, tc.key)
			storedRequests(api, tc.prefix, "old")

			_, err := tc.submit(p)

			require.EqualError(t, err, duplicateRequestError(tc.kind, tc.key).Error())
			api.AssertNotCalled(t, "KVSet", keyWithPrefix(tc.prefix), mock.Anything)
		})

		t.Run(tc.name+"/at the limit", func(t *testing.T) {
			api, p, _ := newSubmit(t, "something else")
			storedRequests(api, tc.prefix, "old")

			_, err := tc.submit(p)

			require.EqualError(t, err, "you have 1 pending requests waiting for review; you can submit more once some are approved, denied or expire")
			api.AssertNotCalled(t, "KVSet", keyWithPrefix(tc.prefix), mock.Anything)
		})

		t.Run(tc.name+"/a failed store frees the slot", func(t *testing.T) {
			api, p, lists := newSubmit(t, "something else")
			// The old request was handled, so its entry no longer counts.
			storedRequests(api, tc.prefix)
			api.On("KVSet", keyWithPrefix(tc.prefix), mock.Anything).Return(testAppErr("kv down"))

			_, err := tc.submit(p)

			require.EqualError(t, err, "could not save your request; try again")
			api.AssertCalled(t, "KVCompareAndSet", pendingListKey(tc.kind, tc.requesterID), mock.Anything, mock.MatchedBy(func(data []byte) bool {
				return strings.Contains(string(data), `"key":"`+tc.key+`"`)
			}))
			require.Empty(t, lists, "the reserved entry was released, and the handled one dropped")
		})
	}
}

func TestEveryRequestTypeExpires(t *testing.T) {
	kinds := map[string]string{
		"team admin":  pendingKindTeamAdmin,
		"team member": pendingKindTeamMember,
		"team guest":  pendingKindTeamGuest,
		"team":        pendingKindTeam,
		"bot":         pendingKindBot,
		"webhook":     pendingKindWebhook,
	}
	for _, tc := range actionCasesNamed("team admin", "team member", "team guest", "team", "bot", "webhook") {
		for _, approve := range []bool{true, false} {
			name := tc.name + "/deny"
			if approve {
				name = tc.name + "/approve"
			}
			t.Run(name, func(t *testing.T) {
				api := &plugintest.API{}
				lists := fakePendingLists(api)
				stubLogs(api)
				p := newTestPlugin(api)
				config := *p.getConfiguration()
				config.expiryDays = 30
				p.setConfiguration(&config)

				record := tc.record("post1").(storedRequest)
				_, _, createdAt := record.pendingFields()
				*createdAt = expiredMillis()
				if two, ok := record.(*botRequest); ok {
					// Half-approved requests expire too.
					two.twoStepState = securityStepDone()
				}
				raw := mustMarshal(t, record)
				lists[pendingListKey(kinds[tc.name], "u_req")] = entriesJSON(t, newPendingEntry(record))

				stubSystemAdmin(api)
				// Every approve-time check would fail too; expiry comes first.
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req", DeleteAt: 1}, nil).Maybe()
				api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops", DeleteAt: 1}, nil).Maybe()
				api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", DeleteAt: 1}, nil).Maybe()
				api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
				api.On("KVCompareAndDelete", tc.kvPrefix+"r1", raw).Return(true, nil).Once()
				stubTicketClose(api)

				resp := runAction(t, p, tc, approve)

				require.Equal(t, expiredText, resp.EphemeralText)
				require.NotNil(t, resp.Update)
				require.Contains(t, resp.Update.Message, "Expired: not reviewed within 30 days.")
				require.Empty(t, lists, "the pending entry was released")
				for _, effect := range []string{"CreateTeam", "CreateBot", "UpdateTeamMemberRoles", "CreateTeamMember"} {
					api.AssertNotCalled(t, effect, mock.Anything)
				}
			})
		}
	}
}

func TestExpiredTwoStepRequestNeedsAnApprover(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	config := *p.getConfiguration()
	config.expiryDays = 30
	p.setConfiguration(&config)
	record := pendingBotRequest(twoStepState{})
	record.CreatedAt = expiredMillis()
	raw := mustMarshal(t, record)
	api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Username: "other"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvBotRequestPrefix+"r1").Return(raw, nil)
	api.On("GetPost", "post1").Return(requestCardPost(), nil)

	resp := clickAction(t, p, botActionHandler, "u_other", actionBody(t, "r1"), false)

	require.Equal(t, "You don't have permission to deny this request.", resp.EphemeralText)
	api.AssertNotCalled(t, "KVCompareAndDelete", kvBotRequestPrefix+"r1", mock.Anything)
}

func TestCreateFailureKeepsPendingEntry(t *testing.T) {
	api := &plugintest.API{}
	lists := fakePendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	record := &teamCreationRequest{ID: "r1", RequesterID: "u_req", DisplayName: "Ops", Name: "ops", ApprovalPostID: "post1", CreatedAt: model.GetMillis()}
	raw := mustMarshal(t, record)
	entry := entriesJSON(t, newPendingEntry(record))
	lists[pendingListKey(pendingKindTeam, "u_req")] = entry
	stubSystemAdmin(api)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvTeamRequestPrefix+"r1").Return(raw, nil)
	api.On("GetPost", "post1").Return(requestCardPost(), nil)
	api.On("KVCompareAndDelete", kvTeamRequestPrefix+"r1", raw).Return(true, nil)
	api.On("CreateTeam", mock.Anything).Return(nil, testAppErr("name taken"))
	api.On("KVSet", kvTeamRequestPrefix+"r1", mock.Anything).Return(nil)

	resp := runAsAdmin(t, p, teamActionHandler, true)

	require.Contains(t, resp.EphemeralText, "The request is still pending.")
	require.Equal(t, entry, lists[pendingListKey(pendingKindTeam, "u_req")])
}

func TestMigratePendingRequestsEveryType(t *testing.T) {
	u1 := model.NewId()
	records := map[string]storedRequest{
		kvTeamRequestPrefix:       &teamCreationRequest{Name: "ops"},
		kvTeamAdminRequestPrefix:  &teamAdminRequest{TeamID: "t1", NomineeIDs: []string{"n1"}},
		kvTeamMemberRequestPrefix: &teamMemberRequest{TeamID: "t1", NomineeIDs: []string{"n1"}},
		kvTeamGuestRequestPrefix:  &teamGuestRequest{TeamID: "t1", Email: "g@example.com"},
		kvBotRequestPrefix:        &botRequest{Username: "svcbot", twoStepState: securityStepDone()},
		kvWebhookRequestPrefix:    &webhookRequest{WebhookType: webhookTypeIncoming, ChannelID: "ch1", DisplayName: "Alerts"},
	}

	for _, previous := range []string{"", "migrated_pending_v1"} {
		t.Run("after "+previous, func(t *testing.T) {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			store := fakeKV(api)
			if previous != "" {
				store[previous] = []byte(migrationDone)
			}
			ids := map[string]string{}
			for prefix, record := range records {
				id := model.NewId()
				ids[prefix] = id
				raw := mustMarshal(t, record)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(raw, &fields))
				fields["id"], fields["requester_id"] = id, u1
				store[prefix+id] = mustMarshal(t, fields)
			}

			require.NoError(t, p.migratePendingRequests())

			for prefix, record := range records {
				kind, _ := pendingKindForKey(prefix)
				migrated := pendingKinds[kind].newRecord()
				require.NoError(t, json.Unmarshal(store[prefix+ids[prefix]], migrated))
				_, _, createdAt := migrated.pendingFields()
				require.Positive(t, *createdAt, kind)

				entries := decodeEntries(t, store[pendingListKey(kind, u1)])
				require.Len(t, entries, 1, kind)
				require.Equal(t, ids[prefix], entries[0].ID)
				require.Equal(t, record.pendingKey(), entries[0].Key)
			}
			var bot botRequest
			require.NoError(t, json.Unmarshal(store[kvBotRequestPrefix+ids[kvBotRequestPrefix]], &bot))
			require.Equal(t, securityStepDone(), bot.twoStepState, "the rest of the record is kept")
			require.Equal(t, migrationDone, string(store[kvPendingMigrated]))
		})
	}
}

func TestBackfillRequestRefusesOtherKeys(t *testing.T) {
	_, err := backfillRequest(kvApprovalCardsPrefix+"r1", []byte("{}"), 10)

	require.EqualError(t, err, "not a stored request")
}
