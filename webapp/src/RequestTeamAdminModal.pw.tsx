import React from 'react';

import {RequestTeamAdminModal} from './RequestTeamAdminModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const TITLE = 'Request Team Admin';

const open = {teamAdminRequestModalOpen: true};
const noTeam = {teams: {currentTeamId: '', teams: {}, myMembers: {}}};

test.describe('RequestTeamAdminModal', () => {
    test('renders nothing when closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestTeamAdminModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('names the current team', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await expect(page.locator('strong')).toHaveText('Team One');
    });

    test('without a team, says "this team" and blocks submit', async ({mount, page}) => {
        await mount(<StoreHarness
            entities={noTeam}
            pluginState={open}
                    ><RequestTeamAdminModal/></StoreHarness>);
        await expect(page.locator('strong')).toHaveText('this team');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeDisabled();
    });

    test('sends the team and shows the server message', async ({mount, page}) => {
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_team_admin_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toEqual({team_id: 'team1'});

        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('shows a default message when the server sends none', async ({mount, page}) => {
        await page.route(`${API}/submit_team_admin_request`, (route) => route.fulfill({contentType: 'application/json', body: '{}'}));
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Your Team Admin request has been submitted.')).toBeVisible();
    });

    test('shows the server error', async ({mount, page}) => {
        await page.route(`${API}/submit_team_admin_request`, (route) => route.fulfill({status: 403, contentType: 'application/json', body: JSON.stringify({error: 'You are already a Team Admin.'})}));
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('You are already a Team Admin.');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('shows a status error when the failure has no body', async ({mount, page}) => {
        await page.route(`${API}/submit_team_admin_request`, (route) => route.fulfill({status: 502, body: ''}));
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Request failed (502). Please try again.');
    });

    test('shows a connection error when the request fails', async ({mount, page}) => {
        await page.route(`${API}/submit_team_admin_request`, (route) => route.abort());
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Could not reach the server. Please check your connection and try again.');
    });

    test('Cancel closes', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('Escape closes', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Cancel'}).focus();
        await page.keyboard.press('Escape');
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('clicking the overlay closes, clicking the dialog does not', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('heading', {name: TITLE}).click();
        await expect(page.getByRole('heading', {name: TITLE})).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('a reopened form starts without the last result', async ({mount, page}) => {
        await page.route(`${API}/submit_team_admin_request`, (route) => route.fulfill({status: 403, contentType: 'application/json', body: JSON.stringify({error: 'You are already a Team Admin.'})}));
        await mount(<StoreHarness pluginState={open}><RequestTeamAdminModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('You are already a Team Admin.');
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open team admin'}).click();
        await expect(page.getByText(TITLE)).toBeVisible();
        await expect(page.getByRole('alert')).toHaveCount(0);
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });
});
