package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	pluginv1 "github.com/orchestra-mcp/gen-go/orchestra/plugin/v1"
	"github.com/orchestra-mcp/sdk-go/helpers"
	"github.com/orchestra-mcp/plugin-tools-sessions/internal/storage"
	"google.golang.org/protobuf/types/known/structpb"
)

// SendMessageSchema returns the JSON Schema for the send_message tool.
func SendMessageSchema() *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "Session ID to send the message to",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "Message to send to Claude Code",
			},
			"permission_mode": map[string]any{
				"type":        "string",
				"description": "Override the session's permission mode for this message (e.g., default, plan, bypassPermissions)",
			},
			"model": map[string]any{
				"type":        "string",
				"description": "Override the model for this message (e.g., claude-sonnet-4-20250514, claude-opus-4-6)",
			},
		},
		"required": []any{"session_id", "message"},
	})
	return s
}

// SendMessage implements the critical path for sending a message to a session.
//
// The flow is:
//  1. Read session metadata from storage
//  2. Call agentops get_account_env to get env vars for the account
//  3. Call agentops check_budget to verify budget is ok
//  4. Call bridge spawn_session with session config and env
//  5. Store turn as a new turn file
//  6. Call agentops report_usage with token counts
//  7. Update session metadata (message_count, last_message_at, totals)
//  8. Return the AI response
func SendMessage(store *storage.DataStorage) ToolHandler {
	return func(ctx context.Context, req *pluginv1.ToolRequest) (*pluginv1.ToolResponse, error) {
		if err := helpers.ValidateRequired(req.Arguments, "session_id", "message"); err != nil {
			return helpers.ErrorResult("validation_error", err.Error()), nil
		}

		sessionID := helpers.GetString(req.Arguments, "session_id")
		message := helpers.GetString(req.Arguments, "message")
		permissionModeOverride := helpers.GetString(req.Arguments, "permission_mode")
		modelOverride := helpers.GetString(req.Arguments, "model")

		// Step 1: Read session metadata.
		session, version, err := store.ReadSession(ctx, sessionID)
		if err != nil {
			return helpers.ErrorResult("not_found", fmt.Sprintf("session %q not found", sessionID)), nil
		}

		if session.Status == "completed" {
			return helpers.ErrorResult("invalid_state", "cannot send messages to a completed session"), nil
		}

		// Reactivate paused sessions when a new message arrives.
		if session.Status == "paused" {
			session.Status = "active"
		}

		// Step 2: Get account environment variables and provider from agentops.
		provider, envMap, err := getAccountEnvWithProvider(ctx, store, session.AccountID)
		if err != nil {
			return helpers.ErrorResult("agentops_error",
				fmt.Sprintf("failed to get account env: %v", err)), nil
		}

		// Step 3: Check budget via agentops.
		if session.MaxBudget > 0 {
			budgetOK, err := checkBudget(ctx, store, session.AccountID, sessionID, session.MaxBudget, session.TotalCostUSD)
			if err != nil {
				return helpers.ErrorResult("agentops_error",
					fmt.Sprintf("failed to check budget: %v", err)), nil
			}
			if !budgetOK {
				return helpers.ErrorResult("budget_exceeded",
					fmt.Sprintf("session budget exceeded: $%.2f / $%.2f",
						session.TotalCostUSD, session.MaxBudget)), nil
			}
		}

		// Allow per-message permission_mode override so the frontend can
		// switch between auto/manual without recreating the session.
		if permissionModeOverride != "" {
			session.PermissionMode = permissionModeOverride
		}

		// Allow per-message model override so the frontend can switch models
		// without recreating the session.
		if modelOverride != "" {
			session.Model = modelOverride
		}

		// Step 4: Call bridge spawn_session.
		startTime := time.Now()
		// Resume only when we have a confirmed Claude session ID from a
		// previous *successful* response. ClaudeSessionID is set only after
		// a response containing a valid session ID is received.
		// MessageCount alone is not reliable because it gets incremented
		// even on failed turns (empty responses, process errors).
		resume := session.ClaudeSessionID != ""

		// Prepend system prompt to the first message if configured.
		prompt := message
		if session.SystemPrompt != "" && !resume {
			prompt = session.SystemPrompt + "\n\n" + message
		}

		spawnResp, err := spawnSession(ctx, store, session, prompt, envMap, resume, provider)
		if err != nil {
			return helpers.ErrorResult("bridge_error",
				fmt.Sprintf("failed to spawn session: %v", err)), nil
		}
		durationMs := time.Since(startTime).Milliseconds()

		// Extract response data from the bridge response.
		// New bridge format returns JSON in the "text" field with all metadata.
		br := parseBridgeResponse(spawnResp)
		responseText := br.Response
		tokensIn := br.TokensIn
		tokensOut := br.TokensOut
		costUSD := br.CostUSD
		model := br.Model
		if model == "" {
			model = session.Model
		}
		// Capture the actual claude session ID from a successful response.
		// This is what we use for --resume on subsequent turns.
		if br.SessionID != "" {
			session.ClaudeSessionID = br.SessionID
		} else if claudeID := extractClaudeSessionID(spawnResp, responseText); claudeID != "" {
			session.ClaudeSessionID = claudeID
		}

		// Step 5: Store turn file.
		now := helpers.NowISO()
		turnNumber := session.MessageCount + 1
		turn := &storage.Turn{
			Number:     turnNumber,
			UserPrompt: message,
			Response:   responseText,
			TokensIn:   tokensIn,
			TokensOut:  tokensOut,
			CostUSD:    costUSD,
			Model:      model,
			DurationMs: durationMs,
			Timestamp:  now,
			Events:     br.ToolEvents,
		}

		_, err = store.WriteTurn(ctx, sessionID, turn)
		if err != nil {
			// Non-fatal: log but continue updating session.
			_ = err
		}

		// Step 6: Report usage to agentops.
		_ = reportUsage(ctx, store, session.AccountID, sessionID, tokensIn, tokensOut, costUSD, model)

		// Step 7: Update session metadata.
		session.MessageCount = turnNumber
		session.LastMessageAt = now
		session.TotalTokensIn += tokensIn
		session.TotalTokensOut += tokensOut
		session.TotalCostUSD += costUSD

		_, err = store.WriteSession(ctx, session, version)
		if err != nil {
			// Non-fatal for the user: they still get the response.
			_ = err
		}

		// Step 8: Return the AI response as structured JSON.
		resp, err := helpers.JSONResult(map[string]any{
			"response":    turn.Response,
			"session_id":  session.ClaudeSessionID,
			"model":       turn.Model,
			"tokens_in":   turn.TokensIn,
			"tokens_out":  turn.TokensOut,
			"cost":        turn.CostUSD,
			"duration_ms": turn.DurationMs,
		})
		if err != nil {
			// Fallback: return just the response text.
			return helpers.TextResult(turn.Response), nil
		}
		return resp, nil
	}
}

// ---------- Cross-plugin call helpers ----------

// getAccountEnvWithProvider calls tools.agentops get_account_env to retrieve
// the provider name and environment variables for the given account.
func getAccountEnvWithProvider(ctx context.Context, store *storage.DataStorage, accountID string) (string, map[string]any, error) {
	resp, err := store.CallTool(ctx, "get_account_env", map[string]any{
		"account_id": accountID,
	})
	if err != nil {
		return "", nil, err
	}
	if !resp.Success {
		return "", nil, fmt.Errorf("%s: %s", resp.ErrorCode, resp.ErrorMessage)
	}
	if resp.Result == nil {
		return "claude", map[string]any{}, nil
	}

	result := resp.Result.AsMap()

	// The response is now {"provider": "...", "env": {...}}.
	provider := "claude"
	if p, ok := result["provider"].(string); ok && p != "" {
		provider = p
	}
	envMap := map[string]any{}
	if e, ok := result["env"].(map[string]any); ok {
		envMap = e
	}

	return provider, envMap, nil
}

// checkBudget calls tools.agentops check_budget to verify that sending another
// message is within the budget for this session.
func checkBudget(ctx context.Context, store *storage.DataStorage, accountID, sessionID string, maxBudget, currentSpend float64) (bool, error) {
	resp, err := store.CallTool(ctx, "check_budget", map[string]any{
		"account_id":    accountID,
		"session_id":    sessionID,
		"max_budget":    maxBudget,
		"current_spend": currentSpend,
	})
	if err != nil {
		return false, err
	}
	if !resp.Success {
		return false, fmt.Errorf("%s: %s", resp.ErrorCode, resp.ErrorMessage)
	}
	// The response result should contain an "allowed" boolean.
	if resp.Result != nil {
		allowed, ok := resp.Result.Fields["allowed"]
		if ok {
			if bv, ok := allowed.Kind.(*structpb.Value_BoolValue); ok {
				return bv.BoolValue, nil
			}
		}
	}
	// If no explicit "allowed" field, treat success as allowed.
	return true, nil
}

// spawnSession calls the appropriate bridge plugin's spawn_session tool to run
// the message through the AI provider. The provider parameter determines which
// bridge plugin receives the call (e.g., "claude" -> bridge.claude, "openai" ->
// bridge.openai, etc.).
func spawnSession(ctx context.Context, store *storage.DataStorage, session *storage.Session, prompt string, envMap map[string]any, resume bool, provider string) (*pluginv1.ToolResponse, error) {
	// Serialize env map to JSON string for the bridge.
	envJSON, err := json.Marshal(envMap)
	if err != nil {
		envJSON = []byte("{}")
	}

	// When resuming, use the actual claude session ID (not our Orchestra ID).
	sessionIDForBridge := session.ID
	if resume && session.ClaudeSessionID != "" {
		sessionIDForBridge = session.ClaudeSessionID
	}

	args := map[string]any{
		"session_id": sessionIDForBridge,
		"prompt":     prompt,
		"env":        string(envJSON),
		"resume":     resume,
		"model":      session.Model,
		"wait":       true,
	}
	if session.Workspace != "" {
		args["workspace"] = session.Workspace
	}
	if session.PermissionMode != "" {
		args["permission_mode"] = session.PermissionMode
	}
	if len(session.AllowedTools) > 0 {
		toolsJSON, err := json.Marshal(session.AllowedTools)
		if err == nil {
			args["allowed_tools"] = string(toolsJSON)
		}
	}

	return store.CallToolWithProvider(ctx, "spawn_session", args, provider)
}

// reportUsage calls tools.agentops report_usage to record token consumption.
func reportUsage(ctx context.Context, store *storage.DataStorage, accountID, sessionID string, tokensIn, tokensOut int64, costUSD float64, model string) error {
	_, err := store.CallTool(ctx, "report_usage", map[string]any{
		"account_id": accountID,
		"session_id": sessionID,
		"tokens_in":  float64(tokensIn),
		"tokens_out": float64(tokensOut),
		"cost_usd":   costUSD,
		"model":      model,
	})
	return err
}

// ---------- Response extraction helpers ----------

// bridgeResponse holds the parsed fields from a bridge spawn_session response.
type bridgeResponse struct {
	Response   string
	SessionID  string
	Model      string
	TokensIn   int64
	TokensOut  int64
	CostUSD    float64
	DurationMs int64
	ToolEvents string // JSON array of tool events (stored as-is)
}

// parseBridgeResponse extracts all fields from the bridge response.
// New bridge format: the "text" field contains a JSON string with structured data.
// Falls back to individual field extraction for old bridge format.
func parseBridgeResponse(resp *pluginv1.ToolResponse) bridgeResponse {
	var br bridgeResponse
	if resp == nil || resp.Result == nil {
		return br
	}

	// Try new JSON format in "text" field.
	if v, ok := resp.Result.Fields["text"]; ok {
		if sv, ok := v.Kind.(*structpb.Value_StringValue); ok {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(sv.StringValue), &parsed); err == nil {
				if s, ok := parsed["response"].(string); ok {
					br.Response = s
				}
				if s, ok := parsed["session_id"].(string); ok {
					br.SessionID = s
				}
				if s, ok := parsed["model"].(string); ok {
					br.Model = s
				}
				if n, ok := parsed["tokens_in"].(float64); ok {
					br.TokensIn = int64(n)
				}
				if n, ok := parsed["tokens_out"].(float64); ok {
					br.TokensOut = int64(n)
				}
				if n, ok := parsed["cost_usd"].(float64); ok {
					br.CostUSD = n
				}
				if n, ok := parsed["duration_ms"].(float64); ok {
					br.DurationMs = int64(n)
				}
				// Extract tool_events as a raw JSON string for storage.
				if evts, ok := parsed["tool_events"]; ok && evts != nil {
					if raw, err := json.Marshal(evts); err == nil {
						br.ToolEvents = string(raw)
					}
				}
				return br
			}
		}
	}

	// Fallback: old bridge format with individual fields.
	br.Response = extractText(resp)
	br.SessionID = extractString(resp, "session_id")
	br.Model = extractString(resp, "model")
	br.TokensIn = extractInt64(resp, "tokens_in")
	br.TokensOut = extractInt64(resp, "tokens_out")
	br.CostUSD = extractFloat64(resp, "cost_usd")
	br.DurationMs = extractInt64(resp, "duration_ms")
	return br
}

func extractText(resp *pluginv1.ToolResponse) string {
	if resp == nil || resp.Result == nil {
		return ""
	}
	// Try "text" field first (standard MCP response).
	if v, ok := resp.Result.Fields["text"]; ok {
		if sv, ok := v.Kind.(*structpb.Value_StringValue); ok {
			text := sv.StringValue
			// New bridge format: JSON with structured fields.
			// Parse and return only the "response" field (clean text).
			var parsed map[string]any
			if err := json.Unmarshal([]byte(text), &parsed); err == nil {
				if response, ok := parsed["response"].(string); ok {
					return response
				}
			}
			return text
		}
	}
	// Try "response" field.
	if v, ok := resp.Result.Fields["response"]; ok {
		if sv, ok := v.Kind.(*structpb.Value_StringValue); ok {
			return sv.StringValue
		}
	}
	// Try "result" field.
	if v, ok := resp.Result.Fields["result"]; ok {
		if sv, ok := v.Kind.(*structpb.Value_StringValue); ok {
			return sv.StringValue
		}
	}
	return ""
}

func extractString(resp *pluginv1.ToolResponse, key string) string {
	if resp == nil || resp.Result == nil {
		return ""
	}
	v, ok := resp.Result.Fields[key]
	if !ok || v == nil {
		return ""
	}
	sv, ok := v.Kind.(*structpb.Value_StringValue)
	if !ok {
		return ""
	}
	return sv.StringValue
}

func extractInt64(resp *pluginv1.ToolResponse, key string) int64 {
	if resp == nil || resp.Result == nil {
		return 0
	}
	v, ok := resp.Result.Fields[key]
	if !ok || v == nil {
		return 0
	}
	nv, ok := v.Kind.(*structpb.Value_NumberValue)
	if !ok {
		return 0
	}
	return int64(nv.NumberValue)
}

func extractFloat64(resp *pluginv1.ToolResponse, key string) float64 {
	if resp == nil || resp.Result == nil {
		return 0
	}
	v, ok := resp.Result.Fields[key]
	if !ok || v == nil {
		return 0
	}
	nv, ok := v.Kind.(*structpb.Value_NumberValue)
	if !ok {
		return 0
	}
	return nv.NumberValue
}

// extractClaudeSessionID extracts the actual claude session UUID from the
// bridge response. New bridge format returns structured JSON in the "text"
// field with a "session_id" key. Falls back to parsing markdown for old format.
func extractClaudeSessionID(resp *pluginv1.ToolResponse, responseText string) string {
	// Try structured JSON first (new bridge format).
	if resp != nil && resp.Result != nil {
		if v, ok := resp.Result.Fields["text"]; ok {
			if sv, ok := v.Kind.(*structpb.Value_StringValue); ok {
				var parsed map[string]any
				if err := json.Unmarshal([]byte(sv.StringValue), &parsed); err == nil {
					if sid, ok := parsed["session_id"].(string); ok && len(sid) == 36 && strings.Count(sid, "-") == 4 {
						return sid
					}
				}
			}
		}
	}
	// Fallback: parse "- **Session:** <uuid>" from old markdown format.
	for _, line := range strings.Split(responseText, "\n") {
		line = strings.TrimSpace(line)
		const prefix = "- **Session:** "
		if strings.HasPrefix(line, prefix) {
			id := strings.TrimPrefix(line, prefix)
			id = strings.TrimSpace(id)
			if len(id) == 36 && strings.Count(id, "-") == 4 {
				return id
			}
		}
	}
	return ""
}

