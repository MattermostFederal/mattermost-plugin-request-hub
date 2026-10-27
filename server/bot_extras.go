package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const tokenPostRegistryKey = "token_post_registry"

type tokenPostEntry struct {
	PostID    string `json:"post_id"`
	ExpiresAt int64  `json:"expires_at"` // Unix timestamp
}

// createBotToken creates a personal access token for botUserID using the
// approving admin's session token. The token value is returned to the caller
// and never stored — it exists only in the approval DM, which auto-deletes.
func (p *Plugin) createBotToken(authToken, botUserID, description string) (string, error) {
	internalURL := p.internalBaseURL()
	if internalURL == "" {
		return "", fmt.Errorf("SiteURL not configured")
	}
	client := model.NewAPIv4Client(internalURL)
	client.SetToken(authToken)
	token, _, err := client.CreateUserAccessToken(context.Background(), botUserID, description, 0)
	if err != nil {
		return "", fmt.Errorf("create token: %w", err)
	}
	return token.Token, nil
}

// addBotToWebhookChannel puts the new bot on the channel's team and in the
// channel. An incoming webhook's owner has to be able to read its channel,
// and a new bot starts on no team.
func (p *Plugin) addBotToWebhookChannel(botUserID, channelID string) error {
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil {
		return fmt.Errorf("load channel: %s", appErr.Error())
	}
	if err := p.ensureTeamMember(channel.TeamId, botUserID); err != nil {
		return fmt.Errorf("add bot to team: %w", err)
	}
	if _, appErr := p.API.AddChannelMember(channel.Id, botUserID); appErr != nil {
		return fmt.Errorf("add bot to channel: %s", appErr.Error())
	}
	return nil
}

// createIncomingWebhookForBot creates an incoming webhook for the bot using the
// approving admin's session token. The hook posts as the bot and only to its
// channel. Returns the full webhook URL on success.
func (p *Plugin) createIncomingWebhookForBot(authToken, botUserID string, req *botRequest) (string, error) {
	client := model.NewAPIv4Client(p.internalBaseURL())
	client.SetToken(authToken)

	name := req.IncomingWebhookDisplayName
	if name == "" {
		name = fmt.Sprintf("Incoming webhook for @%s", req.Username)
	}

	// Resolve the channel's team ID — required by the incoming webhook API.
	channel, appErr := p.API.GetChannel(req.IncomingWebhookChannelID)
	if appErr != nil {
		return "", fmt.Errorf("load channel: %s", appErr.Error())
	}

	hook, _, err := client.CreateIncomingWebhook(context.Background(), &model.IncomingWebhook{
		ChannelId:     req.IncomingWebhookChannelID,
		TeamId:        channel.TeamId,
		UserId:        botUserID,
		ChannelLocked: true,
		DisplayName:   name,
		Description:   fmt.Sprintf("Incoming webhook for bot @%s", req.Username),
	})
	if err != nil {
		return "", fmt.Errorf("create incoming webhook: %w", err)
	}
	// Use the public SiteURL for the hook URL — this is what the user will paste
	// into their service, so it must be the externally reachable address.
	return fmt.Sprintf("%s/hooks/%s", p.siteURL(), hook.Id), nil
}

// createOutgoingWebhookForBot creates an outgoing webhook owned by the bot using
// the approving admin's session token.
func (p *Plugin) createOutgoingWebhookForBot(authToken, botUserID string, req *botRequest) error {
	client := model.NewAPIv4Client(p.internalBaseURL())
	client.SetToken(authToken)

	name := req.OutgoingWebhookDisplayName
	if name == "" {
		name = fmt.Sprintf("Outgoing webhook for @%s", req.Username)
	}

	channel, appErr := p.API.GetChannel(req.OutgoingWebhookChannelID)
	if appErr != nil {
		return fmt.Errorf("load channel: %s", appErr.Error())
	}

	_, _, err := client.CreateOutgoingWebhook(context.Background(), &model.OutgoingWebhook{
		TeamId:       channel.TeamId,
		ChannelId:    req.OutgoingWebhookChannelID,
		CreatorId:    botUserID,
		DisplayName:  name,
		Description:  fmt.Sprintf("Outgoing webhook for bot @%s", req.Username),
		CallbackURLs: []string{req.OutgoingWebhookCallbackURL},
		ContentType:  "application/json",
	})
	if err != nil {
		return fmt.Errorf("create outgoing webhook: %w", err)
	}
	return nil
}

// sendBotExtrasReply posts a thread reply with the token, webhook URLs, and any
// partial errors that came out of bot approval. It only posts if there is
// something to show — if all fields are empty it is a no-op.
// The approval confirmation itself is handled by notifyRequesterInThreadCard.
// If the reply contains a token it is scheduled for auto-deletion after 72 hours.
func (p *Plugin) sendBotExtrasReply(req *botRequest, tokenValue, incomingWebhookURL string, outgoingWebhookCreated bool, partialErrors []string) {
	var lines []string

	if tokenValue != "" {
		lines = append(lines,
			"🔑 **Access Token**",
			fmt.Sprintf("`%s`", tokenValue),
			"⚠️ **Save this now.** This message auto-deletes in ~72 hours and the token cannot be retrieved again.",
		)
	}
	if incomingWebhookURL != "" {
		lines = append(lines, fmt.Sprintf("🔗 **Incoming webhook URL:** `%s`", incomingWebhookURL))
	}
	if outgoingWebhookCreated {
		name := escapeMarkdown(req.OutgoingWebhookDisplayName)
		if name == "" {
			name = "outgoing webhook"
		}
		lines = append(lines, fmt.Sprintf("📤 **Outgoing webhook** *%s* is now active.", name))
	}
	for _, e := range partialErrors {
		lines = append(lines, fmt.Sprintf("⚠️ %s", e))
	}

	if len(lines) == 0 {
		return
	}

	post, appErr := p.API.CreatePost(&model.Post{
		UserId:    p.botUserID,
		ChannelId: req.DMChannelID,
		RootId:    req.DMRootPostID,
		Message:   strings.Join(lines, "\n"),
	})
	if appErr != nil {
		p.API.LogError("failed to send bot extras reply", "user_id", req.RequesterID, "error", appErr.Error())
		return
	}

	// Incoming webhook URLs are Mattermost-generated credentials and get the
	// same 72-hour auto-delete treatment as access tokens.
	if tokenValue != "" || incomingWebhookURL != "" {
		p.scheduleTokenPostDeletion(post.Id)
	}
}

// scheduleTokenPostDeletion persists the post to a KV registry and fires a
// best-effort in-process timer to delete it 72 hours from now. The registry
// survives plugin restarts — cleanupExpiredTokenPosts re-arms timers on
// OnActivate.
func (p *Plugin) scheduleTokenPostDeletion(postID string) {
	expiresAt := time.Now().Add(72 * time.Hour).Unix()
	entry := tokenPostEntry{PostID: postID, ExpiresAt: expiresAt}

	for range 3 {
		raw, _ := p.API.KVGet(tokenPostRegistryKey)
		var entries []tokenPostEntry
		if raw != nil {
			_ = json.Unmarshal(raw, &entries)
		}
		entries = append(entries, entry)
		data, err := json.Marshal(entries)
		if err != nil {
			break
		}
		ok, appErr := p.API.KVCompareAndSet(tokenPostRegistryKey, raw, data)
		if appErr != nil {
			continue
		}
		if ok {
			break
		}
	}

	time.AfterFunc(72*time.Hour, func() {
		if appErr := p.API.DeletePost(postID); appErr != nil {
			p.API.LogWarn("failed to delete expired token post", "post_id", postID, "error", appErr.Error())
		}
		p.removeTokenPostFromRegistry(postID)
	})
}

// cleanupExpiredTokenPosts deletes any token DM posts whose 72-hour window
// elapsed while the plugin was not running. Called from OnActivate.
func (p *Plugin) cleanupExpiredTokenPosts() {
	armed := map[string]bool{}
	for range 3 {
		raw, appErr := p.API.KVGet(tokenPostRegistryKey)
		if appErr != nil || raw == nil {
			return
		}
		var entries []tokenPostEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return
		}

		now := time.Now().Unix()
		var remaining []tokenPostEntry
		for _, e := range entries {
			if e.ExpiresAt <= now {
				if delErr := p.API.DeletePost(e.PostID); delErr != nil {
					p.API.LogWarn("cleanup: failed to delete expired token post",
						"post_id", e.PostID, "error", delErr.Error())
				}
				continue
			}
			remaining = append(remaining, e)
			if armed[e.PostID] {
				continue
			}
			armed[e.PostID] = true
			delay := time.Duration(e.ExpiresAt-now) * time.Second
			postID := e.PostID
			time.AfterFunc(delay, func() {
				if delErr := p.API.DeletePost(postID); delErr != nil {
					p.API.LogWarn("failed to delete expired token post", "post_id", postID, "error", delErr.Error())
				}
				p.removeTokenPostFromRegistry(postID)
			})
		}

		data, err := json.Marshal(remaining)
		if err != nil {
			return
		}
		ok, casErr := p.API.KVCompareAndSet(tokenPostRegistryKey, raw, data)
		if casErr != nil {
			continue
		}
		if ok {
			return
		}
	}
}

func (p *Plugin) removeTokenPostFromRegistry(postID string) {
	for range 3 {
		raw, _ := p.API.KVGet(tokenPostRegistryKey)
		var entries []tokenPostEntry
		if raw != nil {
			_ = json.Unmarshal(raw, &entries)
		}
		filtered := make([]tokenPostEntry, 0, len(entries))
		for _, e := range entries {
			if e.PostID != postID {
				filtered = append(filtered, e)
			}
		}
		data, err := json.Marshal(filtered)
		if err != nil {
			return
		}
		ok, appErr := p.API.KVCompareAndSet(tokenPostRegistryKey, raw, data)
		if appErr != nil {
			continue
		}
		if ok {
			return
		}
	}
}
