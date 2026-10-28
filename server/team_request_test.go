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

func TestSubmitTeamCreationRequest_DisabledWhenToggleOff(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{AllowTeamRequests: false})
	_, err := p.submitTeamCreationRequest(teamCreationInput{RequesterID: "u_req", DisplayName: "Marketing"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "disabled")
}

func TestSubmitTeamCreationRequest_RequiresDisplayName(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{AllowTeamRequests: true})
	_, err := p.submitTeamCreationRequest(teamCreationInput{RequesterID: "u_req", DisplayName: "   "})
	require.Error(t, err)
	require.Contains(t, err.Error(), "team name is required")
}

func TestSubmitTeamCreationRequest_InvalidURLName(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{AllowTeamRequests: true})
	// "!!!" slugifies to "" (all special chars stripped), which fails IsValidTeamName.
	_, err := p.submitTeamCreationRequest(teamCreationInput{
		RequesterID: "u_req",
		DisplayName: "!!!",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a valid team URL name")
}

func TestSubmitTeamCreationRequest_StoresAndPostsRequest(t *testing.T) {
	api := &plugintest.API{}
	stubPendingLists(api)
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowTeamRequests: true})

	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	// ensureTeamCreationApprovalChannel routes team creation to its own channel.
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", DisplayName: "App Team"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-team-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "teams-ch", TeamId: "appteam-id"}, nil)

	// systemAdminMentions + forceTicketChannelCategory share GetUsers.
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sys1", Username: "root"}}, nil)
	api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "root"}, nil)
	api.On("GetChannelSidebarCategories", "sys1", "appteam-id").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"teams-ch"},
		}},
	}, nil).Maybe()

	var approvalPost *model.Post
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post.ChannelId == "teams-ch"
	})).Run(func(args mock.Arguments) {
		approvalPost = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "approval-post-1"}, nil)

	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "req-dm"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dm-post-1"}, nil)

	msg, err := p.submitTeamCreationRequest(teamCreationInput{
		RequesterID: "u_req",
		DisplayName: "Marketing",
		Type:        "O",
	})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	require.NotNil(t, approvalPost)
	require.Equal(t, "teams-ch", approvalPost.ChannelId)
	require.Contains(t, approvalPost.Message, "@root")
	api.AssertCalled(t, "KVSet", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "CreateTeam", mock.Anything)
}

func TestCreateTeamForRequest_AddsRequesterAsMember(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateTeam", mock.Anything).Return(&model.Team{Id: "new-team", Name: "eng", DisplayName: "Engineering"}, nil)
	api.On("CreateTeamMember", "new-team", "u_req").Return(&model.TeamMember{}, nil)

	team, err := p.createTeamForRequest(&teamCreationRequest{
		RequesterID: "u_req",
		Name:        "eng",
		DisplayName: "Engineering",
		Type:        "O",
	})

	require.NoError(t, err)
	require.Equal(t, "new-team", team.Id)
	api.AssertCalled(t, "CreateTeamMember", "new-team", "u_req")
	// No role promotion when RequestTeamAdmin is false.
	api.AssertNotCalled(t, "UpdateTeamMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestCreateTeamForRequest_PromotesRequesterWhenFlagSet(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateTeam", mock.Anything).Return(&model.Team{Id: "new-team", Name: "eng", DisplayName: "Engineering"}, nil)
	api.On("CreateTeamMember", "new-team", "u_req").Return(&model.TeamMember{}, nil)
	api.On("UpdateTeamMemberRoles", "new-team", "u_req", teamAdminRoles).Return(&model.TeamMember{}, nil)

	team, err := p.createTeamForRequest(&teamCreationRequest{
		RequesterID:      "u_req",
		Name:             "eng",
		DisplayName:      "Engineering",
		Type:             "O",
		RequestTeamAdmin: true,
	})

	require.NoError(t, err)
	require.Equal(t, "new-team", team.Id)
	api.AssertCalled(t, "UpdateTeamMemberRoles", "new-team", "u_req", teamAdminRoles)
}

func TestPostTeamCreationApprovalRequest_SeedsApprovalTeamAdminsWhenEnabled(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowTeamRequests: true, AllowApprovalTeamAdminsForTeamRequests: true})

	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", DisplayName: "App Team"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-team-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "teams-ch", TeamId: "appteam-id"}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "appteam-id", 0, 200).Return([]*model.TeamMember{
		{TeamId: "appteam-id", UserId: "appta", SchemeAdmin: true},
		{TeamId: "appteam-id", UserId: "member1"},
	}, nil)
	api.On("AddChannelMember", "teams-ch", "appta").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelSidebarCategories", "appta", "appteam-id").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"teams-ch"},
		}},
	}, nil)
	api.On("GetUser", "appta").Return(&model.User{Id: "appta", Username: "lead"}, nil)
	api.On("GetUsersByIds", []string{"appta"}).Return([]*model.User{{Id: "appta", Username: "lead"}}, nil)
	api.On("KVSet", kvApprovalCardsPrefix+"r1", mock.Anything).Return(nil)

	var approvalPost *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		approvalPost = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "approval-post-1"}, nil)

	req := &teamCreationRequest{ID: "r1", RequesterID: "u_req", DisplayName: "Marketing", Name: "marketing", Type: "O"}
	_, channelID, err := p.postTeamCreationApprovalRequest(req, &model.User{Id: "u_req", Username: "reqer"})

	require.NoError(t, err)
	require.Equal(t, "teams-ch", channelID)
	require.Contains(t, approvalPost.Message, "@lead")
	api.AssertNotCalled(t, "AddChannelMember", "teams-ch", "member1")
}

func TestCanApproveTeamRequest(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetTeamMember", "appteam-id", "appta").Return(&model.TeamMember{TeamId: "appteam-id", UserId: "appta", SchemeAdmin: true}, nil)
	api.On("GetTeamMember", "appteam-id", "member1").Return(&model.TeamMember{TeamId: "appteam-id", UserId: "member1", Roles: "team_user"}, nil)
	sysAdmin := &model.User{Id: "sys1", Roles: "system_user system_admin"}
	appTA := &model.User{Id: "appta", Roles: "system_user"}

	// AllowTeamAdminApprovers covers channel requests only.
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowTeamAdminApprovers: true})
	require.False(t, p.canApproveTeamRequest(nil))
	require.True(t, p.canApproveTeamRequest(sysAdmin))
	require.False(t, p.canApproveTeamRequest(appTA))

	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowApprovalTeamAdminsForTeamRequests: true})
	require.True(t, p.canApproveTeamRequest(appTA))
	require.False(t, p.canApproveTeamRequest(&model.User{Id: "member1", Roles: "system_user"}))
}

// runTeamAction clicks Approve or Deny on team request r1's card as userID.
func runTeamAction(t *testing.T, p *Plugin, userID string, approve bool) model.PostActionIntegrationResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, routeApproveTeam, strings.NewReader(actionBody(t, "r1")))
	r.Header.Set(headerUserID, userID)
	w := httptest.NewRecorder()
	p.handleTeamAction(w, r, approve)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func TestHandleTeamAction_ApprovalTeamAdminRefusedWhenOff(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowTeamRequests: true, AllowTeamAdminApprovers: true})

	api.On("GetUser", "appta").Return(&model.User{Id: "appta", Username: "lead", Roles: "system_user"}, nil)

	resp := runTeamAction(t, p, "appta", true)

	require.Contains(t, resp.EphemeralText, "aren't allowed")
	api.AssertNotCalled(t, "KVGet", kvTeamRequestPrefix+"r1")
	api.AssertNotCalled(t, "CreateTeam", mock.Anything)
}

func TestHandleTeamAction_ApprovalTeamAdminCannotApproveOwnRequest(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowTeamRequests: true, AllowApprovalTeamAdminsForTeamRequests: true})

	raw, err := json.Marshal(&teamCreationRequest{ID: "r1", RequesterID: "appta", DisplayName: "Marketing", Name: "marketing", Type: "O", ApprovalPostID: "post1"})
	require.NoError(t, err)
	api.On("GetUser", "appta").Return(&model.User{Id: "appta", Username: "lead", Roles: "system_user"}, nil)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetTeamMember", "appteam-id", "appta").Return(&model.TeamMember{TeamId: "appteam-id", UserId: "appta", SchemeAdmin: true}, nil)
	api.On("KVGet", kvTeamRequestPrefix+"r1").Return(raw, nil)

	resp := runTeamAction(t, p, "appta", true)

	require.Equal(t, ownRequestText, resp.EphemeralText)
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "CreateTeam", mock.Anything)
}

func TestTeamScopeApprovalHeader_ExerciseMentionsNobody(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam"})
	ch := &model.Channel{Id: "teams-ch", TeamId: "appteam-id"}

	header := p.teamScopeApprovalHeader(ch, "Team Admin request", true)

	require.Equal(t, exerciseMarker+": a Team Admin request needs System Admin review.", header)
	api.AssertNotCalled(t, "GetUsers", mock.Anything)
}
