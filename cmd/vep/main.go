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
	"sync"
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

// loadConfig reads the YAML file. On first start (no file) it is created
// from the VEP_* seed variables; after that the file wins.
func loadConfig(path string) config.Config {
	if path != "" {
		c, exists, err := config.Load(path)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		if exists {
			log.Printf("config: loaded %s (VEP_* seed variables are ignored once it exists)", path)
			return c
		}
	}
	c, err := config.FromEnv(config.Defaults())
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		log.Fatalf("config from environment: %v", err)
	}
	if path == "" {
		log.Printf("config: VEP_CONFIG not set; settings changes will not survive a restart")
		return c
	}
	if err := config.Save(path, c); err != nil {
		log.Fatalf("config: writing %s: %v", path, err)
	}
	log.Printf("config: created %s from environment", path)
	return c
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

	cfgPath := os.Getenv("VEP_CONFIG")
	var mu sync.RWMutex
	cfg := loadConfig(cfgPath)
	getCfg := func() config.Config { mu.RLock(); defer mu.RUnlock(); return cfg }

	store := state.New()
	debug := config.DebugFromEnv()
	hubCfg := func() config.Config { c := getCfg(); c.Debug = debug; return c }
	hc := hub.New(store, hubCfg, log.Printf)
	br := bridge.New(store, hc, func() int { return getCfg().ProtectDevice }, log.Printf)

	var saveMu sync.Mutex // one save at a time; disk I/O stays out of mu
	setCfg := func(n config.Config) error {
		if err := n.Validate(); err != nil {
			return err
		}
		saveMu.Lock()
		defer saveMu.Unlock()
		if cfgPath != "" {
			if err := config.Save(cfgPath, n); err != nil {
				return err
			}
		}
		mu.Lock()
		old := cfg
		cfg = n
		mu.Unlock()
		if old.Hub != n.Hub {
			hc.Reconfigure()
		}
		return nil
	}

	started := time.Now()
	ui := web.New(web.Deps{
		Store:     store,
		Config:    getCfg,
		SetConfig: setCfg,
		Clients:   br.Clients,
		Info: func() web.Info {
			return web.Info{Listen: swAddr, Started: started, Version: version.Label(), LastResync: hc.LastResync(), HubAddr: getCfg().HubAddr()}
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
	log.Printf("vep %s: gui on %s, swp08 on %s, hub %q", version.Label(), httpAddr, swAddr, getCfg().HubAddr())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-drained
}
