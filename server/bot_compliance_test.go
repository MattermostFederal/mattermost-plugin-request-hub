package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestBotComplianceText_BlankUsesTheDefault(t *testing.T) {
	require.Equal(t, defaultBotComplianceText, (&configuration{BotComplianceStatement: "  \n "}).BotComplianceText())
	require.Equal(t, "Be good.", (&configuration{BotComplianceStatement: " Be good. "}).BotComplianceText())
}

func TestCheckBotCompliance(t *testing.T) {
	on := &configuration{BotComplianceEnabled: true, BotComplianceStatement: "Be good."}
	hash := complianceHash("Be good.")

	t.Run("not required", func(t *testing.T) {
		bc, err := checkBotCompliance(&configuration{}, false, "")
		require.NoError(t, err)
		require.Nil(t, bc)
	})
	t.Run("not accepted", func(t *testing.T) {
		_, err := checkBotCompliance(on, false, hash)
		require.EqualError(t, err, botComplianceRequiredMsg)
	})
	t.Run("statement changed since it was shown", func(t *testing.T) {
		_, err := checkBotCompliance(on, true, complianceHash("Be nice."))
		require.EqualError(t, err, botComplianceChangedMsg)
	})
	for name, shown := range map[string]string{"shown hash matches": hash, "no hash (slash command)": ""} {
		t.Run(name, func(t *testing.T) {
			bc, err := checkBotCompliance(on, true, shown)
			require.NoError(t, err)
			require.Equal(t, "Be good.", bc.Text)
			require.Equal(t, hash, bc.TextSHA256)
			require.NotZero(t, bc.AcceptedAt)
		})
	}
}

// stubBotSubmit lets u_req submit a bot request into the bot approval
// channel, and returns the stored request as it was last saved.
func stubBotSubmit(t *testing.T, api *plugintest.API) *botRequest {
	t.Helper()
	stubNamesFree(api)
	stubPendingLists(api)
	stubLogs(api)
	stored := &botRequest{}
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "reqer", Roles: "system_user"}, nil)
	api.On("KVSet", mock.MatchedBy(func(k string) bool {
		return len(k) > len(kvBotRequestPrefix) && k[:len(kvBotRequestPrefix)] == kvBotRequestPrefix
	}), mock.Anything).
		Run(func(args mock.Arguments) { require.NoError(t, json.Unmarshal(args.Get(1).([]byte), stored)) }).Return(nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	api.On("KVCompareAndSet", mock.Anything, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	api.On("GetTeamByName", "appteam").Return(&model.Team{Id: "appteam-id", DisplayName: "App Team"}, nil)
	stubApprovalTeamRecord(api)
	api.On("GetChannelByName", "appteam-id", "mattermost-bot-requests", true).
		Return(&model.Channel{Type: model.ChannelTypePrivate, CreatorId: "bot-user-id", Id: "bots-ch"}, nil)
	api.On("GetDirectChannel", "u_req", "bot-user-id").Return(&model.Channel{Id: "dm-ch"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "post-1"}, nil)
	stubBotHome(api)
	return stored
}

func TestSubmitBotRequest_StoresAndAuditsTheAcceptance(t *testing.T) {
	api := &plugintest.API{}
	stored := stubBotSubmit(t, api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowBotTokenRequests: true, BotComplianceEnabled: true})
	events := captureAudit(p)

	_, err := p.submitBotRequest("u_req", declared(botRequestInput{
		Username: "svcbot", DisplayName: "Service Bot",
		ComplianceAccepted: true, ComplianceTextHash: complianceHash(defaultBotComplianceText),
	}))
	require.NoError(t, err)

	require.NotNil(t, stored.Compliance)
	require.Equal(t, defaultBotComplianceText, stored.Compliance.Text)
	hash := complianceHash(defaultBotComplianceText)
	require.Equal(t, hash, stored.Compliance.TextSHA256)

	require.Equal(t, 1, countEvents(*events, eventSubmitted))
	for _, e := range *events {
		if e.Event != eventSubmitted {
			continue
		}
		require.Equal(t, true, e.Detail["compliance_accepted"])
		require.Equal(t, hash, e.Detail["compliance_text_sha256"])
		require.Equal(t, stored.Compliance.AcceptedAt, e.Detail["compliance_accepted_at"])
		require.Contains(t, e.Note, "accepted the bot compliance statement (sha256 "+hash[:8]+")")
		require.NotContains(t, e.Note, defaultBotComplianceText)
	}
}

func TestSubmitBotRequest_RefusesWithoutAcceptance(t *testing.T) {
	api := &plugintest.API{}
	stubNamesFree(api)
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{ApprovalTeam: "appteam", AllowBotTokenRequests: true, BotComplianceEnabled: true})
	events := captureAudit(p)

	_, err := p.submitBotRequest("u_req", declared(botRequestInput{Username: "svcbot", DisplayName: "Service Bot"}))

	require.EqualError(t, err, botComplianceRequiredMsg)
	require.Equal(t, 1, countEvents(*events, eventSubmitRefused))
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)
}

func TestBotApprovalAttachment_ShowsTheAcceptance(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})
	req := &botRequest{ID: "r1", Username: "svcbot", DisplayName: "Service Bot", Compliance: &botCompliance{TextSHA256: "abcdef0123456789"}}

	att := p.botApprovalAttachment(req, &model.User{Username: "reqer"})

	require.Contains(t, att.Text, "**Compliance:** accepted (sha256 abcdef01)")
}

func TestBotRequestDialog_AddsTheStatementWhenRequired(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})

	p.setConfiguration(&configuration{})
	off := p.botRequestDialog("~town-square")
	require.Empty(t, off.State)
	for _, el := range off.Elements {
		require.NotEqual(t, fieldBotCompliance, el.Name)
	}

	p.setConfiguration(&configuration{BotComplianceEnabled: true, BotComplianceStatement: "Line one.\nLine two."})
	on := p.botRequestDialog("~town-square")
	require.Equal(t, complianceHash("Line one.\nLine two."), on.State)
	require.Contains(t, on.IntroductionText, "> Line one.\n> Line two.")
	last := on.Elements[len(on.Elements)-1]
	require.Equal(t, fieldBotCompliance, last.Name)
	require.Equal(t, "bool", last.Type)
	require.False(t, last.Optional)
}

func TestSubmissionBool(t *testing.T) {
	sub := map[string]any{"a": true, "b": "true", "c": false, "d": "yes"}
	require.True(t, submissionBool(sub, "a"))
	require.True(t, submissionBool(sub, "b"))
	require.False(t, submissionBool(sub, "c"))
	require.False(t, submissionBool(sub, "d"))
	require.False(t, submissionBool(sub, "missing"))
}

func TestBotSlashCommand_ShowsTheStatementWithoutTheFlag(t *testing.T) {
	api := &plugintest.API{}
	stubLogs(api)
	p := newTestPlugin(api)
	p.setConfiguration(&configuration{AllowBotTokenRequests: true, BotComplianceEnabled: true, BotComplianceStatement: "Be good."})

	resp, appErr := p.ExecuteCommand(nil, commandArgs("bot svcbot Service Bot"))

	require.Nil(t, appErr)
	require.Equal(t, "Bot requests need you to accept this statement:\n\n> Be good.\n\nTo accept it, run the command again with `--accept-compliance`.", resp.Text)
	api.AssertNotCalled(t, "KVSet", mock.Anything, mock.Anything)

	resp, appErr = p.ExecuteCommand(nil, commandArgs("bot svcbot Service Bot --accept-compliance yes"))
	require.Nil(t, appErr)
	require.Contains(t, resp.Text, "`--accept-compliance` takes no value")
}

func TestHandleBotForm(t *testing.T) {
	p := newTestPlugin(&plugintest.API{})

	p.setConfiguration(&configuration{})
	w := serve(p, http.MethodGet, routeBotForm, "u1", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"compliance":{"enabled":false},"hosting_envs":["mattermost-dev","mattermost-prod"],`+
		`"privilege_levels":[{"value":"read","label":"Read only"},{"value":"post","label":"Read and post"},{"value":"elevated","label":"Elevated (admin-level APIs)"}]}`, w.Body.String())

	p.setConfiguration(&configuration{BotComplianceEnabled: true, BotComplianceStatement: "Be good.", BotHostingEnvironments: "lab\n\n prod \nlab"})
	w = serve(p, http.MethodGet, routeBotForm, "u1", "")
	var form botFormDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &form))
	require.Equal(t, botComplianceDTO{Enabled: true, Text: "Be good.", SHA256: complianceHash("Be good.")}, form.Compliance)
	require.Equal(t, []string{"lab", "prod"}, form.HostingEnvs)

	w = serve(p, http.MethodGet, routeBotForm, "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
