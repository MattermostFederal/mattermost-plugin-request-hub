import React from 'react';

import {DialogA11yHarness} from './DialogA11yHarness';

import {expect, test} from '../playwright/ct-coverage';

test.describe('useDialogFocusTrap', () => {
    test('moves focus into the dialog and gives it back on close', async ({mount, page}) => {
        await mount(<DialogA11yHarness/>);
        const opener = page.getByRole('button', {name: 'Open'});
        await opener.click();
        await expect(page.getByLabel('first')).toBeFocused();

        await page.keyboard.press('Escape');
        await expect(page.getByRole('dialog')).toBeHidden();
        await expect(page.getByText('closed 1')).toBeVisible();
        await expect(opener).toBeFocused();
    });

    test('opens cleanly when an SVG held focus', async ({mount, page}) => {
        await mount(<DialogA11yHarness/>);
        await page.getByLabel('outside svg').focus();
        await page.getByRole('button', {name: 'Open'}).evaluate((el: HTMLButtonElement) => el.click());
        await expect(page.getByLabel('first')).toBeFocused();
        await page.keyboard.press('Escape');
        await expect(page.getByRole('dialog')).toBeHidden();
    });

    test('Tab wraps from the last control to the first, Shift+Tab the other way', async ({mount, page}) => {
        await mount(<DialogA11yHarness/>);
        await page.getByRole('button', {name: 'Open'}).click();
        await expect(page.getByLabel('first')).toBeFocused();

        await page.keyboard.press('Shift+Tab');
        await expect(page.getByRole('button', {name: 'Swap'})).toBeFocused();
        await page.keyboard.press('Tab');
        await expect(page.getByLabel('first')).toBeFocused();

        // A middle control tabs normally.
        await page.keyboard.press('Tab');
        await expect(page.getByLabel('swallows escape')).toBeFocused();
        await page.keyboard.press('Shift+Tab');
        await expect(page.getByLabel('first')).toBeFocused();
    });

    test('Tab from the dialog itself lands on the first or last control', async ({mount, page}) => {
        await mount(<DialogA11yHarness/>);
        await page.getByRole('button', {name: 'Open'}).click();
        await expect(page.getByLabel('first')).toBeFocused();

        // Focus the dialog root: it is inside the dialog but not a stop, so
        // Tab moves on normally from there.
        await page.getByRole('dialog').focus();
        await page.keyboard.press('Tab');
        await expect(page.getByLabel('first')).toBeFocused();

        // Focus outside the dialog (but keys still reach its handler).
        await page.getByRole('dialog').evaluate((dialog) => {
            const outside = document.querySelector<HTMLElement>('svg[aria-label="outside svg"]')!;
            outside.focus();
            dialog.dispatchEvent(new KeyboardEvent('keydown', {key: 'Tab', bubbles: true, cancelable: true}));
        });
        await expect(page.getByLabel('first')).toBeFocused();
        await page.getByRole('dialog').evaluate((dialog) => {
            const outside = document.querySelector<HTMLElement>('svg[aria-label="outside svg"]')!;
            outside.focus();
            dialog.dispatchEvent(new KeyboardEvent('keydown', {key: 'Tab', shiftKey: true, bubbles: true, cancelable: true}));
        });
        await expect(page.getByRole('button', {name: 'Swap'})).toBeFocused();
    });

    test('ignores other keys and a Tab a child already handled', async ({mount, page}) => {
        await mount(<DialogA11yHarness/>);
        await page.getByRole('button', {name: 'Open'}).click();
        await expect(page.getByLabel('first')).toBeFocused();
        await page.getByLabel('first').press('a');
        await expect(page.getByLabel('first')).toHaveValue('a');

        const prevented = await page.getByRole('dialog').evaluate((dialog) => {
            const swap = dialog.querySelector('button')!;
            swap.focus();
            swap.addEventListener('keydown', (e) => e.preventDefault(), {once: true});
            const ev = new KeyboardEvent('keydown', {key: 'Tab', bubbles: true, cancelable: true});
            swap.dispatchEvent(ev);
            return ev.defaultPrevented;
        });
        expect(prevented).toBe(true);
        await expect(page.getByRole('button', {name: 'Swap'})).toBeFocused();
    });

    test('Escape a child already handled does not close', async ({mount, page}) => {
        await mount(<DialogA11yHarness/>);
        await page.getByRole('button', {name: 'Open'}).click();
        await expect(page.getByLabel('first')).toBeFocused();
        await page.getByLabel('swallows escape').press('Escape');
        await expect(page.getByRole('dialog')).toBeVisible();
        await expect(page.getByText('closed 0')).toBeVisible();
    });

    test('reclaims focus when the form is swapped out', async ({mount, page}) => {
        await mount(<DialogA11yHarness/>);
        await page.getByRole('button', {name: 'Open'}).click();
        await expect(page.getByLabel('first')).toBeFocused();
        await page.getByRole('button', {name: 'Swap'}).click();
        await expect(page.getByRole('button', {name: 'Done'})).toBeFocused();

        // Tab with a single control stays on it.
        await page.keyboard.press('Tab');
        await expect(page.getByRole('button', {name: 'Done'})).toBeFocused();
    });

    test('an empty dialog takes focus itself and lets Tab through', async ({mount, page}) => {
        await mount(<DialogA11yHarness empty={true}/>);
        await page.getByRole('button', {name: 'Open'}).click();
        await expect(page.getByRole('dialog')).toBeFocused();

        // With nothing to cycle through, the trap leaves Tab to the browser.
        await page.keyboard.press('Tab');
        await expect(page.getByRole('dialog')).not.toBeFocused();

        await page.getByRole('dialog').focus();
        await page.keyboard.press('Escape');
        await expect(page.getByText('closed 1')).toBeVisible();
    });
});
