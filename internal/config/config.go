// Package config holds the bridge settings: Defaults → FromEnv (seed, first
// start only) → Normalize/Validate, persisted as YAML.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Hub is the Videohub's address; the port defaults to 9990.
	Hub string `yaml:"hub" json:"hub"`
	// ProtectDevice is the SW-P-08 device number reported for outputs the
	// Videohub has locked.
	ProtectDevice int `yaml:"protectDevice" json:"protectDevice"`
	// ResyncSeconds is how often names, routes and locks are re-polled.
	ResyncSeconds int `yaml:"resyncSeconds" json:"resyncSeconds"`
	// Debug is no longer a setting: it comes from VEP_DEBUG at start-up (set
	// in compose). The field stays so config files written by earlier
	// versions still load (KnownFields), and is never saved or sent to the page.
	Debug bool `yaml:"debug,omitempty" json:"-"`
}

func Defaults() Config {
	return Config{ProtectDevice: 1, ResyncSeconds: 60}
}

// HubAddr is Hub with the default port filled in.
func (c Config) HubAddr() string {
	if c.Hub == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(c.Hub); err != nil {
		return net.JoinHostPort(strings.Trim(c.Hub, "[]"), "9990")
	}
	return c.Hub
}

func (c *Config) Normalize() {
	c.Hub = strings.TrimSpace(c.Hub)
}

func (c Config) Validate() error {
	if c.Hub != "" {
		if _, p, err := net.SplitHostPort(c.HubAddr()); err != nil {
			return fmt.Errorf("hub %q: %w", c.Hub, err)
		} else if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("hub %q: bad port", c.Hub)
		}
	}
	if c.ProtectDevice < 0 || c.ProtectDevice > 0x3FF {
		return errors.New("protectDevice must be 0-1023")
	}
	if c.ResyncSeconds < 5 || c.ResyncSeconds > 3600 {
		return errors.New("resyncSeconds must be 5-3600")
	}
	return nil
}

func envInt(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

// DebugFromEnv reports whether VEP_DEBUG asks for the Videohub wire traffic
// to be logged.
func DebugFromEnv() bool {
	v := os.Getenv("VEP_DEBUG")
	return v == "1" || strings.EqualFold(v, "true")
}

// FromEnv overlays VEP_* variables on base.
func FromEnv(base Config) (Config, error) {
	var err error
	if v := os.Getenv("VEP_HUB"); v != "" {
		base.Hub = v
	}
	if base.ProtectDevice, err = envInt("VEP_PROTECT_DEVICE", base.ProtectDevice); err != nil {
		return base, err
	}
	if base.ResyncSeconds, err = envInt("VEP_RESYNC_SECONDS", base.ResyncSeconds); err != nil {
		return base, err
	}
	return base, nil
}

// Load reads path over Defaults. exists is false if there is no file yet.
func Load(path string) (c Config, exists bool, err error) {
	c = Defaults()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil && err.Error() != "EOF" {
		return c, true, err
	}
	c.Normalize()
	return c, true, c.Validate()
}

// Save writes atomically (temp file + rename).
func Save(path string, c Config) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
