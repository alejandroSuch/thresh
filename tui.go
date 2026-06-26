package main

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// interaction modes the TUI can be in.
const (
	modeList = iota
	modeTransition
	modeComment
)

var (
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// issueItem adapts an Issue to the bubbles list.Item interface.
type issueItem struct{ iss Issue }

func (i issueItem) Title() string { return i.iss.Key + "  " + i.iss.Summary }
func (i issueItem) Description() string {
	a := i.iss.Assignee
	if a == "" {
		a = "unassigned"
	}
	d := fmt.Sprintf("%s · %s · %s", i.iss.Status, i.iss.Type, a)
	if i.iss.Why != "" {
		d += " · " + i.iss.Why
	}
	return d
}
func (i issueItem) FilterValue() string { return i.iss.Key + " " + i.iss.Summary }

// transitionItem adapts a Transition to a list.Item.
type transitionItem struct{ t Transition }

func (t transitionItem) Title() string       { return t.t.Name }
func (t transitionItem) Description() string { return "→ " + t.t.To }
func (t transitionItem) FilterValue() string { return t.t.Name }

// messages produced by async commands.
type actionDone struct {
	msg         string
	err         error
	key         string
	newStatus   string
	newAssignee string
}
type transitionsLoaded struct {
	key   string
	items []Transition
	err   error
}

type tuiModel struct {
	prov    TicketProvider
	mode    int
	list    list.Model
	trList  list.Model
	comment textinput.Model
	selKey  string // issue acted on in a sub-mode
	status  string
	isErr   bool
	w, h    int
}

func newTUI(prov TicketProvider, items []Issue) tuiModel {
	rows := make([]list.Item, len(items))
	for i, iss := range items {
		rows[i] = issueItem{iss: iss}
	}
	l := list.New(rows, list.NewDefaultDelegate(), 0, 0)
	l.Title = "actionable items by impact"
	l.SetStatusBarItemName("issue", "issues")

	ti := textinput.New()
	ti.Placeholder = "comment…"
	ti.CharLimit = 2000

	return tuiModel{prov: prov, list: l, comment: ti, mode: modeList}
}

func (m tuiModel) Init() tea.Cmd { return nil }

func (m tuiModel) selectedIssue() (issueItem, bool) {
	it, ok := m.list.SelectedItem().(issueItem)
	return it, ok
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height-1 // reserve the status/help line
		m.list.SetSize(m.w, m.h)
		if m.mode == modeTransition {
			m.trList.SetSize(m.w, m.h) // only safe once trList is initialized
		}
		return m, nil

	case actionDone:
		if msg.err != nil {
			m.status, m.isErr = msg.err.Error(), true
			return m, nil
		}
		m.status, m.isErr = msg.msg, false
		// optimistic local update so the row reflects the change immediately
		if msg.key != "" {
			for idx, raw := range m.list.Items() {
				it, ok := raw.(issueItem)
				if !ok || it.iss.Key != msg.key {
					continue
				}
				if msg.newStatus != "" {
					it.iss.Status = msg.newStatus
				}
				if msg.newAssignee != "" {
					it.iss.Assignee = msg.newAssignee
				}
				m.list.SetItem(idx, it)
				break
			}
		}
		return m, nil

	case transitionsLoaded:
		if msg.err != nil {
			m.status, m.isErr = msg.err.Error(), true
			return m, nil
		}
		rows := make([]list.Item, len(msg.items))
		for i, t := range msg.items {
			rows[i] = transitionItem{t: t}
		}
		d := list.NewDefaultDelegate()
		m.trList = list.New(rows, d, m.w, m.h)
		m.trList.Title = msg.key + " · pick a transition (esc to cancel)"
		m.selKey = msg.key
		m.mode = modeTransition
		return m, nil
	}

	switch m.mode {
	case modeComment:
		return m.updateComment(msg)
	case modeTransition:
		return m.updateTransition(msg)
	default:
		return m.updateList(msg)
	}
}

func (m tuiModel) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && m.list.FilterState() != list.Filtering {
		switch key.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "o", "enter":
			if it, ok := m.selectedIssue(); ok {
				return m, openCmd(m.prov.browseURL(it.iss.Key))
			}
		case "a":
			if it, ok := m.selectedIssue(); ok {
				return m, assignCmd(m.prov, it.iss.Key)
			}
		case "t":
			if it, ok := m.selectedIssue(); ok {
				m.status, m.isErr = "loading transitions for "+it.iss.Key+"…", false
				return m, loadTransitionsCmd(m.prov, it.iss.Key)
			}
		case "c":
			if it, ok := m.selectedIssue(); ok {
				m.selKey = it.iss.Key
				m.comment.SetValue("")
				m.comment.Focus()
				m.mode = modeComment
				return m, textinput.Blink
			}
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m tuiModel) updateTransition(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && m.trList.FilterState() != list.Filtering {
		switch key.String() {
		case "esc", "q":
			m.mode = modeList
			return m, nil
		case "enter":
			if it, ok := m.trList.SelectedItem().(transitionItem); ok {
				key := m.selKey
				m.mode = modeList
				m.status, m.isErr = "transitioning "+key+"…", false
				return m, transitionCmd(m.prov, key, it.t.ID, it.t.To)
			}
		}
	}
	var cmd tea.Cmd
	m.trList, cmd = m.trList.Update(msg)
	return m, cmd
}

func (m tuiModel) updateComment(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			m.mode = modeList
			m.comment.Blur()
			return m, nil
		case "enter":
			text := m.comment.Value()
			key := m.selKey
			m.mode = modeList
			m.comment.Blur()
			if text == "" {
				m.status, m.isErr = "empty comment, nothing posted", true
				return m, nil
			}
			m.status, m.isErr = "commenting on "+key+"…", false
			return m, commentCmd(m.prov, key, text)
		}
	}
	var cmd tea.Cmd
	m.comment, cmd = m.comment.Update(msg)
	return m, cmd
}

func (m tuiModel) View() string {
	switch m.mode {
	case modeComment:
		it, _ := m.selectedIssue()
		body := fmt.Sprintf("Comment on %s (enter to post, esc to cancel)\n\n%s",
			it.iss.Key, m.comment.View())
		return body + "\n\n" + m.statusLine()
	case modeTransition:
		return m.trList.View() + "\n" + m.statusLine()
	default:
		return m.list.View() + "\n" + m.statusLine()
	}
}

func (m tuiModel) statusLine() string {
	help := helpStyle.Render("↑/↓ move · o open · a self-assign · t transition · c comment · / filter · q quit")
	if m.status == "" {
		return help
	}
	style := statusStyle
	if m.isErr {
		style = errStyle
	}
	return style.Render(m.status) + "  " + help
}

// commands

func openCmd(url string) tea.Cmd {
	return func() tea.Msg {
		if err := openBrowser(url); err != nil {
			return actionDone{err: err}
		}
		return actionDone{msg: "opened " + url}
	}
}

func assignCmd(prov TicketProvider, key string) tea.Cmd {
	return func() tea.Msg {
		if err := prov.assignToMe(key); err != nil {
			return actionDone{err: err}
		}
		return actionDone{msg: "assigned " + key + " to you", key: key, newAssignee: "you"}
	}
}

func loadTransitionsCmd(prov TicketProvider, key string) tea.Cmd {
	return func() tea.Msg {
		ts, err := prov.transitions(key)
		return transitionsLoaded{key: key, items: ts, err: err}
	}
}

func transitionCmd(prov TicketProvider, key, transitionID, toStatus string) tea.Cmd {
	return func() tea.Msg {
		if err := prov.doTransition(key, transitionID); err != nil {
			return actionDone{err: err}
		}
		return actionDone{msg: key + " → " + toStatus, key: key, newStatus: toStatus}
	}
}

func commentCmd(prov TicketProvider, key, text string) tea.Cmd {
	return func() tea.Msg {
		if err := prov.addComment(key, text); err != nil {
			return actionDone{err: err}
		}
		return actionDone{msg: "commented on " + key}
	}
}

// openBrowser opens url in the OS default browser.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	return exec.Command(name, append(args, url)...).Start()
}

// runTUI launches the interactive UI over the ranked issues.
func runTUI(prov TicketProvider, items []Issue) error {
	_, err := tea.NewProgram(newTUI(prov, items), tea.WithAltScreen()).Run()
	return err
}
