package main

import (
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
)

// ticketThread is the shared messaging identity of a pending request: the
// approval card post and the requester's DM ticket root. All request types
// register one of these so MessageHasBeenPosted can relay replies both ways
// (and keep doing so after the ticket closes).
type ticketThread struct {
	ApprovalPostID    string
	ApprovalChannelID string
	DMRootPostID      string
	DMChannelID       string
	Kind              string // requestType* constant
	ScopeID           string // optional: team ID (channel create) or channel ID (channel-admin)
}

func (t ticketThread) toAnchor() threadAnchor {
	return threadAnchor(t)
}

// registerTicketThread stores the reverse lookups used by the relay. Best-effort.
func (p *Plugin) registerTicketThread(t ticketThread) {
	p.storeTicketLookups(t.toAnchor())
}

// refreshTicketThread re-writes the lookups before claim/approve so a failed
// store at submit time does not leave a dead relay.
func (p *Plugin) refreshTicketThread(t ticketThread) {
	p.registerTicketThread(t)
}

// ticketPersist CAS-writes approval/DM post IDs onto the pending KV record and
// registers the messaging thread when the write wins.
type ticketPersist struct {
	KVKey     string
	RawBefore []byte
	Record    any
	RequestID string
	Label     string // e.g. "channel request" — used in warn logs
	Thread    ticketThread
}

// persistTicketPosts re-stores the request with post IDs filled in. Best-effort:
// if the CAS loses or errors, approve/deny still work because the approval
// cards were recorded when they were posted; thread relay may not.
func (p *Plugin) persistTicketPosts(tp ticketPersist) {
	ok, casErr := p.kvUpdateIfUnchanged(tp.KVKey, tp.RawBefore, tp.Record)
	if casErr != nil {
		p.API.LogWarn("failed to re-store "+tp.Label+" with post IDs", "request_id", tp.RequestID, "error", casErr.Error())
		return
	}
	if !ok {
		p.API.LogWarn(tp.Label+" already claimed before post IDs were written", "request_id", tp.RequestID)
		return
	}
	p.registerTicketThread(tp.Thread)
}

// ticketProgress is a mid-lifecycle DM card update (e.g. one of two approvals).
type ticketProgress struct {
	RequesterID  string
	DMRootPostID string
	DMChannelID  string
	Title        string
	Text         string
}

// notifyTicketProgress updates the requester's pending DM card without closing
// the ticket (two-step partial approvals).
func (p *Plugin) notifyTicketProgress(tp ticketProgress) {
	p.notifyRequesterInThreadCard(tp.RequesterID, tp.DMRootPostID, tp.DMChannelID, &model.MessageAttachment{
		Color: colorPending,
		Title: tp.Title,
		Text:  tp.Text,
	})
}

// postTicketCreatedDM opens the bot↔requester DM and posts the pending card as
// the ticket thread root (no Approve/Deny buttons). Returns empty IDs on
// failure — callers treat that as non-fatal.
func (p *Plugin) postTicketCreatedDM(requesterID string, card *model.MessageAttachment, intro string) (dmRootPostID, dmChannelID string) {
	if requesterID == "" || card == nil {
		return "", ""
	}
	dm, appErr := p.API.GetDirectChannel(requesterID, p.botUserID)
	if appErr != nil {
		p.API.LogWarn("failed to open DM for ticket notification", "user_id", requesterID, "error", appErr.Error())
		return "", ""
	}

	attachment := *card
	attachment.Actions = nil
	attachment.Color = colorPending

	if intro == "" {
		intro = "Your request has been submitted for approval. Reply to this thread to add context — your reply will be forwarded to the reviewers."
	}

	post := &model.Post{
		UserId:    p.botUserID,
		ChannelId: dm.Id,
		Message:   intro,
	}
	model.ParseMessageAttachment(post, []*model.MessageAttachment{&attachment})

	created, createErr := p.API.CreatePost(post)
	if createErr != nil {
		p.API.LogWarn("failed to send ticket-created DM", "user_id", requesterID, "error", createErr.Error())
		return "", ""
	}
	return created.Id, dm.Id
}

// ticketClose is the shared approve/deny close sequence for every request type:
// update the requester DM card, post the outcome thread line, squash the
// approval card (archiving details in-thread).
type ticketClose struct {
	RequestID        string // the request whose recorded approval cards are cleared
	RequesterID      string
	DMRootPostID     string
	DMChannelID      string
	ApprovalPostID   string
	Approved         bool
	ApproverUsername string
	CardTitle        string
	CardText         string
	StatusLine       string // one-line tombstone on the approval post

	// Expired closes the ticket without a decision (request expired before
	// review). ThreadReply then replaces the Approved/Denied thread line.
	Expired     bool
	ThreadReply string
}

// closeTicket runs the shared close path and returns the Update payload for the
// PostActionIntegrationResponse.
func (p *Plugin) closeTicket(c ticketClose) *model.Post {
	color := colorDenied
	switch {
	case c.Expired:
		color = colorExpired
	case c.Approved:
		color = colorApproved
	}
	p.notifyRequesterInThreadCard(c.RequesterID, c.DMRootPostID, c.DMChannelID, &model.MessageAttachment{
		Color: color,
		Title: c.CardTitle,
		Text:  c.CardText,
	})
	if c.Expired {
		p.postTicketThreadReply(c.DMChannelID, c.DMRootPostID, c.ThreadReply)
	} else {
		p.postOutcomeThreadReply(c.DMChannelID, c.DMRootPostID, c.Approved, c.ApproverUsername)
	}
	p.deleteApprovalCards(c.RequestID)
	return p.resolvedPost(c.ApprovalPostID, c.StatusLine)
}

// ticketSubmittedIntro builds the default DM root message for a named request kind.
func ticketSubmittedIntro(kindLabel string) string {
	if kindLabel == "" {
		kindLabel = "request"
	}
	return fmt.Sprintf(
		"Your %s has been submitted for approval. Reply to this thread to add context — your reply will be forwarded to the reviewers.",
		kindLabel,
	)
}
