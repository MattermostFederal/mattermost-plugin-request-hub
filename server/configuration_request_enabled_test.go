package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestEnabled(t *testing.T) {
	c := &configuration{
		AllowChannelRequests:         true,
		AllowChannelAdminRequests:    true,
		AllowTeamRequests:            false,
		AllowTeamAdminRequests:       false,
		AllowBotTokenRequests:        true,
		AllowWebhookRequests:         true,
		AllowBugReportRequests:       true,
		AllowBotTransferRequests:     true,
		AllowWebhookTransferRequests: false,
	}

	require.True(t, c.RequestEnabled(requestTypeChannel))
	require.True(t, c.RequestEnabled(requestTypeChannelAdmin))
	require.False(t, c.RequestEnabled(requestTypeTeam))
	require.False(t, c.RequestEnabled(requestTypeTeamAdmin))
	require.True(t, c.RequestEnabled(requestTypeBot))
	require.True(t, c.RequestEnabled(requestTypeWebhook))
	require.True(t, c.RequestEnabled(requestTypeBugReport))
	require.True(t, c.RequestEnabled(requestTypeBotTransfer))
	require.False(t, c.RequestEnabled(requestTypeWebhookTransfer))

	// An unknown type is never enabled — a new entry point can't bypass the
	// gate before its toggle is wired up.
	require.False(t, c.RequestEnabled("bot_token"))
	require.False(t, c.RequestEnabled(""))
}
