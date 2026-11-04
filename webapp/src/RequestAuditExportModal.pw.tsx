import React from 'react';

import {RequestAuditExportModal} from './RequestAuditExportModal';
import {StoreHarness} from './StoreHarness';

import {expect, test} from '../playwright/ct-coverage';
import {API} from '../playwright/ct-helpers';

const open = {auditExportRequestModalOpen: true};

test.describe('RequestAuditExportModal', () => {
    test('renders nothing while closed', async ({mount, page}) => {
        await mount(<StoreHarness><RequestAuditExportModal/></StoreHarness>);
        await expect(page.getByText('Request Audit Export')).toBeHidden();
    });

    test('sends the whole history by default', async ({mount, page}) => {
        let body: Record<string, unknown> = {};
        await page.route(`${API}/submit_audit_export_request`, (route) => {
            body = route.request().postDataJSON();
            route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'Sent to the System Admins.'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestAuditExportModal/></StoreHarness>);

        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent to the System Admins.')).toBeVisible();
        expect(body).toEqual({format: 'csv'});
    });

    test('sends the filters asked for', async ({mount, page}) => {
        let body: Record<string, unknown> = {};
        await page.route(`${API}/submit_audit_export_request`, (route) => {
            body = route.request().postDataJSON();
            route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'ok'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestAuditExportModal/></StoreHarness>);

        await page.getByLabel(/JSON/).check();
        await page.getByLabel('Request type (optional)').selectOption('bot');
        await page.getByLabel('Requested by (optional username)').fill('@alice');
        await page.getByLabel('Active from (optional)').fill('2026-10-01');
        await page.getByLabel('Active until (optional)').fill('2026-10-31');
        await page.getByLabel('Reason (optional)').fill('quarterly review');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('ok')).toBeVisible();
        expect(body).toMatchObject({format: 'json', kind: 'bot', requester: 'alice', reason: 'quarterly review'});
        expect(body.since).toBe(new Date('2026-10-01T00:00:00').getTime());
        expect(body.until).toBe(new Date('2026-10-31T23:59:59.999').getTime());
    });

    test('refuses dates out of order before sending', async ({mount, page}) => {
        let sent = false;
        await page.route(`${API}/submit_audit_export_request`, (route) => {
            sent = true;
            route.fulfill({contentType: 'application/json', body: '{}'});
        });
        await mount(<StoreHarness pluginState={open}><RequestAuditExportModal/></StoreHarness>);
        await page.getByLabel('Active from (optional)').fill('2026-10-31');
        await page.getByLabel('Active until (optional)').fill('2026-10-01');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('The start date must be before the end date.');
        expect(sent).toBe(false);
    });

    test('shows the server\'s refusal', async ({mount, page}) => {
        await page.route(`${API}/submit_audit_export_request`, (route) => route.fulfill({
            contentType: 'application/json',
            body: JSON.stringify({error: 'Only security approvers can request a copy of the audit history.'}),
        }));
        await mount(<StoreHarness pluginState={open}><RequestAuditExportModal/></StoreHarness>);
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Only security approvers can request a copy of the audit history.');
    });
});
