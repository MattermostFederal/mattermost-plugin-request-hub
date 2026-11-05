import manifest from 'manifest';
import React, {useEffect, useMemo, useState} from 'react';

// Mode-gated System Console settings for bot/webhook two-step approval.
// TwoStepApproverMode is the mode dropdown. SecurityApproverGroup and
// SecurityApproverAttribute only render when that mode is selected; the
// system step is always System Admins (no setting).

const DEFAULT_SECURITY_GROUP = 'security-approvers';
const DEFAULT_SECURITY_ATTR = 'security_approver';

type Mode = 'attribute' | 'usergroup' | 'system_admin';

type Props = {
    id: string;
    label?: string;
    helpText?: React.ReactNode;
    value: string;
    disabled?: boolean;
    onChange: (id: string, value: string) => void;
    setSaveNeeded?: () => void;
    config?: Record<string, unknown>;
    currentState?: Record<string, unknown>;
};

type GroupOption = {
    name: string;
    display_name: string;
    source: string;
};

type AttrOption = {
    name: string;
    display_name?: string;
};

function readSiblingSetting(props: Props, key: string): string {
    const pluginID = manifest.id;
    const readFrom = (source?: Record<string, unknown>): string | undefined => {
        if (!source) {
            return undefined;
        }
        const plugins = (source.PluginSettings as {Plugins?: Record<string, Record<string, unknown>>} | undefined)?.Plugins;
        const pluginCfg = plugins?.[pluginID];
        if (!pluginCfg) {
            return undefined;
        }
        const v = pluginCfg[key] ?? pluginCfg[key.toLowerCase()];
        return typeof v === 'string' ? v : undefined;
    };
    return readFrom(props.currentState) ?? readFrom(props.config) ?? '';
}

function normalizeMode(raw: string): Mode {
    switch ((raw || '').toLowerCase().trim()) {
    case 'usergroup':
    case 'group':
    case 'groups':
    case 'user_group':
    case 'user-group':
        return 'usergroup';
    case 'system_admin':
    case 'system-admin':
    case 'sysadmin':
    case 'admin':
        return 'system_admin';
    default:
        return 'attribute';
    }
}

async function fetchGroups(): Promise<GroupOption[]> {
    try {
        const response = await fetch('/api/v4/groups?page=0&per_page=200', {
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        });
        if (!response.ok) {
            return [];
        }
        const data = await response.json();
        if (!Array.isArray(data)) {
            return [];
        }
        return data.
            map((g: {name?: string; display_name?: string; source?: string}) => ({
                name: (g.name || '').trim(),
                display_name: (g.display_name || g.name || '').trim(),
                source: (g.source || '').trim() || 'custom',
            })).
            filter((g: GroupOption) => g.name !== '').
            sort((a: GroupOption, b: GroupOption) => a.display_name.localeCompare(b.display_name));
    } catch {
        return [];
    }
}

async function fetchAttributeFields(): Promise<AttrOption[]> {
    try {
        const response = await fetch('/api/v4/custom_profile_attributes/fields', {
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        });
        if (!response.ok) {
            return [];
        }
        const data = await response.json();
        if (!Array.isArray(data)) {
            return [];
        }
        return data.
            map((f: {name?: string; attrs?: {name?: string}; display_name?: string; label?: string}) => {
                const name = (f.name || f.attrs?.name || '').trim();
                return {
                    name,
                    display_name: (f.display_name || f.label || name).trim(),
                };
            }).
            filter((f: AttrOption) => f.name !== '').
            sort((a: AttrOption, b: AttrOption) => (a.display_name || a.name).localeCompare(b.display_name || b.name));
    } catch {
        return [];
    }
}

function commit(props: Props, next: string) {
    props.onChange(props.id, next);
    props.setSaveNeeded?.();
}

export const TwoStepApproverModeSetting: React.FC<Props> = (props) => {
    const {value, disabled} = props;
    const mode = normalizeMode(value);

    return (
        <div>
            <select
                className='form-control'
                disabled={disabled}
                value={mode}
                onChange={(e) => commit(props, e.target.value)}
                aria-label='Bot/webhook two-step approver mode'
            >
                <option value='attribute'>{'User Attributes (ABAC / CPA)'}</option>
                <option value='usergroup'>{'User Groups'}</option>
                <option value='system_admin'>{'System Admins only'}</option>
            </select>
            <p className='help-text'>
                {'Second approval is always a System Admin (a different person than the security reviewer), except in System Admins only mode where one admin can complete both steps.'}
            </p>
        </div>
    );
};

export const SecurityApproverGroupSetting: React.FC<Props> = (props) => {
    const {value, disabled} = props;
    const mode = normalizeMode(readSiblingSetting(props, 'TwoStepApproverMode'));
    const [groups, setGroups] = useState<GroupOption[]>([]);
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        if (mode !== 'usergroup') {
            setLoading(false);
            return undefined;
        }
        let cancelled = false;
        fetchGroups().then((list) => {
            if (!cancelled) {
                setGroups(list);
                setLoading(false);
            }
        });
        return () => {
            cancelled = true;
        };
    }, [mode]);

    const current = (value || '').trim() || DEFAULT_SECURITY_GROUP;
    const options = useMemo(() => {
        if (groups.some((g) => g.name === current || g.display_name.toLowerCase() === current.toLowerCase())) {
            return groups;
        }
        return [{name: current, display_name: `${current} (not created yet)`, source: 'pending'}, ...groups];
    }, [groups, current]);

    if (mode !== 'usergroup') {
        return null;
    }

    return (
        <div>
            <select
                className='form-control'
                disabled={disabled || loading}
                value={current}
                onChange={(e) => commit(props, e.target.value)}
                aria-label='Security approver user group'
                required={true}
            >
                {options.map((g) => (
                    <option
                        key={`${g.source}:${g.name}`}
                        value={g.name}
                    >
                        {g.display_name === g.name ? g.name : `${g.display_name} (${g.name})`}
                        {g.source && g.source !== 'pending' ? ` · ${g.source}` : ''}
                    </option>
                ))}
            </select>
            {!loading && groups.length === 0 && (
                <p className='help-text'>
                    {`No groups found yet. The default "${DEFAULT_SECURITY_GROUP}" is kept until you create that group (or pick another after it exists).`}
                </p>
            )}
        </div>
    );
};

export const SecurityApproverAttributeSetting: React.FC<Props> = (props) => {
    const {value, disabled} = props;
    const mode = normalizeMode(readSiblingSetting(props, 'TwoStepApproverMode'));
    const [fields, setFields] = useState<AttrOption[]>([]);
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        if (mode !== 'attribute') {
            setLoading(false);
            return undefined;
        }
        let cancelled = false;
        fetchAttributeFields().then((list) => {
            if (!cancelled) {
                setFields(list);
                setLoading(false);
            }
        });
        return () => {
            cancelled = true;
        };
    }, [mode]);

    const current = (value || '').trim() || DEFAULT_SECURITY_ATTR;
    const options = useMemo(() => {
        if (fields.some((f) => f.name === current)) {
            return fields;
        }
        return [{name: current, display_name: `${current} (not created yet)`}, ...fields];
    }, [fields, current]);

    if (mode !== 'attribute') {
        return null;
    }

    return (
        <div>
            <select
                className='form-control'
                disabled={disabled || loading}
                value={current}
                onChange={(e) => commit(props, e.target.value)}
                aria-label='Security approver attribute'
                required={true}
            >
                {options.map((f) => (
                    <option
                        key={f.name}
                        value={f.name}
                    >
                        {f.display_name && f.display_name !== f.name ? `${f.display_name} (${f.name})` : f.name}
                    </option>
                ))}
            </select>
            {!loading && fields.length === 0 && (
                <p className='help-text'>
                    {`No user attributes found yet. The default "${DEFAULT_SECURITY_ATTR}" is kept until you create that attribute (or pick another after it exists).`}
                </p>
            )}
        </div>
    );
};
