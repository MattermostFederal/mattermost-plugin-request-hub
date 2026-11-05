# Agent review brief — delta vs PR #97 as previously pushed

**Target PR:** https://github.com/MattermostFederal/mattermost-plugin-request-hub/pull/97  
**Branch:** `feat/full-audit-trail`  
**Baseline already on the PR (committed):** through `968a97e` — full audit trail, compliance statement, bot declaration (purpose / privilege / ingestion / env / home), audit export requests, bot inventory (PDF/CSV/JSON), bot forensics, approval-channel adoption, **opt-in** `AllowCustomApproverGroups`, System Console text fields for dual security+system pools.  
**This push:** everything below that baseline — local working-tree work that was never on the PR before. Treat it as a **second product layer** on top of the audit trail, not as a small fixup.

**Ask of the reviewing agent:** Read this file end-to-end, then review the diff against `968a97e` (or against `main...HEAD` with this delta in mind). Challenge intent fit, regressions vs the PR description, config migration, dead settings, security model changes, and test coverage. Do **not** rubber-stamp. Call out what should land in this PR vs what should split.

---

## 1. One-sentence verdict for humans

PR #97 already delivered the audit trail + inventory/forensics + the *first* bot-declaration model. This delta **rewrites the two-step approver System Console UX and security model**, **replaces the bot request form/declaration shape**, **hardens inventory owner export**, and **adds a custom admin panel** — large enough that a reviewer should treat it as a feature addendum, not a polish commit.

---

## 2. What was already in PR #97 (do not re-litigate unless this delta breaks it)

Already committed / described on the PR:

| Area | On PR before this push |
| --- | --- |
| Request history KV + `LogAuditRec` + audit-channel detail levels | Yes |
| Audit history UI + slash `audit` / `audit recent` | Yes |
| Export-as-request (`audit_export`) + histories / inventory / forensics | Yes |
| Bot compliance acceptance (checkbox-style statement, SHA-256) | Yes (simpler statement + inline form checkbox) |
| Bot declaration: purpose, privilege select, data ingestion text, hosting env list from System Console, home = current channel | Yes |
| Bot index + inventory CSV columns including `owner_id` / `owner_username` | Yes (columns existed; values often empty — see §5) |
| Approval channel adoption when SysAdmin/bot created the channel | Yes |
| `AllowCustomApproverGroups` (default **off**) + separate System Approver group/attribute | Yes |
| Dual pools: security **and** system group/attribute settings | Yes |

This delta **keeps** the audit/export machinery and most of the history/forensics paths. It **changes** how bots are declared, how compliance is presented, how two-step pools are configured, and how owner shows up in inventory.

---

## 3. Theme A — Two-step approvers: System Console declutter + model change

### 3.1 What PR #97 had

- `TwoStepApproverMode` dropdown: attribute | usergroup | system_admin.
- Four pool fields: `SecurityApproverAttribute`, `SystemApproverAttribute`, `SecurityApproverGroup`, `SystemApproverGroup`.
- Soft-fallback: blank security pool → System Admins can cover the **security step** so tickets are not stuck during setup.
- System step: System Admin **OR** system pool member.
- `AllowCustomApproverGroups` bool (default false): custom groups refused unless toggled on (with a warn log when a custom group matched but was blocked).

### 3.2 What this delta does

| Change | Detail |
| --- | --- |
| Remove system pool settings | Deleted `SystemApproverAttribute`, `SystemApproverGroup` from `configuration`, `plugin.json`, fingerprints, channel sync, retention checks, member listing. |
| System step = System Admins only | `canApproveStep(stepSystem)` is now literally `user.IsSystemAdmin()`. No attribute/group path for system. |
| Always allow custom groups | Removed `AllowCustomApproverGroups`. `resolveApproverGroup` accepts custom, LDAP, and plugin-synced groups. |
| Required security pool | Blank security group/attribute no longer soft-falls to SysAdmin for security. Blank config is filled with shipped defaults (`security-approvers` / `security_approver`) via `applySecurityPoolDefaults` + getters. |
| Mode-gated custom admin UI | New `webapp/src/TwoStepApproverSettings.tsx` registered in `index.tsx`. Mode / attribute / group settings are `type: "custom"` in `plugin.json`. Attribute picker and group picker only render for the matching mode. Group field accepts free-typed names (custom groups that are not yet listed). |
| Channel access sync | `linkTwoStepGroupsToChannel`, `listTwoStepApproverUserIDs`, `shouldRetainTwoStepApprovalChannelAccess` only consider the **security** pool. System Admins already have access via other paths / admin role. |
| Panic isolation | `go syncTwoStepApproverChannels` and bot-card `reconcileTwoStepApproverChannels` wrapped with `defer recover()` so unit-test teardown / background races do not panic the process. |

### 3.3 Intent (why)

- Crimson / federal operators found the dual-pool console noisy and confusing.
- Production desire: security reviewers from a real pool; final step always a distinct System Admin.
- Custom groups are first-class on Mattermost without LDAP; the old “Allow custom” gate blocked the only workable pool on some servers.
- Soft-empty security pool hid misconfiguration (tickets “worked” with only SysAdmins).

### 3.4 Reviewer must challenge

1. **Breaking change vs PR body.** PR #97 summary still documents `Allow custom approver groups` and system pool fields. This delta contradicts that section. Description must be rewritten if this lands.
2. **Existing deployments** with `SystemApproverGroup` / `SystemApproverAttribute` / `AllowCustomApproverGroups` in KV config: settings become ignored dead keys. Confirm no read path still needs them; confirm upgrade story (silent ignore vs migration note).
3. **Dual-role users.** Old model: security+system pool member could take system step without being SysAdmin. New model: they cannot finish system unless they are SysAdmin. Is that intended everywhere this plugin ships?
4. **Required defaults.** Shipping `security-approvers` / `security_approver` even when the group/attribute does not exist yet means tickets can stall until the pool exists. Confirm that is preferred over soft-fallback.
5. **Custom-group privilege escalation.** Removing the allow-toggle means anyone who can manage that custom group’s membership can make themselves a security approver. Old PR explicitly warned about this. Confirm org policy accepts it, or document the permission dependency.
6. **Admin UI.** Mode-gated custom settings: verify sibling-setting reads (`currentState` vs `config`), save-needed, and that unused fields do not flash incorrectly when switching modes.
7. Tests: `approvers*.go`, `two_step_channel_access*_test.go`, `two_step_actions_test.go`, many stubs that previously set system pool / AllowCustom — review for false greens.

---

## 4. Theme B — Bot request form / declaration rewrite

### 4.1 What PR #97 had

Form / dialog / MCP / slash roughly shared:

- Username, display name, **description**
- Purpose, **privilege level select** (read / post / elevated), **data ingestion textarea**
- **Hosting environment** from configurable `BotHostingEnvironments` (default `mattermost-dev` / `mattermost-prod`)
- Home = **current channel** (read-only on web form)
- Optional owner (dialog / slash / MCP; web form often omitted)
- Compliance: statement shown on the form with a checkbox
- `GET /bot_form` returned compliance + hosting_envs + privilege_levels

### 4.2 What this delta does

| Change | Detail |
| --- | --- |
| Drop free-form description field | Purpose is required and **becomes** the Mattermost bot profile description. |
| Drop privilege picker | New requests always `user_equivalent` / MM role `member_user_only`. Legacy read/post/elevated labels remain for old inventory rows. |
| Drop data-ingestion textarea | Replaced by a fixed channel-access policy note (`defaultChannelAccessNote`). |
| Fixed hosting codes | Radios/select: `ci_cd`, `on_prem_server`, `cloud_or_k8s`, `workstation`, `external_saas`, `other` (+ optional other detail). **Not** admin-editable Mattermost env names. |
| Architecture URL (optional) + Integration endpoint (required) | https-only validation on server and client (`isSecureHTTPSURL`). |
| Home team/channel pickers | Web form loads teams/channels via plugin APIs (`fetchBotHomeTeams` / `fetchBotHomeChannels`), not locked to current channel. |
| Terms & Conditions popup | On submit (when compliance required): nested modal with large scrollable statement; accept there. Built-in default text expanded to a multi-clause T&Cs. |
| Optional architecture confirm | Nested confirm when architecture URL omitted (product UX). |
| No owner field on web form | Owner omitted in `RequestBotModal`; create path still falls back owner → requester. |
| Dialog / slash / MCP updated | Dialog fields and command/MCP payloads aligned to new declaration; tests and e2e helpers adjusted. |
| `bot_form` payload shrunk | Only compliance DTO now (no hosting/privilege lists for the web form). |

### 4.3 Files to read carefully

- `webapp/src/RequestBotModal.tsx` (+ `.pw.tsx`)
- `webapp/src/client.ts` / `client.spec.ts`
- `server/bot_declaration.go` / `_test.go`
- `server/bot_request.go` (dialog + approval attachment copy)
- `server/bot_compliance.go` (default T&Cs + `botFormDTO`)
- `server/http.go`, `command.go`, `mcp_tools.go`, `exercise.go`
- e2e: `e2e/helpers/bots.ts`, `e2e/tests/slash-command.spec.ts`

### 4.4 Reviewer must challenge

1. **Semantic break for inventory filters.** Old inventories used strings like `mattermost-prod`. New bots use `ci_cd` etc. Filters by “environment” mean different things. Migration / docs / export consumers?
2. **`BotHostingEnvironments` is now orphaned.** Still in `plugin.json` and `configuration` with help text about mattermost-dev/prod, but `HostingEnvs()` returns fixed codes and ignores the setting. Either remove the setting or restore configurability — do not leave a lying System Console field.
3. **Privilege / ingestion removal** vs compliance/security narrative in the PR and bot guides. Approvers lose declared privilege/ingestion; policy notes are server-fixed. Confirm Security still gets what they need on the approval card.
4. **https-only URLs.** Confirm edge cases (IDN, uppercase HTTPS, trailing junk, `https://` with empty host) match between TS and Go.
5. **Home picker authZ.** Teams/channels endpoints must only list what the requester can see; joining home channel on approve still needs the same checks as before.
6. **T&Cs UX.** Nested modals + focus trap: a11y and Playwright coverage.
7. **MCP / slash parity.** Agent tools and slash must not still require old fields or skip new required ones.
8. **Owner on web.** Form never sends owner; inventory fix (§5) papered over empty index. Is “owner always requester for web” accepted, or should the picker return?

---

## 5. Theme C — Bot inventory CSV owner_id / owner_username

### 5.1 Bug report that triggered the last fix

“Owner id and username are not tracked in the CSV export.”

### 5.2 Reality on PR #97

- CSV headers **already included** `owner_id`, `owner_username`.
- Index field `OwnerUserID` was written from `req.OwnerUserID` as-is.
- `createBotForRequest` already defaulted Mattermost `Bot.OwnerId` to requester when owner was empty.
- Web form often sent **no** owner → index stored empty → CSV cells empty even though MM bot had an owner.

### 5.3 What this delta fixes

1. **`writeBotIndex`:** if `OwnerUserID` blank, store requester ID (same effective owner as create).
2. **`buildBotInventory`:** if index owner blank, fall back to requester; if `GetBot` returns a non-empty `OwnerId`, prefer that as-of-export (live MM owner).
3. Tests updated + `TestBuildBotInventory_LiveOwnerOverridesIndex`.

### 5.4 Reviewer must challenge

1. Prefer live `OwnerId` over index when they diverge — correct for “as of export,” but changes historical declaration semantics. Document which is authoritative.
2. Existing KV index rows with empty owner: export now fills via fallback / GetBot without re-index. Good; confirm PDF “Owner” line behavior when owner == requester (currently suppressed when equal — fine).
3. No backfill job — only export-time logic. OK?

---

## 6. Theme D — Ancillary / test churn

Expect broad test edits that are **followers** of Themes A–C, not independent features:

- Many `*_test.go` files stubbing two-step mode / security pool / removing AllowCustom and system pool.
- Exercise seeding, webhook request, submit checks, MCP/command args, messaging relay, pending, route auth, etc.
- Component tests for the bot modal heavily rewritten for T&Cs, hosting radios, home pickers.
- Admin console registration tests / Playwright index stubs for custom settings.

Reviewer: skim for accidental behavior changes buried in test helpers (e.g. `newTestPlugin` defaulting `system_admin` mode so empty-security soft-fallback removal does not break everything).

---

## 7. Diff size (this push vs `968a97e`)

Approximate (working tree when authored):

- ~46 modified files + 1 new (`TwoStepApproverSettings.tsx`)
- ~+1400 / −1200 lines
- Heaviest: `RequestBotModal.tsx`, `bot_declaration.go` (+tests), `approvers.go`, `two_step_channel_access.go`, `plugin.json`, `bot_request.go`, `TwoStepApproverSettings.tsx`

**Not included on purpose:** `.coderabbit-reviews/*.jsonl` (local review cache — do not commit).

---

## 8. Suggested review order for an agent

1. Read this document.
2. `git log --oneline origin/main..968a97e` — understand PR #97 baseline.
3. `git diff 968a97e..HEAD` (or this commit range) — focus files in §3–§5.
4. Reconcile **PR description** with reality; list must-fix doc sections.
5. Security model matrix: for each mode × step × (SysAdmin / security member / dual-role / custom group manager), who can click Approve?
6. Bot submit matrix: web / dialog / slash / MCP / exercise — required fields, https rules, compliance.
7. Inventory CSV: empty-owner legacy index, live OwnerId override, username lookup failures.
8. Flag dead `BotHostingEnvironments` setting.
9. Run `go test ./server/…` and webapp unit/component tests if environment allows; note failures.
10. Output: **Approve with nits** / **Request changes** / **Split PR**, with concrete file:line or symbol references.

---

## 9. Explicit questions the human wants answered

1. Should this delta stay on PR #97 or split (approver console vs bot form vs inventory owner fix)?
2. Is “system step = SysAdmin only + custom groups always allowed + required security defaults” the long-term federal product stance?
3. Is replacing configurable Mattermost hosting envs with fixed “where the script runs” codes correct for inventory consumers already using `mattermost-prod`?
4. Any blocker before crimson upload / further QA?

---

## 10. Non-goals of this delta

- Not changing the core history append / export-request / forensics design from PR #97.
- Not adding MinIO or any new object store.
- Not committing unless/until this review path; this document ships **with** the code push so a reviewing agent has an explicit charter.

---

## 11. Quick file map

| Path | Role in delta |
| --- | --- |
| `plugin.json` | Custom two-step settings; removed system pool + AllowCustom; **still has** BotHostingEnvironments |
| `server/configuration.go` | Struct + defaults + fingerprint + recover on sync |
| `server/approvers.go` | canApproveStep / attribute / group semantics |
| `server/two_step_channel_access.go` | Security-only sync/list/retain; custom groups always |
| `webapp/src/TwoStepApproverSettings.tsx` | **New** mode-gated admin UI |
| `webapp/src/index.tsx` | Register custom settings |
| `webapp/src/RequestBotModal.tsx` | Form rewrite + T&Cs + home pickers |
| `webapp/src/client.ts` | Payload / bot_form / home fetch helpers |
| `server/bot_declaration.go` | New declaration validation + index owner default |
| `server/bot_inventory.go` | Export-time owner resolution |
| `server/bot_compliance.go` | Expanded default T&Cs; slim bot_form DTO |
| `server/bot_request.go` | Dialog + attachment + submit wiring |
| `docs/AGENT_REVIEW_DELTA_PR97.md` | This brief |

---

*End of brief. Prefer harsh, specific findings over summary praise.*
