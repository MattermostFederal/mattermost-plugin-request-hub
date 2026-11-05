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

const (
	testBotArchURL  = "https://docs.example/arch"
	testBotIntegURL = "https://ci.example/hooks/mm"
)

// declared returns in with a valid declaration and a home of home-ch in
// team1.
func declared(in botRequestInput) botRequestInput {
	in.Purpose = "Posts CI results"
	in.ArchitectureURL = testBotArchURL
	in.IntegrationURL = testBotIntegURL
	in.HostingEnv = hostingCICD
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
		Purpose: "Posts CI results", Description: "Posts CI results",
		ArchitectureURL: testBotArchURL, IntegrationURL: testBotIntegURL,
		PrivilegeLevel: privilegeUserEquivalent, MMPrivilege: mmPrivilegeMemberUserOnly,
		DataIngestion: defaultChannelAccessNote, HostingEnv: hostingCICD,
		HomeTeamID: "team1", HomeChannelID: testHomeChannelID, DMChannelID: "dm1", DMRootPostID: "dm-root",
		Compliance: &botCompliance{AcceptedAt: 5, TextSHA256: "abc", Text: "Be good."},
	})
}

func TestCheckBotDeclaration_Required(t *testing.T) {
	_, err := checkBotDeclaration(nil, botDeclaration{})
	require.ErrorContains(t, err, "purpose is required")

	d := botDeclaration{Purpose: "Posts", IntegrationURL: testBotIntegURL, HostingEnv: hostingCICD}
	got, err := checkBotDeclaration(nil, d)
	require.NoError(t, err)
	require.Equal(t, privilegeUserEquivalent, got.PrivilegeLevel)
	require.Equal(t, mmPrivilegeMemberUserOnly, got.MMPrivilege)
	require.Equal(t, defaultChannelAccessNote, got.DataIngestion)
	require.True(t, got.TransitSecurity)

	_, err = checkBotDeclaration(nil, botDeclaration{Purpose: "Posts", HostingEnv: hostingCICD})
	require.ErrorContains(t, err, "integration endpoint is required")

	_, err = checkBotDeclaration(nil, botDeclaration{
		Purpose: "Posts", IntegrationURL: "http://ci.example/hook", HostingEnv: hostingCICD,
	})
	require.ErrorContains(t, err, "https://")

	_, err = checkBotDeclaration(nil, botDeclaration{
		Purpose: "Posts", IntegrationURL: "javascript:alert(1)", HostingEnv: hostingCICD,
	})
	require.ErrorContains(t, err, "script or data")

	_, err = checkBotDeclaration(nil, botDeclaration{
		Purpose: "Posts", ArchitectureURL: "http://docs.example/arch",
		IntegrationURL: testBotIntegURL, HostingEnv: hostingCICD,
	})
	require.ErrorContains(t, err, "https://")

	got, err = checkBotDeclaration(nil, botDeclaration{
		Purpose: "Posts", ArchitectureURL: testBotArchURL,
		IntegrationURL: testBotIntegURL, HostingEnv: hostingOther, HostingOtherDetail: "air-gapped",
	})
	require.NoError(t, err)
	require.Equal(t, "air-gapped", got.HostingOtherDetail)
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
		"**Architecture link:** " + escapeMarkdown(testBotArchURL),
		"**Integration endpoint:** " + escapeMarkdown(testBotIntegURL),
		"**Mattermost role:** Regular user (member); never Team Admin or System Admin",
		"**Script / service runs from:** CI/CD pipeline",
		"**Home:** #builds (Team: Ops)",
	} {
		require.Contains(t, att.Text, line)
	}
}

func TestBotRequestDialog_HasTheDeclarationAndHome(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{})

	d := p.botRequestDialog("#builds (Team: Ops)")

	require.Contains(t, d.IntroductionText, "**Home:** #builds (Team: Ops).")
	byName := map[string]model.DialogElement{}
	for _, el := range d.Elements {
		byName[el.Name] = el
	}
	for _, name := range []string{fieldBotPurpose, fieldBotIntegrationURL, fieldBotHostingEnv} {
		require.Contains(t, byName, name)
		require.False(t, byName[name].Optional, name)
	}
	require.Contains(t, byName, fieldBotArchitectureURL)
	require.True(t, byName[fieldBotArchitectureURL].Optional)
	require.Equal(t, hostingCICD, byName[fieldBotHostingEnv].Options[0].Value)
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

func TestWriteAndLoadBotIndex(t *testing.T) {
	api := &plugintest.API{}
	stubBotHome(api)
	var stored []byte
	api.On("KVSet", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvBotIndexPrefix) }), mock.Anything).
		Run(func(args mock.Arguments) { stored = args.Get(1).([]byte) }).Return(nil)
	api.On("KVGet", kvBotIndexPrefix+"b1").Return(stored, nil).Maybe()
	p := newTestPlugin(api)

	req := homeBotApproval()
	req.OwnerUserID = ""
	p.writeBotIndex(req, &model.Bot{UserId: "b1", Username: "svcbot"}, "approver")

	require.NotEmpty(t, stored)
	var index botIndexEntry
	require.NoError(t, json.Unmarshal(stored, &index))
	require.Equal(t, "b1", index.BotUserID)
	require.Equal(t, req.ID, index.RequestID)
	require.Equal(t, "Posts CI results", index.Purpose)
	require.Equal(t, hostingCICD, index.HostingEnv)
	require.Equal(t, testBotIntegURL, index.IntegrationURL)
	require.Equal(t, req.RequesterID, index.OwnerUserID, "empty owner is stored as the requester")
}

func TestMMPrivilegeLabel_NeverAdmin(t *testing.T) {
	require.Contains(t, mmPrivilegeLabel(mmPrivilegeMemberUserOnly), "never Team Admin or System Admin")
	require.Contains(t, mmPrivilegeLabel(""), "never Team Admin or System Admin")
}
