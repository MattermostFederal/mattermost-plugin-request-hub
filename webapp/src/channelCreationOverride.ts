import type {Store} from 'redux';

import {makeDebug} from './debug';
import {openRequestModal} from './store';

// Mattermost exposes no plugin API to modify the sidebar "+" dropdown,
// so we work through the DOM. Two cases:
//   1. User HAS channel-creation permission -> menu contains "Create
//      new channel". We relabel it + intercept its click.
//   2. User LACKS the permission -> Mattermost strips that item.
//      We clone a sibling item to inject a "Request new channel"
//      entry in the same spot.
//
// Design choices to keep this robust across MM UI refactors:
//   - We do NOT rely on any specific class name — MM uses SCSS modules
//     that rename constantly. We identify the "+" menu by the presence
//     of at least one recognized SIBLING label ("Browse channels",
//     "Open a direct message", etc.) — those strings are stable.
//   - We scan the WHOLE document on any body mutation (throttled), not
//     just added subtrees. React sometimes renders the menu shell first
//     and populates items in a subsequent tick, which the naive
//     "watch addedNodes only" approach misses.
//   - Enabling PLUGIN_CHANNEL_REQUESTS_DEBUG=true in localStorage turns
//     on console.debug tracing so failures are diagnosable in prod
//     without a code change.

const NATIVE_ITEM_ID = 'createNewChannelMenuItem';
const NATIVE_LABEL = 'Create new channel';
const REQUEST_LABEL = 'Request new channel';
const INJECTED_MARK = 'data-mm-plugin-channel-requests-injected';

const SIBLING_LABELS = [
    'Browse channels',
    'Open a direct message',
    'Create new user group',
    'Create new category',
    'Invite people',
];
const ITEM_SELECTOR = 'a, button, li, [role="menuitem"]';

type UsersState = {
    currentUserId: string;
    profiles: Record<string, {roles?: string}>;
};

type MinimalState = {
    entities?: {
        users?: UsersState;
    };
};

// Traces override behavior when localStorage
// PLUGIN_CHANNEL_REQUESTS_DEBUG === "true". See ./debug.
const debug = makeDebug('channel-requests');

function isCurrentUserAdmin(store: Store): boolean {
    const state = store.getState() as MinimalState;
    const users = state?.entities?.users;
    const currentUserId = users?.currentUserId ?? '';
    const roles = users?.profiles?.[currentUserId]?.roles ?? '';
    return roles.split(' ').includes('system_admin');
}

// findSiblingItem returns the first menu item in `root` whose text
// matches one of our known sibling labels. Used to identify the "+" menu
// and to serve as a template for the injected item.
function findSiblingItem(root: ParentNode): HTMLElement | null {
    const items = Array.from(root.querySelectorAll<HTMLElement>(ITEM_SELECTOR));
    for (const item of items) {
        const label = (item.textContent ?? '').trim();
        if (SIBLING_LABELS.includes(label)) {
            return item;
        }
    }
    return null;
}

// findMenuRoot walks up from a sibling item to the closest common
// container that also holds the OTHER menu items. Used to scope
// injection so we don't accidentally add to nested elements.
function findMenuRoot(sibling: HTMLElement): HTMLElement | null {
    let cursor: HTMLElement | null = sibling.parentElement;
    while (cursor && cursor !== document.body) {
        // A menu root has at least two of our recognized sibling labels
        // as descendants. That gates out inner wrappers of a single item.
        let count = 0;
        cursor.querySelectorAll<HTMLElement>(ITEM_SELECTOR).forEach((el) => {
            const label = (el.textContent ?? '').trim();
            if (SIBLING_LABELS.includes(label)) {
                count += 1;
            }
        });
        if (count >= 2) {
            return cursor;
        }
        cursor = cursor.parentElement;
    }
    return null;
}

function replaceLabel(root: HTMLElement, from: string, to: string): boolean {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    let node = walker.nextNode();
    while (node) {
        if (node.nodeValue && node.nodeValue.trim() === from) {
            node.nodeValue = node.nodeValue.replace(from, to);
            return true;
        }
        node = walker.nextNode();
    }
    return false;
}

// findNativeCreateItem locates the "Create new channel" item in `root`.
function findNativeCreateItem(root: ParentNode): HTMLElement | null {
    const byId = root.querySelector<HTMLElement>(`#${NATIVE_ITEM_ID}`);
    if (byId) {
        return byId;
    }
    const items = Array.from(root.querySelectorAll<HTMLElement>(ITEM_SELECTOR));
    for (const item of items) {
        if ((item.textContent ?? '').trim() === NATIVE_LABEL) {
            return item;
        }
    }
    return null;
}

// injectRequestItem clones a sibling into a "Request new channel" entry
// and prepends it to the sibling's parent container. No-ops if:
//   - the container already has an INJECTED_MARK item, or
//   - the sibling has no parent element.
function injectRequestItem(menuRoot: HTMLElement, sibling: HTMLElement, store: Store): void {
    if (menuRoot.querySelector(`[${INJECTED_MARK}]`)) {
        debug('inject skipped: already injected in this menu');
        return;
    }

    const container = sibling.parentElement;
    if (!container) {
        debug('inject skipped: sibling has no parent');
        return;
    }

    const injected = sibling.cloneNode(true) as HTMLElement;
    injected.removeAttribute('id');
    injected.setAttribute(INJECTED_MARK, 'true');
    injected.setAttribute('data-testid', 'channelRequestsInjectedItem');

    // Rewrite the visible label. If we can't find the source label as a
    // text node (rare, but happens with heavily-composed items), fall
    // back to blowing away all text nodes + prepending our label.
    const siblingLabel = (sibling.textContent ?? '').trim();
    const replaced = replaceLabel(injected, siblingLabel, REQUEST_LABEL);
    if (!replaced) {
        debug('inject fallback: rewrite label via textContent replacement');

        // Preserve leading icon (usually the first child element) then
        // append the new label as a plain text node.
        const iconClone = injected.firstElementChild?.cloneNode(true) ?? null;
        injected.textContent = '';
        if (iconClone) {
            injected.appendChild(iconClone);
        }
        injected.appendChild(document.createTextNode(' ' + REQUEST_LABEL));
    }

    if (injected instanceof HTMLAnchorElement) {
        injected.href = '#';
    }

    injected.addEventListener(
        'click',
        (event) => {
            event.preventDefault();
            event.stopImmediatePropagation();
            document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}));
            store.dispatch(openRequestModal());
        },
        true,
    );

    container.insertBefore(injected, container.firstChild);
    debug('injected Request new channel item', {menuRootTag: menuRoot.tagName, container: container.tagName});
}

// processDocument scans all currently-rendered dropdown menus and, for
// each one that looks like the "+" menu, relabels the native item
// (case 1) or injects our own (case 2).
function processDocument(store: Store): void {
    if (isCurrentUserAdmin(store)) {
        return;
    }

    // Find every element that has at least one recognized sibling label
    // as an immediate child menu item. Walking every menu candidate is
    // O(n), but MM only has a handful of open menus at a time.
    const sibling = findSiblingItem(document);
    if (!sibling) {
        return;
    }

    const menuRoot = findMenuRoot(sibling);
    if (!menuRoot) {
        debug('processDocument: sibling found but no menu root');
        return;
    }

    // Case 1: native item present -> relabel + let click listener handle.
    const native = findNativeCreateItem(menuRoot);
    if (native) {
        if (replaceLabel(native, NATIVE_LABEL, REQUEST_LABEL)) {
            debug('relabeled native item to Request new channel');
        }
        return;
    }

    // Case 2: native item absent -> inject.
    injectRequestItem(menuRoot, sibling, store);
}

// installChannelCreationOverride wires up:
//   1. Initial scan on install (menu may already be open).
//   2. Throttled document-scan on any body mutation.
//   3. Capture-phase click listener that routes both the relabeled
//      native item AND our injected item to the request modal.
export function installChannelCreationOverride(store: Store): () => void {
    debug('installing channel-creation override');

    // Initial pass — covers the case where the menu was already open
    // when the plugin webapp bundle finished loading.
    processDocument(store);

    // Throttle: coalesce bursts of mutations into a single scan per
    // animation frame. requestAnimationFrame is well-supported and
    // aligns with React's render cadence.
    //
    // Skip batches that only churn text/attributes (the common case in a
    // busy SPA — typing, presence, new-post edits). The "+" menu appears
    // as newly-added element nodes, so only those warrant a full-document
    // scan; this keeps the observer from running querySelectorAll over
    // the whole document on essentially every frame.
    let scheduled = false;
    let rafId = 0;
    let disposed = false;
    const observer = new MutationObserver((mutations) => {
        if (scheduled || disposed) {
            return;
        }
        const addedElements = mutations.some((m) =>
            Array.from(m.addedNodes).some((n) => n.nodeType === Node.ELEMENT_NODE),
        );
        if (!addedElements) {
            return;
        }
        scheduled = true;
        rafId = requestAnimationFrame(() => {
            rafId = 0;
            scheduled = false;
            if (!disposed) {
                processDocument(store);
            }
        });
    });
    observer.observe(document.body, {childList: true, subtree: true});

    // Capture-phase click routing. Matches:
    //   - Our injected item (by data attribute)
    //   - The relabeled native item (by id or REQUEST_LABEL text)
    const onClick = (event: MouseEvent) => {
        if (isCurrentUserAdmin(store)) {
            return;
        }
        const target = event.target as HTMLElement | null;
        if (!target) {
            return;
        }

        const injected = target.closest<HTMLElement>(`[${INJECTED_MARK}]`);
        let match: HTMLElement | null = injected;

        if (!match) {
            const byId = target.closest<HTMLElement>(`#${NATIVE_ITEM_ID}`);
            if (byId) {
                match = byId;
            } else {
                // Text-match fallback for a relabeled native item that
                // lacks a stable id. Scope it to the "+" menu (via
                // findMenuRoot) so we don't hijack an unrelated element
                // elsewhere in the UI that happens to read
                // "Request new channel".
                const nearest = target.closest<HTMLElement>(ITEM_SELECTOR);
                if (nearest && (nearest.textContent ?? '').trim() === REQUEST_LABEL && findMenuRoot(nearest)) {
                    match = nearest;
                }
            }
        }

        if (!match) {
            return;
        }

        event.preventDefault();
        event.stopImmediatePropagation();
        document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}));
        store.dispatch(openRequestModal());
        debug('routed click to Request modal', {viaInjected: Boolean(injected)});
    };
    document.addEventListener('click', onClick, true);

    return () => {
        disposed = true;
        document.removeEventListener('click', onClick, true);
        observer.disconnect();
        if (rafId) {
            cancelAnimationFrame(rafId);
            rafId = 0;
        }

        // Remove injected menu items (and their click listeners) so a disabled
        // plugin cannot leave a stale "Request new channel" entry that still
        // dispatches into an unmounted modal.
        document.querySelectorAll(`[${INJECTED_MARK}]`).forEach((el) => el.remove());
        debug('removed channel-creation override');
    };
}
