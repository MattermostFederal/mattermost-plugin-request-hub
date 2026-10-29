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

func TestExecuteCommand_NotConfiguredDoesNotOpenDialog(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true}) // enabled, but no prefixes

	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{TriggerId: "trig", TeamId: "team1"})

	require.Nil(t, appErr)
	require.NotNil(t, resp)
	require.Contains(t, resp.Text, "aren't configured")
	// A doomed dialog must not be opened when no prefix is configured.
	api.AssertNotCalled(t, "OpenInteractiveDialog", mock.Anything)
}

func TestExecuteCommand_OpensDialogWhenConfigured(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		AllowChannelRequests: true,
		prefixes:             []channelPrefix{{Prefix: "team-"}},
	})

	api.On("OpenInteractiveDialog", mock.Anything).Return(nil)

	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{TriggerId: "trig", TeamId: "team1"})

	require.Nil(t, appErr)
	require.NotNil(t, resp)
	api.AssertCalled(t, "OpenInteractiveDialog", mock.Anything)
}

func TestExecuteCommand_DialogFailureRepliesPrivately(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		AllowChannelRequests: true,
		prefixes:             []channelPrefix{{Prefix: "team-", Description: "Teams"}},
	})

	api.On("OpenInteractiveDialog", mock.Anything).Return(testAppErr("expired trigger"))

	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{TriggerId: "trig", TeamId: "team1"})

	require.Nil(t, appErr)
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Contains(t, resp.Text, "Could not open")
}

func TestExecuteCommand_DocsOpensGuide(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{
		Command:   "/mattermost-request-hub docs",
		SiteURL:   "https://mm.example.com",
		TriggerId: "trig",
	})

	require.Nil(t, appErr)
	require.NotNil(t, resp)
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	// GotoLocation must stay empty — SPA push of /plugins/... HTML is a Loading hang.
	require.Empty(t, resp.GotoLocation)
	require.Contains(t, resp.Text, "Documentation")
	require.Contains(t, resp.Text, "https://mm.example.com/plugins/"+manifest.Id+"/public/help/docs.html")
	require.Contains(t, resp.Text, "Open the full guide")
}

func runCommand(p *Plugin, command string) *model.CommandResponse {
	resp, appErr := p.ExecuteCommand(nil, &model.CommandArgs{
		Command:   command,
		UserId:    "u1",
		TeamId:    "team1",
		ChannelId: "chX",
		TriggerId: "trig",
		SiteURL:   "https://mm.example.com",
	})
	if appErr != nil {
		panic(appErr)
	}
	return resp
}

func TestExecuteCommand_HelpAndUnknownSubcommand(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})

	resp := runCommand(p, "/mattermost-request-hub help")
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.True(t, strings.HasPrefix(resp.Text, commandHelpText(p.getConfiguration())))
	require.Contains(t, resp.Text, "https://mm.example.com/plugins/"+manifest.Id+"/public/help/docs.html")

	resp = runCommand(p, "/mattermost-request-hub bogus")
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Contains(t, resp.Text, "Unknown subcommand bogus.")
	require.Contains(t, resp.Text, commandHelpText(p.getConfiguration()))

	// A backtick can't open live Markdown in the reply.
	resp = runCommand(p, "/mattermost-request-hub x`[click](http://evil.test)")
	require.True(t, strings.HasPrefix(resp.Text, "Unknown subcommand x\\`\\[click\\]\\("), resp.Text)
}

func TestExecuteCommand_BareCommandWithTrailingSpaceOpensDialog(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	api.On("OpenInteractiveDialog", mock.Anything).Return(nil).Twice()

	require.Empty(t, runCommand(p, "/mattermost-request-hub   ").Text)
	require.Empty(t, runCommand(p, "/mattermost-request-hub channel  ").Text)
}

func TestExecuteCommand_ChannelAdminWithoutArgumentsOpensDialog(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelAdminRequests: true})

	api.On("OpenInteractiveDialog", mock.Anything).Return(nil).Once()

	require.Empty(t, runCommand(p, "/mattermost-request-hub channel-admin").Text)
}

func TestExecuteCommand_Prefixes(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	p.setConfiguration(&configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})
	require.Equal(t, channelRequestsDisabledMsg, runCommand(p, "/mattermost-request-hub prefixes").Text)

	p.setConfiguration(&configuration{AllowChannelRequests: true})
	require.Equal(t, notConfiguredMessage, runCommand(p, "/mattermost-request-hub prefixes").Text)

	p.setConfiguration(&configuration{AllowChannelRequests: true, prefixes: []channelPrefix{
		{Prefix: "team-", Description: "Team *channels*"},
		{Prefix: "proj-", SuffixPatternRaw: "^[0-9]{4}$"},
		{Prefix: "x`-", SuffixPatternRaw: "^a`b$"},
	}})
	resp := runCommand(p, "/mattermost-request-hub prefixes")
	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Equal(t, "Channel prefixes you can use:\n"+
		"- `team-` Team \\*channels\\*\n"+
		"- `proj-` (suffix rule: `^[0-9]{4}$`)\n"+
		"- `x-` (suffix rule: `^ab$`)", resp.Text)
}

func TestParseChannelCreateArgs(t *testing.T) {
	in, err := parseChannelCreateArgs(argWords("team- Marketing Team --suffix mkt --purpose Plan the launch --private --members @alice bob --admins @carol --category Launch Work"))
	require.NoError(t, err)
	require.Equal(t, channelCreateInput{
		Prefix:      "team-",
		DisplayName: "Marketing Team",
		Suffix:      "mkt",
		Purpose:     "Plan the launch",
		Private:     true,
		Members:     []string{"@alice", "bob"},
		Admins:      []string{"@carol"},
		Category:    "Launch Work",
	}, in)

	in, err = parseChannelCreateArgs(argWords("team- Marketing"))
	require.NoError(t, err)
	require.Equal(t, channelCreateInput{Prefix: "team-", DisplayName: "Marketing"}, in)

	for args, want := range map[string]string{
		"":                                    "a prefix and a channel name are required",
		"team-":                               "a prefix and a channel name are required",
		"--private Marketing":                 "a prefix and a channel name are required",
		"team- --private":                     "a prefix and a channel name are required",
		"team- Marketing --suffix":            "`--suffix` takes exactly one value",
		"team- Marketing --suffix a b":        "`--suffix` takes exactly one value",
		"team- Marketing --purpose":           "`--purpose` needs some text",
		"team- Marketing --category":          "`--category` needs a name",
		"team- Marketing --private yes":       "`--private` takes no value",
		"team- Marketing --color blue":        "unknown option `--color`",
		"team- Marketing --private --private": "`--private` is given more than once",
	} {
		_, err := parseChannelCreateArgs(argWords(args))
		require.EqualError(t, err, want, args)
	}
}

func TestExecuteCommand_ChannelCreateRespectsToggleAndPrefixes(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	p.setConfiguration(&configuration{prefixes: []channelPrefix{{Prefix: "team-"}}})
	require.Equal(t, channelRequestsDisabledMsg, runCommand(p, "/mattermost-request-hub channel team- Marketing").Text)

	p.setConfiguration(&configuration{AllowChannelRequests: true})
	require.Equal(t, notConfiguredMessage, runCommand(p, "/mattermost-request-hub channel team- Marketing").Text)
}

func TestExecuteCommand_ChannelCreateBadArguments(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	resp := runCommand(p, "/mattermost-request-hub channel team-")

	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Contains(t, resp.Text, "Could not read the request: a prefix and a channel name are required.")
	require.Contains(t, resp.Text, channelArgsHint)
}

func TestExecuteCommand_ChannelCreateUnknownUsers(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))
	api.On("GetUserByUsername", "alice").Return(&model.User{Id: "id_alice"}, nil)

	resp := runCommand(p, "/mattermost-request-hub channel team- Marketing --members @ghost")
	require.Equal(t, "unknown user: ghost", resp.Text)

	resp = runCommand(p, "/mattermost-request-hub channel team- Marketing --members @alice --admins @ghost")
	require.Equal(t, "unknown user: ghost", resp.Text)

	api.AssertNotCalled(t, "GetUser", mock.Anything)
}

func TestExecuteCommand_ChannelCreateSubmitsForApproval(t *testing.T) {
	api := &plugintest.API{}
	stubNamesFree(api)
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	// A System Admin's command still queues, like the form.
	api.On("GetUser", "u1").Return(sysadmin("u1"), nil)
	alice, bob, carol := model.NewId(), model.NewId(), model.NewId()
	for id, name := range map[string]string{alice: "alice", bob: "bob", carol: "carol"} {
		api.On("GetUserByUsername", name).Return(&model.User{Id: id, Username: name}, nil)
		api.On("GetUser", id).Return(&model.User{Id: id, Username: name}, nil).Maybe()
	}
	var stored channelRequest
	api.On("KVSet", isRequestRecordKey, mock.Anything).Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &stored))
	}).Return(nil).Once()
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	mockApprovalChannel(api)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", mock.Anything, mock.Anything, mock.Anything).Return([]*model.TeamMember{}, nil).Maybe()
	api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "post1"}, nil)

	resp := runCommand(p, "/mattermost-request-hub channel team- Marketing  Team --suffix mkt --purpose Plan the launch --private --members @alice bob --admins @carol --category Launch")

	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Contains(t, resp.Text, "submitted for approval")
	require.Equal(t, "u1", stored.RequesterID)
	require.Equal(t, "team1", stored.TeamID)
	require.Equal(t, "team-mkt", stored.Name)
	require.Equal(t, "Marketing Team", stored.DisplayName)
	require.Equal(t, "Plan the launch", stored.Purpose)
	require.Equal(t, channelTypePrivate, stored.ChannelType)
	require.Equal(t, []string{alice, bob}, stored.MemberIDs)
	require.Equal(t, []string{carol}, stored.AdminMemberIDs)
	require.Equal(t, "Launch", stored.CategoryName)
	require.NotZero(t, stored.CreatedAt)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestExecuteCommand_ChannelCreateRejectedBySubmit(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "req", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "team1", "u1").Return(nil, testAppErr("not a member"))

	resp := runCommand(p, "/mattermost-request-hub channel team- Marketing")

	require.Equal(t, "you must be a member of the team to request a channel in it", resp.Text)
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestExecuteCommand_ChannelAdminRespectsToggle(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelRequests: true})

	require.Equal(t, channelAdminRequestsDisabledMsg, runCommand(p, "/mattermost-request-hub channel-admin @nommy").Text)
	require.Equal(t, channelAdminRequestsDisabledMsg, runCommand(p, "/mattermost-request-hub channel-admin").Text)
}

func TestExecuteCommand_ChannelAdminNeedsNominees(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelAdminRequests: true})

	resp := runCommand(p, "/mattermost-request-hub channel-admin @")

	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Contains(t, resp.Text, "Name at least one channel member")
}

func TestExecuteCommand_ChannelAdminUnknownUser(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelAdminRequests: true})

	api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))

	resp := runCommand(p, "/mattermost-request-hub channel-admin @ghost")

	require.Equal(t, "unknown user: ghost", resp.Text)
}

func TestExecuteCommand_ChannelAdminRejectedBySubmit(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelAdminRequests: true})

	api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID}, nil)
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "req", Roles: "system_user"}, nil)
	api.On("GetChannelMember", "chX", "u1").Return(nil, testAppErr("not a member"))

	resp := runCommand(p, "/mattermost-request-hub channel-admin @nommy")

	require.Equal(t, "channel not found, or you are not a member of it", resp.Text)
}

func TestExecuteCommand_ChannelAdminQueuesForCurrentChannel(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowChannelAdminRequests: true, ApprovalTeam: "appteam"})

	// Even a System Admin's command queues for approval, like the dialog.
	api.On("GetUserByUsername", "nommy").Return(&model.User{Id: testNomineeID}, nil)
	api.On("GetUser", "u1").Return(sysadmin("u1"), nil)
	api.On("GetChannel", "chX").Return(&model.Channel{Id: "chX", Name: "marketing", TeamId: "team1", Type: model.ChannelTypeOpen}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("GetChannelMember", "chX", testNomineeID).Return(&model.ChannelMember{}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	mockApprovalChannel(api)
	api.On("GetUsers", mock.Anything).Return([]*model.User{}, nil).Maybe()
	api.On("GetTeamMembers", "team1", mock.Anything, mock.Anything).Return([]*model.TeamMember{}, nil).Maybe()
	var approval *model.Post
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "appch" })).
		Run(func(args mock.Arguments) { approval = args.Get(0).(*model.Post) }).Return(&model.Post{Id: "card1"}, nil)
	api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dmroot"}, nil)

	resp := runCommand(p, "/mattermost-request-hub channel-admin @nommy")

	require.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	require.Contains(t, resp.Text, "submitted for approval")
	require.NotNil(t, approval)
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestGetAutocompleteData_MatchesDispatcherAndHelp(t *testing.T) {
	data := getAutocompleteData()
	require.NoError(t, data.IsValid())
	help := commandHelpText(newTestPlugin(&plugintest.API{}).getConfiguration())

	names := make([]string, 0, len(data.SubCommands))
	for _, sub := range data.SubCommands {
		names = append(names, sub.Trigger)
		require.NotEmpty(t, sub.HelpText, sub.Trigger)
		require.Contains(t, help, "`/"+commandTrigger+" "+sub.Trigger, sub.Trigger)
		require.Contains(t, getCommand().AutoCompleteHint, sub.Trigger, sub.Trigger)
	}
	require.Contains(t, names, subcommandPrefixes)
	require.Contains(t, names, "help")
	require.Contains(t, names, subcommandList)
	require.Contains(t, names, subcommandApprove)
	require.Contains(t, names, subcommandDeny)

	// Every listed subcommand is handled, none falls through to the
	// unknown-subcommand reply.
	api := &plugintest.API{}
	stubLogs(api)
	api.On("OpenInteractiveDialog", mock.Anything).Return(nil).Maybe()
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})
	for _, name := range names {
		require.NotContains(t, runCommand(p, "/"+commandTrigger+" "+name).Text, "Unknown subcommand", name)
	}
}

// argWords splits s the way ExecuteCommand splits the words after the
// subcommand.
func argWords(s string) []commandArg {
	words, err := splitCommandArgs(s)
	if err != nil {
		panic(err)
	}
	return words
}
