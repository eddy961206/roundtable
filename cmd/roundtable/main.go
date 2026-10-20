// Command roundtable serves the multi-vendor agent chat room.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"roundtable"
	"roundtable/internal/agent"
	"roundtable/internal/agyhook"
	"roundtable/internal/giteamcp"
	"roundtable/internal/room"
	"roundtable/internal/rtmcp"
	"roundtable/internal/server"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mcp":
			os.Exit(runMCP(os.Args[2:]))
		case "rt":
			os.Exit(runRT(os.Args[2:]))
		case agyhook.Marker:
			os.Exit(runAgyStatusline(os.Args[2:]))
		case "agy-hook":
			os.Exit(runAgyHook(os.Args[2:]))
		case "gitea-mcp":
			os.Exit(runGiteaMCP(os.Args[2:]))
		}
	}
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	data := flag.String("data", filepath.Join(home, ".roundtable"), "data directory")
	claudeCmd := flag.String("claude", "claude", "Claude Code command")
	codexCmd := flag.String("codex", "codex", "Codex command")
	codexSandbox := flag.String("codex-windows-sandbox", "", "opt Codex back into a Windows sandbox: elevated | unelevated (empty = no OS sandbox; explicit value restores read/write sandbox policies)")
	agyCmd := flag.String("agy", "agy", "Antigravity command")
	openCodeCmd := flag.String("opencode", "opencode", "OpenCode command")
	open := flag.Bool("open", false, "open the browser after start")
	agyStatus := flag.String("agy-status", "", "Gemini quota file written by `roundtable agy-statusline` (default <data>/agy-status.json)")
	giteaRepos := flag.String("gitea-repos", "", "comma-separated Owner/Name list agents may read through the read-only Gitea tool (empty = off)")
	giteaLogin := flag.String("gitea-login", "", "tea login name used for the read-only Gitea tool")
	teaCmd := flag.String("tea", "tea", "tea (Gitea CLI) command for the read-only Gitea tool")
	flag.Parse()
	if *agyStatus == "" {
		*agyStatus = filepath.Join(*data, "agy-status.json")
	}

	logger := log.New(os.Stdout, "", log.LstdFlags)
	// Reserve the address before startup recovery can mutate persisted rooms.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Fatalf("listen %s: %v", *addr, err)
	}
	defer ln.Close()
	store, err := room.NewStore(*data)
	if err != nil {
		logger.Fatalf("data dir: %v", err)
	}
	scratch := filepath.Join(*data, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		logger.Fatalf("scratch dir: %v", err)
	}
	switch *codexSandbox {
	case "", "elevated", "unelevated":
	default:
		log.Fatalf("-codex-windows-sandbox: elevated 또는 unelevated만 허용: %q", *codexSandbox)
	}
	reg := agent.DefaultRegistry(*claudeCmd, *codexCmd, *agyCmd, *agyStatus)
	reg.Get("codex").(*agent.Codex).SetWindowsSandbox(*codexSandbox)
	reg.Add(agent.NewOpenCode(*openCodeCmd))
	hub, err := room.NewHub(store, reg, scratch, logger)
	if err != nil {
		logger.Fatalf("load rooms: %v", err)
	}
	if *giteaRepos != "" {
		m, err := giteaMCPServer(*teaCmd, *giteaLogin, *giteaRepos)
		if err != nil {
			logger.Fatalf("gitea: %v", err)
		}
		hub.SetMCP(m)
		logger.Printf("gitea read-only tool on for %s (login %s)", *giteaRepos, *giteaLogin)
	}
	web, err := fs.Sub(roundtable.WebFS, "web")
	if err != nil {
		logger.Fatal(err)
	}
	srv := server.New(hub, reg, web)

	url := "http://" + ln.Addr().String()
	for _, info := range reg.Infos() {
		state := "없음"
		if info.Available {
			state = info.Path
		}
		logger.Printf("provider %-7s %s", info.ID, state)
	}
	logger.Printf("roundtable listening on %s (data %s)", url, *data)
	if *open {
		openBrowser(url)
	}
	httpServer := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10, ErrorLog: logger}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hub.Shutdown(shutdownCtx); err != nil {
			logger.Printf("agent shutdown: %v", err)
		}
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Printf("HTTP shutdown: %v", err)
			_ = httpServer.Close()
		}
	}()
	err = httpServer.Serve(ln)
	stop()
	<-shutdownDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
}

func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	url := fs.String("url", "", "Roundtable HTTP URL (loopback only)")
	readOnly := fs.Bool("read-only", false, "expose only read tools")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *url == "" {
		fmt.Fprintln(os.Stderr, "사용법: roundtable mcp --url http://127.0.0.1:8787 [--read-only]")
		return 2
	}
	s, err := rtmcp.New(*url, *readOnly)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := s.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// runRT is the shell form of the MCP tools for agents without MCP registration.
func runRT(args []string) int {
	fs := flag.NewFlagSet("rt", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8787", "Roundtable HTTP URL (loopback only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := rtmcp.New(*url, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return s.RunCLI(ctx, fs.Args(), os.Stdin, os.Stdout, os.Stderr)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "browser:", err)
	}
}

// runAgyStatusline is what agy executes as its statusLine command once the hook
// is installed. It must stay fast and must not print anything of its own.
func runAgyStatusline(args []string) int {
	fs := flag.NewFlagSet(agyhook.Marker, flag.ContinueOnError)
	cfg := fs.String("config", "", "hook config written by `roundtable agy-hook install`")
	if err := fs.Parse(args); err != nil || *cfg == "" {
		fmt.Fprintln(os.Stderr, "사용법: roundtable agy-statusline --config <path>")
		return 2
	}
	return agyhook.Run(*cfg, os.Stdin, os.Stdout, os.Stderr)
}

// runAgyHook installs, removes or shows the Gemini quota hook.
func runAgyHook(args []string) int {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("agy-hook", flag.ContinueOnError)
	data := fs.String("data", filepath.Join(home, ".roundtable"), "roundtable data directory")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "사용법: roundtable agy-hook install|uninstall|status [-data dir]")
		return 2
	}
	action := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	settings, err := agyhook.SettingsPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfgPath := filepath.Join(*data, "agy-statusline.json")
	out := filepath.Join(*data, "agy-status.json")
	switch action {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		command, err := agyhook.Install(settings, exe, cfgPath, out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "설치 실패:", err)
			return 1
		}
		fmt.Printf("설치함: %s\n  statusLine.command = %s\n  원본 백업 = %s.roundtable-backup\n  agy가 상태표시줄을 그릴 때마다 %s 가 갱신됨\n", settings, command, settings, out)
	case "uninstall":
		if err := agyhook.Uninstall(settings, cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "제거 실패:", err)
			return 1
		}
		fmt.Println("제거함: 원래 statusLine 으로 되돌림")
	case "status":
		a := agent.NewAgy("agy", out)
		l, ok := a.CurrentLimits()
		if !ok {
			fmt.Println("아직 수집된 Gemini 한도 없음:", out)
			return 1
		}
		for _, w := range l.Windows {
			fmt.Printf("%-16s 남음 %5.1f%%\n", w.Label, 100-w.UsedPercent)
		}
	default:
		fmt.Fprintln(os.Stderr, "알 수 없는 동작:", action)
		return 2
	}
	return 0
}

// giteaMCPServer describes how CLIs launch `roundtable gitea-mcp`.
func giteaMCPServer(tea, login, repos string) (*agent.MCPServer, error) {
	if login == "" {
		return nil, fmt.Errorf("-gitea-login 이 필요함")
	}
	teaPath, err := exec.LookPath(tea)
	if err != nil {
		return nil, fmt.Errorf("tea 를 찾지 못함(-tea 로 경로 지정): %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if _, err := giteamcp.New(strings.Split(repos, ","), nil); err != nil {
		return nil, err
	}
	return &agent.MCPServer{Name: "gitea", Command: exe, Args: []string{"gitea-mcp", "--tea", teaPath, "--login", login, "--repos", repos}}, nil
}

// runGiteaMCP is the read-only Gitea MCP server process that CLIs start.
func runGiteaMCP(args []string) int {
	fs := flag.NewFlagSet("gitea-mcp", flag.ContinueOnError)
	tea := fs.String("tea", "tea", "tea command")
	login := fs.String("login", "", "tea login name")
	repos := fs.String("repos", "", "comma-separated Owner/Name allow-list")
	if err := fs.Parse(args); err != nil || *login == "" || *repos == "" {
		fmt.Fprintln(os.Stderr, "사용법: roundtable gitea-mcp --login <tea 로그인> --repos Owner/A,Owner/B [--tea 경로]")
		return 2
	}
	s, err := giteamcp.New(strings.Split(*repos, ","), giteamcp.TeaFetcher(*tea, *login))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := s.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
