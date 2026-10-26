package main

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"unicode"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

const commandTrigger = "mattermost-request-hub"

// fieldNominees is the dialog element name for the nominees selector in the
// channel-admin slash-command dialog (distinct from fieldMembers used by the
// channel-creation dialog).
const fieldNominees = "nominees"

// fieldChannelID is the dialog element name for the channel selector in the
// channel-admin slash-command dialog.
const fieldChannelID = "channel_id"

// channelAdminDialogCallbackID identifies submissions from the channel-admin
// slash-command dialog (distinct from the webapp path which uses JSON directly).
const channelAdminDialogCallbackID = "channel_admin_request"

// Arguments that each request subcommand accepts in place of the form, and
// the `prefixes` subcommand. Shared by the autocomplete hints and the usage
// table so they can't drift apart.
const (
	channelArgsHint         = "<prefix> <Channel name> [--suffix <suffix>] [--purpose <text>] [--private] [--members @user ...] [--admins @user ...] [--category <name>]"
	channelAdminArgsHint    = "@user [@user ...]"
	teamAdminArgsHint       = "@user [@user ...]"
	teamArgsHint            = "<Team name> [--name <url name>] [--description <text>] [--invite-only] [--team-admin]"
	teamMemberArgsHint      = "@user [@user ...] [--reason <text>]"
	teamGuestArgsHint       = "<email> --channels ~channel [~channel ...] [--reason <text>]"
	botArgsHint             = "<username> <Display name> [--description <text>] [--owner @user]"
	incomingWebhookArgsHint = "~channel <Display name> [--description <text>]"
	outgoingWebhookArgsHint = "~channel <Display name> --callback <url> [--description <text>]"
	subcommandPrefixes      = "prefixes"
	subcommandList          = "list"
	listApprovals           = "approvals"
	subcommandApprove       = "approve"
	subcommandDeny          = "deny"
	subcommandExercise      = "exercise"
	requestIDArgHint        = "<request ID>"
)

// notConfiguredMessage is shown by every channel request entry point when
// no prefix is configured.
const notConfiguredMessage = "Channel requests aren't configured yet. Ask a System Admin to define at least one channel prefix in System Console → Plugins → Mattermost Request Hub."

func getCommand() *model.Command {
	return &model.Command{
		Trigger:          commandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Request admin-gated permissions (channels, teams, admin roles) or open docs",
		AutoCompleteHint: "[docs|help|channel|channel-admin|prefixes|list|approve|deny|exercise|team-admin|team|team-member|team-guest|bot|webhook-incoming|webhook-outgoing]",
		AutocompleteData: getAutocompleteData(),
		DisplayName:      "Mattermost Request Hub",
	}
}

func getAutocompleteData() *model.AutocompleteData {
	root := model.NewAutocompleteData(commandTrigger, "[subcommand]", "Request admin-gated access via Request Hub")
	root.AddCommand(model.NewAutocompleteData("docs", "", "Open the full documentation guide in your browser"))
	root.AddCommand(model.NewAutocompleteData("help", "", "Show the subcommands and their arguments"))
	root.AddCommand(model.NewAutocompleteData("channel", "["+channelArgsHint+"]", "Request creation of a new channel (no arguments opens the form)"))
	root.AddCommand(model.NewAutocompleteData("channel-admin", "["+channelAdminArgsHint+"]", "Request Channel Admin role (no arguments opens the form; with users, for this channel)"))
	root.AddCommand(model.NewAutocompleteData(subcommandPrefixes, "", "List the channel prefixes you can use"))
	list := model.NewAutocompleteData(subcommandList, "["+listApprovals+"]", "List your pending requests and their IDs, or with approvals, the requests you can approve")
	list.AddCommand(model.NewAutocompleteData(listApprovals, "", "List the pending requests you can approve and their IDs"))
	root.AddCommand(list)
	root.AddCommand(model.NewAutocompleteData(subcommandApprove, requestIDArgHint, "Approve a request by the ID on its approval card"))
	root.AddCommand(model.NewAutocompleteData(subcommandDeny, requestIDArgHint, "Deny a request by the ID on its approval card"))
	exercise := model.NewAutocompleteData(subcommandExercise, "populate|teardown|status", "Seed, remove or report exercise data (System Admins, exercise mode on)")
	exercise.AddCommand(model.NewAutocompleteData(exercisePopulate, "", "Seed marked exercise data into this team"))
	exercise.AddCommand(model.NewAutocompleteData(exerciseTeardown, "", "Remove this team's exercise data"))
	exercise.AddCommand(model.NewAutocompleteData(exerciseStatus, "", "Compare recorded and live exercise data"))
	root.AddCommand(exercise)
	root.AddCommand(model.NewAutocompleteData("team-admin", "["+teamAdminArgsHint+"]", "Request Team Admin role in the current team (no arguments opens the form)"))
	root.AddCommand(model.NewAutocompleteData("team", "["+teamArgsHint+"]", "Request creation of a new team (no arguments opens the form)"))
	root.AddCommand(model.NewAutocompleteData("team-member", "["+teamMemberArgsHint+"]", "Request adding users or bots to the current team (no arguments opens the form)"))
	root.AddCommand(model.NewAutocompleteData("team-guest", "["+teamGuestArgsHint+"]", "Request a guest invite to the current team (no arguments opens the form)"))
	root.AddCommand(model.NewAutocompleteData("bot", "["+botArgsHint+"]", "Request creation of a bot account (no arguments opens the form)"))
	root.AddCommand(model.NewAutocompleteData("webhook-incoming", "["+incomingWebhookArgsHint+"]", "Request an incoming webhook (external → Mattermost; no arguments opens the form)"))
	root.AddCommand(model.NewAutocompleteData("webhook-outgoing", "["+outgoingWebhookArgsHint+"]", "Request an outgoing webhook (Mattermost → external; no arguments opens the form)"))
	return root
}

func (p *Plugin) ExecuteCommand(_ *plugin.Context, args *model.CommandArgs) (resp *model.CommandResponse, appErr *model.AppError) {
	// Never let a panic deactivate the whole plugin — Mattermost surfaces
	// "command crashed the … plugin" / "Plugin for /… is not working" after that.
	defer func() {
		if r := recover(); r != nil {
			p.API.LogError("channel-requests: ExecuteCommand panic recovered",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			resp = ephemeralResponse("Something went wrong running that command. The error was logged — please try again or contact a System Admin.")
			appErr = nil
		}
	}()

	if args == nil {
		return ephemeralResponse("Could not run that command (missing arguments)."), nil
	}

	words, err := splitCommandArgs(args.Command)
	if err != nil {
		return ephemeralResponse(fmt.Sprintf("Could not read the command: %s.", err.Error())), nil
	}
	subcommand := ""
	if len(words) >= 2 {
		subcommand = strings.ToLower(words[1].Text)
	}

	var params []commandArg
	if len(words) > 2 {
		params = words[2:]
	}

	switch subcommand {
	case "docs", "documentation":
		return p.executeDocsCommand(args)
	case "help":
		return ephemeralResponse(commandHelpText(p.getConfiguration()) + "\n\n" + p.docsLinkText(args)), nil
	case "channel", "":
		if len(params) > 0 {
			return p.executeChannelCreateCommand(args, params), nil
		}
		return p.executeChannelRequestCommand(args)
	case "channel-admin":
		if len(params) > 0 {
			return p.executeChannelAdminNominateCommand(args, params), nil
		}
		return p.executeChannelAdminRequestCommand(args)
	case subcommandPrefixes:
		return p.executePrefixesCommand(), nil
	case subcommandList:
		return p.executeList(args, params), nil
	case subcommandApprove:
		return p.executeDecision(args, params, true), nil
	case subcommandDeny:
		return p.executeDecision(args, params, false), nil
	case subcommandExercise:
		return p.executeExercise(args, params), nil
	case "team-admin":
		if len(params) > 0 {
			return p.executeTeamAdminNominateCommand(args, params), nil
		}
		return p.executeTeamAdminRequestCommand(args)
	case "team":
		if len(params) > 0 {
			return p.executeTeamCreateCommand(args, params), nil
		}
		return p.executeTeamCreationRequestCommand(args)
	case "team-member":
		if len(params) > 0 {
			return p.executeTeamMemberAddCommand(args, params), nil
		}
		return p.executeTeamMemberRequestCommand(args)
	case "team-guest":
		if len(params) > 0 {
			return p.executeTeamGuestInviteCommand(args, params), nil
		}
		return p.executeTeamGuestRequestCommand(args)
	case "bot":
		if len(params) > 0 {
			return p.executeBotCreateCommand(args, params), nil
		}
		return p.executeBotRequestCommand(args)
	case "webhook-incoming":
		if len(params) > 0 {
			return p.executeWebhookCreateCommand(args, webhookTypeIncoming, params), nil
		}
		return p.executeIncomingWebhookRequestCommand(args)
	case "webhook-outgoing":
		if len(params) > 0 {
			return p.executeWebhookCreateCommand(args, webhookTypeOutgoing, params), nil
		}
		return p.executeOutgoingWebhookRequestCommand(args)
	default:
		return ephemeralResponse(fmt.Sprintf("Unknown subcommand `%s`.\n\n%s", words[1].Text, commandHelpText(p.getConfiguration()))), nil
	}
}

// executeDocsCommand posts an ephemeral markdown link to the bundled HTML
// guide (public/help/docs.html). Do NOT set GotoLocation: Mattermost treats
// same-origin /plugins/... URLs as in-app React routes, which leaves the
// channel view stuck on "Loading..." forever. Markdown links to /plugins/
// open in a new tab as a full document load instead.
func (p *Plugin) executeDocsCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text: "**Documentation**\n\n" +
			p.docsLinkText(args) + "\n\n" +
			"Covers channels, Channel Admin, teams, bots, webhooks, messaging threads, and slash commands.",
	}, nil
}

// docsLinkText is the Markdown link to the bundled guide.
func (p *Plugin) docsLinkText(args *model.CommandArgs) string {
	location := docsPath()
	if args != nil && args.SiteURL != "" {
		location = strings.TrimRight(args.SiteURL, "/") + location
	}
	return "[Open the full guide](" + location + ") (opens in a new tab)."
}

func (p *Plugin) executeChannelRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeChannel) {
		return ephemeralResponse(channelRequestsDisabledMsg), nil
	}
	if !p.getConfiguration().UsesPrefixList() {
		return ephemeralResponse(notConfiguredMessage), nil
	}
	if err := p.openRequestDialog(args.TriggerId, args.TeamId); err != nil {
		p.API.LogError("failed to open channel request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the channel request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

func (p *Plugin) executeChannelAdminRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeChannelAdmin) {
		return ephemeralResponse(channelAdminRequestsDisabledMsg), nil
	}
	if err := p.openChannelAdminRequestDialog(args.TriggerId); err != nil {
		p.API.LogError("failed to open channel-admin request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the Channel Admin request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

func (p *Plugin) executeTeamAdminRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeTeamAdmin) {
		return ephemeralResponse(teamAdminRequestsDisabledMsg), nil
	}
	if err := p.openTeamAdminRequestDialog(args.TriggerId, args.TeamId); err != nil {
		p.API.LogError("failed to open team-admin request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the Team Admin request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

func (p *Plugin) executeTeamCreationRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeTeam) {
		return ephemeralResponse(teamRequestsDisabledMsg), nil
	}
	if err := p.openTeamCreationRequestDialog(args.TriggerId); err != nil {
		p.API.LogError("failed to open team creation request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the team creation request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

func (p *Plugin) executeTeamMemberRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeTeamMember) {
		return ephemeralResponse(teamMemberRequestsDisabledMsg), nil
	}
	if err := p.openTeamMemberRequestDialog(args.TriggerId, args.TeamId); err != nil {
		p.API.LogError("failed to open team-member request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the team member request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

func (p *Plugin) executeTeamGuestRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeTeamGuest) {
		return ephemeralResponse(teamGuestRequestsDisabledMsg), nil
	}
	if err := p.openTeamGuestRequestDialog(args.TriggerId, args.TeamId); err != nil {
		p.API.LogError("failed to open team-guest request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the guest invite request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

// openChannelAdminRequestDialog opens a slash-command-driven dialog for
// requesting Channel Admin promotion. Uses the existing submitAdminRequest
// backend — the dialog submission handler adapts the dialog format.
func (p *Plugin) openChannelAdminRequestDialog(triggerID string) error {
	dialog := model.Dialog{
		CallbackId:       channelAdminDialogCallbackID,
		Title:            "Request Channel Admin Promotion",
		IntroductionText: "Nominate users for Channel Admin on an existing channel. Requires admin approval.",
		SubmitLabel:      "Submit request",
		Elements: []model.DialogElement{
			{
				DisplayName: "Channel",
				Name:        fieldChannelID,
				Type:        "select",
				DataSource:  "channels",
				HelpText:    "Select the channel you want to request Channel Admin access for.",
			},
			{
				DisplayName: "Users to promote",
				Name:        fieldNominees,
				Type:        "select",
				DataSource:  "users",
				MultiSelect: true,
			},
		},
	}
	if appErr := p.API.OpenInteractiveDialog(model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s%s", manifest.Id, routeChannelAdminDialog),
		Dialog:    dialog,
	}); appErr != nil {
		return appErr
	}
	return nil
}

func (p *Plugin) executeBotRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeBot) {
		return ephemeralResponse(botRequestsDisabledMsg), nil
	}
	if err := p.openBotRequestDialog(args.TriggerId); err != nil {
		p.API.LogError("failed to open bot request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the bot request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

func (p *Plugin) executeIncomingWebhookRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeWebhook) {
		return ephemeralResponse(webhookRequestsDisabledMsg), nil
	}
	if err := p.openIncomingWebhookDialog(args.TriggerId); err != nil {
		p.API.LogError("failed to open incoming webhook request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the incoming webhook request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

func (p *Plugin) executeOutgoingWebhookRequestCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.getConfiguration().RequestEnabled(requestTypeWebhook) {
		return ephemeralResponse(webhookRequestsDisabledMsg), nil
	}
	if err := p.openOutgoingWebhookDialog(args.TriggerId); err != nil {
		p.API.LogError("failed to open outgoing webhook request dialog", "error", err.Error())
		return ephemeralResponse("Could not open the outgoing webhook request form. Please try again."), nil
	}
	return &model.CommandResponse{}, nil
}

// helpRow is one line of the usage table, shown while its request type is
// turned on. An empty requestType is always shown.
type helpRow struct {
	requestType string
	usage       string
	what        string
}

var helpRows = []helpRow{
	{"", "docs", "Open the full documentation guide"},
	{"", "help", "Show this table"},
	{requestTypeChannel, "channel", "Request a new channel (opens the form)"},
	{requestTypeChannel, "channel " + channelArgsHint, "Request a new channel without the form"},
	{requestTypeChannelAdmin, "channel-admin", "Request Channel Admin role (opens the form)"},
	{requestTypeChannelAdmin, "channel-admin " + channelAdminArgsHint, "Request Channel Admin for those members of this channel"},
	{requestTypeChannel, subcommandPrefixes, "List the channel prefixes you can use"},
	{"", subcommandList, "List your pending requests and their IDs"},
	{"", subcommandList + " " + listApprovals, "List the pending requests you can approve and their IDs"},
	{"", subcommandApprove + " " + requestIDArgHint, "Approve a request, as its Approve button would"},
	{"", subcommandDeny + " " + requestIDArgHint, "Deny a request, as its Deny button would"},
	{"", subcommandExercise + " <populate, teardown or status>", "Seed (`populate`), remove (`teardown`) or report (`status`) marked exercise data in this team (System Admins, when exercise mode is on)"},
	{requestTypeTeamAdmin, "team-admin", "Request Team Admin role in this team (opens the form)"},
	{requestTypeTeamAdmin, "team-admin " + teamAdminArgsHint, "Request Team Admin for those members of this team"},
	{requestTypeTeam, "team", "Request a new team (opens the form)"},
	{requestTypeTeam, "team " + teamArgsHint, "Request a new team without the form"},
	{requestTypeTeamMember, "team-member", "Request adding users or bots to this team (opens the form)"},
	{requestTypeTeamMember, "team-member " + teamMemberArgsHint, "Request adding those users or bots to this team"},
	{requestTypeTeamGuest, "team-guest", "Request a guest invite to this team (opens the form)"},
	{requestTypeTeamGuest, "team-guest " + teamGuestArgsHint, "Request a guest invite to those channels of this team"},
	{requestTypeBot, "bot", "Request a bot account (opens the form)"},
	{requestTypeBot, "bot " + botArgsHint, "Request a bot account without the form"},
	{requestTypeWebhook, "webhook-incoming", "Request an incoming webhook (opens the form)"},
	{requestTypeWebhook, "webhook-incoming " + incomingWebhookArgsHint, "Request an incoming webhook posting to that channel"},
	{requestTypeWebhook, "webhook-outgoing", "Request an outgoing webhook (opens the form)"},
	{requestTypeWebhook, "webhook-outgoing " + outgoingWebhookArgsHint, "Request an outgoing webhook for that channel"},
}

// commandHelpText is the usage table, leaving out request types an admin
// has turned off.
func commandHelpText(config *configuration) string {
	var b strings.Builder
	b.WriteString("**Mattermost Request Hub**: request admin-gated access.\n\n")
	b.WriteString("| Subcommand | What it does |\n|---|---|")
	for _, row := range helpRows {
		if row.requestType != "" && !config.RequestEnabled(row.requestType) {
			continue
		}
		fmt.Fprintf(&b, "\n| `/%s %s` | %s |", commandTrigger, row.usage, row.what)
	}
	b.WriteString("\n\nSeparate users or channels with spaces or commas. Put quotes around text that starts with `--`.")
	return b.String()
}

// channelCreateInput is the parsed form of `channel <prefix> <Channel name> ...`.
type channelCreateInput struct {
	Prefix      string
	DisplayName string
	Suffix      string
	Purpose     string
	Private     bool
	Members     []string
	Admins      []string
	Category    string
}

// commandArg is one word of a slash command. Quoted is set when any part of
// the word was inside quote marks; a quoted word is never read as a flag.
type commandArg struct {
	Text   string
	Quoted bool
}

func (a commandArg) isFlag() bool {
	_, ok := a.flagName()
	return ok
}

// flagName returns the word as a flag. A leading em or en dash, which
// autocorrect types in place of `--`, reads as `--` when a name follows it.
func (a commandArg) flagName() (string, bool) {
	if a.Quoted {
		return "", false
	}
	if strings.HasPrefix(a.Text, "--") {
		return a.Text, true
	}
	for _, dash := range []string{"—", "–"} {
		if rest, ok := strings.CutPrefix(a.Text, dash); ok && rest != "" {
			return "--" + rest, true
		}
	}
	return "", false
}

// isQuoteMark reports whether r opens or closes a quoted span. Curly quotes
// count too, because macOS and iOS autocorrect type them.
func isQuoteMark(r rune) bool {
	return r == '"' || r == '“' || r == '”'
}

// splitCommandArgs splits a command into words like strings.Fields, except
// that a word starting with a quote mark runs to the next quote mark that
// ends a word, and loses its quotes. Quote marks inside a word are kept.
func splitCommandArgs(s string) ([]commandArg, error) {
	runes := []rune(s)
	endsWord := func(i int) bool { return i == len(runes) || unicode.IsSpace(runes[i]) }

	var words []commandArg
	for i := 0; i < len(runes); {
		switch {
		case unicode.IsSpace(runes[i]):
			i++
		case isQuoteMark(runes[i]):
			end := -1
			for j := i + 1; j < len(runes); j++ {
				if isQuoteMark(runes[j]) && endsWord(j+1) {
					end = j
					break
				}
			}
			if end < 0 {
				return nil, errors.New("a quote mark is not closed")
			}
			words = append(words, commandArg{Text: string(runes[i+1 : end]), Quoted: true})
			i = end + 1
		default:
			j := i
			for !endsWord(j) {
				j++
			}
			words = append(words, commandArg{Text: string(runes[i:j])})
			i = j
		}
	}
	return words, nil
}

// readArgs splits params into the words before the first flag and the
// flags after them. Each flag takes the words up to the next flag. A flag
// that is not in allowed, or is given twice, is an error.
func readArgs(params []commandArg, allowed ...string) ([]string, map[string][]string, error) {
	i := 0
	var positional []string
	for ; i < len(params) && !params[i].isFlag(); i++ {
		positional = append(positional, params[i].Text)
	}

	flags := map[string][]string{}
	for i < len(params) {
		flag, _ := params[i].flagName()
		i++
		values := []string{}
		for ; i < len(params) && !params[i].isFlag(); i++ {
			values = append(values, params[i].Text)
		}
		if !slices.Contains(allowed, flag) {
			return nil, nil, errors.Errorf("unknown option `%s`", flag)
		}
		if _, seen := flags[flag]; seen {
			return nil, nil, errors.Errorf("`%s` is given more than once", flag)
		}
		flags[flag] = values
	}
	return positional, flags, nil
}

// flagOne returns the single value of flag, or "" when it wasn't given.
func flagOne(flags map[string][]string, flag string) (string, error) {
	values, ok := flags[flag]
	if !ok {
		return "", nil
	}
	if len(values) != 1 {
		return "", errors.Errorf("`%s` takes exactly one value", flag)
	}
	return values[0], nil
}

// flagText joins the words of flag, or returns "" when it wasn't given.
// need names what is missing when the flag is given with no words.
func flagText(flags map[string][]string, flag, need string) (string, error) {
	values, ok := flags[flag]
	if !ok {
		return "", nil
	}
	text := strings.TrimSpace(strings.Join(values, " "))
	if text == "" {
		return "", errors.Errorf("`%s` needs %s", flag, need)
	}
	return text, nil
}

// flagBool reports whether flag was given; it takes no value.
func flagBool(flags map[string][]string, flag string) (bool, error) {
	values, ok := flags[flag]
	if ok && len(values) != 0 {
		return false, errors.Errorf("`%s` takes no value", flag)
	}
	return ok, nil
}

// splitListArgs splits each word on commas so `@a,@b`, `@a, @b` and
// `@a @b` name the same users, and drops empty entries.
func splitListArgs(words []string) []string {
	var out []string
	for _, w := range words {
		for part := range strings.SplitSeq(w, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// usageReply is the reply for arguments that can't be read.
func usageReply(err error, subcommand, hint string) *model.CommandResponse {
	return ephemeralResponse(fmt.Sprintf("Could not read the request: %s.\nUsage: `/%s %s %s`", err.Error(), commandTrigger, subcommand, hint))
}

// submitReply is the reply to a submit call: its message, or its error.
func submitReply(message string, err error) *model.CommandResponse {
	if err != nil {
		return ephemeralResponse(err.Error())
	}
	return ephemeralResponse(message)
}

// parseChannelCreateArgs parses `<prefix> <Channel name...> [--flag values...]`.
func parseChannelCreateArgs(params []commandArg) (channelCreateInput, error) {
	var in channelCreateInput
	positional, flags, err := readArgs(params, "--suffix", "--purpose", "--private", "--members", "--admins", "--category")
	if err != nil {
		return in, err
	}
	if len(positional) < 2 {
		return in, errors.New("a prefix and a channel name are required")
	}
	in.Prefix = positional[0]
	in.DisplayName = strings.Join(positional[1:], " ")

	if in.Suffix, err = flagOne(flags, "--suffix"); err != nil {
		return in, err
	}
	if in.Purpose, err = flagText(flags, "--purpose", "some text"); err != nil {
		return in, err
	}
	if in.Private, err = flagBool(flags, "--private"); err != nil {
		return in, err
	}
	if in.Category, err = flagText(flags, "--category", "a name"); err != nil {
		return in, err
	}
	in.Members = splitListArgs(flags["--members"])
	in.Admins = splitListArgs(flags["--admins"])
	return in, nil
}

// executeChannelCreateCommand submits a channel request without the form.
// It goes through submitRequest like the dialog does, and like every
// slash-command entry point it always queues the request for approval.
func (p *Plugin) executeChannelCreateCommand(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	config := p.getConfiguration()
	if !config.RequestEnabled(requestTypeChannel) {
		return ephemeralResponse(channelRequestsDisabledMsg)
	}
	if !config.UsesPrefixList() {
		return ephemeralResponse(notConfiguredMessage)
	}

	parsed, err := parseChannelCreateArgs(params)
	if err != nil {
		return usageReply(err, "channel", channelArgsHint)
	}

	memberIDs, err := p.resolveUsernameList(parsed.Members)
	if err != nil {
		return ephemeralResponse(err.Error())
	}
	adminMemberIDs, err := p.resolveUsernameList(parsed.Admins)
	if err != nil {
		return ephemeralResponse(err.Error())
	}

	channelType := channelTypeOpen
	if parsed.Private {
		channelType = channelTypePrivate
	}

	return submitReply(p.submitRequest(requestInput{
		RequesterID:    args.UserId,
		TeamID:         args.TeamId,
		DisplayName:    parsed.DisplayName,
		Name:           parsed.Suffix,
		Prefix:         parsed.Prefix,
		Purpose:        parsed.Purpose,
		ChannelType:    channelType,
		MemberIDs:      memberIDs,
		AdminMemberIDs: adminMemberIDs,
		CategoryName:   parsed.Category,
	}))
}

// executeChannelAdminNominateCommand requests Channel Admin for the named
// users in the channel the command was run from. Same path as the
// Channel Admin dialog, so it always queues for approval.
func (p *Plugin) executeChannelAdminNominateCommand(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	if !p.getConfiguration().RequestEnabled(requestTypeChannelAdmin) {
		return ephemeralResponse(channelAdminRequestsDisabledMsg)
	}

	positional, _, err := readArgs(params)
	if err != nil {
		return usageReply(err, "channel-admin", channelAdminArgsHint)
	}
	nomineeIDs, err := p.resolveUsernameList(splitListArgs(positional))
	if err != nil {
		return ephemeralResponse(err.Error())
	}
	if len(nomineeIDs) == 0 {
		return ephemeralResponse(fmt.Sprintf("Name at least one channel member: `/%s channel-admin %s`", commandTrigger, channelAdminArgsHint))
	}

	return submitReply(p.submitAdminRequest(args.UserId, args.ChannelId, nomineeIDs))
}

// executeTeamAdminNominateCommand requests Team Admin for the named users in
// the team the command was run from, through the same submit as the form.
func (p *Plugin) executeTeamAdminNominateCommand(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	if !p.getConfiguration().RequestEnabled(requestTypeTeamAdmin) {
		return ephemeralResponse(teamAdminRequestsDisabledMsg)
	}

	positional, _, err := readArgs(params)
	if err != nil {
		return usageReply(err, "team-admin", teamAdminArgsHint)
	}
	nomineeIDs, err := p.resolveUsernameList(splitListArgs(positional))
	if err != nil {
		return ephemeralResponse(err.Error())
	}
	if len(nomineeIDs) == 0 {
		return usageReply(errors.New("name at least one team member"), "team-admin", teamAdminArgsHint)
	}

	return submitReply(p.submitTeamAdminRequest(args.UserId, args.TeamId, nomineeIDs))
}

// executeTeamCreateCommand submits a new team request without the form.
func (p *Plugin) executeTeamCreateCommand(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	if !p.getConfiguration().RequestEnabled(requestTypeTeam) {
		return ephemeralResponse(teamRequestsDisabledMsg)
	}

	in, err := parseTeamCreateArgs(params)
	if err != nil {
		return usageReply(err, "team", teamArgsHint)
	}
	in.RequesterID = args.UserId
	return submitReply(p.submitTeamCreationRequest(in))
}

// parseTeamCreateArgs parses `<Team name...> [--flag values...]`.
func parseTeamCreateArgs(params []commandArg) (teamCreationInput, error) {
	in := teamCreationInput{Type: model.TeamOpen}
	positional, flags, err := readArgs(params, "--name", "--description", "--invite-only", "--team-admin")
	if err != nil {
		return in, err
	}
	in.DisplayName = strings.Join(positional, " ")
	if strings.TrimSpace(in.DisplayName) == "" {
		return in, errors.New("a team name is required")
	}
	if in.Name, err = flagOne(flags, "--name"); err != nil {
		return in, err
	}
	if in.Description, err = flagText(flags, "--description", "some text"); err != nil {
		return in, err
	}
	inviteOnly, err := flagBool(flags, "--invite-only")
	if err != nil {
		return in, err
	}
	if inviteOnly {
		in.Type = model.TeamInvite
	}
	if in.RequestTeamAdmin, err = flagBool(flags, "--team-admin"); err != nil {
		return in, err
	}
	return in, nil
}

// executeTeamMemberAddCommand requests adding the named users to the team
// the command was run from, through the same submit as the form.
func (p *Plugin) executeTeamMemberAddCommand(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	if !p.getConfiguration().RequestEnabled(requestTypeTeamMember) {
		return ephemeralResponse(teamMemberRequestsDisabledMsg)
	}

	positional, flags, err := readArgs(params, "--reason")
	if err != nil {
		return usageReply(err, "team-member", teamMemberArgsHint)
	}
	reason, err := flagText(flags, "--reason", "some text")
	if err != nil {
		return usageReply(err, "team-member", teamMemberArgsHint)
	}
	nomineeIDs, err := p.resolveUsernameList(splitListArgs(positional))
	if err != nil {
		return ephemeralResponse(err.Error())
	}
	if len(nomineeIDs) == 0 {
		return usageReply(errors.New("name at least one user"), "team-member", teamMemberArgsHint)
	}

	return submitReply(p.submitTeamMemberRequest(args.UserId, args.TeamId, nomineeIDs, reason))
}

// executeTeamGuestInviteCommand requests a guest invite to channels of the
// team the command was run from, through the same submit as the form.
func (p *Plugin) executeTeamGuestInviteCommand(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	if !p.getConfiguration().RequestEnabled(requestTypeTeamGuest) {
		return ephemeralResponse(teamGuestRequestsDisabledMsg)
	}

	usage := func(err error) *model.CommandResponse { return usageReply(err, "team-guest", teamGuestArgsHint) }
	positional, flags, err := readArgs(params, "--channels", "--reason")
	if err != nil {
		return usage(err)
	}
	if len(positional) != 1 {
		return usage(errors.New("give one guest email"))
	}
	reason, err := flagText(flags, "--reason", "some text")
	if err != nil {
		return usage(err)
	}
	channelIDs, err := p.resolveChannelRefs(args.UserId, args.TeamId, splitListArgs(flags["--channels"]))
	if err != nil {
		return ephemeralResponse(err.Error())
	}
	if len(channelIDs) == 0 {
		return usage(errors.New("name at least one channel with `--channels`"))
	}

	return submitReply(p.submitTeamGuestRequest(args.UserId, args.TeamId, positional[0], channelIDs, reason))
}

// executeBotCreateCommand submits a bot account request without the form.
// Like the form it takes the account fields only; the token and webhook
// extras are offered in the webapp.
func (p *Plugin) executeBotCreateCommand(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	if !p.getConfiguration().RequestEnabled(requestTypeBot) {
		return ephemeralResponse(botRequestsDisabledMsg)
	}

	usage := func(err error) *model.CommandResponse { return usageReply(err, "bot", botArgsHint) }
	positional, flags, err := readArgs(params, "--description", "--owner")
	if err != nil {
		return usage(err)
	}
	if len(positional) < 2 {
		return usage(errors.New("a bot username and a display name are required"))
	}
	description, err := flagText(flags, "--description", "some text")
	if err != nil {
		return usage(err)
	}
	owner, err := flagOne(flags, "--owner")
	if err != nil {
		return usage(err)
	}

	// No --owner leaves the owner to submitBotRequest's default.
	var ownerID string
	if _, given := flags["--owner"]; given {
		ownerIDs, resolveErr := p.resolveUsernameList([]string{owner})
		if resolveErr != nil {
			return ephemeralResponse(resolveErr.Error())
		}
		if len(ownerIDs) == 0 {
			return usage(errors.New("name the owner as `@user`"))
		}
		ownerID = ownerIDs[0]
	}

	return submitReply(p.submitBotRequest(args.UserId, botRequestInput{
		Username:    positional[0],
		DisplayName: strings.Join(positional[1:], " "),
		Description: description,
		OwnerUserID: ownerID,
	}))
}

// executeWebhookCreateCommand submits an incoming or outgoing webhook
// request without the form.
func (p *Plugin) executeWebhookCreateCommand(args *model.CommandArgs, webhookType string, params []commandArg) *model.CommandResponse {
	if !p.getConfiguration().RequestEnabled(requestTypeWebhook) {
		return ephemeralResponse(webhookRequestsDisabledMsg)
	}

	subcommand, hint, allowed := "webhook-incoming", incomingWebhookArgsHint, []string{"--description"}
	if webhookType == webhookTypeOutgoing {
		subcommand, hint, allowed = "webhook-outgoing", outgoingWebhookArgsHint, []string{"--description", "--callback"}
	}

	usage := func(err error) *model.CommandResponse { return usageReply(err, subcommand, hint) }
	positional, flags, err := readArgs(params, allowed...)
	if err != nil {
		return usage(err)
	}
	if len(positional) < 2 {
		return usage(errors.New("a channel and a display name are required"))
	}
	description, err := flagText(flags, "--description", "some text")
	if err != nil {
		return usage(err)
	}
	callbackURL, err := flagOne(flags, "--callback")
	if err != nil {
		return usage(err)
	}
	channelIDs, err := p.resolveChannelRefs(args.UserId, args.TeamId, positional[:1])
	if err != nil {
		return ephemeralResponse(err.Error())
	}
	if len(channelIDs) == 0 {
		return usage(errors.New("name the channel as `~channel`"))
	}

	return submitReply(p.submitWebhookRequest(args.UserId, webhookType, channelIDs[0],
		strings.Join(positional[1:], " "), description, callbackURL))
}

// resolveChannelRefs resolves `~name` (or bare name) references to public
// and private channels of teamID. A missing channel, a direct or group
// message, and a private channel the requester is not in all get the same
// reply, so the command never confirms that one exists. System Admins can
// name any private channel of the team.
func (p *Plugin) resolveChannelRefs(requesterID, teamID string, refs []string) ([]string, error) {
	if len(refs) > maxMembersPerList {
		return nil, errors.Errorf("too many channels: at most %d per list", maxMembersPerList)
	}
	var requester *model.User
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ref), "~"))
		if name == "" {
			continue
		}
		unknown := errors.Errorf("unknown channel: ~%s", name)
		ch, appErr := p.API.GetChannelByName(teamID, name, false)
		if appErr != nil && appErr.StatusCode != http.StatusNotFound {
			p.API.LogWarn("failed to look up channel by name", "team_id", teamID, "name", name, "error", appErr.Error())
		}
		// The lookup also matches direct and group messages, which have no team.
		if appErr != nil || ch == nil || ch.TeamId != teamID ||
			(ch.Type != model.ChannelTypeOpen && ch.Type != model.ChannelTypePrivate) {
			return nil, unknown
		}
		if ch.Type == model.ChannelTypePrivate {
			if _, memberErr := p.API.GetChannelMember(ch.Id, requesterID); memberErr != nil {
				if requester == nil {
					var userErr *model.AppError
					if requester, userErr = p.API.GetUser(requesterID); userErr != nil {
						p.API.LogWarn("failed to load requesting user", "user_id", requesterID, "error", userErr.Error())
					}
				}
				if requester == nil || !requester.IsSystemAdmin() {
					return nil, unknown
				}
			}
		}
		ids = append(ids, ch.Id)
	}
	return ids, nil
}

// executePrefixesCommand lists the configured prefixes, the same data the
// prefixes endpoint serves to the webapp.
func (p *Plugin) executePrefixesCommand() *model.CommandResponse {
	config := p.getConfiguration()
	if !config.RequestEnabled(requestTypeChannel) {
		return ephemeralResponse(channelRequestsDisabledMsg)
	}
	if !config.UsesPrefixList() {
		return ephemeralResponse(notConfiguredMessage)
	}

	var b strings.Builder
	b.WriteString("Channel prefixes you can use:\n")
	for _, pf := range config.prefixes {
		fmt.Fprintf(&b, "- `%s`", pf.Prefix)
		if pf.Description != "" {
			fmt.Fprintf(&b, " %s", escapeMarkdown(pf.Description))
		}
		if pf.SuffixPatternRaw != "" {
			fmt.Fprintf(&b, " (suffix rule: `%s`)", pf.SuffixPatternRaw)
		}
		b.WriteString("\n")
	}
	return ephemeralResponse(strings.TrimRight(b.String(), "\n"))
}

// executeList lists the user's own pending requests, or with
// "approvals", the pending requests they can approve.
func (p *Plugin) executeList(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	approvals := len(params) == 1 && params[0].Text == listApprovals
	if len(params) > 0 && !approvals {
		return ephemeralResponse(fmt.Sprintf("Use `/%s %s` for your own requests, or `/%s %s %s` for the requests you can approve.",
			commandTrigger, subcommandList, commandTrigger, subcommandList, listApprovals))
	}
	if len(enabledPendingKinds(p.getConfiguration())) == 0 {
		return ephemeralResponse(listRequestsDisabledMsg)
	}

	list, title, none := p.listOwnRequests, "Your pending requests:", "You have no pending requests."
	if approvals {
		list, title, none = p.listApprovableRequests, "Requests you can approve:", "There are no pending requests you can approve."
	}
	requests, more, err := list(args.UserId, maxListedRequests)
	if err != nil {
		p.API.LogError("failed to list pending requests", "user_id", args.UserId, "error", err.Error())
		return ephemeralResponse("Could not load the requests. Please try again.")
	}
	if len(requests) == 0 {
		return ephemeralResponse(none)
	}

	var b strings.Builder
	b.WriteString(title + "\n")
	for _, req := range requests {
		fmt.Fprintf(&b, "- %s\n", req.summaryLine())
	}
	if more > 0 {
		fmt.Fprintf(&b, "- ...and %d more\n", more)
	}
	return ephemeralResponse(strings.TrimRight(b.String(), "\n"))
}

// deciderFor returns the decide function for requests of a pending kind.
func (p *Plugin) deciderFor(kind string) decider {
	switch kind {
	case pendingKindChannel:
		return p.decideChannelRequest
	case pendingKindAdmin:
		return p.decideAdminRequest
	case pendingKindTeam:
		return p.decideTeamRequest
	case pendingKindTeamAdmin:
		return p.decideTeamAdminRequest
	case pendingKindTeamMember:
		return p.decideTeamMemberRequest
	case pendingKindTeamGuest:
		return p.decideTeamGuestRequest
	case pendingKindBot:
		return p.decideBotRequest
	case pendingKindWebhook:
		return p.decideWebhookRequest
	default:
		return nil
	}
}

// executeDecision approves or denies a request of any type by ID, through
// the same checks as the approval card buttons.
func (p *Plugin) executeDecision(args *model.CommandArgs, params []commandArg, approve bool) *model.CommandResponse {
	sub := subcommandDeny
	if approve {
		sub = subcommandApprove
	}
	if len(params) != 1 || !model.IsValidId(params[0].Text) {
		return ephemeralResponse(fmt.Sprintf("Give the request ID from the approval card: `/%s %s %s`", commandTrigger, sub, requestIDArgHint))
	}
	requestID := params[0].Text

	// IDs are unique across every kind, so whichever is stored is the one.
	// A turned-off type is still found, so its requests can be denied.
	for _, kind := range allPendingKinds {
		exists, err := p.pendingRequestExists(kind, requestID)
		if err != nil {
			p.API.LogError("failed to look up request", "request_id", requestID, "error", err.Error())
			return ephemeralResponse("Could not load that request.")
		}
		if exists {
			return ephemeralResponse(p.deciderFor(kind)(args.UserId, requestID, "", approve).replyText())
		}
	}
	return ephemeralResponse(alreadyHandledText)
}

func ephemeralResponse(text string) *model.CommandResponse {
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         text,
	}
}
