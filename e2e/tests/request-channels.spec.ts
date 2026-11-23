import {expect, test} from '../helpers/fixtures';
import {ADMIN, PLUGIN_ID, REQUEST_CHANNELS, atLeast, uid, type RequestHub} from '../helpers/requesthub';
import {complianceHash} from '../helpers/bots';
import {createTeam} from '../helpers/channel';

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
    const username = uid('bot');

    // In system_admin mode one System Admin can approve both bot steps.
    await hub.withPluginConfig({twostepapprovermode: 'system_admin', bottokenlifetimemonths: String(TOKEN_LIFETIME_MONTHS)}, async () => {
        const res = await hub.pluginFetch<{message?: string; error?: string}>(requester.username, '/api/v1/submit_bot_request', {
            username,
            display_name: 'E2E Bot',
            request_token: true,
            compliance_accepted: true,
            compliance_text_hash: await complianceHash(hub, requester.username),
            purpose: 'e2e',
            integration_endpoint: 'https://ci.example/hooks/e2e',
            hosting_env: 'ci_cd',
            home_team_id: hub.teamId,
            home_channel_id: await hub.channelId('town-square'),
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

// channelRequestsName polls for a team's Channel Requests channel and
// returns its display name.
async function channelRequestsName(hub: RequestHub, teamId: string) {
    const admin = await hub.client(ADMIN);
    return admin.getChannelByName(teamId, REQUEST_CHANNELS.channel).then((c) => c.display_name, () => '');
}

test('per-team Channel Requests channels carry their team name; the root keeps the plain name', async ({hub}) => {
    const admin = await hub.client(ADMIN);
    const team = await createTeam(hub, [hub.userIds[ADMIN]]);

    // A System Admin joining the team makes the plugin create its channel.
    await expect.poll(() => channelRequestsName(hub, team.id), {timeout: 30_000}).toBe(`Mattermost: Channel Requests (${team.displayName})`);
    expect((await admin.getChannelByName(hub.teamId, REQUEST_CHANNELS.channel)).display_name).toBe('Mattermost: Channel Requests');
});

test('setting a new Approval team creates every shared channel there and swaps the names', async ({hub}) => {
    const admin = await hub.client(ADMIN);
    const team = await createTeam(hub, [hub.userIds[ADMIN]]);
    const home = await admin.getTeam(hub.teamId);
    await expect.poll(() => channelRequestsName(hub, team.id), {timeout: 30_000}).toBe(`Mattermost: Channel Requests (${team.displayName})`);

    await hub.withPluginConfig({approvalteam: team.name}, async () => {
        for (const name of Object.values(REQUEST_CHANNELS)) {
            await expect.poll(
                () => admin.getChannelByName(team.id, name).then((c) => c.type, () => ''),
                {message: `~${name} in the new Approval team`, timeout: 30_000},
            ).toBe('P');
        }
        await expect.poll(() => channelRequestsName(hub, team.id), {timeout: 30_000}).toBe('Mattermost: Channel Requests');
        await expect.poll(() => channelRequestsName(hub, hub.teamId), {timeout: 30_000}).toBe(`Mattermost: Channel Requests (${home.display_name})`);
    });

    // Back to the original Approval team: the names swap back.
    await expect.poll(() => channelRequestsName(hub, hub.teamId), {timeout: 30_000}).toBe('Mattermost: Channel Requests');
    await expect.poll(() => channelRequestsName(hub, team.id), {timeout: 30_000}).toBe(`Mattermost: Channel Requests (${team.displayName})`);
});
