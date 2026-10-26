# Admin Guide — Mattermost Request Hub Plugin

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

### Two-step approver attributes (bot + webhook)

Bot and webhook requests require **two distinct approvals**: one security step and one system step.

| Setting | Purpose |
|---------|---------|
| **Security approver attribute** | User Attribute name (access_control group) that marks security-step approvers. System Admins do **not** auto-qualify for this step. |
| **System approver attribute** | User Attribute name for system-step approvers. System Admins auto-qualify for this step. |

Create and assign these attributes in **System Console → User Management → Attributes**. If they are unset or no users hold them, bot/webhook cards cannot complete.

### Required for channel creation

| Setting | Purpose |
|---------|---------|
| **Channel domain prefixes** | **Required** for channel requests. At least one prefix must be configured — an empty list rejects every channel-creation request. Requesters pick a prefix from a dropdown; the final URL is `<prefix><suffix>`. |

### Optional settings

| Setting | Purpose |
|---------|---------|
| **Team Admins can approve requests** | When on, Team Admins of the approval team can approve or deny channel creation requests. Channel Admin and Team Admin requests are always reviewed by System Admins and the Team Admins of the requested team, whatever this is set to. |
| **Team Admins can approve team creation** | Default off. When on, Team Admins of the approval team can approve or deny team creation requests and are added to `mattermost-team-requests`. Useful when System Admins are scarce. |
| **Per-team approval channels** | Default **on**. Auto-creates `mattermost-channel-requests` in each team that has a Team Admin or System Admin member. Channel / Channel Admin requests from that team go there (not the primary approval team's root channel). All ticket channels (including **Mattermost: Team Requests**) are kept in a managed sidebar category **Mattermost: Request Hub**. |

### Locking down native channel creation

The plugin intercepts the sidebar UI for non-admins, but it can't veto the native Mattermost channel creation API. To fully route members through the request flow, remove the **Create Public/Private Channel** permission from the System User role in **System Console → User Management → Permissions → System Scheme**.

---

## Approval channels

The plugin creates and manages five dedicated private channels in the configured approval team. All System Admins are added as members when the plugin activates (and when each channel is first created).

| Channel | What routes here |
|---------|-----------------|
| `mattermost-channel-requests` (root or per-team) | Channel creation, Channel Admin |
| `mattermost-team-requests` | Team creation, Team Admin, team member add, guest invite |
| `mattermost-bot-requests` | Bot account requests (including associated tokens and webhooks) |
| `mattermost-webhook-requests` | Standalone incoming and outgoing webhook requests |
| `mattermost-audit-requests` | Audit log |

If a channel is accidentally archived, the plugin recreates it on next activation.

---

## Approving and denying requests

Every request posts a card to the appropriate approval channel. The card includes the request details, the requester's username, and **Approve** and **Deny** buttons.

[Screenshot: A representative request card showing request details and Approve/Deny buttons]

Channel, channel-admin, team, and team-admin cards use **single-step** approval (System Admins, plus Team Admins when enabled for channel types).

Bot and webhook cards use **two-step** approval. The card shows security and system status lines. Each eligible approver clicks **Approve** once; two different people must fill the two steps before the resource is created. Any eligible approver may **Deny** at any time.

### What each approval does

| Request type | On Approve |
|--------------|-----------|
| **Channel creation** | Creates the channel, adds members, promotes Channel Admins, places in sidebar category if requested |
| **Channel Admin** | Adds nominees to the channel (if not already members) and promotes them to Channel Admin |
| **Team Admin** | Promotes the requester to Team Admin on their current team |
| **Team creation** | Creates the team, adds the requester, optionally promotes to Team Admin |
| **Bot account** | After both steps: creates the bot, optional token/webhooks; secrets go to a DM that auto-deletes in 72h |
| **Incoming webhook** | After both steps: creates the webhook; URL is DM'd privately. If delivery fails, the hook is deleted |
| **Outgoing webhook** | After both steps: creates the webhook and confirms via DM |

**Deny** always: closes the ticket, notifies the requester by DM, records who denied and when.

---

## Two-way DM relay

When a request is submitted, the bot sends the requester a DM with a thread for that request. Admins can communicate with the requester through the approval thread:

- A reply in the **approval thread** is relayed to the requester's **DM thread** as `Message from @reviewer: …`.
- A reply in the requester's **DM thread** is relayed to the **approval thread** as `Reply from @requester: …`.

This lets reviewers ask clarifying questions (for example, *Why do you need this new team?*) and get responses without leaving the approval channel, and without the requester needing access to the approval channel. Conversation happens in Mattermost's **Thread** right-hand sidebar (Reply on the card) — not as new top-level posts. Threads remain linked after approve/deny for follow-up. Root-level DMs to the bot (outside a ticket thread) get a nudge to reply inside the specific request.

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

Bot requests need a **security** approval and a **system** approval from two different people. When both steps are filled, the server creates the bot and optional extras. If creation fails, the card stays pending so the final approver can retry.

### Token delivery

Access tokens are generated at approve-time using the plugin's bot account, not a stored credential. The token value is sent to the requester in a direct message that **auto-deletes after 72 hours**. The token itself does not expire unless revoked.

If a token is suspected compromised, revoke it in **System Console → User Management → Bot Accounts → {bot name}** and issue a new one.

---

## Webhook approvals in detail

Webhook requests use the same two-step engine as bots. The final approver's session is used for a single create call — never stored.

**Webhook requests from System Admins get both steps too.** A System Admin can fill the system step; a security approver must still approve. If creation fails after both steps, the card stays pending for retry. If an incoming webhook URL cannot be DM'd to the requester, the hook is deleted.

---

## Domain prefix configuration

If you set channel domain prefixes, requesters must pick a prefix from a dropdown. The final channel URL is `<prefix><suffix>`.

[Screenshot: The domain-prefix editor with several rows filled in]

Each row has:
- **Prefix** — the fixed URL segment (e.g. `ops-`, `prod-`)
- **Description** — shown in the dropdown to help requesters choose
- **Max suffix length** — caps how many characters the requester can append (2–48; presets default to 48)

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
for current members (renaming a short-lived `Mattermost Requests` label if
present).

> Upgrades that already saved `PerTeamApprovalChannels = false` keep that
> value — turn the setting back **on** in System Console if channel requests
> are still landing only in the primary team.

---

## Request type reference

| Request type | Entry points | Approval channel | Approved by |
|---|---|---|---|
| Channel creation | Sidebar +, channel header button, More actions, slash cmd | Root channel or per-team channel | Team Admins or System Admins |
| Channel Admin | More actions, Members panel button, slash cmd | Root channel or per-team channel | Team Admins of the channel's team or System Admins |
| Team Admin | More actions, slash cmd | `mattermost-team-requests` | System Admins |
| Team creation | More actions, team sidebar menu | `mattermost-team-requests` | System Admins |
| Team member add | More actions, Request Hub, slash | `mattermost-team-requests` | System Admins |
| Guest invite | More actions, Request Hub, slash | `mattermost-team-requests` | System Admins |
| Bot account (+ token + webhooks) | More actions, Integrations page | `mattermost-bot-requests` | Two-step: security, then system |
| Incoming webhook (standalone) | More actions, Integrations page | `mattermost-webhook-requests` | Two-step: security, then system |
| Outgoing webhook (standalone) | More actions, Integrations page | `mattermost-webhook-requests` | Two-step: security, then system |

Every request is queued for approval, including requests from System Admins.

---

## Related guides

- [Channel Requests — User Guide](CHANNEL_CREATION_GUIDE.md)
- [Channel Admin Requests — User Guide](CHANNEL_ADMIN_GUIDE.md)
- [Team Requests — User Guide](TEAM_REQUEST_GUIDE.md)
- [Team Membership & Guests — User Guide](TEAM_MEMBERSHIP_GUIDE.md)
- [Bot Account Requests — User Guide](BOT_REQUEST_GUIDE.md)
- [Webhook Requests — User Guide](WEBHOOK_REQUEST_GUIDE.md)
