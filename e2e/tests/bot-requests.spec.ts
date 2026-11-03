import type {Post} from '@mattermost/types/posts';
import type {IncomingWebhook} from '@mattermost/types/integrations';

import {
    actionIds,
    attachmentOf,
    botCardsMentioning,
    botName,
    botRecordsOf,
    createAttribute,
    createGroup,
    dmsMentioning,
    fieldValue,
    joinBotChannel,
    listenForEphemerals,
    pendingBotIds,
    setAttribute,
    storedBotRequest,
    submitBot,
    userNamed,
    type BotRequestBody,
} from '../helpers/bots';
import {expect, test} from '../helpers/fixtures';
import {ADMIN, HUB_BOT, PASSWORD, PLUGIN_ID, REQUEST_CHANNELS, cardRequestId, uid, type RequestHub, type TestUser} from '../helpers/requesthub';

const SUBMITTED = 'Your bot account request has been submitted for approval. It needs a security approval and a system approval. You\'ll be notified as each step completes.';
const DISABLED = 'Bot account requests are currently disabled by an administrator.';
const HEADER = 'a bot account request needs review (requires a security approval and a system approval).';
const AWAITING_BOTH = '🔒 Security: ⏳ awaiting approval\n🛠 System: ⏳ awaiting approval';
const NO_PERMISSION = 'You don\'t have permission to approve this request.';
const ALREADY_APPROVED_STEP = 'You\'ve already approved your step. This request needs a different approver to complete it.';
const SYSTEM_ONLY_LEFT = 'Security already approved. This request now needs a system approver (a different person).';
const SECURITY_FIRST = '🔒 Security review required first.';
const REQUESTER_CANNOT = '🚫 You created this request — you cannot approve it.';
const USERNAME_CHARS = 'Bot username must be lowercase letters, numbers, and . - _ (3-22 characters).';

const SYSTEM_ADMIN_MODE = {twostepapprovermode: 'system_admin'};

type Submitted = {card: Post; requestId: string};

async function submitOK(hub: RequestHub, requester: TestUser, body: BotRequestBody & {username: string}): Promise<Submitted> {
    const res = await submitBot(hub, requester.username, body);
    expect(res, JSON.stringify(res)).toEqual({message: SUBMITTED});
    const card = await hub.findCardIn(REQUEST_CHANNELS.bot, `@${body.username.toLowerCase()}`);
    return {card, requestId: cardRequestId(card)};
}

const click = async (hub: RequestHub, username: string, postId: string, action: 'approve' | 'deny') =>
    (await hub.client(username)).doPostAction(postId, action);

const getPost = async (hub: RequestHub, postId: string) => (await hub.client(ADMIN)).getPost(postId);

// A refused submit leaves no trace: no stored request, no pending entry,
// no card, no ticket DM and no bot.
async function expectNothingSubmitted(hub: RequestHub, requester: TestUser, username: string) {
    expect(await botRecordsOf(hub, requester.id), 'stored bot requests').toEqual([]);
    expect(await pendingBotIds(hub, requester.id), 'pending list').toEqual([]);
    if (username.length >= 3) {
        expect(await botCardsMentioning(hub, `@${username}"`), 'approval cards').toEqual([]);
    }
    expect(await dmsMentioning(hub, requester.username, 'bot account request'), 'ticket DMs').toEqual([]);
    if (username.trim() !== '' && !['all', 'channel', 'system', 'matterbot'].includes(username)) {
        expect(await userNamed(hub, username.toLowerCase()), `@${username}`).toBeUndefined();
    }
}

// A refused approval changes nothing: the record and pending entry stay as
// they were, the card keeps its buttons and no bot is created.
async function expectStillPending(hub: RequestHub, requester: TestUser, s: Submitted, before: unknown, username: string) {
    expect(await storedBotRequest(hub, s.requestId)).toEqual(before);
    expect(await pendingBotIds(hub, requester.id)).toContain(s.requestId);
    expect(actionIds(await getPost(hub, s.card.id))).toEqual(['approve', 'deny']);
    expect(await userNamed(hub, username)).toBeUndefined();
}

// Runs fn in attribute mode with two fresh admin-managed attributes as the
// security and system pools, and deletes them afterwards: a server allows
// only a few Custom Profile Attribute fields.
type Pools = {security: (u: TestUser) => Promise<void>; system: (u: TestUser) => Promise<void>};
async function withAttributePools(hub: RequestHub, fn: (pools: Pools) => Promise<void>, {extra = {}, poolsMustWork = true}: {extra?: Record<string, string>; poolsMustWork?: boolean} = {}) {
    const securityAttr = uid('sec');
    const systemAttr = uid('sys');
    const securityField = await createAttribute(hub, securityAttr);
    const systemField = await createAttribute(hub, systemAttr);
    const admin = await hub.client(ADMIN);
    try {
        if (poolsMustWork) {
            test.fail(await attributeGroupName(hub, securityField) !== 'access_control', ATTRIBUTE_GROUP_BUG);
        }
        await hub.withPluginConfig({twostepapprovermode: 'attribute', securityapproverattribute: securityAttr, systemapproverattribute: systemAttr, ...extra}, () => fn({
            security: (u) => setAttribute(hub, u.id, securityField, 'yes'),
            system: (u) => setAttribute(hub, u.id, systemField, 'yes'),
        }));
    } finally {
        await admin.deleteCustomProfileAttributeField(securityField);
        await admin.deleteCustomProfileAttributeField(systemField);
    }
}

// Mattermost 11.3 keeps Custom Profile Attributes in the
// custom_profile_attributes property group; later versions moved them to
// access_control, the only group the plugin looks in.
const ATTRIBUTE_GROUP_BUG = 'BUG: attribute mode finds no approvers where Custom Profile Attributes are not in the access_control group (Mattermost 11.3)';
const attributeGroupName = (hub: RequestHub, fieldId: string) =>
    hub.mm.psql(`SELECT g.name FROM propertyfields f JOIN propertygroups g ON g.id = f.groupid WHERE f.id = '${fieldId}'`);

// Checks a fully approved request: the bot exists and belongs to owner, the
// card and ticket are closed, the audit post is there and the pending
// entry is gone.
async function expectApproved(hub: RequestHub, requester: TestUser, s: Submitted, username: string, finalApprover: string, ownerId = requester.id) {
    const admin = await hub.client(ADMIN);
    let botUser: {id: string; is_bot: boolean} | undefined;
    await expect.poll(async () => {
        botUser = await userNamed(hub, username);
        return botUser?.is_bot;
    }, {message: `@${username} created`}).toBe(true);
    expect((await admin.getBot(botUser!.id)).owner_id).toBe(ownerId);

    const card = await getPost(hub, s.card.id);
    expect(card.message).toBe(`✅ Fully approved (security + system) for @${requester.username}. Bot **@${username}** created.`);
    expect(actionIds(card)).toEqual([]);

    const ticket = await hub.findDM(requester.username, 'Bot Account Request Approved');
    expect(attachmentOf(ticket)?.text).toBe(`Fully approved (security + system). Bot **@${username}** has been created.`);
    await hub.findDM(requester.username, `✅ **Approved** by @${finalApprover}.`);
    await hub.findAudit(`@${finalApprover} gave final approval; bot @${username} created for @${requester.username}`);

    expect(await storedBotRequest(hub, s.requestId)).toBeUndefined();
    expect(await pendingBotIds(hub, requester.id)).not.toContain(s.requestId);
    return botUser!.id;
}

async function expectDenied(hub: RequestHub, requester: TestUser, s: Submitted, username: string, denier: string) {
    const card = await getPost(hub, s.card.id);
    expect(card.message).toBe(`❌ Denied by @${denier} for @${requester.username}. Bot **@${username}**.`);
    expect(actionIds(card)).toEqual([]);
    const ticket = await hub.findDM(requester.username, 'Bot Account Request Denied');
    expect(attachmentOf(ticket)?.text).toContain(`Denied by @${denier}. Your request for bot **@${username}** was not approved.`);
    await hub.findDM(requester.username, `❌ **Denied** by @${denier}.`);
    await hub.findAudit(`@${denier} denied a bot request for @${username}`);
    expect(await storedBotRequest(hub, s.requestId)).toBeUndefined();
    expect(await pendingBotIds(hub, requester.id)).not.toContain(s.requestId);
    expect(await userNamed(hub, username)).toBeUndefined();
}

test.describe('submit', () => {
    test('REST submit posts a card in the bot channel and opens a ticket DM', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        const s = await submitOK(hub, requester, {username: username.toUpperCase(), display_name: 'Card Bot', description: 'posts things', request_token: true});

        // The username is stored lowercased.
        expect(s.card.channel_id).toBe(await hub.channelId(REQUEST_CHANNELS.bot));
        expect(s.card.message).toContain(HEADER);
        expect(actionIds(s.card)).toEqual(['approve', 'deny']);
        expect(fieldValue(s.card, 'Requested by')).toBe(`@${requester.username}`);
        expect(fieldValue(s.card, 'Bot username')).toBe(`@${username}`);
        expect(fieldValue(s.card, 'Approvals')).toBe(AWAITING_BOTH);
        expect(attachmentOf(s.card)?.text).toContain('**Token:** requested');

        const ticket = await hub.findDM(requester.username, `@${username}`);
        expect(ticket.message).toContain('Your bot account request has been submitted for approval.');
        expect(actionIds(ticket)).toEqual([]);

        expect(await storedBotRequest(hub, s.requestId)).toMatchObject({requester_id: requester.id, username, request_token: true});
        expect(await pendingBotIds(hub, requester.id)).toEqual([s.requestId]);
    });

    test('inline slash command submits a request with an owner', async ({hub}) => {
        const requester = await hub.createUser();
        const owner = await hub.createUser();
        const username = botName();
        const reply = await hub.slash(requester.username, `bot ${username} Slash Bot --description from slash --owner @${owner.username}`);
        expect(reply).toBe(SUBMITTED);

        const card = await hub.findCardIn(REQUEST_CHANNELS.bot, `@${username}`);
        expect(attachmentOf(card)?.text).toContain('**Display name:** Slash Bot');
        expect(attachmentOf(card)?.text).toContain('**Description:** from slash');
        expect(attachmentOf(card)?.text).toContain(`**Owner:** @${owner.username}`);
        const s = {card, requestId: cardRequestId(card)};
        const stored = await storedBotRequest(hub, s.requestId);
        expect(stored).toMatchObject({requester_id: requester.id, owner_user_id: owner.id});
        expect(stored?.request_token).toBeUndefined();
        await hub.findDM(requester.username, `@${username}`);

        // The bot belongs to the owner picked, not the requester.
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            await click(hub, ADMIN, card.id, 'approve');
            const botId = await expectApproved(hub, requester, s, username, ADMIN, owner.id);
            expect((await (await hub.client(ADMIN)).getBot(botId)).description).toBe('from slash');
        });
    });

    test('a second pending request for the same bot is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        const s = await submitOK(hub, requester, {username});
        expect(await submitBot(hub, requester.username, {username})).toEqual({error: `you already have a pending request for bot @${username}`});
        expect(await hub.slash(requester.username, `bot ${username} Again`)).toBe(`you already have a pending request for bot @${username}`);
        expect(await botRecordsOf(hub, requester.id)).toEqual([`bot_request_${s.requestId}`]);
        expect(await pendingBotIds(hub, requester.id)).toEqual([s.requestId]);
        expect(await botCardsMentioning(hub, `@${username}"`)).toHaveLength(1);
    });

    test('the pending-request limit refuses another bot request', async ({hub}) => {
        const requester = await hub.createUser();
        await hub.withPluginConfig({maxpendingrequestsperuser: '1'}, async () => {
            const s = await submitOK(hub, requester, {username: botName()});
            const second = botName();
            expect(await submitBot(hub, requester.username, {username: second})).toEqual({
                error: 'you have 1 pending requests waiting for review; you can submit more once some are approved, denied or expire',
            });
            expect(await pendingBotIds(hub, requester.id)).toEqual([s.requestId]);
            expect(await botCardsMentioning(hub, `@${second}"`)).toEqual([]);
        });
    });

    test('turned off, every way to submit is refused', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig({allowbottokenrequests: false}, async () => {
            expect(await submitBot(hub, requester.username, {username})).toEqual({error: DISABLED});
            expect(await hub.slash(requester.username, `bot ${username} Off Bot`)).toBe(DISABLED);
            expect(await hub.slash(requester.username, 'bot')).toBe(DISABLED);

            // The dialog route checks too, for a form opened before the switch.
            const client = await hub.client(requester.username);
            const townSquare = await client.getChannelByName(hub.teamId, 'town-square');
            const res = await client.submitInteractiveDialog({
                url: `/plugins/${PLUGIN_ID}/api/v1/bot_dialog`,
                callback_id: 'bot_request',
                state: '',
                user_id: requester.id,
                channel_id: townSquare.id,
                team_id: hub.teamId,
                submission: {bot_username: username, bot_display_name: 'Off Bot'},
                cancelled: false,
            });
            expect(res.error).toBe(DISABLED);
        });
        await expectNothingSubmitted(hub, requester, username);
    });

    test('a username that is already taken is refused', async ({hub}) => {
        // The username is only checked when the bot is created, after both
        // approvals: see 'a username taken before the final approval'.
        test.fail(true, 'BUG: submit does not check that the bot username is free, so a doomed request reaches the approvers');
        const requester = await hub.createUser();
        const taken = await hub.createUser({prefix: 'tk'});
        const res = await submitBot(hub, requester.username, {username: taken.username});
        expect(res.error).toBeTruthy();
        expect(await botRecordsOf(hub, requester.id)).toEqual([]);
        expect(await botCardsMentioning(hub, `@${taken.username}"`)).toEqual([]);
    });

    test.describe('slash usage errors', () => {
        const cases: Array<[string, (u: string) => string, string]> = [
            ['no display name', (u) => `bot ${u}`, 'Could not read the request: a bot username and a display name are required.\nUsage: `/mattermost-request-hub bot <username> <Display name>'],
            ['unknown owner', (u) => `bot ${u} Name --owner @nosuchuser${u}`, 'unknown user: nosuchuser'],
            ['short username', () => 'bot ab Short Bot', 'Bot username must be 3-22 characters.'],
        ];
        for (const [title, args, reply] of cases) {
            test(title, async ({hub}) => {
                const requester = await hub.createUser();
                const username = botName();
                expect(await hub.slash(requester.username, args(username))).toContain(reply);
                await expectNothingSubmitted(hub, requester, username);
            });
        }
    });

    // Each case builds a body for the requester and names the error submit
    // must return.
    type Case = {title: string; error: string; body: (hub: RequestHub, requester: TestUser, username: string) => Promise<BotRequestBody>};
    const townSquare = (hub: RequestHub) => hub.channelId('town-square');
    const newChannel = async (hub: RequestHub, type: 'O' | 'P', members: TestUser[] = []) => {
        const admin = await hub.client(ADMIN);
        const ch = await admin.createChannel({team_id: hub.teamId, name: uid('ch'), display_name: uid('Ch'), type} as Parameters<typeof admin.createChannel>[0]);
        for (const m of members) {
            await admin.addToChannel(m.id, ch.id);
        }
        return ch.id;
    };
    const cases: Case[] = [
        {title: 'blank username', error: 'A bot username is required.', body: async () => ({username: '   '})},
        {title: 'username too short', error: 'Bot username must be 3-22 characters.', body: async () => ({username: 'ab'})},
        {title: 'username too long', error: 'Bot username must be 3-22 characters.', body: async (_h, _r, u) => ({username: u.padEnd(23, 'x')})},
        {title: 'username with invalid characters', error: USERNAME_CHARS, body: async (_h, _r, u) => ({username: `${u.slice(0, 18)}*bot`})},
        ...['all', 'channel', 'system', 'matterbot'].map((reserved): Case => ({title: `reserved username ${reserved}`, error: USERNAME_CHARS, body: async () => ({username: reserved})})),
        {title: 'blank display name', error: 'A display name is required.', body: async (_h, _r, u) => ({username: u, display_name: '   '})},
        {title: 'display name too long', error: 'Display name must be 64 characters or fewer', body: async (_h, _r, u) => ({username: u, display_name: 'd'.repeat(65)})},
        {title: 'description too long', error: 'Description must be 500 characters or fewer', body: async (_h, _r, u) => ({username: u, description: 'd'.repeat(501)})},
        {
            title: 'incoming webhook name too long in bytes',
            error: 'Incoming webhook name must be 64 bytes or fewer (some characters count as more than one)',
            body: async (h, _r, u) => ({username: u, incoming_webhook_channel_id: await townSquare(h), incoming_webhook_display_name: 'é'.repeat(33)}),
        },
        {
            title: 'outgoing webhook name too long in bytes',
            error: 'Outgoing webhook name must be 64 bytes or fewer (some characters count as more than one)',
            body: async (h, _r, u) => ({username: u, outgoing_webhook_channel_id: await townSquare(h), outgoing_webhook_display_name: 'é'.repeat(33), outgoing_webhook_callback_url: 'https://example.com/hook'}),
        },
        {
            title: 'outgoing webhook without a callback URL',
            error: 'the callback URL must be an http or https address',
            body: async (h, _r, u) => ({username: u, outgoing_webhook_channel_id: await townSquare(h)}),
        },
        {
            title: 'outgoing webhook with a non-http callback URL',
            error: 'the callback URL must be an http or https address',
            body: async (h, _r, u) => ({username: u, outgoing_webhook_channel_id: await townSquare(h), outgoing_webhook_callback_url: 'ftp://example.com/hook'}),
        },
        {title: 'owner that is not an ID', error: 'The bot owner must be picked from the list.', body: async (_h, _r, u) => ({username: u, bot_owner: 'not-an-id'})},
        {
            title: 'deactivated owner',
            error: 'The bot owner must be an active user.',
            body: async (h, _r, u) => {
                const owner = await h.createUser();
                await (await h.client(ADMIN)).updateUserActive(owner.id, false);
                return {username: u, bot_owner: owner.id};
            },
        },
        {
            title: 'bot as owner',
            error: 'The bot owner must be an active user.',
            body: async (h, _r, u) => ({username: u, bot_owner: (await (await h.client(ADMIN)).getUserByUsername(HUB_BOT)).id}),
        },
        {
            title: 'incoming webhook channel that does not exist',
            error: 'incoming webhook channel: could not find the target channel',
            body: async (_h, _r, u) => ({username: u, incoming_webhook_channel_id: 'x'.repeat(26)}),
        },
        {
            title: 'incoming webhook channel the requester is not in',
            error: 'incoming webhook channel: you must be a member of the channel to request a webhook for it',
            body: async (h, _r, u) => ({username: u, incoming_webhook_channel_id: await newChannel(h, 'P')}),
        },
        {
            title: 'outgoing webhook channel that is private',
            error: 'outgoing webhook channel: outgoing webhooks can only use public channels',
            body: async (h, r, u) => ({username: u, outgoing_webhook_channel_id: await newChannel(h, 'P', [r]), outgoing_webhook_callback_url: 'https://example.com/hook'}),
        },
        {
            title: 'archived incoming webhook channel',
            error: 'incoming webhook channel: webhooks can\'t target an archived channel',
            body: async (h, r, u) => {
                const id = await newChannel(h, 'O', [r]);
                await (await h.client(ADMIN)).deleteChannel(id);
                return {username: u, incoming_webhook_channel_id: id};
            },
        },
        {
            title: 'direct message as incoming webhook channel',
            error: 'incoming webhook channel: webhooks can only target public or private channels',
            body: async (h, r, u) => {
                const c = await h.client(r.username);
                const dm = await c.createDirectChannel([r.id, h.userIds.bob]);
                return {username: u, incoming_webhook_channel_id: dm.id};
            },
        },
    ];
    for (const c of cases) {
        test(`refused: ${c.title}`, async ({hub}) => {
            const requester = await hub.createUser();
            const body = await c.body(hub, requester, botName());
            expect(await submitBot(hub, requester.username, body)).toEqual({error: c.error});
            await expectNothingSubmitted(hub, requester, body.username ?? '');
        });
    }
});

test.describe('system_admin mode', () => {
    test('one System Admin fills both steps; the bot, token and token DM are created', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username, display_name: 'Token Bot', request_token: true});
            await click(hub, ADMIN, s.card.id, 'approve');
            const botId = await expectApproved(hub, requester, s, username, ADMIN);

            const tokens = await (await hub.client(ADMIN)).getUserAccessTokensForUser(botId);
            expect(tokens).toHaveLength(1);
            expect(tokens[0].description).toBe(`API token for @${username}`);
            const tokenDM = await hub.findDM(requester.username, '**Access Token**');
            expect(tokenDM.message).toContain('Save this now.');
            expect(await hub.kv<Array<{post_id: string}>>('token_post_registry')).toEqual(expect.arrayContaining([expect.objectContaining({post_id: tokenDM.id})]));
        });
    });

    test('without a token, no token is created or sent', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username});
            await click(hub, ADMIN, s.card.id, 'approve');
            const botId = await expectApproved(hub, requester, s, username, ADMIN);
            expect(await (await hub.client(ADMIN)).getUserAccessTokensForUser(botId)).toEqual([]);
            expect(await dmsMentioning(hub, requester.username, 'Access Token')).toEqual([]);
        });
    });

    test('webhook extras are created for the bot and the incoming URL is sent', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        const channelId = await hub.channelId('town-square');
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {
                username,
                incoming_webhook_channel_id: channelId,
                incoming_webhook_display_name: 'In Hook',
                outgoing_webhook_channel_id: channelId,
                outgoing_webhook_display_name: 'Out Hook',
                outgoing_webhook_callback_url: 'https://example.com/hook',
            });
            await click(hub, ADMIN, s.card.id, 'approve');
            const botId = await expectApproved(hub, requester, s, username, ADMIN);

            const admin = await hub.client(ADMIN);
            const incoming = (await admin.getIncomingWebhooks(hub.teamId, 0, 200) as IncomingWebhook[]).find((h) => h.user_id === botId);
            expect(incoming).toMatchObject({channel_id: channelId, display_name: 'In Hook', channel_locked: true});
            const outgoing = (await admin.getOutgoingWebhooks('', hub.teamId, 0, 200)).find((h) => h.creator_id === botId);
            expect(outgoing).toMatchObject({channel_id: channelId, display_name: 'Out Hook', callback_urls: ['https://example.com/hook']});
            await hub.findDM(requester.username, `/hooks/${incoming!.id}`);
            await hub.findDM(requester.username, '**Outgoing webhook** *Out Hook* is now active.');
        });
    });

    test('a System Admin who requested the bot cannot approve it', async ({hub}) => {
        const requester = await hub.createUser({systemAdmin: true});
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            const ephemerals = await listenForEphemerals(hub, requester.username);
            try {
                await click(hub, requester.username, s.card.id, 'approve');
                await ephemerals.next(REQUESTER_CANNOT);
            } finally {
                ephemerals.close();
            }
            const card = await getPost(hub, s.card.id);
            expect(attachmentOf(card)?.fields?.[0]).toMatchObject({title: '🚫 Requester cannot approve:'});
            await expectStillPending(hub, requester, s, before, username);
        });
    });

    test('a user who is not a System Admin cannot approve or deny', async ({hub}) => {
        const requester = await hub.createUser();
        const teamAdmin = await hub.createUser({teamAdmin: true});
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username});
            await joinBotChannel(hub, teamAdmin);
            const before = await storedBotRequest(hub, s.requestId);
            const ephemerals = await listenForEphemerals(hub, teamAdmin.username);
            try {
                await click(hub, teamAdmin.username, s.card.id, 'approve');
                await ephemerals.next(`⚠️ ${NO_PERMISSION}`);
                await click(hub, teamAdmin.username, s.card.id, 'deny');
                await ephemerals.next('You don\'t have permission to deny this request.');
            } finally {
                ephemerals.close();
            }
            await expectStillPending(hub, requester, s, before, username);
        });
    });

    test('the requester, who cannot see the bot channel, cannot click its buttons', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            await expect(click(hub, requester.username, s.card.id, 'approve')).rejects.toMatchObject({status_code: 403});
            await expect(click(hub, requester.username, s.card.id, 'deny')).rejects.toMatchObject({status_code: 403});
            await expectStillPending(hub, requester, s, before, username);
        });
    });

    test('approve by ID with the slash command finishes the request', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username});
            expect(await hub.slash(ADMIN, `approve ${s.requestId}`)).toBe(`✅ Fully approved (security + system) for @${requester.username}. Bot **@${username}** created.`);
            await expectApproved(hub, requester, s, username, ADMIN);
            expect(await hub.slash(ADMIN, `approve ${s.requestId}`)).toBe('This request has already been handled.');
        });
    });

    test('turned off after submit: approve is refused, deny still closes it', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            await hub.withPluginConfig({allowbottokenrequests: false}, async () => {
                const ephemerals = await listenForEphemerals(hub, ADMIN);
                try {
                    await click(hub, ADMIN, s.card.id, 'approve');
                    await ephemerals.next('Requests of this type are turned off. Deny this request instead.');
                } finally {
                    ephemerals.close();
                }
                await expectStillPending(hub, requester, s, before, username);
                await click(hub, ADMIN, s.card.id, 'deny');
                await expectDenied(hub, requester, s, username, ADMIN);
            });
        });
    });

    test('a deactivated requester blocks approval until the request is denied', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            await (await hub.client(ADMIN)).updateUserActive(requester.id, false);
            expect(await hub.slash(ADMIN, `approve ${s.requestId}`)).toBe('The requester\'s account is no longer active. Deny this request instead.');
            const ephemerals = await listenForEphemerals(hub, ADMIN);
            try {
                await click(hub, ADMIN, s.card.id, 'approve');
                await ephemerals.next('The requester\'s account is no longer active. Deny this request instead.');
            } finally {
                ephemerals.close();
            }
            expect(await storedBotRequest(hub, s.requestId)).toEqual(before);
            expect(actionIds(await getPost(hub, s.card.id))).toEqual(['approve', 'deny']);
            expect(await userNamed(hub, username)).toBeUndefined();

            await click(hub, ADMIN, s.card.id, 'deny');
            await expect.poll(async () => (await getPost(hub, s.card.id)).message).toBe(`❌ Denied by @${ADMIN} for @${requester.username}. Bot **@${username}**.`);
            expect(await storedBotRequest(hub, s.requestId)).toBeUndefined();
            expect(await pendingBotIds(hub, requester.id)).not.toContain(s.requestId);
        });
    });

    test('a deactivated owner blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const owner = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const s = await submitOK(hub, requester, {username, bot_owner: owner.id});
            const before = await storedBotRequest(hub, s.requestId);
            await (await hub.client(ADMIN)).updateUserActive(owner.id, false);
            expect(await hub.slash(ADMIN, `approve ${s.requestId}`)).toBe('The bot owner is no longer an active user. Deny this request instead.');
            await expectStillPending(hub, requester, s, before, username);
        });
    });

    test('an archived webhook channel or a requester who left it blocks approval', async ({hub}) => {
        const requester = await hub.createUser();
        const admin = await hub.client(ADMIN);
        const archived = await admin.createChannel({team_id: hub.teamId, name: uid('ch'), display_name: uid('Ch'), type: 'O'} as Parameters<typeof admin.createChannel>[0]);
        const left = await admin.createChannel({team_id: hub.teamId, name: uid('ch'), display_name: uid('Ch'), type: 'O'} as Parameters<typeof admin.createChannel>[0]);
        await admin.addToChannel(requester.id, archived.id);
        await admin.addToChannel(requester.id, left.id);
        const first = botName();
        const second = botName();
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const a = await submitOK(hub, requester, {username: first, incoming_webhook_channel_id: archived.id});
            const b = await submitOK(hub, requester, {username: second, outgoing_webhook_channel_id: left.id, outgoing_webhook_callback_url: 'https://example.com/hook'});
            const beforeA = await storedBotRequest(hub, a.requestId);
            const beforeB = await storedBotRequest(hub, b.requestId);
            await admin.deleteChannel(archived.id);
            await admin.removeFromChannel(requester.id, left.id);

            expect(await hub.slash(ADMIN, `approve ${a.requestId}`)).toBe(`~${archived.name} has been archived. Deny this request instead.`);
            expect(await hub.slash(ADMIN, `approve ${b.requestId}`)).toBe('The requester is no longer a member of the channel. Deny this request instead.');
            await expectStillPending(hub, requester, a, beforeA, first);
            await expectStillPending(hub, requester, b, beforeB, second);
        });
    });

    test.describe(() => {
        test.use({allowedPluginErrors: [/failed to create bot on approval/]});

        test('a username taken before the final approval keeps the request pending', async ({hub}) => {
            const requester = await hub.createUser();
            const username = botName();
            await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
                const s = await submitOK(hub, requester, {username, request_token: true});
                const before = await storedBotRequest(hub, s.requestId);
                await hub.mm.mmctl('user', 'create', '--email', `${username}@example.com`, '--username', username, '--password', PASSWORD);
                const squatter = (await userNamed(hub, username))!;

                await click(hub, ADMIN, s.card.id, 'approve');
                const card = await getPost(hub, s.card.id);
                expect(card.message).toContain('a bot account request needs review (a previous approval attempt failed).');
                const notice = attachmentOf(card)?.fields?.[0];
                expect(notice?.title).toBe('⚠️ Action needed');
                expect(notice?.value).toMatch(/^Couldn't create the bot: .+\. The request is still pending — click \*\*Approve\*\* again to retry, or \*\*Deny\*\* it\.$/);
                expect(actionIds(card)).toEqual(['approve', 'deny']);

                // Nothing changed hands: the record is back as it was, still
                // pending, and the username still belongs to the user.
                expect(await storedBotRequest(hub, s.requestId)).toEqual(before);
                expect(await pendingBotIds(hub, requester.id)).toContain(s.requestId);
                const user = (await userNamed(hub, username))!;
                expect(user.id).toBe(squatter.id);
                expect(user.is_bot).toBeFalsy();
                expect(await (await hub.client(ADMIN)).getUserAccessTokensForUser(user.id)).toEqual([]);
                expect(await dmsMentioning(hub, requester.username, 'Access Token')).toEqual([]);

                await click(hub, ADMIN, s.card.id, 'deny');
                await expect.poll(async () => (await getPost(hub, s.card.id)).message).toContain('❌ Denied by');
                expect(await pendingBotIds(hub, requester.id)).not.toContain(s.requestId);
            });
        });
    });
});

test.describe('attribute mode', () => {
    test('security then system approval by two attribute holders creates the bot', async ({hub}) => {
        const requester = await hub.createUser();
        const security = await hub.createUser();
        const system = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async (pools) => {
            await pools.security(security);
            await pools.system(system);
            await joinBotChannel(hub, security, system);
            const reply = await hub.slash(requester.username, `bot ${username} Attr Bot`);
            expect(reply).toBe(SUBMITTED);
            const card = await hub.findCardIn(REQUEST_CHANNELS.bot, `@${username}`);
            const s = {card, requestId: cardRequestId(card)};

            await click(hub, security.username, s.card.id, 'approve');
            const afterFirst = await getPost(hub, s.card.id);
            expect(fieldValue(afterFirst, 'Approvals')).toBe(`🔒 Security: ✅ approved by @${security.username}\n🛠 System: ⏳ awaiting approval`);
            expect(actionIds(afterFirst)).toEqual(['approve', 'deny']);
            const afterFirstRecord = await storedBotRequest(hub, s.requestId);
            expect(afterFirstRecord).toMatchObject({security_approver_id: security.id});
            expect(afterFirstRecord?.system_approver_id).toBeUndefined();
            await hub.findDM(requester.username, `Your bot request for **@${username}** received the security approval; awaiting the other approval.`);
            expect(await userNamed(hub, username)).toBeUndefined();

            await click(hub, system.username, s.card.id, 'approve');
            await expectApproved(hub, requester, s, username, system.username);
        });
    });

    test('a token is delivered when the final approver is not a System Admin', async ({hub}) => {
        // The token is created with the final approver's session, and only
        // System Admins may create tokens for another user's bot. The bot
        // is created and the request closed as fully approved, but the
        // requester gets "token generation failed" instead of a token.
        test.fail(true, 'BUG: a system-pool approver who is not a System Admin cannot create the requested bot token');
        const requester = await hub.createUser();
        const security = await hub.createUser();
        const system = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async (pools) => {
            await pools.security(security);
            await pools.system(system);
            await joinBotChannel(hub, security, system);
            const s = await submitOK(hub, requester, {username, request_token: true});
            await click(hub, security.username, s.card.id, 'approve');
            await click(hub, system.username, s.card.id, 'approve');
            const botId = await expectApproved(hub, requester, s, username, system.username);
            expect(await dmsMentioning(hub, requester.username, 'token generation failed')).toEqual([]);
            expect(await (await hub.client(ADMIN)).getUserAccessTokensForUser(botId)).toHaveLength(1);
            await hub.findDM(requester.username, '**Access Token**');
        });
    });

    test('the security step can be approved by ID with the slash command', async ({hub}) => {
        const requester = await hub.createUser();
        const security = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async (pools) => {
            await pools.security(security);
            const s = await submitOK(hub, requester, {username});
            expect(await hub.slash(security.username, `approve ${s.requestId}`)).toBe('Approved the security step. The request now needs the other approval, from a different person.');
            expect(fieldValue(await getPost(hub, s.card.id), 'Approvals')).toContain(`Security: ✅ approved by @${security.username}`);

            // A System Admin qualifies for the system step.
            expect(await hub.slash(ADMIN, `approve ${s.requestId}`)).toBe(`✅ Fully approved (security + system) for @${requester.username}. Bot **@${username}** created.`);
            await expectApproved(hub, requester, s, username, ADMIN);
        });
    });

    test('a System Admin cannot approve before security has', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async () => {
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            const ephemerals = await listenForEphemerals(hub, ADMIN);
            try {
                await click(hub, ADMIN, s.card.id, 'approve');
                await ephemerals.next(SECURITY_FIRST);
            } finally {
                ephemerals.close();
            }
            const card = await getPost(hub, s.card.id);
            expect(attachmentOf(card)?.fields?.[0]).toMatchObject({title: '🔒 Security review required first:'});
            expect(card.message).toContain(HEADER);
            await expectStillPending(hub, requester, s, before, username);
        }, {poolsMustWork: false});
    });

    test('a system-pool member cannot approve before security has', async ({hub}) => {
        const requester = await hub.createUser();
        const system = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async (pools) => {
            await pools.system(system);
            await joinBotChannel(hub, system);
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            const ephemerals = await listenForEphemerals(hub, system.username);
            try {
                await click(hub, system.username, s.card.id, 'approve');
                await ephemerals.next(SECURITY_FIRST);
            } finally {
                ephemerals.close();
            }
            await expectStillPending(hub, requester, s, before, username);
        });
    });

    test('one person holding both attributes cannot fill both steps', async ({hub}) => {
        const requester = await hub.createUser();
        const both = await hub.createUser();
        const security = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async (pools) => {
            await pools.security(both);
            await pools.system(both);
            await pools.security(security);
            await joinBotChannel(hub, both, security);
            const s = await submitOK(hub, requester, {username});

            // A dual-role user's first click fills security.
            await click(hub, both.username, s.card.id, 'approve');
            const afterFirst = await storedBotRequest(hub, s.requestId);
            expect(afterFirst).toMatchObject({security_approver_id: both.id});
            expect(afterFirst?.system_approver_id).toBeUndefined();

            const bothEph = await listenForEphemerals(hub, both.username);
            const secEph = await listenForEphemerals(hub, security.username);
            try {
                await click(hub, both.username, s.card.id, 'approve');
                await bothEph.next(`⚠️ ${ALREADY_APPROVED_STEP}`);

                // Another security-only member can't take the system step.
                await click(hub, security.username, s.card.id, 'approve');
                await secEph.next(`⚠️ ${SYSTEM_ONLY_LEFT}`);
            } finally {
                bothEph.close();
                secEph.close();
            }
            await expectStillPending(hub, requester, s, afterFirst, username);
        });
    });

    test('a requester in the security pool cannot approve their own request', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async (pools) => {
            await pools.security(requester);
            await joinBotChannel(hub, requester);
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            const ephemerals = await listenForEphemerals(hub, requester.username);
            try {
                await click(hub, requester.username, s.card.id, 'approve');
                await ephemerals.next(REQUESTER_CANNOT);
            } finally {
                ephemerals.close();
            }
            const card = await getPost(hub, s.card.id);
            expect(attachmentOf(card)?.fields?.[0]).toMatchObject({
                title: '🚫 Requester cannot approve:',
                value: `**@${requester.username}** (who created this ticket) clicked **Approve**. Requesters cannot approve their own request — another security or system approver must review it.`,
            });
            await expectStillPending(hub, requester, s, before, username);
        }, {poolsMustWork: false});
    });

    test('a user outside both pools cannot approve or deny', async ({hub}) => {
        const requester = await hub.createUser();
        const outsider = await hub.createUser({teamAdmin: true}); // a Team Admin keeps access to the channel
        const username = botName();
        await withAttributePools(hub, async () => {
            await joinBotChannel(hub, outsider);
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            const ephemerals = await listenForEphemerals(hub, outsider.username);
            try {
                await click(hub, outsider.username, s.card.id, 'approve');
                await ephemerals.next(`⚠️ ${NO_PERMISSION}`);
                await click(hub, outsider.username, s.card.id, 'deny');
                await ephemerals.next('You don\'t have permission to deny this request.');
            } finally {
                ephemerals.close();
            }
            await expectStillPending(hub, requester, s, before, username);
        }, {poolsMustWork: false});
    });

    test('a user-writable attribute grants nothing', async ({hub}) => {
        const requester = await hub.createUser();
        const self = await hub.createUser({teamAdmin: true});
        const username = botName();
        const attr = uid('self');
        const fieldId = await createAttribute(hub, attr, false);
        try {
            await hub.withPluginConfig({twostepapprovermode: 'attribute', securityapproverattribute: attr, systemapproverattribute: attr}, async () => {
                // The user gives themselves the attribute.
                await (await hub.client(self.username)).updateCustomProfileAttributeValues({[fieldId]: 'yes'});
                await joinBotChannel(hub, self);
                const s = await submitOK(hub, requester, {username});
                const before = await storedBotRequest(hub, s.requestId);
                const ephemerals = await listenForEphemerals(hub, self.username);
                try {
                    await click(hub, self.username, s.card.id, 'approve');
                    await ephemerals.next(`⚠️ ${NO_PERMISSION}`);
                } finally {
                    ephemerals.close();
                }
                await expectStillPending(hub, requester, s, before, username);
            });
        } finally {
            await (await hub.client(ADMIN)).deleteCustomProfileAttributeField(fieldId);
        }
    });

    test('deny at the security step closes the request', async ({hub}) => {
        const requester = await hub.createUser();
        const security = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async (pools) => {
            await pools.security(security);
            await joinBotChannel(hub, security);
            const s = await submitOK(hub, requester, {username, request_token: true});
            await click(hub, security.username, s.card.id, 'deny');
            await expectDenied(hub, requester, s, username, security.username);
            expect(await dmsMentioning(hub, requester.username, 'Access Token')).toEqual([]);
        });
    });

    test('deny at the system step closes a half-approved request', async ({hub}) => {
        const requester = await hub.createUser();
        const security = await hub.createUser();
        const system = await hub.createUser();
        const username = botName();
        await withAttributePools(hub, async (pools) => {
            await pools.security(security);
            await pools.system(system);
            await joinBotChannel(hub, security, system);
            const s = await submitOK(hub, requester, {username});
            await click(hub, security.username, s.card.id, 'approve');
            expect(await storedBotRequest(hub, s.requestId)).toMatchObject({security_approver_id: security.id});
            await click(hub, system.username, s.card.id, 'deny');
            await expectDenied(hub, requester, s, username, system.username);
        });
    });

    test('with no security attribute set, System Admins cover security but two are needed', async ({hub}) => {
        const requester = await hub.createUser();
        const secondAdmin = await hub.createUser({systemAdmin: true});
        const username = botName();
        await withAttributePools(hub, async () => {
            await joinBotChannel(hub, secondAdmin);
            const s = await submitOK(hub, requester, {username});
            await click(hub, ADMIN, s.card.id, 'approve');
            const afterFirst = await storedBotRequest(hub, s.requestId);
            expect(afterFirst).toMatchObject({security_approver_id: hub.userIds[ADMIN]});
            expect(afterFirst?.system_approver_id).toBeUndefined();

            expect(await hub.slash(ADMIN, `approve ${s.requestId}`)).toBe(`⚠️ ${ALREADY_APPROVED_STEP}`);
            await expectStillPending(hub, requester, s, afterFirst, username);

            await click(hub, secondAdmin.username, s.card.id, 'approve');
            await expectApproved(hub, requester, s, username, secondAdmin.username);
        }, {extra: {securityapproverattribute: ''}, poolsMustWork: false});
    });
});

test.describe('usergroup mode', () => {
    test('synced groups are the pools; one member cannot fill both steps', async ({hub}) => {
        const requester = await hub.createUser();
        const both = await hub.createUser();
        const system = await hub.createUser();
        const outsider = await hub.createUser({teamAdmin: true});
        const securityGroup = await createGroup(hub, [both]);
        const systemGroup = await createGroup(hub, [both, system]);
        const username = botName();
        await hub.withPluginConfig({twostepapprovermode: 'usergroup', securityapprovergroup: securityGroup, systemapprovergroup: `@${systemGroup}`}, async () => {
            await joinBotChannel(hub, both, system, outsider);
            const s = await submitOK(hub, requester, {username});

            const sysEph = await listenForEphemerals(hub, system.username);
            const outEph = await listenForEphemerals(hub, outsider.username);
            const bothEph = await listenForEphemerals(hub, both.username);
            try {
                const before = await storedBotRequest(hub, s.requestId);
                await click(hub, system.username, s.card.id, 'approve');
                await sysEph.next(SECURITY_FIRST);
                await click(hub, outsider.username, s.card.id, 'approve');
                await outEph.next(`⚠️ ${NO_PERMISSION}`);
                await expectStillPending(hub, requester, s, before, username);

                await click(hub, both.username, s.card.id, 'approve');
                const afterFirst = await storedBotRequest(hub, s.requestId);
                expect(afterFirst).toMatchObject({security_approver_id: both.id});
            expect(afterFirst?.system_approver_id).toBeUndefined();
                await click(hub, both.username, s.card.id, 'approve');
                await bothEph.next(`⚠️ ${ALREADY_APPROVED_STEP}`);
                await expectStillPending(hub, requester, s, afterFirst, username);
            } finally {
                sysEph.close();
                outEph.close();
                bothEph.close();
            }

            await click(hub, system.username, s.card.id, 'approve');
            await expectApproved(hub, requester, s, username, system.username);
        });
    });

    test('a custom group is refused as the security pool', async ({hub}) => {
        const requester = await hub.createUser();
        const member = await hub.createUser({teamAdmin: true});
        const customGroup = await createGroup(hub, [member], 'custom');
        const username = botName();
        await hub.withPluginConfig({twostepapprovermode: 'usergroup', securityapprovergroup: customGroup, systemapprovergroup: ''}, async () => {
            await joinBotChannel(hub, member);
            const s = await submitOK(hub, requester, {username});
            const before = await storedBotRequest(hub, s.requestId);
            const memberEph = await listenForEphemerals(hub, member.username);
            const adminEph = await listenForEphemerals(hub, ADMIN);
            try {
                await click(hub, member.username, s.card.id, 'approve');
                await memberEph.next(`⚠️ ${NO_PERMISSION}`);

                // The pool is set, so System Admins don't cover security.
                await click(hub, ADMIN, s.card.id, 'approve');
                await adminEph.next(SECURITY_FIRST);
            } finally {
                memberEph.close();
                adminEph.close();
            }
            await expectStillPending(hub, requester, s, before, username);
        });
    });
});
