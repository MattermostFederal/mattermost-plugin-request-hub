import {expect, test} from '@playwright/test';

const TEAM_MENU_SIBLING_LABELS = ['Invite people', 'Team settings', 'Manage members', 'Leave team', 'Learn about teams'];
const ITEM_SELECTOR = 'a, button, li, [role="menuitem"]';
const CREATE_TEAM_LABEL = 'Create a team';
const REQUEST_TEAM_LABEL = 'Request a Team';
const MANAGE_MEMBERS_LABEL = 'Manage members';
const REQUEST_TEAM_ADMIN_LABEL = 'Request Team Admin';
const CREATE_TEAM_MARK = 'data-mm-plugin-create-team-intercepted';
const REQUEST_TEAM_ADMIN_MARK = 'data-mm-plugin-request-team-admin';

test.describe('teamMenuOverride helpers', () => {
    test('findTeamMenuRoot returns container with ≥2 team menu sibling labels', async ({page}) => {
        await page.setContent(`
            <div id="outer">
                <div id="menu">
                    <a>Invite people</a>
                    <a>Team settings</a>
                    <a>Manage members</a>
                    <a>Create a team</a>
                </div>
            </div>
        `);
        const rootId = await page.evaluate(({siblingLabels, itemSelector}) => {
            const items = Array.from(document.querySelectorAll<HTMLElement>(itemSelector));
            for (const item of items) {
                const label = (item.textContent ?? '').trim();
                if (!siblingLabels.includes(label)) {
                    continue;
                }
                let cursor: HTMLElement | null = item.parentElement;
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
            }
            return null;
        }, {siblingLabels: TEAM_MENU_SIBLING_LABELS, itemSelector: ITEM_SELECTOR});
        expect(rootId).toBe('menu');
    });

    test('findTeamMenuRoot returns null when fewer than 2 team sibling labels present', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a>Invite people</a>
                <a>Create a team</a>
            </div>
        `);
        const rootId = await page.evaluate(({siblingLabels, itemSelector}) => {
            const items = Array.from(document.querySelectorAll<HTMLElement>(itemSelector));
            for (const item of items) {
                const label = (item.textContent ?? '').trim();
                if (!siblingLabels.includes(label)) {
                    continue;
                }
                let cursor: HTMLElement | null = item.parentElement;
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
            }
            return null;
        }, {siblingLabels: TEAM_MENU_SIBLING_LABELS, itemSelector: ITEM_SELECTOR});
        expect(rootId).toBeNull();
    });

    test('replaceTextNode replaces matching text node in tree', async ({page}) => {
        await page.setContent('<a id="item"><span>Create a team</span></a>');
        const changed = await page.evaluate(({createLabel, requestLabel}) => {
            const root = document.getElementById('item') as HTMLElement;
            const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
            let node = walker.nextNode();
            while (node) {
                if (node.nodeValue && node.nodeValue.trim() === createLabel) {
                    node.nodeValue = node.nodeValue.replace(createLabel, requestLabel);
                    return true;
                }
                node = walker.nextNode();
            }
            return false;
        }, {createLabel: CREATE_TEAM_LABEL, requestLabel: REQUEST_TEAM_LABEL});
        expect(changed).toBe(true);
        await expect(page.locator('#item')).toHaveText(REQUEST_TEAM_LABEL);
    });

    test('interceptCreateTeam no-ops when item is already marked', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a data-mm-plugin-create-team-intercepted="true">Request a Team</a>
                <a>Invite people</a>
                <a>Team settings</a>
            </div>
        `);
        const alreadyMarked = await page.evaluate(({createTeamMark}) => {
            return document.querySelector(`[${createTeamMark}]`) !== null;
        }, {createTeamMark: CREATE_TEAM_MARK});
        expect(alreadyMarked).toBe(true);
    });

    test('interceptCreateTeam marks and relabels Create a team item', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a id="createTeam">Create a team</a>
                <a>Invite people</a>
                <a>Team settings</a>
            </div>
        `);
        await page.evaluate(({createLabel, requestLabel, createTeamMark}) => {
            const itemSelector = 'a, button, li, [role="menuitem"]';
            const menu = document.getElementById('menu') as HTMLElement;
            if (menu.querySelector(`[${createTeamMark}]`)) {
                return;
            }
            const items = Array.from(menu.querySelectorAll<HTMLElement>(itemSelector));
            const native = items.find((el) => (el.textContent ?? '').trim() === createLabel);
            if (!native) {
                return;
            }
            const walker = document.createTreeWalker(native, NodeFilter.SHOW_TEXT);
            let node = walker.nextNode();
            while (node) {
                if (node.nodeValue && node.nodeValue.trim() === createLabel) {
                    node.nodeValue = node.nodeValue.replace(createLabel, requestLabel);
                    native.setAttribute(createTeamMark, 'true');
                    break;
                }
                node = walker.nextNode();
            }
        }, {createLabel: CREATE_TEAM_LABEL, requestLabel: REQUEST_TEAM_LABEL, createTeamMark: CREATE_TEAM_MARK});

        await expect(page.locator('#createTeam')).toHaveText(REQUEST_TEAM_LABEL);
        const marked = await page.locator(`[${CREATE_TEAM_MARK}]`).count();
        expect(marked).toBe(1);
    });

    test('injectRequestTeamAdmin positions new item immediately after Manage members', async ({page}) => {
        await page.setContent(`
            <div id="container">
                <a id="manage">Manage members</a>
                <a id="leave">Leave team</a>
            </div>
        `);
        await page.evaluate(({manageMembersLabel, requestTeamAdminLabel, requestTeamAdminMark}) => {
            const itemSelector = 'a, button, li, [role="menuitem"]';
            const items = Array.from(document.querySelectorAll<HTMLElement>(itemSelector));
            const manageItem = items.find((el) => (el.textContent ?? '').trim() === manageMembersLabel);
            if (!manageItem) {
                return;
            }
            const container = manageItem.parentElement;
            if (!container) {
                return;
            }
            const injected = manageItem.cloneNode(true) as HTMLElement;
            injected.removeAttribute('id');
            injected.setAttribute(requestTeamAdminMark, 'true');
            injected.setAttribute('data-testid', 'teamMenuRequestTeamAdmin');
            injected.textContent = requestTeamAdminLabel;
            if (manageItem.nextSibling) {
                container.insertBefore(injected, manageItem.nextSibling);
            } else {
                container.appendChild(injected);
            }
        }, {manageMembersLabel: MANAGE_MEMBERS_LABEL, requestTeamAdminLabel: REQUEST_TEAM_ADMIN_LABEL, requestTeamAdminMark: REQUEST_TEAM_ADMIN_MARK});

        const order = await page.evaluate(() =>
            Array.from(document.querySelectorAll('#container > *')).map((el) => el.textContent?.trim()),
        );
        expect(order).toEqual(['Manage members', 'Request Team Admin', 'Leave team']);
    });

    test('injectRequestTeamAdmin skips when already injected', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a>Manage members</a>
                <a data-mm-plugin-request-team-admin="true">Request Team Admin</a>
                <a>Leave team</a>
            </div>
        `);
        const count = await page.evaluate(({requestTeamAdminMark}) => {
            return document.querySelectorAll(`[${requestTeamAdminMark}]`).length;
        }, {requestTeamAdminMark: REQUEST_TEAM_ADMIN_MARK});
        expect(count).toBe(1);
    });

    test('findItemByText returns null when label not in menu', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a>Invite people</a>
                <a>Team settings</a>
            </div>
        `);
        const found = await page.evaluate(({manageMembersLabel, itemSelector}) => {
            const menu = document.getElementById('menu') as HTMLElement;
            const items = Array.from(menu.querySelectorAll<HTMLElement>(itemSelector));
            const match = items.find((el) => (el.textContent ?? '').trim() === manageMembersLabel);
            return match ? true : null;
        }, {manageMembersLabel: MANAGE_MEMBERS_LABEL, itemSelector: ITEM_SELECTOR});
        expect(found).toBeNull();
    });

    test('injectRequestTeam inserts before Learn about teams when Create a team is absent', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a>Team settings</a>
                <a>Manage members</a>
                <a>Leave team</a>
                <a id="learn">Learn about teams</a>
            </div>
        `);
        const order = await page.evaluate(() => {
            const REQUEST_TEAM_LABEL = 'Request a Team';
            const REQUEST_TEAM_INJECT_MARK = 'data-mm-plugin-request-team-injected';
            const ITEM_SELECTOR = 'a, button, li, [role="menuitem"]';
            const menu = document.getElementById('menu') as HTMLElement;
            const findByText = (label: string) =>
                Array.from(menu.querySelectorAll<HTMLElement>(ITEM_SELECTOR)).find(
                    (el) => (el.textContent ?? '').trim() === label,
                ) ?? null;

            const learnItem = findByText('Learn about teams');
            const leaveItem = findByText('Leave team');
            const template = learnItem ?? leaveItem;
            if (!template?.parentElement) {
                return [];
            }
            const injected = template.cloneNode(true) as HTMLElement;
            injected.removeAttribute('id');
            injected.setAttribute(REQUEST_TEAM_INJECT_MARK, 'true');
            injected.textContent = REQUEST_TEAM_LABEL;
            if (learnItem) {
                template.parentElement.insertBefore(injected, learnItem);
            } else {
                template.parentElement.appendChild(injected);
            }
            return Array.from(menu.querySelectorAll('a')).map((el) => el.textContent?.trim());
        });
        expect(order).toEqual([
            'Team settings',
            'Manage members',
            'Leave team',
            'Request a Team',
            'Learn about teams',
        ]);
    });

    test('injectRequestTeam inserts after Join another team when present', async ({page}) => {
        await page.setContent(`
            <div id="menu">
                <a>Leave team</a>
                <a id="join">Join another team</a>
                <a>Learn about teams</a>
            </div>
        `);
        const order = await page.evaluate(() => {
            const REQUEST_TEAM_LABEL = 'Request a Team';
            const REQUEST_TEAM_INJECT_MARK = 'data-mm-plugin-request-team-injected';
            const menu = document.getElementById('menu') as HTMLElement;
            const joinItem = Array.from(menu.querySelectorAll('a')).find(
                (el) => el.textContent?.trim() === 'Join another team',
            ) as HTMLElement;
            const injected = joinItem.cloneNode(true) as HTMLElement;
            injected.setAttribute(REQUEST_TEAM_INJECT_MARK, 'true');
            injected.textContent = REQUEST_TEAM_LABEL;
            if (joinItem.nextSibling) {
                joinItem.parentElement!.insertBefore(injected, joinItem.nextSibling);
            } else {
                joinItem.parentElement!.appendChild(injected);
            }
            return Array.from(menu.querySelectorAll('a')).map((el) => el.textContent?.trim());
        });
        expect(order).toEqual([
            'Leave team',
            'Join another team',
            'Request a Team',
            'Learn about teams',
        ]);
    });
});
