// Package indicator shows the route chosen for a voice request on the speaker's RGB LED ring.
package indicator

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// DefaultRoot is where Linux exposes LED class devices.
const DefaultRoot = "/sys/class/leds"

type Config struct {
	Enabled bool `json:"enabled"`
	// Colors maps a route to #rrggbb; an empty or missing color leaves that route unlit.
	Colors     map[string]string `json:"colors"`
	DurationMS int               `json:"duration_ms"`
}

func DefaultConfig() Config {
	return Config{Colors: map[string]string{"yandex": "#00c000", "llm": "#0050ff", "home": "#ffa000", "music": "#a000ff"}, DurationMS: 4000}
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (c Config) Validate() error {
	if c.DurationMS < 100 || c.DurationMS > 60000 {
		return errors.New("invalid route_indicator duration_ms")
	}
	for route, color := range c.Colors {
		switch route {
		case "yandex", "llm", "home", "music":
		default:
			return errors.New("invalid route_indicator route " + strconv.Quote(route))
		}
		if color != "" && !hexColor.MatchString(color) {
			return errors.New("invalid route_indicator color for " + route)
		}
	}
	return nil
}

type zone struct{ red, green, blue string }

// Ring paints a route color for a fixed time. Route only records the wanted color:
// callers hold session locks, and sysfs writes on the speaker go over I2C.
type Ring struct {
	zones    []zone
	colors   map[string][3]int
	duration time.Duration
	wake     chan struct{}

	mu    sync.Mutex
	want  [3]int
	timer *time.Timer

	paintMu sync.Mutex
	failed  bool
}

var redRing = regexp.MustCompile(`^(.+):red:ring([0-9]+)$`)

// New returns nil when the indicator is disabled or the host has no RGB ring under root.
func New(cfg Config, root string) *Ring {
	if !cfg.Enabled {
		return nil
	}
	type found struct {
		index int
		zone  zone
	}
	var zones []found
	matches, _ := filepath.Glob(filepath.Join(root, "*:red:ring*"))
	for _, red := range matches {
		m := redRing.FindStringSubmatch(filepath.Base(red))
		if m == nil {
			continue
		}
		index, _ := strconv.Atoi(m[2])
		z := zone{
			red:   filepath.Join(red, "brightness"),
			green: filepath.Join(root, m[1]+":green:ring"+m[2], "brightness"),
			blue:  filepath.Join(root, m[1]+":blue:ring"+m[2], "brightness"),
		}
		if exists(z.red) && exists(z.green) && exists(z.blue) {
			zones = append(zones, found{index, z})
		}
	}
	if len(zones) == 0 {
		return nil
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].index < zones[j].index })
	r := &Ring{colors: map[string][3]int{}, duration: time.Duration(cfg.DurationMS) * time.Millisecond, wake: make(chan struct{}, 1)}
	for _, f := range zones {
		r.zones = append(r.zones, f.zone)
	}
	for route, color := range cfg.Colors {
		if color == "" {
			continue
		}
		v, _ := strconv.ParseUint(color[1:], 16, 32)
		r.colors[route] = [3]int{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
	}
	go r.run()
	return r
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Zones reports how many RGB zones were found.
func (r *Ring) Zones() int {
	if r == nil {
		return 0
	}
	return len(r.zones)
}

// Route matches bridge.Native.OnRoute. A later request replaces the previous color.
func (r *Ring) Route(_, route string) {
	if r == nil {
		return
	}
	color, lit := r.colors[route]
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.setLocked(color)
	if !lit {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(r.duration, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.timer == timer {
			r.timer = nil
			r.setLocked([3]int{})
		}
	})
	r.timer = timer
}

func (r *Ring) setLocked(color [3]int) {
	r.want = color
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Ring) run() {
	for range r.wake {
		// Reading the color under paintMu keeps a stale paint from landing after Off.
		r.paintMu.Lock()
		r.mu.Lock()
		color := r.want
		r.mu.Unlock()
		r.paintLocked(color)
		r.paintMu.Unlock()
	}
}

// Off darkens the ring synchronously, so a stopping bridge does not leave a color behind.
func (r *Ring) Off() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.want = [3]int{}
	r.mu.Unlock()
	r.paintMu.Lock()
	defer r.paintMu.Unlock()
	r.paintLocked([3]int{})
}

func (r *Ring) paintLocked(color [3]int) {
	for _, z := range r.zones {
		for i, path := range [3]string{z.red, z.green, z.blue} {
			if err := os.WriteFile(path, []byte(strconv.Itoa(color[i])), 0o644); err != nil && !r.failed {
				r.failed = true
				slog.Warn("route_indicator_write_failed", "error", err)
			}
		}
	}
}
