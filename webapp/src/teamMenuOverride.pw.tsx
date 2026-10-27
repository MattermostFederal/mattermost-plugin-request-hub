import type {Page} from '@playwright/test';
import React from 'react';

import {DomInjectorHarness} from './DomInjectorHarness';
import {OPEN_TEAM_ADMIN_REQUEST_MODAL, OPEN_TEAM_CREATION_MODAL} from './store';

import {expect, test} from '../playwright/ct-coverage';
import {
    ALL_ON,
    API,
    configLoaded,
    countConfigLoads,
    frame,
    harnessActions as actions,
    harnessInstalled,
    routeConfig,
} from '../playwright/ct-helpers';

function stateFor({systemRoles = 'system_user', teamRoles = 'team_user', schemeAdmin = false} = {}) {
    return {
        entities: {
            users: {currentUserId: 'me', profiles: {me: {roles: systemRoles}}},
            teams: {
                currentTeamId: 'team1',
                teams: {},
                myMembers: {team1: {roles: teamRoles, scheme_admin: schemeAdmin}},
            },
        },
    };
}

const member = stateFor();
const sysadmin = stateFor({systemRoles: 'system_user system_admin'});

const menuItems = (labels: string[]) => labels.map((l) => `<li><button><i class="icon"></i><span>${l}</span></button></li>`).join('');

const teamMenu = (labels: string[]) => `<ul role="menu" id="teamMenu">${menuItems(labels)}</ul>`;

const withCreate = teamMenu(['Invite people', 'Team settings', 'Manage members', 'Join another team', 'Create a team', 'Leave team', 'Learn about teams']);

// recordNative counts clicks that reach a bubble-phase document listener,
// i.e. clicks the override let through to the native handler.
async function recordNative(page: Page) {
    await page.evaluate(() => document.addEventListener('click', (e) => {
        e.preventDefault();
        const w = window as unknown as {nativeClicks?: number};
        w.nativeClicks = (w.nativeClicks ?? 0) + 1;
    }));
}

async function nativeClicks(page: Page) {
    return page.evaluate(() => (window as unknown as {nativeClicks?: number}).nativeClicks ?? 0);
}

async function dispose(page: Page) {
    await page.evaluate(() => window.__harness!.dispose());
}

async function addMarkup(page: Page, html: string) {
    await page.evaluate((h) => window.__harness!.addMarkup(h), html);
}

test.describe('installTeamMenuOverride', () => {
    test.describe('Create a team', () => {
        test('is relabeled for a member and opens the Request a Team modal', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await recordNative(page);

            const item = page.getByRole('button', {name: 'Request a Team'});
            await expect(item).toBeVisible();
            await expect(page.getByText('Create a team')).toHaveCount(0);

            await item.click();
            expect(await actions(page)).toEqual([OPEN_TEAM_CREATION_MODAL]);
            expect(await nativeClicks(page)).toBe(0);
        });

        test('keeps the native flow for a System Admin', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={sysadmin}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await recordNative(page);

            await page.getByRole('button', {name: 'Create a team'}).click();
            expect(await actions(page)).toEqual([]);
            expect(await nativeClicks(page)).toBe(1);
            await expect(page.getByText('Request Team Admin')).toHaveCount(0);
        });

        test('keeps the native flow when team requests are off', async ({mount, page}) => {
            await routeConfig(page, {team: false});
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await recordNative(page);

            await page.getByRole('button', {name: 'Request a Team'}).click();
            expect(await actions(page)).toEqual([]);
            expect(await nativeClicks(page)).toBe(1);
        });

        test('lets a click through before the settings load', async ({mount, page}) => {
            let release: () => void = () => undefined;
            const held = new Promise<void>((r) => {
                release = r;
            });
            await countConfigLoads(page);
            await page.route(`${API}/config`, async (route) => {
                await held;
                await route.fulfill({contentType: 'application/json', body: JSON.stringify(ALL_ON)});
            });
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={member}
                        />);
            await harnessInstalled(page);
            await recordNative(page);

            await page.getByRole('button', {name: 'Request a Team'}).click();
            expect(await actions(page)).toEqual([]);
            expect(await nativeClicks(page)).toBe(1);
            release();
        });

        test('marks an item whose label is split across nodes', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup='<div role="menu"><button>Invite people</button><button>Leave team</button><button>Create <b>a team</b></button></div>'
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);

            await page.getByRole('button', {name: 'Create a team'}).click();
            expect(await actions(page)).toEqual([OPEN_TEAM_CREATION_MODAL]);
        });

        test('ignores clicks elsewhere', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);

            await page.getByRole('button', {name: 'Invite people'}).click();
            expect(await actions(page)).toEqual([]);
        });
    });

    test.describe('Request a Team injected', () => {
        for (const [name, labels, after] of [
            ['after Join another team', ['Invite people', 'Manage members', 'Join another team', 'Leave team'], 'Join another team'],
            ['as the last item after Join another team', ['Invite people', 'Manage members', 'Join another team'], 'Join another team'],
            ['before Learn about teams', ['Invite people', 'Manage members', 'Learn about teams'], 'Manage members'],
            ['after Leave team', ['Invite people', 'Manage members', 'Leave team', 'Team settings'], 'Leave team'],
            ['as the last item after Leave team', ['Invite people', 'Manage members', 'Leave team'], 'Leave team'],
        ] as const) {
            test(`when the native item is missing, ${name}`, async ({mount, page}) => {
                await routeConfig(page);
                await mount(<DomInjectorHarness
                    injector='teamMenu'
                    markup={teamMenu([...labels])}
                    state={member}
                            />);
                await harnessInstalled(page);
                const texts = await page.locator('#teamMenu > li').allTextContents();
                const at = texts.indexOf('Request a Team');
                expect(at).toBeGreaterThan(0);
                const before = texts[at - 1] === 'Request Team Admin' ? texts[at - 2] : texts[at - 1];
                expect(before).toBe(after);
            });
        }

        test('opens the Request a Team modal, or nothing when team requests are off', async ({mount, page}) => {
            let team = true;
            await countConfigLoads(page);
            await page.route(`${API}/config`, (route) => route.fulfill({
                contentType: 'application/json',
                body: JSON.stringify({...ALL_ON, team}),
            }));
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={teamMenu(['Invite people', 'Manage members', 'Leave team'])}
                state={member}
                        />);
            await harnessInstalled(page);
            const item = page.getByTestId('teamMenuRequestTeam');
            await item.click();
            await expect.poll(() => actions(page)).toEqual([OPEN_TEAM_CREATION_MODAL]);

            // One load at install and one per click.
            team = false;
            await item.click();
            await configLoaded(page, 3);
            expect(await actions(page)).toEqual([OPEN_TEAM_CREATION_MODAL]);
        });

        test('rebuilds the label when the template has no single text node', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup='<div role="menu"><a href="/x">Invite people</a><a href="/y"><i class="icon"></i>Leave <b>team</b></a><a href="/z">Leave team</a></div>'
                state={member}
                        />);
            await harnessInstalled(page);
            await expect(page.getByTestId('teamMenuRequestTeam')).toHaveText(/Request a Team/);
            await expect(page.getByTestId('teamMenuRequestTeam')).toHaveAttribute('href', '#');
        });

        test('is not added twice', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={teamMenu(['Invite people', 'Manage members', 'Leave team'])}
                state={member}
                        />);
            await harnessInstalled(page);
            await addMarkup(page, '<p>more</p>');
            await frame(page);
            await expect(page.getByTestId('teamMenuRequestTeam')).toHaveCount(1);
        });
    });

    test.describe('Request Team Admin', () => {
        test('is injected below Manage members and opens the modal', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={member}
                        />);
            await harnessInstalled(page);
            const texts = await page.locator('#teamMenu > li').allTextContents();
            expect(texts[texts.indexOf('Manage members') + 1]).toBe('Request Team Admin');

            await page.getByTestId('teamMenuRequestTeamAdmin').click();
            await expect.poll(() => actions(page)).toEqual([OPEN_TEAM_ADMIN_REQUEST_MODAL]);
        });

        test('is injected below View members, as the last item', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={teamMenu(['Invite people', 'Leave team', 'View members'])}
                state={member}
                        />);
            await harnessInstalled(page);
            const texts = await page.locator('#teamMenu > li').allTextContents();
            expect(texts.at(-1)).toBe('Request Team Admin');
        });

        test('rebuilds the label when Manage members is split across nodes', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup='<div role="menu"><a href="/x">Invite people</a><a href="/y">Leave team</a><a href="/m">Manage members</a></div>'
                state={member}
                        />);
            await harnessInstalled(page);
            await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveAttribute('href', '#');
        });

        test('is not shown to a Team Admin or a scheme admin', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={stateFor({teamRoles: 'team_user team_admin'})}
                        />);
            await harnessInstalled(page);
            await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(0);
        });

        test('is removed after a promotion', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={member}
                        />);
            await harnessInstalled(page);
            await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(1);

            await page.evaluate((s) => window.__harness!.setState(s), stateFor({schemeAdmin: true}));
            await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(0);
        });

        test('is removed when Team Admin requests are off and does nothing on click', async ({mount, page}) => {
            await routeConfig(page, {team_admin: false});
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(0);
        });

        test('is not added to a menu opened after the settings load with Team Admin requests off', async ({mount, page}) => {
            await routeConfig(page, {team_admin: false});
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup='<div>closed</div>'
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);

            await addMarkup(page, withCreate);
            await expect(page.getByRole('button', {name: 'Request a Team'})).toBeVisible();
            await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(0);
        });

        test('does nothing on click when Team Admin requests turn off later', async ({mount, page}) => {
            let teamAdmin = true;
            await countConfigLoads(page);
            await page.route(`${API}/config`, (route) => route.fulfill({
                contentType: 'application/json',
                body: JSON.stringify({...ALL_ON, team_admin: teamAdmin}),
            }));
            await mount(<DomInjectorHarness
                injector='teamMenu'
                markup={withCreate}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            teamAdmin = false;
            await page.getByTestId('teamMenuRequestTeamAdmin').click();
            await configLoaded(page, 2);
            expect(await actions(page)).toEqual([]);
        });
    });

    test('picks up a menu that opens after install', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamMenu'
            markup='<div>loading</div>'
            state={member}
                    />);
        await harnessInstalled(page);
        await addMarkup(page, withCreate);
        await expect(page.getByRole('button', {name: 'Request a Team'})).toBeVisible();
        await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(1);
    });

    test('warns when no team menu is found within 10 seconds', async ({mount, page}) => {
        await page.clock.install();
        await routeConfig(page);
        const warnings: string[] = [];
        page.on('console', (msg) => {
            if (msg.type() === 'warning') {
                warnings.push(msg.text());
            }
        });
        await mount(<DomInjectorHarness
            injector='teamMenu'
            markup='<div>no menu here</div>'
            state={member}
                    />);
        await harnessInstalled(page);
        await page.clock.runFor(10000);
        await expect.poll(() => warnings.some((w) => w.includes('Team sidebar dropdown not found'))).toBe(true);
    });

    test('dispose restores the menu and stops intercepting', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamMenu'
            markup={withCreate}
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(1);

        await dispose(page);
        await recordNative(page);

        await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(0);
        await expect(page.locator('[data-mm-plugin-create-team-intercepted]')).toHaveCount(0);
        await page.getByRole('button', {name: 'Create a team'}).click();
        expect(await actions(page)).toEqual([]);
        expect(await nativeClicks(page)).toBe(1);

        // A later menu render is left alone.
        await addMarkup(page, teamMenu(['Invite people', 'Manage members', 'Leave team']));
        await frame(page);
        await expect(page.getByTestId('teamMenuRequestTeam')).toHaveCount(0);
    });

    test('dispose removes an injected Request a Team', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamMenu'
            markup={teamMenu(['Invite people', 'Manage members', 'Leave team'])}
            state={member}
                    />);
        await harnessInstalled(page);
        await expect(page.getByTestId('teamMenuRequestTeam')).toHaveCount(1);
        await dispose(page);
        await expect(page.getByTestId('teamMenuRequestTeam')).toHaveCount(0);
    });
});

test.describe('installTeamMenuOverride edge cases', () => {
    test('leaves a menu with only one team label alone', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamMenu'
            markup='<div role="menu"><button>Invite people</button><button>Create a team</button></div>'
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);

        // Control: a second team label makes it the team menu.
        await addMarkup(page, '<div role="menu" id="real"><button>Invite people</button><button>Leave team</button><button>Create a team</button></div>');
        await expect(page.locator('#real').getByRole('button', {name: 'Request a Team'})).toBeVisible();
        await page.locator('#real').evaluate((el) => el.remove());

        await recordNative(page);
        await expect(page.locator('[data-mm-plugin-create-team-intercepted]')).toHaveCount(0);
        await page.getByRole('button', {name: 'Create a team'}).click();
        expect(await actions(page)).toEqual([]);
        expect(await nativeClicks(page)).toBe(1);
    });

    test('adds no Request Team Admin without a members item', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamMenu'
            markup={teamMenu(['Invite people', 'Leave team', 'Create a team'])}
            state={member}
                    />);
        await harnessInstalled(page);

        // The menu was processed: Create a team is relabeled.
        await expect(page.getByRole('button', {name: 'Request a Team'})).toBeVisible();
        await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(0);
    });

    test('dispose unmarks an item whose label was never rewritten', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamMenu'
            markup='<div role="menu"><button>Invite people</button><button>Leave team</button><button>Create <b>a team</b></button></div>'
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await expect(page.locator('[data-mm-plugin-create-team-intercepted]')).toHaveCount(1);

        await dispose(page);
        await recordNative(page);
        await expect(page.locator('[data-mm-plugin-create-team-intercepted]')).toHaveCount(0);
        await page.getByRole('button', {name: 'Create a team'}).click();
        expect(await actions(page)).toEqual([]);
        expect(await nativeClicks(page)).toBe(1);
    });

    test('dispose cancels a menu update already scheduled', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamMenu'
            markup='<div>loading</div>'
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);

        // The observer schedules a frame for the new menu; dispose runs before it.
        await page.evaluate(async (html) => {
            window.__harness!.addMarkup(html);
            await Promise.resolve();
            window.__harness!.dispose();
        }, withCreate);
        await frame(page);

        await expect(page.getByRole('button', {name: 'Create a team'})).toBeVisible();
        await expect(page.locator('[data-mm-plugin-create-team-intercepted]')).toHaveCount(0);
        await expect(page.getByTestId('teamMenuRequestTeamAdmin')).toHaveCount(0);
    });
});

const rail = (create: boolean) => `
<div class="team-sidebar">
  <div id="teamSidebarWrapper">
    <div class="team-container"><a href="#" id="team1Button">T1</a></div>
    ${create ? '<div class="team-container"><a id="create_teamTeamButton" href="/create_team"><div class="team-btn team-btn__add">+</div></a></div>' : ''}
    <div class="team-container"><a id="select_teamTeamButton" href="/select_team">Join</a></div>
  </div>
</div>`;

test.describe('installTeamSidebarAddOverride', () => {
    test('routes the native + to the Request a Team modal for a member', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup={rail(true)}
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await recordNative(page);

        await page.locator('#create_teamTeamButton').click();
        expect(await actions(page)).toEqual([OPEN_TEAM_CREATION_MODAL]);
        expect(await nativeClicks(page)).toBe(0);
        await expect(page.locator('#mmPluginRequestTeamAddButton')).toHaveCount(0);
    });

    test('leaves Join and other links alone', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup={rail(true) + '<a href="/elsewhere/create_team" id="outside">outside</a>'}
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await recordNative(page);

        await page.locator('#select_teamTeamButton').click();
        await page.locator('#outside').click();
        expect(await actions(page)).toEqual([]);
        expect(await nativeClicks(page)).toBe(2);
    });

    test('keeps the native + for a System Admin, when team requests are off, and before settings load', async ({mount, page}) => {
        let team = false;
        let release: () => void = () => undefined;
        const held = new Promise<void>((r) => {
            release = r;
        });
        await countConfigLoads(page);
        await page.route(`${API}/config`, async (route) => {
            await held;
            await route.fulfill({contentType: 'application/json', body: JSON.stringify({...ALL_ON, team})});
        });
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup={rail(true)}
            state={member}
                    />);
        await harnessInstalled(page);
        await recordNative(page);

        // Settings not loaded yet.
        await page.locator('#create_teamTeamButton').click();
        expect(await nativeClicks(page)).toBe(1);

        // Team requests off.
        release();
        await configLoaded(page);
        await page.locator('#create_teamTeamButton').click();
        expect(await nativeClicks(page)).toBe(2);

        // System Admin.
        team = true;

        await page.evaluate((s) => window.__harness!.setState(s), sysadmin);
        await page.locator('#create_teamTeamButton').click();
        expect(await nativeClicks(page)).toBe(3);
        expect(await actions(page)).toEqual([]);
    });

    test('adds a + before Join when the native one is missing, and opens the modal', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup={rail(false)}
            state={member}
                    />);
        await harnessInstalled(page);
        const added = page.locator('#mmPluginRequestTeamAddButton');
        await expect(added).toHaveCount(1);
        expect(await page.evaluate(() => document.getElementById('mmPluginRequestTeamAddButton')!.nextElementSibling?.querySelector('#select_teamTeamButton') != null)).toBe(true);

        // The + is an icon-only link with no size outside Mattermost's styles.
        await page.getByRole('link', {name: 'Request a Team'}).dispatchEvent('click');
        await expect.poll(() => actions(page)).toEqual([OPEN_TEAM_CREATION_MODAL]);

        // Further mutations don't add a second one.
        await addMarkup(page, '<p>more</p>');
        await frame(page);
        await expect(added).toHaveCount(1);
    });

    test('appends the + when there is no Join button, and ignores clicks when team requests are off', async ({mount, page}) => {
        await routeConfig(page, {team: false});
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup='<div class="team-sidebar"><div class="team-wrapper"><div class="team-container">T1</div></div></div>'
            state={member}
                    />);
        await harnessInstalled(page);
        await expect(page.locator('.team-wrapper > :last-child')).toHaveId('mmPluginRequestTeamAddButton');

        // The + is an icon-only link with no size outside Mattermost's styles.
        await page.getByRole('link', {name: 'Request a Team'}).dispatchEvent('click');
        await configLoaded(page, 2);
        expect(await actions(page)).toEqual([]);
    });

    test('removes the + for a System Admin and when the native one appears', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup={rail(false)}
            state={member}
                    />);
        await harnessInstalled(page);
        const added = page.locator('#mmPluginRequestTeamAddButton');
        await expect(added).toHaveCount(1);

        await page.evaluate((s) => window.__harness!.setState(s), sysadmin);
        await addMarkup(page, '<p>re-render</p>');
        await expect(added).toHaveCount(0);

        await page.evaluate((s) => window.__harness!.setState(s), member);
        await addMarkup(page, '<p>re-render</p>');
        await expect(added).toHaveCount(1);

        await page.evaluate(() => {
            const a = document.createElement('a');
            a.id = 'create_teamTeamButton';
            a.href = '/create_team';
            document.getElementById('teamSidebarWrapper')!.appendChild(a);
        });
        await expect(added).toHaveCount(0);
    });

    test('does nothing without a team rail', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup='<div>no rail</div>'
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await frame(page);
        await expect(page.locator('#mmPluginRequestTeamAddButton')).toHaveCount(0);

        // The same override adds the + once a rail shows up.
        await addMarkup(page, '<div class="team-sidebar"><div class="team-wrapper"><div class="team-container">T1</div></div></div>');
        await expect(page.locator('#mmPluginRequestTeamAddButton')).toHaveCount(1);
    });

    test('dispose removes the + and stops intercepting', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup={rail(false)}
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await expect(page.locator('#mmPluginRequestTeamAddButton')).toHaveCount(1);

        await dispose(page);
        await expect(page.locator('#mmPluginRequestTeamAddButton')).toHaveCount(0);

        await addMarkup(page, '<div class="team-container"><a id="create_teamTeamButton" href="/create_team">+</a></div>');
        await recordNative(page);
        await page.locator('#create_teamTeamButton').click();
        expect(await actions(page)).toEqual([]);
        expect(await nativeClicks(page)).toBe(1);
        await frame(page);
        await expect(page.locator('#mmPluginRequestTeamAddButton')).toHaveCount(0);
    });

    test('dispose cancels a + already scheduled', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='teamSidebarAdd'
            markup='<div>no rail yet</div>'
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await frame(page);

        // The observer schedules a frame for the new rail; dispose runs before it.
        await page.evaluate(async (html) => {
            window.__harness!.addMarkup(html);
            await Promise.resolve();
            window.__harness!.dispose();
        }, rail(false));
        await frame(page);
        await expect(page.locator('#mmPluginRequestTeamAddButton')).toHaveCount(0);
    });
});
