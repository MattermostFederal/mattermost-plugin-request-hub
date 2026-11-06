# SOP — After Request Hub

**Status:** Working draft  
**Scope:** New integration bots (and related requests) on a Mattermost server running Request Hub.  
**Detail / backlog:** [03-implementation-progress.md](./03-implementation-progress.md)

---

## Mattermost controls (unchanged platform)

- Still owns bot identity, tokens, webhooks, membership APIs, and roles.
- Request Hub uses those APIs; it does not replace Mattermost auth.

---

## Request Hub controls (platform add-on)

> **Note:** Without Request Hub, a bot request is just a user asking a System Admin — details stay in informal conversation and are not recorded formally. This **bot request / declaration form** will require purpose, approvals, ARB linkage, and inventory fields are captured and extractable.

- **Gate create:** bot (and related) requests go through the plugin, not ad hoc System Console (except break-glass).
- **Two-step approve:** Security pool, then a distinct System Admin.
- **Terms & Conditions:** requester accepts; acceptance + text hash recorded.
- **Declaration:** purpose, home team/channel, hosting / script location, integration endpoint, optional architecture link.
- **Non-admin bots:** creates regular member bots only (not Team/System Admin).
- **Evidence:** request history, inventory / forensics exports, who requested / approved / owns.
- **Token create path:** lifetime, delivery DM, reminders (full rotate-as-request may still be rolling out — see 03).

---

## Devs / owners control

- Accurate purpose, endpoint, hosting, and home on the request.
- Accept T&Cs and store secrets immediately in an approved vault.
- Keep apps/CI updated on rotate; plugin cannot see downstream secret stores.
- Audit usage of the token (app/script behavior, call sites, and periodic review of that usage — Request Hub does not replace this).
- Do not expand team/channel footprint outside approved process.
- Request changes (or Security tickets until change-requests exist) when purpose, owner, placement, or auth changes.
- Retire bots that are no longer needed.

---

## Security responsibility

- Own T&Cs / compliance statement text in System Console.
- Staff the Security approver pool; review declarations on the security step.
- Define when ARB is required; use architecture link / purpose as intake.
- Use inventory + history exports as the evidence pack (not ad hoc screenshots alone).
- Offboarding: find bots by owner, **transfer or retire**, prefer **rotate** so the leaver’s token copies die (Mattermost will not auto-kill tokens when the human leaves).
- Break-glass: SysAdmin emergency creates still need a Security follow-up.

---

## One-line summary

Request Hub enforces request → Security → SysAdmin → create with a paper trail; developers still own runtime secrets, app behavior, and auditing of token usage; Security owns policy text, the security review step, and offboarding accountability.
