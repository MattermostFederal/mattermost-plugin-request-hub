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
		"Requester",
		"Bot name",
		"Request ID",
		"Bot ID",
		"Approved",
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

	// Inventory CSV/JSON/PDF layout: column = Security sheet (one bot per
	// column); row = one bot per row (fields across).
	inventoryLayoutColumn = "column"
	inventoryLayoutRow    = "row"

	// Soft-wrap long cells in column-layout CSV so a long purpose doesn't
	// stretch one bot column across the sheet when opened in Excel.
	inventoryCSVWrapWidth = 36
	// Cap bot columns per PDF table page so wrapped cells stay readable.
	inventoryPDFBotsPerPage = 4
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
	layout := inventoryLayout(f)
	out := &auditExport{count: len(rows), detail: map[string]any{"verbose": f.Verbose, "layout": layout}}
	switch f.Format {
	case "json":
		data, err := inventoryJSON(rows, f.Verbose, layout)
		if err != nil {
			return nil, err
		}
		out.data, out.filename, out.contentType = data, filename+".json", "application/json"
	case "csv":
		data, err := inventoryCSV(rows, f.Verbose, layout)
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

// inventoryLayout is f's layout, defaulting to column (Security sheet).
func inventoryLayout(f auditExportFilter) string {
	switch strings.ToLower(strings.TrimSpace(f.Layout)) {
	case inventoryLayoutRow:
		return inventoryLayoutRow
	default:
		return inventoryLayoutColumn
	}
}

func inventoryBotLabel(r inventoryRow) string {
	if r.BotUsername != "" {
		return r.BotUsername
	}
	return r.BotID
}

// inventoryCSV writes the Security fields in f's layout:
//   - column (default): Field | bot | bot | …  (one bot per column)
//   - row: header = field names; one bot per data row
func inventoryCSV(rows []inventoryRow, verbose bool, layout string) ([]byte, error) {
	if layout == inventoryLayoutRow {
		return inventoryCSVByRow(rows, verbose)
	}
	return inventoryCSVByColumn(rows, verbose)
}

func inventoryCSVByColumn(rows []inventoryRow, verbose bool) ([]byte, error) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	header := make([]string, 0, 1+len(rows))
	header = append(header, "Field")
	for _, r := range rows {
		header = append(header, wrapInventoryCSVCell(inventoryBotLabel(r), inventoryCSVWrapWidth))
	}
	_ = cw.Write(csvRecord(header...))
	for _, col := range securityColumns(verbose) {
		record := make([]string, 0, 1+len(rows))
		record = append(record, wrapInventoryCSVCell(col, inventoryCSVWrapWidth))
		for _, r := range rows {
			record = append(record, wrapInventoryCSVCell(securityCell(r, col), inventoryCSVWrapWidth))
		}
		_ = cw.Write(csvRecord(record...))
	}
	cw.Flush()
	return buf.Bytes(), cw.Error()
}

// wrapInventoryCSVCell inserts newlines so spreadsheet apps wrap long
// values instead of widening a single bot column.
func wrapInventoryCSVCell(s string, width int) string {
	s = strings.TrimSpace(s)
	if width <= 0 || len(s) <= width {
		return s
	}
	var b strings.Builder
	for len(s) > 0 {
		if len(s) <= width {
			b.WriteString(s)
			break
		}
		chunk := s[:width]
		cut := strings.LastIndexAny(chunk, " \t-/_,;")
		if cut < width/3 {
			cut = width
		}
		b.WriteString(strings.TrimRight(s[:cut], " \t"))
		b.WriteByte('\n')
		s = strings.TrimLeft(s[cut:], " \t")
	}
	return b.String()
}

func inventoryCSVByRow(rows []inventoryRow, verbose bool) ([]byte, error) {
	cols := securityColumns(verbose)
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write(csvRecord(cols...))
	for _, r := range rows {
		record := make([]string, 0, len(cols))
		for _, col := range cols {
			record = append(record, securityCell(r, col))
		}
		_ = cw.Write(csvRecord(record...))
	}
	cw.Flush()
	return buf.Bytes(), cw.Error()
}

// inventoryJSON encodes the Security fields in f's layout (same shape as CSV).
func inventoryJSON(rows []inventoryRow, verbose bool, layout string) ([]byte, error) {
	cols := securityColumns(verbose)
	var payload any
	if layout == inventoryLayoutRow {
		out := make([]map[string]string, 0, len(rows))
		for _, r := range rows {
			obj := make(map[string]string, len(cols))
			for _, col := range cols {
				obj[col] = securityCell(r, col)
			}
			out = append(out, obj)
		}
		payload = out
	} else {
		out := make([]map[string]string, 0, len(cols))
		for _, col := range cols {
			obj := make(map[string]string, 1+len(rows))
			obj["Field"] = col
			for _, r := range rows {
				obj[inventoryBotLabel(r)] = securityCell(r, col)
			}
			out = append(out, obj)
		}
		payload = out
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func securityCell(r inventoryRow, col string) string {
	switch col {
	case "Requester":
		if r.RequesterUsername != "" {
			return r.RequesterUsername
		}
		return r.RequesterID
	case "Bot name":
		return r.BotUsername
	case "Request ID":
		return r.RequestID
	case "Bot ID":
		return r.BotID
	case "Approved":
		return inventoryApprovedCell(r)
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

// inventoryApprovedCell is yes when Request Hub recorded an approval for
// this bot (every indexed bot was approved to be created).
func inventoryApprovedCell(r inventoryRow) string {
	if r.ApprovedByID != "" || r.ApprovedAt > 0 {
		return "yes"
	}
	if r.RHManaged {
		return "yes"
	}
	return "no"
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

// inventoryPDF lays the inventory out for reading in f's layout.
func (p *Plugin) inventoryPDF(f auditExportFilter, rows []inventoryRow, now time.Time) []byte {
	layout := inventoryLayout(f)
	doc := newPDF("Request Hub bot inventory", now)
	doc.text("Request Hub bot inventory", 18, true)
	doc.space(4)
	doc.text(fmt.Sprintf("Generated %s. %s. %d %s.", formatAuditTime(now.UnixMilli()), strings.TrimSuffix(p.describeExportFilter(f), "."),
		len(rows), plural(len(rows), "bot", "bots")), 10, false)
	doc.space(4)
	if layout == inventoryLayoutColumn {
		doc.text("Column layout: field names down the left, one bot per column. Long values wrap inside each cell so a long purpose does not widen a column. "+
			"Purpose, hosting and the integration endpoint are what the requester declared. Privilege and data ingestion are the plugin's fixed policy for newer bots, and what the requester declared for older ones.", 9, false)
	} else {
		doc.text("Row layout: one bot per block with Security field names. "+
			"Purpose, hosting and the integration endpoint are what the requester declared. Privilege and data ingestion are the plugin's fixed policy for newer bots, and what the requester declared for older ones.", 9, false)
	}
	doc.text("Bot status and home names are as of this export. Bots made outside Request Hub, and bots it created before it kept a bot index, are not listed.", 9, false)
	if len(rows) == 0 {
		doc.space(12)
		doc.text("No bots match.", 11, false)
		return doc.bytes()
	}
	if layout == inventoryLayoutColumn {
		inventoryPDFByColumn(doc, rows, f.Verbose)
		return doc.bytes()
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

// inventoryPDFByColumn draws a wrapped table: Field | bot | bot | …
// Bots are chunked across pages so each cell keeps a readable width.
func inventoryPDFByColumn(doc *pdfDoc, rows []inventoryRow, verbose bool) {
	const size = 8.0
	fields := securityColumns(verbose)
	for start := 0; start < len(rows); start += inventoryPDFBotsPerPage {
		end := start + inventoryPDFBotsPerPage
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		cols := 1 + len(chunk)
		colW := pdfTextWidth / float64(cols)
		if start > 0 || len(rows) > inventoryPDFBotsPerPage {
			doc.space(10)
			doc.text(fmt.Sprintf("Bots %d–%d of %d", start+1, end, len(rows)), 10, true)
		}
		headers := make([]string, 0, cols)
		headers = append(headers, "Field")
		for _, r := range chunk {
			headers = append(headers, inventoryBotLabel(r))
		}
		doc.space(6)
		doc.tableRow(headers, colW, size, true)
		doc.rule()
		for _, field := range fields {
			cells := make([]string, 0, cols)
			cells = append(cells, field)
			for _, r := range chunk {
				cells = append(cells, orBlank(securityCell(r, field), "—"))
			}
			doc.tableRow(cells, colW, size, false)
		}
	}
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
