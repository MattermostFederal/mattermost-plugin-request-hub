import type {Page} from '@playwright/test';
import React from 'react';

import {RequestChannelAdminModal} from './RequestChannelAdminModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';

const API = '**/plugins/com.mattermostfederal.mattermost-plugin-request-hub/api/v1';

async function routeUsers(page: Page) {
    await page.route(`${API}/user_autocomplete**`, (route) => route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify([{id: 'u1', username: 'alice'}]),
    }));
}

async function pickAlice(page: Page) {
    await page.locator('label[for="cra-nominees"] + div input').fill('ali');
    await page.getByText('@alice').click();
}

test.describe('RequestChannelAdminModal', () => {
    test('renders nothing without a channel', async ({mount, page}) => {
        await mount(<StoreHarness><RequestChannelAdminModal/></StoreHarness>);
        await expect(page.getByText('Request a Channel Admin')).toBeHidden();
    });

    test('names the channel', async ({mount, page}) => {
        await mount(<StoreHarness adminChannelId='chan1'><RequestChannelAdminModal/></StoreHarness>);
        await expect(page.locator('strong')).toHaveText('Town Square');
    });

    test('leaves out the channel name when it is unknown', async ({mount, page}) => {
        await mount(<StoreHarness adminChannelId='other'><RequestChannelAdminModal/></StoreHarness>);
        await expect(page.getByText('Request that someone be made a Channel Admin. Your request will be sent to an admin for approval.')).toBeVisible();
    });

    test('requires at least one nominee', async ({mount, page}) => {
        await mount(<StoreHarness adminChannelId='chan1'><RequestChannelAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Pick at least one person to make a Channel Admin.')).toBeVisible();
    });

    test('sends the nominees and shows the server message', async ({mount, page}) => {
        await routeUsers(page);
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/request_admin`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness adminChannelId='chan1'><RequestChannelAdminModal/></StoreHarness>);
        await pickAlice(page);
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toEqual({channel_id: 'chan1', nominees: ['alice']});

        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText('Request a Channel Admin')).toBeHidden();
    });

    test('shows a default message when the server sends none', async ({mount, page}) => {
        await routeUsers(page);
        await page.route(`${API}/request_admin`, (route) => route.fulfill({contentType: 'application/json', body: '{}'}));
        await mount(<StoreHarness adminChannelId='chan1'><RequestChannelAdminModal/></StoreHarness>);
        await pickAlice(page);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Your Channel Admin request has been submitted.')).toBeVisible();
    });

    test('shows the server error', async ({mount, page}) => {
        await routeUsers(page);
        await page.route(`${API}/request_admin`, (route) => route.fulfill({status: 403, contentType: 'application/json', body: JSON.stringify({error: 'You must be a member of this channel.'})}));
        await mount(<StoreHarness adminChannelId='chan1'><RequestChannelAdminModal/></StoreHarness>);
        await pickAlice(page);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('You must be a member of this channel.')).toBeVisible();
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('Cancel closes and a reopened form starts empty', async ({mount, page}) => {
        await routeUsers(page);
        await mount(<StoreHarness adminChannelId='chan1'><RequestChannelAdminModal/></StoreHarness>);
        await pickAlice(page);
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText('Request a Channel Admin')).toBeHidden();

        await page.getByRole('button', {name: 'harness: open admin modal'}).click();
        await expect(page.getByText('@alice')).toBeHidden();
    });

    test('clicking the overlay closes, clicking the dialog does not', async ({mount, page}) => {
        await mount(<StoreHarness adminChannelId='chan1'><RequestChannelAdminModal/></StoreHarness>);
        await page.getByText('Request a Channel Admin').click();
        await expect(page.getByText('Request a Channel Admin')).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText('Request a Channel Admin')).toBeHidden();
    });
});
