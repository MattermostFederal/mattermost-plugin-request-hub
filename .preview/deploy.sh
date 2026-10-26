# Sourced by pr-preview's instance/deploy.sh after the PR's bundle is
# installed, with its helpers (compose, mmctl, wait_for_ping) and $id in
# scope. `make docker-setup` and `make deploy` only create the admin user and
# the `test` team and install the bundle, which the preview already does, so
# this adds what a reviewer needs to try the request flow: the approval team
# and channel prefixes the slash command requires (the plugin creates
# ~mattermost-channel-requests itself), and native channel creation taken away
# from non-admins (the README's recommended setup, since the plugin API can't
# veto it). The database outlives deploys but the config is fresh each time,
# so every step is idempotent.

APPROVAL_TEAM=test
# One "prefix|description|max suffix length" entry per line, the same
# prefixes the e2e suite uses.
CHANNEL_PREFIXES=$'team-|Team channels|32\nproj-|Project channels|32'
MM_SOCKET=docker/mattermost/run/mattermost_local.socket

mm_api() {
  curl -sf --unix-socket "$MM_SOCKET" -X "$1" "http://localhost/api/v4$2" \
    -H 'Content-Type: application/json' ${3:+-d "$3"}
}

# Channel creation is a team permission, held by team_user in the default
# scheme; system_user is cleared too in case a server version keeps it there.
CREATE_PERMS='["create_public_channel","create_private_channel"]'
role_create_perms() {
  mm_api GET "/roles/name/$1" | jq -r --argjson p "$CREATE_PERMS" '.permissions[] | select(IN($p[]))'
}
for role in team_user system_user; do
  held="$(role_create_perms "$role")"
  [ -z "$held" ] && continue
  # shellcheck disable=SC2086
  mmctl permissions remove "$role" $held >/dev/null
  held="$(role_create_perms "$role")"
  [ -z "$held" ] || { echo "${role} still holds ${held//$'\n'/ }"; exit 1; }
done

config="$(mm_api GET /config | jq -c --arg id "$id" --arg team "$APPROVAL_TEAM" --arg prefixes "$CHANNEL_PREFIXES" \
  '.PluginSettings.Plugins[$id] = ((.PluginSettings.Plugins[$id] // {}) + {approvalteam: $team, channelnameprefixes: $prefixes})')"
mm_api PUT /config "$config" >/dev/null
echo "configured ${id}: approvals post to ~mattermost-channel-requests in ${APPROVAL_TEAM}; non-admins request channels instead of creating them"
