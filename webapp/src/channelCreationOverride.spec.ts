import {expect, test} from '@playwright/test';

const SIBLING_LABELS = ['Browse channels', 'Open a direct message', 'Create new user group', 'Create new category', 'Invite people'];
const ITEM_SELECTOR = 'a, button, li, [role="menuitem"]';
const NATIVE_ITEM_ID = 'createNewChannelMenuItem';
const NATIVE_LABEL = 'Create new channel';
const REQUEST_LABEL = 'Request new channel';
const INJECTED_MARK = 'data-mm-plugin-channel-requests-injected';

test.describe('channelCreationOverride helpers', () => {
    test('findMenuRoot returns container with ≥2 recognized sibling labels', async ({page}) => {
        await page.setContent(`
            <div id="outer">
                <div id="menu">
                    <a>Browse channels</a>
                    <a>Open a direct message</a>
                    <a id="createNewChannelMenuItem">Create new channel</a>
                </div>
            </div>
        `);
        const rootId = await page.evaluate(({siblingLabels, itemSelector}) => {
            const items = Array.from(document.querySelectorAll<HTMLElement>(itemSelector));
            const sibling = items.find((el) => siblingLabels.includes((el.textContent ?? '').trim()));
            if (!sibling) {
                return null;
            }
            let cursor: HTMLElement | null = sibling.parentElement;
            while (cursor && cursor !== document.body) {
                let count = 0;
                cursor.querySelectorAll<HTMLElement>(itemSelector).forEach((el) => {
                    if (siblingLabels.includes((el.textContent ?? '').trim())) {
                        count++;
                    }
                });
                if (count >= 2) {
                    return cursor.id;
                }
                cursor = cursor.parentElement;
            }
            return null;
        }, {siblingLabels: SIBLING_LABELS, itemSelector: ITEM_SELECTOR});
        expect(rootId).toBe('menu');
    });

    test('findMenuRoot returns null when fewer than 2 sibling labels present', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a>Browse channels</a>
                <a id="createNewChannelMenuItem">Create new channel</a>
            </div>
        `);
        const rootId = await page.evaluate(({siblingLabels, itemSelector}) => {
            const items = Array.from(document.querySelectorAll<HTMLElement>(itemSelector));
            const sibling = items.find((el) => siblingLabels.includes((el.textContent ?? '').trim()));
            if (!sibling) {
                return null;
            }
            let cursor: HTMLElement | null = sibling.parentElement;
            while (cursor && cursor !== document.body) {
                let count = 0;
                cursor.querySelectorAll<HTMLElement>(itemSelector).forEach((el) => {
                    if (siblingLabels.includes((el.textContent ?? '').trim())) {
                        count++;
                    }
                });
                if (count >= 2) {
                    return cursor.id;
                }
                cursor = cursor.parentElement;
            }
            return null;
        }, {siblingLabels: SIBLING_LABELS, itemSelector: ITEM_SELECTOR});
        expect(rootId).toBeNull();
    });

    test('replaceLabel replaces text node content', async ({page}) => {
        await page.setContent('<div id="item">Create new channel</div>');
        const result = await page.evaluate(({nativeLabel, requestLabel}) => {
            const root = document.getElementById('item') as HTMLElement;
            const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
            let node = walker.nextNode();
            while (node) {
                if (node.nodeValue && node.nodeValue.trim() === nativeLabel) {
                    node.nodeValue = node.nodeValue.replace(nativeLabel, requestLabel);
                    return true;
                }
                node = walker.nextNode();
            }
            return false;
        }, {nativeLabel: NATIVE_LABEL, requestLabel: REQUEST_LABEL});
        expect(result).toBe(true);
        await expect(page.locator('#item')).toHaveText(REQUEST_LABEL);
    });

    test('findNativeCreateItem finds by id when present', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a id="createNewChannelMenuItem">Create new channel</a>
                <a>Browse channels</a>
            </div>
        `);
        const found = await page.evaluate(({nativeItemId}) => {
            const byId = document.querySelector(`#${nativeItemId}`);
            return byId ? byId.id : null;
        }, {nativeItemId: NATIVE_ITEM_ID});
        expect(found).toBe(NATIVE_ITEM_ID);
    });

    test('findNativeCreateItem falls back to text match when id absent', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a>Create new channel</a>
                <a>Browse channels</a>
                <a>Open a direct message</a>
            </div>
        `);
        const foundText = await page.evaluate(({nativeLabel, itemSelector}) => {
            const byId = document.querySelector('#createNewChannelMenuItem');
            if (byId) {
                return byId.textContent?.trim() ?? null;
            }
            const items = Array.from(document.querySelectorAll<HTMLElement>(itemSelector));
            const match = items.find((el) => (el.textContent ?? '').trim() === nativeLabel);
            return match ? match.textContent?.trim() ?? null : null;
        }, {nativeLabel: NATIVE_LABEL, itemSelector: ITEM_SELECTOR});
        expect(foundText).toBe(NATIVE_LABEL);
    });

    test('injection skips duplicate when INJECTED_MARK already present', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a data-mm-plugin-channel-requests-injected="true">Request new channel</a>
                <a>Browse channels</a>
                <a>Open a direct message</a>
            </div>
        `);
        const count = await page.evaluate(({injectedMark}) => {
            return document.querySelectorAll(`[${injectedMark}]`).length;
        }, {injectedMark: INJECTED_MARK});
        expect(count).toBe(1);
    });

    test('injected item is prepended to the container', async ({page}) => {
        await page.setContent(`
            <div id="container">
                <a>Browse channels</a>
                <a>Open a direct message</a>
            </div>
        `);
        await page.evaluate(({requestLabel, injectedMark, itemSelector}) => {
            const container = document.getElementById('container') as HTMLElement;
            const sibling = Array.from(container.querySelectorAll<HTMLElement>(itemSelector))[0];
            const injected = sibling.cloneNode(true) as HTMLElement;
            injected.setAttribute(injectedMark, 'true');
            injected.textContent = requestLabel;
            container.insertBefore(injected, container.firstChild);
        }, {requestLabel: REQUEST_LABEL, injectedMark: INJECTED_MARK, itemSelector: ITEM_SELECTOR});

        const firstItem = await page.evaluate(() => {
            const container = document.getElementById('container') as HTMLElement;
            return (container.firstElementChild as HTMLElement)?.textContent?.trim();
        });
        expect(firstItem).toBe(REQUEST_LABEL);
    });
});
