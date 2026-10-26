package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// waitFor fails the test if done isn't closed within a few seconds.
func waitFor(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// --- request.go ---

func TestLogAudit_LinksThreadAndLogsPostFailure(t *testing.T) {
	p, api := newApprovalTeamPlugin(t)
	stubApprovalTeamLookup(t, api)
	api.On("GetChannelByName", "t1", "mattermost-audit-requests", false).
		Return(pluginChannel("auditch", "mattermost-audit-requests"), nil)
	stubSiteURL(api, "https://mm.example.com/")
	api.On("GetPost", "ap").Return(&model.Post{Id: "ap", ChannelId: "appch"}, nil)
	api.On("GetChannel", "appch").Return(&model.Channel{Id: "appch", TeamId: "t1"}, nil)
	api.On("GetTeam", "t1").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		att := post.Attachments()
		return post.ChannelId == "auditch" && post.Message == "" && len(att) == 1 &&
			att[0].Title == "• Audit" &&
			att[0].Text == "note\n\n[Open ticket thread](https://mm.example.com/appteam/pl/ap)"
	})).Return(nil, testAppErr("down"))

	p.logAudit(auditOutcome(99), " note ", "ap")

	api.AssertCalled(t, "LogWarn", "audit post failed", "channel_id", "auditch", "error", mock.Anything)
}

func TestAuditCardStyle_UnknownKindIsNeutral(t *testing.T) {
	color, title := auditCardStyle(auditOutcome(99))
	require.Equal(t, colorPending, color)
	require.Equal(t, "• Audit", title)
}

func TestPostPermalink_MissingPiecesGiveNoLink(t *testing.T) {
	tests := []struct {
		name  string
		setup func(api *plugintest.API)
	}{
		{"no site URL", func(api *plugintest.API) {
			stubSiteURL(api, "/")
		}},
		{"post not found", func(api *plugintest.API) {
			stubSiteURL(api, "https://mm.example.com")
			api.On("GetPost", "post1").Return(nil, testAppErr("gone"))
		}},
		{"channel not found", func(api *plugintest.API) {
			stubSiteURL(api, "https://mm.example.com")
			api.On("GetPost", "post1").Return(&model.Post{Id: "post1", ChannelId: "ch1"}, nil)
			api.On("GetChannel", "ch1").Return(nil, testAppErr("gone"))
		}},
		{"DM channel has no team", func(api *plugintest.API) {
			stubSiteURL(api, "https://mm.example.com")
			api.On("GetPost", "post1").Return(&model.Post{Id: "post1", ChannelId: "ch1"}, nil)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1"}, nil)
		}},
		{"team not found", func(api *plugintest.API) {
			stubSiteURL(api, "https://mm.example.com")
			api.On("GetPost", "post1").Return(&model.Post{Id: "post1", ChannelId: "ch1"}, nil)
			api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
			api.On("GetTeam", "team1").Return(nil, testAppErr("gone"))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			tc.setup(api)
			require.Empty(t, p.postPermalink("post1"))
		})
	}
}

func TestApprovalAttachment_ShowsSidebarCategory(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)

	att := p.approvalAttachment(&channelRequest{ID: "r1", TeamID: "team1", Name: "ops-x", CategoryName: "Ops_Work"},
		&model.User{Username: "req"})

	var found bool
	for _, f := range att.Fields {
		if f.Title == "Sidebar category" {
			found = true
			require.Equal(t, "Ops\\_Work", f.Value)
		}
	}
	require.True(t, found)
}

func TestTeamDisplayName_Fallbacks(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetTeam", "gone").Return(nil, testAppErr("gone"))
	api.On("GetTeam", "named").Return(&model.Team{Id: "named", DisplayName: " ", Name: "ops"}, nil)
	api.On("GetTeam", "blank").Return(&model.Team{Id: "blank"}, nil)

	require.Equal(t, "(unknown team)", p.teamDisplayName(""))
	require.Equal(t, "gone", p.teamDisplayName("gone"))
	require.Equal(t, "ops", p.teamDisplayName("named"))
	require.Equal(t, "blank", p.teamDisplayName("blank"))
}

func TestAddChannelToSidebarCategory_NoCategoryDoesNothing(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	p.addChannelToSidebarCategory("ch1", "team1", &channelRequest{RequesterID: "u1"})

	api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
}

func TestAddChannelToSidebarCategory_PlacesEachUserOnce(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetChannelSidebarCategories", "u1", "team1").Return(nil, testAppErr("down")).Once()
	api.On("GetChannelSidebarCategories", "m1", "team1").Return(nil, nil).Once()
	api.On("CreateChannelSidebarCategory", "m1", "team1", mock.Anything).Return(nil, nil).Once()
	api.On("GetChannelSidebarCategories", "a1", "team1").Return(nil, nil).Once()
	api.On("CreateChannelSidebarCategory", "a1", "team1", mock.Anything).Return(nil, nil).Once()

	p.addChannelToSidebarCategory("ch1", "team1", &channelRequest{
		RequesterID:    "u1",
		MemberIDs:      []string{"m1", "", "u1"},
		AdminMemberIDs: []string{"a1", "m1"},
		CategoryName:   "Work",
	})

	api.AssertCalled(t, "LogWarn", "failed to place channel in sidebar category",
		"user_id", "u1", "category", "Work", "error", mock.Anything)
}

func TestPlaceChannelInCategory_Outcomes(t *testing.T) {
	work := func(channels ...string) *model.OrderedSidebarCategories {
		return &model.OrderedSidebarCategories{Categories: []*model.SidebarCategoryWithChannels{
			nil,
			{SidebarCategory: model.SidebarCategory{DisplayName: "Other"}},
			{SidebarCategory: model.SidebarCategory{Id: "cat1", DisplayName: "WORK"}, Channels: channels},
		}}
	}

	t.Run("blank name", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		require.NoError(t, p.placeChannelInCategory("u1", "team1", "ch1", "  "))
		api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
	})

	t.Run("lookup failure", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetChannelSidebarCategories", "u1", "team1").Return(nil, testAppErr("down"))
		require.ErrorContains(t, p.placeChannelInCategory("u1", "team1", "ch1", "work"), "down")
	})

	t.Run("already in matching category", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetChannelSidebarCategories", "u1", "team1").Return(work("ch1"), nil)
		require.NoError(t, p.placeChannelInCategory("u1", "team1", "ch1", "work"))
		api.AssertNotCalled(t, "UpdateChannelSidebarCategories", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("added to matching category", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetChannelSidebarCategories", "u1", "team1").Return(work("ch0"), nil)
		api.On("UpdateChannelSidebarCategories", "u1", "team1", mock.MatchedBy(func(cats []*model.SidebarCategoryWithChannels) bool {
			return len(cats) == 1 && cats[0].Id == "cat1" && len(cats[0].Channels) == 2 && cats[0].Channels[1] == "ch1"
		})).Return(nil, nil)
		require.NoError(t, p.placeChannelInCategory("u1", "team1", "ch1", "work"))
	})

	t.Run("update failure", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetChannelSidebarCategories", "u1", "team1").Return(work(), nil)
		api.On("UpdateChannelSidebarCategories", "u1", "team1", mock.Anything).Return(nil, testAppErr("update failed"))
		require.ErrorContains(t, p.placeChannelInCategory("u1", "team1", "ch1", "work"), "update failed")
	})

	t.Run("create failure", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetChannelSidebarCategories", "u1", "team1").Return(nil, nil)
		api.On("CreateChannelSidebarCategory", "u1", "team1", mock.Anything).Return(nil, testAppErr("create failed"))
		require.ErrorContains(t, p.placeChannelInCategory("u1", "team1", "ch1", "work"), "create failed")
	})
}

// --- configuration.go ---

func TestGetConfiguration_UnsetReturnsEmpty(t *testing.T) {
	p := &Plugin{}
	cfg := p.getConfiguration()
	require.NotNil(t, cfg)
	require.Equal(t, configuration{}, *cfg)
}

func TestTwoStepPoolFingerprint_NilAndTrimmed(t *testing.T) {
	require.Empty(t, twoStepPoolFingerprint(nil))
	require.Equal(t, twoStepModeUserGroup+"|sec|||",
		twoStepPoolFingerprint(&configuration{TwoStepApproverMode: "groups", SecurityApproverGroup: " sec "}))
}

func TestConfigurationToMap_KeepsExportedSettingsOnly(t *testing.T) {
	m, err := configurationToMap(&configuration{
		ApprovalTeam:        "appteam",
		ChannelNamePrefixes: "ops-|Ops|48",
		prefixes:            []channelPrefix{{Prefix: "ops-"}},
	})
	require.NoError(t, err)
	require.Equal(t, "appteam", m["ApprovalTeam"])
	require.Equal(t, "ops-|Ops|48", m["ChannelNamePrefixes"])
	require.NotContains(t, m, "prefixes")
}

func TestBumpLegacyDefaultSuffixMax_SkipsShortLines(t *testing.T) {
	out, changed := bumpLegacyDefaultSuffixMax("ops-|Ops\ntown-|Town|24")
	require.True(t, changed)
	require.Equal(t, "ops-|Ops\ntown-|Town|48", out)
}

func TestCompileSuffixRule_PatternTooDeepOnceAnchored(t *testing.T) {
	// Find a nesting depth that compiles alone but not once wrapped in the
	// anchoring group; the limit belongs to the regexp package.
	var pattern string
	for n := 900; n <= 1100; n++ {
		candidate := strings.Repeat("(", n) + "a" + strings.Repeat(")", n)
		_, aloneErr := regexp.Compile(candidate)
		_, wrappedErr := regexp.Compile("^(?:" + candidate + ")$")
		if aloneErr == nil && wrappedErr != nil {
			pattern = candidate
			break
		}
	}
	if pattern == "" {
		t.Skip("regexp nesting limit not reachable by one extra group")
	}

	var logged []string
	re, raw := compileSuffixRule(pattern, "ops-", func(msg string, _ ...any) { logged = append(logged, msg) })

	require.Equal(t, pattern, raw)
	require.False(t, re.MatchString("a"))
	require.Len(t, logged, 1)
}

func TestOnConfigurationChange_MigratesLegacySuffixMax(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		args.Get(0).(*configuration).ChannelNamePrefixes = "ops-|Ops|24"
	}).Return(nil)
	api.On("KVGet", kvPrefixMaxLenV48).Return(nil, nil)
	api.On("KVSet", kvPrefixMaxLenV48, []byte("1")).Return(nil)
	api.On("SavePluginConfig", mock.MatchedBy(func(m map[string]any) bool {
		return m["ChannelNamePrefixes"] == "ops-|Ops|48"
	})).Return(nil)

	require.NoError(t, p.OnConfigurationChange())

	cfg := p.getConfiguration()
	require.Equal(t, "ops-|Ops|48", cfg.ChannelNamePrefixes)
	require.Len(t, cfg.prefixes, 1)
	api.AssertNotCalled(t, "KVDelete", kvPrefixMaxLenV48)
}

func TestOnConfigurationChange_SuffixMigrationSaveFailureRetriesLater(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		args.Get(0).(*configuration).ChannelNamePrefixes = "ops-|Ops|24"
	}).Return(nil)
	api.On("KVGet", kvPrefixMaxLenV48).Return(nil, nil)
	api.On("KVSet", kvPrefixMaxLenV48, []byte("1")).Return(nil)
	api.On("SavePluginConfig", mock.Anything).Return(testAppErr("save failed"))
	api.On("KVDelete", kvPrefixMaxLenV48).Return(nil)

	require.NoError(t, p.OnConfigurationChange())
	require.Equal(t, "ops-|Ops|48", p.getConfiguration().ChannelNamePrefixes)
}

func TestOnConfigurationChange_NoLegacySuffixMaxJustMarksDone(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		args.Get(0).(*configuration).ChannelNamePrefixes = "ops-|Ops|32"
	}).Return(nil)
	api.On("KVGet", kvPrefixMaxLenV48).Return(nil, nil)
	api.On("KVSet", kvPrefixMaxLenV48, []byte("1")).Return(nil)

	require.NoError(t, p.OnConfigurationChange())

	api.AssertNotCalled(t, "SavePluginConfig", mock.Anything)
	require.Equal(t, "ops-|Ops|32", p.getConfiguration().ChannelNamePrefixes)
}

func TestOnConfigurationChange_EnablingPerTeamChannelsStartsSweep(t *testing.T) {
	api := &plugintest.API{}
	// Keep the log stubs: the background sweep still logs after the wait.
	stubLogLevels(api)
	p := newTestPlugin(api)
	stubLoadConfiguration(api, configuration{PerTeamApprovalChannels: true})
	done := make(chan struct{})
	api.On("GetTeams").Run(func(mock.Arguments) { close(done) }).Return(nil, testAppErr("down")).Once()

	require.NoError(t, p.OnConfigurationChange())

	waitFor(t, done, "per-team channel sweep")
	require.True(t, p.getConfiguration().PerTeamApprovalChannels)
}

func TestOnConfigurationChange_TwoStepPoolChangeReseedsChannels(t *testing.T) {
	api := &plugintest.API{}
	done := make(chan struct{})
	// Registered before the catch-all log stubs so it is matched first.
	api.On("LogInfo", "two-step channel access: reseeding approvers into bot/webhook channels", "count", 0).
		Run(func(mock.Arguments) { close(done) }).Once()
	// Keep the log stubs: the background reseed still logs after the wait.
	stubLogLevels(api)
	p := newTestPlugin(api)
	stubLoadConfiguration(api, configuration{TwoStepApproverMode: twoStepModeSystemAdmin})

	require.NoError(t, p.OnConfigurationChange())

	waitFor(t, done, "two-step channel reseed")
}

// --- plugin.go ---

// bundleWithIcon returns a bundle directory holding assets/mattermost-icon.png.
func bundleWithIcon(t *testing.T) (dir string, icon []byte) {
	t.Helper()
	dir = t.TempDir()
	icon = []byte("png-bytes")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "mattermost-icon.png"), icon, 0o600))
	return dir, icon
}

func TestReadBundleAsset_RejectsNonBasenames(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	for _, name := range []string{"", "assets/x.png", "..", "a..b"} {
		_, err := p.readBundleAsset(name)
		require.ErrorContains(t, err, "invalid asset name", name)
	}
	api.AssertNotCalled(t, "GetBundlePath")
}

func TestReadBundleAsset_ReadsFromAssetsDir(t *testing.T) {
	dir, icon := bundleWithIcon(t)
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetBundlePath").Return(dir, nil)

	data, err := p.readBundleAsset("mattermost-icon.png")

	require.NoError(t, err)
	require.Equal(t, icon, data)
}

func TestOnActivate_CommandRegistrationFailureStops(t *testing.T) {
	dir, icon := bundleWithIcon(t)
	api := &plugintest.API{}
	stubLogLevels(api)
	stubPendingMigrated(api)
	defer api.AssertExpectations(t)
	p := &Plugin{}
	p.SetAPI(api)
	api.On("EnsureBotUser", mock.Anything).Return("bot-user-id", nil)
	api.On("GetBundlePath").Return(dir, nil)
	api.On("SetProfileImage", "bot-user-id", icon).Return(testAppErr("image rejected"))
	api.On("RegisterCommand", mock.Anything).Return(testAppErr("taken"))

	err := p.OnActivate()

	require.ErrorContains(t, err, "failed to register slash command")
	api.AssertCalled(t, "LogWarn", "failed to set bot profile image", "error", mock.Anything)
}

func TestOnActivate_BackgroundSetupSeedsChannelsAndSweepsTeams(t *testing.T) {
	dir, icon := bundleWithIcon(t)
	api := &plugintest.API{}
	stubLogLevels(api)
	stubPendingMigrated(api)
	p := &Plugin{}
	p.SetAPI(api)
	p.setConfiguration(&configuration{
		ApprovalTeam:            "appteam",
		PerTeamApprovalChannels: true,
		TwoStepApproverMode:     twoStepModeSystemAdmin,
	})

	api.On("EnsureBotUser", mock.Anything).Return("bot-user-id", nil)
	api.On("GetBundlePath").Return(dir, nil)
	api.On("SetProfileImage", "bot-user-id", icon).Return(nil)
	api.On("RegisterCommand", mock.Anything).Return(nil)

	appch := pluginChannel("appch", "approvals")
	appch.TeamId = "t1"
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "t1", Name: "appteam"}, nil)
	api.On("KVGet", kvApprovalTeamKey).Return(approvalTeamRecordBytes(t, "appteam", "t1"), nil)
	api.On("KVGet", kvSidebarCategoryMigrated).Return(nil, nil)
	api.On("KVGet", mock.Anything).Return(nil, nil)
	api.On("GetChannelByName", "t1", mock.Anything, false).Return(appch, nil)
	api.On("GetChannel", "appch").Return(appch, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sa1", Roles: "system_user system_admin"}}, nil)
	api.On("AddChannelMember", mock.Anything, "sa1").Return(&model.ChannelMember{}, nil).Maybe()
	api.On("CreateTeamMember", "t1", "sa1").Return(&model.TeamMember{}, nil).Maybe()
	api.On("GetChannelSidebarCategories", "sa1", "t1").Return(nil, testAppErr("down")).Maybe()
	api.On("GetTeams").Return([]*model.Team{}, nil)
	api.On("GetUsersInChannel", "appch", "username", 0, 200).Return([]*model.User{}, nil)
	// No SiteURL, so the docs bookmark sweep waits for a later activation.
	api.On("GetConfig").Return(&model.Config{}).Maybe()
	done := make(chan struct{})
	api.On("KVSet", kvSidebarCategoryMigrated, []byte("1")).Run(func(mock.Arguments) { close(done) }).Return(nil)

	require.NoError(t, p.OnActivate())
	defer func() { require.NoError(t, p.OnDeactivate()) }()
	waitFor(t, done, "background channel setup")

	// The System Admin is seeded into the approval channel.
	api.AssertCalled(t, "AddChannelMember", "appch", "sa1")
	// Once for the per-team sweep and once for the sidebar migration.
	api.AssertNumberOfCalls(t, "GetTeams", 2)
	api.AssertCalled(t, "SetProfileImage", "bot-user-id", icon)
}

// --- ticket.go ---

func TestPostTicketCreatedDM_FailuresReturnNoIDs(t *testing.T) {
	card := &model.MessageAttachment{Title: "Team request"}

	t.Run("missing requester or card", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		root, ch := p.postTicketCreatedDM("", card, "")
		require.Empty(t, root)
		require.Empty(t, ch)
		root, ch = p.postTicketCreatedDM("u1", nil, "")
		require.Empty(t, root)
		require.Empty(t, ch)
		api.AssertNotCalled(t, "GetDirectChannel", mock.Anything, mock.Anything)
	})

	t.Run("DM unavailable", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		api.On("GetDirectChannel", "u1", "bot-user-id").Return(nil, testAppErr("down"))
		root, ch := p.postTicketCreatedDM("u1", card, "")
		require.Empty(t, root)
		require.Empty(t, ch)
		api.AssertNotCalled(t, "CreatePost", mock.Anything)
	})

	t.Run("post failure uses default intro", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm"}, nil)
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			att := post.Attachments()
			return strings.HasPrefix(post.Message, "Your request has been submitted for approval.") &&
				len(att) == 1 && att[0].Color == colorPending
		})).Return(nil, testAppErr("down"))
		root, ch := p.postTicketCreatedDM("u1", card, "")
		require.Empty(t, root)
		require.Empty(t, ch)
	})
}

// --- kv_cas.go / approval_engine.go ---

func TestKVUpdateIfUnchanged_UnencodableValueFails(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	updated, err := p.kvUpdateIfUnchanged("k", nil, make(chan int))

	require.False(t, updated)
	require.ErrorContains(t, err, "failed to marshal for KV update")
	api.AssertNotCalled(t, "KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything)
}

func TestPlanApproval_SystemStepWithoutEitherRole(t *testing.T) {
	step, fill, reason := planApproval(twoStepState{SecurityApproverID: "sec1"}, false, false, "u1", "req")
	require.Zero(t, step)
	require.False(t, fill)
	require.Equal(t, "You don't have permission to approve this request.", reason)
}
