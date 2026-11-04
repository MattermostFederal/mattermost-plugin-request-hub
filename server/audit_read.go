package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const (
	subcommandAudit = "audit"
	auditRecent     = "recent"
	auditExportSub  = "export"
	auditArgsHint   = "<request ID> | recent [count] | export [csv|json]"

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
	// A security approver who isn't a System Admin asks for the export,
	// for a System Admin to approve.
	if len(params) >= 1 && len(params) <= 2 && strings.EqualFold(params[0].Text, auditExportSub) {
		if user, appErr := p.API.GetUser(args.UserId); appErr == nil && user != nil && !user.IsSystemAdmin() {
			if _, canRequest := p.auditAccess(user); canRequest {
				format := "csv"
				if len(params) == 2 {
					format = strings.ToLower(params[1].Text)
				}
				msg, err := p.submitAuditExportRequest(user.Id, auditExportFilter{Format: format}, "")
				if err != nil {
					return ephemeralResponse("Could not send the request: " + escapeMarkdown(err.Error()))
				}
				return ephemeralResponse(msg)
			}
		}
	}
	if msg := p.systemAdminOnly(args.UserId, "/"+commandTrigger+" "+subcommandAudit); msg != "" {
		return ephemeralResponse(msg)
	}
	usage := fmt.Sprintf("Use `/%[1]s %[2]s <request ID>` for a request's history, `/%[1]s %[2]s %[3]s [count]` for the latest requests, or `/%[1]s %[2]s %[4]s [csv|json]` to download the whole audit history.",
		commandTrigger, subcommandAudit, auditRecent, auditExportSub)
	if len(params) == 0 {
		return ephemeralResponse(usage)
	}
	if strings.EqualFold(params[0].Text, auditExportSub) {
		if len(params) > 2 {
			return ephemeralResponse(usage)
		}
		format := ""
		if len(params) == 2 {
			format = strings.ToLower(params[1].Text)
			if format != "csv" && format != "json" {
				return ephemeralResponse(usage)
			}
		}
		return ephemeralResponse(exportLinksText(args, format))
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
	return ephemeralResponse(historyTimelineText(h) + "\n\n" + downloadLinks(args, "This request in full", "request="+h.RequestID))
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

// downloadLinks is a line with CSV and JSON download links for query.
func downloadLinks(args *model.CommandArgs, label, query string) string {
	sep := ""
	if query != "" {
		sep = "&"
	}
	return fmt.Sprintf("%s: [CSV](%s) or [JSON](%s).", label,
		exportURL(args, query+sep+"format=csv"), exportURL(args, query+sep+"format=json"))
}

// exportLinksText answers `audit export`: links to the whole history, in
// one format or both.
func exportLinksText(args *model.CommandArgs, format string) string {
	var b strings.Builder
	b.WriteString("**Download the whole audit history**\n\n")
	switch format {
	case "":
		b.WriteString(downloadLinks(args, "Every request, every event", ""))
	default:
		fmt.Fprintf(&b, "Every request, every event: [%s](%s).", strings.ToUpper(format), exportURL(args, "format="+format))
	}
	b.WriteString("\n\nThe file has every event with all its details, including the full text of ticket thread messages. " +
		"The CSV has one row per event; the JSON has one object per request, with its latest copy of the request. " +
		"Add `kind`, `requester` (a user ID), `since` or `until` (Unix milliseconds) to the link to narrow it. " +
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
	b.WriteString("\n" + downloadLinks(args, "Download everything", ""))
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

// auditExportFilter narrows an export. Zero values don't filter.
type auditExportFilter struct {
	Format    string `json:"format"` // "csv" or "json"
	RequestID string `json:"request_id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Requester string `json:"requester,omitempty"` // a user ID
	Since     int64  `json:"since,omitempty"`     // Unix millis
	Until     int64  `json:"until,omitempty"`
}

// detail is the filter as audit event detail.
func (f auditExportFilter) detail() map[string]any {
	return map[string]any{"format": f.Format, "request": f.RequestID, "kind": f.Kind, "requester": f.Requester, "since": f.Since, "until": f.Until}
}

// auditExport is a built export file.
type auditExport struct {
	data        []byte
	filename    string
	contentType string
	requests    int
}

// buildAuditExport builds the export file for f: JSON with one object per
// request and its latest copy, or CSV with one row per event and every
// detail.
func (p *Plugin) buildAuditExport(f auditExportFilter) (*auditExport, error) {
	histories, err := p.listHistories()
	if err != nil {
		return nil, err
	}
	filtered := make([]requestHistory, 0, len(histories))
	for _, h := range histories {
		if (f.RequestID != "" && h.RequestID != f.RequestID) || (f.Kind != "" && h.Kind != f.Kind) ||
			(f.Requester != "" && h.RequesterID != f.Requester) ||
			(f.Since > 0 && h.UpdatedAt < f.Since) || (f.Until > 0 && h.CreatedAt > f.Until) {
			continue
		}
		filtered = append(filtered, h)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedAt < filtered[j].CreatedAt })

	out := &auditExport{requests: len(filtered), filename: "request-hub-audit-" + time.Now().UTC().Format("20060102-150405")}
	var buf bytes.Buffer
	if f.Format == "json" {
		if err := json.NewEncoder(&buf).Encode(filtered); err != nil {
			return nil, err
		}
		out.data, out.filename, out.contentType = buf.Bytes(), out.filename+".json", "application/json"
		return out, nil
	}
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"request_id", "kind", "requester_id", "status", "time_utc", "event", "outcome", "actor_id", "actor_name", "note", "message", "details"})
	for _, h := range filtered {
		for _, ev := range h.Events {
			if (f.Since > 0 && ev.At < f.Since) || (f.Until > 0 && ev.At > f.Until) {
				continue
			}
			message, details := csvDetail(ev.Detail)
			_ = cw.Write([]string{
				h.RequestID, h.Kind, h.RequesterID, h.Status,
				time.UnixMilli(ev.At).UTC().Format(time.RFC3339),
				ev.Event, ev.Outcome, ev.ActorID, csvSafe(ev.ActorName), csvSafe(ev.Note), csvSafe(message), csvSafe(details),
			})
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	out.data, out.filename, out.contentType = buf.Bytes(), out.filename+".csv", "text/csv; charset=utf-8"
	return out, nil
}

// parseExportFilter reads an export filter from query parameters.
func parseExportFilter(q url.Values) (auditExportFilter, error) {
	f := auditExportFilter{Format: strings.ToLower(q.Get("format")), RequestID: q.Get("request"), Kind: q.Get("kind"), Requester: q.Get("requester")}
	if f.Format == "" {
		f.Format = "json"
	}
	if f.Format != "json" && f.Format != "csv" {
		return f, errors.New("format must be json or csv")
	}
	var sinceErr, untilErr error
	f.Since, sinceErr = parseMillisParam(q.Get("since"))
	f.Until, untilErr = parseMillisParam(q.Get("until"))
	if sinceErr != nil || untilErr != nil {
		return f, errors.New("since and until are Unix times in milliseconds")
	}
	return f, nil
}

// handleAuditExport serves the request histories as JSON or CSV, filtered
// by request, kind, requester and time range. System Admins only.
func (p *Plugin) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	user, ok := p.requireAuditReader(w, r)
	if !ok {
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
	detail := f.detail()
	detail["requests"] = export.requests
	p.recordEvent(auditEntry{
		Event: eventAuditExported, ActorID: user.Id, ActorName: user.Username, Detail: detail,
		Note: fmt.Sprintf("@%s exported the audit history of %d requests", user.Username, export.requests),
	})
	w.Header().Set("Content-Type", export.contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+export.filename+`"`)
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

// csvSafe keeps a spreadsheet from running a cell as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
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
