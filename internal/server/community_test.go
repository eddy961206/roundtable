package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

func communityHub(t *testing.T) (*room.Hub, *room.Room) {
	t.Helper()
	store, _ := room.NewStore(t.TempDir())
	reg := agent.NewRegistry(echoProvider{}, room.NewRemoteProvider())
	hub, err := room.NewHub(store, reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })
	r, err := hub.CreateRoom(room.RoomSpec{Title: "휴게실", Kind: room.KindLounge, ParticipantSpecs: []room.ParticipantSpec{{Provider: room.RemoteProvider, Label: "eddy"}, {Provider: "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	return hub, r
}

// The community listener only answers token holders from allowed addresses,
// and never serves the local /api.
func TestCommunityListenerGuards(t *testing.T) {
	hub, r := communityHub(t)
	token, err := hub.IssueRemoteToken(r.ID, r.Participants[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	allow, err := ParseAllow("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	c := NewCommunity(hub, allow, fstest.MapFS{"people.html": {Data: []byte("<html>people</html>")}})
	send := func(remote, method, path, token string, headers ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		c.ServeHTTP(w, req)
		return w
	}
	w1 := []string{"X-RT-Worker", "w1"}
	cases := []struct {
		name, remote, method, path, token string
		headers                           []string
		want                              int
	}{
		{"outside the allow list", "203.0.113.5:1000", "GET", "/community/v1/me", token, nil, 403},
		{"no token", "10.1.2.3:1000", "GET", "/community/v1/me", "", nil, 401},
		{"wrong token", "10.1.2.3:1000", "GET", "/community/v1/me", "rtc_nope", nil, 401},
		{"a browser page", "10.1.2.3:1000", "GET", "/community/v1/me", token, []string{"Origin", "http://evil.example"}, 403},
		{"the local api", "10.1.2.3:1000", "GET", "/api/rooms", token, nil, 404},
		{"seat info", "10.1.2.3:1000", "GET", "/community/v1/me", token, nil, 200},
		{"a poll without a worker id", "10.1.2.3:1000", "GET", "/community/v1/job?wait=0", token, nil, 400},
		{"no job yet", "10.1.2.3:1000", "GET", "/community/v1/job?wait=0", token, w1, 204},
		{"too long a poll", "10.1.2.3:1000", "GET", "/community/v1/job?wait=600", token, w1, 400},
		{"poll seconds that overflow a duration", "10.1.2.3:1000", "GET", "/community/v1/job?wait=18446744074", token, w1, 400},
		{"a result for no turn", "10.1.2.3:1000", "POST", "/community/v1/job/t1/result", token, w1, 409},
	}
	for _, tc := range cases {
		if got := send(tc.remote, tc.method, tc.path, tc.token, tc.headers...); got.Code != tc.want {
			t.Errorf("%s: %d %s", tc.name, got.Code, got.Body)
		}
	}
	if got := send("10.1.2.3:1000", "GET", "/community/v1/me", token); !strings.Contains(got.Body.String(), `"seat":"`+r.Participants[0].ID) {
		t.Fatal(got.Body)
	}
	if _, err := ParseAllow(" , "); err == nil {
		t.Fatal("an empty allow list was accepted")
	}
}

// The local API issues tokens and assigns tasks; the token is shown once.
func TestTaskAndRemoteTokenRoutes(t *testing.T) {
	hub, r := communityHub(t)
	srv := New(hub, agent.NewRegistry(echoProvider{}, room.NewRemoteProvider()), fstest.MapFS{"index.html": {Data: []byte("ok")}})
	srv.SetCommunityURL("http://host:8790")
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	code, body := call(t, ts, "POST", "/api/rooms/"+r.ID+"/participants/"+r.Participants[0].ID+"/remote-token", map[string]any{})
	if code != 201 || !strings.HasPrefix(body["token"].(string), "rtc_") || body["community_url"] != "http://host:8790" {
		t.Fatalf("%d %v", code, body)
	}
	if code, body := call(t, ts, "POST", "/api/rooms/"+r.ID+"/participants/"+r.Participants[1].ID+"/remote-token", map[string]any{}); code != 400 {
		t.Fatalf("a CLI seat got a token: %d %v", code, body)
	}
	code, body = call(t, ts, "POST", "/api/rooms/"+r.ID+"/tasks", map[string]any{"to": r.Participants[1].ID, "text": "인사하기"})
	if code != 201 || body["request"].(map[string]any)["from"] != "user" {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := call(t, ts, "GET", "/api/config", nil); code != 200 {
		t.Fatal(code)
	}
	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"community_url":"http://host:8790"`) {
		t.Fatal(string(b))
	}
}
