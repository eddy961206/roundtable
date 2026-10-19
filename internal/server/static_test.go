package server

import (
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestStaticJSContentType(t *testing.T) {
	// Windows can register .js as text/plain, and the mime package then reports it.
	prev := mime.TypeByExtension(".js")
	if err := mime.AddExtensionType(".js", "text/plain; charset=utf-8"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mime.AddExtensionType(".js", prev); err != nil {
			t.Error(err)
		}
	})

	web := fstest.MapFS{
		"index.html": {Data: []byte("<html>ok</html>")},
		"js/main.js": {Data: []byte("export {};\n")},
	}
	ts := httptest.NewServer(New(nil, nil, web))
	t.Cleanup(ts.Close)

	get := func(path string) (*http.Response, string) {
		t.Helper()
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res, string(body)
	}

	res, body := get("/js/main.js")
	if res.StatusCode != 200 || body != "export {};\n" {
		t.Fatalf("module %d %q", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Fatalf("module content-type %q", ct)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("module response must keep nosniff")
	}

	res, _ = get("/")
	if ct := res.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("index content-type %q", ct)
	}

	// A missing module is an error page, not JavaScript.
	res, _ = get("/js/missing.js")
	if ct := res.Header.Get("Content-Type"); res.StatusCode != 404 || ct != "text/plain; charset=utf-8" {
		t.Fatalf("missing module %d %q", res.StatusCode, ct)
	}
}
