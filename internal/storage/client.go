// Package storage provides an abstraction over the orchestrator's storage and
// tool-call protocols for reading and writing session and turn data. The
// StorageClient interface allows swapping a real QUIC-based client for an
// in-memory fake during testing.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	pluginv1 "github.com/orchestra-mcp/gen-go/orchestra/plugin/v1"
	"github.com/orchestra-mcp/sdk-go/helpers"
	"google.golang.org/protobuf/types/known/structpb"
)

// StorageClient sends requests to the orchestrator for storage and tool-call
// operations.
type StorageClient interface {
	Send(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error)
}

// DataStorage wraps the storage client for session and turn operations.
type DataStorage struct {
	client StorageClient
}

// NewDataStorage creates a new DataStorage with the given client.
func NewDataStorage(client StorageClient) *DataStorage {
	return &DataStorage{client: client}
}

// ---------- Session types ----------

// Session represents a persistent Claude Code session.
type Session struct {
	ID             string   `json:"id"`
	AccountID      string   `json:"account_id"`
	Name           string   `json:"name"`
	Workspace      string   `json:"workspace"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permission_mode"`
	AllowedTools   []string `json:"allowed_tools"`
	MaxBudget      float64  `json:"max_budget"`
	SystemPrompt   string   `json:"system_prompt"`
	Status         string   `json:"status"`           // active, paused, completed
	CreatedAt      string   `json:"created_at"`
	LastMessageAt  string   `json:"last_message_at"`
	MessageCount   int      `json:"message_count"`
	TotalTokensIn  int64    `json:"total_tokens_in"`
	TotalTokensOut int64    `json:"total_tokens_out"`
	TotalCostUSD   float64  `json:"total_cost_usd"`
	// ClaudeSessionID is the actual session UUID returned by the claude CLI
	// after a successful invocation. Empty until the first successful turn.
	// Used for --resume on subsequent messages.
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
}

// Turn represents a single conversation turn within a session.
type Turn struct {
	Number     int     `json:"number"`
	UserPrompt string  `json:"user_prompt"`
	Response   string  `json:"response"`
	TokensIn   int64   `json:"tokens_in"`
	TokensOut  int64   `json:"tokens_out"`
	CostUSD    float64 `json:"cost_usd"`
	Model      string  `json:"model"`
	DurationMs int64   `json:"duration_ms"`
	Timestamp  string  `json:"timestamp"`
	// Events is an optional JSON string containing tool events for this turn.
	// Stored so that sessions can be fully reconstructed on page refresh.
	Events string `json:"events,omitempty"`
}

// ---------- Session operations ----------

// resolveSessionID resolves a (possibly prefix) session ID to the full ID.
// If the exact path exists, it returns the ID as-is. Otherwise it searches
// for a session file starting with the given prefix.
func (ds *DataStorage) resolveSessionID(ctx context.Context, sessionID string) (string, error) {
	// Try exact match first.
	path := sessionPath(sessionID)
	if _, err := ds.storageRead(ctx, path); err == nil {
		return sessionID, nil
	}

	// Prefix match: list all sessions and find one starting with the given ID.
	entries, err := ds.storageList(ctx, "bridge/sessions/", "*.md")
	if err != nil {
		return "", fmt.Errorf("session %q not found", sessionID)
	}
	for _, entry := range entries {
		base := filepath.Base(entry.GetPath())
		if !strings.HasSuffix(base, ".md") {
			continue
		}
		id := strings.TrimSuffix(base, ".md")
		if strings.HasPrefix(id, sessionID) {
			return id, nil
		}
	}
	return "", fmt.Errorf("session %q not found", sessionID)
}

// ReadSession loads a session by ID from storage.
// Supports both full UUIDs and prefix matches (e.g. first 8 chars).
func (ds *DataStorage) ReadSession(ctx context.Context, sessionID string) (*Session, int64, error) {
	resolved, err := ds.resolveSessionID(ctx, sessionID)
	if err != nil {
		return nil, 0, err
	}
	path := sessionPath(resolved)
	resp, err := ds.storageRead(ctx, path)
	if err != nil {
		return nil, 0, fmt.Errorf("read session %s: %w", resolved, err)
	}

	session, err := metadataToSession(resp.Metadata)
	if err != nil {
		return nil, 0, fmt.Errorf("parse session %s: %w", resolved, err)
	}
	return session, resp.Version, nil
}

// WriteSession persists a session to storage.
func (ds *DataStorage) WriteSession(ctx context.Context, session *Session, expectedVersion int64) (int64, error) {
	meta, err := sessionToMetadata(session)
	if err != nil {
		return 0, fmt.Errorf("encode session: %w", err)
	}
	path := sessionPath(session.ID)
	body := fmt.Sprintf("# Session: %s\n\nAccount: %s | Status: %s | Messages: %d\n",
		session.Name, session.AccountID, session.Status, session.MessageCount)
	return ds.storageWrite(ctx, path, meta, []byte(body), expectedVersion)
}

// ListSessions returns all sessions from storage, optionally filtered by
// status and/or account ID.
func (ds *DataStorage) ListSessions(ctx context.Context, status, accountID string) ([]*Session, error) {
	prefix := "bridge/sessions/"
	entries, err := ds.storageList(ctx, prefix, "*.md")
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	var sessions []*Session
	for _, entry := range entries {
		base := filepath.Base(entry.Path)
		// Skip directories (turn subdirectories show up as entries sometimes).
		if !strings.HasSuffix(base, ".md") {
			continue
		}
		sessionID := strings.TrimSuffix(base, ".md")

		sess, _, err := ds.ReadSession(ctx, sessionID)
		if err != nil {
			continue // skip unreadable sessions
		}

		if status != "" && sess.Status != status {
			continue
		}
		if accountID != "" && sess.AccountID != accountID {
			continue
		}

		sessions = append(sessions, sess)
	}
	return sessions, nil
}

// DeleteSession removes a session and all its turn files from storage.
func (ds *DataStorage) DeleteSession(ctx context.Context, sessionID string) error {
	// Resolve prefix to full ID.
	resolved, err := ds.resolveSessionID(ctx, sessionID)
	if err != nil {
		return err
	}

	// Delete all turn files first.
	turns, err := ds.ListTurns(ctx, resolved)
	if err == nil {
		for _, t := range turns {
			turnPath := turnFilePath(resolved, t.Number)
			_ = ds.storageDelete(ctx, turnPath) // best effort
		}
	}

	// Delete the session metadata file.
	path := sessionPath(resolved)
	return ds.storageDelete(ctx, path)
}

// ---------- Turn operations ----------

// WriteTurn persists a turn file for a session.
func (ds *DataStorage) WriteTurn(ctx context.Context, sessionID string, turn *Turn) (int64, error) {
	meta, err := turnToMetadata(turn)
	if err != nil {
		return 0, fmt.Errorf("encode turn: %w", err)
	}
	path := turnFilePath(sessionID, turn.Number)
	body := fmt.Sprintf("## User\n\n%s\n\n## Response\n\n%s\n", turn.UserPrompt, turn.Response)
	if turn.Events != "" {
		body += fmt.Sprintf("\n## Events\n\n```json\n%s\n```\n", turn.Events)
	}
	return ds.storageWrite(ctx, path, meta, []byte(body), 0)
}

// ReadTurn reads a single turn file by session ID and turn number.
func (ds *DataStorage) ReadTurn(ctx context.Context, sessionID string, turnNumber int) (*Turn, error) {
	path := turnFilePath(sessionID, turnNumber)
	resp, err := ds.storageRead(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read turn %d of session %s: %w", turnNumber, sessionID, err)
	}

	turn, err := metadataToTurn(resp.Metadata)
	if err != nil {
		return nil, fmt.Errorf("parse turn %d of session %s: %w", turnNumber, sessionID, err)
	}

	// Extract response body from storage content.
	if len(resp.Content) > 0 {
		content := string(resp.Content)
		// The body format is: ## User\n\n...\n\n## Response\n\n...\n\n## Events\n\n```json\n...\n```
		if idx := strings.Index(content, "## User\n\n"); idx >= 0 {
			endIdx := strings.Index(content, "\n\n## Response")
			if endIdx > idx {
				turn.UserPrompt = strings.TrimSpace(content[idx+len("## User\n\n") : endIdx])
			}
		}
		if idx := strings.Index(content, "## Response\n\n"); idx >= 0 {
			responseStart := idx + len("## Response\n\n")
			responseEnd := len(content)
			if evIdx := strings.Index(content, "\n\n## Events\n\n"); evIdx > idx {
				responseEnd = evIdx
			}
			turn.Response = strings.TrimSpace(content[responseStart:responseEnd])
		}
		// Extract events JSON from ```json ... ``` block.
		if idx := strings.Index(content, "## Events\n\n```json\n"); idx >= 0 {
			start := idx + len("## Events\n\n```json\n")
			if end := strings.Index(content[start:], "\n```"); end >= 0 {
				turn.Events = content[start : start+end]
			}
		}
	}

	return turn, nil
}

// ListTurns returns all turns for a session, sorted by turn number.
func (ds *DataStorage) ListTurns(ctx context.Context, sessionID string) ([]*Turn, error) {
	prefix := fmt.Sprintf("bridge/sessions/%s/", sessionID)
	entries, err := ds.storageList(ctx, prefix, "turn-*.md")
	if err != nil {
		return nil, fmt.Errorf("list turns for session %s: %w", sessionID, err)
	}

	var turns []*Turn
	for _, entry := range entries {
		base := filepath.Base(entry.Path)
		if !strings.HasPrefix(base, "turn-") || !strings.HasSuffix(base, ".md") {
			continue
		}

		// Parse turn number from filename like "turn-001.md".
		numStr := strings.TrimSuffix(strings.TrimPrefix(base, "turn-"), ".md")
		var num int
		if _, err := fmt.Sscanf(numStr, "%d", &num); err != nil {
			continue
		}

		turn, err := ds.ReadTurn(ctx, sessionID, num)
		if err != nil {
			continue // skip unreadable turns
		}
		turns = append(turns, turn)
	}

	sort.Slice(turns, func(i, j int) bool {
		return turns[i].Number < turns[j].Number
	})

	return turns, nil
}

// LastNTurns returns the last N turns for a session.
func (ds *DataStorage) LastNTurns(ctx context.Context, sessionID string, n int) ([]*Turn, error) {
	all, err := ds.ListTurns(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if len(all) <= n {
		return all, nil
	}
	return all[len(all)-n:], nil
}

// ---------- Cross-plugin tool calls ----------

// CallTool sends a ToolRequest to the orchestrator, which routes it to the
// appropriate plugin (e.g. bridge.claude or tools.agentops).
func (ds *DataStorage) CallTool(ctx context.Context, toolName string, args map[string]any) (*pluginv1.ToolResponse, error) {
	return ds.CallToolWithProvider(ctx, toolName, args, "")
}

// CallToolWithProvider sends a ToolRequest with a specific provider, enabling
// the orchestrator to route AI tool calls (ai_prompt, spawn_session) to the
// correct bridge plugin (bridge.claude, bridge.openai, bridge.gemini, etc.).
func (ds *DataStorage) CallToolWithProvider(ctx context.Context, toolName string, args map[string]any, provider string) (*pluginv1.ToolResponse, error) {
	argsStruct, err := structpb.NewStruct(args)
	if err != nil {
		return nil, fmt.Errorf("build args struct for %s: %w", toolName, err)
	}

	req := &pluginv1.PluginRequest{
		RequestId: helpers.NewUUID(),
		Request: &pluginv1.PluginRequest_ToolCall{
			ToolCall: &pluginv1.ToolRequest{
				ToolName:  toolName,
				Arguments: argsStruct,
				Provider:  provider,
			},
		},
	}
	resp, err := ds.client.Send(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("call tool %s: %w", toolName, err)
	}
	tc := resp.GetToolCall()
	if tc == nil {
		return nil, fmt.Errorf("unexpected response type for tool call %s", toolName)
	}
	return tc, nil
}

// ---------- Low-level storage protocol ----------

func (ds *DataStorage) storageRead(ctx context.Context, path string) (*pluginv1.StorageReadResponse, error) {
	resp, err := ds.client.Send(ctx, &pluginv1.PluginRequest{
		RequestId: helpers.NewUUID(),
		Request: &pluginv1.PluginRequest_StorageRead{
			StorageRead: &pluginv1.StorageReadRequest{
				Path:        path,
				StorageType: "markdown",
			},
		},
	})
	if err != nil {
		return nil, err
	}
	sr := resp.GetStorageRead()
	if sr == nil {
		return nil, fmt.Errorf("unexpected response type for storage read")
	}
	return sr, nil
}

func (ds *DataStorage) storageWrite(ctx context.Context, path string, metadata *structpb.Struct, content []byte, expectedVersion int64) (int64, error) {
	resp, err := ds.client.Send(ctx, &pluginv1.PluginRequest{
		RequestId: helpers.NewUUID(),
		Request: &pluginv1.PluginRequest_StorageWrite{
			StorageWrite: &pluginv1.StorageWriteRequest{
				Path:            path,
				Content:         content,
				Metadata:        metadata,
				ExpectedVersion: expectedVersion,
				StorageType:     "markdown",
			},
		},
	})
	if err != nil {
		return 0, err
	}
	sw := resp.GetStorageWrite()
	if sw == nil {
		return 0, fmt.Errorf("unexpected response type for storage write")
	}
	if !sw.Success {
		return 0, fmt.Errorf("storage write failed: %s", sw.Error)
	}
	return sw.NewVersion, nil
}

func (ds *DataStorage) storageDelete(ctx context.Context, path string) error {
	resp, err := ds.client.Send(ctx, &pluginv1.PluginRequest{
		RequestId: helpers.NewUUID(),
		Request: &pluginv1.PluginRequest_StorageDelete{
			StorageDelete: &pluginv1.StorageDeleteRequest{
				Path:        path,
				StorageType: "markdown",
			},
		},
	})
	if err != nil {
		return err
	}
	sd := resp.GetStorageDelete()
	if sd == nil {
		return fmt.Errorf("unexpected response type for storage delete")
	}
	if !sd.Success {
		return fmt.Errorf("storage delete failed")
	}
	return nil
}

func (ds *DataStorage) storageList(ctx context.Context, prefix, pattern string) ([]*pluginv1.StorageEntry, error) {
	resp, err := ds.client.Send(ctx, &pluginv1.PluginRequest{
		RequestId: helpers.NewUUID(),
		Request: &pluginv1.PluginRequest_StorageList{
			StorageList: &pluginv1.StorageListRequest{
				Prefix:      prefix,
				Pattern:     pattern,
				StorageType: "markdown",
			},
		},
	})
	if err != nil {
		return nil, err
	}
	sl := resp.GetStorageList()
	if sl == nil {
		return nil, fmt.Errorf("unexpected response type for storage list")
	}
	return sl.Entries, nil
}

// ---------- Path helpers ----------

func sessionPath(sessionID string) string {
	return fmt.Sprintf("bridge/sessions/%s.md", sessionID)
}

func turnFilePath(sessionID string, turnNumber int) string {
	return fmt.Sprintf("bridge/sessions/%s/turn-%03d.md", sessionID, turnNumber)
}

// ---------- Metadata conversion helpers ----------

func sessionToMetadata(s *Session) (*structpb.Struct, error) {
	m := map[string]any{
		"id":               s.ID,
		"account_id":       s.AccountID,
		"name":             s.Name,
		"workspace":        s.Workspace,
		"model":            s.Model,
		"permission_mode":  s.PermissionMode,
		"max_budget":       s.MaxBudget,
		"system_prompt":    s.SystemPrompt,
		"status":           s.Status,
		"created_at":       s.CreatedAt,
		"last_message_at":  s.LastMessageAt,
		"message_count":    float64(s.MessageCount),
		"total_tokens_in":  float64(s.TotalTokensIn),
		"total_tokens_out": float64(s.TotalTokensOut),
		"total_cost_usd":   s.TotalCostUSD,
	}
	if s.ClaudeSessionID != "" {
		m["claude_session_id"] = s.ClaudeSessionID
	}
	if len(s.AllowedTools) > 0 {
		tools := make([]any, len(s.AllowedTools))
		for i, t := range s.AllowedTools {
			tools[i] = t
		}
		m["allowed_tools"] = tools
	}
	return structpb.NewStruct(m)
}

func metadataToSession(meta *structpb.Struct) (*Session, error) {
	if meta == nil {
		return nil, fmt.Errorf("no metadata")
	}
	raw, err := json.Marshal(meta.AsMap())
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func turnToMetadata(t *Turn) (*structpb.Struct, error) {
	m := map[string]any{
		"number":      float64(t.Number),
		"user_prompt": t.UserPrompt,
		"tokens_in":   float64(t.TokensIn),
		"tokens_out":  float64(t.TokensOut),
		"cost_usd":    t.CostUSD,
		"model":       t.Model,
		"duration_ms": float64(t.DurationMs),
		"timestamp":   t.Timestamp,
	}
	if t.Events != "" {
		m["events"] = t.Events
	}
	return structpb.NewStruct(m)
}

func metadataToTurn(meta *structpb.Struct) (*Turn, error) {
	if meta == nil {
		return nil, fmt.Errorf("no metadata")
	}
	raw, err := json.Marshal(meta.AsMap())
	if err != nil {
		return nil, err
	}
	var t Turn
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
