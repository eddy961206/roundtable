package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

func TestAttachmentAPI(t *testing.T) {
	ts := newTestServer(t)
	code, rm := call(t, ts, "POST", "/api/rooms", map[string]any{"title": "images", "mode": "chat", "participants": []string{"claude"}})
	if code != 201 {
		t.Fatal(code, rm)
	}
	id := rm["id"].(string)
	base := "/api/rooms/" + id
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	upload := func(data []byte, count int) (int, map[string]any) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for i := 0; i < count; i++ {
			f, err := writer.CreateFormFile("file", "disguised.txt")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		writer.Close()
		req, _ := http.NewRequest("POST", ts.URL+base+"/attachments", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var a map[string]any
		if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, a
	}
	code, a := upload(imageData.Bytes(), 1)
	if code != 201 || a["mime"] != "image/png" || a["width"] != float64(2) {
		t.Fatal(code, a)
	}
	aid := a["id"].(string)
	res, err := http.Get(ts.URL + base + "/attachments/" + aid)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" || !bytes.Equal(b, imageData.Bytes()) {
		t.Fatal(res, err)
	}
	for _, tc := range []struct {
		data  []byte
		count int
	}{{[]byte("not png"), 1}, {bytes.Repeat([]byte{0}, 10*1024*1024+1), 1}, {imageData.Bytes(), 2}} {
		if code, _ := upload(tc.data, tc.count); code != 400 {
			t.Fatal(code)
		}
	}
	if code, _ := call(t, ts, "POST", base+"/messages", map[string]any{"attachments": []string{aid, aid, aid, aid, aid}}); code != 400 {
		t.Fatal(code)
	}
	code, other := call(t, ts, "POST", "/api/rooms", map[string]any{"title": "other", "mode": "chat", "participants": []string{"claude"}})
	if code != 201 {
		t.Fatal(code, other)
	}
	if code, _ := call(t, ts, "POST", "/api/rooms/"+other["id"].(string)+"/messages", map[string]any{"attachments": []string{aid}}); code != 400 {
		t.Fatal(code)
	}
	code, msg := call(t, ts, "POST", base+"/messages", map[string]any{"attachments": []string{aid}})
	if code != 201 || len(msg["attachments"].([]any)) != 1 {
		t.Fatal(code, msg)
	}
	if code, _ := call(t, ts, "POST", base+"/messages", map[string]any{"text": "reuse", "attachments": []string{aid}}); code != 400 {
		t.Fatal(code)
	}
	if code, _ := call(t, ts, "POST", base+"/requests/missing/cancel", nil); code != 400 {
		t.Fatal(code)
	}
}

type requestAPIProvider struct {
	echoProvider
	id, reply string
}

func (p requestAPIProvider) ID() string { return p.id }
func (p requestAPIProvider) Run(context.Context, agent.RunOptions, func(agent.Event)) (agent.Result, error) {
	return agent.Result{Text: p.reply, SessionID: "session-" + p.id}, nil
}

func TestRequestSSEAndCancelAPI(t *testing.T) {
	store, err := room.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := agent.NewRegistry(requestAPIProvider{id: "claude", reply: "[[REQUEST:@codex]] inspect"}, requestAPIProvider{id: "codex", reply: "still working"})
	hub, err := room.NewHub(store, reg, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(hub, reg, fstest.MapFS{"index.html": {Data: []byte("ok")}}))
	defer ts.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	r, err := hub.CreateRoom(room.RoomSpec{Title: "requests", Mode: agent.ModeChat, Participants: []string{"claude", "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/rooms/"+r.ID+"/stream", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if code, body := call(t, ts, "POST", "/api/rooms/"+r.ID+"/messages", map[string]any{"text": "start", "target": "claude"}); code != 201 {
		t.Fatal(code, body)
	}
	scanner := bufio.NewScanner(res.Body)
	var got room.Request
	for scanner.Scan() {
		line, ok := strings.CutPrefix(scanner.Text(), "data: {") // skip the hello timestamp
		if !ok {
			continue
		}
		var ev struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte("{"+line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "request" {
			if err := json.Unmarshal(ev.Data, &got); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if got.ID == "" || got.Status != "pending" || got.To != "codex" {
		t.Fatalf("event=%+v err=%v", got, scanner.Err())
	}
	if code, body := call(t, ts, "POST", "/api/rooms/"+r.ID+"/requests/"+got.ID+"/cancel", nil); code != 200 || body["status"] != "cancelled" {
		t.Fatal(code, body)
	}
	snap, err := hub.Snapshot(r.ID)
	if err != nil || len(snap.Requests) != 1 || snap.Requests[0].Status != "cancelled" {
		t.Fatal(snap, err)
	}
}
