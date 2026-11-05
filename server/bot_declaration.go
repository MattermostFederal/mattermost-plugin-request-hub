package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// Security declaration on a bot request. Bots are always regular Mattermost
// users (never Team Admin or System Admin). Privilege and channel-access
// notes are policy defaults, not form choices.

const (
	// Fixed Mattermost role for every bot this plugin creates.
	mmPrivilegeMemberUserOnly = "member_user_only"
	privilegeUserEquivalent   = "user_equivalent"

	// Legacy privilege values that may still appear on older requests /
	// inventory rows.
	privilegeRead     = "read"
	privilegePost     = "post"
	privilegeElevated = "elevated"

	hostingCICD         = "ci_cd"
	hostingOnPrem       = "on_prem_server"
	hostingCloudOrK8s   = "cloud_or_k8s"
	hostingWorkstation  = "workstation"
	hostingExternalSaaS = "external_saas"
	hostingOther        = "other"

	fieldBotPurpose            = "bot_purpose"
	fieldBotArchitectureURL    = "bot_architecture_url"
	fieldBotIntegrationURL     = "bot_integration_endpoint"
	fieldBotHostingEnv         = "bot_hosting_env"
	fieldBotHostingOtherDetail = "bot_hosting_other_detail"

	maxBotPurposeLen      = 500
	maxBotURLLen          = 500
	maxBotHostingOtherLen = 120

	// maxHostingEnvLen caps an environment name in an export filter.
	maxHostingEnvLen = 200

	kvBotIndexPrefix = "bot_index_"
	botIndexPageSize = 200

	defaultChannelAccessNote = "member permissions in channels the bot is added to (Mattermost user role)"
)

var validHostingEnvs = map[string]string{
	hostingCICD:         "CI/CD pipeline",
	hostingOnPrem:       "On-prem server / VM",
	hostingCloudOrK8s:   "Cloud VM / Kubernetes",
	hostingWorkstation:  "Developer workstation",
	hostingExternalSaaS: "External SaaS / vendor",
	hostingOther:        "Other",
}

// HostingEnvs returns the fixed hosting codes, in display order, for the
// dialog's select and the inventory export's filter. They are not
// configurable. Bots requested before these codes existed keep the
// environment name they were requested with (mattermost-prod, say), which
// hostingEnvLabel shows as is.
func (c *configuration) HostingEnvs() []string {
	return []string{
		hostingCICD, hostingOnPrem, hostingCloudOrK8s,
		hostingWorkstation, hostingExternalSaaS, hostingOther,
	}
}

// privilegeLabel returns a human label for a stored privilege value.
func privilegeLabel(value string) string {
	switch value {
	case privilegeUserEquivalent, "":
		return "Regular user (member); never Team Admin or System Admin"
	case privilegeRead:
		return "Read only"
	case privilegePost:
		return "Read and post"
	case privilegeElevated:
		return "Elevated (admin-level APIs)"
	default:
		return value
	}
}

func hostingEnvLabel(code string) string {
	if label, ok := validHostingEnvs[code]; ok {
		return label
	}
	return code
}

func mmPrivilegeLabel(code string) string {
	if code == mmPrivilegeMemberUserOnly || code == "" {
		return "Regular user (member); never Team Admin or System Admin"
	}
	return code
}

// botDeclaration is what the requester declares about the bot.
type botDeclaration struct {
	Purpose            string
	ArchitectureURL    string // optional https architecture / design review link
	IntegrationURL     string // required https integration endpoint
	PrivilegeLevel     string // always user_equivalent on new requests
	MMPrivilege        string // always member_user_only on new requests
	DataIngestion      string // policy note, not a form field
	HostingEnv         string
	HostingOtherDetail string
}

func (d botDeclaration) normalize() botDeclaration {
	out := d
	out.Purpose = strings.TrimSpace(d.Purpose)
	out.ArchitectureURL = strings.TrimSpace(d.ArchitectureURL)
	out.IntegrationURL = strings.TrimSpace(d.IntegrationURL)
	out.HostingEnv = strings.TrimSpace(d.HostingEnv)
	out.HostingOtherDetail = strings.TrimSpace(d.HostingOtherDetail)
	if out.HostingEnv != hostingOther {
		out.HostingOtherDetail = ""
	}
	out.PrivilegeLevel = privilegeUserEquivalent
	out.MMPrivilege = mmPrivilegeMemberUserOnly
	out.DataIngestion = defaultChannelAccessNote
	return out
}

// checkBotDeclaration validates the declared fields for a new submission.
func checkBotDeclaration(_ *configuration, in botDeclaration) (botDeclaration, error) {
	out := in.normalize()
	if out.Purpose == "" {
		return out, errors.New("A purpose is required: describe what the bot does and why. This becomes the bot's profile description.")
	}
	if err := checkTextLen("Purpose", out.Purpose, maxBotPurposeLen); err != nil {
		return out, err
	}
	if out.ArchitectureURL != "" {
		if err := checkSecureHTTPSURL("Architecture link", out.ArchitectureURL, maxBotURLLen); err != nil {
			return out, err
		}
	}
	if out.IntegrationURL == "" {
		return out, errors.New("An integration endpoint is required (https URL).")
	}
	if err := checkSecureHTTPSURL("Integration endpoint", out.IntegrationURL, maxBotURLLen); err != nil {
		return out, err
	}
	if _, ok := validHostingEnvs[out.HostingEnv]; !ok {
		return out, errors.New("Pick where the integration script or service runs (CI/CD, on-prem, cloud, workstation, SaaS, or other).")
	}
	if out.HostingEnv == hostingOther {
		if out.HostingOtherDetail == "" {
			return out, errors.New("Specify where the script or service runs when Hosting is Other.")
		}
		if err := checkTextLen("Hosting detail", out.HostingOtherDetail, maxBotHostingOtherLen); err != nil {
			return out, err
		}
	}
	return out, nil
}

// checkSecureHTTPSURL requires a real https URL: https scheme only and a
// host. The scheme check alone rules out javascript:, data: and file: links;
// those words later in the URL (a query string, say) are fine.
func checkSecureHTTPSURL(field, raw string, limit int) error {
	if err := checkByteLen(field, raw, limit); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)
	for _, bad := range []string{"javascript:", "data:", "vbscript:", "file:"} {
		if strings.HasPrefix(lower, bad) {
			return errors.Errorf("%s must be an https address, not a script or data URL", field)
		}
	}
	if strings.HasPrefix(lower, "http://") {
		return errors.Errorf("%s must use https:// (http is not allowed)", field)
	}
	if !strings.HasPrefix(lower, "https://") {
		return errors.Errorf("%s must start with https://", field)
	}
	// IsValidHTTPURL only knows a lowercase scheme; the browser accepts any
	// case, so match it.
	if !model.IsValidHTTPURL("https://" + trimmed[len("https://"):]) {
		return errors.Errorf("%s must be a valid https address", field)
	}
	u, err := url.Parse(trimmed)
	if err != nil || u == nil || u.Host == "" || !strings.EqualFold(u.Scheme, "https") {
		return errors.Errorf("%s must be a valid https address with a host", field)
	}
	if strings.ContainsAny(u.Host, " <>\"'`") {
		return errors.Errorf("%s host is not valid", field)
	}
	return nil
}

// botHomeMissingMsg is the reply when a request names no home channel,
// such as a slash command run in a direct message.
const botHomeMissingMsg = "A bot request needs a home channel: submit it from a public or private channel of a team you're in."

// requireBotHome checks the bot's home team and channel at submit: the
// requester is on the team and in the channel (System Admins: any), and the
// channel is a live public or private channel of that team. Membership is
// checked first, so someone outside the channel gets the same reply
// whatever state it is in.
func (p *Plugin) requireBotHome(requester *model.User, teamID, channelID string) error {
	if teamID == "" || channelID == "" {
		return errors.New(botHomeMissingMsg)
	}
	if !requester.IsSystemAdmin() {
		member, appErr := p.API.GetTeamMember(teamID, requester.Id)
		if appErr != nil || member == nil || member.DeleteAt != 0 {
			return errors.New("The bot's home team must be a team you're a member of.")
		}
	}
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil || channel == nil || channel.TeamId != teamID {
		return errors.New(botHomeMissingMsg)
	}
	if !requester.IsSystemAdmin() {
		if _, memErr := p.API.GetChannelMember(channelID, requester.Id); memErr != nil {
			return errors.New("The bot's home channel must be a channel you're a member of.")
		}
	}
	if channel.Type != model.ChannelTypeOpen && channel.Type != model.ChannelTypePrivate {
		return errors.New(botHomeMissingMsg)
	}
	if channel.DeleteAt != 0 {
		return errors.Errorf("~%s is archived, so it can't be the bot's home channel.", channel.Name)
	}
	if team, teamErr := p.API.GetTeam(teamID); teamErr != nil || team == nil || team.DeleteAt != 0 {
		return errors.New("The bot's home team is archived or could not be found.")
	}
	return nil
}

// botHomeBlocker re-checks the home team and channel before an approval
// step, as teamGuestApprovalBlocker does for a guest's channels. Requests
// submitted before homes existed have none and skip it.
func (p *Plugin) botHomeBlocker(req *botRequest, requester *model.User) string {
	if req.HomeTeamID == "" || req.HomeChannelID == "" {
		return ""
	}
	team, teamErr := p.API.GetTeam(req.HomeTeamID)
	if msg := p.teamBlocker(req.HomeTeamID, team, teamErr); msg != "" {
		return msg
	}
	if msg := p.requesterTeamBlocker(req.HomeTeamID, requester); msg != "" {
		return msg
	}
	channel, appErr := p.API.GetChannel(req.HomeChannelID)
	if msg := p.channelBlocker(req.HomeChannelID, channel, appErr); msg != "" {
		return msg
	}
	if channel.TeamId != req.HomeTeamID {
		return fmt.Sprintf("~%s is no longer in the bot's home team. Deny this request instead.", channel.Name)
	}
	return p.requesterChannelBlocker(req.HomeChannelID, requester)
}

// botIndexEntry links a bot Request Hub created to its request, and keeps
// what was declared for it, so the bot can be listed and traced by its user
// ID even after the request's history is gone.
type botIndexEntry struct {
	BotUserID          string         `json:"bot_user_id"`
	RequestID          string         `json:"request_id"`
	Username           string         `json:"username"`
	DisplayName        string         `json:"display_name,omitempty"`
	RequesterID        string         `json:"requester_id"`
	OwnerUserID        string         `json:"owner_user_id,omitempty"`
	CreatedAt          int64          `json:"created_at"`
	RequestedAt        int64          `json:"requested_at,omitempty"`
	ApprovedBy         string         `json:"approved_by,omitempty"`
	Purpose            string         `json:"purpose,omitempty"`
	PrivilegeLevel     string         `json:"privilege_level,omitempty"`
	DataIngestion      string         `json:"data_ingestion,omitempty"`
	HostingEnv         string         `json:"hosting_env,omitempty"`
	HostingOtherDetail string         `json:"hosting_other_detail,omitempty"`
	ArchitectureURL    string         `json:"architecture_url,omitempty"`
	IntegrationURL     string         `json:"integration_endpoint,omitempty"`
	HomeTeamID         string         `json:"home_team_id,omitempty"`
	HomeChannelID      string         `json:"home_channel_id,omitempty"`
	Compliance         *botCompliance `json:"compliance,omitempty"`
}

// writeBotIndex records a bot created for req. Exercise bots are tracked
// in the exercise manifest instead. A failure is logged: the bot exists
// either way, and its history still links it to the request.
func (p *Plugin) writeBotIndex(req *botRequest, bot *model.Bot, approverID string) {
	if req.Exercise {
		return
	}
	ownerID := strings.TrimSpace(req.OwnerUserID)
	if ownerID == "" {
		ownerID = req.RequesterID
	}
	entry := botIndexEntry{
		BotUserID: bot.UserId, RequestID: req.ID, Username: bot.Username, DisplayName: req.DisplayName,
		RequesterID: req.RequesterID, OwnerUserID: ownerID,
		CreatedAt: model.GetMillis(), RequestedAt: req.CreatedAt, ApprovedBy: approverID,
		Purpose: req.Purpose, PrivilegeLevel: req.PrivilegeLevel, DataIngestion: req.DataIngestion,
		HostingEnv: req.HostingEnv, HostingOtherDetail: req.HostingOtherDetail,
		ArchitectureURL: req.ArchitectureURL, IntegrationURL: req.IntegrationURL,
		HomeTeamID: req.HomeTeamID, HomeChannelID: req.HomeChannelID,
		Compliance: req.Compliance,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		p.API.LogError("failed to marshal bot index entry", "bot_user_id", bot.UserId, "error", err.Error())
		return
	}
	if appErr := p.API.KVSet(kvBotIndexPrefix+bot.UserId, data); appErr != nil {
		p.API.LogError("failed to write bot index entry", "bot_user_id", bot.UserId, "request_id", req.ID, "error", appErr.Error())
	}
}

// loadBotIndex returns the index entry for a bot, or nil when Request Hub
// has none.
func (p *Plugin) loadBotIndex(botUserID string) (*botIndexEntry, error) {
	data, appErr := p.API.KVGet(kvBotIndexPrefix + botUserID)
	if appErr != nil {
		return nil, appErr
	}
	if data == nil {
		return nil, nil
	}
	var entry botIndexEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal bot index entry")
	}
	return &entry, nil
}

// listBotIndex returns every bot index entry. Unreadable entries are
// logged and skipped.
func (p *Plugin) listBotIndex() ([]botIndexEntry, error) {
	var out []botIndexEntry
	for page := 0; ; page++ {
		keys, appErr := p.API.KVList(page, botIndexPageSize)
		if appErr != nil {
			return nil, appErr
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, kvBotIndexPrefix) {
				continue
			}
			entry, err := p.loadBotIndex(strings.TrimPrefix(key, kvBotIndexPrefix))
			if err != nil {
				p.API.LogWarn("skipping unreadable bot index entry", "key", key, "error", err.Error())
				continue
			}
			if entry != nil {
				out = append(out, *entry)
			}
		}
		if len(keys) < botIndexPageSize {
			return out, nil
		}
	}
}
