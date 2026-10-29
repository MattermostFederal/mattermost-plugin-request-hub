import type {IncomingWebhook} from '@mattermost/types/integrations';

import {expect, test} from '../helpers/fixtures';
import {ADMIN, PLUGIN_ID, REQUEST_CHANNELS, TEAM, cardRequestId, uid, type RequestHub, type TestUser} from '../helpers/requesthub';
import {
    AWAITING_BOTH,
    attachmentOf,
    createChannel,
    expectNothingQueued,
    expectStillPending,
    fieldOf,
    hooksNamed,
    pendingIds,
    plain,
    postsMentioning,
    submitIncoming,
    submitOutgoing,
    webhookRecord,
    webhookRecordKeys,
} from '../helpers/webhooks';

const SUBMITTED = 'Your webhook request has been submitted for approval. It needs a security approval and a system approval.';
const CALLBACK = 'https://example.com/mattermost-hook';

// A valid callback URL exactly bytes long.
const longCallback = (bytes: number) => CALLBACK + '/' + 'a'.repeat(bytes - CALLBACK.length - 1);

// In system_admin mode one System Admin fills both steps with one click.
const SYSTEM_ADMIN_MODE = {twostepapprovermode: 'system_admin'};

// Approves or denies by request ID with the slash command and returns the
// reply, which (unlike a card click) carries the refusal text.
const decide = (hub: RequestHub, username: string, verb: 'approve' | 'deny', requestId: string) => hub.slash(username, `${verb} ${requestId}`);

// Submits a request for a fresh requester in a fresh channel and
// returns everything the approval-time tests need.
async function submitPending(hub: RequestHub, type: 'O' | 'P' = 'O', webhookType: 'incoming' | 'outgoing' = 'incoming') {
    const requester = await hub.createUser();
    const channel = await createChannel(hub, type, [requester]);
    const name = uid('Hook ');
    const body = {channel_id: channel.id, display_name: name, description: 'e2e', callback_url: CALLBACK};
    const res = webhookType === 'incoming' ? await submitIncoming(hub, requester, body) : await submitOutgoing(hub, requester, body);
    expect(res, JSON.stringify(res)).toEqual({message: SUBMITTED});
    const card = await hub.findCardIn(REQUEST_CHANNELS.webhook, name);
    return {requester, channel, name, card, requestId: cardRequestId(card)};
}

async function expectDeniedCleanly(hub: RequestHub, requester: TestUser, requestId: string, card: {id: string}) {
    const reply = await decide(hub, ADMIN, 'deny', requestId);
    expect(reply).toContain(`❌ Denied by @${ADMIN}`);
    expect(await webhookRecord(hub, requestId)).toBeUndefined();
    expect(await pendingIds(hub, requester.id)).not.toContain(requestId);
    const now = await (await hub.client(ADMIN)).getPost(card.id);
    expect(now.message).toContain('❌ Denied');
    expect(attachmentOf(now)?.actions ?? []).toEqual([]);
}

test.describe('webhook requests', () => {
    test('incoming via REST: card, ticket DM, approval creates a working, locked hook owned by the requester', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const requester = await hub.createUser();
            const channel = await createChannel(hub, 'O', [requester]);
            const name = uid('Alerts ');
            const res = await submitIncoming(hub, requester, {channel_id: channel.id, display_name: name, description: 'Monitoring alerts'});
            expect(res, JSON.stringify(res)).toEqual({message: SUBMITTED});

            const card = await hub.findCardIn(REQUEST_CHANNELS.webhook, name);
            expect(card.message).toContain('a webhook request needs review (requires a security approval and a system approval)');
            expect(fieldOf(card, 'Requested by')).toBe(`@${requester.username}`);
            expect(fieldOf(card, 'Type')).toBe('Incoming webhook');
            expect(fieldOf(card, 'Target channel')).toBe(`~${channel.name}`);
            expect(fieldOf(card, 'Approvals')).toBe(AWAITING_BOTH);
            expect(attachmentOf(card)?.text).toContain('Monitoring alerts');
            expect(attachmentOf(card)?.actions?.map((a) => a.id)).toEqual(['approve', 'deny']);
            const requestId = cardRequestId(card);

            const record = (await webhookRecord(hub, requestId))!;
            expect(record).toMatchObject({requester_id: requester.id, webhook_type: 'incoming', channel_id: channel.id, team_id: hub.teamId, display_name: name, approval_post_id: card.id});
            expect(await pendingIds(hub, requester.id)).toEqual([requestId]);

            const ticket = await hub.findDM(requester.username, name);
            expect(ticket.id).toBe(record.dm_root_post_id);
            expect(ticket.message).toContain('Your webhook request has been submitted for approval');

            // The requester can't see the webhook channel, so the click is refused before the plugin.
            const requesterClient = await hub.client(requester.username);
            await expect(requesterClient.doPostAction(card.id, 'approve')).rejects.toMatchObject({status_code: 403});
            expect(await webhookRecord(hub, requestId)).toBeTruthy();

            const admin = await hub.client(ADMIN);
            await admin.doPostAction(card.id, 'approve');

            await expect.poll(async () => (await admin.getPost(card.id)).message).toContain(`✅ Fully approved (security + system) for @${requester.username}. incoming webhook **${name}** created.`);
            expect(attachmentOf(await admin.getPost(card.id))?.actions ?? []).toEqual([]);

            const {incoming} = await hooksNamed(hub, name);
            expect(incoming).toHaveLength(1);
            const hook = incoming[0] as IncomingWebhook & {channel_locked: boolean};
            expect(hook).toMatchObject({channel_id: channel.id, team_id: hub.teamId, user_id: requester.id, channel_locked: true, description: 'Monitoring alerts'});

            // The ticket DM root is updated with the URL and a thread reply records the decision.
            await expect.poll(async () => attachmentOf(await requesterClient.getPost(ticket.id))?.title).toBe('Webhook Request Approved');
            const dmText = attachmentOf(await requesterClient.getPost(ticket.id))?.text ?? '';
            expect(dmText).toContain(`${hub.mm.url()}/hooks/${hook.id}`);
            const thread = await requesterClient.getPostThread(ticket.id);
            expect(Object.values(thread.posts).map((p) => p.message)).toContain(`✅ **Approved** by @${ADMIN}.`);

            const audit = await hub.findAudit(name);
            expect(attachmentOf(audit)?.title).toBe('✅ Approved');
            expect(attachmentOf(audit)?.text).toContain(`@${ADMIN} gave final approval; incoming webhook "${name}" created for @${requester.username}`);

            expect(await webhookRecord(hub, requestId)).toBeUndefined();
            expect(await hub.kv(`pending_webhook_${requester.id}`)).toBeUndefined();

            // The hook works: it posts into the channel as the requester.
            const text = uid('hello from the hook ');
            const hookRes = await fetch(`${hub.mm.url()}/hooks/${hook.id}`, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({text})});
            expect(hookRes.status, await hookRes.text()).toBe(200);
            await expect.poll(async () => (await postsMentioning(hub, channel.id, text)).map((p) => p.user_id)).toEqual([requester.id]);
        });
    });

    test('outgoing via REST at the length limits: approval creates the hook with the callback and JSON content type', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const requester = await hub.createUser();
            const channel = await createChannel(hub, 'O', [requester]);

            // Exactly 64 bytes of name (with two-byte characters), 500 of
            // description and a 1020-byte callback URL, the longest
            // Mattermost can store (see the callback length test below).
            const base = uid('Relay');
            const name = base + 'é'.repeat((64 - base.length) / 2) + (base.length % 2 ? 'x' : '');
            expect(Buffer.byteLength(name)).toBe(64);
            const description = 'd'.repeat(500);
            const callback = longCallback(1020);

            const res = await submitOutgoing(hub, requester, {channel_id: channel.id, display_name: name, description, callback_url: callback});
            expect(res, JSON.stringify(res)).toEqual({message: SUBMITTED});

            const card = await hub.findCardIn(REQUEST_CHANNELS.webhook, name);
            expect(fieldOf(card, 'Type')).toBe('Outgoing webhook');
            expect(plain(attachmentOf(card)?.text)).toContain(`**Callback URL:** ${callback}`);
            const requestId = cardRequestId(card);

            const admin = await hub.client(ADMIN);
            await admin.doPostAction(card.id, 'approve');
            await expect.poll(async () => (await admin.getPost(card.id)).message).toContain(`outgoing webhook **${name}** created.`);

            const {outgoing} = await hooksNamed(hub, name);
            expect(outgoing).toHaveLength(1);
            expect(outgoing[0]).toMatchObject({
                channel_id: channel.id,
                team_id: hub.teamId,
                creator_id: requester.id,
                description,
                callback_urls: [callback],
                content_type: 'application/json',
            });

            // The plugin sets no trigger words: every message in the channel fires the hook.
            expect(outgoing[0].trigger_words ?? []).toEqual([]);

            await expect.poll(async () => attachmentOf(await hub.findDM(requester.username, 'Webhook Request Approved'))?.text).toContain(`Your outgoing webhook **${name}** was approved and is now active.`);
            expect(plain(attachmentOf(await hub.findAudit(name))?.text)).toContain(`outgoing webhook "${name}" created for @${requester.username}`);
            expect(await webhookRecord(hub, requestId)).toBeUndefined();
            expect(await hub.kv(`pending_webhook_${requester.id}`)).toBeUndefined();
        });
    });

    test('inline slash: incoming and outgoing requests are queued and decided by ID', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const requester = await hub.createUser();
            const channel = await createChannel(hub, 'O', [requester]);
            const inName = uid('Slash In ');
            const outName = uid('Slash Out ');

            expect(await hub.slash(requester.username, `webhook-incoming ~${channel.name} ${inName} --description from the slash command`)).toBe(SUBMITTED);
            expect(await hub.slash(requester.username, `webhook-outgoing ~${channel.name} ${outName} --callback ${CALLBACK} --description relay`)).toBe(SUBMITTED);

            const inCard = await hub.findCardIn(REQUEST_CHANNELS.webhook, inName);
            expect(fieldOf(inCard, 'Type')).toBe('Incoming webhook');
            expect(fieldOf(inCard, 'Target channel')).toBe(`~${channel.name}`);
            expect(attachmentOf(inCard)?.text).toContain('from the slash command');
            const outCard = await hub.findCardIn(REQUEST_CHANNELS.webhook, outName);
            expect(fieldOf(outCard, 'Type')).toBe('Outgoing webhook');
            expect(plain(attachmentOf(outCard)?.text)).toContain(`**Callback URL:** ${CALLBACK}`);

            const inId = cardRequestId(inCard);
            const outId = cardRequestId(outCard);
            expect((await pendingIds(hub, requester.id)).sort()).toEqual([inId, outId].sort());
            expect(await webhookRecord(hub, outId)).toMatchObject({webhook_type: 'outgoing', callback_url: CALLBACK, description: 'relay'});

            expect(await decide(hub, ADMIN, 'approve', inId)).toBe(`✅ Fully approved (security + system) for @${requester.username}. incoming webhook **${inName}** created.`);
            expect((await hooksNamed(hub, inName)).incoming).toHaveLength(1);

            expect(await decide(hub, ADMIN, 'deny', outId)).toBe(`❌ Denied by @${ADMIN} for @${requester.username}. outgoing webhook **${outName}**.`);
            expect((await hooksNamed(hub, outName)).outgoing).toEqual([]);

            // Deciding by ID resolves the cards in place.
            const admin = await hub.client(ADMIN);
            await expect.poll(async () => (await admin.getPost(inCard.id)).message).toContain('✅ Fully approved');
            await expect.poll(async () => (await admin.getPost(outCard.id)).message).toContain('❌ Denied');
            expect(await hub.kv(`pending_webhook_${requester.id}`)).toBeUndefined();
        });
    });

    test('deny: no hook, card and ticket closed, audit posted, pending entry released', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, name, card, requestId} = await submitPending(hub);
            const record = (await webhookRecord(hub, requestId))!;
            const admin = await hub.client(ADMIN);
            await admin.doPostAction(card.id, 'deny');

            await expect.poll(async () => (await admin.getPost(card.id)).message).toBe(`❌ Denied by @${ADMIN} for @${requester.username}. incoming webhook **${name}**.`);
            expect(attachmentOf(await admin.getPost(card.id))?.actions ?? []).toEqual([]);
            const requesterClient = await hub.client(requester.username);
            const root = await requesterClient.getPost(record.dm_root_post_id!);
            expect(attachmentOf(root)?.title).toBe('Webhook Request Denied');
            expect(attachmentOf(root)?.text).toContain(`Denied by @${ADMIN}. Your request for a incoming webhook **${name}** was not approved.`);
            const audit = await hub.findAudit(`denied a incoming webhook request for ${name}`);
            expect(attachmentOf(audit)?.title).toBe('❌ Denied');
            expect(await webhookRecord(hub, requestId)).toBeUndefined();
            expect(await hub.kv(`pending_webhook_${requester.id}`)).toBeUndefined();
            const hooks = await hooksNamed(hub, name);
            expect([...hooks.incoming, ...hooks.outgoing]).toEqual([]);
        });
    });
});

test.describe('webhook requests: refused at submit', () => {
    test('channels a hook cannot target', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const requester = await hub.createUser();
            const admin = await hub.client(ADMIN);
            const privateMember = await createChannel(hub, 'P', [requester]);
            const privateOther = await createChannel(hub, 'P');
            const publicOther = await createChannel(hub, 'O');
            const archived = await createChannel(hub, 'O', [requester]);
            await admin.deleteChannel(archived.id);
            const dm = await (await hub.client(requester.username)).createDirectChannel([requester.id, hub.userIds.bob]);

            const cases: Array<[string, 'incoming' | 'outgoing', string, string]> = [
                ['outgoing to a private channel', 'outgoing', privateMember.id, 'outgoing webhooks can only use public channels'],
                ['archived channel', 'incoming', archived.id, "webhooks can't target an archived channel"],
                ['direct message', 'incoming', dm.id, 'webhooks can only target public or private channels'],
                ['unknown channel ID', 'incoming', 'x'.repeat(26), 'could not find the target channel'],
                ['no channel', 'incoming', ' ', 'a target channel is required'],
                ['public channel the requester is not in', 'incoming', publicOther.id, 'you must be a member of the channel to request a webhook for it'],
                ['private channel the requester is not in', 'incoming', privateOther.id, 'you must be a member of the channel to request a webhook for it'],
            ];
            for (const [what, type, channelId, error] of cases) {
                await test.step(`REST: ${what}`, async () => {
                    const name = uid('Refused ');
                    const body = {channel_id: channelId, display_name: name, callback_url: CALLBACK};
                    const res = type === 'incoming' ? await submitIncoming(hub, requester, body) : await submitOutgoing(hub, requester, body);
                    expect(res).toEqual({error});
                    await expectNothingQueued(hub, requester, name);
                });
            }

            await test.step('slash: private channel the requester is not in looks unknown', async () => {
                const name = uid('Refused ');
                expect(await hub.slash(requester.username, `webhook-incoming ~${privateOther.name} ${name}`)).toBe(`unknown channel: ~${privateOther.name}`);
                await expectNothingQueued(hub, requester, name);
            });
            await test.step('slash: outgoing to a private channel', async () => {
                const name = uid('Refused ');
                expect(await hub.slash(requester.username, `webhook-outgoing ~${privateMember.name} ${name} --callback ${CALLBACK}`)).toBe('outgoing webhooks can only use public channels');
                await expectNothingQueued(hub, requester, name);
            });
            await test.step('dialog: outgoing to a private channel', async () => {
                const name = uid('Refused ');
                const client = await hub.client(requester.username);
                const res = await client.submitInteractiveDialog({
                    url: `/plugins/${PLUGIN_ID}/api/v1/outgoing_webhook_dialog`,
                    callback_id: 'outgoing_webhook_request',
                    state: '',
                    user_id: requester.id,
                    channel_id: privateMember.id,
                    team_id: hub.teamId,
                    submission: {channel_id: privateMember.id, display_name: name, purpose: '', callback_url: CALLBACK},
                    cancelled: false,
                });
                expect(res.error).toBe('outgoing webhooks can only use public channels');
                await expectNothingQueued(hub, requester, name);
            });
        });
    });

    test('bad names, descriptions and callback URLs', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const requester = await hub.createUser();
            const channel = await createChannel(hub, 'O', [requester]);
            const tooLong = (field: string, limit: number) => `${field} must be ${limit} bytes or fewer (some characters count as more than one)`;
            const badScheme = 'the callback URL must be an http or https address';

            // Each case's name (or a uid in it) is what expectNothingQueued looks for.
            const cases: Array<[string, 'incoming' | 'outgoing', (n: string) => {display_name: string; description?: string; callback_url?: string}, string, (n: string) => string]> = [
                ['blank name', 'incoming', () => ({display_name: '   '}), 'a display name is required', (n) => n],
                ['65-byte name', 'incoming', (n) => ({display_name: n.padEnd(65, 'x')}), tooLong('display name', 64), (n) => n],
                ['33 two-byte characters', 'outgoing', (n) => ({display_name: n + 'é'.repeat(33), callback_url: CALLBACK}), tooLong('display name', 64), (n) => n],
                ['501-byte description', 'incoming', (n) => ({display_name: n, description: 'd'.repeat(501)}), tooLong('description', 500), (n) => n],
                ['outgoing without a callback URL', 'outgoing', (n) => ({display_name: n, callback_url: ' '}), 'a callback URL is required for outgoing webhooks', (n) => n],
                ['ftp callback', 'outgoing', (n) => ({display_name: n, callback_url: 'ftp://example.com/hook'}), badScheme, (n) => n],
                ['javascript callback', 'outgoing', (n) => ({display_name: n, callback_url: 'javascript:alert(1)'}), badScheme, (n) => n],
                ['callback without a scheme', 'outgoing', (n) => ({display_name: n, callback_url: 'example.com/hook'}), badScheme, (n) => n],
                ['1023-byte callback URL', 'outgoing', (n) => ({display_name: n, callback_url: longCallback(1023)}), tooLong('the callback URL', 1022), (n) => n],
            ];
            for (const [what, type, fields, error, needle] of cases) {
                await test.step(what, async () => {
                    const n = uid('Bad');
                    const body = {channel_id: channel.id, ...fields(n)};
                    const res = type === 'incoming' ? await submitIncoming(hub, requester, body) : await submitOutgoing(hub, requester, body);
                    expect(res).toEqual({error});
                    await expectNothingQueued(hub, requester, needle(n));
                });
            }

            await test.step('slash: no display name', async () => {
                expect(await hub.slash(requester.username, `webhook-incoming ~${channel.name}`)).toContain('Could not read the request: a channel and a display name are required.');
                expect(await webhookRecordKeys(hub, requester.id)).toEqual([]);
            });
            await test.step('slash: outgoing without --callback', async () => {
                const name = uid('Bad');
                expect(await hub.slash(requester.username, `webhook-outgoing ~${channel.name} ${name}`)).toBe('a callback URL is required for outgoing webhooks');
                await expectNothingQueued(hub, requester, name);
            });
            await test.step('slash: incoming takes no callback', async () => {
                const name = uid('Bad');
                expect(await hub.slash(requester.username, `webhook-incoming ~${channel.name} ${name} --callback ${CALLBACK}`)).toContain('Could not read the request: unknown option `--callback`.');
                await expectNothingQueued(hub, requester, name);
            });
            await test.step('slash: bad callback scheme', async () => {
                const name = uid('Bad');
                expect(await hub.slash(requester.username, `webhook-outgoing ~${channel.name} ${name} --callback ftp://example.com/hook`)).toBe(badScheme);
                await expectNothingQueued(hub, requester, name);
            });
        });
    });

    test('a callback URL too long for Mattermost to store is refused', async ({hub}) => {
        // Mattermost stores callback_urls as a JSON array in a 1024-character
        // column, so one URL can be at most 1020 bytes. The plugin allows
        // 1022, and a 1021- or 1022-byte URL fails only at final approval
        // with "Unable to save the OutgoingWebhook".
        test.fail(true, 'BUG: maxCallbackURLLen is 1022 but Mattermost can only store a 1020-byte callback URL');
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const requester = await hub.createUser();
            const channel = await createChannel(hub, 'O', [requester]);
            const name = uid('Long ');
            const res = await submitOutgoing(hub, requester, {channel_id: channel.id, display_name: name, callback_url: longCallback(1021)});
            if (res.message) {
                // Don't leave the request queued on the shared server.
                const card = await hub.findCardIn(REQUEST_CHANNELS.webhook, name);
                await decide(hub, ADMIN, 'deny', cardRequestId(card));
            }
            expect(res.error).toBeTruthy();
            await expectNothingQueued(hub, requester, name);
        });
    });

    test('a second pending request with the same type, channel and name is refused', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, channel, name, requestId} = await submitPending(hub);

            // The name is compared case-insensitively.
            const dup = await submitIncoming(hub, requester, {channel_id: channel.id, display_name: name.toUpperCase()});
            expect(dup).toEqual({error: 'you already have a pending incoming webhook request with this name in this channel'});
            expect(await hub.slash(requester.username, `webhook-incoming ~${channel.name} ${name}`)).toBe('you already have a pending incoming webhook request with this name in this channel');
            expect(await pendingIds(hub, requester.id)).toEqual([requestId]);
            expect(await postsMentioning(hub, await hub.channelId(REQUEST_CHANNELS.webhook), name)).toHaveLength(1);
            expect(await webhookRecordKeys(hub, requester.id)).toHaveLength(1);

            // Another type with the same name is a different request.
            const outgoing = await submitOutgoing(hub, requester, {channel_id: channel.id, display_name: name, callback_url: CALLBACK});
            expect(outgoing).toEqual({message: SUBMITTED});
            expect(await pendingIds(hub, requester.id)).toHaveLength(2);

            // Once decided, the name can be asked for again.
            await decide(hub, ADMIN, 'deny', requestId);
            expect(await submitIncoming(hub, requester, {channel_id: channel.id, display_name: name})).toEqual({message: SUBMITTED});
        });
    });

    test('turned off: every submit route refuses, Approve is refused and Deny still works', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, channel, card, requestId} = await submitPending(hub);
            const disabled = 'Webhook requests are currently disabled by an administrator.';

            await hub.withPluginConfig({allowwebhookrequests: false}, async () => {
                const name = uid('Off ');
                expect(await submitIncoming(hub, requester, {channel_id: channel.id, display_name: name})).toEqual({error: disabled});
                expect(await submitOutgoing(hub, requester, {channel_id: channel.id, display_name: name, callback_url: CALLBACK})).toEqual({error: disabled});
                expect(await hub.slash(requester.username, `webhook-incoming ~${channel.name} ${name}`)).toBe(disabled);
                expect(await hub.slash(requester.username, `webhook-outgoing ~${channel.name} ${name} --callback ${CALLBACK}`)).toBe(disabled);
                expect(await hub.slash(requester.username, 'webhook-incoming')).toBe(disabled);
                expect(await hub.slash(requester.username, 'webhook-outgoing')).toBe(disabled);
                const client = await hub.client(requester.username);
                const res = await client.submitInteractiveDialog({
                    url: `/plugins/${PLUGIN_ID}/api/v1/incoming_webhook_dialog`,
                    callback_id: 'incoming_webhook_request',
                    state: '',
                    user_id: requester.id,
                    channel_id: channel.id,
                    team_id: hub.teamId,
                    submission: {channel_id: channel.id, display_name: name, purpose: ''},
                    cancelled: false,
                });
                expect(res.error).toBe(disabled);
                expect(await pendingIds(hub, requester.id)).toEqual([requestId]);
                expect(await postsMentioning(hub, await hub.channelId(REQUEST_CHANNELS.webhook), name)).toEqual([]);

                expect(await decide(hub, ADMIN, 'approve', requestId)).toBe('Requests of this type are turned off. Deny this request instead.');
                await expectStillPending(hub, requester.id, requestId, card);
                await expectDeniedCleanly(hub, requester, requestId, card);
            });
        });
    });
});

test.describe('webhook requests: refused at approval', () => {
    test('the requester cannot approve their own request, even as a System Admin', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const requester = await hub.createUser({systemAdmin: true});
            const channel = await createChannel(hub, 'O', [requester]);
            const name = uid('Own ');
            const res = await submitIncoming(hub, requester, {channel_id: channel.id, display_name: name});
            expect(res.message).toBe('Your webhook request has been posted for two-step approval. A security approver must sign off first, then another System Admin fills the system step.');
            const card = await hub.findCardIn(REQUEST_CHANNELS.webhook, name);
            const requestId = cardRequestId(card);

            expect(await decide(hub, requester.username, 'approve', requestId)).toBe('⚠️ You created this request — you cannot approve it. Another security or system approver must review this ticket.');
            await expectStillPending(hub, requester.id, requestId, card);

            // A click also paints a reminder on the card for everyone; the buttons stay.
            await (await hub.client(requester.username)).doPostAction(card.id, 'approve');
            const admin = await hub.client(ADMIN);
            await expect.poll(async () => fieldOf(await admin.getPost(card.id), '🚫 Requester cannot approve:')).toContain(`**@${requester.username}** (who created this ticket) clicked **Approve**.`);
            await expectStillPending(hub, requester.id, requestId, card);

            // Another System Admin can still approve it.
            await admin.doPostAction(card.id, 'approve');
            await expect.poll(async () => (await hooksNamed(hub, name)).incoming.length).toBe(1);
        });
    });

    test('a user who is not an approver can neither approve nor deny', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, card, requestId} = await submitPending(hub);
            const outsider = await hub.createUser();

            expect(await decide(hub, outsider.username, 'approve', requestId)).toBe("⚠️ You don't have permission to approve this request.");
            expect(await decide(hub, outsider.username, 'deny', requestId)).toBe("You don't have permission to deny this request.");
            await expectStillPending(hub, requester.id, requestId, card);

            // A Team Admin who can see the card fares no better: webhooks need the two-step pools.
            const admin = await hub.client(ADMIN);
            await admin.addToChannel(hub.userIds.tadmin, await hub.channelId(REQUEST_CHANNELS.webhook));
            const tadmin = await hub.client('tadmin');
            await tadmin.doPostAction(card.id, 'approve');
            await tadmin.doPostAction(card.id, 'deny');
            await expectStillPending(hub, requester.id, requestId, card);
            await expectDeniedCleanly(hub, requester, requestId, card);
        });
    });

    test('outgoing: the channel became private; an incoming hook for it can still be approved', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, channel, card, requestId} = await submitPending(hub, 'O', 'outgoing');
            const admin = await hub.client(ADMIN);
            await admin.updateChannelPrivacy(channel.id, 'P');

            expect(await decide(hub, ADMIN, 'approve', requestId)).toBe(`Outgoing webhooks need a public channel, and ~${channel.name} is no longer public. Deny this request instead.`);
            await expectStillPending(hub, requester.id, requestId, card);

            // Incoming hooks may post into private channels.
            const inName = uid('Still Fine ');
            expect(await submitIncoming(hub, requester, {channel_id: channel.id, display_name: inName})).toEqual({message: SUBMITTED});
            const inId = cardRequestId(await hub.findCardIn(REQUEST_CHANNELS.webhook, inName));
            expect(await decide(hub, ADMIN, 'approve', inId)).toContain(`incoming webhook **${inName}** created.`);

            await expectDeniedCleanly(hub, requester, requestId, card);
        });
    });

    test('the channel was archived', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, channel, card, requestId} = await submitPending(hub);
            const admin = await hub.client(ADMIN);
            await admin.deleteChannel(channel.id);

            expect(await decide(hub, ADMIN, 'approve', requestId)).toBe(`~${channel.name} has been archived. Deny this request instead.`);
            await expectStillPending(hub, requester.id, requestId, card);

            // A click is refused the same way and leaves the card as it was.
            await admin.doPostAction(card.id, 'approve');
            await expectStillPending(hub, requester.id, requestId, card);
            await expectDeniedCleanly(hub, requester, requestId, card);
        });
    });

    test('the channel was deleted', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, channel, card, requestId} = await submitPending(hub);
            await hub.mm.mmctl('channel', 'delete', `${TEAM}:${channel.name}`, '--confirm');

            expect(await decide(hub, ADMIN, 'approve', requestId)).toBe('A channel in this request no longer exists. Deny this request instead.');
            await expectStillPending(hub, requester.id, requestId, card);
            await expectDeniedCleanly(hub, requester, requestId, card);
        });
    });

    test('the requester left the channel', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, channel, card, requestId} = await submitPending(hub);
            await (await hub.client(ADMIN)).removeFromChannel(requester.id, channel.id);

            expect(await decide(hub, ADMIN, 'approve', requestId)).toBe('The requester is no longer a member of the channel. Deny this request instead.');
            await expectStillPending(hub, requester.id, requestId, card);
            await expectDeniedCleanly(hub, requester, requestId, card);
        });
    });

    test('the requester was deactivated', async ({hub}) => {
        await hub.withPluginConfig(SYSTEM_ADMIN_MODE, async () => {
            const {requester, card, requestId} = await submitPending(hub);
            await (await hub.client(ADMIN)).updateUserActive(requester.id, false);

            expect(await decide(hub, ADMIN, 'approve', requestId)).toBe("The requester's account is no longer active. Deny this request instead.");
            await expectStillPending(hub, requester.id, requestId, card);
            await expectDeniedCleanly(hub, requester, requestId, card);
        });
    });
});
