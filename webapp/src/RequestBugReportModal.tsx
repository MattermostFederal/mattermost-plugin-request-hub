import React, {useEffect, useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {KIND_OPTIONS} from './AuditHistoryModal';
import {fetchMyRequests, submitBugReportRequest} from './client';
import type {MyRequestSummary} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {closeBugReportModal, isBugReportModalOpen} from './store';

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
const labelStyle: React.CSSProperties = {display: 'block', fontWeight: 600, marginBottom: 4, fontSize: 14};

// About-area options for bug reports. Exclude bug_report itself (the server
// rejects it) and keep transfer kinds that KIND_OPTIONS already lists.
const ABOUT_OPTIONS: Array<[string, string]> = [
    ['general', 'General / other'],
    ...KIND_OPTIONS.filter(([value]) => value !== 'bug_report'),
];

export const RequestBugReportModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isBugReportModalOpen);

    const [category, setCategory] = useState('bug');
    const [aboutKind, setAboutKind] = useState('general');
    const [aboutOptions, setAboutOptions] = useState('');
    const [relatedRequestID, setRelatedRequestID] = useState('');
    const [myRequests, setMyRequests] = useState<MyRequestSummary[]>([]);
    const [description, setDescription] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    useEffect(() => {
        if (!isOpen) {
            return undefined;
        }
        let cancelled = false;
        (async () => {
            try {
                const list = await fetchMyRequests();
                if (!cancelled) {
                    setMyRequests(list);
                }
            } catch {
                if (!cancelled) {
                    setMyRequests([]);
                }
            }
        })();
        return () => {
            cancelled = true;
        };
    }, [isOpen]);

    const reset = () => {
        setCategory('bug');
        setAboutKind('general');
        setAboutOptions('');
        setRelatedRequestID('');
        setDescription('');
        setError('');
        setSuccess('');
        setSubmitting(false);
    };

    const close = () => {
        // A submit still in flight would land on the next open's state.
        if (submitting) {
            return;
        }
        reset();
        dispatch(closeBugReportModal());
    };

    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

    const submit = async () => {
        if (submitting) {
            return;
        }
        if (!description.trim()) {
            setError('A description is required.');
            return;
        }
        setSubmitting(true);
        setError('');
        try {
            const result = await submitBugReportRequest({
                category,
                about_kind: aboutKind,
                about_options: aboutOptions.trim(),
                related_request_id: relatedRequestID.trim(),
                description: description.trim(),
            });
            if (result.error) {
                setError(result.error);
                return;
            }
            setSuccess(result.message || 'Your report was submitted.');
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
                aria-labelledby='bug-report-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <div style={{display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start'}}>
                    <h3
                        id='bug-report-title'
                        style={{marginTop: 0, marginBottom: 4}}
                    >{'Report a Bug or Feedback'}</h3>
                    <button
                        style={{background: 'none', border: 'none', cursor: 'pointer', fontSize: 20, lineHeight: 1, opacity: 0.6, padding: 0}}
                        onClick={close}
                        aria-label='Close'
                    >
                        {'×'}
                    </button>
                </div>
                <p style={{marginTop: 0, marginBottom: 16, opacity: 0.72, fontSize: 14}}>
                    {'Tell System Admins about a Request Hub problem or suggestion. This is not an approval ticket.'}
                </p>

                {success ? (
                    <div>
                        <p>{success}</p>
                        <button
                            className='btn btn-primary'
                            onClick={close}
                        >{'Done'}</button>
                    </div>
                ) : (
                    <>
                        <div style={fieldStyle}>
                            <label
                                style={labelStyle}
                                htmlFor='bug-category'
                            >{'Report type'}</label>
                            <select
                                id='bug-category'
                                className='form-control'
                                value={category}
                                onChange={(e) => setCategory(e.target.value)}
                            >
                                <option value='bug'>{'Bug'}</option>
                                <option value='feedback'>{'Feedback'}</option>
                            </select>
                        </div>
                        <div style={fieldStyle}>
                            <label
                                style={labelStyle}
                                htmlFor='bug-about'
                            >{'About which area'}</label>
                            <select
                                id='bug-about'
                                className='form-control'
                                value={aboutKind}
                                onChange={(e) => setAboutKind(e.target.value)}
                            >
                                {ABOUT_OPTIONS.map(([value, label]) => (
                                    <option
                                        key={value}
                                        value={value}
                                    >{label}</option>
                                ))}
                            </select>
                        </div>
                        <div style={fieldStyle}>
                            <label
                                style={labelStyle}
                                htmlFor='bug-options'
                            >{'What you selected'}</label>
                            <textarea
                                id='bug-options'
                                className='form-control'
                                rows={2}
                                value={aboutOptions}
                                onChange={(e) => setAboutOptions(e.target.value)}
                                placeholder='Prefix, team, channel, bot name, or other options from the form'
                                maxLength={500}
                            />
                        </div>
                        <div style={fieldStyle}>
                            <label
                                style={labelStyle}
                                htmlFor='bug-related'
                            >{'Related request'}</label>
                            {myRequests.length > 0 ? (
                                <select
                                    id='bug-related'
                                    className='form-control'
                                    value={relatedRequestID}
                                    onChange={(e) => setRelatedRequestID(e.target.value)}
                                    style={{marginBottom: 8}}
                                >
                                    <option value=''>{'None / enter an ID below'}</option>
                                    {myRequests.map((r) => (
                                        <option
                                            key={r.id}
                                            value={r.id}
                                        >{`${r.kind}: ${r.summary.replace(/\*\*/g, '')} (${r.id})`}</option>
                                    ))}
                                </select>
                            ) : null}
                            <input
                                id='bug-related-id'
                                className='form-control'
                                value={relatedRequestID}
                                onChange={(e) => setRelatedRequestID(e.target.value)}
                                placeholder='Optional request ID (your own only)'
                                aria-label='Related request ID'
                                maxLength={26}
                            />
                        </div>
                        <div style={fieldStyle}>
                            <label
                                style={labelStyle}
                                htmlFor='bug-description'
                            >{'Description'}</label>
                            <textarea
                                id='bug-description'
                                className='form-control'
                                rows={4}
                                value={description}
                                onChange={(e) => setDescription(e.target.value)}
                                placeholder='What went wrong, or what would you change?'
                                maxLength={500}
                                required={true}
                            />
                        </div>
                        {error ? (
                            <p
                                style={{color: 'var(--error-text, #d24b4e)', marginBottom: 12}}
                                role='alert'
                            >{error}</p>
                        ) : null}
                        <div style={{display: 'flex', justifyContent: 'flex-end', gap: 8}}>
                            <button
                                className='btn btn-tertiary'
                                onClick={close}
                                disabled={submitting}
                            >{'Cancel'}</button>
                            <button
                                className='btn btn-primary'
                                onClick={submit}
                                disabled={submitting}
                            >{submitting ? 'Submitting…' : 'Submit report'}</button>
                        </div>
                    </>
                )}
            </div>
        </div>
    );
};
