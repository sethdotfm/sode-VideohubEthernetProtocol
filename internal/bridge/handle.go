package bridge

import (
	"strings"

	"github.com/sethdotfm/sode-vep/internal/state"
	"github.com/sethdotfm/sode-vep/internal/swp08"
)

// minLen is the shortest valid message for each command the server answers.
var minLen = map[byte]int{
	swp08.CmdExtInterrogate:    5,
	swp08.CmdExtConnect:        7,
	swp08.CmdExtTallyDumpReq:   3,
	swp08.CmdExtProtectDumpReq: 3,
	swp08.CmdExtSrcNamesReq:    4,
	swp08.CmdExtDstNamesReq:    4,
	swp08.CmdSrcNamesReq:       3,
	swp08.CmdDstNamesReq:       3,
}

// handle answers one client message. minLen is the only length guard.
func (s *Server) handle(c *client, m []byte) {
	snap := s.store.Snapshot()
	if !snap.Up {
		return
	}
	need, known := minLen[m[0]]
	if !known {
		s.logf("ignored cmd 0x%02X", m[0])
		return
	}
	if len(m) < need {
		return
	}
	// Extended messages carry matrix and level in separate bytes, std ones
	// in the high and low nibble of byte 1. Pushes go to the matrix the
	// client last used.
	mx, level := int(m[1]), int(m[2])
	if m[0] < 0x80 {
		mx, level = int(m[1]>>4), int(m[1]&0x0f)
	}
	c.matrix.Store(int32(mx))
	switch m[0] {
	case swp08.CmdExtInterrogate:
		dst := int(m[3])<<8 | int(m[4])
		if dst < len(snap.Route) {
			c.msg(swp08.ExtTally(mx, level, dst, srcOf(snap, level, dst)))
		}
	case swp08.CmdExtConnect:
		s.connect(snap, level, int(m[3])<<8|int(m[4]), int(m[5])<<8|int(m[6]))
	case swp08.CmdExtTallyDumpReq:
		chunks(len(snap.Route), 64, func(i, end int) {
			srcs := make([]int, 0, end-i)
			for d := i; d < end; d++ {
				srcs = append(srcs, srcOf(snap, level, d))
			}
			c.msg(swp08.ExtTallyDump(mx, level, i, srcs))
		})
	case swp08.CmdExtProtectDumpReq:
		first := 0
		if len(m) >= 5 {
			first = int(m[3])<<8 | int(m[4])
		}
		dev := s.protectDevice()
		chunks(len(snap.Lock)-first, 64, func(i, end int) {
			ps := make([]swp08.Protect, 0, end-i)
			for d := first + i; d < first+end; d++ {
				ps = append(ps, swp08.Protect{Protected: level == 0 && state.Locked(snap.Lock[d]), Device: dev})
			}
			c.msg(swp08.ExtProtectDump(mx, level, first+i, ps))
		})
	case swp08.CmdSrcNamesReq, swp08.CmdExtSrcNamesReq:
		names(c, snap, m, false, mx, level)
	case swp08.CmdDstNamesReq, swp08.CmdExtDstNamesReq:
		names(c, snap, m, true, mx, level)
	}
}

// connect queues a take for the hub unless it is out of range, on another level or locked.
func (s *Server) connect(snap state.Snapshot, level, dst, src int) {
	switch {
	case level != 0:
		s.logf("connect on level %d ignored (only level 0 is routed)", level)
	case dst >= len(snap.Route) || src >= len(snap.Inputs):
		s.logf("connect out of range: dst %d src %d", dst, src)
	case state.Locked(snap.Lock[dst]):
		s.logf("connect dst %d dropped: output is locked", dst)
	default:
		select {
		case s.takes <- take{dst, src}:
		default:
			s.logf("take %d <- %d dropped: queue full", dst, src)
		}
	}
}

// names answers a names request: std or extended, cut to the asked length, folded to ASCII.
func names(c *client, snap state.Snapshot, m []byte, dest bool, mx, level int) {
	ext := m[0] >= 0x80
	code := m[len(m)-1] & 3
	list := snap.Inputs
	if dest {
		list = snap.Outputs
	}
	size := swp08.NameLen(code)
	chunks(len(list), swp08.NamesPerMsg(code), func(i, end int) {
		fit := make([]string, 0, end-i)
		for _, n := range list[i:end] {
			fit = append(fit, fold(n, size))
		}
		c.msg(swp08.NamesResp(dest, ext, mx, level, code, i, fit))
	})
}

// chunks calls fn for consecutive [i, end) ranges of at most per items covering total.
func chunks(total, per int, fn func(i, end int)) {
	for i := 0; i < total; i += per {
		fn(i, min(i+per, total))
	}
}

// srcOf is the source on a level: level 0 is the cache, others route nothing.
func srcOf(snap state.Snapshot, level, dst int) int {
	if level != 0 || snap.Route[dst] < 0 {
		return none
	}
	return snap.Route[dst]
}

const latin = "AAAAAAACEEEEIIIIDNOOOOO*OUUUUYTsaaaaaaaceeeeiiiidnooooo/ouuuuyty"

// fold reduces a name to printable ASCII, at most n characters.
func fold(name string, n int) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 0x20 && r < 0x7F:
			b.WriteRune(r)
		case r >= 0xC0 && r <= 0xFF:
			b.WriteByte(latin[r-0xC0])
		default:
			b.WriteByte('?')
		}
		if b.Len() >= n {
			break
		}
	}
	return b.String()
}
