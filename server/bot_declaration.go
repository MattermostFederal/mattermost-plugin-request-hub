package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// A bot requester declares what the bot is for, what it may do, what data
// it takes in and where it runs, and names a home team and channel. The
// declaration is kept with the request, its history and the bot index, so
// Security can list the bots Request Hub created and what each was meant to
// do. It is declared intent: nothing here limits what the bot's token can do.

const (
	privilegeRead     = "read"
	privilegePost     = "post"
	privilegeElevated = "elevated"

	// defaultBotHostingEnvironments is used when BotHostingEnvironments is
	// blank.
	defaultBotHostingEnvironments = "mattermost-dev\nmattermost-prod"

	// maxHostingEnvLen caps an environment name in an export filter.
	maxHostingEnvLen = 200

	kvBotIndexPrefix = "bot_index_"
	botIndexPageSize = 200
)

// botPrivilegeLevels are the privilege levels a requester can declare, in
// the order the forms list them, with their labels.
var botPrivilegeLevels = []struct{ Value, Label string }{
	{privilegeRead, "Read only"},
	{privilegePost, "Read and post"},
	{privilegeElevated, "Elevated (admin-level APIs)"},
}

// privilegeLabel returns a privilege level's label, or the value itself
// when it is unknown.
func privilegeLabel(value string) string {
	for _, level := range botPrivilegeLevels {
		if level.Value == value {
			return level.Label
		}
	}
	return value
}

// HostingEnvs returns the hosting environments a bot requester picks from:
// one per line of BotHostingEnvironments, or the defaults when it is blank.
func (c *configuration) HostingEnvs() []string {
	envs := parseHostingEnvs(c.BotHostingEnvironments)
	if len(envs) == 0 {
		return parseHostingEnvs(defaultBotHostingEnvironments)
	}
	return envs
}

func parseHostingEnvs(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(raw, "\n") {
		env := strings.TrimSpace(line)
		if env == "" || seen[env] {
			continue
		}
		seen[env] = true
		out = append(out, env)
	}
	return out
}

// botDeclaration is what the requester declares about the bot.
type botDeclaration struct {
	Purpose        string
	PrivilegeLevel string
	DataIngestion  string
	HostingEnv     string
}

// checkBotDeclaration validates the declared fields and returns them
// trimmed, with the privilege level lowercased.
func checkBotDeclaration(c *configuration, in botDeclaration) (botDeclaration, error) {
	out := botDeclaration{
		Purpose:        strings.TrimSpace(in.Purpose),
		PrivilegeLevel: strings.ToLower(strings.TrimSpace(in.PrivilegeLevel)),
		DataIngestion:  strings.TrimSpace(in.DataIngestion),
		HostingEnv:     strings.TrimSpace(in.HostingEnv),
	}
	if out.Purpose == "" {
		return out, errors.New("Describe the bot's purpose.")
	}
	if err := checkTextLen("Purpose", out.Purpose, maxDescriptionLen); err != nil {
		return out, err
	}
	if privilegeLabel(out.PrivilegeLevel) == out.PrivilegeLevel {
		return out, errors.Errorf("Pick a privilege level: %s.", strings.Join(privilegeValues(), ", "))
	}
	if out.DataIngestion == "" {
		return out, errors.New("Describe the data the bot takes in.")
	}
	if err := checkTextLen("Data ingestion", out.DataIngestion, maxDescriptionLen); err != nil {
		return out, err
	}
	envs := c.HostingEnvs()
	if slices.Contains(envs, out.HostingEnv) {
		return out, nil
	}
	return out, errors.Errorf("Pick a hosting environment: %s.", strings.Join(envs, ", "))
}

func privilegeValues() []string {
	out := make([]string, len(botPrivilegeLevels))
	for i, level := range botPrivilegeLevels {
		out[i] = level.Value
	}
	return out
}

// botHomeMissingMsg is the reply when a request names no home channel,
// such as a slash command run in a direct message.
const botHomeMissingMsg = "A bot request needs a home channel: submit it from a public or private channel of a team you're in."

// requireBotHome checks the bot's home team and channel at submit: the
// requester is on the team and in the channel (System Admins: any), and the
// channel is a live public or private channel of that team. Membership is
// checked first, so someone outside the channel gets the same reply
// whatever state it is in.
func (p *Plugin) requireBotHome(requester *model.User, teamID, channelID string) error {
	if teamID == "" || channelID == "" {
		return errors.New(botHomeMissingMsg)
	}
	if !requester.IsSystemAdmin() {
		member, appErr := p.API.GetTeamMember(teamID, requester.Id)
		if appErr != nil || member == nil || member.DeleteAt != 0 {
			return errors.New("The bot's home team must be a team you're a member of.")
		}
	}
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil || channel == nil || channel.TeamId != teamID {
		return errors.New(botHomeMissingMsg)
	}
	if !requester.IsSystemAdmin() {
		if _, memErr := p.API.GetChannelMember(channelID, requester.Id); memErr != nil {
			return errors.New("The bot's home channel must be a channel you're a member of.")
		}
	}
	if channel.Type != model.ChannelTypeOpen && channel.Type != model.ChannelTypePrivate {
		return errors.New(botHomeMissingMsg)
	}
	if channel.DeleteAt != 0 {
		return errors.Errorf("~%s is archived, so it can't be the bot's home channel.", channel.Name)
	}
	if team, teamErr := p.API.GetTeam(teamID); teamErr != nil || team == nil || team.DeleteAt != 0 {
		return errors.New("The bot's home team is archived or could not be found.")
	}
	return nil
}

// botHomeBlocker re-checks the home team and channel before an approval
// step, as teamGuestApprovalBlocker does for a guest's channels. Requests
// submitted before homes existed have none and skip it.
func (p *Plugin) botHomeBlocker(req *botRequest, requester *model.User) string {
	if req.HomeTeamID == "" || req.HomeChannelID == "" {
		return ""
	}
	team, teamErr := p.API.GetTeam(req.HomeTeamID)
	if msg := p.teamBlocker(req.HomeTeamID, team, teamErr); msg != "" {
		return msg
	}
	if msg := p.requesterTeamBlocker(req.HomeTeamID, requester); msg != "" {
		return msg
	}
	channel, appErr := p.API.GetChannel(req.HomeChannelID)
	if msg := p.channelBlocker(req.HomeChannelID, channel, appErr); msg != "" {
		return msg
	}
	if channel.TeamId != req.HomeTeamID {
		return fmt.Sprintf("~%s is no longer in the bot's home team. Deny this request instead.", channel.Name)
	}
	return p.requesterChannelBlocker(req.HomeChannelID, requester)
}

// botIndexEntry links a bot Request Hub created to its request, and keeps
// what was declared for it, so the bot can be listed and traced by its user
// ID even after the request's history is gone.
type botIndexEntry struct {
	BotUserID      string         `json:"bot_user_id"`
	RequestID      string         `json:"request_id"`
	Username       string         `json:"username"`
	DisplayName    string         `json:"display_name,omitempty"`
	RequesterID    string         `json:"requester_id"`
	OwnerUserID    string         `json:"owner_user_id,omitempty"`
	CreatedAt      int64          `json:"created_at"`
	RequestedAt    int64          `json:"requested_at,omitempty"`
	ApprovedBy     string         `json:"approved_by,omitempty"`
	Purpose        string         `json:"purpose,omitempty"`
	PrivilegeLevel string         `json:"privilege_level,omitempty"`
	DataIngestion  string         `json:"data_ingestion,omitempty"`
	HostingEnv     string         `json:"hosting_env,omitempty"`
	HomeTeamID     string         `json:"home_team_id,omitempty"`
	HomeChannelID  string         `json:"home_channel_id,omitempty"`
	Compliance     *botCompliance `json:"compliance,omitempty"`
}

// writeBotIndex records a bot created for req. Exercise bots are tracked
// in the exercise manifest instead. A failure is logged: the bot exists
// either way, and its history still links it to the request.
func (p *Plugin) writeBotIndex(req *botRequest, bot *model.Bot, approverID string) {
	if req.Exercise {
		return
	}
	entry := botIndexEntry{
		BotUserID: bot.UserId, RequestID: req.ID, Username: bot.Username, DisplayName: req.DisplayName,
		RequesterID: req.RequesterID, OwnerUserID: req.OwnerUserID,
		CreatedAt: model.GetMillis(), RequestedAt: req.CreatedAt, ApprovedBy: approverID,
		Purpose: req.Purpose, PrivilegeLevel: req.PrivilegeLevel, DataIngestion: req.DataIngestion,
		HostingEnv: req.HostingEnv, HomeTeamID: req.HomeTeamID, HomeChannelID: req.HomeChannelID,
		Compliance: req.Compliance,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		p.API.LogError("failed to marshal bot index entry", "bot_user_id", bot.UserId, "error", err.Error())
		return
	}
	if appErr := p.API.KVSet(kvBotIndexPrefix+bot.UserId, data); appErr != nil {
		p.API.LogError("failed to write bot index entry", "bot_user_id", bot.UserId, "request_id", req.ID, "error", appErr.Error())
	}
}

// loadBotIndex returns the index entry for a bot, or nil when Request Hub
// has none.
func (p *Plugin) loadBotIndex(botUserID string) (*botIndexEntry, error) {
	data, appErr := p.API.KVGet(kvBotIndexPrefix + botUserID)
	if appErr != nil {
		return nil, appErr
	}
	if data == nil {
		return nil, nil
	}
	var entry botIndexEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal bot index entry")
	}
	return &entry, nil
}

// listBotIndex returns every bot index entry. Unreadable entries are
// logged and skipped.
func (p *Plugin) listBotIndex() ([]botIndexEntry, error) {
	var out []botIndexEntry
	for page := 0; ; page++ {
		keys, appErr := p.API.KVList(page, botIndexPageSize)
		if appErr != nil {
			return nil, appErr
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, kvBotIndexPrefix) {
				continue
			}
			entry, err := p.loadBotIndex(strings.TrimPrefix(key, kvBotIndexPrefix))
			if err != nil {
				p.API.LogWarn("skipping unreadable bot index entry", "key", key, "error", err.Error())
				continue
			}
			if entry != nil {
				out = append(out, *entry)
			}
		}
		if len(keys) < botIndexPageSize {
			return out, nil
		}
	}
}
