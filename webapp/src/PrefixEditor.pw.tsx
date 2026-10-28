import type {Page} from '@playwright/test';
import React from 'react';

import {PrefixEditor} from './PrefixEditor';

import {expect, test} from '../playwright/ct-coverage';

type Change = [string, string];

const prefixInputs = (page: Page) => page.getByPlaceholder('team-', {exact: true});
const descriptionInputs = (page: Page) => page.getByPlaceholder('Team collaboration channels');
const lengthInputs = (page: Page) => page.getByRole('spinbutton');
const preview = (page: Page) => page.locator('code');
const presetSelect = (page: Page) => page.locator('select').first();

test.describe('PrefixEditor', () => {
    test.describe('parsing the saved value', () => {
        test('shows the empty state and no preview for no prefixes', async ({mount, page}) => {
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value=''
                onChange={() => undefined}
                        />);
            await expect(page.getByText('No prefixes yet.', {exact: false})).toBeVisible();
            await expect(page.getByText('Live preview')).toBeHidden();
        });

        test('reads rows, skipping comments, blank lines and empty prefixes', async ({mount, page}) => {
            const value = [
                '# saved by an admin',
                '',
                'team-|Team channels|16',
                '|no prefix|10',
                'ops-',
            ].join('\n');
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value={value}
                onChange={() => undefined}
                        />);
            await expect(prefixInputs(page)).toHaveCount(2);
            await expect(prefixInputs(page).nth(0)).toHaveValue('team-');
            await expect(descriptionInputs(page).nth(0)).toHaveValue('Team channels');
            await expect(lengthInputs(page).nth(0)).toHaveValue('16');
            await expect(prefixInputs(page).nth(1)).toHaveValue('ops-');
            await expect(lengthInputs(page).nth(1)).toHaveValue('48');
        });

        test('reads the upper bound from a regex and clamps lengths', async ({mount, page}) => {
            const value = [
                'a-||^[a-z0-9-]{2, 10}$',
                'b-||[a-z]+',
                'c-||99',
                'd-||1',
            ].join('\n');
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value={value}
                onChange={() => undefined}
                        />);
            await expect(lengthInputs(page)).toHaveCount(4);
            const values = await lengthInputs(page).evaluateAll((els) => els.map((el) => (el as HTMLInputElement).value));
            expect(values).toEqual(['10', '48', '48', '2']);
        });

        test('re-reads rows when the value changes', async ({mount, page}) => {
            const component = await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value='team-'
                onChange={() => undefined}
                                          />);
            await component.update(<PrefixEditor
                id='ChannelNamePrefixes'
                value='ops-'
                onChange={() => undefined}
                                   />);
            await expect(prefixInputs(page)).toHaveCount(1);
            await expect(prefixInputs(page).first()).toHaveValue('ops-');
        });
    });

    test.describe('editing rows', () => {
        test('adding a row marks the setting changed', async ({mount, page}) => {
            const changes: Change[] = [];
            let saveNeeded = 0;
            await mount(
                <PrefixEditor
                    id='ChannelNamePrefixes'
                    value='team-|Team|16'
                    onChange={(id, v) => changes.push([id, v])}
                    setSaveNeeded={() => {
                        saveNeeded++;
                    }}
                />,
            );
            await page.getByRole('button', {name: '+ Add prefix'}).click();
            await expect(prefixInputs(page)).toHaveCount(2);
            expect(changes).toEqual([['ChannelNamePrefixes', 'team-|Team|16']]);
            expect(saveNeeded).toBe(1);
        });

        test('editing fields serializes trimmed values', async ({mount, page}) => {
            const changes: Change[] = [];
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value=''
                onChange={(id, v) => changes.push([id, v])}
                        />);
            await page.getByRole('button', {name: '+ Add prefix'}).click();
            await prefixInputs(page).first().fill(' ops- ');
            await descriptionInputs(page).first().fill(' Operations ');
            await lengthInputs(page).first().fill('12');
            expect(changes.at(-1)).toEqual(['ChannelNamePrefixes', 'ops-|Operations|12']);
        });

        test('clamps the max length', async ({mount, page}) => {
            const changes: Change[] = [];
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value='ops-||12'
                onChange={(id, v) => changes.push([id, v])}
                        />);
            await lengthInputs(page).first().fill('99');
            expect(changes.at(-1)?.[1]).toBe('ops-||48');
            await lengthInputs(page).first().fill('');
            expect(changes.at(-1)?.[1]).toBe('ops-||2');
        });

        test('moves and deletes rows', async ({mount, page}) => {
            const changes: Change[] = [];
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value={'a-||10\nb-||10\nc-||10'}
                onChange={(id, v) => changes.push([id, v])}
                        />);

            await expect(page.getByTitle('Move up').first()).toBeDisabled();
            await expect(page.getByTitle('Move down').last()).toBeDisabled();

            await page.getByTitle('Move down').first().click();
            expect(changes.at(-1)?.[1]).toBe('b-||10\na-||10\nc-||10');
            await page.getByTitle('Move up').last().click();
            expect(changes.at(-1)?.[1]).toBe('b-||10\nc-||10\na-||10');
            await page.getByTitle('Delete').first().click();
            expect(changes.at(-1)?.[1]).toBe('c-||10\na-||10');
        });

        test('disables every control', async ({mount, page}) => {
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value='team-'
                disabled={true}
                onChange={() => undefined}
                        />);
            await expect(prefixInputs(page).first()).toBeDisabled();
            await expect(descriptionInputs(page).first()).toBeDisabled();
            await expect(lengthInputs(page).first()).toBeDisabled();
            await expect(page.getByTitle('Delete')).toBeDisabled();
            await expect(page.getByRole('button', {name: '+ Add prefix'})).toBeDisabled();
            await expect(presetSelect(page)).toBeDisabled();
        });
    });

    test.describe('presets', () => {
        test('adds a preset pack', async ({mount, page}) => {
            const changes: Change[] = [];
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value=''
                onChange={(id, v) => changes.push([id, v])}
                        />);
            await presetSelect(page).selectOption('Support / Success');
            expect(changes.at(-1)?.[1]).toBe([
                'help-|Customer help channels|48',
                'onboard-|Customer onboarding|48',
                'vip-|Enterprise account channels|48',
            ].join('\n'));
            await expect(presetSelect(page)).toHaveValue('');
        });

        test('updates matching prefixes and keeps the others', async ({mount, page}) => {
            const changes: Change[] = [];
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value={'mine-|Custom|8\nOPS-|Old|16'}
                onChange={(id, v) => changes.push([id, v])}
                        />);
            await presetSelect(page).selectOption('SRE / Ops');
            expect(changes.at(-1)?.[1]).toBe([
                'mine-|Custom|8',
                'OPS-|Operational channels|48',
                'incident-|Incident response|48',
                'post-|Post-mortem channels|48',
                'runbook-|Runbook discussion|48',
            ].join('\n'));

            await presetSelect(page).selectOption('SRE / Ops');
            expect(changes.at(-1)?.[1]).toBe(changes.at(-2)?.[1]);
        });
    });

    test.describe('live preview', () => {
        test('shows an ok result for the default input', async ({mount, page}) => {
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value='team-||24'
                onChange={() => undefined}
                        />);
            await expect(preview(page)).toHaveText('team-marketing-team  ✓ ok (14/24 chars)');
        });

        test('flags a suffix that is too long or too short', async ({mount, page}) => {
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value='team-||4'
                onChange={() => undefined}
                        />);
            await expect(preview(page)).toHaveText('team-marketing-team  ✗ suffix is 14 chars, exceeds max 4');
            await page.getByPlaceholder('marketing team').fill('x');
            await expect(preview(page)).toHaveText('team-x  ✗ suffix must be at least 2 chars');
        });

        test('asks for input when the suffix slugifies to nothing', async ({mount, page}) => {
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value='team-'
                onChange={() => undefined}
                        />);
            await page.getByPlaceholder('marketing team').fill('!!!');
            await expect(preview(page)).toHaveText('team-<suffix> — type something above to preview');
        });

        test('follows the chosen prefix and a removed one', async ({mount, page}) => {
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value={'team-||24\nops-||24'}
                onChange={() => undefined}
                        />);
            await page.locator('select').nth(1).selectOption('ops-');
            await expect(preview(page)).toContainText('ops-marketing-team');
            await page.getByTitle('Delete').last().click();
            await expect(preview(page)).toContainText('team-marketing-team');
        });

        test('waits for a prefix on a new blank row', async ({mount, page}) => {
            await mount(<PrefixEditor
                id='ChannelNamePrefixes'
                value=''
                onChange={() => undefined}
                        />);
            await page.getByRole('button', {name: '+ Add prefix'}).click();
            await expect(preview(page)).toHaveText('(add a prefix to see a preview)');
        });
    });
});
