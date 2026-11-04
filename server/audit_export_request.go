package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const kvAuditExportRequestPrefix = "audit_export_request_"

// auditExportRequest is a security approver's request for a copy of the
// audit history. Only System Admins may approve it; approving builds the
// export and posts it as a file in the requester's ticket thread.
type auditExportRequest struct {
	ID          string            `json:"id"`
	RequesterID string            `json:"requester_id"`
	Filter      auditExportFilter `json:"filter"`
	Reason      string            `json:"reason,omitempty"`
	CreatedAt   int64             `json:"created_at,omitempty"`
	// Exercise is never set: exercise mode doesn't seed these. It is here
	// because every request record carries it.
	Exercise bool `json:"exercise,omitempty"`

	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`
}

func (r *auditExportRequest) pendingFields() (string, string, *int64) {
	return r.ID, r.RequesterID, &r.CreatedAt
}

// pendingKey refuses a second pending request for the same export.
func (r *auditExportRequest) pendingKey() string {
	f := r.Filter
	return fmt.Sprintf("%s/%s/%s/%s/%d/%d", f.Format, f.RequestID, f.Kind, f.Requester, f.Since, f.Until)
}

const (
	auditExportForAdminsText   = "System Admins can download the audit history directly: run `/%s audit export`, or open Audit history in the Request Hub."
	auditExportNotApproverText = "Only security approvers can request a copy of the audit history."
)

// auditAccess says what the user may do with the audit history: System
// Admins read and download it, and security approvers who aren't System
// Admins ask for a copy, which a System Admin approves.
func (p *Plugin) auditAccess(user *model.User) (canView, canRequest bool) {
	if user == nil || user.DeleteAt != 0 {
		return false, false
	}
	if user.IsSystemAdmin() {
		return true, false
	}
	isSecurity, err := p.canApproveStep(user, stepSecurity)
	if err != nil {
		p.API.LogWarn("security approver check failed for the audit export", "user_id", user.Id, "error", err.Error())
		return false, false
	}
	return false, isSecurity
}

// checkExportFilter refuses a filter the export can't use.
func checkExportFilter(f auditExportFilter) error {
	if f.Format != "csv" && f.Format != "json" {
		return errors.New("pick CSV or JSON")
	}
	if f.Kind != "" {
		if _, ok := pendingKinds[f.Kind]; !ok {
			return errors.New("that request type isn't one the plugin handles")
		}
	}
	if f.RequestID != "" && !model.IsValidId(f.RequestID) {
		return errors.New("the request ID isn't valid")
	}
	if f.Requester != "" && !model.IsValidId(f.Requester) {
		return errors.New("the requester isn't valid")
	}
	if f.Since < 0 || f.Until < 0 || (f.Since > 0 && f.Until > 0 && f.Since > f.Until) {
		return errors.New("the start date must be before the end date")
	}
	return nil
}

// describeExportFilter says in words what an export covers.
func (p *Plugin) describeExportFilter(f auditExportFilter) string {
	var parts []string
	if f.RequestID != "" {
		parts = append(parts, fmt.Sprintf("request `%s`", f.RequestID))
	}
	if f.Kind != "" {
		parts = append(parts, kindLabel(f.Kind)+" requests")
	}
	if f.Requester != "" {
		who := "user " + f.Requester
		if user, appErr := p.API.GetUser(f.Requester); appErr == nil && user != nil {
			who = "@" + user.Username
		}
		parts = append(parts, "requested by "+who)
	}
	day := func(ms int64) string { return time.UnixMilli(ms).UTC().Format("January 2, 2006") }
	switch {
	case f.Since > 0 && f.Until > 0:
		parts = append(parts, fmt.Sprintf("active from %s to %s", day(f.Since), day(f.Until)))
	case f.Since > 0:
		parts = append(parts, "active since "+day(f.Since))
	case f.Until > 0:
		parts = append(parts, "active until "+day(f.Until))
	}
	if len(parts) == 0 {
		return "The whole audit history"
	}
	return "Only " + strings.Join(parts, ", ")
}

// submitAuditExportRequest asks for a copy of the audit history, for
// System Admins to approve.
func (p *Plugin) submitAuditExportRequest(requesterID string, f auditExportFilter, reason string) (submitted string, submitErr error) {
	sa := p.startSubmitAudit(pendingKindAuditExport, requesterID)
	defer sa.done(&submitErr)

	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil || requester == nil {
		return "", errors.New("could not load your account; try again")
	}
	if requester.IsSystemAdmin() {
		return "", errors.Errorf(auditExportForAdminsText, commandTrigger)
	}
	if _, canRequest := p.auditAccess(requester); !canRequest {
		return "", errors.New(auditExportNotApproverText)
	}
	f.Format = strings.ToLower(strings.TrimSpace(f.Format))
	if f.Format == "" {
		f.Format = "csv"
	}
	if err := checkExportFilter(f); err != nil {
		return "", err
	}
	if err := checkTextLen("reason", reason, maxReasonLen); err != nil {
		return "", err
	}

	req := &auditExportRequest{
		ID:          model.NewId(),
		RequesterID: requesterID,
		Filter:      f,
		Reason:      strings.TrimSpace(reason),
		CreatedAt:   model.GetMillis(),
	}
	sa.built(req, requester)
	if err := p.reservePending(pendingKindAuditExport, req.RequesterID, newPendingEntry(req), p.getConfiguration()); err != nil {
		return "", err
	}
	if err := p.storeAuditExportRequest(req); err != nil {
		p.releasePending(pendingKindAuditExport, req.RequesterID, req.ID)
		p.API.LogError("failed to store audit export request", "error", err.Error())
		return "", errors.New("could not save your request; try again")
	}
	sa.markStored()
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		_ = p.API.KVDelete(kvAuditExportRequestPrefix + req.ID)
		p.releasePending(pendingKindAuditExport, req.RequesterID, req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot audit export request")
	}

	approvalPostID, approvalChannelID, err := p.postApprovalAttachment(req.ID, p.auditExportApprovalAttachment(req, requester),
		p.systemAdminMentions()+" — a security approver asks for a copy of the audit history.", p.ensureAuditChannel())
	if err != nil {
		_ = p.API.KVDelete(kvAuditExportRequestPrefix + req.ID)
		p.releasePending(pendingKindAuditExport, req.RequesterID, req.ID)
		p.API.LogError("failed to post audit export request for approval", "error", err.Error())
		return "", errors.New("could not send your request to the approvers; try again or contact a System Admin")
	}
	if approvalPostID == "" {
		p.resolveCardsIfHandled(pendingKindAuditExport, req.ID)
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	req.DMRootPostID, req.DMChannelID = p.postTicketCreatedDM(req.RequesterID,
		p.auditExportApprovalAttachment(req, requester), ticketSubmittedIntro("audit export request"))

	p.persistTicketPosts(ticketPersist{
		KVKey: kvAuditExportRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "audit export request",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Kind: requestTypeAuditExport,
		},
	})
	return "Your request for a copy of the audit history has been sent to the System Admins. Once one approves it, the file is posted in your request thread with the Request Hub bot.", nil
}

func (p *Plugin) auditExportApprovalAttachment(req *auditExportRequest, requester *model.User) *model.MessageAttachment {
	fields := []*model.MessageAttachmentField{
		{Title: "Requested by", Value: "@" + requester.Username, Short: true},
		{Title: "Format", Value: strings.ToUpper(req.Filter.Format), Short: true},
		{Title: "Covers", Value: p.describeExportFilter(req.Filter), Short: false},
	}
	if req.Reason != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Reason", Value: escapeMarkdown(req.Reason), Short: false})
	}
	siteURL := "/plugins/" + manifest.Id
	action := func(id, name, style, route string) *model.PostAction {
		return &model.PostAction{
			Id: id, Name: name, Type: model.PostActionTypeButton, Style: style,
			Integration: &model.PostActionIntegration{URL: siteURL + route, Context: map[string]any{actionContextRequestID: req.ID}},
		}
	}
	return &model.MessageAttachment{
		Title:  "Audit export request",
		Text:   "The export holds every recorded event for the requests it covers, including the text of ticket thread messages.",
		Color:  "#F4A221",
		Fields: fields,
		Footer: requestIDFooter(req.ID),
		Actions: []*model.PostAction{
			action("approve", "Approve", "primary", routeApproveAuditExport),
			action("deny", "Deny", "danger", routeDenyAuditExport),
		},
	}
}

func (p *Plugin) storeAuditExportRequest(req *auditExportRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal audit export request")
	}
	if appErr := p.API.KVSet(kvAuditExportRequestPrefix+req.ID, data); appErr != nil {
		return errors.Wrap(appErr, "failed to store audit export request")
	}
	return nil
}

func (p *Plugin) loadAuditExportRequest(id string) (*auditExportRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvAuditExportRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load audit export request")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req auditExportRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal audit export request")
	}
	return &req, data, nil
}

// auditExportApprovalBlocker re-checks, at approval, that the requester
// may still have the export.
func (p *Plugin) auditExportApprovalBlocker(requester *model.User, requesterErr *model.AppError) string {
	if requesterErr != nil || requester == nil || requester.DeleteAt != 0 {
		return "The requester's account is no longer active. Deny this request instead."
	}
	if _, canRequest := p.auditAccess(requester); !canRequest && !requester.IsSystemAdmin() {
		return "The requester is no longer a security approver. Deny this request instead."
	}
	return ""
}

// decideAuditExportRequest approves or denies an audit export request on
// behalf of actingUserID. Only System Admins may decide it. Approving
// builds the export as asked and posts it as a file in the requester's
// ticket thread.
func (p *Plugin) decideAuditExportRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	actingUser, refused := p.systemAdminDecider(actingUserID, "audit export requests")
	if refused != nil {
		return *refused
	}
	req, rawReq, err := p.loadAuditExportRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load audit export request", "error", err.Error())
		return decisionResult{Reply: "Could not load that request."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if msg := p.decisionRefusal(clickedPostID, req.ApprovalPostID, req.ID, requestTypeAuditExport, approve); msg != "" {
		return decisionResult{Reply: msg}
	}
	if approve && req.RequesterID == actingUser.Id {
		return decisionResult{Reply: ownRequestText}
	}

	p.refreshTicketThread(ticketThread{
		RequestID:         req.ID,
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeAuditExport,
	})
	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID).
		withSubject(pendingKindAuditExport, req.RequesterID, actingUser, req, false)
	requester, requesterErr := p.API.GetUser(req.RequesterID)
	covers := p.describeExportFilter(req.Filter)

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		return p.expireRequest(cards, expiredRequest{
			kvKey: kvAuditExportRequestPrefix + req.ID, raw: rawReq, kind: pendingKindAuditExport,
			requestID: req.ID, requesterID: req.RequesterID, dmRootID: req.DMRootPostID, dmChannelID: req.DMChannelID, days: days,
			cardTitle: "Audit Export Request Expired",
			cardText:  "Your request for a copy of the audit history expired before it was reviewed. Submit it again if you still need it.",
			audit: fmt.Sprintf("@%s closed the audit export request from %s, which was not reviewed within %d days",
				actingUser.Username, requesterUsername(requester, req.RequesterID), days),
		})
	}
	if approve {
		if msg := p.auditExportApprovalBlocker(requester, requesterErr); msg != "" {
			return decisionResult{Reply: msg}
		}
	}
	if res, ok := p.claimRecord(kvAuditExportRequestPrefix+req.ID, rawReq, cards, "audit export request"); !ok {
		return res
	}

	if !approve {
		p.releasePending(pendingKindAuditExport, req.RequesterID, req.ID)
		outcome := fmt.Sprintf("❌ Denied by @%s for %s. Audit export (%s).", actingUser.Username, requesterUsername(requester, req.RequesterID), strings.ToUpper(req.Filter.Format))
		p.decisionEvent(cards, eventDenied, fmt.Sprintf("@%s denied the audit export request from %s", actingUser.Username, requesterUsername(requester, req.RequesterID)), nil)
		return p.finish(cards, "", outcome, ticketClose{
			RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Approved: false, ApproverUsername: actingUser.Username,
			CardTitle:  "Audit Export Request Denied",
			CardText:   fmt.Sprintf("Denied by @%s. Your request for a copy of the audit history was not approved. Reply to this thread if you'd like more information.", actingUser.Username),
			StatusLine: outcome,
		})
	}

	fileID, requests, deliverErr := p.deliverAuditExport(req)
	if deliverErr != nil {
		p.API.LogError("failed to deliver audit export", "request_id", req.ID, "error", deliverErr.Error())
		if restoreErr := p.API.KVSet(kvAuditExportRequestPrefix+req.ID, rawReq); restoreErr != nil {
			p.API.LogError("failed to restore audit export request", "error", restoreErr.Error())
			return p.lostRequest(cards, pendingKindAuditExport, req.RequesterID, actingUser.Username,
				fmt.Sprintf("Could not send the export (%s), and the pending request could not be saved; ask the requester to submit it again.", deliverErr.Error()))
		}
		p.approveFailed(cards, fmt.Sprintf("@%s approved the audit export request from %s, but the file could not be sent",
			actingUser.Username, requesterUsername(requester, req.RequesterID)), deliverErr)
		return decisionResult{audited: true, Reply: fmt.Sprintf("Could not send the export: %s. The request is still pending.", deliverErr.Error())}
	}
	p.releasePending(pendingKindAuditExport, req.RequesterID, req.ID)
	detail := req.Filter.detail()
	detail["requests"] = requests
	detail["file_id"] = fileID
	p.decisionEvent(cards, eventApproved, fmt.Sprintf("@%s approved the audit export request from %s; the %s file (%d requests) was sent to them. %s.",
		actingUser.Username, requesterUsername(requester, req.RequesterID), strings.ToUpper(req.Filter.Format), requests, covers), detail)
	outcome := fmt.Sprintf("✅ Approved by @%s for %s. Audit export (%s, %d requests) sent.", actingUser.Username, requesterUsername(requester, req.RequesterID), strings.ToUpper(req.Filter.Format), requests)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Approved: true, ApproverUsername: actingUser.Username,
		CardTitle:  "Audit Export Request Approved",
		CardText:   fmt.Sprintf("Approved by @%s. The audit export is in this thread.", actingUser.Username),
		StatusLine: outcome,
	})
}

// deliverAuditExport builds the export the request asks for and posts it
// as a file in the requester's ticket thread, or a new DM when there is
// none. It returns the file's ID and how many requests it covers.
func (p *Plugin) deliverAuditExport(req *auditExportRequest) (fileID string, requests int, err error) {
	export, err := p.buildAuditExport(req.Filter)
	if err != nil {
		return "", 0, errors.Wrap(err, "failed to build the export")
	}
	channelID, rootID := req.DMChannelID, req.DMRootPostID
	if channelID == "" {
		dm, appErr := p.API.GetDirectChannel(req.RequesterID, p.botUserID)
		if appErr != nil {
			return "", 0, errors.Wrap(appErr, "failed to open a direct message with the requester")
		}
		channelID, rootID = dm.Id, ""
	}
	info, appErr := p.API.UploadFile(export.data, channelID, export.filename)
	if appErr != nil {
		return "", 0, errors.Wrap(appErr, "failed to upload the export")
	}
	if _, appErr := p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: channelID,
		RootId:    rootID,
		Message:   fmt.Sprintf("📎 **Audit export** (%s, %d requests). %s. It holds every recorded event for those requests, including ticket thread messages; keep it secure.", strings.ToUpper(req.Filter.Format), export.requests, p.describeExportFilter(req.Filter)),
		FileIds:   []string{info.Id},
	}); appErr != nil {
		return "", 0, errors.Wrap(appErr, "failed to post the export")
	}
	return info.Id, export.requests, nil
}

// handleAuditAccess tells the webapp whether to offer the audit history
// (System Admins) or the request for a copy (security approvers).
func (p *Plugin) handleAuditAccess(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	user, appErr := p.API.GetUser(userID)
	if appErr != nil {
		writeJSON(w, map[string]bool{"can_view": false, "can_request": false})
		return
	}
	canView, canRequest := p.auditAccess(user)
	writeJSON(w, map[string]bool{"can_view": canView, "can_request": canRequest})
}

// handleSubmitAuditExportWebapp takes the Request Audit Export form.
func (p *Plugin) handleSubmitAuditExportWebapp(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var payload struct {
		Format    string `json:"format"`
		Kind      string `json:"kind"`
		Requester string `json:"requester"` // a username, optional
		Since     int64  `json:"since"`
		Until     int64  `json:"until"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	f := auditExportFilter{Format: payload.Format, Kind: payload.Kind, Since: payload.Since, Until: payload.Until}
	if name := strings.TrimPrefix(strings.TrimSpace(payload.Requester), "@"); name != "" {
		ids, err := p.resolveUsernameList([]string{name})
		if err != nil || len(ids) != 1 {
			writeJSON(w, map[string]string{"error": "could not find a user named @" + name})
			return
		}
		f.Requester = ids[0]
	}
	msg, err := p.submitAuditExportRequest(userID, f, payload.Reason)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}
