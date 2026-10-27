package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// logOnlyPlugin returns a plugin with only log stubs, so any API call the
// test didn't register fails the test.
func logOnlyPlugin(cfg *configuration) (*Plugin, *plugintest.API) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	p.setConfiguration(cfg)
	return p, api
}

// pluginChannelIn returns a bot-created private channel in teamID.
func pluginChannelIn(id, name, teamID string) *model.Channel {
	ch := pluginChannel(id, name)
	ch.TeamId = teamID
	return ch
}

// takenChannel is a channel with an approval channel name the plugin
// didn't create.
func takenChannel(id string) *model.Channel {
	return &model.Channel{Id: id, Type: model.ChannelTypeOpen, CreatorId: "u-other"}
}

// repeated returns n copies of v.
func repeated[T any](v T, n int) []T {
	out := make([]T, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// --- resolveApprovalChannel ---

func TestResolveApprovalChannel_NoTeamApproversRoutesToRootWithLabel(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	defer api.AssertExpectations(t)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).
		Return([]*model.TeamMember{{TeamId: "team1", UserId: "u1", Roles: "team_user"}}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops_Team"}, nil)

	ch, label := p.resolveApprovalChannel("team1")

	// No Approval team configured, so the root channel is unavailable too.
	require.Nil(t, ch)
	require.Equal(t, "For Team: Ops\\_Team — ", label)
	api.AssertNotCalled(t, "GetChannelByName", mock.Anything, mock.Anything, mock.Anything)
}

func TestResolveApprovalChannel_NoTeamApproversLabelFallsBackToTeamID(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil)
	api.On("GetTeam", "team1").Return(nil, testAppErr("gone"))

	_, label := p.resolveApprovalChannel("team1")

	require.Equal(t, "For Team: team1 — ", label)
}

// --- hasActiveTeamApprovers / systemAdminIDSet ---

func TestHasActiveTeamApprovers_MemberLookupFailureAssumesMonitored(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetUsers", mock.Anything).Return(nil, testAppErr("down"))
	api.On("GetTeamMembers", "team1", 0, 200).Return(nil, testAppErr("down"))

	require.True(t, p.hasActiveTeamApprovers("team1"))
	api.AssertCalled(t, "LogWarn", "failed to list System Admins for approver check", "error", mock.Anything)
}

func TestHasActiveTeamApprovers_StopsAfterPageLimit(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	plain := repeated(&model.TeamMember{TeamId: "team1", UserId: "u1", Roles: "team_user"}, 200)
	api.On("GetTeamMembers", "team1", mock.Anything, 200).Return(plain, nil)

	require.False(t, p.hasActiveTeamApprovers("team1"))
	api.AssertNumberOfCalls(t, "GetTeamMembers", 10)
}

func TestSystemAdminIDSet_SkipsBotAndBlankEntries(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetUsers", mock.Anything).Return([]*model.User{nil, {Id: ""}, {Id: "bot-user-id"}, {Id: "sa1"}}, nil)

	require.Equal(t, map[string]bool{"sa1": true}, p.systemAdminIDSet())
}

// --- ensureRootApprovalChannel ---

func TestEnsureRootApprovalChannel_CreatesChannelAndSeedsAdmins(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	defer api.AssertExpectations(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", true).Return(nil, testAppErr("not found"))
	created := pluginChannelIn("root", "mattermost-channel-requests", "t1")
	api.On("CreateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.TeamId == "t1" && c.Name == "mattermost-channel-requests" &&
			c.Type == model.ChannelTypePrivate && c.CreatorId == "bot-user-id"
	})).Return(created, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "root" })).
		Return(nil, testAppErr("post failed"))
	api.On("GetChannel", "root").Return(created, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "bot-user-id"}, {Id: "sa1"}}, nil)
	api.On("AddChannelMember", "root", "sa1").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelSidebarCategories", "sa1", "t1").Return(nil, nil)
	api.On("CreateChannelSidebarCategory", "sa1", "t1", mock.MatchedBy(func(c *model.SidebarCategoryWithChannels) bool {
		return c.DisplayName == sidebarCategoryName && len(c.Channels) == 1 && c.Channels[0] == "root"
	})).Return(nil, nil)

	ch := p.ensureRootApprovalChannel()

	require.NotNil(t, ch)
	require.Equal(t, "root", ch.Id)
	api.AssertNotCalled(t, "AddChannelMember", "root", "bot-user-id")
	api.AssertCalled(t, "LogWarn", "failed to post welcome message in root approval channel",
		"channel_id", "root", "error", mock.Anything)
}

func TestEnsureRootApprovalChannel_CreateConflictReturnsExisting(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", true).Return(nil, testAppErr("not found")).Once()
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", true).
		Return(pluginChannelIn("root", "mattermost-channel-requests", "t1"), nil)
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("exists"))

	ch := p.ensureRootApprovalChannel()

	require.NotNil(t, ch)
	require.Equal(t, "root", ch.Id)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestEnsureRootApprovalChannel_CreateFailureReturnsNil(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", true).Return(nil, testAppErr("not found"))
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("denied"))

	require.Nil(t, p.ensureRootApprovalChannel())
	api.AssertNumberOfCalls(t, "GetChannelByName", 2)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

// --- addChannelToPermissionsCategory ---

func TestAddChannelToPermissionsCategory_BlankInputsDoNothing(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	p.addChannelToPermissionsCategory("", "t1", "c1")
	p.addChannelToPermissionsCategory("u1", "", "c1")
	p.addChannelToPermissionsCategory("u1", "t1", "")
	api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
}

func TestAddChannelToPermissionsCategory_LookupFailureStops(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(nil, testAppErr("down"))

	p.addChannelToPermissionsCategory("u1", "t1", "c1")

	api.AssertNotCalled(t, "CreateChannelSidebarCategory", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "UpdateChannelSidebarCategories", mock.Anything, mock.Anything, mock.Anything)
}

func TestAddChannelToPermissionsCategory_RenamesLegacyCategory(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	legacy := &model.SidebarCategoryWithChannels{
		SidebarCategory: model.SidebarCategory{Id: "cat1", DisplayName: "Mattermost: Permissions"},
		Channels:        []string{"c1"},
	}
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{
			nil,
			{SidebarCategory: model.SidebarCategory{DisplayName: "Favorites"}},
			legacy,
		},
	}, nil)
	api.On("UpdateChannelSidebarCategories", "u1", "t1", mock.MatchedBy(func(cats []*model.SidebarCategoryWithChannels) bool {
		return len(cats) == 1 && cats[0].DisplayName == sidebarCategoryName &&
			len(cats[0].Channels) == 1 && cats[0].Channels[0] == "c1"
	})).Return(nil, testAppErr("update failed"))

	p.addChannelToPermissionsCategory("u1", "t1", "c1")

	api.AssertCalled(t, "LogWarn", "sidebar: failed to update category",
		"user_id", "u1", "category_id", "cat1", "error", mock.Anything)
	api.AssertNotCalled(t, "CreateChannelSidebarCategory", mock.Anything, mock.Anything, mock.Anything)
}

func TestAddChannelToPermissionsCategory_PrependsMissingChannel(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: sidebarCategoryName},
			Channels:        []string{"c0"},
		}},
	}, nil)
	api.On("UpdateChannelSidebarCategories", "u1", "t1", mock.MatchedBy(func(cats []*model.SidebarCategoryWithChannels) bool {
		return len(cats) == 1 && len(cats[0].Channels) == 2 && cats[0].Channels[0] == "c1" && cats[0].Channels[1] == "c0"
	})).Return(nil, nil)

	p.addChannelToPermissionsCategory("u1", "t1", "c1")

	api.AssertNumberOfCalls(t, "UpdateChannelSidebarCategories", 1)
}

func TestAddChannelToPermissionsCategory_CreateFailureIsLogged(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(&model.OrderedSidebarCategories{}, nil)
	api.On("CreateChannelSidebarCategory", "u1", "t1", mock.Anything).Return(nil, testAppErr("create failed"))

	p.addChannelToPermissionsCategory("u1", "t1", "c1")

	api.AssertCalled(t, "LogWarn", "sidebar: failed to create category",
		"user_id", "u1", "team_id", "t1", "error", mock.Anything)
}

// --- addSystemAdminsToChannel ---

func TestAddSystemAdminsToChannel_ChannelLookupFailureSkipsSidebar(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("GetChannel", "c1").Return(nil, testAppErr("gone"))
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sa1"}}, nil)
	api.On("AddChannelMember", "c1", "sa1").Return(&model.ChannelMember{}, nil)

	p.addSystemAdminsToChannel("c1")

	api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
}

func TestAddSystemAdminsToChannel_AdminListFailureAddsNobody(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", TeamId: "t1"}, nil)
	api.On("GetUsers", mock.Anything).Return(nil, testAppErr("down"))

	p.addSystemAdminsToChannel("c1")

	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestAddSystemAdminsToChannel_AddFailureContinuesWithOthers(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", TeamId: "t1"}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sa1"}, {Id: "sa2"}}, nil)
	api.On("AddChannelMember", "c1", "sa1").Return(nil, testAppErr("denied"))
	api.On("AddChannelMember", "c1", "sa2").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelSidebarCategories", "sa2", "t1").Return(nil, testAppErr("down"))

	p.addSystemAdminsToChannel("c1")

	api.AssertNotCalled(t, "GetChannelSidebarCategories", "sa1", "t1")
}

// --- ensureNamedApprovalChannel ---

func TestEnsureNamedApprovalChannel_TeamNotFoundReturnsNil(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(nil, testAppErr("not found"))

	require.Nil(t, p.ensureBotApprovalChannel())
	api.AssertNotCalled(t, "GetChannelByName", mock.Anything, mock.Anything, mock.Anything)
}

func TestEnsureNamedApprovalChannel_BlankTeamReturnsNil(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})

	require.Nil(t, p.ensureWebhookApprovalChannel())
	api.AssertNotCalled(t, "GetTeamByName", mock.Anything)
}

func TestEnsureNamedApprovalChannel_CreatesAndSeeds(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	defer api.AssertExpectations(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-bot-requests", true).Return(nil, testAppErr("not found"))
	created := pluginChannelIn("botch", "mattermost-bot-requests", "t1")
	api.On("CreateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.Name == "mattermost-bot-requests" && c.DisplayName == "Mattermost: Bot Requests" &&
			c.Type == model.ChannelTypePrivate
	})).Return(created, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post.ChannelId == "botch" && post.UserId == "bot-user-id"
	})).Return(&model.Post{}, nil)
	api.On("GetChannel", "botch").Return(created, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)

	ch := p.ensureBotApprovalChannel()

	require.NotNil(t, ch)
	require.Equal(t, "botch", ch.Id)
}

func TestEnsureNamedApprovalChannel_CreateConflictReturnsExisting(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-audit-requests", true).Return(nil, testAppErr("not found")).Once()
	api.On("GetChannelByName", "t1", "mattermost-audit-requests", true).
		Return(pluginChannelIn("auditch", "mattermost-audit-requests", "t1"), nil)
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("exists"))

	ch := p.ensureAuditChannel()

	require.NotNil(t, ch)
	require.Equal(t, "auditch", ch.Id)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestEnsureNamedApprovalChannel_CreateFailureReturnsNil(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-team-requests", true).Return(nil, testAppErr("not found"))
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("denied"))

	require.Nil(t, p.ensureTeamCreationApprovalChannel())
	api.AssertNotCalled(t, "GetUsers", mock.Anything)
}

// --- forceTicketChannelCategory ---

func TestForceTicketChannelCategory_IgnoresChannelWithoutTeam(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})

	p.forceTicketChannelCategory(nil)
	p.forceTicketChannelCategory(&model.Channel{Id: "c1"})

	api.AssertNotCalled(t, "GetUsers", mock.Anything)
}

// --- addTeamApproversToChannel ---

func TestAddTeamApproversToChannel_MemberListFailureStops(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return(nil, testAppErr("down"))

	p.addTeamApproversToChannel("team1", "c1")

	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestAddTeamApproversToChannel_SkipsBotAndContinuesAfterAddFailure(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sa1"}}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{
		{UserId: "bot-user-id", SchemeAdmin: true},
		{UserId: "ta1", Roles: "team_user team_admin"},
		{UserId: "plain", Roles: "team_user"},
		{UserId: "sa1", Roles: "team_user"},
	}, nil)
	api.On("AddChannelMember", "c1", "ta1").Return(nil, testAppErr("denied"))
	api.On("AddChannelMember", "c1", "sa1").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelSidebarCategories", "sa1", "team1").Return(nil, testAppErr("down"))

	p.addTeamApproversToChannel("team1", "c1")

	api.AssertNotCalled(t, "AddChannelMember", "c1", "bot-user-id")
	api.AssertNotCalled(t, "AddChannelMember", "c1", "plain")
	api.AssertNotCalled(t, "GetChannelSidebarCategories", "ta1", "team1")
}

// --- ensureAllTeamApprovalChannels ---

func TestEnsureAllTeamApprovalChannels_TeamListFailure(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetTeams").Return(nil, testAppErr("down"))

	p.ensureAllTeamApprovalChannels()

	api.AssertNotCalled(t, "GetTeamMembers", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "LogInfo", "per-team approval channel sweep complete",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestEnsureAllTeamApprovalChannels_CountsEnsuredAndSkipped(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetTeams").Return([]*model.Team{
		{Id: "quiet", Name: "quiet"},
		{Id: "ops", Name: "ops"},
		{Id: "town", Name: "town"},
	}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	admin := []*model.TeamMember{{UserId: "ta1", SchemeAdmin: true}}
	api.On("GetTeamMembers", "quiet", 0, 200).Return([]*model.TeamMember{}, nil)
	api.On("GetTeamMembers", "ops", 0, 200).Return(admin, nil)
	api.On("GetTeamMembers", "town", 0, 200).Return(admin, nil)
	api.On("GetChannelByName", "ops", "mattermost-channel-requests", true).
		Return(pluginChannelIn("opsch", "mattermost-channel-requests", "ops"), nil)
	api.On("GetChannelByName", "town", "mattermost-channel-requests", true).Return(takenChannel("townch"), nil)

	p.ensureAllTeamApprovalChannels()

	api.AssertNotCalled(t, "GetChannelByName", "quiet", mock.Anything, mock.Anything)
	api.AssertCalled(t, "LogWarn", "failed to ensure approval channel for team",
		"team_id", "town", "team_name", "town", "error", mock.Anything)
	api.AssertCalled(t, "LogInfo", "per-team approval channel sweep complete",
		"teams", 3, "ensured", 1, "skipped_no_approvers", 1)
}

// --- migrateTicketChannelsToManagedCategory ---

func TestMigrateTicketChannelsToManagedCategory_SkipsWhenAlreadyDone(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("KVGet", kvSidebarCategoryMigrated).Return([]byte("1"), nil)

	p.migrateTicketChannelsToManagedCategory()

	api.AssertNotCalled(t, "GetTeams")
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestMigrateTicketChannelsToManagedCategory_TeamListFailureStillMarksDone(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("KVGet", kvSidebarCategoryMigrated).Return(nil, nil)
	api.On("GetTeams").Return(nil, testAppErr("down"))
	api.On("KVSet", kvSidebarCategoryMigrated, []byte("1")).Return(nil)

	p.migrateTicketChannelsToManagedCategory()

	api.AssertNotCalled(t, "GetUsersInChannel", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestMigrateTicketChannelsToManagedCategory_PlacesEachChannelOnce(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	defer api.AssertExpectations(t)
	stubApprovalTeamLookup(t, api)
	api.On("KVGet", kvSidebarCategoryMigrated).Return(nil, nil)

	root := pluginChannelIn("root", "mattermost-channel-requests", "t1")
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", true).Return(root, nil)
	// The per-team sweep looks the channel up again without archived ones.
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", false).Return(root, nil)
	api.On("GetChannelByName", "t1", "mattermost-bot-requests", true).
		Return(pluginChannelIn("botch", "mattermost-bot-requests", "t1"), nil)
	api.On("GetChannelByName", "t1", "mattermost-webhook-requests", true).
		Return(pluginChannelIn("hookch", "mattermost-webhook-requests", "t1"), nil)
	api.On("GetChannelByName", "t1", "mattermost-team-requests", true).
		Return(pluginChannelIn("teamch", "mattermost-team-requests", "t1"), nil)
	// The audit name is held by someone else's channel, so it is left out.
	api.On("GetChannelByName", "t1", "mattermost-audit-requests", true).Return(takenChannel("other"), nil)
	api.On("GetChannelByName", "t2", "mattermost-channel-requests", false).Return(nil, testAppErr("not found"))
	// forceTicketChannelCategory for the team-requests channel: no System Admins.
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)

	api.On("GetTeams").Return([]*model.Team{{Id: "t1"}, {Id: "t2"}}, nil)
	api.On("GetUsersInChannel", "root", "username", 0, 200).Return(nil, testAppErr("down"))
	api.On("GetUsersInChannel", mock.Anything, "username", 0, 200).Return([]*model.User{{Id: "bot-user-id"}}, nil)
	api.On("KVSet", kvSidebarCategoryMigrated, []byte("1")).Return(nil)

	p.migrateTicketChannelsToManagedCategory()

	// root (also the t1 per-team channel), bot, webhook and team channels.
	api.AssertNumberOfCalls(t, "GetUsersInChannel", 4)
	api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
	api.AssertCalled(t, "LogInfo", "sidebar migration: ticket channels forced into Mattermost: Request Hub category",
		"channels", 4, "member_placements", 0)
}

// --- placeChannelMembersInManagedCategory ---

func TestPlaceChannelMembersInManagedCategory_NoTeamReturnsZero(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})

	require.Zero(t, p.placeChannelMembersInManagedCategory(nil))
	require.Zero(t, p.placeChannelMembersInManagedCategory(&model.Channel{Id: "c1"}))
	api.AssertNotCalled(t, "GetUsersInChannel", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestPlaceChannelMembersInManagedCategory_StopsAfterPageLimit(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetUsersInChannel", "c1", "username", mock.Anything, 200).
		Return(repeated(&model.User{Id: "bot-user-id"}, 200), nil)

	require.Zero(t, p.placeChannelMembersInManagedCategory(&model.Channel{Id: "c1", TeamId: "t1"}))
	api.AssertNumberOfCalls(t, "GetUsersInChannel", 50)
}

func TestPlaceChannelMembersInManagedCategory_ReturnsCountSoFarOnError(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetUsersInChannel", "c1", "username", 0, 200).
		Return(repeated(&model.User{Id: "u1"}, 200), nil)
	api.On("GetUsersInChannel", "c1", "username", 1, 200).Return(nil, testAppErr("down"))
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: sidebarCategoryName},
			Channels:        []string{"c1"},
		}},
	}, nil)

	require.Equal(t, 200, p.placeChannelMembersInManagedCategory(&model.Channel{Id: "c1", TeamId: "t1"}))
}

// --- syncTeamApprovalChannelAfterAdminChange ---

func TestSyncTeamApprovalChannelAfterAdminChange_NoApproversDoesNothing(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil)

	p.syncTeamApprovalChannelAfterAdminChange("team1")

	api.AssertNotCalled(t, "GetChannelByName", mock.Anything, mock.Anything, mock.Anything)
}

func TestSyncTeamApprovalChannelAfterAdminChange_EnsureFailureStops(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{{UserId: "ta1", SchemeAdmin: true}}, nil)
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).Return(takenChannel("x"), nil)

	p.syncTeamApprovalChannelAfterAdminChange("team1")

	api.AssertCalled(t, "LogWarn", "failed to ensure per-team approval channel after admin change",
		"team_id", "team1", "error", mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

// --- UserHasJoinedTeam ---

func TestUserHasJoinedTeam_LooksUpSystemAdminWithoutActor(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	api.On("GetUser", "sa1").Return(&model.User{Id: "sa1", Roles: "system_user system_admin"}, nil)
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).Return(takenChannel("x"), nil)

	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{TeamId: "team1", UserId: "sa1", Roles: "team_user"}, nil)

	api.AssertCalled(t, "LogWarn", "failed to ensure team approval channel on team join",
		"team_id", "team1", "user_id", "sa1", "error", mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestUserHasJoinedTeam_AddFailureSkipsSidebar(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	// Added by another user, so the joiner's roles are looked up.
	api.On("GetUser", "sa1").Return(&model.User{Id: "sa1", Roles: "system_user system_admin"}, nil)
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(pluginChannelIn("ch-team1", "mattermost-channel-requests", "team1"), nil)
	api.On("AddChannelMember", "ch-team1", "sa1").Return(nil, testAppErr("denied"))

	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{TeamId: "team1", UserId: "sa1"}, &model.User{Id: "other"})

	api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
}
