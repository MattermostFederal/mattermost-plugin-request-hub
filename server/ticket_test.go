package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestTicketSubmittedIntro(t *testing.T) {
	require.Contains(t, ticketSubmittedIntro("channel request"), "channel request")
	require.Contains(t, ticketSubmittedIntro(""), "request has been submitted")
}

func TestPostTicketCreatedDM(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post != nil &&
			post.ChannelId == "dm1" &&
			post.UserId == "bot-user-id" &&
			post.Message != "" &&
			post.Props != nil
	})).Return(&model.Post{Id: "dm-root-1", ChannelId: "dm1"}, nil)

	card := &model.MessageAttachment{
		Title:   "Channel creation request",
		Actions: []*model.PostAction{{Id: "approve", Name: "Approve"}},
	}
	rootID, chID := p.postTicketCreatedDM("u_req", card, ticketSubmittedIntro("channel request"))
	require.Equal(t, "dm-root-1", rootID)
	require.Equal(t, "dm1", chID)
}

func TestCloseTicket(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	// updateDMRootCard path
	api.On("GetPost", "dm-root").Return(&model.Post{
		Id:        "dm-root",
		ChannelId: "dm1",
		Props: model.StringInterface{
			"attachments": []any{
				map[string]any{"title": "Pending", "color": colorPending},
			},
		},
	}, nil)
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post != nil && post.RootId == "dm-root" && post.ChannelId == "dm1"
	})).Return(&model.Post{}, nil)
	api.On("GetPost", "approval-post").Return(&model.Post{
		Id:        "approval-post",
		ChannelId: "appr-ch",
		Props: model.StringInterface{
			"attachments": []any{
				map[string]any{"title": "Request", "actions": []any{map[string]any{"id": "approve"}}},
			},
		},
	}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post != nil && post.RootId == "approval-post"
	})).Return(&model.Post{}, nil)

	out := p.closeTicket(ticketClose{
		RequesterID:      "u_req",
		DMRootPostID:     "dm-root",
		DMChannelID:      "dm1",
		ApprovalPostID:   "approval-post",
		Approved:         true,
		ApproverUsername: "admin",
		CardTitle:        "Approved",
		CardText:         "Done",
		StatusLine:       "✅ Approved by @admin",
	})
	require.NotNil(t, out)
	require.Equal(t, "✅ Approved by @admin", out.Message)
}

func TestRegisterTicketThread(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("KVSet", kvApprovalPostPrefix+"ap1", mock.Anything).Return(nil)
	api.On("KVSet", kvDMRootPrefix+"dm1", mock.Anything).Return(nil)

	p.registerTicketThread(ticketThread{
		ApprovalPostID: "ap1",
		DMRootPostID:   "dm1",
		Kind:           requestTypeChannel,
		ScopeID:        "team1",
	})
	api.AssertCalled(t, "KVSet", kvApprovalPostPrefix+"ap1", mock.Anything)
	api.AssertCalled(t, "KVSet", kvDMRootPrefix+"dm1", mock.Anything)
}

func TestPersistTicketPosts_RegistersOnCASSuccess(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	rawBefore := []byte(`{"id":"r1"}`)
	record := map[string]string{"id": "r1", "approval_post_id": "ap1"}
	api.On("KVCompareAndSet", kvRequestPrefix+"r1", rawBefore, mock.Anything).Return(true, nil)
	api.On("KVSet", kvApprovalPostPrefix+"ap1", mock.Anything).Return(nil)
	api.On("KVSet", kvDMRootPrefix+"dm1", mock.Anything).Return(nil)

	p.persistTicketPosts(ticketPersist{
		KVKey: kvRequestPrefix + "r1", RawBefore: rawBefore, Record: record,
		RequestID: "r1", Label: "channel request",
		Thread: ticketThread{ApprovalPostID: "ap1", DMRootPostID: "dm1", Kind: requestTypeChannel},
	})
	api.AssertCalled(t, "KVSet", kvApprovalPostPrefix+"ap1", mock.Anything)
}

func TestNotifyTicketProgress(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("GetPost", "dm-root").Return(&model.Post{
		Id: "dm-root", ChannelId: "dm1",
		Props: model.StringInterface{"attachments": []any{map[string]any{"title": "Pending"}}},
	}, nil)
	api.On("UpdatePost", mock.Anything).Return(&model.Post{}, nil)

	p.notifyTicketProgress(ticketProgress{
		RequesterID: "u_req", DMRootPostID: "dm-root", DMChannelID: "dm1",
		Title: "Update", Text: "Halfway there",
	})
	api.AssertCalled(t, "UpdatePost", mock.Anything)
}

func TestPersistTicketPosts_LostOrFailedWriteSkipsRelay(t *testing.T) {
	for name, casErr := range map[string]*model.AppError{"lost": nil, "failed": testAppErr("kv down")} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogLevels(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)

			rawBefore := []byte(`{"id":"r1"}`)
			api.On("KVCompareAndSet", kvRequestPrefix+"r1", rawBefore, mock.Anything).Return(false, casErr)

			p.persistTicketPosts(ticketPersist{
				KVKey: kvRequestPrefix + "r1", RawBefore: rawBefore, Record: map[string]string{"id": "r1"},
				RequestID: "r1", Label: "channel request",
				Thread: ticketThread{ApprovalPostID: "ap1", DMRootPostID: "dm1", Kind: requestTypeChannel},
			})

			api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
		})
	}
}
