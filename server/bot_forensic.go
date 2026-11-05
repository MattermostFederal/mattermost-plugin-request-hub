package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// The bot forensic export traces one bot over a date range, for Security
// to answer "what did this bot do": what Request Hub knows of it, the bot
// record, where it is now, and the posts it made. It is JSON only and is
// capped so one export can't load the server or fill a DM.

const (
	forensicMaxDays  = 90
	forensicMaxPosts = 5000
	forensicMaxBytes = 20 << 20
	// forensicMaxScanned bounds how many posts are read looking for the
	// bot's, so a busy channel can't make the export run for minutes.
	forensicMaxScanned = 200000
	forensicPageSize   = 200
)

const forensicMaxWindow = forensicMaxDays * 24 * time.Hour

// forensicLimitations go in every forensic export, so a reader knows what
// it can't show.
var forensicLimitations = []string{
	"Only channels the bot is in now, its home channel and its webhook channels are scanned. Channels it has left are not.",
	"Direct and group messages are not scanned; they follow the server's compliance export.",
	"Deleted posts are not included.",
	"Plugins can't see the API calls made with the bot's tokens. Look for them in the server audit log or your SIEM.",
}

// forensicExport is the bot forensic export file.
type forensicExport struct {
	BotID       string `json:"bot_id"`
	GeneratedAt int64  `json:"generated_at"`
	Since       int64  `json:"since"`
	Until       int64  `json:"until"`
	// RHManaged is whether Request Hub created the bot and indexed it.
	RHManaged bool            `json:"rh_managed"`
	Index     *botIndexEntry  `json:"index,omitempty"`
	History   *requestHistory `json:"history,omitempty"`
	// Bot is nil when the bot is gone.
	Bot       *model.Bot        `json:"bot,omitempty"`
	BotStatus string            `json:"bot_status"`
	Teams     []forensicTeam    `json:"teams"`
	Scanned   []forensicChannel `json:"scanned_channels"`
	Posts     []forensicPost    `json:"posts"`
	Files     []forensicFile    `json:"files"`
	PostCount int               `json:"post_count"`
	Truncated bool              `json:"truncated"`
	// TruncatedReason says which cap cut the export short.
	TruncatedReason string   `json:"truncated_reason,omitempty"`
	Limitations     []string `json:"limitations"`
}

type forensicTeam struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name"`
	Channels    []forensicChannel `json:"channels"`
}

type forensicChannel struct {
	ID          string `json:"id"`
	TeamID      string `json:"team_id,omitempty"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Type        string `json:"type,omitempty"`
	Archived    bool   `json:"archived,omitempty"`
	// Why says why a channel was scanned: member, home or webhook.
	Why string `json:"why,omitempty"`
	// Missing is set when the channel couldn't be loaded.
	Missing bool `json:"missing,omitempty"`
}

type forensicPost struct {
	ID        string         `json:"id"`
	ChannelID string         `json:"channel_id"`
	RootID    string         `json:"root_id,omitempty"`
	CreateAt  int64          `json:"create_at"`
	UpdateAt  int64          `json:"update_at"`
	EditAt    int64          `json:"edit_at,omitempty"`
	Type      string         `json:"type,omitempty"`
	Message   string         `json:"message"`
	Props     map[string]any `json:"props,omitempty"`
	FileIDs   []string       `json:"file_ids,omitempty"`
}

type forensicFile struct {
	ID        string `json:"id"`
	PostID    string `json:"post_id"`
	Name      string `json:"name,omitempty"`
	Extension string `json:"extension,omitempty"`
	Size      int64  `json:"size,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	CreateAt  int64  `json:"create_at,omitempty"`
	Missing   bool   `json:"missing,omitempty"`
}

// buildForensicExport builds the bot forensic export f asks for.
func (p *Plugin) buildForensicExport(f auditExportFilter, filename string) (*auditExport, error) {
	fx, err := p.buildBotForensic(f.BotID, f.Since, f.Until, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(fx); err != nil {
		return nil, err
	}
	return &auditExport{
		data: buf.Bytes(), filename: filename + ".json", contentType: "application/json", count: fx.PostCount,
		detail: map[string]any{"truncated": fx.Truncated, "rh_managed": fx.RHManaged, "channels_scanned": len(fx.Scanned)},
	}, nil
}

// buildBotForensic traces botID between since and until (Unix millis).
func (p *Plugin) buildBotForensic(botID string, since, until, now int64) (*forensicExport, error) {
	if since <= 0 || until <= 0 || since > until {
		return nil, errors.New("a bot forensic export needs a start date before its end date")
	}
	if until-since > forensicMaxWindow.Milliseconds() {
		return nil, errors.Errorf("a bot forensic export covers at most %d days", forensicMaxDays)
	}
	fx := &forensicExport{
		BotID: botID, GeneratedAt: now, Since: since, Until: until,
		Teams: []forensicTeam{}, Scanned: []forensicChannel{}, Posts: []forensicPost{}, Files: []forensicFile{},
		Limitations: forensicLimitations,
	}

	index, err := p.loadBotIndex(botID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load the bot index")
	}
	fx.Index, fx.RHManaged = index, index != nil
	var req botRequest
	if index != nil && index.RequestID != "" {
		h, histErr := p.loadHistory(index.RequestID)
		if histErr != nil {
			return nil, errors.Wrap(histErr, "failed to load the bot's request history")
		}
		fx.History = h
		if h != nil && len(h.Request) > 0 {
			if err := json.Unmarshal(h.Request, &req); err != nil {
				p.API.LogWarn("failed to read the bot request snapshot for a forensic export", "request_id", index.RequestID, "error", err.Error())
			}
		}
	}

	bot, appErr := p.API.GetBot(botID, true)
	switch {
	case appErr == nil && bot != nil:
		fx.Bot, fx.BotStatus = bot, botStatusActive
		if bot.DeleteAt != 0 {
			fx.BotStatus = botStatusDeactivated
		}
	case isNotFound(appErr):
		if index == nil {
			return nil, errors.New("there is no bot with that ID")
		}
		fx.BotStatus = botStatusDeleted
	default:
		return nil, errors.Wrap(appErr, "failed to load the bot")
	}

	// Channels to scan, in order: where the bot is now, then its home and
	// webhook channels when it has left them.
	scan := map[string]bool{}
	addScan := func(c forensicChannel) {
		if c.ID == "" || scan[c.ID] {
			return
		}
		scan[c.ID] = true
		fx.Scanned = append(fx.Scanned, c)
	}
	if fx.BotStatus != botStatusDeleted {
		teams, appErr := p.API.GetTeamsForUser(botID)
		if appErr != nil {
			return nil, errors.Wrap(appErr, "failed to load the bot's teams")
		}
		sort.Slice(teams, func(i, j int) bool { return teams[i].Name < teams[j].Name })
		for _, team := range teams {
			channels, appErr := p.API.GetChannelsForTeamForUser(team.Id, botID, true)
			if appErr != nil {
				return nil, errors.Wrap(appErr, "failed to load the bot's channels")
			}
			sort.Slice(channels, func(i, j int) bool { return channels[i].Name < channels[j].Name })
			ft := forensicTeam{ID: team.Id, Name: team.Name, DisplayName: team.DisplayName, Channels: []forensicChannel{}}
			for _, ch := range channels {
				if ch.IsGroupOrDirect() {
					continue
				}
				fc := forensicChannelOf(ch, "member")
				ft.Channels = append(ft.Channels, fc)
				addScan(fc)
			}
			fx.Teams = append(fx.Teams, ft)
		}
	}
	homeID := req.HomeChannelID
	if index != nil && index.HomeChannelID != "" {
		homeID = index.HomeChannelID
	}
	for _, extra := range []struct{ id, why string }{
		{homeID, "home"}, {req.IncomingWebhookChannelID, "webhook"}, {req.OutgoingWebhookChannelID, "webhook"},
	} {
		if extra.id == "" || scan[extra.id] {
			continue
		}
		ch, appErr := p.API.GetChannel(extra.id)
		if appErr != nil || ch == nil {
			addScan(forensicChannel{ID: extra.id, Why: extra.why, Missing: true})
			continue
		}
		if ch.IsGroupOrDirect() {
			continue
		}
		addScan(forensicChannelOf(ch, extra.why))
	}

	if err := p.scanBotPosts(fx); err != nil {
		return nil, err
	}
	fx.PostCount = len(fx.Posts)
	return fx, nil
}

func forensicChannelOf(ch *model.Channel, why string) forensicChannel {
	return forensicChannel{
		ID: ch.Id, TeamID: ch.TeamId, Name: ch.Name, DisplayName: ch.DisplayName,
		Type: string(ch.Type), Archived: ch.DeleteAt != 0, Why: why,
	}
}

// scanBotPosts reads fx's channels newest first, keeping the bot's posts
// in the date range, until the caps are hit. Posts and files come out
// oldest first.
func (p *Plugin) scanBotPosts(fx *forensicExport) error {
	seen := map[string]bool{}
	size, scanned := 0, 0
	stop := func(reason string) {
		fx.Truncated, fx.TruncatedReason = true, reason
	}
scan:
	for _, c := range fx.Scanned {
		if c.Missing {
			continue
		}
		for page := 0; ; page++ {
			list, appErr := p.API.GetPostsForChannel(c.ID, page, forensicPageSize)
			if appErr != nil {
				return errors.Wrap(appErr, "failed to read posts in a channel the bot is in")
			}
			if list == nil || len(list.Order) == 0 {
				break
			}
			older := false
			for _, id := range list.Order {
				post := list.Posts[id]
				if post == nil {
					continue
				}
				if scanned++; scanned > forensicMaxScanned {
					stop("scanned " + strconv.Itoa(forensicMaxScanned) + " posts without reaching the start date")
					break scan
				}
				if post.CreateAt < fx.Since {
					older = true
					break
				}
				if post.CreateAt > fx.Until || post.UserId != fx.BotID || seen[post.Id] {
					continue
				}
				seen[post.Id] = true
				fp := forensicPost{
					ID: post.Id, ChannelID: post.ChannelId, RootID: post.RootId, CreateAt: post.CreateAt, UpdateAt: post.UpdateAt,
					EditAt: post.EditAt, Type: post.Type, Message: post.Message, Props: post.GetProps(), FileIDs: post.FileIds,
				}
				if len(fp.Props) == 0 {
					fp.Props = nil
				}
				raw, err := json.Marshal(fp)
				if err != nil {
					return errors.Wrap(err, "failed to marshal a post")
				}
				if size+len(raw) > forensicMaxBytes {
					stop("reached " + strconv.Itoa(forensicMaxBytes>>20) + " MB")
					break scan
				}
				size += len(raw)
				fx.Posts = append(fx.Posts, fp)
				if len(fx.Posts) >= forensicMaxPosts {
					stop("reached " + strconv.Itoa(forensicMaxPosts) + " posts")
					break scan
				}
			}
			if older || len(list.Order) < forensicPageSize {
				break
			}
		}
	}
	sort.SliceStable(fx.Posts, func(i, j int) bool { return fx.Posts[i].CreateAt < fx.Posts[j].CreateAt })
	for _, post := range fx.Posts {
		for _, fileID := range post.FileIDs {
			file := forensicFile{ID: fileID, PostID: post.ID}
			if info, appErr := p.API.GetFileInfo(fileID); appErr == nil && info != nil {
				file.Name, file.Extension, file.Size, file.MimeType, file.CreateAt = info.Name, info.Extension, info.Size, info.MimeType, info.CreateAt
			} else {
				file.Missing = true
			}
			fx.Files = append(fx.Files, file)
		}
	}
	return nil
}

// resolveExportBot turns the --bot flag or form field into a bot's user
// ID: "" stays "", and anything else must name a bot.
func (p *Plugin) resolveExportBot(name string) (string, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	if name == "" {
		return "", nil
	}
	user, appErr := p.API.GetUserByUsername(name)
	if appErr != nil || user == nil {
		return "", errors.Errorf("could not find a bot named @%s", name)
	}
	if !user.IsBot {
		return "", errors.Errorf("@%s is not a bot", name)
	}
	return user.Id, nil
}
