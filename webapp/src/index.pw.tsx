import type {Page} from '@playwright/test';
import React from 'react';

import {IndexHarness} from './IndexHarness';
import type {Recorded, RecordedMenu} from './IndexHarness';
import manifest from './manifest';

import {expect, test} from '../playwright/ct-coverage';
import {API, ALL_ON} from '../playwright/ct-helpers';

const PLUGIN_ID = manifest.id;

async function routeConfig(page: Page, body: unknown = ALL_ON) {
    await page.route(`${API}/config`, (route) => route.fulfill({contentType: 'application/json', body: JSON.stringify(body)}));
}

// Markup for two injectors: an open Members panel, and a team rail with no
// native Create Team button.
const injectorMarkup = `
    <div id="rhsContainer"><p>Members</p><button data-testid="addMembersButton">Add</button></div>
    <div id="teamSidebarWrapper"></div>
`;

async function recorded(page: Page): Promise<Recorded> {
    const pre = page.getByTestId('recorded');
    await expect(pre).not.toBeEmpty();
    return JSON.parse(await pre.textContent() ?? '{}');
}

const menu = (r: Recorded, text: string): RecordedMenu | undefined => r.menus.find((m) => m.text === text);
const action = (name: string) => `${PLUGIN_ID}_${name}`;

const realChannelsOnly = {O: true, P: true, D: false, G: false, unknown: false};

test.describe('plugin entry point', () => {
    test.beforeEach(async ({page}) => {
        await routeConfig(page);
    });

    test('registers the plugin and its UI', async ({mount, page}) => {
        await mount(<IndexHarness/>);
        const r = await recorded(page);
        expect(r.registeredId).toBe(PLUGIN_ID);
        expect(r.called[0]).toBe('registerReducer');
        expect(r.rootComponents).toEqual([
            'RequestHubModal',
            'RequestChannelModal',
            'RequestChannelAdminModal',
            'RequestTeamModal',
            'RequestTeamAdminModal',
            'RequestTeamMemberModal',
            'RequestTeamGuestModal',
            'RequestBotModal',
            'RequestIncomingWebhookModal',
            'RequestOutgoingWebhookModal',
        ]);
        expect(r.called.filter((c) => c === 'registerChannelHeaderButtonAction')).toHaveLength(1);
        expect(r.menus.map((m) => m.text)).toEqual([
            'Request a Channel',
            'Request Channel Admin',
            'Request a Team',
            'Request Team Admin',
            'Request Team Members',
            'Request Guest Invite',
            'Request Incoming Webhook',
            'Request Outgoing Webhook',
            'Request a Bot',
        ]);
        expect(r.adminSettings).toEqual(['ApprovalTeam', 'ChannelNamePrefixes']);
        expect(r.errors).toEqual([]);
    });

    test('the header button opens the Request Hub', async ({mount, page}) => {
        await mount(<IndexHarness/>);
        const r = await recorded(page);
        expect(r.headerButtonText).toBe('Request Hub');
        expect(r.headerDispatch).toEqual({type: action('open_hub_modal')});
    });

    test('each menu item opens its request modal', async ({mount, page}) => {
        await mount(<IndexHarness/>);
        const r = await recorded(page);
        expect(menu(r, 'Request a Channel')?.dispatch).toEqual({type: action('open_request_modal')});
        expect(menu(r, 'Request Channel Admin')?.dispatch).toEqual({type: action('open_admin_modal'), channelId: 'chan1'});
        expect(menu(r, 'Request a Team')?.dispatch).toEqual({type: action('open_team_creation_modal')});
        expect(menu(r, 'Request Team Admin')?.dispatch).toEqual({type: action('open_team_admin_request_modal')});
        expect(menu(r, 'Request Team Members')?.dispatch).toEqual({type: action('open_team_member_request_modal')});
        expect(menu(r, 'Request Guest Invite')?.dispatch).toEqual({type: action('open_team_guest_request_modal')});
        expect(menu(r, 'Request Incoming Webhook')?.dispatch).toEqual({type: action('open_incoming_webhook_modal')});
        expect(menu(r, 'Request Outgoing Webhook')?.dispatch).toEqual({type: action('open_outgoing_webhook_modal')});
        expect(menu(r, 'Request a Bot')?.dispatch).toEqual({type: action('open_bot_request_modal')});
    });

    test('channel-scoped menu items show only in public and private channels', async ({mount, page}) => {
        await mount(<IndexHarness/>);
        const r = await recorded(page);
        for (const text of ['Request Channel Admin', 'Request Guest Invite', 'Request Incoming Webhook']) {
            expect(menu(r, text)?.shouldRender, text).toEqual(realChannelsOnly);
        }
        expect(menu(r, 'Request Outgoing Webhook')?.shouldRender).toEqual({O: true, P: false, D: false, G: false, unknown: false});
        expect(menu(r, 'Request a Channel')?.shouldRender).toBeUndefined();
        expect(menu(r, 'Request a Bot')?.shouldRender).toBeUndefined();
    });

    test('Request Team Admin shows for a user who is not a Team Admin', async ({mount, page}) => {
        await mount(<IndexHarness/>);
        const r = await recorded(page);
        expect(menu(r, 'Request Team Admin')?.shouldRender).toEqual({O: true, P: true, D: true, G: true, unknown: true});
    });

    test('menu items for turned-off request types do nothing', async ({mount, page}) => {
        await page.unrouteAll();
        await routeConfig(page, {channel_admin: true});
        await mount(<IndexHarness/>);
        const r = await recorded(page);
        for (const m of r.menus) {
            if (m.text === 'Request Channel Admin') {
                expect(m.dispatch).not.toBeNull();
            } else {
                expect(m.dispatch, m.text).toBeNull();
            }
        }
        expect(r.infos).toContain('[channel-requests] Channel requests are disabled by an administrator.');
        expect(r.infos).toContain('[request-hub] Bot requests are disabled by an administrator.');
    });

    test('a failing registration does not stop the others', async ({mount, page}) => {
        await mount(<IndexHarness throwOn='registerChannelHeaderButtonAction'/>);
        const r = await recorded(page);
        expect(r.errors).toEqual(['[channel-requests] channel-header button failed to initialize']);
        expect(r.menus).toHaveLength(9);
        expect(r.adminSettings).toHaveLength(2);
    });

    test('skips the admin console settings on servers without them', async ({mount, page}) => {
        await mount(<IndexHarness withoutAdminConsole={true}/>);
        const r = await recorded(page);
        expect(r.adminSettings).toEqual([]);
        expect(r.menus).toHaveLength(9);
    });

    test('uninitialize removes what the DOM injectors added', async ({mount, page}) => {
        await mount(<IndexHarness
            markup={injectorMarkup}
            uninitialize={true}
                    />);
        const r = await recorded(page);
        expect(r.injected?.before).toEqual({membersPanelButton: 1, teamRailButton: 1});
        expect(r.injected?.after).toEqual({membersPanelButton: 0, teamRailButton: 0});
        expect(r.errors).toEqual([]);
    });

    test('a failing disposer does not stop the others', async ({mount, page}) => {
        await mount(<IndexHarness
            markup={injectorMarkup}
            uninitialize={true}
            failFirstDisposer={true}
                    />);
        const r = await recorded(page);
        expect(r.injected?.before).toEqual({membersPanelButton: 1, teamRailButton: 1});

        // The Members panel disposer threw before removing its button; the
        // team rail disposer still ran.
        expect(r.injected?.after).toEqual({membersPanelButton: 1, teamRailButton: 0});
        expect(r.errors).toEqual(['[channel-requests] disposer failed during uninitialize']);
    });
});
