import manifest from 'manifest';
import React, {useEffect, useRef, useState} from 'react';

// ApprovalChannelPicker: TWO coordinated custom admin-console settings
// that replace the plain text slug fields for ApprovalTeam +
// ApprovalChannel. The team picker is a dropdown of teams the current
// admin is a member of. The channel picker is a dropdown of channels
// in whichever team is currently selected — updates reactively when
// the team changes.
//
// Because registerAdminConsoleCustomSetting maps one component per
// setting key, we export TWO components — TeamPicker (registered for
// key "ApprovalTeam") and ChannelPicker (registered for
// "ApprovalChannel") — and coordinate them through the `currentState`
// prop MM passes to each component so the channel picker sees the
// team selection immediately without a page reload.

type Team = {
    id: string;
    name: string;
    display_name: string;
};

type Channel = {
    id: string;
    name: string;
    display_name: string;
    type: string;
};

// MM plugin custom-setting props. `config` is the last-saved config;
// `currentState` reflects unsaved pending changes so a sibling picker
// can see the tentative value the admin just clicked.
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

// Read the current tentative value of a sibling plugin setting. MM
// nests plugin config under PluginSettings.Plugins.<plugin-id> — we
// try both currentState (unsaved) and config (saved), preferring the
// former. Returns "" if the key isn't found.
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

        // Config keys are case-insensitively matched by MM; try both.
        const v = pluginCfg[key] ?? pluginCfg[key.toLowerCase()];
        return typeof v === 'string' ? v : undefined;
    };
    return readFrom(props.currentState) ?? readFrom(props.config) ?? '';
}

// Small fetch helpers — pull the team + channel lists from the plugin's
// own server endpoints (which respect the admin's per-team visibility).
async function fetchTeams(): Promise<Team[]> {
    try {
        const response = await fetch(`/plugins/${manifest.id}/api/v1/teams`, {
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        });
        if (!response.ok) {
            return [];
        }
        return await response.json();
    } catch {
        return [];
    }
}

async function fetchChannels(teamID: string): Promise<Channel[]> {
    if (!teamID) {
        return [];
    }
    try {
        const response = await fetch(`/plugins/${manifest.id}/api/v1/channels?team_id=${encodeURIComponent(teamID)}`, {
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        });
        if (!response.ok) {
            return [];
        }
        return await response.json();
    } catch {
        return [];
    }
}

// TeamPicker: dropdown of teams. Writes the team NAME (slug) to
// ApprovalTeam, matching what the plugin's server-side code expects.
export const TeamPicker: React.FC<Props> = (props) => {
    const {id, value, disabled, onChange, setSaveNeeded} = props;
    const [teams, setTeams] = useState<Team[]>([]);
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        fetchTeams().then((list) => {
            setTeams(list);
            setLoading(false);
        });
    }, []);

    // If the saved slug isn't in the list (admin not on that team, or teams
    // still loading), keep an option so the console doesn't look blank and a
    // careless Save can't overwrite ApprovalTeam with "".
    const saved = value ?? '';
    const savedMissing = Boolean(saved) && !loading && !teams.some((t) => t.name === saved);

    return (
        <div>
            <select
                className='form-control'
                value={saved}
                disabled={disabled ?? loading}
                onChange={(e) => {
                    onChange(id, e.target.value);
                    setSaveNeeded?.();
                }}
            >
                <option value=''>{loading ? '-- loading teams --' : '-- pick a team --'}</option>
                {savedMissing && (
                    <option value={saved}>
                        {`${saved}  (saved — join this team or re-pick to change)`}
                    </option>
                )}
                {teams.map((t) => (
                    <option
                        key={t.id}
                        value={t.name}
                    >
                        {`${t.display_name || t.name}  (/${t.name})`}
                    </option>
                ))}
            </select>
            <small style={{opacity: 0.6}}>{'Team where the plugin creates approval channels (channel, bot, webhook, team, audit). Required for the approval workflow. Blank = tickets fall back to System Admin DMs.'}</small>
        </div>
    );
};

// ChannelPicker: dropdown of channels in the currently-selected team.
// Reads sibling ApprovalTeam via currentState so it updates the
// moment the admin picks a team, without a page reload.
export const ChannelPicker: React.FC<Props> = (props) => {
    const {id, value, disabled, onChange, setSaveNeeded} = props;
    const teamSlug = readSiblingSetting(props, 'ApprovalTeam');

    const [channels, setChannels] = useState<Channel[]>([]);
    const [loading, setLoading] = useState(false);

    // Read the latest value/onChange from the effect without making them
    // effect dependencies (which would re-run the fetch on every keystroke
    // elsewhere in the console). prevTeamRef distinguishes a real team
    // change from the initial mount.
    const valueRef = useRef(value);
    valueRef.current = value;
    const onChangeRef = useRef(onChange);
    onChangeRef.current = onChange;
    const prevTeamRef = useRef<string | null>(null);

    // Resolve team slug -> team id, then fetch that team's channels.
    // Cheapest path: fetch the full team list (we already do it for
    // TeamPicker; the browser caches) and look up by name.
    useEffect(() => {
        // Guard against a slower earlier fetch resolving after a newer one
        // when the admin switches teams rapidly — a stale response must not
        // overwrite the current team's channels.
        let cancelled = false;

        const isTeamChange = prevTeamRef.current !== null && prevTeamRef.current !== teamSlug;
        prevTeamRef.current = teamSlug;

        // When the admin switches teams, clear any previously-selected
        // channel so a slug from the OLD team can't be saved against the
        // new one. Only on a genuine change — never clobber the saved
        // value on initial mount.
        const clearStaleChannel = () => {
            if (isTeamChange && valueRef.current) {
                onChangeRef.current(id, '');
                setSaveNeeded?.();
            }
        };

        if (!teamSlug) {
            setChannels([]);
            clearStaleChannel();
            return undefined;
        }
        setLoading(true);
        fetchTeams().then(async (teams) => {
            if (cancelled) {
                return;
            }
            const match = teams.find((t) => t.name === teamSlug);
            if (!match) {
                setChannels([]);
                setLoading(false);
                clearStaleChannel();
                return;
            }
            const list = await fetchChannels(match.id);
            if (cancelled) {
                return;
            }
            setChannels(list);
            setLoading(false);

            // Clear the selection if the team changed and the old channel
            // isn't one of the new team's channels.
            if (isTeamChange && valueRef.current && !list.some((c) => c.name === valueRef.current)) {
                onChangeRef.current(id, '');
                setSaveNeeded?.();
            }
        });
        return () => {
            cancelled = true;
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [teamSlug]);

    return (
        <div>
            <select
                className='form-control'
                value={value ?? ''}
                disabled={disabled ?? (loading || !teamSlug)}
                onChange={(e) => {
                    onChange(id, e.target.value);
                    setSaveNeeded?.();
                }}
            >
                <option value=''>
                    {(() => {
                        if (!teamSlug) {
                            return '-- pick a team first --';
                        }
                        if (loading) {
                            return '-- loading channels --';
                        }
                        if (channels.length === 0) {
                            return '-- no channels visible --';
                        }
                        return '-- pick a channel --';
                    })()}
                </option>
                {channels.map((c) => (
                    <option
                        key={c.id}
                        value={c.name}
                    >
                        {`${c.type === 'P' ? '🔒 ' : ''}${c.display_name || c.name}  (/${c.name})`}
                    </option>
                ))}
            </select>
        </div>
    );
};
