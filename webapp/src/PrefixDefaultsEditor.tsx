import React, {useMemo, useState} from 'react';

import {readSiblingSetting} from './ApprovalChannelPicker';
import {parseRows} from './PrefixEditor';

// PrefixDefaultsEditor: System Console custom setting for
// ChannelPrefixDefaults. One section per prefix from the sibling
// ChannelNamePrefixes setting (read from the unsaved console state, so a
// prefix added a moment ago shows up here at once). Each prefix can force
// the channel's visibility, give a purpose used when the requester leaves
// it blank, set a channel header, and name default members.
//
// Stored as JSON keyed by prefix, which the server parses in
// parsePrefixDefaults:
//   {"sec-": {"channel_type": "P", "purpose": "...", "header": "...", "members": ["alice"]}}

type Props = {
    id: string;
    label?: string;
    value: string;
    disabled?: boolean;
    onChange: (id: string, value: string) => void;
    setSaveNeeded?: () => void;
    config?: Record<string, unknown>;
    currentState?: Record<string, unknown>;
};

export type PrefixDefaults = {
    channel_type?: string;
    purpose?: string;
    header?: string;
    members?: string[];
};

export type PrefixDefaultsMap = Record<string, PrefixDefaults>;

// Mirrors the server limits in parsePrefixDefaults.
const MAX_PURPOSE = 250;
const MAX_HEADER = 1024;

// parseDefaults reads the saved JSON. Anything unreadable starts empty: the
// server ignores invalid JSON too, so saving from here repairs it.
export function parseDefaults(raw: string): PrefixDefaultsMap {
    if (!raw || !raw.trim()) {
        return {};
    }
    try {
        const parsed = JSON.parse(raw);
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
            return {};
        }
        const out: PrefixDefaultsMap = {};
        for (const [prefix, entry] of Object.entries(parsed as Record<string, unknown>)) {
            if (!entry || typeof entry !== 'object') {
                continue;
            }
            const e = entry as Record<string, unknown>;
            out[prefix] = {
                channel_type: typeof e.channel_type === 'string' ? e.channel_type : '',
                purpose: typeof e.purpose === 'string' ? e.purpose : '',
                header: typeof e.header === 'string' ? e.header : '',
                members: Array.isArray(e.members) ? e.members.filter((m): m is string => typeof m === 'string') : [],
            };
        }
        return out;
    } catch {
        return {};
    }
}

// parseMembers turns the members text box into usernames.
export function parseMembers(text: string): string[] {
    const seen = new Set<string>();
    const out: string[] = [];
    for (const piece of text.split(/[\s,]+/)) {
        const name = piece.trim().replace(/^@/, '').toLowerCase();
        if (name && !seen.has(name)) {
            seen.add(name);
            out.push(name);
        }
    }
    return out;
}

// serializeDefaults writes only entries that set something, with empty
// fields left out, in a stable key order.
export function serializeDefaults(map: PrefixDefaultsMap): string {
    const out: PrefixDefaultsMap = {};
    for (const prefix of Object.keys(map).sort()) {
        const d = map[prefix];
        const entry: PrefixDefaults = {};
        if (d.channel_type) {
            entry.channel_type = d.channel_type;
        }
        if (d.purpose?.trim()) {
            entry.purpose = d.purpose.trim();
        }
        if (d.header?.trim()) {
            entry.header = d.header.trim();
        }
        if (d.members && d.members.length > 0) {
            entry.members = d.members;
        }
        if (Object.keys(entry).length > 0) {
            out[prefix] = entry;
        }
    }
    return Object.keys(out).length === 0 ? '' : JSON.stringify(out);
}

function summary(d: PrefixDefaults | undefined): string {
    if (!d) {
        return 'No defaults';
    }
    const parts: string[] = [];
    if (d.channel_type === 'P') {
        parts.push('always private');
    } else if (d.channel_type === 'O') {
        parts.push('always public');
    }
    if (d.purpose?.trim()) {
        parts.push('purpose');
    }
    if (d.header?.trim()) {
        parts.push('header');
    }
    if (d.members && d.members.length > 0) {
        parts.push(`${d.members.length} default member${d.members.length === 1 ? '' : 's'}`);
    }
    return parts.length > 0 ? parts.join(', ') : 'No defaults';
}

export const PrefixDefaultsEditor: React.FC<Props> = (props) => {
    const {id, value, disabled, onChange, setSaveNeeded} = props;
    const [map, setMap] = useState<PrefixDefaultsMap>(() => parseDefaults(value));
    const [open, setOpen] = useState<string>('');

    // Members are edited as free text and parsed on blur, so typing a
    // comma or space doesn't fight the input.
    const [memberDrafts, setMemberDrafts] = useState<Record<string, string>>({});

    const prefixes = useMemo(
        () => parseRows(readSiblingSetting(props, 'ChannelNamePrefixes')).map((r) => r.prefix),
        [props.config, props.currentState],
    );
    const unused = Object.keys(map).filter((p) => !prefixes.includes(p)).sort();

    const commit = (next: PrefixDefaultsMap) => {
        setMap(next);
        onChange(id, serializeDefaults(next));
        setSaveNeeded?.();
    };

    const update = (prefix: string, patch: Partial<PrefixDefaults>) => {
        commit({...map, [prefix]: {...map[prefix], ...patch}});
    };

    const remove = (prefix: string) => {
        const next = {...map};
        delete next[prefix];
        commit(next);
    };

    if (prefixes.length === 0 && unused.length === 0) {
        return (
            <div className='help-text'>
                {'Add a channel prefix above first. Each prefix can then have its own channel defaults.'}
            </div>
        );
    }

    return (
        <div>
            {prefixes.map((prefix) => {
                const d = map[prefix] ?? {};
                const isOpen = open === prefix;
                const membersText = memberDrafts[prefix] ?? (d.members ?? []).join(', ');
                return (
                    <div
                        key={prefix}
                        data-testid={`prefix-defaults-${prefix}`}
                        style={{border: '1px solid rgba(63, 67, 80, 0.16)', borderRadius: 4, marginBottom: 8}}
                    >
                        <button
                            type='button'
                            className='btn btn-link'
                            style={{width: '100%', textAlign: 'left', display: 'flex', justifyContent: 'space-between'}}
                            aria-expanded={isOpen}
                            onClick={() => setOpen(isOpen ? '' : prefix)}
                        >
                            <span><code>{prefix}</code></span>
                            <span style={{opacity: 0.72}}>{summary(map[prefix])}</span>
                        </button>
                        {isOpen ? (
                            <div style={{padding: '0 12px 12px'}}>
                                <label style={{display: 'block', marginTop: 8}}>
                                    {'Visibility'}
                                    <select
                                        className='form-control'
                                        value={d.channel_type ?? ''}
                                        disabled={disabled}
                                        onChange={(e) => update(prefix, {channel_type: e.target.value})}
                                    >
                                        <option value=''>{'Requester chooses'}</option>
                                        <option value='O'>{'Always public'}</option>
                                        <option value='P'>{'Always private'}</option>
                                    </select>
                                </label>
                                <label style={{display: 'block', marginTop: 8}}>
                                    {'Purpose (used when the requester leaves it blank)'}
                                    <textarea
                                        className='form-control'
                                        rows={2}
                                        maxLength={MAX_PURPOSE}
                                        value={d.purpose ?? ''}
                                        disabled={disabled}
                                        onChange={(e) => update(prefix, {purpose: e.target.value})}
                                    />
                                </label>
                                <label style={{display: 'block', marginTop: 8}}>
                                    {'Channel header'}
                                    <textarea
                                        className='form-control'
                                        rows={2}
                                        maxLength={MAX_HEADER}
                                        value={d.header ?? ''}
                                        disabled={disabled}
                                        onChange={(e) => update(prefix, {header: e.target.value})}
                                    />
                                </label>
                                <label style={{display: 'block', marginTop: 8}}>
                                    {'Default members (usernames, separated by commas)'}
                                    <input
                                        className='form-control'
                                        value={membersText}
                                        placeholder='alice, bob'
                                        disabled={disabled}
                                        onChange={(e) => setMemberDrafts({...memberDrafts, [prefix]: e.target.value})}
                                        onBlur={() => {
                                            const draft = memberDrafts[prefix];
                                            if (draft === undefined) {
                                                return;
                                            }
                                            const next = {...memberDrafts};
                                            delete next[prefix];
                                            setMemberDrafts(next);
                                            update(prefix, {members: parseMembers(draft)});
                                        }}
                                    />
                                </label>
                                <div className='help-text'>
                                    {'Default members are added when a request is approved, if they are on the channel\'s team. Others are named in the welcome message.'}
                                </div>
                            </div>
                        ) : null}
                    </div>
                );
            })}
            {unused.map((prefix) => (
                <div
                    key={prefix}
                    data-testid={`prefix-defaults-unused-${prefix}`}
                    style={{display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '6px 12px', opacity: 0.72}}
                >
                    <span><code>{prefix}</code>{' Unused: this prefix is no longer in the list.'}</span>
                    <button
                        type='button'
                        className='btn btn-tertiary btn-sm'
                        disabled={disabled}
                        onClick={() => remove(prefix)}
                    >
                        {'Remove'}
                    </button>
                </div>
            ))}
        </div>
    );
};
