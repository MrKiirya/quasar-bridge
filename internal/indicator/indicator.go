// Package indicator recolors the speaker's own LED ring animation to show the route of a voice request.
//
// The firmware draws its animations by opening each sysfs brightness file per frame. While a colored
// route is active, the ring's brightness files are covered by bind-mounted tmpfs stand-ins: the firmware
// keeps animating into them, and the indicator copies every frame to the real LEDs in the route color,
// keeping the firmware's brightness and timing. When the firmware animation ends, a new request starts
// or an uncolored route arrives, the stand-ins are unmounted and the firmware drives the ring again.
//
// On speakers whose firmware draws into a ring framebuffer (Lite 2, Midi, Mini 3 Pro), a kernel LED
// trigger copies that framebuffer to the same sysfs LEDs, so there are no file writes to intercept.
// There the indicator reads the firmware's frames from the framebuffer, detaches the triggers for
// the takeover and attaches them again on release.
package indicator

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	// Stacked stand-ins from crashed runs are peeled one mount at a time.
	maxStacked = 8
	// The firmware's Pro animations change hue more than brightness, so a recolored copy would look flat;
	// a breathing wave on top keeps the ring alive while the firmware still decides when and where it is lit.
	breathPeriod = 1600 * time.Millisecond
	breathFloor  = 0.45
)

// breathe is the share of brightness a zone gets after elapsed: it swings between breathFloor and 1,
// and neighbouring zones lag a little so the wave travels around the ring.
func breathe(zone int, elapsed time.Duration) float64 {
	phase := 2*math.Pi*float64(elapsed)/float64(breathPeriod) - float64(zone)*math.Pi/6
	return breathFloor + (1-breathFloor)*(0.5+0.5*math.Sin(phase))
}

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
	return Config{Colors: map[string]string{"llm": "#ff5000", "home": "#18bcf2", "music": "#00c8b4"}, IdleMS: 600, MaxMS: 60000}
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
	// Unbind detaches the top mount at target; it fails when nothing is mounted there.
	Unbind(target string) error
	// Real rejects a handle that does not point at a real LED, e.g. a stand-in that is still mounted.
	Real(f *os.File) error
}

// channel is one LED brightness file, its stand-in and handles opened before any stand-in covers it.
type channel struct {
	path, standIn string
	real          *os.File
	in            *os.File // the stand-in, open during a takeover
	raw           int      // last value the firmware wrote to the stand-in
	written       int      // last value written to the real LED, -1 when unknown
	trigger       string   // LED trigger at startup, attached again on a framebuffer release
}

// devDir holds framebuffer device nodes; tests point it at a temporary directory.
var devDir = "/dev"

// framebuffer is the ring framebuffer the firmware draws into when a kernel trigger drives the LEDs.
type framebuffer struct {
	file   *os.File
	pixels int
	bpp    int // bytes per pixel; only the first three hold color
	buf    []byte
	// scale converts a framebuffer level to LED brightness: the kernel dims the copy, e.g. by ambient light.
	scale float64
}

const (
	// triggerState lists the triggers detached by a framebuffer takeover, so a crashed run can attach them again.
	triggerState = "triggers"
	scaleSamples = 3
	// minScale keeps a badly timed sample from dimming the indicator until the next takeover.
	minScale = 0.05
)

type Ring struct {
	zones  [][3]*channel
	fb     *framebuffer // nil when the firmware writes the brightness files itself
	state  string
	colors map[string][3]int
	idle   time.Duration
	max    time.Duration
	mount  mounter
	wave   func(zone int, elapsed time.Duration) float64

	mu     sync.Mutex
	route  string
	signal chan struct{}
	done   chan struct{}
	exited chan struct{}
	once   sync.Once
}

var redRing = regexp.MustCompile(`^(.+):red:ring([0-9]+)$`)

// Cleanup unmounts stand-ins left over a ring under root and attaches LED triggers again, after a run
// that was killed mid-takeover. It runs whether or not the indicator is enabled, so turning it off also
// repairs a frozen or dark ring.
func Cleanup(root, standIns string) { cleanup(root, standIns, sysMounter{}) }

func cleanup(root, standIns string, m mounter) {
	files, _ := filepath.Glob(filepath.Join(root, "*:ring*", "brightness"))
	for _, path := range files {
		peel(path, m)
	}
	state := filepath.Join(standIns, triggerState)
	data, err := os.ReadFile(state)
	if err != nil {
		return
	}
	ring := filepath.Join(root, "*:ring*", "trigger")
	failed := false
	for line := range strings.Lines(string(data)) {
		path, trigger, ok := strings.Cut(strings.TrimSpace(line), "\t")
		// The state file lives in /tmp: only ring trigger files are written back.
		if match, _ := filepath.Match(ring, path); ok && match && !attach(path, trigger) {
			failed = true
		}
	}
	if !failed {
		os.Remove(state)
	}
}

func attach(path, trigger string) bool {
	if err := os.WriteFile(path, []byte(trigger), 0); err != nil {
		slog.Warn("route_indicator_trigger_failed", "path", path, "trigger", trigger, "error", err)
		return false
	}
	return true
}

// framebufferTrigger names the trigger that copies a framebuffer to this LED, from the trigger file's list.
func framebufferTrigger(path string) string {
	data, _ := os.ReadFile(path)
	for _, name := range strings.Fields(string(data)) {
		name = strings.Trim(name, "[]")
		if strings.Contains(name, "fb") && strings.Contains(name, "ring") {
			return name
		}
	}
	return ""
}

// activeTrigger reads the bracketed trigger from a sysfs trigger file; a plain value is taken as is.
func activeTrigger(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "none"
	}
	s := strings.TrimSpace(string(data))
	if i := strings.IndexByte(s, '['); i >= 0 {
		if j := strings.IndexByte(s[i:], ']'); j > 0 {
			s = s[i+1 : i+j]
		}
	}
	if s == "" {
		return "none"
	}
	return s
}

// findRingFramebuffer opens the framebuffer named after the ring next to the LED class root.
func findRingFramebuffer(root string) (*framebuffer, error) {
	names, _ := filepath.Glob(filepath.Join(filepath.Dir(root), "graphics", "fb*", "name"))
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil || !strings.Contains(strings.ToLower(string(data)), "ring") {
			continue
		}
		dir := filepath.Dir(name)
		var w, h, bits int
		size, _ := os.ReadFile(filepath.Join(dir, "virtual_size"))
		depth, _ := os.ReadFile(filepath.Join(dir, "bits_per_pixel"))
		if _, err := fmt.Sscanf(strings.TrimSpace(string(size)), "%d,%d", &w, &h); err != nil {
			continue
		}
		bits, _ = strconv.Atoi(strings.TrimSpace(string(depth)))
		if w*h <= 0 || bits < 24 || bits%8 != 0 {
			continue
		}
		f, err := os.Open(filepath.Join(devDir, filepath.Base(dir)))
		if err != nil {
			return nil, err
		}
		return &framebuffer{file: f, pixels: w * h, bpp: bits / 8, buf: make([]byte, w*h*bits/8), scale: 1}, nil
	}
	return nil, nil
}

func peel(path string, m mounter) {
	for range maxStacked {
		if m.Unbind(path) != nil {
			return
		}
	}
}

// New returns nil when the indicator is disabled or the host has no RGB ring under root.
func New(cfg Config, root, standIns string) (*Ring, error) {
	return newRing(cfg, root, standIns, sysMounter{}, breathe)
}

func newRing(cfg Config, root, standIns string, m mounter, wave func(int, time.Duration) float64) (*Ring, error) {
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
		// Mini 3 and Station 2 have the ring only as a framebuffer that the firmware keeps redrawing.
		if fb, err := findRingFramebuffer(root); fb != nil || err != nil {
			if fb != nil {
				fb.file.Close()
			}
			return nil, errors.New("LED ring is a framebuffer without sysfs LEDs; recoloring it is not supported")
		}
		return nil, nil
	}
	slices.SortFunc(zones, func(a, b found) int { return a.index - b.index })
	// The YAM launcher runs with umask 077; the firmware must still be able to open the stand-ins.
	if err := os.MkdirAll(standIns, 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(standIns, 0o755); err != nil {
		return nil, err
	}
	r := &Ring{state: filepath.Join(standIns, triggerState), colors: map[string][3]int{}, idle: time.Duration(cfg.IdleMS) * time.Millisecond, max: time.Duration(cfg.MaxMS) * time.Millisecond, mount: m, wave: wave, signal: make(chan struct{}, 1), done: make(chan struct{}), exited: make(chan struct{})}
	for route, color := range cfg.Colors {
		if color != "" {
			v, _ := strconv.ParseUint(color[1:], 16, 32)
			r.colors[route] = [3]int{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
		}
	}
	for _, z := range zones {
		var zone [3]*channel
		for i, path := range z.paths {
			peel(path, m)
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err == nil {
				err = m.Real(f)
				if err != nil {
					f.Close()
				}
			}
			if err != nil {
				r.closeFiles()
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			zone[i] = &channel{path: path, standIn: filepath.Join(standIns, strings.ReplaceAll(filepath.Base(filepath.Dir(path)), ":", "_")), real: f, written: -1,
				trigger: activeTrigger(filepath.Join(filepath.Dir(path), "trigger"))}
		}
		r.zones = append(r.zones, zone)
	}
	if err := r.detectFramebuffer(root); err != nil {
		r.closeFiles()
		return nil, err
	}
	go r.run()
	return r, nil
}

// detectFramebuffer switches to the framebuffer mode when the ring LEDs follow a kernel trigger.
// A ring left with "none" (a crashed run, a manual write) is still recognized by its framebuffer
// trigger in the list, but only next to a ring framebuffer; without one the sysfs mode stays.
func (r *Ring) detectFramebuffer(root string) error {
	driven := ""
	for _, z := range r.zones {
		for _, c := range z {
			if c.trigger != "none" && driven == "" {
				driven = c.trigger
			}
		}
	}
	fb, err := findRingFramebuffer(root)
	if err != nil {
		return err
	}
	if fb == nil {
		if driven != "" {
			return fmt.Errorf("ring LEDs follow trigger %q but no ring framebuffer was found", driven)
		}
		return nil
	}
	if driven == "" && framebufferTrigger(triggerPath(r.zones[0][0])) == "" {
		fb.file.Close()
		return nil
	}
	for _, z := range r.zones {
		for _, c := range z {
			if t := framebufferTrigger(triggerPath(c)); c.trigger == "none" && t != "" {
				c.trigger = t
			}
		}
	}
	r.fb = fb
	return nil
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

// Start matches bridge.Native.OnStart: a new request hands the ring back until its own route is known.
func (r *Ring) Start(id string) { r.Route(id, "") }

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
	if r.fb != nil {
		r.fb.file.Close()
	}
}

// Mode reports how the indicator follows the firmware: "sysfs" or "framebuffer".
func (r *Ring) Mode() string {
	if r != nil && r.fb != nil {
		return "framebuffer"
	}
	return "sysfs"
}

func (r *Ring) run() {
	defer close(r.exited)
	var (
		ticker                 *time.Ticker
		tick                   <-chan time.Time
		color                  [3]int
		took, started, lastLit time.Time
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
				took = time.Now()
				ticker = time.NewTicker(frame)
				tick = ticker.C
				if r.fb != nil {
					// Detaching a trigger darkens the LED; paint at once instead of a frame later.
					r.mirror(color, 0)
				}
			}
		case now := <-tick:
			if r.mirror(color, now.Sub(took)) {
				lastLit = now
			}
			if now.Sub(lastLit) >= r.idle || now.Sub(started) >= r.max {
				stop()
			}
		}
	}
}

// takeover covers every ring brightness file with a stand-in that starts at the firmware's current value.
func (r *Ring) takeover() error {
	if r.fb != nil {
		return r.takeoverFramebuffer()
	}
	var bound []*channel
	undo := func(err error) error {
		for _, b := range bound {
			r.unbind(b)
		}
		return err
	}
	for _, z := range r.zones {
		for _, c := range z {
			c.raw = readValue(c.real, 0)
			err := os.WriteFile(c.standIn, []byte(strconv.Itoa(c.raw)), 0o644)
			if err == nil {
				err = os.Chmod(c.standIn, 0o644)
			}
			if err == nil {
				c.in, err = os.Open(c.standIn)
			}
			if err != nil {
				return undo(err)
			}
			if err := r.mount.Bind(c.standIn, c.path); err != nil {
				c.in.Close()
				return undo(err)
			}
			c.written = -1
			bound = append(bound, c)
		}
	}
	return nil
}

// release hands the ring back and restores the firmware's own last frame,
// in case the firmware holds a static pattern instead of redrawing it.
func (r *Ring) release() {
	if r.fb != nil {
		r.releaseFramebuffer()
		return
	}
	for _, z := range r.zones {
		for _, c := range z {
			r.unbind(c)
			if _, err := c.real.WriteAt(fmt.Appendf(nil, "%03d\n", c.raw), 0); err == nil {
				c.written = c.raw
			}
		}
	}
}

// takeoverFramebuffer detaches the LED triggers, so the kernel stops copying the firmware's frames.
// The detached triggers are recorded first: a run killed mid-takeover must not leave the ring dark.
func (r *Ring) takeoverFramebuffer() error {
	r.learnScale()
	var state strings.Builder
	for _, z := range r.zones {
		for _, c := range z {
			fmt.Fprintf(&state, "%s\t%s\n", triggerPath(c), c.trigger)
		}
	}
	if err := os.WriteFile(r.state, []byte(state.String()), 0o600); err != nil {
		return err
	}
	for _, z := range r.zones {
		for _, c := range z {
			if err := os.WriteFile(triggerPath(c), []byte("none"), 0); err != nil {
				r.releaseFramebuffer()
				return err
			}
			c.written = -1
		}
	}
	return nil
}

// learnScale compares the LEDs with the frame the kernel copied them from, while the trigger still runs.
// The two reads are not simultaneous and the LEDs lag a rising animation, so the largest of a few samples
// wins and the scale never drops below minScale. A dark ring teaches nothing, so the previous scale is kept.
func (r *Ring) learnScale() {
	best := 0.0
	for i := range scaleSamples {
		if i > 0 {
			time.Sleep(frame)
		}
		fbMax := r.readFramebuffer()
		led := 0
		for _, z := range r.zones {
			for _, c := range z {
				led = max(led, readValue(c.real, 0))
			}
		}
		if fbMax > 0 && led > 0 {
			best = max(best, float64(led)/float64(fbMax))
		}
	}
	if best > 0 {
		r.fb.scale = min(max(best, minScale), 1)
	}
}

// readFramebuffer refreshes the frame buffer copy and returns its brightest color byte.
func (r *Ring) readFramebuffer() int {
	n, _ := r.fb.file.ReadAt(r.fb.buf, 0)
	clear(r.fb.buf[n:])
	level := 0
	for p := range r.fb.pixels {
		level = max(level, r.pixelLevel(p))
	}
	return level
}

// pixelLevel is a pixel's brightest color byte; the byte order (RGB or BGR) does not matter for it.
func (r *Ring) pixelLevel(p int) int {
	px := r.fb.buf[p*r.fb.bpp : p*r.fb.bpp+3]
	return int(max(px[0], px[1], px[2]))
}

// releaseFramebuffer attaches the triggers again; it is the last write, as a brightness write would detach them.
func (r *Ring) releaseFramebuffer() {
	for _, z := range r.zones {
		for _, c := range z {
			attach(triggerPath(c), c.trigger)
		}
	}
	os.Remove(r.state)
}

func triggerPath(c *channel) string { return filepath.Join(filepath.Dir(c.path), "trigger") }

func (r *Ring) unbind(c *channel) {
	// Detaching cannot fail with EBUSY while the firmware has the file open for a frame.
	if err := r.mount.Unbind(c.path); err != nil {
		slog.Warn("route_indicator_release_failed", "path", c.path, "error", err)
	}
	if c.in != nil {
		c.in.Close()
		c.in = nil
	}
}

// mirror repaints the firmware's latest frame in color and reports whether any zone was lit.
func (r *Ring) mirror(color [3]int, elapsed time.Duration) bool {
	lit := false
	whole, scale := 0, 1.0
	if r.fb != nil {
		whole, scale = r.readFramebuffer(), r.fb.scale
	}
	for zi, z := range r.zones {
		level := 0
		switch {
		case r.fb == nil:
			for _, c := range z {
				// The firmware truncates before writing; an empty read keeps the previous value.
				c.raw = readValue(c.in, c.raw)
				level = max(level, c.raw)
			}
		case r.fb.pixels == len(r.zones):
			// Pixel i lights zone i: ring1..3 on the Lite 2, ring0..23 on the Midi.
			level = r.pixelLevel(zi)
		default:
			level = whole
		}
		lit = lit || level > 0
		share := float64(level) * scale / 255 * r.wave(zi, elapsed)
		for i, c := range z {
			v := int(math.Round(float64(color[i]) * share))
			if v == c.written {
				continue
			}
			// sysfs accepts leading zeros; the fixed width also keeps regular files in tests parseable.
			if _, err := c.real.WriteAt(fmt.Appendf(nil, "%03d\n", v), 0); err == nil {
				c.written = v
			}
		}
	}
	return lit
}

// readValue reads a brightness value through an open handle, keeping previous when the read is empty or invalid.
func readValue(f *os.File, previous int) int {
	if f == nil {
		return previous
	}
	var buf [16]byte
	n, _ := f.ReadAt(buf[:], 0)
	v, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return previous
	}
	return min(max(v, 0), 255)
}
