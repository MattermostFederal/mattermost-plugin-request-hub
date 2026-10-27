import React, {useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {submitBotRequest} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {
    closeBotRequestModal,
    getCurrentChannelId,
    getCurrentTeamDisplayName,
    getChannelDisplayName,
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
    width: 540,
    maxWidth: '90vw',
    maxHeight: '90vh',
    overflowY: 'auto',
    padding: 24,
    boxShadow: '0 8px 24px rgba(0, 0, 0, 0.24)',
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

export const RequestBotModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isBotRequestModalOpen);
    const channelId = useSelector((state: GlobalState) => getCurrentChannelId(state));
    const channelName = useSelector((state: GlobalState) => getChannelDisplayName(state, channelId));
    const teamName = useSelector((state: GlobalState) => getCurrentTeamDisplayName(state));

    const [username, setUsername] = useState('');
    const [displayName, setDisplayName] = useState('');
    const [description, setDescription] = useState('');

    const [requestToken, setRequestToken] = useState(true);

    const [requestIncomingWebhook, setRequestIncomingWebhook] = useState(false);
    const [incomingWebhookName, setIncomingWebhookName] = useState('');

    const [requestOutgoingWebhook, setRequestOutgoingWebhook] = useState(false);
    const [outgoingWebhookName, setOutgoingWebhookName] = useState('');
    const [outgoingCallbackURL, setOutgoingCallbackURL] = useState('');

    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const reset = () => {
        setUsername('');
        setDisplayName('');
        setDescription('');
        setRequestToken(true);
        setRequestIncomingWebhook(false);
        setIncomingWebhookName('');
        setRequestOutgoingWebhook(false);
        setOutgoingWebhookName('');
        setOutgoingCallbackURL('');
        setError('');
        setSuccess('');
        setSubmitting(false);
    };

    const close = () => {
        reset();
        dispatch(closeBotRequestModal());
    };

    // Must run unconditionally — never after `if (!isOpen) return null`.
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

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

    const submit = async () => {
        const u = username.trim().toLowerCase();
        if (!u) {
            setError('A bot username is required.');
            return;
        }
        if (u.length < 3 || u.length > 22) {
            setError('Bot username must be 3-22 characters.');
            return;
        }
        if (!displayName.trim()) {
            setError('A display name is required.');
            return;
        }
        if (requestOutgoingWebhook) {
            if (!outgoingCallbackURL.trim()) {
                setError('A callback URL is required for the outgoing webhook.');
                return;
            }
            if (!outgoingCallbackURL.trim().startsWith('http://') && !outgoingCallbackURL.trim().startsWith('https://')) {
                setError('Callback URL must start with http:// or https://.');
                return;
            }
        }

        setSubmitting(true);
        setError('');
        try {
            const result = await submitBotRequest({
                username: username.trim(),
                display_name: displayName.trim(),
                description: description.trim() || undefined,
                request_token: requestToken,
                incoming_webhook_channel_id: requestIncomingWebhook ? channelId : undefined,
                incoming_webhook_display_name: requestIncomingWebhook ? incomingWebhookName.trim() : undefined,
                outgoing_webhook_channel_id: requestOutgoingWebhook ? channelId : undefined,
                outgoing_webhook_display_name: requestOutgoingWebhook ? outgoingWebhookName.trim() : undefined,
                outgoing_webhook_callback_url: requestOutgoingWebhook ? outgoingCallbackURL.trim() : undefined,
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
        }
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
                <p style={{opacity: 0.72}}>{'Bot account requests need a security approval and a system approval.'}</p>

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
                            <label htmlFor='bot-description'>{'Description (optional)'}</label>
                            <textarea
                                id='bot-description'
                                className='form-control'
                                value={description}
                                maxLength={500}
                                rows={3}
                                placeholder='What does this bot do? What systems will it integrate with?'
                                onChange={(e) => setDescription(e.target.value)}
                            />
                        </div>

                        {/* Access token section */}
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

                        {/* Incoming webhook section */}
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
                                        <div style={readOnlyFieldStyle}>
                                            {channelName || channelId}
                                            {teamName ? <span style={{opacity: 0.6, marginLeft: 6}}>{`(Team: ${teamName})`}</span> : null}
                                        </div>
                                        <small style={{opacity: 0.6}}>{'Switch to a different channel before opening this form to change the target.'}</small>
                                    </div>
                                </div>
                            )}
                        </div>

                        {/* Outgoing webhook section */}
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
                                        <div style={readOnlyFieldStyle}>
                                            {channelName || channelId}
                                            {teamName ? <span style={{opacity: 0.6, marginLeft: 6}}>{`(Team: ${teamName})`}</span> : null}
                                        </div>
                                        <small style={{opacity: 0.6}}>{'Switch to a different channel before opening this form to change the source.'}</small>
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
                                onClick={submit}
                                disabled={submitting}
                            >
                                {submitting ? 'Submitting…' : 'Submit request'}
                            </button>
                        </div>
                    </div>
                )}
            </div>
        </div>
    );
};
