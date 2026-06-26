package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLM is the optional tie-break adapter. It speaks two wire protocols behind one
// reRank call: the OpenAI-compatible chat API and the native Anthropic Messages
// API. Provider is "openai" or "anthropic".
type LLM struct {
	provider string
	baseURL  string
	key      string
	model    string
	http     *http.Client
}

func NewLLM(provider, baseURL, key, model string) *LLM {
	return &LLM{
		provider: provider,
		baseURL:  strings.TrimRight(baseURL, "/"),
		key:      key,
		model:    model,
		http:     &http.Client{Timeout: 90 * time.Second},
	}
}

// impactRubric is the shared impact-ranking guidance: used both in the --llm
// tie-break prompt and in the MCP server instructions (so a calling LLM like
// Claude Code can re-rank the deterministic order itself, no extra LLM call).
const impactRubric = "Rank by impact. Rules:\n" +
	"1. Foundational is STRUCTURAL, not semantic. An item is foundational only if its `blocks` list is non-empty (it literally unblocks " +
	"other issues) OR its DESCRIPTION states that other work depends on it. A title that merely sounds like infra/platform/framework/" +
	"'foundation' does NOT qualify — with no `blocks` entry and no such description, it is NOT foundational, however infra-y the wording. " +
	"The more it blocks, the higher.\n" +
	"2. An item with a non-empty `blocked_by` is gated: it cannot start until those clear. Rank it below comparable unblocked work and say 'blocked by X'.\n" +
	"3. Bug severity: classify each bug by blast radius and ORDER bugs by it — data-loss > broken-flow (a common path is broken) > " +
	"degraded (e.g. wrong/missing telemetry, non-blocking) > cosmetic. Judge from the DESCRIPTION, not the title; if the title does not " +
	"make the blast radius clear, request the description. The `why` MUST state the level, e.g. 'data-loss bug', 'broken-flow bug', " +
	"'degraded: telemetry only', 'cosmetic bug'. Do not order bugs by age.\n" +
	"4. Live momentum: ONLY status 'In Progress' or 'Selected for Development' with age_days < 60 is active committed work; rank it up. " +
	"A 'Backlog' item has NO momentum no matter how recent.\n" +
	"5. Zombie: an 'In Progress'/'Selected for Development' item older than 60 days is abandoned; rank it BELOW live work and BELOW real bugs. Say 'zombie: <status> <age>d'.\n" +
	"Collisions: data-loss/broken-flow bugs and strongly-foundational items (blocks 2+) are top tier alongside live work; degraded/cosmetic " +
	"bugs, weakly-foundational (blocks 1), and plain recent backlog are mid; everything else is 'minor backlog chore'.\n" +
	"For each item give the ONE dominant reason for its rank. If ranked low, say why it is low; do not list positive signals for a " +
	"low-ranked item. Never just restate the title.\n"

type rankItem struct {
	Key         string   `json:"key"`
	Summary     string   `json:"summary"`
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	AgeDays     int      `json:"age_days"`
	Blocks      []string `json:"blocks,omitempty"`     // keys this issue blocks
	BlockedBy   []string `json:"blocked_by,omitempty"` // keys blocking this issue
	Relates     []string `json:"relates,omitempty"`    // related keys
	Assigned    bool     `json:"assigned"`
	Description string   `json:"description,omitempty"` // only on the second pass
}

// reRank asks the model to reorder items by impact and return a rationale per item.
// First pass (withDesc=false) ranks on titles+links and may return need_descriptions:
// keys whose title is too ambiguous to judge. Second pass (withDesc=true) ranks with
// those descriptions attached to the items. Returns ordered keys, rationale map, and
// (first pass only) the keys the model wants descriptions for.
func (l *LLM) reRank(items []rankItem, withDesc bool) (order []string, why map[string]string, need []string, err error) {
	itemsJSON, _ := json.Marshal(items)
	sys := "You rank backlog items by impact for an engineering lead. Reorder ALL input items, most impactful first.\n" +
		impactRubric
	if withDesc {
		sys += "Some items carry a `description`; use it (not just the title) to judge severity and whether the work is foundational. " +
			"Return STRICT JSON only, no prose: {\"ranked\":[{\"key\":\"ABC-123\",\"why\":\"...\"}]}. Include EVERY input key EXACTLY once."
	} else {
		sys += "You have titles, links, and signals but NOT descriptions. Request descriptions (put keys in `need_descriptions`, max 15) " +
			"for any item whose rank depends on detail the title doesn't show: EVERY bug whose blast radius is not obvious from the title, " +
			"and any item you would otherwise call foundational on the title alone (you must confirm other work depends on it). " +
			"Then you will be re-asked with those descriptions. Return STRICT JSON only, no prose: " +
			"{\"ranked\":[{\"key\":\"ABC-123\",\"why\":\"...\"}],\"need_descriptions\":[\"ABC-123\"]}. " +
			"Include EVERY input key EXACTLY once in `ranked`."
	}

	content, err := l.complete(sys, "Items:\n"+string(itemsJSON))
	if err != nil {
		return nil, nil, nil, err
	}

	var parsed struct {
		Ranked []struct {
			Key string `json:"key"`
			Why string `json:"why"`
		} `json:"ranked"`
		NeedDescriptions []string `json:"need_descriptions"`
	}
	if err := json.Unmarshal([]byte(extractJSON(content)), &parsed); err != nil {
		return nil, nil, nil, fmt.Errorf("llm: parse ranked json: %w", err)
	}

	why = map[string]string{}
	for _, r := range parsed.Ranked {
		order = append(order, r.Key)
		why[r.Key] = r.Why
	}
	return order, why, parsed.NeedDescriptions, nil
}

// complete sends one system+user turn and returns the assistant's raw text.
func (l *LLM) complete(sys, user string) (string, error) {
	if l.provider == "anthropic" {
		return l.completeAnthropic(sys, user)
	}
	return l.completeOpenAI(sys, user)
}

// completeOpenAI uses the OpenAI-compatible /chat/completions API (Bearer auth).
func (l *LLM) completeOpenAI(sys, user string) (string, error) {
	body := map[string]any{
		"model":       l.model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": user},
		},
	}
	data, err := l.post(l.baseURL+"/chat/completions", body, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+l.key)
	})
	if err != nil {
		return "", err
	}

	var cc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &cc); err != nil || len(cc.Choices) == 0 {
		return "", fmt.Errorf("llm: unexpected response shape")
	}
	return cc.Choices[0].Message.Content, nil
}

// completeAnthropic uses the native Anthropic Messages API (/v1/messages,
// x-api-key + anthropic-version headers). No temperature: Opus 4.7+ reject it.
func (l *LLM) completeAnthropic(sys, user string) (string, error) {
	body := map[string]any{
		"model":      l.model,
		"max_tokens": 8192,
		"system":     sys,
		"messages": []map[string]string{
			{"role": "user", "content": user},
		},
	}
	data, err := l.post(l.baseURL+"/v1/messages", body, func(req *http.Request) {
		req.Header.Set("x-api-key", l.key)
		req.Header.Set("anthropic-version", "2023-06-01")
	})
	if err != nil {
		return "", err
	}

	var mr struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &mr); err != nil {
		return "", fmt.Errorf("llm: unexpected response shape")
	}
	for _, c := range mr.Content {
		if c.Type == "text" {
			return c.Text, nil
		}
	}
	return "", fmt.Errorf("llm: no text block in response")
}

// post marshals body, POSTs it as JSON with auth set by setAuth, and returns the
// response bytes. Errors on any non-2xx with a truncated body snippet.
func (l *LLM) post(url string, body any, setAuth func(*http.Request)) ([]byte, error) {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req)

	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s := string(data)
		if len(s) > 400 {
			s = s[:400]
		}
		return nil, fmt.Errorf("llm %s: %s", resp.Status, s)
	}
	return data, nil
}

// extractJSON pulls the first {...} object out of a possibly fenced/explained reply.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
