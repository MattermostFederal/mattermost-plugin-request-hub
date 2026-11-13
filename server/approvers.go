package main

import (
	"encoding/json"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// approverStep identifies which pool of a two-step request an approval is for.
// A two-step request requires one approval from EACH step, from two distinct
// users (see the engine in Phase 4's request handling).
type approverStep int

const (
	// stepSecurity is the security-team approval. It is gated purely on the
	// security attribute — System Admins do NOT auto-qualify, because the whole
	// point of this step is an independent security sign-off.
	stepSecurity approverStep = iota

	// stepSystem is the admin/dev approval. System Admins auto-qualify here (as
	// they do for every other approval in the plugin), in addition to anyone
	// holding the system approver attribute.
	stepSystem
)

func (s approverStep) String() string {
	if s == stepSecurity {
		return "security"
	}
	return "system"
}

// Two-step approver resolution modes (configuration.TwoStepApproverMode).
const (
	twoStepModeAttribute   = "attribute"
	twoStepModeUserGroup   = "usergroup"
	twoStepModeSystemAdmin = "system_admin"
)

// twoStepMode returns the normalized approver mode. Unknown / empty →
// attribute (shipped default; keep stable for installs that never saved a mode).
func (c *configuration) twoStepMode() string {
	switch strings.ToLower(strings.TrimSpace(c.TwoStepApproverMode)) {
	case twoStepModeUserGroup, "group", "groups", "user_group", "user-group":
		return twoStepModeUserGroup
	case twoStepModeSystemAdmin, "system-admin", "sysadmin", "admin":
		return twoStepModeSystemAdmin
	default:
		return twoStepModeAttribute
	}
}

// approverAttributeName returns the security CPA attribute name (attribute
// mode). The system step has no attribute — only System Admins. Blank config
// uses the shipped default so the pool is always required.
func (c *configuration) approverAttributeName(step approverStep) string {
	if step != stepSecurity {
		return ""
	}
	name := strings.TrimSpace(c.SecurityApproverAttribute)
	if name == "" {
		return defaultSecurityApproverAttribute
	}
	return name
}

// approverGroupName returns the security Mattermost group name (usergroup
// mode). Leading "@" is stripped. Blank config uses the shipped default so
// the pool is always required. The system step has no group.
func (c *configuration) approverGroupName(step approverStep) string {
	if step != stepSecurity {
		return ""
	}
	name := normalizeApproverGroupName(c.SecurityApproverGroup)
	if name == "" {
		return defaultSecurityApproverGroup
	}
	return name
}

// canApproveStep reports whether the user may approve the given step of a
// two-step bot/webhook request. Behavior depends on TwoStepApproverMode:
//
//   - attribute: security = holders of the configured CPA attribute.
//   - usergroup: security = members of the configured Mattermost group
//     (LDAP/plugin-synced, or custom when AllowCustomApproverGroups is on).
//   - system_admin: security = System Admins (setup backup).
//
// The system step is always System Admins — never a second group/attribute.
//
// Production flow (attribute / usergroup):
//  1. Security step first — a security-pool member must review (System Admins
//     do NOT auto-qualify). Dual-role users fill security on their first click.
//  2. System step second — a different System Admin approves.
//  3. Those two clicks must come from two distinct users.
//
// Only system_admin mode lets one System Admin finish both in one click.
func (p *Plugin) canApproveStep(user *model.User, step approverStep) (bool, error) {
	if user == nil {
		return false, nil
	}
	if step == stepSystem {
		return user.IsSystemAdmin(), nil
	}

	cfg := p.getConfiguration()
	switch cfg.twoStepMode() {
	case twoStepModeSystemAdmin:
		return user.IsSystemAdmin(), nil
	case twoStepModeUserGroup:
		return p.canApproveStepViaGroup(user, stepSecurity)
	default:
		return p.canApproveStepViaAttribute(user, stepSecurity)
	}
}

func (p *Plugin) canApproveStepViaAttribute(user *model.User, step approverStep) (bool, error) {
	if step != stepSecurity {
		return false, nil
	}
	attrName := p.getConfiguration().approverAttributeName(stepSecurity)
	if attrName == "" {
		return false, nil
	}

	ok, err := p.userIsApprover(user, attrName)
	if err != nil {
		p.API.LogWarn("approver attribute check failed", "attr", attrName, "step", step.String(), "error", err.Error())
		return false, nil
	}
	if ok {
		return true, nil
	}
	return false, nil
}

func (p *Plugin) canApproveStepViaGroup(user *model.User, step approverStep) (bool, error) {
	if step != stepSecurity {
		return false, nil
	}
	groupName := p.getConfiguration().approverGroupName(stepSecurity)
	if groupName == "" {
		return false, nil
	}

	in, err := p.userInApproverGroup(user.Id, groupName)
	if err != nil {
		p.API.LogWarn("approver group check failed", "group", groupName, "step", step.String(), "error", err.Error())
		return false, nil
	}
	return in, nil
}

// userInApproverGroup reports whether userID belongs to the configured
// Mattermost group (LDAP/plugin-synced, or custom when allowed). It returns an error when
// that group can't be found, so callers don't treat a failed lookup as "not a
// member".
func (p *Plugin) userInApproverGroup(userID, groupName string) (bool, error) {
	if userID == "" || normalizeApproverGroupName(groupName) == "" {
		return false, nil
	}
	group, err := p.resolveApproverGroup(groupName)
	if err != nil {
		return false, err
	}
	groups, appErr := p.API.GetGroupsForUser(userID)
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to list groups for user")
	}
	for _, g := range groups {
		if g != nil && g.Id == group.Id {
			return true, nil
		}
	}
	return false, nil
}

// securityPoolConfigured is true in attribute/usergroup mode: those modes
// always require a distinct security reviewer (pool name defaults if blank).
func (c *configuration) securityPoolConfigured() bool {
	switch c.twoStepMode() {
	case twoStepModeSystemAdmin:
		return false
	default:
		return true
	}
}

// approverAttributePoolReady reports whether the named attribute resolves to a
// trusted CPA field. Empty name, missing group/field, or untrusted field → false.
func (p *Plugin) approverAttributePoolReady(attrName string) bool {
	attrName = strings.TrimSpace(attrName)
	if attrName == "" {
		return false
	}
	field, err := p.loadApproverAttributeField(attrName)
	return err == nil && field != nil
}

// applyTwoStepApproval records the planned step. In attribute / usergroup mode a
// click always fills exactly one step — even when the actor is both a security
// pool member and a System Admin — so two distinct people are required.
// Only system_admin mode lets one System Admin stamp both steps in one click.
func (p *Plugin) applyTwoStepApproval(state twoStepState, step approverStep, user *model.User, atMillis int64) twoStepState {
	if user == nil {
		return state
	}
	newState := state.withApproval(step, user.Id, atMillis)
	if newState.complete() || !user.IsSystemAdmin() {
		return newState
	}
	if p.getConfiguration().twoStepMode() != twoStepModeSystemAdmin {
		return newState
	}
	other := stepSystem
	if step == stepSystem {
		other = stepSecurity
	}
	if newState.approverID(other) == "" {
		newState = newState.withApproval(other, user.Id, atMillis)
	}
	return newState
}

// loadApproverAttributeField resolves a trusted CPA field for attrName.
// Returns (nil, nil) when the group/field is simply missing; an error when the
// field exists but is user-writable or when CPA APIs fail unexpectedly.
func (p *Plugin) loadApproverAttributeField(attrName string) (*model.PropertyField, error) {
	attrName = strings.TrimSpace(attrName)
	if attrName == "" {
		return nil, nil
	}

	field, group, err := p.findAttributeField(attrName)
	if err != nil || field == nil {
		return nil, err
	}
	// User-writable CPA fields must not grant approval rights — only admin-managed,
	// LDAP/SAML-synced, or owner-managed fields are trusted.
	if !propertyFieldIsApproverTrusted(field) {
		return nil, errors.Errorf(
			"approver attribute %q must be admin-managed, LDAP/SAML-synced, or owner-managed (refusing user-writable field)",
			attrName,
		)
	}
	if field.GroupID == "" {
		field.GroupID = group.ID
	}
	return field, nil
}

// attributeGroupNames are the property groups custom profile attributes
// live in. Mattermost renamed custom_profile_attributes to access_control;
// servers before the rename (11.3 among them) only have the old group.
var attributeGroupNames = []string{model.AccessControlPropertyGroupName, model.DeprecatedCPAPropertyGroupName}

// findAttributeField looks attrName up in each attribute group in turn and
// returns the first match with its group, or nil when no group has it.
func (p *Plugin) findAttributeField(attrName string) (*model.PropertyField, *model.PropertyGroup, error) {
	for _, groupName := range attributeGroupNames {
		group, err := p.API.GetPropertyGroup(groupName)
		if err != nil {
			if isNotFoundErr(err) {
				continue
			}
			return nil, nil, errors.Wrapf(err, "failed to load %s property group", groupName)
		}
		if group == nil {
			continue
		}

		// Custom profile attribute fields carry no target, so they're looked
		// up by name with an empty targetID.
		field, err := p.API.GetPropertyFieldByName(group.ID, "", attrName)
		if err != nil {
			// Most installs surface "not found" as an AppError: treat that as
			// pool-not-ready rather than a hard failure so Approve isn't blocked.
			if isNotFoundErr(err) {
				continue
			}
			return nil, nil, errors.Wrapf(err, "failed to load approver attribute %q", attrName)
		}
		if field != nil {
			return field, group, nil
		}
	}
	return nil, nil, nil
}

// isNotFoundErr reports Mattermost AppErrors (and plain messages) that mean a
// CPA group/field simply isn't present.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if appErr, ok := err.(*model.AppError); ok {
		if appErr.StatusCode == 404 {
			return true
		}
		msg := strings.ToLower(appErr.Message + " " + appErr.Id)
		if strings.Contains(msg, "not found") || strings.Contains(msg, "not_found") {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "not_found")
}

// userIsApprover reports whether the user has a non-empty value for the named
// User Attribute (a custom profile attribute in the "access_control" property
// group). This is a pure attribute read — no role shortcuts — so it can be
// composed by canApproveStep. An empty attrName (pool not configured) is not an
// approver. Missing group/field is (false, nil); untrusted fields and unexpected
// CPA failures return an error for canApproveStep to soft-handle.
func (p *Plugin) userIsApprover(user *model.User, attrName string) (bool, error) {
	if user == nil {
		return false, nil
	}
	attrName = strings.TrimSpace(attrName)
	if attrName == "" {
		return false, nil
	}

	field, err := p.loadApproverAttributeField(attrName)
	if err != nil {
		return false, err
	}
	if field == nil {
		return false, nil
	}

	groupID := field.GroupID
	if groupID == "" {
		group, gErr := p.API.GetPropertyGroup(model.AccessControlPropertyGroupName)
		if gErr != nil {
			return false, errors.Wrap(gErr, "failed to load access_control property group")
		}
		if group == nil {
			return false, nil
		}
		groupID = group.ID
	}

	values, err := p.API.SearchPropertyValues(groupID, model.PropertyValueSearchOpts{
		GroupID:    groupID,
		TargetType: model.PropertyValueTargetTypeUser,
		TargetIDs:  []string{user.Id},
		FieldID:    field.ID,
		PerPage:    10,
	})
	if err != nil {
		return false, errors.Wrapf(err, "failed to read approver attribute %q", attrName)
	}

	for _, v := range values {
		if v != nil && attributeValueIsSet(v.Value, field) {
			return true, nil
		}
	}
	return false, nil
}

// attributeValueIsSet reports whether a CPA value marks the user as an approver.
//
// CPA values are JSON: "opt-id" (select), ["a","b"] (multiselect), "text", or a
// bool. For select/multiselect the stored string is an option ID — we resolve it
// to the option's display name via field metadata and only treat affirmative
// labels as set, so a "No" option cannot authorize. Text/bool keep non-empty /
// non-false-y rules. Prefer a Yes/No select (option named Yes) or an attribute
// present only for the approver pool.
func attributeValueIsSet(raw json.RawMessage, field *model.PropertyField) bool {
	if len(raw) == 0 {
		return false
	}

	if field != nil && field.Type.SupportsOptions() {
		return selectAttributeIsAffirmative(raw, field)
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return stringLooksAffirmative(s)
	}

	var arr []any
	if err := json.Unmarshal(raw, &arr); err == nil {
		return len(arr) > 0
	}

	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}

	return false
}

func selectAttributeIsAffirmative(raw json.RawMessage, field *model.PropertyField) bool {
	options := selectOptionsByID(field)
	if len(options) == 0 {
		return false
	}

	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		name, ok := options[id]
		return ok && stringLooksAffirmative(name)
	}

	var ids []string
	if err := json.Unmarshal(raw, &ids); err == nil {
		for _, optID := range ids {
			if name, ok := options[optID]; ok && stringLooksAffirmative(name) {
				return true
			}
		}
		return false
	}

	return false
}

func selectOptionsByID(field *model.PropertyField) map[string]string {
	out := map[string]string{}
	if field == nil {
		return out
	}
	cpa, err := model.NewCPAFieldFromPropertyField(field)
	if err != nil || cpa == nil {
		return out
	}
	for _, opt := range cpa.Attrs.Options {
		if opt == nil || opt.ID == "" {
			continue
		}
		out[opt.ID] = opt.Name
	}
	return out
}

func stringLooksAffirmative(s string) bool {
	t := strings.TrimSpace(strings.ToLower(s))
	return t != "" && t != "false" && t != "no" && t != "0" && t != "n" && t != "off"
}

// propertyFieldIsApproverTrusted reports whether a CPA field is safe to use as
// an approval gate. User-editable fields are rejected so requesters cannot
// grant themselves approval rights.
func propertyFieldIsApproverTrusted(field *model.PropertyField) bool {
	cpa, err := model.NewCPAFieldFromPropertyField(field)
	if err != nil || cpa == nil {
		return false
	}
	return cpa.IsAdminManaged() || cpa.IsSynced() || len(cpa.Attrs.Owners) > 0
}
