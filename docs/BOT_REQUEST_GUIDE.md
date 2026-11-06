# Bot Account Requests: User Guide

Mattermost Request Hub lets members **request a bot account** (optionally
with an access token and incoming/outgoing webhooks in the same ticket) instead
of creating bots directly. Bot requests use **two-step approval** (security +
system). On final approval the bot is created and secrets are delivered by DM
(the DM auto-deletes after 72 hours).

> If you only need a webhook and already have a bot (or don't need one), use the
> [standalone webhook guide](WEBHOOK_REQUEST_GUIDE.md) instead.
>
> Companion guides: [new channels](CHANNEL_CREATION_GUIDE.md) ·
> [Channel Admin](CHANNEL_ADMIN_GUIDE.md) · [teams](TEAM_REQUEST_GUIDE.md) ·
> [webhooks](WEBHOOK_REQUEST_GUIDE.md).
>
> In Mattermost: **`/mattermost-request-hub docs`** opens the full HTML guide.

This guide covers:

- [Requesting a bot](#requesting-a-bot): any member
- [Messaging while a request is pending](#messaging-while-a-request-is-pending): two-way threads
- [Approving requests](#approving-requests): a security approver, then a System Admin
- [Admin setup](#admin-setup): system admins

---

## Requesting a bot

Same **Request a Bot Account** form from every entry point.

### How to open

- **Native:** Main Menu → Integrations → **Bot Accounts** (non–System Admins; System Admins still get native bot management)
- **Dropdown:** channel More actions → **Request a Bot**
- **Hub:** right App Bar **plug** → Requests → **Request a Bot**
- **Slash:** `/mattermost-request-hub bot` (or skip the form with
  `/mattermost-request-hub bot <username> <Display name> --purpose <text> --integration <https-url> --env ci_cd|on_prem_server|cloud_or_k8s|workstation|external_saas|other [--architecture <https-url>] [--hosting-other <text>] [--owner @user] [--accept-compliance]`,
  run in the channel that will be the bot's home; access tokens and webhooks
  for the bot are only in the form)
- **Agent:** when **Offer tools to Agents** is on, ask an agent from the
  Mattermost Agents plugin to file it for you (the `request_bot` tool, which like the
  slash command covers the account only; see the
  [admin guide](admin-guide.md#agents-tools))

<p>
<img src="images/bot-request/mattermost-bot-integrations.png" alt="Integrations" width="720">
<img src="images/bot-request/mattermost-bot-more-actions.png" alt="More actions" width="240">
<img src="images/shared/mattermost-requests-plug-appbar.png" alt="Right App Bar (plug)" width="56">
<img src="images/bot-request/mattermost-bot-hub.png" alt="Request Hub" width="440">
</p>

### Form

<img src="images/bot-request/mattermost-bot-modal.png" alt="Request a Bot Account form" width="480">

#### Required fields

| Field | What it does |
|-------|--------------|
| **Bot username** | Username the bot posts as. Lowercase letters, numbers, periods, underscores, and hyphens. 3–22 characters. Example: `deploy_bot` or `deploy-notifier`. |
| **Display name** | Friendly name shown in messages. Example: `Deploy Notifier`. |
| **Purpose** | Why the bot is needed and what it will do. Up to 500 characters. It also becomes the bot's profile description. |
| **Home team** / **Home channel** | Picked from the teams and channels you belong to. The form starts on the team and channel you're in. |
| **Integration endpoint** | The `https://` address of the integration that drives the bot. `http://` is refused. |
| **Where does the script / service run?** | **CI/CD pipeline**, **On-prem server / VM**, **Cloud VM / Kubernetes**, **Developer workstation**, **External SaaS / vendor** or **Other**. With **Other**, say where in **Specify hosting**. This is where the integration runs, not which Mattermost server the bot is on. |

The form also has an optional **Architecture link** (an `https://` link to the
design or architecture review). Submitting without one asks you to confirm.

The bot is always a regular Mattermost user, never a Team Admin or System
Admin, with member permissions in the channels it is added to. The form shows
this, and it is recorded with the request: there is no privilege level to pick.

#### Home team and channel

When the request is approved, the bot is added to its home team and home
channel, even if you asked for no webhook. The home says where the bot belongs
and who answers for it. It is not a sandbox: an access token can still reach
anything the bot account can, in any channel the bot is added to. Before either
approval step, Approve checks that the home team and channel still exist and
that you are still in them; if not, the request can only be denied.

Security can later list the bots Request Hub created by their user ID, with
what was declared for each. Bots approved before these fields existed have no
declaration. Bots requested before the current form keep the privilege level,
data ingestion and hosting environment declared for them then.

#### Compliance statement

Unless a System Admin has turned it off, **Submit request** opens the Terms and
Conditions. Tick **I have read and agree to the Terms and Conditions above.**
and click **Agree and submit**. By default they say the bot is for authorized
use only, that you are responsible for everything done with it, its tokens and
its webhooks, that you will protect its credentials, and that what you declared
is accurate; your organization may have replaced them with its own wording. The request keeps the
exact text you accepted and when, and both are in its audit history. If the
statement changes while the form is open, submitting asks you to read it again.

#### Optional: Access token

**Request an access token** is checked by default. External systems use this
token to authenticate to Mattermost as the bot.

> The token is sent in a DM when the request is fully approved. That DM
> **auto-deletes after 72 hours**. Save the token in a secrets manager or CI
> vault before it disappears. The token expires after one year by default (an
> admin can set 1 to 12 months); the DM gives the date. From a month before, you
> get a weekly reminder, then a daily one in the last week.

Uncheck this if the bot only receives messages via an incoming webhook and never
calls the API.

#### Optional: Incoming webhook

Check **Request an incoming webhook** to also create a webhook that posts
*into* Mattermost on behalf of this bot.

| Field | What it does |
|-------|--------------|
| **Webhook display name** | Pre-filled with `{username} incoming`. Edit if needed. |
| **Target channel** | The bot's home channel (read-only). Pick another home channel to change it. |

#### Optional: Outgoing webhook

Check **Request an outgoing webhook** to forward messages from a channel to an
external URL (for example, to trigger a pipeline on a keyword).

| Field | What it does |
|-------|--------------|
| **Webhook display name** | Pre-filled with `{username} outgoing`. |
| **Source channel** | The bot's home channel (read-only). |
| **Callback URL** | Required. The `https://` endpoint that receives the payload. |

### Submitting

Click **Submit request**. One approval ticket covers the bot, token, and any
webhooks you checked. The approval card shows that you accepted the compliance
statement. You'll get a DM when both approvers have acted.

---

**Limits.** A System Admin sets how many bot requests you can have waiting for review
at once (each request type is counted separately). At the limit you'll see *you
have N pending requests waiting for review; you can submit more once some are
approved, denied or expire*. Asking again for a bot username you're already waiting on shows *you already
have a pending request for bot @username*. Requests that aren't reviewed within the configured number of days of being
submitted expire, even if one approval step is done, and you'll get a direct
message saying so.

## Messaging while a request is pending

Bot tickets use the same **two-way thread relay** as other request types.
Open the **Thread** RHS from the card (Reply / reply count), not a new root post.

- **Approver:** ask in the approval Thread (e.g. *What systems will this bot call?*)
- **Requester:** see `Message from @admin:` in your DM Thread with the bot
- **Reply:** answer in that DM Thread; the approval Thread shows `Reply from @you:`

<p>
<img src="images/shared/mattermost-thread-approver-ask.png" alt="Approval Thread" width="300">
<img src="images/shared/mattermost-thread-requester-dm.png" alt="Requester DM" width="720">
<img src="images/shared/mattermost-thread-relay-back.png" alt="Relay back" width="300">
</p>

Reply inside the ticket Thread only. A brand-new DM to the bot gets a nudge to
use the specific request thread. Threads remain open after approve/deny (useful
if a token DM needs follow-up).

---

## Approving requests

Bot requests post to **`mattermost-bot-requests`**. They need **two different
people**: a security approver first, then a System Admin for the system step.
The card shows both status lines.

<img src="images/bot-request/mattermost-bot-approval-card.png" alt="Bot approval card" width="720">

Each eligible approver clicks **Approve** once for their step. When both steps
are complete, the plugin:

1. Creates the bot account.
2. Generates an access token if requested.
3. Creates any requested incoming/outgoing webhooks.
4. DMs the requester with results (tokens/URLs auto-delete after 72 hours).

Before either step, Approve checks that the requester and any chosen owner are
still active (the owner can't be a bot), and that any webhook channels still
exist and aren't archived, the requester is still in them (System Admins
excepted), and an outgoing webhook's channel is still public. If not, you'll
get a private message saying so (for example *The bot owner is no longer an
active user. Deny this request instead.*) and the request and its steps stay as
they were, so deny it.

Any eligible approver may **Deny** at any time. That closes the ticket and
notifies the requester.

### DM to the requester

<img src="images/bot-request/mattermost-bot-token-dm.png" alt="Bot token DM" width="720">

The DM includes:

- Bot account confirmation
- Access token (if requested): **message auto-deletes in 72 hours**
- Incoming webhook URL (if requested)
- Outgoing webhook confirmation (if requested)

If something failed, the DM says what couldn't be created and what to do next.

Every bot request goes through both approval steps, including requests from
System Admins. Nobody can approve either step of their own request, so a System
Admin's request needs a security approver and a different System Admin (or
one other System Admin when the approver mode is System Admins only).

---

## Admin setup

| Setting | Purpose |
|---------|---------|
| **Approval team** | Owns `mattermost-bot-requests`. Required. |
| **Allow bot account requests** | Enables bot entry points. Default **on**. |
| **Bot/webhook two-step approver mode** | Who does the security step: holders of a User Attribute (default), members of a user group, or System Admins. The system step is always a System Admin. |
| **Security approver attribute** / **Security approver user group** | The security pool for the chosen mode (defaults `security_approver` and `security-approvers`). System Admins do **not** cover the security step. |
| **Allow custom approver groups** | Off by default. Lets a custom group be the security approver group. |

Create and assign attributes in **System Console → User Management → Attributes**.
See the [Admin Guide](admin-guide.md).

### Token security

Tokens are delivered by DM and not stored by the plugin. If a token is
compromised, rotate it under **System Console → User Management → Bot Accounts**
and revoke the old one.
