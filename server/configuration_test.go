package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestCompileSuffixRule_FailsClosed(t *testing.T) {
	noopLog := func(string, ...any) {}

	// A regex that doesn't compile rejects every suffix instead of lifting
	// the rule.
	prefixes := parsePrefixList("proj-|Projects|[0-9", noopLog)
	_, err := resolvePrefixedName(prefixes, requestInput{Prefix: "proj-", Name: "anything-goes"})
	require.Error(t, err)

	// An unbalanced pattern is rejected too.
	prefixes = parsePrefixList("proj-|Projects|[0-9]{4})|(.*", noopLog)
	_, err = resolvePrefixedName(prefixes, requestInput{Prefix: "proj-", Name: "anything-goes"})
	require.Error(t, err)
}

func TestSetConfiguration_SamePointerPanics(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	cfg := &configuration{ApprovalTeam: "ops"}
	p.setConfiguration(cfg)

	require.Panics(t, func() { p.setConfiguration(cfg) })
}

func TestOnConfigurationChange_LoadFailure(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	before := p.getConfiguration()
	api.On("LoadPluginConfiguration", mock.Anything).Return(testAppErr("bad config"))

	require.ErrorContains(t, p.OnConfigurationChange(), "failed to load plugin configuration")
	// The previous configuration stays in place.
	require.Same(t, before, p.getConfiguration())
}

func TestParsePrefixList_SkipsEmptyPrefix(t *testing.T) {
	var logged []string
	got := parsePrefixList("|No prefix\nteam-|Teams", func(msg string, _ ...any) { logged = append(logged, msg) })

	require.Len(t, got, 1)
	require.Equal(t, "team-", got[0].Prefix)
	require.Equal(t, []string{"channel-requests: skipping prefix entry with empty prefix"}, logged)
}

func TestCompileSuffixRule_ClampsNumericLength(t *testing.T) {
	noopLog := func(string, ...any) {}
	cases := []struct{ in, want string }{
		{"1", "^(?:[a-z0-9-]{2,2})$"},
		{"16", "^(?:[a-z0-9-]{2,16})$"},
		{"99", "^(?:[a-z0-9-]{2,48})$"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			re, raw := compileSuffixRule(tc.in, "team-", noopLog)
			require.Equal(t, tc.want, re.String())
			require.Equal(t, tc.in, raw)
		})
	}
	require.False(t, isPositiveInt(""))
}

func TestParseNonNegativeInt(t *testing.T) {
	cases := []struct {
		raw    string
		want   int
		warned bool
	}{
		{raw: "", want: 7},
		{raw: "  ", want: 7},
		{raw: "12", want: 12},
		{raw: " 3 ", want: 3},
		{raw: "0", want: 0},
		{raw: "-1", want: 7, warned: true},
		{raw: "five", want: 7, warned: true},
		{raw: "2.5", want: 7, warned: true},
		{raw: "+5", want: 7, warned: true},
		{raw: "100", want: 100},
		{raw: "101", want: 100, warned: true},
		{raw: "99999999999999", want: 100, warned: true},
		{raw: "999999999999999999999", want: 7, warned: true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			warned := false
			got := parseNonNegativeInt(tc.raw, 7, 100, "Setting", func(string, ...any) { warned = true })
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.warned, warned)
		})
	}
}

func TestConfigurationAllowsChannelName(t *testing.T) {
	noop := func(string, ...any) {}
	opsRule, opsRaw := compileSuffixRule("[a-z]+", "team-ops-", noop)
	config := &configuration{prefixes: []channelPrefix{
		{Prefix: "team-"},
		{Prefix: "team-ops-", SuffixPattern: opsRule, SuffixPatternRaw: opsRaw},
		{Prefix: "proj-", SuffixPattern: matchNothing},
	}}

	require.True(t, config.allowsChannelName("team-marketing"))
	require.True(t, config.allowsChannelName("team-ops-oncall"))
	// The longest matching prefix decides which rule applies.
	require.False(t, config.allowsChannelName("team-ops-42"))
	require.False(t, config.allowsChannelName("team-"))
	require.False(t, config.allowsChannelName("proj-alpha"))
	require.False(t, config.allowsChannelName("eng-alpha"))
	require.False(t, (&configuration{}).allowsChannelName("team-marketing"))
}

func TestOnConfigurationChange_ParsesRequestLimits(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVGet", kvPrefixMaxLenV48).Return([]byte("1"), nil)

	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		dest := args.Get(0).(*configuration)
		dest.MaxPendingRequestsPerUser = "2"
		dest.RequestExpiryDays = "soon"
	}).Return(nil)

	require.NoError(t, p.OnConfigurationChange())
	require.Equal(t, 2, p.getConfiguration().MaxPending())
	require.Equal(t, defaultRequestExpiryDays, p.getConfiguration().ExpiryDays())
	// A blank token lifetime means the one-year default.
	require.Equal(t, defaultBotTokenLifetime, p.getConfiguration().TokenLifetimeMonths())
	api.AssertCalled(t, "LogWarn", "channel-requests: setting must be a whole number 0 or greater; using the default",
		"setting", "RequestExpiryDays", "value", "soon", "default", defaultRequestExpiryDays)
}

func TestOnConfigurationChange_CapsRequestLimits(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("KVGet", kvPrefixMaxLenV48).Return([]byte("1"), nil)
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		dest := args.Get(0).(*configuration)
		dest.MaxPendingRequestsPerUser = "5000"
		dest.RequestExpiryDays = "99999999999999"
	}).Return(nil)

	require.NoError(t, p.OnConfigurationChange())
	require.Equal(t, maxPendingRequestsLimit, p.getConfiguration().MaxPending())
	require.Equal(t, maxRequestExpiryDays, p.getConfiguration().ExpiryDays())
	require.False(t, requestExpired(model.GetMillis()-day, p.getConfiguration().ExpiryDays(), model.GetMillis()))
}

func TestParseTokenLifetimeMonths(t *testing.T) {
	for raw, tc := range map[string]struct {
		want int
		warn bool
	}{
		"":     {want: defaultBotTokenLifetime},
		"1":    {want: 1},
		"6\n":  {want: 6},
		"12":   {want: 12},
		"0":    {want: defaultBotTokenLifetime, warn: true},
		"13":   {want: defaultBotTokenLifetime, warn: true},
		"-3":   {want: defaultBotTokenLifetime, warn: true},
		"year": {want: defaultBotTokenLifetime, warn: true},
	} {
		t.Run(raw, func(t *testing.T) {
			warned := false
			require.Equal(t, tc.want, parseTokenLifetimeMonths(raw, func(string, ...any) { warned = true }))
			require.Equal(t, tc.warn, warned)
		})
	}
}

func TestTokenLifetimeMonths_UnparsedConfigUsesDefault(t *testing.T) {
	require.Equal(t, defaultBotTokenLifetime, (&configuration{}).TokenLifetimeMonths())
}
