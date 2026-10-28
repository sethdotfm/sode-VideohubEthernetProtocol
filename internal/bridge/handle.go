package bridge

import (
	"strings"

	"github.com/sethdotfm/sode-vep/internal/state"
	"github.com/sethdotfm/sode-vep/internal/swp08"
)

func (s *Server) handle(c *client, m []byte) {
	snap := s.store.Snapshot()
	if !snap.Up {
		return
	}
	switch m[0] {
	case swp08.CmdExtInterrogate:
		if len(m) < 5 {
			return
		}
		mx, l, dst := int(m[1]), int(m[2]), int(m[3])<<8|int(m[4])
		c.matrix.Store(int32(mx))
		if dst >= len(snap.Route) {
			return
		}
		c.msg(swp08.ExtTally(mx, l, dst, srcOf(snap, l, dst)))

	case swp08.CmdExtConnect:
		if len(m) < 7 {
			return
		}
		mx, l := int(m[1]), int(m[2])
		dst, src := int(m[3])<<8|int(m[4]), int(m[5])<<8|int(m[6])
		c.matrix.Store(int32(mx))
		switch {
		case l != 0:
			s.logf("connect on level %d ignored (only level 0 is routed)", l)
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

	case swp08.CmdExtTallyDumpReq:
		if len(m) < 3 {
			return
		}
		mx, l := int(m[1]), int(m[2])
		c.matrix.Store(int32(mx))
		for i := 0; i < len(snap.Route); i += 64 {
			end := min(i+64, len(snap.Route))
			srcs := make([]int, 0, end-i)
			for d := i; d < end; d++ {
				srcs = append(srcs, srcOf(snap, l, d))
			}
			c.msg(swp08.ExtTallyDump(mx, l, i, srcs))
		}

	case swp08.CmdExtProtectDumpReq:
		if len(m) < 3 {
			return
		}
		mx, l := int(m[1]), int(m[2])
		c.matrix.Store(int32(mx))
		first := 0
		if len(m) >= 5 {
			first = int(m[3])<<8 | int(m[4])
		}
		dev := s.protectDevice()
		for i := first; i < len(snap.Lock); i += 64 {
			end := min(i+64, len(snap.Lock))
			ps := make([]swp08.Protect, 0, end-i)
			for d := i; d < end; d++ {
				ps = append(ps, swp08.Protect{Protected: l == 0 && state.Locked(snap.Lock[d]), Device: dev})
			}
			c.msg(swp08.ExtProtectDump(mx, l, i, ps))
		}

	case swp08.CmdSrcNamesReq, swp08.CmdDstNamesReq, swp08.CmdExtSrcNamesReq, swp08.CmdExtDstNamesReq:
		dest := m[0] == swp08.CmdDstNamesReq || m[0] == swp08.CmdExtDstNamesReq
		ext := m[0] >= 0x80
		var mx, l int
		if ext {
			if len(m) < 4 {
				return
			}
			mx, l = int(m[1]), int(m[2])
		} else {
			if len(m) < 3 {
				return
			}
			mx, l = int(m[1]>>4), int(m[1]&0x0f)
		}
		c.matrix.Store(int32(mx))
		code := m[len(m)-1] & 3
		list := snap.Inputs
		if dest {
			list = snap.Outputs
		}
		size := 4 * (int(code) + 1)
		fit := make([]string, len(list))
		for i, n := range list {
			fit[i] = fold(n, size)
		}
		per := swp08.NamesPerMsg(code)
		for i := 0; i < len(fit); i += per {
			c.msg(swp08.NamesResp(dest, ext, mx, l, code, i, fit[i:min(i+per, len(fit))]))
		}

	default:
		s.logf("ignored cmd 0x%02X", m[0])
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
