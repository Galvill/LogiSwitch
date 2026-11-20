package hidpp_test

import (
	"errors"
	"testing"

	"logiswitch/internal/hidpp"
	"logiswitch/internal/hidpp/hidpptest"
)

func newConn(t *testing.T, f *hidpptest.Fake) *hidpp.Conn {
	t.Helper()
	c := hidpp.NewConn(f, 0xFF)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestFeatureIndexMissing(t *testing.T) {
	c := newConn(t, hidpptest.NewKeyboard(0))
	if _, err := c.FeatureIndex(0x9999); !errors.Is(err, hidpp.ErrNoFeature) {
		t.Fatalf("err = %v, want ErrNoFeature", err)
	}
	i, err := c.FeatureIndex(hidpp.FeatChangeHost)
	if err != nil || i != 9 {
		t.Fatalf("got %d, %v", i, err)
	}
}

func TestHostInfo(t *testing.T) {
	c := newConn(t, hidpptest.NewKeyboard(2))
	n, cur, err := c.HostInfo(9)
	if err != nil || n != 3 || cur != 2 {
		t.Fatalf("got %d %d %v", n, cur, err)
	}
}

func TestControlsListsHostCIDs(t *testing.T) {
	c := newConn(t, hidpptest.NewKeyboard(0))
	cs, err := c.Controls(8)
	if err != nil || len(cs) != 3 {
		t.Fatalf("got %v %v", cs, err)
	}
	for i, ctl := range cs {
		if ctl.CID != hidpp.HostSwitchCIDs[i] || ctl.Flags != 0x64 {
			t.Fatalf("control %d = %+v", i, ctl)
		}
	}
}

func TestDivertRoundTrip(t *testing.T) {
	f := hidpptest.NewKeyboard(0)
	c := newConn(t, f)
	if on, err := c.Diverted(8, 0xD1); err != nil || on {
		t.Fatalf("initial %v %v", on, err)
	}
	if err := c.SetDiverted(8, 0xD1, true); err != nil {
		t.Fatal(err)
	}
	if on, err := c.Diverted(8, 0xD1); err != nil || !on || !f.Diverted(0xD1) {
		t.Fatalf("after on: %v %v", on, err)
	}
	if err := c.SetDiverted(8, 0xD1, false); err != nil {
		t.Fatal(err)
	}
	if on, _ := c.Diverted(8, 0xD1); on {
		t.Fatal("still diverted")
	}
}

func TestDivertedCIDsParsesRelease(t *testing.T) {
	var m hidpp.Message
	m.Params[0], m.Params[1] = 0x00, 0xD2
	got := hidpp.DivertedCIDs(m)
	if len(got) != 1 || got[0] != 0xD2 {
		t.Fatalf("press: %v", got)
	}
	if got := hidpp.DivertedCIDs(hidpp.Message{}); len(got) != 0 {
		t.Fatalf("release: %v", got)
	}
	m.Function = 1
	if got := hidpp.DivertedCIDs(m); len(got) != 0 {
		t.Fatalf("fn1: %v", got)
	}
}
