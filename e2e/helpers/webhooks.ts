// Helpers for webhook request specs: target channels, the submit routes,
// the request's KV state and the hooks Mattermost holds.
import type {IncomingWebhook, OutgoingWebhook} from '@mattermost/types/integrations';
import type {Post} from '@mattermost/types/posts';

import {expect} from './fixtures';
import {ADMIN, HUB_BOT, PLUGIN_ID, REQUEST_CHANNELS, uid, type RequestHub, type TestUser} from './requesthub';

export type SubmitResult = {message?: string; error?: string};

export type WebhookRecord = {
    id: string;
    requester_id: string;
    webhook_type: 'incoming' | 'outgoing';
    channel_id: string;
    team_id: string;
    display_name: string;
    description: string;
    callback_url?: string;
    approval_post_id?: string;
    dm_root_post_id?: string;
    dm_channel_id?: string;
    security_approver_id?: string;
    system_approver_id?: string;
};

export type Attachment = {
    title?: string;
    text?: string;
    footer?: string;
    fields?: Array<{title: string; value: string}>;
    actions?: Array<{id: string}>;
};

export const attachmentOf = (post: Post): Attachment | undefined => (post.props?.attachments as Attachment[] | undefined)?.[0];

// Text with the plugin's markdown escaping undone (backslashes, and the
// zero-width space it puts in "://"), to compare with what was submitted.
export const plain = (s: string | undefined) => (s ?? '').replace(/\\/g, '').replace(/\u200b/g, '');

export const fieldOf = (post: Post, title: string) => attachmentOf(post)?.fields?.find((f) => f.title === title)?.value;

export const AWAITING_BOTH = '🔒 Security: ⏳ awaiting approval\n🛠 System: ⏳ awaiting approval';

// A new channel in team "test" with members added. Its name is also its
// display name, so cards and messages can be matched by it.
export async function createChannel(hub: RequestHub, type: 'O' | 'P', members: TestUser[] = []) {
    const admin = await hub.client(ADMIN);
    const name = uid('wh');
    const channel = await admin.createChannel({team_id: hub.teamId, name, display_name: name, type} as Parameters<typeof admin.createChannel>[0]);
    for (const m of members) {
        await admin.addToChannel(m.id, channel.id);
    }
    return channel;
}

export const submitIncoming = (hub: RequestHub, user: TestUser, body: {channel_id: string; display_name: string; description?: string}) =>
    hub.pluginFetch<SubmitResult>(user.username, '/api/v1/submit_incoming_webhook_request', body);

export const submitOutgoing = (hub: RequestHub, user: TestUser, body: {channel_id: string; display_name: string; description?: string; callback_url?: string}) =>
    hub.pluginFetch<SubmitResult>(user.username, '/api/v1/submit_outgoing_webhook_request', body);

// The requester's stored webhook requests (record keys), read from Postgres.
export async function webhookRecordKeys(hub: RequestHub, requesterId: string): Promise<string[]> {
    const out = await hub.mm.psql(
        `SELECT pkey FROM pluginkeyvaluestore WHERE pluginid = '${PLUGIN_ID}' AND pkey LIKE 'webhook\\_request\\_%' ` +
        `AND convert_from(pvalue, 'UTF8')::jsonb->>'requester_id' = '${requesterId}' ORDER BY pkey`,
    );
    return out === '' ? [] : out.split('\n');
}

export const webhookRecord = (hub: RequestHub, requestId: string) => hub.kv<WebhookRecord>(`webhook_request_${requestId}`);

export async function pendingIds(hub: RequestHub, requesterId: string): Promise<string[]> {
    const list = await hub.kv<Array<{id: string}>>(`pending_webhook_${requesterId}`);
    return (list ?? []).map((e) => e.id);
}

// Hooks in team "test" with displayName.
export async function hooksNamed(hub: RequestHub, displayName: string) {
    const admin = await hub.client(ADMIN);
    const incoming = (await admin.getIncomingWebhooks(hub.teamId, 0, 200) as IncomingWebhook[]).filter((h) => h.display_name === displayName);
    const outgoing = (await admin.getOutgoingWebhooks('', hub.teamId, 0, 200)).filter((h: OutgoingWebhook) => h.display_name === displayName);
    return {incoming, outgoing};
}

// Posts in a channel mentioning needle, read now: a refused submit answers
// only after it would have posted its card, so there is nothing to wait for.
export async function postsMentioning(hub: RequestHub, channelId: string, needle: string): Promise<Post[]> {
    const admin = await hub.client(ADMIN);
    const list = await admin.getPosts(channelId, 0, 100);
    return list.order.map((id) => list.posts[id]).filter((p) => JSON.stringify(p).replace(/\\\\/g, '').replace(/​/g, '').includes(needle));
}

// Asserts a refused submit left nothing behind: no card, no ticket DM, no
// request record, no pending entry and no hook.
export async function expectNothingQueued(hub: RequestHub, requester: TestUser, displayName: string) {
    expect(await postsMentioning(hub, await hub.channelId(REQUEST_CHANNELS.webhook), displayName), 'approval card').toEqual([]);
    const c = await hub.client(requester.username);
    const bot = await c.getUserByUsername(HUB_BOT);
    const dm = await c.createDirectChannel([requester.id, bot.id]);
    expect((await c.getPosts(dm.id, 0, 20)).order, 'ticket DM').toEqual([]);
    expect(await webhookRecordKeys(hub, requester.id), 'request records').toEqual([]);
    expect(await hub.kv(`pending_webhook_${requester.id}`), 'pending list').toBeUndefined();
    const hooks = await hooksNamed(hub, displayName);
    expect([...hooks.incoming, ...hooks.outgoing], 'hooks').toEqual([]);
}

// Asserts a request whose approval was refused is still pending as it was:
// record with no step filled, pending entry, card with its buttons, no hook.
export async function expectStillPending(hub: RequestHub, requesterId: string, requestId: string, card: Post) {
    const record = await webhookRecord(hub, requestId);
    expect(record, 'request record').toBeTruthy();
    expect(record!.security_approver_id ?? '').toBe('');
    expect(record!.system_approver_id ?? '').toBe('');
    expect(await pendingIds(hub, requesterId)).toContain(requestId);
    const now = await (await hub.client(ADMIN)).getPost(card.id);
    expect(attachmentOf(now)?.actions?.map((a) => a.id)).toEqual(['approve', 'deny']);
    expect(fieldOf(now, 'Approvals')).toBe(AWAITING_BOTH);
    const hooks = await hooksNamed(hub, record!.display_name);
    expect([...hooks.incoming, ...hooks.outgoing], 'hooks').toEqual([]);
}
