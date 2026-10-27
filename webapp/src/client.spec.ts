import {expect, test} from '@playwright/test';
import manifest from 'manifest';

import {
    fetchEnabledRequestTypes,
    fetchPrefixes,
    fetchSidebarCategories,
    submitAdminRequest,
    submitBotRequest,
    submitChannelRequest,
    submitIncomingWebhookRequest,
    submitOutgoingWebhookRequest,
    submitTeamAdminRequest,
    submitTeamCreationRequest,
    submitTeamGuestRequest,
    submitTeamMemberRequest,
} from './client';
import type {ChannelRequestPayload} from './client';

type Call = {url: string; init?: RequestInit};

const originalFetch = globalThis.fetch;
const originalDocument = (globalThis as {document?: unknown}).document;

let calls: Call[];

// mockFetch replaces fetch with one that records each call and answers with
// the given status and body. A body of undefined makes json() throw, like an
// empty or non-JSON response.
function mockFetch(status: number, body?: unknown) {
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
        calls.push({url, init});
        return {
            ok: status >= 200 && status < 300,
            status,
            json: async () => {
                if (body === undefined) {
                    throw new SyntaxError('Unexpected end of JSON input');
                }
                return body;
            },
        };
    }) as unknown as typeof globalThis.fetch;
}

function setCookie(cookie: string) {
    (globalThis as {document?: unknown}).document = {cookie};
}

function headersOf(call: Call): Record<string, string> {
    return call.init?.headers as Record<string, string>;
}

const payload: ChannelRequestPayload = {
    team_id: 'team1',
    display_name: 'Ops',
    name: 'ops',
    prefix: 'team-',
    purpose: '',
    channel_type: 'O',
    members: ['alice'],
    admin_members: [],
};

test.beforeEach(() => {
    calls = [];
});

test.afterEach(() => {
    globalThis.fetch = originalFetch;
    if (originalDocument === undefined) {
        delete (globalThis as {document?: unknown}).document;
    } else {
        (globalThis as {document?: unknown}).document = originalDocument;
    }
});

// The CSRF token is read from the MMCSRF cookie and sent on every POST.
test.describe('CSRF token header', () => {
    const tokenSent = async () => {
        mockFetch(200, {});
        await submitChannelRequest(payload);
        return headersOf(calls[0])['X-CSRF-Token'];
    };

    test('is empty without a document', async () => {
        expect(await tokenSent()).toBe('');
    });

    test('reads the MMCSRF cookie', async () => {
        setCookie('MMUSERID=u1; MMCSRF=token123; other=x');
        expect(await tokenSent()).toBe('token123');
    });

    test('is empty when the cookie is missing', async () => {
        setCookie('MMUSERID=u1');
        expect(await tokenSent()).toBe('');
    });
});

test.describe('fetchPrefixes', () => {
    test('returns the prefix list', async () => {
        const list = [{prefix: 'team-', description: 'Team', suffix_regex: ''}];
        mockFetch(200, list);
        expect(await fetchPrefixes()).toEqual(list);
        expect(calls[0].url).toBe(`/plugins/${manifest.id}/api/v1/prefixes`);
        expect(calls[0].init?.method).toBe('GET');
    });

    test('returns an empty list for a non-array body', async () => {
        mockFetch(200, {unexpected: true});
        expect(await fetchPrefixes()).toEqual([]);
    });

    test('throws on a non-OK status', async () => {
        mockFetch(500, {});
        await expect(fetchPrefixes()).rejects.toThrow('Failed to load prefixes (500)');
    });
});

([
    ['submitChannelRequest', () => submitChannelRequest(payload), 'create'],
    ['submitAdminRequest', () => submitAdminRequest({channel_id: 'chan1', nominees: ['alice']}), 'request_admin'],
    ['submitTeamCreationRequest', () => submitTeamCreationRequest({display_name: 'Team Two', type: 'O'}), 'submit_team_creation'],
    ['submitTeamAdminRequest', () => submitTeamAdminRequest({team_id: 'team1'}), 'submit_team_admin_request'],
    ['submitTeamMemberRequest', () => submitTeamMemberRequest({team_id: 'team1', nominees: ['alice']}), 'submit_team_member_request'],
    ['submitTeamGuestRequest', () => submitTeamGuestRequest({team_id: 'team1', email: 'guest@example.com', channel_ids: ['chan1']}), 'submit_team_guest_request'],
    ['submitBotRequest', () => submitBotRequest({username: 'helper-bot', display_name: 'Helper'}), 'submit_bot_request'],
    ['submitIncomingWebhookRequest', () => submitIncomingWebhookRequest({channel_id: 'chan1', display_name: 'Alerts'}), 'submit_incoming_webhook_request'],
    ['submitOutgoingWebhookRequest', () => submitOutgoingWebhookRequest({channel_id: 'chan1', display_name: 'Relay', callback_url: 'https://example.com/hook'}), 'submit_outgoing_webhook_request'],
] as const).forEach(([name, submit, path]) => {
    test.describe(name, () => {
        test('posts JSON with the CSRF token', async () => {
            setCookie('MMCSRF=token123');
            mockFetch(200, {message: 'ok'});
            expect(await submit()).toEqual({message: 'ok'});
            expect(typeof calls[0].init?.body).toBe('string');
            expect(calls[0].url).toBe(`/plugins/${manifest.id}/api/v1/${path}`);
            expect(calls[0].init?.method).toBe('POST');
            expect(headersOf(calls[0])['X-CSRF-Token']).toBe('token123');
            expect(headersOf(calls[0])['Content-Type']).toBe('application/json');
        });

        test('returns the server error message', async () => {
            mockFetch(400, {error: 'Channel name is already taken'});
            expect(await submit()).toEqual({error: 'Channel name is already taken'});
        });

        test('builds an error when a failed response has no error body', async () => {
            mockFetch(502);
            expect(await submit()).toEqual({error: 'Request failed (502). Please try again.'});
        });

        test('returns an empty result for a successful response with no body', async () => {
            mockFetch(200);
            expect(await submit()).toEqual({});
        });
    });
});

test.describe('fetchSidebarCategories', () => {
    test('returns the categories for the team', async () => {
        const list = [{id: 'cat1', display_name: 'Favorites', type: 'favorites'}];
        mockFetch(200, list);
        expect(await fetchSidebarCategories('team 1')).toEqual(list);
        expect(calls[0].url).toBe(`/plugins/${manifest.id}/api/v1/sidebar_categories?team_id=team%201`);
    });

    test('returns an empty list for a non-array body', async () => {
        mockFetch(200, {unexpected: true});
        expect(await fetchSidebarCategories('team1')).toEqual([]);
    });

    test('throws on a non-OK status', async () => {
        mockFetch(403, {});
        await expect(fetchSidebarCategories('team1')).rejects.toThrow('Failed to load sidebar categories (403)');
    });
});

test.describe('fetchEnabledRequestTypes', () => {
    const failOpen = {
        channel: true,
        channelAdmin: true,
        team: true,
        teamAdmin: true,
        teamMember: true,
        teamGuest: false,
        bot: true,
        webhook: true,
    };

    test('maps the config fields', async () => {
        mockFetch(200, {channel: true, channel_admin: false, team: true, team_admin: false, team_member: true, team_guest: true, bot: false, webhook: true});
        expect(await fetchEnabledRequestTypes()).toEqual({
            channel: true,
            channelAdmin: false,
            team: true,
            teamAdmin: false,
            teamMember: true,
            teamGuest: true,
            bot: false,
            webhook: true,
        });
        expect(calls[0].url).toBe(`/plugins/${manifest.id}/api/v1/config`);
        expect(calls[0].init?.method).toBe('GET');
    });

    test('treats missing fields as off', async () => {
        mockFetch(200, {});
        expect(Object.values(await fetchEnabledRequestTypes()).every((v) => v === false)).toBe(true);
    });

    test('falls back to the defaults on a non-OK status', async () => {
        mockFetch(500, {});
        expect(await fetchEnabledRequestTypes()).toEqual(failOpen);
    });

    test('falls back to the defaults when the request fails', async () => {
        globalThis.fetch = (async () => {
            throw new TypeError('Failed to fetch');
        }) as unknown as typeof globalThis.fetch;
        expect(await fetchEnabledRequestTypes()).toEqual(failOpen);
    });
});
