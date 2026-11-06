package rtmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func deliveryPage(t *testing.T, state, turn string) *page {
	t.Helper()
	var p page
	raw := `{"room":{"id":"r1"},"messages":[` +
		`{"id":"u1","seq":1,"kind":"user","text":"original"},` +
		`{"id":"a1","seq":2,"kind":"agent","author":"s1","status":"done","turn_id":"exact","text":"target answer"},` +
		`{"id":"a2","seq":3,"kind":"agent","author":"s1","status":"done","turn_id":"unrelated","text":"unrelated answer"},` +
		`{"id":"u2","seq":4,"kind":"user","author":"user","target":"s1","text":"input","delivery":{"state":"` + state + `","turn_id":"` + turn + `","detail":"history unavailable"}}]}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestReadFollowsTheReceiptTurnNotTheNearestReply(t *testing.T) {
	s := &Server{}
	r, err := s.readSelected(context.Background(), "r1", 4, 4, 100, deliveryPage(t, "accepted", "exact"), nil, 4)
	if err != nil || len(r.Messages) != 1 || r.Messages[0].Text != "target answer" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestWaitReadExposesFailedInputAtItsCursor(t *testing.T) {
	for _, budget := range []int64{1, 100} {
		s := &Server{}
		r, err := s.readSelected(context.Background(), "r1", 4, 4, budget, deliveryPage(t, "failed", "exact"), nil, 4)
		if err != nil {
			t.Fatal(err)
		}
		visible := false
		for _, m := range r.Messages {
			visible = visible || (m.Seq == 4 && m.Delivery != nil && m.Delivery.State == "failed")
		}
		if !visible {
			t.Fatalf("wait returned only an old answer and hid the failed input: %+v", r)
		}
	}
}

func TestReadMissingReceiptTurnIsExplicit(t *testing.T) {
	s := &Server{}
	_, err := s.readSelected(context.Background(), "r1", 4, 4, 100, deliveryPage(t, "accepted", "missing"), nil, 4)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("%v", err)
	}
}
