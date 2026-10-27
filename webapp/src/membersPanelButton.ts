import type {Store} from 'redux';

import {getChannelType, getCurrentChannelId, isCurrentUserChannelAdmin, openAdminRequestModal} from './store';

// MARKER tags our injected button so we never insert it twice and can detect
// when Mattermost's re-render has wiped it (so we re-inject).
const MARKER = 'data-cr-request-admin';

// ANCHOR_XPATH is the exact spot in the Members right-hand sidebar the button
// should sit next to, supplied against the current Mattermost build:
//   //*[@id="rhsContainer"]/div[3]/div/div/div/div[2]/div/span
// Absolute XPaths are brittle across MM upgrades, so it's only the FIRST
// choice — findAnchor() falls back to the Add/Manage/Done action buttons when
// this element isn't present.
const ANCHOR_XPATH = '//*[@id="rhsContainer"]/div[3]/div/div/div/div[2]/div/span';

// MEMBERS_PANEL_MARKERS are selectors unique to the channel Members RHS (its
// action buttons and member-row controls). Presence of any one means the panel
// is open — a structural, theme/locale-independent signal, unlike matching the
// word "members" in the panel text.
const MEMBERS_PANEL_MARKERS = [
    '.add-members',
    '.manage-members',
    '.manage-members-done',
    '#addMembersButton',
    '[data-testid="addMembersButton"]',
    '.channel-members-rhs__send-message',
].join(', ');

// debug logs when localStorage.PLUGIN_CHANNEL_REQUESTS_DEBUG === "true" so the
// injection can be traced in a real browser without a code change:
//   localStorage.setItem('PLUGIN_CHANNEL_REQUESTS_DEBUG', 'true')
function debug(...args: unknown[]): void {
    try {
        if (typeof localStorage !== 'undefined' && localStorage.getItem('PLUGIN_CHANNEL_REQUESTS_DEBUG') === 'true') {
            // eslint-disable-next-line no-console
            console.debug('[members-panel]', ...args);
        }
    } catch {
        // localStorage blocked in some sandboxes; ignore.
    }
}

// installMembersPanelButton injects a "Request Admin" button into Mattermost's
// Members right-hand sidebar, next to the element identified by ANCHOR_XPATH
// (falling back to the native "Add"/"Manage"/"Done" action button).
//
// This is a deliberate DOM hack. Mattermost exposes NO plugin extension point
// for the core Members panel, so we watch the DOM and inject into it. It is
// inherently fragile: it keys off Mattermost's internal markup and may need
// updating across Mattermost upgrades. Everything is defensive — if the markup
// it expects isn't found, it simply does nothing (the channel-name menu item
// remains the supported entry point).
export function installMembersPanelButton(store: Store): () => void {
    if (typeof document === 'undefined' || typeof MutationObserver === 'undefined') {
        return () => undefined;
    }

    // Unconditional one-liner so it's obvious in the console whether this build
    // of the plugin webapp is actually running (vs. a stale cached bundle). The
    // window flag lets a console snippet confirm the injector loaded even after
    // the log line has scrolled away.
    // eslint-disable-next-line no-console
    console.info('[channel-requests] members-panel injector active');
    try {
        // eslint-disable-next-line no-underscore-dangle -- deliberate window global; the __cr_ prefix namespaces our console-debugging markers.
        (window as unknown as {__cr_injector_loaded?: boolean}).__cr_injector_loaded = true;
    } catch {
        // window not writable in some sandboxes; ignore.
    }

    // Coalesce bursts of mutations into one injection attempt per frame so we
    // don't run the DOM scan on every keystroke/re-render.
    let rafId = 0;
    const schedule = () => {
        if (rafId) {
            return;
        }
        rafId = window.requestAnimationFrame(() => {
            rafId = 0;
            tryInject(store);
        });
    };

    const observer = new MutationObserver(schedule);
    observer.observe(document.body, {childList: true, subtree: true});

    // Handle the case where the panel is already open at load time.
    schedule();

    return () => {
        observer.disconnect();
        if (rafId) {
            window.cancelAnimationFrame(rafId);
            rafId = 0;
        }

        // Remove the injected button so a disabled plugin leaves no control
        // that dispatches into an unmounted modal.
        document.querySelectorAll(`[${MARKER}]`).forEach((el) => el.remove());
    };
}

// RHS_SELECTORS are the candidate roots for Mattermost's right-hand sidebar,
// tried in order — the id/class has varied across MM versions.
const RHS_SELECTORS = [
    '#rhsContainer',
    '#sidebar-right',
    '[data-testid="sidebarRight"]',
    '.sidebar--right',
    '.sidebar-right',
];

function findRhs(): Element | null {
    for (const sel of RHS_SELECTORS) {
        const el = document.querySelector(sel);
        if (el) {
            return el;
        }
    }
    return null;
}

// resolveXPath returns the first element matching an XPath, or null. Wrapped in
// a try/catch because a malformed expression (or a browser without
// document.evaluate) must never throw out of the mutation observer.
function resolveXPath(xpath: string): HTMLElement | null {
    try {
        if (typeof document.evaluate !== 'function') {
            return null;
        }
        const result = document.evaluate(xpath, document, null, XPathResult.FIRST_ORDERED_NODE_TYPE, null);
        const node = result.singleNodeValue;
        return node instanceof HTMLElement ? node : null;
    } catch {
        return null;
    }
}

// findAnchor locates the element to sit next to inside the Members RHS. Tried
// in order, most-specific first, so the button appears wherever this MM build
// happens to render the members controls:
//   1. ANCHOR_XPATH — the exact spot supplied for the current build.
//   2. The "Manage"/"Members" dropdown toggle (aria-haspopup) or the member
//      count text — the row the user pointed at ("where it says Member with
//      the dropdown").
//   3. The native Add/Manage/Done action buttons.
// Every branch is defensive and returns null when nothing matches, so the
// injector simply no-ops rather than throwing.
function findAnchor(rhs: Element): HTMLElement | null {
    // Primary: the header action row's "Add" / "Manage" buttons. Confirmed
    // against the live build (classes .add-members / .manage-members), this is
    // the cleanest, most visible spot — the button sits right beside "Add".
    const actionRow =
        rhs.querySelector<HTMLElement>('.add-members') ||
        rhs.querySelector<HTMLElement>('.manage-members') ||
        rhs.querySelector<HTMLElement>('#addMembersButton') ||
        rhs.querySelector<HTMLElement>('[data-testid="addMembersButton"]') ||
        rhs.querySelector<HTMLElement>('.manage-members-done');
    if (actionRow) {
        return actionRow;
    }

    // Next: the exact XPath spot supplied for this build.
    const xpathAnchor = resolveXPath(ANCHOR_XPATH);
    if (xpathAnchor) {
        return xpathAnchor;
    }

    // Then the "Members" count/heading, if present as its own label.
    const countEl = Array.from(rhs.querySelectorAll<HTMLElement>('span, div, h2, h3, button')).find((el) => {
        const text = (el.textContent || '').trim();
        return el.children.length === 0 && ((/^\d+\s+members?$/i).test(text) || (/^members?$/i).test(text));
    });
    if (countEl) {
        return countEl;
    }

    // Last resort: any clickable labelled Add/Manage/Done.
    const clickable = Array.from(rhs.querySelectorAll<HTMLElement>('button, a, [role="button"]'));
    return clickable.find((el) => {
        const text = (el.textContent || '').trim();
        const label = el.getAttribute('aria-label') || '';
        return text === 'Add' || text === 'Manage' || text === 'Done' ||
            (/add\s+members?/i).test(label) || (/add\s+members?/i).test(text);
    }) || null;
}

// setStatus records the outcome of the latest injection attempt on the window
// so it can be read from the console when the extension can't attach:
//   JSON.stringify(window.__cr_status)
// This is a debugging aid, not load-bearing — wrapped so it never throws.
function setStatus(status: Record<string, unknown>): void {
    try {
        // eslint-disable-next-line no-underscore-dangle -- deliberate window global read from the console as window.__cr_status (see doc comment above).
        (window as unknown as {__cr_status?: unknown}).__cr_status = status;
    } catch {
        // window not writable in some sandboxes; ignore.
    }
}

// buildButton constructs the "Request Admin" control. Styled with Mattermost's
// own button classes plus a couple of inline fallbacks so it reads as a button
// even when anchored next to a plain <span> (where cloning wouldn't help).
function buildButton(store: Store): HTMLElement {
    const btn = document.createElement('button');
    btn.setAttribute(MARKER, 'true');
    btn.className = 'btn btn-sm btn-primary';
    btn.type = 'button';
    btn.textContent = 'Request Admin';
    btn.style.marginLeft = '8px';
    btn.style.verticalAlign = 'middle';
    btn.addEventListener('click', (e) => {
        e.preventDefault();
        e.stopPropagation();

        // Resolve the channel at click time so the button always acts on the
        // currently-open channel, even if the panel was reused.
        const currentChannelId = getCurrentChannelId(store.getState());
        if (currentChannelId) {
            store.dispatch(openAdminRequestModal(currentChannelId));
        }
    });
    return btn;
}

function tryInject(store: Store): void {
    const rhs = findRhs();
    if (!rhs) {
        setStatus({skip: 'no-rhs'});
        return;
    }

    // Guard so we only touch the Members panel and not other RHS views
    // (search, threads, etc.). Detect it STRUCTURALLY by its own controls —
    // reliable across themes/locales — with a loose text check as a backstop.
    // (An earlier word-boundary text match failed here: panels render member
    // rows as "user1Member" with no standalone "Members" word to match.)
    const isMembersPanel =
        rhs.querySelector(MEMBERS_PANEL_MARKERS) != null ||
        (/members?/i).test(rhs.textContent || '');
    if (!isMembersPanel) {
        setStatus({skip: 'not-members-panel'});
        return;
    }

    // Already injected for this render.
    if (rhs.querySelector(`[${MARKER}]`)) {
        setStatus({skip: 'already-injected'});
        return;
    }

    const state = store.getState();
    const channelId = getCurrentChannelId(state);

    // Only where a Channel Admin role exists. Fail OPEN: skip only when we can
    // positively identify a DM ('D') or group message ('G'). If the channel
    // type can't be read (empty), inject anyway rather than silently hiding —
    // the members panel only appears in real channels.
    const channelType = getChannelType(state, channelId);
    if (channelType === 'D' || channelType === 'G') {
        setStatus({skip: 'dm-or-gm', channelId, channelType});
        debug('skipping DM/GM');
        return;
    }

    // Don't offer "Request Admin" to someone who is already an admin here —
    // they can manage the channel directly. Requesters are non-admin members.
    if (isCurrentUserChannelAdmin(state, channelId)) {
        setStatus({skip: 'is-admin', channelId, channelType});
        debug('skipping — current user is already a channel/system admin');
        return;
    }

    const anchor = findAnchor(rhs);
    if (!anchor) {
        setStatus({skip: 'no-anchor', channelId, channelType});
        debug('members panel detected but no anchor found');
        return;
    }

    const btn = buildButton(store);
    anchor.insertAdjacentElement('afterend', btn);
    setStatus({
        skip: 'injected',
        nextTo: (anchor.textContent || '').trim(),
        anchorTag: anchor.tagName,
        anchorCls: anchor.className,
        channelId,
        channelType,
    });
    debug('injected "Request Admin" button', {
        nextTo: (anchor.textContent || '').trim(),
        anchorTag: anchor.tagName,
        channelId,
        channelType,
    });
}
