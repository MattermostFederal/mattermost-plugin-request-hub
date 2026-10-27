package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// relayAnchor is a thread anchor linking approval post "ap" in channel
// "appch" to DM root "dmroot" in DM channel "dm".
var relayAnchor = threadAnchor{
	ApprovalPostID:    "ap",
	ApprovalChannelID: "appch",
	DMRootPostID:      "dmroot",
	DMChannelID:       "dm",
	Kind:              requestTypeTeam,
}

// --- storeTicketLookups ---

func TestStoreTicketLookups_WriteFailuresAreLogged(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	defer api.AssertExpectations(t)
	data := mustMarshal(t, relayAnchor)
	api.On("KVSet", kvApprovalPostPrefix+"ap", data).Return(testAppErr("down"))
	api.On("KVSet", kvDMRootPrefix+"dmroot", data).Return(testAppErr("down"))

	p.storeTicketLookups(relayAnchor)

	api.AssertCalled(t, "LogWarn", "failed to store approval-post lookup", "post_id", "ap", "error", mock.Anything)
	api.AssertCalled(t, "LogWarn", "failed to store DM-root lookup", "post_id", "dmroot", "error", mock.Anything)
}

func TestStoreTicketLookups_SkipsBlankPostIDs(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})

	p.storeTicketLookups(threadAnchor{})

	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

// --- MessageHasBeenPosted ---

func TestMessageHasBeenPosted_IgnoresBotPosts(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})

	p.MessageHasBeenPosted(&plugin.Context{}, &model.Post{UserId: "bot-user-id", ChannelId: "c1"})

	api.AssertNotCalled(t, "GetChannel", mock.Anything)
}

func TestMessageHasBeenPosted_ChannelLookupFailureStops(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetChannel", "c1").Return(nil, testAppErr("gone"))

	p.MessageHasBeenPosted(&plugin.Context{}, &model.Post{UserId: "u1", ChannelId: "c1", RootId: "r1"})

	api.AssertNotCalled(t, "KVGet", mock.Anything)
}

func TestMessageHasBeenPosted_DirectMessages(t *testing.T) {
	dm := &model.Channel{Id: "dm", Type: model.ChannelTypeDirect}

	t.Run("DM without the bot is ignored", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("GetChannel", "dm").Return(dm, nil)
		api.On("GetChannelMember", "dm", "bot-user-id").Return(nil, testAppErr("not a member"))

		p.MessageHasBeenPosted(&plugin.Context{}, &model.Post{UserId: "u1", ChannelId: "dm"})

		api.AssertNotCalled(t, "CreatePost", mock.Anything)
	})

	t.Run("root DM gets a nudge", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		defer api.AssertExpectations(t)
		api.On("GetChannel", "dm").Return(dm, nil)
		api.On("GetChannelMember", "dm", "bot-user-id").Return(&model.ChannelMember{}, nil)
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return post.ChannelId == "dm" && post.UserId == "bot-user-id" && post.RootId == "" &&
				post.Message == "Please reply to a specific ticket thread above to add information to that request."
		})).Return(&model.Post{}, nil)

		p.MessageHasBeenPosted(&plugin.Context{}, &model.Post{UserId: "u1", ChannelId: "dm", Message: "hi"})
	})

	t.Run("reply outside a ticket thread is ignored", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("GetChannel", "dm").Return(dm, nil)
		api.On("GetChannelMember", "dm", "bot-user-id").Return(&model.ChannelMember{}, nil)
		api.On("KVGet", kvDMRootPrefix+"other").Return(nil, nil)

		p.MessageHasBeenPosted(&plugin.Context{}, &model.Post{UserId: "u1", ChannelId: "dm", RootId: "other"})

		api.AssertNotCalled(t, "CreatePost", mock.Anything)
	})

	t.Run("reply in a ticket thread is forwarded to the approval thread", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		defer api.AssertExpectations(t)
		api.On("GetChannel", "dm").Return(dm, nil)
		api.On("GetChannelMember", "dm", "bot-user-id").Return(&model.ChannelMember{}, nil)
		api.On("KVGet", kvDMRootPrefix+"dmroot").Return(mustMarshal(t, relayAnchor), nil)
		api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "req"}, nil)
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return post.ChannelId == "appch" && post.RootId == "ap" &&
				post.Message == "**Reply from @req:**\n> more detail"
		})).Return(&model.Post{}, nil)

		p.MessageHasBeenPosted(&plugin.Context{}, &model.Post{
			UserId: "u1", ChannelId: "dm", RootId: "dmroot", Message: "more detail",
		})
	})
}

func TestMessageHasBeenPosted_ChannelPosts(t *testing.T) {
	appch := &model.Channel{Id: "appch", Type: model.ChannelTypePrivate}
	reply := func() *model.Post {
		return &model.Post{UserId: "sa1", ChannelId: "appch", RootId: "ap", Message: "which team?"}
	}

	t.Run("root post is ignored", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("GetChannel", "appch").Return(appch, nil)

		p.MessageHasBeenPosted(&plugin.Context{}, &model.Post{UserId: "sa1", ChannelId: "appch"})

		api.AssertNotCalled(t, "KVGet", mock.Anything)
	})

	t.Run("reply under an unknown post is ignored", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("GetChannel", "appch").Return(appch, nil)
		api.On("KVGet", kvApprovalPostPrefix+"ap").Return(nil, testAppErr("down"))

		p.MessageHasBeenPosted(&plugin.Context{}, reply())

		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("unreadable anchor is ignored", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("GetChannel", "appch").Return(appch, nil)
		api.On("KVGet", kvApprovalPostPrefix+"ap").Return([]byte("{"), nil)

		p.MessageHasBeenPosted(&plugin.Context{}, reply())

		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("anchor without a DM thread is ignored", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("GetChannel", "appch").Return(appch, nil)
		api.On("KVGet", kvApprovalPostPrefix+"ap").Return(mustMarshal(t, threadAnchor{ApprovalPostID: "ap"}), nil)

		p.MessageHasBeenPosted(&plugin.Context{}, reply())

		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("poster lookup failure is ignored", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("GetChannel", "appch").Return(appch, nil)
		api.On("KVGet", kvApprovalPostPrefix+"ap").Return(mustMarshal(t, relayAnchor), nil)
		api.On("GetUser", "sa1").Return(nil, testAppErr("gone"))

		p.MessageHasBeenPosted(&plugin.Context{}, reply())

		api.AssertNotCalled(t, "CreatePost", mock.Anything)
	})

	t.Run("reply from a non-reviewer stays local", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("GetChannel", "appch").Return(appch, nil)
		api.On("KVGet", kvApprovalPostPrefix+"ap").Return(mustMarshal(t, relayAnchor), nil)
		api.On("GetUser", "sa1").Return(&model.User{Id: "sa1", Username: "plain", Roles: "system_user"}, nil)

		p.MessageHasBeenPosted(&plugin.Context{}, reply())

		api.AssertNotCalled(t, "CreatePost", mock.Anything)
	})

	t.Run("reviewer reply is forwarded to the requester DM", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		defer api.AssertExpectations(t)
		api.On("GetChannel", "appch").Return(appch, nil)
		api.On("KVGet", kvApprovalPostPrefix+"ap").Return(mustMarshal(t, relayAnchor), nil)
		api.On("GetUser", "sa1").Return(&model.User{Id: "sa1", Username: "admin", Roles: "system_user system_admin"}, nil)
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return post.ChannelId == "dm" && post.RootId == "dmroot" && post.UserId == "bot-user-id" &&
				post.Message == "**Message from @admin:** which team?"
		})).Return(&model.Post{}, nil)

		p.MessageHasBeenPosted(&plugin.Context{}, reply())
	})
}

// --- canRelayApprovalThreadReply / canRelayTwoStepApprover ---

func TestCanRelayApprovalThreadReply_ChannelTicketWithTeamUsesTeamAdmins(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetTeamMember", "team1", "ta1").Return(&model.TeamMember{TeamId: "team1", UserId: "ta1", SchemeAdmin: true}, nil)

	user := &model.User{Id: "ta1", Roles: "system_user"}
	require.True(t, p.canRelayApprovalThreadReply(user, threadAnchor{Kind: requestTypeChannel, ScopeID: "team1"}))
}

func TestCanRelayApprovalThreadReply_ChannelAdminTicketUsesTargetChannelTeam(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", TeamId: "team1"}, nil)
	api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{TeamId: "team1", UserId: "u1", Roles: "team_user team_admin"}, nil)

	user := &model.User{Id: "u1", Roles: "system_user"}
	require.True(t, p.canRelayApprovalThreadReply(user, threadAnchor{Kind: requestTypeChannelAdmin, ScopeID: "c1"}))
}

func TestCanRelayApprovalThreadReply_LegacyAnchor(t *testing.T) {
	t.Run("system admin", func(t *testing.T) {
		p, _ := logOnlyPlugin(&configuration{})
		user := &model.User{Id: "sa1", Roles: "system_user system_admin"}
		require.True(t, p.canRelayApprovalThreadReply(user, threadAnchor{}))
	})

	t.Run("security group member", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeUserGroup, SecurityApproverGroup: "sec"})
		stubSyncedGroup(api, "sec", "g-sec")
		api.On("GetGroupsForUser", "u2").Return([]*model.Group{{Id: "g-sec"}}, nil)

		require.True(t, p.canRelayApprovalThreadReply(&model.User{Id: "u2", Roles: "system_user"}, threadAnchor{}))
	})

	t.Run("plain user", func(t *testing.T) {
		p, _ := logOnlyPlugin(&configuration{})
		p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})
		require.False(t, p.canRelayApprovalThreadReply(&model.User{Id: "u3", Roles: "system_user"}, threadAnchor{}))
	})
}

// --- notifyRequesterInThreadCard / updateDMRootCard ---

func TestNotifyRequesterInThreadCard_FallbackReplyFailureIsLogged(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetPost", "dmroot").Return(nil, testAppErr("gone"))
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post.ChannelId == "dm" && post.RootId == "dmroot"
	})).Return(nil, testAppErr("down"))

	p.notifyRequesterInThreadCard("u1", "dmroot", "dm", &model.MessageAttachment{Title: "Approved"})

	api.AssertCalled(t, "LogWarn", "failed to send fallback outcome reply", "user_id", "u1", "error", mock.Anything)
	api.AssertNotCalled(t, "GetDirectChannel", mock.Anything, mock.Anything)
}

func TestUpdateDMRootCard_UsesOutcomeWhenPostHasNoCard(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	outcome := &model.MessageAttachment{Title: "Approved", Color: colorApproved}
	api.On("GetPost", "dmroot").Return(&model.Post{Id: "dmroot"}, nil)
	api.On("UpdatePost", mock.MatchedBy(func(post *model.Post) bool {
		att := post.Attachments()
		return len(att) == 1 && att[0].Title == "Approved" && att[0].Color == colorApproved
	})).Return(nil, testAppErr("down"))

	require.False(t, p.updateDMRootCard("dmroot", outcome))
	api.AssertCalled(t, "LogWarn", "failed to update DM root post with outcome", "post_id", "dmroot", "error", mock.Anything)
}

// --- extractFirstAttachment ---

func TestExtractFirstAttachment_UnusableProps(t *testing.T) {
	tests := []struct {
		name  string
		props model.StringInterface
	}{
		{"no props", nil},
		{"no attachments key", model.StringInterface{"other": 1}},
		{"value can't be encoded", model.StringInterface{"attachments": func() {}}},
		{"wrong shape", model.StringInterface{"attachments": "text"}},
		{"empty list", model.StringInterface{"attachments": []any{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			post := &model.Post{}
			post.SetProps(tc.props)
			require.Nil(t, extractFirstAttachment(post))
		})
	}
}

// --- postOutcomeThreadReply / notifyRequesterCard ---

func TestPostOutcomeThreadReply_Messages(t *testing.T) {
	t.Run("missing thread IDs skip the reply", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		p.postOutcomeThreadReply("", "dmroot", true, "admin")
		p.postOutcomeThreadReply("dm", "", true, "admin")
		api.AssertNotCalled(t, "CreatePost", mock.Anything)
	})

	t.Run("denied reply failure is logged", func(t *testing.T) {
		p, api := logOnlyPlugin(&configuration{})
		api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return post.ChannelId == "dm" && post.RootId == "dmroot" && post.Message == "❌ **Denied** by @admin."
		})).Return(nil, testAppErr("down"))

		p.postOutcomeThreadReply("dm", "dmroot", false, "admin")

		api.AssertCalled(t, "LogWarn", "failed to post outcome thread reply",
			"channel_id", "dm", "root_id", "dmroot", "error", mock.Anything)
	})
}

func TestNotifyRequesterCard_PostFailureIsLogged(t *testing.T) {
	p, api := logOnlyPlugin(&configuration{})
	api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		att := post.Attachments()
		return post.ChannelId == "dm" && len(att) == 1 && att[0].Title == "Denied"
	})).Return(nil, testAppErr("down"))

	p.notifyRequesterCard("u1", &model.MessageAttachment{Title: "Denied"})

	api.AssertCalled(t, "LogWarn", "failed to send card DM to requester", "user_id", "u1", "error", mock.Anything)
}
