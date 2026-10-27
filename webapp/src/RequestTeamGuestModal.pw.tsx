import React from 'react';

import {RequestTeamGuestModal} from './RequestTeamGuestModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const TITLE = 'Request Guest Invite';

const open = {teamGuestRequestModalOpen: true};
const noTeam = {teams: {currentTeamId: '', teams: {}, myMembers: {}}};
const noChannel = {channels: {currentChannelId: '', channels: {}}};

test.describe('RequestTeamGuestModal', () => {
    test('renders nothing when closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestTeamGuestModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('names the current team', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await expect(page.locator('strong')).toHaveText('Team One');
    });

    test('without a team, says "this team" and blocks submit', async ({mount, page}) => {
        await mount(<StoreHarness
            entities={noTeam}
            pluginState={open}
                    ><RequestTeamGuestModal/></StoreHarness>);
        await expect(page.locator('strong')).toHaveText('this team');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeDisabled();
    });

    test('without a channel, blocks submit', async ({mount, page}) => {
        await mount(<StoreHarness
            entities={noChannel}
            pluginState={open}
                    ><RequestTeamGuestModal/></StoreHarness>);
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeDisabled();
    });

    for (const email of ['', '   ', 'not-an-email']) {
        test(`rejects the email "${email}"`, async ({mount, page}) => {
            await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
            await page.getByLabel('Guest email').fill(email);
            await page.getByRole('button', {name: 'Submit request'}).click();
            await expect(page.getByRole('alert')).toHaveText('Enter a valid guest email. Mattermost will email a channel-scoped guest invitation on approve.');
        });
    }

    test('sends the email, current channel and reason and shows the server message', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_team_guest_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await page.getByLabel('Guest email').fill('  guest@example.com ');
        await page.getByLabel('Message / reason (optional)').fill(' Visiting for a week. ');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toEqual({team_id: 'team1', email: 'guest@example.com', channel_ids: ['chan1'], reason: 'Visiting for a week.'});

        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('leaves out a blank reason and shows the default message', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_team_guest_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await page.getByLabel('Guest email').fill('guest@example.com');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Your guest invite request has been submitted.')).toBeVisible();
        expect(sent).toEqual({team_id: 'team1', email: 'guest@example.com', channel_ids: ['chan1']});
    });

    test('shows the server error', async ({mount, page}) => {
        await page.route(`${API}/submit_team_guest_request`, (route) => route.fulfill({status: 400, contentType: 'application/json', body: JSON.stringify({error: 'Guest Accounts are turned off.'})}));
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await page.getByLabel('Guest email').fill('guest@example.com');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Guest Accounts are turned off.');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('shows a connection error when the request fails', async ({mount, page}) => {
        await page.route(`${API}/submit_team_guest_request`, (route) => route.abort());
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await page.getByLabel('Guest email').fill('guest@example.com');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Could not reach the server. Please check your connection and try again.');
    });

    test('Cancel closes', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('Escape closes', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await page.getByLabel('Guest email').focus();
        await page.keyboard.press('Escape');
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('clicking the overlay closes, clicking the dialog does not', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await page.getByRole('heading', {name: TITLE}).click();
        await expect(page.getByRole('heading', {name: TITLE})).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('a reopened form starts empty', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamGuestModal/></StoreHarness>);
        await page.getByLabel('Guest email').fill('guest@example.com');
        await page.getByLabel('Message / reason (optional)').fill('Visiting for a week.');
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open team guest'}).click();
        await expect(page.getByLabel('Guest email')).toHaveValue('');
        await expect(page.getByLabel('Message / reason (optional)')).toHaveValue('');
    });
});
