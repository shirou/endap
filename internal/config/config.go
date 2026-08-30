// Package config reads the endap config file: a key = value text format small
// enough not to justify a TOML dependency.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Config holds the tunables that shape ranking and recording.
type Config struct {
	// Sort is the default ordering of `endap list`: "recent" or "rank".
	Sort         string
	CwdBoost     float64
	FailPenalty  float64
	ShortPenalty float64
	ShortLen     int
	Ignore       *Ignore

	// Path is the file the settings were read from, empty if there was none.
	Path string
	// Warnings are non-fatal problems doctor reports: unknown keys, bad values.
	Warnings []string
	// Critical is the subset that changes what gets recorded, and that add
	// therefore prints on every command until it is fixed. An ignore pattern
	// that will not compile belongs here: the user believes a secret is being
	// filtered out, and it is not.
	Critical []string
}

// Defaults returns the configuration used when there is no config file.
func Defaults() *Config {
	ig, warnings := buildIgnore(nil)
	return &Config{
		Critical:     warnings,
		Sort:         "recent",
		CwdBoost:     2.0,
		FailPenalty:  0.5,
		ShortPenalty: 0.3,
		ShortLen:     3,
		Ignore:       ig,
	}
}

// Load reads the config file, applies environment overrides, and returns the
// result. A missing file is not an error. A file that cannot be read is.
func Load(path string) (*Config, error) {
	cfg := Defaults()
	cfg.Path = path
	var userIgnore []string

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		userIgnore, cfg.Warnings = parseInto(cfg, string(data), cfg.Warnings)
	case errors.Is(err, os.ErrNotExist):
		cfg.Path = ""
	default:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	envIgnore, warns := applyEnv(cfg)
	cfg.Warnings = append(cfg.Warnings, warns...)
	userIgnore = append(userIgnore, envIgnore...)

	ig, warnings := buildIgnore(userIgnore)
	cfg.Ignore = ig
	cfg.Critical = warnings
	return cfg, nil
}

// parseInto applies the settings in text to cfg and returns the ignore values.
//
// Two rules matter more than they look. The line is split at the first "=" only,
// because the built-in ignore patterns contain "=" and a naive split truncates
// them into something that silently matches nothing. And a "#" only starts a
// comment at the beginning of a line, because "ignore = /^#/" is a legitimate
// setting.
func parseInto(cfg *Config, text string, warnings []string) ([]string, []string) {
	var ignores []string
	for n, raw := range strings.Split(text, "\n") {
		lineno := n + 1
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.Index(trimmed, "=")
		if eq < 0 {
			warnings = append(warnings, fmt.Sprintf("%s:%d: no '=' in %q", cfg.Path, lineno, trimmed))
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		value := strings.TrimLeft(trimmed[eq+1:], " \t")
		if key == "ignore" {
			ignores = append(ignores, value)
			continue
		}
		if err := cfg.set(key, strings.TrimSpace(value)); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s:%d: %v", cfg.Path, lineno, err))
		}
	}
	return ignores, warnings
}

func (c *Config) set(key, value string) error {
	switch key {
	case "sort":
		if value != "recent" && value != "rank" {
			return fmt.Errorf("sort must be recent or rank")
		}
		c.Sort = value
	case "halflife":
		// Dropped along with the exponential decay it parameterised: recency
		// is now four fixed bands. Rejecting the key by name says so, where
		// the generic unknown-key warning would leave the user believing a
		// ranking they had tuned was still in effect.
		return fmt.Errorf("halflife is not supported; recency now uses fixed time bands")
	case "cwd_boost":
		return setFloat(&c.CwdBoost, key, value)
	case "fail_penalty":
		return setFloat(&c.FailPenalty, key, value)
	case "short_penalty":
		return setFloat(&c.ShortPenalty, key, value)
	case "short_len":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("short_len must be a non-negative integer")
		}
		c.ShortLen = n
	case "retention":
		// Dropped in v1 along with the fold-up it existed for: aggregated
		// counts cannot be merged across hosts, and a history tool that
		// deletes a year-old command by default is not one worth having.
		return fmt.Errorf("retention is not supported; compaction no longer folds old records")
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

func setFloat(dst *float64, key, value string) error {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("%s must be a number: %v", key, err)
	}
	// ParseFloat accepts "Inf" and "NaN" as ordinary values. Either one reaches
	// the score as a multiplier, and a NaN there makes the ranking comparison
	// stop being a strict weak ordering, which puts sort.Slice into undefined
	// behaviour. ("1e400" is not a way in: that overflows and errors above.)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("%s must be a finite number", key)
	}
	if f < 0 {
		return fmt.Errorf("%s must not be negative", key)
	}
	*dst = f
	return nil
}

// applyEnv lets every setting be overridden from the environment.
func applyEnv(cfg *Config) ([]string, []string) {
	var ignores, warnings []string
	for env, key := range map[string]string{
		"ENDAP_SORT":          "sort",
		"ENDAP_CWD_BOOST":     "cwd_boost",
		"ENDAP_FAIL_PENALTY":  "fail_penalty",
		"ENDAP_SHORT_PENALTY": "short_penalty",
		"ENDAP_SHORT_LEN":     "short_len",
	} {
		v, ok := os.LookupEnv(env)
		if !ok || v == "" {
			continue
		}
		if err := cfg.set(key, v); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", env, err))
		}
	}
	if v := os.Getenv("ENDAP_IGNORE"); v != "" {
		ignores = append(ignores, v)
	}
	return ignores, warnings
}

// Host returns the host name recorded in new records. ENDAP_HOST overrides it,
// which is the escape hatch for environments where host names collide.
func Host() string {
	if h := os.Getenv("ENDAP_HOST"); h != "" {
		return h
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
