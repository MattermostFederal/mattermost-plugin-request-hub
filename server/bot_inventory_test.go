package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// inventoryPlugin is auditExportPlugin with three indexed bots: b1 (prod,
// active, from u_req), b2 (dev, deactivated, from u_other, no compliance)
// and b3 (prod, deleted, from u_req).
func inventoryPlugin(t *testing.T) (*Plugin, *plugintest.API, map[string][]byte) {
	t.Helper()
	p, api, store := auditExportPlugin(t)
	compliance := &botCompliance{AcceptedAt: 5, TextSHA256: "abc123", Text: "I accept"}
	for _, e := range []botIndexEntry{
		{
			BotUserID: "b1", RequestID: "r1", Username: "alpha", RequesterID: "u_req", CreatedAt: 100, RequestedAt: 50, ApprovedBy: "admin1",
			Purpose: "=posts alerts", PrivilegeLevel: privilegePost, DataIngestion: "webhooks", HostingEnv: "mattermost-prod",
			HomeTeamID: "t1", HomeChannelID: "c1", Compliance: compliance,
		},
		{
			BotUserID: "b2", RequestID: "r2", Username: "beta", RequesterID: "u_other", CreatedAt: 200, HostingEnv: "mattermost-dev",
			HomeTeamID: "t1", HomeChannelID: "gone",
		},
		{BotUserID: "b3", RequestID: "r3", Username: "gamma", RequesterID: "u_req", OwnerUserID: "u_owner", CreatedAt: 300, HostingEnv: "mattermost-prod", Compliance: compliance},
	} {
		store[kvBotIndexPrefix+e.BotUserID] = recordJSON(t, e)
	}
	api.On("GetBot", "b1", true).Return(&model.Bot{UserId: "b1", Username: "alpha-renamed", DisplayName: "Alpha", OwnerId: "u_req"}, nil).Maybe()
	api.On("GetBot", "b2", true).Return(&model.Bot{UserId: "b2", Username: "beta", DeleteAt: 9, OwnerId: "u_other"}, nil).Maybe()
	api.On("GetBot", "b3", true).Return(nil, model.NewAppError("GetBot", "not_found", nil, "", http.StatusNotFound)).Maybe()
	api.On("GetTeam", "t1").Return(&model.Team{Id: "t1", DisplayName: "Ops"}, nil).Maybe()
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", DisplayName: "Alerts"}, nil).Maybe()
	api.On("GetChannel", "gone").Return(nil, testAppErr("not found")).Maybe()
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Username: "other"}, nil).Maybe()
	api.On("GetUser", "u_owner").Return(nil, testAppErr("gone")).Maybe()
	return p, api, store
}

func botIDs(rows []inventoryRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.BotID
	}
	return out
}

func TestBuildBotInventory_LiveOwnerOverridesIndex(t *testing.T) {
	p, api, store := inventoryPlugin(t)
	store[kvBotIndexPrefix+"b1"] = recordJSON(t, botIndexEntry{
		BotUserID: "b1", RequestID: "r1", Username: "alpha", RequesterID: "u_req", OwnerUserID: "u_req", CreatedAt: 100, HostingEnv: "mattermost-prod",
	})
	api.ExpectedCalls = nil
	api.On("KVList", 0, botIndexPageSize).Return([]string{kvBotIndexPrefix + "b1"}, nil).Maybe()
	api.On("KVList", 1, botIndexPageSize).Return(nil, nil).Maybe()
	api.On("KVGet", kvBotIndexPrefix+"b1").Return(store[kvBotIndexPrefix+"b1"], nil).Maybe()
	api.On("GetBot", "b1", true).Return(&model.Bot{UserId: "b1", Username: "alpha", OwnerId: "u_other"}, nil)
	api.On("GetUser", "u_req").Return(&model.User{Id: "u_req", Username: "req"}, nil).Maybe()
	api.On("GetUser", "u_other").Return(&model.User{Id: "u_other", Username: "other"}, nil).Maybe()
	api.On("GetTeam", mock.Anything).Return(nil, testAppErr("skip")).Maybe()
	api.On("GetChannel", mock.Anything).Return(nil, testAppErr("skip")).Maybe()

	rows, err := p.buildBotInventory(auditExportFilter{}, 1)

	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "u_other", rows[0].OwnerID)
	require.Equal(t, "other", rows[0].OwnerUsername)
}

func TestBuildBotInventory(t *testing.T) {
	p, _, _ := inventoryPlugin(t)

	rows, err := p.buildBotInventory(auditExportFilter{}, 999)

	require.NoError(t, err)
	require.Equal(t, []string{"b1", "b2", "b3"}, botIDs(rows), "oldest first")
	require.Equal(t, inventoryRow{
		BotID: "b1", BotUsername: "alpha-renamed", BotDisplayName: "Alpha", BotStatus: botStatusActive, BotActive: true,
		HomeTeamID: "t1", HomeTeamName: "Ops", HomeChannelID: "c1", HomeChannelName: "Alerts",
		Purpose: "=posts alerts", PrivilegeLevel: privilegePost, DataIngestion: "webhooks", HostingEnv: "mattermost-prod",
		RequesterID: "u_req", RequesterUsername: "req", OwnerID: "u_req", OwnerUsername: "req",
		RequestID: "r1", RequestedAt: 50, ApprovedAt: 100,
		ApprovedByID: "admin1", ApprovedByUsername: "admin",
		ComplianceAccepted: true, ComplianceAcceptedAt: 5, ComplianceTextSHA256: "abc123",
		RHManaged: true, AsOf: 999,
	}, rows[0])
	require.Equal(t, botStatusDeactivated, rows[1].BotStatus)
	require.False(t, rows[1].BotActive)
	require.Empty(t, rows[1].HomeChannelName, "a home channel that's gone has no name")
	require.False(t, rows[1].ComplianceAccepted)
	require.Equal(t, "u_other", rows[1].OwnerID, "index with no owner falls back to requester")
	require.Equal(t, "other", rows[1].OwnerUsername)
	require.Equal(t, botStatusDeleted, rows[2].BotStatus)
	require.Equal(t, "gamma", rows[2].BotUsername, "a deleted bot keeps its indexed username")
	require.Equal(t, "u_owner", rows[2].OwnerID)
	require.Empty(t, rows[2].OwnerUsername)

	t.Run("filters", func(t *testing.T) {
		for name, tc := range map[string]struct {
			f    auditExportFilter
			want []string
		}{
			"environment": {auditExportFilter{HostingEnv: "mattermost-prod"}, []string{"b1", "b3"}},
			"requester":   {auditExportFilter{Requester: "u_other"}, []string{"b2"}},
			"since":       {auditExportFilter{Since: 150}, []string{"b2", "b3"}},
			"until":       {auditExportFilter{Until: 250}, []string{"b1", "b2"}},
			"no match":    {auditExportFilter{HostingEnv: "lab"}, []string{}},
		} {
			t.Run(name, func(t *testing.T) {
				rows, err := p.buildBotInventory(tc.f, 1)
				require.NoError(t, err)
				require.Equal(t, tc.want, botIDs(rows))
			})
		}
	})
}

func TestBuildBotInventory_UnknownBotState(t *testing.T) {
	api := &plugintest.API{}
	stubLogLines(api)
	p := newTestPlugin(api)
	store := fakeKV(api)
	store[kvBotIndexPrefix+"b1"] = recordJSON(t, botIndexEntry{BotUserID: "b1", Username: "alpha"})
	api.On("GetBot", "b1", true).Return(nil, testAppErr("db down"))

	rows, err := p.buildBotInventory(auditExportFilter{}, 1)

	require.NoError(t, err)
	require.Equal(t, botStatusUnknown, rows[0].BotStatus)
	require.False(t, rows[0].BotActive)
}

func TestBuildBotInventory_IndexUnreadable(t *testing.T) {
	api := &plugintest.API{}
	p := newTestPlugin(api)
	api.On("KVList", 0, botIndexPageSize).Return(nil, testAppErr("kv down"))

	_, err := p.buildAuditExport(auditExportFilter{Type: exportBotInventory, Format: "pdf"})

	require.Error(t, err)
}

func TestBuildInventoryExport_Formats(t *testing.T) {
	p, _, _ := inventoryPlugin(t)
	build := func(format string, f auditExportFilter) *auditExport {
		f.Type, f.Format = exportBotInventory, format
		export, err := p.buildAuditExport(f)
		require.NoError(t, err)
		require.Equal(t, 3, export.count)
		require.True(t, strings.HasPrefix(export.filename, "request-hub-bot-inventory-"))
		require.True(t, strings.HasSuffix(export.filename, "."+format))
		return export
	}

	t.Run("json", func(t *testing.T) {
		export := build("json", auditExportFilter{})
		require.Equal(t, "application/json", export.contentType)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(export.data, &rows))
		require.Len(t, rows, 3)
		require.Equal(t, "b1", rows[0]["bot_id"])
		require.Equal(t, true, rows[0]["rh_managed"])
		require.Equal(t, "abc123", rows[0]["compliance_text_sha256"])
	})
	t.Run("csv", func(t *testing.T) {
		export := build("csv", auditExportFilter{})
		require.Contains(t, export.contentType, "text/csv")
		rows, err := csv.NewReader(bytes.NewReader(export.data)).ReadAll()
		require.NoError(t, err)
		require.Len(t, rows, 4)
		col := map[string]int{}
		for i, name := range rows[0] {
			col[name] = i
		}
		require.Equal(t, "b1", rows[1][col["bot_id"]])
		require.Equal(t, "'=posts alerts", rows[1][col["purpose"]], "formulas are neutralised")
		require.Equal(t, "Ops", rows[1][col["home_team_name"]])
		require.Equal(t, "u_req", rows[1][col["owner_id"]])
		require.Equal(t, "req", rows[1][col["owner_username"]])
		require.Equal(t, "1970-01-01T00:00:00Z", rows[1][col["approved_at_utc"]])
		require.Equal(t, "true", rows[1][col["compliance_accepted"]])
		require.Equal(t, "deactivated", rows[2][col["bot_status"]])
		require.Equal(t, "u_other", rows[2][col["owner_id"]])
		require.Equal(t, "other", rows[2][col["owner_username"]])
		require.Equal(t, "", rows[2][col["compliance_accepted_at_utc"]])
		require.Equal(t, "true", rows[3][col["rh_managed"]])
		require.Equal(t, "u_owner", rows[3][col["owner_id"]])
		require.Equal(t, "", rows[3][col["owner_username"]], "gone owner keeps the id")
	})
	t.Run("pdf", func(t *testing.T) {
		export := build("pdf", auditExportFilter{})
		require.Equal(t, "application/pdf", export.contentType)
		require.Equal(t, 1, checkPDF(t, export.data))
		s := string(export.data)
		require.Contains(t, s, "(Request Hub bot inventory) Tj")
		require.Contains(t, s, "Every bot Request Hub created. 3 bots.")
		require.Contains(t, s, "(@alpha-renamed \\(Alpha\\)) Tj")
		require.Contains(t, s, "(Read and post) Tj")
		require.Contains(t, s, "(Ops \\(t1\\) / Alerts \\(c1\\)) Tj")
		require.Contains(t, s, "/ gone \\(not found\\)) Tj", "a missing home channel says so")
		require.Contains(t, s, "statement sha256 abc123")
		require.Contains(t, s, "(not recorded) Tj", "b2 has no compliance record")
		require.Contains(t, s, "(u_owner) Tj", "b3's owner is listed by ID when the user is gone")
	})
	t.Run("pdf with nothing to list", func(t *testing.T) {
		export, err := p.buildAuditExport(auditExportFilter{Type: exportBotInventory, Format: "pdf", HostingEnv: "lab"})
		require.NoError(t, err)
		checkPDF(t, export.data)
		require.Contains(t, string(export.data), "(No bots match.) Tj")
		require.Contains(t, string(export.data), "Only bots in `lab`. 0 bots.")
	})
}

func TestCheckExportScope(t *testing.T) {
	inv := func(f auditExportFilter) auditExportFilter {
		f.Type, f.Format = exportBotInventory, "pdf"
		return f
	}
	require.NoError(t, checkExportFilter(inv(auditExportFilter{HostingEnv: "prod", Since: 1, Until: 2})))
	require.ErrorContains(t, checkExportFilter(inv(auditExportFilter{Kind: pendingKindBot})), "can't be narrowed by request or request type")
	require.ErrorContains(t, checkExportFilter(inv(auditExportFilter{RequestID: model.NewId()})), "can't be narrowed by request or request type")
	require.ErrorContains(t, checkExportFilter(inv(auditExportFilter{HostingEnv: strings.Repeat("e", maxHostingEnvLen+1)})), "environment")
	require.ErrorContains(t, checkExportFilter(auditExportFilter{Format: "csv", HostingEnv: "prod"}), "only the bot inventory can be narrowed by environment")
	require.ErrorContains(t, checkExportFilter(auditExportFilter{Type: exportBotInventory, Format: "xml"}), "pick PDF, CSV or JSON for bot inventory")
}

func TestDescribeExportFilter_Inventory(t *testing.T) {
	p, _, _ := inventoryPlugin(t)
	require.Equal(t, "Every bot Request Hub created", p.describeExportFilter(auditExportFilter{Type: exportBotInventory}))
	require.Equal(t, "Only bots in `prod`, requested by @req, approved since January 1, 1970",
		p.describeExportFilter(auditExportFilter{Type: exportBotInventory, HostingEnv: "prod", Requester: "u_req", Since: 1}))
	require.Equal(t, "Only active since January 1, 1970", p.describeExportFilter(auditExportFilter{Since: 1}))
}

func TestPendingKey_IncludesEnvironment(t *testing.T) {
	a := &auditExportRequest{Filter: auditExportFilter{Type: exportBotInventory, Format: "pdf", HostingEnv: "dev"}}
	b := &auditExportRequest{Filter: auditExportFilter{Type: exportBotInventory, Format: "pdf", HostingEnv: "prod"}}
	require.NotEqual(t, a.pendingKey(), b.pendingKey())
}

func TestDecideAuditExport_ApproveSendsTheInventory(t *testing.T) {
	p, api, store := inventoryPlugin(t)
	events := captureAudit(p)
	req := storedExportRequest(t, store, auditExportFilter{Type: exportBotInventory, Format: "pdf", HostingEnv: "mattermost-prod"})
	api.On("GetPost", "card1").Return(approvalCard("card1"), nil).Maybe()
	api.On("GetPost", "dmroot").Return(ticketRoot(), nil).Maybe()
	updatedPosts(api)
	var uploaded []byte
	api.On("UploadFile", mock.Anything, "dm1", mock.MatchedBy(func(name string) bool {
		return strings.HasPrefix(name, "request-hub-bot-inventory-") && strings.HasSuffix(name, ".pdf")
	})).Run(func(args mock.Arguments) { uploaded = args.Get(0).([]byte) }).Return(&model.FileInfo{Id: "file1"}, nil)
	var delivered *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		if post := args.Get(0).(*model.Post); len(post.FileIds) > 0 {
			delivered = post
		}
	}).Return(&model.Post{Id: "p"}, nil)

	resp := runCommandAs(p, "admin1", "/mattermost-request-hub approve "+req.ID)

	require.Contains(t, resp.Text, "✅ Approved by @admin for @sec. Audit export (PDF, 2 bots) sent.")
	require.NotNil(t, delivered)
	require.Contains(t, delivered.Message, "**Audit export: Bot inventory** (PDF, 2 bots). Only bots in `mattermost-prod`.")
	checkPDF(t, uploaded)
	require.Contains(t, string(uploaded), "(@alpha-renamed \\(Alpha\\)) Tj")
	require.NotContains(t, string(uploaded), "@beta", "a dev bot isn't in a prod inventory")

	var approved *auditEntry
	for i := range *events {
		if (*events)[i].Event == eventApproved {
			approved = &(*events)[i]
		}
	}
	require.NotNil(t, approved)
	require.Equal(t, 2, approved.Detail["bots"])
	require.Equal(t, exportBotInventory, approved.Detail["type"])
	require.Equal(t, "mattermost-prod", approved.Detail["env"])
	require.Contains(t, approved.Note, "the Bot inventory PDF file (2 bots) was sent to them")
}

func TestHandleAuditExport_Inventory(t *testing.T) {
	p, _, _ := inventoryPlugin(t)
	events := captureAudit(p)
	withDirectExport(p)

	w := exportRequest(t, p, "admin1", "?type=bot_inventory&env=mattermost-dev")

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"), "the API gives JSON when no format is named")
	var rows []inventoryRow
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	require.Equal(t, []string{"b2"}, botIDs(rows))
	require.Equal(t, 1, countEvents(*events, eventAuditExported))
	require.Equal(t, 1, (*events)[len(*events)-1].Detail["bots"])

	w = exportRequest(t, p, "admin1", "?type=bot_inventory&format=pdf")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/pdf", w.Header().Get("Content-Type"))
	checkPDF(t, w.Body.Bytes())

	require.Equal(t, http.StatusBadRequest, exportRequest(t, p, "admin1", "?type=bot_inventory&kind=bot").Code)
	require.Equal(t, http.StatusBadRequest, exportRequest(t, p, "admin1", "?env=prod").Code)
}

func TestAuditExportSlash_Inventory(t *testing.T) {
	p, api, store := inventoryPlugin(t)
	c := *p.getConfiguration()
	c.ApprovalTeam = "appteam"
	p.setConfiguration(&c)
	mockAuditCards(api)
	api.On("GetUsers", mock.Anything).Return([]*model.User{sysadmin("admin1")}, nil)
	api.On("GetDirectChannel", "u_sec", "bot-user-id").Return(&model.Channel{Id: "dm1"}, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: model.NewId()}, nil)

	resp := runCommandAs(p, "u_sec", "/mattermost-request-hub audit export bot-inventory pdf --env mattermost-prod --reason quarterly")

	require.Contains(t, resp.Text, "has been sent to the System Admins")
	var stored *auditExportRequest
	for key, value := range store {
		if strings.HasPrefix(key, kvAuditExportRequestPrefix) {
			stored = &auditExportRequest{}
			require.NoError(t, json.Unmarshal(value, stored))
		}
	}
	require.NotNil(t, stored)
	require.Equal(t, auditExportFilter{Type: exportBotInventory, Format: "pdf", HostingEnv: "mattermost-prod"}, stored.Filter)

	withDirectExport(p)
	resp = runCommandAs(p, "admin1", "/mattermost-request-hub audit export bot_inventory --env mattermost-dev")
	require.Contains(t, resp.Text, "**Download the audit export: Bot inventory**")
	require.Contains(t, resp.Text, "type=bot_inventory&env=mattermost-dev&format=pdf")
	require.Contains(t, resp.Text, "format=csv")
	require.Contains(t, resp.Text, "Add `env`, `requester`")

	resp = runCommandAs(p, "admin1", "/mattermost-request-hub audit export bot_inventory --kind bot")
	require.Contains(t, resp.Text, "can't be narrowed by request or request type")
}
