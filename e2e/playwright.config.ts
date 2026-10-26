import {defineConfig, devices} from '@playwright/test';

// Each spec file starts its own Mattermost + Postgres containers in
// beforeAll (see helpers/requesthub.ts), so files are independent but
// heavy. One worker in CI keeps the runner's memory in check.
export default defineConfig({
    testDir: './tests',
    fullyParallel: false,
    forbidOnly: Boolean(process.env.CI),

    // No retries: a flaky e2e test should show up as a failure, not get hidden.
    retries: 0,
    workers: process.env.CI ? 1 : 2,
    reporter: [['html', {open: 'never'}], ['list']],
    timeout: 60_000,
    use: {
        trace: 'retain-on-failure',
        screenshot: 'only-on-failure',
    },
    projects: [
        {
            name: 'chromium',
            use: {...devices['Desktop Chrome']},
        },
    ],
});
