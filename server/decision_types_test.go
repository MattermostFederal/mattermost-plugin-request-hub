package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newerTypeFixture is a plugin backed by fakeKV, with System Admin admin1,
// requester u_req (@reqer) and nominee n1 in team1 ("Ops"), and webhook
// channel ch1. Requests stored with storeDMFallback went to System Admin
// direct messages, recorded as post1 and post2.
func newerTypeFixture(t *testing.T) (*plugintest.API, *Plugin, map[string][]byte, map[string]*model.Post) {
	t.Helper()
	api := &plugintest.API{}
	stubLogLines(api)
	t.Cleanup(func() { api.AssertExpectations(t) })
	p := newTestPlugin(api)
	store := fakeKV(api)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil).Maybe()
	api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
	api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "n1"}, nil).Maybe()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Maybe()
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "alerts", TeamId: "team1", Type: model.ChannelTypeOpen}, nil).Maybe()
	api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil).Maybe()
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
	api.On("GetPost", "post1").Return(approvalCard("post1"), nil).Maybe()
	api.On("GetPost", "post2").Return(approvalCard("post2"), nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "created"}, nil).Maybe()
	return api, p, store, updatedPosts(api)
}

// storeDMFallback stores a request of kind under id, with its card copies
// in System Admin direct messages and a ticket DM.
func storeDMFallback(t *testing.T, store map[string][]byte, kind, id string, record any) {
	t.Helper()
	store[pendingKinds[kind].kvPrefix+id] = recordJSON(t, record)
	store[kvApprovalCardsPrefix+id] = recordJSON(t, []string{"post1", "post2"})
}

// newerTypeRecord returns a pending request of kind from u_req with the
// ticket DM, and whatever else each kind needs.
func newerTypeRecord(kind, id string, state twoStepState) any {
	const root, dm = "dmroot", "dm1"
	created := model.GetMillis()
	switch kind {
	case pendingKindTeam:
		return &teamCreationRequest{ID: id, RequesterID: "u_req", DisplayName: "Field Ops", Name: "field-ops", Type: model.TeamOpen, CreatedAt: created, DMRootPostID: root, DMChannelID: dm}
	case pendingKindTeamAdmin:
		return &teamAdminRequest{ID: id, RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"n1"}, CreatedAt: created, DMRootPostID: root, DMChannelID: dm}
	case pendingKindTeamMember:
		return &teamMemberRequest{ID: id, RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"n1"}, CreatedAt: created, DMRootPostID: root, DMChannelID: dm}
	case pendingKindTeamGuest:
		return &teamGuestRequest{ID: id, RequesterID: "u_req", TeamID: "team1", Email: "guest@example.com", CreatedAt: created, DMRootPostID: root, DMChannelID: dm}
	case pendingKindBot:
		return &botRequest{ID: id, RequesterID: "u_req", Username: "svcbot", CreatedAt: created, DMRootPostID: root, DMChannelID: dm, twoStepState: state}
	default:
		return &webhookRequest{ID: id, RequesterID: "u_req", WebhookType: webhookTypeOutgoing, ChannelID: "ch1", TeamID: "team1", DisplayName: "Relay", CallbackURL: "https://hooks.example.com/out", CreatedAt: created, DMRootPostID: root, DMChannelID: dm, twoStepState: state}
	}
}

func TestExecuteDecision_DenyClosesEveryCopyForEveryNewerType(t *testing.T) {
	for kind, want := range map[string]string{
		pendingKindTeam:       "❌ Denied by @admin for @reqer. Team **Field Ops** (`/field-ops`).",
		pendingKindTeamAdmin:  "❌ Denied by @admin for @reqer. Team Admin for @n1 in **Ops**.",
		pendingKindTeamMember: "❌ Denied by @admin for @reqer. Team member add for @n1 in **Ops**.",
		pendingKindTeamGuest:  "❌ Denied by @admin for @reqer. Guest invite for **Ops**.",
		pendingKindBot:        "❌ Denied by @admin for @reqer. Bot **@svcbot**.",
		pendingKindWebhook:    "❌ Denied by @admin for @reqer. outgoing webhook **Relay**.",
	} {
		t.Run(kind, func(t *testing.T) {
			_, p, store, updated := newerTypeFixture(t)
			id := model.NewId()
			storeDMFallback(t, store, kind, id, newerTypeRecord(kind, id, twoStepState{}))

			resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+id)

			require.Equal(t, want, resp.Text)
			for _, postID := range []string{"post1", "post2"} {
				require.Contains(t, updated, postID)
				require.Equal(t, want, updated[postID].Message)
				require.Empty(t, updated[postID].Attachments())
			}
			require.Contains(t, extractFirstAttachment(updated["dmroot"]).Title, "Denied")
			require.NotContains(t, store, pendingKinds[kind].kvPrefix+id)
			require.NotContains(t, store, kvApprovalCardsPrefix+id)
		})
	}
}

func TestExecuteDecision_TeamTypesRefuseOtherUsers(t *testing.T) {
	for kind, reply := range map[string]string{
		pendingKindTeam:       "You aren't allowed to approve or deny team creation requests.",
		pendingKindTeamAdmin:  "You aren't allowed to approve or deny Team Admin requests.",
		pendingKindTeamMember: "Only System Admins can approve or deny team member requests.",
		pendingKindTeamGuest:  "Only System Admins can approve or deny guest invite requests.",
	} {
		t.Run(kind, func(t *testing.T) {
			api, p, store, updated := newerTypeFixture(t)
			api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{TeamId: "team1", UserId: "u_req", Roles: "team_user"}, nil).Maybe()
			id := model.NewId()
			storeDMFallback(t, store, kind, id, newerTypeRecord(kind, id, twoStepState{}))

			resp := runCommandAs(p, "u_req", "/mattermost-request-hub approve "+id)

			require.Equal(t, reply, resp.Text)
			require.Contains(t, store, pendingKinds[kind].kvPrefix+id)
			require.Empty(t, updated)
		})
	}
}

func TestExecuteDecision_TeamMemberApproveAddsAndClosesEveryCopy(t *testing.T) {
	api, p, store, updated := newerTypeFixture(t)
	stubActiveTeamMember(api, "team1", "u_req")
	api.On("CreateTeamMember", "team1", "n1").Return(&model.TeamMember{}, nil)
	id := model.NewId()
	storeDMFallback(t, store, pendingKindTeamMember, id, newerTypeRecord(pendingKindTeamMember, id, twoStepState{}))

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+id)

	const want = "✅ Approved by @admin for @reqer. Added @n1 to **Ops**."
	require.Equal(t, want, resp.Text)
	require.Equal(t, want, updated["post1"].Message)
	require.Equal(t, want, updated["post2"].Message)
	require.Equal(t, "Team Member Request Approved", extractFirstAttachment(updated["dmroot"]).Title)
	require.NotContains(t, store, kvTeamMemberRequestPrefix+id)
}

func TestExecuteDecision_LostClaimOnANewerTypeAsksToRunAgain(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	id := model.NewId()
	raw := mustMarshal(t, newerTypeRecord(pendingKindTeam, id, twoStepState{}))
	stubSystemAdmin(api)
	// Not a channel or Channel Admin request.
	api.On("KVGet", kvRequestPrefix+id).Return(nil, nil)
	api.On("KVGet", kvAdminRequestPrefix+id).Return(nil, nil)
	api.On("KVGet", kvTeamRequestPrefix+id).Return(raw, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	api.On("KVCompareAndDelete", kvTeamRequestPrefix+id, raw).Return(false, nil)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub deny "+id)

	require.Equal(t, "This request was just updated. Please run the command again.", resp.Text)
}

func TestExecuteDecision_ExpiredNewerTypeClosesEveryCopy(t *testing.T) {
	_, p, store, updated := newerTypeFixture(t)
	config := *p.getConfiguration()
	config.expiryDays = 30
	p.setConfiguration(&config)
	id := model.NewId()
	record := newerTypeRecord(pendingKindTeamAdmin, id, twoStepState{}).(*teamAdminRequest)
	record.CreatedAt = expiredMillis()
	storeDMFallback(t, store, pendingKindTeamAdmin, id, record)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+id)

	require.Equal(t, expiredText, resp.Text)
	require.Equal(t, "⌛ Expired: not reviewed within 30 days.", updated["post1"].Message)
	require.Equal(t, "⌛ Expired: not reviewed within 30 days.", updated["post2"].Message)
	require.NotContains(t, store, kvTeamAdminRequestPrefix+id)
}

func TestExecuteDecision_TwoStepFirstApprovalRedrawsEveryCopy(t *testing.T) {
	for _, kind := range []string{pendingKindBot, pendingKindWebhook} {
		t.Run(kind, func(t *testing.T) {
			api, p, store, updated := newerTypeFixture(t)
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil).Maybe()
			id := model.NewId()
			storeDMFallback(t, store, kind, id, newerTypeRecord(kind, id, twoStepState{}))

			resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+id)

			require.Equal(t, "Approved the security step. The request now needs the other approval, from a different person.", resp.Text)
			for _, postID := range []string{"post1", "post2"} {
				require.Contains(t, updated, postID)
				att := extractFirstAttachment(updated[postID])
				require.NotNil(t, att)
				require.Equal(t, requestIDFooter(id), att.Footer)
				require.NotEmpty(t, att.Actions, "the request is still pending")
				require.Contains(t, fieldValues(att), "approved by @admin")
			}
			var state twoStepState
			require.NoError(t, json.Unmarshal(store[pendingKinds[kind].kvPrefix+id], &state))
			require.Equal(t, "admin1", state.SecurityApproverID)
			require.Empty(t, state.SystemApproverID)
		})
	}
}

func TestExecuteDecision_TwoStepRefusalIsAPlainReply(t *testing.T) {
	// The requester is a System Admin, so in both pools, and still can't
	// approve their own request. No card is painted from the command.
	_, p, store, updated := newerTypeFixture(t)
	id := model.NewId()
	record := newerTypeRecord(pendingKindBot, id, twoStepState{}).(*botRequest)
	record.RequesterID = "admin1"
	storeDMFallback(t, store, pendingKindBot, id, record)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+id)

	require.Equal(t, "⚠️ You created this request — you cannot approve it. Another security or system approver must review this ticket.", resp.Text)
	require.Empty(t, updated)
	require.Equal(t, recordJSON(t, record), store[kvBotRequestPrefix+id], "no step was filled")
}

func TestExecuteDecision_WebhookFinalApprovalClosesEveryCopy(t *testing.T) {
	api, p, store, updated := newerTypeFixture(t)
	stubSiteURL(api, "http://localhost:8065")
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) {
		return "Your outgoing webhook **Relay** is ready.", "hook1", nil
	}
	id := model.NewId()
	storeDMFallback(t, store, pendingKindWebhook, id, newerTypeRecord(pendingKindWebhook, id, securityStepDone()))

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+id)

	const want = "✅ Fully approved (security + system) for @reqer. outgoing webhook **Relay** created."
	require.Equal(t, want, resp.Text)
	require.Equal(t, want, updated["post1"].Message)
	require.Equal(t, want, updated["post2"].Message)
	require.Equal(t, "Webhook Request Approved", extractFirstAttachment(updated["dmroot"]).Title)
	require.NotContains(t, store, kvWebhookRequestPrefix+id)
}

func TestHandleTeamAction_ClickClosesTheOtherSystemAdminCopy(t *testing.T) {
	_, p, store, updated := newerTypeFixture(t)
	id := model.NewId()
	storeDMFallback(t, store, pendingKindTeam, id, newerTypeRecord(pendingKindTeam, id, twoStepState{}))
	body, err := json.Marshal(model.PostActionIntegrationRequest{PostId: "post1", Context: map[string]any{actionContextRequestID: id}})
	require.NoError(t, err)

	resp := clickAction(t, p, teamActionHandler, "admin1", string(body), false)

	const want = "❌ Denied by @admin for @reqer. Team **Field Ops** (`/field-ops`)."
	require.Equal(t, want, resp.Update.Message)
	require.NotContains(t, updated, "post1", "the click's response updates the clicked copy")
	require.Equal(t, want, updated["post2"].Message)
}

func TestTwoStepRetryText(t *testing.T) {
	require.Contains(t, retryText(true), "click **Approve** again")
	require.Contains(t, retryText(false), "run the approve command again")
}

func TestExecuteList_Approvals(t *testing.T) {
	api, p, store, _ := newerTypeFixture(t)
	config := *p.getConfiguration()
	config.AllowTeamGuestRequests = false
	p.setConfiguration(&config)
	stubSyncedGroup(api, "security-approvers", "g-sec")

	ids := map[string]string{}
	add := func(name, kind string, record any) {
		id, _, createdAt := record.(storedRequest).pendingFields()
		*createdAt = int64(len(ids) + 1) // oldest first, in the order added
		ids[name] = id
		store[pendingKinds[kind].kvPrefix+id] = recordJSON(t, record)
	}
	add("team member", pendingKindTeamMember, newerTypeRecord(pendingKindTeamMember, model.NewId(), twoStepState{}))
	own := newerTypeRecord(pendingKindTeam, model.NewId(), twoStepState{}).(*teamCreationRequest)
	own.RequesterID = "admin1"
	add("own", pendingKindTeam, own)
	add("guest, turned off", pendingKindTeamGuest, newerTypeRecord(pendingKindTeamGuest, model.NewId(), twoStepState{}))
	add("bot, my step done", pendingKindBot, newerTypeRecord(pendingKindBot, model.NewId(), twoStepState{SecurityApproverID: "admin1", SecurityApprovedAt: 1}))
	add("webhook", pendingKindWebhook, newerTypeRecord(pendingKindWebhook, model.NewId(), twoStepState{}))

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub list approvals")

	lines := strings.Split(resp.Text, "\n")
	require.Equal(t, "Requests you can approve:", lines[0])
	require.Len(t, lines, 3, resp.Text)
	require.Contains(t, lines[1], "Add @n1 to Ops")
	require.Contains(t, lines[1], ids["team member"])
	require.Contains(t, lines[2], "Outgoing webhook **Relay** in ~alerts, awaiting security approval")
	require.Contains(t, lines[2], ids["webhook"])
}

func TestExecuteList_ApprovalsNoneAndBadArgument(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		_, p, _, _ := newerTypeFixture(t)

		resp := runCommandAs(p, "admin1", "/mattermost-request-hub list approvals")

		require.Equal(t, "There are no pending requests you can approve.", resp.Text)
	})
	t.Run("bad argument", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)

		resp := runCommandAs(p, "admin1", "/mattermost-request-hub list everything")

		require.Contains(t, resp.Text, "`/mattermost-request-hub list approvals`")
	})
}

func TestListApprovableRequests_CapsAndCounts(t *testing.T) {
	_, p, store, _ := newerTypeFixture(t)
	for i := range 3 {
		id := model.NewId()
		record := newerTypeRecord(pendingKindTeam, id, twoStepState{}).(*teamCreationRequest)
		record.CreatedAt = int64(i + 1)
		store[kvTeamRequestPrefix+id] = recordJSON(t, record)
	}

	got, more, err := p.listApprovableRequests("admin1", 2)

	require.NoError(t, err)
	require.Equal(t, 1, more)
	require.Equal(t, []int64{1, 2}, []int64{got[0].CreatedAt, got[1].CreatedAt}, "the oldest two")
}

func TestListApprovableRequests_Failures(t *testing.T) {
	t.Run("acting user", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetUser", "admin1").Return(nil, testAppErr("gone"))

		_, _, err := p.listApprovableRequests("admin1", 10)

		require.Error(t, err)
	})
	t.Run("key listing", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubSystemAdmin(api)
		api.On("KVList", 0, 200).Return(nil, testAppErr("kv down"))

		_, _, err := p.listApprovableRequests("admin1", 10)

		require.ErrorContains(t, err, "failed to list stored requests")
	})
	t.Run("unreadable record is skipped", func(t *testing.T) {
		_, p, store, _ := newerTypeFixture(t)
		store[kvTeamRequestPrefix+model.NewId()] = []byte("{not json")

		got, _, err := p.listApprovableRequests("admin1", 10)

		require.NoError(t, err)
		require.Empty(t, got)
	})
}

func TestRequestStatus_NewerTypes(t *testing.T) {
	cases := map[string]string{
		pendingKindTeam:       "Team **Field Ops** (`/field-ops`)",
		pendingKindTeamAdmin:  "Team Admin for @n1 in Ops",
		pendingKindTeamMember: "Add @n1 to Ops",
		pendingKindTeamGuest:  "Guest invite for **" + escapeMarkdown("guest@example.com") + "** to Ops",
		pendingKindBot:        "Bot **@svcbot**, awaiting security approval",
		pendingKindWebhook:    "Outgoing webhook **Relay** in ~alerts, awaiting security approval",
	}
	for kind, summary := range cases {
		t.Run(kind, func(t *testing.T) {
			api, p, store, _ := newerTypeFixture(t)
			id := model.NewId()
			store[pendingKinds[kind].kvPrefix+id] = recordJSON(t, newerTypeRecord(kind, id, twoStepState{}))

			for _, viewer := range []string{"u_req", "admin1"} {
				got, err := p.requestStatus(viewer, id)
				require.NoError(t, err, viewer)
				require.Equal(t, kind, got.Kind)
				require.Equal(t, summary, got.Summary)
			}

			// Someone who is neither the requester nor an approver.
			api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Roles: "system_user"}, nil)
			api.On("GetTeamMember", mock.Anything, "u_other").Return(nil, testAppErr("not a member")).Maybe()
			stubSyncedGroup(api, "security-approvers", "g-sec")
			api.On("GetGroupsForUser", "u_other").Return([]*model.Group{}, nil).Maybe()
			_, err := p.requestStatus("u_other", id)
			require.ErrorIs(t, err, errNoVisibleRequest)
		})
	}
}

func TestSummarize_SystemStepAwaited(t *testing.T) {
	_, p, _, _ := newerTypeFixture(t)

	got := p.summarize(p.getConfiguration(), pendingKindBot, newerTypeRecord(pendingKindBot, "r1", securityStepDone()).(storedRequest))

	require.Equal(t, "Bot **@svcbot**, awaiting system approval", got.Summary)
}

func TestNewerTypeCardsShowTheRequestID(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetConfig").Return(&model.Config{}).Maybe()
	api.On("GetUser", mock.Anything).Return(&model.User{Username: "x"}, nil).Maybe()
	api.On("GetChannel", mock.Anything).Return(&model.Channel{Name: "x"}, nil).Maybe()
	requester := regularUser()
	team := &model.Team{Id: "team1", DisplayName: "Ops"}
	channel := &model.Channel{Id: "ch1", Name: "alerts"}

	for name, att := range map[string]*model.MessageAttachment{
		"team":        p.teamCreationApprovalAttachment(&teamCreationRequest{ID: "r1"}, requester),
		"team admin":  p.teamAdminApprovalAttachment(&teamAdminRequest{ID: "r1"}, requester, team),
		"team member": p.teamMemberApprovalAttachment(&teamMemberRequest{ID: "r1"}, requester, team),
		"team guest":  p.teamGuestApprovalAttachment(&teamGuestRequest{ID: "r1"}, requester, team),
		"bot":         p.botApprovalAttachment(&botRequest{ID: "r1"}, requester),
		"webhook":     p.webhookApprovalAttachment(&webhookRequest{ID: "r1"}, requester, channel),
	} {
		require.Equal(t, requestIDFooter("r1"), att.Footer, name)
	}
}

func TestListRequestKeys_Pages(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	page := make([]string, 200)
	for i := range page {
		page[i] = "pending_channel_u" + model.NewId()
	}
	page[0] = kvTeamRequestPrefix + "a"
	api.On("KVList", 0, 200).Return(page, nil)
	api.On("KVList", 1, 200).Return([]string{kvBotRequestPrefix + "b", "unrelated"}, nil)

	keys, err := p.listRequestKeys()

	require.NoError(t, err)
	require.Equal(t, []string{kvTeamRequestPrefix + "a", kvBotRequestPrefix + "b"}, keys)
}

// fieldValues joins an attachment's field values.
func fieldValues(att *model.MessageAttachment) string {
	values := make([]string, 0, len(att.Fields))
	for _, f := range att.Fields {
		values = append(values, f.Value.(string))
	}
	return strings.Join(values, "\n")
}

func TestRepaintCards_SkipsForeignPostsAndLogsFailures(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	store := fakeKV(api)
	store[kvBotRequestPrefix+"r1"] = []byte("{}")
	store[kvApprovalCardsPrefix+"r1"] = recordJSON(t, []string{"mine", "theirs", "gone", "closed"})
	api.On("GetPost", "mine").Return(approvalCard("mine"), nil)
	api.On("GetPost", "theirs").Return(&model.Post{Id: "theirs", UserId: "someone"}, nil)
	api.On("GetPost", "gone").Return(nil, testAppErr("gone"))
	api.On("GetPost", "closed").Return(&model.Post{Id: "closed", UserId: "bot-user-id", Message: "✅ Approved"}, nil)
	api.On("UpdatePost", mock.Anything).Return(nil, testAppErr("db down")).Once()

	res := p.repaintCards(newDecisionCards("r1", "", ""), kvBotRequestPrefix+"r1", "reply", "header", &model.MessageAttachment{Title: "x"})

	require.Equal(t, "reply", res.Reply)
	require.Nil(t, res.Update)
	api.AssertNumberOfCalls(t, "UpdatePost", 1)
}

func TestOtherCardIDs_UnreadableRecordStillHasTheStoredCard(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	api.On("KVGet", kvApprovalCardsPrefix+"r1").Return([]byte("{not json"), nil)

	require.Equal(t, []string{"card1"}, p.otherCardIDs(newDecisionCards("r1", "", "card1")))
}

func TestListApprovableRequests_LoadFailure(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	stubSystemAdmin(api)
	api.On("KVList", 0, 200).Return([]string{kvTeamRequestPrefix + "r1"}, nil)
	api.On("KVGet", kvTeamRequestPrefix+"r1").Return(nil, testAppErr("kv down"))

	_, _, err := p.listApprovableRequests("admin1", 10)

	require.ErrorContains(t, err, "failed to load request")
}

func TestCapitalize(t *testing.T) {
	require.Equal(t, "", capitalize(""))
	require.Equal(t, "Incoming", capitalize("incoming"))
}

func TestExecuteDecision_FailedFinalApprovalKeepsTheRequestAndRedrawsEveryCopy(t *testing.T) {
	api, p, store, updated := newerTypeFixture(t)
	stubSiteURL(api, "http://localhost:8065")
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) { return "", "", testAppErr("hooks are off") }
	api.On("GetUser", "sec1").Return(&model.User{Id: "sec1", Username: "sec"}, nil)
	id := model.NewId()
	storeDMFallback(t, store, pendingKindWebhook, id, newerTypeRecord(pendingKindWebhook, id, securityStepDone()))

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+id)

	require.Contains(t, resp.Text, "run the approve command again to retry, or deny it")
	for _, postID := range []string{"post1", "post2"} {
		att := extractFirstAttachment(updated[postID])
		require.NotNil(t, att, postID)
		require.Equal(t, requestIDFooter(id), att.Footer)
		require.NotEmpty(t, att.Actions, "the request is still pending")
		require.Contains(t, fieldValues(att), "click **Approve** again", "the cards are for clicking")
	}
	require.Contains(t, store, kvWebhookRequestPrefix+id)
	require.Contains(t, store, kvApprovalCardsPrefix+id)
}

func TestRepaintCards_LeavesTheCardsOfADecidedRequest(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	fakeKV(api) // the request is no longer stored

	res := p.repaintCards(newDecisionCards("r1", "post1", "post1"), kvBotRequestPrefix+"r1", "", "header", &model.MessageAttachment{})

	require.Nil(t, res.Update)
	api.AssertNotCalled(t, "UpdatePost", mock.Anything)
}

func TestExecuteDecision_NewerTypeRefusals(t *testing.T) {
	t.Run("turned off: approve is refused, deny still works", func(t *testing.T) {
		_, p, store, _ := newerTypeFixture(t)
		config := *p.getConfiguration()
		config.AllowTeamRequests = false
		p.setConfiguration(&config)
		id := model.NewId()
		storeDMFallback(t, store, pendingKindTeam, id, newerTypeRecord(pendingKindTeam, id, twoStepState{}))

		require.Equal(t, requestTypeOffText, runCommandAs(p, "admin1", "/mattermost-request-hub approve "+id).Text)
		require.Contains(t, store, kvTeamRequestPrefix+id)
		require.Contains(t, runCommandAs(p, "admin1", "/mattermost-request-hub deny "+id).Text, "❌ Denied by @admin")
		require.NotContains(t, store, kvTeamRequestPrefix+id)
	})
	t.Run("bot deny by someone in neither pool", func(t *testing.T) {
		api, p, store, updated := newerTypeFixture(t)
		p.setConfiguration(securityGroupConfig(api))
		api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Username: "other", Roles: "system_user"}, nil)
		api.On("GetGroupsForUser", "u_other").Return([]*model.Group{}, nil)
		id := model.NewId()
		storeDMFallback(t, store, pendingKindBot, id, newerTypeRecord(pendingKindBot, id, twoStepState{}))

		resp := runCommandAs(p, "u_other", "/mattermost-request-hub deny "+id)

		require.Equal(t, "You don't have permission to deny this request.", resp.Text)
		require.Contains(t, store, kvBotRequestPrefix+id)
		require.Empty(t, updated)
	})
}

func TestExecuteList_ApprovalsForASecurityApprover(t *testing.T) {
	// sec1 is in the security group and not a System Admin: only the bot
	// request waiting for security is theirs to approve.
	api, p, store, _ := newerTypeFixture(t)
	p.setConfiguration(securityGroupConfig(api))
	secName := "security-approvers"
	api.On("GetUser", "sec1").Return(&model.User{Id: "sec1", Username: "sec", Roles: "system_user"}, nil)
	api.On("GetGroupsForUser", "sec1").Return([]*model.Group{{Id: "g-sec", Name: &secName}}, nil)
	waiting := model.NewId()
	store[kvBotRequestPrefix+waiting] = recordJSON(t, newerTypeRecord(pendingKindBot, waiting, twoStepState{}))
	systemNext := model.NewId()
	store[kvBotRequestPrefix+systemNext] = recordJSON(t, newerTypeRecord(pendingKindBot, systemNext, twoStepState{SecurityApproverID: "other", SecurityApprovedAt: 1}))
	team := model.NewId()
	store[kvTeamRequestPrefix+team] = recordJSON(t, newerTypeRecord(pendingKindTeam, team, twoStepState{}))

	resp := runCommandAs(p, "sec1", "/mattermost-request-hub list approvals")

	require.Contains(t, resp.Text, waiting)
	require.NotContains(t, resp.Text, systemNext)
	require.NotContains(t, resp.Text, team)
}

// securityGroupConfig puts the security step in the security-approvers
// group, with every request type on.
func securityGroupConfig(api *plugintest.API) *configuration {
	stubSyncedGroup(api, "security-approvers", "g-sec")
	return &configuration{
		AllowTeamRequests: true, AllowTeamAdminRequests: true, AllowTeamMemberRequests: true, AllowTeamGuestRequests: true,
		AllowBotTokenRequests: true, AllowWebhookRequests: true,
		TwoStepApproverMode: twoStepModeUserGroup, SecurityApproverGroup: "security-approvers",
	}
}
