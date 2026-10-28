---
name: add-help-docs
description: Use when plugin code changes need to be reflected in the user-facing documentation, the built-in help pages under public/help and the guides under docs/. Trigger after adding or modifying admin settings, the slash command, the request or approval flow, or any user-visible message, or before cutting a release.
user-invocable: true
---

# Add Help Docs

Keep the user-facing documentation in sync with the code.

Mattermost Request Hub ships documentation in two places, both edited by hand:

- **`public/help/`**: static HTML bundled with the plugin. Mattermost serves the
  bundle's `public/` directory at `/plugins/<id>/public/**`, so there is no
  route, no renderer and no generator script. The overview is `docs.html`: the
  request modals, the Request Hub, `/mattermost-request-hub docs` and the System
  Console header all link to it.
- **`docs/`**: Markdown guides read on GitHub, with screenshots in
  `docs/images/`.

## When to Use

- After adding, renaming, or removing an admin setting in `plugin.json`
- After changing the prefix rules, suffix rules or preset packs (`server/configuration.go`, `webapp/src/PrefixEditor.tsx`)
- After changing the `/mattermost-request-hub` command or how the form opens (`server/command.go`, `webapp/src/channelCreationOverride.ts`, `webapp/src/HeaderIcon.tsx`, `webapp/src/membersPanelButton.ts`)
- After changing the request form (`webapp/src/RequestChannelModal.tsx`, the dialog in `server/http.go`)
- After changing who can approve, what approval does, or what gets posted (`server/request.go`, `server/decision*.go`, `server/http.go`)
- After changing the Channel Admin request flow (`server/admin_request.go`, `webapp/src/RequestChannelAdminModal.tsx`)
- After changing any user-visible message text
- Before cutting a release

## When NOT to Use

- Pure refactors with no user-visible change
- Test-only or build-only changes
- Changes already documented in a previous commit on the same branch

## Workflow

Run these steps in order. **Always produce a plan and get explicit user
confirmation before editing any documentation file.**

Before starting, create tasks using TaskCreate for each applicable step. Mark
each complete with TaskUpdate as you finish it.

### 1. Survey what changed

```bash
git diff main...HEAD -- server/ plugin.json webapp/src/ public/ docs/
git log main..HEAD --oneline
```

Focus on: new or renamed settings; changes to prefixes and naming; changes to
the request form, the approval post or the welcome message; changes to who can
request or approve; and any changed string a user can see.

### 2. Produce a plan

Write a short plan and present it. It must list which pages will be edited and
the specific user-visible change driving each edit, plus anything intentionally
left alone and why. Wait for explicit confirmation before continuing.

### 3. Update the pages

Edit only the pages whose scope actually changed. Match the existing tone and
heading structure.

#### Layout

| File | Covers | Kept in sync with |
|---|---|---|
| `public/help/docs.html` | Overview: what can be requested, how to open, how a request flows | `server/command.go`, `webapp/src/RequestHubModal.tsx` |
| `public/help/open.html` | Every way to open each request form | `webapp/src/index.tsx` and the override files it registers |
| `public/help/messaging.html` | The two-way ticket thread relay | `server/messaging.go`, `server/ticket.go` |
| `public/help/channel.html`, `channel-admin.html`, `team.html`, `team-member.html`, `team-guest.html`, `bot.html`, `webhook.html` | One page per request type: how to open, the form, limits, what approve does | That type's `server/*_request.go` and `webapp/src/Request*Modal.tsx` |
| `public/help/approving.html` | Who can approve, the approve re-checks, expiry, approving from the slash command, two-step approval | `server/decision.go`, `server/decision_team.go`, `server/decision_two_step.go` (one `decide*Request` per type), `server/http.go`, `server/approvers.go`, `server/approval_engine.go` |
| `public/help/slash.html` | Every `/mattermost-request-hub` subcommand (a section with `id="<subcommand>"`), the `channel` options and the replies `channel`, `channel-admin`, `approve`, `deny` and `exercise` give when they refuse, and the exercise scenario format | `server/command.go`, `server/command_exercise.go`, `server/exercise.go`, `server/exercise/silver_harbor.json`, `server/request.go`, `server/admin_request.go`, `server/pending.go`, `server/decision.go`, `server/status.go` |
| `public/help/agents.html` | The Agents tools | `server/mcp.go`, `server/mcp_tools.go`, `server/status.go` |
| `public/help/troubleshooting.html` | One section per symptom: the message, its cause and the fix | Every user-visible error and log warning it quotes |
| `public/help/styles.css` | Shared stylesheet. Rarely changes | |
| `docs/admin-guide.md` | Every setting, the auto-created channels, the approver rules | `plugin.json`, `server/configuration.go`, `server/team_channels.go` |
| `docs/*_GUIDE.md` | One guide per request type, for requesters, approvers and admins | The matching help page |

The `settings_schema.header` and each setting's `help_text` in `plugin.json`
are documentation too: the System Console shows them. Keep them consistent with
`docs/admin-guide.md`.

#### Change-to-file matrix

| What changed | Pages to update |
|---|---|
| New or renamed setting in `plugin.json` | `docs/admin-guide.md`, the admin setup section of the matching guide, and any help page that names the setting |
| Prefix, suffix rule or preset pack behavior | `channel.html`, and the domain prefixes section of `docs/CHANNEL_CREATION_GUIDE.md` |
| New way to open a form, or a change to a form | `open.html`, that type's page, and the requesting section of its guide |
| Who can approve, or what approval or denial does | `approving.html`, that type's page, and the approving section of its guide |
| New or changed `/mattermost-request-hub` subcommand or option | `slash.html` (summary row linking to a section with `id="<subcommand>"`), and the short list in `docs/CHANNEL_CREATION_GUIDE.md` if it is a channel subcommand |
| New or changed Agents tool | `agents.html` and the tool list in `troubleshooting.html#agents-missing` |
| New error or warning a user or admin could hit | `troubleshooting.html`, if the fix isn't obvious from the message |
| Changed user-facing message text | Every page that quotes it, verbatim |
| A UI change visible in a screenshot | Note the stale image in `docs/images/` in the report; do not fake a screenshot |

#### Rules

1. **No remote assets and no scripts in `public/help/`.** No CDN, no web fonts,
   no external images, no inline event handlers. These pages must render on an
   air-gapped host and work with scripting off.
2. **Preserve anchor ids and headings other pages link to.** A rename fails
   silently: the browser lands at the top. If you must rename one, update every
   `href` to it, including the links between the two `docs/` guides.
3. **Every help page's sidebar lists every page**, with `class="active"` on its
   own entry and nowhere else.
4. **Quote messages verbatim.** Copy user-facing strings from the Go or TSX
   source rather than paraphrasing, so search finds them.
5. **No em dashes** in `public/help/`, `docs/`, `README.md` or `plugin.json`.
   Use a comma, a colon, a period or parentheses.
6. **Hardcode no plugin id in the webapp.** It builds help links from `manifest.id`.
   The `plugin.json` header link and the Agents tool names on the help pages are
   the exceptions; `server/help_docs_test.go` checks both against the manifest
   and the running MCP server.

#### Adding a help page

1. Copy the shell from an existing page: `<head>`, the sidebar nav, and the
   breadcrumb. Move `class="active"` onto the new entry.
2. Add the new page to the nav block of **all** the other pages. The block is
   identical everywhere except for which entry carries `class="active"`.
3. Add it to `helpPages` in `server/help_docs_test.go`.
4. Link to it from `docs.html`.

### 4. Verify

```sh
make check-style
make test
make dist
tar tzf dist/com.mattermostfederal.mattermost-plugin-request-hub-*.tar.gz | grep public/help
grep -o 'href="[^"#]*' public/help/*.html | sort -u
```

`server/help_docs_test.go` (part of `make test`) checks that the page list
matches `public/help/`, that every sidebar is complete with one active entry,
that every link and `#anchor` resolves, that every subcommand and Agents tool is
documented, that the System Console header links to the docs, that no page
loads a script or remote asset, and that the docs and `plugin.json` contain no
em dashes. It doesn't check the prose, so still open
`public/help/docs.html` over `file://` and read what you changed.

### 5. Report

Summarize which pages changed and why, and anything intentionally left alone
(including stale screenshots).

## Common Mistakes

- Skipping the plan step. Always write the plan and get confirmation first.
- Updating `public/help/` but not the matching section of the `docs/` guide, or
  the reverse.
- Changing a setting's behavior without updating its `help_text` in
  `plugin.json`.
- Renaming an anchor or heading another page links to.
- Adding a page but forgetting the other navigation blocks.
- Paraphrasing a user-facing message instead of quoting it.
