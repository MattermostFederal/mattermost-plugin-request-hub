import React from 'react';

import {RequestTeamModal} from './RequestTeamModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const TITLE = 'Request a New Team';

const open = {teamCreationModalOpen: true};

test.describe('RequestTeamModal', () => {
    test('renders nothing when closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestTeamModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('previews the URL from the name, then from the URL field', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        const preview = page.locator('code');
        await expect(preview).toHaveText('team-url');
        await page.getByLabel('Team name').fill('  Field Ops & Logistics  ');
        await expect(preview).toHaveText('field-ops-logistics');
        await page.getByLabel('Team URL (optional)').fill('--Ops--Team--');
        await expect(preview).toHaveText('ops-team');
    });

    test('requires a team name', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').fill('   ');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('A team name is required.');
    });

    test('sends every field and shows the server message', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_team_creation`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').fill(' Team Two ');
        await page.getByLabel('Team URL (optional)').fill(' team-two ');
        await page.getByLabel('Description (optional)').fill(' For testing. ');
        await page.getByLabel('Visibility').selectOption('I');
        await page.getByLabel('I want to be Team Admin of this team').check();
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toEqual({
            display_name: 'Team Two',
            name: 'team-two',
            type: 'I',
            description: 'For testing.',
            request_team_admin: true,
        });

        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('shows a default message when the server sends none', async ({mount, page}) => {
        await page.route(`${API}/submit_team_creation`, (route) => route.fulfill({contentType: 'application/json', body: '{}'}));
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').fill('Team Two');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Your team request has been submitted.')).toBeVisible();
    });

    test('disables the buttons while submitting', async ({mount, page}) => {
        let release: () => void = () => undefined;
        const held = new Promise<void>((resolve) => {
            release = resolve;
        });
        await page.route(`${API}/submit_team_creation`, async (route) => {
            await held;
            await route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').fill('Team Two');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('button', {name: 'Submitting…'})).toBeDisabled();
        await expect(page.getByRole('button', {name: 'Cancel'})).toBeDisabled();
        release();
        await expect(page.getByText('Your team request has been submitted.')).toBeVisible();
    });

    test('shows the server error', async ({mount, page}) => {
        await page.route(`${API}/submit_team_creation`, (route) => route.fulfill({status: 403, contentType: 'application/json', body: JSON.stringify({error: 'Team requests are turned off.'})}));
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').fill('Team Two');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Team requests are turned off.');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('shows a status error when the failure has no body', async ({mount, page}) => {
        await page.route(`${API}/submit_team_creation`, (route) => route.fulfill({status: 500, body: 'oops'}));
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').fill('Team Two');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Request failed (500). Please try again.');
    });

    test('shows a connection error when the request fails', async ({mount, page}) => {
        await page.route(`${API}/submit_team_creation`, (route) => route.abort());
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').fill('Team Two');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Could not reach the server. Please check your connection and try again.');
    });

    test('Cancel closes', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('Escape closes', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').focus();
        await page.keyboard.press('Escape');
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('clicking the overlay closes, clicking the dialog does not', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByText(TITLE).click();
        await expect(page.getByText(TITLE)).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('a reopened form starts empty', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamModal/></StoreHarness>);
        await page.getByLabel('Team name').fill('Team Two');
        await page.getByLabel('Team URL (optional)').fill('team-two');
        await page.getByLabel('Description (optional)').fill('For testing.');
        await page.getByLabel('Visibility').selectOption('I');
        await page.getByLabel('I want to be Team Admin of this team').check();
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open team', exact: true}).click();
        await expect(page.getByLabel('Team name')).toHaveValue('');
        await expect(page.getByLabel('Team URL (optional)')).toHaveValue('');
        await expect(page.getByLabel('Description (optional)')).toHaveValue('');
        await expect(page.getByLabel('Visibility')).toHaveValue('O');
        await expect(page.getByLabel('I want to be Team Admin of this team')).not.toBeChecked();
    });
});
