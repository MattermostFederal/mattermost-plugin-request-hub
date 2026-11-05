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
    purpose?: string;
    privilege_level?: string;
    data_ingestion?: string;
    hosting_env?: string;
    home_team_id?: string;
    home_channel_id?: string;
    request_token?: boolean;
    incoming_webhook_channel_id?: string;
    incoming_webhook_display_name?: string;
    outgoing_webhook_channel_id?: string;
    outgoing_webhook_display_name?: string;
    outgoing_webhook_callback_url?: string;
    compliance_accepted?: boolean;
    compliance_text_hash?: string;
};

export type BotFormOption = {value: string; label: string};

export type BotForm = {
    compliance: {
        enabled: boolean;
        text?: string;
        sha256?: string;
    };
    hosting_envs: string[];
    privilege_levels: BotFormOption[];
};

// fetchBotForm returns the bot form's settings, or null when they can't be
// loaded. Unlike fetchEnabledRequestTypes it does not fail open: the form
// must not be submitted without the statement the server requires.
export async function fetchBotForm(): Promise<BotForm | null> {
    try {
        const response = await fetch(`/plugins/${manifest.id}/api/v1/bot_form`, {
            method: 'GET',
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        });
        if (!response.ok) {
            return null;
        }
        const body = await response.json();
        return {
            compliance: {
                enabled: Boolean(body?.compliance?.enabled),
                text: typeof body?.compliance?.text === 'string' ? body.compliance.text : undefined,
                sha256: typeof body?.compliance?.sha256 === 'string' ? body.compliance.sha256 : undefined,
            },
            hosting_envs: Array.isArray(body?.hosting_envs) ? body.hosting_envs.filter((env: unknown): env is string => typeof env === 'string') : [],
            privilege_levels: Array.isArray(body?.privilege_levels) ? body.privilege_levels.filter(
                (level: unknown): level is BotFormOption => typeof (level as BotFormOption)?.value === 'string' && typeof (level as BotFormOption)?.label === 'string',
            ) : [],
        };
    } catch {
        return null;
    }
}

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

// AuditAccess says what the user may do with the audit history: System
// Admins view and download it; security approvers ask for a copy.
export type AuditAccess = {
    can_view: boolean;
    can_request: boolean;
};

// fetchAuditAccess fails closed: on any error neither entry point shows.
export async function fetchAuditAccess(): Promise<AuditAccess> {
    try {
        const response = await fetch(`/plugins/${manifest.id}/api/v1/audit/access`, {
            method: 'GET',
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        });
        if (!response.ok) {
            return {can_view: false, can_request: false};
        }
        const body = await response.json();
        return {can_view: Boolean(body?.can_view), can_request: Boolean(body?.can_request)};
    } catch {
        return {can_view: false, can_request: false};
    }
}

export type AuditRequestSummary = {
    request_id: string;
    kind: string;
    kind_label: string;
    title: string;
    status: string;
    requester_id: string;
    requester_username?: string;
    created_at: number;
    updated_at: number;
    event_count: number;
    last_event: string;
    exercise?: boolean;
};

export type AuditEvent = {
    at: number;
    event: string;
    actor_id?: string;
    actor_name?: string;
    outcome: string;
    note?: string;
    detail?: Record<string, unknown>;
};

export type AuditHistory = {
    request_id: string;
    kind: string;
    requester_id?: string;
    created_at: number;
    updated_at: number;
    status: string;
    exercise?: boolean;
    request?: Record<string, unknown>;
    events: AuditEvent[];
};

export type AuditListQuery = {
    q?: string;
    kind?: string;
    status?: string;
    page?: number;
    perPage?: number;
};

function auditQuery(params: Record<string, string | number | undefined>): string {
    const search = new URLSearchParams();
    for (const [key, value] of Object.entries(params)) {
        if (value !== undefined && value !== '') {
            search.set(key, String(value));
        }
    }
    const text = search.toString();
    return text ? `?${text}` : '';
}

async function getAuditJSON<T>(path: string): Promise<T> {
    const response = await fetch(`/plugins/${manifest.id}${path}`, {
        method: 'GET',
        credentials: 'same-origin',
        headers: {'X-Requested-With': 'XMLHttpRequest'},
    });
    if (!response.ok) {
        throw new Error((await response.text()).trim() || `Request failed (${response.status})`);
    }
    return response.json() as Promise<T>;
}

// fetchAuditRequests returns a page of the audit history list, newest
// activity first. System Admins only; throws on any failure.
export function fetchAuditRequests(query: AuditListQuery): Promise<{total: number; requests: AuditRequestSummary[]}> {
    return getAuditJSON(`/api/v1/audit/requests${auditQuery({q: query.q, kind: query.kind, status: query.status, page: query.page, per_page: query.perPage})}`);
}

// fetchAuditHistory returns one request's whole history. System Admins only.
export function fetchAuditHistory(requestId: string): Promise<{summary: AuditRequestSummary; requester_username: string; history: AuditHistory}> {
    return getAuditJSON(`/api/v1/audit/request${auditQuery({id: requestId})}`);
}

// auditExportURL is the download link for the export, as CSV or JSON,
// narrowed to one request or one kind when given.
export function auditExportURL(format: 'csv' | 'json', filter: {request?: string; kind?: string} = {}): string {
    return `/plugins/${manifest.id}/api/v1/audit/export${auditQuery({format, request: filter.request, kind: filter.kind})}`;
}

export type AuditExportRequestPayload = {
    format: 'csv' | 'json';
    kind?: string;
    requester?: string;
    since?: number;
    until?: number;
    reason?: string;
};

export async function submitAuditExportRequest(payload: AuditExportRequestPayload): Promise<ChannelRequestResult> {
    return postJSON('/api/v1/submit_audit_export_request', payload);
}
