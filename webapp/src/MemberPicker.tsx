import manifest from 'manifest';
import React, {useEffect, useRef, useState} from 'react';

import {makeDebug} from './debug';

// Traces autocomplete behavior when localStorage
// PLUGIN_CHANNEL_REQUESTS_DEBUG === "true". See ./debug.
const debug = makeDebug('member-picker');

// MemberPicker replaces the "Comma-separated usernames" text field in
// the request modal with a real autocomplete-driven picker.
//
// UX:
//   - Type in the input -> debounced fetch to /api/v1/user_autocomplete
//   - Matching users show as a dropdown below the input
//   - Click a user (or Enter on a highlighted one) -> add to the pill list
//   - "×" on a pill removes them
//   - The picker exposes value as a comma-separated username string via
//     onChange, so the parent Modal doesn't have to know about pills
//
// Debounce is 200ms — snappy enough to feel live, sparse enough not to
// hammer the endpoint on every keystroke.

type User = {
    id: string;
    username: string;
    nickname?: string;
    first_name?: string;
    last_name?: string;
};

type Props = {
    value: string; // comma-separated usernames
    disabled?: boolean;
    onChange: (value: string) => void;
    placeholder?: string;

    // teamId, when set, scopes autocomplete to members of that team
    // (channel create / Channel Admin). Mutually exclusive with notInTeamId.
    teamId?: string;

    // notInTeamId scopes autocomplete to users who are NOT on that team
    // (team member add — find people elsewhere on the instance).
    notInTeamId?: string;

    // usernameBadges maps username -> a short label ("Channel Admin",
    // "Member", "Auto-approved", etc.). When a candidate's username
    // is in this map, the dropdown row renders the label as a chip
    // next to the name. Enables "already assigned" visibility without
    // filtering the user out — you SEE the role at a glance.
    //
    // Candidates with a badge are grouped into a "CURRENT ASSIGNMENTS"
    // section at the top of the dropdown, above "AVAILABLE".
    usernameBadges?: Record<string, string>;
};

// Parse comma-separated usernames -> array of trimmed non-empty names.
export function parseUsernames(v: string): string[] {
    return v.
        split(',').
        map((s) => s.trim().replace(/^@/, '')).
        filter((s) => s.length > 0);
}

// avatarURL returns MM's built-in profile-image URL. MM serves this
// with the caller's cookie session so no additional auth is required;
// it gracefully falls back to a color+initial gradient for users
// without a set profile picture.
function avatarURL(userID: string): string {
    return `/api/v4/users/${userID}/image?_=0`;
}

// Fallback avatar: a colored circle with the user's initials. Used
// when the profile-image endpoint errors (e.g. permissions). Color is
// derived from a stable hash of the username so the same user always
// gets the same color.
function initialsFor(u: User): string {
    const first = (u.first_name ?? '').trim();
    const last = (u.last_name ?? '').trim();
    if (first || last) {
        return ((first[0] ?? '') + (last[0] ?? '')).toUpperCase();
    }
    return (u.username[0] ?? '?').toUpperCase();
}

function colorFor(username: string): string {
    let h = 0;
    for (let i = 0; i < username.length; i++) {
        h = ((h * 31) + username.charCodeAt(i)) >>> 0;
    }
    const hue = h % 360;
    return `hsl(${hue}, 55%, 45%)`;
}

// UserAvatar renders MM's profile image with an initials fallback.
// Uses onError to swap to the initials rendering — cleaner than
// probing the image existence separately.
const UserAvatar: React.FC<{user: User; size?: number}> = ({user, size = 26}) => {
    const [failed, setFailed] = useState(false);
    if (failed) {
        return (
            <div
                style={{
                    width: size,
                    height: size,
                    borderRadius: '50%',
                    background: colorFor(user.username),
                    color: '#fff',
                    display: 'inline-flex',
                    alignItems: 'center',
                    justifyContent: 'center',
                    fontSize: Math.round(size * 0.4),
                    fontWeight: 600,
                    flexShrink: 0,
                }}
            >
                {initialsFor(user)}
            </div>
        );
    }
    return (
        <img
            src={avatarURL(user.id)}
            alt=''
            width={size}
            height={size}
            style={{borderRadius: '50%', display: 'inline-block', flexShrink: 0, objectFit: 'cover'}}
            onError={() => setFailed(true)}
        />
    );
};

export const MemberPicker: React.FC<Props> = ({value, disabled, onChange, placeholder, teamId, notInTeamId, usernameBadges}) => {
    const [selected, setSelected] = useState<string[]>(() => parseUsernames(value));
    const [query, setQuery] = useState('');
    const [candidates, setCandidates] = useState<User[]>([]);
    const [showDropdown, setShowDropdown] = useState(false);
    const [highlightIndex, setHighlightIndex] = useState(0);
    const [dropdownRect, setDropdownRect] = useState<{top: number; left: number; width: number} | null>(null);
    const containerRef = useRef<HTMLDivElement>(null);

    // Track the input container's screen position so the dropdown can
    // render via position:fixed OUTSIDE the parent modal — the modal
    // has overflow:auto which would otherwise clip the dropdown when
    // this picker is near the bottom (like "Channel Admins to add").
    useEffect(() => {
        if (!showDropdown) {
            return undefined;
        }
        const updateRect = () => {
            const el = containerRef.current;
            if (!el) {
                return;
            }
            const r = el.getBoundingClientRect();
            setDropdownRect({top: r.bottom + 2, left: r.left, width: r.width});
        };
        updateRect();
        window.addEventListener('scroll', updateRect, true);
        window.addEventListener('resize', updateRect);
        return () => {
            window.removeEventListener('scroll', updateRect, true);
            window.removeEventListener('resize', updateRect);
        };
    }, [showDropdown, candidates.length]);

    // Re-sync when parent value changes externally.
    useEffect(() => {
        setSelected(parseUsernames(value));
    }, [value]);

    // Debounced autocomplete fetch. Scoped via ?team_id (in-team) or
    // ?not_in_team_id (everyone else). Strips leading @ from the query —
    // MM usernames don't contain @, so "@cfi" would never match; users
    // expect to type @-prefixed like every other mention affordance.
    useEffect(() => {
        // Strip any leading @ AND trim. If nothing meaningful left,
        // clear the dropdown.
        const q = query.trim().replace(/^@+/, '').trim();
        if (!q) {
            setCandidates([]);
            return undefined;
        }

        // Need exactly one scope — otherwise the API returns [].
        if ((teamId && notInTeamId) || (!teamId && !notInTeamId)) {
            debug('missing or conflicting scope', {teamId, notInTeamId});
            setCandidates([]);
            return undefined;
        }

        // Ignore a superseded in-flight fetch: when the query changes we
        // clear the timer, but a request already awaiting the network must
        // not apply its (stale) results over the newer query's.
        let cancelled = false;
        const timer = window.setTimeout(async () => {
            try {
                const params = new URLSearchParams({q});
                if (teamId) {
                    params.set('team_id', teamId);
                } else if (notInTeamId) {
                    params.set('not_in_team_id', notInTeamId);
                }
                const url = `/plugins/${manifest.id}/api/v1/user_autocomplete?${params.toString()}`;
                debug('fetching', {url, q, teamId, notInTeamId, selectedCount: selected.length});
                const response = await fetch(url, {
                    credentials: 'same-origin',
                    headers: {'X-Requested-With': 'XMLHttpRequest'},
                });
                if (cancelled) {
                    return;
                }
                if (!response.ok) {
                    debug('response not ok', {status: response.status});
                    setCandidates([]);
                    return;
                }
                const users: User[] = await response.json();
                if (cancelled) {
                    return;
                }
                debug('got users', {count: users.length, usernames: users.map((u) => u.username)});

                // Exclude users already in this picker's OWN selection
                // (shown as pills). NOT excluded: badged users — those
                // stay visible so the requester can see WHO is already
                // assigned where.
                const excluded = new Set(selected);
                const filtered = users.filter((u) => !excluded.has(u.username));
                debug('after exclude', {kept: filtered.length});
                setCandidates(filtered);
                setHighlightIndex(0);
            } catch (err) {
                if (!cancelled) {
                    debug('fetch threw', {err: String(err)});
                    setCandidates([]);
                }
            }
        }, 200);
        return () => {
            cancelled = true;
            window.clearTimeout(timer);
        };
    }, [query, selected, teamId, notInTeamId]);

    // Close dropdown when clicking outside.
    useEffect(() => {
        const handler = (e: MouseEvent) => {
            if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
                setShowDropdown(false);
            }
        };
        document.addEventListener('mousedown', handler);
        return () => document.removeEventListener('mousedown', handler);
    }, []);

    const commit = (next: string[]) => {
        setSelected(next);
        onChange(next.join(', '));
    };

    const addUser = (username: string) => {
        if (!username || selected.includes(username)) {
            return;
        }
        commit([...selected, username]);
        setQuery('');
        setCandidates([]);
    };

    const removeUser = (username: string) => {
        commit(selected.filter((u) => u !== username));
    };

    // Badged (assigned-elsewhere) candidates render in a section ABOVE the
    // available ones. highlightIndex is a flat index across that displayed
    // order, so keyboard selection MUST index this same ordered list — not
    // the raw `candidates` array — or Enter/Tab would add the wrong user
    // whenever a badged candidate sorts ahead of an unbadged one. These
    // three are the single source of truth for that order; the dropdown
    // renders `assigned` then `available` to match.
    const badges = usernameBadges ?? {};
    const assigned = candidates.filter((u) => badges[u.username]);
    const available = candidates.filter((u) => !badges[u.username]);
    const orderedCandidates = [...assigned, ...available];

    const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
        // Backspace on empty input removes the last selected user.
        if (e.key === 'Backspace' && !query && selected.length > 0) {
            commit(selected.slice(0, -1));
            return;
        }

        // With no visible dropdown, Enter still accepts the literal
        // typed name (useful for offline / unknown-user fallback).
        if (!showDropdown || orderedCandidates.length === 0) {
            if (e.key === 'Enter' && query.trim()) {
                e.preventDefault();
                addUser(query.trim().replace(/^@/, ''));
            }
            return;
        }
        if (e.key === 'ArrowDown') {
            e.preventDefault();
            setHighlightIndex((i) => Math.min(i + 1, orderedCandidates.length - 1));
        } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            setHighlightIndex((i) => Math.max(i - 1, 0));
        } else if (e.key === 'Enter' || e.key === 'Tab') {
            // Tab completes the currently highlighted candidate — same
            // behavior as Enter. Prevents Tab from bouncing focus out
            // of the picker mid-selection.
            e.preventDefault();
            const u = orderedCandidates[highlightIndex];
            if (u) {
                addUser(u.username);
            }
        } else if (e.key === 'Escape') {
            // Stop the dialog focus trap from treating this Escape as "close
            // the whole modal" while autocomplete suggestions are open.
            if (showDropdown) {
                e.preventDefault();
                e.stopPropagation();
                setShowDropdown(false);
            }
        }
    };

    return (
        <div
            ref={containerRef}
            style={{position: 'relative'}}
        >
            <div
                style={{
                    display: 'flex',
                    flexWrap: 'wrap',
                    gap: 4,
                    padding: 6,
                    minHeight: 34,
                    border: '1px solid var(--center-channel-color-rgb, rgba(0,0,0,0.16))',
                    borderRadius: 4,
                    background: 'var(--center-channel-bg, #fff)',
                }}
                onClick={() => {
                    setShowDropdown(true);
                    (containerRef.current?.querySelector('input') as HTMLInputElement | null)?.focus();
                }}
            >
                {selected.map((username) => (
                    <span
                        key={username}
                        style={{
                            display: 'inline-flex',
                            alignItems: 'center',
                            gap: 4,
                            padding: '2px 8px',
                            borderRadius: 12,
                            background: 'rgba(28, 88, 217, 0.12)',
                            fontSize: 13,
                        }}
                    >
                        {'@' + username}
                        <button
                            type='button'
                            style={{
                                border: 'none',
                                background: 'transparent',
                                cursor: 'pointer',
                                padding: 0,
                                fontSize: 14,
                                lineHeight: 1,
                            }}
                            disabled={disabled}
                            onClick={(e) => {
                                e.stopPropagation();
                                removeUser(username);
                            }}
                        >
                            {'×'}
                        </button>
                    </span>
                ))}
                <input
                    style={{
                        flex: 1,
                        minWidth: 120,
                        border: 'none',
                        outline: 'none',
                        background: 'transparent',
                        fontSize: 14,
                        padding: '2px 4px',
                    }}
                    value={query}
                    placeholder={selected.length === 0 ? (placeholder ?? 'Type a name...') : ''}
                    disabled={disabled}
                    onChange={(e) => {
                        setQuery(e.target.value);
                        setShowDropdown(true);
                    }}
                    onFocus={() => setShowDropdown(true)}
                    onKeyDown={handleKeyDown}
                />
            </div>

            {showDropdown && candidates.length > 0 && dropdownRect ? (
                <div
                    style={{

                        // Fixed positioning + explicit top/left/width
                        // computed from the input container's DOMRect.
                        // Avoids clipping by any ancestor with
                        // overflow:auto (the modal is one such).
                        position: 'fixed',
                        top: dropdownRect.top,
                        left: dropdownRect.left,
                        width: dropdownRect.width,
                        background: 'var(--center-channel-bg, #fff)',
                        color: 'var(--center-channel-color, #3d3c40)',
                        border: '1px solid rgba(0, 0, 0, 0.12)',
                        borderRadius: 4,
                        boxShadow: '0 4px 12px rgba(0, 0, 0, 0.24)',
                        maxHeight: 320,
                        overflowY: 'auto',
                        zIndex: 2000,
                    }}
                >
                    {(() => {
                        // Render the two visual sections (matching MM's
                        // mention dropdown pattern): "Current assignments"
                        // (badged, shown FIRST) then "Available to add".
                        // assigned/available/orderedCandidates are computed
                        // once above; flat indices here match that order so
                        // highlightIndex stays in sync with keyboard nav.
                        const renderRow = (u: User, flatIndex: number) => {
                            const badge = badges[u.username];
                            return (
                                <div
                                    key={u.id}
                                    style={{
                                        padding: '8px 12px',
                                        cursor: 'pointer',
                                        background: flatIndex === highlightIndex ? 'rgba(28, 88, 217, 0.10)' : 'transparent',
                                        display: 'flex',
                                        alignItems: 'center',
                                        gap: 10,
                                        fontSize: 14,
                                    }}
                                    onMouseEnter={() => setHighlightIndex(flatIndex)}
                                    onMouseDown={(e) => {
                                        e.preventDefault();
                                        addUser(u.username);
                                    }}
                                >
                                    <UserAvatar user={u}/>
                                    <div style={{flex: 1, minWidth: 0, display: 'flex', alignItems: 'baseline', gap: 6}}>
                                        <span style={{fontWeight: 600}}>{'@' + u.username}</span>
                                        {(() => {
                                            const full = `${u.first_name ?? ''} ${u.last_name ?? ''}`.trim();
                                            const secondary = full || u.nickname || '';
                                            return secondary ? (
                                                <span style={{opacity: 0.6, fontSize: 13}}>{secondary}</span>
                                            ) : null;
                                        })()}
                                    </div>
                                    {badge ? (
                                        <span
                                            style={{
                                                fontSize: 11,
                                                padding: '2px 8px',
                                                borderRadius: 10,
                                                background: 'rgba(28, 88, 217, 0.15)',
                                                color: 'rgb(20, 66, 165)',
                                                fontWeight: 500,
                                                whiteSpace: 'nowrap',
                                                flexShrink: 0,
                                            }}
                                        >
                                            {badge}
                                        </span>
                                    ) : null}
                                </div>
                            );
                        };

                        const SectionHeader: React.FC<{label: string}> = ({label}) => (
                            <div
                                style={{
                                    padding: '6px 12px 4px',
                                    fontSize: 11,
                                    fontWeight: 700,
                                    letterSpacing: 0.6,
                                    textTransform: 'uppercase',
                                    color: 'rgba(63, 67, 80, 0.72)',
                                    background: 'rgba(63, 67, 80, 0.04)',
                                }}
                            >
                                {label}
                            </div>
                        );

                        return (
                            <>
                                {assigned.length > 0 ? (
                                    <>
                                        <SectionHeader label='Current assignments'/>
                                        {assigned.map((u, i) => renderRow(u, i))}
                                    </>
                                ) : null}
                                {available.length > 0 ? (
                                    <>
                                        {assigned.length > 0 ? <SectionHeader label='Available to add'/> : null}
                                        {available.map((u, i) => renderRow(u, assigned.length + i))}
                                    </>
                                ) : null}
                            </>
                        );
                    })()}
                </div>
            ) : null}
        </div>
    );
};
