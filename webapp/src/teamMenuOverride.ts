import type {Store} from 'redux';

import {fetchEnabledRequestTypes} from './client';
import {makeDebug} from './debug';
import {openTeamAdminRequestModal, openTeamCreationModal} from './store';
import type {GlobalState} from './store';

// The team sidebar dropdown contains two plugin-owned items:
//
//   1. "Request a Team" — for non-System Admin users:
//        a. Native "Create a team" present → relabel + intercept click.
//        b. Native item missing (no CREATE_TEAM permission) → inject a
//           "Request a Team" item where Create would have been.
//      System Admins always keep the native create flow.
//
//   2. "Request Team Admin" — injected next to "Manage members". Shown only
//      when the current user is NOT already a Team Admin or System Admin for
//      the active team.
//
// Both items are DOM-injected because Mattermost exposes no plugin hook into
// the team sidebar dropdown. The approach mirrors channelCreationOverride.ts:
// identify the menu by stable sibling text, scan on body mutations, and
// use a capture-phase click listener for reliable interception.

const CREATE_TEAM_LABEL = 'Create a team';
const JOIN_TEAM_LABEL = 'Join another team';
const MANAGE_MEMBERS_LABEL = 'Manage members';
const VIEW_MEMBERS_LABEL = 'View members'; // label shown to non-admin members
const LEAVE_TEAM_LABEL = 'Leave team';
const LEARN_ABOUT_TEAMS_LABEL = 'Learn about teams';
const REQUEST_TEAM_LABEL = 'Request a Team';
const REQUEST_TEAM_ADMIN_LABEL = 'Request Team Admin';

const CREATE_TEAM_MARK = 'data-mm-plugin-create-team-intercepted';
const REQUEST_TEAM_INJECT_MARK = 'data-mm-plugin-request-team-injected';
const REQUEST_TEAM_ADMIN_MARK = 'data-mm-plugin-request-team-admin';

const ITEM_SELECTOR = 'a, button, li, [role="menuitem"]';

// Stable text strings that identify the team dropdown (as opposed to the
// channel "+" dropdown which has its own sibling labels).
const TEAM_MENU_SIBLING_LABELS = [
    'Invite people',
    'Team settings',
    'Manage members',
    'Leave team',
    'Learn about teams',
];

const debug = makeDebug('team-menu');

function isSystemAdmin(store: Store): boolean {
    const state = store.getState() as GlobalState;
    const users = state?.entities?.users;
    const uid = users?.currentUserId ?? '';
    const roles = (uid ? users?.profiles?.[uid]?.roles : '') ?? '';
    return roles.split(' ').includes('system_admin');
}

function isTeamAdmin(store: Store): boolean {
    const state = store.getState() as GlobalState;
    const teamId = state?.entities?.teams?.currentTeamId ?? '';
    if (!teamId) {
        return false;
    }
    const myMember = state.entities?.teams?.myMembers?.[teamId];
    if (!myMember) {
        return false;
    }

    // scheme_admin and roles are non-optional on TeamMembership — no fallbacks needed.
    return myMember.scheme_admin || myMember.roles.split(' ').includes('team_admin');
}

// findTeamMenuRoot walks through the DOM looking for the team dropdown. It's
// identified by having at least 2 of the known sibling labels as descendants.
function findTeamMenuRoot(): HTMLElement | null {
    const items = Array.from(document.querySelectorAll<HTMLElement>(ITEM_SELECTOR));
    for (const item of items) {
        const label = (item.textContent ?? '').trim();
        if (!TEAM_MENU_SIBLING_LABELS.includes(label)) {
            continue;
        }

        // Walk up until we find an ancestor with ≥2 team-menu sibling labels.
        let cursor: HTMLElement | null = item.parentElement;
        while (cursor && cursor !== document.body) {
            let count = 0;
            cursor.querySelectorAll<HTMLElement>(ITEM_SELECTOR).forEach((el) => {
                if (TEAM_MENU_SIBLING_LABELS.includes((el.textContent ?? '').trim())) {
                    count++;
                }
            });
            if (count >= 2) {
                return cursor;
            }
            cursor = cursor.parentElement;
        }
    }
    return null;
}

function replaceTextNode(root: HTMLElement, from: string, to: string): boolean {
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

function findItemByText(root: ParentNode, label: string): HTMLElement | null {
    const items = Array.from(root.querySelectorAll<HTMLElement>(ITEM_SELECTOR));
    return items.find((el) => (el.textContent ?? '').trim() === label) ?? null;
}

// interceptCreateTeam relabels the native "Create a team" item to
// "Request a Team" and marks it so the click listener can intercept it.
// Returns true when the native item was found (and intercepted or already marked).
function interceptCreateTeam(menuRoot: HTMLElement): boolean {
    // Already intercepted — the label was already swapped on a prior scan.
    if (menuRoot.querySelector(`[${CREATE_TEAM_MARK}]`)) {
        return true;
    }

    const native = findItemByText(menuRoot, CREATE_TEAM_LABEL);
    if (!native) {
        debug('interceptCreateTeam: native item not found');
        return false;
    }

    if (replaceTextNode(native, CREATE_TEAM_LABEL, REQUEST_TEAM_LABEL)) {
        native.setAttribute(CREATE_TEAM_MARK, 'true');
        debug('relabeled "Create a team" → "Request a Team"');
    } else {
        // Label rewrite failed (composed item) — still mark so clicks route.
        native.setAttribute(CREATE_TEAM_MARK, 'true');
        debug('marked native Create a team without text rewrite');
    }
    return true;
}

// injectRequestTeam clones a sibling menu item into a "Request a Team" entry
// when the user lacks CREATE_TEAM (native item is absent). Placement mirrors
// where "Create a team" sits: after "Join another team" when present, otherwise
// before "Learn about teams" / after "Leave team".
function injectRequestTeam(menuRoot: HTMLElement, store: Store): void {
    if (
        menuRoot.querySelector(`[${CREATE_TEAM_MARK}]`) ||
        menuRoot.querySelector(`[${REQUEST_TEAM_INJECT_MARK}]`) ||
        findItemByText(menuRoot, REQUEST_TEAM_LABEL)
    ) {
        debug('injectRequestTeam: already present');
        return;
    }

    const joinItem = findItemByText(menuRoot, JOIN_TEAM_LABEL);
    const learnItem = findItemByText(menuRoot, LEARN_ABOUT_TEAMS_LABEL);
    const leaveItem = findItemByText(menuRoot, LEAVE_TEAM_LABEL);
    const manageItem =
        findItemByText(menuRoot, MANAGE_MEMBERS_LABEL) ??
        findItemByText(menuRoot, VIEW_MEMBERS_LABEL);
    const template = joinItem ?? learnItem ?? leaveItem ?? manageItem;
    if (!template) {
        debug('injectRequestTeam: no sibling template found');
        return;
    }

    const container = template.parentElement;
    if (!container) {
        return;
    }

    const injected = template.cloneNode(true) as HTMLElement;
    injected.removeAttribute('id');
    injected.setAttribute(REQUEST_TEAM_INJECT_MARK, 'true');
    injected.setAttribute('data-testid', 'teamMenuRequestTeam');

    const templateLabel = (template.textContent ?? '').trim();
    const replaced = replaceTextNode(injected, templateLabel, REQUEST_TEAM_LABEL);
    if (!replaced) {
        const iconClone = injected.firstElementChild?.cloneNode(true) ?? null;
        injected.textContent = '';
        if (iconClone) {
            injected.appendChild(iconClone);
        }
        injected.appendChild(document.createTextNode(' ' + REQUEST_TEAM_LABEL));
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
            fetchEnabledRequestTypes().then((enabled) => {
                if (!enabled.team) {
                    debug('team requests disabled — injected Request a Team ignored');
                    return;
                }
                store.dispatch(openTeamCreationModal());
                debug('injected Request a Team opened modal');
            });
        },
        true,
    );

    if (joinItem?.parentElement === container) {
        // After "Join another team" — same slot as native Create a team.
        if (joinItem.nextSibling) {
            container.insertBefore(injected, joinItem.nextSibling);
        } else {
            container.appendChild(injected);
        }
    } else if (learnItem?.parentElement === container) {
        // No Join item — put Request a Team just above "Learn about teams".
        container.insertBefore(injected, learnItem);
    } else if (leaveItem?.parentElement === container) {
        if (leaveItem.nextSibling) {
            container.insertBefore(injected, leaveItem.nextSibling);
        } else {
            container.appendChild(injected);
        }
    } else if (manageItem?.nextSibling) {
        container.insertBefore(injected, manageItem.nextSibling);
    } else {
        container.appendChild(injected);
    }

    debug('injected "Request a Team" (native Create a team absent)');
}

// injectRequestTeamAdmin clones the "Manage members" item and inserts a
// "Request Team Admin" entry directly below it. No-ops when already injected.
function injectRequestTeamAdmin(menuRoot: HTMLElement, store: Store): void {
    if (menuRoot.querySelector(`[${REQUEST_TEAM_ADMIN_MARK}]`)) {
        debug('injectRequestTeamAdmin: already injected');
        return;
    }

    // Admins see "Manage members"; regular members see "View members" — try both.
    const manageItem =
        findItemByText(menuRoot, MANAGE_MEMBERS_LABEL) ??
        findItemByText(menuRoot, VIEW_MEMBERS_LABEL);
    if (!manageItem) {
        debug('injectRequestTeamAdmin: neither "Manage members" nor "View members" found');
        return;
    }

    const container = manageItem.parentElement;
    if (!container) {
        return;
    }

    const injected = manageItem.cloneNode(true) as HTMLElement;
    injected.removeAttribute('id');
    injected.setAttribute(REQUEST_TEAM_ADMIN_MARK, 'true');
    injected.setAttribute('data-testid', 'teamMenuRequestTeamAdmin');

    const replaced =
        replaceTextNode(injected, MANAGE_MEMBERS_LABEL, REQUEST_TEAM_ADMIN_LABEL) ||
        replaceTextNode(injected, VIEW_MEMBERS_LABEL, REQUEST_TEAM_ADMIN_LABEL);
    if (!replaced) {
        // Fallback: overwrite text content while keeping the leading icon.
        const iconClone = injected.firstElementChild?.cloneNode(true) ?? null;
        injected.textContent = '';
        if (iconClone) {
            injected.appendChild(iconClone);
        }
        injected.appendChild(document.createTextNode(' ' + REQUEST_TEAM_ADMIN_LABEL));
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
            fetchEnabledRequestTypes().then((enabled) => {
                if (!enabled.teamAdmin) {
                    debug('team admin requests disabled — ignoring Request Team Admin click');
                    return;
                }
                store.dispatch(openTeamAdminRequestModal());
            });
        },
        true,
    );

    // Insert immediately after "Manage members".
    if (manageItem.nextSibling) {
        container.insertBefore(injected, manageItem.nextSibling);
    } else {
        container.appendChild(injected);
    }

    debug('injected "Request Team Admin" after "Manage members"');
}

// processTeamMenu scans the document for the team dropdown and applies both
// DOM patches: "Request a Team" (relabel or inject) and "Request Team Admin".
// Set to true the first time processTeamMenu successfully finds the team dropdown.
// Used by the install-time timeout to detect silent failures.
let menuFoundOnce = false;

function processTeamMenu(store: Store, teamAdminRequestsOff = false): void {
    const menuRoot = findTeamMenuRoot();
    if (!menuRoot) {
        return;
    }
    menuFoundOnce = true;

    // Non-admins: relabel native Create a team, or inject when it's stripped.
    if (!isSystemAdmin(store)) {
        if (!interceptCreateTeam(menuRoot)) {
            injectRequestTeam(menuRoot, store);
        }
    }

    // "Request Team Admin" is injected for members who are not already
    // Team Admin or System Admin, while the request type is on. Remove a
    // stale inject after promotion or after the type is turned off.
    if (!teamAdminRequestsOff && !isSystemAdmin(store) && !isTeamAdmin(store)) {
        injectRequestTeamAdmin(menuRoot, store);
    } else {
        menuRoot.querySelector(`[${REQUEST_TEAM_ADMIN_MARK}]`)?.remove();
    }
}

const REQUEST_TEAM_ADD_MARK = 'data-mm-plugin-request-team-add';
const REQUEST_TEAM_ADD_ID = 'mmPluginRequestTeamAddButton';

function nativeCreateTeamButtonPresent(): boolean {
    return Boolean(
        document.getElementById('create_teamTeamButton') ||
        document.querySelector('.team-sidebar a[href*="create_team"], #teamSidebarWrapper a[href*="create_team"]'),
    );
}

function removeInjectedRequestTeamAdd(): void {
    document.getElementById(REQUEST_TEAM_ADD_ID)?.remove();
}

// injectRequestTeamAddButton puts a Mattermost-styled "+" into the team rail
// when the native Create Team button is missing (CREATE_TEAM permission stripped).
// Clicks open the Request a Team modal.
function injectRequestTeamAddButton(store: Store): void {
    if (isSystemAdmin(store)) {
        removeInjectedRequestTeamAdd();
        return;
    }
    if (nativeCreateTeamButtonPresent()) {
        // Native create button is visible — intercept handles it; don't duplicate.
        removeInjectedRequestTeamAdd();
        return;
    }

    const wrapper =
        document.getElementById('teamSidebarWrapper') ??
        document.querySelector<HTMLElement>('.team-sidebar .team-wrapper');
    if (!wrapper) {
        return;
    }
    if (document.getElementById(REQUEST_TEAM_ADD_ID)) {
        return;
    }

    const container = document.createElement('div');
    container.className = 'team-container special';
    container.id = REQUEST_TEAM_ADD_ID;
    container.setAttribute(REQUEST_TEAM_ADD_MARK, 'true');

    const link = document.createElement('a');
    link.href = '#';
    link.setAttribute('aria-label', 'Request a Team');
    link.setAttribute('title', 'Request a Team');
    link.setAttribute(REQUEST_TEAM_ADD_MARK, 'true');

    const btn = document.createElement('div');
    btn.className = 'team-btn team-btn__add';
    btn.innerHTML = '<i class="icon icon-plus" role="img" aria-hidden="true"></i>';

    link.appendChild(btn);
    container.appendChild(link);

    link.addEventListener(
        'click',
        (event) => {
            event.preventDefault();
            event.stopImmediatePropagation();
            fetchEnabledRequestTypes().then((enabled) => {
                if (!enabled.team) {
                    debug('team requests disabled — injected + ignored');
                    return;
                }
                store.dispatch(openTeamCreationModal());
                debug('injected team-rail + opened Request Team modal');
            });
        },
        true,
    );

    // After team list / after join button if present.
    const selectTeam = document.getElementById('select_teamTeamButton')?.closest('.team-container');
    if (selectTeam?.parentElement === wrapper) {
        wrapper.insertBefore(container, selectTeam);
    } else {
        wrapper.appendChild(container);
    }
    debug('injected Request a Team + into team rail');
}

// installTeamSidebarAddOverride:
//   1. Intercepts native #create_teamTeamButton → Request a Team (non-admins).
//   2. Injects a synthetic + when Mattermost hides create (no CREATE_TEAM).
// Join (#select_teamTeamButton) is never intercepted.
export function installTeamSidebarAddOverride(store: Store): () => void {
    debug('installing team-sidebar + override');

    // Cache AllowTeamRequests so the native Create Team + is not cancelled
    // before we know whether the request flow should intercept it.
    let teamRequestsEnabled: boolean | null = null;
    const refreshTeamEnabled = () => {
        fetchEnabledRequestTypes().then((enabled) => {
            teamRequestsEnabled = enabled.team;
        });
    };
    refreshTeamEnabled();

    // Always restore a + when Mattermost hid Create Team (no CREATE_TEAM).
    // AllowTeamRequests only gates the modal on click — not button visibility —
    // so an older saved "off" config can't leave the rail empty.
    const refresh = () => {
        if (isSystemAdmin(store)) {
            removeInjectedRequestTeamAdd();
            return;
        }
        injectRequestTeamAddButton(store);
    };

    const onClick = (event: MouseEvent) => {
        if (isSystemAdmin(store)) {
            return;
        }
        const target = event.target as HTMLElement | null;
        if (!target) {
            return;
        }

        // Injected button handles its own click.
        if (target.closest(`[${REQUEST_TEAM_ADD_MARK}]`)) {
            return;
        }

        const createLink =
            target.closest<HTMLAnchorElement>('#create_teamTeamButton') ??
            target.closest<HTMLAnchorElement>('a[href*="create_team"]');
        if (!createLink) {
            return;
        }

        if (!createLink.closest('.team-sidebar, #teamSidebarWrapper')) {
            if (!createLink.querySelector('.team-btn__add') && !createLink.id.includes('create_team')) {
                return;
            }
        }

        // When team requests are disabled, leave the native Create Team click alone.
        if (teamRequestsEnabled === false) {
            return;
        }
        if (teamRequestsEnabled === null) {
            refreshTeamEnabled();
            return;
        }

        event.preventDefault();
        event.stopImmediatePropagation();
        store.dispatch(openTeamCreationModal());
        debug('routed team-sidebar + (create_team) to Request Team modal');
    };
    document.addEventListener('click', onClick, true);

    let rafId = 0;
    const observer = new MutationObserver(() => {
        if (rafId) {
            return;
        }
        rafId = requestAnimationFrame(() => {
            rafId = 0;
            refresh();
        });
    });
    observer.observe(document.body, {childList: true, subtree: true});

    // Team rail often mounts after plugin init — retry a few times.
    const timers = [0, 500, 2000, 5000].map((ms) => window.setTimeout(refresh, ms));

    return () => {
        document.removeEventListener('click', onClick, true);
        observer.disconnect();
        if (rafId) {
            cancelAnimationFrame(rafId);
            rafId = 0;
        }
        timers.forEach((id) => window.clearTimeout(id));
        removeInjectedRequestTeamAdd();
        debug('removed team-sidebar + override');
    };
}

export function installTeamMenuOverride(store: Store): () => void {
    debug('installing team-menu override');

    // Cache AllowTeamRequests so Create Team intercept does not preventDefault
    // before knowing whether native create should remain available.
    let teamRequestsEnabled: boolean | null = null;
    let teamAdminRequestsOff = false;
    const refreshTeamEnabled = () => {
        fetchEnabledRequestTypes().then((enabled) => {
            teamRequestsEnabled = enabled.team;
            teamAdminRequestsOff = !enabled.teamAdmin;
            if (!enabled.teamAdmin) {
                document.querySelectorAll(`[${REQUEST_TEAM_ADMIN_MARK}]`).forEach((n) => n.remove());
            }
        });
    };
    refreshTeamEnabled();

    processTeamMenu(store);

    let rafId = 0;
    const observer = new MutationObserver((mutations) => {
        if (rafId) {
            return;
        }
        const addedElements = mutations.some((m) =>
            Array.from(m.addedNodes).some((n) => n.nodeType === Node.ELEMENT_NODE),
        );
        if (!addedElements) {
            return;
        }
        rafId = requestAnimationFrame(() => {
            rafId = 0;
            processTeamMenu(store, teamAdminRequestsOff);
        });
    });
    observer.observe(document.body, {childList: true, subtree: true});

    // Warn once if the team dropdown was never found — likely means MM changed
    // its DOM structure and "Request a Team" / "Request Team Admin" are broken.
    const warnTimer = window.setTimeout(() => {
        if (!menuFoundOnce) {
            // eslint-disable-next-line no-console -- intentional one-shot DOM-structure diagnostic
            console.warn(
                '[mattermost-request-hub] Team sidebar dropdown not found after 10s. ' +
                '"Request a Team" and "Request Team Admin" labels may not be working. ' +
                'Check whether Mattermost changed its team menu DOM structure.',
            );
        }
    }, 10000);

    // Re-run when Redux team membership state changes so "Request Team Admin"
    // disappears immediately after a server-side promotion rather than persisting
    // until the user reloads the page.
    let prevMyMembers: unknown = null;
    const unsubscribe = store.subscribe(() => {
        const state = store.getState() as GlobalState;
        const myMembers = state?.entities?.teams?.myMembers;
        if (myMembers !== prevMyMembers) {
            prevMyMembers = myMembers;
            processTeamMenu(store, teamAdminRequestsOff);
        }
    });

    // Capture-phase listener: intercepts clicks on the relabeled native
    // "Create a team" item. Injected items handle their own click.
    const onClick = (event: MouseEvent) => {
        if (isSystemAdmin(store)) {
            // System Admins keep the native Create Team flow — the plugin
            // AllowTeamRequests toggle only gates the request/UI paths.
            return;
        }
        const target = event.target as HTMLElement | null;
        if (!target) {
            return;
        }

        // Injected "Request a Team" has its own listener.
        if (target.closest(`[${REQUEST_TEAM_INJECT_MARK}]`)) {
            return;
        }

        const intercepted = target.closest<HTMLElement>(`[${CREATE_TEAM_MARK}]`);
        if (!intercepted) {
            return;
        }

        // When team requests are disabled, leave the native Create Team click alone.
        if (teamRequestsEnabled === false) {
            return;
        }
        if (teamRequestsEnabled === null) {
            refreshTeamEnabled();
            return;
        }

        event.preventDefault();
        event.stopImmediatePropagation();
        document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}));
        store.dispatch(openTeamCreationModal());
        debug('routed "Create a team" click to Request Team modal');
    };
    document.addEventListener('click', onClick, true);

    return () => {
        document.removeEventListener('click', onClick, true);
        observer.disconnect();
        unsubscribe();
        window.clearTimeout(warnTimer);
        if (rafId) {
            cancelAnimationFrame(rafId);
            rafId = 0;
        }

        // Put an open dropdown back the way Mattermost drew it, so a disabled
        // plugin leaves no injected items or relabeled "Create a team".
        document.querySelectorAll(`[${REQUEST_TEAM_INJECT_MARK}], [${REQUEST_TEAM_ADMIN_MARK}]`).forEach((n) => n.remove());
        document.querySelectorAll<HTMLElement>(`[${CREATE_TEAM_MARK}]`).forEach((el) => {
            replaceTextNode(el, REQUEST_TEAM_LABEL, CREATE_TEAM_LABEL);
            el.removeAttribute(CREATE_TEAM_MARK);
        });
        debug('removed team-menu override');
    };
}
