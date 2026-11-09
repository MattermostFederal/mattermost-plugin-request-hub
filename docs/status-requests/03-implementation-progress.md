# Request Hub — Implementation progress

**Status:** Living engineering doc — update Progress as work lands  
**Last updated:** 2026-11-06 (Phase 1 baseline; backlog: in-plugin bug report → audit channel)  
**Related:** short SOPs [01](./01-sop-before-request-hub.md) / [02](./02-sop-after-request-hub.md); design/arch [04](./04-design-architecture-request-hub-bots.md)  
**Shared on this branch** so the next person can continue the work. Update Progress as it lands.

Progress values: `Not started` · `In progress` · `Partial` · `Done` · `Won't do` · `Needs decision`

---

## How to use this doc

- This is the **product/implementation** tracker (phases, bot lifecycle roadmap, open questions).
- Do **not** paste this into the short SOPs. SOPs stay “who owns what.”
- When a row hits `Done`, bump the date and, if it changes duties, adjust [02](./02-sop-after-request-hub.md) in one sentence.

---

## Bot lifecycle roadmap (from longer local draft)

Direction: **create is only the first request type.** Existing bots come back through Request Hub for material changes (prefill + diff ticket). Inventory should cover integration bots Request Hub did **not** create, and **exclude** Mattermost plugin/service bots.

| Item | Progress | Notes |
| --- | --- | --- |
| Bot create + T&Cs + declaration (purpose, https endpoint, hosting, home) | `Done` (core) | On `main` via #97; change-request path still Planned |
| Two-step Security then SysAdmin; custom security groups | `Done` | Custom groups via **Allow custom approver groups** (opt-in) |
| Inventory / forensics export for RH-managed bots | `Partial` | Owners fixed; CSV vs Security spreadsheet gaps remain |
| **System Admins can receive / download exports with no approval** when they want | `Done` | Setting **Allow direct audit export** (default **off**) |
| Inventory includes non–RH integration bots (`rh_managed=false`) | `Not started` | Exclude plugin bots (Request Hub bot, Boards, etc.) |
| Adopt / register legacy bot into index | `Not started` | First step before change-request on kind B |
| Change request: prefill latest state; ticket = Changes + Unchanged | `Not started` | Purpose, endpoint, hosting, membership, owner, auth |
| Token rotate as request (wired to reminders) | `Not started` | Lifetime + reminders exist; rotate-as-request does not |
| **Idle bot-token disable (unused ~90 days)** | `Not started` / `Needs design` | If a bot access token has not been used for ~90 days, start a graduated disable path (warn → soft-disable / revoke). See Open questions — Mattermost may not expose last-used; need a workable signal (API audit, RH-side proxy, or time-since-issue as a weaker stand-in). |
| Owner transfer / offboarding (owner leave ≠ token death) | `Not started` | Tokens keep working after human deactivated |
| Membership drift detect (live ≠ approved set) | `Not started` | After change-request exists |
| Tiered approval (light rotate vs full two-step material change) | `Needs decision` | Security input |

### Suggested build order

1. Finish create + inventory + owners  
2. Inventory kind B (non–RH), exclude kind C (plugin bots)  
3. Change request (prefill + diff + apply)  
4. Token rotate + reminders  
5. Owner transfer  
6. Drift detection  

---

## Security extract gaps (inventory columns)

Security wants something like: Bot ID | Name | Team | Channel | Purpose | Auth method | Privilege | Data ingestion | Integration endpoint | Hosting | Transit security | Activity logs.

| Column / need | Progress | Notes |
| --- | --- | --- |
| ID, name, purpose, privilege, hosting, home team/channel, requester/owner/approver, T&Cs hash | `Partial` | In inventory today for RH-managed |
| Integration endpoint on CSV | `Partial` | On declaration/index; not a CSV/PDF column yet |
| Auth method column | `Not started` | Token / webhook flags |
| Transit security / activity-logs flags | `Not started` | Mostly policy defaults today |
| Full team/channel footprint (not only home) | `Partial` | Forensics per bot; not fleet CSV |

---

## Nice-to-haves (phased)

### Phase 1: Baseline — branch `feat/phase1-baseline`

| Item | Progress | Notes |
| --- | --- | --- |
| Bot auto-joins channels (plugin requirement) | `Partial` | Home channel on approve today; broader set TBD (change-request) |
| Users auto-join channels, including admins and security-group members promoted after channels/accounts existed | `Partial` | SysAdmin reseed + two-step reconcile + **primary Team Admin visibility** on login / promote / join / activate |
| **Primary-team Team Admins auto-join all Request Hub approval channels for visibility** (including bot + webhook + audit), even when they cannot approve | `Done` (on branch) | Seed + **evict on demotion** for root/team/audit (`reconcilePrimaryTeamAdminVisibilityForUser`, periodic pass). Bot/webhook demotion stays with two-step reconcile so security-pool members are not removed. Approve rights stay gated. **Does not** auto-join into other teams’ channels. |
| Team and Channel request channels created at setup, not on first request | `Done` (on branch) | Activate ensures all primary ticket channels; **Approval team set/changed** also ensures + Team Admin seed (not waiting for first request) |
| Channel request templates so channel exists up front; users added on first request | `Partial` / `Needs decision` | Channel exists + welcome/bookmark at setup. Clarify whether “templates” means Mattermost channel templates vs orientation posts. Approvers are seeded at setup (not deferred to first request) for visibility. |
| Prevent duplicate Channel Requests channel | `Partial` | Name lookup + adopt/refuse already; root cause of historical duplicate still Open |
| Rename to `Mattermost: Channel Requests` + managed bookmark | `Done` | Display name already that; docs bookmark + managed category migration exist |
| Managed category always auto-adds new users | `Partial` | RH places channel in **Mattermost: Request Hub** whenever it adds a member; Mattermost does not auto-add arbitrary new users into custom categories without our seed path |
| Team Admin create — confirm step after approval (same team vs new team) | `Needs decision` | Hold — not implementing without Security/Product pick |

### Phase 2: Request polish

| Item | Progress | Notes |
| --- | --- | --- |
| Requesters can delete / withdraw their own requests | `Not started` | |
| Define delete when approvers already acted | `Needs decision` | Withdraw vs deny vs audit-only close |
| Remove webhook option from bot requests (bots already support IW/OW) | `Won't do` (for now) | **Hold:** a normal bot cannot manage webhooks unless granted team webhook permissions; for the bot that posts messages that is usually the wrong design. Keep form extras. |
| Who owns webhooks: plugin bot vs requesting user | `Needs decision` | Related to hold above — prefer requester-owned unless Security says otherwise |
| **In-plugin bug / feedback report → audit channel** | `Not started` | **Any user** can file a short bug/feedback report about Request Hub (slash / App Bar / More actions). Form requires **dropdowns of what they selected** (request type, and the key options from that flow — e.g. channel prefix, two-step step, team, etc.) plus free-text description. On submit: post a card into the **audit channel** (not an approval ticket; no approve/deny). Goal: capture real misconfig / UX issues with enough context to reproduce. Confirm whether reports are also written to request history / server audit log. |

### Phase 3: New capability

| Item | Progress | Notes |
| --- | --- | --- |
| **Idle bot-token disable formula (~90 days unused)** | `Not started` / `Needs design` | Goal: stale integration tokens don’t live forever. Sketch to think through: (1) detect idle — prefer real last-use if the platform/audit log can provide it; else weaker proxies (days since issue / last RH reminder ack). (2) notify owner + Security (DM / ticket). (3) grace window. (4) revoke or deactivate token (and optionally the bot) via Request Hub so inventory/history stay consistent. (5) allow “still in use” ack to reset the clock. Pair with rotate-as-request; do **not** surprise-kill production tokens without a warning path. |
| **User group creation requests** (enable via System Console tab/setting) | `Not started` | Mirror channel-creation pattern: toggle to enable; request ticket includes a **who can join** member picker box (same idea as channel creation’s member list on the ticket). On approve: create the custom user group and add the listed members. Confirm approval channel routing + whether Security two-step is required or SysAdmin-only. |
| User group request form fields (name/display name, initial members) | `Not started` | Keep parity with channel request UX where practical |
| OAuth request type (e.g. Claude OAuth 2.0) | `Not started` | |
| OAuth fields (app name, redirect URIs, scopes, owner, justification, …) | `Not started` | Infer + confirm with Security |
| Security review view in plugin (System Admins) | `Not started` | |
| Columns from Chris’s security review spreadsheet as data model | `Not started` | Get spreadsheet → map to export/UI |
| Multi-approver status (Security + SysAdmins) in that view | `Not started` | |

### Phase 4: Exploratory

| Item | Progress | Notes |
| --- | --- | --- |
| Slash: AI PR review e.g. `/claude review <pr-link>` | `Not started` | Scope for v1? |
| ✅ reaction triggers approve/close (request, PR, or Jira) with permission checks | `Not started` | Scope for v1? |

---

## Open questions

| Question | Owner | Status |
| --- | --- | --- |
| Why did the duplicate Channel Requests channel appear? | Eng | Open |
| Which security review fields are mandatory? | Security / Chris | Open |
| Are slash command + ✅ reaction actions in scope for v1? | Product | Open |
| Team Admin: “Are you sure” vs second approver vs current two-step only? | Security + Eng | Open |
| Routine token rotate: lighter approval than material declaration change? | Security | Open |
| Remove webhooks from bot request form? | Product | **Held** — keep webhooks; normal bots shouldn’t manage webhooks without team perms |
| Webhook owner: plugin bot or requester? | Eng + Security | Open |
| User group create: SysAdmin-only approve vs Security two-step? | Security + Eng | Open |
| User group create: which approval channel (new vs reuse)? | Eng | Open |
| What does “channel request templates” mean exactly? | Product + Eng | Open |
| Idle bot tokens (~90 days unused): what signal counts as “used”? | Eng + Security | Open — MM user-access tokens may not expose last-used to plugins; options: server audit/API logs, RH-only heuristic (age since issue), or require integrations to heartbeat |
| Idle bot tokens: warn-only vs auto-revoke; who must ack? | Security + Product | Open — prefer warn → ticket → revoke over silent disable |
| Idle bot tokens: disable token only, or deactivate whole bot? | Security | Open |
| Bug-report form: which dropdowns are mandatory (request type only vs full option set)? | Product + Eng | Open |
| Bug report: audit channel only, or also history / server audit event? | Eng | Open |

---

## Concern → implementation (bots)

| Security concern | Implementation angle | Progress |
| --- | --- | --- |
| Data spillage | T&Cs + purpose; history hash | `Partial` |
| Token rotation | Lifetime + reminders now; rotate request Planned; idle-disable backlog | `Partial` |
| Bots in teams/channels | Home join now; change-request + footprint Planned | `Partial` |
| What bots are used for | Purpose on form (ARB taxonomy = process) | `Partial` |
| Not admin bot | Forced member role on create | `Done` (create path) |
| Non-repudiation | History + export requests | `Partial` |
| Owner leaves | Transfer/retire + rotate (tokens survive owner deactivate) | `Not started` |

---

## Changelog (doc only)

| Date | Change |
| --- | --- |
| 2026-11-05 | Initial progress doc; bot roadmap; phases 1–4; open questions |
| 2026-11-05 | Phase 3: user group creation requests (enable setting + who-can-join box like channel create) |
| 2026-11-05 | Phase 1: note primary-team Team Admin visibility auto-join for all approval channels (bots included) |
| 2026-11-05 | Clarify: do not auto-join primary Team Admins into new/other teams’ channels unless already on that team |
| 2026-11-05 | Note: SysAdmins can auto-receive/download exports with no approval via Allow direct audit export |
| 2026-11-06 | Audit vs `main` @ #97; Phase 1 branch `feat/phase1-baseline`; Team Admin visibility + setup-time channel ensure; webhook removal held |
| 2026-11-06 | Backlog: idle bot-token disable (~90 days unused) — needs design (usage signal, warn→revoke formula) |
| 2026-11-06 | Backlog: in-plugin bug/feedback report for all users → audit channel; required dropdowns of selected options |
| 2026-11-06 | Fix: secondary-team TA demotion now evicts from that team’s `mattermost-channel-requests` (login + ~2m ticker + syncAfterAdminChange) |
