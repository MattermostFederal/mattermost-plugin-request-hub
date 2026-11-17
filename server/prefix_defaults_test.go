package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// logCollector records the messages passed to a log function.
type logCollector struct{ msgs []string }

func (l *logCollector) log(msg string, _ ...any) { l.msgs = append(l.msgs, msg) }

func testPrefixes(t *testing.T) []channelPrefix {
	t.Helper()
	return parsePrefixList("sec-|Security|32\nsec-ir-|Incident response|32\nproj-|Projects|32", func(string, ...any) {})
}

func TestParsePrefixDefaults(t *testing.T) {
	prefixes := testPrefixes(t)

	t.Run("blank is no defaults", func(t *testing.T) {
		require.Nil(t, parsePrefixDefaults("  ", prefixes, t.Fatalf))
	})

	t.Run("invalid JSON is logged and ignored", func(t *testing.T) {
		var logs logCollector
		require.Nil(t, parsePrefixDefaults("{not json", prefixes, logs.log))
		require.Len(t, logs.msgs, 1)
		require.Contains(t, logs.msgs[0], "not valid JSON")
	})

	t.Run("normalizes and keeps entries in range", func(t *testing.T) {
		var logs logCollector
		got := parsePrefixDefaults(`{
			" sec- ": {"channel_type": "p", "purpose": "  SOC work ", "header": " Runbook ", "members": [" @Alice", "bob", "alice", ""]},
			"proj-": {"channel_type": "X", "purpose": "`+strings.Repeat("a", maxPurposeLen+1)+`"},
			"gone-": {"header": "kept until the prefix comes back"},
			"sec-ir-": {}
		}`, prefixes, logs.log)

		require.Equal(t, map[string]prefixDefaults{
			"sec-":  {ChannelType: "P", Purpose: "SOC work", Header: "Runbook", Members: []string{"alice", "bob"}},
			"gone-": {Header: "kept until the prefix comes back"},
		}, got)
		require.Len(t, logs.msgs, 3) // unknown type, long purpose, unknown prefix
	})

	t.Run("too long header and too many members", func(t *testing.T) {
		var logs logCollector
		members := make([]string, maxMembersPerList+5)
		for i := range members {
			members[i] = "u" + strings.Repeat("x", i+1)
		}
		raw := `{"sec-": {"header": "` + strings.Repeat("h", model.ChannelHeaderMaxRunes+1) + `", "members": ["` + strings.Join(members, `","`) + `"]}}`
		got := parsePrefixDefaults(raw, prefixes, logs.log)
		require.Empty(t, got["sec-"].Header)
		require.Len(t, got["sec-"].Members, maxMembersPerList)
		require.Len(t, logs.msgs, 2)
	})
}

func TestDefaultsForChannelName_UsesTheLongestPrefix(t *testing.T) {
	c := &configuration{prefixes: testPrefixes(t), prefixDefaults: map[string]prefixDefaults{
		"sec-":    {Header: "security"},
		"sec-ir-": {Header: "incident"},
	}}

	d, ok := c.defaultsForChannelName("sec-ir-phish")
	require.True(t, ok)
	require.Equal(t, "incident", d.Header)

	d, ok = c.defaultsForChannelName("sec-audit")
	require.True(t, ok)
	require.Equal(t, "security", d.Header)

	_, ok = c.defaultsForChannelName("proj-x")
	require.False(t, ok)
	_, ok = c.defaultsForChannelName("other")
	require.False(t, ok)
}

func TestApplyPrefixDefaultsAtSubmit(t *testing.T) {
	c := &configuration{prefixes: testPrefixes(t), prefixDefaults: map[string]prefixDefaults{
		"sec-": {ChannelType: "P", Purpose: "SOC work"},
	}}

	req := &channelRequest{Name: "sec-x", ChannelType: "O"}
	c.applyPrefixDefaultsAtSubmit(req)
	require.Equal(t, "P", req.ChannelType)
	require.Equal(t, "SOC work", req.Purpose)

	req = &channelRequest{Name: "sec-x", ChannelType: "O", Purpose: "mine"}
	c.applyPrefixDefaultsAtSubmit(req)
	require.Equal(t, "mine", req.Purpose, "the requester's purpose wins")

	req = &channelRequest{Name: "proj-x", ChannelType: "O"}
	c.applyPrefixDefaultsAtSubmit(req)
	require.Equal(t, "O", req.ChannelType)
	require.Empty(t, req.Purpose)
}

func TestCreateChannelForRequest_AppliesPrefixDefaults(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{prefixes: testPrefixes(t), prefixDefaults: map[string]prefixDefaults{
		"sec-": {Header: "Runbook", Members: []string{"alice", "bob", "carol", "dave", "req"}},
	}})

	api.On("CreateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.Header == "Runbook" && c.Name == "sec-x"
	})).Return(&model.Channel{Id: "ch1", Name: "sec-x"}, nil)
	stubBotJoinsCreatedChannel(api, "team1", "ch1")
	api.On("GetUserByUsername", "alice").Return(&model.User{Id: "u_alice"}, nil)
	api.On("GetUserByUsername", "bob").Return(&model.User{Id: "u_bob"}, nil)
	api.On("GetUserByUsername", "carol").Return(&model.User{Id: "u_carol", DeleteAt: 1}, nil)
	api.On("GetUserByUsername", "dave").Return(nil, testAppErr("not found"))
	api.On("GetUserByUsername", "req").Return(&model.User{Id: "u_req"}, nil)
	api.On("GetTeamMember", "team1", "u_alice").Return(&model.TeamMember{}, nil)
	api.On("GetTeamMember", "team1", "u_bob").Return(nil, testAppErr("not a member"))
	api.On("GetTeamMember", "team1", "u_req").Return(&model.TeamMember{}, nil)
	api.On("AddChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil).Once()
	api.On("AddChannelMember", "ch1", "u_alice").Return(&model.ChannelMember{}, nil).Once()
	api.On("UpdateChannelMemberRoles", "ch1", "u_req", channelAdminRoles).Return(&model.ChannelMember{}, nil)

	req := &channelRequest{RequesterID: "u_req", TeamID: "team1", Name: "sec-x", ChannelType: "P"}
	_, err := p.createChannelForRequest(req)
	require.NoError(t, err)

	// The requester is a default member too, but is added once and only
	// listed as the requester.
	require.Equal(t, []string{"u_alice"}, req.MemberIDs)
	require.Equal(t,
		"Default members not added: @bob (not on this team), @carol (deactivated), @dave (no such user)",
		skippedDefaultMembersLine(req.defaultsSkipped))
}

func TestPostWelcomeMessage_NamesSkippedDefaultMembers(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	var posted string
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		posted = args.Get(0).(*model.Post).Message
	}).Return(&model.Post{}, nil)

	req := &channelRequest{defaultsSkipped: []skippedDefaultMember{{"bob", "not on this team"}}}
	p.postWelcomeMessage(&model.Channel{Id: "ch1"}, req, &model.User{Id: "u_req", Username: "req"}, nil)

	require.Contains(t, posted, "\nDefault members not added: @bob (not on this team)")
}

func TestApprovalAttachment_SummarizesPrefixDefaults(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{prefixes: testPrefixes(t), prefixDefaults: map[string]prefixDefaults{
		"sec-":  {Header: "Runbook", Members: []string{"alice"}},
		"proj-": {ChannelType: "P"},
	}})
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)

	att := p.approvalAttachment(&channelRequest{ID: "r1", TeamID: "team1", Name: "sec-x"}, &model.User{Username: "req"})
	require.Contains(t, att.Text, "**Prefix defaults:** Approve also sets the channel header and adds @alice if they're on the team.")

	// Visibility alone is already on the card, so nothing extra.
	att = p.approvalAttachment(&channelRequest{ID: "r2", TeamID: "team1", Name: "proj-x"}, &model.User{Username: "req"})
	require.NotContains(t, att.Text, "Prefix defaults")
}

func TestHandlePrefixes_IncludesPrefixDefaults(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{
		prefixes:       parsePrefixList("sec-|Security|8\nops-", func(string, ...any) {}),
		prefixDefaults: map[string]prefixDefaults{"sec-": {ChannelType: "P", Purpose: "SOC work", Header: "not served"}},
	})

	w := callHandler(p.handlePrefixes, http.MethodGet, routePrefixes, "u1", "")

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `[
		{"prefix":"sec-","description":"Security","suffix_regex":"8","channel_type":"P","default_purpose":"SOC work"},
		{"prefix":"ops-","description":"","suffix_regex":""}
	]`, w.Body.String())
}

func TestExecutePrefixesCommand_NamesForcedVisibility(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	cfg := approvableConfig()
	cfg.prefixes = parsePrefixList("sec-|Security|8\nops-", func(string, ...any) {})
	cfg.prefixDefaults = map[string]prefixDefaults{"sec-": {ChannelType: "P"}}
	p.setConfiguration(cfg)

	resp := p.executePrefixesCommand()
	require.Contains(t, resp.Text, "`sec-` Security (suffix rule: `8`) (always private)")
	require.NotContains(t, resp.Text, "`ops-` (always")
}
