// thresh: backlog discovery for Jira (v0 slice).
// Given a scope key (e.g. an initiative), walk the hierarchy to leaves, keep the
// pending ones, rank them by impact, and print a markdown report to stdout.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// version is set at build time via -ldflags "-X main.version=...". Defaults to "dev".
var version = "dev"

var issueFields = []string{"summary", "status", "issuetype", "assignee", "updated", "parent", "issuelinks"}

// Issue is the normalized form discovery + ranking work on.
type Issue struct {
	Key            string
	Summary        string
	Type           string
	Subtask        bool
	Status         string
	StatusCategory string // "new" | "indeterminate" | "done"
	Assignee       string
	Updated        time.Time
	ParentKey      string
	BlocksOut      int      // how many other issues this one blocks (foundational signal)
	Blocks         []string // keys this issue blocks
	BlockedBy      []string // keys blocking this issue
	Relates        []string // related keys
	Score          float64
	Why            string // LLM rationale (only set with --llm)
}

func toIssue(r rawIssue) Issue {
	i := Issue{
		Key:            r.Key,
		Summary:        r.Fields.Summary,
		Type:           r.Fields.Issuetype.Name,
		Subtask:        r.Fields.Issuetype.Subtask,
		Status:         r.Fields.Status.Name,
		StatusCategory: r.Fields.Status.StatusCategory.Key,
		Updated:        parseTime(r.Fields.Updated),
	}
	if r.Fields.Assignee != nil {
		i.Assignee = r.Fields.Assignee.DisplayName
	}
	if r.Fields.Parent != nil {
		i.ParentKey = r.Fields.Parent.Key
	}
	for _, l := range r.Fields.Issuelinks {
		// Outward link: this issue → outwardIssue (relationship = Type.Outward).
		// Inward link: inwardIssue → this issue (this issue's side = Type.Inward).
		if l.OutwardIssue != nil {
			switch {
			case strings.EqualFold(l.Type.Outward, "blocks"):
				i.Blocks = append(i.Blocks, l.OutwardIssue.Key)
			case strings.EqualFold(l.Type.Name, "relates"):
				i.Relates = append(i.Relates, l.OutwardIssue.Key)
			}
		}
		if l.InwardIssue != nil {
			switch {
			case strings.EqualFold(l.Type.Inward, "is blocked by"):
				i.BlockedBy = append(i.BlockedBy, l.InwardIssue.Key)
			case strings.EqualFold(l.Type.Name, "relates"):
				i.Relates = append(i.Relates, l.InwardIssue.Key)
			}
		}
	}
	i.BlocksOut = len(i.Blocks) // foundational signal for the deterministic score
	return i
}

func parseTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// descendants walks the hierarchy under root and returns every descendant (across
// any status) plus the set of keys that have children. The root is not included.
func (j *Jira) descendants(root string) (map[string]Issue, map[string]bool, error) {
	all := map[string]Issue{}
	isParent := map[string]bool{}
	frontier := []string{root}

	for depth := 0; len(frontier) > 0 && depth < 8; depth++ {
		var next []string
		for _, batch := range chunk(frontier, 80) {
			jql := fmt.Sprintf("parent in (%s)", strings.Join(batch, ","))
			raws, err := j.search(jql, issueFields)
			if err != nil {
				return nil, nil, err
			}
			for _, r := range raws {
				iss := toIssue(r)
				if _, seen := all[iss.Key]; seen {
					continue
				}
				all[iss.Key] = iss
				if iss.ParentKey != "" {
					isParent[iss.ParentKey] = true
				}
				next = append(next, iss.Key)
			}
		}
		frontier = next
	}
	return all, isParent, nil
}

// discover resolves the scope into the issue set plus the "is a parent of another
// in-set issue" mask (rankPending drops those). A query or a bare project key
// (no "-") runs one flat search; an issue key walks the hierarchy below it.
func (j *Jira) discover(arg, jql string) (map[string]Issue, map[string]bool, error) {
	if jql == "" && strings.Contains(arg, "-") {
		return j.descendants(arg) // issue key -> hierarchy BFS
	}

	q := jql
	if q == "" {
		q = "project = " + arg // bare project key
	}
	raws, err := j.search(q, issueFields)
	if err != nil {
		return nil, nil, err
	}
	all := map[string]Issue{}
	for _, r := range raws {
		iss := toIssue(r)
		all[iss.Key] = iss
	}
	// A parent is anything referenced as another in-set issue's parent: drop it so
	// we don't list an epic and its stories together (mirrors the BFS behavior).
	isParent := map[string]bool{}
	for _, iss := range all {
		if iss.ParentKey != "" {
			if _, ok := all[iss.ParentKey]; ok {
				isParent[iss.ParentKey] = true
			}
		}
	}
	return all, isParent, nil
}

// momentum: how active the work is. In Progress > Selected for Development > the rest.
func momentum(i Issue) float64 {
	if i.StatusCategory == "indeterminate" {
		return 3
	}
	if i.Status == "Selected for Development" {
		return 2
	}
	return 1
}

// staleFactor decays a node's momentum with time-since-update: fresh work counts
// full, a months-stale "In Progress" should not outrank fresh committed work.
func staleFactor(t time.Time) float64 {
	if t.IsZero() {
		return 0.7
	}
	d := time.Since(t).Hours() / 24
	switch {
	case d < 14:
		return 1.0
	case d < 90:
		return 1.0 - 0.6*((d-14)/76) // 1.0 -> 0.4 linearly
	default:
		return 0.4
	}
}

func blocks(i Issue) int {
	return min(i.BlocksOut, 3)
}

// finalScore ranks a leaf by impact. Signals: decayed momentum, foundational
// (blocks others), bug nudge, ownership. Hierarchy level is NOT penalized; instead
// a leaf inherits one level up: a sub-task is as active as its story, and any leaf
// inherits its parent's foundational signal. No flat sub-task penalty.
func finalScore(leaf Issue, all map[string]Issue) float64 {
	mom := momentum(leaf) * staleFactor(leaf.Updated)
	blk := blocks(leaf)

	if p, ok := all[leaf.ParentKey]; ok {
		if leaf.Subtask {
			if pm := momentum(p) * staleFactor(p.Updated); pm > mom {
				mom = pm
			}
		}
		if pb := blocks(p); pb > blk {
			blk = pb
		}
	}

	s := mom + 1.5*float64(blk)
	if leaf.Type == "Bug" {
		s += 0.5
	}
	if leaf.Assignee != "" {
		s += 0.3
	}
	return s
}

// rankPending filters out parents and done issues, scores the rest, and returns
// them sorted by impact (score desc, then most-recently-updated).
func rankPending(all map[string]Issue, isParent map[string]bool) []Issue {
	var pending []Issue
	for k, iss := range all {
		if isParent[k] || iss.StatusCategory == "done" {
			continue
		}
		iss.Score = finalScore(iss, all)
		pending = append(pending, iss)
	}
	sort.SliceStable(pending, func(a, b int) bool {
		if pending[a].Score != pending[b].Score {
			return pending[a].Score > pending[b].Score
		}
		return pending[a].Updated.After(pending[b].Updated)
	})
	return pending
}

func age(t time.Time) (string, bool) {
	if t.IsZero() {
		return "?", false
	}
	d := int(time.Since(t).Hours() / 24)
	switch {
	case d < 1:
		return "today", false
	case d < 60:
		return fmt.Sprintf("%dd", d), d >= 30
	default:
		return fmt.Sprintf("%dmo", d/30), true
	}
}

func render(p TicketProvider, root string, items []Issue, withWhy bool) {
	fmt.Printf("# %s: actionable items ranked by impact (%d)\n\n", root, len(items))
	head := "| # | Key | Summary | Type | Status | Assignee | Age | Blocks | Stale |"
	sep := "|---|-----|---------|------|--------|----------|-----|--------|-------|"
	if withWhy {
		head += " Why (LLM) |"
		sep += "-----------|"
	}
	fmt.Println(head)
	fmt.Println(sep)
	for n, i := range items {
		a, stale := age(i.Updated)
		assignee := i.Assignee
		if assignee == "" {
			assignee = "-"
		}
		staleMark := ""
		if stale {
			staleMark = "stale"
		}
		blk := ""
		if i.BlocksOut > 0 {
			blk = fmt.Sprintf("%d", i.BlocksOut)
		}
		link := fmt.Sprintf("[%s](%s)", i.Key, p.browseURL(i.Key))
		row := fmt.Sprintf("| %d | %s | %s | %s | %s | %s | %s | %s | %s |",
			n+1, link, escape(i.Summary), i.Type, i.Status, assignee, a, blk, staleMark)
		if withWhy {
			row += " " + escape(i.Why) + " |"
		}
		fmt.Println(row)
	}
}

func escape(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

func chunk(s []string, n int) [][]string {
	var out [][]string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

// llmConfig resolves provider, base URL, model, and API key for the --llm tie-break,
// merging env vars (which win) over the stored config. Provider is THRESH_LLM_PROVIDER
// or auto-detected: a "claude" model or an anthropic.com base URL means the native
// Anthropic API, else OpenAI-compatible.
// Base URL and model fall back to per-provider defaults.
func llmConfig() (provider, base, model, key string) {
	c := loadConfig()
	provider = strings.ToLower(firstNonEmpty(os.Getenv("THRESH_LLM_PROVIDER"), c.LLMProvider))
	base = firstNonEmpty(os.Getenv("THRESH_LLM_BASE_URL"), c.LLMBaseURL)
	model = firstNonEmpty(os.Getenv("THRESH_LLM_MODEL"), c.LLMModel)
	key = firstNonEmpty(os.Getenv("THRESH_LLM_API_KEY"), c.LLMAPIKey)

	if provider == "" {
		if strings.HasPrefix(model, "claude") || strings.Contains(base, "anthropic.com") {
			provider = "anthropic"
		} else {
			provider = "openai"
		}
	}

	if provider == "anthropic" {
		if base == "" {
			base = "https://api.anthropic.com"
		}
		if model == "" {
			model = "claude-opus-4-8"
		}
	} else {
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		if model == "" {
			model = "gpt-4o-mini"
		}
	}
	return provider, base, model, key
}

func ageDays(t time.Time) int {
	if t.IsZero() {
		return -1
	}
	return int(time.Since(t).Hours() / 24)
}

// llmReRank reorders the top n items via the LLM tie-break (semantic impact),
// keeping the deterministic order for the tail. Two passes: the model ranks on
// titles+links, names the items it can't judge, thresh fetches those descriptions,
// and the model re-ranks with them. Falls back to the input on error.
func llmReRank(prov TicketProvider, pending []Issue, n int) ([]Issue, error) {
	if n > len(pending) {
		n = len(pending)
	}
	items := make([]rankItem, n)
	for k := 0; k < n; k++ {
		p := pending[k]
		items[k] = rankItem{
			Key: p.Key, Summary: p.Summary, Type: p.Type, Status: p.Status,
			AgeDays: ageDays(p.Updated), Blocks: p.Blocks, BlockedBy: p.BlockedBy,
			Relates: p.Relates, Assigned: p.Assignee != "",
		}
	}

	provider, base, model, key := llmConfig()
	if key == "" {
		return pending, fmt.Errorf("no LLM API key (set THRESH_LLM_API_KEY or run `thresh setup`)")
	}
	llm := NewLLM(provider, base, key, model)

	order, why, need, err := llm.reRank(items, false)
	if err != nil {
		return pending, err
	}
	// Second pass: fetch descriptions for the ambiguous items and re-rank with them.
	if len(need) > 0 {
		if len(need) > 12 {
			need = need[:12]
		}
		if details, derr := prov.describe(need); derr == nil {
			byKeyDesc := make(map[string]string, len(details))
			for _, d := range details {
				byKeyDesc[d.Key] = d.Description
			}
			for k := range items {
				if d := byKeyDesc[items[k].Key]; d != "" {
					items[k].Description = d
				}
			}
			if o2, w2, _, e2 := llm.reRank(items, true); e2 == nil {
				order, why = o2, w2
			}
		}
	}

	byKey := make(map[string]Issue, n)
	for k := 0; k < n; k++ {
		byKey[pending[k].Key] = pending[k]
	}
	var head []Issue
	seen := map[string]bool{}
	for _, k := range order {
		if iss, ok := byKey[k]; ok && !seen[k] {
			iss.Why = why[k]
			head = append(head, iss)
			seen[k] = true
		}
	}
	for k := 0; k < n; k++ { // anything the LLM dropped, in deterministic order
		if !seen[pending[k].Key] {
			head = append(head, pending[k])
		}
	}
	return append(head, pending[n:]...), nil
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			if err := runSetup(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "setup error: %v\n", err)
				os.Exit(1)
			}
			return
		case "version", "--version", "-v":
			fmt.Println("thresh", version)
			return
		}
	}

	llmFlag := flag.Bool("llm", false, "re-rank the top items semantically via an LLM (tie-break)")
	topFlag := flag.Int("top", 30, "how many top items the --llm tie-break re-ranks")
	tuiFlag := flag.Bool("tui", false, "open the interactive TUI instead of printing markdown")
	mcpFlag := flag.Bool("mcp", false, "run as an MCP server over stdio (for Claude Code et al.)")
	noLLMFlag := flag.Bool("no-llm", false, "MCP only: ignore the discover_backlog llm argument (the calling model re-ranks)")
	toolsFlag := flag.String("tools", "", "MCP only: comma-separated allowlist of tools to expose (default all)")
	jqlFlag := flag.String("jql", "", "discover via a JQL query instead of a scope (e.g. \"project = ABC AND assignee = currentUser()\")")
	flag.Usage = func() {
		w := flag.CommandLine.Output()
		fmt.Fprint(w, "thresh — backlog discovery for Jira\n\n"+
			"usage:\n"+
			"  thresh [flags] <scope>          markdown report on stdout (or --tui for the UI)\n"+
			"  thresh --jql \"<JQL>\" [flags]    use a raw JQL query as the scope\n"+
			"  thresh --mcp [flags]            run as an MCP server over stdio (Claude Code)\n"+
			"  thresh setup [flags]            store credentials in a config file\n\n"+
			"<scope>: an issue key (ABC-123, walks the hierarchy below it) or a project key (ABC, whole project).\n\n"+
			"flags:\n")
		flag.PrintDefaults()
		fmt.Fprint(w, "\nenvironment (or set via `thresh setup`; env wins over the config file):\n"+
			"  THRESH_JIRA_EMAIL      Atlassian account email                          required\n"+
			"  THRESH_JIRA_TOKEN      Jira API token                                   required\n"+
			"  THRESH_JIRA_BASE_URL   Jira site URL (e.g. https://your-org.atlassian.net) required\n"+
			"  --llm only:\n"+
			"  THRESH_LLM_API_KEY     model API key                              required for --llm\n"+
			"  THRESH_LLM_PROVIDER    anthropic | openai                             auto-detected\n"+
			"  THRESH_LLM_MODEL       model id              default claude-opus-4-8 (or gpt-4o-mini)\n"+
			"  THRESH_LLM_BASE_URL    API base URL          default per provider (e.g. OpenRouter)\n")
	}
	flag.Parse()

	if *mcpFlag {
		if err := runMCP(*noLLMFlag, *toolsFlag); err != nil {
			fmt.Fprintf(os.Stderr, "mcp error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	jql := *jqlFlag
	var arg, label string
	switch {
	case jql != "":
		label = jql
		if flag.NArg() > 0 {
			fmt.Fprintln(os.Stderr, "note: --jql given, ignoring the positional scope")
		}
	case flag.NArg() == 1:
		arg = flag.Arg(0)
		label = arg
	default:
		flag.Usage()
		os.Exit(2)
	}

	base, email, token := resolveJira()
	if base == "" || email == "" || token == "" {
		fmt.Fprintln(os.Stderr, "error: missing Jira config (set THRESH_JIRA_BASE_URL/THRESH_JIRA_EMAIL/THRESH_JIRA_TOKEN or run `thresh setup`)")
		os.Exit(2)
	}

	var prov TicketProvider = NewJira(base, email, token)
	all, isParent, err := prov.discover(arg, jql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	pending := rankPending(all, isParent)

	if *llmFlag {
		p, err := llmReRank(prov, pending, *topFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "llm error: %v (keeping deterministic order)\n", err)
		}
		pending = p
	}

	if *tuiFlag {
		if err := runTUI(prov, pending); err != nil {
			fmt.Fprintf(os.Stderr, "tui error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	render(prov, label, pending, *llmFlag)
}
