package main

// Persisted config so credentials don't have to live in env vars (or in a
// committed .mcp.json). Resolution precedence is env var > config file, matching
// gh/aws. `thresh setup` writes the file; everything else reads it via
// resolveJira / llmConfig.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/term"
)

// Config is the on-disk shape (~/.config/thresh/config.json or the OS equivalent).
type Config struct {
	JiraEmail   string `json:"jira_email,omitempty"`
	JiraToken   string `json:"jira_token,omitempty"`
	JiraBaseURL string `json:"jira_base_url,omitempty"`
	LLMProvider string `json:"llm_provider,omitempty"`
	LLMBaseURL  string `json:"llm_base_url,omitempty"`
	LLMModel    string `json:"llm_model,omitempty"`
	LLMAPIKey   string `json:"llm_api_key,omitempty"`
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// configDir resolves the per-user config directory cross-platform:
// $XDG_CONFIG_HOME/thresh, else %AppData%\thresh on Windows, else ~/.config/thresh.
func configDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "thresh"), nil
	}
	if runtime.GOOS == "windows" {
		d, err := os.UserConfigDir() // %AppData%
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "thresh"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "thresh"), nil
}

func configPath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// loadConfig returns the stored config, or a zero Config if none exists / unreadable.
func loadConfig() Config {
	var c Config
	p, err := configPath()
	if err != nil {
		return c
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return c
	}
	json.Unmarshal(data, &c)
	return c
}

// saveConfig writes the config 0600 (Unix; on Windows access is governed by the
// per-user %AppData% ACLs, the mode bits are largely a no-op there).
func saveConfig(c Config) (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(d, "config.json")
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// resolveJira returns the Jira base URL, email, token: env wins over config.
func resolveJira() (base, email, token string) {
	c := loadConfig()
	base = firstNonEmpty(os.Getenv("THRESH_JIRA_BASE_URL"), c.JiraBaseURL)
	email = firstNonEmpty(os.Getenv("THRESH_JIRA_EMAIL"), c.JiraEmail)
	token = firstNonEmpty(os.Getenv("THRESH_JIRA_TOKEN"), c.JiraToken)
	return
}

// --- thresh setup ---

func readLine() string {
	// Read one byte at a time so we never buffer past the newline; that keeps
	// stdin aligned for a subsequent term.ReadPassword on the same fd.
	var b []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			if buf[0] != '\r' {
				b = append(b, buf[0])
			}
		}
		if err != nil {
			break
		}
	}
	return string(b)
}

func prompt(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	if line := strings.TrimSpace(readLine()); line != "" {
		return line
	}
	return def
}

func promptSecret(label string) string {
	fmt.Printf("%s: ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// runSetup writes the config. Interactive prompts when stdin is a TTY and no
// field flags are given; otherwise the --jira-*/--llm-* flags drive it.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	var (
		fEmail = fs.String("jira-email", "", "Jira account email")
		fToken = fs.String("jira-token", "", "Jira API token")
		fBase  = fs.String("jira-base-url", "", "Jira base URL")
		fLP    = fs.String("llm-provider", "", "LLM provider (openai|anthropic)")
		fLB    = fs.String("llm-base-url", "", "LLM base URL")
		fLM    = fs.String("llm-model", "", "LLM model")
		fLK    = fs.String("llm-api-key", "", "LLM API key")
		fShow  = fs.Bool("show", false, "print config path and current values (secrets masked)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fShow {
		return showConfig()
	}

	cfg := loadConfig()

	provided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "show" {
			provided = true
		}
	})

	if provided {
		if *fEmail != "" {
			cfg.JiraEmail = *fEmail
		}
		if *fToken != "" {
			cfg.JiraToken = *fToken
		}
		if *fBase != "" {
			cfg.JiraBaseURL = *fBase
		}
		if *fLP != "" {
			cfg.LLMProvider = *fLP
		}
		if *fLB != "" {
			cfg.LLMBaseURL = *fLB
		}
		if *fLM != "" {
			cfg.LLMModel = *fLM
		}
		if *fLK != "" {
			cfg.LLMAPIKey = *fLK
		}
	} else {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("not a terminal: pass --jira-email/--jira-token/... for non-interactive setup")
		}
		fmt.Println("thresh setup — leave a field blank to keep the current value.")
		cfg.JiraBaseURL = prompt("Jira base URL (e.g. https://your-org.atlassian.net)", cfg.JiraBaseURL)
		cfg.JiraEmail = prompt("Jira email", cfg.JiraEmail)
		if t := promptSecret("Jira API token (blank = keep current)"); t != "" {
			cfg.JiraToken = t
		}
		if strings.HasPrefix(strings.ToLower(prompt("Configure an LLM for standalone --llm? (y/N)", "")), "y") {
			cfg.LLMProvider = strings.ToLower(prompt("LLM provider (openai|anthropic)", cfg.LLMProvider))
			cfg.LLMModel = prompt("LLM model (blank = provider default)", cfg.LLMModel)
			cfg.LLMBaseURL = prompt("LLM base URL (blank = provider default)", cfg.LLMBaseURL)
			if k := promptSecret("LLM API key (blank = keep current)"); k != "" {
				cfg.LLMAPIKey = k
			}
		}
	}

	p, err := saveConfig(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("saved %s\n", p)
	return nil
}

func showConfig() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	c := loadConfig()
	mask := func(s string) string {
		if s == "" {
			return "(unset)"
		}
		return "(set)"
	}
	fmt.Println("config:", p)
	fmt.Println("jira base url:", firstNonEmpty(c.JiraBaseURL, "(unset)"))
	fmt.Println("jira email:   ", firstNonEmpty(c.JiraEmail, "(unset)"))
	fmt.Println("jira token:   ", mask(c.JiraToken))
	fmt.Println("llm provider: ", firstNonEmpty(c.LLMProvider, "(unset)"))
	fmt.Println("llm model:    ", firstNonEmpty(c.LLMModel, "(unset)"))
	fmt.Println("llm base url: ", firstNonEmpty(c.LLMBaseURL, "(unset)"))
	fmt.Println("llm api key:  ", mask(c.LLMAPIKey))
	return nil
}
