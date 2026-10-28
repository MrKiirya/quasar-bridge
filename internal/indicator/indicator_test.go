package indicator

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMounter records bind mounts; the test plays the firmware by writing into stand-ins.
type fakeMounter struct {
	mu      sync.Mutex
	bound   map[string]int // stacked mounts per target
	binds   int
	unbinds int
	failAt  int // fail the n-th Bind (1-based); 0 never fails
}

func newMounter() *fakeMounter { return &fakeMounter{bound: map[string]int{}} }

func (m *fakeMounter) Bind(_, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.binds++
	if m.failAt != 0 && m.binds == m.failAt {
		return errors.New("bind failed")
	}
	m.bound[target]++
	return nil
}

func (m *fakeMounter) Unbind(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unbinds++
	if m.bound[target] == 0 {
		return errors.New("not mounted")
	}
	m.bound[target]--
	return nil
}

func (m *fakeMounter) Real(*os.File) error { return nil }

func (m *fakeMounter) mounted() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.bound {
		n += c
	}
	return n
}

func (m *fakeMounter) bindCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.binds
}

// fakeLEDs creates sysfs-like brightness files for zones 1..n under a temp root.
func fakeLEDs(t *testing.T, prefix string, n int) string {
	t.Helper()
	// The LED class sits in its own directory, so a ring framebuffer can be added next to it.
	root := filepath.Join(t.TempDir(), "leds")
	for i := 1; i <= n; i++ {
		for _, c := range []string{"red", "green", "blue"} {
			dir := filepath.Join(root, prefix+":"+c+":ring"+strconv.Itoa(i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "brightness"), []byte("000\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func newTestRing(t *testing.T, zones int, cfg Config, m *fakeMounter) *Ring {
	t.Helper()
	cfg.Enabled = true
	r, err := newRing(cfg, fakeLEDs(t, "mini2", zones), t.TempDir(), m, steady)
	if err != nil || r == nil {
		t.Fatalf("newRing = %v, %v", r, err)
	}
	t.Cleanup(r.Off)
	return r
}

func config(idle, maxMS int) Config {
	cfg := DefaultConfig()
	cfg.IdleMS, cfg.MaxMS = idle, maxMS
	return cfg
}

// firmware writes one frame into the stand-ins, as the speaker firmware would through the bind mounts.
func firmware(t *testing.T, r *Ring, zone int, rgb [3]int) {
	t.Helper()
	for i, c := range r.zones[zone] {
		if err := os.WriteFile(c.standIn, []byte(strconv.Itoa(rgb[i])), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// setLED writes a zone's real LED files, e.g. the firmware's frame before a takeover.
func setLED(t *testing.T, r *Ring, zone int, rgb [3]int) {
	t.Helper()
	for i, c := range r.zones[zone] {
		if err := os.WriteFile(c.path, []byte(strconv.Itoa(rgb[i])+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func ledValue(t *testing.T, r *Ring, zone int) [3]int {
	t.Helper()
	var out [3]int
	for i, c := range r.zones[zone] {
		data, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		out[i], _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	return out
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var (
	alicePurple = [3]int{0x6b, 0x0f, 0xff}
	llmColor    = [3]int{0xff, 0x50, 0x00}
	homeBlue    = [3]int{0x18, 0xbc, 0xf2}
)

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []Config{
		{IdleMS: 99, MaxMS: 60000},
		{IdleMS: 600, MaxMS: 999},
		{IdleMS: 600, MaxMS: 60000, Colors: map[string]string{"classify": "#ffffff"}},
		{IdleMS: 600, MaxMS: 60000, Colors: map[string]string{"llm": "blue"}},
		{IdleMS: 600, MaxMS: 60000, Colors: map[string]string{"llm": "#12345"}},
	} {
		if tc.Validate() == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	if err := (Config{IdleMS: 600, MaxMS: 60000, Colors: map[string]string{"llm": "", "playback": "#ffffff"}}).Validate(); err != nil {
		t.Fatalf("empty color and playback route must be accepted: %v", err)
	}
}

func TestNewNeedsEnabledCompleteRing(t *testing.T) {
	m := newMounter()
	root := fakeLEDs(t, "mini2", 6)
	if r, err := newRing(DefaultConfig(), root, t.TempDir(), m, steady); r != nil || err != nil {
		t.Fatalf("disabled indicator: %v, %v", r, err)
	}
	cfg := DefaultConfig()
	cfg.Enabled = true
	if r, err := newRing(cfg, t.TempDir(), t.TempDir(), m, steady); r != nil || err != nil {
		t.Fatalf("indicator without LEDs: %v, %v", r, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "mini2:blue:ring6")); err != nil {
		t.Fatal(err)
	}
	r, err := newRing(cfg, root, t.TempDir(), m, steady)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Off()
	if r.Zones() != 5 {
		t.Fatalf("zones = %d, want 5 complete zones", r.Zones())
	}
}

func TestNewPeelsStackedLeftoversBeforeOpening(t *testing.T) {
	m := newMounter()
	root := fakeLEDs(t, "mini2", 6)
	red := filepath.Join(root, "mini2:red:ring3", "brightness")
	m.bound[red] = 3
	cfg := DefaultConfig()
	cfg.Enabled = true
	r, err := newRing(cfg, root, t.TempDir(), m, steady)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Off()
	if m.mounted() != 0 || m.binds != 0 {
		t.Fatalf("mounted = %d, binds = %d after startup; leftovers must be peeled", m.mounted(), m.binds)
	}
	if !strings.Contains(r.zones[0][0].standIn, "mini2_red_ring1") {
		t.Fatalf("stand-in name: %s", r.zones[0][0].standIn)
	}
}

func TestNewRejectsHandleThatIsNotRealLED(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	m := &rejectingMounter{fakeMounter: newMounter()}
	if r, err := newRing(cfg, fakeLEDs(t, "mini2", 2), t.TempDir(), m, steady); r != nil || err == nil {
		t.Fatalf("newRing = %v, %v; want an error", r, err)
	}
}

type rejectingMounter struct{ *fakeMounter }

func (*rejectingMounter) Real(*os.File) error { return errors.New("not a sysfs file") }

func TestCleanupRunsWithoutRing(t *testing.T) {
	m := newMounter()
	root := fakeLEDs(t, "mini2", 2)
	blue := filepath.Join(root, "mini2:blue:ring2", "brightness")
	m.bound[blue] = 2
	cleanup(root, t.TempDir(), m)
	if m.mounted() != 0 {
		t.Fatalf("%d stand-ins left after cleanup", m.mounted())
	}
}

func TestZonesSortedNumerically(t *testing.T) {
	r := newTestRing(t, 10, config(600, 60000), newMounter())
	if !strings.Contains(r.zones[1][0].path, "ring2") || !strings.Contains(r.zones[9][0].path, "ring10") {
		t.Fatalf("zone order: %s ... %s", r.zones[1][0].path, r.zones[9][0].path)
	}
}

func TestRecolorsFirmwareFramesKeepingBrightness(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 6, config(600, 60000), m)
	setLED(t, r, 0, alicePurple)
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return m.mounted() == 18 })
	eventually(t, "frame copied at takeover", func() bool { return ledValue(t, r, 0) == llmColor })
	firmware(t, r, 1, [3]int{0, 0, 128})
	eventually(t, "half zone", func() bool { return ledValue(t, r, 1) == [3]int{128, 40, 0} })
	if got := ledValue(t, r, 2); got != ([3]int{}) {
		t.Fatalf("dark zone = %v", got)
	}
}

func TestEmptyStandInKeepsPreviousLevel(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 1, config(600, 60000), m)
	setLED(t, r, 0, alicePurple)
	r.Route("a", "llm")
	eventually(t, "recolor", func() bool { return ledValue(t, r, 0) == llmColor })
	for _, c := range r.zones[0] {
		if err := os.WriteFile(c.standIn, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := ledValue(t, r, 0); got != llmColor {
		t.Fatalf("truncated frame changed the ring to %v", got)
	}
}

func TestReleasesWhenFirmwareGoesDarkAndRestoresItsFrame(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 6, config(300, 60000), m)
	setLED(t, r, 0, alicePurple)
	r.Route("a", "home")
	eventually(t, "home color", func() bool { return ledValue(t, r, 0) == homeBlue })
	firmware(t, r, 0, [3]int{})
	eventually(t, "release", func() bool { return m.mounted() == 0 })
	if got := ledValue(t, r, 0); got != ([3]int{}) {
		t.Fatalf("ring left lit after release: %v", got)
	}
}

func TestReleaseRestoresStaticFirmwareFrame(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 1, config(10000, 60000), m)
	setLED(t, r, 0, alicePurple)
	r.Route("a", "llm")
	eventually(t, "recolor", func() bool { return ledValue(t, r, 0) == llmColor })
	r.Route("b", "yandex")
	eventually(t, "release", func() bool { return m.mounted() == 0 })
	if got := ledValue(t, r, 0); got != alicePurple {
		t.Fatalf("ring after release = %v, want the firmware's own %v", got, alicePurple)
	}
}

func TestNewRequestHandsRingBack(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 6, config(10000, 60000), m)
	setLED(t, r, 0, alicePurple)
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return m.mounted() == 18 })
	r.Start("b")
	eventually(t, "release on new request", func() bool { return m.mounted() == 0 })
}

func TestColorChangeKeepsTakeoverAndReleaseAllowsNext(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 6, config(10000, 60000), m)
	setLED(t, r, 0, alicePurple)
	r.Route("a", "llm")
	eventually(t, "claude", func() bool { return ledValue(t, r, 0) == llmColor })
	r.Route("b", "home")
	eventually(t, "home", func() bool { return ledValue(t, r, 0) == homeBlue })
	if m.bindCount() != 18 {
		t.Fatalf("binds = %d; a color change must not mount again", m.bindCount())
	}
	r.Route("c", "yandex")
	eventually(t, "release", func() bool { return m.mounted() == 0 })
	r.Route("d", "llm")
	eventually(t, "second takeover", func() bool { return m.mounted() == 18 && m.bindCount() == 36 })
}

func TestUncoloredRouteWhileIdleLeavesRingAlone(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 6, config(600, 60000), m)
	setLED(t, r, 0, [3]int{0, 0, 200})
	r.Route("a", "yandex")
	r.Route("b", "playback")
	r.Start("c")
	time.Sleep(100 * time.Millisecond)
	if m.bindCount() != 0 || ledValue(t, r, 0) != [3]int{0, 0, 200} {
		t.Fatalf("binds = %d, ring = %v; firmware colors must stay untouched", m.bindCount(), ledValue(t, r, 0))
	}
}

func TestLitRingOutlivesIdleUntilMax(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 1, config(300, 1000), m)
	setLED(t, r, 0, alicePurple)
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return m.mounted() == 3 })
	time.Sleep(600 * time.Millisecond)
	if m.mounted() != 3 {
		t.Fatal("a lit ring was released by idle_ms")
	}
	eventually(t, "release by max_ms", func() bool { return m.mounted() == 0 })
}

func TestOffReleasesSynchronously(t *testing.T) {
	m := newMounter()
	r := newTestRing(t, 6, config(10000, 60000), m)
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return m.mounted() == 18 })
	r.Off()
	if m.mounted() != 0 {
		t.Fatalf("%d stand-ins still mounted after Off", m.mounted())
	}
	r.Route("b", "llm")
	r.Off()
}

func TestFailedTakeoverRollsBack(t *testing.T) {
	m := newMounter()
	m.failAt = 5
	r := newTestRing(t, 6, config(600, 60000), m)
	r.Route("a", "llm")
	eventually(t, "attempt", func() bool { return m.bindCount() >= 5 })
	time.Sleep(50 * time.Millisecond)
	if m.mounted() != 0 {
		t.Fatalf("%d stand-ins left after failed takeover", m.mounted())
	}
}

func TestNilRingIsInert(t *testing.T) {
	var r *Ring
	r.Route("a", "llm")
	r.Start("b")
	r.Off()
	if r.Zones() != 0 {
		t.Fatal("nil ring reports zones")
	}
}

// steady keeps the firmware brightness as is, so tests can expect exact colors.
func steady(int, time.Duration) float64 { return 1 }

func TestBreatheSwingsAndTravels(t *testing.T) {
	lo, hi := 1.0, 0.0
	for ms := 0; ms < int(breathPeriod/time.Millisecond); ms += 20 {
		v := breathe(0, time.Duration(ms)*time.Millisecond)
		lo, hi = min(lo, v), max(hi, v)
	}
	if lo < breathFloor-0.01 || lo > breathFloor+0.01 || hi < 0.99 || hi > 1 {
		t.Fatalf("breath range = %.2f..%.2f, want %.2f..1", lo, hi, breathFloor)
	}
	if breathe(0, breathPeriod/4) == breathe(3, breathPeriod/4) {
		t.Fatal("zones breathe in lockstep; the wave must travel")
	}
}

func TestRingBreathesWhileFirmwareHoldsSteadyFrame(t *testing.T) {
	m := newMounter()
	cfg := config(10000, 60000)
	cfg.Enabled = true
	r, err := newRing(cfg, fakeLEDs(t, "mini2", 1), t.TempDir(), m, breathe)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Off()
	setLED(t, r, 0, alicePurple)
	r.Route("a", "llm")
	seen := map[int]bool{}
	deadline := time.Now().Add(breathPeriod)
	for time.Now().Before(deadline) {
		seen[ledValue(t, r, 0)[0]] = true
		time.Sleep(20 * time.Millisecond)
	}
	if len(seen) < 10 {
		t.Fatalf("red channel took %d distinct values over one breath; the ring looks flat", len(seen))
	}
}

// fakeFramebufferRing builds a Lite 2-like ring: sysfs LEDs that follow a kernel trigger fed by a ring framebuffer.
func fakeFramebufferRing(t *testing.T, zones, pixels int) (root string, fb string) {
	t.Helper()
	root = fakeLEDs(t, "plum", zones)
	for i := 1; i <= zones; i++ {
		for _, c := range []string{"red", "green", "blue"} {
			path := filepath.Join(root, "plum:"+c+":ring"+strconv.Itoa(i), "trigger")
			if err := os.WriteFile(path, []byte("none timer [led_ring_fb] default-on\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	graphics := filepath.Join(filepath.Dir(root), "graphics", "fb0")
	if err := os.MkdirAll(graphics, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"name": "YNDX LED ring\n", "virtual_size": strconv.Itoa(pixels) + ",1\n", "bits_per_pixel": "24\n"} {
		if err := os.WriteFile(filepath.Join(graphics, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dev := t.TempDir()
	previous := devDir
	devDir = dev
	t.Cleanup(func() { devDir = previous })
	fb = filepath.Join(dev, "fb0")
	if err := os.WriteFile(fb, make([]byte, pixels*3), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, fb
}

func newFramebufferRing(t *testing.T, zones, pixels int, cfg Config, m *fakeMounter) (*Ring, string) {
	t.Helper()
	root, fb := fakeFramebufferRing(t, zones, pixels)
	cfg.Enabled = true
	r, err := newRing(cfg, root, t.TempDir(), m, steady)
	if err != nil || r == nil {
		t.Fatalf("newRing = %v, %v", r, err)
	}
	t.Cleanup(r.Off)
	return r, fb
}

// drawFramebuffer plays the firmware drawing BGR pixels into the ring framebuffer.
func drawFramebuffer(t *testing.T, fb string, pixels ...[3]byte) {
	t.Helper()
	var frame []byte
	for _, p := range pixels {
		frame = append(frame, p[:]...)
	}
	if err := os.WriteFile(fb, frame, 0o644); err != nil {
		t.Fatal(err)
	}
}

func triggers(t *testing.T, r *Ring) []string {
	t.Helper()
	var out []string
	for _, z := range r.zones {
		for _, c := range z {
			out = append(out, activeTrigger(triggerPath(c)))
		}
	}
	return out
}

func allTriggers(t *testing.T, r *Ring, want string) bool {
	for _, got := range triggers(t, r) {
		if got != want {
			return false
		}
	}
	return true
}

func TestFramebufferModeIsDetectedByTrigger(t *testing.T) {
	r, _ := newFramebufferRing(t, 3, 3, config(600, 60000), newMounter())
	if r.Mode() != "framebuffer" || r.Zones() != 3 {
		t.Fatalf("mode = %s, zones = %d", r.Mode(), r.Zones())
	}
	if m := newTestRing(t, 6, config(600, 60000), newMounter()); m.Mode() != "sysfs" {
		t.Fatalf("Mini 2 ring mode = %s", m.Mode())
	}
}

func TestFramebufferTakeoverRecolorsFirmwareFrameAndAttachesTriggersBack(t *testing.T) {
	m := newMounter()
	r, fb := newFramebufferRing(t, 3, 3, config(300, 60000), m)
	drawFramebuffer(t, fb, [3]byte{255, 0, 0}, [3]byte{0, 128, 0}, [3]byte{})
	r.Route("a", "llm")
	eventually(t, "triggers detached", func() bool { return allTriggers(t, r, "none") })
	eventually(t, "recolor", func() bool {
		return ledValue(t, r, 0) == llmColor && ledValue(t, r, 1) == [3]int{128, 40, 0} && ledValue(t, r, 2) == [3]int{}
	})
	if m.bindCount() != 0 {
		t.Fatalf("binds = %d; the framebuffer mode must not mount stand-ins", m.bindCount())
	}
	drawFramebuffer(t, fb, [3]byte{}, [3]byte{}, [3]byte{})
	eventually(t, "triggers attached again", func() bool { return allTriggers(t, r, "led_ring_fb") })
	if _, err := os.Stat(r.state); !os.IsNotExist(err) {
		t.Fatalf("trigger state left after release: %v", err)
	}
}

func TestFramebufferKeepsKernelDimming(t *testing.T) {
	r, fb := newFramebufferRing(t, 3, 3, config(10000, 60000), newMounter())
	// The kernel showed the firmware's 200 as 100 on the LEDs, e.g. in a dark room.
	drawFramebuffer(t, fb, [3]byte{200, 0, 0}, [3]byte{200, 0, 0}, [3]byte{200, 0, 0})
	setLED(t, r, 0, [3]int{0, 0, 100})
	r.Route("a", "llm")
	eventually(t, "dimmed recolor", func() bool {
		return ledValue(t, r, 0) == [3]int{100, 31, 0}
	})
}

func TestFramebufferWithOtherPixelCountUsesWholeFrame(t *testing.T) {
	r, fb := newFramebufferRing(t, 3, 6, config(10000, 60000), newMounter())
	drawFramebuffer(t, fb, [3]byte{}, [3]byte{}, [3]byte{}, [3]byte{}, [3]byte{}, [3]byte{255, 255, 255})
	r.Route("a", "llm")
	eventually(t, "every zone lit", func() bool {
		return ledValue(t, r, 0) == llmColor && ledValue(t, r, 2) == llmColor
	})
}

func TestCleanupAttachesTriggersAfterCrash(t *testing.T) {
	root, fb := fakeFramebufferRing(t, 3, 3)
	drawFramebuffer(t, fb, [3]byte{255, 0, 0}, [3]byte{}, [3]byte{})
	standIns := t.TempDir()
	cfg := config(10000, 60000)
	cfg.Enabled = true
	r, err := newRing(cfg, root, standIns, newMounter(), steady)
	if err != nil {
		t.Fatal(err)
	}
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return allTriggers(t, r, "none") })
	// A killed run never releases: only the trigger state remains.
	state, _ := os.ReadFile(r.state)
	r.closeFiles()
	cleanup(root, standIns, newMounter())
	if len(state) == 0 || !allTriggers(t, r, "led_ring_fb") {
		t.Fatalf("triggers after cleanup = %v", triggers(t, r))
	}
	if _, err := os.Stat(filepath.Join(standIns, triggerState)); !os.IsNotExist(err) {
		t.Fatal("trigger state kept after cleanup")
	}
}

func TestTriggerWithoutFramebufferIsRejected(t *testing.T) {
	root, _ := fakeFramebufferRing(t, 3, 3)
	if err := os.RemoveAll(filepath.Join(filepath.Dir(root), "graphics")); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Enabled = true
	if r, err := newRing(cfg, root, t.TempDir(), newMounter(), steady); r != nil || err == nil {
		t.Fatalf("newRing = %v, %v; want an error", r, err)
	}
}

func TestFramebufferOnlyRingIsReported(t *testing.T) {
	root, _ := fakeFramebufferRing(t, 1, 6)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Enabled = true
	if r, err := newRing(cfg, root, t.TempDir(), newMounter(), steady); r != nil || err == nil {
		t.Fatalf("newRing = %v, %v; want unsupported", r, err)
	}
}

// setTriggers writes a sysfs-like trigger list to every ring channel under root.
func setTriggers(t *testing.T, root, content string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(root, "*:ring*"))
	for _, dir := range files {
		if err := os.WriteFile(filepath.Join(dir, "trigger"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMini2TriggerListKeepsSysfsMode(t *testing.T) {
	root := fakeLEDs(t, "mini2", 6)
	setTriggers(t, root, "[none] rc-feedback timer heartbeat default-on\n")
	cfg := config(600, 60000)
	cfg.Enabled = true
	m := newMounter()
	r, err := newRing(cfg, root, t.TempDir(), m, steady)
	if err != nil || r == nil {
		t.Fatalf("newRing = %v, %v", r, err)
	}
	defer r.Off()
	if r.Mode() != "sysfs" {
		t.Fatalf("mode = %s", r.Mode())
	}
	setLED(t, r, 0, alicePurple)
	r.Route("a", "llm")
	eventually(t, "takeover by stand-ins", func() bool { return m.mounted() == 18 })
	eventually(t, "recolor", func() bool { return ledValue(t, r, 0) == llmColor })
}

func TestDetachedFramebufferRingIsStillRecognized(t *testing.T) {
	root, fb := fakeFramebufferRing(t, 3, 3)
	setTriggers(t, root, "[none] timer led_ring_fb default-on\n")
	cfg := config(300, 60000)
	cfg.Enabled = true
	m := newMounter()
	r, err := newRing(cfg, root, t.TempDir(), m, steady)
	if err != nil || r == nil || r.Mode() != "framebuffer" {
		t.Fatalf("newRing = %v, %v", r, err)
	}
	defer r.Off()
	drawFramebuffer(t, fb, [3]byte{255, 0, 0}, [3]byte{}, [3]byte{})
	r.Route("a", "llm")
	eventually(t, "recolor", func() bool { return ledValue(t, r, 0) == llmColor })
	drawFramebuffer(t, fb, [3]byte{}, [3]byte{}, [3]byte{})
	eventually(t, "framebuffer trigger attached", func() bool { return allTriggers(t, r, "led_ring_fb") })
	if m.bindCount() != 0 {
		t.Fatalf("binds = %d", m.bindCount())
	}
}

func TestFramebufferWithoutRingTriggerKeepsSysfsMode(t *testing.T) {
	root, _ := fakeFramebufferRing(t, 3, 3)
	setTriggers(t, root, "[none] timer default-on\n")
	cfg := config(600, 60000)
	cfg.Enabled = true
	r, err := newRing(cfg, root, t.TempDir(), newMounter(), steady)
	if err != nil || r == nil {
		t.Fatalf("newRing = %v, %v", r, err)
	}
	defer r.Off()
	if r.Mode() != "sysfs" {
		t.Fatalf("mode = %s", r.Mode())
	}
}

func TestCleanupWritesOnlyRingTriggers(t *testing.T) {
	root := fakeLEDs(t, "plum", 1)
	standIns := t.TempDir()
	foreign := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(foreign, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	ring := filepath.Join(root, "plum:red:ring1", "trigger")
	if err := os.WriteFile(ring, []byte("[none] led_ring_fb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := foreign + "\tnone\n" + ring + "\tled_ring_fb\n"
	if err := os.WriteFile(filepath.Join(standIns, triggerState), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup(root, standIns, newMounter())
	if data, _ := os.ReadFile(foreign); string(data) != "keep" {
		t.Fatalf("cleanup wrote outside the ring: %q", data)
	}
	if got := activeTrigger(ring); got != "led_ring_fb" {
		t.Fatalf("ring trigger = %q", got)
	}
}

func TestScaleHasFloor(t *testing.T) {
	r, fb := newFramebufferRing(t, 3, 3, config(10000, 60000), newMounter())
	drawFramebuffer(t, fb, [3]byte{255, 0, 0}, [3]byte{255, 0, 0}, [3]byte{255, 0, 0})
	setLED(t, r, 0, [3]int{0, 0, 1})
	r.Route("a", "llm")
	eventually(t, "floor brightness", func() bool { return ledValue(t, r, 0) == [3]int{13, 4, 0} })
}
