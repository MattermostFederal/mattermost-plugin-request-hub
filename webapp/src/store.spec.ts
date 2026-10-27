import {expect, test} from '@playwright/test';
import manifest from 'manifest';

import reducer, {
    CLOSE_ADMIN_MODAL,
    CLOSE_REQUEST_MODAL,
    OPEN_ADMIN_MODAL,
    OPEN_REQUEST_MODAL,
    closeAdminRequestModal,
    closeRequestModal,
    getAdminModalChannelId,
    getChannelDisplayName,
    getChannelType,
    getCurrentChannelId,
    getCurrentTeamDisplayName,
    getCurrentTeamId,
    isBotRequestModalOpen,
    isCurrentUserChannelAdmin,
    isCurrentUserTeamAdmin,
    isHubModalOpen,
    isIncomingWebhookModalOpen,
    isOutgoingWebhookModalOpen,
    isRequestModalOpen,
    isTeamAdminRequestModalOpen,
    isTeamCreationModalOpen,
    isTeamGuestRequestModalOpen,
    isTeamMemberRequestModalOpen,
    openAdminRequestModal,
    openRequestModal,
    closeBotRequestModal,
    closeHubModal,
    closeIncomingWebhookModal,
    closeOutgoingWebhookModal,
    closeTeamAdminRequestModal,
    closeTeamCreationModal,
    closeTeamGuestRequestModal,
    closeTeamMemberRequestModal,
    openBotRequestModal,
    openHubModal,
    openIncomingWebhookModal,
    openOutgoingWebhookModal,
    openTeamAdminRequestModal,
    openTeamCreationModal,
    openTeamGuestRequestModal,
    openTeamMemberRequestModal,
} from './store';
import type {GlobalState} from './store';

const pluginKey = `plugins-${manifest.id}`;

function stateWith(overrides: Partial<GlobalState['entities']> = {}, plugin?: unknown): GlobalState {
    return {
        entities: {
            teams: {currentTeamId: 'team1', teams: {}, myMembers: {}},
            ...overrides,
        },
        ...(plugin === undefined ? {} : {[pluginKey]: plugin}),
    };
}

test.describe('action creators', () => {
    test('action types are namespaced by the plugin id', () => {
        for (const type of [OPEN_REQUEST_MODAL, CLOSE_REQUEST_MODAL, OPEN_ADMIN_MODAL, CLOSE_ADMIN_MODAL]) {
            expect(type.startsWith(`${manifest.id}_`)).toBe(true);
        }
    });

    test('build the expected actions', () => {
        expect(openRequestModal()).toEqual({type: OPEN_REQUEST_MODAL});
        expect(closeRequestModal()).toEqual({type: CLOSE_REQUEST_MODAL});
        expect(openAdminRequestModal('chan1')).toEqual({type: OPEN_ADMIN_MODAL, channelId: 'chan1'});
        expect(closeAdminRequestModal()).toEqual({type: CLOSE_ADMIN_MODAL});
    });
});

test.describe('reducer', () => {
    test('starts with every modal closed', () => {
        const state = reducer(undefined, {type: '@@INIT'});
        expect(state.modalOpen).toBe(false);
        expect(state.adminModalChannelId).toBeNull();
        for (const [key, value] of Object.entries(state)) {
            if (key !== 'adminModalChannelId') {
                expect(value, key).toBe(false);
            }
        }
    });

    test('opens and closes the request modal', () => {
        const opened = reducer(undefined, openRequestModal());
        expect(opened.modalOpen).toBe(true);
        expect(reducer(opened, closeRequestModal()).modalOpen).toBe(false);
    });

    test('opens the admin modal for a channel and closes it', () => {
        const opened = reducer(undefined, openAdminRequestModal('chan1'));
        expect(opened.adminModalChannelId).toBe('chan1');
        expect(reducer(opened, closeAdminRequestModal()).adminModalChannelId).toBeNull();
    });

    test('keeps the other modal state when one changes', () => {
        const both = reducer(reducer(undefined, openRequestModal()), openAdminRequestModal('chan1'));
        expect(both).toMatchObject({modalOpen: true, adminModalChannelId: 'chan1'});
    });

    test('returns the same state for unknown actions', () => {
        const state = {...reducer(undefined, openRequestModal()), adminModalChannelId: 'chan1'};
        expect(reducer(state, {type: 'something_else'})).toBe(state);
    });
});

test.describe('selectors', () => {
    test('modal selectors default when plugin state is missing', () => {
        const state = stateWith();
        expect(isRequestModalOpen(state)).toBe(false);
        expect(getAdminModalChannelId(state)).toBeNull();
    });

    test('modal selectors read plugin state', () => {
        const state = stateWith({}, {modalOpen: true, adminModalChannelId: 'chan1'});
        expect(isRequestModalOpen(state)).toBe(true);
        expect(getAdminModalChannelId(state)).toBe('chan1');
    });

    test('team and channel selectors read entities', () => {
        const state = stateWith({
            channels: {
                currentChannelId: 'chan1',
                channels: {chan1: {display_name: 'Town Square', type: 'O'}},
            },
        });
        expect(getCurrentTeamId(state)).toBe('team1');
        expect(getCurrentChannelId(state)).toBe('chan1');
        expect(getChannelDisplayName(state, 'chan1')).toBe('Town Square');
        expect(getChannelType(state, 'chan1')).toBe('O');
    });

    test('team and channel selectors default to empty strings', () => {
        const state = {entities: {}} as unknown as GlobalState;
        expect(getCurrentTeamId(state)).toBe('');
        expect(getCurrentChannelId(state)).toBe('');
        expect(getChannelDisplayName(state, 'chan1')).toBe('');
        expect(getChannelType(state, 'chan1')).toBe('');
    });
});

test.describe('isCurrentUserChannelAdmin', () => {
    const users = (roles: string) => ({currentUserId: 'me', profiles: {me: {roles}}});

    test('is true for a System Admin even without channel membership', () => {
        expect(isCurrentUserChannelAdmin(stateWith({users: users('system_user system_admin')}), 'chan1')).toBe(true);
    });

    test('is true for a scheme admin member', () => {
        const state = stateWith({
            users: users('system_user'),
            channels: {myMembers: {chan1: {roles: 'channel_user', scheme_admin: true}}},
        });
        expect(isCurrentUserChannelAdmin(state, 'chan1')).toBe(true);
    });

    test('is true when the member roles include channel_admin', () => {
        const state = stateWith({
            users: users('system_user'),
            channels: {myMembers: {chan1: {roles: 'channel_user channel_admin'}}},
        });
        expect(isCurrentUserChannelAdmin(state, 'chan1')).toBe(true);
    });

    test('is false for a regular member', () => {
        const state = stateWith({
            users: users('system_user'),
            channels: {myMembers: {chan1: {roles: 'channel_user'}}},
        });
        expect(isCurrentUserChannelAdmin(state, 'chan1')).toBe(false);
    });

    test('is false for a member with no roles string', () => {
        const state = stateWith({channels: {myMembers: {chan1: {}}}});
        expect(isCurrentUserChannelAdmin(state, 'chan1')).toBe(false);
    });

    test('is false when not a member or no channel is given', () => {
        const state = stateWith({users: users('system_user'), channels: {myMembers: {}}});
        expect(isCurrentUserChannelAdmin(state, 'chan1')).toBe(false);
        expect(isCurrentUserChannelAdmin(state, '')).toBe(false);
    });

    test('is false when there is no current user', () => {
        expect(isCurrentUserChannelAdmin(stateWith({users: {profiles: {}}}), 'chan1')).toBe(false);
    });
});

// Each request type's modal has an open flag in plugin state, an
// open/close action pair and a selector.
test.describe('request type modals', () => {
    const modals = [
        ['team creation', 'teamCreationModalOpen', openTeamCreationModal, closeTeamCreationModal, isTeamCreationModalOpen],
        ['Team Admin', 'teamAdminRequestModalOpen', openTeamAdminRequestModal, closeTeamAdminRequestModal, isTeamAdminRequestModalOpen],
        ['team member', 'teamMemberRequestModalOpen', openTeamMemberRequestModal, closeTeamMemberRequestModal, isTeamMemberRequestModalOpen],
        ['team guest', 'teamGuestRequestModalOpen', openTeamGuestRequestModal, closeTeamGuestRequestModal, isTeamGuestRequestModalOpen],
        ['bot', 'botRequestModalOpen', openBotRequestModal, closeBotRequestModal, isBotRequestModalOpen],
        ['incoming webhook', 'incomingWebhookModalOpen', openIncomingWebhookModal, closeIncomingWebhookModal, isIncomingWebhookModalOpen],
        ['outgoing webhook', 'outgoingWebhookModalOpen', openOutgoingWebhookModal, closeOutgoingWebhookModal, isOutgoingWebhookModalOpen],
        ['hub', 'hubModalOpen', openHubModal, closeHubModal, isHubModalOpen],
    ] as const;

    for (const [name, key, open, close, isOpen] of modals) {
        test(`${name}: opens, closes and is read by its selector`, () => {
            expect(open().type.startsWith(`${manifest.id}_`)).toBe(true);
            expect(close().type.startsWith(`${manifest.id}_`)).toBe(true);

            const opened = reducer(undefined, open());
            expect(opened[key]).toBe(true);
            expect(isOpen(stateWith({}, opened))).toBe(true);

            const closed = reducer(opened, close());
            expect(closed[key]).toBe(false);
            expect(isOpen(stateWith({}, closed))).toBe(false);
            expect(isOpen(stateWith())).toBe(false);
        });
    }

    test('opening one modal leaves the others closed', () => {
        const opened = reducer(undefined, openBotRequestModal());
        for (const [, key] of modals) {
            expect(opened[key], key).toBe(key === 'botRequestModalOpen');
        }
    });
});

test.describe('isCurrentUserTeamAdmin', () => {
    const users = (roles: string) => ({currentUserId: 'me', profiles: {me: {roles}}});
    const teams = (myMembers: GlobalState['entities']['teams']['myMembers']) => ({currentTeamId: 'team1', teams: {}, myMembers});

    test('is true for a System Admin', () => {
        expect(isCurrentUserTeamAdmin(stateWith({users: users('system_user system_admin')}), 'team1')).toBe(true);
    });

    test('is true for a scheme admin member', () => {
        const state = stateWith({users: users('system_user'), teams: teams({team1: {roles: 'team_user', scheme_admin: true}})});
        expect(isCurrentUserTeamAdmin(state, 'team1')).toBe(true);
    });

    test('is true when the member roles include team_admin', () => {
        const state = stateWith({users: users('system_user'), teams: teams({team1: {roles: 'team_user team_admin', scheme_admin: false}})});
        expect(isCurrentUserTeamAdmin(state, 'team1')).toBe(true);
    });

    test('is false for a regular member', () => {
        const state = stateWith({users: users('system_user'), teams: teams({team1: {roles: 'team_user', scheme_admin: false}})});
        expect(isCurrentUserTeamAdmin(state, 'team1')).toBe(false);
    });

    test('is false when not a member, no team is given or there is no user', () => {
        const state = stateWith({users: {profiles: {}}});
        expect(isCurrentUserTeamAdmin(state, 'team1')).toBe(false);
        expect(isCurrentUserTeamAdmin(state, '')).toBe(false);
    });
});

test.describe('getCurrentTeamDisplayName', () => {
    test('returns the display name of the current team', () => {
        const state = stateWith({teams: {currentTeamId: 'team1', teams: {team1: {id: 'team1', display_name: 'Team One', name: 'team-one'}}, myMembers: {}}});
        expect(getCurrentTeamDisplayName(state)).toBe('Team One');
    });

    test('falls back to the team ID before the team loads', () => {
        expect(getCurrentTeamDisplayName(stateWith())).toBe('team1');
    });

    test('is empty with no current team', () => {
        expect(getCurrentTeamDisplayName({entities: {}} as unknown as GlobalState)).toBe('');
    });
});

test.describe('getChannelType', () => {
    for (const type of ['O', 'P', 'D', 'G']) {
        test(`returns ${type}`, () => {
            expect(getChannelType(stateWith({channels: {channels: {chan1: {type}}}}), 'chan1')).toBe(type);
        });
    }

    test('is empty for an unknown channel', () => {
        expect(getChannelType(stateWith({channels: {channels: {}}}), 'chan1')).toBe('');
    });
});
