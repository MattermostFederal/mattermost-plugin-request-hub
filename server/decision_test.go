package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Request IDs that pass model.IsValidId.
var (
	testChannelRequestID = model.NewId()
	testAdminRequestID   = model.NewId()
)

// approvalCard is a bot-authored approval post with buttons.
func approvalCard(id string) *model.Post {
	post := &model.Post{Id: id, ChannelId: "ch-" + id, UserId: "bot-user-id", Message: "header"}
	model.ParseMessageAttachment(post, []*model.MessageAttachment{{
		Title:   "Channel creation request",
		Footer:  requestIDFooter("x"),
		Actions: []*model.PostAction{{Id: "approve", Name: "Approve"}},
	}})
	return post
}

// ticketRoot is the requester's ticket DM root card.
func ticketRoot() *model.Post {
	root := &model.Post{Id: "dmroot", ChannelId: "dm1", UserId: "bot-user-id"}
	model.ParseMessageAttachment(root, []*model.MessageAttachment{{Title: "Channel request", Color: colorPending}})
	return root
}

// updatedPosts records every UpdatePost by post ID.
func updatedPosts(api *plugintest.API) map[string]*model.Post {
	updated := map[string]*model.Post{}
	api.On("UpdatePost", mock.Anything).Run(func(args mock.Arguments) {
		post := args.Get(0).(*model.Post)
		updated[post.Id] = post
	}).Return(&model.Post{}, nil).Maybe()
	return updated
}

// decisionFixture is a plugin backed by fakeKV with a stored channel request
// from u_req and its ticket DM. With approvalPostID set the request was
// posted to an approval channel; without it, it went to System Admin DMs
// recorded as post1 and post2. The channel and Channel Admin toggles are on.
func decisionFixture(t *testing.T, approvalPostID string) (*plugintest.API, *Plugin, map[string][]byte) {
	t.Helper()
	api := &plugintest.API{}
	stubLogLines(api)
	t.Cleanup(func() { api.AssertExpectations(t) })
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())
	store := fakeKV(api)

	req := &channelRequest{
		ID: testChannelRequestID, RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O", CreatedAt: model.GetMillis(),
		ApprovalPostID: approvalPostID, DMRootPostID: "dmroot", DMChannelID: "dm1",
	}
	if approvalPostID != "" {
		req.ApprovalChannelID = "appch"
	} else {
		store[kvApprovalCardsPrefix+req.ID] = recordJSON(t, []string{"post1", "post2"})
	}
	store[kvRequestPrefix+req.ID] = recordJSON(t, req)
	store[pendingListKey(pendingKindChannel, "u_req")] = recordJSON(t, []pendingEntry{{ID: req.ID, CreatedAt: req.CreatedAt, Key: req.pendingKey()}})

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil).Maybe()
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
	return api, p, store
}

// mockChannelCreate lets an approval of the fixture's request create its channel.
func mockChannelCreate(api *plugintest.API) {
	stubActiveTeamMember(api, "team1", "u_req")
	stubLiveTeam(api, "team1")
	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "team-x", TeamId: "team1"}, nil)
	api.On("AddChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_req", channelAdminRoleString).Return(&model.ChannelMember{}, nil)
}

func TestExecuteDecision_NeedsOneValidID(t *testing.T) {
	for _, command := range []string{
		"/mattermost-request-hub approve",
		"/mattermost-request-hub approve not-an-id",
		"/mattermost-request-hub deny " + testChannelRequestID + " extra",
	} {
		t.Run(command, func(t *testing.T) {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)

			resp := runCommandAs(p, "admin1", command)

			require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
			// Nothing is mocked, so any KV read would have panicked.
			require.Contains(t, resp.Text, "Give the request ID from the approval card")
			require.Contains(t, resp.Text, "/mattermost-request-hub")
		})
	}
}

func TestExecuteDecision_UnknownIDIsAlreadyHandled(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	fakeKV(api)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+model.NewId())

	require.Equal(t, alreadyHandledText, resp.Text)
}

func TestExecuteDecision_LookupFailure(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVGet", kvRequestPrefix+testChannelRequestID).Return(nil, testAppErr("kv down"))

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

	require.Equal(t, "Could not load that request.", resp.Text)
}

func TestExecuteDecision_ApproveUpdatesTheStoredCardAndTicket(t *testing.T) {
	api, p, store := decisionFixture(t, "card1")
	mockChannelCreate(api)
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil)
	updated := updatedPosts(api)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

	const outcome = "✅ Approved by @admin for @req. Channel ~team-x created."
	require.Equal(t, outcome, resp.Text)
	// The approval-channel card is tombstoned with the outcome, its details
	// archived into its thread.
	require.Contains(t, updated, "card1")
	require.Equal(t, outcome, updated["card1"].Message)
	require.Empty(t, updated["card1"].Attachments())
	api.AssertCalled(t, "CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.RootId == "card1" }))
	// The requester's ticket is closed as approved.
	ticket := extractFirstAttachment(updated["dmroot"])
	require.Equal(t, "Channel Request Approved", ticket.Title)
	require.Equal(t, colorApproved, ticket.Color)
	api.AssertCalled(t, "CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post.RootId == "dmroot" && post.Message == "✅ **Approved** by @admin."
	}))
	require.NotContains(t, store, kvRequestPrefix+testChannelRequestID)
	require.NotContains(t, store, pendingListKey(pendingKindChannel, "u_req"))
}

func TestExecuteDecision_ApproveResolvesEverySystemAdminCopy(t *testing.T) {
	api, p, store := decisionFixture(t, "")
	mockChannelCreate(api)
	api.On("GetPost", "post1").Return(approvalCard("post1"), nil)
	api.On("GetPost", "post2").Return(approvalCard("post2"), nil)
	updated := updatedPosts(api)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

	require.Equal(t, "✅ Approved by @admin for @req. Channel ~team-x created.", resp.Text)
	for _, id := range []string{"post1", "post2"} {
		require.Contains(t, updated, id)
		require.Equal(t, resp.Text, updated[id].Message)
		require.Empty(t, updated[id].Attachments())
	}
	require.Contains(t, updated, "dmroot")
	require.NotContains(t, store, kvRequestPrefix+testChannelRequestID)
	require.NotContains(t, store, kvApprovalCardsPrefix+testChannelRequestID)
}

func TestExecuteDecision_RefusalsLeaveTheRequestPending(t *testing.T) {
	t.Run("not an approver", func(t *testing.T) {
		api, p, store := decisionFixture(t, "")
		api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Roles: "system_user"}, nil)
		api.On("GetTeamMember", "team1", "u_other").Return(&model.TeamMember{TeamId: "team1", UserId: "u_other"}, nil)

		resp := runCommandAs(p, "u_other", "/mattermost-request-hub approve "+testChannelRequestID)

		require.Contains(t, resp.Text, "You don't have permission to approve or deny this channel request")
		require.Contains(t, store, kvRequestPrefix+testChannelRequestID)
		require.Contains(t, store, kvApprovalCardsPrefix+testChannelRequestID)
	})

	t.Run("own request", func(t *testing.T) {
		// A Team Admin of the request's team who filed it.
		api, p, store := decisionFixture(t, "card1")
		api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{TeamId: "team1", UserId: "u_req", SchemeAdmin: true}, nil)

		resp := runCommandAs(p, "u_req", "/mattermost-request-hub approve "+testChannelRequestID)

		require.Equal(t, ownRequestText, resp.Text)
		require.Contains(t, store, kvRequestPrefix+testChannelRequestID)
	})

	t.Run("requester left the team", func(t *testing.T) {
		api, p, store := decisionFixture(t, "card1")
		api.On("GetTeamMember", "team1", "u_req").Return(nil, &model.AppError{StatusCode: http.StatusNotFound})
		stubLiveTeam(api, "team1")

		resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

		require.Equal(t, "The requester is no longer a member of the team. Deny this request instead.", resp.Text)
		require.Contains(t, store, kvRequestPrefix+testChannelRequestID)
		api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	})
}

func TestExecuteDecision_TurnedOffTypeRefusesApproveOnly(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		api, p, store := decisionFixture(t, "card1")
		p.setConfiguration(&configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})

		resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

		require.Equal(t, requestTypeOffText, resp.Text)
		require.Contains(t, store, kvRequestPrefix+testChannelRequestID)
		api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	})

	t.Run("deny", func(t *testing.T) {
		api, p, store := decisionFixture(t, "card1")
		p.setConfiguration(&configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})
		api.On("GetPost", "card1").Return(approvalCard("card1"), nil)
		updatedPosts(api)

		resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+testChannelRequestID)

		require.Contains(t, resp.Text, "❌ Denied by @admin")
		require.NotContains(t, store, kvRequestPrefix+testChannelRequestID)
	})
}

func TestActionHandlers_ClickWithoutPostIsRefused(t *testing.T) {
	for name, handler := range map[string]func(p *Plugin) func(http.ResponseWriter, *http.Request, bool){
		"channel":       func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleAction },
		"channel admin": func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleAdminAction },
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			body, err := json.Marshal(model.PostActionIntegrationRequest{Context: map[string]any{actionContextRequestID: testChannelRequestID}})
			require.NoError(t, err)

			// Nothing is mocked, so loading the request would have panicked.
			w := callHandler(func(w http.ResponseWriter, r *http.Request) { handler(p)(w, r, true) },
				http.MethodPost, routeApprove, "admin1", string(body))

			require.Equal(t, buttonNotOnCardText, actionResponse(t, w).EphemeralText)
		})
	}
}

func TestExecuteDecision_LostClaimAsksToRunAgain(t *testing.T) {
	req := &channelRequest{ID: testChannelRequestID, RequesterID: "u_req", TeamID: "team1", Name: "team-x"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+req.ID).Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+req.ID, raw).Return(false, nil)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+req.ID)

	require.Equal(t, "This request was just updated. Please run the command again.", resp.Text)
}

func TestExecuteDecision_DenyAdminRequest(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())
	store := fakeKV(api)

	req := &adminRequest{
		ID: testAdminRequestID, RequesterID: "u_req", ChannelID: "ch1", NomineeIDs: []string{"u_nom"}, CreatedAt: model.GetMillis(),
		ApprovalPostID: "card1", ApprovalChannelID: "appch", DMRootPostID: "dmroot", DMChannelID: "dm1",
	}
	store[kvAdminRequestPrefix+req.ID] = recordJSON(t, req)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1"}, nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil)
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil)
	updated := updatedPosts(api)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+testAdminRequestID)

	require.Equal(t, "❌ Denied by @admin for @req. Channel Admin for @nom in ~town.", resp.Text)
	require.Equal(t, resp.Text, updated["card1"].Message)
	require.Equal(t, "Channel Admin Request Denied", extractFirstAttachment(updated["dmroot"]).Title)
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
	require.NotContains(t, store, kvAdminRequestPrefix+testAdminRequestID)
}

func TestExecuteDecision_ExpiredClosesCardsAndTicket(t *testing.T) {
	api, p, store := decisionFixture(t, "")
	config := approvableConfig()
	config.expiryDays = 30
	p.setConfiguration(config)
	req := &channelRequest{ID: testChannelRequestID, RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", CreatedAt: expiredMillis(), DMRootPostID: "dmroot", DMChannelID: "dm1"}
	store[kvRequestPrefix+req.ID] = recordJSON(t, req)
	api.On("GetPost", "post1").Return(approvalCard("post1"), nil)
	api.On("GetPost", "post2").Return(approvalCard("post2"), nil)
	updated := updatedPosts(api)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

	require.Equal(t, expiredText, resp.Text)
	require.Equal(t, "⌛ Expired: not reviewed within 30 days.", updated["post1"].Message)
	require.Equal(t, "⌛ Expired: not reviewed within 30 days.", updated["post2"].Message)
	require.Equal(t, "Channel Request Expired", extractFirstAttachment(updated["dmroot"]).Title)
	require.NotContains(t, store, kvApprovalCardsPrefix+testChannelRequestID)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestHandleAction_ResolvesTheOtherSystemAdminCopies(t *testing.T) {
	api, p, store := decisionFixture(t, "")
	api.On("GetPost", "post1").Return(approvalCard("post1"), nil)
	api.On("GetPost", "post2").Return(approvalCard("post2"), nil)
	updated := updatedPosts(api)

	// actionBody clicks post1, which the response updates.
	w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, false) },
		http.MethodPost, routeDeny, "admin1", actionBody(t, testChannelRequestID))

	resp := actionResponse(t, w)
	const outcome = "❌ Denied by @admin for @req. Channel request **X** (`team-x`)."
	require.Empty(t, resp.EphemeralText)
	require.Equal(t, outcome, resp.Update.Message)
	require.NotContains(t, updated, "post1")
	require.Equal(t, outcome, updated["post2"].Message)
	require.NotContains(t, store, kvApprovalCardsPrefix+testChannelRequestID)
}

func TestResolveApprovalCards(t *testing.T) {
	t.Run("skips missing, foreign and failing posts", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLines(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		store := fakeKV(api)
		store[kvApprovalCardsPrefix+"r1"] = recordJSON(t, []string{"gone", "foreign", "fails", "ok", "skip"})

		api.On("GetPost", "gone").Return(nil, testAppErr("not found"))
		api.On("GetPost", "foreign").Return(&model.Post{Id: "foreign", UserId: "someone"}, nil)
		api.On("GetPost", "fails").Return(approvalCard("fails"), nil)
		api.On("GetPost", "ok").Return(approvalCard("ok"), nil)
		api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
		api.On("UpdatePost", mock.MatchedBy(func(post *model.Post) bool { return post.Id == "fails" })).Return(nil, testAppErr("update failed"))
		api.On("UpdatePost", mock.MatchedBy(func(post *model.Post) bool { return post.Id == "ok" })).Return(&model.Post{}, nil)

		p.resolveApprovalCards("r1", "done", "skip")

		api.AssertNumberOfCalls(t, "UpdatePost", 2)
		api.AssertNotCalled(t, "GetPost", "skip")
		api.AssertCalled(t, "LogWarn", "failed to resolve approval card", "request_id", "r1", "post_id", "fails", "error", mock.Anything)
		api.AssertCalled(t, "LogWarn", "failed to load approval card", "request_id", "r1", "post_id", "gone", "error", mock.Anything)
		// The record stays for closeTicket to clear.
		require.Contains(t, store, kvApprovalCardsPrefix+"r1")
	})

	t.Run("nothing recorded", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", kvApprovalCardsPrefix+"r1").Return(nil, nil)

		// Nothing else is mocked, so any other call would have panicked.
		p.resolveApprovalCards("r1", "done", "")
	})

	for name, kv := range map[string]func(api *plugintest.API){
		"load fails": func(api *plugintest.API) {
			api.On("KVGet", kvApprovalCardsPrefix+"r1").Return(nil, testAppErr("kv down"))
		},
		"unreadable": func(api *plugintest.API) { api.On("KVGet", kvApprovalCardsPrefix+"r1").Return([]byte("not json"), nil) },
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			kv(api)

			p.resolveApprovalCards("r1", "done", "")

			api.AssertCalled(t, "LogWarn", "failed to resolve approval cards", "request_id", "r1", "error", mock.Anything)
			api.AssertNotCalled(t, "GetPost", mock.Anything)
		})
	}
}

func TestResolveCardsIfHandled(t *testing.T) {
	t.Run("leaves the cards while the request is pending", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		store := fakeKV(api)
		store[kvAdminRequestPrefix+"r1"] = []byte("{}")
		store[kvApprovalCardsPrefix+"r1"] = recordJSON(t, []string{"a", "b"})

		// GetPost isn't mocked, so resolving a card would have panicked.
		p.resolveCardsIfHandled(pendingKindAdmin, "r1")

		require.Contains(t, store, kvApprovalCardsPrefix+"r1")
	})

	t.Run("resolves the cards when the request was decided meanwhile", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		store := fakeKV(api)
		store[kvApprovalCardsPrefix+"r1"] = recordJSON(t, []string{"a"})
		api.On("GetPost", "a").Return(approvalCard("a"), nil)
		api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
		api.On("UpdatePost", mock.MatchedBy(func(post *model.Post) bool {
			return post.Id == "a" && post.Message == alreadyHandledText
		})).Return(&model.Post{}, nil)

		p.resolveCardsIfHandled(pendingKindChannel, "r1")

		api.AssertNumberOfCalls(t, "UpdatePost", 1)
		require.NotContains(t, store, kvApprovalCardsPrefix+"r1")
	})

	t.Run("a failed check is only logged", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", kvRequestPrefix+"r1").Return(nil, testAppErr("kv down"))

		p.resolveCardsIfHandled(pendingKindChannel, "r1")

		api.AssertCalled(t, "LogWarn", "failed to check request after recording approval cards", "request_id", "r1", "error", mock.Anything)
	})
}

func TestSubmitRequest_ShowsTheIDAndRecordsSystemAdminCopies(t *testing.T) {
	t.Run("approval channel", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		stubPendingLists(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(approvalConfig())
		mockMemberRequester(api)
		mockApprovalChannel(api)
		api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng"}, nil)
		var requestKey string
		api.On("KVSet", isRequestRecordKey, mock.Anything).Run(func(args mock.Arguments) { requestKey = args.String(0) }).Return(nil)
		api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
		var card *model.Post
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "appch" })).
			Run(func(args mock.Arguments) { card = args.Get(0).(*model.Post) }).Return(&model.Post{Id: "card1"}, nil)
		api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dmroot"}, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()

		_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})

		require.NoError(t, err)
		id := requestKey[len(kvRequestPrefix):]
		require.Equal(t, "Request ID: "+id, card.Attachments()[0].Footer)
		// The approval-channel card is recorded as the request's only card.
		api.AssertCalled(t, "KVSet", kvApprovalCardsPrefix+id, []byte(`["card1"]`))
	})

	t.Run("System Admin direct messages", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		stubPendingLists(api)
		stubSubmittedRequestExists(api, kvRequestPrefix)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		config := approvalConfig()
		config.ApprovalTeam = ""
		p.setConfiguration(config)
		mockMemberRequester(api)
		api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng"}, nil)
		var requestKey string
		api.On("KVSet", isRequestRecordKey, mock.Anything).Run(func(args mock.Arguments) { requestKey = args.String(0) }).Return(nil)
		api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sys1", Roles: "system_admin"}, {Id: "sys2", Roles: "system_admin"}}, nil)
		api.On("GetDirectChannel", "sys1", "bot-user-id").Return(&model.Channel{Id: "dm-sys1"}, nil)
		api.On("GetDirectChannel", "sys2", "bot-user-id").Return(&model.Channel{Id: "dm-sys2"}, nil)
		api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm-sys1" })).Return(&model.Post{Id: "copy1"}, nil)
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm-sys2" })).Return(&model.Post{Id: "copy2"}, nil)
		api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dmroot"}, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()

		_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})

		require.NoError(t, err)
		id := requestKey[len(kvRequestPrefix):]
		api.AssertCalled(t, "KVSet", kvApprovalCardsPrefix+id, []byte(`["copy1","copy2"]`))
	})
}

func TestSubmitAdminRequest_ShowsTheIDAndRecordsSystemAdminCopies(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	stubSubmittedRequestExists(api, kvAdminRequestPrefix)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "req", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "ch1", mock.Anything).Return(&model.ChannelMember{}, nil)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng"}, nil).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sys1", Roles: "system_admin"}}, nil)
	api.On("GetTeamMembers", "team1", mock.Anything, mock.Anything).Return([]*model.TeamMember{}, nil).Maybe()
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nom"}, nil)
	api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "sys", Roles: "system_admin"}, nil).Maybe()
	api.On("GetDirectChannel", "sys1", "bot-user-id").Return(&model.Channel{Id: "dm-sys1"}, nil)
	api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	var requestKey string
	api.On("KVSet", mock.MatchedBy(func(key string) bool {
		return len(key) > len(kvAdminRequestPrefix) && key[:len(kvAdminRequestPrefix)] == kvAdminRequestPrefix
	}), mock.Anything).
		Run(func(args mock.Arguments) { requestKey = args.String(0) }).Return(nil)
	var card *model.Post
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm-sys1" })).
		Run(func(args mock.Arguments) { card = args.Get(0).(*model.Post) }).Return(&model.Post{Id: "copy1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dmroot"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()

	_, err := p.submitAdminRequest("u1", "ch1", []string{testNomineeID})

	require.NoError(t, err)
	id := requestKey[len(kvAdminRequestPrefix):]
	require.Equal(t, "Request ID: "+id, card.Attachments()[0].Footer)
	api.AssertCalled(t, "KVSet", kvApprovalCardsPrefix+id, []byte(`["copy1"]`))
}

func TestDecision_CreateFailureKeepsTheCards(t *testing.T) {
	api, p, store := decisionFixture(t, "")
	stubActiveTeamMember(api, "team1", "u_req")
	stubLiveTeam(api, "team1")
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("name taken"))

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

	require.Contains(t, resp.Text, "The request is still pending")
	require.Contains(t, store, kvRequestPrefix+testChannelRequestID)
	require.Contains(t, store, kvApprovalCardsPrefix+testChannelRequestID)
	api.AssertNotCalled(t, "UpdatePost", mock.Anything)
}

func TestDecision_LostRequestResolvesTheCards(t *testing.T) {
	api, p, store := decisionFixture(t, "")
	stubActiveTeamMember(api, "team1", "u_req")
	stubLiveTeam(api, "team1")
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("name taken"))
	// The restore after the failed create fails too. fakeKV's KVSet was
	// registered first, so move this one ahead of it.
	restore := api.On("KVSet", kvRequestPrefix+testChannelRequestID, mock.Anything).Return(testAppErr("kv down")).Once()
	api.ExpectedCalls = append([]*mock.Call{restore}, api.ExpectedCalls[:len(api.ExpectedCalls)-1]...)
	api.On("GetPost", "post1").Return(approvalCard("post1"), nil)
	api.On("GetPost", "post2").Return(approvalCard("post2"), nil)
	updated := updatedPosts(api)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+testChannelRequestID)

	require.Contains(t, resp.Text, "could not be saved")
	const lost = "⚠️ Approval by @admin failed and the request could not be kept. Ask the requester to submit it again."
	require.Equal(t, lost, updated["post1"].Message)
	require.Equal(t, lost, updated["post2"].Message)
	require.NotContains(t, store, kvApprovalCardsPrefix+testChannelRequestID)
	require.NotContains(t, store, pendingListKey(pendingKindChannel, "u_req"))
}

func TestExecuteDecision_CardUpdateFailureIsLogged(t *testing.T) {
	api, p, store := decisionFixture(t, "card1")
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil)
	api.On("UpdatePost", mock.MatchedBy(func(post *model.Post) bool { return post.Id == "card1" })).Return(nil, testAppErr("update failed"))
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+testChannelRequestID)

	// The decision stands even though the card couldn't be updated.
	require.Contains(t, resp.Text, "❌ Denied by @admin")
	require.NotContains(t, store, kvRequestPrefix+testChannelRequestID)
	api.AssertCalled(t, "LogWarn", "failed to resolve approval card", "request_id", testChannelRequestID, "post_id", "card1", "error", mock.Anything)
}
