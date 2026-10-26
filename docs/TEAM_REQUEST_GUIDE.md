# Team Requests — User Guide

Mattermost Request Hub lets members **request a new team** (and, separately,
**request Team Admin** on a team they already belong to) instead of creating or
promoting themselves directly. Requests go to System Admins. Team Admin requests
also go to the Team Admins of that team, and team creation requests can go to the
approval team's Team Admins (see [Admin setup](#admin-setup)). On approval the team
is created (or the nominees are promoted) and the requester is notified by DM.

> Companion guides: [new channels](CHANNEL_CREATION_GUIDE.md) ·
> [Channel Admin](CHANNEL_ADMIN_GUIDE.md) ·
> [team membership & guests](TEAM_MEMBERSHIP_GUIDE.md) ·
> [bots](BOT_REQUEST_GUIDE.md) · [webhooks](WEBHOOK_REQUEST_GUIDE.md).
>
> In Mattermost: **`/mattermost-request-hub docs`** opens the full HTML guide.

This guide covers:

- [Requesting a new team](#requesting-a-new-team) — any member
- [Requesting Team Admin](#requesting-team-admin) — members who are not already Team Admin
- [Messaging while a request is pending](#messaging-while-a-request-is-pending) — two-way threads
- [Approving requests](#approving-requests) — System Admins and Team Admins
- [Admin setup](#admin-setup) — system admins

---

## Requesting a new team

Same **Request a New Team** form from every entry point.

### How to open

- **Native:** team sidebar → **Request a Team** (relabeled Create a team for non-admins; System Admins still see native Create a team)
- **Team rail +:** the + under teams opens Request a Team for non–System Admins (join-other-teams + is unchanged)
- **Dropdown:** channel More actions → **Request a Team**
- **Hub:** right App Bar **plug** → Requests → **Request a Team**
- **Slash:** `/mattermost-request-hub team` (or skip the form with
  `/mattermost-request-hub team <Team name> [options]`)
- **Agent:** when **Offer tools to Agents** is on, ask an agent from the
  Mattermost Agents plugin to file it for you (the `request_team` tool; see the
  [admin guide](admin-guide.md#agents-tools))

<p>
<img src="images/team-request/mattermost-team-native-menu.png" alt="Team sidebar" width="240">
<img src="images/team-request/mattermost-team-more-actions.png" alt="More actions" width="240">
<img src="images/shared/mattermost-requests-plug-appbar.png" alt="Right App Bar (plug)" width="56">
<img src="images/team-request/mattermost-team-hub.png" alt="Request Hub" width="440">
</p>

### Form

<img src="images/team-request/mattermost-team-modal.png" alt="Request a New Team form" width="480">

| Field | What it does |
|-------|--------------|
| **Team name** | Display name people see (e.g. *Marketing*). Required. |
| **Team URL** | Optional. Leave blank to auto-generate from the team name. A live **Preview** shows the final URL. |
| **Description** | Optional short description of the team's purpose. |
| **Visibility** | **Open** (anyone can join) or **Invite-only** (members must be invited). |
| **I want to be Team Admin of this team** | Optional. When checked, approval also promotes you to Team Admin on the new team. |

Click **Submit request**. You'll see a confirmation, and you'll get a direct
message when a System Admin responds.

---

## Requesting Team Admin

Use this when you already belong to a team and need Team Admin rights (manage
members, create channels, configure team settings). The option is **hidden if
you're already a Team Admin or System Admin** for that team.

### How to open

- **Native:** team sidebar → **Request Team Admin**
- **Dropdown:** channel More actions → **Request Team Admin**
- **Hub:** right App Bar **plug** → Requests → **Request Team Admin**
- **Slash:** `/mattermost-request-hub team-admin` (or skip the form with
  `/mattermost-request-hub team-admin @user [@user ...]`)
- **Agent:** when **Offer tools to Agents** is on, ask an agent from the
  Mattermost Agents plugin to file it for you (the `request_team_admin` tool; see the
  [admin guide](admin-guide.md#agents-tools))

<p>
<img src="images/team-admin-request/mattermost-team-admin-native.png" alt="Team sidebar" width="240">
<img src="images/team-admin-request/mattermost-team-admin-more-actions.png" alt="More actions" width="240">
<img src="images/shared/mattermost-requests-plug-appbar.png" alt="Right App Bar (plug)" width="56">
</p>

### Form

<img src="images/team-admin-request/mattermost-team-admin-modal.png" alt="Request Team Admin confirmation" width="480">

The form confirms which team you're requesting admin on. Click **Submit request**.
You'll get a DM when a System Admin responds.

---

**Limits.** A System Admin sets how many team requests and how many Team Admin requests you can have waiting for review
at once (each request type is counted separately). At the limit you'll see *you
have N pending requests waiting for review; you can submit more once some are
approved, denied or expire*. Asking again for a team URL you're already waiting on shows *you already have a
pending request for a team named name*; asking again for the same people in the
same team shows *you already have a pending Team Admin request for these people
in this team*. Requests that aren't reviewed within the configured number of days expire, and
you'll get a direct message saying so.

## Messaging while a request is pending

Every request opens a **linked pair of threads**: one in the approval channel
(for reviewers) and one in a DM from the bot (for you). Replies are relayed both
ways.

Use Mattermost's **Thread** panel (right-hand sidebar) — not a new top-level
message. Open the thread from the request card ("Reply" / reply count).

- **Approver:** ask in the approval Thread (e.g. *Why do you need this new team?*)
- **Requester:** see `Message from @admin:` in your DM Thread with the bot
- **Reply:** answer in that DM Thread; the approval Thread shows `Reply from @you:`

<p>
<img src="images/shared/mattermost-thread-approver-ask.png" alt="Approval Thread" width="300">
<img src="images/shared/mattermost-thread-requester-dm.png" alt="Requester DM" width="720">
<img src="images/shared/mattermost-thread-relay-back.png" alt="Relay back" width="300">
</p>

The same pattern works for **Team Admin** requests (and every other request
type). Threads stay open after approve/deny so you can still follow up.

> Tip: If you message the bot outside a ticket thread, it nudges you to reply
> inside the specific request thread instead.

---

## Approving requests

### New team

Team creation requests post to the plugin-managed
**`mattermost-team-requests`** private channel in the approval team.
System Admins see **Approve** and **Deny**. With **Team Admins can approve team
creation** on, Team Admins of the approval team are added to that channel and can
approve or deny too.

<img src="images/team-request/mattermost-team-approval-card.png" alt="Team creation approval card" width="720">

- **Approve** — creates the team, adds the requester, and (if they checked the
  box) promotes them to Team Admin. The requester is notified by DM.
- **Deny** — closes the request and notifies the requester by DM.

Requests from System Admins are queued for approval too. Only a System Admin can
approve a request they made themselves.

### Team Admin

System Admins and the Team Admins of the requested team can approve or deny. A
Team Admin can already promote their own team's members in Mattermost, so this
grants them nothing new. Team Admins of other teams, including the approval
team, can't. Only a System Admin can approve a request they made themselves.

The card posts to the team's own **`mattermost-channel-requests`** channel when
**Per-team approval channels** is on and the team has a Team Admin or System
Admin. Otherwise it posts to **`mattermost-team-requests`** (same channel as team
creation requests).

<img src="images/team-admin-request/mattermost-team-admin-approval-card.png" alt="Team Admin approval card" width="720">

- **Approve**: promotes the nominees to Team Admin on that team and notifies the requester by DM.
  A request from the Request Hub or More actions nominates the requester; the slash
  command and its form can nominate other members of the team. Nominees must already
  be on the team.
- **Deny** — closes the request and notifies the requester by DM.

---

## Admin setup

Configure the plugin in **System Console → Plugins → Mattermost Request Hub**.

| Setting | Purpose |
|---------|---------|
| **Approval team** | Team that owns plugin-managed approval channels. Required. |
| **Allow team requests** | Enables **Request a Team** entry points. Default **on**. |
| **Allow team admin requests** | Enables **Request Team Admin** entry points. Default **on**. |
| **Team Admins can approve team creation** | Default **off**. When on, Team Admins of the approval team can approve or deny team creation requests and are added to **`mattermost-team-requests`**. Useful when System Admins are scarce. Turn it on only if you trust those Team Admins to decide who gets a new team. |

Team creation cards go to **`mattermost-team-requests`**. Team Admin cards go to
the team's own approval channel when there is one, and to
**`mattermost-team-requests`** otherwise.
See the [Admin Guide](admin-guide.md) for the full settings reference.

> To fully funnel non-admins through the request flow, remove the built-in
> **Create Team** permission from roles that shouldn't create teams natively
> (System Console → User Management → Permissions). The plugin relabels the UI
> for non-admins, but it cannot veto the native create-team API.
