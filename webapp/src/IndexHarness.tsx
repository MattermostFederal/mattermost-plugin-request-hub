import React, {useEffect, useState} from 'react';

/**
 * Harness for the plugin entry point.
 *
 * The entry point calls `window.registerPlugin(...)` at import time, so the
 * stub has to be in place before the module is pulled in: hence a dynamic
 * import. A Proxy registry records every method `initialize` calls. The
 * harness then runs the registered header button and every channel header
 * menu item (and its shouldRender, when it has one), and serializes
 * everything into a text node for the test.
 *
 * With `uninitialize`, `markup` is written into the page first so the DOM
 * injectors have something to work on; the harness counts their injected
 * elements, calls `uninitialize`, and counts again. `failFirstDisposer`
 * makes the first disposer throw partway through.
 */

export type RecordedMenu = {
    text: string;

    // dispatch is the action the item dispatched, or null when it did not
    // dispatch (for example because the request type is turned off).
    dispatch: unknown;

    // shouldRender maps a channel type to the item's shouldRender result.
    // Absent when the item is always shown.
    shouldRender?: Record<string, boolean>;
};

export type Recorded = {
    registeredId: string;
    called: string[];
    adminSettings: string[];
    rootComponents: string[];
    headerButtonText: string;
    headerDispatch: unknown;
    menus: RecordedMenu[];
    errors: string[];
    infos: string[];

    // injected counts the DOM injectors' elements before and after
    // uninitialize. Present only with the uninitialize prop.
    injected?: {before: InjectedCounts; after: InjectedCounts};
};

export type InjectedCounts = {
    membersPanelButton: number;
    teamRailButton: number;
};

type Props = {

    // throwOn names a registry method that throws when called.
    throwOn?: string;

    // withoutAdminConsole leaves registerAdminConsoleCustomSetting undefined,
    // like an older Mattermost.
    withoutAdminConsole?: boolean;

    markup?: string;
    uninitialize?: boolean;
    failFirstDisposer?: boolean;
};

type Fn = (...args: unknown[]) => unknown;

const countInjected = (): InjectedCounts => ({
    membersPanelButton: document.querySelectorAll('[data-cr-request-admin]').length,
    teamRailButton: document.querySelectorAll('#mmPluginRequestTeamAddButton').length,
});

const nextFrame = () => new Promise((r) => requestAnimationFrame(() => setTimeout(r, 0)));

export const IndexHarness = ({throwOn, withoutAdminConsole, markup, uninitialize, failFirstDisposer}: Props) => {
    const [result, setResult] = useState('');

    useEffect(() => {
        const run = async () => {
            const calls: Array<{name: string; args: unknown[]}> = [];
            const errors: string[] = [];
            const infos: string[] = [];
            const dispatched: unknown[] = [];

            // eslint-disable-next-line no-console
            const originalError = console.error;
            // eslint-disable-next-line no-console
            const originalInfo = console.info;
            // eslint-disable-next-line no-console
            console.error = (...args: unknown[]) => {
                errors.push(String(args[0]));
            };
            // eslint-disable-next-line no-console
            console.info = (...args: unknown[]) => {
                infos.push(String(args[0]));
            };

            const registry = new Proxy({}, {
                get: (_target, prop: string) => {
                    if (withoutAdminConsole && prop === 'registerAdminConsoleCustomSetting') {
                        return undefined;
                    }
                    return (...args: unknown[]) => {
                        calls.push({name: prop, args});
                        if (prop === throwOn) {
                            throw new Error(`${prop} unavailable`);
                        }
                    };
                },
            });
            const stateFor = (channelType: string) => ({
                entities: {
                    teams: {currentTeamId: 't1', teams: {}, myMembers: {}},
                    users: {currentUserId: 'me', profiles: {me: {roles: 'system_user'}}},
                    channels: {currentChannelId: 'c', channels: {c: {type: channelType}}},
                },
            });
            const store = {
                getState: () => stateFor('O'),
                dispatch: (action: unknown) => {
                    dispatched.push(action);
                    return action;
                },
                subscribe: () => () => undefined,
            };

            const host = document.createElement('div');
            host.innerHTML = markup ?? '';
            document.body.appendChild(host);

            let plugin: {initialize: (r: unknown, s: unknown) => Promise<void>; uninitialize: () => void} | undefined;
            let registeredId = '';
            window.registerPlugin = (id: string, p: unknown) => {
                registeredId = id;
                plugin = p as typeof plugin;
            };
            await import('./index');
            await plugin!.initialize(registry, store);

            const find = (name: string) => calls.find((c) => c.name === name);
            const header = find('registerChannelHeaderButtonAction');

            let headerDispatch: unknown = null;
            if (header) {
                (header.args[1] as Fn)();
                headerDispatch = dispatched.at(-1);
            }

            const menus: RecordedMenu[] = [];
            for (const menu of calls.filter((c) => c.name === 'registerChannelHeaderMenuAction')) {
                const before = dispatched.length;
                // eslint-disable-next-line no-await-in-loop
                await (menu.args[1] as Fn)('chan1');
                const entry: RecordedMenu = {
                    text: String(menu.args[0]),
                    dispatch: dispatched.length > before ? dispatched.at(-1) : null,
                };
                const shouldRender = menu.args[2] as Fn | undefined;
                if (shouldRender) {
                    entry.shouldRender = {};
                    for (const type of ['O', 'P', 'D', 'G', '']) {
                        entry.shouldRender[type || 'unknown'] = shouldRender(stateFor(type)) as boolean;
                    }
                }
                menus.push(entry);
            }

            let injected: Recorded['injected'];
            if (uninitialize) {
                // The injectors work on the next frame; wait until both have run.
                for (let i = 0; i < 20; i++) {
                    const counts = countInjected();
                    if (counts.membersPanelButton && counts.teamRailButton) {
                        break;
                    }
                    // eslint-disable-next-line no-await-in-loop
                    await nextFrame();
                }
                const before = countInjected();

                const disconnect = MutationObserver.prototype.disconnect;
                if (failFirstDisposer) {
                    MutationObserver.prototype.disconnect = function failOnce(this: MutationObserver) {
                        MutationObserver.prototype.disconnect = disconnect;
                        throw new Error('disconnect failed');
                    };
                }
                try {
                    plugin!.uninitialize();
                } finally {
                    MutationObserver.prototype.disconnect = disconnect;
                }
                await nextFrame();
                injected = {before, after: countInjected()};
            }
            host.remove();

            // eslint-disable-next-line no-console
            console.error = originalError;
            // eslint-disable-next-line no-console
            console.info = originalInfo;

            const recorded: Recorded = {
                registeredId,
                called: calls.map((c) => c.name),
                adminSettings: calls.filter((c) => c.name === 'registerAdminConsoleCustomSetting').map((c) => String(c.args[0])),
                rootComponents: calls.filter((c) => c.name === 'registerRootComponent').map((c) => (c.args[0] as {name: string}).name),
                headerButtonText: header ? String(header.args[2]) : '',
                headerDispatch,
                menus,
                errors,
                infos,
                injected,
            };
            setResult(JSON.stringify(recorded));
        };
        run().catch((err) => setResult(JSON.stringify({error: String(err)})));
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    return <pre data-testid='recorded'>{result}</pre>;
};
