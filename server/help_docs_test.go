package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/require"
)

// The built-in documentation is static HTML under public/help, served from
// the bundle at /plugins/<id>/public/help/. Nothing at build time checks it
// against the code, so these tests do.
const helpDir = "../public/help"

// helpPages is every page in the bundle. The sidebar on each one links to
// all of them, so adding a page means adding it here and to every sidebar.
var helpPages = []string{
	"docs.html",
	"open.html",
	"messaging.html",
	"channel.html",
	"channel-admin.html",
	"team.html",
	"team-member.html",
	"team-guest.html",
	"bot.html",
	"webhook.html",
	"approving.html",
	"slash.html",
	"agents.html",
	"troubleshooting.html",
}

func readHelpFile(t *testing.T, name string) string {
	t.Helper()
	// #nosec G304 -- fixed, repo-relative path
	data, err := os.ReadFile(filepath.Join(helpDir, name))
	require.NoError(t, err, name)
	return string(data)
}

// A page on disk that isn't listed is covered by no test here, and one
// listed but missing would leave dead sidebar links.
func TestHelpBundleMatchesTheListedPages(t *testing.T) {
	entries, err := os.ReadDir(helpDir)
	require.NoError(t, err)

	var onDisk []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".html") {
			onDisk = append(onDisk, entry.Name())
		}
	}

	listed := append([]string(nil), helpPages...)
	sort.Strings(listed)
	sort.Strings(onDisk)
	require.Equal(t, listed, onDisk)
}

// The sidebar is the only navigation, so every page must link to every
// page and mark exactly its own link as current.
func TestHelpNavigationIsComplete(t *testing.T) {
	active := regexp.MustCompile(`href="([a-z0-9-]+\.html)" class="active"`)
	for _, name := range helpPages {
		t.Run(name, func(t *testing.T) {
			body := readHelpFile(t, name)
			for _, target := range helpPages {
				require.Contains(t, body, `href="`+target+`"`, "%s does not link to %s", name, target)
			}

			marked := active.FindAllStringSubmatch(body, -1)
			require.Len(t, marked, 1, "%s must mark exactly one sidebar link active", name)
			require.Equal(t, name, marked[0][1])
		})
	}
}

// A renamed id breaks a link silently: the browser lands at the top of the
// page instead of the section.
func TestEveryHelpAnchorResolves(t *testing.T) {
	pages := map[string]string{}
	ids := map[string]map[string]bool{}
	idPattern := regexp.MustCompile(`\sid="([^"]+)"`)
	for _, name := range helpPages {
		body := readHelpFile(t, name)
		pages[name] = body
		ids[name] = map[string]bool{}
		for _, m := range idPattern.FindAllStringSubmatch(body, -1) {
			ids[name][m[1]] = true
		}
	}

	// Every href must be styles.css, a page, a page#id or #id. Anything
	// else fails rather than going unchecked.
	href := regexp.MustCompile(`(?i)href\s*=\s*["']?([^"'\s>]*)`)
	link := regexp.MustCompile(`^([a-z0-9-]+\.html)?(?:#(.+))?$`)
	checked := 0
	for _, from := range helpPages {
		for _, h := range href.FindAllStringSubmatch(pages[from], -1) {
			if h[1] == "styles.css" {
				continue
			}
			m := link.FindStringSubmatch(h[1])
			if m == nil || h[1] == "" {
				t.Errorf("%s has href %q; use page.html, page.html#id or #id", from, h[1])
				continue
			}
			target, anchor := m[1], m[2]
			if target == "" {
				target = from
			}
			if _, ok := pages[target]; !ok {
				t.Errorf("%s links to %s, which is not a page in the bundle", from, target)
				continue
			}
			if anchor == "" {
				continue
			}
			checked++
			if !ids[target][anchor] {
				t.Errorf("%s links to %s#%s, which has no matching id", from, target, anchor)
			}
		}
	}
	require.NotZero(t, checked, "no anchors were checked; the link pattern no longer matches the pages")
}

// Subcommands come from the autocomplete data, the same list
// TestGetAutocompleteData_ListsEverySubcommand checks against the usage text.
func TestSlashPageDocumentsEverySubcommand(t *testing.T) {
	page := readHelpFile(t, "slash.html")
	for _, sub := range getAutocompleteData().SubCommands {
		require.Contains(t, page, "/"+commandTrigger+" "+sub.Trigger, "slash.html does not document %s", sub.Trigger)
		require.Contains(t, page, ` id="`+sub.Trigger+`"`, "slash.html has no section for %s", sub.Trigger)
	}
}

// The summary table at the top of slash.html links each subcommand to its
// section, and TestEveryHelpAnchorResolves checks the section exists. The
// overview links to slash.html.
func TestSlashSummaryLinksEverySubcommand(t *testing.T) {
	page := readHelpFile(t, "slash.html")
	for _, sub := range getAutocompleteData().SubCommands {
		require.Contains(t, page, `href="#`+sub.Trigger+`"`, "slash.html's summary does not link to %s", sub.Trigger)
	}
	require.Contains(t, readHelpFile(t, "docs.html"), `href="slash.html"`)
}

// The tools come from the running MCP server, so a tool added to
// registerMCPTools fails here until it's documented.
func TestAgentsPageDocumentsEveryAgentsTool(t *testing.T) {
	stubMCP(t)
	p := newMCPTestPlugin(t, &plugintest.API{}, &configuration{})
	listed, err := mcpSession(t, p, "u1").ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, listed.Tools)

	agents := readHelpFile(t, "agents.html")
	troubleshooting := readHelpFile(t, "troubleshooting.html")
	for _, tool := range listed.Tools {
		require.True(t, strings.HasPrefix(tool.Name, mcpToolPrefix), tool.Name)
		short := strings.TrimPrefix(tool.Name, mcpToolPrefix)
		require.Contains(t, agents, "<code>"+short+"</code>", "agents.html does not document %s", short)
		require.Contains(t, troubleshooting, tool.Name, "troubleshooting.html does not list %s", tool.Name)
	}
}

// The System Console header is the one place every admin sees a link to
// the docs. It opens the overview, as the docs subcommand does.
func TestSettingsHeaderLinksToTheDocs(t *testing.T) {
	want := fmt.Sprintf("/plugins/%s/public/help/docs.html", manifest.Id)
	require.NotNil(t, manifest.SettingsSchema)
	require.Contains(t, manifest.SettingsSchema.Header, want)
}

// The pages are served from the bundle, often on servers with no route
// out, so they load nothing from elsewhere and run no script.
func TestHelpPagesAreSelfContained(t *testing.T) {
	banned := []struct {
		pattern *regexp.Regexp
		why     string
	}{
		{regexp.MustCompile(`(?i)<script`), "no scripts"},
		{regexp.MustCompile(`(?i)\son[a-z]+\s*=`), "no inline event handlers"},
		{regexp.MustCompile(`(?i)(src|href)\s*=\s*"(https?:)?//`), "no remote assets or absolute links"},
		{regexp.MustCompile(`(?i)@import`), "no imported stylesheets"},
	}
	for _, name := range append(append([]string(nil), helpPages...), "styles.css") {
		body := readHelpFile(t, name)
		for _, b := range banned {
			if m := b.pattern.FindString(body); m != "" {
				t.Errorf("%s contains %q: %s", name, m, b.why)
			}
		}
	}
}

// Em dashes are banned in documentation and in the System Console text,
// which plugin.json carries.
func TestDocsUseNoEmDash(t *testing.T) {
	files := []string{"../plugin.json", "../README.md"}
	for _, name := range append(append([]string(nil), helpPages...), "styles.css") {
		files = append(files, filepath.Join(helpDir, name))
	}
	guides, err := filepath.Glob("../docs/*.md")
	require.NoError(t, err)
	require.NotEmpty(t, guides)
	files = append(files, guides...)

	for _, file := range files {
		// #nosec G304 -- fixed, repo-relative paths
		data, err := os.ReadFile(file)
		require.NoError(t, err, file)
		for _, form := range []string{"—", "&mdash;", "&#8212;", "&#x2014;"} {
			if strings.Contains(string(data), form) {
				t.Errorf("%s contains an em dash (%s); use a comma, a colon, a period or parentheses", file, form)
			}
		}
	}
}
