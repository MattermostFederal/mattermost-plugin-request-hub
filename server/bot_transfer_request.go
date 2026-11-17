package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	kvBotTransferRequestPrefix = "bot_transfer_request_"

	botTransferDialogCallbackID = "bot_transfer_dialog"
	fieldTransferBotID          = "transfer_bot_id"
	fieldTransferAction         = "transfer_action"
	fieldTransferNewOwner       = "transfer_new_owner"
	fieldTransferReason         = "transfer_reason"
	fieldTransferRotate         = "transfer_rotate"
	fieldTransferNote           = "transfer_note"

	botTransferRequestsDisabledMsg = "Bot transfer/retire requests are currently disabled by an administrator."
)

// botTransferRequest transfers an RH-managed bot to a new owner, or retires
// it. Because it changes integration ownership and credentials it routes to
// the bot approval channel and needs two-step approval (security + system),
// the same as a bot request. Bot-owned RH webhooks ride with the bot — there
// is no separate hook ownership change on transfer.
type botTransferRequest struct {
	ID          string `json:"id"`
	RequesterID string `json:"requester_id"`
	BotUserID   string `json:"bot_user_id"`
	BotUsername string `json:"bot_username"`
	// Action is transfer or retire.
	Action string `json:"action"`
	// NewOwnerUserID is required for transfer.
	NewOwnerUserID string `json:"new_owner_user_id,omitempty"`
	// Reason is voluntary / owner_deactivated / offboarding; free text in Note.
	Reason string `json:"reason,omitempty"`
	// RotateToken revokes tracked tokens and issues a fresh one to the new
	// owner (transfer) or just revokes them (retire).
	RotateToken bool   `json:"rotate_token,omitempty"`
	Note        string `json:"note,omitempty"`
	CreatedAt   int64  `json:"created_at,omitempty"`
	Exercise    bool   `json:"exercise,omitempty"`
	// FromOrphanCard marks a ticket opened from an audit-channel orphan card
	// "on behalf of" a deactivated owner.
	FromOrphanCard bool `json:"from_orphan_card,omitempty"`

	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`

	twoStepState
}

// botTransferInput carries the caller-supplied fields for a new bot
// transfer/retire request.
type botTransferInput struct {
	BotUserID      string
	Action         string
	NewOwnerUserID string
	Reason         string
	Note           string
	RotateToken    bool
	// RotateSet marks RotateToken as explicitly chosen, so the reason-based
	// default only applies when the caller left it unset.
	RotateSet      bool
	FromOrphanCard bool
}

func transferActionLabel(action string) string {
	if action == transferActionRetire {
		return "Retire"
	}
	return "Transfer"
}

// submitBotTransferRequest validates the input and queues a bot
// transfer/retire for two-step approval.
func (p *Plugin) submitBotTransferRequest(requesterID string, in botTransferInput) (submitted string, submitErr error) {
	sa := p.startSubmitAudit(pendingKindBotTransfer, requesterID)
	defer sa.done(&submitErr)

	if !p.getConfiguration().RequestEnabled(requestTypeBotTransfer) {
		return "", errors.New(botTransferRequestsDisabledMsg)
	}
	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil || requester == nil {
		return "", errors.New("could not load your account; try again")
	}

	botUserID := strings.TrimSpace(in.BotUserID)
	index, err := p.loadBotIndex(botUserID)
	if err != nil {
		p.API.LogWarn("failed to load bot index for transfer", "bot_user_id", botUserID, "error", err.Error())
		return "", errors.New("could not look up that bot; try again")
	}
	if index == nil {
		return "", errors.New("that bot isn't managed by Request Hub, so it can't be transferred or retired here")
	}
	if index.RetiredAt != 0 {
		return "", errors.New("that bot has already been retired")
	}

	action := strings.TrimSpace(strings.ToLower(in.Action))
	switch action {
	case transferActionTransfer, transferActionRetire:
	default:
		return "", errors.New("pick Transfer or Retire")
	}

	asset := transferAsset{
		assetType: "bot", assetID: botUserID,
		ownerUserID: index.OwnerUserID, requesterID: index.RequesterID, channelID: index.HomeChannelID,
	}
	if err = p.canNominateTransfer(requesterID, asset); err != nil {
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
	note := strings.TrimSpace(in.Note)
	if err = checkTextLen("note", note, maxTransferNoteLen); err != nil {
		return "", err
	}

	newOwnerID := strings.TrimSpace(in.NewOwnerUserID)
	if action == transferActionTransfer {
		if err = p.checkTransferNewOwner(newOwnerID); err != nil {
			return "", err
		}
	} else {
		newOwnerID = ""
	}

	rotate := in.RotateToken
	if !in.RotateSet {
		rotate = rotateDefaultOn(reason)
	}
	if action == transferActionRetire {
		// Retire always revokes tracked tokens; model it as rotate-off so
		// no new token is issued.
		rotate = false
	}

	botUsername := index.Username
	if bot, botErr := p.API.GetBot(botUserID, true); botErr == nil && bot != nil && bot.Username != "" {
		botUsername = bot.Username
	}

	req := &botTransferRequest{
		ID:             model.NewId(),
		RequesterID:    requesterID,
		BotUserID:      botUserID,
		BotUsername:    botUsername,
		Action:         action,
		NewOwnerUserID: newOwnerID,
		Reason:         reason,
		RotateToken:    rotate,
		Note:           note,
		FromOrphanCard: in.FromOrphanCard,
		CreatedAt:      model.GetMillis(),
	}
	sa.built(req, requester)
	if err = p.reservePending(pendingKindBotTransfer, req.RequesterID, newPendingEntry(req), p.getConfiguration()); err != nil {
		return "", err
	}
	if err = p.storeBotTransferRequest(req); err != nil {
		p.releasePending(pendingKindBotTransfer, req.RequesterID, req.ID)
		p.API.LogError("failed to store bot transfer request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	sa.markStored()
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		p.deletePendingRequestKV(kvBotTransferRequestPrefix + req.ID)
		p.releasePending(pendingKindBotTransfer, req.RequesterID, req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot bot transfer request")
	}

	approvalPostID, approvalChannelID, err := p.postBotTransferApprovalRequest(req, requester)
	if err != nil {
		p.deletePendingRequestKV(kvBotTransferRequestPrefix + req.ID)
		p.releasePending(pendingKindBotTransfer, req.RequesterID, req.ID)
		p.API.LogError("failed to post bot transfer request", "error", err.Error())
		return "", errors.New("could not send your request to the approvers; try again or contact a System Admin")
	}
	if approvalPostID == "" {
		p.resolveCardsIfHandled(pendingKindBotTransfer, req.ID)
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	req.DMRootPostID, req.DMChannelID = p.postTicketCreatedDM(req.RequesterID,
		p.botTransferApprovalAttachment(req, requester), ticketSubmittedIntro("bot "+strings.ToLower(transferActionLabel(action))+" request"), req.ID)

	p.persistTicketPosts(ticketPersist{
		KVKey: kvBotTransferRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "bot transfer request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Kind: requestTypeBotTransfer,
		},
	})
	return fmt.Sprintf("Your request to %s bot @%s was submitted for approval. It needs a security approval and a system approval.",
		strings.ToLower(transferActionLabel(action)), req.BotUsername), nil
}

// checkTransferNewOwner requires an active, non-bot user for a transfer.
func (p *Plugin) checkTransferNewOwner(ownerID string) error {
	if ownerID == "" {
		return errors.New("pick the new owner for the bot")
	}
	if !model.IsValidId(ownerID) {
		return errors.New("the new owner must be picked from the list")
	}
	owner, appErr := p.API.GetUser(ownerID)
	if appErr != nil || owner == nil || owner.DeleteAt != 0 || owner.IsBot {
		return errors.New("the new owner must be an active user")
	}
	return nil
}

func (p *Plugin) postBotTransferApprovalRequest(req *botTransferRequest, requester *model.User) (postID, channelID string, err error) {
	ch := p.ensureBotApprovalChannel()
	go func() {
		defer p.logRecoveredPanic("two-step approver channel reconcile")
		p.reconcileTwoStepApproverChannels()
	}()
	return p.postApprovalAttachment(req.ID, p.botTransferApprovalAttachment(req, requester), p.botTransferApprovalHeader(req.Exercise), ch)
}

func (p *Plugin) botTransferApprovalHeader(exercise bool) string {
	return cardMention(exercise) + "a bot transfer/retire request needs review (requires a security approval and a system approval)."
}

func botTransferRetryHeader(exercise bool) string {
	return cardMention(exercise) + "a bot transfer/retire request needs review (a previous approval attempt failed)."
}

func (p *Plugin) botTransferApprovalAttachment(req *botTransferRequest, requester *model.User) *model.MessageAttachment {
	fields := []*model.MessageAttachmentField{
		{Title: "Requested by", Value: "@" + requester.Username, Short: true},
		{Title: "Action", Value: transferActionLabel(req.Action), Short: true},
		{Title: "Bot", Value: "@" + req.BotUsername, Short: true},
	}
	if req.Action == transferActionTransfer {
		fields = append(fields, &model.MessageAttachmentField{Title: "New owner", Value: p.mentionList([]string{req.NewOwnerUserID}), Short: true})
		rotate := "no"
		if req.RotateToken {
			rotate = "yes — a fresh token is issued to the new owner"
		}
		fields = append(fields, &model.MessageAttachmentField{Title: "Rotate token", Value: rotate, Short: true})
	}
	if req.FromOrphanCard {
		fields = append(fields, &model.MessageAttachmentField{Title: "Origin", Value: "orphaned-owner card (on behalf of the deactivated owner)", Short: false})
	}
	if req.Reason != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Reason", Value: req.Reason, Short: true})
	}
	if req.Note != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Note", Value: escapeMarkdown(req.Note), Short: false})
	}
	fields = append(fields, &model.MessageAttachmentField{Title: "Approvals", Value: p.twoStepStatusValue(req.twoStepState), Short: false})

	text := "Bot-owned webhooks move with the bot; there's no separate hook ownership change."
	if req.Action == transferActionRetire {
		text = "Retire disables the bot and revokes its tracked tokens. Bot-owned webhooks may need manual cleanup when their IDs aren't recorded."
	}
	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:   exerciseLabel(req.Exercise) + "Bot " + strings.ToLower(transferActionLabel(req.Action)) + " request",
		Color:   "#8B008B",
		Fields:  fields,
		Footer:  requestIDFooter(req.ID),
		Text:    text,
		Actions: p.botTransferApprovalActions(req.ID, siteURL),
	}
}

func (p *Plugin) botTransferApprovalAttachmentWithTitledNotice(req *botTransferRequest, requester *model.User, title, notice string) *model.MessageAttachment {
	att := p.botTransferApprovalAttachment(req, requester)
	att.Color = "#D24B4E"
	if title == "" {
		title = "⚠️ Action needed"
	}
	att.Fields = append([]*model.MessageAttachmentField{{Title: title, Value: notice, Short: false}}, att.Fields...)
	return att
}

func (p *Plugin) botTransferApprovalActions(requestID, siteURL string) []*model.PostAction {
	return []*model.PostAction{
		{
			Id: "approve", Name: approveButtonLabel, Type: model.PostActionTypeButton, Style: "primary",
			Integration: &model.PostActionIntegration{URL: siteURL + routeApproveBotTransfer, Context: map[string]any{actionContextRequestID: requestID}},
		},
		{
			Id: "deny", Name: "Deny", Type: model.PostActionTypeButton, Style: "danger",
			Integration: &model.PostActionIntegration{URL: siteURL + routeDenyBotTransfer, Context: map[string]any{actionContextRequestID: requestID}},
		},
	}
}

func (p *Plugin) storeBotTransferRequest(req *botTransferRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal bot transfer request")
	}
	if err := p.putPendingRequestKV(kvBotTransferRequestPrefix+req.ID, data); err != nil {
		return errors.Wrap(err, "failed to store bot transfer request")
	}
	return nil
}

func (p *Plugin) loadBotTransferRequest(id string) (*botTransferRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvBotTransferRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load bot transfer request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req botTransferRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal bot transfer request")
	}
	return &req, data, nil
}

// decideBotTransferRequest approves or denies a bot transfer/retire through
// the two-step engine. On final approval it carries out the transfer or
// retire with the approving admin's session.
func (p *Plugin) decideBotTransferRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil {
		return decisionResult{Reply: "Could not verify your identity."}
	}
	key := kvBotTransferRequestPrefix + requestID
	req, rawReq, err := p.loadBotTransferRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load bot transfer request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeBotTransfer, approve); msg != "" {
		return decisionResult{Reply: msg}
	}

	p.refreshTicketThread(ticketThread{
		RequestID: req.ID, ApprovalPostID: req.ApprovalPostID, ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID, Kind: requestTypeBotTransfer,
	})

	canSec, canSys := p.twoStepPools(actingUser, "bot transfer")
	requester, _ := p.API.GetUser(req.RequesterID)
	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID).
		withSubject(pendingKindBotTransfer, req.RequesterID, actingUser, req, req.Exercise)

	verb := strings.ToLower(transferActionLabel(req.Action))
	if days := p.getConfiguration().ExpiryDays(); (canSec || canSys) && requestExpired(req.CreatedAt, days, model.GetMillis()) {
		return p.expireRequest(cards, expiredRequest{
			kvKey: key, raw: rawReq, kind: pendingKindBotTransfer, requestID: req.ID, requesterID: req.RequesterID,
			dmRootID: req.DMRootPostID, dmChannelID: req.DMChannelID, days: days,
			cardTitle: "Bot " + transferActionLabel(req.Action) + " Request Expired",
			cardText:  fmt.Sprintf("Your request to %s bot **@%s** expired before it was reviewed. Submit it again if you still need it.", verb, req.BotUsername),
			audit: fmt.Sprintf("@%s closed the request to %s bot @%s from %s, which was not reviewed within %d days",
				actingUser.Username, verb, req.BotUsername, requesterUsername(requester, req.RequesterID), days),
		})
	}

	if !approve {
		if !canSec && !canSys {
			return decisionResult{Reply: "You don't have permission to deny this request."}
		}
		if res, ok := p.claimRecord(key, rawReq, cards, "bot transfer request"); !ok {
			return res
		}
		p.releasePending(pendingKindBotTransfer, req.RequesterID, req.ID)
		outcome := fmt.Sprintf("❌ Denied by @%s. Bot **@%s** %s.", actingUser.Username, req.BotUsername, verb)
		p.decisionEvent(cards, eventDenied, fmt.Sprintf("@%s denied the request to %s bot @%s", actingUser.Username, verb, req.BotUsername), nil)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: false, ApproverUsername: actingUser.Username,
			CardTitle:  "Bot " + transferActionLabel(req.Action) + " Request Denied",
			CardText:   fmt.Sprintf("Denied by @%s. The request to %s bot **@%s** was not approved.", actingUser.Username, verb, req.BotUsername),
			StatusLine: outcome,
		})
	}

	step, fill, reason := planApproval(req.twoStepState, canSec, canSys, actingUser.Id, req.RequesterID)
	if !fill {
		return twoStepBlocked(cards, actingUser.Username, reason, p.botTransferApprovalHeader(req.Exercise), func(title, notice string) *model.MessageAttachment {
			cardRequester := requester
			if cardRequester == nil {
				cardRequester = &model.User{Username: requesterUsername(nil, req.RequesterID)}
			}
			return p.botTransferApprovalAttachmentWithTitledNotice(req, cardRequester, title, notice)
		})
	}

	if msg := p.botTransferApprovalBlocker(req); msg != "" {
		return decisionResult{Reply: msg}
	}

	newState := p.applyTwoStepApproval(req.twoStepState, step, actingUser, model.GetMillis())
	if !newState.complete() {
		newReq := *req
		newReq.twoStepState = newState
		return p.recordFirstApproval(cards, key, rawReq, &newReq, step, ticketProgress{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Title: "Bot " + transferActionLabel(req.Action) + " Request Update",
			Text:  fmt.Sprintf("Your request to %s bot **@%s** received the %s approval; awaiting the other approval.", verb, req.BotUsername, step),
		}, p.botTransferApprovalHeader(req.Exercise), func() *model.MessageAttachment { return p.botTransferApprovalAttachment(&newReq, requester) })
	}

	if res, ok := p.claimRecord(key, rawReq, cards, "bot transfer request"); !ok {
		return res
	}
	p.releasePending(pendingKindBotTransfer, req.RequesterID, req.ID)
	return p.fulfillBotTransferRequest(req, actingUser, requester, cards, rawReq, key)
}

// botTransferApprovalBlocker re-checks before final approval that a transfer
// still has a valid new owner and that the bot still exists.
func (p *Plugin) botTransferApprovalBlocker(req *botTransferRequest) string {
	if _, appErr := p.API.GetBot(req.BotUserID, true); appErr != nil {
		return fmt.Sprintf("Bot @%s no longer exists. Deny this request instead.", req.BotUsername)
	}
	if req.Action == transferActionTransfer {
		if err := p.checkTransferNewOwner(req.NewOwnerUserID); err != nil {
			return err.Error() + " Deny this request, or ask for a new one with a valid owner."
		}
	}
	return ""
}

// fulfillBotTransferRequest carries out a fully approved bot transfer or
// retire with the approving admin's session.
func (p *Plugin) fulfillBotTransferRequest(req *botTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string) decisionResult {
	var partialErrors []string

	if req.Action == transferActionRetire {
		revoked, revErr := p.revokeBotTokens(req.BotUserID)
		if revErr != nil {
			partialErrors = append(partialErrors, "revoking tracked tokens failed: "+revErr.Error())
		}
		if disErr := p.disableBot(req.BotUserID, actingUser.Id); disErr != nil {
			// Nothing was changed yet that we can't leave pending for a retry.
			return p.restoreBotTransferAfterFailure(req, requester, cards, rawReq, key, disErr)
		}
		if index, _ := p.loadBotIndex(req.BotUserID); index != nil && (index.HasIncomingWebhook || index.HasOutgoingWebhook) {
			partialErrors = append(partialErrors, "any bot-owned webhooks may need manual cleanup (their hook IDs aren't recorded in the bot index)")
		}
		p.retireBotIndex(req.BotUserID)
		p.clearOrphanNotice("bot", req.BotUserID)
		outcome := fmt.Sprintf("✅ Fully approved (security + system). Bot **@%s** retired.", req.BotUsername)
		p.auditTransferApproved(cards, req, actingUser, requester, revoked, 0, partialErrors)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Bot Retire Approved",
			CardText:   fmt.Sprintf("Fully approved. Bot **@%s** has been disabled and its tracked tokens revoked.", req.BotUsername),
			StatusLine: outcome,
		})
	}

	// Transfer.
	if assignErr := p.assignBot(req.BotUserID, req.NewOwnerUserID, actingUser.Id); assignErr != nil {
		return p.restoreBotTransferAfterFailure(req, requester, cards, rawReq, key, assignErr)
	}
	p.updateBotIndexOwner(req.BotUserID, req.NewOwnerUserID)
	p.clearOrphanNotice("bot", req.BotUserID)

	var tokenValue string
	var tokenExpiresAt int64
	revoked := 0
	if req.RotateToken {
		var rotErr error
		revoked, tokenValue, tokenExpiresAt, rotErr = p.rotateBotToken(req.BotUserID, req.BotUsername, req.NewOwnerUserID, req.ID)
		if rotErr != nil {
			partialErrors = append(partialErrors, "token rotation failed: "+rotErr.Error())
		}
	}

	outcome := fmt.Sprintf("✅ Fully approved (security + system). Bot **@%s** transferred to %s.",
		req.BotUsername, p.mentionList([]string{req.NewOwnerUserID}))
	p.auditTransferApproved(cards, req, actingUser, requester, revoked, tokenExpiresAt, partialErrors)
	result := p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: true, ApproverUsername: actingUser.Username,
		CardTitle:  "Bot Transfer Approved",
		CardText:   fmt.Sprintf("Fully approved. Bot **@%s** now belongs to %s.", req.BotUsername, p.mentionList([]string{req.NewOwnerUserID})),
		StatusLine: outcome,
	})

	if tokenValue != "" {
		lines := []string{
			fmt.Sprintf("🔑 **New access token for bot @%s**", req.BotUsername),
			fmt.Sprintf("`%s`", tokenValue),
			"⚠️ **Save this now.** This message auto-deletes in ~72 hours and the token cannot be retrieved again.",
		}
		if tokenExpiresAt > 0 {
			lines = append(lines, fmt.Sprintf("⏳ The token expires on **%s**.", tokenExpiryDate(tokenExpiresAt)))
		}
		if !p.sendTransferSecretsDM(req.NewOwnerUserID, req.ID, lines, true) {
			p.API.LogError("failed to deliver rotated bot token to new owner", "bot_user_id", req.BotUserID, "new_owner_id", req.NewOwnerUserID)
		}
	}
	return result
}

// restoreBotTransferAfterFailure re-stores the pending request after a REST
// side effect failed, so approvers can retry or deny.
func (p *Plugin) restoreBotTransferAfterFailure(req *botTransferRequest, requester *model.User, cards decisionCards, rawReq []byte, key string, cause error) decisionResult {
	verb := strings.ToLower(transferActionLabel(req.Action))
	p.API.LogError("failed to carry out bot transfer/retire on approval", "action", req.Action, "error", cause.Error())
	if restoreErr := p.putPendingRequestKV(key, rawReq); restoreErr != nil {
		p.API.LogError("failed to restore bot transfer request after failure", "error", restoreErr.Error())
		return p.lostRequest(cards, pendingKindBotTransfer, req.RequesterID, "",
			fmt.Sprintf("Could not %s the bot (%s), and the request could not be saved — ask the requester to resubmit.", verb, cause.Error()))
	}
	notice := func(clicked bool) string {
		return fmt.Sprintf("Couldn't %s the bot: %s. The request is still pending — %s.", verb, cause.Error(), retryText(clicked))
	}
	p.approveFailed(cards, fmt.Sprintf("final approval to %s bot @%s could not be carried out", verb, req.BotUsername), cause)
	result := p.repaintCards(cards, key, notice(cards.clickedPostID != ""),
		botTransferRetryHeader(req.Exercise),
		p.botTransferApprovalAttachmentWithTitledNotice(req, requesterOrPlaceholder(requester, req.RequesterID), "⚠️ Action needed", notice(true)))
	result.audited = true
	return result
}

func (p *Plugin) auditTransferApproved(cards decisionCards, req *botTransferRequest, actingUser, requester *model.User, tokensRevoked int, tokenExpiresAt int64, partialErrors []string) {
	verb := strings.ToLower(transferActionLabel(req.Action))
	event, problems := eventApproved, ""
	if len(partialErrors) > 0 {
		event, problems = eventApprovedPartial, "; "+escapeMarkdown(strings.Join(partialErrors, "; "))
	}
	detail := map[string]any{
		"bot_user_id": req.BotUserID, "action": req.Action, "tokens_revoked": tokensRevoked,
		"rotated": req.RotateToken && tokenExpiresAt > 0, "from_orphan_card": req.FromOrphanCard,
	}
	if req.Action == transferActionTransfer {
		detail["new_owner_id"] = req.NewOwnerUserID
	}
	p.decisionEvent(cards, event, fmt.Sprintf("@%s gave final approval; bot @%s %sd for %s%s",
		actingUser.Username, req.BotUsername, verb, requesterUsername(requester, req.RequesterID), problems), detail)
}

func requesterOrPlaceholder(requester *model.User, requesterID string) *model.User {
	if requester != nil {
		return requester
	}
	return &model.User{Username: requesterUsername(nil, requesterID)}
}

// handleBotTransferAction handles Approve/Deny on a bot transfer card.
func (p *Plugin) handleBotTransferAction(w http.ResponseWriter, r *http.Request, approve bool) {
	p.handleTwoStepClick(w, r, approve, p.deciderFor(pendingKindBotTransfer))
}
