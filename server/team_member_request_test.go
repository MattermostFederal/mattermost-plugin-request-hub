package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestSubmitTeamMemberRequest_Disabled(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	p.setConfiguration(&configuration{AllowTeamMemberRequests: false})
	_, err := p.submitTeamMemberRequest("u_req", "team1", []string{testNomineeID}, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "disabled")
}

func TestSubmitTeamMemberRequest_QueuesForApproval(t *testing.T) {
	api := &plugintest.API{}
	stubNamesFree(api)
	stubPendingLists(api)
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowTeamMemberRequests: true})

	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", DisplayName: "Ops"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("GetUser", testNomineeID).Return(&model.User{Id: testNomineeID, Username: "nommy"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-team-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "team-req-ch", TeamId: "appteam-id"}, nil)
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sys1", Username: "root"}}, nil)
	api.On("GetUser", "sys1").Return(&model.User{Id: "sys1", Username: "root"}, nil)
	api.On("GetChannelSidebarCategories", "sys1", "appteam-id").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{{
			SidebarCategory: model.SidebarCategory{DisplayName: "Mattermost: Request Hub"},
			Channels:        []string{"team-req-ch"},
		}},
	}, nil).Maybe()

	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
		return post.ChannelId == "team-req-ch"
	})).Return(&model.Post{Id: "approval-post-1"}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "req-dm"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "dm-post-1"}, nil)

	msg, err := p.submitTeamMemberRequest("u_req", "team1", []string{testNomineeID}, "need access")
	require.NoError(t, err)
	require.Contains(t, msg, "submitted")
}
