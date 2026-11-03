package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Jira is a minimal Jira Cloud client (the TicketProvider adapter, v0).
type Jira struct {
	baseURL string
	auth    string
	http    *http.Client
}

func NewJira(baseURL, email, token string) *Jira {
	return &Jira{
		baseURL: baseURL,
		auth:    "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token)),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type rawIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"` // ADF; only fetched when requested
		Updated     string          `json:"updated"`
		Status      struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
		Issuetype struct {
			Name    string `json:"name"`
			Subtask bool   `json:"subtask"`
		} `json:"issuetype"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
		Parent *struct {
			Key string `json:"key"`
		} `json:"parent"`
		Issuelinks []struct {
			Type struct {
				Name    string `json:"name"`
				Outward string `json:"outward"`
				Inward  string `json:"inward"`
			} `json:"type"`
			OutwardIssue *struct {
				Key string `json:"key"`
			} `json:"outwardIssue"`
			InwardIssue *struct {
				Key string `json:"key"`
			} `json:"inwardIssue"`
		} `json:"issuelinks"`
	} `json:"fields"`
}

type searchResp struct {
	Issues        []rawIssue `json:"issues"`
	NextPageToken string     `json:"nextPageToken"`
	IsLast        bool       `json:"isLast"`
}

// search runs a JQL query against the enhanced search endpoint, following pagination.
func (j *Jira) search(jql string, fields []string) ([]rawIssue, error) {
	return j.searchN(jql, fields, 0)
}

func (j *Jira) searchN(jql string, fields []string, limit int) ([]rawIssue, error) {
	var out []rawIssue
	token := ""
	for {
		page := 100
		if limit > 0 {
			page = min(page, limit-len(out))
		}
		body := map[string]any{
			"jql":        jql,
			"maxResults": page,
			"fields":     fields,
		}
		if token != "" {
			body["nextPageToken"] = token
		}
		buf, _ := json.Marshal(body)

		req, err := http.NewRequest(http.MethodPost, j.baseURL+"/rest/api/3/search/jql", bytes.NewReader(buf))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", j.auth)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := j.http.Do(req)
		if err != nil {
			return nil, err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			snippet := string(data)
			if len(snippet) > 400 {
				snippet = snippet[:400]
			}
			return nil, fmt.Errorf("jira %s: %s", resp.Status, snippet)
		}

		var sr searchResp
		if err := json.Unmarshal(data, &sr); err != nil {
			return nil, fmt.Errorf("decode search response: %w", err)
		}
		out = append(out, sr.Issues...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}

		if sr.NextPageToken == "" || sr.IsLast {
			break
		}
		token = sr.NextPageToken
	}
	return out, nil
}

// do is a generic JSON request against the Jira REST API. body may be nil.
func (j *Jira) do(method, path string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, j.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", j.auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := j.http.Do(req)
	if err != nil {
		return nil, err
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet := string(data)
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		return nil, fmt.Errorf("jira %s: %s", resp.Status, snippet)
	}
	return data, nil
}

// myAccountID returns the current user's accountId (for self-assign).
func (j *Jira) myAccountID() (string, error) {
	data, err := j.do(http.MethodGet, "/rest/api/3/myself", nil)
	if err != nil {
		return "", err
	}
	var m struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.AccountID == "" {
		return "", fmt.Errorf("jira: could not read accountId")
	}
	return m.AccountID, nil
}

// assign sets the issue's assignee to accountID.
func (j *Jira) assign(key, accountID string) error {
	_, err := j.do(http.MethodPut, "/rest/api/3/issue/"+key+"/assignee",
		map[string]any{"accountId": accountID})
	return err
}

// assignToMe assigns the issue to the authenticated user.
func (j *Jira) assignToMe(key string) error {
	me, err := j.myAccountID()
	if err != nil {
		return err
	}
	return j.assign(key, me)
}

// browseURL is the human-facing URL for an issue key.
func (j *Jira) browseURL(key string) string {
	return j.baseURL + "/browse/" + key
}

// Transition is a workflow transition available on an issue.
type Transition struct {
	ID         string
	Name       string
	To         string // target status name
	ToCategory string
}

// transitions lists the workflow transitions currently available on the issue.
func (j *Jira) transitions(key string) ([]Transition, error) {
	data, err := j.do(http.MethodGet, "/rest/api/3/issue/"+key+"/transitions", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name           string `json:"name"`
				StatusCategory struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode transitions: %w", err)
	}
	out := make([]Transition, 0, len(r.Transitions))
	for _, t := range r.Transitions {
		out = append(out, Transition{ID: t.ID, Name: t.Name, To: t.To.Name, ToCategory: t.To.StatusCategory.Key})
	}
	return out, nil
}

// doTransition moves the issue through the given transition id.
func (j *Jira) doTransition(key, transitionID string) error {
	_, err := j.do(http.MethodPost, "/rest/api/3/issue/"+key+"/transitions",
		map[string]any{"transition": map[string]string{"id": transitionID}})
	return err
}

type IssueRow struct {
	Key            string `json:"key"`
	Summary        string `json:"summary"`
	Type           string `json:"type"`
	Status         string `json:"status"`
	StatusCategory string `json:"statusCategory"`
	Updated        string `json:"updated"`
	URL            string `json:"url"`
	Parent         string `json:"parent,omitempty"`
}

func (j *Jira) searchIssues(jql string, limit int) ([]IssueRow, error) {
	raws, err := j.searchN(jql, []string{"summary", "status", "issuetype", "updated", "parent"}, limit)
	if err != nil {
		return nil, err
	}
	out := make([]IssueRow, 0, len(raws))
	for _, r := range raws {
		t, err := time.Parse("2006-01-02T15:04:05.000-0700", r.Fields.Updated)
		if err != nil {
			return nil, fmt.Errorf("%s: unparseable updated %q", r.Key, r.Fields.Updated)
		}
		row := IssueRow{
			Key:            r.Key,
			Summary:        r.Fields.Summary,
			Type:           r.Fields.Issuetype.Name,
			Status:         r.Fields.Status.Name,
			StatusCategory: r.Fields.Status.StatusCategory.Key,
			Updated:        t.Format(time.RFC3339),
			URL:            j.browseURL(r.Key),
		}
		if r.Fields.Parent != nil {
			row.Parent = r.Fields.Parent.Key
		}
		out = append(out, row)
	}
	return out, nil
}

type User struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress,omitempty"` // hidden by Jira for many users
	AccountType  string `json:"accountType"`
	Active       bool   `json:"active"`
}

func (j *Jira) findUsers(query string) ([]User, error) {
	data, err := j.do(http.MethodGet, "/rest/api/3/user/search?query="+url.QueryEscape(query)+"&maxResults=20", nil)
	if err != nil {
		return nil, err
	}
	out := []User{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode user search: %w", err)
	}
	return out, nil
}

// IssueDetail is an issue plus its (flattened, truncated) description.
type IssueDetail struct {
	Key         string `json:"key"`
	Summary     string `json:"summary"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description"`
	Format      string `json:"format,omitempty"`
}

// describe fetches the keys with their descriptions: ADF flattened to text and truncated, or, with
// markdown, the whole description rendered as markdown.
func (j *Jira) describe(keys []string, markdown bool) ([]IssueDetail, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	jql := "key in (" + strings.Join(keys, ",") + ")"
	raws, err := j.search(jql, []string{"summary", "description", "issuetype", "status"})
	if err != nil {
		return nil, err
	}
	out := make([]IssueDetail, 0, len(raws))
	for _, r := range raws {
		var b strings.Builder
		var node any
		if len(r.Fields.Description) > 0 {
			if err := json.Unmarshal(r.Fields.Description, &node); err != nil {
				return nil, fmt.Errorf("%s: decode description: %w", r.Key, err)
			}
		}
		d := IssueDetail{Key: r.Key, Summary: r.Fields.Summary, Type: r.Fields.Issuetype.Name, Status: r.Fields.Status.Name}
		if markdown {
			adfMarkdown(node, &b, "")
			d.Description, d.Format = strings.TrimSpace(b.String()), "markdown"
		} else {
			adfText(node, &b)
			d.Description = truncate(strings.TrimSpace(b.String()), 900)
		}
		out = append(out, d)
	}
	return out, nil
}

// adfText flattens an Atlassian Document Format tree into plain text.
func adfText(node any, b *strings.Builder) {
	switch n := node.(type) {
	case map[string]any:
		if t, ok := n["text"].(string); ok {
			b.WriteString(t)
		}
		if c, ok := n["content"].([]any); ok {
			for _, ch := range c {
				adfText(ch, b)
			}
		}
		switch n["type"] {
		case "paragraph", "heading", "listItem", "codeBlock", "blockquote":
			b.WriteString("\n")
		}
	case []any:
		for _, ch := range n {
			adfText(ch, b)
		}
	}
}

// adfMarkdown renders an Atlassian Document Format tree as markdown; indent prefixes nested list items.
func adfMarkdown(node any, b *strings.Builder, indent string) {
	switch n := node.(type) {
	case []any:
		for _, ch := range n {
			adfMarkdown(ch, b, indent)
		}
	case map[string]any:
		kids, _ := n["content"].([]any)
		attrs, _ := n["attrs"].(map[string]any)
		switch n["type"] {
		case "text":
			t, _ := n["text"].(string)
			b.WriteString(t)
		case "hardBreak":
			b.WriteString("\n")
		case "heading":
			level, _ := attrs["level"].(float64)
			b.WriteString("\n" + strings.Repeat("#", max(1, int(level))) + " ")
			adfMarkdown(kids, b, indent)
			b.WriteString("\n\n")
		case "paragraph":
			adfMarkdown(kids, b, indent)
			b.WriteString("\n\n")
		case "bulletList", "orderedList", "taskList":
			adfMarkdown(kids, b, indent)
			b.WriteString("\n")
		case "listItem":
			var item strings.Builder
			adfMarkdown(kids, &item, indent+"  ")
			b.WriteString(indent + "- " + strings.TrimSpace(item.String()) + "\n")
		case "taskItem":
			box := "[ ]"
			if attrs["state"] == "DONE" {
				box = "[x]"
			}
			var item strings.Builder
			adfMarkdown(kids, &item, indent+"  ")
			b.WriteString(indent + "- " + box + " " + strings.TrimSpace(item.String()) + "\n")
		case "codeBlock":
			b.WriteString("```\n")
			adfMarkdown(kids, b, indent)
			b.WriteString("\n```\n\n")
		case "blockquote":
			var q strings.Builder
			adfMarkdown(kids, &q, indent)
			for _, l := range strings.Split(strings.TrimSpace(q.String()), "\n") {
				b.WriteString("> " + l + "\n")
			}
			b.WriteString("\n")
		case "tableRow":
			cells := make([]string, 0, len(kids))
			for _, c := range kids {
				var cell strings.Builder
				adfMarkdown(c, &cell, indent)
				cells = append(cells, strings.ReplaceAll(strings.TrimSpace(cell.String()), "\n", " "))
			}
			b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		case "table":
			adfMarkdown(kids, b, indent)
			b.WriteString("\n")
		default:
			adfMarkdown(kids, b, indent)
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// addComment posts a plain-text comment, wrapped in the ADF doc the v3 API requires.
func (j *Jira) addComment(key, text string) error {
	body := map[string]any{
		"body": map[string]any{
			"type":    "doc",
			"version": 1,
			"content": []any{
				map[string]any{
					"type": "paragraph",
					"content": []any{
						map[string]any{"type": "text", "text": text},
					},
				},
			},
		},
	}
	_, err := j.do(http.MethodPost, "/rest/api/3/issue/"+key+"/comment", body)
	return err
}
