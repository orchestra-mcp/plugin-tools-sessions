// Package internal contains the core registration logic for the tools.sessions
// plugin. The ToolsPlugin struct wires all 6 session tool handlers to the
// plugin builder with their schemas and descriptions.
package internal

import (
	"github.com/orchestra-mcp/sdk-go/plugin"
	"github.com/orchestra-mcp/plugin-tools-sessions/internal/storage"
	"github.com/orchestra-mcp/plugin-tools-sessions/internal/tools"
)

// ToolsPlugin holds the shared dependencies for all tool handlers.
type ToolsPlugin struct {
	Storage *storage.DataStorage
}

// RegisterTools registers all 6 session tools on the given plugin builder.
func (tp *ToolsPlugin) RegisterTools(builder *plugin.PluginBuilder) {
	s := tp.Storage

	// --- Session management tools (5) ---
	builder.RegisterTool("create_session",
		"Create a new persistent Claude Code session for an account",
		tools.CreateSessionSchema(), tools.CreateSession(s))

	builder.RegisterTool("list_sessions",
		"List all sessions, optionally filtered by status or account",
		tools.ListSessionsSchema(), tools.ListSessions(s))

	builder.RegisterTool("get_session",
		"Get session details with recent conversation history",
		tools.GetSessionSchema(), tools.GetSession(s))

	builder.RegisterTool("delete_session",
		"Delete a session and all its conversation history",
		tools.DeleteSessionSchema(), tools.DeleteSession(s))

	builder.RegisterTool("pause_session",
		"Pause a running session and stop its Claude Code process",
		tools.PauseSessionSchema(), tools.PauseSession(s))

	// --- Chat tool (1) ---
	builder.RegisterTool("send_message",
		"Send a message to a session and get the AI response",
		tools.SendMessageSchema(), tools.SendMessage(s))
}
