package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Mode is the room-wide permission level handed to every CLI.
const (
	ModeChat  = "chat"  // scratch cwd; CLI-specific tool restrictions
	ModeRead  = "read"  // workspace cwd; CLI-specific read/plan permissions
	ModeWrite = "write" // workspace edits, without permission-bypass flags
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
	MCP        *MCPServer    // extra read-only tool server for this run (nil = none)
	ExtraDirs  []string      // additional readable folders (provider-internal use)
}

// MCPServer is a stdio MCP server handed to a CLI for one run. Name becomes the
// tool prefix (Claude shows tools as mcp__<Name>__<tool>).
type MCPServer struct {
	Name    string
	Command string
	Args    []string
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
	// ModelEfforts lists the reasoning efforts per model when they differ by
	// model; nil means every model accepts Efforts().
	ModelEfforts() map[string][]string
	Efforts() []string
	DefaultModel() (model, effort string)
	Available() bool
	CanSteer() bool // true when a message can be injected into a running turn
	ContextWindow(model string) int
	Run(ctx context.Context, opts RunOptions, emit func(Event)) (Result, error)
}

// ModelOption is one entry of a provider's model picker.
type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group,omitempty"`
}

// Info is the serialisable view of a provider for the UI.
type Info struct {
	ID            string              `json:"id"`
	Label         string              `json:"label"`
	Command       string              `json:"command"`
	Available     bool                `json:"available"`
	Path          string              `json:"path"`
	Models        []ModelOption       `json:"models"`
	Efforts       []string            `json:"efforts"`
	ModelEfforts  map[string][]string `json:"model_efforts,omitempty"`
	Default       string              `json:"default_model"`
	DefaultEffort string              `json:"default_effort"`
	CanSteer      bool                `json:"can_steer"`
	CatalogStatus string              `json:"catalog_status,omitempty"`
	CatalogError  string              `json:"catalog_error,omitempty"`
}

// Registry holds the configured providers in display order.
type Registry struct {
	providers []Provider
}

// NewRegistry holds any providers (tests inject fakes).
func NewRegistry(ps ...Provider) *Registry { return &Registry{providers: ps} }

// DefaultRegistry builds the three built-in providers from command names.
// agyStatus is the file `roundtable agy-statusline` writes Gemini quota into.
func DefaultRegistry(claudeCmd, codexCmd, agyCmd, agyStatus string) *Registry {
	return NewRegistry(NewClaude(claudeCmd), NewCodex(codexCmd), NewAgy(agyCmd, agyStatus))
}

func (r *Registry) Add(p Provider) { r.providers = append(r.providers, p) }

// LimitSource is implemented by providers whose account limits can be read at
// any time, not only at the end of a turn.
type LimitSource interface {
	CurrentLimits() (Limits, bool)
}

// LimitRefresher reads quotas without sending a prompt or starting a model turn.
// A refresh may read a local observation; Limits.At remains its original time.
type LimitRefresher interface {
	RefreshLimits(context.Context) (Limits, error)
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
		in := Info{ID: p.ID(), Label: p.Label(), Command: p.Command(), Available: p.Available(), Path: path, Models: p.Models(), Efforts: p.Efforts(), ModelEfforts: p.ModelEfforts(), CanSteer: p.CanSteer()}
		in.Default, in.DefaultEffort = p.DefaultModel()
		if catalog, ok := p.(interface{ CatalogStatus() (string, string) }); ok {
			in.CatalogStatus, in.CatalogError = catalog.CatalogStatus()
		}
		out = append(out, in)
	}
	return out
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
