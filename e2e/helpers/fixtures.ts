// Playwright fixtures: one Mattermost server per worker, shared by every
// spec file that worker runs, and a per-test check that the plugin logged
// no errors.
//
// A spec that needs its own server (activation, bootstrap, migration, or
// anything that changes global state for good) calls
//     test.use({hubOptions: {name: 'bootstrap', pluginConfig: {...}}});
// at the top of the file. Playwright then runs that file in a worker of its
// own, which starts a fresh server.
import {test as base, expect} from '@playwright/test';

import {DEFAULT_PLUGIN_CONFIG, startRequestHub, type PluginConfig, type RequestHub} from './requesthub';

export type HubOptions = {
    name: string;
    pluginConfig: PluginConfig;
};

type WorkerFixtures = {
    hubOptions: HubOptions;
    hub: RequestHub;
};

type TestFixtures = {

    // Plugin error log lines a test expects; anything else fails the test.
    allowedPluginErrors: RegExp[];
    noPluginErrors: void;
};

export const test = base.extend<TestFixtures, WorkerFixtures>({
    hubOptions: [{name: 'shared', pluginConfig: DEFAULT_PLUGIN_CONFIG}, {scope: 'worker', option: true}],

    hub: [async ({hubOptions}, use, workerInfo) => {
        const hub = await startRequestHub(`${hubOptions.name}-w${workerInfo.parallelIndex}`, hubOptions.pluginConfig);
        try {
            // Errors logged while the plugin activated fall before any
            // test's window, so check them here.
            const bootErrors = hub.pluginErrors();
            if (bootErrors.length > 0) {
                throw new Error(`plugin logged errors while starting:\n${bootErrors.join('\n')}`);
            }
            await use(hub);
        } finally {
            await hub.stop();
        }
    }, {scope: 'worker', timeout: 6 * 60_000}], // image pull + server boot

    allowedPluginErrors: [[], {option: true}],

    noPluginErrors: [async ({hub, allowedPluginErrors}, use) => {
        const start = hub.logLineCount();
        await use();

        // Give the log consumer a moment to write the test's last lines.
        await new Promise((resolve) => setTimeout(resolve, 250));
        const errors = hub.pluginErrors(start).filter((line) => !allowedPluginErrors.some((re) => re.test(line)));
        expect(errors, 'plugin error log lines').toEqual([]);
    }, {auto: true}],
});

export {expect};
