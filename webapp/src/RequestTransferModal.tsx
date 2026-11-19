import React, {useEffect, useState} from 'react';
import {useDispatch, useSelector} from 'react-redux';

import {
    fetchTransferableAssets,
    submitBotTransferRequest,
    submitWebhookTransferRequest,
} from './client';
import type {TransferableAsset} from './client';
import {useDialogFocusTrap} from './dialogA11y';
import {MemberPicker} from './MemberPicker';
import {
    closeBotTransferModal,
    closeWebhookTransferModal,
    getCurrentTeamId,
    isBotTransferModalOpen,
    isWebhookTransferModalOpen,
} from './store';
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
const labelStyle: React.CSSProperties = {display: 'block', fontWeight: 600, marginBottom: 4, fontSize: 14};

type Kind = 'bot' | 'webhook';

const BOT_ACTIONS: Array<[string, string]> = [
    ['transfer', 'Transfer to a new owner'],
    ['retire', 'Retire (disable + revoke tokens)'],
];
const WEBHOOK_ACTIONS: Array<[string, string]> = [
    ['reassign', 'Reassign to a new owner'],
    ['retire_recreate', 'Retire & recreate under a new owner'],
    ['retire_only', 'Retire (delete)'],
];

// actionNeedsOwner reports whether the chosen action needs a new owner.
function actionNeedsOwner(action: string): boolean {
    return action === 'transfer' || action === 'reassign' || action === 'retire_recreate';
}

function assetPlaceholder(loading: boolean, count: number): string {
    if (loading) {
        return 'Loading…';
    }
    if (count) {
        return 'Select…';
    }
    return 'Nothing you can transfer or retire';
}

type Props = {kind: Kind};

// RequestTransferModal collects a transfer/retire request for a bot or a
// standalone webhook and POSTs it. Both route through security + system
// approval on the server; secrets only ever reach the new owner after that.
export const RequestTransferModal: React.FC<Props> = ({kind}) => {
    const dispatch = useDispatch();
    const isBot = kind === 'bot';
    const isOpen = useSelector(isBot ? isBotTransferModalOpen : isWebhookTransferModalOpen);
    const teamId = useSelector((state: GlobalState) => getCurrentTeamId(state));

    const [assets, setAssets] = useState<TransferableAsset[]>([]);

    // Start true so the first paint does not flash "Nothing you can transfer".
    const [assetsLoading, setAssetsLoading] = useState(true);
    const [loadError, setLoadError] = useState('');
    const [assetId, setAssetId] = useState('');
    const [action, setAction] = useState(isBot ? 'transfer' : 'reassign');
    const [newOwner, setNewOwner] = useState('');
    const [rotate, setRotate] = useState(false);

    // rotateTouched marks an explicit checkbox choice so we only send
    // rotate_token when the user opted in/out; otherwise the server applies
    // its offboarding / owner-deactivated default.
    const [rotateTouched, setRotateTouched] = useState(false);
    const [note, setNote] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    useEffect(() => {
        if (!isOpen) {
            return undefined;
        }
        let cancelled = false;
        setAssetsLoading(true);
        fetchTransferableAssets().then((all) => {
            if (cancelled) {
                return;
            }
            const list = isBot ? all.bots : all.webhooks;
            setAssets(list);
            setLoadError('');
            setAssetsLoading(false);
            if (list.length === 1) {
                setAssetId(list[0].asset_id);
            }
        }).catch(() => {
            if (!cancelled) {
                setLoadError('Could not load the assets you can transfer. Try again.');
                setAssetsLoading(false);
            }
        });
        return () => {
            cancelled = true;
        };
    }, [isOpen, isBot]);

    const selectedOrphaned = Boolean(
        isBot && assetId && assets.find((a) => a.asset_id === assetId)?.orphaned,
    );

    useEffect(() => {
        if (!isBot || !assetId) {
            return;
        }

        // Visual default only — leave rotateTouched false so the server
        // applies rotateDefaultOn(reason) until the user toggles explicitly.
        setRotate(selectedOrphaned);
        setRotateTouched(false);
    }, [assetId, isBot, selectedOrphaned]);

    useEffect(() => {
        if (isBot || !assetId) {
            return;
        }
        const selected = assets.find((a) => a.asset_id === assetId);
        const allowed = selected?.allowed_actions;
        if (allowed && allowed.length > 0 && !allowed.includes(action)) {
            setAction(allowed[0]);
        }
    }, [assetId, assets, action, isBot]);

    const reset = () => {
        setAssets([]);
        setAssetsLoading(false);
        setLoadError('');
        setAssetId('');
        setAction(isBot ? 'transfer' : 'reassign');
        setNewOwner('');
        setRotate(false);
        setRotateTouched(false);
        setNote('');
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
        dispatch(isBot ? closeBotTransferModal() : closeWebhookTransferModal());
    };

    const {dialogRef, onKeyDown: onDialogKeyDown} = useDialogFocusTrap(close, isOpen);

    if (!isOpen) {
        return null;
    }

    const ownerUsername = newOwner.split(',')[0]?.trim().replace(/^@/, '') ?? '';

    const submit = async () => {
        if (submitting) {
            return;
        }
        if (!assetId) {
            setError(`Pick the ${kind} to act on.`);
            return;
        }
        if (actionNeedsOwner(action) && !ownerUsername) {
            setError('Pick the new owner.');
            return;
        }
        setSubmitting(true);
        setError('');
        try {
            const botPayload: Parameters<typeof submitBotTransferRequest>[0] = {
                bot_user_id: assetId,
                action: action as 'transfer' | 'retire',
                new_owner_username: ownerUsername,
                note: note.trim(),
            };
            if (rotateTouched) {
                botPayload.rotate_token = rotate;
            }
            const result = isBot ? await submitBotTransferRequest(botPayload) : await submitWebhookTransferRequest({
                hook_id: assetId,
                action: action as 'reassign' | 'retire_recreate' | 'retire_only',
                new_owner_username: ownerUsername,
                note: note.trim(),
            });
            if (result.error) {
                setError(result.error);
                return;
            }
            setSuccess(result.message || 'Your request was submitted for approval.');
        } catch {
            setError('Could not reach the server. Please check your connection and try again.');
        } finally {
            setSubmitting(false);
        }
    };

    const title = isBot ? 'Transfer or retire a bot' : 'Transfer or retire a webhook';
    const selectedAsset = assets.find((a) => a.asset_id === assetId);
    const actions = (() => {
        const base = isBot ? BOT_ACTIONS : WEBHOOK_ACTIONS;
        const allowed = selectedAsset?.allowed_actions;
        if (!allowed || allowed.length === 0) {
            return base;
        }
        const filtered = base.filter(([value]) => allowed.includes(value));
        return filtered.length ? filtered : base;
    })();
    const titleId = `transfer-title-${kind}`;

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
                aria-labelledby={titleId}
                tabIndex={-1}
                onKeyDown={onDialogKeyDown}
                onClick={(e) => e.stopPropagation()}
            >
                <div style={{display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start'}}>
                    <h3
                        id={titleId}
                        style={{marginTop: 0, marginBottom: 4}}
                    >{title}</h3>
                    <button
                        style={{background: 'none', border: 'none', cursor: 'pointer', fontSize: 20, lineHeight: 1, opacity: 0.6, padding: 0}}
                        onClick={close}
                        aria-label='Close'
                    >
                        {'×'}
                    </button>
                </div>
                <p style={{marginTop: 0, marginBottom: 16, opacity: 0.72, fontSize: 14}}>
                    {'Needs a security approval and a system approval. Secrets only reach the new owner after approval.'}
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
                                htmlFor='transfer-asset'
                            >{isBot ? 'Bot' : 'Webhook'}</label>
                            {loadError ? (
                                <p
                                    style={{color: 'var(--error-text, #d24b4e)'}}
                                    role='alert'
                                >{loadError}</p>
                            ) : (
                                <select
                                    id='transfer-asset'
                                    className='form-control'
                                    value={assetId}
                                    onChange={(e) => setAssetId(e.target.value)}
                                    disabled={assetsLoading}
                                >
                                    <option value=''>{assetPlaceholder(assetsLoading, assets.length)}</option>
                                    {assets.map((a) => (
                                        <option
                                            key={a.asset_id}
                                            value={a.asset_id}
                                        >{`${a.display_name}${a.orphaned ? ' (owner deactivated)' : ''}`}</option>
                                    ))}
                                </select>
                            )}
                        </div>

                        <div style={fieldStyle}>
                            <label
                                style={labelStyle}
                                htmlFor='transfer-action'
                            >{'Action'}</label>
                            <select
                                id='transfer-action'
                                className='form-control'
                                value={action}
                                onChange={(e) => {
                                    const next = e.target.value;
                                    setAction(next);
                                    if (next === 'transfer') {
                                        // Re-apply the orphaned visual default
                                        // without marking an explicit choice.
                                        setRotate(selectedOrphaned);
                                        setRotateTouched(false);
                                    } else {
                                        setRotate(false);
                                        setRotateTouched(false);
                                    }
                                }}
                            >
                                {actions.map(([value, label]) => (
                                    <option
                                        key={value}
                                        value={value}
                                    >{label}</option>
                                ))}
                            </select>
                        </div>

                        {actionNeedsOwner(action) ? (
                            <div style={fieldStyle}>
                                <label
                                    style={labelStyle}
                                    htmlFor='transfer-new-owner'
                                >{'New owner'}</label>
                                <MemberPicker
                                    inputId='transfer-new-owner'
                                    value={newOwner}
                                    onChange={setNewOwner}
                                    teamId={teamId}
                                    single={true}
                                    placeholder='Type a name…'
                                />
                            </div>
                        ) : null}

                        {isBot && action === 'transfer' ? (
                            <div style={fieldStyle}>
                                <label style={{...labelStyle, display: 'flex', alignItems: 'center', gap: 8, fontWeight: 400}}>
                                    <input
                                        type='checkbox'
                                        checked={rotate}
                                        onChange={(e) => {
                                            setRotateTouched(true);
                                            setRotate(e.target.checked);
                                        }}
                                    />
                                    {'Rotate the access token (revoke the old one, issue a fresh one to the new owner)'}
                                </label>
                            </div>
                        ) : null}

                        <div style={fieldStyle}>
                            <label
                                style={labelStyle}
                                htmlFor='transfer-note'
                            >{'Note'}</label>
                            <textarea
                                id='transfer-note'
                                className='form-control'
                                rows={3}
                                value={note}
                                onChange={(e) => setNote(e.target.value)}
                                placeholder='Optional reason or context for the approvers'
                                maxLength={500}
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
                            >{submitting ? 'Submitting…' : 'Submit request'}</button>
                        </div>
                    </>
                )}
            </div>
        </div>
    );
};

// Named wrappers so each modal registers as its own root component with a
// stable name (the webapp registry keys root components by function name).
export const BotTransferModal = () => <RequestTransferModal kind='bot'/>;
export const WebhookTransferModal = () => <RequestTransferModal kind='webhook'/>;
