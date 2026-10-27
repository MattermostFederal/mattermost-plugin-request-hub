package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// unknownStep is a step value outside the two defined pools.
const unknownStep approverStep = 99

// adminField returns an admin-managed text field, trusted as an approval gate.
func adminField(id, groupID string) *model.PropertyField {
	return &model.PropertyField{
		ID:      id,
		GroupID: groupID,
		Type:    model.PropertyFieldTypeText,
		Attrs: model.StringInterface{
			model.CustomProfileAttributesPropertyAttrsManaged: "admin",
		},
	}
}

// selectField returns a select field with the given options.
func selectField(opts ...*model.CustomProfileAttributesSelectOption) *model.PropertyField {
	return &model.PropertyField{
		Type: model.PropertyFieldTypeSelect,
		Attrs: model.StringInterface{
			model.PropertyFieldAttributeOptions: opts,
		},
	}
}

func TestApproverStep_String(t *testing.T) {
	require.Equal(t, "security", stepSecurity.String())
	require.Equal(t, "system", stepSystem.String())
}

func TestApproverPoolNames_UnknownStepIsEmpty(t *testing.T) {
	cfg := &configuration{
		SecurityApproverAttribute: "sec",
		SystemApproverAttribute:   "sys",
		SecurityApproverGroup:     "@sec-group",
		SystemApproverGroup:       " sys-group ",
	}
	require.Equal(t, "sec", cfg.approverAttributeName(stepSecurity))
	require.Equal(t, "sys", cfg.approverAttributeName(stepSystem))
	require.Empty(t, cfg.approverAttributeName(unknownStep))
	require.Equal(t, "sec-group", cfg.approverGroupName(stepSecurity))
	require.Equal(t, "sys-group", cfg.approverGroupName(stepSystem))
	require.Empty(t, cfg.approverGroupName(unknownStep))
}

func TestSecurityPoolConfigured_ByMode(t *testing.T) {
	cases := []struct {
		name string
		cfg  configuration
		want bool
	}{
		{"system admin mode never has a pool", configuration{TwoStepApproverMode: twoStepModeSystemAdmin, SecurityApproverAttribute: "sec", SecurityApproverGroup: "g"}, false},
		{"usergroup mode with a group", configuration{TwoStepApproverMode: twoStepModeUserGroup, SecurityApproverGroup: "g"}, true},
		{"usergroup mode ignores the attribute", configuration{TwoStepApproverMode: twoStepModeUserGroup, SecurityApproverAttribute: "sec"}, false},
		{"attribute mode with an attribute", configuration{SecurityApproverAttribute: "sec"}, true},
		{"attribute mode without an attribute", configuration{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.cfg.securityPoolConfigured())
		})
	}
}

func TestCanApproveStep_NilUserIsRefused(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	ok, err := p.canApproveStep(nil, stepSystem)
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, api.Calls)
}

func TestCanApproveStepViaGroup_NoGroupFallsBackToSystemAdmin(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeUserGroup})

	ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSecurity)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = p.canApproveStep(&model.User{Id: "u"}, stepSecurity)
	require.NoError(t, err)
	require.False(t, ok)
	api.AssertNotCalled(t, "GetGroupByName", mock.Anything)
}

func TestPoolLookupSoftFallback_SystemStepUsesSystemAdminRole(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{SecurityApproverAttribute: "sec"})

	admin := &model.User{Id: "a", Roles: model.SystemAdminRoleId}
	require.True(t, p.poolLookupSoftFallback(admin, stepSystem))
	require.False(t, p.poolLookupSoftFallback(&model.User{Id: "u"}, stepSystem))
	// A configured security pool is never covered by the fallback.
	require.False(t, p.poolLookupSoftFallback(admin, stepSecurity))

	p.setConfiguration(&configuration{})
	require.True(t, p.poolLookupSoftFallback(admin, stepSecurity))
}

func TestCanApproveStepViaAttribute_LookupFailureSoftFallsBackForSystemStep(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{SystemApproverAttribute: "sys"})
	api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(nil, testAppErr("unavailable"))

	// canApproveStep lets System Admins through the system step before any
	// lookup, so the fallback is reached through the attribute check itself.
	ok, err := p.canApproveStepViaAttribute(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSystem)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = p.canApproveStepViaAttribute(&model.User{Id: "u"}, stepSystem)
	require.NoError(t, err)
	require.False(t, ok)
	api.AssertCalled(t, "LogWarn", "approver attribute check failed", "attr", "sys", "step", "system", "error", mock.Anything)
}

func TestApproverAttributePoolReady_BlankNameIsNotReady(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	require.False(t, p.approverAttributePoolReady("   "))
	require.Empty(t, api.Calls)
}

func TestApplyTwoStepApproval_NilUserKeepsState(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	state := twoStepState{SecurityApproverID: "sec"}
	require.Equal(t, state, p.applyTwoStepApproval(state, stepSystem, nil, 1))
}

func TestApplyTwoStepApproval_SystemAdminModeSystemStepFillsSecurity(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})

	admin := &model.User{Id: "admin", Roles: model.SystemAdminRoleId}
	state := p.applyTwoStepApproval(twoStepState{}, stepSystem, admin, 100)
	require.True(t, state.complete())
	require.Equal(t, "admin", state.SystemApproverID)
	require.Equal(t, "admin", state.SecurityApproverID)
}

func TestLoadApproverAttributeField_Paths(t *testing.T) {
	t.Run("blank name makes no calls", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		field, err := p.loadApproverAttributeField(" ")
		require.NoError(t, err)
		require.Nil(t, field)
		require.Empty(t, api.Calls)
	})

	t.Run("missing property group", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(nil, nil)
		field, err := p.loadApproverAttributeField("sec")
		require.NoError(t, err)
		require.Nil(t, field)
		api.AssertNotCalled(t, "GetPropertyFieldByName", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("unexpected field error is returned", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "sec").Return(nil, errors.New("database unavailable"))
		field, err := p.loadApproverAttributeField("sec")
		require.Error(t, err)
		require.Contains(t, err.Error(), `failed to load approver attribute "sec"`)
		require.Nil(t, field)
	})

	t.Run("missing field", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "sec").Return(nil, nil)
		field, err := p.loadApproverAttributeField("sec")
		require.NoError(t, err)
		require.Nil(t, field)
	})

	t.Run("field without a group takes the property group ID", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "sec").Return(adminField("fld", ""), nil)
		field, err := p.loadApproverAttributeField("sec")
		require.NoError(t, err)
		require.NotNil(t, field)
		require.Equal(t, "grp", field.GroupID)
	})
}

func TestIsNotFoundErr_Cases(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"404 status", model.NewAppError("x", "x.error", nil, "", 404), true},
		{"app error message", &model.AppError{Id: "x.error", Message: "Field Not Found", StatusCode: 500}, true},
		{"app error id", model.NewAppError("x", "app.field.not_found", nil, "", 500), true},
		{"app error other", testAppErr("unavailable"), false},
		{"plain not found", errors.New("record Not Found"), true},
		{"plain not_found", errors.New("err_not_found"), true},
		{"plain other", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isNotFoundErr(tc.err))
		})
	}
}

func TestUserIsApprover_MorePaths(t *testing.T) {
	t.Run("nil user", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		ok, err := p.userIsApprover(nil, "sec")
		require.NoError(t, err)
		require.False(t, ok)
		require.Empty(t, api.Calls)
	})

	t.Run("missing field is not an approver", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "sec").Return(nil, model.NewAppError("x", "x.error", nil, "", 404))
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "sec")
		require.NoError(t, err)
		require.False(t, ok)
		api.AssertNotCalled(t, "SearchPropertyValues", mock.Anything, mock.Anything)
	})

	t.Run("value search failure is returned", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "sec").Return(adminField("fld", "grp"), nil)
		api.On("SearchPropertyValues", "grp", mock.Anything).Return(nil, errors.New("search failed"))
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "sec")
		require.Error(t, err)
		require.Contains(t, err.Error(), `failed to read approver attribute "sec"`)
		require.False(t, ok)
	})

	// A property group with an empty ID leaves the field's GroupID empty, so
	// the group is looked up a second time.
	t.Run("blank group ID re-reads the group", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: ""}, nil)
		api.On("GetPropertyFieldByName", "", "", "sec").Return(adminField("fld", ""), nil)
		api.On("SearchPropertyValues", "", mock.Anything).Return([]*model.PropertyValue{nil, {Value: json.RawMessage(`"yes"`)}}, nil)
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "sec")
		require.NoError(t, err)
		require.True(t, ok)
		api.AssertNumberOfCalls(t, "GetPropertyGroup", 2)
	})

	t.Run("second group read fails", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: ""}, nil).Once()
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(nil, errors.New("gone")).Once()
		api.On("GetPropertyFieldByName", "", "", "sec").Return(adminField("fld", ""), nil)
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "sec")
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to load access_control property group")
		require.False(t, ok)
		api.AssertNotCalled(t, "SearchPropertyValues", mock.Anything, mock.Anything)
	})

	t.Run("second group read finds nothing", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: ""}, nil).Once()
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(nil, nil).Once()
		api.On("GetPropertyFieldByName", "", "", "sec").Return(adminField("fld", ""), nil)
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "sec")
		require.NoError(t, err)
		require.False(t, ok)
		api.AssertNotCalled(t, "SearchPropertyValues", mock.Anything, mock.Anything)
	})
}

func TestAttributeValueIsSet_ObjectValueIsNotSet(t *testing.T) {
	textField := &model.PropertyField{Type: model.PropertyFieldTypeText}
	require.False(t, attributeValueIsSet(json.RawMessage(`{"a":1}`), textField))
}

func TestSelectAttributeIsAffirmative_Cases(t *testing.T) {
	field := selectField(
		&model.CustomProfileAttributesSelectOption{ID: "opt-yes", Name: "Yes"},
		&model.CustomProfileAttributesSelectOption{ID: "opt-no", Name: "No"},
	)
	require.True(t, selectAttributeIsAffirmative(json.RawMessage(`["opt-no","opt-yes"]`), field))
	require.False(t, selectAttributeIsAffirmative(json.RawMessage(`["opt-no","opt-other"]`), field))
	require.False(t, selectAttributeIsAffirmative(json.RawMessage(`123`), field))
	// No options means nothing can be affirmative.
	require.False(t, selectAttributeIsAffirmative(json.RawMessage(`"opt-yes"`), selectField()))
}

func TestSelectOptionsByID_Cases(t *testing.T) {
	require.Empty(t, selectOptionsByID(nil))

	badAttrs := &model.PropertyField{
		Type:  model.PropertyFieldTypeSelect,
		Attrs: model.StringInterface{model.PropertyFieldAttributeOptions: "not a list"},
	}
	require.Empty(t, selectOptionsByID(badAttrs))

	field := selectField(
		&model.CustomProfileAttributesSelectOption{ID: "", Name: "Blank"},
		&model.CustomProfileAttributesSelectOption{ID: "opt-yes", Name: "Yes"},
	)
	require.Equal(t, map[string]string{"opt-yes": "Yes"}, selectOptionsByID(field))
}

func TestPropertyFieldIsApproverTrusted_Cases(t *testing.T) {
	require.True(t, propertyFieldIsApproverTrusted(adminField("f", "g")))
	require.True(t, propertyFieldIsApproverTrusted(&model.PropertyField{
		Attrs: model.StringInterface{"ldap": "attr"},
	}))
	require.False(t, propertyFieldIsApproverTrusted(&model.PropertyField{}))
	// Attrs that cannot be decoded are never trusted.
	require.False(t, propertyFieldIsApproverTrusted(&model.PropertyField{
		Attrs: model.StringInterface{"managed": 42},
	}))
}

func TestUserHasLoggedIn_SkipsBlankAndBotUsers(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.UserHasLoggedIn(nil, nil)
	p.UserHasLoggedIn(nil, &model.User{})
	p.UserHasLoggedIn(nil, &model.User{Id: p.botUserID})
	require.Empty(t, api.Calls)
}

func TestUserHasLoggedIn_ReconcilesInBackground(t *testing.T) {
	api := &plugintest.API{}
	// Keep the log stubs: the background reconcile still logs after the wait.
	stubLogLevels(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam"})
	done := make(chan struct{}, 2)
	// The approval team lookup fails, so no channel is resolved; the call
	// itself shows the reconcile ran.
	api.On("GetTeamByName", "appteam").Return(nil, testAppErr("missing")).Run(func(mock.Arguments) {
		done <- struct{}{}
	})
	api.On("KVGet", kvApprovalTeamKey).Return(nil, nil).Maybe()

	p.UserHasLoggedIn(nil, &model.User{Id: "u1"})
	// Once for the bot channel and once for the webhook channel.
	waitFor(t, done, "bot channel reconcile")
	waitFor(t, done, "webhook channel reconcile")
}

func TestShouldRetainTwoStepApprovalChannelAccess_Cases(t *testing.T) {
	t.Run("blank user is kept", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		retain, confident := p.shouldRetainTwoStepApprovalChannelAccess(nil, "tid")
		require.True(t, retain)
		require.True(t, confident)
	})

	t.Run("system admin mode drops non-admins", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})
		retain, confident := p.shouldRetainTwoStepApprovalChannelAccess(&model.User{Id: "u1"}, "")
		require.False(t, retain)
		require.True(t, confident)
	})

	t.Run("group lookup failure is not confident", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeUserGroup, SystemApproverGroup: "sys-group"})
		api.On("GetGroupByName", "sys-group").Return(nil, testAppErr("missing"))
		api.On("GetGroups", 0, 100, mock.Anything, mock.Anything).Return(nil, nil)
		api.On("GetGroupsBySource", model.GroupSourceLdap).Return(nil, testAppErr("ldap down"))
		retain, confident := p.shouldRetainTwoStepApprovalChannelAccess(&model.User{Id: "u1"}, "")
		require.False(t, retain)
		require.False(t, confident)
	})

	t.Run("attribute holder is kept", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SystemApproverAttribute: "sys"})
		mockApproverAttr(api, "sys", "u1", []*model.PropertyValue{{Value: json.RawMessage(`"yes"`)}})
		retain, confident := p.shouldRetainTwoStepApprovalChannelAccess(&model.User{Id: "u1"}, "")
		require.True(t, retain)
		require.True(t, confident)
	})

	t.Run("attribute non-holder is dropped", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SystemApproverAttribute: "sys"})
		mockApproverAttr(api, "sys", "u1", nil)
		retain, confident := p.shouldRetainTwoStepApprovalChannelAccess(&model.User{Id: "u1"}, "")
		require.False(t, retain)
		require.True(t, confident)
	})

	t.Run("attribute lookup failure is not confident", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SecurityApproverAttribute: "sec"})
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(nil, testAppErr("unavailable"))
		retain, confident := p.shouldRetainTwoStepApprovalChannelAccess(&model.User{Id: "u1"}, "")
		require.False(t, retain)
		require.False(t, confident)
	})
}

func TestUserIsTeamAdmin_Cases(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	require.False(t, p.userIsTeamAdmin("", "u1"))
	require.False(t, p.userIsTeamAdmin("tid", ""))

	api.On("GetTeamMember", "tid", "err").Return(nil, testAppErr("missing"))
	api.On("GetTeamMember", "tid", "left").Return(&model.TeamMember{SchemeAdmin: true, DeleteAt: 5}, nil)
	api.On("GetTeamMember", "tid", "role").Return(&model.TeamMember{Roles: "team_user team_admin"}, nil)
	require.False(t, p.userIsTeamAdmin("tid", "err"))
	require.False(t, p.userIsTeamAdmin("tid", "left"))
	require.True(t, p.userIsTeamAdmin("tid", "role"))
}

func TestAddUserToApprovalChannel_FailurePaths(t *testing.T) {
	t.Run("nil channel or blank user", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.addUserToApprovalChannel(nil, "u1")
		p.addUserToApprovalChannel(&model.Channel{Id: "ch"}, "")
		require.Empty(t, api.Calls)
	})

	t.Run("team join and channel add failures stop before the sidebar", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		api.On("CreateTeamMember", "tid", "u1").Return(nil, testAppErr("already"))
		api.On("AddChannelMember", "ch", "u1").Return(nil, testAppErr("denied"))
		p.addUserToApprovalChannel(&model.Channel{Id: "ch", TeamId: "tid"}, "u1")
		api.AssertCalled(t, "AddChannelMember", "ch", "u1")
		api.AssertNotCalled(t, "GetChannelSidebarCategories", mock.Anything, mock.Anything)
	})
}

func TestListApproverAttributeUserIDs_FailurePaths(t *testing.T) {
	t.Run("blank name", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		require.Nil(t, p.listApproverAttributeUserIDs(" "))
		require.Empty(t, api.Calls)
	})

	t.Run("field load failure", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(nil, testAppErr("unavailable"))
		require.Nil(t, p.listApproverAttributeUserIDs("sec"))
		api.AssertNotCalled(t, "SearchPropertyValues", mock.Anything, mock.Anything)
	})

	t.Run("blank group ID and second group read fails", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: ""}, nil).Once()
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(nil, testAppErr("gone")).Once()
		api.On("GetPropertyFieldByName", "", "", "sec").Return(adminField("fld", ""), nil)
		require.Nil(t, p.listApproverAttributeUserIDs("sec"))
		api.AssertNotCalled(t, "SearchPropertyValues", mock.Anything, mock.Anything)
	})

	t.Run("blank group ID re-read and search failure returns what was found", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: ""}, nil)
		api.On("GetPropertyFieldByName", "", "", "sec").Return(adminField("fld", ""), nil)
		api.On("SearchPropertyValues", "", mock.Anything).Return(nil, testAppErr("search failed"))
		require.Empty(t, p.listApproverAttributeUserIDs("sec"))
		api.AssertNumberOfCalls(t, "GetPropertyGroup", 2)
	})

	t.Run("values without a target are skipped", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "sec").Return(adminField("fld", "grp"), nil)
		api.On("SearchPropertyValues", "grp", mock.Anything).Return([]*model.PropertyValue{
			nil,
			{ID: "v1", TargetID: "", Value: json.RawMessage(`"yes"`)},
			{ID: "v2", TargetID: "u2", Value: json.RawMessage(`"yes"`)},
		}, nil)
		require.Equal(t, []string{"u2"}, p.listApproverAttributeUserIDs("sec"))
	})
}

func TestListGroupMemberIDsByGroupID_FailureKeepsEarlierPages(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	page := make([]*model.User, 100)
	for i := range page {
		page[i] = &model.User{Id: "u"}
	}
	page[0] = nil
	api.On("GetGroupMemberUsers", "gid", 0, 100).Return(page, nil)
	api.On("GetGroupMemberUsers", "gid", 1, 100).Return(nil, testAppErr("failed"))
	ids := p.listGroupMemberIDsByGroupID("gid", "label")
	require.Len(t, ids, 99)
}

func TestEvictStaleMembersFromTwoStepChannel_FailurePaths(t *testing.T) {
	t.Run("nil channel", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.evictStaleMembersFromTwoStepChannel(nil)
		require.Empty(t, api.Calls)
	})

	t.Run("member list failure", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		api.On("GetChannelMembers", "ch", 0, 100).Return(nil, testAppErr("failed"))
		p.evictStaleMembersFromTwoStepChannel(&model.Channel{Id: "ch"})
		api.AssertNotCalled(t, "GetUser", mock.Anything)
	})

	t.Run("unknown user skipped and delete failure logged", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogLevels(api)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})
		api.On("GetChannelMembers", "ch", 0, 100).Return(model.ChannelMembers{{UserId: "gone"}, {UserId: "u1"}}, nil)
		api.On("GetUser", "gone").Return(nil, testAppErr("missing"))
		api.On("GetUser", "u1").Return(&model.User{Id: "u1"}, nil)
		api.On("DeleteChannelMember", "ch", "u1").Return(testAppErr("denied"))
		p.evictStaleMembersFromTwoStepChannel(&model.Channel{Id: "ch"})
		api.AssertCalled(t, "DeleteChannelMember", "ch", "u1")
		api.AssertNotCalled(t, "DeleteChannelMember", "ch", "gone")
	})
}

func TestEvictStaleTwoStepApproverChannelMembers_SystemAdminModeDoesNothing(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})
	p.evictStaleTwoStepApproverChannelMembers()
	p.reconcileTwoStepApproverChannels()
	require.Empty(t, api.Calls)
}

func TestLinkTwoStepGroupsToChannel_SyncableFailuresAreLogged(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		TwoStepApproverMode:   twoStepModeUserGroup,
		SecurityApproverGroup: "sec-group",
		SystemApproverGroup:   "sys-group",
	})
	stubSyncedGroup(api, "sec-group", "g-sec")
	stubSyncedGroup(api, "sys-group", "g-sys")
	ch := &model.Channel{Id: "ch", TeamId: "tid"}

	api.On("UpsertGroupSyncable", mock.MatchedBy(func(gs *model.GroupSyncable) bool { return gs.GroupId == "g-sec" })).
		Return(nil, testAppErr("not allowed"))
	api.On("UpsertGroupSyncable", mock.MatchedBy(func(gs *model.GroupSyncable) bool {
		return gs.GroupId == "g-sys"
	})).Return(&model.GroupSyncable{}, nil)
	api.On("CreateDefaultSyncableMemberships", mock.Anything).Return(testAppErr("sync failed"))

	p.linkTwoStepGroupsToChannel(ch)

	// The security group's channel link failed, so only the system group syncs.
	api.AssertNumberOfCalls(t, "CreateDefaultSyncableMemberships", 1)
	api.AssertNumberOfCalls(t, "UpsertGroupSyncable", 4)
}

// approvalChannels resolves the bot and webhook approval channels in team
// "tid" through the Approval team "appteam".
func approvalChannels(api *plugintest.API) {
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "tid", Name: "appteam"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "tid", "mattermost-bot-requests", true).Return(
		&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "bot-ch", TeamId: "tid"}, nil)
	api.On("GetChannelByName", "tid", "mattermost-webhook-requests", true).Return(
		&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "wh-ch", TeamId: "tid"}, nil)
}

func TestTwoStepChannelAccess_BlankInputsDoNothing(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeUserGroup, SecurityApproverGroup: "g"})
	p.linkTwoStepGroupsToChannel(nil)
	p.reconcileTwoStepAccessForUser(nil)
	p.reconcileTwoStepAccessForUser(&model.User{Id: p.botUserID})
	p.addUserToTwoStepApprovalChannels("")
	p.addUserToTwoStepApprovalChannels(p.botUserID)
	require.Empty(t, api.Calls)
}

func TestAddUserToTwoStepApprovalChannels_NoApprovalTeamSkipsChannels(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{})
	p.addUserToTwoStepApprovalChannels("u1")
	require.Empty(t, api.Calls)
}

func TestReconcileTwoStepAccessForUser_LookupFailureKeepsMembership(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", SecurityApproverAttribute: "sec"})
	approvalChannels(api)
	api.On("GetTeamMember", "tid", "u1").Return(nil, testAppErr("missing"))
	api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(nil, testAppErr("unavailable"))

	p.reconcileTwoStepAccessForUser(&model.User{Id: "u1"})

	api.AssertNotCalled(t, "DeleteChannelMember", mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestReconcileTwoStepAccessForUser_EvictFailureIsLogged(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", TwoStepApproverMode: twoStepModeSystemAdmin})
	approvalChannels(api)
	api.On("GetTeamMember", "tid", "u1").Return(&model.TeamMember{Roles: model.TeamUserRoleId}, nil)
	api.On("DeleteChannelMember", mock.Anything, "u1").Return(testAppErr("not a member"))

	p.reconcileTwoStepAccessForUser(&model.User{Id: "u1"})

	api.AssertCalled(t, "DeleteChannelMember", "bot-ch", "u1")
	api.AssertCalled(t, "DeleteChannelMember", "wh-ch", "u1")
	for _, ch := range []string{"bot-ch", "wh-ch"} {
		api.AssertCalled(t, "LogDebug", "two-step channel access: evict on login/reconcile",
			"channel_id", ch, "user_id", "u1", "error", mock.Anything)
	}
	api.AssertNotCalled(t, "LogInfo", "two-step channel access: evicted demoted approver",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestListTwoStepApproverUserIDs_DedupesAndSkipsBlank(t *testing.T) {
	t.Run("attribute mode", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		// Both pools read the same attribute, so every holder appears twice.
		p.setConfiguration(&configuration{SecurityApproverAttribute: "shared", SystemApproverAttribute: "shared"})
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "shared").Return(adminField("fld", "grp"), nil)
		api.On("SearchPropertyValues", "grp", mock.Anything).Return([]*model.PropertyValue{
			{ID: "v1", TargetID: "u1", Value: json.RawMessage(`"yes"`)},
		}, nil)
		require.Equal(t, []string{"u1"}, p.listTwoStepApproverUserIDs())
	})

	t.Run("usergroup mode", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeUserGroup, SecurityApproverGroup: "grp-a", SystemApproverGroup: "grp-a"})
		stubSyncedGroup(api, "grp-a", "g-a")
		api.On("GetGroupMemberUsers", "g-a", 0, 100).Return([]*model.User{{Id: ""}, {Id: "u1"}}, nil)
		require.Equal(t, []string{"u1"}, p.listTwoStepApproverUserIDs())
	})
}

func TestReconcileTwoStepApproverChannels_SkipsBotUser(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", SecurityApproverAttribute: "sec"})
	approvalChannels(api)
	api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
	api.On("GetPropertyFieldByName", "grp", "", "sec").Return(adminField("fld", "grp"), nil)
	api.On("SearchPropertyValues", "grp", mock.Anything).Return([]*model.PropertyValue{
		{ID: "v1", TargetID: p.botUserID, Value: json.RawMessage(`"yes"`)},
		{ID: "v2", TargetID: "u1", Value: json.RawMessage(`"yes"`)},
	}, nil)
	api.On("CreateTeamMember", mock.Anything, mock.Anything).Return(&model.TeamMember{}, nil).Maybe()
	api.On("AddChannelMember", mock.Anything, mock.Anything).Return(&model.ChannelMember{}, nil).Maybe()
	api.On("GetChannelSidebarCategories", mock.Anything, mock.Anything).Return(nil, testAppErr("down")).Maybe()
	// No members to evict afterwards.
	api.On("GetChannelMembers", mock.Anything, 0, 100).Return(model.ChannelMembers{}, nil)

	p.reconcileTwoStepApproverChannels()

	api.AssertCalled(t, "LogInfo", "two-step channel access: periodic reconcile", "count", 2)
	api.AssertCalled(t, "AddChannelMember", "bot-ch", "u1")
	api.AssertCalled(t, "AddChannelMember", "wh-ch", "u1")
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, p.botUserID)
	api.AssertNotCalled(t, "CreateTeamMember", mock.Anything, p.botUserID)
}
