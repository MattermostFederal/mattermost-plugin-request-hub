package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/pluginapi/cluster"
)

// Bot token expiry reminders. When an approved bot request issues a token,
// the plugin keeps who asked for it and when it expires, never the token
// value. An hourly job reminds the requester weekly from a month before the
// expiry, daily in the last week, and once when it has expired. If the
// requester's account is gone, System Admins get the reminders instead.

const (
	botTokenExpiryKeyPrefix = "bot_token_expiry_"   //nolint:gosec // G101: a KV key, not a credential
	tokenReminderJobKey     = "bot_token_reminders" //nolint:gosec // G101: a KV key, not a credential
	tokenReminderJobPeriod  = time.Hour

	tokenWeeklyWindow = 30 * 24 * time.Hour
	tokenDailyWindow  = 7 * 24 * time.Hour
	// tokenReminderSlack lets a reminder go out up to one job run early, so
	// an hourly job doesn't push a daily reminder to the next day.
	tokenReminderSlack = tokenReminderJobPeriod
)

// botTokenExpiry is what the plugin keeps about a token it issued.
// ExpiresAt and LastRemindedAt are Unix millis.
type botTokenExpiry struct {
	TokenID        string `json:"token_id"`
	BotUserID      string `json:"bot_user_id"`
	BotUsername    string `json:"bot_username"`
	RequesterID    string `json:"requester_id"`
	ExpiresAt      int64  `json:"expires_at"`
	LastRemindedAt int64  `json:"last_reminded_at"`
}

func botTokenExpiryKey(tokenID string) string {
	return botTokenExpiryKeyPrefix + tokenID
}

// trackBotTokenExpiry stores the details the reminders need. A failure is
// logged and otherwise ignored: the token is already issued.
func (p *Plugin) trackBotTokenExpiry(token *model.UserAccessToken, botUsername, requesterID string) {
	if token == nil || token.Id == "" || token.ExpiresAt <= 0 {
		return
	}
	// LastRemindedAt starts at issue time, so a short-lived token doesn't
	// get a reminder right after the DM that carried it.
	rec := botTokenExpiry{
		TokenID:        token.Id,
		BotUserID:      token.UserId,
		BotUsername:    botUsername,
		RequesterID:    requesterID,
		ExpiresAt:      token.ExpiresAt,
		LastRemindedAt: model.GetMillis(),
	}
	if err := p.saveBotTokenExpiry(rec); err != nil {
		p.API.LogWarn("failed to store bot token expiry; no reminders will be sent", "bot_user_id", token.UserId, "error", err.Error())
	}
}

func (p *Plugin) saveBotTokenExpiry(rec botTokenExpiry) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if appErr := p.API.KVSet(botTokenExpiryKey(rec.TokenID), data); appErr != nil {
		return appErr
	}
	return nil
}

// startTokenReminderJob schedules the reminder job. The cluster job runs it
// on one server at a time.
func (p *Plugin) startTokenReminderJob() {
	job, err := cluster.Schedule(p.API, tokenReminderJobKey, cluster.MakeWaitForInterval(tokenReminderJobPeriod), func() {
		p.sendTokenExpiryReminders(time.Now())
	})
	if err != nil {
		p.API.LogError("failed to schedule bot token reminders", "error", err.Error())
		return
	}
	p.tokenReminderJob = job
}

func (p *Plugin) stopTokenReminderJob() {
	if p.tokenReminderJob == nil {
		return
	}
	if err := p.tokenReminderJob.Close(); err != nil {
		p.API.LogWarn("failed to stop bot token reminders", "error", err.Error())
	}
	p.tokenReminderJob = nil
}

// sendTokenExpiryReminders goes through every tracked token and sends the
// reminders that are due at now.
func (p *Plugin) sendTokenExpiryReminders(now time.Time) {
	const perPage = 200
	var keys []string
	for page := 0; ; page++ {
		batch, appErr := p.API.KVList(page, perPage)
		if appErr != nil {
			p.API.LogError("failed to list bot token expiries", "error", appErr.Error())
			return
		}
		for _, key := range batch {
			if strings.HasPrefix(key, botTokenExpiryKeyPrefix) {
				keys = append(keys, key)
			}
		}
		if len(batch) < perPage {
			break
		}
	}
	for _, key := range keys {
		p.remindTokenExpiry(key, now)
	}
}

func (p *Plugin) remindTokenExpiry(key string, now time.Time) {
	raw, appErr := p.API.KVGet(key)
	if appErr != nil || raw == nil {
		return
	}
	var rec botTokenExpiry
	if err := json.Unmarshal(raw, &rec); err != nil {
		p.API.LogWarn("dropping unreadable bot token expiry", "key", key, "error", err.Error())
		p.deleteBotTokenExpiry(key)
		return
	}

	// A bot that was deleted or disabled has no token worth renewing.
	bot, appErr := p.API.GetBot(rec.BotUserID, true)
	if appErr != nil {
		if appErr.StatusCode == http.StatusNotFound {
			p.deleteBotTokenExpiry(key)
		} else {
			p.API.LogWarn("failed to load bot for token reminder", "bot_user_id", rec.BotUserID, "error", appErr.Error())
		}
		return
	}
	if bot.DeleteAt > 0 {
		p.deleteBotTokenExpiry(key)
		return
	}
	if bot.Username != "" {
		rec.BotUsername = bot.Username
	}

	nowMillis := now.UnixMilli()
	remaining := time.Duration(rec.ExpiresAt-nowMillis) * time.Millisecond
	if remaining <= 0 {
		// One last notice, then stop tracking.
		if p.sendTokenReminder(rec, tokenExpiredMessage(rec)) {
			p.deleteBotTokenExpiry(key)
		}
		return
	}

	var every time.Duration
	switch {
	case remaining <= tokenDailyWindow:
		every = 24 * time.Hour
	case remaining <= tokenWeeklyWindow:
		every = 7 * 24 * time.Hour
	default:
		return
	}
	sinceLast := time.Duration(nowMillis-rec.LastRemindedAt) * time.Millisecond
	if sinceLast < every-tokenReminderSlack {
		return
	}
	if !p.sendTokenReminder(rec, tokenExpiringMessage(rec, remaining)) {
		return
	}
	rec.LastRemindedAt = nowMillis
	if err := p.saveBotTokenExpiry(rec); err != nil {
		p.API.LogWarn("failed to record bot token reminder", "bot_user_id", rec.BotUserID, "error", err.Error())
	}
}

func (p *Plugin) deleteBotTokenExpiry(key string) {
	if appErr := p.API.KVDelete(key); appErr != nil {
		p.API.LogWarn("failed to delete bot token expiry", "key", key, "error", appErr.Error())
	}
}

// sendTokenReminder DMs the requester, or every active System Admin when
// the requester's account is deactivated or deleted. It reports whether
// anyone got the message.
func (p *Plugin) sendTokenReminder(rec botTokenExpiry, message string) bool {
	requester, appErr := p.API.GetUser(rec.RequesterID)
	if appErr == nil && requester.DeleteAt == 0 {
		return p.sendTokenReminderDM(requester.Id, message)
	}

	admins, appErr := p.API.GetUsers(&model.UserGetOptions{Role: model.SystemAdminRoleId, Active: true, Page: 0, PerPage: 100})
	if appErr != nil {
		p.API.LogWarn("failed to list System Admins for bot token reminder", "error", appErr.Error())
		return false
	}
	message += "\n\nYou're getting this because " + requesterUsername(requester, rec.RequesterID) +
		", who asked for the bot, no longer has an active account."
	sent := false
	for _, admin := range admins {
		if p.sendTokenReminderDM(admin.Id, message) {
			sent = true
		}
	}
	return sent
}

func (p *Plugin) sendTokenReminderDM(userID, message string) bool {
	channel, appErr := p.API.GetDirectChannel(userID, p.botUserID)
	if appErr != nil {
		p.API.LogWarn("failed to open DM for bot token reminder", "user_id", userID, "error", appErr.Error())
		return false
	}
	if _, appErr := p.API.CreatePost(&model.Post{UserId: p.botUserID, ChannelId: channel.Id, Message: message}); appErr != nil {
		p.API.LogWarn("failed to send bot token reminder", "user_id", userID, "error", appErr.Error())
		return false
	}
	return true
}

func tokenExpiryDate(millis int64) string {
	return time.UnixMilli(millis).UTC().Format("January 2, 2006") + " (UTC)"
}

func tokenRenewalHint(botUsername string) string {
	return fmt.Sprintf("To get a new token, ask a System Admin to create one for @%s under **Integrations > Bot Accounts**, or request a new bot with `/%s bot`.",
		botUsername, commandTrigger)
}

func tokenExpiringMessage(rec botTokenExpiry, remaining time.Duration) string {
	days := int((remaining + 24*time.Hour - 1) / (24 * time.Hour))
	in := fmt.Sprintf("in %d days", days)
	if days == 1 {
		in = "within a day"
	}
	return fmt.Sprintf("⏳ The access token for bot **@%s** expires on **%s**, %s. Anything using it stops working then. %s",
		rec.BotUsername, tokenExpiryDate(rec.ExpiresAt), in, tokenRenewalHint(rec.BotUsername))
}

func tokenExpiredMessage(rec botTokenExpiry) string {
	return fmt.Sprintf("⌛ The access token for bot **@%s** expired on **%s**. %s",
		rec.BotUsername, tokenExpiryDate(rec.ExpiresAt), tokenRenewalHint(rec.BotUsername))
}
