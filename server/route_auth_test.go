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

// actionCasesNamed returns the actionHandlerCases with the given names.
func actionCasesNamed(names ...string) []actionHandlerCase {
	var out []actionHandlerCase
	for _, tc := range actionHandlerCases() {
		for _, name := range names {
			if tc.name == name {
				out = append(out, tc)
			}
		}
	}
	return out
}

// pendingRecord marshals tc's pending request on card "post1" with the
// given requester.
func pendingRecord(t *testing.T, tc actionHandlerCase, requesterID string) []byte {
	t.Helper()
	raw, err := json.Marshal(tc.record("post1"))
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	fields["requester_id"] = requesterID
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	return raw
}

func TestTeamTypeActions_OnlySystemAdminsMayDecide(t *testing.T) {
	for _, tc := range actionCasesNamed("team member", "team guest") {
		for _, approve := range []bool{true, false} {
			t.Run(tc.name+"/"+decisionName(approve), func(t *testing.T) {
				api := &plugintest.API{}
				stubLogLevels(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)
				api.On("GetUser", "u_lead").Return(&model.User{Id: "u_lead", Username: "lead", Roles: "system_user team_admin"}, nil)

				resp := clickAction(t, p, tc.handler, "u_lead", actionBody(t, "r1"), approve)

				require.Contains(t, resp.EphemeralText, "Only System Admins can approve or deny")
				// Refused before the request is read: the mock has no KVGet.
				require.Nil(t, resp.Update)
			})
		}
	}
}

// Team creation requests are for System Admins, and for approval-team Team
// Admins only when AllowTeamAdminTeamApprovers is on.
func TestTeamAction_TeamAdminRefusedWhenNotAllowed(t *testing.T) {
	for _, tc := range actionCasesNamed("team") {
		for _, approve := range []bool{true, false} {
			t.Run(decisionName(approve), func(t *testing.T) {
				api := &plugintest.API{}
				stubLogLevels(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)
				api.On("GetUser", "u_lead").Return(&model.User{Id: "u_lead", Username: "lead", Roles: "system_user"}, nil)

				resp := clickAction(t, p, tc.handler, "u_lead", actionBody(t, "r1"), approve)

				require.Contains(t, resp.EphemeralText, "aren't allowed to approve or deny team creation requests")
				// Refused before the request is read: the mock has no KVGet.
				require.Nil(t, resp.Update)
			})
		}
	}
}

// Team Admin requests are for System Admins and the request's own team's
// Team Admins.
func TestTeamAdminAction_OtherTeamsTeamAdminRefused(t *testing.T) {
	for _, tc := range actionCasesNamed("team admin") {
		for _, approve := range []bool{true, false} {
			t.Run(decisionName(approve), func(t *testing.T) {
				api := &plugintest.API{}
				stubLogLevels(api)
				defer api.AssertExpectations(t)
				p := newTestPlugin(api)
				api.On("GetUser", "u_lead").Return(&model.User{Id: "u_lead", Username: "lead", Roles: "system_user"}, nil)
				api.On("KVGet", kvTeamAdminRequestPrefix+"r1").Return(pendingRecord(t, tc, "u_req"), nil)
				api.On("GetTeamMember", "team1", "u_lead").Return(nil, testAppErr("not a member"))

				resp := clickAction(t, p, tc.handler, "u_lead", actionBody(t, "r1"), approve)

				require.Contains(t, resp.EphemeralText, "the team's Team Admins")
				require.Nil(t, resp.Update)
				api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
			})
		}
	}
}

// System Admins may approve their own channel and Channel Admin requests:
// the click gets past the own-request check to the claim.
func TestActionHandlers_SystemAdminMayApproveOwnRequest(t *testing.T) {
	for _, tc := range actionCasesNamed("channel", "channel admin") {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			stubPendingLists(api)
			stubActiveTeamMember(api, "team1", "admin1")
			p := newTestPlugin(api)
			p.setConfiguration(approvableConfig())
			stubSystemAdmin(api)
			raw := pendingRecord(t, tc, "admin1")
			if tc.name == "channel" {
				// The name must still match a configured prefix at approval.
				var fields map[string]any
				require.NoError(t, json.Unmarshal(raw, &fields))
				fields["name"] = "team-x"
				raw = mustMarshal(t, fields)
			}
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
			api.On("GetTeam", mock.Anything).Return(&model.Team{Id: "team1", Name: "ops"}, nil).Maybe()
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1"}, nil).Maybe()
			api.On("KVCompareAndDelete", tc.kvPrefix+"r1", raw).Return(false, testAppErr("kv down"))

			resp := clickAction(t, p, tc.handler, "admin1", actionBody(t, "r1"), true)

			require.Equal(t, "Could not process that request.", resp.EphemeralText)
			api.AssertCalled(t, "KVCompareAndDelete", tc.kvPrefix+"r1", raw)
		})
	}
}

// stubTwoStepGroups puts the plugin in group mode and makes userID a member
// of the named groups only.
func stubTwoStepGroups(api *plugintest.API, p *Plugin, userID string, groups ...string) {
	p.setConfiguration(&configuration{
		AllowBotTokenRequests: true,
		AllowWebhookRequests:  true,
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "security-approvers",
		SystemApproverGroup:   "system-approvers",
	})
	stubSyncedGroup(api, "security-approvers", "g-sec")
	stubSyncedGroup(api, "system-approvers", "g-sys")
	ids := map[string]string{"security-approvers": "g-sec", "system-approvers": "g-sys"}
	var member []*model.Group
	for _, g := range groups {
		name := g
		member = append(member, &model.Group{Id: ids[g], Name: &name})
	}
	api.On("GetGroupsForUser", userID).Return(member, nil)
}

// stubBlockedCard lets respondTwoStepApproveBlocked repaint "post1" and post
// its channel notice.
func stubBlockedCard(api *plugintest.API) {
	api.On("GetPost", "post1").Return(&model.Post{Id: "post1", ChannelId: "appr"}, nil).Maybe()
	api.On("SendEphemeralPost", mock.Anything, mock.Anything).Return(&model.Post{}).Maybe()
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1"}, nil).Maybe()
	api.On("GetTeam", mock.Anything).Return(&model.Team{Id: "team1", Name: "ops"}, nil).Maybe()
}

// failWebhookCreation makes any webhook creation fail the test.
func failWebhookCreation(t *testing.T, p *Plugin) {
	p.createWebhookFn = func(*webhookRequest, string) (string, string, error) {
		t.Fatal("webhook created")
		return "", "", nil
	}
}

// requireNoTwoStepProgress checks a refused click left the request as it was.
func requireNoTwoStepProgress(t *testing.T, api *plugintest.API, kvPrefix string) {
	t.Helper()
	api.AssertNotCalled(t, "KVCompareAndSet", kvPrefix+"r1", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "CreateBot", mock.Anything)
	api.AssertNotCalled(t, "CreateSession", mock.Anything)
}

func TestTwoStepActions_UserInNeitherPoolRefused(t *testing.T) {
	for _, tc := range actionCasesNamed("bot", "webhook") {
		for _, approve := range []bool{true, false} {
			t.Run(tc.name+"/"+decisionName(approve), func(t *testing.T) {
				api := &plugintest.API{}
				stubLogs(api)
				p := newTestPlugin(api)
				stubTwoStepGroups(api, p, "u_x")
				failWebhookCreation(t, p)
				stubBlockedCard(api)
				api.On("GetUser", "u_x").Return(&model.User{Id: "u_x", Username: "x", Roles: "system_user"}, nil)
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
				api.On("KVGet", tc.kvPrefix+"r1").Return(pendingRecord(t, tc, "u_req"), nil)

				resp := clickAction(t, p, tc.handler, "u_x", actionBody(t, "r1"), approve)

				if approve {
					require.Contains(t, resp.EphemeralText, "You don't have permission to approve this request.")
				} else {
					require.Equal(t, "You don't have permission to deny this request.", resp.EphemeralText)
				}
				requireNoTwoStepProgress(t, api, tc.kvPrefix)
			})
		}
	}
}

// The click context only names the request. Anything else in it is ignored
// and the stored request decides what gets created.
func TestHandleAction_ClickContextOnlySelectsTheRequest(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	stubActiveTeamMember(api, "team1", "u_req")
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "team-x"}, nil)
	api.On("AddChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_req", channelAdminRoleString).Return(&model.ChannelMember{}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	body, err := json.Marshal(model.PostActionIntegrationRequest{
		PostId: "post1",
		Context: map[string]any{
			actionContextRequestID: "req1",
			"requester_id":         "u_other",
			"team_id":              "team-other",
			"name":                 "other-name",
			"channel_type":         "P",
		},
	})
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(string(body)))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.ServeHTTP(nil, w, r)

	require.NotNil(t, actionResponse(t, w).Update)
	api.AssertCalled(t, "CreateChannel", mock.MatchedBy(func(ch *model.Channel) bool {
		return ch.TeamId == "team1" && ch.Name == "team-x" && ch.Type == model.ChannelTypeOpen
	}))
	api.AssertCalled(t, "AddChannelMember", "ch1", "u_req")
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, "u_other")
}

func TestSubmitRoutes_TurnedOffTypeRefusesBeforeReadingTheBody(t *testing.T) {
	disabled := map[string]string{
		routeDialog:                      channelRequestsDisabledMsg,
		routeCreate:                      channelRequestsDisabledMsg,
		routeRequestAdmin:                channelAdminRequestsDisabledMsg,
		routeChannelAdminDialog:          channelAdminRequestsDisabledMsg,
		routeTeamAdminDialog:             teamAdminRequestsDisabledMsg,
		routeTeamDialog:                  teamRequestsDisabledMsg,
		routeTeamMemberDialog:            teamMemberRequestsDisabledMsg,
		routeTeamGuestDialog:             teamGuestRequestsDisabledMsg,
		routeBotDialog:                   botRequestsDisabledMsg,
		routeIncomingWebhookDialog:       webhookRequestsDisabledMsg,
		routeOutgoingWebhookDialog:       webhookRequestsDisabledMsg,
		routeSubmitTeamCreationWebapp:    teamRequestsDisabledMsg,
		routeSubmitTeamAdminWebapp:       teamAdminRequestsDisabledMsg,
		routeSubmitTeamMemberWebapp:      teamMemberRequestsDisabledMsg,
		routeSubmitTeamGuestWebapp:       teamGuestRequestsDisabledMsg,
		routeSubmitBotWebapp:             botRequestsDisabledMsg,
		routeSubmitIncomingWebhookWebapp: webhookRequestsDisabledMsg,
		routeSubmitOutgoingWebhookWebapp: webhookRequestsDisabledMsg,
	}
	require.Len(t, disabled, len(submitRoutes))
	for _, route := range submitRoutes {
		t.Run(route, func(t *testing.T) {
			// The mock has no expectations: nothing is looked up or stored.
			api := &plugintest.API{}
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{})

			r := httptest.NewRequest(http.MethodPost, route, strings.NewReader("not json"))
			r.Header.Set(headerUserID, "u1")
			w := httptest.NewRecorder()
			p.ServeHTTP(nil, w, r)

			require.Equal(t, http.StatusOK, w.Code)
			var resp map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			require.Equal(t, disabled[route], resp["error"])
		})
	}
}

func decisionName(approve bool) string {
	if approve {
		return "approve"
	}
	return "deny"
}
