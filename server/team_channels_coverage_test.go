package main

import (
	"fmt"
	"net/http"
	"strings"
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

// stubRefuseAdopt stubs the adopt probes so takenChannel(id) stays unused.
func stubRefuseAdopt(api *plugintest.API, channelID string) {
	api.On("GetBot", "u-other", true).Return(nil, testAppErr("not a bot")).Maybe()
	api.On("GetUser", "u-other").Return(&model.User{Id: "u-other", Username: "other"}, nil).Maybe()
	api.On("GetChannelMember", channelID, mock.Anything).Return(nil, testAppErr("not a member")).Maybe()
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
	expectDocsBookmarkAttempt(api)
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
	expectSeededAdd(api, "root", "sa1")
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
	expectSeededAdd(api, "c1", "sa1")

	p.addSystemAdminsToChannel("c1")

	api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
}

func TestAddSystemAdminsToChannel_RecordsOnlyNewMemberships(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1"}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "existing"}, {Id: "new"}, {Id: "gone", DeleteAt: 1}}, nil)
	api.On("GetChannelMember", "c1", "existing").Return(&model.ChannelMember{UserId: "existing"}, nil)
	var recorded []byte
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { recorded = args.Get(2).([]byte) }).Return(true, nil)
	expectSeededAdd(api, "c1", "new")

	p.addSystemAdminsToChannel("c1")

	require.JSONEq(t, `["new"]`, string(recorded))
	api.AssertNotCalled(t, "AddChannelMember", "c1", "existing")
	api.AssertNotCalled(t, "GetChannelMember", "c1", "gone")
}

func TestAddSystemAdminsToChannel_PagesPastFirstHundred(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1"}, nil)
	first := make([]*model.User, systemAdminPageSize)
	for i := range first {
		first[i] = &model.User{Id: fmt.Sprintf("sa%03d", i)}
		api.On("GetChannelMember", "c1", first[i].Id).Return(&model.ChannelMember{}, nil)
	}
	api.On("GetUsers", mock.MatchedBy(func(o *model.UserGetOptions) bool { return o.Page == 0 })).Return(first, nil).Once()
	api.On("GetUsers", mock.MatchedBy(func(o *model.UserGetOptions) bool { return o.Page == 1 })).
		Return([]*model.User{{Id: "sa-late"}}, nil).Once()
	expectSeededAdd(api, "c1", "sa-late")

	p.addSystemAdminsToChannel("c1")

	api.AssertCalled(t, "AddChannelMember", "c1", "sa-late")
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
	api.On("GetChannelMember", "c1", "sa1").Return(nil, notFoundErr())
	api.On("AddChannelMember", "c1", "sa1").Return(nil, testAppErr("denied"))
	expectSeededAdd(api, "c1", "sa2")
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
	expectDocsBookmarkAttempt(api)
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
	notFound := model.NewAppError("test", "not_found", nil, "", http.StatusNotFound)
	api.On("GetChannelMember", "c1", "ta1").Return(nil, notFound)
	api.On("GetChannelMember", "c1", "sa1").Return(nil, notFound)
	api.On("AddChannelMember", "c1", "ta1").Return(nil, testAppErr("denied"))
	api.On("AddChannelMember", "c1", "sa1").Return(&model.ChannelMember{}, nil)
	api.On("KVGet", taVisibilitySeededKey("c1")).Return(nil, nil)
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(nil), []byte(`["sa1"]`)).Return(true, nil)
	api.On("GetChannelSidebarCategories", "sa1", "team1").Return(nil, testAppErr("down"))

	p.addTeamApproversToChannel("team1", "c1")

	api.AssertNotCalled(t, "AddChannelMember", "c1", "bot-user-id")
	api.AssertNotCalled(t, "AddChannelMember", "c1", "plain")
	api.AssertNotCalled(t, "GetChannelSidebarCategories", "ta1", "team1")
}

func TestAddTeamApproversToChannel_RollsBackAddWhenLedgerWriteFails(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{
		{UserId: "ta1", Roles: "team_user team_admin"},
	}, nil)
	notFound := model.NewAppError("test", "not_found", nil, "", http.StatusNotFound)
	api.On("GetChannelMember", "c1", "ta1").Return(nil, notFound)
	api.On("AddChannelMember", "c1", "ta1").Return(&model.ChannelMember{}, nil)
	api.On("KVGet", taVisibilitySeededKey("c1")).Return(nil, testAppErr("kv down"))
	api.On("DeleteChannelMember", "c1", "ta1").Return(nil).Once()

	p.addTeamApproversToChannel("team1", "c1")

	api.AssertCalled(t, "DeleteChannelMember", "c1", "ta1")
	api.AssertNotCalled(t, "GetChannelSidebarCategories", "ta1", "team1")
}

func TestAddTeamApproversToChannel_DoesNotMarkExistingMembersAsSeeded(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil)
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{
		{UserId: "ta1", Roles: "team_user team_admin"},
	}, nil)
	api.On("GetChannelMember", "c1", "ta1").Return(&model.ChannelMember{UserId: "ta1"}, nil)
	api.On("GetChannelSidebarCategories", "ta1", "team1").Return(nil, testAppErr("down"))

	p.addTeamApproversToChannel("team1", "c1")

	api.AssertNotCalled(t, "AddChannelMember", "c1", "ta1")
	api.AssertNotCalled(t, "KVCompareAndSet", mock.MatchedBy(func(key string) bool {
		return strings.HasPrefix(key, kvTAVisibilitySeededPrefix)
	}), mock.Anything, mock.Anything)
}

func TestSeedRequestTeamApproversForCard_OnlyOwnPerTeamChannel(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	defer api.AssertExpectations(t)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()

	// Root fallback (different team) — must not seed.
	root := &model.Channel{Id: "root", TeamId: "approval-team", Name: "mattermost-channel-requests"}
	p.seedRequestTeamApproversForCard("team1", root)
	api.AssertNotCalled(t, "GetTeamMembers", mock.Anything, mock.Anything, mock.Anything)

	// Own per-team channel — seeds.
	own := &model.Channel{Id: "own", TeamId: "team1", Name: "mattermost-channel-requests"}
	api.On("GetTeamMembers", "team1", 0, 200).Return([]*model.TeamMember{}, nil).Once()
	p.seedRequestTeamApproversForCard("team1", own)
	api.AssertNumberOfCalls(t, "GetTeamMembers", 1)

	// PerTeam off — never seed on card post.
	p.setConfiguration(&configuration{PerTeamApprovalChannels: false})
	p.seedRequestTeamApproversForCard("team1", own)
	api.AssertNumberOfCalls(t, "GetTeamMembers", 1)
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
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	admin := []*model.TeamMember{{UserId: "ta1", SchemeAdmin: true}}
	// Fast path looks up the channel first for every team.
	api.On("GetChannelByName", "quiet", "mattermost-channel-requests", true).Return(nil, testAppErr("not found"))
	api.On("GetTeamMembers", "quiet", 0, 200).Return([]*model.TeamMember{}, nil)
	api.On("GetChannelByName", "ops", "mattermost-channel-requests", true).
		Return(pluginChannelIn("opsch", "mattermost-channel-requests", "ops"), nil)
	api.On("GetChannelByName", "town", "mattermost-channel-requests", true).Return(takenChannel("townch"), nil)
	stubRefuseAdopt(api, "townch")
	api.On("GetTeamMembers", "town", 0, 200).Return(admin, nil)

	p.ensureAllTeamApprovalChannels()

	api.AssertNotCalled(t, "GetTeamMembers", "ops", mock.Anything, mock.Anything)
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
	stubRefuseAdopt(api, "other")
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
	stubRefuseAdopt(api, "x")

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
	stubRefuseAdopt(api, "x")

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
	api.On("GetChannelMember", "ch-team1", "sa1").Return(nil, model.NewAppError("test", "not_found", nil, "", http.StatusNotFound))
	api.On("AddChannelMember", "ch-team1", "sa1").Return(nil, testAppErr("denied"))

	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{TeamId: "team1", UserId: "sa1"}, &model.User{Id: "other"})

	api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
}

func TestUserHasJoinedTeam_RecordsAddedSystemAdmin(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	defer api.AssertExpectations(t)
	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).
		Return(pluginChannelIn("ch-team1", "mattermost-channel-requests", "team1"), nil)
	api.On("GetChannelMember", "ch-team1", "sa1").Return(nil, model.NewAppError("test", "not_found", nil, "", http.StatusNotFound))
	api.On("AddChannelMember", "ch-team1", "sa1").Return(&model.ChannelMember{}, nil)
	api.On("KVGet", taVisibilitySeededKey("ch-team1")).Return(nil, nil)
	api.On("KVCompareAndSet", taVisibilitySeededKey("ch-team1"), []byte(nil), []byte(`["sa1"]`)).Return(true, nil)
	api.On("GetChannelSidebarCategories", "sa1", "team1").Return(nil, testAppErr("down"))

	p.UserHasJoinedTeam(&plugin.Context{}, &model.TeamMember{TeamId: "team1", UserId: "sa1"},
		&model.User{Id: "sa1", Roles: "system_user system_admin"})
}

// --- seed ledger ownership ---

func TestEvictSeededMember_DeletesBeforeClearingLedger(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	ch := &model.Channel{Id: "c1", TeamId: "t1"}
	var order []string
	api.On("KVGet", taVisibilitySeededKey("c1")).Return([]byte(`["u1"]`), nil)
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(`["u1"]`), []byte(`[]`)).
		Run(func(mock.Arguments) { order = append(order, "clear") }).Return(true, nil)
	api.On("DeleteChannelMember", "c1", "u1").
		Run(func(mock.Arguments) { order = append(order, "delete") }).Return(nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1"}, nil)
	api.On("GetChannelMember", "c1", "u1").Return(nil, notFoundErr())

	evicted, err := p.evictSeededMember(ch, "u1", nil)

	require.Nil(t, err)
	require.True(t, evicted)
	require.Equal(t, []string{"delete", "clear"}, order)
}

// The ledger entry survives a failed delete so the next sweep retries.
func TestEvictSeededMember_DeleteFailureKeepsLedger(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	ch := &model.Channel{Id: "c1", TeamId: "t1"}
	api.On("DeleteChannelMember", "c1", "u1").Return(testAppErr("down"))

	evicted, err := p.evictSeededMember(ch, "u1", func() bool { return true })

	require.NotNil(t, err)
	require.False(t, evicted)
	api.AssertNotCalled(t, "KVGet", taVisibilitySeededKey("c1"))
	api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
}

func TestEvictSeededMember_ClearFailureStillRevokes(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	ch := &model.Channel{Id: "c1", TeamId: "t1"}
	api.On("DeleteChannelMember", "c1", "u1").Return(nil).Once()
	api.On("KVGet", taVisibilitySeededKey("c1")).Return(nil, testAppErr("kv down")).Once()
	api.On("GetUser", "u1").Return(&model.User{Id: "u1"}, nil)
	api.On("GetChannelMember", "c1", "u1").Return(nil, notFoundErr())

	evicted, err := p.evictSeededMember(ch, "u1", nil)

	require.Nil(t, err)
	require.True(t, evicted)
	api.AssertCalled(t, "DeleteChannelMember", "c1", "u1")
}

// A seed that re-added and recorded the user between the delete and the
// clear has its record restored rather than left unrecorded.
func TestEvictSeededMember_PromotedAndReaddedMemberIsRecorded(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	ch := &model.Channel{Id: "c1", TeamId: "t1"}
	api.On("DeleteChannelMember", "c1", "u1").Return(nil).Once()
	api.On("KVGet", taVisibilitySeededKey("c1")).Return([]byte(`["u1"]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(`["u1"]`), []byte(`[]`)).Return(true, nil).Once()
	api.On("GetChannelMember", "c1", "u1").Return(&model.ChannelMember{UserId: "u1"}, nil).Once()
	api.On("KVGet", taVisibilitySeededKey("c1")).Return([]byte(`[]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(`[]`), []byte(`["u1"]`)).Return(true, nil).Once()
	api.On("GetUser", "u1").Return(&model.User{Id: "u1"}, nil)

	evicted, err := p.evictSeededMember(ch, "u1", func() bool { return true })

	require.Nil(t, err)
	require.False(t, evicted)
}

// stubEvictedLedger stubs a successful delete and ledger clear of u1 on c1.
func stubEvictedLedger(api *plugintest.API) {
	api.On("DeleteChannelMember", "c1", "u1").Return(nil).Once()
	api.On("KVGet", taVisibilitySeededKey("c1")).Return([]byte(`["u1"]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(`["u1"]`), []byte(`[]`)).Return(true, nil).Once()
}

// A user re-promoted to System Admin and re-added by a concurrent seed keeps
// their ledger entry even when the caller's Team Admin check fails.
func TestEvictSeededMember_RepromotedSystemAdminReaddedIsRecorded(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	stubEvictedLedger(api)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemAdminRoleId}, nil)
	api.On("GetChannelMember", "c1", "u1").Return(&model.ChannelMember{UserId: "u1"}, nil).Once()
	api.On("KVGet", taVisibilitySeededKey("c1")).Return([]byte(`[]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(`[]`), []byte(`["u1"]`)).Return(true, nil).Once()

	evicted, err := p.evictSeededMember(&model.Channel{Id: "c1", TeamId: "t1"}, "u1", func() bool { return false })

	require.Nil(t, err)
	require.False(t, evicted)
}

func TestEvictSeededMember_RepromotedSystemAdminNotReaddedIsAddedBack(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	stubEvictedLedger(api)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemAdminRoleId}, nil)
	api.On("GetChannelMember", "c1", "u1").Return(nil, notFoundErr()).Twice()
	api.On("AddChannelMember", "c1", "u1").Return(&model.ChannelMember{}, nil).Once()
	api.On("KVGet", taVisibilitySeededKey("c1")).Return([]byte(`[]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(`[]`), []byte(`["u1"]`)).Return(true, nil).Once()

	evicted, err := p.evictSeededMember(&model.Channel{Id: "c1", TeamId: "t1"}, "u1", nil)

	require.Nil(t, err)
	require.False(t, evicted)
}

// A failed role lookup still re-records a re-added member so their seat
// stays revocable.
func TestEvictSeededMember_RoleLookupFailureRerecordsMember(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	stubEvictedLedger(api)
	api.On("GetUser", "u1").Return(nil, testAppErr("down"))
	api.On("GetChannelMember", "c1", "u1").Return(&model.ChannelMember{UserId: "u1"}, nil).Once()
	api.On("KVGet", taVisibilitySeededKey("c1")).Return([]byte(`[]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(`[]`), []byte(`["u1"]`)).Return(true, nil).Once()

	evicted, err := p.evictSeededMember(&model.Channel{Id: "c1", TeamId: "t1"}, "u1", nil)

	require.Nil(t, err)
	require.False(t, evicted)
}

// A failed role lookup never adds anyone back.
func TestEvictSeededMember_RoleLookupFailureNeverReadds(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	stubEvictedLedger(api)
	api.On("GetUser", "u1").Return(nil, testAppErr("down"))
	api.On("GetChannelMember", "c1", "u1").Return(nil, notFoundErr()).Once()

	evicted, err := p.evictSeededMember(&model.Channel{Id: "c1", TeamId: "t1"}, "u1", nil)

	require.Nil(t, err)
	require.True(t, evicted)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestEvictSeededMember_DeactivatedSystemAdminIsRemoved(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	stubEvictedLedger(api)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemAdminRoleId, DeleteAt: 1}, nil)
	api.On("GetChannelMember", "c1", "u1").Return(nil, notFoundErr())

	evicted, err := p.evictSeededMember(&model.Channel{Id: "c1", TeamId: "t1"}, "u1", nil)

	require.Nil(t, err)
	require.True(t, evicted)
}

// A concurrent seed that re-adds a non-qualifying user between delete and
// clear is stripped again so they are not left in-channel without a ledger.
func TestEvictSeededMember_NonQualifyingReaddIsRestipped(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	stubEvictedLedger(api)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1"}, nil)
	api.On("GetChannelMember", "c1", "u1").Return(&model.ChannelMember{UserId: "u1"}, nil).Once()
	api.On("DeleteChannelMember", "c1", "u1").Return(nil).Once()
	api.On("KVGet", taVisibilitySeededKey("c1")).Return([]byte(`["u1"]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("c1"), []byte(`["u1"]`), []byte(`[]`)).Return(true, nil).Once()

	evicted, err := p.evictSeededMember(&model.Channel{Id: "c1", TeamId: "t1"}, "u1", nil)

	require.Nil(t, err)
	require.True(t, evicted)
	api.AssertNumberOfCalls(t, "DeleteChannelMember", 2)
}

func TestStripAdoptedApprovalChannel_RefusesWhenSystemAdminCannotBeRecorded(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	api.On("GetChannelMembers", "c1", 0, 100).Return(model.ChannelMembers{{UserId: "sa1"}}, nil)
	api.On("GetUser", "sa1").Return(&model.User{Id: "sa1", Roles: model.SystemAdminRoleId}, nil)
	api.On("KVGet", taVisibilitySeededKey("c1")).Return(nil, testAppErr("kv down"))

	require.False(t, p.stripAdoptedApprovalChannelToPrivileged(&model.Channel{Id: "c1"}))
}

// seedTAVisibility re-checks eligibility after the add: a toggle that went
// off after the caller's snapshot must not leave a stale seed.
func TestSeedTAVisibility_UndoesSeedThatNoLongerQualifies(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{AllowApprovalTeamAdminsForTeamRequests: false})
	defer api.AssertExpectations(t)
	ch := &model.Channel{Id: "ch-team", TeamId: "hq", Name: "mattermost-team-requests"}
	api.On("GetChannelMember", "ch-team", "ta1").Return(nil, notFoundErr()).Once()
	api.On("CreateTeamMember", "hq", "ta1").Return(&model.TeamMember{}, nil)
	api.On("AddChannelMember", "ch-team", "ta1").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelSidebarCategories", "ta1", "hq").Return(nil, testAppErr("down"))
	api.On("GetChannelMember", "ch-team", "ta1").Return(&model.ChannelMember{UserId: "ta1"}, nil).Once()
	api.On("KVGet", taVisibilitySeededKey("ch-team")).Return(nil, nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("ch-team"), []byte(nil), []byte(`["ta1"]`)).Return(true, nil).Once()
	api.On("GetTeamMember", "hq", "ta1").Return(&model.TeamMember{TeamId: "hq", UserId: "ta1", SchemeAdmin: true}, nil)
	api.On("KVGet", taVisibilitySeededKey("ch-team")).Return([]byte(`["ta1"]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("ch-team"), []byte(`["ta1"]`), []byte(`[]`)).Return(true, nil).Once()
	api.On("DeleteChannelMember", "ch-team", "ta1").Return(nil).Once()
	api.On("GetUser", "ta1").Return(&model.User{Id: "ta1"}, nil)
	api.On("GetChannelMember", "ch-team", "ta1").Return(nil, notFoundErr()).Once()

	p.seedTAVisibility(ch, "ta1")
}

func TestSeedTAVisibility_RollsBackAddWhenLedgerWriteFails(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{AllowApprovalTeamAdminsForTeamRequests: true})
	defer api.AssertExpectations(t)
	ch := &model.Channel{Id: "ch-team", TeamId: "hq", Name: "mattermost-team-requests"}
	api.On("GetChannelMember", "ch-team", "ta1").Return(nil, notFoundErr()).Once()
	api.On("CreateTeamMember", "hq", "ta1").Return(&model.TeamMember{}, nil)
	api.On("AddChannelMember", "ch-team", "ta1").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelSidebarCategories", "ta1", "hq").Return(nil, testAppErr("down"))
	api.On("GetChannelMember", "ch-team", "ta1").Return(&model.ChannelMember{UserId: "ta1"}, nil).Once()
	api.On("KVGet", taVisibilitySeededKey("ch-team")).Return(nil, testAppErr("kv down"))
	api.On("DeleteChannelMember", "ch-team", "ta1").Return(nil).Once()

	p.seedTAVisibility(ch, "ta1")

	api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
}

func TestReconcilePerTeamApprovalAccessForUser_DeleteFailureWarnsAndKeepsLedger(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	defer api.AssertExpectations(t)
	ops := &model.Team{Id: "team-ops", Name: "ops"}
	opsCh := pluginChannelIn("ch-ops", "mattermost-channel-requests", "team-ops")
	api.On("GetTeamsForUser", "demoted").Return([]*model.Team{ops}, nil)
	api.On("GetChannelByName", "team-ops", "mattermost-channel-requests", true).Return(opsCh, nil)
	api.On("GetTeamMember", "team-ops", "demoted").Return(&model.TeamMember{UserId: "demoted", Roles: model.TeamUserRoleId}, nil)
	api.On("KVGet", taVisibilitySeededKey("ch-ops")).Return([]byte(`["demoted"]`), nil).Once()
	api.On("GetChannelMember", "ch-ops", "demoted").Return(&model.ChannelMember{UserId: "demoted"}, nil)
	api.On("DeleteChannelMember", "ch-ops", "demoted").Return(testAppErr("down"))

	p.reconcilePerTeamApprovalAccessForUser(&model.User{Id: "demoted", Username: "ex"})

	api.AssertCalled(t, "LogWarn", "per-team approval: failed to evict demoted Team Admin on login",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
}

// A promotion that lands mid-eviction on login re-seeds the user (and the
// ledger) instead of leaving them evicted or as an unrecorded member.
func TestReconcilePerTeamApprovalAccessForUser_PromotedMidEvictionIsReseeded(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{PerTeamApprovalChannels: true})
	defer api.AssertExpectations(t)
	ops := &model.Team{Id: "team-ops", Name: "ops"}
	opsCh := pluginChannelIn("ch-ops", "mattermost-channel-requests", "team-ops")
	api.On("GetTeamsForUser", "u1").Return([]*model.Team{ops}, nil)
	api.On("GetChannelByName", "team-ops", "mattermost-channel-requests", true).Return(opsCh, nil)
	api.On("GetTeamMember", "team-ops", "u1").Return(&model.TeamMember{UserId: "u1", Roles: model.TeamUserRoleId}, nil).Once()
	api.On("GetTeamMember", "team-ops", "u1").Return(&model.TeamMember{TeamId: "team-ops", UserId: "u1", SchemeAdmin: true}, nil)
	api.On("KVGet", taVisibilitySeededKey("ch-ops")).Return([]byte(`["u1"]`), nil).Times(2)
	api.On("GetChannelMember", "ch-ops", "u1").Return(&model.ChannelMember{UserId: "u1"}, nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("ch-ops"), []byte(`["u1"]`), []byte(`[]`)).Return(true, nil).Once()
	api.On("DeleteChannelMember", "ch-ops", "u1").Return(nil).Once()
	api.On("GetUser", "u1").Return(&model.User{Id: "u1"}, nil)
	// Post-clear recheck, then seedTAVisibility's own lookup.
	api.On("GetChannelMember", "ch-ops", "u1").Return(nil, notFoundErr()).Twice()
	api.On("CreateTeamMember", "team-ops", "u1").Return(&model.TeamMember{}, nil)
	api.On("AddChannelMember", "ch-ops", "u1").Return(&model.ChannelMember{}, nil).Once()
	api.On("GetChannelSidebarCategories", "u1", "team-ops").Return(nil, testAppErr("down"))
	api.On("GetChannelMember", "ch-ops", "u1").Return(&model.ChannelMember{UserId: "u1"}, nil).Once()
	api.On("KVGet", taVisibilitySeededKey("ch-ops")).Return([]byte(`[]`), nil).Once()
	api.On("KVCompareAndSet", taVisibilitySeededKey("ch-ops"), []byte(`[]`), []byte(`["u1"]`)).Return(true, nil).Once()

	p.reconcilePerTeamApprovalAccessForUser(&model.User{Id: "u1", Username: "u1"})

	api.AssertNotCalled(t, "LogInfo", "per-team approval: evicted demoted Team Admin",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestAddUserToApprovalChannelIfTeamMember_RecordsOnlyNewMemberships(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	newCh := &model.Channel{Id: "c-new", TeamId: "hq"}
	oldCh := &model.Channel{Id: "c-old", TeamId: "hq"}
	api.On("GetTeamMember", "hq", "sa1").Return(&model.TeamMember{TeamId: "hq", UserId: "sa1"}, nil)
	api.On("GetChannelSidebarCategories", "sa1", "hq").Return(nil, testAppErr("down"))

	api.On("GetChannelMember", "c-new", "sa1").Return(nil, notFoundErr())
	api.On("AddChannelMember", "c-new", "sa1").Return(&model.ChannelMember{}, nil)
	api.On("KVGet", taVisibilitySeededKey("c-new")).Return(nil, nil)
	api.On("KVCompareAndSet", taVisibilitySeededKey("c-new"), []byte(nil), []byte(`["sa1"]`)).Return(true, nil)
	p.addUserToApprovalChannelIfTeamMember(newCh, "sa1")

	// Possibly hand-added: left unrecorded so it is never evicted.
	api.On("GetChannelMember", "c-old", "sa1").Return(&model.ChannelMember{UserId: "sa1"}, nil)
	p.addUserToApprovalChannelIfTeamMember(oldCh, "sa1")

	api.AssertNotCalled(t, "AddChannelMember", "c-old", "sa1")
	api.AssertNotCalled(t, "KVGet", taVisibilitySeededKey("c-old"))
}

func TestAddUserToApprovalChannelIfTeamMember_LookupErrorSkipsAdd(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	ch := &model.Channel{Id: "c1", TeamId: "hq"}
	api.On("GetTeamMember", "hq", "sa1").Return(&model.TeamMember{TeamId: "hq", UserId: "sa1"}, nil)
	api.On("GetChannelMember", "c1", "sa1").Return(nil, testAppErr("down"))

	p.addUserToApprovalChannelIfTeamMember(ch, "sa1")

	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}
