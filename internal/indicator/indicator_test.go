package indicator

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
			if err := os.WriteFile(filepath.Join(dir, "brightness"), []byte("0"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func colors(t *testing.T, r *Ring) [][3]int {
	t.Helper()
	var out [][3]int
	for _, z := range r.zones {
		var c [3]int
		for i, path := range [3]string{z.red, z.green, z.blue} {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c[i], _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		out = append(out, c)
	}
	return out
}

func waitColor(t *testing.T, r *Ring, want [3]int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := colors(t, r)
		ok := true
		for _, c := range got {
			ok = ok && c == want
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ring = %v, want every zone %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func enabled(duration int) Config {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.DurationMS = duration
	return cfg
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []Config{
		{DurationMS: 99},
		{DurationMS: 60001},
		{DurationMS: 4000, Colors: map[string]string{"classify": "#ffffff"}},
		{DurationMS: 4000, Colors: map[string]string{"llm": "blue"}},
		{DurationMS: 4000, Colors: map[string]string{"llm": "#12345"}},
	} {
		if tc.Validate() == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	if err := (Config{DurationMS: 4000, Colors: map[string]string{"yandex": ""}}).Validate(); err != nil {
		t.Fatalf("empty color must disable a route: %v", err)
	}
}

func TestNewNeedsEnabledCompleteRing(t *testing.T) {
	root := fakeLEDs(t, "mini2", 6)
	if New(DefaultConfig(), root) != nil {
		t.Fatal("disabled indicator created")
	}
	if New(enabled(4000), t.TempDir()) != nil {
		t.Fatal("indicator without LEDs created")
	}
	if err := os.RemoveAll(filepath.Join(root, "mini2:blue:ring6")); err != nil {
		t.Fatal(err)
	}
	if got := New(enabled(4000), root).Zones(); got != 5 {
		t.Fatalf("zones = %d, want 5 complete zones", got)
	}
}

func TestZonesSortedNumerically(t *testing.T) {
	r := New(enabled(4000), fakeLEDs(t, "mini2", 10))
	if !strings.Contains(r.zones[1].red, "ring2") || !strings.Contains(r.zones[9].red, "ring10") {
		t.Fatalf("zone order: %s ... %s", r.zones[1].red, r.zones[9].red)
	}
}

func TestRouteLightsThenClears(t *testing.T) {
	r := New(enabled(100), fakeLEDs(t, "mini2", 6))
	r.Route("req", "llm")
	waitColor(t, r, [3]int{0x00, 0x50, 0xff})
	waitColor(t, r, [3]int{})
}

func TestUnlitRouteClearsImmediately(t *testing.T) {
	cfg := enabled(60000)
	cfg.Colors["yandex"] = ""
	r := New(cfg, fakeLEDs(t, "mini2", 3))
	r.Route("a", "home")
	waitColor(t, r, [3]int{0xff, 0xa0, 0x00})
	r.Route("b", "yandex")
	waitColor(t, r, [3]int{})
}

func TestLaterRouteReplacesEarlierTimer(t *testing.T) {
	r := New(enabled(300), fakeLEDs(t, "mini2", 2))
	r.Route("a", "llm")
	time.Sleep(200 * time.Millisecond)
	r.Route("b", "home")
	time.Sleep(150 * time.Millisecond)
	// The first timer would have fired by now; the second route must still be lit.
	for _, c := range colors(t, r) {
		if c != [3]int{0xff, 0xa0, 0x00} {
			t.Fatalf("ring = %v after replaced timer", colors(t, r))
		}
	}
	waitColor(t, r, [3]int{})
}

func TestOffDarkensSynchronously(t *testing.T) {
	r := New(enabled(60000), fakeLEDs(t, "mini2", 6))
	r.Route("a", "llm")
	waitColor(t, r, [3]int{0x00, 0x50, 0xff})
	r.Off()
	for _, c := range colors(t, r) {
		if c != ([3]int{}) {
			t.Fatalf("ring = %v right after Off", colors(t, r))
		}
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
