package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	docsBookmarkDisplayName = "Request Hub docs"
	docsBookmarkEmoji       = "books"

	// kvDocsBookmarksMigrated marks the one-time sweep that bookmarks the
	// docs in channels created before bookmarks were added. New channels are
	// bookmarked when they're created, so a bookmark an admin removes stays
	// removed.
	kvDocsBookmarksMigrated = "migration_docs_bookmarks_v1"
)

// docsPath is the server-relative path of the bundled guide.
func docsPath() string {
	return fmt.Sprintf("/plugins/%s/public/help/docs.html", manifest.Id)
}

// docsURL is the absolute URL of the bundled guide, or "" when SiteURL is
// not configured. Channel bookmarks need an absolute URL.
func (p *Plugin) docsURL() string {
	siteURL := strings.TrimRight(p.siteURL(), "/")
	if siteURL == "" {
		return ""
	}
	return siteURL + docsPath()
}

// bookmarkDocs adds a "Request Hub docs" link bookmark to ch, unless the
// channel already has a bookmark to the guide. The plugin API has no
// bookmark call, so the bot joins the channel if needed and creates the
// bookmark through REST with a short-lived session. Best-effort: failures
// are logged.
func (p *Plugin) bookmarkDocs(ch *model.Channel) {
	if ch == nil {
		return
	}
	if err := p.addDocsBookmark(ch); err != nil {
		p.API.LogWarn("failed to bookmark the docs in a request channel",
			"channel_id", ch.Id, "error", err.Error())
	}
}

func (p *Plugin) addDocsBookmark(ch *model.Channel) error {
	url := p.docsURL()
	if url == "" {
		return errors.New("SiteURL is not configured")
	}

	if _, appErr := p.API.GetChannelMember(ch.Id, p.botUserID); appErr != nil {
		if err := p.ensureTeamMember(ch.TeamId, p.botUserID); err != nil {
			return errors.Wrap(err, "add bot to team")
		}
		if _, appErr := p.API.AddChannelMember(ch.Id, p.botUserID); appErr != nil {
			return errors.Wrap(appErr, "add bot to channel")
		}
	}

	session, err := p.createUserSession(p.botUserID)
	if err != nil {
		return err
	}
	defer func() {
		if revErr := p.API.RevokeSession(session.Id); revErr != nil {
			p.API.LogWarn("failed to revoke bot session after bookmarking the docs", "error", revErr.Error())
		}
	}()

	client := model.NewAPIv4Client(p.internalBaseURL())
	client.SetToken(session.Token)
	ctx := context.Background()

	existing, _, err := client.ListChannelBookmarksForChannel(ctx, ch.Id, 0)
	if err != nil {
		return errors.Wrap(err, "list bookmarks")
	}
	for _, b := range existing {
		if b != nil && b.LinkUrl == url {
			return nil
		}
	}

	if _, _, err := client.CreateChannelBookmark(ctx, &model.ChannelBookmark{
		ChannelId:   ch.Id,
		DisplayName: docsBookmarkDisplayName,
		LinkUrl:     url,
		Emoji:       docsBookmarkEmoji,
		Type:        model.ChannelBookmarkLink,
	}); err != nil {
		return errors.Wrap(err, "create bookmark")
	}
	return nil
}

// bookmarkDocsInExistingChannels is a one-shot sweep that bookmarks the docs
// in every request channel that already exists. It waits for SiteURL, since
// the bookmark needs an absolute URL.
func (p *Plugin) bookmarkDocsInExistingChannels() {
	if migrated, _ := p.API.KVGet(kvDocsBookmarksMigrated); migrated != nil {
		return
	}
	if p.docsURL() == "" {
		p.API.LogInfo("docs bookmarks: SiteURL is not configured; will retry on the next activation")
		return
	}
	channels := p.requestChannels()
	for _, ch := range channels {
		p.bookmarkDocs(ch)
	}
	_ = p.API.KVSet(kvDocsBookmarksMigrated, []byte("1"))
	p.API.LogInfo("docs bookmarks: bookmarked the docs in existing request channels", "channels", len(channels))
}
