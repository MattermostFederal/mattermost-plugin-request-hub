package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestExecuteCommand_NotConfiguredDoesNotOpenDialog(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true}) // enabled, but no prefixes

	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{TriggerId: "trig", TeamId: "team1"})

	require.Nil(t, appErr)
	require.NotNil(t, resp)
	require.Contains(t, resp.Text, "aren't configured")
	// A doomed dialog must not be opened when no prefix is configured.
	api.AssertNotCalled(t, "OpenInteractiveDialog", mock.Anything)
}

func TestExecuteCommand_OpensDialogWhenConfigured(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		AllowChannelRequests: true,
		prefixes:             []channelPrefix{{Prefix: "team-"}},
	})

	api.On("OpenInteractiveDialog", mock.Anything).Return(nil)

	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{TriggerId: "trig", TeamId: "team1"})

	require.Nil(t, appErr)
	require.NotNil(t, resp)
	api.AssertCalled(t, "OpenInteractiveDialog", mock.Anything)
}

func TestExecuteCommand_DialogFailureRepliesPrivately(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		AllowChannelRequests: true,
		prefixes:             []channelPrefix{{Prefix: "team-", Description: "Teams"}},
	})

	api.On("OpenInteractiveDialog", mock.Anything).Return(testAppErr("expired trigger"))

	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{TriggerId: "trig", TeamId: "team1"})

	require.Nil(t, appErr)
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Contains(t, resp.Text, "Could not open")
}

func TestExecuteCommand_DocsOpensGuide(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{
		Command:   "/mattermost-request-hub docs",
		SiteURL:   "https://mm.example.com",
		TriggerId: "trig",
	})

	require.Nil(t, appErr)
	require.NotNil(t, resp)
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	// GotoLocation must stay empty — SPA push of /plugins/... HTML is a Loading hang.
	require.Empty(t, resp.GotoLocation)
	require.Contains(t, resp.Text, "Documentation")
	require.Contains(t, resp.Text, "https://mm.example.com/plugins/"+manifest.Id+"/public/help/docs.html")
	require.Contains(t, resp.Text, "Open the full guide")
}
