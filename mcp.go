package main

// Minimal MCP server over stdio: newline-delimited JSON-RPC 2.0. Exposes thresh's
// discovery+ranking and the same per-task mutations as the TUI, so Claude Code
// (or any MCP client) can call them as tools. Hand-rolled to avoid an SDK
// dependency; only the initialize / tools.list / tools.call subset is needed.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const mcpProtocolVersion = "2025-06-18"

// mcpInstructions is a short orientation. The impact rubric is NOT here: it
// rides on the discover_backlog result, and only when that call did not use the
// `llm` option (i.e. only when the order still needs a semantic pass).
const mcpInstructions = "thresh discovers and ranks the pending, actionable Jira issues for a scope. " +
	"Typical flow: call discover_backlog with a scope (e.g. ABC-123), re-rank, then act via " +
	"assign_issue / list_transitions / transition_issue / comment_issue. " +
	"discover_backlog returns each issue with raw signals (status, ageDays, type, score) and its " +
	"dependency links (blocks / blockedBy / relates). Called without `llm` it returns the cheap " +
	"deterministic order and a rubric asking you to re-rank by impact in context. " +
	"The signals are title-level: when a title is too ambiguous to judge severity or whether the work " +
	"is foundational, call describe_issues for those few keys to read their descriptions, then finalize. " +
	"The `llm` option makes thresh run the whole re-rank itself via its own model and exists only for standalone CLI use."

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpServer struct {
	prov       TicketProvider
	out        *json.Encoder
	disableLLM bool            // ignore the discover_backlog `llm` argument
	allowed    map[string]bool // nil = all tools enabled
}

// runMCP builds the Jira client from env/config and serves MCP on stdin/stdout.
// disableLLM forces the deterministic+rubric path; tools is an optional
// comma-separated allowlist of tool names to expose (empty = all).
func runMCP(disableLLM bool, tools string) error {
	base, email, token := resolveJira()
	if base == "" || email == "" || token == "" {
		return fmt.Errorf("missing Jira config (set THRESH_JIRA_BASE_URL/THRESH_JIRA_EMAIL/THRESH_JIRA_TOKEN or run `thresh setup`)")
	}
	var allowed map[string]bool
	if strings.TrimSpace(tools) != "" {
		allowed = map[string]bool{}
		for _, t := range strings.Split(tools, ",") {
			if t = strings.TrimSpace(t); t != "" {
				allowed[t] = true
			}
		}
	}
	s := &mcpServer{prov: NewJira(base, email, token), disableLLM: disableLLM, allowed: allowed}
	return s.serve(os.Stdin, os.Stdout)
}

func (s *mcpServer) serve(in io.Reader, out io.Writer) error {
	s.out = json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue // malformed frame; nothing to reply to
		}
		s.handle(req)
	}
	return sc.Err()
}

func (s *mcpServer) handle(req rpcRequest) {
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		s.reply(req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "thresh", "version": "0.1.0"},
			"instructions":    mcpInstructions,
		})
	case "notifications/initialized", "notifications/cancelled":
		// no response
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": s.visibleTools()})
	case "tools/call":
		s.handleToolCall(req)
	case "ping":
		s.reply(req.ID, map[string]any{})
	default:
		if !isNotification {
			s.replyError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *mcpServer) reply(id json.RawMessage, result any) {
	if len(id) == 0 {
		return
	}
	s.out.Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *mcpServer) replyError(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		return
	}
	s.out.Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// toolText returns a tools/call result carrying a single text block.
func toolText(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// visibleTools returns the advertised tools, filtered by the allowlist if set.
func (s *mcpServer) visibleTools() []any {
	all := toolSchemas()
	if s.allowed == nil {
		return all
	}
	var out []any
	for _, t := range all {
		if m, ok := t.(map[string]any); ok {
			if name, _ := m["name"].(string); s.allowed[name] {
				out = append(out, t)
			}
		}
	}
	return out
}

func toolSchemas() []any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []any{
		map[string]any{
			"name":        "discover_backlog",
			"description": "Discover and rank pending Jira issues, dropping done ones and items that are parents of others in the set, and return them with per-issue signals (status, ageDays, blocks, type, score). Provide either `scope` (an issue key like ABC-123 to walk the hierarchy below it, or a project key like ABC for the whole project) or `jql` (a raw JQL query, e.g. \"project = ABC AND assignee = currentUser()\"). Without `llm` (the normal case) it returns the deterministic order and a rubric asking you to re-rank by impact yourself. The `llm` option makes thresh run that re-rank via its own model and is only for standalone CLI use.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope": str("issue key (ABC-123: hierarchy below it) or project key (ABC: whole project)"),
					"jql":   str("raw JQL query; use instead of scope for project+assignee, sprint, board filters, etc."),
					"llm":   map[string]any{"type": "boolean", "description": "re-rank the top items via the configured LLM tie-break"},
					"top":   map[string]any{"type": "integer", "description": "how many top items the LLM tie-break re-ranks (default 30)"},
				},
			},
		},
		map[string]any{
			"name":        "describe_issues",
			"description": "Fetch the full (truncated) descriptions of specific issues, to judge severity or whether work is foundational when the title alone is ambiguous. Call this for the handful of discover_backlog items you are unsure about, then finalize your ranking.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "issue keys, e.g. [\"ABC-123\",\"ABC-456\"]"}},
				"required":   []any{"keys"},
			},
		},
		map[string]any{
			"name":        "assign_issue",
			"description": "Assign a Jira issue to the current user (the THRESH_JIRA_EMAIL identity).",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"key": str("issue key, e.g. ABC-123")},
				"required":   []any{"key"},
			},
		},
		map[string]any{
			"name":        "list_transitions",
			"description": "List the workflow transitions currently available on a Jira issue (id, name, target status).",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"key": str("issue key")},
				"required":   []any{"key"},
			},
		},
		map[string]any{
			"name":        "transition_issue",
			"description": "Move a Jira issue through a workflow transition. Use list_transitions first to get the transition id.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":          str("issue key"),
					"transitionId": str("transition id from list_transitions"),
				},
				"required": []any{"key", "transitionId"},
			},
		},
		map[string]any{
			"name":        "comment_issue",
			"description": "Add a plain-text comment to a Jira issue.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":  str("issue key"),
					"text": str("comment body (plain text)"),
				},
				"required": []any{"key", "text"},
			},
		},
	}
}

func (s *mcpServer) handleToolCall(req rpcRequest) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.replyError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}

	result, err := s.dispatchTool(p.Name, p.Arguments)
	if err != nil {
		// Tool errors are reported in-band (isError), not as protocol errors,
		// so the model sees and can react to them.
		s.reply(req.ID, toolText(err.Error(), true))
		return
	}
	s.reply(req.ID, result)
}

func (s *mcpServer) dispatchTool(name string, args json.RawMessage) (map[string]any, error) {
	if s.allowed != nil && !s.allowed[name] {
		return nil, fmt.Errorf("tool %q is not enabled on this server", name)
	}
	switch name {
	case "discover_backlog":
		var a struct {
			Scope string `json:"scope"`
			JQL   string `json:"jql"`
			LLM   bool   `json:"llm"`
			Top   int    `json:"top"`
		}
		json.Unmarshal(args, &a)
		if a.Scope == "" && a.JQL == "" {
			return nil, fmt.Errorf("provide scope or jql")
		}
		if a.Top == 0 {
			a.Top = 30
		}
		all, isParent, err := s.prov.discover(a.Scope, a.JQL)
		if err != nil {
			return nil, err
		}
		pending := rankPending(all, isParent)
		out, _ := json.MarshalIndent(rankedRows(s.prov, pending), "", "  ")
		if a.LLM && !s.disableLLM {
			// thresh runs its own semantic pass and returns that order as-is.
			p, err := llmReRank(s.prov, pending, a.Top)
			if err != nil {
				return nil, err
			}
			out, _ = json.MarshalIndent(rankedRows(s.prov, p), "", "  ")
			return toolText(string(out), false), nil
		}
		// Deterministic order: ask the calling model to re-rank, rubric attached.
		text := "These issues are in DETERMINISTIC order; the score cannot judge semantics. " +
			"Re-rank them yourself by impact using this rubric, then present your order:\n\n" +
			impactRubric + "\n" + string(out)
		return toolText(text, false), nil

	case "describe_issues":
		var a struct {
			Keys []string `json:"keys"`
		}
		json.Unmarshal(args, &a)
		if len(a.Keys) == 0 {
			return nil, fmt.Errorf("keys is required")
		}
		details, err := s.prov.describe(a.Keys)
		if err != nil {
			return nil, err
		}
		out, _ := json.MarshalIndent(details, "", "  ")
		return toolText(string(out), false), nil

	case "assign_issue":
		key, err := argKey(args)
		if err != nil {
			return nil, err
		}
		if err := s.prov.assignToMe(key); err != nil {
			return nil, err
		}
		return toolText("assigned "+key+" to you", false), nil

	case "list_transitions":
		key, err := argKey(args)
		if err != nil {
			return nil, err
		}
		ts, err := s.prov.transitions(key)
		if err != nil {
			return nil, err
		}
		out, _ := json.MarshalIndent(ts, "", "  ")
		return toolText(string(out), false), nil

	case "transition_issue":
		var a struct {
			Key          string `json:"key"`
			TransitionID string `json:"transitionId"`
		}
		json.Unmarshal(args, &a)
		if a.Key == "" || a.TransitionID == "" {
			return nil, fmt.Errorf("key and transitionId are required")
		}
		if err := s.prov.doTransition(a.Key, a.TransitionID); err != nil {
			return nil, err
		}
		return toolText("transitioned "+a.Key, false), nil

	case "comment_issue":
		var a struct {
			Key  string `json:"key"`
			Text string `json:"text"`
		}
		json.Unmarshal(args, &a)
		if a.Key == "" || a.Text == "" {
			return nil, fmt.Errorf("key and text are required")
		}
		if err := s.prov.addComment(a.Key, a.Text); err != nil {
			return nil, err
		}
		return toolText("commented on "+a.Key, false), nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func argKey(args json.RawMessage) (string, error) {
	var a struct {
		Key string `json:"key"`
	}
	json.Unmarshal(args, &a)
	if a.Key == "" {
		return "", fmt.Errorf("key is required")
	}
	return a.Key, nil
}

// rankedRow is the structured shape returned by discover_backlog. The link fields
// (blocks/blockedBy/relates) let the caller reason about dependencies and decide
// which issues to pull descriptions for via describe_issues.
type rankedRow struct {
	Rank      int      `json:"rank"`
	Key       string   `json:"key"`
	Summary   string   `json:"summary"`
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	Assignee  string   `json:"assignee,omitempty"`
	AgeDays   int      `json:"ageDays"`
	Blocks    []string `json:"blocks,omitempty"`
	BlockedBy []string `json:"blockedBy,omitempty"`
	Relates   []string `json:"relates,omitempty"`
	Stale     bool     `json:"stale"`
	Score     float64  `json:"score"`
	Why       string   `json:"why,omitempty"`
	URL       string   `json:"url"`
}

func rankedRows(prov TicketProvider, items []Issue) []rankedRow {
	rows := make([]rankedRow, len(items))
	for i, iss := range items {
		d := ageDays(iss.Updated)
		rows[i] = rankedRow{
			Rank:      i + 1,
			Key:       iss.Key,
			Summary:   iss.Summary,
			Type:      iss.Type,
			Status:    iss.Status,
			Assignee:  iss.Assignee,
			AgeDays:   d,
			Blocks:    iss.Blocks,
			BlockedBy: iss.BlockedBy,
			Relates:   iss.Relates,
			Stale:     d >= 30,
			Score:     iss.Score,
			Why:       iss.Why,
			URL:       prov.browseURL(iss.Key),
		}
	}
	return rows
}
