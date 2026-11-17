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
	WebhookType string `json:"webhook_type"` // incoming / outgoing
	DisplayName string `json:"display_name,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	TeamID      string `json:"team_id,omitempty"`
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

	hookID := strings.TrimSpace(in.HookID)
	index, err := p.loadWebhookIndex(hookID)
	if err != nil {
		p.API.LogWarn("failed to load webhook index for transfer", "hook_id", hookID, "error", err.Error())
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

	asset := transferAsset{
		assetType: "webhook", assetID: hookID,
		ownerUserID: index.OwnerUserID, requesterID: index.RequesterID, channelID: index.ChannelID,
	}
	if err = p.canNominateTransfer(requesterID, asset); err != nil {
		return "", err
	}

	newOwnerID := strings.TrimSpace(in.NewOwnerUserID)
	if webhookTransferNeedsNewOwner(action) {
		if err = p.checkTransferNewOwner(newOwnerID); err != nil {
			return "", err
		}
	} else {
		newOwnerID = ""
	}
	note := strings.TrimSpace(in.Note)
	if err = checkTextLen("note", note, maxTransferNoteLen); err != nil {
		return "", err
	}
	reason := strings.TrimSpace(strings.ToLower(in.Reason))
	if reason == "" {
		if !p.userActive(index.OwnerUserID) {
			reason = transferReasonOwnerDeactivated
		} else {
			reason = transferReasonVoluntary
		}
	}

	req := &webhookTransferRequest{
		ID:             model.NewId(),
		RequesterID:    requesterID,
		HookID:         hookID,
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
	if err = p.reservePending(pendingKindWebhookTransfer, req.RequesterID, newPendingEntry(req), p.getConfiguration()); err != nil {
		return "", err
	}
	if err = p.storeWebhookTransferRequest(req); err != nil {
		p.releasePending(pendingKindWebhookTransfer, req.RequesterID, req.ID)
		p.API.LogError("failed to store webhook transfer request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	sa.markStored()
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		p.deletePendingRequestKV(kvWebhookTransferRequestPrefix + req.ID)
		p.releasePending(pendingKindWebhookTransfer, req.RequesterID, req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot webhook transfer request")
	}

	approvalPostID, approvalChannelID, err := p.postWebhookTransferApprovalRequest(req, requester)
	if err != nil {
		p.deletePendingRequestKV(kvWebhookTransferRequestPrefix + req.ID)
		p.releasePending(pendingKindWebhookTransfer, req.RequesterID, req.ID)
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
		fields = append(fields, &model.MessageAttachmentField{Title: "Reason", Value: req.Reason, Short: true})
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
			cardTitle: "Webhook " + label + " Request Expired",
			cardText:  fmt.Sprintf("Your request for the %s webhook **%s** expired before it was reviewed. Submit it again if you still need it.", req.WebhookType, escapeMarkdown(req.DisplayName)),
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
		p.releasePending(pendingKindWebhookTransfer, req.RequesterID, req.ID)
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

	newState := p.applyTwoStepApproval(req.twoStepState, step, actingUser, model.GetMillis())
	if !newState.complete() {
		newReq := *req
		newReq.twoStepState = newState
		return p.recordFirstApproval(cards, key, rawReq, &newReq, step, ticketProgress{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Title: "Webhook " + label + " Request Update",
			Text:  fmt.Sprintf("Your %s request for the %s webhook **%s** received the %s approval; awaiting the other approval.", strings.ToLower(label), req.WebhookType, escapeMarkdown(req.DisplayName), step),
		}, p.webhookTransferApprovalHeader(req.Exercise), func() *model.MessageAttachment { return p.webhookTransferApprovalAttachment(&newReq, requester) })
	}

	if res, ok := p.claimRecord(key, rawReq, cards, "webhook transfer request"); !ok {
		return res
	}
	p.releasePending(pendingKindWebhookTransfer, req.RequesterID, req.ID)
	return p.fulfillWebhookTransferRequest(req, actingUser, requester, cards, rawReq, key)
}

// fulfillWebhookTransferRequest carries out a fully approved webhook
// transfer/retire with the approving admin's session.
func (p *Plugin) fulfillWebhookTransferRequest(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string) decisionResult {
	var partialErrors []string

	switch req.Action {
	case transferActionReassign:
		if err := p.updateWebhookOwner(req.WebhookType, req.HookID, req.NewOwnerUserID, actingUser.Id); err != nil {
			// Index still updated so RH reflects the intended owner; note it.
			partialErrors = append(partialErrors, "Mattermost did not update the webhook creator: "+err.Error()+" — the RH index records the new owner")
		}
		p.updateWebhookIndexOwner(req.HookID, req.NewOwnerUserID)
		p.clearOrphanNotice("webhook", req.HookID)
		outcome := fmt.Sprintf("✅ Fully approved. %s webhook **%s** reassigned to %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
		p.auditWebhookTransferApproved(cards, req, actingUser, requester, partialErrors)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle: "Webhook Reassigned", CardText: fmt.Sprintf("Fully approved. The %s webhook **%s** now belongs to %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID})),
			StatusLine: outcome,
		})

	case transferActionRetireOnly:
		if err := p.deleteWebhook(req.WebhookType, req.HookID, actingUser.Id); err != nil {
			return p.restoreWebhookTransferAfterFailure(req, requester, cards, rawReq, key, err)
		}
		p.retireWebhookIndex(req.HookID)
		p.clearOrphanNotice("webhook", req.HookID)
		outcome := fmt.Sprintf("✅ Fully approved. %s webhook **%s** retired.", req.WebhookType, escapeMarkdown(req.DisplayName))
		p.auditWebhookTransferApproved(cards, req, actingUser, requester, partialErrors)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle: "Webhook Retired", CardText: fmt.Sprintf("Fully approved. The %s webhook **%s** has been deleted.", req.WebhookType, escapeMarkdown(req.DisplayName)),
			StatusLine: outcome,
		})

	default: // retire_recreate
		return p.fulfillWebhookRetireRecreate(req, actingUser, requester, cards, rawReq, key)
	}
}

// fulfillWebhookRetireRecreate deletes the old hook and creates a new one
// owned by the successor, DMing an incoming webhook's URL to the new owner.
// For outgoing hooks the callback URL is resolved before delete (index →
// request snapshot → live GetOutgoingWebhook) so recreate cannot orphan
// the integration without a callback.
func (p *Plugin) fulfillWebhookRetireRecreate(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string) decisionResult {
	callbackURL := strings.TrimSpace(req.CallbackURL)
	if req.WebhookType == webhookTypeOutgoing {
		if callbackURL == "" {
			var resolveErr error
			callbackURL, resolveErr = p.resolveOutgoingWebhookCallback(req.HookID, actingUser.Id)
			if resolveErr != nil || callbackURL == "" {
				err := resolveErr
				if err == nil {
					err = errors.New("outgoing webhook has no callback URL recorded")
				}
				return p.restoreWebhookTransferAfterFailure(req, requester, cards, rawReq, key, err)
			}
			req.CallbackURL = callbackURL
		}
	}

	if err := p.deleteWebhook(req.WebhookType, req.HookID, actingUser.Id); err != nil {
		return p.restoreWebhookTransferAfterFailure(req, requester, cards, rawReq, key, err)
	}
	p.retireWebhookIndex(req.HookID)

	synthetic := &webhookRequest{
		ID:          req.ID,
		RequesterID: req.NewOwnerUserID,
		WebhookType: req.WebhookType,
		ChannelID:   req.ChannelID,
		TeamID:      req.TeamID,
		DisplayName: req.DisplayName,
		Description: "Recreated for transfer to a new owner",
		CallbackURL: callbackURL,
	}
	var partialErrors []string
	dmMsg, newHookID, createErr := p.createWebhookForRequest(synthetic, actingUser.Id)
	if createErr != nil {
		// Old hook is already gone; record the gap for an admin to recreate.
		partialErrors = append(partialErrors, "the replacement webhook could not be created: "+createErr.Error()+" — the old hook was deleted; recreate manually")
		outcome := fmt.Sprintf("⚠️ Fully approved, but the replacement %s webhook could not be created; the old one was deleted.", req.WebhookType)
		p.auditWebhookTransferApproved(cards, req, actingUser, requester, partialErrors)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle: "Webhook Retired (recreate failed)", CardText: fmt.Sprintf("The old %s webhook **%s** was deleted, but the replacement couldn't be created. Ask a System Admin to recreate it under the new owner.", req.WebhookType, escapeMarkdown(req.DisplayName)),
			StatusLine: outcome,
		})
	}
	p.writeWebhookIndex(synthetic, newHookID, actingUser.Id)
	p.clearOrphanNotice("webhook", req.HookID)

	outcome := fmt.Sprintf("✅ Fully approved. %s webhook **%s** recreated under %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
	result := p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: true, ApproverUsername: actingUser.Username,
		CardTitle: "Webhook Recreated", CardText: fmt.Sprintf("Fully approved. A new %s webhook **%s** was created under %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID})),
		StatusLine: outcome,
	})
	p.auditWebhookTransferApproved(cards, req, actingUser, requester, partialErrors)

	switch req.WebhookType {
	case webhookTypeIncoming:
		if dmMsg != "" {
			if !p.sendTransferSecretsDM(req.NewOwnerUserID, req.ID, []string{dmMsg, "⚠️ **Save this now.** This message auto-deletes in ~72 hours."}, true) {
				p.API.LogError("failed to deliver recreated webhook URL to new owner", "hook_id", newHookID, "new_owner_id", req.NewOwnerUserID)
			}
		}
	case webhookTypeOutgoing:
		// Outgoing hooks have no secret URL in the create DM; tell the new
		// owner the callback was preserved so they know recreate succeeded.
		notice := fmt.Sprintf("Your outgoing webhook **%s** was recreated under your account.\n**Callback URL (unchanged):** `%s`",
			escapeMarkdown(req.DisplayName), escapeMarkdown(callbackURL))
		_ = p.sendTransferSecretsDM(req.NewOwnerUserID, req.ID, []string{notice}, false)
	}
	return result
}

// resolveOutgoingWebhookCallback returns the first callback URL for an
// outgoing hook: live Mattermost GetOutgoingWebhook, then empty.
func (p *Plugin) resolveOutgoingWebhookCallback(hookID, adminUserID string) (string, error) {
	if p.getOutgoingWebhookCallbackFn != nil {
		return p.getOutgoingWebhookCallbackFn(hookID, adminUserID)
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return "", err
	}
	defer cleanup()
	hook, _, apiErr := client.GetOutgoingWebhook(context.Background(), hookID)
	if apiErr != nil {
		return "", errors.Wrap(apiErr, "failed to load outgoing webhook for recreate")
	}
	if hook == nil || len(hook.CallbackURLs) == 0 {
		return "", errors.New("outgoing webhook has no callback URLs")
	}
	return strings.TrimSpace(hook.CallbackURLs[0]), nil
}

func (p *Plugin) restoreWebhookTransferAfterFailure(req *webhookTransferRequest, requester *model.User, cards decisionCards, rawReq []byte, key string, cause error) decisionResult {
	label := strings.ToLower(webhookTransferActionLabel(req.Action))
	p.API.LogError("failed to carry out webhook transfer/retire on approval", "action", req.Action, "error", cause.Error())
	if restoreErr := p.putPendingRequestKV(key, rawReq); restoreErr != nil {
		p.API.LogError("failed to restore webhook transfer request after failure", "error", restoreErr.Error())
		return p.lostRequest(cards, pendingKindWebhookTransfer, req.RequesterID, "",
			fmt.Sprintf("Could not %s the webhook (%s), and the request could not be saved — ask the requester to resubmit.", label, cause.Error()))
	}
	notice := func(clicked bool) string {
		return fmt.Sprintf("Couldn't %s the webhook: %s. The request is still pending — %s.", label, cause.Error(), retryText(clicked))
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
	detail := map[string]any{"hook_id": req.HookID, "action": req.Action, "webhook_type": req.WebhookType}
	if webhookTransferNeedsNewOwner(req.Action) {
		detail["new_owner_id"] = req.NewOwnerUserID
	}
	p.decisionEvent(cards, event, fmt.Sprintf("@%s gave final approval; %s webhook \"%s\" %s for %s%s",
		actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName), label, requesterUsername(requester, req.RequesterID), problems), detail)
}

// updateWebhookIndexOwner records a new owner on a webhook index entry.
func (p *Plugin) updateWebhookIndexOwner(hookID, newOwnerID string) {
	entry, err := p.loadWebhookIndex(hookID)
	if err != nil || entry == nil {
		return
	}
	entry.OwnerUserID = newOwnerID
	if storeErr := p.storeWebhookIndex(*entry); storeErr != nil {
		p.API.LogWarn("failed to update webhook index owner", "hook_id", hookID, "error", storeErr.Error())
	}
}

// handleWebhookTransferAction handles Approve/Deny on a webhook transfer card.
func (p *Plugin) handleWebhookTransferAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleTwoStepClick(w, r, approve, p.deciderFor(pendingKindWebhookTransfer))
}
