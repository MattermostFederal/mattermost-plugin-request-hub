package main

import (
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// recheckCase is one approve-time check that fails. stub runs before the
// case's defaults, so its mocks win.
type recheckCase struct {
	name string
	stub func(api *plugintest.API)
	want string
}

func activeUser(id, username string) *model.User {
	return &model.User{Id: id, Username: username, Roles: "system_user"}
}

// requireApproveBlocked checks that Approve was answered privately and left
// the request, its steps and its card as they were.
func requireApproveBlocked(t *testing.T, api *plugintest.API, resp model.PostActionIntegrationResponse, kvKey, want string) {
	t.Helper()
	require.Equal(t, want, resp.EphemeralText)
	require.Nil(t, resp.Update)
	api.AssertNotCalled(t, "KVCompareAndDelete", kvKey, mock.Anything)
	api.AssertNotCalled(t, "KVCompareAndSet", kvKey, mock.Anything, mock.Anything)
}

func runRecheckCases(t *testing.T, cases []recheckCase, h actionHandler, kvKey string, record any, defaults func(api *plugintest.API)) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			p := newTestPlugin(api)
			stubSystemAdmin(api)
			api.On("KVGet", kvKey).Return(mustMarshal(t, record), nil)
			tc.stub(api)
			defaults(api)

			resp := runAsAdmin(t, p, h, true)

			requireApproveBlocked(t, api, resp, kvKey, tc.want)
		})
	}
}

// requesterCases are the requester checks every type makes.
func requesterCases() []recheckCase {
	return []recheckCase{
		{"requester deactivated", func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", DeleteAt: 1}, nil)
		}, requesterInactiveText},
		{"requester not found", func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(nil, notFoundErr())
		}, requesterInactiveText},
		{"requester lookup fails", func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(nil, testAppErr("db down"))
		}, approvalRetryText},
	}
}

// requesterTeamCases are the checks that the requester is still on team1.
func requesterTeamCases() []recheckCase {
	const left = "The requester is no longer a member of the team. Deny this request instead."
	return []recheckCase{
		{"requester left the team", func(api *plugintest.API) {
			api.On("GetTeamMember", "team1", "u_req").Return(nil, notFoundErr())
		}, left},
		{"requester's team membership deleted", func(api *plugintest.API) {
			api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{TeamId: "team1", UserId: "u_req", DeleteAt: 1}, nil)
		}, left},
		{"requester's team membership lookup fails", func(api *plugintest.API) {
			api.On("GetTeamMember", "team1", "u_req").Return(nil, testAppErr("db down"))
		}, approvalRetryText},
	}
}

func TestTeamAdminApproveRechecks(t *testing.T) {
	cases := append(requesterCases(), requesterTeamCases()...)
	cases = append(cases,
		recheckCase{"nominee deactivated", func(api *plugintest.API) {
			api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "nom", DeleteAt: 1}, nil)
		}, "@nom is deactivated. Deny this request instead."},
		recheckCase{"nominee is a bot", func(api *plugintest.API) {
			api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "nom", IsBot: true}, nil)
		}, "@nom is a bot and can't be made a Team Admin. Deny this request instead."},
		recheckCase{"nominee is a guest", func(api *plugintest.API) {
			api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "nom", Roles: model.SystemGuestRoleId}, nil)
		}, "@nom is a guest and can't be made a Team Admin. Deny this request instead."},
		recheckCase{"nominee not found", func(api *plugintest.API) {
			api.On("GetUser", "n1").Return(nil, notFoundErr())
		}, "One of the nominees could not be found. Deny this request instead."},
		recheckCase{"nominee lookup fails", func(api *plugintest.API) {
			api.On("GetUser", "n1").Return(nil, testAppErr("db down"))
		}, approvalCheckRetryText},
		recheckCase{"nominee left the team", func(api *plugintest.API) {
			api.On("GetTeamMember", "team1", "n1").Return(nil, notFoundErr())
		}, "@nom is no longer a member of the team. Deny this request instead."},
		recheckCase{"nominee's team membership lookup fails", func(api *plugintest.API) {
			api.On("GetTeamMember", "team1", "n1").Return(nil, testAppErr("db down"))
		}, approvalCheckRetryText},
	)
	record := &teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"n1"}, ApprovalPostID: "post1"}
	runRecheckCases(t, cases, teamAdminActionHandler, kvTeamAdminRequestPrefix+"r1", record, func(api *plugintest.API) {
		stubLiveTeam(api, "team1")
		api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
		api.On("GetUser", "n1").Return(activeUser("n1", "nom"), nil).Maybe()
		stubActiveTeamMember(api, "team1", "u_req")
		stubActiveTeamMember(api, "team1", "n1")
	})
}

func TestTeamAdminApproveSystemAdminRequesterSkipsTeamCheck(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	p := newTestPlugin(api)
	raw := mustMarshal(t, &teamAdminRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"n1"}, ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	api.On("KVGet", kvTeamAdminRequestPrefix+"r1").Return(raw, nil)
	stubLiveTeam(api, "team1")
	api.On("GetUser", "u_req").Return(sysadmin("u_req"), nil)
	api.On("GetUser", "n1").Return(activeUser("n1", "nom"), nil)
	stubActiveTeamMember(api, "team1", "n1")
	api.On("KVCompareAndDelete", kvTeamAdminRequestPrefix+"r1", raw).Return(true, nil)
	api.On("CreateTeamMember", "team1", "n1").Return(&model.TeamMember{}, nil)
	api.On("UpdateTeamMemberRoles", "team1", "n1", teamAdminRoles).Return(&model.TeamMember{}, nil)
	stubTicketClose(api)

	resp := runAsAdmin(t, p, teamAdminActionHandler, true)

	require.NotNil(t, resp.Update)
	api.AssertNotCalled(t, "GetTeamMember", "team1", "u_req")
}

func TestTeamMemberApproveRechecks(t *testing.T) {
	cases := append(requesterCases(),
		recheckCase{"team now managed by groups", func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", GroupConstrained: model.NewPointer(true)}, nil)
		}, "The team's members are now managed by groups. Deny this request instead."},
		recheckCase{"nominee deactivated", func(api *plugintest.API) {
			api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "nom", DeleteAt: 1}, nil)
		}, "@nom is deactivated. Deny this request instead."},
	)
	record := &teamMemberRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"n1"}, ApprovalPostID: "post1"}
	runRecheckCases(t, cases, teamMemberActionHandler, kvTeamMemberRequestPrefix+"r1", record, func(api *plugintest.API) {
		stubLiveTeam(api, "team1")
		api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
		api.On("GetUser", "n1").Return(activeUser("n1", "nom"), nil).Maybe()
	})
}

func TestTeamMemberApproveAllowsBotAndGuestNominees(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	p := newTestPlugin(api)
	raw := mustMarshal(t, &teamMemberRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", NomineeIDs: []string{"b1", "g1"}, ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	api.On("KVGet", kvTeamMemberRequestPrefix+"r1").Return(raw, nil)
	stubLiveTeam(api, "team1")
	stubRequester(api)
	api.On("GetUser", "b1").Return(&model.User{Id: "b1", Username: "botty", IsBot: true}, nil)
	api.On("GetUser", "g1").Return(&model.User{Id: "g1", Username: "guesty", Roles: model.SystemGuestRoleId}, nil)
	api.On("KVCompareAndDelete", kvTeamMemberRequestPrefix+"r1", raw).Return(false, testAppErr("kv down"))

	resp := runAsAdmin(t, p, teamMemberActionHandler, true)

	require.Equal(t, "Could not process that request.", resp.EphemeralText)
	api.AssertCalled(t, "KVCompareAndDelete", kvTeamMemberRequestPrefix+"r1", raw)
}

func TestTeamGuestApproveRechecks(t *testing.T) {
	cases := append(requesterCases(), requesterTeamCases()...)
	cases = append(cases,
		recheckCase{"channel archived", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1", DeleteAt: 1}, nil)
		}, "~town has been archived. Deny this request instead."},
		recheckCase{"channel deleted", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(nil, notFoundErr())
		}, "A channel in this request no longer exists. Deny this request instead."},
		recheckCase{"channel lookup fails", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(nil, testAppErr("db down"))
		}, approvalCheckRetryText},
		recheckCase{"channel moved to another team", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team2"}, nil)
		}, "~town is no longer in this team. Deny this request instead."},
		recheckCase{"requester left the channel", func(api *plugintest.API) {
			api.On("GetChannelMember", "ch1", "u_req").Return(nil, notFoundErr())
		}, "The requester is no longer a member of the channel. Deny this request instead."},
		recheckCase{"requester's channel membership lookup fails", func(api *plugintest.API) {
			api.On("GetChannelMember", "ch1", "u_req").Return(nil, testAppErr("db down"))
		}, approvalRetryText},
	)
	record := &teamGuestRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", Email: "g@example.com", ChannelIDs: []string{"ch1"}, ApprovalPostID: "post1"}
	runRecheckCases(t, cases, teamGuestActionHandler, kvTeamGuestRequestPrefix+"r1", record, func(api *plugintest.API) {
		stubLiveTeam(api, "team1")
		api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
		stubActiveTeamMember(api, "team1", "u_req")
		api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1"}, nil).Maybe()
		api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil).Maybe()
	})
}

func TestTeamApproveRechecks(t *testing.T) {
	record := &teamCreationRequest{ID: "r1", RequesterID: "u_req", Name: "ops", DisplayName: "Ops", ApprovalPostID: "post1"}
	runRecheckCases(t, requesterCases(), teamActionHandler, kvTeamRequestPrefix+"r1", record, func(*plugintest.API) {})
}

func TestChannelAdminApproveRechecksNominees(t *testing.T) {
	cases := []recheckCase{
		{"channel archived", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1", DeleteAt: 1}, nil)
		}, "~town has been archived. Deny this request instead."},
		{"channel deleted", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(nil, notFoundErr())
		}, "A channel in this request no longer exists. Deny this request instead."},
		{"channel lookup fails", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(nil, testAppErr("db down"))
		}, approvalCheckRetryText},
		{"channel lookup returns nothing", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(nil, nil)
		}, "A channel in this request no longer exists. Deny this request instead."},
		{"nominee deactivated", func(api *plugintest.API) {
			api.On("GetUser", "n1").Return(&model.User{Id: "n1", Username: "nom", DeleteAt: 1}, nil)
		}, "@nom is deactivated. Deny this request instead."},
		{"nominee left the channel", func(api *plugintest.API) {
			api.On("GetChannelMember", "ch1", "n1").Return(nil, notFoundErr())
		}, "@nom is no longer a member of the channel. Deny this request instead."},
		{"nominee's channel membership lookup fails", func(api *plugintest.API) {
			api.On("GetChannelMember", "ch1", "n1").Return(nil, testAppErr("db down"))
		}, approvalCheckRetryText},
	}
	record := &adminRequest{ID: "r1", RequesterID: "u_req", ChannelID: "ch1", NomineeIDs: []string{"n1"}, ApprovalPostID: "post1"}
	runRecheckCases(t, cases, func(p *Plugin) func(w http.ResponseWriter, r *http.Request, approve bool) { return p.handleAdminAction },
		kvAdminRequestPrefix+"r1", record, func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
			api.On("GetUser", "n1").Return(activeUser("n1", "nom"), nil).Maybe()
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1"}, nil).Maybe()
			api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil).Maybe()
			api.On("GetChannelMember", "ch1", "n1").Return(&model.ChannelMember{}, nil).Maybe()
		})
}

func TestChannelAdminApproveAllowsBotAndGuestNominees(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	raw := mustMarshal(t, &adminRequest{ID: "r1", RequesterID: "u_req", ChannelID: "ch1", NomineeIDs: []string{"b1", "g1"}, ApprovalPostID: "post1"})
	stubSystemAdmin(api)
	api.On("KVGet", kvAdminRequestPrefix+"r1").Return(raw, nil)
	stubRequester(api)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", TeamId: "team1"}, nil)
	api.On("GetUser", "b1").Return(&model.User{Id: "b1", Username: "botty", IsBot: true}, nil)
	api.On("GetUser", "g1").Return(&model.User{Id: "g1", Username: "guesty", Roles: model.SystemGuestRoleId}, nil)
	for _, id := range []string{"u_req", "b1", "g1"} {
		api.On("GetChannelMember", "ch1", id).Return(&model.ChannelMember{}, nil)
	}
	api.On("KVCompareAndDelete", kvAdminRequestPrefix+"r1", raw).Return(false, testAppErr("kv down"))

	resp := runAsAdmin(t, p, func(p *Plugin) func(w http.ResponseWriter, r *http.Request, approve bool) { return p.handleAdminAction }, true)

	require.Equal(t, "Could not process that request.", resp.EphemeralText)
	api.AssertCalled(t, "KVCompareAndDelete", kvAdminRequestPrefix+"r1", raw)
}

func TestChannelApproveRechecksTeam(t *testing.T) {
	cases := []recheckCase{
		{"team archived", func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DeleteAt: 1}, nil)
		}, "The team has been archived. Deny this request instead."},
		{"team deleted", func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(nil, notFoundErr())
		}, "The team no longer exists. Deny this request instead."},
		{"team lookup fails", func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(nil, testAppErr("db down"))
		}, approvalCheckRetryText},
	}
	record := &channelRequest{ID: "r1", RequesterID: "u_req", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: "O", ApprovalPostID: "post1"}
	runRecheckCases(t, cases, func(p *Plugin) func(w http.ResponseWriter, r *http.Request, approve bool) { return p.handleAction },
		kvRequestPrefix+"r1", record, func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
		})
}

func TestBotApproveRechecks(t *testing.T) {
	cases := append(requesterCases(),
		recheckCase{"owner deactivated", func(api *plugintest.API) {
			api.On("GetUser", "own1").Return(&model.User{Id: "own1", Username: "owner", DeleteAt: 1}, nil)
		}, "The bot owner is no longer an active user. Deny this request instead."},
		recheckCase{"owner is a bot", func(api *plugintest.API) {
			api.On("GetUser", "own1").Return(&model.User{Id: "own1", Username: "owner", IsBot: true}, nil)
		}, "The bot owner is no longer an active user. Deny this request instead."},
		recheckCase{"owner not found", func(api *plugintest.API) {
			api.On("GetUser", "own1").Return(nil, notFoundErr())
		}, "The bot owner is no longer an active user. Deny this request instead."},
		recheckCase{"owner lookup fails", func(api *plugintest.API) {
			api.On("GetUser", "own1").Return(nil, testAppErr("db down"))
		}, approvalCheckRetryText},
		recheckCase{"incoming webhook channel archived", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", DeleteAt: 1}, nil)
		}, "~town has been archived. Deny this request instead."},
		recheckCase{"outgoing webhook channel deleted", func(api *plugintest.API) {
			api.On("GetChannel", "ch2").Return(nil, notFoundErr())
		}, "A channel in this request no longer exists. Deny this request instead."},
		recheckCase{"outgoing webhook channel made private", func(api *plugintest.API) {
			api.On("GetChannel", "ch2").Return(&model.Channel{Id: "ch2", Name: "news", Type: model.ChannelTypePrivate}, nil)
		}, "Outgoing webhooks need a public channel, and ~news is no longer public. Deny this request instead."},
		recheckCase{"requester left the incoming webhook channel", func(api *plugintest.API) {
			api.On("GetChannelMember", "ch1", "u_req").Return(nil, notFoundErr())
		}, "The requester is no longer a member of the channel. Deny this request instead."},
	)
	defaults := func(api *plugintest.API) {
		api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
		api.On("GetUser", "own1").Return(activeUser("own1", "owner"), nil).Maybe()
		api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", Type: model.ChannelTypePrivate}, nil).Maybe()
		api.On("GetChannel", "ch2").Return(&model.Channel{Id: "ch2", Name: "news", Type: model.ChannelTypeOpen}, nil).Maybe()
		api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil).Maybe()
		api.On("GetChannelMember", "ch2", "u_req").Return(&model.ChannelMember{}, nil).Maybe()
	}
	for _, step := range []struct {
		name  string
		state twoStepState
	}{
		{"security step", twoStepState{}},
		{"system step", securityStepDone()},
	} {
		t.Run(step.name, func(t *testing.T) {
			record := pendingBotRequest(step.state)
			record.OwnerUserID = "own1"
			record.IncomingWebhookChannelID = "ch1"
			record.OutgoingWebhookChannelID = "ch2"
			runRecheckCases(t, cases, botActionHandler, kvBotRequestPrefix+"r1", record, defaults)
		})
	}
}

func TestBotApproveSkipsOwnerCheckWhenRequesterOwnsTheBot(t *testing.T) {
	api := &plugintest.API{}
	stubRelayLookups(api)
	p := newTestPlugin(api)
	record := pendingBotRequest(twoStepState{})
	record.OwnerUserID = "u_req"
	raw := mustMarshal(t, record)
	stubSystemAdmin(api)
	stubRequester(api)
	api.On("KVGet", kvBotRequestPrefix+"r1").Return(raw, nil)
	api.On("KVCompareAndSet", kvBotRequestPrefix+"r1", raw, mock.Anything).Return(false, nil).Once()

	resp := runAsAdmin(t, p, botActionHandler, true)

	require.Equal(t, "This request was just updated by someone else. Please try again.", resp.EphemeralText)
	api.AssertNumberOfCalls(t, "GetUser", 2) // admin1 and the requester only
}

func TestWebhookApproveRechecks(t *testing.T) {
	cases := append(requesterCases(),
		recheckCase{"channel archived", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", DeleteAt: 1}, nil)
		}, "~town has been archived. Deny this request instead."},
		recheckCase{"channel deleted", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(nil, notFoundErr())
		}, "A channel in this request no longer exists. Deny this request instead."},
		recheckCase{"requester left the channel", func(api *plugintest.API) {
			api.On("GetChannelMember", "ch1", "u_req").Return(nil, notFoundErr())
		}, "The requester is no longer a member of the channel. Deny this request instead."},
	)
	defaults := func(api *plugintest.API) {
		api.On("GetUser", "u_req").Return(regularUser(), nil).Maybe()
		api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town"}, nil).Maybe()
		api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil).Maybe()
	}
	for _, step := range []struct {
		name  string
		state twoStepState
	}{
		{"security step", twoStepState{}},
		{"system step", securityStepDone()},
	} {
		t.Run(step.name, func(t *testing.T) {
			runRecheckCases(t, cases, webhookActionHandler, kvWebhookRequestPrefix+"r1", pendingWebhookRequest(step.state), defaults)
		})
	}
	t.Run("outgoing webhook channel made private", func(t *testing.T) {
		record := pendingWebhookRequest(twoStepState{})
		record.WebhookType = webhookTypeOutgoing
		runRecheckCases(t, []recheckCase{{"private", func(api *plugintest.API) {
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", Type: model.ChannelTypePrivate}, nil)
		}, "Outgoing webhooks need a public channel, and ~town is no longer public. Deny this request instead."}},
			webhookActionHandler, kvWebhookRequestPrefix+"r1", record, defaults)
	})
}

func TestTwoStepApproveRechecksComeAfterPermission(t *testing.T) {
	// A user who can't approve either step gets the permission reply, not
	// the state of the request.
	for _, tc := range []struct {
		name     string
		handler  actionHandler
		kvPrefix string
		record   any
	}{
		{"bot", botActionHandler, kvBotRequestPrefix, pendingBotRequest(twoStepState{})},
		{"webhook", webhookActionHandler, kvWebhookRequestPrefix, pendingWebhookRequest(twoStepState{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubRelayLookups(api)
			p := newTestPlugin(api)
			api.On("GetUser", "u1").Return(activeUser("u1", "plain"), nil)
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", DeleteAt: 1}, nil)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", DeleteAt: 1}, nil).Maybe()
			api.On("KVGet", tc.kvPrefix+"r1").Return(mustMarshal(t, tc.record), nil)
			api.On("GetPost", "post1").Return(requestCardPost(), nil).Maybe()
			api.On("SendEphemeralPost", "u1", mock.Anything).Return(&model.Post{}).Maybe()

			resp := clickAction(t, p, tc.handler, "u1", actionBody(t, "r1"), true)

			require.Equal(t, "⚠️ You don't have permission to approve this request.", resp.EphemeralText)
			api.AssertNotCalled(t, "KVCompareAndSet", tc.kvPrefix+"r1", mock.Anything, mock.Anything)
		})
	}
}

func TestDenyIgnoresApproveRechecks(t *testing.T) {
	// The requester is deactivated and the team archived, but Deny still
	// closes every type.
	for _, tc := range actionCasesNamed("team admin", "team member", "team guest", "team", "bot", "webhook") {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			stubPendingLists(api)
			p := newTestPlugin(api)
			raw := mustMarshal(t, tc.record("post1"))
			stubSystemAdmin(api)
			api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", DeleteAt: 1}, nil).Maybe()
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops", DeleteAt: 1}, nil).Maybe()
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "town", DeleteAt: 1}, nil).Maybe()
			api.On("KVGet", tc.kvPrefix+"r1").Return(raw, nil)
			api.On("KVCompareAndDelete", tc.kvPrefix+"r1", raw).Return(true, nil).Once()
			stubTicketClose(api)

			resp := runAction(t, p, tc, false)

			require.NotNil(t, resp.Update)
			require.Contains(t, resp.Update.Message, "Denied by @admin")
		})
	}
}

func TestApprovalCheckHelpers(t *testing.T) {
	t.Run("requester team check skips System Admins", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		require.Empty(t, p.requesterTeamBlocker("team1", sysadmin("u_req")))
		api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
	})
	t.Run("requester channel check skips System Admins", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		require.Empty(t, p.requesterChannelBlocker("ch1", sysadmin("u_req")))
		api.AssertNotCalled(t, "GetChannelMember", mock.Anything, mock.Anything)
	})
	t.Run("live team passes", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		require.Empty(t, p.teamBlocker("team1", &model.Team{Id: "team1"}, nil))
	})
	t.Run("live channel passes", func(t *testing.T) {
		p := newTestPlugin(&plugintest.API{})
		require.Empty(t, p.channelBlocker("ch1", &model.Channel{Id: "ch1"}, nil))
	})
	t.Run("nominees who qualify pass", func(t *testing.T) {
		api := &plugintest.API{}
		api.On("GetUser", "n1").Return(activeUser("n1", "nom"), nil)
		stubActiveTeamMember(api, "team1", "n1")
		api.On("GetChannelMember", "ch1", "n1").Return(&model.ChannelMember{}, nil)
		p := newTestPlugin(api)
		require.Empty(t, p.nomineeBlocker([]string{"n1"}, nomineeRules{role: "a Team Admin", teamID: "team1", channelID: "ch1"}))
	})
}
