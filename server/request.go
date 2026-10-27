package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	// kvRequestPrefix namespaces pending requests in the KV store.
	kvRequestPrefix = "request_"

	// dialogCallbackID identifies submissions from the channel request dialog.
	dialogCallbackID = "channel_request"

	// Dialog element / action context field names.
	fieldDisplayName = "display_name"
	fieldName        = "name"
	fieldPurpose     = "purpose"
	fieldType        = "type"
	fieldMembers     = "members"
	fieldAdmins      = "admins"

	// actionContextRequestID carries the pending request ID on the approve/deny buttons.
	actionContextRequestID = "request_id"

	// fieldCategory is the optional sidebar category name the requester specifies.
	// On approval the new channel is placed in this category for the requester and
	// all designated members.
	fieldCategory = "category"

	// Channel type values as plain strings, for use in dialog options, comparisons, and storage.
	channelTypeOpen    = string(model.ChannelTypeOpen)
	channelTypePrivate = string(model.ChannelTypePrivate)

	// Server-side input bounds. The dialog enforces these client-side via
	// MaxLength, but the webapp JSON endpoint bypasses the dialog, so the
	// server must enforce them too.
	maxDisplayNameLen = 64
	maxPurposeLen     = 250

	// Bounds for the other request types' free-text fields.
	maxDescriptionLen = 500
	maxReasonLen      = 500
	maxEmailLen       = 128

	// Mattermost limits these in bytes, not characters.
	maxWebhookNameBytes        = 64
	maxWebhookDescriptionBytes = 500
	maxTeamDescriptionBytes    = model.TeamDescriptionMaxLength
	// Mattermost checks an outgoing webhook's callback list as printed,
	// "[url]", against 1024 bytes, leaving 1022 for a single URL.
	maxCallbackURLLen = 1022

	// maxMembersPerList caps the regular-member and channel-admin lists
	// independently so a single request can't fan out into thousands of
	// synchronous user-lookup and add-member API calls.
	maxMembersPerList = 100

	// channelAdminRoleString grants a channel member the Channel Admin
	// role. MM maps this classic role string to the appropriate scheme
	// role internally. Used both when creating a channel and when
	// promoting members on an existing one.
	channelAdminRoleString = "channel_user channel_admin"
)

// channelRequest is a pending request to create a channel, persisted in the KV store until a System
// Admin approves or denies it.
type channelRequest struct {
	ID          string   `json:"id"`
	RequesterID string   `json:"requester_id"`
	TeamID      string   `json:"team_id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Purpose     string   `json:"purpose"`
	ChannelType string   `json:"channel_type"`
	MemberIDs   []string `json:"member_ids"`
	// AdminMemberIDs are the requester-designated Channel Admins. On
	// approval, these users get channel_admin scheme roles in the newly
	// created channel (in addition to being members).
	AdminMemberIDs []string `json:"admin_member_ids"`

	// CategoryName is the optional sidebar category the requester wants the
	// new channel placed in on approval. Matched case-insensitively; if no
	// match exists for a user, a custom category with this name is created.
	CategoryName string `json:"category_name,omitempty"`

	// Ticket messaging fields — set after the approval post and DM are created.
	// Used by the MessageHasBeenPosted hook to route thread replies in both directions.
	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`
}

// requestInput is the normalized set of values gathered from either entry point (slash command
// dialog or webapp modal) before a request is created.
type requestInput struct {
	RequesterID string
	TeamID      string
	DisplayName string
	// Name holds ONLY THE SUFFIX (the part after the selected prefix);
	// server code prepends the prefix. May be blank, in which case the
	// suffix is generated from DisplayName.
	Name string
	// Prefix is the selected domain prefix (e.g., "team-", "project-").
	// Required — a request with no prefix is rejected by resolveChannelName.
	Prefix         string
	Purpose        string
	ChannelType    string
	MemberIDs      []string
	AdminMemberIDs []string
	// CategoryName is the optional sidebar category name to place the channel
	// in on approval. Passed through to channelRequest unchanged.
	CategoryName string
}

// promoteSoleMember applies the orphaned-channel guard: when no Channel
// Admins were designated and exactly one regular member was added, that
// member becomes the Channel Admin (returned as the admin list, with the
// member list emptied). In every other case the inputs pass through
// unchanged. A single-member channel with no admin has nobody who can
// manage it, which is the "No Channel Admin" orphan case.
func promoteSoleMember(memberIDs, adminIDs []string) (members, admins []string) {
	if len(adminIDs) == 0 && len(memberIDs) == 1 {
		return nil, memberIDs
	}
	return memberIDs, adminIDs
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9]+`)

// markdownSpecial matches characters that start Markdown syntax in a
// Mattermost post: emphasis, code, links, images, headings, quotes, tables,
// strikethrough, lists, HTML-ish brackets and the backslash itself. "." is
// included so bare domains aren't auto-linked. ":" is deliberately not: the
// emoticon parser ignores the escape, so "\:/" renders as a stray backslash
// and an emoji. escapeMarkdown breaks up bare URLs instead.
var markdownSpecial = regexp.MustCompile("([\\\\`*_\\[\\]()#~>|!<{}+\\-.])")

// escapeMarkdown makes requester-supplied text (display names, purposes,
// team names) render as typed in bot posts and approval cards: Markdown
// characters are backslash-escaped, a zero-width space inside "://" keeps
// bare URLs as text, and one after each @ keeps mentions as text.
func escapeMarkdown(s string) string {
	s = markdownSpecial.ReplaceAllString(s, `\$1`)
	s = strings.ReplaceAll(s, "://", ":\u200b//")
	return strings.ReplaceAll(s, "@", "@\u200b")
}

// slugify converts a free-form string into a valid channel URL name.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = invalidNameChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > model.ChannelNameMaxLength {
		s = s[:model.ChannelNameMaxLength]
	}
	return s
}

// resolveChannelName derives the final channel URL name from the
// request using the admin's configured prefix list. Requires at least
// one prefix to be defined; returns an error otherwise so the admin
// notices the misconfiguration instead of silently permitting free-form
// channel names.
func (p *Plugin) resolveChannelName(config *configuration, in requestInput) (string, error) {
	if !config.UsesPrefixList() {
		return "", errors.New("plugin is not configured: an admin must define at least one channel prefix in System Console -> Plugins -> Mattermost Request Hub")
	}
	return resolvePrefixedName(config.Prefixes(), in)
}

// resolvePrefixedName joins the selected prefix with a slugified suffix.
// The suffix is slugified BEFORE joining so users can type "My Team" and
// get "team-my-team".
func resolvePrefixedName(prefixes []channelPrefix, in requestInput) (string, error) {
	selected := strings.TrimSpace(in.Prefix)
	if selected == "" {
		return "", errors.New("please pick a channel prefix (e.g., team-, project-, ops-) from the dropdown")
	}
	var entry *channelPrefix
	for i := range prefixes {
		if prefixes[i].Prefix == selected {
			entry = &prefixes[i]
			break
		}
	}
	if entry == nil {
		return "", errors.Errorf("prefix %q is not in the list of allowed prefixes", selected)
	}

	suffix := strings.TrimSpace(in.Name)
	if suffix == "" {
		// Fall back to slugifying the display name so users who leave
		// the URL field blank still get a sensible suffix.
		suffix = in.DisplayName
	}
	suffix = slugify(suffix)
	// Requesters sometimes double-type the prefix; strip it so
	// "team-marketing" under prefix "team-" doesn't become "team-team-marketing".
	suffix = strings.TrimPrefix(suffix, strings.TrimSuffix(entry.Prefix, "-")+"-")
	suffix = strings.Trim(suffix, "-")

	if suffix == "" {
		return "", errors.New("channel name suffix is required (letters/numbers, becomes the part after the prefix)")
	}

	// The compiled pattern is anchored (^(?:...)$) so it must match the
	// whole suffix. Error messages surface the raw pattern text so users
	// see what the admin wrote, not our anchored rewrite.
	if entry.SuffixPattern != nil && !entry.SuffixPattern.MatchString(suffix) {
		return "", errors.Errorf("suffix %q doesn't match the required pattern for prefix %q (%s)", suffix, entry.Prefix, entry.SuffixPatternRaw)
	}

	name := entry.Prefix + suffix
	// IsValidChannelIdentifier doesn't check the upper bound, and a name the
	// database can't hold would only fail at approval time.
	if len(name) > model.ChannelNameMaxLength || !model.IsValidChannelIdentifier(name) {
		return "", errors.Errorf("%q is not a valid channel URL name; combined prefix + suffix must be 2-64 lowercase letters, numbers, or hyphens", name)
	}
	// With nested prefixes ("team-" and "team-ops-"), require the most
	// specific one so its suffix rule applies.
	for i := range prefixes {
		other := prefixes[i].Prefix
		if len(other) > len(entry.Prefix) && strings.HasPrefix(name, other) {
			return "", errors.Errorf("%q starts with the prefix %q; pick that prefix instead", name, other)
		}
	}
	return name, nil
}

// validateRequestInput checks the caller-supplied fields that require no
// API calls, returning a user-facing error for the first problem. It is
// pure so it can be unit-tested without mocks. Side-effecting checks —
// team membership (authorization) and prefix/name resolution (needs
// config) — happen in submitRequest after this passes.
func validateRequestInput(in requestInput) error {
	if strings.TrimSpace(in.DisplayName) == "" {
		return errors.New("a channel name is required")
	}
	// Count runes on the trimmed value to match the dialog's MaxLength
	// (which counts characters), so a multibyte name the UI accepts isn't
	// rejected server-side by a byte-length check.
	if utf8.RuneCountInString(strings.TrimSpace(in.DisplayName)) > maxDisplayNameLen {
		return errors.Errorf("channel name must be %d characters or fewer", maxDisplayNameLen)
	}
	if utf8.RuneCountInString(strings.TrimSpace(in.Purpose)) > maxPurposeLen {
		return errors.Errorf("purpose must be %d characters or fewer", maxPurposeLen)
	}
	if len(in.MemberIDs) > maxMembersPerList || len(in.AdminMemberIDs) > maxMembersPerList {
		return errors.Errorf("too many members: at most %d members and %d channel admins per request", maxMembersPerList, maxMembersPerList)
	}
	// The dialog submits raw IDs; reject anything that isn't one.
	for _, id := range append(append([]string{}, in.MemberIDs...), in.AdminMemberIDs...) {
		if !model.IsValidId(id) {
			return errors.New("members and channel admins must be valid users")
		}
	}
	if strings.TrimSpace(in.TeamID) == "" {
		return errors.New("a team is required")
	}
	return nil
}

// checkTextLen returns a user-facing error when the trimmed s is longer
// than limit characters.
func checkTextLen(field, s string, limit int) error {
	if utf8.RuneCountInString(strings.TrimSpace(s)) > limit {
		return errors.Errorf("%s must be %d characters or fewer", field, limit)
	}
	return nil
}

// checkByteLen is checkTextLen for fields Mattermost limits in bytes, where
// a character outside basic Latin counts as more than one.
func checkByteLen(field, s string, limit int) error {
	if len(strings.TrimSpace(s)) > limit {
		return errors.Errorf("%s must be %d bytes or fewer (some characters count as more than one)", field, limit)
	}
	return nil
}

// checkIDList bounds a caller-supplied list of user or channel IDs and
// rejects anything that isn't an ID. noun names the list in the error.
func checkIDList(ids []string, noun string) error {
	if len(ids) > maxMembersPerList {
		return errors.Errorf("too many %s: at most %d per request", noun, maxMembersPerList)
	}
	for _, id := range ids {
		if !model.IsValidId(id) {
			return errors.Errorf("%s must be picked from the list", noun)
		}
	}
	return nil
}

// checkCallbackURL requires an outgoing webhook callback to be an http or
// https address Mattermost accepts.
func checkCallbackURL(raw string) error {
	if err := checkByteLen("the callback URL", raw, maxCallbackURLLen); err != nil {
		return err
	}
	if !model.IsValidHTTPURL(strings.TrimSpace(raw)) {
		return errors.New("the callback URL must be an http or https address")
	}
	return nil
}

// nomineeRules says who may be nominated by a request.
type nomineeRules struct {
	role        string // what nominees become, for messages ("a Team Admin")
	allowBots   bool
	allowGuests bool
	teamID      string // when set, nominees must be active members of this team
}

// checkNominees loads each nominee and returns a user-facing error for the
// first who can't be nominated: unknown, deactivated, or excluded by rules.
func (p *Plugin) checkNominees(ids []string, rules nomineeRules) error {
	for _, id := range ids {
		user, appErr := p.API.GetUser(id)
		if appErr != nil || user == nil {
			return errors.New("one of the people you picked could not be found")
		}
		if user.DeleteAt != 0 {
			return errors.Errorf("@%s is deactivated and can't be made %s", user.Username, rules.role)
		}
		if user.IsBot && !rules.allowBots {
			return errors.Errorf("@%s is a bot and can't be made %s", user.Username, rules.role)
		}
		if user.IsGuest() && !rules.allowGuests {
			return errors.Errorf("@%s is a guest and can't be made %s", user.Username, rules.role)
		}
		if rules.teamID != "" {
			member, appErr := p.API.GetTeamMember(rules.teamID, id)
			if appErr != nil || member == nil || member.DeleteAt != 0 {
				return errors.Errorf("@%s isn't a member of this team", user.Username)
			}
		}
	}
	return nil
}

// submitRequest validates the input, stores a pending request and posts it to the approval channel.
// It returns a message suitable for showing to the requester.
func (p *Plugin) submitRequest(in requestInput) (string, error) {
	config := p.getConfiguration()

	if err := validateRequestInput(in); err != nil {
		return "", err
	}

	requester, appErr := p.API.GetUser(in.RequesterID)
	if appErr != nil {
		p.API.LogError("failed to load requesting user", "user_id", in.RequesterID, "error", appErr.Error())
		return "", errors.New("could not load your account; try again")
	}

	// System Admins can create channels in any team (they administer all
	// of them). Everyone else —
	// must belong to the target team, so a client-supplied TeamID can't be
	// used to plant channels in a team the requester has no access to.
	if !requester.IsSystemAdmin() {
		// GetTeamMember returns soft-deleted rows (a user who LEFT the team
		// still has a TeamMembers row with DeleteAt != 0), so an error isn't
		// enough — require an active membership.
		member, appErr := p.API.GetTeamMember(in.TeamID, in.RequesterID)
		if appErr != nil || member == nil || member.DeleteAt != 0 {
			return "", errors.New("you must be a member of the team to request a channel in it")
		}
	}

	if in.ChannelType != channelTypeOpen && in.ChannelType != channelTypePrivate {
		in.ChannelType = channelTypeOpen
	}
	// Display names are single-line.
	in.DisplayName = strings.Join(strings.Fields(in.DisplayName), " ")
	in.MemberIDs = dedupeNonEmpty(in.MemberIDs)
	in.AdminMemberIDs = dedupeNonEmpty(in.AdminMemberIDs)

	// Guard against orphaned channels: when the requester adds exactly one
	// member and names no Channel Admins, that lone member is promoted to
	// Channel Admin so the channel always has someone who can manage it.
	// Applied here (before the request is built) so the approval card and
	// welcome message reflect the promotion too — not just the created
	// channel.
	in.MemberIDs, in.AdminMemberIDs = promoteSoleMember(in.MemberIDs, in.AdminMemberIDs)

	name, err := p.resolveChannelName(config, in)
	if err != nil {
		return "", err
	}

	req := &channelRequest{
		ID:             model.NewId(),
		RequesterID:    in.RequesterID,
		TeamID:         in.TeamID,
		Name:           name,
		DisplayName:    strings.TrimSpace(in.DisplayName),
		Purpose:        strings.TrimSpace(in.Purpose),
		ChannelType:    in.ChannelType,
		MemberIDs:      in.MemberIDs,
		AdminMemberIDs: in.AdminMemberIDs,
		CategoryName:   strings.TrimSpace(in.CategoryName),
	}

	// Store the request before posting the approval message so the approve/deny
	// handlers can find it by ID the moment the post is live.
	if storeErr := p.storeRequest(req); storeErr != nil {
		p.API.LogError("failed to store channel request", "error", storeErr.Error())
		return "", errors.New("could not save your request; try again")
	}
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		_ = p.API.KVDelete(kvRequestPrefix + req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot channel request")
	}

	approvalPostID, approvalChannelID, err := p.postApprovalRequest(req, requester)
	if err != nil {
		// Roll back the stored request so it isn't orphaned without an approval message.
		_ = p.API.KVDelete(kvRequestPrefix + req.ID)
		p.API.LogError("failed to post channel request for approval", "error", err.Error())
		return "", errors.New("could not send your request to the approvers; try again or contact a System Admin")
	}

	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	dmRootPostID, dmChannelID := p.sendTicketCreatedDM(req, requester)
	req.DMRootPostID = dmRootPostID
	req.DMChannelID = dmChannelID

	p.persistTicketPosts(ticketPersist{
		KVKey: kvRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "channel request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
			Kind: requestTypeChannel, ScopeID: req.TeamID,
		},
	})

	return "Your channel request has been submitted for approval. You'll be notified once an admin responds.", nil
}

// sendTicketCreatedDM posts the channel-request pending card as the requester's
// ticket DM root (shared postTicketCreatedDM path).
func (p *Plugin) sendTicketCreatedDM(req *channelRequest, requester *model.User) (dmRootPostID, dmChannelID string) {
	return p.postTicketCreatedDM(
		req.RequesterID,
		p.approvalAttachment(req, requester),
		ticketSubmittedIntro("channel request"),
	)
}

// postWelcomeMessage posts a bot message in the newly-created channel announcing who requested it
// and who approved, then @-mentions the added members and channel admins so everyone who was
// added is notified they're in the channel.
func (p *Plugin) postWelcomeMessage(channel *model.Channel, req *channelRequest, requester, approver *model.User) {
	body := fmt.Sprintf("Welcome — this channel was requested by @%s", requester.Username)
	if approver != nil && approver.Id != requester.Id {
		body += fmt.Sprintf(" and approved by @%s", approver.Username)
	}
	body += ". Adjust the header + purpose to fit."

	// Mention the added users so they get notified. The requester is the
	// creator and already here, so only call out the designated members
	// and admins. mentionList returns "" for an empty list.
	if members := p.mentionList(req.MemberIDs); members != "" {
		body += "\nAdded: " + members
	}
	if admins := p.mentionList(req.AdminMemberIDs); admins != "" {
		body += "\nChannel admins: " + admins
	}

	if _, appErr := p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: channel.Id,
		Message:   body,
	}); appErr != nil {
		p.API.LogWarn("failed to post welcome message", "channel_id", channel.Id, "error", appErr.Error())
	}
}

// auditOutcome selects the colored card styling for an audit-channel post.
type auditOutcome int

const (
	auditApproved auditOutcome = iota // green ✅
	auditDenied                       // red ❌
)

// logAudit posts a colored attachment card to the auto-created audit channel.
// When threadPostID is set (usually the approval-card root), the card includes
// an "Open ticket thread" markdown link. No-op when ApprovalTeam is unset.
func (p *Plugin) logAudit(kind auditOutcome, body, threadPostID string) {
	ch := p.ensureAuditChannel()
	if ch == nil {
		return
	}

	color, title := auditCardStyle(kind)
	text := strings.TrimSpace(body)
	if link := p.postPermalink(threadPostID); link != "" {
		text = text + "\n\n[Open ticket thread](" + link + ")"
	}

	// Leave Message empty — the attachment Title already carries Approved or
	// Denied. Duplicating it as the post body made every audit card show the
	// status twice.
	post := &model.Post{
		UserId:    p.botUserID,
		ChannelId: ch.Id,
	}
	model.ParseMessageAttachment(post, []*model.MessageAttachment{{
		Color: color,
		Title: title,
		Text:  text,
	}})
	if _, appErr := p.API.CreatePost(post); appErr != nil {
		p.API.LogWarn("audit post failed", "channel_id", ch.Id, "error", appErr.Error())
	}
}

func auditCardStyle(kind auditOutcome) (color, title string) {
	switch kind {
	case auditApproved:
		return colorApproved, "✅ Approved"
	case auditDenied:
		return colorDenied, "❌ Denied"
	default:
		return colorPending, "• Audit"
	}
}

// postPermalink builds a Mattermost /{team}/pl/{postID} URL for a post in a
// team channel. Returns "" when SiteURL is unset, the post/channel/team cannot
// be loaded, or the channel has no team (DM/GM).
func (p *Plugin) postPermalink(postID string) string {
	postID = strings.TrimSpace(postID)
	if postID == "" {
		return ""
	}
	base := strings.TrimRight(p.siteURL(), "/")
	if base == "" {
		return ""
	}
	post, appErr := p.API.GetPost(postID)
	if appErr != nil || post == nil || post.ChannelId == "" {
		return ""
	}
	ch, appErr := p.API.GetChannel(post.ChannelId)
	if appErr != nil || ch == nil || ch.TeamId == "" {
		return ""
	}
	team, appErr := p.API.GetTeam(ch.TeamId)
	if appErr != nil || team == nil || team.Name == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/pl/%s", base, team.Name, postID)
}

// createChannelForRequest creates the channel described by req, adds the requester and any
// designated members, promotes anyone in AdminMemberIDs to channel_admin, and returns the
// created channel. Add-and-promote failures on individual users are logged but don't fail
// the overall operation — a partially-populated channel is better than nothing.
func (p *Plugin) createChannelForRequest(req *channelRequest) (*model.Channel, error) {
	channel, appErr := p.API.CreateChannel(&model.Channel{
		TeamId:      req.TeamID,
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Type:        model.ChannelType(req.ChannelType),
		Purpose:     req.Purpose,
		CreatorId:   req.RequesterID,
	})
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to create channel")
	}

	// Union of everyone who should be a member: requester + regular + admin sets.
	// If no explicit Channel Admins were designated, the requester is promoted
	// automatically — a channel with no admin has nobody who can manage it.
	adminIDs := req.AdminMemberIDs
	if len(adminIDs) == 0 {
		adminIDs = []string{req.RequesterID}
	}
	adminSet := map[string]bool{}
	for _, uid := range adminIDs {
		if uid != "" {
			adminSet[uid] = true
		}
	}

	seen := map[string]bool{}
	addedOK := map[string]bool{}
	promotedOK := map[string]bool{}
	for _, userID := range append(append([]string{req.RequesterID}, req.MemberIDs...), req.AdminMemberIDs...) {
		if userID == "" || seen[userID] {
			continue
		}
		seen[userID] = true
		if _, appErr := p.API.AddChannelMember(channel.Id, userID); appErr != nil {
			p.API.LogWarn("failed to add member to created channel", "channel_id", channel.Id, "user_id", userID, "error", appErr.Error())
			continue
		}
		addedOK[userID] = true
		// Promote to channel admin if they were designated as such.
		// "channel_user channel_admin" is the classic role string;
		// MM handles the scheme-role mapping internally.
		if adminSet[userID] {
			if _, appErr := p.API.UpdateChannelMemberRoles(channel.Id, userID, channelAdminRoleString); appErr != nil {
				p.API.LogWarn("failed to promote member to channel admin",
					"channel_id", channel.Id, "user_id", userID, "error", appErr.Error())
				continue
			}
			promotedOK[userID] = true
		}
	}

	// Trim the request's lists to who was actually added (and, for admins,
	// actually promoted) so the welcome message doesn't claim users were
	// added when the API call failed — e.g. a user who isn't a team member
	// can't be added to the channel.
	req.MemberIDs = filterIDs(req.MemberIDs, addedOK)
	req.AdminMemberIDs = filterIDs(req.AdminMemberIDs, promotedOK)

	return channel, nil
}

// filterIDs returns the elements of ids present in keep, preserving order.
func filterIDs(ids []string, keep map[string]bool) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if keep[id] {
			out = append(out, id)
		}
	}
	return out
}

func (p *Plugin) storeRequest(req *channelRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal request")
	}
	if appErr := p.API.KVSet(kvRequestPrefix+req.ID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to store request")
	}
	return nil
}

// loadRequest returns the stored request and its raw KV bytes. The raw
// bytes let callers do an atomic KVCompareAndDelete against exactly what
// was read, rather than re-marshaling (which would silently stop matching
// if the struct ever gained a map field or the on-disk schema evolved).
func (p *Plugin) loadRequest(id string) (*channelRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req channelRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal request")
	}
	return &req, data, nil
}

// postApprovalRequest posts a channel-creation request (with Approve/Deny buttons) for an admin to
// act on. Returns the created post's ID and channel ID, which are empty in fallback mode (when the
// request is DM'd to system admins instead of posted to a channel).
func (p *Plugin) postApprovalRequest(req *channelRequest, requester *model.User) (postID, channelID string, err error) {
	approvalChannel, teamLabel := p.resolveApprovalChannel(req.TeamID)
	return p.postApprovalAttachment(
		req.ID,
		p.approvalAttachment(req, requester),
		teamLabel+"@channel — a new channel request needs your review.",
		approvalChannel,
	)
}

// postApprovalAttachment delivers an approval attachment to the reviewers. When approvalChannel is
// non-nil, the post goes there; otherwise it falls back to DMing every System Admin so requests are
// never silently dropped. Shared by the channel-creation and channel-admin request flows.
//
// Returns (postID, channelID, error). postID and channelID are empty in fallback mode — in that case
// the ticket messaging features (thread reply forwarding) are unavailable, but approve/deny notifications
// still reach the requester via a new DM root post. Every card posted is recorded under requestID so
// its buttons are recognized as the request's cards; if that record can't be saved, the cards are
// deleted again and the error is returned so the caller drops the request.
func (p *Plugin) postApprovalAttachment(requestID string, attachment *model.MessageAttachment, headerMessage string, approvalChannel *model.Channel) (postID, channelID string, err error) {
	var cardIDs []string
	if approvalChannel != nil {
		post := &model.Post{
			UserId:    p.botUserID,
			ChannelId: approvalChannel.Id,
			Message:   headerMessage,
		}
		model.ParseMessageAttachment(post, []*model.MessageAttachment{attachment})
		created, postErr := p.API.CreatePost(post)
		if postErr != nil {
			return "", "", errors.Wrap(postErr, "failed to post approval request")
		}
		postID, channelID = created.Id, approvalChannel.Id
		cardIDs = []string{created.Id}
	} else {
		cardIDs, err = p.postApprovalToSystemAdmins(attachment)
		if err != nil {
			return "", "", err
		}
	}

	if err := p.saveApprovalCards(requestID, cardIDs); err != nil {
		p.API.LogError("failed to record approval cards", "request_id", requestID, "error", err.Error())
		p.deleteApprovalCardPosts(cardIDs)
		return "", "", err
	}
	return postID, channelID, nil
}

// postApprovalToSystemAdmins DMs the approval request to every System Admin. Any admin can act on
// it; once one does, the others' copies resolve to an "already handled" message. Returns the IDs of
// the posts it created.
func (p *Plugin) postApprovalToSystemAdmins(attachment *model.MessageAttachment) ([]string, error) {
	admins, appErr := p.API.GetUsers(&model.UserGetOptions{Role: model.SystemAdminRoleId, Page: 0, PerPage: 100})
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to list System Admins")
	}

	var posted []string
	for _, admin := range admins {
		if admin.Id == p.botUserID {
			continue
		}
		dm, appErr := p.API.GetDirectChannel(admin.Id, p.botUserID)
		if appErr != nil {
			p.API.LogWarn("failed to open DM with System Admin", "user_id", admin.Id, "error", appErr.Error())
			continue
		}
		post := &model.Post{UserId: p.botUserID, ChannelId: dm.Id}
		model.ParseMessageAttachment(post, []*model.MessageAttachment{attachment})
		created, appErr := p.API.CreatePost(post)
		if appErr != nil {
			p.API.LogWarn("failed to DM approval request to System Admin", "user_id", admin.Id, "error", appErr.Error())
			continue
		}
		posted = append(posted, created.Id)
	}

	if len(posted) == 0 {
		return nil, errors.New("no System Admins are available to receive the approval request")
	}
	return posted, nil
}

// approvalAttachment builds the Slack attachment (with Approve/Deny buttons) describing a request.
// Team is first and full-width so approvers scanning root or per-team queues see which
// team the channel will live in without relying on the message preamble alone.
// Verbose details (purpose, members, admins) go in Text so Mattermost's native "Show more"
// collapses them on long requests — the approval channel stays scannable.
func (p *Plugin) approvalAttachment(req *channelRequest, requester *model.User) *model.MessageAttachment {
	visibility := "Public"
	if req.ChannelType == channelTypePrivate {
		visibility = "Private"
	}

	teamName := p.teamDisplayName(req.TeamID)
	fields := []*model.MessageAttachmentField{
		{Title: "Team", Value: escapeMarkdown(teamName), Short: false},
		{Title: "Requested by", Value: fmt.Sprintf("@%s", requester.Username), Short: true},
		{Title: "Visibility", Value: visibility, Short: true},
		{Title: "Channel name", Value: escapeMarkdown(req.DisplayName), Short: true},
		{Title: "URL", Value: req.Name, Short: true},
	}
	if req.CategoryName != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Sidebar category", Value: escapeMarkdown(req.CategoryName), Short: true})
	}

	var details []string
	if req.Purpose != "" {
		details = append(details, fmt.Sprintf("**Purpose:** %s", escapeMarkdown(req.Purpose)))
	}
	if len(req.MemberIDs) > 0 {
		details = append(details, fmt.Sprintf("**Members to add:** %s", p.mentionList(req.MemberIDs)))
	}
	if len(req.AdminMemberIDs) > 0 {
		details = append(details, fmt.Sprintf("**Channel Admins to add:** %s", p.mentionList(req.AdminMemberIDs)))
	}

	title := "Channel creation request"
	if teamName != "" && teamName != "(unknown team)" {
		title = fmt.Sprintf("Channel creation request — %s", teamName)
	}

	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   title,
		Color:   "#0058CC",
		Fields:  fields,
		Text:    strings.Join(details, "\n"),
		Actions: p.approvalActions(req.ID, siteURL),
	}
}

// teamDisplayName returns the team's display name for approval cards.
// Falls back to the team ID when lookup fails so the Team field is never blank.
func (p *Plugin) teamDisplayName(teamID string) string {
	if teamID == "" {
		return "(unknown team)"
	}
	team, appErr := p.API.GetTeam(teamID)
	if appErr != nil || team == nil {
		return teamID
	}
	if name := strings.TrimSpace(team.DisplayName); name != "" {
		return name
	}
	if name := strings.TrimSpace(team.Name); name != "" {
		return name
	}
	return teamID
}

func (p *Plugin) approvalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id:    "approve",
			Name:  "Approve",
			Type:  model.PostActionTypeButton,
			Style: "primary",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeApprove,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
		{
			Id:    "deny",
			Name:  "Deny",
			Type:  model.PostActionTypeButton,
			Style: "danger",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeDeny,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
	}
}

// addChannelToSidebarCategory places the newly-created channel in the
// requester's chosen sidebar category for all relevant users (requester +
// members + channel admins). Best-effort: failures are logged and don't
// block the overall approval flow.
func (p *Plugin) addChannelToSidebarCategory(channelID, teamID string, req *channelRequest) {
	if req.CategoryName == "" {
		return
	}
	seen := map[string]bool{}
	for _, uid := range append([]string{req.RequesterID}, append(req.MemberIDs, req.AdminMemberIDs...)...) {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		if err := p.placeChannelInCategory(uid, teamID, channelID, req.CategoryName); err != nil {
			p.API.LogWarn("failed to place channel in sidebar category",
				"user_id", uid, "category", req.CategoryName, "error", fmt.Sprintf("%v", err))
		}
	}
}

// placeChannelInCategory adds channelID to the named sidebar category for a
// single user. Match is case-insensitive against existing custom/display names.
// If nothing matches, a new custom category is created with that display name
// (so requesters can type a new bucket in the same form field).
//
// Never return a typed-nil *model.AppError as error — that interfaces as
// non-nil and panics on .Error() (which broke Approve squash after create).
func (p *Plugin) placeChannelInCategory(userID, teamID, channelID, categoryName string) error {
	categoryName = strings.TrimSpace(categoryName)
	if categoryName == "" {
		return nil
	}
	cats, appErr := p.API.GetChannelSidebarCategories(userID, teamID)
	if appErr != nil {
		// Never return a typed-nil *model.AppError as error — interfaces as non-nil.
		return errors.New(appErr.Error())
	}
	if cats != nil {
		for _, cat := range cats.Categories {
			if cat == nil || !strings.EqualFold(cat.DisplayName, categoryName) {
				continue
			}
			if slices.Contains(cat.Channels, channelID) {
				return nil // already in this category
			}
			cat.Channels = append(cat.Channels, channelID)
			if _, updateErr := p.API.UpdateChannelSidebarCategories(userID, teamID, []*model.SidebarCategoryWithChannels{cat}); updateErr != nil {
				return errors.New(updateErr.Error())
			}
			return nil
		}
	}

	if _, createErr := p.API.CreateChannelSidebarCategory(userID, teamID, &model.SidebarCategoryWithChannels{
		SidebarCategory: model.SidebarCategory{
			UserId:      userID,
			TeamId:      teamID,
			DisplayName: categoryName,
			Type:        model.SidebarCategoryCustom,
		},
		Channels: []string{channelID},
	}); createErr != nil {
		return errors.New(createErr.Error())
	}
	return nil
}

// mentionList renders a list of user IDs as @mentions for display.
func (p *Plugin) mentionList(userIDs []string) string {
	mentions := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		if user, appErr := p.API.GetUser(id); appErr == nil {
			mentions = append(mentions, "@"+user.Username)
		}
	}
	return strings.Join(mentions, ", ")
}
