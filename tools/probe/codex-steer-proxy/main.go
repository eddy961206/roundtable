// codex-steer-proxy is an opt-in fault injector for an isolated rt server.
// It always delegates inference to the configured real Codex executable.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	real, dir := os.Getenv("ROUNDTABLE_CODEX_PROBE_REAL"), os.Getenv("ROUNDTABLE_CODEX_PROBE_DIR")
	if !filepath.IsAbs(real) || !filepath.IsAbs(dir) {
		fmt.Fprintln(os.Stderr, "ROUNDTABLE_CODEX_PROBE_REAL and ROUNDTABLE_CODEX_PROBE_DIR must be absolute paths")
		os.Exit(2)
	}
	modeBytes, err := os.ReadFile(filepath.Join(dir, "mode"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	mode := strings.TrimSpace(string(modeBytes))
	if mode == "missing-appserver" && len(os.Args) > 1 && os.Args[1] == "app-server" {
		fmt.Fprintln(os.Stderr, "error: unrecognized subcommand 'app-server' (injected)")
		os.Exit(2)
	}
	cmd := exec.Command(real, os.Args[1:]...)
	hideWindow(cmd)
	cmd.Env, cmd.Stderr = os.Environ(), os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		panic(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	if err = cmd.Start(); err != nil {
		panic(err)
	}
	log, err := os.OpenFile(filepath.Join(dir, "wire-"+strconv.Itoa(os.Getpid())+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		panic(err)
	}
	defer log.Close()
	var logMu, outputMu, requestsMu sync.Mutex
	requests := map[string]string{}
	write := func(line []byte) {
		outputMu.Lock()
		defer outputMu.Unlock()
		_, _ = os.Stdout.Write(append(line, '\n'))
	}
	record := func(direction string, packet map[string]any) {
		logMu.Lock()
		defer logMu.Unlock()
		_ = json.NewEncoder(log).Encode(map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "direction": direction, "packet": packet, "pid": os.Getpid(), "child": cmd.Process.Pid})
	}
	gate := func(kind string, packet map[string]any) {
		b, _ := json.Marshal(map[string]any{"kind": kind, "pid": os.Getpid(), "child": cmd.Process.Pid, "packet": packet, "at": time.Now().UTC().Format(time.RFC3339Nano)})
		_ = os.WriteFile(filepath.Join(dir, "gate.json"), b, 0600)
	}
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		defer in.Close()
		s := bufio.NewScanner(os.Stdin)
		s.Buffer(make([]byte, 65536), 32<<20)
		for s.Scan() {
			line := append([]byte(nil), s.Bytes()...)
			var p map[string]any
			if err := json.Unmarshal(line, &p); err != nil {
				panic(err)
			}
			record("client", p)
			method, _ := p["method"].(string)
			if method != "" && p["id"] != nil {
				requestsMu.Lock()
				requests[fmt.Sprint(p["id"])] = method
				requestsMu.Unlock()
			}
			if method == "turn/steer" && mode == "unsupported-steer" {
				response := map[string]any{"id": p["id"], "error": map[string]any{"code": -32601, "message": "method not found: turn/steer (injected)"}}
				record("injected", response)
				gate("unsupported", p)
				b, _ := json.Marshal(response)
				write(b)
				continue
			}
			if _, err := in.Write(append(line, '\n')); err != nil {
				return
			}
		}
	}()
	s := bufio.NewScanner(out)
	s.Buffer(make([]byte, 65536), 32<<20)
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		var p map[string]any
		if err := json.Unmarshal(line, &p); err != nil {
			panic(err)
		}
		record("server", p)
		if p["method"] == "turn/completed" && mode == "delay-completed" {
			gate("completed", p)
			time.Sleep(1500 * time.Millisecond)
		}
		if mode == "hold-steer-reply" && p["method"] == nil && p["id"] != nil {
			requestsMu.Lock()
			method := requests[fmt.Sprint(p["id"])]
			requestsMu.Unlock()
			if method == "turn/steer" {
				gate("steer-reply", p)
				select {
				case <-eof:
				case <-time.After(25 * time.Second):
				}
			}
		}
		write(line)
	}
	_ = out.Close()
	if err := cmd.Wait(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
