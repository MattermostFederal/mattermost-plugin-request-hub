import manifest from 'manifest';
import React from 'react';
import type {Store} from 'redux';

import type {PluginRegistry} from 'types/mattermost-webapp';

import {TeamPicker} from './ApprovalChannelPicker';
import {installChannelCreationOverride} from './channelCreationOverride';
import {fetchEnabledRequestTypes} from './client';
import {HeaderIcon} from './HeaderIcon';
import {installIntegrationsPageOverride} from './integrationsPageOverride';
import {installMembersPanelButton} from './membersPanelButton';
import {PrefixEditor} from './PrefixEditor';
import {RequestBotModal} from './RequestBotModal';
import {RequestChannelAdminModal} from './RequestChannelAdminModal';
import {RequestChannelModal} from './RequestChannelModal';
import {RequestHubModal} from './RequestHubModal';
import {RequestIncomingWebhookModal} from './RequestIncomingWebhookModal';
import {RequestOutgoingWebhookModal} from './RequestOutgoingWebhookModal';
import {RequestTeamAdminModal} from './RequestTeamAdminModal';
import {RequestTeamGuestModal} from './RequestTeamGuestModal';
import {RequestTeamMemberModal} from './RequestTeamMemberModal';
import {RequestTeamModal} from './RequestTeamModal';
import reducer, {
    getCurrentTeamId,
    getChannelType,
    getCurrentChannelId,
    isCurrentUserTeamAdmin,
    openAdminRequestModal,
    openBotRequestModal,
    openHubModal,
    openIncomingWebhookModal,
    openOutgoingWebhookModal,
    openRequestModal,
    openTeamAdminRequestModal,
    openTeamCreationModal,
    openTeamGuestRequestModal,
    openTeamMemberRequestModal,
} from './store';
import type {GlobalState} from './store';
import {installTeamMenuOverride, installTeamSidebarAddOverride} from './teamMenuOverride';

export default class Plugin {
    private disposers: Array<() => void> = [];

    public async initialize(registry: PluginRegistry, store: Store) {
        registry.registerReducer(reducer);

        registry.registerRootComponent(RequestHubModal);
        registry.registerRootComponent(RequestChannelModal);
        registry.registerRootComponent(RequestChannelAdminModal);
        registry.registerRootComponent(RequestTeamModal);
        registry.registerRootComponent(RequestTeamAdminModal);
        registry.registerRootComponent(RequestTeamMemberModal);
        registry.registerRootComponent(RequestTeamGuestModal);
        registry.registerRootComponent(RequestBotModal);
        registry.registerRootComponent(RequestIncomingWebhookModal);
        registry.registerRootComponent(RequestOutgoingWebhookModal);

        // Each feature is installed independently and defensively: a throw in
        // one (e.g. a DOM hack that trips over unexpected markup) must not abort
        // initialize() and take the others down with it. safe() logs and
        // continues. The Members-panel injector is installed FIRST so it's
        // never blocked by an earlier registration failing.
        // Registry helpers often return a registration id (string); only function
        // returns are treated as disposers.
        const safe = (label: string, fn: () => void | string | (() => void)) => {
            try {
                const disposer = fn();
                if (typeof disposer === 'function') {
                    this.disposers.push(disposer);
                }
            } catch (err) {
                // eslint-disable-next-line no-console
                console.error(`[channel-requests] ${label} failed to initialize`, err);
            }
        };

        // DOM injection of a "Request Admin" button into the native Members
        // right-hand sidebar (next to "Add"). Mattermost has no plugin slot
        // there, so this is a fragile hack that no-ops if the expected markup
        // isn't found — the channel-name menu item below stays as the supported
        // fallback.
        safe('members-panel button', () => installMembersPanelButton(store));

        // For non-admins, reroute the native sidebar "Create new channel" action to the request
        // workflow and rename it to "Request new channel". Also injects a "Request new channel"
        // item when the native one has been stripped by permissions.
        safe('channel-creation override', () => installChannelCreationOverride(store));

        // Team sidebar dropdown overrides:
        //   - Relabels "Create a team" → "Request a Team" for non-admins and
        //     routes the click to the team creation request modal.
        //   - Injects "Request Team Admin" below "Manage members" for members
        //     who are not already a Team Admin or System Admin.
        safe('team-menu override', () => installTeamMenuOverride(store));

        // Team rail "+" (Create a Team) → same Request a Team modal for non-admins.
        // Native /select_team (join other teams) is not intercepted.
        safe('team-sidebar add override', () => installTeamSidebarAddOverride(store));

        // Integrations page override: intercepts clicks on Incoming Webhooks,
        // Outgoing Webhooks, and Bot Accounts links for non-System Admin users,
        // opening the request modal instead of the native creation flow.
        safe('integrations-page override', () => installIntegrationsPageOverride(store));

        safe('channel-header button', () => registry.registerChannelHeaderButtonAction(
            <HeaderIcon/>,
            () => {
                store.dispatch(openHubModal());
            },
            'Request Hub',
            'Open Mattermost Request Hub — channel, team, membership, guest, bot, and webhook requests',
        ));

        // "Request a Channel" sits with the other request items under More actions.
        // (Sidebar "+" and the Requests hub button are the other entry points.)
        safe('channel-header menu: request channel', () => registry.registerChannelHeaderMenuAction(
            'Request a Channel',
            async () => {
                const enabled = await fetchEnabledRequestTypes();
                if (!enabled.channel) {
                    // eslint-disable-next-line no-console
                    console.info('[channel-requests] Channel requests are disabled by an administrator.');
                    return;
                }
                store.dispatch(openRequestModal());
            },
        ));

        // Channel name dropdown menu item. Members who can't manage the channel
        // themselves can request that someone be made a Channel Admin; the
        // request goes to an admin for approval. Shown only in public/private
        // channels — DMs and group messages have no Channel Admin role, so the
        // item is hidden there via shouldRender.
        safe('channel-header menu: request channel admin', () => registry.registerChannelHeaderMenuAction(
            'Request Channel Admin',
            (channelId: string) => {
                store.dispatch(openAdminRequestModal(channelId));
            },
            (state: GlobalState) => {
                const type = getChannelType(state, getCurrentChannelId(state));
                return type === 'O' || type === 'P';
            },
        ));

        safe('channel-header menu: request team', () => registry.registerChannelHeaderMenuAction(
            'Request a Team',
            async () => {
                const enabled = await fetchEnabledRequestTypes();
                if (!enabled.team) {
                    // eslint-disable-next-line no-console
                    console.info('[channel-requests] Team requests are disabled by an administrator.');
                    return;
                }
                store.dispatch(openTeamCreationModal());
            },
        ));

        // Hidden for existing Team Admins — they don't need to request a role they already hold.
        safe('channel-header menu: request team admin', () => registry.registerChannelHeaderMenuAction(
            'Request Team Admin',
            async () => {
                const enabled = await fetchEnabledRequestTypes();
                if (!enabled.teamAdmin) {
                    // eslint-disable-next-line no-console
                    console.info('[request-hub] Team Admin requests are disabled by an administrator.');
                    return;
                }
                store.dispatch(openTeamAdminRequestModal());
            },
            (state: GlobalState) => {
                const teamId = getCurrentTeamId(state);
                return !isCurrentUserTeamAdmin(state, teamId);
            },
        ));

        safe('channel-header menu: request team members', () => registry.registerChannelHeaderMenuAction(
            'Request Team Members',
            async () => {
                const enabled = await fetchEnabledRequestTypes();
                if (!enabled.teamMember) {
                    // eslint-disable-next-line no-console
                    console.info('[request-hub] Team member requests are disabled by an administrator.');
                    return;
                }
                store.dispatch(openTeamMemberRequestModal());
            },
        ));

        safe('channel-header menu: request guest invite', () => registry.registerChannelHeaderMenuAction(
            'Request Guest Invite',
            async () => {
                const enabled = await fetchEnabledRequestTypes();
                if (!enabled.teamGuest) {
                    // eslint-disable-next-line no-console
                    console.info('[request-hub] Team guest requests are disabled by an administrator.');
                    return;
                }
                store.dispatch(openTeamGuestRequestModal());
            },
            (state: GlobalState) => {
                const type = getChannelType(state, getCurrentChannelId(state));
                return type === 'O' || type === 'P';
            },
        ));

        // Webhook and bot items only shown in real channels (O/P) — DMs have no
        // meaningful "target channel" context for a webhook request.
        safe('channel-header menu: request incoming webhook', () => registry.registerChannelHeaderMenuAction(
            'Request Incoming Webhook',
            async () => {
                const enabled = await fetchEnabledRequestTypes();
                if (!enabled.webhook) {
                    // eslint-disable-next-line no-console
                    console.info('[request-hub] Webhook requests are disabled by an administrator.');
                    return;
                }
                store.dispatch(openIncomingWebhookModal());
            },
            (state: GlobalState) => {
                const type = getChannelType(state, getCurrentChannelId(state));
                return type === 'O' || type === 'P';
            },
        ));

        safe('channel-header menu: request outgoing webhook', () => registry.registerChannelHeaderMenuAction(
            'Request Outgoing Webhook',
            async () => {
                const enabled = await fetchEnabledRequestTypes();
                if (!enabled.webhook) {
                    // eslint-disable-next-line no-console
                    console.info('[request-hub] Webhook requests are disabled by an administrator.');
                    return;
                }
                store.dispatch(openOutgoingWebhookModal());
            },
            (state: GlobalState) => {
                const type = getChannelType(state, getCurrentChannelId(state));
                return type === 'O' || type === 'P';
            },
        ));

        safe('channel-header menu: request bot', () => registry.registerChannelHeaderMenuAction(
            'Request a Bot',
            async () => {
                const enabled = await fetchEnabledRequestTypes();
                if (!enabled.bot) {
                    // eslint-disable-next-line no-console
                    console.info('[request-hub] Bot requests are disabled by an administrator.');
                    return;
                }
                store.dispatch(openBotRequestModal());
            },
        ));

        // Custom admin console settings — replace plain-text fields
        // with structured pickers. Each maps to a settings_schema key
        // in plugin.json (whose type is "custom").
        if (registry.registerAdminConsoleCustomSetting) {
            registry.registerAdminConsoleCustomSetting(
                'ApprovalTeam',
                TeamPicker,
                {showTitle: true},
            );
            registry.registerAdminConsoleCustomSetting(
                'ChannelNamePrefixes',
                PrefixEditor,
                {showTitle: true},
            );
        }
    }

    public uninitialize() {
        for (const dispose of this.disposers.splice(0)) {
            try {
                dispose();
            } catch (err) {
                // eslint-disable-next-line no-console
                console.error('[channel-requests] disposer failed during uninitialize', err);
            }
        }
    }
}

declare global {
    interface Window {
        registerPlugin(pluginId: string, plugin: Plugin): void;
    }
}

window.registerPlugin(manifest.id, new Plugin());
