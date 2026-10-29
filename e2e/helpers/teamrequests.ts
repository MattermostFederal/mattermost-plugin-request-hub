// Helpers for the team, Team Admin, team member and team guest request
// specs: submit routes, the decision routes behind the card buttons, and
// checks that a refused request left nothing behind.
import type {Post} from '@mattermost/types/posts';

import {expect} from './fixtures';
import {ADMIN, HUB_BOT, PLUGIN_ID, REQUEST_CHANNELS, REQUEST_KINDS, type RequestHub, type TestUser} from './requesthub';

export type TeamKind = 'team' | 'team_admin' | 'team_member' | 'team_guest';

export const SUBMIT_ROUTES: Record<TeamKind, string> = {
    team: '/api/v1/submit_team_creation',
    team_admin: '/api/v1/submit_team_admin_request',
    team_member: '/api/v1/submit_team_member_request',
    team_guest: '/api/v1/submit_team_guest_request',
};

export const DISABLED_MSG: Record<TeamKind, string> = {
    team: 'Team requests are currently disabled by an administrator.',
    team_admin: 'Team Admin requests are currently disabled by an administrator.',
    team_member: 'Team member requests are currently disabled by an administrator.',
    team_guest: 'Team guest requests are currently disabled by an administrator.',
};

// Replies from server/http.go, approval_checks.go and approval_posts.go.
export const ALREADY_HANDLED = 'This request has already been handled.';
export const OWN_REQUEST = "You can't approve your own request. Another approver has to review it.";
export const OWN_NOMINATION = "You can't approve a request that nominates you. Another approver has to review it.";
export const TYPE_OFF = 'Requests of this type are turned off. Deny this request instead.';
export const REQUESTER_INACTIVE = "The requester's account is no longer active. Deny this request instead.";

export type SubmitResult = {message?: string; error?: string};

export const submit = (hub: RequestHub, user: TestUser, kind: TeamKind, body: unknown) =>
    hub.pluginFetch<SubmitResult>(user.username, SUBMIT_ROUTES[kind], body);

export type DecisionReply = {ephemeral_text?: string; update?: Post};

// Calls the route an Approve or Deny button posts to, with the body
// Mattermost sends for a click, so the private reply can be read. (A real
// click through doPostAction only shows that reply over the websocket.)
export const decide = (hub: RequestHub, username: string, kind: TeamKind, card: Post, requestId: string, approve: boolean) =>
    hub.pluginFetch<DecisionReply>(username, `/api/v1/${approve ? 'approve' : 'deny'}_${kind}`, {
        post_id: card.id,
        channel_id: card.channel_id,
        context: {request_id: requestId},
    });

// Undoes the plugin's markdown escaping (a backslash before markdown
// characters, a zero-width space after @ and in ://), as findPost does.
export const plain = (text: string) => text.replace(/\\(?=\S)/g, '').replace(/​/g, '');

// A card's message and attachment text, unescaped, for substring checks.
export const cardText = (post: Post) => {
    const attachments = (post.props?.attachments as Array<{title?: string; text?: string; fields?: Array<{title: string; value: unknown}>}> | undefined) ?? [];
    const parts = attachments.flatMap((a) => [a.title ?? '', a.text ?? '', ...(a.fields ?? []).map((f) => `${f.title}: ${String(f.value)}`)]);
    return plain([post.message, ...parts].join('\n'));
};

const postsMentioning = async (hub: RequestHub, channelId: string, needle: string, asUser = ADMIN) => {
    const list = await (await hub.client(asUser)).getPosts(channelId, 0, 100);
    return list.order.map((id) => list.posts[id]).filter((p) => cardText(p).includes(needle));
};

// Stored requests of kind whose requester is requesterId.
export async function recordsBy(hub: RequestHub, kind: TeamKind, requesterId: string): Promise<string[]> {
    const prefix = REQUEST_KINDS[kind].recordPrefix.replace(/_/g, '\\_');
    const out = await hub.mm.psql(
        `SELECT pkey FROM pluginkeyvaluestore WHERE pluginid = '${PLUGIN_ID}' AND pkey LIKE '${prefix}%' ` +
        `AND convert_from(pvalue, 'UTF8')::jsonb->>'requester_id' = '${requesterId}'`,
    );
    return out === '' ? [] : out.split('\n');
}

export const pendingIds = async (hub: RequestHub, kind: TeamKind, userId: string) =>
    ((await hub.kv<Array<{id: string}>>(`pending_${kind}_${userId}`)) ?? []).map((e) => e.id);

// A refused submit leaves no stored request, no pending entry, no approval
// card and no ticket DM. The submit route answers after posting the card, so
// one look is enough.
export async function expectNoRequest(hub: RequestHub, kind: TeamKind, requester: TestUser) {
    expect(await recordsBy(hub, kind, requester.id), 'stored requests').toEqual([]);
    expect(await hub.kv(`pending_${kind}_${requester.id}`), 'pending list').toBeUndefined();
    const cards = await postsMentioning(hub, await hub.channelId(REQUEST_CHANNELS.team), `@${requester.username}`);
    expect(cards.map((p) => p.id), 'approval cards').toEqual([]);
    const c = await hub.client(requester.username);
    const bot = await c.getUserByUsername(HUB_BOT);
    const dm = await c.createDirectChannel([requester.id, bot.id]);
    expect((await c.getPosts(dm.id, 0, 10)).order, 'ticket DMs').toEqual([]);
}

// After a refused decision the request is exactly as it was: still stored,
// still pending, and its card still has its buttons.
export async function expectStillPending(hub: RequestHub, kind: TeamKind, requester: TestUser, card: Post, requestId: string) {
    expect(await hub.kv(REQUEST_KINDS[kind].recordPrefix + requestId), 'stored request').toBeDefined();
    expect(await pendingIds(hub, kind, requester.id)).toContain(requestId);
    const after = await (await hub.client(ADMIN)).getPost(card.id);
    expect(after.message).toBe(card.message);
    expect(actionIds(after)).toEqual(['approve', 'deny']);
}

export const actionIds = (post: Post) =>
    ((post.props?.attachments as Array<{actions?: Array<{id: string}>}> | undefined) ?? []).flatMap((a) => a.actions ?? []).map((a) => a.id);

// A decided request is gone from the store and the pending list, and its
// card lost its buttons and shows the outcome.
export async function expectResolved(hub: RequestHub, kind: TeamKind, requester: TestUser, card: Post, requestId: string, outcome: string) {
    const admin = await hub.client(ADMIN);
    await expect.poll(async () => plain((await admin.getPost(card.id)).message)).toContain(outcome);
    expect(actionIds(await admin.getPost(card.id))).toEqual([]);
    expect(await hub.kv(REQUEST_KINDS[kind].recordPrefix + requestId), 'stored request').toBeUndefined();
    expect(await pendingIds(hub, kind, requester.id)).not.toContain(requestId);
}

// Team membership as the server reports it: undefined when the user has
// never been on the team or has left it.
export async function teamMember(hub: RequestHub, teamId: string, userId: string) {
    const admin = await hub.client(ADMIN);
    const member = await admin.getTeamMember(teamId, userId).catch(() => undefined);
    return member && member.delete_at === 0 ? member : undefined;
}

// A new bot account. Bot creation is off on the test server, so it is
// switched on just for this.
export async function makeBot(hub: RequestHub, username: string) {
    const admin = await hub.client(ADMIN);
    const before = String((await admin.getConfig()).ServiceSettings.EnableBotAccountCreation);
    await hub.mm.mmctl('config', 'set', 'ServiceSettings.EnableBotAccountCreation', 'true');
    try {
        return await admin.createBot({username, display_name: 'E2E bot'});
    } finally {
        await hub.mm.mmctl('config', 'set', 'ServiceSettings.EnableBotAccountCreation', before);
    }
}

// Runs fn with Mattermost's guest accounts and email invitations switched
// on, then puts both settings back.
export async function withGuestAccounts<T>(hub: RequestHub, fn: () => Promise<T>): Promise<T> {
    const config = await (await hub.client(ADMIN)).getConfig();
    const guests = String(config.GuestAccountsSettings.Enable);
    const invites = String(config.ServiceSettings.EnableEmailInvitations);
    await hub.mm.mmctl('config', 'set', 'GuestAccountsSettings.Enable', 'true');
    await hub.mm.mmctl('config', 'set', 'ServiceSettings.EnableEmailInvitations', 'true');
    try {
        return await fn();
    } finally {
        await hub.mm.mmctl('config', 'set', 'GuestAccountsSettings.Enable', guests);
        await hub.mm.mmctl('config', 'set', 'ServiceSettings.EnableEmailInvitations', invites);
    }
}
