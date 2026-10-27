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
	bound   map[string]bool
	binds   int
	unbinds int
	failAt  int // fail the n-th Bind (1-based); 0 never fails
}

func (m *fakeMounter) Bind(_, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.binds++
	if m.failAt != 0 && m.binds == m.failAt {
		return errors.New("bind failed")
	}
	m.bound[target] = true
	return nil
}

func (m *fakeMounter) Unbind(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unbinds++
	if !m.bound[target] {
		return errors.New("not mounted")
	}
	delete(m.bound, target)
	return nil
}

func (m *fakeMounter) mounted() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bound)
}

// fakeLEDs creates sysfs-like brightness files for zones 1..n under a temp root.
func fakeLEDs(t *testing.T, prefix string, n int) string {
	t.Helper()
	root := t.TempDir()
	for i := 1; i <= n; i++ {
		for _, c := range []string{"red", "green", "blue"} {
			dir := filepath.Join(root, prefix+":"+c+":ring"+strconv.Itoa(i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "brightness"), []byte("0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func newTestRing(t *testing.T, zones int, cfg Config, m *fakeMounter) *Ring {
	t.Helper()
	cfg.Enabled = true
	r, err := newRing(cfg, fakeLEDs(t, "mini2", zones), t.TempDir(), m)
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
	m := &fakeMounter{bound: map[string]bool{}}
	root := fakeLEDs(t, "mini2", 6)
	if r, err := newRing(DefaultConfig(), root, t.TempDir(), m); r != nil || err != nil {
		t.Fatalf("disabled indicator: %v, %v", r, err)
	}
	cfg := DefaultConfig()
	cfg.Enabled = true
	if r, err := newRing(cfg, t.TempDir(), t.TempDir(), m); r != nil || err != nil {
		t.Fatalf("indicator without LEDs: %v, %v", r, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "mini2:blue:ring6")); err != nil {
		t.Fatal(err)
	}
	r, err := newRing(cfg, root, t.TempDir(), m)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Off()
	if r.Zones() != 5 {
		t.Fatalf("zones = %d, want 5 complete zones", r.Zones())
	}
}

func TestNewClearsLeftoverStandInsBeforeOpening(t *testing.T) {
	m := &fakeMounter{bound: map[string]bool{}}
	r := newTestRing(t, 6, config(600, 60000), m)
	if m.unbinds != 18 || m.binds != 0 {
		t.Fatalf("startup unbinds = %d, binds = %d; want 18 and 0", m.unbinds, m.binds)
	}
	if !strings.Contains(r.zones[0][0].standIn, "mini2_red_ring1") {
		t.Fatalf("stand-in name: %s", r.zones[0][0].standIn)
	}
}

func TestZonesSortedNumerically(t *testing.T) {
	r := newTestRing(t, 10, config(600, 60000), &fakeMounter{bound: map[string]bool{}})
	if !strings.Contains(r.zones[1][0].path, "ring2") || !strings.Contains(r.zones[9][0].path, "ring10") {
		t.Fatalf("zone order: %s ... %s", r.zones[1][0].path, r.zones[9][0].path)
	}
}

func TestRecolorsFirmwareFramesKeepingBrightness(t *testing.T) {
	m := &fakeMounter{bound: map[string]bool{}}
	r := newTestRing(t, 6, config(600, 60000), m)
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return m.mounted() == 18 })
	firmware(t, r, 0, [3]int{0x6b, 0x0f, 0xff})
	firmware(t, r, 1, [3]int{0, 0, 128})
	eventually(t, "full zone", func() bool { return ledValue(t, r, 0) == [3]int{217, 119, 87} })
	eventually(t, "half zone", func() bool { return ledValue(t, r, 1) == [3]int{109, 60, 44} })
	if got := ledValue(t, r, 2); got != ([3]int{}) {
		t.Fatalf("dark zone = %v", got)
	}
}

func TestReleasesWhenFirmwareGoesDark(t *testing.T) {
	m := &fakeMounter{bound: map[string]bool{}}
	r := newTestRing(t, 6, config(100, 60000), m)
	r.Route("a", "home")
	eventually(t, "takeover", func() bool { return m.mounted() == 18 })
	firmware(t, r, 0, [3]int{0x6b, 0x0f, 0xff})
	eventually(t, "home color", func() bool { return ledValue(t, r, 0) == [3]int{0x18, 0xbc, 0xf2} })
	firmware(t, r, 0, [3]int{})
	eventually(t, "release", func() bool { return m.mounted() == 0 })
	if got := ledValue(t, r, 0); got != ([3]int{}) {
		t.Fatalf("ring left lit after release: %v", got)
	}
}

func TestUncoloredRouteReleasesTakeover(t *testing.T) {
	m := &fakeMounter{bound: map[string]bool{}}
	r := newTestRing(t, 6, config(10000, 60000), m)
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return m.mounted() == 18 })
	r.Route("b", "yandex")
	eventually(t, "release", func() bool { return m.mounted() == 0 })
}

func TestUncoloredRouteWhileIdleLeavesRingAlone(t *testing.T) {
	m := &fakeMounter{bound: map[string]bool{}}
	r := newTestRing(t, 6, config(600, 60000), m)
	if err := os.WriteFile(r.zones[0][2].path, []byte("200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Route("a", "yandex")
	r.Route("b", "playback")
	time.Sleep(100 * time.Millisecond)
	if m.binds != 0 || ledValue(t, r, 0)[2] != 200 {
		t.Fatalf("binds = %d, ring = %v; firmware colors must stay untouched", m.binds, ledValue(t, r, 0))
	}
}

func TestMaxEndsTakeoverEvenWhileLit(t *testing.T) {
	m := &fakeMounter{bound: map[string]bool{}}
	r := newTestRing(t, 2, config(10000, 1000), m)
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return m.mounted() == 6 })
	firmware(t, r, 0, [3]int{0x6b, 0x0f, 0xff})
	eventually(t, "release by max_ms", func() bool { return m.mounted() == 0 })
}

func TestOffReleasesSynchronously(t *testing.T) {
	m := &fakeMounter{bound: map[string]bool{}}
	r := newTestRing(t, 6, config(10000, 60000), m)
	r.Route("a", "llm")
	eventually(t, "takeover", func() bool { return m.mounted() == 18 })
	r.Off()
	if m.mounted() != 0 {
		t.Fatalf("%d stand-ins still mounted after Off", m.mounted())
	}
	r.Route("b", "llm")
}

func TestFailedTakeoverRollsBack(t *testing.T) {
	m := &fakeMounter{bound: map[string]bool{}, failAt: 5}
	r := newTestRing(t, 6, config(600, 60000), m)
	r.Route("a", "llm")
	eventually(t, "attempt", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.binds >= 5 })
	time.Sleep(50 * time.Millisecond)
	if m.mounted() != 0 {
		t.Fatalf("%d stand-ins left after failed takeover", m.mounted())
	}
}

func TestNilRingIsInert(t *testing.T) {
	var r *Ring
	r.Route("a", "llm")
	r.Off()
	if r.Zones() != 0 {
		t.Fatal("nil ring reports zones")
	}
}
