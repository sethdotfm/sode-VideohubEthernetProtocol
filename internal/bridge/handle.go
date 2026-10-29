package bridge

import (
	"strings"

	"github.com/sethdotfm/sode-vep/internal/state"
	"github.com/sethdotfm/sode-vep/internal/swp08"
)

// request is a parsed client command; only the fields its command uses are set.
type request struct {
	mx, level, dst, src, first int
	dest, ext                  bool
	code                       byte
}

// parseRequest checks the message is long enough for its command and records
// the client's matrix. ok is false for short or unknown messages.
func (s *Server) parseRequest(c *client, m []byte) (r request, ok bool) {
	need := 0
	switch m[0] {
	case swp08.CmdExtInterrogate:
		need = 5
	case swp08.CmdExtConnect:
		need = 7
	case swp08.CmdExtTallyDumpReq, swp08.CmdExtProtectDumpReq:
		need = 3
	case swp08.CmdExtSrcNamesReq, swp08.CmdExtDstNamesReq:
		need = 4
	case swp08.CmdSrcNamesReq, swp08.CmdDstNamesReq:
		need = 3
	default:
		s.logf("ignored cmd 0x%02X", m[0])
		return r, false
	}
	if len(m) < need {
		return r, false
	}
	r.ext = m[0] >= 0x80
	if r.ext {
		r.mx, r.level = int(m[1]), int(m[2])
	} else {
		r.mx, r.level = int(m[1]>>4), int(m[1]&0x0f)
	}
	c.matrix.Store(int32(r.mx))
	switch m[0] {
	case swp08.CmdExtInterrogate:
		r.dst = int(m[3])<<8 | int(m[4])
	case swp08.CmdExtConnect:
		r.dst, r.src = int(m[3])<<8|int(m[4]), int(m[5])<<8|int(m[6])
	case swp08.CmdExtProtectDumpReq:
		if len(m) >= 5 {
			r.first = int(m[3])<<8 | int(m[4])
		}
	case swp08.CmdSrcNamesReq, swp08.CmdDstNamesReq, swp08.CmdExtSrcNamesReq, swp08.CmdExtDstNamesReq:
		r.dest = m[0] == swp08.CmdDstNamesReq || m[0] == swp08.CmdExtDstNamesReq
		r.code = m[len(m)-1] & 3
	}
	return r, true
}

func (s *Server) handle(c *client, m []byte) {
	snap := s.store.Snapshot()
	if !snap.Up {
		return
	}
	r, ok := s.parseRequest(c, m)
	if !ok {
		return
	}
	switch m[0] {
	case swp08.CmdExtInterrogate:
		if r.dst < len(snap.Route) {
			c.msg(swp08.ExtTally(r.mx, r.level, r.dst, srcOf(snap, r.level, r.dst)))
		}
	case swp08.CmdExtConnect:
		s.connect(r, snap)
	case swp08.CmdExtTallyDumpReq:
		chunks(len(snap.Route), 64, func(i, end int) {
			srcs := make([]int, 0, end-i)
			for d := i; d < end; d++ {
				srcs = append(srcs, srcOf(snap, r.level, d))
			}
			c.msg(swp08.ExtTallyDump(r.mx, r.level, i, srcs))
		})
	case swp08.CmdExtProtectDumpReq:
		dev := s.protectDevice()
		chunks(len(snap.Lock)-r.first, 64, func(i, end int) {
			ps := make([]swp08.Protect, 0, end-i)
			for d := r.first + i; d < r.first+end; d++ {
				ps = append(ps, swp08.Protect{Protected: r.level == 0 && state.Locked(snap.Lock[d]), Device: dev})
			}
			c.msg(swp08.ExtProtectDump(r.mx, r.level, r.first+i, ps))
		})
	default:
		names(c, r, snap)
	}
}

// connect queues a take for the hub unless it is out of range, on another level or locked.
func (s *Server) connect(r request, snap state.Snapshot) {
	switch {
	case r.level != 0:
		s.logf("connect on level %d ignored (only level 0 is routed)", r.level)
	case r.dst >= len(snap.Route) || r.src >= len(snap.Inputs):
		s.logf("connect out of range: dst %d src %d", r.dst, r.src)
	case state.Locked(snap.Lock[r.dst]):
		s.logf("connect dst %d dropped: output is locked", r.dst)
	default:
		select {
		case s.takes <- take{r.dst, r.src}:
		default:
			s.logf("take %d <- %d dropped: queue full", r.dst, r.src)
		}
	}
}

// names answers a names request: std or extended, cut to the asked length, folded to ASCII.
func names(c *client, r request, snap state.Snapshot) {
	list := snap.Inputs
	if r.dest {
		list = snap.Outputs
	}
	size := swp08.NameLen(r.code)
	chunks(len(list), swp08.NamesPerMsg(r.code), func(i, end int) {
		fit := make([]string, 0, end-i)
		for _, n := range list[i:end] {
			fit = append(fit, fold(n, size))
		}
		c.msg(swp08.NamesResp(r.dest, r.ext, r.mx, r.level, r.code, i, fit))
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
