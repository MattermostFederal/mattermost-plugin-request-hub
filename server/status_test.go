package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// statusFixture stores, for u1, a channel request created at
// channelCreated and a Channel Admin request created at adminCreated, plus
// a pending entry whose request is gone.
type statusFixture struct {
	api       *plugintest.API
	p         *Plugin
	store     map[string][]byte
	channelID string
	adminID   string
}

func newStatusFixture(t *testing.T, expiryDays int, channelCreated, adminCreated int64) statusFixture {
	t.Helper()
	api := &plugintest.API{}
	stubLogs(api)
	t.Cleanup(func() { api.AssertExpectations(t) })
	p := newTestPlugin(api)
	config := listConfig()
	config.expiryDays = expiryDays
	p.setConfiguration(config)
	store := fakeKV(api)

	f := statusFixture{api: api, p: p, store: store, channelID: model.NewId(), adminID: model.NewId()}
	store[kvRequestPrefix+f.channelID] = recordJSON(t, &channelRequest{ID: f.channelID, RequesterID: "u1", TeamID: "team1", Name: "team-x", DisplayName: "X *bold*", CreatedAt: channelCreated})
	store[kvAdminRequestPrefix+f.adminID] = recordJSON(t, &adminRequest{ID: f.adminID, RequesterID: "u1", ChannelID: "ch1", NomineeIDs: []string{"u_nom"}, CreatedAt: adminCreated})
	store[pendingListKey(pendingKindChannel, "u1")] = recordJSON(t, []pendingEntry{{ID: f.channelID, CreatedAt: channelCreated}, {ID: model.NewId(), CreatedAt: channelCreated}})
	store[pendingListKey(pendingKindAdmin, "u1")] = recordJSON(t, []pendingEntry{{ID: f.adminID, CreatedAt: adminCreated}})

	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Eng", Name: "eng"}, nil).Maybe()
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1"}, nil).Maybe()
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil).Maybe()
	return f
}

// listConfig turns on both request types the list covers.
func listConfig() *configuration {
	config := approvableConfig()
	config.AllowChannelRequests = true
	config.AllowChannelAdminRequests = true
	return config
}

const (
	oct3 = int64(1790985600000) // 2026-10-03T00:00:00Z
	oct5 = oct3 + 2*millisPerDay
)

func TestListOwnRequests(t *testing.T) {
	t.Run("both kinds, oldest first, gone entries skipped", func(t *testing.T) {
		f := newStatusFixture(t, 7, oct5, oct3)

		got, _, err := f.p.listOwnRequests("u1", maxListedRequests)

		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, f.adminID, got[0].ID)
		require.Equal(t, pendingKindAdmin, got[0].Kind)
		require.Equal(t, "Channel Admin for @nom in ~town", got[0].Summary)
		require.Equal(t, f.channelID, got[1].ID)
		require.Equal(t, `Channel **X \*bold\*** (`+"`team-x`"+`) in Eng`, got[1].Summary)
		require.Equal(t, oct5+7*millisPerDay, got[1].ExpiresAt)
		// Both are past 7 days.
		require.True(t, got[0].Expired)
		require.True(t, got[1].Expired)
	})

	t.Run("no expiry", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)

		got, _, err := f.p.listOwnRequests("u1", maxListedRequests)

		require.NoError(t, err)
		require.Len(t, got, 2)
		for _, req := range got {
			require.False(t, req.Expired)
			require.Zero(t, req.ExpiresAt)
		}
	})

	t.Run("pending with an expiry date", func(t *testing.T) {
		yesterday := model.GetMillis() - millisPerDay
		f := newStatusFixture(t, 7, yesterday, 0)

		got, _, err := f.p.listOwnRequests("u1", maxListedRequests)

		require.NoError(t, err)
		require.Len(t, got, 2)
		// A request saved without a time never expires.
		require.Equal(t, f.adminID, got[0].ID)
		require.False(t, got[0].Expired)
		require.Zero(t, got[0].ExpiresAt)
		require.Contains(t, got[0].summaryLine(), "submitted before the upgrade. ID")
		require.False(t, got[1].Expired)
		require.Equal(t, yesterday+7*millisPerDay, got[1].ExpiresAt)
		require.Contains(t, got[1].summaryLine(), ", expires "+formatDay(yesterday+7*millisPerDay)+".")
	})

	t.Run("an unreadable record is skipped", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		f.store[kvRequestPrefix+f.channelID] = []byte("not json")

		got, _, err := f.p.listOwnRequests("u1", maxListedRequests)

		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, f.adminID, got[0].ID)
		f.api.AssertCalled(t, "LogWarn", "skipping unreadable stored request", "request_id", f.channelID, "error", mock.Anything)
	})

	t.Run("only the first entries are loaded", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)

		got, more, err := f.p.listOwnRequests("u1", 1)

		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, f.adminID, got[0].ID)
		// The channel request and the entry whose request is gone.
		require.Equal(t, 2, more)
		f.api.AssertNotCalled(t, "GetTeam", mock.Anything)
	})

	t.Run("a type that is turned off is left out", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		config := listConfig()
		config.AllowChannelRequests = false
		f.p.setConfiguration(config)

		got, _, err := f.p.listOwnRequests("u1", maxListedRequests)

		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, f.adminID, got[0].ID)
		f.api.AssertNotCalled(t, "KVGet", pendingListKey(pendingKindChannel, "u1"))
	})

	t.Run("another user's request in the list is left out", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		f.store[pendingListKey(pendingKindChannel, "u2")] = f.store[pendingListKey(pendingKindChannel, "u1")]

		got, _, err := f.p.listOwnRequests("u2", maxListedRequests)

		require.NoError(t, err)
		require.Empty(t, got)
	})

	for name, kv := range map[string]func(api *plugintest.API){
		"list load fails": func(api *plugintest.API) {
			api.On("KVGet", pendingListKey(pendingKindChannel, "u1")).Return(nil, testAppErr("kv down"))
		},
		"list unreadable": func(api *plugintest.API) {
			api.On("KVGet", pendingListKey(pendingKindChannel, "u1")).Return([]byte("not json"), nil)
		},
		"request load fails": func(api *plugintest.API) {
			api.On("KVGet", pendingListKey(pendingKindChannel, "u1")).Return(recordJSON(t, []pendingEntry{{ID: "r1"}}), nil)
			api.On("KVGet", pendingListKey(pendingKindAdmin, "u1")).Return(nil, nil)
			api.On("KVGet", kvRequestPrefix+"r1").Return(nil, testAppErr("kv down"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(listConfig())
			kv(api)

			_, _, err := p.listOwnRequests("u1", maxListedRequests)

			require.Error(t, err)
		})
	}
}

func TestRequestStatus(t *testing.T) {
	t.Run("the requester sees both kinds", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)

		got, err := f.p.requestStatus("u1", f.channelID)
		require.NoError(t, err)
		require.Equal(t, pendingKindChannel, got.Kind)

		got, err = f.p.requestStatus("u1", f.adminID)
		require.NoError(t, err)
		require.Equal(t, pendingKindAdmin, got.Kind)
	})

	t.Run("an approver sees it", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		f.api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)

		got, err := f.p.requestStatus("admin1", f.channelID)
		require.NoError(t, err)
		require.Equal(t, f.channelID, got.ID)
	})

	t.Run("a Team Admin of the request's team sees both kinds", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		f.api.On("GetUser", "ta").Return(&model.User{Id: "ta", Roles: "system_user"}, nil)
		f.api.On("GetTeamMember", "team1", "ta").Return(&model.TeamMember{TeamId: "team1", UserId: "ta", SchemeAdmin: true}, nil)

		_, err := f.p.requestStatus("ta", f.adminID)
		require.NoError(t, err)
		_, err = f.p.requestStatus("ta", f.channelID)
		require.NoError(t, err)
	})

	t.Run("a Team Admin of another team sees neither", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		f.api.On("GetUser", "ta").Return(&model.User{Id: "ta", Roles: "system_user"}, nil)
		f.api.On("GetTeamMember", "team1", "ta").Return(nil, testAppErr("not found"))

		_, err := f.p.requestStatus("ta", f.adminID)
		require.ErrorIs(t, err, errNoVisibleRequest)
		_, err = f.p.requestStatus("ta", f.channelID)
		require.ErrorIs(t, err, errNoVisibleRequest)
	})

	t.Run("a type that is turned off reads as not found", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		config := listConfig()
		config.AllowChannelAdminRequests = false
		f.p.setConfiguration(config)

		_, err := f.p.requestStatus("u1", f.adminID)
		require.ErrorIs(t, err, errNoVisibleRequest)
		_, err = f.p.requestStatus("u1", f.channelID)
		require.NoError(t, err)
	})

	t.Run("a request the user can't see is not looked up further", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		f.api.On("GetUser", "u2").Return(&model.User{Id: "u2", Roles: "system_user"}, nil)
		f.api.On("GetTeamMember", "team1", "u2").Return(&model.TeamMember{TeamId: "team1", UserId: "u2"}, nil)

		_, err := f.p.requestStatus("u2", f.adminID)

		require.ErrorIs(t, err, errNoVisibleRequest)
		f.api.AssertNotCalled(t, "GetUser", "u_nom")
	})

	t.Run("anyone else gets the same answer as for a missing request", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)
		f.api.On("GetUser", "u2").Return(&model.User{Id: "u2", Roles: "system_user"}, nil)
		f.api.On("GetTeamMember", "team1", "u2").Return(&model.TeamMember{TeamId: "team1", UserId: "u2"}, nil)
		f.api.On("GetUser", "ghost").Return(nil, testAppErr("not found"))

		for _, call := range []struct{ user, id string }{
			{"u2", f.channelID},
			{"ghost", f.channelID},
			{"u1", model.NewId()},
			{"u1", "not-an-id"},
		} {
			_, err := f.p.requestStatus(call.user, call.id)
			require.ErrorIs(t, err, errNoVisibleRequest, call)
		}
	})

	t.Run("a load failure is not hidden", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(listConfig())
		id := model.NewId()
		api.On("KVGet", kvRequestPrefix+id).Return(nil, testAppErr("kv down"))

		_, err := p.requestStatus("u1", id)
		require.Error(t, err)
		require.NotErrorIs(t, err, errNoVisibleRequest)
	})
}

func TestChannelLabel_DeletedChannel(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetChannel", "gone").Return(nil, testAppErr("not found"))

	require.Equal(t, "a channel that no longer exists", p.channelLabel("gone"))
}

func TestSummaryLine(t *testing.T) {
	base := requestSummary{ID: "id1", Summary: "Channel **X**", CreatedAt: oct3}

	require.Equal(t, "Channel **X**, submitted 2026-10-03. ID `id1`", base.summaryLine())

	withExpiry := base
	withExpiry.ExpiresAt = oct5
	require.Equal(t, "Channel **X**, submitted 2026-10-03, expires 2026-10-05. ID `id1`", withExpiry.summaryLine())

	expired := withExpiry
	expired.Expired = true
	require.Equal(t, "Channel **X**, submitted 2026-10-03, expired (an approver can close it). ID `id1`", expired.summaryLine())

	old := base
	old.CreatedAt = 0
	require.Contains(t, old.summaryLine(), "submitted before the upgrade")
}

func TestExecuteCommand_List(t *testing.T) {
	t.Run("lists the user's requests", func(t *testing.T) {
		f := newStatusFixture(t, 0, oct5, oct3)

		resp := runCommand(f.p, "/mattermost-request-hub list")

		require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
		lines := strings.Split(resp.Text, "\n")
		require.Equal(t, "Your pending requests:", lines[0])
		require.Len(t, lines, 3)
		require.Contains(t, lines[1], "ID `"+f.adminID+"`")
		require.Contains(t, lines[2], "ID `"+f.channelID+"`")
	})

	t.Run("nothing pending", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(listConfig())
		fakeKV(api)

		require.Equal(t, "You have no pending requests.", runCommand(p, "/mattermost-request-hub list").Text)
	})

	t.Run("every type turned off", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{})

		// No KV calls are mocked, so a read would have panicked.
		require.Equal(t, listRequestsDisabledMsg, runCommand(p, "/mattermost-request-hub list").Text)
	})

	t.Run("long lists are capped", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(listConfig())
		store := fakeKV(api)
		api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng"}, nil)
		var entries []pendingEntry
		for i := range maxListedRequests + 3 {
			id := model.NewId()
			store[kvRequestPrefix+id] = recordJSON(t, &channelRequest{ID: id, RequesterID: "u1", TeamID: "team1", Name: fmt.Sprintf("team-%d", i), CreatedAt: oct3 + int64(i)})
			entries = append(entries, pendingEntry{ID: id, CreatedAt: oct3 + int64(i)})
		}
		store[pendingListKey(pendingKindChannel, "u1")] = recordJSON(t, entries)

		lines := strings.Split(runCommand(p, "/mattermost-request-hub list").Text, "\n")

		require.Len(t, lines, maxListedRequests+2)
		require.Equal(t, "- ...and 3 more", lines[len(lines)-1])
	})

	t.Run("a load failure", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(listConfig())
		api.On("KVGet", mock.Anything).Return(nil, testAppErr("kv down"))

		require.Equal(t, "Could not load the requests. Please try again.", runCommand(p, "/mattermost-request-hub list").Text)
	})
}

func TestListMyRequestsTool(t *testing.T) {
	t.Run("no user", func(t *testing.T) {
		stubMCP(t)
		p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})

		require.Equal(t, errNoMCPUser.Error(), mcpToolError(t, callMCPTool(t, p, "", "list_my_requests", nil)))
	})

	t.Run("the caller's requests", func(t *testing.T) {
		stubMCP(t)
		f := newStatusFixture(t, 7, oct5, oct3)
		p := newMCPTestPlugin(t, f.api, &configuration{prefixes: []channelPrefix{{Prefix: "team-"}}, expiryDays: 7})

		out := mcpToolOutput[ListMyRequestsOutput](t, callMCPTool(t, p, "u1", "list_my_requests", nil))

		require.Equal(t, []MCPRequest{
			{ID: f.adminID, Kind: "channel_admin", Summary: "Channel Admin for @nom in ~town", State: "expired", CreatedAt: "2026-10-03T00:00:00Z", ExpiresAt: "2026-10-10T00:00:00Z"},
			{ID: f.channelID, Kind: "channel", Summary: `Channel **X \*bold\*** (` + "`team-x`" + `) in Eng`, State: "expired", CreatedAt: "2026-10-05T00:00:00Z", ExpiresAt: "2026-10-12T00:00:00Z"},
		}, out.Requests)
		require.Zero(t, out.More)
	})

	t.Run("every type turned off", func(t *testing.T) {
		stubMCP(t)
		p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})
		p.setConfiguration(&configuration{EnableMCP: true})

		require.Equal(t, listRequestsDisabledMsg, mcpToolError(t, callMCPTool(t, p, "u1", "list_my_requests", nil)))
	})

	t.Run("a load failure", func(t *testing.T) {
		stubMCP(t)
		api := &plugintest.API{}
		stubLogs(api)
		api.On("KVGet", mock.Anything).Return(nil, testAppErr("kv down"))
		p := newMCPTestPlugin(t, api, &configuration{})

		require.Equal(t, "could not load your requests; try again", mcpToolError(t, callMCPTool(t, p, "u1", "list_my_requests", nil)))
	})
}

func TestListRequestsToApproveTool(t *testing.T) {
	t.Run("no user", func(t *testing.T) {
		stubMCP(t)
		p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})

		require.Equal(t, errNoMCPUser.Error(), mcpToolError(t, callMCPTool(t, p, "", "list_requests_to_approve", nil)))
	})

	t.Run("requests the caller can approve", func(t *testing.T) {
		stubMCP(t)
		api, _, store, _ := newerTypeFixture(t)
		p := newMCPTestPlugin(t, api, &configuration{})
		id := model.NewId()
		record := newerTypeRecord(pendingKindTeam, id, twoStepState{}).(*teamCreationRequest)
		record.CreatedAt = oct3
		store[kvTeamRequestPrefix+id] = recordJSON(t, record)

		out := mcpToolOutput[ListRequestsToApproveOutput](t, callMCPTool(t, p, "admin1", "list_requests_to_approve", nil))

		require.Equal(t, []MCPRequest{
			{ID: id, Kind: "team", Summary: "Team **Field Ops** (`/field-ops`)", State: "pending", CreatedAt: "2026-10-03T00:00:00Z"},
		}, out.Requests)
		require.Zero(t, out.More)
	})

	t.Run("every type turned off", func(t *testing.T) {
		stubMCP(t)
		p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})
		p.setConfiguration(&configuration{EnableMCP: true})

		require.Equal(t, listRequestsDisabledMsg, mcpToolError(t, callMCPTool(t, p, "u1", "list_requests_to_approve", nil)))
	})

	t.Run("a load failure", func(t *testing.T) {
		stubMCP(t)
		api := &plugintest.API{}
		stubLogs(api)
		api.On("GetUser", "u1").Return(nil, testAppErr("gone"))
		p := newMCPTestPlugin(t, api, &configuration{})

		require.Equal(t, "could not load the requests; try again", mcpToolError(t, callMCPTool(t, p, "u1", "list_requests_to_approve", nil)))
	})
}

func TestGetRequestStatusTool(t *testing.T) {
	t.Run("no user", func(t *testing.T) {
		stubMCP(t)
		p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})

		require.Equal(t, errNoMCPUser.Error(), mcpToolError(t, callMCPTool(t, p, "", "get_request_status", map[string]any{"request_id": model.NewId()})))
	})

	t.Run("the caller's request", func(t *testing.T) {
		stubMCP(t)
		f := newStatusFixture(t, 0, oct5, oct3)
		p := newMCPTestPlugin(t, f.api, &configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})

		out := mcpToolOutput[MCPRequest](t, callMCPTool(t, p, "u1", "get_request_status", map[string]any{"request_id": f.channelID}))

		require.Equal(t, f.channelID, out.ID)
		require.Equal(t, "pending", out.State)
		require.Empty(t, out.ExpiresAt)
	})

	t.Run("not visible", func(t *testing.T) {
		stubMCP(t)
		f := newStatusFixture(t, 0, oct5, oct3)
		p := newMCPTestPlugin(t, f.api, &configuration{})

		require.Equal(t, errNoVisibleRequest.Error(), mcpToolError(t, callMCPTool(t, p, "u1", "get_request_status", map[string]any{"request_id": model.NewId()})))
	})

	t.Run("a load failure", func(t *testing.T) {
		stubMCP(t)
		api := &plugintest.API{}
		stubLogs(api)
		api.On("KVGet", mock.Anything).Return(nil, testAppErr("kv down"))
		p := newMCPTestPlugin(t, api, &configuration{})

		require.Equal(t, "could not load that request; try again", mcpToolError(t, callMCPTool(t, p, "u1", "get_request_status", map[string]any{"request_id": model.NewId()})))
	})
}

func TestGuardTool(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("LogError", "MCP tool failed", "tool", "boom", "panic", "kaboom").Once()

	guarded := guardTool(p, "boom", func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, MCPMessageOutput, error) {
		panic("kaboom")
	})
	result, out, err := guarded(context.Background(), nil, struct{}{})

	require.Nil(t, result)
	require.Equal(t, MCPMessageOutput{}, out)
	require.ErrorIs(t, err, errMCPInternal)

	// A handler that doesn't panic passes straight through.
	ok := guardTool(p, "fine", func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, MCPMessageOutput, error) {
		return nil, MCPMessageOutput{Message: "done"}, nil
	})
	_, out, err = ok(context.Background(), nil, struct{}{})
	require.NoError(t, err)
	require.Equal(t, "done", out.Message)
}
