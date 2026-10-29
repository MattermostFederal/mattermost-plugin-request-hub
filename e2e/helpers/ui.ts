// Browser helpers for specs that drive the Mattermost webapp.
import type {Page} from '@playwright/test';

import {PASSWORD, TEAM, type RequestHub} from './requesthub';

export async function login(page: Page, hub: RequestHub, username: string) {
    await page.goto(`${hub.mm.url()}/login`);
    const viewInBrowser = page.getByText('View in Browser');
    await viewInBrowser.or(page.locator('#input_loginId')).first().waitFor();
    if (await viewInBrowser.isVisible()) {
        await viewInBrowser.click();
    }
    await page.locator('#input_loginId').fill(username);
    await page.locator('#input_password-input').fill(PASSWORD);
    await page.keyboard.press('Enter');
    await page.waitForURL((u) => !u.pathname.startsWith('/login'));
}

export async function openChannel(page: Page, hub: RequestHub, channelName: string) {
    await page.goto(`${hub.mm.url()}/${TEAM}/channels/${channelName}`);
    await page.getByTestId('post_textbox').waitFor();
}
