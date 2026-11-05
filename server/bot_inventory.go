package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
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

const (
	botStatusActive      = "active"
	botStatusDeactivated = "deactivated"
	botStatusDeleted     = "deleted"
	botStatusUnknown     = "unknown"
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
	Purpose              string `json:"purpose"`
	PrivilegeLevel       string `json:"privilege_level"`
	DataIngestion        string `json:"data_ingestion"`
	HostingEnv           string `json:"hosting_env"`
	RequesterID          string `json:"requester_id"`
	RequesterUsername    string `json:"requester_username"`
	OwnerID              string `json:"owner_id"`
	OwnerUsername        string `json:"owner_username"`
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
	channels map[string]string
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

func (l *inventoryLookups) channelName(id string) string {
	if id == "" {
		return ""
	}
	if name, ok := l.channels[id]; ok {
		return name
	}
	name := ""
	if channel, appErr := l.p.API.GetChannel(id); appErr == nil && channel != nil {
		name = channel.DisplayName
	}
	l.channels[id] = name
	return name
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
	look := &inventoryLookups{p: p, users: map[string]string{}, teams: map[string]string{}, channels: map[string]string{}}
	rows := make([]inventoryRow, 0, len(entries))
	for _, e := range entries {
		if (f.HostingEnv != "" && e.HostingEnv != f.HostingEnv) || (f.Requester != "" && e.RequesterID != f.Requester) ||
			(f.Since > 0 && e.CreatedAt < f.Since) || (f.Until > 0 && e.CreatedAt > f.Until) {
			continue
		}
		row := inventoryRow{
			BotID: e.BotUserID, BotUsername: e.Username, BotDisplayName: e.DisplayName,
			HomeTeamID: e.HomeTeamID, HomeTeamName: look.teamName(e.HomeTeamID),
			HomeChannelID: e.HomeChannelID, HomeChannelName: look.channelName(e.HomeChannelID),
			Purpose: e.Purpose, PrivilegeLevel: e.PrivilegeLevel, DataIngestion: e.DataIngestion, HostingEnv: e.HostingEnv,
			RequesterID: e.RequesterID, RequesterUsername: look.username(e.RequesterID),
			OwnerID: e.OwnerUserID, OwnerUsername: look.username(e.OwnerUserID),
			RequestID: e.RequestID, RequestedAt: e.RequestedAt, ApprovedAt: e.CreatedAt,
			ApprovedByID: e.ApprovedBy, ApprovedByUsername: look.username(e.ApprovedBy),
			RHManaged: true, AsOf: asOf,
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
	out := &auditExport{count: len(rows)}
	switch f.Format {
	case "json":
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(rows); err != nil {
			return nil, err
		}
		out.data, out.filename, out.contentType = buf.Bytes(), filename+".json", "application/json"
	case "csv":
		data, err := inventoryCSV(rows)
		if err != nil {
			return nil, err
		}
		out.data, out.filename, out.contentType = data, filename+".csv", "text/csv; charset=utf-8"
	default:
		out.data, out.filename, out.contentType = p.inventoryPDF(f, rows, now), filename+".pdf", "application/pdf"
	}
	return out, nil
}

func inventoryCSV(rows []inventoryRow) ([]byte, error) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write(csvRecord("bot_id", "bot_username", "bot_display_name", "bot_status", "bot_active",
		"home_team_id", "home_team_name", "home_channel_id", "home_channel_name",
		"purpose", "privilege_level", "data_ingestion", "hosting_env",
		"requester_id", "requester_username", "owner_id", "owner_username",
		"request_id", "requested_at_utc", "approved_at_utc", "approved_by_id", "approved_by_username",
		"compliance_accepted", "compliance_accepted_at_utc", "compliance_text_sha256", "rh_managed", "as_of_utc"))
	for _, r := range rows {
		_ = cw.Write(csvRecord(r.BotID, r.BotUsername, r.BotDisplayName, r.BotStatus, strconv.FormatBool(r.BotActive),
			r.HomeTeamID, r.HomeTeamName, r.HomeChannelID, r.HomeChannelName,
			r.Purpose, r.PrivilegeLevel, r.DataIngestion, r.HostingEnv,
			r.RequesterID, r.RequesterUsername, r.OwnerID, r.OwnerUsername,
			r.RequestID, rfc3339(r.RequestedAt), rfc3339(r.ApprovedAt), r.ApprovedByID, r.ApprovedByUsername,
			strconv.FormatBool(r.ComplianceAccepted), rfc3339(r.ComplianceAcceptedAt), r.ComplianceTextSHA256,
			strconv.FormatBool(r.RHManaged), rfc3339(r.AsOf)))
	}
	cw.Flush()
	return buf.Bytes(), cw.Error()
}

// inventoryPDF lays the inventory out for reading: a title, what it
// covers, and one block per bot.
func (p *Plugin) inventoryPDF(f auditExportFilter, rows []inventoryRow, now time.Time) []byte {
	doc := newPDF("Request Hub bot inventory", now)
	doc.text("Request Hub bot inventory", 18, true)
	doc.space(4)
	doc.text(fmt.Sprintf("Generated %s. %s. %d %s.", formatAuditTime(now.UnixMilli()), strings.TrimSuffix(p.describeExportFilter(f), "."),
		len(rows), plural(len(rows), "bot", "bots")), 10, false)
	doc.space(4)
	doc.text("Purpose, privilege level, data ingestion and environment are what the requester declared; they do not limit what the bot's token can do. "+
		"Bot status and home names are as of this export. Bots made outside Request Hub, and bots it created before it kept a bot index, are not listed.", 9, false)
	if len(rows) == 0 {
		doc.space(12)
		doc.text("No bots match.", 11, false)
	}
	or := func(s, fallback string) string {
		if s == "" {
			return fallback
		}
		return s
	}
	for _, r := range rows {
		doc.space(10)
		// Keep a bot's block on one page when its usual dozen lines fit.
		doc.ensure(200)
		doc.rule()
		heading := "@" + r.BotUsername
		if r.BotDisplayName != "" {
			heading += " (" + r.BotDisplayName + ")"
		}
		doc.text(heading, 12, true)
		doc.space(2)
		doc.field("Bot ID", r.BotID, 9)
		doc.field("Status", r.BotStatus, 9)
		doc.field("Environment", or(r.HostingEnv, "not declared"), 9)
		doc.field("Privilege", or(privilegeLabel(r.PrivilegeLevel), "not declared"), 9)
		doc.field("Home", inventoryHome(r), 9)
		doc.field("Purpose", or(r.Purpose, "not declared"), 9)
		doc.field("Data ingestion", or(r.DataIngestion, "not declared"), 9)
		doc.field("Requested by", inventoryUser(r.RequesterUsername, r.RequesterID)+" on "+inventoryTime(r.RequestedAt), 9)
		if r.OwnerID != "" && r.OwnerID != r.RequesterID {
			doc.field("Owner", inventoryUser(r.OwnerUsername, r.OwnerID), 9)
		}
		doc.field("Approved by", inventoryUser(r.ApprovedByUsername, r.ApprovedByID)+" on "+inventoryTime(r.ApprovedAt), 9)
		compliance := "not recorded"
		if r.ComplianceAccepted {
			compliance = fmt.Sprintf("accepted on %s (statement sha256 %s)", inventoryTime(r.ComplianceAcceptedAt), r.ComplianceTextSHA256)
		}
		doc.field("Compliance", compliance, 9)
		doc.field("Request ID", r.RequestID, 9)
	}
	return doc.bytes()
}

func inventoryHome(r inventoryRow) string {
	if r.HomeTeamID == "" && r.HomeChannelID == "" {
		return "not declared"
	}
	name := func(display, id string) string {
		if display == "" {
			return id + " (not found)"
		}
		return display + " (" + id + ")"
	}
	return name(r.HomeTeamName, r.HomeTeamID) + " / " + name(r.HomeChannelName, r.HomeChannelID)
}

func inventoryUser(username, id string) string {
	switch {
	case id == "":
		return "unknown"
	case username == "":
		return id
	default:
		return "@" + username + " (" + id + ")"
	}
}

func inventoryTime(millis int64) string {
	if millis == 0 {
		return "an unknown date"
	}
	return formatAuditTime(millis)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
