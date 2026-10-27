import React, {useEffect, useState} from 'react';
import type {Store} from 'redux';

import {installChannelCreationOverride} from './channelCreationOverride';
import {installIntegrationsPageOverride} from './integrationsPageOverride';
import {installMembersPanelButton} from './membersPanelButton';
import {installTeamMenuOverride, installTeamSidebarAddOverride} from './teamMenuOverride';

/**
 * Harness for the DOM injectors.
 *
 * Mattermost's own markup is replaced by `markup`, written into the page
 * before the injector is installed with a fake store. Every dispatched
 * action, and every Escape keydown the injectors send to close a menu, is
 * recorded on `window.__harness` so the test can read it with
 * page.evaluate. `window.__harness.addMarkup(html)` appends more markup
 * later, to exercise the injectors' MutationObserver.
 * `window.__harness.setState(state)` swaps the store state and notifies
 * subscribers, and `window.__harness.dispose()` runs the injector's disposer
 * as Plugin.uninitialize would.
 */

export type HarnessRecord = {
    actions: Array<{type: string; channelId?: string}>;
    escapes: number;
};

type Props = {
    injector: 'channelCreation' | 'membersPanel' | 'teamMenu' | 'teamSidebarAdd' | 'integrationsPage';
    markup: string;
    state: unknown;
};

declare global {
    interface Window {
        __harness?: HarnessRecord & {
            addMarkup: (html: string) => void;
            setState: (state: unknown) => void;
            dispose: () => void;
        };
    }
}

export const DomInjectorHarness = ({injector, markup, state}: Props) => {
    const [ready, setReady] = useState(false);

    useEffect(() => {
        const host = document.createElement('div');
        host.innerHTML = markup;
        document.body.appendChild(host);

        let current = state;
        const listeners: Array<() => void> = [];
        const extras: HTMLElement[] = [];
        let disposer: () => void = () => undefined;
        let disposed = false;
        const dispose = () => {
            if (!disposed) {
                disposed = true;
                disposer();
            }
        };

        const record: HarnessRecord = {actions: [], escapes: 0};
        // eslint-disable-next-line no-underscore-dangle
        window.__harness = {
            ...record,
            addMarkup: (html: string) => {
                const extra = document.createElement('div');
                extra.innerHTML = html;
                document.body.appendChild(extra);
                extras.push(extra);
            },
            setState: (next: unknown) => {
                current = next;
                [...listeners].forEach((l) => l());
            },
            dispose,
        };
        const onKeyDown = (e: KeyboardEvent) => {
            if (e.key === 'Escape') {
                // eslint-disable-next-line no-underscore-dangle
                window.__harness!.escapes++;
            }
        };
        document.addEventListener('keydown', onKeyDown);

        const store = {
            getState: () => current,
            dispatch: (action: {type: string}) => {
                // eslint-disable-next-line no-underscore-dangle
                window.__harness!.actions.push(action);
                return action;
            },
            subscribe: (listener: () => void) => {
                listeners.push(listener);
                return () => {
                    const i = listeners.indexOf(listener);
                    if (i >= 0) {
                        listeners.splice(i, 1);
                    }
                };
            },
        } as unknown as Store;

        const installers = {
            channelCreation: installChannelCreationOverride,
            membersPanel: installMembersPanelButton,
            teamMenu: installTeamMenuOverride,
            teamSidebarAdd: installTeamSidebarAddOverride,
            integrationsPage: installIntegrationsPageOverride,
        };
        disposer = installers[injector](store);
        setReady(true);

        return () => {
            dispose();
            document.removeEventListener('keydown', onKeyDown);
            host.remove();
            extras.forEach((el) => el.remove());
            // eslint-disable-next-line no-underscore-dangle
            delete window.__harness;
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    return ready ? <div data-testid='harness-ready'/> : null;
};
