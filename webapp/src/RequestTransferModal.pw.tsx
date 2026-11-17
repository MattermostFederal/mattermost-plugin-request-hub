import type {Page} from '@playwright/test';
import React from 'react';

import {BotTransferModal, WebhookTransferModal} from './RequestTransferModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const BOT_TITLE = 'Transfer or retire a bot';
const WEBHOOK_TITLE = 'Transfer or retire a webhook';

const botOpen = {botTransferModalOpen: true};
const webhookOpen = {webhookTransferModalOpen: true};

const botsPayload = {
    bots: [{asset_id: 'b1', display_name: '@svcbot', orphaned: false}],
    webhooks: [],
};
const webhooksPayload = {
    bots: [],
    webhooks: [{asset_id: 'h1', display_name: 'Alerts (incoming)', orphaned: true}],
};

async function routeAssets(page: Page, body: unknown) {
    await page.route(`${API}/transferable_assets`, (route) => route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify(body),
    }));
}

async function routeUsers(page: Page) {
    await page.route(`${API}/user_autocomplete**`, (route) => route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify([{id: 'u1', username: 'alice'}]),
    }));
}

async function pickAlice(page: Page) {
    await page.getByPlaceholder('Type a name…').fill('ali');
    await page.getByText('@alice').click();
}

test.describe('BotTransferModal', () => {
    test('renders nothing while closed', async ({mount, page}) => {
        await mount(<StoreHarness><BotTransferModal/></StoreHarness>);
        await expect(page.getByText(BOT_TITLE)).toBeHidden();
    });

    test('requires a bot and a new owner for transfer', async ({mount, page}) => {
        await routeAssets(page, botsPayload);
        await mount(<StoreHarness pluginState={botOpen}><BotTransferModal/></StoreHarness>);
        await expect(page.getByText(BOT_TITLE)).toBeVisible();
        await page.getByRole('button', {name: 'Submit request'}).click();

        // Auto-selects the only bot; still needs a new owner.
        await expect(page.getByRole('alert')).toHaveText('Pick the new owner.');
    });

    test('hides the new-owner field for retire', async ({mount, page}) => {
        await routeAssets(page, botsPayload);
        await mount(<StoreHarness pluginState={botOpen}><BotTransferModal/></StoreHarness>);
        await page.getByLabel('Action').selectOption('retire');
        await expect(page.getByPlaceholder('Type a name…')).toBeHidden();
        await expect(page.getByText('Rotate the access token', {exact: false})).toBeHidden();
    });

    test('sends a transfer with rotate and shows the server message', async ({mount, page}) => {
        await routeAssets(page, botsPayload);
        await routeUsers(page);
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_bot_transfer_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Queued for approval.'})});
        });
        await mount(<StoreHarness pluginState={botOpen}><BotTransferModal/></StoreHarness>);
        await pickAlice(page);
        await page.getByText('Rotate the access token', {exact: false}).click();
        await page.getByLabel('Note').fill(' Jane left ');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Queued for approval.')).toBeVisible();
        expect(sent).toEqual({
            bot_user_id: 'b1',
            action: 'transfer',
            new_owner_username: 'alice',
            rotate_token: true,
            note: 'Jane left',
        });
        await page.getByRole('button', {name: 'Done'}).click();
        await expect(page.getByText(BOT_TITLE)).toBeHidden();
    });

    test('shows the server error and allows a retry', async ({mount, page}) => {
        await routeAssets(page, botsPayload);
        await routeUsers(page);
        await page.route(`${API}/submit_bot_transfer_request`, (route) => route.fulfill({
            contentType: 'application/json',
            body: JSON.stringify({error: 'only the current owner can transfer'}),
        }));
        await mount(<StoreHarness pluginState={botOpen}><BotTransferModal/></StoreHarness>);
        await pickAlice(page);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('only the current owner can transfer');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('shows a load error when the asset list fails', async ({mount, page}) => {
        await page.route(`${API}/transferable_assets`, (route) => route.fulfill({status: 500, body: ''}));
        await mount(<StoreHarness pluginState={botOpen}><BotTransferModal/></StoreHarness>);
        await expect(page.getByRole('alert')).toHaveText('Could not load the assets you can transfer. Try again.');
    });
});

test.describe('WebhookTransferModal', () => {
    test('renders nothing while closed', async ({mount, page}) => {
        await mount(<StoreHarness><WebhookTransferModal/></StoreHarness>);
        await expect(page.getByText(WEBHOOK_TITLE)).toBeHidden();
    });

    test('lists orphaned webhooks and submits retire & recreate', async ({mount, page}) => {
        await routeAssets(page, webhooksPayload);
        await routeUsers(page);
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_webhook_transfer_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Webhook transfer submitted.'})});
        });
        await mount(<StoreHarness pluginState={webhookOpen}><WebhookTransferModal/></StoreHarness>);
        await expect(page.getByRole('option', {name: 'Alerts (incoming) (owner deactivated)'})).toBeAttached();
        await page.getByLabel('Action').selectOption('retire_recreate');
        await pickAlice(page);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Webhook transfer submitted.')).toBeVisible();
        expect(sent).toEqual({
            hook_id: 'h1',
            action: 'retire_recreate',
            new_owner_username: 'alice',
            note: '',
        });
    });

    test('retire-only does not ask for a new owner', async ({mount, page}) => {
        await routeAssets(page, webhooksPayload);
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_webhook_transfer_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={webhookOpen}><WebhookTransferModal/></StoreHarness>);
        await page.getByLabel('Action').selectOption('retire_only');
        await expect(page.getByPlaceholder('Type a name…')).toBeHidden();
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Your request was submitted for approval.')).toBeVisible();
        expect(sent).toEqual({
            hook_id: 'h1',
            action: 'retire_only',
            new_owner_username: '',
            note: '',
        });
    });
});
