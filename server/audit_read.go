package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	subcommandAudit = "audit"
	auditRecent     = "recent"
	auditExportSub  = "export"
	auditArgsHint   = "<request ID> | recent [count] | export [type] [format] [--request ID] [--kind kind] [--env name] [--bot @name] [--requester @name] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--reason text]"

	defaultAuditRecent = 20
	maxAuditRecent     = 200
	// maxAuditTimeline caps the events one audit reply shows, and
	// maxTimelineNote and maxTimelineBytes its size, to stay well inside a
	// post; the export has them all in full.
	maxAuditTimeline = 100
	maxTimelineNote  = 300
	maxTimelineBytes = 12000
)

// executeAudit routes `/mattermost-request-hub audit`: a request's
// timeline by ID, or the most recently updated requests.
func (p *Plugin) executeAudit(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	if len(params) >= 1 && strings.EqualFold(params[0].Text, auditExportSub) {
		return p.executeAuditExport(args, params[1:])
	}
	if msg := p.systemAdminOnly(args.UserId, "/"+commandTrigger+" "+subcommandAudit); msg != "" {
		return ephemeralResponse(msg)
	}
	usage := fmt.Sprintf("Use `/%[1]s %[2]s <request ID>` for a request's history, `/%[1]s %[2]s %[3]s [count]` for the latest requests, or `/%[1]s %[2]s %[4]s` to export the audit history.",
		commandTrigger, subcommandAudit, auditRecent, auditExportSub)
	if len(params) == 0 {
		return ephemeralResponse(usage)
	}
	if strings.EqualFold(params[0].Text, auditRecent) {
		count := defaultAuditRecent
		if len(params) > 1 {
			n, err := strconv.Atoi(params[1].Text)
			if err != nil || n < 1 {
				return ephemeralResponse(usage)
			}
			count = min(n, maxAuditRecent)
		}
		return ephemeralResponse(p.recentAuditText(args, count))
	}
	if len(params) != 1 || !model.IsValidId(params[0].Text) {
		return ephemeralResponse(usage)
	}
	h, err := p.loadHistory(params[0].Text)
	if err != nil {
		p.API.LogError("failed to load request history", "request_id", params[0].Text, "error", err.Error())
		return ephemeralResponse("Could not load that request's history. Please try again.")
	}
	if h == nil {
		return ephemeralResponse("No history is recorded for that request ID.")
	}
	f := auditExportFilter{RequestID: h.RequestID}
	return ephemeralResponse(historyTimelineText(h) + "\n\n" + p.exportOffer(args, "This request in full", f))
}

// exportOffer is the line under an audit reply that offers its export:
// download links when direct export is on, or how to request it.
func (p *Plugin) exportOffer(args *model.CommandArgs, label string, f auditExportFilter) string {
	if p.getConfiguration().AllowDirectAuditExport {
		return downloadLinks(args, label, f)
	}
	request := ""
	if f.RequestID != "" {
		request = " --request " + f.RequestID
	}
	return fmt.Sprintf("Direct downloads are off. Run `/%s %s %s%s` to request a copy, which another System Admin approves.",
		commandTrigger, subcommandAudit, auditExportSub, request)
}

// executeAuditExport answers `audit export`: links to download it for a
// System Admin when direct export is on, or else a request for a copy
// from anyone allowed to ask.
func (p *Plugin) executeAuditExport(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	user, appErr := p.API.GetUser(args.UserId)
	if appErr != nil && !isNotFound(appErr) {
		p.API.LogWarn("failed to load the user running audit export", "user_id", args.UserId, "error", appErr.Error())
		return ephemeralResponse("Could not check your account. Please try again.")
	}
	direct := p.getConfiguration().AllowDirectAuditExport
	_, canRequest := p.auditAccess(user)
	isAdmin := appErr == nil && user != nil && user.IsSystemAdmin()
	if !canRequest && (!isAdmin || !direct) {
		return ephemeralResponse("Only System Admins and security approvers can run `/" + commandTrigger + " " + subcommandAudit + " " + auditExportSub + "`.")
	}
	f, reason, err := p.parseAuditExportArgs(params)
	if err != nil {
		return usageReply(err, subcommandAudit+" "+auditExportSub, auditExportArgsHint)
	}
	if isAdmin && direct {
		// No format links every format the type comes in.
		typeErr := checkExportType(f.withDefaults())
		if typeErr == nil {
			typeErr = checkExportScope(f)
		}
		if typeErr != nil {
			return usageReply(typeErr, subcommandAudit+" "+auditExportSub, auditExportArgsHint)
		}
		return ephemeralResponse(p.exportLinksText(args, f))
	}
	msg, err := p.submitAuditExportRequest(user.Id, f, reason)
	if err != nil {
		return ephemeralResponse("Could not send the request: " + escapeMarkdown(err.Error()))
	}
	return ephemeralResponse(msg)
}

// flagDay reads flag as a YYYY-MM-DD date, in Unix milliseconds at the
// start of that UTC day, or 0 when it wasn't given.
func flagDay(flags map[string][]string, flag string) (int64, error) {
	value, err := flagOne(flags, flag)
	if err != nil || value == "" {
		return 0, err
	}
	t, err := time.Parse(time.DateOnly, value)
	if err != nil {
		return 0, errors.Errorf("`%s` takes a date like 2026-01-31", flag)
	}
	return t.UnixMilli(), nil
}

const auditExportArgsHint = "[type] [format] [--request ID] [--kind kind] [--env name] [--bot @name] [--requester @name] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--reason text]"

// parseAuditExportArgs reads `audit export` arguments: an export type and
// a format in either order, then filters. The format is left empty when
// not given.
func (p *Plugin) parseAuditExportArgs(params []commandArg) (auditExportFilter, string, error) {
	var f auditExportFilter
	positional, flags, err := readArgs(params, "--request", "--kind", "--env", "--bot", "--requester", "--since", "--until", "--reason")
	if err != nil {
		return f, "", err
	}
	for _, word := range positional {
		word = strings.ToLower(word)
		switch {
		case f.Type == "" && isExportType(strings.ReplaceAll(word, "-", "_")):
			f.Type = strings.ReplaceAll(word, "-", "_")
		case f.Format == "" && slices.Contains(exportFormats, word):
			f.Format = word
		default:
			return f, "", errors.Errorf("%s is not an export type or format", inlineCode(word))
		}
	}
	if f.RequestID, err = flagOne(flags, "--request"); err != nil {
		return f, "", err
	}
	if f.Kind, err = flagOne(flags, "--kind"); err != nil {
		return f, "", err
	}
	if f.HostingEnv, err = flagOne(flags, "--env"); err != nil {
		return f, "", err
	}
	bot, err := flagOne(flags, "--bot")
	if err != nil {
		return f, "", err
	}
	if f.BotID, err = p.resolveExportBot(bot); err != nil {
		return f, "", err
	}
	requester, err := flagOne(flags, "--requester")
	if err != nil {
		return f, "", err
	}
	if name := strings.TrimPrefix(requester, "@"); name != "" {
		ids, resolveErr := p.resolveUsernameList([]string{name})
		if resolveErr != nil || len(ids) != 1 {
			return f, "", errors.Errorf("could not find a user named @%s", name)
		}
		f.Requester = ids[0]
	}
	// Dates are whole UTC days: since from the start of its day, until to
	// the end of its.
	if f.Since, err = flagDay(flags, "--since"); err != nil {
		return f, "", err
	}
	if f.Until, err = flagDay(flags, "--until"); err != nil {
		return f, "", err
	}
	if f.Until > 0 {
		f.Until += (24 * time.Hour).Milliseconds() - 1
	}
	reason, err := flagText(flags, "--reason", "a reason")
	if err != nil {
		return f, "", err
	}
	return f, reason, nil
}

// exportURL is the export endpoint with query, absolute when the server's
// site URL is known.
func exportURL(args *model.CommandArgs, query string) string {
	location := "/plugins/" + manifest.Id + routeAuditExport
	if args != nil && args.SiteURL != "" {
		location = strings.TrimRight(args.SiteURL, "/") + location
	}
	if query != "" {
		location += "?" + query
	}
	return location
}

// downloadLinks is a line with a download link for each format f's
// export type comes in, or only f.Format when it is set.
func downloadLinks(args *model.CommandArgs, label string, f auditExportFilter) string {
	formats := []string{f.Format}
	if f.Format == "" {
		et, _ := findExportType(f.exportType())
		formats = et.Formats
	}
	query := f.query()
	if query != "" {
		query += "&"
	}
	links := make([]string, 0, len(formats))
	for _, format := range formats {
		links = append(links, fmt.Sprintf("[%s](%s)", strings.ToUpper(format), exportURL(args, query+"format="+format)))
	}
	return label + ": " + joinOr(links) + "."
}

// joinOr joins items as "a", "a or b", or "a, b or c".
func joinOr(items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// exportLinksText answers `audit export` for a System Admin while direct
// export is on: links to download what f asks for.
func (p *Plugin) exportLinksText(args *model.CommandArgs, f auditExportFilter) string {
	et, _ := findExportType(f.exportType())
	var b strings.Builder
	fmt.Fprintf(&b, "**Download the audit export: %s**\n\n", et.Label)
	b.WriteString(downloadLinks(args, p.describeExportFilter(f), f))
	b.WriteString("\n\n" + et.About + " Add " + et.Narrow + " to the link to narrow it. " +
		"Each download is recorded in the server audit log.")
	return b.String()
}

// systemAdminOnly returns the refusal for a user who isn't a System Admin,
// or "" when they are. what names the command in the refusal.
func (p *Plugin) systemAdminOnly(userID, what string) string {
	user, appErr := p.API.GetUser(userID)
	if appErr != nil && !isNotFound(appErr) {
		p.API.LogWarn("failed to load the user running an admin command", "user_id", userID, "error", appErr.Error())
		return "Could not check your account. Please try again."
	}
	if appErr != nil || user == nil || !user.IsSystemAdmin() {
		return "Only System Admins can run `" + what + "`."
	}
	return ""
}

// historyTimelineText renders one request's history for the slash command.
func historyTimelineText(h *requestHistory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Audit history for %s request `%s`**\n", kindLabel(h.Kind), h.RequestID)
	fmt.Fprintf(&b, "Status: **%s**", h.Status)
	if h.Exercise {
		b.WriteString(" (exercise)")
	}
	if h.RequesterID != "" {
		fmt.Fprintf(&b, " · Requester ID: `%s`", h.RequesterID)
	}
	b.WriteString("\n\n")
	events := h.Events
	if len(events) > maxAuditTimeline {
		fmt.Fprintf(&b, "_Showing the last %d of %d events; the export has them all._\n", maxAuditTimeline, len(events))
		events = events[len(events)-maxAuditTimeline:]
	}
	for _, ev := range events {
		actor := "plugin"
		if ev.ActorID != "" {
			actor = userRef(ev.ActorID, ev.ActorName)
		}
		outcome := ""
		if ev.Outcome == model.AuditStatusFail {
			outcome = " ⚠️"
		}
		fmt.Fprintf(&b, "- %s · `%s`%s · %s", formatAuditTime(ev.At), ev.Event, outcome, actor)
		if ev.Note != "" {
			fmt.Fprintf(&b, ": %s", clip(oneLine(ev.Note), maxTimelineNote))
		}
		b.WriteString("\n")
		if b.Len() > maxTimelineBytes {
			b.WriteString("_The rest is cut short; the export has every event in full._\n")
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// recentAuditText lists the most recently updated requests.
func (p *Plugin) recentAuditText(args *model.CommandArgs, count int) string {
	histories, err := p.listHistories()
	if err != nil {
		p.API.LogError("failed to list request histories", "error", err.Error())
		return "Could not load the request histories. Please try again."
	}
	if len(histories) == 0 {
		return "No request history is recorded yet."
	}
	sort.Slice(histories, func(i, j int) bool { return histories[i].UpdatedAt > histories[j].UpdatedAt })
	var b strings.Builder
	fmt.Fprintf(&b, "**Latest %d of %d requests**\n\n", min(count, len(histories)), len(histories))
	for _, h := range histories[:min(count, len(histories))] {
		last := ""
		if n := len(h.Events); n > 0 {
			last = h.Events[n-1].Event
		}
		fmt.Fprintf(&b, "- `%s` · %s · **%s** · last event `%s` at %s\n", h.RequestID, kindLabel(h.Kind), h.Status, last, formatAuditTime(h.UpdatedAt))
	}
	b.WriteString("\n" + p.exportOffer(args, "Download everything", auditExportFilter{}))
	return b.String()
}

func formatAuditTime(millis int64) string {
	return time.UnixMilli(millis).UTC().Format("2006-01-02 15:04:05") + " UTC"
}

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// oneLine keeps a note on one list line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// requireAuditReader answers a request to read the audit history: only a
// GET from a System Admin goes on.
func (p *Plugin) requireAuditReader(w http.ResponseWriter, r *http.Request) (*model.User, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return nil, false
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}
	user, appErr := p.API.GetUser(userID)
	if appErr != nil || user == nil || !user.IsSystemAdmin() {
		http.Error(w, "only System Admins can read the audit history", http.StatusForbidden)
		return nil, false
	}
	return user, true
}

// Export types and formats.
const (
	exportFullHistories = "full_histories"
	exportBotInventory  = "bot_inventory"
	exportBotForensic   = "bot_forensic"
)

var exportFormats = []string{"csv", "json", "pdf"}

// auditExportType is one export the plugin builds and the formats it
// comes in, the default first.
type auditExportType struct {
	Type    string
	Label   string
	Formats []string
	// About says what the file holds, under download links and on the
	// approval card.
	About string
	// Unit counts what the file lists, such as "requests".
	Unit string
	// Narrow names the link parameters that filter it.
	Narrow string
}

// auditExportTypes are the exports the plugin builds.
var auditExportTypes = []auditExportType{
	{
		Type: exportFullHistories, Label: "Request histories", Formats: []string{"csv", "json"},
		About: "The file has every event with all its details, including the full text of ticket thread messages. " +
			"The CSV has one row per event; the JSON has one object per request, with its latest copy of the request.",
		Unit:   "requests",
		Narrow: "`kind`, `requester` (a user ID), `since` or `until` (Unix milliseconds)",
	},
	{
		Type: exportBotInventory, Label: "Bot inventory", Formats: []string{"pdf", "csv", "json"},
		About: "The file lists every bot Request Hub created: what its requester declared (purpose, privilege level, data ingestion, " +
			"hosting environment and home channel), who requested and approved it, the compliance statement they accepted, and whether " +
			"the bot is still active. Bots made outside Request Hub, or before it kept a bot index, are not listed.",
		Unit:   "bots",
		Narrow: "`env`, `requester` (a user ID), `since` or `until` (Unix milliseconds, on the approval date)",
	},
	{
		Type: exportBotForensic, Label: "Bot forensics", Formats: []string{"json"},
		About: fmt.Sprintf("The file traces one bot over at most %d days: its Request Hub request and history when Request Hub created it, "+
			"its bot record, the teams and channels it is in now, and the posts it made in those channels and in its home and webhook "+
			"channels, with their file details, up to %d posts or %d MB. Channels it has left, direct and group messages, and deleted "+
			"posts are not scanned, and calls made with its token are in the server audit log, not here.",
			forensicMaxDays, forensicMaxPosts, forensicMaxBytes>>20),
		Unit:   "posts",
		Narrow: "`since` and `until` (Unix milliseconds, at most 90 days apart)",
	},
}

func findExportType(t string) (auditExportType, bool) {
	for _, et := range auditExportTypes {
		if et.Type == t {
			return et, true
		}
	}
	return auditExportType{}, false
}

// exportLabel names f's export type.
func exportLabel(f auditExportFilter) string {
	et, _ := findExportType(f.exportType())
	return et.Label
}

func isExportType(t string) bool {
	_, ok := findExportType(t)
	return ok
}

// auditExportFilter narrows an export. Zero values don't filter.
type auditExportFilter struct {
	// Type is the export (see auditExportTypes); "" is full_histories, as
	// requests stored before there were types have it.
	Type      string `json:"type,omitempty"`
	Format    string `json:"format"` // one of the type's Formats
	RequestID string `json:"request_id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	// HostingEnv narrows the bot inventory to one hosting environment.
	HostingEnv string `json:"env,omitempty"`
	// BotID is the bot user a forensic export traces.
	BotID     string `json:"bot_id,omitempty"`
	Requester string `json:"requester,omitempty"` // a user ID
	Since     int64  `json:"since,omitempty"`     // Unix millis
	Until     int64  `json:"until,omitempty"`
}

// exportType is f's export type, with "" read as full_histories.
func (f auditExportFilter) exportType() string {
	if f.Type == "" {
		return exportFullHistories
	}
	return f.Type
}

// withDefaults normalises f: the type and format lowercased, and the
// type's default format when none is given.
func (f auditExportFilter) withDefaults() auditExportFilter {
	f.Type = strings.ToLower(strings.TrimSpace(f.Type))
	f.Format = strings.ToLower(strings.TrimSpace(f.Format))
	if f.Format == "" {
		if et, ok := findExportType(f.exportType()); ok {
			f.Format = et.Formats[0]
		}
	}
	return f
}

// query is f as export link parameters, without the format. The type is
// left out when it is the default.
func (f auditExportFilter) query() string {
	var parts []string
	add := func(key, value string) {
		if value != "" {
			parts = append(parts, key+"="+url.QueryEscape(value))
		}
	}
	if f.exportType() != exportFullHistories {
		add("type", f.Type)
	}
	add("request", f.RequestID)
	add("kind", f.Kind)
	add("env", f.HostingEnv)
	add("bot", f.BotID)
	add("requester", f.Requester)
	if f.Since > 0 {
		add("since", strconv.FormatInt(f.Since, 10))
	}
	if f.Until > 0 {
		add("until", strconv.FormatInt(f.Until, 10))
	}
	return strings.Join(parts, "&")
}

// detail is the filter as audit event detail.
func (f auditExportFilter) detail() map[string]any {
	return map[string]any{"type": f.exportType(), "format": f.Format, "request": f.RequestID, "kind": f.Kind, "env": f.HostingEnv, "bot_id": f.BotID, "requester": f.Requester, "since": f.Since, "until": f.Until}
}

// auditExport is a built export file.
type auditExport struct {
	data        []byte
	filename    string
	contentType string
	// count is how many requests, bots or posts the file lists.
	count int
	// detail is more for the audit event, such as whether the file was
	// cut short.
	detail map[string]any
}

// exportDetail is the audit event detail for an export built from f: the
// filter, how many of what it lists, and the export's own detail.
func exportDetail(f auditExportFilter, export *auditExport) map[string]any {
	detail := f.detail()
	detail[exportUnit(f)] = export.count
	maps.Copy(detail, export.detail)
	return detail
}

// exportUnit is what f's export counts, such as "bots".
func exportUnit(f auditExportFilter) string {
	et, _ := findExportType(f.exportType())
	return et.Unit
}

// exportCount says how many of what f's export lists, such as "3 bots".
func exportCount(f auditExportFilter, n int) string {
	unit := exportUnit(f)
	return fmt.Sprintf("%d %s", n, plural(n, strings.TrimSuffix(unit, "s"), unit))
}

// buildAuditExport builds the export file f asks for.
func (p *Plugin) buildAuditExport(f auditExportFilter) (*auditExport, error) {
	if err := checkExportType(f); err != nil {
		return nil, err
	}
	filename := "request-hub-" + strings.ReplaceAll(f.exportType(), "_", "-") + "-" + time.Now().UTC().Format("20060102-150405")
	switch f.exportType() {
	case exportFullHistories:
		return p.buildHistoryExport(f, filename)
	case exportBotInventory:
		return p.buildInventoryExport(f, filename)
	case exportBotForensic:
		return p.buildForensicExport(f, filename)
	}
	return nil, errors.New("unknown export type")
}

// buildHistoryExport builds the request histories export: JSON with one
// object per request and its latest copy, or CSV with one row per event
// and every detail. Caps histories/events so a full-store export cannot
// load the whole node into memory.
func (p *Plugin) buildHistoryExport(f auditExportFilter, filename string) (*auditExport, error) {
	filtered, histTruncated, err := p.listHistoriesForExport(f)
	if err != nil {
		return nil, err
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedAt < filtered[j].CreatedAt })

	out := &auditExport{count: len(filtered), filename: filename}
	var buf bytes.Buffer
	if f.Format == "json" {
		capped, eventsTruncated := capHistoriesEvents(filtered, f, historyExportMaxEvents)
		// Always encode a JSON array so consumers can parse histories the same
		// way whether or not the export was capped. Truncation is flagged in
		// export.detail (audit event + X-Request-Hub-Export-Truncated header).
		if err := json.NewEncoder(&buf).Encode(capped); err != nil {
			return nil, err
		}
		out.count = len(capped)
		if histTruncated || eventsTruncated {
			out.detail = map[string]any{
				"truncated":     true,
				"history_limit": historyExportMaxHistories,
				"event_limit":   historyExportMaxEvents,
				"histories_cut": histTruncated,
				"events_cut":    eventsTruncated,
			}
		}
		out.data, out.filename, out.contentType = buf.Bytes(), out.filename+".json", "application/json"
		return out, nil
	}
	cw := csv.NewWriter(&buf)
	_ = cw.Write(csvRecord("request_id", "kind", "requester_id", "status", "time_utc", "event", "outcome", "actor_id", "actor_name", "note", "message", "details"))
	events := 0
	eventsTruncated := false
eventLoop:
	for _, h := range filtered {
		for _, ev := range h.Events {
			if (f.Since > 0 && ev.At < f.Since) || (f.Until > 0 && ev.At > f.Until) {
				continue
			}
			// Cap reached: another eligible event proves truncation.
			if events >= historyExportMaxEvents {
				eventsTruncated = true
				break eventLoop
			}
			message, details := csvDetail(ev.Detail)
			_ = cw.Write(csvRecord(
				h.RequestID, h.Kind, h.RequesterID, h.Status,
				time.UnixMilli(ev.At).UTC().Format(time.RFC3339),
				ev.Event, ev.Outcome, ev.ActorID, ev.ActorName, ev.Note, message, details,
			))
			events++
		}
	}
	if histTruncated || eventsTruncated {
		_ = cw.Write(csvRecord("TRUNCATED", "", "", "", "", "", "", "", "",
			fmt.Sprintf("export capped at %d histories / %d events", historyExportMaxHistories, historyExportMaxEvents), "", ""))
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	out.data, out.filename, out.contentType = buf.Bytes(), out.filename+".csv", "text/csv; charset=utf-8"
	return out, nil
}

// capHistoriesEvents returns histories with at most maxEvents eligible events
// total (same since/until rules as the CSV export). eventsTruncated is true
// when a further eligible event existed beyond the cap.
func capHistoriesEvents(in []requestHistory, f auditExportFilter, maxEvents int) (out []requestHistory, eventsTruncated bool) {
	out = make([]requestHistory, 0, len(in))
	events := 0
	for _, h := range in {
		clipped := h
		clipped.Events = nil
		for _, ev := range h.Events {
			if (f.Since > 0 && ev.At < f.Since) || (f.Until > 0 && ev.At > f.Until) {
				continue
			}
			if events >= maxEvents {
				if len(clipped.Events) > 0 {
					out = append(out, clipped)
				}
				return out, true
			}
			clipped.Events = append(clipped.Events, ev)
			events++
		}
		out = append(out, clipped)
	}
	return out, false
}

// parseExportFilter reads an export filter from query parameters.
func parseExportFilter(q url.Values) (auditExportFilter, error) {
	f := auditExportFilter{Type: q.Get("type"), Format: q.Get("format"), RequestID: q.Get("request"), Kind: q.Get("kind"), HostingEnv: q.Get("env"), BotID: q.Get("bot"), Requester: q.Get("requester")}
	// The API has always given JSON when no format is named.
	if et, ok := findExportType(f.exportType()); ok && f.Format == "" && slices.Contains(et.Formats, "json") {
		f.Format = "json"
	}
	f = f.withDefaults()
	var sinceErr, untilErr error
	f.Since, sinceErr = parseMillisParam(q.Get("since"))
	f.Until, untilErr = parseMillisParam(q.Get("until"))
	if sinceErr != nil || untilErr != nil {
		return f, errors.New("since and until are Unix times in milliseconds")
	}
	if err := checkExportType(f); err != nil {
		return f, err
	}
	return f, checkExportScope(f)
}

// handleAuditExport serves an audit export, filtered by request, kind,
// requester and time range. System Admins only, and only while direct
// export is on.
func (p *Plugin) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	user, ok := p.requireAuditReader(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().AllowDirectAuditExport {
		http.Error(w, "Direct export is off. Request an export instead.", http.StatusForbidden)
		return
	}
	f, err := parseExportFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	export, err := p.buildAuditExport(f)
	if err != nil {
		p.API.LogError("failed to build the audit export", "error", err.Error())
		http.Error(w, "could not load the audit history", http.StatusInternalServerError)
		return
	}
	detail := exportDetail(f, export)
	p.recordEvent(auditEntry{
		Event: eventAuditExported, ActorID: user.Id, ActorName: user.Username, Detail: detail,
		Note: fmt.Sprintf("@%s downloaded the %s export (%s, %s)", user.Username, exportLabel(f), strings.ToUpper(f.Format), exportCount(f, export.count)),
	})
	w.Header().Set("Content-Type", export.contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+export.filename+`"`)
	if export.detail != nil {
		if truncated, _ := export.detail["truncated"].(bool); truncated {
			w.Header().Set("X-Request-Hub-Export-Truncated", "true")
		}
	}
	if _, err := w.Write(export.data); err != nil {
		p.API.LogError("failed to write the audit export", "user_id", user.Id, "error", err.Error())
	}
}

// csvDetail splits an event's details for the CSV: a relayed message's
// text in its own column, and everything else as JSON.
func csvDetail(detail map[string]any) (message, details string) {
	if len(detail) == 0 {
		return "", ""
	}
	rest := make(map[string]any, len(detail))
	for k, v := range detail {
		if k == "message" {
			message, _ = v.(string)
			continue
		}
		rest[k] = v
	}
	if len(rest) > 0 {
		if data, err := json.Marshal(rest); err == nil {
			details = string(data)
		}
	}
	return message, details
}

// csvRecord is one CSV row, every cell through csvSafe.
func csvRecord(cells ...string) []string {
	for i, c := range cells {
		cells[i] = csvSafe(c)
	}
	return cells
}

// csvSafe keeps a spreadsheet from running a cell as a formula: a cell that
// starts with a control character (tab, carriage return and so on), or
// whose first character past any spaces and control characters starts a
// formula, gets a leading apostrophe.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	if first, _ := utf8.DecodeRuneInString(s); unicode.IsControl(first) {
		return "'" + s
	}
	trimmed := strings.TrimLeftFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + s
	}
	return s
}

func parseMillisParam(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// auditRequestSummary is one row of the audit history list in the webapp.
type auditRequestSummary struct {
	RequestID         string `json:"request_id"`
	Kind              string `json:"kind"`
	KindLabel         string `json:"kind_label"`
	Title             string `json:"title"`
	Status            string `json:"status"`
	RequesterID       string `json:"requester_id"`
	RequesterUsername string `json:"requester_username,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
	EventCount        int    `json:"event_count"`
	LastEvent         string `json:"last_event"`
	Exercise          bool   `json:"exercise,omitempty"`
}

const (
	defaultAuditPageSize = 25
	maxAuditPageSize     = 100
)

// handleAuditRequests lists request histories for the webapp, newest
// activity first: filtered by kind, status and a search over the request
// ID, title and requester, a page at a time. System Admins only.
func (p *Plugin) handleAuditRequests(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.requireAuditReader(w, r); !ok {
		return
	}
	q := r.URL.Query()
	page, pageErr := strconv.Atoi(q.Get("page"))
	if q.Get("page") == "" {
		page, pageErr = 0, nil
	}
	perPage, perPageErr := strconv.Atoi(q.Get("per_page"))
	if q.Get("per_page") == "" {
		perPage, perPageErr = defaultAuditPageSize, nil
	}
	if pageErr != nil || perPageErr != nil || page < 0 || perPage < 1 {
		http.Error(w, "page and per_page must be whole numbers", http.StatusBadRequest)
		return
	}
	perPage = min(perPage, maxAuditPageSize)
	kind, status := q.Get("kind"), q.Get("status")
	search := strings.ToLower(strings.TrimSpace(q.Get("q")))

	histories, err := p.listHistories()
	if err != nil {
		p.API.LogError("failed to list request histories", "error", err.Error())
		http.Error(w, "could not load the audit history", http.StatusInternalServerError)
		return
	}
	names := map[string]string{}
	username := func(userID string) string {
		if userID == "" {
			return ""
		}
		if name, ok := names[userID]; ok {
			return name
		}
		name := ""
		if user, appErr := p.API.GetUser(userID); appErr == nil && user != nil {
			name = user.Username
		}
		names[userID] = name
		return name
	}

	rows := make([]auditRequestSummary, 0, len(histories))
	for _, h := range histories {
		if (kind != "" && h.Kind != kind) || (status != "" && h.Status != status) {
			continue
		}
		row := summarizeHistory(h)
		if search != "" && !strings.Contains(strings.ToLower(row.RequestID+" "+strings.Join(requestNames(h.Request), " ")+" "+row.RequesterID), search) &&
			!strings.Contains(strings.ToLower(username(row.RequesterID)), search) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt > rows[j].UpdatedAt })
	total := len(rows)
	start := min(page*perPage, total)
	rows = rows[start:min(start+perPage, total)]
	for i := range rows {
		rows[i].RequesterUsername = username(rows[i].RequesterID)
	}
	writeJSON(w, map[string]any{"total": total, "requests": rows})
}

// handleAuditRequest returns one request's whole history for the webapp,
// with the requester's username. System Admins only.
func (p *Plugin) handleAuditRequest(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.requireAuditReader(w, r); !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if !model.IsValidId(id) {
		http.Error(w, "id must be a request ID", http.StatusBadRequest)
		return
	}
	h, err := p.loadHistory(id)
	if err != nil {
		p.API.LogError("failed to load request history", "request_id", id, "error", err.Error())
		http.Error(w, "could not load the request's history", http.StatusInternalServerError)
		return
	}
	if h == nil {
		http.Error(w, "no history is recorded for that request", http.StatusNotFound)
		return
	}
	requester := ""
	if user, appErr := p.API.GetUser(h.RequesterID); h.RequesterID != "" && appErr == nil && user != nil {
		requester = user.Username
	}
	row := summarizeHistory(*h)
	writeJSON(w, map[string]any{"summary": row, "requester_username": requester, "history": h})
}

// summarizeHistory is h's row in the audit history list.
func summarizeHistory(h requestHistory) auditRequestSummary {
	row := auditRequestSummary{
		RequestID: h.RequestID, Kind: h.Kind, KindLabel: kindLabel(h.Kind), Title: requestTitle(h.Request),
		Status: h.Status, RequesterID: h.RequesterID, CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
		EventCount: len(h.Events), Exercise: h.Exercise,
	}
	if n := len(h.Events); n > 0 {
		row.LastEvent = h.Events[n-1].Event
	}
	return row
}

// requestTitle names a request from its stored copy: the channel, team,
// bot or webhook it asks for, or the guest's email.
func requestTitle(raw json.RawMessage) string {
	if names := requestNames(raw); len(names) > 0 {
		return names[0]
	}
	return ""
}

// requestNames lists every name a request's stored copy has, best first:
// display name, name, bot username, guest email. Search matches any.
func requestNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var r struct {
		DisplayName string `json:"display_name"`
		Name        string `json:"name"`
		Username    string `json:"username"`
		Email       string `json:"email"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil
	}
	var names []string
	for _, v := range []string{r.DisplayName, r.Name, r.Username, r.Email} {
		if v != "" {
			names = append(names, v)
		}
	}
	return names
}
