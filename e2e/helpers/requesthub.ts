// Starts a server with the plugin bundle from `make dist` installed, a team
// "test" and a fixed set of users, and wraps the plugin's REST routes, the
// slash command, the request channels and the plugin's KV store.
import fs from 'fs';
import path from 'path';

import type {Client4} from '@mattermost/client';
import type {Post} from '@mattermost/types/posts';

import MattermostContainer from './mmcontainer';

const repoRoot = path.join(__dirname, '..', '..');
export const PLUGIN_ID: string = JSON.parse(fs.readFileSync(path.join(repoRoot, 'plugin.json'), 'utf8')).id;
export const SLASH_COMMAND = '/mattermost-request-hub';
export const HUB_BOT = 'mattermost-request-hub';

// Channels the plugin creates in the approval team on activation.
export const REQUEST_CHANNELS = {
    channel: 'mattermost-channel-requests',
    bot: 'mattermost-bot-requests',
    webhook: 'mattermost-webhook-requests',
    team: 'mattermost-team-requests',
    audit: 'mattermost-audit-requests',
} as const;
export const APPROVAL_CHANNEL = REQUEST_CHANNELS.channel;

export const TEAM = 'test';
export const PASSWORD = 'Password-123!';
export const ADMIN = 'sysadmin';

// Fixed users shared by every test on a server: bob and carol are added as
// members, tadmin is a Team Admin of "test". Tests that submit requests
// should use hub.createUser() instead of alice, so they don't share alice's
// pending-request allowance with every other spec on the same server.
export const USERS = ['alice', 'bob', 'carol', 'tadmin'] as const;

// Keys under PluginSettings.Plugins are lowercased by the server.
export type PluginConfig = Record<string, unknown>;
export const DEFAULT_PLUGIN_CONFIG: PluginConfig = {
    approvalteam: TEAM,
    channelnameprefixes: 'team-|Team channels|32\nproj-|Project channels|32',
};

let uidCounter = 0;

// A short name that is unique on the server, for channels, users and
// request fields: lowercase letters and digits only, so it is valid in
// every kind of name.
export function uid(prefix = 'e2e'): string {
    uidCounter++;
    return `${prefix}${Date.now().toString(36)}${process.pid.toString(36)}${uidCounter}`;
}

function findPluginBundle(): string {
    const dist = path.join(repoRoot, 'dist');
    const bundles = fs.existsSync(dist) ? fs.readdirSync(dist).filter((f) => f.endsWith('.tar.gz')) : [];
    if (bundles.length !== 1) {
        throw new Error(`Expected exactly one plugin bundle in dist/ (run make dist first), found: ${bundles.join(', ') || 'none'}`);
    }
    return path.join(dist, bundles[0]);
}

// Request kinds as the plugin names them in KV keys: the record is
// <recordPrefix><id> and the requester's pending list is
// pending_<kind>_<userId>.
export const REQUEST_KINDS = {
    channel: {recordPrefix: 'request_'},
    admin: {recordPrefix: 'admin_request_'},
    team: {recordPrefix: 'team_request_'},
    team_admin: {recordPrefix: 'team_admin_request_'},
    team_member: {recordPrefix: 'team_member_request_'},
    team_guest: {recordPrefix: 'team_guest_request_'},
    bot: {recordPrefix: 'bot_request_'},
    webhook: {recordPrefix: 'webhook_request_'},
} as const;
export type RequestKind = keyof typeof REQUEST_KINDS;

// The request ID printed in an approval card's footer. (The server strips
// the buttons' integration context from posts it sends to clients.)
export function cardRequestId(card: Post): string {
    const attachments = card.props?.attachments as Array<{footer?: string}> | undefined;
    const id = attachments?.map((a) => (/Request ID: (\w+)/).exec(a.footer ?? '')?.[1]).find(Boolean);
    if (!id) {
        throw new Error(`Post ${card.id} has no request ID footer`);
    }
    return id;
}

export type TestUser = {username: string; id: string};

export type CreateUserOptions = {
    prefix?: string;
    systemAdmin?: boolean;
    teamAdmin?: boolean;

    // Defaults to true: the user joins team "test".
    inTeam?: boolean;
};

// Compares dotted versions numerically: atLeast('11.10.1', '11.9') is true.
export function atLeast(version: string, minimum: string): boolean {
    const a = version.split('.').map(Number);
    const b = minimum.split('.').map(Number);
    for (let i = 0; i < b.length; i++) {
        if ((a[i] ?? 0) !== b[i]) {
            return (a[i] ?? 0) > b[i];
        }
    }
    return true;
}

export type RequestHub = {
    mm: MattermostContainer;

    // The server's version, e.g. "11.3.0", for features that need a newer
    // server than the plugin's min_server_version.
    serverVersion: string;
    teamId: string;
    userIds: Record<string, string>;
    client: (username: string) => Promise<Client4>;
    createUser: (options?: CreateUserOptions) => Promise<TestUser>;
    pluginFetch: <T = Record<string, string>>(username: string, route: string, body?: unknown) => Promise<T>;
    slash: (username: string, args: string, channelName?: string) => Promise<string>;
    channelId: (name: string) => Promise<string>;
    approvalChannelId: () => Promise<string>;
    findCard: (needle: string) => Promise<Post>;
    findCardIn: (channelName: string, needle: string) => Promise<Post>;
    findDM: (username: string, needle: string) => Promise<Post>;
    findAudit: (needle: string) => Promise<Post>;
    waitForChannel: (name: string) => Promise<string | undefined>;
    kv: <T = Record<string, unknown>>(key: string) => Promise<T | undefined>;
    kvKeys: (prefix: string) => Promise<string[]>;
    rewindRequest: (kind: RequestKind, requesterId: string, requestId: string, days: number) => Promise<void>;
    withPluginConfig: <T>(patch: PluginConfig, fn: () => Promise<T>) => Promise<T>;
    logLineCount: () => number;
    pluginErrors: (sinceLine?: number) => string[];
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

// Undoes the plugin's escapeMarkdown in a JSON-encoded post (backslashes
// before markdown characters, zero-width spaces after @ and in ://), so
// tests can look for names as they typed them.
const unescaped = (json: string) => json.replace(/\\\\/g, '').replace(/​/g, '');

// SQL string literal; the values tests pass are generated, but quote them
// properly anyway.
const sqlString = (s: string) => `'${s.replace(/'/g, "''")}'`;

export async function startRequestHub(name: string, pluginConfig: PluginConfig = DEFAULT_PLUGIN_CONFIG): Promise<RequestHub> {
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
        const serverVersion = (await admin.getClientConfig()).Version;
        const userIds: Record<string, string> = {};
        for (const u of [ADMIN, ...USERS]) {
            userIds[u] = (await admin.getUserByUsername(u)).id;
            await dismissOnboarding(await client(u));
        }
        await admin.updateTeamMemberSchemeRoles(teamId, userIds.tadmin, true, true);

        await mm.container.copyFilesToContainer([{source: findPluginBundle(), target: '/tmp/plugin.tar.gz'}]);
        await mm.mmctl('plugin', 'add', '/tmp/plugin.tar.gz');

        const setPluginConfig = async (config: PluginConfig) => {
            const current = await admin.getConfig();
            await admin.patchConfig({PluginSettings: {...current.PluginSettings, Plugins: {...current.PluginSettings.Plugins, [PLUGIN_ID]: config}}});
        };
        await setPluginConfig(pluginConfig);
        await mm.mmctl('plugin', 'enable', PLUGIN_ID);

        const createUser = async (options: CreateUserOptions = {}): Promise<TestUser> => {
            const username = uid(options.prefix ?? 'user');
            await mm.mmctl('user', 'create', '--email', `${username}@example.com`, '--username', username, '--password', PASSWORD, '--email-verified', ...(options.systemAdmin ? ['--system-admin'] : []));
            const id = (await admin.getUserByUsername(username)).id;
            if (options.inTeam ?? true) {
                await admin.addToTeam(teamId, id);
                if (options.teamAdmin) {
                    await admin.updateTeamMemberSchemeRoles(teamId, id, true, true);
                }
            }
            userIds[username] = id;
            await dismissOnboarding(await client(username));
            return {username, id};
        };

        const pluginFetch = async <T>(username: string, route: string, body?: unknown): Promise<T> => {
            const c = await client(username);
            const url = `${mm.url()}/plugins/${PLUGIN_ID}${route}`;
            const res = await fetch(url, c.getOptions(body === undefined ? {method: 'get'} : {method: 'post', body: JSON.stringify(body)}));
            if (!res.ok) {
                throw new Error(`${route} as ${username}: HTTP ${res.status} ${await res.text()}`);
            }
            return res.json() as Promise<T>;
        };

        const channelId = async (channelName: string) => (await admin.getChannelByName(teamId, channelName)).id;

        // Runs "/mattermost-request-hub <args>" as username and returns the
        // ephemeral reply text ("" when the command opened a dialog).
        const slash = async (username: string, args: string, channelName = 'town-square') => {
            const c = await client(username);
            const channel = await c.getChannelByName(teamId, channelName);
            const res = await c.executeCommand(`${SLASH_COMMAND} ${args}`.trim(), {channel_id: channel.id, team_id: teamId} as Parameters<Client4['executeCommand']>[1]);
            return res.text ?? '';
        };

        // Newest post in channelId whose JSON mentions needle, read as username.
        const findPost = async (c: Client4, chId: string, needle: string, what: string): Promise<Post> => {
            const post = await poll(async () => {
                const list = await c.getPosts(chId, 0, 60);
                return list.order.map((id) => list.posts[id]).find((p) => unescaped(JSON.stringify(p)).includes(needle));
            });
            if (!post) {
                throw new Error(`No post mentioning "${needle}" in ${what}`);
            }
            return post;
        };

        const findCardIn = async (channelName: string, needle: string) => findPost(admin, await channelId(channelName), needle, `~${channelName}`);
        const findCard = (needle: string) => findCardIn(APPROVAL_CHANNEL, needle);

        // The requester's DM with the Request Hub bot: ticket threads, token
        // DMs and decision notices.
        const findDM = async (username: string, needle: string) => {
            const c = await client(username);
            const bot = await c.getUserByUsername(HUB_BOT);
            const dm = await c.createDirectChannel([userIds[username] ?? (await c.getMe()).id, bot.id]); // returns the existing DM
            return findPost(c, dm.id, needle, `@${username}'s DM with @${HUB_BOT}`);
        };

        // Audit posts carry their text in an attachment, not the message.
        const findAudit = (needle: string) => findCardIn(REQUEST_CHANNELS.audit, needle);

        const waitForChannel = (channelName: string) => poll(async () => channelId(channelName));

        const kvWhere = (key: string) => `pluginid = ${sqlString(PLUGIN_ID)} AND pkey = ${sqlString(key)}`;

        // A plugin KV value decoded as JSON, read straight from Postgres.
        const kv = async <T>(key: string): Promise<T | undefined> => {
            const raw = await mm.psql(`SELECT convert_from(pvalue, 'UTF8') FROM pluginkeyvaluestore WHERE ${kvWhere(key)}`);
            return raw === '' ? undefined : JSON.parse(raw) as T;
        };

        const kvKeys = async (prefix: string) => {
            const out = await mm.psql(`SELECT pkey FROM pluginkeyvaluestore WHERE pluginid = ${sqlString(PLUGIN_ID)} AND pkey LIKE ${sqlString(prefix.replace(/[\\%_]/g, '\\$&') + '%')} ORDER BY pkey`);
            return out === '' ? [] : out.split('\n');
        };

        // Moves a request's created_at back by days, in both its record and
        // its entry in the requester's pending list, so expiry and pruning
        // can be tested without waiting. The plugin reads both from the KV
        // store on every use and keeps no copy in memory.
        const rewindRequest = async (kind: RequestKind, requesterId: string, requestId: string, days: number) => {
            const millis = Math.round(days * 24 * 60 * 60 * 1000);
            const rewound = (json: string) => `jsonb_set(${json}, '{created_at}', to_jsonb((${json}->>'created_at')::bigint - ${millis}))`;
            const value = `convert_from(pvalue, 'UTF8')::jsonb`;

            const recordKey = REQUEST_KINDS[kind].recordPrefix + requestId;
            const record = await mm.psql(
                `UPDATE pluginkeyvaluestore SET pvalue = convert_to(${rewound(value)}::text, 'UTF8') ` +
                `WHERE ${kvWhere(recordKey)} AND ${value} ? 'created_at' RETURNING pkey`,
            );
            if (record !== recordKey) {
                throw new Error(`rewindRequest: no KV record ${recordKey} with a created_at`);
            }

            const listKey = `pending_${kind}_${requesterId}`;
            const list = await mm.psql(
                `UPDATE pluginkeyvaluestore SET pvalue = convert_to((` +
                `SELECT jsonb_agg(CASE WHEN e->>'id' = ${sqlString(requestId)} THEN ${rewound('e')} ELSE e END ORDER BY n) ` +
                `FROM jsonb_array_elements(${value}) WITH ORDINALITY AS t(e, n))::text, 'UTF8') ` +
                `WHERE ${kvWhere(listKey)} AND ${value} @> ${sqlString(JSON.stringify([{id: requestId}]))}::jsonb RETURNING pkey`,
            );
            if (list !== listKey) {
                throw new Error(`rewindRequest: ${requestId} is not in ${listKey}`);
            }
        };

        // Applies patch on top of the current plugin config for the length
        // of fn, then puts the old config back. OnConfigurationChange runs
        // before patchConfig returns, so the new values apply straight away.
        const withPluginConfig = async <T>(patch: PluginConfig, fn: () => Promise<T>): Promise<T> => {
            const before = (await admin.getConfig()).PluginSettings.Plugins[PLUGIN_ID] as PluginConfig;
            await setPluginConfig({...before, ...patch});
            try {
                return await fn();
            } finally {
                await setPluginConfig(before);
            }
        };

        const logLineCount = () => mm.logLines().length;

        // Error-level server log lines that came from the plugin.
        const pluginErrors = (sinceLine = 0) => mm.logLines().slice(sinceLine).filter((l) => l.includes(`"plugin_id":"${PLUGIN_ID}"`) && l.includes('"level":"error"'));

        return {
            mm,
            serverVersion,
            teamId,
            userIds,
            client,
            createUser,
            pluginFetch,
            slash,
            channelId,
            approvalChannelId: () => channelId(APPROVAL_CHANNEL),
            findCard,
            findCardIn,
            findDM,
            findAudit,
            waitForChannel,
            kv,
            kvKeys,
            rewindRequest,
            withPluginConfig,
            logLineCount,
            pluginErrors,
            stop: mm.stop,
        };
    } catch (error) {
        await mm.stop().catch(() => undefined);
        throw error;
    }
}
