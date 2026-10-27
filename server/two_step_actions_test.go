package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// stubRelayLookups lets the ticket relay lookups and the approval card record be
// written and cleared, without the blanket KVCompareAndSet stubLogs adds.
func stubRelayLookups(api *plugintest.API) {
	stubLogLevels(api)
	api.On("KVSet", mock.MatchedBy(func(key string) bool {
		return strings.HasPrefix(key, kvApprovalPostPrefix) || strings.HasPrefix(key, kvDMRootPrefix)
	}), mock.Anything).Return(nil).Maybe()
	api.On("KVDelete", mock.MatchedBy(func(key string) bool {
		return strings.HasPrefix(key, kvApprovalCardsPrefix)
	})).Return(nil).Maybe()
}

// securityStepDone is the state where only the system step remains.
func securityStepDone() twoStepState {
	return twoStepState{SecurityApproverID: "sec1", SecurityApprovedAt: 1}
}

func pendingBotRequest(state twoStepState) *botRequest {
	return &botRequest{ID: "r1", RequesterID: "u_req", Username: "svcbot", ApprovalPostID: "post1", twoStepState: state}
}

func pendingWebhookRequest(state twoStepState) *webhookRequest {
	return &webhookRequest{
		ID: "r1", RequesterID: "u_req", WebhookType: webhookTypeIncoming, ChannelID: "ch1", DisplayName: "Alerts",
		ApprovalPostID: "post1", twoStepState: state,
	}
}

// userGroupConfig requires a security group admin1 is not in.
func userGroupConfig(api *plugintest.API) *configuration {
	stubSyncedGroup(api, "security-approvers", "g-sec")
	api.On("GetGroupsForUser", "admin1").Return([]*model.Group{}, nil)
	return &configuration{
		AllowBotTokenRequests: true,
		AllowWebhookRequests:  true,
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
	}
}

// extrasReply captures DM thread replies under "dm-root".
func extrasReply(api *plugintest.API) *[]string {
	var replies []string
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		if post := args.Get(0).(*model.Post); post.RootId == "dm-root" {
			replies = append(replies, post.Message)
		}
	}).Return(&model.Post{Id: "created"}, nil)
	return &replies
}

func TestTwoStepActionHandlers_UnidentifiedRequestAsksToRefresh(t *testing.T) {
	for name, h := range map[string]actionHandler{"bot": botActionHandler, "webhook": webhookActionHandler} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			stubSystemAdmin(api)
			body, err := json.Marshal(model.PostActionIntegrationRequest{})
			require.NoError(t, err)

			resp := clickAction(t, p, h, "admin1", string(body), true)

			// Refused before the request is read: the mock has no KVGet.
			require.Contains(t, resp.EphemeralText, "Could not identify that request")
		})
	}
}

func TestTwoStepActionHandlers_FinalClaimErrorChangesNothing(t *testing.T) {
	cases := []struct {
		name     string
		handler  actionHandler
		kvPrefix string
		record   any
		// guard lets the final step's result be created; check proves it wasn't.
		guard func(t *testing.T, api *plugintest.API, p *Plugin)
		check func(t *testing.T, api *plugintest.API)
	}{
		{
			name: "bot", handler: botActionHandler, kvPrefix: kvBotRequestPrefix, record: pendingBotRequest(securityStepDone()),
			guard: func(_ *testing.T, api *plugintest.API, _ *Plugin) {
				api.On("CreateBot", mock.Anything).Return(&model.Bot{UserId: "new-bot-id"}, nil).Maybe()
			},
			check: func(t *testing.T, api *plugintest.API) { api.AssertNotCalled(t, "CreateBot", mock.Anything) },
		},
		{
			name: "webhook", handler: webhookActionHandler, kvPrefix: kvWebhookRequestPrefix, record: pendingWebhookRequest(securityStepDone()),
			guard: func(t *testing.T, api *plugintest.API, p *Plugin) {
				api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil).Maybe()
				failWebhookCreation(t, p)
			},
			check: func(t *testing.T, api *plugintest.API) { api.AssertNotCalled(t, "CreateSession", mock.Anything) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			p := newTestPlugin(api)
			raw := mustMarshal(t, tc.record)
			stubSystemAdmin(api)
			stubRequester(api)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil).Maybe()
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
			api.On("KVCompareAndDelete", tc.kvPrefix+"r1", raw).Return(false, testAppErr("kv down"))
			tc.guard(t, api, p)

			resp := runAsAdmin(t, p, tc.handler, true)

			require.Equal(t, "Could not process that request.", resp.EphemeralText)
			require.Nil(t, resp.Update)
			tc.check(t, api)
		})
	}
}

func TestTwoStepActionHandlers_RequesterApproveIsShownOnCard(t *testing.T) {
	cases := []struct {
		name     string
		handler  actionHandler
		kvPrefix string
		record   any
		header   string
		stub     func(api *plugintest.API)
	}{
		{
			name: "bot", handler: botActionHandler, kvPrefix: kvBotRequestPrefix,
			record: &botRequest{ID: "r1", RequesterID: "admin1", Username: "svcbot", ApprovalPostID: "post1"},
			header: (&Plugin{}).botApprovalHeader(),
			stub:   func(*plugintest.API) {},
		},
		{
			name: "webhook", handler: webhookActionHandler, kvPrefix: kvWebhookRequestPrefix,
			record: &webhookRequest{ID: "r1", RequesterID: "admin1", WebhookType: webhookTypeIncoming, ChannelID: "ch1", ApprovalPostID: "post1"},
			header: (&Plugin{}).webhookApprovalHeader(),
			stub: func(api *plugintest.API) {
				// The target channel is gone; the card still renders.
				api.On("GetChannel", "ch1").Return(nil, testAppErr("gone"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubRelayLookups(api)
			p := newTestPlugin(api)
			stubSystemAdmin(api)
			tc.stub(api)
			api.On("KVGet", tc.kvPrefix+"r1").Return(mustMarshal(t, tc.record), nil)
			api.On("GetPost", "post1").Return(approvalCard(), nil)
			api.On("SendEphemeralPost", "admin1", mock.MatchedBy(func(post *model.Post) bool {
				return post.ChannelId == "appr-ch"
			})).Return(&model.Post{}).Once()

			resp := runAsAdmin(t, p, tc.handler, true)

			require.Contains(t, resp.EphemeralText, "you cannot approve it")
			require.NotNil(t, resp.Update)
			require.Equal(t, "post1", resp.Update.Id)
			require.Equal(t, tc.header, resp.Update.Message)
			att := extractFirstAttachment(resp.Update)
			require.NotNil(t, att)
			require.NotEmpty(t, att.Fields)
			require.Equal(t, "🚫 Requester cannot approve:", att.Fields[0].Title)
			require.Contains(t, att.Fields[0].Value, "**@admin**")
			api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
			api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
			api.AssertNotCalled(t, "UpdatePost", mock.Anything)
		})
	}
}

func TestTwoStepActionHandlers_SystemApproverBeforeSecurityIsRefused(t *testing.T) {
	cases := []struct {
		name     string
		handler  actionHandler
		kvPrefix string
		record   any
	}{
		{"bot", botActionHandler, kvBotRequestPrefix, pendingBotRequest(twoStepState{})},
		{"webhook", webhookActionHandler, kvWebhookRequestPrefix, pendingWebhookRequest(twoStepState{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubRelayLookups(api)
			p := newTestPlugin(api)
			p.setConfiguration(userGroupConfig(api))
			stubSystemAdmin(api)
			// Requester, target channel and card can't be loaded.
			api.On("GetUser", "u_req").Return(nil, testAppErr("gone"))
			api.On("GetChannel", "ch1").Return(nil, testAppErr("gone")).Maybe()
			api.On("KVGet", tc.kvPrefix+"r1").Return(mustMarshal(t, tc.record), nil)
			api.On("GetPost", "post1").Return(nil, testAppErr("gone"))

			resp := runAsAdmin(t, p, tc.handler, true)

			require.Contains(t, resp.EphemeralText, "Security review required first")
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
			api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
			api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
		})
	}
}

func TestTwoStepActionHandlers_SecondClickBySameApproverIsEphemeralOnly(t *testing.T) {
	state := twoStepState{SecurityApproverID: "admin1", SecurityApprovedAt: 1}
	cases := []struct {
		name     string
		handler  actionHandler
		kvPrefix string
		record   any
	}{
		{"bot", botActionHandler, kvBotRequestPrefix, pendingBotRequest(state)},
		{"webhook", webhookActionHandler, kvWebhookRequestPrefix, pendingWebhookRequest(state)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubRelayLookups(api)
			p := newTestPlugin(api)
			stubSystemAdmin(api)
			stubRequester(api)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil).Maybe()
			api.On("KVGet", tc.kvPrefix+"r1").Return(mustMarshal(t, tc.record), nil)
			api.On("GetPost", "post1").Return(approvalCard(), nil)
			api.On("SendEphemeralPost", "admin1", mock.Anything).Return(&model.Post{}).Once()

			resp := runAsAdmin(t, p, tc.handler, true)

			require.Equal(t, "⚠️ You've already approved your step. This request needs a different approver to complete it.", resp.EphemeralText)
			require.Nil(t, resp.Update)
			api.AssertNumberOfCalls(t, "SendEphemeralPost", 1)
			api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestTwoStepActionHandlers_PartialApprovalLostToConcurrentUpdate(t *testing.T) {
	cases := []struct {
		name     string
		handler  actionHandler
		kvPrefix string
		record   any
	}{
		{"bot", botActionHandler, kvBotRequestPrefix, pendingBotRequest(twoStepState{})},
		{"webhook", webhookActionHandler, kvWebhookRequestPrefix, pendingWebhookRequest(twoStepState{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubRelayLookups(api)
			p := newTestPlugin(api)
			raw := mustMarshal(t, tc.record)
			stubSystemAdmin(api)
			stubRequester(api)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil).Maybe()
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
			api.On("KVCompareAndSet", tc.kvPrefix+"r1", raw, mock.Anything).Return(false, nil).Once()

			resp := runAsAdmin(t, p, tc.handler, true)

			require.Equal(t, "This request was just updated by someone else. Please try again.", resp.EphemeralText)
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "CreateBot", mock.Anything)
			api.AssertNotCalled(t, "GetPost", mock.Anything)
		})
	}
}

func TestTwoStepActionHandlers_PartialApprovalRecordsStepAndRepaintsCard(t *testing.T) {
	cases := []struct {
		name     string
		handler  actionHandler
		kvPrefix string
		record   any
		header   string
	}{
		{"bot", botActionHandler, kvBotRequestPrefix, pendingBotRequest(twoStepState{}), (&Plugin{}).botApprovalHeader()},
		{"webhook", webhookActionHandler, kvWebhookRequestPrefix, pendingWebhookRequest(twoStepState{}), (&Plugin{}).webhookApprovalHeader()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubRelayLookups(api)
			p := newTestPlugin(api)
			raw := mustMarshal(t, tc.record)
			stubSystemAdmin(api)
			// Requester and target channel are gone; the card falls back to IDs.
			api.On("GetUser", "u_req").Return(nil, testAppErr("gone"))
			api.On("GetChannel", "ch1").Return(nil, testAppErr("gone")).Maybe()
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
			var stored []byte
			api.On("KVCompareAndSet", tc.kvPrefix+"r1", raw, mock.Anything).Run(func(args mock.Arguments) {
				stored = args.Get(2).([]byte)
			}).Return(true, nil).Once()
			// Progress card for the requester, then the repainted approval card.
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
			api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "progress"}, nil)
			api.On("GetPost", "post1").Return(approvalCard(), nil)

			resp := runAsAdmin(t, p, tc.handler, true)

			require.Empty(t, resp.EphemeralText)
			require.NotNil(t, resp.Update)
			require.Equal(t, "post1", resp.Update.Id)
			require.Equal(t, tc.header, resp.Update.Message)
			att := extractFirstAttachment(resp.Update)
			require.NotNil(t, att)
			require.NotEmpty(t, att.Fields)
			require.Equal(t, "@user u_req", att.Fields[0].Value)
			var state twoStepState
			require.NoError(t, json.Unmarshal(stored, &state))
			require.Equal(t, "admin1", state.SecurityApproverID)
			require.Empty(t, state.SystemApproverID)
			api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
			api.AssertNotCalled(t, "CreateBot", mock.Anything)
		})
	}
}

func TestHandleBotAction_DenyClosesTicket(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	raw := mustMarshal(t, pendingBotRequest(twoStepState{}))
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("KVGet", kvBotRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvBotRequestPrefix+"r1", raw).Return(true, nil).Once()
	stubTicketClose(api)

	resp := runAsAdmin(t, p, botActionHandler, false)

	require.NotNil(t, resp.Update)
	require.Equal(t, "post1", resp.Update.Id)
	require.Contains(t, resp.Update.Message, "Denied by @admin for @reqer. Bot **@svcbot**.")
	api.AssertCalled(t, "KVCompareAndDelete", kvBotRequestPrefix+"r1", raw)
	api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
	api.AssertNotCalled(t, "CreateBot", mock.Anything)
}

func TestHandleBotAction_CreateFailureRestoresAndRepaintsCard(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	raw := mustMarshal(t, pendingBotRequest(securityStepDone()))
	stubSystemAdmin(api)
	api.On("GetUser", "u_req").Return(nil, testAppErr("gone"))
	api.On("GetUser", "sec1").Return(&model.User{Id: "sec1", Username: "sec"}, nil)
	api.On("KVGet", kvBotRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvBotRequestPrefix+"r1", raw).Return(true, nil).Once()
	api.On("CreateBot", mock.Anything).Return(nil, testAppErr("name taken"))
	api.On("KVSet", kvBotRequestPrefix+"r1", raw).Return(nil).Once()
	api.On("GetPost", "post1").Return(approvalCard(), nil)

	resp := runAsAdmin(t, p, botActionHandler, true)

	require.Contains(t, resp.EphemeralText, "Couldn't create the bot")
	require.Contains(t, resp.EphemeralText, "still pending")
	require.NotNil(t, resp.Update)
	require.Equal(t, "post1", resp.Update.Id)
	require.Contains(t, resp.Update.Message, "a previous approval attempt failed")
	api.AssertCalled(t, "KVSet", kvBotRequestPrefix+"r1", raw)
	api.AssertNotCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}

// finalBot registers admin1's final Approve up to creating the bot, for a
// request with a DM ticket thread unless the request clears it.
func finalBot(t *testing.T, api *plugintest.API, req *botRequest) {
	t.Helper()
	raw := mustMarshal(t, req)
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("KVGet", kvBotRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvBotRequestPrefix+"r1", raw).Return(true, nil).Once()
	api.On("CreateBot", mock.MatchedBy(func(bot *model.Bot) bool {
		return bot.Username == "svcbot" && bot.OwnerId == "u_req"
	})).Return(&model.Bot{UserId: "new-bot-id", Username: "svcbot"}, nil).Once()
	api.On("GetPost", "post1").Return(approvalCard(), nil)
	api.On("GetPost", "dm-root").Return(nil, testAppErr("no root")).Maybe()
}

func TestHandleBotAction_ExtrasSkippedWithoutSessionOrSiteURL(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("GetConfig").Return(&model.Config{})
	finalBot(t, api, finalBotApproval(botRequest{RequestToken: true, IncomingWebhookChannelID: "ch1", DMChannelID: "dm1", DMRootPostID: "dm-root"}))
	api.On("CreateSession", mock.Anything).Return(nil, testAppErr("no session"))
	replies := extrasReply(api)

	resp := runAsAdmin(t, p, botActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Fully approved (security + system) for @reqer. Bot **@svcbot** created.")
	all := strings.Join(*replies, "\n")
	require.Contains(t, all, "token/webhook extras were skipped because an admin session could not be created")
	require.Contains(t, all, "webhook extras were skipped because SiteURL is not configured")
	api.AssertNotCalled(t, "RevokeSession", mock.Anything)
	api.AssertNotCalled(t, "GetChannel", "ch1")
}

func TestHandleBotAction_ExtrasFailuresAreReported(t *testing.T) {
	capture := &hookCapture{status: http.StatusForbidden}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	finalBot(t, api, finalBotApproval(botRequest{
		RequestToken: true, IncomingWebhookChannelID: "ch1",
		OutgoingWebhookChannelID: "ch2", OutgoingWebhookCallbackURL: "https://hooks.example.com/out",
		DMChannelID: "dm1", DMRootPostID: "dm-root",
	}))
	api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
	api.On("RevokeSession", "sess1").Return(testAppErr("revoke failed")).Once()
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
	api.On("GetChannel", "ch2").Return(&model.Channel{Id: "ch2", TeamId: "team1"}, nil)
	api.On("GetTeamMember", "team1", "new-bot-id").Return(&model.TeamMember{}, nil)
	api.On("AddChannelMember", "ch1", "new-bot-id").Return(&model.ChannelMember{}, nil)
	replies := extrasReply(api)

	resp := runAsAdmin(t, p, botActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Bot **@svcbot** created.")
	all := strings.Join(*replies, "\n")
	require.Contains(t, all, "token generation failed")
	require.Contains(t, all, "incoming webhook creation failed: create incoming webhook")
	require.Contains(t, all, "outgoing webhook creation failed")
	require.NotContains(t, all, "Access Token")
	api.AssertCalled(t, "RevokeSession", "sess1")
	api.AssertNotCalled(t, "PermanentDeleteBot", mock.Anything)
}

func TestHandleBotAction_TokenDeliveredAfterReopeningDM(t *testing.T) {
	capture := &hookCapture{}
	ts := capture.server(t)
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)
	finalBot(t, api, finalBotApproval(botRequest{RequestToken: true}))
	api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
	api.On("RevokeSession", "sess1").Return(nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm9"}, nil)
	api.On("KVGet", tokenPostRegistryKey).Return(nil, nil)
	var tokenReply string
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		if post := args.Get(0).(*model.Post); post.ChannelId == "dm9" && strings.Contains(post.Message, "Access Token") {
			tokenReply = post.Message
		}
	}).Return(&model.Post{Id: "created"}, nil)

	resp := runAsAdmin(t, p, botActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Bot **@svcbot** created.")
	require.Contains(t, tokenReply, "tok-value")
	api.AssertCalled(t, "KVCompareAndDelete", kvBotRequestPrefix+"r1", mock.Anything)
	api.AssertNotCalled(t, "PermanentDeleteBot", mock.Anything)
}

// incomingHookServer answers the incoming webhook REST call with hookID.
func incomingHookServer(t *testing.T, hookID string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/hooks/incoming" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(model.IncomingWebhook{Id: hookID})
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestHandleBotAction_UndeliverableIncomingWebhookIsRemoved(t *testing.T) {
	cases := []struct {
		name      string
		hookID    string
		deleteErr error
		want      string
	}{
		{name: "removed", hookID: "in-hook-id", want: "incoming webhook URL could not be delivered, so the hook was removed"},
		{name: "remove fails", hookID: "in-hook-id", deleteErr: errors.New("delete failed"), want: "the hook could not be removed automatically"},
		{name: "no hook id", want: "incoming webhook URL could not be delivered (no DM channel)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := incomingHookServer(t, tc.hookID)
			api := &plugintest.API{}
			stubLogs(api)
			p := newTestPlugin(api)
			stubSiteURL(api, ts.URL)
			var deleted []string
			p.deleteWebhookFn = func(webhookType, hookID string) error {
				deleted = append(deleted, webhookType+":"+hookID)
				return tc.deleteErr
			}
			finalBot(t, api, finalBotApproval(botRequest{IncomingWebhookChannelID: "ch1"}))
			api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
			api.On("RevokeSession", "sess1").Return(nil)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
			api.On("GetTeamMember", "team1", "new-bot-id").Return(&model.TeamMember{}, nil)
			api.On("AddChannelMember", "ch1", "new-bot-id").Return(&model.ChannelMember{}, nil)
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(nil, testAppErr("no dm"))
			var notes []string
			api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
				notes = append(notes, args.Get(0).(*model.Post).Message)
			}).Return(&model.Post{Id: "created"}, nil)

			resp := runAsAdmin(t, p, botActionHandler, true)

			require.NotNil(t, resp.Update)
			require.Contains(t, resp.Update.Message, "Bot **@svcbot** created.")
			all := strings.Join(notes, "\n")
			require.Contains(t, all, tc.want)
			require.NotContains(t, all, "Incoming webhook URL:")
			if tc.hookID == "" {
				require.Empty(t, deleted)
			} else {
				require.Equal(t, []string{webhookTypeIncoming + ":in-hook-id"}, deleted)
			}
		})
	}
}

// finalWebhook registers admin1's final Approve on a webhook request up to
// the claim. createWebhookForRequest reads the server config first.
func finalWebhook(t *testing.T, api *plugintest.API, req *webhookRequest) []byte {
	t.Helper()
	raw := mustMarshal(t, req)
	stubSiteURL(api, "http://localhost:8065")
	stubSystemAdmin(api)
	api.On("KVGet", kvWebhookRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvWebhookRequestPrefix+"r1", raw).Return(true, nil).Once()
	return raw
}

func TestHandleWebhookAction_DenyClosesTicket(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	created := false
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) { created = true; return "", "", nil }
	raw := mustMarshal(t, pendingWebhookRequest(twoStepState{}))
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil)
	api.On("KVGet", kvWebhookRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvWebhookRequestPrefix+"r1", raw).Return(true, nil).Once()
	stubTicketClose(api)

	resp := runAsAdmin(t, p, webhookActionHandler, false)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Denied by @admin for @reqer. incoming webhook **Alerts**.")
	require.False(t, created)
	api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}

func TestHandleWebhookAction_ApproveCreatesWebhookAfterReopeningDM(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	var createdFor *webhookRequest
	var createdBy string
	p.createWebhookFn = func(req *webhookRequest, adminUserID string) (string, string, error) {
		createdFor, createdBy = req, adminUserID
		return "Your webhook URL is ready.", "hook1", nil
	}
	p.deleteWebhookFn = func(string, string) error {
		t.Fatal("a delivered webhook must not be deleted")
		return nil
	}
	finalWebhook(t, api, pendingWebhookRequest(securityStepDone()))
	stubRequester(api)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm9"}, nil)
	var dmCard *model.MessageAttachment
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		if post := args.Get(0).(*model.Post); post.ChannelId == "dm9" {
			dmCard = extractFirstAttachment(post)
		}
	}).Return(&model.Post{Id: "created"}, nil)
	api.On("GetPost", "post1").Return(approvalCard(), nil)

	resp := runAsAdmin(t, p, webhookActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Equal(t, "post1", resp.Update.Id)
	require.Contains(t, resp.Update.Message, "Fully approved (security + system) for @reqer. incoming webhook **Alerts** created.")
	require.NotNil(t, createdFor)
	require.Equal(t, "ch1", createdFor.ChannelID)
	require.Equal(t, "admin1", createdBy)
	require.NotNil(t, dmCard)
	require.Contains(t, dmCard.Text, "Your webhook URL is ready.")
	api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}

func TestHandleWebhookAction_CreateFailureRestoresAndRepaintsCard(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) { return "", "", errors.New("hooks are off") }
	raw := finalWebhook(t, api, pendingWebhookRequest(securityStepDone()))
	api.On("GetUser", "u_req").Return(nil, testAppErr("gone"))
	api.On("GetUser", "sec1").Return(&model.User{Id: "sec1", Username: "sec"}, nil)
	api.On("GetChannel", "ch1").Return(nil, testAppErr("gone"))
	api.On("KVSet", kvWebhookRequestPrefix+"r1", raw).Return(nil).Once()
	api.On("GetPost", "post1").Return(approvalCard(), nil)

	resp := runAsAdmin(t, p, webhookActionHandler, true)

	require.Contains(t, resp.EphemeralText, "Couldn't create the webhook: hooks are off.")
	require.NotNil(t, resp.Update)
	require.Equal(t, "post1", resp.Update.Id)
	require.Contains(t, resp.Update.Message, "a previous approval attempt failed")
	att := extractFirstAttachment(resp.Update)
	require.NotNil(t, att)
	require.NotEmpty(t, att.Fields)
	require.Contains(t, att.Fields[0].Value, "hooks are off")
	api.AssertCalled(t, "KVSet", kvWebhookRequestPrefix+"r1", raw)
	api.AssertNotCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}

func TestHandleWebhookAction_UndeliverableURLRemovalFailureAsksAdmin(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) { return "msg", "hook1", nil }
	var deleted string
	p.deleteWebhookFn = func(_ string, hookID string) error {
		deleted = hookID
		return errors.New("delete failed")
	}
	finalWebhook(t, api, pendingWebhookRequest(securityStepDone()))
	stubRequester(api)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(nil, testAppErr("no dm"))
	api.On("GetPost", "post1").Return(approvalCard(), nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "archive"}, nil)

	resp := runAsAdmin(t, p, webhookActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "could not be removed automatically")
	require.Equal(t, "hook1", deleted)
	api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}
