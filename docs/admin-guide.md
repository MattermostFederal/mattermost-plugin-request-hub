# Admin Guide: Mattermost Request Hub Plugin

This guide covers plugin configuration, the approval workflow, and how each request type is handled. For end-user guides, see the linked request-type docs at the bottom.

---

## Initial setup

Configure the plugin in **System Console → Plugins → Mattermost Request Hub**.

### Per-type toggles

Each request type can be enabled independently. Defaults after install:

| Toggle | Default |
|--------|---------|
| Channel requests | On |
| Channel admin requests | On |
| Team requests | On |
| Team admin requests | On |
| Team member requests | On |
| Team guest requests | Off |
| Bot account requests | On |
| Webhook requests | On |

Disabled types are hidden from the Request Hub and slash commands, and the server rejects submissions.

### Required settings

| Setting | Purpose |
|---------|---------|
| **Approval team** | The team that owns the plugin-managed approval channels. |

### Two-step approvers (bot + webhook)

Bot and webhook requests require **two distinct approvals**: a security step, then a system step. The system step is always a System Admin. **Bot/webhook two-step approver mode** picks who does the security step:

| Mode | Security step | System step |
|------|---------------|-------------|
| **User Attributes** (default) | Holders of the **Security approver attribute** | A System Admin other than the security approver |
| **User Groups** | Members of the **Security approver user group** | A System Admin other than the security approver |
| **System Admins only** | System Admins | System Admins (one System Admin can complete both steps) |

The System Console shows only the setting for the selected mode.

| Setting | Purpose |
|---------|---------|
| **Security approver attribute** | Used in User Attributes mode. User Attribute name (access_control group) that marks security approvers. Default `security_approver`; blank uses the default. System Admins do **not** cover this step. |
| **Security approver user group** | Used in User Groups mode. Default `security-approvers`; blank uses the default. LDAP and plugin-synced groups always count. A custom group counts only when **Allow custom approver groups** is on. System Admins do **not** cover this step. |
| **Allow custom approver groups** | Off by default. Turn it on for servers without LDAP, where the security approver group is a custom group. Anyone who can manage a custom group's members can make themselves a security approver, so restrict who manages custom groups first (System Console > User Management > Permissions > Custom Groups). |

Create and assign attributes in **System Console → User Management → Attributes**. If the security attribute or group doesn't exist or has no members, nobody can fill the security step and bot and webhook cards stay pending. The server log then says so (`the security approver group has no usable members` or `nobody holds the security approver attribute`) on activation and whenever the approver settings change.

Earlier versions had a system approver attribute and group. They are no longer read: the system step now always needs a System Admin.

### Required for channel creation

| Setting | Purpose |
|---------|---------|
| **Channel domain prefixes** | **Required** for channel requests. At least one prefix must be configured. An empty list rejects every channel-creation request. Requesters pick a prefix from a dropdown; the final URL is `<prefix><suffix>`. |

### Optional settings

| Setting | Purpose |
|---------|---------|
| **Team Admins can approve requests** | When on, Team Admins of the approval team can approve or deny channel creation requests. Channel Admin requests are always reviewed by System Admins and the Team Admins of the channel's team, whatever this is set to. |
| **Approval team's Team Admins can approve team and Team Admin requests** | Default off. When on, Team Admins of the approval team can approve or deny team creation and Team Admin requests and are added to `mattermost-team-requests`. They can then make anyone a Team Admin on any team, which is close to System Admin trust. Turn it on only where System Admins are scarce and you trust those Team Admins fully. |
| **Per-team approval channels** | Default **on**. Auto-creates `mattermost-channel-requests` in each team that has a Team Admin or System Admin member. Channel / Channel Admin requests from that team go there (not the primary approval team's root channel). All ticket channels (including **Mattermost: Team Requests**) are kept in a managed sidebar category **Mattermost: Request Hub**. |
| **Offer tools to Agents** | Default **off**. Lets the Mattermost Agents plugin list prefixes, file requests of every type on a user's behalf and look up pending ones (see [Agents tools](#agents-tools)). Takes effect on save. |
| **Pending requests per user** | Default **5**, up to 1000; 0 means no limit. How many requests of each type one user can have waiting for review; each type is counted separately. A second pending request for the same thing is refused: the same channel name in a team, the same nominees in the same channel or team (Channel Admin, Team Admin, team members), the same team URL, the same guest email in a team, the same bot username, or a webhook of the same type and name in the same channel. |
| **Request expiry (days)** | Default **30**, at most 3650; 0 means requests never expire. Pending requests of any type older than this can no longer be approved: clicking Approve or Deny marks the card expired and closes the requester's ticket. Bot and webhook requests count from submission, even after one approval step. Applies to requests already pending too. |
| **Require bot compliance acceptance** | On by default. Bot requesters must accept the **Bot compliance statement**: the form shows it as Terms and Conditions when they submit, and they agree there. The request keeps the exact text, its SHA-256 and the time of acceptance; the approval card shows it was accepted and the audit history records it. A form or API submission must send the SHA-256 of the statement it showed, and is refused without it or when it no longer matches. The slash command needs `--accept-compliance`, which accepts the statement configured at that moment (the one a command without the flag prints), and the Agents `request_bot` tool is refused while this is on. A refused submission (box not ticked, no statement hash, or a statement changed while the form was open) is recorded in the server audit log. The acceptance is an acknowledgement, not an approval: both approval steps are still required. |
| **Bot compliance statement** | The Terms and Conditions bot requesters accept. Blank uses the built-in Terms and Conditions (authorized use only; the requester is responsible for everything done with the bot, its tokens and its webhooks; protecting credentials; the bot stays a regular user; activity may be audited; what they declared is accurate). Changing it doesn't change what earlier requesters accepted. |
| **Bot token lifetime** | **1 to 12 months**, default **12**. How long an access token issued with an approved bot request lasts. The token DM gives the expiry date, and the requester gets reminders before it (see [Token expiry reminders](#token-expiry-reminders)). Only affects tokens issued after the change. Not the same as Request expiry. Needs Mattermost 11.9 or later (see [Token expiry reminders](#token-expiry-reminders)). |
| **Audit channel detail** | Default **Every lifecycle event**. What is posted to `mattermost-audit-requests`: **Decisions only** (approved, denied, expired), **Every lifecycle event** (also submissions, approval steps, failures, token reminders), or **Everything** (also refused approvals and every relayed message). It changes only the channel. The request history and the server audit log always get every event (see [Audit trail](#audit-trail)). |
| **Allow direct audit export** | Default off. Off: every audit export is a request with a reason that a System Admin approves (see [Audit export requests](#audit-export-requests)), and the export API refuses downloads. On: System Admins can also download exports directly from Audit history, `audit export` and the export API. |
| **Enable exercise mode** | Default **off**. **Do not enable in production.** Lets System Admins run `/mattermost-request-hub exercise populate`, `teardown` and `status` to seed and remove marked exercise data in a team: persona accounts nobody can sign in as, channels, and pending requests of every type on real approval cards. Populate needs **Allow channel requests**, at least one prefix, and the setting for each other type the scenario seeds (the built-in scenario seeds all eight). Approving a seeded request does the real thing, marked EXERCISE: a channel, team, bot or webhook, a Team Admin or team member change for a persona, or a guest invite. Teardown deletes the webhooks and bots, archives the teams and channels, and undoes the personas' team changes. Personas and guest invites use `exercise.invalid` email addresses, so a server or team that restricts email domains, or a guest domain list, must allow that domain, and guest access must be on for a scenario with guest invites. A guest invite can't be withdrawn; it reaches nobody and expires on its own. |
| **Exercise scenario (JSON)** | Optional. Blank uses the built-in fictional scenario. Paste a scenario to seed your own; it is stored only in this server's configuration. The format is on the plugin's built-in `slash.html#exercise-scenario` page. |

### Agents tools

**Offer tools to Agents** needs the Mattermost Agents plugin and Mattermost 11.3 or later. Once it's on, its tools appear in the Agents plugin's own settings, where you choose which of them agents may use. Each tool acts as the user the agent is talking to, with the same checks as the form for its type, and follows that type's toggle. Every request goes to the approvers, System Admins' included. There are no approve or deny tools.

| Tool | Does |
|------|------|
| `list_prefixes` | Lists the channel prefixes for `request_channel` |
| `request_channel` | Requests a new channel |
| `request_channel_admin` | Requests Channel Admin for members of a channel |
| `request_team` | Requests a new team |
| `request_team_admin` | Requests Team Admin for members of a team |
| `request_team_member` | Requests that people be added to a team |
| `request_team_guest` | Requests a guest invite to channels of a team |
| `request_bot` | Requests a bot account (account fields only; tokens and webhooks for the bot are in the form) |
| `request_webhook` | Requests an incoming or outgoing webhook |
| `list_my_requests` | Lists the user's pending requests of every type |
| `list_requests_to_approve` | Lists the pending requests the user can approve now (read only) |
| `get_request_status` | Looks up one pending request of any type by ID |

### Locking down native channel creation

The plugin intercepts the sidebar UI for non-admins, but it can't veto the native Mattermost channel creation API. To fully route members through the request flow, remove the **Create Public/Private Channel** permission from the System User role in **System Console → User Management → Permissions → System Scheme**.

### Locking down native Team Admin promotion

Team Admins can promote their own team's members in Mattermost directly, without a request. To make the plugin the only way to become a Team Admin, remove **Manage team roles** from the Team Admin role in **System Console → User Management → Permissions → System Scheme**. The plugin still promotes approved requests, because it acts through the plugin API. Only System Admins approve Team Admin requests unless **Approval team's Team Admins can approve team and Team Admin requests** is on.

---

## Approval channels

The plugin creates and manages five dedicated private channels in the configured approval team. System Admins who are members of the approval team are added to these channels when each channel is created and again whenever the plugin activates. The plugin doesn't add anyone to the approval team itself, except two-step approvers (see [Who sees and approves what](#who-sees-and-approves-what)).

| Channel | What routes here |
|---------|-----------------|
| `mattermost-channel-requests` (root or per-team) | Channel creation, Channel Admin |
| `mattermost-team-requests` | Team creation, Team Admin, team member add, guest invite |
| `mattermost-bot-requests` | Bot account requests (including associated tokens and webhooks) |
| `mattermost-webhook-requests` | Standalone incoming and outgoing webhook requests |
| `mattermost-audit-requests` | Audit cards (see [Audit trail](#audit-trail)) |

Each channel gets a **Request Hub docs** bookmark that links to the built-in guide. The plugin bot joins the channel to add it. Channels that existed before the upgrade get the bookmark once, on the next activation that has **Site URL** set. If you remove the bookmark, the plugin doesn't add it back.

If one of these channels is archived, the plugin doesn't recreate it: the archived channel still holds the name. Unarchive it (or rename it, so the plugin can create a new one). Until then, cards for that channel are DM'd to System Admins, and channel requests for a per-team channel go to the root channel.

---

## Who sees and approves what

### Who is in each channel

- **Approval team channels** (root `mattermost-channel-requests`, `mattermost-team-requests`, `mattermost-bot-requests`, `mattermost-webhook-requests`, `mattermost-audit-requests`): private channels created by the plugin's bot. System Admins on the approval team are added when a channel is created and on every activation.
- **Two-step approvers:** in User Attributes or User Groups mode, members of the security pool are added to the approval team and to the bot and webhook channels. This is kept in step on activation, when the pool settings change, at login and every few minutes. Someone who leaves a pool is removed from those two channels, unless they are a System Admin or a Team Admin of the approval team.
- **Per-team channels** (`mattermost-channel-requests` in other teams, when **Per-team approval channels** is on): created in each team that has a Team Admin or a System Admin member, and seeded with those people. A user who joins the team later as an admin is added when they join; someone promoted to Team Admin in Mattermost itself (not through a Request Hub request) is added the next time they join the team. Existing per-team channels aren't reseeded on activation. In the approval team, the per-team channel and the root channel are the same channel.

People who aren't members of a channel can't see its cards. The requester gets their own copy of the card, without buttons, as the root of a DM thread with the bot.

### Where each card goes

- **Channel and Channel Admin:** the per-team channel of the request's team. If that team has no Team Admin or System Admin, the card goes to the root channel labeled `For Team: …`. With **Per-team approval channels** off, every card goes to the root channel.
- **Team, Team Admin, team member and guest:** `mattermost-team-requests`.
- **Bot:** `mattermost-bot-requests`. **Webhook:** `mattermost-webhook-requests`.
- **No channel available** (for example, **Approval team** is blank, missing or archived): the card is DM'd to every System Admin instead, and the message relay isn't available for that request. The requester sees an error only if no System Admin could be reached.

The Approve and Deny buttons work only on the cards the plugin posted for that request. Approvers can also use `/mattermost-request-hub approve <request ID>` or `deny <request ID>` from any channel, for every request type, with the same checks as the buttons. `/mattermost-request-hub list approvals` lists the requests they can approve, with their IDs.

### Who can approve

| Request type | Approvers |
|---|---|
| Channel creation | System Admins and Team Admins of the team the channel will be in. With **Team Admins can approve requests** on, also Team Admins of the approval team. |
| Channel Admin | System Admins and Team Admins of the channel's team |
| Team creation, Team Admin | System Admins. With **Approval team's Team Admins can approve team and Team Admin requests** on, also Team Admins of the approval team. |
| Team member, guest invite | System Admins |
| Bot, incoming webhook, outgoing webhook | Two steps, security first, then system, by two different people (one System Admin in System Admins only mode). See [Two-step approvers](#two-step-approvers-bot--webhook). |

Approving your own request:

- **Channel, Channel Admin, team creation and Team Admin:** only a System Admin can approve their own request.
- **Team member and guest invite:** System Admins only, so a System Admin can approve their own.
- **Bot and webhook:** nobody can approve either step of their own request, System Admins included. A System Admin's request needs two other approvers, or one other System Admin in System Admins only mode.

Being allowed to approve doesn't make a card visible. Team Admins of the approval team are added to the root channel the same way as to any per-team channel (while **Per-team approval channels** is on, when they join the approval team or are promoted through a Request Hub request), and never to other teams' channels. So **Team Admins can approve requests** mostly matters for cards in the root channel. Likewise, when **Per-team approval channels** is off, Team Admins of other teams can approve their teams' channel cards but usually can't see them, because those cards go to the root channel.

---

## Approving and denying requests

Every request posts a card to the appropriate approval channel. The card includes the request details, the requester's username, and **Approve** and **Deny** buttons.

[Screenshot: A representative request card showing request details and Approve/Deny buttons]

Channel, Channel Admin, team, Team Admin, team member and guest cards use **single-step** approval: one approver decides (see [Who can approve](#who-can-approve)).

Bot and webhook cards use **two-step** approval. The card shows security and system status lines. Each eligible approver clicks **Approve** once; two different people must fill the two steps before the resource is created. Any eligible approver may **Deny** at any time.

### What each approval does

| Request type | On Approve |
|--------------|-----------|
| **Channel creation** | Creates the channel, adds members, promotes Channel Admins, places in sidebar category if requested |
| **Channel Admin** | Promotes the nominees to Channel Admin. Nominees must already be channel members when the request is made. Approve is refused if a nominee has since left the channel or been deactivated |
| **Team Admin** | Promotes the nominees to Team Admin on that team. Nominees must already be team members. From the Request Hub or More actions the nominee is the requester; the slash command and its form can name other team members |
| **Team creation** | Creates the team, adds the requester, optionally promotes to Team Admin |
| **Bot account** | After both steps: creates the bot, optional token/webhooks; secrets go to a DM that auto-deletes in 72h |
| **Incoming webhook** | After both steps: creates the webhook; URL is DM'd privately. If delivery fails, the hook is deleted |
| **Outgoing webhook** | After both steps: creates the webhook and confirms via DM |

**Deny** always: closes the ticket, notifies the requester by DM, records who denied and when.

---

## Audit trail

Every event on every request is recorded, with who did it and when:

- the submission, and a submission that was refused or failed and was withdrawn
- each step of a two-step approval, and the final approval or denial
- an Approve or Deny that was refused (no permission, own request, already handled, a failed re-check)
- an approval that failed and left the request pending, or that lost the request
- an approval that was only partly carried out (a team member who couldn't be added, a bot token that couldn't be made)
- a bot token or incoming webhook URL that couldn't be delivered, so what was made was removed
- expiry, which is recorded as the plugin's doing with the person whose click noticed it
- every message in the request's ticket threads, with its text, including messages that weren't relayed
- bot token expiry reminders, the end of reminders, and the deletion of the message that delivered the token
- exercise mode seeding and teardown, and requests upgraded from older plugin versions

Tokens and incoming webhook URLs are never recorded.

Events go to three places:

| Where | What it is | Who can see it |
|-------|------------|----------------|
| Request history | A record per request in the plugin's KV store, kept after the request is decided. Nothing deletes it. | System Admins, in **Audit history** in the Request Hub, with `/mattermost-request-hub audit`, and the export below. Security approvers can ask for a copy (see [Audit export requests](#audit-export-requests)). |
| Server audit log | One `requesthub_<event>` record per event (for example `requesthub_submitted`, `requesthub_approved`), with the actor, request ID, kind, requester and details. Failed events have status `fail`. | Wherever your server's audit logging sends records. Audit logging must be turned on for the server. |
| `mattermost-audit-requests` | Cards people can read, as chosen by **Audit channel detail** | Members of the audit channel |

### Turning on the server audit log

The plugin sends its records through the server's own audit logger, so they go wherever the server's audit records go. Nothing is written until audit logging is turned on:

- **File:** in **System Console > Compliance > Audit Logging**, turn on **File Enabled** and set **File Name** (the console shows this page on Enterprise licenses). On other editions, set `ExperimentalAuditSettings.FileEnabled` and `ExperimentalAuditSettings.FileName` in `config.json` or with `mmctl config set`. The file has one JSON record per line.
- **Syslog, TCP and other targets:** use **Advanced Logging** (`ExperimentalAuditSettings.AdvancedLoggingJSON`), which needs a license with advanced logging. Plugin records use the `audit-cli` level, so a target must include that level.

Changes to these settings need a server restart. Every record from the plugin has `event_name` starting with `requesthub_` and a `plugin_id` parameter of `com.mattermostfederal.mattermost-plugin-request-hub`, so a SIEM can filter on either.

### Audit history in the Request Hub

System Admins see an **Audit history** card in the Request Hub. It lists every request, newest activity first, and can be searched by request ID, name or requester and filtered by type and status. Opening a request shows its timeline: every event, who did it and when, the text of ticket thread messages, and each event's details. When **Allow direct audit export** is on, download buttons give the whole history, every request of one type, or one request, as CSV or JSON, and the [bot inventory](#bot-inventory) as PDF, CSV or JSON. When it is off (the default), the window has a **Request an export** button instead.

### Audit export requests

Security approvers (the security group or attribute set under **Two-step approvers**) who aren't System Admins see **Request Audit Export** in the Request Hub instead. While **Allow direct audit export** is off, System Admins request exports too, from **Request an export** in Audit history or with the slash command, and a different System Admin must approve them. The slash command is:

```
/mattermost-request-hub audit export [type] [format] [--request ID] [--kind kind] [--env name] [--bot @name] [--requester @name] [--since YYYY-MM-DD] [--until YYYY-MM-DD] --reason text
```

The requester picks the export type and a format that type comes in:

| Type | Formats (default first) | Narrowed by |
|------|-------------------------|-------------|
| `full_histories` | CSV, JSON | Request, request type, requester, dates the request was active |
| `bot_inventory` | PDF, CSV, JSON | Hosting environment (`--env`), requester, dates the bot was approved |
| `bot_forensic` | JSON | One bot (`--bot`, required) and the dates it posted (`--since` and `--until`, both required, at most 90 days apart) |

They must give a reason. The approval card shows the export type, format, what it covers and the reason. The request goes to `mattermost-audit-requests` (or to System Admins by direct message when there is no audit channel), and only a System Admin can approve or deny it, with the card's buttons or `approve`/`deny` by ID. Nobody can approve their own export request. Approving builds the export as asked and posts it as a file in the requester's request thread with the Request Hub bot. If the file can't be sent, the request stays pending. The request has no on/off setting: who may ask is decided by the security approver pool, and it expires like other requests.

### Exporting

While **Allow direct audit export** is on, System Admins can download exports from `/plugins/com.mattermostfederal.mattermost-plugin-request-hub/api/v1/audit/export`. `audit recent` and `audit export` link to it. While it is off, the endpoint answers `403 Direct export is off. Request an export instead.` The export is a `GET` with these optional parameters:

| Parameter | Values |
|-----------|--------|
| `type` | `full_histories` (default), `bot_inventory` or `bot_forensic` |
| `format` | `json` (default). For `full_histories`: `json` gives one object per request, with its events, and `csv` one row per event, with the ticket thread message text and the event's other details (as JSON) in their own columns. For `bot_inventory`: `json`, `csv` or `pdf`, one entry per bot. For `bot_forensic`: `json` only. |
| `request` | One request ID (`full_histories` only) |
| `kind` | One request kind: `channel`, `admin` (Channel Admin), `team`, `team_admin`, `team_member`, `team_guest`, `bot`, `webhook`, `audit_export` (`full_histories` only) |
| `env` | One hosting code: `ci_cd`, `on_prem_server`, `cloud_or_k8s`, `workstation`, `external_saas` or `other`. A bot requested before these codes keeps the environment it was requested with (such as `mattermost-prod`), which also works here. (`bot_inventory` only) |
| `bot` | The bot's user ID (`bot_forensic` only, and required there) |
| `requester` | A requester's user ID (not for `bot_forensic`) |
| `since`, `until` | Unix time in milliseconds. For `bot_inventory`, the date the bot was approved. For `bot_forensic`, when the bot posted: both are required and at most 90 days apart. |

Each export is itself recorded in the server audit log as `requesthub_audit_exported`.

Every cell of a CSV export that a spreadsheet could run as a formula (one starting with `=`, `+`, `-` or `@`, even after spaces, or with a tab, carriage return or other control character) starts with an apostrophe.

If the history can't be saved for an event, the server log has `failed to save request audit history` with the whole event.

### Bot inventory

The `bot_inventory` export lists every bot Request Hub created, oldest first, for Security to review the fleet. Each bot has:

- its user ID, username and display name, and its status as of the export: `active`, `deactivated`, `deleted`, or `unknown` when the server could not be asked
- its home team and channel, by ID and by their current display names
- what the requester declared: purpose, where the integration runs, the integration endpoint and the architecture link. Privilege level and data ingestion are the plugin's fixed policy for new bots (a regular user, with member permissions in the channels it is added to); bots requested before then show what was declared
- the owner. A bot requested without one is owned by its requester; the export shows the bot's owner in Mattermost when it has one
- the requester, the owner (when another user owns it), the request ID, when it was requested, and who approved it and when
- whether the requester accepted the bot compliance statement, when, and the SHA-256 of the statement's text
- `rh_managed: true` and the time of the export (`as_of`)

The PDF is for reading: a title, what it covers and one block per bot. The CSV and JSON have the same fields for tooling, and the CSV guards against formulas as above.

The inventory is built from the bot index Request Hub writes when it creates a bot, so it lists only bots created after the index was added. Bots created outside Request Hub are not listed. The declared fields are the requester's word: they do not limit what the bot's token can do.

### Bot forensics

The `bot_forensic` export traces one bot over at most 90 days, for Security to answer "what did this bot do". It is one JSON document with:

- `rh_managed`, and when Request Hub created the bot, its bot index entry (`index`) and the full history of the request that created it (`history`)
- the bot record and its status (`active`, `deactivated` or `deleted`)
- `teams`: the teams the bot is in now, each with the channels it is in now (direct and group messages left out)
- `scanned_channels`: every channel searched, and why: `member`, `home` or `webhook`. The home channel and the channels its webhooks post to are searched even when the bot has left them.
- `posts`: every post the bot made in those channels in the date range, oldest first, with its message, props and file IDs. This includes system posts made as the bot, such as joining a channel.
- `files`: the name, size and type of each file attached to those posts
- `post_count`, `truncated` and, when it is cut short, `truncated_reason`
- `limitations`: what the export can't show

It stops at 5,000 posts or 20 MB of posts, whichever comes first, or after reading 200,000 posts in the scanned channels without reaching the start date. It then sets `truncated: true`; ask for a shorter date range to see the rest.

It does not show:

- posts in channels the bot has left, other than its home and webhook channels
- direct and group messages, which follow the server's own compliance export
- deleted posts
- the API calls made with the bot's tokens. Plugins can't see them; look in the server audit log or your SIEM.

The export can trace a bot Request Hub didn't create (`rh_managed: false`), as long as the bot still exists. The audit event for the export records the filter, the post count, whether it was truncated and how many channels were scanned, never the posts themselves.

A request's history keeps relayed messages in full until it reaches 2 MB. After that, each new message is kept to its first 2 KB in the history (marked `message_truncated`), so a very long ticket thread can't stop the history being saved. The server audit log always gets the full text.

Requests that were already pending when auditing was added get their history from their next event on.

---

## Two-way DM relay

When a request is submitted, the bot sends the requester a DM with a thread for that request. Admins can communicate with the requester through the approval thread:

- A reply in the **approval thread** is relayed to the requester's **DM thread** as `Message from @reviewer: …`.
- A reply in the requester's **DM thread** is relayed to the **approval thread** as `Reply from @requester: …`.

This lets reviewers ask clarifying questions (for example, *Why do you need this new team?*) and get responses without leaving the approval channel, and without the requester needing access to the approval channel. Conversation happens in Mattermost's **Thread** right-hand sidebar (Reply on the card), not as new top-level posts. Threads remain linked after approve/deny for follow-up. Root-level DMs to the bot (outside a ticket thread) get a nudge to reply inside the specific request.

![Approver asks in the Thread RHS](images/shared/mattermost-thread-approver-ask.png)

![Requester sees the question in their DM Thread RHS and replies](images/shared/mattermost-thread-requester-dm.png)

![Requester's reply appears back in the approval Thread RHS](images/shared/mattermost-thread-relay-back.png)

End-user guides document the same flow with request-type examples:

- [Channel](CHANNEL_CREATION_GUIDE.md#messaging-while-a-request-is-pending)
- [Channel Admin](CHANNEL_ADMIN_GUIDE.md#messaging-while-a-request-is-pending)
- [Team](TEAM_REQUEST_GUIDE.md#messaging-while-a-request-is-pending)
- [Bot](BOT_REQUEST_GUIDE.md#messaging-while-a-request-is-pending)
- [Webhook](WEBHOOK_REQUEST_GUIDE.md#messaging-while-a-request-is-pending)

---

## Bot approvals in detail

A single bot approval ticket can cover three operations: creating the bot account, generating an access token, and creating incoming and/or outgoing webhooks.

Bot requests need a **security** approval and a **system** approval from two different people. When both steps are filled, the server creates the bot and optional extras. If creation fails, the card stays pending so the final approver can retry. Webhooks are created with the final approver's session, so when a bot request asks for a webhook, the final approver must be allowed to manage other people's webhooks in that team (Team Admins and System Admins are, by default). Anyone else gets a private reply asking for someone who is, and the request stays pending.

### Token delivery

Access tokens are generated at approve-time by the plugin, whoever the final approver is; no credential is stored. The token value is sent to the requester in a direct message that **auto-deletes after 72 hours**. The token expires after **Bot token lifetime** (1 to 12 months, default 12), and the DM gives the date. Token expiry needs Mattermost 11.9 or later.

### Token expiry reminders

For each token it issues, the plugin keeps the bot, the requester and the expiry date. It never keeps the token value. From a month before the expiry the requester gets a weekly direct message naming the bot and the date, then a daily one in the last week, and one last message once the token has expired. If the requester's account is deactivated or deleted, every System Admin gets the reminders instead. Reminders stop when the bot is deleted or disabled.

Token expiry needs **Mattermost 11.9 or later**. Older servers ignore the expiry date when a token is created, so the token never expires. On those servers the token DM gives no date, no reminders are sent and **Bot token lifetime** has no effect. Revoke tokens by hand (**System Console > User Management > Users**, then the bot's tokens) if you need them to end.

The reminders say how to get a new token: a System Admin creates one under **Integrations → Bot Accounts**, or the requester asks for a new bot. Tokens issued before reminders were added, or created outside Request Hub, get none.

If a token is suspected compromised, revoke it in **System Console → User Management → Bot Accounts → {bot name}** and issue a new one.

---

## Webhook approvals in detail

Webhook requests use the same two-step engine as bots. The final approver's session is used for a single create call and is never stored, so the final approver must be allowed to manage other people's webhooks in the channel's team (Team Admins and System Admins are, by default). Anyone else gets a private reply asking for someone who is, and the request stays pending.

**Webhook requests from System Admins get both steps too.** Nobody can approve either step of their own request, so a System Admin's request needs a security approver and a different System Admin (or one other System Admin in System Admins only mode). If creation fails after both steps, the card stays pending for retry. If an incoming webhook URL cannot be DM'd to the requester, the hook is deleted.

---

## Domain prefix configuration

If you set channel domain prefixes, requesters must pick a prefix from a dropdown. The final channel URL is `<prefix><suffix>`.

[Screenshot: The domain-prefix editor with several rows filled in]

Each row has:
- **Prefix**: the fixed URL segment (e.g. `ops-`, `prod-`)
- **Description**: shown in the dropdown to help requesters choose
- **Max suffix length**: caps how many characters the requester can append (2–48; presets default to 48)

Use **Load preset** to populate from a starter pack. The **Live preview** at the bottom shows what a given prefix and suffix combination produces.

[Screenshot: Domain prefix editor with preset loaded and live preview showing example URL]

---

## Per-team approval channels

When **Per-team approval channels** is enabled (default), the plugin ensures a
`mattermost-channel-requests` channel in every team that has at least one
**Team Admin** or **System Admin** as a member. Channel creation and Channel
Admin requests from that team route there instead of the primary approval
team's root channel.

Teams with no local Team Admin / System Admin fall back to the root approval
channel (card labeled `For Team: …`).

Team Admins and System Admins who belong to the team are seeded into the
per-team channel and placed under the managed sidebar category
**Mattermost: Request Hub**. The dedicated **Mattermost: Team Requests**
channel (team-creation tickets) gets the same category treatment. On plugin
activate, a one-time migration forces all existing ticket/approval channels
(root, bot, webhook, team-creation, audit, and per-team) into that category
for current members. A category from an earlier version named
`Mattermost: Permissions` or `Mattermost Requests` is renamed to
**Mattermost: Request Hub**.

> Upgrades that already saved `PerTeamApprovalChannels = false` keep that
> value. Turn the setting back **on** in System Console if channel requests
> are still landing only in the primary team.

---

## Upgrading

### From Channel Requests (plugin ID `com.mattermostfederal.channel-requests`)

Request Hub has a different plugin ID, so Mattermost treats it as a separate plugin. Nothing is imported from Channel Requests: its settings and its pending requests stay with the old plugin ID.

1. Let pending requests in the old plugin be approved or denied first. Once the old plugin is disabled, its cards stop working.
2. Note the old settings, such as the approval team and the channel prefixes.
3. Disable and remove the old plugin, then install Request Hub.
4. Before enabling Request Hub, review its settings. A new install turns on every request type except guest invites, including types the old plugin didn't have (team, Team Admin, team member, bot, webhook). Turn off the ones you don't want yet.
5. Enter the approval team and the other settings again, then enable the plugin.

Channel Requests posted its cards to an approval channel you chose. Request Hub doesn't use that channel: it creates its own private channels in the approval team (see [Approval channels](#approval-channels)). If a private channel with one of those names already exists and a System Admin or this plugin's bot created it (for example on an upgraded server, or after the plugin's bot was recreated), Request Hub adds its bot to that channel and uses it. Any other channel with one of those names (public, archived, or created by a regular user or by another bot) is not used: the plugin logs an error and sends cards to System Admins by direct message instead. Rename that channel so the plugin can create its own.

### Between Request Hub versions

Settings and pending requests carry over. Each time the plugin activates it:

- creates any missing approval team channels and adds System Admins on the approval team to them;
- adds the two-step approvers to the bot and webhook channels;
- creates missing per-team channels when **Per-team approval channels** is on (existing ones aren't reseeded);
- runs the one-time sidebar category migration described in [Per-team approval channels](#per-team-approval-channels);
- deletes token and webhook URL DMs older than 72 hours.

Once, on the first configuration load, channel prefixes saved with the old maximum suffix length of 24 are raised to 48.

The plugin records which team **Approval team** named the first time it was used. If that name later belongs to a different team (for example, the original was renamed or deleted and another team took the name), the plugin stops using it until the setting is changed: set **Approval team** to a different team, save, then set it back.

---

## Request type reference

| Request type | Entry points | Approval channel | Approved by |
|---|---|---|---|
| Channel creation | Sidebar +, channel header button, More actions, slash cmd | Root channel or per-team channel | Team Admins or System Admins |
| Channel Admin | More actions, Members panel button, slash cmd | Root channel or per-team channel | Team Admins of the channel's team or System Admins |
| Team Admin | More actions, slash cmd | `mattermost-team-requests` | System Admins, or approval-team Team Admins when enabled |
| Team creation | More actions, team sidebar menu | `mattermost-team-requests` | System Admins, or approval-team Team Admins when enabled |
| Team member add | More actions, Request Hub, slash | `mattermost-team-requests` | System Admins |
| Guest invite | More actions, Request Hub, slash | `mattermost-team-requests` | System Admins |
| Bot account (+ token + webhooks) | More actions, Integrations page | `mattermost-bot-requests` | Two-step: security, then system |
| Incoming webhook (standalone) | More actions, Integrations page | `mattermost-webhook-requests` | Two-step: security, then system |
| Outgoing webhook (standalone) | More actions, Integrations page | `mattermost-webhook-requests` | Two-step: security, then system |
| Audit export | Request Hub, `audit export` (security approvers, and System Admins while direct export is off) | `mattermost-audit-requests` | System Admins (not the requester) |

Every request is queued for approval, including requests from System Admins.

---

## Related guides

- [Channel Requests: User Guide](CHANNEL_CREATION_GUIDE.md)
- [Channel Admin Requests: User Guide](CHANNEL_ADMIN_GUIDE.md)
- [Team Requests: User Guide](TEAM_REQUEST_GUIDE.md)
- [Team Membership & Guests: User Guide](TEAM_MEMBERSHIP_GUIDE.md)
- [Bot Account Requests: User Guide](BOT_REQUEST_GUIDE.md)
- [Webhook Requests: User Guide](WEBHOOK_REQUEST_GUIDE.md)
