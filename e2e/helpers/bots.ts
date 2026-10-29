// Helpers for bot account requests: submitting them, building the approver
// pools of each two-step mode (Custom Profile Attributes and synced groups),
// and reading what a refused card click told the clicker.
import type {Client4} from '@mattermost/client';
import type {Post} from '@mattermost/types/posts';

import {ADMIN, PLUGIN_ID, REQUEST_CHANNELS, uid, type RequestHub, type TestUser} from './requesthub';

export const SUBMIT_BOT_ROUTE = '/api/v1/submit_bot_request';

export type BotRequestBody = {
    username?: string;
    display_name?: string;
    description?: string;
    bot_owner?: string;
    request_token?: boolean;
    incoming_webhook_channel_id?: string;
    incoming_webhook_display_name?: string;
    outgoing_webhook_channel_id?: string;
    outgoing_webhook_display_name?: string;
    outgoing_webhook_callback_url?: string;
};

export type SubmitReply = {message?: string; error?: string};

// The stored request (bot_request_<id>) and its entry in the requester's
// pending list.
export type StoredBotRequest = {
    id: string;
    requester_id: string;
    username: string;
    owner_user_id?: string;
    request_token?: boolean;
    security_approver_id?: string;
    system_approver_id?: string;
};

export const submitBot = (hub: RequestHub, username: string, body: BotRequestBody) =>
    hub.pluginFetch<SubmitReply>(username, SUBMIT_BOT_ROUTE, {display_name: 'E2E Bot', ...body});

// A valid bot username that is unique on the server (3-22 characters).
export const botName = () => uid('b').slice(0, 22);

export async function storedBotRequest(hub: RequestHub, requestId: string) {
    return hub.kv<StoredBotRequest>(`bot_request_${requestId}`);
}

export async function pendingBotIds(hub: RequestHub, userId: string): Promise<string[]> {
    return ((await hub.kv<Array<{id: string}>>(`pending_bot_${userId}`)) ?? []).map((e) => e.id);
}

// Keys of every stored bot request from requesterId, whatever its username.
export async function botRecordsOf(hub: RequestHub, requesterId: string): Promise<string[]> {
    const out = await hub.mm.psql(
        `SELECT pkey FROM pluginkeyvaluestore WHERE pluginid = '${PLUGIN_ID}' AND pkey LIKE 'bot\\_request\\_%' ` +
        `AND convert_from(pvalue, 'UTF8')::jsonb->>'requester_id' = '${requesterId}'`,
    );
    return out === '' ? [] : out.split('\n');
}

// Posts among the newest 100 in channelId that mention needle. Submitting
// posts the card and the ticket DM before it returns, so a refused submit
// can be checked straight away instead of waiting for a post that never
// comes.
async function postsMentioning(client: Client4, channelId: string, needle: string): Promise<Post[]> {
    const list = await client.getPosts(channelId, 0, 100);
    return list.order.map((id) => list.posts[id]).filter((p) => JSON.stringify(p).includes(needle));
}

export async function botCardsMentioning(hub: RequestHub, needle: string) {
    return postsMentioning(await hub.client(ADMIN), await hub.channelId(REQUEST_CHANNELS.bot), needle);
}

export async function dmsMentioning(hub: RequestHub, username: string, needle: string) {
    const c = await hub.client(username);
    const bot = await c.getUserByUsername('mattermost-request-hub');
    const dm = await c.createDirectChannel([(await c.getMe()).id, bot.id]);
    return postsMentioning(c, dm.id, needle);
}

// The user with this username, or undefined when there is none.
export async function userNamed(hub: RequestHub, username: string) {
    return (await hub.client(ADMIN)).getUserByUsername(username).catch(() => undefined);
}

type Attachment = {title?: string; text?: string; fields?: Array<{title: string; value: string}>; actions?: Array<{id: string}>};

export const attachmentOf = (post: Post) => (post.props?.attachments as Attachment[] | undefined)?.[0];
export const actionIds = (post: Post) => (attachmentOf(post)?.actions ?? []).map((a) => a.id);
export const fieldValue = (post: Post, title: string) => attachmentOf(post)?.fields?.find((f) => f.title === title)?.value;

// The bot approval channel is private: approvers need to be members to click
// its buttons. The plugin adds pool members in the background; adding them
// here keeps the test from waiting on that. When the plugin adds the same
// user at the same moment, one of the two inserts fails; either way the
// user ends up a member.
export async function joinBotChannel(hub: RequestHub, ...users: TestUser[]) {
    const admin = await hub.client(ADMIN);
    const channelId = await hub.channelId(REQUEST_CHANNELS.bot);
    for (const u of users) {
        await admin.addToChannel(u.id, channelId).catch(async (error) => {
            await admin.getChannelMember(channelId, u.id).catch(() => {
                throw error;
            });
        });
    }
}

// Creates a Custom Profile Attribute field. Admin-managed fields can mark
// approvers; user-writable ones must not.
export async function createAttribute(hub: RequestHub, name: string, managed = true): Promise<string> {
    const admin = await hub.client(ADMIN);
    const field = await admin.createCustomProfileAttributeField({name, type: 'text', attrs: managed ? {managed: 'admin'} : {}} as Parameters<Client4['createCustomProfileAttributeField']>[0]);
    return field.id;
}

export async function setAttribute(hub: RequestHub, userId: string, fieldId: string, value: string) {
    await (await hub.client(ADMIN)).updateUserCustomProfileAttributesValues(userId, {[fieldId]: value});
}

// Inserts a group straight into the database, as LDAP or a group-sync plugin
// would. Entry servers can't create custom groups, and the plugin refuses
// them as approver pools anyway. Returns the group's name.
export async function createGroup(hub: RequestHub, members: TestUser[], source = 'plugin_e2e'): Promise<string> {
    const name = uid('grp');
    const id = uid('g').padEnd(26, '0').slice(0, 26);
    const now = Date.now();
    await hub.mm.psql(
        'INSERT INTO usergroups (id, name, displayname, description, source, remoteid, createat, updateat, deleteat, allowreference) ' +
        `VALUES ('${id}', '${name}', '${name}', 'e2e', '${source}', ${source === 'custom' ? 'NULL' : `'${id}'`}, ${now}, ${now}, 0, true)`,
    );
    for (const m of members) {
        await hub.mm.psql(`INSERT INTO groupmembers (groupid, userid, createat, deleteat) VALUES ('${id}', '${m.id}', ${now}, 0)`);
    }
    return name;
}

// Collects the ephemeral posts the server sends a user over the websocket.
// A refused card click answers only the clicker, this way.
export type Ephemerals = {
    next: (needle: string) => Promise<string>;
    close: () => void;
};

export async function listenForEphemerals(hub: RequestHub, username: string): Promise<Ephemerals> {
    const client = await hub.client(username);
    const ws = new WebSocket(`${hub.mm.url().replace(/^http/, 'ws')}/api/v4/websocket`);
    const messages: string[] = [];
    ws.addEventListener('message', (event) => {
        const msg = JSON.parse(String(event.data));
        if (msg.event === 'ephemeral_message') {
            messages.push((JSON.parse(msg.data.post) as Post).message);
        }
    });
    await new Promise<void>((resolve, reject) => {
        ws.addEventListener('open', () => resolve());
        ws.addEventListener('error', () => reject(new Error('websocket failed to open')));
    });
    const authenticated = new Promise<void>((resolve) => {
        ws.addEventListener('message', (event) => {
            const msg = JSON.parse(String(event.data));
            if (msg.seq_reply === 1 || msg.event === 'hello') {
                resolve();
            }
        });
    });
    ws.send(JSON.stringify({seq: 1, action: 'authentication_challenge', data: {token: client.getToken()}}));
    await authenticated;

    const next = async (needle: string) => {
        const deadline = Date.now() + 10_000;
        while (Date.now() < deadline) {
            const found = messages.find((m) => m.includes(needle));
            if (found !== undefined) {
                return found;
            }
            await new Promise((resolve) => setTimeout(resolve, 100));
        }
        throw new Error(`no ephemeral post mentioning "${needle}" for @${username}; got: ${JSON.stringify(messages)}`);
    };
    return {next, close: () => ws.close()};
}
