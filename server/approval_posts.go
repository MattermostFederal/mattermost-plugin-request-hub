package main

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// kvApprovalCardsPrefix namespaces the IDs of every approval card posted for
// a request: the card in the approval channel, or the direct-message copies
// sent to each System Admin when there is no approval channel.
const kvApprovalCardsPrefix = "approval_cards_"

// buttonNotOnCardText is the private reply to an Approve or Deny click on a
// post that isn't one of the request's approval cards.
const buttonNotOnCardText = "This button doesn't belong to that request's card."

// cardCheckFailedText is the private reply to an Approve or Deny click when
// the request's recorded cards couldn't be read.
const cardCheckFailedText = "Couldn't check this card. Try again in a moment."

// requestTypeOffText is the private reply to Approve on a request whose
// type has been turned off since it was submitted.
const requestTypeOffText = "Requests of this type are turned off. Deny this request instead."

// saveApprovalCards records the approval cards posted for a request so
// Approve and Deny can tell them apart from other posts.
func (p *Plugin) saveApprovalCards(requestID string, postIDs []string) error {
	if len(postIDs) == 0 {
		return nil
	}
	data, _ := json.Marshal(postIDs) // a []string always marshals
	if appErr := p.API.KVSet(kvApprovalCardsPrefix+requestID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to record approval cards")
	}
	return nil
}

// deleteApprovalCards removes a request's recorded approval card IDs.
func (p *Plugin) deleteApprovalCards(requestID string) {
	if appErr := p.API.KVDelete(kvApprovalCardsPrefix + requestID); appErr != nil {
		p.API.LogWarn("failed to delete approval cards record", "request_id", requestID, "error", appErr.Error())
	}
}

// deleteApprovalCardPosts removes approval cards that were posted for a
// request that couldn't be saved. Best-effort: failures are logged.
func (p *Plugin) deleteApprovalCardPosts(postIDs []string) {
	for _, postID := range postIDs {
		if appErr := p.API.DeletePost(postID); appErr != nil {
			p.API.LogWarn("failed to delete approval card", "post_id", postID, "error", appErr.Error())
		}
	}
}

// loadApprovalCards returns the recorded approval card IDs for a request,
// or nil when none were recorded.
func (p *Plugin) loadApprovalCards(requestID string) ([]string, error) {
	data, appErr := p.API.KVGet(kvApprovalCardsPrefix + requestID)
	if appErr != nil {
		return nil, errors.Wrap(appErr, "failed to load approval cards")
	}
	if data == nil {
		return nil, nil
	}
	var postIDs []string
	if err := json.Unmarshal(data, &postIDs); err != nil {
		return nil, errors.Wrap(err, "failed to read approval cards")
	}
	return postIDs, nil
}

// isRequestCard reports whether postID is one of the request's approval
// cards: the card stored on the request, or a recorded card. Requests
// submitted before cards were recorded have no record; for those, a post
// by the bot whose buttons carry the request's ID counts as its card.
// Approve and Deny act only on clicks from those posts.
func (p *Plugin) isRequestCard(postID, approvalPostID, requestID string) (bool, error) {
	if postID == "" {
		return false, nil
	}
	if postID == approvalPostID {
		return true, nil
	}
	postIDs, err := p.loadApprovalCards(requestID)
	if err != nil {
		return false, err
	}
	if postIDs != nil {
		return slices.Contains(postIDs, postID), nil
	}
	post, appErr := p.API.GetPost(postID)
	if appErr != nil || post == nil || post.UserId != p.botUserID {
		return false, nil
	}
	return postRequestID(post) == requestID, nil
}

// refuseAction answers an Approve or Deny click that can't go ahead and
// reports whether it did: the button isn't on one of the request's cards
// (or the cards couldn't be checked), or Approve was clicked after requests
// of this type were turned off. Deny still works then, so queued requests
// can be closed.
func (p *Plugin) refuseAction(w http.ResponseWriter, postID, approvalPostID, requestID, requestType string, approve bool) bool {
	isCard, err := p.isRequestCard(postID, approvalPostID, requestID)
	if err != nil {
		p.API.LogWarn("failed to check approval card", "request_id", requestID, "error", err.Error())
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: cardCheckFailedText})
		return true
	}
	if !isCard {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: buttonNotOnCardText})
		return true
	}
	if approve && !p.getConfiguration().RequestEnabled(requestType) {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: requestTypeOffText})
		return true
	}
	return false
}
