package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Mode is the room-wide permission level handed to every CLI.
const (
	ModeChat  = "chat"  // no tools, conversation only
	ModeRead  = "read"  // read-only tools inside the workspace
	ModeWrite = "write" // edits auto-accepted inside the workspace, no sandbox bypass
)

// RunOptions describes one turn of one agent.
type RunOptions struct {
	Prompt     string
	SessionID  string // resume when non-empty
	Model      string
	Effort     string
	Mode       string
	Workspace  string // absolute path; required for read/write
	ScratchDir string // cwd for chat mode
	Timeout    time.Duration
	Steer      <-chan string // live user messages while the turn runs (providers that CanSteer)
}

// Result is the outcome of a turn.
type Result struct {
	Text      string
	SessionID string
	Usage     map[string]any
	Stderr    string
	Steered   int // steer messages actually written to the running CLI
}

// Provider is one vendor CLI.
type Provider interface {
	ID() string
	Label() string
	Command() string
	Models() []ModelOption
	Available() bool
	CanSteer() bool // true when a message can be injected into a running turn
	ContextWindow(model string) int
	Run(ctx context.Context, opts RunOptions, emit func(Event)) (Result, error)
}

// ModelOption is one entry of a provider's model picker.
type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Info is the serialisable view of a provider for the UI.
type Info struct {
	ID            string        `json:"id"`
	Label         string        `json:"label"`
	Command       string        `json:"command"`
	Available     bool          `json:"available"`
	Path          string        `json:"path"`
	Models        []ModelOption `json:"models"`
	Efforts       []string      `json:"efforts"`
	Default       string        `json:"default_model"`
	DefaultEffort string        `json:"default_effort"`
	CanSteer      bool          `json:"can_steer"`
}

// Registry holds the configured providers in display order.
type Registry struct {
	providers []Provider
}

// NewRegistry holds any providers (tests inject fakes).
func NewRegistry(ps ...Provider) *Registry { return &Registry{providers: ps} }

// DefaultRegistry builds the three built-in providers from command names.
func DefaultRegistry(claudeCmd, codexCmd, agyCmd string) *Registry {
	return NewRegistry(NewClaude(claudeCmd), NewCodex(codexCmd), NewAgy(agyCmd))
}

func (r *Registry) Get(id string) Provider {
	for _, p := range r.providers {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

func (r *Registry) All() []Provider { return r.providers }

func (r *Registry) Infos() []Info {
	var out []Info
	for _, p := range r.providers {
		path, _ := resolveCommand(p.Command())
		in := Info{ID: p.ID(), Label: p.Label(), Command: p.Command(), Available: p.Available(), Path: path, Models: p.Models(), Efforts: efforts(p.ID()), CanSteer: p.CanSteer()}
		in.Default, in.DefaultEffort = defaults(p.ID())
		out = append(out, in)
	}
	return out
}

func efforts(id string) []string {
	switch id {
	case "codex":
		return []string{"low", "medium", "high", "xhigh"}
	case "agy":
		return nil // effort is part of the agy model slug
	default:
		return []string{"low", "medium", "high", "max"}
	}
}

func defaults(id string) (model, effort string) {
	switch id {
	case "claude":
		return "opus", "high"
	case "codex":
		return "gpt-6.1-sol", "high"
	case "agy":
		return "gemini-3.8-flash-high", ""
	}
	return "", ""
}

func available(cmd string) bool {
	_, err := resolveCommand(cmd)
	return err == nil
}

// listOutput runs a CLI listing command with a short timeout (used for `agy models`).
func listOutput(cmd string, args ...string) []string {
	path, err := resolveCommand(cmd)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	hideWindow(c)
	out, err := c.Output()
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
