import React from 'react';

import {PrefixDefaultsEditor} from './PrefixDefaultsEditor';

import {expect, test} from '../playwright/ct-coverage';

type Change = [string, string];

const PLUGIN_ID = 'com.mattermostfederal.mattermost-plugin-request-hub';

// consoleState builds the admin console state the editor reads the
// sibling prefix list from.
function consoleState(prefixes: string) {
    return {PluginSettings: {Plugins: {[PLUGIN_ID]: {channelnameprefixes: prefixes}}}};
}

test.describe('PrefixDefaultsEditor', () => {
    test('asks for a prefix first when the list is empty', async ({mount, page}) => {
        await mount(<PrefixDefaultsEditor
            id='ChannelPrefixDefaults'
            value=''
            onChange={() => undefined}
            currentState={consoleState('')}
                    />);
        await expect(page.getByText('Add a channel prefix above first.', {exact: false})).toBeVisible();
    });

    test('lists every prefix from the unsaved prefix list with a summary', async ({mount, page}) => {
        await mount(<PrefixDefaultsEditor
            id='ChannelPrefixDefaults'
            value='{"sec-": {"channel_type": "P", "header": "Runbook", "members": ["alice", "bob"]}}'
            onChange={() => undefined}
            config={consoleState('team-|Team|16')}
            currentState={consoleState('team-|Team|16\nsec-|Security|32')}
                    />);
        await expect(page.getByTestId('prefix-defaults-team-')).toContainText('No defaults');
        await expect(page.getByTestId('prefix-defaults-sec-')).toContainText('always private, header, 2 default members');
    });

    test('edits a prefix and saves JSON', async ({mount, page}) => {
        const changes: Change[] = [];
        let saveNeeded = 0;
        await mount(<PrefixDefaultsEditor
            id='ChannelPrefixDefaults'
            value=''
            onChange={(id, v) => changes.push([id, v])}
            setSaveNeeded={() => {
                saveNeeded++;
            }}
            currentState={consoleState('sec-|Security|32')}
                    />);

        await page.getByRole('button', {name: /sec-/}).click();
        await page.getByLabel('Visibility').selectOption('P');
        await page.getByLabel('Channel header').fill('Runbook');
        const members = page.getByLabel('Default members', {exact: false});
        await members.fill('@Alice, bob');
        await members.blur();

        expect(changes.at(-1)).toEqual(['ChannelPrefixDefaults', '{"sec-":{"channel_type":"P","header":"Runbook","members":["alice","bob"]}}']);
        expect(saveNeeded).toBeGreaterThan(0);
        await expect(members).toHaveValue('alice, bob');
    });

    test('shows defaults for removed prefixes as unused and removes them', async ({mount, page}) => {
        const changes: Change[] = [];
        await mount(<PrefixDefaultsEditor
            id='ChannelPrefixDefaults'
            value='{"old-": {"header": "x"}, "sec-": {"header": "y"}}'
            onChange={(id, v) => changes.push([id, v])}
            currentState={consoleState('sec-|Security|32')}
                    />);

        const unused = page.getByTestId('prefix-defaults-unused-old-');
        await expect(unused).toContainText('Unused');
        await unused.getByRole('button', {name: 'Remove'}).click();
        await expect(unused).toBeHidden();
        expect(changes.at(-1)).toEqual(['ChannelPrefixDefaults', '{"sec-":{"header":"y"}}']);
    });
});
