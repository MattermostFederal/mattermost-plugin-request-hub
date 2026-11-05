import React, {useEffect, useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {auditExportURL, fetchAuditAccess, fetchAuditHistory, fetchAuditRequests} from './client';
import type {AuditExportFormat, AuditEvent, AuditHistory, AuditRequestSummary} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {closeAuditHistoryModal, isAuditHistoryModalOpen, openAuditExportRequestModal} from './store';

const PAGE_SIZE = 25;

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
    width: 880,
    maxWidth: '94vw',
    maxHeight: '90vh',
    overflowY: 'auto',
    padding: 24,
    boxShadow: '0 8px 24px rgba(0, 0, 0, 0.24)',
};
const mutedStyle: React.CSSProperties = {opacity: 0.72, fontSize: 13};
const cellStyle: React.CSSProperties = {padding: '8px 10px', borderBottom: '1px solid var(--center-channel-color-16, rgba(61,60,64,0.16))', verticalAlign: 'top', textAlign: 'left'};
const linkButtonStyle: React.CSSProperties = {background: 'none', border: 'none', padding: 0, color: 'var(--link-color, #1c58d9)', cursor: 'pointer', textAlign: 'left', font: 'inherit'};

// KIND_OPTIONS are the request types, as the server names them.
export const KIND_OPTIONS: Array<[string, string]> = [
    ['channel', 'Channel'],
    ['admin', 'Channel Admin'],
    ['team', 'Team creation'],
    ['team_admin', 'Team Admin'],
    ['team_member', 'Team member'],
    ['team_guest', 'Guest invite'],
    ['bot', 'Bot account'],
    ['webhook', 'Webhook'],
    ['audit_export', 'Audit export'],
];

const STATUS_OPTIONS = ['pending', 'approved', 'denied', 'expired', 'failed', 'lost', 'closed'];

const EVENT_LABELS: Record<string, string> = {
    submitted: 'Submitted',
    submit_failed: 'Submission failed',
    submit_refused: 'Submission refused',
    step_approved: 'Step approved',
    approved: 'Approved',
    approved_partial: 'Approved with problems',
    denied: 'Denied',
    expired: 'Expired',
    approve_failed: 'Approval failed',
    lost: 'Request lost',
    undelivered: 'Not delivered',
    closed_after_teardown: 'Closed after teardown',
    decision_refused: 'Approve or Deny refused',
    message_relayed: 'Message',
    message_dropped: 'Message not relayed',
    token_reminder: 'Token reminder',
    token_tracking_ended: 'Token reminders ended',
    token_post_deleted: 'Token message deleted',
    exercise_seeded: 'Exercise seeded',
    exercise_removed: 'Exercise removed',
    migrated: 'Upgraded',
};

const formatTime = (millis: number) => (millis ? new Date(millis).toLocaleString() : '');

const actorOf = (ev: AuditEvent) => {
    if (!ev.actor_id) {
        return 'Request Hub';
    }
    return ev.actor_name ? `@${ev.actor_name}` : ev.actor_id;
};

type DownloadLinksProps = {
    label: string;
    filter?: {type?: string; request?: string; kind?: string};
    formats?: AuditExportFormat[];

    // noun goes in each link's name, as in "Download inventory PDF".
    noun?: string;
};

const DownloadLinks = ({label, filter, formats = ['csv', 'json'], noun}: DownloadLinksProps) => (
    <span style={{display: 'inline-flex', gap: 8, alignItems: 'center', flexWrap: 'wrap'}}>
        <span style={mutedStyle}>{label}</span>
        {formats.map((format) => (
            <a
                key={format}
                className='btn btn-tertiary btn-sm'
                href={auditExportURL(format, filter)}
                download={true}
            >{`Download ${noun ? `${noun} ` : ''}${format.toUpperCase()}`}</a>
        ))}
    </span>
);

const EventItem = ({ev}: {ev: AuditEvent}) => {
    const {message, ...rest} = ev.detail ?? {};
    const failed = ev.outcome === 'fail';
    return (
        <li style={{padding: '10px 0', borderBottom: '1px solid var(--center-channel-color-08, rgba(61,60,64,0.08))'}}>
            <div style={{display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'baseline'}}>
                <strong>{EVENT_LABELS[ev.event] ?? ev.event}</strong>
                {failed ? (
                    <span
                        style={{fontSize: 12, color: 'var(--error-text, #d24b4e)'}}
                    >{'failed'}</span>
                ) : null}
                <span style={mutedStyle}>{`${formatTime(ev.at)} · ${actorOf(ev)}`}</span>
            </div>
            {ev.note ? <div style={{marginTop: 4, whiteSpace: 'pre-wrap'}}>{ev.note}</div> : null}
            {typeof message === 'string' && message !== '' ? (
                <blockquote style={{margin: '6px 0 0', padding: '4px 10px', borderLeft: '3px solid var(--center-channel-color-24, rgba(61,60,64,0.24))', whiteSpace: 'pre-wrap'}}>
                    {message}
                </blockquote>
            ) : null}
            {Object.keys(rest).length > 0 ? (
                <details style={{marginTop: 4}}>
                    <summary style={{cursor: 'pointer', ...mutedStyle}}>{'Details'}</summary>
                    <pre style={{fontSize: 12, whiteSpace: 'pre-wrap', margin: '4px 0 0'}}>{JSON.stringify(rest, null, 2)}</pre>
                </details>
            ) : null}
        </li>
    );
};

type Detail = {summary: AuditRequestSummary; requester_username: string; history: AuditHistory};

// AuditHistoryModal is the System Admins' window on the audit history:
// every request, searchable and filterable, each with its full timeline,
// and downloads of the whole history or any part of it.
export const AuditHistoryModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isAuditHistoryModalOpen);

    const [search, setSearch] = useState('');
    const [query, setQuery] = useState('');
    const [kind, setKind] = useState('');
    const [status, setStatus] = useState('');
    const [rows, setRows] = useState<AuditRequestSummary[]>([]);
    const [total, setTotal] = useState(0);
    const [page, setPage] = useState(0);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState('');
    const [detail, setDetail] = useState<Detail | null>(null);

    // Download links show only while direct export is on; otherwise the
    // window offers a request for an export, for another System Admin to
    // approve. null is still loading, when neither shows.
    const [directExport, setDirectExport] = useState<boolean | null>(null);
    useEffect(() => {
        if (!isOpen) {
            return undefined;
        }
        let cancelled = false;
        fetchAuditAccess().then((access) => {
            if (!cancelled) {
                setDirectExport(access.direct_export);
            }
        });
        return () => {
            cancelled = true;
        };
    }, [isOpen]);

    const close = () => {
        setDetail(null);
        setSearch('');
        setQuery('');
        setKind('');
        setStatus('');
        setError('');
        dispatch(closeAuditHistoryModal());
    };
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    // The search box updates the query once typing pauses.
    useEffect(() => {
        const timer = window.setTimeout(() => setQuery(search.trim()), 300);
        return () => window.clearTimeout(timer);
    }, [search]);

    useEffect(() => {
        if (!isOpen) {
            return undefined;
        }
        let cancelled = false;
        setLoading(true);
        setError('');
        fetchAuditRequests({q: query, kind, status, page, perPage: PAGE_SIZE}).then((res) => {
            if (cancelled) {
                return;
            }
            setTotal(res.total);
            setRows((prev) => (page === 0 ? res.requests : [...prev, ...res.requests]));
        }).catch((err: Error) => {
            if (!cancelled) {
                setError(`Could not load the audit history: ${err.message}`);
            }
        }).finally(() => {
            if (!cancelled) {
                setLoading(false);
            }
        });
        return () => {
            cancelled = true;
        };
    }, [isOpen, query, kind, status, page]);

    // A new filter starts again from the first page.
    useEffect(() => {
        setPage(0);
    }, [query, kind, status]);

    if (!isOpen) {
        return null;
    }

    const openDetail = async (requestId: string) => {
        setError('');
        try {
            setDetail(await fetchAuditHistory(requestId));
        } catch (err) {
            setError(`Could not load that request's history: ${(err as Error).message}`);
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
                aria-labelledby='audit-history-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <div style={{display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 12}}>
                    <div>
                        <h3
                            id='audit-history-title'
                            style={{marginTop: 0, marginBottom: 4}}
                        >{'Audit history'}</h3>
                        <p style={{margin: 0, ...mutedStyle}}>
                            {'Everything that happened to every request: who did it and when. Visible to System Admins only.'}
                        </p>
                    </div>
                    <button
                        style={{background: 'none', border: 'none', cursor: 'pointer', fontSize: 20, lineHeight: 1, opacity: 0.6, padding: 0}}
                        onClick={close}
                        aria-label='Close'
                    >{'×'}</button>
                </div>

                {error ? (
                    <div
                        className='alert alert-danger'
                        role='alert'
                        style={{marginTop: 16}}
                    >{error}</div>
                ) : null}

                {detail ? (
                    <div style={{marginTop: 16}}>
                        <button
                            style={linkButtonStyle}
                            onClick={() => setDetail(null)}
                        >{'← All requests'}</button>
                        <h4 style={{margin: '12px 0 4px'}}>
                            {`${detail.summary.kind_label} request${detail.summary.title ? `: ${detail.summary.title}` : ''}`}
                        </h4>
                        <p style={{margin: 0, ...mutedStyle}}>
                            {`Status: ${detail.summary.status} · Requested by ${detail.requester_username ? `@${detail.requester_username}` : detail.summary.requester_id || 'unknown'} · ${formatTime(detail.summary.created_at)} · ID ${detail.summary.request_id}`}
                            {detail.summary.exercise ? ' · exercise' : ''}
                        </p>
                        {directExport ? (
                            <div style={{marginTop: 12}}>
                                <DownloadLinks
                                    label='This request:'
                                    filter={{request: detail.summary.request_id}}
                                />
                            </div>
                        ) : null}
                        <ol
                            aria-label='Events'
                            style={{listStyle: 'none', padding: 0, margin: '12px 0 0'}}
                        >
                            {detail.history.events.map((ev, i) => (
                                <EventItem
                                    key={`${ev.at}-${i}`}
                                    ev={ev}
                                />
                            ))}
                        </ol>
                    </div>
                ) : (
                    <div style={{marginTop: 16}}>
                        <div style={{display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'flex-end'}}>
                            <div style={{flex: '1 1 220px'}}>
                                <label htmlFor='audit-search'>{'Search'}</label>
                                <input
                                    id='audit-search'
                                    className='form-control'
                                    placeholder='Request ID, name or requester'
                                    value={search}
                                    onChange={(e) => setSearch(e.target.value)}
                                />
                            </div>
                            <div>
                                <label htmlFor='audit-kind'>{'Type'}</label>
                                <select
                                    id='audit-kind'
                                    className='form-control'
                                    value={kind}
                                    onChange={(e) => setKind(e.target.value)}
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
                            <div>
                                <label htmlFor='audit-status'>{'Status'}</label>
                                <select
                                    id='audit-status'
                                    className='form-control'
                                    value={status}
                                    onChange={(e) => setStatus(e.target.value)}
                                >
                                    <option value=''>{'Any status'}</option>
                                    {STATUS_OPTIONS.map((s) => (
                                        <option
                                            key={s}
                                            value={s}
                                        >{s}</option>
                                    ))}
                                </select>
                            </div>
                        </div>

                        <div style={{marginTop: 12}}>
                            {directExport === true ? (
                                <div style={{display: 'flex', flexDirection: 'column', gap: 8}}>
                                    <DownloadLinks
                                        label={kind ? 'Every request of this type, every event:' : 'Whole audit history, every event:'}
                                        filter={kind ? {kind} : {}}
                                    />
                                    <DownloadLinks
                                        label='Every bot Request Hub created:'
                                        filter={{type: 'bot_inventory'}}
                                        formats={['pdf', 'csv', 'json']}
                                        noun='inventory'
                                    />
                                </div>
                            ) : null}
                            {directExport === false ? (
                                <span style={{display: 'inline-flex', gap: 8, alignItems: 'center', flexWrap: 'wrap'}}>
                                    <span style={mutedStyle}>{'Direct downloads are off. Another System Admin approves each export.'}</span>
                                    <button
                                        className='btn btn-tertiary btn-sm'
                                        onClick={() => {
                                            close();
                                            dispatch(openAuditExportRequestModal());
                                        }}
                                    >{'Request an export'}</button>
                                </span>
                            ) : null}
                        </div>

                        <table style={{width: '100%', borderCollapse: 'collapse', marginTop: 12, fontSize: 14}}>
                            <caption style={{captionSide: 'top', textAlign: 'left', ...mutedStyle}}>
                                {loading && rows.length === 0 ? 'Loading…' : `${total} ${total === 1 ? 'request' : 'requests'}`}
                            </caption>
                            <thead>
                                <tr>
                                    <th style={cellStyle}>{'Request'}</th>
                                    <th style={cellStyle}>{'Type'}</th>
                                    <th style={cellStyle}>{'Requester'}</th>
                                    <th style={cellStyle}>{'Status'}</th>
                                    <th style={cellStyle}>{'Last activity'}</th>
                                </tr>
                            </thead>
                            <tbody>
                                {rows.map((row) => (
                                    <tr key={row.request_id}>
                                        <td style={cellStyle}>
                                            <button
                                                style={linkButtonStyle}
                                                onClick={() => openDetail(row.request_id)}
                                            >{row.title || row.request_id}</button>
                                            <div style={{...mutedStyle, fontSize: 12}}>{row.request_id}</div>
                                        </td>
                                        <td style={cellStyle}>{row.kind_label}</td>
                                        <td style={cellStyle}>{row.requester_username ? `@${row.requester_username}` : row.requester_id}</td>
                                        <td style={cellStyle}>{row.status}</td>
                                        <td style={cellStyle}>
                                            {formatTime(row.updated_at)}
                                            <div style={{...mutedStyle, fontSize: 12}}>{EVENT_LABELS[row.last_event] ?? row.last_event}</div>
                                        </td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                        {!loading && rows.length === 0 && !error ? (
                            <p style={{marginTop: 12, ...mutedStyle}}>{'No requests match.'}</p>
                        ) : null}
                        {rows.length < total ? (
                            <div style={{textAlign: 'center', marginTop: 12}}>
                                <button
                                    className='btn btn-tertiary'
                                    disabled={loading}
                                    onClick={() => setPage((p) => p + 1)}
                                >{loading ? 'Loading…' : 'Load more'}</button>
                            </div>
                        ) : null}
                    </div>
                )}
            </div>
        </div>
    );
};
