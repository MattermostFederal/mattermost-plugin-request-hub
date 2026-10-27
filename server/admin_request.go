package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	// kvAdminRequestPrefix namespaces pending channel-admin requests in the
	// KV store, kept distinct from kvRequestPrefix (channel-creation
	// requests) so the two request types never collide.
	kvAdminRequestPrefix = "admin_request_"
)

// adminRequest is a pending request to promote one or more existing users to
// Channel Admin on an already-created channel. Persisted in the KV store
// until a reviewer approves or denies it. This is the counterpart to
// channelRequest, which is about creating brand-new channels.
type adminRequest struct {
	ID          string `json:"id"`
	RequesterID string `json:"requester_id"`
	ChannelID   string `json:"channel_id"`
	// NomineeIDs are the users the requester wants promoted to Channel
	// Admin. They must already be channel members; on approval, those who
	// still are get the channel_admin scheme role. Nobody is added to the
	// channel by this flow.
	NomineeIDs []string `json:"nominee_ids"`

	// Ticket messaging fields — set after the approval post and DM are created.
	// Used by the MessageHasBeenPosted hook to route thread replies in both directions.
	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`
}

// submitAdminRequest validates the input, stores a pending request and posts
// it to the approval channel. Requests from System Admins queue too.
func (p *Plugin) submitAdminRequest(requesterID, channelID string, nomineeIDs []string) (string, error) {
	if strings.TrimSpace(channelID) == "" {
		return "", errors.New("a channel is required")
	}

	nominees := dedupeNonEmpty(nomineeIDs)
	if len(nominees) == 0 {
		return "", errors.New("pick at least one person to make a Channel Admin")
	}
	if err := checkIDList(nominees, "Channel Admin nominees"); err != nil {
		return "", err
	}

	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil {
		p.API.LogError("failed to load requesting user", "user_id", requesterID, "error", appErr.Error())
		return "", errors.New("could not load your account; try again")
	}

	// The requester must belong to the channel they're nominating admins for.
	// The UI only offers channels the user is in, but that's a client-side
	// convenience — a crafted request could name any channel ID, so enforce
	// membership on the server. System Admins manage every channel and are
	// exempt. GetChannelMember returns an error for non-members (channel
	// memberships are hard-deleted on leave, so no soft-delete check needed).
	// Membership is checked first, and both failures share one message.
	const noChannelMsg = "channel not found, or you are not a member of it"
	if !requester.IsSystemAdmin() {
		if _, appErr = p.API.GetChannelMember(channelID, requesterID); appErr != nil {
			return "", errors.New(noChannelMsg)
		}
	}
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil || channel == nil {
		return "", errors.New(noChannelMsg)
	}
	if channel.Type != model.ChannelTypeOpen && channel.Type != model.ChannelTypePrivate {
		return "", errors.New("Channel Admins can only be requested for public or private channels")
	}
	if channel.DeleteAt != 0 {
		return "", errors.New("that channel is archived")
	}

	// Nominees must already be members of the channel.
	for _, id := range nominees {
		if _, appErr = p.API.GetChannelMember(channelID, id); appErr != nil {
			return "", errors.New("everyone you nominate must already be a member of the channel")
		}
	}

	req := &adminRequest{
		ID:          model.NewId(),
		RequesterID: requesterID,
		ChannelID:   channelID,
		NomineeIDs:  nominees,
	}

	// Store before posting so approve/deny handlers can find the request immediately.
	if err := p.storeAdminRequest(req); err != nil {
		p.API.LogError("failed to store Channel Admin request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	rawBefore, err := json.Marshal(req)
	if err != nil {
		_ = p.API.KVDelete(kvAdminRequestPrefix + req.ID)
		return "", errors.Wrap(err, "failed to snapshot admin request")
	}

	approvalPostID, approvalChannelID, err := p.postAdminApprovalRequest(req, requester, channel)
	if err != nil {
		// Roll back so the stored request isn't orphaned without an approval message.
		_ = p.API.KVDelete(kvAdminRequestPrefix + req.ID)
		p.API.LogError("failed to post Channel Admin request for approval", "error", err.Error())
		return "", errors.New("could not send your request to the approvers; try again or contact a System Admin")
	}

	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	dmRootPostID, dmChannelID := p.sendAdminTicketCreatedDM(req, requester, channel)
	req.DMRootPostID = dmRootPostID
	req.DMChannelID = dmChannelID

	p.persistTicketPosts(ticketPersist{
		KVKey: kvAdminRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "admin request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
			Kind: requestTypeChannelAdmin, ScopeID: channelID,
		},
	})

	return "Your Channel Admin request has been submitted for approval. You'll be notified once an admin responds.", nil
}

func (p *Plugin) sendAdminTicketCreatedDM(req *adminRequest, requester *model.User, channel *model.Channel) (dmRootPostID, dmChannelID string) {
	return p.postTicketCreatedDM(
		req.RequesterID,
		p.adminApprovalAttachment(req, requester, channel),
		ticketSubmittedIntro("Channel Admin request"),
	)
}

// promoteChannelAdmins grants the channel_admin role to each nominee who is
// still a member of the channel and returns the IDs actually promoted.
// Membership is re-checked because a request can sit pending while people
// leave or are removed; nobody is added to the channel. Per-user failures
// are logged but don't abort the others — a partial promotion is better than
// none, and callers report only who was promoted.
func (p *Plugin) promoteChannelAdmins(req *adminRequest) []string {
	promoted := make([]string, 0, len(req.NomineeIDs))
	for _, userID := range req.NomineeIDs {
		if userID == "" {
			continue
		}
		if _, appErr := p.API.GetChannelMember(req.ChannelID, userID); appErr != nil {
			p.API.LogWarn("skipping nominee who is not a channel member", "channel_id", req.ChannelID, "user_id", userID)
			continue
		}
		if _, appErr := p.API.UpdateChannelMemberRoles(req.ChannelID, userID, channelAdminRoleString); appErr != nil {
			p.API.LogWarn("failed to promote nominee to channel admin", "channel_id", req.ChannelID, "user_id", userID, "error", appErr.Error())
			continue
		}
		promoted = append(promoted, userID)
	}
	return promoted
}

// postAdminPromotionMessage posts a bot message in the channel announcing the
// newly-promoted Channel Admins so the channel sees who can now manage it.
func (p *Plugin) postAdminPromotionMessage(channel *model.Channel, req *adminRequest, approver *model.User) {
	nominees := p.mentionList(req.NomineeIDs)
	if nominees == "" {
		return
	}
	body := nominees + " is now a Channel Admin for this channel."
	if approver != nil {
		body += fmt.Sprintf(" (Approved by @%s.)", approver.Username)
	}
	if _, appErr := p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: channel.Id,
		Message:   body,
	}); appErr != nil {
		p.API.LogWarn("failed to post channel-admin promotion message", "channel_id", channel.Id, "error", appErr.Error())
	}
}

func (p *Plugin) storeAdminRequest(req *adminRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal admin request")
	}
	if appErr := p.API.KVSet(kvAdminRequestPrefix+req.ID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to store admin request")
	}
	return nil
}

// loadAdminRequest returns the stored request and its raw KV bytes. The raw
// bytes let handleAdminAction do an atomic KVCompareAndDelete against exactly
// what was read, so two reviewers acting on the same request at once can't both
// process it — mirrors loadRequest in the channel-creation flow.
func (p *Plugin) loadAdminRequest(id string) (*adminRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvAdminRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load admin request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req adminRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal admin request")
	}
	return &req, data, nil
}

// postAdminApprovalRequest delivers a channel-admin request (with Approve/Deny
// buttons) to the reviewers. It reuses postApprovalAttachment, which posts to
// the resolved approval channel and falls back to DMing every System Admin when
// none is available, exactly as the channel-creation flow does. Returns the
// created post ID and channel ID; both empty in fallback mode.
func (p *Plugin) postAdminApprovalRequest(req *adminRequest, requester *model.User, channel *model.Channel) (postID, channelID string, err error) {
	approvalChannel, teamLabel := p.resolveApprovalChannel(channel.TeamId)
	return p.postApprovalAttachment(
		req.ID,
		p.adminApprovalAttachment(req, requester, channel),
		teamLabel+p.adminApprovalHeader(channel),
		approvalChannel,
	)
}

// adminApprovalHeader builds the message that leads the approval post,
// @-mentioning the users who can approve it so they're notified.
func (p *Plugin) adminApprovalHeader(channel *model.Channel) string {
	mentions := p.mentionList(p.adminApproverIDs(channel.TeamId))
	if mentions == "" {
		return "A Channel Admin request needs your review."
	}
	return mentions + " — a Channel Admin request needs your review."
}

// adminApproverIDs returns the user IDs authorized to approve a channel-admin
// request: every System Admin plus the Team Admins of the target channel's
// team. Deduped, bot excluded. Best-effort — lookup failures are logged and
// skipped rather than aborting the request.
func (p *Plugin) adminApproverIDs(teamID string) []string {
	seen := make(map[string]bool)
	var ids []string
	add := func(id string) {
		if id == "" || id == p.botUserID || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}

	sysAdmins, appErr := p.API.GetUsers(&model.UserGetOptions{Role: model.SystemAdminRoleId, Page: 0, PerPage: 100})
	if appErr != nil {
		p.API.LogWarn("failed to list System Admins for admin-request approvers", "error", appErr.Error())
	} else {
		for _, u := range sysAdmins {
			add(u.Id)
		}
	}

	if teamID != "" {
		const perPage = 200
		for page := range 50 {
			members, appErr := p.API.GetTeamMembers(teamID, page, perPage)
			if appErr != nil {
				p.API.LogWarn("failed to list Team Admins for admin-request approvers", "team_id", teamID, "error", appErr.Error())
				break
			}
			if len(members) == 0 {
				break
			}
			for _, m := range members {
				if m.SchemeAdmin || slices.Contains(strings.Fields(m.Roles), model.TeamAdminRoleId) {
					add(m.UserId)
				}
			}
			if len(members) < perPage {
				break
			}
		}
	}
	return ids
}

// adminApprovalAttachment builds the Slack attachment (with Approve/Deny
// buttons) describing a channel-admin request. Core fields (requester +
// channel) are always visible; the nominee list goes in Text so a large
// nomination doesn't dominate the card.
func (p *Plugin) adminApprovalAttachment(req *adminRequest, requester *model.User, channel *model.Channel) *model.MessageAttachment {
	teamName := p.teamDisplayName(channel.TeamId)
	fields := []*model.MessageAttachmentField{
		{Title: "Team", Value: escapeMarkdown(teamName), Short: false},
		{Title: "Requested by", Value: fmt.Sprintf("@%s", requester.Username), Short: true},
		{Title: "Channel", Value: escapeMarkdown(fmt.Sprintf("%s (%s)", channel.DisplayName, channel.Name)), Short: true},
	}

	title := "Channel Admin request"
	if teamName != "" && teamName != "(unknown team)" {
		title = fmt.Sprintf("Channel Admin request — %s", teamName)
	}

	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   title,
		Color:   "#0058CC",
		Fields:  fields,
		Text:    fmt.Sprintf("**Proposed Channel Admins:** %s", p.mentionList(req.NomineeIDs)),
		Actions: p.adminApprovalActions(req.ID, siteURL),
	}
}

func (p *Plugin) adminApprovalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id:    "approve",
			Name:  "Approve",
			Type:  model.PostActionTypeButton,
			Style: "primary",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeApproveAdmin,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
		{
			Id:    "deny",
			Name:  "Deny",
			Type:  model.PostActionTypeButton,
			Style: "danger",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeDenyAdmin,
				Context: map[string]any{actionContextRequestID: requestID},
			},
		},
	}
}

// dedupeNonEmpty returns the input with empty strings and duplicates removed,
// preserving first-seen order.
func dedupeNonEmpty(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
