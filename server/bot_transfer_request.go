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
	RotateSet bool
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

	indexOwner := botIndexOwner(*index)
	ownerID, liveErr := p.liveBotOwnerID(botUserID, indexOwner)
	if errors.Is(liveErr, errBotGone) {
		return "", errors.New("that bot no longer exists")
	}
	if liveErr != nil {
		p.API.LogWarn("failed to load live bot owner for transfer", "bot_user_id", botUserID, "error", liveErr.Error())
		return "", errors.New("could not verify the bot's current owner; try again")
	}
	asset := transferAsset{
		assetType: "bot", assetID: botUserID,
		ownerUserID: ownerID, requesterID: index.RequesterID, channelID: index.HomeChannelID,
	}
	if err = p.canNominateTransfer(requesterID, asset); err != nil {
		return "", err
	}

	ownerActive, ownerKnown := p.userActiveStatus(ownerID)
	if !ownerKnown {
		return "", errors.New("could not verify the current owner's account; try again")
	}
	reason, err := normalizeTransferReason(in.Reason, ownerActive)
	if err != nil {
		return "", err
	}
	note := strings.TrimSpace(in.Note)
	if err = checkTextLen("note", note, maxTransferNoteLen); err != nil {
		return "", err
	}

	newOwnerID := strings.TrimSpace(in.NewOwnerUserID)
	if action == transferActionTransfer {
		if err = p.checkTransferNewOwner(newOwnerID, transferNewOwnerOpts{
			currentOwnerID: ownerID, noun: "bot",
		}); err != nil {
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
		CreatedAt:      model.GetMillis(),
	}
	sa.built(req, requester)
	if held, holdErr := p.transferAssetPendingElsewhere(pendingKindBotTransfer, req.pendingKey(), ""); holdErr != nil {
		p.API.LogWarn("failed to check for existing bot transfer ticket", "bot_user_id", botUserID, "error", holdErr.Error())
		return "", errors.New("could not check for an existing transfer ticket; try again")
	} else if held {
		return "", duplicateRequestError(pendingKindBotTransfer, req.pendingKey())
	}
	// Store before claim so a concurrent submitter sees this request as live
	// and cannot treat an in-flight claim as stale.
	if err = p.reservePending(pendingKindBotTransfer, req.RequesterID, newPendingEntry(req), p.getConfiguration()); err != nil {
		return "", err
	}
	if err = p.storeBotTransferRequest(req); err != nil {
		p.releasePending(pendingKindBotTransfer, req.RequesterID, req.ID)
		p.API.LogError("failed to store bot transfer request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	sa.markStored()
	if claimErr := p.claimTransferAsset(pendingKindBotTransfer, req.pendingKey(), req.ID); claimErr != nil {
		p.deletePendingRequestKV(kvBotTransferRequestPrefix + req.ID)
		p.releasePending(pendingKindBotTransfer, req.RequesterID, req.ID)
		return "", claimErr
	}
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		p.deletePendingRequestKV(kvBotTransferRequestPrefix + req.ID)
		p.releaseTransferTicket(pendingKindBotTransfer, req.RequesterID, req.ID, req.pendingKey())
		return "", errors.Wrap(snapErr, "failed to snapshot bot transfer request")
	}

	approvalPostID, approvalChannelID, err := p.postBotTransferApprovalRequest(req, requester)
	if err != nil {
		p.deletePendingRequestKV(kvBotTransferRequestPrefix + req.ID)
		p.releaseTransferTicket(pendingKindBotTransfer, req.RequesterID, req.ID, req.pendingKey())
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
	requester = requesterOrPlaceholder(requester, req.RequesterID)
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
	if req.Reason != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Reason", Value: escapeMarkdown(req.Reason), Short: true})
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
			transferAssetKey: req.pendingKey(),
			cardTitle:        "Bot " + transferActionLabel(req.Action) + " Request Expired",
			cardText:         fmt.Sprintf("Your request to %s bot **@%s** expired before it was reviewed. Submit it again if you still need it.", verb, req.BotUsername),
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
		p.releaseTransferTicket(pendingKindBotTransfer, req.RequesterID, req.ID, req.pendingKey())
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

	if msg := p.botTransferApprovalBlockerFor(req, actingUser.Id); msg != "" {
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
		}, p.botTransferApprovalHeader(req.Exercise), func() *model.MessageAttachment {
			return p.botTransferApprovalAttachment(&newReq, requesterOrPlaceholder(requester, req.RequesterID))
		})
	}

	if res, ok := p.claimTransferForFulfill(req.ID, key, rawReq, cards, "bot transfer request"); !ok {
		return res
	}
	// Keep the pending reservation until fulfill succeeds so MaxPending and
	// restore-after-failure stay consistent (release happens in fulfill*).
	return p.fulfillBotTransferRequest(req, actingUser, requester, cards, rawReq, key)
}

// botTransferApprovalBlocker re-checks before final approval that a transfer
// still has a valid new owner, that the bot still exists, and that the
// nominator ACL still holds against the live index owner.
func (p *Plugin) botTransferApprovalBlocker(req *botTransferRequest) string {
	if _, appErr := p.API.GetBot(req.BotUserID, true); appErr != nil {
		return fmt.Sprintf("Bot @%s no longer exists. Deny this request instead.", req.BotUsername)
	}
	index, indexErr := p.loadBotIndex(req.BotUserID)
	if indexErr != nil {
		return "Could not re-check bot ownership; try Approve again."
	}
	if index == nil || index.RetiredAt != 0 {
		return fmt.Sprintf("Bot @%s is no longer managed (or was retired). Deny this request instead.", req.BotUsername)
	}
	indexOwner := botIndexOwner(*index)
	ownerID, liveErr := p.liveBotOwnerID(req.BotUserID, indexOwner)
	if liveErr != nil {
		return "Could not re-check the bot's current owner; try Approve again."
	}
	asset := transferAsset{
		assetType: "bot", assetID: req.BotUserID,
		ownerUserID: ownerID, requesterID: index.RequesterID, channelID: index.HomeChannelID,
	}
	if err := p.canNominateTransfer(req.RequesterID, asset); err != nil {
		return "The requester can no longer transfer or retire this bot (" + err.Error() + "). Deny this request instead."
	}
	if req.Action == transferActionTransfer {
		if err := p.checkTransferNewOwner(req.NewOwnerUserID, transferNewOwnerOpts{
			currentOwnerID: ownerID, actingUserID: "", noun: "bot",
		}); err != nil {
			return err.Error() + " Deny this request, or ask for a new one with a valid owner."
		}
	}
	return ""
}

// botTransferApprovalBlockerFor adds approve-time checks that need the actor
// (e.g. cannot approve a transfer that names yourself as the new owner).
func (p *Plugin) botTransferApprovalBlockerFor(req *botTransferRequest, actingUserID string) string {
	if msg := p.botTransferApprovalBlocker(req); msg != "" {
		return msg
	}
	if req.Action == transferActionTransfer {
		if err := p.checkTransferNewOwner(req.NewOwnerUserID, transferNewOwnerOpts{
			currentOwnerID: "", actingUserID: actingUserID, noun: "bot",
		}); err != nil {
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
		// Disable first so the bot stops acting before token revoke races.
		if disErr := p.disableBot(req.BotUserID, actingUser.Id); disErr != nil {
			return p.restoreBotTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, disErr)
		}
		revoked, revErr := p.revokeBotTokens(req.BotUserID, actingUser.Id)
		if revErr != nil {
			partialErrors = append(partialErrors, "revoking bot tokens failed: "+revErr.Error())
		}
		if index, loadErr := p.loadBotIndex(req.BotUserID); loadErr != nil {
			partialErrors = append(partialErrors, "bot index could not be loaded after retire: "+loadErr.Error()+" — bot-owned webhooks may need manual cleanup")
		} else if index != nil && (index.HasIncomingWebhook || index.HasOutgoingWebhook) {
			partialErrors = append(partialErrors, "any bot-owned webhooks may need manual cleanup (their hook IDs aren't recorded in the bot index)")
		}
		if idxErr := p.retireBotIndex(req.BotUserID); idxErr != nil {
			partialErrors = append(partialErrors, "bot index could not be marked retired: "+idxErr.Error())
		}
		p.releaseTransferTicket(pendingKindBotTransfer, req.RequesterID, req.ID, req.pendingKey())
		outcome := fmt.Sprintf("✅ Fully approved (security + system). Bot **@%s** retired.", req.BotUsername)
		cardText := fmt.Sprintf("Fully approved. Bot **@%s** has been disabled and its tokens revoked.", req.BotUsername)
		if len(partialErrors) > 0 {
			outcome = fmt.Sprintf("⚠️ Fully approved with issues. Bot **@%s** retired.", req.BotUsername)
			cardText = fmt.Sprintf("Fully approved with issues. Bot **@%s** was disabled, but: %s.", req.BotUsername, escapeMarkdown(strings.Join(partialErrors, "; ")))
		}
		p.auditTransferApproved(cards, req, actingUser, requester, revoked, 0, false, partialErrors)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Bot Retire Approved",
			CardText:   cardText,
			StatusLine: outcome,
		})
	}

	// Transfer.
	if assignErr := p.assignBot(req.BotUserID, req.NewOwnerUserID, actingUser.Id); assignErr != nil {
		return p.restoreBotTransferAfterFailure(req, actingUser, requester, cards, rawReq, key, assignErr)
	}
	if idxErr := p.updateBotIndexOwner(req.BotUserID, req.NewOwnerUserID); idxErr != nil {
		partialErrors = append(partialErrors, "bot index owner could not be updated: "+idxErr.Error()+" — transfer rights may still show the previous owner")
	}

	var tokenID, tokenValue string
	var tokenExpiresAt int64
	revoked := 0
	rotated := false
	if req.RotateToken {
		var rotErr error
		revoked, tokenID, tokenValue, tokenExpiresAt, rotErr = p.rotateBotToken(req.BotUserID, req.BotUsername, req.NewOwnerUserID, actingUser.Id, req.ID)
		if rotErr != nil {
			// Owner already moved; keep the ticket closed but surface the gap
			// so an admin can rotate manually. Restoring would re-open a
			// transfer against an already-reassigned bot.
			if tokenValue != "" {
				partialErrors = append(partialErrors, "a new token was issued, but not every old token could be revoked: "+rotErr.Error()+" — revoke the remaining tokens manually")
			} else {
				partialErrors = append(partialErrors, "token rotation failed: "+rotErr.Error()+" — rotate manually for the new owner")
			}
		}
		rotated = tokenValue != ""
	}

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
			// Do not leave an undelivered secret live under the new owner.
			if revErr := p.revokeNewlyIssuedBotToken(tokenID); revErr != nil {
				partialErrors = append(partialErrors, "the new bot token could not be DM'd and could not be revoked automatically: "+revErr.Error())
			} else {
				partialErrors = append(partialErrors, "the new bot token could not be DM'd to the new owner, so it was revoked — rotate again")
				tokenExpiresAt = 0
				rotated = false
			}
		}
	}

	p.releaseTransferTicket(pendingKindBotTransfer, req.RequesterID, req.ID, req.pendingKey())
	outcome := fmt.Sprintf("✅ Fully approved (security + system). Bot **@%s** transferred to %s.",
		req.BotUsername, p.mentionList([]string{req.NewOwnerUserID}))
	cardText := fmt.Sprintf("Fully approved. Bot **@%s** now belongs to %s.", req.BotUsername, p.mentionList([]string{req.NewOwnerUserID}))
	if len(partialErrors) > 0 {
		outcome = fmt.Sprintf("⚠️ Fully approved with issues. Bot **@%s** transferred to %s.",
			req.BotUsername, p.mentionList([]string{req.NewOwnerUserID}))
		cardText = fmt.Sprintf("Fully approved with issues. Bot **@%s** transferred to %s, but: %s.",
			req.BotUsername, p.mentionList([]string{req.NewOwnerUserID}), escapeMarkdown(strings.Join(partialErrors, "; ")))
	}
	p.auditTransferApproved(cards, req, actingUser, requester, revoked, tokenExpiresAt, rotated, partialErrors)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: true, ApproverUsername: actingUser.Username,
		CardTitle:  "Bot Transfer Approved",
		CardText:   cardText,
		StatusLine: outcome,
	})
}

// restoreBotTransferAfterFailure re-stores the pending request after a REST
// side effect failed, so approvers can retry or deny.
func (p *Plugin) restoreBotTransferAfterFailure(req *botTransferRequest, actingUser, requester *model.User, cards decisionCards, rawReq []byte, key string, cause error) decisionResult {
	verb := strings.ToLower(transferActionLabel(req.Action))
	approverName := ""
	if actingUser != nil {
		approverName = actingUser.Username
	}
	p.API.LogError("failed to carry out bot transfer/retire on approval", "action", req.Action, "error", cause.Error())
	restored, restoreErr := p.restoreTransferRecord(pendingKindBotTransfer, req.pendingKey(), req.ID, key, rawReq)
	if restoreErr != nil && !restored {
		p.API.LogError("failed to restore bot transfer request after failure", "error", restoreErr.Error())
		return p.lostRequest(cards, pendingKindBotTransfer, req.RequesterID, approverName,
			fmt.Sprintf("Could not %s the bot (%s), and the request could not be saved — ask the requester to resubmit.", verb, cause.Error()))
	}
	if !restored {
		return p.lostRequest(cards, pendingKindBotTransfer, req.RequesterID, approverName,
			fmt.Sprintf("Could not %s the bot (%s), and another transfer ticket took over this bot — ask the requester to resubmit if needed.", verb, cause.Error()))
	}
	causeMsg := cause.Error()
	if restoreErr != nil {
		p.API.LogWarn("bot transfer restored but asset claim could not be re-checked", "error", restoreErr.Error())
		causeMsg = cause.Error() + "; asset claim could not be re-checked — retry Approve"
	}
	notice := func(clicked bool) string {
		return fmt.Sprintf("Couldn't %s the bot: %s. The request is still pending — %s.", verb, causeMsg, retryText(clicked))
	}
	p.approveFailed(cards, fmt.Sprintf("final approval to %s bot @%s could not be carried out", verb, req.BotUsername), cause)
	result := p.repaintCards(cards, key, notice(cards.clickedPostID != ""),
		botTransferRetryHeader(req.Exercise),
		p.botTransferApprovalAttachmentWithTitledNotice(req, requesterOrPlaceholder(requester, req.RequesterID), "⚠️ Action needed", notice(true)))
	result.audited = true
	return result
}

func (p *Plugin) auditTransferApproved(cards decisionCards, req *botTransferRequest, actingUser, requester *model.User, tokensRevoked int, tokenExpiresAt int64, rotated bool, partialErrors []string) {
	verb := strings.ToLower(transferActionLabel(req.Action))
	event, problems := eventApproved, ""
	if len(partialErrors) > 0 {
		event, problems = eventApprovedPartial, "; "+escapeMarkdown(strings.Join(partialErrors, "; "))
	}
	detail := map[string]any{
		"bot_user_id": req.BotUserID, "action": req.Action, "tokens_revoked": tokensRevoked,
		"rotated": rotated, "token_expires_at": tokenExpiresAt,
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
