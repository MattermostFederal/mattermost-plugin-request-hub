import React, {useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {submitAdminRequest} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {MemberPicker} from './MemberPicker';
import {closeAdminRequestModal, getAdminModalChannelId, getChannelDisplayName, getCurrentTeamId} from './store';
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

// RequestChannelAdminModal lets a channel member request that one or more
// people be made Channel Admin on the current channel. The request is sent to
// an admin for approval, reusing the same approval flow as channel-creation
// requests.
export const RequestChannelAdminModal = () => {
    const dispatch = useDispatch();
    const channelId = useSelector(getAdminModalChannelId);
    const teamId = useSelector((state: GlobalState) => getCurrentTeamId(state));
    const channelName = useSelector((state: GlobalState) => (channelId ? getChannelDisplayName(state, channelId) : ''));

    const [nomineesText, setNomineesText] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const reset = () => {
        setNomineesText('');
        setError('');
        setSuccess('');
        setSubmitting(false);
    };

    const close = () => {
        reset();
        dispatch(closeAdminRequestModal());
    };

    // Must run unconditionally — never after `if (!channelId) return null`.
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, Boolean(channelId));

    if (!channelId) {
        return null;
    }

    const submit = async () => {
        const nominees = nomineesText.
            split(',').
            map((m) => m.trim().replace(/^@/, '')).
            filter((m) => m.length > 0);

        if (nominees.length === 0) {
            setError('Pick at least one person to make a Channel Admin.');
            return;
        }

        setSubmitting(true);
        setError('');

        const result = await submitAdminRequest({
            channel_id: channelId,
            nominees,
        });

        setSubmitting(false);

        if (result.error) {
            setError(result.error);
            return;
        }

        setSuccess(result.message || 'Your Channel Admin request has been submitted.');
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
                aria-labelledby='rca-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='rca-title'
                    style={{marginTop: 0}}
                >{'Request a Channel Admin'}</h3>
                <p style={{opacity: 0.72}}>
                    {'Request that someone be made a Channel Admin'}
                    {channelName ? <>{' for '}<strong>{channelName}</strong></> : null}
                    {'. Your request will be sent to an admin for approval.'}
                </p>

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
                        <div style={fieldStyle}>
                            <label htmlFor='cra-nominees'>{'Make Channel Admin'}</label>
                            <MemberPicker
                                value={nomineesText}
                                teamId={teamId}
                                placeholder='Type a name — Tab / Enter to add'
                                onChange={setNomineesText}
                            />
                            <small style={{opacity: 0.6, display: 'block', marginTop: 4}}>
                                {'These users are promoted to Channel Admin once an admin approves. They must already be members of the channel.'}
                            </small>
                        </div>

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
