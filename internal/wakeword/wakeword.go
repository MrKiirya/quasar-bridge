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
	"strings"
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
}

func New(cfg Config) *Client {
	return &Client{url: cfg.URL, window: time.Duration(cfg.WindowMS) * time.Millisecond, http: &http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond}}
}

type hit struct {
	Word  string    `json:"word"`
	At    time.Time `json:"at"`
	Fired bool      `json:"fired"`
}

// Word returns the lower-cased id of the custom word that woke the speaker
// for a voice request received at `at`. It returns "" when the stock spotter
// did, or when the detector does not answer in time.
func (c *Client) Word(ctx context.Context, at time.Time) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return ""
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var state struct {
		Hits []hit `json:"hits"`
	}
	if json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), &state) != nil {
		return ""
	}
	return pick(state.Hits, at, c.window)
}

// pick takes the newest fired hit within window before `at`.
func pick(hits []hit, at time.Time, window time.Duration) string {
	var best *hit
	for i := range hits {
		h := &hits[i]
		if !h.Fired || h.Word == "" || h.At.Before(at.Add(-window)) || h.At.After(at.Add(skew)) {
			continue
		}
		if best == nil || h.At.After(best.At) {
			best = h
		}
	}
	if best == nil {
		return ""
	}
	return strings.ToLower(best.Word)
}
