import React from 'react';

import {RequestBotModal} from './RequestBotModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const TITLE = 'Request a Bot Account';

const open = {botRequestModalOpen: true};

test.describe('RequestBotModal', () => {
    test('renders nothing while closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestBotModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('requires a username', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('A bot username is required.');
    });

    test('requires a 3 to 22 character username', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('ab');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Bot username must be 3-22 characters.');
    });

    test('requires a display name', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('helper-bot');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('A display name is required.');
    });

    test('an outgoing webhook needs an http or https callback URL', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('helper-bot');
        await page.getByLabel('Display name').fill('Helper Bot');
        await page.getByLabel('Request an outgoing webhook').check();
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('A callback URL is required for the outgoing webhook.');

        await page.getByLabel('Callback URL').fill('ftp://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Callback URL must start with http:// or https://.');
    });

    test('sends the bot fields only, and shows the server message', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_bot_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await expect(page.getByText('An access token lets external systems authenticate as this bot.', {exact: false})).toBeVisible();
        await page.getByLabel('Request an access token').uncheck();
        await expect(page.getByText('An access token lets external systems authenticate as this bot.', {exact: false})).toBeHidden();
        await page.getByLabel('Bot username').fill(' helper-bot ');
        await page.getByLabel('Display name').fill(' Helper Bot ');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toEqual({username: 'helper-bot', display_name: 'Helper Bot', request_token: false});

        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('sends both webhooks for the current channel', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_bot_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('helper-bot');
        await page.getByLabel('Display name').fill('Helper Bot');
        await page.getByLabel('Description (optional)').fill('Posts build results.');

        await page.getByLabel('Request an incoming webhook').check();
        await expect(page.locator('#iwh-name')).toHaveValue('helper-bot incoming');
        await expect(page.getByText('Town Square(Team: Team One)').first()).toBeVisible();
        await page.getByLabel('Request an outgoing webhook').check();
        await expect(page.locator('#owh-name')).toHaveValue('helper-bot outgoing');
        await page.locator('#owh-name').fill('Build relay');
        await page.getByLabel('Callback URL').fill('https://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Your bot account request has been submitted.')).toBeVisible();
        expect(sent).toEqual({
            username: 'helper-bot',
            display_name: 'Helper Bot',
            description: 'Posts build results.',
            request_token: true,
            incoming_webhook_channel_id: 'chan1',
            incoming_webhook_display_name: 'helper-bot incoming',
            outgoing_webhook_channel_id: 'chan1',
            outgoing_webhook_display_name: 'Build relay',
            outgoing_webhook_callback_url: 'https://example.com/hook',
        });
    });

    test('webhook names start empty when there is no username yet, and keep a typed name', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Request an incoming webhook').check();
        await expect(page.locator('#iwh-name')).toHaveValue('');
        await expect(page.locator('#iwh-name')).toHaveAttribute('placeholder', 'my-bot incoming');
        await page.locator('#iwh-name').fill('Kept');
        await page.getByLabel('Request an incoming webhook').uncheck();
        await page.getByLabel('Request an incoming webhook').check();
        await expect(page.locator('#iwh-name')).toHaveValue('Kept');

        await page.getByLabel('Request an outgoing webhook').check();
        await expect(page.locator('#owh-name')).toHaveValue('');
        await expect(page.locator('#owh-name')).toHaveAttribute('placeholder', 'my-bot outgoing');
        await page.getByLabel('Bot username').fill('helper-bot');
        await expect(page.locator('#owh-name')).toHaveAttribute('placeholder', 'helper-bot outgoing');
        await expect(page.locator('#iwh-name')).toHaveAttribute('placeholder', 'helper-bot incoming');
    });

    test('shows the channel ID and no team when the names are unknown', async ({mount, page}) => {
        await mount(
            <StoreHarness
                pluginState={open}
                entities={{
                    teams: {currentTeamId: '', teams: {}, myMembers: {}},
                    channels: {currentChannelId: 'chan9', channels: {}},
                }}
            ><RequestBotModal/></StoreHarness>,
        );
        await page.getByLabel('Request an incoming webhook').check();
        await expect(page.getByText('chan9', {exact: true})).toBeVisible();
        await expect(page.getByText('(Team:', {exact: false})).toBeHidden();
    });

    test('shows the server error and allows a retry', async ({mount, page}) => {
        await page.route(`${API}/submit_bot_request`, (route) => route.fulfill({status: 400, contentType: 'application/json', body: JSON.stringify({error: 'That username is taken.'})}));
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('helper-bot');
        await page.getByLabel('Display name').fill('Helper Bot');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('That username is taken.');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('a failed request with no body shows the status', async ({mount, page}) => {
        await page.route(`${API}/submit_bot_request`, (route) => route.fulfill({status: 502, body: ''}));
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('helper-bot');
        await page.getByLabel('Display name').fill('Helper Bot');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Request failed (502). Please try again.');
    });

    test('a network failure shows a connection message', async ({mount, page}) => {
        await page.route(`${API}/submit_bot_request`, (route) => route.abort());
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByLabel('Bot username').fill('helper-bot');
        await page.getByLabel('Display name').fill('Helper Bot');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Could not reach the server. Please check your connection and try again.');
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

    test('Escape and the overlay close, clicking the dialog does not', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestBotModal/></StoreHarness>);
        await page.getByText(TITLE).click();
        await expect(page.getByText(TITLE)).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open bot'}).click();
        await expect(page.getByLabel('Bot username')).toBeFocused();
        await page.keyboard.press('Escape');
        await expect(page.getByText(TITLE)).toBeHidden();
    });
});
