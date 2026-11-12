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
// value (bot_token_expiry_* KV). A once-daily cluster job scans that index
// and DMs the requester: weekly from a month before expiry, daily in the
// last week, and once when expired. LastRemindedAt in the same KV record is
// the notification ledger so we do not re-spam between scans. If the
// requester's account is gone, System Admins get the reminders instead.

const (
	botTokenExpiryKeyPrefix = "bot_token_expiry_"   //nolint:gosec // G101: a KV key, not a credential
	tokenReminderJobKey     = "bot_token_reminders" //nolint:gosec // G101: a KV key, not a credential
	tokenReminderJobPeriod  = 24 * time.Hour

	tokenWeeklyWindow = 30 * 24 * time.Hour
	tokenDailyWindow  = 7 * 24 * time.Hour
	// tokenReminderSlack lets a reminder go out up to an hour early so a
	// daily job that drifts slightly still fires the due reminder same day,
	// without collapsing the weekly cadence into near-daily sends.
	tokenReminderSlack = time.Hour
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
	// RequestID is the bot request the token was issued for, for auditing.
	// Empty on tokens tracked before auditing.
	RequestID string `json:"request_id,omitempty"`
}

func botTokenExpiryKey(tokenID string) string {
	return botTokenExpiryKeyPrefix + tokenID
}

// trackBotTokenExpiry stores the details the reminders need. A failure is
// logged and otherwise ignored: the token is already issued.
func (p *Plugin) trackBotTokenExpiry(token *model.UserAccessToken, botUsername, requesterID, requestID string) {
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
		RequestID:      requestID,
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
	key := botTokenExpiryKey(rec.TokenID)
	if appErr := p.API.KVSet(key, data); appErr != nil {
		return appErr
	}
	p.trackTokenExpiryKey(key)
	return nil
}

// startTokenReminderJob schedules the reminder job. The cluster job runs it
// on one server at a time.
func (p *Plugin) startTokenReminderJob() {
	job, err := cluster.Schedule(p.API, tokenReminderJobKey, cluster.MakeWaitForInterval(tokenReminderJobPeriod), func() {
		defer p.logRecoveredPanic("bot token reminders")
		// Daily repair for rolling HA: old-version nodes may write
		// pending/token records without indexing them.
		p.rebuildOpenRequestIndex()
		p.rebuildTokenExpiryIndex()
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
// reminders that are due at now. Uses the token-expiry index so it does not
// scan histories/anchors/other plugin KV on every daily run.
func (p *Plugin) sendTokenExpiryReminders(now time.Time) {
	keys, err := p.listTokenExpiryKeys()
	if err != nil {
		p.API.LogError("failed to list bot token expiries", "error", err.Error())
		return
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
		p.endTokenTracking(key, rec, "its record could not be read")
		return
	}

	// A bot that was deleted or disabled has no token worth renewing.
	bot, appErr := p.API.GetBot(rec.BotUserID, true)
	if appErr != nil {
		if appErr.StatusCode == http.StatusNotFound {
			p.endTokenTracking(key, rec, "the bot no longer exists")
		} else {
			p.API.LogWarn("failed to load bot for token reminder", "bot_user_id", rec.BotUserID, "error", appErr.Error())
		}
		return
	}
	if bot == nil || bot.DeleteAt > 0 {
		p.endTokenTracking(key, rec, "the bot was deactivated")
		return
	}
	if bot.Username != "" {
		rec.BotUsername = bot.Username
	}

	nowMillis := now.UnixMilli()
	remaining := time.Duration(rec.ExpiresAt-nowMillis) * time.Millisecond
	if remaining <= 0 {
		// One last notice, then stop tracking. If KVDelete keeps failing, do
		// not re-DM every day — bump LastRemindedAt and retry the delete later.
		sinceLast := time.Duration(nowMillis-rec.LastRemindedAt) * time.Millisecond
		if rec.LastRemindedAt > 0 && sinceLast < 24*time.Hour-tokenReminderSlack {
			_ = p.endTokenTracking(key, rec, "the token expired")
			return
		}
		if p.sendTokenReminder(rec, tokenExpiredMessage(rec)) {
			p.auditTokenReminder(rec, true)
			if p.endTokenTracking(key, rec, "the token expired") {
				return
			}
			rec.LastRemindedAt = nowMillis
			if err := p.saveBotTokenExpiry(rec); err != nil {
				p.API.LogWarn("failed to persist token reminder after expired DM with stuck delete",
					"key", key, "error", err.Error())
			}
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
	p.auditTokenReminder(rec, false)
	rec.LastRemindedAt = nowMillis
	if err := p.saveBotTokenExpiry(rec); err != nil {
		p.API.LogWarn("failed to record bot token reminder", "bot_user_id", rec.BotUserID, "error", err.Error())
	}
}

// auditTokenReminder records a token expiry reminder that was sent.
func (p *Plugin) auditTokenReminder(rec botTokenExpiry, expired bool) {
	note := fmt.Sprintf("Reminded about the access token for bot @%s, which expires on %s", rec.BotUsername, formatDay(rec.ExpiresAt))
	if expired {
		note = fmt.Sprintf("Told that the access token for bot @%s has expired", rec.BotUsername)
	}
	p.recordEvent(auditEntry{
		Event: eventTokenReminder, Kind: pendingKindBot, RequestID: rec.RequestID, RequesterID: rec.RequesterID,
		Note:   note,
		Detail: map[string]any{"bot_user_id": rec.BotUserID, "token_id": rec.TokenID, "expires_at": rec.ExpiresAt, "expired": expired},
	})
}

// endTokenTracking stops the reminders for a token, recording why. rec is
// empty when its record couldn't be read; key still names the token. The
// audit event is written only after a successful delete so a failed delete
// does not claim tracking ended while the daily job would keep reminding.
// Returns whether tracking was actually ended (delete succeeded).
func (p *Plugin) endTokenTracking(key string, rec botTokenExpiry, reason string) bool {
	if !p.deleteBotTokenExpiry(key) {
		return false
	}
	tokenID := rec.TokenID
	if tokenID == "" {
		tokenID = strings.TrimPrefix(key, botTokenExpiryKeyPrefix)
	}
	bot := "bot @" + rec.BotUsername
	if rec.BotUsername == "" {
		bot = "token " + tokenID
	}
	p.recordEvent(auditEntry{
		Event: eventTokenTrackingEnded, Kind: pendingKindBot, RequestID: rec.RequestID, RequesterID: rec.RequesterID,
		Note:   fmt.Sprintf("Stopped the expiry reminders for the access token of %s: %s", bot, reason),
		Detail: map[string]any{"bot_user_id": rec.BotUserID, "token_id": tokenID, "reason": reason},
	})
	return true
}

func (p *Plugin) deleteBotTokenExpiry(key string) bool {
	if appErr := p.API.KVDelete(key); appErr != nil {
		p.API.LogWarn("failed to delete bot token expiry", "key", key, "error", appErr.Error())
		return false
	}
	p.untrackTokenExpiryKey(key)
	return true
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
