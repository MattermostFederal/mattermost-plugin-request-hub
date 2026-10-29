// Helpers for the slash command spec: reading the help table, checking
// that markdown in a reply is inert, and checking that a refused command
// left nothing behind.
import {expect} from '@playwright/test';

import {ADMIN, HUB_BOT, PLUGIN_ID, REQUEST_CHANNELS, type RequestHub, type TestUser} from './requesthub';

// The first word of each subcommand in the help table, e.g. "channel" for
// both "channel" and "channel <prefix> ...".
export function helpSubcommands(help: string): string[] {
    const words = [...help.matchAll(/^\| `\/mattermost-request-hub ([a-z-]+)[^`]*` \|/gm)].map((m) => m[1]);
    return [...new Set(words)].sort();
}

// The reply with its code spans removed: what is left renders as markdown.
export function outsideCode(text: string): string {
    return text.replace(/`[^`\n]*`/g, '');
}

// The rows of a list reply ("- ..."), and the request IDs they end with.
export function listRows(reply: string): string[] {
    return reply.split('\n').filter((l) => l.startsWith('- '));
}
export function listedIds(reply: string): string[] {
    return listRows(reply).map((l) => (/ID `(\w+)`$/).exec(l)?.[1]).filter((id): id is string => Boolean(id));
}

// Fails unless user has no stored request, no pending entry, no card in a
// request channel and no ticket DM: what a refused submission leaves.
export async function expectNoRequest(hub: RequestHub, user: TestUser) {
    const plugin = `'${PLUGIN_ID}'`;
    const records = await hub.mm.psql(
        `SELECT pkey FROM pluginkeyvaluestore WHERE pluginid = ${plugin} AND (` +
        `(pkey LIKE 'pending\\_%' AND pkey LIKE '%\\_${user.id}' AND convert_from(pvalue, 'UTF8') NOT IN ('[]', 'null')) OR ` +
        `position(convert_to('"requester_id":"${user.id}"', 'UTF8') in pvalue) > 0)`,
    );
    expect(records, `KV records left by @${user.username}`).toBe('');

    const admin = await hub.client(ADMIN);
    for (const name of Object.values(REQUEST_CHANNELS)) {
        const list = await admin.getPosts(await hub.channelId(name), 0, 60);
        const cards = list.order.filter((id) => JSON.stringify(list.posts[id]).includes(user.username));
        expect(cards, `posts mentioning @${user.username} in ~${name}`).toEqual([]);
    }

    const c = await hub.client(user.username);
    const bot = await c.getUserByUsername(HUB_BOT);
    const dm = await c.createDirectChannel([user.id, bot.id]);
    expect((await c.getPosts(dm.id, 0, 10)).order, `DMs from @${HUB_BOT} to @${user.username}`).toEqual([]);
}
