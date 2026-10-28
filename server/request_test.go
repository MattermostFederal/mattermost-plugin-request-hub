package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// channelAdminRoles is the role string createChannelForRequest promotes
// designated admins to. Kept here so the test breaks loudly if the
// production string changes.
const channelAdminRoles = "channel_user channel_admin"

// newTestPlugin wires a Plugin up to a mock API with a known bot user.
// Every request type defaults to enabled so tests don't each set toggles;
// a test that calls setConfiguration replaces these.
func newTestPlugin(api *plugintest.API) *Plugin {
	p := &Plugin{}
	p.SetAPI(api)
	p.botUserID = "bot-user-id"
	p.setConfiguration(&configuration{
		AllowChannelRequests:      true,
		AllowChannelAdminRequests: true,
		AllowTeamRequests:         true,
		AllowTeamAdminRequests:    true,
		AllowTeamMemberRequests:   true,
		AllowTeamGuestRequests:    true,
		AllowBotTokenRequests:     true,
		AllowWebhookRequests:      true,
	})
	return p
}

// stubLogs registers permissive log expectations so production code can
// log at any level/arity without the test caring about the exact calls.
func stubLogs(api *plugintest.API) {
	stubLogLevels(api)
	// CAS re-store of post IDs after approval/DM create — most queue tests
	// don't care about the success path beyond "submitted". Pending lists are
	// left to stubPendingLists / fakePendingLists.
	api.On("KVCompareAndSet", isNotPendingListKey, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	stubApprovalTeamRecord(api)
	// Ticket relay lookups refreshed before an approve/deny claim, and the
	// approval card record saved at submit and cleared at close.
	api.On("KVSet", mock.MatchedBy(func(key string) bool {
		return strings.HasPrefix(key, kvApprovalPostPrefix) || strings.HasPrefix(key, kvDMRootPrefix) ||
			strings.HasPrefix(key, kvApprovalCardsPrefix)
	}), mock.Anything).Return(nil).Maybe()
	api.On("KVDelete", mock.MatchedBy(func(key string) bool {
		return strings.HasPrefix(key, kvApprovalCardsPrefix)
	})).Return(nil).Maybe()
}

// stubLogLines registers only the permissive log expectations, for tests
// that back the KV store with fakeKV.
func stubLogLines(api *plugintest.API) {
	stubLogLevels(api)
}

// stubLogLevels registers permissive log expectations and nothing else, for
// tests that assert no other API call is made.
func stubLogLevels(api *plugintest.API) {
	for _, method := range []string{"LogDebug", "LogInfo", "LogWarn", "LogError"} {
		for arity := 1; arity <= 16; arity++ {
			args := make([]any, arity)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(method, args...).Maybe()
		}
	}
}

// stubApprovalTeamRecord lets approvalTeam read and write its record of the
// Approval team; nothing is recorded yet.
func stubApprovalTeamRecord(api *plugintest.API) {
	api.On("KVGet", kvApprovalTeamKey).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", kvApprovalTeamKey, mock.Anything, mock.Anything).Return(true, nil).Maybe()
}

// pluginChannel returns a private channel the plugin's bot created, the
// only kind used as an approval channel.
func pluginChannel(id, name string) *model.Channel {
	return &model.Channel{Id: id, Name: name, Type: model.ChannelTypePrivate, CreatorId: "bot-user-id"}
}

// Valid IDs for fixtures that pass through input checks (model.IsValidId).
const (
	testNomineeID = "nomineeuseridaaaaaaaaaaaaa"
	testChannelID = "guestchannelidaaaaaaaaaaaa"
)

// testAppErr builds a throwaway *model.AppError for failure-path mocks.
func testAppErr(msg string) *model.AppError {
	return model.NewAppError("test", "test.error", nil, msg, http.StatusInternalServerError)
}

func TestCreateChannelForRequest_AddsMembersAndPromotesAdmins(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	created := &model.Channel{Id: "ch1", Name: "team-marketing"}

	// Assert the channel is created from the request's fields verbatim.
	api.On("CreateChannel", mock.MatchedBy(func(c *model.Channel) bool {
		return c.TeamId == "team1" &&
			c.Name == "team-marketing" &&
			c.DisplayName == "Marketing" &&
			c.Type == model.ChannelTypeOpen &&
			c.Purpose == "Coordination" &&
			c.CreatorId == "u_req"
	})).Return(created, nil)
	api.On("AddChannelMember", "ch1", mock.Anything).Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_a1", channelAdminRoles).Return(&model.ChannelMember{}, nil)

	req := &channelRequest{
		RequesterID:    "u_req",
		TeamID:         "team1",
		Name:           "team-marketing",
		DisplayName:    "Marketing",
		Purpose:        "Coordination",
		ChannelType:    "O",
		MemberIDs:      []string{"u_m1", "u_m2"},
		AdminMemberIDs: []string{"u_a1"},
	}

	channel, err := p.createChannelForRequest(req)

	require.NoError(t, err)
	require.Equal(t, "ch1", channel.Id)

	// Requester + two members + one admin, each added exactly once.
	api.AssertNumberOfCalls(t, "AddChannelMember", 4)
	api.AssertCalled(t, "AddChannelMember", "ch1", "u_req")
	api.AssertCalled(t, "AddChannelMember", "ch1", "u_m1")
	api.AssertCalled(t, "AddChannelMember", "ch1", "u_m2")
	api.AssertCalled(t, "AddChannelMember", "ch1", "u_a1")

	// Only the designated admin is promoted, and only once.
	api.AssertNumberOfCalls(t, "UpdateChannelMemberRoles", 1)
	api.AssertCalled(t, "UpdateChannelMemberRoles", "ch1", "u_a1", channelAdminRoles)
}

func TestCreateChannelForRequest_DeduplicatesUsers(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1"}, nil)
	api.On("AddChannelMember", "ch1", mock.Anything).Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_dup", channelAdminRoles).Return(&model.ChannelMember{}, nil)

	// Requester repeated as a member, and one user listed as both a member
	// and an admin. Neither should be added twice.
	req := &channelRequest{
		RequesterID:    "u_req",
		TeamID:         "team1",
		Name:           "team-x",
		ChannelType:    "O",
		MemberIDs:      []string{"u_req", "u_dup"},
		AdminMemberIDs: []string{"u_dup"},
	}

	_, err := p.createChannelForRequest(req)

	require.NoError(t, err)
	// Unique users only: u_req, u_dup.
	api.AssertNumberOfCalls(t, "AddChannelMember", 2)
	// u_dup is an admin -> promoted exactly once despite appearing twice.
	api.AssertNumberOfCalls(t, "UpdateChannelMemberRoles", 1)
	api.AssertCalled(t, "UpdateChannelMemberRoles", "ch1", "u_dup", channelAdminRoles)
}

func TestCreateChannelForRequest_SkipsEmptyIDs(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1"}, nil)
	api.On("AddChannelMember", "ch1", mock.Anything).Return(&model.ChannelMember{}, nil)

	req := &channelRequest{
		RequesterID:    "u_req",
		TeamID:         "team1",
		Name:           "team-x",
		ChannelType:    "O",
		MemberIDs:      []string{"", "u_m1"},
		AdminMemberIDs: []string{""},
	}

	_, err := p.createChannelForRequest(req)

	require.NoError(t, err)
	// Empty IDs are skipped: only u_req and u_m1 are added.
	api.AssertNumberOfCalls(t, "AddChannelMember", 2)
	// The empty admin entry must not trigger any promotion.
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", mock.Anything, mock.Anything, mock.Anything)
}

func TestCreateChannelForRequest_CreateChannelErrorAbortsEarly(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateChannel", mock.Anything).Return(nil, testAppErr("boom"))

	req := &channelRequest{
		RequesterID: "u_req",
		TeamID:      "team1",
		Name:        "team-x",
		ChannelType: "O",
		MemberIDs:   []string{"u_m1"},
	}

	channel, err := p.createChannelForRequest(req)

	require.Error(t, err)
	require.Nil(t, channel)
	// No member work happens if the channel itself can't be created.
	api.AssertNotCalled(t, "AddChannelMember", mock.Anything, mock.Anything)
}

func TestCreateChannelForRequest_AddMemberFailureIsNonFatalAndSkipsPromotion(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1"}, nil)
	// u_bad can't be added; register it before the catch-all so testify
	// matches the specific expectation first.
	api.On("AddChannelMember", "ch1", "u_bad").Return(nil, testAppErr("cannot add"))
	api.On("AddChannelMember", "ch1", mock.Anything).Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", mock.Anything, mock.Anything).Return(&model.ChannelMember{}, nil)

	// Both u_bad and u_good are admins; only u_good is added successfully.
	req := &channelRequest{
		RequesterID:    "u_req",
		TeamID:         "team1",
		Name:           "team-x",
		ChannelType:    "O",
		AdminMemberIDs: []string{"u_bad", "u_good"},
	}

	channel, err := p.createChannelForRequest(req)

	// A member that can't be added is logged, not fatal.
	require.NoError(t, err)
	require.Equal(t, "ch1", channel.Id)
	api.AssertCalled(t, "AddChannelMember", "ch1", "u_bad")
	// A user that failed to be added must not be promoted...
	api.AssertNotCalled(t, "UpdateChannelMemberRoles", "ch1", "u_bad", channelAdminRoles)
	// ...but a successfully-added admin still is.
	api.AssertCalled(t, "UpdateChannelMemberRoles", "ch1", "u_good", channelAdminRoles)
}

func TestCreateChannelForRequest_TrimsListsToActualAdds(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1"}, nil)
	// u_bad can't be added; register before the catch-all so testify
	// matches the specific expectation first.
	api.On("AddChannelMember", "ch1", "u_bad").Return(nil, testAppErr("cannot add"))
	api.On("AddChannelMember", "ch1", mock.Anything).Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", mock.Anything, mock.Anything).Return(&model.ChannelMember{}, nil)

	req := &channelRequest{
		RequesterID:    "u_req",
		TeamID:         "team1",
		Name:           "team-x",
		ChannelType:    "O",
		MemberIDs:      []string{"u_bad", "u_good"},
		AdminMemberIDs: []string{"u_admin"},
	}

	_, err := p.createChannelForRequest(req)
	require.NoError(t, err)

	// The lists are trimmed to who was actually added/promoted so the
	// welcome message can't claim a user was added when the API call
	// failed (e.g. a non-team-member).
	require.Equal(t, []string{"u_good"}, req.MemberIDs)
	require.Equal(t, []string{"u_admin"}, req.AdminMemberIDs)
}

func TestResolvePrefixedName_PrefixWithoutHyphen(t *testing.T) {
	prefixes := []channelPrefix{{Prefix: "eng-"}, {Prefix: "ops"}, {Prefix: "ops-"}}

	name, err := resolvePrefixedName(prefixes, requestInput{Prefix: "eng", Name: "chat"})
	require.NoError(t, err)
	require.Equal(t, "eng-chat", name)

	// An exact match wins over the hyphen fallback.
	name, err = resolvePrefixedName(prefixes, requestInput{Prefix: "ops", Name: "chat"})
	require.NoError(t, err)
	require.Equal(t, "opschat", name)

	_, err = resolvePrefixedName(prefixes, requestInput{Prefix: "nope", Name: "chat"})
	require.EqualError(t, err, `prefix "nope" is not in the list of allowed prefixes: eng-, ops, ops-`)
}

func TestResolvePrefixedName_AnchoredSuffixMatchesWholeSuffix(t *testing.T) {
	noopLog := func(string, ...any) {}
	// Third pipe-field is the suffix regex "dev|development".
	prefixes := parsePrefixList("proj-|Projects|dev|development", noopLog)
	require.Len(t, prefixes, 1)

	// The longer alternative must be accepted — the old leftmost-first
	// FindStringIndex check wrongly rejected "development" because it
	// matched only the "dev" alternative.
	name, err := resolvePrefixedName(prefixes, requestInput{Prefix: "proj-", Name: "development"})
	require.NoError(t, err)
	require.Equal(t, "proj-development", name)

	// The short alternative still works.
	name, err = resolvePrefixedName(prefixes, requestInput{Prefix: "proj-", Name: "dev"})
	require.NoError(t, err)
	require.Equal(t, "proj-dev", name)

	// A suffix matching neither alternative is rejected.
	_, err = resolvePrefixedName(prefixes, requestInput{Prefix: "proj-", Name: "prod"})
	require.Error(t, err)
}

func TestResolvePrefixedName_NumericSuffixRuleIsLengthBounded(t *testing.T) {
	noopLog := func(string, ...any) {}
	// Numeric third field -> max suffix length (compiled as ^[a-z0-9-]{2,8}$).
	prefixes := parsePrefixList("team-|Teams|8", noopLog)
	require.Len(t, prefixes, 1)

	name, err := resolvePrefixedName(prefixes, requestInput{Prefix: "team-", Name: "sales"})
	require.NoError(t, err)
	require.Equal(t, "team-sales", name)

	// Over the length bound -> rejected (anchored, so no partial match).
	_, err = resolvePrefixedName(prefixes, requestInput{Prefix: "team-", Name: "supercalifragilistic"})
	require.Error(t, err)
}

func TestSubmitRequest_NonMemberRejected(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "team1", "u1").Return(nil, testAppErr("not a member"))

	_, err := p.submitRequest(requestInput{
		RequesterID: "u1",
		TeamID:      "team1",
		DisplayName: "Marketing",
		Prefix:      "team-",
		Name:        "marketing",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "member of the team")
	// Rejected before any channel work happens.
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestSubmitRequest_SystemAdminRequestQueues(t *testing.T) {
	// System Admins get an approval card like everyone else. They may
	// request in any team, so their team membership isn't checked.
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		prefixes:     []channelPrefix{{Prefix: "team-"}},
		ApprovalTeam: "appteam",
	})

	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", Name: "appteam"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-channel-requests", true).
		Return(pluginChannel("approval-ch", "mattermost-channel-requests"), nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "team", DisplayName: "Team"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "approval-1"}, nil)
	api.On("GetDirectChannel", "admin1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)

	msg, err := p.submitRequest(requestInput{
		RequesterID: "admin1",
		TeamID:      "team1",
		DisplayName: "Marketing",
		Prefix:      "team-",
		Name:        "marketing",
	})
	require.NoError(t, err)
	require.Contains(t, msg, "submitted")
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
	api.AssertCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestSubmitRequest_CardRecordFailureRollsBack(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	// Pending lists only: the card record write must reach the failing mock below.
	api.On("KVGet", isPendingListKey).Return(nil, nil).Maybe()
	api.On("KVCompareAndSet", isPendingListKey, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	api.On("KVCompareAndDelete", isPendingListKey, mock.Anything).Return(true, nil).Maybe()
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{
		AllowChannelRequests: true,
		prefixes:             []channelPrefix{{Prefix: "team-"}},
		ApprovalTeam:         "appteam",
	})

	isCardKey := func(key string) bool { return strings.HasPrefix(key, kvApprovalCardsPrefix) }
	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("KVSet", mock.MatchedBy(isCardKey), mock.Anything).Return(testAppErr("kv down"))
	api.On("KVSet", mock.MatchedBy(func(key string) bool { return !isCardKey(key) }), mock.Anything).Return(nil)
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", Name: "appteam"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-channel-requests", true).
		Return(pluginChannel("approval-ch", "mattermost-channel-requests"), nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "team", DisplayName: "Team"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "approval-1"}, nil).Once()
	api.On("DeletePost", "approval-1").Return(nil).Once()
	api.On("KVDelete", mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvRequestPrefix) })).Return(nil).Once()

	_, err := p.submitRequest(requestInput{
		RequesterID: "admin1",
		TeamID:      "team1",
		DisplayName: "Marketing",
		Prefix:      "team-",
		Name:        "marketing",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "could not send your request")
	api.AssertNotCalled(t, "GetDirectChannel", mock.Anything, mock.Anything)
}

func TestSubmitRequest_TakenApprovalChannelNameSendsCardToSystemAdmins(t *testing.T) {
	for _, taken := range []*model.Channel{
		{Id: "taken-ch", Name: "mattermost-channel-requests", Type: model.ChannelTypeOpen, CreatorId: "bot-user-id"},
		{Id: "taken-ch", Name: "mattermost-channel-requests", Type: model.ChannelTypePrivate, CreatorId: "u_other"},
	} {
		t.Run("type "+string(taken.Type), func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			stubPendingLists(api)
			stubSubmittedRequestExists(api, kvRequestPrefix)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(&configuration{
				AllowChannelRequests: true,
				prefixes:             []channelPrefix{{Prefix: "team-"}},
				ApprovalTeam:         "appteam",
			})

			api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
			api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
			api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", Name: "appteam"}, nil)
			api.On("GetChannelByName", "appteam-id", "mattermost-channel-requests", true).Return(taken, nil)
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "team", DisplayName: "Team"}, nil)
			api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "sa1"}}, nil)
			api.On("GetDirectChannel", mock.Anything, "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
			api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "copy-1"}, nil)

			msg, err := p.submitRequest(requestInput{
				RequesterID: "admin1",
				TeamID:      "team1",
				DisplayName: "Marketing",
				Prefix:      "team-",
				Name:        "marketing",
			})

			require.NoError(t, err)
			require.Contains(t, msg, "submitted")
			api.AssertCalled(t, "GetUsers", mock.Anything)
			api.AssertNotCalled(t, "CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "taken-ch" }))
			api.AssertNotCalled(t, "CreateChannel", mock.Anything)
		})
	}
}

func TestSubmitRequest_SoftDeletedMemberRejected(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Roles: "system_user"}, nil)
	// A user who LEFT the team: GetTeamMember returns a row with DeleteAt set.
	api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{TeamId: "team1", UserId: "u1", DeleteAt: 123}, nil)

	_, err := p.submitRequest(requestInput{
		RequesterID: "u1",
		TeamID:      "team1",
		DisplayName: "Marketing",
		Prefix:      "team-",
		Name:        "marketing",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "member of the team")
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestValidateRequestInput(t *testing.T) {
	tooManyMembers := make([]string, maxMembersPerList+1)
	for i := range tooManyMembers {
		tooManyMembers[i] = model.NewId()
	}

	cases := []struct {
		name    string
		in      requestInput
		wantErr string // "" means expect no error
	}{
		{
			name: "valid",
			in:   requestInput{DisplayName: "Marketing", TeamID: "team1"},
		},
		{
			name:    "empty display name",
			in:      requestInput{DisplayName: "   ", TeamID: "team1"},
			wantErr: "channel name is required",
		},
		{
			name:    "display name too long",
			in:      requestInput{DisplayName: strings.Repeat("x", maxDisplayNameLen+1), TeamID: "team1"},
			wantErr: "characters or fewer",
		},
		{
			name:    "purpose too long",
			in:      requestInput{DisplayName: "Marketing", TeamID: "team1", Purpose: strings.Repeat("x", maxPurposeLen+1)},
			wantErr: "purpose must be",
		},
		{
			name:    "too many members",
			in:      requestInput{DisplayName: "Marketing", TeamID: "team1", MemberIDs: tooManyMembers},
			wantErr: "too many members",
		},
		{
			name:    "too many admins",
			in:      requestInput{DisplayName: "Marketing", TeamID: "team1", AdminMemberIDs: tooManyMembers},
			wantErr: "too many members",
		},
		{
			name:    "missing team",
			in:      requestInput{DisplayName: "Marketing", TeamID: "  "},
			wantErr: "a team is required",
		},
		{
			// Dialog submissions carry raw strings.
			name:    "member that is not a user ID",
			in:      requestInput{DisplayName: "Marketing", TeamID: "team1", MemberIDs: []string{strings.Repeat("A", 100000)}},
			wantErr: "valid users",
		},
		{
			name:    "admin that is not a user ID",
			in:      requestInput{DisplayName: "Marketing", TeamID: "team1", AdminMemberIDs: []string{"alice"}},
			wantErr: "valid users",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRequestInput(tc.in)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestPromoteSoleMember(t *testing.T) {
	tests := []struct {
		name        string
		members     []string
		admins      []string
		wantMembers []string
		wantAdmins  []string
	}{
		{
			name:        "sole member, no admins -> member promoted",
			members:     []string{"u1"},
			admins:      nil,
			wantMembers: nil,
			wantAdmins:  []string{"u1"},
		},
		{
			name:        "multiple members, no admins -> unchanged",
			members:     []string{"u1", "u2"},
			admins:      nil,
			wantMembers: []string{"u1", "u2"},
			wantAdmins:  nil,
		},
		{
			name:        "sole member, admins already set -> unchanged",
			members:     []string{"u1"},
			admins:      []string{"u2"},
			wantMembers: []string{"u1"},
			wantAdmins:  []string{"u2"},
		},
		{
			name:        "no members, no admins -> unchanged",
			members:     nil,
			admins:      nil,
			wantMembers: nil,
			wantAdmins:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			members, admins := promoteSoleMember(tt.members, tt.admins)
			require.Equal(t, tt.wantMembers, members)
			require.Equal(t, tt.wantAdmins, admins)
		})
	}
}

func TestResolveUsernameList(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	api.On("GetUserByUsername", "alice").Return(&model.User{Id: "id_alice"}, nil)
	api.On("GetUserByUsername", "bob").Return(&model.User{Id: "id_bob"}, nil)

	// Leading @ and surrounding whitespace are stripped; blank entries dropped.
	ids, err := p.resolveUsernameList([]string{"@alice", " bob ", ""})

	require.NoError(t, err)
	require.Equal(t, []string{"id_alice", "id_bob"}, ids)
}

func TestResolveUsernameList_RejectsTooMany(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	names := make([]string, maxMembersPerList+1)
	for i := range names {
		names[i] = "user"
	}

	ids, err := p.resolveUsernameList(names)

	// Bounded before any per-username lookup (no GetUserByUsername mocks
	// registered, so a lookup would fail the test).
	require.Error(t, err)
	require.Nil(t, ids)
	require.Contains(t, err.Error(), "too many users")
}

func TestResolveUsernameList_UnknownUserErrors(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	api.On("GetUserByUsername", "ghost").Return(nil, testAppErr("not found"))

	ids, err := p.resolveUsernameList([]string{"ghost"})

	require.Error(t, err)
	require.Nil(t, ids)
	require.Contains(t, err.Error(), "ghost")
}

func TestResolvePrefixedName_RejectsNamesOverMaxLength(t *testing.T) {
	prefixes := []channelPrefix{{Prefix: "team-"}}

	// slugify caps the suffix at 64, but prefix + suffix can still exceed
	// the 64-char column and would only fail when an approver clicks.
	_, err := resolvePrefixedName(prefixes, requestInput{Prefix: "team-", DisplayName: strings.Repeat("a", 64)})
	require.Error(t, err)

	name, err := resolvePrefixedName(prefixes, requestInput{Prefix: "team-", Name: strings.Repeat("a", model.ChannelNameMaxLength-len("team-"))})
	require.NoError(t, err)
	require.Len(t, name, model.ChannelNameMaxLength)
}

func TestResolvePrefixedName_RequiresMostSpecificPrefix(t *testing.T) {
	noopLog := func(string, ...any) {}
	prefixes := parsePrefixList("team-|Teams|32\nteam-ops-|Ops teams|6", noopLog)

	// A name that starts with a more specific prefix must use that prefix.
	_, err := resolvePrefixedName(prefixes, requestInput{Prefix: "team-", Name: "ops-anything-very-long"})
	require.Error(t, err)
	require.Contains(t, err.Error(), `"team-ops-"`)

	name, err := resolvePrefixedName(prefixes, requestInput{Prefix: "team-ops-", Name: "alpha"})
	require.NoError(t, err)
	require.Equal(t, "team-ops-alpha", name)
}

func TestEscapeMarkdown(t *testing.T) {
	got := escapeMarkdown("a` @here [b](https://example.com) ![](https://example.com/i.png)")

	require.NotContains(t, got, "@here")
	require.NotContains(t, got, "](")
	require.NotContains(t, got, "https://")
	// ":" stays unescaped: the emoticon parser ignores the backslash, so
	// "\:/" rendered as a stray backslash and a :/ emoji.
	require.NotContains(t, got, `\:`)
	require.Contains(t, got, "\\`")
	require.Equal(t, "Marketing Team", escapeMarkdown("Marketing Team"))
}

func TestApprovalAttachment_RendersRequesterTextAsTextAndNamesTeam(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "ops", DisplayName: "Operations"}, nil)

	req := &channelRequest{
		ID:          "req1",
		TeamID:      "team1",
		Name:        "team-x",
		DisplayName: "Ops ![](https://example.com/i.png)",
		Purpose:     "**Bold @someone** [details](https://example.com)\n# Heading",
		ChannelType: channelTypeOpen,
	}
	att := p.approvalAttachment(req, &model.User{Username: "req"})

	values := map[string]string{}
	for _, f := range att.Fields {
		values[f.Title], _ = f.Value.(string)
	}
	require.Contains(t, values["Team"], "Operations")
	// The purpose is rendered in the card text, the name in a field.
	for title, text := range map[string]string{"Channel name": values["Channel name"], "Purpose": att.Text} {
		require.NotContains(t, text, "](", title)
		require.NotContains(t, text, "@someone", title)
		require.NotContains(t, text, "\n#", title)
	}
	require.Contains(t, att.Text, "Purpose")
}

// Regression: CreateChannelSidebarCategory returning (cat, nil) must not
// surface a typed-nil *AppError as a non-nil error — that panicked
// ServeHTTP after Approve and skipped the soft-squash update.
func TestPlaceChannelInCategory_CreateSuccessReturnsRealNil(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetChannelSidebarCategories", "u1", "team1").Return(&model.OrderedSidebarCategories{
		Categories: []*model.SidebarCategoryWithChannels{},
	}, nil)
	api.On("CreateChannelSidebarCategory", "u1", "team1", mock.MatchedBy(func(c *model.SidebarCategoryWithChannels) bool {
		return c != nil && c.DisplayName == "Requests" && len(c.Channels) == 1 && c.Channels[0] == "ch1"
	})).Return(&model.SidebarCategoryWithChannels{}, nil)

	err := p.placeChannelInCategory("u1", "team1", "ch1", "Requests")
	require.NoError(t, err)
	// Prove the interface value is truly nil (not typed-nil *AppError).
	require.Nil(t, err)
}

func TestPostPermalink(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	t.Run("empty post id", func(t *testing.T) {
		require.Equal(t, "", p.postPermalink(""))
	})

	t.Run("builds team pl link", func(t *testing.T) {
		api.On("GetConfig").Return(&model.Config{
			ServiceSettings: model.ServiceSettings{SiteURL: new("https://mm.example.com")},
		})
		api.On("GetPost", "post1").Return(&model.Post{Id: "post1", ChannelId: "ch1"}, nil)
		api.On("GetChannel", "ch1").Return(&model.Channel{Id: "ch1", TeamId: "team1"}, nil)
		api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "ops"}, nil)

		require.Equal(t, "https://mm.example.com/ops/pl/post1", p.postPermalink("post1"))
	})
}

func TestAuditCardStyle(t *testing.T) {
	color, title := auditCardStyle(auditApproved)
	require.Equal(t, colorApproved, color)
	require.Equal(t, "✅ Approved", title)

	color, title = auditCardStyle(auditDenied)
	require.Equal(t, colorDenied, color)
	require.Equal(t, "❌ Denied", title)
}

func TestApprovalAttachment_IncludesTeamProminently(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	api.On("GetTeam", "team1").Return(&model.Team{
		Id: "team1", Name: "testing-team", DisplayName: "Testing Team Creation Admin",
	}, nil)

	att := p.approvalAttachment(&channelRequest{
		ID:          "req1",
		TeamID:      "team1",
		DisplayName: "testing 2",
		Name:        "ops-testing-2",
		ChannelType: channelTypeOpen,
	}, &model.User{Id: "u1", Username: "chris.fickess"})

	require.Equal(t, "Channel creation request — Testing Team Creation Admin", att.Title)
	require.NotEmpty(t, att.Fields)
	require.Equal(t, "Team", att.Fields[0].Title)
	require.Equal(t, "Testing Team Creation Admin", att.Fields[0].Value)
	require.False(t, bool(att.Fields[0].Short), "Team should be full-width so it stands out")
}

// isRequestRecordKey matches the KV key of any stored channel request.
var isRequestRecordKey = mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, kvRequestPrefix) })

// approvalConfig enables channel requests and routes approvals to the
// auto-created approval channel in team "appteam" (mocked as "appch" by
// mockApprovalChannel).
func approvalConfig() *configuration {
	return &configuration{
		AllowChannelRequests: true,
		prefixes:             []channelPrefix{{Prefix: "team-"}},
		ApprovalTeam:         "appteam",
	}
}

// mockApprovalChannel resolves the root approval channel to "appch", a
// channel the plugin created in team "appteam". Pair it with
// stubApprovalTeamRecord (stubLogs includes it).
func mockApprovalChannel(api *plugintest.API) {
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", Name: "appteam"}, nil)
	ch := pluginChannel("appch", "mattermost-channel-requests")
	ch.TeamId = "appteam-id"
	api.On("GetChannelByName", "appteam-id", "mattermost-channel-requests", true).Return(ch, nil)
}

// mockMemberRequester sets up a regular (non-admin) requester u1 who is an
// active member of team1.
func mockMemberRequester(api *plugintest.API) {
	api.On("GetUser", "u1").Return(&model.User{Id: "u1", Username: "req", Roles: "system_user"}, nil)
	api.On("GetTeamMember", "team1", "u1").Return(&model.TeamMember{TeamId: "team1", UserId: "u1"}, nil)
}

func TestSubmitRequest_StoresAndPostsForApproval(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	mockMemberRequester(api)
	var stored channelRequest
	api.On("KVSet", isRequestRecordKey, mock.Anything).Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal(args.Get(1).([]byte), &stored))
	}).Return(nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	mockApprovalChannel(api)
	var approval *model.Post
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "appch" })).
		Run(func(args mock.Arguments) { approval = args.Get(0).(*model.Post) }).Return(&model.Post{Id: "card1"}, nil)
	// The requester gets a ticket DM for the pending request.
	api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm1" })).Return(&model.Post{Id: "dmroot"}, nil)
	// Thread lookups for reply relaying.
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()

	msg, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "  Marketing   Team ", Prefix: "team-", ChannelType: "bogus"})

	require.NoError(t, err)
	require.Contains(t, msg, "submitted for approval")
	require.Equal(t, "team-marketing-team", stored.Name)
	require.Equal(t, "Marketing Team", stored.DisplayName)
	require.Equal(t, channelTypeOpen, stored.ChannelType) // unknown types default to public
	require.NotNil(t, approval)
	require.Equal(t, "appch", approval.ChannelId)
	require.Len(t, approval.Attachments(), 1)
	api.AssertNotCalled(t, "CreateChannel", mock.Anything)
}

func TestSubmitRequest_StoreFailure(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	mockMemberRequester(api)
	api.On("KVSet", isRequestRecordKey, mock.Anything).Return(testAppErr("kv down"))

	_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})

	require.EqualError(t, err, "could not save your request; try again")
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestSubmitRequest_ApprovalPostFailureRollsBack(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	stubPendingLists(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	mockMemberRequester(api)
	var key string
	api.On("KVSet", isRequestRecordKey, mock.Anything).Run(func(args mock.Arguments) { key = args.String(0) }).Return(nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng", DisplayName: "Engineering"}, nil)
	mockApprovalChannel(api)
	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))
	api.On("KVDelete", isRequestRecordKey).Return(nil)

	_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})

	require.ErrorContains(t, err, "could not send your request to the approvers")
	api.AssertCalled(t, "KVDelete", key)
}

func TestSubmitRequest_RequesterLookupFails(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	api.On("GetUser", "u1").Return(nil, testAppErr("gone"))

	_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})

	require.EqualError(t, err, "could not load your account; try again")
}

func TestSubmitRequest_InvalidInputSkipsLookups(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1"})

	require.EqualError(t, err, "a channel name is required")
	api.AssertNotCalled(t, "GetUser", mock.Anything)
}

func TestSubmitRequest_ChannelNameErrors(t *testing.T) {
	cases := []struct {
		name   string
		config *configuration
		in     requestInput
		want   string
	}{
		{name: "no prefixes configured", config: &configuration{}, in: requestInput{DisplayName: "Ops", Prefix: "team-"}, want: "plugin is not configured"},
		{name: "no prefix picked", config: approvalConfig(), in: requestInput{DisplayName: "Ops"}, want: "please pick a channel prefix"},
		{name: "unknown prefix", config: approvalConfig(), in: requestInput{DisplayName: "Ops", Prefix: "nope-"}, want: `prefix "nope-" is not in the list`},
		{name: "suffix empty after cleanup", config: approvalConfig(), in: requestInput{DisplayName: "!!!", Name: "---", Prefix: "team-"}, want: "channel name suffix is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(tc.config)
			// A System Admin skips the team-membership check.
			api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Roles: "system_user system_admin"}, nil)

			tc.in.RequesterID, tc.in.TeamID = "admin1", "team1"
			_, err := p.submitRequest(tc.in)

			require.ErrorContains(t, err, tc.want)
			api.AssertNotCalled(t, "CreateChannel", mock.Anything)
		})
	}
}

func TestPostApprovalAttachment_ChannelPostFailureIsReturned(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))

	_, _, err := p.postApprovalAttachment("r1", &model.MessageAttachment{Title: "x"}, "header", &model.Channel{Id: "appch", TeamId: "appteam"})

	require.ErrorContains(t, err, "failed to post approval request")
	// A failed post in a working channel is not retried as DMs.
	api.AssertNotCalled(t, "GetUsers", mock.Anything)
}

func TestPostApprovalAttachment_FallsBackToSystemAdminDMs(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	api.On("GetUsers", mock.MatchedBy(func(o *model.UserGetOptions) bool { return o.Role == model.SystemAdminRoleId })).
		Return([]*model.User{{Id: "bot-user-id"}, {Id: "a1"}, {Id: "a2"}, {Id: "a3"}}, nil)
	api.On("GetDirectChannel", "a1", "bot-user-id").Return(nil, testAppErr("no dm"))
	api.On("GetDirectChannel", "a2", "bot-user-id").Return(&model.Channel{Id: "dm2"}, nil)
	api.On("GetDirectChannel", "a3", "bot-user-id").Return(&model.Channel{Id: "dm3"}, nil)
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm2" })).Return(nil, testAppErr("post failed"))
	api.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool { return post.ChannelId == "dm3" })).Return(&model.Post{Id: "post3"}, nil)

	// No approval channel resolved: the card goes to System Admin DMs.
	postID, channelID, err := p.postApprovalAttachment("r1", &model.MessageAttachment{Title: "x"}, "header", nil)
	require.NoError(t, err)
	require.Empty(t, postID)
	require.Empty(t, channelID)
	// Only the copies that were created are recorded.
	api.AssertCalled(t, "KVSet", kvApprovalCardsPrefix+"r1", []byte(`["post3"]`))
	// The bot never DMs itself.
	api.AssertNotCalled(t, "GetDirectChannel", "bot-user-id", "bot-user-id")
}

func TestPostApprovalToSystemAdmins_Failures(t *testing.T) {
	t.Run("listing admins fails", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetUsers", mock.Anything).Return(nil, testAppErr("db down"))

		_, err := p.postApprovalToSystemAdmins(&model.MessageAttachment{})
		require.ErrorContains(t, err, "failed to list System Admins")
	})

	t.Run("nobody receives it", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: "bot-user-id"}}, nil)

		_, err := p.postApprovalToSystemAdmins(&model.MessageAttachment{})
		require.EqualError(t, err, "no System Admins are available to receive the approval request")
	})
}

func TestResolveApprovalChannel_Unavailable(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{})
		ch, label := p.resolveApprovalChannel("team1")
		require.Nil(t, ch)
		require.Empty(t, label)
		api.AssertNotCalled(t, "GetChannelByNameForTeamName", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("approval team not found", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		p.setConfiguration(&configuration{ApprovalTeam: "appteam"})
		api.On("GetTeamByName", "appteam").Return(nil, testAppErr("gone"))
		ch, _ := p.resolveApprovalChannel("team1")
		require.Nil(t, ch)
		api.AssertNotCalled(t, "CreateChannel", mock.Anything)
	})
}

func TestApprovalAttachment_PrivateWithMembersAndUnknownTeam(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("GetTeam", "team1").Return(nil, testAppErr("gone"))
	api.On("GetUser", "u_mem").Return(&model.User{Id: "u_mem", Username: "mem"}, nil)
	api.On("GetUser", "u_adm").Return(&model.User{Id: "u_adm", Username: "adm"}, nil)

	att := p.approvalAttachment(&channelRequest{
		ID: "req1", TeamID: "team1", Name: "team-x", DisplayName: "X", ChannelType: channelTypePrivate,
		MemberIDs: []string{"u_mem"}, AdminMemberIDs: []string{"u_adm"},
	}, &model.User{Username: "req"})

	fields := map[string]string{}
	for _, f := range att.Fields {
		fields[f.Title] = f.Value.(string)
	}
	require.Equal(t, "Private", fields["Visibility"])
	require.Equal(t, "team1", fields["Team"]) // falls back to the raw ID
	// Member lists sit in the collapsible Text, not the fields.
	require.Contains(t, att.Text, "**Members to add:** @mem")
	require.Contains(t, att.Text, "**Channel Admins to add:** @adm")
}

func TestLoadRequest_Errors(t *testing.T) {
	t.Run("store read fails", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", kvRequestPrefix+"r1").Return(nil, testAppErr("kv down"))
		_, _, err := p.loadRequest("r1")
		require.ErrorContains(t, err, "failed to load request")
	})

	t.Run("corrupt record", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("KVGet", kvRequestPrefix+"r1").Return([]byte("not json"), nil)
		_, _, err := p.loadRequest("r1")
		require.ErrorContains(t, err, "failed to unmarshal request")
	})
}

func TestBotPosts_FailuresAreLoggedNotReturned(t *testing.T) {
	t.Run("welcome message", func(t *testing.T) {
		api := &plugintest.API{}
		stubLogs(api)
		defer api.AssertExpectations(t)
		p := newTestPlugin(api)
		api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))

		p.postWelcomeMessage(&model.Channel{Id: "ch1"}, &channelRequest{}, &model.User{Id: "u1", Username: "req"}, nil)

		api.AssertCalled(t, "LogWarn", "failed to post welcome message", "channel_id", "ch1", "error", mock.Anything)
	})
}

func TestCreateChannelForRequest_PromotionFailureIsNonFatal(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	api.On("CreateChannel", mock.Anything).Return(&model.Channel{Id: "ch1", Name: "team-x"}, nil)
	api.On("AddChannelMember", "ch1", "u_req").Return(&model.ChannelMember{}, nil)
	api.On("AddChannelMember", "ch1", "u_adm").Return(&model.ChannelMember{}, nil)
	api.On("UpdateChannelMemberRoles", "ch1", "u_adm", channelAdminRoles).Return(nil, testAppErr("denied"))

	req := &channelRequest{RequesterID: "u_req", TeamID: "team1", Name: "team-x", AdminMemberIDs: []string{"u_adm"}}
	ch, err := p.createChannelForRequest(req)

	require.NoError(t, err)
	require.Equal(t, "ch1", ch.Id)
	// The welcome message must not claim a promotion that failed.
	require.Empty(t, req.AdminMemberIDs)
}

func TestSlugify_TruncatesToChannelNameLimit(t *testing.T) {
	got := slugify(strings.Repeat("a", model.ChannelNameMaxLength+10))
	require.Len(t, got, model.ChannelNameMaxLength)
}

// mockQueuedChannelRequest lets a channel request from u1 in team1 be
// stored, posted to the approval channel and DMed to the requester.
func mockQueuedChannelRequest(api *plugintest.API) {
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng"}, nil)
	mockApprovalChannel(api)
	api.On("GetDirectChannel", "u1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "post1"}, nil)
}

func TestSubmitRequestPendingLimit(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	config := approvalConfig()
	config.maxPending = 1
	p.setConfiguration(config)

	lists := fakePendingLists(api)
	mockMemberRequester(api)
	// The first request was reserved within the grace period, so its entry
	// counts without its record being looked up.
	api.On("KVGet", isRequestRecordKey).Return([]byte("{}"), nil).Maybe()
	mockQueuedChannelRequest(api)

	_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})
	require.NoError(t, err)
	entries := decodeEntries(t, lists[pendingListKey(pendingKindChannel, "u1")])
	require.Len(t, entries, 1)
	require.Equal(t, "team1/team-marketing", entries[0].Key)
	require.NotZero(t, entries[0].CreatedAt)

	_, err = p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Sales", Prefix: "team-"})
	require.ErrorContains(t, err, "you have 1 pending requests waiting for review")
	api.AssertNumberOfCalls(t, "GetChannelByName", 1)
}

func TestSubmitRequestPendingLimitAppliesToQueuedSystemAdmins(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	config := approvalConfig()
	config.maxPending = 1
	p.setConfiguration(config)

	lists := fakePendingLists(api)
	lists[pendingListKey(pendingKindChannel, "admin1")] = entriesJSON(t, pendingEntry{ID: "r1", CreatedAt: model.GetMillis() - 2*pendingReserveGraceMillis, Key: "team1/team-other"})
	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Username: "admin", Roles: "system_user system_admin"}, nil)
	api.On("KVGet", isRequestRecordKey).Return([]byte("{}"), nil)

	// System Admins' requests queue too, so the limit applies to them.
	_, err := p.submitRequest(requestInput{RequesterID: "admin1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})
	require.ErrorContains(t, err, "you have 1 pending requests waiting for review")
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestSubmitRequestDuplicate(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	fakePendingLists(api)
	mockMemberRequester(api)
	// The first request was reserved within the grace period, so its entry
	// counts without its record being looked up.
	api.On("KVGet", isRequestRecordKey).Return([]byte("{}"), nil).Maybe()
	mockQueuedChannelRequest(api)

	in := requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"}
	_, err := p.submitRequest(in)
	require.NoError(t, err)

	_, err = p.submitRequest(in)
	require.EqualError(t, err, "you already have a pending request for ~team-marketing in this team")
}

func TestSubmitRequestFailureFreesPendingSlot(t *testing.T) {
	for name, setup := range map[string]func(api *plugintest.API){
		"store fails": func(api *plugintest.API) {
			api.On("KVSet", isRequestRecordKey, mock.Anything).Return(testAppErr("kv down"))
		},
		"approval post fails": func(api *plugintest.API) {
			api.On("KVSet", isRequestRecordKey, mock.Anything).Return(nil)
			api.On("KVDelete", isRequestRecordKey).Return(nil)
			api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "eng"}, nil)
			mockApprovalChannel(api)
			api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			defer api.AssertExpectations(t)
			p := newTestPlugin(api)
			p.setConfiguration(approvalConfig())

			lists := fakePendingLists(api)
			mockMemberRequester(api)
			setup(api)

			_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})
			require.Error(t, err)
			require.Empty(t, lists, "the pending slot is given back")
		})
	}
}

func TestSubmitRequestPendingListUnavailable(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	p.setConfiguration(approvalConfig())

	mockMemberRequester(api)
	api.On("KVGet", isPendingListKey).Return(nil, testAppErr("kv down"))

	_, err := p.submitRequest(requestInput{RequesterID: "u1", TeamID: "team1", DisplayName: "Marketing", Prefix: "team-"})
	require.EqualError(t, err, "could not save your request; try again")
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestPostApprovalToSystemAdmins_SkipsNonHumanAccounts(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)

	remote := "remote1"
	api.On("GetUsers", isSystemAdminQuery).Return([]*model.User{
		{Id: "bot-user-id"},
		{Id: "otherbot", IsBot: true},
		{Id: "gone", DeleteAt: 1},
		{Id: "shared", RemoteId: &remote},
		nil,
		{Id: "sys1"},
	}, nil)
	api.On("GetDirectChannel", "sys1", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{}, nil)

	_, err := p.postApprovalToSystemAdmins(&model.MessageAttachment{})
	require.NoError(t, err)
	api.AssertNumberOfCalls(t, "GetDirectChannel", 1)
}
