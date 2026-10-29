// With the approval channel archived, the plugin can't post there (#61): it
// logs an error and sends the card to every System Admin as a direct
// message instead. Archiving the shared approval channel would break other
// specs, so this file gets a server of its own.
import {cardActions, click, pendingIds, postsMentioning, reloadPost, submitAdmin} from '../helpers/channel';
import {expect, test} from '../helpers/fixtures';
import {ADMIN, DEFAULT_PLUGIN_CONFIG, cardRequestId, uid} from '../helpers/requesthub';

test.use({
    hubOptions: {name: 'approval-archived', pluginConfig: DEFAULT_PLUGIN_CONFIG},
    allowedPluginErrors: [/An approval channel name is used by a channel the plugin can't use/],
});

test('an archived approval channel sends Channel Admin cards to System Admins by direct message', async ({hub}) => {
    const admin = await hub.client(ADMIN);
    const approvalChannelId = await hub.approvalChannelId();
    await admin.deleteChannel(approvalChannelId);
    try {
        const requester = await hub.createUser();
        const nominee = await hub.createUser();
        const name = uid('ca');
        const channel = await admin.createChannel({team_id: hub.teamId, name, display_name: `CA ${name}`, type: 'O'});
        await admin.addToChannel(requester.id, channel.id);
        await admin.addToChannel(nominee.id, channel.id);

        expect(await submitAdmin(hub, requester.username, channel.id, [nominee.username])).toEqual({
            message: "Your Channel Admin request has been submitted for approval. You'll be notified once an admin responds.",
        });
        expect(await postsMentioning(hub, approvalChannelId, `(${name})`)).toEqual([]);

        const card = await hub.findDM(ADMIN, `(${name})`);
        expect(cardActions(card)).toEqual(['approve', 'deny']);
        const requestId = cardRequestId(card);
        expect(await hub.kv(`admin_request_${requestId}`)).toMatchObject({channel_id: channel.id, nominee_ids: [nominee.id]});
        expect((await hub.kv<{approval_post_id?: string}>(`admin_request_${requestId}`))!.approval_post_id).toBeUndefined();

        // The requester still gets a ticket, and the DM copy approves.
        await hub.findDM(requester.username, `(${name})`);
        expect(await click(hub, ADMIN, card, 'approve')).toEqual({reply: ''});
        expect((await admin.getChannelMember(channel.id, nominee.id)).scheme_admin).toBe(true);
        expect((await reloadPost(hub, card.id)).message).toBe(`✅ Approved by @${ADMIN} for @${requester.username}. @${nominee.username} promoted to Channel Admin in ~${name}.`);
        expect(await pendingIds(hub, 'admin', requester.id)).toEqual([]);
    } finally {
        await admin.unarchiveChannel(approvalChannelId);
    }
});
