// Package indicator recolors the speaker's own LED ring animation to show the route of a voice request.
//
// The firmware draws its animations by opening each sysfs brightness file per frame. While a colored
// route is active, the ring's brightness files are covered by bind-mounted tmpfs stand-ins: the firmware
// keeps animating into them, and the indicator copies every frame to the real LEDs in the route color,
// keeping the firmware's brightness and timing. When the firmware animation ends or an uncolored route
// arrives, the stand-ins are unmounted and the firmware drives the ring again.
package indicator

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultRoot is where Linux exposes LED class devices.
	DefaultRoot = "/sys/class/leds"
	// DefaultStandIns must be on tmpfs: the firmware writes there on every animation frame.
	DefaultStandIns = "/tmp/quasar-bridge-ring"
	frame           = 20 * time.Millisecond
)

type Config struct {
	Enabled bool `json:"enabled"`
	// Colors maps a route to #rrggbb; an empty or missing color leaves the firmware's own colors.
	Colors map[string]string `json:"colors"`
	// IdleMS ends recoloring once the firmware has kept the ring dark this long.
	IdleMS int `json:"idle_ms"`
	// MaxMS caps one recoloring period, in case the firmware never darkens the ring.
	MaxMS int `json:"max_ms"`
}

func DefaultConfig() Config {
	return Config{Colors: map[string]string{"llm": "#d97757", "home": "#18bcf2", "music": "#00c8b4"}, IdleMS: 600, MaxMS: 60000}
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (c Config) Validate() error {
	if c.IdleMS < 100 || c.IdleMS > 10000 || c.MaxMS < 1000 || c.MaxMS > 600000 {
		return errors.New("invalid route_indicator idle_ms or max_ms")
	}
	for route, color := range c.Colors {
		switch route {
		case "yandex", "llm", "home", "music", "playback":
		default:
			return errors.New("invalid route_indicator route " + strconv.Quote(route))
		}
		if color != "" && !hexColor.MatchString(color) {
			return errors.New("invalid route_indicator color for " + route)
		}
	}
	return nil
}

type mounter interface {
	Bind(source, target string) error
	Unbind(target string) error
}

// channel is one LED brightness file, its stand-in and an fd opened before any stand-in covers it.
type channel struct {
	path, standIn string
	real          *os.File
	level         int // last level read from the stand-in
	written       int // last value written to the real LED, -1 when unknown
}

type Ring struct {
	zones  [][3]*channel
	colors map[string][3]int
	idle   time.Duration
	max    time.Duration
	mount  mounter

	mu     sync.Mutex
	route  string
	signal chan struct{}
	done   chan struct{}
	exited chan struct{}
	once   sync.Once
}

var redRing = regexp.MustCompile(`^(.+):red:ring([0-9]+)$`)

// New returns nil when the indicator is disabled or the host has no RGB ring under root.
func New(cfg Config, root, standIns string) (*Ring, error) {
	return newRing(cfg, root, standIns, sysMounter{})
}

func newRing(cfg Config, root, standIns string, m mounter) (*Ring, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	type found struct {
		index int
		paths [3]string
	}
	var zones []found
	matches, _ := filepath.Glob(filepath.Join(root, "*:red:ring*"))
	for _, red := range matches {
		match := redRing.FindStringSubmatch(filepath.Base(red))
		if match == nil {
			continue
		}
		index, _ := strconv.Atoi(match[2])
		paths := [3]string{filepath.Join(red, "brightness")}
		for i, c := range []string{"green", "blue"} {
			paths[i+1] = filepath.Join(root, match[1]+":"+c+":ring"+match[2], "brightness")
		}
		if exists(paths[0]) && exists(paths[1]) && exists(paths[2]) {
			zones = append(zones, found{index, paths})
		}
	}
	if len(zones) == 0 {
		return nil, nil
	}
	slices.SortFunc(zones, func(a, b found) int { return a.index - b.index })
	if err := os.MkdirAll(standIns, 0o755); err != nil {
		return nil, err
	}
	r := &Ring{colors: map[string][3]int{}, idle: time.Duration(cfg.IdleMS) * time.Millisecond, max: time.Duration(cfg.MaxMS) * time.Millisecond, mount: m, signal: make(chan struct{}, 1), done: make(chan struct{}), exited: make(chan struct{})}
	for route, color := range cfg.Colors {
		if color != "" {
			v, _ := strconv.ParseUint(color[1:], 16, 32)
			r.colors[route] = [3]int{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
		}
	}
	for _, z := range zones {
		var zone [3]*channel
		for i, path := range z.paths {
			// A crashed run can leave a stand-in mounted; the real file must be reachable before opening it.
			_ = m.Unbind(path)
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				r.closeFiles()
				return nil, err
			}
			zone[i] = &channel{path: path, standIn: filepath.Join(standIns, strings.ReplaceAll(filepath.Base(filepath.Dir(path)), ":", "_")), real: f, written: -1}
		}
		r.zones = append(r.zones, zone)
	}
	go r.run()
	return r, nil
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

// Route matches bridge.Native.OnRoute, which runs under the session lock: it only records the route.
func (r *Ring) Route(_, route string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.route = route
	r.mu.Unlock()
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// Off unmounts any stand-ins and stops the indicator, so a stopping bridge leaves the ring to the firmware.
func (r *Ring) Off() {
	if r == nil {
		return
	}
	r.once.Do(func() { close(r.done) })
	<-r.exited
	r.closeFiles()
}

func (r *Ring) closeFiles() {
	for _, z := range r.zones {
		for _, c := range z {
			if c != nil && c.real != nil {
				c.real.Close()
			}
		}
	}
}

func (r *Ring) run() {
	defer close(r.exited)
	var (
		ticker           *time.Ticker
		tick             <-chan time.Time
		color            [3]int
		started, lastLit time.Time
	)
	stop := func() {
		if ticker != nil {
			ticker.Stop()
			ticker, tick = nil, nil
			r.release()
		}
	}
	defer stop()
	for {
		select {
		case <-r.done:
			return
		case <-r.signal:
			r.mu.Lock()
			route := r.route
			r.mu.Unlock()
			c, lit := r.colors[route]
			if !lit {
				stop()
				continue
			}
			color = c
			started, lastLit = time.Now(), time.Now()
			if ticker == nil {
				if err := r.takeover(); err != nil {
					slog.Warn("route_indicator_takeover_failed", "error", err)
					continue
				}
				ticker = time.NewTicker(frame)
				tick = ticker.C
			}
		case now := <-tick:
			if r.mirror(color) {
				lastLit = now
			}
			if now.Sub(lastLit) >= r.idle || now.Sub(started) >= r.max {
				stop()
			}
		}
	}
}

// takeover covers every ring brightness file with a stand-in that starts at the current value.
func (r *Ring) takeover() error {
	var bound []*channel
	for _, z := range r.zones {
		for _, c := range z {
			value, _ := os.ReadFile(c.path)
			err := os.WriteFile(c.standIn, value, 0o644)
			if err == nil {
				err = r.mount.Bind(c.standIn, c.path)
			}
			if err != nil {
				for _, b := range bound {
					_ = r.mount.Unbind(b.path)
				}
				return err
			}
			c.level, _ = readLevel(c.standIn)
			c.written = -1
			bound = append(bound, c)
		}
	}
	return nil
}

func (r *Ring) release() {
	for _, z := range r.zones {
		for _, c := range z {
			if err := r.mount.Unbind(c.path); err != nil {
				slog.Warn("route_indicator_release_failed", "path", c.path, "error", err)
			}
		}
	}
}

// mirror repaints the firmware's latest frame in color and reports whether any zone was lit.
func (r *Ring) mirror(color [3]int) bool {
	lit := false
	for _, z := range r.zones {
		level := 0
		for _, c := range z {
			// The firmware truncates before writing; an empty read keeps the previous level.
			if v, ok := readLevel(c.standIn); ok {
				c.level = v
			}
			level = max(level, c.level)
		}
		lit = lit || level > 0
		for i, c := range z {
			v := (color[i]*level + 127) / 255
			if v == c.written {
				continue
			}
			// Zero padding keeps the value parseable whatever was in the file before; sysfs ignores it.
			if _, err := c.real.WriteAt(fmt.Appendf(nil, "%03d\n", v), 0); err == nil {
				c.written = v
			}
		}
	}
	return lit
}

func readLevel(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return min(max(v, 0), 255), true
}
