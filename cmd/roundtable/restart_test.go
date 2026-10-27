package main

import (
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallRenamesTheExistingCopy(t *testing.T) {
	dir := t.TempDir()
	src, dest := filepath.Join(dir, "new.exe"), filepath.Join(dir, "bin", "roundtable.exe")
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{src: "새 버전", dest: "옛 버전", dest + ".old-1": "더 옛 버전"} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	kept, err := install(src, dest)
	if err != nil || len(kept) != 0 {
		t.Fatalf("install: %v kept %v", err, kept)
	}
	if b, _ := os.ReadFile(dest); string(b) != "새 버전" {
		t.Fatalf("dest holds %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "bin", "*")); len(left) != 1 {
		t.Fatalf("unused earlier copies must be removed: %v", left)
	}
	if _, err := install(src, src); err == nil || !strings.Contains(err.Error(), "자신") {
		t.Fatalf("install onto itself: %v", err)
	}
}

// child runs main() with the given arguments when this test binary is started
// by TestRestartReplacesTheServerWhenIdle.
func TestRestartChild(t *testing.T) {
	args := os.Getenv("ROUNDTABLE_TEST_ARGS")
	if args == "" {
		t.Skip("child process only")
	}
	flag.CommandLine = flag.NewFlagSet("roundtable", flag.ExitOnError)
	os.Args = append([]string{"roundtable"}, strings.Split(args, "\x1f")...)
	main()
}

func startChild(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestRestartChild$")
	cmd.Env = append(os.Environ(), "ROUNDTABLE_TEST_ARGS="+strings.Join(args, "\x1f"))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func waitUp(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url + "/api/rooms"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not answer", url)
}

func TestRestartReplacesTheServerWhenIdle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	url := "http://" + addr
	old := startChild(t, "-addr", addr, "-data", t.TempDir())
	waitUp(t, url)
	exited := make(chan error, 1)
	go func() { exited <- old.Wait() }()
	startChild(t, "restart", "--url", url, "--timeout", "10s")
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("the old server did not exit")
	}
	waitUp(t, url) // the restart process now serves the same address and data
}
