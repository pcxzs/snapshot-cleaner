package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config is the optional configuration file. It exists as the escape hatch for
// layouts autodetection cannot infer: a user on an unusual setup declares the
// live/snapshot pairs by hand and everything else works unchanged.
//
// Format is deliberately trivial - "key = value", '#' comments - so it needs no
// parser dependency and is obvious to edit:
//
//	min-size = 100M
//	cache-min-size = 1M
//	pair = /home : /mnt/btr/snapshots/home
//	pair = / : /mnt/btr/snapshots/root
type Config struct {
	Pairs        []ConfigPair
	MinSize      string
	CacheMinSize string
	Provider     string
	Source       string // file the config came from, for doctor output
}

type ConfigPair struct {
	Live      string
	Snapshots string
}

// ConfigCandidates lists the files searched, in order of decreasing priority.
func ConfigCandidates(override string) []string {
	if override != "" {
		return []string{override}
	}
	var out []string
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		out = append(out, filepath.Join(x, appName, "config"))
	}
	if h, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(h, ".config", appName, "config"))
	}
	return append(out, "/etc/"+appName+".conf")
}

// LoadConfig reads the first config file that exists. A missing file is not an
// error; the tool is fully usable without one.
func LoadConfig(override string) (*Config, error) {
	for _, path := range ConfigCandidates(override) {
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		defer f.Close()
		cfg, err := parseConfig(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		cfg.Source = path
		return cfg, nil
	}
	return &Config{}, nil
}

func parseConfig(f io.Reader) (*Config, error) {
	cfg := &Config{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if i := strings.Index(text, "#"); i >= 0 {
			text = strings.TrimSpace(text[:i])
		}
		if text == "" {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "pair":
			live, snaps, ok := strings.Cut(value, ":")
			if !ok {
				return nil, fmt.Errorf("line %d: pair needs the form <live> : <snapshots dir>", line)
			}
			cfg.Pairs = append(cfg.Pairs, ConfigPair{
				Live:      filepath.Clean(strings.TrimSpace(live)),
				Snapshots: filepath.Clean(strings.TrimSpace(snaps)),
			})
		case "min-size":
			if _, err := ParseSize(value); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			cfg.MinSize = value
		case "cache-min-size":
			if _, err := ParseSize(value); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			cfg.CacheMinSize = value
		case "provider":
			cfg.Provider = value
		default:
			return nil, fmt.Errorf("line %d: unknown key %q", line, key)
		}
	}
	return cfg, sc.Err()
}
