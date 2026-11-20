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

	// CutoverStage and the Resume* fields are the checkpoint of a
	// retire+recreate whose replacement is already live but whose cutover
	// did not finish. Approve picks up from there instead of creating a
	// second hook. ResumeOldDeleted predates CutoverStage and is kept in
	// step with it for records written by older versions.
	CutoverStage       string   `json:"cutover_stage,omitempty"`
	ResumeNewHookID    string   `json:"resume_new_hook_id,omitempty"`
	ResumeOldDeleted   bool     `json:"resume_old_deleted,omitempty"`
	ResumeTriggerWords []string `json:"resume_trigger_words,omitempty"`
	ResumeTriggerWhen  int      `json:"resume_trigger_when,omitempty"`
	// ResumeChannelID is where the replacement actually lives, which the
	// successor checks need once the old hook's index row is retired.
	ResumeChannelID string `json:"resume_channel_id,omitempty"`
	// ResumeCallbackURLs are the outgoing callbacks the replacement was
	// built with, so a resumed DM still names all of them.
	ResumeCallbackURLs []string `json:"resume_callback_urls,omitempty"`

	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`

	twoStepState

	// attemptID names the fulfill attempt that holds the asset claim for
	// this run. It belongs to the attempt, not the ticket, so it is never
	// persisted.
	attemptID string
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
		// The replacement's ID is the new secret URL.
		req.ResumeNewHookID = ""
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
	if action == transferActionReassign && p.webhookReassignUnsupported() {
		return "", errors.New(webhookReassignUnsupportedMsg)
	}

	// Prefer Mattermost's own owner: a reassign that moved the hook but
	// could not write the index row must not keep letting the previous
	// owner file tickets against it.
	ownerID := p.webhookOwnerForChecks(index, requesterID)
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
		p.releaseTransferTicketLease(req.transferLease(), req.RequesterID)
		return "", errors.Wrap(snapErr, "failed to snapshot webhook transfer request")
	}

	approvalPostID, approvalChannelID, err := p.postWebhookTransferApprovalRequest(req, requester)
	if err != nil {
		p.deletePendingRequestKV(kvWebhookTransferRequestPrefix + req.ID)
		p.releaseTransferTicketLease(req.transferLease(), req.RequesterID)
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
		text = "Reassign moves the webhook's owner in Mattermost, and only succeeds if Mattermost confirms the creator changed. On builds that keep the original creator it is refused and nothing is recorded — use Retire & recreate there."
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
	// Turning the request type off stops new work; it must not strand a
	// handover that is already part-way through, which only Approve can
	// finish. The card check and the approver pools below still apply.
	gated := approve && !req.cutoverInProgress()
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeWebhookTransfer, gated); msg != "" {
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
	// Expiring deletes the record, which is the only pointer to a
	// replacement hook that already exists. Approve is the way out of that
	// state, so it is let through to the fulfill path however old the
	// ticket is; Deny still has to be told why it cannot close the ticket.
	expired := (canSec || canSys) && requestExpired(req.CreatedAt, p.getConfiguration().ExpiryDays(), model.GetMillis())
	if expired && req.cutoverInProgress() && !approve {
		return decisionResult{Reply: cutoverExpiryBlockedMsg}
	}
	if days := p.getConfiguration().ExpiryDays(); expired && !req.cutoverInProgress() {
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
		if req.cutoverInProgress() {
			if res, ok := p.denyStagedCutover(req, actingUser, requester, cards, rawReq, key); !ok {
				return res
			}
		} else if res, ok := p.claimRecord(key, rawReq, cards, "webhook transfer request"); !ok {
			return res
		}
		p.releaseTransferTicketLease(req.transferLease(), req.RequesterID)
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
	if msg := p.webhookTransferApprovalBlockerFor(req, actingUser, newState.complete()); msg != "" {
		return decisionResult{Reply: msg}
	}

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

	res, attemptID, ok := p.claimTransferForFulfill(pendingKindWebhookTransfer, req.pendingKey(), req.ID, key, rawReq, cards, "webhook transfer request")
	if !ok {
		return res
	}
	req.attemptID = attemptID
	// Keep the pending reservation until fulfill succeeds (released there).
	return p.fulfillWebhookTransferRequest(req, actingUser, requester, cards, rawReq, key)
}

// webhookTransferApprovalBlockerFor re-checks successor validity, the live
// nominate ACL, and — only on the approval that completes both steps — that
// the hook still exists. The live GET needs Manage*Webhooks on someone
// else's hook, so running it on the first (security) step would 403 for
// reviewers who are not System Admins.
func (p *Plugin) webhookTransferApprovalBlockerFor(req *webhookTransferRequest, actingUser *model.User, final bool) string {
	// A cutover waiting to be resumed has already deleted the old hook and
	// retired its index row; re-checking them would block the retry.
	if req.cutoverInProgress() {
		return ""
	}
	index, indexErr := p.loadWebhookIndex(req.HookID)
	if indexErr != nil {
		return "Could not re-check webhook ownership; try Approve again."
	}
	if index == nil || index.RetiredAt != 0 {
		return "That webhook is no longer managed (or was retired). Deny this request instead."
	}
	if req.Action == transferActionReassign && p.webhookReassignUnsupported() {
		return webhookReassignUnsupportedMsg + " Deny this request, and ask for Retire & recreate."
	}
	// Retire deletes the hook, so a hook that is already gone is the wanted
	// end state: let fulfill run and retire the index row. Reassign and
	// recreate need the live hook, so they still fail closed.
	if final && actingUser != nil && req.Action != transferActionRetireOnly {
		if exists, existErr := p.webhookStillExists(req.WebhookType, req.HookID, actingUser.Id); existErr != nil {
			return "Could not re-check whether the webhook still exists; try Approve again."
		} else if !exists {
			return "That webhook no longer exists in Mattermost. Deny this request instead."
		}
	}
	actingID := ""
	if actingUser != nil {
		actingID = actingUser.Id
	}
	// Reading someone else's hook needs Manage*Webhooks, so the live owner
	// is only consulted on the step that carries the change out.
	liveReaderID := ""
	if final {
		liveReaderID = actingID
	}
	ownerID := p.webhookOwnerForChecks(index, liveReaderID)
	// A reassign whose index write failed after Mattermost moved the hook
	// shows the successor as the live owner already; the owner-based checks
	// below would refuse the retry that has to finish the index update.
	if req.Action == transferActionReassign && ownerID != "" && ownerID == req.NewOwnerUserID {
		return ""
	}
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
			// Mattermost has moved the hook, but the index still names the
			// previous owner and that row is what grants transfer rights.
			// Keep the ticket open rather than closing it as a partial
			// success with the old owner still able to act on the hook.
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key,
				errors.Wrap(idxErr, "the webhook owner was changed in Mattermost but the Request Hub index still names the previous owner; retry Approve to finish the handover"))
		}
		p.releaseTransferTicketLease(req.transferLease(), req.RequesterID)
		outcome := fmt.Sprintf("✅ Fully approved. %s webhook **%s** reassigned to %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
		cardText := fmt.Sprintf("Fully approved. The %s webhook **%s** now belongs to %s.", req.WebhookType, escapeMarkdown(req.DisplayName), p.mentionList([]string{req.NewOwnerUserID}))
		p.auditWebhookTransferApproved(cards, req, actingUser, requester, nil)
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
		p.releaseTransferTicketLease(req.transferLease(), req.RequesterID)
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

// Cutover stages for retire+recreate. The replacement hook is created
// before the old one is deleted, so between those calls Mattermost holds
// state that only the pending record points at — final approval deleted
// that record when it claimed the ticket. Each stage is therefore written
// back to the record before the next irreversible call, never after.
const (
	// cutoverStagePreparing: this attempt has cleared whatever the last one
	// left behind and is getting ready to ask for a replacement. Nothing
	// exists in Mattermost yet, but the ticket must not read as a plain
	// pending record — a Deny landing here would delete it and free the
	// claim while the attempt goes on to create a hook.
	cutoverStagePreparing = "preparing"
	// cutoverStageCreating: Mattermost has been asked to make the
	// replacement hook but its ID is not known yet. A lost response leaves
	// an orphan that only this stage records.
	cutoverStageCreating = "creating"
	// cutoverStageOrphanCheck: an earlier create never reported back and
	// the ticket has been paused once, naming what to look for. The next
	// Approve is a System Admin's word that it is safe to build another.
	cutoverStageOrphanCheck = "orphan_check"
	// cutoverStageReplacementReady: the replacement exists (an outgoing
	// hook staged on a temporary trigger); the old hook may still be live.
	cutoverStageReplacementReady = "replacement_ready"
	// cutoverStageOldDeleted: the old hook is gone; an outgoing
	// replacement may still be on its temporary trigger.
	cutoverStageOldDeleted = "old_deleted"
	// cutoverStageTriggersRestored: the outgoing replacement carries its
	// real trigger words. A retry must not put them back a second time and
	// overwrite edits the new owner has made since.
	cutoverStageTriggersRestored = "triggers_restored"
	// cutoverStageCredentialsPending: the successor's secret is about to go
	// out. A resume from here cannot tell whether it arrived, so the DM it
	// sends says a copy may already have landed.
	cutoverStageCredentialsPending = "credentials_pending"
	// cutoverStageCredentialsDelivered: the successor holds the secret;
	// only the index row and the ticket close are left.
	cutoverStageCredentialsDelivered = "credentials_delivered"
	// cutoverStageCleanupPending: a replacement exists that nobody can use
	// — its secret never reached the successor — and it could not be taken
	// back down. The old hook is untouched, so the ticket carries the
	// replacement's ID purely so Deny can remove it and Approve can clear
	// it before creating another.
	cutoverStageCleanupPending = "cleanup_pending"
)

// cutoverStageOrder ranks the stages a handover passes through, so a resume
// can tell which steps are already behind it. Stages that sit outside that
// line (nothing staged yet, or a leftover to clean up) rank zero.
var cutoverStageOrder = map[string]int{
	cutoverStageCreating:             1,
	cutoverStageReplacementReady:     2,
	cutoverStageOldDeleted:           3,
	cutoverStageTriggersRestored:     4,
	cutoverStageCredentialsPending:   5,
	cutoverStageCredentialsDelivered: 6,
}

// cutoverReached reports whether the ticket is at or past stage.
func (req *webhookTransferRequest) cutoverReached(stage string) bool {
	return cutoverStageOrder[req.CutoverStage] >= cutoverStageOrder[stage]
}

const cutoverExpiryBlockedMsg = "This request is past the review window, but its replacement webhook already exists. Click Approve to finish the handover — a System Admin has to clean up by hand if it should be abandoned instead."

// cutoverInProgress reports whether a retire+recreate is part-way through.
// The ticket is the only pointer to a replacement hook, so Expire, Deny and
// Withdraw are refused in that state — including while the create is still
// outstanding, when a hook may exist that nothing else records, and while
// an attempt is preparing one, when a closed ticket would leave the hook it
// is about to make with nothing pointing at it.
func (req *webhookTransferRequest) cutoverInProgress() bool {
	switch req.CutoverStage {
	case cutoverStagePreparing, cutoverStageCreating, cutoverStageOrphanCheck:
		return true
	}
	return strings.TrimSpace(req.ResumeNewHookID) != ""
}

// cutoverOldDeleted reports whether the hook being replaced is already
// gone. ResumeOldDeleted covers records written before CutoverStage.
func (req *webhookTransferRequest) cutoverOldDeleted() bool {
	return req.cutoverReached(cutoverStageOldDeleted) || req.ResumeOldDeleted
}

// stageCutover records everything a later Approve needs to pick the
// handover up at stage.
func (req *webhookTransferRequest) stageCutover(stage string, cut webhookCutover) {
	req.CutoverStage = stage
	req.ResumeOldDeleted = req.cutoverReached(cutoverStageOldDeleted)
	req.ResumeNewHookID = cut.newHookID
	req.ResumeChannelID = cut.channelID
	req.ResumeCallbackURLs = append([]string(nil), cut.callbackURLs...)
	if cut.snap != nil {
		req.ResumeTriggerWords = append([]string(nil), cut.snap.TriggerWords...)
		req.ResumeTriggerWhen = cut.snap.TriggerWhen
	}
}

func (req *webhookTransferRequest) clearCutover() {
	req.CutoverStage = ""
	req.ResumeNewHookID = ""
	req.ResumeOldDeleted = false
	req.ResumeTriggerWords = nil
	req.ResumeTriggerWhen = 0
	req.ResumeChannelID = ""
	req.ResumeCallbackURLs = nil
}

// transferLease names this ticket's per-asset claim, which the cutover
// renews before each side effect so a stalled approval cannot run on after
// the asset was handed to another ticket.
func (req *webhookTransferRequest) transferLease() transferLease {
	return transferLease{
		kind: pendingKindWebhookTransfer, assetKey: req.pendingKey(),
		requestID: req.ID, attemptID: req.attemptID,
	}
}

// webhookCutover is the in-flight state of a retire+recreate: the
// replacement hook, what the finish step still needs, and the bytes of the
// last checkpoint written for it.
type webhookCutover struct {
	checkpoint    []byte
	newHookID     string
	newHookToken  string
	snap          *outgoingWebhookRecreate
	channelID     string
	callbackURLs  []string
	description   string
	resumed       bool
	partialErrors []string
}

// writeCutoverCheckpoint publishes the ticket with its current stage. This
// is the only durable record that a replacement hook exists.
func (p *Plugin) writeCutoverCheckpoint(req *webhookTransferRequest, key string) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to record the webhook cutover checkpoint")
	}
	if err := p.putPendingRequestKV(key, data); err != nil {
		return nil, errors.Wrap(err, "failed to store the webhook cutover checkpoint")
	}
	return data, nil
}

// publishCutoverPreparing puts the ticket back as a cutover this attempt is
// still working on, after it has cleared whatever the last one left behind.
// Claiming the approval deleted the record, so without this write the only
// thing standing between a concurrent Deny and a half-made replacement is
// nothing at all.
//
// The bytes returned are the plain ticket, for the failure paths that
// follow: those end the attempt with nothing staged in Mattermost, so the
// ticket they put back must be deniable and withdrawable again.
func (p *Plugin) publishCutoverPreparing(req *webhookTransferRequest, key string, rawReq []byte) ([]byte, error) {
	req.clearCutover()
	req.CutoverStage = cutoverStagePreparing
	if _, err := p.writeCutoverCheckpoint(req, key); err != nil {
		return rawReq, err
	}
	req.clearCutover()
	return p.cutoverRecordBytes(req, rawReq), nil
}

// clearCutoverCheckpoint removes exactly the checkpoint bytes this approval
// wrote, once the handover is finished. A checkpoint that survives is still
// an open ticket pointing at a live replacement hook, so the caller must not
// close the ticket as approved on an error here.
func (p *Plugin) clearCutoverCheckpoint(key string, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	deleted, appErr := p.API.KVCompareAndDelete(key, raw)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to clear the webhook cutover checkpoint")
	}
	if !deleted {
		return errors.New("the webhook cutover checkpoint changed while the handover was finishing")
	}
	p.untrackOpenRequestKey(key)
	return nil
}

const cutoverStrandedMsg = "The replacement webhook already exists and the old one is gone, so this can't be denied — click Approve to finish the handover."

// cutoverDenyBlocker decides whether a staged cutover can still be denied.
// While the old hook is live the replacement can be taken back down and the
// ticket closed; once the old hook is gone, denying would strand the
// replacement and only Approve can finish the handover. This is read-only:
// nothing is deleted until the deny has claimed the ticket.
func (p *Plugin) cutoverDenyBlocker(req *webhookTransferRequest, actingUserID string) string {
	if req.cutoverOldDeleted() {
		return cutoverStrandedMsg
	}
	if req.CutoverStage == cutoverStagePreparing {
		// Nothing is staged: the last attempt had removed the leftover it
		// found and had not asked for a replacement yet.
		return ""
	}
	if strings.TrimSpace(req.ResumeNewHookID) == "" {
		// The create never reported back, so there is no hook ID to take
		// down and denying would leave an orphan nothing points at.
		return "The last attempt stopped while the replacement webhook was being created, so whether one exists isn't known — click Approve to finish the handover."
	}
	exists, existErr := p.webhookStillExists(req.WebhookType, req.HookID, actingUserID)
	if existErr != nil {
		return "A replacement webhook is already staged and whether the old one still exists could not be checked; try again, or click Approve to finish the handover."
	}
	if !exists {
		return cutoverStrandedMsg
	}
	return ""
}

// denyStagedCutover claims a ticket whose replacement hook already exists,
// then takes that replacement back down. The ticket is the only pointer to
// the replacement, so it is claimed first — nothing may delete a hook while
// another click could still act on the record. ok is false when the deny
// must stop; the ticket (and its checkpoint) is then kept so the cleanup
// can be retried.
func (p *Plugin) denyStagedCutover(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string) (decisionResult, bool) {
	actingID := ""
	if actingUser != nil {
		actingID = actingUser.Id
	}
	if msg := p.cutoverDenyBlocker(req, actingID); msg != "" {
		return decisionResult{Reply: msg}, false
	}
	res, attemptID, ok := p.claimTransferForFulfill(pendingKindWebhookTransfer, req.pendingKey(), req.ID, key, rawReq, cards, "webhook transfer request")
	if !ok {
		return res, false
	}
	req.attemptID = attemptID
	// The old hook may have gone away between the blocker and the claim,
	// which would make deleting the replacement strand the webhook entirely.
	exists, existErr := p.webhookStillExists(req.WebhookType, req.HookID, actingID)
	switch {
	case existErr != nil:
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key,
			errors.Wrap(existErr, "whether the old webhook still exists could not be re-checked before removing the replacement")), false
	case !exists:
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key,
			errors.New(cutoverStrandedMsg)), false
	}
	if delErr := p.deleteWebhook(req.WebhookType, req.ResumeNewHookID, actingID); delErr != nil && !webhookDeleteAlreadyGone(delErr) {
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key,
			errors.Wrap(delErr, "the staged replacement webhook could not be removed")), false
	}
	return decisionResult{}, true
}

// fulfillWebhookRetireRecreate creates the replacement under the successor
// first, then runs the cutover. Outgoing hooks load the live Mattermost
// record so trigger words and every callback URL are preserved; if the live
// hook cannot be loaded and the snapshot is incomplete, the request stays
// pending (refuse) instead of dropping fidelity.
func (p *Plugin) fulfillWebhookRetireRecreate(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string) decisionResult {
	// The last attempt left a replacement nobody can use. Take it down
	// before making another, or the channel collects duplicate hooks.
	if req.CutoverStage == cutoverStageCleanupPending {
		stale := strings.TrimSpace(req.ResumeNewHookID)
		if delErr := p.deleteWebhook(req.WebhookType, stale, actingUser.Id); delErr != nil && !webhookDeleteAlreadyGone(delErr) {
			return p.pauseCutover(req, actingUser, requester, cards, key,
				webhookCutover{newHookID: stale, channelID: req.ResumeChannelID, checkpoint: rawReq},
				errors.Wrap(delErr, "the unused replacement webhook left by the last attempt could not be removed"))
		}
		var cpErr error
		if rawReq, cpErr = p.publishCutoverPreparing(req, key, rawReq); cpErr != nil {
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, cpErr)
		}
	}
	// A previous approval already made the replacement; finish that cutover
	// instead of creating a second hook.
	if strings.TrimSpace(req.ResumeNewHookID) != "" {
		return p.resumeWebhookRetireRecreate(req, actingUser, requester, cards, rawReq, key)
	}
	// A checkpoint stuck at "creating" means the last attempt never learned
	// the replacement's ID, so a hook may exist that nothing points at.
	// Building another one silently would double it; pause once naming what
	// to look for, and treat the next Approve as the word that it is dealt
	// with.
	var orphanWarning []string
	switch req.CutoverStage {
	case cutoverStageCreating:
		p.API.LogWarn("pausing a webhook cutover that stopped while the replacement was being created", "request_id", req.ID, "webhook_type", req.WebhookType)
		req.CutoverStage = cutoverStageOrphanCheck
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, p.cutoverRecordBytes(req, rawReq), key,
			errors.New("the last attempt stopped while the replacement was being created, so "+p.cutoverOrphanWarning(req)+
				"; deal with it and click Approve again to build a fresh replacement"))
	case cutoverStageOrphanCheck:
		orphanWarning = append(orphanWarning, p.cutoverOrphanWarning(req))
		var cpErr error
		if rawReq, cpErr = p.publishCutoverPreparing(req, key, rawReq); cpErr != nil {
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, cpErr)
		}
	}

	var outgoingSnap *outgoingWebhookRecreate
	callbackURL := strings.TrimSpace(req.CallbackURL)
	channelID := req.ChannelID
	if req.WebhookType == webhookTypeIncoming {
		live, chErr := p.resolveIncomingRecreateChannel(req, actingUser.Id)
		if chErr != nil {
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, chErr)
		}
		channelID = live
	}
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
			// Only the index snapshot's callback is known; it records no channel.
			outgoingSnap = &outgoingWebhookRecreate{CallbackURLs: []string{callbackURL}, channelUnknown: true}
		}
		req.CallbackURL = callbackURL
		// The hook may have been moved since the ticket was filed. Creating
		// the replacement in the approved channel would publish it somewhere
		// the reviewers never looked at.
		if liveChannel := strings.TrimSpace(outgoingSnap.ChannelID); liveChannel != "" {
			if channelID != "" && liveChannel != channelID {
				return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key,
					errors.New("the outgoing webhook now lives in a different channel than this request approved; deny it and submit a new request for the hook's current channel"))
			}
			channelID = liveChannel
		} else if channelID != "" && !outgoingSnap.channelUnknown {
			// The hook fires team-wide now. Recreating it in the channel the
			// ticket named would quietly narrow its scope, and recreating it
			// team-wide was never reviewed.
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key,
				errors.New("the outgoing webhook is no longer limited to the channel this request approved; deny it and recreate the hook by hand"))
		}
	}

	// The successor was vetted against the channel the ticket named; a hook
	// that lives somewhere else now puts the replacement — and its secret —
	// in a channel nobody checked them for.
	if channelID != "" && channelID != req.ChannelID {
		if err := p.checkTransferNewOwner(req.NewOwnerUserID, transferNewOwnerOpts{
			channelID: channelID, webhookType: req.WebhookType, noun: "webhook",
		}); err != nil {
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key,
				errors.Wrap(err, "the new owner can't take the webhook in the channel it lives in now"))
		}
	}

	synthetic := &webhookRequest{
		ID:          req.ID,
		RequesterID: req.NewOwnerUserID,
		WebhookType: req.WebhookType,
		ChannelID:   channelID,
		TeamID:      req.TeamID,
		DisplayName: req.DisplayName,
		Description: p.webhookRecreateDescription(req),
		CallbackURL: callbackURL,
	}
	if outgoingSnap != nil && strings.TrimSpace(outgoingSnap.Description) != "" {
		synthetic.Description = strings.TrimSpace(outgoingSnap.Description)
	}
	// Everything above is read-only, and a slow read can outlast the lease.
	// Confirm this attempt still owns the asset before it publishes an
	// intent — or a hook — against someone else's ticket.
	if leaseErr := p.renewTransferLease(req.transferLease()); leaseErr != nil {
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, leaseErr)
	}
	// Record the intent before Mattermost is asked for the hook: the pending
	// record went away when the approval was claimed, so a lost create
	// response would otherwise leave a hook nothing at all points at.
	req.CutoverStage = cutoverStageCreating
	req.ResumeNewHookID = ""
	req.ResumeChannelID = channelID
	creatingRaw, intentErr := p.writeCutoverCheckpoint(req, key)
	if intentErr != nil {
		req.clearCutover()
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, intentErr)
	}

	partialErrors := orphanWarning
	var dmMsg, newHookID, newHookToken string
	var createErr error
	if req.WebhookType == webhookTypeOutgoing && outgoingSnap != nil {
		dmMsg, newHookID, newHookToken, createErr = p.createOutgoingWebhookFromRecreate(synthetic, outgoingSnap, actingUser.Id)
	} else {
		dmMsg, newHookID, createErr = p.createWebhookForRequest(synthetic, actingUser.Id)
	}
	if createErr != nil {
		if !webhookCreateRefused(createErr) {
			// A lost response may still have made a hook. Keep the creating
			// checkpoint so the next attempt names the orphan instead of
			// leaving one nothing at all points at.
			return p.pauseCutover(req, actingUser, requester, cards, key,
				webhookCutover{channelID: channelID, checkpoint: creatingRaw}, createErr)
		}
		// The server turned the create down, so no hook exists and the old
		// one is untouched — leave the ticket plain for a retry.
		req.clearCutover()
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, p.cutoverRecordBytes(req, creatingRaw), key, createErr)
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
				cause += ", and the unused replacement hook could not be deleted — a retry removes it before making another"
				// Keep the only pointer to that live hook: dropping it here
				// strands an incoming webhook whose URL nobody holds.
				req.stageCutover(cutoverStageCleanupPending, webhookCutover{newHookID: newHookID, channelID: channelID})
				return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, p.cutoverRecordBytes(req, creatingRaw), key, errors.New(cause))
			}
			req.clearCutover()
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, errors.New(cause))
		}
	}

	cut := webhookCutover{
		newHookID:     newHookID,
		newHookToken:  newHookToken,
		snap:          outgoingSnap,
		channelID:     channelID,
		callbackURLs:  recreateCallbackURLs(callbackURL, outgoingSnap),
		description:   synthetic.Description,
		partialErrors: partialErrors,
	}
	// Checkpoint before the old hook is deleted: the pending record went
	// away when the approval was claimed, so until this lands nothing
	// durable knows the replacement exists.
	req.stageCutover(cutoverStageReplacementReady, cut)
	checkpoint, cpErr := p.writeCutoverCheckpoint(req, key)
	if cpErr != nil {
		msg := "the cutover could not be recorded (" + cpErr.Error() + "), so the replacement was removed and the old webhook left in place"
		if delErr := p.deleteWebhook(req.WebhookType, newHookID, actingUser.Id); delErr != nil && !webhookDeleteAlreadyGone(delErr) {
			p.API.LogError("failed to delete replacement webhook after the cutover checkpoint failed", "error", delErr.Error())
			msg = "the cutover could not be recorded (" + cpErr.Error() + ") and the unused replacement hook could not be deleted either — a retry removes it before making another"
			req.stageCutover(cutoverStageCleanupPending, cut)
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, p.cutoverRecordBytes(req, creatingRaw), key, errors.New(msg))
		}
		req.clearCutover()
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, errors.New(msg))
	}
	cut.checkpoint = checkpoint
	return p.advanceWebhookCutover(req, actingUser, requester, cards, key, cut)
}

// resolveIncomingRecreateChannel returns the channel an incoming
// replacement must be built in. Same risk as an outgoing hook that moved:
// rebuilding it in the approved channel would publish it somewhere the hook
// has already left.
func (p *Plugin) resolveIncomingRecreateChannel(req *webhookTransferRequest, adminUserID string) (string, error) {
	live, err := p.liveIncomingWebhookChannelID(req.HookID, adminUserID)
	if err != nil {
		return "", errors.Wrap(err, "could not confirm which channel the incoming webhook posts to; retry Approve")
	}
	if live == "" {
		return req.ChannelID, nil
	}
	if req.ChannelID != "" && live != req.ChannelID {
		return "", errors.New("the incoming webhook now posts to a different channel than this request approved; deny it and submit a new request for the hook's current channel")
	}
	return live, nil
}

// webhookCreateRefused reports a create the server clearly turned down, so
// no hook can exist. Anything else — a timeout, a 5xx, a dropped
// connection — may have landed, and the caller has to keep its checkpoint.
func webhookCreateRefused(err error) bool {
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr == nil {
		return false
	}
	return appErr.StatusCode >= http.StatusBadRequest && appErr.StatusCode < http.StatusInternalServerError
}

// advanceWebhookCutover runs every stage left once the replacement hook
// exists: delete the old hook, retire the old index row, put the real
// triggers back, then hand the hook over. The claim lease is renewed before
// each REST call, so an approval that lost the asset stops instead of
// acting on someone else's ticket.
func (p *Plugin) advanceWebhookCutover(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, key string, cut webhookCutover) decisionResult {
	lease := req.transferLease()

	// A resumed cutover can be days after the approval, so the replacement
	// and the successor are re-checked before anything is torn down: an
	// ineligible successor must not cost the requester the hook that is
	// still live.
	if cut.resumed {
		if res, stop := p.confirmResumedReplacement(req, actingUser, requester, cards, key, cut); stop {
			return res
		}
		if err := p.checkCutoverSuccessor(req, cut, actingUser.Id); err != nil {
			return p.pauseCutover(req, actingUser, requester, cards, key, cut, err)
		}
	}

	if !req.cutoverOldDeleted() {
		if leaseErr := p.renewTransferLease(lease); leaseErr != nil {
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, cut.checkpoint, key, leaseErr)
		}
		oldGone := true
		if err := p.deleteWebhook(req.WebhookType, req.HookID, actingUser.Id); err != nil && !webhookDeleteAlreadyGone(err) {
			// Mattermost may have applied the delete and lost the response,
			// so look the hook up before tearing the replacement down.
			exists, existErr := p.webhookStillExists(req.WebhookType, req.HookID, actingUser.Id)
			switch {
			case existErr != nil:
				// Ambiguous. An outgoing replacement is parked on a
				// temporary trigger, so it cannot fire next to the old
				// hook — keep it and resume on retry.
				return p.pauseCutover(req, actingUser, requester, cards, key, cut,
					errors.Errorf("the old webhook could not be deleted (%s) and whether it still exists could not be checked (%s); the replacement is parked — retry Approve", err.Error(), existErr.Error()))
			case exists && req.WebhookType == webhookTypeOutgoing:
				// Both hooks are live; take the replacement back down.
				if delErr := p.deleteWebhook(req.WebhookType, cut.newHookID, actingUser.Id); delErr != nil && !webhookDeleteAlreadyGone(delErr) {
					p.API.LogError("failed to delete replacement outgoing webhook after old delete failed", "hook_id", cut.newHookID, "error", delErr.Error())
					// The replacement is still live and parked on its
					// temporary trigger. Keep the checkpoint pointing at it,
					// or nothing would know it has to be cleaned up.
					return p.pauseCutover(req, actingUser, requester, cards, key, cut,
						errors.Errorf("the old outgoing webhook could not be deleted (%s) and the new hook could not be removed either (%s); the replacement is parked — retry Approve", err.Error(), delErr.Error()))
				}
				// The replacement is gone, so the ticket must go back
				// without the checkpoint that claims it exists.
				req.clearCutover()
				return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, p.cutoverRecordBytes(req, cut.checkpoint), key,
					errors.New("the old outgoing webhook could not be deleted: "+err.Error()+"; the new hook was removed and the old one left in place"))
			case exists:
				// Incoming: the successor already holds the new URL, so the
				// replacement stays and the old index row keeps pointing at
				// a hook the hub must still be able to target.
				oldGone = false
				cut.partialErrors = append(cut.partialErrors, "the old webhook could not be deleted after recreate: "+err.Error()+" — delete the old hook manually; the new hook was DM'd to the new owner")
			}
		}
		if oldGone {
			req.stageCutover(cutoverStageOldDeleted, cut)
			if raw, cpErr := p.writeCutoverCheckpoint(req, key); cpErr != nil {
				cut.partialErrors = append(cut.partialErrors, "the cutover checkpoint could not be updated: "+cpErr.Error())
			} else {
				cut.checkpoint = raw
			}
		}
	}

	if req.cutoverOldDeleted() {
		if idxErr := p.retireWebhookIndex(req.HookID); idxErr != nil {
			cut.partialErrors = append(cut.partialErrors, "old webhook index could not be marked retired: "+idxErr.Error())
		}
	}
	// The replacement is deliberately not indexed yet: a row in
	// Transfer/Retire is an invitation for another ticket to retire the
	// hook while this handover is still half-done. It goes in at the end,
	// in finishWebhookRecreate.
	if req.WebhookType == webhookTypeOutgoing && req.cutoverOldDeleted() && cut.snap != nil &&
		!req.cutoverReached(cutoverStageTriggersRestored) {
		if leaseErr := p.renewTransferLease(lease); leaseErr != nil {
			return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, cut.checkpoint, key, leaseErr)
		}
		if trigErr := p.restoreOutgoingWebhookTriggers(cut.newHookID, actingUser.Id, cut.snap); trigErr != nil {
			return p.pauseCutover(req, actingUser, requester, cards, key, cut, trigErr)
		}
		// The real triggers are live. Record that before the handover goes
		// any further: a retry that put them back a second time would wipe
		// whatever the new owner has changed since.
		cut.checkpoint = p.checkpointCutoverStage(req, key, cutoverStageTriggersRestored, &cut)
	}
	return p.finishWebhookRecreate(req, actingUser, requester, cards, key, cut)
}

// checkpointCutoverStage records a step that is now behind the handover, so
// a retry does not run it again. A write that fails is noted and the last
// checkpoint kept: the step really did happen, and losing the note costs a
// repeat rather than the hook.
func (p *Plugin) checkpointCutoverStage(req *webhookTransferRequest, key, stage string, cut *webhookCutover) []byte {
	req.stageCutover(stage, *cut)
	raw, err := p.writeCutoverCheckpoint(req, key)
	if err != nil {
		cut.partialErrors = append(cut.partialErrors, "the cutover checkpoint could not be updated after "+stage+": "+err.Error())
		return cut.checkpoint
	}
	return raw
}

// confirmResumedReplacement makes sure the hook a resumed checkpoint points
// at is really there before the old one is torn down. A checkpoint that
// outlived its replacement would otherwise cost the requester both hooks.
// stop is true when the caller must not go on.
func (p *Plugin) confirmResumedReplacement(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, key string, cut webhookCutover) (decisionResult, bool) {
	if req.cutoverOldDeleted() {
		// Nothing left to tear down, so a missing replacement is for the
		// finish step to report rather than a reason to stop here.
		return decisionResult{}, false
	}
	exists, err := p.webhookStillExists(req.WebhookType, cut.newHookID, actingUser.Id)
	switch {
	case err != nil:
		return p.pauseCutover(req, actingUser, requester, cards, key, cut,
			errors.Wrap(err, "whether the replacement webhook still exists could not be checked; the old hook was left in place — retry Approve")), true
	case !exists:
		// The old hook is still live and nothing replaced it, so drop the
		// checkpoint and let a retry build a fresh replacement.
		req.clearCutover()
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, p.cutoverRecordBytes(req, cut.checkpoint), key,
			errors.New("the replacement webhook this handover recorded no longer exists; the old hook was left in place — retry Approve to build a new one")), true
	}
	return decisionResult{}, false
}

// checkCutoverSuccessor re-runs the successor checks against the channel
// the replacement lives in now. A staged hook can be moved while its
// cutover sits parked, and the channel the checkpoint recorded is then the
// wrong one to vet anybody against — the successor would be handed a hook,
// and its secret, somewhere nobody checked them for.
func (p *Plugin) checkCutoverSuccessor(req *webhookTransferRequest, cut webhookCutover, adminUserID string) error {
	approved := cut.channelID
	if approved == "" {
		approved = req.ChannelID
	}
	channelID, known := p.liveWebhookChannelID(req.WebhookType, cut.newHookID, adminUserID)
	if !known {
		return errors.New("could not confirm which channel the replacement webhook posts to; retry Approve")
	}
	if channelID != approved {
		return errors.New("the replacement webhook no longer sits where this handover approved it; a System Admin has to move it back or remove it, then Approve again")
	}
	if err := p.checkTransferNewOwner(req.NewOwnerUserID, transferNewOwnerOpts{
		channelID: channelID, webhookType: req.WebhookType, noun: "webhook",
	}); err != nil {
		return errors.Wrap(err, "the new owner can no longer take this webhook")
	}
	return nil
}

// indexReplacementWebhook lists the replacement under the successor. It is
// create-if-absent and safe to repeat: a row written by a later owner must
// win over a resumed cutover.
func (p *Plugin) indexReplacementWebhook(req *webhookTransferRequest, cut webhookCutover) error {
	channelID := cut.channelID
	if channelID == "" {
		channelID = req.ChannelID
	}
	callbackURL := strings.TrimSpace(req.CallbackURL)
	if len(cut.callbackURLs) > 0 {
		callbackURL = strings.TrimSpace(cut.callbackURLs[0])
	}
	description := cut.description
	if description == "" {
		description = p.webhookRecreateDescription(req)
	}
	return p.writeWebhookIndexIfAbsent(&webhookRequest{
		ID:          req.ID,
		RequesterID: req.NewOwnerUserID,
		WebhookType: req.WebhookType,
		ChannelID:   channelID,
		TeamID:      req.TeamID,
		DisplayName: req.DisplayName,
		Description: description,
		CallbackURL: callbackURL,
		Exercise:    req.Exercise,
	}, cut.newHookID)
}

// pauseCutover puts the ticket back carrying the stage this attempt reached,
// so a retry picks up from there. The approval is not closed: an outgoing
// replacement is still on its temporary trigger, so the hook is not usable
// until Approve is clicked again.
func (p *Plugin) pauseCutover(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, key string, cut webhookCutover, cause error) decisionResult {
	stage := req.CutoverStage
	if stage == "" {
		stage = cutoverStageReplacementReady
	}
	req.stageCutover(stage, cut)
	return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, p.cutoverRecordBytes(req, cut.checkpoint), key, cause)
}

// cutoverOrphanWarning describes the hook a create that never reported back
// may have left behind, in terms a System Admin can search on. Outgoing
// replacements are staged on a trigger word carrying the request ID.
func (p *Plugin) cutoverOrphanWarning(req *webhookTransferRequest) string {
	if req.WebhookType == webhookTypeOutgoing {
		return "an earlier attempt may have left an unused outgoing webhook staged on a trigger word starting " +
			inlineCode(cutoverStagingTriggerPrefix+req.ID) + " — a System Admin should delete it"
	}
	return "an earlier attempt may have left an unused incoming webhook named " + inlineCode(req.DisplayName) +
		" in this channel — a System Admin should delete it"
}

// cutoverRecordBytes marshals the ticket as it stands now, falling back to
// the last checkpoint when it cannot be marshalled.
func (p *Plugin) cutoverRecordBytes(req *webhookTransferRequest, fallback []byte) []byte {
	raw, err := json.Marshal(req)
	if err != nil {
		p.API.LogError("failed to record webhook cutover state", "request_id", req.ID, "error", err.Error())
		return fallback
	}
	return raw
}

// resumeWebhookRetireRecreate finishes a cutover whose replacement already
// exists: delete the old hook if that is still outstanding, put the real
// trigger words back, and close the ticket.
func (p *Plugin) resumeWebhookRetireRecreate(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string) decisionResult {
	cut := webhookCutover{
		newHookID:    strings.TrimSpace(req.ResumeNewHookID),
		channelID:    strings.TrimSpace(req.ResumeChannelID),
		callbackURLs: append([]string(nil), req.ResumeCallbackURLs...),
		resumed:      true,
	}
	if cut.channelID == "" {
		cut.channelID = req.ChannelID
	}
	if len(cut.callbackURLs) == 0 {
		cut.callbackURLs = []string{strings.TrimSpace(req.CallbackURL)}
	}
	if req.WebhookType == webhookTypeOutgoing {
		cut.snap = &outgoingWebhookRecreate{
			ChannelID:    cut.channelID,
			CallbackURLs: append([]string(nil), cut.callbackURLs...),
			TriggerWords: append([]string(nil), req.ResumeTriggerWords...),
			TriggerWhen:  req.ResumeTriggerWhen,
		}
	}
	// A crash can leave the checkpoint behind what actually happened: the
	// old hook may already be gone even though the stage still says live.
	if !req.cutoverOldDeleted() {
		if exists, existErr := p.webhookStillExists(req.WebhookType, req.HookID, actingUser.Id); existErr == nil && !exists {
			req.stageCutover(cutoverStageOldDeleted, cut)
		}
	}
	checkpoint, cpErr := p.writeCutoverCheckpoint(req, key)
	if cpErr != nil {
		return p.restoreWebhookTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, cpErr)
	}
	cut.checkpoint = checkpoint
	return p.advanceWebhookCutover(req, actingUser, requester, cards, key, cut)
}

func recreateCallbackURLs(callbackURL string, snap *outgoingWebhookRecreate) []string {
	if snap != nil && len(snap.CallbackURLs) > 0 {
		return append([]string(nil), snap.CallbackURLs...)
	}
	return []string{callbackURL}
}

// finishWebhookRecreate DMs the successor what they need, lists the
// replacement, clears the cutover checkpoint and closes the ticket. The
// outgoing hook's token is a secret, so it only ever goes out in an
// auto-deleting DM — never on the approval card or in the audit trail.
//
// Each step is written back before the next, so a close that has to be
// retried picks up where it stopped instead of re-sending the secret and
// re-stamping triggers the new owner may have edited since.
func (p *Plugin) finishWebhookRecreate(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, key string, out webhookCutover) decisionResult {
	if req.WebhookType == webhookTypeOutgoing && !req.cutoverReached(cutoverStageCredentialsDelivered) {
		if res, stop := p.deliverRecreatedWebhookToken(req, actingUser, requester, cards, key, &out); stop {
			return res
		}
	}
	partialErrors := out.partialErrors

	// The replacement only joins Transfer/Retire now the handover is done:
	// a row that appears earlier lets another ticket retire the hook while
	// this one is still half-way through. A failed write keeps the ticket,
	// because a live hook nothing lists is a hook nobody can hand on.
	if idxErr := p.indexReplacementWebhook(req, out); idxErr != nil {
		return p.pauseCutover(req, actingUser, requester, cards, key, out,
			errors.Wrap(idxErr, "the replacement webhook could not be listed in Transfer/Retire; it exists and the handover is otherwise done — retry Approve"))
	}

	if clearErr := p.clearCutoverCheckpoint(key, out.checkpoint); clearErr != nil {
		// The checkpoint is still on disk naming a live replacement hook.
		// Closing the ticket now would leave that record with nothing to
		// resolve it, so keep it and let Approve run again.
		return p.pauseCutover(req, actingUser, requester, cards, key, out, clearErr)
	}
	req.clearCutover()
	p.releaseTransferTicketLease(req.transferLease(), req.RequesterID)
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

// deliverRecreatedWebhookToken hands the successor the outgoing hook's new
// token. The intent is written down first, so a retry that cannot tell
// whether the DM arrived says so rather than quietly sending a second copy,
// and the delivery is written down after, so a retry past this point does
// not send one at all. stop is true when the caller must not go on.
func (p *Plugin) deliverRecreatedWebhookToken(req *webhookTransferRequest, actingUser, requester *model.User, cards decisionCards, key string, out *webhookCutover) (decisionResult, bool) {
	resend := req.cutoverReached(cutoverStageCredentialsPending)
	if !resend {
		req.stageCutover(cutoverStageCredentialsPending, *out)
		raw, cpErr := p.writeCutoverCheckpoint(req, key)
		if cpErr != nil {
			return p.pauseCutover(req, actingUser, requester, cards, key, *out,
				errors.Wrap(cpErr, "the handover could not be recorded before the new token was sent; nothing was sent — retry Approve")), true
		}
		out.checkpoint = raw
	}
	if out.newHookToken == "" {
		// A resumed cutover never saw the create response.
		if token, tokErr := p.outgoingWebhookToken(out.newHookID, actingUser.Id); tokErr != nil {
			p.API.LogWarn("failed to read recreated outgoing webhook token", "request_id", req.ID, "error", tokErr.Error())
		} else {
			out.newHookToken = token
		}
	}
	lines := []string{fmt.Sprintf("Your outgoing webhook **%s** was recreated under your account.\n**Callback URL(s) (unchanged):** %s",
		escapeMarkdown(req.DisplayName), inlineCode(strings.Join(out.callbackURLs, ", ")))}
	hasSecret := out.newHookToken != ""
	if hasSecret {
		lines = append(lines,
			fmt.Sprintf("🔑 **New webhook token:** %s", inlineCode(out.newHookToken)),
			"⚠️ **Save this now.** This message auto-deletes in ~72 hours.")
	} else {
		lines = append(lines, "The hook has a new token — find it under **Integrations → Outgoing Webhooks**.")
	}
	if resend {
		lines = append(lines, "_An earlier attempt may already have sent you this — it is the same hook and the same token._")
	}
	if !p.sendTransferSecretsDM(req.NewOwnerUserID, req.ID, lines, hasSecret) {
		if hasSecret {
			out.partialErrors = append(out.partialErrors, "the new webhook token could not be DM'd to the new owner — they can read it under Integrations → Outgoing Webhooks")
		}
		return decisionResult{}, false
	}
	out.checkpoint = p.checkpointCutoverStage(req, key, cutoverStageCredentialsDelivered, out)
	return decisionResult{}, false
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

// cutoverStagingTriggerPrefix leads the temporary trigger word a recreated
// outgoing hook is parked on. The request ID follows it, so a hook left
// behind by a create whose response was lost can be traced to its ticket.
const cutoverStagingTriggerPrefix = "rh-recreate-"

// outgoingWebhookRecreate carries the live fields retire+recreate must copy.
type outgoingWebhookRecreate struct {
	// ChannelID is where the hook lives now, which may differ from the
	// channel the ticket was approved for. Empty means the hook fires
	// team-wide, unless channelUnknown says it was never read.
	ChannelID string
	// channelUnknown marks a snapshot that carries no channel at all,
	// rather than a hook that is deliberately team-wide.
	channelUnknown bool
	// CreatorID is the hook's live Mattermost owner, which the RH index can
	// disagree with. Empty when the read could not supply it.
	CreatorID    string
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
		return &outgoingWebhookRecreate{CallbackURLs: []string{cb}, ContentType: "application/json", channelUnknown: true}, nil
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
		ChannelID:    hook.ChannelId,
		CreatorID:    hook.CreatorId,
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
// live trigger/callback fields onto the successor's ownership. The hook is
// always staged on a temporary unique trigger so it cannot fire alongside
// the hook it replaces; restoreOutgoingWebhookTriggers puts the real
// triggers on once the old hook is gone.
func (p *Plugin) createOutgoingWebhookFromRecreate(req *webhookRequest, snap *outgoingWebhookRecreate, adminUserID string) (dmMsg, hookID, token string, err error) {
	if snap == nil || len(snap.CallbackURLs) == 0 {
		return "", "", "", errors.New("outgoing webhook recreate is missing callback URLs")
	}
	if p.createOutgoingWebhookFromRecreateFn != nil {
		return p.createOutgoingWebhookFromRecreateFn(req, snap, adminUserID)
	}
	contentType := strings.TrimSpace(snap.ContentType)
	if contentType == "" {
		contentType = "application/json"
	}
	internalURL := p.internalBaseURL()
	if internalURL == "" {
		return "", "", "", errors.New("site URL is not configured — cannot create webhook")
	}
	client, cleanup, sessErr := p.transferClient(adminUserID)
	if sessErr != nil {
		return "", "", "", sessErr
	}
	defer cleanup()

	hook, _, apiErr := client.CreateOutgoingWebhook(context.Background(), &model.OutgoingWebhook{
		TeamId:       req.TeamID,
		ChannelId:    req.ChannelID,
		CreatorId:    req.RequesterID,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		CallbackURLs: append([]string(nil), snap.CallbackURLs...),
		TriggerWords: []string{cutoverStagingTriggerPrefix + req.ID + "-" + model.NewId()},
		TriggerWhen:  0,
		ContentType:  contentType,
		Username:     snap.Username,
		IconURL:      snap.IconURL,
	})
	if apiErr != nil {
		return "", "", "", errors.Wrap(apiErr, "failed to recreate outgoing webhook")
	}
	return fmt.Sprintf("Your outgoing webhook **%s** was approved and is now active.", escapeMarkdown(hook.DisplayName)), hook.Id, hook.Token, nil
}

// outgoingWebhookToken reads a live outgoing hook's token, so a resumed
// cutover can still hand the successor the secret it needs.
func (p *Plugin) outgoingWebhookToken(hookID, adminUserID string) (string, error) {
	if strings.TrimSpace(hookID) == "" {
		return "", errors.New("the recreated hook's ID is unknown")
	}
	if p.outgoingWebhookTokenFn != nil {
		return p.outgoingWebhookTokenFn(hookID, adminUserID)
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return "", err
	}
	defer cleanup()
	hook, _, apiErr := client.GetOutgoingWebhook(context.Background(), hookID)
	if apiErr != nil {
		return "", errors.Wrap(apiErr, "failed to load recreated outgoing webhook")
	}
	if hook == nil {
		return "", errors.New("recreated outgoing webhook not found")
	}
	return hook.Token, nil
}

// restoreOutgoingWebhookTriggers replaces the staging trigger on a recreated
// hook with the live snapshot's triggers, after the old overlapping hook has
// been deleted. An empty snapshot trigger list still has to be applied, or
// the temporary trigger would stay live.
func (p *Plugin) restoreOutgoingWebhookTriggers(hookID, adminUserID string, snap *outgoingWebhookRecreate) error {
	if snap == nil || strings.TrimSpace(hookID) == "" {
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
	restored, restoreErr := p.restoreTransferRecord(req.transferLease(), key, rawReq)
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
