import React, {useEffect, useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {
    fetchBotForm,
    fetchBotHomeChannels,
    fetchBotHomeTeams,
    submitBotRequest,
} from './client';
import type {BotForm, BotHomeChannel, BotHomeTeam, BotRequestPayload} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {
    closeBotRequestModal,
    getCurrentChannelId,
    getCurrentTeamId,
    isBotRequestModalOpen,
} from './store';
import type {GlobalState} from './store';

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
    width: 560,
    maxWidth: '90vw',
    maxHeight: '90vh',
    overflowY: 'auto',
    padding: 24,
    boxShadow: '0 8px 24px rgba(0, 0, 0, 0.24)',
};
const nestedOverlayStyle: React.CSSProperties = {
    ...overlayStyle,
    zIndex: 1060,
    background: 'rgba(0, 0, 0, 0.55)',
};
const nestedDialogStyle: React.CSSProperties = {
    ...dialogStyle,
    width: 520,
    maxHeight: '80vh',
};
const termsDialogStyle: React.CSSProperties = {
    ...dialogStyle,
    width: 720,
    maxWidth: '94vw',
    maxHeight: '92vh',
    display: 'flex',
    flexDirection: 'column',
};
const statementStyle: React.CSSProperties = {
    flex: '1 1 auto',
    minHeight: 360,
    maxHeight: 'calc(92vh - 220px)',
    overflowY: 'auto',
    whiteSpace: 'pre-wrap',
    padding: '12px 16px',
    marginBottom: 16,
    border: '1px solid var(--center-channel-color-16, rgba(61,60,64,0.16))',
    borderRadius: 4,
    fontSize: 14,
    lineHeight: 1.45,
};
const fieldStyle: React.CSSProperties = {marginBottom: 16};
const sectionStyle: React.CSSProperties = {
    borderTop: '1px solid var(--center-channel-color-16, rgba(61,60,64,0.16))',
    paddingTop: 16,
    marginTop: 8,
    marginBottom: 16,
};
const readOnlyFieldStyle: React.CSSProperties = {
    padding: '6px 12px',
    background: 'var(--center-channel-bg)',
    border: '1px solid var(--center-channel-color-16, rgba(61,60,64,0.16))',
    borderRadius: 4,
    opacity: 0.7,
};
const subFieldStyle: React.CSSProperties = {marginTop: 12};
const checkRowStyle: React.CSSProperties = {display: 'block', marginBottom: 6, fontWeight: 'normal' as const};

type HostingEnv = BotRequestPayload['hosting_env'];

const HOSTING_OPTIONS: Array<{value: HostingEnv; label: string}> = [
    {value: 'ci_cd', label: 'CI/CD pipeline'},
    {value: 'on_prem_server', label: 'On-prem server / VM'},
    {value: 'cloud_or_k8s', label: 'Cloud VM / Kubernetes'},
    {value: 'workstation', label: 'Developer workstation'},
    {value: 'external_saas', label: 'External SaaS / vendor'},
    {value: 'other', label: 'Other'},
];

const channelLabel = (ch: BotHomeChannel) => {
    const kind = ch.type === 'P' ? 'private' : 'public';
    return `${ch.display_name || ch.name} (~${ch.name}, ${kind})`;
};

const botFormLoading = 'loading';

// isSecureHTTPSURL mirrors the server's https-only checks. Requiring the
// https:// prefix already rules out javascript:, data: and file: links.
export const isSecureHTTPSURL = (raw: string): boolean => {
    const trimmed = raw.trim();
    if (!trimmed.toLowerCase().startsWith('https://')) {
        return false;
    }
    try {
        const u = new URL(trimmed);
        return u.protocol === 'https:' && Boolean(u.host);
    } catch {
        return false;
    }
};

export const RequestBotModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isBotRequestModalOpen);
    const currentChannelId = useSelector((state: GlobalState) => getCurrentChannelId(state));
    const currentTeamId = useSelector((state: GlobalState) => getCurrentTeamId(state));

    const [username, setUsername] = useState('');
    const [displayName, setDisplayName] = useState('');
    const [purpose, setPurpose] = useState('');
    const [architectureURL, setArchitectureURL] = useState('');
    const [integrationURL, setIntegrationURL] = useState('');
    const [hostingEnv, setHostingEnv] = useState<HostingEnv>('ci_cd');
    const [hostingOtherDetail, setHostingOtherDetail] = useState('');

    const [teams, setTeams] = useState<BotHomeTeam[]>([]);
    const [channels, setChannels] = useState<BotHomeChannel[]>([]);
    const [homeTeamId, setHomeTeamId] = useState('');
    const [homeChannelId, setHomeChannelId] = useState('');
    const [loadingTeams, setLoadingTeams] = useState(false);
    const [loadingChannels, setLoadingChannels] = useState(false);
    const [homeLoadError, setHomeLoadError] = useState('');

    const [requestToken, setRequestToken] = useState(true);

    const [requestIncomingWebhook, setRequestIncomingWebhook] = useState(false);
    const [incomingWebhookName, setIncomingWebhookName] = useState('');

    const [requestOutgoingWebhook, setRequestOutgoingWebhook] = useState(false);
    const [outgoingWebhookName, setOutgoingWebhookName] = useState('');
    const [outgoingCallbackURL, setOutgoingCallbackURL] = useState('');

    const [botForm, setBotForm] = useState<BotForm | null | typeof botFormLoading>(botFormLoading);
    const [termsOpen, setTermsOpen] = useState(false);
    const [termsAccepted, setTermsAccepted] = useState(false);
    const [archConfirmOpen, setArchConfirmOpen] = useState(false);

    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const reset = () => {
        setUsername('');
        setDisplayName('');
        setPurpose('');
        setArchitectureURL('');
        setIntegrationURL('');
        setHostingEnv('ci_cd');
        setHostingOtherDetail('');
        setTeams([]);
        setChannels([]);
        setHomeTeamId('');
        setHomeChannelId('');
        setHomeLoadError('');
        setRequestToken(true);
        setRequestIncomingWebhook(false);
        setIncomingWebhookName('');
        setRequestOutgoingWebhook(false);
        setOutgoingWebhookName('');
        setOutgoingCallbackURL('');
        setTermsOpen(false);
        setTermsAccepted(false);
        setArchConfirmOpen(false);
        setError('');
        setSuccess('');
        setSubmitting(false);
    };

    const close = () => {
        reset();
        dispatch(closeBotRequestModal());
    };

    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    useEffect(() => {
        if (!isOpen) {
            return undefined;
        }
        let live = true;
        setBotForm(botFormLoading);
        fetchBotForm().then((form) => {
            if (live) {
                setBotForm(form);
            }
        });
        return () => {
            live = false;
        };
    }, [isOpen]);

    useEffect(() => {
        if (!isOpen) {
            return undefined;
        }
        let cancelled = false;
        setLoadingTeams(true);
        setHomeLoadError('');
        fetchBotHomeTeams().
            then((list) => {
                if (cancelled) {
                    return;
                }
                setTeams(list);
                const preferred = list.find((t) => t.id === currentTeamId)?.id || list[0]?.id || '';
                setHomeTeamId(preferred);
            }).
            catch(() => {
                if (!cancelled) {
                    setHomeLoadError('Could not load teams.');
                }
            }).
            finally(() => {
                if (!cancelled) {
                    setLoadingTeams(false);
                }
            });
        return () => {
            cancelled = true;
        };
    }, [isOpen, currentTeamId]);

    useEffect(() => {
        if (!isOpen || !homeTeamId) {
            setChannels([]);
            setHomeChannelId('');
            return undefined;
        }
        let cancelled = false;
        setLoadingChannels(true);
        setHomeChannelId('');
        fetchBotHomeChannels(homeTeamId).
            then((list) => {
                if (cancelled) {
                    return;
                }
                setChannels(list);
                const preferred = list.find((c) => c.id === currentChannelId)?.id || list[0]?.id || '';
                setHomeChannelId(preferred);
            }).
            catch(() => {
                if (!cancelled) {
                    setChannels([]);
                    setHomeLoadError('Could not load channels for that team.');
                }
            }).
            finally(() => {
                if (!cancelled) {
                    setLoadingChannels(false);
                }
            });
        return () => {
            cancelled = true;
        };
    }, [isOpen, homeTeamId, currentChannelId]);

    if (!isOpen) {
        return null;
    }

    const selectedChannel = channels.find((c) => c.id === homeChannelId);
    const homeLabel = selectedChannel ? channelLabel(selectedChannel) : (homeChannelId || 'Select a channel');

    const handleIncomingWebhookToggle = (checked: boolean) => {
        setRequestIncomingWebhook(checked);
        if (checked && !incomingWebhookName) {
            setIncomingWebhookName(username.trim() ? username.trim() + ' incoming' : '');
        }
    };

    const handleOutgoingWebhookToggle = (checked: boolean) => {
        setRequestOutgoingWebhook(checked);
        if (checked && !outgoingWebhookName) {
            setOutgoingWebhookName(username.trim() ? username.trim() + ' outgoing' : '');
        }
    };

    const validateForm = (): string | null => {
        const u = username.trim().toLowerCase();
        if (!u) {
            return 'A bot username is required.';
        }
        if (u.length < 3 || u.length > 22) {
            return 'Bot username must be 3-22 characters.';
        }
        if (!displayName.trim()) {
            return 'A display name is required.';
        }
        if (botForm === botFormLoading) {
            return 'The form is still loading. Try again in a moment.';
        }
        if (botForm === null) {
            return 'Could not load the Terms and Conditions. Close the form and try again.';
        }
        if (!purpose.trim()) {
            return 'A purpose is required: describe what the bot does and why.';
        }
        if (architectureURL.trim() && !isSecureHTTPSURL(architectureURL)) {
            return 'Architecture link must be an https address (not http, javascript, or data URLs).';
        }
        if (!integrationURL.trim()) {
            return 'An integration endpoint is required (https URL).';
        }
        if (!isSecureHTTPSURL(integrationURL)) {
            return 'Integration endpoint must be an https address (not http, javascript, or data URLs).';
        }
        if (hostingEnv === 'other' && !hostingOtherDetail.trim()) {
            return 'Specify where the script or service runs when Hosting is Other.';
        }
        if (!homeTeamId || !homeChannelId) {
            return 'Pick a home team and channel.';
        }
        if (requestOutgoingWebhook) {
            if (!outgoingCallbackURL.trim()) {
                return 'A callback URL is required for the outgoing webhook.';
            }
            if (!outgoingCallbackURL.trim().startsWith('http://') && !outgoingCallbackURL.trim().startsWith('https://')) {
                return 'Callback URL must start with http:// or https://.';
            }
        }
        return null;
    };

    const sendRequest = async () => {
        if (botForm === botFormLoading || botForm === null) {
            return;
        }
        setSubmitting(true);
        setError('');
        try {
            const result = await submitBotRequest({
                username: username.trim(),
                display_name: displayName.trim(),
                request_token: requestToken,
                incoming_webhook_channel_id: requestIncomingWebhook ? homeChannelId : undefined,
                incoming_webhook_display_name: requestIncomingWebhook ? incomingWebhookName.trim() : undefined,
                outgoing_webhook_channel_id: requestOutgoingWebhook ? homeChannelId : undefined,
                outgoing_webhook_display_name: requestOutgoingWebhook ? outgoingWebhookName.trim() : undefined,
                outgoing_webhook_callback_url: requestOutgoingWebhook ? outgoingCallbackURL.trim() : undefined,
                purpose: purpose.trim(),
                architecture_url: architectureURL.trim() || undefined,
                home_team_id: homeTeamId,
                home_channel_id: homeChannelId,
                integration_endpoint: integrationURL.trim(),
                hosting_env: hostingEnv,
                hosting_other_detail: hostingEnv === 'other' ? hostingOtherDetail.trim() : undefined,
                ...(botForm.compliance.enabled ? {compliance_accepted: true, compliance_text_hash: botForm.compliance.sha256} : {}),
            });
            if (result.error) {
                setError(result.error);
                return;
            }
            setSuccess(result.message || 'Your bot account request has been submitted.');
        } catch {
            setError('Could not reach the server. Please check your connection and try again.');
        } finally {
            setSubmitting(false);
            setTermsOpen(false);
            setTermsAccepted(false);
            setArchConfirmOpen(false);
        }
    };

    const continueAfterArch = () => {
        setArchConfirmOpen(false);
        if (botForm !== botFormLoading && botForm?.compliance.enabled) {
            setTermsAccepted(false);
            setTermsOpen(true);
            return;
        }
        sendRequest();
    };

    const beginSubmit = () => {
        const validationError = validateForm();
        if (validationError) {
            setError(validationError);
            return;
        }
        setError('');
        if (!architectureURL.trim()) {
            setArchConfirmOpen(true);
            return;
        }
        continueAfterArch();
    };

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
                aria-labelledby='rb-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='rb-title'
                    style={{marginTop: 0}}
                >{'Request a Bot Account'}</h3>
                <p style={{opacity: 0.72}}>
                    {'Bot account requests need a security approval and a system approval from two different people. The bot is always a regular Mattermost user (never Team Admin or System Admin). On approval it is joined to the home team and channel you pick.'}
                </p>

                {success ? (
                    <div>
                        <div
                            className='alert alert-success'
                            style={{marginBottom: 16}}
                        >{success}</div>
                        <div style={{textAlign: 'right'}}>
                            <button
                                className='btn btn-primary'
                                onClick={close}
                            >{'Close'}</button>
                        </div>
                    </div>
                ) : (
                    <div>
                        <div style={fieldStyle}>
                            <label htmlFor='bot-username'>{'Bot username'}</label>
                            <input
                                id='bot-username'
                                className='form-control'
                                value={username}
                                maxLength={22}
                                placeholder='my-integration-bot'
                                onChange={(e) => setUsername(e.target.value)}
                            />
                            <small style={{opacity: 0.6}}>{'Lowercase letters, numbers, and hyphens only. 3–22 characters.'}</small>
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='bot-display-name'>{'Display name'}</label>
                            <input
                                id='bot-display-name'
                                className='form-control'
                                value={displayName}
                                maxLength={64}
                                placeholder='My Integration Bot'
                                onChange={(e) => setDisplayName(e.target.value)}
                            />
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='bot-purpose'>{'Purpose'}</label>
                            <textarea
                                id='bot-purpose'
                                className='form-control'
                                value={purpose}
                                maxLength={500}
                                rows={3}
                                placeholder='What the bot does and why'
                                onChange={(e) => setPurpose(e.target.value)}
                            />
                            <small style={{opacity: 0.6}}>{'Required. This also becomes the bot’s Mattermost profile description.'}</small>
                        </div>

                        <div style={sectionStyle}>
                            <div style={{fontWeight: 600, marginBottom: 8}}>{'Security declaration'}</div>
                            <div style={fieldStyle}>
                                <label htmlFor='bot-home-team'>{'Home team'}</label>
                                <select
                                    id='bot-home-team'
                                    className='form-control'
                                    value={homeTeamId}
                                    disabled={loadingTeams || submitting}
                                    onChange={(e) => setHomeTeamId(e.target.value)}
                                >
                                    <option value=''>{loadingTeams ? 'Loading teams…' : 'Select a team'}</option>
                                    {teams.map((t) => (
                                        <option
                                            key={t.id}
                                            value={t.id}
                                        >{t.display_name || t.name}</option>
                                    ))}
                                </select>
                            </div>
                            <div style={fieldStyle}>
                                <label htmlFor='bot-home-channel'>{'Home channel'}</label>
                                <select
                                    id='bot-home-channel'
                                    className='form-control'
                                    value={homeChannelId}
                                    disabled={!homeTeamId || loadingChannels || submitting}
                                    onChange={(e) => setHomeChannelId(e.target.value)}
                                >
                                    <option value=''>{loadingChannels ? 'Loading channels…' : 'Select a channel'}</option>
                                    {channels.map((c) => (
                                        <option
                                            key={c.id}
                                            value={c.id}
                                        >{channelLabel(c)}</option>
                                    ))}
                                </select>
                                <small style={{opacity: 0.6}}>{'Only teams and channels you belong to. The bot is auto-joined here on approval.'}</small>
                                {homeLoadError ? <div
                                    className='alert alert-danger'
                                    style={{marginTop: 8}}
                                                 >{homeLoadError}</div> : null}
                            </div>
                            <div style={fieldStyle}>
                                <label>{'Mattermost role'}</label>
                                <div style={readOnlyFieldStyle}>{'Regular user (member); never Team Admin or System Admin'}</div>
                            </div>
                            <div style={fieldStyle}>
                                <label>{'Transit security'}</label>
                                <div style={readOnlyFieldStyle}>{'The integration endpoint must be an https address (checked on submit)'}</div>
                            </div>
                            <div style={fieldStyle}>
                                <label>{'Activity logs'}</label>
                                <div style={readOnlyFieldStyle}>{'Request Hub audit history; forensic pull by bot user ID after approval'}</div>
                            </div>
                            <div style={fieldStyle}>
                                <label htmlFor='bot-arch'>{'Architecture link (optional)'}</label>
                                <input
                                    id='bot-arch'
                                    className='form-control'
                                    value={architectureURL}
                                    placeholder='https://…'
                                    onChange={(e) => setArchitectureURL(e.target.value)}
                                />
                                <small style={{opacity: 0.6}}>{'https link to an architecture or design review. Prefer including one when you have it.'}</small>
                            </div>
                            <div style={fieldStyle}>
                                <label htmlFor='bot-endpoint'>{'Integration endpoint'}</label>
                                <input
                                    id='bot-endpoint'
                                    className='form-control'
                                    value={integrationURL}
                                    placeholder='https://ci.example/hooks/mm'
                                    onChange={(e) => setIntegrationURL(e.target.value)}
                                />
                                <small style={{opacity: 0.6}}>{'Required https URL of the integration that drives this bot.'}</small>
                            </div>
                            <fieldset style={fieldStyle}>
                                <legend style={{fontSize: 14, fontWeight: 600, border: 'none', marginBottom: 4}}>{'Where does the script / service run?'}</legend>
                                {HOSTING_OPTIONS.map((opt) => (
                                    <label
                                        key={opt.value}
                                        style={checkRowStyle}
                                    >
                                        <input
                                            type='radio'
                                            name='bot-hosting'
                                            checked={hostingEnv === opt.value}
                                            onChange={() => setHostingEnv(opt.value)}
                                            disabled={submitting}
                                        />{' '}{opt.label}
                                    </label>
                                ))}
                                <small style={{opacity: 0.6}}>{'Where the integration that drives this bot runs (not which Mattermost environment).'}</small>
                            </fieldset>
                            {hostingEnv === 'other' ? (
                                <div style={fieldStyle}>
                                    <label htmlFor='bot-hosting-other'>{'Specify hosting'}</label>
                                    <input
                                        id='bot-hosting-other'
                                        className='form-control'
                                        value={hostingOtherDetail}
                                        maxLength={120}
                                        placeholder='e.g. air-gapped appliance in building A'
                                        onChange={(e) => setHostingOtherDetail(e.target.value)}
                                    />
                                </div>
                            ) : null}
                        </div>

                        <div style={sectionStyle}>
                            <label style={{display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', fontWeight: 600}}>
                                <input
                                    type='checkbox'
                                    checked={requestToken}
                                    onChange={(e) => setRequestToken(e.target.checked)}
                                />
                                {'Request an access token'}
                            </label>
                            {requestToken && (
                                <small style={{display: 'block', opacity: 0.6, marginTop: 6}}>
                                    {'An access token lets external systems authenticate as this bot. The token is delivered via DM and auto-deletes in ~72 hours.'}
                                </small>
                            )}
                        </div>

                        <div style={sectionStyle}>
                            <label style={{display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', fontWeight: 600}}>
                                <input
                                    type='checkbox'
                                    checked={requestIncomingWebhook}
                                    onChange={(e) => handleIncomingWebhookToggle(e.target.checked)}
                                />
                                {'Request an incoming webhook'}
                            </label>
                            {!requestIncomingWebhook && (
                                <small style={{display: 'block', opacity: 0.6, marginTop: 6}}>
                                    {'Posts messages into a channel from an external system via this bot.'}
                                </small>
                            )}
                            {requestIncomingWebhook && (
                                <div style={{marginTop: 12}}>
                                    <div style={subFieldStyle}>
                                        <label htmlFor='iwh-name'>{'Webhook display name'}</label>
                                        <input
                                            id='iwh-name'
                                            className='form-control'
                                            value={incomingWebhookName}
                                            maxLength={64}
                                            placeholder={username.trim() ? username.trim() + ' incoming' : 'my-bot incoming'}
                                            onChange={(e) => setIncomingWebhookName(e.target.value)}
                                        />
                                    </div>
                                    <div style={subFieldStyle}>
                                        <label>{'Target channel'}</label>
                                        <div style={readOnlyFieldStyle}>{homeLabel}</div>
                                    </div>
                                </div>
                            )}
                        </div>

                        <div style={sectionStyle}>
                            <label style={{display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', fontWeight: 600}}>
                                <input
                                    type='checkbox'
                                    checked={requestOutgoingWebhook}
                                    onChange={(e) => handleOutgoingWebhookToggle(e.target.checked)}
                                />
                                {'Request an outgoing webhook'}
                            </label>
                            {!requestOutgoingWebhook && (
                                <small style={{display: 'block', opacity: 0.6, marginTop: 6}}>
                                    {'Sends channel messages to an external URL for this bot to process.'}
                                </small>
                            )}
                            {requestOutgoingWebhook && (
                                <div style={{marginTop: 12}}>
                                    <div style={subFieldStyle}>
                                        <label htmlFor='owh-name'>{'Webhook display name'}</label>
                                        <input
                                            id='owh-name'
                                            className='form-control'
                                            value={outgoingWebhookName}
                                            maxLength={64}
                                            placeholder={username.trim() ? username.trim() + ' outgoing' : 'my-bot outgoing'}
                                            onChange={(e) => setOutgoingWebhookName(e.target.value)}
                                        />
                                    </div>
                                    <div style={subFieldStyle}>
                                        <label>{'Source channel'}</label>
                                        <div style={readOnlyFieldStyle}>{homeLabel}</div>
                                    </div>
                                    <div style={subFieldStyle}>
                                        <label htmlFor='owh-callback'>{'Callback URL'}</label>
                                        <input
                                            id='owh-callback'
                                            className='form-control'
                                            value={outgoingCallbackURL}
                                            placeholder='https://your-service.example.com/webhook'
                                            onChange={(e) => setOutgoingCallbackURL(e.target.value)}
                                        />
                                    </div>
                                </div>
                            )}
                        </div>

                        {botForm !== botFormLoading && botForm?.compliance.enabled ? (
                            <p style={{opacity: 0.72, marginBottom: 16}}>
                                {'Submitting opens the Terms and Conditions for you to review and agree.'}
                            </p>
                        ) : null}

                        {error ? <div
                            className='alert alert-danger'
                            role='alert'
                            style={{marginBottom: 16}}
                                 >{error}</div> : null}
                        <div style={{textAlign: 'right'}}>
                            <button
                                className='btn btn-tertiary'
                                style={{marginRight: 8}}
                                onClick={close}
                                disabled={submitting}
                            >{'Cancel'}</button>
                            <button
                                className='btn btn-primary'
                                onClick={beginSubmit}
                                disabled={submitting}
                            >
                                {submitting ? 'Submitting…' : 'Submit request'}
                            </button>
                        </div>
                    </div>
                )}
            </div>

            {archConfirmOpen ? (
                <div
                    style={nestedOverlayStyle}
                    onClick={(e) => e.stopPropagation()}
                    data-testid='bot-arch-confirm'
                >
                    <div
                        style={nestedDialogStyle}
                        role='dialog'
                        aria-modal={true}
                        aria-labelledby='rb-arch-title'
                        onClick={(e) => e.stopPropagation()}
                    >
                        <h3
                            id='rb-arch-title'
                            style={{marginTop: 0}}
                        >{'No architecture link'}</h3>
                        <p>{'You did not include an architecture or design review link. Prefer adding one when you have it. Submit without one?'}</p>
                        <div style={{textAlign: 'right'}}>
                            <button
                                className='btn btn-tertiary'
                                style={{marginRight: 8}}
                                onClick={() => setArchConfirmOpen(false)}
                                disabled={submitting}
                            >{'Go back'}</button>
                            <button
                                className='btn btn-primary'
                                onClick={continueAfterArch}
                                disabled={submitting}
                            >{'Submit without architecture link'}</button>
                        </div>
                    </div>
                </div>
            ) : null}

            {termsOpen && botForm !== botFormLoading && botForm?.compliance.enabled ? (
                <div
                    style={nestedOverlayStyle}
                    onClick={(e) => e.stopPropagation()}
                    data-testid='bot-terms-popup'
                >
                    <div
                        style={termsDialogStyle}
                        role='dialog'
                        aria-modal={true}
                        aria-labelledby='rb-terms-title'
                        onClick={(e) => e.stopPropagation()}
                    >
                        <h3
                            id='rb-terms-title'
                            style={{marginTop: 0, marginBottom: 12}}
                        >{'Terms and Conditions'}</h3>
                        <div
                            style={statementStyle}
                            data-testid='bot-compliance-text'
                        >{botForm.compliance.text}</div>
                        <label
                            htmlFor='bot-terms-accept'
                            style={{display: 'flex', alignItems: 'flex-start', gap: 10, marginBottom: 16, fontWeight: 'normal', cursor: 'pointer'}}
                        >
                            <input
                                id='bot-terms-accept'
                                type='checkbox'
                                checked={termsAccepted}
                                onChange={(e) => setTermsAccepted(e.target.checked)}
                                disabled={submitting}
                                style={{marginTop: 3}}
                                data-testid='bot-terms-checkbox'
                            />
                            <span>{'I have read and agree to the Terms and Conditions above.'}</span>
                        </label>
                        <div style={{textAlign: 'right'}}>
                            <button
                                className='btn btn-tertiary'
                                style={{marginRight: 8}}
                                onClick={() => {
                                    setTermsOpen(false);
                                    setTermsAccepted(false);
                                }}
                                disabled={submitting}
                            >{'Cancel'}</button>
                            <button
                                className='btn btn-primary'
                                onClick={() => {
                                    if (!termsAccepted) {
                                        setError('Check the box to agree to the Terms and Conditions.');
                                        return;
                                    }
                                    sendRequest();
                                }}
                                disabled={submitting || !termsAccepted}
                            >
                                {submitting ? 'Submitting…' : 'Agree and submit'}
                            </button>
                        </div>
                    </div>
                </div>
            ) : null}
        </div>
    );
};
