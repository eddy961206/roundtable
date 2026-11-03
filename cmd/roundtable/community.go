package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
	"roundtable/internal/server"
	"roundtable/internal/worker"
)

// communityFlags is the remote seat listener of the server (docs/사내에이전트_커뮤니티_설계.md).
type communityFlags struct {
	addr, allow, cert, key, url *string
}

func registerCommunityFlags() communityFlags {
	return communityFlags{
		addr:  flag.String("community-addr", "", "listen address for remote seat workers, e.g. 0.0.0.0:8790 (empty = off; the local UI and /api stay loopback only)"),
		allow: flag.String("community-allow", server.DefaultCommunityAllow, "comma-separated CIDR list of worker addresses the community listener accepts"),
		cert:  flag.String("community-cert", "", "TLS certificate file for the community listener (with -community-key; empty = plain HTTP)"),
		key:   flag.String("community-key", "", "TLS key file for the community listener"),
		url:   flag.String("community-url", "", "address shown to people for their workers (default http(s)://<hostname>:<port>)"),
	}
}

// startCommunity opens the listener before the hub loads rooms, like the main
// address, and returns the server to start once the hub exists.
func (f communityFlags) listen() (net.Listener, string, error) {
	if *f.addr == "" {
		return nil, "", nil
	}
	if (*f.cert == "") != (*f.key == "") {
		return nil, "", fmt.Errorf("-community-cert와 -community-key는 함께 지정해야 함")
	}
	ln, err := net.Listen("tcp", *f.addr)
	if err != nil {
		return nil, "", err
	}
	url := *f.url
	if url == "" {
		scheme := "http"
		if *f.cert != "" {
			scheme = "https"
		}
		host, _ := os.Hostname()
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		if h, _, err := net.SplitHostPort(*f.addr); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
			host = h
		}
		url = scheme + "://" + net.JoinHostPort(host, port)
	}
	return ln, url, nil
}

func (f communityFlags) serve(ln net.Listener, hub *room.Hub, web fs.FS, logger *log.Logger) (*http.Server, error) {
	allow, err := server.ParseAllow(*f.allow)
	if err != nil {
		return nil, err
	}
	// No write timeout: a job poll holds the response for up to a minute.
	srv := &http.Server{Handler: server.NewCommunity(hub, allow, web), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10, ErrorLog: logger}
	go func() {
		var err error
		if *f.cert != "" {
			err = srv.ServeTLS(ln, *f.cert, *f.key)
		} else {
			logger.Printf("community listener uses plain HTTP: connection tokens cross the network unencrypted (use -community-cert/-community-key outside a trusted LAN)")
			err = srv.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Printf("community listener: %v", err)
		}
	}()
	return srv, nil
}

// runCommunity is `roundtable community worker`: this PC runs one remote seat.
func runCommunity(args []string) int {
	const use = "사용법: roundtable community worker --url http://<호스트>:<포트> --cli claude|codex|agy|opencode [--token-file 파일] [--model 모델] [--effort 수준] [--mode chat|read|write] [--workspace 폴더] [--once]\n  토큰은 --token-file 또는 환경변수 ROUNDTABLE_COMMUNITY_TOKEN 으로 준다(명령줄에 직접 쓰지 않음)."
	if len(args) == 0 || args[0] != "worker" {
		fmt.Fprintln(os.Stderr, use)
		return 2
	}
	fs := flag.NewFlagSet("community worker", flag.ContinueOnError)
	url := fs.String("url", "", "community server address from the host")
	tokenFile := fs.String("token-file", "", "file holding the seat's connection token")
	cli := fs.String("cli", "", "CLI this PC runs: claude | codex | agy | opencode")
	cliPath := fs.String("cli-path", "", "CLI command name or path (default the --cli name)")
	model := fs.String("model", "", "model for the CLI (empty = the CLI default)")
	effort := fs.String("effort", "", "reasoning effort (empty = the CLI default)")
	mode := fs.String("mode", agent.ModeChat, "most this PC allows: chat | read | write; a room's mode above it runs at this mode")
	workspace := fs.String("workspace", "", "folder on this PC for read and write")
	once := fs.Bool("once", false, "stop after one turn")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *url == "" || *cli == "" {
		fmt.Fprintln(os.Stderr, use)
		return 2
	}
	token := strings.TrimSpace(os.Getenv("ROUNDTABLE_COMMUNITY_TOKEN"))
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "토큰 파일:", err)
			return 1
		}
		token = strings.TrimSpace(string(b))
	}
	command := *cliPath
	if command == "" {
		command = *cli
	}
	var prov agent.Provider
	switch *cli {
	case "claude":
		prov = agent.NewClaude(command)
	case "codex":
		codex := agent.NewCodex(command)
		codex.SetWindowsSandbox("")
		prov = codex
	case "agy":
		prov = agent.NewAgy(command, "")
	case "opencode":
		prov = agent.NewOpenCode(command)
	default:
		fmt.Fprintln(os.Stderr, "알 수 없는 --cli:", *cli)
		return 2
	}
	if *workspace != "" {
		abs, err := filepath.Abs(*workspace)
		if err != nil {
			fmt.Fprintln(os.Stderr, "작업 폴더:", err)
			return 1
		}
		*workspace = abs
	}
	scratch := filepath.Join(os.TempDir(), "roundtable-worker")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "임시 폴더:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	err := worker.Run(ctx, worker.Config{URL: *url, Token: token, Provider: prov, Model: *model, Effort: *effort, Workspace: *workspace, Scratch: scratch, MaxMode: *mode, Once: *once, Client: &http.Client{Timeout: 2 * room.MaxRemoteWait}, Log: logger})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
