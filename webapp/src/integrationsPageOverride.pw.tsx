import type {Page} from '@playwright/test';
import React from 'react';

import {DomInjectorHarness} from './DomInjectorHarness';
import {OPEN_BOT_REQUEST_MODAL, OPEN_INCOMING_WEBHOOK_MODAL, OPEN_OUTGOING_WEBHOOK_MODAL} from './store';

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

const member = {entities: {users: {currentUserId: 'me', profiles: {me: {roles: 'system_user'}}}}};
const sysadmin = {entities: {users: {currentUserId: 'me', profiles: {me: {roles: 'system_user system_admin'}}}}};

const links = `
<div id="links">
  <a id="incoming" href="/team-one/integrations/add_incoming_webhook">Add incoming</a>
  <a id="outgoing" href="/team-one/integrations/add_outgoing_webhook">Add outgoing</a>
  <a id="bot" href="/team-one/integrations/add_bot_account">Add bot</a>
  <a id="list" href="/team-one/integrations/incoming_webhooks">Incoming list</a>
  <a id="external" href="https://example.com/integrations/add_incoming_webhook">External</a>
  <a id="script" href="javascript:void(0)">Script</a>
  <a id="hash" href="#top">Hash</a>
  <a id="nohref">No href</a>
  <button id="plain">Plain</button>
</div>`;

// recordNative counts clicks that reach a bubble-phase document listener,
// i.e. clicks the override let through, and stops the browser navigating.
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

async function goTo(page: Page, path: string) {
    await page.evaluate((p) => {
        history.pushState({}, '', p);
        window.dispatchEvent(new PopStateEvent('popstate'));
    }, path);
}

test.describe('integrationsPageOverride', () => {
    test.describe('create links', () => {
        test('open the request modal for each type', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup={links}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await recordNative(page);

            await page.locator('#incoming').click();
            await page.locator('#outgoing').click();
            await page.locator('#bot').click();
            expect(await actions(page)).toEqual([OPEN_INCOMING_WEBHOOK_MODAL, OPEN_OUTGOING_WEBHOOK_MODAL, OPEN_BOT_REQUEST_MODAL]);
            expect(await nativeClicks(page)).toBe(0);
        });

        test('are left alone when the type is off', async ({mount, page}) => {
            await routeConfig(page, {webhook: false, bot: false});
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup={links}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await recordNative(page);

            await page.locator('#incoming').click();
            await page.locator('#bot').click();
            expect(await actions(page)).toEqual([]);
            expect(await nativeClicks(page)).toBe(2);
        });

        test('are left alone before the settings load', async ({mount, page}) => {
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
                injector='integrationsPage'
                markup={links}
                state={member}
                        />);
            await harnessInstalled(page);
            await recordNative(page);

            await page.locator('#incoming').click();
            expect(await actions(page)).toEqual([]);
            expect(await nativeClicks(page)).toBe(1);
            release();
        });

        test('are left alone for a System Admin', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup={links}
                state={sysadmin}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await recordNative(page);

            await page.locator('#incoming').click();
            expect(await actions(page)).toEqual([]);
            expect(await nativeClicks(page)).toBe(1);
        });

        test('are left alone on a modifier or non-primary click', async ({mount, page}) => {
            await routeConfig(page);
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup={links}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await recordNative(page);

            for (const init of [{metaKey: true}, {ctrlKey: true}, {shiftKey: true}, {altKey: true}, {button: 1}]) {
                // eslint-disable-next-line no-await-in-loop
                await page.locator('#incoming').dispatchEvent('click', init);
            }
            expect(await actions(page)).toEqual([]);
            expect(await nativeClicks(page)).toBe(5);
        });
    });

    test('other links and clicks are left alone', async ({mount, page}) => {
        await routeConfig(page);
        await mount(<DomInjectorHarness
            injector='integrationsPage'
            markup={links}
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await recordNative(page);

        for (const id of ['list', 'external', 'script', 'hash', 'nohref', 'plain']) {
            // eslint-disable-next-line no-await-in-loop
            await page.locator(`#${id}`).click();
        }
        await page.evaluate(() => document.dispatchEvent(new MouseEvent('click', {bubbles: true})));
        expect(await actions(page)).toEqual([]);
        expect(await nativeClicks(page)).toBe(7);
    });

    test.describe('landing page cards', () => {
        const backstage = (tiles = '') => `<div class="backstage-content"><h1>Integrations</h1>${tiles}</div>`;

        test('are added when the native tiles are missing and open the modals', async ({mount, page}) => {
            await routeConfig(page);
            await page.evaluate(() => history.pushState({}, '', '/team-one/integrations'));
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup={backstage()}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);

            const row = page.locator('[data-request-hub-integrations="row"]');
            await expect(row).toHaveCount(1);
            expect(await page.evaluate(() => document.querySelector('.backstage-content h1')!.nextElementSibling!.getAttribute('data-request-hub-integrations'))).toBe('row');

            await page.getByTestId('request-hub-integrations-incoming').click();
            await page.getByTestId('request-hub-integrations-outgoing').click();
            await page.getByTestId('request-hub-integrations-bot').click();
            expect(await actions(page)).toEqual([OPEN_INCOMING_WEBHOOK_MODAL, OPEN_OUTGOING_WEBHOOK_MODAL, OPEN_BOT_REQUEST_MODAL]);

            // A later scan doesn't duplicate them.
            await page.evaluate(() => window.__harness!.addMarkup('<p>more</p>'));
            await frame(page);
            await expect(page.locator('[data-request-hub-integrations]')).toHaveCount(4);
        });

        test('skip types whose native tile is shown and types that are off', async ({mount, page}) => {
            await routeConfig(page, {bot: false});
            await page.evaluate(() => history.pushState({}, '', '/team-one/integrations/'));
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup={backstage('<a>Incoming Webhooks</a>')}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await frame(page);

            await expect(page.getByTestId('request-hub-integrations-incoming')).toHaveCount(0);
            await expect(page.getByTestId('request-hub-integrations-outgoing')).toHaveCount(1);
            await expect(page.getByTestId('request-hub-integrations-bot')).toHaveCount(0);
        });

        test('are removed when every native tile shows', async ({mount, page}) => {
            await routeConfig(page);
            await page.evaluate(() => history.pushState({}, '', '/team-one/integrations'));
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup={backstage()}
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await expect(page.locator('[data-request-hub-integrations="row"]')).toHaveCount(1);

            await page.evaluate(() => window.__harness!.addMarkup('<p>Incoming Webhooks</p><p>Outgoing Webhooks</p><p>Bot Accounts</p>'));
            await expect(page.locator('[data-request-hub-integrations]')).toHaveCount(0);
        });

        test('go at the top of the host when the heading is nested', async ({mount, page}) => {
            await routeConfig(page);
            await page.evaluate(() => history.pushState({}, '', '/team-one/integrations'));
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup='<main><header><h2>Integrations</h2></header><p>intro</p></main>'
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await expect(page.locator('main > :first-child')).toHaveAttribute('data-request-hub-integrations', 'row');
        });

        test('are not added without a host or off the landing page, and are removed for a System Admin', async ({mount, page}) => {
            await routeConfig(page);
            await page.evaluate(() => history.pushState({}, '', '/team-one/integrations'));
            await mount(<DomInjectorHarness
                injector='integrationsPage'
                markup='<div>no host</div>'
                state={member}
                        />);
            await harnessInstalled(page);
            await configLoaded(page);
            await frame(page);
            await expect(page.locator('[data-request-hub-integrations]')).toHaveCount(0);

            // A host appears.
            await page.evaluate((html) => window.__harness!.addMarkup(html), backstage());
            await expect(page.locator('[data-request-hub-integrations="row"]')).toHaveCount(1);

            // Navigating to a sub-page removes them. Each navigation reloads the settings.
            await goTo(page, '/team-one/integrations/incoming_webhooks');
            await configLoaded(page, 2);
            await expect(page.locator('[data-request-hub-integrations]')).toHaveCount(0);

            // Back on the landing page they return.
            await goTo(page, '/team-one/integrations');
            await configLoaded(page, 3);
            await expect(page.locator('[data-request-hub-integrations="row"]')).toHaveCount(1);

            // A System Admin gets none.
            await page.evaluate((s) => window.__harness!.setState(s), sysadmin);
            await goTo(page, '/team-one/integrations');
            await configLoaded(page, 4);
            await expect(page.locator('[data-request-hub-integrations]')).toHaveCount(0);
        });
    });

    test('dispose removes the cards and stops intercepting', async ({mount, page}) => {
        await routeConfig(page);
        await page.evaluate(() => history.pushState({}, '', '/team-one/integrations'));
        await mount(<DomInjectorHarness
            injector='integrationsPage'
            markup={'<div class="backstage-content"><h1>Integrations</h1></div>' + links}
            state={member}
                    />);
        await harnessInstalled(page);
        await configLoaded(page);
        await expect(page.locator('[data-request-hub-integrations="row"]')).toHaveCount(1);

        await page.evaluate(() => window.__harness!.dispose());
        await expect(page.locator('[data-request-hub-integrations]')).toHaveCount(0);

        await recordNative(page);
        await page.locator('#incoming').click();
        expect(await actions(page)).toEqual([]);
        expect(await nativeClicks(page)).toBe(1);

        await page.evaluate(() => window.__harness!.addMarkup('<p>more</p>'));
        await frame(page);
        await expect(page.locator('[data-request-hub-integrations]')).toHaveCount(0);
    });
});
