package main

import (
	"fmt"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// commandCase is one slash subcommand and the dialog it should open.
type commandCase struct {
	sub         string
	route       string
	title       string
	disabledMsg string
	failText    string
}

func commandCases() []commandCase {
	return []commandCase{
		{"channel-admin", routeChannelAdminDialog, "Request Channel Admin Promotion", channelAdminRequestsDisabledMsg, "Channel Admin request form"},
		{"team-admin", routeTeamAdminDialog, "Request Team Admin Promotion", teamAdminRequestsDisabledMsg, "Team Admin request form"},
		{"team", routeTeamDialog, "Request a New Team", teamRequestsDisabledMsg, "team creation request form"},
		{"team-member", routeTeamMemberDialog, "Request Team Members", teamMemberRequestsDisabledMsg, "team member request form"},
		{"team-guest", routeTeamGuestDialog, "Request Guest Invite", teamGuestRequestsDisabledMsg, "guest invite request form"},
		{"bot", routeBotDialog, "Request a Bot Account", botRequestsDisabledMsg, "bot request form"},
		{"webhook-incoming", routeIncomingWebhookDialog, "Request an Incoming Webhook", webhookRequestsDisabledMsg, "incoming webhook request form"},
		{"webhook-outgoing", routeOutgoingWebhookDialog, "Request an Outgoing Webhook", webhookRequestsDisabledMsg, "outgoing webhook request form"},
	}
}

func commandArgs(sub string) *model.CommandArgs {
	return &model.CommandArgs{
		Command:   "/" + commandTrigger + " " + sub,
		TriggerId: "trig",
		TeamId:    "team1",
		UserId:    "u1",
	}
}

func TestExecuteCommand_SubcommandsOpenTheirDialog(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.sub, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Maybe()
			var opened model.OpenDialogRequest
			api.On("OpenInteractiveDialog", mock.Anything).Run(func(args mock.Arguments) {
				opened = args.Get(0).(model.OpenDialogRequest)
			}).Return(nil)

			resp, appErr := p.ExecuteCommand(nil, commandArgs(tc.sub))

			require.Nil(t, appErr)
			require.NotNil(t, resp)
			require.Empty(t, resp.Text)
			require.Equal(t, "trig", opened.TriggerId)
			require.Equal(t, fmt.Sprintf("/plugins/%s%s", manifest.Id, tc.route), opened.URL)
			require.Equal(t, tc.title, opened.Dialog.Title)
		})
	}
}

func TestExecuteCommand_SubcommandsDisabledReply(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.sub, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{})

			resp, appErr := p.ExecuteCommand(nil, commandArgs(tc.sub))

			require.Nil(t, appErr)
			require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
			require.Equal(t, tc.disabledMsg, resp.Text)
			api.AssertNotCalled(t, "OpenInteractiveDialog", mock.Anything)
		})
	}
}

func TestExecuteCommand_SubcommandsDialogFailureReply(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.sub, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)
			api.On("GetTeam", "team1").Return(nil, testAppErr("missing")).Maybe()
			api.On("OpenInteractiveDialog", mock.Anything).Return(testAppErr("expired trigger"))

			resp, appErr := p.ExecuteCommand(nil, commandArgs(tc.sub))

			require.Nil(t, appErr)
			require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
			require.Contains(t, resp.Text, "Could not open the "+tc.failText)
		})
	}
}

func TestExecuteCommand_ChannelDisabledReply(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	resp, appErr := p.ExecuteCommand(nil, commandArgs("channel"))

	require.Nil(t, appErr)
	require.Equal(t, channelRequestsDisabledMsg, resp.Text)
	api.AssertNotCalled(t, "OpenInteractiveDialog", mock.Anything)
}

func TestExecuteCommand_UnknownSubcommandShowsHelp(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	resp, appErr := p.ExecuteCommand(nil, commandArgs("nonsense"))

	require.Nil(t, appErr)
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Equal(t, commandHelpText(), resp.Text)
	require.Contains(t, resp.Text, "/mattermost-request-hub webhook-outgoing")
	require.Empty(t, api.Calls)
}

func TestExecuteCommand_NilArgsReply(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	resp, appErr := p.ExecuteCommand(nil, nil)

	require.Nil(t, appErr)
	require.Contains(t, resp.Text, "missing arguments")
}

func TestExecuteCommand_PanicIsRecovered(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	// OpenInteractiveDialog is left unregistered, so the mock panics.

	resp, appErr := p.ExecuteCommand(nil, commandArgs("bot"))

	require.Nil(t, appErr)
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Contains(t, resp.Text, "Something went wrong")
	api.AssertCalled(t, "LogError", "channel-requests: ExecuteCommand panic recovered",
		"panic", mock.Anything, "stack", mock.Anything)
}
