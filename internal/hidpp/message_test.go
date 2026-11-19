package hidpp

import "testing"

func TestEncodeDecodeRoundTrip(t *testing.T) {
	m := Message{Dev: 0xFF, Feature: 0x09, Function: 0x3, SWID: 0x07}
	m.Params[0], m.Params[15] = 0xAA, 0xBB
	b := Encode(m)
	if len(b) != 20 || b[0] != 0x11 || b[1] != 0xFF || b[2] != 0x09 || b[3] != 0x37 || b[4] != 0xAA || b[19] != 0xBB {
		t.Fatalf("bad encoding: % x", b)
	}
	got, ok := Decode(b)
	if !ok || got != m {
		t.Fatalf("round trip: %+v ok=%v", got, ok)
	}
}

func TestDecodeRejectsShortReport(t *testing.T) {
	if _, ok := Decode(make([]byte, 19)); ok {
		t.Fatal("accepted short report")
	}
	b := make([]byte, 20)
	b[0] = 0x10
	if _, ok := Decode(b); ok {
		t.Fatal("accepted wrong report id")
	}
}
