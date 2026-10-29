// Channel Admin requests: promoting existing channel members, through the
// REST route, the dialog and the inline slash command, and every way a
// request is refused at submit or at approval.
import type {Channel} from '@mattermost/types/channels';

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
    submitAdmin,
    submitAdminDialog,
} from '../helpers/channel';
import {expect, test} from '../helpers/fixtures';
import {ADMIN, HUB_BOT, cardRequestId, uid, type RequestHub, type TestUser} from '../helpers/requesthub';

const SUBMITTED = "Your Channel Admin request has been submitted for approval. You'll be notified once an admin responds.";
const NO_PERMISSION = "You don't have permission to approve or deny this Channel Admin request. Contact a Team Admin of the channel's team or a System Admin.";
const OFF = 'Channel Admin requests are currently disabled by an administrator.';
const NO_CHANNEL = 'channel not found, or you are not a member of it';

// A new channel in teamId (team "test" by default) with the given members.
async function makeChannel(hub: RequestHub, members: string[], options: {type?: 'O' | 'P'; teamId?: string} = {}): Promise<Channel> {
    const admin = await hub.client(ADMIN);
    const name = uid('ca');
    const channel = await admin.createChannel({team_id: options.teamId ?? hub.teamId, name, display_name: `CA ${name}`, type: options.type ?? 'O'});
    for (const id of members) {
        await admin.addToChannel(id, channel.id);
    }
    return channel;
}

async function isChannelAdmin(hub: RequestHub, channelId: string, userId: string): Promise<boolean> {
    return (await (await hub.client(ADMIN)).getChannelMember(channelId, userId)).scheme_admin === true;
}

async function expectNoPromotion(hub: RequestHub, channelId: string, users: TestUser[]) {
    for (const user of users) {
        expect(await isChannelAdmin(hub, channelId, user.id), `@${user.username} is Channel Admin`).toBe(false);
    }
}

// Submits over REST and returns the request's card and ID.
async function pendingAdminRequest(hub: RequestHub, requester: TestUser, channel: Channel, nominees: TestUser[]) {
    expect(await submitAdmin(hub, requester.username, channel.id, nominees.map((n) => n.username))).toEqual({message: SUBMITTED});
    const card = await hub.findCard(`(${channel.name})`);
    return {card, requestId: cardRequestId(card)};
}

test.describe('submit', () => {
    test('request for yourself over REST: card, ticket DM, approval, promotion post, audit and pending list', async ({hub}) => {
        const requester = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [requester]);

        // The card mentions everyone who can approve it.
        expect(card.channel_id).toBe(await hub.approvalChannelId());
        expect(card.message).toMatch(/ — a Channel Admin request needs your review\.$/);
        expect(card.message).toContain(`@${ADMIN}`);
        expect(card.message).toContain('@tadmin');
        expect((card.props.attachments as Array<{text: string}>)[0].text).toBe(`**Proposed Channel Admins:** @${requester.username}`);
        expect(cardActions(card)).toEqual(['approve', 'deny']);
        expect(await hub.kv(`admin_request_${requestId}`)).toMatchObject({requester_id: requester.id, channel_id: channel.id, nominee_ids: [requester.id]});
        expect(await pendingIds(hub, 'admin', requester.id)).toEqual([requestId]);

        const dm = await hub.findDM(requester.username, channel.name);
        expect(dm.message).toBe('Your Channel Admin request has been submitted for approval. Reply to this thread to add context — your reply will be forwarded to the reviewers.');

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: ''});
        expect(await isChannelAdmin(hub, channel.id, requester.id)).toBe(true);

        const resolved = await reloadPost(hub, card.id);
        expect(resolved.message).toBe(`✅ Approved by @${ADMIN} for @${requester.username}. @${requester.username} promoted to Channel Admin in ~${channel.name}.`);
        expect(cardActions(resolved)).toEqual([]);

        const ticket = await reloadPost(hub, dm.id);
        expect((ticket.props.attachments as Array<{title: string; text: string}>)[0]).toMatchObject({
            title: 'Channel Admin Request Approved',
            text: `Approved by @${ADMIN}. @${requester.username} is now Channel Admin in ~${channel.name}.`,
        });
        expect(await dmThread(hub, requester.username, dm.id)).toEqual([dm.message, `✅ **Approved** by @${ADMIN}.`]);

        expect((await postsMentioning(hub, channel.id, 'is now a Channel Admin')).map((p) => p.message)).toEqual([
            `@${requester.username} is now a Channel Admin for this channel. (Approved by @${ADMIN}.)`,
        ]);
        const audit = await hub.findAudit(`~${channel.name} (requested by`);
        expect((audit.props.attachments as Array<{title: string; text: string}>)[0]).toMatchObject({
            title: '✅ Approved',
            text: expect.stringContaining(`@${ADMIN} promoted @${requester.username} to Channel Admin in ~${channel.name} (requested by @${requester.username})`),
        });

        expect(await hub.kv(`admin_request_${requestId}`)).toBeUndefined();
        expect(await pendingIds(hub, 'admin', requester.id)).toEqual([]);
    });

    test('inline slash in the target channel nominates several members, and approval promotes each', async ({hub}) => {
        const requester = await hub.createUser();
        const a = await hub.createUser();
        const b = await hub.createUser();
        const bystander = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, a.id, b.id, bystander.id], {type: 'P'});

        expect(await hub.slash(requester.username, `channel-admin @${a.username}, ${b.username} @${a.username}`, channel.name)).toBe(SUBMITTED);
        const card = await hub.findCard(`(${channel.name})`);
        const requestId = cardRequestId(card);
        expect(await hub.kv(`admin_request_${requestId}`)).toMatchObject({channel_id: channel.id, nominee_ids: [a.id, b.id]});

        expect(await click(hub, 'tadmin', card, 'approve')).toEqual({reply: ''});
        expect(await isChannelAdmin(hub, channel.id, a.id)).toBe(true);
        expect(await isChannelAdmin(hub, channel.id, b.id)).toBe(true);
        await expectNoPromotion(hub, channel.id, [requester, bystander]);
        expect((await reloadPost(hub, card.id)).message).toBe(`✅ Approved by @tadmin for @${requester.username}. @${a.username}, @${b.username} promoted to Channel Admin in ~${channel.name}.`);
        expect(await pendingIds(hub, 'admin', requester.id)).toEqual([]);
    });

    test('a bot that is a channel member can be nominated and promoted', async ({hub}) => {
        // Bot creation is off on the test server, so use the plugin's own
        // bot, which is already on the team. The channel is thrown away.
        const requester = await hub.createUser();
        const bot = await (await hub.client(ADMIN)).getUserByUsername(HUB_BOT);
        expect(bot.is_bot).toBe(true);
        const channel = await makeChannel(hub, [requester.id, bot.id]);

        const {card} = await pendingAdminRequest(hub, requester, channel, [{username: bot.username, id: bot.id}]);
        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: ''});
        expect(await isChannelAdmin(hub, channel.id, bot.id)).toBe(true);
    });

    test('no nominees, or an unknown or invalid one, is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id]);
        const ghost = uid('ghost');

        expect(await submitAdmin(hub, requester.username, channel.id, [])).toEqual({error: 'pick at least one person to make a Channel Admin'});
        expect(await submitAdmin(hub, requester.username, channel.id, [`@${ghost}`])).toEqual({error: `unknown user: ${ghost}`});
        expect(await hub.slash(requester.username, `channel-admin @${ghost}`, channel.name)).toBe(`unknown user: ${ghost}`);
        expect(await hub.slash(requester.username, 'channel-admin ,', channel.name)).toBe('Name at least one channel member: `/mattermost-request-hub channel-admin @user [@user ...]`');
        expect(await submitAdminDialog(hub, requester.username, channel.id, [])).toBe('pick at least one person to make a Channel Admin');
        expect(await submitAdminDialog(hub, requester.username, channel.id, ['not-an-id'])).toBe('Channel Admin nominees must be picked from the list');
        await expectNothingStored(hub, 'admin', requester.id, `(${channel.name})`);
        await expectNoPromotion(hub, channel.id, [requester]);
    });

    test('a missing channel, or one the requester isn\'t in or can\'t see, is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const open = await makeChannel(hub, [nominee.id]);
        const secret = await makeChannel(hub, [nominee.id], {type: 'P'});

        expect(await submitAdmin(hub, requester.username, '', [nominee.username])).toEqual({error: 'a channel is required'});
        expect(await submitAdmin(hub, requester.username, open.id, [nominee.username])).toEqual({error: NO_CHANNEL});
        expect(await submitAdmin(hub, requester.username, secret.id, [nominee.username])).toEqual({error: NO_CHANNEL});
        expect(await submitAdmin(hub, requester.username, 'x'.repeat(26), [nominee.username])).toEqual({error: NO_CHANNEL});
        expect(await submitAdminDialog(hub, requester.username, secret.id, [nominee.id])).toBe(NO_CHANNEL);
        for (const channel of [open, secret]) {
            await expectNothingStored(hub, 'admin', requester.id, `(${channel.name})`);
            await expectNoPromotion(hub, channel.id, [nominee]);
        }
    });

    test('a nominee who isn\'t in the channel is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const member = await hub.createUser();
        const outsider = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, member.id]);

        const error = 'everyone you nominate must already be a member of the channel';
        expect(await submitAdmin(hub, requester.username, channel.id, [member.username, outsider.username])).toEqual({error});
        expect(await hub.slash(requester.username, `channel-admin @${outsider.username}`, channel.name)).toBe(error);
        await expectNothingStored(hub, 'admin', requester.id, `(${channel.name})`);
        await expectNoPromotion(hub, channel.id, [requester, member]);
    });

    test('an archived channel or a direct message is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const other = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, other.id]);
        await (await hub.client(ADMIN)).deleteChannel(channel.id);
        expect(await submitAdmin(hub, requester.username, channel.id, [other.username])).toEqual({error: 'that channel is archived'});
        await expectNothingStored(hub, 'admin', requester.id, `(${channel.name})`);

        const dm = await (await hub.client(requester.username)).createDirectChannel([requester.id, other.id]);
        expect(await submitAdmin(hub, requester.username, dm.id, [other.username])).toEqual({error: 'Channel Admins can only be requested for public or private channels'});
        await expectNothingStored(hub, 'admin', requester.id, other.username);
    });

    test('with Channel Admin requests turned off, REST, dialog and slash command refuse', async ({hub}) => {
        const requester = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id]);
        await hub.withPluginConfig({allowchanneladminrequests: false}, async () => {
            expect(await submitAdmin(hub, requester.username, channel.id, [requester.username])).toEqual({error: OFF});
            expect(await submitAdminDialog(hub, requester.username, channel.id, [requester.id])).toBe(OFF);
            expect(await hub.slash(requester.username, `channel-admin @${requester.username}`, channel.name)).toBe(OFF);
            expect(await hub.slash(requester.username, 'channel-admin', channel.name)).toBe(OFF);
        });
        await expectNothingStored(hub, 'admin', requester.id, `(${channel.name})`);
        await expectNoPromotion(hub, channel.id, [requester]);
    });

    test('a second pending request for the same people in the same channel is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const a = await hub.createUser();
        const b = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, a.id, b.id]);
        const {requestId} = await pendingAdminRequest(hub, requester, channel, [a, b]);

        expect(await submitAdmin(hub, requester.username, channel.id, [b.username, a.username])).toEqual({error: 'you already have a pending Channel Admin request for these people in this channel'});
        expect(await postsMentioning(hub, await hub.approvalChannelId(), `(${channel.name})`)).toHaveLength(1);
        expect(await pendingIds(hub, 'admin', requester.id)).toEqual([requestId]);
    });
});

test.describe('approval', () => {
    test('deny: card, ticket DM and audit, and nobody is promoted', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, nominee.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);
        const dm = await hub.findDM(requester.username, channel.name);

        expect(await click(hub, ADMIN, card, 'deny')).toEqual({reply: ''});
        await expectNoPromotion(hub, channel.id, [requester, nominee]);

        expect((await reloadPost(hub, card.id)).message).toBe(`❌ Denied by @${ADMIN} for @${requester.username}. Channel Admin for @${nominee.username} in ~${channel.name}.`);
        expect((await reloadPost(hub, dm.id)).props.attachments).toMatchObject([{title: 'Channel Admin Request Denied'}]);
        expect(await dmThread(hub, requester.username, dm.id)).toEqual([dm.message, `❌ **Denied** by @${ADMIN}.`]);
        const audit = await hub.findAudit(`for @${nominee.username} in ~${channel.name}`);
        expect((audit.props.attachments as Array<{title: string; text: string}>)[0]).toMatchObject({
            title: '❌ Denied',
            text: expect.stringContaining(`@${ADMIN} denied a Channel Admin request for @${nominee.username} in ~${channel.name}`),
        });
        expect(await postsMentioning(hub, channel.id, 'is now a Channel Admin')).toEqual([]);
        expect(await hub.kv(`admin_request_${requestId}`)).toBeUndefined();
        expect(await pendingIds(hub, 'admin', requester.id)).toEqual([]);
    });

    test('a nominee who is an approver can\'t approve their own promotion', async ({hub}) => {
        const requester = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, hub.userIds.tadmin]);
        const tadmin = {username: 'tadmin', id: hub.userIds.tadmin};
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [tadmin]);

        expect(await click(hub, 'tadmin', card, 'approve')).toEqual({reply: "You can't approve a request that nominates you. Another approver has to review it."});
        expect(await hub.slash('tadmin', `approve ${requestId}`)).toBe("You can't approve a request that nominates you. Another approver has to review it.");
        await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
        await expectNoPromotion(hub, channel.id, [tadmin]);

        // Another approver can.
        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: ''});
        expect(await isChannelAdmin(hub, channel.id, tadmin.id)).toBe(true);
    });

    test('a Team Admin can\'t approve their own request', async ({hub}) => {
        const requester = await hub.createUser({teamAdmin: true});
        const nominee = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, nominee.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);

        expect(await hub.slash(requester.username, `approve ${requestId}`)).toBe("You can't approve your own request. Another approver has to review it.");
        await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
        await expectNoPromotion(hub, channel.id, [nominee]);
    });

    test('a regular member in the approval channel can neither approve nor deny', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const bystander = await hub.createUser();
        await joinApprovalChannel(hub, bystander.id);
        const channel = await makeChannel(hub, [requester.id, nominee.id, bystander.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);

        expect(await click(hub, bystander.username, card, 'approve')).toEqual({reply: NO_PERMISSION});
        expect(await click(hub, bystander.username, card, 'deny')).toEqual({reply: NO_PERMISSION});
        expect(await click(hub, requester.username, card, 'approve')).toEqual({reply: '', status: 403});
        await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
        await expectNoPromotion(hub, channel.id, [nominee]);
    });

    test('an approval-team Team Admin can\'t approve for another team\'s channel, even with AllowTeamAdminApprovers on', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const team = await createTeam(hub, [requester.id, nominee.id]);
        const channel = await makeChannel(hub, [requester.id, nominee.id], {teamId: team.id});
        await hub.withPluginConfig({allowteamadminapprovers: true}, async () => {
            const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);
            expect(card.message).toMatch(new RegExp(`^For Team: ${team.displayName} — `));

            expect(await click(hub, 'tadmin', card, 'approve')).toEqual({reply: NO_PERMISSION});
            await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
            await expectNoPromotion(hub, channel.id, [nominee]);
        });
    });

    test('a channel archived before approval blocks it; Deny still closes the request', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, nominee.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);
        await (await hub.client(ADMIN)).deleteChannel(channel.id);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: `~${channel.name} has been archived. Deny this request instead.`});
        await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
        await expectNoPromotion(hub, channel.id, [nominee]);

        expect(await click(hub, ADMIN, card, 'deny')).toEqual({reply: ''});
        expect(await pendingIds(hub, 'admin', requester.id)).toEqual([]);
    });

    test('a requester who left the channel blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, nominee.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);
        await (await hub.client(ADMIN)).removeFromChannel(requester.id, channel.id);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: 'The requester is no longer a member of the channel. Deny this request instead.'});
        await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
        await expectNoPromotion(hub, channel.id, [nominee]);
    });

    test('a deactivated requester blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, nominee.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);
        await (await hub.client(ADMIN)).updateUserActive(requester.id, false);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: "The requester's account is no longer active. Deny this request instead."});
        await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
        await expectNoPromotion(hub, channel.id, [nominee]);
    });

    test('a nominee who left the channel blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const stays = await hub.createUser();
        const leaves = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, stays.id, leaves.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [stays, leaves]);
        await (await hub.client(ADMIN)).removeFromChannel(leaves.id, channel.id);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: `@${leaves.username} is no longer a member of the channel. Deny this request instead.`});
        await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
        await expectNoPromotion(hub, channel.id, [stays]);
    });

    test('a deactivated nominee blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, nominee.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);
        await (await hub.client(ADMIN)).updateUserActive(nominee.id, false);

        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: `@${nominee.username} is deactivated. Deny this request instead.`});
        await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
        await expectNoPromotion(hub, channel.id, [nominee]);
    });

    test('with Channel Admin requests turned off, Approve is refused but Deny still closes the request', async ({hub}) => {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const channel = await makeChannel(hub, [requester.id, nominee.id]);
        const {card, requestId} = await pendingAdminRequest(hub, requester, channel, [nominee]);
        await hub.withPluginConfig({allowchanneladminrequests: false}, async () => {
            expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: 'Requests of this type are turned off. Deny this request instead.'});
            await expectStillPending(hub, 'admin', card.id, requestId, requester.id);
            await expectNoPromotion(hub, channel.id, [nominee]);

            expect(await click(hub, ADMIN, card, 'deny')).toEqual({reply: ''});
        });
        expect(await hub.kv(`admin_request_${requestId}`)).toBeUndefined();
        expect(await pendingIds(hub, 'admin', requester.id)).toEqual([]);
    });
});
