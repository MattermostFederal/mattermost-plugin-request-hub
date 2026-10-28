package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestPromoteChannelAdmins_PromotesEachMemberNominee(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetChannelMember", "ch1", mock.Anything).Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", mock.Anything, channelAdminRoles).Return(&model.ChannelMember{}, nil)

	promoted := p.promoteChannelAdmins(&adminRequest{ChannelID: "ch1", NomineeIDs: []string{"u1", "u2"}})

	require.Equal(t, []string{"u1", "u2"}, promoted)
	api.AssertNumberOfCalls(t, "UpdateChannelMemberRoles", 2)
	api.AssertCalled(t, "UpdateChannelMemberRoles", "ch1", "u1", channelAdminRoles)
	api.AssertCalled(t, "UpdateChannelMemberRoles", "ch1", "u2", channelAdminRoles)
	// Promotion never adds anyone to the channel.
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestPromoteChannelAdmins_SkipsNonMembersAndFailedPromotions(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	// u_left left the channel while the request was pending; u_fail is a
	// member but the role update fails. Neither may be reported as promoted.
	api.On("GetChannelMember", "ch1", "u_ok").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelMember", "ch1", "u_left").Return(nil, testAppErr("not a member"))
	api.On("GetChannelMember", "ch1", "u_fail").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_ok", channelAdminRoles).Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_fail", channelAdminRoles).Return(nil, testAppErr("boom"))

	promoted := p.promoteChannelAdmins(&adminRequest{ChannelID: "ch1", NomineeIDs: []string{"u_ok", "u_left", "u_fail"}})

	require.Equal(t, []string{"u_ok"}, promoted)
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", "ch1", "u_left", mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestSubmitAdminRequest_ValidationErrors(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})

	_, err := p.submitAdminRequest("u_req", "", []string{"u1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "channel is required")

	_, err = p.submitAdminRequest("u_req", "ch1", []string{"", "  "})
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least one person")
}

func TestSubmitAdminRequest_PostsToApprovalChannelMentioningApprovers(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	stubSubmittedRequestExists(api, kvAdminRequestPrefix)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam"})

	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "marketing", TeamId: "team1", Type: model.ChannelTypePrivate}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelMember", "ch1", testNomineeID).Return(&model.ChannelMember{}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	// Approval destination + approver set: one System Admin + one Team Admin
	// of the target channel's team.
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", Name: "appteam"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-channel-requests", true).Return(pluginChannel("appch", "mattermost-channel-requests"), nil)
	api.On("GetUsers", isSystemAdminQuery).Return([]*model.User{{Id: "sys1"}}, nil)
	api.On("GetTeamMembers", "team1", mock.Anything, mock.Anything).Return([]*model.TeamMember{
		{TeamId: "team1", UserId: "ta1", Roles: "team_user team_admin"},
		{TeamId: "team1", UserId: "tu1", Roles: "team_user"},
	}, nil)
	api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "root"}, nil)
	api.On("GetUser", "ta1").Return(&model.User{Id: "ta1", Username: "lead"}, nil)
	api.On("GetUsersByIds", []string{"ta1"}).Return([]*model.User{{Id: "ta1", Username: "lead"}}, nil)

	// Capture only the post going to the approval channel so the requester DM
	// (a second CreatePost call) doesn't overwrite the assertion target.
	var posted *model.Post
	api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool {
		return p.ChannelId == "appch"
	})).Run(func(args mock.Arguments) {
		posted = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "appch-post"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)

	// sendTicketCreatedDM opens a DM with the requester to start their ticket thread.
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "req-dm"}, nil)

	msg, err := p.submitAdminRequest("u_req", "ch1", []string{testNomineeID})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	api.AssertCalled(t, "KVSet", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)

	// Posted to the approval channel, mentioning the System Admin + Team Admin
	// (not the plain team member).
	require.NotNil(t, posted)
	require.Equal(t, "appch", posted.ChannelId)
	require.Contains(t, posted.Message, "@root")
	require.Contains(t, posted.Message, "@lead")
	// The card names the target team, since ~links resolve in the viewer's team.
	attachments := posted.Attachments()
	require.Len(t, attachments, 1)
	var teamField string
	for _, f := range attachments[0].Fields {
		if f.Title == "Team" {
			teamField, _ = f.Value.(string)
		}
	}
	require.Contains(t, teamField, "Engineering")
}

func TestSubmitAdminRequest_NoApprovalChannelFallsBackToSystemAdmins(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	stubSubmittedRequestExists(api, kvAdminRequestPrefix)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	// No approval channel configured -> DM every System Admin, mirroring the
	// channel-creation flow, rather than dropping the request.
	p.setConfiguration(&configuration{})

	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "marketing", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Team One"}, nil).Maybe()
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelMember", "ch1", testNomineeID).Return(&model.ChannelMember{}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	// adminApprovalHeader also lists approvers; supply one System Admin who is
	// both an approver mention and a DM recipient.
	api.On("GetUsers", isSystemAdminQuery).Return([]*model.User{{Id: "sys1", Username: "root"}}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil)
	api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "root"}, nil)

	// Capture only the post going to the system-admin DM so the requester DM
	// (a second CreatePost call) doesn't overwrite the assertion target.
	var dmPost *model.Post
	api.On("GetDirectChannel", "sys1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "req-dm"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(p *model.Post) bool {
		return p.ChannelId == "dm1"
	})).Run(func(args mock.Arguments) {
		dmPost = args.Get(0).(*model.Post)
	}).Return(&model.Post{}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)

	msg, err := p.submitAdminRequest("u_req", "ch1", []string{testNomineeID})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	// The request was DMed to the System Admin, not dropped.
	require.NotNil(t, dmPost)
	require.Equal(t, "dm1", dmPost.ChannelId)
	api.AssertCalled(t, "GetDirectChannel", "sys1", "bot-user-id")
}

func TestSubmitAdminRequest_RejectsNonChannelMember(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	// The requester is not a member of the channel.
	api.On("GetChannelMember", "ch1", "u_req").Return(nil, testAppErr("not a member"))

	_, err := p.submitAdminRequest("u_req", "ch1", []string{testNomineeID})

	require.Error(t, err)
	require.Equal(t, "channel not found, or you are not a member of it", err.Error())
	// The channel isn't loaded, and nothing is promoted or stored.
	api.AssertNotCalled(t, "GetChannel", mock.Anything)
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestSubmitAdminRequest_MissingChannelLooksLikeNonMember(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	// A System Admin skips the membership check, so the channel lookup runs.
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user system_admin"}, nil)
	api.On("GetChannel", "ch1").Return(nil, testAppErr("app.channel.get.existing.app_error id=ch1"))

	_, err := p.submitAdminRequest("u_req", "ch1", []string{testNomineeID})

	require.Error(t, err)
	require.Equal(t, "channel not found, or you are not a member of it", err.Error())
}

func TestSubmitAdminRequest_RejectsNomineeWhoIsNotAChannelMember(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "planning", TeamId: "team1", Type: model.ChannelTypePrivate}, nil)
	api.On("GetChannelMember", "ch1", "nonmemberuseridaaaaaaaaaaa").Return(nil, testAppErr("not a member"))

	_, err := p.submitAdminRequest("u_req", "ch1", []string{"nonmemberuseridaaaaaaaaaaa"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "already be a member of the channel")
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestSubmitAdminRequest_RejectsDirectAndArchivedChannels(t *testing.T) {
	for name, channel := range map[string]*model.Channel{
		"direct message": {Id: "ch1", Type: model.ChannelTypeDirect},
		"group message":  {Id: "ch1", Type: model.ChannelTypeGroup},
		"archived":       {Id: "ch1", Type: model.ChannelTypeOpen, DeleteAt: 1},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)

			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
			api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
			api.On("GetChannel", "ch1").Return(channel, nil)

			_, err := p.submitAdminRequest("u_req", "ch1", []string{testNomineeID})

			require.Error(t, err)
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}

func TestCanApproveAdminRequest_TeamAdminMayApprove(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Team One"}, nil).Maybe()

	// A Team Admin of the target channel's team may approve.
	teamAdmin := &model.User{Id: "u_ta", Username: "lead", Roles: "system_user"}
	api.On("GetTeamMember", "team1", "u_ta").Return(&model.TeamMember{TeamId: "team1", UserId: "u_ta", Roles: "team_user team_admin"}, nil)
	require.True(t, p.canApproveAdminRequest(teamAdmin, "ch1"))

	// A plain team member may not.
	plainMember := &model.User{Id: "u_member", Username: "joe", Roles: "system_user"}
	api.On("GetTeamMember", "team1", "u_member").Return(&model.TeamMember{TeamId: "team1", UserId: "u_member", Roles: "team_user"}, nil)
	require.False(t, p.canApproveAdminRequest(plainMember, "ch1"))

	// A System Admin may approve regardless of team membership.
	sysAdmin := &model.User{Id: "u_sys", Username: "root", Roles: "system_user system_admin"}
	require.True(t, p.canApproveAdminRequest(sysAdmin, "ch1"))
}

func TestCanApproveAdminRequest_ApprovalTeamAdminLimitedToOwnTeam(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	// AllowTeamAdminApprovers applies to channel-creation requests only.
	p.setConfiguration(&configuration{AllowTeamAdminApprovers: true, ApprovalTeam: "appteam"})

	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
	api.On("GetTeamMember", "team1", "u_appadmin").Return(&model.TeamMember{TeamId: "team1", UserId: "u_appadmin", Roles: "team_user"}, nil)

	appTeamAdmin := &model.User{Id: "u_appadmin", Username: "approver", Roles: "system_user"}
	require.False(t, p.canApproveAdminRequest(appTeamAdmin, "ch1"))
	api.AssertNotCalled(t, "GetTeamMember", "appteam", mock.Anything)
}

func TestCanApproveAdminRequest_FormerTeamAdminDenied(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
	// Leaving a team clears Roles but keeps SchemeAdmin on the soft-deleted row.
	api.On("GetTeamMember", "team1", "u_former").Return(&model.TeamMember{TeamId: "team1", UserId: "u_former", SchemeAdmin: true, DeleteAt: 12345}, nil)

	former := &model.User{Id: "u_former", Username: "gone", Roles: "system_user"}
	require.False(t, p.canApproveAdminRequest(former, "ch1"))
}

// mockAdminRequestPrereqs sets up a regular requester u1 who belongs to the
// open channel chX (team1) and nominates testNomineeID, who is also a member.
func mockAdminRequestPrereqs(api *plugintest.API) {
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "req", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "chX", "u1").Return(&model.ChannelMember{}, nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetChannelMember", "chX", testNomineeID).Return(&model.ChannelMember{}, nil)
}

func TestSubmitAdminRequest_RequesterLookupFails(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUser", "u1").Return(nil, testAppErr("gone"))

	_, err := p.submitAdminRequest("u1", "chX", []string{testNomineeID})

	require.EqualError(t, err, "could not load your account; try again")
}

func TestSubmitAdminRequest_StoreFailure(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	mockAdminRequestPrereqs(api)
	api.On("KVSet", mock.Anything, mock.Anything).Return(testAppErr("kv down"))

	_, err := p.submitAdminRequest("u1", "chX", []string{testNomineeID})

	require.EqualError(t, err, "could not save your request; try again")
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestSubmitAdminRequest_DeliveryFailureRollsBack(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{}) // no approval channel: DMs only

	mockAdminRequestPrereqs(api)
	var key string
	api.On("KVSet", mock.Anything, mock.Anything).Run(func(args mock.Arguments) { key = args.String(0) }).Return(nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nom"}, nil)
	// Neither the approver lookup nor the DM fallback can find anyone.
	api.On("GetUsers", mock.Anything).Return(nil, testAppErr("db down"))
	api.On("GetTeamMembers", "team1", 0, 200).Return(nil, testAppErr("db down"))
	api.On("KVDelete", mock.Anything).Return(nil)

	_, err := p.submitAdminRequest("u1", "chX", []string{testNomineeID})

	require.ErrorContains(t, err, "could not send your request to the approvers")
	require.True(t, strings.HasPrefix(key, kvAdminRequestPrefix))
	api.AssertCalled(t, "KVDelete", key)
}

func TestAdminApprovalHeader_NoApproversIsPlainText(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUsers", isSystemAdminQuery).Return([]*model.User{{Id: "bot-user-id", IsBot: true}}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil)

	require.Equal(t, "A Channel Admin request needs your review.", p.adminApprovalHeader(&model.Channel{TeamId: "team1"}))
}

func TestAdminApproverIDs_DedupesAndSkipsNonHumanAccounts(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	remote := "remote1"
	api.On("GetUsers", isSystemAdminQuery).Return([]*model.User{
		{Id: "bot-user-id"},
		{Id: "a1"},
		{Id: "otherbot", IsBot: true},
		{Id: "gone", DeleteAt: 123},
		{Id: "shared", RemoteId: &remote},
	}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{
		{UserId: "a1", Roles: "team_user team_admin"}, // also a System Admin
		{UserId: "t2", SchemeAdmin: true},
		{UserId: "tbot", SchemeAdmin: true},
		{UserId: "tleft", SchemeAdmin: true, DeleteAt: 1},
		{UserId: "tmissing", SchemeAdmin: true},
		{UserId: "u3", Roles: "team_user"},
		{UserId: "", Roles: "team_admin"},
	}, nil)
	// tmissing can't be loaded, so GetUsersByIds leaves it out.
	api.On("GetUsersByIds", []string{"t2", "tbot", "tmissing"}).Return([]*model.User{
		{Id: "t2"},
		{Id: "tbot", IsBot: true},
	}, nil)

	require.Equal(t, []string{"a1", "t2"}, p.adminApproverIDs("team1"))
}

func TestAdminApproverIDs_TeamAdminLookupFails(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUsers", isSystemAdminQuery).Return([]*model.User{{Id: "a1"}}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{{UserId: "t2", SchemeAdmin: true}}, nil)
	api.On("GetUsersByIds", []string{"t2"}).Return(nil, testAppErr("db down"))

	require.Equal(t, []string{"a1"}, p.adminApproverIDs("team1"))
	api.AssertCalled(t, "LogWarn", "failed to load Team Admins for admin-request approvers", "team_id", "team1", "error", mock.Anything)
}

func TestAdminApproverIDs_PagesTeamMembers(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	full := make([]*model.TeamMember, 200)
	for i := range full {
		full[i] = &model.TeamMember{UserId: fmt.Sprintf("tu%03d", i), Roles: "team_user"}
	}
	api.On("GetUsers", isSystemAdminQuery).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return(full, nil)
	api.On("GetTeamMembers", "team1", 1, 200).Return([]*model.TeamMember{{UserId: "last", SchemeAdmin: true}}, nil)
	api.On("GetUsersByIds", []string{"last"}).Return([]*model.User{{Id: "last"}}, nil)

	require.Equal(t, []string{"last"}, p.adminApproverIDs("team1"))
}

// isSystemAdminQuery matches the GetUsers call that lists active System Admins.
var isSystemAdminQuery = mock.MatchedBy(func(o *model.UserGetOptions) bool {
	return o.Role == model.SystemAdminRoleId && o.Active
})

func TestLoadAdminRequest_Errors(t *testing.T) {
	t.Run("store read fails", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", kvAdminRequestPrefix+"a1").Return(nil, testAppErr("kv down"))
		_, _, err := p.loadAdminRequest("a1")
		require.ErrorContains(t, err, "failed to load admin request")
	})

	t.Run("corrupt record", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", kvAdminRequestPrefix+"a1").Return([]byte("{"), nil)
		_, _, err := p.loadAdminRequest("a1")
		require.ErrorContains(t, err, "failed to unmarshal admin request")
	})
}

func TestPostAdminPromotionMessage(t *testing.T) {
	t.Run("nobody to name posts nothing", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetUser", "u_nom").Return(nil, testAppErr("gone"))

		p.postAdminPromotionMessage(&model.Channel{Id: "chX"}, &adminRequest{NomineeIDs: []string{"u_nom"}}, nil)

		api.AssertNotCalled(t, "CreatePost", mock.Anything)
	})

	t.Run("post failure is logged", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return post.Message == "@nom is now a Channel Admin for this channel."
		})).Return(nil, testAppErr("post failed"))

		p.postAdminPromotionMessage(&model.Channel{Id: "chX"}, &adminRequest{NomineeIDs: []string{"u_nom"}}, nil)

		api.AssertCalled(t, "LogWarn", "failed to post channel-admin promotion message", "channel_id", "chX", "error", mock.Anything)
	})
}

func TestPromoteChannelAdmins_SkipsEmptyIDs(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetChannelMember", "chX", "u_nom").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "chX", "u_nom", channelAdminRoles).Return(&model.ChannelMember{}, nil)

	got := p.promoteChannelAdmins(&adminRequest{ChannelID: "chX", NomineeIDs: []string{"", "u_nom"}})

	require.Equal(t, []string{"u_nom"}, got)
	api.AssertNotCalled(t, "GetChannelMember", "chX", "")
}

func TestSubmitAdminRequestPendingLimitAndDuplicate(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{maxPending: 1})

	const secondNomineeID = "secondnomineeidaaaaaaaaaaa"
	lists := fakePendingLists(api)
	mockAdminRequestPrereqs(api)
	api.On("GetChannelMember", "chX", secondNomineeID).Return(&model.ChannelMember{}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	// The first request is still stored, so its entry keeps counting.
	api.On("KVGet", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvAdminRequestPrefix) })).Return([]byte("{}"), nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng"}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nom"}, nil)
	api.On("GetUsers", isSystemAdminQuery).Return([]*model.User{{Id: "sys1", Username: "root"}}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil)
	api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "root"}, nil)
	api.On("GetDirectChannel", "sys1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm2"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)

	_, err := p.submitAdminRequest("u1", "chX", []string{testNomineeID})
	require.NoError(t, err)
	require.Len(t, decodeEntries(t, lists[pendingListKey(pendingKindAdmin, "u1")]), 1)

	_, err = p.submitAdminRequest("u1", "chX", []string{testNomineeID})
	require.EqualError(t, err, "you already have a pending Channel Admin request for these people in this channel")

	_, err = p.submitAdminRequest("u1", "chX", []string{secondNomineeID})
	require.ErrorContains(t, err, "you have 1 pending requests waiting for review")
}

func TestSubmitAdminRequestFailureFreesPendingSlot(t *testing.T) {
	for name, setup := range map[string]func(api *plugintest.API){
		"store fails": func(api *plugintest.API) {
			api.On("KVSet", mock.Anything, mock.Anything).Return(testAppErr("kv down"))
		},
		"delivery fails": func(api *plugintest.API) {
			api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
			api.On("KVDelete", mock.Anything).Return(nil)
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng"}, nil)
			api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nom"}, nil)
			api.On("GetUsers", mock.Anything).Return(nil, testAppErr("db down"))
			api.On("GetTeamMembers", "team1", 0, 200).Return(nil, testAppErr("db down"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{})

			lists := fakePendingLists(api)
			mockAdminRequestPrereqs(api)
			setup(api)

			_, err := p.submitAdminRequest("u1", "chX", []string{testNomineeID})
			require.Error(t, err)
			require.Empty(t, lists, "the pending slot is given back")
		})
	}
}

func TestSubmitAdminRequestPendingListUnavailable(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	mockAdminRequestPrereqs(api)
	api.On("KVGet", isPendingListKey).Return(nil, testAppErr("kv down"))

	_, err := p.submitAdminRequest("u1", "chX", []string{testNomineeID})
	require.EqualError(t, err, "could not save your request; try again")
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}
