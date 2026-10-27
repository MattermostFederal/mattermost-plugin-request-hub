package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

const (
	// kvDMRootPrefix namespaces the reverse-lookup from a DM root post ID to a
	// threadAnchor JSON blob. Stored as dm_root_{postID} → JSON.
	kvDMRootPrefix = "dm_root_"

	// kvApprovalPostPrefix namespaces the reverse-lookup from an approval post ID
	// to a threadAnchor JSON blob. Stored as approval_post_{postID} → JSON.
	kvApprovalPostPrefix = "approval_post_"

	// colorPending is the sidebar color for the initial "ticket submitted" DM card.
	// colorApproved and colorDenied are used when the root DM card is updated on outcome.
	colorPending  = "#E8A838"
	colorApproved = "#2DB887"
	colorDenied   = "#D9534F"
)

// threadAnchor holds all four post IDs needed to relay messages between a
// requester's DM thread and the admin's approval thread. Stored as JSON in two
// KV entries (keyed by approvalPostID and dmRootPostID) so the relay works
// without loading the full request record. KV entries are never deleted —
// threads remain active after approval or denial for follow-up conversation.
type threadAnchor struct {
	ApprovalPostID    string `json:"ap"`
	ApprovalChannelID string `json:"ac"`
	DMRootPostID      string `json:"dp"`
	DMChannelID       string `json:"dc"`
	// Kind is the request type constant (channel, bot, …). Empty on legacy
	// anchors written before this field existed.
	Kind string `json:"k,omitempty"`
	// ScopeID is the channel ID for channel-admin tickets so Team Admins of
	// that channel's team can relay. Unused for other kinds.
	ScopeID string `json:"s,omitempty"`
}

// storeTicketLookups writes the two reverse-lookup KV entries that let
// MessageHasBeenPosted route replies between the requester's DM thread and the
// admin's approval thread. Best-effort — failures are logged but don't block
// ticket creation.
func (p *Plugin) storeTicketLookups(anchor threadAnchor) {
	data, err := json.Marshal(anchor)
	if err != nil {
		p.API.LogWarn("failed to marshal thread anchor", "error", err.Error())
		return
	}
	if anchor.ApprovalPostID != "" {
		if appErr := p.API.KVSet(kvApprovalPostPrefix+anchor.ApprovalPostID, data); appErr != nil {
			p.API.LogWarn("failed to store approval-post lookup", "post_id", anchor.ApprovalPostID, "error", appErr.Error())
		}
	}
	if anchor.DMRootPostID != "" {
		if appErr := p.API.KVSet(kvDMRootPrefix+anchor.DMRootPostID, data); appErr != nil {
			p.API.LogWarn("failed to store DM-root lookup", "post_id", anchor.DMRootPostID, "error", appErr.Error())
		}
	}
}

// MessageHasBeenPosted intercepts every new post and:
//   - Forwards DM thread replies from requesters to their ticket's approval thread.
//   - Forwards approval thread replies from reviewers to the requester's DM thread.
//   - Prompts users who DM the bot outside of any ticket thread.
func (p *Plugin) MessageHasBeenPosted(_ *plugin.Context, post *model.Post) {
	// Never react to our own posts — would create an infinite forwarding loop.
	if post.UserId == p.botUserID {
		return
	}

	channel, appErr := p.API.GetChannel(post.ChannelId)
	if appErr != nil {
		return
	}

	if channel.Type == model.ChannelTypeDirect {
		p.handleIncomingDMPost(post)
		return
	}

	// Only care about thread replies in non-DM channels (reviewers commenting in ticket threads).
	if post.RootId != "" {
		p.handleIncomingThreadReply(post)
	}
}

// handleIncomingDMPost handles a post that arrived in a DM channel.
//   - Ignores DMs that don't involve the bot.
//   - Root-level DMs (not a reply to any ticket thread) → help nudge.
//   - Thread replies to a known ticket DM thread → forwarded to the approval thread.
func (p *Plugin) handleIncomingDMPost(post *model.Post) {
	// Only act on DMs where our bot is a member.
	if _, appErr := p.API.GetChannelMember(post.ChannelId, p.botUserID); appErr != nil {
		return
	}

	// Root-level DM (not a reply to a ticket thread) → helpful nudge.
	if post.RootId == "" {
		_, _ = p.API.CreatePost(&model.Post{
			UserId:    p.botUserID,
			ChannelId: post.ChannelId,
			Message:   "Please reply to a specific ticket thread above to add information to that request.",
		})
		return
	}

	// Check whether this is a reply inside a known ticket DM thread.
	rawAnchor, appErr := p.API.KVGet(kvDMRootPrefix + post.RootId)
	if appErr != nil || rawAnchor == nil {
		return
	}

	p.forwardDMReplyToTicketThread(post, rawAnchor)
}

// handleIncomingThreadReply handles a non-DM thread reply. If the root post
// is a known ticket approval post, it forwards the message to the requester's
// DM thread.
func (p *Plugin) handleIncomingThreadReply(post *model.Post) {
	rawAnchor, appErr := p.API.KVGet(kvApprovalPostPrefix + post.RootId)
	if appErr != nil || rawAnchor == nil {
		return
	}

	p.forwardThreadReplyToRequesterDM(post, rawAnchor)
}

// forwardDMReplyToTicketThread posts the requester's DM reply as a bot message
// in the approval thread, attributed to the requester by name.
func (p *Plugin) forwardDMReplyToTicketThread(post *model.Post, rawAnchor []byte) {
	var anchor threadAnchor
	if err := json.Unmarshal(rawAnchor, &anchor); err != nil || anchor.ApprovalPostID == "" {
		return
	}

	poster, appErr := p.API.GetUser(post.UserId)
	if appErr != nil {
		return
	}

	_, _ = p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: anchor.ApprovalChannelID,
		RootId:    anchor.ApprovalPostID,
		Message:   fmt.Sprintf("**Reply from @%s:**\n%s", poster.Username, quotedText(post.Message)),
	})
}

// quotedText renders a requester's message as typed, as a blockquote, so it
// reads as their words and not as the plugin's own status lines.
func quotedText(msg string) string {
	return "> " + strings.ReplaceAll(escapeMarkdown(msg), "\n", "\n> ")
}

// forwardThreadReplyToRequesterDM posts the reviewer's approval thread reply
// as a bot message in the requester's DM ticket thread. Only eligible
// approvers/reviewers for that ticket kind are relayed — other posts in the
// approval channel stay local so non-approvers cannot impersonate reviewers.
func (p *Plugin) forwardThreadReplyToRequesterDM(post *model.Post, rawAnchor []byte) {
	var anchor threadAnchor
	if err := json.Unmarshal(rawAnchor, &anchor); err != nil || anchor.DMRootPostID == "" {
		return
	}

	poster, appErr := p.API.GetUser(post.UserId)
	if appErr != nil {
		return
	}
	if !p.canRelayApprovalThreadReply(poster, anchor) {
		return
	}

	_, _ = p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: anchor.DMChannelID,
		RootId:    anchor.DMRootPostID,
		Message:   fmt.Sprintf("**Message from @%s:** %s", poster.Username, post.Message),
	})
}

// canRelayApprovalThreadReply mirrors Approve-button authz for the ticket kind
// stored on the anchor. Legacy anchors without Kind fall back to any reviewer
// pool (channel approvers, System Admin, or two-step attribute holders).
func (p *Plugin) canRelayApprovalThreadReply(user *model.User, anchor threadAnchor) bool {
	if user == nil {
		return false
	}
	switch anchor.Kind {
	case requestTypeChannel:
		// ScopeID holds the team the channel will be created in (per-team cards).
		if anchor.ScopeID != "" {
			return p.canApproveChannelRequest(user, anchor.ScopeID)
		}
		return p.canApprove(user)
	case requestTypeChannelAdmin:
		return p.canApproveAdminRequest(user, anchor.ScopeID)
	case requestTypeTeam, requestTypeTeamAdmin, requestTypeTeamMember, requestTypeTeamGuest:
		return user.IsSystemAdmin()
	case requestTypeBot, requestTypeWebhook:
		// System Admins can always ask clarifying questions in the thread,
		// even when they aren't in the security-attribute pool (they still
		// can't fake a security Approve click — that's gated separately).
		if user.IsSystemAdmin() {
			return true
		}
		return p.canRelayTwoStepApprover(user)
	default:
		if user.IsSystemAdmin() || p.canApprove(user) {
			return true
		}
		return p.canRelayTwoStepApprover(user)
	}
}

// canRelayTwoStepApprover is true when the user is eligible for either
// security or system approval. A lookup error on one step must not block
// the other — otherwise a misconfigured security attribute would silence
// System / attribute-based system approvers in the thread.
func (p *Plugin) canRelayTwoStepApprover(user *model.User) bool {
	canSec, errSec := p.canApproveStep(user, stepSecurity)
	if errSec == nil && canSec {
		return true
	}
	canSys, errSys := p.canApproveStep(user, stepSystem)
	return errSys == nil && canSys
}

// notifyRequesterInThreadCard updates the root DM post (the yellow "pending" card)
// in-place: swaps color, title, and text to reflect the outcome while preserving
// the original request detail fields (Requested by, Channel name, etc.).
// Falls back to a thread reply if the update fails, or a new root DM when no
// thread was created (e.g. no approval channel configured).
func (p *Plugin) notifyRequesterInThreadCard(requesterID, dmRootPostID, dmChannelID string, outcome *model.MessageAttachment) {
	if dmRootPostID != "" {
		if p.updateDMRootCard(dmRootPostID, outcome) {
			return
		}
		// UpdatePost failed — fall back to a thread reply so the requester still
		// sees the outcome.
		if dmChannelID != "" {
			post := &model.Post{UserId: p.botUserID, ChannelId: dmChannelID, RootId: dmRootPostID}
			model.ParseMessageAttachment(post, []*model.MessageAttachment{outcome})
			if _, appErr := p.API.CreatePost(post); appErr != nil {
				p.API.LogWarn("failed to send fallback outcome reply", "user_id", requesterID, "error", appErr.Error())
			}
			return
		}
	}
	p.notifyRequesterCard(requesterID, outcome)
}

// updateDMRootCard fetches the root DM post and updates its attachment color,
// title, and text to reflect the request outcome, preserving the original
// detail fields. Returns true on success.
func (p *Plugin) updateDMRootCard(postID string, outcome *model.MessageAttachment) bool {
	post, appErr := p.API.GetPost(postID)
	if appErr != nil {
		p.API.LogWarn("failed to fetch DM root post for outcome update", "post_id", postID, "error", appErr.Error())
		return false
	}

	attachment := extractFirstAttachment(post)
	if attachment == nil {
		attachment = outcome
	} else {
		attachment.Color = outcome.Color
		attachment.Title = outcome.Title
		attachment.Text = outcome.Text
		attachment.Actions = nil
	}

	model.ParseMessageAttachment(post, []*model.MessageAttachment{attachment})
	if _, updateErr := p.API.UpdatePost(post); updateErr != nil {
		p.API.LogWarn("failed to update DM root post with outcome", "post_id", postID, "error", updateErr.Error())
		return false
	}
	return true
}

// extractFirstAttachment deserializes the first message attachment from a
// post's Props map. Returns nil if none is present.
func extractFirstAttachment(post *model.Post) *model.MessageAttachment {
	if post.Props == nil {
		return nil
	}
	raw, ok := post.Props["attachments"]
	if !ok {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var attachments []*model.MessageAttachment
	if err := json.Unmarshal(data, &attachments); err != nil || len(attachments) == 0 {
		return nil
	}
	return attachments[0]
}

// postOutcomeThreadReply posts a plain-text thread reply in the requester's DM
// ticket thread so the approver's identity is visible directly in the thread,
// in addition to the colour change on the root card. No-ops when the DM thread
// IDs are missing (e.g. no approval channel was configured).
func (p *Plugin) postOutcomeThreadReply(dmChannelID, dmRootPostID string, approved bool, approverUsername string) {
	if dmChannelID == "" || dmRootPostID == "" {
		return
	}
	msg := fmt.Sprintf("❌ **Denied** by @%s.", approverUsername)
	if approved {
		msg = fmt.Sprintf("✅ **Approved** by @%s.", approverUsername)
	}
	if _, appErr := p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: dmChannelID,
		RootId:    dmRootPostID,
		Message:   msg,
	}); appErr != nil {
		p.API.LogWarn("failed to post outcome thread reply", "channel_id", dmChannelID, "root_id", dmRootPostID, "error", appErr.Error())
	}
}

// notifyRequesterCard opens a fresh root DM with the given card attachment.
// Used as a fallback when no ticket thread was created.
func (p *Plugin) notifyRequesterCard(requesterID string, attachment *model.MessageAttachment) {
	channel, appErr := p.API.GetDirectChannel(requesterID, p.botUserID)
	if appErr != nil {
		p.API.LogWarn("failed to open DM for card notification", "user_id", requesterID, "error", appErr.Error())
		return
	}
	post := &model.Post{UserId: p.botUserID, ChannelId: channel.Id}
	model.ParseMessageAttachment(post, []*model.MessageAttachment{attachment})
	if _, appErr := p.API.CreatePost(post); appErr != nil {
		p.API.LogWarn("failed to send card DM to requester", "user_id", requesterID, "error", appErr.Error())
	}
}
