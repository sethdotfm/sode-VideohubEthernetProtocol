// Command vep bridges a Blackmagic Videohub to Sode: SW-P-08 router on one
// side, Videohub Ethernet Protocol client on the other, one-page GUI on :8081.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sethdotfm/sode-vep/internal/bridge"
	"github.com/sethdotfm/sode-vep/internal/config"
	"github.com/sethdotfm/sode-vep/internal/hub"
	"github.com/sethdotfm/sode-vep/internal/state"
	"github.com/sethdotfm/sode-vep/internal/version"
	"github.com/sethdotfm/sode-vep/internal/web"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	health := flag.Bool("healthcheck", false, "probe the local /healthz and exit")
	flag.Parse()
	httpAddr := env("VEP_LISTEN_HTTP", ":8081")
	swAddr := env("VEP_LISTEN_SWP08", ":8910")

	if *health {
		_, port, _ := net.SplitHostPort(httpAddr)
		cl := http.Client{Timeout: 2 * time.Second}
		resp, err := cl.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}

	debug, _ := strconv.ParseBool(os.Getenv("VEP_DEBUG")) // wire log: env only, not a setting
	h, err := config.OpenHolder(os.Getenv("VEP_CONFIG"), log.Printf)
	if err != nil {
		log.Fatal(err)
	}

	store := state.New()
	hc := hub.New(store, h.Get, log.Printf, debug)
	br := bridge.New(store, hc, func() int { return h.Get().ProtectDevice }, log.Printf)

	started := time.Now()
	ui := web.New(web.Deps{
		Store:  store,
		Config: h.Get,
		SetConfig: func(n config.Config) error {
			hubChanged, err := h.Set(n)
			if hubChanged {
				hc.Reconfigure()
			}
			return err
		},
		Clients: br.Clients,
		Info: func() web.Info {
			return web.Info{Listen: swAddr, Started: started, Version: version.Label(), LastResync: hc.LastResync(), HubAddr: h.Get().HubAddr()}
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", swAddr)
	if err != nil {
		log.Fatalf("swp08 listen: %v", err)
	}
	go hc.Run(ctx)
	go func() {
		if err := br.Serve(ctx, ln); err != nil {
			log.Fatalf("swp08: %v", err)
		}
	}()

	// No WriteTimeout: it would cut the /events stream.
	srv := &http.Server{Addr: httpAddr, Handler: ui, ReadHeaderTimeout: 10 * time.Second}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("vep %s: gui on %s, swp08 on %s, hub %q", version.Label(), httpAddr, swAddr, h.Get().HubAddr())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-drained
}
