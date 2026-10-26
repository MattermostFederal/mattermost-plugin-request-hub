package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

const (
	routeDialog           = "/api/v1/dialog"
	routeCreate           = "/api/v1/create"
	routeApprove          = "/api/v1/approve"
	routeDeny             = "/api/v1/deny"
	routeRequestAdmin     = "/api/v1/request_admin" // request Channel Admin promotion on an existing channel
	routeApproveAdmin     = "/api/v1/approve_admin"
	routeDenyAdmin        = "/api/v1/deny_admin"
	routePrefixes         = "/api/v1/prefixes"
	routeTeams            = "/api/v1/teams"             // list teams for the approval-channel picker
	routeChannels         = "/api/v1/channels"          // list channels in a team, ?team_id=...
	routeUserAutocomplete = "/api/v1/user_autocomplete" // ?q=... for the request-modal member picker

	routeChannelAdminDialog = "/api/v1/channel_admin_dialog"
	routeTeamAdminDialog    = "/api/v1/team_admin_dialog"
	routeTeamDialog         = "/api/v1/team_dialog"
	routeApproveTeamAdmin   = "/api/v1/approve_team_admin"
	routeDenyTeamAdmin      = "/api/v1/deny_team_admin"
	routeApproveTeam        = "/api/v1/approve_team"
	routeDenyTeam           = "/api/v1/deny_team"
	routeTeamMemberDialog   = "/api/v1/team_member_dialog"
	routeApproveTeamMember  = "/api/v1/approve_team_member"
	routeDenyTeamMember     = "/api/v1/deny_team_member"
	routeTeamGuestDialog    = "/api/v1/team_guest_dialog"
	routeApproveTeamGuest   = "/api/v1/approve_team_guest"
	routeDenyTeamGuest      = "/api/v1/deny_team_guest"
	routeBotDialog          = "/api/v1/bot_dialog"
	routeApproveBot         = "/api/v1/approve_bot"
	routeDenyBot            = "/api/v1/deny_bot"

	routeIncomingWebhookDialog = "/api/v1/incoming_webhook_dialog"
	routeOutgoingWebhookDialog = "/api/v1/outgoing_webhook_dialog"
	routeApproveWebhook        = "/api/v1/approve_webhook"
	routeDenyWebhook           = "/api/v1/deny_webhook"

	// Webapp-driven REST endpoints — no triggerID required. Used by React
	// modals that collect form data and POST directly to the plugin.
	routeSubmitTeamCreationWebapp    = "/api/v1/submit_team_creation"
	routeSubmitTeamAdminWebapp       = "/api/v1/submit_team_admin_request"
	routeSubmitTeamMemberWebapp      = "/api/v1/submit_team_member_request"
	routeSubmitTeamGuestWebapp       = "/api/v1/submit_team_guest_request"
	routeSubmitBotWebapp             = "/api/v1/submit_bot_request"
	routeSubmitIncomingWebhookWebapp = "/api/v1/submit_incoming_webhook_request"
	routeSubmitOutgoingWebhookWebapp = "/api/v1/submit_outgoing_webhook_request"
	routeSidebarCategories           = "/api/v1/sidebar_categories" // ?team_id=... — list caller's sidebar categories
	routeConfig                      = "/api/v1/config"

	// fieldPrefix is the dialog element name for the domain-prefix
	// dropdown. Kept alongside the other field* constants in request.go.
	fieldPrefix = "prefix"

	channelRequestsDisabledMsg      = "Channel requests are currently disabled by an administrator."
	channelAdminRequestsDisabledMsg = "Channel Admin requests are currently disabled by an administrator."
	teamRequestsDisabledMsg         = "Team requests are currently disabled by an administrator."
	teamAdminRequestsDisabledMsg    = "Team Admin requests are currently disabled by an administrator."
	teamMemberRequestsDisabledMsg   = "Team member requests are currently disabled by an administrator."
	teamGuestRequestsDisabledMsg    = "Team guest requests are currently disabled by an administrator."
	botRequestsDisabledMsg          = "Bot account requests are currently disabled by an administrator."
	webhookRequestsDisabledMsg      = "Webhook requests are currently disabled by an administrator."

	// maxRequestBodyBytes bounds every request body. The largest legitimate
	// payload (two full member lists of usernames plus the text fields) is
	// a few KB.
	maxRequestBodyBytes = 64 << 10
)

func (p *Plugin) ServeHTTP(_ *plugin.Context, w http.ResponseWriter, r *http.Request) {
	// Same rationale as ExecuteCommand: a handler panic must not kill the plugin.
	defer func() {
		if rec := recover(); rec != nil {
			p.API.LogError("channel-requests: ServeHTTP panic recovered",
				"path", r.URL.Path,
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			// Action endpoints must return JSON or the client shows
			// "Action integration error" instead of a useful message.
			if isPostActionPath(r.URL.Path) {
				writeJSON(w, model.PostActionIntegrationResponse{
					EphemeralText: "Something went wrong processing that action. Please try again or ask a System Admin to check the logs.",
				})
				return
			}
			http.Error(w, "internal plugin error", http.StatusInternalServerError)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if p.serveMCPIfMatch(w, r) {
		return
	}

	switch r.URL.Path {
	case routeDialog:
		p.handleDialogSubmit(w, r)
	case routeCreate:
		p.handleWebappCreate(w, r)
	case routeApprove:
		p.handleAction(w, r, true)
	case routeDeny:
		p.handleAction(w, r, false)
	case routeRequestAdmin:
		p.handleRequestAdmin(w, r)
	case routeApproveAdmin:
		p.handleAdminAction(w, r, true)
	case routeDenyAdmin:
		p.handleAdminAction(w, r, false)
	case routePrefixes:
		p.handlePrefixes(w, r)
	case routeTeams:
		p.handleListTeams(w, r)
	case routeChannels:
		p.handleListChannels(w, r)
	case routeUserAutocomplete:
		p.handleUserAutocomplete(w, r)
	case routeChannelAdminDialog:
		p.handleChannelAdminDialogSubmit(w, r)
	case routeTeamAdminDialog:
		p.handleTeamAdminDialogSubmit(w, r)
	case routeTeamDialog:
		p.handleTeamCreationDialogSubmit(w, r)
	case routeApproveTeamAdmin:
		p.handleTeamAdminAction(w, r, true)
	case routeDenyTeamAdmin:
		p.handleTeamAdminAction(w, r, false)
	case routeApproveTeam:
		p.handleTeamAction(w, r, true)
	case routeDenyTeam:
		p.handleTeamAction(w, r, false)
	case routeTeamMemberDialog:
		p.handleTeamMemberDialogSubmit(w, r)
	case routeApproveTeamMember:
		p.handleTeamMemberAction(w, r, true)
	case routeDenyTeamMember:
		p.handleTeamMemberAction(w, r, false)
	case routeTeamGuestDialog:
		p.handleTeamGuestDialogSubmit(w, r)
	case routeApproveTeamGuest:
		p.handleTeamGuestAction(w, r, true)
	case routeDenyTeamGuest:
		p.handleTeamGuestAction(w, r, false)
	case routeBotDialog:
		p.handleBotDialogSubmit(w, r)
	case routeApproveBot:
		p.handleBotAction(w, r, true)
	case routeDenyBot:
		p.handleBotAction(w, r, false)
	case routeIncomingWebhookDialog:
		p.handleIncomingWebhookDialogSubmit(w, r)
	case routeOutgoingWebhookDialog:
		p.handleOutgoingWebhookDialogSubmit(w, r)
	case routeApproveWebhook:
		p.handleWebhookAction(w, r, true)
	case routeDenyWebhook:
		p.handleWebhookAction(w, r, false)
	case routeSubmitTeamCreationWebapp:
		p.handleSubmitTeamCreationWebapp(w, r)
	case routeSubmitTeamAdminWebapp:
		p.handleSubmitTeamAdminWebapp(w, r)
	case routeSubmitTeamMemberWebapp:
		p.handleSubmitTeamMemberWebapp(w, r)
	case routeSubmitTeamGuestWebapp:
		p.handleSubmitTeamGuestWebapp(w, r)
	case routeSubmitBotWebapp:
		p.handleSubmitBotWebapp(w, r)
	case routeSubmitIncomingWebhookWebapp:
		p.handleSubmitIncomingWebhookWebapp(w, r)
	case routeSubmitOutgoingWebhookWebapp:
		p.handleSubmitOutgoingWebhookWebapp(w, r)
	case routeConfig:
		p.handleConfig(w, r)
	case routeSidebarCategories:
		p.handleListSidebarCategories(w, r)
	default:
		http.NotFound(w, r)
	}
}

// requireUserID returns the authenticated caller's ID from the
// Mattermost-User-Id header, which the Mattermost server populates from
// the session — a client cannot forge it. Every handler that acts on
// behalf of a user MUST derive identity this way rather than trusting an
// ID in the request body. When the header is absent it writes a 401 and
// returns ("", false); callers should return immediately on !ok.
func requireUserID(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID := r.Header.Get("Mattermost-User-Id")
	if userID == "" {
		http.Error(w, "not authorized", http.StatusUnauthorized)
		return "", false
	}
	return userID, true
}

// requireActionUserID is like requireUserID but returns a PostActionIntegrationResponse
// JSON body on failure. Plain-text 401/400 from action handlers surfaces in the
// Mattermost UI as the opaque "Action integration error."
func requireActionUserID(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID := r.Header.Get("Mattermost-User-Id")
	if userID == "" {
		writeJSON(w, model.PostActionIntegrationResponse{
			EphemeralText: "Could not verify your identity to approve/deny. Please refresh and try again.",
		})
		return "", false
	}
	return userID, true
}

// isPostActionPath reports Approve/Deny (and similar) plugin routes that
// Mattermost invokes via DoPostAction — those must always respond with JSON.
func isPostActionPath(path string) bool {
	switch path {
	case routeApprove, routeDeny,
		routeApproveAdmin, routeDenyAdmin,
		routeApproveTeam, routeDenyTeam,
		routeApproveTeamAdmin, routeDenyTeamAdmin,
		routeApproveTeamMember, routeDenyTeamMember,
		routeApproveTeamGuest, routeDenyTeamGuest,
		routeApproveBot, routeDenyBot,
		routeApproveWebhook, routeDenyWebhook:
		return true
	default:
		return false
	}
}

// handleListTeams returns the list of teams the current user can see.
// Used by the ApprovalChannelPicker component to populate the team
// dropdown. Any logged-in user gets the list (only sysadmins reach the
// admin console anyway; MM enforces that at the UI level).
func (p *Plugin) handleListTeams(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	teams, appErr := p.API.GetTeamsForUser(userID)
	if appErr != nil {
		p.API.LogWarn("list-teams failed", "user_id", userID, "error", appErr.Error())
		http.Error(w, "failed to list teams", http.StatusInternalServerError)
		return
	}
	type teamDTO struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	out := make([]teamDTO, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamDTO{ID: t.Id, Name: t.Name, DisplayName: t.DisplayName})
	}
	writeJSON(w, out)
}

// handleListChannels returns the channels in a team the current user
// can see. Query param: team_id. Filters to public + private channels
// (excludes DMs/GMs — a DM channel isn't a valid approval destination).
func (p *Plugin) handleListChannels(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	teamID := r.URL.Query().Get("team_id")
	if teamID == "" {
		http.Error(w, "team_id required", http.StatusBadRequest)
		return
	}
	channels, appErr := p.API.GetChannelsForTeamForUser(teamID, userID, false)
	if appErr != nil {
		p.API.LogWarn("list-channels failed", "team_id", teamID, "user_id", userID, "error", appErr.Error())
		http.Error(w, "failed to list channels", http.StatusInternalServerError)
		return
	}
	type channelDTO struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
	}
	out := make([]channelDTO, 0, len(channels))
	for _, c := range channels {
		// Only public + private — DMs/GMs make no sense as an approval channel.
		if c.Type != model.ChannelTypeOpen && c.Type != model.ChannelTypePrivate {
			continue
		}
		out = append(out, channelDTO{
			ID:          c.Id,
			Name:        c.Name,
			DisplayName: c.DisplayName,
			Type:        string(c.Type),
		})
	}
	writeJSON(w, out)
}

// handleUserAutocomplete returns up to 20 users matching the query
// string. Used by the request modal's member picker. Proxies to MM's
// SearchUsers API.
//
// Scopes (exactly one required):
//   - ?team_id=...        → users who ARE members of that team
//     (channel create / Channel Admin nominate)
//   - ?not_in_team_id=... → users who are NOT members of that team
//     (team member add — pick people from elsewhere in the instance)
//
// The caller must be an active member of the referenced team either way,
// so SearchUsers (plugin-admin visibility) can't be used to enumerate
// teams the caller isn't on.
func (p *Plugin) handleUserAutocomplete(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, []any{})
		return
	}
	teamID := strings.TrimSpace(r.URL.Query().Get("team_id"))
	notInTeamID := strings.TrimSpace(r.URL.Query().Get("not_in_team_id"))

	if (teamID == "" && notInTeamID == "") || (teamID != "" && notInTeamID != "") {
		writeJSON(w, []any{})
		return
	}
	scopeTeamID := teamID
	if scopeTeamID == "" {
		scopeTeamID = notInTeamID
	}

	// GetTeamMember returns soft-deleted rows too, so a former member (who
	// left the team) would still pass a bare error check. Require an active
	// membership before scoping the search to this team.
	member, appErr := p.API.GetTeamMember(scopeTeamID, userID)
	if appErr != nil || member == nil || member.DeleteAt != 0 {
		writeJSON(w, []any{})
		return
	}

	// Guests get no results, and real names follow
	// PrivacySettings.ShowFullName.
	caller, appErr := p.API.GetUser(userID)
	if appErr != nil || caller == nil || caller.IsGuest() {
		writeJSON(w, []any{})
		return
	}
	showFullName := caller.IsSystemAdmin()
	if cfg := p.API.GetConfig(); cfg != nil && cfg.PrivacySettings.ShowFullName != nil && *cfg.PrivacySettings.ShowFullName {
		showFullName = true
	}

	// Callers who can't see every user only get people who share a team
	// with them.
	teammatesOnly := notInTeamID != "" && !p.API.HasPermissionTo(userID, model.PermissionViewMembers)

	// Limit MUST be non-zero — MM's UserSearch treats Limit=0 as
	// "return no results" (not "unlimited"). Set Limit to a
	// slightly-larger cap than our display cap of 20 so we still
	// have headroom after post-filtering already-selected users,
	// and more when results are kept to teammates.
	search := &model.UserSearch{
		Term:          q,
		AllowInactive: false,
		Limit:         50,
	}
	if teammatesOnly {
		search.Limit = 100
	}
	if teamID != "" {
		search.TeamId = teamID
	} else {
		search.NotInTeamId = notInTeamID
	}
	users, appErr := p.API.SearchUsers(search)
	if appErr != nil {
		p.API.LogWarn("user autocomplete failed", "q", q, "team_id", teamID, "not_in_team_id", notInTeamID, "error", appErr.Error())
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}

	type userDTO struct {
		ID        string `json:"id"`
		Username  string `json:"username"`
		Nickname  string `json:"nickname"`
		FirstName string `json:"first_name,omitempty"`
		LastName  string `json:"last_name,omitempty"`
	}
	if teammatesOnly {
		users = p.usersSharingATeam(userID, notInTeamID, users)
	}
	if len(users) > 20 {
		users = users[:20]
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		dto := userDTO{ID: u.Id, Username: u.Username, Nickname: u.Nickname}
		if showFullName {
			dto.FirstName = u.FirstName
			dto.LastName = u.LastName
		}
		out = append(out, dto)
	}
	writeJSON(w, out)
}

// usersSharingATeam keeps the users who are active members of at least one
// team the caller is in, other than excludeTeamID, for callers who may only
// see their teammates.
func (p *Plugin) usersSharingATeam(callerID, excludeTeamID string, users []*model.User) []*model.User {
	teams, appErr := p.API.GetTeamsForUser(callerID)
	if appErr != nil {
		p.API.LogWarn("failed to list the caller's teams", "user_id", callerID, "error", appErr.Error())
		return nil
	}
	callerTeams := make(map[string]bool, len(teams))
	for _, team := range teams {
		if team != nil && team.Id != excludeTeamID {
			callerTeams[team.Id] = true
		}
	}
	kept := make([]*model.User, 0, len(users))
	for _, u := range users {
		members, appErr := p.API.GetTeamMembersForUser(u.Id, 0, 200)
		if appErr != nil {
			p.API.LogWarn("failed to list a user's teams", "user_id", u.Id, "error", appErr.Error())
			continue
		}
		if slices.ContainsFunc(members, func(m *model.TeamMember) bool {
			return m != nil && m.DeleteAt == 0 && callerTeams[m.TeamId]
		}) {
			kept = append(kept, u)
		}
	}
	return kept
}

// handlePrefixes serves the current admin-configured prefix list so the
// webapp modal can populate its dropdown. Read-only, authenticated
// (any logged-in user can see it — same visibility as the plugin
// settings page shows anyway).
func (p *Plugin) handlePrefixes(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireUserID(w, r); !ok {
		return
	}
	config := p.getConfiguration()
	type prefixDTO struct {
		Prefix      string `json:"prefix"`
		Description string `json:"description"`
		SuffixRegex string `json:"suffix_regex"`
	}
	out := make([]prefixDTO, 0, len(config.prefixes))
	for _, p := range config.prefixes {
		out = append(out, prefixDTO{
			Prefix:      p.Prefix,
			Description: p.Description,
			SuffixRegex: p.SuffixPatternRaw,
		})
	}
	writeJSON(w, out)
}

// openRequestDialog opens the interactive channel request dialog for the
// slash command entry point. Callers must ensure a prefix list is
// configured (ExecuteCommand guards this) — the dialog always presents a
// "Domain prefix" dropdown and frames the URL field as a "URL suffix".
func (p *Plugin) openRequestDialog(triggerID, teamID string) error {
	config := p.getConfiguration()

	prefixOptions := make([]*model.PostActionOptions, 0, len(config.prefixes))
	for _, pf := range config.prefixes {
		label := pf.Prefix
		if pf.Description != "" {
			label = fmt.Sprintf("%s  (%s)", pf.Prefix, pf.Description)
		}
		prefixOptions = append(prefixOptions, &model.PostActionOptions{Text: label, Value: pf.Prefix})
	}

	elements := []model.DialogElement{
		{
			DisplayName: "Domain prefix",
			Name:        fieldPrefix,
			Type:        "select",
			Options:     prefixOptions,
			HelpText:    "Pick the category for this channel. The final URL is <prefix><suffix>.",
		},
		{
			DisplayName: "Channel name",
			Name:        fieldDisplayName,
			Type:        "text",
			Placeholder: "e.g. Marketing Team",
			MaxLength:   maxDisplayNameLen,
		},
		{
			DisplayName: "URL suffix",
			Name:        fieldName,
			Type:        "text",
			Optional:    true,
			HelpText:    "The part AFTER the prefix. Lowercase letters, numbers, and hyphens. Leave blank to generate from the channel name.",
			MaxLength:   64,
		},
		{
			DisplayName: "Purpose",
			Name:        fieldPurpose,
			Type:        "textarea",
			Optional:    true,
			MaxLength:   maxPurposeLen,
		},
		{
			DisplayName: "Visibility",
			Name:        fieldType,
			Type:        "radio",
			Default:     channelTypeOpen,
			Options: []*model.PostActionOptions{
				{Text: "Public", Value: channelTypeOpen},
				{Text: "Private", Value: channelTypePrivate},
			},
		},
		{
			DisplayName: "Sidebar category",
			Name:        fieldCategory,
			Type:        "text",
			Optional:    true,
			HelpText:    "Sidebar category for the new channel (you + members). Pick an existing name or type a new one — created on approval if missing. Leave blank to skip.",
			MaxLength:   64,
		},
		{
			DisplayName: "Members to add",
			Name:        fieldMembers,
			Type:        "select",
			DataSource:  "users",
			MultiSelect: true,
			Optional:    true,
			HelpText:    "These users are added to the channel once it's approved.",
		},
		{
			DisplayName: "Channel admins",
			Name:        fieldAdmins,
			Type:        "select",
			DataSource:  "users",
			MultiSelect: true,
			Optional:    true,
			HelpText:    "These users are made channel admins once the channel is approved.",
		},
	}

	dialog := model.Dialog{
		CallbackId:       dialogCallbackID,
		Title:            "Request a Channel",
		IntroductionText: "This request will be sent to an admin for approval.",
		SubmitLabel:      "Submit request",
		State:            teamID,
		Elements:         elements,
	}

	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}

func (p *Plugin) handleDialogSubmit(w http.ResponseWriter, r *http.Request) {
	// Identity comes from the authenticated header, not submission.UserId
	// in the body, so the request is filed under the user who sent it.
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeChannel) {
		writeJSON(w, model.SubmitDialogResponse{Error: channelRequestsDisabledMsg})
		return
	}

	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	teamID := submission.State
	if teamID == "" {
		teamID = submission.TeamId
	}

	in := requestInput{
		RequesterID: userID,
		TeamID:      teamID,
		DisplayName: submissionString(submission.Submission, fieldDisplayName),
		Name:        submissionString(submission.Submission, fieldName),
		// Prefix is present in submission only when the dialog was
		// opened with the prefix-list feature active.
		Prefix:         submissionString(submission.Submission, fieldPrefix),
		Purpose:        submissionString(submission.Submission, fieldPurpose),
		ChannelType:    submissionString(submission.Submission, fieldType),
		MemberIDs:      splitIDs(submissionString(submission.Submission, fieldMembers)),
		AdminMemberIDs: splitIDs(submissionString(submission.Submission, fieldAdmins)),
		CategoryName:   submissionString(submission.Submission, fieldCategory),
	}

	message, err := p.submitRequest(in)
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}

	// Acknowledge success with an ephemeral message in the channel the dialog was opened from.
	p.API.SendEphemeralPost(userID, &model.Post{
		ChannelId: submission.ChannelId,
		Message:   message,
	})
	w.WriteHeader(http.StatusOK)
}

// webappCreateRequest is the JSON payload sent by the webapp modal.
type webappCreateRequest struct {
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
	// Prefix is the selected domain prefix from the modal's dropdown
	// (e.g., "team-"). Required — the server rejects requests without a
	// configured prefix.
	Prefix       string   `json:"prefix"`
	Purpose      string   `json:"purpose"`
	ChannelType  string   `json:"channel_type"`
	Members      []string `json:"members"`       // usernames — regular members
	AdminMembers []string `json:"admin_members"` // usernames — Channel Admins
	// Category is the optional sidebar category name to place the new channel
	// in on approval. Matched case-insensitively against existing categories.
	Category string `json:"category"`
}

func (p *Plugin) handleWebappCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeChannel) {
		writeJSON(w, map[string]string{"error": channelRequestsDisabledMsg})
		return
	}

	var body webappCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	memberIDs, err := p.resolveUsernameList(body.Members)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	adminMemberIDs, err := p.resolveUsernameList(body.AdminMembers)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}

	message, err := p.submitRequest(requestInput{
		RequesterID:    userID,
		TeamID:         body.TeamID,
		DisplayName:    body.DisplayName,
		Name:           body.Name,
		Prefix:         body.Prefix,
		Purpose:        body.Purpose,
		ChannelType:    body.ChannelType,
		MemberIDs:      memberIDs,
		AdminMemberIDs: adminMemberIDs,
		CategoryName:   body.Category,
	})
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, map[string]string{"message": message})
}

// adminRequestBody is the JSON payload sent by the "Request Channel Admin"
// modal.
type adminRequestBody struct {
	ChannelID string   `json:"channel_id"`
	Nominees  []string `json:"nominees"` // usernames to promote to Channel Admin
}

func (p *Plugin) handleRequestAdmin(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeChannelAdmin) {
		writeJSON(w, map[string]string{"error": channelAdminRequestsDisabledMsg})
		return
	}

	var body adminRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	nomineeIDs, err := p.resolveUsernameList(body.Nominees)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}

	message, err := p.submitAdminRequest(userID, body.ChannelID, nomineeIDs)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, map[string]string{"message": message})
}

// decider approves or denies one request type on behalf of a user; see
// decideChannelRequest. The card buttons and the slash command share it.
type decider func(actingUserID, requestID, clickedPostID string, approve bool) decisionResult

// handleDecisionClick handles an Approve or Deny click on an approval card.
// The checks live in decide, which the slash command shares.
func (p *Plugin) handleDecisionClick(w http.ResponseWriter, r *http.Request, approve bool, decide decider) {
	// Identity comes from the authenticated header, NOT request.UserId in
	// the body — trusting the body would let anyone who can read a pending
	// request_id forge a System Admin user_id and approve/deny requests.
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}

	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		// Must return JSON — plain-text 400 becomes "Action integration error" in the client.
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}

	if request.PostId == "" {
		// The decide functions treat a missing post as the slash command.
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: buttonNotOnCardText})
		return
	}
	requestID, _ := request.Context[actionContextRequestID].(string)
	writeDecision(w, decide(userID, requestID, request.PostId, approve))
}

// handleAction handles Approve/Deny clicks on a channel request card.
func (p *Plugin) handleAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleDecisionClick(w, r, approve, p.decideChannelRequest)
}

// handleAdminAction handles Approve/Deny clicks on a Channel Admin request
// card.
func (p *Plugin) handleAdminAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleDecisionClick(w, r, approve, p.decideAdminRequest)
}

// handleTwoStepClick handles an Approve or Deny click on a bot or webhook
// request card. Unlike handleDecisionClick, a card whose buttons predate
// the request ID in the click context is still recognized by its buttons,
// and a refused Approve is also shown under the card.
func (p *Plugin) handleTwoStepClick(w http.ResponseWriter, r *http.Request, approve bool, decide decider) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}
	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}
	requestID := p.actionRequestID(&request)
	if requestID == "" {
		writeJSON(w, model.PostActionIntegrationResponse{
			EphemeralText: "Could not identify that request. Refresh the channel and try Approve again.",
		})
		return
	}
	if request.PostId == "" {
		// The decide functions treat a missing post as the slash command.
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: buttonNotOnCardText})
		return
	}
	d := decide(userID, requestID, request.PostId, approve)
	if d.Blocked {
		p.respondTwoStepApproveBlocked(w, userID, request.PostId, d.Reply, d.BannerHeader, d.Banner)
		return
	}
	writeDecision(w, d)
}

// writeDecision answers a card click: the private reply, and the clicked
// card resolved when the request was.
func writeDecision(w http.ResponseWriter, d decisionResult) {
	writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: d.Reply, Update: d.Update})
}

func (p *Plugin) handleChannelAdminDialogSubmit(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeChannelAdmin) {
		writeJSON(w, model.SubmitDialogResponse{Error: channelAdminRequestsDisabledMsg})
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	channelID := submissionString(submission.Submission, fieldChannelID)
	nomineeIDs := splitIDs(submissionString(submission.Submission, fieldNominees))

	message, err := p.submitAdminRequest(userID, channelID, nomineeIDs)
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	p.API.SendEphemeralPost(userID, &model.Post{
		ChannelId: submission.ChannelId,
		Message:   message,
	})
	w.WriteHeader(http.StatusOK)
}

func (p *Plugin) handleTeamAdminDialogSubmit(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeamAdmin) {
		writeJSON(w, model.SubmitDialogResponse{Error: teamAdminRequestsDisabledMsg})
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	teamID := submission.State
	if teamID == "" {
		teamID = submission.TeamId
	}
	nomineeIDs := splitIDs(submissionString(submission.Submission, fieldMembers))

	message, err := p.submitTeamAdminRequest(userID, teamID, nomineeIDs)
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	p.API.SendEphemeralPost(userID, &model.Post{
		ChannelId: submission.ChannelId,
		Message:   message,
	})
	w.WriteHeader(http.StatusOK)
}

func (p *Plugin) handleTeamCreationDialogSubmit(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeam) {
		writeJSON(w, model.SubmitDialogResponse{Error: teamRequestsDisabledMsg})
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	requestTeamAdmin := false
	if v, ok := submission.Submission[fieldRequestTeamAdmin].(bool); ok {
		requestTeamAdmin = v
	}

	message, err := p.submitTeamCreationRequest(teamCreationInput{
		RequesterID:      userID,
		DisplayName:      submissionString(submission.Submission, fieldDisplayName),
		Name:             submissionString(submission.Submission, fieldName),
		Description:      submissionString(submission.Submission, fieldPurpose),
		Type:             submissionString(submission.Submission, fieldType),
		RequestTeamAdmin: requestTeamAdmin,
	})
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	p.API.SendEphemeralPost(userID, &model.Post{
		ChannelId: submission.ChannelId,
		Message:   message,
	})
	w.WriteHeader(http.StatusOK)
}

// handleTeamAdminAction handles Approve/Deny on a Team Admin request card.
func (p *Plugin) handleTeamAdminAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleDecisionClick(w, r, approve, p.decideTeamAdminRequest)
}

func (p *Plugin) handleTeamMemberDialogSubmit(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeamMember) {
		writeJSON(w, model.SubmitDialogResponse{Error: teamMemberRequestsDisabledMsg})
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	teamID := submission.State
	if teamID == "" {
		teamID = submission.TeamId
	}
	nomineeIDs := submissionIDList(submission.Submission, fieldMembers)
	reason := submissionString(submission.Submission, "reason")

	message, err := p.submitTeamMemberRequest(userID, teamID, nomineeIDs, reason)
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	p.API.SendEphemeralPost(userID, &model.Post{
		ChannelId: submission.ChannelId,
		Message:   message,
	})
	w.WriteHeader(http.StatusOK)
}

// handleTeamMemberAction handles Approve/Deny on a team member request card.
func (p *Plugin) handleTeamMemberAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleDecisionClick(w, r, approve, p.decideTeamMemberRequest)
}

func (p *Plugin) handleTeamGuestDialogSubmit(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeamGuest) {
		writeJSON(w, model.SubmitDialogResponse{Error: teamGuestRequestsDisabledMsg})
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	teamID := submission.State
	if teamID == "" {
		teamID = submission.TeamId
	}
	email := submissionString(submission.Submission, "email")
	channelIDs := submissionIDList(submission.Submission, "guest_channels")
	reason := submissionString(submission.Submission, "reason")

	message, err := p.submitTeamGuestRequest(userID, teamID, email, channelIDs, reason)
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	p.API.SendEphemeralPost(userID, &model.Post{
		ChannelId: submission.ChannelId,
		Message:   message,
	})
	w.WriteHeader(http.StatusOK)
}

// handleTeamGuestAction handles Approve/Deny on a guest invite request card.
func (p *Plugin) handleTeamGuestAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleDecisionClick(w, r, approve, p.decideTeamGuestRequest)
}

// handleTeamAction handles Approve/Deny on a team creation request card.
func (p *Plugin) handleTeamAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleDecisionClick(w, r, approve, p.decideTeamRequest)
}

func (p *Plugin) handleBotDialogSubmit(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeBot) {
		writeJSON(w, model.SubmitDialogResponse{Error: botRequestsDisabledMsg})
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	message, err := p.submitBotRequest(userID, botRequestInput{
		Username:    submissionString(submission.Submission, fieldBotUsername),
		DisplayName: submissionString(submission.Submission, fieldBotDisplayName),
		Description: submissionString(submission.Submission, fieldBotDescription),
		OwnerUserID: submissionString(submission.Submission, fieldBotOwner),
	})
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	p.API.SendEphemeralPost(userID, &model.Post{ChannelId: submission.ChannelId, Message: message})
	w.WriteHeader(http.StatusOK)
}

// handleBotAction handles Approve/Deny on a bot account request card.
func (p *Plugin) handleBotAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleTwoStepClick(w, r, approve, p.decideBotRequest)
}

func (p *Plugin) handleIncomingWebhookDialogSubmit(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeWebhook) {
		writeJSON(w, model.SubmitDialogResponse{Error: webhookRequestsDisabledMsg})
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	message, err := p.submitWebhookRequest(
		userID,
		webhookTypeIncoming,
		submissionString(submission.Submission, fieldChannelID),
		submissionString(submission.Submission, fieldDisplayName),
		submissionString(submission.Submission, fieldPurpose),
		"",
	)
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	p.API.SendEphemeralPost(userID, &model.Post{ChannelId: submission.ChannelId, Message: message})
	w.WriteHeader(http.StatusOK)
}

func (p *Plugin) handleOutgoingWebhookDialogSubmit(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeWebhook) {
		writeJSON(w, model.SubmitDialogResponse{Error: webhookRequestsDisabledMsg})
		return
	}
	var submission model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if submission.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	message, err := p.submitWebhookRequest(
		userID,
		webhookTypeOutgoing,
		submissionString(submission.Submission, fieldChannelID),
		submissionString(submission.Submission, fieldDisplayName),
		submissionString(submission.Submission, fieldPurpose),
		submissionString(submission.Submission, fieldCallbackURL),
	)
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	p.API.SendEphemeralPost(userID, &model.Post{ChannelId: submission.ChannelId, Message: message})
	w.WriteHeader(http.StatusOK)
}

// handleWebhookAction handles Approve/Deny on a webhook request card.
func (p *Plugin) handleWebhookAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleTwoStepClick(w, r, approve, p.decideWebhookRequest)
}

// handleListSidebarCategories returns the caller's sidebar categories for a
// team. The webapp modal uses this to populate the "Sidebar category" dropdown.
// Query param: team_id (required).
func (p *Plugin) handleListSidebarCategories(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	teamID := r.URL.Query().Get("team_id")
	if teamID == "" {
		http.Error(w, "team_id required", http.StatusBadRequest)
		return
	}
	// Categories are per team; only list them for teams the caller is in.
	member, appErr := p.API.GetTeamMember(teamID, userID)
	if appErr != nil || member == nil || member.DeleteAt != 0 {
		writeJSON(w, []any{})
		return
	}
	cats, appErr := p.API.GetChannelSidebarCategories(userID, teamID)
	if appErr != nil {
		p.API.LogWarn("sidebar-categories list failed", "user_id", userID, "team_id", teamID, "error", appErr.Error())
		http.Error(w, "failed to list categories", http.StatusInternalServerError)
		return
	}
	type catDTO struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
	}
	// GetChannelSidebarCategories can return (nil, nil) on some server builds —
	// never dereference without a nil check (that panic was deactivating the plugin).
	if cats == nil {
		writeJSON(w, []catDTO{})
		return
	}
	out := make([]catDTO, 0, len(cats.Categories))
	for _, c := range cats.Categories {
		if c == nil {
			continue
		}
		out = append(out, catDTO{ID: c.Id, DisplayName: c.DisplayName, Type: string(c.Type)})
	}
	writeJSON(w, out)
}

// handleConfig serves per-type request toggles so the webapp can hide entry
// points for disabled types. Failures are fail-open on the client.
func (p *Plugin) handleConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireUserID(w, r); !ok {
		return
	}
	config := p.getConfiguration()
	writeJSON(w, map[string]bool{
		requestTypeChannel:      config.RequestEnabled(requestTypeChannel),
		requestTypeChannelAdmin: config.RequestEnabled(requestTypeChannelAdmin),
		requestTypeTeam:         config.RequestEnabled(requestTypeTeam),
		requestTypeTeamAdmin:    config.RequestEnabled(requestTypeTeamAdmin),
		requestTypeTeamMember:   config.RequestEnabled(requestTypeTeamMember),
		requestTypeTeamGuest:    config.RequestEnabled(requestTypeTeamGuest),
		requestTypeWebhook:      config.RequestEnabled(requestTypeWebhook),
		requestTypeBot:          config.RequestEnabled(requestTypeBot),
	})
}

// actionRequestID reads the pending request id from the button click context,
// falling back to the Approve/Deny integration context stored on the post.
// Some interactive Updates stripped click Context and left "Invalid action id"
// / no-op Approves on the system step — the post fallback recovers those.
func (p *Plugin) actionRequestID(request *model.PostActionIntegrationRequest) string {
	if request == nil {
		return ""
	}
	if id := contextString(request.Context, actionContextRequestID); id != "" {
		return id
	}
	if request.PostId == "" {
		return ""
	}
	post, appErr := p.API.GetPost(request.PostId)
	if appErr != nil || post == nil {
		return ""
	}
	return postRequestID(post)
}

// postRequestID returns the request ID carried by the buttons on a post's
// first attachment, or "" when there is none.
func postRequestID(post *model.Post) string {
	att := extractFirstAttachment(post)
	if att == nil {
		return ""
	}
	for _, a := range att.Actions {
		if a == nil || a.Integration == nil {
			continue
		}
		if id := contextString(a.Integration.Context, actionContextRequestID); id != "" {
			return id
		}
	}
	return ""
}

func contextString(ctx map[string]any, key string) string {
	if ctx == nil {
		return ""
	}
	switch v := ctx[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

// repaintedApprovalPost replaces the approval card's message and attachment
// while keeping the post identity (used after a partial two-step approval).
func (p *Plugin) repaintedApprovalPost(postID, message string, attachment *model.MessageAttachment) *model.Post {
	post, appErr := p.API.GetPost(postID)
	if appErr != nil {
		return &model.Post{Message: message}
	}
	post.Message = message
	post.DelProp("attachments")
	model.ParseMessageAttachment(post, []*model.MessageAttachment{attachment})
	return post
}

// twoStepApproveBlock returns the card banner (only for self-approve /
// security-first), the System "(Only visible to you)" channel message, and
// whether to paint the card. Other reject reasons stay ephemeral-only.
func twoStepApproveBlock(actingUsername, reason string) (cardTitle, cardNotice, ephemeral string, paintCard bool) {
	if actingUsername == "" {
		actingUsername = "someone"
	}
	switch {
	case strings.Contains(reason, "cannot approve it"):
		return "🚫 Requester cannot approve:",
			fmt.Sprintf("**@%s** (who created this ticket) clicked **Approve**. Requesters cannot approve their own request — another security or system approver must review it.", actingUsername),
			"🚫 You created this request — you cannot approve it. The ticket card now shows that reminder for everyone. Ask another approver to review it.",
			true
	case strings.Contains(reason, "Security review must happen first"),
		strings.Contains(reason, "security approver first"):
		return "🔒 Security review required first:",
			fmt.Sprintf("**@%s** clicked **Approve**. A **security approver** must sign off first — after security approves, a system approver can complete this ticket.", actingUsername),
			"🔒 Security review required first. A security approver has to click Approve before a system approver can finish this ticket. The ticket card now shows that reminder for everyone.",
			true
	default:
		return "", "", "⚠️ " + reason, false
	}
}

// respondTwoStepApproveBlocked paints the card banner when attachment is set,
// and always posts a System "(Only visible to you)" message in the approval
// channel for the clicker (same style for self-approve and security-first).
//
// Important: do NOT call UpdatePost here. Returning Update in the action
// response is how Mattermost persists the card; an extra UpdatePost double-
// writes the post and has broken Approve/Deny action IDs so later clicks
// (especially the system step) no longer apply.
func (p *Plugin) respondTwoStepApproveBlocked(w http.ResponseWriter, userID, postID, ephemeral, header string, attachment *model.MessageAttachment) {
	var update *model.Post
	if attachment != nil {
		update = p.repaintedApprovalPost(postID, header, attachment)
		if update != nil && update.Id == "" {
			// Never return an Update without a post id — Mattermost surfaces
			// that as "Invalid action id" and the buttons stop working.
			update = nil
		}
	}

	// Channel-level System ephemeral (no RootId / no bot UserId) so it appears
	// under the ticket in the channel as "System (Only visible to you)".
	if post, appErr := p.API.GetPost(postID); appErr == nil && post != nil && post.ChannelId != "" {
		p.API.SendEphemeralPost(userID, &model.Post{
			ChannelId: post.ChannelId,
			Message:   ephemeral,
		})
	}

	resp := model.PostActionIntegrationResponse{EphemeralText: ephemeral}
	if update != nil {
		resp.Update = update
	}
	writeJSON(w, resp)
}

// teamDisplayRef is the team's display name in bold, or its ID when the
// team couldn't be loaded.
func teamDisplayRef(team *model.Team, teamErr *model.AppError, teamID string) string {
	if teamErr != nil || team == nil {
		return teamID
	}
	return "**" + escapeMarkdown(team.DisplayName) + "**"
}

// expiredText is the private reply to a click on an expired request.
const expiredText = "This request has expired."

// alreadyHandledText is the private reply to a click on a request that no
// longer exists (approved, denied, or claimed by a concurrent click).
const alreadyHandledText = "This request has already been handled."

// ownRequestText is the private reply when a requester who isn't a System
// Admin clicks Approve on their own request.
const ownRequestText = "You can't approve your own request. Another approver has to review it."

// claimLostPendingText is the private reply to a click that lost the claim
// on a request that is still pending.
const claimLostPendingText = "This request was just updated. Please click Approve or Deny again."

// resolvedPost soft-squashes a finished approval card into a one-line
// tombstone: strips Approve/Deny buttons and all attachments, keeps the post
// ID (so DM ticket threads still resolve), and sets Message to status.
// Before stripping, the original request card is copied into a thread reply
// (no action buttons) so reviewers can expand the thread and recover every
// field after close. If that archive reply fails, attachments are retained
// (actions stripped) so request details are not lost. Callers pass a short
// outcome like "✅ Approved by @bob for @alice. Channel ~ops created."
//
// Only posts the bot authored are rebuilt; anything else gets a bare status
// post.
func (p *Plugin) resolvedPost(postID, status string) *model.Post {
	if postID == "" {
		return &model.Post{Message: status}
	}
	post, appErr := p.API.GetPost(postID)
	if appErr != nil || post == nil || post.UserId != p.botUserID {
		return &model.Post{Message: status}
	}
	return p.resolvedCard(post, status)
}

// resolvedCard is resolvedPost for an approval card the bot authored that
// is already loaded.
func (p *Plugin) resolvedCard(post *model.Post, status string) *model.Post {
	if err := p.archiveApprovalDetailsInThread(post); err != nil {
		post.Message = status
		if att := extractFirstAttachment(post); att != nil {
			kept := *att
			kept.Actions = nil
			model.ParseMessageAttachment(post, []*model.MessageAttachment{&kept})
		}
		return post
	}
	post.Message = status
	post.DelProp("attachments")
	return post
}

// archiveApprovalDetailsInThread posts a reply under the approval card that
// preserves the original attachment fields (actions stripped). No-ops when the
// post has already been squashed or has no attachments. Returns an error when
// the archive reply cannot be created so callers can retain the attachment.
func (p *Plugin) archiveApprovalDetailsInThread(post *model.Post) error {
	if post == nil || post.Id == "" || post.ChannelId == "" {
		return nil
	}
	original := extractFirstAttachment(post)
	if original == nil {
		return nil
	}
	archived := *original
	archived.Actions = nil
	if archived.Title == "" {
		archived.Title = "Original request"
	} else if !strings.HasPrefix(archived.Title, "Original request") {
		archived.Title = "Original request — " + archived.Title
	}

	reply := &model.Post{
		UserId:    p.botUserID,
		ChannelId: post.ChannelId,
		RootId:    post.Id,
		Message:   "Original request details (preserved on close):",
	}
	model.ParseMessageAttachment(reply, []*model.MessageAttachment{&archived})
	if _, appErr := p.API.CreatePost(reply); appErr != nil {
		p.API.LogWarn("failed to archive approval details into thread",
			"post_id", post.Id, "error", appErr.Error())
		return appErr
	}
	return nil
}

// resolveUsernameList takes a list of "@alice"/"alice"-style usernames
// and resolves them to MM user IDs. Returns an error for the first
// name that doesn't resolve so the requester sees a clear "unknown
// user: X" instead of a silent partial add.
func (p *Plugin) resolveUsernameList(usernames []string) ([]string, error) {
	// Bound the list before the per-username lookups so a crafted request
	// can't fan out into thousands of synchronous GetUserByUsername calls.
	if len(usernames) > maxMembersPerList {
		return nil, errors.Errorf("too many users: at most %d per list", maxMembersPerList)
	}
	out := make([]string, 0, len(usernames))
	for _, username := range usernames {
		username = strings.TrimPrefix(strings.TrimSpace(username), "@")
		if username == "" {
			continue
		}
		user, appErr := p.API.GetUserByUsername(username)
		if appErr != nil {
			return nil, errors.Errorf("unknown user: %s", username)
		}
		out = append(out, user.Id)
	}
	return out, nil
}

// canApprove reports whether the acting user is allowed to approve or
// deny a channel request. Cascading policy:
//
//	System Admin                                       -> yes (always)
//	Team Admin  of the approval team + opt-in flag on  -> yes
//	everyone else                                      -> no
//
// Channel admins are NOT approvers of channel-CREATION requests by
// design: the channel-admin role only exists on channels that already
// exist, whereas channel creation is about NEW channels. For
// channel-ADMIN requests (promotions on an existing channel) that
// reasoning is reversed — see canApproveAdminRequest.
func (p *Plugin) canApprove(user *model.User) bool {
	if user == nil {
		return false
	}
	if user.IsSystemAdmin() {
		return true
	}
	config := p.getConfiguration()
	if config.AllowTeamAdminApprovers && p.isApprovalTeamAdmin(user.Id) {
		return true
	}
	return false
}

// canApproveChannelRequest reports whether the acting user may Approve/Deny a
// channel-*creation* request for teamID. Extends canApprove so Team Admins of
// the team the channel will live in can act on per-team approval cards
// (mattermost-channel-requests in that team). Without this, PerTeamApprovalChannels
// posts cards Team Admins can see but never successfully approve.
func (p *Plugin) canApproveChannelRequest(user *model.User, teamID string) bool {
	if p.canApprove(user) {
		return true
	}
	if user == nil || teamID == "" {
		return false
	}
	return p.isTeamAdminOfTeamID(user.Id, teamID)
}

// canApproveAdminRequest reports whether the acting user may approve or deny a
// Channel Admin request for the given channel: System Admins, and the Team
// Admins of the TARGET channel's team. Approval-team Team Admins
// (AllowTeamAdminApprovers) are not included: a Team Admin's authority stops
// at their own team.
// postAdminApprovalRequest mentions exactly this set when it posts.
func (p *Plugin) canApproveAdminRequest(user *model.User, channelID string) bool {
	if user == nil {
		return false
	}
	if user.IsSystemAdmin() {
		return true
	}
	if channelID == "" {
		return false
	}
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil || channel == nil {
		return false
	}
	return p.isTeamAdminOfTeamID(user.Id, channel.TeamId)
}

// canApproveTeamRequest reports whether the acting user may approve or deny a
// team creation or Team Admin request: System Admins, and Team Admins of the
// approval team when AllowApprovalTeamAdminsForTeamRequests is on. A target
// team's own Team Admins are deliberately not included: the plugin promotes
// through the plugin API, which skips permission checks, so granting them this
// would bypass a System Scheme that removed "Manage team roles" from Team
// Admins. teamScopeApprovalHeader mentions this set.
func (p *Plugin) canApproveTeamRequest(user *model.User) bool {
	if user == nil {
		return false
	}
	if user.IsSystemAdmin() {
		return true
	}
	return p.getConfiguration().AllowApprovalTeamAdminsForTeamRequests && p.isApprovalTeamAdmin(user.Id)
}

// isTeamAdminOfTeamID reports whether the user is a Team Admin of the team
// identified by teamID (e.g. a channel's TeamId). isApprovalTeamAdmin uses it
// for the team approvalTeam resolves.
func (p *Plugin) isTeamAdminOfTeamID(userID, teamID string) bool {
	if teamID == "" {
		return false
	}
	member, appErr := p.API.GetTeamMember(teamID, userID)
	if appErr != nil || member == nil || member.DeleteAt != 0 {
		return false
	}
	if slices.Contains(strings.Fields(member.Roles), model.TeamAdminRoleId) {
		return true
	}
	return member.SchemeAdmin
}

// isApprovalTeamAdmin reports whether the user is a Team Admin of the
// configured Approval team. Handles both classic roles-string admin and
// scheme-based admin (permissions v2).
func (p *Plugin) isApprovalTeamAdmin(userID string) bool {
	team, err := p.approvalTeam()
	if err != nil || team == nil {
		return false
	}
	return p.isTeamAdminOfTeamID(userID, team.Id)
}

// requesterUsername returns "@username" for the audit log when we have
// the user object; falls back to the raw ID otherwise.
func requesterUsername(user *model.User, userID string) string {
	if user != nil {
		return "@" + user.Username
	}
	return "user " + userID
}

func submissionString(submission map[string]any, key string) string {
	if value, ok := submission[key].(string); ok {
		return value
	}
	return ""
}

// submissionIDList reads a dialog multi-select (comma string or []any of strings).
func submissionIDList(submission map[string]any, key string) []string {
	if submission == nil {
		return nil
	}
	switch v := submission[key].(type) {
	case string:
		return splitIDs(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
		return out
	case []string:
		return dedupeNonEmpty(v)
	default:
		return nil
	}
}

func splitIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			ids = append(ids, trimmed)
		}
	}
	return ids
}

// handleSubmitTeamCreationWebapp handles team creation requests submitted from
// the plugin's React modal (no triggerID needed — identity comes from the
// Mattermost-User-Id header). The same submitTeamCreationRequest path is used
// for slash commands and interactive dialogs; this endpoint is the webapp-only
// entry point that skips the dialog machinery.
func (p *Plugin) handleSubmitTeamCreationWebapp(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeam) {
		writeJSON(w, map[string]string{"error": teamRequestsDisabledMsg})
		return
	}

	var payload struct {
		DisplayName      string `json:"display_name"`
		Name             string `json:"name"`
		Type             string `json:"type"`
		Description      string `json:"description"`
		RequestTeamAdmin bool   `json:"request_team_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	msg, err := p.submitTeamCreationRequest(teamCreationInput{
		RequesterID:      userID,
		DisplayName:      payload.DisplayName,
		Name:             payload.Name,
		Type:             payload.Type,
		Description:      payload.Description,
		RequestTeamAdmin: payload.RequestTeamAdmin,
	})
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// handleSubmitTeamAdminWebapp handles a self-nomination Team Admin request
// from the plugin's React modal. The requester nominates themselves (nomineeIDs
// = [userID]) for the current team. All validation and routing logic is in
// submitTeamAdminRequest.
func (p *Plugin) handleSubmitTeamAdminWebapp(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeamAdmin) {
		writeJSON(w, map[string]string{"error": teamAdminRequestsDisabledMsg})
		return
	}

	var payload struct {
		TeamID string `json:"team_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.TeamID) == "" {
		writeJSON(w, map[string]string{"error": "team_id is required"})
		return
	}

	msg, err := p.submitTeamAdminRequest(userID, payload.TeamID, []string{userID})
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

func (p *Plugin) handleSubmitTeamMemberWebapp(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeamMember) {
		writeJSON(w, map[string]string{"error": teamMemberRequestsDisabledMsg})
		return
	}

	var payload struct {
		TeamID   string   `json:"team_id"`
		Nominees []string `json:"nominees"` // usernames
		Reason   string   `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.TeamID) == "" {
		writeJSON(w, map[string]string{"error": "team_id is required"})
		return
	}
	nomineeIDs, resolveErr := p.resolveUsernameList(payload.Nominees)
	if resolveErr != nil {
		writeJSON(w, map[string]string{"error": resolveErr.Error()})
		return
	}

	msg, err := p.submitTeamMemberRequest(userID, payload.TeamID, nomineeIDs, payload.Reason)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

func (p *Plugin) handleSubmitTeamGuestWebapp(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeamGuest) {
		writeJSON(w, map[string]string{"error": teamGuestRequestsDisabledMsg})
		return
	}

	var payload struct {
		TeamID     string   `json:"team_id"`
		Email      string   `json:"email"`
		ChannelIDs []string `json:"channel_ids"`
		Reason     string   `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.TeamID) == "" {
		writeJSON(w, map[string]string{"error": "team_id is required"})
		return
	}

	msg, err := p.submitTeamGuestRequest(userID, payload.TeamID, payload.Email, payload.ChannelIDs, payload.Reason)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// handleSubmitBotWebapp handles bot account creation requests from the plugin's
// React modal. Fields match the slash-command dialog: username, display_name,
// description, and an optional bot_owner (defaults to the requester when
// empty). Every request, a System Admin's included, goes to the approvers.
func (p *Plugin) handleSubmitBotWebapp(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeBot) {
		writeJSON(w, map[string]string{"error": botRequestsDisabledMsg})
		return
	}

	var payload struct {
		Username                   string `json:"username"`
		DisplayName                string `json:"display_name"`
		Description                string `json:"description"`
		BotOwner                   string `json:"bot_owner"`
		RequestToken               bool   `json:"request_token"`
		IncomingWebhookChannelID   string `json:"incoming_webhook_channel_id"`
		IncomingWebhookDisplayName string `json:"incoming_webhook_display_name"`
		OutgoingWebhookChannelID   string `json:"outgoing_webhook_channel_id"`
		OutgoingWebhookDisplayName string `json:"outgoing_webhook_display_name"`
		OutgoingWebhookCallbackURL string `json:"outgoing_webhook_callback_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	msg, err := p.submitBotRequest(userID, botRequestInput{
		Username:                   payload.Username,
		DisplayName:                payload.DisplayName,
		Description:                payload.Description,
		OwnerUserID:                payload.BotOwner,
		RequestToken:               payload.RequestToken,
		IncomingWebhookChannelID:   payload.IncomingWebhookChannelID,
		IncomingWebhookDisplayName: payload.IncomingWebhookDisplayName,
		OutgoingWebhookChannelID:   payload.OutgoingWebhookChannelID,
		OutgoingWebhookDisplayName: payload.OutgoingWebhookDisplayName,
		OutgoingWebhookCallbackURL: payload.OutgoingWebhookCallbackURL,
	})
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// handleSubmitIncomingWebhookWebapp handles incoming webhook requests from the
// plugin's React modal. The channel_id field is the UUID of the target channel
// (pre-filled from the current channel in Redux state). Every request goes to
// the approvers; the approver's session token is used at creation time.
func (p *Plugin) handleSubmitIncomingWebhookWebapp(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeWebhook) {
		writeJSON(w, map[string]string{"error": webhookRequestsDisabledMsg})
		return
	}

	var payload struct {
		ChannelID   string `json:"channel_id"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	msg, err := p.submitWebhookRequest(userID, "incoming", payload.ChannelID, payload.DisplayName, payload.Description, "")
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// handleSubmitOutgoingWebhookWebapp handles outgoing webhook requests from the
// plugin's React modal. Adds a callback_url field on top of the incoming
// webhook payload. Like every request, it goes to the approvers.
func (p *Plugin) handleSubmitOutgoingWebhookWebapp(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !p.getConfiguration().RequestEnabled(requestTypeWebhook) {
		writeJSON(w, map[string]string{"error": webhookRequestsDisabledMsg})
		return
	}

	var payload struct {
		ChannelID   string `json:"channel_id"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	msg, err := p.submitWebhookRequest(userID, "outgoing", payload.ChannelID, payload.DisplayName, payload.Description, payload.CallbackURL)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
