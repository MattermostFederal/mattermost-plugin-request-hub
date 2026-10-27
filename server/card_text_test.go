package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// markupText mixes a mention, a link, an image and inline code, which must
// all show as typed on cards.
const markupText = "@channel [x](https://e.example) ![i](https://e.example/i.png) `tick`"

// requireShownAsTyped fails when s contains markup from markupText that would
// still render: a link, a mention, or an unescaped backtick.
func requireShownAsTyped(t *testing.T, s string) {
	t.Helper()
	require.NotContains(t, s, "](")
	require.NotContains(t, s, "@channel")
	require.NotContains(t, s, "https://")
	for i, r := range s {
		if r == '`' {
			require.True(t, i > 0 && s[i-1] == '\\', "unescaped backtick in %q", s)
		}
	}
}

// cardText joins everything a reviewer reads on an attachment.
func cardText(att *model.MessageAttachment) string {
	parts := []string{att.Title, att.Text, att.Pretext}
	for _, f := range att.Fields {
		parts = append(parts, f.Title, f.Value.(string))
	}
	return strings.Join(parts, "\n")
}

func markupTeam() *model.Team {
	return &model.Team{Id: "team1", Name: "ops", DisplayName: markupText}
}

func cardRequester() *model.User {
	return &model.User{Id: "u_req", Username: "reqer"}
}

func TestTeamAdminCard_ShowsTeamNameAsTyped(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	att := p.teamAdminApprovalAttachment(&teamAdminRequest{ID: "r1"}, cardRequester(), markupTeam())

	requireShownAsTyped(t, cardText(att))
	require.Contains(t, cardText(att), "@\u200bchannel")
}

func TestTeamMemberCard_ShowsTeamAndReasonAsTyped(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	att := p.teamMemberApprovalAttachment(&teamMemberRequest{ID: "r1", Reason: markupText}, cardRequester(), markupTeam())

	requireShownAsTyped(t, cardText(att))
}

func TestTeamGuestCard_ShowsTextAsTyped(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", Name: "guests", Type: model.ChannelTypePrivate}, nil)
	api.On("GetChannel", "c2").Return(nil, testAppErr("gone"))

	att := p.teamGuestApprovalAttachment(&teamGuestRequest{
		ID: "r1", Email: "a_b*c@example.com", ChannelIDs: []string{"c1", "c2"}, Reason: markupText,
	}, cardRequester(), markupTeam())

	text := cardText(att)
	requireShownAsTyped(t, text)
	require.Contains(t, text, `a\_b\*c@`+"\u200b"+`example\.com`)
	require.Contains(t, text, "~guests (private), c2")
}

func TestTeamCreationCard_ShowsTextAsTyped(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetUser", "u_req").Return(cardRequester(), nil)

	att := p.teamCreationApprovalAttachment(&teamCreationRequest{
		ID: "r1", RequesterID: "u_req", DisplayName: markupText, Name: "ops", Description: markupText, RequestTeamAdmin: true,
	}, cardRequester())

	requireShownAsTyped(t, cardText(att))
}

func TestBotCard_ShowsTextAsTyped(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetChannel", "ch-in").Return(&model.Channel{Id: "ch-in", Name: "alerts", TeamId: "team1"}, nil)
	api.On("GetChannel", "ch-out").Return(&model.Channel{Id: "ch-out", Name: "relay", TeamId: "team1"}, nil)
	api.On("GetTeam", "team1").Return(markupTeam(), nil)

	att := p.botApprovalAttachment(&botRequest{
		ID: "r1", Username: "svcbot", DisplayName: markupText, Description: markupText,
		IncomingWebhookChannelID: "ch-in", IncomingWebhookDisplayName: markupText,
		OutgoingWebhookChannelID: "ch-out", OutgoingWebhookDisplayName: markupText,
		OutgoingWebhookCallbackURL: "https://e.example/[x](y)",
	}, cardRequester())

	requireShownAsTyped(t, cardText(att))
}

func TestWebhookCard_ShowsTextAsTyped(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	att := p.webhookApprovalAttachment(&webhookRequest{
		ID: "r1", WebhookType: webhookTypeOutgoing, DisplayName: markupText, Description: markupText,
		CallbackURL: "https://e.example/[x](y)",
	}, cardRequester(), &model.Channel{Id: "ch1", Name: "alerts"})

	text := cardText(att)
	requireShownAsTyped(t, text)
	require.Contains(t, text, "Outgoing webhook")
}

func TestChannelNamesList(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", Name: "town", Type: model.ChannelTypeOpen}, nil)
	api.On("GetChannel", "c2").Return(&model.Channel{Id: "c2", Name: "staff", Type: model.ChannelTypePrivate}, nil)
	api.On("GetChannel", "c3").Return(nil, testAppErr("gone"))

	require.Empty(t, p.channelNamesList(nil))
	require.Equal(t, "~town, ~staff (private), c3", p.channelNamesList([]string{"c1", "c2", "c3"}))
}

func TestChannelDisplayRef(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", Name: "town", TeamId: "team1"}, nil)
	api.On("GetChannel", "c2").Return(&model.Channel{Id: "c2", Name: "staff", TeamId: "team2"}, nil)
	api.On("GetChannel", "c3").Return(nil, testAppErr("gone"))
	api.On("GetTeam", "team1").Return(markupTeam(), nil)
	api.On("GetTeam", "team2").Return(nil, testAppErr("gone"))

	withTeam := p.channelDisplayRef("c1")
	require.True(t, strings.HasPrefix(withTeam, "#town (Team: "))
	requireShownAsTyped(t, withTeam)
	require.Equal(t, "#staff", p.channelDisplayRef("c2"))
	require.Equal(t, "c3", p.channelDisplayRef("c3"))
}

func TestQuotedText(t *testing.T) {
	quoted := quotedText(markupText + "\nsecond line")

	requireShownAsTyped(t, quoted)
	require.True(t, strings.HasPrefix(quoted, "> "))
	require.Contains(t, quoted, "\n> second line")
}

func TestForwardDMReplyToTicketThread_QuotesReplyAsTyped(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUser", "u_req").Return(cardRequester(), nil)
	var forwarded *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		forwarded = args.Get(0).(*model.Post)
	}).Return(&model.Post{}, nil)

	p.forwardDMReplyToTicketThread(
		&model.Post{UserId: "u_req", Message: markupText},
		[]byte(`{"ap":"card1","ac":"appr"}`),
	)

	require.NotNil(t, forwarded)
	require.Equal(t, "card1", forwarded.RootId)
	require.Equal(t, "appr", forwarded.ChannelId)
	require.True(t, strings.HasPrefix(forwarded.Message, "**Reply from @reqer:**\n> "))
	requireShownAsTyped(t, strings.TrimPrefix(forwarded.Message, "**Reply from @reqer:**"))
}

func TestForwardDMReplyToTicketThread_NoCardNoForward(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	p.forwardDMReplyToTicketThread(&model.Post{UserId: "u_req", Message: "hi"}, []byte(`{"dp":"root"}`))
	p.forwardDMReplyToTicketThread(&model.Post{UserId: "u_req", Message: "hi"}, []byte("not json"))

	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestForwardDMReplyToTicketThread_UnknownPosterNoForward(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetUser", "u_req").Return(nil, testAppErr("gone"))

	p.forwardDMReplyToTicketThread(&model.Post{UserId: "u_req", Message: "hi"}, []byte(`{"ap":"card1"}`))

	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestCreateUserSession_ExpiresSoon(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1", Roles: "system_user system_admin"}, nil)
	var created *model.Session
	api.On("CreateSession", mock.Anything).Run(func(args mock.Arguments) {
		created = args.Get(0).(*model.Session)
	}).Return(&model.Session{Id: "sess1"}, nil)

	before := model.GetMillis()
	session, err := p.createUserSession("admin1")

	require.NoError(t, err)
	require.Equal(t, "sess1", session.Id)
	require.NotNil(t, created)
	require.Equal(t, "admin1", created.UserId)
	require.Equal(t, "system_user system_admin", created.Roles)
	require.Greater(t, created.ExpiresAt, int64(0))
	require.GreaterOrEqual(t, created.ExpiresAt, before+(5*time.Minute).Milliseconds())
	require.LessOrEqual(t, created.ExpiresAt, model.GetMillis()+(5*time.Minute).Milliseconds())
}

func TestCreateUserSession_RequiresUserID(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	_, err := p.createUserSession("")

	require.Error(t, err)
	api.AssertNotCalled(t, "GetUser", mock.Anything)
}

func TestCreateUserSession_UserLoadFailure(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetUser", "admin1").Return(nil, testAppErr("gone"))

	_, err := p.createUserSession("admin1")

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to load user for session")
	api.AssertNotCalled(t, "CreateSession", mock.Anything)
}

func TestCreateUserSession_CreateFailure(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("GetUser", "admin1").Return(&model.User{Id: "admin1"}, nil)
	api.On("CreateSession", mock.Anything).Return(nil, testAppErr("no session"))

	_, err := p.createUserSession("admin1")

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create session")
}

func TestUsersSharingATeam_KeepsActiveTeammates(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	// t3 is the team being added to, so it doesn't count as shared.
	api.On("GetTeamsForUser", "caller").Return([]*model.Team{{Id: "t1"}, nil, {Id: "t2"}, {Id: "t3"}}, nil).Once()
	// mate is only on t2; left was on t1 but left; stranger is on another
	// team; pending is only on t3; unknown's teams can't be listed.
	api.On("GetTeamMembersForUser", "mate", 0, 200).Return([]*model.TeamMember{{TeamId: "t9"}, nil, {TeamId: "t2"}}, nil).Once()
	api.On("GetTeamMembersForUser", "left", 0, 200).Return([]*model.TeamMember{{TeamId: "t1", DeleteAt: 1}}, nil).Once()
	api.On("GetTeamMembersForUser", "stranger", 0, 200).Return([]*model.TeamMember{{TeamId: "t9"}}, nil).Once()
	api.On("GetTeamMembersForUser", "pending", 0, 200).Return([]*model.TeamMember{{TeamId: "t3"}}, nil).Once()
	api.On("GetTeamMembersForUser", "unknown", 0, 200).Return(nil, testAppErr("db down")).Once()

	kept := p.usersSharingATeam("caller", "t3", []*model.User{{Id: "mate"}, {Id: "left"}, {Id: "stranger"}, {Id: "pending"}, {Id: "unknown"}})

	require.Len(t, kept, 1)
	require.Equal(t, "mate", kept[0].Id)
	api.AssertNotCalled(t, "GetTeamMember", mock.Anything, mock.Anything)
}

func TestUsersSharingATeam_TeamListFailureKeepsNobody(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	api.On("GetTeamsForUser", "caller").Return(nil, testAppErr("db down"))

	require.Nil(t, p.usersSharingATeam("caller", "t3", []*model.User{{Id: "mate"}}))
	api.AssertNotCalled(t, "GetTeamMembersForUser", mock.Anything, mock.Anything, mock.Anything)
}

func TestTeamApprovalChannelWelcome_ShowsTeamNameAsTyped(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	var welcome *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		welcome = args.Get(0).(*model.Post)
	}).Return(&model.Post{}, nil)

	p.postChannelWelcomeMessage(&model.Channel{Id: "ch1"}, markupTeam())

	require.NotNil(t, welcome)
	requireShownAsTyped(t, welcome.Message)
	require.Contains(t, welcome.Message, "@\u200bchannel")
}

func TestCreateWebhookForRequest_ShowsNameAsTyped(t *testing.T) {
	for _, hookType := range []string{webhookTypeIncoming, webhookTypeOutgoing} {
		t.Run(hookType, func(t *testing.T) {
			capture := &hookCapture{}
			ts := capture.server(t)
			api := &plugintest.API{}
			stubLogs(api)
			p := newTestPlugin(api)
			stubSiteURL(api, ts.URL)
			stubApproverSession(api)

			msg, _, err := p.createWebhookForRequest(&webhookRequest{
				RequesterID: "u_req", WebhookType: hookType, ChannelID: "ch1", TeamID: "team1",
				DisplayName: markupText, CallbackURL: "https://hooks.example.com/in",
			}, "admin1")

			require.NoError(t, err)
			// The incoming hook's URL follows in a code span.
			name, _, _ := strings.Cut(msg, "Your webhook URL")
			requireShownAsTyped(t, name)
			require.Contains(t, name, "@\u200bchannel")
		})
	}
}

func TestBotExtrasReply_ShowsOutgoingWebhookNameAsTyped(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	var reply *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		reply = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "reply1"}, nil)

	p.sendBotExtrasReply(&botRequest{DMChannelID: "dm1", OutgoingWebhookDisplayName: markupText}, "", "", true, nil)

	require.NotNil(t, reply)
	requireShownAsTyped(t, reply.Message)
	require.Contains(t, reply.Message, "@\u200bchannel")
}

func TestTeamRequestDialogs_ShowTeamNameAsTyped(t *testing.T) {
	cases := []struct {
		name string
		open func(p *Plugin) error
	}{
		{name: "team member", open: func(p *Plugin) error { return p.openTeamMemberRequestDialog("trigger", "team1") }},
		{name: "team admin", open: func(p *Plugin) error { return p.openTeamAdminRequestDialog("trigger", "team1") }},
		{name: "team guest", open: func(p *Plugin) error { return p.openTeamGuestRequestDialog("trigger", "team1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			stubLogs(api)
			p := newTestPlugin(api)
			stubSiteURL(api, "http://localhost:8065")
			api.On("GetTeam", "team1").Return(markupTeam(), nil)
			var dialog model.OpenDialogRequest
			api.On("OpenInteractiveDialog", mock.Anything).Run(func(args mock.Arguments) {
				dialog = args.Get(0).(model.OpenDialogRequest)
			}).Return(nil)

			require.NoError(t, tc.open(p))

			requireShownAsTyped(t, dialog.Dialog.IntroductionText)
			require.Contains(t, dialog.Dialog.IntroductionText, "@\u200bchannel")
		})
	}
}

func TestTeamApprovalChannelWelcome_PostFailureIsLogged(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))
	api.On("LogWarn", "failed to post welcome message in team approval channel", "channel_id", "ch1", "error", mock.Anything).Once()

	p.postChannelWelcomeMessage(&model.Channel{Id: "ch1"}, markupTeam())
}

func TestTeamRequestDialogs_UnknownTeamAndOpenFailure(t *testing.T) {
	for _, open := range []func(p *Plugin) error{
		func(p *Plugin) error { return p.openTeamMemberRequestDialog("trigger", "team1") },
		func(p *Plugin) error { return p.openTeamAdminRequestDialog("trigger", "team1") },
		func(p *Plugin) error { return p.openTeamGuestRequestDialog("trigger", "team1") },
	} {
		api := &plugintest.API{}
		stubLogs(api)
		p := newTestPlugin(api)
		stubSiteURL(api, "http://localhost:8065")
		api.On("GetTeam", "team1").Return(nil, testAppErr("gone"))
		var dialog model.OpenDialogRequest
		api.On("OpenInteractiveDialog", mock.Anything).Run(func(args mock.Arguments) {
			dialog = args.Get(0).(model.OpenDialogRequest)
		}).Return(testAppErr("no dialog"))

		require.Error(t, open(p))
		require.Contains(t, dialog.Dialog.IntroductionText, "**team1**")
	}
}

func TestBotExtrasReply_TokenAndURLAreScheduledForDeletion(t *testing.T) {
	api := &plugintest.API{}
	stubLogLevels(api)
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	var reply *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		reply = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "reply1"}, nil)
	api.On("KVGet", tokenPostRegistryKey).Return(nil, nil)
	api.On("KVCompareAndSet", tokenPostRegistryKey, []byte(nil), mock.Anything).Return(true, nil)

	p.sendBotExtrasReply(&botRequest{DMChannelID: "dm1"}, "tok-value", "https://chat.example.com/hooks/h1", true, []string{"one thing failed"})

	require.NotNil(t, reply)
	require.Contains(t, reply.Message, "tok-value")
	require.Contains(t, reply.Message, "hooks/h1")
	require.Contains(t, reply.Message, "*outgoing webhook* is now active")
	require.Contains(t, reply.Message, "one thing failed")
}

func TestBotExtrasReply_NothingToSayPostsNothing(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	p.sendBotExtrasReply(&botRequest{DMChannelID: "dm1"}, "", "", false, nil)

	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestBotExtrasReply_PostFailureIsLogged(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	p := newTestPlugin(api)
	api.On("CreatePost", mock.Anything).Return(nil, testAppErr("post failed"))
	api.On("LogError", "failed to send bot extras reply", "user_id", "u_req", "error", mock.Anything).Once()

	p.sendBotExtrasReply(&botRequest{RequesterID: "u_req", DMChannelID: "dm1"}, "tok-value", "", false, nil)
}

func TestRequireWebhookTargetChannel_ChannelRequired(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)

	_, err := p.requireWebhookTargetChannel(regularUser(), "  ", webhookTypeIncoming)

	require.Error(t, err)
	require.Contains(t, err.Error(), "a target channel is required")
	api.AssertNotCalled(t, "GetChannel", mock.Anything)
}
