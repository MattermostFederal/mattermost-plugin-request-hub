import type {Page} from '@playwright/test';
import React from 'react';

import {AuditHistoryModal} from './AuditHistoryModal';
import manifest from './manifest';
import {RequestAuditExportModal} from './RequestAuditExportModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const open = {auditHistoryModalOpen: true};
const EXPORT = `/plugins/${manifest.id}/api/v1/audit/export`;

const ROWS = [
    {request_id: 'r1', kind: 'bot', kind_label: 'bot account', title: 'svcbot', status: 'approved', requester_id: 'u1', requester_username: 'alice', created_at: 1, updated_at: 3, event_count: 3, last_event: 'approved'},
    {request_id: 'r2', kind: 'channel', kind_label: 'channel', title: 'Ops Room', status: 'pending', requester_id: 'u2', requester_username: 'bob', created_at: 2, updated_at: 2, event_count: 1, last_event: 'submitted'},
];

// routeAccess answers the access check, with direct export on or off.
async function routeAccess(page: Page, directExport: boolean) {
    await page.route(`${API}/audit/access`, (route) => route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({can_view: true, can_request: !directExport, direct_export: directExport}),
    }));
}

// routeList answers the list with ROWS (or rows) and records each query.
// Direct export is on unless a test routes the access check itself.
async function routeList(page: Page, rows = ROWS, total = rows.length): Promise<URLSearchParams[]> {
    await routeAccess(page, true);
    const queries: URLSearchParams[] = [];
    await page.route(`${API}/audit/requests**`, (route) => {
        const params = new URL(route.request().url()).searchParams;
        queries.push(params);
        const start = Number(params.get('page') ?? 0) * 25;
        route.fulfill({contentType: 'application/json', body: JSON.stringify({total, requests: rows.slice(start, start + 25)})});
    });
    return queries;
}

test.describe('AuditHistoryModal', () => {
    test('renders nothing while closed', async ({mount, page}) => {
        await mount(<StoreHarness><AuditHistoryModal/></StoreHarness>);
        await expect(page.getByText('Audit history')).toBeHidden();
    });

    test('lists the requests with download links for the whole history', async ({mount, page}) => {
        await routeList(page);
        await mount(<StoreHarness pluginState={open}><AuditHistoryModal/></StoreHarness>);

        await expect(page.getByText('2 requests')).toBeVisible();
        await expect(page.getByRole('row')).toHaveCount(3);
        await expect(page.getByRole('row').nth(1)).toContainText('svcbot');
        await expect(page.getByRole('row').nth(1)).toContainText('@alice');
        await expect(page.getByRole('link', {name: 'Download CSV'})).toHaveAttribute('href', `${EXPORT}?format=csv`);
        await expect(page.getByRole('link', {name: 'Download JSON'})).toHaveAttribute('href', `${EXPORT}?format=json`);
        await expect(page.getByRole('link', {name: 'Download inventory PDF'})).toHaveAttribute('href', `${EXPORT}?type=bot_inventory&format=pdf`);
        await expect(page.getByRole('link', {name: 'Download inventory CSV'})).toHaveAttribute('href', `${EXPORT}?type=bot_inventory&format=csv`);
        await expect(page.getByRole('link', {name: 'Download inventory JSON'})).toHaveAttribute('href', `${EXPORT}?type=bot_inventory&format=json`);
    });

    test('offers a request instead of downloads while direct export is off', async ({mount, page}) => {
        await routeList(page);
        await page.unroute(`${API}/audit/access`);
        await routeAccess(page, false);
        await mount(<StoreHarness pluginState={open}><AuditHistoryModal/><RequestAuditExportModal/></StoreHarness>);

        await expect(page.getByText('Direct downloads are off. Another System Admin approves each export.')).toBeVisible();
        await expect(page.getByRole('link', {name: 'Download CSV'})).toHaveCount(0);
        await expect(page.getByRole('link', {name: 'Download inventory PDF'})).toHaveCount(0);
        await page.getByRole('button', {name: 'Request an export'}).click();
        await expect(page.getByRole('heading', {name: 'Request Audit Export'})).toBeVisible();
        await expect(page.getByRole('heading', {name: 'Audit history'})).toBeHidden();
    });

    test('search, type and status narrow the list and the download', async ({mount, page}) => {
        const queries = await routeList(page);
        await mount(<StoreHarness pluginState={open}><AuditHistoryModal/></StoreHarness>);
        await expect(page.getByText('2 requests')).toBeVisible();

        await page.getByLabel('Search').fill('alice');
        await expect.poll(() => queries.at(-1)?.get('q')).toBe('alice');
        await page.getByLabel('Type').selectOption('bot');
        await expect.poll(() => queries.at(-1)?.get('kind')).toBe('bot');
        await page.getByLabel('Status').selectOption('approved');
        await expect.poll(() => queries.at(-1)?.get('status')).toBe('approved');
        await expect(page.getByRole('link', {name: 'Download CSV'})).toHaveAttribute('href', `${EXPORT}?format=csv&kind=bot`);
    });

    test('loads more pages', async ({mount, page}) => {
        const many = Array.from({length: 30}, (_, i) => ({...ROWS[0], request_id: `r${i}`, title: `bot${i}`}));
        await routeList(page, many);
        await mount(<StoreHarness pluginState={open}><AuditHistoryModal/></StoreHarness>);
        await expect(page.getByRole('row')).toHaveCount(26);
        await page.getByRole('button', {name: 'Load more'}).click();
        await expect(page.getByRole('row')).toHaveCount(31);
        await expect(page.getByRole('button', {name: 'Load more'})).toBeHidden();
    });

    test('shows a request\'s timeline, with messages and details', async ({mount, page}) => {
        await routeList(page);
        await page.route(`${API}/audit/request?id=r1`, (route) => route.fulfill({
            contentType: 'application/json',
            body: JSON.stringify({
                summary: ROWS[0],
                requester_username: 'alice',
                history: {
                    request_id: 'r1',
kind: 'bot',
status: 'approved',
created_at: 1,
updated_at: 3,
                    events: [
                        {at: 1, event: 'submitted', actor_id: 'u1', actor_name: 'alice', outcome: 'success', note: '@alice submitted a bot account request'},
                        {at: 2, event: 'message_relayed', actor_id: 'u1', actor_name: 'alice', outcome: 'success', note: '@alice wrote in a bot account ticket thread', detail: {message: 'please hurry', direction: 'requester_to_approvers'}},
                        {at: 3, event: 'decision_refused', actor_id: 'u9', outcome: 'fail', note: 'User u9 could not approve', detail: {reason: 'no permission'}},
                    ],
                },
            }),
        }));
        await mount(<StoreHarness pluginState={open}><AuditHistoryModal/></StoreHarness>);

        await page.getByRole('button', {name: 'svcbot'}).click();

        await expect(page.getByText('bot account request: svcbot')).toBeVisible();
        const events = page.getByRole('list', {name: 'Events'}).getByRole('listitem');
        await expect(events).toHaveCount(3);
        await expect(events.nth(0)).toContainText('Submitted');
        await expect(events.nth(1).locator('blockquote')).toHaveText('please hurry');
        await expect(events.nth(2)).toContainText('failed');
        await events.nth(2).getByText('Details').click();
        await expect(events.nth(2).locator('pre')).toContainText('no permission');
        await expect(page.getByRole('link', {name: 'Download CSV'})).toHaveAttribute('href', `${EXPORT}?format=csv&request=r1`);

        await page.getByRole('button', {name: '← All requests'}).click();
        await expect(page.getByText('2 requests')).toBeVisible();
    });

    test('says when the history cannot load', async ({mount, page}) => {
        await page.route(`${API}/audit/requests**`, (route) => route.fulfill({status: 403, body: 'only System Admins can read the audit history'}));
        await mount(<StoreHarness pluginState={open}><AuditHistoryModal/></StoreHarness>);
        await expect(page.getByRole('alert')).toHaveText('Could not load the audit history: only System Admins can read the audit history');
    });

    test('reopening after Load more starts again from the first page', async ({mount, page}) => {
        const many = Array.from({length: 30}, (_, i) => ({...ROWS[0], request_id: `r${i}`, title: `bot${i}`}));
        await routeList(page, many);
        await mount(<StoreHarness pluginState={open}><AuditHistoryModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Load more'}).click();
        await expect(page.getByRole('row')).toHaveCount(31);
        await page.keyboard.press('Escape');
        await expect(page.getByRole('dialog')).toBeHidden();

        await page.getByRole('button', {name: 'harness: open audit history'}).click();
        await expect(page.getByRole('row')).toHaveCount(26);
        await expect(page.getByRole('button', {name: 'Load more'})).toBeVisible();
    });

    test('Escape and the close button close it', async ({mount, page}) => {
        await routeList(page);
        await mount(<StoreHarness pluginState={open}><AuditHistoryModal/></StoreHarness>);
        await expect(page.getByText('2 requests')).toBeVisible();
        await page.keyboard.press('Escape');
        await expect(page.getByRole('dialog')).toBeHidden();
        await page.getByRole('button', {name: 'harness: open audit history'}).click();
        await page.getByRole('button', {name: 'Close'}).click();
        await expect(page.getByRole('dialog')).toBeHidden();
    });
});
