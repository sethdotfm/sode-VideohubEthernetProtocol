package config

import (
	"fmt"
	"sync"
)

// Holder owns the live Config: it loads (or seeds) the YAML file once, hands
// out copies, and persists changes. A failed save leaves the old config in
// place; disk I/O happens outside the read lock; saves run one at a time.
type Holder struct {
	path   string
	saveMu sync.Mutex // one save at a time; disk I/O stays out of mu
	mu     sync.RWMutex
	cfg    Config
}

// OpenHolder reads the YAML file at path. On first start (no file) the
// config is created from the VEP_* seed variables and written to path; after
// that the file wins. An empty path keeps the config in memory only. Errors
// carry the text main should log before exiting.
func OpenHolder(path string, logf func(string, ...any)) (*Holder, error) {
	if path != "" {
		c, exists, err := Load(path)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if exists {
			logf("config: loaded %s (VEP_* seed variables are ignored once it exists)", path)
			return &Holder{path: path, cfg: c}, nil
		}
	}
	c, err := FromEnv(Defaults())
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config from environment: %w", err)
	}
	if path == "" {
		logf("config: VEP_CONFIG not set; settings changes will not survive a restart")
		return &Holder{cfg: c}, nil
	}
	if err := Save(path, c); err != nil {
		return nil, fmt.Errorf("config: writing %s: %w", path, err)
	}
	logf("config: created %s from environment", path)
	return &Holder{path: path, cfg: c}, nil
}

// Get returns the current config.
func (h *Holder) Get() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Set validates n, saves it (if there is a file) and makes it current.
// hubChanged reports whether Hub differs from the previous config.
func (h *Holder) Set(n Config) (hubChanged bool, err error) {
	if err := n.Validate(); err != nil {
		return false, err
	}
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	if h.path != "" {
		if err := Save(h.path, n); err != nil {
			return false, err
		}
	}
	h.mu.Lock()
	old := h.cfg
	h.cfg = n
	h.mu.Unlock()
	return old.Hub != n.Hub, nil
}
