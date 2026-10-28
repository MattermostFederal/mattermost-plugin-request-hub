package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost-plugin-agents/v2/external/pluginmcp"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

// how often to pull security/system group members into bot/webhook channels.
// Custom user groups have no membership plugin hook; this closes the gap when
// someone is re-added while already logged in.
const twoStepChannelReconcileInterval = 2 * time.Minute

type Plugin struct {
	plugin.MattermostPlugin

	configurationLock sync.RWMutex
	configuration     *configuration

	// mcpSyncLock serializes starting and stopping the MCP server.
	// mcpServerLock guards mcpServer and mcpActive, which ServeHTTP reads.
	// mcpActive is true between OnActivate and OnDeactivate.
	mcpSyncLock   sync.Mutex
	mcpServerLock sync.RWMutex
	mcpServer     *pluginmcp.Server
	mcpActive     bool

	// botUserID is the user ID of the bot account used to post approval requests and notify
	// requesters of the outcome.
	botUserID string

	// twoStepSyncStop cancels the periodic two-step channel reconciler.
	twoStepSyncStop chan struct{}

	// Test seams for webhook create/delete side effects (nil in production).
	createWebhookFn func(req *webhookRequest, adminUserID string) (dmMsg, hookID string, err error)
	deleteWebhookFn func(webhookType, hookID string) error
}

func (p *Plugin) OnActivate() error {
	botUserID, err := p.API.EnsureBotUser(&model.Bot{
		Username:    "mattermost-request-hub",
		DisplayName: "Mattermost Request Hub",
		Description: "Handles channel, team, membership, guest, bot, and webhook requests; relays messages between requesters and reviewers.",
	})
	if err != nil {
		return errors.Wrap(err, "failed to ensure bot user")
	}
	p.botUserID = botUserID

	// Set the Mattermost icon as the bot's profile picture. Non-fatal if the
	// asset isn't present — the bot still works with its default avatar.
	if iconData, readErr := p.readBundleAsset("mattermost-icon.png"); readErr == nil {
		if setErr := p.API.SetProfileImage(p.botUserID, iconData); setErr != nil {
			p.API.LogWarn("failed to set bot profile image", "error", setErr.Error())
		}
	} else {
		p.API.LogWarn("bot profile image asset not found; place a PNG at assets/mattermost-icon.png to activate it")
	}

	if err := p.API.RegisterCommand(getCommand()); err != nil {
		return errors.Wrap(err, "failed to register slash command")
	}

	// Start the MCP server before the background work below, so a failure
	// here stops activation without leaving goroutines running.
	p.mcpServerLock.Lock()
	p.mcpActive = true
	p.mcpServerLock.Unlock()
	if err := p.syncMCPServer(); err != nil {
		return errors.Wrap(err, "failed to initialize the MCP server")
	}

	// Ensure approval channels exist in the background so activation doesn't
	// block on channel-creation API calls. ensureRootApprovalChannel is a no-op
	// when ApprovalTeam/ApprovalChannel are blank (first-time unconfigured).
	go func() {
		// Re-sync System Admins into every approval channel on activation so
		// admins promoted after initial install are added without manual
		// intervention. addSystemAdminsToChannel is idempotent.
		for _, ensure := range []func() *model.Channel{
			p.ensureRootApprovalChannel,
			p.ensureBotApprovalChannel,
			p.ensureWebhookApprovalChannel,
			p.ensureTeamCreationApprovalChannel,
			p.ensureAuditChannel,
		} {
			if ch := ensure(); ch != nil {
				p.addSystemAdminsToChannel(ch.Id)
			}
		}
		// Security / system pool members (usergroup or ABAC) into bot + webhook
		// private channels so they can see and click Approve.
		p.syncTwoStepApproverChannels()
		if p.getConfiguration().PerTeamApprovalChannels {
			p.ensureAllTeamApprovalChannels()
		}
		// One-shot: force every ticket/approval channel (root, bot, webhook,
		// team-creation, audit, per-team) into the managed
		// "Mattermost: Request Hub" sidebar category for all current members
		// (also renames a short-lived "Mattermost Requests" category if present).
		p.migrateTicketChannelsToManagedCategory()
	}()

	p.startTwoStepChannelReconciler()

	// Delete any token DM posts whose 72-hour window lapsed while the plugin
	// was not running, and re-arm in-process timers for the remainder.
	go p.cleanupExpiredTokenPosts()

	return nil
}

func (p *Plugin) OnDeactivate() error {
	p.stopTwoStepChannelReconciler()
	p.stopMCPServer()
	return nil
}

// readBundleAsset reads a known file from the plugin's bundled assets directory.
// name must be a basename we ship (no path separators) — callers pass constants.
func (p *Plugin) readBundleAsset(name string) ([]byte, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return nil, errors.New("invalid asset name")
	}
	bundlePath, appErr := p.API.GetBundlePath()
	if appErr != nil {
		return nil, appErr
	}
	// name is validated as a basename above; not attacker-controlled path input.
	return os.ReadFile(filepath.Join(bundlePath, "assets", name)) //nolint:gosec // G304: basename-only, fixed by caller
}
