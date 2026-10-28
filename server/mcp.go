package main

import (
	"net/http"
	"strings"

	"github.com/mattermost/mattermost-plugin-agents/v2/external/pluginmcp"
	"github.com/pkg/errors"
)

const mcpPath = "/mcp"

var (
	mcpNewServer = pluginmcp.NewServer

	mcpRegister = func(server *pluginmcp.Server) error {
		return server.Register()
	}

	mcpUnregister = func(server *pluginmcp.Server) error {
		return server.Unregister()
	}
)

// syncMCPServer starts or stops the MCP server to match the EnableMCP
// setting. It runs from OnActivate and from every configuration change
// after it, so turning the setting on or off takes effect on save. A
// stopped server can't register again, so each start builds a new one.
// Before activation, and after deactivation, it does nothing.
func (p *Plugin) syncMCPServer() error {
	// Serialize whole syncs so an unregister for an old server can't land
	// at the Agents plugin after the register for its replacement.
	p.mcpSyncLock.Lock()
	defer p.mcpSyncLock.Unlock()

	enabled := p.getConfiguration().EnableMCP

	p.mcpServerLock.Lock()
	if !p.mcpActive || enabled == (p.mcpServer != nil) {
		p.mcpServerLock.Unlock()
		return nil
	}
	if !enabled {
		server := p.mcpServer
		p.mcpServer = nil
		p.mcpServerLock.Unlock()
		p.unregisterMCPServerBestEffort(server)
		return nil
	}
	server, err := p.newMCPServer()
	if err != nil {
		p.mcpServerLock.Unlock()
		return err
	}
	p.mcpServer = server
	p.mcpServerLock.Unlock()

	p.registerMCPServerBestEffort(server)
	return nil
}

// stopMCPServer unregisters the MCP server, if one is running, and stops
// later configuration changes from starting another. Called from
// OnDeactivate.
func (p *Plugin) stopMCPServer() {
	p.mcpSyncLock.Lock()
	defer p.mcpSyncLock.Unlock()

	p.mcpServerLock.Lock()
	server := p.mcpServer
	p.mcpServer = nil
	p.mcpActive = false
	p.mcpServerLock.Unlock()

	if server != nil {
		p.unregisterMCPServerBestEffort(server)
	}
}

func (p *Plugin) newMCPServer() (*pluginmcp.Server, error) {
	if manifest.Id == "" || manifest.Version == "" || strings.TrimSpace(manifest.Name) == "" {
		return nil, errors.New("the manifest is missing an id, a version or a name")
	}

	server := mcpNewServer(p.API, pluginmcp.Config{
		PluginID:       manifest.Id,
		Name:           manifest.Name + " MCP",
		Path:           mcpPath,
		ExposeExternal: true,
		Version:        manifest.Version,
	})

	p.registerMCPTools(server)
	return server, nil
}

// registerMCPServerBestEffort starts registration with the Agents plugin.
// Register returns at once and retries in the background, so a missing or
// slow Agents plugin never blocks or fails this plugin.
func (p *Plugin) registerMCPServerBestEffort(server *pluginmcp.Server) {
	if err := mcpRegister(server); err != nil {
		p.API.LogWarn("MCP registration unavailable; continuing activation", "error", err.Error())
	}
}

func (p *Plugin) unregisterMCPServerBestEffort(server *pluginmcp.Server) {
	if err := mcpUnregister(server); err != nil {
		p.API.LogWarn("MCP unregister failed; continuing shutdown", "error", err.Error())
	}
}

// serveMCPIfMatch hands requests under mcpPath to the MCP server. It must
// run before ServeHTTP's exact-path switch and without requireUserID: the
// Agents plugin calls in over inter-plugin RPC with no Mattermost-User-Id,
// and pluginmcp.Server.ServeHTTP checks the caller itself.
func (p *Plugin) serveMCPIfMatch(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != mcpPath && !strings.HasPrefix(r.URL.Path, mcpPath+"/") {
		return false
	}

	server := p.currentMCPServer()
	if server == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return true
	}

	server.ServeHTTP(w, r)
	return true
}

func (p *Plugin) currentMCPServer() *pluginmcp.Server {
	p.mcpServerLock.RLock()
	defer p.mcpServerLock.RUnlock()

	return p.mcpServer
}
