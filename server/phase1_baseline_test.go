package main

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestCreateChannelForRequest_BotJoinFailureIsNonFatal(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "team-x"}, nil)
	api.On("GetTeamMember", "team1", "bot-user-id").Return(nil, testAppErr("not a member"))
	api.On("CreateTeamMember", "team1", "bot-user-id").Return(nil, testAppErr("team is full"))
	api.On("AddChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_req", channelAdminRoles).Return(&model.ChannelMember{}, nil)

	channel, err := p.createChannelForRequest(&channelRequest{RequesterID: "u_req", TeamID: "team1", Name: "team-x", ChannelType: "O"})

	require.NoError(t, err)
	require.Equal(t, "ch1", channel.Id)
	api.AssertNotCalled(t, "AddChannelMember", "ch1", "bot-user-id")
	api.AssertCalled(t, "LogWarn", "failed to add the plugin bot to created channel", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestApprovalChannelDisplayName(t *testing.T) {
	long := strings.Repeat("Ü", 40)
	cases := []struct {
		name     string
		team     *model.Team
		approval bool
		want     string
	}{
		{"approval team keeps the plain name", &model.Team{Name: "hq", DisplayName: "HQ"}, true, "Mattermost: Channel Requests"},
		{"other team adds its display name", &model.Team{Name: "eng", DisplayName: "Engineering"}, false, "Mattermost: Channel Requests (Engineering)"},
		{"blank display name falls back to the URL name", &model.Team{Name: "eng", DisplayName: "  "}, false, "Mattermost: Channel Requests (eng)"},
		{"no team", nil, false, "Mattermost: Channel Requests"},
		{"long team name is cut to fit", &model.Team{Name: "x", DisplayName: long}, false, "Mattermost: Channel Requests (" + strings.Repeat("Ü", 32) + "…)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := approvalChannelDisplayName(tc.team, tc.approval)
			require.Equal(t, tc.want, got)
			require.LessOrEqual(t, utf8.RuneCountInString(got), model.ChannelDisplayNameMaxRunes)
		})
	}
}

func TestIsGeneratedChannelRequestsName(t *testing.T) {
	require.True(t, isGeneratedChannelRequestsName("Mattermost: Channel Requests"))
	require.True(t, isGeneratedChannelRequestsName("Mattermost: Channel Requests (Engineering)"))
	require.False(t, isGeneratedChannelRequestsName("Engineering approvals"))
	require.False(t, isGeneratedChannelRequestsName("Mattermost: Channel Requests for Eng"))
}

func TestEnsureTeamApprovalChannel_NamesTheChannelAfterTheTeam(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	expectDocsBookmarkAttempt(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "hq"})

	api.On("GetChannelByName", "team1", "mattermost-channel-requests", true).Return(nil, testAppErr("not found"))
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	api.On("GetTeamMember", "team1", "bot-user-id").Return(&model.TeamMember{}, nil)
	api.On("CreateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.DisplayName == "Mattermost: Channel Requests (Engineering)"
	})).Return(&model.Channel{Id: "new-ch", TeamId: "team1"}, nil)
	api.On("GetTeamMembers", "team1", mock.Anything, mock.Anything).Return([]*model.TeamMember{}, nil).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()

	ch, err := p.ensureTeamApprovalChannel("team1")
	require.NoError(t, err)
	require.Equal(t, "new-ch", ch.Id)
	api.AssertCalled(t, "CreateChannel", mock.Anything)
}

func TestSyncApprovalChannelDisplayNames(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "hq"})

	named := func(id, teamID, displayName string) *model.Channel {
		ch := pluginChannelIn(id, "mattermost-channel-requests", teamID)
		ch.DisplayName = displayName
		return ch
	}
	api.On("GetTeams").Return([]*model.Team{
		{Id: "t-hq", Name: "hq", DisplayName: "HQ"},
		{Id: "t-eng", Name: "eng", DisplayName: "Engineering"},
		{Id: "t-ops", Name: "ops", DisplayName: "Ops"},
		{Id: "t-sec", Name: "sec", DisplayName: "Security"},
		{Id: "t-new", Name: "new", DisplayName: "New"},
		{Id: "t-gone", Name: "gone", DisplayName: "Gone", DeleteAt: 1},
	}, nil)
	// The old root name in the new Approval team goes plain.
	api.On("GetChannelByName", "t-hq", "mattermost-channel-requests", true).Return(named("c-hq", "t-hq", "Mattermost: Channel Requests (HQ)"), nil)
	// A per-team channel from before gets its team's name.
	api.On("GetChannelByName", "t-eng", "mattermost-channel-requests", true).Return(named("c-eng", "t-eng", "Mattermost: Channel Requests"), nil)
	// Already right: untouched.
	api.On("GetChannelByName", "t-ops", "mattermost-channel-requests", true).Return(named("c-ops", "t-ops", "Mattermost: Channel Requests (Ops)"), nil)
	// Renamed by hand: untouched.
	api.On("GetChannelByName", "t-sec", "mattermost-channel-requests", true).Return(named("c-sec", "t-sec", "SOC approvals"), nil)
	// No channel in this team.
	api.On("GetChannelByName", "t-new", "mattermost-channel-requests", true).Return(nil, testAppErr("not found"))

	var mu sync.Mutex
	renamed := map[string]string{}
	api.On("UpdateChannel", mock.Anything).Run(func(args mock.Arguments) {
		ch := args.Get(0).(*model.Channel)
		mu.Lock()
		renamed[ch.Id] = ch.DisplayName
		mu.Unlock()
	}).Return(&model.Channel{}, nil)

	p.syncApprovalChannelDisplayNames()

	require.Equal(t, map[string]string{
		"c-hq":  "Mattermost: Channel Requests",
		"c-eng": "Mattermost: Channel Requests (Engineering)",
	}, renamed)
	api.AssertNotCalled(t, "GetChannelByName", "t-gone", mock.Anything, mock.Anything)
}

func TestSyncApprovalChannelDisplayNames_ToleratesFailures(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "hq"})

	ch := pluginChannelIn("c-eng", "mattermost-channel-requests", "t-eng")
	ch.DisplayName = "Mattermost: Channel Requests"
	api.On("GetTeams").Return([]*model.Team{{Id: "t-eng", Name: "eng", DisplayName: "Engineering"}}, nil)
	api.On("GetChannelByName", "t-eng", "mattermost-channel-requests", true).Return(ch, nil)
	api.On("UpdateChannel", mock.Anything).Return(nil, testAppErr("denied"))

	p.syncApprovalChannelDisplayNames()
	api.AssertCalled(t, "LogWarn", "channel requests names: failed to rename channel", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)

	api2 := &plugintest.API{}
	stubLogs(api2)
	p2 := newTestPlugin(api2)
	api2.On("GetTeams").Return(nil, testAppErr("down"))
	p2.syncApprovalChannelDisplayNames()
	api2.AssertNotCalled(t, "UpdateChannel", mock.Anything)
}

func TestOnConfigurationChange_NewApprovalTeamEnsuresEverySharedChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	// Blank Approval team and both Team Admin toggles off: before, setting
	// the team created none of the shared channels until a restart.
	p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})
	stubLoadConfiguration(api, configuration{ApprovalTeam: "hq"})
	api.On("KVDelete", kvApprovalTeamKey).Return(nil)
	api.On("GetTeamByName", "hq").Return(&model.Team{Id: "team-hq", Name: "hq", DisplayName: "HQ"}, nil)
	stubApprovalTeamRecord(api)

	shared := []string{
		"mattermost-channel-requests",
		"mattermost-bot-requests",
		"mattermost-webhook-requests",
		"mattermost-team-requests",
		"mattermost-audit-requests",
	}
	var mu sync.Mutex
	looked := map[string]bool{}
	for _, name := range shared {
		ch := pluginChannelIn("ch-"+name, name, "team-hq")
		api.On("GetChannelByName", "team-hq", name, true).Run(func(mock.Arguments) {
			mu.Lock()
			looked[name] = true
			mu.Unlock()
		}).Return(ch, nil)
	}
	api.On("GetChannel", mock.Anything).Return(nil, testAppErr("skip sidebar")).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team-hq", mock.Anything, mock.Anything).Return([]*model.TeamMember{}, nil).Maybe()
	api.On("KVGet", mock.Anything).Return(nil, nil).Maybe()
	api.On("GetChannelMembers", mock.Anything, mock.Anything, mock.Anything).Return(model.ChannelMembers{}, nil).Maybe()
	done := make(chan struct{})
	api.On("GetTeams").Run(func(mock.Arguments) { close(done) }).Return([]*model.Team{}, nil).Once()

	require.NoError(t, p.OnConfigurationChange())
	waitFor(t, done, "Approval team setup")

	mu.Lock()
	defer mu.Unlock()
	for _, name := range shared {
		require.True(t, looked[name], "expected %s to be ensured", name)
	}
}
