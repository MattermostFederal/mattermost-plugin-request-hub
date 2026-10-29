import {expect, test} from '../helpers/fixtures';
import {ADMIN, PLUGIN_ID, REQUEST_CHANNELS, atLeast, uid} from '../helpers/requesthub';

const TOKEN_LIFETIME_MONTHS = 1;

// addMonths matches the server's time.AddDate for the dates the test runs on.
const addMonths = (millis: number, months: number) => {
    const d = new Date(millis);
    d.setUTCMonth(d.getUTCMonth() + months);
    return d.getTime();
};

test('every request channel has one docs bookmark', async ({hub}) => {
    const admin = await hub.client(ADMIN);
    const docsURL = `${hub.mm.url()}/plugins/${PLUGIN_ID}/public/help/docs.html`;
    for (const name of Object.values(REQUEST_CHANNELS)) {
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

// Personal access token expiry (MM-68419) arrived in Mattermost 11.9; older
// servers ignore expires_at and the token never expires.
const TOKEN_EXPIRY_SERVER_VERSION = '11.9.0';

test('an approved bot token expires after the configured lifetime', async ({hub}) => {
    const requester = await hub.createUser();
    const username = uid('e2e-bot-');

    // In system_admin mode one System Admin can approve both bot steps.
    await hub.withPluginConfig({twostepapprovermode: 'system_admin', bottokenlifetimemonths: String(TOKEN_LIFETIME_MONTHS)}, async () => {
        const res = await hub.pluginFetch<{message?: string; error?: string}>(requester.username, '/api/v1/submit_bot_request', {
            username,
            display_name: 'E2E Bot',
            description: 'e2e',
            request_token: true,
        });
        expect(res.error, JSON.stringify(res)).toBeUndefined();

        const admin = await hub.client(ADMIN);
        const card = await hub.findCardIn(REQUEST_CHANNELS.bot, username);

        const before = Date.now();
        await admin.doPostAction(card.id, 'approve');

        let botId = '';
        await expect.poll(async () => {
            botId = (await admin.getUserByUsername(username)).id;
            return botId;
        }, {message: `@${username} created`}).not.toBe('');

        const tokens = await admin.getUserAccessTokensForUser(botId) as Array<{expires_at?: number}>;
        expect(tokens).toHaveLength(1);
        const expiresAt = tokens[0].expires_at ?? 0;

        if (!atLeast(hub.serverVersion, TOKEN_EXPIRY_SERVER_VERSION)) {
            // The token can't expire here, so the DM must not claim a date.
            expect(expiresAt).toBe(0);
            const dm = await hub.findDM(requester.username, '**Access Token**');
            expect(dm.message).not.toContain('expires on');
            return;
        }

        expect(expiresAt).toBeGreaterThanOrEqual(addMonths(before, TOKEN_LIFETIME_MONTHS) - 60_000);
        expect(expiresAt).toBeLessThanOrEqual(addMonths(Date.now(), TOKEN_LIFETIME_MONTHS) + 60_000);

        // The requester's token DM gives the date.
        await hub.findDM(requester.username, 'The token expires on');
    });
});
