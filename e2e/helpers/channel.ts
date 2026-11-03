// Helpers for the channel and Channel Admin request specs: submitting,
// reading the private reply to an approval-card click, and checking that a
// refused request left nothing behind.
import crypto from 'crypto';
import http from 'http';

import type {Post} from '@mattermost/types/posts';

import {expect} from './fixtures';
import {ADMIN, PLUGIN_ID, uid, type RequestHub} from './requesthub';

export type ChannelRequestBody = {
    team_id?: string;
    display_name?: string;
    name?: string;
    prefix?: string;
    purpose?: string;
    channel_type?: string;
    members?: string[];
    admin_members?: string[];
    category?: string;
};

export type SubmitResult = {message?: string; error?: string};

// Posts the webapp modal's payload to /api/v1/create as username.
export function submitChannel(hub: RequestHub, username: string, body: ChannelRequestBody): Promise<SubmitResult> {
    return hub.pluginFetch<SubmitResult>(username, '/api/v1/create', {
        team_id: hub.teamId,
        name: '',
        prefix: 'proj-',
        channel_type: 'O',
        members: [],
        admin_members: [],
        ...body,
    });
}

// Posts the Channel Admin modal's payload to /api/v1/request_admin.
export function submitAdmin(hub: RequestHub, username: string, channelId: string, nominees: string[]): Promise<SubmitResult> {
    return hub.pluginFetch<SubmitResult>(username, '/api/v1/request_admin', {channel_id: channelId, nominees});
}

// Submits the channel request dialog as username, the way the webapp does
// after the slash command opened it. Returns the dialog's error, if any.
export async function submitChannelDialog(hub: RequestHub, username: string, submission: Record<string, string>): Promise<string | undefined> {
    const client = await hub.client(username);
    const townSquare = await client.getChannelByName(hub.teamId, 'town-square');
    const res = await client.submitInteractiveDialog({
        url: `/plugins/${PLUGIN_ID}/api/v1/dialog`,
        callback_id: 'channel_request',
        state: hub.teamId,
        user_id: '',
        channel_id: townSquare.id,
        team_id: hub.teamId,
        submission: {prefix: 'proj-', display_name: '', name: '', purpose: '', type: 'O', category: '', members: '', admins: '', ...submission},
        cancelled: false,
    });
    return (res as {error?: string}).error;
}

// Submits the Channel Admin dialog as username. nominees are user IDs.
export async function submitAdminDialog(hub: RequestHub, username: string, channelId: string, nominees: string[]): Promise<string | undefined> {
    const client = await hub.client(username);

    // Opened from town square: Mattermost refuses a dialog submitted from a
    // channel the user can't read, before the plugin sees it.
    const townSquare = await client.getChannelByName(hub.teamId, 'town-square');
    const res = await client.submitInteractiveDialog({
        url: `/plugins/${PLUGIN_ID}/api/v1/channel_admin_dialog`,
        callback_id: 'channel_admin_request',
        state: '',
        user_id: '',
        channel_id: townSquare.id,
        team_id: hub.teamId,
        submission: {channel_id: channelId, nominees: nominees.join(',')},
        cancelled: false,
    });
    return (res as {error?: string}).error;
}

// Recent posts in a channel (by ID) that mention needle, read as the admin.
// Unlike findCard it doesn't wait: approval cards are posted before the
// submit call returns, so a refused request is checked once.
export async function postsMentioning(hub: RequestHub, channelId: string, needle: string): Promise<Post[]> {
    const admin = await hub.client(ADMIN);
    const list = await admin.getPosts(channelId, 0, 100);
    return list.order.map((id) => list.posts[id]).filter((p) => JSON.stringify(p).replace(/\\\\/g, '').replace(/​/g, '').includes(needle));
}

// Checks that a refused submission left nothing behind: no approval card or
// audit post mentioning needle, no stored request from the user, and an
// empty pending list.
export async function expectNothingStored(hub: RequestHub, kind: 'channel' | 'admin', userId: string, needle: string) {
    expect(await postsMentioning(hub, await hub.approvalChannelId(), needle), 'approval cards').toEqual([]);
    const recordPrefix = kind === 'channel' ? 'request\\_' : 'admin\\_request\\_';
    const records = await hub.mm.psql(
        `SELECT pkey FROM pluginkeyvaluestore WHERE pluginid = '${PLUGIN_ID}' AND pkey LIKE '${recordPrefix}%' ` +
        `AND convert_from(pvalue, 'UTF8') LIKE '%"requester_id":"${userId}"%'`,
    );
    expect(records, 'stored requests').toBe('');
    expect((await hub.kv<unknown[]>(`pending_${kind}_${userId}`)) ?? [], 'pending list').toEqual([]);
}

// The actions still on an approval card ([] once it is resolved).
export function cardActions(card: Post): string[] {
    const attachments = (card.props?.attachments ?? []) as Array<{actions?: Array<{id: string}>}>;
    return attachments.flatMap((a) => (a.actions ?? []).map((x) => x.id));
}

// A minimal WebSocket reader for the Mattermost event stream (Node 20 has
// no WebSocket client). It only reads text frames, which is all it needs:
// the session token goes in the upgrade request, so nothing is sent.
class EventStream {
    private buffer = Buffer.alloc(0);
    private fragments: Buffer[] = [];
    private listeners: Array<(event: {event: string; data: Record<string, unknown>}) => void> = [];
    private socket?: import('stream').Duplex;

    static async open(url: string, token: string): Promise<EventStream> {
        const stream = new EventStream();
        const hello = new Promise<void>((resolve) => stream.listeners.push((e) => e.event === 'hello' && resolve()));
        await new Promise<void>((resolve, reject) => {
            const req = http.request(`${url}/api/v4/websocket`, {
                headers: {
                    Connection: 'Upgrade',
                    Upgrade: 'websocket',
                    'Sec-WebSocket-Version': '13',
                    'Sec-WebSocket-Key': crypto.randomBytes(16).toString('base64'),
                    Authorization: `Bearer ${token}`,
                },
            });
            req.on('upgrade', (_res, socket, head) => {
                stream.socket = socket;
                socket.on('data', (chunk: Buffer) => stream.read(chunk));
                socket.on('error', () => undefined);
                stream.read(head);
                resolve();
            });
            req.on('response', (res) => reject(new Error(`websocket upgrade refused: HTTP ${res.statusCode}`)));
            req.on('error', reject);
            req.end();
        });
        await Promise.race([hello, new Promise((_, reject) => setTimeout(() => reject(new Error('no websocket hello')), 10_000))]);
        return stream;
    }

    onEvent(fn: (event: {event: string; data: Record<string, unknown>}) => void) {
        this.listeners.push(fn);
    }

    close() {
        this.socket?.destroy();
    }

    private read(chunk: Buffer) {
        this.buffer = Buffer.concat([this.buffer, chunk]);
        for (;;) {
            if (this.buffer.length < 2) {
                return;
            }
            const fin = (this.buffer[0] & 0x80) !== 0;
            const opcode = this.buffer[0] & 0x0f;
            let length = this.buffer[1] & 0x7f;
            let offset = 2;
            if (length === 126) {
                if (this.buffer.length < 4) {
                    return;
                }
                length = this.buffer.readUInt16BE(2);
                offset = 4;
            } else if (length === 127) {
                if (this.buffer.length < 10) {
                    return;
                }
                length = Number(this.buffer.readBigUInt64BE(2));
                offset = 10;
            }
            if (this.buffer.length < offset + length) {
                return;
            }
            const payload = this.buffer.subarray(offset, offset + length);
            this.buffer = this.buffer.subarray(offset + length);
            if (opcode === 1 || opcode === 0) {
                this.fragments.push(payload);
                if (fin) {
                    const text = Buffer.concat(this.fragments).toString('utf8');
                    this.fragments = [];
                    try {
                        const event = JSON.parse(text);
                        this.listeners.forEach((fn) => fn(event));
                    } catch {
                        // not an event
                    }
                }
            }
        }
    }
}

export type ClickResult = {

    // The private reply shown under the card, '' when there was none (the
    // card update says it all).
    reply: string;

    // Set when Mattermost refused the click before the plugin saw it.
    status?: number;
};

// Clicks a card button as username and returns the plugin's private reply,
// which Mattermost delivers over the websocket rather than in the response.
export async function click(hub: RequestHub, username: string, card: Post | string, actionId: string): Promise<ClickResult> {
    const postId = typeof card === 'string' ? card : card.id;
    const client = await hub.client(username);
    const stream = await EventStream.open(hub.mm.url(), client.getToken());
    try {
        let reply = '';
        let gotReply: () => void = () => undefined;
        const replied = new Promise<void>((resolve) => {
            gotReply = resolve;
        });
        stream.onEvent((e) => {
            if (e.event !== 'ephemeral_message' && e.event !== 'posted') {
                return;
            }
            const post = JSON.parse(String(e.data.post ?? '{}')) as Post;
            if (post.type === 'system_ephemeral') {
                reply = post.message;
                gotReply();
            }
        });
        try {
            await client.doPostAction(postId, actionId);
        } catch (error) {
            return {reply: '', status: (error as {status_code?: number}).status_code ?? -1};
        }

        // The reply is sent before the click's response, so a short wait
        // is enough to tell "no reply" apart.
        await Promise.race([replied, new Promise((resolve) => setTimeout(resolve, 1500))]);
        return {reply};
    } finally {
        stream.close();
    }
}

// Reads a card again (the click response doesn't carry it).
export async function reloadPost(hub: RequestHub, postId: string): Promise<Post> {
    return (await hub.client(ADMIN)).getPost(postId);
}

// Adds username to the approval channel, so Mattermost lets them click its
// buttons and the plugin's own permission check is what answers.
export async function joinApprovalChannel(hub: RequestHub, userId: string) {
    await (await hub.client(ADMIN)).addToChannel(userId, await hub.approvalChannelId());
}

// IDs in a user's pending list for kind.
export async function pendingIds(hub: RequestHub, kind: 'channel' | 'admin', userId: string): Promise<string[]> {
    return ((await hub.kv<Array<{id: string}>>(`pending_${kind}_${userId}`)) ?? []).map((e) => e.id);
}

// Checks that a refused decision changed nothing: the card keeps its
// buttons, and the request is still stored and on the requester's list.
export async function expectStillPending(hub: RequestHub, kind: 'channel' | 'admin', cardId: string, requestId: string, requesterId: string) {
    expect(cardActions(await reloadPost(hub, cardId)), 'card buttons').toEqual(['approve', 'deny']);
    const recordKey = (kind === 'channel' ? 'request_' : 'admin_request_') + requestId;
    expect(await hub.kv(recordKey), recordKey).toBeTruthy();
    expect(await pendingIds(hub, kind, requesterId)).toContain(requestId);
}

// Messages in a ticket DM thread, oldest first, read as the requester.
export async function dmThread(hub: RequestHub, username: string, rootId: string): Promise<string[]> {
    const thread = await (await hub.client(username)).getPostThread(rootId);
    return thread.order.map((id) => thread.posts[id]).sort((a, b) => a.create_at - b.create_at).map((p) => p.message);
}

// A new team made with mmctl, so nobody is a member yet: unlike the REST
// API, mmctl doesn't make the creator its Team Admin.
export async function createTeam(hub: RequestHub, memberIds: string[] = []): Promise<{id: string; name: string; displayName: string}> {
    const name = uid('team');
    const displayName = `Team ${name}`;
    await hub.mm.mmctl('team', 'create', '--name', name, '--display-name', displayName);
    const admin = await hub.client(ADMIN);
    const id = (await admin.getTeamByName(name)).id;
    for (const userId of memberIds) {
        await admin.addToTeam(id, userId);
    }
    return {id, name, displayName};
}
