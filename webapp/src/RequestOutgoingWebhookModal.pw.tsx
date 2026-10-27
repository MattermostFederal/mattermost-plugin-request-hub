import React from 'react';

import {RequestOutgoingWebhookModal} from './RequestOutgoingWebhookModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const TITLE = 'Request an Outgoing Webhook';

const open = {outgoingWebhookModalOpen: true};

const privateEntities = {
    channels: {currentChannelId: 'priv1', channels: {priv1: {display_name: 'Planning', type: 'P'}}},
};

test.describe('RequestOutgoingWebhookModal', () => {
    test('renders nothing while closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestOutgoingWebhookModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('names the source channel and team', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        await expect(page.getByText('Town Square(Team: Team One)')).toBeVisible();
    });

    test('shows the channel ID and no team when the names are unknown', async ({mount, page}) => {
        await mount(
            <StoreHarness
                pluginState={open}
                entities={{
                    teams: {currentTeamId: '', teams: {}, myMembers: {}},
                    channels: {currentChannelId: 'chan9', channels: {}},
                }}
            ><RequestOutgoingWebhookModal/></StoreHarness>,
        );
        await expect(page.getByText('chan9', {exact: true})).toBeVisible();
        await expect(page.getByText('(Team:', {exact: false})).toBeHidden();
    });

    test('requires a display name and an http or https callback URL', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        const submit = page.getByRole('button', {name: 'Submit request'});
        await submit.click();
        await expect(page.getByRole('alert')).toHaveText('A display name is required.');

        await page.getByLabel('Display name').fill('Alert relay');
        await submit.click();
        await expect(page.getByRole('alert')).toHaveText('A callback URL is required.');

        await page.getByLabel('Callback URL').fill('example.com/hook');
        await submit.click();
        await expect(page.getByRole('alert')).toHaveText('Callback URL must start with http:// or https://');
    });

    test('refuses a private channel as the source', async ({mount, page}) => {
        await mount(<StoreHarness
            pluginState={open}
            entities={privateEntities}
                    ><RequestOutgoingWebhookModal/></StoreHarness>);
        await expect(page.getByText('Outgoing webhooks can only use public channels.', {exact: false})).toBeVisible();
        await page.getByLabel('Display name').fill('Alert relay');
        await page.getByLabel('Callback URL').fill('https://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Open a public channel on this team first, then submit. Outgoing webhooks can only use public channels.');
    });

    test('sends the request and shows the server message', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_outgoing_webhook_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill(' Alert relay ');
        await page.getByLabel('Callback URL').fill(' http://example.com/hook ');
        await page.getByLabel('Description (optional)').fill('Forwards alerts.');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toEqual({channel_id: 'chan1', display_name: 'Alert relay', callback_url: 'http://example.com/hook', description: 'Forwards alerts.'});

        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('shows a default message and leaves out an empty description', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_outgoing_webhook_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Alert relay');
        await page.getByLabel('Callback URL').fill('https://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Your outgoing webhook request has been submitted.')).toBeVisible();
        expect(sent).toEqual({channel_id: 'chan1', display_name: 'Alert relay', callback_url: 'https://example.com/hook'});
    });

    test('shows the server error and allows a retry', async ({mount, page}) => {
        await page.route(`${API}/submit_outgoing_webhook_request`, (route) => route.fulfill({status: 400, contentType: 'application/json', body: JSON.stringify({error: 'Callback URL is not allowed.'})}));
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Alert relay');
        await page.getByLabel('Callback URL').fill('https://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Callback URL is not allowed.');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('a failed request with no body shows the status', async ({mount, page}) => {
        await page.route(`${API}/submit_outgoing_webhook_request`, (route) => route.fulfill({status: 503, body: ''}));
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Alert relay');
        await page.getByLabel('Callback URL').fill('https://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Request failed (503). Please try again.');
    });

    test('a network failure shows a connection message', async ({mount, page}) => {
        await page.route(`${API}/submit_outgoing_webhook_request`, (route) => route.abort());
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Alert relay');
        await page.getByLabel('Callback URL').fill('https://example.com/hook');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Could not reach the server. Please check your connection and try again.');
    });

    test('Cancel closes and a reopened form starts empty', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Alert relay');
        await page.getByLabel('Callback URL').fill('https://example.com/hook');
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open outgoing'}).click();
        await expect(page.getByLabel('Display name')).toHaveValue('');
        await expect(page.getByLabel('Callback URL')).toHaveValue('');
    });

    test('Escape and the overlay close, clicking the dialog does not', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestOutgoingWebhookModal/></StoreHarness>);
        await page.getByText(TITLE).click();
        await expect(page.getByText(TITLE)).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open outgoing'}).click();
        await expect(page.getByLabel('Display name')).toBeFocused();
        await page.keyboard.press('Escape');
        await expect(page.getByText(TITLE)).toBeHidden();
    });
});
