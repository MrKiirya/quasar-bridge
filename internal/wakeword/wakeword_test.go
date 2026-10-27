package wakeword

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWordPicksNewestFiredHitInWindow(t *testing.T) {
	at := time.Date(2026, 10, 27, 21, 55, 26, 802_000_000, time.UTC)
	body := `{"station":"x","hits":[` +
		`{"word":"Товарищ","at":"2026-10-27T21:55:26.742Z","fired":true},` +
		`{"word":"джарвис","at":"2026-10-27T21:55:25.000Z","fired":true},` +
		`{"word":"пятница","at":"2026-10-27T21:55:26.700Z","fired":false}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.URL = srv.URL
	if got := New(cfg).Word(t.Context(), at); got != "товарищ" {
		t.Fatalf("got %q", got)
	}
}

func TestPickIgnoresHitsOutsideWindow(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 0, 10, 0, time.UTC)
	for _, tc := range []struct {
		name string
		hit  hit
		want string
	}{
		{"stale", hit{Word: "товарищ", At: at.Add(-4 * time.Second), Fired: true}, ""},
		{"after request", hit{Word: "товарищ", At: at.Add(time.Second), Fired: true}, ""},
		{"skew", hit{Word: "товарищ", At: at.Add(200 * time.Millisecond), Fired: true}, "товарищ"},
		{"dry run", hit{Word: "товарищ", At: at.Add(-time.Second), Fired: false}, ""},
	} {
		if got := pick([]hit{tc.hit}, at, 3*time.Second); got != tc.want {
			t.Errorf("%s: got %q", tc.name, got)
		}
	}
}

func TestWordUnreachableDetector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.URL, cfg.TimeoutMS = srv.URL, 20
	if got := New(cfg).Word(t.Context(), time.Now()); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := DefaultConfig()
	bad.URL = "unix:///tmp/x"
	if bad.Validate() == nil {
		t.Fatal("non-http url accepted")
	}
	bad = DefaultConfig()
	bad.WindowMS = 0
	if bad.Validate() == nil {
		t.Fatal("zero window accepted")
	}
}
