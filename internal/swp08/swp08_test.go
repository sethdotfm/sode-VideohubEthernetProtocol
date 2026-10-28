package swp08

import (
	"bytes"
	"reflect"
	"testing"
)

func decodeAll(t *testing.T, p []byte) []Frame {
	t.Helper()
	var d Decoder
	var out []Frame
	d.Feed(p, func(f Frame) { out = append(out, f) })
	return out
}

func TestEncodeChecksum(t *testing.T) {
	// cmd 0x02, data 00 00 05 03 → BTC 5, sum 0x0F, CHK 0xF1
	got := Encode([]byte{0x02, 0x00, 0x00, 0x05, 0x03})
	want := []byte{DLE, STX, 0x02, 0x00, 0x00, 0x05, 0x03, 0x05, 0xF1, DLE, ETX}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X want % X", got, want)
	}
}

func TestRoundTripWithDLE(t *testing.T) {
	msg := ExtConnect(0, 0x10, 0x1010, 0x0110)
	enc := Encode(msg)
	fs := decodeAll(t, append(append([]byte{0xFF}, enc...), DLE, ACK))
	if len(fs) != 2 || fs[0].Kind != KindMsg || fs[1].Kind != KindAck {
		t.Fatalf("frames %+v", fs)
	}
	if fs[0].Err != nil || !bytes.Equal(fs[0].Msg, msg) {
		t.Fatalf("got % X err %v", fs[0].Msg, fs[0].Err)
	}
}

func TestSplitFeed(t *testing.T) {
	enc := Encode(ExtTally(1, 2, 300, 400))
	var d Decoder
	var got []Frame
	for _, b := range enc {
		d.Feed([]byte{b}, func(f Frame) { got = append(got, f) })
	}
	if len(got) != 1 || got[0].Kind != KindMsg {
		t.Fatalf("frames %+v", got)
	}
}

func TestBadChecksum(t *testing.T) {
	enc := Encode([]byte{0x01, 0x00, 0x00, 0x05})
	enc[len(enc)-3]++ // corrupt CHK
	fs := decodeAll(t, enc)
	if len(fs) != 1 || fs[0].Kind != KindBad || fs[0].Err != ErrChecksum {
		t.Fatalf("frames %+v", fs)
	}
}

func TestParseStdConnected(t *testing.T) {
	// matrix 1 level 2, dst = 3*128+5 = 389, src = 2*128+7 = 263
	ev, err := Parse([]byte{CmdConnected, 0x12, 0x32, 0x05, 0x07})
	if err != nil {
		t.Fatal(err)
	}
	want := []Crosspoint{{Matrix: 1, Level: 2, Dst: 389, Src: 263, Cmd: CmdConnected}}
	if !reflect.DeepEqual(ev.Crosspoints, want) {
		t.Fatalf("got %+v", ev.Crosspoints)
	}
}

func TestParseExtDump(t *testing.T) {
	ev, err := Parse(ExtTallyDump(0, 3, 1000, []int{5, 1500}))
	if err != nil {
		t.Fatal(err)
	}
	want := []Crosspoint{{0, 3, 1000, 5, CmdExtTallyDump}, {0, 3, 1001, 1500, CmdExtTallyDump}}
	if !reflect.DeepEqual(ev.Crosspoints, want) {
		t.Fatalf("got %+v", ev.Crosspoints)
	}
}

func TestParseNames(t *testing.T) {
	for _, ext := range []bool{false, true} {
		ev, err := Parse(NamesResp(true, ext, 0, 0, 2, 300, []string{"DSM5", "Zoom Return 1"}))
		if err != nil {
			t.Fatal(err)
		}
		nb := ev.Names
		if nb == nil || !nb.Dest || nb.First != 300 || !reflect.DeepEqual(nb.Names, []string{"DSM5", "Zoom Return"}) {
			t.Fatalf("ext=%v got %+v", ext, nb)
		}
	}
}

func TestParseProtectDump(t *testing.T) {
	// Captured from a Ross Ultrix: level 0, dsts 64-65, 64 protected by device 9.
	ev, err := Parse([]byte{CmdExtProtectDump, 0x00, 0x00, 0x02, 0x00, 0x40, 0x20, 0x09, 0x00, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	want := []Protect{{Dst: 64, Protected: true, Device: 9}, {Dst: 65}}
	if !reflect.DeepEqual(ev.Protects, want) {
		t.Fatalf("got %+v", ev.Protects)
	}
}

func TestDescribeTx(t *testing.T) {
	for _, c := range []struct {
		m    []byte
		want Request
	}{
		{ExtConnect(1, 2, 700, 2600), Request{"connect", 1, 2, 700, 2600}},
		{ExtInterrogate(0, 16, 300), Request{"interrogate", 0, 16, 300, -1}},
		{ExtTallyDumpReq(0, 3), Request{"tallyDump", 0, 3, -1, -1}},
		{ExtProtectDumpReq(0, 4, 5), Request{"protectDump", 0, 4, 5, -1}},
		{NamesReq(false, true, 0, 0, 2), Request{"srcNames", 0, -1, -1, -1}},
		{NamesReq(true, false, 1, 0, 2), Request{"dstNames", 1, -1, -1, -1}},
		{[]byte{0x7F, 1}, Request{"other", -1, -1, -1, -1}},
		{[]byte{CmdExtConnect, 0}, Request{"other", -1, -1, -1, -1}},
	} {
		if got := DescribeTx(c.m); got != c.want {
			t.Errorf("DescribeTx(% X) = %+v, want %+v", c.m, got, c.want)
		}
	}
}

func TestProtectBuildersRoundTrip(t *testing.T) {
	ev, err := Parse(ExtProtectDump(0, 0, 62, []Protect{{Protected: true, Device: 9}, {}, {Protected: true, Device: 0x123}}))
	if err != nil || len(ev.Protects) != 3 {
		t.Fatalf("dump: %v %+v", err, ev)
	}
	want := []Protect{{0, 0, 62, true, 9}, {0, 0, 63, false, 0}, {0, 0, 64, true, 0x123}}
	for i, p := range ev.Protects {
		if p != want[i] {
			t.Errorf("dump[%d] = %+v, want %+v", i, p, want[i])
		}
	}
	// The packing the Ultrix uses: protected by device 9 is 0x20 0x09.
	if m := ExtProtectDump(0, 0, 0, []Protect{{Protected: true, Device: 9}}); m[6] != 0x20 || m[7] != 9 {
		t.Errorf("packing % X", m)
	}
	for _, c := range []struct {
		msg  []byte
		want Protect
	}{
		{ExtProtectTally(1, 0, 300, true, 7), Protect{1, 0, 300, true, 7}},
		{ExtProtectConnected(1, 0, 300, 7), Protect{1, 0, 300, true, 7}},
		{ExtProtectDisconnected(1, 0, 300), Protect{1, 0, 300, false, 0}},
	} {
		ev, err := Parse(c.msg)
		if err != nil || len(ev.Protects) != 1 || ev.Protects[0] != c.want {
			t.Errorf("% X: %v %+v, want %+v", c.msg, err, ev.Protects, c.want)
		}
	}
}
