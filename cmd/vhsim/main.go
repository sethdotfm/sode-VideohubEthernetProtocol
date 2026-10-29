// Command vhsim runs the fake Blackmagic Videohub from internal/vhsim.
//
// Env: VHSIM_PORT (9990), VHSIM_INPUTS (40), VHSIM_OUTPUTS (40), VHSIM_MODEL,
// VHSIM_PANEL_SECONDS (hardware-panel route every N s, 0=off), VHSIM_LOCKS
// ("3,5": outputs locked by another client), VHSIM_DOWN_AFTER / VHSIM_DOWN_FOR
// (seconds: drop all clients and refuse connections, 0=off).
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sethdotfm/sode-vep/internal/vhsim"
)

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	nin, nout := envInt("VHSIM_INPUTS", 40), envInt("VHSIM_OUTPUTS", 40)
	if nin < 1 || nout < 1 {
		log.Fatal("VHSIM_INPUTS and VHSIM_OUTPUTS must be >= 1")
	}
	o := vhsim.Options{Model: envStr("VHSIM_MODEL", "Blackmagic Smart Videohub 40x40"), Inputs: nin, Outputs: nout}
	for _, f := range strings.Split(os.Getenv("VHSIM_LOCKS"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			o.Locks = append(o.Locks, n)
		}
	}
	s := vhsim.New(o)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if n := envInt("VHSIM_PANEL_SECONDS", 0); n > 0 {
		go s.RunPanel(ctx, time.Duration(n)*time.Second)
	}
	if after := envInt("VHSIM_DOWN_AFTER", 0); after > 0 {
		go s.RunDownCycle(ctx, time.Duration(after)*time.Second, time.Duration(envInt("VHSIM_DOWN_FOR", 10))*time.Second, log.Printf)
	}
	addr := ":" + strconv.Itoa(envInt("VHSIM_PORT", 9990))
	if err := s.Start(addr); err != nil {
		log.Fatal(err)
	}
	log.Printf("vhsim %dx%d on %s", nin, nout, addr)
	<-ctx.Done()
	s.Stop()
}
