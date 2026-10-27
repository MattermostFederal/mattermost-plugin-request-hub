import {expect, test} from '@playwright/test';

import {makeDebug} from './debug';

const originalLocalStorage = (globalThis as {localStorage?: unknown}).localStorage;
// eslint-disable-next-line no-console
const originalDebug = console.debug;

let logged: unknown[][];

function setLocalStorage(getItem: (key: string) => string | null) {
    Object.defineProperty(globalThis, 'localStorage', {value: {getItem}, configurable: true, writable: true});
}

test.beforeEach(() => {
    logged = [];
    // eslint-disable-next-line no-console
    console.debug = (...args: unknown[]) => {
        logged.push(args);
    };
});

test.afterEach(() => {
    // eslint-disable-next-line no-console
    console.debug = originalDebug;
    Object.defineProperty(globalThis, 'localStorage', {value: originalLocalStorage, configurable: true, writable: true});
});

test('logs with the prefix when the debug flag is set', () => {
    setLocalStorage((key) => (key === 'PLUGIN_CHANNEL_REQUESTS_DEBUG' ? 'true' : null));
    makeDebug('picker')('hello', 1);
    expect(logged).toEqual([['[picker]', 'hello', 1]]);
});

test('stays quiet when the flag is not set', () => {
    setLocalStorage(() => null);
    makeDebug('picker')('hello');
    expect(logged).toEqual([]);
});

test('stays quiet without localStorage', () => {
    Object.defineProperty(globalThis, 'localStorage', {value: undefined, configurable: true, writable: true});
    makeDebug('picker')('hello');
    expect(logged).toEqual([]);
});

test('ignores a localStorage that throws', () => {
    setLocalStorage(() => {
        throw new Error('blocked');
    });
    expect(() => makeDebug('picker')('hello')).not.toThrow();
    expect(logged).toEqual([]);
});
