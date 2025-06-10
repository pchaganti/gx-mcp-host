package tools

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	einomcp "github.com/cloudwego/eino-ext/components/tool/mcp"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcphost/internal/config"
)

// MCPToolManager manages MCP tools and clients
type MCPToolManager struct {
	clients map[string]client.MCPClient
	tools   []tool.BaseTool
}

// NewMCPToolManager creates a new MCP tool manager
func NewMCPToolManager() *MCPToolManager {
	return &MCPToolManager{
		clients: make(map[string]client.MCPClient),
		tools:   make([]tool.BaseTool, 0),
	}
}

// LoadTools loads tools from MCP servers based on configuration
func (m *MCPToolManager) LoadTools(ctx context.Context, config *config.Config) error {
	for serverName, serverConfig := range config.MCPServers {
		client, err := m.createMCPClient(ctx, serverName, serverConfig)
		if err != nil {
			return fmt.Errorf("failed to create MCP client for %s: %v", serverName, err)
		}

		m.clients[serverName] = client

		// Initialize the client
		if err := m.initializeClient(ctx, client); err != nil {
			return fmt.Errorf("failed to initialize MCP client for %s: %v", serverName, err)
		}

		// Get allowed tools list for this server
		var allowedTools []string
		if len(serverConfig.AllowedTools) > 0 {
			allowedTools = serverConfig.AllowedTools
		} else {
			// If no allowed tools specified, get all tools and filter out excluded ones
			toolsResult, err := client.ListTools(ctx, mcp.ListToolsRequest{})
			if err != nil {
				return fmt.Errorf("failed to list tools from server %s: %v", serverName, err)
			}

			for _, mcpTool := range toolsResult.Tools {
				if !m.isToolExcluded(mcpTool.Name, serverConfig.ExcludedTools) {
					allowedTools = append(allowedTools, mcpTool.Name)
				}
			}
		}

		// Use eino's MCP tool adapter
		mcpTools, err := einomcp.GetTools(ctx, &einomcp.Config{
			Cli:          client,
			ToolNameList: allowedTools,
		})
		if err != nil {
			return fmt.Errorf("failed to get MCP tools from server %s: %v", serverName, err)
		}

		// Add tools directly - eino's MCP adapter should handle everything
		for _, mcpTool := range mcpTools {
			// Check if the tool already has a prefix, if not add server prefix
			if invokableTool, ok := mcpTool.(tool.InvokableTool); ok {
				wrappedTool := &PrefixedTool{
					InvokableTool: invokableTool,
					prefix:        serverName,
				}
				m.tools = append(m.tools, wrappedTool)
			} else {
				return fmt.Errorf("tool from server %s does not implement InvokableTool interface", serverName)
			}
		}
	}

	return nil
}

// GetTools returns all loaded tools
func (m *MCPToolManager) GetTools() []tool.BaseTool {
	return m.tools
}

// Close closes all MCP clients
func (m *MCPToolManager) Close() error {
	for name, client := range m.clients {
		if err := client.Close(); err != nil {
			return fmt.Errorf("failed to close client %s: %v", name, err)
		}
	}
	return nil
}

// isToolExcluded checks if a tool is in the excluded list
func (m *MCPToolManager) isToolExcluded(toolName string, excludedTools []string) bool {
	for _, excludedTool := range excludedTools {
		if excludedTool == toolName {
			return true
		}
	}
	return false
}

func (m *MCPToolManager) createMCPClient(ctx context.Context, serverName string, serverConfig config.MCPServerConfig) (client.MCPClient, error) {
	if serverConfig.Command != "" {
		// STDIO client
		return client.NewStdioMCPClient(serverConfig.Command, nil, serverConfig.Args...)
	} else if serverConfig.URL != "" {
		// SSE client
		sseClient, err := client.NewSSEMCPClient(serverConfig.URL)
		if err != nil {
			return nil, err
		}

		// Start the SSE client
		if err := sseClient.Start(ctx); err != nil {
			return nil, fmt.Errorf("failed to start SSE client: %v", err)
		}

		return sseClient, nil
	}

	return nil, fmt.Errorf("invalid server configuration for %s: must specify either command or url", serverName)
}

func (m *MCPToolManager) initializeClient(ctx context.Context, client client.MCPClient) error {
	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{
		Name:    "mcphost",
		Version: "1.0.0",
	}

	_, err := client.Initialize(ctx, initRequest)
	return err
}

// PrefixedTool wraps an eino tool to add a server prefix to its name
type PrefixedTool struct {
	tool.InvokableTool
	prefix string
}

// Info returns the tool information with prefixed name
func (p *PrefixedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	info, err := p.InvokableTool.Info(ctx)
	if err != nil {
		return nil, err
	}
	
	// Add server prefix to tool name only if it doesn't already have one
	if !hasPrefix(info.Name, p.prefix) {
		info.Name = fmt.Sprintf("%s__%s", p.prefix, info.Name)
	}
	return info, nil
}

// hasPrefix checks if the tool name already has the server prefix
func hasPrefix(toolName, prefix string) bool {
	expectedPrefix := prefix + "__"
	return len(toolName) > len(expectedPrefix) && toolName[:len(expectedPrefix)] == expectedPrefix
}
