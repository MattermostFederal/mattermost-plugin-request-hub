package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The bot inventory lists the bots Request Hub created, from the bot index:
// what the requester declared, who requested and approved each bot, the
// compliance statement they accepted, and the bot's state and home names
// looked up as of the export. Bots made outside Request Hub, and those it
// created before it kept the index, are not listed.
//
// CSV matches the Security spreadsheet: one field per row, one bot per
// column. Recommended-extra fields appear only when the export is verbose.

const (
	botStatusActive      = "active"
	botStatusDeactivated = "deactivated"
	botStatusDeleted     = "deleted"
	botStatusUnknown     = "unknown"
)

// Security spreadsheet column names, in order. Keep in sync with
// docs/status-requests/03-implementation-progress.md.
var (
	securityCoreColumns = []string{
		"Request ID",
		"Bot name",
		"Bot ID",
		"Mattermost team",
		"Channel",
		"Purpose",
		"Authentication method",
		"Privilege level",
		"Data ingestion / capabilities",
		"Integration endpoint",
		"Hosting env",
		"Data transit security",
		"Activity logs",
	}
	securityVerboseColumns = []string{
		"Requester name",
		"Requester ID",
		"Owner name",
		"Owner ID",
		"Approver name(s)",
		"Approver ID(s)",
		"T&Cs acceptance",
	}
	// securityFieldNotes is the left-hand explanation for each Security row
	// (what that row means). Empty when the field name is enough.
	securityFieldNotes = map[string]string{
		"Request ID":                    "Request Hub request ID for this bot",
		"Bot name":                      "Mattermost bot username",
		"Bot ID":                        "Mattermost bot user ID",
		"Mattermost team":               "Home team display name",
		"Channel":                       "Home channel (handle), marked (home)",
		"Purpose":                       "What the requester declared the bot is for",
		"Authentication method":         "How the bot authenticates (token and/or webhooks)",
		"Privilege level":               "Declared Mattermost privilege for the bot",
		"Data ingestion / capabilities": "What data the bot posts or reads",
		"Integration endpoint":          "Declared HTTPS integration URL",
		"Hosting env":                   "Where the integration that drives this bot runs",
		"Data transit security":         "Whether the declared endpoint is HTTPS",
		"Activity logs":                 "Fleet flag (true/false); use a forensic export for one bot's posts",
		"Requester name":                "Username of who requested the bot",
		"Requester ID":                  "User ID of who requested the bot",
		"Owner name":                    "Current owner username (defaults to requester; may differ after transfer)",
		"Owner ID":                      "Current owner user ID",
		"Approver name(s)":              "Username(s) of who approved the bot",
		"Approver ID(s)":                "User ID(s) of who approved the bot",
		"T&Cs acceptance":               "Compliance statement hash and acceptance time",
	}
)

// inventoryRow is one bot in the inventory.
type inventoryRow struct {
	BotID                string `json:"bot_id"`
	BotUsername          string `json:"bot_username"`
	BotDisplayName       string `json:"bot_display_name"`
	BotStatus            string `json:"bot_status"`
	BotActive            bool   `json:"bot_active"`
	HomeTeamID           string `json:"home_team_id"`
	HomeTeamName         string `json:"home_team_name"`
	HomeChannelID        string `json:"home_channel_id"`
	HomeChannelName      string `json:"home_channel_name"`
	HomeChannelHandle    string `json:"home_channel_handle"`
	Purpose              string `json:"purpose"`
	PrivilegeLevel       string `json:"privilege_level"`
	DataIngestion        string `json:"data_ingestion"`
	HostingEnv           string `json:"hosting_env"`
	HostingOtherDetail   string `json:"hosting_other_detail,omitempty"`
	IntegrationURL       string `json:"integration_endpoint"`
	RequesterID          string `json:"requester_id"`
	RequesterUsername    string `json:"requester_username"`
	OwnerID              string `json:"owner_id"`
	OwnerUsername        string `json:"owner_username"`
	HasToken             bool   `json:"has_token"`
	HasIncomingWebhook   bool   `json:"has_incoming_webhook"`
	HasOutgoingWebhook   bool   `json:"has_outgoing_webhook"`
	WebhookOwner         string `json:"webhook_owner,omitempty"`
	RequestID            string `json:"request_id"`
	RequestedAt          int64  `json:"requested_at"`
	ApprovedAt           int64  `json:"approved_at"`
	ApprovedByID         string `json:"approved_by_id"`
	ApprovedByUsername   string `json:"approved_by_username"`
	ComplianceAccepted   bool   `json:"compliance_accepted"`
	ComplianceAcceptedAt int64  `json:"compliance_accepted_at"`
	ComplianceTextSHA256 string `json:"compliance_text_sha256"`
	RHManaged            bool   `json:"rh_managed"`
	AsOf                 int64  `json:"as_of"`
}

// inventoryLookups caches the users, teams and channels the inventory
// names, since many bots share them.
type inventoryLookups struct {
	p        *Plugin
	users    map[string]string
	teams    map[string]string
	channels map[string]channelLookup
}

type channelLookup struct {
	display string
	handle  string
}

func (l *inventoryLookups) username(id string) string {
	if id == "" {
		return ""
	}
	if name, ok := l.users[id]; ok {
		return name
	}
	name := ""
	if user, appErr := l.p.API.GetUser(id); appErr == nil && user != nil {
		name = user.Username
	}
	l.users[id] = name
	return name
}

func (l *inventoryLookups) teamName(id string) string {
	if id == "" {
		return ""
	}
	if name, ok := l.teams[id]; ok {
		return name
	}
	name := ""
	if team, appErr := l.p.API.GetTeam(id); appErr == nil && team != nil {
		name = team.DisplayName
	}
	l.teams[id] = name
	return name
}

func (l *inventoryLookups) channel(id string) channelLookup {
	if id == "" {
		return channelLookup{}
	}
	if got, ok := l.channels[id]; ok {
		return got
	}
	got := channelLookup{}
	if channel, appErr := l.p.API.GetChannel(id); appErr == nil && channel != nil {
		got.display = channel.DisplayName
		got.handle = channel.Name
	}
	l.channels[id] = got
	return got
}

// buildBotInventory lists the indexed bots f covers, oldest first: in f's
// hosting environment, requested by f's requester, approved between f's
// dates.
func (p *Plugin) buildBotInventory(f auditExportFilter, asOf int64) ([]inventoryRow, error) {
	entries, err := p.listBotIndex()
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt < entries[j].CreatedAt })
	look := &inventoryLookups{p: p, users: map[string]string{}, teams: map[string]string{}, channels: map[string]channelLookup{}}
	rows := make([]inventoryRow, 0, len(entries))
	for _, e := range entries {
		if (f.HostingEnv != "" && e.HostingEnv != f.HostingEnv) || (f.Requester != "" && e.RequesterID != f.Requester) ||
			(f.Since > 0 && e.CreatedAt < f.Since) || (f.Until > 0 && e.CreatedAt > f.Until) {
			continue
		}
		ownerID := strings.TrimSpace(e.OwnerUserID)
		if ownerID == "" {
			ownerID = e.RequesterID
		}
		ch := look.channel(e.HomeChannelID)
		row := inventoryRow{
			BotID: e.BotUserID, BotUsername: e.Username, BotDisplayName: e.DisplayName,
			HomeTeamID: e.HomeTeamID, HomeTeamName: look.teamName(e.HomeTeamID),
			HomeChannelID: e.HomeChannelID, HomeChannelName: ch.display, HomeChannelHandle: ch.handle,
			Purpose: e.Purpose, PrivilegeLevel: e.PrivilegeLevel, DataIngestion: e.DataIngestion,
			HostingEnv: e.HostingEnv, HostingOtherDetail: e.HostingOtherDetail, IntegrationURL: e.IntegrationURL,
			RequesterID: e.RequesterID, RequesterUsername: look.username(e.RequesterID),
			OwnerID: ownerID, OwnerUsername: look.username(ownerID),
			HasToken: e.HasToken, HasIncomingWebhook: e.HasIncomingWebhook, HasOutgoingWebhook: e.HasOutgoingWebhook,
			RequestID: e.RequestID, RequestedAt: e.RequestedAt, ApprovedAt: e.CreatedAt,
			ApprovedByID: e.ApprovedBy, ApprovedByUsername: look.username(e.ApprovedBy),
			RHManaged: true, AsOf: asOf,
		}
		if e.HasIncomingWebhook || e.HasOutgoingWebhook {
			row.WebhookOwner = inventoryWebhookOwner(row)
		}
		if c := e.Compliance; c != nil {
			row.ComplianceAccepted, row.ComplianceAcceptedAt, row.ComplianceTextSHA256 = true, c.AcceptedAt, c.TextSHA256
		}
		row.BotStatus = botStatusUnknown
		bot, appErr := p.API.GetBot(e.BotUserID, true)
		switch {
		case appErr == nil && bot != nil:
			row.BotUsername, row.BotDisplayName = bot.Username, bot.DisplayName
			row.BotStatus, row.BotActive = botStatusActive, bot.DeleteAt == 0
			if !row.BotActive {
				row.BotStatus = botStatusDeactivated
			}
			// Prefer the live Mattermost bot owner when present (as-of export).
			if id := strings.TrimSpace(bot.OwnerId); id != "" {
				row.OwnerID = id
				row.OwnerUsername = look.username(id)
			}
			if row.HasIncomingWebhook || row.HasOutgoingWebhook {
				row.WebhookOwner = inventoryWebhookOwner(row)
			}
		case isNotFound(appErr):
			row.BotStatus = botStatusDeleted
		default:
			p.API.LogWarn("failed to load a bot for the inventory", "bot_user_id", e.BotUserID, "error", appErr.Error())
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// buildInventoryExport builds the bot inventory export in f's format.
func (p *Plugin) buildInventoryExport(f auditExportFilter, filename string) (*auditExport, error) {
	now := time.Now()
	rows, err := p.buildBotInventory(f, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := &auditExport{count: len(rows), detail: map[string]any{"verbose": f.Verbose}}
	switch f.Format {
	case "json":
		data, err := inventoryJSON(rows, f.Verbose)
		if err != nil {
			return nil, err
		}
		out.data, out.filename, out.contentType = data, filename+".json", "application/json"
	case "csv":
		data, err := inventoryCSV(rows, f.Verbose)
		if err != nil {
			return nil, err
		}
		out.data, out.filename, out.contentType = data, filename+".csv", "text/csv; charset=utf-8"
	default:
		out.data, out.filename, out.contentType = p.inventoryPDF(f, rows, now), filename+".pdf", "application/pdf"
	}
	return out, nil
}

func securityColumns(verbose bool) []string {
	if !verbose {
		return securityCoreColumns
	}
	out := make([]string, 0, len(securityCoreColumns)+len(securityVerboseColumns))
	out = append(out, securityCoreColumns...)
	return append(out, securityVerboseColumns...)
}

// inventoryCSV writes the Security spreadsheet layout:
//
//	row 0:   Field | What this row is | <bot username> | <bot username> | ...
//	row 1+:  field name | explanation   | value          | value          | ...
//
// One bot per column (oldest first). Field names and explanations stay in
// the first two columns; adding a bot adds a column, never a row of bots.
func inventoryCSV(rows []inventoryRow, verbose bool) ([]byte, error) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	header := make([]string, 0, 2+len(rows))
	header = append(header, "Field", "What this row is")
	for _, r := range rows {
		name := r.BotUsername
		if name == "" {
			name = r.BotID
		}
		header = append(header, name)
	}
	_ = cw.Write(csvRecord(header...))
	for _, col := range securityColumns(verbose) {
		record := make([]string, 0, 2+len(rows))
		record = append(record, col, securityFieldNotes[col])
		for _, r := range rows {
			record = append(record, securityCell(r, col))
		}
		_ = cw.Write(csvRecord(record...))
	}
	cw.Flush()
	return buf.Bytes(), cw.Error()
}

// inventoryJSON encodes one object per bot with Security column keys.
func inventoryJSON(rows []inventoryRow, verbose bool) ([]byte, error) {
	cols := securityColumns(verbose)
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		obj := make(map[string]string, len(cols))
		for _, col := range cols {
			obj[col] = securityCell(r, col)
		}
		out = append(out, obj)
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func securityCell(r inventoryRow, col string) string {
	switch col {
	case "Request ID":
		return r.RequestID
	case "Bot name":
		return r.BotUsername
	case "Bot ID":
		return r.BotID
	case "Mattermost team":
		return r.HomeTeamName
	case "Channel":
		return inventoryChannelCell(r)
	case "Purpose":
		return r.Purpose
	case "Authentication method":
		return inventoryAuthMethod(r)
	case "Privilege level":
		return privilegeLabel(r.PrivilegeLevel)
	case "Data ingestion / capabilities":
		return r.DataIngestion
	case "Integration endpoint":
		return r.IntegrationURL
	case "Hosting env":
		return inventoryHostingCell(r)
	case "Data transit security":
		return inventoryTransitSecurity(r.IntegrationURL)
	case "Activity logs":
		return strconv.FormatBool(r.RHManaged)
	case "Requester name":
		return r.RequesterUsername
	case "Requester ID":
		return r.RequesterID
	case "Owner name":
		return r.OwnerUsername
	case "Owner ID":
		return r.OwnerID
	case "Approver name(s)":
		return r.ApprovedByUsername
	case "Approver ID(s)":
		return r.ApprovedByID
	case "T&Cs acceptance":
		return inventoryTCsAcceptance(r)
	default:
		return ""
	}
}

func inventoryChannelCell(r inventoryRow) string {
	if r.HomeChannelID == "" && r.HomeChannelHandle == "" && r.HomeChannelName == "" {
		return ""
	}
	switch {
	case r.HomeChannelHandle != "":
		return "~" + r.HomeChannelHandle + " (home)"
	case r.HomeChannelName != "":
		return r.HomeChannelName + " (home)"
	default:
		return r.HomeChannelID + " (home, not found)"
	}
}

func inventoryHostingCell(r inventoryRow) string {
	label := hostingEnvLabel(r.HostingEnv)
	if r.HostingOtherDetail != "" {
		if label == "" {
			return r.HostingOtherDetail
		}
		return label + " — " + r.HostingOtherDetail
	}
	return label
}

func inventoryTransitSecurity(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" {
		return ""
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "HTTPS required for declared URLs"
	}
	return "Declared URL is not HTTPS"
}

func inventoryAuthMethod(r inventoryRow) string {
	s := inventoryAuthSummary(r)
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func inventoryTCsAcceptance(r inventoryRow) string {
	if !r.ComplianceAccepted || r.ComplianceTextSHA256 == "" {
		return ""
	}
	at := rfc3339(r.ComplianceAcceptedAt)
	if at == "" {
		return "sha256:" + r.ComplianceTextSHA256
	}
	return "sha256:" + r.ComplianceTextSHA256 + " @ " + at
}

// inventoryPDF lays the inventory out for reading: a title, what it
// covers, and one block per bot using the Security field names.
func (p *Plugin) inventoryPDF(f auditExportFilter, rows []inventoryRow, now time.Time) []byte {
	doc := newPDF("Request Hub bot inventory", now)
	doc.text("Request Hub bot inventory", 18, true)
	doc.space(4)
	doc.text(fmt.Sprintf("Generated %s. %s. %d %s.", formatAuditTime(now.UnixMilli()), strings.TrimSuffix(p.describeExportFilter(f), "."),
		len(rows), plural(len(rows), "bot", "bots")), 10, false)
	doc.space(4)
	doc.text("Fields follow the Security spreadsheet. CSV puts one bot per column; this PDF lists one bot per block. "+
		"Purpose, hosting and the integration endpoint are what the requester declared. Privilege and data ingestion are the plugin's fixed policy for newer bots, and what the requester declared for older ones. None of them limit what the bot's token can do. "+
		"Bot status and home names are as of this export. Bots made outside Request Hub, and bots it created before it kept a bot index, are not listed.", 9, false)
	if len(rows) == 0 {
		doc.space(12)
		doc.text("No bots match.", 11, false)
	}
	for _, r := range rows {
		doc.space(10)
		doc.ensure(220)
		doc.rule()
		heading := "@" + r.BotUsername
		if r.BotDisplayName != "" {
			heading += " (" + r.BotDisplayName + ")"
		}
		doc.text(heading, 12, true)
		doc.space(2)
		for _, col := range securityColumns(f.Verbose) {
			doc.field(col, orBlank(securityCell(r, col), "—"), 9)
		}
		doc.field("Status", r.BotStatus, 9)
	}
	return doc.bytes()
}

func orBlank(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// inventoryWebhookOwner is "@bot (bot owned by @owner)" for bot-bundled hooks.
// Usernames only (IDs are already on the owner_id / bot_id columns).
func inventoryWebhookOwner(r inventoryRow) string {
	bot := "@" + r.BotUsername
	if r.BotUsername == "" {
		bot = r.BotID
	}
	owner := "@" + r.OwnerUsername
	if r.OwnerUsername == "" {
		owner = r.OwnerID
		if owner == "" {
			owner = "unknown"
		}
	}
	return fmt.Sprintf("%s (bot owned by %s)", bot, owner)
}

func inventoryAuthSummary(r inventoryRow) string {
	var parts []string
	if r.HasToken {
		parts = append(parts, "bot access token")
	}
	if r.HasIncomingWebhook {
		parts = append(parts, "incoming webhook")
	}
	if r.HasOutgoingWebhook {
		parts = append(parts, "outgoing webhook")
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
