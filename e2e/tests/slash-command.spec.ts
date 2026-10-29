// The slash command without the forms: help, docs, prefixes, argument
// parsing, the inline request forms and what they refuse, list, and
// approve and deny by request ID. API only, no browser.
import type {Post} from '@mattermost/types/posts';

import {expect, test} from '../helpers/fixtures';
import {ADMIN, cardRequestId, HUB_BOT, PLUGIN_ID, REQUEST_CHANNELS, uid, type RequestHub, type TestUser} from '../helpers/requesthub';
import {expectNoRequest, helpSubcommands, listedIds, listRows, outsideCode} from '../helpers/slash';

const ALREADY_HANDLED = 'This request has already been handled.';
const OWN_REQUEST = "You can't approve your own request. Another approver has to review it.";
const NO_CHANNEL_PERMISSION = "You don't have permission to approve or deny this channel request. Team Admins of this team or a System Admin can approve it.";
const TYPE_OFF = 'Requests of this type are turned off. Deny this request instead.';
const CHANNEL_SUBMITTED = 'Your channel request has been submitted for approval.';

// Every subcommand in the help table with the default settings (team guest
// requests are off by default).
const DEFAULT_SUBCOMMANDS = [
    'approve', 'bot', 'channel', 'channel-admin', 'deny', 'docs', 'exercise', 'help', 'list', 'prefixes',
    'team', 'team-admin', 'team-member', 'webhook-incoming', 'webhook-outgoing',
];

// The reply for arguments readArgs or a flag helper can't use.
const usage = (err: string, subcommand: string) => new RegExp(`^Could not read the request: ${escapeRegExp(err)}\\.\\nUsage: \`/mattermost-request-hub ${subcommand} `);

function escapeRegExp(s: string) {
    return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

// Runs each [args, expected reply] as user and checks the reply.
async function expectReplies(hub: RequestHub, user: TestUser, cases: Array<[string, string | RegExp]>, channel?: string) {
    for (const [args, expected] of cases) {
        const reply = await hub.slash(user.username, args, channel);
        if (typeof expected === 'string') {
            expect(reply, args).toBe(expected);
        } else {
            expect(reply, args).toMatch(expected);
        }
    }
}

// A channel request submitted with the slash command, and its card.
async function submitChannel(hub: RequestHub, user: TestUser, extra = '') {
    const name = uid('slash');
    expect(await hub.slash(user.username, `channel proj- ${name} ${extra}`)).toContain(CHANNEL_SUBMITTED);
    const card = await hub.findCard(name);
    return {name, card, id: cardRequestId(card)};
}

// The stored record and card of a request, to show a refusal changed
// neither.
async function snapshot(hub: RequestHub, key: string, card: Post) {
    const admin = await hub.client(ADMIN);
    const post = await admin.getPost(card.id);
    return {record: await hub.kv(key), message: post.message, attachments: JSON.stringify(post.props.attachments)};
}

async function privateChannel(hub: RequestHub, members: TestUser[]) {
    const admin = await hub.client(ADMIN);
    const name = uid('priv');
    const channel = await admin.createChannel({team_id: hub.teamId, name, display_name: name, type: 'P'} as Parameters<typeof admin.createChannel>[0]);
    for (const m of members) {
        await admin.addToChannel(m.id, channel.id);
    }
    return channel;
}

test.describe('help, docs and prefixes', () => {
    test('help lists exactly the subcommands of the enabled request types', async ({hub}) => {
        const user = await hub.createUser();
        const help = await hub.slash(user.username, 'help');
        expect(help.startsWith('**Mattermost Request Hub**: request admin-gated access.')).toBe(true);
        expect(helpSubcommands(help)).toEqual(DEFAULT_SUBCOMMANDS);
        expect(await hub.slash(user.username, 'HELP')).toBe(help);

        await hub.withPluginConfig({allowchanneladminrequests: false, allowbottokenrequests: false, allowwebhookrequests: false, allowteamguestrequests: true}, async () => {
            const without = DEFAULT_SUBCOMMANDS.filter((s) => !['channel-admin', 'bot', 'webhook-incoming', 'webhook-outgoing'].includes(s));
            expect(helpSubcommands(await hub.slash(user.username, 'help'))).toEqual([...without, 'team-guest'].sort());
        });

        // Prefixes belong to channel requests and go with them.
        await hub.withPluginConfig({allowchannelrequests: false, allowteamrequests: false, allowteamadminrequests: false, allowteammemberrequests: false}, async () => {
            const without = DEFAULT_SUBCOMMANDS.filter((s) => !['channel', 'prefixes', 'team', 'team-admin', 'team-member'].includes(s));
            expect(helpSubcommands(await hub.slash(user.username, 'help'))).toEqual(without);
        });
    });

    test('docs links the bundled guide on the site URL; no subcommand opens the channel form', async ({hub}) => {
        const user = await hub.createUser();
        const siteURL = (await (await hub.client(ADMIN)).getConfig()).ServiceSettings.SiteURL!.replace(/\/$/, '');
        const link = `[Open the full guide](${siteURL}/plugins/${PLUGIN_ID}/public/help/docs.html) (opens in a new tab).`;

        const docs = await hub.slash(user.username, 'docs');
        expect(docs).toBe(`**Documentation**\n\n${link}\n\nCovers channels, Channel Admin, teams, bots, webhooks, messaging threads, and slash commands.`);
        expect(await hub.slash(user.username, 'documentation')).toBe(docs);
        expect((await hub.slash(user.username, 'help')).endsWith(`\n\n${link}`)).toBe(true);

        // An empty reply means the dialog was opened.
        expect(await hub.slash(user.username, '')).toBe('');
        expect(await hub.slash(user.username, 'channel')).toBe('');
        await expectNoRequest(hub, user);
    });

    test('prefixes lists the configured prefixes, or why there are none', async ({hub}) => {
        const user = await hub.createUser();
        expect(await hub.slash(user.username, 'prefixes')).toBe('Channel prefixes you can use:\n- `team-` Team channels (suffix rule: `32`)\n- `proj-` Project channels (suffix rule: `32`)');

        await hub.withPluginConfig({channelnameprefixes: 'ops-|Ops *rooms*'}, async () => {
            expect(await hub.slash(user.username, 'prefixes')).toBe('Channel prefixes you can use:\n- `ops-` Ops \\*rooms\\*');
        });
        await hub.withPluginConfig({channelnameprefixes: ''}, async () => {
            const notConfigured = "Channel requests aren't configured yet. Ask a System Admin to define at least one channel prefix in System Console → Plugins → Mattermost Request Hub.";
            expect(await hub.slash(user.username, 'prefixes')).toBe(notConfigured);
            expect(await hub.slash(user.username, 'channel team- Foo')).toBe(notConfigured);
            expect(await hub.slash(user.username, '')).toBe(notConfigured);
        });
        await hub.withPluginConfig({allowchannelrequests: false}, async () => {
            expect(await hub.slash(user.username, 'prefixes')).toBe('Channel requests are currently disabled by an administrator.');
        });
        await expectNoRequest(hub, user);
    });
});

test.describe('parsing', () => {
    test('unknown subcommands and options are echoed inert', async ({hub}) => {
        const user = await hub.createUser();

        const link = await hub.slash(user.username, '[x](https://evil.test)');
        expect(link.startsWith('Unknown subcommand \\[x\\]\\(https:\u200b//evil\\.test\\).\n\n**Mattermost Request Hub**')).toBe(true);
        expect(link).not.toContain('](');
        expect(helpSubcommands(link)).toEqual(DEFAULT_SUBCOMMANDS);

        const mention = await hub.slash(user.username, '@channel');
        expect(mention.startsWith('Unknown subcommand @\u200bchannel.')).toBe(true);
        expect(mention).not.toContain('@channel');

        // inlineCode drops backticks so the value can't close its code span.
        const option = await hub.slash(user.username, 'channel team- Foo --a`](https://evil.test)`b');
        expect(option).toMatch(usage('unknown option `--a](https://evil.test)b`', 'channel'));
        expect(outsideCode(option)).not.toContain('evil.test');

        const optionMention = await hub.slash(user.username, 'team Foo --@channel');
        expect(optionMention).toMatch(usage('unknown option `--@channel`', 'team'));
        expect(outsideCode(optionMention)).not.toContain('@channel');

        await expectNoRequest(hub, user);
    });

    test('quotes and dashes: quoted names, -- inside quotes, em and en dashes as --', async ({hub}) => {
        const user = await hub.createUser();
        const name = uid('q');

        // Curly quotes count too; a quoted word starting with -- is text.
        // (The default prefixes allow 32-character suffixes.)
        const reply = await hub.slash(user.username, `channel team- "--x ${name}" “Big Room” —private –purpose "a --b c" —category “My Cat”`);
        expect(reply).toContain(CHANNEL_SUBMITTED);

        const card = await hub.findCard(name);
        const id = cardRequestId(card);
        type Stored = {display_name: string; name: string; channel_type: string; purpose: string; category_name: string};
        expect(await hub.kv<Stored>(`request_${id}`)).toMatchObject({
            display_name: `--x ${name} Big Room`,
            name: `team-x-${name}-big-room`,
            channel_type: 'P',
            purpose: 'a --b c',
            category_name: 'My Cat',
        });
        await hub.slash(ADMIN, `deny ${id}`);
    });

    test('words, quotes and flags that cannot be read submit nothing', async ({hub}) => {
        const user = await hub.createUser();
        await expectReplies(hub, user, [
            ['channel team- "Foo bar', 'Could not read the command: a quote mark is not closed.'],
            ['channel team- "Foo"bar baz', 'Could not read the command: a closing quote mark must be followed by a space.'],
            ['channel team- Foo --purpose', usage('`--purpose` needs some text', 'channel')],
            ['channel team- Foo --category', usage('`--category` needs a name', 'channel')],
            ['channel team- Foo --suffix', usage('`--suffix` takes exactly one value', 'channel')],
            ['channel team- Foo --suffix a b', usage('`--suffix` takes exactly one value', 'channel')],
            ['channel team- Foo --private yes', usage('`--private` takes no value', 'channel')],
            ['channel team- Foo —private now', usage('`--private` takes no value', 'channel')],
            ['channel team- Foo --members', usage('`--members` needs at least one name', 'channel')],
            ['channel team- Foo --admins ,', usage('`--admins` needs at least one name', 'channel')],
            ['channel team- Foo --purpose a --purpose b', usage('`--purpose` is given more than once', 'channel')],
            ['channel team- Foo --purpose a —purpose b', usage('`--purpose` is given more than once', 'channel')],
            ['channel team- Foo --bogus', usage('unknown option `--bogus`', 'channel')],
        ]);
        await expectNoRequest(hub, user);
    });
});

test.describe('inline request forms refuse what they cannot submit', () => {
    test('channel', async ({hub}) => {
        const user = await hub.createUser();
        await expectReplies(hub, user, [
            ['channel team-', usage('a prefix and a channel name are required', 'channel')],
            ['channel --private', usage('a prefix and a channel name are required', 'channel')],
            ['channel zzz- Foo', 'prefix "zzz-" is not in the list of allowed prefixes: team-, proj-'],
            ['channel team- !!!', 'channel name suffix is required (letters/numbers, becomes the part after the prefix)'],
            ['channel team- Foo --members @nosuchuser', 'unknown user: nosuchuser'],
            ['channel team- Foo --members @carol --admins nosuchuser', 'unknown user: nosuchuser'],
            [`channel team- ${'x'.repeat(65)}`, 'channel name must be 64 characters or fewer'],
        ]);
        await expectNoRequest(hub, user);
    });

    test('channel-admin', async ({hub}) => {
        const user = await hub.createUser();
        const room = await privateChannel(hub, [user]);
        await expectReplies(hub, user, [
            ['channel-admin @nosuchuser', 'unknown user: nosuchuser'],
            ['channel-admin ,', 'Name at least one channel member: `/mattermost-request-hub channel-admin @user [@user ...]`'],
            ['channel-admin @carol --now', usage('unknown option `--now`', 'channel-admin')],
            ['channel-admin @carol', 'everyone you nominate must already be a member of the channel'],
        ], room.name);
        await expectNoRequest(hub, user);
    });

    test('team', async ({hub}) => {
        const user = await hub.createUser();
        await expectReplies(hub, user, [
            ['team --invite-only', usage('a team name is required', 'team')],
            ['team Foo --name x', '"x" is not a valid team URL name; use 2-64 lowercase letters, numbers, and hyphens'],
            ['team Foo --name a b', usage('`--name` takes exactly one value', 'team')],
            ['team Foo --name signup', /^"signup" is (reserved by Mattermost|not a valid team URL name)/],
            ['team Foo --invite-only yes', usage('`--invite-only` takes no value', 'team')],
            ['team Foo --description', usage('`--description` needs some text', 'team')],
            ['team Foo --private', usage('unknown option `--private`', 'team')],
        ]);
        await expectNoRequest(hub, user);
    });

    test('team-admin', async ({hub}) => {
        const user = await hub.createUser();
        const outsider = await hub.createUser({inTeam: false});
        await expectReplies(hub, user, [
            ['team-admin @nosuchuser', 'unknown user: nosuchuser'],
            ['team-admin ,', usage('name at least one team member', 'team-admin')],
            ['team-admin @carol --reason x', usage('unknown option `--reason`', 'team-admin')],
            [`team-admin @${outsider.username}`, `@${outsider.username} isn't a member of this team`],
            [`team-admin @${HUB_BOT}`, `@${HUB_BOT} is a bot and can't be made a Team Admin`],
        ]);
        await expectNoRequest(hub, user);
    });

    test('team-member', async ({hub}) => {
        const user = await hub.createUser();
        const gone = await hub.createUser({inTeam: false});
        await (await hub.client(ADMIN)).updateUserActive(gone.id, false);
        await expectReplies(hub, user, [
            ['team-member --reason hi', usage('name at least one user', 'team-member')],
            ['team-member @nosuchuser', 'unknown user: nosuchuser'],
            ['team-member @carol --reason', usage('`--reason` needs some text', 'team-member')],
            ['team-member @carol --channels ~town-square', usage('unknown option `--channels`', 'team-member')],
            [`team-member @${gone.username}`, `@${gone.username} is deactivated and can't be made a team member`],
        ]);
        await expectNoRequest(hub, user);
    });

    test('team-guest', async ({hub}) => {
        const user = await hub.createUser();
        const room = await privateChannel(hub, []);

        // Off by default.
        expect(await hub.slash(user.username, 'team-guest guest@example.com --channels ~town-square')).toBe('Team guest requests are currently disabled by an administrator.');
        expect(await hub.slash(user.username, 'team-guest')).toBe('Team guest requests are currently disabled by an administrator.');

        await hub.withPluginConfig({allowteamguestrequests: true}, async () => {
            await expectReplies(hub, user, [
                ['team-guest --channels ~town-square', usage('give one guest email', 'team-guest')],
                ['team-guest a@example.com b@example.com --channels ~town-square', usage('give one guest email', 'team-guest')],
                ['team-guest guest@example.com', usage('name at least one channel with `--channels`', 'team-guest')],
                ['team-guest guest@example.com --channels', usage('`--channels` needs at least one name', 'team-guest')],
                ['team-guest not-an-email --channels ~town-square', 'a valid guest email is required'],
                ['team-guest guest@example.com --channels ~nosuchchannel', 'unknown channel: ~nosuchchannel'],
                // A private channel the requester isn't in reads as missing.
                [`team-guest guest@example.com --channels ~${room.name}`, `unknown channel: ~${room.name}`],
            ]);
        });
        await expectNoRequest(hub, user);
    });

    test('bot', async ({hub}) => {
        const user = await hub.createUser();
        await expectReplies(hub, user, [
            ['bot onlyname', usage('a bot username and a display name are required', 'bot')],
            ['bot ab Some Bot', 'Bot username must be 3-22 characters.'],
            ['bot bad!name Some Bot', 'Bot username must be lowercase letters, numbers, and . - _ (3-22 characters).'],
            ['bot goodbot Some Bot --owner @nosuchuser', 'unknown user: nosuchuser'],
            ['bot goodbot Some Bot --owner @carol @bob', usage('`--owner` takes exactly one value', 'bot')],
            ['bot goodbot Some Bot --description', usage('`--description` needs some text', 'bot')],
            ['bot goodbot Some Bot --token', usage('unknown option `--token`', 'bot')],
        ]);
        await expectNoRequest(hub, user);
    });

    test('webhook-incoming and webhook-outgoing', async ({hub}) => {
        const user = await hub.createUser();
        const room = await privateChannel(hub, [user]);
        await expectReplies(hub, user, [
            ['webhook-incoming ~town-square', usage('a channel and a display name are required', 'webhook-incoming')],
            ['webhook-incoming ~nosuchchannel Hook', 'unknown channel: ~nosuchchannel'],
            ['webhook-incoming ~town-square Hook --callback https://example.com', usage('unknown option `--callback`', 'webhook-incoming')],
            ['webhook-incoming ~town-square Hook --description', usage('`--description` needs some text', 'webhook-incoming')],
            ['webhook-outgoing ~town-square Hook', 'a callback URL is required for outgoing webhooks'],
            ['webhook-outgoing ~town-square Hook --callback ftp://example.com', 'the callback URL must be an http or https address'],
            ['webhook-outgoing ~town-square Hook --callback https://a.example https://b.example', usage('`--callback` takes exactly one value', 'webhook-outgoing')],
            [`webhook-outgoing ~${room.name} Hook --callback https://example.com`, 'outgoing webhooks can only use public channels'],
        ]);
        await expectNoRequest(hub, user);
    });

    test('every inline form, and every form, is refused while its type is off', async ({hub}) => {
        const user = await hub.createUser();
        const off = {
            allowchannelrequests: false, allowchanneladminrequests: false, allowteamrequests: false, allowteamadminrequests: false,
            allowteammemberrequests: false, allowteamguestrequests: false, allowbottokenrequests: false, allowwebhookrequests: false,
        };
        await hub.withPluginConfig(off, async () => {
            const disabled = (what: string) => `${what} requests are currently disabled by an administrator.`;
            await expectReplies(hub, user, [
                ['channel team- Foo', disabled('Channel')],
                ['', disabled('Channel')],
                ['channel-admin @carol', disabled('Channel Admin')],
                ['channel-admin', disabled('Channel Admin')],
                ['team Foo', disabled('Team')],
                ['team', disabled('Team')],
                ['team-admin @carol', disabled('Team Admin')],
                ['team-member @carol', disabled('Team member')],
                ['team-guest guest@example.com --channels ~town-square', disabled('Team guest')],
                ['bot goodbot Some Bot', disabled('Bot account')],
                ['bot', disabled('Bot account')],
                ['webhook-incoming ~town-square Hook', disabled('Webhook')],
                ['webhook-outgoing ~town-square Hook --callback https://example.com', disabled('Webhook')],
                ['list', 'Every request type is currently disabled by an administrator.'],
                ['list approvals', 'Every request type is currently disabled by an administrator.'],
            ]);
            expect(helpSubcommands(await hub.slash(user.username, 'help'))).toEqual(['approve', 'deny', 'docs', 'exercise', 'help', 'list']);
        });
        await expectNoRequest(hub, user);
    });
});

test('inline channel request with every flag; approve by ID creates it as asked', async ({hub}) => {
    const requester = await hub.createUser();
    const [m1, m2, chAdmin] = [await hub.createUser(), await hub.createUser(), await hub.createUser()];
    const name = `Flags ${uid()}`;
    const suffix = uid('flags');
    const category = `Cat ${uid()}`;
    const reply = await hub.slash(requester.username,
        `channel team- "${name}" --purpose Purpose of the room —private --members @${m1.username}, @${m2.username} --admins ${chAdmin.username} --category "${category}" --suffix ${suffix}`);
    expect(reply).toBe(`${CHANNEL_SUBMITTED} You'll be notified once an admin responds.`);

    const card = await hub.findCard(name);
    const id = cardRequestId(card);
    const attachment = (card.props.attachments as Array<{fields: Array<{title: string; value: string}>; text: string}>)[0];
    const field = (title: string) => attachment.fields.find((f) => f.title === title)?.value;
    expect(field('Visibility')).toBe('Private');
    expect(field('URL')).toBe(`team-${suffix}`);
    expect(field('Requested by')).toBe(`@${requester.username}`);
    expect(field('Sidebar category')).toBe(category);
    expect(attachment.text).toContain('**Purpose:** Purpose of the room');
    expect(attachment.text).toContain(`**Members to add:** @${m1.username}, @${m2.username}`);
    expect(attachment.text).toContain(`**Channel Admins to add:** @${chAdmin.username}`);

    expect(await hub.slash(ADMIN, `approve ${id}`)).toBe(`✅ Approved by @${ADMIN} for @${requester.username}. Channel ~team-${suffix} created.`);

    const admin = await hub.client(ADMIN);
    const channel = await admin.getChannelByName(hub.teamId, `team-${suffix}`);
    expect(channel).toMatchObject({type: 'P', display_name: name, purpose: 'Purpose of the room'});
    for (const u of [requester, m1, m2, chAdmin]) {
        await admin.getChannelMember(channel.id, u.id);
    }
    expect((await admin.getChannelMember(channel.id, chAdmin.id)).scheme_admin).toBe(true);
    expect((await admin.getChannelMember(channel.id, m1.id)).scheme_admin).toBe(false);

    const categories = await (await hub.client(requester.username)).getChannelCategories(requester.id, hub.teamId);
    const mine = categories.categories.find((c) => c.display_name === category);
    expect(mine?.channel_ids).toContain(channel.id);

    // The same outcome as the Approve button: card resolved, ticket closed.
    await expect.poll(async () => (await admin.getPost(card.id)).message).toContain('Approved');
    await hub.findDM(requester.username, 'Channel Request Approved');
    await hub.findAudit(name);
    expect(await hub.kv(`request_${id}`)).toBeUndefined();
    expect(await hub.slash(requester.username, 'list')).toBe('You have no pending requests.');

    expect(await hub.slash(ADMIN, `approve ${id}`)).toBe(ALREADY_HANDLED);
    expect(await hub.slash(ADMIN, `deny ${id}`)).toBe(ALREADY_HANDLED);
});

test.describe('list', () => {
    test('list shows only the caller\'s own pending requests, oldest first', async ({hub}) => {
        const user = await hub.createUser();
        const other = await hub.createUser();
        expect(await hub.slash(user.username, 'list')).toBe('You have no pending requests.');
        expect(await hub.slash(user.username, 'list bogus')).toBe('Use `/mattermost-request-hub list` for your own requests, or `/mattermost-request-hub list approvals` for the requests you can approve.');

        const a = await submitChannel(hub, user);
        const teamName = uid('slashteam');
        expect(await hub.slash(user.username, `team ${teamName}`)).toContain('submitted');
        const teamId = cardRequestId(await hub.findCardIn(REQUEST_CHANNELS.team, teamName));
        const theirs = await submitChannel(hub, other);

        // Rewound so the team request is older than the channel request.
        await hub.rewindRequest('team', user.id, teamId, 1);

        const list = await hub.slash(user.username, 'list');
        expect(list.startsWith('Your pending requests:\n')).toBe(true);
        expect(listedIds(list)).toEqual([teamId, a.id]);
        const [teamRow, channelRow] = listRows(list);
        expect(teamRow).toMatch(new RegExp(`^- Team \\*\\*${teamName}\\*\\* \\(\`/${teamName}\`\\), submitted \\d{4}-\\d\\d-\\d\\d, expires \\d{4}-\\d\\d-\\d\\d\\. ID \`${teamId}\`$`));
        expect(channelRow).toMatch(new RegExp(`^- Channel \\*\\*${a.name}\\*\\* \\(\`proj-${a.name}\`\\) in Test, submitted `));
        expect(list).not.toContain(theirs.id);
        expect(listedIds(await hub.slash(other.username, 'list'))).toEqual([theirs.id]);

        // A turned-off type drops out of the list.
        await hub.withPluginConfig({allowteamrequests: false}, async () => {
            expect(listedIds(await hub.slash(user.username, 'list'))).toEqual([a.id]);
        });

        for (const id of [a.id, teamId, theirs.id]) {
            await hub.slash(ADMIN, `deny ${id}`);
        }
        expect(await hub.slash(user.username, 'list')).toBe('You have no pending requests.');
    });

    test('list approvals shows what the caller can approve now, oldest first', async ({hub}) => {
        const requester = await hub.createUser();
        const regular = await hub.createUser();
        const a = await submitChannel(hub, requester);
        const b = await submitChannel(hub, requester);
        const teamName = uid('slashteam');
        await hub.slash(requester.username, `team ${teamName}`);
        const t = cardRequestId(await hub.findCardIn(REQUEST_CHANNELS.team, teamName));

        // Rewound older than anything other specs left pending, so they
        // fall inside the list cap, and in an order other than submission.
        await hub.rewindRequest('channel', requester.id, b.id, 3);
        await hub.rewindRequest('channel', requester.id, a.id, 2);
        await hub.rewindRequest('team', requester.id, t, 1);

        expect(await hub.slash(regular.username, 'list approvals')).toBe('There are no pending requests you can approve.');

        const sysadmin = await hub.slash(ADMIN, 'list approvals');
        expect(sysadmin.startsWith('Requests you can approve:\n')).toBe(true);
        expect(listedIds(sysadmin).filter((id) => [a.id, b.id, t].includes(id))).toEqual([b.id, a.id, t]);

        // A Team Admin approves channel requests for their team, but not
        // team creation (AllowApprovalTeamAdminsForTeamRequests is off).
        const tadmin = listedIds(await hub.slash('tadmin', 'list approvals'));
        expect(tadmin.filter((id) => [a.id, b.id, t].includes(id))).toEqual([b.id, a.id]);

        // Never the caller's own requests, even for an approver.
        const teamAdminRequester = await hub.createUser({teamAdmin: true});
        const own = await submitChannel(hub, teamAdminRequester);
        expect(listedIds(await hub.slash(teamAdminRequester.username, 'list approvals'))).not.toContain(own.id);
        expect(listedIds(await hub.slash('tadmin', 'list approvals'))).toContain(own.id);

        for (const id of [a.id, b.id, t, own.id]) {
            await hub.slash(ADMIN, `deny ${id}`);
        }
    });

    test('both lists stop at 50 and say how many more there are', async ({hub}) => {
        test.setTimeout(180_000);
        const user = await hub.createUser();
        const ids: string[] = [];
        await hub.withPluginConfig({maxpendingrequestsperuser: '0'}, async () => {
            try {
                for (let i = 0; i < 51; i++) {
                    const res = await hub.pluginFetch(user.username, '/api/v1/create', {team_id: hub.teamId, display_name: uid('many'), prefix: 'proj-', name: '', channel_type: 'O', members: [], admin_members: []});
                    expect(res.message).toContain('submitted for approval');
                }
                type Entry = {id: string};
                ids.push(...(await hub.kv<Entry[]>(`pending_channel_${user.id}`))!.map((e) => e.id));
                expect(ids).toHaveLength(51);

                const list = await hub.slash(user.username, 'list');
                expect(listRows(list)).toHaveLength(51);
                expect(listedIds(list)).toEqual(ids.slice(0, 50));
                expect(list.endsWith('\n- ...and 1 more')).toBe(true);

                const approvals = await hub.slash(ADMIN, 'list approvals');
                expect(listedIds(approvals)).toHaveLength(50);
                expect(approvals).toMatch(/\n- \.\.\.and \d+ more$/);
            } finally {
                const pending = (await hub.kv<Array<{id: string}>>(`pending_channel_${user.id}`)) ?? [];
                for (let i = 0; i < pending.length; i += 10) {
                    await Promise.all(pending.slice(i, i + 10).map((e) => hub.slash(ADMIN, `deny ${e.id}`)));
                }
            }
        });
        expect(await hub.slash(user.username, 'list')).toBe('You have no pending requests.');
    });
});

test.describe('approve and deny by ID', () => {
    test('refusals leave the request, its card and the pending entry untouched', async ({hub}) => {
        const requester = await hub.createUser();
        const regular = await hub.createUser();
        const r = await submitChannel(hub, requester);
        const key = `request_${r.id}`;
        const before = await snapshot(hub, key, r.card);
        const give = (sub: string) => `Give the request ID from the approval card: \`/mattermost-request-hub ${sub} <request ID>\``;

        await expectReplies(hub, regular, [
            [`approve ${r.id}`, NO_CHANNEL_PERMISSION],
            [`deny ${r.id}`, NO_CHANNEL_PERMISSION],
        ]);
        await expectReplies(hub, requester, [
            [`approve ${r.id}`, NO_CHANNEL_PERMISSION],
            [`deny ${r.id}`, NO_CHANNEL_PERMISSION],
        ]);
        await expectReplies(hub, {username: ADMIN, id: hub.userIds[ADMIN]}, [
            ['approve', give('approve')],
            ['deny', give('deny')],
            ['approve not-an-id', give('approve')],
            [`approve ${r.id} ${r.id}`, give('approve')],
            [`deny ${r.id.toUpperCase()}`, ALREADY_HANDLED],
            [`approve ${'a'.repeat(26)}`, ALREADY_HANDLED],
        ]);

        expect(await snapshot(hub, key, r.card)).toEqual(before);
        expect(listedIds(await hub.slash(requester.username, 'list'))).toEqual([r.id]);
        await expect((await hub.client(ADMIN)).getChannelByName(hub.teamId, `proj-${r.name}`)).rejects.toMatchObject({status_code: 404});

        expect(await hub.slash(ADMIN, `deny ${r.id}`)).toBe(`❌ Denied by @${ADMIN} for @${requester.username}. Channel request **${r.name}** (\`proj-${r.name}\`).`);
        await expect.poll(async () => (await (await hub.client(ADMIN)).getPost(r.card.id)).message).toContain('Denied');
        await hub.findDM(requester.username, 'Channel Request Denied');
        expect(await hub.slash(ADMIN, `approve ${r.id}`)).toBe(ALREADY_HANDLED);
        await expect((await hub.client(ADMIN)).getChannelByName(hub.teamId, `proj-${r.name}`)).rejects.toMatchObject({status_code: 404});
    });

    test('a Team Admin can\'t approve their own request, but another approver can', async ({hub}) => {
        const requester = await hub.createUser({teamAdmin: true});
        const r = await submitChannel(hub, requester);
        const before = await snapshot(hub, `request_${r.id}`, r.card);

        expect(await hub.slash(requester.username, `approve ${r.id}`)).toBe(OWN_REQUEST);
        expect(await snapshot(hub, `request_${r.id}`, r.card)).toEqual(before);

        expect(await hub.slash('tadmin', `approve ${r.id}`)).toBe(`✅ Approved by @tadmin for @${requester.username}. Channel ~proj-${r.name} created.`);
        expect(await hub.waitForChannel(`proj-${r.name}`)).toBeTruthy();
    });

    test('a turned-off type can be denied but not approved', async ({hub}) => {
        const requester = await hub.createUser();
        const r = await submitChannel(hub, requester);
        const before = await snapshot(hub, `request_${r.id}`, r.card);
        await hub.withPluginConfig({allowchannelrequests: false}, async () => {
            expect(await hub.slash(ADMIN, `approve ${r.id}`)).toBe(TYPE_OFF);
            expect(await snapshot(hub, `request_${r.id}`, r.card)).toEqual(before);
            expect(await hub.slash(ADMIN, `deny ${r.id}`)).toMatch(/^❌ Denied by @sysadmin/);
        });
        expect(await hub.kv(`request_${r.id}`)).toBeUndefined();
    });

    test('a bot request ID is routed to the two-step approval', async ({hub}) => {
        const requester = await hub.createUser();
        const botName = uid('bot'); // bot usernames are 3-22 characters
        await hub.withPluginConfig({twostepapprovermode: 'system_admin'}, async () => {
            expect(await hub.slash(requester.username, `bot ${botName} Slash Bot --description made by slash`)).toBe(
                "Your bot account request has been submitted for approval. It needs a security approval and a system approval. You'll be notified as each step completes.");
            const card = await hub.findCardIn(REQUEST_CHANNELS.bot, botName);
            const id = cardRequestId(card);
            const key = `bot_request_${id}`;
            const before = await snapshot(hub, key, card);

            expect(await hub.slash(requester.username, `approve ${id}`)).toBe('⚠️ You created this request — you cannot approve it. Another security or system approver must review this ticket.');
            expect(await hub.slash('carol', `approve ${id}`)).toBe("⚠️ You don't have permission to approve this request.");
            expect(await hub.slash('carol', `deny ${id}`)).toBe("You don't have permission to deny this request.");
            expect(await hub.slash('tadmin', `deny ${id}`)).toBe("You don't have permission to deny this request.");
            expect(await snapshot(hub, key, card)).toEqual(before);
            expect(listedIds(await hub.slash('carol', 'list approvals'))).not.toContain(id);
            expect(listedIds(await hub.slash(ADMIN, 'list approvals'))).toContain(id);

            // In system_admin mode one System Admin fills both steps.
            expect(await hub.slash(ADMIN, `approve ${id}`)).toBe(`✅ Fully approved (security + system) for @${requester.username}. Bot **@${botName}** created.`);
            const bot = await (await hub.client(ADMIN)).getUserByUsername(botName);
            expect(bot.is_bot).toBe(true);
            expect(await hub.kv(key)).toBeUndefined();
            expect(await hub.slash(ADMIN, `deny ${id}`)).toBe(ALREADY_HANDLED);
        });
    });
});

test('exercise is refused while exercise mode is off and for non-admins', async ({hub}) => {
    const user = await hub.createUser();
    const off = 'Exercise mode is off. A System Admin can turn on **Enable exercise mode** in System Console → Plugins → Mattermost Request Hub.';
    expect(await hub.slash(user.username, 'exercise populate')).toBe(off);
    expect(await hub.slash(ADMIN, 'exercise status')).toBe(off);

    await hub.withPluginConfig({enableexercise: true}, async () => {
        expect(await hub.slash(user.username, 'exercise populate')).toBe('Only System Admins can run `/mattermost-request-hub exercise`.');
        expect(await hub.slash('tadmin', 'exercise teardown')).toBe('Only System Admins can run `/mattermost-request-hub exercise`.');
        expect(await hub.slash(ADMIN, 'exercise')).toBe('Usage: `/mattermost-request-hub exercise populate|teardown|status`');
        expect(await hub.slash(ADMIN, 'exercise bogus`x')).toBe('Unknown exercise subcommand `bogusx`. Usage: `/mattermost-request-hub exercise populate|teardown|status`');
    });
});
