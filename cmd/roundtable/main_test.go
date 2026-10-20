package main

import (
	"bytes"
	"flag"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenConflictDoesNotRecoverRooms(t *testing.T) {
	if os.Getenv("ROUNDTABLE_TEST_LISTEN_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet("roundtable", flag.ExitOnError)
		os.Args = []string{"roundtable", "-addr", os.Getenv("ROUNDTABLE_TEST_ADDR"), "-data", os.Getenv("ROUNDTABLE_TEST_DATA")}
		main()
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	data := t.TempDir()
	dir := filepath.Join(data, "rooms", "r1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	room := []byte(`{"id":"r1","seq":1,"participants":[]}`)
	messages := []byte("{\"id\":\"m1\",\"seq\":1,\"kind\":\"agent\",\"status\":\"streaming\"}\n")
	for name, content := range map[string][]byte{"room.json": room, "messages.jsonl": messages} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestListenConflictDoesNotRecoverRooms$")
	cmd.Env = append(os.Environ(), "ROUNDTABLE_TEST_LISTEN_CHILD=1", "ROUNDTABLE_TEST_ADDR="+ln.Addr().String(), "ROUNDTABLE_TEST_DATA="+data)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "listen "+ln.Addr().String()+":") {
		t.Fatalf("wanted listen failure, got %v: %s", err, out)
	}
	for name, want := range map[string][]byte{"room.json": room, "messages.jsonl": messages} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("startup mutated %s: %s (%v)", name, got, err)
		}
	}
}
