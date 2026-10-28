// Starts a server with the plugin bundle from `make dist` installed, a team
// "test" and a fixed set of users, and wraps the plugin's REST routes.
import fs from 'fs';
import path from 'path';

import type {Client4} from '@mattermost/client';
import type {Post} from '@mattermost/types/posts';

import MattermostContainer from './mmcontainer';

const repoRoot = path.join(__dirname, '..', '..');
export const PLUGIN_ID: string = JSON.parse(fs.readFileSync(path.join(repoRoot, 'plugin.json'), 'utf8')).id;
export const SLASH_COMMAND = '/mattermost-request-hub';
export const APPROVAL_CHANNEL = 'mattermost-channel-requests';

export const TEAM = 'test';
export const PASSWORD = 'Password-123!';
export const ADMIN = 'sysadmin';

// alice requests, bob and carol are added as members, tadmin is a Team
// Admin of "test".
export const USERS = ['alice', 'bob', 'carol', 'tadmin'] as const;

// Keys under PluginSettings.Plugins are lowercased by the server.
export const DEFAULT_PLUGIN_CONFIG = {
    approvalteam: TEAM,
    channelnameprefixes: 'team-|Team channels|32\nproj-|Project channels|32',
};

function findPluginBundle(): string {
    const dist = path.join(repoRoot, 'dist');
    const bundles = fs.existsSync(dist) ? fs.readdirSync(dist).filter((f) => f.endsWith('.tar.gz')) : [];
    if (bundles.length !== 1) {
        throw new Error(`Expected exactly one plugin bundle in dist/ (run make dist first), found: ${bundles.join(', ') || 'none'}`);
    }
    return path.join(dist, bundles[0]);
}

export type RequestHub = {
    mm: MattermostContainer;
    teamId: string;
    userIds: Record<string, string>;
    client: (username: string) => Promise<Client4>;
    pluginFetch: <T = Record<string, string>>(username: string, route: string, body?: unknown) => Promise<T>;
    approvalChannelId: () => Promise<string>;
    findCard: (needle: string) => Promise<Post>;
    waitForChannel: (name: string) => Promise<string | undefined>;
    pluginErrors: () => string[];
    stop: () => Promise<void>;
};

async function poll<T>(fn: () => Promise<T | undefined>, timeoutMs = 15_000): Promise<T | undefined> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
        const value = await fn().catch(() => undefined);
        if (value !== undefined) {
            return value;
        }
        await new Promise((resolve) => setTimeout(resolve, 500));
    }
    return undefined;
}

// Skips tours and tips that would cover the UI in browser tests.
async function dismissOnboarding(client: Client4) {
    const me = await client.getMe();
    await client.savePreferences(me.id, [
        {user_id: me.id, category: 'tutorial_step', name: me.id, value: '999'},
        {user_id: me.id, category: 'onboarding_task_list', name: 'onboarding_task_list_show', value: 'false'},
        {user_id: me.id, category: 'onboarding_task_list', name: 'onboarding_task_list_open', value: 'false'},
        {user_id: me.id, category: 'crt_thread_pane_step', name: me.id, value: '999'},
        {user_id: me.id, category: 'drafts', name: 'drafts_tour_tip_showed', value: JSON.stringify({drafts_tour_tip_showed: true})},
    ]);
}

export async function startRequestHub(name: string, pluginConfig: Record<string, unknown> = DEFAULT_PLUGIN_CONFIG): Promise<RequestHub> {
    const mm = await new MattermostContainer().start(name);
    try {
        await mm.mmctl('user', 'create', '--email', `${ADMIN}@example.com`, '--username', ADMIN, '--password', PASSWORD, '--system-admin', '--email-verified');
        for (const u of USERS) {
            await mm.mmctl('user', 'create', '--email', `${u}@example.com`, '--username', u, '--password', PASSWORD, '--email-verified');
        }
        await mm.mmctl('team', 'create', '--name', TEAM, '--display-name', 'Test');
        await mm.mmctl('team', 'users', 'add', TEAM, ADMIN, ...USERS);

        const clients: Record<string, Client4> = {};
        const client = async (username: string) => {
            clients[username] ??= await mm.getClient(username, PASSWORD);
            return clients[username];
        };

        const admin = await client(ADMIN);
        await admin.completeSetup({organization: TEAM, install_plugins: []});
        const teamId = (await admin.getTeamByName(TEAM)).id;
        const userIds: Record<string, string> = {};
        for (const u of [ADMIN, ...USERS]) {
            userIds[u] = (await admin.getUserByUsername(u)).id;
            await dismissOnboarding(await client(u));
        }
        await admin.updateTeamMemberSchemeRoles(teamId, userIds.tadmin, true, true);

        await mm.container.copyFilesToContainer([{source: findPluginBundle(), target: '/tmp/plugin.tar.gz'}]);
        await mm.mmctl('plugin', 'add', '/tmp/plugin.tar.gz');
        const config = await admin.getConfig();
        await admin.patchConfig({PluginSettings: {...config.PluginSettings, Plugins: {...config.PluginSettings.Plugins, [PLUGIN_ID]: pluginConfig}}});
        await mm.mmctl('plugin', 'enable', PLUGIN_ID);

        const pluginFetch = async <T>(username: string, route: string, body?: unknown): Promise<T> => {
            const c = await client(username);
            const url = `${mm.url()}/plugins/${PLUGIN_ID}${route}`;
            const res = await fetch(url, c.getOptions(body === undefined ? {method: 'get'} : {method: 'post', body: JSON.stringify(body)}));
            if (!res.ok) {
                throw new Error(`${route} as ${username}: HTTP ${res.status} ${await res.text()}`);
            }
            return res.json() as Promise<T>;
        };

        const approvalChannelId = async () => (await admin.getChannelByName(teamId, APPROVAL_CHANNEL)).id;

        // Newest bot post in the approval channel whose content mentions needle.
        const findCard = async (needle: string): Promise<Post> => {
            const card = await poll(async () => {
                const list = await admin.getPosts(await approvalChannelId(), 0, 30);
                return list.order.map((id) => list.posts[id]).find((p) => JSON.stringify(p).includes(needle));
            });
            if (!card) {
                throw new Error(`No approval card mentioning "${needle}" in ~${APPROVAL_CHANNEL}`);
            }
            return card;
        };

        const waitForChannel = (channelName: string) => poll(async () => (await admin.getChannelByName(teamId, channelName)).id);

        // Error-level server log lines that came from the plugin.
        const pluginErrors = () => mm.logLines().filter((l) => l.includes(`"plugin_id":"${PLUGIN_ID}"`) && l.includes('"level":"error"'));

        return {mm, teamId, userIds, client, pluginFetch, approvalChannelId, findCard, waitForChannel, pluginErrors, stop: mm.stop};
    } catch (error) {
        await mm.stop().catch(() => undefined);
        throw error;
    }
}
