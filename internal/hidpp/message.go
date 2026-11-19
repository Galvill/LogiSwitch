// Package hidpp implements HID++ 2.0 long-report framing and a request/reply connection.
package hidpp

// Message is a HID++ long report (20 bytes, report ID 0x11).
type Message struct {
	Dev, Feature, Function, SWID byte
	Params                       [16]byte
}

const reportLen = 20

// Encode returns the 20-byte wire form of m.
func Encode(m Message) []byte {
	b := make([]byte, reportLen)
	b[0] = 0x11
	b[1] = m.Dev
	b[2] = m.Feature
	b[3] = (m.Function&0x0F)<<4 | m.SWID&0x0F
	copy(b[4:], m.Params[:])
	return b
}

// Decode parses a long report; ok is false unless len(b)>=20 and b[0]==0x11.
func Decode(b []byte) (Message, bool) {
	if len(b) < reportLen || b[0] != 0x11 {
		return Message{}, false
	}
	m := Message{Dev: b[1], Feature: b[2], Function: b[3] >> 4, SWID: b[3] & 0x0F}
	copy(m.Params[:], b[4:reportLen])
	return m, true
}
