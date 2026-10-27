package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestResolveApproverGroup_BlankNameIsAnError(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	_, err := p.resolveApproverGroup(" @ ")

	require.Error(t, err)
	api.AssertNotCalled(t, "GetGroupByName", mock.Anything)
}

func TestResolveApproverGroup_FoundBySearch(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	name := "sec"
	synced := &model.Group{Id: "g-plugin", Name: &name, Source: model.GroupSource("plugin_okta")}
	api.On("GetGroupByName", "sec").Return(nil, testAppErr("not found"))
	api.On("GetGroups", 0, 100, mock.Anything, mock.Anything).Return([]*model.Group{nil, synced}, nil)

	group, err := p.resolveApproverGroup("@sec")

	require.NoError(t, err)
	require.Equal(t, "g-plugin", group.Id)
	api.AssertNotCalled(t, "GetGroupsBySource", mock.Anything)
}

func TestResolveApproverGroup_DisplayNameMatches(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	other := "something-else"
	ldap := &model.Group{Id: "g-ldap", Name: &other, DisplayName: "Security Reviewers", Source: model.GroupSourceLdap}
	api.On("GetGroupByName", "security reviewers").Return(nil, testAppErr("not found"))
	api.On("GetGroups", 0, 100, mock.Anything, mock.Anything).Return([]*model.Group{ldap}, nil)

	group, err := p.resolveApproverGroup("security reviewers")

	require.NoError(t, err)
	require.Equal(t, "g-ldap", group.Id)
}

func TestResolveApproverGroup_DeletedGroupIsSkipped(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	name := "sec"
	deleted := &model.Group{Id: "g-old", Name: &name, Source: model.GroupSourceLdap, DeleteAt: 9}
	api.On("GetGroupByName", "sec").Return(deleted, nil)
	api.On("GetGroups", 0, 100, mock.Anything, mock.Anything).Return([]*model.Group{deleted}, nil)
	api.On("GetGroupsBySource", model.GroupSourceLdap).Return([]*model.Group{deleted}, nil)

	_, err := p.resolveApproverGroup("sec")

	require.Error(t, err)
	api.AssertCalled(t, "LogWarn", "two-step channel access: could not resolve approver group", "group", "sec")
}

func TestResolveApproverGroup_LDAPListFailureIsAnError(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	api.On("GetGroupByName", "sec").Return(nil, testAppErr("not found"))
	api.On("GetGroups", 0, 100, mock.Anything, mock.Anything).Return(nil, testAppErr("search failed"))
	api.On("GetGroupsBySource", model.GroupSourceLdap).Return(nil, testAppErr("ldap down"))

	_, err := p.resolveApproverGroup("sec")

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to list LDAP groups")
}

func TestUserInApproverGroup_BlankInputsAreNotMembers(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	in, err := p.userInApproverGroup("", "sec")
	require.NoError(t, err)
	require.False(t, in)

	in, err = p.userInApproverGroup("u1", "  ")
	require.NoError(t, err)
	require.False(t, in)

	api.AssertNotCalled(t, "GetGroupByName", mock.Anything)
}

func TestUserInApproverGroup_GroupListFailureIsAnError(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	stubSyncedGroup(api, "sec", "g-sec")
	api.On("GetGroupsForUser", "u1").Return(nil, testAppErr("db down"))

	in, err := p.userInApproverGroup("u1", "sec")

	require.Error(t, err)
	require.False(t, in)
}

func TestUserInApproverGroup_MatchesByIDOnly(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	stubSyncedGroup(api, "sec", "g-sec")
	// A different group that shares the name is not the configured group.
	sameName := "sec"
	api.On("GetGroupsForUser", "u1").Return([]*model.Group{nil, {Id: "g-custom", Name: &sameName}}, nil)

	in, err := p.userInApproverGroup("u1", "sec")

	require.NoError(t, err)
	require.False(t, in)
}

func TestUserInApproverGroup_MissingGroupIsAnError(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("GetGroupByName", "sec").Return(nil, testAppErr("not found"))
	api.On("GetGroups", 0, 100, mock.Anything, mock.Anything).Return([]*model.Group{}, nil)
	api.On("GetGroupsBySource", model.GroupSourceLdap).Return([]*model.Group{}, nil)

	in, err := p.userInApproverGroup("u1", "sec")

	require.Error(t, err)
	require.False(t, in)
	api.AssertNotCalled(t, "GetGroupsForUser", mock.Anything)
}

func TestLinkTwoStepGroupsToChannel_SkipsUnresolvedGroup(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeUserGroup, SecurityApproverGroup: "sec"})
	api.On("GetGroupByName", "sec").Return(nil, testAppErr("not found"))
	api.On("GetGroups", 0, 100, mock.Anything, mock.Anything).Return([]*model.Group{}, nil)
	api.On("GetGroupsBySource", model.GroupSourceLdap).Return([]*model.Group{}, nil)

	p.linkTwoStepGroupsToChannel(pluginChannel("bot-ch", "mattermost-bot-requests"))

	api.AssertNotCalled(t, "UpsertGroupSyncable", mock.Anything)
	api.AssertNotCalled(t, "CreateDefaultSyncableMemberships", mock.Anything)
}

func TestResolveApproverGroup_SearchHitWithOtherNameIsSkipped(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)

	similar := "sec-team"
	exact := "sec"
	api.On("GetGroupByName", "sec").Return(nil, testAppErr("not found"))
	api.On("GetGroups", 0, 100, mock.Anything, mock.Anything).
		Return([]*model.Group{{Id: "g-similar", Name: &similar, DisplayName: "Sec Team", Source: model.GroupSourceLdap}}, nil)
	api.On("GetGroupsBySource", model.GroupSourceLdap).
		Return([]*model.Group{{Id: "g-sec", Name: &exact, Source: model.GroupSourceLdap}}, nil)

	group, err := p.resolveApproverGroup("sec")

	require.NoError(t, err)
	require.Equal(t, "g-sec", group.Id)
}
