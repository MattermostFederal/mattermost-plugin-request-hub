# End-to-end tests

Playwright tests that install the plugin bundle from `make dist` on a real Mattermost server (Enterprise image plus Postgres, started with testcontainers) and drive it through the REST API, the slash command and the browser.

## Running

```sh
make e2e                      # from the repo root: builds dist/, installs deps, runs everything
cd e2e && npx playwright test # reuse an existing dist/ bundle
npx playwright test tests/harness.spec.ts --headed
MM_IMAGE=mattermost/mattermost-enterprise-edition:11.3 npx playwright test
```

- Needs Docker and the Node version in `.nvmrc`. On Apple Silicon the amd64 image runs under Rosetta (`MATTERMOST_PLATFORM` overrides the platform).
- Server logs go to `logs/<name>-w<worker>.log` (one JSON object per line). Traces and screenshots of failed tests go to `test-results/`, and the HTML report to `playwright-report/`.
- CI (`.github/workflows/e2e.yml`) builds the bundle once, then runs the suite in 2 shards against Mattermost 11.3 (`min_server_version`) and the newest release, both pinned by digest. The `e2e` job is the one check that sums them up. A nightly run repeats every test 3 times to find flaky tests. Retries stay at 0: a flaky test gets fixed, or skipped with a link to an issue.
- A test for a known product bug asserts the correct behaviour and is marked `test.fail(true, 'BUG: ...')`. It passes while the bug is there and fails once the bug is fixed, which is the reminder to remove the marker.

## How the suite is organised

`helpers/fixtures.ts` exports `test` and `expect`. Spec files import them from there, not from `@playwright/test`.

- **One server per worker.** The `hub` fixture is worker-scoped: a worker starts one server and every spec file it runs shares it. Locally there are 2 workers; in CI each shard runs 1.
- **A file that needs its own server** (activation, migration, anything that changes global state for good) adds `test.use({hubOptions: {name: 'bootstrap', pluginConfig: {...}}})`. Playwright runs that file in a worker of its own.
- **No plugin errors.** After every test, the auto fixture fails the test if the plugin logged an error-level line during it. A test that expects one lists it: `test.use({allowedPluginErrors: [/approval channel is archived/]})`.

Because tests share a server, they must not depend on each other or on leftovers:

- Use `uid()` for every name you create (channels, teams, bots, display names).
- Submit requests as a new user from `hub.createUser()`, not as `alice`. Every user has a limited number of pending requests (`MaxPendingRequestsPerUser`), and other tests leave requests pending.
- Change plugin settings only with `hub.withPluginConfig(patch, fn)`, which puts the old config back afterwards. Config changes apply before `patchConfig` returns.

## Helpers

`RequestHub` (`helpers/requesthub.ts`):

| Helper | What it does |
|---|---|
| `client(username)` | Logged-in `Client4` (cached). `sysadmin`, `alice`, `bob`, `carol` and `tadmin` (Team Admin of `test`) always exist; the password is `PASSWORD` |
| `createUser({systemAdmin, teamAdmin, inTeam, prefix})` | New user with a unique name, in team `test` unless `inTeam: false` |
| `pluginFetch(username, route, body?)` | Calls a plugin route (`GET` without a body, `POST` with one); throws on a non-2xx status |
| `slash(username, args, channel?)` | Runs `/mattermost-request-hub <args>` and returns the ephemeral reply |
| `findCard(needle)`, `findCardIn(channel, needle)` | Newest post mentioning `needle` in a request channel (`REQUEST_CHANNELS.channel`, `.bot`, `.webhook`, `.team`, `.audit`) |
| `findDM(username, needle)` | Newest post mentioning `needle` in the user's DM with the Request Hub bot |
| `findAudit(needle)` | Newest audit-channel post mentioning `needle` |
| `kv(key)`, `kvKeys(prefix)` | Plugin KV records, read from Postgres |
| `rewindRequest(kind, requesterId, requestId, days)` | Moves a request's `created_at` back in its record and the requester's pending list, to test expiry and pruning |
| `cardRequestId(card)` | The request ID in an approval card's footer |
| `withPluginConfig(patch, fn)` | Runs `fn` with plugin settings changed (keys are lowercase) |
| `serverVersion`, `atLeast(version, min)` | For features newer than `min_server_version` |
| `mm.mmctl(...)`, `mm.psql(sql)` | Run `mmctl --local` in the server container, or SQL against its database |

`helpers/ui.ts` has browser helpers: `login(page, hub, username)` and `openChannel(page, hub, channel)`.

Each request area has its own helper file next to its spec: `channel.ts`, `bots.ts`, `webhooks.ts`, `teamrequests.ts`, `limits.ts` and `slash.ts`. They hold that area's submit wrappers, exact message texts and "nothing was stored" checks.

## Server versions and licensing

- The Enterprise image without a license file runs as **Mattermost Entry**, which includes guest accounts, LDAP groups and custom profile attributes. No license secret is needed.
- Custom user groups can't be created (Entry), and the plugin refuses them as approver groups anyway. `usergroup` mode tests need a plugin-synced group, inserted with `mm.psql`.
- Bot token expiry needs Mattermost 11.9 or later (MM-68419). On older servers the token has no expiry and the token DM doesn't give a date.

## Coverage

Feature checklist for the suite. A feature is ticked once a spec covers it end to end, including the requests that must be refused at submit and at approval; anything left to unit tests says why.

| Area | Spec | Status |
|---|---|---|
| Plugin activates; request channels get one docs bookmark each | `channel-request`, `request-channels` | ✅ |
| Channel request: dialog (browser and API), REST, approve, deny, Team Admin approves | `channel-request` | ✅ |
| Bot token lifetime and token DM | `request-channels` | ✅ |
| Bootstrap: channels, per-team approval channels, idempotent re-enable | `bootstrap` | ⬜ |
| Channel request: inline slash, modal, prefixes, name clashes, concurrent approvals, re-checks | `channel-request` |✅ |
| Channel Admin requests | `channel-admin` |✅ |
| Team, Team Admin, team member and team guest requests | `team-requests` |✅ |
| Bot requests: two-step approval in each approver mode | `bot-requests` |✅ |
| Incoming and outgoing webhook requests | `webhook-requests` |✅ |
| Pending limit, duplicates, double submit, expiry, pruning | `limits-expiry` |✅ |
| Slash utilities: help, docs, prefixes, list, approve and deny by ID, parsing | `slash-command` |✅ |
| Settings toggles and the admin console | `settings` | ⬜ |
| Webapp modals and menus | `webapp-modals` | ⬜ |
| Exercise mode | `exercise` | ⬜ |
| Security: auth on every route, 403s, body cap, markdown injection | `security` | ⬜ |
| Help pages and links | `help-docs` | ⬜ |
| MCP tools | `mcp` | ⬜ (needs a test-only caller plugin) |
| Upgrade from the last release | `upgrade` | ⬜ |
| Bot token reminders (hourly job) | n/a | Unit tests: the job period is an hour and there is no setting to shorten it |
