package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	kvWebhookTransferRequestPrefix = "webhook_transfer_request_"

	webhookTransferRequestsDisabledMsg = "Webhook transfer/retire requests are currently disabled by an administrator."
)

// webhookTransferRequest reassigns or retires a standalone RH-managed webhook.
// It routes to the webhook approval channel and needs two-step approval, the
// same as a webhook request.
type webhookTransferRequest struct {
	ID          string `json:"id"`
	RequesterID string `json:"requester_id"`
	HookID      string `json:"hook_id"`
	// TransferHandle is the opaque id that stands in for an incoming hook's
	// ID (its secret URL) in audit data. Empty for outgoing hooks.
	TransferHandle string `json:"transfer_handle,omitempty"`
	WebhookType    string `json:"webhook_type"` // incoming / outgoing
	DisplayName    string `json:"display_name,omitempty"`
	ChannelID      string `json:"channel_id,omitempty"`
	TeamID         string `json:"team_id,omitempty"`
	// CallbackURL is copied from the webhook index (outgoing only) so
	// retire+recreate can rebuild without a live GetOutgoingWebhook.
	CallbackURL string `json:"callback_url,omitempty"`
	// Action is reassign / retire_recreate / retire_only.
	Action string `json:"action"`
	// NewOwnerUserID is required for reassign and retire_recreate.
	NewOwnerUserID string `json:"new_owner_user_id,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Note           string `json:"note,omitempty"`
	CreatedAt      int64  `json:"created_at,omitempty"`
	Exercise       bool   `json:"exercise,omitempty"`

	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`

	twoStepState
}

type webhookTransferInput struct {
	HookID         string
	Action         string
	NewOwnerUserID string
	Reason         string
	Note           string
}

func webhookTransferActionLabel(action string) string {
	switch action {
	case transferActionReassign:
		return "Reassign"
	case transferActionRetireRecreate:
		return "Retire & recreate"
	case transferActionRetireOnly:
		return "Retire"
	default:
		return action
	}
}

// auditSnapshot is the copy of the request kept in its history and the
// server audit log. An incoming hook's ID is its secret URL, so it is left
// out and the transfer handle identifies the hook instead.
func (req webhookTransferRequest) auditSnapshot() any {
	if req.WebhookType == webhookTypeIncoming {
		req.HookID = ""
	}
	return req
}

func webhookTransferNeedsNewOwner(action string) bool {
	return action == transferActionReassign || action == transferActionRetireRecreate
}

// submitWebhookTransferRequest validates input and queues a webhook
// transfer/retire for two-step approval.
func (p *Plugin) submitWebhookTransferRequest(requesterID string, in webhookTransferInput) (submitted string, submitErr error) {
	sa := p.startSubmitAudit(pendingKindWebhookTransfer, requesterID)
	defer sa.done(&submitErr)

	if !p.getConfiguration().RequestEnabled(requestTypeWebhookTransfer) {
		return "", errors.New(webhookTransferRequestsDisabledMsg)
	}
	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil || requester == nil {
		return "", errors.New("could not load your account; try again")
	}

	hookID, err := p.resolveWebhookTransferHookID(in.HookID)
	if err != nil {
		return "", err
	}
	index, err := p.loadWebhookIndex(hookID)
	if err != nil {
		p.API.LogWarn("failed to load webhook index for transfer", "requester_id", requesterID, "error", err.Error())
		return "", errors.New("could not look up that webhook; try again")
	}
	if index == nil {
		return "", errors.New("that webhook isn't managed by Request Hub, so it can't be transferred or retired here")
	}
	if index.RetiredAt != 0 {
		return "", errors.New("that webhook has already been retired")
	}

	action := strings.TrimSpace(strings.ToLower(in.Action))
	switch action {
	case transferActionReassign, transferActionRetireRecreate, transferActionRetireOnly:
	default:
		return "", errors.New("pick Reassign, Retire & recreate, or Retire")
	}

	ownerID := webhookIndexOwner(*index)
	asset := transferAsset{
		assetType: "webhook", assetID: hookID,
		ownerUserID: ownerID, requesterID: index.RequesterID, channelID: index.ChannelID,
	}
	if err = p.canNominateTransfer(requesterID, asset); err != nil {
		return "", err
	}

	ownerActive, ownerKnown := p.userActiveStatus(ownerID)
	if !ownerKnown {
		return "", errors.New("could not verify the current owner's account; try again")
	}
	// Orphaned incoming hooks keep the same secret URL on reassign — refuse.
	if index.WebhookType == webhookTypeIncoming && !ownerActive && action == transferActionReassign {
		return "", errors.New("an orphaned incoming webhook must be retired or retired & recreated (reassign keeps the same secret URL)")
	}
	newOwnerID := strings.TrimSpace(in.NewOwnerUserID)
	if webhookTransferNeedsNewOwner(action) {
		if err = p.checkTransferNewOwner(newOwnerID, transferNewOwnerOpts{
			currentOwnerID: ownerID, channelID: index.ChannelID,
			webhookType: index.WebhookType, noun: "webhook",
		}); err != nil {
			return "", err
		}
	} else {
		newOwnerID = ""
	}
	note := strings.TrimSpace(in.Note)
	if err = checkTextLen("note", note, maxTransferNoteLen); err != nil {
		return "", err
	}
	reason, err := normalizeTransferReason(in.Reason, ownerActive)
	if err != nil {
		return "", err
	}

	req := &webhookTransferRequest{
		ID:             model.NewId(),
		RequesterID:    requesterID,
		HookID:         hookID,
		TransferHandle: index.TransferHandle,
		WebhookType:    index.WebhookType,
		DisplayName:    index.DisplayName,
		ChannelID:      index.ChannelID,
		TeamID:         index.TeamID,
		CallbackURL:    strings.TrimSpace(index.CallbackURL),
		Action:         action,
		NewOwnerUserID: newOwnerID,
		Reason:         reason,
		Note:           note,
		CreatedAt:      model.GetMillis(),
	}
	sa.built(req, requester)
	if held, holdErr := p.transferAssetPendingElsewhere(pendingKindWebhookTransfer, req.pendingKey(), ""); holdErr != nil {
		p.API.LogWarn("failed to check for existing webhook transfer ticket", "request_id", req.ID, "error", holdErr.Error())
		return "", errors.New("could not check for an existing transfer ticket; try again")
	} else if held {
		return "", duplicateRequestError(pendingKindWebhookTransfer, req.pendingKey())
	}
	if err = p.reservePending(pendingKindWebhookTransfer, req.RequesterID, newPendingEntry(req), p.getConfiguration()); err != nil {
		return "", err
	}
	if err = p.storeWebhookTransferRequest(req); err != nil {
		p.releasePending(pendingKindWebhookTransfer, req.RequesterID, req.ID)
		p.API.LogError("failed to store webhook transfer request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	sa.markStored()
	if claimErr := p.claimTransferAsset(pendingKindWebhookTransfer, req.pendingKey(), req.ID); claimErr != nil {
		p.deletePendingRequestKV(kvWebhookTransferRequestPrefix + req.ID)
		p.releasePending(pendingKindWebhookTransfer, req.RequesterID, req.ID)
		return "", claimErr
	}
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		p.deletePendingRequestKV(kvWebhookTransferRequestPrefix + req.ID)
		p.releaseTransferTicket(pendingKindWebhookTransfer, req.RequesterID, req.ID, req.pendingKey())
		return "", errors.Wrap(snapErr, "failed to snapshot webhook transfer request")
	}

	approvalPostID, approvalChannelID, err := p.postWebhookTransferApprovalRequest(req, requester)
	if err != nil {
		p.deletePendingRequestKV(kvWebhookTransferRequestPrefix + req.ID)
		p.releaseTransferTicket(pendingKindWebhookTransfer, req.RequesterID, req.ID, req.pendingKey())
		p.API.LogError("failed to post webhook transfer request", "error", err.Error())
		return "", errors.New("could not send your request to the approvers; try again or contact a System Admin")
	}
	if approvalPostID == "" {
		p.resolveCardsIfHandled(pendingKindWebhookTransfer, req.ID)
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	req.DMRootPostID, req.DMChannelID = p.postTicketCreatedDM(req.RequesterID,
		p.webhookTransferApprovalAttachment(req, requester), ticketSubmittedIntro("webhook transfer/retire request"), req.ID)

	p.persistTicketPosts(ticketPersist{
		KVKey: kvWebhookTransferRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "webhook transfer request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Kind: requestTypeWebhookTransfer,
		},
	})
	return fmt.Sprintf("Your request for the %s webhook **%s** was submitted for approval. It needs a security approval and a system approval.",
		req.WebhookType, escapeMarkdown(req.DisplayName)), nil
}

func (p *Plugin) postWebhookTransferApprovalRequest(req *webhookTransferRequest, requester *model.User) (postID, channelID string, err error) {
	ch := p.ensureWebhookApprovalChannel()
	go func() {
		defer p.logRecoveredPanic("two-step approver channel reconcile")
		p.reconcileTwoStepApproverChannels()
	}()
	return p.postApprovalAttachment(req.ID, p.webhookTransferApprovalAttachment(req, requester), p.webhookTransferApprovalHeader(req.Exercise), ch)
}

func (p *Plugin) webhookTransferApprovalHeader(exercise bool) string {
	return cardMention(exercise) + "a webhook transfer/retire request needs review (requires a security approval and a system approval)."
}

func webhookTransferRetryHeader(exercise bool) string {
	return cardMention(exercise) + "a webhook transfer/retire request needs review (a previous approval attempt failed)."
}

func (p *Plugin) webhookTransferApprovalAttachment(req *webhookTransferRequest, requester *model.User) *model.MessageAttachment {
	requester = requesterOrPlaceholder(requester, req.RequesterID)
	fields := []*model.MessageAttachmentField{
		{Title: "Requested by", Value: "@" + requester.Username, Short: true},
		{Title: "Action", Value: webhookTransferActionLabel(req.Action), Short: true},
		{Title: "Type", Value: req.WebhookType, Short: true},
		{Title: "Webhook", Value: escapeMarkdown(req.DisplayName), Short: true},
	}
	if webhookTransferNeedsNewOwner(req.Action) {
		fields = append(fields, &model.MessageAttachmentField{Title: "New owner", Value: p.mentionList([]string{req.NewOwnerUserID}), Short: true})
	}
	if req.Reason != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Reason", Value: escapeMarkdown(req.Reason), Short: true})
	}
	if req.Note != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Note", Value: escapeMarkdown(req.Note), Short: false})
	}
	fields = append(fields, &model.MessageAttachmentField{Title: "Approvals", Value: p.twoStepStatusValue(req.twoStepState), Short: false})

	var text string
	switch req.Action {
	case transferActionReassign:
		text = "Reassign updates the webhook's owner. Mattermost may keep the original creator on some builds; the RH index is updated and the outcome noted."
	case transferActionRetireRecreate:
		text = "Retire & recreate deletes the old hook and creates a new one owned by the successor. An incoming webhook's new URL is DMed to the new owner only."
	case transferActionRetireOnly:
		text = "Retire deletes the webhook."
	}
	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   exerciseLabel(req.Exercise) + "Webhook transfer/retire request",
		Color:   "#FF6B35",
		Fields:  fields,
		Footer:  requestIDFooter(req.ID),
		Text:    text,
		Actions: p.webhookTransferApprovalActions(req.ID, siteURL),
	}
}

func (p *Plugin) webhookTransferApprovalAttachmentWithTitledNotice(req *webhookTransferRequest, requester *model.User, title, notice string) *model.MessageAttachment {
	att := p.webhookTransferApprovalAttachment(req, requester)
	att.Color = "#D24B4E"
	if title == "" {
		title = "⚠️ Action needed"
	}
	att.Fields = append([]*model.MessageAttachmentField{{Title: title, Value: notice, Short: false}}, att.Fields...)
	return att
}

func (p *Plugin) webhookTransferApprovalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id: "approve", Name: approveButtonLabel, Type: model.PostActionTypeButton, Style: "primary",
			Integration: &model.PostActionIntegration{URL: siteURL + routeApproveWebhookTransfer, Context: map[string]any{actionContextRequestID: requestID}},
		},
		{
			Id: "deny", Name: "Deny", Type: model.PostActionTypeButton, Style: "danger",
			Integration: &model.PostActionIntegration{URL: siteURL + routeDenyWebhookTransfer, Context: map[string]any{actionContextRequestID: requestID}},
		},
	}
}

func (p *Plugin) storeWebhookTransferRequest(req *webhookTransferRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal webhook transfer request")
	}
	if err := p.putPendingRequestKV(kvWebhookTransferRequestPrefix+req.ID, data); err != nil {
		return errors.Wrap(err, "failed to store webhook transfer request")
	}
	return nil
}

func (p *Plugin) loadWebhookTransferRequest(id string) (*webhookTransferRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvWebhookTransferRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load webhook transfer request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req webhookTransferRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal webhook transfer request")
	}
	return &req, data, nil
}

// decideWebhookTransferRequest approves or denies a webhook transfer/retire
// through the two-step engine.
func (p *Plugin) decideWebhookTransferRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil {
		return decisionResult{Reply: "Could not verify your identity."}
	}
	key := kvWebhookTransferRequestPrefix + requestID
	req, rawReq, err := p.loadWebhookTransferRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load webhook transfer request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeWebhookTransfer, approve); msg != "" {
		return decisionResult{Reply: msg}
	}

	p.refreshTicketThread(ticketThread{
		RequestID: req.ID, ApprovalPostID: req.ApprovalPostID, ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID, Kind: requestTypeWebhookTransfer,
	})

	canSec, canSys := p.twoStepPools(actingUser, "webhook transfer")
	requester, _ := p.API.GetUser(req.RequesterID)
	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID).
		withSubject(pendingKindWebhookTransfer, req.RequesterID, actingUser, req, req.Exercise)

	label := webhookTransferActionLabel(req.Action)
	if days := p.getConfiguration().ExpiryDays(); (canSec || canSys) && requestExpired(req.CreatedAt, days, model.GetMillis()) {
		return p.expireRequest(cards, expiredRequest{
			kvKey: key, raw: rawReq, kind: pendingKindWebhookTransfer, requestID: req.ID, requesterID: req.RequesterID,
			dmRootID: req.DMRootPostID, dmChannelID: req.DMChannelID, days: days,
			transferAssetKey: req.pendingKey(),
			cardTitle:        "Webhook " + label + " Request Expired",
			cardText:         fmt.Sprintf("Your request for the %s webhook **%s** expired before it was reviewed. Submit it again if you still need it.", req.WebhookType, escapeMarkdown(req.DisplayName)),
			audit: fmt.Sprintf("@%s closed the %s request for the %s webhook %s from %s, which was not reviewed within %d days",
				actingUser.Username, strings.ToLower(label), req.WebhookType, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID), days),
		})
	}

	if !approve {
		if !canSec && !canSys {
			return decisionResult{Reply: "You don't have permission to deny this request."}
		}
		if res, ok := p.claimRecord(key, rawReq, cards, "webhook transfer request"); !ok {
			return res
		}
		p.releaseTransferTicket(pendingKindWebhookTransfer, req.RequesterID, req.ID, req.pendingKey())
		outcome := fmt.Sprintf("❌ Denied by @%s. %s webhook **%s**.", actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName))
		p.decisionEvent(cards, eventDenied, fmt.Sprintf("@%s denied the %s request for the %s webhook %s", actingUser.Username, strings.ToLower(label), req.WebhookType, escapeMarkdown(req.DisplayName)), nil)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: false, ApproverUsername: actingUser.Username,
			CardTitle:  "Webhook " + label + " Request Denied",
			CardText:   fmt.Sprintf("Denied by @%s. The %s request for the %s webhook **%s** was not approved.", actingUser.Username, strings.ToLower(label), req.WebhookType, escapeMarkdown(req.DisplayName)),
			StatusLine: outcome,
		})
	}

	step, fill, reason := planApproval(req.twoStepState, canSec, canSys, actingUser.Id, req.RequesterID)
	if !fill {
		return twoStepBlocked(cards, actingUser.Username, reason, p.webhookTransferApprovalHeader(req.Exercise), func(title, notice string) *model.MessageAttachment {
			return p.webhookTransferApprovalAttachmentWithTitledNotice(req, requesterOrPlaceholder(requester, req.RequesterID), title, notice)
		})
	}

	if msg := p.webhookTransferApprovalBlockerFor(req, actingUser); msg != "" {
		return decisionResult{Reply: msg}
	}

	newState := p.applyTwoStepApproval(req.twoStepState, step, actingUser, model.GetMillis())
	if !newState.complete() {
		newReq := *req
		newReq.twoStepState = newState
		return p.recordFirstApproval(cards, key, rawReq, &newReq, step, ticketProgress{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Title: "Webhook " + label + " Request Update",
			Text:  fmt.Sprintf("Your %s request for the %s webhook **%s** received the %s approval; awaiting the other approval.", strings.ToLower(label), req.WebhookType, escapeMarkdown(req.DisplayName), step),
		}, p.webhookTransferApprovalHeader(req.Exercise), func() *model.MessageAttachment {
			return p.webhookTransferApprovalAttachment(&newReq, requesterOrPlaceholder(requester, req.RequesterID))
		})
	}

	if res, ok := p.claimTransferForFulfill(req.ID, key, rawReq, cards, "webhook transfer request"); !ok {
		return res
	}
	// Keep the pending reservation until fulfill succeeds (released there).
	return p.fulfillWebhookTransferRequest(req, actingUser, requester, cards, rawReq, key)
}

// webhookTransferApprovalBlockerFor re-checks successor validity, live
// nominate ACL, and that the hook still exists before final approval.
func (p *Plugin) webhookTransferApprovalBlockerFor(req *webhookTransferRequest, actingUser *model.User) string {
	index, indexErr := p.loadWebhookIndex(req.HookID)
	if indexErr != nil {
		return "Could not re-check webhook ownership; try Approve again."
	}
	if index == nil || index.RetiredAt != 0 {
		return "That webhook is no longer managed (or was retired). Deny this request instead."
	}
	if actingUser != nil {
		if exists, existErr := p.webhookStillExists(req.WebhookType, req.HookID, actingUser.Id); existErr != nil {
			return "Could not re-check whether the webhook still exists; try Approve again."
		} else if !exists {
			return "That webhook no longer exists in Mattermost. Deny this request instead."
		}
	}
	ownerID := webhookIndexOwner(*index)
	asset := transferAsset{
		assetType: "webhook", assetID: req.HookID,
		ownerUserID: ownerID, requesterID: index.RequesterID, channelID: index.ChannelID,
	}
	if err := p.canNominateTransfer(req.RequesterID, asset); err != nil {
		return "The requester can no longer transfer or retire this webhook (" + err.Error() + "). Deny this request instead."
	}
	// Submit refuses this too; the owner may have been deactivated since.
	if index.WebhookType == webhookTypeIncoming && req.Action == transferActionReassign {
		ownerActive, ownerKnown := p.userActiveStatus(ownerID)
		if !ownerKnown {
			return "Could not re-check the current owner's account; try Approve again."
		}
		if !ownerActive {
			return "The current owner has been deactivated, and reassigning an incoming webhook keeps the same secret URL. Deny this request, and ask for Retire & recreate instead."
		}
	}
	if webhookTransferNeedsNewOwner(req.Action) {
		actingID := ""
		if actingUser != nil {
			actingID = actingUser.Id
		}
		if err := p.checkTransferNewOwner(req.NewOwnerUserID, transferNewOwnerOpts{
			currentOwnerID: ownerID, actingUserID: actingID,
			channelID: index.ChannelID, webhookType: index.WebhookType, noun: "webhook",
		}); err != nil {
			return err.Error() + " Deny this request, or ask for a new one with a valid owner."
		}
	}
	return ""
}

// fulfillWebhookTransferRequest carries out a fully approved webhook
// transfer/retire with the approving admin's session.
func (p *Plugin) fulfillWebhookTransferRequest(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string) decisionResult {
	var partialErrors []string

	switch req.Action {
	case transferActionReassign:
		if err := p.updateWebhookOwner(req.WebhookType, req.HookID, req.NewOwnerUserID, actingUser.Id); err != nil {
			// Fail closed: do not update the RH index when Mattermost did not
			// change ownership (use Retire & recreate when update is a no-op).
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, err)
		}
		if idxErr := p.updateWebhookIndexOwner(req.HookID, req.NewOwnerUserID); idxErr != nil {
			partialErrors = append(partialErrors, "webhook index owner could not be updated: "+idxErr.Error()+" — transfer rights may still show the previous owner")
		}
		p.releaseTransferTicket(pendingKindWebhookTransfer, req.RequesterID, req.ID, req.pendingKey())
		outcome := fmt.Sprintf("✅ Fully approved. %s webhook **%s** reassigned to %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
		cardText := fmt.Sprintf("Fully approved. The %s webhook **%s** now belongs to %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
		if len(partialErrors) > 0 {
			outcome = fmt.Sprintf("⚠️ Fully approved with issues. %s webhook **%s** reassigned to %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
			cardText = fmt.Sprintf("Fully approved with issues. The %s webhook **%s** was reassigned, but: %s.", req.WebhookType, escapeMarkdown(req.DisplayName), escapeMarkdown(strings.Join(partialErrors, "; ")))
		}
		p.auditWebhookTransferApproved(cards, req, actingUser, requester, partialErrors)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle: "Webhook Reassigned", CardText: cardText,
			StatusLine: outcome,
		})

	case transferActionRetireOnly:
		if err := p.deleteWebhook(req.WebhookType, req.HookID, actingUser.Id); err != nil && !webhookDeleteAlreadyGone(err) {
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, err)
		}
		if idxErr := p.retireWebhookIndex(req.HookID); idxErr != nil {
			partialErrors = append(partialErrors, "webhook index could not be marked retired: "+idxErr.Error())
		}
		p.releaseTransferTicket(pendingKindWebhookTransfer, req.RequesterID, req.ID, req.pendingKey())
		outcome := fmt.Sprintf("✅ Fully approved. %s webhook **%s** retired.", req.WebhookType, escapeMarkdown(req.DisplayName))
		cardText := fmt.Sprintf("Fully approved. The %s webhook **%s** has been deleted.", req.WebhookType, escapeMarkdown(req.DisplayName))
		if len(partialErrors) > 0 {
			outcome = fmt.Sprintf("⚠️ Fully approved with issues. %s webhook **%s** retired.", req.WebhookType, escapeMarkdown(req.DisplayName))
			cardText = fmt.Sprintf("Fully approved with issues. The %s webhook **%s** was deleted, but: %s.", req.WebhookType, escapeMarkdown(req.DisplayName), escapeMarkdown(strings.Join(partialErrors, "; ")))
		}
		p.auditWebhookTransferApproved(cards, req, actingUser, requester, partialErrors)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle: "Webhook Retired", CardText: cardText,
			StatusLine: outcome,
		})

	default: // retire_recreate
		return p.fulfillWebhookRetireRecreate(req, actingUser, requester, cards, rawReq, key)
	}
}

// fulfillWebhookRetireRecreate creates the replacement under the successor
// first, then deletes the old hook. Outgoing hooks load the live Mattermost
// record so trigger words and every callback URL are preserved; if the live
// hook cannot be loaded and the snapshot is incomplete, the request stays
// pending (refuse) instead of dropping fidelity.
func (p *Plugin) fulfillWebhookRetireRecreate(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string) decisionResult {
	var outgoingSnap *outgoingWebhookRecreate
	callbackURL := strings.TrimSpace(req.CallbackURL)
	if req.WebhookType == webhookTypeOutgoing {
		live, liveErr := p.loadOutgoingWebhookForRecreate(req.HookID, actingUser.Id)
		switch {
		case liveErr == nil && live != nil && len(live.CallbackURLs) > 0:
			outgoingSnap = live
			callbackURL = strings.TrimSpace(live.CallbackURLs[0])
		case callbackURL == "":
			err := liveErr
			if err == nil {
				err = errors.New("outgoing webhook has no callback URL recorded")
			}
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, err)
		case liveErr != nil:
			// Snapshot has one callback but we could not confirm triggers /
			// extra callbacks — refuse rather than silently drop them.
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key,
				errors.Wrap(liveErr, "could not load the live outgoing webhook to preserve triggers and callbacks; deny and recreate manually, or retry"))
		default:
			outgoingSnap = &outgoingWebhookRecreate{CallbackURLs: []string{callbackURL}}
		}
		req.CallbackURL = callbackURL
	}

	desc := p.webhookRecreateDescription(req)
	synthetic := &webhookRequest{
		ID:          req.ID,
		RequesterID: req.NewOwnerUserID,
		WebhookType: req.WebhookType,
		ChannelID:   req.ChannelID,
		TeamID:      req.TeamID,
		DisplayName: req.DisplayName,
		Description: desc,
		CallbackURL: callbackURL,
	}
	if outgoingSnap != nil && strings.TrimSpace(outgoingSnap.Description) != "" {
		synthetic.Description = strings.TrimSpace(outgoingSnap.Description)
	}
	var partialErrors []string
	var dmMsg, newHookID string
	var createErr error
	if req.WebhookType == webhookTypeOutgoing && outgoingSnap != nil {
		dmMsg, newHookID, createErr = p.createOutgoingWebhookFromRecreate(synthetic, outgoingSnap, actingUser.Id)
	} else {
		dmMsg, newHookID, createErr = p.createWebhookForRequest(synthetic, actingUser.Id)
	}
	if createErr != nil {
		// Old hook is untouched — leave the ticket pending for a retry.
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, createErr)
	}

	// Incoming: DM the new secret URL before deleting the old hook so a DM
	// failure does not leave nobody with a working URL.
	if req.WebhookType == webhookTypeIncoming && dmMsg != "" {
		if !p.sendTransferSecretsDM(req.NewOwnerUserID, req.ID, []string{dmMsg, "⚠️ **Save this now.** This message auto-deletes in ~72 hours."}, true) {
			p.API.LogError("failed to deliver recreated webhook URL to new owner", "new_owner_id", req.NewOwnerUserID)
			// Nobody has the new URL, so the old hook must stay live whether
			// or not the replacement can be cleaned up.
			cause := "the new incoming webhook URL could not be DM'd to the new owner; the old hook was left in place"
			if delErr := p.deleteWebhook(req.WebhookType, newHookID, actingUser.Id); delErr != nil && !webhookDeleteAlreadyGone(delErr) {
				p.API.LogError("failed to delete undelivered replacement webhook", "error", delErr.Error())
				cause += ", and the unused replacement hook could not be deleted (a System Admin should remove the extra incoming webhook in this channel before retrying)"
			}
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, errors.New(cause))
		}
	}

	oldDeleted := true
	if err := p.deleteWebhook(req.WebhookType, req.HookID, actingUser.Id); err != nil && !webhookDeleteAlreadyGone(err) {
		if req.WebhookType == webhookTypeOutgoing {
			// The new outgoing hook runs on a temporary trigger (or would
			// double every callback next to the old one), so take it back
			// down and leave the old hook and the ticket as they were.
			cause := "the old outgoing webhook could not be deleted: " + err.Error() + "; the new hook was removed and the old one left in place"
			if delErr := p.deleteWebhook(req.WebhookType, newHookID, actingUser.Id); delErr != nil && !webhookDeleteAlreadyGone(delErr) {
				p.API.LogError("failed to delete replacement outgoing webhook after old delete failed", "hook_id", newHookID, "error", delErr.Error())
				cause = "the old outgoing webhook could not be deleted: " + err.Error() + ", and the new hook could not be removed either (a System Admin should delete the extra outgoing webhook in this channel before retrying)"
			}
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, errors.New(cause))
		}
		oldDeleted = false
		// Replacement exists; old still present. Do not retire the old index
		// row — the hub must still be able to target the live old hook.
		partialErrors = append(partialErrors, "the old webhook could not be deleted after recreate: "+err.Error()+" — delete the old hook manually; the new hook was DM'd to the new owner")
	}
	if oldDeleted {
		if idxErr := p.retireWebhookIndex(req.HookID); idxErr != nil {
			partialErrors = append(partialErrors, "old webhook index could not be marked retired: "+idxErr.Error())
		}
		if req.WebhookType == webhookTypeOutgoing && outgoingSnap != nil {
			if trigErr := p.restoreOutgoingWebhookTriggers(newHookID, actingUser.Id, outgoingSnap); trigErr != nil {
				partialErrors = append(partialErrors, "recreated webhook triggers could not be restored: "+trigErr.Error()+" — set the new outgoing webhook's trigger words manually to: "+strings.Join(outgoingSnap.TriggerWords, ", "))
			}
		}
	}
	if idxErr := p.writeWebhookIndex(synthetic, newHookID, actingUser.Id); idxErr != nil {
		partialErrors = append(partialErrors, "new webhook index could not be written: "+idxErr.Error()+" — the hook exists but may not appear in Transfer/Retire")
	}

	if req.WebhookType == webhookTypeOutgoing {
		cbNote := callbackURL
		if outgoingSnap != nil && len(outgoingSnap.CallbackURLs) > 1 {
			cbNote = strings.Join(outgoingSnap.CallbackURLs, ", ")
		}
		notice := fmt.Sprintf("Your outgoing webhook **%s** was recreated under your account.\n**Callback URL(s) (unchanged):** %s",
			escapeMarkdown(req.DisplayName), inlineCode(cbNote))
		_ = p.sendTransferSecretsDM(req.NewOwnerUserID, req.ID, []string{notice}, false)
	}

	p.releaseTransferTicket(pendingKindWebhookTransfer, req.RequesterID, req.ID, req.pendingKey())
	outcome := fmt.Sprintf("✅ Fully approved. %s webhook **%s** recreated under %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
	cardText := fmt.Sprintf("Fully approved. A new %s webhook **%s** was created under %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
	if len(partialErrors) > 0 {
		outcome = fmt.Sprintf("⚠️ Fully approved with issues. %s webhook **%s** recreated under %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
		cardText = fmt.Sprintf("Fully approved with issues. A new %s webhook **%s** was created under %s, but: %s.",
			req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}), escapeMarkdown(strings.Join(partialErrors, "; ")))
	}
	p.auditWebhookTransferApproved(cards, req, actingUser, requester, partialErrors)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: true, ApproverUsername: actingUser.Username,
		CardTitle: "Webhook Recreated", CardText: cardText,
		StatusLine: outcome,
	})
}

// webhookRecreateDescription prefers the indexed description, then a short
// transfer note, so recreate does not wipe the original purpose text.
func (p *Plugin) webhookRecreateDescription(req *webhookTransferRequest) string {
	if index, err := p.loadWebhookIndex(req.HookID); err == nil && index != nil {
		if d := strings.TrimSpace(index.Description); d != "" {
			return d
		}
	}
	return "Recreated for transfer to a new owner"
}

// outgoingWebhookRecreate carries the live fields retire+recreate must copy.
type outgoingWebhookRecreate struct {
	CallbackURLs []string
	TriggerWords []string
	TriggerWhen  int
	ContentType  string
	Username     string
	IconURL      string
	Description  string
}

// loadOutgoingWebhookForRecreate returns the live outgoing hook fields needed
// to recreate without dropping triggers or extra callbacks.
func (p *Plugin) loadOutgoingWebhookForRecreate(hookID, adminUserID string) (*outgoingWebhookRecreate, error) {
	if p.getOutgoingWebhookCallbackFn != nil {
		// Test seam historically returned only the first callback; treat as
		// a single-callback snapshot with no triggers.
		cb, err := p.getOutgoingWebhookCallbackFn(hookID, adminUserID)
		if err != nil {
			return nil, err
		}
		cb = strings.TrimSpace(cb)
		if cb == "" {
			return nil, errors.New("outgoing webhook has no callback URLs")
		}
		return &outgoingWebhookRecreate{CallbackURLs: []string{cb}, ContentType: "application/json"}, nil
	}
	if p.getOutgoingWebhookForRecreateFn != nil {
		return p.getOutgoingWebhookForRecreateFn(hookID, adminUserID)
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	hook, _, apiErr := client.GetOutgoingWebhook(context.Background(), hookID)
	if apiErr != nil {
		return nil, errors.Wrap(apiErr, "failed to load outgoing webhook for recreate")
	}
	if hook == nil || len(hook.CallbackURLs) == 0 {
		return nil, errors.New("outgoing webhook has no callback URLs")
	}
	contentType := strings.TrimSpace(hook.ContentType)
	if contentType == "" {
		contentType = "application/json"
	}
	return &outgoingWebhookRecreate{
		CallbackURLs: append([]string(nil), hook.CallbackURLs...),
		TriggerWords: append([]string(nil), hook.TriggerWords...),
		TriggerWhen:  hook.TriggerWhen,
		ContentType:  contentType,
		Username:     hook.Username,
		IconURL:      hook.IconURL,
		Description:  hook.Description,
	}, nil
}

// createOutgoingWebhookFromRecreate creates an outgoing hook copying the
// live trigger/callback fields onto the successor's ownership.
func (p *Plugin) createOutgoingWebhookFromRecreate(req *webhookRequest, snap *outgoingWebhookRecreate, adminUserID string) (dmMsg, hookID string, err error) {
	if snap == nil || len(snap.CallbackURLs) == 0 {
		return "", "", errors.New("outgoing webhook recreate is missing callback URLs")
	}
	if p.createOutgoingWebhookFromRecreateFn != nil {
		return p.createOutgoingWebhookFromRecreateFn(req, snap, adminUserID)
	}
	contentType := strings.TrimSpace(snap.ContentType)
	if contentType == "" {
		contentType = "application/json"
	}
	// Fall back through the standard create path only when the snapshot is a
	// JSON single-callback with no triggers (same as a fresh request).
	if len(snap.CallbackURLs) == 1 && len(snap.TriggerWords) == 0 && snap.TriggerWhen == 0 &&
		(snap.Username == "" && snap.IconURL == "") &&
		strings.EqualFold(contentType, "application/json") {
		req.CallbackURL = snap.CallbackURLs[0]
		return p.createWebhookForRequest(req, adminUserID)
	}

	internalURL := p.internalBaseURL()
	if internalURL == "" {
		return "", "", errors.New("site URL is not configured — cannot create webhook")
	}
	client, cleanup, sessErr := p.transferClient(adminUserID)
	if sessErr != nil {
		return "", "", sessErr
	}
	defer cleanup()

	// Mattermost rejects a new outgoing hook that overlaps an existing one in
	// the same channel on callback + trigger words. Create with a temporary
	// unique trigger when the live hook has triggers; fulfill restores the
	// real triggers after the old hook is deleted.
	createTriggers := append([]string(nil), snap.TriggerWords...)
	createTriggerWhen := snap.TriggerWhen
	if len(snap.TriggerWords) > 0 {
		createTriggers = []string{"rh-recreate-" + model.NewId()}
		createTriggerWhen = 0
	}

	hook, _, apiErr := client.CreateOutgoingWebhook(context.Background(), &model.OutgoingWebhook{
		TeamId:       req.TeamID,
		ChannelId:    req.ChannelID,
		CreatorId:    req.RequesterID,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		CallbackURLs: append([]string(nil), snap.CallbackURLs...),
		TriggerWords: createTriggers,
		TriggerWhen:  createTriggerWhen,
		ContentType:  contentType,
		Username:     snap.Username,
		IconURL:      snap.IconURL,
	})
	if apiErr != nil {
		return "", "", errors.Wrap(apiErr, "failed to recreate outgoing webhook")
	}
	return fmt.Sprintf("Your outgoing webhook **%s** was approved and is now active.", escapeMarkdown(hook.DisplayName)), hook.Id, nil
}

// restoreOutgoingWebhookTriggers sets the recreated hook's trigger words to
// the live snapshot after the old overlapping hook has been deleted.
func (p *Plugin) restoreOutgoingWebhookTriggers(hookID, adminUserID string, snap *outgoingWebhookRecreate) error {
	if snap == nil || len(snap.TriggerWords) == 0 || strings.TrimSpace(hookID) == "" {
		return nil
	}
	if p.restoreOutgoingWebhookTriggersFn != nil {
		return p.restoreOutgoingWebhookTriggersFn(hookID, adminUserID, snap)
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return err
	}
	defer cleanup()
	hook, _, getErr := client.GetOutgoingWebhook(context.Background(), hookID)
	if getErr != nil {
		return errors.Wrap(getErr, "failed to load recreated outgoing webhook")
	}
	if hook == nil {
		return errors.New("recreated outgoing webhook not found")
	}
	hook.TriggerWords = append([]string(nil), snap.TriggerWords...)
	hook.TriggerWhen = snap.TriggerWhen
	if _, _, upErr := client.UpdateOutgoingWebhook(context.Background(), hook); upErr != nil {
		return errors.Wrap(upErr, "failed to restore outgoing webhook triggers")
	}
	return nil
}

// resolveOutgoingWebhookCallback returns the first callback URL for an
// outgoing hook (compat helper for tests and call sites that only need one).
func (p *Plugin) resolveOutgoingWebhookCallback(hookID, adminUserID string) (string, error) {
	snap, err := p.loadOutgoingWebhookForRecreate(hookID, adminUserID)
	if err != nil {
		return "", err
	}
	if snap == nil || len(snap.CallbackURLs) == 0 {
		return "", errors.New("outgoing webhook has no callback URLs")
	}
	return strings.TrimSpace(snap.CallbackURLs[0]), nil
}

func (p *Plugin) restoreWebhookTransferAfterFailure(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string, cause error) decisionResult {
	label := strings.ToLower(webhookTransferActionLabel(req.Action))
	approverName := ""
	if actingUser != nil {
		approverName = actingUser.Username
	}
	p.API.LogError("failed to carry out webhook transfer/retire on approval", "action", req.Action, "error", cause.Error())
	restored, restoreErr := p.restoreTransferRecord(pendingKindWebhookTransfer, req.pendingKey(), req.ID, key, rawReq)
	if restoreErr != nil && !restored {
		p.API.LogError("failed to restore webhook transfer request after failure", "error", restoreErr.Error())
		return p.lostRequest(cards, pendingKindWebhookTransfer, req.RequesterID, approverName,
			fmt.Sprintf("Could not %s the webhook (%s), and the request could not be saved — ask the requester to resubmit.", label, cause.Error()))
	}
	if !restored {
		return p.lostRequest(cards, pendingKindWebhookTransfer, req.RequesterID, approverName,
			fmt.Sprintf("Could not %s the webhook (%s), and another transfer ticket took over this hook — ask the requester to resubmit if needed.", label, cause.Error()))
	}
	causeMsg := cause.Error()
	if restoreErr != nil {
		p.API.LogWarn("webhook transfer restored but asset claim could not be re-checked", "error", restoreErr.Error())
		causeMsg = cause.Error() + "; asset claim could not be re-checked — retry Approve"
	}
	notice := func(clicked bool) string {
		return fmt.Sprintf("Couldn't %s the webhook: %s. The request is still pending — %s.", label, causeMsg, retryText(clicked))
	}
	p.approveFailed(cards, fmt.Sprintf("final approval for the %s webhook \"%s\" could not be carried out", req.WebhookType, escapeMarkdown(req.DisplayName)), cause)
	result := p.repaintCards(cards, key, notice(cards.clickedPostID != ""),
		webhookTransferRetryHeader(req.Exercise),
		p.webhookTransferApprovalAttachmentWithTitledNotice(req, requesterOrPlaceholder(requester, req.RequesterID), "⚠️ Action needed", notice(true)))
	result.audited = true
	return result
}

func (p *Plugin) auditWebhookTransferApproved(cards decisionCards, req *webhookTransferRequest, actingUser, requester *model.User, partialErrors []string) {
	label := strings.ToLower(webhookTransferActionLabel(req.Action))
	event, problems := eventApproved, ""
	if len(partialErrors) > 0 {
		event, problems = eventApprovedPartial, "; "+escapeMarkdown(strings.Join(partialErrors, "; "))
	}
	detail := map[string]any{"action": req.Action, "webhook_type": req.WebhookType}
	if req.WebhookType == webhookTypeIncoming {
		detail["transfer_handle"] = req.TransferHandle
	} else {
		detail["hook_id"] = req.HookID
	}
	if webhookTransferNeedsNewOwner(req.Action) {
		detail["new_owner_id"] = req.NewOwnerUserID
	}
	p.decisionEvent(cards, event, fmt.Sprintf("@%s gave final approval; %s webhook \"%s\" %s for %s%s",
		actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName), label, requesterUsername(requester, req.RequesterID), problems), detail)
}

// handleWebhookTransferAction handles Approve/Deny on a webhook transfer card.
func (p *Plugin) handleWebhookTransferAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleTwoStepClick(w, r, approve, p.deciderFor(pendingKindWebhookTransfer))
}
