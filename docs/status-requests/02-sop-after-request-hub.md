# SOP — After Request Hub

**Scope:** New bot and webhook requests on a Mattermost server running Request Hub.  
**Design / architecture (review):** [04 — manual vs Request Hub pictures](./04-design-architecture-request-hub-bots.md)

---

## Mattermost controls (unchanged platform)

- Still owns bot identity, tokens, webhooks, membership APIs, and roles. Request Hub uses those APIs; it does not replace Mattermost auth.
- Does **not** stop System Admins creating bots in the System Console or through the API.

---

## Request Hub controls (platform add-on)

> **Note:** Without Request Hub, a bot request is just a user asking a System Admin: details stay informal and are not recorded formally. The bot request / declaration form captures purpose, approvals, ARB linkage, and inventory fields.

- **Gate create:** bot (and related) requests go through the plugin, not ad hoc System Console (except break-glass).
- **Two-step approve:** a security approver, then a **different** System Admin. Order enforced; requesters can't approve their own.
- **Terms & Conditions:** requester accepts; the text, its SHA-256 and the time are recorded.
- **Declaration:** purpose, home team/channel, hosting / script location, integration endpoint, optional architecture link.
- **Non-admin bots:** regular members only. On approval the bot joins the home team and home channel (team defaults like Town Square / Off-Topic come with team join).
- **Tokens:** expire after the configured lifetime (1 to 12 months); expiry reminders; delivery message in the requester DM auto-deletes after ~72 hours.
- **Evidence:** every request event goes to the request history and the server audit log. Exports (histories, bot inventory, bot forensics) are requests another System Admin approves.

---

## Devs / owners control

Request Hub records the declaration; owners still run the integration.

- Put an accurate purpose, integration endpoint, where the service runs, and optional architecture link on the request (why it exists, which systems call it).
- Accept the T&Cs. Store tokens and webhook URLs in an approved vault or CI secrets, not in Mattermost. Save them before the delivery DM is deleted (~72 hours).
- Own app or script behaviour: posts, DMs, external HTTP, and any data moved out of channels. Request Hub does not see API calls made with the token.
- Keep the bot in the approved home channel unless a further add is approved. Request Hub does not block or detect extra team/channel membership.
- Rotate or revoke on expiry, when people leave, or if secrets may have leaked. Update apps and CI when a token is replaced (RH cannot see downstream stores). Until rotation is a request type, raise a Security ticket for a new token.
- Audit use of the token (call sites, behaviour, periodic review). Use forensics / server audit / SIEM for evidence; RH history is not a substitute for reviewing the app.
- Request changes (or a Security ticket until change-requests exist) when purpose, owner, placement, or auth changes.
- Retire or disable bots that are no longer needed.

---

## Security responsibility

- Approve the T&Cs text and any change to the settings above; System Admins enter them in the System Console.
- Staff the Security approver pool; review declarations on the security step. Define when ARB is required; use architecture link / purpose as intake.
- Use the server audit log in the SIEM as the system of record; Request Hub exports are working copies.
- Offboarding: find the leaver's bots in the inventory, **transfer or retire**, and **rotate** tokens (manual System Admin steps today).


---

## One-line summary

Request Hub enforces request → Security → SysAdmin → create with a paper trail; developers still own runtime secrets, app behavior, and auditing of token usage; Security owns policy text, the security review step, and offboarding accountability.
