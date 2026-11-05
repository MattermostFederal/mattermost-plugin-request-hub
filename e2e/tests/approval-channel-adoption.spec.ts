// A private channel with an approval channel's name that the plugin's bot
// didn't create (an upgraded server, a recreated bot) is used when a System
// Admin or a bot created it. One a regular user created is not: a user could
// otherwise pre-create the name and read every card. Renaming the shared
// approval channel would break other specs, so this file gets a server of
// its own.
import {postsMentioning, submitAdmin} from '../helpers/channel';
import {expect, test} from '../helpers/fixtures';
import {ADMIN, APPROVAL_CHANNEL, DEFAULT_PLUGIN_CONFIG, HUB_BOT, cardRequestId, uid, type RequestHub} from '../helpers/requesthub';

test.use({
    hubOptions: {name: 'approval-adoption', pluginConfig: DEFAULT_PLUGIN_CONFIG},
    allowedPluginErrors: [/An approval channel name is used by a channel the plugin can't use/],
});

// Moves the current approval channel out of the way under another name.
async function renameApprovalChannel(hub: RequestHub) {
    const admin = await hub.client(ADMIN);
    const current = await admin.getChannelByName(hub.teamId, APPROVAL_CHANNEL);
    await admin.patchChannel(current.id, {name: uid('old-approvals')});
}

// Submits a Channel Admin request and returns the channel name it mentions.
async function submitChannelAdmin(hub: RequestHub): Promise<string> {
    const admin = await hub.client(ADMIN);
    const requester = await hub.createUser();
    const nominee = await hub.createUser();
    const name = uid('ca');
    const channel = await admin.createChannel({team_id: hub.teamId, name, display_name: `CA ${name}`, type: 'O'});
    await admin.addToChannel(requester.id, channel.id);
    await admin.addToChannel(nominee.id, channel.id);
    expect(await submitAdmin(hub, requester.username, channel.id, [nominee.username])).toMatchObject({message: expect.stringContaining('submitted')});
    return name;
}

test.describe.serial('approval channel adoption', () => {
    test('a private channel a System Admin made with the name is adopted', async ({hub}) => {
        const admin = await hub.client(ADMIN);
        await renameApprovalChannel(hub);
        const made = await admin.createChannel({team_id: hub.teamId, name: APPROVAL_CHANNEL, display_name: 'Approvals by hand', type: 'P'});

        const name = await submitChannelAdmin(hub);
        await expect.poll(async () => (await postsMentioning(hub, made.id, `(${name})`)).length).toBe(1);
        const bot = await admin.getUserByUsername(HUB_BOT);
        expect((await admin.getChannelMember(made.id, bot.id)).user_id).toBe(bot.id);
    });

    test('a private channel a regular user made with the name is not used', async ({hub}) => {
        await renameApprovalChannel(hub);
        const squatter = await hub.createUser();
        const theirs = await (await hub.client(squatter.username)).createChannel({team_id: hub.teamId, name: APPROVAL_CHANNEL, display_name: 'Not approvals', type: 'P'});

        const name = await submitChannelAdmin(hub);
        const card = await hub.findDM(ADMIN, `(${name})`);
        expect(cardRequestId(card)).toBeTruthy();
        expect(await postsMentioning(hub, theirs.id, `(${name})`)).toEqual([]);
    });
});
