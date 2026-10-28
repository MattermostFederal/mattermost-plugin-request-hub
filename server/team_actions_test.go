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

// actionHandler returns the Approve/Deny handler under test.
type actionHandler func(p *Plugin) func(http.ResponseWriter, *http.Request, bool)

var (
	teamAdminActionHandler  actionHandler = func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamAdminAction }
	teamMemberActionHandler actionHandler = func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamMemberAction }
	teamGuestActionHandler  actionHandler = func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamGuestAction }
	teamActionHandler       actionHandler = func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleTeamAction }
	botActionHandler        actionHandler = func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleBotAction }
	webhookActionHandler    actionHandler = func(p *Plugin) func(http.ResponseWriter, *http.Request, bool) { return p.handleWebhookAction }
)

// clickAction sends body to the handler as userID and decodes the reply.
func clickAction(t *testing.T, p *Plugin, h actionHandler, userID, body string, approve bool) model.PostActionIntegrationResponse {
	t.Helper()
	w := callHandler(func(w http.ResponseWriter, r *http.Request) { h(p)(w, r, approve) }, http.MethodPost, "/action", userID, body)
	return actionResponse(t, w)
}

// runAsAdmin clicks on "post1" as System Admin "admin1" for request "r1".
func runAsAdmin(t *testing.T, p *Plugin, h actionHandler, approve bool) model.PostActionIntegrationResponse {
	t.Helper()
	return clickAction(t, p, h, "admin1", actionBody(t, "r1"), approve)
}

// mustMarshal encodes a pending request record.
func mustMarshal(t *testing.T, record any) []byte {
	t.Helper()
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	return raw
}

// requestCardPost is the bot's approval card "post1" in the approval channel.
func requestCardPost() *model.Post {
	post := &model.Post{Id: "post1", ChannelId: "appr-ch", UserId: "bot-user-id", Message: "needs review"}
	model.ParseMessageAttachment(post, []*model.MessageAttachment{{
		Title:   "Request",
		Fields:  []*model.MessageAttachmentField{{Title: "Requested by", Value: "@req"}},
		Actions: []*model.PostAction{{Id: "approve", Name: "Approve"}, {Id: "deny", Name: "Deny"}},
	}})
	return post
}

// stubTicketClose registers the calls closeTicket makes for a request with no
// DM ticket thread: a fresh DM card, the archive reply, and the card lookup.
func stubTicketClose(api *plugintest.API) {
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "created"}, nil).Maybe()
	api.On("GetPost", "post1").Return(requestCardPost(), nil).Maybe()
}

func TestNewerActionHandlers_UnusableClickReplies(t *testing.T) {
	for _, tc := range actionCasesNamed("team admin", "team member", "team guest", "team", "bot", "webhook") {
		// The mocks below register only what each click may reach; any
		// further API call fails the test.
		t.Run(tc.name+"/unreadable body", func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)

			resp := clickAction(t, p, tc.handler, "admin1", "{not json", true)

			require.Equal(t, "Invalid action request. Please try again.", resp.EphemeralText)
		})
		t.Run(tc.name+"/acting user lookup fails", func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			api.On("GetUser", "admin1").Return(nil, testAppErr("gone"))

			resp := runAsAdmin(t, p, tc.handler, true)

			require.Equal(t, "Could not verify your identity.", resp.EphemeralText)
		})
		t.Run(tc.name+"/request load fails", func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)
			stubSystemAdmin(api)
			api.On("KVGet", tc.kvPrefix+"r1").Return(nil, testAppErr("kv down"))

			resp := runAsAdmin(t, p, tc.handler, false)

			require.Equal(t, "Could not load that request.", resp.EphemeralText)
			require.Nil(t, resp.Update)
		})
	}
}

func TestNewerActionHandlers_ApproveOnDeletedTeamLeavesRequestPending(t *testing.T) {
	cases := []struct {
		name     string
		handler  actionHandler
		kvPrefix string
		record   any
	}{
		{"team admin", teamAdminActionHandler, kvTeamAdminRequestPrefix, &teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", ApprovalPostID: "post1"}},
		{"team member", teamMemberActionHandler, kvTeamMemberRequestPrefix, &teamMemberRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", ApprovalPostID: "post1"}},
		{"team guest", teamGuestActionHandler, kvTeamGuestRequestPrefix, &teamGuestRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", ApprovalPostID: "post1"}},
	}
	teams := []struct {
		name string
		team *model.Team
		err  *model.AppError
		want string
	}{
		{"deleted", nil, notFoundErr(), "The team no longer exists. Deny this request instead."},
		{"archived", &model.Team{Id: "team1", DeleteAt: 1}, nil, "The team has been archived. Deny this request instead."},
		{"lookup fails", nil, testAppErr("db down"), approvalCheckRetryText},
	}
	for _, tc := range cases {
		for _, team := range teams {
			t.Run(tc.name+"/"+team.name, func(t *testing.T) {
				api := &plugintest.API{}
				stubLogs(api)
				p := newTestPlugin(api)
				stubSystemAdmin(api)
				api.On("KVGet", tc.kvPrefix+"r1").Return(mustMarshal(t, tc.record), nil)
				api.On("GetTeam", "team1").Return(team.team, team.err)

				resp := runAsAdmin(t, p, tc.handler, true)

				require.Equal(t, team.want, resp.EphemeralText)
				require.Nil(t, resp.Update)
				api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
			})
		}
	}
}

func TestHandleTeamAdminAction_ApprovePromotesNominees(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)

	raw := mustMarshal(t, &teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"n1"}, ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "nommy"}, nil)
	api.On("KVGet", kvTeamAdminRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Sales"}, nil)
	stubActiveTeamMember(api, "team1", "u_req")
	stubActiveTeamMember(api, "team1", "n1")
	api.On("KVCompareAndDelete", kvTeamAdminRequestPrefix+"r1", raw).Return(true, nil).Once()
	api.On("CreateTeamMember", "team1", "n1").Return(&model.TeamMember{}, nil)
	api.On("UpdateTeamMemberRoles", "team1", "n1", teamAdminRoles).Return(&model.TeamMember{}, nil)
	stubTicketClose(api)

	resp := runAsAdmin(t, p, teamAdminActionHandler, true)

	require.Empty(t, resp.EphemeralText)
	require.NotNil(t, resp.Update)
	require.Equal(t, "post1", resp.Update.Id)
	require.Contains(t, resp.Update.Message, "Approved by @admin for @reqer")
	require.Contains(t, resp.Update.Message, "@nommy promoted to Team Admin in **Sales**")
	api.AssertCalled(t, "UpdateTeamMemberRoles", "team1", "n1", teamAdminRoles)
	api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}

func TestHandleTeamAdminAction_DenyAfterTeamDeletedNamesTeamByID(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)

	raw := mustMarshal(t, &teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"n1"}, ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	// Neither the requester nor the team can be loaded any more.
	api.On("GetUser", "u_req").Return(nil, testAppErr("gone"))
	api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "nommy"}, nil)
	api.On("KVGet", kvTeamAdminRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeam", "team1").Return(nil, testAppErr("deleted"))
	api.On("KVCompareAndDelete", kvTeamAdminRequestPrefix+"r1", raw).Return(true, nil).Once()
	stubTicketClose(api)

	resp := runAsAdmin(t, p, teamAdminActionHandler, false)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Denied by @admin for user u_req")
	require.Contains(t, resp.Update.Message, "Team Admin for @nommy in team1")
	api.AssertNotCalled(t, "UpdateTeamMemberRoles", mock.Anything, mock.Anything, mock.Anything)
	api.AssertCalled(t, "KVCompareAndDelete", kvTeamAdminRequestPrefix+"r1", raw)
}

func TestHandleTeamMemberAction_ApproveReportsEachOutcome(t *testing.T) {
	cases := []struct {
		name     string
		nominees []string
		failing  map[string]bool
		want     string
	}{
		{name: "all added", nominees: []string{"n1", "n2"}, want: "Added @n1, @n2 to **Ops**."},
		{name: "some fail", nominees: []string{"n1", "n2"}, failing: map[string]bool{"n2": true}, want: "Added @n1 to **Ops**. Failed for @n2."},
		{name: "all fail", nominees: []string{"n1"}, failing: map[string]bool{"n1": true}, want: "but could not add @n1 to **Ops**."},
		{name: "nobody to add", want: "No users were added to **Ops**."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubPendingLists(api)
			stubLogs(api)
			p := newTestPlugin(api)

			raw := mustMarshal(t, &teamMemberRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: tc.nominees, ApprovalPostID: "post1"})
			stubSystemAdmin(api)
			stubRequester(api)
			for _, id := range []string{"n1", "n2"} {
				api.On("GetUser", id).Return(&model.User{Id: id, Username: id}, nil).Maybe()
				if tc.failing[id] {
					api.On("CreateTeamMember", "team1", id).Return(nil, testAppErr("cannot join")).Maybe()
				} else {
					api.On("CreateTeamMember", "team1", id).Return(&model.TeamMember{}, nil).Maybe()
				}
			}
			api.On("KVGet", kvTeamMemberRequestPrefix+"r1").Return(raw, nil)
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
			api.On("KVCompareAndDelete", kvTeamMemberRequestPrefix+"r1", raw).Return(true, nil).Once()
			stubTicketClose(api)

			resp := runAsAdmin(t, p, teamMemberActionHandler, true)

			require.NotNil(t, resp.Update)
			require.Contains(t, resp.Update.Message, "Approved by @admin for @reqer")
			require.Contains(t, resp.Update.Message, tc.want)
			api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
		})
	}
}

func TestHandleTeamMemberAction_DenyAddsNobody(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)

	raw := mustMarshal(t, &teamMemberRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"n1"}, ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "n1"}, nil)
	api.On("KVGet", kvTeamMemberRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	api.On("KVCompareAndDelete", kvTeamMemberRequestPrefix+"r1", raw).Return(true, nil).Once()
	stubTicketClose(api)

	resp := runAsAdmin(t, p, teamMemberActionHandler, false)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Denied by @admin for @reqer. Team member add for @n1 in **Ops**.")
	api.AssertNotCalled(t, "CreateTeamMember", mock.Anything, mock.Anything)
}

func TestHandleTeamGuestAction_ApproveSendsInvite(t *testing.T) {
	srv := &restServer{status: http.StatusOK}
	ts := srv.start(t)
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)

	raw := mustMarshal(t, &teamGuestRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", Email: "g@example.com", ChannelIDs: []string{"ch1"}, ApprovalPostID: "post1"})
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	stubSystemAdmin(api)
	stubApproverSession(api)
	stubRequester(api)
	api.On("KVGet", kvTeamGuestRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	stubActiveTeamMember(api, "team1", "u_req")
	api.On("KVCompareAndDelete", kvTeamGuestRequestPrefix+"r1", raw).Return(true, nil).Once()
	stubTicketClose(api)

	resp := runAsAdmin(t, p, teamGuestActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Guest invitation emailed to "+escapeMarkdown("g@example.com")+" for **Ops**.")
	require.Equal(t, []string{"POST /api/v4/teams/team1/invite-guests/email"}, srv.seen())
	api.AssertCalled(t, "RevokeSession", "sess1")
	api.AssertNotCalled(t, "KVSet", kvTeamGuestRequestPrefix+"r1", mock.Anything)
}

func TestHandleTeamGuestAction_InviteFailureRestoresRequest(t *testing.T) {
	srv := &restServer{status: http.StatusNotImplemented, body: `{"message":"guest accounts are off","status_code":501}`}
	ts := srv.start(t)
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	stubSiteURL(api, ts.URL)

	raw := mustMarshal(t, &teamGuestRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", Email: "g@example.com", ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	stubApproverSession(api)
	stubRequester(api)
	api.On("KVGet", kvTeamGuestRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	stubActiveTeamMember(api, "team1", "u_req")
	api.On("KVCompareAndDelete", kvTeamGuestRequestPrefix+"r1", raw).Return(true, nil).Once()
	api.On("KVSet", kvTeamGuestRequestPrefix+"r1", raw).Return(nil).Once()

	resp := runAsAdmin(t, p, teamGuestActionHandler, true)

	require.Contains(t, resp.EphemeralText, "guest email invite failed")
	require.Nil(t, resp.Update)
	api.AssertCalled(t, "KVSet", kvTeamGuestRequestPrefix+"r1", raw)
	api.AssertNotCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}

func TestHandleTeamGuestAction_DenySendsNoInvite(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)

	raw := mustMarshal(t, &teamGuestRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", Email: "g@example.com", ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("KVGet", kvTeamGuestRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	stubActiveTeamMember(api, "team1", "u_req")
	api.On("KVCompareAndDelete", kvTeamGuestRequestPrefix+"r1", raw).Return(true, nil).Once()
	stubTicketClose(api)

	resp := runAsAdmin(t, p, teamGuestActionHandler, false)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Denied by @admin for @reqer. Guest invite for **Ops**.")
	api.AssertNotCalled(t, "CreateSession", mock.Anything)
}

func TestHandleTeamAction_ApproveCreatesTeam(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)

	raw := mustMarshal(t, &teamCreationRequest{ID: "r1", RequesterID: "u_req", Name: "ops", DisplayName: "Ops", Type: "O", RequestTeamAdmin: true, ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("KVGet", kvTeamRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvTeamRequestPrefix+"r1", raw).Return(true, nil).Once()
	api.On("CreateTeam", mock.MatchedBy(func(team *model.Team) bool {
		return team.Name == "ops" && team.DisplayName == "Ops" && team.Type == "O"
	})).Return(&model.Team{Id: "new-team", Name: "ops", DisplayName: "Ops"}, nil).Once()
	api.On("CreateTeamMember", "new-team", "u_req").Return(&model.TeamMember{}, nil)
	api.On("UpdateTeamMemberRoles", "new-team", "u_req", teamAdminRoles).Return(&model.TeamMember{}, nil)
	stubTicketClose(api)

	resp := runAsAdmin(t, p, teamActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Team **Ops** created. Requester promoted to Team Admin.")
	api.AssertCalled(t, "UpdateTeamMemberRoles", "new-team", "u_req", teamAdminRoles)
	api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}

func TestHandleTeamAction_CreateFailureKeepsRequestPending(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	raw := mustMarshal(t, &teamCreationRequest{ID: "r1", RequesterID: "u_req", Name: "ops", DisplayName: "Ops", ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("KVGet", kvTeamRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvTeamRequestPrefix+"r1", raw).Return(true, nil).Once()
	api.On("CreateTeam", mock.Anything).Return(nil, testAppErr("name taken"))
	api.On("KVSet", kvTeamRequestPrefix+"r1", mock.Anything).Return(nil).Once()

	resp := runAsAdmin(t, p, teamActionHandler, true)

	require.Contains(t, resp.EphemeralText, "Could not create the team")
	require.Contains(t, resp.EphemeralText, "The request is still pending.")
	require.Nil(t, resp.Update)
	api.AssertCalled(t, "KVSet", kvTeamRequestPrefix+"r1", mock.Anything)
	api.AssertNotCalled(t, "KVDelete", kvApprovalCardsPrefix+"r1")
}

func TestHandleTeamAction_DenyCreatesNoTeam(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)

	raw := mustMarshal(t, &teamCreationRequest{ID: "r1", RequesterID: "u_req", Name: "ops", DisplayName: "Ops", ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("KVGet", kvTeamRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvTeamRequestPrefix+"r1", raw).Return(true, nil).Once()
	stubTicketClose(api)

	resp := runAsAdmin(t, p, teamActionHandler, false)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Denied by @admin for @reqer. Team **Ops** (`/ops`).")
	api.AssertNotCalled(t, "CreateTeam", mock.Anything)
}

func TestRepaintedApprovalPost_PostLoadFailureKeepsMessageOnly(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetPost", "post1").Return(nil, testAppErr("gone"))

	post := p.repaintedApprovalPost("post1", "header", &model.MessageAttachment{Title: "x"})

	require.Equal(t, "header", post.Message)
	require.Empty(t, post.Id)
	require.Nil(t, extractFirstAttachment(post))
}

func TestRepaintedApprovalPost_ReplacesAttachment(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetPost", "post1").Return(requestCardPost(), nil)

	post := p.repaintedApprovalPost("post1", "header", &model.MessageAttachment{Title: "repainted"})

	require.Equal(t, "post1", post.Id)
	require.Equal(t, "header", post.Message)
	require.Equal(t, "repainted", extractFirstAttachment(post).Title)
}

func TestTwoStepApproveBlock_BlankUsernameIsSomeone(t *testing.T) {
	_, notice, _, paint := twoStepApproveBlock("", "You created this request — you cannot approve it.")
	require.True(t, paint)
	require.Contains(t, notice, "**@someone**")
}

func TestRespondTwoStepApproveBlocked_RepaintsCardAndSendsChannelEphemeral(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetPost", "post1").Return(requestCardPost(), nil)
	api.On("SendEphemeralPost", "u1", mock.MatchedBy(func(post *model.Post) bool {
		return post.ChannelId == "appr-ch" && post.Message == "blocked" && post.RootId == "" && post.UserId == ""
	})).Return(&model.Post{}).Once()

	w := httptest.NewRecorder()
	p.respondTwoStepApproveBlocked(w, "u1", "post1", "blocked", "header", &model.MessageAttachment{Title: "notice"})
	resp := actionResponse(t, w)

	require.Equal(t, "blocked", resp.EphemeralText)
	require.NotNil(t, resp.Update)
	require.Equal(t, "post1", resp.Update.Id)
	require.Equal(t, "header", resp.Update.Message)
	api.AssertNumberOfCalls(t, "SendEphemeralPost", 1)
	api.AssertNotCalled(t, "UpdatePost", mock.Anything)
}

func TestRespondTwoStepApproveBlocked_UnloadableCardSendsNoUpdate(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetPost", "post1").Return(nil, testAppErr("gone"))

	w := httptest.NewRecorder()
	p.respondTwoStepApproveBlocked(w, "u1", "post1", "blocked", "header", &model.MessageAttachment{Title: "notice"})
	resp := actionResponse(t, w)

	require.Equal(t, "blocked", resp.EphemeralText)
	require.Nil(t, resp.Update)
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestRespondTwoStepApproveBlocked_NoAttachmentOnlyReplies(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetPost", "post1").Return(&model.Post{Id: "post1"}, nil)

	w := httptest.NewRecorder()
	p.respondTwoStepApproveBlocked(w, "u1", "post1", "blocked", "header", nil)
	resp := actionResponse(t, w)

	require.Equal(t, "blocked", resp.EphemeralText)
	require.Nil(t, resp.Update)
	// The card has no channel, so there is nowhere to post the ephemeral.
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestArchiveApprovalDetailsInThread_SkipsPostsWithoutDetails(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	require.NoError(t, p.archiveApprovalDetailsInThread(nil))
	require.NoError(t, p.archiveApprovalDetailsInThread(&model.Post{ChannelId: "c"}))
	require.NoError(t, p.archiveApprovalDetailsInThread(&model.Post{Id: "p"}))
	require.NoError(t, p.archiveApprovalDetailsInThread(&model.Post{Id: "p", ChannelId: "c"}))
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestArchiveApprovalDetailsInThread_TitlesTheArchivedCard(t *testing.T) {
	cases := map[string]string{
		"":                               "Original request",
		"Bot request":                    "Original request — Bot request",
		"Original request — Bot request": "Original request — Bot request",
	}
	for title, want := range cases {
		t.Run(title, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			post := &model.Post{Id: "p", ChannelId: "c"}
			model.ParseMessageAttachment(post, []*model.MessageAttachment{{
				Title:   title,
				Actions: []*model.PostAction{{Id: "approve"}},
			}})
			var archived *model.MessageAttachment
			api.On("CreatePost", mock.MatchedBy(func(reply *model.Post) bool {
				return reply.RootId == "p" && reply.ChannelId == "c" && reply.UserId == "bot-user-id"
			})).Run(func(args mock.Arguments) {
				archived = extractFirstAttachment(args.Get(0).(*model.Post))
			}).Return(&model.Post{Id: "reply"}, nil).Once()

			require.NoError(t, p.archiveApprovalDetailsInThread(post))

			require.NotNil(t, archived)
			require.Equal(t, want, archived.Title)
			require.Empty(t, archived.Actions)
			require.True(t, strings.HasPrefix(archived.Title, "Original request"))
		})
	}
}
