import {expect, test, type Page} from '@playwright/test';

import {ADMIN, PASSWORD, PLUGIN_ID, SLASH_COMMAND, TEAM, startRequestHub, type RequestHub} from '../helpers/requesthub';

test.describe.configure({mode: 'serial'});

let hub: RequestHub;

test.beforeAll(async () => {
    test.setTimeout(6 * 60_000); // image pull + server boot
    hub = await startRequestHub('channel-request');
});

test.afterAll(async () => {
    await hub?.stop();
});

async function login(page: Page, username: string) {
    await page.goto(`${hub.mm.url()}/login`);
    const viewInBrowser = page.getByText('View in Browser');
    await viewInBrowser.or(page.locator('#input_loginId')).first().waitFor();
    if (await viewInBrowser.isVisible()) {
        await viewInBrowser.click();
    }
    await page.locator('#input_loginId').fill(username);
    await page.locator('#input_password-input').fill(PASSWORD);
    await page.keyboard.press('Enter');
    await page.waitForURL((u) => !u.pathname.startsWith('/login'));
}

test('plugin is running', async () => {
    const admin = await hub.client(ADMIN);
    const statuses = await admin.getPluginStatuses();
    const status = statuses.find((s) => s.plugin_id === PLUGIN_ID);
    expect(status?.state, JSON.stringify(status)).toBe(2); // PluginStateRunning
});

test('slash command opens the dialog and a submission posts an approval card', async ({page}) => {
    const name = `Browser Dialog ${Date.now()}`;
    await login(page, 'alice');
    await page.goto(`${hub.mm.url()}/${TEAM}/channels/town-square`);

    const input = page.getByTestId('post_textbox');
    await input.fill(SLASH_COMMAND);

    // Enter would pick the open autocomplete suggestion instead of sending.
    await page.getByText('Request admin-gated access via Request Hub').waitFor();
    await page.getByTestId('SendMessageButton').click();

    const dialog = page.getByRole('dialog').filter({hasText: 'Request a Channel'});
    await expect(dialog).toBeVisible();
    // Domain prefix is the first dropdown (react-select) in the form.
    await dialog.getByRole('combobox').first().click();
    await page.getByText('team-  (Team channels)').click();
    await dialog.getByPlaceholder('e.g. Marketing Team').fill(name);
    await dialog.getByRole('button', {name: 'Submit request'}).click();
    await expect(dialog).toBeHidden();

    const card = await hub.findCard(name);
    expect(card.props.attachments).toBeTruthy();
});

test('dialog submission: requester cannot approve, System Admin approval creates the channel', async () => {
    const name = `Smoke Dialog ${Date.now()}`;
    const alice = await hub.client('alice');
    const townSquare = await alice.getChannelByName(hub.teamId, 'town-square');
    await alice.submitInteractiveDialog({
        url: `/plugins/${PLUGIN_ID}/api/v1/dialog`,
        callback_id: 'channel_request',
        state: hub.teamId,
        user_id: hub.userIds.alice, // ignored: the server takes the user from the session
        channel_id: townSquare.id,
        team_id: hub.teamId,
        submission: {prefix: 'team-', display_name: name, name: '', purpose: 'e2e', type: 'P', category: '', members: hub.userIds.carol, admins: hub.userIds.bob},
        cancelled: false,
    });

    const card = await hub.findCard(name);
    const attachment = (card.props.attachments as Array<{actions: Array<{id: string}>}>)[0];
    expect(attachment.actions.map((a) => a.id)).toEqual(['approve', 'deny']);

    await expect(alice.doPostAction(card.id, 'approve')).rejects.toMatchObject({status_code: 403});

    const admin = await hub.client(ADMIN);
    await admin.doPostAction(card.id, 'approve');

    const channelName = `team-${name.toLowerCase().replace(/ /g, '-')}`;
    const channelId = await hub.waitForChannel(channelName);
    expect(channelId, `~${channelName} was not created`).toBeTruthy();

    const channel = await admin.getChannel(channelId!);
    expect(channel.type).toBe('P');
    for (const u of ['alice', 'bob', 'carol']) {
        await admin.getChannelMember(channelId!, hub.userIds[u]);
    }
    expect((await admin.getChannelMember(channelId!, hub.userIds.bob)).scheme_admin).toBe(true);
    expect((await admin.getPost(card.id)).message).toContain('Approved');
});

test('REST request that is denied creates no channel', async () => {
    const name = `smoke-deny-${Date.now()}`;
    const res = await hub.pluginFetch('alice', '/api/v1/create', {team_id: hub.teamId, display_name: name, prefix: 'proj-', name: '', channel_type: 'O', members: [], admin_members: []});
    expect(res.message).toContain('submitted for approval');

    const card = await hub.findCard(name);
    await (await hub.client(ADMIN)).doPostAction(card.id, 'deny');
    await expect.poll(async () => (await (await hub.client(ADMIN)).getPost(card.id)).message).toContain('Denied');

    const admin = await hub.client(ADMIN);
    await expect(admin.getChannelByName(hub.teamId, `proj-${name}`)).rejects.toMatchObject({status_code: 404});
});

test('Team Admin can approve a request for their team', async () => {
    const name = `smoke-ta-${Date.now()}`;
    await hub.pluginFetch('alice', '/api/v1/create', {team_id: hub.teamId, display_name: name, prefix: 'proj-', name: '', channel_type: 'O', members: [], admin_members: []});

    const card = await hub.findCard(name);
    await (await hub.client('tadmin')).doPostAction(card.id, 'approve');
    expect(await hub.waitForChannel(`proj-${name}`)).toBeTruthy();
});

test('plugin logged no errors', () => {
    expect(hub.pluginErrors()).toEqual([]);
});
