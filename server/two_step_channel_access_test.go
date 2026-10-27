package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestAddUserToTwoStepApprovalChannels_AddsTeamAndChannelMembers(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		ApprovalTeam:          "appteam",
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
	})

	team := &model.Team{Id: "tid", Name: "appteam"}
	botCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "bot-ch", TeamId: "tid", Name: "mattermost-bot-requests"}
	whCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "wh-ch", TeamId: "tid", Name: "mattermost-webhook-requests"}

	api.On("GetTeamByName", "appteam").Return(team, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "tid", "mattermost-bot-requests", true).Return(botCh, nil)
	api.On("GetChannelByName", "tid", "mattermost-webhook-requests", true).Return(whCh, nil)
	api.On("CreateTeamMember", "tid", "groot").Return(&model.TeamMember{}, nil).Twice()
	api.On("AddChannelMember", "bot-ch", "groot").Return(&model.ChannelMember{}, nil).Once()
	api.On("AddChannelMember", "wh-ch", "groot").Return(&model.ChannelMember{}, nil).Once()
	// Sidebar category helpers may call GetChannel / GetUsers / etc.
	api.On("GetChannel", mock.Anything).Return(botCh, nil).Maybe()
	api.On("GetChannelSidebarCategories", mock.Anything, mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("CreateChannelSidebarCategory", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	p.addUserToTwoStepApprovalChannels("groot")

	api.AssertCalled(t, "AddChannelMember", "bot-ch", "groot")
	api.AssertCalled(t, "AddChannelMember", "wh-ch", "groot")
}

func TestSyncTwoStepApproverChannels_SeedsSecurityGroupMembers(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		ApprovalTeam:          "appteam",
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
		SystemApproverGroup:   "system-approvers",
	})

	secName := "security-approvers"
	team := &model.Team{Id: "tid", Name: "appteam"}
	botCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "bot-ch", TeamId: "tid", Name: "mattermost-bot-requests"}
	whCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "wh-ch", TeamId: "tid", Name: "mattermost-webhook-requests"}
	secGroup := &model.Group{Id: "g-sec", Name: &secName, Source: model.GroupSourceLdap, DeleteAt: 0}

	api.On("GetTeamByName", "appteam").Return(team, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "tid", "mattermost-bot-requests", true).Return(botCh, nil)
	api.On("GetChannelByName", "tid", "mattermost-webhook-requests", true).Return(whCh, nil)
	api.On("GetGroupByName", "security-approvers").Return(secGroup, nil)
	api.On("GetGroupByName", "system-approvers").Return(nil, testAppErr("missing")).Maybe()
	api.On("GetGroups", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]*model.Group{}, nil).Maybe()
	api.On("GetGroupsBySource", mock.Anything).Return([]*model.Group{}, nil).Maybe()
	// The synced group is linked to the approval team and channels.
	api.On("UpsertGroupSyncable", mock.Anything).Return(&model.GroupSyncable{}, nil).Maybe()
	api.On("CreateDefaultSyncableMemberships", mock.Anything).Return(nil).Maybe()
	api.On("GetGroupMemberUsers", "g-sec", 0, 100).Return([]*model.User{{Id: "groot"}, {Id: "dual"}}, nil)
	// system-approvers missing → user-scan fallback
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("CreateTeamMember", "tid", mock.Anything).Return(&model.TeamMember{}, nil).Maybe()
	api.On("AddChannelMember", mock.Anything, mock.Anything).Return(&model.ChannelMember{}, nil).Maybe()
	api.On("GetChannel", mock.Anything).Return(botCh, nil).Maybe()
	api.On("GetChannelSidebarCategories", mock.Anything, mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("CreateChannelSidebarCategory", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	api.On("GetChannelMembers", mock.Anything, mock.Anything, mock.Anything).Return(model.ChannelMembers{}, nil).Maybe()

	p.syncTwoStepApproverChannels()

	api.AssertCalled(t, "AddChannelMember", "bot-ch", "groot")
	api.AssertCalled(t, "AddChannelMember", "bot-ch", "dual")
}

func TestListApproverGroupMemberIDs_IgnoresCustomGroups(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	secName := "security-approvers"
	custom := &model.Group{Id: "g-custom", Name: &secName, Source: model.GroupSourceCustom}
	api.On("GetGroupByName", "security-approvers").Return(custom, nil)
	api.On("GetGroups", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]*model.Group{custom}, nil)
	api.On("GetGroupsBySource", model.GroupSourceLdap).Return([]*model.Group{}, nil)

	require.Empty(t, p.listApproverGroupMemberIDs("@security-approvers"))
	api.AssertNotCalled(t, "GetGroupMemberUsers", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "GetUsers", mock.Anything)
}

func TestReconcileTwoStepApproverChannels_UsesGroupMembersOnly(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.setConfiguration(&configuration{
		ApprovalTeam:          "appteam",
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
		SystemApproverGroup:   "system-approvers",
	})

	secName := "security-approvers"
	team := &model.Team{Id: "tid", Name: "appteam"}
	botCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot", Id: "bot-ch", TeamId: "tid", Name: "mattermost-bot-requests"}
	whCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot", Id: "wh-ch", TeamId: "tid", Name: "mattermost-webhook-requests"}
	secGroup := &model.Group{Id: "g-sec", Name: &secName, Source: model.GroupSourceLdap, DeleteAt: 0}

	api.On("GetTeamByName", "appteam").Return(team, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "tid", "mattermost-bot-requests", true).Return(botCh, nil)
	api.On("GetChannelByName", "tid", "mattermost-webhook-requests", true).Return(whCh, nil)
	api.On("GetGroupByName", "security-approvers").Return(secGroup, nil)
	api.On("GetGroupByName", "system-approvers").Return(nil, testAppErr("missing")).Maybe()
	api.On("GetGroups", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]*model.Group{}, nil).Maybe()
	api.On("GetGroupsBySource", mock.Anything).Return([]*model.Group{}, nil).Maybe()
	api.On("GetGroupMemberUsers", "g-sec", 0, 100).Return([]*model.User{{Id: "readded"}}, nil)
	// Must not fall back to GetUsers on the light path.
	api.On("CreateTeamMember", "tid", "readded").Return(&model.TeamMember{}, nil).Maybe()
	api.On("AddChannelMember", "bot-ch", "readded").Return(&model.ChannelMember{}, nil).Once()
	api.On("AddChannelMember", "wh-ch", "readded").Return(&model.ChannelMember{}, nil).Once()
	api.On("GetChannel", mock.Anything).Return(botCh, nil).Maybe()
	api.On("GetChannelSidebarCategories", mock.Anything, mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("CreateChannelSidebarCategory", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	// Eviction pass: channel already only has the pool member + bot.
	api.On("GetChannelMembers", "bot-ch", 0, 100).Return(model.ChannelMembers{
		{UserId: "readded"}, {UserId: "bot"},
	}, nil)
	api.On("GetChannelMembers", "wh-ch", 0, 100).Return(model.ChannelMembers{
		{UserId: "readded"}, {UserId: "bot"},
	}, nil)
	api.On("GetUser", "readded").Return(&model.User{Id: "readded"}, nil).Maybe()
	api.On("GetTeamMember", "tid", "readded").Return(&model.TeamMember{UserId: "readded", Roles: model.TeamUserRoleId}, nil).Maybe()
	api.On("GetGroupsForUser", "readded").Return([]*model.Group{{Id: "g-sec", Name: &secName}}, nil).Maybe()

	p.reconcileTwoStepApproverChannels()

	api.AssertCalled(t, "AddChannelMember", "bot-ch", "readded")
	api.AssertCalled(t, "AddChannelMember", "wh-ch", "readded")
	api.AssertNotCalled(t, "GetUsers", mock.Anything)
	api.AssertNotCalled(t, "DeleteChannelMember", mock.Anything, mock.Anything)
}

func TestEvictStaleTwoStepApproverChannelMembers_RemovesDemotedKeepsAdmins(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.setConfiguration(&configuration{
		ApprovalTeam:          "appteam",
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
	})

	secName := "security-approvers"
	team := &model.Team{Id: "tid", Name: "appteam"}
	botCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot", Id: "bot-ch", TeamId: "tid", Name: "mattermost-bot-requests"}
	whCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot", Id: "wh-ch", TeamId: "tid", Name: "mattermost-webhook-requests"}

	api.On("GetTeamByName", "appteam").Return(team, nil)
	stubApprovalTeamRecord(api)
	stubSyncedGroup(api, "security-approvers", "g-sec")
	stubSyncedGroup(api, "system-approvers", "g-sys")
	api.On("GetChannelByName", "tid", "mattermost-bot-requests", true).Return(botCh, nil)
	api.On("GetChannelByName", "tid", "mattermost-webhook-requests", true).Return(whCh, nil)

	api.On("GetChannelMembers", "bot-ch", 0, 100).Return(model.ChannelMembers{
		{UserId: "bot"},
		{UserId: "demoted"},
		{UserId: "sysadmin"},
		{UserId: "teamadmin"},
		{UserId: "still-in"},
	}, nil)
	api.On("GetChannelMembers", "wh-ch", 0, 100).Return(model.ChannelMembers{}, nil)

	api.On("GetUser", "demoted").Return(&model.User{Id: "demoted", Username: "demoted"}, nil)
	api.On("GetUser", "sysadmin").Return(&model.User{Id: "sysadmin", Username: "sys", Roles: model.SystemAdminRoleId}, nil)
	api.On("GetUser", "teamadmin").Return(&model.User{Id: "teamadmin", Username: "tadmin"}, nil)
	api.On("GetUser", "still-in").Return(&model.User{Id: "still-in", Username: "ok"}, nil)

	api.On("GetTeamMember", "tid", "demoted").Return(&model.TeamMember{UserId: "demoted", Roles: model.TeamUserRoleId}, nil)
	api.On("GetTeamMember", "tid", "teamadmin").Return(&model.TeamMember{UserId: "teamadmin", SchemeAdmin: true, Roles: model.TeamAdminRoleId}, nil)
	api.On("GetTeamMember", "tid", "still-in").Return(&model.TeamMember{UserId: "still-in", Roles: model.TeamUserRoleId}, nil).Maybe()
	api.On("GetTeamMember", "tid", "sysadmin").Return(&model.TeamMember{UserId: "sysadmin", Roles: model.TeamUserRoleId}, nil).Maybe()

	api.On("GetGroupsForUser", "demoted").Return([]*model.Group{}, nil)
	api.On("GetGroupsForUser", "still-in").Return([]*model.Group{{Id: "g-sec", Name: &secName}}, nil)
	api.On("GetGroupsForUser", "teamadmin").Return([]*model.Group{}, nil).Maybe()

	api.On("DeleteChannelMember", "bot-ch", "demoted").Return(nil).Once()

	p.evictStaleTwoStepApproverChannelMembers()

	api.AssertCalled(t, "DeleteChannelMember", "bot-ch", "demoted")
	api.AssertNotCalled(t, "DeleteChannelMember", "bot-ch", "sysadmin")
	api.AssertNotCalled(t, "DeleteChannelMember", "bot-ch", "teamadmin")
	api.AssertNotCalled(t, "DeleteChannelMember", "bot-ch", "still-in")
	api.AssertNotCalled(t, "DeleteChannelMember", "bot-ch", "bot")
}

func TestReconcileTwoStepAccessForUser_EvictsWhenDemoted(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		ApprovalTeam:          "appteam",
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
	})

	team := &model.Team{Id: "tid", Name: "appteam"}
	botCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "bot-ch", TeamId: "tid", Name: "mattermost-bot-requests"}
	whCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "wh-ch", TeamId: "tid", Name: "mattermost-webhook-requests"}

	api.On("GetTeamByName", "appteam").Return(team, nil)
	stubApprovalTeamRecord(api)
	stubSyncedGroup(api, "security-approvers", "g-sec")
	stubSyncedGroup(api, "system-approvers", "g-sys")
	api.On("GetChannelByName", "tid", "mattermost-bot-requests", true).Return(botCh, nil)
	api.On("GetChannelByName", "tid", "mattermost-webhook-requests", true).Return(whCh, nil)
	api.On("GetTeamMember", "tid", "demoted").Return(&model.TeamMember{UserId: "demoted", Roles: model.TeamUserRoleId}, nil)
	api.On("GetGroupsForUser", "demoted").Return([]*model.Group{}, nil)
	api.On("DeleteChannelMember", "bot-ch", "demoted").Return(nil).Once()
	api.On("DeleteChannelMember", "wh-ch", "demoted").Return(nil).Once()

	p.reconcileTwoStepAccessForUser(&model.User{Id: "demoted", Username: "demoted"})

	api.AssertCalled(t, "DeleteChannelMember", "bot-ch", "demoted")
	api.AssertCalled(t, "DeleteChannelMember", "wh-ch", "demoted")
}

func TestEnsureTwoStepApproverChannelAccess_SecurityGroupMember(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		ApprovalTeam:          "appteam",
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
	})

	secName := "security-approvers"
	team := &model.Team{Id: "tid", Name: "appteam"}
	botCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "bot-ch", TeamId: "tid", Name: "mattermost-bot-requests"}
	whCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "wh-ch", TeamId: "tid", Name: "mattermost-webhook-requests"}

	api.On("GetGroupsForUser", "groot").Return([]*model.Group{{Id: "g-sec", Name: &secName}}, nil)
	api.On("GetTeamMember", "tid", "groot").Return(&model.TeamMember{UserId: "groot", Roles: model.TeamUserRoleId}, nil).Maybe()
	api.On("GetTeamByName", "appteam").Return(team, nil)
	stubApprovalTeamRecord(api)
	stubSyncedGroup(api, "security-approvers", "g-sec")
	stubSyncedGroup(api, "system-approvers", "g-sys")
	api.On("GetChannelByName", "tid", "mattermost-bot-requests", true).Return(botCh, nil)
	api.On("GetChannelByName", "tid", "mattermost-webhook-requests", true).Return(whCh, nil)
	api.On("CreateTeamMember", "tid", "groot").Return(&model.TeamMember{}, nil).Maybe()
	api.On("AddChannelMember", "bot-ch", "groot").Return(&model.ChannelMember{}, nil).Once()
	api.On("AddChannelMember", "wh-ch", "groot").Return(&model.ChannelMember{}, nil).Once()
	api.On("GetChannel", mock.Anything).Return(botCh, nil).Maybe()
	api.On("GetChannelSidebarCategories", mock.Anything, mock.Anything).Return(nil, testAppErr("none")).Maybe()
	api.On("CreateChannelSidebarCategory", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	p.ensureTwoStepApproverChannelAccess(&model.User{Id: "groot", Username: "groot"})

	api.AssertCalled(t, "AddChannelMember", "bot-ch", "groot")
	api.AssertCalled(t, "AddChannelMember", "wh-ch", "groot")
}

func TestListApproverAttributeUserIDs_AffirmativeOnly(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	field := &model.PropertyField{
		ID: "fld-security_approver", GroupID: "grp", Name: "security_approver",
		Type: model.PropertyFieldTypeText,
		Attrs: model.StringInterface{
			model.CustomProfileAttributesPropertyAttrsManaged: "admin",
		},
	}
	api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
	api.On("GetPropertyFieldByName", "grp", "", "security_approver").Return(field, nil)
	api.On("SearchPropertyValues", "grp", mock.Anything).Return([]*model.PropertyValue{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", TargetID: "uyes01aaaaaaaaaaaaaaaaaaaa", Value: json.RawMessage(`"yes"`), CreateAt: 1},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbb", TargetID: "uno02aaaaaaaaaaaaaaaaaaaaa", Value: json.RawMessage(`"no"`), CreateAt: 2},
		{ID: "cccccccccccccccccccccccccc", TargetID: "uemptyaaaaaaaaaaaaaaaaaaaa", Value: json.RawMessage(`""`), CreateAt: 3},
	}, nil)

	ids := p.listApproverAttributeUserIDs("security_approver")
	require.Equal(t, []string{"uyes01aaaaaaaaaaaaaaaaaaaa"}, ids)
}
