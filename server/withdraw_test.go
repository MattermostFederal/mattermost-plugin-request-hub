package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDecideWithdraw_RequesterClosesPending(t *testing.T) {
	api, p, store := decisionFixture(t, "app1")
	store[kvApprovalCardsPrefix+testChannelRequestID] = recordJSON(t, []string{"app1"})
	api.On("GetPost", "app1").Return(approvalCard("app1"), nil).Maybe()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Eng"}, nil).Maybe()
	updated := updatedPosts(api)

	res := p.decideWithdraw("u_req", testChannelRequestID, "dmroot")

	require.Contains(t, res.Reply, "withdrawn")
	require.Contains(t, res.Outcome, "Withdrawn by @req")
	require.NotContains(t, store, kvRequestPrefix+testChannelRequestID)
	require.NotContains(t, store, kvApprovalCardsPrefix+testChannelRequestID)
	require.NotNil(t, updated["app1"], "approval card must be resolved for admins")
	require.NotNil(t, res.Update, "DM click returns the updated ticket card")
}

func TestDecideWithdraw_AfterSecurityStepStillAllowed(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	t.Cleanup(func() { api.AssertExpectations(t) })
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())
	store := fakeKV(api)

	id := model.NewId()
	req := &botTransferRequest{
		ID: id, RequesterID: "u_req", BotUserID: "bot1", BotUsername: "svcbot",
		Action: transferActionTransfer, NewOwnerUserID: "u_new", Reason: transferReasonVoluntary,
		ApprovalPostID: "app1", DMRootPostID: "dmroot", DMChannelID: "dm1",
		twoStepState: securityStepDone(),
	}
	store[kvBotTransferRequestPrefix+id] = recordJSON(t, req)
	store[kvApprovalCardsPrefix+id] = recordJSON(t, []string{"app1"})
	store[pendingListKey(pendingKindBotTransfer, "u_req")] = recordJSON(t, []pendingEntry{{ID: id, CreatedAt: model.GetMillis(), Key: req.pendingKey()}})
	store[transferAssetClaimKey(pendingKindBotTransfer, req.pendingKey())] = []byte(id)

	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetUser", "u_new").Return(&model.User{Id: "u_new", Username: "newowner"}, nil).Maybe()
	api.On("GetPost", "app1").Return(approvalCard("app1"), nil).Maybe()
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
	updatedPosts(api)

	res := p.decideWithdraw("u_req", id, "")

	require.Contains(t, res.Reply, "withdrawn")
	require.NotContains(t, store, kvBotTransferRequestPrefix+id)
	require.NotContains(t, store, transferAssetClaimKey(pendingKindBotTransfer, req.pendingKey()))
}

func TestDecideWithdraw_OnlyRequester(t *testing.T) {
	api, p, _ := decisionFixture(t, "app1")
	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)

	res := p.decideWithdraw("admin1", testChannelRequestID, "")

	require.Contains(t, res.Reply, "Only the person who submitted")
	assertTicketNotClaimed(t, api)
}

func TestDecideWithdraw_WrongCardRefused(t *testing.T) {
	api, p, _ := decisionFixture(t, "app1")

	res := p.decideWithdraw("u_req", testChannelRequestID, "someone-elses-post")

	require.Equal(t, buttonNotOnCardText, res.Reply)
	assertTicketNotClaimed(t, api)
}

// assertTicketNotClaimed checks a refused withdraw never took the ticket's
// decision claim: taking it and rolling back would still race a real
// decision running at the same moment.
func assertTicketNotClaimed(t *testing.T, api *plugintest.API) {
	t.Helper()
	api.AssertNotCalled(t, "KVCompareAndDelete", kvRequestPrefix+testChannelRequestID, mock.Anything)
}

func TestDecideWithdraw_CardClickNeedsADMRoot(t *testing.T) {
	api, p, store := decisionFixture(t, "app1")

	// A ticket with no recorded DM root has no Withdraw button, so a click
	// claiming to come from one is spoofed.
	var req channelRequest
	require.NoError(t, json.Unmarshal(store[kvRequestPrefix+testChannelRequestID], &req))
	req.DMRootPostID = ""
	store[kvRequestPrefix+testChannelRequestID] = recordJSON(t, &req)
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()

	res := p.decideWithdraw("u_req", testChannelRequestID, "someone-elses-post")

	require.Equal(t, buttonNotOnCardText, res.Reply)
	require.Contains(t, store, kvRequestPrefix+testChannelRequestID, "a refused click leaves the ticket alone")
	assertTicketNotClaimed(t, api)
}

func TestDecideWithdraw_LostClaimKeepsTheTicket(t *testing.T) {
	cases := []struct {
		name     string
		casErr   *model.AppError
		casOK    bool
		wantHint string
	}{
		{name: "another decision claimed it first", casOK: false, wantHint: "just updated"},
		{name: "the store could not be reached", casErr: testAppErr("kv down"), wantHint: "Could not process"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLines(api)
			t.Cleanup(func() { api.AssertExpectations(t) })
			p := newTestPlugin(api)
			p.setConfiguration(approvableConfig())

			id := model.NewId()
			req := &botTransferRequest{
				ID: id, RequesterID: "u_req", BotUserID: "bot1", BotUsername: "svcbot",
				Action: transferActionTransfer, NewOwnerUserID: "u_new", Reason: transferReasonVoluntary,
				ApprovalPostID: "app1", DMRootPostID: "dmroot", DMChannelID: "dm1",
			}
			key := kvBotTransferRequestPrefix + id
			claimKey := transferAssetClaimKey(pendingKindBotTransfer, req.pendingKey())
			// Registered before fakeKV so this claim attempt is the one that fails.
			api.On("KVCompareAndDelete", key, mock.Anything).Return(tc.casOK, tc.casErr)
			store := fakeKV(api)
			store[key] = recordJSON(t, req)
			store[claimKey] = []byte(id)
			store[pendingListKey(pendingKindBotTransfer, "u_req")] = recordJSON(t, []pendingEntry{{ID: id, CreatedAt: model.GetMillis(), Key: req.pendingKey()}})

			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
			api.On("GetPost", mock.Anything).Return(nil, testAppErr("none")).Maybe()

			res := p.decideWithdraw("u_req", id, "dmroot")

			require.Contains(t, res.Reply, tc.wantHint)
			require.Nil(t, res.Update, "a withdraw that never landed must not repaint the ticket DM")
			require.Empty(t, res.Outcome, "approval cards stay as they are")
			require.Contains(t, store, key, "the request is still open")
			require.Contains(t, store, claimKey, "the bot keeps its asset claim")
			api.AssertNotCalled(t, "UpdatePost", mock.Anything)
		})
	}
}

func TestExecuteWithdraw_NeedsValidID(t *testing.T) {
	api := plugintestAPI()
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	resp := runCommandAs(p, "u_req", "/mattermost-request-hub withdraw")
	require.Contains(t, resp.Text, "Give the request ID")
}

func TestPostTicketCreatedDM_IncludesWithdrawButton(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	var created *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		created = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "dmroot", ChannelId: "dm1"}, nil)

	id := model.NewId()
	root, ch := p.postTicketCreatedDM("u_req", &model.MessageAttachment{
		Title:   "Channel request",
		Actions: []*model.PostAction{{Id: "approve", Name: "Approve"}},
	}, ticketSubmittedIntro("channel request"), id)

	require.Equal(t, "dmroot", root)
	require.Equal(t, "dm1", ch)
	require.Len(t, created.Attachments(), 1)
	actions := created.Attachments()[0].Actions
	require.Len(t, actions, 1)
	require.Equal(t, "withdraw", actions[0].Id)
	require.Equal(t, "/plugins/"+manifest.Id+routeWithdraw, actions[0].Integration.URL)
	require.Equal(t, id, actions[0].Integration.Context[actionContextRequestID])
}

// plugintestAPI is a tiny alias so the needs-valid-ID test stays readable.
func plugintestAPI() *plugintest.API {
	api := &plugintest.API{}
	stubLogLevels(api)
	return api
}
