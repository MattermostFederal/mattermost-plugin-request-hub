import type {Page} from '@playwright/test';
import React from 'react';

import {DomInjectorHarness} from './DomInjectorHarness';
import type {HarnessRecord} from './DomInjectorHarness';
import {OPEN_REQUEST_MODAL} from './store';

import {expect, test} from '../playwright/ct-coverage';
import {frame, harnessInstalled} from '../playwright/ct-helpers';

const member = {entities: {users: {currentUserId: 'me', profiles: {me: {roles: 'system_user'}}}}};
const admin = {entities: {users: {currentUserId: 'me', profiles: {me: {roles: 'system_user system_admin'}}}}};

const menuWithNativeById = `
<ul role="menu">
  <li><button id="createNewChannelMenuItem"><i class="icon"></i><span>Create new channel</span></button></li>
  <li><button><span>Browse channels</span></button></li>
  <li><button><span>Open a direct message</span></button></li>
</ul>`;

const menuWithNativeByText = `
<ul role="menu">
  <li><button><span>Create new channel</span></button></li>
  <li><button><span>Browse channels</span></button></li>
  <li><button><span>Create new category</span></button></li>
</ul>`;

const menuWithoutNative = `
<ul role="menu">
  <li><a href="/browse"><span>Browse channels</span></a></li>
  <li><a href="/dm"><span>Open a direct message</span></a></li>
</ul>`;

const menuWithComposedLabels = `
<div role="menu">
  <button><i class="icon-globe"></i>Browse <b>channels</b></button>
  <button>Invite people</button>
</div>`;

async function recorded(page: Page): Promise<HarnessRecord> {
    return page.evaluate(() => ({actions: window.__harness!.actions, escapes: window.__harness!.escapes}));
}

test.describe('channelCreationOverride', () => {
    test.describe('with the native item', () => {
        test('relabels it by id and opens the request modal on click', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={menuWithNativeById}
                state={member}
                        />);
            await harnessInstalled(page);
            const item = page.locator('#createNewChannelMenuItem');
            await expect(item).toHaveText('Request new channel');
            await item.click();
            expect(await recorded(page)).toEqual({actions: [{type: OPEN_REQUEST_MODAL}], escapes: 1});
        });

        test('closes the menu with an Escape from inside it', async ({mount, page}) => {
            // MUI menus only close on an Escape that reaches the menu itself,
            // not one sent straight to document.
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={menuWithNativeById}
                state={member}
                        />);
            await harnessInstalled(page);
            const menuEscapes = page.evaluate(() => new Promise<string>((resolve) => {
                document.querySelector('[role="menu"]')!.addEventListener('keydown', (e) => resolve((e as KeyboardEvent).key), {once: true});
            }));
            await page.locator('#createNewChannelMenuItem').click();
            expect(await menuEscapes).toBe('Escape');
        });

        test('relabels it by text and opens the request modal on click', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={menuWithNativeByText}
                state={member}
                        />);
            await harnessInstalled(page);
            const item = page.getByRole('button', {name: 'Request new channel'});
            await expect(item).toBeVisible();
            await item.click();
            expect((await recorded(page)).actions).toEqual([{type: OPEN_REQUEST_MODAL}]);
        });

        test('leaves the menu alone for a System Admin', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={menuWithNativeById}
                state={admin}
                        />);
            await harnessInstalled(page);
            await frame(page);
            await expect(page.locator('#createNewChannelMenuItem')).toHaveText('Create new channel');
            await page.locator('#createNewChannelMenuItem').click();
            expect((await recorded(page)).actions).toEqual([]);
        });
    });

    test('keeps one item when a menu relabeled by text re-renders', async ({mount, page}) => {
        await mount(<DomInjectorHarness
            injector='channelCreation'
            markup={menuWithNativeByText}
            state={member}
                    />);
        await harnessInstalled(page);
        await expect(page.getByText('Request new channel')).toHaveCount(1);

        await page.evaluate(() => window.__harness!.addMarkup('<p>unrelated</p>'));
        await frame(page);
        await expect(page.getByText('Request new channel')).toHaveCount(1);
    });

    test.describe('without the native item', () => {
        test('injects one request item that opens the modal', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={menuWithoutNative}
                state={member}
                        />);
            await harnessInstalled(page);
            const injected = page.getByTestId('channelRequestsInjectedItem');
            await expect(injected).toHaveText('Request new channel');
            await expect(page.locator('ul li').first()).toHaveAttribute('data-mm-plugin-channel-requests-injected', 'true');

            // Another DOM change rescans the menu without injecting again.
            await page.evaluate(() => window.__harness!.addMarkup('<p>unrelated</p>'));
            await frame(page);
            await expect(injected).toHaveCount(1);

            await injected.click();
            expect(await recorded(page)).toEqual({actions: [{type: OPEN_REQUEST_MODAL}], escapes: 1});
        });

        test('relabels a native item that appears after the injected one', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={menuWithoutNative}
                state={member}
                        />);
            await harnessInstalled(page);
            await expect(page.getByTestId('channelRequestsInjectedItem')).toBeVisible();

            // The injected <li> carries the mark but its <a> doesn't; the <a>
            // must not pass for the native item and block the relabel.
            await page.locator('ul[role="menu"]').evaluate((el) => el.insertAdjacentHTML('beforeend', '<li><button><span>Create new channel</span></button></li>'));
            const native = page.locator('ul[role="menu"] li:last-child button');
            await expect(native).toHaveText('Request new channel');
            await native.click();
            expect((await recorded(page)).actions).toEqual([{type: OPEN_REQUEST_MODAL}]);
        });

        test('points an injected link nowhere', async ({mount, page}) => {
            const markup = '<nav role="menu"><a href="/browse">Browse channels</a><a href="/dm">Open a direct message</a></nav>';
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={markup}
                state={member}
                        />);
            await harnessInstalled(page);
            await expect(page.getByTestId('channelRequestsInjectedItem')).toHaveAttribute('href', '#');
        });

        test('keeps the icon when the label is split across elements', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={menuWithComposedLabels}
                state={member}
                        />);
            await harnessInstalled(page);
            const injected = page.getByTestId('channelRequestsInjectedItem');
            await expect(injected).toHaveText('Request new channel');
            await expect(injected.locator('i.icon-globe')).toBeAttached();
        });

        test('handles a menu that opens after install', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup=''
                state={member}
                        />);
            await harnessInstalled(page);
            await page.evaluate((html) => window.__harness!.addMarkup(html), menuWithoutNative);
            await expect(page.getByTestId('channelRequestsInjectedItem')).toBeVisible();
        });

        test('ignores a page with no channel menu', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup='<ul><li>Browse channels</li></ul>'
                state={member}
                        />);
            await harnessInstalled(page);
            await frame(page);
            await expect(page.getByTestId('channelRequestsInjectedItem')).toHaveCount(0);

            // Control: once the stray list is gone, a real channel menu gets the item.
            await page.locator('ul:not([role])').evaluate((el) => el.remove());
            await page.evaluate((html) => window.__harness!.addMarkup(html), menuWithoutNative);
            await expect(page.getByTestId('channelRequestsInjectedItem')).toHaveCount(1);
        });
    });

    test.describe('click routing', () => {
        test('ignores a matching label outside the channel menu', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup='<button>Request new channel</button>'
                state={member}
                        />);
            await harnessInstalled(page);
            await page.getByRole('button', {name: 'Request new channel'}).click();
            expect((await recorded(page)).actions).toEqual([]);
        });

        test('ignores other clicks', async ({mount, page}) => {
            await mount(<DomInjectorHarness
                injector='channelCreation'
                markup={menuWithNativeById}
                state={member}
                        />);
            await harnessInstalled(page);
            await page.getByRole('button', {name: 'Browse channels'}).click();
            expect((await recorded(page)).actions).toEqual([]);
        });
    });
});
