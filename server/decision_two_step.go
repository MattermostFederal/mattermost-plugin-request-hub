package main

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/mattermost/mattermost/server/public/model"
)

// twoStepPools reports which approval steps actingUser may fill. A failed
// check counts as not eligible. what names the request type in the log.
func (p *Plugin) twoStepPools(actingUser *model.User, what string) (canSec, canSys bool) {
	canSec, errSec := p.canApproveStep(actingUser, stepSecurity)
	canSys, errSys := p.canApproveStep(actingUser, stepSystem)
	if errSec != nil {
		p.API.LogWarn(what+" security approver check failed; treating as not eligible", "user_id", actingUser.Id, "error", errSec.Error())
		canSec = false
	}
	if errSys != nil {
		p.API.LogWarn(what+" system approver check failed; treating as not eligible", "user_id", actingUser.Id, "error", errSys.Error())
		canSys = false
	}
	return canSec, canSys
}

// twoStepBlocked answers an Approve that planApproval refused with reason.
// A click is answered under the card too, and for some reasons the card
// shows a banner built by banner (see twoStepApproveBlock). The slash
// command has no card, so it just gets the reason.
func twoStepBlocked(c decisionCards, actingUsername, reason, header string, banner func(title, notice string) *model.MessageAttachment) decisionResult {
	if c.clickedPostID == "" {
		return decisionResult{Reply: "⚠️ " + reason}
	}
	title, notice, ephemeral, paint := twoStepApproveBlock(actingUsername, reason)
	result := decisionResult{Reply: ephemeral, Blocked: true, BannerHeader: header}
	if paint {
		result.Banner = banner(title, notice)
	}
	return result
}

// retryText is how the private reply to a failed final approval tells the
// approver to try again.
func retryText(clicked bool) string {
	if clicked {
		return "click **Approve** again to retry, or **Deny** it"
	}
	return "run the approve command again to retry, or deny it"
}

// repaintCards redraws every copy of a two-step request's card with header
// and att, after a first approval or a failed final one. A click gets the
// clicked card back as its Update; with no click the stored card is
// updated in place. key is the request's record: once it is gone, another
// decision has closed the cards, and drawing them again would offer a
// request that no longer exists.
func (p *Plugin) repaintCards(c decisionCards, key, reply, header string, att *model.MessageAttachment) decisionResult {
	result := decisionResult{Reply: reply}
	if still, appErr := p.API.KVGet(key); appErr == nil && still == nil {
		return result
	}
	for _, postID := range p.otherCardIDs(c) {
		post, appErr := p.API.GetPost(postID)
		if appErr != nil || post == nil || post.UserId != p.botUserID {
			continue
		}
		if card := extractFirstAttachment(post); card == nil || len(card.Actions) == 0 {
			continue // already closed
		}
		post.Message = header
		post.DelProp("attachments")
		model.ParseMessageAttachment(post, []*model.MessageAttachment{att})
		if _, appErr := p.API.UpdatePost(post); appErr != nil {
			p.API.LogWarn("failed to update approval card", "request_id", c.requestID, "post_id", postID, "error", appErr.Error())
		}
	}
	if c.clickedPostID != "" {
		// Returned as the click's Update, never written with UpdatePost:
		// see respondTwoStepApproveBlocked. An Update without a post ID
		// would break the card's buttons.
		if update := p.repaintedApprovalPost(c.clickedPostID, header, att); update.Id != "" {
			result.Update = update
		}
	}
	return result
}

// otherCardIDs lists the card copies a decision updates itself: every
// recorded copy and the stored card, except the clicked one, which the
// click's response updates.
func (p *Plugin) otherCardIDs(c decisionCards) []string {
	postIDs, err := p.loadApprovalCards(c.requestID)
	if err != nil {
		p.API.LogWarn("failed to load approval cards", "request_id", c.requestID, "error", err.Error())
	}
	if c.cardPostID != "" && !slices.Contains(postIDs, c.cardPostID) {
		postIDs = append(postIDs, c.cardPostID)
	}
	ids := make([]string, 0, len(postIDs))
	for _, id := range postIDs {
		if id != "" && id != c.clickedPostID {
			ids = append(ids, id)
		}
	}
	return ids
}

// recordFirstApproval stores a request whose first step was just approved,
// comparing against the bytes that were read, tells the requester, and
// redraws the cards with header and the attachment card builds, which
// shows the approval.
func (p *Plugin) recordFirstApproval(c decisionCards, key string, raw []byte, updated any, step approverStep, progress ticketProgress, header string, card func() *model.MessageAttachment) decisionResult {
	newBytes, err := json.Marshal(updated)
	if err != nil {
		p.API.LogError("failed to encode approved request step", "request_id", c.requestID, "error", err.Error())
		return decisionResult{Reply: "Could not process that request."}
	}
	set, setErr := p.API.KVCompareAndSet(key, raw, newBytes)
	if setErr != nil {
		p.API.LogError("failed to store approved request step", "request_id", c.requestID, "error", setErr.Error())
	}
	if setErr != nil || !set {
		return decisionResult{Reply: "This request was just updated by someone else. Please try again."}
	}
	p.notifyTicketProgress(progress)
	reply := ""
	if c.clickedPostID == "" {
		reply = fmt.Sprintf("Approved the %s step. The request now needs the other approval, from a different person.", step)
	}
	return p.repaintCards(c, key, reply, header, card())
}

// endUndelivered closes the cards of a fully approved request whose secret
// couldn't be delivered, so nothing was handed over.
func (p *Plugin) endUndelivered(c decisionCards, outcome string) decisionResult {
	p.resolveApprovalCards(c.requestID, outcome, c.cardPostID)
	p.deleteApprovalCards(c.requestID)
	return p.applyResolved(c, "", outcome, p.resolvedPost(c.cardPostID, outcome))
}

// decideBotRequest approves or denies a bot account request on behalf of
// actingUserID through the two-step engine. Approve fills the acting
// user's step; once two different people have filled both, the bot (and
// any token or webhooks asked for) is created. See decideChannelRequest
// for clickedPostID.
func (p *Plugin) decideBotRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil {
		return decisionResult{Reply: "Could not verify your identity."}
	}

	key := kvBotRequestPrefix + requestID
	req, rawReq, err := p.loadBotRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load bot request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeBot, approve); msg != "" {
		return decisionResult{Reply: msg}
	}

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeBot,
	})

	canSec, canSys := p.twoStepPools(actingUser, "bot")

	// Best-effort for cards and audit; approve checks requesterErr.
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID)

	// Expiry counts from submission, even when one step is already approved.
	if days := p.getConfiguration().ExpiryDays(); (canSec || canSys) && requestExpired(req.CreatedAt, days, model.GetMillis()) {
		return p.expireRequest(cards, expiredRequest{
			kvKey:       key,
			raw:         rawReq,
			kind:        pendingKindBot,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   exerciseLabel(req.Exercise) + "Bot Account Request Expired",
			cardText:    fmt.Sprintf("Your request for bot **@%s** expired before it was reviewed. Submit it again if you still need it.", req.Username),
			audit: exerciseLabel(req.Exercise) + fmt.Sprintf("@%s closed the bot request for @%s from %s, which was not reviewed within %d days",
				actingUser.Username, req.Username, requesterUsername(requester, req.RequesterID), days),
		})
	}

	if !approve {
		if !canSec && !canSys {
			return decisionResult{Reply: "You don't have permission to deny this request."}
		}
		if res, ok := p.claimRecord(key, rawReq, cards, "bot request"); !ok {
			return res
		}
		p.releasePending(pendingKindBot, req.RequesterID, req.ID)
		outcome := fmt.Sprintf("❌ Denied by @%s for %s. Bot **@%s**.", actingUser.Username, requesterUsername(requester, req.RequesterID), req.Username)
		p.logAudit(auditDenied, exerciseLabel(req.Exercise)+fmt.Sprintf("@%s denied a bot request for @%s",
			actingUser.Username, req.Username), cards.cardPostID)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: false, ApproverUsername: actingUser.Username,
			CardTitle:  exerciseLabel(req.Exercise) + "Bot Account Request Denied",
			CardText:   fmt.Sprintf("Denied by @%s. Your request for bot **@%s** was not approved. Reply to this thread if you'd like more information.", actingUser.Username, req.Username),
			StatusLine: outcome,
		})
	}

	step, fill, reason := planApproval(req.twoStepState, canSec, canSys, actingUser.Id, req.RequesterID)
	if !fill {
		return twoStepBlocked(cards, actingUser.Username, reason, p.botApprovalHeader(req.Exercise), func(title, notice string) *model.MessageAttachment {
			cardRequester := requester
			if cardRequester == nil {
				cardRequester = &model.User{Username: requesterUsername(nil, req.RequesterID)}
			}
			return p.botApprovalAttachmentWithTitledNotice(req, cardRequester, title, notice)
		})
	}

	// Re-check what submit checked, before either step, so neither step is
	// filled on a request that can no longer be carried out. A failed check
	// leaves the request and its steps as they are, so it can be denied.
	if msg := p.botApprovalBlocker(req, requester, requesterErr); msg != "" {
		return decisionResult{Reply: msg}
	}

	newState := p.applyTwoStepApproval(req.twoStepState, step, actingUser, model.GetMillis())
	if !newState.complete() {
		newReq := *req
		newReq.twoStepState = newState
		return p.recordFirstApproval(cards, key, rawReq, &newReq, step, ticketProgress{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Title: "Bot Account Request Update",
			Text:  fmt.Sprintf("Your bot request for **@%s** received the %s approval; awaiting the other approval.", req.Username, step),
		}, p.botApprovalHeader(req.Exercise), func() *model.MessageAttachment { return p.botApprovalAttachment(&newReq, requester) })
	}

	if res, ok := p.claimRecord(key, rawReq, cards, "bot request"); !ok {
		return res
	}

	bot, createErr := p.createBotForRequest(req)
	if createErr != nil {
		p.API.LogError("failed to create bot on approval", "error", createErr.Error())
		if restoreErr := p.API.KVSet(key, rawReq); restoreErr != nil {
			p.API.LogError("failed to restore bot request after create failure", "error", restoreErr.Error())
			return p.lostRequest(cards, pendingKindBot, req.RequesterID, actingUser.Username,
				fmt.Sprintf("Could not create the bot (%s), and the pending request could not be saved — ask the requester to resubmit.", createErr.Error()))
		}
		notice := func(clicked bool) string {
			return fmt.Sprintf("Couldn't create the bot: %s. The request is still pending — %s.", createErr.Error(), retryText(clicked))
		}
		// The cards are for clicking, whoever ran into the failure.
		return p.repaintCards(cards, key, notice(clickedPostID != ""),
			botRetryHeader(req.Exercise),
			p.botApprovalAttachmentWithNotice(req, requester, notice(true)))
	}
	p.releasePending(pendingKindBot, req.RequesterID, req.ID)
	return p.fulfillBotRequest(req, bot, actingUser, requester, cards)
}

// fulfillBotRequest finishes a fully approved bot request once the bot
// exists: it creates the token and webhooks asked for, as the approving
// admin, closes the ticket and cards, and sends the requester the extras.
func (p *Plugin) fulfillBotRequest(req *botRequest, bot *model.Bot, actingUser, requester *model.User, cards decisionCards) decisionResult {
	// An exercise bot is recorded before anything is made for it, and
	// each webhook as soon as it exists, so teardown misses none of them.
	if req.Exercise && !p.recordExerciseBot(req.ExerciseTeamID, bot.UserId) {
		return p.closeRemovedExerciseRequest(cards, req.RequesterID, req.DMRootPostID, req.DMChannelID)
	}
	recordHook := func(hook exerciseWebhook) bool {
		return !req.Exercise || p.recordExerciseWebhook(req.ExerciseTeamID, hook, actingUser.Id)
	}

	authToken := ""
	siteURL := p.siteURL()
	if adminSession, sessErr := p.createUserSession(actingUser.Id); sessErr != nil {
		p.API.LogWarn("failed to create admin session — token/webhook extras will be skipped", "error", sessErr.Error())
	} else {
		authToken = adminSession.Token
		defer func() {
			if revErr := p.API.RevokeSession(adminSession.Id); revErr != nil {
				p.API.LogWarn("failed to revoke admin session after bot extras", "error", revErr.Error())
			}
		}()
	}

	var tokenValue string
	var tokenExpiresAt int64
	var incomingWebhookURL string
	var outgoingWebhookCreated bool
	var partialErrors []string

	wantsToken := req.RequestToken
	wantsIncoming := req.IncomingWebhookChannelID != ""
	wantsOutgoing := req.OutgoingWebhookChannelID != ""
	if (wantsToken || wantsIncoming || wantsOutgoing) && authToken == "" {
		partialErrors = append(partialErrors, "token/webhook extras were skipped because an admin session could not be created")
	}
	if (wantsIncoming || wantsOutgoing) && siteURL == "" {
		partialErrors = append(partialErrors, "webhook extras were skipped because SiteURL is not configured")
	}

	if wantsToken && authToken != "" {
		tok, expiresAt, tokErr := p.createBotToken(authToken, bot.UserId, "API token for @"+req.Username)
		if tokErr != nil {
			partialErrors = append(partialErrors, fmt.Sprintf("token generation failed: %s", tokErr.Error()))
			p.API.LogError("failed to create bot token", "bot_user_id", bot.UserId, "error", tokErr.Error())
		} else {
			tokenValue = tok
			tokenExpiresAt = expiresAt
		}
	}

	if wantsIncoming && authToken != "" && siteURL != "" {
		// The hook's owner is the bot, which must be able to read the channel.
		if joinErr := p.addBotToWebhookChannel(bot.UserId, req.IncomingWebhookChannelID); joinErr != nil {
			partialErrors = append(partialErrors, fmt.Sprintf("incoming webhook creation failed: %s", joinErr.Error()))
			p.API.LogError("failed to add bot to incoming webhook channel", "bot_user_id", bot.UserId, "error", joinErr.Error())
		} else {
			var whErr error
			incomingWebhookURL, whErr = p.createIncomingWebhookForBot(authToken, bot.UserId, req)
			if whErr != nil {
				partialErrors = append(partialErrors, fmt.Sprintf("incoming webhook creation failed: %s", whErr.Error()))
				p.API.LogError("failed to create incoming webhook for bot", "error", whErr.Error())
			} else if !recordHook(exerciseWebhook{ID: hookIDFromPublicURL(incomingWebhookURL), Type: webhookTypeIncoming}) {
				incomingWebhookURL = ""
			}
		}
	}

	var outgoingHookID string
	if wantsOutgoing && authToken != "" && siteURL != "" {
		var outErr error
		if outgoingHookID, outErr = p.createOutgoingWebhookForBot(authToken, bot.UserId, req); outErr != nil {
			partialErrors = append(partialErrors, fmt.Sprintf("outgoing webhook creation failed: %s", outErr.Error()))
			p.API.LogError("failed to create outgoing webhook for bot", "error", outErr.Error())
		} else {
			outgoingWebhookCreated = recordHook(exerciseWebhook{ID: outgoingHookID, Type: webhookTypeOutgoing})
		}
	}

	// Best-effort: reopen the requester DM if submit-time open failed.
	if req.DMChannelID == "" && (tokenValue != "" || incomingWebhookURL != "") {
		if dm, dmErr := p.API.GetDirectChannel(req.RequesterID, p.botUserID); dmErr != nil {
			p.API.LogWarn("failed to open DM for bot extras delivery", "user_id", req.RequesterID, "error", dmErr.Error())
		} else {
			req.DMChannelID = dm.Id
		}
	}

	// If a token was requested but delivery of the extras reply would leave
	// an undeliverable secret, delete the bot when token creation succeeded
	// but we cannot open the DM channel. sendBotExtrasReply logs failures;
	// permanent-delete is reserved for total delivery failure of a token.
	if req.RequestToken && tokenValue != "" && req.DMChannelID == "" {
		p.API.LogError("bot token created but no DM channel for delivery; deleting bot", "bot_user_id", bot.UserId)
		if delErr := p.API.PermanentDeleteBot(bot.UserId); delErr != nil {
			p.API.LogError("failed to delete bot after token delivery failure", "bot_user_id", bot.UserId, "error", delErr.Error())
			return p.endUndelivered(cards,
				fmt.Sprintf("⚠️ Fully approved, but the token could not be delivered and bot @%s could not be removed automatically — an admin should delete it.", bot.Username))
		}
		return p.endUndelivered(cards,
			fmt.Sprintf("⚠️ Fully approved, but the token could not be delivered, so bot @%s was removed. Ask the requester to submit again.", bot.Username))
	}

	// Incoming webhook URL is also a secret — delete the hook if undeliverable.
	if incomingWebhookURL != "" && req.DMChannelID == "" {
		p.API.LogError("bot incoming webhook created but no DM channel for URL delivery")
		if hookID := hookIDFromPublicURL(incomingWebhookURL); hookID != "" {
			if delErr := p.deleteWebhook(webhookTypeIncoming, hookID, actingUser.Id); delErr != nil {
				p.API.LogError("failed to delete undeliverable bot incoming webhook", "hook_id", hookID, "error", delErr.Error())
				partialErrors = append(partialErrors, "incoming webhook URL could not be delivered and the hook could not be removed automatically")
			} else {
				partialErrors = append(partialErrors, "incoming webhook URL could not be delivered, so the hook was removed")
			}
		} else {
			partialErrors = append(partialErrors, "incoming webhook URL could not be delivered (no DM channel)")
		}
		incomingWebhookURL = ""
	}

	outcome := fmt.Sprintf("✅ Fully approved (security + system) for %s. Bot **@%s** created.", requesterUsername(requester, req.RequesterID), bot.Username)
	p.logAudit(auditApproved, exerciseLabel(req.Exercise)+fmt.Sprintf("@%s gave final approval; bot @%s created for %s",
		actingUser.Username, bot.Username, requesterUsername(requester, req.RequesterID)), cards.cardPostID)
	result := p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: true, ApproverUsername: actingUser.Username,
		CardTitle:  exerciseLabel(req.Exercise) + "Bot Account Request Approved",
		CardText:   fmt.Sprintf("Fully approved (security + system). Bot **@%s** has been created.", bot.Username),
		StatusLine: outcome,
	})
	p.sendBotExtrasReply(req, tokenValue, tokenExpiresAt, incomingWebhookURL, outgoingWebhookCreated, partialErrors)
	return result
}

// decideWebhookRequest approves or denies a webhook request on behalf of
// actingUserID through the two-step engine. On final approval it creates
// the webhook and sends the requester the result; an incoming webhook
// whose URL can't be delivered is deleted. See decideChannelRequest for
// clickedPostID.
func (p *Plugin) decideWebhookRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil {
		return decisionResult{Reply: "Could not verify your identity."}
	}

	key := kvWebhookRequestPrefix + requestID
	req, rawReq, err := p.loadWebhookRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load webhook request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeWebhook, approve); msg != "" {
		return decisionResult{Reply: msg}
	}

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeWebhook,
	})

	canSec, canSys := p.twoStepPools(actingUser, "webhook")

	// Best-effort for cards and audit; approve checks requesterErr.
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	channel, channelErr := p.API.GetChannel(req.ChannelID)
	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID)

	// Expiry counts from submission, even when one step is already approved.
	if days := p.getConfiguration().ExpiryDays(); (canSec || canSys) && requestExpired(req.CreatedAt, days, model.GetMillis()) {
		return p.expireRequest(cards, expiredRequest{
			kvKey:       key,
			raw:         rawReq,
			kind:        pendingKindWebhook,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   exerciseLabel(req.Exercise) + "Webhook Request Expired",
			cardText:    fmt.Sprintf("Your request for a %s webhook **%s** expired before it was reviewed. Submit it again if you still need it.", req.WebhookType, escapeMarkdown(req.DisplayName)),
			audit: exerciseLabel(req.Exercise) + fmt.Sprintf("@%s closed the %s webhook request for %s from %s, which was not reviewed within %d days",
				actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID), days),
		})
	}

	if !approve {
		if !canSec && !canSys {
			return decisionResult{Reply: "You don't have permission to deny this request."}
		}
		if res, ok := p.claimRecord(key, rawReq, cards, "webhook request"); !ok {
			return res
		}
		p.releasePending(pendingKindWebhook, req.RequesterID, req.ID)
		outcome := fmt.Sprintf("❌ Denied by @%s for %s. %s webhook **%s**.", actingUser.Username, requesterUsername(requester, req.RequesterID), req.WebhookType, escapeMarkdown(req.DisplayName))
		p.logAudit(auditDenied, exerciseLabel(req.Exercise)+fmt.Sprintf("@%s denied a %s webhook request for %s",
			actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName)), cards.cardPostID)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: false, ApproverUsername: actingUser.Username,
			CardTitle:  exerciseLabel(req.Exercise) + "Webhook Request Denied",
			CardText:   fmt.Sprintf("Denied by @%s. Your request for a %s webhook **%s** was not approved. Reply to this thread if you'd like more information.", actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName)),
			StatusLine: outcome,
		})
	}

	step, fill, reason := planApproval(req.twoStepState, canSec, canSys, actingUser.Id, req.RequesterID)
	if !fill {
		return twoStepBlocked(cards, actingUser.Username, reason, p.webhookApprovalHeader(req.Exercise), func(title, notice string) *model.MessageAttachment {
			cardRequester := requester
			if cardRequester == nil {
				cardRequester = &model.User{Username: requesterUsername(nil, req.RequesterID)}
			}
			cardChannel := channel
			if cardChannel == nil {
				cardChannel = &model.Channel{Name: req.ChannelID}
			}
			return p.webhookApprovalAttachmentWithTitledNotice(req, cardRequester, cardChannel, title, notice)
		})
	}

	// Re-check what submit checked, before either step, so neither step is
	// filled on a request that can no longer be carried out. A failed check
	// leaves the request and its steps as they are, so it can be denied.
	if msg := p.webhookApprovalBlocker(req, requester, requesterErr, channel, channelErr); msg != "" {
		return decisionResult{Reply: msg}
	}

	newState := p.applyTwoStepApproval(req.twoStepState, step, actingUser, model.GetMillis())
	if !newState.complete() {
		newReq := *req
		newReq.twoStepState = newState
		return p.recordFirstApproval(cards, key, rawReq, &newReq, step, ticketProgress{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Title: "Webhook Request Update",
			Text:  fmt.Sprintf("Your %s webhook request for **%s** received the %s approval; awaiting the other approval.", req.WebhookType, escapeMarkdown(req.DisplayName), step),
		}, p.webhookApprovalHeader(req.Exercise), func() *model.MessageAttachment { return p.webhookApprovalAttachment(&newReq, requester, channel) })
	}

	if res, ok := p.claimRecord(key, rawReq, cards, "webhook request"); !ok {
		return res
	}

	dmMsg, hookID, createErr := p.createWebhookForRequest(req, actingUser.Id)
	if createErr != nil {
		p.API.LogError("failed to create webhook on approval", "error", createErr.Error())
		if restoreErr := p.API.KVSet(key, rawReq); restoreErr != nil {
			p.API.LogError("failed to restore webhook request after create failure", "error", restoreErr.Error())
			return p.lostRequest(cards, pendingKindWebhook, req.RequesterID, actingUser.Username,
				fmt.Sprintf("Could not create the webhook (%s), and the pending request could not be saved — ask the requester to resubmit.", createErr.Error()))
		}
		notice := func(clicked bool) string {
			return fmt.Sprintf("Couldn't create the webhook: %s. The request is still pending — %s.", createErr.Error(), retryText(clicked))
		}
		// The cards are for clicking, whoever ran into the failure.
		return p.repaintCards(cards, key, notice(clickedPostID != ""),
			webhookRetryHeader(req.Exercise),
			p.webhookApprovalAttachmentWithNotice(req, requester, channel, notice(true)))
	}
	p.releasePending(pendingKindWebhook, req.RequesterID, req.ID)
	if req.Exercise && !p.recordExerciseWebhook(req.TeamID, exerciseWebhook{ID: hookID, Type: req.WebhookType}, actingUser.Id) {
		return p.closeRemovedExerciseRequest(cards, req.RequesterID, req.DMRootPostID, req.DMChannelID)
	}

	// Best-effort: reopen the requester DM if submit-time open failed.
	if req.DMChannelID == "" {
		if dm, dmErr := p.API.GetDirectChannel(req.RequesterID, p.botUserID); dmErr != nil {
			p.API.LogWarn("failed to open DM for webhook URL delivery", "user_id", req.RequesterID, "error", dmErr.Error())
		} else {
			req.DMChannelID = dm.Id
		}
	}

	// Deliver via the existing ticket thread. If DM channel is missing for
	// an incoming webhook (secret URL), delete the hook.
	if req.WebhookType == webhookTypeIncoming && req.DMChannelID == "" {
		p.API.LogError("webhook created but no DM channel for URL delivery", "hook_id", hookID)
		if delErr := p.deleteWebhook(req.WebhookType, hookID, actingUser.Id); delErr != nil {
			p.API.LogError("failed to delete webhook after URL delivery failure", "hook_id", hookID, "error", delErr.Error())
			return p.endUndelivered(cards,
				fmt.Sprintf("⚠️ Fully approved, but the URL could not be delivered and the %s webhook **%s** could not be removed automatically — an admin should delete it.", req.WebhookType, escapeMarkdown(req.DisplayName)))
		}
		return p.endUndelivered(cards,
			fmt.Sprintf("⚠️ Fully approved, but the URL could not be delivered, so the %s webhook **%s** was removed. Ask the requester to submit again.", req.WebhookType, escapeMarkdown(req.DisplayName)))
	}

	outcome := fmt.Sprintf("✅ Fully approved (security + system) for %s. %s webhook **%s** created.", requesterUsername(requester, req.RequesterID), req.WebhookType, escapeMarkdown(req.DisplayName))
	p.logAudit(auditApproved, exerciseLabel(req.Exercise)+fmt.Sprintf("@%s gave final approval; %s webhook \"%s\" created for %s",
		actingUser.Username, req.WebhookType, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID)), cards.cardPostID)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: true, ApproverUsername: actingUser.Username,
		CardTitle:  exerciseLabel(req.Exercise) + "Webhook Request Approved",
		CardText:   fmt.Sprintf("Fully approved (security + system). %s", dmMsg),
		StatusLine: outcome,
	})
}
