package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestOnActivate_EnsuresBotAndRegistersCommand(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)
	// No approval team and System Admin two-step approvers: the background
	// channel setup has nothing to create.
	p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})

	api.On("EnsureBotUser", mock.MatchedBy(func(b *model.Bot) bool {
		return b.Username == "mattermost-request-hub"
	})).Return("bot-id", nil)
	api.On("GetBundlePath").Return("", testAppErr("no bundle")).Maybe()
	api.On("RegisterCommand", mock.MatchedBy(func(c *model.Command) bool {
		return c.Trigger == commandTrigger && c.AutoComplete && c.AutocompleteData != nil &&
			c.AutocompleteData.Trigger == commandTrigger
	})).Return(nil)
	api.On("KVGet", mock.Anything).Return(nil, nil).Maybe()
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("GetTeams").Return([]*model.Team{}, nil).Maybe()

	require.NoError(t, p.OnActivate())
	defer func() { require.NoError(t, p.OnDeactivate()) }()
	require.Equal(t, "bot-id", p.botUserID)
}

func TestOnActivate_BotFailureStopsActivation(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)

	api.On("EnsureBotUser", mock.Anything).Return("", testAppErr("no bot"))

	err := p.OnActivate()

	require.ErrorContains(t, err, "failed to ensure bot user")
	require.Empty(t, p.botUserID)
	api.AssertNotCalled(t, "RegisterCommand", mock.Anything)
}
