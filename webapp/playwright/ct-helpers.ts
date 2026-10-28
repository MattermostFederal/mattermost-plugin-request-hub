// Shared helpers for the component tests. Test files import these from
// '../playwright/ct-helpers' next to the coverage fixture.

import type {Page} from '@playwright/test';

import manifest from '../src/manifest';

// API matches the plugin's REST routes, for page.route and waitForResponse.
export const API = `**/plugins/${manifest.id}/api/v1`;

// ALL_ON is a /config response with every request type turned on.
export const ALL_ON = {
    channel: true,
    channel_admin: true,
    team: true,
    team_admin: true,
    team_member: true,
    team_guest: true,
    bot: true,
    webhook: true,
};

// routeConfig answers /config with ALL_ON plus any overrides, and starts
// counting parsed /config responses so configLoaded can wait for them.
export async function routeConfig(page: Page, overrides: Record<string, boolean> = {}) {
    await countConfigLoads(page);
    await page.route(`${API}/config`, (route) => route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({...ALL_ON, ...overrides}),
    }));
}

// countConfigLoads counts /config bodies the page has parsed, in
// window.__configLoads. Call it before mount.
export async function countConfigLoads(page: Page) {
    await page.evaluate(() => {
        const w = window as unknown as {__configLoads?: number; __configLoadsPatched?: boolean};
        w.__configLoads = 0;
        if (w.__configLoadsPatched) {
            return;
        }
        w.__configLoadsPatched = true;
        const json = Response.prototype.json;
        Response.prototype.json = function patchedJson(this: Response) {
            return json.call(this).then((body: unknown) => {
                if (this.url.includes('/api/v1/config')) {
                    w.__configLoads = (w.__configLoads ?? 0) + 1;
                }
                return body;
            });
        };
    });
}

// configLoaded waits until the page has parsed at least `count` /config
// responses. waitForFunction polls on a later task, so the code that
// awaited the parsed body has run by the time it returns.
export async function configLoaded(page: Page, count = 1) {
    await page.waitForFunction((n) => ((window as unknown as {__configLoads?: number}).__configLoads ?? 0) >= n, count);
}

// harnessInstalled waits for DomInjectorHarness to write its markup and
// install the injector; mount() can resolve before React commits it.
export async function harnessInstalled(page: Page) {
    await page.getByTestId('harness-ready').waitFor({state: 'attached'});
}

// harnessActions returns the action types the injector dispatched.
export async function harnessActions(page: Page): Promise<string[]> {
    return page.evaluate(() => window.__harness!.actions.map((a) => a.type));
}

// frame waits for the page to render one animation frame.
export async function frame(page: Page) {
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => setTimeout(r, 0))));
}
