// Checks the e2e helpers themselves against the real server, so a broken
// helper fails here rather than as a confusing failure in a feature spec.
import {expect, test} from '../helpers/fixtures';
import {ADMIN, cardRequestId, uid} from '../helpers/requesthub';

test('createUser makes a team member who can run the slash command', async ({hub}) => {
    const user = await hub.createUser();
    const admin = await hub.client(ADMIN);
    await admin.getTeamMember(hub.teamId, user.id);

    const help = await hub.slash(user.username, 'help');
    expect(help).toContain('Mattermost Request Hub');
    expect(help).toContain('`/mattermost-request-hub team-admin`');
});

test('withPluginConfig applies at once and restores the old config', async ({hub}) => {
    const user = await hub.createUser();
    await hub.withPluginConfig({allowteamadminrequests: false}, async () => {
        expect(await hub.slash(user.username, 'help')).not.toContain('`/mattermost-request-hub team-admin`');
    });
    expect(await hub.slash(user.username, 'help')).toContain('`/mattermost-request-hub team-admin`');
});

test('kv, kvKeys and rewindRequest read and edit a stored request', async ({hub}) => {
    const requester = await hub.createUser();
    const name = uid('harness-');
    await hub.pluginFetch(requester.username, '/api/v1/create', {team_id: hub.teamId, display_name: name, prefix: 'proj-', name: '', channel_type: 'O', members: [], admin_members: []});
    const requestId = cardRequestId(await hub.findCard(name));
    await hub.findDM(requester.username, name);

    type Stored = {id: string; requester_id: string; display_name: string; created_at: number};
    type Entry = {id: string; created_at: number};
    expect(await hub.kvKeys('request_')).toContain(`request_${requestId}`);
    const before = (await hub.kv<Stored>(`request_${requestId}`))!;
    expect(before).toMatchObject({id: requestId, requester_id: requester.id, display_name: name});
    const listKey = `pending_channel_${requester.id}`;
    const [entryBefore] = (await hub.kv<Entry[]>(listKey))!;
    expect(entryBefore.id).toBe(requestId);

    await hub.rewindRequest('channel', requester.id, requestId, 2);
    const twoDays = 2 * 24 * 60 * 60 * 1000;
    const after = (await hub.kv<Stored>(`request_${requestId}`))!;
    expect(before.created_at - after.created_at).toBe(twoDays);
    expect({...after, created_at: 0}).toEqual({...before, created_at: 0});
    const [entryAfter] = (await hub.kv<Entry[]>(listKey))!;
    expect(entryBefore.created_at - entryAfter.created_at).toBe(twoDays);

    await expect(hub.rewindRequest('channel', requester.id, 'missing', 1)).rejects.toThrow(/no KV record/);
});

test('findAudit finds the audit post for a decision', async ({hub}) => {
    const requester = await hub.createUser();
    const name = uid('harness-audit-');
    await hub.pluginFetch(requester.username, '/api/v1/create', {team_id: hub.teamId, display_name: name, prefix: 'proj-', name: '', channel_type: 'O', members: [], admin_members: []});
    const card = await hub.findCard(name);
    await (await hub.client(ADMIN)).doPostAction(card.id, 'deny');
    await hub.findAudit(name);
});
