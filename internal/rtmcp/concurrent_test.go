package rtmcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestEOFCancelsActiveWaitAndHTTPObservation(t *testing.T) {
	for _, slowRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked-read=%t", slowRead), func(t *testing.T) {
			f, s := newWaitAPI(t)
			defer close(f.events) // release a regressed server even if the EOF assertion fails
			f.finishAt, f.slowFirst = 100000, slowRead
			in, send := io.Pipe()
			defer send.Close()
			var out bytes.Buffer
			done := make(chan error, 1)
			go func() { done <- s.Serve(in, &out) }()
			if _, err := fmt.Fprintln(send, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rt_post","arguments":{"room":"r1","text":"x","wait_seconds":600}}}`); err != nil {
				t.Fatal(err)
			}
			select {
			case <-f.polled:
			case <-time.After(2 * time.Second):
				t.Fatal("post-submission observation did not start")
			}
			// EOF alone must end the wait; no cancellation notification is sent.
			if err := send.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("EOF did not cancel the 600-second wait")
			}
			select {
			case <-f.streamClosed:
			case <-time.After(2 * time.Second):
				t.Fatal("EOF left the SSE connection open")
			}
			if out.Len() != 0 {
				t.Fatalf("cancelled call wrote a response after EOF: %s", out.String())
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.posts != 1 || len(f.queue) != 1 {
				t.Fatal("EOF must not cancel the underlying room turn")
			}
		})
	}
}

func TestCancellationLeavesOtherRequestsResponsive(t *testing.T) {
	f, s := newWaitAPI(t)
	f.finishAt = 100000
	in, send := io.Pipe()
	out, receive := io.Pipe()
	defer send.Close()
	defer out.Close()
	defer receive.Close()
	done := make(chan error, 1)
	go func() { done <- s.Serve(in, receive) }()
	responses := make(chan map[string]any, 16)
	go func() {
		decoder := json.NewDecoder(out)
		for {
			var reply map[string]any
			if decoder.Decode(&reply) != nil {
				return
			}
			responses <- reply
		}
	}()
	write := func(line string) {
		t.Helper()
		if _, err := fmt.Fprintln(send, line); err != nil {
			t.Fatal(err)
		}
	}
	next := func() map[string]any {
		t.Helper()
		select {
		case reply := <-responses:
			return reply
		case <-time.After(2 * time.Second):
			t.Fatal("stdio blocked behind a long call")
			return nil
		}
	}
	write(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rt_post","arguments":{"room":"r1","text":"x","wait_seconds":600}}}`)
	select {
	case <-f.posted:
	case <-time.After(2 * time.Second):
		t.Fatal("post did not start")
	}
	write(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if r := next(); r["id"] != float64(2) || r["error"] != nil {
		t.Fatal(r)
	}
	write(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rt_list_rooms","arguments":{}}}`)
	if r := next(); r["id"] != float64(3) || r["error"] != nil {
		t.Fatal(r)
	}
	// A string ID cannot cancel a numeric ID with the same displayed value.
	write(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"1"}}`)
	write(`{"jsonrpc":"2.0","id":4,"method":"ping"}`)
	if r := next(); r["id"] != float64(4) {
		t.Fatal(r)
	}
	select {
	case <-f.streamClosed:
		t.Fatal("wrong id cancelled the post")
	default:
	}
	write(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	select {
	case <-f.streamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close HTTP observation")
	}
	// Reuse is safe: a late result from the cancelled worker must not answer it.
	write(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if r := next(); r["id"] != float64(1) || r["error"] != nil {
		t.Fatal(r)
	}
	_ = send.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not finish after cancellation and EOF")
	}
	select {
	case r := <-responses:
		t.Fatalf("cancelled request sent a response: %v", r)
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || len(f.queue) != 1 {
		t.Fatal("MCP cancellation must not cancel the underlying agent turn")
	}
}

func TestStringCancellationIDUsesDecodedValue(t *testing.T) {
	if rpcIDKey(json.RawMessage(`"a"`)) != rpcIDKey(json.RawMessage(`"\u0061"`)) {
		t.Fatal("escaped string ids differ")
	}
	if rpcIDKey(json.RawMessage(`"1"`)) == rpcIDKey(json.RawMessage(`1`)) {
		t.Fatal("string and numeric ids collide")
	}
	if rpcIDKey(json.RawMessage(`-0`)) != rpcIDKey(json.RawMessage(`0`)) {
		t.Fatal("equivalent numeric ids differ")
	}
}
