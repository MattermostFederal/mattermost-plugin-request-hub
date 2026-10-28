package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost-plugin-agents/v2/external/pluginmcp"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const agentsPluginID = "mattermost-ai"

// mcpStub replaces registration with the Agents plugin for one test and
// records which servers were registered and unregistered.
type mcpStub struct {
	registered    []*pluginmcp.Server
	unregistered  []*pluginmcp.Server
	registerErr   error
	unregisterErr error
}

func stubMCP(t *testing.T) *mcpStub {
	t.Helper()
	stub := &mcpStub{}
	origRegister, origUnregister := mcpRegister, mcpUnregister
	mcpRegister = func(server *pluginmcp.Server) error {
		stub.registered = append(stub.registered, server)
		return stub.registerErr
	}
	mcpUnregister = func(server *pluginmcp.Server) error {
		stub.unregistered = append(stub.unregistered, server)
		return stub.unregisterErr
	}
	t.Cleanup(func() {
		mcpRegister, mcpUnregister = origRegister, origUnregister
	})
	return stub
}

// newMCPTestPlugin returns an activated plugin with MCP enabled, its
// server running, and channel and Channel Admin requests turned on.
func newMCPTestPlugin(t *testing.T, api *plugintest.API, config *configuration) *Plugin {
	t.Helper()
	p := newTestPlugin(api)
	config.EnableMCP = true
	config.AllowChannelRequests = true
	config.AllowChannelAdminRequests = true
	config.AllowTeamRequests = true
	config.AllowTeamAdminRequests = true
	config.AllowTeamMemberRequests = true
	config.AllowTeamGuestRequests = true
	config.AllowBotTokenRequests = true
	config.AllowWebhookRequests = true
	p.setConfiguration(config)
	p.mcpActive = true
	require.NoError(t, p.syncMCPServer())
	require.NotNil(t, p.currentMCPServer())
	return p
}

type headerTransport struct {
	headers map[string]string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	for name, value := range h.headers {
		clone.Header.Set(name, value)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

// mcpSession connects a go-sdk client to the plugin's ServeHTTP the way
// the Agents plugin does, acting for userID (no user when empty).
func mcpSession(t *testing.T, p *Plugin, userID string) *mcp.ClientSession {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(&plugin.Context{}, w, r)
	}))
	t.Cleanup(server.Close)

	headers := map[string]string{"Mattermost-Plugin-ID": agentsPluginID}
	if userID != "" {
		headers["X-Mattermost-UserID"] = userID
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             server.URL + mcpPath,
		HTTPClient:           &http.Client{Transport: headerTransport{headers}},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func serveMCP(p *Plugin, method, path, pluginID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	if pluginID != "" {
		r.Header.Set("Mattermost-Plugin-ID", pluginID)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(&plugin.Context{}, w, r)
	return w
}

// stubActivation mocks the calls OnActivate makes besides the MCP server.
// The config it sets has no approval team and System Admin two-step
// approvers, so the background channel setup has nothing to create.
func stubActivation(t *testing.T, api *plugintest.API, p *Plugin, config *configuration, commandErr error) {
	t.Helper()
	config.TwoStepApproverMode = twoStepModeSystemAdmin
	p.setConfiguration(config)
	api.On("EnsureBotUser", mock.Anything).Return("bot-id", nil)
	api.On("GetBundlePath").Return("", testAppErr("no bundle")).Maybe()
	api.On("RegisterCommand", mock.Anything).Return(commandErr)
	api.On("KVGet", mock.Anything).Return(nil, nil).Maybe()
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("GetTeams").Return([]*model.Team{}, nil).Maybe()
}

func TestSyncMCPServer_DoesNothingBeforeActivation(t *testing.T) {
	stub := stubMCP(t)
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{EnableMCP: true})

	require.NoError(t, p.syncMCPServer())

	require.Nil(t, p.currentMCPServer())
	require.Empty(t, stub.registered)
}

func TestOnActivate_LeavesMCPOffByDefault(t *testing.T) {
	stub := stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)

	stubActivation(t, api, p, &configuration{}, nil)

	require.NoError(t, p.OnActivate())
	defer p.stopTwoStepChannelReconciler()
	require.Nil(t, p.currentMCPServer())
	require.Empty(t, stub.registered)
}

func TestOnActivate_StartsMCPServerWhenEnabled(t *testing.T) {
	stub := stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)

	stubActivation(t, api, p, &configuration{EnableMCP: true}, nil)

	require.NoError(t, p.OnActivate())
	defer p.stopTwoStepChannelReconciler()
	require.NotNil(t, p.currentMCPServer())
	require.Equal(t, []*pluginmcp.Server{p.currentMCPServer()}, stub.registered)
}

func TestOnActivate_CommandFailureStopsActivation(t *testing.T) {
	stub := stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)

	stubActivation(t, api, p, &configuration{EnableMCP: true}, errors.New("no command"))

	require.EqualError(t, p.OnActivate(), "failed to register slash command: no command")
	require.Nil(t, p.currentMCPServer())
	require.Empty(t, stub.registered)
}

func TestOnActivate_IncompleteManifestStopsActivation(t *testing.T) {
	stub := stubMCP(t)
	origManifest := manifest
	manifest = &model.Manifest{Id: origManifest.Id, Name: origManifest.Name}
	t.Cleanup(func() { manifest = origManifest })

	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)

	stubActivation(t, api, p, &configuration{EnableMCP: true}, nil)

	require.ErrorContains(t, p.OnActivate(), "failed to initialize the MCP server")
	require.Nil(t, p.currentMCPServer())
	require.Empty(t, stub.registered)
}

func TestOnActivate_RegistrationFailureIsLoggedNotFatal(t *testing.T) {
	stub := stubMCP(t)
	stub.registerErr = errors.New("agents unavailable")
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)

	stubActivation(t, api, p, &configuration{EnableMCP: true}, nil)

	require.NoError(t, p.OnActivate())
	defer p.stopTwoStepChannelReconciler()
	require.NotNil(t, p.currentMCPServer())
	api.AssertCalled(t, "LogWarn", "MCP registration unavailable; continuing activation", "error", "agents unavailable")
}

func TestOnConfigurationChange_TogglesMCPServer(t *testing.T) {
	stub := stubMCP(t)
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.mcpActive = true

	enabled := false
	api.On("KVGet", kvPrefixMaxLenV48).Return([]byte("1"), nil)
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		args.Get(0).(*configuration).EnableMCP = enabled
	}).Return(nil)

	require.NoError(t, p.OnConfigurationChange())
	require.Nil(t, p.currentMCPServer())

	enabled = true
	require.NoError(t, p.OnConfigurationChange())
	first := p.currentMCPServer()
	require.NotNil(t, first)
	require.Equal(t, []*pluginmcp.Server{first}, stub.registered)

	// Saving again with the setting unchanged keeps the running server.
	require.NoError(t, p.OnConfigurationChange())
	require.Same(t, first, p.currentMCPServer())
	require.Len(t, stub.registered, 1)

	enabled = false
	require.NoError(t, p.OnConfigurationChange())
	require.Nil(t, p.currentMCPServer())
	require.Equal(t, []*pluginmcp.Server{first}, stub.unregistered)

	// A stopped server can't register again, so re-enabling builds a new one.
	enabled = true
	require.NoError(t, p.OnConfigurationChange())
	second := p.currentMCPServer()
	require.NotNil(t, second)
	require.NotSame(t, first, second)
	require.Equal(t, []*pluginmcp.Server{first, second}, stub.registered)
}

func TestOnConfigurationChange_LogsMCPFailure(t *testing.T) {
	stubMCP(t)
	origManifest := manifest
	manifest = &model.Manifest{}
	t.Cleanup(func() { manifest = origManifest })

	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.mcpActive = true
	api.On("KVGet", kvPrefixMaxLenV48).Return([]byte("1"), nil)
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		args.Get(0).(*configuration).EnableMCP = true
	}).Return(nil)

	require.NoError(t, p.OnConfigurationChange())
	require.Nil(t, p.currentMCPServer())
	api.AssertCalled(t, "LogError", "channel-requests: failed to apply the MCP setting", "error", mock.Anything)
}

func TestOnDeactivate_UnregistersAndStaysStopped(t *testing.T) {
	stub := stubMCP(t)
	stub.unregisterErr = errors.New("agents unavailable")
	api := &plugintest.API{}
	stubLogs(api)
	p := newMCPTestPlugin(t, api, &configuration{})
	server := p.currentMCPServer()

	require.NoError(t, p.OnDeactivate())

	require.Nil(t, p.currentMCPServer())
	require.Equal(t, []*pluginmcp.Server{server}, stub.unregistered)
	api.AssertCalled(t, "LogWarn", "MCP unregister failed; continuing shutdown", "error", "agents unavailable")

	// A configuration change after deactivation doesn't start a server.
	require.NoError(t, p.syncMCPServer())
	require.Nil(t, p.currentMCPServer())
	require.Len(t, stub.registered, 1)
}

func TestOnDeactivate_WithoutServerDoesNothing(t *testing.T) {
	stub := stubMCP(t)
	p := newTestPlugin(&plugintest.API{})

	require.NoError(t, p.OnDeactivate())
	require.Empty(t, stub.unregistered)
}

func TestServeHTTP_MCPRouteIsMatchedBeforeTheSwitch(t *testing.T) {
	stubMCP(t)
	p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})

	for _, path := range []string{mcpPath, mcpPath + "/anything"} {
		// No Agents plugin header: the MCP server answers 403, where the
		// switch would have answered 404 and requireUserID 401.
		w := serveMCP(p, http.MethodPost, path, "")
		require.Equal(t, http.StatusForbidden, w.Code, path)
	}

	// A path that only starts with the same letters is not MCP.
	w := serveMCP(p, http.MethodPost, mcpPath+"x", agentsPluginID)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestServeHTTP_MCPNotReadyReturns503(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})

	w := serveMCP(p, http.MethodPost, mcpPath, agentsPluginID)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestServeHTTP_MCPOtherPluginIsForbidden(t *testing.T) {
	stubMCP(t)
	p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})

	w := serveMCP(p, http.MethodPost, mcpPath, "com.example.other")

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestMCP_ListsOnlyTheRequestTools(t *testing.T) {
	stubMCP(t)
	p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})
	session := mcpSession(t, p, "u1")

	listed, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		// Identity comes from the caller, never from the arguments.
		schema, ok := tool.InputSchema.(map[string]any)
		require.True(t, ok, tool.Name)
		properties, _ := schema["properties"].(map[string]any)
		for property := range properties {
			// The bot's own username is the one property about a user that
			// is not about who is asking.
			if tool.Name == mcpToolPrefix+"request_bot" && property == "username" {
				continue
			}
			require.NotContains(t, property, "user", tool.Name)
			require.NotContains(t, property, "requester", tool.Name)
		}
	}
	require.ElementsMatch(t, []string{
		mcpToolPrefix + "list_prefixes",
		mcpToolPrefix + "request_channel",
		mcpToolPrefix + "request_channel_admin",
		mcpToolPrefix + "request_team",
		mcpToolPrefix + "request_team_admin",
		mcpToolPrefix + "request_team_member",
		mcpToolPrefix + "request_team_guest",
		mcpToolPrefix + "request_bot",
		mcpToolPrefix + "request_webhook",
	}, names)
}
