import React, {useEffect, useMemo, useState} from 'react';

// PrefixEditor: MM System Console custom setting for the channel
// domain-prefix list. Table row per prefix. No regex — the admin only
// picks a MAX LENGTH (2..48 chars). Character class is fixed at
// letters + digits + dashes because MM channel URLs are restricted to
// that anyway (and lowercased at request time).
//
// Serializes to the same pipe-delimited storage format the server
// parses ("prefix|description|max_length" one per line). Server-side
// parsePrefixList treats a numeric third field as a max-length and
// constructs the regex [a-z0-9-]{2,N} internally. Legacy regex-string
// third fields still work for anyone who hand-edited the raw setting.
//
// Registered via registry.registerAdminConsoleCustomSetting.

type Props = {
    id: string;
    label?: string;
    value: string;
    disabled?: boolean;
    onChange: (id: string, value: string) => void;
    setSaveNeeded?: () => void;
};

type PrefixRow = {
    prefix: string;
    description: string;
    maxLength: number;
};

const DEFAULT_MAX_LENGTH = 48;
const MIN_MAX_LENGTH = 2;
const MAX_MAX_LENGTH = 48;

// Preset packs — populate the table with sensible starter values.
// Clicking one UPSERTS its rows: prefixes already in the table are
// updated to the preset's description + max length, and any missing
// prefixes are appended. Re-picking is idempotent.
const PRESET_PACKS: Array<{name: string; description: string; rows: PrefixRow[]}> = [
    {
        name: 'SRE / Ops',
        description: 'Operational + incident-response channels',
        rows: [
            {prefix: 'ops-', description: 'Operational channels', maxLength: 48},
            {prefix: 'incident-', description: 'Incident response', maxLength: 48},
            {prefix: 'post-', description: 'Post-mortem channels', maxLength: 48},
            {prefix: 'runbook-', description: 'Runbook discussion', maxLength: 48},
        ],
    },
    {
        name: 'Product org',
        description: 'Team, project, launch, design channels',
        rows: [
            {prefix: 'team-', description: 'Team collaboration channels', maxLength: 48},
            {prefix: 'project-', description: 'Project workstreams', maxLength: 48},
            {prefix: 'launch-', description: 'Launch coordination', maxLength: 48},
            {prefix: 'design-', description: 'Design reviews', maxLength: 48},
        ],
    },
    {
        name: 'Support / Success',
        description: 'Customer help + onboarding + VIP channels',
        rows: [
            {prefix: 'help-', description: 'Customer help channels', maxLength: 48},
            {prefix: 'onboard-', description: 'Customer onboarding', maxLength: 48},
            {prefix: 'vip-', description: 'Enterprise account channels', maxLength: 48},
        ],
    },
    {
        name: 'General org',
        description: 'Broad-purpose starter (team + channel + project + announcements + general)',
        rows: [
            {prefix: 'team-', description: 'Team collaboration channels', maxLength: 48},
            {prefix: 'channel-', description: 'General purpose channels', maxLength: 48},
            {prefix: 'project-', description: 'Project workstreams', maxLength: 48},
            {prefix: 'announcements-', description: 'Read-only announcements', maxLength: 48},
            {prefix: 'general-', description: 'General-purpose channels', maxLength: 48},
        ],
    },
];

// Parse the stored value into rows. Third field is:
//   - a numeric string like "16"       -> use that as maxLength
//   - a legacy regex string (any non-numeric) -> best-effort extract
//     the {2,N} bound if present; else fall back to DEFAULT_MAX_LENGTH
//   - missing/empty                     -> DEFAULT_MAX_LENGTH
export function parseRows(raw: string): PrefixRow[] {
    if (!raw) {
        return [];
    }
    const rows: PrefixRow[] = [];
    for (const line of raw.split('\n')) {
        const trimmed = line.trim();
        if (!trimmed || trimmed.startsWith('#')) {
            continue;
        }
        const parts = trimmed.split('|');
        const prefix = (parts[0] ?? '').trim();
        if (!prefix) {
            continue;
        }
        const description = (parts[1] ?? '').trim();
        const raw3 = (parts[2] ?? '').trim();

        let maxLength = DEFAULT_MAX_LENGTH;
        if (raw3 !== '') {
            if ((/^\d+$/).test(raw3)) {
                maxLength = clampLength(parseInt(raw3, 10));
            } else {
                // Legacy regex — try to extract the upper bound from
                // "{2,N}" so a converted setting keeps the same limit.
                const m = raw3.match(/\{\s*\d+\s*,\s*(\d+)\s*\}/);
                if (m) {
                    maxLength = clampLength(parseInt(m[1], 10));
                }
            }
        }
        rows.push({prefix, description, maxLength});
    }
    return rows;
}

// Serialize back to pipe-delim. Third field is always the numeric max
// length so future round-trips are lossless.
function serializeRows(rows: PrefixRow[]): string {
    return rows.
        filter((r) => r.prefix.trim() !== '').
        map((r) => [r.prefix.trim(), r.description.trim(), String(clampLength(r.maxLength))].join('|')).
        join('\n');
}

// prefixNeedsSeparator reports whether a prefix would run together with the
// suffix. MM channel URLs allow only [a-z0-9-], so "-" is the only possible
// separator — a non-empty prefix that doesn't end in one yields URLs like
// "teammarketing". Non-blocking: the admin can keep it, but we flag it.
function prefixNeedsSeparator(prefix: string): boolean {
    const p = prefix.trim();
    return p !== '' && !p.endsWith('-');
}

function clampLength(n: number): number {
    if (Number.isNaN(n) || n < MIN_MAX_LENGTH) {
        return MIN_MAX_LENGTH;
    }
    if (n > MAX_MAX_LENGTH) {
        return MAX_MAX_LENGTH;
    }
    return Math.floor(n);
}

// Slugify: same rules as server. Used for the live preview so what the
// admin sees matches what a real request would produce.
function slugify(input: string): string {
    return input.
        toLowerCase().
        trim().
        replace(/[^a-z0-9]+/g, '-').
        replace(/^-+|-+$/g, '');
}

export const PrefixEditor: React.FC<Props> = ({id, value, disabled, onChange, setSaveNeeded}) => {
    const [rows, setRows] = useState<PrefixRow[]>(() => parseRows(value ?? ''));
    const [presetChoice, setPresetChoice] = useState('');
    const [previewInput, setPreviewInput] = useState('marketing team');
    const [selectedPreviewPrefix, setSelectedPreviewPrefix] = useState('');

    // Re-sync when MM updates the value externally (e.g. reset button).
    useEffect(() => {
        setRows(parseRows(value ?? ''));
    }, [value]);

    // Ensure the preview always has a valid selected prefix.
    useEffect(() => {
        if (rows.length === 0) {
            setSelectedPreviewPrefix('');
            return;
        }
        if (!rows.find((r) => r.prefix === selectedPreviewPrefix)) {
            setSelectedPreviewPrefix(rows[0].prefix);
        }
    }, [rows, selectedPreviewPrefix]);

    const commit = (next: PrefixRow[]) => {
        setRows(next);
        onChange(id, serializeRows(next));
        setSaveNeeded?.();
    };

    const updateRow = (idx: number, patch: Partial<PrefixRow>) => {
        const next = rows.slice();
        next[idx] = {...next[idx], ...patch};
        commit(next);
    };

    const addRow = () => {
        commit([...rows, {prefix: '', description: '', maxLength: DEFAULT_MAX_LENGTH}]);
    };

    const removeRow = (idx: number) => {
        commit(rows.filter((_, i) => i !== idx));
    };

    const moveRow = (idx: number, direction: -1 | 1) => {
        const target = idx + direction;
        if (target < 0 || target >= rows.length) {
            return;
        }
        const next = rows.slice();
        [next[idx], next[target]] = [next[target], next[idx]];
        commit(next);
    };

    const applyPreset = (name: string) => {
        setPresetChoice(name);
        if (!name) {
            return;
        }
        const pack = PRESET_PACKS.find((p) => p.name === name);
        if (!pack) {
            return;
        }

        // Upsert: bring any prefix the table already has in line with the
        // preset (description + max length), then append the ones it's
        // missing. Previously existing rows were skipped, so a stale length
        // (e.g. an old 16) would survive a re-pick — this makes loading a
        // preset actually apply that preset's values to matching prefixes.
        const byPrefix = new Map(pack.rows.map((r) => [r.prefix.toLowerCase(), r]));
        const updated = rows.map((r) => {
            const preset = byPrefix.get(r.prefix.toLowerCase());
            return preset ? {...r, description: preset.description, maxLength: preset.maxLength} : r;
        });
        const existing = new Set(rows.map((r) => r.prefix.toLowerCase()));
        const additions = pack.rows.filter((r) => !existing.has(r.prefix.toLowerCase()));
        commit([...updated, ...additions]);
        setPresetChoice('');
    };

    // Live preview: what channel URL does the current input produce?
    const previewResult = useMemo(() => {
        if (!selectedPreviewPrefix) {
            return '(add a prefix to see a preview)';
        }
        const suffix = slugify(previewInput);
        const selected = rows.find((r) => r.prefix === selectedPreviewPrefix);
        if (!suffix) {
            return selectedPreviewPrefix + '<suffix> — type something above to preview';
        }
        const combined = selectedPreviewPrefix + suffix;
        let note = '';
        if (selected) {
            if (suffix.length < MIN_MAX_LENGTH) {
                note = `  ✗ suffix must be at least ${MIN_MAX_LENGTH} chars`;
            } else if (suffix.length > selected.maxLength) {
                note = `  ✗ suffix is ${suffix.length} chars, exceeds max ${selected.maxLength}`;
            } else {
                note = `  ✓ ok (${suffix.length}/${selected.maxLength} chars)`;
            }
        }
        return combined + note;
    }, [selectedPreviewPrefix, previewInput, rows]);

    return (
        <div style={{padding: '8px 0'}}>
            <div style={{marginBottom: 12, fontSize: 14, opacity: 0.85}}>
                {'Define one row per allowed channel prefix. Requesters pick from these in a dropdown; the final channel URL is prefix + slugified suffix. Suffix characters are always lowercased letters, digits, and dashes (MM channel-URL rules). Max length is the cap on the suffix portion only, not the prefix.'}
            </div>

            <table style={{width: '100%', borderCollapse: 'collapse', marginBottom: 12}}>
                <thead>
                    <tr style={{textAlign: 'left', borderBottom: '1px solid rgba(0,0,0,0.1)'}}>
                        <th style={{padding: 6, width: '22%'}}>{'Prefix'}</th>
                        <th style={{padding: 6, width: '46%'}}>{'Description'}</th>
                        <th style={{padding: 6, width: '14%'}}>{'Max suffix length'}</th>
                        <th style={{padding: 6, width: '18%'}}>{''}</th>
                    </tr>
                </thead>
                <tbody>
                    {rows.length === 0 ? (
                        <tr>
                            <td
                                colSpan={4}
                                style={{padding: 12, opacity: 0.6, fontStyle: 'italic'}}
                            >
                                {'No prefixes yet. Use "Add prefix" below, or "Load preset" to start with a common pack.'}
                            </td>
                        </tr>
                    ) : (
                        rows.map((row, idx) => (
                            <tr key={idx}>
                                <td style={{padding: 6}}>
                                    <input
                                        className='form-control'
                                        value={row.prefix}
                                        placeholder='team-'
                                        disabled={disabled}
                                        onChange={(e) => updateRow(idx, {prefix: e.target.value})}
                                    />
                                    {prefixNeedsSeparator(row.prefix) ? (
                                        <div style={{marginTop: 4, fontSize: 12, color: 'rgba(210, 75, 78, 0.95)'}}>
                                            {`⚠ No separator — URLs run together (${row.prefix.trim()}suffix). `}
                                            <button
                                                type='button'
                                                className='btn btn-link btn-sm'
                                                style={{padding: 0, fontSize: 12, verticalAlign: 'baseline'}}
                                                disabled={disabled}
                                                onClick={() => updateRow(idx, {prefix: row.prefix.trim() + '-'})}
                                            >
                                                {'Add “-”'}
                                            </button>
                                        </div>
                                    ) : null}
                                </td>
                                <td style={{padding: 6}}>
                                    <input
                                        className='form-control'
                                        value={row.description}
                                        placeholder='Team collaboration channels'
                                        disabled={disabled}
                                        onChange={(e) => updateRow(idx, {description: e.target.value})}
                                    />
                                </td>
                                <td style={{padding: 6}}>
                                    <input
                                        className='form-control'
                                        type='number'
                                        min={MIN_MAX_LENGTH}
                                        max={MAX_MAX_LENGTH}
                                        step={1}
                                        value={row.maxLength}
                                        disabled={disabled}
                                        onChange={(e) => updateRow(idx, {maxLength: clampLength(parseInt(e.target.value, 10))})}
                                    />
                                </td>
                                <td style={{padding: 6, textAlign: 'right'}}>
                                    <button
                                        type='button'
                                        className='btn btn-tertiary btn-sm'
                                        disabled={disabled || idx === 0}
                                        onClick={() => moveRow(idx, -1)}
                                        title='Move up'
                                    >{'↑'}</button>
                                    {' '}
                                    <button
                                        type='button'
                                        className='btn btn-tertiary btn-sm'
                                        disabled={disabled || idx === rows.length - 1}
                                        onClick={() => moveRow(idx, 1)}
                                        title='Move down'
                                    >{'↓'}</button>
                                    {' '}
                                    <button
                                        type='button'
                                        className='btn btn-tertiary btn-sm'
                                        disabled={disabled}
                                        onClick={() => removeRow(idx)}
                                        title='Delete'
                                    >{'×'}</button>
                                </td>
                            </tr>
                        ))
                    )}
                </tbody>
            </table>

            <div style={{marginBottom: 16, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap'}}>
                <button
                    type='button'
                    className='btn btn-primary btn-sm'
                    disabled={disabled}
                    onClick={addRow}
                >
                    {'+ Add prefix'}
                </button>
                <span style={{marginLeft: 16}}>{'Load preset:'}</span>
                <select
                    className='form-control'
                    style={{width: 320}}
                    value={presetChoice}
                    disabled={disabled}
                    onChange={(e) => applyPreset(e.target.value)}
                >
                    <option value=''>{'-- pick a starting point --'}</option>
                    {PRESET_PACKS.map((p) => (
                        <option
                            key={p.name}
                            value={p.name}
                        >
                            {`${p.name} — ${p.description}`}
                        </option>
                    ))}
                </select>
            </div>

            {rows.length > 0 ? (
                <div style={{background: 'rgba(63, 67, 80, 0.06)', padding: 12, borderRadius: 4}}>
                    <div style={{fontWeight: 600, marginBottom: 8}}>{'Live preview'}</div>
                    <div style={{display: 'flex', gap: 8, alignItems: 'center', marginBottom: 8, flexWrap: 'wrap'}}>
                        <span>{'A request under prefix'}</span>
                        <select
                            className='form-control'
                            style={{width: 180}}
                            value={selectedPreviewPrefix}
                            onChange={(e) => setSelectedPreviewPrefix(e.target.value)}
                        >
                            {rows.map((r) => (
                                <option
                                    key={r.prefix}
                                    value={r.prefix}
                                >
                                    {r.prefix}
                                </option>
                            ))}
                        </select>
                        <span>{'with input'}</span>
                        <input
                            className='form-control'
                            style={{width: 220}}
                            value={previewInput}
                            placeholder='marketing team'
                            onChange={(e) => setPreviewInput(e.target.value)}
                        />
                    </div>
                    <div style={{fontFamily: 'monospace'}}>
                        {'→ '}<code>{previewResult}</code>
                    </div>
                </div>
            ) : null}
        </div>
    );
};
