import manifest from 'manifest';
import React, {useEffect, useMemo, useRef, useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {fetchPrefixes, fetchSidebarCategories, submitChannelRequest} from './client';
import type {ChannelPrefix, SidebarCategory} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {MemberPicker, parseUsernames} from './MemberPicker';
import {closeRequestModal, getCurrentTeamId, isRequestModalOpen} from './store';
import type {GlobalState} from './store';

// Slugify mirrors the server-side slugify() so the live preview shows
// the exact URL the server will produce. Keep in sync with
// server/request.go's slugify() — same char-class + trim + lower.
function slugifySuffix(input: string): string {
    return input.
        toLowerCase().
        trim().
        replace(/[^a-z0-9]+/g, '-').
        replace(/^-+|-+$/g, '');
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

export const RequestChannelModal = () => {
    const dispatch = useDispatch();
    const isOpen = useSelector(isRequestModalOpen);
    const teamId = useSelector((state: GlobalState) => getCurrentTeamId(state));

    const [displayName, setDisplayName] = useState('');
    const [urlName, setUrlName] = useState('');
    const [purpose, setPurpose] = useState('');
    const [channelType, setChannelType] = useState('O');
    const [membersText, setMembersText] = useState('');

    // adminMembersText holds the second picker's selection — users the
    // requester is proposing to be Channel Admins on the new channel.
    // Stored as a comma-separated username string, same shape as
    // membersText, so the payload path is symmetric.
    const [adminMembersText, setAdminMembersText] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');
    const [prefixes, setPrefixes] = useState<ChannelPrefix[]>([]);
    const [prefixesLoaded, setPrefixesLoaded] = useState(false);
    const [prefixError, setPrefixError] = useState(false);
    const [selectedPrefix, setSelectedPrefix] = useState('');
    const [categories, setCategories] = useState<SidebarCategory[]>([]);
    const [selectedCategory, setSelectedCategory] = useState('');

    // Generation counter so only the latest fetch updates state. Rapid
    // Retry clicks, or close-then-reopen while a fetch is in flight, would
    // otherwise let a stale success/error overwrite the current one.
    const loadGenRef = useRef(0);

    // Load the admin-configured prefixes. An empty list means "not
    // configured"; a fetch failure sets prefixError so the modal shows a
    // retry instead of the misleading "not configured" notice (a transient
    // 500 must not look like an unconfigured plugin).
    const loadPrefixes = () => {
        const gen = ++loadGenRef.current;
        setPrefixError(false);
        setPrefixesLoaded(false);
        fetchPrefixes().then((list) => {
            if (gen !== loadGenRef.current) {
                return;
            }
            setPrefixes(list);
            setPrefixesLoaded(true);
            if (list.length > 0) {
                setSelectedPrefix((cur) => cur || list[0].prefix);
            }
        }).catch(() => {
            if (gen !== loadGenRef.current) {
                return;
            }
            setPrefixError(true);
            setPrefixesLoaded(true);
        });
    };

    useEffect(() => {
        if (!isOpen) {
            return;
        }
        loadPrefixes();

        // Load sidebar categories alongside prefixes. Failures are silent —
        // the dropdown just won't appear (feature degrades gracefully).
        fetchSidebarCategories(teamId).then(setCategories).catch(() => setCategories([]));
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [isOpen]);

    const usingPrefixList = prefixes.length > 0;

    // Live preview of the final channel URL: prefix + slugified suffix
    // (or slugified display name when the suffix field is blank).
    const previewURL = useMemo(() => {
        const baseInput = urlName.trim() || displayName;
        const slug = slugifySuffix(baseInput);
        return selectedPrefix + (slug || 'suffix');
    }, [urlName, displayName, selectedPrefix]);

    const reset = () => {
        setDisplayName('');
        setUrlName('');
        setPurpose('');
        setChannelType('O');
        setMembersText('');
        setAdminMembersText('');
        setSelectedCategory('');
        setError('');
        setSuccess('');
        setSubmitting(false);
        setSelectedPrefix(prefixes[0]?.prefix ?? '');

        // Invalidate any in-flight fetch and force a fresh load next open so
        // a stale loaded/error state doesn't flash before the new fetch.
        loadGenRef.current++;
        setPrefixesLoaded(false);
        setPrefixError(false);
    };

    const close = () => {
        reset();
        dispatch(closeRequestModal());
    };

    // Must run unconditionally — never after `if (!isOpen) return null`.
    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

    const submit = async () => {
        if (!displayName.trim()) {
            setError('A channel name is required.');
            return;
        }

        setSubmitting(true);
        setError('');

        const members = parseUsernames(membersText);
        const adminMembers = parseUsernames(adminMembersText);

        try {
            const result = await submitChannelRequest({
                team_id: teamId,
                display_name: displayName.trim(),
                name: urlName.trim(),
                prefix: selectedPrefix,
                purpose: purpose.trim(),
                channel_type: channelType,
                members,
                admin_members: adminMembers,
                category: selectedCategory || undefined,
            });

            if (result.error) {
                setError(result.error);
                return;
            }

            setSuccess(result.message || 'Your channel request has been submitted.');
        } catch {
            // Network failure or unexpected throw — surface it instead of
            // leaving the button stuck on "Submitting…".
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
                aria-labelledby='rc-title'
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <h3
                    id='rc-title'
                    style={{marginTop: 0}}
                >{'Request a Channel'}</h3>
                <p style={{opacity: 0.72}}>
                    {'Your request will be sent to an admin for approval. '}
                    <a
                        href={`/plugins/${manifest.id}/public/help/docs.html`}
                        target='_blank'
                        rel='noopener noreferrer'
                    >
                        {'Need help?'}
                    </a>
                </p>

                {(() => {
                    if (success) {
                        return (
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
                        );
                    }
                    if (!prefixesLoaded) {
                        return <div style={{opacity: 0.7}}>{'Loading…'}</div>;
                    }
                    if (prefixError) {
                        return (
                            <div>
                                <div
                                    className='alert alert-danger'
                                    role='alert'
                                    style={{marginBottom: 16}}
                                >
                                    {'Could not load channel request settings. Please try again.'}
                                </div>
                                <div style={{textAlign: 'right'}}>
                                    <button
                                        className='btn btn-tertiary'
                                        style={{marginRight: 8}}
                                        onClick={close}
                                    >
                                        {'Close'}
                                    </button>
                                    <button
                                        className='btn btn-primary'
                                        onClick={loadPrefixes}
                                    >
                                        {'Retry'}
                                    </button>
                                </div>
                            </div>
                        );
                    }
                    if (!usingPrefixList) {
                        return (
                            <div>
                                <div
                                    className='alert alert-warning'
                                    style={{marginBottom: 16}}
                                >
                                    {'Channel creation requests aren’t configured yet — no channel prefixes have been defined. Ask a System Admin to add at least one prefix in System Console → Plugins → Mattermost Request Hub, then try again.'}
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
                        );
                    }
                    return (
                        <div>
                            <div style={fieldStyle}>
                                <label htmlFor='cr-prefix'>{'Domain prefix'}</label>
                                <select
                                    id='cr-prefix'
                                    className='form-control'
                                    value={selectedPrefix}
                                    onChange={(e) => setSelectedPrefix(e.target.value)}
                                >
                                    {prefixes.map((p) => (
                                        <option
                                            key={p.prefix}
                                            value={p.prefix}
                                        >
                                            {p.description ? `${p.prefix}  (${p.description})` : p.prefix}
                                        </option>
                                ))}
                                </select>
                                <small style={{opacity: 0.6}}>{'Choose the category for this channel. The final URL is <prefix><suffix>.'}</small>
                            </div>

                            <div style={fieldStyle}>
                                <label htmlFor='cr-display-name'>{'Channel name'}</label>
                                <input
                                    id='cr-display-name'
                                    className='form-control'
                                    value={displayName}
                                    maxLength={64}
                                    placeholder='e.g. Marketing Team'
                                    onChange={(e) => setDisplayName(e.target.value)}
                                />
                            </div>

                            <div style={fieldStyle}>
                                <label htmlFor='cr-url-name'>{'URL suffix (optional)'}</label>
                                <input
                                    id='cr-url-name'
                                    className='form-control'
                                    value={urlName}
                                    maxLength={64}
                                    placeholder='The part after the prefix — leave blank to generate from the channel name'
                                    onChange={(e) => setUrlName(e.target.value)}
                                />
                                <small style={{opacity: 0.6, display: 'block', marginTop: 4}}>
                                    {'Preview: '}<code>{previewURL}</code>
                                </small>
                            </div>

                            <div style={fieldStyle}>
                                <label htmlFor='cr-purpose'>{'Purpose (optional)'}</label>
                                <textarea
                                    id='cr-purpose'
                                    className='form-control'
                                    value={purpose}
                                    maxLength={250}
                                    rows={2}
                                    onChange={(e) => setPurpose(e.target.value)}
                                />
                            </div>

                            <div style={fieldStyle}>
                                <label htmlFor='cr-type'>{'Visibility'}</label>
                                <select
                                    id='cr-type'
                                    className='form-control'
                                    value={channelType}
                                    onChange={(e) => setChannelType(e.target.value)}
                                >
                                    <option value='O'>{'Public'}</option>
                                    <option value='P'>{'Private'}</option>
                                </select>
                            </div>

                            {(() => {
                            // Build cross-picker badge maps so each
                            // picker's dropdown shows a "Currently:
                            // Channel Admin" or "Currently: Member"
                            // chip next to any user already claimed by
                            // the sibling picker. Requester sees at a
                            // glance who is assigned where.
                            const memberUsernames = parseUsernames(membersText);
                            const adminUsernames = parseUsernames(adminMembersText);
                            const badgesForMembersPicker: Record<string, string> = {};
                            adminUsernames.forEach((u) => {
                                badgesForMembersPicker[u] = 'Channel Admin';
                            });
                            const badgesForAdminsPicker: Record<string, string> = {};
                            memberUsernames.forEach((u) => {
                                badgesForAdminsPicker[u] = 'Member';
                            });

                            // When the requester picks a user in one
                            // picker, drop them from the sibling if
                            // they were previously there — a user can
                            // only have one role at creation time.
                            // The onChange handlers below implement
                            // this move-on-select semantics.
                            return (
                                <>
                                    <div style={fieldStyle}>
                                        <label htmlFor='cr-members'>{'Members to add (optional)'}</label>
                                        <MemberPicker
                                            value={membersText}
                                            teamId={teamId}
                                            usernameBadges={badgesForMembersPicker}
                                            placeholder='Type a name — Tab / Enter to add'
                                            onChange={(newMembers) => {
                                                setMembersText(newMembers);

                                                // If the user just added
                                                // was in the admin list,
                                                // remove them there.
                                                const newSet = new Set(parseUsernames(newMembers));
                                                const stillAdmins = adminUsernames.filter((u) => !newSet.has(u));
                                                if (stillAdmins.length !== adminUsernames.length) {
                                                    setAdminMembersText(stillAdmins.join(', '));
                                                }
                                            }}
                                        />
                                    </div>

                                    <div style={fieldStyle}>
                                        <label htmlFor='cr-admin-members'>{'Channel Admins to add (optional)'}</label>
                                        <MemberPicker
                                            value={adminMembersText}
                                            teamId={teamId}
                                            usernameBadges={badgesForAdminsPicker}
                                            placeholder='Type a name — Tab / Enter to promote to Channel Admin'
                                            onChange={(newAdmins) => {
                                                setAdminMembersText(newAdmins);

                                                // If the user just added
                                                // was in the member list,
                                                // remove them there.
                                                const newSet = new Set(parseUsernames(newAdmins));
                                                const stillMembers = memberUsernames.filter((u) => !newSet.has(u));
                                                if (stillMembers.length !== memberUsernames.length) {
                                                    setMembersText(stillMembers.join(', '));
                                                }
                                            }}
                                        />
                                        <small style={{opacity: 0.6, display: 'block', marginTop: 4}}>
                                            {'These users are promoted to Channel Admin on the newly-created channel. Users with a "Member" badge in the dropdown are currently in the Members list above — picking them here moves them to Channel Admin.'}
                                        </small>
                                    </div>
                                </>
                            );
                        })()}

                            <div style={fieldStyle}>
                                <label htmlFor='cr-category'>{'Sidebar category (optional)'}</label>
                                <input
                                    id='cr-category'
                                    className='form-control'
                                    list='cr-category-suggestions'
                                    value={selectedCategory}
                                    onChange={(e) => setSelectedCategory(e.target.value)}
                                    placeholder='Pick an existing category or type a new name'
                                    maxLength={64}
                                    autoComplete='off'
                                />
                                <datalist id='cr-category-suggestions'>
                                    {categories.map((c) => (
                                        <option
                                            key={c.id}
                                            value={c.display_name}
                                        />
                                ))}
                                </datalist>
                                <small style={{opacity: 0.6}}>
                                    {'Pick from suggestions or type a new name. On approval, the channel is placed there for you and listed members — a missing category is created automatically.'}
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
                    );
                })()}
            </div>
        </div>
    );
};
