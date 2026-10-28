# Mattermost Request Hub

Governance plugin for Mattermost: non-admins **request** channels, Channel Admin, teams, team membership, guest invites, bots, and webhooks. Approvers act from plugin-managed channels with two-way messaging threads.

**Plugin ID:** `com.mattermostfederal.mattermost-plugin-request-hub`  
**Slash command:** `/mattermost-request-hub`  
**Sidebar category:** Mattermost: Request Hub

> **Breaking (0.5.0):** plugin ID changed from `com.mattermostfederal.channel-requests`. Settings and pending requests are not carried over from the old plugin; see [Upgrading](docs/admin-guide.md#upgrading) before you switch.

## Features

- Request Hub (App Bar) plus More actions, slash commands, and native menu overrides
- Slash command arguments for every request type (for example `channel <prefix> <Channel name> [options]` or `channel-admin @user ...`) that skip the form, plus `prefixes` to list the channel prefixes
- Per-type enable toggles in System Console
- Approval routing: channel requests (root or per-team), team-scope tickets in `mattermost-team-requests`, bots/webhooks in dedicated channels
- Soft-squash approval cards and DM ticket threads with message relay
- Bot/webhook two-step approval via User Attributes, User Groups, or System Admins only
- Optional tools for the Mattermost Agents plugin (**Offer tools to Agents**): an agent can list prefixes and file any type of request for the user it's talking to, under the same rules and per-type toggles. Agents can't approve or deny.

## Documentation

- [Channel Requests](docs/CHANNEL_CREATION_GUIDE.md) — requesting and approving **new channels**
- [Channel Admin](docs/CHANNEL_ADMIN_GUIDE.md) — requesting **admin rights on an existing channel**
- [Team Requests](docs/TEAM_REQUEST_GUIDE.md) — create team / Team Admin
- [Team Membership & Guests](docs/TEAM_MEMBERSHIP_GUIDE.md) — add members / guest invite email
- [Bot Accounts](docs/BOT_REQUEST_GUIDE.md)
- [Webhooks](docs/WEBHOOK_REQUEST_GUIDE.md)
- [Admin Guide](docs/admin-guide.md)
- [Releasing](docs/RELEASING.md) — how to cut a release

In Mattermost, **`/mattermost-request-hub docs`** replies with a link to the bundled HTML guide.

Requires Mattermost 11.3 or later.

## Build and deploy

```sh
make dist    # builds dist/com.mattermostfederal.mattermost-plugin-request-hub-*.tar.gz
make deploy  # optional: deploy to a running Mattermost server
```

## Common commands

- `make dist` — build the plugin bundle
- `make check-style` — lint Go and webapp code
- `make test` — run tests
- `make deploy` — deploy to the Mattermost server specified by `MM_SERVICESETTINGS_SITEURL` and `MM_ADMIN_TOKEN`

## Configuration

**System Console → Plugins → Mattermost Request Hub**

- Set **Approval team**
- Enable the request types you need (guest invites default off; team member / team / bot / webhook default on)
- Configure channel domain prefixes for channel creation
- For guests: enable Guest Accounts on the server before relying on emailed invites

> To fully funnel non-admins through the request flow, remove the built-in **Create Public/Private Channel** permission from the System User role (System Console → User Management → Permissions). Mattermost's plugin API cannot veto native channel creation.

## PR preview environments

Add the `preview` label to a pull request to stand up a throwaway Mattermost server with that PR's bundle installed. The URL is posted on the PR. Removing the label or closing the PR tears it down.

`.github/workflows/preview.yml` is a thin copy of the shared workflow in the private `MattermostFederal/pr-preview` repo, checked out at its protected `v1` tag. It never checks out or runs PR code: it waits for `build.yml` to pass on the PR's head commit and deploys the `plugin-bundle` artifact that run uploaded. A fork PR deploys only the commit that was labeled; a later push tears the preview down until a maintainer re-applies the label.

What the preview does beyond installing the bundle lives in `.preview/`, which pr-preview reads from the PR's base branch, so a change to it reaches previews only after it merges. `deploy.sh` runs after the bundle is installed: it sets the plugin's approval team to `test` (it also creates a `~channel-requests` channel there, which the plugin no longer uses; the plugin creates its own approval channels) and removes the built-in create-channel permissions from non-admins so they go through the request flow. Keep it in step with `make docker-setup` and `make deploy`. A `mattermost.env` file there would add `MM_*` settings to the server; none are needed today.

One-time setup:

- create the label: `gh label create preview`
- make sure the org variables `PREVIEW_CHECKOUT_APP_ID` and `PREVIEW_AWS_ROLE_ARN` and the org secrets `PREVIEW_CHECKOUT_APP_PRIVATE_KEY` and `PREVIEW_ADMIN_SECRET` are visible to this repo
- optionally set `PREVIEW_APP` (hostname prefix, default is the repo name without `mattermost-plugin-`) or `PREVIEW_MM_IMAGE_TAG` in the workflow's `env:` block

## Vulnerability scanning

This plugin ships with a CycloneDX SBOM and grype-based vulnerability audit:

- `make sbom` — generate CycloneDX SBOMs for the Go server and npm webapp into `dist/sbom/`
- `make sbom-scan` — run grype against the SBOMs and fail on high or critical CVEs
- `make sbom-audit` — `sbom` then `sbom-scan` in one step

Suppress known-false-positive findings by adding entries to `.grype.yaml` with a short reason. Typical reasons include: dev-only transitive dependency (not in the shipped bundle) or runtime dependency that Mattermost externalizes (e.g. react, redux).

See the [Mattermost plugin developer docs](https://developers.mattermost.com/extend/plugins/) for more information.

## License

See [LICENSE](LICENSE).
