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

### Two-step approvers (bot + webhook)

Bot and webhook requests require **two distinct approvals**: one security step and one system step. **Bot/webhook two-step approver mode** picks where the approvers come from:

| Mode | Security step | System step |
|------|---------------|-------------|
| **User Attributes** (default) | Holders of the **Security approver attribute** | System Admins and holders of the **System approver attribute** |
| **User Groups** | Members of the **Security approver user group** | System Admins and members of the **System approver user group** |
| **System Admins only** | System Admins | System Admins (one System Admin can complete both steps) |

| Setting | Purpose |
|---------|---------|
| **Security approver attribute** | User Attribute name (access_control group) that marks security-step approvers. System Admins do **not** auto-qualify for this step while it is set. |
| **System approver attribute** | User Attribute name for system-step approvers. System Admins always qualify for this step. |
| **Security approver user group** / **System approver user group** | Used in User Groups mode. Only LDAP or plugin-synced groups count; custom groups are ignored, because any user can join them. |

Create and assign attributes in **System Console → User Management → Attributes**. If an attribute or group name is left blank, System Admins cover that step. If a security attribute or group is named but doesn't exist or has no members, nobody can fill the security step and bot/webhook cards stay pending.

### Required for channel creation

| Setting | Purpose |
|---------|---------|
| **Channel domain prefixes** | **Required** for channel requests. At least one prefix must be configured — an empty list rejects every channel-creation request. Requesters pick a prefix from a dropdown; the final URL is `<prefix><suffix>`. |

### Optional settings

| Setting | Purpose |
|---------|---------|
| **Team Admins can approve requests** | When on, Team Admins of the approval team can approve or deny channel creation requests. Channel Admin requests are always reviewed by System Admins and the Team Admins of the channel's team, whatever this is set to. |
| **Approval team's Team Admins can approve team and Team Admin requests** | Default off. When on, Team Admins of the approval team can approve or deny team creation and Team Admin requests and are added to `mattermost-team-requests`. They can then make anyone a Team Admin on any team, which is close to System Admin trust. Turn it on only where System Admins are scarce and you trust those Team Admins fully. |
| **Per-team approval channels** | Default **on**. Auto-creates `mattermost-channel-requests` in each team that has a Team Admin or System Admin member. Channel / Channel Admin requests from that team go there (not the primary approval team's root channel). All ticket channels (including **Mattermost: Team Requests**) are kept in a managed sidebar category **Mattermost: Request Hub**. |

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
| `mattermost-audit-requests` | Audit log |

If one of these channels is archived, the plugin doesn't recreate it: the archived channel still holds the name. Unarchive it (or rename it, so the plugin can create a new one). Until then, cards for that channel are DM'd to System Admins, and channel requests for a per-team channel go to the root channel.

---

## Who sees and approves what

### Who is in each channel

- **Approval team channels** (root `mattermost-channel-requests`, `mattermost-team-requests`, `mattermost-bot-requests`, `mattermost-webhook-requests`, `mattermost-audit-requests`): private channels created by the plugin's bot. System Admins on the approval team are added when a channel is created and on every activation.
- **Two-step approvers:** in User Attributes or User Groups mode, members of the security and system pools are added to the approval team and to the bot and webhook channels. This is kept in step on activation, when the pool settings change, at login and every few minutes. Someone who leaves a pool is removed from those two channels, unless they are a System Admin or a Team Admin of the approval team.
- **Per-team channels** (`mattermost-channel-requests` in other teams, when **Per-team approval channels** is on): created in each team that has a Team Admin or a System Admin member, and seeded with those people. A user who joins the team later as an admin is added when they join; someone promoted to Team Admin in Mattermost itself (not through a Request Hub request) is added the next time they join the team. Existing per-team channels aren't reseeded on activation. In the approval team, the per-team channel and the root channel are the same channel.

People who aren't members of a channel can't see its cards. The requester gets their own copy of the card, without buttons, as the root of a DM thread with the bot.

### Where each card goes

- **Channel and Channel Admin:** the per-team channel of the request's team. If that team has no Team Admin or System Admin, the card goes to the root channel labeled `For Team: …`. With **Per-team approval channels** off, every card goes to the root channel.
- **Team, Team Admin, team member and guest:** `mattermost-team-requests`.
- **Bot:** `mattermost-bot-requests`. **Webhook:** `mattermost-webhook-requests`.
- **No channel available** (for example, **Approval team** is blank, missing or archived): the card is DM'd to every System Admin instead, and the message relay isn't available for that request. The requester sees an error only if no System Admin could be reached.

Approve and Deny work only on the cards the plugin posted for that request.

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
| **Channel Admin** | Promotes the nominees to Channel Admin. Nominees must already be channel members when the request is made; anyone who has left by approval is skipped |
| **Team Admin** | Promotes the nominees to Team Admin on that team. Nominees must already be team members. From the Request Hub or More actions the nominee is the requester; the slash command and its form can name other team members |
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

Access tokens are generated at approve-time through a short-lived session for the final approver, which is revoked right after; no credential is stored. The token value is sent to the requester in a direct message that **auto-deletes after 72 hours**. The token itself does not expire unless revoked.

If a token is suspected compromised, revoke it in **System Console → User Management → Bot Accounts → {bot name}** and issue a new one.

---

## Webhook approvals in detail

Webhook requests use the same two-step engine as bots. The final approver's session is used for a single create call — never stored.

**Webhook requests from System Admins get both steps too.** Nobody can approve either step of their own request, so a System Admin's request needs a security approver and a different system approver (or one other System Admin in System Admins only mode). If creation fails after both steps, the card stays pending for retry. If an incoming webhook URL cannot be DM'd to the requester, the hook is deleted.

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
for current members. A category from an earlier version named
`Mattermost: Permissions` or `Mattermost Requests` is renamed to
**Mattermost: Request Hub**.

> Upgrades that already saved `PerTeamApprovalChannels = false` keep that
> value — turn the setting back **on** in System Console if channel requests
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

Channel Requests posted its cards to an approval channel you chose. Request Hub doesn't use that channel: it creates its own private channels in the approval team (see [Approval channels](#approval-channels)). If a channel with one of those names already exists and Request Hub didn't create it, the plugin won't post there and logs an error; rename that channel so the plugin can create its own.

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

Every request is queued for approval, including requests from System Admins.

---

## Related guides

- [Channel Requests — User Guide](CHANNEL_CREATION_GUIDE.md)
- [Channel Admin Requests — User Guide](CHANNEL_ADMIN_GUIDE.md)
- [Team Requests — User Guide](TEAM_REQUEST_GUIDE.md)
- [Team Membership & Guests — User Guide](TEAM_MEMBERSHIP_GUIDE.md)
- [Bot Account Requests — User Guide](BOT_REQUEST_GUIDE.md)
- [Webhook Requests — User Guide](WEBHOOK_REQUEST_GUIDE.md)
