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

// twoStepMode returns the normalized approver mode. Unknown / empty → attribute.
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

// approverAttributeName returns the configured User Attribute name for the given
// step, trimmed. Empty means the admin hasn't configured that pool.
func (c *configuration) approverAttributeName(step approverStep) string {
	switch step {
	case stepSecurity:
		return strings.TrimSpace(c.SecurityApproverAttribute)
	case stepSystem:
		return strings.TrimSpace(c.SystemApproverAttribute)
	default:
		return ""
	}
}

// approverGroupName returns the configured Mattermost group name/handle for the
// given step (usergroup mode). Empty means fall back to System Admins for that step.
// Leading "@" is stripped so admins can paste either form from the UI.
func (c *configuration) approverGroupName(step approverStep) string {
	var raw string
	switch step {
	case stepSecurity:
		raw = c.SecurityApproverGroup
	case stepSystem:
		raw = c.SystemApproverGroup
	default:
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(raw), "@")
}

// canApproveStep reports whether the user may approve the given step of a
// two-step bot/webhook request. Behavior depends on TwoStepApproverMode:
//
//   - attribute: CPA / User Attribute holders (names are configurable).
//   - usergroup: members of the configured Mattermost groups.
//   - system_admin: System Admins only (intentional backup when ABAC/groups
//     are not available).
//
// Desired production flow (attribute / usergroup with a security pool name set):
//  1. Security step first — a security-pool member must review (System Admins do
//     NOT auto-qualify for security once a pool is configured). Dual-role users
//     (security + system) fill security on their first click.
//  2. System step second — a different System Admin OR system-pool member
//     approves (another dual-role security member may take this step).
//  3. Those two clicks must come from two distinct users.
//
// Soft fallback: if the security pool name is blank, System Admins may cover
// the security *step* so tickets are not stuck during setup — but attribute /
// usergroup mode still needs a second distinct click for the system step.
// Only system_admin mode lets one System Admin finish both in one click.
func (p *Plugin) canApproveStep(user *model.User, step approverStep) (bool, error) {
	if user == nil {
		return false, nil
	}
	// System Admins always qualify for the system step (role OR system group).
	if step == stepSystem && user.IsSystemAdmin() {
		return true, nil
	}

	cfg := p.getConfiguration()
	switch cfg.twoStepMode() {
	case twoStepModeSystemAdmin:
		return user.IsSystemAdmin(), nil
	case twoStepModeUserGroup:
		return p.canApproveStepViaGroup(user, step)
	default:
		return p.canApproveStepViaAttribute(user, step)
	}
}

func (p *Plugin) canApproveStepViaAttribute(user *model.User, step approverStep) (bool, error) {
	attrName := p.getConfiguration().approverAttributeName(step)
	if attrName == "" {
		return user.IsSystemAdmin(), nil
	}

	ok, err := p.userIsApprover(user, attrName)
	if err != nil {
		p.API.LogWarn("approver attribute check failed", "attr", attrName, "step", step.String(), "error", err.Error())
		return p.poolLookupSoftFallback(user, step), nil
	}
	if ok {
		return true, nil
	}
	if !p.approverAttributePoolReady(attrName) {
		return p.poolLookupSoftFallback(user, step), nil
	}
	return false, nil
}

func (p *Plugin) canApproveStepViaGroup(user *model.User, step approverStep) (bool, error) {
	groupName := p.getConfiguration().approverGroupName(step)
	if groupName == "" {
		return user.IsSystemAdmin(), nil
	}

	in, err := p.userInApproverGroup(user.Id, groupName)
	if err != nil {
		p.API.LogWarn("approver group check failed", "group", groupName, "step", step.String(), "error", err.Error())
		return p.poolLookupSoftFallback(user, step), nil
	}
	return in, nil
}

// poolLookupSoftFallback: once a security pool name is configured, System
// Admins must not cover the security step — that would skip the forced
// security review. System-step lookups may still fall back to System Admin.
func (p *Plugin) poolLookupSoftFallback(user *model.User, step approverStep) bool {
	if step == stepSecurity && p.getConfiguration().securityPoolConfigured() {
		return false
	}
	return user.IsSystemAdmin()
}

// userInApproverGroup reports whether userID belongs to the LDAP or
// plugin-synced group configured as groupName. It returns an error when that
// group can't be found, so callers don't treat a failed lookup as "not a
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

// securityPoolConfigured is true when attribute/usergroup mode has a non-empty
// security pool name — meaning a forced security review is required.
func (c *configuration) securityPoolConfigured() bool {
	switch c.twoStepMode() {
	case twoStepModeSystemAdmin:
		return false
	case twoStepModeUserGroup:
		return c.approverGroupName(stepSecurity) != ""
	default:
		return c.approverAttributeName(stepSecurity) != ""
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

	group, err := p.API.GetPropertyGroup(model.AccessControlPropertyGroupName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load access_control property group")
	}
	if group == nil {
		return nil, nil
	}

	// Custom profile attribute fields carry no target, so they're looked up by
	// name with an empty targetID.
	field, err := p.API.GetPropertyFieldByName(group.ID, "", attrName)
	if err != nil {
		// Most installs surface "not found" as an AppError — treat that as
		// pool-not-ready rather than a hard failure so Approve isn't blocked.
		if isNotFoundErr(err) {
			return nil, nil
		}
		return nil, errors.Wrapf(err, "failed to load approver attribute %q", attrName)
	}
	if field == nil {
		return nil, nil
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
