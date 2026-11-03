import type {Post} from '@mattermost/types/posts';

import {
    cardActions,
    click,
    createTeam,
    dmThread,
    expectNothingStored,
    expectStillPending,
    joinApprovalChannel,
    pendingIds,
    postsMentioning,
    reloadPost,
    submitChannel,
    submitChannelDialog,
    type ChannelRequestBody,
} from '../helpers/channel';
import {expect, test} from '../helpers/fixtures';
import {ADMIN, PLUGIN_ID, SLASH_COMMAND, cardRequestId, uid, type RequestHub, type TestUser} from '../helpers/requesthub';
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

type StoredChannelRequest = {
    id: string;
    requester_id: string;
    team_id: string;
    name: string;
    display_name: string;
    purpose: string;
    channel_type: string;
    member_ids: string[] | null;
    admin_member_ids: string[] | null;
    category_name?: string;
};

const SUBMITTED = "Your channel request has been submitted for approval. You'll be notified once an admin responds.";
const NO_PERMISSION = "You don't have permission to approve or deny this channel request. Team Admins of this team or a System Admin can approve it.";

// Display names as the plugin escapes them in card and audit text.
const escaped = (s: string) => s.replace(/[\\`*_[\]()#~>|!<{}+\-.]/g, '\\$&');

// Submits a request over REST as requester and returns its card, ID and
// stored record.
async function pendingRequest(hub: RequestHub, requester: TestUser, body: ChannelRequestBody = {}) {
    const displayName = body.display_name ?? uid('chan-');
    const res = await submitChannel(hub, requester.username, {display_name: displayName, ...body});
    expect(res).toEqual({message: SUBMITTED});
    const card = await hub.findCard(displayName.trim().replace(/\s+/g, ' '));
    const requestId = cardRequestId(card);
    const record = (await hub.kv<StoredChannelRequest>(`request_${requestId}`))!;
    return {card, requestId, record, displayName};
}

// The value of a field on an approval card.
function cardField(card: Post, title: string): string | undefined {
    const attachments = card.props.attachments as Array<{fields?: Array<{title: string; value: string}>}>;
    return attachments[0].fields?.find((f) => f.title === title)?.value;
}

async function expectNoChannel(hub: RequestHub, teamId: string, name: string) {
    await expect((await hub.client(ADMIN)).getChannelByName(teamId, name, true)).rejects.toMatchObject({status_code: 404});
}

test.describe('submit', () => {
    test('every configured prefix names the channel <prefix><suffix>', async ({hub}) => {
        const prefixes = 'team-|Team channels|32\nproj-|Project channels|32\nops-|Operations|\nlab-|Labs|[a-z]{3,40}';
        await hub.withPluginConfig({channelnameprefixes: prefixes}, async () => {
            const listed = await hub.pluginFetch<Array<{prefix: string}>>((await hub.createUser()).username, '/api/v1/prefixes');
            expect(listed.map((p) => p.prefix)).toEqual(['team-', 'proj-', 'ops-', 'lab-']);

            for (const prefix of ['team-', 'proj-', 'ops-', 'lab-']) {
                const requester = await hub.createUser();

                // Letters only, so the lab- rule matches too.
                const suffix = uid('sfx').replace(/[0-9]/g, 'x');
                const {card, record} = await pendingRequest(hub, requester, {prefix, name: suffix});
                expect(record.name).toBe(`${prefix}${suffix}`);
                expect(cardField(card, 'URL')).toBe(`${prefix}${suffix}`);
            }
        });
    });

    test('a prefix typed without its hyphen, or typed again in the suffix, is used once', async ({hub}) => {
        const requester = await hub.createUser();
        const alpha = uid('alpha');
        const a = await pendingRequest(hub, requester, {prefix: 'team', name: alpha});
        expect(a.record.name).toBe(`team-${alpha}`);

        const beta = uid('beta');
        const b = await pendingRequest(hub, requester, {prefix: 'team-', name: `team-${beta}`});
        expect(b.record.name).toBe(`team-${beta}`);
    });

    test('display name and suffix are slugged to lowercase letters, digits and hyphens', async ({hub}) => {
        const requester = await hub.createUser();
        const u = uid('');
        const {record, card} = await pendingRequest(hub, requester, {prefix: 'proj-', display_name: `  Big   Plan_${u} (Q3)!  `});
        expect(record.name).toBe(`proj-big-plan-${u}-q3`);
        expect(record.display_name).toBe(`Big Plan_${u} (Q3)!`);
        expect(cardField(card, 'Channel name')).toBe(escaped(`Big Plan_${u} (Q3)!`));
    });

    test('suffix length follows the prefix line: 32 is accepted, 33 and 1 are refused', async ({hub}) => {
        const requester = await hub.createUser();
        const at32 = (uid('s') + 'x'.repeat(32)).slice(0, 32);
        const {record} = await pendingRequest(hub, requester, {prefix: 'team-', name: at32, display_name: uid('len32-')});
        expect(record.name).toBe(`team-${at32}`);

        for (const suffix of [at32 + 'y', 'a']) {
            const user = await hub.createUser();
            const displayName = uid('len-');
            const res = await submitChannel(hub, user.username, {prefix: 'team-', name: suffix, display_name: displayName});
            expect(res.error).toBe(`suffix "${suffix}" doesn't match the required pattern for prefix "team-" (32)`);
            await expectNothingStored(hub, 'channel', user.id, displayName);
            await expectNoChannel(hub, hub.teamId, `team-${suffix}`);
        }
    });

    test('a suffix with no letters or digits is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const displayName = uid('symbols-');
        const res = await submitChannel(hub, requester.username, {prefix: 'proj-', name: '!!! ___ ...', display_name: displayName});
        expect(res.error).toBe('channel name suffix is required (letters/numbers, becomes the part after the prefix)');
        await expectNothingStored(hub, 'channel', requester.id, displayName);
    });

    test('an unknown or missing prefix is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const displayName = uid('prefix-');
        const unknown = 'prefix "zzz-" is not in the list of allowed prefixes: team-, proj-';

        expect(await submitChannel(hub, requester.username, {prefix: 'zzz-', display_name: displayName})).toEqual({error: unknown});
        expect(await submitChannel(hub, requester.username, {prefix: '', display_name: displayName})).toEqual({error: 'please pick a channel prefix (e.g., team-, project-, ops-) from the dropdown'});
        expect(await submitChannelDialog(hub, requester.username, {prefix: 'zzz-', display_name: displayName})).toBe(unknown);
        expect(await hub.slash(requester.username, `channel zzz- ${displayName}`)).toBe(unknown);
        await expectNothingStored(hub, 'channel', requester.id, displayName);
    });

    test('display name over 64 characters, purpose over 250, or no display name is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const u = uid('');
        const name64 = (u + 'n'.repeat(64)).slice(0, 64);

        expect(await submitChannel(hub, requester.username, {display_name: name64 + 'n', name: u})).toEqual({error: 'channel name must be 64 characters or fewer'});
        expect(await submitChannel(hub, requester.username, {display_name: u, purpose: 'p'.repeat(251)})).toEqual({error: 'purpose must be 250 characters or fewer'});
        expect(await submitChannel(hub, requester.username, {display_name: '   ', name: u})).toEqual({error: 'a channel name is required'});

        // The dialog is checked on the server too, not only by its MaxLength.
        expect(await submitChannelDialog(hub, requester.username, {display_name: name64 + 'n', name: u})).toBe('channel name must be 64 characters or fewer');
        expect(await submitChannelDialog(hub, requester.username, {display_name: u, purpose: 'p'.repeat(251)})).toBe('purpose must be 250 characters or fewer');
        await expectNothingStored(hub, 'channel', requester.id, u);

        // Exactly at the limits is fine.
        const {record} = await pendingRequest(hub, requester, {display_name: name64, name: u, purpose: 'p'.repeat(250)});
        expect(record.display_name).toBe(name64);
        expect(record.purpose).toHaveLength(250);
    });

    test('an unknown member or Channel Admin is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const displayName = uid('members-');
        const ghost = uid('ghost');

        expect(await submitChannel(hub, requester.username, {display_name: displayName, members: [`@${ghost}`]})).toEqual({error: `unknown user: ${ghost}`});
        expect(await submitChannel(hub, requester.username, {display_name: displayName, admin_members: [ghost]})).toEqual({error: `unknown user: ${ghost}`});
        expect(await hub.slash(requester.username, `channel proj- ${displayName} --admins @${ghost}`)).toBe(`unknown user: ${ghost}`);
        expect(await submitChannelDialog(hub, requester.username, {display_name: displayName, members: 'not-an-id'})).toBe('members and channel admins must be valid users');
        await expectNothingStored(hub, 'channel', requester.id, displayName);
    });

    test('a requester who is not on the team is refused', async ({hub}) => {
        const requester = await hub.createUser({inTeam: false});
        const displayName = uid('outsider-');
        expect(await submitChannel(hub, requester.username, {display_name: displayName})).toEqual({error: 'you must be a member of the team to request a channel in it'});
        await expectNothingStored(hub, 'channel', requester.id, displayName);
    });

    test('with channel requests turned off, REST, dialog and slash command refuse', async ({hub}) => {
        const requester = await hub.createUser();
        const displayName = uid('off-');
        const off = 'Channel requests are currently disabled by an administrator.';
        await hub.withPluginConfig({allowchannelrequests: false}, async () => {
            expect(await submitChannel(hub, requester.username, {display_name: displayName})).toEqual({error: off});
            expect(await submitChannelDialog(hub, requester.username, {display_name: displayName})).toBe(off);
            expect(await hub.slash(requester.username, `channel proj- ${displayName}`)).toBe(off);
            expect(await hub.slash(requester.username, 'channel')).toBe(off);
        });
        await expectNothingStored(hub, 'channel', requester.id, displayName);
    });

    test('a second pending request for the same channel is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const {requestId, record, displayName} = await pendingRequest(hub, requester);

        const res = await submitChannel(hub, requester.username, {display_name: displayName.toUpperCase()});
        expect(res).toEqual({error: `you already have a pending request for ~${record.name} in this team`});
        expect(await postsMentioning(hub, await hub.approvalChannelId(), displayName)).toHaveLength(1);
        expect(await pendingIds(hub, 'channel', requester.id)).toEqual([requestId]);
    });

    test('a channel name that already exists is refused at submit', async ({hub}) => {
        test.fail(true, 'BUG: submitRequest never checks whether the channel name is taken, so the request queues and only fails at approval');
        const requester = await hub.createUser();
        const displayName = uid('taken-');
        const admin = await hub.client(ADMIN);
        await admin.createChannel({team_id: hub.teamId, name: `proj-${displayName}`, display_name: displayName, type: 'O'});

        const res = await submitChannel(hub, requester.username, {display_name: displayName});
        expect(res.error).toBeTruthy();
        await expectNothingStored(hub, 'channel', requester.id, displayName);
    });

    test('inline slash command files every field, and needs a prefix and a name', async ({hub}) => {
        const requester = await hub.createUser();
        const member = await hub.createUser();
        const chanAdmin = await hub.createUser();
        const displayName = uid('Inline ');
        const suffix = uid('inl');
        const reply = await hub.slash(requester.username,
            `channel team "${displayName}" --suffix ${suffix} --purpose Launch plans --private --members @${member.username} --admins ${chanAdmin.username} --category Launch Work`);
        expect(reply).toBe(SUBMITTED);

        const card = await hub.findCard(displayName);
        const record = (await hub.kv<StoredChannelRequest>(`request_${cardRequestId(card)}`))!;
        expect(record).toMatchObject({
            requester_id: requester.id,
            team_id: hub.teamId,
            name: `team-${suffix}`,
            display_name: displayName,
            purpose: 'Launch plans',
            channel_type: 'P',
            member_ids: [member.id],
            admin_member_ids: [chanAdmin.id],
            category_name: 'Launch Work',
        });
        expect(cardField(card, 'Visibility')).toBe('Private');
        expect(cardField(card, 'Sidebar category')).toBe('Launch Work');

        expect(await hub.slash(requester.username, 'channel team-')).toContain('Could not read the request: a prefix and a channel name are required.');
        expect(await hub.slash(requester.username, 'channel team- x --colour red')).toContain('Could not read the request: unknown option `--colour`.');
        expect(await pendingIds(hub, 'channel', requester.id)).toEqual([record.id]);
    });
});

test.describe('approval', () => {
    test('approve: card, ticket DM, channel, welcome post, audit and pending list', async ({hub}) => {
        const requester = await hub.createUser();
        const member = await hub.createUser();
        const other = await hub.createUser();
        const {card, requestId, record, displayName} = await pendingRequest(hub, requester, {purpose: 'Roadmap', members: [member.username, other.username]});
        expect(card.message).toBe('@channel — a new channel request needs your review.');
        expect(card.channel_id).toBe(await hub.approvalChannelId());
        expect(cardActions(card)).toEqual(['approve', 'deny']);
        expect(await pendingIds(hub, 'channel', requester.id)).toEqual([requestId]);

        const dm = await hub.findDM(requester.username, displayName);
        expect(dm.message).toBe('Your channel request has been submitted for approval. Reply to this thread to add context — your reply will be forwarded to the reviewers.');
        expect(cardActions(dm)).toEqual([]);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: ''});

        // With no Channel Admins named, the requester becomes one.
        const admin = await hub.client(ADMIN);
        const channel = await admin.getChannelByName(hub.teamId, record.name);
        expect(channel).toMatchObject({display_name: displayName, purpose: 'Roadmap', type: 'O'});
        expect((await admin.getChannelMember(channel.id, requester.id)).scheme_admin).toBe(true);
        expect((await admin.getChannelMember(channel.id, member.id)).scheme_admin).toBe(false);
        expect((await admin.getChannelMember(channel.id, other.id)).scheme_admin).toBe(false);

        const resolved = await reloadPost(hub, card.id);
        expect(resolved.message).toBe(`✅ Approved by @${ADMIN} for @${requester.username}. Channel ~${record.name} created.`);
        expect(cardActions(resolved)).toEqual([]);

        const ticket = await reloadPost(hub, dm.id);
        expect((ticket.props.attachments as Array<{title: string}>)[0].title).toBe('Channel Request Approved');
        expect(await dmThread(hub, requester.username, dm.id)).toEqual([dm.message, `✅ **Approved** by @${ADMIN}.`]);

        const welcome = await postsMentioning(hub, channel.id, 'Welcome');
        expect(welcome.map((p) => p.message)).toEqual([
            `Welcome — this channel was requested by @${requester.username} and approved by @${ADMIN}. Adjust the header + purpose to fit.\nAdded: @${member.username}, @${other.username}`,
        ]);

        const audit = await hub.findAudit(displayName);
        expect((audit.props.attachments as Array<{title: string; text: string}>)[0]).toMatchObject({
            title: '✅ Approved',
            text: expect.stringContaining(`@${ADMIN} approved channel request "${escaped(displayName)}" (~${record.name}) from @${requester.username}`),
        });

        expect(await hub.kv(`request_${requestId}`)).toBeUndefined();
        expect(await pendingIds(hub, 'channel', requester.id)).toEqual([]);
    });

    test('deny: card, ticket DM, audit and pending list, and no channel', async ({hub}) => {
        const requester = await hub.createUser();
        const {card, requestId, record, displayName} = await pendingRequest(hub, requester);
        const dm = await hub.findDM(requester.username, displayName);

        expect(await click(hub, 'tadmin', card, 'deny')).toEqual({reply: ''});

        const resolved = await reloadPost(hub, card.id);
        expect(resolved.message).toBe(`❌ Denied by @tadmin for @${requester.username}. Channel request **${escaped(displayName)}** (\`${record.name}\`).`);
        expect(cardActions(resolved)).toEqual([]);

        const ticket = await reloadPost(hub, dm.id);
        expect((ticket.props.attachments as Array<{title: string}>)[0].title).toBe('Channel Request Denied');
        expect(await dmThread(hub, requester.username, dm.id)).toEqual([dm.message, '❌ **Denied** by @tadmin.']);

        const audit = await hub.findAudit(displayName);
        expect((audit.props.attachments as Array<{title: string; text: string}>)[0]).toMatchObject({
            title: '❌ Denied',
            text: expect.stringContaining(`@tadmin denied channel request "${escaped(displayName)}" from @${requester.username}`),
        });

        await expectNoChannel(hub, hub.teamId, record.name);
        expect(await hub.kv(`request_${requestId}`)).toBeUndefined();
        expect(await pendingIds(hub, 'channel', requester.id)).toEqual([]);
    });

    test('a regular member in the approval channel can neither approve nor deny', async ({hub}) => {
        const requester = await hub.createUser();
        const bystander = await hub.createUser();
        await joinApprovalChannel(hub, bystander.id);
        const {card, requestId, record} = await pendingRequest(hub, requester);

        expect(await click(hub, bystander.username, card, 'approve')).toEqual({reply: NO_PERMISSION});
        expect(await click(hub, bystander.username, card, 'deny')).toEqual({reply: NO_PERMISSION});
        expect(await hub.slash(bystander.username, `approve ${requestId}`)).toBe(NO_PERMISSION);

        // Someone outside the approval channel can't even reach the button.
        expect(await click(hub, requester.username, card, 'approve')).toEqual({reply: '', status: 403});

        await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
        await expectNoChannel(hub, hub.teamId, record.name);
    });

    test('a Team Admin can\'t approve their own request', async ({hub}) => {
        const requester = await hub.createUser({teamAdmin: true});
        const {card, requestId, record} = await pendingRequest(hub, requester);
        expect(await hub.slash(requester.username, `approve ${requestId}`)).toBe("You can't approve your own request. Another approver has to review it.");
        await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
        await expectNoChannel(hub, hub.teamId, record.name);
    });

    test('with AllowTeamAdminApprovers off, an approval-team Team Admin can\'t approve for another team', async ({hub}) => {
        const requester = await hub.createUser();
        const team = await createTeam(hub, [requester.id]);
        await hub.withPluginConfig({allowteamadminapprovers: false}, async () => {
            // The new team has no approvers of its own, so its card goes to the approval team.
            const {card, requestId, record} = await pendingRequest(hub, requester, {team_id: team.id});
            expect(card.message).toBe(`For Team: ${escaped(team.displayName)} — @channel — a new channel request needs your review.`);

            expect(await click(hub, 'tadmin', card, 'approve')).toEqual({reply: NO_PERMISSION});
            expect(await click(hub, 'tadmin', card, 'deny')).toEqual({reply: NO_PERMISSION});
            await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
            await expectNoChannel(hub, team.id, record.name);
        });
    });

    test('with AllowTeamAdminApprovers on, an approval-team Team Admin approves for another team', async ({hub}) => {
        const requester = await hub.createUser();
        const team = await createTeam(hub, [requester.id]);
        await hub.withPluginConfig({allowteamadminapprovers: true}, async () => {
            const {card, record} = await pendingRequest(hub, requester, {team_id: team.id});
            expect(await click(hub, 'tadmin', card, 'approve')).toEqual({reply: ''});
            const channel = await (await hub.client(ADMIN)).getChannelByName(team.id, record.name);
            expect(channel.team_id).toBe(team.id);
        });
    });

    test('two approvers clicking Approve at once create the channel once', async ({hub}) => {
        const requester = await hub.createUser();
        const {card, requestId, record, displayName} = await pendingRequest(hub, requester);
        const dm = await hub.findDM(requester.username, displayName);

        const results = await Promise.all([click(hub, ADMIN, card, 'approve'), click(hub, 'tadmin', card, 'approve')]);

        // One wins with no reply. The other finds the request gone, or its
        // card no longer the request's, or (once the card lost its buttons)
        // is turned away by Mattermost.
        const winners = results.filter((r) => r.reply === '' && r.status === undefined);
        expect(winners, JSON.stringify(results)).toHaveLength(1);
        const loser = results.find((r) => r !== winners[0])!;
        expect(['This request has already been handled.', "This button doesn't belong to that request's card.", ''], JSON.stringify(loser)).toContain(loser.reply);
        if (loser.reply === '') {
            expect(loser.status).toBe(404);
        }

        const channel = await (await hub.client(ADMIN)).getChannelByName(hub.teamId, record.name);
        expect(await postsMentioning(hub, channel.id, 'Welcome')).toHaveLength(1);
        const resolved = await reloadPost(hub, card.id);
        expect(resolved.message).toMatch(new RegExp(`^✅ Approved by @(${ADMIN}|tadmin) for @${requester.username}\\. Channel ~${record.name} created\\.$`));
        expect(await dmThread(hub, requester.username, dm.id)).toHaveLength(2);
        expect(await postsMentioning(hub, await hub.channelId('mattermost-audit-requests'), `(~${record.name})`)).toHaveLength(1);
        expect(await hub.kv(`request_${requestId}`)).toBeUndefined();
        expect(await pendingIds(hub, 'channel', requester.id)).toEqual([]);
    });

    test('a deactivated requester blocks approval; the request stays pending and can be denied', async ({hub}) => {
        const requester = await hub.createUser();
        const {card, requestId, record} = await pendingRequest(hub, requester);
        await (await hub.client(ADMIN)).updateUserActive(requester.id, false);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: "The requester's account is no longer active. Deny this request instead."});
        await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
        await expectNoChannel(hub, hub.teamId, record.name);

        expect(await click(hub, ADMIN, card, 'deny')).toEqual({reply: ''});
        expect((await reloadPost(hub, card.id)).message).toContain(`❌ Denied by @${ADMIN}`);
        expect(await hub.kv(`request_${requestId}`)).toBeUndefined();
    });

    test('a requester who left the team blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const {card, requestId, record} = await pendingRequest(hub, requester);
        await (await hub.client(ADMIN)).removeFromTeam(hub.teamId, requester.id);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: 'The requester is no longer a member of the team. Deny this request instead.'});
        await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
        await expectNoChannel(hub, hub.teamId, record.name);
    });

    test('a name that no longer fits the prefixes blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const {card, requestId, record} = await pendingRequest(hub, requester, {prefix: 'team-'});
        await hub.withPluginConfig({channelnameprefixes: 'proj-|Project channels|32'}, async () => {
            expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: 'The channel name no longer matches the configured prefixes. Deny this request and ask the requester to submit it again.'});
        });
        await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
        await expectNoChannel(hub, hub.teamId, record.name);
    });

    test('with channel requests turned off, Approve is refused but Deny still closes the request', async ({hub}) => {
        const requester = await hub.createUser();
        const {card, requestId, record} = await pendingRequest(hub, requester);
        await hub.withPluginConfig({allowchannelrequests: false}, async () => {
            expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: 'Requests of this type are turned off. Deny this request instead.'});
            await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
            await expectNoChannel(hub, hub.teamId, record.name);

            expect(await click(hub, ADMIN, card, 'deny')).toEqual({reply: ''});
        });
        expect(await hub.kv(`request_${requestId}`)).toBeUndefined();
        expect(await pendingIds(hub, 'channel', requester.id)).toEqual([]);
    });

    test('an archived team blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const team = await createTeam(hub, [requester.id]);
        const {card, requestId, record} = await pendingRequest(hub, requester, {team_id: team.id});
        await (await hub.client(ADMIN)).deleteTeam(team.id);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: 'The team has been archived. Deny this request instead.'});
        await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
        await expectNoChannel(hub, team.id, record.name);
    });

    test.describe(() => {
        test.use({allowedPluginErrors: [/failed to create channel on approval/]});

        test('a channel created meanwhile with the same name fails approval and keeps the request pending', async ({hub}) => {
            const requester = await hub.createUser();
            const {card, requestId, record, displayName} = await pendingRequest(hub, requester);
            const admin = await hub.client(ADMIN);
            const squatter = await admin.createChannel({team_id: hub.teamId, name: record.name, display_name: 'Squatter', type: 'O'});

            const {reply} = await click(hub, ADMIN, card, 'approve');
            expect(reply).toMatch(/^Could not create the channel: .+\. The request is still pending\. If this keeps failing, the channel name may already be taken — deny it and ask the requester to resubmit with a different name\.$/);
            await expectStillPending(hub, 'channel', card.id, requestId, requester.id);
            expect((await admin.getChannelByName(hub.teamId, record.name)).id).toBe(squatter.id);
            expect(await postsMentioning(hub, await hub.channelId('mattermost-audit-requests'), displayName)).toEqual([]);

            expect(await click(hub, ADMIN, card, 'deny')).toEqual({reply: ''});
            expect(await pendingIds(hub, 'channel', requester.id)).toEqual([]);
        });
    });

    test('the sidebar category is used for the requester, members and Channel Admins', async ({hub}) => {
        const requester = await hub.createUser();
        const member = await hub.createUser();
        const chanAdmin = await hub.createUser();
        const category = uid('Cat ');

        // An existing category matches case-insensitively instead of being duplicated.
        const existing = await (await hub.client(chanAdmin.username)).createChannelCategory(chanAdmin.id, hub.teamId, {
            user_id: chanAdmin.id,
            team_id: hub.teamId,
            type: 'custom',
            display_name: category.toUpperCase(),
            channel_ids: [],
        });

        const {card, record} = await pendingRequest(hub, requester, {members: [member.username], admin_members: [chanAdmin.username], category});
        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: ''});
        const channel = await (await hub.client(ADMIN)).getChannelByName(hub.teamId, record.name);

        for (const user of [requester, member, chanAdmin]) {
            const {categories} = await (await hub.client(user.username)).getChannelCategories(user.id, hub.teamId);
            const matching = categories.filter((c) => c.display_name.toLowerCase() === category.toLowerCase());
            expect(matching, user.username).toHaveLength(1);
            expect(matching[0].channel_ids, user.username).toContain(channel.id);
            if (user === chanAdmin) {
                expect(matching[0].id).toBe(existing.id);
            }
        }
        expect((await (await hub.client(ADMIN)).getChannelMember(channel.id, chanAdmin.id)).scheme_admin).toBe(true);
    });

    test('per-team approval channels: a team with a Team Admin gets its own approval channel', async ({hub}) => {
        const requester = await hub.createUser();
        const teamAdmin = await hub.createUser({inTeam: false});
        const team = await createTeam(hub, [requester.id, teamAdmin.id]);
        const admin = await hub.client(ADMIN);
        await admin.updateTeamMemberSchemeRoles(team.id, teamAdmin.id, true, true);

        await hub.withPluginConfig({perteamapprovalchannels: true, allowteamadminapprovers: false}, async () => {
            const displayName = uid('perteam-');
            expect(await submitChannel(hub, requester.username, {team_id: team.id, display_name: displayName})).toEqual({message: SUBMITTED});
            expect(await postsMentioning(hub, await hub.approvalChannelId(), displayName)).toEqual([]);

            const teamChannel = await admin.getChannelByName(team.id, 'mattermost-channel-requests');
            const [card] = await postsMentioning(hub, teamChannel.id, displayName);
            expect(card.message).toBe('@channel — a new channel request needs your review.');

            // That team's Team Admin approves there, whatever AllowTeamAdminApprovers says.
            expect(await click(hub, teamAdmin.username, card, 'approve')).toEqual({reply: ''});
            expect((await admin.getChannelByName(team.id, `proj-${displayName}`)).team_id).toBe(team.id);
        });
    });

    test('per-team approval channels off: every card goes to the approval team', async ({hub}) => {
        const requester = await hub.createUser();
        const teamAdmin = await hub.createUser({inTeam: false});
        const team = await createTeam(hub, [requester.id, teamAdmin.id]);
        const admin = await hub.client(ADMIN);
        await admin.updateTeamMemberSchemeRoles(team.id, teamAdmin.id, true, true);

        await hub.withPluginConfig({perteamapprovalchannels: false}, async () => {
            const {card, displayName} = await pendingRequest(hub, requester, {team_id: team.id});
            expect(card.channel_id).toBe(await hub.approvalChannelId());
            expect(card.message).toBe('@channel — a new channel request needs your review.');

            const teamChannel = await admin.getChannelByName(team.id, 'mattermost-channel-requests', true).catch(() => undefined);
            if (teamChannel) {
                expect(await postsMentioning(hub, teamChannel.id, displayName)).toEqual([]);
            }
        });
    });
});

test('channel header menu "Request a Channel" opens the modal and submits', async ({hub, page}) => {
    const requester = await hub.createUser();
    const displayName = uid('Modal ');
    await login(page, hub, requester.username);
    await openChannel(page, hub, 'town-square');

    await page.locator('#channelHeaderTitle').click();
    await page.getByRole('menuitem', {name: 'More actions'}).hover();
    await page.getByRole('menuitem', {name: 'Request a Channel'}).click();

    const dialog = page.getByRole('dialog', {name: 'Request a Channel'});
    await expect(dialog).toBeVisible();
    await dialog.getByLabel('Domain prefix').selectOption('team-');
    await dialog.getByLabel('Channel name', {exact: true}).fill(displayName);
    await dialog.getByLabel('Purpose (optional)').fill('From the modal');
    await dialog.getByLabel('Visibility').selectOption('P');
    await dialog.getByRole('button', {name: 'Submit request'}).click();
    await expect(dialog.getByText(SUBMITTED)).toBeVisible();

    const card = await hub.findCard(displayName);
    const record = (await hub.kv<StoredChannelRequest>(`request_${cardRequestId(card)}`))!;
    expect(record).toMatchObject({requester_id: requester.id, name: `team-${displayName.toLowerCase().replace(/ /g, '-')}`, purpose: 'From the modal', channel_type: 'P'});
});
