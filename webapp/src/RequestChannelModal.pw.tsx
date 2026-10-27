import type {Page} from '@playwright/test';
import React from 'react';

import {RequestChannelModal} from './RequestChannelModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';

const API = '**/plugins/com.mattermostfederal.mattermost-plugin-request-hub/api/v1';

const prefixes = [
    {prefix: 'team-', description: 'Team channels', suffix_regex: ''},
    {prefix: 'ops-', description: '', suffix_regex: ''},
];

const users = [
    {id: 'u1', username: 'alice', first_name: 'Alice', last_name: 'Adams'},
    {id: 'u2', username: 'bob', nickname: 'Bobby'},
];

async function routePrefixes(page: Page, body: unknown = prefixes, status = 200) {
    await page.route(`${API}/prefixes`, (route) => route.fulfill({status, contentType: 'application/json', body: JSON.stringify(body)}));
}

async function routeUsers(page: Page) {
    await page.route(`${API}/user_autocomplete**`, (route) => route.fulfill({contentType: 'application/json', body: JSON.stringify(users)}));
}

const membersInput = (page: Page) => page.locator('label[for="cr-members"] + div input');
const adminsInput = (page: Page) => page.locator('label[for="cr-admin-members"] + div input');

test.describe('RequestChannelModal', () => {
    test.describe('rendering', () => {
        test('renders nothing while closed', async ({mount, page}) => {
            await mount(<StoreHarness><RequestChannelModal/></StoreHarness>);
            await expect(page.getByText('Request a Channel')).toBeHidden();
        });

        test('shows loading, then the form with the prefix list', async ({mount, page}) => {
            let release: () => void = () => undefined;
            const gate = new Promise<void>((resolve) => {
                release = resolve;
            });
            await page.route(`${API}/prefixes`, async (route) => {
                await gate;
                await route.fulfill({contentType: 'application/json', body: JSON.stringify(prefixes)});
            });
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await expect(page.getByText('Loading…')).toBeVisible();
            release();

            const prefix = page.getByLabel('Domain prefix');
            await expect(prefix).toHaveValue('team-');
            await expect(prefix.locator('option')).toHaveText(['team-  (Team channels)', 'ops-']);
            await expect(page.getByRole('link', {name: 'Need help?'})).toHaveAttribute('href', '/plugins/com.mattermostfederal.mattermost-plugin-request-hub/public/help/docs.html');
        });

        test('explains when no prefixes are configured', async ({mount, page}) => {
            await routePrefixes(page, []);
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await expect(page.getByText('Channel creation requests aren’t configured yet', {exact: false})).toBeVisible();
            await page.getByRole('button', {name: 'Close'}).click();
            await expect(page.getByText('Request a Channel')).toBeHidden();
        });

        test('offers a retry when prefixes fail to load', async ({mount, page}) => {
            let fail = true;
            await page.route(`${API}/prefixes`, (route) => (fail ? route.fulfill({status: 500, body: ''}) : route.fulfill({contentType: 'application/json', body: JSON.stringify(prefixes)})));
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await expect(page.getByText('Could not load channel request settings. Please try again.')).toBeVisible();

            fail = false;
            await page.getByRole('button', {name: 'Retry'}).click();
            await expect(page.getByLabel('Domain prefix')).toBeVisible();
        });

        test('closes from the load error', async ({mount, page}) => {
            await page.route(`${API}/prefixes`, (route) => route.abort('connectionrefused'));
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByRole('button', {name: 'Close'}).click();
            await expect(page.getByText('Request a Channel')).toBeHidden();
        });
    });

    test.describe('URL preview', () => {
        test.beforeEach(async ({page}) => {
            await routePrefixes(page);
        });

        test('previews the placeholder suffix, then the slugified name', async ({mount, page}) => {
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            const preview = page.locator('code');
            await expect(preview).toHaveText('team-suffix');

            await page.getByLabel('Channel name').fill('  Marketing Team!! ');
            await expect(preview).toHaveText('team-marketing-team');
        });

        test('prefers the URL suffix and follows the selected prefix', async ({mount, page}) => {
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByLabel('Channel name').fill('Marketing Team');
            await page.getByLabel('URL suffix (optional)').fill('Launch 2026');
            await page.getByLabel('Domain prefix').selectOption('ops-');
            await expect(page.locator('code')).toHaveText('ops-launch-2026');
        });
    });

    test.describe('submitting', () => {
        test.beforeEach(async ({page}) => {
            await routePrefixes(page);
        });

        test('requires a channel name', async ({mount, page}) => {
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByLabel('Channel name').fill('   ');
            await page.getByRole('button', {name: 'Submit request'}).click();
            await expect(page.getByText('A channel name is required.')).toBeVisible();
        });

        test('sends the request and shows the server message', async ({mount, page}) => {
            await routeUsers(page);
            let sent: Record<string, unknown> = {};
            await page.route(`${API}/create`, async (route) => {
                sent = route.request().postDataJSON();
                await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Request sent for approval.'})});
            });
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);

            await page.getByLabel('Domain prefix').selectOption('ops-');
            await page.getByLabel('Channel name').fill(' Ops Room ');
            await page.getByLabel('URL suffix (optional)').fill('room');
            await page.getByLabel('Purpose (optional)').fill(' Daily ops ');
            await page.getByLabel('Visibility').selectOption('P');
            await membersInput(page).fill('ali');
            await page.getByText('@alice').click();
            await adminsInput(page).fill('bo');
            await page.getByText('@bob').click();

            await page.getByRole('button', {name: 'Submit request'}).click();
            await expect(page.getByText('Request sent for approval.')).toBeVisible();
            expect(sent).toEqual({
                team_id: 'team1',
                display_name: 'Ops Room',
                name: 'room',
                prefix: 'ops-',
                purpose: 'Daily ops',
                channel_type: 'P',
                members: ['alice'],
                admin_members: ['bob'],
            });
        });

        test('shows a default message when the server sends none', async ({mount, page}) => {
            await page.route(`${API}/create`, (route) => route.fulfill({contentType: 'application/json', body: '{}'}));
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByLabel('Channel name').fill('Ops');
            await page.getByRole('button', {name: 'Submit request'}).click();
            await expect(page.getByText('Your channel request has been submitted.')).toBeVisible();
            await page.getByRole('button', {name: 'Close'}).click();
            await expect(page.getByText('Request a Channel')).toBeHidden();
        });

        test('shows the server error', async ({mount, page}) => {
            await page.route(`${API}/create`, (route) => route.fulfill({status: 400, contentType: 'application/json', body: JSON.stringify({error: 'Channel name is already taken'})}));
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByLabel('Channel name').fill('Ops');
            await page.getByRole('button', {name: 'Submit request'}).click();
            await expect(page.getByText('Channel name is already taken')).toBeVisible();
            await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
        });

        test('shows a connection error when the request fails', async ({mount, page}) => {
            await page.route(`${API}/create`, (route) => route.abort('connectionrefused'));
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByLabel('Channel name').fill('Ops');
            await page.getByRole('button', {name: 'Submit request'}).click();
            await expect(page.getByText('Could not reach the server. Please check your connection and try again.')).toBeVisible();
        });

        test('disables the buttons while submitting', async ({mount, page}) => {
            let release: () => void = () => undefined;
            const gate = new Promise<void>((resolve) => {
                release = resolve;
            });
            await page.route(`${API}/create`, async (route) => {
                await gate;
                await route.fulfill({contentType: 'application/json', body: '{}'});
            });
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByLabel('Channel name').fill('Ops');
            await page.getByRole('button', {name: 'Submit request'}).click();
            await expect(page.getByRole('button', {name: 'Submitting…'})).toBeDisabled();
            await expect(page.getByRole('button', {name: 'Cancel'})).toBeDisabled();
            release();
            await expect(page.getByText('Your channel request has been submitted.')).toBeVisible();
        });
    });

    test.describe('closing', () => {
        test.beforeEach(async ({page}) => {
            await routePrefixes(page);
        });

        test('Cancel closes and a reopened form starts empty', async ({mount, page}) => {
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByLabel('Domain prefix').selectOption('ops-');
            await page.getByLabel('Channel name').fill('Ops');
            await page.getByRole('button', {name: 'Cancel'}).click();
            await expect(page.getByText('Request a Channel')).toBeHidden();

            await page.getByRole('button', {name: 'harness: open request modal'}).click();
            await expect(page.getByLabel('Channel name')).toHaveValue('');
            await expect(page.getByLabel('Domain prefix')).toHaveValue('team-');
        });

        test('clicking the overlay closes, clicking the dialog does not', async ({mount, page}) => {
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await page.getByText('Request a Channel').click();
            await expect(page.getByText('Request a Channel')).toBeVisible();
            await page.mouse.click(5, 5);
            await expect(page.getByText('Request a Channel')).toBeHidden();
        });
    });

    test.describe('member pickers', () => {
        test.beforeEach(async ({page}) => {
            await routePrefixes(page);
            await routeUsers(page);
        });

        test('badges a user picked in the other list', async ({mount, page}) => {
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            await membersInput(page).fill('ali');
            await page.getByText('@alice').click();

            await adminsInput(page).fill('a');
            await expect(page.getByText('Current assignments')).toBeVisible();
            await expect(page.getByText('Member', {exact: true})).toBeVisible();
            await expect(page.getByText('Available to add')).toBeVisible();
        });

        test('moves a member to Channel Admin', async ({mount, page}) => {
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            const membersBox = page.locator('label[for="cr-members"] + div');
            const adminsBox = page.locator('label[for="cr-admin-members"] + div');
            await membersInput(page).fill('ali');
            await page.getByText('@alice').click();
            await adminsInput(page).fill('ali');
            await adminsBox.getByText('@alice').click();

            await expect(membersBox.getByText('@alice')).toBeHidden();
            await expect(adminsBox.getByText('@alice')).toBeVisible();
        });

        test('moves a Channel Admin back to member', async ({mount, page}) => {
            await mount(<StoreHarness requestOpen={true}><RequestChannelModal/></StoreHarness>);
            const membersBox = page.locator('label[for="cr-members"] + div');
            const adminsBox = page.locator('label[for="cr-admin-members"] + div');
            await adminsInput(page).fill('bo');
            await page.getByText('@bob').click();
            await membersInput(page).fill('bo');
            await membersBox.getByText('@bob').click();

            await expect(adminsBox.getByText('@bob')).toBeHidden();
            await expect(membersBox.getByText('@bob')).toBeVisible();
        });
    });
});
