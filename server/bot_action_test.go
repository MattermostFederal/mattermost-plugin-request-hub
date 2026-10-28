package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestHandleBotAction_UserGroupDualRoleFillsOnlySecurity verifies the full
// approve path: a System Admin who is also in security-approvers fills ONLY
// the security step on the first click. CreateBot must not run.
func TestHandleBotAction_UserGroupDualRoleFillsOnlySecurity(t *testing.T) {
	api := &plugintest.API{}
	// Log stubs only — do NOT use stubLogs: it registers a blanket
	// KVCompareAndSet Maybe that would swallow the CAS we need to assert on.
	for _, method := range []string{"LogDebug", "LogInfo", "LogWarn", "LogError"} {
		for arity := 1; arity <= 16; arity++ {
			args := make([]any, arity)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(method, args...).Maybe()
		}
	}
	defer api.AssertExpectations(t)

	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		AllowBotTokenRequests: true,
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
		SystemApproverGroup:   "system-approvers",
	})

	req := &botRequest{
		ID:                "bot-req-1",
		RequesterID:       "u_req",
		Username:          "svcbot",
		DisplayName:       "Service Bot",
		ApprovalPostID:    "post1",
		ApprovalChannelID: "bot-ch",
		DMRootPostID:      "dm-root",
		DMChannelID:       "dm1",
	}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	dual := &model.User{
		Id:       "dual",
		Username: "dualadmin",
		Roles:    "system_user system_admin",
	}
	secName := "security-approvers"

	api.On("GetUser", "dual").Return(dual, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer"}, nil)
	api.On("KVGet", kvBotRequestPrefix+"bot-req-1").Return(raw, nil)
	stubSyncedGroup(api, "security-approvers", "g-sec")
	api.On("GetGroupsForUser", "dual").Return([]*model.Group{{Id: "g-sec", Name: &secName}}, nil)

	// refreshTicketThread lookups
	api.On("KVSet", kvApprovalPostPrefix+"post1", mock.Anything).Return(nil)
	api.On("KVSet", kvDMRootPrefix+"dm-root", mock.Anything).Return(nil)

	// Partial approve: CAS re-store, not claim/delete. Capture written bytes.
	var stored []byte
	api.On("KVCompareAndSet", kvBotRequestPrefix+"bot-req-1", raw, mock.Anything).
		Run(func(args mock.Arguments) {
			stored = args.Get(2).([]byte)
		}).
		Return(true, nil).Once()

	// notifyTicketProgress updates the DM card
	api.On("GetPost", "dm-root").Return(&model.Post{
		Id: "dm-root", ChannelId: "dm1",
		Props: model.StringInterface{"attachments": []any{map[string]any{"title": "Pending"}}},
	}, nil)
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil)

	// No other card copies to redraw.
	api.On("KVGet", kvApprovalCardsPrefix+"bot-req-1").Return(nil, nil)
	// repaintedApprovalPost + stepStatusLine GetUser for dual
	api.On("GetPost", "post1").Return(&model.Post{Id: "post1", ChannelId: "bot-ch"}, nil)

	r := httptest.NewRequest(http.MethodPost, routeApproveBot, strings.NewReader(actionBody(t, "bot-req-1")))
	r.Header.Set(headerUserID, "dual")
	w := httptest.NewRecorder()
	p.handleBotAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Empty(t, resp.EphemeralText, "dual-role first click should succeed")
	require.NotNil(t, resp.Update)
	require.NotContains(t, resp.Update.Message, "Fully approved")
	require.Contains(t, resp.Update.Message, "needs review")

	api.AssertNotCalled(t, "CreateBot", mock.Anything)
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)

	require.NotEmpty(t, stored, "partial approval must CAS-write the pending record")
	var after botRequest
	require.NoError(t, json.Unmarshal(stored, &after))
	require.Equal(t, "dual", after.SecurityApproverID)
	require.Empty(t, after.SystemApproverID, "system step must remain open for a second approver")
	require.False(t, after.complete())
}

// TestPlanAndApply_UserGroupDualRoleEndToEnd chains canApproveStep → planApproval
// → applyTwoStepApproval the same way handleBotAction does.
func TestPlanAndApply_UserGroupDualRoleEndToEnd(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
		SystemApproverGroup:   "system-approvers",
	})

	secName := "security-approvers"
	stubSyncedGroup(api, "security-approvers", "g-sec")
	api.On("GetGroupsForUser", "dual").Return([]*model.Group{{Id: "g-sec", Name: &secName}}, nil)

	dual := &model.User{Id: "dual", Roles: "system_user system_admin"}
	canSec, err := p.canApproveStep(dual, stepSecurity)
	require.NoError(t, err)
	require.True(t, canSec, "security group member must qualify for security")
	canSys, err := p.canApproveStep(dual, stepSystem)
	require.NoError(t, err)
	require.True(t, canSys, "system admin must qualify for system")

	step, fill, reason := planApproval(twoStepState{}, canSec, canSys, dual.Id, "u_req")
	require.True(t, fill, reason)
	require.Equal(t, stepSecurity, step)

	state := p.applyTwoStepApproval(twoStepState{}, step, dual, 100)
	require.False(t, state.complete())
	require.Equal(t, "dual", state.SecurityApproverID)
	require.Empty(t, state.SystemApproverID)

	// Second distinct dual-role user completes system only.
	stubSyncedGroup(api, "security-approvers", "g-sec")
	api.On("GetGroupsForUser", "dual2").Return([]*model.Group{{Id: "g-sec", Name: &secName}}, nil)
	dual2 := &model.User{Id: "dual2", Roles: "system_user system_admin"}
	canSec2, err := p.canApproveStep(dual2, stepSecurity)
	require.NoError(t, err)
	canSys2, err := p.canApproveStep(dual2, stepSystem)
	require.NoError(t, err)
	step2, fill2, reason2 := planApproval(state, canSec2, canSys2, dual2.Id, "u_req")
	require.True(t, fill2, reason2)
	require.Equal(t, stepSystem, step2)
	final := p.applyTwoStepApproval(state, step2, dual2, 200)
	require.True(t, final.complete())
	require.Equal(t, "dual", final.SecurityApproverID)
	require.Equal(t, "dual2", final.SystemApproverID)
}
