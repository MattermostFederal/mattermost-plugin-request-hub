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

func (p *Plugin) handleAction(w http.ResponseWriter, r *http.Request, approve bool) {
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

	actingUser, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not verify your identity to approve/deny."})
		return
	}

	requestID, _ := request.Context[actionContextRequestID].(string)
	req, rawReq, err := p.loadRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load channel request", "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not load that request."})
		return
	}
	if req == nil {
		// Only the click that wins the claim below updates the approval card.
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
		return
	}
	if p.refuseAction(w, request.PostId, req.ApprovalPostID, req.ID, requestTypeChannel, approve) {
		return
	}

	// System Admin, primary ApprovalTeam Team Admin (opt-in), or Team Admin of
	// the team this channel will be created in (per-team approval cards).
	if !p.canApproveChannelRequest(actingUser, req.TeamID) {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "You don't have permission to approve or deny this channel request. Team Admins of this team or a System Admin can approve it."})
		return
	}
	if approve && req.RequesterID == actingUser.Id && !actingUser.IsSystemAdmin() {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: ownRequestText})
		return
	}

	// Refresh lookups before claim so a failed register at submit still relays.
	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeChannel,
		ScopeID:           req.TeamID,
	})

	config := p.getConfiguration()
	// Best-effort for the welcome post and audit; approve checks requesterErr.
	requester, requesterErr := p.API.GetUser(req.RequesterID)

	if requestExpired(req.CreatedAt, config.ExpiryDays(), model.GetMillis()) {
		p.expireRequest(w, expiredRequest{
			kvKey:       kvRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindChannel,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			postID:      request.PostId,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        config.ExpiryDays(),
			cardTitle:   "Channel Request Expired",
			cardText:    fmt.Sprintf("Your request for channel **%s** expired before it was reviewed. Submit it again if you still need it.", escapeMarkdown(req.DisplayName)),
			audit: fmt.Sprintf("@%s closed channel request \"%s\" from %s, which was not reviewed within %d days",
				actingUser.Username, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID), config.ExpiryDays()),
		})
		return
	}

	// Re-check what submitRequest checked, since the request may have been
	// pending a while. A failed check leaves it pending so it can be denied.
	if approve {
		if msg := p.approvalBlocker(config, req, requester, requesterErr); msg != "" {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
			return
		}
	}

	// Atomically claim the request before acting on it. Two admins
	// clicking Approve (or one Approve + one Deny) at nearly the same
	// time both load a non-nil req; without an atomic claim both would
	// proceed, creating duplicate channels or approving-and-denying the
	// same request. Compare against the exact bytes we read so the claim
	// is robust regardless of how the request struct is marshaled.
	// Whoever wins owns the outcome; the loser is told it was already
	// handled.
	claimed, claimErr := p.API.KVCompareAndDelete(kvRequestPrefix+req.ID, rawReq)
	if claimErr != nil {
		p.API.LogError("failed to claim channel request", "error", claimErr.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	if !claimed {
		still, _, loadErr := p.loadRequest(req.ID)
		p.respondClaimLost(w, loadErr != nil || still != nil)
		return
	}

	var outcome string
	if approve {
		channel, createErr := p.createChannelForRequest(req)
		if createErr != nil {
			p.API.LogError("failed to create channel on approval", "error", createErr.Error())
			// The claim already removed the request from the KV store.
			// Restore it so a transient creation failure doesn't silently
			// drop the pending request. If the restore ALSO fails the
			// request is genuinely lost, so tell the approver to have the
			// requester resubmit rather than implying a retry will work.
			if restoreErr := p.storeRequest(req); restoreErr != nil {
				p.API.LogError("failed to restore request after create failure", "error", restoreErr.Error())
				p.releasePending(pendingKindChannel, req.RequesterID, req.ID)
				p.deleteApprovalCards(req.ID)
				writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: fmt.Sprintf("Could not create the channel (%s), and the pending request could not be saved — ask the requester to submit it again.", createErr.Error())})
				return
			}
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: fmt.Sprintf("Could not create the channel: %s. The request is still pending. If this keeps failing, the channel name may already be taken — deny it and ask the requester to resubmit with a different name.", createErr.Error())})
			return
		}
		// The KV key was already removed by the atomic claim above.
		p.releasePending(pendingKindChannel, req.RequesterID, req.ID)
		p.addChannelToSidebarCategory(channel.Id, channel.TeamId, req)
		outcome = fmt.Sprintf("✅ Approved by @%s for %s. Channel ~%s created.", actingUser.Username, requesterUsername(requester, req.RequesterID), channel.Name)
		if requester != nil {
			p.postWelcomeMessage(channel, req, requester, actingUser)
		}
		p.logAudit(auditApproved, fmt.Sprintf("@%s approved channel request \"%s\" (~%s) from %s",
			actingUser.Username, escapeMarkdown(req.DisplayName), channel.Name, requesterUsername(requester, req.RequesterID)), request.PostId)
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Channel Request Approved",
			CardText:   fmt.Sprintf("Approved by @%s. Your request for channel **%s** is now available at ~%s.", actingUser.Username, escapeMarkdown(req.DisplayName), channel.Name),
			StatusLine: outcome,
		})})
		return
	}

	p.releasePending(pendingKindChannel, req.RequesterID, req.ID)
	outcome = fmt.Sprintf("❌ Denied by @%s for %s. Channel request **%s** (`%s`).", actingUser.Username, requesterUsername(requester, req.RequesterID), escapeMarkdown(req.DisplayName), req.Name)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied channel request \"%s\" from %s",
		actingUser.Username, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID)), request.PostId)
	writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
		RequestID:   req.ID,
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		ApprovalPostID: request.PostId, Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Channel Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your request for channel **%s** was not approved. Reply to this thread if you'd like more information.", actingUser.Username, escapeMarkdown(req.DisplayName)),
		StatusLine: outcome,
	})})
}

// handleAdminAction handles Approve/Deny on a channel-admin request. Mirrors
// handleAction but operates on adminRequest records and promotes nominees
// instead of creating a channel.
func (p *Plugin) handleAdminAction(w http.ResponseWriter, r *http.Request, approve bool) {
	// Same rule as handleAction: the acting user comes from the session, not
	// the client-supplied body.
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}

	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}

	actingUser, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not verify your identity to approve/deny."})
		return
	}

	requestID, _ := request.Context[actionContextRequestID].(string)
	req, rawReq, err := p.loadAdminRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load channel-admin request", "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not load that request."})
		return
	}
	if req == nil {
		// See handleAction.
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
		return
	}
	if p.refuseAction(w, request.PostId, req.ApprovalPostID, req.ID, requestTypeChannelAdmin, approve) {
		return
	}

	// A channel-admin request is scoped to a specific channel, so the check
	// needs the loaded request's ChannelID and happens here rather than up
	// front.
	if !p.canApproveAdminRequest(actingUser, req.ChannelID) {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "You don't have permission to approve or deny this Channel Admin request. Contact a Team Admin of the channel's team or a System Admin."})
		return
	}
	if approve && req.RequesterID == actingUser.Id && !actingUser.IsSystemAdmin() {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: ownRequestText})
		return
	}

	// Best-effort for notify and audit; approve checks requesterErr.
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	channel, channelErr := p.API.GetChannel(req.ChannelID)

	channelRef := "the channel"
	if channelErr == nil && channel != nil {
		channelRef = "~" + channel.Name
	}
	nominees := p.mentionList(req.NomineeIDs)

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeChannelAdmin,
		ScopeID:           req.ChannelID,
	})

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		p.expireRequest(w, expiredRequest{
			kvKey:       kvAdminRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindAdmin,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			postID:      request.PostId,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Channel Admin Request Expired",
			cardText:    fmt.Sprintf("Your request to make %s Channel Admin in %s expired before it was reviewed. Submit it again if you still need it.", nominees, channelRef),
			audit: fmt.Sprintf("@%s closed the Channel Admin request for %s in %s from %s, which was not reviewed within %d days",
				actingUser.Username, nominees, channelRef, requesterUsername(requester, req.RequesterID), days),
		})
		return
	}

	// Re-check what submitAdminRequest checked, starting with a live channel
	// to promote into. A failed check leaves the request pending so it can be
	// denied.
	if approve {
		if msg := p.adminApprovalBlocker(req, channel, channelErr, requester, requesterErr); msg != "" {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
			return
		}
	}

	claimed, claimErr := p.API.KVCompareAndDelete(kvAdminRequestPrefix+req.ID, rawReq)
	if claimErr != nil {
		p.API.LogError("failed to claim channel-admin request", "error", claimErr.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	if !claimed {
		still, _, loadErr := p.loadAdminRequest(req.ID)
		p.respondClaimLost(w, loadErr != nil || still != nil)
		return
	}

	// The KV key was already removed by the atomic claim above.
	p.releasePending(pendingKindAdmin, req.RequesterID, req.ID)

	if approve {
		// Report only who was actually promoted; nominees who have since
		// left the channel are skipped.
		req.NomineeIDs = p.promoteChannelAdmins(req)
		promoted := p.mentionList(req.NomineeIDs)
		var outcome, cardText string
		if promoted == "" {
			outcome = fmt.Sprintf("⚠️ Approved by @%s for %s, but nobody was promoted: the nominees are no longer members of %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), channelRef)
			cardText = fmt.Sprintf("Approved by @%s, but nobody was promoted: %s are no longer members of %s.", actingUser.Username, nominees, channelRef)
			p.logAudit(auditApproved, fmt.Sprintf("@%s approved %s for Channel Admin in %s, but none were still members (requested by %s)",
				actingUser.Username, nominees, channelRef, requesterUsername(requester, req.RequesterID)), request.PostId)
		} else {
			p.postAdminPromotionMessage(channel, req, actingUser)
			outcome = fmt.Sprintf("✅ Approved by @%s for %s. %s promoted to Channel Admin in %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), promoted, channelRef)
			cardText = fmt.Sprintf("Approved by @%s. %s is now Channel Admin in %s.", actingUser.Username, promoted, channelRef)
			p.logAudit(auditApproved, fmt.Sprintf("@%s promoted %s to Channel Admin in %s (requested by %s)",
				actingUser.Username, promoted, channelRef, requesterUsername(requester, req.RequesterID)), request.PostId)
		}
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Channel Admin Request Approved",
			CardText:   cardText,
			StatusLine: outcome,
		})})
		return
	}

	outcome := fmt.Sprintf("❌ Denied by @%s for %s. Channel Admin for %s in %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), nominees, channelRef)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied a Channel Admin request for %s in %s",
		actingUser.Username, nominees, channelRef), request.PostId)
	writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
		RequestID:   req.ID,
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		ApprovalPostID: request.PostId, Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Channel Admin Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your request to make %s Channel Admin in %s was not approved. Reply to this thread if you'd like more information.", actingUser.Username, nominees, channelRef),
		StatusLine: outcome,
	})})
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

// handleTeamAdminAction handles Approve/Deny on a team admin promotion request.
// System Admins and the Team Admins of the target team may act (see
// canApproveTeamAdminRequest).
func (p *Plugin) handleTeamAdminAction(w http.ResponseWriter, r *http.Request, approve bool) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}
	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}

	actingUser, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not verify your identity."})
		return
	}

	requestID, _ := request.Context[actionContextRequestID].(string)
	req, rawReq, err := p.loadTeamAdminRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load team admin request", "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not load that request."})
		return
	}
	if req == nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
		return
	}
	if !p.canApproveTeamAdminRequest(actingUser, req.TeamID) {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Only System Admins and the team's Team Admins can approve or deny Team Admin requests."})
		return
	}
	if approve && req.RequesterID == actingUser.Id && !actingUser.IsSystemAdmin() {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: ownRequestText})
		return
	}
	if p.refuseAction(w, request.PostId, req.ApprovalPostID, req.ID, requestTypeTeamAdmin, approve) {
		return
	}

	team, teamErr := p.API.GetTeam(req.TeamID)
	teamRef := teamDisplayRef(team, teamErr, req.TeamID)

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeTeamAdmin,
	})

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		requester, _ := p.API.GetUser(req.RequesterID)
		nominees := p.mentionList(req.NomineeIDs)
		p.expireRequest(w, expiredRequest{
			kvKey:       kvTeamAdminRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindTeamAdmin,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			postID:      request.PostId,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Team Admin Request Expired",
			cardText:    fmt.Sprintf("Your request to make %s Team Admin in %s expired before it was reviewed. Submit it again if you still need it.", nominees, teamRef),
			audit: fmt.Sprintf("@%s closed the Team Admin request for %s in %s from %s, which was not reviewed within %d days",
				actingUser.Username, nominees, teamRef, requesterUsername(requester, req.RequesterID), days),
		})
		return
	}

	// Re-check what submit checked. A failed check leaves the request
	// pending so it can be denied.
	if approve {
		if msg := p.teamAdminApprovalBlocker(req, team, teamErr); msg != "" {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
			return
		}
	}

	claimed, claimErr := p.API.KVCompareAndDelete(kvTeamAdminRequestPrefix+req.ID, rawReq)
	if claimErr != nil {
		p.API.LogError("failed to claim team admin request", "error", claimErr.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	if !claimed {
		still, _, loadErr := p.loadTeamAdminRequest(req.ID)
		p.respondClaimLost(w, loadErr != nil || still != nil)
		return
	}
	// The KV key was already removed by the atomic claim above.
	p.releasePending(pendingKindTeamAdmin, req.RequesterID, req.ID)

	requester, _ := p.API.GetUser(req.RequesterID)
	nominees := p.mentionList(req.NomineeIDs)

	if approve {
		p.promoteTeamAdmins(req)
		outcome := fmt.Sprintf("✅ Approved by @%s for %s. %s promoted to Team Admin in %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), nominees, teamRef)
		p.logAudit(auditApproved, fmt.Sprintf("@%s promoted %s to Team Admin in %s (requested by %s)",
			actingUser.Username, nominees, teamRef, requesterUsername(requester, req.RequesterID)), request.PostId)
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Team Admin Request Approved",
			CardText:   fmt.Sprintf("Approved by @%s. %s is now Team Admin in %s.", actingUser.Username, nominees, teamRef),
			StatusLine: outcome,
		})})
		return
	}
	outcome := fmt.Sprintf("❌ Denied by @%s for %s. Team Admin for %s in %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), nominees, teamRef)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied a Team Admin request for %s in %s",
		actingUser.Username, nominees, teamRef), request.PostId)
	writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
		RequestID:   req.ID,
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		ApprovalPostID: request.PostId, Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Team Admin Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your request to make %s Team Admin in %s was not approved. Reply to this thread if you'd like more information.", actingUser.Username, nominees, teamRef),
		StatusLine: outcome,
	})})
}

// handleTeamAction handles Approve/Deny on a team creation request.
// Only System Admins may approve.
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

func (p *Plugin) handleTeamMemberAction(w http.ResponseWriter, r *http.Request, approve bool) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}
	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}

	actingUser, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not verify your identity."})
		return
	}
	if !actingUser.IsSystemAdmin() {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Only System Admins can approve or deny team member requests."})
		return
	}

	requestID, _ := request.Context[actionContextRequestID].(string)
	req, rawReq, err := p.loadTeamMemberRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load team member request", "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not load that request."})
		return
	}
	if req == nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
		return
	}
	if p.refuseAction(w, request.PostId, req.ApprovalPostID, req.ID, requestTypeTeamMember, approve) {
		return
	}

	team, teamErr := p.API.GetTeam(req.TeamID)
	teamRef := teamDisplayRef(team, teamErr, req.TeamID)

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeTeamMember,
	})

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		requester, _ := p.API.GetUser(req.RequesterID)
		nominees := p.mentionList(req.NomineeIDs)
		p.expireRequest(w, expiredRequest{
			kvKey:       kvTeamMemberRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindTeamMember,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			postID:      request.PostId,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Team Member Request Expired",
			cardText:    fmt.Sprintf("Your request to add %s to %s expired before it was reviewed. Submit it again if you still need it.", nominees, teamRef),
			audit: fmt.Sprintf("@%s closed the request to add %s to %s from %s, which was not reviewed within %d days",
				actingUser.Username, nominees, teamRef, requesterUsername(requester, req.RequesterID), days),
		})
		return
	}

	// Re-check what submit checked. A failed check leaves the request
	// pending so it can be denied.
	if approve {
		if msg := p.teamMemberApprovalBlocker(req, team, teamErr); msg != "" {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
			return
		}
	}

	claimed, claimErr := p.API.KVCompareAndDelete(kvTeamMemberRequestPrefix+req.ID, rawReq)
	if claimErr != nil {
		p.API.LogError("failed to claim team member request", "error", claimErr.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	if !claimed {
		still, _, loadErr := p.loadTeamMemberRequest(req.ID)
		p.respondClaimLost(w, loadErr != nil || still != nil)
		return
	}
	// The KV key was already removed by the atomic claim above.
	p.releasePending(pendingKindTeamMember, req.RequesterID, req.ID)

	requester, _ := p.API.GetUser(req.RequesterID)
	nominees := p.mentionList(req.NomineeIDs)

	var outcome string
	if approve {
		failed := p.addTeamMembers(req)
		added := exceptIDs(req.NomineeIDs, failed)
		addedMentions := p.mentionList(added)
		failedMentions := p.mentionList(failed)
		switch {
		case len(added) > 0 && len(failed) > 0:
			outcome = fmt.Sprintf("✅ Approved by @%s for %s. Added %s to %s. Failed for %s.",
				actingUser.Username, requesterUsername(requester, req.RequesterID), addedMentions, teamRef, failedMentions)
		case len(added) > 0:
			outcome = fmt.Sprintf("✅ Approved by @%s for %s. Added %s to %s.",
				actingUser.Username, requesterUsername(requester, req.RequesterID), addedMentions, teamRef)
		case len(failed) > 0:
			outcome = fmt.Sprintf("⚠️ Approved by @%s for %s, but could not add %s to %s.",
				actingUser.Username, requesterUsername(requester, req.RequesterID), failedMentions, teamRef)
		default:
			outcome = fmt.Sprintf("✅ Approved by @%s for %s. No users were added to %s.",
				actingUser.Username, requesterUsername(requester, req.RequesterID), teamRef)
		}
		if len(added) > 0 {
			p.logAudit(auditApproved, fmt.Sprintf("@%s added %s to %s (requested by %s)",
				actingUser.Username, addedMentions, teamRef, requesterUsername(requester, req.RequesterID)), request.PostId)
		} else if len(failed) > 0 {
			p.logAudit(auditApproved, fmt.Sprintf("@%s approved team member request for %s but adds failed for %s",
				actingUser.Username, teamRef, failedMentions), request.PostId)
		}
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Team Member Request Approved",
			CardText:   outcome,
			StatusLine: outcome,
		})})
		return
	}
	outcome = fmt.Sprintf("❌ Denied by @%s for %s. Team member add for %s in %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), nominees, teamRef)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied adding %s to %s",
		actingUser.Username, nominees, teamRef), request.PostId)
	writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
		RequestID:   req.ID,
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		ApprovalPostID: request.PostId, Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Team Member Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your request to add %s to %s was not approved.", actingUser.Username, nominees, teamRef),
		StatusLine: outcome,
	})})
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

func (p *Plugin) handleTeamGuestAction(w http.ResponseWriter, r *http.Request, approve bool) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}
	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}

	actingUser, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not verify your identity."})
		return
	}
	if !actingUser.IsSystemAdmin() {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Only System Admins can approve or deny guest invite requests."})
		return
	}

	requestID, _ := request.Context[actionContextRequestID].(string)
	req, rawReq, err := p.loadTeamGuestRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load team guest request", "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not load that request."})
		return
	}
	if req == nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
		return
	}
	if p.refuseAction(w, request.PostId, req.ApprovalPostID, req.ID, requestTypeTeamGuest, approve) {
		return
	}

	team, teamErr := p.API.GetTeam(req.TeamID)
	teamRef := teamDisplayRef(team, teamErr, req.TeamID)

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeTeamGuest,
	})

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		requester, _ := p.API.GetUser(req.RequesterID)
		p.expireRequest(w, expiredRequest{
			kvKey:       kvTeamGuestRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindTeamGuest,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			postID:      request.PostId,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Guest Invite Request Expired",
			cardText:    fmt.Sprintf("Your request to invite **%s** to %s as a guest expired before it was reviewed. Submit it again if you still need it.", escapeMarkdown(req.Email), teamRef),
			audit: fmt.Sprintf("@%s closed the guest invite request for %s from %s, which was not reviewed within %d days",
				actingUser.Username, teamRef, requesterUsername(requester, req.RequesterID), days),
		})
		return
	}

	// Re-check what submit checked. A failed check leaves the request
	// pending so it can be denied.
	if approve {
		if msg := p.teamGuestApprovalBlocker(req, team, teamErr); msg != "" {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
			return
		}
	}

	claimed, claimErr := p.API.KVCompareAndDelete(kvTeamGuestRequestPrefix+req.ID, rawReq)
	if claimErr != nil {
		p.API.LogError("failed to claim team guest request", "error", claimErr.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	if !claimed {
		still, _, loadErr := p.loadTeamGuestRequest(req.ID)
		p.respondClaimLost(w, loadErr != nil || still != nil)
		return
	}

	requester, _ := p.API.GetUser(req.RequesterID)

	var outcome string
	if approve {
		if fulfillErr := p.fulfillTeamGuestRequest(req, actingUser.Id); fulfillErr != nil {
			if restoreErr := p.API.KVSet(kvTeamGuestRequestPrefix+req.ID, rawReq); restoreErr != nil {
				p.API.LogError("failed to restore team guest request", "error", restoreErr.Error())
				p.releasePending(pendingKindTeamGuest, req.RequesterID, req.ID)
				p.deleteApprovalCards(req.ID)
			}
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: fulfillErr.Error()})
			return
		}
		p.releasePending(pendingKindTeamGuest, req.RequesterID, req.ID)
		outcome = fmt.Sprintf("✅ Approved by @%s for %s. Guest invitation emailed to %s for %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), escapeMarkdown(req.Email), teamRef)
		p.logAudit(auditApproved, fmt.Sprintf("@%s guest invite for %s email \"%s\" (requested by %s)",
			actingUser.Username, teamRef, escapeMarkdown(req.Email), requesterUsername(requester, req.RequesterID)), request.PostId)
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Guest Invite Request Approved",
			CardText:   fmt.Sprintf("Approved by @%s. A guest invitation was emailed to **%s** for %s (channel-scoped guest access).", actingUser.Username, escapeMarkdown(req.Email), teamRef),
			StatusLine: outcome,
		})})
		return
	}
	p.releasePending(pendingKindTeamGuest, req.RequesterID, req.ID)
	outcome = fmt.Sprintf("❌ Denied by @%s for %s. Guest invite for %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), teamRef)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied guest invite for %s",
		actingUser.Username, teamRef), request.PostId)
	writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
		RequestID:   req.ID,
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		ApprovalPostID: request.PostId, Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Guest Invite Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your guest invite request for %s was not approved.", actingUser.Username, teamRef),
		StatusLine: outcome,
	})})
}

func (p *Plugin) handleTeamAction(w http.ResponseWriter, r *http.Request, approve bool) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}
	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}

	actingUser, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not verify your identity."})
		return
	}
	if !p.canApproveTeamRequest(actingUser) {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "You aren't allowed to approve or deny team creation requests."})
		return
	}

	requestID, _ := request.Context[actionContextRequestID].(string)
	req, rawReq, err := p.loadTeamRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load team request", "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not load that request."})
		return
	}
	if req == nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
		return
	}
	if approve && req.RequesterID == actingUser.Id && !actingUser.IsSystemAdmin() {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: ownRequestText})
		return
	}
	if p.refuseAction(w, request.PostId, req.ApprovalPostID, req.ID, requestTypeTeam, approve) {
		return
	}

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeTeam,
	})

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		requester, _ := p.API.GetUser(req.RequesterID)
		p.expireRequest(w, expiredRequest{
			kvKey:       kvTeamRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindTeam,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			postID:      request.PostId,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Team Creation Request Expired",
			cardText:    fmt.Sprintf("Your request to create team **%s** expired before it was reviewed. Submit it again if you still need it.", escapeMarkdown(req.DisplayName)),
			audit: fmt.Sprintf("@%s closed the team creation request for %s from %s, which was not reviewed within %d days",
				actingUser.Username, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID), days),
		})
		return
	}

	if approve {
		if msg := p.teamApprovalBlocker(req); msg != "" {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
			return
		}
	}

	claimed, claimErr := p.API.KVCompareAndDelete(kvTeamRequestPrefix+req.ID, rawReq)
	if claimErr != nil {
		p.API.LogError("failed to claim team request", "error", claimErr.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	if !claimed {
		still, _, loadErr := p.loadTeamRequest(req.ID)
		p.respondClaimLost(w, loadErr != nil || still != nil)
		return
	}

	requester, _ := p.API.GetUser(req.RequesterID)

	var outcome string
	if approve {
		team, createErr := p.createTeamForRequest(req)
		if createErr != nil {
			p.API.LogError("failed to create team on approval", "error", createErr.Error())
			if restoreErr := p.storeTeamRequest(req); restoreErr != nil {
				p.API.LogError("failed to restore team request after create failure", "error", restoreErr.Error())
				p.releasePending(pendingKindTeam, req.RequesterID, req.ID)
				p.deleteApprovalCards(req.ID)
				writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: fmt.Sprintf("Could not create the team (%s), and the pending request could not be saved — ask the requester to resubmit.", createErr.Error())})
				return
			}
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: fmt.Sprintf("Could not create the team: %s. The request is still pending.", createErr.Error())})
			return
		}
		p.releasePending(pendingKindTeam, req.RequesterID, req.ID)
		adminNote := ""
		if req.RequestTeamAdmin {
			adminNote = " Requester promoted to Team Admin."
		}
		outcome = fmt.Sprintf("✅ Approved by @%s for %s. Team **%s** created.%s", actingUser.Username, requesterUsername(requester, req.RequesterID), escapeMarkdown(team.DisplayName), adminNote)
		p.logAudit(auditApproved, fmt.Sprintf("@%s approved team %s (%s) requested by %s",
			actingUser.Username, escapeMarkdown(req.DisplayName), team.Name, requesterUsername(requester, req.RequesterID)), request.PostId)
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Team Creation Request Approved",
			CardText:   fmt.Sprintf("Approved by @%s. Team **%s** has been created.%s", actingUser.Username, escapeMarkdown(req.DisplayName), adminNote),
			StatusLine: outcome,
		})})
		return
	}
	p.releasePending(pendingKindTeam, req.RequesterID, req.ID)
	outcome = fmt.Sprintf("❌ Denied by @%s for %s. Team **%s** (`/%s`).", actingUser.Username, requesterUsername(requester, req.RequesterID), escapeMarkdown(req.DisplayName), req.Name)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied a team creation request for %s",
		actingUser.Username, escapeMarkdown(req.DisplayName)), request.PostId)
	writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
		RequestID:   req.ID,
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		ApprovalPostID: request.PostId, Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Team Creation Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your request to create team **%s** was not approved. Reply to this thread if you'd like more information.", actingUser.Username, escapeMarkdown(req.DisplayName)),
		StatusLine: outcome,
	})})
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

// handleBotAction handles Approve/Deny on a bot account request via the
// two-step engine. Approve records the clicker's step; when both steps are
// filled by two distinct users the bot (and optional extras) are created.
func (p *Plugin) handleBotAction(w http.ResponseWriter, r *http.Request, approve bool) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}
	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}

	actingUser, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not verify your identity."})
		return
	}

	requestID := p.actionRequestID(&request)
	if requestID == "" {
		writeJSON(w, model.PostActionIntegrationResponse{
			EphemeralText: "Could not identify that request. Refresh the channel and try Approve again.",
		})
		return
	}
	key := kvBotRequestPrefix + requestID
	req, rawReq, err := p.loadBotRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load bot request", "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not load that request."})
		return
	}
	if req == nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
		return
	}
	if p.refuseAction(w, request.PostId, req.ApprovalPostID, req.ID, requestTypeBot, approve) {
		return
	}

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeBot,
	})

	canSec, errSec := p.canApproveStep(actingUser, stepSecurity)
	canSys, errSys := p.canApproveStep(actingUser, stepSystem)
	if errSec != nil {
		p.API.LogWarn("bot security approver check failed; treating as not eligible", "user_id", userID, "error", errSec.Error())
		canSec = false
	}
	if errSys != nil {
		p.API.LogWarn("bot system approver check failed; treating as not eligible", "user_id", userID, "error", errSys.Error())
		canSys = false
	}

	// Best-effort for cards and audit; approve checks requesterErr.
	requester, requesterErr := p.API.GetUser(req.RequesterID)

	// Expiry counts from submission, even when one step is already approved.
	if days := p.getConfiguration().ExpiryDays(); (canSec || canSys) && requestExpired(req.CreatedAt, days, model.GetMillis()) {
		p.expireRequest(w, expiredRequest{
			kvKey:       key,
			raw:         rawReq,
			kind:        pendingKindBot,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			postID:      request.PostId,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Bot Account Request Expired",
			cardText:    fmt.Sprintf("Your request for bot **@%s** expired before it was reviewed. Submit it again if you still need it.", req.Username),
			audit: fmt.Sprintf("@%s closed the bot request for @%s from %s, which was not reviewed within %d days",
				actingUser.Username, req.Username, requesterUsername(requester, req.RequesterID), days),
		})
		return
	}

	if !approve {
		if !canSec && !canSys {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "You don't have permission to deny this request."})
			return
		}
		claimed, claimErr := p.API.KVCompareAndDelete(key, rawReq)
		if claimErr != nil {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
			return
		}
		if !claimed {
			still, _, loadErr := p.loadBotRequest(req.ID)
			p.respondClaimLost(w, loadErr != nil || still != nil)
			return
		}
		p.releasePending(pendingKindBot, req.RequesterID, req.ID)
		outcome := fmt.Sprintf("❌ Denied by @%s for %s. Bot **@%s**.", actingUser.Username, requesterUsername(requester, req.RequesterID), req.Username)
		p.logAudit(auditDenied, fmt.Sprintf("@%s denied a bot request for @%s",
			actingUser.Username, req.Username), request.PostId)
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: false, ApproverUsername: actingUser.Username,
			CardTitle:  "Bot Account Request Denied",
			CardText:   fmt.Sprintf("Denied by @%s. Your request for bot **@%s** was not approved. Reply to this thread if you'd like more information.", actingUser.Username, req.Username),
			StatusLine: outcome,
		})})
		return
	}

	step, fill, reason := planApproval(req.twoStepState, canSec, canSys, userID, req.RequesterID)
	if !fill {
		cardRequester := requester
		if cardRequester == nil {
			cardRequester = &model.User{Username: requesterUsername(nil, req.RequesterID)}
		}
		title, cardNotice, ephemeral, paintCard := twoStepApproveBlock(actingUser.Username, reason)
		var att *model.MessageAttachment
		if paintCard {
			att = p.botApprovalAttachmentWithTitledNotice(req, cardRequester, title, cardNotice)
		}
		p.respondTwoStepApproveBlocked(w, userID, request.PostId, ephemeral, p.botApprovalHeader(), att)
		return
	}

	// Re-check what submit checked, before either step, so neither step is
	// filled on a request that can no longer be carried out. A failed check
	// leaves the request and its steps as they are, so it can be denied.
	if msg := p.botApprovalBlocker(req, requester, requesterErr); msg != "" {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
		return
	}

	newState := p.applyTwoStepApproval(req.twoStepState, step, actingUser, model.GetMillis())

	if newState.complete() {
		claimed, claimErr := p.API.KVCompareAndDelete(key, rawReq)
		if claimErr != nil {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
			return
		}
		if !claimed {
			still, _, loadErr := p.loadBotRequest(req.ID)
			p.respondClaimLost(w, loadErr != nil || still != nil)
			return
		}

		bot, createErr := p.createBotForRequest(req)
		if createErr != nil {
			p.API.LogError("failed to create bot on approval", "error", createErr.Error())
			if restoreErr := p.API.KVSet(key, rawReq); restoreErr != nil {
				p.API.LogError("failed to restore bot request after create failure", "error", restoreErr.Error())
				p.releasePending(pendingKindBot, req.RequesterID, req.ID)
				p.deleteApprovalCards(req.ID)
				writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: fmt.Sprintf("Could not create the bot (%s), and the pending request could not be saved — ask the requester to resubmit.", createErr.Error())})
				return
			}
			notice := fmt.Sprintf("Couldn't create the bot: %s. The request is still pending — click **Approve** again to retry, or **Deny** it.", createErr.Error())
			writeJSON(w, model.PostActionIntegrationResponse{
				EphemeralText: notice,
				Update: p.repaintedApprovalPost(
					request.PostId,
					"@channel — a bot account request needs review (a previous approval attempt failed).",
					p.botApprovalAttachmentWithNotice(req, requester, notice),
				),
			})
			return
		}
		p.releasePending(pendingKindBot, req.RequesterID, req.ID)

		authToken := ""
		siteURL := p.siteURL()
		if adminSession, sessErr := p.createUserSession(actingUser.Id); sessErr != nil {
			p.API.LogWarn("failed to create admin session — token/webhook extras will be skipped", "error", sessErr.Error())
		} else {
			authToken = adminSession.Token
			defer func() {
				if revErr := p.API.RevokeSession(adminSession.Id); revErr != nil {
					p.API.LogWarn("failed to revoke admin session after bot extras", "error", revErr.Error())
				}
			}()
		}

		var tokenValue string
		var incomingWebhookURL string
		var outgoingWebhookCreated bool
		var partialErrors []string

		wantsToken := req.RequestToken
		wantsIncoming := req.IncomingWebhookChannelID != ""
		wantsOutgoing := req.OutgoingWebhookChannelID != ""
		if (wantsToken || wantsIncoming || wantsOutgoing) && authToken == "" {
			partialErrors = append(partialErrors, "token/webhook extras were skipped because an admin session could not be created")
		}
		if (wantsIncoming || wantsOutgoing) && siteURL == "" {
			partialErrors = append(partialErrors, "webhook extras were skipped because SiteURL is not configured")
		}

		if wantsToken && authToken != "" {
			tok, tokErr := p.createBotToken(authToken, bot.UserId, "API token for @"+req.Username)
			if tokErr != nil {
				partialErrors = append(partialErrors, fmt.Sprintf("token generation failed: %s", tokErr.Error()))
				p.API.LogError("failed to create bot token", "bot_user_id", bot.UserId, "error", tokErr.Error())
			} else {
				tokenValue = tok
			}
		}

		if wantsIncoming && authToken != "" && siteURL != "" {
			// The hook's owner is the bot, which must be able to read the channel.
			if joinErr := p.addBotToWebhookChannel(bot.UserId, req.IncomingWebhookChannelID); joinErr != nil {
				partialErrors = append(partialErrors, fmt.Sprintf("incoming webhook creation failed: %s", joinErr.Error()))
				p.API.LogError("failed to add bot to incoming webhook channel", "bot_user_id", bot.UserId, "error", joinErr.Error())
			} else {
				var whErr error
				incomingWebhookURL, whErr = p.createIncomingWebhookForBot(authToken, bot.UserId, req)
				if whErr != nil {
					partialErrors = append(partialErrors, fmt.Sprintf("incoming webhook creation failed: %s", whErr.Error()))
					p.API.LogError("failed to create incoming webhook for bot", "error", whErr.Error())
				}
			}
		}

		if wantsOutgoing && authToken != "" && siteURL != "" {
			if outErr := p.createOutgoingWebhookForBot(authToken, bot.UserId, req); outErr != nil {
				partialErrors = append(partialErrors, fmt.Sprintf("outgoing webhook creation failed: %s", outErr.Error()))
				p.API.LogError("failed to create outgoing webhook for bot", "error", outErr.Error())
			} else {
				outgoingWebhookCreated = true
			}
		}

		// Best-effort: reopen the requester DM if submit-time open failed.
		if req.DMChannelID == "" && (tokenValue != "" || incomingWebhookURL != "") {
			if dm, dmErr := p.API.GetDirectChannel(req.RequesterID, p.botUserID); dmErr != nil {
				p.API.LogWarn("failed to open DM for bot extras delivery", "user_id", req.RequesterID, "error", dmErr.Error())
			} else {
				req.DMChannelID = dm.Id
			}
		}

		// If a token was requested but delivery of the extras reply would leave
		// an undeliverable secret, delete the bot when token creation succeeded
		// but we cannot open the DM channel. sendBotExtrasReply logs failures;
		// permanent-delete is reserved for total delivery failure of a token.
		if req.RequestToken && tokenValue != "" && req.DMChannelID == "" {
			p.API.LogError("bot token created but no DM channel for delivery; deleting bot", "bot_user_id", bot.UserId)
			p.deleteApprovalCards(req.ID)
			if delErr := p.API.PermanentDeleteBot(bot.UserId); delErr != nil {
				p.API.LogError("failed to delete bot after token delivery failure", "bot_user_id", bot.UserId, "error", delErr.Error())
				writeJSON(w, model.PostActionIntegrationResponse{Update: p.resolvedPost(request.PostId,
					fmt.Sprintf("⚠️ Fully approved, but the token could not be delivered and bot @%s could not be removed automatically — an admin should delete it.", bot.Username))})
				return
			}
			writeJSON(w, model.PostActionIntegrationResponse{Update: p.resolvedPost(request.PostId,
				fmt.Sprintf("⚠️ Fully approved, but the token could not be delivered, so bot @%s was removed. Ask the requester to submit again.", bot.Username))})
			return
		}

		// Incoming webhook URL is also a secret — delete the hook if undeliverable.
		if incomingWebhookURL != "" && req.DMChannelID == "" {
			p.API.LogError("bot incoming webhook created but no DM channel for URL delivery")
			if hookID := hookIDFromPublicURL(incomingWebhookURL); hookID != "" {
				if delErr := p.deleteWebhook(webhookTypeIncoming, hookID, actingUser.Id); delErr != nil {
					p.API.LogError("failed to delete undeliverable bot incoming webhook", "hook_id", hookID, "error", delErr.Error())
					partialErrors = append(partialErrors, "incoming webhook URL could not be delivered and the hook could not be removed automatically")
				} else {
					partialErrors = append(partialErrors, "incoming webhook URL could not be delivered, so the hook was removed")
				}
			} else {
				partialErrors = append(partialErrors, "incoming webhook URL could not be delivered (no DM channel)")
			}
			incomingWebhookURL = ""
		}

		outcome := fmt.Sprintf("✅ Fully approved (security + system) for %s. Bot **@%s** created.", requesterUsername(requester, req.RequesterID), bot.Username)
		p.logAudit(auditApproved, fmt.Sprintf("@%s gave final approval; bot @%s created for %s",
			actingUser.Username, bot.Username, requesterUsername(requester, req.RequesterID)), request.PostId)
		closed := p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Bot Account Request Approved",
			CardText:   fmt.Sprintf("Fully approved (security + system). Bot **@%s** has been created.", bot.Username),
			StatusLine: outcome,
		})
		p.sendBotExtrasReply(req, tokenValue, incomingWebhookURL, outgoingWebhookCreated, partialErrors)
		writeJSON(w, model.PostActionIntegrationResponse{Update: closed})
		return
	}

	newReq := *req
	newReq.twoStepState = newState
	newBytes, marshalErr := json.Marshal(&newReq)
	if marshalErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	set, setErr := p.API.KVCompareAndSet(key, rawReq, newBytes)
	if setErr != nil || !set {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "This request was just updated by someone else. Please try again."})
		return
	}
	p.notifyTicketProgress(ticketProgress{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Title: "Bot Account Request Update",
		Text:  fmt.Sprintf("Your bot request for **@%s** received the %s approval; awaiting the other approval.", req.Username, step),
	})
	writeJSON(w, model.PostActionIntegrationResponse{
		Update: p.repaintedApprovalPost(
			request.PostId,
			p.botApprovalHeader(),
			p.botApprovalAttachment(&newReq, requester),
		),
	})
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

// handleWebhookAction handles Approve/Deny on a webhook request via the
// two-step engine. On final approval it creates the webhook and DMs the
// result; undelivered incoming-hook URLs are deleted as unowned secrets.
func (p *Plugin) handleWebhookAction(w http.ResponseWriter, r *http.Request, approve bool) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}

	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}

	actingUser, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not verify your identity."})
		return
	}

	requestID := p.actionRequestID(&request)
	if requestID == "" {
		writeJSON(w, model.PostActionIntegrationResponse{
			EphemeralText: "Could not identify that request. Refresh the channel and try Approve again.",
		})
		return
	}
	key := kvWebhookRequestPrefix + requestID
	req, rawReq, err := p.loadWebhookRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load webhook request", "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not load that request."})
		return
	}
	if req == nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
		return
	}
	if p.refuseAction(w, request.PostId, req.ApprovalPostID, req.ID, requestTypeWebhook, approve) {
		return
	}

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeWebhook,
	})

	canSec, errSec := p.canApproveStep(actingUser, stepSecurity)
	canSys, errSys := p.canApproveStep(actingUser, stepSystem)
	if errSec != nil {
		p.API.LogWarn("webhook security approver check failed; treating as not eligible", "user_id", userID, "error", errSec.Error())
		canSec = false
	}
	if errSys != nil {
		p.API.LogWarn("webhook system approver check failed; treating as not eligible", "user_id", userID, "error", errSys.Error())
		canSys = false
	}

	// Best-effort for cards and audit; approve checks requesterErr.
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	channel, channelErr := p.API.GetChannel(req.ChannelID)

	// Expiry counts from submission, even when one step is already approved.
	if days := p.getConfiguration().ExpiryDays(); (canSec || canSys) && requestExpired(req.CreatedAt, days, model.GetMillis()) {
		p.expireRequest(w, expiredRequest{
			kvKey:       key,
			raw:         rawReq,
			kind:        pendingKindWebhook,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			postID:      request.PostId,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Webhook Request Expired",
			cardText:    fmt.Sprintf("Your request for a %s webhook **%s** expired before it was reviewed. Submit it again if you still need it.", req.WebhookType, escapeMarkdown(req.DisplayName)),
			audit: fmt.Sprintf("@%s closed the %s webhook request for %s from %s, which was not reviewed within %d days",
				actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID), days),
		})
		return
	}

	if !approve {
		if !canSec && !canSys {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "You don't have permission to deny this request."})
			return
		}
		claimed, claimErr := p.API.KVCompareAndDelete(key, rawReq)
		if claimErr != nil {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
			return
		}
		if !claimed {
			still, _, loadErr := p.loadWebhookRequest(req.ID)
			p.respondClaimLost(w, loadErr != nil || still != nil)
			return
		}
		p.releasePending(pendingKindWebhook, req.RequesterID, req.ID)
		outcome := fmt.Sprintf("❌ Denied by @%s for %s. %s webhook **%s**.", actingUser.Username, requesterUsername(requester, req.RequesterID), req.WebhookType, escapeMarkdown(req.DisplayName))
		p.logAudit(auditDenied, fmt.Sprintf("@%s denied a %s webhook request for %s",
			actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName)), request.PostId)
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: false, ApproverUsername: actingUser.Username,
			CardTitle:  "Webhook Request Denied",
			CardText:   fmt.Sprintf("Denied by @%s. Your request for a %s webhook **%s** was not approved. Reply to this thread if you'd like more information.", actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName)),
			StatusLine: outcome,
		})})
		return
	}

	step, fill, reason := planApproval(req.twoStepState, canSec, canSys, userID, req.RequesterID)
	if !fill {
		cardRequester := requester
		if cardRequester == nil {
			cardRequester = &model.User{Username: requesterUsername(nil, req.RequesterID)}
		}
		cardChannel := channel
		if cardChannel == nil {
			cardChannel = &model.Channel{Name: req.ChannelID}
		}
		title, cardNotice, ephemeral, paintCard := twoStepApproveBlock(actingUser.Username, reason)
		var att *model.MessageAttachment
		if paintCard {
			att = p.webhookApprovalAttachmentWithTitledNotice(req, cardRequester, cardChannel, title, cardNotice)
		}
		p.respondTwoStepApproveBlocked(w, userID, request.PostId, ephemeral, p.webhookApprovalHeader(), att)
		return
	}

	// Re-check what submit checked, before either step, so neither step is
	// filled on a request that can no longer be carried out. A failed check
	// leaves the request and its steps as they are, so it can be denied.
	if msg := p.webhookApprovalBlocker(req, requester, requesterErr, channel, channelErr); msg != "" {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: msg})
		return
	}

	newState := p.applyTwoStepApproval(req.twoStepState, step, actingUser, model.GetMillis())

	if newState.complete() {
		claimed, claimErr := p.API.KVCompareAndDelete(key, rawReq)
		if claimErr != nil {
			writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
			return
		}
		if !claimed {
			still, _, loadErr := p.loadWebhookRequest(req.ID)
			p.respondClaimLost(w, loadErr != nil || still != nil)
			return
		}

		dmMsg, hookID, createErr := p.createWebhookForRequest(req, actingUser.Id)
		if createErr != nil {
			p.API.LogError("failed to create webhook on approval", "error", createErr.Error())
			if restoreErr := p.API.KVSet(key, rawReq); restoreErr != nil {
				p.API.LogError("failed to restore webhook request after create failure", "error", restoreErr.Error())
				p.releasePending(pendingKindWebhook, req.RequesterID, req.ID)
				p.deleteApprovalCards(req.ID)
				writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: fmt.Sprintf("Could not create the webhook (%s), and the pending request could not be saved — ask the requester to resubmit.", createErr.Error())})
				return
			}
			notice := fmt.Sprintf("Couldn't create the webhook: %s. The request is still pending — click **Approve** again to retry, or **Deny** it.", createErr.Error())
			writeJSON(w, model.PostActionIntegrationResponse{
				EphemeralText: notice,
				Update: p.repaintedApprovalPost(
					request.PostId,
					"@channel — a webhook request needs review (a previous approval attempt failed).",
					p.webhookApprovalAttachmentWithNotice(req, requester, channel, notice),
				),
			})
			return
		}
		p.releasePending(pendingKindWebhook, req.RequesterID, req.ID)

		// Best-effort: reopen the requester DM if submit-time open failed.
		if req.DMChannelID == "" {
			if dm, dmErr := p.API.GetDirectChannel(req.RequesterID, p.botUserID); dmErr != nil {
				p.API.LogWarn("failed to open DM for webhook URL delivery", "user_id", req.RequesterID, "error", dmErr.Error())
			} else {
				req.DMChannelID = dm.Id
			}
		}

		// Deliver via the existing ticket thread. If DM channel is missing for
		// an incoming webhook (secret URL), delete the hook.
		if req.WebhookType == webhookTypeIncoming && req.DMChannelID == "" {
			p.API.LogError("webhook created but no DM channel for URL delivery", "hook_id", hookID)
			p.deleteApprovalCards(req.ID)
			removed := true
			if delErr := p.deleteWebhook(req.WebhookType, hookID, actingUser.Id); delErr != nil {
				p.API.LogError("failed to delete webhook after URL delivery failure", "hook_id", hookID, "error", delErr.Error())
				removed = false
			}
			var outcome string
			if removed {
				outcome = fmt.Sprintf("⚠️ Fully approved, but the URL could not be delivered, so the %s webhook **%s** was removed. Ask the requester to submit again.", req.WebhookType, escapeMarkdown(req.DisplayName))
			} else {
				outcome = fmt.Sprintf("⚠️ Fully approved, but the URL could not be delivered and the %s webhook **%s** could not be removed automatically — an admin should delete it.", req.WebhookType, escapeMarkdown(req.DisplayName))
			}
			writeJSON(w, model.PostActionIntegrationResponse{Update: p.resolvedPost(request.PostId, outcome)})
			return
		}

		outcome := fmt.Sprintf("✅ Fully approved (security + system) for %s. %s webhook **%s** created.", requesterUsername(requester, req.RequesterID), req.WebhookType, escapeMarkdown(req.DisplayName))
		p.logAudit(auditApproved, fmt.Sprintf("@%s gave final approval; %s webhook \"%s\" created for %s",
			actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID)), request.PostId)
		writeJSON(w, model.PostActionIntegrationResponse{Update: p.closeTicket(ticketClose{
			RequestID:   req.ID,
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			ApprovalPostID: request.PostId, Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Webhook Request Approved",
			CardText:   fmt.Sprintf("Fully approved (security + system). %s", dmMsg),
			StatusLine: outcome,
		})})
		return
	}

	newReq := *req
	newReq.twoStepState = newState
	newBytes, marshalErr := json.Marshal(&newReq)
	if marshalErr != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	set, setErr := p.API.KVCompareAndSet(key, rawReq, newBytes)
	if setErr != nil || !set {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "This request was just updated by someone else. Please try again."})
		return
	}
	p.notifyTicketProgress(ticketProgress{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Title: "Webhook Request Update",
		Text:  fmt.Sprintf("Your %s webhook request for **%s** received the %s approval; awaiting the other approval.", req.WebhookType, escapeMarkdown(req.DisplayName), step),
	})
	writeJSON(w, model.PostActionIntegrationResponse{
		Update: p.repaintedApprovalPost(
			request.PostId,
			p.webhookApprovalHeader(),
			p.webhookApprovalAttachment(&newReq, requester, channel),
		),
	})
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

// expiredRequest describes a pending request whose approval card was
// clicked after it expired.
type expiredRequest struct {
	kvKey       string
	raw         []byte
	kind        string // pendingKind*
	requestID   string
	requesterID string
	postID      string // approval card
	dmRootID    string // requester's ticket DM root
	dmChannelID string
	days        int
	cardTitle   string // requester's ticket card
	cardText    string
	audit       string // audit-channel line
}

// expireRequest closes an expired request: it claims the record like an
// approve or deny would, closes the requester's ticket as expired, and
// marks the approval card expired.
func (p *Plugin) expireRequest(w http.ResponseWriter, e expiredRequest) {
	claimed, claimErr := p.API.KVCompareAndDelete(e.kvKey, e.raw)
	if claimErr != nil {
		p.API.LogError("failed to claim expired request", "error", claimErr.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Could not process that request."})
		return
	}
	if !claimed {
		still, loadErr := p.API.KVGet(e.kvKey)
		p.respondClaimLost(w, loadErr != nil || still != nil)
		return
	}
	p.releasePending(e.kind, e.requesterID, e.requestID)
	p.logAudit(auditExpired, e.audit, e.postID)
	writeJSON(w, model.PostActionIntegrationResponse{
		EphemeralText: expiredText,
		Update: p.closeTicket(ticketClose{
			RequestID:   e.requestID,
			RequesterID: e.requesterID, DMRootPostID: e.dmRootID, DMChannelID: e.dmChannelID,
			ApprovalPostID: e.postID, Expired: true,
			CardTitle:   e.cardTitle,
			CardText:    e.cardText,
			ThreadReply: fmt.Sprintf("⌛ **Expired**: not reviewed within %d days.", e.days),
			StatusLine:  fmt.Sprintf("⌛ Expired: not reviewed within %d days.", e.days),
		}),
	})
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

// respondClaimLost answers a click that lost the claim on a request. It
// never updates the card: the click that won the claim already did, and
// overwriting it here would erase that outcome.
func (p *Plugin) respondClaimLost(w http.ResponseWriter, stillPending bool) {
	if stillPending {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: claimLostPendingText})
		return
	}
	writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: alreadyHandledText})
}

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
	post, appErr := p.API.GetPost(postID)
	if appErr != nil || post == nil || post.UserId != p.botUserID {
		return &model.Post{Message: status}
	}
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

// canApproveTeamAdminRequest reports whether the acting user may approve or
// deny a Team Admin request for teamID: System Admins, and the Team Admins of
// that team. Mattermost already lets a Team Admin promote their own team's
// members, so this grants nothing new. Approval-team Team Admins are not
// included for other teams. postTeamAdminApprovalRequest mentions this set.
func (p *Plugin) canApproveTeamAdminRequest(user *model.User, teamID string) bool {
	if user == nil {
		return false
	}
	if user.IsSystemAdmin() {
		return true
	}
	return p.isTeamAdminOfTeamID(user.Id, teamID)
}

// canApproveTeamRequest reports whether the acting user may approve or deny a
// team creation request: System Admins, and Team Admins of the approval team
// when AllowTeamAdminTeamApprovers is on.
func (p *Plugin) canApproveTeamRequest(user *model.User) bool {
	if user == nil {
		return false
	}
	if user.IsSystemAdmin() {
		return true
	}
	return p.getConfiguration().AllowTeamAdminTeamApprovers && p.isApprovalTeamAdmin(user.Id)
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
