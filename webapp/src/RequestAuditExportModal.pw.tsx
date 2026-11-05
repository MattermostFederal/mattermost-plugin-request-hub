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

        await page.getByLabel('Reason').fill('  incident review ');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('Sent to the System Admins.')).toBeVisible();
        expect(body).toEqual({type: 'full_histories', format: 'csv', reason: 'incident review'});
    });

    test('needs a reason before it sends', async ({mount, page}) => {
        await mount(<StoreHarness pluginState={open}><RequestAuditExportModal/></StoreHarness>);
        const submit = page.getByRole('button', {name: 'Submit request'});
        await expect(submit).toBeDisabled();
        await page.getByLabel('Reason').fill('   ');
        await expect(submit).toBeDisabled();
        await page.getByLabel('Reason').fill('audit');
        await expect(submit).toBeEnabled();
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
        await page.getByLabel('Reason').fill('quarterly review');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('ok')).toBeVisible();
        expect(body).toMatchObject({type: 'full_histories', format: 'json', kind: 'bot', requester: 'alice', reason: 'quarterly review'});
        expect(body.since).toBe(new Date('2026-10-01T00:00:00').getTime());
        expect(body.until).toBe(new Date('2026-10-31T23:59:59.999').getTime());
    });

    test('asks for the bot inventory by environment, with its own formats', async ({mount, page}) => {
        let body: Record<string, unknown> = {};
        await page.route(`${API}/bot_form`, (route) => route.fulfill({
            contentType: 'application/json',
            body: JSON.stringify({compliance: {enabled: false}, hosting_envs: ['mattermost-dev', 'mattermost-prod'], privilege_levels: []}),
        }));
        await page.route(`${API}/submit_audit_export_request`, (route) => {
            body = route.request().postDataJSON();
            route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'ok'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestAuditExportModal/></StoreHarness>);

        await page.getByLabel('Request type (optional)').selectOption('bot');
        await page.getByLabel('Bot inventory').check();
        await expect(page.getByLabel('CSV (one row per bot)')).toBeChecked();
        await page.getByLabel('PDF (for reading)').check();
        await expect(page.getByLabel('Request type (optional)')).toHaveCount(0);
        await expect(page.getByLabel('Approved from (optional)')).toBeVisible();
        await page.getByLabel('Hosting environment (optional)').selectOption('mattermost-prod');
        await page.getByLabel('Reason').fill('fleet review');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('ok')).toBeVisible();
        expect(body).toEqual({type: 'bot_inventory', format: 'pdf', env: 'mattermost-prod', reason: 'fleet review'});
    });

    test('asks for one bot\'s forensics, which needs the bot and both dates', async ({mount, page}) => {
        let body: Record<string, unknown> = {};
        await page.route(`${API}/submit_audit_export_request`, (route) => {
            body = route.request().postDataJSON();
            route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'ok'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestAuditExportModal/></StoreHarness>);

        await page.getByLabel('Requested by (optional username)').fill('@alice');
        await page.getByLabel('Bot forensics').check();
        await expect(page.getByLabel('JSON', {exact: true})).toBeChecked();
        await expect(page.getByLabel('Requested by (optional username)')).toHaveCount(0);
        await expect(page.getByLabel('Request type (optional)')).toHaveCount(0);
        await expect(page.getByLabel('Hosting environment (optional)')).toHaveCount(0);
        const submit = page.getByRole('button', {name: 'Submit request'});
        await page.getByLabel('Reason').fill('incident 42');
        await expect(submit).toBeDisabled();
        await page.getByLabel('Bot', {exact: true}).fill(' @alerts-bot ');
        await expect(submit).toBeDisabled();
        await page.getByLabel('Posted from').fill('2026-10-01');
        await expect(submit).toBeDisabled();
        await page.getByLabel('Posted until').fill('2026-10-31');
        await expect(submit).toBeEnabled();
        await submit.click();

        await expect(page.getByText('ok')).toBeVisible();
        expect(body).toEqual({
            type: 'bot_forensic',
            format: 'json',
            bot: 'alerts-bot',
            since: new Date('2026-10-01T00:00:00').getTime(),
            until: new Date('2026-10-31T23:59:59.999').getTime(),
            reason: 'incident 42',
        });
    });

    test('takes a typed environment when the list can\'t be loaded', async ({mount, page}) => {
        let body: Record<string, unknown> = {};
        await page.route(`${API}/bot_form`, (route) => route.fulfill({status: 500}));
        await page.route(`${API}/submit_audit_export_request`, (route) => {
            body = route.request().postDataJSON();
            route.fulfill({contentType: 'application/json', body: JSON.stringify({message: 'ok'})});
        });
        await mount(<StoreHarness pluginState={open}><RequestAuditExportModal/></StoreHarness>);

        await page.getByLabel('Bot inventory').check();
        await page.getByLabel('JSON (one object per bot)').check();
        await page.getByLabel('Hosting environment (optional)').fill(' lab ');
        await page.getByLabel('Reason').fill('why');
        await page.getByRole('button', {name: 'Submit request'}).click();

        await expect(page.getByText('ok')).toBeVisible();
        expect(body).toEqual({type: 'bot_inventory', format: 'json', env: 'lab', reason: 'why'});
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
        await page.getByLabel('Reason').fill('why');
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
        await page.getByLabel('Reason').fill('why');
        await page.getByRole('button', {name: 'Submit request'}).click();
        await expect(page.getByRole('alert')).toHaveText('Only security approvers can request a copy of the audit history.');
    });
});
