// Checks the e2e helpers themselves against the real server, so a broken
// helper fails here rather than as a confusing failure in a feature spec.
import {expect, test} from '../helpers/fixtures';
import {ADMIN, uid} from '../helpers/requesthub';

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
    await hub.findCard(name);
    await hub.findDM(requester.username, name);

    type Stored = {id: string; requester_id: string; display_name: string; created_at: number};
    let key = '';
    for (const k of await hub.kvKeys('request_')) {
        const r = await hub.kv<Stored>(k);
        if (r?.display_name === name) {
            key = k;
        }
    }
    expect(key, `KV record for ${name}`).not.toBe('');
    const before = (await hub.kv<Stored>(key))!;
    expect(before.requester_id).toBe(requester.id);

    const pending = await hub.kv<unknown>(`pending_channel_${requester.id}`);
    expect(JSON.stringify(pending)).toContain(before.id);

    await hub.rewindRequest(key, 2);
    const after = (await hub.kv<Stored>(key))!;
    expect(before.created_at - after.created_at).toBe(2 * 24 * 60 * 60 * 1000);
    expect({...after, created_at: 0}).toEqual({...before, created_at: 0});

    await expect(hub.rewindRequest('request_missing', 1)).rejects.toThrow(/no KV record/);
});
