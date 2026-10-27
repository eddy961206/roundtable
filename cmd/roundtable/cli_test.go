package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 2026-10-27: "roundtable version" started a server on 8787 and
// "roundtable install --help" copied the binary to a file named "--help".
func TestUnknownSubcommandIsRefused(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"serve"}, {"instal", "x.exe"}} {
		code, serve, out := dispatch(args)
		if serve || code != 2 || !strings.Contains(out, "알 수 없는 하위 명령") || !strings.Contains(out, "install") {
			t.Errorf("%v: code=%d serve=%v out=%q", args, code, serve, out)
		}
	}
	for _, args := range [][]string{nil, {"-addr", "127.0.0.1:0"}, {"-open"}} {
		if _, serve, _ := dispatch(args); !serve {
			t.Errorf("%v must start the server", args)
		}
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		if code, serve, out := dispatch(args); serve || code != 0 || !strings.Contains(out, "restart") {
			t.Errorf("%v: code=%d serve=%v out=%q", args, code, serve, out)
		}
	}
}

func TestInstallHelpCopiesNothing(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		if code := runInstall(args); code != 0 {
			t.Errorf("%v: code %d", args, code)
		}
	}
	if code := runInstall([]string{"--force", "x.exe"}); code != 2 {
		t.Errorf("unknown option accepted: %d", code)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("install created files: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "--help")); !os.IsNotExist(err) {
		t.Fatal("--help was written as a file")
	}
}
