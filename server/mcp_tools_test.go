package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const mcpToolPrefix = "com_mattermostfederal_mattermost-plugin-request-hub__"

// callMCPTool calls a tool over a real MCP session acting for userID.
func callMCPTool(t *testing.T, p *Plugin, userID, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := mcpSession(t, p, userID).CallTool(context.Background(), &mcp.CallToolParams{
		Name:      mcpToolPrefix + tool,
		Arguments: args,
	})
	require.NoError(t, err)
	return result
}

// mcpToolError returns the error text of a failed tool call.
func mcpToolError(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.True(t, result.IsError, "the tool call should have failed")
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return text.Text
}

// mcpToolOutput decodes a successful tool call's structured output.
func mcpToolOutput[Out any](t *testing.T, result *mcp.CallToolResult) Out {
	t.Helper()
	require.False(t, result.IsError, "the tool call failed: %v", result.Content)
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var out Out
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestListPrefixesTool_RefusesWithoutUserID(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})

	_, _, err := p.listPrefixesTool(context.Background(), nil, ListPrefixesArgs{})

	require.ErrorIs(t, err, errNoMCPUser)
}

func TestListPrefixesTool_NotConfigured(t *testing.T) {
	stubMCP(t)
	p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})

	result := callMCPTool(t, p, "u1", "list_prefixes", nil)

	require.Equal(t, notConfiguredMessage, mcpToolError(t, result))
}

func TestListPrefixesTool_ListsPrefixes(t *testing.T) {
	stubMCP(t)
	p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{prefixes: []channelPrefix{
		{Prefix: "team-", Description: "Team channels", SuffixPatternRaw: "16"},
		{Prefix: "proj-"},
	}})

	out := mcpToolOutput[ListPrefixesOutput](t, callMCPTool(t, p, "u1", "list_prefixes", nil))

	require.Equal(t, []MCPPrefix{
		{Prefix: "team-", Description: "Team channels", SuffixRule: "16"},
		{Prefix: "proj-"},
	}, out.Prefixes)
}

func TestRequestChannelTool_RefusesWithoutUserID(t *testing.T) {
	stubMCP(t)
	api := &plugintest.API{}
	p := newMCPTestPlugin(t, api, &configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})

	result := callMCPTool(t, p, "", "request_channel", map[string]any{
		"team_id": "team1", "prefix": "team-", "display_name": "Marketing", "members": []string{"alice"},
	})

	require.Equal(t, errNoMCPUser.Error(), mcpToolError(t, result))
	api.AssertNotCalled(t, "GetUserByUsername", mock.Anything)
	api.AssertNotCalled(t, "GetUser", mock.Anything)
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestRequestChannelTool_UsesContextUserNotArgs(t *testing.T) {
	stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newMCPTestPlugin(t, api, &configuration{prefixes: []channelPrefix{{Prefix: "team-"}}, ApprovalTeam: "appteam"})

	// A System Admin's request queues for approval like anyone else's.
	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	var stored channelRequest
	api.On("KVSet", pendingKey, mock.Anything).Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &stored))
	}).Return(nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	mockApprovalChannel(api)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "appch" })).Return(&model.Post{Id: "card1"}, nil)
	api.On("GetDirectChannel", "admin1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dmroot"}, nil)

	result := callMCPTool(t, p, "admin1", "request_channel", map[string]any{
		"team_id": "team1", "prefix": "team-", "display_name": "Marketing",
	})

	out := mcpToolOutput[MCPMessageOutput](t, result)
	require.Contains(t, out.Message, "submitted for approval")
	require.Equal(t, "admin1", stored.RequesterID)
	require.Equal(t, "team-marketing", stored.Name)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestRequestChannelTool_NonMemberIsRefused(t *testing.T) {
	stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newMCPTestPlugin(t, api, &configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})

	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "team1", "u1").Return(nil, testAppErr("not a member"))

	result := callMCPTool(t, p, "u1", "request_channel", map[string]any{
		"team_id": "team1", "prefix": "team-", "display_name": "Marketing",
	})

	require.Equal(t, "you must be a member of the team to request a channel in it", mcpToolError(t, result))
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestRequestChannelTool_UnknownUsersAreRefused(t *testing.T) {
	for _, field := range []string{"members", "admin_members"} {
		t.Run(field, func(t *testing.T) {
			stubMCP(t)
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newMCPTestPlugin(t, api, &configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})

			api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))

			result := callMCPTool(t, p, "u1", "request_channel", map[string]any{
				"team_id": "team1", "prefix": "team-", "display_name": "Marketing", field: []string{"@ghost"},
			})

			require.Equal(t, "unknown user: ghost", mcpToolError(t, result))
			api.AssertNotCalled(t, "GetUser", mock.Anything)
		})
	}
}

func TestRequestChannelAdminTool_RefusesWithoutUserID(t *testing.T) {
	stubMCP(t)
	api := &plugintest.API{}
	p := newMCPTestPlugin(t, api, &configuration{})

	result := callMCPTool(t, p, "", "request_channel_admin", map[string]any{
		"channel_id": "ch1", "nominees": []string{"nommy"},
	})

	require.Equal(t, errNoMCPUser.Error(), mcpToolError(t, result))
	api.AssertNotCalled(t, "GetUserByUsername", mock.Anything)
	api.AssertNotCalled(t, "GetUser", mock.Anything)
}

func TestRequestChannelAdminTool_QueuesForSysadmin(t *testing.T) {
	stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newMCPTestPlugin(t, api, &configuration{ApprovalTeam: "appteam"})

	api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "marketing", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetChannelMember", "ch1", testNomineeID).Return(&model.ChannelMember{}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	mockApprovalChannel(api)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team1", mock.Anything, mock.Anything).Return([]*model.TeamMember{}, nil).Maybe()
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "appch" })).Return(&model.Post{Id: "card1"}, nil)
	api.On("GetDirectChannel", "admin1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dmroot"}, nil)

	result := callMCPTool(t, p, "admin1", "request_channel_admin", map[string]any{
		"channel_id": "ch1", "nominees": []string{"@nommy"},
	})

	out := mcpToolOutput[MCPMessageOutput](t, result)
	require.Contains(t, out.Message, "submitted for approval")
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestRequestChannelAdminTool_NonMemberIsRefused(t *testing.T) {
	stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newMCPTestPlugin(t, api, &configuration{})

	api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "ch1", "u1").Return(nil, testAppErr("not a member"))

	result := callMCPTool(t, p, "u1", "request_channel_admin", map[string]any{
		"channel_id": "ch1", "nominees": []string{"nommy"},
	})

	require.Equal(t, "channel not found, or you are not a member of it", mcpToolError(t, result))
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestRequestChannelAdminTool_UnknownNomineeIsRefused(t *testing.T) {
	stubMCP(t)
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newMCPTestPlugin(t, api, &configuration{})

	api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))

	result := callMCPTool(t, p, "u1", "request_channel_admin", map[string]any{
		"channel_id": "ch1", "nominees": []string{"ghost"},
	})

	require.Equal(t, "unknown user: ghost", mcpToolError(t, result))
	api.AssertNotCalled(t, "GetUser", mock.Anything)
}

func TestMCPTools_HonorRequestTypeSettings(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
		off  func(c *configuration)
		want string
	}{
		{"list_prefixes", nil, func(c *configuration) { c.AllowChannelRequests = false }, channelRequestsDisabledMsg},
		{"request_channel", map[string]any{
			"team_id": "team1", "prefix": "team-", "display_name": "Marketing", "members": []string{"alice"},
		}, func(c *configuration) { c.AllowChannelRequests = false }, channelRequestsDisabledMsg},
		{"request_channel_admin", map[string]any{
			"channel_id": "ch1", "nominees": []string{"nommy"},
		}, func(c *configuration) { c.AllowChannelAdminRequests = false }, channelAdminRequestsDisabledMsg},
		{"request_team", map[string]any{
			"display_name": "Field Ops",
		}, func(c *configuration) { c.AllowTeamRequests = false }, teamRequestsDisabledMsg},
		{"request_team_admin", map[string]any{
			"team_id": "team1", "nominees": []string{"nommy"},
		}, func(c *configuration) { c.AllowTeamAdminRequests = false }, teamAdminRequestsDisabledMsg},
		{"request_team_member", map[string]any{
			"team_id": "team1", "nominees": []string{"nommy"},
		}, func(c *configuration) { c.AllowTeamMemberRequests = false }, teamMemberRequestsDisabledMsg},
		{"request_team_guest", map[string]any{
			"team_id": "team1", "email": "guest@example.com", "channel_ids": []string{testChannelID},
		}, func(c *configuration) { c.AllowTeamGuestRequests = false }, teamGuestRequestsDisabledMsg},
		{"request_bot", map[string]any{
			"username": "svcbot", "display_name": "Service Bot", "owner": "owner",
		}, func(c *configuration) { c.AllowBotTokenRequests = false }, botRequestsDisabledMsg},
		{"request_webhook", map[string]any{
			"webhook_type": "incoming", "channel_id": "ch1", "display_name": "Alerts",
		}, func(c *configuration) { c.AllowWebhookRequests = false }, webhookRequestsDisabledMsg},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			stubMCP(t)
			api := &plugintest.API{}
			p := newMCPTestPlugin(t, api, &configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})
			config := *p.getConfiguration()
			tc.off(&config)
			p.setConfiguration(&config)

			result := callMCPTool(t, p, "u1", tc.tool, tc.args)

			require.Equal(t, tc.want, mcpToolError(t, result))
			api.AssertNotCalled(t, "GetUserByUsername", mock.Anything)
			api.AssertNotCalled(t, "GetUser", mock.Anything)
		})
	}
}

func TestRequestChannelTool_QueuesForRegularMember(t *testing.T) {
	stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newMCPTestPlugin(t, api, approvalConfig())

	mockMemberRequester(api)
	var stored channelRequest
	api.On("KVSet", pendingKey, mock.Anything).Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &stored))
	}).Return(nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	mockApprovalChannel(api)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "appch" })).Return(&model.Post{Id: "card1"}, nil)
	api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm1" })).Return(&model.Post{Id: "dmroot"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()

	out := mcpToolOutput[MCPMessageOutput](t, callMCPTool(t, p, "u1", "request_channel", map[string]any{
		"team_id": "team1", "prefix": "team-", "display_name": "Marketing",
	}))

	require.Contains(t, out.Message, "submitted for approval")
	require.Equal(t, "u1", stored.RequesterID)
	require.Equal(t, "team-marketing", stored.Name)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

// mcpRequestCases are the tools added beside the channel ones, each with
// arguments that queue a request from team1 and the fields it stores.
type mcpRequestCase struct {
	tool     string
	args     map[string]any
	stub     func(api *plugintest.API)
	kvPrefix string
	want     map[string]any
}

func mcpRequestCases() map[string]mcpRequestCase {
	return map[string]mcpRequestCase{
		"request_team": {
			tool: "request_team",
			args: map[string]any{
				"display_name": "Field Ops", "name": "field-ops", "description": "Weekly sync",
				"invite_only": true, "request_team_admin": true,
			},
			kvPrefix: kvTeamRequestPrefix,
			want: map[string]any{
				"display_name": "Field Ops", "name": "field-ops", "description": "Weekly sync",
				"type": model.TeamInvite, "request_team_admin": true,
			},
		},
		"request_team open by default": {
			tool:     "request_team",
			args:     map[string]any{"display_name": "Field Ops"},
			kvPrefix: kvTeamRequestPrefix,
			want:     map[string]any{"name": "field-ops", "type": model.TeamOpen, "request_team_admin": false},
		},
		"request_team_admin": {
			tool: "request_team_admin",
			args: map[string]any{"team_id": "team1", "nominees": []string{"@nommy"}},
			stub: func(api *plugintest.API) {
				stubTeamAdminQueue(testNomineeID)(api)
				api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID}, nil)
			},
			kvPrefix: kvTeamAdminRequestPrefix,
			want:     map[string]any{"team_id": "team1", "nominee_ids": []any{testNomineeID}},
		},
		"request_team_member": {
			tool: "request_team_member",
			args: map[string]any{"team_id": "team1", "nominees": []string{"nommy"}, "reason": "Ops cell"},
			stub: func(api *plugintest.API) {
				stubTeamMemberQueue(api)
				api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID}, nil)
			},
			kvPrefix: kvTeamMemberRequestPrefix,
			want:     map[string]any{"team_id": "team1", "nominee_ids": []any{testNomineeID}, "reason": "Ops cell"},
		},
		"request_team_guest": {
			tool: "request_team_guest",
			args: map[string]any{
				"team_id": "team1", "email": "Guest@Example.com", "channel_ids": []string{testChannelID}, "reason": "Vendor access",
			},
			stub:     stubTeamGuestQueue,
			kvPrefix: kvTeamGuestRequestPrefix,
			want: map[string]any{
				"team_id": "team1", "email": "guest@example.com", "channel_ids": []any{testChannelID}, "reason": "Vendor access",
			},
		},
		"request_bot": {
			tool: "request_bot",
			args: map[string]any{
				"username": "svcbot", "display_name": "Service Bot", "description": "Posts build results", "owner": "@owner",
			},
			stub: func(api *plugintest.API) {
				api.On("GetUserByUsername", "owner").Return(&model.User{Id: testOwnerID}, nil)
				api.On("GetUser", testOwnerID).Return(&model.User{Id: testOwnerID, Username: "owner"}, nil)
			},
			kvPrefix: kvBotRequestPrefix,
			want: map[string]any{
				"username": "svcbot", "display_name": "Service Bot", "description": "Posts build results", "owner_user_id": testOwnerID,
			},
		},
		"request_bot no owner": {
			tool:     "request_bot",
			args:     map[string]any{"username": "svcbot", "display_name": "Service Bot"},
			kvPrefix: kvBotRequestPrefix,
			want:     map[string]any{"username": "svcbot", "owner_user_id": nil},
		},
		"request_webhook incoming": {
			tool: "request_webhook",
			args: map[string]any{
				"webhook_type": "Incoming", "channel_id": "ch1", "display_name": "Build alerts", "description": "CI results",
			},
			stub:     stubWebhookQueue,
			kvPrefix: kvWebhookRequestPrefix,
			want: map[string]any{
				"webhook_type": webhookTypeIncoming, "channel_id": "ch1", "display_name": "Build alerts", "description": "CI results",
			},
		},
		"request_webhook outgoing": {
			tool: "request_webhook",
			args: map[string]any{
				"webhook_type": " Outgoing ", "channel_id": "ch1", "display_name": "Relay", "callback_url": "https://hooks.example.com/out",
			},
			stub:     stubWebhookQueue,
			kvPrefix: kvWebhookRequestPrefix,
			want: map[string]any{
				"webhook_type": webhookTypeOutgoing, "channel_id": "ch1", "display_name": "Relay", "callback_url": "https://hooks.example.com/out",
			},
		},
	}
}

func TestMCPRequestTools_QueueForSystemAdmin(t *testing.T) {
	for name, tc := range mcpRequestCases() {
		t.Run(name, func(t *testing.T) {
			stubMCP(t)
			api := &plugintest.API{}
			stubLogs(api)
			p := newMCPTestPlugin(t, api, &configuration{})
			stubSystemAdminQueue(api)
			if tc.stub != nil {
				tc.stub(api)
			}

			out := mcpToolOutput[MCPMessageOutput](t, callMCPTool(t, p, testSysAdminID, tc.tool, tc.args))

			require.Contains(t, out.Message, "approval")
			requireQueued(t, api, tc.kvPrefix)
			stored := storedFields(t, api, tc.kvPrefix)
			require.Equal(t, testSysAdminID, stored["requester_id"])
			for field, value := range tc.want {
				require.Equal(t, value, stored[field], field)
			}
			if tc.want["owner_user_id"] == testOwnerID {
				api.AssertCalled(t, "GetUser", testOwnerID)
			}
		})
	}
}

func TestMCPRequestTools_QueueForRegularUser(t *testing.T) {
	requester := regularUser()
	for name, tc := range mcpRequestCases() {
		t.Run(name, func(t *testing.T) {
			stubMCP(t)
			api := &plugintest.API{}
			stubLogs(api)
			p := newMCPTestPlugin(t, api, &configuration{})
			api.On("GetUser", requester.Id).Return(requester, nil)
			api.On("GetTeamMember", "team1", requester.Id).Return(&model.TeamMember{TeamId: "team1", UserId: requester.Id}, nil).Maybe()
			for _, channelID := range []string{"ch1", testChannelID} {
				api.On("GetChannelMember", channelID, requester.Id).Return(&model.ChannelMember{UserId: requester.Id}, nil).Maybe()
			}
			stubSystemAdminQueue(api)
			if tc.stub != nil {
				tc.stub(api)
			}

			out := mcpToolOutput[MCPMessageOutput](t, callMCPTool(t, p, requester.Id, tc.tool, tc.args))

			require.Contains(t, out.Message, "approval")
			requireQueued(t, api, tc.kvPrefix)
			require.Equal(t, requester.Id, storedFields(t, api, tc.kvPrefix)["requester_id"])
			// A regular requester's membership is checked where the form checks it.
			switch tc.tool {
			case "request_team_admin":
				api.AssertCalled(t, "GetTeamMember", "team1", requester.Id)
			case "request_team_guest":
				api.AssertCalled(t, "GetTeamMember", "team1", requester.Id)
				api.AssertCalled(t, "GetChannelMember", testChannelID, requester.Id)
			case "request_webhook":
				api.AssertCalled(t, "GetChannelMember", "ch1", requester.Id)
			}
		})
	}
}

func TestMCPRequestTools_RefuseWithoutUserID(t *testing.T) {
	for name, tc := range mcpRequestCases() {
		t.Run(name, func(t *testing.T) {
			stubMCP(t)
			api := &plugintest.API{}
			p := newMCPTestPlugin(t, api, &configuration{})

			result := callMCPTool(t, p, "", tc.tool, tc.args)

			require.Equal(t, errNoMCPUser.Error(), mcpToolError(t, result))
			require.Empty(t, api.Calls)
		})
	}
}

func TestMCPRequestTools_ReturnValidationErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"unknown team admin nominee": {"request_team_admin", map[string]any{"team_id": "team1", "nominees": []string{"ghost"}}, "unknown user: ghost"},
		"unknown team member":        {"request_team_member", map[string]any{"team_id": "team1", "nominees": []string{"ghost"}}, "unknown user: ghost"},
		"unknown bot owner":          {"request_bot", map[string]any{"username": "svcbot", "display_name": "Svc", "owner": "ghost"}, "unknown user: ghost"},
		"no team admin nominees":     {"request_team_admin", map[string]any{"team_id": "team1", "nominees": []string{}}, "pick at least one person to make a Team Admin"},
		"reserved team name":         {"request_team", map[string]any{"display_name": "Ops", "name": "signup-team"}, "reserved by Mattermost"},
		"bad guest email":            {"request_team_guest", map[string]any{"team_id": "team1", "email": "not-an-email", "channel_ids": []string{testChannelID}}, "valid guest email"},
		"bad bot username":           {"request_bot", map[string]any{"username": "ab", "display_name": "Svc"}, "3-22 characters"},
		"unknown webhook type":       {"request_webhook", map[string]any{"webhook_type": "slash", "channel_id": "ch1", "display_name": "A"}, `unknown webhook type "slash"`},
		"outgoing without callback":  {"request_webhook", map[string]any{"webhook_type": "outgoing", "channel_id": "ch1", "display_name": "A"}, "callback URL is required"},
		"incoming with callback": {"request_webhook", map[string]any{
			"webhook_type": "incoming", "channel_id": "ch1", "display_name": "A", "callback_url": "https://hooks.example.com",
		}, "a callback URL is only for outgoing webhooks"},
		"blank bot owner": {"request_bot", map[string]any{"username": "svcbot", "display_name": "Svc", "owner": " @ "}, "name the owner by username"},
	} {
		t.Run(name, func(t *testing.T) {
			stubMCP(t)
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newMCPTestPlugin(t, api, &configuration{})
			api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found")).Maybe()

			result := callMCPTool(t, p, "u1", tc.tool, tc.args)

			require.Contains(t, mcpToolError(t, result), tc.want)
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}

func TestMCPRequestTools_NonMemberIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		tool     string
		args     map[string]any
		want     string
		teamMate bool
	}{
		"team admin": {
			tool: "request_team_admin",
			args: map[string]any{"team_id": "team1", "nominees": []string{"nommy"}},
			want: "you must be a member of the team to request Team Admin access",
		},
		"team guest": {
			tool: "request_team_guest",
			args: map[string]any{"team_id": "team1", "email": "guest@example.com", "channel_ids": []string{testChannelID}},
			want: "you must be a member of the team to request a guest invite",
		},
		"guest channel": {
			tool:     "request_team_guest",
			args:     map[string]any{"team_id": "team1", "email": "guest@example.com", "channel_ids": []string{testChannelID}},
			want:     "you can only invite guests to channels you're a member of",
			teamMate: true,
		},
		"webhook": {
			tool: "request_webhook",
			args: map[string]any{"webhook_type": "incoming", "channel_id": "ch1", "display_name": "Alerts"},
			want: "you must be a member of the channel to request a webhook for it",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stubMCP(t)
			api := &plugintest.API{}
			stubLogs(api)
			p := newMCPTestPlugin(t, api, &configuration{})
			requester := regularUser()
			api.On("GetUser", requester.Id).Return(requester, nil)
			api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID}, nil).Maybe()
			if tc.teamMate {
				api.On("GetTeamMember", "team1", requester.Id).Return(&model.TeamMember{TeamId: "team1", UserId: requester.Id}, nil)
			} else {
				api.On("GetTeamMember", "team1", requester.Id).Return(nil, testAppErr("not a member")).Maybe()
			}
			for _, channelID := range []string{"ch1", testChannelID} {
				api.On("GetChannelMember", channelID, requester.Id).Return(nil, testAppErr("not a member")).Maybe()
			}
			stubTeamGuestQueue(api)
			stubWebhookQueue(api)

			result := callMCPTool(t, p, requester.Id, tc.tool, tc.args)

			require.Equal(t, tc.want, mcpToolError(t, result))
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}
