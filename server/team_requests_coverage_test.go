package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// teamSubmit describes one team-scoped request type: its KV prefix, the
// stubs that let validation pass, and the submit call.
type teamSubmit struct {
	name     string
	prefix   string
	storeErr string
	setup    func(api *plugintest.API)
	submit   func(p *Plugin) (string, error)
}

func teamSubmits() []teamSubmit {
	return []teamSubmit{
		{
			name:     "team member",
			prefix:   kvTeamMemberRequestPrefix,
			storeErr: "failed to store team member request",
			setup: func(api *plugintest.API) {
				stubTeam1(api)
				stubRequester(api)
				api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
			},
			submit: func(p *Plugin) (string, error) {
				return p.submitTeamMemberRequest("u_req", "team1", []string{testNomineeID}, "")
			},
		},
		{
			name:     "team guest",
			prefix:   kvTeamGuestRequestPrefix,
			storeErr: "failed to store team guest request",
			setup: func(api *plugintest.API) {
				stubTeam1(api)
				api.On("GetUser", testSysAdminID).Return(systemAdminUser(), nil)
				api.On("GetChannel", testChannelID).Return(&model.Channel{Id: testChannelID, TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
			},
			submit: func(p *Plugin) (string, error) {
				return p.submitTeamGuestRequest(testSysAdminID, "team1", "guest@example.com", []string{testChannelID}, "")
			},
		},
		{
			name:     "team admin",
			prefix:   kvTeamAdminRequestPrefix,
			storeErr: "failed to store team admin request",
			setup: func(api *plugintest.API) {
				stubTeam1(api)
				api.On("GetUser", testSysAdminID).Return(systemAdminUser(), nil)
				api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
				api.On("GetTeamMember", "team1", testNomineeID).Return(&model.TeamMember{TeamId: "team1", UserId: testNomineeID}, nil)
			},
			submit: func(p *Plugin) (string, error) {
				return p.submitTeamAdminRequest(testSysAdminID, "team1", []string{testNomineeID})
			},
		},
		{
			name:     "team creation",
			prefix:   kvTeamRequestPrefix,
			storeErr: "failed to store team request",
			setup:    stubRequester,
			submit: func(p *Plugin) (string, error) {
				return p.submitTeamCreationRequest(teamCreationInput{RequesterID: "u_req", DisplayName: "Marketing"})
			},
		},
	}
}

func keyWithPrefix(prefix string) any {
	return mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, prefix) })
}

func TestTeamScopedSubmit_StoreFailure(t *testing.T) {
	for _, tc := range teamSubmits() {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubPendingLists(api)
			stubLogs(api)
			p := newTestPlugin(api)
			tc.setup(api)
			api.On("KVSet", keyWithPrefix(tc.prefix), mock.Anything).Return(testAppErr("kv down"))

			_, err := tc.submit(p)

			require.EqualError(t, err, "could not save your request; try again")
			api.AssertCalled(t, "LogError", tc.storeErr, "error", mock.Anything)
			api.AssertNotCalled(t, "CreatePost", mock.Anything)
			api.AssertNotCalled(t, "GetUsers", mock.Anything)
		})
	}
}

// With no Approval team the card goes to System Admins by DM; listing them
// fails, so the stored request is removed again.
func TestTeamScopedSubmit_ApprovalPostFailureRemovesRequest(t *testing.T) {
	for _, tc := range teamSubmits() {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubPendingLists(api)
			stubLogs(api)
			p := newTestPlugin(api)
			tc.setup(api)
			api.On("KVSet", keyWithPrefix(tc.prefix), mock.Anything).Return(nil)
			api.On("GetUsers", mock.Anything).Return(nil, testAppErr("list failed"))
			var deleted string
			api.On("KVDelete", keyWithPrefix(tc.prefix)).Run(func(args mock.Arguments) {
				deleted = args.String(0)
			}).Return(nil)

			_, err := tc.submit(p)

			require.EqualError(t, err, "could not send your request to the approvers; try again or contact a System Admin")
			api.AssertCalled(t, "LogError", mock.MatchedBy(func(msg string) bool { return strings.HasPrefix(msg, "failed to post ") }), "error", mock.Anything)
			require.True(t, strings.HasPrefix(deleted, tc.prefix))
			api.AssertNotCalled(t, "CreatePost", mock.Anything)
		})
	}
}

func TestSystemAdminMentions_Fallbacks(t *testing.T) {
	t.Run("list failure", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetUsers", mock.Anything).Return(nil, testAppErr("list failed"))
		require.Equal(t, "@channel", p.systemAdminMentions())
	})

	t.Run("only the bot", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "bot-user-id"}}, nil)
		require.Equal(t, "@channel", p.systemAdminMentions())
	})

	t.Run("admins mentioned", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "bot-user-id"}, {Id: "sys1"}}, nil)
		api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "root"}, nil)
		require.Equal(t, "@root", p.systemAdminMentions())
	})
}

func TestPromoteTeamAdmins_SkipsBlankAndLogsRoleFailure(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	api.On("CreateTeamMember", "team1", "u1").Return(&model.TeamMember{}, nil)
	api.On("UpdateTeamMemberRoles", "team1", "u1", teamAdminRoles).Return(nil, testAppErr("denied"))

	p.promoteTeamAdmins(&teamAdminRequest{TeamID: "team1", NomineeIDs: []string{"", "u1"}})

	api.AssertNumberOfCalls(t, "CreateTeamMember", 1)
	api.AssertCalled(t, "LogWarn", "failed to promote nominee to team admin",
		"team_id", "team1", "user_id", "u1", "error", mock.Anything)
}

func TestAddTeamMembers_ReportsFailures(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	api.On("CreateTeamMember", "team1", "ok").Return(&model.TeamMember{}, nil)
	api.On("CreateTeamMember", "team1", "bad").Return(nil, testAppErr("denied"))

	failed := p.addTeamMembers(&teamMemberRequest{TeamID: "team1", NomineeIDs: []string{"ok", "", "bad"}})

	require.Equal(t, []string{"bad"}, failed)
	api.AssertNumberOfCalls(t, "CreateTeamMember", 2)
}

func TestExceptIDs_Cases(t *testing.T) {
	require.Equal(t, []string{"a", "c"}, exceptIDs([]string{"a", "", "b", "c"}, []string{"b"}))
	require.Equal(t, []string{"a"}, exceptIDs([]string{"a"}, nil))
	require.Empty(t, exceptIDs(nil, []string{"a"}))
}

func TestCreateTeamForRequest_LogsMemberAndRoleFailures(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	api.On("CreateTeam", mock.Anything).Return(&model.Team{Id: "new-team"}, nil)
	api.On("CreateTeamMember", "new-team", "u_req").Return(nil, testAppErr("denied"))
	api.On("UpdateTeamMemberRoles", "new-team", "u_req", teamAdminRoles).Return(nil, testAppErr("denied"))

	team, err := p.createTeamForRequest(&teamCreationRequest{RequesterID: "u_req", Name: "marketing", DisplayName: "Marketing", RequestTeamAdmin: true})

	require.NoError(t, err)
	require.Equal(t, "new-team", team.Id)
	api.AssertCalled(t, "LogWarn", "failed to add requester to new team",
		"team_id", "new-team", "user_id", "u_req", "error", mock.Anything)
	api.AssertCalled(t, "LogWarn", "failed to promote requester to team admin in new team",
		"team_id", "new-team", "user_id", "u_req", "error", mock.Anything)
}

func TestTeamCreationApprovalAttachment_InviteOnlyAndUnknownRequester(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetUser", "u_gone").Return(nil, testAppErr("missing"))

	att := p.teamCreationApprovalAttachment(&teamCreationRequest{
		ID: "r1", RequesterID: "u_gone", DisplayName: "Marketing", Name: "marketing", Type: "I", RequestTeamAdmin: true,
	}, &model.User{Username: "reqer"})

	fields := map[string]string{}
	for _, f := range att.Fields {
		fields[f.Title] = fmt.Sprint(f.Value)
	}
	require.Equal(t, "Invite only", fields["Visibility"])
	require.Contains(t, fields["⚠️ Privilege Escalation — Team Admin Requested"], "@u_gone is also requesting")
}

func TestTeamRequestStoreLoad_Failures(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVSet", mock.Anything, mock.Anything).Return(testAppErr("kv down"))
	for _, prefix := range []string{kvTeamMemberRequestPrefix, kvTeamGuestRequestPrefix, kvTeamAdminRequestPrefix, kvTeamRequestPrefix} {
		api.On("KVGet", prefix+"err").Return(nil, testAppErr("kv down"))
		api.On("KVGet", prefix+"bad").Return([]byte("{not json"), nil)
	}

	require.ErrorContains(t, p.storeTeamMemberRequest(&teamMemberRequest{ID: "r"}), "failed to store team member request")
	require.ErrorContains(t, p.storeTeamGuestRequest(&teamGuestRequest{ID: "r"}), "failed to store team guest request")
	require.ErrorContains(t, p.storeTeamAdminRequest(&teamAdminRequest{ID: "r"}), "failed to store team admin request")

	loads := []struct {
		kind string
		load func(id string) error
	}{
		{"team member", func(id string) error { _, _, err := p.loadTeamMemberRequest(id); return err }},
		{"team guest", func(id string) error { _, _, err := p.loadTeamGuestRequest(id); return err }},
		{"team admin", func(id string) error { _, _, err := p.loadTeamAdminRequest(id); return err }},
		{"team", func(id string) error { _, _, err := p.loadTeamRequest(id); return err }},
	}
	for _, l := range loads {
		require.ErrorContains(t, l.load("err"), "failed to load "+l.kind+" request")
		require.ErrorContains(t, l.load("bad"), "failed to unmarshal "+l.kind+" request")
	}
}

func TestFulfillTeamGuestRequest_Paths(t *testing.T) {
	guestReq := func() *teamGuestRequest {
		return &teamGuestRequest{TeamID: "team1", Email: "guest@example.com", ChannelIDs: []string{"ch1"}, Reason: "audit"}
	}

	t.Run("missing email", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		require.EqualError(t, p.fulfillTeamGuestRequest(nil, "admin1"), "a guest email is required")
		require.EqualError(t, p.fulfillTeamGuestRequest(&teamGuestRequest{Email: " "}, "admin1"), "a guest email is required")
		require.Empty(t, api.Calls)
	})

	t.Run("team load failure", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetTeam", "team1").Return(nil, testAppErr("missing"))
		require.ErrorContains(t, p.fulfillTeamGuestRequest(guestReq(), "admin1"), "failed to load team")
		api.AssertNotCalled(t, "CreateSession", mock.Anything)
	})

	t.Run("session failure", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubTeam1(api)
		api.On("GetUser", "admin1").Return(nil, testAppErr("no user"))
		err := p.fulfillTeamGuestRequest(guestReq(), "admin1")
		require.ErrorContains(t, err, "could not send guest email")
		api.AssertNotCalled(t, "RevokeSession", mock.Anything)
	})

	t.Run("invite sent and session revoked", func(t *testing.T) {
		srv := &restServer{status: http.StatusOK}
		ts := srv.start(t)
		api := &plugintest.API{}
		p := newTestPlugin(api)
		stubTeam1(api)
		stubSiteURL(api, ts.URL)
		stubApproverSession(api)

		require.NoError(t, p.fulfillTeamGuestRequest(guestReq(), "admin1"))
		require.Equal(t, []string{"POST /api/v4/teams/team1/invite-guests/email"}, srv.seen())
		api.AssertCalled(t, "RevokeSession", "sess1")
	})

	t.Run("invite failure is returned and revoke failure logged", func(t *testing.T) {
		srv := &restServer{status: http.StatusNotImplemented, body: `{"id":"x","message":"guest accounts off","status_code":501}`}
		ts := srv.start(t)
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		stubTeam1(api)
		stubSiteURL(api, ts.URL)
		api.On("GetUser", "admin1").Return(&model.User{Id: "admin1"}, nil)
		api.On("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
		api.On("RevokeSession", "sess1").Return(testAppErr("revoke failed"))

		err := p.fulfillTeamGuestRequest(guestReq(), "admin1")

		require.ErrorContains(t, err, "guest email invite failed")
		api.AssertCalled(t, "LogWarn", "failed to revoke admin session after guest invite", "error", mock.Anything)
	})
}
