import manifest from 'manifest';
import type {Action} from 'redux';

export const OPEN_REQUEST_MODAL = `${manifest.id}_open_request_modal`;
export const CLOSE_REQUEST_MODAL = `${manifest.id}_close_request_modal`;
export const OPEN_ADMIN_MODAL = `${manifest.id}_open_admin_modal`;
export const CLOSE_ADMIN_MODAL = `${manifest.id}_close_admin_modal`;
export const OPEN_TEAM_CREATION_MODAL = `${manifest.id}_open_team_creation_modal`;
export const CLOSE_TEAM_CREATION_MODAL = `${manifest.id}_close_team_creation_modal`;
export const OPEN_TEAM_ADMIN_REQUEST_MODAL = `${manifest.id}_open_team_admin_request_modal`;
export const CLOSE_TEAM_ADMIN_REQUEST_MODAL = `${manifest.id}_close_team_admin_request_modal`;
export const OPEN_TEAM_MEMBER_REQUEST_MODAL = `${manifest.id}_open_team_member_request_modal`;
export const CLOSE_TEAM_MEMBER_REQUEST_MODAL = `${manifest.id}_close_team_member_request_modal`;
export const OPEN_TEAM_GUEST_REQUEST_MODAL = `${manifest.id}_open_team_guest_request_modal`;
export const CLOSE_TEAM_GUEST_REQUEST_MODAL = `${manifest.id}_close_team_guest_request_modal`;
export const OPEN_BOT_REQUEST_MODAL = `${manifest.id}_open_bot_request_modal`;
export const CLOSE_BOT_REQUEST_MODAL = `${manifest.id}_close_bot_request_modal`;
export const OPEN_INCOMING_WEBHOOK_MODAL = `${manifest.id}_open_incoming_webhook_modal`;
export const CLOSE_INCOMING_WEBHOOK_MODAL = `${manifest.id}_close_incoming_webhook_modal`;
export const OPEN_OUTGOING_WEBHOOK_MODAL = `${manifest.id}_open_outgoing_webhook_modal`;
export const CLOSE_OUTGOING_WEBHOOK_MODAL = `${manifest.id}_close_outgoing_webhook_modal`;
export const OPEN_HUB_MODAL = `${manifest.id}_open_hub_modal`;
export const CLOSE_HUB_MODAL = `${manifest.id}_close_hub_modal`;

export const openRequestModal = (): Action => ({type: OPEN_REQUEST_MODAL});
export const closeRequestModal = (): Action => ({type: CLOSE_REQUEST_MODAL});

// openAdminRequestModal carries the channel the request is scoped to, since
// the "Request Channel Admin" action is invoked from a specific channel's
// header menu.
type OpenAdminModalAction = Action & {channelId: string};
export const openAdminRequestModal = (channelId: string): OpenAdminModalAction => ({type: OPEN_ADMIN_MODAL, channelId});
export const closeAdminRequestModal = (): Action => ({type: CLOSE_ADMIN_MODAL});

export const openTeamCreationModal = (): Action => ({type: OPEN_TEAM_CREATION_MODAL});
export const closeTeamCreationModal = (): Action => ({type: CLOSE_TEAM_CREATION_MODAL});
export const openTeamAdminRequestModal = (): Action => ({type: OPEN_TEAM_ADMIN_REQUEST_MODAL});
export const closeTeamAdminRequestModal = (): Action => ({type: CLOSE_TEAM_ADMIN_REQUEST_MODAL});
export const openTeamMemberRequestModal = (): Action => ({type: OPEN_TEAM_MEMBER_REQUEST_MODAL});
export const closeTeamMemberRequestModal = (): Action => ({type: CLOSE_TEAM_MEMBER_REQUEST_MODAL});
export const openTeamGuestRequestModal = (): Action => ({type: OPEN_TEAM_GUEST_REQUEST_MODAL});
export const closeTeamGuestRequestModal = (): Action => ({type: CLOSE_TEAM_GUEST_REQUEST_MODAL});
export const openBotRequestModal = (): Action => ({type: OPEN_BOT_REQUEST_MODAL});
export const closeBotRequestModal = (): Action => ({type: CLOSE_BOT_REQUEST_MODAL});
export const openIncomingWebhookModal = (): Action => ({type: OPEN_INCOMING_WEBHOOK_MODAL});
export const closeIncomingWebhookModal = (): Action => ({type: CLOSE_INCOMING_WEBHOOK_MODAL});
export const openOutgoingWebhookModal = (): Action => ({type: OPEN_OUTGOING_WEBHOOK_MODAL});
export const closeOutgoingWebhookModal = (): Action => ({type: CLOSE_OUTGOING_WEBHOOK_MODAL});
export const openHubModal = (): Action => ({type: OPEN_HUB_MODAL});
export const closeHubModal = (): Action => ({type: CLOSE_HUB_MODAL});

export type PluginState = {
    modalOpen: boolean;

    // adminModalChannelId is the channel the "Request Channel Admin" modal is
    // open for, or null when the modal is closed.
    adminModalChannelId: string | null;

    teamCreationModalOpen: boolean;
    teamAdminRequestModalOpen: boolean;
    teamMemberRequestModalOpen: boolean;
    teamGuestRequestModalOpen: boolean;
    botRequestModalOpen: boolean;
    incomingWebhookModalOpen: boolean;
    outgoingWebhookModalOpen: boolean;
    hubModalOpen: boolean;
};

// GlobalState is a minimal shape of the parts of the Redux store this plugin reads. Plugin state is
// namespaced under "plugins-<pluginId>"; team + channel entities live under entities.
export type GlobalState = {
    entities: {
        teams: {
            currentTeamId: string;

            // Matches TeamsState from @mattermost/types — Record<string, Team>.
            // display_name and name are non-optional on the Team type.
            teams: Record<string, {id: string; display_name: string; name: string}>;

            // Matches TeamsState.myMembers — Record<string, TeamMembership>.
            // roles and scheme_admin are non-optional on TeamMembership.
            myMembers: Record<string, {roles: string; scheme_admin: boolean}>;
        };
        users?: {
            currentUserId?: string;
            profiles?: Record<string, {roles?: string}>;
        };
        channels?: {
            currentChannelId?: string;
            channels?: Record<string, {display_name?: string; type?: string}>;

            // myMembers holds the current user's membership per channel,
            // including their per-channel roles ("channel_admin") and the
            // scheme_admin flag (permissions v2). Used to hide the "Request
            // Admin" action from users who are already Channel Admins.
            myMembers?: Record<string, {roles?: string; scheme_admin?: boolean}>;
        };
    };
    [pluginKey: string]: unknown;
};

export const isRequestModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.modalOpen);
};

export const getAdminModalChannelId = (state: GlobalState): string | null => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return pluginState?.adminModalChannelId ?? null;
};

export const isTeamCreationModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.teamCreationModalOpen);
};

export const isTeamAdminRequestModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.teamAdminRequestModalOpen);
};

export const isTeamMemberRequestModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.teamMemberRequestModalOpen);
};

export const isTeamGuestRequestModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.teamGuestRequestModalOpen);
};

export const isBotRequestModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.botRequestModalOpen);
};

export const isIncomingWebhookModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.incomingWebhookModalOpen);
};

export const isOutgoingWebhookModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.outgoingWebhookModalOpen);
};

export const isHubModalOpen = (state: GlobalState): boolean => {
    const pluginState = state[`plugins-${manifest.id}`] as PluginState | undefined;
    return Boolean(pluginState?.hubModalOpen);
};

// isCurrentUserTeamAdmin returns true when the logged-in user is already a
// Team Admin (or System Admin) in the given team. Used to hide the
// "Request Team Admin" menu item for users who already have that role.
export const isCurrentUserTeamAdmin = (state: GlobalState, teamId: string): boolean => {
    const users = state?.entities?.users;
    const currentUserId = users?.currentUserId ?? '';
    const systemRoles = (currentUserId ? users?.profiles?.[currentUserId]?.roles : '') ?? '';
    if (systemRoles.split(' ').includes('system_admin')) {
        return true;
    }
    const myMember = teamId ? state.entities?.teams?.myMembers?.[teamId] : undefined;
    if (!myMember) {
        return false;
    }

    // scheme_admin and roles are non-optional on TeamMembership.
    return myMember.scheme_admin || myMember.roles.split(' ').includes('team_admin');
};

export const getCurrentTeamId = (state: GlobalState): string => state.entities?.teams?.currentTeamId ?? '';

// getCurrentTeamDisplayName returns the display name of the active team.
// Falls back to the team ID when the entity map hasn't populated yet —
// cosmetic fallback, acceptable on initial render.
export const getCurrentTeamDisplayName = (state: GlobalState): string => {
    const teamId = state.entities?.teams?.currentTeamId ?? '';
    return state.entities?.teams?.teams?.[teamId]?.display_name ?? teamId;
};

export const getCurrentChannelId = (state: GlobalState): string => state.entities?.channels?.currentChannelId ?? '';

export const getChannelDisplayName = (state: GlobalState, channelId: string): string =>
    state.entities?.channels?.channels?.[channelId]?.display_name ?? '';

// getChannelType returns the MM channel type: 'O' (public), 'P' (private),
// 'D' (direct), or 'G' (group). Empty string when unknown.
export const getChannelType = (state: GlobalState, channelId: string): string =>
    state.entities?.channels?.channels?.[channelId]?.type ?? '';

// isCurrentUserChannelAdmin reports whether the logged-in user is already an
// admin for the given channel — either a Channel Admin (via the roles string
// or the scheme_admin flag) or a System Admin (who can manage any channel).
// The "Request Admin" affordance is hidden for these users since they don't
// need to request what they already have.
export const isCurrentUserChannelAdmin = (state: GlobalState, channelId: string): boolean => {
    const users = state.entities?.users;
    const currentUserId = users?.currentUserId ?? '';
    const systemRoles = (currentUserId ? users?.profiles?.[currentUserId]?.roles : '') ?? '';
    if (systemRoles.split(' ').includes('system_admin')) {
        return true;
    }

    const member = channelId ? state.entities?.channels?.myMembers?.[channelId] : undefined;
    if (!member) {
        return false;
    }
    if (member.scheme_admin) {
        return true;
    }
    return (member.roles ?? '').split(' ').includes('channel_admin');
};

const initialState: PluginState = {
    modalOpen: false,
    adminModalChannelId: null,
    teamCreationModalOpen: false,
    teamAdminRequestModalOpen: false,
    teamMemberRequestModalOpen: false,
    teamGuestRequestModalOpen: false,
    botRequestModalOpen: false,
    incomingWebhookModalOpen: false,
    outgoingWebhookModalOpen: false,
    hubModalOpen: false,
};

export default function reducer(state: PluginState = initialState, action: Action): PluginState {
    switch (action.type) {
    case OPEN_REQUEST_MODAL:
        return {...state, modalOpen: true};
    case CLOSE_REQUEST_MODAL:
        return {...state, modalOpen: false};
    case OPEN_ADMIN_MODAL:
        return {...state, adminModalChannelId: (action as OpenAdminModalAction).channelId};
    case CLOSE_ADMIN_MODAL:
        return {...state, adminModalChannelId: null};
    case OPEN_TEAM_CREATION_MODAL:
        return {...state, teamCreationModalOpen: true};
    case CLOSE_TEAM_CREATION_MODAL:
        return {...state, teamCreationModalOpen: false};
    case OPEN_TEAM_ADMIN_REQUEST_MODAL:
        return {...state, teamAdminRequestModalOpen: true};
    case CLOSE_TEAM_ADMIN_REQUEST_MODAL:
        return {...state, teamAdminRequestModalOpen: false};
    case OPEN_TEAM_MEMBER_REQUEST_MODAL:
        return {...state, teamMemberRequestModalOpen: true};
    case CLOSE_TEAM_MEMBER_REQUEST_MODAL:
        return {...state, teamMemberRequestModalOpen: false};
    case OPEN_TEAM_GUEST_REQUEST_MODAL:
        return {...state, teamGuestRequestModalOpen: true};
    case CLOSE_TEAM_GUEST_REQUEST_MODAL:
        return {...state, teamGuestRequestModalOpen: false};
    case OPEN_BOT_REQUEST_MODAL:
        return {...state, botRequestModalOpen: true};
    case CLOSE_BOT_REQUEST_MODAL:
        return {...state, botRequestModalOpen: false};
    case OPEN_INCOMING_WEBHOOK_MODAL:
        return {...state, incomingWebhookModalOpen: true};
    case CLOSE_INCOMING_WEBHOOK_MODAL:
        return {...state, incomingWebhookModalOpen: false};
    case OPEN_OUTGOING_WEBHOOK_MODAL:
        return {...state, outgoingWebhookModalOpen: true};
    case CLOSE_OUTGOING_WEBHOOK_MODAL:
        return {...state, outgoingWebhookModalOpen: false};
    case OPEN_HUB_MODAL:
        return {...state, hubModalOpen: true};
    case CLOSE_HUB_MODAL:
        return {...state, hubModalOpen: false};
    default:
        return state;
    }
}
