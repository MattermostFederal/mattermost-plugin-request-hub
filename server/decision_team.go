package main

import (
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
)

// systemAdminDecider returns the acting user when they may decide a
// System-Admin-only request type, or the reply refusing them. what names
// the type in the refusal, as in "Team Admin requests".
func (p *Plugin) systemAdminDecider(actingUserID, what string) (*model.User, *decisionResult) {
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil {
		return nil, &decisionResult{Reply: "Could not verify your identity."}
	}
	if !actingUser.IsSystemAdmin() {
		return nil, &decisionResult{Reply: fmt.Sprintf("Only System Admins can approve or deny %s.", what)}
	}
	return actingUser, nil
}

// decideTeamAdminRequest approves or denies a Team Admin request on behalf
// of actingUserID. System Admins and the Team Admins of the request's team
// may decide it (see canApproveTeamAdminRequest). See decideChannelRequest
// for clickedPostID.
func (p *Plugin) decideTeamAdminRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil {
		return decisionResult{Reply: "Could not verify your identity."}
	}

	req, rawReq, err := p.loadTeamAdminRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load team admin request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if !p.canApproveTeamAdminRequest(actingUser, req.TeamID) {
		return decisionResult{Reply: "Only System Admins and the team's Team Admins can approve or deny Team Admin requests."}
	}
	if approve && req.RequesterID == actingUser.Id && !actingUser.IsSystemAdmin() {
		return decisionResult{Reply: ownRequestText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeTeamAdmin, approve); msg != "" {
		return decisionResult{Reply: msg}
	}

	team, teamErr := p.API.GetTeam(req.TeamID)
	teamRef := teamDisplayRef(team, teamErr, req.TeamID)

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeTeamAdmin,
	})

	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID)

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		requester, _ := p.API.GetUser(req.RequesterID)
		nominees := p.mentionList(req.NomineeIDs)
		return p.expireRequest(cards, expiredRequest{
			kvKey:       kvTeamAdminRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindTeamAdmin,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Team Admin Request Expired",
			cardText:    fmt.Sprintf("Your request to make %s Team Admin in %s expired before it was reviewed. Submit it again if you still need it.", nominees, teamRef),
			audit: fmt.Sprintf("@%s closed the Team Admin request for %s in %s from %s, which was not reviewed within %d days",
				actingUser.Username, nominees, teamRef, requesterUsername(requester, req.RequesterID), days),
		})
	}

	// Re-check what submit checked. A failed check leaves the request
	// pending so it can be denied.
	if approve {
		if msg := p.teamAdminApprovalBlocker(req, team, teamErr); msg != "" {
			return decisionResult{Reply: msg}
		}
	}

	if res, ok := p.claimRecord(kvTeamAdminRequestPrefix+req.ID, rawReq, cards, "team admin request"); !ok {
		return res
	}
	// The KV key was already removed by the atomic claim above.
	p.releasePending(pendingKindTeamAdmin, req.RequesterID, req.ID)

	requester, _ := p.API.GetUser(req.RequesterID)
	nominees := p.mentionList(req.NomineeIDs)

	if approve {
		p.promoteTeamAdmins(req)
		outcome := fmt.Sprintf("✅ Approved by @%s for %s. %s promoted to Team Admin in %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), nominees, teamRef)
		p.logAudit(auditApproved, fmt.Sprintf("@%s promoted %s to Team Admin in %s (requested by %s)",
			actingUser.Username, nominees, teamRef, requesterUsername(requester, req.RequesterID)), cards.cardPostID)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Team Admin Request Approved",
			CardText:   fmt.Sprintf("Approved by @%s. %s is now Team Admin in %s.", actingUser.Username, nominees, teamRef),
			StatusLine: outcome,
		})
	}
	outcome := fmt.Sprintf("❌ Denied by @%s for %s. Team Admin for %s in %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), nominees, teamRef)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied a Team Admin request for %s in %s",
		actingUser.Username, nominees, teamRef), cards.cardPostID)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Team Admin Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your request to make %s Team Admin in %s was not approved. Reply to this thread if you'd like more information.", actingUser.Username, nominees, teamRef),
		StatusLine: outcome,
	})
}

// decideTeamMemberRequest approves or denies a request to add users to a
// team on behalf of actingUserID. Only System Admins may decide it.
func (p *Plugin) decideTeamMemberRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, refused := p.systemAdminDecider(actingUserID, "team member requests")
	if refused != nil {
		return *refused
	}

	req, rawReq, err := p.loadTeamMemberRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load team member request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeTeamMember, approve); msg != "" {
		return decisionResult{Reply: msg}
	}

	team, teamErr := p.API.GetTeam(req.TeamID)
	teamRef := teamDisplayRef(team, teamErr, req.TeamID)

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeTeamMember,
	})

	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID)

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		requester, _ := p.API.GetUser(req.RequesterID)
		nominees := p.mentionList(req.NomineeIDs)
		return p.expireRequest(cards, expiredRequest{
			kvKey:       kvTeamMemberRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindTeamMember,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Team Member Request Expired",
			cardText:    fmt.Sprintf("Your request to add %s to %s expired before it was reviewed. Submit it again if you still need it.", nominees, teamRef),
			audit: fmt.Sprintf("@%s closed the request to add %s to %s from %s, which was not reviewed within %d days",
				actingUser.Username, nominees, teamRef, requesterUsername(requester, req.RequesterID), days),
		})
	}

	// Re-check what submit checked. A failed check leaves the request
	// pending so it can be denied.
	if approve {
		if msg := p.teamMemberApprovalBlocker(req, team, teamErr); msg != "" {
			return decisionResult{Reply: msg}
		}
	}

	if res, ok := p.claimRecord(kvTeamMemberRequestPrefix+req.ID, rawReq, cards, "team member request"); !ok {
		return res
	}
	// The KV key was already removed by the atomic claim above.
	p.releasePending(pendingKindTeamMember, req.RequesterID, req.ID)

	requester, _ := p.API.GetUser(req.RequesterID)
	nominees := p.mentionList(req.NomineeIDs)

	if approve {
		var outcome string
		failed := p.addTeamMembers(req)
		added := exceptIDs(req.NomineeIDs, failed)
		addedMentions := p.mentionList(added)
		failedMentions := p.mentionList(failed)
		switch {
		case len(added) > 0 && len(failed) > 0:
			outcome = fmt.Sprintf("✅ Approved by @%s for %s. Added %s to %s. Failed for %s.",
				actingUser.Username, requesterUsername(requester, req.RequesterID), addedMentions, teamRef, failedMentions)
		case len(added) > 0:
			outcome = fmt.Sprintf("✅ Approved by @%s for %s. Added %s to %s.",
				actingUser.Username, requesterUsername(requester, req.RequesterID), addedMentions, teamRef)
		case len(failed) > 0:
			outcome = fmt.Sprintf("⚠️ Approved by @%s for %s, but could not add %s to %s.",
				actingUser.Username, requesterUsername(requester, req.RequesterID), failedMentions, teamRef)
		default:
			outcome = fmt.Sprintf("✅ Approved by @%s for %s. No users were added to %s.",
				actingUser.Username, requesterUsername(requester, req.RequesterID), teamRef)
		}
		if len(added) > 0 {
			p.logAudit(auditApproved, fmt.Sprintf("@%s added %s to %s (requested by %s)",
				actingUser.Username, addedMentions, teamRef, requesterUsername(requester, req.RequesterID)), cards.cardPostID)
		} else if len(failed) > 0 {
			p.logAudit(auditApproved, fmt.Sprintf("@%s approved team member request for %s but adds failed for %s",
				actingUser.Username, teamRef, failedMentions), cards.cardPostID)
		}
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Team Member Request Approved",
			CardText:   outcome,
			StatusLine: outcome,
		})
	}
	outcome := fmt.Sprintf("❌ Denied by @%s for %s. Team member add for %s in %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), nominees, teamRef)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied adding %s to %s",
		actingUser.Username, nominees, teamRef), cards.cardPostID)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Team Member Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your request to add %s to %s was not approved.", actingUser.Username, nominees, teamRef),
		StatusLine: outcome,
	})
}

// decideTeamGuestRequest approves or denies a guest invite request on
// behalf of actingUserID. Only System Admins may decide it. Approving
// sends the invitation as the acting admin.
func (p *Plugin) decideTeamGuestRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, refused := p.systemAdminDecider(actingUserID, "guest invite requests")
	if refused != nil {
		return *refused
	}

	req, rawReq, err := p.loadTeamGuestRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load team guest request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeTeamGuest, approve); msg != "" {
		return decisionResult{Reply: msg}
	}

	team, teamErr := p.API.GetTeam(req.TeamID)
	teamRef := teamDisplayRef(team, teamErr, req.TeamID)

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeTeamGuest,
	})

	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID)

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		requester, _ := p.API.GetUser(req.RequesterID)
		return p.expireRequest(cards, expiredRequest{
			kvKey:       kvTeamGuestRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindTeamGuest,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Guest Invite Request Expired",
			cardText:    fmt.Sprintf("Your request to invite **%s** to %s as a guest expired before it was reviewed. Submit it again if you still need it.", escapeMarkdown(req.Email), teamRef),
			audit: fmt.Sprintf("@%s closed the guest invite request for %s from %s, which was not reviewed within %d days",
				actingUser.Username, teamRef, requesterUsername(requester, req.RequesterID), days),
		})
	}

	// Re-check what submit checked. A failed check leaves the request
	// pending so it can be denied.
	if approve {
		if msg := p.teamGuestApprovalBlocker(req, team, teamErr); msg != "" {
			return decisionResult{Reply: msg}
		}
	}

	if res, ok := p.claimRecord(kvTeamGuestRequestPrefix+req.ID, rawReq, cards, "team guest request"); !ok {
		return res
	}

	requester, _ := p.API.GetUser(req.RequesterID)

	if approve {
		if fulfillErr := p.fulfillTeamGuestRequest(req, actingUser.Id); fulfillErr != nil {
			if restoreErr := p.API.KVSet(kvTeamGuestRequestPrefix+req.ID, rawReq); restoreErr != nil {
				p.API.LogError("failed to restore team guest request", "error", restoreErr.Error())
				return p.lostRequest(cards, pendingKindTeamGuest, req.RequesterID, actingUser.Username,
					fulfillErr.Error()+" The pending request could not be saved either; ask the requester to submit it again.")
			}
			return decisionResult{Reply: fulfillErr.Error()}
		}
		p.releasePending(pendingKindTeamGuest, req.RequesterID, req.ID)
		outcome := fmt.Sprintf("✅ Approved by @%s for %s. Guest invitation emailed to %s for %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), escapeMarkdown(req.Email), teamRef)
		p.logAudit(auditApproved, fmt.Sprintf("@%s guest invite for %s email \"%s\" (requested by %s)",
			actingUser.Username, teamRef, escapeMarkdown(req.Email), requesterUsername(requester, req.RequesterID)), cards.cardPostID)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Guest Invite Request Approved",
			CardText:   fmt.Sprintf("Approved by @%s. A guest invitation was emailed to **%s** for %s (channel-scoped guest access).", actingUser.Username, escapeMarkdown(req.Email), teamRef),
			StatusLine: outcome,
		})
	}
	p.releasePending(pendingKindTeamGuest, req.RequesterID, req.ID)
	outcome := fmt.Sprintf("❌ Denied by @%s for %s. Guest invite for %s.", actingUser.Username, requesterUsername(requester, req.RequesterID), teamRef)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied guest invite for %s",
		actingUser.Username, teamRef), cards.cardPostID)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Guest Invite Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your guest invite request for %s was not approved.", actingUser.Username, teamRef),
		StatusLine: outcome,
	})
}

// decideTeamRequest approves or denies a team creation request on behalf
// of actingUserID. System Admins may decide it, and so may Team Admins of
// the approval team when AllowTeamAdminTeamApprovers is on.
func (p *Plugin) decideTeamRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil {
		return decisionResult{Reply: "Could not verify your identity."}
	}
	if !p.canApproveTeamRequest(actingUser) {
		return decisionResult{Reply: "You aren't allowed to approve or deny team creation requests."}
	}

	req, rawReq, err := p.loadTeamRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load team request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if approve && req.RequesterID == actingUser.Id && !actingUser.IsSystemAdmin() {
		return decisionResult{Reply: ownRequestText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeTeam, approve); msg != "" {
		return decisionResult{Reply: msg}
	}

	p.refreshTicketThread(ticketThread{
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeTeam,
	})

	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID)

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		requester, _ := p.API.GetUser(req.RequesterID)
		return p.expireRequest(cards, expiredRequest{
			kvKey:       kvTeamRequestPrefix + req.ID,
			raw:         rawReq,
			kind:        pendingKindTeam,
			requestID:   req.ID,
			requesterID: req.RequesterID,
			dmRootID:    req.DMRootPostID,
			dmChannelID: req.DMChannelID,
			days:        days,
			cardTitle:   "Team Creation Request Expired",
			cardText:    fmt.Sprintf("Your request to create team **%s** expired before it was reviewed. Submit it again if you still need it.", escapeMarkdown(req.DisplayName)),
			audit: fmt.Sprintf("@%s closed the team creation request for %s from %s, which was not reviewed within %d days",
				actingUser.Username, escapeMarkdown(req.DisplayName), requesterUsername(requester, req.RequesterID), days),
		})
	}

	if approve {
		if msg := p.teamApprovalBlocker(req); msg != "" {
			return decisionResult{Reply: msg}
		}
	}

	if res, ok := p.claimRecord(kvTeamRequestPrefix+req.ID, rawReq, cards, "team request"); !ok {
		return res
	}

	requester, _ := p.API.GetUser(req.RequesterID)

	if approve {
		team, createErr := p.createTeamForRequest(req)
		if createErr != nil {
			p.API.LogError("failed to create team on approval", "error", createErr.Error())
			if restoreErr := p.storeTeamRequest(req); restoreErr != nil {
				p.API.LogError("failed to restore team request after create failure", "error", restoreErr.Error())
				return p.lostRequest(cards, pendingKindTeam, req.RequesterID, actingUser.Username,
					fmt.Sprintf("Could not create the team (%s), and the pending request could not be saved — ask the requester to resubmit.", createErr.Error()))
			}
			return decisionResult{Reply: fmt.Sprintf("Could not create the team: %s. The request is still pending.", createErr.Error())}
		}
		p.releasePending(pendingKindTeam, req.RequesterID, req.ID)
		adminNote := ""
		if req.RequestTeamAdmin {
			adminNote = " Requester promoted to Team Admin."
		}
		outcome := fmt.Sprintf("✅ Approved by @%s for %s. Team **%s** created.%s", actingUser.Username, requesterUsername(requester, req.RequesterID), escapeMarkdown(team.DisplayName), adminNote)
		p.logAudit(auditApproved, fmt.Sprintf("@%s approved team %s (%s) requested by %s",
			actingUser.Username, escapeMarkdown(req.DisplayName), team.Name, requesterUsername(requester, req.RequesterID)), cards.cardPostID)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: true, ApproverUsername: actingUser.Username,
			CardTitle:  "Team Creation Request Approved",
			CardText:   fmt.Sprintf("Approved by @%s. Team **%s** has been created.%s", actingUser.Username, escapeMarkdown(req.DisplayName), adminNote),
			StatusLine: outcome,
		})
	}
	p.releasePending(pendingKindTeam, req.RequesterID, req.ID)
	outcome := fmt.Sprintf("❌ Denied by @%s for %s. Team **%s** (`/%s`).", actingUser.Username, requesterUsername(requester, req.RequesterID), escapeMarkdown(req.DisplayName), req.Name)
	p.logAudit(auditDenied, fmt.Sprintf("@%s denied a team creation request for %s",
		actingUser.Username, escapeMarkdown(req.DisplayName)), cards.cardPostID)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: false, ApproverUsername: actingUser.Username,
		CardTitle:  "Team Creation Request Denied",
		CardText:   fmt.Sprintf("Denied by @%s. Your request to create team **%s** was not approved. Reply to this thread if you'd like more information.", actingUser.Username, escapeMarkdown(req.DisplayName)),
		StatusLine: outcome,
	})
}
