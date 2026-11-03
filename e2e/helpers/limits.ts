// Helpers for the pending-limit, duplicate and expiry specs: submitting
// channel and team requests over REST, and counting what a submit left
// behind (cards, ticket DMs, KV records, pending-list entries) without
// polling, so a test can show that a refused request left nothing.
import {ADMIN, APPROVAL_CHANNEL, HUB_BOT, PLUGIN_ID, REQUEST_KINDS, type RequestHub, type RequestKind} from './requesthub';

export type SubmitResult = {message?: string; error?: string};

export const SUBMITTED = 'Your channel request has been submitted for approval. You\'ll be notified once an admin responds.';
export const TEAM_SUBMITTED = 'Your team creation request has been submitted for approval. You\'ll be notified once an approver responds.';

export const limitText = (n: number) => `you have ${n} pending requests waiting for review; you can submit more once some are approved, denied or expire`;
export const duplicateText = (channelName: string) => `you already have a pending request for ~${channelName} in this team`;

// Submits a "proj-" channel request named name, the way the webapp modal does.
export function submitChannel(hub: RequestHub, username: string, name: string): Promise<SubmitResult> {
    return hub.pluginFetch<SubmitResult>(username, '/api/v1/create', {team_id: hub.teamId, display_name: name, prefix: 'proj-', name: '', channel_type: 'O', members: [], admin_members: []});
}

export function submitTeam(hub: RequestHub, username: string, name: string): Promise<SubmitResult> {
    return hub.pluginFetch<SubmitResult>(username, '/api/v1/submit_team_creation', {display_name: name, name, type: 'O', description: 'e2e'});
}

// Same unescaping as the find helpers in requesthub.ts.
const unescaped = (json: string) => json.replace(/\\\\/g, '').replace(/​/g, '');

async function countIn(hub: RequestHub, username: string, channelId: string, needle: string): Promise<number> {
    const list = await (await hub.client(username)).getPosts(channelId, 0, 200);
    return list.order.filter((id) => unescaped(JSON.stringify(list.posts[id])).includes(needle)).length;
}

// Posts mentioning needle in a request channel. Submits post their card
// before they return, so no wait is needed after one.
export async function countCards(hub: RequestHub, needle: string, channelName: string = APPROVAL_CHANNEL): Promise<number> {
    return countIn(hub, ADMIN, await hub.channelId(channelName), needle);
}

// Posts mentioning needle in the user's DM with the Request Hub bot.
export async function countDMs(hub: RequestHub, username: string, needle: string): Promise<number> {
    const c = await hub.client(username);
    const me = await c.getMe();
    const bot = await c.getUserByUsername(HUB_BOT);
    const dm = await c.createDirectChannel([me.id, bot.id]);
    return countIn(hub, username, dm.id, needle);
}

export type StoredRequest = {id: string; requester_id: string; display_name: string; name: string; created_at: number};

// Every stored request of kind submitted by requesterId.
export async function storedRequests(hub: RequestHub, kind: RequestKind, requesterId: string): Promise<StoredRequest[]> {
    const prefix = REQUEST_KINDS[kind].recordPrefix;
    const out = await hub.mm.psql(
        `SELECT convert_from(pvalue, 'UTF8') FROM pluginkeyvaluestore WHERE pluginid = '${PLUGIN_ID}' ` +
        `AND pkey LIKE '${prefix.replace(/_/g, '\\_')}%' AND convert_from(pvalue, 'UTF8')::jsonb->>'requester_id' = '${requesterId}'`,
    );
    return out === '' ? [] : out.split('\n').map((line) => JSON.parse(line) as StoredRequest);
}

// IDs in the user's pending list for kind, in list order.
export async function pendingIds(hub: RequestHub, kind: RequestKind, userId: string): Promise<string[]> {
    const list = await hub.kv<Array<{id: string}>>(`pending_${kind}_${userId}`);
    return (list ?? []).map((e) => e.id);
}

// Deletes a stored request's record but not its pending entry, as if the
// plugin stopped between reserving the entry and storing the request.
export async function deleteRecord(hub: RequestHub, kind: RequestKind, requestId: string) {
    await hub.mm.psql(`DELETE FROM pluginkeyvaluestore WHERE pluginid = '${PLUGIN_ID}' AND pkey = '${REQUEST_KINDS[kind].recordPrefix}${requestId}'`);
}
