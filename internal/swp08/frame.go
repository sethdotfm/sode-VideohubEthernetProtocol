// Package swp08 implements the Pro-Bel SW-P-08 router control protocol over TCP.
package swp08

import "errors"

// Transport control bytes.
const (
	DLE = 0x10
	STX = 0x02
	ETX = 0x03
	ACK = 0x06
	NAK = 0x15
)

var (
	AckFrame = []byte{DLE, ACK}
	NakFrame = []byte{DLE, NAK}
)

// maxMsg bounds a single unstuffed message (DATA + BTC + CHK). BTC is one
// byte, so anything longer than this is garbage.
const maxMsg = 258

// Encode wraps a message (command byte + data) as
// DLE STX DATA BTC CHK DLE ETX, doubling any DLE inside.
func Encode(msg []byte) []byte {
	body := make([]byte, 0, len(msg)+2)
	body = append(body, msg...)
	body = append(body, byte(len(msg)))
	var sum byte
	for _, b := range body {
		sum += b
	}
	body = append(body, -sum)

	out := make([]byte, 0, len(body)*2+4)
	out = append(out, DLE, STX)
	for _, b := range body {
		if b == DLE {
			out = append(out, DLE)
		}
		out = append(out, b)
	}
	return append(out, DLE, ETX)
}

type Kind int

const (
	KindMsg Kind = iota // a valid message; Msg holds command + data
	KindAck
	KindNak
	KindBad // framing or checksum error; reply with NAK
)

type Frame struct {
	Kind Kind
	Msg  []byte
	Err  error // set for KindBad; for KindMsg a non-fatal warning (BTC mismatch)
}

var (
	ErrChecksum = errors.New("swp08: bad checksum")
	ErrShort    = errors.New("swp08: message too short")
	ErrOverflow = errors.New("swp08: message too long")
	ErrFraming  = errors.New("swp08: unexpected byte after DLE")
	ErrBTC      = errors.New("swp08: byte count mismatch")
)

// Decoder turns a TCP byte stream into frames. It is not safe for
// concurrent use.
type Decoder struct {
	inMsg bool
	esc   bool
	buf   []byte
}

func (d *Decoder) Feed(p []byte, emit func(Frame)) {
	for _, b := range p {
		if d.esc {
			d.esc = false
			switch b {
			case DLE:
				if d.inMsg {
					d.push(DLE, emit)
				}
			case STX:
				d.inMsg = true
				d.buf = d.buf[:0]
			case ETX:
				if d.inMsg {
					d.inMsg = false
					emit(check(d.buf))
				}
			case ACK:
				d.inMsg = false
				emit(Frame{Kind: KindAck})
			case NAK:
				d.inMsg = false
				emit(Frame{Kind: KindNak})
			default:
				if d.inMsg {
					d.inMsg = false
					emit(Frame{Kind: KindBad, Err: ErrFraming})
				}
			}
			continue
		}
		if b == DLE {
			d.esc = true
			continue
		}
		if d.inMsg {
			d.push(b, emit)
		}
	}
}

func (d *Decoder) push(b byte, emit func(Frame)) {
	if len(d.buf) >= maxMsg {
		d.inMsg = false
		emit(Frame{Kind: KindBad, Err: ErrOverflow})
		return
	}
	d.buf = append(d.buf, b)
}

func check(buf []byte) Frame {
	if len(buf) < 3 {
		return Frame{Kind: KindBad, Err: ErrShort}
	}
	var sum byte
	for _, b := range buf {
		sum += b
	}
	if sum != 0 {
		return Frame{Kind: KindBad, Err: ErrChecksum}
	}
	msg := append([]byte(nil), buf[:len(buf)-2]...)
	f := Frame{Kind: KindMsg, Msg: msg}
	// The checksum already covers integrity; a BTC mismatch is reported but
	// tolerated, since some devices count it differently.
	if int(buf[len(buf)-2]) != len(msg) {
		f.Err = ErrBTC
	}
	return f
}
