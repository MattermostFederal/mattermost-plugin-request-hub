import React from 'react';

import {RequestIncomingWebhookModal} from './RequestIncomingWebhookModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const TITLE = 'Request an Incoming Webhook';

const open = {incomingWebhookModalOpen: true};

const dmEntities = {
    channels: {currentChannelId: 'dm1', channels: {dm1: {display_name: 'alice, bob', type: 'D'}}},
};

test.describe('RequestIncomingWebhookModal', () => {
    test('renders nothing while closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestIncomingWebhookModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('names the target channel and team', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await expect(page.getByText('Town Square(Team: Team One)')).toBeVisible();
        await expect(page.getByText('The webhook will post to this channel.', {exact: false})).toBeVisible();
    });

    test('shows the channel ID and no team when the names are unknown', async ({mount, page}) => {
        await mount(
            <StoreHarness
                pluginState={open}
                entities={{
                    teams: {currentTeamId: '', teams: {}, myMembers: {}},
                    channels: {currentChannelId: 'chan9', channels: {}},
                }}
            ><RequestIncomingWebhookModal/></StoreHarness>,
        );
        await expect(page.getByText('chan9', {exact: true})).toBeVisible();
        await expect(page.getByText('(Team:', {exact: false})).toBeHidden();
    });

    test('requires a display name', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('A display name is required.');
    });

    test('refuses a DM as the target', async ({mount, page}) => {
        await mount(<StoreHarness
            pluginState={open}
            entities={dmEntities}
                    ><RequestIncomingWebhookModal/></StoreHarness>);
        await expect(page.getByText('DMs and group messages cannot be webhook targets.', {exact: false})).toBeVisible();
        await page.getByLabel('Display name').fill('Build alerts');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Open a public or private channel on this team first, then submit. Webhooks cannot target DMs or group messages.');
    });

    test('sends the request and shows the server message', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_incoming_webhook_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill(' Build alerts ');
        await page.getByLabel('Description (optional)').fill('Posts build results.');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toEqual({channel_id: 'chan1', display_name: 'Build alerts', description: 'Posts build results.'});

        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('shows a default message and leaves out an empty description', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_incoming_webhook_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Build alerts');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Your incoming webhook request has been submitted.')).toBeVisible();
        expect(sent).toEqual({channel_id: 'chan1', display_name: 'Build alerts'});
    });

    test('shows the server error and allows a retry', async ({mount, page}) => {
        await page.route(`${API}/submit_incoming_webhook_request`, (route) => route.fulfill({status: 403, contentType: 'application/json', body: JSON.stringify({error: 'Webhook requests are turned off.'})}));
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Build alerts');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Webhook requests are turned off.');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('a failed request with no body shows the status', async ({mount, page}) => {
        await page.route(`${API}/submit_incoming_webhook_request`, (route) => route.fulfill({status: 500, body: 'oops'}));
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Build alerts');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Request failed (500). Please try again.');
    });

    test('a network failure shows a connection message', async ({mount, page}) => {
        await page.route(`${API}/submit_incoming_webhook_request`, (route) => route.abort());
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Build alerts');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Could not reach the server. Please check your connection and try again.');
    });

    test('Cancel closes and a reopened form starts empty', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await page.getByLabel('Display name').fill('Build alerts');
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open incoming'}).click();
        await expect(page.getByLabel('Display name')).toHaveValue('');
    });

    test('Escape and the overlay close, clicking the dialog does not', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestIncomingWebhookModal/></StoreHarness>);
        await page.getByText(TITLE).click();
        await expect(page.getByText(TITLE)).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open incoming'}).click();
        await expect(page.getByLabel('Display name')).toBeFocused();
        await page.keyboard.press('Escape');
        await expect(page.getByText(TITLE)).toBeHidden();
    });
});
