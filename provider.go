package main

// TicketProvider is the backend boundary: everything above it (discovery
// orchestration, ranking, the LLM tie-break, the TUI, the MCP server) works on
// the normalized Issue model and never touches a concrete API. Jira implements
// it today; a Linear or GitHub Issues adapter would implement the same methods.
type TicketProvider interface {
	// discover resolves a scope (issue key -> hierarchy below it, project key ->
	// whole project) or a raw query into the issue set plus the "is a parent of
	// another in-set issue" mask.
	discover(scope, query string) (map[string]Issue, map[string]bool, error)
	// describe returns the keys with their (flattened, truncated) descriptions.
	describe(keys []string) ([]IssueDetail, error)
	// transitions lists the workflow transitions available on an issue.
	transitions(key string) ([]Transition, error)
	doTransition(key, transitionID string) error
	addComment(key, text string) error
	// assignToMe assigns the issue to the authenticated user.
	assignToMe(key string) error
	// browseURL is the human-facing URL for an issue key.
	browseURL(key string) string
}
