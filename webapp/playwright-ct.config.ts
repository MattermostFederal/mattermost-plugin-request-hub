import path from 'path';

import {defineConfig, devices} from '@playwright/experimental-ct-react';

export default defineConfig({
    testMatch: '**/*.pw.tsx',
    testDir: './src',
    retries: process.env.CI ? 1 : 0,
    use: {
        ctViteConfig: {
            resolve: {
                alias: {
                    manifest: path.resolve(__dirname, 'src/manifest.ts'),
                },
            },
        },
    },
    projects: [
        {
            name: 'chromium',
            use: {
                ...devices['Desktop Chrome'],
                launchOptions: {executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined},
            },
        },
    ],
});
