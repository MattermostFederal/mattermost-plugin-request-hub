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

func TestSubmitTeamAdminRequest_RequiresTeam(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	_, err := p.submitTeamAdminRequest("u_req", "", []string{testNomineeID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "team is required")
}

func TestSubmitTeamAdminRequest_RequiresNominees(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	_, err := p.submitTeamAdminRequest("u_req", "team1", []string{"", "  "})
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least one person")
}

func TestSubmitTeamAdminRequest_RequesterMustBeTeamMember(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Sales"}, nil)
	// Non-admin requester who is not a team member.
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "team1", "u_req").Return(nil, testAppErr("not a member"))

	_, err := p.submitTeamAdminRequest("u_req", "team1", []string{testNomineeID})

	require.Error(t, err)
	require.Contains(t, err.Error(), "member of the team")
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestSubmitTeamAdminRequest_StoresAndPostsRequest(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowTeamAdminRequests: true})

	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Sales"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{UserId: "u_req"}, nil)
	// mentionList calls GetUser for each nominee (in the attachment Text field).
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetTeamMember", "team1", testNomineeID).Return(&model.TeamMember{UserId: testNomineeID}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	// Team Admin requests go to the dedicated team-requests channel via
	// ensureTeamCreationApprovalChannel → ensureNamedApprovalChannel.
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-team-requests", false).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "team-req-ch", TeamId: "appteam-id"}, nil)
	// adminApproverIDs + forceTicketChannelCategory share GetUsers.
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sys1", Username: "root"}}, nil)
	api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "root"}, nil)
	// The team's Team Admins are mentioned with the System Admins.
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{
		{TeamId: "team1", UserId: "ta1", SchemeAdmin: true},
		{TeamId: "team1", UserId: "u_req"},
	}, nil)
	api.On("GetUser", "ta1").Return(&model.User{Id: "ta1", Username: "lead"}, nil)
	api.On("GetUsersByIds", []string{"ta1"}).Return([]*model.User{{Id: "ta1", Username: "lead"}}, nil)
	api.On("GetChannelSidebarCategories", "sys1", "appteam-id").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"team-req-ch"},
		}},
	}, nil).Maybe()

	// Approval post captured to verify destination.
	var approvalPost *model.Post
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post.ChannelId == "team-req-ch"
	})).Run(func(args mock.Arguments) {
		approvalPost = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "approval-post-1"}, nil)

	// DM notification to requester.
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "req-dm"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dm-post-1"}, nil)

	msg, err := p.submitTeamAdminRequest("u_req", "team1", []string{testNomineeID})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	require.NotNil(t, approvalPost)
	require.Equal(t, "team-req-ch", approvalPost.ChannelId)
	require.Contains(t, approvalPost.Message, "@root")
	require.Contains(t, approvalPost.Message, "@lead")
	require.NotContains(t, approvalPost.Message, "@reqer")
	api.AssertCalled(t, "KVSet", mock.Anything, mock.Anything)
	// No promotion yet — pending approval.
	api.AssertNotCalled(t, "UpdateTeamMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestPromoteTeamAdmins_AddsMemberAndRole(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateTeamMember", "team1", mock.Anything).Return(&model.TeamMember{}, nil)
	api.On("UpdateTeamMemberRoles", "team1", mock.Anything, teamAdminRoles).Return(&model.TeamMember{}, nil)

	p.promoteTeamAdmins(&teamAdminRequest{TeamID: "team1", NomineeIDs: []string{"u1", "u2"}})

	api.AssertNumberOfCalls(t, "CreateTeamMember", 2)
	api.AssertCalled(t, "CreateTeamMember", "team1", "u1")
	api.AssertCalled(t, "CreateTeamMember", "team1", "u2")
	api.AssertNumberOfCalls(t, "UpdateTeamMemberRoles", 2)
	api.AssertCalled(t, "UpdateTeamMemberRoles", "team1", "u1", teamAdminRoles)
	api.AssertCalled(t, "UpdateTeamMemberRoles", "team1", "u2", teamAdminRoles)
}

func TestPromoteTeamAdmins_ReseedsPerTeamApprovalChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true})

	api.On("CreateTeamMember", "team1", "u1").Return(&model.TeamMember{}, nil)
	api.On("UpdateTeamMemberRoles", "team1", "u1", teamAdminRoles).Return(&model.TeamMember{}, nil)

	// hasActiveTeamApprovers → true via new Team Admin
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{
		{TeamId: "team1", UserId: "u1", Roles: "team_user team_admin", SchemeAdmin: true},
	}, nil)

	existing := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "ch-req", TeamId: "team1", Name: "mattermost-channel-requests"}
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).Return(existing, nil)
	api.On("AddChannelMember", "ch-req", "u1").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelSidebarCategories", "u1", "team1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"ch-req"},
		}},
	}, nil)
	api.On("GetUsersInChannel", "ch-req", "username", 0, 200).Return([]*model.User{{Id: "u1"}}, nil).Maybe()
	api.On("KVGet", mock.Anything).Return(nil, nil).Maybe()
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()

	p.promoteTeamAdmins(&teamAdminRequest{TeamID: "team1", NomineeIDs: []string{"u1"}})

	api.AssertCalled(t, "AddChannelMember", "ch-req", "u1")
}

func TestPromoteTeamAdmins_MemberAddFailureStillAttemptsRoleGrant(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	// CreateTeamMember fails (already a member), but role grant must still run.
	api.On("CreateTeamMember", "team1", "u1").Return(nil, testAppErr("already a member"))
	api.On("UpdateTeamMemberRoles", "team1", "u1", teamAdminRoles).Return(&model.TeamMember{}, nil)

	p.promoteTeamAdmins(&teamAdminRequest{TeamID: "team1", NomineeIDs: []string{"u1"}})

	api.AssertCalled(t, "UpdateTeamMemberRoles", "team1", "u1", teamAdminRoles)
}

func TestPostTeamAdminApprovalRequest_UsesPerTeamChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", PerTeamApprovalChannels: true})

	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{
		{TeamId: "team1", UserId: "ta1", SchemeAdmin: true},
	}, nil)
	api.On("GetUser", "ta1").Return(&model.User{Id: "ta1", Username: "lead"}, nil)
	api.On("GetUsersByIds", []string{"ta1"}).Return([]*model.User{{Id: "ta1", Username: "lead"}}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
		Return(pluginChannel("team-ch", "mattermost-channel-requests"), nil)
	api.On("KVSet", kvApprovalCardsPrefix+"r1", mock.Anything).Return(nil)

	var approvalPost *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		approvalPost = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "approval-post-1"}, nil)

	req := &teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{testNomineeID}}
	postID, channelID, err := p.postTeamAdminApprovalRequest(req, &model.User{Id: "u_req", Username: "reqer"}, &model.Team{Id: "team1", DisplayName: "Sales"})

	require.NoError(t, err)
	require.Equal(t, "approval-post-1", postID)
	require.Equal(t, "team-ch", channelID)
	require.Contains(t, approvalPost.Message, "@lead")
	api.AssertNotCalled(t, "GetChannelByName", "appteam-id", "mattermost-team-requests", false)
}

func TestCanApproveTeamAdminRequest(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowTeamAdminApprovers: true, AllowTeamAdminTeamApprovers: true, ApprovalTeam: "appteam"})

	api.On("GetTeamMember", "team1", "ta1").Return(&model.TeamMember{TeamId: "team1", UserId: "ta1", Roles: "team_user team_admin"}, nil)
	api.On("GetTeamMember", "team1", "member1").Return(&model.TeamMember{TeamId: "team1", UserId: "member1", Roles: "team_user"}, nil)
	// Team Admin of the approval team only.
	api.On("GetTeamMember", "team1", "appta").Return(nil, testAppErr("not a member"))

	require.False(t, p.canApproveTeamAdminRequest(nil, "team1"))
	require.True(t, p.canApproveTeamAdminRequest(&model.User{Id: "sys1", Roles: "system_user system_admin"}, "team1"))
	require.True(t, p.canApproveTeamAdminRequest(&model.User{Id: "ta1", Roles: "system_user"}, "team1"))
	require.False(t, p.canApproveTeamAdminRequest(&model.User{Id: "member1", Roles: "system_user"}, "team1"))
	require.False(t, p.canApproveTeamAdminRequest(&model.User{Id: "appta", Roles: "system_user"}, "team1"))
	require.False(t, p.canApproveTeamAdminRequest(&model.User{Id: "ta1", Roles: "system_user"}, ""))
}

// runTeamAdminAction clicks Approve or Deny on request r1's card as userID.
func runTeamAdminAction(t *testing.T, p *Plugin, userID string, approve bool) model.PostActionIntegrationResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, routeApproveTeamAdmin, strings.NewReader(actionBody(t, "r1")))
	r.Header.Set(headerUserID, userID)
	w := httptest.NewRecorder()
	p.handleTeamAdminAction(w, r, approve)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func TestHandleTeamAdminAction_OtherTeamAdminIsRefused(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowTeamAdminRequests: true, AllowTeamAdminApprovers: true, ApprovalTeam: "appteam"})

	raw, err := json.Marshal(&teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{testNomineeID}, ApprovalPostID: "post1"})
	require.NoError(t, err)
	api.On("GetUser", "appta").Return(&model.User{Id: "appta", Username: "appta", Roles: "system_user"}, nil)
	api.On("KVGet", kvTeamAdminRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeamMember", "team1", "appta").Return(nil, testAppErr("not a member"))

	resp := runTeamAdminAction(t, p, "appta", true)

	require.Contains(t, resp.EphemeralText, "team's Team Admins")
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "UpdateTeamMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestHandleTeamAdminAction_TeamAdminCannotApproveOwnRequest(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowTeamAdminRequests: true})

	raw, err := json.Marshal(&teamAdminRequest{ID: "r1", RequesterID: "ta1", TeamID: "team1", NomineeIDs: []string{testNomineeID}, ApprovalPostID: "post1"})
	require.NoError(t, err)
	api.On("GetUser", "ta1").Return(&model.User{Id: "ta1", Username: "lead", Roles: "system_user"}, nil)
	api.On("KVGet", kvTeamAdminRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeamMember", "team1", "ta1").Return(&model.TeamMember{TeamId: "team1", UserId: "ta1", SchemeAdmin: true}, nil)

	resp := runTeamAdminAction(t, p, "ta1", true)

	require.Equal(t, ownRequestText, resp.EphemeralText)
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "UpdateTeamMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestHandleTeamAdminAction_TeamAdminApproves(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowTeamAdminRequests: true})

	raw, err := json.Marshal(&teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{testNomineeID}, ApprovalPostID: "post1"})
	require.NoError(t, err)
	api.On("GetUser", "ta1").Return(&model.User{Id: "ta1", Username: "lead", Roles: "system_user"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer"}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("KVGet", kvTeamAdminRequestPrefix+"r1").Return(raw, nil)
	api.On("GetTeamMember", "team1", "ta1").Return(&model.TeamMember{TeamId: "team1", UserId: "ta1", SchemeAdmin: true}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Sales"}, nil)
	api.On("KVCompareAndDelete", kvTeamAdminRequestPrefix+"r1", raw).Return(true, nil)
	api.On("CreateTeamMember", "team1", testNomineeID).Return(&model.TeamMember{}, nil)
	api.On("UpdateTeamMemberRoles", "team1", testNomineeID, teamAdminRoles).Return(&model.TeamMember{}, nil)
	// Ticket thread, audit and card bookkeeping are not under test here.
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("KVGet", mock.Anything).Return(nil, nil).Maybe()
	api.On("KVDelete", mock.Anything).Return(nil).Maybe()
	api.On("GetTeamByName", mock.Anything).Return(nil, testAppErr("no approval team")).Maybe()
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "req-dm"}, nil).Maybe()
	api.On("GetPost", mock.Anything).Return(nil, testAppErr("no post")).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dm-post"}, nil).Maybe()

	resp := runTeamAdminAction(t, p, "ta1", true)

	require.Empty(t, resp.EphemeralText)
	require.NotNil(t, resp.Update)
	api.AssertCalled(t, "UpdateTeamMemberRoles", "team1", testNomineeID, teamAdminRoles)
}
