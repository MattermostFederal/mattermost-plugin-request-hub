import {DECLARED, botName, createAttribute, createGroup, setAttribute, submitBot, userNamed} from '../helpers/bots';
import {expect, test} from '../helpers/fixtures';
import {ADMIN, PLUGIN_ID, REQUEST_CHANNELS, cardRequestId, uid, type RequestHub} from '../helpers/requesthub';
import {login, openChannel} from '../helpers/ui';

type AuditEvent = {at: number; event: string; actor_id?: string; actor_name?: string; outcome: string; note?: string; detail?: Record<string, unknown>};
type RequestHistory = {request_id: string; kind: string; requester_id: string; status: string; request?: Record<string, unknown>; events: AuditEvent[]};

const EXPORT_ROUTE = '/api/v1/audit/export';

const history = (hub: RequestHub, requestId: string) => hub.kv<RequestHistory>(`history_${requestId}`);

async function eventsOf(hub: RequestHub, requestId: string): Promise<string[]> {
    return ((await history(hub, requestId))?.events ?? []).map((e) => e.event);
}

type ForensicPost = {type?: string; message: string};

// userPosts is the messages of a forensic export's posts that aren't system posts.
const userPosts = (fx: {posts: ForensicPost[]}) => fx.posts.filter((post) => !post.type).map((post) => post.message);

// Fetches the export as username, returning the status and body text.
async function exportAs(hub: RequestHub, username: string, query: string) {
    const c = await hub.client(username);
    const res = await fetch(`${hub.mm.url()}/plugins/${PLUGIN_ID}${EXPORT_ROUTE}${query}`, c.getOptions({method: 'get'}));
    return {status: res.status, type: res.headers.get('content-type') ?? '', body: await res.text()};
}

test.describe('audit trail', () => {
    test('a two-step bot request records every step, with refusals, and exports', async ({hub}) => {
        const requester = await hub.createUser();
        const security = await hub.createUser();
        const outsider = await hub.createUser();
        const username = botName();
        const securityAttr = uid('sec');
        const securityField = await createAttribute(hub, securityAttr);
        const admin = await hub.client(ADMIN);
        try {
            await hub.withPluginConfig({twostepapprovermode: 'attribute', securityapproverattribute: securityAttr, auditchanneldetail: 'all', allowdirectauditexport: true}, async () => {
                await setAttribute(hub, security.id, securityField, 'yes');
                const res = await submitBot(hub, requester.username, {username});
                expect(res.error, JSON.stringify(res)).toBeUndefined();
                const card = await hub.findCardIn(REQUEST_CHANNELS.bot, `@${username}`);
                const requestId = cardRequestId(card);

                await expect.poll(() => eventsOf(hub, requestId)).toEqual(['submitted']);
                const submitted = (await history(hub, requestId))!;
                expect(submitted.kind).toBe('bot');
                expect(submitted.requester_id).toBe(requester.id);
                expect(submitted.status).toBe('pending');
                expect(submitted.request?.username).toBe(username);
                await hub.findAudit(`@${requester.username} submitted a bot account request`);

                // Someone outside the security pool is refused, and that is recorded.
                await hub.slash(outsider.username, `approve ${requestId}`);
                await expect.poll(() => eventsOf(hub, requestId)).toEqual(['submitted', 'decision_refused']);
                const refused = (await history(hub, requestId))!.events[1];
                expect(refused.actor_id).toBe(outsider.id);
                expect(refused.outcome).toBe('fail');
                expect(refused.detail?.action).toBe('approve');

                expect(await hub.slash(security.username, `approve ${requestId}`)).toContain('Approved the security step.');
                expect(await hub.slash(ADMIN, `approve ${requestId}`)).toContain('Fully approved (security + system)');

                await expect.poll(() => eventsOf(hub, requestId)).toEqual(['submitted', 'decision_refused', 'step_approved', 'approved']);
                const done = (await history(hub, requestId))!;
                expect(done.status).toBe('approved');
                expect(done.events[2].actor_id).toBe(security.id);
                expect(done.events[2].detail?.step).toBe('security');
                expect(done.events[3].actor_name).toBe(ADMIN);
                // The request record is gone; the history stays.
                expect(await hub.kv(`bot_request_${requestId}`)).toBeUndefined();
                await hub.findAudit(`gave final approval; bot @${username} created`);

                // A message in the ticket thread is recorded with its text.
                const note = `please hurry ${uid('m')}`;
                const requesterClient = await hub.client(requester.username);
                const dmRoot = await hub.findDM(requester.username, username);
                await requesterClient.createPost({channel_id: dmRoot.channel_id, root_id: dmRoot.root_id || dmRoot.id, message: note} as Parameters<typeof requesterClient.createPost>[0]);
                await expect.poll(async () => (await history(hub, requestId))?.events.find((e) => e.event === 'message_relayed')?.detail?.message).toBe(note);

                // The direct export (on for this test): System Admins only, JSON and CSV.
                expect((await exportAs(hub, requester.username, '')).status).toBe(403);
                const json = await exportAs(hub, ADMIN, '?kind=bot');
                expect(json.status).toBe(200);
                const all = JSON.parse(json.body) as RequestHistory[];
                expect(all.find((h) => h.request_id === requestId)?.status).toBe('approved');
                const csv = await exportAs(hub, ADMIN, `?format=csv&requester=${requester.id}`);
                expect(csv.status).toBe(200);
                expect(csv.type).toContain('text/csv');
                const rows = csv.body.trim().split('\n');
                expect(rows[0]).toBe('request_id,kind,requester_id,status,time_utc,event,outcome,actor_id,actor_name,note,message,details');
                expect(rows.filter((r) => r.startsWith(requestId)).map((r) => r.split(',')[5])).toEqual(
                    ['submitted', 'decision_refused', 'step_approved', 'approved', 'message_relayed']);

                // The slash command shows the timeline to System Admins only.
                expect(await hub.slash(requester.username, `audit ${requestId}`)).toContain('Only System Admins');
                const timeline = await hub.slash(ADMIN, `audit ${requestId}`);
                expect(timeline).toContain(`Audit history for bot account request \`${requestId}\``);
                expect(timeline).toContain('Status: **approved**');
                expect(timeline).toContain('`step_approved`');
            });
        } finally {
            await admin.deleteCustomProfileAttributeField(securityField);
        }
    });

    test('a refused submission leaves no history', async ({hub}) => {
        const requester = await hub.createUser();
        const before = await hub.kvKeys('history_');
        const res = await submitBot(hub, requester.username, {username: 'NO'});
        expect(res.error).toBeTruthy();
        expect(await hub.kvKeys('history_')).toEqual(before);
    });

    test('decisions-only detail keeps submissions out of the audit channel', async ({hub}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig({twostepapprovermode: 'system_admin', auditchanneldetail: 'decisions'}, async () => {
            const res = await submitBot(hub, requester.username, {username});
            expect(res.error, JSON.stringify(res)).toBeUndefined();
            const requestId = cardRequestId(await hub.findCardIn(REQUEST_CHANNELS.bot, `@${username}`));
            await expect.poll(() => eventsOf(hub, requestId)).toEqual(['submitted']);
            await hub.slash(ADMIN, `deny ${requestId}`);
            await hub.findAudit(`denied a bot request for @${username}`);
            await expect.poll(() => eventsOf(hub, requestId)).toEqual(['submitted', 'denied']);

            // The deny card is there, so anything posted before it would be too.
            const admin = await hub.client(ADMIN);
            const posts = await admin.getPosts(await hub.channelId(REQUEST_CHANNELS.audit), 0, 200);
            const texts = Object.values(posts.posts).map((post) => JSON.stringify(post.props ?? {}));
            expect(texts.filter((t) => t.includes(`@${requester.username} submitted`))).toEqual([]);
        });
    });

    test('a security approver asks for the export; a System Admin approves and the file arrives', async ({hub}) => {
        const security = await hub.createUser();
        const outsider = await hub.createUser();
        const group = await createGroup(hub, [security]);
        await hub.withPluginConfig({twostepapprovermode: 'usergroup', securityapprovergroup: group}, async () => {
            // Only the security approver may ask; an outsider is refused and nothing is stored.
            expect(await hub.pluginFetch(security.username, '/api/v1/audit/access')).toEqual({can_view: false, can_request: true, direct_export: false});
            expect(await hub.pluginFetch(outsider.username, '/api/v1/audit/access')).toEqual({can_view: false, can_request: false, direct_export: false});
            expect((await hub.pluginFetch(outsider.username, '/api/v1/submit_audit_export_request', {format: 'csv', reason: 'why'})).error).toContain('Only security approvers');
            expect((await exportAs(hub, security.username, '')).status).toBe(403);
            expect((await hub.pluginFetch(security.username, '/api/v1/submit_audit_export_request', {format: 'csv'})).error).toContain('Give a reason for the export');

            // Direct export is off by default: System Admins ask too, and the download is refused.
            expect(await hub.pluginFetch(ADMIN, '/api/v1/audit/access')).toEqual({can_view: true, can_request: true, direct_export: false});
            const direct = await exportAs(hub, ADMIN, '');
            expect(direct.status).toBe(403);
            expect(direct.body).toContain('Direct export is off. Request an export instead.');
            expect(await hub.slash(ADMIN, 'audit recent')).toContain('Direct downloads are off');

            const res = await hub.pluginFetch(security.username, '/api/v1/submit_audit_export_request', {format: 'csv', kind: 'bot', reason: uid('why')});
            expect(res.message, JSON.stringify(res)).toContain('sent to the System Admins');
            const card = await hub.findAudit('Audit export request');
            const requestId = cardRequestId(card);
            expect(JSON.stringify(card.props)).toContain('Audit export request');
            expect(JSON.stringify(card.props)).toContain('Request histories');

            // The requester can't approve it, a System Admin can.
            expect(await hub.slash(security.username, `approve ${requestId}`)).toBe('Only System Admins can approve or deny audit export requests.');
            expect(await hub.slash(ADMIN, `approve ${requestId}`)).toMatch(/^✅ Approved by @sysadmin for @.+\. Audit export \(CSV, \d+ requests\) sent\.$/);

            const delivered = await hub.findDM(security.username, 'Audit export: Request histories');
            expect(delivered.file_ids).toHaveLength(1);
            const c = await hub.client(security.username);
            const file = await fetch(`${hub.mm.url()}/api/v4/files/${delivered.file_ids![0]}`, c.getOptions({method: 'get'}));
            expect(file.status).toBe(200);
            const csv = await file.text();
            expect(csv.split('\n')[0]).toBe('request_id,kind,requester_id,status,time_utc,event,outcome,actor_id,actor_name,note,message,details');
            for (const row of csv.trim().split('\n').slice(1)) {
                expect(row.split(',')[1]).toBe('bot');
            }

            await expect.poll(() => eventsOf(hub, requestId)).toEqual(['submitted', 'decision_refused', 'approved']);
            expect((await history(hub, requestId))?.status).toBe('approved');
        });
    });

    test('the bot inventory lists a bot Request Hub created, downloaded directly and delivered on request', async ({hub}) => {
        const requester = await hub.createUser();
        const security = await hub.createUser();
        const username = botName();
        const env = 'cloud_or_k8s';

        await hub.withPluginConfig({twostepapprovermode: 'system_admin'}, async () => {
            const res = await submitBot(hub, requester.username, {username, hosting_env: env});
            expect(res.error, JSON.stringify(res)).toBeUndefined();
            const card = await hub.findCardIn(REQUEST_CHANNELS.bot, `@${username}`);
            await (await hub.client(ADMIN)).doPostAction(card.id, 'approve');
        });
        await expect.poll(async () => (await userNamed(hub, username))?.id).toBeTruthy();
        const bot = (await userNamed(hub, username))!;
        await expect.poll(() => hub.kv(`bot_index_${bot.id}`)).toBeTruthy();

        // A System Admin downloads it as JSON while direct export is on.
        await hub.withPluginConfig({allowdirectauditexport: true}, async () => {
            const json = await exportAs(hub, ADMIN, `?type=bot_inventory&env=${env}&requester=${requester.id}`);
            expect(json.status, json.body).toBe(200);
            const rows = JSON.parse(json.body);
            expect(rows).toHaveLength(1);
            expect(rows[0]).toMatchObject({
                bot_id: bot.id, bot_username: username, bot_status: 'active', bot_active: true,
                hosting_env: env, purpose: DECLARED.purpose, privilege_level: 'user_equivalent',
                data_ingestion: 'member permissions in channels the bot is added to (Mattermost user role)',
                home_team_id: hub.teamId, home_channel_name: 'Town Square',
                requester_id: requester.id, requester_username: requester.username, approved_by_username: ADMIN,
                compliance_accepted: true, rh_managed: true,
            });
            expect((await exportAs(hub, ADMIN, '?type=bot_inventory&kind=bot')).status).toBe(400);
        });

        // A security approver asks for it as a PDF, and a System Admin approves.
        const group = await createGroup(hub, [security]);
        await hub.withPluginConfig({twostepapprovermode: 'usergroup', securityapprovergroup: group, auditchanneldetail: 'decisions'}, async () => {
            const reason = uid('fleet');
            const res = await hub.pluginFetch(security.username, '/api/v1/submit_audit_export_request', {
                type: 'bot_inventory', format: 'pdf', env, requester: requester.username, reason,
            });
            expect(res.message, JSON.stringify(res)).toContain('sent to the System Admins');
            const card = await hub.findAudit(reason);
            expect(JSON.stringify(card.props)).toContain('Bot inventory');
            expect(await hub.slash(ADMIN, `approve ${cardRequestId(card)}`)).toBe(`✅ Approved by @${ADMIN} for @${security.username}. Audit export (PDF, 1 bot) sent.`);

            const delivered = await hub.findDM(security.username, 'Audit export: Bot inventory');
            expect(delivered.file_ids).toHaveLength(1);
            const c = await hub.client(security.username);
            const file = await fetch(`${hub.mm.url()}/api/v4/files/${delivered.file_ids![0]}`, c.getOptions({method: 'get'}));
            expect(file.status).toBe(200);
            const pdf = Buffer.from(await file.arrayBuffer()).toString('latin1');
            expect(pdf.startsWith('%PDF-1.4')).toBe(true);
            expect(pdf.trimEnd().endsWith('%%EOF')).toBe(true);
            expect(pdf).toContain(`(@${username}`);
            expect(pdf).toContain(`(${bot.id}) Tj`);
        });
    });

    test('the bot forensic export traces what one bot posted, downloaded directly and delivered on request', async ({hub}) => {
        const requester = await hub.createUser();
        const security = await hub.createUser();
        const username = botName();

        await hub.withPluginConfig({twostepapprovermode: 'system_admin'}, async () => {
            const res = await submitBot(hub, requester.username, {username});
            expect(res.error, JSON.stringify(res)).toBeUndefined();
            const card = await hub.findCardIn(REQUEST_CHANNELS.bot, `@${username}`);
            await (await hub.client(ADMIN)).doPostAction(card.id, 'approve');
        });
        await expect.poll(async () => (await userNamed(hub, username))?.id).toBeTruthy();
        const bot = (await userNamed(hub, username))!;
        await expect.poll(() => hub.kv(`bot_index_${bot.id}`)).toBeTruthy();

        // The bot posts in its home channel with a token of its own.
        const admin = await hub.client(ADMIN);
        const {token} = await admin.createUserAccessToken(bot.id, 'e2e forensics');
        const townSquare = await hub.channelId('town-square');
        const message = uid('forensic-post');
        await expect.poll(async () => {
            const res = await fetch(`${hub.mm.url()}/api/v4/posts`, {
                method: 'POST',
                headers: {Authorization: `Bearer ${token}`, 'Content-Type': 'application/json'},
                body: JSON.stringify({channel_id: townSquare, message}),
            });
            return res.status;
        }, {message: 'the bot joins its home channel on approval'}).toBe(201);
        const since = Date.now() - (24 * 60 * 60 * 1000);
        const until = Date.now() + (60 * 60 * 1000);

        // A System Admin downloads it directly while direct export is on.
        await hub.withPluginConfig({allowdirectauditexport: true}, async () => {
            const json = await exportAs(hub, ADMIN, `?type=bot_forensic&bot=${bot.id}&since=${since}&until=${until}`);
            expect(json.status, json.body).toBe(200);
            const fx = JSON.parse(json.body);
            expect(fx).toMatchObject({bot_id: bot.id, rh_managed: true, bot_status: 'active', truncated: false});
            expect(fx.index.username).toBe(username);
            expect(fx.history.kind).toBe('bot');
            // Its own posts, plus the system posts for joining its home channel.
            expect(userPosts(fx)).toEqual([message]);
            expect(fx.posts.some((post: ForensicPost) => post.type === 'system_join_channel')).toBe(true);
            expect(fx.post_count).toBe(fx.posts.length);
            expect(fx.scanned_channels).toContainEqual(expect.objectContaining({id: townSquare, why: 'member'}));
            expect(fx.limitations.length).toBeGreaterThan(0);

            expect((await exportAs(hub, ADMIN, `?type=bot_forensic&bot=${bot.id}`)).status, 'dates are required').toBe(400);
            expect((await exportAs(hub, ADMIN, `?type=bot_forensic&bot=${bot.id}&since=1&until=${until}`)).status, 'at most 90 days').toBe(400);
        });

        // A security approver asks for it, and a System Admin approves.
        const group = await createGroup(hub, [security]);
        await hub.withPluginConfig({twostepapprovermode: 'usergroup', securityapprovergroup: group, auditchanneldetail: 'decisions'}, async () => {
            const reason = uid('incident');
            const refused = await hub.pluginFetch(security.username, '/api/v1/submit_audit_export_request', {
                type: 'bot_forensic', format: 'json', bot: requester.username, since, until, reason,
            });
            expect(refused.error).toBe(`@${requester.username} is not a bot`);
            const res = await hub.pluginFetch(security.username, '/api/v1/submit_audit_export_request', {
                type: 'bot_forensic', format: 'json', bot: username, since, until, reason,
            });
            expect(res.message, JSON.stringify(res)).toContain('sent to the System Admins');
            const card = await hub.findAudit(reason);
            expect(JSON.stringify(card.props)).toContain('Bot forensics');
            expect(await hub.slash(ADMIN, `approve ${cardRequestId(card)}`)).toMatch(new RegExp(`^✅ Approved by @${ADMIN} for @${security.username}\\. Audit export \\(JSON, \\d+ posts?\\) sent\\.$`));

            const delivered = await hub.findDM(security.username, 'Audit export: Bot forensics');
            expect(delivered.file_ids).toHaveLength(1);
            const c = await hub.client(security.username);
            const file = await fetch(`${hub.mm.url()}/api/v4/files/${delivered.file_ids![0]}`, c.getOptions({method: 'get'}));
            expect(file.status).toBe(200);
            const fx = JSON.parse(await file.text());
            expect(fx.bot_id).toBe(bot.id);
            expect(userPosts(fx)).toEqual([message]);
        });
    });

    test('the Request Hub shows System Admins the audit history and its downloads', async ({hub, page}) => {
        const requester = await hub.createUser();
        const username = botName();
        await hub.withPluginConfig({twostepapprovermode: 'system_admin', allowdirectauditexport: true}, async () => {
            const res = await submitBot(hub, requester.username, {username});
            expect(res.error, JSON.stringify(res)).toBeUndefined();
            const requestId = cardRequestId(await hub.findCardIn(REQUEST_CHANNELS.bot, `@${username}`));

            await login(page, hub, ADMIN);
            await openChannel(page, hub, 'town-square');
            // The Request Hub button: the plug icon the plugin adds to the app bar.
            await page.locator('i.fa-plug').first().click();
            const hubDialog = page.getByRole('dialog').filter({hasText: 'Mattermost Request Hub'});
            await hubDialog.getByRole('button', {name: /Audit history/}).click();

            const audit = page.getByRole('dialog').filter({hasText: 'Audit history'});
            await expect(audit.getByRole('link', {name: 'Download CSV'})).toHaveAttribute('href', `/plugins/${PLUGIN_ID}/api/v1/audit/export?format=csv`);
            await audit.getByLabel('Search').fill(username);
            await expect(audit.getByText('1 request', {exact: true})).toBeVisible();
            await audit.getByRole('row').nth(1).getByRole('button').click();
            await expect(audit.getByRole('list', {name: 'Events'}).getByRole('listitem').first()).toContainText('Submitted');
            await expect(audit.getByRole('link', {name: 'Download JSON'})).toHaveAttribute('href', `/plugins/${PLUGIN_ID}/api/v1/audit/export?format=json&request=${requestId}`);
        });
    });
});
