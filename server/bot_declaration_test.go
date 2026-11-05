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

const testHomeChannelID = "home-ch"

// declared returns in with a valid declaration and a home of home-ch in
// team1.
func declared(in botRequestInput) botRequestInput {
	in.Purpose = "Posts CI results"
	in.PrivilegeLevel = privilegePost
	in.DataIngestion = "Jenkins webhook payloads"
	in.HostingEnv = "mattermost-dev"
	in.HomeTeamID = "team1"
	in.HomeChannelID = testHomeChannelID
	return in
}

// stubBotHome makes home-ch a public channel of team1 that everyone is in.
func stubBotHome(api *plugintest.API) {
	api.On("GetTeamMember", "team1", mock.Anything).Return(&model.TeamMember{TeamId: "team1"}, nil).Maybe()
	api.On("GetChannel", testHomeChannelID).
		Return(&model.Channel{Id: testHomeChannelID, Name: "builds", TeamId: "team1", Type: model.ChannelTypeOpen}, nil).Maybe()
	api.On("GetChannelMember", testHomeChannelID, mock.Anything).Return(&model.ChannelMember{}, nil).Maybe()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Maybe()
}

// stubBotIndexWrite accepts the bot index entry written on approval.
func stubBotIndexWrite(api *plugintest.API) {
	api.On("KVSet", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvBotIndexPrefix) }), mock.Anything).Return(nil).Maybe()
}

// homeBotApproval is a request with its security step done, a full
// declaration and a home of home-ch in team1.
func homeBotApproval() *botRequest {
	return finalBotApproval(botRequest{
		Purpose: "Posts CI results", PrivilegeLevel: privilegeRead, DataIngestion: "Build logs", HostingEnv: "mattermost-prod",
		HomeTeamID: "team1", HomeChannelID: testHomeChannelID, DMChannelID: "dm1", DMRootPostID: "dm-root",
		Compliance: &botCompliance{AcceptedAt: 5, TextSHA256: "abc", Text: "Be good."},
	})
}

func TestHostingEnvs(t *testing.T) {
	require.Equal(t, []string{"mattermost-dev", "mattermost-prod"}, (&configuration{}).HostingEnvs())
	require.Equal(t, []string{"mattermost-dev", "mattermost-prod"}, (&configuration{BotHostingEnvironments: " \n "}).HostingEnvs())
	require.Equal(t, []string{"lab", "gov cloud"}, (&configuration{BotHostingEnvironments: "lab\r\n\n gov cloud \nlab"}).HostingEnvs())
}

func TestCheckBotDeclaration_TrimsAndLowercases(t *testing.T) {
	got, err := checkBotDeclaration(&configuration{}, botDeclaration{
		Purpose: "  Posts  ", PrivilegeLevel: " ELEVATED ", DataIngestion: " logs ", HostingEnv: " mattermost-prod ",
	})
	require.NoError(t, err)
	require.Equal(t, botDeclaration{Purpose: "Posts", PrivilegeLevel: privilegeElevated, DataIngestion: "logs", HostingEnv: "mattermost-prod"}, got)
}

func TestBotApprovalAttachment_ShowsTheDeclarationAndHome(t *testing.T) {
	api := &plugintest.API{}
	stubBotHome(api)
	p := newTestPlugin(api)

	req := homeBotApproval()
	req.twoStepState = twoStepState{}
	att := p.botApprovalAttachment(req, &model.User{Username: "reqer"})

	for _, line := range []string{
		"**Purpose:** Posts CI results",
		"**Privilege:** Read only",
		"**Data ingestion:** Build logs",
		"**Environment:** mattermost\\-prod",
		"**Home:** #builds (Team: Ops)",
	} {
		require.Contains(t, att.Text, line)
	}
}

func TestBotRequestDialog_HasTheDeclarationAndHome(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{BotHostingEnvironments: "lab\nprod"})

	d := p.botRequestDialog("#builds (Team: Ops)")

	require.Contains(t, d.IntroductionText, "**Home:** #builds (Team: Ops).")
	byName := map[string]model.DialogElement{}
	for _, el := range d.Elements {
		byName[el.Name] = el
	}
	for _, name := range []string{fieldBotPurpose, fieldBotPrivilege, fieldBotIngestion, fieldBotHostingEnv} {
		require.Contains(t, byName, name)
		require.False(t, byName[name].Optional, name)
	}
	require.Len(t, byName[fieldBotPrivilege].Options, 3)
	require.Equal(t, "lab", byName[fieldBotHostingEnv].Options[0].Value)
	require.Equal(t, "prod", byName[fieldBotHostingEnv].Options[1].Value)
}

func TestBotRequestCommand_RefusesOutsideATeamChannel(t *testing.T) {
	for name, channel := range map[string]*model.Channel{
		"direct message": {Id: testHomeChannelID, Type: model.ChannelTypeDirect},
		"group message":  {Id: testHomeChannelID, Type: model.ChannelTypeGroup},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			api.On("GetChannel", testHomeChannelID).Return(channel, nil)

			resp, appErr := p.ExecuteCommand(nil, commandArgs("bot"))

			require.Nil(t, appErr)
			require.Equal(t, botHomeMissingMsg, resp.Text)
			api.AssertNotCalled(t, "OpenInteractiveDialog", mock.Anything)
		})
	}
}

func TestBotHomeBlocker(t *testing.T) {
	member := &model.TeamMember{TeamId: "team1"}
	home := &model.Channel{Id: testHomeChannelID, Name: "builds", TeamId: "team1", Type: model.ChannelTypeOpen}
	for name, tc := range map[string]struct {
		setup func(api *plugintest.API)
		want  string
	}{
		"still valid": {setup: stubBotHome},
		"team archived": {setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DeleteAt: 1}, nil)
		}, want: "The team has been archived. Deny this request instead."},
		"requester left the team": {setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1"}, nil)
			api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{DeleteAt: 1}, nil)
		}, want: "The requester is no longer a member of the team."},
		"channel archived": {setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1"}, nil)
			api.On("GetTeamMember", "team1", "u_req").Return(member, nil)
			api.On("GetChannel", testHomeChannelID).Return(&model.Channel{Id: testHomeChannelID, Name: "builds", TeamId: "team1", DeleteAt: 1}, nil)
		}, want: "~builds has been archived."},
		"channel moved to another team": {setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1"}, nil)
			api.On("GetTeamMember", "team1", "u_req").Return(member, nil)
			api.On("GetChannel", testHomeChannelID).Return(&model.Channel{Id: testHomeChannelID, Name: "builds", TeamId: "team2"}, nil)
		}, want: "~builds is no longer in the bot's home team."},
		"requester left the channel": {setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1"}, nil)
			api.On("GetTeamMember", "team1", "u_req").Return(member, nil)
			api.On("GetChannel", testHomeChannelID).Return(home, nil)
			api.On("GetChannelMember", testHomeChannelID, "u_req").Return(nil, &model.AppError{StatusCode: 404})
		}, want: "The requester is no longer a member of the channel."},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			p := newTestPlugin(api)
			tc.setup(api)

			got := p.botHomeBlocker(homeBotApproval(), regularUser())

			if tc.want == "" {
				require.Empty(t, got)
				return
			}
			require.Contains(t, got, tc.want)
		})
	}

	// Requests from before homes existed skip the check.
	p := newTestPlugin(&plugintest.API{})
	require.Empty(t, p.botHomeBlocker(finalBotApproval(botRequest{}), regularUser()))
}

func TestHandleBotAction_BotJoinsItsHomeAndIsIndexed(t *testing.T) {
	api := &plugintest.API{}
	stubBotExtras(api)
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	events := captureAudit(p)
	api.On("GetConfig").Return(&model.Config{})
	req := homeBotApproval()
	finalBot(t, api, req)
	stubBotHome(api)
	api.On("CreateSession", mock.Anything).Return(nil, testAppErr("no session"))
	api.On("AddChannelMember", testHomeChannelID, "new-bot-id").Return(&model.ChannelMember{}, nil).Once()
	extrasReply(api)

	resp := runAsAdmin(t, p, botActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Bot **@svcbot** created.")
	api.AssertCalled(t, "AddChannelMember", testHomeChannelID, "new-bot-id")

	var index botIndexEntry
	for _, call := range api.Calls {
		if call.Method == "KVSet" && call.Arguments.String(0) == kvBotIndexPrefix+"new-bot-id" {
			require.NoError(t, json.Unmarshal(call.Arguments.Get(1).([]byte), &index))
		}
	}

	require.Equal(t, "new-bot-id", index.BotUserID)
	require.Equal(t, "r1", index.RequestID)
	require.Equal(t, "u_req", index.RequesterID)
	require.Equal(t, "admin1", index.ApprovedBy)
	require.Equal(t, "Posts CI results", index.Purpose)
	require.Equal(t, privilegeRead, index.PrivilegeLevel)
	require.Equal(t, "Build logs", index.DataIngestion)
	require.Equal(t, "mattermost-prod", index.HostingEnv)
	require.Equal(t, testHomeChannelID, index.HomeChannelID)
	require.Equal(t, "abc", index.Compliance.TextSHA256)

	var approved *auditEntry
	for i := range *events {
		if (*events)[i].Event == eventApproved {
			approved = &(*events)[i]
		}
	}
	require.NotNil(t, approved)
	require.Equal(t, "team1", approved.Detail["home_team_id"])
	require.Equal(t, testHomeChannelID, approved.Detail["home_channel_id"])
	require.Equal(t, true, approved.Detail["home_channel_joined"])
}

func TestHandleBotAction_HomeJoinFailureIsPartial(t *testing.T) {
	api := &plugintest.API{}
	stubBotExtras(api)
	stubPendingLists(api)
	stubLogs(api)
	p := newTestPlugin(api)
	events := captureAudit(p)
	api.On("GetConfig").Return(&model.Config{})
	finalBot(t, api, homeBotApproval())
	stubBotHome(api)
	stubBotIndexWrite(api)
	api.On("CreateSession", mock.Anything).Return(nil, testAppErr("no session"))
	api.On("AddChannelMember", testHomeChannelID, "new-bot-id").Return(nil, testAppErr("channel is read-only"))
	replies := extrasReply(api)

	resp := runAsAdmin(t, p, botActionHandler, true)

	require.NotNil(t, resp.Update)
	require.Contains(t, strings.Join(*replies, "\n"), "adding the bot to its home channel failed")
	require.Equal(t, 1, countEvents(*events, eventApprovedPartial))
}

func TestWriteBotIndex_SkipsExerciseBots(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	p.writeBotIndex(&botRequest{ID: "r1", Exercise: true}, &model.Bot{UserId: "b1"}, "admin1")

	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestListBotIndex(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("KVList", 0, botIndexPageSize).Return([]string{"history_r1", kvBotIndexPrefix + "b1", kvBotIndexPrefix + "bad", kvBotIndexPrefix + "gone"}, nil)
	api.On("KVGet", kvBotIndexPrefix+"b1").Return([]byte(`{"bot_user_id":"b1","request_id":"r1","hosting_env":"lab"}`), nil)
	api.On("KVGet", kvBotIndexPrefix+"bad").Return([]byte(`{`), nil)
	api.On("KVGet", kvBotIndexPrefix+"gone").Return(nil, nil)

	entries, err := p.listBotIndex()

	require.NoError(t, err)
	require.Equal(t, []botIndexEntry{{BotUserID: "b1", RequestID: "r1", HostingEnv: "lab"}}, entries)
}
