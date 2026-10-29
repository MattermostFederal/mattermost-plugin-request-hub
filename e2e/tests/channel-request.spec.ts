import {expect, test} from '../helpers/fixtures';
import {ADMIN, PLUGIN_ID, SLASH_COMMAND, uid} from '../helpers/requesthub';
import {login, openChannel} from '../helpers/ui';

test('plugin is running', async ({hub}) => {
    const admin = await hub.client(ADMIN);
    const statuses = await admin.getPluginStatuses();
    const status = statuses.find((s) => s.plugin_id === PLUGIN_ID);
    expect(status?.state, JSON.stringify(status)).toBe(2); // PluginStateRunning
});

test('slash command opens the dialog and a submission posts an approval card', async ({hub, page}) => {
    const requester = await hub.createUser();
    const name = `Browser Dialog ${uid()}`;
    await login(page, hub, requester.username);
    await openChannel(page, hub, 'town-square');

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

test('dialog submission: requester cannot approve, System Admin approval creates the channel', async ({hub}) => {
    const requester = await hub.createUser();
    const name = `Smoke Dialog ${uid()}`;
    const client = await hub.client(requester.username);
    const townSquare = await client.getChannelByName(hub.teamId, 'town-square');
    await client.submitInteractiveDialog({
        url: `/plugins/${PLUGIN_ID}/api/v1/dialog`,
        callback_id: 'channel_request',
        state: hub.teamId,
        user_id: requester.id, // ignored: the server takes the user from the session
        channel_id: townSquare.id,
        team_id: hub.teamId,
        submission: {prefix: 'team-', display_name: name, name: '', purpose: 'e2e', type: 'P', category: '', members: hub.userIds.carol, admins: hub.userIds.bob},
        cancelled: false,
    });

    const card = await hub.findCard(name);
    const attachment = (card.props.attachments as Array<{actions: Array<{id: string}>}>)[0];
    expect(attachment.actions.map((a) => a.id)).toEqual(['approve', 'deny']);

    await expect(client.doPostAction(card.id, 'approve')).rejects.toMatchObject({status_code: 403});

    const admin = await hub.client(ADMIN);
    await admin.doPostAction(card.id, 'approve');

    const channelName = `team-${name.toLowerCase().replace(/ /g, '-')}`;
    const channelId = await hub.waitForChannel(channelName);
    expect(channelId, `~${channelName} was not created`).toBeTruthy();

    const channel = await admin.getChannel(channelId!);
    expect(channel.type).toBe('P');
    for (const id of [requester.id, hub.userIds.bob, hub.userIds.carol]) {
        await admin.getChannelMember(channelId!, id);
    }
    expect((await admin.getChannelMember(channelId!, hub.userIds.bob)).scheme_admin).toBe(true);
    expect((await admin.getPost(card.id)).message).toContain('Approved');
});

test('REST request that is denied creates no channel', async ({hub}) => {
    const requester = await hub.createUser();
    const name = uid('smoke-deny-');
    const res = await hub.pluginFetch(requester.username, '/api/v1/create', {team_id: hub.teamId, display_name: name, prefix: 'proj-', name: '', channel_type: 'O', members: [], admin_members: []});
    expect(res.message).toContain('submitted for approval');

    const admin = await hub.client(ADMIN);
    const card = await hub.findCard(name);
    await admin.doPostAction(card.id, 'deny');
    await expect.poll(async () => (await admin.getPost(card.id)).message).toContain('Denied');

    await expect(admin.getChannelByName(hub.teamId, `proj-${name}`)).rejects.toMatchObject({status_code: 404});
});

test('Team Admin can approve a request for their team', async ({hub}) => {
    const requester = await hub.createUser();
    const name = uid('smoke-ta-');
    await hub.pluginFetch(requester.username, '/api/v1/create', {team_id: hub.teamId, display_name: name, prefix: 'proj-', name: '', channel_type: 'O', members: [], admin_members: []});

    const card = await hub.findCard(name);
    await (await hub.client('tadmin')).doPostAction(card.id, 'approve');
    expect(await hub.waitForChannel(`proj-${name}`)).toBeTruthy();
});
