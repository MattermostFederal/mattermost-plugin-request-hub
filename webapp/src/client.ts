import manifest from 'manifest';

export type ChannelRequestPayload = {
    team_id: string;
    display_name: string;
    name: string;

    // prefix is the selected domain prefix (e.g., "team-") when the
    // admin has configured a prefix list.
    prefix?: string;
    purpose: string;
    channel_type: string;
    members: string[];

    // admin_members are usernames the requester is proposing to have
    // Channel Admin role on the newly-created channel. Server promotes
    // them via UpdateChannelMemberRoles at creation time.
    admin_members: string[];

    // category is the optional sidebar category name to place the channel in
    // on approval. Matched case-insensitively; created per user if missing.
    category?: string;
};

export type SidebarCategory = {
    id: string;
    display_name: string;
    type: string;
};

export type ChannelRequestResult = {
    message?: string;
    error?: string;
};

// AdminRequestPayload is sent by the "Request Channel Admin" modal. nominees
// are usernames the requester wants promoted to Channel Admin on channel_id.
export type AdminRequestPayload = {
    channel_id: string;
    nominees: string[];
};

export type ChannelPrefix = {
    prefix: string;
    description: string;
    suffix_regex: string;
};

// fetchPrefixes returns the admin-configured domain prefix list. An empty
// array means the admin hasn't configured any prefixes ("not configured").
// A load FAILURE (network error / non-OK status) THROWS so callers can
// distinguish a transient outage from a genuinely empty config — otherwise
// a 500 would masquerade as "not configured" and block valid requests.
export async function fetchPrefixes(): Promise<ChannelPrefix[]> {
    const response = await fetch(`/plugins/${manifest.id}/api/v1/prefixes`, {
        method: 'GET',
        credentials: 'same-origin',
        headers: {'X-Requested-With': 'XMLHttpRequest'},
    });
    if (!response.ok) {
        throw new Error(`Failed to load prefixes (${response.status})`);
    }
    const body = await response.json();
    return Array.isArray(body) ? body : [];
}

// fetchSidebarCategories returns the caller's sidebar categories for the given
// team. Used by the channel-request modal to populate the optional category
// placement dropdown. Throws on network or server error.
export async function fetchSidebarCategories(teamId: string): Promise<SidebarCategory[]> {
    const response = await fetch(
        `/plugins/${manifest.id}/api/v1/sidebar_categories?team_id=${encodeURIComponent(teamId)}`,
        {method: 'GET', credentials: 'same-origin', headers: {'X-Requested-With': 'XMLHttpRequest'}},
    );
    if (!response.ok) {
        throw new Error(`Failed to load sidebar categories (${response.status})`);
    }
    const body = await response.json();
    return Array.isArray(body) ? body : [];
}

// EnabledRequestTypes mirrors /api/v1/config — which request kinds the admin
// has enabled. Used to hide hub tiles and other entry points.
export type EnabledRequestTypes = {
    channel: boolean;
    channelAdmin: boolean;
    team: boolean;
    teamAdmin: boolean;
    teamMember: boolean;
    teamGuest: boolean;
    bot: boolean;
    webhook: boolean;
};

// fetchEnabledRequestTypes fails open (channel + channelAdmin on) so a
// transient config-load failure never hides a working feature. The server
// re-checks toggles authoritatively on every submission.
export async function fetchEnabledRequestTypes(): Promise<EnabledRequestTypes> {
    const failOpen: EnabledRequestTypes = {
        channel: true,
        channelAdmin: true,
        team: true,
        teamAdmin: true,
        teamMember: true,
        teamGuest: false,
        bot: true,
        webhook: true,
    };
    try {
        const response = await fetch(`/plugins/${manifest.id}/api/v1/config`, {
            method: 'GET',
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        });
        if (!response.ok) {
            return failOpen;
        }
        const body = await response.json();
        return {
            channel: Boolean(body?.channel),
            channelAdmin: Boolean(body?.channel_admin),
            team: Boolean(body?.team),
            teamAdmin: Boolean(body?.team_admin),
            teamMember: Boolean(body?.team_member),
            teamGuest: Boolean(body?.team_guest),
            bot: Boolean(body?.bot),
            webhook: Boolean(body?.webhook),
        };
    } catch {
        return failOpen;
    }
}

// getCSRFToken reads the CSRF token Mattermost sets as a cookie, required for authenticated
// state-changing requests to plugin endpoints.
function getCSRFToken(): string {
    const match = (typeof document === 'undefined' ? '' : document.cookie).match(/(?:^|;\s*)MMCSRF=([^;]+)/);
    return match ? match[1] : '';
}

export async function submitChannelRequest(payload: ChannelRequestPayload): Promise<ChannelRequestResult> {
    const response = await fetch(`/plugins/${manifest.id}/api/v1/create`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': getCSRFToken(),
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(payload),
    });

    let body: ChannelRequestResult = {};
    try {
        body = await response.json();
    } catch {
        // Body may be empty or non-JSON on unexpected errors; fall through to status handling.
    }

    if (!response.ok && !body.error) {
        return {error: `Request failed (${response.status}). Please try again.`};
    }

    return body;
}

export type TeamCreationPayload = {
    display_name: string;
    name?: string; // optional URL slug — server generates from display_name if blank
    type: string; // 'O' (open) or 'I' (invite-only)
    description?: string;
    request_team_admin?: boolean;
};

export type TeamAdminRequestPayload = {
    team_id: string;
};

export async function submitTeamCreationRequest(payload: TeamCreationPayload): Promise<ChannelRequestResult> {
    const response = await fetch(`/plugins/${manifest.id}/api/v1/submit_team_creation`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': getCSRFToken(),
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(payload),
    });

    let body: ChannelRequestResult = {};
    try {
        body = await response.json();
    } catch {
        // fall through
    }

    if (!response.ok && !body.error) {
        return {error: `Request failed (${response.status}). Please try again.`};
    }
    return body;
}

export async function submitTeamAdminRequest(payload: TeamAdminRequestPayload): Promise<ChannelRequestResult> {
    const response = await fetch(`/plugins/${manifest.id}/api/v1/submit_team_admin_request`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': getCSRFToken(),
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(payload),
    });

    let body: ChannelRequestResult = {};
    try {
        body = await response.json();
    } catch {
        // fall through
    }

    if (!response.ok && !body.error) {
        return {error: `Request failed (${response.status}). Please try again.`};
    }
    return body;
}

export type TeamMemberRequestPayload = {
    team_id: string;
    nominees: string[];
    reason?: string;
};

export type TeamGuestRequestPayload = {
    team_id: string;
    email?: string;
    channel_ids: string[];
    reason?: string;
};

async function postJSON(path: string, payload: unknown): Promise<ChannelRequestResult> {
    const response = await fetch(`/plugins/${manifest.id}${path}`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': getCSRFToken(),
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(payload),
    });
    let body: ChannelRequestResult = {};
    try {
        body = await response.json();
    } catch {
        // fall through
    }
    if (!response.ok && !body.error) {
        return {error: `Request failed (${response.status}). Please try again.`};
    }
    return body;
}

export async function submitTeamMemberRequest(payload: TeamMemberRequestPayload): Promise<ChannelRequestResult> {
    return postJSON('/api/v1/submit_team_member_request', payload);
}

export async function submitTeamGuestRequest(payload: TeamGuestRequestPayload): Promise<ChannelRequestResult> {
    return postJSON('/api/v1/submit_team_guest_request', payload);
}

export type BotRequestPayload = {
    username: string;
    display_name: string;
    description?: string;
    request_token?: boolean;
    incoming_webhook_channel_id?: string;
    incoming_webhook_display_name?: string;
    outgoing_webhook_channel_id?: string;
    outgoing_webhook_display_name?: string;
    outgoing_webhook_callback_url?: string;
};

export type IncomingWebhookPayload = {
    channel_id: string;
    display_name: string;
    description?: string;
};

export type OutgoingWebhookPayload = {
    channel_id: string;
    display_name: string;
    description?: string;
    callback_url: string;
};

export async function submitBotRequest(payload: BotRequestPayload): Promise<ChannelRequestResult> {
    const response = await fetch(`/plugins/${manifest.id}/api/v1/submit_bot_request`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': getCSRFToken(),
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(payload),
    });
    let body: ChannelRequestResult = {};
    try {
        body = await response.json();
    } catch {
        // fall through
    }
    if (!response.ok && !body.error) {
        return {error: `Request failed (${response.status}). Please try again.`};
    }
    return body;
}

export async function submitIncomingWebhookRequest(payload: IncomingWebhookPayload): Promise<ChannelRequestResult> {
    const response = await fetch(`/plugins/${manifest.id}/api/v1/submit_incoming_webhook_request`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': getCSRFToken(),
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(payload),
    });
    let body: ChannelRequestResult = {};
    try {
        body = await response.json();
    } catch {
        // fall through
    }
    if (!response.ok && !body.error) {
        return {error: `Request failed (${response.status}). Please try again.`};
    }
    return body;
}

export async function submitOutgoingWebhookRequest(payload: OutgoingWebhookPayload): Promise<ChannelRequestResult> {
    const response = await fetch(`/plugins/${manifest.id}/api/v1/submit_outgoing_webhook_request`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': getCSRFToken(),
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(payload),
    });
    let body: ChannelRequestResult = {};
    try {
        body = await response.json();
    } catch {
        // fall through
    }
    if (!response.ok && !body.error) {
        return {error: `Request failed (${response.status}). Please try again.`};
    }
    return body;
}

export async function submitAdminRequest(payload: AdminRequestPayload): Promise<ChannelRequestResult> {
    const response = await fetch(`/plugins/${manifest.id}/api/v1/request_admin`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': getCSRFToken(),
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(payload),
    });

    let body: ChannelRequestResult = {};
    try {
        body = await response.json();
    } catch {
        // Body may be empty or non-JSON on unexpected errors; fall through to status handling.
    }

    if (!response.ok && !body.error) {
        return {error: `Request failed (${response.status}). Please try again.`};
    }

    return body;
}
