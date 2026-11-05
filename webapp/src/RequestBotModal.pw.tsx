import React from 'react';

import {RequestBotModal} from './RequestBotModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const TITLE = 'Request a Bot Account';

const open = {botRequestModalOpen: true};

const TEAMS = [{id: 'team1', name: 'team-one', display_name: 'Team One'}, {id: 'team2', name: 'crimson', display_name: 'Crimson'}];
const CHANNELS_T1 = [
    {id: 'chan1', name: 'town-square', display_name: 'Town Square', type: 'O'},
    {id: 'chan2', name: 'ops', display_name: 'Ops', type: 'P'},
];
const CHANNELS_T2 = [
    {id: 'chan9', name: 'alerts', display_name: 'Alerts', type: 'O'},
];

async function stubBotForm(page: import('@playwright/test').Page, compliance: {enabled: boolean; text?: string; sha256?: string} = {enabled: false}) {
    await page.route(`${API}/bot_form`, (route) => route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({compliance}),
    }));
}

async function mockHomeLists(page: import('@playwright/test').Page) {
    await page.route(`${API}/teams`, (route) => route.fulfill({contentType: 'application/json', body: JSON.stringify(TEAMS)}));
    await page.route((url) => url.pathname.endsWith('/api/v1/channels') && url.searchParams.has('team_id'), async (route) => {
        const url = new URL(route.request().url());
        const teamId = url.searchParams.get('team_id');
        const list = teamId === 'team2' ? CHANNELS_T2 : CHANNELS_T1;
        await route.fulfill({contentType: 'application/json', body: JSON.stringify(list)});
    });
}

async function fillRequired(page: import('@playwright/test').Page, username = 'helper-bot', display = 'Helper Bot', purpose = 'CI notifier for builds') {
    await page.getByLabel('Bot username').fill(username);
    await page.getByLabel('Display name').fill(display);
    await page.getByLabel('Purpose').fill(purpose);
    await page.getByLabel('Architecture link (optional)').fill('https://docs.example/arch');
    await page.getByLabel('Integration endpoint').fill('https://ci.example/hooks/mm');
    await expect(page.getByLabel('Home team')).toHaveValue('team1');
    await expect(page.getByLabel('Home channel')).toHaveValue('chan1');
}

test.describe('RequestBotModal', () => {
    test.beforeEach(async ({page}) => {
        await stubBotForm(page);
        await mockHomeLists(page);
    });

    test('renders nothing while closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestBotModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('requires a username', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('A bot username is required.');
    });

    test('optional architecture confirm then requires integration URL', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('helper-bot');
        await page.getByLabel('Display name').fill('Helper Bot');
        await page.getByLabel('Purpose').fill('CI notifier');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('An integration endpoint is required (https URL).');
        await page.getByLabel('Integration endpoint').fill('https://ci.example/hooks/mm');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByTestId('bot-arch-confirm')).toBeVisible();
        await page.getByRole('button', {name: 'Go back'}).click();
        await expect(page.getByTestId('bot-arch-confirm')).toHaveCount(0);
    });

    test('Other hosting requires a specify field', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await fillRequired(page);
        await page.getByLabel('Other', {exact: true}).check();
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Specify where the script or service runs when Hosting is Other.');
        await page.getByLabel('Specify hosting').fill('air-gapped rack');
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_bot_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'ok'})});
        });
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('ok', {exact: true})).toBeVisible();
        expect(sent).toMatchObject({
            hosting_env: 'other',
            hosting_other_detail: 'air-gapped rack',
        });
    });

    test('cascading team/channel dropdowns and submit payload', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_bot_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await expect(page.getByText('Regular user (member); never Team Admin or System Admin')).toBeVisible();
        await expect(page.getByLabel('Home team')).toHaveValue('team1');
        await page.getByLabel('Home team').selectOption('team2');
        await expect(page.getByLabel('Home channel')).toHaveValue('chan9');
        await page.getByLabel('Bot username').fill(' helper-bot ');
        await page.getByLabel('Display name').fill(' Helper Bot ');
        await page.getByLabel('Purpose').fill('CI notifier for builds');
        await page.getByLabel('Architecture link (optional)').fill('https://docs.example/arch');
        await page.getByLabel('Integration endpoint').fill('https://ci.example/hooks/mm');
        await page.getByLabel('Request an access token').uncheck();
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toMatchObject({
            username: 'helper-bot',
            display_name: 'Helper Bot',
            request_token: false,
            purpose: 'CI notifier for builds',
            architecture_url: 'https://docs.example/arch',
            integration_endpoint: 'https://ci.example/hooks/mm',
            home_team_id: 'team2',
            home_channel_id: 'chan9',
            hosting_env: 'ci_cd',
        });
        expect(sent).not.toHaveProperty('hosting_other_detail');
    });

    test('an outgoing webhook needs an http or https callback URL', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await fillRequired(page);
        await page.getByLabel('Request an outgoing webhook').check();
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('A callback URL is required for the outgoing webhook.');

        await page.getByLabel('Callback URL').fill('ftp://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Callback URL must start with http:// or https://.');
    });

    test('sends both webhooks for the selected home channel', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_bot_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await fillRequired(page, 'helper-bot', 'Helper Bot', 'Posts build results to the team channel');
        await page.getByLabel('Request an incoming webhook').check();
        await expect(page.locator('#iwh-name')).toHaveValue('helper-bot incoming');
        await page.getByLabel('Request an outgoing webhook').check();
        await page.locator('#owh-name').fill('Build relay');
        await page.getByLabel('Callback URL').fill('https://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Your bot account request has been submitted.')).toBeVisible();
        expect(sent).toMatchObject({
            incoming_webhook_channel_id: 'chan1',
            outgoing_webhook_channel_id: 'chan1',
            outgoing_webhook_callback_url: 'https://example.com/hook',
            home_channel_id: 'chan1',
        });
    });

    test('shows the server error and allows a retry', async ({mount, page}) => {
        await page.route(`${API}/submit_bot_request`, (route) => route.fulfill({status: 400, contentType: 'application/json', body: JSON.stringify({error: 'That username is taken.'})}));
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await fillRequired(page);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('That username is taken.');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('Cancel closes and a reopened form starts empty', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('helper-bot');
        await page.getByLabel('Request an incoming webhook').check();
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open bot'}).click();
        await expect(page.getByLabel('Bot username')).toHaveValue('');
        await expect(page.getByLabel('Request an incoming webhook')).not.toBeChecked();
        await expect(page.getByLabel('Request an access token')).toBeChecked();
    });
});
