package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const headerUserID = "Mattermost-User-Id"

func TestHandleUserAutocomplete_MissingHeaderUnauthorized(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=a&team_id=t1", nil)
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	// No API work without an authenticated caller.
	api.AssertNotCalled(t, "SearchUsers", mock.Anything)
}

func TestHandleUserAutocomplete_NonMemberGetsEmpty(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	// Caller is not a member of the requested team.
	api.On("GetTeamMember", "t1", "u1").Return(nil, testAppErr("not a member"))

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=alice&team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, "[]", strings.TrimSpace(w.Body.String()))
	// The user directory is never searched for a non-member — no enumeration.
	api.AssertNotCalled(t, "SearchUsers", mock.Anything)
}

func TestHandleUserAutocomplete_TeamScopedHappyPath(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("GetConfig").Return(&model.Config{})
	// The search MUST be scoped to the requested team — assert the TeamId
	// (and term) reach SearchUsers, so team-scoping can't regress silently.
	api.On("SearchUsers", mock.MatchedBy(func(s *model.UserSearch) bool {
		return s.TeamId == "t1" && s.Term == "alice"
	})).Return([]*model.User{{Id: "id_alice", Username: "alice", FirstName: "Alice", LastName: "Smith"}}, nil)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=alice&team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1)
	require.Equal(t, "alice", out[0]["username"])
	// ShowFullName is unset (off): real names are withheld.
	require.NotContains(t, out[0], "first_name")
	require.NotContains(t, out[0], "last_name")
}

func TestHandleUserAutocomplete_ShowFullNameReturnsNames(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	cfg := &model.Config{}
	cfg.SetDefaults()
	*cfg.PrivacySettings.ShowFullName = true
	api.On("GetConfig").Return(cfg)
	api.On("SearchUsers", mock.Anything).Return([]*model.User{{Id: "id_alice", Username: "alice", FirstName: "Alice", LastName: "Smith"}}, nil)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=alice&team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1)
	require.Equal(t, "Alice", out[0]["first_name"])
}

func TestHandleUserAutocomplete_GuestGetsEmpty(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t1", "g1").Return(&model.TeamMember{TeamId: "t1", UserId: "g1"}, nil)
	api.On("GetUser", "g1").Return(&model.User{Id: "g1", Roles: model.SystemGuestRoleId}, nil)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=a&team_id=t1", nil)
	req.Header.Set(headerUserID, "g1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	require.JSONEq(t, "[]", strings.TrimSpace(w.Body.String()))
	api.AssertNotCalled(t, "SearchUsers", mock.Anything)
}

func TestServeHTTP_RejectsOversizedBody(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	huge := `{"members":["` + strings.Repeat("a", maxRequestBodyBytes) + `"]}`
	r := httptest.NewRequest(http.MethodPost, routeCreate, strings.NewReader(huge))
	r.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.ServeHTTP(nil, w, r)

	require.Equal(t, http.StatusBadRequest, w.Code)
	api.AssertNotCalled(t, "GetUserByUsername", mock.Anything)
}

func TestHandleUserAutocomplete_SoftDeletedMemberGetsEmpty(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	// Former member: GetTeamMember returns a row, but DeleteAt != 0.
	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1", DeleteAt: 123}, nil)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=alice&team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, "[]", strings.TrimSpace(w.Body.String()))
	// A former member must not be able to enumerate the team's users.
	api.AssertNotCalled(t, "SearchUsers", mock.Anything)
}

func TestHandleUserAutocomplete_RequiresTeamScope(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=alice", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, "[]", strings.TrimSpace(w.Body.String()))
	api.AssertNotCalled(t, "SearchUsers", mock.Anything)
}

func TestHandleUserAutocomplete_NotInTeamHappyPath(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("GetConfig").Return(&model.Config{})
	api.On("SearchUsers", mock.MatchedBy(func(s *model.UserSearch) bool {
		return s.NotInTeamId == "t1" && s.TeamId == "" && s.Term == "bob" && s.Limit == 50
	})).Return([]*model.User{{Id: "id_bob", Username: "bob"}}, nil)
	api.On("HasPermissionTo", "u1", model.PermissionViewMembers).Return(true)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=bob&not_in_team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1)
	require.Equal(t, "bob", out[0]["username"])
}

func TestHandleUserAutocomplete_NotInTeamKeepsTeammatesWithoutViewMembers(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("GetConfig").Return(&model.Config{})
	api.On("SearchUsers", mock.MatchedBy(func(s *model.UserSearch) bool { return s.Limit == 100 })).Return([]*model.User{
		{Id: "id_bob", Username: "bob"},
		{Id: "id_eve", Username: "eve"},
	}, nil)
	api.On("HasPermissionTo", "u1", model.PermissionViewMembers).Return(false)
	api.On("GetTeamsForUser", "u1").Return([]*model.Team{{Id: "t1"}, {Id: "t2"}}, nil)
	api.On("GetTeamMembersForUser", "id_bob", 0, 200).Return([]*model.TeamMember{{TeamId: "t2", UserId: "id_bob"}}, nil)
	api.On("GetTeamMembersForUser", "id_eve", 0, 200).Return([]*model.TeamMember{{TeamId: "t2", UserId: "id_eve", DeleteAt: 5}}, nil)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=b&not_in_team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1)
	require.Equal(t, "bob", out[0]["username"])
}

func TestHandleUserAutocomplete_NotInTeamCutsToTwentyAfterFiltering(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("GetConfig").Return(&model.Config{})
	api.On("HasPermissionTo", "u1", model.PermissionViewMembers).Return(false)
	api.On("GetTeamsForUser", "u1").Return([]*model.Team{{Id: "t1"}, {Id: "t2"}}, nil)
	// The first 25 results share no team with the caller; the next 25 do.
	var found []*model.User
	for i := range 50 {
		id := fmt.Sprintf("user%02d", i)
		found = append(found, &model.User{Id: id, Username: id})
		team := "t3"
		if i >= 25 {
			team = "t2"
		}
		api.On("GetTeamMembersForUser", id, 0, 200).Return([]*model.TeamMember{{TeamId: team, UserId: id}}, nil)
	}
	api.On("SearchUsers", mock.Anything).Return(found, nil)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=user&not_in_team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 20)
	require.Equal(t, "user25", out[0]["username"])
}

func TestHandleUserAutocomplete_NotInTeamTeamListFailureReturnsNothing(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("GetConfig").Return(&model.Config{})
	api.On("SearchUsers", mock.Anything).Return([]*model.User{{Id: "id_bob", Username: "bob"}}, nil)
	api.On("HasPermissionTo", "u1", model.PermissionViewMembers).Return(false)
	api.On("GetTeamsForUser", "u1").Return(nil, testAppErr("boom"))

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=b&not_in_team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Empty(t, out)
}

func TestHandleListSidebarCategories_NonMemberGetsNothing(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t9", "u1").Return(&model.TeamMember{TeamId: "t9", UserId: "u1", DeleteAt: 7}, nil)

	req := httptest.NewRequest(http.MethodGet, routeSidebarCategories+"?team_id=t9", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleListSidebarCategories(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, "[]", w.Body.String())
	api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
}

func TestHandleListSidebarCategories_NeverJoinedGetsNothing(t *testing.T) {
	for name, stub := range map[string]func(api *plugintest.API){
		"lookup error": func(api *plugintest.API) {
			api.On("GetTeamMember", "t9", "u1").Return(nil, testAppErr("not found"))
		},
		"no membership": func(api *plugintest.API) {
			api.On("GetTeamMember", "t9", "u1").Return(nil, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			p := newTestPlugin(api)
			stub(api)

			req := httptest.NewRequest(http.MethodGet, routeSidebarCategories+"?team_id=t9", nil)
			req.Header.Set(headerUserID, "u1")
			w := httptest.NewRecorder()
			p.handleListSidebarCategories(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			require.JSONEq(t, "[]", w.Body.String())
			api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
		})
	}
}

func TestHandleListSidebarCategories_MemberGetsCategories(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetChannelSidebarCategories", "u1", "t1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{Id: "c1", DisplayName: "Work", Type: model.SidebarCategoryCustom},
		}},
	}, nil)

	req := httptest.NewRequest(http.MethodGet, routeSidebarCategories+"?team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleListSidebarCategories(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "Work")
}

func TestHandleUserAutocomplete_BothScopesRejected(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	req := httptest.NewRequest(http.MethodGet, routeUserAutocomplete+"?q=alice&team_id=t1&not_in_team_id=t1", nil)
	req.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleUserAutocomplete(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, "[]", strings.TrimSpace(w.Body.String()))
	api.AssertNotCalled(t, "SearchUsers", mock.Anything)
}

func TestHandleAction_MissingHeaderUnauthorized(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	req := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader("{}"))
	w := httptest.NewRecorder()
	p.handleAction(w, req, true)

	// Action handlers must return JSON (not plain-text 401) or the client
	// shows the opaque "Action integration error."
	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Contains(t, resp.EphemeralText, "verify your identity")
	api.AssertNotCalled(t, "GetUser", mock.Anything)
}

// actionBody builds a PostAction integration request body carrying the
// given pending request id in its context.
func actionBody(t *testing.T, requestID string) string {
	t.Helper()
	body, err := json.Marshal(model.PostActionIntegrationRequest{
		PostId:  "post1",
		Context: map[string]any{actionContextRequestID: requestID},
	})
	require.NoError(t, err)
	return string(body)
}

func TestHandleAction_ApproveClaimsAndCreates(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubLiveTeam(api, "team1")
	stubPendingLists(api)
	stubActiveTeamMember(api, "team1", "u_req")
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	// The claim compares against the exact bytes loadRequest returned.
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "team-x"}, nil)
	api.On("AddChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	// No AdminMemberIDs → requester is auto-promoted to channel admin.
	api.On("UpdateChannelMemberRoles", "ch1", "u_req", channelAdminRoleString).Return(&model.ChannelMember{}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post")) // resolvedPost falls back

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Update)
	api.AssertCalled(t, "KVCompareAndDelete", kvRequestPrefix+"req1", raw)
	api.AssertCalled(t, "CreateChannel", mock.Anything)
}

func TestHandleAction_CreateFailureRestoresRequest(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubLiveTeam(api, "team1")
	stubPendingLists(api)
	stubActiveTeamMember(api, "team1", "u_req")
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	// Channel creation fails after the claim already deleted the request.
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("boom"))
	// The request must be restored so it isn't lost.
	api.On("KVSet", kvRequestPrefix+"req1", mock.Anything).Return(nil)

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Contains(t, resp.EphemeralText, "still pending")
	// The claimed request was written back to the KV store, and its cards
	// still work.
	api.AssertCalled(t, "KVSet", kvRequestPrefix+"req1", mock.Anything)
	api.AssertNotCalled(t, "KVDelete", kvApprovalCardsPrefix+"req1")
}

func TestHandleAction_CreateAndRestoreBothFailWarnsResubmit(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubLiveTeam(api, "team1")
	stubPendingLists(api)
	stubActiveTeamMember(api, "team1", "u_req")
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("boom"))
	// The restore write also fails -> the request is genuinely lost.
	api.On("KVSet", kvRequestPrefix+"req1", mock.Anything).Return(testAppErr("kv down"))
	api.On("GetPost", "post1").Return(nil, testAppErr("no post")) // resolvedPost falls back

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Contains(t, resp.EphemeralText, "could not be saved")
	// The request is gone, so the card no longer offers it.
	require.NotNil(t, resp.Update)
	require.Contains(t, resp.Update.Message, "Ask the requester to submit it again")
	api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"req1")
}

func TestHandleDialogSubmit_UsesHeaderIdentityNotBody(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		AllowChannelRequests: true, AllowChannelAdminRequests: true,
		prefixes: []channelPrefix{{Prefix: "team-"}},
	})

	// The header says "realuser"; the body tries to forge a sysadmin.
	api.On("GetUser", "realuser").Return(&model.User{Id: "realuser", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "team1", "realuser").Return(nil, testAppErr("not a member"))

	body, err := json.Marshal(model.SubmitDialogRequest{
		UserId: "forged-admin",
		State:  "team1",
		Submission: map[string]any{
			fieldDisplayName: "X",
			fieldPrefix:      "team-",
			fieldName:        "marketing",
		},
	})
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodPost, routeDialog, strings.NewReader(string(body)))
	r.Header.Set(headerUserID, "realuser")
	w := httptest.NewRecorder()
	p.handleDialogSubmit(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	// Identity is the header user, never the body's forged id.
	api.AssertCalled(t, "GetUser", "realuser")
	api.AssertNotCalled(t, "GetUser", "forged-admin")
	api.AssertCalled(t, "GetTeamMember", "team1", "realuser")
}

func TestCanApprove(t *testing.T) {
	t.Run("nil user", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})
		require.False(t, p.canApprove(nil))
	})

	t.Run("system admin always", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})
		require.True(t, p.canApprove(&model.User{Id: "a", Roles: "system_user system_admin"}))
	})

	t.Run("team admin approves when flag on", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, AllowTeamAdminApprovers: true, ApprovalTeam: "appteam"})
		api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "team1"}, nil)
		stubApprovalTeamRecord(api)
		api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)
		require.True(t, p.canApprove(&model.User{Id: "u1", Roles: "system_user"}))
	})

	t.Run("team admin denied when flag off", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, AllowTeamAdminApprovers: false, ApprovalTeam: "appteam"})
		// Team membership must not even be consulted when the flag is off.
		require.False(t, p.canApprove(&model.User{Id: "u1", Roles: "system_user"}))
		api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
	})

	t.Run("regular user denied", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, AllowTeamAdminApprovers: true, ApprovalTeam: "appteam"})
		api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "team1"}, nil)
		stubApprovalTeamRecord(api)
		api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{Roles: "team_user"}, nil)
		require.False(t, p.canApprove(&model.User{Id: "u1", Roles: "system_user"}))
	})

	t.Run("former team admin denied", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, AllowTeamAdminApprovers: true, ApprovalTeam: "appteam"})
		api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "team1"}, nil)
		stubApprovalTeamRecord(api)
		// Leaving the team keeps SchemeAdmin on the soft-deleted row.
		api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{SchemeAdmin: true, DeleteAt: 12345}, nil)
		require.False(t, p.canApprove(&model.User{Id: "u1", Roles: "system_user"}))
	})
}

func TestHandleAction_TeamAdminCannotApproveOwnRequest(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, AllowTeamAdminApprovers: true, ApprovalTeam: "appteam"})

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_ta", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "u_ta").Return(&model.User{Id: "u_ta", Username: "lead", Roles: "system_user"}, nil)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetTeamMember", "appteam-id", "u_ta").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "u_ta")
	w := httptest.NewRecorder()
	p.handleAction(w, r, true)

	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Contains(t, resp.EphemeralText, "your own request")
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestHandleAction_AuditRendersDisplayNameAsText(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, ApprovalTeam: "appteam"})

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "a`b @here [c](https://example.com)", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-audit-requests", true).Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "audit1"}, nil)
	api.On("GetConfig").Return(&model.Config{}).Maybe()
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil).Maybe()
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))
	var audit *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		if post := args.Get(0).(*model.Post); post.ChannelId == "audit1" {
			audit = post
		}
	}).Return(&model.Post{}, nil)

	r := httptest.NewRequest(http.MethodPost, routeDeny, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAction(w, r, false)

	require.NotNil(t, audit)
	att := extractFirstAttachment(audit)
	require.NotNil(t, att)
	require.NotContains(t, att.Text, "@here")
	require.NotContains(t, att.Text, "](https")
	require.NotContains(t, att.Text, "@@")
	require.Contains(t, att.Text, "from @req")
}

func TestResolvedPost_OnlyRebuildsBotPosts(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	other := &model.Post{Id: "p1", UserId: "someone", ChannelId: "ch2", Message: "hello"}
	other.AddProp("custom", "value")
	api.On("GetPost", "p1").Return(other, nil)

	got := p.resolvedPost("p1", "done")

	require.Equal(t, "done", got.Message)
	require.Empty(t, got.ChannelId)
	require.Nil(t, got.GetProp("custom"))
}

func TestHandleAction_LostClaimReportsAlreadyHandled(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubLiveTeam(api, "team1")
	stubPendingLists(api)
	stubActiveTeamMember(api, "team1", "u_req")
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil).Once()
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	// Another approver already claimed it — compare-and-delete finds no match.
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(false, nil)
	// Reload finds nothing: truly handled.
	api.On("KVGet", kvRequestPrefix+"req1").Return(nil, nil).Once()

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// The loser replies privately and leaves the winner's card alone.
	require.Nil(t, resp.Update)
	require.Contains(t, resp.EphemeralText, "already been handled")
	// The loser of the race must NOT create a channel.
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	api.AssertNotCalled(t, "GetPost", mock.Anything)
}

func TestHandleAction_StaleClaimBytesAsksRetry(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubLiveTeam(api, "team1")
	stubPendingLists(api)
	stubActiveTeamMember(api, "team1", "u_req")
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil).Once()
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(false, nil)
	// Metadata re-store raced us — record still pending under different bytes.
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil).Once()

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Nil(t, resp.Update, "must not squash the card while the request is still pending")
	require.Contains(t, resp.EphemeralText, "just updated")
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestHandleAction_DenyDoesNotCreateChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	r := httptest.NewRequest(http.MethodPost, routeDeny, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAction(w, r, false)

	require.Equal(t, http.StatusOK, w.Code)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	// Requester is still notified of the denial.
	api.AssertCalled(t, "GetDirectChannel", "u_req", "bot-user-id")
}

func TestHandleAction_NonApproverRejected(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true}) // AllowTeamAdminApprovers false

	creq := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team_new", Name: "x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(creq)
	require.NoError(t, err)

	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	// Plain member of the target team — not Team Admin.
	api.On("GetTeamMember", "team_new", "u1").Return(&model.TeamMember{Roles: "team_user"}, nil)

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Contains(t, resp.EphemeralText, "permission")
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestHandleAction_PerTeamTeamAdminMayApprove(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubLiveTeam(api, "team_new")
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	// Global ApprovalTeam Team Admins are NOT opted in — only the target team's
	// Team Admins (per-team approval cards) may approve besides System Admins.
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, AllowTeamAdminApprovers: false, prefixes: []channelPrefix{{Prefix: "ops-"}}})
	stubActiveTeamMember(api, "team_new", "u_req")

	creq := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team_new", Name: "ops-test-6", DisplayName: "test 6", ChannelType: "O"}
	raw, err := json.Marshal(creq)
	require.NoError(t, err)

	api.On("GetUser", "u_ta").Return(&model.User{Id: "u_ta", Username: "lead", Roles: "system_user"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("GetTeamMember", "team_new", "u_ta").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "ops-test-6", TeamId: "team_new"}, nil)
	api.On("AddChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_req", channelAdminRoleString).Return(&model.ChannelMember{}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	r := httptest.NewRequest(http.MethodPost, routeApprove, strings.NewReader(actionBody(t, "req1")))
	r.Header.Set(headerUserID, "u_ta")
	w := httptest.NewRecorder()
	p.handleAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Update)
	require.Empty(t, resp.EphemeralText)
	api.AssertCalled(t, "CreateChannel", mock.Anything)
}

func TestCanApproveChannelRequest(t *testing.T) {
	t.Run("system admin always", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})
		require.True(t, p.canApproveChannelRequest(&model.User{Id: "a", Roles: "system_user system_admin"}, "team_new"))
	})

	t.Run("target team admin may approve", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, AllowTeamAdminApprovers: false})
		api.On("GetTeamMember", "team_new", "u_ta").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)
		require.True(t, p.canApproveChannelRequest(&model.User{Id: "u_ta", Roles: "system_user"}, "team_new"))
	})

	t.Run("plain member denied", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, AllowTeamAdminApprovers: false})
		api.On("GetTeamMember", "team_new", "u1").Return(&model.TeamMember{Roles: "team_user"}, nil)
		require.False(t, p.canApproveChannelRequest(&model.User{Id: "u1", Roles: "system_user"}, "team_new"))
	})

	t.Run("empty team id denied for non-global-approver", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})
		require.False(t, p.canApproveChannelRequest(&model.User{Id: "u1", Roles: "system_user"}, ""))
	})
}

func TestHandleAdminAction_ApproveClaimsAndPromotes(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	areq := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(areq)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1"}, nil)
	// The claim compares against the exact bytes loadAdminRequest returned.
	api.On("GetChannelMember", "chX", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(true, nil)
	api.On("GetChannelMember", "chX", "u_nom").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "chX", "u_nom", channelAdminRoles).Return(&model.ChannelMember{}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post")) // resolvedPost falls back

	r := httptest.NewRequest(http.MethodPost, routeApproveAdmin, strings.NewReader(actionBody(t, "areq1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAdminAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Update)
	api.AssertCalled(t, "KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw)
	api.AssertCalled(t, "UpdateChannelMemberRoles", "chX", "u_nom", channelAdminRoles)
	// The claim deletes the key atomically; the only KVDelete clears the
	// request's recorded approval cards.
	api.AssertNotCalled(t, "KVDelete", kvAdminRequestPrefix+"areq1")
	api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"areq1")
}

func TestHandleAdminAction_LostClaimReportsAlreadyHandled(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	areq := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(areq)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil).Once()
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	// Another reviewer already claimed and processed the request.
	api.On("GetChannelMember", "chX", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelMember", "chX", "u_nom").Return(&model.ChannelMember{}, nil)
	api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(false, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(nil, nil).Once()

	r := httptest.NewRequest(http.MethodPost, routeApproveAdmin, strings.NewReader(actionBody(t, "areq1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAdminAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Nil(t, resp.Update)
	require.Contains(t, resp.EphemeralText, "already been handled")
	// The loser must not promote anyone.
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestHandleAdminAction_UnknownRequestRepliesPrivately(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	// An unknown request gets a private reply; the post isn't loaded.
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"bogus").Return(nil, nil)

	r := httptest.NewRequest(http.MethodPost, routeDenyAdmin, strings.NewReader(actionBody(t, "bogus")))
	r.Header.Set(headerUserID, "u1")
	w := httptest.NewRecorder()
	p.handleAdminAction(w, r, false)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Nil(t, resp.Update)
	api.AssertNotCalled(t, "GetPost", mock.Anything)
}

func TestHandleAdminAction_NomineeCannotApproveOwnNomination(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	// u_req nominated u_ta, a Team Admin of the channel's team.
	areq := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_ta"}}
	raw, err := json.Marshal(areq)
	require.NoError(t, err)

	api.On("GetUser", "u_ta").Return(&model.User{Id: "u_ta", Username: "lead", Roles: "system_user"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", TeamId: "team1"}, nil)
	api.On("GetTeamMember", "team1", "u_ta").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)

	r := httptest.NewRequest(http.MethodPost, routeApproveAdmin, strings.NewReader(actionBody(t, "areq1")))
	r.Header.Set(headerUserID, "u_ta")
	w := httptest.NewRecorder()
	p.handleAdminAction(w, r, true)

	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, ownNominationText, resp.EphemeralText)
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestHandleAdminAction_RequesterCannotApproveOwnRequest(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	areq := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_ta", ChannelID: "chX", NomineeIDs: []string{"u_ta"}}
	raw, err := json.Marshal(areq)
	require.NoError(t, err)

	api.On("GetUser", "u_ta").Return(&model.User{Id: "u_ta", Username: "lead", Roles: "system_user"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", TeamId: "team1"}, nil)
	api.On("GetTeamMember", "team1", "u_ta").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)

	r := httptest.NewRequest(http.MethodPost, routeApproveAdmin, strings.NewReader(actionBody(t, "areq1")))
	r.Header.Set(headerUserID, "u_ta")
	w := httptest.NewRecorder()
	p.handleAdminAction(w, r, true)

	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Contains(t, resp.EphemeralText, "your own request")
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestHandleAdminAction_ArchivedChannelLeavesRequestPending(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	areq := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(areq)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "old", TeamId: "team1", DeleteAt: 1}, nil)

	r := httptest.NewRequest(http.MethodPost, routeApproveAdmin, strings.NewReader(actionBody(t, "areq1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAdminAction(w, r, true)

	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "~old has been archived. Deny this request instead.", resp.EphemeralText)
	api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

// sysadmin is an acting user who passes every approver check.
func sysadmin(id string) *model.User {
	return &model.User{Id: id, Username: "admin", Roles: "system_user system_admin"}
}

// callHandler sends body to handler as userID (no header when empty) and
// returns the recorder.
func callHandler(handler http.HandlerFunc, method, target, userID, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if userID != "" {
		r.Header.Set(headerUserID, userID)
	}
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

// actionResponse decodes a PostAction integration response.
func actionResponse(t *testing.T, w *httptest.ResponseRecorder) model.PostActionIntegrationResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

// jsonMap decodes a {"message"} / {"error"} style response.
func jsonMap(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code)
	var out map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

var (
	submitRoutes = []string{
		routeDialog, routeCreate, routeRequestAdmin,
		routeChannelAdminDialog, routeTeamAdminDialog, routeTeamDialog,
		routeTeamMemberDialog, routeTeamGuestDialog, routeBotDialog,
		routeIncomingWebhookDialog, routeOutgoingWebhookDialog,
		routeSubmitTeamCreationWebapp, routeSubmitTeamAdminWebapp,
		routeSubmitTeamMemberWebapp, routeSubmitTeamGuestWebapp,
		routeSubmitBotWebapp, routeSubmitIncomingWebhookWebapp,
		routeSubmitOutgoingWebhookWebapp,
	}
	lookupRoutes = []string{
		routePrefixes, routeTeams, routeChannels, routeUserAutocomplete,
		routeSidebarCategories, routeConfig,
	}
	actionRoutes = []string{
		routeApprove, routeDeny, routeApproveAdmin, routeDenyAdmin,
		routeApproveTeam, routeDenyTeam, routeApproveTeamAdmin, routeDenyTeamAdmin,
		routeApproveTeamMember, routeDenyTeamMember, routeApproveTeamGuest, routeDenyTeamGuest,
		routeApproveBot, routeDenyBot, routeApproveWebhook, routeDenyWebhook,
	}
)

func allRoutes() []string {
	return append(append(append([]string{}, submitRoutes...), lookupRoutes...), actionRoutes...)
}

// TestServeHTTP_RouteListsMatchDispatch keeps the route lists above in step
// with ServeHTTP, so a new route can't be added without these tests.
func TestServeHTTP_RouteListsMatchDispatch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "http.go", nil, 0)
	require.NoError(t, err)

	var dispatched []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ServeHTTP" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					id, ok := e.(*ast.Ident)
					require.True(t, ok, "ServeHTTP cases must name a route constant")
					dispatched = append(dispatched, id.Name)
				}
			}
			return true
		})
		return false
	})

	values := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok {
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					break
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					values[name.Name] = strings.Trim(lit.Value, `"`)
				}
			}
		}
		return true
	})

	var paths []string
	for _, name := range dispatched {
		require.Contains(t, values, name)
		paths = append(paths, values[name])
	}
	require.ElementsMatch(t, paths, allRoutes())
}

func TestServeHTTP_RoutesEveryPathToAnAuthenticatedHandler(t *testing.T) {
	routes := allRoutes()
	for _, route := range actionRoutes {
		require.True(t, isPostActionPath(route), route)
	}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			// The mock only allows logging, so any other API call panics and
			// the recovered reply fails the checks below.
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)

			r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"user_id":"u1","UserId":"u1"}`))
			w := httptest.NewRecorder()
			p.ServeHTTP(nil, w, r)

			// Each handler rejects a request with no session user, even when
			// the body names one. Approve and Deny buttons always get a JSON
			// reply the client can show.
			if isPostActionPath(route) {
				require.Equal(t, http.StatusOK, w.Code)
				require.Contains(t, actionResponse(t, w).EphemeralText, "Could not verify your identity")
				return
			}
			require.Equal(t, http.StatusUnauthorized, w.Code)
		})
	}

	t.Run("unknown path", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		r := httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil)
		r.Header.Set(headerUserID, "u1")
		w := httptest.NewRecorder()
		p.ServeHTTP(nil, w, r)
		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandleListTeams(t *testing.T) {
	t.Run("missing header", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		w := callHandler(p.handleListTeams, http.MethodGet, routeTeams, "", "")
		require.Equal(t, http.StatusUnauthorized, w.Code)
		api.AssertNotCalled(t, "GetTeamsForUser", mock.Anything)
	})

	t.Run("api error", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetTeamsForUser", "u1").Return(nil, testAppErr("db down"))

		w := callHandler(p.handleListTeams, http.MethodGet, routeTeams, "u1", "")
		require.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("lists the caller's teams", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetTeamsForUser", "u1").Return([]*model.Team{{Id: "t1", Name: "ops", DisplayName: "Ops"}}, nil)

		w := callHandler(p.handleListTeams, http.MethodGet, routeTeams, "u1", "")

		require.Equal(t, http.StatusOK, w.Code)
		require.JSONEq(t, `[{"id":"t1","name":"ops","display_name":"Ops"}]`, w.Body.String())
	})
}

func TestHandleListChannels(t *testing.T) {
	t.Run("missing header", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		w := callHandler(p.handleListChannels, http.MethodGet, routeChannels+"?team_id=t1", "", "")
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("requires team_id", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		w := callHandler(p.handleListChannels, http.MethodGet, routeChannels, "u1", "")
		require.Equal(t, http.StatusBadRequest, w.Code)
		api.AssertNotCalled(t, "GetChannelsForTeamForUser", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("api error", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetChannelsForTeamForUser", "t1", "u1", false).Return(nil, testAppErr("db down"))

		w := callHandler(p.handleListChannels, http.MethodGet, routeChannels+"?team_id=t1", "u1", "")
		require.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("keeps only public and private channels", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetChannelsForTeamForUser", "t1", "u1", false).Return([]*model.Channel{
			{Id: "c1", Name: "town-square", DisplayName: "Town Square", Type: model.ChannelTypeOpen},
			{Id: "c2", Name: "secret", DisplayName: "Secret", Type: model.ChannelTypePrivate},
			{Id: "c3", Name: "dm", Type: model.ChannelTypeDirect},
			{Id: "c4", Name: "gm", Type: model.ChannelTypeGroup},
		}, nil)

		w := callHandler(p.handleListChannels, http.MethodGet, routeChannels+"?team_id=t1", "u1", "")

		require.Equal(t, http.StatusOK, w.Code)
		require.JSONEq(t, `[
			{"id":"c1","name":"town-square","display_name":"Town Square","type":"O"},
			{"id":"c2","name":"secret","display_name":"Secret","type":"P"}
		]`, w.Body.String())
	})
}

func TestHandlePrefixes(t *testing.T) {
	t.Run("missing header", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		w := callHandler(p.handlePrefixes, http.MethodGet, routePrefixes, "", "")
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("lists configured prefixes", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		p.setConfiguration(&configuration{prefixes: parsePrefixList("team-|Teams|8\nops-", func(string, ...any) {})})

		w := callHandler(p.handlePrefixes, http.MethodGet, routePrefixes, "u1", "")

		require.Equal(t, http.StatusOK, w.Code)
		require.JSONEq(t, `[
			{"prefix":"team-","description":"Teams","suffix_regex":"8"},
			{"prefix":"ops-","description":"","suffix_regex":""}
		]`, w.Body.String())
	})
}

func TestHandleRequestAdmin(t *testing.T) {
	t.Run("missing header", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		w := callHandler(p.handleRequestAdmin, http.MethodPost, routeRequestAdmin, "", `{}`)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("invalid body", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		w := callHandler(p.handleRequestAdmin, http.MethodPost, routeRequestAdmin, "u1", `not json`)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("unknown nominee", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))

		w := callHandler(p.handleRequestAdmin, http.MethodPost, routeRequestAdmin, "u1", `{"channel_id":"chX","nominees":["@ghost"]}`)

		require.Equal(t, "unknown user: ghost", jsonMap(t, w)["error"])
	})

	t.Run("validation error", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetUserByUsername", "nom").Return(&model.User{Id: "u_nom"}, nil)

		w := callHandler(p.handleRequestAdmin, http.MethodPost, routeRequestAdmin, "u1", `{"channel_id":"","nominees":["nom"]}`)

		require.Equal(t, "a channel is required", jsonMap(t, w)["error"])
	})
}

func TestHandleWebappCreate(t *testing.T) {
	t.Run("missing header", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		w := callHandler(p.handleWebappCreate, http.MethodPost, routeCreate, "", `{}`)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("invalid body", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		w := callHandler(p.handleWebappCreate, http.MethodPost, routeCreate, "u1", `[`)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("unknown member", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))

		w := callHandler(p.handleWebappCreate, http.MethodPost, routeCreate, "u1", `{"members":["ghost"]}`)

		require.Equal(t, "unknown user: ghost", jsonMap(t, w)["error"])
	})

	t.Run("unknown channel admin", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))

		w := callHandler(p.handleWebappCreate, http.MethodPost, routeCreate, "u1", `{"admin_members":["ghost"]}`)

		require.Equal(t, "unknown user: ghost", jsonMap(t, w)["error"])
	})

	t.Run("submit error", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)

		w := callHandler(p.handleWebappCreate, http.MethodPost, routeCreate, "u1", `{"team_id":"team1","display_name":"  "}`)

		require.Equal(t, "a channel name is required", jsonMap(t, w)["error"])
		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})
}

func TestHandleDialogSubmit(t *testing.T) {
	dialogBody := func(t *testing.T, req model.SubmitDialogRequest) string {
		t.Helper()
		b, err := json.Marshal(req)
		require.NoError(t, err)
		return string(b)
	}

	t.Run("missing header", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		w := callHandler(p.handleDialogSubmit, http.MethodPost, routeDialog, "", `{}`)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("invalid body", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		w := callHandler(p.handleDialogSubmit, http.MethodPost, routeDialog, "u1", `{`)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("cancelled does nothing", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)

		w := callHandler(p.handleDialogSubmit, http.MethodPost, routeDialog, "u1", dialogBody(t, model.SubmitDialogRequest{Cancelled: true}))

		require.Equal(t, http.StatusOK, w.Code)
		require.Empty(t, w.Body.String())
		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("submit error is returned to the dialog", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)

		w := callHandler(p.handleDialogSubmit, http.MethodPost, routeDialog, "u1", dialogBody(t, model.SubmitDialogRequest{
			State:      "team1",
			Submission: map[string]any{fieldDisplayName: ""},
		}))

		require.Equal(t, http.StatusOK, w.Code)
		var resp model.SubmitDialogResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, "a channel name is required", resp.Error)
		api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
	})

	t.Run("falls back to TeamId and splits member lists", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		stubPendingLists(api)
		stubSubmittedRequestExists(api, kvRequestPrefix)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, prefixes: []channelPrefix{{Prefix: "team-"}}})

		m1, m2 := model.NewId(), model.NewId()
		api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
		api.On("GetUser", m1).Return(&model.User{Id: m1, Username: "one"}, nil).Maybe()
		api.On("GetUser", m2).Return(&model.User{Id: m2, Username: "two"}, nil).Maybe()
		api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Team One"}, nil).Maybe()
		// Dialog submissions always queue, even for a System Admin.
		var stored *channelRequest
		api.On("KVSet", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
			if stored == nil {
				require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &stored))
			}
		}).Return(nil)
		// No approval team: the card is DMed to System Admins.
		api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "a1"}}, nil).Maybe()
		api.On("GetDirectChannel", mock.Anything, "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil).Maybe()
		api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "post1"}, nil).Maybe()
		api.On("SendEphemeralPost", "admin1", mock.MatchedBy(func(post *model.Post) bool {
			return post.ChannelId == "here" && strings.Contains(post.Message, "submitted for approval")
		})).Return(&model.Post{})

		w := callHandler(p.handleDialogSubmit, http.MethodPost, routeDialog, "admin1", dialogBody(t, model.SubmitDialogRequest{
			TeamId:    "team1",
			ChannelId: "here",
			Submission: map[string]any{
				fieldDisplayName: "Ops",
				fieldPrefix:      "team-",
				fieldMembers:     " " + m1 + ", ," + m2 + " ",
				fieldAdmins:      "   ",
			},
		}))

		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, stored)
		require.Equal(t, "team1", stored.TeamID)
		require.Equal(t, "team-ops", stored.Name)
		require.Equal(t, []string{m1, m2}, stored.MemberIDs)
		require.Empty(t, stored.AdminMemberIDs)
		api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	})
}

func TestHandleAction_ErrorPaths(t *testing.T) {
	pending := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(pending)
	require.NoError(t, err)

	t.Run("invalid body", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, true) }, http.MethodPost, routeApprove, "admin1", `{`)
		// The Approve button gets a JSON reply the client can show.
		require.Contains(t, actionResponse(t, w).EphemeralText, "Invalid action request")
		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	cases := []struct {
		name  string
		setup func(api *plugintest.API)
		want  string
	}{
		{
			name: "acting user lookup fails",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "admin1").Return(nil, testAppErr("gone"))
			},
			want: "Could not verify your identity",
		},
		{
			name: "store read fails",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
				api.On("KVGet", kvRequestPrefix+"req1").Return(nil, testAppErr("kv down"))
			},
			want: "Could not load that request.",
		},
		{
			name: "request no longer pending",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
				api.On("KVGet", kvRequestPrefix+"req1").Return(nil, nil)
			},
			want: alreadyHandledText,
		},
		{
			name: "claim fails",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req"}, nil)
				api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
				api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(false, testAppErr("kv down"))
			},
			want: "Could not process that request.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			stubLiveTeam(api, "team1")
			stubPendingLists(api)
			stubActiveTeamMember(api, "team1", "u_req")
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(approvableConfig())
			tc.setup(api)

			w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, true) },
				http.MethodPost, routeApprove, "admin1", actionBody(t, "req1"))

			resp := actionResponse(t, w)
			require.Contains(t, resp.EphemeralText, tc.want)
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "CreateChannel", mock.Anything)
		})
	}
}

func TestHandleAction_WelcomeMentionsAddedMembersAndAdmins(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubLiveTeam(api, "team1")
	stubPendingLists(api)
	stubActiveTeamMember(api, "team1", "u_req")
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())

	pending := &channelRequest{
		ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O",
		MemberIDs: []string{"u_mem"}, AdminMemberIDs: []string{"u_adm"},
	}
	raw, err := json.Marshal(pending)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetUser", "u_mem").Return(&model.User{Id: "u_mem", Username: "mem"}, nil)
	api.On("GetUser", "u_adm").Return(&model.User{Id: "u_adm", Username: "adm"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "team-x"}, nil)
	for _, id := range []string{"u_req", "u_mem", "u_adm"} {
		api.On("AddChannelMember", "ch1", id).Return(&model.ChannelMember{}, nil)
	}
	api.On("UpdateChannelMemberRoles", "ch1", "u_adm", channelAdminRoles).Return(&model.ChannelMember{}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	var welcome *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		if post := args.Get(0).(*model.Post); post.ChannelId == "ch1" {
			welcome = post
		}
	}).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, true) },
		http.MethodPost, routeApprove, "admin1", actionBody(t, "req1"))

	resp := actionResponse(t, w)
	require.Contains(t, resp.Update.Message, "Approved by @admin")
	require.NotNil(t, welcome)
	require.Contains(t, welcome.Message, "requested by @req and approved by @admin")
	require.Contains(t, welcome.Message, "\nAdded: @mem")
	require.Contains(t, welcome.Message, "\nChannel admins: @adm")
}

func TestHandleAction_DenyWithUnknownRequesterAuditsByID(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, ApprovalTeam: "appteam"})
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id"}, nil)
	api.On("GetChannelByName", "appteam-id", "mattermost-audit-requests", true).Return(pluginChannel("audit", "mattermost-audit-requests"), nil)
	api.On("GetConfig").Return(&model.Config{}).Maybe()

	pending := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(pending)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(nil, testAppErr("deleted"))
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	// The requester's DM can't be opened, and the audit post fails; both
	// are logged and the denial still completes.
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(nil, testAppErr("no dm"))
	var audit *model.Post
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "audit" })).
		Run(func(args mock.Arguments) { audit = args.Get(0).(*model.Post) }).
		Return(nil, testAppErr("audit down"))
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, false) },
		http.MethodPost, routeDeny, "admin1", actionBody(t, "req1"))

	resp := actionResponse(t, w)
	require.Contains(t, resp.Update.Message, "Denied by @")
	require.NotNil(t, audit)
	auditCard := extractFirstAttachment(audit)
	require.NotNil(t, auditCard)
	require.Contains(t, auditCard.Text, "from user u_req")
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	api.AssertCalled(t, "LogWarn", "failed to open DM for card notification", "user_id", "u_req", "error", mock.Anything)
	api.AssertCalled(t, "LogWarn", "audit post failed", "channel_id", "audit", "error", mock.Anything)
}

func TestHandleAdminAction_ErrorPaths(t *testing.T) {
	pending := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(pending)
	require.NoError(t, err)

	t.Run("missing header", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) }, http.MethodPost, routeApproveAdmin, "", `{}`)
		// The Approve button gets a JSON reply the client can show.
		require.Contains(t, actionResponse(t, w).EphemeralText, "Could not verify your identity")
		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("invalid body", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) }, http.MethodPost, routeApproveAdmin, "admin1", `{`)
		require.Contains(t, actionResponse(t, w).EphemeralText, "Invalid action request")
	})

	cases := []struct {
		name  string
		setup func(api *plugintest.API)
		want  string
	}{
		{
			name: "acting user lookup fails",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "u1").Return(nil, testAppErr("gone"))
			},
			want: "Could not verify your identity",
		},
		{
			name: "store read fails",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "u1").Return(sysadmin("u1"), nil)
				api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(nil, testAppErr("kv down"))
			},
			want: "Could not load that request.",
		},
		{
			name: "not a Team Admin of the channel's team",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "plain", Roles: "system_user"}, nil)
				api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
				api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", TeamId: "team1"}, nil)
				api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{TeamId: "team1", UserId: "u1", Roles: "team_user"}, nil)
			},
			want: "You don't have permission",
		},
		{
			name: "claim fails",
			setup: func(api *plugintest.API) {
				api.On("GetUser", "u1").Return(sysadmin("u1"), nil)
				api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
				api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
				api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
				api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1"}, nil)
				api.On("GetChannelMember", "chX", "u_req").Return(&model.ChannelMember{}, nil)
				api.On("GetChannelMember", "chX", "u_nom").Return(&model.ChannelMember{}, nil)
				api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(false, testAppErr("kv down"))
			},
			want: "Could not process that request.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{AllowChannelAdminRequests: true})
			tc.setup(api)

			w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) },
				http.MethodPost, routeApproveAdmin, "u1", actionBody(t, "areq1"))

			resp := actionResponse(t, w)
			require.Contains(t, resp.EphemeralText, tc.want)
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestHandleAdminAction_DenyAfterChannelDeleted(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	pending := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(pending)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
	api.On("GetChannel", "chX").Return(nil, testAppErr("deleted"))
	api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(true, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	var dm *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) { dm = args.Get(0).(*model.Post) }).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, false) },
		http.MethodPost, routeDenyAdmin, "admin1", actionBody(t, "areq1"))

	resp := actionResponse(t, w)
	require.Equal(t, "❌ Denied by @admin for @req. Channel Admin for @nom in the channel.", resp.Update.Message)
	require.NotNil(t, dm)
	card := extractFirstAttachment(dm)
	require.NotNil(t, card)
	require.Contains(t, card.Text, "Your request to make @nom Channel Admin in the channel was not approved.")
}

func TestHandleAdminAction_ApproveWithNoRemainingNominees(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelAdminRequests: true})

	pending := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(pending)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1"}, nil)
	api.On("GetChannelMember", "chX", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(true, nil)
	// The nominee passes the approve-time check, then leaves the channel
	// before the promotion.
	api.On("GetChannelMember", "chX", "u_nom").Return(&model.ChannelMember{}, nil).Once()
	api.On("GetChannelMember", "chX", "u_nom").Return(nil, testAppErr("not a member")).Once()
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm1" })).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) },
		http.MethodPost, routeApproveAdmin, "admin1", actionBody(t, "areq1"))

	resp := actionResponse(t, w)
	require.Contains(t, resp.Update.Message, "nobody was promoted")
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
	// No promotion announcement in the channel.
	api.AssertNotCalled(t, "CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "chX" }))
}

func TestResolvedPost_StripsButtonsFromBotPost(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	post := &model.Post{Id: "p1", UserId: "bot-user-id", ChannelId: "appch", Message: "@channel — review"}
	model.ParseMessageAttachment(post, []*model.MessageAttachment{{
		Title:   "Channel creation request",
		Fields:  []*model.MessageAttachmentField{{Title: "URL", Value: "team-x"}},
		Actions: p.approvalActions("req1", "/plugins/x"),
	}})
	api.On("GetPost", "p1").Return(post, nil)
	// The original details move into a thread reply, without the buttons.
	var archived *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) { archived = args.Get(0).(*model.Post) }).
		Return(&model.Post{Id: "archive"}, nil)

	got := p.resolvedPost("p1", "done")

	require.Equal(t, "done", got.Message)
	require.Equal(t, "appch", got.ChannelId)
	require.Empty(t, got.Attachments())
	require.NotNil(t, archived)
	require.Equal(t, "p1", archived.RootId)
	attachments := archived.Attachments()
	require.Len(t, attachments, 1)
	require.Empty(t, attachments[0].Actions)
	require.Equal(t, "team-x", attachments[0].Fields[0].Value)
}

func TestCanApproveAdminRequest_Guards(t *testing.T) {
	plain := &model.User{Id: "u1", Roles: "system_user"}
	cases := []struct {
		name      string
		user      *model.User
		channelID string
		setup     func(api *plugintest.API)
		want      bool
	}{
		{name: "nil user", user: nil, channelID: "chX"},
		{name: "no channel", user: plain, channelID: ""},
		{
			name: "channel lookup fails", user: plain, channelID: "chX",
			setup: func(api *plugintest.API) { api.On("GetChannel", "chX").Return(nil, testAppErr("gone")) },
		},
		{
			name: "channel with no team", user: plain, channelID: "chX",
			setup: func(api *plugintest.API) { api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX"}, nil) },
		},
		{
			name: "scheme Team Admin", user: plain, channelID: "chX", want: true,
			setup: func(api *plugintest.API) {
				api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", TeamId: "team1"}, nil)
				api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{TeamId: "team1", UserId: "u1", SchemeAdmin: true}, nil)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			if tc.setup != nil {
				tc.setup(api)
			}
			require.Equal(t, tc.want, p.canApproveAdminRequest(tc.user, tc.channelID))
		})
	}
}

func TestHandleUserAutocomplete_EmptyQueryReturnsNothing(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	w := callHandler(p.handleUserAutocomplete, http.MethodGet, routeUserAutocomplete+"?q=%20%20&team_id=t1", "u1", "")

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `[]`, w.Body.String())
	api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "SearchUsers", mock.Anything)
}

// mockAutocompleteCaller sets up u1 as an active, non-guest member of t1.
func mockAutocompleteCaller(api *plugintest.API) {
	api.On("GetTeamMember", "t1", "u1").Return(&model.TeamMember{TeamId: "t1", UserId: "u1"}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("GetConfig").Return(&model.Config{})
}

func TestHandleUserAutocomplete_SearchFailure(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	mockAutocompleteCaller(api)
	api.On("SearchUsers", mock.Anything).Return(nil, testAppErr("search down"))

	w := callHandler(p.handleUserAutocomplete, http.MethodGet, routeUserAutocomplete+"?q=al&team_id=t1", "u1", "")

	require.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleUserAutocomplete_CapsResultsAtTwenty(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	mockAutocompleteCaller(api)
	users := make([]*model.User, 30)
	for i := range users {
		users[i] = &model.User{Id: model.NewId(), Username: fmt.Sprintf("user%02d", i)}
	}
	api.On("SearchUsers", mock.MatchedBy(func(s *model.UserSearch) bool { return s.Limit == 50 })).Return(users, nil)

	w := callHandler(p.handleUserAutocomplete, http.MethodGet, routeUserAutocomplete+"?q=user&team_id=t1", "u1", "")

	require.Equal(t, http.StatusOK, w.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 20)
	require.Equal(t, "user00", out[0]["username"])
	require.Equal(t, "user19", out[19]["username"])
}

func TestHandleAdminAction_StaleClaimBytesAsksRetry(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	areq := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(areq)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil).Once()
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("GetChannelMember", "chX", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("GetChannelMember", "chX", "u_nom").Return(&model.ChannelMember{}, nil)
	api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(false, nil)
	// Post-ID re-store raced the claim — pending record still present.
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil).Once()

	r := httptest.NewRequest(http.MethodPost, routeApproveAdmin, strings.NewReader(actionBody(t, "areq1")))
	r.Header.Set(headerUserID, "admin1")
	w := httptest.NewRecorder()
	p.handleAdminAction(w, r, true)

	require.Equal(t, http.StatusOK, w.Code)
	var resp model.PostActionIntegrationResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Nil(t, resp.Update, "must not squash while the request is still pending")
	require.Contains(t, resp.EphemeralText, "just updated")
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestResolvedPost_ArchivesOriginalTicketIntoThread(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	src := &model.Post{
		Id:        "approval-post",
		ChannelId: "approval-ch",
		Message:   "@channel — a channel request needs review.",
		UserId:    "bot-user-id",
	}
	model.ParseMessageAttachment(src, []*model.MessageAttachment{{
		Title:   "Channel request",
		Color:   "#F4A221",
		Fields:  []*model.MessageAttachmentField{{Title: "Requested by", Value: "@alice", Short: true}},
		Actions: []*model.PostAction{{Name: "Approve"}, {Name: "Deny"}},
	}})

	api.On("GetPost", "approval-post").Return(src, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		if post.RootId != "approval-post" || post.ChannelId != "approval-ch" {
			return false
		}
		if !strings.Contains(post.Message, "Original request details") {
			return false
		}
		att := extractFirstAttachment(post)
		return att != nil &&
			strings.HasPrefix(att.Title, "Original request") &&
			len(att.Actions) == 0 &&
			len(att.Fields) == 1
	})).Return(&model.Post{Id: "archive-reply"}, nil)

	out := p.resolvedPost("approval-post", "✅ Approved by @bob for @alice. Channel ~ops created.")
	require.NotNil(t, out)
	require.Equal(t, "✅ Approved by @bob for @alice. Channel ~ops created.", out.Message)
	require.Nil(t, extractFirstAttachment(out), "squashed post must not keep the original attachment")
}

func TestResolvedPost_NoArchiveWhenAlreadySquashed(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetPost", "already-done").Return(&model.Post{
		Id:        "already-done",
		ChannelId: "approval-ch",
		Message:   "This request has already been handled.",
	}, nil)

	out := p.resolvedPost("already-done", "This request has already been handled.")
	require.Equal(t, "This request has already been handled.", out.Message)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestResolvedPost_RetainsAttachmentWhenArchiveFails(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	src := &model.Post{
		Id:        "approval-post",
		ChannelId: "approval-ch",
		Message:   "@channel — a channel request needs review.",
		UserId:    "bot-user-id",
	}
	model.ParseMessageAttachment(src, []*model.MessageAttachment{{
		Title:   "Channel request",
		Color:   "#F4A221",
		Fields:  []*model.MessageAttachmentField{{Title: "Requested by", Value: "@alice", Short: true}},
		Actions: []*model.PostAction{{Name: "Approve"}, {Name: "Deny"}},
	}})

	api.On("GetPost", "approval-post").Return(src, nil)
	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("create failed"))

	out := p.resolvedPost("approval-post", "✅ Approved.")
	require.Equal(t, "✅ Approved.", out.Message)
	att := extractFirstAttachment(out)
	require.NotNil(t, att, "attachment must be retained when archive fails")
	require.Empty(t, att.Actions, "Approve/Deny buttons must still be stripped")
	require.Len(t, att.Fields, 1)
}

// expiredMillis is a CreatedAt older than 30 days.
func expiredMillis() int64 {
	return model.GetMillis() - 31*millisPerDay
}

// mockAuditCards resolves the auto-created audit channel to "audit1" and
// returns a pointer that captures the last audit card posted.
func mockAuditCards(api *plugintest.API) **model.MessageAttachment {
	var audit *model.MessageAttachment
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", Name: "appteam"}, nil)
	api.On("GetChannelByName", "appteam-id", "mattermost-audit-requests", true).Return(pluginChannel("audit1", "mattermost-audit-requests"), nil)
	api.On("GetConfig").Return(&model.Config{}).Maybe()
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "audit1" })).
		Run(func(args mock.Arguments) { audit = extractFirstAttachment(args.Get(0).(*model.Post)) }).Return(&model.Post{}, nil)
	return &audit
}

func TestHandleActionExpired(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprintf("approve=%v", approve), func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			config := approvableConfig()
			config.expiryDays = 30
			config.ApprovalTeam = "appteam"
			p.setConfiguration(config)

			req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O", CreatedAt: expiredMillis()}
			raw, err := json.Marshal(req)
			require.NoError(t, err)
			lists := fakePendingLists(api)
			lists[pendingListKey(pendingKindChannel, "u_req")] = recordJSON(t, []pendingEntry{{ID: "req1", CreatedAt: req.CreatedAt, Key: "team1/team-x"}})

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
			api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
			api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
			audit := mockAuditCards(api)
			// No ticket thread was stored, so the outcome goes out as a new DM card.
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
			var dm *model.Post
			api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm1" })).
				Run(func(args mock.Arguments) { dm = args.Get(0).(*model.Post) }).Return(&model.Post{}, nil)
			api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

			route := routeDeny
			if approve {
				route = routeApprove
			}
			w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, approve) },
				http.MethodPost, route, "admin1", actionBody(t, "req1"))

			resp := actionResponse(t, w)
			require.Equal(t, expiredText, resp.EphemeralText)
			require.Equal(t, "⌛ Expired: not reviewed within 30 days.", resp.Update.Message)
			card := extractFirstAttachment(dm)
			require.Equal(t, "Channel Request Expired", card.Title)
			require.Equal(t, colorExpired, card.Color)
			require.Contains(t, card.Text, "expired before it was reviewed")
			require.Equal(t, "⌛ Expired", (*audit).Title)
			require.Equal(t, `@admin closed channel request "X" from @req, which was not reviewed within 30 days`, (*audit).Text)
			require.Empty(t, lists)
			api.AssertNotCalled(t, "CreateChannel", mock.Anything)
			// Expiry clears the record of the request's cards.
			api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"req1")
			api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
		})
	}
}

func TestHandleActionExpiredClosesTicketThread(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30})

	req := &channelRequest{
		ID: "req1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", CreatedAt: expiredMillis(),
		ApprovalPostID: "post1", ApprovalChannelID: "appch", DMRootPostID: "dmroot", DMChannelID: "dm1",
	}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	// Relay lookups are refreshed so replies keep flowing after close.
	api.On("KVSet", kvApprovalPostPrefix+"post1", mock.Anything).Return(nil)
	api.On("KVSet", kvDMRootPrefix+"dmroot", mock.Anything).Return(nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	root := &model.Post{Id: "dmroot", ChannelId: "dm1", UserId: "bot-user-id"}
	model.ParseMessageAttachment(root, []*model.MessageAttachment{{Title: "Channel request", Color: colorPending}})
	api.On("GetPost", "dmroot").Return(root, nil)
	var updated *model.Post
	api.On("UpdatePost", mock.Anything).Run(func(args mock.Arguments) { updated = args.Get(0).(*model.Post) }).Return(&model.Post{}, nil)
	var reply *model.Post
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.RootId == "dmroot" })).
		Run(func(args mock.Arguments) { reply = args.Get(0).(*model.Post) }).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	resp := actionResponse(t, callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, true) },
		http.MethodPost, routeApprove, "admin1", actionBody(t, "req1")))

	require.Equal(t, expiredText, resp.EphemeralText)
	card := extractFirstAttachment(updated)
	require.Equal(t, "Channel Request Expired", card.Title)
	require.Equal(t, colorExpired, card.Color)
	require.Equal(t, "⌛ **Expired**: not reviewed within 30 days.", reply.Message)
}

func TestHandleActionExpiredClaim(t *testing.T) {
	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", CreatedAt: expiredMillis()}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		appErr   *model.AppError
		reloaded []byte
		want     string
	}{
		"lost":                     {want: alreadyHandledText},
		"lost, record still there": {reloaded: raw, want: "This request was just updated. Please click Approve or Deny again."},
		"failed":                   {appErr: testAppErr("kv down"), want: "Could not process that request."},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30})

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req"}, nil)
			api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil).Once()
			if tc.appErr == nil {
				api.On("KVGet", kvRequestPrefix+"req1").Return(tc.reloaded, nil).Once()
			}
			api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(false, tc.appErr)

			w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, true) },
				http.MethodPost, routeApprove, "admin1", actionBody(t, "req1"))

			resp := actionResponse(t, w)
			require.Equal(t, tc.want, resp.EphemeralText)
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "CreatePost", mock.Anything)
		})
	}
}

func TestHandleActionApproveRechecks(t *testing.T) {
	notFound := model.NewAppError("test", "test.not_found", nil, "", http.StatusNotFound)
	cases := []struct {
		name         string
		requester    *model.User
		requesterErr *model.AppError
		member       *model.TeamMember
		memberErr    *model.AppError
		memberCalled bool
		reqName      string
		want         string
	}{
		{name: "requester not found", requesterErr: notFound, reqName: "team-x", want: "The requester's account is no longer active."},
		{name: "requester lookup fails", requesterErr: testAppErr("db down"), reqName: "team-x", want: approvalRetryText},
		{name: "requester deactivated", requester: &model.User{Id: "u_req", DeleteAt: 1}, reqName: "team-x", want: "The requester's account is no longer active."},
		{name: "deactivated System Admin requester", requester: &model.User{Id: "u_req", Roles: "system_user system_admin", DeleteAt: 1}, reqName: "team-x", want: "The requester's account is no longer active."},
		{name: "requester left the team", requester: &model.User{Id: "u_req"}, member: &model.TeamMember{DeleteAt: 1}, memberCalled: true, reqName: "team-x", want: "The requester is no longer a member of the team."},
		{name: "requester was never on the team", requester: &model.User{Id: "u_req"}, memberErr: notFound, memberCalled: true, reqName: "team-x", want: "The requester is no longer a member of the team."},
		{name: "membership lookup returns nothing", requester: &model.User{Id: "u_req"}, memberCalled: true, reqName: "team-x", want: "The requester is no longer a member of the team."},
		{name: "membership lookup fails", requester: &model.User{Id: "u_req"}, memberErr: testAppErr("db down"), memberCalled: true, reqName: "team-x", want: approvalRetryText},
		{name: "prefix no longer configured", requester: &model.User{Id: "u_req"}, member: &model.TeamMember{}, memberCalled: true, reqName: "proj-x", want: "The channel name no longer matches the configured prefixes."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			stubLiveTeam(api, "team1")
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(approvableConfig())

			req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: tc.reqName, ChannelType: "O"}
			raw, err := json.Marshal(req)
			require.NoError(t, err)

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(tc.requester, tc.requesterErr)
			if tc.memberCalled {
				api.On("GetTeamMember", "team1", "u_req").Return(tc.member, tc.memberErr)
			}
			api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)

			w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, true) },
				http.MethodPost, routeApprove, "admin1", actionBody(t, "req1"))

			resp := actionResponse(t, w)
			require.Contains(t, resp.EphemeralText, tc.want)
			require.Nil(t, resp.Update, "the card keeps its buttons")
			api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
		})
	}
}

func TestHandleActionApproveSystemAdminRequesterSkipsTeamCheck(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubLiveTeam(api, "team1")
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvableConfig())

	req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "admin2", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "admin2").Return(sysadmin("admin2"), nil)
	api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
	api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "team-x"}, nil)
	api.On("AddChannelMember", "ch1", "admin2").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "admin2", channelAdminRoles).Return(&model.ChannelMember{}, nil)
	api.On("GetDirectChannel", "admin2", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, true) },
		http.MethodPost, routeApprove, "admin1", actionBody(t, "req1"))

	resp := actionResponse(t, w)
	require.Contains(t, resp.Update.Message, "Channel ~team-x created")
	api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
}

func TestHandleAdminActionApproveRechecks(t *testing.T) {
	notFound := model.NewAppError("test", "test.not_found", nil, "", http.StatusNotFound)
	cases := []struct {
		name         string
		requester    *model.User
		requesterErr *model.AppError
		member       *model.ChannelMember
		memberErr    *model.AppError
		memberCalled bool
		want         string
	}{
		{name: "requester not found", requesterErr: notFound, want: requesterInactiveText},
		{name: "requester lookup fails", requesterErr: testAppErr("db down"), want: approvalRetryText},
		{name: "requester deactivated", requester: &model.User{Id: "u_req", DeleteAt: 1}, want: requesterInactiveText},
		{name: "deactivated System Admin requester", requester: &model.User{Id: "u_req", Roles: "system_user system_admin", DeleteAt: 1}, want: requesterInactiveText},
		{name: "requester left the channel", requester: &model.User{Id: "u_req"}, memberErr: notFound, memberCalled: true, want: "The requester is no longer a member of the channel."},
		{name: "membership lookup returns nothing", requester: &model.User{Id: "u_req"}, memberCalled: true, want: "The requester is no longer a member of the channel."},
		{name: "membership lookup fails", requester: &model.User{Id: "u_req"}, memberErr: testAppErr("db down"), memberCalled: true, want: approvalRetryText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

			req := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
			raw, err := json.Marshal(req)
			require.NoError(t, err)

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(tc.requester, tc.requesterErr)
			api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
			if tc.memberCalled {
				api.On("GetChannelMember", "chX", "u_req").Return(tc.member, tc.memberErr)
			}
			api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
			api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1"}, nil)

			w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) },
				http.MethodPost, routeApproveAdmin, "admin1", actionBody(t, "areq1"))

			resp := actionResponse(t, w)
			require.Contains(t, resp.EphemeralText, tc.want)
			require.Nil(t, resp.Update, "the card keeps its buttons")
			api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
			api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestHandleAdminActionApproveSystemAdminRequesterSkipsChannelCheck(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	req := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "admin2", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "admin2").Return(sysadmin("admin2"), nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1"}, nil)
	api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(true, nil)
	api.On("GetChannelMember", "chX", "u_nom").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "chX", "u_nom", channelAdminRoles).Return(&model.ChannelMember{}, nil)
	api.On("GetDirectChannel", "admin2", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) },
		http.MethodPost, routeApproveAdmin, "admin1", actionBody(t, "areq1"))

	resp := actionResponse(t, w)
	require.NotNil(t, resp.Update, resp.EphemeralText)
	require.Contains(t, resp.Update.Message, "promoted to Channel Admin")
	api.AssertNotCalled(t, "GetChannelMember", "chX", "admin2")
}

func TestHandleAdminActionDenyInactiveRequester(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

	req := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "chX", NomineeIDs: []string{"u_nom"}}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req", DeleteAt: 1}, nil)
	api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
	api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1"}, nil)
	api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(true, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

	w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, false) },
		http.MethodPost, routeDenyAdmin, "admin1", actionBody(t, "areq1"))

	resp := actionResponse(t, w)
	require.NotNil(t, resp.Update, resp.EphemeralText)
	require.Contains(t, resp.Update.Message, "❌ Denied by @admin for @req.")
	api.AssertNotCalled(t, "GetChannelMember", mock.Anything, mock.Anything)
}

func TestHandleActionFreesPendingSlot(t *testing.T) {
	entry := pendingEntry{ID: "req1", CreatedAt: 1, Key: "team1/team-x"}
	listKey := pendingListKey(pendingKindChannel, "u_req")

	for name, tc := range map[string]struct {
		approve    bool
		createErr  *model.AppError
		restoreErr *model.AppError
		freed      bool
	}{
		"approved":                       {approve: true, freed: true},
		"denied":                         {freed: true},
		"create fails, kept":             {approve: true, createErr: testAppErr("boom")},
		"create and restore fail, freed": {approve: true, createErr: testAppErr("boom"), restoreErr: testAppErr("kv down"), freed: true},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			stubLiveTeam(api, "team1")
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(approvableConfig())

			req := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O"}
			raw, err := json.Marshal(req)
			require.NoError(t, err)
			lists := fakePendingLists(api)
			lists[listKey] = recordJSON(t, []pendingEntry{entry})

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
			stubActiveTeamMember(api, "team1", "u_req")
			api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
			api.On("KVCompareAndDelete", kvRequestPrefix+"req1", raw).Return(true, nil)
			if tc.approve {
				if tc.createErr != nil {
					api.On("CreateChannel", mock.Anything).Return(nil, tc.createErr)
					api.On("KVSet", kvRequestPrefix+"req1", mock.Anything).Return(tc.restoreErr)
				} else {
					api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "team-x"}, nil)
					api.On("AddChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
					api.On("UpdateChannelMemberRoles", "ch1", "u_req", channelAdminRoles).Return(&model.ChannelMember{}, nil)
				}
			}
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil).Maybe()
			api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil).Maybe()
			api.On("GetPost", "post1").Return(nil, testAppErr("no post")).Maybe()

			w := callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, tc.approve) },
				http.MethodPost, routeApprove, "admin1", actionBody(t, "req1"))

			resp := actionResponse(t, w)
			switch {
			case tc.restoreErr != nil:
				require.Contains(t, resp.EphemeralText, "ask the requester to submit it again")
			case tc.createErr != nil:
				require.Contains(t, resp.EphemeralText, "still pending")
			case tc.approve:
				require.Contains(t, resp.Update.Message, "Approved by @admin")
			default:
				require.Contains(t, resp.Update.Message, "Denied by @admin")
			}
			if tc.freed {
				require.Empty(t, lists)
			} else {
				require.Equal(t, []pendingEntry{entry}, decodeEntries(t, lists[listKey]))
			}
		})
	}
}

func TestHandleAdminActionExpired(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprintf("approve=%v", approve), func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30, ApprovalTeam: "appteam"})

			req := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "ch1", NomineeIDs: []string{"u_nom"}, CreatedAt: expiredMillis()}
			raw, err := json.Marshal(req)
			require.NoError(t, err)
			lists := fakePendingLists(api)
			lists[pendingListKey(pendingKindAdmin, "u_req")] = recordJSON(t, []pendingEntry{{ID: "areq1", CreatedAt: req.CreatedAt, Key: "ch1/u_nom"}})

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
			api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
			api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
			// Expiry is checked before the archived-channel check on approve.
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "marketing", TeamId: "team1", DeleteAt: 1}, nil)
			api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(true, nil)
			audit := mockAuditCards(api)
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
			var dm *model.Post
			api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm1" })).
				Run(func(args mock.Arguments) { dm = args.Get(0).(*model.Post) }).Return(&model.Post{}, nil)
			api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

			route := routeDenyAdmin
			if approve {
				route = routeApproveAdmin
			}
			resp := actionResponse(t, callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, approve) },
				http.MethodPost, route, "admin1", actionBody(t, "areq1")))

			require.Equal(t, expiredText, resp.EphemeralText)
			require.Equal(t, "⌛ Expired: not reviewed within 30 days.", resp.Update.Message)
			card := extractFirstAttachment(dm)
			require.Equal(t, "Channel Admin Request Expired", card.Title)
			require.Equal(t, "Your request to make @nom Channel Admin in ~marketing expired before it was reviewed. Submit it again if you still need it.", card.Text)
			require.Equal(t, "@admin closed the Channel Admin request for @nom in ~marketing from @req, which was not reviewed within 30 days", (*audit).Text)
			require.Empty(t, lists)
			api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
			// Expiry clears the record of the request's cards.
			api.AssertCalled(t, "KVDelete", kvApprovalCardsPrefix+"areq1")
		})
	}
}

func TestHandleAdminActionExpiredInactiveRequester(t *testing.T) {
	notFound := model.NewAppError("test", "test.not_found", nil, "", http.StatusNotFound)
	for name, tc := range map[string]struct {
		requester    *model.User
		requesterErr *model.AppError
	}{
		"deactivated": {requester: &model.User{Id: "u_req", Username: "req", DeleteAt: 1}},
		"not found":   {requesterErr: notFound},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30})

			req := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "ch1", NomineeIDs: []string{"u_nom"}, CreatedAt: expiredMillis()}
			raw, err := json.Marshal(req)
			require.NoError(t, err)
			fakePendingLists(api)

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(tc.requester, tc.requesterErr)
			api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
			api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "marketing", TeamId: "team1"}, nil)
			api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(true, nil)
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
			api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
			api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

			// Expiry is checked before the requester, so the request still
			// closes as expired rather than being left pending.
			resp := actionResponse(t, callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) },
				http.MethodPost, routeApproveAdmin, "admin1", actionBody(t, "areq1")))

			require.Equal(t, expiredText, resp.EphemeralText)
			require.NotNil(t, resp.Update)
			require.Contains(t, resp.Update.Message, "Expired")
			api.AssertNotCalled(t, "GetChannelMember", mock.Anything, mock.Anything)
		})
	}
}

func TestHandleAdminActionFreesPendingSlot(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprintf("approve=%v", approve), func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true})

			req := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "ch1", NomineeIDs: []string{"u_nom"}}
			raw, err := json.Marshal(req)
			require.NoError(t, err)
			lists := fakePendingLists(api)
			lists[pendingListKey(pendingKindAdmin, "u_req")] = recordJSON(t, []pendingEntry{{ID: "areq1", CreatedAt: 1, Key: "ch1/u_nom"}})

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil)
			api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
			api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "marketing", TeamId: "team1"}, nil)
			api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil).Maybe()
			api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(true, nil)
			api.On("GetChannelMember", "ch1", "u_nom").Return(&model.ChannelMember{}, nil).Maybe()
			api.On("UpdateChannelMemberRoles", "ch1", "u_nom", channelAdminRoles).Return(&model.ChannelMember{}, nil).Maybe()
			api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
			api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)
			api.On("GetPost", "post1").Return(nil, testAppErr("no post"))

			callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, approve) },
				http.MethodPost, routeApproveAdmin, "admin1", actionBody(t, "areq1"))

			require.Empty(t, lists)
		})
	}
}

func TestHandleActionExpiredChecksPermissionsFirst(t *testing.T) {
	expired := &channelRequest{ID: "req1", ApprovalPostID: "post1", RequesterID: "u_ta", TeamID: "team1", Name: "team-x", DisplayName: "X", CreatedAt: expiredMillis()}
	raw, err := json.Marshal(expired)
	require.NoError(t, err)

	t.Run("non-approver", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30})
		api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
		api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)
		api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{Roles: "team_user"}, nil).Maybe()

		resp := actionResponse(t, callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, false) },
			http.MethodPost, routeDeny, "u1", actionBody(t, "req1")))

		require.Contains(t, resp.EphemeralText, "permission")
		api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	})

	t.Run("requester approving their own expired request", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30})
		api.On("GetUser", "u_ta").Return(&model.User{Id: "u_ta", Username: "lead", Roles: "system_user"}, nil)
		api.On("GetTeamMember", "team1", "u_ta").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)
		api.On("KVGet", kvRequestPrefix+"req1").Return(raw, nil)

		resp := actionResponse(t, callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAction(w, r, true) },
			http.MethodPost, routeApprove, "u_ta", actionBody(t, "req1")))

		require.Contains(t, resp.EphemeralText, "your own request")
		api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	})
}

func TestHandleAdminActionExpiredChecksPermissionsFirst(t *testing.T) {
	expired := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_ta", ChannelID: "chX", NomineeIDs: []string{"u_ta"}, CreatedAt: expiredMillis()}
	raw, err := json.Marshal(expired)
	require.NoError(t, err)

	t.Run("non-approver", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30})
		lists := fakePendingLists(api)
		lists[pendingListKey(pendingKindAdmin, "u_ta")] = recordJSON(t, []pendingEntry{{ID: "areq1", CreatedAt: expired.CreatedAt, Key: "chX/u_ta"}})
		api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
		api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
		api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", TeamId: "team1"}, nil)
		api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{Roles: "team_user"}, nil)

		resp := actionResponse(t, callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, false) },
			http.MethodPost, routeDenyAdmin, "u1", actionBody(t, "areq1")))

		require.Contains(t, resp.EphemeralText, "permission")
		require.Nil(t, resp.Update)
		require.Len(t, decodeEntries(t, lists[pendingListKey(pendingKindAdmin, "u_ta")]), 1)
		api.AssertNotCalled(t, "KVCompareAndDelete", kvAdminRequestPrefix+"areq1", mock.Anything)
		api.AssertNotCalled(t, "CreatePost", mock.Anything)
	})

	t.Run("requester approving their own expired request", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30})
		api.On("GetUser", "u_ta").Return(&model.User{Id: "u_ta", Username: "lead", Roles: "system_user"}, nil)
		api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil)
		api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", TeamId: "team1"}, nil)
		api.On("GetTeamMember", "team1", "u_ta").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)

		resp := actionResponse(t, callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) },
			http.MethodPost, routeApproveAdmin, "u_ta", actionBody(t, "areq1")))

		require.Contains(t, resp.EphemeralText, "your own request")
		api.AssertNotCalled(t, "KVCompareAndDelete", mock.Anything, mock.Anything)
	})
}

func TestHandleAdminActionExpiredClaim(t *testing.T) {
	req := &adminRequest{ID: "areq1", ApprovalPostID: "post1", RequesterID: "u_req", ChannelID: "ch1", NomineeIDs: []string{"u_nom"}, CreatedAt: expiredMillis()}
	raw, err := json.Marshal(req)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		appErr *model.AppError
		want   string
	}{
		"lost":   {want: alreadyHandledText},
		"failed": {appErr: testAppErr("kv down"), want: "Could not process that request."},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{AllowChannelRequests: true, AllowChannelAdminRequests: true, expiryDays: 30})

			api.On("GetUser", "admin1").Return(sysadmin("admin1"), nil)
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req"}, nil)
			api.On("GetUser", "u_nom").Return(&model.User{Id: "u_nom", Username: "nom"}, nil)
			api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(raw, nil).Once()
			if tc.appErr == nil {
				api.On("KVGet", kvAdminRequestPrefix+"areq1").Return(nil, nil).Once()
			}
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "marketing", TeamId: "team1"}, nil)
			api.On("KVCompareAndDelete", kvAdminRequestPrefix+"areq1", raw).Return(false, tc.appErr)

			resp := actionResponse(t, callHandler(func(w http.ResponseWriter, r *http.Request) { p.handleAdminAction(w, r, true) },
				http.MethodPost, routeApproveAdmin, "admin1", actionBody(t, "areq1")))

			require.Equal(t, tc.want, resp.EphemeralText)
			require.Nil(t, resp.Update)
			api.AssertNotCalled(t, "CreatePost", mock.Anything)
		})
	}
}

func TestActionRequestID(t *testing.T) {
	cases := []struct {
		name    string
		request *model.PostActionIntegrationRequest
		post    *model.Post
		postErr *model.AppError
		want    string
	}{
		{name: "no request"},
		{name: "from click context", request: &model.PostActionIntegrationRequest{PostId: "p1", Context: map[string]any{actionContextRequestID: "r1"}}, want: "r1"},
		{name: "no context and no post", request: &model.PostActionIntegrationRequest{}},
		{name: "post can't be loaded", request: &model.PostActionIntegrationRequest{PostId: "p1"}, postErr: testAppErr("gone")},
		{name: "from the post's buttons", request: &model.PostActionIntegrationRequest{PostId: "p1"}, post: cardPost("bot-user-id", "r2"), want: "r2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			if tc.post != nil || tc.postErr != nil {
				api.On("GetPost", "p1").Return(tc.post, tc.postErr)
			}

			require.Equal(t, tc.want, p.actionRequestID(tc.request))
		})
	}
}

func TestPostRequestID(t *testing.T) {
	withActions := func(actions ...*model.PostAction) *model.Post {
		post := &model.Post{}
		model.ParseMessageAttachment(post, []*model.MessageAttachment{{Title: "Request", Actions: actions}})
		return post
	}
	require.Empty(t, postRequestID(&model.Post{}))
	require.Empty(t, postRequestID(withActions(nil, &model.PostAction{Id: "noop"})))
	require.Empty(t, postRequestID(withActions(&model.PostAction{Integration: &model.PostActionIntegration{}})))
	require.Equal(t, "r1", postRequestID(withActions(
		&model.PostAction{Integration: &model.PostActionIntegration{}},
		&model.PostAction{Integration: &model.PostActionIntegration{Context: map[string]any{actionContextRequestID: "r1"}}},
	)))
}
