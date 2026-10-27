package main

import (
	"testing"

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
