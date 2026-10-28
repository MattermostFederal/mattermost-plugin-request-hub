import React from 'react';

import {HeaderIcon} from './HeaderIcon';

import {expect, test} from '../playwright/ct-coverage';

test('HeaderIcon renders the plug icon', async ({mount, page}) => {
    await mount(<HeaderIcon/>);
    await expect(page.locator('i')).toHaveClass(/fa-plug/);
});
