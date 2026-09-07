package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoutes(t *testing.T) {
	s := New(":0", testToken, nil) // nil watcher: only static + auth-rejected paths hit
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	// Static frontend is served at /.
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", res.StatusCode)
	}
	buf := make([]byte, 512)
	n, _ := res.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "telegram-web-app.js") {
		t.Fatal("index.html not served (no Telegram SDK reference)")
	}

	// API without initData → 401 (must happen before any watcher access).
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/meta"},
		{"GET", "/api/trains?dir=tb&date=2030-01-01"},
		{"GET", "/api/searches"},
		{"POST", "/api/searches"},
		{"DELETE", "/api/searches/1"},
		{"POST", "/api/searches/1/check"},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, res.StatusCode)
		}
	}
}
