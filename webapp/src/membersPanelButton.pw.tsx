import type {Page} from '@playwright/test';
import React from 'react';

import {DomInjectorHarness} from './DomInjectorHarness';
import {OPEN_ADMIN_MODAL} from './store';

import {expect, test} from '../playwright/ct-coverage';
import {frame, harnessInstalled} from '../playwright/ct-helpers';

type ChannelState = {type?: string; roles?: string; systemRoles?: string; channelId?: string};

function stateFor({type = 'O', roles = 'channel_user', systemRoles = 'system_user', channelId = 'chan1'}: ChannelState = {}) {
    return {
        entities: {
            teams: {currentTeamId: 't1'},
            users: {currentUserId: 'me', profiles: {me: {roles: systemRoles}}},
            channels: {
                currentChannelId: channelId,
                channels: channelId ? {[channelId]: {type}} : {},
                myMembers: channelId ? {[channelId]: {roles}} : {},
            },
        },
    };
}

const membersPanel = `
<div id="rhsContainer">
  <div class="channel-members-rhs">
    <span>3 members</span>
    <button class="add-members">Add</button>
  </div>
</div>`;

const button = (page: Page) => page.getByRole('button', {name: 'Request Admin'});

async function status(page: Page) {
    return page.evaluate(() => (window as unknown as {__cr_status?: Record<string, unknown>}).__cr_status);
}

test.describe('membersPanelButton', () => {
    test.describe('injecting', () => {
        test('adds the button next to Add and opens the admin modal', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup={membersPanel}
                state={stateFor()}
                        />);
            await harnessInstalled(page);
            await expect(button(page)).toBeVisible();
            await expect(page.locator('.add-members + button')).toHaveText('Request Admin');

            expect(await page.evaluate(() => (window as unknown as {__cr_injector_loaded?: boolean}).__cr_injector_loaded)).toBe(true);

            await button(page).click();

            expect(await page.evaluate(() => window.__harness!.actions)).toEqual([{type: OPEN_ADMIN_MODAL, channelId: 'chan1'}]);
        });

        for (const [name, markup] of [
            ['the Manage button', '<div id="sidebar-right"><button class="manage-members">Manage</button></div>'],
            ['the add-members id', '<div data-testid="sidebarRight"><button id="addMembersButton">Add</button></div>'],
            ['the add-members test id', '<div class="sidebar--right"><button data-testid="addMembersButton">Add</button></div>'],
            ['the Done button', '<div class="sidebar-right"><button class="manage-members-done">Done</button></div>'],
        ]) {
            test(`anchors on ${name}`, async ({mount, page}) => {
                await mount(<DomInjectorHarness
                    injector='membersPanel'
                    markup={markup}
                    state={stateFor()}
                            />);
                await harnessInstalled(page);
                await expect(button(page)).toBeVisible();
            });
        }

        test('anchors on the known layout position', async ({mount, page}) => {
            const markup = `
<div id="rhsContainer">
  <div>Channel members</div><div></div>
  <div><div><div><div><div></div><div><div><span>Member list</span></div></div></div></div></div></div>
</div>`;
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup={markup}
                state={stateFor()}
                        />);
            await harnessInstalled(page);
            await expect(page.locator('span + button')).toHaveText('Request Admin');
        });

        test('anchors on the member count', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup='<div id="rhsContainer"><h2>12 Members</h2></div>'
                state={stateFor()}
                        />);
            await harnessInstalled(page);
            await expect(page.locator('h2 + button')).toHaveText('Request Admin');
        });

        test('anchors on an add-members link', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup='<div id="rhsContainer"><p>Members of this channel</p><a role="button" aria-label="Add members">+</a></div>'
                state={stateFor()}
                        />);
            await harnessInstalled(page);
            await expect(page.locator('a + button')).toHaveText('Request Admin');
        });

        test('adds the button for an unknown channel type', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup={membersPanel}
                state={stateFor({type: ''})}
                        />);
            await harnessInstalled(page);
            await expect(button(page)).toBeVisible();
        });

        test('adds it only once and again after a re-render removes it', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup={membersPanel}
                state={stateFor()}
                        />);
            await harnessInstalled(page);
            await expect(button(page)).toBeVisible();

            await page.evaluate(() => window.__harness!.addMarkup('<p>new post</p>'));
            await expect.poll(async () => (await status(page))?.skip).toBe('already-injected');
            await expect(button(page)).toHaveCount(1);

            await page.evaluate(() => document.querySelector('[data-cr-request-admin]')!.remove());
            await expect(button(page)).toHaveCount(1);
        });

        test('does nothing on click when no channel is open', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup={membersPanel}
                state={stateFor({channelId: ''})}
                        />);
            await harnessInstalled(page);
            await button(page).click();

            expect(await page.evaluate(() => window.__harness!.actions)).toEqual([]);
        });
    });

    test.describe('disposing', () => {
        test('removes the button', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup={membersPanel}
                state={stateFor()}
                        />);
            await harnessInstalled(page);
            await expect(button(page)).toBeVisible();

            await page.evaluate(() => window.__harness!.dispose());
            await expect(button(page)).toHaveCount(0);

            // The panel's own controls still work and dispatch nothing of ours.
            await page.locator('.add-members').click();
            expect(await page.evaluate(() => window.__harness!.actions)).toEqual([]);
        });

        test('does not add the button again on later renders', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='membersPanel'
                markup='<div>loading</div>'
                state={stateFor()}
                        />);
            await harnessInstalled(page);

            await page.evaluate(() => window.__harness!.dispose());

            await page.evaluate((html) => window.__harness!.addMarkup(html), membersPanel);
            await frame(page);
            await expect(button(page)).toHaveCount(0);
        });

        for (const disposeFirst of [false, true]) {
            test(disposeFirst ? 'cancels an injection already scheduled' : 'an injection scheduled the same way lands without dispose', async ({mount, page}) => {
                await mount(<DomInjectorHarness
                    injector='membersPanel'
                    markup='<div>loading</div>'
                    state={stateFor()}
                            />);
                await harnessInstalled(page);

                // The observer callback runs as a microtask and schedules the
                // injection for the next frame; dispose lands in between.
                await page.evaluate(async ({html, doDispose}) => {
                    window.__harness!.addMarkup(html);
                    await Promise.resolve();
                    if (doDispose) {
                        window.__harness!.dispose();
                    }
                }, {html: membersPanel, doDispose: disposeFirst});
                await frame(page);
                await expect(button(page)).toHaveCount(disposeFirst ? 0 : 1);
            });
        }
    });

    test.describe('skipping', () => {
        for (const [name, markup, state, skip] of [
            ['without a right-hand sidebar', '<div>nothing here</div>', stateFor(), 'no-rhs'],
            ['for a panel other than Members', '<div id="rhsContainer"><p>Search results</p></div>', stateFor(), 'not-members-panel'],
            ['in a direct message', membersPanel, stateFor({type: 'D'}), 'dm-or-gm'],
            ['in a group message', membersPanel, stateFor({type: 'G'}), 'dm-or-gm'],
            ['for a Channel Admin', membersPanel, stateFor({roles: 'channel_user channel_admin'}), 'is-admin'],
            ['for a System Admin', membersPanel, stateFor({systemRoles: 'system_admin'}), 'is-admin'],
            ['when there is nothing to anchor to', '<div id="rhsContainer"><p>Members of this channel</p></div>', stateFor(), 'no-anchor'],
        ] as const) {
            test(name, async ({mount, page}) => {
                await mount(<DomInjectorHarness
                    injector='membersPanel'
                    markup={markup}
                    state={state}
                            />);
                await harnessInstalled(page);
                await expect.poll(async () => (await status(page))?.skip).toBe(skip);
                await expect(button(page)).toHaveCount(0);
            });
        }
    });
});
