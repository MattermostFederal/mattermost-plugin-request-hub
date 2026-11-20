package main

import (
	"encoding/json"
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
)

// ticketPostIDs are the messaging fields every pending request JSON carries.
type ticketPostIDs struct {
	ApprovalPostID string `json:"approval_post_id"`
	DMRootPostID   string `json:"dm_root_post_id"`
	DMChannelID    string `json:"dm_channel_id"`
}

// withdrawAction is the requester-only button on the ticket DM card.
func (p *Plugin) withdrawAction(requestID string) *model.PostAction {
	if requestID == "" {
		return nil
	}
	siteURL := "/plugins/" + manifest.Id
	return &model.PostAction{
		Id:    "withdraw",
		Name:  "Withdraw",
		Type:  model.PostActionTypeButton,
		Style: "danger",
		Integration: &model.PostActionIntegration{
			URL:     siteURL + routeWithdraw,
			Context: map[string]any{actionContextRequestID: requestID},
		},
	}
}

// decideWithdraw closes a pending request on behalf of its requester. Allowed
// until a final approve/deny claims the record — including after a security
// step on two-step tickets. Approvers still deny when they want to refuse.
func (p *Plugin) decideWithdraw(actingUserID, requestID, clickedPostID string) decisionResult {
	if !model.IsValidId(requestID) {
		return decisionResult{Reply: alreadyHandledText}
	}
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil || actingUser == nil {
		return decisionResult{Reply: "Could not verify your identity to withdraw."}
	}

	kind, rec, raw, err := p.findPendingRequest(requestID)
	if err != nil {
		p.API.LogError("failed to look up request for withdraw", "request_id", requestID, "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if rec == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	requesterID := recordRequesterID(rec)
	if requesterID != actingUserID {
		return decisionResult{Reply: "Only the person who submitted this request can withdraw it."}
	}
	// The ticket is the only pointer to a handover Mattermost has already
	// started: a replacement hook that exists, or a bot whose ownership may
	// have moved. Withdrawing it would strand either one.
	switch r := rec.(type) {
	case *webhookTransferRequest:
		if r.cutoverInProgress() {
			return decisionResult{Reply: "The replacement webhook for this request already exists, so it can't be withdrawn — an approver has to finish the handover."}
		}
	case *botTransferRequest:
		if r.ResumeAfterAssign {
			return decisionResult{Reply: resumeAfterAssignWithdrawBlockedMsg}
		}
		// An ambiguous AssignBot can land after the owner read that
		// followed it, so the bot is looked at once more before the only
		// record of the attempt goes.
		if r.AssignAttempted {
			if _, msg := p.checkBotTransferReconciled(r); msg != "" {
				return decisionResult{Reply: resumeAfterAssignWithdrawBlockedMsg}
			}
		}
	}
	var posts ticketPostIDs
	_ = json.Unmarshal(raw, &posts)
	// Withdraw lives on the ticket DM root; refuse spoofed clicks, including
	// when the ticket records no DM root to compare against.
	if clickedPostID != "" && (posts.DMRootPostID == "" || clickedPostID != posts.DMRootPostID) {
		return decisionResult{Reply: buttonNotOnCardText}
	}

	// Resolve approval-channel cards (not the DM root). Slash and DM use the
	// same finish path; the DM Update is filled after close when clicked.
	cards := newDecisionCards(requestID, "", posts.ApprovalPostID)
	var flags struct {
		Exercise bool `json:"exercise"`
	}
	_ = json.Unmarshal(raw, &flags)
	cards = cards.withSubject(kind, requesterID, actingUser, rec, flags.Exercise)

	if res, ok := p.claimRecord(pendingKinds[kind].kvPrefix+requestID, raw, cards, "withdrawn request"); !ok {
		return res
	}
	if assetKey := transferPendingAssetKey(rec); assetKey != "" {
		p.releaseTransferTicket(kind, requesterID, requestID, assetKey)
	} else {
		p.releasePending(kind, requesterID, requestID)
	}

	summary := p.summarize(p.getConfiguration(), kind, rec)
	note := fmt.Sprintf("@%s withdrew their %s request", actingUser.Username, aKind(kind))
	if summary.Summary != "" {
		note = fmt.Sprintf("%s (%s)", note, summary.Summary)
	}
	// clickedPostID is the DM root (not an approval card). Keep finish on the
	// approval post, but stamp via=card when the Withdraw button was used.
	auditCards := cards
	auditCards.clickedPostID = clickedPostID
	p.decisionEvent(auditCards, eventWithdrawn, note, map[string]any{
		"withdrawn_by_id": actingUser.Id,
		"withdrawn_by":    actingUser.Username,
	})

	outcome := fmt.Sprintf("↩️ Withdrawn by @%s", actingUser.Username)
	result := p.finish(cards, "Your request was withdrawn. Approvers can no longer act on it.", outcome, ticketClose{
		RequesterID:  requesterID,
		DMRootPostID: posts.DMRootPostID,
		DMChannelID:  posts.DMChannelID,
		Withdrawn:    true,
		CardTitle:    "Request Withdrawn",
		CardText:     "You withdrew this request. Approvers were notified in the approval channel.",
		StatusLine:   outcome,
		ThreadReply:  fmt.Sprintf("↩️ **Withdrawn** by @%s.", actingUser.Username),
	})
	if clickedPostID != "" && posts.DMRootPostID != "" {
		if updated, getErr := p.API.GetPost(posts.DMRootPostID); getErr == nil {
			result.Update = updated
		}
	}
	return result
}

// findPendingRequest returns the kind and record for a live request ID.
func (p *Plugin) findPendingRequest(requestID string) (kind string, rec storedRequest, raw []byte, err error) {
	for _, k := range allPendingKinds {
		info := pendingKinds[k]
		data, appErr := p.API.KVGet(info.kvPrefix + requestID)
		if appErr != nil {
			return "", nil, nil, appErr
		}
		if data == nil {
			continue
		}
		record := info.newRecord()
		if uErr := json.Unmarshal(data, record); uErr != nil {
			return "", nil, nil, uErr
		}
		return k, record, data, nil
	}
	return "", nil, nil, nil
}
