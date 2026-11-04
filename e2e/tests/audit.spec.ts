import {botName, createAttribute, setAttribute, submitBot} from '../helpers/bots';
import {expect, test} from '../helpers/fixtures';
import {ADMIN, PLUGIN_ID, REQUEST_CHANNELS, cardRequestId, uid, type RequestHub} from '../helpers/requesthub';

type AuditEvent = {at: number; event: string; actor_id?: string; actor_name?: string; outcome: string; note?: string; detail?: Record<string, unknown>};
type RequestHistory = {request_id: string; kind: string; requester_id: string; status: string; request?: Record<string, unknown>; events: AuditEvent[]};

const EXPORT_ROUTE = '/api/v1/audit/export';

const history = (hub: RequestHub, requestId: string) => hub.kv<RequestHistory>(`history_${requestId}`);

async function eventsOf(hub: RequestHub, requestId: string): Promise<string[]> {
    return ((await history(hub, requestId))?.events ?? []).map((e) => e.event);
}

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
        const systemAttr = uid('sys');
        const securityField = await createAttribute(hub, securityAttr);
        const systemField = await createAttribute(hub, systemAttr);
        const admin = await hub.client(ADMIN);
        try {
            await hub.withPluginConfig({twostepapprovermode: 'attribute', securityapproverattribute: securityAttr, systemapproverattribute: systemAttr, auditchanneldetail: 'all'}, async () => {
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

                // Someone outside both pools is refused, and that is recorded.
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

                // The export: System Admins only, JSON and CSV.
                expect((await exportAs(hub, requester.username, '')).status).toBe(403);
                const json = await exportAs(hub, ADMIN, '?kind=bot');
                expect(json.status).toBe(200);
                const all = JSON.parse(json.body) as RequestHistory[];
                expect(all.find((h) => h.request_id === requestId)?.status).toBe('approved');
                const csv = await exportAs(hub, ADMIN, `?format=csv&requester=${requester.id}`);
                expect(csv.status).toBe(200);
                expect(csv.type).toContain('text/csv');
                const rows = csv.body.trim().split('\n');
                expect(rows[0]).toBe('request_id,kind,requester_id,status,time_utc,event,outcome,actor_id,actor_name,note');
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
            await admin.deleteCustomProfileAttributeField(systemField);
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
});
