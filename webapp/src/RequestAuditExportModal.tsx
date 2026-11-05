import React, {useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {KIND_OPTIONS} from './AuditHistoryModal';
import {submitAuditExportRequest} from './client';
import type {AuditExportFormat} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {closeAuditExportRequestModal, isAuditExportRequestModalOpen} from './store';

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
const legendStyle: React.CSSProperties = {fontSize: 14, fontWeight: 600, border: 'none', marginBottom: 4};

type ExportType = {
    value: string;
    label: string;
    formats: Array<[AuditExportFormat, string]>;
};

// EXPORT_TYPES are the exports the server builds and the formats each comes
// in, the default first. They match auditExportTypes on the server.
export const EXPORT_TYPES: ExportType[] = [
    {
        value: 'full_histories',
        label: 'Request histories',
        formats: [['csv', 'CSV (one row per event)'], ['json', 'JSON (one object per request)']],
    },
];

// dayStart and dayEnd turn a date field (YYYY-MM-DD, local time) into Unix
// milliseconds at the start or end of that day; '' is no limit.
const dayStart = (value: string) => (value ? new Date(`${value}T00:00:00`).getTime() : undefined);
const dayEnd = (value: string) => (value ? new Date(`${value}T23:59:59.999`).getTime() : undefined);

// RequestAuditExportModal is how a security approver who isn't a System
// Admin asks for a copy of the audit history. A System Admin approves it,
// and the file is posted in the requester's request thread.
export const RequestAuditExportModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isAuditExportRequestModalOpen);

    const [exportType, setExportType] = useState(EXPORT_TYPES[0].value);
    const [format, setFormat] = useState<AuditExportFormat>(EXPORT_TYPES[0].formats[0][0]);
    const [kind, setKind] = useState('');
    const [requester, setRequester] = useState('');
    const [since, setSince] = useState('');
    const [until, setUntil] = useState('');
    const [reason, setReason] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const close = () => {
        setExportType(EXPORT_TYPES[0].value);
        setFormat(EXPORT_TYPES[0].formats[0][0]);
        setKind('');
        setRequester('');
        setSince('');
        setUntil('');
        setReason('');
        setError('');
        setSuccess('');
        setSubmitting(false);
        dispatch(closeAuditExportRequestModal());
    };
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

    const currentType = EXPORT_TYPES.find((t) => t.value === exportType) ?? EXPORT_TYPES[0];

    // A new type keeps the format when the type comes in it.
    const chooseType = (t: ExportType) => {
        setExportType(t.value);
        if (!t.formats.some(([value]) => value === format)) {
            setFormat(t.formats[0][0]);
        }
    };

    const submit = async () => {
        if (since && until && since > until) {
            setError('The start date must be before the end date.');
            return;
        }
        setSubmitting(true);
        setError('');
        try {
            const result = await submitAuditExportRequest({
                type: exportType,
                format,
                kind: kind || undefined,
                requester: requester.trim().replace(/^@/, '') || undefined,
                since: dayStart(since),
                until: dayEnd(until),
                reason: reason.trim(),
            });
            if (result.error) {
                setError(result.error);
                return;
            }
            setSuccess(result.message || 'Your request has been sent to the System Admins.');
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
                aria-labelledby='rae-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='rae-title'
                    style={{marginTop: 0}}
                >{'Request Audit Export'}</h3>
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
                            {'Ask for a copy of the request audit history. A System Admin reviews the request; once approved, the file is posted in your request thread with the Request Hub bot. Leave the filters empty for the whole history.'}
                        </p>
                        {EXPORT_TYPES.length > 1 ? (
                            <fieldset style={fieldStyle}>
                                <legend style={legendStyle}>{'Export'}</legend>
                                {EXPORT_TYPES.map((t) => (
                                    <label
                                        key={t.value}
                                        style={{marginRight: 16, fontWeight: 'normal'}}
                                    >
                                        <input
                                            type='radio'
                                            name='rae-type'
                                            checked={exportType === t.value}
                                            onChange={() => chooseType(t)}
                                            disabled={submitting}
                                        />{` ${t.label}`}
                                    </label>
                                ))}
                            </fieldset>
                        ) : null}
                        <fieldset style={fieldStyle}>
                            <legend style={legendStyle}>{'Format'}</legend>
                            {currentType.formats.map(([value, label]) => (
                                <label
                                    key={value}
                                    style={{marginRight: 16, fontWeight: 'normal'}}
                                >
                                    <input
                                        type='radio'
                                        name='rae-format'
                                        checked={format === value}
                                        onChange={() => setFormat(value)}
                                        disabled={submitting}
                                    />{` ${label}`}
                                </label>
                            ))}
                        </fieldset>
                        <div style={fieldStyle}>
                            <label htmlFor='rae-kind'>{'Request type (optional)'}</label>
                            <select
                                id='rae-kind'
                                className='form-control'
                                value={kind}
                                onChange={(e) => setKind(e.target.value)}
                                disabled={submitting}
                            >
                                <option value=''>{'All types'}</option>
                                {KIND_OPTIONS.map(([value, label]) => (
                                    <option
                                        key={value}
                                        value={value}
                                    >{label}</option>
                                ))}
                            </select>
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='rae-requester'>{'Requested by (optional username)'}</label>
                            <input
                                id='rae-requester'
                                className='form-control'
                                placeholder='@username'
                                value={requester}
                                onChange={(e) => setRequester(e.target.value)}
                                disabled={submitting}
                            />
                        </div>
                        <div style={{...fieldStyle, display: 'flex', gap: 12}}>
                            <div style={{flex: 1}}>
                                <label htmlFor='rae-since'>{'Active from (optional)'}</label>
                                <input
                                    id='rae-since'
                                    type='date'
                                    className='form-control'
                                    value={since}
                                    onChange={(e) => setSince(e.target.value)}
                                    disabled={submitting}
                                />
                            </div>
                            <div style={{flex: 1}}>
                                <label htmlFor='rae-until'>{'Active until (optional)'}</label>
                                <input
                                    id='rae-until'
                                    type='date'
                                    className='form-control'
                                    value={until}
                                    onChange={(e) => setUntil(e.target.value)}
                                    disabled={submitting}
                                />
                            </div>
                        </div>
                        <div style={fieldStyle}>
                            <label htmlFor='rae-reason'>{'Reason'}</label>
                            <textarea
                                id='rae-reason'
                                className='form-control'
                                rows={3}
                                required={true}
                                placeholder='Why you need this export'
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
                                disabled={submitting || reason.trim() === ''}
                            >{submitting ? 'Submitting…' : 'Submit request'}</button>
                        </div>
                    </div>
                )}
            </div>
        </div>
    );
};
