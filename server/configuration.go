package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

// kvPrefixMaxLenV48 marks that ChannelNamePrefixes has been considered for
// the 24→48 default-suffix-max migration. Prevents SavePluginConfig from
// re-entering OnConfigurationChange in a loop.
const kvPrefixMaxLenV48 = "migration_prefix_maxlen_v48"

// Previous / current shipped defaults for the numeric third field in
// ChannelNamePrefixes. Uploading a new bundle does not rewrite persisted
// config, so entries still carrying the old default need a one-shot bump.
const (
	legacyDefaultSuffixMax  = 24
	currentDefaultSuffixMax = 48
)

// Request type identifiers. Each names a kind of request users can
// submit and maps to a per-type admin toggle via RequestEnabled.
const (
	requestTypeChannel      = "channel"
	requestTypeChannelAdmin = "channel_admin"
	requestTypeTeam         = "team"
	requestTypeTeamAdmin    = "team_admin"
	requestTypeTeamMember   = "team_member"
	requestTypeTeamGuest    = "team_guest"
	requestTypeWebhook      = "webhook"
	requestTypeBot          = "bot"
)

// channelPrefix is one entry in the admin-configured list of allowed
// domain prefixes. The requester picks a prefix by ID from a dropdown;
// the server enforces that the final channel name starts with the
// prefix and (if a SuffixPattern is set) that the suffix matches it.
type channelPrefix struct {
	// Prefix is the literal string prepended to the suffix (e.g., "team-").
	Prefix string

	// Description is the human-readable label shown next to the prefix
	// in the dropdown ("Team collaboration channels"). Empty is fine.
	Description string

	// SuffixPattern is an optional regex applied to just the suffix
	// portion (not the full name). Nil when the admin didn't configure one.
	SuffixPattern *regexp.Regexp

	// SuffixPatternRaw is the un-compiled pattern string, retained for
	// UI display + error messages so users see the original regex text
	// rather than Go's re-serialized form.
	SuffixPatternRaw string
}

// configuration captures the plugin's external configuration as exposed in the Mattermost server
// configuration, as well as values computed from the configuration. Any public fields will be
// deserialized from the Mattermost server configuration in OnConfigurationChange.
//
// Field grouping (matches the System Console layout):
//  1. Approval routing (team + channel slugs)
//  2. Naming enforcement (structured prefix list — required)
//  3. Approver policy (who can approve)
//  4. Requestable types (per-type toggles)
//  5. Multi-step approver pools (bot / webhook two-step)
//  6. Agents (MCP tools)
type configuration struct {
	// --- 1. Approval routing ---

	// ApprovalTeam is the team where all named approval channels are created
	// (bot, webhook, team, audit, and the root channel/admin channel requests).
	// The channel URL is always "mattermost-channel-requests" — no separate
	// setting is needed.
	ApprovalTeam string

	// PerTeamApprovalChannels enables automatic creation of a per-team
	// approval channel (named by PerTeamChannelName) in every team. When
	// true, channel and admin-promotion requests route to the requesting
	// team's dedicated channel instead of the global ApprovalTeam/Channel.
	PerTeamApprovalChannels bool

	// --- 2. Naming enforcement ---
	// ChannelNamePrefixes is the raw multi-line prefix list. Each line
	// is "prefix|description|max_length". The structured editor
	// component serializes to this format on save; parsed back into
	// prefixes below. Prefix list is REQUIRED — there is no legacy
	// fallback.
	ChannelNamePrefixes string

	// --- 3. Approver policy ---
	AllowTeamAdminApprovers bool
	// AllowTeamAdminTeamApprovers lets Team Admins of the approval team
	// approve team creation requests. Off by default.
	AllowTeamAdminTeamApprovers bool

	// --- 4. Requestable types (per-type feature toggles) ---
	// Defaults (plugin.json): channel, channel-admin, team, team-admin,
	// team-member, bot, and webhook are on; guest stays off until opted in.
	AllowChannelRequests      bool
	AllowChannelAdminRequests bool
	AllowTeamRequests         bool
	AllowTeamAdminRequests    bool
	AllowTeamMemberRequests   bool
	AllowTeamGuestRequests    bool
	AllowBotTokenRequests     bool // covers full bot-account requests
	AllowWebhookRequests      bool

	// --- 5. Multi-step approver pools (bot / webhook two-step) ---
	// TwoStepApproverMode selects how security + system pools are resolved:
	//   "attribute"    — User Attributes / ABAC (CPA); default
	//   "usergroup"    — Mattermost custom / synced user groups by name
	//   "system_admin" — System Admins only (backup when ABAC/groups unavailable)
	TwoStepApproverMode string

	// SecurityApproverAttribute / SystemApproverAttribute: CPA field names used
	// when TwoStepApproverMode is "attribute". Rename to match your ABAC schema.
	SecurityApproverAttribute string
	SystemApproverAttribute   string

	// SecurityApproverGroup / SystemApproverGroup: Mattermost group names
	// (handles) used when TwoStepApproverMode is "usergroup". Defaults in
	// plugin.json are "security-approvers" / "system-approvers"; admins can
	// rename them in System Console. Empty → fall back to System Admins.
	SecurityApproverGroup string
	SystemApproverGroup   string

	// --- 6. Agents ---
	// EnableMCP offers the request tools to the Mattermost Agents plugin.
	// Off by default; syncMCPServer applies changes on save.
	EnableMCP bool

	// --- Parsed / computed (unexported) ---
	prefixes []channelPrefix
}

// TeamChannelName returns the fixed per-team approval channel URL name.
// Follows the mattermost-*-requests pattern used by all plugin-managed channels.
func (c *configuration) TeamChannelName() string {
	return "mattermost-channel-requests"
}

// UsesPrefixList reports whether the admin has configured at least one
// channel prefix. A prefix is REQUIRED to submit a request: when false,
// the slash command and modal show a "not configured" message and
// resolveChannelName returns an error.
func (c *configuration) UsesPrefixList() bool {
	return len(c.prefixes) > 0
}

// Prefixes returns the parsed prefix list (copy to avoid callers
// mutating shared state).
func (c *configuration) Prefixes() []channelPrefix {
	out := make([]channelPrefix, len(c.prefixes))
	copy(out, c.prefixes)
	return out
}

// RequestEnabled reports whether the given request type is currently
// enabled for users to submit. Unknown types return false.
func (c *configuration) RequestEnabled(requestType string) bool {
	switch requestType {
	case requestTypeChannel:
		return c.AllowChannelRequests
	case requestTypeChannelAdmin:
		return c.AllowChannelAdminRequests
	case requestTypeTeam:
		return c.AllowTeamRequests
	case requestTypeTeamAdmin:
		return c.AllowTeamAdminRequests
	case requestTypeTeamMember:
		return c.AllowTeamMemberRequests
	case requestTypeTeamGuest:
		return c.AllowTeamGuestRequests
	case requestTypeWebhook:
		return c.AllowWebhookRequests
	case requestTypeBot:
		return c.AllowBotTokenRequests
	default:
		return false
	}
}

func (p *Plugin) getConfiguration() *configuration {
	p.configurationLock.RLock()
	defer p.configurationLock.RUnlock()

	if p.configuration == nil {
		return &configuration{}
	}

	return p.configuration
}

func (p *Plugin) setConfiguration(configuration *configuration) {
	p.configurationLock.Lock()
	defer p.configurationLock.Unlock()

	if configuration != nil && p.configuration == configuration {
		if reflect.ValueOf(*configuration).NumField() == 0 {
			return
		}

		panic("setConfiguration called with the existing configuration")
	}

	p.configuration = configuration
}

func (p *Plugin) OnConfigurationChange() error {
	configuration := new(configuration)

	if err := p.API.LoadPluginConfiguration(configuration); err != nil {
		return errors.Wrap(err, "failed to load plugin configuration")
	}

	// One-shot: persisted ChannelNamePrefixes keep their old |24 default
	// across plugin uploads. Rewrite those to |48 and SavePluginConfig so
	// System Console + runtime enforcement both see the new default.
	// KV flag is set before SavePluginConfig to break the re-entry loop;
	// cleared again if persistence fails so the next config load retries.
	if migrated, _ := p.API.KVGet(kvPrefixMaxLenV48); migrated == nil {
		if bumped, changed := bumpLegacyDefaultSuffixMax(configuration.ChannelNamePrefixes); changed {
			configuration.ChannelNamePrefixes = bumped
			_ = p.API.KVSet(kvPrefixMaxLenV48, []byte("1"))
			if cfgMap, mapErr := configurationToMap(configuration); mapErr != nil {
				_ = p.API.KVDelete(kvPrefixMaxLenV48)
				p.API.LogWarn("channel-requests: failed to serialize config for prefix max-length migration",
					"error", mapErr.Error())
			} else if appErr := p.API.SavePluginConfig(cfgMap); appErr != nil {
				_ = p.API.KVDelete(kvPrefixMaxLenV48)
				p.API.LogWarn("channel-requests: failed to persist prefix max-length migration",
					"error", appErr.Error())
			} else {
				p.API.LogInfo("channel-requests: migrated ChannelNamePrefixes suffix max from 24 to 48")
			}
		} else {
			_ = p.API.KVSet(kvPrefixMaxLenV48, []byte("1"))
		}
	}

	// Parse the new prefix-list setting. Each non-blank line is a
	// pipe-delimited entry: "prefix|description|optional_regex". Bad
	// entries are logged and skipped rather than failing the whole
	// activation — a typo in one line should not wedge the plugin.
	configuration.prefixes = parsePrefixList(configuration.ChannelNamePrefixes, p.API.LogError)

	// Detect PerTeamApprovalChannels being enabled for the first time so we can
	// backfill channels for teams that existed before the setting was toggled on.
	// Only triggers on a real false→true change after a previous configuration
	// exists — OnConfigurationChange runs before OnActivate, and the first load
	// must not race botUserID or duplicate the OnActivate sweep.
	p.configurationLock.RLock()
	hadPrevConfig := p.configuration != nil
	prevPerTeamEnabled := false
	prevTwoStep := ""
	prevApprovalTeam := ""
	if hadPrevConfig {
		prevPerTeamEnabled = p.configuration.PerTeamApprovalChannels
		prevTwoStep = twoStepPoolFingerprint(p.configuration)
		prevApprovalTeam = strings.TrimSpace(p.configuration.ApprovalTeam)
	}
	p.configurationLock.RUnlock()

	p.setConfiguration(configuration)

	// A newly selected Approval team is recorded on its next use.
	if hadPrevConfig && strings.TrimSpace(configuration.ApprovalTeam) != prevApprovalTeam {
		p.forgetApprovalTeam()
	}

	if hadPrevConfig && configuration.PerTeamApprovalChannels && !prevPerTeamEnabled {
		go p.ensureAllTeamApprovalChannels()
	}
	// Reseed bot/webhook channels when two-step pool settings change (mode,
	// group names, or attribute names) so newly configured security approvers
	// are added without a plugin restart.
	if hadPrevConfig && twoStepPoolFingerprint(configuration) != prevTwoStep {
		go p.syncTwoStepApproverChannels()
	}

	if err := p.syncMCPServer(); err != nil {
		p.API.LogError("channel-requests: failed to apply the MCP setting", "error", err.Error())
	}

	return nil
}

// twoStepPoolFingerprint captures the settings that control who must be in the
// bot/webhook approval channels.
func twoStepPoolFingerprint(c *configuration) string {
	if c == nil {
		return ""
	}
	return strings.Join([]string{
		c.twoStepMode(),
		strings.TrimSpace(c.SecurityApproverGroup),
		strings.TrimSpace(c.SystemApproverGroup),
		strings.TrimSpace(c.SecurityApproverAttribute),
		strings.TrimSpace(c.SystemApproverAttribute),
	}, "|")
}

// bumpLegacyDefaultSuffixMax rewrites numeric third fields equal to the
// previous shipped default (24) up to the current default (48). Custom
// lengths (16, 32, …) are left alone. Comment/blank lines are preserved.
func bumpLegacyDefaultSuffixMax(raw string) (string, bool) {
	if raw == "" {
		return raw, false
	}
	legacy := strconv.Itoa(legacyDefaultSuffixMax)
	current := strconv.Itoa(currentDefaultSuffixMax)
	lines := strings.Split(raw, "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "|", 3)
		if len(parts) < 3 {
			continue
		}
		if strings.TrimSpace(parts[2]) != legacy {
			continue
		}
		lines[i] = strings.TrimSpace(parts[0]) + "|" + strings.TrimSpace(parts[1]) + "|" + current
		changed = true
	}
	if !changed {
		return raw, false
	}
	return strings.Join(lines, "\n"), true
}

// configurationToMap JSON-round-trips exported settings into the map shape
// SavePluginConfig expects. Unexported computed fields are omitted.
func configurationToMap(c *configuration) (map[string]any, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// parsePrefixList parses the admin's multi-line prefix-list setting.
// Line format: "prefix|description|third". The THIRD field accepts
// two shapes:
//
//	numeric ("16", "24")         -> treated as a max suffix length.
//	                                Internally compiled as
//	                                [a-z0-9-]{2,N}. This is the format
//	                                the new PrefixEditor UI emits.
//	regex string ("[a-z]+", ...) -> compiled as-is. Backward compat
//	                                with hand-edited configs from
//	                                the pre-editor days.
//
// Empty third field -> no constraint on the suffix (other than MM's
// channel identifier validity check).
//
// Empty lines and comments (# prefix) are skipped. A malformed regex is
// logged and the prefix stays listed but rejects every suffix (fail
// closed), so a typo can't silently lift the naming rule.
func parsePrefixList(raw string, logErr func(msg string, keyValuePairs ...any)) []channelPrefix {
	var out []channelPrefix
	for lineNo, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		prefix := strings.TrimSpace(parts[0])
		if prefix == "" {
			logErr("channel-requests: skipping prefix entry with empty prefix",
				"line", lineNo+1, "text", line)
			continue
		}
		entry := channelPrefix{Prefix: prefix}
		if len(parts) >= 2 {
			entry.Description = strings.TrimSpace(parts[1])
		}
		if len(parts) >= 3 {
			third := strings.TrimSpace(parts[2])
			if third != "" {
				entry.SuffixPattern, entry.SuffixPatternRaw = compileSuffixRule(third, prefix, logErr)
			}
		}
		out = append(out, entry)
	}
	return out
}

// matchNothing is the suffix rule used when an admin's pattern can't be
// compiled: requests under that prefix are rejected until it's fixed.
var matchNothing = regexp.MustCompile(`[^\s\S]`)

// compileSuffixRule converts the third pipe-field into a compiled regex.
// Returns (matchNothing, third) when the field can't be compiled — the
// caller stores the raw text so error messages reference what the admin
// wrote.
func compileSuffixRule(third, prefix string, logErr func(msg string, keyValuePairs ...any)) (*regexp.Regexp, string) {
	// Numeric shape: treat as max length. Build [a-z0-9-]{2,N}. Cap at
	// 48 so long suffixes fit while leaving room for the prefix under
	// MM's 64-char channel name limit.
	if isPositiveInt(third) {
		n, _ := strconv.Atoi(third)
		if n < 2 {
			n = 2
		}
		if n > 48 {
			n = 48
		}
		// Anchor the pattern so it matches the whole suffix. This lets
		// callers use MatchString directly and avoids leftmost-first
		// surprises (e.g. an alternation like "dev|development" wrongly
		// matching only the "dev" prefix of "development").
		expr := fmt.Sprintf("^(?:[a-z0-9-]{2,%d})$", n)
		re, err := regexp.Compile(expr)
		if err != nil {
			// Shouldn't happen — regex is generated from a bounded int.
			logErr("channel-requests: failed to compile numeric suffix rule",
				"prefix", prefix, "n", third, "error", err.Error())
			return matchNothing, third
		}
		return re, third
	}

	// Legacy regex path. Anchor the admin-supplied pattern so it must
	// match the entire suffix — admins write patterns like [a-z0-9-]{2,16}
	// expecting "the suffix must be exactly this shape". Wrapping in a
	// non-capturing group keeps top-level alternations intact. The pattern
	// must also compile on its own so the wrapping group stays balanced.
	if _, err := regexp.Compile(third); err != nil {
		logErr("channel-requests: invalid suffix regex for prefix; rejecting all suffixes until it's fixed",
			"prefix", prefix, "pattern", third, "error", err.Error())
		return matchNothing, third
	}
	re, err := regexp.Compile("^(?:" + third + ")$")
	if err != nil {
		logErr("channel-requests: invalid suffix regex for prefix; rejecting all suffixes until it's fixed",
			"prefix", prefix, "pattern", third, "error", err.Error())
		return matchNothing, third
	}
	return re, third
}

func isPositiveInt(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
