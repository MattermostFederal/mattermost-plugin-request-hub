import React, {useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {submitTeamAdminRequest} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {
    closeTeamAdminRequestModal,
    getCurrentTeamDisplayName,
    getCurrentTeamId,
    isTeamAdminRequestModalOpen,
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
    width: 480,
    maxWidth: '90vw',
    padding: 24,
    boxShadow: '0 8px 24px rgba(0, 0, 0, 0.24)',
};

export const RequestTeamAdminModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isTeamAdminRequestModalOpen);
    const teamId = useSelector((state: GlobalState) => getCurrentTeamId(state));
    const teamDisplayName = useSelector((state: GlobalState) => getCurrentTeamDisplayName(state));

    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const reset = () => {
        setError('');
        setSuccess('');
        setSubmitting(false);
    };

    const close = () => {
        reset();
        dispatch(closeTeamAdminRequestModal());
    };

    // Must run unconditionally — never after `if (!isOpen) return null`.
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

    const submit = async () => {
        if (!teamId) {
            setError('No active team found. Please navigate to the team and try again.');
            return;
        }

        setSubmitting(true);
        setError('');

        try {
            const result = await submitTeamAdminRequest({team_id: teamId});

            if (result.error) {
                setError(result.error);
                return;
            }

            setSuccess(result.message || 'Your Team Admin request has been submitted.');
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
                aria-labelledby='rta-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='rta-title'
                    style={{marginTop: 0}}
                >{'Request Team Admin'}</h3>

                {success ? (
                    <div>
                        <div
                            className='alert alert-success'
                            style={{marginBottom: 16}}
                        >
                            {success}
                        </div>
                        <div style={{textAlign: 'right'}}>
                            <button
                                className='btn btn-primary'
                                onClick={close}
                            >
                                {'Close'}
                            </button>
                        </div>
                    </div>
                ) : (
                    <div>
                        <p style={{opacity: 0.8}}>
                            {'You are requesting Team Admin access for '}
                            <strong>{teamDisplayName || 'this team'}</strong>
                            {'. A System Admin will review your request.'}
                        </p>
                        <p style={{opacity: 0.72, fontSize: 13}}>
                            {'Team Admins can manage members, create channels, and configure team settings.'}
                        </p>

                        {error ? (
                            <div
                                className='alert alert-danger'
                                role='alert'
                                style={{marginBottom: 16}}
                            >
                                {error}
                            </div>
                        ) : null}

                        <div style={{textAlign: 'right'}}>
                            <button
                                className='btn btn-tertiary'
                                style={{marginRight: 8}}
                                onClick={close}
                                disabled={submitting}
                            >
                                {'Cancel'}
                            </button>
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
