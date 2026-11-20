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
	kvBugReportRequestPrefix = "bug_report_request_"

	bugReportCategoryBug      = "bug"
	bugReportCategoryFeedback = "feedback"

	bugReportAboutGeneral = "general"

	bugReportRequestsDisabledMsg = "Bug and feedback reports are currently disabled by an administrator."
)

// bugReportRequest is a user's report about Request Hub itself. It posts into
// the audit channel for System Admins to Acknowledge (not approve/deny).
type bugReportRequest struct {
	ID          string `json:"id"`
	RequesterID string `json:"requester_id"`
	// Category is bug or feedback.
	Category string `json:"category"`
	// AboutKind is the request type the reporter was using, or "general".
	AboutKind string `json:"about_kind"`
	// AboutOptions is what they picked on that form (prefix, team, etc.).
	AboutOptions string `json:"about_options,omitempty"`
	// RelatedRequestID links another RH request they own (SA may link any).
	RelatedRequestID string `json:"related_request_id,omitempty"`
	Description      string `json:"description"`
	CreatedAt        int64  `json:"created_at,omitempty"`
	Exercise         bool   `json:"exercise,omitempty"`

	ApprovalPostID    string `json:"approval_post_id,omitempty"`
	ApprovalChannelID string `json:"approval_channel_id,omitempty"`
	DMRootPostID      string `json:"dm_root_post_id,omitempty"`
	DMChannelID       string `json:"dm_channel_id,omitempty"`
}

func (r *bugReportRequest) pendingFields() (string, string, *int64) {
	return r.ID, r.RequesterID, &r.CreatedAt
}

// pendingKey is unique per report so a user may file more than one at once.
func (r *bugReportRequest) pendingKey() string {
	return r.ID
}

func bugReportCategoryLabel(category string) string {
	switch category {
	case bugReportCategoryFeedback:
		return "Feedback"
	default:
		return "Bug"
	}
}

func bugReportAboutLabel(about string) string {
	if about == "" || about == bugReportAboutGeneral {
		return "General / other"
	}
	return kindLabel(about)
}

// relatedRequestVisible reports whether userID may link relatedID: own
// requests always, any request when the caller is a System Admin.
func (p *Plugin) relatedRequestVisible(userID, relatedID string, asAdmin bool) error {
	if relatedID == "" {
		return nil
	}
	if !model.IsValidId(relatedID) {
		return errors.New("the related request ID isn't valid")
	}
	if h, err := p.loadHistory(relatedID); err != nil {
		return errors.New("could not look up that related request; try again")
	} else if h != nil {
		if asAdmin || h.RequesterID == userID {
			return nil
		}
		return errors.New("you can only link one of your own requests")
	}
	kind, rec, _, err := p.findPendingRequest(relatedID)
	if err != nil {
		return errors.New("could not look up that related request; try again")
	}
	if rec == nil || kind == "" {
		return errors.New("no request with that ID was found")
	}
	if asAdmin || recordRequesterID(rec) == userID {
		return nil
	}
	return errors.New("you can only link one of your own requests")
}

func (p *Plugin) checkBugReportInput(requesterID string, category, aboutKind, aboutOptions, relatedID, description string, asAdmin bool) (string, string, string, string, string, error) {
	category = strings.TrimSpace(strings.ToLower(category))
	switch category {
	case bugReportCategoryBug, bugReportCategoryFeedback:
	default:
		return "", "", "", "", "", errors.New("pick Bug or Feedback")
	}
	aboutKind = strings.TrimSpace(aboutKind)
	if aboutKind == "" {
		aboutKind = bugReportAboutGeneral
	}
	switch aboutKind {
	case bugReportAboutGeneral,
		pendingKindChannel, pendingKindAdmin, pendingKindTeam, pendingKindTeamAdmin,
		pendingKindTeamMember, pendingKindTeamGuest, pendingKindBot, pendingKindWebhook,
		pendingKindBotTransfer, pendingKindWebhookTransfer,
		pendingKindAuditExport:
		// ok
	case requestTypeChannelAdmin:
		aboutKind = pendingKindAdmin
	default:
		return "", "", "", "", "", errors.New("pick which Request Hub area this is about")
	}
	aboutOptions = strings.TrimSpace(aboutOptions)
	if err := checkTextLen("what you selected", aboutOptions, maxDescriptionLen); err != nil {
		return "", "", "", "", "", err
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return "", "", "", "", "", errors.New("describe the bug or feedback")
	}
	if err := checkTextLen("description", description, maxDescriptionLen); err != nil {
		return "", "", "", "", "", err
	}
	relatedID = strings.TrimSpace(relatedID)
	if err := p.relatedRequestVisible(requesterID, relatedID, asAdmin); err != nil {
		return "", "", "", "", "", err
	}
	return category, aboutKind, aboutOptions, relatedID, description, nil
}

// submitBugReportRequest files a bug or feedback report into the audit channel.
func (p *Plugin) submitBugReportRequest(requesterID, category, aboutKind, aboutOptions, relatedID, description string) (submitted string, submitErr error) {
	sa := p.startSubmitAudit(pendingKindBugReport, requesterID)
	defer sa.done(&submitErr)

	if !p.getConfiguration().RequestEnabled(requestTypeBugReport) {
		return "", errors.New(bugReportRequestsDisabledMsg)
	}
	requester, appErr := p.API.GetUser(requesterID)
	if appErr != nil || requester == nil {
		return "", errors.New("could not load your account; try again")
	}
	category, aboutKind, aboutOptions, relatedID, description, err := p.checkBugReportInput(
		requesterID, category, aboutKind, aboutOptions, relatedID, description, requester.IsSystemAdmin())
	if err != nil {
		return "", err
	}

	req := &bugReportRequest{
		ID:               model.NewId(),
		RequesterID:      requesterID,
		Category:         category,
		AboutKind:        aboutKind,
		AboutOptions:     aboutOptions,
		RelatedRequestID: relatedID,
		Description:      description,
		CreatedAt:        model.GetMillis(),
	}
	sa.built(req, requester)
	if err = p.reservePending(pendingKindBugReport, req.RequesterID, newPendingEntry(req), p.getConfiguration()); err != nil {
		return "", err
	}
	if err = p.storeBugReportRequest(req); err != nil {
		p.releasePending(pendingKindBugReport, req.RequesterID, req.ID)
		p.API.LogError("failed to store bug report", "error", err.Error())
		return "", errors.New("could not save your report; try again")
	}
	sa.markStored()
	rawBefore, snapErr := json.Marshal(req)
	if snapErr != nil {
		p.deletePendingRequestKV(kvBugReportRequestPrefix + req.ID)
		p.releasePending(pendingKindBugReport, req.RequesterID, req.ID)
		return "", errors.Wrap(snapErr, "failed to snapshot bug report")
	}

	mention := p.systemAdminMentions()
	msg := mention + " — @" + requester.Username + " filed a Request Hub " + strings.ToLower(bugReportCategoryLabel(req.Category)) + " report."
	// The card carries the reporter's own words. Team Admins are seeded
	// into the audit channel whenever either approver toggle is on, so the
	// card goes out as System Admin DMs instead of into a channel whose
	// membership reaches past the audience the reporter was promised.
	var auditChannel *model.Channel
	if !p.auditChannelMayIncludeTeamAdmins() {
		auditChannel = p.ensureAuditChannel()
	}
	approvalPostID, approvalChannelID, err := p.postApprovalAttachment(req.ID, p.bugReportAttachment(req, requester),
		msg, auditChannel)
	if err != nil {
		p.deletePendingRequestKV(kvBugReportRequestPrefix + req.ID)
		p.releasePending(pendingKindBugReport, req.RequesterID, req.ID)
		p.API.LogError("failed to post bug report", "error", err.Error())
		return "", errors.New("could not send your report; try again or contact a System Admin")
	}
	if approvalPostID == "" {
		p.resolveCardsIfHandled(pendingKindBugReport, req.ID)
	}
	req.ApprovalPostID = approvalPostID
	req.ApprovalChannelID = approvalChannelID

	req.DMRootPostID, req.DMChannelID = p.postTicketCreatedDM(req.RequesterID,
		p.bugReportAttachment(req, requester), ticketAcknowledgedIntro("bug/feedback report"), req.ID)

	p.persistTicketPosts(ticketPersist{
		KVKey: kvBugReportRequestPrefix + req.ID, RawBefore: rawBefore, Record: req,
		RequestID: req.ID, Label: "bug report",
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
			Kind: requestTypeBugReport,
		},
	})
	// No approval post means there was no audit channel to post into, so
	// the card went out as a DM to each System Admin instead.
	where := "to the System Admins in the audit channel"
	if approvalPostID == "" {
		where = "directly to the System Admins"
	}
	return "Your report was sent " + where + ". You'll get a DM update when one acknowledges it.", nil
}

func (p *Plugin) bugReportAttachment(req *bugReportRequest, requester *model.User) *model.MessageAttachment {
	fields := []*model.MessageAttachmentField{
		{Title: "Reported by", Value: "@" + requester.Username, Short: true},
		{Title: "Type", Value: bugReportCategoryLabel(req.Category), Short: true},
		{Title: "About", Value: bugReportAboutLabel(req.AboutKind), Short: true},
	}
	if req.RelatedRequestID != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "Related request", Value: "`" + req.RelatedRequestID + "`", Short: true})
	}
	if req.AboutOptions != "" {
		fields = append(fields, &model.MessageAttachmentField{Title: "What they selected", Value: escapeMarkdown(req.AboutOptions), Short: false})
	}
	fields = append(fields, &model.MessageAttachmentField{Title: "Description", Value: escapeMarkdown(req.Description), Short: false})

	siteURL := "/plugins/" + manifest.Id
	return &model.MessageAttachment{
		Title:  "Request Hub " + bugReportCategoryLabel(req.Category) + " report",
		Text:   "System Admins: acknowledge once you've seen this. There is nothing to approve or deny.",
		Color:  colorPending,
		Fields: fields,
		Footer: requestIDFooter(req.ID),
		Actions: []*model.PostAction{{
			Id: "acknowledge", Name: "Acknowledge", Type: model.PostActionTypeButton, Style: "primary",
			Integration: &model.PostActionIntegration{
				URL:     siteURL + routeAcknowledgeBugReport,
				Context: map[string]any{actionContextRequestID: req.ID},
			},
		}},
	}
}

func (p *Plugin) storeBugReportRequest(req *bugReportRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to marshal bug report")
	}
	if err := p.putPendingRequestKV(kvBugReportRequestPrefix+req.ID, data); err != nil {
		return errors.Wrap(err, "failed to store bug report")
	}
	return nil
}

func (p *Plugin) loadBugReportRequest(id string) (*bugReportRequest, []byte, error) {
	data, appErr := p.API.KVGet(kvBugReportRequestPrefix + id)
	if appErr != nil {
		return nil, nil, errors.Wrap(appErr, "failed to load bug report")
	}
	if data == nil {
		return nil, nil, nil
	}
	var req bugReportRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, errors.Wrap(err, "failed to unmarshal bug report")
	}
	return &req, data, nil
}

// decideBugReportRequest handles /approve (as Acknowledge) and /deny (refused).
func (p *Plugin) decideBugReportRequest(actingUserID, requestID, clickedPostID string, approve bool) decisionResult {
	if !approve {
		return decisionResult{Reply: "Bug and feedback reports can't be denied. Use Acknowledge, or ask the reporter to withdraw."}
	}
	return p.decideAcknowledgeBugReport(actingUserID, requestID, clickedPostID)
}

// decideAcknowledgeBugReport closes a bug/feedback report for a System Admin.
func (p *Plugin) decideAcknowledgeBugReport(actingUserID, requestID, clickedPostID string) decisionResult {
	actingUser, appErr := p.API.GetUser(actingUserID)
	if appErr != nil {
		return decisionResult{Reply: "Could not verify your identity."}
	}
	if !actingUser.IsSystemAdmin() {
		return decisionResult{Reply: "Only System Admins can acknowledge bug and feedback reports."}
	}
	req, rawReq, err := p.loadBugReportRequest(requestID)
	if err != nil {
		p.API.LogError("failed to load bug report", "error", err.Error())
		return decisionResult{Reply: "Could not load that report."}
	}
	if req == nil {
		return decisionResult{Reply: alreadyHandledText}
	}
	if clickedPostID != "" {
		if msg := p.cardRefusal(clickedPostID, req.ApprovalPostID, req.ID); msg != "" {
			return decisionResult{Reply: msg}
		}
	}

	p.refreshTicketThread(ticketThread{
		RequestID:         req.ID,
		ApprovalPostID:    req.ApprovalPostID,
		ApprovalChannelID: req.ApprovalChannelID,
		DMRootPostID:      req.DMRootPostID,
		DMChannelID:       req.DMChannelID,
		Kind:              requestTypeBugReport,
	})
	cards := newDecisionCards(req.ID, clickedPostID, req.ApprovalPostID).
		withSubject(pendingKindBugReport, req.RequesterID, actingUser, req, false)
	requester, _ := p.API.GetUser(req.RequesterID)

	if days := p.getConfiguration().ExpiryDays(); requestExpired(req.CreatedAt, days, model.GetMillis()) {
		return p.expireRequest(cards, expiredRequest{
			kvKey: kvBugReportRequestPrefix + req.ID, raw: rawReq, kind: pendingKindBugReport,
			requestID: req.ID, requesterID: req.RequesterID, dmRootID: req.DMRootPostID, dmChannelID: req.DMChannelID, days: days,
			cardTitle: "Bug/Feedback Report Expired",
			cardText:  "Your report expired before a System Admin acknowledged it. Submit it again if you still need it seen.",
			audit: fmt.Sprintf("@%s closed the %s report from %s, which was not reviewed within %d days",
				actingUser.Username, strings.ToLower(bugReportCategoryLabel(req.Category)), requesterUsername(requester, req.RequesterID), days),
		})
	}
	if res, ok := p.claimRecord(kvBugReportRequestPrefix+req.ID, rawReq, cards, "bug report"); !ok {
		return res
	}
	p.releasePending(pendingKindBugReport, req.RequesterID, req.ID)

	note := fmt.Sprintf("@%s acknowledged the %s report from %s",
		actingUser.Username, strings.ToLower(bugReportCategoryLabel(req.Category)), requesterUsername(requester, req.RequesterID))
	p.decisionEvent(cards, eventAcknowledged, note, map[string]any{
		"acknowledged_by_id": actingUser.Id,
		"acknowledged_by":    actingUser.Username,
		"category":           req.Category,
		"about_kind":         req.AboutKind,
	})
	outcome := fmt.Sprintf("✅ Acknowledged by @%s", actingUser.Username)
	return p.finish(cards, "", outcome, ticketClose{
		RequesterID: req.RequesterID, DMRootPostID: req.DMRootPostID, DMChannelID: req.DMChannelID,
		Acknowledged: true, ApproverUsername: actingUser.Username,
		CardTitle:   "Report Acknowledged",
		CardText:    fmt.Sprintf("Acknowledged by @%s. Thanks for the report.", actingUser.Username),
		StatusLine:  outcome,
		ThreadReply: fmt.Sprintf("✅ **Acknowledged** by @%s.", actingUser.Username),
	})
}

// handleAcknowledgeBugReport handles the Acknowledge button on a bug report card.
func (p *Plugin) handleAcknowledgeBugReport(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireActionUserID(w, r)
	if !ok {
		return
	}
	var request model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: "Invalid action request. Please try again."})
		return
	}
	if request.PostId == "" {
		writeJSON(w, model.PostActionIntegrationResponse{EphemeralText: buttonNotOnCardText})
		return
	}
	requestID := p.actionRequestID(&request)
	writeDecision(w, p.decideAcknowledgeBugReport(userID, requestID, request.PostId))
}

// handleSubmitBugReportWebapp takes the Report a Bug / Feedback form.
func (p *Plugin) handleSubmitBugReportWebapp(w http.ResponseWriter, r *http.Request) {
	if !p.getConfiguration().RequestEnabled(requestTypeBugReport) {
		writeJSON(w, map[string]string{"error": bugReportRequestsDisabledMsg})
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var payload struct {
		Category         string `json:"category"`
		AboutKind        string `json:"about_kind"`
		AboutOptions     string `json:"about_options"`
		RelatedRequestID string `json:"related_request_id"`
		Description      string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	msg, err := p.submitBugReportRequest(userID, payload.Category, payload.AboutKind, payload.AboutOptions, payload.RelatedRequestID, payload.Description)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}

// handleMyRequests lists the caller's pending requests for the related-request picker.
func (p *Plugin) handleMyRequests(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	summaries, _, err := p.listOwnRequests(userID, 50)
	if err != nil {
		p.API.LogError("failed to list own requests", "user_id", userID, "error", err.Error())
		http.Error(w, "could not list your requests", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, map[string]any{
			"id":      s.ID,
			"kind":    s.Kind,
			"summary": s.Summary,
		})
	}
	writeJSON(w, out)
}

const (
	bugReportDialogCallbackID = "bug_report_dialog"
	fieldBugCategory          = "bug_category"
	fieldBugAbout             = "bug_about"
	fieldBugOptions           = "bug_options"
	fieldBugRelated           = "bug_related"
	fieldBugDescription       = "bug_description"
)

func (p *Plugin) openBugReportDialog(triggerID string) error {
	aboutOpts := []*model.PostActionOptions{
		{Text: "General / other", Value: bugReportAboutGeneral},
		{Text: "Channel", Value: pendingKindChannel},
		{Text: "Channel Admin", Value: pendingKindAdmin},
		{Text: "Team creation", Value: pendingKindTeam},
		{Text: "Team Admin", Value: pendingKindTeamAdmin},
		{Text: "Team member", Value: pendingKindTeamMember},
		{Text: "Guest invite", Value: pendingKindTeamGuest},
		{Text: "Bot account", Value: pendingKindBot},
		{Text: "Webhook", Value: pendingKindWebhook},
		{Text: "Bot transfer / retire", Value: pendingKindBotTransfer},
		{Text: "Webhook transfer / retire", Value: pendingKindWebhookTransfer},
		{Text: "Audit export", Value: pendingKindAuditExport},
	}
	dialog := model.Dialog{
		CallbackId:       bugReportDialogCallbackID,
		Title:            "Report a Bug or Feedback",
		IntroductionText: "Tell System Admins about a Request Hub problem or suggestion. This is not an approval ticket.",
		SubmitLabel:      "Submit report",
		Elements: []model.DialogElement{
			{
				DisplayName: "Report type",
				Name:        fieldBugCategory,
				Type:        "select",
				Options: []*model.PostActionOptions{
					{Text: "Bug", Value: bugReportCategoryBug},
					{Text: "Feedback", Value: bugReportCategoryFeedback},
				},
				Default: bugReportCategoryBug,
			},
			{
				DisplayName: "About which area",
				Name:        fieldBugAbout,
				Type:        "select",
				Options:     aboutOpts,
				Default:     bugReportAboutGeneral,
			},
			{
				DisplayName: "What you selected",
				Name:        fieldBugOptions,
				Type:        "textarea",
				Optional:    true,
				HelpText:    "Prefix, team, channel, bot name, or other options you chose on the form.",
				MaxLength:   maxDescriptionLen,
			},
			{
				DisplayName: "Related request ID",
				Name:        fieldBugRelated,
				Type:        "text",
				Optional:    true,
				HelpText:    "Optional. One of your Request Hub request IDs (from your ticket DM or /list). System Admins may link anyone's.",
				MaxLength:   26,
			},
			{
				DisplayName: "Description",
				Name:        fieldBugDescription,
				Type:        "textarea",
				Placeholder: "What went wrong, or what would you change?",
				MaxLength:   maxDescriptionLen,
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       "/plugins/" + manifest.Id + routeBugReportDialog,
		Dialog:    dialog,
	}); appErr != nil {
		return errors.Wrap(appErr, "failed to open bug report dialog")
	}
	return nil
}

func (p *Plugin) handleBugReportDialogSubmit(w http.ResponseWriter, r *http.Request) {
	if !p.getConfiguration().RequestEnabled(requestTypeBugReport) {
		writeJSON(w, model.SubmitDialogResponse{Error: bugReportRequestsDisabledMsg})
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var request model.SubmitDialogRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	submission := request.Submission
	str := func(name string) string {
		v, _ := submission[name].(string)
		return v
	}
	msg, err := p.submitBugReportRequest(userID, str(fieldBugCategory), str(fieldBugAbout), str(fieldBugOptions), str(fieldBugRelated), str(fieldBugDescription))
	if err != nil {
		writeJSON(w, model.SubmitDialogResponse{Error: err.Error()})
		return
	}
	writeJSON(w, model.SubmitDialogResponse{})
	_ = p.API.SendEphemeralPost(userID, &model.Post{ChannelId: request.ChannelId, Message: msg})
}

func (p *Plugin) executeBugReportCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeBugReport) {
		return ephemeralResponse(bugReportRequestsDisabledMsg), nil
	}
	if err := p.openBugReportDialog(args.TriggerId); err != nil {
		p.API.LogError("failed to open bug report dialog", "error", err.Error())
		return ephemeralResponse("Could not open the report form. Try again, or use Request Hub in the channel header."), nil
	}
	return &model.CommandResponse{}, nil
}
