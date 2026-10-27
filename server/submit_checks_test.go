package main

import (
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testSysAdminID = "sysadminrequesteraaaaaaaaa"
	testOwnerID    = "botowneruseridaaaaaaaaaaaa"
)

func systemAdminUser() *model.User {
	return &model.User{Id: testSysAdminID, Username: "sysadmin", Roles: "system_user system_admin"}
}

func regularUser() *model.User {
	return &model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}
}

// stubSystemAdminQueue lets a System Admin's request be stored and DMed to
// System Admins (no Approval team is configured). Calls that would carry out
// the request are left unregistered, so reaching one fails the test.
func stubSystemAdminQueue(api *plugintest.API) {
	api.On("GetUser", testSysAdminID).Return(systemAdminUser(), nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sys1", Username: "root"}}, nil).Maybe()
	api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "root"}, nil).Maybe()
	api.On("GetTeamMembers", mock.Anything, mock.Anything, mock.Anything).Return([]*model.TeamMember{}, nil).Maybe()
	api.On("GetDirectChannel", mock.Anything, "bot-user-id").Return(&model.Channel{Id: "dm"}, nil).Maybe()
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "posted"}, nil).Maybe()
}

// requireQueued checks that a request was stored under kvPrefix, its card
// copies were recorded, and nothing was carried out.
func requireQueued(t *testing.T, api *plugintest.API, kvPrefix string) {
	t.Helper()
	stored, recorded := false, false
	for _, call := range api.Calls {
		if call.Method != "KVSet" {
			continue
		}
		key := call.Arguments.String(0)
		if strings.HasPrefix(key, kvPrefix) {
			stored = true
		}
		if strings.HasPrefix(key, kvApprovalCardsPrefix) {
			recorded = true
		}
	}
	require.True(t, stored, "request should be stored")
	require.True(t, recorded, "System Admin card copies should be recorded")
	for _, method := range []string{"CreateChannel", "CreateTeam", "CreateBot", "InviteGuestsToTeam"} {
		api.AssertNotCalled(t, method, mock.Anything)
	}
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "UpdateTeamMemberRoles", mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "CreateTeamMember", mock.Anything, mock.Anything)
}

func newQueuePlugin(t *testing.T) (*Plugin, *plugintest.API) {
	t.Helper()
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	return p, api
}

func TestSystemAdminChannelRequestQueues(t *testing.T) {
	p, api := newQueuePlugin(t)
	cfg := *p.getConfiguration()
	cfg.prefixes = []channelPrefix{{Prefix: "team-"}}
	p.setConfiguration(&cfg)
	stubSystemAdminQueue(api)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Maybe()

	msg, err := p.submitRequest(requestInput{RequesterID: testSysAdminID, TeamID: "team1", DisplayName: "Ops Room", Prefix: "team-", Name: "ops", ChannelType: "O"})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	requireQueued(t, api, kvRequestPrefix)
}

func TestSystemAdminChannelAdminRequestQueues(t *testing.T) {
	p, api := newQueuePlugin(t)
	stubSystemAdminQueue(api)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "ops", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetChannelMember", "ch1", testNomineeID).Return(&model.ChannelMember{}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil).Maybe()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Maybe()

	msg, err := p.submitAdminRequest(testSysAdminID, "ch1", []string{testNomineeID})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted")
	requireQueued(t, api, kvAdminRequestPrefix)
}

func TestSystemAdminTeamRequestQueues(t *testing.T) {
	p, api := newQueuePlugin(t)
	stubSystemAdminQueue(api)

	msg, err := p.submitTeamCreationRequest(teamCreationInput{RequesterID: testSysAdminID, DisplayName: "Marketing", Type: "O", RequestTeamAdmin: true})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	requireQueued(t, api, kvTeamRequestPrefix)
}

func TestSystemAdminTeamAdminRequestQueues(t *testing.T) {
	p, api := newQueuePlugin(t)
	stubSystemAdminQueue(api)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetTeamMember", "team1", testNomineeID).Return(&model.TeamMember{TeamId: "team1", UserId: testNomineeID}, nil)

	msg, err := p.submitTeamAdminRequest(testSysAdminID, "team1", []string{testNomineeID})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	requireQueued(t, api, kvTeamAdminRequestPrefix)
}

func TestSystemAdminTeamMemberRequestQueues(t *testing.T) {
	p, api := newQueuePlugin(t)
	stubSystemAdminQueue(api)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)

	msg, err := p.submitTeamMemberRequest(testSysAdminID, "team1", []string{testNomineeID}, "")

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	requireQueued(t, api, kvTeamMemberRequestPrefix)
}

func TestSystemAdminTeamGuestRequestQueues(t *testing.T) {
	p, api := newQueuePlugin(t)
	stubSystemAdminQueue(api)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	api.On("GetChannel", testChannelID).Return(&model.Channel{Id: testChannelID, Name: "guests", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)

	msg, err := p.submitTeamGuestRequest(testSysAdminID, "team1", "guest@example.com", []string{testChannelID}, "")

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	requireQueued(t, api, kvTeamGuestRequestPrefix)
}

func TestSystemAdminBotRequestQueues(t *testing.T) {
	p, api := newQueuePlugin(t)
	stubSystemAdminQueue(api)

	msg, err := p.submitBotRequest(testSysAdminID, botRequestInput{Username: "tokbot", DisplayName: "Token Bot", RequestToken: true})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	requireQueued(t, api, kvBotRequestPrefix)
}

func TestSystemAdminWebhookRequestQueues(t *testing.T) {
	p, api := newQueuePlugin(t)
	stubSystemAdminQueue(api)
	api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Name: "alerts", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil).Maybe()

	msg, err := p.submitWebhookRequest(testSysAdminID, webhookTypeIncoming, "ch1", "Alerts", "", "")

	require.NoError(t, err)
	require.Contains(t, msg, "two-step approval")
	requireQueued(t, api, kvWebhookRequestPrefix)
}

func TestCheckTextLen(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		limit   int
		wantErr bool
	}{
		{"empty", "", 3, false},
		{"at limit", "abc", 3, false},
		{"surrounding spaces ignored", "  abc  ", 3, false},
		{"multibyte at limit", "ééé", 3, false},
		{"over limit", "abcd", 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTextLen("field", tc.s, tc.limit)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), "field must be 3 characters or fewer")
		})
	}
}

func TestCheckByteLen(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		limit   int
		wantErr bool
	}{
		{"empty", "", 4, false},
		{"at limit", "abcd", 4, false},
		{"surrounding spaces ignored", "  abcd  ", 4, false},
		{"multibyte at limit", "éé", 4, false},
		{"multibyte over limit", "ééé", 4, true},
		{"over limit", "abcde", 4, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkByteLen("field", tc.s, tc.limit)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), "field must be 4 bytes or fewer")
		})
	}
}

func TestCheckIDList(t *testing.T) {
	tooMany := make([]string, maxMembersPerList+1)
	for i := range tooMany {
		tooMany[i] = model.NewId()
	}
	cases := []struct {
		name    string
		ids     []string
		wantErr string
	}{
		{name: "empty", ids: nil},
		{name: "valid", ids: []string{testNomineeID, testChannelID}},
		{name: "too many", ids: tooMany, wantErr: "too many things: at most 100"},
		{name: "not an ID", ids: []string{testNomineeID, "bob"}, wantErr: "things must be picked from the list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkIDList(tc.ids, "things")
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestCheckCallbackURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "https", raw: "https://hooks.example.com/in"},
		{name: "http with spaces", raw: "  http://hooks.example.com:8080/in  "},
		{name: "at limit", raw: "https://e.example/" + strings.Repeat("a", maxCallbackURLLen-len("https://e.example/"))},
		{name: "too long", raw: "https://e.example/" + strings.Repeat("a", maxCallbackURLLen-len("https://e.example/")+1), wantErr: "1022 bytes or fewer"},
		{name: "relative path", raw: "/in", wantErr: "http or https"},
		{name: "scheme in other case", raw: "HTTPS://hooks.example.com/in", wantErr: "http or https"},
		{name: "other scheme", raw: "ftp://hooks.example.com/in", wantErr: "http or https"},
		{name: "no host", raw: "https:///in", wantErr: "http or https"},
		{name: "not a URL", raw: "http://[::1", wantErr: "http or https"},
		{name: "blank", raw: "", wantErr: "http or https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCallbackURL(tc.raw)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestCheckNominees(t *testing.T) {
	cases := []struct {
		name    string
		user    *model.User
		userErr bool
		rules   nomineeRules
		member  *model.TeamMember
		wantErr string
	}{
		{name: "unknown user", userErr: true, rules: nomineeRules{role: "a Team Admin"}, wantErr: "could not be found"},
		{name: "deactivated", user: &model.User{Username: "gone", DeleteAt: 1}, rules: nomineeRules{role: "a Team Admin"}, wantErr: "@gone is deactivated and can't be made a Team Admin"},
		{name: "bot refused", user: &model.User{Username: "b", IsBot: true}, rules: nomineeRules{role: "a Team Admin"}, wantErr: "is a bot"},
		{name: "bot allowed", user: &model.User{Username: "b", IsBot: true}, rules: nomineeRules{role: "a team member", allowBots: true}},
		{name: "guest refused", user: &model.User{Username: "g", Roles: model.SystemGuestRoleId}, rules: nomineeRules{role: "a Team Admin"}, wantErr: "is a guest"},
		{name: "guest allowed", user: &model.User{Username: "g", Roles: model.SystemGuestRoleId}, rules: nomineeRules{role: "a team member", allowGuests: true}},
		{name: "not on team", user: &model.User{Username: "n"}, rules: nomineeRules{role: "a Team Admin", teamID: "team1"}, wantErr: "@n isn't a member of this team"},
		{name: "left team", user: &model.User{Username: "n"}, rules: nomineeRules{role: "a Team Admin", teamID: "team1"}, member: &model.TeamMember{DeleteAt: 5}, wantErr: "isn't a member"},
		{name: "on team", user: &model.User{Username: "n"}, rules: nomineeRules{role: "a Team Admin", teamID: "team1"}, member: &model.TeamMember{TeamId: "team1"}},
		{name: "no team rule", user: &model.User{Username: "n"}, rules: nomineeRules{role: "a Team Admin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			if tc.userErr {
				api.On("GetUser", testNomineeID).Return(nil, testAppErr("not found"))
			} else {
				api.On("GetUser", testNomineeID).Return(tc.user, nil)
			}
			if tc.rules.teamID != "" {
				if tc.member != nil {
					api.On("GetTeamMember", "team1", testNomineeID).Return(tc.member, nil)
				} else {
					api.On("GetTeamMember", "team1", testNomineeID).Return(nil, testAppErr("not a member"))
				}
			}

			err := p.checkNominees([]string{testNomineeID}, tc.rules)

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestCheckBotOwner(t *testing.T) {
	cases := []struct {
		name    string
		ownerID string
		owner   *model.User
		lookup  bool
		wantErr string
	}{
		{name: "blank owner is the requester", ownerID: ""},
		{name: "requester", ownerID: "u_req"},
		{name: "not an ID", ownerID: "bob", wantErr: "picked from the list"},
		{name: "unknown user", ownerID: testOwnerID, lookup: true, wantErr: "active user"},
		{name: "deactivated", ownerID: testOwnerID, lookup: true, owner: &model.User{Id: testOwnerID, DeleteAt: 1}, wantErr: "active user"},
		{name: "a bot", ownerID: testOwnerID, lookup: true, owner: &model.User{Id: testOwnerID, IsBot: true}, wantErr: "active user"},
		{name: "another active user", ownerID: testOwnerID, lookup: true, owner: &model.User{Id: testOwnerID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			if tc.lookup {
				if tc.owner == nil {
					api.On("GetUser", testOwnerID).Return(nil, testAppErr("not found"))
				} else {
					api.On("GetUser", testOwnerID).Return(tc.owner, nil)
				}
			}

			err := p.checkBotOwner("u_req", tc.ownerID)

			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			}
			if !tc.lookup {
				api.AssertNotCalled(t, "GetUser", mock.Anything)
			}
		})
	}
}

// submitCase is one refused submission: setup registers only the calls made
// before the refusal.
type submitCase struct {
	name    string
	cfg     *configuration // nil keeps every request type on
	setup   func(api *plugintest.API)
	wantErr string
}

func runSubmitCases(t *testing.T, cases []submitCase, submit func(p *Plugin) error) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			if tc.cfg != nil {
				p.setConfiguration(tc.cfg)
			}
			if tc.setup != nil {
				tc.setup(api)
			}

			err := submit(p)

			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}

func stubRequester(api *plugintest.API) {
	api.On("GetUser", "u_req").Return(regularUser(), nil)
}

func stubTeam1(api *plugintest.API) {
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
}

func TestSubmitTeamCreationRequest_Refusals(t *testing.T) {
	type input = teamCreationInput
	inputs := map[string]input{
		"name too long":              {RequesterID: "u_req", DisplayName: strings.Repeat("x", model.TeamDisplayNameMaxRunes+1)},
		"description too long":       {RequesterID: "u_req", DisplayName: "Marketing", Description: strings.Repeat("x", maxTeamDescriptionBytes+1)},
		"description too many bytes": {RequesterID: "u_req", DisplayName: "Marketing", Description: strings.Repeat("é", maxTeamDescriptionBytes/2+1)},
		"reserved URL name":          {RequesterID: "u_req", DisplayName: "Marketing", Name: "signup-team"},
		"requester not loaded":       {RequesterID: "u_req", DisplayName: "Marketing", Name: "Marketing Team"},
	}
	cases := []submitCase{
		{name: "name too long", wantErr: "team name must be"},
		{name: "description too long", wantErr: "description must be"},
		{name: "description too many bytes", wantErr: "description must be 255 bytes or fewer"},
		{name: "reserved URL name", wantErr: "reserved by Mattermost"},
		{name: "requester not loaded", setup: func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(nil, testAppErr("no user"))
		}, wantErr: "could not load your account"},
	}
	for _, tc := range cases {
		in := inputs[tc.name]
		runSubmitCases(t, []submitCase{tc}, func(p *Plugin) error {
			_, err := p.submitTeamCreationRequest(in)
			return err
		})
	}
}

func TestSubmitTeamAdminRequest_Refusals(t *testing.T) {
	type input struct {
		teamID   string
		nominees []string
	}
	ok := input{teamID: "team1", nominees: []string{testNomineeID}}
	cases := []struct {
		submitCase
		in input
	}{
		{submitCase{name: "turned off", cfg: &configuration{}, wantErr: "disabled"}, ok},
		{submitCase{name: "no team", wantErr: "a team is required"}, input{teamID: " ", nominees: ok.nominees}},
		{submitCase{name: "no nominees", wantErr: "pick at least one person"}, input{teamID: "team1", nominees: []string{"", ""}}},
		{submitCase{name: "nominee not an ID", wantErr: "picked from the list"}, input{teamID: "team1", nominees: []string{"bob"}}},
		{submitCase{name: "team not found", setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(nil, testAppErr("no team"))
		}, wantErr: "could not find that team"}, ok},
		{submitCase{name: "requester not loaded", setup: func(api *plugintest.API) {
			stubTeam1(api)
			api.On("GetUser", "u_req").Return(nil, testAppErr("no user"))
		}, wantErr: "could not load your account"}, ok},
		{submitCase{name: "requester not on team", setup: func(api *plugintest.API) {
			stubTeam1(api)
			stubRequester(api)
			api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{DeleteAt: 3}, nil)
		}, wantErr: "you must be a member of the team"}, ok},
		{submitCase{name: "nominee is a bot", setup: func(api *plugintest.API) {
			stubTeam1(api)
			stubRequester(api)
			api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{}, nil)
			api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "b", IsBot: true}, nil)
		}, wantErr: "is a bot"}, ok},
		{submitCase{name: "nominee not on team", setup: func(api *plugintest.API) {
			stubTeam1(api)
			stubRequester(api)
			api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{}, nil)
			api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "n"}, nil)
			api.On("GetTeamMember", "team1", testNomineeID).Return(nil, testAppErr("no member"))
		}, wantErr: "isn't a member of this team"}, ok},
	}
	for _, tc := range cases {
		in := tc.in
		runSubmitCases(t, []submitCase{tc.submitCase}, func(p *Plugin) error {
			_, err := p.submitTeamAdminRequest("u_req", in.teamID, in.nominees)
			return err
		})
	}
}

func TestSubmitTeamMemberRequest_Refusals(t *testing.T) {
	type input struct {
		teamID   string
		nominees []string
		reason   string
	}
	ok := input{teamID: "team1", nominees: []string{testNomineeID}}
	cases := []struct {
		submitCase
		in input
	}{
		{submitCase{name: "no team", wantErr: "a team is required"}, input{nominees: ok.nominees}},
		{submitCase{name: "no nominees", wantErr: "pick at least one person"}, input{teamID: "team1"}},
		{submitCase{name: "nominee not an ID", wantErr: "people to add must be picked"}, input{teamID: "team1", nominees: []string{"bob"}}},
		{submitCase{name: "reason too long", wantErr: "reason must be"}, input{teamID: "team1", nominees: ok.nominees, reason: strings.Repeat("r", maxReasonLen+1)}},
		{submitCase{name: "team not found", setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(nil, testAppErr("no team"))
		}, wantErr: "could not find that team"}, ok},
		{submitCase{name: "team managed by groups", setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", GroupConstrained: model.NewPointer(true)}, nil)
		}, wantErr: "managed by groups"}, ok},
		{submitCase{name: "requester not loaded", setup: func(api *plugintest.API) {
			stubTeam1(api)
			api.On("GetUser", "u_req").Return(nil, testAppErr("no user"))
		}, wantErr: "could not load your account"}, ok},
		{submitCase{name: "nominee deactivated", setup: func(api *plugintest.API) {
			stubTeam1(api)
			stubRequester(api)
			api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "gone", DeleteAt: 1}, nil)
		}, wantErr: "deactivated"}, ok},
	}
	for _, tc := range cases {
		in := tc.in
		runSubmitCases(t, []submitCase{tc.submitCase}, func(p *Plugin) error {
			_, err := p.submitTeamMemberRequest("u_req", in.teamID, in.nominees, in.reason)
			return err
		})
	}
}

func TestSubmitTeamGuestRequest_Refusals(t *testing.T) {
	type input struct {
		teamID   string
		email    string
		channels []string
		reason   string
	}
	ok := input{teamID: "team1", email: "guest@example.com", channels: []string{testChannelID}}
	withChannel := func(ch *model.Channel) func(api *plugintest.API) {
		return func(api *plugintest.API) {
			stubTeam1(api)
			stubRequester(api)
			api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{}, nil)
			api.On("GetChannel", testChannelID).Return(ch, nil)
		}
	}
	cases := []struct {
		submitCase
		in input
	}{
		{submitCase{name: "turned off", cfg: &configuration{}, wantErr: "disabled"}, ok},
		{submitCase{name: "no team", wantErr: "a team is required"}, input{email: ok.email, channels: ok.channels}},
		{submitCase{name: "no channels", wantErr: "pick at least one channel"}, input{teamID: "team1", email: ok.email}},
		{submitCase{name: "channel not an ID", wantErr: "guest channels must be picked"}, input{teamID: "team1", email: ok.email, channels: []string{"town-square"}}},
		{submitCase{name: "bad email", wantErr: "valid guest email"}, input{teamID: "team1", email: "not-an-email", channels: ok.channels}},
		{submitCase{name: "email too long", wantErr: "valid guest email"}, input{teamID: "team1", email: strings.Repeat("a", maxEmailLen) + "@example.com", channels: ok.channels}},
		{submitCase{name: "reason too long", wantErr: "reason must be"}, input{teamID: "team1", email: ok.email, channels: ok.channels, reason: strings.Repeat("r", maxReasonLen+1)}},
		{submitCase{name: "team not found", setup: func(api *plugintest.API) {
			api.On("GetTeam", "team1").Return(nil, testAppErr("no team"))
		}, wantErr: "could not find that team"}, ok},
		{submitCase{name: "requester not loaded", setup: func(api *plugintest.API) {
			stubTeam1(api)
			api.On("GetUser", "u_req").Return(nil, testAppErr("no user"))
		}, wantErr: "could not load your account"}, ok},
		{submitCase{name: "requester not on team", setup: func(api *plugintest.API) {
			stubTeam1(api)
			stubRequester(api)
			api.On("GetTeamMember", "team1", "u_req").Return(nil, testAppErr("no member"))
		}, wantErr: "you must be a member of the team"}, ok},
		{submitCase{name: "channel in another team", setup: withChannel(&model.Channel{Id: testChannelID, TeamId: "team2", Type: model.ChannelTypeOpen}), wantErr: "must belong to the selected team"}, ok},
		{submitCase{name: "channel not found", setup: func(api *plugintest.API) {
			stubTeam1(api)
			stubRequester(api)
			api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{}, nil)
			api.On("GetChannel", testChannelID).Return(nil, testAppErr("no channel"))
		}, wantErr: "must belong to the selected team"}, ok},
		{submitCase{name: "group message channel", setup: withChannel(&model.Channel{Id: testChannelID, TeamId: "team1", Type: model.ChannelTypeGroup}), wantErr: "public or private channels"}, ok},
		{submitCase{name: "archived channel", setup: withChannel(&model.Channel{Id: testChannelID, Name: "old", TeamId: "team1", Type: model.ChannelTypeOpen, DeleteAt: 1}), wantErr: "~old is archived"}, ok},
		{submitCase{name: "requester not in channel", setup: func(api *plugintest.API) {
			withChannel(&model.Channel{Id: testChannelID, TeamId: "team1", Type: model.ChannelTypePrivate})(api)
			api.On("GetChannelMember", testChannelID, "u_req").Return(nil, testAppErr("not a member"))
		}, wantErr: "channels you're a member of"}, ok},
	}
	for _, tc := range cases {
		in := tc.in
		runSubmitCases(t, []submitCase{tc.submitCase}, func(p *Plugin) error {
			_, err := p.submitTeamGuestRequest("u_req", in.teamID, in.email, in.channels, in.reason)
			return err
		})
	}
}

func TestSubmitBotRequest_Refusals(t *testing.T) {
	ok := botRequestInput{Username: "svcbot", DisplayName: "Service Bot"}
	with := func(edit func(in *botRequestInput)) botRequestInput {
		in := ok
		edit(&in)
		return in
	}
	long := strings.Repeat("x", maxDisplayNameLen+1)
	cases := []struct {
		submitCase
		in botRequestInput
	}{
		{submitCase{name: "turned off", cfg: &configuration{}, wantErr: "disabled"}, ok},
		{submitCase{name: "username too short", wantErr: "3-22 characters"}, with(func(in *botRequestInput) { in.Username = "@ab" })},
		{submitCase{name: "username not allowed", wantErr: "lowercase letters"}, with(func(in *botRequestInput) { in.Username = "svc bot" })},
		{submitCase{name: "display name too long", wantErr: "Display name must be"}, with(func(in *botRequestInput) { in.DisplayName = long })},
		{submitCase{name: "description too long", wantErr: "Description must be"}, with(func(in *botRequestInput) { in.Description = strings.Repeat("d", maxDescriptionLen+1) })},
		{submitCase{name: "incoming hook name too long", wantErr: "Incoming webhook name must be"}, with(func(in *botRequestInput) { in.IncomingWebhookDisplayName = long })},
		{submitCase{name: "outgoing hook name too long", wantErr: "Outgoing webhook name must be"}, with(func(in *botRequestInput) { in.OutgoingWebhookDisplayName = long })},
		{submitCase{name: "hook name too many bytes", wantErr: "Incoming webhook name must be 64 bytes or fewer"}, with(func(in *botRequestInput) {
			in.IncomingWebhookDisplayName = strings.Repeat("é", maxWebhookNameBytes/2+1)
		})},
		{submitCase{name: "outgoing channel private", setup: func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannel", "ch-out").Return(&model.Channel{Id: "ch-out", Type: model.ChannelTypePrivate}, nil)
		}, wantErr: "outgoing webhook channel: outgoing webhooks can only use public channels"}, with(func(in *botRequestInput) {
			in.OutgoingWebhookChannelID = "ch-out"
			in.OutgoingWebhookCallbackURL = "https://hooks.example.com/out"
		})},
		{submitCase{name: "outgoing hook without http callback", wantErr: "http or https"}, with(func(in *botRequestInput) {
			in.OutgoingWebhookChannelID = "ch1"
			in.OutgoingWebhookCallbackURL = "mailto:ops@example.com"
		})},
		{submitCase{name: "requester not loaded", setup: func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(nil, testAppErr("no user"))
		}, wantErr: "could not load your account"}, ok},
		{submitCase{name: "owner not an ID", setup: stubRequester, wantErr: "bot owner must be picked"}, with(func(in *botRequestInput) { in.OwnerUserID = "alice" })},
		{submitCase{name: "incoming channel archived", setup: func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannel", "ch-in").Return(&model.Channel{Id: "ch-in", Type: model.ChannelTypeOpen, DeleteAt: 1}, nil)
		}, wantErr: "incoming webhook channel: webhooks can't target an archived channel"}, with(func(in *botRequestInput) { in.IncomingWebhookChannelID = "ch-in" })},
		{submitCase{name: "outgoing channel not found", setup: func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannel", "ch-out").Return(nil, testAppErr("no channel"))
		}, wantErr: "outgoing webhook channel: could not find the target channel"}, with(func(in *botRequestInput) {
			in.OutgoingWebhookChannelID = "ch-out"
			in.OutgoingWebhookCallbackURL = "https://hooks.example.com/out"
		})},
	}
	for _, tc := range cases {
		in := tc.in
		runSubmitCases(t, []submitCase{tc.submitCase}, func(p *Plugin) error {
			_, err := p.submitBotRequest("u_req", in)
			return err
		})
	}
}

func TestSubmitWebhookRequest_Refusals(t *testing.T) {
	type input struct {
		hookType, channelID, displayName, description, callbackURL string
	}
	ok := input{hookType: webhookTypeIncoming, channelID: "ch1", displayName: "Alerts"}
	outgoing := input{hookType: webhookTypeOutgoing, channelID: "ch1", displayName: "Relay"}
	cases := []struct {
		submitCase
		in input
	}{
		{submitCase{name: "turned off", cfg: &configuration{}, wantErr: "disabled"}, ok},
		{submitCase{name: "unknown type", wantErr: "unknown webhook type"}, input{hookType: "sideways", channelID: "ch1", displayName: "x"}},
		{submitCase{name: "no channel", wantErr: "a target channel is required"}, input{hookType: webhookTypeIncoming, displayName: "x"}},
		{submitCase{name: "no display name", wantErr: "a display name is required"}, input{hookType: webhookTypeIncoming, channelID: "ch1", displayName: " "}},
		{submitCase{name: "display name too long", wantErr: "display name must be"}, input{hookType: webhookTypeIncoming, channelID: "ch1", displayName: strings.Repeat("x", maxWebhookNameBytes+1)}},
		{submitCase{name: "display name too many bytes", wantErr: "display name must be 64 bytes or fewer"}, input{hookType: webhookTypeIncoming, channelID: "ch1", displayName: strings.Repeat("é", maxWebhookNameBytes/2+1)}},
		{submitCase{name: "description too long", wantErr: "description must be"}, input{hookType: webhookTypeIncoming, channelID: "ch1", displayName: "x", description: strings.Repeat("d", maxWebhookDescriptionBytes+1)}},
		{submitCase{name: "description too many bytes", wantErr: "description must be 500 bytes or fewer"}, input{hookType: webhookTypeIncoming, channelID: "ch1", displayName: "x", description: strings.Repeat("é", maxWebhookDescriptionBytes/2+1)}},
		{submitCase{name: "outgoing without callback", wantErr: "callback URL is required"}, outgoing},
		{submitCase{name: "outgoing with non-http callback", wantErr: "http or https"}, input{hookType: webhookTypeOutgoing, channelID: "ch1", displayName: "Relay", callbackURL: "file:///tmp/hook"}},
		{submitCase{name: "requester not loaded", setup: func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(nil, testAppErr("no user"))
		}, wantErr: "could not load your account"}, ok},
		{submitCase{name: "channel not found", setup: func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannel", "ch1").Return(nil, testAppErr("no channel"))
		}, wantErr: "could not find the target channel"}, ok},
		{submitCase{name: "archived channel", setup: func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Type: model.ChannelTypePrivate, DeleteAt: 1}, nil)
		}, wantErr: "archived channel"}, ok},
		{submitCase{name: "outgoing on private channel", setup: func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", Type: model.ChannelTypePrivate}, nil)
		}, wantErr: "outgoing webhooks can only use public channels"}, input{hookType: webhookTypeOutgoing, channelID: "ch1", displayName: "Relay", callbackURL: "https://hooks.example.com/out"}},
	}
	for _, tc := range cases {
		in := tc.in
		runSubmitCases(t, []submitCase{tc.submitCase}, func(p *Plugin) error {
			_, err := p.submitWebhookRequest("u_req", in.hookType, in.channelID, in.displayName, in.description, in.callbackURL)
			return err
		})
	}
}

func TestSubmitAdminRequest_Refusals(t *testing.T) {
	tooMany := make([]string, maxMembersPerList+1)
	for i := range tooMany {
		tooMany[i] = model.NewId()
	}
	type input struct {
		channelID string
		nominees  []string
	}
	ok := input{channelID: "ch1", nominees: []string{testNomineeID}}
	memberOf := func(ch *model.Channel) func(api *plugintest.API) {
		return func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
			api.On("GetChannel", "ch1").Return(ch, nil)
		}
	}
	cases := []struct {
		submitCase
		in input
	}{
		{submitCase{name: "no channel", wantErr: "a channel is required"}, input{nominees: ok.nominees}},
		{submitCase{name: "no nominees", wantErr: "pick at least one person"}, input{channelID: "ch1"}},
		{submitCase{name: "too many nominees", wantErr: "at most 100 per request"}, input{channelID: "ch1", nominees: tooMany}},
		{submitCase{name: "nominee not an ID", wantErr: "Channel Admin nominees must be picked from the list"}, input{channelID: "ch1", nominees: []string{"alice"}}},
		{submitCase{name: "requester not loaded", setup: func(api *plugintest.API) {
			api.On("GetUser", "u_req").Return(nil, testAppErr("no user"))
		}, wantErr: "could not load your account"}, ok},
		{submitCase{name: "requester not in channel", setup: func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannelMember", "ch1", "u_req").Return(nil, testAppErr("not a member"))
		}, wantErr: "not a member of it"}, ok},
		{submitCase{name: "channel not found", setup: func(api *plugintest.API) {
			stubRequester(api)
			api.On("GetChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
			api.On("GetChannel", "ch1").Return(nil, testAppErr("no channel"))
		}, wantErr: "not a member of it"}, ok},
		{submitCase{name: "direct message channel", setup: memberOf(&model.Channel{Id: "ch1", Type: model.ChannelTypeDirect}), wantErr: "public or private channels"}, ok},
		{submitCase{name: "archived channel", setup: memberOf(&model.Channel{Id: "ch1", Type: model.ChannelTypeOpen, DeleteAt: 1}), wantErr: "archived"}, ok},
		{submitCase{name: "nominee not in channel", setup: func(api *plugintest.API) {
			memberOf(&model.Channel{Id: "ch1", Type: model.ChannelTypeOpen})(api)
			api.On("GetChannelMember", "ch1", testNomineeID).Return(nil, testAppErr("not a member"))
		}, wantErr: "already be a member of the channel"}, ok},
	}
	for _, tc := range cases {
		in := tc.in
		runSubmitCases(t, []submitCase{tc.submitCase}, func(p *Plugin) error {
			_, err := p.submitAdminRequest("u_req", in.channelID, in.nominees)
			return err
		})
	}
}
