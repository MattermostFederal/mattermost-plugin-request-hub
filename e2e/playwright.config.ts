import {defineConfig, devices} from '@playwright/test';

// Each worker starts one Mattermost + Postgres pair and every spec file it
// runs shares it (see helpers/fixtures.ts). Tests in a file run in order;
// files are spread over workers locally and over shards in CI. One worker
// per CI job keeps the runner's memory in check.
export default defineConfig({
    testDir: './tests',
    fullyParallel: false,
    forbidOnly: Boolean(process.env.CI),

    // No retries: a flaky e2e test should show up as a failure, not get hidden.
    retries: 0,
    workers: process.env.CI ? 1 : 2,

    // CI shards write blob reports that the workflow merges into one HTML report.
    reporter: process.env.CI ? [['blob'], ['list']] : [['html', {open: 'never'}], ['list']],
    timeout: 60_000,
    use: {
        trace: 'retain-on-failure',
        screenshot: 'only-on-failure',
    },
    projects: [
        {
            // CI runs the suite once per server image; naming the project
            // after it keeps the runs apart in the merged report.
            name: process.env.E2E_SERVER ? `chromium (${process.env.E2E_SERVER})` : 'chromium',
            use: {...devices['Desktop Chrome']},
        },
    ],
});
