import type {Page} from '@playwright/test';
import React from 'react';

import {MemberPicker} from './MemberPicker';

import {expect, test} from '../playwright/ct-coverage';

const AUTOCOMPLETE = '**/plugins/com.mattermostfederal.mattermost-plugin-request-hub/api/v1/user_autocomplete**';

const users = [
    {id: 'u1', username: 'alice', first_name: 'Alice', last_name: 'Adams'},
    {id: 'u2', username: 'albert', nickname: 'Al'},
    {id: 'u3', username: 'alex'},
];

async function routeUsers(page: Page, body: unknown = users, status = 200) {
    const queries: URLSearchParams[] = [];
    await page.route(AUTOCOMPLETE, (route) => {
        queries.push(new URL(route.request().url()).searchParams);
        return route.fulfill({status, contentType: 'application/json', body: JSON.stringify(body)});
    });
    return queries;
}

const PLACEHOLDER = 'Type a name...';
const input = (page: Page) => page.locator('input');
const pill = (page: Page, username: string) => page.locator('span', {hasText: `@${username}`}).filter({has: page.getByRole('button', {name: '×'})});

test.describe('MemberPicker', () => {
    test.describe('rendering', () => {
        test('shows the default placeholder and no pills', async ({mount, page}) => {
            await mount(<MemberPicker
                value=''
                onChange={() => undefined}
                        />);
            await expect(page.getByPlaceholder(PLACEHOLDER)).toBeVisible();
            await expect(page.getByRole('button', {name: '×'})).toHaveCount(0);
        });

        test('shows pills from the value and hides the placeholder', async ({mount, page}) => {
            await mount(<MemberPicker
                value='alice, @bob, , '
                placeholder='Pick people'
                onChange={() => undefined}
                        />);
            await expect(pill(page, 'alice')).toBeVisible();
            await expect(pill(page, 'bob')).toBeVisible();
            await expect(input(page)).toHaveAttribute('placeholder', '');
        });

        test('re-syncs pills when the value changes', async ({mount, page}) => {
            const component = await mount(<MemberPicker
                value='alice'
                onChange={() => undefined}
                                          />);
            await component.update(<MemberPicker
                value='bob'
                onChange={() => undefined}
                                   />);
            await expect(pill(page, 'bob')).toBeVisible();
            await expect(pill(page, 'alice')).toBeHidden();
        });

        test('disables the input and remove buttons', async ({mount, page}) => {
            await mount(<MemberPicker
                value='alice'
                disabled={true}
                onChange={() => undefined}
                        />);
            await expect(input(page)).toBeDisabled();
            await expect(page.getByRole('button', {name: '×'})).toBeDisabled();
        });
    });

    test.describe('autocomplete', () => {
        test('searches the team with the leading @ removed', async ({mount, page}) => {
            const queries = await routeUsers(page);
            await mount(<MemberPicker
                value=''
                teamId='team1'
                onChange={() => undefined}
                        />);
            await input(page).fill('@al');
            await expect(page.getByText('@alice')).toBeVisible();
            expect(queries[0].get('q')).toBe('al');
            expect(queries[0].get('team_id')).toBe('team1');
        });

        test('searches users outside a team with not_in_team_id', async ({mount, page}) => {
            const queries = await routeUsers(page);
            await mount(<MemberPicker
                value=''
                notInTeamId='team1'
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('@alice')).toBeVisible();
            expect(queries[0].get('not_in_team_id')).toBe('team1');
            expect(queries[0].has('team_id')).toBe(false);
        });

        test('does not search without exactly one scope', async ({mount, page}) => {
            const queries = await routeUsers(page);
            const component = await mount(<MemberPicker
                value=''
                onChange={() => undefined}
                                          />);
            await input(page).fill('al');
            await page.waitForTimeout(400);
            await component.update(<MemberPicker
                value=''
                teamId='team1'
                notInTeamId='team2'
                onChange={() => undefined}
                                   />);
            await input(page).fill('ale');
            await page.waitForTimeout(400);
            expect(queries).toHaveLength(0);
            await expect(page.getByText('@alice')).toHaveCount(0);
        });

        test('shows full name, then nickname, as secondary text', async ({mount, page}) => {
            await routeUsers(page);
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('Alice Adams')).toBeVisible();
            await expect(page.getByText('Al', {exact: true})).toBeVisible();
        });

        test('does not search for a blank or @-only query', async ({mount, page}) => {
            const queries = await routeUsers(page);
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill(' @ ');
            await page.waitForTimeout(400);
            expect(queries).toHaveLength(0);
        });

        test('leaves out users already picked', async ({mount, page}) => {
            await routeUsers(page);
            await mount(<MemberPicker
                teamId='team1'
                value='alice'
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('@albert')).toBeVisible();
            await expect(page.getByText('@alice')).toHaveCount(1);
        });

        test('shows nothing when the search fails', async ({mount, page}) => {
            await routeUsers(page, {error: 'nope'}, 500);
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await page.waitForTimeout(400);
            await expect(page.getByText('@alice')).toBeHidden();
        });

        test('shows nothing when the network fails', async ({mount, page}) => {
            await page.route(AUTOCOMPLETE, (route) => route.abort('connectionrefused'));
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await page.waitForTimeout(400);
            await expect(page.getByText('@alice')).toBeHidden();
        });

        test('ignores a response for an outdated query', async ({mount, page}) => {
            await page.route(AUTOCOMPLETE, async (route) => {
                const q = new URL(route.request().url()).searchParams.get('q');
                if (q === 'al') {
                    await new Promise((resolve) => setTimeout(resolve, 600));
                    await route.fulfill({contentType: 'application/json', body: JSON.stringify([users[2]])});
                    return;
                }
                await route.fulfill({contentType: 'application/json', body: JSON.stringify([users[0]])});
            });
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await page.waitForTimeout(300);
            await input(page).fill('ali');
            await expect(page.getByText('@alice')).toBeVisible();
            await page.waitForTimeout(600);
            await expect(page.getByText('@alex')).toBeHidden();
        });

        test('groups badged users above the rest', async ({mount, page}) => {
            await routeUsers(page);
            await mount(<MemberPicker
                teamId='team1'
                value=''
                usernameBadges={{albert: 'Channel Admin'}}
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('Current assignments')).toBeVisible();
            await expect(page.getByText('Available to add')).toBeVisible();
            await expect(page.getByText('Channel Admin')).toBeVisible();
            await expect(page.getByText(/^@al/)).toHaveText(['@albert', '@alice', '@alex']);
        });

        test('closes the dropdown on an outside click and reopens on click', async ({mount, page}) => {
            await routeUsers(page);
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('@alice')).toBeVisible();
            await page.mouse.click(600, 500);
            await expect(page.getByText('@alice')).toBeHidden();
            await input(page).click();
            await expect(page.getByText('@alice')).toBeVisible();
        });

        test('falls back to initials when the avatar fails to load', async ({mount, page}) => {
            await routeUsers(page);
            await page.route('**/api/v4/users/*/image**', (route) => route.fulfill({status: 404, body: ''}));
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('AA', {exact: true})).toBeVisible();
            await expect(page.getByText('A', {exact: true})).toHaveCount(2);
        });

        test('shows the avatar image when it loads', async ({mount, page}) => {
            await routeUsers(page, [users[0]]);
            const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=', 'base64');
            await page.route('**/api/v4/users/*/image**', (route) => route.fulfill({contentType: 'image/png', body: png}));
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await expect(page.locator('img')).toHaveAttribute('src', '/api/v4/users/u1/image?_=0');
            await expect(page.getByText('AA', {exact: true})).toBeHidden();
        });
    });

    test.describe('selecting', () => {
        test('adds a user on click', async ({mount, page}) => {
            await routeUsers(page);
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).fill('al');
            await page.getByText('@albert').click();
            await expect(pill(page, 'albert')).toBeVisible();
            await expect(input(page)).toHaveValue('');
            expect(changes).toEqual(['albert']);
        });

        test('adds the highlighted user with arrows and Enter', async ({mount, page}) => {
            await routeUsers(page);
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('@alice')).toBeVisible();
            await input(page).press('ArrowDown');
            await input(page).press('ArrowDown');
            await input(page).press('ArrowDown');
            await input(page).press('ArrowUp');
            await input(page).press('Enter');
            expect(changes).toEqual(['albert']);
        });

        test('ArrowUp stops at the first user and Tab adds it', async ({mount, page}) => {
            await routeUsers(page);
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('@alice')).toBeVisible();
            await input(page).press('ArrowUp');
            await input(page).press('Tab');
            expect(changes).toEqual(['alice']);
            await expect(input(page)).toBeFocused();
        });

        test('hovering a row highlights it for Enter', async ({mount, page}) => {
            await routeUsers(page);
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).fill('al');
            await page.getByText('@alex').hover();
            await input(page).press('Enter');
            expect(changes).toEqual(['alex']);
        });

        test('Enter keeps a typed name when there are no matches', async ({mount, page}) => {
            await routeUsers(page, []);
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value='alice'
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).fill('@carol');
            await input(page).press('Enter');
            expect(changes).toEqual(['alice, carol']);
        });

        test('Enter with an empty query adds nothing', async ({mount, page}) => {
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).press('Enter');
            expect(changes).toEqual([]);
        });

        test('does not add a user twice', async ({mount, page}) => {
            await routeUsers(page, []);
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value='alice'
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).fill('alice');
            await input(page).press('Enter');
            expect(changes).toEqual([]);
        });

        test('Escape closes the dropdown', async ({mount, page}) => {
            await routeUsers(page);
            await mount(<MemberPicker
                teamId='team1'
                value=''
                onChange={() => undefined}
                        />);
            await input(page).fill('al');
            await expect(page.getByText('@alice')).toBeVisible();
            await input(page).press('Escape');
            await expect(page.getByText('@alice')).toBeHidden();
        });
    });

    test.describe('removing', () => {
        test('removes a user with ×', async ({mount, page}) => {
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value='alice, bob'
                onChange={(v) => changes.push(v)}
                        />);
            await pill(page, 'alice').getByRole('button', {name: '×'}).click();
            await expect(pill(page, 'alice')).toBeHidden();
            expect(changes).toEqual(['bob']);
        });

        test('Backspace on an empty input removes the last user', async ({mount, page}) => {
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value='alice, bob'
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).focus();
            await input(page).press('Backspace');
            await expect(pill(page, 'bob')).toBeHidden();
            expect(changes).toEqual(['alice']);
        });

        test('Backspace while typing edits the query only', async ({mount, page}) => {
            await routeUsers(page, []);
            const changes: string[] = [];
            await mount(<MemberPicker
                teamId='team1'
                value='alice'
                onChange={(v) => changes.push(v)}
                        />);
            await input(page).fill('bo');
            await input(page).press('Backspace');
            await expect(input(page)).toHaveValue('b');
            await expect(pill(page, 'alice')).toBeVisible();
            expect(changes).toEqual([]);
        });
    });
});
