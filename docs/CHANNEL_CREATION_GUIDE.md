# Channel Requests — User Guide

Mattermost Request Hub lets members **request** a new channel instead of
creating one directly. The request goes to an approver; on approval the channel
is created, the requested members and Channel Admins are added, optionally placed
in a sidebar category, and everyone added gets an @-mention welcome message.

> Companion guides: [Channel Admin](CHANNEL_ADMIN_GUIDE.md) ·
> [teams](TEAM_REQUEST_GUIDE.md) · [bots](BOT_REQUEST_GUIDE.md) ·
> [webhooks](WEBHOOK_REQUEST_GUIDE.md) · [Admin Guide](admin-guide.md).
>
> In Mattermost, open the full in-app guide anytime with
> **`/mattermost-request-hub docs`**.

This guide covers:

- [Requesting a channel](#requesting-a-channel) — any member
- [Requesting from the command line](#requesting-from-the-command-line): skip the form
- [Messaging while a request is pending](#messaging-while-a-request-is-pending) — two-way threads
- [Approving requests](#approving-requests) — approvers
- [Admin setup](#admin-setup) — system admins

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
**move** to the new list automatically — a badge in the dropdown shows where a
user is currently assigned.

Click **Submit request**. You'll see a confirmation, and you'll get a direct
message when an approver responds.

Requests need at least one domain prefix. Until a System Admin defines one, the
form and the slash command show: *Channel requests aren't configured yet. Ask a
System Admin to define at least one channel prefix in System Console → Plugins →
Mattermost Request Hub.*

### Requesting from the command line

`/mattermost-request-hub channel` (or `/mattermost-request-hub` on its own) opens
the request form. Add a prefix, a channel name and options to request a channel
in the current team without the form. Replies are only visible to you.

| Command | What it does |
|---------|--------------|
| `/mattermost-request-hub channel` | Opens the channel request form. |
| `/mattermost-request-hub channel <prefix> <Channel name> [options]` | Requests a channel in the current team without the form. Options are listed below. |
| `/mattermost-request-hub channel-admin @user [@user ...]` | Asks for those channel members to be made Channel Admins of the channel you run it in. See the [Channel Admin guide](CHANNEL_ADMIN_GUIDE.md#from-the-slash-command). |
| `/mattermost-request-hub prefixes` | Lists the channel prefixes you can use, with each one's description and suffix rule. |
| `/mattermost-request-hub help` | Lists every subcommand and its arguments. |

The prefix comes first, typed exactly as `/mattermost-request-hub prefixes` lists
it (for example `team-`). Then comes the channel name, which can be several
words, then any options in any order. Each option takes the words up to the next
`--`.

| Option | What it does |
|--------|--------------|
| `--suffix <suffix>` | The URL part after the prefix, as one word. Leave it out to generate it from the channel name. |
| `--purpose <text>` | A short description of the channel. |
| `--private` | Makes the channel private. Without it the channel is public. |
| `--members @user ...` | Users who become regular members on approval. |
| `--admins @user ...` | Users who become Channel Admins on approval. |
| `--category <name>` | Sidebar category to place the channel in on approval. It can be several words; a missing category is created. |

For example:

```
/mattermost-request-hub channel team- Marketing Team --purpose Campaign planning --members @alice @bob --admins @carol
/mattermost-request-hub channel ops- SRE On-call --suffix sre-oncall --private --category Operations
```

The request goes through the same checks and approval as the form, and you get
the same confirmation. If something is wrong, such as an unknown prefix, an
unknown `@user` or a suffix that breaks the prefix's rule, the reply says what,
and nothing is submitted. If channel requests are turned off, the command says
so.

Put quotes around text that should stay as typed, such as a purpose with a word
that starts with `--`: `--purpose "--draft-- plans"`. The quote marks are not
saved. Users can be separated by spaces or commas, so `@alice @bob` and
`@alice,@bob` mean the same.

Every other request type can be filed from the command line too. Run
`/mattermost-request-hub help` for the list, or see the Slash commands page in
`/mattermost-request-hub docs`.

---

## Messaging while a request is pending

Every request opens a **linked pair of threads**: one on the approval card and
one in a DM from the bot. Replies are relayed both ways.

Use the **Thread** right-hand sidebar (open **Reply** on the card) — not a new
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
themselves. The card names the team the channel will be created in.

<img src="images/channel-request/mattermost-creation-approval-card.png" alt="Channel creation approval card" width="720">

- **Approve** — creates the channel, adds the requested members, promotes the
  Channel Admins, places the channel in the requested sidebar category (if any),
  and posts a welcome message that @-mentions everyone added.
- **Deny** — the request is closed and the requester is notified by DM.

<img src="images/channel-request/mattermost-creation-welcome.png" alt="Welcome message in the new channel" width="720">

Requests from **System Admins** are queued for approval too. A System Admin can
approve their own request.

---

## Admin setup

Configure the plugin in **System Console → Plugins → Mattermost Request Hub**.

<img src="images/admin/mattermost-admin-settings.png" alt="System Console plugin settings" width="280">

| Setting | Purpose |
|---------|---------|
| **Approval team** | The team that owns plugin-managed approval channels. Required. |
| **Allow channel requests** | Enables channel-creation entry points. |
| **Channel domain prefixes** | **Required.** The prefix list requesters pick from (see below). An empty list rejects every channel request. |
| **Team Admins can approve requests** | When on, Team Admins of the approval team can approve/deny channel-creation requests, not just System Admins. |
| **Per-team approval channels** | Default on. Creates `mattermost-channel-requests` in teams with a Team Admin or System Admin; those teams' channel requests go there (not the primary approval team). Ticket channels (including team-creation) live under sidebar category **Mattermost: Request Hub**. |

> To fully funnel non-admins through the request flow, remove the built-in
> **Create Public/Private Channel** permission from the System User role (System
> Console → User Management → Permissions). Mattermost's plugin API can't veto
> native channel creation.

### Domain prefixes

Each prefix is one row with a **prefix**, a **description**, and a **max suffix
length** (2–48 characters, capping the suffix only — not the prefix). A fresh
install starts empty:

<img src="images/admin/mattermost-admin-prefixes.png" alt="Domain-prefix editor, empty state" width="520">

Use **Load preset** to populate the table with a starter pack — **SRE / Ops**,
**Product org**, **Support / Success**, or **General org**. Loading a preset
updates prefixes you already have (description + max length) and appends any that
are missing. The **Live preview** at the bottom shows exactly what URL a request
will produce for a given prefix and typed suffix.

<img src="images/admin/mattermost-admin-prefix-preview.png" alt="Load preset and live preview" width="520">

See also the [Admin Guide](admin-guide.md) and open **`/mattermost-request-hub docs`**
in Mattermost for the bundled HTML guide.
