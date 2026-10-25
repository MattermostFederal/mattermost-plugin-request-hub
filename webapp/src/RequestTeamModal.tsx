import React, {useMemo, useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {submitTeamCreationRequest} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {closeTeamCreationModal, isTeamCreationModalOpen} from './store';

// Slugify mirrors server/team_request.go slugify() so the live URL preview
// matches exactly what the server will produce.
function slugify(input: string): string {
    return input.
        toLowerCase().
        trim().
        replace(/[^a-z0-9-]/g, '-').
        replace(/-+/g, '-').
        replace(/^-|-$/g, '');
}

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

export const RequestTeamModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isTeamCreationModalOpen);

    const [displayName, setDisplayName] = useState('');
    const [urlName, setUrlName] = useState('');
    const [description, setDescription] = useState('');
    const [teamType, setTeamType] = useState('O');
    const [requestTeamAdmin, setRequestTeamAdmin] = useState(false);
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const previewURL = useMemo(() => {
        const slug = slugify(urlName || displayName);
        return slug || 'team-url';
    }, [urlName, displayName]);

    const reset = () => {
        setDisplayName('');
        setUrlName('');
        setDescription('');
        setTeamType('O');
        setRequestTeamAdmin(false);
        setError('');
        setSuccess('');
        setSubmitting(false);
    };

    const close = () => {
        reset();
        dispatch(closeTeamCreationModal());
    };

    // Must run unconditionally — never after `if (!isOpen) return null`.
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

    const submit = async () => {
        if (!displayName.trim()) {
            setError('A team name is required.');
            return;
        }

        setSubmitting(true);
        setError('');

        try {
            const result = await submitTeamCreationRequest({
                display_name: displayName.trim(),
                name: urlName.trim(),
                type: teamType,
                description: description.trim(),
                request_team_admin: requestTeamAdmin,
            });

            if (result.error) {
                setError(result.error);
                return;
            }

            setSuccess(result.message || 'Your team request has been submitted.');
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
                aria-labelledby='rt-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='rt-title'
                    style={{marginTop: 0}}
                >{'Request a New Team'}</h3>
                <p style={{opacity: 0.72}}>
                    {'System Admin approval is required to create a new team.'}
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
                            <label htmlFor='rt-display-name'>{'Team name'}</label>
                            <input
                                id='rt-display-name'
                                className='form-control'
                                value={displayName}
                                maxLength={64}
                                placeholder='e.g. Marketing'
                                onChange={(e) => setDisplayName(e.target.value)}
                            />
                        </div>

                        <div style={fieldStyle}>
                            <label htmlFor='rt-url-name'>{'Team URL (optional)'}</label>
                            <input
                                id='rt-url-name'
                                className='form-control'
                                value={urlName}
                                maxLength={64}
                                placeholder='Leave blank to generate from team name'
                                onChange={(e) => setUrlName(e.target.value)}
                            />
                            <small style={{opacity: 0.6, display: 'block', marginTop: 4}}>
                                {'Preview: '}<code>{previewURL}</code>
                            </small>
                        </div>

                        <div style={fieldStyle}>
                            <label htmlFor='rt-description'>{'Description (optional)'}</label>
                            <textarea
                                id='rt-description'
                                className='form-control'
                                value={description}
                                maxLength={250}
                                rows={2}
                                placeholder='What is this team for?'
                                onChange={(e) => setDescription(e.target.value)}
                            />
                        </div>

                        <div style={fieldStyle}>
                            <label htmlFor='rt-type'>{'Visibility'}</label>
                            <select
                                id='rt-type'
                                className='form-control'
                                value={teamType}
                                onChange={(e) => setTeamType(e.target.value)}
                            >
                                <option value='O'>{'Open — anyone can join'}</option>
                                <option value='I'>{'Invite-only — members must be invited'}</option>
                            </select>
                        </div>

                        <div style={{...fieldStyle, display: 'flex', alignItems: 'center', gap: 8}}>
                            <input
                                id='rt-team-admin'
                                type='checkbox'
                                checked={requestTeamAdmin}
                                onChange={(e) => setRequestTeamAdmin(e.target.checked)}
                            />
                            <label
                                htmlFor='rt-team-admin'
                                style={{marginBottom: 0, cursor: 'pointer'}}
                            >
                                {'I want to be Team Admin of this team'}
                            </label>
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
