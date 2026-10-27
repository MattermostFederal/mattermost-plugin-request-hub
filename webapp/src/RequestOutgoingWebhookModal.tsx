import React, {useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {submitOutgoingWebhookRequest} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {
    closeOutgoingWebhookModal,
    getChannelType,
    getCurrentChannelId,
    getCurrentTeamDisplayName,
    getChannelDisplayName,
    isOutgoingWebhookModalOpen,
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
    width: 512,
    maxWidth: '90vw',
    maxHeight: '90vh',
    overflowY: 'auto',
    padding: 24,
    boxShadow: '0 8px 24px rgba(0, 0, 0, 0.24)',
};
const fieldStyle: React.CSSProperties = {marginBottom: 16};

export const RequestOutgoingWebhookModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isOutgoingWebhookModalOpen);
    const channelId = useSelector((state: GlobalState) => getCurrentChannelId(state));
    const channelName = useSelector((state: GlobalState) => getChannelDisplayName(state, channelId));
    const channelType = useSelector((state: GlobalState) => getChannelType(state, channelId));
    const teamName = useSelector((state: GlobalState) => getCurrentTeamDisplayName(state));
    const inPublicChannel = channelType === 'O';

    const [displayName, setDisplayName] = useState('');
    const [callbackUrl, setCallbackUrl] = useState('');
    const [description, setDescription] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const reset = () => {
        setDisplayName('');
        setCallbackUrl('');
        setDescription('');
        setError('');
        setSuccess('');
        setSubmitting(false);
    };
    const close = () => {
        reset();
        dispatch(closeOutgoingWebhookModal());
    };

    // Must run unconditionally — never after `if (!isOpen) return null`.
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

    const submit = async () => {
        if (!displayName.trim()) {
            setError('A display name is required.');
            return;
        }
        if (!callbackUrl.trim()) {
            setError('A callback URL is required.');
            return;
        }
        if (!callbackUrl.trim().startsWith('http://') && !callbackUrl.trim().startsWith('https://')) {
            setError('Callback URL must start with http:// or https://');
            return;
        }
        if (!inPublicChannel) {
            setError('Open a public channel on this team first, then submit. Outgoing webhooks can only use public channels.');
            return;
        }

        setSubmitting(true);
        setError('');
        try {
            const result = await submitOutgoingWebhookRequest({
                channel_id: channelId,
                display_name: displayName.trim(),
                callback_url: callbackUrl.trim(),
                description: description.trim() || undefined,
            });
            if (result.error) {
                setError(result.error);
                return;
            }
            setSuccess(result.message || 'Your outgoing webhook request has been submitted.');
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
                aria-labelledby='row-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='row-title'
                    style={{marginTop: 0}}
                >{'Request an Outgoing Webhook'}</h3>
                <p style={{opacity: 0.72}}>{'Outgoing webhook requests are reviewed by System Admins.'}</p>

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
                            <label>{'Source channel'}</label>
                            <div
                                style={{
                                    padding: '6px 12px',
                                    background: 'var(--center-channel-bg)',
                                    border: '1px solid var(--center-channel-color-16, rgba(61,60,64,0.16))',
                                    borderRadius: 4,
                                    opacity: 0.7,
                                }}
                            >
                                {channelName || channelId}
                                {teamName ? <span style={{marginLeft: 6, fontSize: 12}}>{`(Team: ${teamName})`}</span> : null}
                            </div>
                            <small style={{opacity: 0.6}}>
                                {inPublicChannel ?
                                    'The webhook will fire on messages in this channel. Switch channels before opening the form to change the source.' :
                                    'Open a public channel on this team before submitting. Outgoing webhooks can only use public channels.'}
                            </small>
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='owh-display-name'>{'Display name'}</label>
                            <input
                                id='owh-display-name'
                                className='form-control'
                                value={displayName}
                                maxLength={64}
                                placeholder='e.g. Alerting Bridge'
                                onChange={(e) => setDisplayName(e.target.value)}
                            />
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='owh-callback-url'>{'Callback URL'}</label>
                            <input
                                id='owh-callback-url'
                                className='form-control'
                                value={callbackUrl}
                                maxLength={1024}
                                placeholder='https://example.com/webhook'
                                onChange={(e) => setCallbackUrl(e.target.value)}
                            />
                            <small style={{opacity: 0.6}}>{'The endpoint that will receive POST requests from Mattermost.'}</small>
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='owh-description'>{'Description (optional)'}</label>
                            <textarea
                                id='owh-description'
                                className='form-control'
                                value={description}
                                maxLength={500}
                                rows={2}
                                placeholder='What does this webhook do? What system receives events?'
                                onChange={(e) => setDescription(e.target.value)}
                            />
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
