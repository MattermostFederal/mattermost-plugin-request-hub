import Module from 'module';
import path from 'path';

import {defineConfig} from '@playwright/test';

// eslint-disable-next-line no-underscore-dangle
const originalResolveFilename = (Module as any)._resolveFilename;
// eslint-disable-next-line no-underscore-dangle, func-names, space-before-function-paren
(Module as any)._resolveFilename = function(request: string, parent: any, ...rest: any[]) {
    if (request === 'manifest') {
        return path.resolve(__dirname, 'src', 'manifest.ts');
    }
    return originalResolveFilename.call(this, request, parent, ...rest);
};

export default defineConfig({
    testMatch: '**/*.spec.ts',
    testDir: './src',
    use: {
        launchOptions: {executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined},
    },
});
