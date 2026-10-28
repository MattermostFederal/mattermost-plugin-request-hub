package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// kindsFake is an exerciseFake that seeds the whole built-in scenario:
// every request type is on, and System Admins fill both two-step steps.
// Teams and bots that approvals create are kept in its maps.
type kindsFake struct {
	*exerciseFake
	teams       map[string]*model.Team
	bots        map[string]*model.Bot
	hooks       map[string]string // hook ID -> type
	teamMembers map[string]*model.TeamMember
	// deleted lists "hook" and "bot" as each is deleted, in order.
	deleted []string
}

func newKindsFake(t *testing.T) *kindsFake {
	t.Helper()
	f := &kindsFake{
		exerciseFake: newExerciseFake(t),
		teams:        map[string]*model.Team{},
		bots:         map[string]*model.Bot{},
		hooks:        map[string]string{},
		teamMembers:  map[string]*model.TeamMember{},
	}
	f.users["admin2"] = sysadmin("admin2")
	f.serverConfig.GuestAccountsSettings.Enable = model.NewPointer(true)
	f.serverConfig.ServiceSettings.SiteURL = model.NewPointer("http://localhost:8065")
	f.setConfig(func(*configuration) {})
	f.mockKinds()
	return f
}

func (f *kindsFake) setConfig(change func(*configuration)) {
	f.exerciseFake.setConfig(func(c *configuration) {
		c.ExerciseScenarioJSON = ""
		c.AllowTeamRequests, c.AllowTeamAdminRequests, c.AllowTeamMemberRequests, c.AllowTeamGuestRequests = true, true, true, true
		c.AllowBotTokenRequests, c.AllowWebhookRequests = true, true
		c.TwoStepApproverMode = twoStepModeSystemAdmin
		change(c)
	})
}

func (f *kindsFake) mockKinds() {
	f.override("GetTeam", mock.Anything).Return(func(id string) (*model.Team, *model.AppError) {
		if e := f.err("GetTeam"); e != nil {
			return nil, e
		}
		if id == exerciseTeamID {
			return f.team, nil
		}
		if team, ok := f.teams[id]; ok {
			cp := *team
			return &cp, nil
		}
		return nil, notFoundErr()
	}).Maybe()
	f.override("GetTeamByName", mock.Anything).Return(func(name string) (*model.Team, *model.AppError) {
		if name == "appteam" {
			return &model.Team{Id: "appteam", Name: "appteam", DisplayName: "Approvals"}, nil
		}
		if e := f.err("GetTeamByName"); e != nil {
			return nil, e
		}
		for _, team := range f.teams {
			if team.Name == name {
				cp := *team
				return &cp, nil
			}
		}
		return nil, notFoundErr()
	}).Maybe()
	f.api.On("CreateTeam", mock.Anything).Return(func(team *model.Team) (*model.Team, *model.AppError) {
		team.Id = f.newID()
		cp := *team
		f.teams[team.Id] = &cp
		return team, nil
	}).Maybe()
	f.api.On("UpdateTeam", mock.Anything).Return(func(team *model.Team) (*model.Team, *model.AppError) {
		if e := f.err("UpdateTeam"); e != nil {
			return nil, e
		}
		cp := *team
		f.teams[team.Id] = &cp
		return team, nil
	}).Maybe()
	f.api.On("DeleteTeam", mock.Anything).Return(func(id string) *model.AppError {
		if e := f.err("DeleteTeam"); e != nil {
			return e
		}
		f.teams[id].DeleteAt = 1
		return nil
	}).Maybe()
	f.override("CreateTeamMember", mock.Anything, mock.Anything).Return(func(teamID, userID string) (*model.TeamMember, *model.AppError) {
		if e := f.err("CreateTeamMember"); e != nil {
			return nil, e
		}
		f.exerciseFake.teamMembers[teamID+"/"+userID] = true
		member := &model.TeamMember{TeamId: teamID, UserId: userID, SchemeUser: true}
		f.teamMembers[teamID+"/"+userID] = member
		return member, nil
	}).Maybe()
	f.override("GetTeamMember", mock.Anything, mock.Anything).Return(func(teamID, userID string) (*model.TeamMember, *model.AppError) {
		if e := f.err("GetTeamMember"); e != nil {
			return nil, e
		}
		if member, ok := f.teamMembers[teamID+"/"+userID]; ok {
			cp := *member
			return &cp, nil
		}
		return nil, notFoundErr()
	}).Maybe()
	f.api.On("UpdateTeamMemberRoles", mock.Anything, mock.Anything, mock.Anything).Return(func(teamID, userID, roles string) (*model.TeamMember, *model.AppError) {
		if e := f.err("UpdateTeamMemberRoles"); e != nil {
			return nil, e
		}
		member := f.teamMembers[teamID+"/"+userID]
		member.SchemeAdmin = strings.Contains(roles, model.TeamAdminRoleId)
		return member, nil
	}).Maybe()
	f.api.On("DeleteTeamMember", mock.Anything, mock.Anything, mock.Anything).Return(func(teamID, userID, _ string) *model.AppError {
		if e := f.err("DeleteTeamMember"); e != nil {
			return e
		}
		f.teamMembers[teamID+"/"+userID].DeleteAt = 1
		return nil
	}).Maybe()
	f.api.On("CreateBot", mock.Anything).Return(func(bot *model.Bot) (*model.Bot, *model.AppError) {
		bot.UserId = f.newID()
		cp := *bot
		f.bots[bot.UserId] = &cp
		f.users[bot.UserId] = &model.User{Id: bot.UserId, Username: bot.Username, IsBot: true}
		return bot, nil
	}).Maybe()
	f.api.On("GetBot", mock.Anything, mock.Anything).Return(func(id string, _ bool) (*model.Bot, *model.AppError) {
		if e := f.err("GetBot"); e != nil {
			return nil, e
		}
		if bot, ok := f.bots[id]; ok {
			return bot, nil
		}
		return nil, notFoundErr()
	}).Maybe()
	f.api.On("PermanentDeleteBot", mock.Anything).Return(func(id string) *model.AppError {
		if e := f.err("PermanentDeleteBot"); e != nil {
			return e
		}
		delete(f.bots, id)
		delete(f.users, id)
		f.deleted = append(f.deleted, "bot")
		return nil
	}).Maybe()
	// Sidebar placement is best-effort and not under test here.
	f.api.On("GetChannelSidebarCategories", mock.Anything, mock.Anything).Return(nil, testAppErr("not here")).Maybe()
	// No admin session, so bot extras are skipped; webhooks go through
	// the plugin's test hooks.
	f.api.On("CreateSession", mock.Anything).Return(nil, testAppErr("no sessions here")).Maybe()
	f.p.createWebhookFn = func(req *webhookRequest, _ string) (string, string, error) {
		id := f.newID()
		f.hooks[id] = req.WebhookType
		return "made", id, nil
	}
	f.p.deleteWebhookFn = func(webhookType, hookID string) error {
		if e := f.err("deleteWebhook"); e != nil {
			return e
		}
		if _, ok := f.hooks[hookID]; !ok {
			return notFoundErr()
		}
		delete(f.hooks, hookID)
		f.deleted = append(f.deleted, "hook")
		return nil
	}
}

// serveExtras answers the REST calls an admin session makes, so bot
// extras are created and webhooks are deleted over HTTP, as on a server.
func (f *kindsFake) serveExtras(t *testing.T) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v4/"), "/")
		switch {
		case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == "hooks":
			id := f.newID()
			f.hooks[id] = parts[1]
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
		case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "users" && parts[2] == "tokens":
			_ = json.NewEncoder(w).Encode(model.UserAccessToken{Id: "tok-id", Token: "tok-value"})
		case r.Method == http.MethodDelete && len(parts) == 3 && parts[0] == "hooks":
			if _, ok := f.hooks[parts[2]]; !ok {
				http.Error(w, `{"id":"app.webhooks.get.not_found","status_code":404}`, http.StatusNotFound)
				return
			}
			delete(f.hooks, parts[2])
			f.deleted = append(f.deleted, "hook")
			_, _ = fmt.Fprint(w, `{"status":"OK"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	f.serverConfig.ServiceSettings.SiteURL = model.NewPointer(ts.URL)
	f.serverConfig.ServiceSettings.ListenAddress = nil
	f.override("CreateSession", mock.Anything).Return(&model.Session{Id: "sess1", Token: "tok1"}, nil)
	f.api.On("RevokeSession", "sess1").Return(nil).Maybe()
	f.p.deleteWebhookFn = nil
}

func TestExercise_PopulateSeedsEveryKind(t *testing.T) {
	f := newKindsFake(t)

	reply := f.run("admin1", "/mattermost-request-hub exercise populate")
	require.Equal(t, "Seeded the **Silver Harbor** exercise into this team: 7 personas, 1 channels, 3 channel requests, 1 Channel Admin requests, 1 team creation requests, 1 Team Admin requests, 1 team member requests, 1 guest invite requests, 1 bot account requests and 1 webhook requests. The approval cards are waiting for review. Run `/mattermost-request-hub exercise teardown` to remove it all.", reply)

	m := f.manifest(t)
	require.Equal(t, exerciseStateReady, m.State)
	require.Len(t, m.Done, 11) // the channel and ten requests
	for _, rt := range m.Plan.requestTypes() {
		for _, id := range rt.specIDs {
			var rec struct {
				Exercise       bool   `json:"exercise"`
				ExerciseTeamID string `json:"exercise_team_id"`
				ApprovalPostID string `json:"approval_post_id"`
				DMRootPostID   string `json:"dm_root_post_id"`
			}
			require.NoError(t, json.Unmarshal(f.kv[requestKVPrefix(rt.kind)+id], &rec), rt.kind)
			require.True(t, rec.Exercise, rt.kind)
			require.NotEmpty(t, rec.ApprovalPostID, rt.kind)
			require.NotEmpty(t, rec.DMRootPostID, rt.kind)
			if rt.kind == pendingKindTeam || rt.kind == pendingKindBot {
				require.Equal(t, exerciseTeamID, rec.ExerciseTeamID, rt.kind)
			}
			require.Contains(t, m.KVKeys, kvApprovalCardsPrefix+id)
		}
	}

	// Every card is marked and mentions nobody.
	cards := 0
	for _, post := range f.posts {
		atts := post.Attachments()
		if len(atts) == 0 || len(atts[0].Actions) == 0 {
			continue
		}
		cards++
		require.True(t, strings.HasPrefix(atts[0].Title, "EXERCISE "), atts[0].Title)
		require.True(t, strings.Contains(post.Message, "EXERCISE: "), post.Message)
		require.NotContains(t, post.Message, "@", post.Message)
	}
	require.Equal(t, 10, cards)

	// The off-team persona isn't on the team; the rest are.
	require.Nil(t, f.teamMembers[exerciseTeamID+"/"+f.persona("liaison").Id])
	require.NotNil(t, f.teamMembers[exerciseTeamID+"/"+f.persona("planner").Id])
}

// exerciseRequestID returns the ID of the plan's first request of kind.
func exerciseRequestID(t *testing.T, m *exerciseManifest, kind string) string {
	t.Helper()
	for _, rt := range m.Plan.requestTypes() {
		if rt.kind == kind {
			require.NotEmpty(t, rt.specIDs, kind)
			return rt.specIDs[0]
		}
	}
	t.Fatalf("no kind %s", kind)
	return ""
}

// approveAll approves the seeded team, Team Admin, team member, bot and
// webhook requests. Guest invites need a real server and are left pending.
func (f *kindsFake) approveAll(t *testing.T, m *exerciseManifest) {
	t.Helper()
	res := f.p.decideTeamRequest("admin1", exerciseRequestID(t, m, pendingKindTeam), "", true)
	require.Contains(t, res.Reply+res.Outcome, "created")
	res = f.p.decideTeamAdminRequest("admin1", exerciseRequestID(t, m, pendingKindTeamAdmin), "", true)
	require.Contains(t, res.Reply+res.Outcome, "promoted")
	res = f.p.decideTeamMemberRequest("admin1", exerciseRequestID(t, m, pendingKindTeamMember), "", true)
	require.Contains(t, res.Reply+res.Outcome, "Added")
	// One System Admin fills both steps.
	res = f.p.decideBotRequest("admin1", exerciseRequestID(t, m, pendingKindBot), "", true)
	require.Contains(t, res.Reply+res.Outcome, "created")
	res = f.p.decideWebhookRequest("admin1", exerciseRequestID(t, m, pendingKindWebhook), "", true)
	require.Contains(t, res.Reply+res.Outcome, "created")
}

func TestExercise_ApproveEveryKindThenTeardown(t *testing.T) {
	f := newKindsFake(t)
	m := f.populate(t)
	planner, liaison := f.persona("planner"), f.persona("liaison")
	f.approveAll(t, m)

	require.Len(t, f.teams, 1)
	require.Len(t, f.bots, 1)
	require.Len(t, f.hooks, 1)
	require.True(t, f.teamMembers[exerciseTeamID+"/"+planner.Id].SchemeAdmin)
	require.NotNil(t, f.teamMembers[exerciseTeamID+"/"+liaison.Id])
	m = f.manifest(t)
	var teamID, botID string
	for id, team := range f.teams {
		teamID = id
		require.Equal(t, "EXERCISE Silver Harbor Planning Cell", team.DisplayName)
	}
	for id := range f.bots {
		botID = id
	}
	require.Equal(t, []string{teamID}, m.TeamIDs)
	require.Equal(t, []string{botID}, m.BotIDs)
	require.Len(t, m.Webhooks, 1)
	require.Equal(t, webhookTypeIncoming, m.Webhooks[0].Type)

	status := f.run("admin1", "/mattermost-request-hub exercise status")
	require.Contains(t, status, "| Team creation requests (pending) | 1 | 0 |")
	require.Contains(t, status, "| Guest invite requests (pending) | 1 | 1 |")
	require.Contains(t, status, "| Teams (not archived) | 1 | 1 |")
	require.Contains(t, status, "| Bots | 1 | 1 |")
	require.Contains(t, status, "| Webhooks | 1 | not checked |")

	reply := f.run("admin1", "/mattermost-request-hub exercise teardown")
	require.Equal(t, "Removed the exercise data from this team: 5 pending requests closed, 1 teams archived, 1 bots deleted, 1 webhooks deleted, 1 channels archived and 7 personas deactivated.", reply)
	require.Nil(t, f.manifest(t))
	require.Empty(t, f.bots)
	require.Empty(t, f.hooks)
	require.NotZero(t, f.teams[teamID].DeleteAt)
	require.Equal(t, "exercise-planning-cell-teamexer-"+teamID, f.teams[teamID].Name)
	require.False(t, f.teamMembers[exerciseTeamID+"/"+planner.Id].SchemeAdmin)
	require.NotZero(t, f.teamMembers[exerciseTeamID+"/"+liaison.Id].DeleteAt)
	for _, u := range f.personas() {
		require.NotZero(t, u.DeleteAt, u.Username)
		for _, kind := range allPendingKinds {
			require.Nil(t, f.kv[pendingListKey(kind, u.Id)], kind)
		}
	}

	// The names are free again, so the same team can be seeded again.
	f.populate(t)
}

func TestExercise_PopulateRefusesNewKindsUpfront(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *kindsFake)
		want  string
	}{
		{"type off", func(f *kindsFake) {
			f.setConfig(func(c *configuration) { c.AllowWebhookRequests = false })
		}, "this scenario seeds webhook requests, which are turned off. Turn on **Allow webhook requests**"},
		{"team URL name taken", func(f *kindsFake) {
			f.teams["t9"] = &model.Team{Id: "t9", Name: "exercise-planning-cell-teamexer"}
		}, "a team with the URL name exercise-planning-cell-teamexer already exists"},
		{"team lookup fails", func(f *kindsFake) { f.fail["GetTeamByName"] = true }, "could not look up the team"},
		{"bot username taken", func(f *kindsFake) {
			f.users["b9"] = &model.User{Id: "b9", Username: "ex-status-teamex"}
		}, "the username @ex-status-teamex is taken"},
		{"guest accounts off", func(f *kindsFake) {
			f.serverConfig.GuestAccountsSettings.Enable = model.NewPointer(false)
		}, "guest accounts are off"},
		{"guest domains exclude the exercise domain", func(f *kindsFake) {
			f.serverConfig.GuestAccountsSettings.RestrictCreationToDomains = model.NewPointer("example.com")
		}, "guests can only be invited from example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newKindsFake(t)
			tt.setup(f)
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), tt.want)
			require.Nil(t, f.manifest(t))
			require.Empty(t, f.personas())
		})
	}

	t.Run("guest domains that include the exercise domain", func(t *testing.T) {
		f := newKindsFake(t)
		f.serverConfig.GuestAccountsSettings.RestrictCreationToDomains = model.NewPointer("example.com, exercise.invalid")
		f.populate(t)
	})
}

func TestExercise_BotUsernameLookupFails(t *testing.T) {
	f := newKindsFake(t)
	f.override("GetUserByUsername", "ex-status-teamex").Return(nil, testAppErr("down"))
	require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise populate"), "could not look up @ex-status-teamex")
	require.Nil(t, f.manifest(t))
}

func TestExercise_SeedingANewKindFails(t *testing.T) {
	f := newKindsFake(t)
	// override puts the newest mock first: the email domain check gets the
	// team, and seeding's own load, the second call, fails.
	f.override("GetTeam", exerciseTeamID).Return(nil, testAppErr("down")).Once()
	f.override("GetTeam", exerciseTeamID).Return(f.team, nil).Once()
	reply := f.run("admin1", "/mattermost-request-hub exercise populate")
	require.Contains(t, reply, "could not finish seeding")
	require.Contains(t, reply, "could not load this team")
	require.Equal(t, exerciseStatePopulating, f.manifest(t).State)
	require.Len(t, f.manifest(t).Done, 1) // the team is loaded before any request is seeded
}

func TestExercise_ApprovalAfterTeardownRemovesWhatItMade(t *testing.T) {
	f := newKindsFake(t)
	m := f.populate(t)
	teamReq, botReq, hookReq := exerciseRequestID(t, m, pendingKindTeam), exerciseRequestID(t, m, pendingKindBot), exerciseRequestID(t, m, pendingKindWebhook)
	// As if teardown finished while these approvals ran.
	delete(f.kv, exerciseManifestKey(exerciseTeamID))

	for _, res := range []decisionResult{
		f.p.decideTeamRequest("admin1", teamReq, "", true),
		f.p.decideBotRequest("admin1", botReq, "", true),
		f.p.decideWebhookRequest("admin1", hookReq, "", true),
	} {
		require.Equal(t, exerciseRemovedText, res.Outcome)
	}

	require.Len(t, f.teams, 1)
	for _, team := range f.teams {
		require.NotZero(t, team.DeleteAt)
	}
	require.Empty(t, f.bots)
	require.Empty(t, f.hooks)
	require.Equal(t, []string{"bot", "hook"}, f.deleted)
	require.Nil(t, f.manifest(t))
}

func TestExercise_ApprovalAfterTeardownUndoFails(t *testing.T) {
	f := newKindsFake(t)
	m := f.populate(t)
	botReq, hookReq := exerciseRequestID(t, m, pendingKindBot), exerciseRequestID(t, m, pendingKindWebhook)
	delete(f.kv, exerciseManifestKey(exerciseTeamID))
	teamReq := exerciseRequestID(t, m, pendingKindTeam)
	f.fail["PermanentDeleteBot"] = true
	f.fail["deleteWebhook"] = true
	f.fail["DeleteTeam"] = true

	f.p.decideBotRequest("admin1", botReq, "", true)
	f.p.decideWebhookRequest("admin1", hookReq, "", true)
	f.p.decideTeamRequest("admin1", teamReq, "", true)

	require.Len(t, f.bots, 1)
	require.Len(t, f.hooks, 1)
	for _, what := range []string{"bot", "webhook", "team"} {
		f.api.AssertCalled(t, "LogWarn", "failed to remove exercise "+what+" after teardown; remove it by hand", "id", mock.Anything, "error", mock.Anything)
	}
}

func TestExercise_RecordingFailsAfterApproval(t *testing.T) {
	f := newKindsFake(t)
	m := f.populate(t)
	f.override("KVGet", exerciseManifestKey(exerciseTeamID)).Return(nil, testAppErr("down"))
	res := f.p.decideTeamRequest("admin1", exerciseRequestID(t, m, pendingKindTeam), "", true)
	require.Contains(t, res.Outcome, "created", "the team is kept")
	f.api.AssertCalled(t, "LogWarn", "failed to record exercise team; remove it by hand", "id", mock.Anything, "error", mock.Anything)
}

func TestExercise_BotWebhooksAreRecordedAndRemoved(t *testing.T) {
	f := newKindsFake(t)
	data, err := json.Marshal(allKindsScenario())
	require.NoError(t, err)
	f.setConfig(func(c *configuration) { c.ExerciseScenarioJSON = string(data) })
	f.serveExtras(t)
	m := f.populate(t)

	res := f.p.decideBotRequest("admin1", exerciseRequestID(t, m, pendingKindBot), "", true)
	require.Contains(t, res.Outcome, "created")
	require.NotContains(t, res.Outcome, "⚠️")
	m = f.manifest(t)
	require.Len(t, m.BotIDs, 1)
	require.Len(t, m.Webhooks, 2)
	for _, hook := range m.Webhooks {
		require.Equal(t, f.hooks[hook.ID], hook.Type, "recorded with the type it was made as")
	}
	// One was removed by hand already; teardown still finishes.
	delete(f.hooks, m.Webhooks[0].ID)

	reply := f.run("admin1", "/mattermost-request-hub exercise teardown")
	require.Contains(t, reply, "1 bots deleted, 1 webhooks deleted")
	require.Empty(t, f.hooks)
	require.Equal(t, []string{"hook", "bot"}, f.deleted, "webhooks go before the bot")
}

func TestExercise_BotWebhooksAfterTeardownAreRemoved(t *testing.T) {
	f := newKindsFake(t)
	data, err := json.Marshal(allKindsScenario())
	require.NoError(t, err)
	f.setConfig(func(c *configuration) { c.ExerciseScenarioJSON = string(data) })
	f.serveExtras(t)
	m := f.populate(t)
	// Teardown finishes once the bot is recorded and before its webhooks.
	f.override("AddChannelMember", mock.Anything, mock.Anything).Return(func(channelID, userID string) (*model.ChannelMember, *model.AppError) {
		delete(f.kv, exerciseManifestKey(exerciseTeamID))
		return &model.ChannelMember{ChannelId: channelID, UserId: userID}, nil
	})

	res := f.p.decideBotRequest("admin1", exerciseRequestID(t, m, pendingKindBot), "", true)
	require.Contains(t, res.Outcome, "created")
	require.Equal(t, []string{"hook", "hook"}, f.deleted, "both webhooks were removed as soon as they were made")
	require.Empty(t, f.hooks)
}

func TestExercise_RealApprovalsLeaveTheManifestAlone(t *testing.T) {
	f := newKindsFake(t)
	f.populate(t)
	before := f.kv[exerciseManifestKey(exerciseTeamID)]
	f.users["req1"] = &model.User{Id: "req1", Username: "req1"}
	req := &teamCreationRequest{ID: "realteamreq", RequesterID: "req1", DisplayName: "Real", Name: "real", Type: model.TeamOpen, CreatedAt: model.GetMillis()}
	require.NoError(t, f.p.storeTeamRequest(req))

	f.p.decideTeamRequest("admin1", "realteamreq", "", true)
	require.Len(t, f.teams, 1)
	require.Equal(t, before, f.kv[exerciseManifestKey(exerciseTeamID)])
}

func TestExercise_TeardownFindsUnrecordedBotsAndTeams(t *testing.T) {
	f := newKindsFake(t)
	f.populate(t)
	chief := f.persona("chiefops")
	// Made by approvals that stopped before recording them.
	f.bots["b1"] = &model.Bot{UserId: "b1", Username: "ex-status-teamex", OwnerId: f.persona("planner").Id}
	f.users["b1"] = &model.User{Id: "b1", Username: "ex-status-teamex", IsBot: true}
	f.teams["t1"] = &model.Team{Id: "t1", Name: "exercise-planning-cell-teamexer", DisplayName: "EXERCISE Silver Harbor Planning Cell"}
	f.teamMembers["t1/"+chief.Id] = &model.TeamMember{TeamId: "t1", UserId: chief.Id}

	reply := f.run("admin1", "/mattermost-request-hub exercise teardown")
	require.Contains(t, reply, "1 teams archived, 1 bots deleted")
	require.Empty(t, f.bots)
	require.NotZero(t, f.teams["t1"].DeleteAt)
}

func TestExercise_TeardownLeavesLookalikeBotsAndTeamsAlone(t *testing.T) {
	cases := map[string]func(f *kindsFake){
		"a user, not a bot": func(f *kindsFake) {
			f.users["b9"] = &model.User{Id: "b9", Username: "ex-status-teamex"}
		},
		"a bot someone else owns": func(f *kindsFake) {
			f.bots["b9"] = &model.Bot{UserId: "b9", OwnerId: "admin1"}
			f.users["b9"] = &model.User{Id: "b9", Username: "ex-status-teamex", IsBot: true}
		},
		"a team not marked EXERCISE": func(f *kindsFake) {
			f.teams["t9"] = &model.Team{Id: "t9", Name: "exercise-planning-cell-teamexer", DisplayName: "Planning Cell"}
		},
		"a marked team the requester isn't on": func(f *kindsFake) {
			f.teams["t9"] = &model.Team{Id: "t9", Name: "exercise-planning-cell-teamexer", DisplayName: "EXERCISE Planning Cell"}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newKindsFake(t)
			f.populate(t)
			setup(f)
			reply := f.run("admin1", "/mattermost-request-hub exercise teardown")
			require.Contains(t, reply, "Removed the exercise data")
			require.NotContains(t, reply, "teams archived")
			require.NotContains(t, reply, "bots deleted")
			if team, ok := f.teams["t9"]; ok {
				require.Zero(t, team.DeleteAt)
			}
			if _, ok := f.bots["b9"]; ok || name == "a user, not a bot" {
				require.Contains(t, f.users, "b9")
			}
		})
	}
}

func TestExercise_TeardownFailuresKeepTheManifest(t *testing.T) {
	for _, method := range []string{"deleteWebhook", "GetBot", "PermanentDeleteBot", "UpdateTeam", "DeleteTeam", "GetTeamMember", "UpdateTeamMemberRoles", "DeleteTeamMember", "GetTeamByName"} {
		t.Run(method, func(t *testing.T) {
			f := newKindsFake(t)
			f.approveAll(t, f.populate(t))
			if method == "GetTeamByName" {
				// Only the lookup for an unrecorded team fails.
				f.setManifest(t, func(m *exerciseManifest) { m.TeamIDs = nil })
			}
			f.fail[method] = true
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not be removed")
			require.NotNil(t, f.manifest(t))

			delete(f.fail, method)
			require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "Removed the exercise data")
		})
	}
}

func TestExercise_TeardownFallbackLookupFailures(t *testing.T) {
	t.Run("bot username", func(t *testing.T) {
		f := newKindsFake(t)
		f.populate(t)
		f.override("GetUserByUsername", "ex-status-teamex").Return(nil, testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "1 exercise items could not be removed")
	})
	t.Run("bot record", func(t *testing.T) {
		f := newKindsFake(t)
		f.populate(t)
		f.users["b1"] = &model.User{Id: "b1", Username: "ex-status-teamex", IsBot: true}
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "1 exercise items could not be removed")
	})
	t.Run("bot owner", func(t *testing.T) {
		f := newKindsFake(t)
		f.populate(t)
		f.bots["b1"] = &model.Bot{UserId: "b1", OwnerId: "o1"}
		f.users["b1"] = &model.User{Id: "b1", Username: "ex-status-teamex", IsBot: true}
		f.override("KVGet", kvExercisePersonaPrefix+"o1").Return(nil, testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "1 exercise items could not be removed")
	})
	t.Run("team requester gone", func(t *testing.T) {
		f := newKindsFake(t)
		f.populate(t)
		f.teams["t1"] = &model.Team{Id: "t1", Name: "exercise-planning-cell-teamexer", DisplayName: "EXERCISE Cell"}
		delete(f.users, f.persona("chiefops").Id)
		reply := f.run("admin1", "/mattermost-request-hub exercise teardown")
		require.Contains(t, reply, "Removed the exercise data")
		require.NotContains(t, reply, "teams archived")
		require.Zero(t, f.teams["t1"].DeleteAt)
	})
	t.Run("team requester lookup", func(t *testing.T) {
		f := newKindsFake(t)
		f.populate(t)
		f.teams["t1"] = &model.Team{Id: "t1", Name: "exercise-planning-cell-teamexer", DisplayName: "EXERCISE Cell"}
		f.override("GetUserByUsername", f.persona("chiefops").Username).Return(nil, testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not be removed")
		require.Zero(t, f.teams["t1"].DeleteAt)
	})
	t.Run("team member lookup", func(t *testing.T) {
		f := newKindsFake(t)
		f.populate(t)
		f.teams["t1"] = &model.Team{Id: "t1", Name: "exercise-planning-cell-teamexer", DisplayName: "EXERCISE Cell"}
		f.override("GetTeamMember", "t1", mock.Anything).Return(nil, testAppErr("down"))
		require.Contains(t, f.run("admin1", "/mattermost-request-hub exercise teardown"), "could not be removed")
		require.Zero(t, f.teams["t1"].DeleteAt)
	})
}

func TestArchiveExerciseTeam(t *testing.T) {
	f := newKindsFake(t)
	long := strings.Repeat("a", model.TeamNameMaxLength-4) + "-xyz"
	f.teams["t1"] = &model.Team{Id: "t1", Name: long}
	ok, err := f.p.archiveExerciseTeam("t1")
	require.NoError(t, err)
	require.True(t, ok)
	require.LessOrEqual(t, len(f.teams["t1"].Name), model.TeamNameMaxLength)
	require.True(t, strings.HasSuffix(f.teams["t1"].Name, "-t1"))

	ok, err = f.p.archiveExerciseTeam("t1")
	require.NoError(t, err)
	require.False(t, ok, "already archived")
	ok, err = f.p.archiveExerciseTeam("missing")
	require.NoError(t, err)
	require.False(t, ok)

	f.fail["GetTeam"] = true
	_, err = f.p.archiveExerciseTeam("t1")
	require.ErrorContains(t, err, "failed to load the team")
}

func TestDeleteExerciseWebhookAndBot(t *testing.T) {
	f := newKindsFake(t)
	ok, err := f.p.deleteExerciseWebhook(exerciseWebhook{ID: "gone", Type: webhookTypeOutgoing}, "admin1")
	require.NoError(t, err)
	require.False(t, ok, "already gone")

	ok, err = f.p.deleteExerciseBot("gone")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestExerciseStatus_UncheckedTeamsAndBots(t *testing.T) {
	f := newKindsFake(t)
	f.approveAll(t, f.populate(t))
	f.fail["GetBot"] = true
	m := f.manifest(t)
	f.override("GetTeam", m.TeamIDs[0]).Return(nil, testAppErr("down"))
	status := f.run("admin1", "/mattermost-request-hub exercise status")
	require.Contains(t, status, "| Teams (not archived) | 1 | 0 |")
	require.Contains(t, status, "| Bots | 1 | 0 |")
	require.Contains(t, status, "2 items could not be checked")
}

func TestExercise_SeedingACardFailsForEachKind(t *testing.T) {
	for title, what := range map[string]string{
		"EXERCISE Team creation request":     "team creation request",
		"EXERCISE Team Admin request":        "Team Admin request",
		"EXERCISE Team member request":       "team member request",
		"EXERCISE Team guest invite request": "guest invite request",
		"EXERCISE Bot account request":       "bot account request",
		"EXERCISE Webhook request":           "webhook request",
	} {
		t.Run(what, func(t *testing.T) {
			f := newKindsFake(t)
			f.override("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
				atts := post.Attachments()
				return len(atts) > 0 && atts[0].Title == title
			})).Return(nil, testAppErr("down"))
			reply := f.run("admin1", "/mattermost-request-hub exercise populate")
			require.Contains(t, reply, "could not post a "+what+"'s approval card")
		})
	}
}

func TestExercise_SeedsOutgoingWebhooks(t *testing.T) {
	f := newKindsFake(t)
	sc := allKindsScenario()
	data, err := json.Marshal(sc)
	require.NoError(t, err)
	f.setConfig(func(c *configuration) { c.ExerciseScenarioJSON = string(data) })
	f.populate(t)

	m := f.manifest(t)
	var bot botRequest
	require.NoError(t, json.Unmarshal(f.kv[kvBotRequestPrefix+exerciseRequestID(t, m, pendingKindBot)], &bot))
	require.Equal(t, exerciseCallbackURL, bot.OutgoingWebhookCallbackURL)
	require.NotEmpty(t, bot.OutgoingWebhookChannelID)
	require.NotEmpty(t, bot.IncomingWebhookChannelID)
	require.Equal(t, f.persona("out").Id, bot.OwnerUserID)
	var hook webhookRequest
	require.NoError(t, json.Unmarshal(f.kv[kvWebhookRequestPrefix+exerciseRequestID(t, m, pendingKindWebhook)], &hook))
	require.Equal(t, "https://exercise.invalid/hook", hook.CallbackURL)
}

func TestExercise_SeedsToSystemAdminDMsWithoutAnApprovalChannel(t *testing.T) {
	f := newKindsFake(t)
	f.setConfig(func(c *configuration) { c.ApprovalTeam = "" })
	f.populate(t)
	cards := 0
	for _, post := range f.posts {
		if atts := post.Attachments(); len(atts) > 0 && len(atts[0].Actions) > 0 {
			cards++
			require.Equal(t, "dm", post.ChannelId, atts[0].Title)
		}
	}
	require.Equal(t, 20, cards, "each of the two System Admins gets every card")
}

func TestTwoStepHeadersMentionNobodyForExercises(t *testing.T) {
	p := &Plugin{}
	for _, header := range []string{p.botApprovalHeader(true), botRetryHeader(true), p.webhookApprovalHeader(true), webhookRetryHeader(true)} {
		require.True(t, strings.HasPrefix(header, "EXERCISE: "), header)
		require.NotContains(t, header, "@")
	}
	for _, header := range []string{p.botApprovalHeader(false), botRetryHeader(false), p.webhookApprovalHeader(false), webhookRetryHeader(false)} {
		require.True(t, strings.HasPrefix(header, "@channel "), header)
	}
}
