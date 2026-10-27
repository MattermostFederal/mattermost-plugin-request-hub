import type {Page} from '@playwright/test';
import React from 'react';

import manifest from './manifest';
import {RequestBotModal} from './RequestBotModal';
import {RequestChannelAdminModal} from './RequestChannelAdminModal';
import {RequestHubModal} from './RequestHubModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API, configLoaded, routeConfig} from '../playwright/ct-helpers';

const TITLE = 'Mattermost Request Hub';

const open = {hubModalOpen: true};

const ALL_OFF = {
    channel: false,
    channel_admin: false,
    team: false,
    team_admin: false,
    team_member: false,
    team_guest: false,
    bot: false,
    webhook: false,
};

const ALL_TILES = [
    'Request a Channel',
    'Request Channel Admin',
    'Request a Team',
    'Request Team Admin',
    'Request Team Members',
    'Request Guest Invite',
    'Request a Bot',
    'Request Incoming Webhook',
    'Request Outgoing Webhook',
];

// The tiles left outside a public or private channel.
const OUTSIDE_TEAM_CHANNEL = [
    'Request a Channel',
    'Request a Team',
    'Request Team Admin',
    'Request Team Members',
    'Request a Bot',
];

// tiles returns the titles of the request tiles, in order. A tile's
// accessible name is its title followed by its description.
async function tiles(page: Page): Promise<string[]> {
    const names = await page.getByRole('dialog').getByRole('button', {name: /^Request /}).evaluateAll(
        (els) => els.map((el) => (el.textContent ?? '').replace(/^[^A-Za-z]+/, '')),
    );
    return names.map((name) => ALL_TILES.filter((t) => name.startsWith(t)).sort((a, b) => b.length - a.length)[0] ?? name);
}

test.describe('RequestHubModal', () => {
    test('renders nothing while closed', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<StoreHarness><RequestHubModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('shows every enabled type in a team channel', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<StoreHarness pluginState={open}><RequestHubModal/></StoreHarness>);
        await configLoaded(page);
        await expect.poll(() => tiles(page)).toEqual(ALL_TILES);
    });

    test('hides the types an admin turned off', async ({mount, page}) => {
        await routeConfig(page, {bot: false, webhook: false, team_guest: false, channel: false});
        await mount(<StoreHarness pluginState={open}><RequestHubModal/></StoreHarness>);
        await configLoaded(page);
        await expect.poll(() => tiles(page)).toEqual([
            'Request Channel Admin',
            'Request a Team',
            'Request Team Admin',
            'Request Team Members',
        ]);
    });

    test('says so when every type is off', async ({mount, page}) => {
        await routeConfig(page, ALL_OFF);
        await mount(<StoreHarness pluginState={open}><RequestHubModal/></StoreHarness>);
        await expect(page.getByText('No request types are currently enabled.', {exact: false})).toBeVisible();
        expect(await tiles(page)).toEqual([]);
    });

    test('falls back to the defaults when the config cannot load', async ({mount, page}) => {
        // Open once with the bot type off, so the defaults are a visible change.
        await routeConfig(page, {bot: false});
        await mount(<StoreHarness pluginState={open}><RequestHubModal/></StoreHarness>);
        await configLoaded(page);
        await expect.poll(() => tiles(page)).not.toContain('Request a Bot');
        await page.getByRole('button', {name: 'Close'}).click();

        await page.unroute(`${API}/config`);
        const failed = page.waitForResponse(`${API}/config`);
        await page.route(`${API}/config`, (route) => route.fulfill({status: 500, body: ''}));
        await page.getByRole('button', {name: 'harness: open hub'}).click();
        await failed;
        await expect.poll(() => tiles(page)).toEqual(ALL_TILES.filter((t) => t !== 'Request Guest Invite'));
    });

    for (const [where, channels] of [
        ['a DM', {currentChannelId: 'dm1', channels: {dm1: {display_name: 'alice, bob', type: 'D'}}}],
        ['a group message', {currentChannelId: 'gm1', channels: {gm1: {display_name: 'alice, bob, carol', type: 'G'}}}],
        ['an unknown channel', {currentChannelId: 'gone', channels: {}}],
    ] as const) {
        test(`hides the channel-bound tiles in ${where}`, async ({mount, page}) => {
            await routeConfig(page);
            await mount(
                <StoreHarness
                    pluginState={open}
                    entities={{channels}}
                ><RequestHubModal/></StoreHarness>,
            );
            await configLoaded(page);
            await expect.poll(() => tiles(page)).toEqual(OUTSIDE_TEAM_CHANNEL);
        });
    }

    test('hides the Outgoing Webhook tile in a private channel', async ({mount, page}) => {
        await routeConfig(page);
        await mount(
            <StoreHarness
                pluginState={open}
                entities={{channels: {currentChannelId: 'p1', channels: {p1: {display_name: 'Ops', type: 'P'}}}}}
            ><RequestHubModal/></StoreHarness>,
        );
        await configLoaded(page);
        await expect.poll(() => tiles(page)).toEqual(ALL_TILES.filter((t) => t !== 'Request Outgoing Webhook'));
    });

    test('hides the webhook tiles with no current team', async ({mount, page}) => {
        await routeConfig(page);
        await mount(
            <StoreHarness
                pluginState={open}
                entities={{teams: {currentTeamId: '', teams: {}, myMembers: {}}}}
            ><RequestHubModal/></StoreHarness>,
        );
        await configLoaded(page);
        await expect(page.getByText('Request Guest Invite')).toBeVisible();
        expect(await tiles(page)).not.toContain('Request Incoming Webhook');
        expect(await tiles(page)).not.toContain('Request Outgoing Webhook');
    });

    test('hides Request Team Admin from a Team Admin', async ({mount, page}) => {
        await routeConfig(page);
        await mount(
            <StoreHarness
                pluginState={open}
                entities={{
                    teams: {
                        currentTeamId: 'team1',
                        teams: {team1: {id: 'team1', display_name: 'Team One', name: 'team-one'}},
                        myMembers: {team1: {roles: 'team_user team_admin', scheme_admin: false}},
                    },
                }}
            ><RequestHubModal/></StoreHarness>,
        );
        await configLoaded(page);
        await expect(page.getByText('Request Guest Invite')).toBeVisible();
        expect(await tiles(page)).not.toContain('Request Team Admin');
    });

    test('a tile closes the hub and opens its form', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<StoreHarness pluginState={open}><RequestHubModal/><RequestBotModal/></StoreHarness>);
        await page.getByText('Request a Bot', {exact: true}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
        await expect(page.getByText('Request a Bot Account')).toBeVisible();
    });

    test('the Channel Admin tile opens the form for the current channel', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<StoreHarness pluginState={open}><RequestHubModal/><RequestChannelAdminModal/></StoreHarness>);
        await page.getByText('Request Channel Admin').click();
        await expect(page.getByText(TITLE)).toBeHidden();
        await expect(page.locator('strong')).toHaveText('Town Square');
    });

    test('links to the plugin docs in a new tab', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<StoreHarness pluginState={open}><RequestHubModal/></StoreHarness>);
        const docs = page.getByRole('link', {name: /Documentation/});
        await expect(docs).toHaveAttribute('href', `/plugins/${manifest.id}/public/help/docs.html`);
        await expect(docs).toHaveAttribute('target', '_blank');
    });

    test('the close button, Escape and the overlay close it', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<StoreHarness pluginState={open}><RequestHubModal/></StoreHarness>);
        await page.getByText(TITLE).click();
        await expect(page.getByText(TITLE)).toBeVisible();
        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open hub'}).click();
        await expect(page.getByRole('button', {name: 'Close'})).toBeFocused();
        await page.keyboard.press('Escape');
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open hub'}).click();
        await expect(page.getByText(TITLE)).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText(TITLE)).toBeHidden();
    });
});
