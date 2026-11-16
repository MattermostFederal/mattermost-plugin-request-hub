package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
)

// prefixDefaults are the channel settings an admin attaches to one channel
// name prefix (the ChannelPrefixDefaults setting). They are applied to every
// channel requested with that prefix.
type prefixDefaults struct {
	// ChannelType forces the channel's visibility: "O" or "P". Blank lets
	// the requester choose.
	ChannelType string `json:"channel_type,omitempty"`
	// Purpose fills the channel purpose when the requester leaves it blank.
	Purpose string `json:"purpose,omitempty"`
	// Header is set on the channel when it's created.
	Header string `json:"header,omitempty"`
	// Members are usernames added to the channel when it's created, if they
	// are active members of the channel's team.
	Members []string `json:"members,omitempty"`
}

// isEmpty reports whether d changes nothing.
func (d prefixDefaults) isEmpty() bool {
	return d.ChannelType == "" && d.Purpose == "" && d.Header == "" && len(d.Members) == 0
}

// parsePrefixDefaults parses the ChannelPrefixDefaults setting: a JSON object
// keyed by prefix. A setting that isn't valid JSON is logged and ignored, so
// requests keep working without defaults. Each entry is trimmed and kept
// within Mattermost's limits; anything out of range is logged and dropped.
func parsePrefixDefaults(raw string, prefixes []channelPrefix, logError func(msg string, keyValuePairs ...any)) map[string]prefixDefaults {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var entries map[string]prefixDefaults
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		logError("Channel defaults per prefix is not valid JSON, so no defaults are applied", "error", err.Error())
		return nil
	}
	known := make(map[string]bool, len(prefixes))
	for _, entry := range prefixes {
		known[entry.Prefix] = true
	}

	out := make(map[string]prefixDefaults, len(entries))
	for prefix, d := range entries {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		if !known[prefix] {
			logError("Channel defaults are set for a prefix that is not in the prefix list; they apply once the prefix is added", "prefix", prefix)
		}
		d.ChannelType = strings.ToUpper(strings.TrimSpace(d.ChannelType))
		if d.ChannelType != "" && d.ChannelType != channelTypeOpen && d.ChannelType != channelTypePrivate {
			logError("Channel defaults: unknown channel type ignored", "prefix", prefix, "channel_type", d.ChannelType)
			d.ChannelType = ""
		}
		d.Purpose = strings.TrimSpace(d.Purpose)
		if utf8.RuneCountInString(d.Purpose) > maxPurposeLen {
			logError("Channel defaults: purpose is too long and is ignored", "prefix", prefix, "max", maxPurposeLen)
			d.Purpose = ""
		}
		d.Header = strings.TrimSpace(d.Header)
		if utf8.RuneCountInString(d.Header) > model.ChannelHeaderMaxRunes {
			logError("Channel defaults: header is too long and is ignored", "prefix", prefix, "max", model.ChannelHeaderMaxRunes)
			d.Header = ""
		}
		d.Members = normalizeUsernames(d.Members)
		if len(d.Members) > maxMembersPerList {
			logError("Channel defaults: too many default members; extra names are ignored", "prefix", prefix, "max", maxMembersPerList)
			d.Members = d.Members[:maxMembersPerList]
		}
		if !d.isEmpty() {
			out[prefix] = d
		}
	}
	return out
}

// normalizeUsernames trims names, drops a leading @, lowercases them and
// removes blanks and repeats, keeping order.
func normalizeUsernames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchPrefix returns the longest configured prefix name starts with, or
// nil.
func (c *configuration) matchPrefix(name string) *channelPrefix {
	var entry *channelPrefix
	for i := range c.prefixes {
		if strings.HasPrefix(name, c.prefixes[i].Prefix) && (entry == nil || len(c.prefixes[i].Prefix) > len(entry.Prefix)) {
			entry = &c.prefixes[i]
		}
	}
	return entry
}

// defaultsForChannelName returns the defaults of the prefix a channel name
// uses. It matches on the name, so requests stored before the defaults were
// set get them too.
func (c *configuration) defaultsForChannelName(name string) (prefixDefaults, bool) {
	entry := c.matchPrefix(name)
	if entry == nil {
		return prefixDefaults{}, false
	}
	return c.defaultsForPrefix(entry.Prefix)
}

// defaultsForPrefix returns the defaults set for prefix.
func (c *configuration) defaultsForPrefix(prefix string) (prefixDefaults, bool) {
	d, ok := c.prefixDefaults[prefix]
	return d, ok
}

// applyPrefixDefaultsAtSubmit sets what the approver must see on the card:
// a forced visibility, and the default purpose when the requester gave none.
func (c *configuration) applyPrefixDefaultsAtSubmit(req *channelRequest) {
	d, ok := c.defaultsForChannelName(req.Name)
	if !ok {
		return
	}
	if d.ChannelType != "" {
		req.ChannelType = d.ChannelType
	}
	if req.Purpose == "" && d.Purpose != "" {
		req.Purpose = d.Purpose
	}
}

// prefixDefaultsSummary describes the defaults for the approval card, or ""
// when the channel's prefix has none that change what Approve does.
func prefixDefaultsSummary(d prefixDefaults) string {
	var parts []string
	if d.Header != "" {
		parts = append(parts, "sets the channel header")
	}
	if len(d.Members) > 0 {
		names := make([]string, len(d.Members))
		for i, name := range d.Members {
			names[i] = "@" + name
		}
		parts = append(parts, "adds "+strings.Join(names, ", ")+" if they're on the team")
	}
	if len(parts) == 0 {
		return ""
	}
	return "Approve also " + strings.Join(parts, " and ") + "."
}

// skippedDefaultMember is a default member who couldn't be added, with why.
type skippedDefaultMember struct {
	Username string
	Reason   string
}

// resolveDefaultMembers turns the default member usernames into user IDs of
// active users on teamID. Anyone else is returned in skipped. Looked up at
// approval time, since the setting may have changed since the request.
func (p *Plugin) resolveDefaultMembers(usernames []string, teamID string) (ids []string, skipped []skippedDefaultMember) {
	for _, name := range usernames {
		user, appErr := p.API.GetUserByUsername(name)
		if appErr != nil || user == nil {
			skipped = append(skipped, skippedDefaultMember{name, "no such user"})
			continue
		}
		if user.DeleteAt != 0 {
			skipped = append(skipped, skippedDefaultMember{name, "deactivated"})
			continue
		}
		member, appErr := p.API.GetTeamMember(teamID, user.Id)
		if appErr != nil || member == nil || member.DeleteAt != 0 {
			skipped = append(skipped, skippedDefaultMember{name, "not on this team"})
			continue
		}
		ids = append(ids, user.Id)
	}
	return ids, skipped
}

// skippedDefaultMembersLine renders skipped default members for the welcome
// message, sorted for a stable order.
func skippedDefaultMembersLine(skipped []skippedDefaultMember) string {
	if len(skipped) == 0 {
		return ""
	}
	sorted := append([]skippedDefaultMember(nil), skipped...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Username < sorted[j].Username })
	parts := make([]string, len(sorted))
	for i, s := range sorted {
		parts[i] = fmt.Sprintf("@%s (%s)", s.Username, s.Reason)
	}
	return "Default members not added: " + strings.Join(parts, ", ")
}

// visibilityWord names a channel type for messages: "public" or "private".
func visibilityWord(channelType string) string {
	if channelType == channelTypePrivate {
		return "private"
	}
	return "public"
}
