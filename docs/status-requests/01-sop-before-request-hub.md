# SOP — Before Request Hub

**Scope:** Mattermost bot / integration accounts when there is no Request Hub (or bots created outside it).  
**Design / architecture (review):** [04 — manual vs Request Hub pictures](./04-design-architecture-request-hub-bots.md)

---

## Mattermost controls

- Bot account create / disable in System Console (who has permission).
- Bot **owner** field on the bot object (accountability metadata only).
- Access **token** issue and revoke; optional incoming / outgoing webhooks.
- Team and channel membership (bots are not everywhere or anywhere by default).
- Server roles and permissions (a bot is a user-like principal).
- Server audit / admin logs (not bot-purpose or T&Cs aware).

> Without Request Hub, a bot request is just a user asking a System Admin. Details stay in informal conversation and are not recorded formally.

---

## Devs / owners control

- Why the bot exists and which systems call it.
- Where tokens and webhook URLs are stored (vault / CI vars — not Mattermost).
- App/script behavior (posts, DMs, external HTTP, data moved out of channels).
- Which teams/channels the bot is added to after create.
- Rotation and revocation when people leave or secrets leak.
- Auditing usage of the token (what the app/script does with it, where it is called from, and review of that usage).
- Disabling unused bots.

---

## Security responsibility

- Policy: who may create bots; data handling; when ARB is required.
- Manual or ticket-based review of bot creates (no plugin gate).
- Periodic inventory / spot checks via admin tools.
- Incident response (spillage, stolen token): revoke, investigate, require rotate.
- Offboarding: bots owned by a departing user (tokens **keep working** after the human is deactivated — owner field is not the credential).

---

## One-line summary

Without Request Hub, Mattermost provides the account and secret primitives; developers operate the integration and audit token usage; Security relies on process and spot checks, not an enforced request trail.
