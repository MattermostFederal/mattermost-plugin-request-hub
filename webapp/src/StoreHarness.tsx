import manifest from 'manifest';
import React, {useState} from 'react';
import {Provider} from 'react-redux';
import {combineReducers, legacy_createStore as createStore} from 'redux';
import type {Reducer} from 'redux';

import reducer, {
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
import type {GlobalState, PluginState} from './store';

/**
 * Harness for components that read the plugin's Redux state.
 *
 * Builds a real store from the plugin reducer (under `plugins-<id>`, as
 * Mattermost registers it) plus static `entities`, so closing a modal
 * really unmounts it. Hidden-label buttons ("harness: open <modal>") reopen
 * each modal, which lets a test check that a reopened form starts empty.
 */

type Props = {
    entities?: Partial<GlobalState['entities']>;
    requestOpen?: boolean;
    adminChannelId?: string | null;

    // pluginState overrides fields of the plugin's own state, e.g.
    // {hubModalOpen: true} to start with the hub open. It is applied after
    // requestOpen and adminChannelId, and like them is read once at mount.
    pluginState?: Partial<PluginState>;
    children?: React.ReactNode;
};

const defaultEntities: GlobalState['entities'] = {
    teams: {
        currentTeamId: 'team1',
        teams: {team1: {id: 'team1', display_name: 'Team One', name: 'team-one'}},
        myMembers: {},
    },
    channels: {
        currentChannelId: 'chan1',
        channels: {chan1: {display_name: 'Town Square', type: 'O'}},
    },
};

const reopeners = {
    'request modal': openRequestModal,
    'admin modal': () => openAdminRequestModal('chan1'),
    team: openTeamCreationModal,
    'team admin': openTeamAdminRequestModal,
    'team member': openTeamMemberRequestModal,
    'team guest': openTeamGuestRequestModal,
    bot: openBotRequestModal,
    hub: openHubModal,
    incoming: openIncomingWebhookModal,
    outgoing: openOutgoingWebhookModal,
};

export const StoreHarness = ({entities, requestOpen = false, adminChannelId = null, pluginState, children}: Props) => {
    const [store] = useState(() => {
        const allEntities = {...defaultEntities, ...entities};
        const rootReducer = combineReducers({
            entities: (state: GlobalState['entities'] = allEntities) => state,
            [`plugins-${manifest.id}`]: reducer,
        }) as Reducer;
        return createStore(
            rootReducer,
            {
                entities: allEntities,
                [`plugins-${manifest.id}`]: {
                    ...reducer(undefined, {type: '@@INIT'}),
                    modalOpen: requestOpen,
                    adminModalChannelId: adminChannelId,
                    ...pluginState,
                },
            },
        );
    });

    return (
        <Provider store={store}>
            {children}
            {Object.entries(reopeners).map(([modal, open]) => (
                <button
                    key={modal}
                    aria-label={`harness: open ${modal}`}
                    onClick={() => store.dispatch(open())}
                />
            ))}
        </Provider>
    );
};
