package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"reflect"
	"testing"
	"testing/fstest"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

func TestTraceTailHTTP(t *testing.T) {
	store, err := room.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRoom(&room.Room{ID: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("r", &room.Message{ID: "m"}); err != nil {
		t.Fatal(err)
	}
	events := []agent.Event{
		{Kind: agent.KindSubagentStart, Parent: "a"},
		{Kind: agent.KindSubagentMeta, Parent: "a", Meta: map[string]any{"model": "test"}},
		{Kind: agent.KindText, Text: "discard"},
		{Kind: agent.KindSubagentMsg, Parent: "a", Text: "last"},
	}
	for _, event := range events {
		if err := store.AppendTrace("r", room.TraceLine{MsgID: "m", Event: event}); err != nil {
			t.Fatal(err)
		}
	}
	reg := agent.NewRegistry()
	hub, err := room.NewHub(store, reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(hub, reg, fstest.MapFS{})
	for _, query := range []string{"?tail=0", "?tail=-1", "?tail=5001", "?tail=", "?tail", "?tail=abc", "?tail=1.5", "?tail=999999999999999999999", "?tail=1&tail=2", "?tail=%zz", "?tail=1;tail=2"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://localhost/api/rooms/r/trace/m"+query, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		srv.ServeHTTP(rr, req)
		if rr.Code != 400 {
			t.Errorf("%s: status %d", query, rr.Code)
		}
	}
	for _, query := range []string{"", "?tail=1", "?tail=4", "?tail=5000"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://localhost/api/rooms/r/trace/m"+query, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		srv.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: %d %s", query, rr.Code, rr.Body)
		}
		if query == "" {
			want, _ := json.Marshal(events)
			if rr.Body.String() != string(want)+"\n" {
				t.Fatalf("legacy changed: %s", rr.Body)
			}
			continue
		}
		var page room.TraceTail
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		want := events
		if query == "?tail=1" {
			want = []agent.Event{events[0], events[1], events[3]}
		}
		if page.Total != 4 || page.Truncated != (query == "?tail=1") || !reflect.DeepEqual(page.Events, want) {
			t.Fatalf("%s: %+v", query, page)
		}
	}
	for _, path := range []string{"/api/rooms/r/trace/missing?tail=1", "/api/rooms/missing/trace/m?tail=1"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://localhost"+path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		srv.ServeHTTP(rr, req)
		if rr.Code != 500 {
			t.Fatalf("%s: %d", path, rr.Code)
		}
	}
}
