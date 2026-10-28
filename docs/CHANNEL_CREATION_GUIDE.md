# Channel Requests: User Guide

Mattermost Request Hub lets members **request** a new channel instead of
creating one directly. The request goes to an approver; on approval the channel
is created, the requested members and Channel Admins are added, optionally placed
in a sidebar category, and everyone added gets an @-mention welcome message.

> Companion guides: [Channel Admin](CHANNEL_ADMIN_GUIDE.md) ·
> [teams](TEAM_REQUEST_GUIDE.md) · [bots](BOT_REQUEST_GUIDE.md) ·
> [webhooks](WEBHOOK_REQUEST_GUIDE.md) · [Admin Guide](admin-guide.md).
>
> In Mattermost, open the full in-app guide anytime with
> **`/mattermost-request-hub docs`**. It is served by the plugin at
> `/plugins/com.mattermostfederal.mattermost-plugin-request-hub/public/help/docs.html`,
> with a full slash command reference (`slash.html`) and a troubleshooting page
> (`troubleshooting.html`).

This guide covers:

- [Requesting a channel](#requesting-a-channel): any member
- [Requesting from the command line](#requesting-from-the-command-line): skip the form
- [Requesting through an AI agent](#requesting-through-an-ai-agent): the Agents plugin
- [Messaging while a request is pending](#messaging-while-a-request-is-pending): two-way threads
- [Approving requests](#approving-requests): approvers
- [Admin setup](#admin-setup): system admins

---

## Requesting a channel

Same form from every entry point.

### How to open

- **+ menu:** channel sidebar **+** → **Request new channel** (native “Create new channel” is relabeled when members can’t create channels directly)
- **Hub:** right App Bar **plug** → Requests → **Request a Channel** (if several plugins share the strip, pick **Requests** first; with App Bar off, the same control is a channel-header plugin button)
- **Dropdown:** channel name → More actions → **Request a Channel**
- **Slash:** `/mattermost-request-hub channel` (or skip the form, see
  [Requesting from the command line](#requesting-from-the-command-line))

<p>
<img src="images/channel-request/mattermost-creation-plus-menu.png" alt="+ menu" width="240">
<img src="images/shared/mattermost-requests-plug-appbar.png" alt="Right App Bar (plug)" width="56">
<img src="images/shared/mattermost-request-hub.png" alt="Request Hub" width="440">
</p>

> Tip: **`/mattermost-request-hub docs`** opens this documentation in a simple
> browser page (all request types in one place).

### Form

<img src="images/channel-request/mattermost-creation-modal.png" alt="Request a Channel form" width="480">

| Field | What it does |
|-------|--------------|
| **Domain prefix** | Required. Pick the category your admin configured; the final URL is `<prefix><suffix>`. |
| **Channel name** | The display name people see (e.g. *SRE Team*). Required. |
| **URL suffix / URL name** | Optional. Leave blank to auto-generate from the channel name. A live **Preview** shows the final URL (e.g. `ops-sre-team`). |
| **Purpose** | Optional short description of the channel. |
| **Visibility** | **Public** (anyone can join) or **Private** (invite only). |
| **Members to add** | Search and add users who become regular members on approval. |
| **Channel Admins to add** | Search and add users who become Channel Admins on approval. |
| **Sidebar category** | Optional. Pick an existing category or type a new name. On approval the channel is placed there for you and listed members; a missing category is created automatically. |

**Members vs Channel Admins:** a user can be in only one of the two lists. If you
add someone to Channel Admins who's already a Member (or vice versa), they
**move** to the new list automatically. A badge in the dropdown shows where a
user is currently assigned.

Click **Submit request**. You'll see a confirmation, and you'll get a direct
message when an approver responds.

**Limits.** A System Admin sets how many requests you can have waiting for
review at once (each request type is counted separately). At the
limit you'll see *you have N pending requests waiting for review; you can submit
more once some are approved, denied or expire*. You can't have the same request
pending twice: asking again for a name you're already waiting on shows *you
already have a pending request for ~name in this team*. Requests that aren't
reviewed within the configured number of days expire, and you'll get a direct
message saying so.

Requests need at least one domain prefix. Until a System Admin defines one, the
form and the slash command show: *Channel requests aren't configured yet. Ask a
System Admin to define at least one channel prefix in System Console → Plugins →
Mattermost Request Hub.*

### Requesting from the command line

`/mattermost-request-hub channel` (or `/mattermost-request-hub` on its own) opens
the request form. The subcommands do the same things from the message box, and
replies are only visible to you:

- `channel <prefix> <Channel name> [options]` requests a channel in the current
  team without the form, for example
  `/mattermost-request-hub channel team- Marketing Team --members @alice @bob`.
- `channel-admin @user [@user ...]` asks for channel members to be made Channel
  Admins. See the [Channel Admin guide](CHANNEL_ADMIN_GUIDE.md#from-the-slash-command).
- `prefixes` lists the channel prefixes you can use.
- `list` lists your pending requests of every type, with their IDs and expiry
  dates. `list approvals` lists the requests you can approve.
- `approve <request ID>` and `deny <request ID>` let approvers act on a request
  of any type without clicking its card. The ID is at the bottom of the
  approval card.
- `exercise populate|teardown|status` lets System Admins seed, remove or report
  marked exercise data for training and demonstrations, when exercise mode is on.
- `help` lists every subcommand and its arguments.

Put quotes around text that should stay as typed, such as a purpose with a word
that starts with `--`: `--purpose "--draft-- plans"`. Users can be separated by
spaces or commas. Every other request type can be filed from the command line
too.

The full reference, with every option and what each error means, is the
plugin's built-in `slash.html` page.

### Requesting through an AI agent

When a System Admin turns on **Offer tools to Agents**, an agent from the
Mattermost Agents plugin can list prefixes, file requests of every type for
you, and tell you what requests you have pending or can approve,
under the same rules as the form. As with the webapp form, the
request goes to the approvers, even when you are a System Admin. No agent can approve or deny a request. The tools are
listed on the built-in `agents.html` page.

---

## Messaging while a request is pending

Every request opens a **linked pair of threads**: one on the approval card and
one in a DM from the bot. Replies are relayed both ways.

Use the **Thread** right-hand sidebar (open **Reply** on the card), not a new
top-level channel or DM message.

- **Approver:** ask in the approval Thread (e.g. *Why do you need this channel?*)
- **Requester:** see `Message from @admin:` in your DM Thread with the bot
- **Reply:** answer in that DM Thread; the approval Thread shows `Reply from @you:`

<p>
<img src="images/shared/mattermost-thread-approver-ask.png" alt="Approval Thread" width="300">
<img src="images/shared/mattermost-thread-requester-dm.png" alt="Requester DM" width="720">
<img src="images/shared/mattermost-thread-relay-back.png" alt="Relay back" width="300">
</p>

Threads stay usable after approve/deny for follow-ups. Messaging the bot outside
a ticket thread gets a nudge to reply inside the specific request instead.

---

## Approving requests

Approvers (System Admins, the Team Admins of the team the channel is requested
in, plus Team Admins of the approval team if your admin enabled it) receive each
request in the **approval channel** with **Approve** and **Deny** buttons. Only a System Admin can approve a request they made
themselves. The card names the team the channel will be created in. A request
an AI agent filed for someone arrives the same way and names that person as the
requester; agents can't approve or deny.

<img src="images/channel-request/mattermost-creation-approval-card.png" alt="Channel creation approval card" width="720">

- **Approve**: creates the channel, adds the requested members, promotes the
  Channel Admins, places the channel in the requested sidebar category (if any),
  and posts a welcome message that @-mentions everyone added.
- **Deny**: the request is closed and the requester is notified by DM.

Approve checks the request again first. If the team has been archived, the
requester's account is no longer active, they've left the team, or the channel
name no longer matches the configured prefixes, you'll get a private message saying so (for example *The
requester is no longer a member of the team. Deny this request instead.*). The
request stays pending and the card keeps its buttons, so deny it. If the
requester can't be checked at that moment, you'll see *Could not check the
requester right now. Try again in a moment.* instead.

A request that wasn't reviewed within **Request expiry (days)** can't be
approved: clicking either button replies *This request has expired.*, marks the
card *⌛ Expired: not reviewed within N days.* and closes the requester's ticket
as expired.

Approvers can also act without the card: the card ends with *Request ID:* and
the ID, and `/mattermost-request-hub approve <request ID>` or
`deny <request ID>` makes the same checks as the buttons. The card and the
requester's ticket are then closed as after a click, and when the request went
to System Admins by direct message, every copy is closed.

<img src="images/channel-request/mattermost-creation-welcome.png" alt="Welcome message in the new channel" width="720">

Requests from **System Admins** are queued for approval too, including ones an
AI agent files for them. A System Admin can approve their own request.

---

## Admin setup

Configure the plugin in **System Console → Plugins → Mattermost Request Hub**.

<img src="images/admin/mattermost-admin-settings.png" alt="System Console plugin settings" width="280">

| Setting | Purpose |
|---------|---------|
| **Approval team** | The team that owns plugin-managed approval channels. Recommended: without it, a channel request that has no per-team approval channel is sent by direct message to every System Admin instead. |
| **Allow channel requests** | Enables channel-creation entry points. |
| **Channel domain prefixes** | **Required.** The prefix list requesters pick from (see below). An empty list rejects every channel request. |
| **Team Admins can approve requests** | When on, Team Admins of the approval team can approve/deny channel-creation requests, not just System Admins. |
| **Per-team approval channels** | Default on. Creates `mattermost-channel-requests` in teams with a Team Admin or System Admin; those teams' channel requests go there (not the primary approval team). Ticket channels (including team-creation) live under sidebar category **Mattermost: Request Hub**. |
| **Offer tools to Agents** | Off by default. When on, the Mattermost Agents plugin can list prefixes, file requests of every type on a user's behalf and look up pending ones, under the same rules and per-type toggles as the forms. Every request goes to the approvers, including System Admins' requests. Agents can never approve or deny. Takes effect on save; choose which tools agents may use in the Agents plugin's own settings. Needs Mattermost 11.3 or later. |
| **Pending requests per user** | How many requests of each type one user can have waiting for review (each type is counted separately), up to `1000`. Default `5`; `0` means no limit. |
| **Request expiry (days)** | Pending requests of any type older than this can no longer be approved, and stop counting toward the per-user limit. At most `3650`. Default `30`; `0` means requests never expire. A change applies to requests already pending too. |
| **Enable exercise mode** | Off by default. **Do not enable in production.** Lets System Admins run `/mattermost-request-hub exercise populate`, `teardown` and `status` to seed and remove marked exercise data in a team: persona accounts nobody can sign in as, channels, and pending requests of every type on real approval cards. Populate needs **Allow channel requests**, at least one prefix, and the setting for each other type the scenario seeds (the built-in scenario seeds all eight). Approving a seeded request does the real thing, marked EXERCISE: a channel, team, bot or webhook, a Team Admin or team member change for a persona, or a guest invite. Teardown deletes the webhooks and bots, archives the teams and channels, and undoes the personas' team changes. Personas and guest invites use `exercise.invalid` email addresses, so a server or team that restricts email domains, or a guest domain list, must allow that domain, and guest access must be on for a scenario with guest invites. A guest invite can't be withdrawn; it reaches nobody and expires on its own. |
| **Exercise scenario (JSON)** | Optional. Blank uses the built-in fictional scenario. Paste a scenario to seed your own; it is stored only in this server's configuration. The format is on the plugin's built-in `slash.html#exercise-scenario` page. |

> To fully funnel non-admins through the request flow, remove the built-in
> **Create Public/Private Channel** permission from the System User role (System
> Console → User Management → Permissions). Mattermost's plugin API can't veto
> native channel creation.

### Domain prefixes

Each prefix is one row with a **prefix**, a **description**, and a **max suffix
length** (2–48 characters, capping the suffix only, not the prefix). A fresh
install starts empty:

<img src="images/admin/mattermost-admin-prefixes.png" alt="Domain-prefix editor, empty state" width="520">

Use **Load preset** to populate the table with a starter pack: **SRE / Ops**,
**Product org**, **Support / Success**, or **General org**. Loading a preset
updates prefixes you already have (description + max length) and appends any that
are missing. The **Live preview** at the bottom shows exactly what URL a request
will produce for a given prefix and typed suffix.

<img src="images/admin/mattermost-admin-prefix-preview.png" alt="Load preset and live preview" width="520">

See also the [Admin Guide](admin-guide.md) and open **`/mattermost-request-hub docs`**
in Mattermost for the bundled HTML guide.
