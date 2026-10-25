import type {Store} from 'redux';

import {fetchEnabledRequestTypes} from './client';
import type {EnabledRequestTypes} from './client';
import {makeDebug} from './debug';
import {
    openBotRequestModal,
    openIncomingWebhookModal,
    openOutgoingWebhookModal,
} from './store';
import type {GlobalState} from './store';

// installIntegrationsPageOverride:
// 1) Intercepts clicks on native Integrations links (Incoming/Outgoing/Bots)
//    for non–System Admins and opens the request modal instead.
// 2) Injects "Request …" cards on the Integrations landing page when Mattermost
//    hides the native tiles (users without manage_*_webhooks / manage_bots).
//    That commonly happens on secondary teams where the user is only a member.
//
// System Admins are never intercepted or injected for — they get the native UI.
// Returns a disposer (call from Plugin.uninitialize).

const debug = makeDebug('integrations-page');
const INJECT_MARKER = 'data-request-hub-integrations';

type IntegrationKind = 'bot' | 'incoming' | 'outgoing';

function isSystemAdmin(store: Store): boolean {
    const state = store.getState() as GlobalState;
    const users = state?.entities?.users;
    const uid = users?.currentUserId ?? '';
    const roles = (uid ? users?.profiles?.[uid]?.roles : '') ?? '';
    return roles.split(' ').includes('system_admin');
}

function resolveSameOriginPath(href: string): string | null {
    if (!href || href.startsWith('#')) {
        return null;
    }
    // eslint-disable-next-line no-script-url -- reject javascript: hrefs before URL parse
    if (href.toLowerCase().startsWith('javascript:')) {
        return null;
    }
    try {
        const url = new URL(href, window.location.origin);
        if (url.origin !== window.location.origin) {
            return null;
        }
        return url.pathname;
    } catch {
        return null;
    }
}

// detectIntegrationKind only matches *create* routes. List/edit/regenerate
// pages must keep native navigation for users who already have manage_* rights
// (or who landed on a list via a deep link).
function detectIntegrationKind(pathname: string): IntegrationKind | null {
    if (pathname.includes('/integrations/add_incoming_webhook')) {
        return 'incoming';
    }
    if (pathname.includes('/integrations/add_outgoing_webhook')) {
        return 'outgoing';
    }
    if (pathname.includes('/integrations/add_bot_account')) {
        return 'bot';
    }
    return null;
}

function shouldIntercept(kind: IntegrationKind, enabled: EnabledRequestTypes): boolean {
    if (kind === 'bot') {
        return enabled.bot;
    }
    return enabled.webhook;
}

function isIntegrationsLandingPath(pathname: string): boolean {
    // /{team}/integrations with optional trailing slash — not a sub-page.
    return (/\/integrations\/?$/).test(pathname);
}

function nativeTilePresent(kind: IntegrationKind): boolean {
    const body = document.body?.innerText ?? '';
    switch (kind) {
    case 'incoming':
        return (/Incoming Webhooks/i).test(body);
    case 'outgoing':
        return (/Outgoing Webhooks/i).test(body);
    case 'bot':
        return (/Bot Accounts/i).test(body);
    default: {
        const exhaustiveCheck: never = kind;
        return exhaustiveCheck;
    }
    }
}

function openKind(store: Store, kind: IntegrationKind): void {
    if (kind === 'incoming') {
        store.dispatch(openIncomingWebhookModal());
        return;
    }
    if (kind === 'outgoing') {
        store.dispatch(openOutgoingWebhookModal());
        return;
    }
    store.dispatch(openBotRequestModal());
}

function makeCard(kind: IntegrationKind, title: string, description: string, onClick: () => void): HTMLButtonElement {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.setAttribute(INJECT_MARKER, kind);
    btn.setAttribute('data-testid', `request-hub-integrations-${kind}`);
    btn.style.cssText = [
        'display:flex',
        'flex-direction:column',
        'gap:6px',
        'text-align:left',
        'padding:16px 18px',
        'margin:0 12px 12px 0',
        'min-width:220px',
        'max-width:280px',
        'border:1px solid rgba(61,60,64,0.16)',
        'border-radius:6px',
        'background:var(--center-channel-bg,#fff)',
        'color:var(--center-channel-color,#3d3c40)',
        'cursor:pointer',
        'font:inherit',
    ].join(';');
    const titleEl = document.createElement('strong');
    titleEl.textContent = title;
    titleEl.style.fontSize = '14px';
    const descEl = document.createElement('span');
    descEl.textContent = description;
    descEl.style.cssText = 'font-size:13px;opacity:0.72;line-height:1.4';
    btn.append(titleEl, descEl);
    btn.addEventListener('click', (e) => {
        e.preventDefault();
        e.stopPropagation();
        onClick();
    });
    return btn;
}

function findInjectHost(): HTMLElement | null {
    // Prefer the main Integrations content column.
    const candidates = [
        document.querySelector<HTMLElement>('.backstage-content'),
        document.querySelector<HTMLElement>('.BackstageContent'),
        document.querySelector<HTMLElement>('[class*="backstage"] .content'),
        document.querySelector<HTMLElement>('main'),
    ];
    for (const el of candidates) {
        if (el && el.isConnected) {
            return el;
        }
    }
    return null;
}

function injectRequestCards(store: Store, enabled: EnabledRequestTypes): void {
    if (isSystemAdmin(store) || !isIntegrationsLandingPath(window.location.pathname)) {
        document.querySelectorAll(`[${INJECT_MARKER}]`).forEach((n) => n.remove());
        return;
    }

    const host = findInjectHost();
    if (!host) {
        return;
    }

    let row = host.querySelector<HTMLElement>(`[${INJECT_MARKER}="row"]`);
    if (!row) {
        row = document.createElement('div');
        row.setAttribute(INJECT_MARKER, 'row');
        row.style.cssText = 'display:flex;flex-wrap:wrap;margin:16px 0 8px;';

        // Place after the Integrations heading / intro when possible.
        const heading = host.querySelector('h1, h2');
        if (heading?.parentElement === host) {
            heading.insertAdjacentElement('afterend', row);
        } else {
            host.prepend(row);
        }
    }

    const specs: Array<{kind: IntegrationKind; title: string; description: string; on: boolean}> = [
        {
            kind: 'incoming',
            title: 'Request Incoming Webhook',
            description: 'Ask to create an incoming webhook for a channel on this team.',
            on: enabled.webhook && !nativeTilePresent('incoming'),
        },
        {
            kind: 'outgoing',
            title: 'Request Outgoing Webhook',
            description: 'Ask to create an outgoing webhook for a channel on this team.',
            on: enabled.webhook && !nativeTilePresent('outgoing'),
        },
        {
            kind: 'bot',
            title: 'Request a Bot Account',
            description: 'Ask to create a bot (optional token and webhooks).',
            on: enabled.bot && !nativeTilePresent('bot'),
        },
    ];

    for (const spec of specs) {
        const existing = row.querySelector(`[${INJECT_MARKER}="${spec.kind}"]`);
        if (!spec.on) {
            existing?.remove();
            continue;
        }
        if (existing) {
            continue;
        }
        row.appendChild(makeCard(spec.kind, spec.title, spec.description, () => {
            debug('injected card clicked', spec.kind);
            openKind(store, spec.kind);
        }));
        debug('injected card', spec.kind);
    }

    if (!row.querySelector(`[${INJECT_MARKER}]:not([${INJECT_MARKER}="row"])`)) {
        row.remove();
    }
}

export function installIntegrationsPageOverride(store: Store): () => void {
    debug('installing integrations-page override');

    let cachedEnabled: EnabledRequestTypes | null = null;
    let disposed = false;
    let scheduled = false;

    const refreshEnabled = () => {
        fetchEnabledRequestTypes().then((enabled) => {
            if (!disposed) {
                cachedEnabled = enabled;
                scheduleInject();
            }
        });
    };
    refreshEnabled();

    const scheduleInject = () => {
        if (scheduled || disposed) {
            return;
        }
        scheduled = true;
        window.requestAnimationFrame(() => {
            scheduled = false;
            if (!disposed && cachedEnabled) {
                injectRequestCards(store, cachedEnabled);
            }
        });
    };

    const onClick = (event: MouseEvent) => {
        if (isSystemAdmin(store)) {
            return;
        }
        if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button !== 0) {
            return;
        }

        const target = event.target as HTMLElement | null;
        if (!target) {
            return;
        }

        const link = target.closest<HTMLAnchorElement>('a');
        if (!link) {
            return;
        }

        const pathname = resolveSameOriginPath(link.getAttribute('href') ?? '');
        if (!pathname) {
            return;
        }
        const kind = detectIntegrationKind(pathname);
        if (!kind) {
            return;
        }

        if (!cachedEnabled || !shouldIntercept(kind, cachedEnabled)) {
            refreshEnabled();
            return;
        }

        event.preventDefault();
        event.stopImmediatePropagation();
        debug('intercepted native link', kind);
        openKind(store, kind);
        refreshEnabled();
    };

    document.addEventListener('click', onClick, true);

    const observer = typeof MutationObserver === 'undefined' ?
        null :
        new MutationObserver(() => scheduleInject());
    observer?.observe(document.documentElement, {childList: true, subtree: true});

    // Re-run on SPA navigations.
    const onNav = () => {
        refreshEnabled();
        scheduleInject();
    };
    window.addEventListener('popstate', onNav);
    scheduleInject();

    return () => {
        disposed = true;
        document.removeEventListener('click', onClick, true);
        window.removeEventListener('popstate', onNav);
        observer?.disconnect();
        document.querySelectorAll(`[${INJECT_MARKER}]`).forEach((n) => n.remove());
        debug('removed integrations-page override');
    };
}
