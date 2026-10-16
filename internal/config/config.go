// Package config loads the server configuration: defaults, then the YAML file,
// then CONTEXTMATRIX_ARCHITECT_* env overrides. No secret but the backend HMAC
// key lives here; everything else arrives per session from ContextMatrix.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"time"

	kyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

const envPrefix = "CONTEXTMATRIX_ARCHITECT_"

// Config is the typed server configuration.
type Config struct {
	ContextMatrixURL string
	APIKey           string
	Port             int
	LogLevel         string
	StateDir         string
	// Pack is a pipeline pack directory; empty means the embedded default.
	Pack string
	// Models overrides the model per persona; the session's model is the
	// default for every persona not listed.
	Models map[string]string
	// MaxRunCostUSD is the harness per-run cap.
	MaxRunCostUSD float64
	MaxTurns      int

	ReplaySkew            time.Duration
	ReplayCacheSize       int
	MessageDedupTTL       time.Duration
	MessageDedupCacheSize int
}

// raw is the koanf wire shape; second-granularity durations are ints here and
// converted on load so Config never carries half-parsed values.
type raw struct {
	ContextMatrixURL       string            `koanf:"contextmatrix_url"`
	APIKey                 string            `koanf:"api_key"`
	Port                   int               `koanf:"port"`
	LogLevel               string            `koanf:"log_level"`
	StateDir               string            `koanf:"state_dir"`
	Pack                   string            `koanf:"pack"`
	Models                 map[string]string `koanf:"models"`
	MaxRunCostUSD          float64           `koanf:"max_run_cost_usd"`
	MaxTurns               int               `koanf:"max_turns"`
	ReplaySkewSeconds      int               `koanf:"webhook_replay_skew_seconds"`
	ReplayCacheSize        int               `koanf:"webhook_replay_cache_size"`
	MessageDedupTTLSeconds int               `koanf:"message_dedup_ttl_seconds"`
	MessageDedupCacheSize  int               `koanf:"message_dedup_cache_size"`
}

func defaults() raw {
	return raw{
		Port:                   9094,
		LogLevel:               "info",
		StateDir:               "/var/lib/contextmatrix-architect",
		Models:                 map[string]string{},
		MaxRunCostUSD:          2,
		MaxTurns:               40,
		ReplaySkewSeconds:      330,
		ReplayCacheSize:        10000,
		MessageDedupTTLSeconds: 600,
		MessageDedupCacheSize:  1000,
	}
}

func (r raw) toConfig() *Config {
	return &Config{
		ContextMatrixURL:      r.ContextMatrixURL,
		APIKey:                r.APIKey,
		Port:                  r.Port,
		LogLevel:              r.LogLevel,
		StateDir:              r.StateDir,
		Pack:                  r.Pack,
		Models:                r.Models,
		MaxRunCostUSD:         r.MaxRunCostUSD,
		MaxTurns:              r.MaxTurns,
		ReplaySkew:            time.Duration(r.ReplaySkewSeconds) * time.Second,
		ReplayCacheSize:       r.ReplayCacheSize,
		MessageDedupTTL:       time.Duration(r.MessageDedupTTLSeconds) * time.Second,
		MessageDedupCacheSize: r.MessageDedupCacheSize,
	}
}

// Load reads defaults, then path (a missing file is allowed; any other read or
// parse error is not; unknown keys are an error), then the environment.
// CONTEXTMATRIX_ARCHITECT_FOO_BAR maps to foo_bar and "__" nests:
// CONTEXTMATRIX_ARCHITECT_MODELS__REVIEWER maps to models.reviewer.
func Load(path string) (*Config, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(defaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}

	if path != "" {
		fk := koanf.New(".")
		if err := fk.Load(file.Provider(path), kyaml.Parser()); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("load config file %q: %w", path, err)
			}
		} else {
			if unknown := unknownKeys(fk.Keys()); len(unknown) > 0 {
				return nil, fmt.Errorf("config file %q: unknown keys: %s", path, strings.Join(unknown, ", "))
			}

			if err := k.Merge(fk); err != nil {
				return nil, fmt.Errorf("merge config file %q: %w", path, err)
			}
		}
	}

	envCb := func(s string) string {
		s = strings.ToLower(strings.TrimPrefix(s, envPrefix))

		return strings.ReplaceAll(s, "__", ".")
	}
	if err := k.Load(env.Provider(envPrefix, ".", envCb), nil); err != nil {
		return nil, fmt.Errorf("load env: %w", err)
	}

	var r raw
	if err := k.UnmarshalWithConf("", &r, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	return r.toConfig(), nil
}

// unknownKeys returns the sorted flattened file keys that are not koanf tags of
// raw. Keys under the models map are free-form.
func unknownKeys(keys []string) []string {
	known := map[string]bool{}
	t := reflect.TypeFor[raw]()

	for i := range t.NumField() {
		known[t.Field(i).Tag.Get("koanf")] = true
	}

	var out []string

	for _, key := range keys {
		if known[key] || strings.HasPrefix(key, "models.") {
			continue
		}

		out = append(out, key)
	}

	sort.Strings(out)

	return out
}

// DefaultsYAML renders the defaults through the same pipeline Load reads, so
// the printed key set is exactly what the loader accepts.
func DefaultsYAML() ([]byte, error) {
	k := koanf.New(".")
	if err := k.Load(structs.Provider(defaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}

	out, err := k.Marshal(kyaml.Parser())
	if err != nil {
		return nil, fmt.Errorf("marshal defaults: %w", err)
	}

	return out, nil
}

// Validate checks the loaded configuration.
func (c *Config) Validate() error {
	if c.ContextMatrixURL == "" {
		return errors.New("contextmatrix_url is required")
	}

	if len(c.APIKey) < 32 {
		return fmt.Errorf("api_key must be at least 32 characters, got %d", len(c.APIKey))
	}

	if c.StateDir == "" {
		return errors.New("state_dir is required")
	}

	if c.MaxRunCostUSD <= 0 {
		return errors.New("max_run_cost_usd must be positive")
	}

	if c.MaxTurns <= 0 {
		return errors.New("max_turns must be positive")
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be debug|info|warn|error, got %q", c.LogLevel)
	}

	return nil
}
