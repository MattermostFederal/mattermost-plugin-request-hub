# Request Hub — Implementation progress

**Status:** Living engineering doc — update Progress as work lands  
**Last updated:** 2026-11-05 (user-group creation note)  
**Related:** short SOPs [01](./01-sop-before-request-hub.md) / [02](./02-sop-after-request-hub.md)

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
| Bot create + T&Cs + declaration (purpose, https endpoint, hosting, home) | `Partial` | On feat/full-audit-trail; polish continues |
| Two-step Security then SysAdmin; custom security groups | `Partial` | Model simplified on current branch |
| Inventory / forensics export for RH-managed bots | `Partial` | Owner columns fixed in working tree; CSV vs Security spreadsheet gaps remain |
| **System Admins can receive / download exports with no approval** when they want | `Done` | Setting **Allow direct audit export** (default **off**). Off = every export is a ticketed request another SysAdmin must approve. On = SysAdmins can pull inventory / histories / forensics straight from Audit history, `audit export`, and the export API with no second approval. Keep off in high-control envs; turn on when SysAdmins want self-serve without the ticket path. |
| Inventory includes non–RH integration bots (`rh_managed=false`) | `Not started` | Exclude plugin bots (Request Hub bot, Boards, etc.) |
| Adopt / register legacy bot into index | `Not started` | First step before change-request on kind B |
| Change request: prefill latest state; ticket = Changes + Unchanged | `Not started` | Purpose, endpoint, hosting, membership, owner, auth |
| Token rotate as request (wired to reminders) | `Not started` | Mattermost-side lifecycle; owner still updates vault/CI |
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
| Integration endpoint on CSV | `Partial` | On declaration/index; confirm CSV/PDF columns |
| Auth method column | `Not started` | Token / webhook flags |
| Transit security / activity-logs flags | `Not started` | Mostly policy defaults today |
| Full team/channel footprint (not only home) | `Partial` | Forensics per bot; not fleet CSV |

---

## Nice-to-haves (phased)

### Phase 1: Baseline

| Item | Progress | Notes |
| --- | --- | --- |
| Bot auto-joins channels (plugin requirement) | `Partial` | Home channel on approve today; broader set TBD |
| Users auto-join channels, including admins and security-group members promoted after channels/accounts existed | `Partial` | Two-step channel sync/reconcile exists; verify “promoted later” cases |
| **Primary-team Team Admins auto-join all Request Hub approval channels for visibility** (including bot + webhook + audit), even when they cannot approve | `Not started` | **Today:** SysAdmins are seeded into every named approval channel; security-pool members into bot/webhook only; approval-team Team Admins get root `channel-requests` (and `team-requests` only if that approve toggle is on) — **not** bot/webhook/audit. Eviction keeps a Team Admin if they were already in bot/webhook, but nothing seeds them for watch-only. **Wanted:** membership for visibility on all **primary approval team** ticket channels; re-seed when someone becomes Team Admin later or when a channel is created after they already are one. Approve rights stay gated by existing toggles / two-step rules. **Do not** auto-join them into a **new team's** channels (per-team `channel-requests`, or any channel on a team created via Request Hub) unless they are **already a member of that team**. Visibility seeding is scoped to the primary approval team only. |
| Team and Channel request channels created at setup, not on first request | `Not started` / `Needs decision` | Confirm current behavior on crimson |
| Channel request templates so channel exists up front; users added on first request | `Not started` | |
| Prevent duplicate Channel Requests channel | `Not started` | See Open Questions |
| Rename to `Mattermost: Channel Requests` + managed bookmark | `Not started` | |
| Managed category always auto-adds new users | `Not started` | |
| Team Admin create — confirm step after approval (same team vs new team) | `Needs decision` | Options: post-approve “Are you sure?” modal; second SysAdmin click; or treat as already covered by two-step. New-team path may need a stronger confirm than promote-on-existing-team. Pick one UX and document in 02 once decided |

### Phase 2: Request polish

| Item | Progress | Notes |
| --- | --- | --- |
| Requesters can delete / withdraw their own requests | `Not started` | |
| Define delete when approvers already acted | `Needs decision` | Withdraw vs deny vs audit-only close |
| Remove webhook option from bot requests (bots already support IW/OW) | `Needs decision` | Conflicts with current bot form extras — product call |
| Who owns webhooks: plugin bot vs requesting user | `Needs decision` | |

### Phase 3: New capability

| Item | Progress | Notes |
| --- | --- | --- |
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
| Remove webhooks from bot request form? | Product | Open |
| Webhook owner: plugin bot or requester? | Eng + Security | Open |
| User group create: SysAdmin-only approve vs Security two-step? | Security + Eng | Open |
| User group create: which approval channel (new vs reuse)? | Eng | Open |

---

## Concern → implementation (bots)

| Security concern | Implementation angle | Progress |
| --- | --- | --- |
| Data spillage | T&Cs + purpose; history hash | `Partial` |
| Token rotation | Lifetime + reminders now; rotate request Planned | `Partial` |
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
