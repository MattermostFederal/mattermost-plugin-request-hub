import React, {useEffect, useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {KIND_OPTIONS} from './AuditHistoryModal';
import {fetchBotForm, submitAuditExportRequest} from './client';
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

    // narrow is what else narrows the export: the request type (the
    // histories), the hosting environment (the bot inventory) or one bot
    // (the forensics, which has no requester filter).
    narrow: 'request' | 'env' | 'bot';

    // dates labels the date range.
    dates: string;

    // datesRequired: the export needs both dates.
    datesRequired?: boolean;
};

// EXPORT_TYPES are the exports the server builds and the formats each comes
// in, the default first. They match auditExportTypes on the server.
export const EXPORT_TYPES: ExportType[] = [
    {
        value: 'full_histories',
        label: 'Request histories',
        formats: [['csv', 'CSV (one row per event)'], ['json', 'JSON (one object per request)']],
        narrow: 'request',
        dates: 'Active',
    },
    {
        value: 'bot_inventory',
        label: 'Bot inventory',
        formats: [['pdf', 'PDF (for reading)'], ['csv', 'CSV (one row per bot)'], ['json', 'JSON (one object per bot)']],
        narrow: 'env',
        dates: 'Approved',
    },
    {
        value: 'bot_forensic',
        label: 'Bot forensics',
        formats: [['json', 'JSON']],
        narrow: 'bot',
        dates: 'Posted',
        datesRequired: true,
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
    const [env, setEnv] = useState('');
    const [bot, setBot] = useState('');
    const [envs, setEnvs] = useState<Array<{value: string; label: string}>>([]);
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
        setEnv('');
        setBot('');
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

    // The bot inventory is narrowed by the fixed hosting codes the server
    // lists. Without them the field is a free text box.
    useEffect(() => {
        if (!isOpen) {
            return undefined;
        }
        let cancelled = false;
        fetchBotForm().then((form) => {
            if (!cancelled) {
                setEnvs(form?.hosting_envs ?? []);
            }
        });
        return () => {
            cancelled = true;
        };
    }, [isOpen]);

    if (!isOpen) {
        return null;
    }

    const currentType = EXPORT_TYPES.find((t) => t.value === exportType) ?? EXPORT_TYPES[0];
    const byBot = currentType.narrow === 'bot';
    const ready = reason.trim() !== '' && (!byBot || (bot.trim().replace(/^@/, '') !== '' && since !== '' && until !== ''));
    const dateLabel = (edge: string) => `${currentType.dates} ${edge}${currentType.datesRequired ? '' : ' (optional)'}`;

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
                kind: currentType.narrow === 'request' ? kind || undefined : undefined,
                env: currentType.narrow === 'env' ? env.trim() || undefined : undefined,
                bot: byBot ? bot.trim().replace(/^@/, '') : undefined,
                requester: byBot ? undefined : requester.trim().replace(/^@/, '') || undefined,
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
                            {'Ask for a copy of the request audit history, the inventory of bots Request Hub created, or the posts and memberships of one bot. A System Admin reviews the request; once approved, the file is posted in your request thread with the Request Hub bot. Leave the filters empty for everything.'}
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
                        {byBot ? (
                            <div style={fieldStyle}>
                                <label htmlFor='rae-bot'>{'Bot'}</label>
                                <input
                                    id='rae-bot'
                                    className='form-control'
                                    placeholder='@bot-username'
                                    required={true}
                                    value={bot}
                                    onChange={(e) => setBot(e.target.value)}
                                    disabled={submitting}
                                />
                                <div style={{fontSize: 12, opacity: 0.7, marginTop: 4}}>
                                    {'Covers at most 90 days, up to 5,000 posts. Channels the bot has left, direct messages and deleted posts are not included.'}
                                </div>
                            </div>
                        ) : null}
                        {currentType.narrow === 'env' ? (
                            <div style={fieldStyle}>
                                <label htmlFor='rae-env'>{'Hosting environment (optional)'}</label>
                                {envs.length > 0 ? (
                                    <select
                                        id='rae-env'
                                        className='form-control'
                                        value={env}
                                        onChange={(e) => setEnv(e.target.value)}
                                        disabled={submitting}
                                    >
                                        <option value=''>{'All environments'}</option>
                                        {envs.map((opt) => (
                                            <option
                                                key={opt.value}
                                                value={opt.value}
                                            >{opt.label}</option>
                                        ))}
                                    </select>
                                ) : (
                                    <input
                                        id='rae-env'
                                        className='form-control'
                                        placeholder='All environments'
                                        value={env}
                                        onChange={(e) => setEnv(e.target.value)}
                                        disabled={submitting}
                                    />
                                )}
                            </div>
                        ) : null}
                        {currentType.narrow === 'request' ? (<div style={fieldStyle}>
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
                        </div>) : null}
                        {byBot ? null : (
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
                        )}
                        <div style={{...fieldStyle, display: 'flex', gap: 12}}>
                            <div style={{flex: 1}}>
                                <label htmlFor='rae-since'>{dateLabel('from')}</label>
                                <input
                                    id='rae-since'
                                    type='date'
                                    className='form-control'
                                    required={currentType.datesRequired}
                                    value={since}
                                    onChange={(e) => setSince(e.target.value)}
                                    disabled={submitting}
                                />
                            </div>
                            <div style={{flex: 1}}>
                                <label htmlFor='rae-until'>{dateLabel('until')}</label>
                                <input
                                    id='rae-until'
                                    type='date'
                                    className='form-control'
                                    required={currentType.datesRequired}
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
                                disabled={submitting || !ready}
                            >{submitting ? 'Submitting…' : 'Submit request'}</button>
                        </div>
                    </div>
                )}
            </div>
        </div>
    );
};
