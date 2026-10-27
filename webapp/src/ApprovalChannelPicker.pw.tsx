import type {Page} from '@playwright/test';
import React from 'react';

import {ChannelPicker, TeamPicker} from './ApprovalChannelPicker';

import {expect, test} from '../playwright/ct-coverage';

const API = '**/plugins/com.mattermostfederal.mattermost-plugin-request-hub/api/v1';
const PLUGIN_ID = 'com.mattermostfederal.mattermost-plugin-request-hub';

type Change = [string, string];

const teams = [
    {id: 't1', name: 'ops', display_name: 'Operations'},
    {id: 't2', name: 'eng', display_name: ''},
];

const channelsByTeam: Record<string, unknown[]> = {
    t1: [
        {id: 'c1', name: 'approvals', display_name: 'Approvals', type: 'O'},
        {id: 'c2', name: 'leads', display_name: '', type: 'P'},
    ],
    t2: [{id: 'c3', name: 'eng-approvals', display_name: 'Eng Approvals', type: 'O'}],
};

function settings(values: Record<string, string>) {
    return {PluginSettings: {Plugins: {[PLUGIN_ID]: values}}};
}

async function routeTeams(page: Page, list: unknown[] = teams) {
    await page.route(`${API}/teams`, (route) => route.fulfill({contentType: 'application/json', body: JSON.stringify(list)}));
}

// routeChannels also serves the team list, since ChannelPicker resolves the
// saved team name to an ID before loading that team's channels.
async function routeChannels(page: Page, delays: Record<string, number> = {}, teamList: unknown[] = teams) {
    await routeTeams(page, teamList);
    await page.route(`${API}/channels**`, async (route) => {
        const team = new URL(route.request().url()).searchParams.get('team_id') ?? '';
        if (delays[team]) {
            await new Promise((resolve) => setTimeout(resolve, delays[team]));
        }
        await route.fulfill({contentType: 'application/json', body: JSON.stringify(channelsByTeam[team] ?? [])});
    });
}

const options = (page: Page) => page.locator('option');

test.describe('TeamPicker', () => {
    test('lists teams by name once loaded', async ({mount, page}) => {
        let release: () => void = () => undefined;
        const gate = new Promise<void>((resolve) => {
            release = resolve;
        });
        await page.route(`${API}/teams`, async (route) => {
            await gate;
            await route.fulfill({contentType: 'application/json', body: JSON.stringify(teams)});
        });
        await mount(<TeamPicker
            id='ApprovalTeam'
            value='ops'
            onChange={() => undefined}
                    />);
        await expect(page.locator('select')).toBeDisabled();
        await expect(options(page).first()).toHaveText('-- loading teams --');
        release();

        await expect(options(page)).toHaveText(['-- pick a team --', 'Operations  (/ops)', 'eng  (/eng)']);
        await expect(page.locator('select')).toHaveValue('ops');
        await expect(page.locator('select')).toBeEnabled();
    });

    test('writes the picked team name', async ({mount, page}) => {
        await page.route(`${API}/teams`, (route) => route.fulfill({contentType: 'application/json', body: JSON.stringify(teams)}));
        const changes: Change[] = [];
        let saveNeeded = 0;
        await mount(
            <TeamPicker
                id='ApprovalTeam'
                value=''
                onChange={(id, v) => changes.push([id, v])}
                setSaveNeeded={() => {
                    saveNeeded++;
                }}
            />,
        );
        await page.locator('select').selectOption('eng');
        expect(changes).toEqual([['ApprovalTeam', 'eng']]);
        expect(saveNeeded).toBe(1);
    });

    test('shows no teams when the list fails to load', async ({mount, page}) => {
        await page.route(`${API}/teams`, (route) => route.fulfill({status: 500, body: ''}));
        await mount(<TeamPicker
            id='ApprovalTeam'
            value=''
            onChange={() => undefined}
                    />);
        await expect(options(page)).toHaveText(['-- pick a team --']);
    });

    test('shows no teams when the request fails', async ({mount, page}) => {
        await page.route(`${API}/teams`, (route) => route.abort('connectionrefused'));
        await mount(<TeamPicker
            id='ApprovalTeam'
            value=''
            onChange={() => undefined}
                    />);
        await expect(options(page)).toHaveText(['-- pick a team --']);
    });

    test('honors disabled', async ({mount, page}) => {
        await page.route(`${API}/teams`, (route) => route.fulfill({contentType: 'application/json', body: JSON.stringify(teams)}));
        await mount(<TeamPicker
            id='ApprovalTeam'
            value=''
            disabled={true}
            onChange={() => undefined}
                    />);
        await expect(options(page)).toHaveCount(3);
        await expect(page.locator('select')).toBeDisabled();
    });
});

test.describe('ChannelPicker', () => {
    test('asks for a team first', async ({mount, page}) => {
        await mount(<ChannelPicker
            id='ApprovalChannel'
            value=''
            onChange={() => undefined}
                    />);
        await expect(options(page)).toHaveText(['-- pick a team first --']);
        await expect(page.locator('select')).toBeDisabled();
    });

    test('lists the saved team’s channels and marks private ones', async ({mount, page}) => {
        await routeChannels(page);
        await mount(<ChannelPicker
            id='ApprovalChannel'
            value='approvals'
            config={settings({ApprovalTeam: 'ops'})}
            onChange={() => undefined}
                    />);
        await expect(options(page)).toHaveText(['-- pick a channel --', 'Approvals  (/approvals)', '🔒 leads  (/leads)']);
        await expect(page.locator('select')).toHaveValue('approvals');
    });

    test('prefers the unsaved team and reads a lowercase key', async ({mount, page}) => {
        await routeChannels(page);
        await mount(
            <ChannelPicker
                id='ApprovalChannel'
                value=''
                config={settings({ApprovalTeam: 'ops'})}
                currentState={settings({approvalteam: 'eng'})}
                onChange={() => undefined}
            />,
        );
        await expect(options(page)).toHaveText(['-- pick a channel --', 'Eng Approvals  (/eng-approvals)']);
    });

    test('falls back to the saved team when the unsaved state has none', async ({mount, page}) => {
        await routeChannels(page);
        await mount(
            <ChannelPicker
                id='ApprovalChannel'
                value=''
                config={settings({ApprovalTeam: 'ops'})}
                currentState={{PluginSettings: {Plugins: {}}}}
                onChange={() => undefined}
            />,
        );
        await expect(options(page)).toHaveCount(3);
    });

    test('ignores a non-string team setting', async ({mount, page}) => {
        await mount(<ChannelPicker
            id='ApprovalChannel'
            value=''
            config={{PluginSettings: {Plugins: {[PLUGIN_ID]: {ApprovalTeam: 5}}}}}
            onChange={() => undefined}
                    />);
        await expect(options(page)).toHaveText(['-- pick a team first --']);
    });

    test('shows loading, then an empty list', async ({mount, page}) => {
        await routeChannels(page, {t9: 300}, [...teams, {id: 't9', name: 'empty', display_name: 'Empty'}]);
        await mount(<ChannelPicker
            id='ApprovalChannel'
            value=''
            config={settings({ApprovalTeam: 'empty'})}
            onChange={() => undefined}
                    />);
        await expect(options(page).first()).toHaveText('-- loading channels --');
        await expect(options(page).first()).toHaveText('-- no channels visible --');
    });

    test('shows an empty list when channels fail to load', async ({mount, page}) => {
        await routeTeams(page);
        await page.route(`${API}/channels**`, (route) => route.fulfill({status: 403, body: ''}));
        await mount(<ChannelPicker
            id='ApprovalChannel'
            value=''
            config={settings({ApprovalTeam: 'ops'})}
            onChange={() => undefined}
                    />);
        await expect(options(page).first()).toHaveText('-- no channels visible --');
    });

    test('shows an empty list when the request fails', async ({mount, page}) => {
        await routeTeams(page);
        await page.route(`${API}/channels**`, (route) => route.abort('connectionrefused'));
        await mount(<ChannelPicker
            id='ApprovalChannel'
            value=''
            config={settings({ApprovalTeam: 'ops'})}
            onChange={() => undefined}
                    />);
        await expect(options(page).first()).toHaveText('-- no channels visible --');
    });

    test('writes the picked channel name', async ({mount, page}) => {
        await routeChannels(page);
        const changes: Change[] = [];
        let saveNeeded = 0;
        await mount(
            <ChannelPicker
                id='ApprovalChannel'
                value=''
                config={settings({ApprovalTeam: 'ops'})}
                onChange={(id, v) => changes.push([id, v])}
                setSaveNeeded={() => {
                    saveNeeded++;
                }}
            />,
        );
        await page.locator('select').selectOption('leads');
        expect(changes).toEqual([['ApprovalChannel', 'leads']]);
        expect(saveNeeded).toBe(1);
    });

    test('keeps the saved channel on first load', async ({mount, page}) => {
        await routeChannels(page);
        const changes: Change[] = [];
        await mount(<ChannelPicker
            id='ApprovalChannel'
            value='gone'
            config={settings({ApprovalTeam: 'ops'})}
            onChange={(id, v) => changes.push([id, v])}
                    />);
        await expect(options(page)).toHaveCount(3);
        expect(changes).toEqual([]);
    });

    test('clears a channel from the previous team', async ({mount, page}) => {
        await routeChannels(page);
        const changes: Change[] = [];
        let saveNeeded = 0;
        const onChange = (id: string, v: string) => changes.push([id, v]);
        const setSaveNeeded = () => {
            saveNeeded++;
        };
        const component = await mount(<ChannelPicker
            id='ApprovalChannel'
            value='approvals'
            currentState={settings({ApprovalTeam: 'ops'})}
            onChange={onChange}
            setSaveNeeded={setSaveNeeded}
                                      />);
        await expect(options(page)).toHaveCount(3);

        await component.update(<ChannelPicker
            id='ApprovalChannel'
            value='approvals'
            currentState={settings({ApprovalTeam: 'eng'})}
            onChange={onChange}
            setSaveNeeded={setSaveNeeded}
                               />);
        await expect(options(page)).toHaveCount(2);
        expect(changes).toEqual([['ApprovalChannel', '']]);
        expect(saveNeeded).toBe(1);
    });

    test('keeps a channel that the new team also has', async ({mount, page}) => {
        channelsByTeam.t3 = [{id: 'c1', name: 'approvals', display_name: 'Approvals', type: 'O'}];
        await routeChannels(page, {}, [...teams, {id: 't3', name: 'ops2', display_name: 'Ops 2'}]);
        const changes: Change[] = [];
        const onChange = (id: string, v: string) => changes.push([id, v]);
        const component = await mount(<ChannelPicker
            id='ApprovalChannel'
            value='approvals'
            currentState={settings({ApprovalTeam: 'ops'})}
            onChange={onChange}
                                      />);
        await expect(options(page)).toHaveCount(3);
        await component.update(<ChannelPicker
            id='ApprovalChannel'
            value='approvals'
            currentState={settings({ApprovalTeam: 'ops2'})}
            onChange={onChange}
                               />);
        await expect(options(page)).toHaveCount(2);
        expect(changes).toEqual([]);
        delete channelsByTeam.t3;
    });

    test('clears the channel when the team is removed', async ({mount, page}) => {
        await routeChannels(page);
        const changes: Change[] = [];
        const onChange = (id: string, v: string) => changes.push([id, v]);
        const component = await mount(<ChannelPicker
            id='ApprovalChannel'
            value='approvals'
            currentState={settings({ApprovalTeam: 'ops'})}
            onChange={onChange}
                                      />);
        await expect(options(page)).toHaveCount(3);
        await component.update(<ChannelPicker
            id='ApprovalChannel'
            value='approvals'
            currentState={settings({ApprovalTeam: ''})}
            onChange={onChange}
                               />);
        await expect(options(page)).toHaveText(['-- pick a team first --']);
        expect(changes).toEqual([['ApprovalChannel', '']]);
    });

    test('ignores a slow response for a team that is no longer selected', async ({mount, page}) => {
        await routeChannels(page, {t1: 600});
        const component = await mount(<ChannelPicker
            id='ApprovalChannel'
            value=''
            currentState={settings({ApprovalTeam: 'ops'})}
            onChange={() => undefined}
                                      />);
        await component.update(<ChannelPicker
            id='ApprovalChannel'
            value=''
            currentState={settings({ApprovalTeam: 'eng'})}
            onChange={() => undefined}
                               />);
        await expect(options(page)).toHaveText(['-- pick a channel --', 'Eng Approvals  (/eng-approvals)']);
        await page.waitForTimeout(800);
        await expect(options(page)).toHaveCount(2);
    });
});
