package main

import "testing"

func TestSamePath(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"/Program Files/LogiSwitch/logiswitch.exe", "/program files/logiswitch/LOGISWITCH.EXE", true},
		{"/Program Files/LogiSwitch/./logiswitch.exe", "/Program Files/LogiSwitch/logiswitch.exe", true},
		{"/Users/me/Downloads/logiswitch.exe", "/Program Files/LogiSwitch/logiswitch.exe", false},
		{"/Program Files/LogiSwitch/logiswitch.exe.old", "/Program Files/LogiSwitch/logiswitch.exe", false},
	} {
		if got := samePath(c.a, c.b); got != c.want {
			t.Errorf("samePath(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
