package room

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"roundtable/internal/agent"
)

func pngImage(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestAttachmentValidationAndOwnership(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mime    string
		data          []byte
		width, height int
	}{
		{"disguised.txt", "image/png", pngImage(t), 3, 2},
		{"wrong.png", "image/jpeg", jpg.Bytes(), 3, 2},
		{"wrong.jpg", "image/webp", webp, 1, 1},
	} {
		a, err := h.UploadAttachment(r.ID, tc.name, bytes.NewReader(tc.data))
		if err != nil {
			t.Fatalf("%s: %v", tc.mime, err)
		}
		if a.Mime != tc.mime || a.Width != tc.width || a.Height != tc.height || a.Bytes != int64(len(tc.data)) {
			t.Fatalf("%+v", a)
		}
		f, loaded, err := h.OpenAttachment(r.ID, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if loaded != a {
			t.Fatal(loaded)
		}
	}
	for _, b := range [][]byte{[]byte("not an image"), []byte("GIF89a"), bytes.Repeat([]byte{0}, int(MaxAttachmentBytes+1)), []byte("RIFF1234WEBP")} {
		if _, err := h.UploadAttachment(r.ID, "fake.png", bytes.NewReader(b)); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	a, err := h.UploadAttachment(r.ID, "ok.png", bytes.NewReader(pngImage(t)))
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.CreateRoom(RoomSpec{Title: "other", Mode: agent.ModeChat, Participants: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.PostUser(other.ID, "cross room", "", a.ID); err == nil {
		t.Fatal("cross-room attachment accepted")
	}
	if _, err := h.PostUser(r.ID, "too many", "", a.ID, a.ID, a.ID, a.ID, a.ID); err == nil {
		t.Fatal("five attachments accepted")
	}
	if _, err := h.PostUser(r.ID, "duplicate", "", a.ID, a.ID); err == nil {
		t.Fatal("duplicate attachment accepted")
	}
	if _, err := h.PostUser(r.ID, "", "", a.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	if _, err := h.PostUser(r.ID, "reuse", "", a.ID); err == nil {
		t.Fatal("reused attachment accepted")
	}
	if _, _, err := h.OpenAttachment(r.ID, "../room"); err == nil {
		t.Fatal("ID traversal accepted")
	}
}

func TestAttachmentSourceBoundaries(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	scratch := t.TempDir()
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(workspace, "image.png")
	outside := filepath.Join(parent, "image.png")
	for _, p := range []string{inside, outside, filepath.Join(scratch, "image.png")} {
		if err := os.WriteFile(p, pngImage(t), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"image.png", inside, filepath.Join(scratch, "image.png")} {
		f, err := attachmentSource(p, workspace, scratch)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	for _, p := range []string{outside, filepath.Join("..", "image.png"), workspace} {
		if f, err := attachmentSource(p, workspace, scratch); err == nil {
			f.Close()
			t.Fatalf("escaped: %s", p)
		}
	}
	t.Run("symlink escape", func(t *testing.T) {
		link := filepath.Join(workspace, "linked.png")
		if err := os.Symlink(outside, link); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("Windows symlink creation needs Developer Mode or symbolic-link privilege: %v", err)
			}
			t.Fatal(err)
		}
		if f, err := attachmentSource(link, workspace, scratch); err == nil {
			f.Close()
			t.Fatal("symlink escape accepted")
		}
	})
}

func TestAttachmentStartupCleanup(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	old, _ := h.UploadAttachment(r.ID, "orphan.png", bytes.NewReader(pngImage(t)))
	kept, _ := h.UploadAttachment(r.ID, "bound.png", bytes.NewReader(pngImage(t)))
	fresh, _ := h.UploadAttachment(r.ID, "fresh.png", bytes.NewReader(pngImage(t)))
	if _, err := h.PostUser(r.ID, "keep", "", kept.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	past := time.Now().Add(-25 * time.Hour)
	for _, a := range []Attachment{old, kept} {
		for _, path := range []string{h.store.attachmentPath(r.ID, a), filepath.Join(h.store.attachmentDir(r.ID), a.ID+".json")} {
			if err := os.Chtimes(path, past, past); err != nil {
				t.Fatal(err)
			}
		}
	}
	restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.OpenAttachment(r.ID, old.ID); err == nil {
		t.Fatal("old orphan survived startup")
	}
	for _, a := range []Attachment{kept, fresh} {
		f, _, err := restarted.OpenAttachment(r.ID, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}

func TestAttachmentDeliveryUsesCapUnseenAndPartCursor(t *testing.T) {
	for _, partSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "room", true: "part"}[partSession], func(t *testing.T) {
			h, r, ps := controlledHub(t, "codex")
			p := r.Participants[0]
			if partSession {
				r.Work = &WorkPlan{Parts: []*Part{{ID: "p1", Seat: p.ID, State: "working", Worktree: t.TempDir(), SessionID: "part-session"}}}
				p.LastSeen = 999
			}
			a, _ := h.UploadAttachment(r.ID, "a.png", bytes.NewReader(pngImage(t)))
			if _, err := h.PostUser(r.ID, "first", "", a.ID); err != nil {
				t.Fatal(err)
			}
			first := nextCall(t, ps[0])
			if len(first.opts.Images) != 1 || !strings.Contains(first.opts.Prompt, "[첨부 "+a.ID) || !strings.Contains(first.opts.Prompt, "첨부됨") {
				t.Fatal(first.opts)
			}
			b, _ := h.UploadAttachment(r.ID, "b.png", bytes.NewReader(pngImage(t)))
			if _, err := h.PostUser(r.ID, "second", "", b.ID); err != nil {
				t.Fatal(err)
			}
			first.result <- controlledResult{text: "ok"}
			second := nextCall(t, ps[0])
			if len(second.opts.Images) != 1 || !strings.Contains(second.opts.Prompt, "[첨부 "+b.ID) || strings.Contains(second.opts.Prompt, "[첨부 "+a.ID) {
				t.Fatal(second.opts)
			}
			second.result <- controlledResult{text: "ok"}
			waitIdle(t, h, r.ID)
			if _, err := h.PostUser(r.ID, "third", ""); err != nil {
				t.Fatal(err)
			}
			third := nextCall(t, ps[0])
			if len(third.opts.Images) != 0 {
				t.Fatal(third.opts)
			}
			third.result <- controlledResult{text: "ok"}
			waitIdle(t, h, r.ID)
			if partSession && p.LastSeen != 999 {
				t.Fatal("part consumed room cursor")
			}
		})
	}
	// A streaming reply below cap is a hole, then delivered exactly once.
	p := &Participant{ID: "codex", LastSeen: 2, Unseen: []int64{1}}
	m := &Message{ID: "source", Seq: 1, Author: "claude", Status: "done", Attachments: []Attachment{{ID: "a"}}}
	rs := &roomState{msgs: []*Message{m}}
	hist, seen, unseen := rs.deltaFor(p, false, 2)
	if len(hist) != 1 || len(hist[0].Attachments) != 1 {
		t.Fatal(hist)
	}
	p.LastSeen, p.Unseen = seen, unseen
	hist, _, _ = rs.deltaFor(p, false, 2)
	if len(hist) != 0 {
		t.Fatal("hole redelivered")
	}
}

func TestAttachmentMarkersAndFeedback(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	r.Mode, r.Workspace = agent.ModeRead, t.TempDir()
	path := filepath.Join(r.Workspace, "result.png")
	if err := os.WriteFile(path, pngImage(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PostUser(r.ID, "start", ""); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	call.result <- controlledResult{text: "answer\n[[ATTACH:result.png]]\n[[ATTACH:../escape.png]]"}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	var found bool
	for _, m := range snap.Messages {
		if m.Kind == "agent" {
			if m.Status != "done" || len(m.Attachments) != 1 || !strings.Contains(m.Text, "answer") {
				t.Fatal(m)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no response")
	}
	var reply, notice *Message
	for _, m := range snap.Messages {
		switch {
		case m.Kind == "agent":
			reply = m
		case m.Kind == "system" && m.NoticeFor != "":
			notice = m
		}
	}
	if notice == nil || notice.NoticeMarker != "attach" || notice.NoticeMsg != reply.ID {
		t.Fatalf("rejection not attributed to its reply: %+v", notice)
	}
	stats, err := h.ModelStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Rows) != 1 || stats.Rows[0].MarkerRejects["attach"] != 1 || stats.UnattributedMarkerRejects != 0 {
		t.Fatalf("marker rejects not counted: %+v", stats)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, m := range snap.Messages {
		for _, a := range m.Attachments {
			f, _, err := h.OpenAttachment(r.ID, a.ID)
			if err != nil {
				t.Fatal("original was not copied", err)
			}
			f.Close()
		}
	}
	if _, err := h.PostUser(r.ID, "next", ""); err != nil {
		t.Fatal(err)
	}
	next := nextCall(t, ps[0])
	if !strings.Contains(next.opts.Prompt, "마커 거절") {
		t.Fatal("feedback not delivered")
	}
	next.result <- controlledResult{text: "ok"}
	waitIdle(t, h, r.ID)
}

func TestAttachmentMessageWaitsForNextTurnInsteadOfTextSteer(t *testing.T) {
	h, r, ps := controlledHub(t, "claude")
	ps[0].steer = true
	if _, err := h.PostUser(r.ID, "start", "claude"); err != nil {
		t.Fatal(err)
	}
	first := nextCall(t, ps[0])
	a, err := h.UploadAttachment(r.ID, "image.png", bytes.NewReader(pngImage(t)))
	if err != nil {
		t.Fatal(err)
	}
	m, err := h.PostUser(r.ID, "with image", "claude", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Live {
		t.Fatal("image was reduced to a text steer")
	}
	select {
	case <-first.opts.Steer:
		t.Fatal("text-only steer sent")
	default:
	}
	first.result <- controlledResult{text: "ok"}
	next := nextCall(t, ps[0])
	if len(next.opts.Images) != 1 || !strings.Contains(next.opts.Prompt, "경로: "+next.opts.Images[0].Path) {
		t.Fatal(next.opts)
	}
	next.result <- controlledResult{text: "ok"}
	waitIdle(t, h, r.ID)
}

func TestAttachmentFourImagesAndChatMarkerRejection(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	var ids []string
	for i := 0; i < 4; i++ {
		a, err := h.UploadAttachment(r.ID, "image.png", bytes.NewReader(pngImage(t)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	if len(ps[0].callsCh) != 0 {
		t.Fatal("upload started a turn")
	}
	if _, err := h.PostUser(r.ID, "four", "", ids...); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	if len(call.opts.Images) != 4 {
		t.Fatal(call.opts)
	}
	call.result <- controlledResult{text: "[[ATTACH:missing.png]]"}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	found := false
	for _, m := range snap.Messages {
		if m.Kind == "agent" && len(m.Attachments) != 0 {
			t.Fatal("chat attachment processed")
		}
		if m.Kind == "system" && strings.Contains(m.Text, "chat 모드") {
			found = true
		}
	}
	if !found {
		t.Fatal("no mode feedback")
	}
}
