import type {Page} from '@playwright/test';
import React from 'react';

import {RequestTeamMemberModal} from './RequestTeamMemberModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const TITLE = 'Request Team Members';

const open = {teamMemberRequestModalOpen: true};
const noTeam = {teams: {currentTeamId: '', teams: {}, myMembers: {}}};

async function routeUsers(page: Page) {
    await page.route(`${API}/user_autocomplete**`, (route) => route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify([{id: 'u1', username: 'alice'}, {id: 'u2', username: 'bob'}]),
    }));
}

async function pick(page: Page, query: string, username: string) {
    await page.locator('label[for="rtm-nominees"] + div input').fill(query);
    await page.getByText(`@${username}`).click();
}

test.describe('RequestTeamMemberModal', () => {
    test('renders nothing when closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestTeamMemberModal/></StoreHarness>);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('names the current team', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await expect(page.locator('p strong').first()).toHaveText('Team One');
    });

    test('without a team, says "this team" and blocks submit', async ({mount, page}) => {
        await mount(<StoreHarness
            entities={noTeam}
            pluginState={open}
                    ><RequestTeamMemberModal/></StoreHarness>);
        await expect(page.locator('p strong').first()).toHaveText('this team');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeDisabled();
    });

    test('requires at least one person', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Pick at least one person to add to the team.');
    });

    test('sends the nominees and reason and shows the server message', async ({mount, page}) => {
        await routeUsers(page);
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_team_member_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent for approval.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await pick(page, 'ali', 'alice');
        await pick(page, 'bo', 'bob');
        await page.getByLabel('Reason (optional)').fill('  Joining the project.  ');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent for approval.')).toBeVisible();
        expect(sent).toEqual({team_id: 'team1', nominees: ['alice', 'bob'], reason: 'Joining the project.'});

        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('leaves out a blank reason and shows the default message', async ({mount, page}) => {
        await routeUsers(page);
        let sent: Record<string, unknown> = {};
        await page.route(`${API}/submit_team_member_request`, async (route) => {
            sent = route.request().postDataJSON();
            await route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await pick(page, 'ali', 'alice');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByText('Your team member request has been submitted.')).toBeVisible();
        expect(sent).toEqual({team_id: 'team1', nominees: ['alice']});
    });

    test('shows the server error', async ({mount, page}) => {
        await routeUsers(page);
        await page.route(`${API}/submit_team_member_request`, (route) => route.fulfill({status: 400, contentType: 'application/json', body: JSON.stringify({error: 'alice is already on this team.'})}));
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await pick(page, 'ali', 'alice');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('alice is already on this team.');
        await expect(page.getByRole('button', {name: 'Submit request'})).toBeEnabled();
    });

    test('shows a connection error when the request fails', async ({mount, page}) => {
        await routeUsers(page);
        await page.route(`${API}/submit_team_member_request`, (route) => route.abort());
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await pick(page, 'ali', 'alice');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Could not reach the server. Please check your connection and try again.');
    });

    test('Cancel closes', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('Escape closes', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await page.getByLabel('Reason (optional)').focus();
        await page.keyboard.press('Escape');
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('clicking the overlay closes, clicking the dialog does not', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await page.getByRole('heading', {name: TITLE}).click();
        await expect(page.getByRole('heading', {name: TITLE})).toBeVisible();
        await page.mouse.click(5, 5);
        await expect(page.getByText(TITLE)).toBeHidden();
    });

    test('a reopened form starts empty', async ({mount, page}) => {
        await routeUsers(page);
        await mount(<StoreHarness pluginState={open}><RequestTeamMemberModal/></StoreHarness>);
        await pick(page, 'ali', 'alice');
        await page.getByLabel('Reason (optional)').fill('Joining the project.');
        await page.getByRole('button', {name: 'Cancel'}).click();
        await expect(page.getByText(TITLE)).toBeHidden();

        await page.getByRole('button', {name: 'harness: open team member'}).click();
        await expect(page.getByText(TITLE)).toBeVisible();
        await expect(page.getByText('@alice')).toHaveCount(0);
        await expect(page.getByLabel('Reason (optional)')).toHaveValue('');
    });
});
