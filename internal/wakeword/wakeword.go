// Package wakeword asks a local custom wake word detector
// (github.com/tangenx/station-wakeword) which word opened a voice request.
// The detector wakes Alice like the button does, so the word itself never
// reaches the ASR text; its recent hits are the only record of it.
package wakeword

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Config struct {
	Enabled   bool   `json:"enabled"`
	URL       string `json:"url"`
	WindowMS  int    `json:"window_ms"`
	TimeoutMS int    `json:"timeout_ms"`
}

func DefaultConfig() Config {
	return Config{URL: "http://127.0.0.1:8780/api/state", WindowMS: 3000, TimeoutMS: 300}
}
func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("invalid wake_word url")
	}
	if c.WindowMS < 100 || c.WindowMS > 10000 || c.TimeoutMS < 10 || c.TimeoutMS > 2000 {
		return errors.New("invalid wake_word window_ms or timeout_ms")
	}
	return nil
}

// skew tolerates a hit stamped just after the request arrived.
const skew = 500 * time.Millisecond

type Client struct {
	url    string
	window time.Duration
	http   *http.Client

	mu sync.Mutex
	// claimed is the time of the last hit given to a request: each hit opens
	// at most one request. Times survive a detector restart, its hit ids do not.
	claimed time.Time
}

func New(cfg Config) *Client {
	return &Client{url: cfg.URL, window: time.Duration(cfg.WindowMS) * time.Millisecond, http: &http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond}}
}

type hit struct {
	Word  string    `json:"word"`
	At    time.Time `json:"at"`
	Fired bool      `json:"fired"`
}

// Word returns the id of the custom word that woke the speaker for a voice
// request received at `at` and claims that hit. It returns "" when the stock
// spotter did, or when the detector does not answer in time.
func (c *Client) Word(ctx context.Context, at time.Time) string {
	hits, err := c.hits(ctx)
	if err != nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := pick(hits, at, c.window, c.claimed)
	if h == nil {
		return ""
	}
	c.claimed = h.At
	return h.Word
}
func (c *Client) hits(ctx context.Context) ([]hit, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("wake word detector status " + resp.Status)
	}
	var state struct {
		Hits []hit `json:"hits"`
	}
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), &state); err != nil {
		return nil, err
	}
	return state.Hits, nil
}

// pick takes the newest unclaimed fired hit within window before `at`.
func pick(hits []hit, at time.Time, window time.Duration, claimed time.Time) *hit {
	var best *hit
	for i := range hits {
		h := &hits[i]
		if !h.Fired || h.Word == "" || !h.At.After(claimed) || h.At.Before(at.Add(-window)) || h.At.After(at.Add(skew)) {
			continue
		}
		if best == nil || h.At.After(best.At) {
			best = h
		}
	}
	return best
}
