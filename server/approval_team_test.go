package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newApprovalTeamPlugin returns a plugin whose Approval team setting is
// "appteam", with only log stubs registered so KV calls can be asserted.
func newApprovalTeamPlugin(t *testing.T) (*Plugin, *plugintest.API) {
	t.Helper()
	api := &plugintest.API{}
	for _, method := range []string{"LogDebug", "LogInfo", "LogWarn", "LogError"} {
		for arity := 1; arity <= 16; arity++ {
			args := make([]any, arity)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(method, args...).Maybe()
		}
	}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", TwoStepApproverMode: twoStepModeSystemAdmin})
	return p, api
}

func approvalTeamRecordBytes(t *testing.T, name, id string) []byte {
	t.Helper()
	data, err := json.Marshal(approvalTeamRecord{Name: name, ID: id})
	require.NoError(t, err)
	return data
}

func TestApprovalTeam_BlankSettingReturnsNothing(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	team, err := p.approvalTeam()

	require.NoError(t, err)
	require.Nil(t, team)
	api.AssertNotCalled(t, "GetTeamByName", mock.Anything)
}

func TestApprovalTeam_MissingTeamIsAnError(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(nil, testAppErr("not found"))

	team, err := p.approvalTeam()

	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
	require.Nil(t, team)
}

func TestApprovalTeam_ArchivedTeamIsRefused(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam", DeleteAt: 5}, nil)

	team, err := p.approvalTeam()

	require.Error(t, err)
	require.Contains(t, err.Error(), "archived")
	require.Nil(t, team)
	api.AssertNotCalled(t, "KVGet", mock.Anything)
}

func TestApprovalTeam_RecordLoadFailureIsAnError(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(nil, testAppErr("kv down"))

	team, err := p.approvalTeam()

	require.Error(t, err)
	require.Nil(t, team)
}

func TestApprovalTeam_FirstUseRecordsTeam(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	defer api.AssertExpectations(t)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(nil, nil)
	api.On("KVCompareAndSet", kvApprovalTeamKey, []byte(nil), approvalTeamRecordBytes(t, "appteam", "t1")).Return(true, nil)

	team, err := p.approvalTeam()

	require.NoError(t, err)
	require.Equal(t, "t1", team.Id)
}

func TestApprovalTeam_RecordedTeamIsReturned(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(approvalTeamRecordBytes(t, "appteam", "t1"), nil)

	team, err := p.approvalTeam()

	require.NoError(t, err)
	require.Equal(t, "t1", team.Id)
	api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
}

func TestApprovalTeam_DifferentTeamWithRecordedNameIsAnError(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	// The recorded team was renamed and another team now has its name.
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t2", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(approvalTeamRecordBytes(t, "appteam", "t1"), nil)

	team, err := p.approvalTeam()

	require.Error(t, err)
	require.Contains(t, err.Error(), "set Approval team to a different team, save, then set it back")
	require.Nil(t, team)
	api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
	api.AssertCalled(t, "LogError", mock.MatchedBy(func(msg string) bool {
		return strings.Contains(msg, "set Approval team to a different team, save, then set it back")
	}), "team", "appteam", "recorded_team_id", "t1", "team_id", "t2")
}

func TestApprovalTeam_NewSettingNameReplacesRecord(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	defer api.AssertExpectations(t)
	old := approvalTeamRecordBytes(t, "oldteam", "t1")
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t2", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(old, nil)
	api.On("KVCompareAndSet", kvApprovalTeamKey, old, approvalTeamRecordBytes(t, "appteam", "t2")).Return(true, nil)

	team, err := p.approvalTeam()

	require.NoError(t, err)
	require.Equal(t, "t2", team.Id)
}

func TestApprovalTeam_UnreadableRecordIsAnError(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return([]byte("not json"), nil)

	team, err := p.approvalTeam()

	require.Error(t, err)
	require.Nil(t, team)
	api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
	api.AssertCalled(t, "LogError", mock.MatchedBy(func(msg string) bool {
		return strings.Contains(msg, "set Approval team to a different team, save, then set it back")
	}), "error", mock.Anything)
}

func TestApprovalTeam_LostRecordRaceUsesWinnersRecord(t *testing.T) {
	t.Run("same team recorded", func(t *testing.T) {
		p, api := newApprovalTeamPlugin(t)
		defer api.AssertExpectations(t)
		api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
		api.On("KVGet", kvApprovalTeamKey).Return(nil, nil).Once()
		api.On("KVCompareAndSet", kvApprovalTeamKey, []byte(nil), mock.Anything).Return(false, nil).Once()
		api.On("KVGet", kvApprovalTeamKey).Return(approvalTeamRecordBytes(t, "appteam", "t1"), nil).Once()

		team, err := p.approvalTeam()

		require.NoError(t, err)
		require.Equal(t, "t1", team.Id)
	})
	t.Run("another team recorded", func(t *testing.T) {
		p, api := newApprovalTeamPlugin(t)
		defer api.AssertExpectations(t)
		api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
		api.On("KVGet", kvApprovalTeamKey).Return(nil, nil).Once()
		api.On("KVCompareAndSet", kvApprovalTeamKey, []byte(nil), mock.Anything).Return(false, nil).Once()
		api.On("KVGet", kvApprovalTeamKey).Return(approvalTeamRecordBytes(t, "appteam", "t9"), nil).Once()

		team, err := p.approvalTeam()

		require.Error(t, err)
		require.Nil(t, team)
	})
	t.Run("record keeps changing", func(t *testing.T) {
		p, api := newApprovalTeamPlugin(t)
		defer api.AssertExpectations(t)
		api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
		api.On("KVGet", kvApprovalTeamKey).Return(nil, nil).Twice()
		api.On("KVCompareAndSet", kvApprovalTeamKey, []byte(nil), mock.Anything).Return(false, nil).Twice()

		team, err := p.approvalTeam()

		require.Error(t, err)
		require.Nil(t, team)
	})
}

func TestApprovalTeam_RecordSaveFailureStillReturnsTeam(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(nil, nil)
	api.On("KVCompareAndSet", kvApprovalTeamKey, mock.Anything, mock.Anything).Return(false, testAppErr("kv down"))

	team, err := p.approvalTeam()

	require.NoError(t, err)
	require.Equal(t, "t1", team.Id)
	api.AssertCalled(t, "LogWarn", "failed to record the approval team", "team", "appteam", "error", mock.Anything)
}

func TestForgetApprovalTeam_DeletesRecord(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	defer api.AssertExpectations(t)
	api.On("KVDelete", kvApprovalTeamKey).Return(nil)

	p.forgetApprovalTeam()
}

func TestForgetApprovalTeam_FailureIsLogged(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("KVDelete", kvApprovalTeamKey).Return(testAppErr("kv down"))

	p.forgetApprovalTeam()

	api.AssertCalled(t, "LogWarn", "failed to clear the recorded approval team", "error", mock.Anything)
}

// stubLoadConfiguration makes LoadPluginConfiguration fill in cfg and skips
// the one-time prefix migration.
func stubLoadConfiguration(api *plugintest.API, cfg configuration) {
	// Tests that omit TwoStepApproverMode would otherwise flip to usergroup
	// mode and background-reseed group lookups against an unstubbed mock.
	if strings.TrimSpace(cfg.TwoStepApproverMode) == "" {
		cfg.TwoStepApproverMode = twoStepModeSystemAdmin
	}
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		*args.Get(0).(*configuration) = cfg
	}).Return(nil)
	api.On("KVGet", kvPrefixMaxLenV48).Return([]byte("1"), nil)
}

func TestOnConfigurationChange_NewApprovalTeamForgetsRecord(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	defer api.AssertExpectations(t)
	// Keep system_admin so the two-step fingerprint does not reseed in the background.
	stubLoadConfiguration(api, configuration{ApprovalTeam: "otherteam", TwoStepApproverMode: twoStepModeSystemAdmin})
	api.On("KVDelete", kvApprovalTeamKey).Return(nil).Once()

	require.NoError(t, p.OnConfigurationChange())
	require.Equal(t, "otherteam", p.getConfiguration().ApprovalTeam)
}

func TestOnConfigurationChange_SameApprovalTeamKeepsRecord(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubLoadConfiguration(api, configuration{ApprovalTeam: " appteam ", TwoStepApproverMode: twoStepModeSystemAdmin})

	require.NoError(t, p.OnConfigurationChange())
	api.AssertNotCalled(t, "KVDelete", kvApprovalTeamKey)
}

func TestIsPluginApprovalChannel(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	tests := []struct {
		name string
		ch   *model.Channel
		want bool
	}{
		{"nil", nil, false},
		{"bot private channel", pluginChannel("c1", "x"), true},
		{"open channel", &model.Channel{Type: model.ChannelTypeOpen, CreatorId: "bot-user-id"}, false},
		{"created by a user", &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "u1"}, false},
		{"archived", &model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", DeleteAt: 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, p.isPluginApprovalChannel(tc.ch))
		})
	}
}

func TestLookupApprovalChannel_NoChannel(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(nil, testAppErr("not found"))

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.False(t, taken)
}

func TestLookupApprovalChannel_PluginChannelIsReturned(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(pluginChannel("c1", "approvals"), nil)

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.NotNil(t, ch)
	require.Equal(t, "c1", ch.Id)
	require.False(t, taken)
}

func TestLookupApprovalChannel_OtherChannelWithNameIsTaken(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).
		Return(&model.Channel{Id: "c9", Type: model.ChannelTypeOpen, CreatorId: "u1"}, nil)
	api.On("GetBot", "u1", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemUserRoleId}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member"))

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
	api.AssertCalled(t, "LogError", mock.Anything, "team_id", "t1", "channel_id", "c9", "channel_name", "approvals", "creator_id", "u1")
}

func TestLookupApprovalChannel_ConvertsPublicLegacyBotChannelToPrivate(t *testing.T) {
	// Legacy: mattermost-channel-requests was created as public by the
	// pre-rename bot. Convert to private and adopt so tickets stop DMing
	// System Admins.
	p, api := newApprovalTeamPlugin(t)
	open := &model.Channel{
		Id: "c9", TeamId: "t1", Name: "approvals",
		Type: model.ChannelTypeOpen, CreatorId: "legacy-bot",
	}
	api.On("GetChannelByName", "t1", "approvals", true).Return(open, nil)
	api.On("GetBot", "legacy-bot", true).Return(&model.Bot{
		UserId: "legacy-bot", OwnerId: legacyRequestHubPluginID,
	}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member")).Maybe()
	api.On("UpdateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.Id == "c9" && c.Type == model.ChannelTypePrivate
	})).Return(&model.Channel{
		Id: "c9", TeamId: "t1", Name: "approvals",
		Type: model.ChannelTypePrivate, CreatorId: "legacy-bot",
	}, nil)
	api.On("GetTeamMember", "t1", p.botUserID).Return(&model.TeamMember{TeamId: "t1", UserId: p.botUserID}, nil)
	api.On("AddChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{}, nil)

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.NotNil(t, ch)
	require.Equal(t, model.ChannelTypePrivate, ch.Type)
	require.False(t, taken)
	api.AssertCalled(t, "UpdateChannel", mock.Anything)
}

func TestLookupApprovalChannel_NoAdoptionWhenPublicToPrivateFails(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	open := &model.Channel{
		Id: "c9", TeamId: "t1", Name: "approvals",
		Type: model.ChannelTypeOpen, CreatorId: "legacy-bot",
	}
	api.On("GetChannelByName", "t1", "approvals", true).Return(open, nil)
	api.On("GetBot", "legacy-bot", true).Return(&model.Bot{
		UserId: "legacy-bot", OwnerId: legacyRequestHubPluginID,
	}, nil)
	api.On("UpdateChannel", mock.Anything).Return(nil, testAppErr("cannot convert"))

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestLookupApprovalChannel_AdoptsWhenCreatorDeactivatedAndBotIsMember(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("old-user"), nil)
	api.On("GetBot", "old-user", true).Return(nil, testAppErr("not a bot")).Maybe()
	api.On("GetUser", "old-user").Return(&model.User{Id: "old-user", DeleteAt: 1}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{ChannelId: "c9", UserId: p.botUserID}, nil)
	api.On("GetTeamMember", "t1", p.botUserID).Return(&model.TeamMember{TeamId: "t1", UserId: p.botUserID}, nil)
	api.On("AddChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{}, nil)

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.NotNil(t, ch)
	require.Equal(t, "c9", ch.Id)
	require.False(t, taken)
}

// otherApprovalChannel is a live private channel named like an approval
// channel that the plugin's bot didn't create.
func otherApprovalChannel(creatorID string) *model.Channel {
	return &model.Channel{Id: "c9", TeamId: "t1", Name: "approvals", Type: model.ChannelTypePrivate, CreatorId: creatorID}
}

func TestLookupApprovalChannel_PrivateChannelOfARegularUserIsTaken(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("u1"), nil)
	api.On("GetBot", "u1", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemUserRoleId}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member"))

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestLookupApprovalChannel_AdoptsPrivateChannelOfASystemAdminOrThePluginsBot(t *testing.T) {
	for name, creator := range map[string]*model.User{
		"system admin":          {Id: "u1", Roles: model.SystemUserRoleId + " " + model.SystemAdminRoleId},
		"an earlier plugin bot": {Id: "u1", Roles: model.SystemUserRoleId, IsBot: true},
	} {
		t.Run(name, func(t *testing.T) {
			p, api := newApprovalTeamPlugin(t)
			api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("u1"), nil)
			if creator.IsBot {
				api.On("GetBot", "u1", true).Return(&model.Bot{UserId: "u1", OwnerId: manifest.Id, Username: requestHubBotUsername}, nil).Once()
			} else {
				api.On("GetBot", "u1", true).Return(nil, testAppErr("not a bot")).Once()
				api.On("GetUser", "u1").Return(creator, nil).Once()
			}
			api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member")).Maybe()
			api.On("GetTeamMember", "t1", p.botUserID).Return(&model.TeamMember{TeamId: "t1", UserId: p.botUserID}, nil)
			api.On("AddChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{}, nil).Once()

			ch, taken := p.lookupApprovalChannel("t1", "approvals")
			require.NotNil(t, ch)
			require.Equal(t, "c9", ch.Id)
			require.False(t, taken)
			api.AssertCalled(t, "LogInfo", mock.Anything, "channel_id", "c9", "channel_name", "approvals", "creator_id", "u1")

			// Adopted once: the next lookup doesn't check or join again.
			ch, taken = p.lookupApprovalChannel("t1", "approvals")
			require.NotNil(t, ch)
			require.False(t, taken)
			api.AssertNumberOfCalls(t, "AddChannelMember", 1)
		})
	}
}

// A bot this plugin doesn't own could pre-create the name and read the
// cards, so its channel is refused like a regular user's.
func TestLookupApprovalChannel_PrivateChannelOfAnotherBotIsTaken(t *testing.T) {
	for name, getBot := range map[string]func(api *plugintest.API){
		"owned by another plugin": func(api *plugintest.API) {
			api.On("GetBot", "u1", true).Return(&model.Bot{UserId: "u1", OwnerId: "com.example.other"}, nil)
		},
		"owned by a user": func(api *plugintest.API) {
			api.On("GetBot", "u1", true).Return(&model.Bot{UserId: "u1", OwnerId: "u2"}, nil)
		},
		"bot lookup fails": func(api *plugintest.API) {
			api.On("GetBot", "u1", true).Return(nil, testAppErr("db down"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, api := newApprovalTeamPlugin(t)
			api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("u1"), nil)
			// GetUser only when GetBot fails; OwnerId mismatch refuses without it.
			api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemUserRoleId, IsBot: true, Username: requestHubBotUsername}, nil).Maybe()
			getBot(api)
			api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member"))

			ch, taken := p.lookupApprovalChannel("t1", "approvals")

			require.Nil(t, ch)
			require.True(t, taken)
			api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
		})
	}
}

func TestLookupApprovalChannel_NoAdoptionWhenTheBotCantJoin(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("u1"), nil)
	api.On("GetBot", "u1", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemAdminRoleId}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member")).Maybe()
	api.On("GetTeamMember", "t1", p.botUserID).Return(&model.TeamMember{TeamId: "t1", UserId: p.botUserID}, nil)
	api.On("AddChannelMember", "c9", p.botUserID).Return(nil, testAppErr("no"))

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
}

func TestLookupApprovalChannel_AdoptsWhenBotAlreadyMemberAfterCreatorGone(t *testing.T) {
	// Plugin re-upload / bot recreate: channel CreatorId points at a purged
	// user, but the current bot is still in the channel — keep using it so
	// tickets don't fall back to System Admin DMs.
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("old-bot"), nil)
	api.On("GetBot", "old-bot", true).Return(nil, testAppErr("not found"))
	api.On("GetUser", "old-bot").Return(nil, &model.AppError{StatusCode: http.StatusNotFound, Message: "not found"})
	api.On("GetChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{ChannelId: "c9", UserId: p.botUserID}, nil)
	api.On("GetTeamMember", "t1", p.botUserID).Return(&model.TeamMember{TeamId: "t1", UserId: p.botUserID}, nil)
	api.On("AddChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{}, nil)

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.NotNil(t, ch)
	require.Equal(t, "c9", ch.Id)
	require.False(t, taken)
}

func TestLookupApprovalChannel_NoAdoptionOnTransientCreatorLookupError(t *testing.T) {
	// A 500 from GetUser must not be treated as "creator gone" — that would
	// adopt a channel owned by a still-live untrusted user during an outage.
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("u_other"), nil)
	api.On("GetBot", "u_other", true).Return(nil, testAppErr("not a bot")).Maybe()
	api.On("GetUser", "u_other").Return(nil, testAppErr("api down"))
	api.On("GetChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{ChannelId: "c9", UserId: p.botUserID}, nil)

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestLookupApprovalChannel_BotMembershipAloneDoesNotAdoptLiveUntrustedCreator(t *testing.T) {
	// A random user who pre-created the name must not capture tickets just
	// because the bot was added to their channel.
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("u_other"), nil)
	api.On("GetBot", "u_other", true).Return(nil, testAppErr("not a bot")).Maybe()
	api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Username: "other"}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{ChannelId: "c9", UserId: p.botUserID}, nil)

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestLookupApprovalChannel_AdoptsLegacyPluginOwnedBotWithoutMembership(t *testing.T) {
	// Legacy pre-0.5.0 bot (OwnerId = channel-requests) created the channel.
	// Current bot is not a member → still adopt and join, otherwise tickets
	// fall back to System Admin DMs.
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel("legacy-bot"), nil)
	api.On("GetBot", "legacy-bot", true).Return(&model.Bot{
		UserId: "legacy-bot", OwnerId: legacyRequestHubPluginID, Username: requestHubBotUsername,
	}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member")).Maybe()
	api.On("GetTeamMember", "t1", p.botUserID).Return(&model.TeamMember{TeamId: "t1", UserId: p.botUserID}, nil)
	api.On("AddChannelMember", "c9", p.botUserID).Return(&model.ChannelMember{}, nil)

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.NotNil(t, ch)
	require.Equal(t, "c9", ch.Id)
	require.False(t, taken)
	api.AssertCalled(t, "AddChannelMember", "c9", p.botUserID)
}

func TestLookupApprovalChannel_NoAdoptionWithoutACreator(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetChannelByName", "t1", "approvals", true).Return(otherApprovalChannel(""), nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member"))

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
	api.AssertNotCalled(t, "GetUser", mock.Anything)
}

func TestLookupApprovalChannel_ArchivedPluginChannelIsTaken(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	archived := pluginChannel("c1", "approvals")
	archived.DeleteAt = 1
	api.On("GetChannelByName", "t1", "approvals", true).Return(archived, nil)

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
}

// stubApprovalTeamLookup lets approvalTeam find "appteam" as t1, already recorded.
// Also stubs Team Admin listing so channel-create seed paths (SysAdmin + primary
// Team Admin visibility) can run without each test repeating the mock.
func stubApprovalTeamLookup(t *testing.T, api *plugintest.API) {
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(approvalTeamRecordBytes(t, "appteam", "t1"), nil)
	api.On("GetTeamMembers", "t1", 0, 200).Return([]*model.TeamMember{}, nil).Maybe()
}

func TestEnsureRootApprovalChannel_TakenNameIsNotUsed(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", true).
		Return(&model.Channel{Id: "c9", Type: model.ChannelTypeOpen, CreatorId: "u1"}, nil)
	api.On("GetBot", "u1", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemUserRoleId}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member"))

	require.Nil(t, p.ensureRootApprovalChannel())
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestEnsureRootApprovalChannel_TeamErrorFallsBack(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", DeleteAt: 1}, nil)

	require.Nil(t, p.ensureRootApprovalChannel())
	api.AssertNotCalled(t, "GetChannelByName", mock.Anything, mock.Anything, mock.Anything)
}

func TestEnsureNamedApprovalChannel_TakenNameIsNotUsed(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-bot-requests", true).
		Return(&model.Channel{Id: "c9", Type: model.ChannelTypePrivate, CreatorId: "u1"}, nil)
	api.On("GetBot", "u1", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemUserRoleId}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member"))

	require.Nil(t, p.ensureNamedApprovalChannel("mattermost-bot-requests", "Bots", "purpose"))
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestEnsureNamedApprovalChannel_TeamErrorFallsBack(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(nil, testAppErr("not found"))

	require.Nil(t, p.ensureNamedApprovalChannel("mattermost-bot-requests", "Bots", "purpose"))
}

func TestEnsureTeamApprovalChannel_TakenNameIsAnError(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	name := p.getConfiguration().TeamChannelName()
	api.On("GetChannelByName", "team1", name, true).
		Return(&model.Channel{Id: "c9", Type: model.ChannelTypeOpen, CreatorId: "u1"}, nil)
	api.On("GetBot", "u1", true).Return(nil, testAppErr("not a bot"))
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: model.SystemUserRoleId}, nil)
	api.On("GetChannelMember", "c9", p.botUserID).Return(nil, testAppErr("not a member"))

	ch, err := p.ensureTeamApprovalChannel("team1")

	require.Error(t, err)
	require.Nil(t, ch)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	api.AssertNotCalled(t, "GetTeam", mock.Anything)
}

func TestIsApprovalTeamAdmin_NoApprovalTeam(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	require.False(t, p.isApprovalTeamAdmin("u1"))
	api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
}

func TestIsApprovalTeamAdmin_ApprovalTeamErrorIsNotAdmin(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t2", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(approvalTeamRecordBytes(t, "appteam", "t1"), nil)

	require.False(t, p.isApprovalTeamAdmin("u1"))
	api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
}

func TestIsApprovalTeamAdmin_TeamAdminOfRecordedTeam(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1", SchemeAdmin: true}, nil)

	require.True(t, p.isApprovalTeamAdmin("u1"))
}

func TestIsApprovalTeamAdmin_PlainMemberIsNotAdmin(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1", Roles: "team_user"}, nil)

	require.False(t, p.isApprovalTeamAdmin("u1"))
}
