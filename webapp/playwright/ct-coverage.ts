// Custom Playwright CT fixture that collects V8 code coverage from Chromium.
//
// Playwright Component Tests run component code in the browser, so Node-side
// C8 instrumentation cannot see it. This fixture uses the CDP Coverage API
// (page.coverage) to capture V8 coverage per test, writes raw V8 JSON (with
// an embedded source-map-cache) to a temp directory. After the run finishes,
// c8 report reads these files and produces the final coverage report.
//
// Usage: import { test, expect } from '../playwright/ct-coverage';

import {test as ctBase, expect} from '@playwright/experimental-ct-react';
import * as crypto from 'crypto';
import * as fs from 'fs';
import * as path from 'path';

const WEBAPP_ROOT = path.resolve(__dirname, '..');
const COVERAGE_DIR = path.join(WEBAPP_ROOT, '.v8-ct-coverage');

// Rewrite relative paths in a source map's "sources" array to absolute
// file:// URIs rooted at the webapp directory. Vite emits paths like
// "../../../src/components/Foo.tsx" relative to its build output; we
// resolve them against the webapp root so c8 can find them. Every entry is
// kept because "mappings" refers to sources by index; c8 drops node_modules
// sources after remapping (--exclude-after-remap).
function fixSourceMap(mapJson: string): Record<string, unknown> {
    const map = JSON.parse(mapJson);
    if (Array.isArray(map.sources)) {
        map.sources = map.sources.map((s: string) => 'file://' + path.resolve(WEBAPP_ROOT, s.replace(/^(\.\.\/)+/, '')));
    }
    delete map.sourceRoot;
    return map;
}

// Compute line lengths array that c8 uses for offset calculations.
function lineLengths(source: string): number[] {
    return source.split('\n').map((line) => line.length);
}

// CI skips collection: the CDP coverage round-trip roughly doubles the CT run,
// and the merged report there would reflect unit tests only. Read a merged
// number from a local run, not from CI.
const COLLECT_COVERAGE = !process.env.CI;

export const test = ctBase.extend({
    page: async ({page}, use) => {
        if (COLLECT_COVERAGE) {
            await page.coverage.startJSCoverage({resetOnNavigation: false});
        }
        await use(page);
        if (!COLLECT_COVERAGE) {
            return;
        }
        const entries = await page.coverage.stopJSCoverage();

        // Keep only Vite asset bundles that contain our project source.
        // Skip the React JSX runtime which is just framework glue.
        const filtered = entries.filter(
            (e) => e.url.includes('/assets/') && !e.url.includes('jsx-runtime'),
        );

        if (filtered.length === 0) {
            return;
        }

        fs.mkdirSync(COVERAGE_DIR, {recursive: true});

        const sourceMapCache: Record<string, unknown> = {};
        const rewritten = [];

        for (const entry of filtered) {
            // Use a file:// URL so c8 can match it against source-map-cache keys.
            const basename = new URL(entry.url).pathname.split('/').pop()!;
            const fileUrl = 'file://' + path.join(COVERAGE_DIR, basename);

            // Fetch source map from the still-running Vite dev server.
            try {
                const resp = await page.request.get(entry.url + '.map');
                if (resp.ok()) {
                    const mapData = fixSourceMap(await resp.text());
                    sourceMapCache[fileUrl] = {
                        lineLengths: lineLengths(entry.source ?? ''),
                        data: mapData,
                        url: fileUrl + '.map',
                    };
                }
            } catch {
                // Vite may have shut down; skip source map for this entry.
            }

            rewritten.push({
                scriptId: String(entry.scriptId),
                url: fileUrl,
                functions: entry.functions,
            });
        }

        const id = crypto.randomUUID();
        fs.writeFileSync(
            path.join(COVERAGE_DIR, `coverage-${id}.json`),
            JSON.stringify({
                result: rewritten,
                timestamp: Date.now(),
                'source-map-cache': sourceMapCache,
            }),
        );
    },
});

export {expect};
