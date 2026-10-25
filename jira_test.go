package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, h http.HandlerFunc) *mcpServer {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &mcpServer{prov: NewJira(srv.URL, "me@example.com", "tok")}
}

func callText(t *testing.T, s *mcpServer, name, args string) (string, error) {
	t.Helper()
	res, err := s.dispatchTool(name, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	if res["isError"] == true {
		t.Fatalf("%s: isError result: %v", name, res)
	}
	return res["content"].([]any)[0].(map[string]any)["text"].(string), nil
}

func searchIssue(key, cat, updated, parent string) map[string]any {
	f := map[string]any{
		"summary":   "summary " + key,
		"updated":   updated,
		"status":    map[string]any{"name": "Status " + cat, "statusCategory": map[string]any{"key": cat}},
		"issuetype": map[string]any{"name": "Story"},
	}
	if parent != "" {
		f["parent"] = map[string]any{"key": parent}
	}
	return map[string]any{"key": key, "fields": f}
}

// pagedSearch serves issues in pages of pageSize, honouring the request's maxResults.
func pagedSearch(t *testing.T, issues []map[string]any, pageSize int, requests *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		*requests++
		var body struct {
			JQL           string   `json:"jql"`
			MaxResults    int      `json:"maxResults"`
			Fields        []string `json:"fields"`
			NextPageToken string   `json:"nextPageToken"`
		}
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		for _, want := range []string{"summary", "status", "issuetype", "updated", "parent"} {
			if !strings.Contains(strings.Join(body.Fields, ","), want) {
				t.Errorf("fields %v lack %s", body.Fields, want)
			}
		}
		start := 0
		if body.NextPageToken != "" {
			fmt.Sscanf(body.NextPageToken, "p%d", &start)
		}
		n := min(pageSize, body.MaxResults)
		end := min(start+n, len(issues))
		resp := map[string]any{"issues": issues[start:end]}
		if end < len(issues) {
			resp["nextPageToken"] = fmt.Sprintf("p%d", end)
		} else {
			resp["isLast"] = true
		}
		json.NewEncoder(w).Encode(resp)
	}
}

type issueRowJSON struct {
	Key            string  `json:"key"`
	Summary        string  `json:"summary"`
	Type           string  `json:"type"`
	Status         string  `json:"status"`
	StatusCategory string  `json:"statusCategory"`
	Updated        string  `json:"updated"`
	URL            string  `json:"url"`
	Parent         *string `json:"parent"`
}

func TestSearchIssuesPaginatesAndKeepsDoneAndParents(t *testing.T) {
	issues := []map[string]any{
		searchIssue("A-1", "new", "2026-10-23T03:47:21.000-0700", ""),
		searchIssue("A-2", "done", "2026-10-22T10:00:00.000+0200", "A-1"),
		searchIssue("A-3", "indeterminate", "2026-10-21T00:00:00.000+0000", ""),
	}
	reqs := 0
	s := newTestServer(t, pagedSearch(t, issues, 2, &reqs))
	text, err := callText(t, s, "search_issues", `{"jql":"project = A"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "[") || !strings.HasSuffix(text, "]") {
		t.Fatalf("text is not a bare JSON array: %q", text)
	}
	var rows []issueRowJSON
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatal(err)
	}
	if reqs != 2 || len(rows) != 3 {
		t.Fatalf("requests=%d rows=%d, want 2 and 3", reqs, len(rows))
	}
	done := rows[1]
	if done.Key != "A-2" || done.StatusCategory != "done" || done.Status != "Status done" || done.Type != "Story" {
		t.Fatalf("done row: %+v", done)
	}
	if done.Parent == nil || *done.Parent != "A-1" {
		t.Fatalf("parent: %v", done.Parent)
	}
	if rows[0].Parent != nil || strings.Contains(text[:strings.Index(text, "A-2")], `"parent"`) {
		t.Fatalf("parent should be omitted on A-1: %s", text)
	}
	if rows[0].Updated != "2026-10-23T03:47:21-07:00" {
		t.Fatalf("updated = %q", rows[0].Updated)
	}
	if !strings.HasSuffix(rows[0].URL, "/browse/A-1") {
		t.Fatalf("url = %q", rows[0].URL)
	}
}

func TestSearchIssuesHonoursLimit(t *testing.T) {
	var issues []map[string]any
	for i := range 5 {
		issues = append(issues, searchIssue(fmt.Sprintf("A-%d", i+1), "new", "2026-10-23T03:47:21.000-0700", ""))
	}
	reqs := 0
	s := newTestServer(t, pagedSearch(t, issues, 2, &reqs))
	text, err := callText(t, s, "search_issues", `{"jql":"project = A","limit":3}`)
	if err != nil {
		t.Fatal(err)
	}
	var rows []issueRowJSON
	json.Unmarshal([]byte(text), &rows)
	if len(rows) != 3 || rows[2].Key != "A-3" {
		t.Fatalf("rows = %+v", rows)
	}
	if reqs != 2 {
		t.Fatalf("requests = %d, want 2 (stop once limit collected)", reqs)
	}
}

func TestSearchIssuesEmptyIsArray(t *testing.T) {
	reqs := 0
	s := newTestServer(t, pagedSearch(t, nil, 2, &reqs))
	text, err := callText(t, s, "search_issues", `{"jql":"project = A"}`)
	if err != nil {
		t.Fatal(err)
	}
	if text != "[]" {
		t.Fatalf("text = %q, want []", text)
	}
}

func TestSearchIssuesBadUpdatedNamesKey(t *testing.T) {
	reqs := 0
	s := newTestServer(t, pagedSearch(t, []map[string]any{searchIssue("A-9", "new", "yesterday", "")}, 2, &reqs))
	_, err := callText(t, s, "search_issues", `{"jql":"project = A"}`)
	if err == nil || !strings.Contains(err.Error(), "A-9") {
		t.Fatalf("err = %v, want one naming A-9", err)
	}
}

func TestSearchIssuesRequiresJQL(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("no request expected") })
	if _, err := callText(t, s, "search_issues", `{}`); err == nil {
		t.Fatal("want error without jql")
	}
}

func TestFindUsersEscapesQueryAndOmitsHiddenEmail(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/user/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.RawQuery != "query=a%2Bb%40x.com+%26c&maxResults=20" {
			t.Errorf("raw query = %s", r.URL.RawQuery)
		}
		w.Write([]byte(`[
			{"accountId":"1","displayName":"Ana","emailAddress":"ana@x.com","accountType":"atlassian","active":true},
			{"accountId":"2","displayName":"Bot","accountType":"app","active":false}
		]`))
	})
	text, err := callText(t, s, "find_users", `{"query":"a+b@x.com &c"}`)
	if err != nil {
		t.Fatal(err)
	}
	var users []map[string]any
	if err := json.Unmarshal([]byte(text), &users); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0]["emailAddress"] != "ana@x.com" || users[0]["active"] != true {
		t.Fatalf("users = %v", users)
	}
	if _, ok := users[1]["emailAddress"]; ok {
		t.Fatalf("hidden email should be omitted: %v", users[1])
	}
	if users[1]["accountType"] != "app" || users[1]["active"] != false {
		t.Fatalf("user 2 = %v", users[1])
	}
}

func TestFindUsersEmptyIsArray(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	text, err := callText(t, s, "find_users", `{"query":"nobody"}`)
	if err != nil {
		t.Fatal(err)
	}
	if text != "[]" {
		t.Fatalf("text = %q", text)
	}
}

func TestListTransitionsCarriesToCategory(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"transitions":[{"id":"31","name":"Done","to":{"name":"Closed","statusCategory":{"key":"done"}}}]}`))
	})
	text, err := callText(t, s, "list_transitions", `{"key":"AB_C-1"}`)
	if err != nil {
		t.Fatal(err)
	}
	var ts []map[string]any
	json.Unmarshal([]byte(text), &ts)
	want := map[string]any{"ID": "31", "Name": "Done", "To": "Closed", "ToCategory": "done"}
	if len(ts) != 1 || fmt.Sprint(ts[0]) != fmt.Sprint(want) {
		t.Fatalf("transitions = %v, want [%v]", ts, want)
	}
}

func TestNewToolsFollowAllowlist(t *testing.T) {
	s := &mcpServer{allowed: map[string]bool{"describe_issues": true}}
	for _, name := range []string{"search_issues", "find_users"} {
		if _, err := s.dispatchTool(name, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "not enabled") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	names := map[string]bool{}
	for _, t := range (&mcpServer{}).visibleTools() {
		names[t.(map[string]any)["name"].(string)] = true
	}
	if !names["search_issues"] || !names["find_users"] {
		t.Fatalf("tools/list lacks the new tools: %v", names)
	}
}

func TestToolsRefuseAKeyOrTransitionIDThatIsNotOneBeforeCallingJira(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Jira was called: %s %s", r.Method, r.URL)
	})
	for _, c := range []struct{ name, args string }{
		{"assign_issue", `{"key":"ABC-1/../../myself"}`},
		{"list_transitions", `{"key":"abc-1"}`},
		{"transition_issue", `{"key":"ABC-1?x=1","transitionId":"31"}`},
		{"transition_issue", `{"key":"ABC-1","transitionId":"31/../x"}`},
		{"comment_issue", `{"key":"ABC-","text":"hi"}`},
		{"describe_issues", `{"keys":["ABC-1","ABC-2) OR project = X"]}`},
	} {
		if _, err := callText(t, s, c.name, c.args); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%s %s: err = %v", c.name, c.args, err)
		}
	}
}
