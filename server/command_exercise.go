package main

import (
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

const (
	exercisePopulate = "populate"
	exerciseTeardown = "teardown"
	exerciseStatus   = "status"
)

const exerciseUsage = "Usage: `/mattermost-request-hub exercise populate|teardown|status`"

// executeExercise routes `/mattermost-request-hub exercise`. The setting is
// checked first, then that the caller is a System Admin, so nobody learns
// more than that exercise mode is off.
func (p *Plugin) executeExercise(args *model.CommandArgs, params []commandArg) *model.CommandResponse {
	if !p.getConfiguration().EnableExercise {
		return ephemeralResponse("Exercise mode is off. A System Admin can turn on **Enable exercise mode** in System Console → Plugins → Mattermost Request Hub.")
	}
	user, appErr := p.API.GetUser(args.UserId)
	if appErr != nil && !isNotFound(appErr) {
		p.API.LogWarn("failed to load the user running an exercise command", "user_id", args.UserId, "error", appErr.Error())
		return ephemeralResponse("Could not check your account. Please try again.")
	}
	if appErr != nil || !user.IsSystemAdmin() {
		return ephemeralResponse("Only System Admins can run `/mattermost-request-hub exercise`.")
	}
	if args.TeamId == "" {
		return ephemeralResponse("Run `/mattermost-request-hub exercise` from a channel in the team to seed.")
	}
	if len(params) != 1 {
		return ephemeralResponse(exerciseUsage)
	}

	sub := params[0].Text
	var (
		reply string
		err   error
	)
	switch sub {
	case exercisePopulate:
		reply, err = p.populateExercise(args.TeamId, args.UserId)
	case exerciseTeardown:
		reply, err = p.teardownExercise(args.TeamId, args.UserId)
	case exerciseStatus:
		reply, err = p.exerciseStatus(args.TeamId)
	default:
		return ephemeralResponse(fmt.Sprintf("Unknown exercise subcommand `%s`. %s", sub, exerciseUsage))
	}
	if err != nil {
		p.API.LogWarn("exercise command failed", "subcommand", sub, "team_id", args.TeamId, "error", err.Error())
		return ephemeralResponse(fmt.Sprintf("Exercise %s failed: %s.", sub, strings.TrimRight(err.Error(), ".")))
	}
	return ephemeralResponse(reply)
}
