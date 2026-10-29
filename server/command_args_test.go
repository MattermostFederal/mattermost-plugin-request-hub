package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// runCommandAs runs command as userID from channel chX in team1.
func runCommandAs(p *Plugin, userID, command string) *model.CommandResponse {
	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{
		Command:   command,
		UserId:    userID,
		TeamId:    "team1",
		ChannelId: "chX",
		TriggerId: "trig",
	})
	if appErr != nil {
		panic(appErr)
	}
	return resp
}

// storedFields returns the request stored under kvPrefix as JSON fields.
func storedFields(t *testing.T, api *plugintest.API, kvPrefix string) map[string]any {
	t.Helper()
	for _, call := range api.Calls {
		if call.Method != "KVSet" || !strings.HasPrefix(call.Arguments.String(0), kvPrefix) {
			continue
		}
		var fields map[string]any
		require.NoError(t, json.Unmarshal(call.Arguments.Get(1).([]byte), &fields))
		return fields
	}
	require.Fail(t, "no request stored under "+kvPrefix)
	return nil
}

func TestSplitCommandArgs(t *testing.T) {
	for input, want := range map[string][]commandArg{
		"":                           nil,
		"  a   b ":                   {{Text: "a"}, {Text: "b"}},
		`a "b c" d`:                  {{Text: "a"}, {Text: "b c", Quoted: true}, {Text: "d"}},
		"“Ops chat” x":               {{Text: "Ops chat", Quoted: true}, {Text: "x"}},
		`""`:                         {{Text: "", Quoted: true}},
		`--purpose "--x"`:            {{Text: "--purpose"}, {Text: "--x", Quoted: true}},
		`name="a b"`:                 {{Text: `name="a`}, {Text: `b"`}},
		`27" monitors`:               {{Text: `27"`}, {Text: "monitors"}},
		`Ship the "beta"`:            {{Text: "Ship"}, {Text: "the"}, {Text: "beta", Quoted: true}},
		`"a"b c"`:                    {{Text: `a"b c`, Quoted: true}},
		"O'Brien's\tchannel\nbridge": {{Text: "O'Brien's"}, {Text: "channel"}, {Text: "bridge"}},
	} {
		got, err := splitCommandArgs(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got, input)
	}

	_, err := splitCommandArgs(`a "b c`)
	require.EqualError(t, err, "a quote mark is not closed")

	// The quotes are balanced, but the closing one is mid-word.
	_, err = splitCommandArgs(`--purpose "hello"world`)
	require.EqualError(t, err, "a closing quote mark must be followed by a space")
}

func TestReadArgs_QuotedWordIsNeverAFlag(t *testing.T) {
	positional, flags, err := readArgs(argWords(`"--weird" name --purpose "--launch" plan --private`), "--purpose", "--private")
	require.NoError(t, err)
	require.Equal(t, []string{"--weird", "name"}, positional)
	require.Equal(t, map[string][]string{"--purpose": {"--launch", "plan"}, "--private": {}}, flags)
}

func TestReadArgs_AutocorrectDashesReadAsFlags(t *testing.T) {
	positional, flags, err := readArgs(argWords("Ops – East —private –members @a"), "--private", "--members")
	require.NoError(t, err)
	require.Equal(t, []string{"Ops", "–", "East"}, positional)
	require.Equal(t, map[string][]string{"--private": {}, "--members": {"@a"}}, flags)

	_, _, err = readArgs(argWords("Ops —color"), "--private")
	require.EqualError(t, err, "unknown option `--color`")
}

func TestReadArgs_UnknownOptionCantCloseCodeSpan(t *testing.T) {
	_, _, err := readArgs([]commandArg{{Text: "Ops"}, {Text: "--a`](https://evil.test)`b"}}, "--private")
	require.EqualError(t, err, "unknown option `--a](https://evil.test)b`")
}

func TestSplitListArgs(t *testing.T) {
	for input, want := range map[string][]string{
		"@a,@b":    {"@a", "@b"},
		"@a, @b":   {"@a", "@b"},
		"@a @b,":   {"@a", "@b"},
		",":        nil,
		"~x,,~y z": {"~x", "~y", "z"},
	} {
		require.Equal(t, want, splitListArgs(strings.Fields(input)), input)
	}
}

func TestParseChannelCreateArgs_QuotesAndLists(t *testing.T) {
	in, err := parseChannelCreateArgs(argWords(`team- "Ops  Chat" --purpose "Plan the --launch" --members @alice,@bob --admins "@carol, @dave"`))
	require.NoError(t, err)
	require.Equal(t, "Ops  Chat", in.DisplayName)
	require.Equal(t, "Plan the --launch", in.Purpose)
	require.Equal(t, []string{"@alice", "@bob"}, in.Members)
	require.Equal(t, []string{"@carol", "@dave"}, in.Admins)
}

func TestParseChannelCreateArgs_EmptyListFlags(t *testing.T) {
	for command, want := range map[string]string{
		"eng- Chat --members --private": "`--members` needs at least one name",
		"eng- Chat --admins , ,":        "`--admins` needs at least one name",
	} {
		_, err := parseChannelCreateArgs(argWords(command))
		require.EqualError(t, err, want, command)
	}
}

func TestExecuteCommand_UnclosedQuote(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	resp := runCommand(p, `/mattermost-request-hub team "Field Ops`)

	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Equal(t, "Could not read the command: a quote mark is not closed.", resp.Text)
	// Nothing is registered, so any API call would panic and fail the test.
	require.Empty(t, api.Calls)
}

func TestExecuteCommand_ChannelAdminSplitsCommaList(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID}, nil)
	api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))

	require.Equal(t, "unknown user: ghost", runCommand(p, "/mattermost-request-hub channel-admin @nommy,@ghost").Text)
}

// argumentFormCase is a newer request type filed with its argument form.
type argumentFormCase struct {
	command  string
	stub     func(api *plugintest.API)
	kvPrefix string
	want     map[string]any
}

// argumentFormCases files one request of every newer type, as System Admin
// testSysAdminID in team1.
func argumentFormCases() map[string]argumentFormCase {
	return map[string]argumentFormCase{
		"team admin": {
			command: "team-admin @nommy,",
			stub: func(api *plugintest.API) {
				stubTeamAdminQueue(testNomineeID)(api)
				api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID}, nil)
			},
			kvPrefix: kvTeamAdminRequestPrefix,
			want:     map[string]any{"team_id": "team1", "nominee_ids": []any{testNomineeID}},
		},
		"team": {
			command:  `team “Field Ops” --name field-ops --description "Run the --weekly sync" —invite-only --team-admin`,
			kvPrefix: kvTeamRequestPrefix,
			want: map[string]any{
				"display_name": "Field Ops", "name": "field-ops", "description": "Run the --weekly sync",
				"type": model.TeamInvite, "request_team_admin": true,
			},
		},
		"team open by default": {
			command:  "team Field Ops",
			kvPrefix: kvTeamRequestPrefix,
			want:     map[string]any{"display_name": "Field Ops", "name": "field-ops", "type": model.TeamOpen, "request_team_admin": false},
		},
		"team member": {
			command: `team-member @nommy --reason "Ops cell"`,
			stub: func(api *plugintest.API) {
				stubTeamMemberQueue(api)
				api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID}, nil)
			},
			kvPrefix: kvTeamMemberRequestPrefix,
			want:     map[string]any{"team_id": "team1", "nominee_ids": []any{testNomineeID}, "reason": "Ops cell"},
		},
		"team guest": {
			command: "team-guest Guest@Example.com --channels ~Guests --reason Vendor access",
			stub: func(api *plugintest.API) {
				stubTeamGuestQueue(api)
				api.On("GetChannelByName", "team1", "guests", false).
					Return(&model.Channel{Id: testChannelID, Name: "guests", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
			},
			kvPrefix: kvTeamGuestRequestPrefix,
			want: map[string]any{
				"team_id": "team1", "email": "guest@example.com", "channel_ids": []any{testChannelID}, "reason": "Vendor access",
			},
		},
		"bot": {
			command: "bot svcbot Service Bot --description Posts build results --owner @owner",
			stub: func(api *plugintest.API) {
				api.On("GetUserByUsername", "owner").Return(&model.User{Id: testOwnerID}, nil)
				api.On("GetUser", testOwnerID).Return(&model.User{Id: testOwnerID, Username: "owner"}, nil)
			},
			kvPrefix: kvBotRequestPrefix,
			want: map[string]any{
				"username": "svcbot", "display_name": "Service Bot", "description": "Posts build results", "owner_user_id": testOwnerID,
			},
		},
		"incoming webhook": {
			command: "webhook-incoming ~alerts Build alerts --description CI results",
			stub: func(api *plugintest.API) {
				stubWebhookQueue(api)
				api.On("GetChannelByName", "team1", "alerts", false).
					Return(&model.Channel{Id: "ch1", Name: "alerts", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
			},
			kvPrefix: kvWebhookRequestPrefix,
			want: map[string]any{
				"webhook_type": webhookTypeIncoming, "channel_id": "ch1", "display_name": "Build alerts", "description": "CI results",
			},
		},
		"outgoing webhook": {
			command: "webhook-outgoing alerts Relay --callback https://hooks.example.com/out",
			stub: func(api *plugintest.API) {
				stubWebhookQueue(api)
				api.On("GetChannelByName", "team1", "alerts", false).
					Return(&model.Channel{Id: "ch1", Name: "alerts", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
			},
			kvPrefix: kvWebhookRequestPrefix,
			want: map[string]any{
				"webhook_type": webhookTypeOutgoing, "channel_id": "ch1", "display_name": "Relay", "callback_url": "https://hooks.example.com/out",
			},
		},
	}
}

func TestExecuteCommand_ArgumentFormsQueue(t *testing.T) {
	for name, tc := range argumentFormCases() {
		t.Run(name, func(t *testing.T) {
			// A System Admin's command queues like everyone else's.
			p, api := newQueuePlugin(t)
			stubSystemAdminQueue(api)
			if tc.stub != nil {
				tc.stub(api)
			}

			resp := runCommandAs(p, testSysAdminID, "/mattermost-request-hub "+tc.command)

			require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
			// Calls that would carry the request out are not registered, so
			// reaching one panics into ExecuteCommand's recover reply.
			require.NotContains(t, resp.Text, "Something went wrong")
			require.Contains(t, resp.Text, "approval")
			requireQueued(t, api, tc.kvPrefix)
			stored := storedFields(t, api, tc.kvPrefix)
			require.Equal(t, testSysAdminID, stored["requester_id"])
			for field, value := range tc.want {
				require.Equal(t, value, stored[field], field)
			}
		})
	}
}

func TestExecuteCommand_ArgumentFormsCloseCopiesDecidedWhilePosting(t *testing.T) {
	for name, tc := range argumentFormCases() {
		t.Run(name, func(t *testing.T) {
			// A System Admin decided the request from the first direct-message
			// copy while the copies were still being posted, so it is gone
			// once they are recorded.
			api := &plugintest.API{}
			stubNamesFree(api)
			stubLogs(api)
			api.On("KVGet", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, tc.kvPrefix) })).Return(nil, nil)
			api.On("KVGet", isApprovalCardsKey).Return([]byte(`["posted"]`), nil)
			stubPendingLists(api)
			p := newTestPlugin(api)
			stubSystemAdminQueue(api)
			if tc.stub != nil {
				tc.stub(api)
			}
			card := approvalCard("posted")
			api.On("GetPost", "posted").Return(card, nil)
			var closed *model.Post
			api.On("UpdatePost", mock.Anything).Run(func(args mock.Arguments) { closed = args.Get(0).(*model.Post) }).Return(&model.Post{}, nil)

			resp := runCommandAs(p, testSysAdminID, "/mattermost-request-hub "+tc.command)

			require.NotContains(t, resp.Text, "Something went wrong")
			require.NotNil(t, closed, "the recorded copy should be closed")
			require.Equal(t, "posted", closed.Id)
			require.Equal(t, alreadyHandledText, closed.Message)
			api.AssertCalled(t, "KVDelete", isApprovalCardsKey)
		})
	}
}

func TestExecuteCommand_ArgumentFormsRespectToggles(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.sub, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{})

			resp := runCommand(p, "/mattermost-request-hub "+tc.sub+" @someone x --reason y")

			require.Equal(t, tc.disabledMsg, resp.Text)
			require.Empty(t, api.Calls)
		})
	}
}

func TestExecuteCommand_ArgumentFormsUsage(t *testing.T) {
	for command, want := range map[string]string{
		"channel-admin --x":                          "unknown option `--x`",
		"team-admin --x":                             "unknown option `--x`",
		"team-admin @":                               "name at least one team member",
		"team --x":                                   "unknown option `--x`",
		"team --invite-only":                         "a team name is required",
		"team Ops --name a b":                        "`--name` takes exactly one value",
		"team Ops --description":                     "`--description` needs some text",
		"team Ops --invite-only yes":                 "`--invite-only` takes no value",
		"team Ops --team-admin yes":                  "`--team-admin` takes no value",
		"team-member @a --reason":                    "`--reason` needs some text",
		"team-member , --reason x":                   "name at least one user",
		"team-member --nope":                         "unknown option `--nope`",
		"team-guest a@b.co d@e.co --channels ~x":     "give one guest email",
		"team-guest a@b.co --reason":                 "`--reason` needs some text",
		"team-guest --x":                             "unknown option `--x`",
		"team-guest a@b.co":                          "name at least one channel with `--channels`",
		"team-guest a@b.co --channels ~,":            "name at least one channel with `--channels`",
		"team-guest a@b.co --channels":               "`--channels` needs at least one name",
		"bot svcbot Svc --owner @":                   "name the owner as `@user`",
		"bot svcbot":                                 "a bot username and a display name are required",
		"bot svcbot Svc --owner a b":                 "`--owner` takes exactly one value",
		"bot svcbot Svc --description":               "`--description` needs some text",
		"bot --x":                                    "unknown option `--x`",
		"webhook-incoming ~alerts":                   "a channel and a display name are required",
		"webhook-incoming ~alerts A --callback http": "unknown option `--callback`",
		"webhook-incoming ~ Alerts":                  "name the channel as `~channel`",
		"webhook-outgoing ~alerts A --callback":      "`--callback` takes exactly one value",
		"webhook-outgoing ~alerts A --description":   "`--description` needs some text",
		"webhook-outgoing --x":                       "unknown option `--x`",
	} {
		t.Run(command, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)

			resp := runCommand(p, "/mattermost-request-hub "+command)

			require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
			require.Contains(t, resp.Text, "Could not read the request: "+want+".")
			require.Contains(t, resp.Text, "Usage: `/mattermost-request-hub "+strings.Fields(command)[0]+" ")
			// Nothing is registered, so any API call would panic and fail the test.
			require.Empty(t, api.Calls)
		})
	}
}

func TestExecuteCommand_ArgumentFormsUnknownNames(t *testing.T) {
	for command, want := range map[string]string{
		"team-admin @ghost":                                                 "unknown user: ghost",
		"team-member @ghost --reason x":                                     "unknown user: ghost",
		"bot svcbot Svc --owner @ghost":                                     "unknown user: ghost",
		"team-guest a@b.co --channels ~nope":                                "unknown channel: ~nope",
		"webhook-incoming ~nope Alerts":                                     "unknown channel: ~nope",
		"webhook-outgoing ~nope Relay --callback https://hooks.example.com": "unknown channel: ~nope",
	} {
		t.Run(command, func(t *testing.T) {
			api := &plugintest.API{}
			p := newTestPlugin(api)
			api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found")).Maybe()
			api.On("GetChannelByName", "team1", "nope", false).Return(nil, notFoundErr()).Maybe()

			require.Equal(t, want, runCommand(p, "/mattermost-request-hub "+command).Text)
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}

func TestExecuteCommand_ArgumentFormsPassSubmitErrorsBack(t *testing.T) {
	for command, want := range map[string]string{
		"bot @ab Svc":                                "3-22 characters",
		"team Ops --name signup-team":                "reserved by Mattermost",
		"team-guest not-an-email --channels ~guests": "valid guest email",
		"webhook-outgoing ~guests Relay":             "callback URL is required",
	} {
		t.Run(command, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)
			api.On("GetChannelByName", "team1", "guests", false).
				Return(&model.Channel{Id: testChannelID, Name: "guests", TeamId: "team1", Type: model.ChannelTypeOpen}, nil).Maybe()
			api.On("GetUser", "u1").Return(nil, testAppErr("not loaded")).Maybe()

			require.Contains(t, runCommand(p, "/mattermost-request-hub "+command).Text, want)
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}

func TestResolveChannelRefs(t *testing.T) {
	open := &model.Channel{Id: "open1", Name: "alerts", TeamId: "team1", Type: model.ChannelTypeOpen}
	private := &model.Channel{Id: "priv1", Name: "secret", TeamId: "team1", Type: model.ChannelTypePrivate}
	dm := &model.Channel{Id: "dm1", Name: "aaaa__bbbb", Type: model.ChannelTypeDirect}
	group := &model.Channel{Id: "gm1", Name: "0123abcd", Type: model.ChannelTypeGroup}
	elsewhere := &model.Channel{Id: "other1", Name: "elsewhere", TeamId: "team2", Type: model.ChannelTypeOpen}
	setup := func(requester *model.User, member bool) (*Plugin, *plugintest.API) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		api.On("GetChannelByName", "team1", "aaaa__bbbb", false).Return(dm, nil).Maybe()
		api.On("GetChannelByName", "team1", "0123abcd", false).Return(group, nil).Maybe()
		api.On("GetChannelByName", "team1", "elsewhere", false).Return(elsewhere, nil).Maybe()
		api.On("GetChannelByName", "team1", "broken", false).Return(nil, testAppErr("database down")).Maybe()
		api.On("GetChannelByName", "team1", "alerts", false).Return(open, nil).Maybe()
		api.On("GetChannelByName", "team1", "secret", false).Return(private, nil).Maybe()
		api.On("GetChannelByName", "team1", "missing", false).Return(nil, notFoundErr()).Maybe()
		if member {
			api.On("GetChannelMember", "priv1", "u1").Return(&model.ChannelMember{}, nil).Maybe()
		} else {
			api.On("GetChannelMember", "priv1", "u1").Return(nil, testAppErr("not a member")).Maybe()
		}
		if requester != nil {
			api.On("GetUser", "u1").Return(requester, nil).Maybe()
		} else {
			api.On("GetUser", "u1").Return(nil, testAppErr("not loaded")).Maybe()
		}
		return p, api
	}
	regular := &model.User{Id: "u1", Roles: "system_user"}
	admin := &model.User{Id: "u1", Roles: "system_user system_admin"}

	t.Run("open and blank", func(t *testing.T) {
		p, api := setup(regular, false)
		ids, err := p.resolveChannelRefs("u1", "team1", []string{"~Alerts", "~", " ", "alerts"})
		require.NoError(t, err)
		require.Equal(t, []string{"open1", "open1"}, ids)
		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("private and a member", func(t *testing.T) {
		p, api := setup(regular, true)
		ids, err := p.resolveChannelRefs("u1", "team1", []string{"~secret"})
		require.NoError(t, err)
		require.Equal(t, []string{"priv1"}, ids)
		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("private and a System Admin", func(t *testing.T) {
		p, api := setup(admin, false)
		ids, err := p.resolveChannelRefs("u1", "team1", []string{"~secret", "~secret"})
		require.NoError(t, err)
		require.Equal(t, []string{"priv1", "priv1"}, ids)
		api.AssertNumberOfCalls(t, "GetUser", 1)
	})

	// Direct and group messages match a name lookup in any team, and a
	// System Admin can't name them either.
	for _, ref := range []string{"aaaa__bbbb", "0123abcd", "elsewhere"} {
		t.Run("not a channel of the team: "+ref, func(t *testing.T) {
			p, api := setup(admin, true)
			_, err := p.resolveChannelRefs("u1", "team1", []string{"~" + ref})
			require.EqualError(t, err, "unknown channel: ~"+ref)
			api.AssertNotCalled(t, "GetChannelMember", mock.Anything, mock.Anything)
		})
	}

	t.Run("lookup failure is logged", func(t *testing.T) {
		p, api := setup(regular, false)
		_, err := p.resolveChannelRefs("u1", "team1", []string{"~broken"})
		require.EqualError(t, err, "unknown channel: ~broken")
		api.AssertCalled(t, "LogWarn", "failed to look up channel by name", "team_id", "team1", "name", "broken", "error", mock.Anything)
	})

	t.Run("missing is not logged", func(t *testing.T) {
		p, api := setup(regular, false)
		_, err := p.resolveChannelRefs("u1", "team1", []string{"~missing"})
		require.Error(t, err)
		api.AssertNotCalled(t, "LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	// A private channel the requester can't see reads exactly like one
	// that doesn't exist.
	for name, requester := range map[string]*model.User{"private and not a member": regular, "requester not loaded": nil} {
		t.Run(name, func(t *testing.T) {
			p, _ := setup(requester, false)
			_, hiddenErr := p.resolveChannelRefs("u1", "team1", []string{"~secret"})
			_, missingErr := p.resolveChannelRefs("u1", "team1", []string{"~missing"})
			require.EqualError(t, hiddenErr, "unknown channel: ~secret")
			require.EqualError(t, missingErr, "unknown channel: ~missing")
		})
	}

	t.Run("too many", func(t *testing.T) {
		p, api := setup(regular, false)
		_, err := p.resolveChannelRefs("u1", "team1", make([]string, maxMembersPerList+1))
		require.EqualError(t, err, "too many channels: at most 100 per list")
		api.AssertNotCalled(t, "GetChannelByName", mock.Anything, mock.Anything, mock.Anything)
	})
}

func TestCommandHelpText_HidesTurnedOffTypes(t *testing.T) {
	all := commandHelpText(newTestPlugin(&plugintest.API{}).getConfiguration())
	for _, row := range helpRows {
		require.Contains(t, all, "`/"+commandTrigger+" "+row.usage+"`", row.usage)
	}
	require.Contains(t, all, "Separate users or channels with spaces or commas.")

	none := commandHelpText(&configuration{})
	require.Contains(t, none, "`/"+commandTrigger+" docs`")
	require.Contains(t, none, "`/"+commandTrigger+" help`")
	for _, row := range helpRows {
		if row.requestType != "" {
			require.NotContains(t, none, "`/"+commandTrigger+" "+row.usage+"`", row.usage)
		}
	}

	cfg := *newTestPlugin(&plugintest.API{}).getConfiguration()
	cfg.AllowBotTokenRequests = false
	some := commandHelpText(&cfg)
	require.NotContains(t, some, "`/"+commandTrigger+" bot")
	require.Contains(t, some, "`/"+commandTrigger+" webhook-incoming`")
}

func notFoundErr() *model.AppError {
	return model.NewAppError("test", "test.not_found", nil, "not found", http.StatusNotFound)
}

// A private channel the requester isn't in, a direct message and a missing
// channel get the same reply from every subcommand that names channels.
func TestExecuteCommand_HiddenChannelsReadAsUnknown(t *testing.T) {
	for _, form := range []string{"team-guest a@b.co --channels ~%s", "webhook-incoming ~%s Alerts", "webhook-outgoing ~%s Relay --callback https://hooks.example.com"} {
		t.Run(strings.Fields(form)[0], func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			p := newTestPlugin(api)
			api.On("GetChannelByName", "team1", "secret", false).
				Return(&model.Channel{Id: "priv1", Name: "secret", TeamId: "team1", Type: model.ChannelTypePrivate}, nil)
			api.On("GetChannelByName", "team1", "aaaa__bbbb", false).
				Return(&model.Channel{Id: "dm1", Name: "aaaa__bbbb", Type: model.ChannelTypeDirect}, nil)
			api.On("GetChannelByName", "team1", "missing", false).Return(nil, notFoundErr())
			api.On("GetChannelMember", "priv1", "u1").Return(nil, notFoundErr())
			api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)

			reply := func(name string) string {
				return strings.ReplaceAll(runCommand(p, "/mattermost-request-hub "+fmt.Sprintf(form, name)).Text, name, "NAME")
			}
			require.Equal(t, "unknown channel: ~NAME", reply("missing"))
			require.Equal(t, reply("missing"), reply("secret"))
			require.Equal(t, reply("missing"), reply("aaaa__bbbb"))
			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}
