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
			IntegrationURL: "https://ci.example/hooks/mm", HomeTeamID: "t1", HomeChannelID: "c1", Compliance: compliance,
			HasToken: true, HasIncomingWebhook: true,
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
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", Name: "alerts", DisplayName: "Alerts"}, nil).Maybe()
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
	stubLogLevels(api)
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
		HomeTeamID: "t1", HomeTeamName: "Ops", HomeChannelID: "c1", HomeChannelName: "Alerts", HomeChannelHandle: "alerts",
		Purpose: "=posts alerts", PrivilegeLevel: privilegePost, DataIngestion: "webhooks", HostingEnv: "mattermost-prod",
		IntegrationURL: "https://ci.example/hooks/mm",
		RequesterID: "u_req", RequesterUsername: "req", OwnerID: "u_req", OwnerUsername: "req",
		HasToken: true, HasIncomingWebhook: true, WebhookOwner: "@alpha-renamed (bot owned by @req)",
		RequestID: "r1", RequestedAt: 50, ApprovedAt: 100,
		ApprovedByID: "admin1", ApprovedByUsername: "admin",
		ComplianceAccepted: true, ComplianceAcceptedAt: 5, ComplianceTextSHA256: "abc123",
		RHManaged: true, AsOf: 999,
	}, rows[0])
	require.Equal(t, botStatusDeactivated, rows[1].BotStatus)
	require.False(t, rows[1].BotActive)
	require.Empty(t, rows[1].HomeChannelName, "a home channel that's gone has no name")
	require.Empty(t, rows[1].HomeChannelHandle)
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
	stubLogs(api)
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

	t.Run("json column", func(t *testing.T) {
		export := build("json", auditExportFilter{})
		require.Equal(t, "application/json", export.contentType)
		var rows []map[string]string
		require.NoError(t, json.Unmarshal(export.data, &rows))
		require.Len(t, rows, len(securityCoreColumns))
		byField := map[string]map[string]string{}
		for _, row := range rows {
			byField[row["Field"]] = row
		}
		require.Equal(t, "b1", byField["Bot ID"]["alpha-renamed"])
		require.Equal(t, "b2", byField["Bot ID"]["beta"])
		require.Equal(t, "req", byField["Requester"]["alpha-renamed"])
		require.Equal(t, "yes", byField["Approved"]["alpha-renamed"])
		require.Equal(t, "HTTPS required for declared URLs", byField["Data transit security"]["alpha-renamed"])
	})
	t.Run("json row", func(t *testing.T) {
		export := build("json", auditExportFilter{Layout: inventoryLayoutRow, Verbose: true})
		var rows []map[string]string
		require.NoError(t, json.Unmarshal(export.data, &rows))
		require.Len(t, rows, 3)
		require.Equal(t, "b1", rows[0]["Bot ID"])
		require.Equal(t, "u_req", rows[0]["Owner ID"])
		require.Equal(t, "sha256:abc123 @ 1970-01-01T00:00:00Z", rows[0]["T&Cs acceptance"])
		require.Equal(t, "u_owner", rows[2]["Owner ID"])
	})
	t.Run("csv column", func(t *testing.T) {
		export := build("csv", auditExportFilter{})
		require.Contains(t, export.contentType, "text/csv")
		rows, err := csv.NewReader(bytes.NewReader(export.data)).ReadAll()
		require.NoError(t, err)
		require.Len(t, rows, 1+len(securityCoreColumns), "header + one CSV row per Security field")
		require.Equal(t, []string{"Field", "alpha-renamed", "beta", "gamma"}, rows[0],
			"header names bots across columns")
		byField := map[string][]string{}
		for _, row := range rows[1:] {
			byField[row[0]] = row[1:]
		}
		require.Equal(t, []string{"r1", "r2", "r3"}, byField["Request ID"])
		require.Equal(t, []string{"alpha-renamed", "beta", "gamma"}, byField["Bot name"])
		require.Equal(t, "'=posts alerts", byField["Purpose"][0], "formulas are neutralised")
		require.Contains(t, byField["Authentication method"][0], "Bot access token")
		require.Equal(t, []string{"true", "true", "true"}, byField["Activity logs"])
		_, hasOwner := byField["Owner ID"]
		require.False(t, hasOwner, "owner is verbose-only")
	})
	t.Run("csv row", func(t *testing.T) {
		export := build("csv", auditExportFilter{Layout: inventoryLayoutRow})
		rows, err := csv.NewReader(bytes.NewReader(export.data)).ReadAll()
		require.NoError(t, err)
		require.Equal(t, securityCoreColumns, rows[0], "fields across the header")
		require.Len(t, rows, 4, "header + one row per bot")
		col := map[string]int{}
		for i, name := range rows[0] {
			col[name] = i
		}
		require.Equal(t, "b1", rows[1][col["Bot ID"]])
		require.Equal(t, "alpha-renamed", rows[1][col["Bot name"]])
		require.Equal(t, "b3", rows[3][col["Bot ID"]])
	})
	t.Run("csv wraps long column cells", func(t *testing.T) {
		long := strings.Repeat("purpose word ", 20)
		data, err := inventoryCSV([]inventoryRow{{
			RequestID: "r", BotUsername: "bot", BotID: "b", Purpose: long, RHManaged: true,
		}}, false, inventoryLayoutColumn)
		require.NoError(t, err)
		require.Contains(t, string(data), "\n", "long values soft-wrap inside the cell")
	})
	t.Run("pdf column", func(t *testing.T) {
		export := build("pdf", auditExportFilter{Layout: inventoryLayoutColumn})
		require.Equal(t, "application/pdf", export.contentType)
		require.GreaterOrEqual(t, checkPDF(t, export.data), 1)
		s := string(export.data)
		require.Contains(t, s, "(Request Hub bot inventory) Tj")
		require.Contains(t, s, "Column layout")
		require.Contains(t, s, "(alpha-renamed) Tj")
		require.Contains(t, s, "(Read and post) Tj")
		require.Contains(t, s, "HTTPS required for declared")
	})
	t.Run("pdf row", func(t *testing.T) {
		export := build("pdf", auditExportFilter{Layout: inventoryLayoutRow, Verbose: true})
		require.GreaterOrEqual(t, checkPDF(t, export.data), 1)
		s := string(export.data)
		require.Contains(t, s, "Row layout")
		require.Contains(t, s, "(@alpha-renamed \\(Alpha\\)) Tj")
		require.Contains(t, s, "(Owner ID: ) Tj")
		require.Contains(t, s, "(u_owner) Tj")
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
	require.NoError(t, checkExportFilter(inv(auditExportFilter{Verbose: true})))
	require.ErrorContains(t, checkExportFilter(inv(auditExportFilter{Kind: pendingKindBot})), "can't be narrowed by request or request type")
	require.ErrorContains(t, checkExportFilter(inv(auditExportFilter{RequestID: model.NewId()})), "can't be narrowed by request or request type")
	require.ErrorContains(t, checkExportFilter(inv(auditExportFilter{HostingEnv: strings.Repeat("e", maxHostingEnvLen+1)})), "environment")
	require.ErrorContains(t, checkExportFilter(auditExportFilter{Format: "csv", HostingEnv: "prod"}), "only the bot inventory can be narrowed by environment")
	require.ErrorContains(t, checkExportFilter(auditExportFilter{Format: "csv", Verbose: true}), "only the bot inventory has a verbose mode")
	require.ErrorContains(t, checkExportFilter(auditExportFilter{Type: exportBotInventory, Format: "xml"}), "pick PDF, CSV or JSON for bot inventory")
}

func TestDescribeExportFilter_Inventory(t *testing.T) {
	p, _, _ := inventoryPlugin(t)
	require.Equal(t, "Every bot Request Hub created", p.describeExportFilter(auditExportFilter{Type: exportBotInventory}))
	require.Equal(t, "Every bot Request Hub created, with recommended-extra columns",
		p.describeExportFilter(auditExportFilter{Type: exportBotInventory, Verbose: true}))
	require.Equal(t, "Only bots in `prod`, requested by @req, approved since January 1, 1970",
		p.describeExportFilter(auditExportFilter{Type: exportBotInventory, HostingEnv: "prod", Requester: "u_req", Since: 1}))
	require.Equal(t, "Only active since January 1, 1970", p.describeExportFilter(auditExportFilter{Since: 1}))
}

func TestPendingKey_IncludesEnvironment(t *testing.T) {
	a := &auditExportRequest{Filter: auditExportFilter{Type: exportBotInventory, Format: "pdf", HostingEnv: "dev"}}
	b := &auditExportRequest{Filter: auditExportFilter{Type: exportBotInventory, Format: "pdf", HostingEnv: "prod"}}
	require.NotEqual(t, a.pendingKey(), b.pendingKey())
	verbose := &auditExportRequest{Filter: auditExportFilter{Type: exportBotInventory, Format: "pdf", HostingEnv: "prod", Verbose: true}}
	require.NotEqual(t, b.pendingKey(), verbose.pendingKey())
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
	require.Contains(t, string(uploaded), "(alpha-renamed) Tj")
	require.NotContains(t, string(uploaded), "(beta) Tj", "a dev bot isn't in a prod inventory")

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
	var rows []map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	require.GreaterOrEqual(t, len(rows), 1)
	botIDRow := map[string]string{}
	for _, row := range rows {
		if row["Field"] == "Bot ID" {
			botIDRow = row
			break
		}
	}
	require.Equal(t, "b2", botIDRow["beta"])
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
