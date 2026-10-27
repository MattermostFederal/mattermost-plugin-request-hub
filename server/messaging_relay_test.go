package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/require"
)

func TestCanRelayApprovalThreadReply(t *testing.T) {
	t.Run("system admin can relay team tickets", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		user := &model.User{Id: "a", Roles: model.SystemAdminRoleId}
		require.True(t, p.canRelayApprovalThreadReply(user, threadAnchor{Kind: requestTypeTeamMember}))
	})

	t.Run("plain user cannot relay team tickets", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		user := &model.User{Id: "u", Roles: "system_user"}
		require.False(t, p.canRelayApprovalThreadReply(user, threadAnchor{Kind: requestTypeTeamGuest}))
	})

	t.Run("channel approver can relay channel tickets", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowTeamAdminApprovers: false})
		user := &model.User{Id: "a", Roles: model.SystemAdminRoleId}
		require.True(t, p.canRelayApprovalThreadReply(user, threadAnchor{Kind: requestTypeChannel}))
	})

	t.Run("nil user cannot relay", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		require.False(t, p.canRelayApprovalThreadReply(nil, threadAnchor{Kind: requestTypeChannel}))
	})

	t.Run("system admin can relay bot tickets", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		user := &model.User{Id: "a", Roles: model.SystemAdminRoleId}
		require.True(t, p.canRelayApprovalThreadReply(user, threadAnchor{Kind: requestTypeBot}))
	})

	t.Run("system admin can relay webhook tickets", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		user := &model.User{Id: "a", Roles: model.SystemAdminRoleId}
		require.True(t, p.canRelayApprovalThreadReply(user, threadAnchor{Kind: requestTypeWebhook}))
	})

	t.Run("plain user cannot relay bot tickets without attributes", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{})
		user := &model.User{Id: "u", Roles: "system_user"}
		require.False(t, p.canRelayApprovalThreadReply(user, threadAnchor{Kind: requestTypeBot}))
	})
}

func TestSubmitTeamGuestRequest_RequiresMembership(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowTeamGuestRequests: true})

	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "team1", "u_req").Return(nil, testAppErr("not a member"))

	_, err := p.submitTeamGuestRequest("u_req", "team1", "guest@example.com", []string{testChannelID}, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "member of the team")
}
