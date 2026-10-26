package main

import (
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
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
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

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
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
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
		Return(nil, testAppErr("not found"))
	api.On("GetTeam", "team1").
		Return(nil, testAppErr("team not found"))

	// Global fallback succeeds.
	api.On("GetTeamByName", "globalteam").Return(&model.Team{Id: "globalteam-id", Name: "globalteam"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "globalteam-id", "mattermost-channel-requests", false).
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
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).Return(existing, nil)

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
	api.On("GetChannelByName", "team-hq", "mattermost-channel-requests", false).
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
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team-eng", 0, 200).Return([]*model.TeamMember{
		{TeamId: "team-eng", UserId: "u_tadmin", SchemeAdmin: true},
	}, nil)
	api.On("GetChannelByName", "team-eng", "mattermost-channel-requests", false).
		Return(&model.Channel{Id: "ch-eng", TeamId: "team-eng"}, nil)

	p.ensureAllTeamApprovalChannels()

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
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
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
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
		Return(nil, testAppErr("not found")).Once()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Eng"}, nil)
	api.On("GetTeamMember", "team1", "bot-user-id").Return(nil, testAppErr("not a member"))
	api.On("CreateTeamMember", "team1", "bot-user-id").Return(&model.TeamMember{}, nil)
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("already exists"))
	// Retry lookup after CreateChannel fails.
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
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

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
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

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
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

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
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

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
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

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
		Return(&model.Channel{Id: "theirs", Type: model.ChannelTypePrivate, CreatorId: "u_other"}, nil)

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

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", false).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "ch-team1"}, nil)
	api.On("AddChannelMember", "ch-team1", "u_ta").Return(&model.ChannelMember{}, nil)

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
	p.setConfiguration(&configuration{PerTeamApprovalChannels: false})

	// Even if the joiner is a team admin, no channel work should happen.
	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{
		TeamId: "team1", UserId: "u_ta", SchemeAdmin: true,
	}, &model.User{Id: "u_ta"})

	api.AssertNotCalled(t, "GetChannelByName", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

// --- Per-team routing integration: submitRequest routes to team channel ---

func TestSubmitRequest_RoutesToPerTeamChannel(t *testing.T) {
	api := &plugintest.API{}
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
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", false).Return(teamCh, nil)
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
