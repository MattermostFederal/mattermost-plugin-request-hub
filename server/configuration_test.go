package main

import (
	"testing"

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
