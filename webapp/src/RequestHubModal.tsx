import manifest from 'manifest';
import React, {useEffect, useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';
import type {Action} from 'redux';

import {fetchEnabledRequestTypes} from './client';
import type {EnabledRequestTypes} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {
    closeHubModal,
    getChannelType,
    getCurrentChannelId,
    getCurrentTeamId,
    isCurrentUserTeamAdmin,
    isHubModalOpen,
    openAdminRequestModal,
    openBotRequestModal,
    openIncomingWebhookModal,
    openOutgoingWebhookModal,
    openRequestModal,
    openTeamAdminRequestModal,
    openTeamCreationModal,
    openTeamGuestRequestModal,
    openTeamMemberRequestModal,
} from './store';
import type {GlobalState} from './store';

const DOCS_URL = `/plugins/${manifest.id}/public/help/docs.html`;

const overlayStyle: React.CSSProperties = {
    position: 'fixed',
    inset: 0,
    background: 'rgba(0, 0, 0, 0.5)',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    zIndex: 1050,
};
const dialogStyle: React.CSSProperties = {
    background: 'var(--center-channel-bg, #fff)',
    color: 'var(--center-channel-color, #3d3c40)',
    borderRadius: 8,
    width: 600,
    maxWidth: '92vw',
    maxHeight: '90vh',
    overflowY: 'auto',
    padding: 24,
    boxShadow: '0 8px 24px rgba(0, 0, 0, 0.24)',
};
const gridStyle: React.CSSProperties = {
    display: 'grid',
    gridTemplateColumns: 'repeat(2, 1fr)',
    gap: 12,
    marginTop: 16,
};
const cardStyle: React.CSSProperties = {
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
    padding: '16px 18px',
    border: '1px solid var(--center-channel-color-16, rgba(61,60,64,0.16))',
    borderRadius: 6,
    cursor: 'pointer',
    transition: 'background 0.1s',
    textAlign: 'left',
    background: 'transparent',
    color: 'inherit',
    width: '100%',
};

// Fixed square so emoji glyphs (🤖 vs 📤) paint at the same visual size.
const iconBoxStyle: React.CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    width: 24,
    height: 24,
    fontSize: 20,
    lineHeight: 1,
    overflow: 'hidden',
    flexShrink: 0,
};

type RequestItem = {
    icon: string;
    title: string;
    description: string;
    action: () => Action;
    enabled: boolean;
    hidden?: boolean;

    /** When true, only show in public/private channels (not DM/GM). */
    requiresTeamChannel?: boolean;

    /** When true, only show when a team is selected (any team — not just the approval team). */
    requiresTeam?: boolean;

    /** Override emoji font-size inside the 24×24 box (large glyphs like 🤖). */
    iconFontSize?: number;
};

// Fail-closed for newer types until /api/v1/config responds — matches client.ts.
const defaultEnabled: EnabledRequestTypes = {
    channel: true,
    channelAdmin: true,
    team: true,
    teamAdmin: true,
    teamMember: true,
    teamGuest: false,
    bot: true,
    webhook: true,
};

export const RequestHubModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isHubModalOpen);
    const channelId = useSelector((state: GlobalState) => getCurrentChannelId(state));
    const teamId = useSelector((state: GlobalState) => getCurrentTeamId(state));
    const channelType = useSelector((state: GlobalState) => getChannelType(state, channelId));
    const inTeamChannel = channelType === 'O' || channelType === 'P';
    const alreadyTeamAdmin = useSelector((state: GlobalState) => isCurrentUserTeamAdmin(state, teamId));
    const [enabled, setEnabled] = useState<EnabledRequestTypes>(defaultEnabled);
    const close = () => dispatch(closeHubModal());
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    useEffect(() => {
        if (!isOpen) {
            return undefined;
        }
        let cancelled = false;
        fetchEnabledRequestTypes().then((types) => {
            if (!cancelled) {
                setEnabled(types);
            }
        });
        return () => {
            cancelled = true;
        };
    }, [isOpen]);

    if (!isOpen) {
        return null;
    }

    // Close the hub first, then open the target modal on the next tick so the
    // originating click cannot land on the new modal's overlay and immediately
    // dismiss it (mousedown on hub tile → unmount → mouseup on new overlay).
    const open = (action: () => Action) => {
        dispatch(closeHubModal());
        window.setTimeout(() => {
            dispatch(action());
        }, 0);
    };

    const items: RequestItem[] = [
        {
            icon: '📢',
            title: 'Request a Channel',
            description: 'Ask for a new public or private channel to be created.',
            action: openRequestModal,
            enabled: enabled.channel,
        },
        {
            icon: '⭐',
            title: 'Request Channel Admin',
            description: 'Nominate a Channel Admin for the current channel.',
            action: () => openAdminRequestModal(channelId),
            enabled: enabled.channelAdmin,
            requiresTeamChannel: true,
        },
        {
            icon: '🏢',
            title: 'Request a Team',
            description: 'Ask for a new team to be created.',
            action: openTeamCreationModal,
            enabled: enabled.team,
        },
        {
            icon: '🔑',
            title: 'Request Team Admin',
            description: 'Request Team Admin privileges for your current team.',
            action: openTeamAdminRequestModal,
            enabled: enabled.teamAdmin,
            hidden: alreadyTeamAdmin,
        },
        {
            icon: '👥',
            title: 'Request Team Members',
            description: 'Ask to add existing users or bots to your current team.',
            action: openTeamMemberRequestModal,
            enabled: enabled.teamMember,
        },
        {
            icon: '✉️',
            title: 'Request Guest Invite',
            description: 'Request a channel-scoped guest invitation email for this team.',
            action: openTeamGuestRequestModal,
            enabled: enabled.teamGuest,
            requiresTeamChannel: true,
        },
        {
            icon: '🤖',
            title: 'Request a Bot',
            description: 'Request a bot account, access token, or webhook.',
            action: openBotRequestModal,
            enabled: enabled.bot,

            // Robot glyph paints larger than tray/building emojis at the same em.
            iconFontSize: 16,
        },
        {
            icon: '📥',
            title: 'Request Incoming Webhook',
            description: 'Post messages into a channel from an external system.',
            action: openIncomingWebhookModal,
            enabled: enabled.webhook,

            // Any team the user is on — not tied to the approval/primary team.
            // Forms still need an O/P channel; they validate that on submit.
            requiresTeam: true,
        },
        {
            icon: '📤',
            title: 'Request Outgoing Webhook',
            description: 'Send channel messages to an external endpoint.',
            action: openOutgoingWebhookModal,
            enabled: enabled.webhook,
            requiresTeam: true,
        },
    ];

    const visible = items.filter((item) =>
        !item.hidden &&
        item.enabled &&
        (!item.requiresTeamChannel || inTeamChannel) &&
        (!item.requiresTeam || Boolean(teamId)),
    );

    return (
        <div
            style={overlayStyle}
            onClick={close}
        >
            <div
                ref={dialogRef}
                style={dialogStyle}
                role='dialog'
                aria-modal={true}
                aria-labelledby='hub-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <div style={{display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start'}}>
                    <div>
                        <h3
                            id='hub-title'
                            style={{marginTop: 0, marginBottom: 4}}
                        >{'Mattermost Request Hub'}</h3>
                        <p style={{margin: 0, opacity: 0.72, fontSize: 14}}>
                            {'Choose a request type. Each request is reviewed by the appropriate admins.'}
                        </p>
                    </div>
                    <button
                        style={{background: 'none', border: 'none', cursor: 'pointer', fontSize: 20, lineHeight: 1, opacity: 0.6, padding: 0}}
                        onClick={close}
                        aria-label='Close'
                    >
                        {'×'}
                    </button>
                </div>

                {visible.length === 0 ? (
                    <p style={{marginTop: 24, opacity: 0.72}}>
                        {'No request types are currently enabled. Ask a System Admin to enable them in System Console → Plugins → Mattermost Request Hub.'}
                    </p>
                ) : (
                    <div style={gridStyle}>
                        {visible.map((item) => (
                            <button
                                key={item.title}
                                style={cardStyle}
                                onClick={() => open(item.action)}
                                onMouseEnter={(e) => {
                                    (e.currentTarget as HTMLButtonElement).style.background =
                                        'var(--center-channel-color-08, rgba(61,60,64,0.08))';
                                }}
                                onMouseLeave={(e) => {
                                    (e.currentTarget as HTMLButtonElement).style.background = 'transparent';
                                }}
                            >
                                <span
                                    style={item.iconFontSize ? {...iconBoxStyle, fontSize: item.iconFontSize} : iconBoxStyle}
                                    aria-hidden={true}
                                >
                                    {item.icon}
                                </span>
                                <span style={{fontWeight: 600, fontSize: 14}}>{item.title}</span>
                                <span style={{fontSize: 13, opacity: 0.7, lineHeight: 1.4}}>{item.description}</span>
                            </button>
                        ))}
                    </div>
                )}

                {/* Always shown — docs without needing /mattermost-request-hub slash commands. */}
                <a
                    href={DOCS_URL}
                    target='_blank'
                    rel='noopener noreferrer'
                    style={{
                        ...cardStyle,
                        marginTop: 16,
                        textDecoration: 'none',
                        flexDirection: 'row',
                        alignItems: 'center',
                        gap: 14,
                    }}
                    onMouseEnter={(e) => {
                        (e.currentTarget as HTMLAnchorElement).style.background =
                            'var(--center-channel-color-08, rgba(61,60,64,0.08))';
                    }}
                    onMouseLeave={(e) => {
                        (e.currentTarget as HTMLAnchorElement).style.background = 'transparent';
                    }}
                >
                    <span
                        style={iconBoxStyle}
                        aria-hidden={true}
                    >{'📖'}</span>
                    <span style={{display: 'flex', flexDirection: 'column', gap: 4}}>
                        <span style={{fontWeight: 600, fontSize: 14}}>{'Documentation'}</span>
                        <span style={{fontSize: 13, opacity: 0.7, lineHeight: 1.4}}>
                            {'Guides for every request type — no slash commands needed.'}
                        </span>
                    </span>
                </a>
            </div>
        </div>
    );
};
