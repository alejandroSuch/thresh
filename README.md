# thresh

Backlog discovery for Jira (v0 slice). Point it at a scope (an issue key, a project, or a JQL query) and it surfaces the pending, actionable issues ranked by impact as a markdown report on stdout. Optional interactive TUI and an MCP server for Claude Code.

## Run

Requires Go 1.22+. The core (discovery, ranking, markdown, `--llm`) is stdlib only; the `--tui` mode pulls in Bubble Tea.

```sh
# 1. Create a Jira API token: https://id.atlassian.com/manage-profile/security/api-tokens
export THRESH_JIRA_EMAIL="you@example.com"
export THRESH_JIRA_TOKEN="<api-token>"
export THRESH_JIRA_BASE_URL="https://your-org.atlassian.net"

go run . ABC-123                       # issue key: walk the hierarchy below it
go run . ABC                          # project key: all issues in the project
go run . --jql "project = ABC AND assignee = currentUser()"   # any JQL scope
# or build a binary:
go build -o thresh . && ./thresh ABC-123
```

The scope is an **issue key** (`ABC-123`, walks the hierarchy below it down to actionable items), a **project key** (`ABC`, every issue in the project), or **`--jql`** for anything you can express: a sprint (`sprint in openSprints()`), a board's filter, your own work (`assignee = currentUser()`), etc. A board or sprint is, under the hood, just a JQL filter.

## Configuration

Credentials come from env vars or a config file written by `thresh setup`. **Precedence: env var > config file**, so CI/containers and the MCP `env:` block still override.

```sh
thresh setup            # interactive prompts (token entered without echo)
thresh setup --jira-email you@example.com --jira-token <tok>   # non-interactive (flags)
thresh setup --show     # print the config path + which fields are set (secrets masked)
```

`setup` writes `config.json` (mode 0600) to the per-user config dir:

| OS | path |
|---|---|
| Linux | `$XDG_CONFIG_HOME/thresh/` or `~/.config/thresh/` |
| macOS | `~/.config/thresh/` |
| Windows | `%AppData%\thresh\` |

Every setting has three interchangeable sources: an **env var**, a **config-file key**, or a **`thresh setup` flag** (the env var lowercased, `THRESH_` dropped, `_`→`-`: `THRESH_JIRA_EMAIL` → `--jira-email`). Env wins over the file.

**Jira (required):**

| Env var | config.json key | What it is | Default |
|---|---|---|---|
| `THRESH_JIRA_EMAIL` | `jira_email` | Atlassian account email (the Basic-auth user) | — (required) |
| `THRESH_JIRA_TOKEN` | `jira_token` | Jira API token, from id.atlassian.com → Security → API tokens | — (required) |
| `THRESH_JIRA_BASE_URL` | `jira_base_url` | Jira Cloud site URL, e.g. `https://your-org.atlassian.net` | — (required) |

**LLM (only for standalone `--llm`; not needed for MCP, where the calling model re-ranks):**

| Env var | config.json key | What it is | Default |
|---|---|---|---|
| `THRESH_LLM_API_KEY` | `llm_api_key` | API key for the tie-break model | — (required for `--llm`) |
| `THRESH_LLM_PROVIDER` | `llm_provider` | `anthropic` or `openai`. Auto-detected when empty: a `claude…` model or an `anthropic.com` base URL → `anthropic`, else `openai` | auto |
| `THRESH_LLM_MODEL` | `llm_model` | Model id (e.g. `claude-haiku-4-5`, `gpt-4o-mini`, or an OpenRouter slug like `google/gemini-2.5-flash`) | `claude-opus-4-8` (anthropic) / `gpt-4o-mini` (openai) |
| `THRESH_LLM_BASE_URL` | `llm_base_url` | API base URL. Point at OpenRouter (`https://openrouter.ai/api/v1`), a local server, etc. | `https://api.anthropic.com` (anthropic) / `https://api.openai.com/v1` (openai) |

Note: the token is plain text on disk (0600 on Unix; on Windows it relies on the per-user `%AppData%` ACLs, the mode bits are a no-op).

Output is markdown on stdout, so pipe it wherever you like:

```sh
./thresh ABC-123 | glow -      # pretty-render
./thresh ABC-123 > backlog.md  # if you do want a file
```

### Optional: LLM tie-break (`--llm`)

Deterministic ranking is the default (free, no model). `--llm` re-ranks the top items by **semantic** impact (foundational / unblocks-others, severity, zombie-detection). This recovers signal that is not in Jira's structured data (e.g. "this is foundational" when no `Blocks` link encodes it).

Two providers, same prompt and JSON contract:

```sh
export THRESH_LLM_API_KEY="<key>"
# provider is auto-detected: a "claude" model or an anthropic.com base URL -> Anthropic;
# otherwise OpenAI-compatible. Force it with THRESH_LLM_PROVIDER=openai|anthropic.

# Anthropic (Claude) — native Messages API:
export THRESH_LLM_PROVIDER="anthropic"
export THRESH_LLM_MODEL="claude-haiku-4-5"   # default claude-opus-4-8; haiku is cheaper and plenty for a tie-break
# THRESH_LLM_BASE_URL defaults to https://api.anthropic.com

# OpenAI-compatible — /chat/completions:
export THRESH_LLM_PROVIDER="openai"
export THRESH_LLM_MODEL="gpt-4o-mini"        # default
# THRESH_LLM_BASE_URL defaults to https://api.openai.com/v1

go run . --llm ABC-123          # re-rank top 30 (default)
go run . --llm --top 50 ABC-123
```

It re-ranks only the top N (default 30) and keeps the deterministic order for the tail; on any LLM error it falls back to the deterministic order. Adds a "Why (LLM)" column.

Two passes: the model first ranks on titles + dependency links, names the items whose title is too ambiguous to judge (severity, foundational-ness), thresh fetches just those descriptions (one targeted query), and the model re-ranks with them. Descriptions are not pulled in discovery, so the cheap path stays cheap.

### Interactive TUI (`--tui`)

Opens the ranked items in a navigable list (Bubble Tea) with per-task actions, instead of printing markdown. Combine with `--llm` to act on the semantically re-ranked order.

```sh
go run . --tui ABC-123
go run . --tui --llm ABC-123
```

Keys: `↑/↓` (or `j/k`) move, `o`/`enter` open in browser, `a` self-assign, `t` transition (pick from available), `c` comment, `/` filter, `q` quit. Assign/transition/comment write to Jira via your token; the row updates optimistically on success.

### Use inside Claude Code (MCP)

`--mcp` runs thresh as an MCP server over stdio, exposing its discovery+ranking and the per-task actions as tools Claude can call. thresh keeps its own Jira token (it talks to Jira directly); Claude does not pass its Atlassian OAuth to it.

Build a binary and store creds once (MCP launches the command per session, so avoid `go run` and the inline env):

```sh
go build -o thresh .
./thresh setup                      # store Jira creds in the config file (see Configuration)

claude mcp add thresh -- /absolute/path/to/thresh --mcp
# creds inline instead of setup:
#   claude mcp add thresh -e THRESH_JIRA_EMAIL=... -e THRESH_JIRA_TOKEN=... -- /abs/thresh --mcp
```

Or project-scoped via `.mcp.json` (no creds in the file once `setup` is done):

```json
{
  "mcpServers": {
    "thresh": { "command": "/absolute/path/to/thresh", "args": ["--mcp"] }
  }
}
```

Tools exposed:

| Tool | What it does | Writes to Jira? |
|---|---|---|
| `discover_backlog` | Discover + rank the pending issues for a scope (JSON) plus per-issue signals and dependency links (`blocks`/`blockedBy`/`relates`). Args: `scope` (issue or project key) or `jql`, plus `llm`, `top`. | no |
| `describe_issues` | Fetch the (truncated) descriptions of specific issues, to judge severity / foundational-ness when the title is ambiguous. Arg `keys` (array). | no |
| `list_transitions` | List the workflow transitions available on an issue. Arg `key`. | no |
| `assign_issue` | Assign an issue to you (the configured identity). Arg `key`. | **yes** |
| `transition_issue` | Move an issue through a transition. Args `key`, `transitionId`. | **yes** |
| `comment_issue` | Add a plain-text comment. Args `key`, `text`. | **yes** |

Server flags (put them in `args`):

- `--no-llm` — ignore the `discover_backlog` `llm` argument so the calling model always re-ranks. Hard guarantee thresh never makes its own LLM call (and never needs `THRESH_LLM_*`).
- `--tools a,b` — allowlist of tools to expose; anything not listed is hidden and rejected. To drop a tool, just leave it out of the list.

You can also gate the tool surface from the Claude Code side via permissions (allow/deny `mcp__thresh__*`).

Concrete example: a read-only thresh (discovery + transition listing, no writes), with the calling model doing the re-rank:

```json
{
  "mcpServers": {
    "thresh": {
      "command": "/Users/you/projects/thresh/thresh",
      "args": ["--mcp", "--no-llm", "--tools", "discover_backlog,list_transitions"]
    }
  }
}
```

Drop `--tools` for the full set (adds `assign_issue` / `transition_issue` / `comment_issue`); drop `--no-llm` to let a `discover_backlog` call with `llm: true` run thresh's own tie-break (needs `THRESH_LLM_*`).

You don't need the `--llm` tie-break here: the MCP caller is itself an LLM. Called **without** `llm`, `discover_backlog` returns the deterministic order plus the raw signals (status, ageDays, blocks, type, score) and attaches the impact rubric to the result, asking the model to re-rank in its own context (same rubric the `--llm` path uses). No `THRESH_LLM_*` keys, no extra API call. Pass `llm: true` only if you want thresh to run the re-rank itself (redundant from an LLM client); the rubric is then omitted because the order is already semantic. The `llm` flag is really for standalone CLI/TUI, where there is no model in the loop.

## How it works (v0)

- **Discovery**: an issue-key scope walks the hierarchy via `parent in (...)` down to the items with no children (BFS, capped at 8 levels); a project key or `--jql` runs a single flat query instead.
- **Pending filter**: drops `done` issues, and any issue that is a parent of another issue in the result (so an epic and its stories aren't both listed).
- **Ranking** (deterministic): decayed `momentum` (In Progress > Selected for Development > Backlog, aged down over time) + foundational (`Blocks`) + bug nudge + ownership. Subtasks inherit the parent's momentum/blocks. Optional `--llm` semantic tie-break on top, which also reads the dependency links and (on demand) descriptions.
- **Stale flag**: issues not updated in 30+ days.

## Not yet (roadmap)

Dedicated `--board` selector (Agile API: columns/sprints), won't-do rationale, Linear / GitHub Issues adapters.
