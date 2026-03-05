// Package tools contains all tool handler implementations for the
// tools.sessions plugin. Each function returns a ToolHandler closure that
// captures the DataStorage for data access and cross-plugin communication.
package tools

import (
	"context"
	"fmt"
	"strings"

	pluginv1 "github.com/orchestra-mcp/gen-go/orchestra/plugin/v1"
	"github.com/orchestra-mcp/sdk-go/helpers"
	"github.com/orchestra-mcp/plugin-tools-sessions/internal/storage"
	"google.golang.org/protobuf/types/known/structpb"
)

// ToolHandler is an alias for readability.
type ToolHandler = func(ctx context.Context, req *pluginv1.ToolRequest) (*pluginv1.ToolResponse, error)

// ---------- Schemas ----------

// CreateSessionSchema returns the JSON Schema for the create_session tool.
func CreateSessionSchema() *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"account_id": map[string]any{
				"type":        "string",
				"description": "Account ID that owns this session",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Human-readable session name (optional)",
			},
			"workspace": map[string]any{
				"type":        "string",
				"description": "Workspace directory for the session (optional)",
			},
			"model": map[string]any{
				"type":        "string",
				"description": "Model to use (e.g. claude-sonnet-4-20250514). Defaults to claude-sonnet-4-20250514",
			},
			"permission_mode": map[string]any{
				"type":        "string",
				"description": "Permission mode for Claude Code (e.g. plan, auto-edit). Defaults to plan",
			},
			"allowed_tools": map[string]any{
				"type":        "array",
				"description": "List of allowed tool names (optional)",
				"items":       map[string]any{"type": "string"},
			},
			"max_budget": map[string]any{
				"type":        "number",
				"description": "Maximum budget in USD for this session (optional, 0 = unlimited)",
			},
			"system_prompt": map[string]any{
				"type":        "string",
				"description": "System prompt to prepend to every message (optional)",
			},
		},
		"required": []any{"account_id"},
	})
	return s
}

// ListSessionsSchema returns the JSON Schema for the list_sessions tool.
func ListSessionsSchema() *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{
				"type":        "string",
				"description": "Filter by status: active, paused, completed (optional)",
			},
			"account_id": map[string]any{
				"type":        "string",
				"description": "Filter by account ID (optional)",
			},
		},
	})
	return s
}

// GetSessionSchema returns the JSON Schema for the get_session tool.
func GetSessionSchema() *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "Session ID to retrieve",
			},
			"message_count": map[string]any{
				"type":        "number",
				"description": "Number of recent messages to include (default 5)",
			},
		},
		"required": []any{"session_id"},
	})
	return s
}

// DeleteSessionSchema returns the JSON Schema for the delete_session tool.
func DeleteSessionSchema() *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "Session ID to delete",
			},
		},
		"required": []any{"session_id"},
	})
	return s
}

// PauseSessionSchema returns the JSON Schema for the pause_session tool.
func PauseSessionSchema() *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "Session ID to pause",
			},
		},
		"required": []any{"session_id"},
	})
	return s
}

// ---------- Handlers ----------

// CreateSession creates a new persistent Claude Code session.
func CreateSession(store *storage.DataStorage) ToolHandler {
	return func(ctx context.Context, req *pluginv1.ToolRequest) (*pluginv1.ToolResponse, error) {
		if err := helpers.ValidateRequired(req.Arguments, "account_id"); err != nil {
			return helpers.ErrorResult("validation_error", err.Error()), nil
		}

		accountID := helpers.GetString(req.Arguments, "account_id")
		name := helpers.GetStringOr(req.Arguments, "name", "Untitled Session")
		workspace := helpers.GetString(req.Arguments, "workspace")
		model := helpers.GetStringOr(req.Arguments, "model", "claude-sonnet-4-20250514")
		permissionMode := helpers.GetStringOr(req.Arguments, "permission_mode", "bypassPermissions")
		allowedTools := helpers.GetStringSlice(req.Arguments, "allowed_tools")
		maxBudget := helpers.GetFloat64(req.Arguments, "max_budget")
		systemPrompt := helpers.GetString(req.Arguments, "system_prompt")

		sessionID := helpers.NewUUID()
		now := helpers.NowISO()

		session := &storage.Session{
			ID:             sessionID,
			AccountID:      accountID,
			Name:           name,
			Workspace:      workspace,
			Model:          model,
			PermissionMode: permissionMode,
			AllowedTools:   allowedTools,
			MaxBudget:      maxBudget,
			SystemPrompt:   systemPrompt,
			Status:         "active",
			CreatedAt:      now,
			LastMessageAt:  "",
			MessageCount:   0,
			TotalTokensIn:  0,
			TotalTokensOut: 0,
			TotalCostUSD:   0,
		}

		_, err := store.WriteSession(ctx, session, 0)
		if err != nil {
			return helpers.ErrorResult("storage_error", err.Error()), nil
		}

		md := formatSessionCreatedMD(session)
		return helpers.TextResult(md), nil
	}
}

// ListSessions returns all sessions, optionally filtered by status and account.
func ListSessions(store *storage.DataStorage) ToolHandler {
	return func(ctx context.Context, req *pluginv1.ToolRequest) (*pluginv1.ToolResponse, error) {
		status := helpers.GetString(req.Arguments, "status")
		accountID := helpers.GetString(req.Arguments, "account_id")

		if status != "" {
			if err := helpers.ValidateOneOf(status, "active", "paused", "completed"); err != nil {
				return helpers.ErrorResult("validation_error", err.Error()), nil
			}
		}

		sessions, err := store.ListSessions(ctx, status, accountID)
		if err != nil {
			return helpers.ErrorResult("storage_error", err.Error()), nil
		}

		if sessions == nil {
			sessions = []*storage.Session{}
		}

		md := formatSessionListMD(sessions, status)
		return helpers.TextResult(md), nil
	}
}

// GetSession returns session details with the last N turn messages.
func GetSession(store *storage.DataStorage) ToolHandler {
	return func(ctx context.Context, req *pluginv1.ToolRequest) (*pluginv1.ToolResponse, error) {
		if err := helpers.ValidateRequired(req.Arguments, "session_id"); err != nil {
			return helpers.ErrorResult("validation_error", err.Error()), nil
		}

		sessionID := helpers.GetString(req.Arguments, "session_id")
		messageCount := helpers.GetInt(req.Arguments, "message_count")
		if messageCount <= 0 {
			messageCount = 5
		}

		session, _, err := store.ReadSession(ctx, sessionID)
		if err != nil {
			return helpers.ErrorResult("not_found", fmt.Sprintf("session %q not found", sessionID)), nil
		}

		turns, err := store.LastNTurns(ctx, sessionID, messageCount)
		if err != nil {
			// Non-fatal: return session data without turns.
			turns = nil
		}

		md := formatSessionDetailMD(session, turns)
		return helpers.TextResult(md), nil
	}
}

// DeleteSession removes a session and all its turn files.
func DeleteSession(store *storage.DataStorage) ToolHandler {
	return func(ctx context.Context, req *pluginv1.ToolRequest) (*pluginv1.ToolResponse, error) {
		if err := helpers.ValidateRequired(req.Arguments, "session_id"); err != nil {
			return helpers.ErrorResult("validation_error", err.Error()), nil
		}

		sessionID := helpers.GetString(req.Arguments, "session_id")

		// Verify session exists.
		_, _, err := store.ReadSession(ctx, sessionID)
		if err != nil {
			return helpers.ErrorResult("not_found", fmt.Sprintf("session %q not found", sessionID)), nil
		}

		if err := store.DeleteSession(ctx, sessionID); err != nil {
			return helpers.ErrorResult("storage_error", err.Error()), nil
		}

		return helpers.TextResult(fmt.Sprintf("Deleted session `%s` and all its conversation history.", sessionID)), nil
	}
}

// PauseSession stops a running session process and marks it as paused.
func PauseSession(store *storage.DataStorage) ToolHandler {
	return func(ctx context.Context, req *pluginv1.ToolRequest) (*pluginv1.ToolResponse, error) {
		if err := helpers.ValidateRequired(req.Arguments, "session_id"); err != nil {
			return helpers.ErrorResult("validation_error", err.Error()), nil
		}

		sessionID := helpers.GetString(req.Arguments, "session_id")

		session, version, err := store.ReadSession(ctx, sessionID)
		if err != nil {
			return helpers.ErrorResult("not_found", fmt.Sprintf("session %q not found", sessionID)), nil
		}

		if session.Status == "paused" {
			return helpers.TextResult(fmt.Sprintf("Session `%s` is already paused.", sessionID)), nil
		}
		if session.Status == "completed" {
			return helpers.ErrorResult("invalid_state", "cannot pause a completed session"), nil
		}

		// Call bridge.claude kill_session to stop any running process.
		killResp, err := store.CallTool(ctx, "kill_session", map[string]any{
			"session_id": sessionID,
		})
		if err != nil {
			// Log the error but continue with marking paused. The process may
			// not be running.
			_ = killResp
		}

		session.Status = "paused"

		_, err = store.WriteSession(ctx, session, version)
		if err != nil {
			return helpers.ErrorResult("storage_error", err.Error()), nil
		}

		return helpers.TextResult(fmt.Sprintf("Paused session `%s` (%s).", sessionID, session.Name)), nil
	}
}

// ---------- Markdown formatters ----------

func formatSessionCreatedMD(s *storage.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Created session **%s**\n\n", s.ID)
	fmt.Fprintf(&b, "- **Name:** %s\n", s.Name)
	fmt.Fprintf(&b, "- **Account:** %s\n", s.AccountID)
	fmt.Fprintf(&b, "- **Model:** %s\n", s.Model)
	fmt.Fprintf(&b, "- **Permission Mode:** %s\n", s.PermissionMode)
	fmt.Fprintf(&b, "- **Status:** %s\n", s.Status)
	if s.Workspace != "" {
		fmt.Fprintf(&b, "- **Workspace:** %s\n", s.Workspace)
	}
	if s.MaxBudget > 0 {
		fmt.Fprintf(&b, "- **Max Budget:** $%.2f\n", s.MaxBudget)
	}
	if len(s.AllowedTools) > 0 {
		fmt.Fprintf(&b, "- **Allowed Tools:** %s\n", strings.Join(s.AllowedTools, ", "))
	}
	return b.String()
}

func formatSessionListMD(sessions []*storage.Session, statusFilter string) string {
	header := "Sessions"
	if statusFilter != "" {
		header = fmt.Sprintf("Sessions (%s)", statusFilter)
	}

	if len(sessions) == 0 {
		return fmt.Sprintf("## %s\n\nNo sessions found.\n", header)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%d)\n\n", header, len(sessions))
	fmt.Fprintf(&b, "| ID | Name | Account | Status | Messages | Cost |\n")
	fmt.Fprintf(&b, "|----|------|---------|--------|----------|------|\n")
	for _, s := range sessions {
		name := s.Name
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		idShort := s.ID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | $%.2f |\n",
			idShort, name, s.AccountID, s.Status, s.MessageCount, s.TotalCostUSD)
	}
	return b.String()
}

func formatSessionDetailMD(s *storage.Session, turns []*storage.Turn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Session: %s\n\n", s.Name)
	fmt.Fprintf(&b, "- **ID:** `%s`\n", s.ID)
	fmt.Fprintf(&b, "- **Account:** %s\n", s.AccountID)
	fmt.Fprintf(&b, "- **Model:** %s\n", s.Model)
	fmt.Fprintf(&b, "- **Permission Mode:** %s\n", s.PermissionMode)
	fmt.Fprintf(&b, "- **Status:** %s\n", s.Status)
	fmt.Fprintf(&b, "- **Messages:** %d\n", s.MessageCount)
	fmt.Fprintf(&b, "- **Created:** %s\n", s.CreatedAt)
	if s.LastMessageAt != "" {
		fmt.Fprintf(&b, "- **Last Message:** %s\n", s.LastMessageAt)
	}
	if s.Workspace != "" {
		fmt.Fprintf(&b, "- **Workspace:** %s\n", s.Workspace)
	}
	if s.MaxBudget > 0 {
		fmt.Fprintf(&b, "- **Max Budget:** $%.2f\n", s.MaxBudget)
	}
	fmt.Fprintf(&b, "- **Total Tokens In:** %d\n", s.TotalTokensIn)
	fmt.Fprintf(&b, "- **Total Tokens Out:** %d\n", s.TotalTokensOut)
	fmt.Fprintf(&b, "- **Total Cost:** $%.4f\n", s.TotalCostUSD)
	if len(s.AllowedTools) > 0 {
		fmt.Fprintf(&b, "- **Allowed Tools:** %s\n", strings.Join(s.AllowedTools, ", "))
	}

	if len(turns) > 0 {
		fmt.Fprintf(&b, "\n---\n\n### Recent Messages (%d)\n\n", len(turns))
		for _, t := range turns {
			fmt.Fprintf(&b, "#### Turn %d — %s\n\n", t.Number, t.Timestamp)
			fmt.Fprintf(&b, "**User:** %s\n\n", truncate(t.UserPrompt, 200))
			fmt.Fprintf(&b, "**Response:** %s\n\n", truncate(t.Response, 500))
			fmt.Fprintf(&b, "_Model: %s | Tokens: %d in / %d out | Cost: $%.4f | Duration: %dms_\n\n",
				t.Model, t.TokensIn, t.TokensOut, t.CostUSD, t.DurationMs)
		}
	}

	return b.String()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
