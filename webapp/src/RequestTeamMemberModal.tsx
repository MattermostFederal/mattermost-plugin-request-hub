import React, {useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {submitTeamMemberRequest} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {MemberPicker} from './MemberPicker';
import {
    closeTeamMemberRequestModal,
    getCurrentTeamDisplayName,
    getCurrentTeamId,
    isTeamMemberRequestModalOpen,
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

export const RequestTeamMemberModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isTeamMemberRequestModalOpen);
    const teamId = useSelector((state: GlobalState) => getCurrentTeamId(state));
    const teamDisplayName = useSelector((state: GlobalState) => getCurrentTeamDisplayName(state));

    const [nomineesText, setNomineesText] = useState('');
    const [reason, setReason] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const reset = () => {
        setNomineesText('');
        setReason('');
        setError('');
        setSuccess('');
        setSubmitting(false);
    };

    const close = () => {
        reset();
        dispatch(closeTeamMemberRequestModal());
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
        const nominees = nomineesText.
            split(',').
            map((m) => m.trim().replace(/^@/, '')).
            filter((m) => m.length > 0);
        if (nominees.length === 0) {
            setError('Pick at least one person to add to the team.');
            return;
        }

        setSubmitting(true);
        setError('');
        try {
            const result = await submitTeamMemberRequest({
                team_id: teamId,
                nominees,
                reason: reason.trim() || undefined,
            });
            if (result.error) {
                setError(result.error);
                return;
            }
            setSuccess(result.message || 'Your team member request has been submitted.');
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
                aria-labelledby='rtm-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='rtm-title'
                    style={{marginTop: 0}}
                >{'Request Team Members'}</h3>
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
                            {'Nominate existing Mattermost users to be added to '}
                            <strong>{teamDisplayName || 'this team'}</strong>
                            {'. A System Admin will review the request.'}
                        </p>
                        <p
                            style={{
                                opacity: 0.85,
                                fontSize: 13,
                                marginTop: -4,
                                marginBottom: 16,
                                padding: '8px 10px',
                                borderRadius: 4,
                                background: 'rgba(var(--center-channel-color-rgb, 63, 67, 80), 0.08)',
                            }}
                        >
                            <strong>{'Bots: '}</strong>
                            {'To put an existing bot on this team, search for the bot username here and submit the same way. Create the bot first (Request a Bot) if it does not exist yet. This flow only adds accounts that already exist in Mattermost.'}
                        </p>
                        <div style={fieldStyle}>
                            <label htmlFor='rtm-nominees'>{'Users or bots to add'}</label>
                            <MemberPicker
                                value={nomineesText}
                                notInTeamId={teamId}
                                onChange={setNomineesText}
                                disabled={submitting}
                                placeholder='Type a user or bot username — Tab / Enter to add'
                            />
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='rtm-reason'>{'Reason (optional)'}</label>
                            <textarea
                                id='rtm-reason'
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
                                disabled={submitting || !teamId}
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
