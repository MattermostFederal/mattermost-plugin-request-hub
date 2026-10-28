package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

const (
	// kvExerciseManifestPrefix namespaces the per-team record of what
	// exercise mode created. It doesn't start with kvRequestPrefix, so the
	// pending-request migration never reads it.
	kvExerciseManifestPrefix = "exercise_manifest_"

	// kvExercisePersonaPrefix records, by user ID, the accounts exercise
	// mode created and the team each was created for. It outlives
	// teardown, so populate can reactivate the same accounts. Only
	// accounts recorded here are treated as personas: users can edit their
	// own username and props, so neither proves an account is a persona.
	kvExercisePersonaPrefix = "exercise_persona_"

	exerciseStatePopulating  = "populating"
	exerciseStateReady       = "ready"
	exerciseStateTearingDown = "tearing_down"

	// exerciseStaleMillis is how long a populate can go without updating
	// the manifest before teardown treats it as stopped.
	exerciseStaleMillis = 5 * 60 * 1000

	// exerciseMarker labels everything exercise mode creates.
	exerciseMarker         = "EXERCISE"
	exerciseDisplayPrefix  = exerciseMarker + " "
	exercisePurposePrefix  = "[" + exerciseMarker + "] "
	exercisePositionPrefix = exerciseMarker + ": "

	// exerciseBanner is the header of every exercise channel.
	exerciseBanner = "**EXERCISE EXERCISE EXERCISE**: this channel is exercise data from Mattermost Request Hub. Teardown archives it."

	exerciseRemovedText = exerciseMarker + ": removed by teardown."

	// The populate refusals when the request types it seeds are turned off.
	exerciseChannelRequestsOffText = "exercise mode seeds channel requests, which are turned off. Turn on **Allow channel requests** in System Console → Plugins → Mattermost Request Hub, then run populate again"

	// exerciseEmailDomain is a reserved domain (RFC 2606), so mail to a
	// persona goes nowhere.
	exerciseEmailDomain = "exercise.invalid"

	// exerciseTeamIDChars is how much of the team ID a persona username
	// carries, keeping each team's personas apart.
	exerciseTeamIDChars = 8

	// Scenario size limits, so a pasted scenario can't make populate run
	// for minutes.
	maxExercisePersonas   = 50
	maxExerciseOperations = 200

	manifestUpdateAttempts = 5
)

const (
	exerciseKindChannel        = "channel"
	exerciseKindChannelRequest = "channel_request"
	exerciseKindAdminRequest   = "admin_request"
	exerciseKindTeamRequest    = "team_request"
	exerciseKindTeamAdmin      = "team_admin_request"
	exerciseKindTeamMember     = "team_member_request"
	exerciseKindTeamGuest      = "team_guest_request"
	exerciseKindBotRequest     = "bot_request"
	exerciseKindWebhookRequest = "webhook_request"
)

const (
	// exerciseTeamNamePrefix starts the URL name of every exercise team.
	exerciseTeamNamePrefix = "exercise-"
	// exerciseBotPrefix starts every exercise bot username. It is short
	// because bot usernames are short.
	exerciseBotPrefix = "ex-"
	// exerciseBotTeamIDChars is how much of the team ID a bot username
	// carries.
	exerciseBotTeamIDChars = 6
	// exerciseGuestEmailPrefix starts the address of every exercise guest.
	exerciseGuestEmailPrefix = "exercise-guest-"
	// exerciseCallbackURL is the callback of every exercise outgoing
	// webhook. The domain is reserved, so nothing is ever called.
	exerciseCallbackURL = "https://" + exerciseEmailDomain + "/hook"
)

// builtInExerciseScenario is a fictional scenario that shows the
// mechanics. Scenarios with real names are supplied on the server through
// the ExerciseScenarioJSON setting, never shipped in the plugin.
//
//go:embed exercise/silver_harbor.json
var builtInExerciseScenario []byte

var (
	// errNoExerciseManifest means the team has no exercise data recorded.
	errNoExerciseManifest = errors.New("no exercise data recorded for this team")
	errExerciseExists     = errors.New("exercise data already exists for this team, or a populate is running. Run `/mattermost-request-hub exercise status` to see it, or `/mattermost-request-hub exercise teardown` to remove it")
	// errExerciseStopped stops a populate once teardown has started.
	errExerciseStopped = errors.New("teardown started while seeding")
)

// personaTakenError means a persona's username belongs to an account
// exercise mode didn't create.
type personaTakenError struct{ username string }

func (e *personaTakenError) Error() string {
	return fmt.Sprintf("the username @%s is taken by an account that isn't exercise data", e.username)
}

func isPersonaTaken(err error) bool {
	var taken *personaTakenError
	return errors.As(err, &taken)
}

// exerciseScenario is the JSON form of a scenario.
type exerciseScenario struct {
	ID         string                  `json:"id"`
	Name       string                  `json:"name"`
	Personas   []exercisePersonaSpec   `json:"personas"`
	Operations []exerciseOperationSpec `json:"operations"`
}

type exercisePersonaSpec struct {
	Role      string `json:"role"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Position  string `json:"position"`
	// OffTeam keeps the persona out of the team, so a team member request
	// can add them.
	OffTeam bool `json:"off_team,omitempty"`
}

// exerciseOperationSpec is one step of a scenario. Persona fields hold
// roles; Channel, Channels, IncomingChannel and OutgoingChannel hold the
// operation IDs of channel operations.
type exerciseOperationSpec struct {
	Act             int      `json:"act"`
	OperationID     string   `json:"operation_id"`
	Kind            string   `json:"kind"`
	Suffix          string   `json:"suffix,omitempty"`
	DisplayName     string   `json:"display_name,omitempty"`
	Purpose         string   `json:"purpose,omitempty"`
	Private         bool     `json:"private,omitempty"`
	Requester       string   `json:"requester"`
	Members         []string `json:"members,omitempty"`
	Admins          []string `json:"admins,omitempty"`
	AgeDays         int      `json:"age_days,omitempty"`
	Channel         string   `json:"channel,omitempty"`
	Nominees        []string `json:"nominees,omitempty"`
	Reason          string   `json:"reason,omitempty"`
	TeamAdmin       bool     `json:"team_admin,omitempty"`
	Guest           string   `json:"guest,omitempty"`
	Channels        []string `json:"channels,omitempty"`
	Owner           string   `json:"owner,omitempty"`
	Token           bool     `json:"token,omitempty"`
	IncomingChannel string   `json:"incoming_channel,omitempty"`
	OutgoingChannel string   `json:"outgoing_channel,omitempty"`
	WebhookType     string   `json:"webhook_type,omitempty"`
}

// exerciseRequestType describes one request type a plan seeds.
type exerciseRequestType struct {
	kind string
	// plural names the type, as in "channel requests".
	plural string
	// setting is the System Console setting that turns the type on.
	setting string
	// specIDs are the IDs of the plan's requests of this type.
	specIDs []string
}

// requestTypes lists every request type in plan order, with the plan's
// requests of each.
func (plan *exercisePlan) requestTypes() []exerciseRequestType {
	ids := func(n int, id func(int) string) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, id(i))
		}
		return out
	}
	return []exerciseRequestType{
		{
			pendingKindChannel, "channel requests", "Allow channel requests",
			ids(len(plan.ChannelRequests), func(i int) string { return plan.ChannelRequests[i].ID }),
		},
		{
			pendingKindAdmin, "Channel Admin requests", "Allow channel admin requests",
			ids(len(plan.AdminRequests), func(i int) string { return plan.AdminRequests[i].ID }),
		},
		{
			pendingKindTeam, "team creation requests", "Allow team requests",
			ids(len(plan.TeamRequests), func(i int) string { return plan.TeamRequests[i].ID }),
		},
		{
			pendingKindTeamAdmin, "Team Admin requests", "Allow team admin requests",
			ids(len(plan.TeamAdminRequests), func(i int) string { return plan.TeamAdminRequests[i].ID }),
		},
		{
			pendingKindTeamMember, "team member requests", "Allow team member requests",
			ids(len(plan.TeamMemberRequests), func(i int) string { return plan.TeamMemberRequests[i].ID }),
		},
		{
			pendingKindTeamGuest, "guest invite requests", "Allow team guest requests",
			ids(len(plan.TeamGuestRequests), func(i int) string { return plan.TeamGuestRequests[i].ID }),
		},
		{
			pendingKindBot, "bot account requests", "Allow bot account requests",
			ids(len(plan.BotRequests), func(i int) string { return plan.BotRequests[i].ID }),
		},
		{
			pendingKindWebhook, "webhook requests", "Allow webhook requests",
			ids(len(plan.WebhookRequests), func(i int) string { return plan.WebhookRequests[i].ID }),
		},
	}
}

// alwaysShown reports whether populate and status count the type even
// when the scenario seeds none: channel and Channel Admin requests.
func (t exerciseRequestType) alwaysShown() bool {
	return t.kind == pendingKindChannel || t.kind == pendingKindAdmin
}

// exerciseTypeOffText refuses a populate whose scenario seeds a request
// type that is turned off.
func exerciseTypeOffText(t exerciseRequestType) string {
	return fmt.Sprintf("this scenario seeds %s, which are turned off. Turn on **%s** in System Console → Plugins → Mattermost Request Hub, or use a scenario without them", t.plural, t.setting)
}

// exercisePlan is everything populate will create for one team, with
// every name, request ID and timestamp already decided.
type exercisePlan struct {
	Scenario        string                   `json:"scenario"`
	TeamID          string                   `json:"team_id"`
	Personas        []exercisePersona        `json:"personas"`
	Channels        []exerciseChannel        `json:"channels"`
	ChannelRequests []exerciseChannelRequest `json:"channel_requests"`
	AdminRequests   []exerciseAdminRequest   `json:"admin_requests"`

	TeamRequests       []exerciseTeamRequest    `json:"team_requests,omitempty"`
	TeamAdminRequests  []exerciseNomineeRequest `json:"team_admin_requests,omitempty"`
	TeamMemberRequests []exerciseNomineeRequest `json:"team_member_requests,omitempty"`
	TeamGuestRequests  []exerciseGuestRequest   `json:"team_guest_requests,omitempty"`
	BotRequests        []exerciseBotRequest     `json:"bot_requests,omitempty"`
	WebhookRequests    []exerciseWebhookRequest `json:"webhook_requests,omitempty"`
}

type exercisePersona struct {
	Role      string `json:"role"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Position  string `json:"position"`
	OffTeam   bool   `json:"off_team,omitempty"`
}

type exerciseChannel struct {
	OpKey       string   `json:"op_key"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Purpose     string   `json:"purpose"`
	ChannelType string   `json:"channel_type"`
	Creator     string   `json:"creator"`
	Members     []string `json:"members"`
	Admins      []string `json:"admins"`
}

type exerciseChannelRequest struct {
	OpKey       string   `json:"op_key"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Purpose     string   `json:"purpose"`
	ChannelType string   `json:"channel_type"`
	Requester   string   `json:"requester"`
	Members     []string `json:"members"`
	Admins      []string `json:"admins"`
	CreatedAt   int64    `json:"created_at"`
}

type exerciseAdminRequest struct {
	OpKey        string   `json:"op_key"`
	ID           string   `json:"id"`
	ChannelOpKey string   `json:"channel_op_key"`
	Requester    string   `json:"requester"`
	Nominees     []string `json:"nominees"`
	CreatedAt    int64    `json:"created_at"`
}

// exerciseTeamRequest is a planned team creation request. Name is the
// team's URL name.
type exerciseTeamRequest struct {
	OpKey       string `json:"op_key"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Requester   string `json:"requester"`
	TeamAdmin   bool   `json:"team_admin,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// exerciseNomineeRequest is a planned Team Admin or team member request
// for the exercise team.
type exerciseNomineeRequest struct {
	OpKey     string   `json:"op_key"`
	ID        string   `json:"id"`
	Requester string   `json:"requester"`
	Nominees  []string `json:"nominees"`
	Reason    string   `json:"reason,omitempty"`
	CreatedAt int64    `json:"created_at"`
}

type exerciseGuestRequest struct {
	OpKey         string   `json:"op_key"`
	ID            string   `json:"id"`
	Requester     string   `json:"requester"`
	Email         string   `json:"email"`
	ChannelOpKeys []string `json:"channel_op_keys"`
	Reason        string   `json:"reason,omitempty"`
	CreatedAt     int64    `json:"created_at"`
}

type exerciseBotRequest struct {
	OpKey                string `json:"op_key"`
	ID                   string `json:"id"`
	Username             string `json:"username"`
	DisplayName          string `json:"display_name"`
	Description          string `json:"description"`
	Requester            string `json:"requester"`
	Owner                string `json:"owner,omitempty"`
	Token                bool   `json:"token,omitempty"`
	IncomingChannelOpKey string `json:"incoming_channel_op_key,omitempty"`
	OutgoingChannelOpKey string `json:"outgoing_channel_op_key,omitempty"`
	CreatedAt            int64  `json:"created_at"`
}

type exerciseWebhookRequest struct {
	OpKey        string `json:"op_key"`
	ID           string `json:"id"`
	WebhookType  string `json:"webhook_type"`
	ChannelOpKey string `json:"channel_op_key"`
	DisplayName  string `json:"display_name"`
	Description  string `json:"description"`
	Requester    string `json:"requester"`
	CreatedAt    int64  `json:"created_at"`
}

// exerciseManifest records a team's exercise data. It is claimed before
// populate writes anything, so it always names everything that may exist.
type exerciseManifest struct {
	CreatedBy string        `json:"created_by"`
	CreatedAt int64         `json:"created_at"`
	State     string        `json:"state"`
	Plan      *exercisePlan `json:"plan"`
	// UpdatedAt is refreshed by every write, so teardown can tell a
	// running populate from one that stopped.
	UpdatedAt int64 `json:"updated_at"`
	// Done maps each finished operation's key to the ID it created.
	Done map[string]string `json:"done"`
	// ChannelIDs lists every exercise channel, seeded or created by an
	// approval, for teardown to archive.
	ChannelIDs []string `json:"channel_ids"`
	// KVKeys lists the reply-forwarding lookups and approval card records
	// of the seeded requests, which outlive a decision, for teardown to
	// delete.
	KVKeys []string `json:"kv_keys,omitempty"`
	// TeamIDs, BotIDs and Webhooks list what approving exercise requests
	// created, for teardown to remove.
	TeamIDs  []string          `json:"team_ids,omitempty"`
	BotIDs   []string          `json:"bot_ids,omitempty"`
	Webhooks []exerciseWebhook `json:"webhooks,omitempty"`
}

// exerciseWebhook is a webhook created by approving an exercise request.
type exerciseWebhook struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func exerciseManifestKey(teamID string) string {
	return kvExerciseManifestPrefix + teamID
}

// exerciseOpKey identifies an operation by scenario, act and operation ID,
// so replaying an act never creates it twice.
func exerciseOpKey(scenario string, act int, operationID string) string {
	return fmt.Sprintf("%s/%d/%s", scenario, act, operationID)
}

// exercisePersonaUsername includes part of the team ID so each team gets
// its own personas.
func exercisePersonaUsername(role, teamID string) string {
	return "exercise-" + role + "-" + teamID[:min(len(teamID), exerciseTeamIDChars)]
}

// loadExerciseScenario parses the scenario from the ExerciseScenarioJSON
// setting, or the built-in one when the setting is blank.
func loadExerciseScenario(configured string) (*exerciseScenario, error) {
	data := builtInExerciseScenario
	if strings.TrimSpace(configured) != "" {
		data = []byte(configured)
	}
	var sc exerciseScenario
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, errors.New("the exercise scenario in the plugin settings is not valid JSON")
	}
	if sc.ID == "" || sc.Name == "" {
		return nil, errors.New("the exercise scenario needs an id and a name")
	}
	return &sc, nil
}

// buildExercisePlan turns a scenario into the plan for one team. It makes
// no API calls: names go through the same prefix rules as real requests,
// and request IDs come from newID, so a test can fix them. Channel names
// use the first configured prefix.
func buildExercisePlan(sc *exerciseScenario, teamID string, prefixes []channelPrefix, nowMs int64, newID func() string) (*exercisePlan, error) {
	if len(prefixes) == 0 {
		return nil, errors.New(notConfiguredMessage)
	}
	if len(sc.Personas) > maxExercisePersonas || len(sc.Operations) > maxExerciseOperations {
		return nil, errors.Errorf("a scenario can have at most %d personas and %d operations", maxExercisePersonas, maxExerciseOperations)
	}
	prefix := prefixes[0].Prefix
	teamTail := "-" + teamID[:min(len(teamID), exerciseTeamIDChars)]

	plan := &exercisePlan{Scenario: sc.ID, TeamID: teamID}
	roles := map[string]bool{}
	offTeam := map[string]bool{}
	for _, ps := range sc.Personas {
		if roles[ps.Role] {
			return nil, errors.Errorf("persona %q is listed twice", ps.Role)
		}
		roles[ps.Role] = true
		offTeam[ps.Role] = ps.OffTeam
		username := exercisePersonaUsername(ps.Role, teamID)
		if !model.IsValidUsername(username) {
			return nil, errors.Errorf("persona %q gives the invalid username %q", ps.Role, username)
		}
		if utf8.RuneCountInString(ps.FirstName) > model.UserFirstNameMaxRunes || utf8.RuneCountInString(ps.LastName) > model.UserLastNameMaxRunes {
			return nil, errors.Errorf("persona %q has a first or last name longer than %d characters", ps.Role, model.UserFirstNameMaxRunes)
		}
		position := exercisePositionPrefix + ps.Position
		if utf8.RuneCountInString(position) > model.UserPositionMaxRunes {
			return nil, errors.Errorf("persona %q has a position longer than %d characters", ps.Role, model.UserPositionMaxRunes-len(exercisePositionPrefix))
		}
		plan.Personas = append(plan.Personas, exercisePersona{
			Role:      ps.Role,
			Username:  username,
			Email:     username + "@" + exerciseEmailDomain,
			FirstName: ps.FirstName,
			LastName:  ps.LastName,
			Position:  position,
			OffTeam:   ps.OffTeam,
		})
	}

	checkRoles := func(op exerciseOperationSpec, list []string) error {
		if len(list) > maxMembersPerList {
			return errors.Errorf("operation %q names more than %d personas in one list", op.OperationID, maxMembersPerList)
		}
		for _, r := range list {
			if !roles[r] {
				return errors.Errorf("operation %q names the unknown persona %q", op.OperationID, r)
			}
		}
		return nil
	}
	// onTeam also refuses off-team personas, for lists that need team
	// members.
	onTeam := func(op exerciseOperationSpec, list []string) error {
		if err := checkRoles(op, list); err != nil {
			return err
		}
		for _, r := range list {
			if offTeam[r] {
				return errors.Errorf("operation %q needs persona %q on the team, but it is off_team", op.OperationID, r)
			}
		}
		return nil
	}
	nominees := func(op exerciseOperationSpec, check func(exerciseOperationSpec, []string) error) error {
		if len(op.Nominees) == 0 {
			return errors.Errorf("operation %q names no nominees", op.OperationID)
		}
		return check(op, op.Nominees)
	}

	seenOps := map[string]bool{}
	channelOps := map[string]exerciseChannel{} // operation ID -> channel
	// channelRef returns the earlier channel operation ref names, which
	// member must belong to.
	channelRef := func(op exerciseOperationSpec, ref, member string) (exerciseChannel, error) {
		ch, ok := channelOps[ref]
		if !ok {
			return ch, errors.Errorf("operation %q needs an earlier channel operation %q", op.OperationID, ref)
		}
		if member != ch.Creator && !slices.Contains(ch.Members, member) && !slices.Contains(ch.Admins, member) {
			return ch, errors.Errorf("operation %q needs persona %q in channel operation %q", op.OperationID, member, ref)
		}
		return ch, nil
	}
	names := map[string]bool{}
	unique := func(op exerciseOperationSpec, what, value string) error {
		key := what + "\x00" + value
		if names[key] {
			return errors.Errorf("operation %q reuses the %s %q", op.OperationID, what, value)
		}
		names[key] = true
		return nil
	}
	for _, op := range sc.Operations {
		key := exerciseOpKey(sc.ID, op.Act, op.OperationID)
		if op.OperationID == "" || seenOps[op.OperationID] {
			return nil, errors.Errorf("operation %q needs a unique ID", op.OperationID)
		}
		seenOps[op.OperationID] = true
		if op.AgeDays < 0 || op.AgeDays > maxRequestExpiryDays {
			return nil, errors.Errorf("operation %q needs an age_days from 0 to %d", op.OperationID, maxRequestExpiryDays)
		}
		// Only a team member request and a team creation request may
		// come from someone off the team.
		requesterCheck := onTeam
		if op.Kind == exerciseKindTeamMember || op.Kind == exerciseKindTeamRequest {
			requesterCheck = checkRoles
		}
		if err := requesterCheck(op, []string{op.Requester}); err != nil {
			return nil, err
		}
		op.Nominees, op.Channels = dedupeNonEmpty(op.Nominees), dedupeNonEmpty(op.Channels)
		createdAt := nowMs - int64(op.AgeDays)*millisPerDay
		displayName := exerciseDisplayPrefix + strings.Join(strings.Fields(op.DisplayName), " ")
		purpose := exercisePurposePrefix + op.Purpose

		switch op.Kind {
		case exerciseKindChannel, exerciseKindChannelRequest:
			if err := onTeam(op, op.Members); err != nil {
				return nil, err
			}
			if err := onTeam(op, op.Admins); err != nil {
				return nil, err
			}
			if err := validateRequestInput(requestInput{TeamID: teamID, DisplayName: displayName, Purpose: purpose}); err != nil {
				return nil, errors.Wrapf(err, "operation %q", op.OperationID)
			}
			name, err := resolvePrefixedName(prefixes, requestInput{Prefix: prefix, Name: op.Suffix, DisplayName: op.DisplayName})
			if err != nil {
				return nil, errors.Wrapf(err, "operation %q", op.OperationID)
			}
			if err := unique(op, "channel name", name); err != nil {
				return nil, err
			}
			channelType := channelTypeOpen
			if op.Private {
				channelType = channelTypePrivate
			}
			if op.Kind == exerciseKindChannel {
				ch := exerciseChannel{
					OpKey: key, Name: name, DisplayName: displayName, Purpose: purpose, ChannelType: channelType,
					Creator: op.Requester, Members: op.Members, Admins: op.Admins,
				}
				channelOps[op.OperationID] = ch
				plan.Channels = append(plan.Channels, ch)
				continue
			}
			plan.ChannelRequests = append(plan.ChannelRequests, exerciseChannelRequest{
				OpKey: key, ID: newID(), Name: name, DisplayName: displayName, Purpose: purpose, ChannelType: channelType,
				Requester: op.Requester, Members: op.Members, Admins: op.Admins, CreatedAt: createdAt,
			})
		case exerciseKindAdminRequest:
			ch, err := channelRef(op, op.Channel, op.Requester)
			if err != nil {
				return nil, err
			}
			if err := nominees(op, onTeam); err != nil {
				return nil, err
			}
			plan.AdminRequests = append(plan.AdminRequests, exerciseAdminRequest{
				OpKey: key, ID: newID(), ChannelOpKey: ch.OpKey, Requester: op.Requester, Nominees: op.Nominees, CreatedAt: createdAt,
			})
		case exerciseKindTeamRequest:
			name := exerciseTeamNamePrefix + slugify(op.Suffix) + teamTail
			if slugify(op.Suffix) == "" || len(name) > model.TeamNameMaxLength || !model.IsValidTeamName(name) || model.IsReservedTeamName(name) {
				return nil, errors.Errorf("operation %q gives the invalid team URL name %q", op.OperationID, name)
			}
			if err := unique(op, "team URL name", name); err != nil {
				return nil, err
			}
			if err := errors.Wrapf(firstError(
				checkTextLen("team name", displayName, model.TeamDisplayNameMaxRunes),
				checkByteLen("description", purpose, maxTeamDescriptionBytes),
			), "operation %q", op.OperationID); err != nil {
				return nil, err
			}
			teamType := model.TeamOpen
			if op.Private {
				teamType = model.TeamInvite
			}
			plan.TeamRequests = append(plan.TeamRequests, exerciseTeamRequest{
				OpKey: key, ID: newID(), Name: name, DisplayName: displayName, Description: purpose, Type: teamType,
				Requester: op.Requester, TeamAdmin: op.TeamAdmin, CreatedAt: createdAt,
			})
		case exerciseKindTeamAdmin, exerciseKindTeamMember:
			check, list := onTeam, &plan.TeamAdminRequests
			if op.Kind == exerciseKindTeamMember {
				check, list = checkRoles, &plan.TeamMemberRequests
			}
			if err := nominees(op, check); err != nil {
				return nil, err
			}
			if err := errors.Wrapf(checkTextLen("reason", op.Reason, maxReasonLen), "operation %q", op.OperationID); err != nil {
				return nil, err
			}
			*list = append(*list, exerciseNomineeRequest{
				OpKey: key, ID: newID(), Requester: op.Requester, Nominees: op.Nominees, Reason: op.Reason, CreatedAt: createdAt,
			})
		case exerciseKindTeamGuest:
			email := exerciseGuestEmailPrefix + slugify(op.Guest) + teamTail + "@" + exerciseEmailDomain
			if slugify(op.Guest) == "" || len(email) > maxEmailLen || !model.IsValidEmail(email) {
				return nil, errors.Errorf("operation %q needs a guest label that makes a valid email address", op.OperationID)
			}
			if err := unique(op, "guest", email); err != nil {
				return nil, err
			}
			if len(op.Channels) == 0 || len(op.Channels) > maxMembersPerList {
				return nil, errors.Errorf("operation %q needs 1 to %d channels", op.OperationID, maxMembersPerList)
			}
			var channelKeys []string
			for _, ref := range op.Channels {
				ch, err := channelRef(op, ref, op.Requester)
				if err != nil {
					return nil, err
				}
				channelKeys = append(channelKeys, ch.OpKey)
			}
			if err := errors.Wrapf(checkTextLen("reason", op.Reason, maxReasonLen), "operation %q", op.OperationID); err != nil {
				return nil, err
			}
			plan.TeamGuestRequests = append(plan.TeamGuestRequests, exerciseGuestRequest{
				OpKey: key, ID: newID(), Requester: op.Requester, Email: email, ChannelOpKeys: channelKeys, Reason: op.Reason, CreatedAt: createdAt,
			})
		case exerciseKindBotRequest:
			username := exerciseBotPrefix + strings.ToLower(op.Suffix) + "-" + teamID[:min(len(teamID), exerciseBotTeamIDChars)]
			if op.Suffix == "" || len(username) < minBotUsernameLen || len(username) > maxBotUsernameLen || !model.IsValidUsername(username) {
				return nil, errors.Errorf("operation %q gives the invalid bot username %q (%d to %d characters)", op.OperationID, username, minBotUsernameLen, maxBotUsernameLen)
			}
			if err := unique(op, "bot username", username); err != nil {
				return nil, err
			}
			if op.Owner != "" {
				if err := checkRoles(op, []string{op.Owner}); err != nil {
					return nil, err
				}
			}
			if err := errors.Wrapf(firstError(
				checkTextLen("display name", displayName, maxDisplayNameLen),
				checkTextLen("description", purpose, maxDescriptionLen),
			), "operation %q", op.OperationID); err != nil {
				return nil, err
			}
			bot := exerciseBotRequest{
				OpKey: key, ID: newID(), Username: username, DisplayName: displayName, Description: purpose,
				Requester: op.Requester, Owner: op.Owner, Token: op.Token, CreatedAt: createdAt,
			}
			if op.IncomingChannel != "" {
				ch, err := channelRef(op, op.IncomingChannel, op.Requester)
				if err != nil {
					return nil, err
				}
				bot.IncomingChannelOpKey = ch.OpKey
			}
			if op.OutgoingChannel != "" {
				ch, err := channelRef(op, op.OutgoingChannel, op.Requester)
				if err != nil {
					return nil, err
				}
				if ch.ChannelType != channelTypeOpen {
					return nil, errors.Errorf("operation %q needs a public channel for its outgoing webhook", op.OperationID)
				}
				bot.OutgoingChannelOpKey = ch.OpKey
			}
			plan.BotRequests = append(plan.BotRequests, bot)
		case exerciseKindWebhookRequest:
			if op.WebhookType != webhookTypeIncoming && op.WebhookType != webhookTypeOutgoing {
				return nil, errors.Errorf("operation %q needs a webhook_type of %q or %q", op.OperationID, webhookTypeIncoming, webhookTypeOutgoing)
			}
			ch, err := channelRef(op, op.Channel, op.Requester)
			if err != nil {
				return nil, err
			}
			if op.WebhookType == webhookTypeOutgoing && ch.ChannelType != channelTypeOpen {
				return nil, errors.Errorf("operation %q needs a public channel for an outgoing webhook", op.OperationID)
			}
			if err := errors.Wrapf(firstError(
				checkByteLen("display name", displayName, maxWebhookNameBytes),
				checkByteLen("description", purpose, maxWebhookDescriptionBytes),
			), "operation %q", op.OperationID); err != nil {
				return nil, err
			}
			plan.WebhookRequests = append(plan.WebhookRequests, exerciseWebhookRequest{
				OpKey: key, ID: newID(), WebhookType: op.WebhookType, ChannelOpKey: ch.OpKey,
				DisplayName: displayName, Description: purpose, Requester: op.Requester, CreatedAt: createdAt,
			})
		default:
			return nil, errors.Errorf("operation %q has the unknown kind %q", op.OperationID, op.Kind)
		}
	}
	return plan, nil
}

// firstError returns the first non-nil error.
func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// loadExerciseManifest returns the team's manifest and its stored bytes,
// or errNoExerciseManifest. A manifest without a plan reads as an empty
// plan, so teardown can still clear it.
func (p *Plugin) loadExerciseManifest(teamID string) (*exerciseManifest, []byte, error) {
	data, appErr := p.API.KVGet(exerciseManifestKey(teamID))
	if appErr != nil {
		return nil, nil, p.exerciseFailure("could not load the exercise record", appErr)
	}
	if data == nil {
		return nil, nil, errNoExerciseManifest
	}
	var m exerciseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, errors.New("the exercise record for this team is unreadable")
	}
	if m.Plan == nil {
		m.Plan = &exercisePlan{TeamID: teamID}
	}
	return &m, data, nil
}

// updateExerciseManifest applies change to the team's manifest with a
// compare-and-set, retrying when another writer got there first. A
// change that returns an error aborts without writing.
func (p *Plugin) updateExerciseManifest(teamID string, change func(*exerciseManifest) error) error {
	for range manifestUpdateAttempts {
		m, raw, err := p.loadExerciseManifest(teamID)
		if err != nil {
			return err
		}
		if m.Done == nil {
			m.Done = map[string]string{}
		}
		if err = change(m); err != nil {
			return err
		}
		m.UpdatedAt = model.GetMillis()
		data, _ := json.Marshal(m) // the manifest holds only strings, numbers and slices
		ok, appErr := p.API.KVCompareAndSet(exerciseManifestKey(teamID), raw, data)
		if appErr != nil {
			return p.exerciseFailure("could not save the exercise record", appErr)
		}
		if ok {
			return nil
		}
	}
	return errors.New("the exercise record kept changing")
}

// recordExerciseEffect adds what approving an exercise request created
// to teamID's manifest, so teardown removes it too. When the exercise was
// already torn down, undo removes it at once and recordExerciseEffect
// reports false. what and id name it in the logs.
func (p *Plugin) recordExerciseEffect(teamID, what, id string, change func(*exerciseManifest), undo func() error) bool {
	err := p.updateExerciseManifest(teamID, func(m *exerciseManifest) error {
		change(m)
		return nil
	})
	if errors.Is(err, errNoExerciseManifest) {
		if undoErr := undo(); undoErr != nil {
			p.API.LogWarn("failed to remove exercise "+what+" after teardown; remove it by hand", "id", id, "error", undoErr.Error())
		}
		return false
	}
	if err != nil {
		p.API.LogWarn("failed to record exercise "+what+"; remove it by hand", "id", id, "error", err.Error())
	}
	return true
}

// recordExerciseChannel records a channel created by approving an
// exercise request and marks its header.
func (p *Plugin) recordExerciseChannel(channel *model.Channel) {
	recorded := p.recordExerciseEffect(channel.TeamId, "channel", channel.Id, func(m *exerciseManifest) {
		m.ChannelIDs = append(m.ChannelIDs, channel.Id)
	}, func() error {
		_, err := p.archiveExerciseChannel(channel.Id)
		return err
	})
	if !recorded {
		return
	}
	channel.Header = exerciseBanner
	if _, appErr := p.API.UpdateChannel(channel); appErr != nil {
		p.API.LogWarn("failed to mark exercise channel", "channel_id", channel.Id, "error", appErr.Error())
	}
}

// recordExerciseTeam records a team created by approving an exercise
// request, reporting false when it was removed again instead.
func (p *Plugin) recordExerciseTeam(exerciseTeamID string, team *model.Team) bool {
	return p.recordExerciseEffect(exerciseTeamID, "team", team.Id, func(m *exerciseManifest) {
		m.TeamIDs = append(m.TeamIDs, team.Id)
	}, func() error {
		_, err := p.archiveExerciseTeam(team.Id)
		return err
	})
}

// recordExerciseBot records a bot created by approving an exercise
// request, before anything is made for it, reporting false when it was
// removed again instead.
func (p *Plugin) recordExerciseBot(exerciseTeamID, botUserID string) bool {
	return p.recordExerciseEffect(exerciseTeamID, "bot", botUserID, func(m *exerciseManifest) {
		m.BotIDs = append(m.BotIDs, botUserID)
	}, func() error {
		_, err := p.deleteExerciseBot(botUserID)
		return err
	})
}

// recordExerciseWebhook records a webhook created by approving an
// exercise request, or made for an exercise bot, reporting false when it
// was removed again instead. actorID is the approving admin, whose
// session removes it.
func (p *Plugin) recordExerciseWebhook(teamID string, hook exerciseWebhook, actorID string) bool {
	return p.recordExerciseEffect(teamID, "webhook", hook.ID, func(m *exerciseManifest) {
		m.Webhooks = append(m.Webhooks, hook)
	}, func() error {
		_, err := p.deleteExerciseWebhook(hook, actorID)
		return err
	})
}

// exerciseLabel prefixes audit lines and card titles about exercise
// requests.
func exerciseLabel(exercise bool) string {
	if exercise {
		return exerciseDisplayPrefix
	}
	return ""
}

// cardMention opens a two-step card's header. Exercise cards mention
// nobody, so seeding doesn't notify the channel.
func cardMention(exercise bool) string {
	if exercise {
		return exerciseMarker + ": "
	}
	return "@channel — "
}

// exercisePassword returns a random password that is never stored or
// shown, so nobody can sign in as a persona. The fixed start meets any
// password policy.
func exercisePassword() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return "Ex1!" + base64.RawURLEncoding.EncodeToString(b)
}

func isNotFound(appErr *model.AppError) bool {
	return appErr != nil && appErr.StatusCode == http.StatusNotFound
}

// exerciseFailure logs an API failure in full and returns a short error
// for the reply.
func (p *Plugin) exerciseFailure(what string, appErr *model.AppError) error {
	p.API.LogWarn("exercise step failed", "step", what, "error", appErr.Error())
	return errors.Errorf("%s (%s)", what, strings.TrimRight(appErr.Message, "."))
}

// isExercisePersonaID reports whether userID is an account exercise mode
// created for teamID.
func (p *Plugin) isExercisePersonaID(userID, teamID string) (bool, error) {
	data, appErr := p.API.KVGet(kvExercisePersonaPrefix + userID)
	if appErr != nil {
		return false, p.exerciseFailure("could not check an exercise persona", appErr)
	}
	return string(data) == teamID, nil
}

// findExercisePersona returns the account with the persona's username, or
// nil when the name is free. An account exercise mode didn't create is an
// error, since it belongs to somebody real.
func (p *Plugin) findExercisePersona(persona exercisePersona, teamID string) (*model.User, error) {
	user, appErr := p.API.GetUserByUsername(persona.Username)
	if isNotFound(appErr) {
		return nil, nil
	}
	if appErr != nil {
		return nil, p.exerciseFailure("could not look up @"+persona.Username, appErr)
	}
	ok, err := p.isExercisePersonaID(user.Id, teamID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &personaTakenError{persona.Username}
	}
	return user, nil
}

// checkExercisePreconditions makes sure nothing the plan needs is taken and
// that personas may join the team, before anything is written.
func (p *Plugin) checkExercisePreconditions(plan *exercisePlan) error {
	if err := p.checkExerciseEmailDomain(plan.TeamID); err != nil {
		return err
	}
	for _, persona := range plan.Personas {
		if _, err := p.findExercisePersona(persona, plan.TeamID); err != nil {
			return err
		}
	}
	var names []string
	for _, ch := range plan.Channels {
		names = append(names, ch.Name)
	}
	for _, req := range plan.ChannelRequests {
		names = append(names, req.Name)
	}
	for _, name := range names {
		_, appErr := p.API.GetChannelByName(plan.TeamID, name, true)
		if appErr == nil {
			return errors.Errorf("a channel named ~%s already exists in this team", name)
		}
		if !isNotFound(appErr) {
			return p.exerciseFailure("could not look up ~"+name, appErr)
		}
	}
	for _, spec := range plan.TeamRequests {
		_, appErr := p.API.GetTeamByName(spec.Name)
		if appErr == nil {
			return errors.Errorf("a team with the URL name %s already exists", spec.Name)
		}
		if !isNotFound(appErr) {
			return p.exerciseFailure("could not look up the team "+spec.Name, appErr)
		}
	}
	for _, spec := range plan.BotRequests {
		_, appErr := p.API.GetUserByUsername(spec.Username)
		if appErr == nil {
			return errors.Errorf("the username @%s is taken", spec.Username)
		}
		if !isNotFound(appErr) {
			return p.exerciseFailure("could not look up @"+spec.Username, appErr)
		}
	}
	if len(plan.TeamGuestRequests) > 0 {
		return p.checkExerciseGuestSettings()
	}
	return nil
}

// checkExerciseGuestSettings refuses when approving a guest invite request
// would fail: guest accounts are off, or guests are only invited from email
// domains that don't include the exercise domain.
func (p *Plugin) checkExerciseGuestSettings() error {
	config := p.API.GetConfig()
	if config == nil || config.GuestAccountsSettings.Enable == nil || !*config.GuestAccountsSettings.Enable {
		return errors.New("this scenario seeds guest invite requests, but guest accounts are off. Turn on **Enable Guest Access** in System Console → Authentication → Guest Access, or use a scenario without them")
	}
	if config.GuestAccountsSettings.RestrictCreationToDomains != nil {
		if domains := domainsExcludingExercise(*config.GuestAccountsSettings.RestrictCreationToDomains); domains != nil {
			return errors.Errorf("guests can only be invited from %s, so exercise guest invites (@%s) would fail. Add %s to the guest domains while you run the exercise", strings.Join(domains, ", "), exerciseEmailDomain, exerciseEmailDomain)
		}
	}
	return nil
}

// domainsExcludingExercise returns the domains in a comma- or space-separated
// list when the list is set and doesn't include the exercise domain, or nil.
func domainsExcludingExercise(list string) []string {
	domains := strings.Fields(strings.NewReplacer("@", " ", ",", " ").Replace(strings.ToLower(list)))
	if len(domains) == 0 || slices.Contains(domains, exerciseEmailDomain) {
		return nil
	}
	return domains
}

// checkExerciseEmailDomain refuses when the server or the team only lets
// in accounts from email domains that don't include the personas' domain:
// Mattermost would refuse to add them to the team.
func (p *Plugin) checkExerciseEmailDomain(teamID string) error {
	var lists []string
	if config := p.API.GetConfig(); config != nil && config.TeamSettings.RestrictCreationToDomains != nil {
		lists = append(lists, *config.TeamSettings.RestrictCreationToDomains)
	}
	team, appErr := p.API.GetTeam(teamID)
	if appErr != nil {
		return p.exerciseFailure("could not load this team", appErr)
	}
	lists = append(lists, team.AllowedDomains)
	for _, list := range lists {
		if domains := domainsExcludingExercise(list); domains != nil {
			return errors.Errorf("this server or team only allows accounts with email addresses at %s, so exercise personas (@%s) can't join. Add %s to the allowed domains while you run the exercise", strings.Join(domains, ", "), exerciseEmailDomain, exerciseEmailDomain)
		}
	}
	return nil
}

// populateExercise seeds the scenario into a team for actorID. It is the
// shared operation behind `/mattermost-request-hub exercise populate`.
func (p *Plugin) populateExercise(teamID, actorID string) (string, error) {
	// The claim below is what makes populate run once; this check only
	// gives a second populate the right answer instead of a name clash.
	if _, _, err := p.loadExerciseManifest(teamID); !errors.Is(err, errNoExerciseManifest) {
		if err != nil {
			return "", err
		}
		return "", errExerciseExists
	}
	config := p.getConfiguration()
	if !config.AllowChannelRequests {
		return "", errors.New(exerciseChannelRequestsOffText)
	}
	if !config.UsesPrefixList() {
		return "", errors.New(notConfiguredMessage)
	}
	sc, err := loadExerciseScenario(config.ExerciseScenarioJSON)
	if err != nil {
		return "", err
	}
	now := model.GetMillis()
	plan, err := buildExercisePlan(sc, teamID, config.Prefixes(), now, model.NewId)
	if err != nil {
		return "", errors.Wrap(err, "the exercise scenario doesn't fit this server's settings")
	}
	for _, t := range plan.requestTypes() {
		if len(t.specIDs) > 0 && !config.RequestEnabled(pendingKindRequestType(t.kind)) {
			return "", errors.New(exerciseTypeOffText(t))
		}
	}
	if err = p.checkExercisePreconditions(plan); err != nil {
		return "", err
	}

	// Claim the manifest before writing anything. Only one populate per
	// team can win, and teardown can always find what was written.
	manifest := exerciseManifest{
		CreatedBy: actorID, CreatedAt: now, UpdatedAt: now, State: exerciseStatePopulating,
		Plan: plan, Done: map[string]string{},
	}
	data, _ := json.Marshal(manifest)
	claimed, appErr := p.API.KVSetWithOptions(exerciseManifestKey(teamID), data, model.PluginKVSetOptions{Atomic: true, OldValue: nil})
	if appErr != nil {
		return "", p.exerciseFailure("could not start the exercise", appErr)
	}
	if !claimed {
		return "", errExerciseExists
	}

	if err := p.seedExercise(plan); err != nil {
		p.API.LogError("failed to populate exercise data", "team_id", teamID, "error", err.Error())
		return "", errors.Errorf("could not finish seeding: %s. Run `/mattermost-request-hub exercise teardown` to remove what was created, then try again", err.Error())
	}
	if err := p.updateExerciseManifest(teamID, func(m *exerciseManifest) error {
		if m.State != exerciseStatePopulating {
			return errExerciseStopped
		}
		m.State = exerciseStateReady
		return nil
	}); err != nil {
		p.API.LogError("failed to mark exercise data ready", "team_id", teamID, "error", err.Error())
		return "", errors.New("seeded the exercise but could not mark it finished. Run `/mattermost-request-hub exercise status` to check it, or `/mattermost-request-hub exercise teardown` to remove it")
	}
	counts := []string{fmt.Sprintf("%d personas", len(plan.Personas)), fmt.Sprintf("%d channels", len(plan.Channels))}
	for _, t := range plan.requestTypes() {
		if t.alwaysShown() || len(t.specIDs) > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", len(t.specIDs), t.plural))
		}
	}
	return fmt.Sprintf("Seeded the **%s** exercise into this team: %s and %s. The approval cards are waiting for review. Run `/mattermost-request-hub exercise teardown` to remove it all.",
		escapeMarkdown(sc.Name), strings.Join(counts[:len(counts)-1], ", "), counts[len(counts)-1]), nil
}

// recordSeeded applies change to the manifest and refreshes its
// heartbeat. Once teardown has started it still records what was made, so
// teardown finds it, but returns errExerciseStopped to end the populate.
func (p *Plugin) recordSeeded(teamID string, change func(*exerciseManifest)) error {
	stopped := false
	err := p.updateExerciseManifest(teamID, func(m *exerciseManifest) error {
		change(m)
		stopped = m.State != exerciseStatePopulating
		return nil
	})
	if err == nil && stopped {
		return errExerciseStopped
	}
	return err
}

// seedExercise creates the plan's personas, channels and requests,
// recording each in the manifest as it goes, which also tells teardown it
// is still running. It stops at the first error, or once teardown has
// started. When teardown has already removed the manifest, it undoes the
// step it just made, since nothing else would find it.
func (p *Plugin) seedExercise(plan *exercisePlan) error {
	teamID := plan.TeamID
	noop := func(*exerciseManifest) {}
	made := func(change func(*exerciseManifest), undo func()) error {
		err := p.recordSeeded(teamID, change)
		if errors.Is(err, errNoExerciseManifest) {
			undo()
		}
		return err
	}

	users := map[string]*model.User{}
	for _, persona := range plan.Personas {
		user, err := p.ensureExercisePersona(persona, teamID)
		if err != nil {
			return err
		}
		if err := made(noop, func() { _ = p.API.UpdateUserActive(user.Id, false) }); err != nil {
			return err
		}
		users[persona.Role] = user
	}
	ids := func(roles []string) []string {
		out := make([]string, 0, len(roles))
		for _, r := range roles {
			out = append(out, users[r].Id)
		}
		return out
	}

	channels := map[string]*model.Channel{}
	for _, spec := range plan.Channels {
		channel, appErr := p.API.CreateChannel(&model.Channel{
			TeamId:      teamID,
			Name:        spec.Name,
			DisplayName: spec.DisplayName,
			Purpose:     spec.Purpose,
			Header:      exerciseBanner,
			Type:        model.ChannelType(spec.ChannelType),
			CreatorId:   users[spec.Creator].Id,
		})
		if appErr != nil {
			return p.exerciseFailure("could not create ~"+spec.Name, appErr)
		}
		if err := made(func(m *exerciseManifest) {
			m.Done[spec.OpKey] = channel.Id
			m.ChannelIDs = append(m.ChannelIDs, channel.Id)
		}, func() { _, _ = p.archiveExerciseChannel(channel.Id) }); err != nil {
			return err
		}
		admins := map[string]bool{}
		for _, r := range spec.Admins {
			admins[r] = true
		}
		for _, r := range dedupeNonEmpty(append(append([]string{spec.Creator}, spec.Members...), spec.Admins...)) {
			if _, appErr := p.API.AddChannelMember(channel.Id, users[r].Id); appErr != nil {
				return p.exerciseFailure(fmt.Sprintf("could not add @%s to ~%s", users[r].Username, spec.Name), appErr)
			}
			if admins[r] {
				if _, appErr := p.API.UpdateChannelMemberRoles(channel.Id, users[r].Id, channelAdminRoleString); appErr != nil {
					return p.exerciseFailure(fmt.Sprintf("could not make @%s Channel Admin of ~%s", users[r].Username, spec.Name), appErr)
				}
			}
		}
		channels[spec.OpKey] = channel
	}

	var team *model.Team
	if len(plan.TeamAdminRequests)+len(plan.TeamMemberRequests)+len(plan.TeamGuestRequests) > 0 {
		var appErr *model.AppError
		if team, appErr = p.API.GetTeam(teamID); appErr != nil {
			return p.exerciseFailure("could not load this team", appErr)
		}
	}
	seed := func(s exerciseSeed) error { return p.seedRequest(s, made) }

	for _, spec := range plan.ChannelRequests {
		req := &channelRequest{
			ID: spec.ID, RequesterID: users[spec.Requester].Id, TeamID: teamID,
			Name: spec.Name, DisplayName: spec.DisplayName, Purpose: spec.Purpose, ChannelType: spec.ChannelType,
			MemberIDs: ids(spec.Members), AdminMemberIDs: ids(spec.Admins), CreatedAt: spec.CreatedAt, Exercise: true,
		}
		requester := users[spec.Requester]
		if err := seed(exerciseSeed{
			opKey: spec.OpKey, kind: pendingKindChannel, what: "channel request", record: req,
			scopeID: teamID,
			store:   func() error { return p.storeRequest(req) },
			post:    func() (string, string, error) { return p.postApprovalRequest(req, requester) },
			dm:      func() (string, string) { return p.sendTicketCreatedDM(req, requester) },
			posts:   ticketPostFields{&req.ApprovalPostID, &req.ApprovalChannelID, &req.DMRootPostID, &req.DMChannelID},
		}); err != nil {
			return err
		}
	}

	for _, spec := range plan.AdminRequests {
		channel := channels[spec.ChannelOpKey]
		req := &adminRequest{
			ID: spec.ID, RequesterID: users[spec.Requester].Id, ChannelID: channel.Id,
			NomineeIDs: ids(spec.Nominees), CreatedAt: spec.CreatedAt, Exercise: true,
		}
		requester := users[spec.Requester]
		if err := seed(exerciseSeed{
			opKey: spec.OpKey, kind: pendingKindAdmin, what: "Channel Admin request", record: req,
			scopeID: channel.Id,
			store:   func() error { return p.storeAdminRequest(req) },
			post:    func() (string, string, error) { return p.postAdminApprovalRequest(req, requester, channel) },
			dm:      func() (string, string) { return p.sendAdminTicketCreatedDM(req, requester, channel) },
			posts:   ticketPostFields{&req.ApprovalPostID, &req.ApprovalChannelID, &req.DMRootPostID, &req.DMChannelID},
		}); err != nil {
			return err
		}
	}

	for _, spec := range plan.TeamRequests {
		req := &teamCreationRequest{
			ID: spec.ID, RequesterID: users[spec.Requester].Id, DisplayName: spec.DisplayName, Name: spec.Name,
			Description: spec.Description, Type: spec.Type, RequestTeamAdmin: spec.TeamAdmin,
			CreatedAt: spec.CreatedAt, Exercise: true, ExerciseTeamID: teamID,
		}
		requester := users[spec.Requester]
		if err := seed(exerciseSeed{
			opKey: spec.OpKey, kind: pendingKindTeam, what: "team creation request", record: req,
			store: func() error { return p.storeTeamRequest(req) },
			post:  func() (string, string, error) { return p.postTeamCreationApprovalRequest(req, requester) },
			dm:    func() (string, string) { return p.sendTeamCreationTicketCreatedDM(req, requester) },
			posts: ticketPostFields{&req.ApprovalPostID, &req.ApprovalChannelID, &req.DMRootPostID, &req.DMChannelID},
		}); err != nil {
			return err
		}
	}

	for _, spec := range plan.TeamAdminRequests {
		req := &teamAdminRequest{
			ID: spec.ID, RequesterID: users[spec.Requester].Id, TeamID: teamID,
			NomineeIDs: ids(spec.Nominees), CreatedAt: spec.CreatedAt, Exercise: true,
		}
		requester := users[spec.Requester]
		if err := seed(exerciseSeed{
			opKey: spec.OpKey, kind: pendingKindTeamAdmin, what: "Team Admin request", record: req,
			store: func() error { return p.storeTeamAdminRequest(req) },
			post:  func() (string, string, error) { return p.postTeamAdminApprovalRequest(req, requester, team) },
			dm:    func() (string, string) { return p.sendTeamAdminTicketCreatedDM(req, requester, team) },
			posts: ticketPostFields{&req.ApprovalPostID, &req.ApprovalChannelID, &req.DMRootPostID, &req.DMChannelID},
		}); err != nil {
			return err
		}
	}

	for _, spec := range plan.TeamMemberRequests {
		req := &teamMemberRequest{
			ID: spec.ID, RequesterID: users[spec.Requester].Id, TeamID: teamID,
			NomineeIDs: ids(spec.Nominees), Reason: spec.Reason, CreatedAt: spec.CreatedAt, Exercise: true,
		}
		requester := users[spec.Requester]
		if err := seed(exerciseSeed{
			opKey: spec.OpKey, kind: pendingKindTeamMember, what: "team member request", record: req,
			store: func() error { return p.storeTeamMemberRequest(req) },
			post:  func() (string, string, error) { return p.postTeamMemberApprovalRequest(req, requester, team) },
			dm:    func() (string, string) { return p.sendTeamMemberTicketCreatedDM(req, requester, team) },
			posts: ticketPostFields{&req.ApprovalPostID, &req.ApprovalChannelID, &req.DMRootPostID, &req.DMChannelID},
		}); err != nil {
			return err
		}
	}

	for _, spec := range plan.TeamGuestRequests {
		channelIDs := make([]string, 0, len(spec.ChannelOpKeys))
		for _, key := range spec.ChannelOpKeys {
			channelIDs = append(channelIDs, channels[key].Id)
		}
		req := &teamGuestRequest{
			ID: spec.ID, RequesterID: users[spec.Requester].Id, TeamID: teamID, Email: spec.Email,
			ChannelIDs: channelIDs, Reason: spec.Reason, CreatedAt: spec.CreatedAt, Exercise: true,
		}
		requester := users[spec.Requester]
		if err := seed(exerciseSeed{
			opKey: spec.OpKey, kind: pendingKindTeamGuest, what: "guest invite request", record: req,
			store: func() error { return p.storeTeamGuestRequest(req) },
			post:  func() (string, string, error) { return p.postTeamGuestApprovalRequest(req, requester, team) },
			dm:    func() (string, string) { return p.sendTeamGuestTicketCreatedDM(req, requester, team) },
			posts: ticketPostFields{&req.ApprovalPostID, &req.ApprovalChannelID, &req.DMRootPostID, &req.DMChannelID},
		}); err != nil {
			return err
		}
	}

	channelID := func(opKey string) string {
		if opKey == "" {
			return ""
		}
		return channels[opKey].Id
	}
	for _, spec := range plan.BotRequests {
		req := &botRequest{
			ID: spec.ID, RequesterID: users[spec.Requester].Id, Username: spec.Username,
			DisplayName: spec.DisplayName, Description: spec.Description, RequestToken: spec.Token,
			IncomingWebhookChannelID: channelID(spec.IncomingChannelOpKey),
			OutgoingWebhookChannelID: channelID(spec.OutgoingChannelOpKey),
			CreatedAt:                spec.CreatedAt, Exercise: true, ExerciseTeamID: teamID,
		}
		if spec.Owner != "" {
			req.OwnerUserID = users[spec.Owner].Id
		}
		if req.OutgoingWebhookChannelID != "" {
			req.OutgoingWebhookCallbackURL = exerciseCallbackURL
		}
		requester := users[spec.Requester]
		if err := seed(exerciseSeed{
			opKey: spec.OpKey, kind: pendingKindBot, what: "bot account request", record: req,
			store: func() error { return p.storeBotRequest(req) },
			post:  func() (string, string, error) { return p.postBotApprovalRequest(req, requester) },
			dm:    func() (string, string) { return p.sendBotTicketCreatedDM(req, requester) },
			posts: ticketPostFields{&req.ApprovalPostID, &req.ApprovalChannelID, &req.DMRootPostID, &req.DMChannelID},
		}); err != nil {
			return err
		}
	}

	for _, spec := range plan.WebhookRequests {
		channel := channels[spec.ChannelOpKey]
		req := &webhookRequest{
			ID: spec.ID, RequesterID: users[spec.Requester].Id, WebhookType: spec.WebhookType,
			ChannelID: channel.Id, TeamID: teamID, DisplayName: spec.DisplayName, Description: spec.Description,
			CreatedAt: spec.CreatedAt, Exercise: true,
		}
		if spec.WebhookType == webhookTypeOutgoing {
			req.CallbackURL = exerciseCallbackURL
		}
		requester := users[spec.Requester]
		if err := seed(exerciseSeed{
			opKey: spec.OpKey, kind: pendingKindWebhook, what: "webhook request", record: req,
			store: func() error { return p.storeWebhookRequest(req) },
			post:  func() (string, string, error) { return p.postWebhookApprovalRequest(req, requester, channel) },
			dm:    func() (string, string) { return p.sendWebhookTicketCreatedDM(req, requester, channel) },
			posts: ticketPostFields{&req.ApprovalPostID, &req.ApprovalChannelID, &req.DMRootPostID, &req.DMChannelID},
		}); err != nil {
			return err
		}
	}
	return nil
}

// exerciseSeed is one request for seedRequest: the record, and the
// functions of its type that store it, post its card and send the
// requester's ticket DM.
type exerciseSeed struct {
	opKey string
	kind  string
	// what names the type in errors, as in "channel request".
	what   string
	record storedRequest
	// scopeID is the ticket thread's scope, for the types that have one.
	scopeID string
	store   func() error
	post    func() (postID, channelID string, err error)
	dm      func() (dmRootPostID, dmChannelID string)
	posts   ticketPostFields
}

// ticketPostFields points at a record's card and ticket post fields.
type ticketPostFields struct {
	approvalPostID, approvalChannelID, dmRootPostID, dmChannelID *string
}

// seedRequest stores an exercise request, lists it as pending, posts its
// card and ticket DM the way a submit does, and records it with made.
func (p *Plugin) seedRequest(s exerciseSeed, made func(change func(*exerciseManifest), undo func()) error) error {
	id, requesterID, _ := s.record.pendingFields()
	fail := func(msg string, err error) error {
		p.API.LogWarn("failed to seed exercise data: "+msg, "error", err.Error())
		return errors.New(msg)
	}
	if err := s.store(); err != nil {
		return fail("could not save a "+s.what, err)
	}
	undo := func() {
		if _, err := p.removeExerciseRequest(s.kind, id); err != nil {
			p.API.LogWarn("failed to remove exercise request", "request_id", id, "error", err.Error())
		}
	}
	if err := p.addPendingEntry(s.kind, requesterID, newPendingEntry(s.record)); err != nil {
		return fail("could not list a "+s.what+" as pending", err)
	}
	rawBefore, _ := json.Marshal(s.record) // the bytes store wrote
	approvalPostID, approvalChannelID, err := s.post()
	if err != nil {
		return fail("could not post a "+s.what+"'s approval card", err)
	}
	if approvalPostID == "" {
		p.resolveCardsIfHandled(s.kind, id)
	}
	// The ticket DM reads the card's IDs from the record.
	*s.posts.approvalPostID, *s.posts.approvalChannelID = approvalPostID, approvalChannelID
	dmRootPostID, dmChannelID := s.dm()
	*s.posts.dmRootPostID, *s.posts.dmChannelID = dmRootPostID, dmChannelID
	p.persistTicketPosts(ticketPersist{
		KVKey: requestKVPrefix(s.kind) + id, RawBefore: rawBefore, Record: s.record,
		RequestID: id, Label: "exercise " + s.what,
		Thread: ticketThread{
			ApprovalPostID: approvalPostID, ApprovalChannelID: approvalChannelID,
			DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
			Kind: pendingKindRequestType(s.kind), ScopeID: s.scopeID,
		},
	})
	keys := exerciseRequestKeys(id, approvalPostID, dmRootPostID)
	return made(func(m *exerciseManifest) {
		m.Done[s.opKey] = id
		m.KVKeys = append(m.KVKeys, keys...)
	}, undo)
}

// ensureExercisePersona returns the persona's active account, creating it
// or reactivating the one an earlier teardown deactivated, and makes sure
// it is on the team unless it is off_team.
func (p *Plugin) ensureExercisePersona(persona exercisePersona, teamID string) (*model.User, error) {
	user, err := p.findExercisePersona(persona, teamID)
	if err != nil {
		return nil, err
	}
	switch {
	case user == nil:
		newUser := &model.User{
			Username:  persona.Username,
			Email:     persona.Email,
			FirstName: persona.FirstName,
			LastName:  persona.LastName,
			Nickname:  exerciseMarker,
			Position:  persona.Position,
			Password:  exercisePassword(),
		}
		newUser.SetDefaultNotifications()
		newUser.NotifyProps[model.EmailNotifyProp] = "false"
		var appErr *model.AppError
		if user, appErr = p.API.CreateUser(newUser); appErr != nil {
			return nil, p.exerciseFailure("could not create @"+persona.Username, appErr)
		}
		if appErr = p.API.KVSet(kvExercisePersonaPrefix+user.Id, []byte(teamID)); appErr != nil {
			// Unrecorded, the account would never be treated as a persona
			// again, so don't leave it active.
			_ = p.API.UpdateUserActive(user.Id, false)
			return nil, p.exerciseFailure("could not record @"+persona.Username, appErr)
		}
	case user.DeleteAt != 0:
		if appErr := p.API.UpdateUserActive(user.Id, true); appErr != nil {
			return nil, p.exerciseFailure("could not reactivate @"+persona.Username, appErr)
		}
	}
	if persona.OffTeam {
		return user, nil
	}
	if _, appErr := p.API.CreateTeamMember(teamID, user.Id); appErr != nil {
		return nil, p.exerciseFailure(fmt.Sprintf("could not add @%s to the team", persona.Username), appErr)
	}
	return user, nil
}

// exerciseTeardownCounts tallies what teardown removed.
type exerciseTeardownCounts struct {
	removed, archived, deactivated, failed int
	teams, bots, webhooks                  int
}

// teardownExercise removes everything the team's manifest names: pending
// exercise requests and their cards, the webhooks, bots and teams their
// approvals created, exercise channels, and personas. Real requests and
// accounts are never changed. actorID is the System Admin running it, whose
// session deletes webhooks. The manifest is kept when anything fails, so
// teardown can be run again.
func (p *Plugin) teardownExercise(teamID, actorID string) (string, error) {
	m, _, err := p.loadExerciseManifest(teamID)
	if errors.Is(err, errNoExerciseManifest) {
		return "No exercise data is recorded for this team, so there is nothing to tear down.", nil
	}
	if err != nil {
		return "", err
	}
	if m.State == exerciseStatePopulating && model.GetMillis()-m.UpdatedAt < exerciseStaleMillis {
		return "", errors.New("a populate is still running in this team. Wait for it to finish, then run teardown")
	}
	// A populate that is still running stops at its next step.
	err = p.updateExerciseManifest(teamID, func(m *exerciseManifest) error {
		m.State = exerciseStateTearingDown
		return nil
	})
	if err != nil {
		return "", err
	}

	// Each pass removes everything the manifest names, then deletes the
	// manifest with a compare-and-delete. A populate or approval that
	// records something in between makes the delete fail, and the next
	// pass removes that too. Every step skips what is already gone.
	var counts exerciseTeardownCounts
	for range manifestUpdateAttempts {
		latest, raw, loadErr := p.loadExerciseManifest(teamID)
		if loadErr != nil {
			return "", loadErr
		}
		counts.failed = 0
		p.teardownPass(teamID, actorID, latest, &counts)
		if counts.failed > 0 {
			return "", errors.Errorf("%d exercise items could not be removed; see the server logs, then run `/mattermost-request-hub exercise teardown` again", counts.failed)
		}
		deleted, appErr := p.API.KVCompareAndDelete(exerciseManifestKey(teamID), raw)
		if appErr != nil {
			p.API.LogWarn("failed to clear exercise record", "team_id", teamID, "error", appErr.Error())
			return "", errors.New("removed the exercise data but could not clear its record. Run `/mattermost-request-hub exercise teardown` again")
		}
		if deleted {
			return "Removed the exercise data from this team: " + counts.summary() + ".", nil
		}
	}
	return "", errors.New("the exercise record kept changing while it was removed. Run `/mattermost-request-hub exercise teardown` again")
}

// summary lists what teardown removed. Teams, bots and webhooks are only
// listed when there were any.
func (c exerciseTeardownCounts) summary() string {
	parts := []string{fmt.Sprintf("%d pending requests closed", c.removed)}
	for _, extra := range []struct {
		n    int
		what string
	}{{c.teams, "teams archived"}, {c.bots, "bots deleted"}, {c.webhooks, "webhooks deleted"}} {
		if extra.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", extra.n, extra.what))
		}
	}
	parts = append(parts, fmt.Sprintf("%d channels archived", c.archived))
	return strings.Join(parts, ", ") + fmt.Sprintf(" and %d personas deactivated", c.deactivated)
}

// teardownPass removes the requests, webhooks, bots, teams, channels and
// personas the manifest names, adding to counts.
func (p *Plugin) teardownPass(teamID, actorID string, m *exerciseManifest, counts *exerciseTeardownCounts) {
	plan := m.Plan
	removeRequest := func(kind, id string) {
		ok, err := p.removeExerciseRequest(kind, id)
		switch {
		case err != nil:
			p.API.LogWarn("failed to remove exercise request", "request_id", id, "error", err.Error())
			counts.failed++
		case ok:
			counts.removed++
		}
	}
	for _, t := range plan.requestTypes() {
		for _, id := range t.specIDs {
			removeRequest(t.kind, id)
		}
	}
	for _, key := range m.KVKeys {
		if appErr := p.API.KVDelete(key); appErr != nil {
			p.API.LogWarn("failed to delete exercise record", "key", key, "error", appErr.Error())
			counts.failed++
		}
	}
	// remove runs one removal step and counts its outcome.
	remove := func(what, id string, count *int, step func() (bool, error)) {
		ok, err := step()
		switch {
		case err != nil:
			p.API.LogWarn("failed to remove exercise "+what, "id", id, "error", err.Error())
			counts.failed++
		case ok:
			*count++
		}
	}

	// Webhooks go before bots: deleting a bot leaves its webhooks behind.
	for _, hook := range m.Webhooks {
		remove("webhook", hook.ID, &counts.webhooks, func() (bool, error) { return p.deleteExerciseWebhook(hook, actorID) })
	}

	// A crash right after an approval leaves its bot or team unrecorded.
	// The planned name finds it, but only one tied to a persona counts.
	botIDs := append([]string{}, m.BotIDs...)
	for _, spec := range plan.BotRequests {
		if id, ok := p.findUnrecordedExerciseBot(spec.Username, teamID, counts); ok {
			botIDs = append(botIDs, id)
		}
	}
	for _, id := range dedupeNonEmpty(botIDs) {
		remove("bot", id, &counts.bots, func() (bool, error) { return p.deleteExerciseBot(id) })
	}
	teamIDs := append([]string{}, m.TeamIDs...)
	for _, spec := range plan.TeamRequests {
		if id, ok := p.findUnrecordedExerciseTeam(spec, plan, counts); ok {
			teamIDs = append(teamIDs, id)
		}
	}
	for _, id := range dedupeNonEmpty(teamIDs) {
		remove("team", id, &counts.teams, func() (bool, error) { return p.archiveExerciseTeam(id) })
	}

	// A crash right after creating a channel leaves it unrecorded. Its
	// name finds it, but only a channel a persona created counts, since
	// members can rename a channel and change its header.
	channelIDs := append([]string{}, m.ChannelIDs...)
	var planned []string
	for _, spec := range plan.Channels {
		planned = append(planned, spec.Name)
	}
	for _, spec := range plan.ChannelRequests {
		planned = append(planned, spec.Name)
	}
	for _, name := range planned {
		ch, appErr := p.API.GetChannelByName(teamID, name, false)
		if isNotFound(appErr) {
			continue
		}
		if appErr != nil {
			p.API.LogWarn("failed to look up exercise channel", "name", name, "error", appErr.Error())
			counts.failed++
			continue
		}
		isPersona, err := p.isExercisePersonaID(ch.CreatorId, teamID)
		if err != nil {
			counts.failed++
			continue
		}
		if isPersona {
			channelIDs = append(channelIDs, ch.Id)
		}
	}
	for _, id := range dedupeNonEmpty(channelIDs) {
		ok, err := p.archiveExerciseChannel(id)
		switch {
		case err != nil:
			p.API.LogWarn("failed to archive exercise channel", "channel_id", id, "error", err.Error())
			counts.failed++
		case ok:
			counts.archived++
		}
	}

	for _, persona := range plan.Personas {
		user, err := p.findExercisePersona(persona, teamID)
		if isPersonaTaken(err) {
			// Never created, and the name has since gone to someone real.
			continue
		}
		if err != nil {
			p.API.LogWarn("failed to find exercise persona", "username", persona.Username, "error", err.Error())
			counts.failed++
			continue
		}
		if user == nil {
			continue
		}
		// A persona's pending lists only ever name exercise requests.
		for _, kind := range allPendingKinds {
			if appErr := p.API.KVDelete(pendingListKey(kind, user.Id)); appErr != nil {
				p.API.LogWarn("failed to delete exercise persona's pending list", "user_id", user.Id, "error", appErr.Error())
				counts.failed++
			}
		}
		if err := p.resetExercisePersonaTeam(persona, user.Id, teamID); err != nil {
			p.API.LogWarn("failed to reset exercise persona's team membership", "user_id", user.Id, "error", err.Error())
			counts.failed++
		}
		if user.DeleteAt != 0 {
			continue
		}
		if appErr := p.API.UpdateUserActive(user.Id, false); appErr != nil {
			p.API.LogWarn("failed to deactivate exercise persona", "user_id", user.Id, "error", appErr.Error())
			counts.failed++
			continue
		}
		counts.deactivated++
	}
}

// resetExercisePersonaTeam undoes what approvals changed about a persona's
// team membership: an off-team persona leaves the team, and any other
// persona loses Team Admin.
func (p *Plugin) resetExercisePersonaTeam(persona exercisePersona, userID, teamID string) error {
	member, appErr := p.API.GetTeamMember(teamID, userID)
	if isNotFound(appErr) {
		return nil
	}
	if appErr != nil {
		return errors.Wrap(appErr, "failed to load the team membership")
	}
	if member.DeleteAt != 0 {
		return nil
	}
	if persona.OffTeam {
		if appErr = p.API.DeleteTeamMember(teamID, userID, p.botUserID); appErr != nil {
			return errors.Wrap(appErr, "failed to remove the persona from the team")
		}
		return nil
	}
	if member.SchemeAdmin {
		if _, appErr = p.API.UpdateTeamMemberRoles(teamID, userID, model.TeamUserRoleId); appErr != nil {
			return errors.Wrap(appErr, "failed to remove Team Admin")
		}
	}
	return nil
}

// findUnrecordedExerciseBot returns the bot with a planned exercise bot
// username when a persona of teamID owns it.
func (p *Plugin) findUnrecordedExerciseBot(username, teamID string, counts *exerciseTeardownCounts) (string, bool) {
	user, appErr := p.API.GetUserByUsername(username)
	if isNotFound(appErr) {
		return "", false
	}
	if appErr != nil {
		p.API.LogWarn("failed to look up exercise bot", "username", username, "error", appErr.Error())
		counts.failed++
		return "", false
	}
	if !user.IsBot {
		return "", false
	}
	bot, appErr := p.API.GetBot(user.Id, true)
	if appErr != nil {
		p.API.LogWarn("failed to load exercise bot", "user_id", user.Id, "error", appErr.Error())
		counts.failed++
		return "", false
	}
	isPersona, err := p.isExercisePersonaID(bot.OwnerId, teamID)
	if err != nil {
		counts.failed++
		return "", false
	}
	return user.Id, isPersona
}

// findUnrecordedExerciseTeam returns the team with a planned exercise team
// URL name when it is marked EXERCISE and its requesting persona is a
// member: the plugin creates teams, so there is no persona creator to check.
func (p *Plugin) findUnrecordedExerciseTeam(spec exerciseTeamRequest, plan *exercisePlan, counts *exerciseTeardownCounts) (string, bool) {
	team, appErr := p.API.GetTeamByName(spec.Name)
	if isNotFound(appErr) {
		return "", false
	}
	if appErr != nil {
		p.API.LogWarn("failed to look up exercise team", "name", spec.Name, "error", appErr.Error())
		counts.failed++
		return "", false
	}
	if !strings.HasPrefix(team.DisplayName, exerciseDisplayPrefix) {
		return "", false
	}
	for _, persona := range plan.Personas {
		if persona.Role != spec.Requester {
			continue
		}
		user, err := p.findExercisePersona(persona, plan.TeamID)
		if err != nil && !isPersonaTaken(err) {
			p.API.LogWarn("failed to look up exercise persona", "username", persona.Username, "error", err.Error())
			counts.failed++
			return "", false
		}
		if user == nil {
			return "", false
		}
		member, appErr := p.API.GetTeamMember(team.Id, user.Id)
		if isNotFound(appErr) {
			return "", false
		}
		if appErr != nil {
			p.API.LogWarn("failed to look up exercise team member", "team_id", team.Id, "error", appErr.Error())
			counts.failed++
			return "", false
		}
		return team.Id, member.DeleteAt == 0
	}
	return "", false
}

// removeExerciseRequest claims and deletes a pending exercise request,
// releases its pending entry and resolves its cards. It reports false when
// the request was already decided.
func (p *Plugin) removeExerciseRequest(kind, requestID string) (bool, error) {
	key := requestKVPrefix(kind) + requestID
	data, appErr := p.API.KVGet(key)
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to load the request")
	}
	if data == nil {
		return false, nil
	}
	var rec struct {
		RequesterID    string `json:"requester_id"`
		Exercise       bool   `json:"exercise"`
		ApprovalPostID string `json:"approval_post_id"`
		DMRootPostID   string `json:"dm_root_post_id"`
		DMChannelID    string `json:"dm_channel_id"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return false, errors.Wrap(err, "failed to read the request")
	}
	if !rec.Exercise {
		return false, errors.New("the stored request isn't exercise data, so it was left alone")
	}
	// The same claim an approval makes, so only one of them wins.
	claimed, appErr := p.API.KVCompareAndDelete(key, data)
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to remove the request")
	}
	if !claimed {
		// A decision took it, or a first approval rewrote it and it is
		// still pending, which must keep the manifest for another pass.
		if data, appErr := p.API.KVGet(key); appErr != nil || data != nil {
			return false, errors.New("the request changed while it was being removed")
		}
		return false, nil
	}
	p.releasePending(kind, rec.RequesterID, requestID)
	p.closeRemovedExerciseRequest(newDecisionCards(requestID, "", rec.ApprovalPostID), rec.RequesterID, rec.DMRootPostID, rec.DMChannelID)
	for _, key := range exerciseRequestKeys(requestID, rec.ApprovalPostID, rec.DMRootPostID) {
		if appErr := p.API.KVDelete(key); appErr != nil {
			p.API.LogWarn("failed to delete exercise record", "key", key, "error", appErr.Error())
		}
	}
	return true, nil
}

// closeRemovedExerciseRequest closes the persona's ticket and every
// approval card of a request teardown removed, the way an expiry would.
// An approval that lands after teardown, and so removes what it made
// again, closes its request this way too.
func (p *Plugin) closeRemovedExerciseRequest(cards decisionCards, requesterID, dmRootPostID, dmChannelID string) decisionResult {
	return p.finish(cards, "", exerciseRemovedText, ticketClose{
		RequesterID: requesterID, DMRootPostID: dmRootPostID, DMChannelID: dmChannelID,
		Expired:     true,
		CardTitle:   exerciseDisplayPrefix + "Request Removed",
		CardText:    "Teardown removed this exercise request.",
		ThreadReply: exerciseRemovedText,
		StatusLine:  exerciseRemovedText,
	})
}

// exerciseRequestKeys names the records a request leaves besides itself:
// its approval card copies and the two reply-forwarding lookups, which
// outlive a decision.
func exerciseRequestKeys(requestID, approvalPostID, dmRootPostID string) []string {
	keys := []string{kvApprovalCardsPrefix + requestID}
	if approvalPostID != "" {
		keys = append(keys, kvApprovalPostPrefix+approvalPostID)
	}
	if dmRootPostID != "" {
		keys = append(keys, kvDMRootPrefix+dmRootPostID)
	}
	return keys
}

// archiveExerciseChannel renames a channel so its name can be used again,
// then archives it. It reports false when the channel is already gone.
func (p *Plugin) archiveExerciseChannel(channelID string) (bool, error) {
	channel, appErr := p.API.GetChannel(channelID)
	if isNotFound(appErr) {
		return false, nil
	}
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to load the channel")
	}
	if channel.DeleteAt != 0 {
		return false, nil
	}
	tail := "-" + channel.Id
	if !strings.HasSuffix(channel.Name, tail) {
		base := channel.Name
		if len(base)+len(tail) > model.ChannelNameMaxLength {
			base = strings.TrimRight(base[:model.ChannelNameMaxLength-len(tail)], "-")
		}
		channel.Name = base + tail
		if _, appErr = p.API.UpdateChannel(channel); appErr != nil {
			return false, errors.Wrap(appErr, "failed to rename the channel")
		}
	}
	if appErr = p.API.DeleteChannel(channelID); appErr != nil {
		return false, errors.Wrap(appErr, "failed to archive the channel")
	}
	return true, nil
}

// archiveExerciseTeam renames a team so its name can be used again, then
// archives it. It reports false when the team is already gone.
func (p *Plugin) archiveExerciseTeam(teamID string) (bool, error) {
	team, appErr := p.API.GetTeam(teamID)
	if isNotFound(appErr) {
		return false, nil
	}
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to load the team")
	}
	if team.DeleteAt != 0 {
		return false, nil
	}
	tail := "-" + team.Id
	if !strings.HasSuffix(team.Name, tail) {
		base := team.Name
		if len(base)+len(tail) > model.TeamNameMaxLength {
			base = strings.TrimRight(base[:model.TeamNameMaxLength-len(tail)], "-")
		}
		team.Name = base + tail
		if _, appErr = p.API.UpdateTeam(team); appErr != nil {
			return false, errors.Wrap(appErr, "failed to rename the team")
		}
	}
	if appErr = p.API.DeleteTeam(teamID); appErr != nil {
		return false, errors.Wrap(appErr, "failed to archive the team")
	}
	return true, nil
}

// deleteExerciseBot permanently deletes a bot, with its tokens, so its
// username can be used again. It reports false when the bot is already
// gone.
func (p *Plugin) deleteExerciseBot(botUserID string) (bool, error) {
	_, appErr := p.API.GetBot(botUserID, true)
	if isNotFound(appErr) {
		return false, nil
	}
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to load the bot")
	}
	if appErr = p.API.PermanentDeleteBot(botUserID); appErr != nil {
		return false, errors.Wrap(appErr, "failed to delete the bot")
	}
	return true, nil
}

// deleteExerciseWebhook deletes a webhook with actorID's session. It
// reports false when the webhook is already gone.
func (p *Plugin) deleteExerciseWebhook(hook exerciseWebhook, actorID string) (bool, error) {
	err := p.deleteWebhook(hook.Type, hook.ID, actorID)
	var appErr *model.AppError
	if errors.As(err, &appErr) && appErr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// exerciseStatus compares what the team's manifest recorded with what is
// still live.
func (p *Plugin) exerciseStatus(teamID string) (string, error) {
	m, _, err := p.loadExerciseManifest(teamID)
	if errors.Is(err, errNoExerciseManifest) {
		return "No exercise data is recorded for this team. Run `/mattermost-request-hub exercise populate` to seed it.", nil
	}
	if err != nil {
		return "", err
	}
	plan := m.Plan

	unchecked := 0
	activePersonas := 0
	for _, persona := range plan.Personas {
		user, findErr := p.findExercisePersona(persona, teamID)
		switch {
		case isPersonaTaken(findErr):
		case findErr != nil:
			unchecked++
		case user != nil && user.DeleteAt == 0:
			activePersonas++
		}
	}
	liveChannels := 0
	for _, id := range m.ChannelIDs {
		ch, appErr := p.API.GetChannel(id)
		switch {
		case appErr != nil && !isNotFound(appErr):
			unchecked++
		case appErr == nil && ch.DeleteAt == 0:
			liveChannels++
		}
	}
	pending := map[string]int{}
	for _, t := range plan.requestTypes() {
		for _, id := range t.specIDs {
			ok, existsErr := p.pendingRequestExists(t.kind, id)
			switch {
			case existsErr != nil:
				unchecked++
			case ok:
				pending[t.kind]++
			}
		}
	}
	liveTeams := 0
	for _, id := range m.TeamIDs {
		team, appErr := p.API.GetTeam(id)
		switch {
		case appErr != nil && !isNotFound(appErr):
			unchecked++
		case appErr == nil && team.DeleteAt == 0:
			liveTeams++
		}
	}
	liveBots := 0
	for _, id := range m.BotIDs {
		_, appErr := p.API.GetBot(id, false)
		switch {
		case appErr != nil && !isNotFound(appErr):
			unchecked++
		case appErr == nil:
			liveBots++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Exercise **%s** in this team is **%s**.\n\n", escapeMarkdown(plan.Scenario), strings.ReplaceAll(m.State, "_", " "))
	b.WriteString("| | Recorded | Live |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Personas (active) | %d | %d |\n", len(plan.Personas), activePersonas)
	fmt.Fprintf(&b, "| Channels (not archived) | %d | %d |\n", len(m.ChannelIDs), liveChannels)
	var rows []string
	for _, t := range plan.requestTypes() {
		if t.alwaysShown() || len(t.specIDs) > 0 {
			rows = append(rows, fmt.Sprintf("| %s (pending) | %d | %d |", strings.ToUpper(t.plural[:1])+t.plural[1:], len(t.specIDs), pending[t.kind]))
		}
	}
	if len(m.TeamIDs) > 0 {
		rows = append(rows, fmt.Sprintf("| Teams (not archived) | %d | %d |", len(m.TeamIDs), liveTeams))
	}
	if len(m.BotIDs) > 0 {
		rows = append(rows, fmt.Sprintf("| Bots | %d | %d |", len(m.BotIDs), liveBots))
	}
	if len(m.Webhooks) > 0 {
		// The plugin API can't look a webhook up, so only teardown checks.
		rows = append(rows, fmt.Sprintf("| Webhooks | %d | not checked |", len(m.Webhooks)))
	}
	b.WriteString(strings.Join(rows, "\n"))
	if unchecked > 0 {
		p.API.LogWarn("exercise status could not check some items", "team_id", teamID, "count", unchecked)
		fmt.Fprintf(&b, "\n\n%d items could not be checked, so the live counts may be low. See the server logs.", unchecked)
	}
	switch m.State {
	case exerciseStatePopulating:
		b.WriteString("\n\nIf no populate is running, it stopped partway. Run `/mattermost-request-hub exercise teardown` to remove what it created.")
	case exerciseStateTearingDown:
		b.WriteString("\n\nA teardown started but didn't finish. Run `/mattermost-request-hub exercise teardown` again.")
	}
	return b.String(), nil
}
