package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const (
	subcommandAudit = "audit"
	auditRecent     = "recent"
	auditArgsHint   = "<request ID> | recent [count]"

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
	if msg := p.systemAdminOnly(args.UserId, "/"+commandTrigger+" "+subcommandAudit); msg != "" {
		return ephemeralResponse(msg)
	}
	usage := fmt.Sprintf("Use `/%s %s <request ID>` for a request's history, or `/%s %s %s [count]` for the latest requests.",
		commandTrigger, subcommandAudit, commandTrigger, subcommandAudit, auditRecent)
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
	return ephemeralResponse(historyTimelineText(h))
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
	location := "/plugins/" + manifest.Id + routeAuditExport
	if args != nil && args.SiteURL != "" {
		location = strings.TrimRight(args.SiteURL, "/") + location
	}
	fmt.Fprintf(&b, "\nDownload everything: [CSV](%s?format=csv) or [JSON](%s?format=json).", location, location)
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

// handleAuditExport serves every request history as JSON or CSV, filtered
// by kind, requester and time range. System Admins only.
func (p *Plugin) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, appErr := p.API.GetUser(userID)
	if appErr != nil || user == nil || !user.IsSystemAdmin() {
		http.Error(w, "only System Admins can export the audit history", http.StatusForbidden)
		return
	}

	q := r.URL.Query()
	format := strings.ToLower(q.Get("format"))
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		http.Error(w, "format must be json or csv", http.StatusBadRequest)
		return
	}
	since, sinceErr := parseMillisParam(q.Get("since"))
	until, untilErr := parseMillisParam(q.Get("until"))
	if sinceErr != nil || untilErr != nil {
		http.Error(w, "since and until are Unix times in milliseconds", http.StatusBadRequest)
		return
	}
	kind, requester := q.Get("kind"), q.Get("requester")

	histories, err := p.listHistories()
	if err != nil {
		p.API.LogError("failed to list request histories for export", "error", err.Error())
		http.Error(w, "could not load the audit history", http.StatusInternalServerError)
		return
	}
	filtered := make([]requestHistory, 0, len(histories))
	for _, h := range histories {
		if kind != "" && h.Kind != kind {
			continue
		}
		if requester != "" && h.RequesterID != requester {
			continue
		}
		if since > 0 && h.UpdatedAt < since {
			continue
		}
		if until > 0 && h.CreatedAt > until {
			continue
		}
		filtered = append(filtered, h)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedAt < filtered[j].CreatedAt })

	p.recordEvent(auditEntry{
		Event: eventAuditExported, ActorID: user.Id, ActorName: user.Username,
		Detail: map[string]any{"format": format, "kind": kind, "requester": requester, "since": since, "until": until, "requests": len(filtered)},
		Note:   fmt.Sprintf("@%s exported the audit history of %d requests", user.Username, len(filtered)),
	})

	stamp := time.Now().UTC().Format("20060102-150405")
	if format == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="request-hub-audit-`+stamp+`.json"`)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(filtered); err != nil {
			p.API.LogError("failed to write the audit export", "user_id", user.Id, "error", err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="request-hub-audit-`+stamp+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"request_id", "kind", "requester_id", "status", "time_utc", "event", "outcome", "actor_id", "actor_name", "note"})
	// Write errors are kept by the writer and checked once at the end.
	for _, h := range filtered {
		for _, ev := range h.Events {
			if (since > 0 && ev.At < since) || (until > 0 && ev.At > until) {
				continue
			}
			_ = cw.Write([]string{
				h.RequestID, h.Kind, h.RequesterID, h.Status,
				time.UnixMilli(ev.At).UTC().Format(time.RFC3339),
				ev.Event, ev.Outcome, ev.ActorID, csvSafe(ev.ActorName), csvSafe(ev.Note),
			})
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		p.API.LogError("failed to write the audit export", "user_id", user.Id, "error", err.Error())
	}
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
