// Command roundtable serves the multi-vendor agent chat room.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"roundtable"
	"roundtable/internal/agent"
	"roundtable/internal/room"
	"roundtable/internal/server"
)

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	data := flag.String("data", filepath.Join(home, ".roundtable"), "data directory")
	claudeCmd := flag.String("claude", "claude", "Claude Code command")
	codexCmd := flag.String("codex", "codex", "Codex command")
	agyCmd := flag.String("agy", "agy", "Antigravity command")
	open := flag.Bool("open", false, "open the browser after start")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)
	store, err := room.NewStore(*data)
	if err != nil {
		logger.Fatalf("data dir: %v", err)
	}
	scratch := filepath.Join(*data, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		logger.Fatalf("scratch dir: %v", err)
	}
	reg := agent.DefaultRegistry(*claudeCmd, *codexCmd, *agyCmd)
	hub, err := room.NewHub(store, reg, scratch, logger)
	if err != nil {
		logger.Fatalf("load rooms: %v", err)
	}
	web, err := fs.Sub(roundtable.WebFS, "web")
	if err != nil {
		logger.Fatal(err)
	}
	srv := server.New(hub, reg, web)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Fatalf("listen %s: %v", *addr, err)
	}
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
	if err := http.Serve(ln, srv); err != nil {
		logger.Fatal(err)
	}
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
