import React, {useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {submitTeamGuestRequest} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {
    closeTeamGuestRequestModal,
    getCurrentChannelId,
    getCurrentTeamDisplayName,
    getCurrentTeamId,
    isTeamGuestRequestModalOpen,
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

export const RequestTeamGuestModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isTeamGuestRequestModalOpen);
    const teamId = useSelector((state: GlobalState) => getCurrentTeamId(state));
    const teamDisplayName = useSelector((state: GlobalState) => getCurrentTeamDisplayName(state));
    const channelId = useSelector((state: GlobalState) => getCurrentChannelId(state));

    const [email, setEmail] = useState('');
    const [reason, setReason] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const reset = () => {
        setEmail('');
        setReason('');
        setError('');
        setSuccess('');
        setSubmitting(false);
    };

    const close = () => {
        reset();
        dispatch(closeTeamGuestRequestModal());
    };
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

    const submit = async () => {
        if (!teamId) {
            setError('No active team found. Please navigate to the team and try again.');
            return;
        }
        if (!channelId) {
            setError('Open a channel in this team first — the guest needs at least one channel.');
            return;
        }
        const guestEmail = email.trim();
        if (!guestEmail || !guestEmail.includes('@')) {
            setError('Enter a valid guest email. Mattermost will email a channel-scoped guest invitation on approve.');
            return;
        }

        setSubmitting(true);
        setError('');
        try {
            const result = await submitTeamGuestRequest({
                team_id: teamId,
                email: guestEmail,
                channel_ids: [channelId],
                reason: reason.trim() || undefined,
            });
            if (result.error) {
                setError(result.error);
                return;
            }
            setSuccess(result.message || 'Your guest invite request has been submitted.');
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
                aria-labelledby='rtg-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='rtg-title'
                    style={{marginTop: 0}}
                >{'Request Guest Invite'}</h3>
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
                        <p style={{opacity: 0.8}}>
                            {'Request a guest invite for '}
                            <strong>{teamDisplayName || 'this team'}</strong>
                            {'. On approval, Mattermost emails a channel-scoped guest invitation (not a regular team invite link). Guest Accounts must be enabled.'}
                        </p>
                        <p style={{opacity: 0.72, fontSize: 13}}>
                            {'The current channel will be included as the guest\'s initial channel access.'}
                        </p>
                        <div style={fieldStyle}>
                            <label htmlFor='rtg-email'>{'Guest email'}</label>
                            <input
                                id='rtg-email'
                                className='form-control'
                                type='email'
                                value={email}
                                onChange={(e) => setEmail(e.target.value)}
                                disabled={submitting}
                                placeholder='guest@example.com'
                                required={true}
                            />
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='rtg-reason'>{'Message / reason (optional)'}</label>
                            <textarea
                                id='rtg-reason'
                                className='form-control'
                                rows={3}
                                value={reason}
                                onChange={(e) => setReason(e.target.value)}
                                disabled={submitting}
                            />
                        </div>
                        {error ? (
                            <div
                                className='alert alert-danger'
                                role='alert'
                                style={{marginBottom: 16}}
                            >{error}</div>
                        ) : null}
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
                                disabled={submitting || !teamId || !channelId}
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
