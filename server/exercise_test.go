package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	exerciseTeamID = "teamexercise000000000000aa"
	// personaSuffix is what exercisePersonaUsername takes from exerciseTeamID.
	personaSuffix = "-teamexer"
)

// counterIDs returns a newID that yields id-1, id-2, ...
func counterIDs() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("id-%d", n)
	}
}

func builtInScenario(t *testing.T) *exerciseScenario {
	t.Helper()
	sc, err := loadExerciseScenario("")
	require.NoError(t, err)
	return sc
}

func TestBuildExercisePlan_BuiltInScenario(t *testing.T) {
	now := int64(1_800_000_000_000)
	prefixes := []channelPrefix{{Prefix: "team-"}}
	plan, err := buildExercisePlan(builtInScenario(t), exerciseTeamID, prefixes, now, counterIDs())
	require.NoError(t, err)

	require.Equal(t, "silver-harbor", plan.Scenario)
	require.Len(t, plan.Personas, 7)
	require.Len(t, plan.Channels, 1)
	require.Len(t, plan.ChannelRequests, 3)
	require.Len(t, plan.AdminRequests, 1)
	require.Len(t, plan.TeamRequests, 1)
	require.Len(t, plan.TeamAdminRequests, 1)
	require.Len(t, plan.TeamMemberRequests, 1)
	require.Len(t, plan.TeamGuestRequests, 1)
	require.Len(t, plan.BotRequests, 1)
	require.Len(t, plan.WebhookRequests, 1)
	require.True(t, plan.Personas[6].OffTeam)

	for _, persona := range plan.Personas {
		require.Equal(t, "exercise-"+persona.Role+personaSuffix, persona.Username)
		require.Equal(t, persona.Username+"@exercise.invalid", persona.Email)
		require.True(t, strings.HasPrefix(persona.Position, "EXERCISE: "), persona.Position)
	}
	require.Equal(t, "silver-harbor/1/current-ops-channel", plan.Channels[0].OpKey)
	require.Equal(t, "team-current-ops", plan.Channels[0].Name)
	for _, req := range plan.ChannelRequests {
		require.True(t, strings.HasPrefix(req.Name, "team-"), req.Name)
		require.True(t, strings.HasPrefix(req.DisplayName, "EXERCISE "), req.DisplayName)
		require.True(t, strings.HasPrefix(req.Purpose, "[EXERCISE] "), req.Purpose)
	}
	require.Equal(t, channelTypePrivate, plan.ChannelRequests[1].ChannelType)
	require.Equal(t, now, plan.ChannelRequests[0].CreatedAt)
	require.Equal(t, now-45*millisPerDay, plan.ChannelRequests[2].CreatedAt)
	require.Equal(t, []string{"id-1", "id-2", "id-3", "id-4"},
		[]string{plan.ChannelRequests[0].ID, plan.ChannelRequests[1].ID, plan.ChannelRequests[2].ID, plan.AdminRequests[0].ID})
	require.Equal(t, plan.Channels[0].OpKey, plan.AdminRequests[0].ChannelOpKey)

	// Every type a scenario can seed, marked and named after the team.
	team := plan.TeamRequests[0]
	require.Equal(t, "exercise-planning-cell-teamexer", team.Name)
	require.Equal(t, "EXERCISE Silver Harbor Planning Cell", team.DisplayName)
	require.Equal(t, model.TeamInvite, team.Type)
	require.True(t, team.TeamAdmin)
	require.Equal(t, []string{"liaison"}, plan.TeamMemberRequests[0].Nominees)
	require.Equal(t, "exercise-guest-fuel-vendor-teamexer@exercise.invalid", plan.TeamGuestRequests[0].Email)
	require.Equal(t, []string{plan.Channels[0].OpKey}, plan.TeamGuestRequests[0].ChannelOpKeys)
	bot := plan.BotRequests[0]
	require.Equal(t, "ex-status-teamex", bot.Username)
	require.Equal(t, plan.Channels[0].OpKey, bot.IncomingChannelOpKey)
	require.Empty(t, bot.OutgoingChannelOpKey)
	require.True(t, bot.Token)
	require.Equal(t, webhookTypeIncoming, plan.WebhookRequests[0].WebhookType)
	require.Equal(t, "EXERCISE Tanker Status Feed", plan.WebhookRequests[0].DisplayName)
	var ids []string
	for _, rt := range plan.requestTypes() {
		ids = append(ids, rt.specIDs...)
	}
	require.Equal(t, []string{"id-1", "id-2", "id-3", "id-4", "id-5", "id-6", "id-7", "id-8", "id-9", "id-10"}, ids)

	again, err := buildExercisePlan(builtInScenario(t), exerciseTeamID, prefixes, now, counterIDs())
	require.NoError(t, err)
	require.Equal(t, plan, again)
}

func TestBuildExercisePlan_CollapsesDisplayNameWhitespace(t *testing.T) {
	sc := &exerciseScenario{
		ID: "test", Name: "Test",
		Personas:   []exercisePersonaSpec{{Role: "lead"}},
		Operations: []exerciseOperationSpec{{OperationID: "ch", Kind: exerciseKindChannel, Suffix: "ops", DisplayName: " Ops \n Floor ", Requester: "lead"}},
	}
	plan, err := buildExercisePlan(sc, exerciseTeamID, []channelPrefix{{Prefix: "team-"}}, 1, counterIDs())
	require.NoError(t, err)
	require.Equal(t, "EXERCISE Ops Floor", plan.Channels[0].DisplayName)
}

func TestBuildExercisePlan_Refusals(t *testing.T) {
	persona := exercisePersonaSpec{Role: "lead", FirstName: "A", LastName: "B", Position: "Lead"}
	channelOp := exerciseOperationSpec{Act: 1, OperationID: "ch", Kind: exerciseKindChannel, Suffix: "ops", DisplayName: "Ops", Requester: "lead"}
	requestOp := exerciseOperationSpec{Act: 1, OperationID: "req", Kind: exerciseKindChannelRequest, Suffix: "plans", DisplayName: "Plans", Requester: "lead"}
	adminOp := exerciseOperationSpec{Act: 1, OperationID: "adm", Kind: exerciseKindAdminRequest, Channel: "ch", Requester: "lead", Nominees: []string{"lead"}}
	many := func(n int, v string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = v
		}
		return out
	}

	tests := []struct {
		name     string
		prefixes []channelPrefix
		mutate   func(*exerciseScenario)
		want     string
	}{
		{"no prefixes", []channelPrefix{}, func(*exerciseScenario) {}, "aren't configured"},
		{"too many personas", nil, func(sc *exerciseScenario) {
			for i := range maxExercisePersonas {
				sc.Personas = append(sc.Personas, exercisePersonaSpec{Role: fmt.Sprintf("p%d", i)})
			}
		}, "at most 50 personas"},
		{"too many operations", nil, func(sc *exerciseScenario) {
			sc.Operations = make([]exerciseOperationSpec, maxExerciseOperations+1)
		}, "200 operations"},
		{"duplicate persona", nil, func(sc *exerciseScenario) { sc.Personas = append(sc.Personas, persona) }, "listed twice"},
		{"bad username", nil, func(sc *exerciseScenario) { sc.Personas[0].Role = "Bad Role" }, "invalid username"},
		{"long first name", nil, func(sc *exerciseScenario) { sc.Personas[0].FirstName = strings.Repeat("x", 65) }, "first or last name longer"},
		{"long last name", nil, func(sc *exerciseScenario) { sc.Personas[0].LastName = strings.Repeat("x", 65) }, "first or last name longer"},
		{"long position", nil, func(sc *exerciseScenario) { sc.Personas[0].Position = strings.Repeat("x", 130) }, "position longer"},
		{"unknown requester", nil, func(sc *exerciseScenario) { sc.Operations[0].Requester = "ghost" }, "unknown persona \"ghost\""},
		{"unknown member", nil, func(sc *exerciseScenario) { sc.Operations[1].Members = []string{"ghost"} }, "unknown persona"},
		{"unknown admin", nil, func(sc *exerciseScenario) { sc.Operations[1].Admins = []string{"ghost"} }, "unknown persona"},
		{"unknown nominee", nil, func(sc *exerciseScenario) { sc.Operations[2].Nominees = []string{"ghost"} }, "unknown persona"},
		{"too many members", nil, func(sc *exerciseScenario) { sc.Operations[1].Members = many(maxMembersPerList+1, "lead") }, "more than 100 personas"},
		{"blank operation ID", nil, func(sc *exerciseScenario) { sc.Operations[1].OperationID = "" }, "unique ID"},
		{"duplicate operation ID", nil, func(sc *exerciseScenario) { sc.Operations[1].OperationID = "ch" }, "unique ID"},
		{"negative age", nil, func(sc *exerciseScenario) { sc.Operations[1].AgeDays = -1 }, "age_days from 0 to 3650"},
		{"huge age", nil, func(sc *exerciseScenario) { sc.Operations[1].AgeDays = maxRequestExpiryDays + 1 }, "age_days"},
		{"long display name", nil, func(sc *exerciseScenario) { sc.Operations[1].DisplayName = strings.Repeat("x", 60) }, "operation \"req\""},
		{"suffix rule", []channelPrefix{{Prefix: "team-", SuffixPatternRaw: "[0-9]+", SuffixPattern: regexp.MustCompile("^(?:[0-9]+)$")}}, func(*exerciseScenario) {}, "doesn't match"},
		{"duplicate name", nil, func(sc *exerciseScenario) { sc.Operations[1].Suffix = "ops" }, "reuses the channel name"},
		{"admin request before its channel", nil, func(sc *exerciseScenario) { sc.Operations[2].Channel = "req" }, "earlier channel operation"},
		{"admin request without nominees", nil, func(sc *exerciseScenario) { sc.Operations[2].Nominees = nil }, "no nominees"},
		{"unknown kind", nil, func(sc *exerciseScenario) { sc.Operations[1].Kind = "webhook" }, "unknown kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &exerciseScenario{
				ID: "test", Name: "Test",
				Personas:   []exercisePersonaSpec{persona},
				Operations: []exerciseOperationSpec{channelOp, requestOp, adminOp},
			}
			tt.mutate(sc)
			prefixes := tt.prefixes
			if prefixes == nil {
				prefixes = []channelPrefix{{Prefix: "team-"}}
			}
			_, err := buildExercisePlan(sc, exerciseTeamID, prefixes, 1, counterIDs())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

// allKindsScenario has one valid operation of every kind after a public
// and a private channel, with "out" off the team.
func allKindsScenario() *exerciseScenario {
	return &exerciseScenario{
		ID: "test", Name: "Test",
		Personas: []exercisePersonaSpec{{Role: "lead"}, {Role: "aide"}, {Role: "out", OffTeam: true}},
		Operations: []exerciseOperationSpec{
			{OperationID: "ch", Kind: exerciseKindChannel, Suffix: "ops", DisplayName: "Ops", Requester: "lead", Members: []string{"aide"}},
			{OperationID: "priv", Kind: exerciseKindChannel, Suffix: "quiet", DisplayName: "Quiet", Requester: "lead", Private: true},
			{OperationID: "team", Kind: exerciseKindTeamRequest, Suffix: "cell", DisplayName: "Cell", Requester: "out"},
			{OperationID: "tadm", Kind: exerciseKindTeamAdmin, Requester: "lead", Nominees: []string{"aide"}},
			{OperationID: "tmem", Kind: exerciseKindTeamMember, Requester: "out", Nominees: []string{"out"}, Reason: "Needs access"},
			{OperationID: "guest", Kind: exerciseKindTeamGuest, Requester: "lead", Guest: "Vendor", Channels: []string{"ch", "priv"}},
			{OperationID: "bot", Kind: exerciseKindBotRequest, Suffix: "feed", DisplayName: "Feed", Requester: "lead", Owner: "out", IncomingChannel: "priv", OutgoingChannel: "ch"},
			{OperationID: "hook", Kind: exerciseKindWebhookRequest, WebhookType: webhookTypeOutgoing, Channel: "ch", DisplayName: "Hook", Requester: "aide"},
		},
	}
}

func TestBuildExercisePlan_AllKinds(t *testing.T) {
	plan, err := buildExercisePlan(allKindsScenario(), exerciseTeamID, []channelPrefix{{Prefix: "team-"}}, 1, counterIDs())
	require.NoError(t, err)
	require.True(t, plan.Personas[2].OffTeam)
	require.Equal(t, model.TeamOpen, plan.TeamRequests[0].Type)
	require.Equal(t, "[EXERCISE] ", plan.TeamRequests[0].Description)
	require.Equal(t, "exercise-guest-vendor-teamexer@exercise.invalid", plan.TeamGuestRequests[0].Email)
	require.Equal(t, []string{"test/0/ch", "test/0/priv"}, plan.TeamGuestRequests[0].ChannelOpKeys)
	require.Equal(t, "out", plan.BotRequests[0].Owner)
	require.Equal(t, "test/0/priv", plan.BotRequests[0].IncomingChannelOpKey)
	require.Equal(t, "test/0/ch", plan.BotRequests[0].OutgoingChannelOpKey)
	require.Equal(t, "Needs access", plan.TeamMemberRequests[0].Reason)
	require.Equal(t, "test/0/ch", plan.WebhookRequests[0].ChannelOpKey)
}

func TestBuildExercisePlan_AllKindsRefusals(t *testing.T) {
	op := func(sc *exerciseScenario, id string) *exerciseOperationSpec {
		for i := range sc.Operations {
			if sc.Operations[i].OperationID == id {
				return &sc.Operations[i]
			}
		}
		panic(id)
	}
	tests := []struct {
		name   string
		mutate func(*exerciseScenario)
		want   string
	}{
		{"off-team channel requester", func(sc *exerciseScenario) { op(sc, "ch").Requester = "out" }, `needs persona "out" on the team`},
		{"off-team channel member", func(sc *exerciseScenario) { op(sc, "ch").Members = []string{"out"} }, `needs persona "out" on the team`},
		{"off-team Team Admin nominee", func(sc *exerciseScenario) { op(sc, "tadm").Nominees = []string{"out"} }, `needs persona "out" on the team`},
		{"off-team Team Admin requester", func(sc *exerciseScenario) { op(sc, "tadm").Requester = "out" }, `needs persona "out" on the team`},
		{"Team Admin without nominees", func(sc *exerciseScenario) { op(sc, "tadm").Nominees = nil }, "no nominees"},
		{"team member without nominees", func(sc *exerciseScenario) { op(sc, "tmem").Nominees = nil }, "no nominees"},
		{"unknown team member nominee", func(sc *exerciseScenario) { op(sc, "tmem").Nominees = []string{"ghost"} }, "unknown persona"},
		{"long team member reason", func(sc *exerciseScenario) { op(sc, "tmem").Reason = strings.Repeat("x", maxReasonLen+1) }, `operation "tmem"`},
		{"bad team URL name", func(sc *exerciseScenario) { op(sc, "team").Suffix = strings.Repeat("x", 60) }, "invalid team URL name"},
		{"team suffix with no letters", func(sc *exerciseScenario) { op(sc, "team").Suffix = "!!" }, "invalid team URL name"},
		{"long team name", func(sc *exerciseScenario) { op(sc, "team").DisplayName = strings.Repeat("x", 70) }, `operation "team"`},
		{"long team description", func(sc *exerciseScenario) { op(sc, "team").Purpose = strings.Repeat("x", maxTeamDescriptionBytes) }, `operation "team"`},
		{"duplicate team URL name", func(sc *exerciseScenario) {
			sc.Operations = append(sc.Operations, exerciseOperationSpec{OperationID: "team2", Kind: exerciseKindTeamRequest, Suffix: "cell", DisplayName: "Cell", Requester: "lead"})
		}, "reuses the team URL name"},
		{"guest without label", func(sc *exerciseScenario) { op(sc, "guest").Guest = "" }, "needs a guest label"},
		{"guest label with no letters", func(sc *exerciseScenario) { op(sc, "guest").Guest = "!!" }, "needs a guest label"},
		{"guest without channels", func(sc *exerciseScenario) { op(sc, "guest").Channels = nil }, "needs 1 to 100 channels"},
		{"guest with too many channels", func(sc *exerciseScenario) {
			for i := range maxMembersPerList + 1 {
				op(sc, "guest").Channels = append(op(sc, "guest").Channels, fmt.Sprintf("c%d", i))
			}
		}, "needs 1 to 100 channels"},
		{"guest channel not a channel op", func(sc *exerciseScenario) { op(sc, "guest").Channels = []string{"team"} }, "earlier channel operation"},
		{"guest requester not in channel", func(sc *exerciseScenario) { op(sc, "guest").Requester = "aide" }, `needs persona "aide" in channel operation "priv"`},
		{"long guest reason", func(sc *exerciseScenario) { op(sc, "guest").Reason = strings.Repeat("x", maxReasonLen+1) }, `operation "guest"`},
		{"duplicate guest", func(sc *exerciseScenario) {
			sc.Operations = append(sc.Operations, exerciseOperationSpec{OperationID: "guest2", Kind: exerciseKindTeamGuest, Requester: "lead", Guest: "vendor", Channels: []string{"ch"}})
		}, "reuses the guest"},
		{"long bot username", func(sc *exerciseScenario) { op(sc, "bot").Suffix = "much-too-long" }, "invalid bot username"},
		{"bot without suffix", func(sc *exerciseScenario) { op(sc, "bot").Suffix = "" }, "invalid bot username"},
		{"bad bot username", func(sc *exerciseScenario) { op(sc, "bot").Suffix = "has space" }, "invalid bot username"},
		{"duplicate bot username", func(sc *exerciseScenario) {
			sc.Operations = append(sc.Operations, exerciseOperationSpec{OperationID: "bot2", Kind: exerciseKindBotRequest, Suffix: "feed", DisplayName: "Feed", Requester: "lead"})
		}, "reuses the bot username"},
		{"unknown bot owner", func(sc *exerciseScenario) { op(sc, "bot").Owner = "ghost" }, "unknown persona"},
		{"long bot display name", func(sc *exerciseScenario) { op(sc, "bot").DisplayName = strings.Repeat("x", maxDisplayNameLen) }, `operation "bot"`},
		{"bot incoming channel unknown", func(sc *exerciseScenario) { op(sc, "bot").IncomingChannel = "nope" }, "earlier channel operation"},
		{"bot outgoing channel unknown", func(sc *exerciseScenario) { op(sc, "bot").OutgoingChannel = "nope" }, "earlier channel operation"},
		{"bot outgoing channel private", func(sc *exerciseScenario) { op(sc, "bot").OutgoingChannel = "priv" }, "public channel for its outgoing webhook"},
		{"webhook type", func(sc *exerciseScenario) { op(sc, "hook").WebhookType = "both" }, "needs a webhook_type"},
		{"webhook requester not in channel", func(sc *exerciseScenario) { op(sc, "hook").Channel = "priv" }, `needs persona "aide" in channel operation "priv"`},
		{"outgoing webhook on private channel", func(sc *exerciseScenario) {
			op(sc, "hook").Channel = "priv"
			op(sc, "hook").Requester = "lead"
		}, "public channel for an outgoing webhook"},
		{"long webhook name", func(sc *exerciseScenario) { op(sc, "hook").DisplayName = strings.Repeat("x", maxWebhookNameBytes) }, `operation "hook"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := allKindsScenario()
			tt.mutate(sc)
			_, err := buildExercisePlan(sc, exerciseTeamID, []channelPrefix{{Prefix: "team-"}}, 1, counterIDs())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestBuildExercisePlan_DropsRepeatedNomineesAndChannels(t *testing.T) {
	sc := allKindsScenario()
	for i := range sc.Operations {
		switch sc.Operations[i].OperationID {
		case "tadm":
			sc.Operations[i].Nominees = append(sc.Operations[i].Nominees, sc.Operations[i].Nominees...)
		case "guest":
			sc.Operations[i].Channels = append(sc.Operations[i].Channels, sc.Operations[i].Channels...)
		}
	}
	plan, err := buildExercisePlan(sc, exerciseTeamID, []channelPrefix{{Prefix: "team-"}}, 1, counterIDs())
	require.NoError(t, err)
	want, err := buildExercisePlan(allKindsScenario(), exerciseTeamID, []channelPrefix{{Prefix: "team-"}}, 1, counterIDs())
	require.NoError(t, err)
	require.Equal(t, want.TeamAdminRequests[0].Nominees, plan.TeamAdminRequests[0].Nominees)
	require.Equal(t, want.TeamGuestRequests[0].ChannelOpKeys, plan.TeamGuestRequests[0].ChannelOpKeys)
}

func TestLoadExerciseScenario(t *testing.T) {
	sc, err := loadExerciseScenario("  ")
	require.NoError(t, err)
	require.Equal(t, "silver-harbor", sc.ID)

	sc, err = loadExerciseScenario(`{"id": "own", "name": "Own", "personas": [{"role": "x"}]}`)
	require.NoError(t, err)
	require.Equal(t, "own", sc.ID)
	require.Len(t, sc.Personas, 1)

	_, err = loadExerciseScenario(`{"id": `)
	require.EqualError(t, err, "the exercise scenario in the plugin settings is not valid JSON")

	_, err = loadExerciseScenario(`{"name": "No ID"}`)
	require.EqualError(t, err, "the exercise scenario needs an id and a name")
}

func TestExercisePersonaUsername_ShortTeamID(t *testing.T) {
	require.Equal(t, "exercise-lead-ab", exercisePersonaUsername("lead", "ab"))
}

func TestExercisePassword(t *testing.T) {
	a, b := exercisePassword(), exercisePassword()
	require.NotEqual(t, a, b)
	require.True(t, strings.HasPrefix(a, "Ex1!"))
	require.Greater(t, len(a), 40)
}

func TestExerciseLabel(t *testing.T) {
	require.Equal(t, "EXERCISE ", exerciseLabel(true))
	require.Empty(t, exerciseLabel(false))
}

// exerciseFake is a small in-memory server for the exercise operations:
// KV, users, team and channel members, channels and posts. fail names API
// methods that return an error.
type exerciseFake struct {
	api      *plugintest.API
	p        *Plugin
	kv       map[string][]byte
	users    map[string]*model.User
	channels map[string]*model.Channel
	posts    map[string]*model.Post
	// teamMembers and channelMembers record who was added; channelMembers
	// maps a channel to each member's roles.
	teamMembers    map[string]bool
	channelMembers map[string]map[string]string
	team           *model.Team
	serverConfig   *model.Config
	fail           map[string]bool
	created        int
}

func newExerciseFake(t *testing.T) *exerciseFake {
	t.Helper()
	api := &plugintest.API{}
	stubLogLines(api)
	serverConfig := &model.Config{}
	serverConfig.SetDefaults()
	f := &exerciseFake{
		api: api, p: newTestPlugin(api),
		users: map[string]*model.User{"admin1": sysadmin("admin1")},
		channels: map[string]*model.Channel{
			"appch":   {Id: "appch", TeamId: "appteam", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"},
			"auditch": {Id: "auditch", TeamId: "appteam", Name: "mattermost-audit-requests", Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"},
		},
		posts:          map[string]*model.Post{},
		teamMembers:    map[string]bool{},
		channelMembers: map[string]map[string]string{},
		team:           &model.Team{Id: exerciseTeamID, Name: "ops", DisplayName: "Ops"},
		serverConfig:   serverConfig,
		fail:           map[string]bool{},
	}
	f.setConfig(func(*configuration) {})
	f.mockServer()
	f.kv = fakeKV(api)
	return f
}

// channelScenarioJSON is the built-in scenario cut down to its channel
// and Channel Admin operations and on-team personas, which most exercise
// tests drive.
var channelScenarioJSON = func() string {
	sc, err := loadExerciseScenario("")
	if err != nil {
		panic(err)
	}
	sc.Personas = slices.DeleteFunc(sc.Personas, func(p exercisePersonaSpec) bool { return p.OffTeam })
	sc.Operations = slices.DeleteFunc(sc.Operations, func(op exerciseOperationSpec) bool {
		return op.Kind != exerciseKindChannel && op.Kind != exerciseKindChannelRequest && op.Kind != exerciseKindAdminRequest
	})
	data, err := json.Marshal(sc)
	if err != nil {
		panic(err)
	}
	return string(data)
}()

// setConfig installs the approval config with exercise mode on and the
// channel scenario, changed by change.
func (f *exerciseFake) setConfig(change func(*configuration)) {
	config := approvalConfig()
	config.AllowChannelAdminRequests = true
	config.EnableExercise = true
	config.ExerciseScenarioJSON = channelScenarioJSON
	change(config)
	f.p.setConfiguration(config)
}

// override puts a mock in front of the fake's own, so it is matched first.
func (f *exerciseFake) override(method string, args ...any) *mock.Call {
	call := f.api.On(method, args...)
	f.api.ExpectedCalls = append([]*mock.Call{call}, slices.DeleteFunc(f.api.ExpectedCalls, func(c *mock.Call) bool { return c == call })...)
	return call
}

func (f *exerciseFake) err(method string) *model.AppError {
	if f.fail[method] {
		return testAppErr(method + " failed")
	}
	return nil
}

func (f *exerciseFake) newID() string {
	f.created++
	return fmt.Sprintf("obj%023d", f.created)
}

func (f *exerciseFake) mockServer() {
	api := f.api
	api.On("GetConfig").Return(func() *model.Config { return f.serverConfig }).Maybe()
	api.On("GetTeam", mock.Anything).Return(func(string) (*model.Team, *model.AppError) {
		if e := f.err("GetTeam"); e != nil {
			return nil, e
		}
		return f.team, nil
	}).Maybe()
	// The approval team is "appteam"; its channels are found and
	// auto-created like any other.
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam", Name: "appteam", DisplayName: "Approvals"}, nil).Maybe()
	api.On("GetUsers", mock.Anything).Return(func(*model.UserGetOptions) ([]*model.User, *model.AppError) {
		var admins []*model.User
		for _, u := range f.users {
			if u.IsSystemAdmin() {
				admins = append(admins, u)
			}
		}
		return admins, nil
	}).Maybe()
	api.On("GetUserByUsername", mock.Anything).Return(func(name string) (*model.User, *model.AppError) {
		if e := f.err("GetUserByUsername"); e != nil {
			return nil, e
		}
		for _, u := range f.users {
			if u.Username == name {
				return u, nil
			}
		}
		return nil, notFoundErr()
	}).Maybe()
	api.On("GetUser", mock.Anything).Return(func(id string) (*model.User, *model.AppError) {
		if u, ok := f.users[id]; ok {
			return u, nil
		}
		return nil, notFoundErr()
	}).Maybe()
	api.On("CreateUser", mock.Anything).Return(func(u *model.User) (*model.User, *model.AppError) {
		if e := f.err("CreateUser"); e != nil {
			return nil, e
		}
		u.Id = f.newID()
		f.users[u.Id] = u
		return u, nil
	}).Maybe()
	api.On("UpdateUserActive", mock.Anything, mock.Anything).Return(func(id string, active bool) *model.AppError {
		if e := f.err("UpdateUserActive"); e != nil {
			return e
		}
		f.users[id].DeleteAt = 0
		if !active {
			f.users[id].DeleteAt = 1
		}
		return nil
	}).Maybe()
	api.On("CreateTeamMember", mock.Anything, mock.Anything).Return(func(teamID, userID string) (*model.TeamMember, *model.AppError) {
		if e := f.err("CreateTeamMember"); e != nil {
			return nil, e
		}
		f.teamMembers[teamID+"/"+userID] = true
		return &model.TeamMember{TeamId: teamID, UserId: userID}, nil
	}).Maybe()
	api.On("GetTeamMember", mock.Anything, mock.Anything).Return(func(teamID, userID string) (*model.TeamMember, *model.AppError) {
		return &model.TeamMember{TeamId: teamID, UserId: userID}, nil
	}).Maybe()
	api.On("GetChannelByName", mock.Anything, mock.Anything, mock.Anything).Return(func(teamID, name string, includeDeleted bool) (*model.Channel, *model.AppError) {
		if e := f.err("GetChannelByName"); e != nil {
			return nil, e
		}
		for _, c := range f.channels {
			if c.TeamId == teamID && c.Name == name && (includeDeleted || c.DeleteAt == 0) {
				cp := *c
				return &cp, nil
			}
		}
		return nil, notFoundErr()
	}).Maybe()
	api.On("GetChannel", mock.Anything).Return(func(id string) (*model.Channel, *model.AppError) {
		if e := f.err("GetChannel"); e != nil {
			return nil, e
		}
		if c, ok := f.channels[id]; ok {
			cp := *c
			return &cp, nil
		}
		return nil, notFoundErr()
	}).Maybe()
	api.On("CreateChannel", mock.Anything).Return(func(c *model.Channel) (*model.Channel, *model.AppError) {
		if e := f.err("CreateChannel"); e != nil {
			return nil, e
		}
		return f.createChannel(c), nil
	}).Maybe()
	api.On("UpdateChannel", mock.Anything).Return(func(c *model.Channel) (*model.Channel, *model.AppError) {
		if e := f.err("UpdateChannel"); e != nil {
			return nil, e
		}
		cp := *c
		f.channels[c.Id] = &cp
		return c, nil
	}).Maybe()
	api.On("DeleteChannel", mock.Anything).Return(func(id string) *model.AppError {
		if e := f.err("DeleteChannel"); e != nil {
			return e
		}
		f.channels[id].DeleteAt = 1
		return nil
	}).Maybe()
	api.On("AddChannelMember", mock.Anything, mock.Anything).Return(func(channelID, userID string) (*model.ChannelMember, *model.AppError) {
		if e := f.err("AddChannelMember"); e != nil {
			return nil, e
		}
		if f.channelMembers[channelID] == nil {
			f.channelMembers[channelID] = map[string]string{}
		}
		f.channelMembers[channelID][userID] = "channel_user"
		return &model.ChannelMember{ChannelId: channelID, UserId: userID}, nil
	}).Maybe()
	api.On("GetChannelMember", mock.Anything, mock.Anything).Return(func(channelID, userID string) (*model.ChannelMember, *model.AppError) {
		if _, ok := f.channelMembers[channelID][userID]; ok {
			return &model.ChannelMember{ChannelId: channelID, UserId: userID}, nil
		}
		return nil, notFoundErr()
	}).Maybe()
	api.On("UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything).Return(func(channelID, userID, roles string) (*model.ChannelMember, *model.AppError) {
		if e := f.err("UpdateChannelMemberRoles"); e != nil {
			return nil, e
		}
		f.channelMembers[channelID][userID] = roles
		return &model.ChannelMember{ChannelId: channelID, UserId: userID}, nil
	}).Maybe()
	api.On("CreatePost", mock.Anything).Return(func(post *model.Post) (*model.Post, *model.AppError) {
		if e := f.err("CreatePost"); e != nil {
			return nil, e
		}
		post.Id = f.newID()
		f.posts[post.Id] = post
		return post, nil
	}).Maybe()
	api.On("GetPost", mock.Anything).Return(func(id string) (*model.Post, *model.AppError) {
		if post, ok := f.posts[id]; ok {
			return post, nil
		}
		return nil, notFoundErr()
	}).Maybe()
	api.On("UpdatePost", mock.Anything).Return(func(post *model.Post) (*model.Post, *model.AppError) {
		f.posts[post.Id] = post
		return post, nil
	}).Maybe()
	api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(&model.Channel{Id: "dm"}, nil).Maybe()
}

func (f *exerciseFake) createChannel(c *model.Channel) *model.Channel {
	c.Id = f.newID()
	cp := *c
	f.channels[c.Id] = &cp
	return c
}

func (f *exerciseFake) runIn(teamID, userID, command string) string {
	resp, appErr := f.p.ExecuteCommand(nil, &model.CommandArgs{Command: command, UserId: userID, TeamId: teamID, ChannelId: "chX"})
	if appErr != nil {
		panic(appErr)
	}
	return resp.Text
}

func (f *exerciseFake) run(userID, command string) string {
	return f.runIn(exerciseTeamID, userID, command)
}

func (f *exerciseFake) populate(t *testing.T) *exerciseManifest {
	t.Helper()
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "Seeded")
	return f.manifest(t)
}

func (f *exerciseFake) manifest(t *testing.T) *exerciseManifest {
	t.Helper()
	data := f.kv[exerciseManifestKey(exerciseTeamID)]
	if data == nil {
		return nil
	}
	var m exerciseManifest
	require.NoError(t, json.Unmarshal(data, &m))
	return &m
}

// setManifest rewrites the stored manifest.
func (f *exerciseFake) setManifest(t *testing.T, change func(*exerciseManifest)) {
	t.Helper()
	m := f.manifest(t)
	change(m)
	data, err := json.Marshal(m)
	require.NoError(t, err)
	f.kv[exerciseManifestKey(exerciseTeamID)] = data
}

// personas returns the accounts recorded as personas of the test team.
func (f *exerciseFake) personas() []*model.User {
	var out []*model.User
	for id, u := range f.users {
		if string(f.kv[kvExercisePersonaPrefix+id]) == exerciseTeamID {
			out = append(out, u)
		}
	}
	return out
}

func (f *exerciseFake) persona(role string) *model.User {
	for _, u := range f.users {
		if u.Username == "exercise-"+role+personaSuffix {
			return u
		}
	}
	return nil
}

func (f *exerciseFake) activeExerciseChannels() int {
	n := 0
	for _, c := range f.channels {
		if c.Header == exerciseBanner && c.DeleteAt == 0 {
			n++
		}
	}
	return n
}

// postsIn returns the messages of the root posts in a channel.
func (f *exerciseFake) postsIn(channelID string) []string {
	var out []string
	for _, post := range f.posts {
		if post.ChannelId == channelID && post.RootId == "" {
			out = append(out, post.Message)
		}
	}
	slices.Sort(out)
	return out
}

// auditLines returns the audit card lines, title first, from the
// auto-created audit channel.
func (f *exerciseFake) auditLines() []string {
	var out []string
	for _, post := range f.posts {
		ch := f.channels[post.ChannelId]
		if ch == nil || ch.Name != "mattermost-audit-requests" {
			continue
		}
		att := post.Attachments()[0]
		out = append(out, att.Title+" "+att.Text)
	}
	slices.Sort(out)
	return out
}

func keyPrefix(prefix string) any {
	return mock.MatchedBy(func(k string) bool { return strings.HasPrefix(k, prefix) })
}

func TestExerciseCommand_Gate(t *testing.T) {
	f := newExerciseFake(t)
	f.users["u1"] = &model.User{Id: "u1", Username: "u1", Roles: model.SystemUserRoleId}

	f.setConfig(func(c *configuration) { c.EnableExercise = false })
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "Exercise mode is off")
	require.Contains(t, f.run("u1", "/mattermost-request-hub exercise status"), "Exercise mode is off")
	require.Nil(t, f.manifest(t))

	f.setConfig(func(*configuration) {})
	require.Equal(t, "Only System Admins can run `/mattermost-request-hub exercise`.", f.run("u1", "/mattermost-request-hub exercise populate"))
	require.Nil(t, f.manifest(t))

	f.populate(t)
	require.Contains(t, f.run("u1", "/mattermost-request-hub exercise teardown"), "Only System Admins")
	require.Contains(t, f.run("ghost", "/mattermost-request-hub exercise teardown"), "Only System Admins")
	require.Equal(t, exerciseUsage, f.run("admin1", "/mattermost-request-hub exercise"))
	require.Equal(t, exerciseUsage, f.run("admin1", "/mattermost-request-hub exercise populate now"))
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise reset"), "Unknown exercise subcommand `reset`")
	require.Contains(t, f.runIn("", "admin1", "/mattermost-request-hub exercise status"), "from a channel in the team")

	f.override("GetUser", "flaky").Return(nil, testAppErr("down"))
	require.Equal(t, "Could not check your account. Please try again.", f.run("flaky", "/mattermost-request-hub exercise status"))

	// Nothing the refused calls ran touched the seeded data.
	require.Len(t, f.personas(), 6)
	for _, u := range f.personas() {
		require.Zero(t, u.DeleteAt)
	}
	require.Equal(t, exerciseStateReady, f.manifest(t).State)
}

func TestExercise_PopulateApproveTeardownRepopulate(t *testing.T) {
	f := newExerciseFake(t)

	reply := f.run("admin1", "/mattermost-request-hub exercise populate")
	require.Equal(t, "Seeded the **Silver Harbor** exercise into this team: 6 personas, 1 channels, 3 channel requests and 1 Channel Admin requests. The approval cards are waiting for review. Run `/mattermost-request-hub exercise teardown` to remove it all.", reply)

	m := f.manifest(t)
	require.Equal(t, exerciseStateReady, m.State)
	require.Equal(t, "admin1", m.CreatedBy)
	require.NotZero(t, m.UpdatedAt)
	require.Len(t, m.Done, 5)
	require.Len(t, m.ChannelIDs, 1)

	personas := f.personas()
	require.Len(t, personas, 6)
	for _, u := range personas {
		require.Equal(t, "EXERCISE", u.Nickname)
		require.NotEmpty(t, u.Password)
		require.Equal(t, "false", u.NotifyProps[model.EmailNotifyProp])
		require.True(t, f.teamMembers[exerciseTeamID+"/"+u.Id], u.Username)
	}

	// The seeded channel holds its creator, members and admin.
	ops := f.channels[m.ChannelIDs[0]]
	require.Equal(t, exerciseBanner, ops.Header)
	require.Equal(t, f.persona("chiefops").Id, ops.CreatorId)
	require.Equal(t, map[string]string{
		f.persona("chiefops").Id: channelAdminRoleString,
		f.persona("planner").Id:  "channel_user",
		f.persona("bomberac").Id: "channel_user",
	}, f.channelMembers[ops.Id])

	// Four cards in the approval channel, all marked, none mentioning
	// anyone, and a ticket for each in its persona's direct messages.
	require.Len(t, f.posts, 8)
	for _, post := range f.posts {
		require.True(t, strings.HasPrefix(post.Attachments()[0].Title, "EXERCISE "))
		if post.ChannelId == "dm" {
			require.Contains(t, post.Message, "has been submitted for approval")
			continue
		}
		require.Equal(t, "appch", post.ChannelId)
		require.True(t, strings.HasPrefix(post.Message, "EXERCISE: "), post.Message)
		require.NotContains(t, post.Message, "@")
	}
	require.Len(t, m.KVKeys, 12)

	// Seeded requests are listed for their persona, not the admin.
	plan := m.Plan
	planner := plan.ChannelRequests[0]
	req, _, err := f.p.loadRequest(planner.ID)
	require.NoError(t, err)
	require.True(t, req.Exercise)
	require.Equal(t, "team-tanker-fallout", req.Name)
	require.NotEmpty(t, req.ApprovalPostID)
	require.NotEmpty(t, req.DMRootPostID)
	require.NotNil(t, f.kv[kvApprovalPostPrefix+req.ApprovalPostID])
	require.NotNil(t, f.kv[kvDMRootPrefix+req.DMRootPostID])
	own, _, err := f.p.listOwnRequests(req.RequesterID, maxListedRequests)
	require.NoError(t, err)
	require.Len(t, own, 1)
	admins, _, err := f.p.listOwnRequests("admin1", maxListedRequests)
	require.NoError(t, err)
	require.Empty(t, admins)

	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "already exists for this team")

	status := f.run("admin1", "/mattermost-request-hub exercise status")
	require.Contains(t, status, "is **ready**")
	require.Contains(t, status, "| Personas (active) | 6 | 6 |")
	require.Contains(t, status, "| Channels (not archived) | 1 | 1 |")
	require.Contains(t, status, "| Channel requests (pending) | 3 | 3 |")
	require.Contains(t, status, "| Channel Admin requests (pending) | 1 | 1 |")

	// Approving a seeded request goes through the normal path and makes a
	// real, marked channel that the manifest records.
	require.Contains(t, f.run("admin1", "/mattermost-request-hub approve "+planner.ID), "Channel ~team-tanker-fallout created")
	m = f.manifest(t)
	require.Len(t, m.ChannelIDs, 2)
	approved := f.channels[m.ChannelIDs[1]]
	require.Equal(t, exerciseBanner, approved.Header)
	require.Equal(t, "EXERCISE Tanker Fallout", approved.DisplayName)
	require.Equal(t, 2, f.activeExerciseChannels())
	status = f.run("admin1", "/mattermost-request-hub exercise status")
	require.Contains(t, status, "| Channels (not archived) | 2 | 2 |")
	require.Contains(t, status, "| Channel requests (pending) | 3 | 2 |")

	reply = f.run("admin1", "/mattermost-request-hub exercise teardown")
	require.Equal(t, "Removed the exercise data from this team: 3 pending requests closed, 2 channels archived and 6 personas deactivated.", reply)
	require.Nil(t, f.manifest(t))
	require.Equal(t, 0, f.activeExerciseChannels())
	for _, id := range m.ChannelIDs {
		require.True(t, strings.HasSuffix(f.channels[id].Name, "-"+id), f.channels[id].Name)
	}
	for _, u := range f.personas() {
		require.NotZero(t, u.DeleteAt)
	}
	for _, spec := range plan.ChannelRequests[1:] {
		exists, err := f.p.pendingRequestExists(pendingKindChannel, spec.ID)
		require.NoError(t, err)
		require.False(t, exists)
	}
	// The pending cards and tickets are closed; the approved one keeps
	// its outcome.
	removedCards, removedTickets := 0, 0
	for _, post := range f.posts {
		switch {
		case post.ChannelId == "appch" && post.RootId == "" && post.Message == exerciseRemovedText:
			removedCards++
			require.Empty(t, post.Attachments())
		case post.ChannelId == "dm" && post.RootId == "" && post.Attachments()[0].Title == "EXERCISE Request Removed":
			removedTickets++
		}
	}
	require.Equal(t, 3, removedCards)
	require.Equal(t, 3, removedTickets)
	require.Equal(t, "EXERCISE Channel Request Approved", f.posts[req.DMRootPostID].Attachments()[0].Title)
	// Nothing but the persona records is left in the KV store, besides the
	// plugin's own record of the Approval team, which isn't exercise data.
	for key := range f.kv {
		require.True(t, strings.HasPrefix(key, kvExercisePersonaPrefix) || key == kvApprovalTeamKey, key)
	}
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise status"), "No exercise data is recorded")
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "nothing to tear down")

	// Populating again reuses the deactivated personas and the freed names.
	created := len(f.users)
	f.populate(t)
	require.Len(t, f.users, created)
	for _, u := range f.personas() {
		require.Zero(t, u.DeleteAt)
	}
}

func TestExercise_TeardownLeavesRealDataAlone(t *testing.T) {
	f := newExerciseFake(t)
	m := f.populate(t)

	// Real data next to the exercise: a member who renamed a public channel
	// to a planned name and pasted the banner, a real pending request, and
	// a real user.
	f.users["real"] = &model.User{Id: "real", Username: "real"}
	f.channels["realch"] = &model.Channel{Id: "realch", TeamId: exerciseTeamID, Name: "team-tanker-fallout", Header: exerciseBanner, CreatorId: "real"}
	realReq := &channelRequest{ID: model.NewId(), RequesterID: "real", TeamID: exerciseTeamID, Name: "team-real", DisplayName: "Real", CreatedAt: model.GetMillis()}
	require.NoError(t, f.p.storeRequest(realReq))
	require.NoError(t, f.p.addPendingEntry(pendingKindChannel, "real", newPendingEntry(realReq)))
	// The seeded channel lost its record, so only the name scan finds it.
	f.setManifest(t, func(m *exerciseManifest) { m.ChannelIDs = nil })

	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "1 channels archived")

	require.NotZero(t, f.channels[m.ChannelIDs[0]].DeleteAt, "the seeded channel is archived")
	require.Zero(t, f.channels["realch"].DeleteAt)
	require.Equal(t, "team-tanker-fallout", f.channels["realch"].Name)
	require.Zero(t, f.channels["appch"].DeleteAt)
	require.Zero(t, f.users["real"].DeleteAt)
	require.Zero(t, f.users["admin1"].DeleteAt)
	stored, _, err := f.p.loadRequest(realReq.ID)
	require.NoError(t, err)
	require.Equal(t, realReq.ID, stored.ID)
	own, _, err := f.p.listOwnRequests("real", maxListedRequests)
	require.NoError(t, err)
	require.Len(t, own, 1)
}

func TestExercise_TeardownRefusesARequestThatIsntExerciseData(t *testing.T) {
	f := newExerciseFake(t)
	m := f.populate(t)
	id := m.Plan.ChannelRequests[0].ID
	f.kv[kvRequestPrefix+id] = recordJSON(t, &channelRequest{ID: id, RequesterID: "real"})

	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "1 exercise items could not be removed")
	require.NotNil(t, f.kv[kvRequestPrefix+id])
	require.NotNil(t, f.manifest(t))
}

func TestExercise_PopulateRefusesBeforeWriting(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*exerciseFake)
		want  string
	}{
		{"no prefixes", func(f *exerciseFake) {
			f.setConfig(func(c *configuration) { c.prefixes = nil })
		}, "Exercise populate failed: Channel requests aren't configured yet. Ask a System Admin to define at least one channel prefix in System Console → Plugins → Mattermost Request Hub."},
		{"channel requests turned off", func(f *exerciseFake) {
			f.p.setConfiguration(&configuration{EnableExercise: true})
		}, "Exercise populate failed: exercise mode seeds channel requests, which are turned off. Turn on **Allow channel requests** in System Console → Plugins → Mattermost Request Hub, then run populate again."},
		{"Channel Admin requests turned off", func(f *exerciseFake) {
			f.setConfig(func(c *configuration) { c.AllowChannelAdminRequests = false })
		}, "Exercise populate failed: this scenario seeds Channel Admin requests, which are turned off. Turn on **Allow channel admin requests** in System Console → Plugins → Mattermost Request Hub, or use a scenario without them."},
		{"bad scenario setting", func(f *exerciseFake) {
			f.setConfig(func(c *configuration) { c.ExerciseScenarioJSON = "{" })
		}, "Exercise populate failed: the exercise scenario in the plugin settings is not valid JSON."},
		{"scenario doesn't fit", func(f *exerciseFake) {
			f.setConfig(func(c *configuration) {
				c.prefixes = []channelPrefix{{Prefix: "team-", SuffixPatternRaw: "[0-9]+", SuffixPattern: regexp.MustCompile("^(?:[0-9]+)$")}}
			})
		}, "doesn't fit this server's settings: operation \"current-ops-channel\""},
		{"username taken by a real account", func(f *exerciseFake) {
			f.users["real"] = &model.User{Id: "real", Username: "exercise-planner" + personaSuffix}
		}, "the username @exercise-planner" + personaSuffix + " is taken by an account that isn't exercise data"},
		{"a real account that copied the marker", func(f *exerciseFake) {
			f.users["lookalike"] = &model.User{
				Id: "lookalike", Username: "exercise-planner" + personaSuffix, DeleteAt: 1,
				Props: model.StringMap{"channel_requests_exercise": exerciseTeamID},
			}
		}, "isn't exercise data"},
		{"persona of another team", func(f *exerciseFake) {
			f.users["other"] = &model.User{Id: "other", Username: "exercise-planner" + personaSuffix}
			f.kv[kvExercisePersonaPrefix+"other"] = []byte("otherteam")
		}, "isn't exercise data"},
		{"channel name taken", func(f *exerciseFake) {
			f.channels["real"] = &model.Channel{Id: "real", TeamId: exerciseTeamID, Name: "team-tanker-fallout"}
		}, "~team-tanker-fallout already exists"},
		{"archived channel holds the name", func(f *exerciseFake) {
			f.channels["real"] = &model.Channel{Id: "real", TeamId: exerciseTeamID, Name: "team-current-ops", DeleteAt: 1}
		}, "already exists"},
		{"server restricts email domains", func(f *exerciseFake) {
			*f.serverConfig.TeamSettings.RestrictCreationToDomains = "example.com, example.org"
		}, "only allows accounts with email addresses at example.com, example.org, so exercise personas (@exercise.invalid) can't join"},
		{"team restricts email domains", func(f *exerciseFake) { f.team.AllowedDomains = "@example.com" }, "at example.com"},
		{"team lookup fails", func(f *exerciseFake) { f.fail["GetTeam"] = true }, "could not load this team"},
		{"user lookup fails", func(f *exerciseFake) { f.fail["GetUserByUsername"] = true }, "could not look up @exercise-chiefops" + personaSuffix},
		{"persona record lookup fails", func(f *exerciseFake) {
			f.users["p"] = &model.User{Id: "p", Username: "exercise-chiefops" + personaSuffix}
			f.override("KVGet", kvExercisePersonaPrefix+"p").Return(nil, testAppErr("down"))
		}, "could not check an exercise persona"},
		{"channel lookup fails", func(f *exerciseFake) { f.fail["GetChannelByName"] = true }, "could not look up ~team-current-ops"},
		{"manifest check fails", func(f *exerciseFake) {
			f.override("KVGet", exerciseManifestKey(exerciseTeamID)).Return(nil, testAppErr("down"))
		}, "could not load the exercise record"},
		{"unreadable manifest", func(f *exerciseFake) {
			f.kv[exerciseManifestKey(exerciseTeamID)] = []byte("{")
		}, "the exercise record for this team is unreadable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newExerciseFake(t)
			tt.setup(f)
			before := len(f.kv)
			reply := f.run("admin1", "/mattermost-request-hub exercise populate")
			require.Contains(t, reply, "Exercise populate failed")
			require.Contains(t, reply, tt.want)
			require.NotContains(t, reply, "..")
			require.Len(t, f.kv, before, "nothing was written")
			require.Empty(t, f.posts)
			require.Empty(t, f.teamMembers)
			if lookalike := f.users["lookalike"]; lookalike != nil {
				require.NotZero(t, lookalike.DeleteAt, "a refused account stays deactivated")
			}
		})
	}
}

func TestExercise_AllowedEmailDomainsThatIncludeTheExerciseDomain(t *testing.T) {
	f := newExerciseFake(t)
	*f.serverConfig.TeamSettings.RestrictCreationToDomains = "example.org,exercise.invalid"
	f.team.AllowedDomains = "exercise.invalid"
	f.populate(t)
}

func TestExercise_PopulateUsesTheScenarioSetting(t *testing.T) {
	f := newExerciseFake(t)
	f.setConfig(func(c *configuration) {
		c.ExerciseScenarioJSON = `{
			"id": "custom", "name": "Custom_Name",
			"personas": [{"role": "lead", "first_name": "Ana", "last_name": "Lee", "position": "Lead"}, {"role": "crew", "first_name": "Bo", "last_name": "Ng", "position": "Crew"}],
			"operations": [{"act": 2, "operation_id": "plans", "kind": "channel_request", "suffix": "plans", "display_name": "Plans", "purpose": "Planning", "requester": "lead", "members": ["crew"]}]
		}`
	})

	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), `Seeded the **Custom\_Name** exercise into this team: 2 personas, 0 channels, 1 channel requests`)
	m := f.manifest(t)
	require.Equal(t, "custom", m.Plan.Scenario)
	require.Contains(t, m.Done, "custom/2/plans")
}

func TestExercise_PopulateFailurePartway(t *testing.T) {
	tests := map[string]string{
		"CreateUser":               "could not create @exercise-chiefops" + personaSuffix,
		"UpdateUserActive":         "could not reactivate @exercise-chiefops" + personaSuffix,
		"CreateTeamMember":         "could not add @exercise-chiefops" + personaSuffix + " to the team",
		"CreateChannel":            "could not create ~team-current-ops",
		"AddChannelMember":         "could not add @exercise-chiefops" + personaSuffix + " to ~team-current-ops",
		"UpdateChannelMemberRoles": "could not make @exercise-chiefops" + personaSuffix + " Channel Admin of ~team-current-ops",
		"CreatePost":               "could not post a channel request's approval card",
	}
	for method, want := range tests {
		t.Run(method, func(t *testing.T) {
			f := newExerciseFake(t)
			if method == "UpdateUserActive" {
				f.users["old"] = &model.User{Id: "old", Username: "exercise-chiefops" + personaSuffix, DeleteAt: 1}
				f.kv[kvExercisePersonaPrefix+"old"] = []byte(exerciseTeamID)
			}
			f.fail[method] = true
			reply := f.run("admin1", "/mattermost-request-hub exercise populate")
			require.Contains(t, reply, "could not finish seeding: "+want)
			require.Contains(t, reply, "teardown")
			require.NotContains(t, reply, "..")
			require.Equal(t, exerciseStatePopulating, f.manifest(t).State)
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise status"), "stopped partway")

			// A populate that just stopped looks like a running one.
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "a populate is still running")

			delete(f.fail, method)
			f.setManifest(t, func(m *exerciseManifest) { m.UpdatedAt -= exerciseStaleMillis })
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "Removed the exercise data")
			require.Nil(t, f.manifest(t))
			require.Equal(t, 0, f.activeExerciseChannels())
			for _, u := range f.personas() {
				require.NotZero(t, u.DeleteAt)
			}
		})
	}
}

func TestExercise_PopulateRecordFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*exerciseFake)
		want  string
	}{
		{"request store", func(f *exerciseFake) {
			f.override("KVSet", keyPrefix(kvRequestPrefix), mock.Anything).Return(testAppErr("down"))
		}, "could not save a channel request"},
		{"admin request store", func(f *exerciseFake) {
			f.override("KVSet", keyPrefix(kvAdminRequestPrefix), mock.Anything).Return(testAppErr("down"))
		}, "could not save a Channel Admin request"},
		{"pending list", func(f *exerciseFake) {
			f.override("KVGet", keyPrefix(kvPendingPrefix+pendingKindChannel)).Return(nil, testAppErr("down"))
		}, "could not list a channel request as pending"},
		{"admin pending list", func(f *exerciseFake) {
			f.override("KVGet", keyPrefix(kvPendingPrefix+pendingKindAdmin)).Return(nil, testAppErr("down"))
		}, "could not list a Channel Admin request as pending"},
		{"persona record", func(f *exerciseFake) {
			f.override("KVSet", keyPrefix(kvExercisePersonaPrefix), mock.Anything).Return(testAppErr("down"))
		}, "could not record @exercise-chiefops" + personaSuffix},
		{"manifest update", func(f *exerciseFake) {
			f.override("KVCompareAndSet", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.Anything).Return(false, testAppErr("down"))
		}, "could not save the exercise record"},
		{"admin request card", func(f *exerciseFake) {
			f.override("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
				return post.ChannelId == "appch" && len(post.Attachments()) > 0 && strings.HasPrefix(post.Attachments()[0].Title, "EXERCISE Channel Admin request")
			})).Return(nil, testAppErr("down"))
		}, "could not post a Channel Admin request's approval card"},
		{"persona lookup fails after the check", func(f *exerciseFake) {
			lookups := 0
			f.override("GetUserByUsername", "exercise-boomop"+personaSuffix).Return(func(string) (*model.User, *model.AppError) {
				lookups++
				if lookups > 1 { // the check looks it up first
					return nil, testAppErr("down")
				}
				return nil, notFoundErr()
			})
		}, "could not look up @exercise-boomop" + personaSuffix},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newExerciseFake(t)
			tt.setup(f)
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not finish seeding: "+tt.want)
		})
	}

	t.Run("an unrecorded persona isn't left active", func(t *testing.T) {
		f := newExerciseFake(t)
		f.override("KVSet", keyPrefix(kvExercisePersonaPrefix), mock.Anything).Return(testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not finish seeding")
		require.NotZero(t, f.persona("chiefops").DeleteAt)
	})

	t.Run("claim fails", func(t *testing.T) {
		f := newExerciseFake(t)
		f.override("KVSetWithOptions", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.Anything).Return(false, testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not start the exercise")
	})

	t.Run("another populate claims first", func(t *testing.T) {
		f := newExerciseFake(t)
		f.override("KVSetWithOptions", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.Anything).Return(false, nil)
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "already exists for this team")
		require.Empty(t, f.personas())
	})

	t.Run("marking it finished fails", func(t *testing.T) {
		f := newExerciseFake(t)
		f.override("KVCompareAndSet", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.MatchedBy(func(v []byte) bool {
			return strings.Contains(string(v), `"state":"ready"`)
		})).Return(false, testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "seeded the exercise but could not mark it finished. Run `/mattermost-request-hub exercise status`")
	})

	t.Run("teardown starts before it is marked finished", func(t *testing.T) {
		f := newExerciseFake(t)
		f.override("KVCompareAndSet", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.Anything).Return(func(key string, oldValue, newValue []byte) (bool, *model.AppError) {
			f.kv[key] = newValue
			var m exerciseManifest
			if json.Unmarshal(newValue, &m) == nil && len(m.Done) == 5 && m.State == exerciseStatePopulating {
				f.setManifest(t, func(m *exerciseManifest) { m.State = exerciseStateTearingDown })
			}
			return true, nil
		})
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not mark it finished")
		require.Equal(t, exerciseStateTearingDown, f.manifest(t).State)
	})

	// Each operation is recorded as it finishes, so a failure leaves the
	// earlier ones recorded.
	for _, failAt := range []int{2, 5} {
		t.Run(fmt.Sprintf("recording operation %d fails", failAt), func(t *testing.T) {
			f := newExerciseFake(t)
			f.override("KVCompareAndSet", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.MatchedBy(func(v []byte) bool {
				var m exerciseManifest
				return json.Unmarshal(v, &m) == nil && len(m.Done) == failAt
			})).Return(false, testAppErr("down"))
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not finish seeding")
			require.Len(t, f.manifest(t).Done, failAt-1)
		})
	}
}

func TestExercise_TeardownStopsARunningPopulate(t *testing.T) {
	// Teardown starts right after the seeded channel is created: populate
	// records the channel so teardown finds it, then stops.
	f := newExerciseFake(t)
	f.override("CreateChannel", mock.Anything).Return(func(c *model.Channel) (*model.Channel, *model.AppError) {
		f.setManifest(t, func(m *exerciseManifest) { m.State = exerciseStateTearingDown })
		return f.createChannel(c), nil
	})
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not finish seeding: teardown started while seeding")
	require.Len(t, f.manifest(t).ChannelIDs, 1)
	require.Empty(t, f.posts, "no request was seeded after teardown began")
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise status"), "A teardown started but didn't finish")

	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "0 pending requests closed, 1 channels archived and 6 personas deactivated")
}

func TestExercise_PopulateUndoesWhatTeardownMissed(t *testing.T) {
	// Teardown finished while populate was making something: populate's
	// record then finds no manifest and removes what it just made.
	tests := map[string]struct {
		setup func(f *exerciseFake, gone func())
		check func(t *testing.T, f *exerciseFake)
	}{
		"persona": {
			func(f *exerciseFake, gone func()) {
				f.override("CreateTeamMember", mock.Anything, mock.Anything).Return(func(string, string) (*model.TeamMember, *model.AppError) {
					gone()
					return &model.TeamMember{}, nil
				}).Once()
			},
			func(t *testing.T, f *exerciseFake) { require.NotZero(t, f.persona("chiefops").DeleteAt) },
		},
		"channel": {
			func(f *exerciseFake, gone func()) {
				f.override("CreateChannel", mock.Anything).Return(func(c *model.Channel) (*model.Channel, *model.AppError) {
					gone()
					return f.createChannel(c), nil
				})
			},
			func(t *testing.T, f *exerciseFake) { require.Equal(t, 0, f.activeExerciseChannels()) },
		},
		"channel request": {
			func(f *exerciseFake, gone func()) {
				f.override("KVSet", keyPrefix(kvRequestPrefix), mock.Anything).Return(func(key string, value []byte) *model.AppError {
					f.kv[key] = value
					gone()
					return nil
				}).Once()
			},
			func(t *testing.T, f *exerciseFake) {
				for key := range f.kv {
					require.False(t, strings.HasPrefix(key, kvRequestPrefix), key)
				}
				require.Equal(t, []string{exerciseRemovedText}, f.postsIn("appch"))
			},
		},
		"admin request": {
			func(f *exerciseFake, gone func()) {
				f.override("KVSet", keyPrefix(kvAdminRequestPrefix), mock.Anything).Return(func(key string, value []byte) *model.AppError {
					f.kv[key] = value
					gone()
					return nil
				})
			},
			func(t *testing.T, f *exerciseFake) {
				for key := range f.kv {
					require.False(t, strings.HasPrefix(key, kvAdminRequestPrefix), key)
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newExerciseFake(t)
			tt.setup(f, func() { delete(f.kv, exerciseManifestKey(exerciseTeamID)) })
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not finish seeding: no exercise data recorded")
			tt.check(t, f)
		})
	}
}

func TestUpdateExerciseManifest(t *testing.T) {
	t.Run("keeps losing the race", func(t *testing.T) {
		f := newExerciseFake(t)
		f.kv[exerciseManifestKey(exerciseTeamID)] = []byte(`{"state":"ready"}`)
		f.override("KVCompareAndSet", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.Anything).Return(false, nil).Times(manifestUpdateAttempts)
		err := f.p.updateExerciseManifest(exerciseTeamID, func(*exerciseManifest) error { return nil })
		require.EqualError(t, err, "the exercise record kept changing")
	})

	t.Run("no manifest", func(t *testing.T) {
		f := newExerciseFake(t)
		err := f.p.updateExerciseManifest(exerciseTeamID, func(*exerciseManifest) error { return nil })
		require.ErrorIs(t, err, errNoExerciseManifest)
	})

	t.Run("unreadable manifest", func(t *testing.T) {
		f := newExerciseFake(t)
		f.kv[exerciseManifestKey(exerciseTeamID)] = []byte("{")
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise status"), "the exercise record for this team is unreadable")
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "the exercise record for this team is unreadable")
	})
}

func TestExercise_ManifestWithoutAPlan(t *testing.T) {
	f := newExerciseFake(t)
	f.kv[exerciseManifestKey(exerciseTeamID)] = []byte(`{"state":"ready"}`)
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise status"), "| Personas (active) | 0 | 0 |")
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "Removed the exercise data")
	require.Nil(t, f.manifest(t))
}

func TestRecordExerciseChannel(t *testing.T) {
	t.Run("after teardown, the channel is archived at once", func(t *testing.T) {
		f := newExerciseFake(t)
		f.channels["c1"] = &model.Channel{Id: "c1", TeamId: exerciseTeamID, Name: "team-late"}
		f.p.recordExerciseChannel(&model.Channel{Id: "c1", TeamId: exerciseTeamID, Name: "team-late"})
		require.NotZero(t, f.channels["c1"].DeleteAt)
		require.Nil(t, f.manifest(t))
	})

	t.Run("the archive fails", func(t *testing.T) {
		f := newExerciseFake(t)
		f.fail["GetChannel"] = true
		f.p.recordExerciseChannel(&model.Channel{Id: "c1", TeamId: exerciseTeamID})
		f.api.AssertCalled(t, "LogWarn", "failed to remove exercise channel after teardown; remove it by hand", "id", "c1", "error", mock.Anything)
	})

	t.Run("the record and the banner fail", func(t *testing.T) {
		f := newExerciseFake(t)
		f.populate(t)
		f.override("KVCompareAndSet", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.Anything).Return(false, testAppErr("down"))
		f.fail["UpdateChannel"] = true
		f.p.recordExerciseChannel(&model.Channel{Id: "c1", TeamId: exerciseTeamID})
		f.api.AssertCalled(t, "LogWarn", "failed to record exercise channel; remove it by hand", "id", "c1", "error", mock.Anything)
		f.api.AssertCalled(t, "LogWarn", "failed to mark exercise channel", "channel_id", "c1", "error", mock.Anything)
	})
}

func TestExercise_ApprovalRacingTeardown(t *testing.T) {
	// An approval records its channel after teardown read the manifest:
	// the compare-and-delete fails and the next pass archives the channel.
	f := newExerciseFake(t)
	f.populate(t)
	raced := false
	f.override("KVCompareAndDelete", exerciseManifestKey(exerciseTeamID), mock.Anything).Return(func(key string, _ []byte) (bool, *model.AppError) {
		if !raced {
			raced = true
			f.channels["late"] = &model.Channel{Id: "late", TeamId: exerciseTeamID, Name: "team-late", Header: exerciseBanner}
			f.setManifest(t, func(m *exerciseManifest) { m.ChannelIDs = append(m.ChannelIDs, "late") })
			return false, nil
		}
		delete(f.kv, key)
		return true, nil
	})
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "2 channels archived")
	require.NotZero(t, f.channels["late"].DeleteAt)
	require.Nil(t, f.manifest(t))
}

func TestExercise_TeardownKeepsTheManifestOnFailure(t *testing.T) {
	for _, method := range []string{"DeleteChannel", "UpdateChannel", "GetChannel", "UpdateUserActive", "GetUserByUsername", "GetChannelByName"} {
		t.Run(method, func(t *testing.T) {
			f := newExerciseFake(t)
			f.populate(t)
			f.fail[method] = true
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not be removed")
			require.NotNil(t, f.manifest(t))

			delete(f.fail, method)
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "Removed the exercise data")
			require.Nil(t, f.manifest(t))
		})
	}
}

func TestExercise_TeardownRequestFailures(t *testing.T) {
	tests := map[string]func(*exerciseFake){
		"load": func(f *exerciseFake) {
			f.override("KVGet", keyPrefix(kvRequestPrefix)).Return(nil, testAppErr("down"))
		},
		"unreadable": func(f *exerciseFake) {
			f.override("KVGet", keyPrefix(kvAdminRequestPrefix)).Return([]byte("{"), nil)
		},
		"claim": func(f *exerciseFake) {
			f.override("KVCompareAndDelete", keyPrefix(kvRequestPrefix), mock.Anything).Return(false, testAppErr("down"))
		},
		"persona record": func(f *exerciseFake) {
			f.override("KVGet", keyPrefix(kvExercisePersonaPrefix)).Return(nil, testAppErr("down"))
		},
		"reply lookup": func(f *exerciseFake) {
			f.override("KVDelete", keyPrefix(kvApprovalPostPrefix)).Return(testAppErr("down"))
		},
		"persona pending list": func(f *exerciseFake) {
			f.override("KVDelete", keyPrefix(kvPendingPrefix)).Return(testAppErr("down"))
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			f := newExerciseFake(t)
			f.populate(t)
			setup(f)
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not be removed")
			require.NotNil(t, f.manifest(t))
		})
	}

	t.Run("an approver wins the claim", func(t *testing.T) {
		f := newExerciseFake(t)
		f.populate(t)
		f.override("KVCompareAndDelete", keyPrefix(kvRequestPrefix), mock.Anything).Return(func(key string, _ []byte) (bool, *model.AppError) {
			delete(f.kv, key)
			return false, nil
		})
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "1 pending requests closed")
	})

	t.Run("a first approval rewrites the request", func(t *testing.T) {
		f := newExerciseFake(t)
		f.populate(t)
		f.override("KVCompareAndDelete", keyPrefix(kvRequestPrefix), mock.Anything).Return(false, nil)
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not be removed")
		require.NotNil(t, f.manifest(t))
	})

	t.Run("marking it torn down fails", func(t *testing.T) {
		f := newExerciseFake(t)
		f.populate(t)
		f.override("KVCompareAndSet", exerciseManifestKey(exerciseTeamID), mock.Anything, mock.Anything).Return(false, testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not save the exercise record")
	})

	t.Run("re-reading the manifest fails", func(t *testing.T) {
		f := newExerciseFake(t)
		f.populate(t)
		f.override("KVGet", exerciseManifestKey(exerciseTeamID)).Return(func(key string) ([]byte, *model.AppError) {
			if strings.Contains(string(f.kv[key]), exerciseStateTearingDown) {
				return nil, testAppErr("down")
			}
			return f.kv[key], nil
		})
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not load the exercise record")
	})

	t.Run("clearing the manifest fails", func(t *testing.T) {
		f := newExerciseFake(t)
		f.populate(t)
		f.override("KVCompareAndDelete", exerciseManifestKey(exerciseTeamID), mock.Anything).Return(false, testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not clear its record")
	})

	t.Run("the manifest keeps changing", func(t *testing.T) {
		f := newExerciseFake(t)
		f.populate(t)
		f.override("KVCompareAndDelete", exerciseManifestKey(exerciseTeamID), mock.Anything).Return(false, nil)
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "kept changing while it was removed")
	})
}

func TestExercise_TeardownSkipsANameTakenByARealAccount(t *testing.T) {
	// Populate stopped, and the account behind one persona name isn't
	// recorded as a persona: teardown leaves it alone.
	f := newExerciseFake(t)
	f.fail["CreateChannel"] = true
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not finish seeding")
	delete(f.fail, "CreateChannel")
	planner := f.persona("planner")
	delete(f.kv, kvExercisePersonaPrefix+planner.Id)
	f.setManifest(t, func(m *exerciseManifest) { m.UpdatedAt -= exerciseStaleMillis })

	status := f.run("admin1", "/mattermost-request-hub exercise status")
	require.Contains(t, status, "| Personas (active) | 6 | 5 |")
	require.NotContains(t, status, "could not be checked")
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "5 personas deactivated")
	require.Zero(t, planner.DeleteAt)
}

func TestExercise_StatusReportsWhatItCouldNotCheck(t *testing.T) {
	f := newExerciseFake(t)
	f.populate(t)
	f.fail["GetUserByUsername"] = true
	f.fail["GetChannel"] = true
	f.override("KVGet", keyPrefix(kvRequestPrefix)).Return(nil, testAppErr("down"))
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise status"), "10 items could not be checked")
	f.api.AssertCalled(t, "LogWarn", "exercise status could not check some items", "team_id", exerciseTeamID, "count", 10)
}

func TestArchiveExerciseChannel(t *testing.T) {
	f := newExerciseFake(t)

	ok, err := f.p.archiveExerciseChannel("missing")
	require.NoError(t, err)
	require.False(t, ok)

	f.channels["gone"] = &model.Channel{Id: "gone", Name: "x", DeleteAt: 1}
	ok, err = f.p.archiveExerciseChannel("gone")
	require.NoError(t, err)
	require.False(t, ok)

	id := "longid00000000000000000000"
	f.channels[id] = &model.Channel{Id: id, Name: strings.Repeat("a", 40) + "-" + strings.Repeat("b", 23)}
	ok, err = f.p.archiveExerciseChannel(id)
	require.NoError(t, err)
	require.True(t, ok)
	name := f.channels[id].Name
	require.LessOrEqual(t, len(name), model.ChannelNameMaxLength)
	require.True(t, strings.HasSuffix(name, "-"+id))
	require.NotContains(t, name, "--")

	// A channel renamed by an earlier, interrupted teardown isn't renamed again.
	renamed := "renamed0000000000000000000"
	f.channels[renamed] = &model.Channel{Id: renamed, Name: "team-x-" + renamed}
	f.fail["UpdateChannel"] = true
	ok, err = f.p.archiveExerciseChannel(renamed)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestApprovalCardMarkers(t *testing.T) {
	f := newExerciseFake(t)
	requester := &model.User{Id: "u1", Username: "u1"}

	for _, exercise := range []bool{false, true} {
		_, _, err := f.p.postApprovalRequest(&channelRequest{ID: model.NewId(), TeamID: exerciseTeamID, Name: "team-x", DisplayName: "X", Exercise: exercise}, requester)
		require.NoError(t, err)
	}
	channel := &model.Channel{Id: "c1", TeamId: exerciseTeamID, Name: "x", DisplayName: "X"}
	_, _, err := f.p.postAdminApprovalRequest(&adminRequest{ID: model.NewId(), Exercise: true}, requester, channel)
	require.NoError(t, err)

	type card struct{ header, title string }
	var cards []card
	for _, post := range f.posts {
		cards = append(cards, card{post.Message, post.Attachments()[0].Title})
	}
	require.ElementsMatch(t, []card{
		{"@channel — a new channel request needs your review.", "Channel creation request — Ops"},
		{"EXERCISE: a new channel request needs your review.", "EXERCISE Channel creation request — Ops"},
		{"EXERCISE: a Channel Admin request needs your review.", "EXERCISE Channel Admin request — Ops"},
	}, cards)
	require.Equal(t, "Channel Admin request — Ops", f.p.adminApprovalAttachment(&adminRequest{}, requester, channel).Title)
}

// A team without its own approvers sends its cards to the root approval
// channel with a team label; exercise cards keep it in front of the marker.
func TestApprovalCardMarkers_KeepTheTeamLabel(t *testing.T) {
	f := newExerciseFake(t)
	f.setConfig(func(c *configuration) { c.PerTeamApprovalChannels = true })
	f.api.On("GetTeamMembers", exerciseTeamID, mock.Anything, mock.Anything).Return([]*model.TeamMember{}, nil)
	requester := &model.User{Id: "u1", Username: "u1"}

	_, _, err := f.p.postApprovalRequest(&channelRequest{ID: model.NewId(), TeamID: exerciseTeamID, Name: "team-x", DisplayName: "X", Exercise: true}, requester)
	require.NoError(t, err)
	channel := &model.Channel{Id: "c1", TeamId: exerciseTeamID, Name: "x", DisplayName: "X"}
	_, _, err = f.p.postAdminApprovalRequest(&adminRequest{ID: model.NewId(), Exercise: true}, requester, channel)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{
		"For Team: Ops — EXERCISE: a Channel Admin request needs your review.",
		"For Team: Ops — EXERCISE: a new channel request needs your review.",
	}, f.postsIn("appch"))
}

func TestExercise_DecisionsAreMarkedInTheAudit(t *testing.T) {
	f := newExerciseFake(t)
	f.setConfig(func(c *configuration) {
		c.expiryDays = 30
		c.ExerciseScenarioJSON = `{
			"id": "audit", "name": "Audit",
			"personas": [{"role": "lead"}, {"role": "crew"}],
			"operations": [
				{"act": 1, "operation_id": "ops", "kind": "channel", "suffix": "ops", "display_name": "Ops", "requester": "lead", "members": ["crew"]},
				{"act": 1, "operation_id": "approve", "kind": "channel_request", "suffix": "approved", "display_name": "Approved", "requester": "lead"},
				{"act": 1, "operation_id": "deny", "kind": "channel_request", "suffix": "denied", "display_name": "Denied", "requester": "lead"},
				{"act": 1, "operation_id": "old", "kind": "channel_request", "suffix": "old", "display_name": "Old", "requester": "lead", "age_days": 45},
				{"act": 1, "operation_id": "promote", "kind": "admin_request", "channel": "ops", "requester": "lead", "nominees": ["crew"]},
				{"act": 1, "operation_id": "refuse", "kind": "admin_request", "channel": "ops", "requester": "lead", "nominees": ["crew"]},
				{"act": 1, "operation_id": "left", "kind": "admin_request", "channel": "ops", "requester": "lead", "nominees": ["crew"]},
				{"act": 1, "operation_id": "stale", "kind": "admin_request", "channel": "ops", "requester": "lead", "nominees": ["crew"], "age_days": 45}
			]
		}`
	})
	m := f.populate(t)
	cr, ar := m.Plan.ChannelRequests, m.Plan.AdminRequests

	require.Contains(t, f.run("admin1", "/mattermost-request-hub approve "+cr[0].ID), "created")
	require.Contains(t, f.run("admin1", "/mattermost-request-hub deny "+cr[1].ID), "Denied")
	require.Equal(t, expiredText, f.run("admin1", "/mattermost-request-hub approve "+cr[2].ID))
	require.Contains(t, f.run("admin1", "/mattermost-request-hub approve "+ar[0].ID), "promoted to Channel Admin")
	require.Contains(t, f.run("admin1", "/mattermost-request-hub deny "+ar[1].ID), "Denied")
	lead := "@exercise-lead" + personaSuffix
	crew := "@exercise-crew" + personaSuffix
	// A nominee who left blocks Approve, so nothing is audited for it.
	delete(f.channelMembers[m.ChannelIDs[0]], f.persona("crew").Id)
	require.Equal(t, crew+" is no longer a member of the channel. Deny this request instead.", f.run("admin1", "/mattermost-request-hub approve "+ar[2].ID))
	require.Equal(t, expiredText, f.run("admin1", "/mattermost-request-hub approve "+ar[3].ID))

	require.Equal(t, []string{
		fmt.Sprintf(`⌛ Expired EXERCISE @admin closed channel request "EXERCISE Old" from %s, which was not reviewed within 30 days`, lead),
		fmt.Sprintf("⌛ Expired EXERCISE @admin closed the Channel Admin request for %s in ~team-ops from %s, which was not reviewed within 30 days", crew, lead),
		fmt.Sprintf(`✅ Approved EXERCISE @admin approved channel request "EXERCISE Approved" (~team-approved) from %s`, lead),
		fmt.Sprintf("✅ Approved EXERCISE @admin promoted %s to Channel Admin in ~team-ops (requested by %s)", crew, lead),
		fmt.Sprintf("❌ Denied EXERCISE @admin denied a Channel Admin request for %s in ~team-ops", crew),
		fmt.Sprintf(`❌ Denied EXERCISE @admin denied channel request "EXERCISE Denied" from %s`, lead),
	}, f.auditLines())
}
