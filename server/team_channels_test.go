package main

import (
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// --- resolveApprovalChannel ---

func TestResolveApprovalChannel_UsesPerTeamChannelWhenEnabled(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true})

	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	// hasActiveTeamApprovers: returns one Team Admin so the per-team path is taken.
	api.On("GetTeamMembers", "team1", 0, 200).
		Return([]*model.TeamMember{{TeamId: "team1", UserId: "u_admin", SchemeAdmin: true}}, nil)

	// Fast path: channel already exists in this team.
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "ch-team1"}, nil)

	ch, label := p.resolveApprovalChannel("team1")
	require.NotNil(t, ch)
	require.Equal(t, "ch-team1", ch.Id)
	require.Empty(t, label)
}

func TestResolveApprovalChannel_UsesPerTeamWhenSystemAdminOnTeam(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true})

	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "u_sys"}}, nil)
	// Plain member who is a System Admin — counts as a local approver.
	api.On("GetTeamMembers", "team1", 0, 200).
		Return([]*model.TeamMember{{TeamId: "team1", UserId: "u_sys", Roles: "team_user"}}, nil)

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "ch-team1"}, nil)

	ch, label := p.resolveApprovalChannel("team1")
	require.NotNil(t, ch)
	require.Equal(t, "ch-team1", ch.Id)
	require.Empty(t, label)
}

func TestResolveApprovalChannel_FallsBackToGlobalWhenPerTeamFails(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		PerTeamApprovalChannels: true,
		ApprovalTeam:            "globalteam",
	})

	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	// hasActiveTeamApprovers: returns one admin so the per-team path is attempted.
	api.On("GetTeamMembers", "team1", 0, 200).
		Return([]*model.TeamMember{{TeamId: "team1", UserId: "u_admin", SchemeAdmin: true}}, nil)

	// Per-team fast path fails — channel doesn't exist, team lookup also fails
	// so ensureTeamApprovalChannel returns an error.
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(nil, testAppErr("not found"))
	api.On("GetTeam", "team1").
		Return(nil, testAppErr("team not found"))

	// Global fallback succeeds.
	api.On("GetTeamByName", "globalteam").Return(&model.Team{Id: "globalteam-id", Name: "globalteam"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "globalteam-id", "mattermost-channel-requests", true).
		Return(pluginChannel("ch-global", "mattermost-channel-requests"), nil)

	ch, label := p.resolveApprovalChannel("team1")
	require.NotNil(t, ch)
	require.Equal(t, "ch-global", ch.Id)
	require.Empty(t, label)
}

func TestResolveApprovalChannel_ReturnsNilWhenNothingConfigured(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	ch, label := p.resolveApprovalChannel("team1")
	require.Nil(t, ch)
	require.Empty(t, label)
}

// --- ensureTeamApprovalChannel ---

func TestEnsureTeamApprovalChannel_FastPathReturnsExisting(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	existing := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "existing-ch"}
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).Return(existing, nil)

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.NoError(t, err)
	require.Equal(t, "existing-ch", ch.Id)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

// --- ensureAllTeamApprovalChannels ---

// The approval team's per-team channel is the root channel, created with
// System Admins only. The sweep must add the team's Team Admins to it.
func TestEnsureAllTeamApprovalChannels_SeedsApprovalTeamAdminsIntoRootChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true, ApprovalTeam: "hq"})

	api.On("GetTeams").Return([]*model.Team{{Id: "team-hq", Name: "hq"}}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team-hq", 0, 200).Return([]*model.TeamMember{
		{TeamId: "team-hq", UserId: "u_tadmin", SchemeAdmin: true},
		{TeamId: "team-hq", UserId: "u_member", Roles: "team_user"},
	}, nil)
	api.On("GetChannelByName", "team-hq", "mattermost-channel-requests", true).
		Return(&model.Channel{Id: "ch-root", TeamId: "team-hq", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"}, nil)
	api.On("AddChannelMember", "ch-root", "u_tadmin").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelSidebarCategories", "u_tadmin", "team-hq").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"ch-root"},
		}},
	}, nil)

	p.ensureAllTeamApprovalChannels()

	api.AssertCalled(t, "AddChannelMember", "ch-root", "u_tadmin")
	api.AssertNotCalled(t, "AddChannelMember", "ch-root", "u_member")
}

// Other teams' existing channels keep the fast path: no re-seeding.
func TestEnsureAllTeamApprovalChannels_LeavesOtherTeamsExistingChannels(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true, ApprovalTeam: "hq"})

	api.On("GetTeams").Return([]*model.Team{{Id: "team-eng", Name: "eng"}}, nil)
	api.On("GetChannelByName", "team-eng", "mattermost-channel-requests", true).
		Return(&model.Channel{Id: "ch-eng", TeamId: "team-eng", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"}, nil)

	p.ensureAllTeamApprovalChannels()

	// Existing non-approval-team channels skip membership scans and re-seeding.
	api.AssertNotCalled(t, "GetTeamMembers", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestEnsureTeamApprovalChannel_CreatesAndSeeds(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	expectDocsBookmarkAttempt(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	// Channel doesn't exist yet.
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(nil, testAppErr("not found"))

	team := &model.Team{Id: "team1", DisplayName: "Engineering"}
	api.On("GetTeam", "team1").Return(team, nil)

	// Bot must be a team member before the channel can be created.
	api.On("GetTeamMember", "team1", "bot-user-id").Return(nil, testAppErr("not a member"))
	api.On("CreateTeamMember", "team1", "bot-user-id").Return(&model.TeamMember{}, nil)

	created := &model.Channel{Id: "new-ch"}
	api.On("CreateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.TeamId == "team1" && c.Name == "mattermost-channel-requests"
	})).Return(created, nil)

	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()

	// Seed: two team members, one admin.
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{
		{TeamId: "team1", UserId: "u_admin", Roles: "team_user team_admin"},
		{TeamId: "team1", UserId: "u_member", Roles: "team_user"},
	}, nil)
	api.On("AddChannelMember", "new-ch", "u_admin").Return(&model.ChannelMember{}, nil)

	// Short-lived "Mattermost Requests" label is renamed back to
	// "Mattermost: Request Hub" even when the channel is already listed.
	api.On("GetChannelSidebarCategories", "u_admin", "team1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{Id: "cat1", DisplayName: "Mattermost Requests"},
			Channels:        []string{"new-ch"},
		}},
	}, nil)
	api.On("UpdateChannelSidebarCategories", "u_admin", "team1", mock.MatchedBy(func(cats []*model.SidebarCategoryWithChannels) bool {
		return len(cats) == 1 && cats[0].DisplayName == "Mattermost: Request Hub"
	})).Return([]*model.SidebarCategoryWithChannels{}, nil)

	// Welcome message post.
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post.ChannelId == "new-ch"
	})).Return(&model.Post{}, nil)

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.NoError(t, err)
	require.Equal(t, "new-ch", ch.Id)

	// Only the team admin was added, not the plain member.
	api.AssertCalled(t, "AddChannelMember", "new-ch", "u_admin")
	api.AssertNotCalled(t, "AddChannelMember", "new-ch", "u_member")
}

func TestEnsureTeamApprovalChannel_RaceRetryOnCreateConflict(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	// First GetChannelByName (fast path) misses; a concurrent call creates it
	// before ours does, so CreateChannel fails; the retry lookup succeeds.
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(nil, testAppErr("not found")).Once()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Eng"}, nil)
	api.On("GetTeamMember", "team1", "bot-user-id").Return(nil, testAppErr("not a member"))
	api.On("CreateTeamMember", "team1", "bot-user-id").Return(&model.TeamMember{}, nil)
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("already exists"))
	// Retry lookup after CreateChannel fails.
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "already-created"}, nil).Once()

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.NoError(t, err)
	require.Equal(t, "already-created", ch.Id)
}

func TestEnsureTeamApprovalChannel_UsesFixedChannelName(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	expectDocsBookmarkAttempt(api)
	p := newTestPlugin(api)
	// Channel name is hardcoded — no per-team name setting exists.
	p.setConfiguration(&configuration{})

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(nil, testAppErr("not found"))
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Acme"}, nil)
	api.On("GetTeamMember", "team1", "bot-user-id").Return(&model.TeamMember{DeleteAt: 1}, nil)
	api.On("CreateTeamMember", "team1", "bot-user-id").Return(&model.TeamMember{}, nil)
	api.On("CreateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.Name == "mattermost-channel-requests"
	})).Return(&model.Channel{Id: "fixed-ch"}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.NoError(t, err)
	require.Equal(t, "fixed-ch", ch.Id)
}

func TestEnsureTeamApprovalChannel_CreateFailureWithNoChannelIsAnError(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(nil, testAppErr("not found"))
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Acme"}, nil)
	api.On("GetTeamMember", "team1", "bot-user-id").Return(&model.TeamMember{}, nil)
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("create failed"))

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.Error(t, err)
	require.Nil(t, ch)
}

func TestEnsureTeamApprovalChannel_BotAlreadyOnTeamIsNotAddedAgain(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	expectDocsBookmarkAttempt(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(nil, testAppErr("not found"))
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Acme"}, nil)
	api.On("GetTeamMember", "team1", "bot-user-id").Return(&model.TeamMember{TeamId: "team1", UserId: "bot-user-id"}, nil)
	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "new-ch"}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.NoError(t, err)
	require.Equal(t, "new-ch", ch.Id)
	api.AssertNotCalled(t, "CreateTeamMember", mock.Anything, mock.Anything)
}

func TestEnsureTeamApprovalChannel_BotTeamJoinFailureStillCreates(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	expectDocsBookmarkAttempt(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(nil, testAppErr("not found"))
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Acme"}, nil)
	api.On("GetTeamMember", "team1", "bot-user-id").Return(nil, testAppErr("not a member"))
	api.On("CreateTeamMember", "team1", "bot-user-id").Return(nil, testAppErr("join failed"))
	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "new-ch"}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.NoError(t, err)
	require.Equal(t, "new-ch", ch.Id)
	api.AssertCalled(t, "LogWarn", "failed to add bot to team before creating approval channel",
		"team_id", "team1", "bot_user_id", "bot-user-id", "error", mock.Anything)
}

func TestEnsureTeamApprovalChannel_NameTakenByPrivateChannelOfSomeoneElse(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(&model.Channel{Id: "theirs", Type: model.ChannelTypePrivate, CreatorId: "u_other"}, nil)
	api.On("GetBot", "u_other", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Roles: model.SystemUserRoleId}, nil)
	api.On("GetChannelMember", "theirs", mock.Anything).Return(nil, testAppErr("not a member"))

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.Error(t, err)
	require.Nil(t, ch)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	api.AssertNotCalled(t, "CreateTeamMember", mock.Anything, mock.Anything)
}

func TestEnsureTeamMember(t *testing.T) {
	t.Run("active member is left alone", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{}, nil)

		require.NoError(t, p.ensureTeamMember("team1", "u1"))
		api.AssertNotCalled(t, "CreateTeamMember", mock.Anything, mock.Anything)
	})
	t.Run("someone who left is added back", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{DeleteAt: 5}, nil)
		api.On("CreateTeamMember", "team1", "u1").Return(&model.TeamMember{}, nil)

		require.NoError(t, p.ensureTeamMember("team1", "u1"))
	})
	t.Run("add failure is returned", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetTeamMember", "team1", "u1").Return(nil, testAppErr("not found"))
		api.On("CreateTeamMember", "team1", "u1").Return(nil, testAppErr("join failed"))

		require.Error(t, p.ensureTeamMember("team1", "u1"))
	})
}

// --- UserHasJoinedTeam ---

func TestUserHasJoinedTeam_AddsTeamAdminToApprovalChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true})

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "ch-team1"}, nil)
	api.On("AddChannelMember", "ch-team1", "u_ta").Return(&model.ChannelMember{}, nil)
	// Seed ledger write so demotion eviction can remove this Team Admin later.
	api.On("KVGet", taVisibilitySeededKey("ch-team1")).Return(nil, nil)
	api.On("KVCompareAndSet", taVisibilitySeededKey("ch-team1"), mock.Anything, mock.Anything).Return(true, nil)

	// Already in managed category — no update needed.
	api.On("GetChannelSidebarCategories", "u_ta", "team1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"ch-team1"},
		}},
	}, nil)

	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{
		TeamId: "team1", UserId: "u_ta", Roles: "team_user team_admin",
	}, &model.User{Id: "u_ta"})

	api.AssertCalled(t, "AddChannelMember", "ch-team1", "u_ta")
	api.AssertCalled(t, "KVCompareAndSet", taVisibilitySeededKey("ch-team1"), mock.Anything, mock.Anything)
}

func TestUserHasJoinedTeam_IgnoresPlainMember(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true})

	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{
		TeamId: "team1", UserId: "u_member", Roles: "team_user",
	}, &model.User{Id: "u_member"})

	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestUserHasJoinedTeam_NoOpWhenFeatureDisabled(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	// No ApprovalTeam: primary visibility seeding is a no-op, and with
	// PerTeamApprovalChannels off the per-team path is skipped too.
	p.setConfiguration(&configuration{PerTeamApprovalChannels: false})

	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{
		TeamId: "team1", UserId: "u_ta", SchemeAdmin: true,
	}, &model.User{Id: "u_ta"})

	api.AssertNotCalled(t, "GetChannelByName", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

// Primary Approval-team Team Admins get watch-only membership on gated
// visibility channels (root/team/audit), never bot/webhook pools.
func TestSyncPrimaryTeamAdminVisibility_SeedsAllPrimaryChannels(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		ApprovalTeam:                           "hq",
		AllowTeamAdminApprovers:                true,
		AllowApprovalTeamAdminsForTeamRequests: true,
	})

	team := &model.Team{Id: "team-hq", Name: "hq"}
	api.On("GetTeamByName", "hq").Return(team, nil)
	stubApprovalTeamRecord(api)

	channels := map[string]*model.Channel{
		"mattermost-channel-requests": {Id: "ch-root", TeamId: "team-hq", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"},
		"mattermost-team-requests":    {Id: "ch-team", TeamId: "team-hq", Name: "mattermost-team-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"},
		"mattermost-audit-requests":   {Id: "ch-audit", TeamId: "team-hq", Name: "mattermost-audit-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"},
	}
	for name, ch := range channels {
		api.On("GetChannelByName", "team-hq", name, true).Return(ch, nil)
	}

	api.On("GetTeamMembers", "team-hq", 0, 200).Return([]*model.TeamMember{
		{TeamId: "team-hq", UserId: "u_tadmin", SchemeAdmin: true},
		{TeamId: "team-hq", UserId: "u_member", Roles: "team_user"},
	}, nil)

	api.On("CreateTeamMember", "team-hq", "u_tadmin").Return(&model.TeamMember{}, nil).Maybe()
	notFound := model.NewAppError("test", "not_found", nil, "", http.StatusNotFound)
	for _, id := range []string{"ch-root", "ch-team", "ch-audit"} {
		// Not a member yet → seed adds and marks; confirm membership after add.
		api.On("GetChannelMember", id, "u_tadmin").Return(nil, notFound).Once()
		api.On("AddChannelMember", id, "u_tadmin").Return(&model.ChannelMember{}, nil)
		api.On("GetChannelMember", id, "u_tadmin").Return(&model.ChannelMember{UserId: "u_tadmin"}, nil).Once()
	}
	api.On("GetChannelSidebarCategories", "u_tadmin", "team-hq").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"ch-root", "ch-team", "ch-audit"},
		}},
	}, nil).Maybe()
	// forceTicketChannelCategory for team-requests walks System Admins.
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	// Eviction pass after seed: empty ledgers skip member paging.
	for _, id := range []string{"ch-root", "ch-team", "ch-audit"} {
		api.On("KVGet", taVisibilitySeededKey(id)).Return(nil, nil).Maybe()
		api.On("KVCompareAndSet", taVisibilitySeededKey(id), mock.Anything, mock.Anything).Return(true, nil).Maybe()
		api.On("GetChannelMembers", id, 0, 100).Return(model.ChannelMembers{}, nil).Maybe()
	}

	p.syncPrimaryTeamAdminVisibility()

	for _, id := range []string{"ch-root", "ch-team", "ch-audit"} {
		api.AssertCalled(t, "AddChannelMember", id, "u_tadmin")
	}
	api.AssertNotCalled(t, "AddChannelMember", "ch-bot", "u_tadmin")
	api.AssertNotCalled(t, "AddChannelMember", "ch-wh", "u_tadmin")
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, "u_member")
}

func TestEvictStalePrimaryTeamAdminChannelMembers_RemovesDemoted(t *testing.T) {
	api := &plugintest.API{}
	// Seed stub before stubLogs so the specific KVGet wins over the catch-all.
	stubTAVisibilitySeeded(api, "ch-root", "demoted", "still-admin")
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.botUserID = "bot"
	// Toggle on so still-admin (Team Admin) qualifies to stay on root.
	p.setConfiguration(&configuration{ApprovalTeam: "hq", AllowTeamAdminApprovers: true})

	team := &model.Team{Id: "team-hq", Name: "hq"}
	api.On("GetTeamByName", "hq").Return(team, nil)
	stubApprovalTeamRecord(api)

	root := &model.Channel{Id: "ch-root", TeamId: "team-hq", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "bot"}
	teamCh := &model.Channel{Id: "ch-team", TeamId: "team-hq", Name: "mattermost-team-requests", Type: model.ChannelTypePrivate, CreatorId: "bot"}
	audit := &model.Channel{Id: "ch-audit", TeamId: "team-hq", Name: "mattermost-audit-requests", Type: model.ChannelTypePrivate, CreatorId: "bot"}
	api.On("GetChannelByName", "team-hq", "mattermost-channel-requests", true).Return(root, nil)
	api.On("GetChannelByName", "team-hq", "mattermost-team-requests", true).Return(teamCh, nil)
	api.On("GetChannelByName", "team-hq", "mattermost-audit-requests", true).Return(audit, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()

	// Only root has a demoted member; team/audit empty ledgers skip paging.
	api.On("GetChannelMembers", "ch-root", 0, 100).Return(model.ChannelMembers{
		{UserId: "bot"},
		{UserId: "demoted"},
		{UserId: "still-admin"},
		{UserId: "sysadmin"},
	}, nil)
	api.On("KVGet", taVisibilitySeededKey("ch-team")).Return(nil, nil).Maybe()
	api.On("KVGet", taVisibilitySeededKey("ch-audit")).Return(nil, nil).Maybe()

	api.On("GetUser", "demoted").Return(&model.User{Id: "demoted", Username: "ex"}, nil)
	api.On("GetUser", "still-admin").Return(&model.User{Id: "still-admin", Username: "ta"}, nil)
	// sysadmin is not in the seed set — eviction must not even look them up.

	api.On("GetTeamMember", "team-hq", "demoted").Return(&model.TeamMember{UserId: "demoted", Roles: model.TeamUserRoleId}, nil)
	api.On("GetTeamMember", "team-hq", "still-admin").Return(&model.TeamMember{UserId: "still-admin", SchemeAdmin: true}, nil)

	api.On("DeleteChannelMember", "ch-root", "demoted").Return(nil).Once()

	p.evictStalePrimaryTeamAdminChannelMembers()

	api.AssertCalled(t, "DeleteChannelMember", "ch-root", "demoted")
	api.AssertNotCalled(t, "DeleteChannelMember", "ch-root", "still-admin")
	api.AssertNotCalled(t, "DeleteChannelMember", "ch-root", "sysadmin")
	api.AssertNotCalled(t, "GetUser", "sysadmin")
	api.AssertNotCalled(t, "DeleteChannelMember", "ch-root", "bot")
}

func TestEvictStalePrimaryTeamAdminChannelMembers_ToggleOffEvictsSeededTeamAdmins(t *testing.T) {
	api := &plugintest.API{}
	stubTAVisibilitySeeded(api, "ch-root", "still-admin")
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.botUserID = "bot"
	// Defaults: AllowTeamAdminApprovers off → seeded TA must leave root.
	p.setConfiguration(&configuration{ApprovalTeam: "hq"})

	team := &model.Team{Id: "team-hq", Name: "hq"}
	api.On("GetTeamByName", "hq").Return(team, nil)
	stubApprovalTeamRecord(api)

	root := &model.Channel{Id: "ch-root", TeamId: "team-hq", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "bot"}
	api.On("GetChannelByName", "team-hq", "mattermost-channel-requests", true).Return(root, nil)
	api.On("GetChannelByName", "team-hq", "mattermost-team-requests", true).Return(nil, testAppErr("missing"))
	api.On("GetChannelByName", "team-hq", "mattermost-audit-requests", true).Return(nil, testAppErr("missing"))

	api.On("GetChannelMembers", "ch-root", 0, 100).Return(model.ChannelMembers{
		{UserId: "bot"},
		{UserId: "still-admin"},
	}, nil)
	api.On("GetUser", "still-admin").Return(&model.User{Id: "still-admin", Username: "ta"}, nil)
	api.On("GetTeamMember", "team-hq", "still-admin").Return(&model.TeamMember{UserId: "still-admin", SchemeAdmin: true}, nil)
	api.On("DeleteChannelMember", "ch-root", "still-admin").Return(nil).Once()

	p.evictStalePrimaryTeamAdminChannelMembers()

	api.AssertCalled(t, "DeleteChannelMember", "ch-root", "still-admin")
}

func TestEvictStaleApproversFromApprovalChannel_LeavesHandAddedObservers(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.botUserID = "bot"

	ch := &model.Channel{Id: "ch-root", TeamId: "team-hq"}
	// Empty ledger → skip member paging entirely (hand-added observers stay).
	api.On("KVGet", taVisibilitySeededKey("ch-root")).Return(nil, nil)

	p.evictStaleApproversFromApprovalChannel(ch, "team-hq", "team-hq")

	api.AssertNotCalled(t, "GetChannelMembers", "ch-root", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "GetUser", "hand-added")
	api.AssertNotCalled(t, "DeleteChannelMember", "ch-root", "hand-added")
}

func TestEvictStalePerTeamApprovalChannelMembers_RemovesDemotedOnSecondaryTeam(t *testing.T) {
	api := &plugintest.API{}
	stubTAVisibilitySeeded(api, "ch-ops", "demoted", "still-admin")
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.botUserID = "bot"
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true, ApprovalTeam: "hq"})

	api.On("GetTeamByName", "hq").Return(&model.Team{Id: "team-hq", Name: "hq"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetTeams").Return([]*model.Team{
		{Id: "team-hq", Name: "hq"},
		{Id: "team-ops", Name: "ops"},
	}, nil)
	opsCh := &model.Channel{Id: "ch-ops", TeamId: "team-ops", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "bot"}
	api.On("GetChannelByName", "team-hq", "mattermost-channel-requests", true).Return(nil, testAppErr("missing"))
	api.On("GetChannelByName", "team-ops", "mattermost-channel-requests", true).Return(opsCh, nil)

	api.On("GetChannelMembers", "ch-ops", 0, 100).Return(model.ChannelMembers{
		{UserId: "bot"},
		{UserId: "demoted"},
		{UserId: "still-admin"},
	}, nil)
	api.On("GetUser", "demoted").Return(&model.User{Id: "demoted", Username: "ex"}, nil)
	api.On("GetUser", "still-admin").Return(&model.User{Id: "still-admin", Username: "ta"}, nil)
	api.On("GetTeamMember", "team-ops", "demoted").Return(&model.TeamMember{UserId: "demoted", Roles: model.TeamUserRoleId}, nil)
	api.On("GetTeamMember", "team-ops", "still-admin").Return(&model.TeamMember{UserId: "still-admin", SchemeAdmin: true}, nil)
	api.On("DeleteChannelMember", "ch-ops", "demoted").Return(nil).Once()

	p.evictStalePerTeamApprovalChannelMembers()

	api.AssertCalled(t, "DeleteChannelMember", "ch-ops", "demoted")
	api.AssertNotCalled(t, "DeleteChannelMember", "ch-ops", "still-admin")
}

func TestReconcilePerTeamApprovalAccessForUser_EvictsDemoted(t *testing.T) {
	api := &plugintest.API{}
	stubTAVisibilitySeeded(api, "ch-ops", "demoted")
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true})

	ops := &model.Team{Id: "team-ops", Name: "ops"}
	api.On("GetTeamsForUser", "demoted").Return([]*model.Team{ops}, nil)
	opsCh := &model.Channel{Id: "ch-ops", TeamId: "team-ops", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"}
	api.On("GetChannelByName", "team-ops", "mattermost-channel-requests", true).Return(opsCh, nil)
	api.On("GetTeamMember", "team-ops", "demoted").Return(&model.TeamMember{UserId: "demoted", Roles: model.TeamUserRoleId}, nil)
	api.On("GetChannelMember", "ch-ops", "demoted").Return(&model.ChannelMember{UserId: "demoted"}, nil)
	api.On("DeleteChannelMember", "ch-ops", "demoted").Return(nil).Once()

	p.reconcilePerTeamApprovalAccessForUser(&model.User{Id: "demoted", Username: "ex"})

	api.AssertCalled(t, "DeleteChannelMember", "ch-ops", "demoted")
}

func TestReconcilePerTeamApprovalAccessForUser_LeavesHandAdded(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{PerTeamApprovalChannels: true})

	ops := &model.Team{Id: "team-ops", Name: "ops"}
	api.On("GetTeamsForUser", "observer").Return([]*model.Team{ops}, nil)
	opsCh := &model.Channel{Id: "ch-ops", TeamId: "team-ops", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"}
	api.On("GetChannelByName", "team-ops", "mattermost-channel-requests", true).Return(opsCh, nil)
	api.On("GetTeamMember", "team-ops", "observer").Return(&model.TeamMember{UserId: "observer", Roles: model.TeamUserRoleId}, nil)
	api.On("KVGet", taVisibilitySeededKey("ch-ops")).Return(nil, nil)

	p.reconcilePerTeamApprovalAccessForUser(&model.User{Id: "observer", Username: "obs"})

	api.AssertNotCalled(t, "DeleteChannelMember", "ch-ops", "observer")
}

func TestReconcilePrimaryTeamAdminVisibilityForUser_DoesNotEvictNonAdminOnLogin(t *testing.T) {
	// Login must seed TAs only. Evicting every non-TA on login also removes
	// manually added observers and turns their Approve click into a 403.
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "hq"})

	api.On("GetTeamByName", "hq").Return(&model.Team{Id: "team-hq", Name: "hq"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetTeamMember", "team-hq", "bystander").Return(&model.TeamMember{
		UserId: "bystander", Roles: model.TeamUserRoleId,
	}, nil)

	p.reconcilePrimaryTeamAdminVisibilityForUser(&model.User{Id: "bystander", Username: "by"})

	api.AssertNotCalled(t, "DeleteChannelMember", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestUserHasJoinedTeam_SeedsPrimaryTicketChannelsWhenApprovalTeamAdmin(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	// Per-team channels off: only primary visibility should run.
	p.setConfiguration(&configuration{
		ApprovalTeam:                           "hq",
		PerTeamApprovalChannels:                false,
		AllowTeamAdminApprovers:                true,
		AllowApprovalTeamAdminsForTeamRequests: true,
	})

	team := &model.Team{Id: "team-hq", Name: "hq"}
	api.On("GetTeamByName", "hq").Return(team, nil)
	stubApprovalTeamRecord(api)

	channels := map[string]*model.Channel{
		"mattermost-channel-requests": {Id: "ch-root", TeamId: "team-hq", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"},
		"mattermost-team-requests":    {Id: "ch-team", TeamId: "team-hq", Name: "mattermost-team-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"},
		"mattermost-audit-requests":   {Id: "ch-audit", TeamId: "team-hq", Name: "mattermost-audit-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"},
	}
	for name, ch := range channels {
		api.On("GetChannelByName", "team-hq", name, true).Return(ch, nil)
	}
	api.On("GetTeamMember", "team-hq", "u_ta").Return(&model.TeamMember{
		TeamId: "team-hq", UserId: "u_ta", SchemeAdmin: true,
	}, nil)
	api.On("CreateTeamMember", "team-hq", "u_ta").Return(&model.TeamMember{}, nil).Maybe()
	notFound := model.NewAppError("test", "not_found", nil, "", http.StatusNotFound)
	for _, id := range []string{"ch-root", "ch-team", "ch-audit"} {
		api.On("GetChannelMember", id, "u_ta").Return(nil, notFound).Once()
		api.On("AddChannelMember", id, "u_ta").Return(&model.ChannelMember{}, nil)
		api.On("GetChannelMember", id, "u_ta").Return(&model.ChannelMember{UserId: "u_ta"}, nil).Once()
		api.On("KVGet", taVisibilitySeededKey(id)).Return(nil, nil).Maybe()
		api.On("KVCompareAndSet", taVisibilitySeededKey(id), mock.Anything, mock.Anything).Return(true, nil).Maybe()
	}
	api.On("GetChannelSidebarCategories", "u_ta", "team-hq").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"ch-root", "ch-team", "ch-audit"},
		}},
	}, nil).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()

	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{
		TeamId: "team-hq", UserId: "u_ta", SchemeAdmin: true,
	}, &model.User{Id: "u_ta"})

	for _, id := range []string{"ch-root", "ch-team", "ch-audit"} {
		api.AssertCalled(t, "AddChannelMember", id, "u_ta")
	}
}

func TestAdoptApprovalChannel_PublicConvertStripsNonPrivilegedMembers(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.botUserID = "bot"

	ch := &model.Channel{
		Id: "ch-audit", TeamId: "team-hq", Name: "mattermost-audit-requests",
		Type: model.ChannelTypeOpen, CreatorId: "sa1", DeleteAt: 0,
	}
	api.On("GetBot", "sa1", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "sa1").Return(&model.User{Id: "sa1", Roles: model.SystemAdminRoleId}, nil)
	api.On("UpdateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.Id == "ch-audit" && c.Type == model.ChannelTypePrivate
	})).Return(&model.Channel{
		Id: "ch-audit", TeamId: "team-hq", Name: "mattermost-audit-requests",
		Type: model.ChannelTypePrivate, CreatorId: "sa1",
	}, nil)
	api.On("GetTeamMember", "team-hq", "bot").Return(&model.TeamMember{TeamId: "team-hq", UserId: "bot"}, nil).Maybe()
	api.On("CreateTeamMember", "team-hq", "bot").Return(&model.TeamMember{}, nil).Maybe()
	api.On("AddChannelMember", "ch-audit", "bot").Return(&model.ChannelMember{}, nil)
	api.On("KVSetWithOptions", adoptedStripKey("ch-audit"), []byte("1"), mock.Anything).Return(true, nil)
	api.On("GetChannelMembers", "ch-audit", 0, 100).Return(model.ChannelMembers{
		{UserId: "bot"},
		{UserId: "observer"},
		{UserId: "sa2"},
	}, nil)
	api.On("GetUser", "observer").Return(&model.User{Id: "observer", Username: "obs", Roles: model.SystemUserRoleId}, nil)
	api.On("GetUser", "sa2").Return(&model.User{Id: "sa2", Username: "admin", Roles: model.SystemAdminRoleId}, nil)
	api.On("DeleteChannelMember", "ch-audit", "observer").Return(nil).Once()
	api.On("KVDelete", taVisibilitySeededKey("ch-audit")).Return(nil)

	require.True(t, p.adoptApprovalChannel(ch))
	api.AssertCalled(t, "DeleteChannelMember", "ch-audit", "observer")
	api.AssertNotCalled(t, "DeleteChannelMember", "ch-audit", "sa2")
	api.AssertNotCalled(t, "DeleteChannelMember", "ch-audit", "bot")
}

func TestAdoptApprovalChannel_UntrustedOrphanStripsObservers(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.botUserID = "bot"

	ch := &model.Channel{
		Id: "ch-audit", TeamId: "team-hq", Name: "mattermost-audit-requests",
		Type: model.ChannelTypePrivate, CreatorId: "gone", DeleteAt: 0,
	}
	api.On("GetBot", "gone", true).Return(nil, testAppErr("not a bot"))
	// Deactivated creator → membership adoption path (untrusted orphan).
	api.On("GetUser", "gone").Return(&model.User{Id: "gone", DeleteAt: 1, Roles: model.SystemUserRoleId}, nil)
	api.On("GetChannelMember", "ch-audit", "bot").Return(&model.ChannelMember{UserId: "bot"}, nil)
	api.On("GetTeamMember", "team-hq", "bot").Return(&model.TeamMember{TeamId: "team-hq", UserId: "bot"}, nil).Maybe()
	api.On("AddChannelMember", "ch-audit", "bot").Return(&model.ChannelMember{}, nil)
	api.On("KVSetWithOptions", adoptedStripKey("ch-audit"), []byte("1"), mock.Anything).Return(true, nil)
	api.On("GetChannelMembers", "ch-audit", 0, 100).Return(model.ChannelMembers{
		{UserId: "bot"},
		{UserId: "observer"},
	}, nil)
	api.On("GetUser", "observer").Return(&model.User{Id: "observer", Username: "obs", Roles: model.SystemUserRoleId}, nil)
	api.On("DeleteChannelMember", "ch-audit", "observer").Return(nil).Once()
	api.On("KVDelete", taVisibilitySeededKey("ch-audit")).Return(nil)

	require.True(t, p.adoptApprovalChannel(ch))
	api.AssertCalled(t, "DeleteChannelMember", "ch-audit", "observer")
}

func TestAdoptApprovalChannel_StripFailureRefusesAdoption(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.botUserID = "bot"

	ch := &model.Channel{
		Id: "ch-audit", TeamId: "team-hq", Name: "mattermost-audit-requests",
		Type: model.ChannelTypePrivate, CreatorId: "gone", DeleteAt: 0,
	}
	api.On("GetBot", "gone", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "gone").Return(&model.User{Id: "gone", DeleteAt: 1, Roles: model.SystemUserRoleId}, nil)
	api.On("GetChannelMember", "ch-audit", "bot").Return(&model.ChannelMember{UserId: "bot"}, nil)
	api.On("GetTeamMember", "team-hq", "bot").Return(&model.TeamMember{TeamId: "team-hq", UserId: "bot"}, nil).Maybe()
	api.On("AddChannelMember", "ch-audit", "bot").Return(&model.ChannelMember{}, nil)
	api.On("KVSetWithOptions", adoptedStripKey("ch-audit"), []byte("1"), mock.Anything).Return(true, nil)
	api.On("GetChannelMembers", "ch-audit", 0, 100).Return(nil, testAppErr("list down"))
	api.On("KVDelete", adoptedStripKey("ch-audit")).Return(nil)

	require.False(t, p.adoptApprovalChannel(ch))
}

func TestKeepSeededTeamAdminOnChannel_PerTeamKeepsApprovalTeamTAs(t *testing.T) {
	p, _ := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true, AllowTeamAdminApprovers: false})
	root := &model.Channel{Id: "root", TeamId: "hq", Name: "mattermost-channel-requests"}
	require.True(t, p.keepSeededTeamAdminOnChannel(root, "hq"))

	p.setConfiguration(&configuration{PerTeamApprovalChannels: false, AllowTeamAdminApprovers: false})
	require.False(t, p.keepSeededTeamAdminOnChannel(root, "hq"))
}

func TestSyncPrimaryTeamAdminVisibility_SkipsOtherTeams(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "hq"})

	api.On("GetTeamByName", "hq").Return(&model.Team{Id: "team-hq", Name: "hq"}, nil)
	stubApprovalTeamRecord(api)
	// Joining a non-approval team as Team Admin must not touch primary channels.
	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{
		TeamId: "team-eng", UserId: "u_ta", SchemeAdmin: true,
	}, &model.User{Id: "u_ta"})

	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

// --- Per-team routing integration: submitRequest routes to team channel ---

func TestSubmitRequest_RoutesToPerTeamChannel(t *testing.T) {
	api := &plugintest.API{}
	stubNamesFree(api)
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	cfg := &configuration{
		PerTeamApprovalChannels: true,
		ChannelNamePrefixes:     "team-|Team channels|16",
	}
	// parsePrefixList populates the unexported prefixes slice that resolveChannelName requires.
	cfg.prefixes = parsePrefixList(cfg.ChannelNamePrefixes, func(_ string, _ ...any) {})
	p.setConfiguration(cfg)

	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "t1", "u_req").Return(&model.TeamMember{TeamId: "t1", UserId: "u_req", DeleteAt: 0}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	// hasActiveTeamApprovers: returns one admin so the per-team path is taken.
	api.On("GetTeamMembers", "t1", 0, 200).
		Return([]*model.TeamMember{{TeamId: "t1", UserId: "u_admin", SchemeAdmin: true}}, nil)

	// Per-team channel already exists — fast path.
	teamCh := &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "team-ch-t1"}
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", true).Return(teamCh, nil)
	api.On("GetTeam", "t1").Return(&model.Team{Id: "t1", Name: "t1", DisplayName: "Team One"}, nil)

	// Approval post goes to the per-team channel.
	var approvalPost *model.Post
	api.On("CreatePost", mock.MatchedBy(func(pp *model.Post) bool {
		return pp.ChannelId == "team-ch-t1"
	})).Run(func(args mock.Arguments) {
		approvalPost = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "ap1"}, nil)

	// Requester DM.
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)

	msg, err := p.submitRequest(requestInput{
		RequesterID: "u_req",
		TeamID:      "t1",
		DisplayName: "Marketing",
		Prefix:      "team-",
		Name:        "marketing",
		ChannelType: channelTypeOpen,
	})
	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	require.NotNil(t, approvalPost)
	require.Equal(t, "team-ch-t1", approvalPost.ChannelId)
}
