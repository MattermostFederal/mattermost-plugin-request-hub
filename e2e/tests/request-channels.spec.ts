import {expect, test} from '@playwright/test';

import {ADMIN, DEFAULT_PLUGIN_CONFIG, PLUGIN_ID, startRequestHub, type RequestHub} from '../helpers/requesthub';

test.describe.configure({mode: 'serial'});

const TOKEN_LIFETIME_MONTHS = 1;

// addMonths matches the server's time.AddDate for the dates the test runs on.
const addMonths = (millis: number, months: number) => {
    const d = new Date(millis);
    d.setUTCMonth(d.getUTCMonth() + months);
    return d.getTime();
};

const REQUEST_CHANNELS = [
    'mattermost-channel-requests',
    'mattermost-bot-requests',
    'mattermost-webhook-requests',
    'mattermost-team-requests',
    'mattermost-audit-requests',
];

let hub: RequestHub;

test.beforeAll(async () => {
    test.setTimeout(6 * 60_000); // image pull + server boot
    // In system_admin mode one System Admin can approve both bot steps.
    hub = await startRequestHub('request-channels', {
        ...DEFAULT_PLUGIN_CONFIG,
        twostepapprovermode: 'system_admin',
        bottokenlifetimemonths: String(TOKEN_LIFETIME_MONTHS),
    });
    await hub.mm.mmctl('config', 'set', 'ServiceSettings.EnableUserAccessTokens', 'true');
});

test.afterAll(async () => {
    await hub?.stop();
});

test('every request channel has one docs bookmark', async () => {
    const admin = await hub.client(ADMIN);
    const docsURL = `${hub.mm.url()}/plugins/${PLUGIN_ID}/public/help/docs.html`;
    for (const name of REQUEST_CHANNELS) {
        // The plugin creates the channels in the background after activation.
        await expect.poll(async () => {
            const channel = await admin.getChannelByName(hub.teamId, name).catch(() => undefined);
            if (!channel) {
                return [];
            }
            const bookmarks = await admin.getChannelBookmarks(channel.id);
            return bookmarks.filter((b) => b.link_url === docsURL).map((b) => b.display_name);
        }, {message: `~${name} bookmarks`, timeout: 30_000}).toEqual(['Request Hub docs']);
    }
});

test('an approved bot token expires after the configured lifetime', async () => {
    const username = `e2e-bot-${Date.now()}`;
    const res = await hub.pluginFetch<{message?: string; error?: string}>('alice', '/api/v1/submit_bot_request', {
        username,
        display_name: 'E2E Bot',
        description: 'e2e',
        request_token: true,
    });
    expect(res.error, JSON.stringify(res)).toBeUndefined();

    const admin = await hub.client(ADMIN);
    const botChannel = await admin.getChannelByName(hub.teamId, 'mattermost-bot-requests');
    let cardId = '';
    await expect.poll(async () => {
        const list = await admin.getPosts(botChannel.id, 0, 30);
        cardId = list.order.find((id) => JSON.stringify(list.posts[id]).includes(username)) ?? '';
        return cardId;
    }, {message: 'bot approval card'}).not.toBe('');

    const before = Date.now();
    await admin.doPostAction(cardId, 'approve');

    let botId = '';
    await expect.poll(async () => {
        botId = (await admin.getUserByUsername(username)).id;
        return botId;
    }, {message: `@${username} created`}).not.toBe('');

    const tokens = await admin.getUserAccessTokensForUser(botId) as Array<{expires_at?: number}>;
    expect(tokens).toHaveLength(1);
    const expiresAt = tokens[0].expires_at ?? 0;
    expect(expiresAt).toBeGreaterThanOrEqual(addMonths(before, TOKEN_LIFETIME_MONTHS) - 60_000);
    expect(expiresAt).toBeLessThanOrEqual(addMonths(Date.now(), TOKEN_LIFETIME_MONTHS) + 60_000);

    // The requester's token DM gives the date.
    const alice = await hub.client('alice');
    const hubBot = await alice.getUserByUsername('mattermost-request-hub');
    const dm = await alice.createDirectChannel([hub.userIds.alice, hubBot.id]); // returns the existing DM
    await expect.poll(async () => {
        const list = await alice.getPosts(dm.id, 0, 30);
        return list.order.map((id) => list.posts[id].message).some((m) => m.includes('The token expires on'));
    }, {message: 'token DM with expiry date'}).toBe(true);
});

test('plugin logged no errors', () => {
    expect(hub.pluginErrors()).toEqual([]);
});
