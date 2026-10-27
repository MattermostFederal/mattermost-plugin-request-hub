package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestAttributeValueIsSet(t *testing.T) {
	textField := &model.PropertyField{Type: model.PropertyFieldTypeText}
	set := func(s string) bool { return attributeValueIsSet(json.RawMessage(s), textField) }

	// Set values (text / bool / array).
	require.True(t, set(`"yes"`))
	require.True(t, set(`"security"`))
	require.True(t, set(`["a","b"]`))
	require.True(t, set(`true`))

	// Unset / false-y values.
	require.False(t, set(``))
	require.False(t, set(`""`))
	require.False(t, set(`"no"`))
	require.False(t, set(`"false"`))
	require.False(t, set(`"0"`))
	require.False(t, set(`[]`))
	require.False(t, set(`false`))
}

func TestAttributeValueIsSet_SelectResolvesOptionNames(t *testing.T) {
	yesID := model.NewId()
	noID := model.NewId()
	field := &model.PropertyField{
		Type: model.PropertyFieldTypeSelect,
		Attrs: model.StringInterface{
			model.PropertyFieldAttributeOptions: []*model.CustomProfileAttributesSelectOption{
				{ID: yesID, Name: "Yes"},
				{ID: noID, Name: "No"},
			},
		},
	}

	require.True(t, attributeValueIsSet(json.RawMessage(`"`+yesID+`"`), field))
	require.False(t, attributeValueIsSet(json.RawMessage(`"`+noID+`"`), field))
	// Opaque ID with no matching option must not authorize.
	require.False(t, attributeValueIsSet(json.RawMessage(`"opt-unknown"`), field))
}

// mockApproverAttr wires the three Property API calls for one attribute lookup.
// The field is admin-managed so userIsApprover accepts it as an approval gate.
func mockApproverAttr(api *plugintest.API, attrName, userID string, values []*model.PropertyValue) {
	api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
	api.On("GetPropertyFieldByName", "grp", "", attrName).Return(&model.PropertyField{
		ID:      "fld-" + attrName,
		GroupID: "grp",
		Type:    model.PropertyFieldTypeText,
		Attrs: model.StringInterface{
			model.CustomProfileAttributesPropertyAttrsManaged: "admin",
		},
	}, nil)
	api.On("SearchPropertyValues", "grp", mock.MatchedBy(func(opts model.PropertyValueSearchOpts) bool {
		return opts.TargetType == model.PropertyValueTargetTypeUser &&
			len(opts.TargetIDs) == 1 && opts.TargetIDs[0] == userID &&
			opts.FieldID == "fld-"+attrName
	})).Return(values, nil)
}

func TestUserIsApprover(t *testing.T) {
	t.Run("empty attribute name is never an approver", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "  ")
		require.NoError(t, err)
		require.False(t, ok)
		api.AssertNotCalled(t, "GetPropertyGroup", mock.Anything)
	})

	t.Run("set attribute value means approver", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		mockApproverAttr(api, "security_approver", "u1", []*model.PropertyValue{{Value: json.RawMessage(`"yes"`)}})
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "security_approver")
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("no value means not an approver", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		mockApproverAttr(api, "security_approver", "u1", []*model.PropertyValue{})
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "security_approver")
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("user-writable attribute is rejected", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "self_serve").Return(&model.PropertyField{
			ID:   "fld-self",
			Type: model.PropertyFieldTypeText,
			// No Managed / LDAP / SAML / Owners — user can write this field.
		}, nil)
		ok, err := p.userIsApprover(&model.User{Id: "u1"}, "self_serve")
		require.Error(t, err)
		require.False(t, ok)
		api.AssertNotCalled(t, "SearchPropertyValues", mock.Anything, mock.Anything)
	})
}

func TestCanApproveStep(t *testing.T) {
	t.Run("system admin auto-qualifies for the system step", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SystemApproverAttribute: "system_approver"})
		ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSystem)
		require.NoError(t, err)
		require.True(t, ok)
		// The role shortcut must skip the attribute lookup entirely.
		api.AssertNotCalled(t, "GetPropertyGroup", mock.Anything)
	})

	t.Run("system admin does NOT auto-qualify for the security step", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SecurityApproverAttribute: "security_approver"})
		// No security attribute value set for this admin.
		mockApproverAttr(api, "security_approver", "a", []*model.PropertyValue{})
		ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSecurity)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("non-admin with the security attribute qualifies for the security step", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SecurityApproverAttribute: "security_approver"})
		mockApproverAttr(api, "security_approver", "u2", []*model.PropertyValue{{Value: json.RawMessage(`"yes"`)}})
		ok, err := p.canApproveStep(&model.User{Id: "u2"}, stepSecurity)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("missing CPA field: configured security pool still blocks system admin", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SecurityApproverAttribute: "security_approver"})
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(&model.PropertyGroup{ID: "grp"}, nil)
		api.On("GetPropertyFieldByName", "grp", "", "security_approver").Return(
			nil, model.NewAppError("GetPropertyFieldByName", "app.property_field.get_by_name.app_error", nil, "not found", 404),
		)
		ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSecurity)
		require.NoError(t, err)
		require.False(t, ok, "forced security review: System Admins must not cover a configured security pool")
	})

	t.Run("empty security attribute: system admin covers security", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{})
		ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSecurity)
		require.NoError(t, err)
		require.True(t, ok)
		api.AssertNotCalled(t, "GetPropertyGroup", mock.Anything)
	})

	t.Run("CPA group failure: configured security pool still blocks system admin", func(t *testing.T) {
		api := &plugintest.API{}
		api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SecurityApproverAttribute: "security_approver"})
		api.On("GetPropertyGroup", model.AccessControlPropertyGroupName).Return(
			nil, model.NewAppError("GetPropertyGroup", "app.error", nil, "unavailable", 500),
		)
		ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSecurity)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("system_admin mode: only system admins qualify for security", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})
		ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSecurity)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = p.canApproveStep(&model.User{Id: "u"}, stepSecurity)
		require.NoError(t, err)
		require.False(t, ok)
		api.AssertNotCalled(t, "GetPropertyGroup", mock.Anything)
		api.AssertNotCalled(t, "GetGroupsForUser", mock.Anything)
	})

	t.Run("usergroup mode: group member qualifies for security", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{
			TwoStepApproverMode:   twoStepModeUserGroup,
			SecurityApproverGroup: "security-approvers",
		})
		stubSyncedGroup(api, "security-approvers", "g-sec")
		api.On("GetGroupsForUser", "u2").Return([]*model.Group{{Id: "g-sec"}}, nil)
		ok, err := p.canApproveStep(&model.User{Id: "u2"}, stepSecurity)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("usergroup mode: a custom group with the configured name does not qualify", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{
			TwoStepApproverMode:   twoStepModeUserGroup,
			SecurityApproverGroup: "security-approvers",
		})
		name := "security-approvers"
		custom := &model.Group{Id: "g-custom", Name: &name, Source: model.GroupSourceCustom}
		api.On("GetGroupByName", "security-approvers").Return(custom, nil)
		api.On("GetGroups", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]*model.Group{custom}, nil)
		api.On("GetGroupsBySource", model.GroupSourceLdap).Return([]*model.Group{}, nil)
		ok, err := p.canApproveStep(&model.User{Id: "u2"}, stepSecurity)
		require.NoError(t, err)
		require.False(t, ok)
		api.AssertNotCalled(t, "GetGroupsForUser", mock.Anything)
	})

	t.Run("usergroup mode: a synced group found by display name qualifies its members", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{
			TwoStepApproverMode:   twoStepModeUserGroup,
			SecurityApproverGroup: "Security Approvers",
		})
		ldap := &model.Group{Id: "g-ldap", DisplayName: "Security Approvers", Source: model.GroupSourceLdap}
		api.On("GetGroupByName", "Security Approvers").Return(nil, testAppErr("not found"))
		api.On("GetGroups", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, testAppErr("search failed"))
		api.On("GetGroupsBySource", model.GroupSourceLdap).Return([]*model.Group{ldap}, nil)
		api.On("GetGroupsForUser", "u2").Return([]*model.Group{{Id: "g-ldap"}}, nil)
		ok, err := p.canApproveStep(&model.User{Id: "u2"}, stepSecurity)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("usergroup mode: missing security group does not let system admin skip review", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{
			TwoStepApproverMode:   twoStepModeUserGroup,
			SecurityApproverGroup: "missing-group",
		})
		api.On("GetGroupsForUser", "a").Return([]*model.Group{}, nil)
		api.On("GetGroupByName", "missing-group").Return(
			nil, model.NewAppError("GetGroupByName", "app.group.not_found", nil, "", 404),
		)
		api.On("GetGroups", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]*model.Group{}, nil).Maybe()
		api.On("GetGroupsBySource", mock.Anything).Return([]*model.Group{}, nil).Maybe()
		ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSecurity)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("usergroup mode: system admin still qualifies for system step", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{
			TwoStepApproverMode:   twoStepModeUserGroup,
			SecurityApproverGroup: "security-approvers",
			SystemApproverGroup:   "system-approvers",
		})
		ok, err := p.canApproveStep(&model.User{Id: "a", Roles: model.SystemAdminRoleId}, stepSystem)
		require.NoError(t, err)
		require.True(t, ok)
		api.AssertNotCalled(t, "GetGroupsForUser", mock.Anything)
	})
}

// stubSyncedGroup makes name resolve to an LDAP group with the given ID.
func stubSyncedGroup(api *plugintest.API, name, id string) {
	groupName := name
	api.On("GetGroupByName", name).Return(&model.Group{Id: id, Name: &groupName, Source: model.GroupSourceLdap}, nil).Maybe()
}

func TestApplyTwoStepApproval(t *testing.T) {
	t.Run("attribute mode never one-clicks both steps for system admin", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		// Empty security attribute used to solo-complete; that was the dual-role bug.
		p.setConfiguration(&configuration{SecurityApproverAttribute: ""})

		admin := &model.User{Id: "admin", Roles: model.SystemAdminRoleId}
		state := p.applyTwoStepApproval(twoStepState{}, stepSecurity, admin, 100)
		require.False(t, state.complete())
		require.Equal(t, "admin", state.SecurityApproverID)
		require.Empty(t, state.SystemApproverID)
	})

	t.Run("usergroup dual-role system admin fills only security on first click", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{
			TwoStepApproverMode:   twoStepModeUserGroup,
			SecurityApproverGroup: "security-approvers",
			SystemApproverGroup:   "system-approvers",
		})

		dual := &model.User{Id: "dual", Roles: model.SystemAdminRoleId}
		state := p.applyTwoStepApproval(twoStepState{}, stepSecurity, dual, 100)
		require.False(t, state.complete(), "security+sysadmin must not complete both steps alone")
		require.Equal(t, "dual", state.SecurityApproverID)
		require.Empty(t, state.SystemApproverID)
	})

	t.Run("configured security attribute keeps two-person rule for system admin", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SecurityApproverAttribute: "security_approver"})

		admin := &model.User{Id: "admin", Roles: model.SystemAdminRoleId}
		state := p.applyTwoStepApproval(twoStepState{}, stepSecurity, admin, 100)
		require.False(t, state.complete())
		require.Equal(t, "admin", state.SecurityApproverID)
		require.Empty(t, state.SystemApproverID)
	})

	t.Run("healthy security pool keeps two-person rule", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{SecurityApproverAttribute: "security_approver"})
		mockApproverAttr(api, "security_approver", "sec", []*model.PropertyValue{{Value: json.RawMessage(`"yes"`)}})

		sec := &model.User{Id: "sec"}
		state := p.applyTwoStepApproval(twoStepState{}, stepSecurity, sec, 100)
		require.False(t, state.complete())
		require.Equal(t, "sec", state.SecurityApproverID)
		require.Empty(t, state.SystemApproverID)
	})

	t.Run("system_admin mode: solo system admin completes both steps", func(t *testing.T) {
		api := &plugintest.API{}
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{TwoStepApproverMode: twoStepModeSystemAdmin})

		admin := &model.User{Id: "admin", Roles: model.SystemAdminRoleId}
		state := p.applyTwoStepApproval(twoStepState{}, stepSecurity, admin, 100)
		require.True(t, state.complete())
		require.Equal(t, "admin", state.SecurityApproverID)
		require.Equal(t, "admin", state.SystemApproverID)
	})
}

func TestTwoStepMode(t *testing.T) {
	require.Equal(t, twoStepModeAttribute, (&configuration{}).twoStepMode())
	require.Equal(t, twoStepModeAttribute, (&configuration{TwoStepApproverMode: "attribute"}).twoStepMode())
	require.Equal(t, twoStepModeUserGroup, (&configuration{TwoStepApproverMode: "usergroup"}).twoStepMode())
	require.Equal(t, twoStepModeUserGroup, (&configuration{TwoStepApproverMode: "group"}).twoStepMode())
	require.Equal(t, twoStepModeSystemAdmin, (&configuration{TwoStepApproverMode: "system_admin"}).twoStepMode())
	require.Equal(t, twoStepModeSystemAdmin, (&configuration{TwoStepApproverMode: "sysadmin"}).twoStepMode())
}
