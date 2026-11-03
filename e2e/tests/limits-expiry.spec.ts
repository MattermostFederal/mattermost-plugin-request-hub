// Per-user pending limits, duplicate requests, expiry and pruning of the
// pending lists (server/pending.go, decision.go expireRequest).
import type {Post} from '@mattermost/types/posts';

import {expect, test} from '../helpers/fixtures';
import {
    countCards,
    countDMs,
    deleteRecord,
    duplicateText,
    limitText,
    pendingIds,
    storedRequests,
    submitChannel,
    submitTeam,
    SUBMITTED,
    TEAM_SUBMITTED,
} from '../helpers/limits';
import {ADMIN, cardRequestId, REQUEST_CHANNELS, uid, type RequestHub, type TestUser} from '../helpers/requesthub';

// The trailing x keeps one name from being a prefix of another (uid ends
// in a counter), so counting posts that mention a name is exact.
const reqName = () => `${uid('lim')}x`;

// Submits a channel request that must go through and returns its ID.
async function submitOk(hub: RequestHub, user: TestUser, name: string): Promise<string> {
    expect(await submitChannel(hub, user.username, name)).toEqual({message: SUBMITTED});
    return cardRequestId(await hub.findCard(name));
}

// A refused channel request left nothing behind: no card, no ticket DM, no
// stored record, no channel, and the pending list is still pending.
async function expectNothingFor(hub: RequestHub, user: TestUser, name: string, pending: string[]) {
    expect(await countCards(hub, name), `cards for ${name}`).toBe(0);
    expect(await countDMs(hub, user.username, name), `ticket DMs for ${name}`).toBe(0);
    const records = await storedRequests(hub, 'channel', user.id);
    expect(records.filter((r) => r.display_name === name)).toEqual([]);
    expect(await pendingIds(hub, 'channel', user.id)).toEqual(pending);
    await expect((await hub.client(ADMIN)).getChannelByName(hub.teamId, `proj-${name}`)).rejects.toMatchObject({status_code: 404});
}

// Plugin log lines with msg logged since line start, decoded. Field values
// are logged as strings.
const pluginLogs = (hub: RequestHub, start: number, msg: string) => hub.mm.logLines().slice(start).
    map((l) => JSON.parse(l) as Record<string, string>).
    filter((l) => l.msg === msg);

const attachmentsOf = (post: Post) => post.props?.attachments as unknown[] | undefined;

test('the limit refuses one more request over REST and the slash command, and leaves nothing behind', async ({hub}) => {
    const user = await hub.createUser();
    await hub.withPluginConfig({maxpendingrequestsperuser: '2'}, async () => {
        const first = await submitOk(hub, user, reqName());
        const second = await submitOk(hub, user, reqName());

        const overRest = reqName();
        expect(await submitChannel(hub, user.username, overRest)).toEqual({error: limitText(2)});
        await expectNothingFor(hub, user, overRest, [first, second]);

        const overSlash = reqName();
        expect(await hub.slash(user.username, `channel proj- ${overSlash}`)).toBe(limitText(2));
        await expectNothingFor(hub, user, overSlash, [first, second]);

        expect((await storedRequests(hub, 'channel', user.id)).map((r) => r.id).sort()).toEqual([first, second].sort());
    });
});

test('each request type has its own limit', async ({hub}) => {
    const user = await hub.createUser();
    await hub.withPluginConfig({maxpendingrequestsperuser: '1'}, async () => {
        const first = await submitOk(hub, user, reqName());
        const refused = reqName();
        expect(await submitChannel(hub, user.username, refused)).toEqual({error: limitText(1)});
        await expectNothingFor(hub, user, refused, [first]);

        // The channel list is full, but a team request still goes through.
        const team = reqName();
        expect(await submitTeam(hub, user.username, team)).toEqual({message: TEAM_SUBMITTED});
        const card = await hub.findCardIn(REQUEST_CHANNELS.team, team);
        expect(await pendingIds(hub, 'team', user.id)).toEqual([cardRequestId(card)]);

        // And now that one is full too.
        const team2 = reqName();
        expect(await submitTeam(hub, user.username, team2)).toEqual({error: limitText(1)});
        expect(await countCards(hub, team2, REQUEST_CHANNELS.team)).toBe(0);
        expect((await storedRequests(hub, 'team', user.id)).map((r) => r.id)).toEqual([cardRequestId(card)]);
        expect(await pendingIds(hub, 'team', user.id)).toEqual([cardRequestId(card)]);
    });
});

test('a limit of 0 means no limit', async ({hub}) => {
    const user = await hub.createUser();
    await hub.withPluginConfig({maxpendingrequestsperuser: '0'}, async () => {
        // One more than the default limit of 5.
        for (let i = 0; i < 6; i++) {
            expect(await submitChannel(hub, user.username, reqName())).toEqual({message: SUBMITTED});
        }
    });
    expect(await pendingIds(hub, 'channel', user.id)).toHaveLength(6);
    expect(await storedRequests(hub, 'channel', user.id)).toHaveLength(6);
});

test('a duplicate request is refused; another user may request the same channel', async ({hub}) => {
    const user = await hub.createUser();
    const other = await hub.createUser();
    const name = reqName();
    const id = await submitOk(hub, user, name);

    expect(await submitChannel(hub, user.username, name)).toEqual({error: duplicateText(`proj-${name}`)});
    expect(await hub.slash(user.username, `channel proj- ${name}`)).toBe(duplicateText(`proj-${name}`));
    expect(await countCards(hub, name)).toBe(1);
    expect(await countDMs(hub, user.username, name)).toBe(1);
    expect((await storedRequests(hub, 'channel', user.id)).map((r) => r.id)).toEqual([id]);
    expect(await pendingIds(hub, 'channel', user.id)).toEqual([id]);

    const otherId = await submitOk(hub, other, name);
    expect(otherId).not.toBe(id);
    expect(await countCards(hub, name)).toBe(2);
    expect(await pendingIds(hub, 'channel', other.id)).toEqual([otherId]);
});

test('a double submit stores one request and refuses the other as a duplicate', async ({hub}) => {
    const user = await hub.createUser();
    const name = reqName();
    const results = await Promise.all([submitChannel(hub, user.username, name), submitChannel(hub, user.username, name)]);
    expect(results).toEqual(expect.arrayContaining([{message: SUBMITTED}, {error: duplicateText(`proj-${name}`)}]));

    const card = await hub.findCard(name);
    const records = await storedRequests(hub, 'channel', user.id);
    expect(records.map((r) => r.id)).toEqual([cardRequestId(card)]);
    expect(await pendingIds(hub, 'channel', user.id)).toEqual([cardRequestId(card)]);
    expect(await countCards(hub, name)).toBe(1);
    expect(await countDMs(hub, user.username, name)).toBe(1);
});

test('approving or denying a request frees its slot', async ({hub}) => {
    const user = await hub.createUser();
    const admin = await hub.client(ADMIN);
    await hub.withPluginConfig({maxpendingrequestsperuser: '1'}, async () => {
        const approved = reqName();
        await submitOk(hub, user, approved);
        expect(await submitChannel(hub, user.username, reqName())).toEqual({error: limitText(1)});

        await admin.doPostAction((await hub.findCard(approved)).id, 'approve');
        expect(await hub.waitForChannel(`proj-${approved}`)).toBeTruthy();
        expect(await hub.kv(`pending_channel_${user.id}`)).toBeUndefined();

        const denied = reqName();
        const deniedId = await submitOk(hub, user, denied);
        expect(await pendingIds(hub, 'channel', user.id)).toEqual([deniedId]);
        const card = await hub.findCard(denied);
        await admin.doPostAction(card.id, 'deny');
        await expect.poll(async () => (await admin.getPost(card.id)).message).toContain('Denied');
        expect(await hub.kv(`pending_channel_${user.id}`)).toBeUndefined();
        expect(await storedRequests(hub, 'channel', user.id)).toEqual([]);

        await submitOk(hub, user, reqName());
    });
});

test('approving an expired request closes it as expired', async ({hub}) => {
    const user = await hub.createUser();
    const admin = await hub.client(ADMIN);
    const name = reqName();
    const id = await submitOk(hub, user, name);
    const card = await hub.findCard(name);
    const ticket = await hub.findDM(user.username, name);
    await hub.rewindRequest('channel', user.id, id, 31);

    await admin.doPostAction(card.id, 'approve');

    const outcome = '⌛ Expired: not reviewed within 30 days.';
    await expect.poll(async () => (await admin.getPost(card.id)).message).toBe(outcome);
    expect(attachmentsOf(await admin.getPost(card.id)) ?? []).toEqual([]);

    const root = await hub.findDM(user.username, 'Channel Request Expired');
    expect(root.id).toBe(ticket.id);
    expect(JSON.stringify(root.props.attachments)).toContain('expired before it was reviewed');
    const reply = await hub.findDM(user.username, 'not reviewed within 30 days');
    expect(reply.root_id).toBe(ticket.id);
    expect(reply.message).toBe('⌛ **Expired**: not reviewed within 30 days.');

    const audit = JSON.stringify((await hub.findAudit(name)).props.attachments);
    expect(audit).toContain(`@${ADMIN} closed channel request`);
    expect(audit).toContain(`from @${user.username}, which was not reviewed within 30 days`);

    expect(await hub.kv(`request_${id}`)).toBeUndefined();
    expect(await hub.kv(`pending_channel_${user.id}`)).toBeUndefined();
    await expect(admin.getChannelByName(hub.teamId, `proj-${name}`)).rejects.toMatchObject({status_code: 404});
});

test('deciding an expired request by slash command replies that it expired', async ({hub}) => {
    const user = await hub.createUser();
    const admin = await hub.client(ADMIN);
    const name = reqName();
    const id = await submitOk(hub, user, name);
    const card = await hub.findCard(name);
    await hub.rewindRequest('channel', user.id, id, 31);

    // Deny goes the same way as approve once the request has expired.
    expect(await hub.slash(ADMIN, `deny ${id}`)).toBe('This request has expired.');
    await expect.poll(async () => (await admin.getPost(card.id)).message).toBe('⌛ Expired: not reviewed within 30 days.');
    expect(await hub.kv(`request_${id}`)).toBeUndefined();
    expect(await pendingIds(hub, 'channel', user.id)).toEqual([]);

    expect(await hub.slash(ADMIN, `approve ${id}`)).toBe('This request has already been handled.');
    await expect(admin.getChannelByName(hub.teamId, `proj-${name}`)).rejects.toMatchObject({status_code: 404});
});

test('expired requests stop counting toward the limit and no longer block a duplicate', async ({hub}) => {
    const user = await hub.createUser();
    await hub.withPluginConfig({maxpendingrequestsperuser: '1'}, async () => {
        const name = reqName();
        const oldId = await submitOk(hub, user, name);
        await hub.rewindRequest('channel', user.id, oldId, 31);

        // Same name, at a limit of 1: both refusals would apply if the
        // expired request still counted.
        expect(await submitChannel(hub, user.username, name)).toEqual({message: SUBMITTED});
        const newId = cardRequestId(await hub.findCard(name));
        expect(newId).not.toBe(oldId);

        // The expired entry stays in the list until it is pruned.
        expect(await pendingIds(hub, 'channel', user.id)).toEqual([oldId, newId]);
        expect(await submitChannel(hub, user.username, reqName())).toEqual({error: limitText(1)});
    });
});

test('a request expiry of 0 means requests never expire', async ({hub}) => {
    const user = await hub.createUser();
    await hub.withPluginConfig({requestexpirydays: '0'}, async () => {
        const name = reqName();
        const id = await submitOk(hub, user, name);
        await hub.rewindRequest('channel', user.id, id, 400);
        await (await hub.client(ADMIN)).doPostAction((await hub.findCard(name)).id, 'approve');
        expect(await hub.waitForChannel(`proj-${name}`)).toBeTruthy();
        expect(await hub.kv(`pending_channel_${user.id}`)).toBeUndefined();
    });
});

test('the next submit prunes entries expired for over twice the expiry and keeps the rest uncounted', async ({hub}) => {
    const user = await hub.createUser();
    const longGone = await submitOk(hub, user, reqName());
    const recent = await submitOk(hub, user, reqName());
    await hub.rewindRequest('channel', user.id, longGone, 61);
    await hub.rewindRequest('channel', user.id, recent, 31);

    await hub.withPluginConfig({maxpendingrequestsperuser: '1'}, async () => {
        const fresh = await submitOk(hub, user, reqName());
        expect(await pendingIds(hub, 'channel', user.id)).toEqual([recent, fresh]);
    });

    // Pruning only touches the list; the record stays until someone
    // decides it, which closes it as expired.
    expect(await hub.kv(`request_${longGone}`)).toBeTruthy();
});

test('an entry whose request is gone holds its place for a minute, then stops counting', async ({hub}) => {
    const user = await hub.createUser();
    await hub.withPluginConfig({maxpendingrequestsperuser: '1'}, async () => {
        const lostId = await submitOk(hub, user, reqName());
        await deleteRecord(hub, 'channel', lostId);

        // Within the grace period the entry may belong to a submit that
        // hasn't stored its request yet.
        const held = reqName();
        expect(await submitChannel(hub, user.username, held)).toEqual({error: limitText(1)});
        await expectNothingFor(hub, user, held, [lostId]);

        // Ten minutes on, the entry is known to be orphaned and is dropped.
        await hub.mm.psql(
            `UPDATE pluginkeyvaluestore SET pvalue = convert_to(jsonb_set(convert_from(pvalue, 'UTF8')::jsonb, '{0,created_at}', ` +
            `to_jsonb((convert_from(pvalue, 'UTF8')::jsonb->0->>'created_at')::bigint - 600000))::text, 'UTF8') ` +
            `WHERE pkey = 'pending_channel_${user.id}'`,
        );
        const next = await submitOk(hub, user, reqName());
        expect(await pendingIds(hub, 'channel', user.id)).toEqual([next]);
    });
});

test('invalid limit and expiry settings fall back with a warning', async ({hub}) => {
    const user = await hub.createUser();
    const logStart = hub.logLineCount();
    await hub.withPluginConfig({maxpendingrequestsperuser: 'lots', requestexpirydays: '-3'}, async () => {
        const warnings = pluginLogs(hub, logStart, 'channel-requests: setting must be a whole number 0 or greater; using the default');
        expect(warnings).toContainEqual(expect.objectContaining({level: 'warn', setting: 'MaxPendingRequestsPerUser', value: 'lots', default: '5'}));
        expect(warnings).toContainEqual(expect.objectContaining({level: 'warn', setting: 'RequestExpiryDays', value: '-3', default: '30'}));

        // The default limit of 5 applies.
        const ids: string[] = [];
        for (let i = 0; i < 5; i++) {
            ids.push(await submitOk(hub, user, reqName()));
        }
        const refused = reqName();
        expect(await submitChannel(hub, user.username, refused)).toEqual({error: limitText(5)});
        await expectNothingFor(hub, user, refused, ids);

        // The default expiry of 30 days applies.
        await hub.rewindRequest('channel', user.id, ids[0], 29);
        expect(await submitChannel(hub, user.username, reqName())).toEqual({error: limitText(5)});
        await hub.rewindRequest('channel', user.id, ids[0], 2);
        await submitOk(hub, user, reqName());
    });
});

test('settings above their maximum are capped with a warning', async ({hub}) => {
    const user = await hub.createUser();
    const logStart = hub.logLineCount();
    await hub.withPluginConfig({maxpendingrequestsperuser: '5000', requestexpirydays: '99999'}, async () => {
        const warnings = pluginLogs(hub, logStart, 'channel-requests: setting is above its maximum; using the maximum');
        expect(warnings).toContainEqual(expect.objectContaining({level: 'warn', setting: 'MaxPendingRequestsPerUser', value: '5000', maximum: '1000'}));
        expect(warnings).toContainEqual(expect.objectContaining({level: 'warn', setting: 'RequestExpiryDays', value: '99999', maximum: '3650'}));

        const name = reqName();
        const id = await submitOk(hub, user, name);
        const card = await hub.findCard(name);
        await hub.rewindRequest('channel', user.id, id, 3651);
        expect(await hub.slash(ADMIN, `approve ${id}`)).toBe('This request has expired.');
        await expect.poll(async () => (await (await hub.client(ADMIN)).getPost(card.id)).message).toBe('⌛ Expired: not reviewed within 3650 days.');
    });
});
