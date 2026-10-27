package main

import (
	"encoding/json"
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
	p.setConfiguration(&configuration{ApprovalTeam: "appteam"})
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
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		*args.Get(0).(*configuration) = cfg
	}).Return(nil)
	api.On("KVGet", kvPrefixMaxLenV48).Return([]byte("1"), nil)
}

func TestOnConfigurationChange_NewApprovalTeamForgetsRecord(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	defer api.AssertExpectations(t)
	stubLoadConfiguration(api, configuration{ApprovalTeam: "otherteam"})
	api.On("KVDelete", kvApprovalTeamKey).Return(nil).Once()

	require.NoError(t, p.OnConfigurationChange())
	require.Equal(t, "otherteam", p.getConfiguration().ApprovalTeam)
}

func TestOnConfigurationChange_SameApprovalTeamKeepsRecord(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubLoadConfiguration(api, configuration{ApprovalTeam: " appteam "})

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

	ch, taken := p.lookupApprovalChannel("t1", "approvals")

	require.Nil(t, ch)
	require.True(t, taken)
	api.AssertCalled(t, "LogError", mock.Anything, "team_id", "t1", "channel_id", "c9", "channel_name", "approvals")
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
func stubApprovalTeamLookup(t *testing.T, api *plugintest.API) {
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(approvalTeamRecordBytes(t, "appteam", "t1"), nil)
}

func TestEnsureRootApprovalChannel_TakenNameIsNotUsed(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-channel-requests", true).
		Return(&model.Channel{Id: "c9", Type: model.ChannelTypeOpen, CreatorId: "u1"}, nil)

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
