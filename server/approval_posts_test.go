package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// actionHandlerCase describes one Approve/Deny handler so the card checks
// can be run against every request type.
type actionHandlerCase struct {
	name     string
	kvPrefix string
	// record builds the pending request with the given approval card ID.
	record func(approvalPostID string) any
	// handler returns the handler under test.
	handler func(p *Plugin) func(http.ResponseWriter, *http.Request, bool)
	// stubBeforeClaim registers the calls a Deny makes between the card
	// check and the claim.
	stubBeforeClaim func(api *plugintest.API)
}

func actionHandlerCases() []actionHandlerCase {
	none := func(*plugintest.API) {}
	stubTeam := func(api *plugintest.API) {
		api.On("GetTeam", "team1").Return(nil, testAppErr("no team"))
	}
	return []actionHandlerCase{
		{
			name:     "channel",
			kvPrefix: kvRequestPrefix,
			record: func(postID string) any {
				return &channelRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", ApprovalPostID: postID}
			},
			handler:         func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleAction },
			stubBeforeClaim: none,
		},
		{
			name:     "channel admin",
			kvPrefix: kvAdminRequestPrefix,
			record: func(postID string) any {
				return &adminRequest{ID: "r1", RequesterID: "u_req", ChannelID: "ch1", ApprovalPostID: postID}
			},
			handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleAdminAction },
			stubBeforeClaim: func(api *plugintest.API) {
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
				api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil)
			},
		},
		{
			name:     "team admin",
			kvPrefix: kvTeamAdminRequestPrefix,
			record: func(postID string) any {
				return &teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", ApprovalPostID: postID}
			},
			handler:         func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamAdminAction },
			stubBeforeClaim: stubTeam,
		},
		{
			name:     "team member",
			kvPrefix: kvTeamMemberRequestPrefix,
			record: func(postID string) any {
				return &teamMemberRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", ApprovalPostID: postID}
			},
			handler:         func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamMemberAction },
			stubBeforeClaim: stubTeam,
		},
		{
			name:     "team guest",
			kvPrefix: kvTeamGuestRequestPrefix,
			record: func(postID string) any {
				return &teamGuestRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", ApprovalPostID: postID}
			},
			handler:         func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamGuestAction },
			stubBeforeClaim: stubTeam,
		},
		{
			name:     "team",
			kvPrefix: kvTeamRequestPrefix,
			record: func(postID string) any {
				return &teamCreationRequest{ID: "r1", RequesterID: "u_req", ApprovalPostID: postID}
			},
			handler:         func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamAction },
			stubBeforeClaim: none,
		},
		{
			name:     "bot",
			kvPrefix: kvBotRequestPrefix,
			record: func(postID string) any {
				return &botRequest{ID: "r1", RequesterID: "u_req", Username: "svcbot", ApprovalPostID: postID}
			},
			handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleBotAction },
			stubBeforeClaim: func(api *plugintest.API) {
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
			},
		},
		{
			name:     "webhook",
			kvPrefix: kvWebhookRequestPrefix,
			record: func(postID string) any {
				return &webhookRequest{ID: "r1", RequesterID: "u_req", WebhookType: "incoming", ChannelID: "ch1", ApprovalPostID: postID}
			},
			handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleWebhookAction },
			stubBeforeClaim: func(api *plugintest.API) {
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
				api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil)
			},
		},
	}
}

// runAction calls the handler as System Admin "admin1" clicking on "post1"
// and returns the decoded reply.
func runAction(t *testing.T, p *Plugin, tc actionHandlerCase, approve bool) model.PostActionIntegrationResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(actionBody(t, "r1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	tc.handler(p)(w, r, approve)
	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func stubSystemAdmin(api *plugintest.API) {
	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
}

func TestActionHandlers_ClickOnOtherPostIsRefused(t *testing.T) {
	for _, tc := range actionHandlerCases() {
		for _, approve := range []bool{true, false} {
			name := tc.name + "/deny"
			if approve {
				name = tc.name + "/approve"
			}
			t.Run(name, func(t *testing.T) {
				api := &plugintest.API{}
				stubLogLevels(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)

				raw, err := json.Marshal(tc.record("card1"))
				require.NoError(t, err)
				stubSystemAdmin(api)
				api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
				api.On("KVGet", kvApprovalCardsPrefix+"r1").Return([]byte(`["card1"]`), nil)

				resp := runAction(t, p, tc, approve)

				require.Equal(t, buttonNotOnCardText, resp.EphemeralText)
				require.Nil(t, resp.Update)
				api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
				api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
				api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
				api.AssertNotCalled(t, "CreatePost", mock.Anything)
				api.AssertNotCalled(t, "UpdatePost", mock.Anything)
			})
		}
	}
}

func TestActionHandlers_UnreadableCardRecordAsksToRetry(t *testing.T) {
	for _, tc := range actionHandlerCases() {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)

			raw, err := json.Marshal(tc.record("card1"))
			require.NoError(t, err)
			stubSystemAdmin(api)
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
			api.On("KVGet", kvApprovalCardsPrefix+"r1").Return(nil, testAppErr("kv down"))

			resp := runAction(t, p, tc, false)

			require.Equal(t, cardCheckFailedText, resp.EphemeralText)
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
		})
	}
}

func TestActionHandlers_LostClaimRepliesPrivately(t *testing.T) {
	for _, tc := range actionHandlerCases() {
		for _, stillPending := range []bool{false, true} {
			name := tc.name + "/handled"
			if stillPending {
				name = tc.name + "/still pending"
			}
			t.Run(name, func(t *testing.T) {
				api := &plugintest.API{}
				stubLogs(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)

				// The click is on the request's own card, so no card lookup.
				raw, err := json.Marshal(tc.record("post1"))
				require.NoError(t, err)
				stubSystemAdmin(api)
				api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil).Once()
				if stillPending {
					api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil).Once()
				} else {
					api.On("KVGet", tc.kvPrefix+"r1").Return(nil, nil).Once()
				}
				tc.stubBeforeClaim(api)
				api.On("KVCompareAndDelete", tc.kvPrefix+"r1", raw).Return(false, nil).Once()

				resp := runAction(t, p, tc, false)

				want := alreadyHandledText
				if stillPending {
					want = claimLostPendingText
				}
				require.Equal(t, want, resp.EphemeralText)
				require.Nil(t, resp.Update)
				api.AssertNotCalled(t, "GetPost", mock.Anything)
				api.AssertNotCalled(t, "UpdatePost", mock.Anything)
			})
		}
	}
}

func TestTwoStepActionHandlers_LostFinalClaimRepliesPrivately(t *testing.T) {
	// The security step is already filled, so a System Admin's Approve is
	// the final one and claims the request.
	cases := []struct {
		name     string
		kvPrefix string
		record   any
		handler  func(p *Plugin) func(http.ResponseWriter, *http.Request, bool)
		stub     func(api *plugintest.API)
	}{
		{
			name:     "bot",
			kvPrefix: kvBotRequestPrefix,
			record: &botRequest{
				ID: "r1", RequesterID: "u_req", Username: "svcbot", ApprovalPostID: "post1",
				twoStepState: twoStepState{SecurityApproverID: "sec1", SecurityApprovedAt: 1},
			},
			handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleBotAction },
			stub: func(api *plugintest.API) {
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
			},
		},
		{
			name:     "webhook",
			kvPrefix: kvWebhookRequestPrefix,
			record: &webhookRequest{
				ID: "r1", RequesterID: "u_req", WebhookType: "incoming", ChannelID: "ch1", ApprovalPostID: "post1",
				twoStepState: twoStepState{SecurityApproverID: "sec1", SecurityApprovedAt: 1},
			},
			handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleWebhookAction },
			stub: func(api *plugintest.API) {
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
				api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil)
			},
		},
	}
	for _, tc := range cases {
		for _, stillPending := range []bool{false, true} {
			name := tc.name + "/handled"
			if stillPending {
				name = tc.name + "/still pending"
			}
			t.Run(name, func(t *testing.T) {
				api := &plugintest.API{}
				stubLogs(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)

				raw, err := json.Marshal(tc.record)
				require.NoError(t, err)
				stubSystemAdmin(api)
				api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil).Once()
				if stillPending {
					api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil).Once()
				} else {
					api.On("KVGet", tc.kvPrefix+"r1").Return(nil, nil).Once()
				}
				tc.stub(api)
				api.On("KVCompareAndDelete", tc.kvPrefix+"r1", raw).Return(false, nil).Once()

				resp := runAction(t, p, actionHandlerCase{handler: tc.handler}, true)

				want := alreadyHandledText
				if stillPending {
					want = claimLostPendingText
				}
				require.Equal(t, want, resp.EphemeralText)
				require.Nil(t, resp.Update)
				api.AssertNotCalled(t, "GetPost", mock.Anything)
				api.AssertNotCalled(t, "UpdatePost", mock.Anything)
				api.AssertNotCalled(t, "CreateBot", mock.Anything)
			})
		}
	}
}

func TestActionHandlers_ClickOnDMCopyIsAccepted(t *testing.T) {
	for _, tc := range actionHandlerCases() {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)

			// No approval channel card: the request went to System Admins by DM.
			raw, err := json.Marshal(tc.record(""))
			require.NoError(t, err)
			stubSystemAdmin(api)
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
			api.On("KVGet", kvApprovalCardsPrefix+"r1").Return([]byte(`["other-copy","post1"]`), nil)
			tc.stubBeforeClaim(api)
			api.On("KVCompareAndDelete", tc.kvPrefix+"r1", raw).Return(false, testAppErr("kv down"))

			resp := runAction(t, p, tc, false)

			require.Equal(t, "Could not process that request.", resp.EphemeralText)
			api.AssertCalled(t, "KVCompareAndDelete", tc.kvPrefix+"r1", raw)
		})
	}
}

func TestActionHandlers_MissingRequestRepliesPrivately(t *testing.T) {
	for _, tc := range actionHandlerCases() {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)

			stubSystemAdmin(api)
			api.On("KVGet", tc.kvPrefix+"r1").Return(nil, nil)

			resp := runAction(t, p, tc, true)

			require.Equal(t, alreadyHandledText, resp.EphemeralText)
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "GetPost", mock.Anything)
			api.AssertNotCalled(t, "UpdatePost", mock.Anything)
		})
	}
}

func TestActionHandlers_TurnedOffTypeRefusesApprove(t *testing.T) {
	for _, tc := range actionHandlerCases() {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{}) // every request type off

			raw, err := json.Marshal(tc.record("post1"))
			require.NoError(t, err)
			stubSystemAdmin(api)
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)

			resp := runAction(t, p, tc, true)

			require.Equal(t, requestTypeOffText, resp.EphemeralText)
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
		})
	}
}

func TestActionHandlers_TurnedOffTypeStillAllowsDeny(t *testing.T) {
	for _, tc := range actionHandlerCases() {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{})

			raw, err := json.Marshal(tc.record("post1"))
			require.NoError(t, err)
			stubSystemAdmin(api)
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
			tc.stubBeforeClaim(api)
			api.On("KVCompareAndDelete", tc.kvPrefix+"r1", raw).Return(false, testAppErr("kv down"))

			resp := runAction(t, p, tc, false)

			require.NotEqual(t, requestTypeOffText, resp.EphemeralText)
			api.AssertCalled(t, "KVCompareAndDelete", tc.kvPrefix+"r1", raw)
		})
	}
}

func TestIsRequestCard_EmptyPostIDIsNotACard(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	ok, err := p.isRequestCard("", "", "r1")
	require.NoError(t, err)
	require.False(t, ok)
	api.AssertNotCalled(t, "KVGet", mock.Anything)
}

func TestIsRequestCard_ApprovalChannelCardMatches(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	ok, err := p.isRequestCard("card1", "card1", "r1")
	require.NoError(t, err)
	require.True(t, ok)
	api.AssertNotCalled(t, "KVGet", mock.Anything)
}

func TestIsRequestCard_RecordedCardMatches(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvApprovalCardsPrefix+"r1").Return([]byte(`["a","b"]`), nil)

	ok, err := p.isRequestCard("b", "", "r1")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestIsRequestCard_UnrecordedPostDoesNotMatch(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvApprovalCardsPrefix+"r1").Return([]byte(`["a","b"]`), nil)

	ok, err := p.isRequestCard("c", "card1", "r1")
	require.NoError(t, err)
	require.False(t, ok)
	api.AssertNotCalled(t, "GetPost", mock.Anything)
}

func TestIsRequestCard_LoadFailureIsReturned(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvApprovalCardsPrefix+"r1").Return(nil, testAppErr("kv down"))

	ok, err := p.isRequestCard("a", "", "r1")
	require.Error(t, err)
	require.False(t, ok)
}

// cardPost builds a post carrying Approve/Deny buttons for requestID.
func cardPost(userID, requestID string) *model.Post {
	post := &model.Post{Id: "old-card", UserId: userID}
	model.ParseMessageAttachment(post, []*model.MessageAttachment{{
		Title: "Request",
		Actions: []*model.PostAction{{
			Id:          "approve",
			Integration: &model.PostActionIntegration{Context: map[string]any{actionContextRequestID: requestID}},
		}},
	}})
	return post
}

func TestIsRequestCard_UnrecordedRequestAcceptsBotCardForIt(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvApprovalCardsPrefix+"r1").Return(nil, nil)
	api.On("GetPost", "old-card").Return(cardPost("bot-user-id", "r1"), nil)

	ok, err := p.isRequestCard("old-card", "", "r1")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestIsRequestCard_UnrecordedRequestRefusesOtherPosts(t *testing.T) {
	cases := []struct {
		name string
		post *model.Post
		err  *model.AppError
	}{
		{name: "post by someone else", post: cardPost("u_other", "r1")},
		{name: "card for another request", post: cardPost("bot-user-id", "r2")},
		{name: "bot post without buttons", post: &model.Post{Id: "old-card", UserId: "bot-user-id"}},
		{name: "post can't be loaded", err: testAppErr("no post")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			api.On("KVGet", kvApprovalCardsPrefix+"r1").Return(nil, nil)
			api.On("GetPost", "old-card").Return(tc.post, tc.err)

			ok, err := p.isRequestCard("old-card", "", "r1")
			require.NoError(t, err)
			require.False(t, ok)
		})
	}
}

func TestLoadApprovalCards_NothingRecorded(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvApprovalCardsPrefix+"r1").Return(nil, nil)

	ids, err := p.loadApprovalCards("r1")
	require.NoError(t, err)
	require.Nil(t, ids)
}

func TestLoadApprovalCards_UnreadableRecord(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvApprovalCardsPrefix+"r1").Return([]byte("not json"), nil)

	ids, err := p.loadApprovalCards("r1")
	require.Error(t, err)
	require.Nil(t, ids)
}

func TestLoadApprovalCards_KVError(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVGet", kvApprovalCardsPrefix+"r1").Return(nil, testAppErr("kv down"))

	_, err := p.loadApprovalCards("r1")
	require.Error(t, err)
}

func TestSaveApprovalCards_NothingToSave(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	require.NoError(t, p.saveApprovalCards("r1", nil))
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestSaveApprovalCards_StoresIDs(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVSet", kvApprovalCardsPrefix+"r1", []byte(`["a","b"]`)).Return(nil)

	require.NoError(t, p.saveApprovalCards("r1", []string{"a", "b"}))
}

func TestSaveApprovalCards_StoreFailureIsReturned(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVSet", kvApprovalCardsPrefix+"r1", mock.Anything).Return(testAppErr("kv down"))

	require.Error(t, p.saveApprovalCards("r1", []string{"a"}))
}

func TestDeleteApprovalCards_RemovesRecord(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVDelete", kvApprovalCardsPrefix+"r1").Return(nil)

	p.deleteApprovalCards("r1")
}

func TestDeleteApprovalCards_FailureIsLogged(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVDelete", kvApprovalCardsPrefix+"r1").Return(testAppErr("kv down"))
	api.On("LogWarn", "failed to delete approval cards record", "request_id", "r1", "error", mock.Anything).Once()

	p.deleteApprovalCards("r1")
}

func TestDeleteApprovalCardPosts_DeletesEachAndLogsFailures(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("DeletePost", "a").Return(nil).Once()
	api.On("DeletePost", "b").Return(testAppErr("gone")).Once()
	api.On("LogWarn", "failed to delete approval card", "post_id", "b", "error", mock.Anything).Once()

	p.deleteApprovalCardPosts([]string{"a", "b"})
}

func TestCloseTicket_ClearsRecordedCards(t *testing.T) {
	for _, approvalPostID := range []string{"", "card1"} {
		t.Run("approval post "+approvalPostID, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)

			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
			api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
			api.On("KVDelete", kvApprovalCardsPrefix+"r1").Return(nil).Once()
			api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

			post := p.closeTicket(ticketClose{RequestID: "r1", RequesterID: "u_req", ApprovalPostID: "post1", StatusLine: "done"})

			require.Equal(t, "done", post.Message)
		})
	}
}

func TestPostApprovalAttachment_DMFallbackRecordsCopies(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetUsers", mock.Anything).Return([]*model.User{
		{Id: "bot-user-id"},
		{Id: "sa1"},
		{Id: "sa2"},
		{Id: "sa3"},
	}, nil)
	api.On("GetDirectChannel", "sa1", "bot-user-id").Return(&model.Channel{Id: "dm-sa1"}, nil)
	api.On("GetDirectChannel", "sa2", "bot-user-id").Return(nil, testAppErr("no dm"))
	api.On("GetDirectChannel", "sa3", "bot-user-id").Return(&model.Channel{Id: "dm-sa3"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool { return p.ChannelId == "dm-sa1" })).
		Return(&model.Post{Id: "copy-sa1"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool { return p.ChannelId == "dm-sa3" })).
		Return(&model.Post{Id: "copy-sa3"}, nil)
	api.On("KVSet", kvApprovalCardsPrefix+"r1", []byte(`["copy-sa1","copy-sa3"]`)).Return(nil)

	postID, channelID, err := p.postApprovalAttachment("r1", &model.MessageAttachment{Title: "x"}, "header", nil)

	require.NoError(t, err)
	require.Empty(t, postID)
	require.Empty(t, channelID)
}

func TestPostApprovalAttachment_NoAdminsReachedRecordsNothing(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)

	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sa1"}}, nil)
	api.On("GetDirectChannel", "sa1", "bot-user-id").Return(&model.Channel{Id: "dm-sa1"}, nil)
	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))

	_, _, err := p.postApprovalAttachment("r1", &model.MessageAttachment{Title: "x"}, "header", nil)

	require.Error(t, err)
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestPostApprovalAttachment_ChannelCardIsRecorded(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "card1"}, nil)
	api.On("KVSet", kvApprovalCardsPrefix+"r1", []byte(`["card1"]`)).Return(nil)

	postID, channelID, err := p.postApprovalAttachment("r1", &model.MessageAttachment{Title: "x"}, "header", pluginChannel("appr", "mattermost-channel-requests"))

	require.NoError(t, err)
	require.Equal(t, "card1", postID)
	require.Equal(t, "appr", channelID)
}

func TestPostApprovalAttachment_ChannelPostFailureRecordsNothing(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)

	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))

	_, _, err := p.postApprovalAttachment("r1", &model.MessageAttachment{Title: "x"}, "header", pluginChannel("appr", "mattermost-channel-requests"))

	require.Error(t, err)
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestPostApprovalAttachment_RecordFailureDeletesCards(t *testing.T) {
	t.Run("approval channel", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)

		api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "card1"}, nil)
		api.On("KVSet", kvApprovalCardsPrefix+"r1", mock.Anything).Return(testAppErr("kv down"))
		api.On("DeletePost", "card1").Return(nil).Once()

		postID, channelID, err := p.postApprovalAttachment("r1", &model.MessageAttachment{Title: "x"}, "header", pluginChannel("appr", "mattermost-channel-requests"))

		require.Error(t, err)
		require.Empty(t, postID)
		require.Empty(t, channelID)
	})
	t.Run("System Admin copies", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)

		api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sa1"}, {Id: "sa2"}}, nil)
		api.On("GetDirectChannel", "sa1", "bot-user-id").Return(&model.Channel{Id: "dm-sa1"}, nil)
		api.On("GetDirectChannel", "sa2", "bot-user-id").Return(&model.Channel{Id: "dm-sa2"}, nil)
		api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool { return p.ChannelId == "dm-sa1" })).
			Return(&model.Post{Id: "copy-sa1"}, nil)
		api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool { return p.ChannelId == "dm-sa2" })).
			Return(&model.Post{Id: "copy-sa2"}, nil)
		api.On("KVSet", kvApprovalCardsPrefix+"r1", mock.Anything).Return(testAppErr("kv down"))
		api.On("DeletePost", "copy-sa1").Return(nil).Once()
		api.On("DeletePost", "copy-sa2").Return(testAppErr("gone")).Once()

		_, _, err := p.postApprovalAttachment("r1", &model.MessageAttachment{Title: "x"}, "header", nil)

		require.Error(t, err)
	})
}

func TestActionHandlers_LostRequestClearsCards(t *testing.T) {
	t.Run("team", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)

		raw, err := json.Marshal(&teamCreationRequest{ID: "r1", RequesterID: "u_req", Name: "ops", DisplayName: "Ops", ApprovalPostID: "post1"})
		require.NoError(t, err)
		stubSystemAdmin(api)
		api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
		api.On("KVGet", kvTeamRequestPrefix+"r1").Return(raw, nil)
		api.On("KVCompareAndDelete", kvTeamRequestPrefix+"r1", raw).Return(true, nil)
		api.On("CreateTeam", mock.Anything).Return(nil, testAppErr("name taken"))
		api.On("KVSet", kvTeamRequestPrefix+"r1", mock.Anything).Return(testAppErr("kv down"))

		resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamAction }}, true)

		require.Contains(t, resp.EphemeralText, "could not be saved")
		api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
	})
	t.Run("team guest", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)

		raw, err := json.Marshal(&teamGuestRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", Email: "g@example.com", ApprovalPostID: "post1"})
		require.NoError(t, err)
		stubSystemAdmin(api)
		api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
		api.On("KVGet", kvTeamGuestRequestPrefix+"r1").Return(raw, nil)
		api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Once()
		api.On("KVCompareAndDelete", kvTeamGuestRequestPrefix+"r1", raw).Return(true, nil)
		// The invite can't be sent, and the pending request can't be written back.
		api.On("GetTeam", "team1").Return(nil, testAppErr("gone")).Once()
		api.On("KVSet", kvTeamGuestRequestPrefix+"r1", raw).Return(testAppErr("kv down"))

		resp := runAction(t, p, actionHandlerCase{handler: func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamGuestAction }}, true)

		require.Contains(t, resp.EphemeralText, "failed to load team")
		api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
	})
}
