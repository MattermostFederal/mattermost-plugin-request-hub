package main

import (
	"context"
	"strings"

	"github.com/mattermost/mattermost-plugin-agents/v2/external/pluginmcp"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/errors"
)

// The tools are a third entry point beside the slash command and the
// webapp forms, with one request tool per request type. Each goes through
// the same submit function as the form for its type, which owns validation
// and membership checks. Every request, a System Admin's included, is
// queued for approval. Each tool honors the same per-type setting as the
// slash command and the forms. Approving and denying are deliberately not
// tools: they are the human checkpoint.

type ListPrefixesArgs struct{}

type MCPPrefix struct {
	Prefix      string `json:"prefix" jsonschema:"The prefix exactly as it must be passed to request_channel, such as team-"`
	Description string `json:"description,omitempty" jsonschema:"What channels under this prefix are for"`
	SuffixRule  string `json:"suffix_rule,omitempty" jsonschema:"The rule for the part after the prefix: a maximum length, or a pattern it must match. Empty means no rule"`
}

type ListPrefixesOutput struct {
	Prefixes []MCPPrefix `json:"prefixes" jsonschema:"The channel prefixes a request can use"`
}

type RequestChannelArgs struct {
	TeamID       string   `json:"team_id" jsonschema:"The ID of the team to create the channel in"`
	Prefix       string   `json:"prefix" jsonschema:"One of the prefixes from list_prefixes, exactly as listed"`
	DisplayName  string   `json:"display_name" jsonschema:"The channel's display name, at most 64 characters"`
	Name         string   `json:"name,omitempty" jsonschema:"The URL suffix after the prefix; generated from display_name when blank"`
	Purpose      string   `json:"purpose,omitempty" jsonschema:"The channel purpose, at most 250 characters"`
	ChannelType  string   `json:"channel_type,omitempty" jsonschema:"O for a public channel (the default) or P for a private one"`
	Members      []string `json:"members,omitempty" jsonschema:"Usernames to add as members, at most 100"`
	AdminMembers []string `json:"admin_members,omitempty" jsonschema:"Usernames to make Channel Admins, at most 100"`
}

type RequestChannelAdminArgs struct {
	ChannelID string   `json:"channel_id" jsonschema:"The ID of an existing public or private channel the user is a member of"`
	Nominees  []string `json:"nominees" jsonschema:"Usernames of channel members to make Channel Admins, at most 100"`
}

type RequestTeamArgs struct {
	DisplayName      string `json:"display_name" jsonschema:"The team's display name, at most 64 characters"`
	Name             string `json:"name,omitempty" jsonschema:"The team URL name, 2-64 lowercase letters, numbers and hyphens; generated from display_name when blank"`
	Description      string `json:"description,omitempty" jsonschema:"The team description, at most 255 bytes"`
	InviteOnly       bool   `json:"invite_only,omitempty" jsonschema:"True for an invite-only team; the default is open"`
	RequestTeamAdmin bool   `json:"request_team_admin,omitempty" jsonschema:"True to also ask for the user to be made a Team Admin of the new team"`
}

type RequestTeamAdminArgs struct {
	TeamID   string   `json:"team_id" jsonschema:"The ID of a team the user is a member of"`
	Nominees []string `json:"nominees" jsonschema:"Usernames of team members to make Team Admins, at most 100"`
}

type RequestTeamMemberArgs struct {
	TeamID   string   `json:"team_id" jsonschema:"The ID of the team to add the people to"`
	Nominees []string `json:"nominees" jsonschema:"Usernames of the people to add to the team, at most 100"`
	Reason   string   `json:"reason,omitempty" jsonschema:"Why they need access, at most 500 characters"`
}

type RequestTeamGuestArgs struct {
	TeamID     string   `json:"team_id" jsonschema:"The ID of a team the user is a member of"`
	Email      string   `json:"email" jsonschema:"The guest's email address"`
	ChannelIDs []string `json:"channel_ids" jsonschema:"IDs of public or private channels of the team, which the user is a member of, for the guest to join; at most 100"`
	Reason     string   `json:"reason,omitempty" jsonschema:"Why the guest needs access, at most 500 characters"`
}

type RequestBotArgs struct {
	Username    string `json:"username" jsonschema:"The bot's username, 3-22 lowercase letters, numbers and . - _"`
	DisplayName string `json:"display_name" jsonschema:"The bot's display name, at most 64 characters"`
	Description string `json:"description,omitempty" jsonschema:"What the bot is for, at most 500 characters"`
	Owner       string `json:"owner,omitempty" jsonschema:"The username of the bot's owner; the user when blank"`
}

type RequestWebhookArgs struct {
	WebhookType string `json:"webhook_type" jsonschema:"incoming or outgoing"`
	ChannelID   string `json:"channel_id" jsonschema:"The ID of a channel the user is a member of; public or private for incoming, public only for outgoing"`
	DisplayName string `json:"display_name" jsonschema:"The webhook's display name, at most 64 bytes"`
	Description string `json:"description,omitempty" jsonschema:"What the webhook is for, at most 500 bytes"`
	CallbackURL string `json:"callback_url,omitempty" jsonschema:"For outgoing webhooks only, and required for them: the http or https URL to post to"`
}

type MCPMessageOutput struct {
	Message string `json:"message" jsonschema:"What happened: the request was submitted for approval"`
}

var errNoMCPUser = errors.New("no user is associated with this request")

func (p *Plugin) registerMCPTools(server *pluginmcp.Server) {
	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "list_prefixes",
		Description: "List the channel prefixes a channel request can use, with what each is for and any rule for the rest of the name.",
	}, p.listPrefixesTool)

	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "request_channel",
		Description: "Request a new channel on the user's behalf. The request is posted for admin approval. Call list_prefixes first for the prefix.",
	}, p.requestChannelTool)

	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "request_channel_admin",
		Description: "Request that existing members of a channel be made Channel Admins, on the user's behalf. The request is posted for admin approval.",
	}, p.requestChannelAdminTool)

	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "request_team",
		Description: "Request a new team on the user's behalf. The request is posted for admin approval.",
	}, p.requestTeamTool)

	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "request_team_admin",
		Description: "Request that existing members of a team be made Team Admins, on the user's behalf. The request is posted for admin approval.",
	}, p.requestTeamAdminTool)

	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "request_team_member",
		Description: "Request that people be added to a team, on the user's behalf. The request is posted for admin approval.",
	}, p.requestTeamMemberTool)

	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "request_team_guest",
		Description: "Request a guest invite by email to channels of a team, on the user's behalf. The request is posted for admin approval.",
	}, p.requestTeamGuestTool)

	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "request_bot",
		Description: "Request a new bot account on the user's behalf. The request is posted for admin approval.",
	}, p.requestBotTool)

	pluginmcp.AddTool(server, &mcp.Tool{
		Name:        "request_webhook",
		Description: "Request an incoming or outgoing webhook for a channel, on the user's behalf. The request is posted for admin approval.",
	}, p.requestWebhookTool)
}

func (p *Plugin) listPrefixesTool(ctx context.Context, _ *mcp.CallToolRequest, _ ListPrefixesArgs) (*mcp.CallToolResult, ListPrefixesOutput, error) {
	if pluginmcp.GetUserID(ctx) == "" {
		return nil, ListPrefixesOutput{}, errNoMCPUser
	}

	config := p.getConfiguration()
	if !config.RequestEnabled(requestTypeChannel) {
		return nil, ListPrefixesOutput{}, errors.New(channelRequestsDisabledMsg)
	}
	if !config.UsesPrefixList() {
		return nil, ListPrefixesOutput{}, errors.New(notConfiguredMessage)
	}

	out := ListPrefixesOutput{Prefixes: make([]MCPPrefix, 0, len(config.prefixes))}
	for _, pf := range config.prefixes {
		out.Prefixes = append(out.Prefixes, MCPPrefix{
			Prefix:      pf.Prefix,
			Description: pf.Description,
			SuffixRule:  pf.SuffixPatternRaw,
		})
	}
	return nil, out, nil
}

func (p *Plugin) requestChannelTool(ctx context.Context, _ *mcp.CallToolRequest, in RequestChannelArgs) (*mcp.CallToolResult, MCPMessageOutput, error) {
	userID := pluginmcp.GetUserID(ctx)
	if userID == "" {
		return nil, MCPMessageOutput{}, errNoMCPUser
	}
	if !p.getConfiguration().RequestEnabled(requestTypeChannel) {
		return nil, MCPMessageOutput{}, errors.New(channelRequestsDisabledMsg)
	}

	memberIDs, err := p.resolveUsernameList(in.Members)
	if err != nil {
		return nil, MCPMessageOutput{}, err
	}
	adminMemberIDs, err := p.resolveUsernameList(in.AdminMembers)
	if err != nil {
		return nil, MCPMessageOutput{}, err
	}

	return mcpMessage(p.submitRequest(requestInput{
		RequesterID:    userID,
		TeamID:         in.TeamID,
		DisplayName:    in.DisplayName,
		Name:           in.Name,
		Prefix:         in.Prefix,
		Purpose:        in.Purpose,
		ChannelType:    in.ChannelType,
		MemberIDs:      memberIDs,
		AdminMemberIDs: adminMemberIDs,
	}))
}

func (p *Plugin) requestChannelAdminTool(ctx context.Context, _ *mcp.CallToolRequest, in RequestChannelAdminArgs) (*mcp.CallToolResult, MCPMessageOutput, error) {
	userID := pluginmcp.GetUserID(ctx)
	if userID == "" {
		return nil, MCPMessageOutput{}, errNoMCPUser
	}
	if !p.getConfiguration().RequestEnabled(requestTypeChannelAdmin) {
		return nil, MCPMessageOutput{}, errors.New(channelAdminRequestsDisabledMsg)
	}

	nomineeIDs, err := p.resolveUsernameList(in.Nominees)
	if err != nil {
		return nil, MCPMessageOutput{}, err
	}

	return mcpMessage(p.submitAdminRequest(userID, in.ChannelID, nomineeIDs))
}

func (p *Plugin) requestTeamTool(ctx context.Context, _ *mcp.CallToolRequest, in RequestTeamArgs) (*mcp.CallToolResult, MCPMessageOutput, error) {
	userID := pluginmcp.GetUserID(ctx)
	if userID == "" {
		return nil, MCPMessageOutput{}, errNoMCPUser
	}

	teamType := model.TeamOpen
	if in.InviteOnly {
		teamType = model.TeamInvite
	}
	return mcpMessage(p.submitTeamCreationRequest(teamCreationInput{
		RequesterID:      userID,
		DisplayName:      in.DisplayName,
		Name:             in.Name,
		Description:      in.Description,
		Type:             teamType,
		RequestTeamAdmin: in.RequestTeamAdmin,
	}))
}

func (p *Plugin) requestTeamAdminTool(ctx context.Context, _ *mcp.CallToolRequest, in RequestTeamAdminArgs) (*mcp.CallToolResult, MCPMessageOutput, error) {
	userID := pluginmcp.GetUserID(ctx)
	if userID == "" {
		return nil, MCPMessageOutput{}, errNoMCPUser
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeamAdmin) {
		return nil, MCPMessageOutput{}, errors.New(teamAdminRequestsDisabledMsg)
	}

	nomineeIDs, err := p.resolveUsernameList(in.Nominees)
	if err != nil {
		return nil, MCPMessageOutput{}, err
	}
	return mcpMessage(p.submitTeamAdminRequest(userID, in.TeamID, nomineeIDs))
}

func (p *Plugin) requestTeamMemberTool(ctx context.Context, _ *mcp.CallToolRequest, in RequestTeamMemberArgs) (*mcp.CallToolResult, MCPMessageOutput, error) {
	userID := pluginmcp.GetUserID(ctx)
	if userID == "" {
		return nil, MCPMessageOutput{}, errNoMCPUser
	}
	if !p.getConfiguration().RequestEnabled(requestTypeTeamMember) {
		return nil, MCPMessageOutput{}, errors.New(teamMemberRequestsDisabledMsg)
	}

	nomineeIDs, err := p.resolveUsernameList(in.Nominees)
	if err != nil {
		return nil, MCPMessageOutput{}, err
	}
	return mcpMessage(p.submitTeamMemberRequest(userID, in.TeamID, nomineeIDs, in.Reason))
}

func (p *Plugin) requestTeamGuestTool(ctx context.Context, _ *mcp.CallToolRequest, in RequestTeamGuestArgs) (*mcp.CallToolResult, MCPMessageOutput, error) {
	userID := pluginmcp.GetUserID(ctx)
	if userID == "" {
		return nil, MCPMessageOutput{}, errNoMCPUser
	}
	return mcpMessage(p.submitTeamGuestRequest(userID, in.TeamID, in.Email, in.ChannelIDs, in.Reason))
}

func (p *Plugin) requestBotTool(ctx context.Context, _ *mcp.CallToolRequest, in RequestBotArgs) (*mcp.CallToolResult, MCPMessageOutput, error) {
	userID := pluginmcp.GetUserID(ctx)
	if userID == "" {
		return nil, MCPMessageOutput{}, errNoMCPUser
	}
	if !p.getConfiguration().RequestEnabled(requestTypeBot) {
		return nil, MCPMessageOutput{}, errors.New(botRequestsDisabledMsg)
	}

	// A blank owner leaves the owner to submitBotRequest's default, as the
	// slash command does.
	var ownerID string
	if strings.TrimSpace(in.Owner) != "" {
		ownerIDs, err := p.resolveUsernameList([]string{in.Owner})
		if err != nil {
			return nil, MCPMessageOutput{}, err
		}
		if len(ownerIDs) == 0 {
			return nil, MCPMessageOutput{}, errors.New("name the owner by username")
		}
		ownerID = ownerIDs[0]
	}

	return mcpMessage(p.submitBotRequest(userID, botRequestInput{
		Username:    in.Username,
		DisplayName: in.DisplayName,
		Description: in.Description,
		OwnerUserID: ownerID,
	}))
}

func (p *Plugin) requestWebhookTool(ctx context.Context, _ *mcp.CallToolRequest, in RequestWebhookArgs) (*mcp.CallToolResult, MCPMessageOutput, error) {
	userID := pluginmcp.GetUserID(ctx)
	if userID == "" {
		return nil, MCPMessageOutput{}, errNoMCPUser
	}
	webhookType := strings.ToLower(strings.TrimSpace(in.WebhookType))
	return mcpMessage(p.submitWebhookRequest(userID, webhookType, in.ChannelID, in.DisplayName, in.Description, in.CallbackURL))
}

// mcpMessage turns a submit function's reply into a tool result.
func mcpMessage(message string, err error) (*mcp.CallToolResult, MCPMessageOutput, error) {
	if err != nil {
		return nil, MCPMessageOutput{}, err
	}
	return nil, MCPMessageOutput{Message: message}, nil
}
