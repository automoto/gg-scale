package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	env "github.com/caarlos0/env/v11"
)

// fieldMeta describes one env-backed Config field, derived from struct tags.
type fieldMeta struct {
	fieldName    string
	envName      string
	fileFallback bool
}

// fieldMetas reflects over Config once and returns the env metadata for every
// field that maps to an env var. Fields tagged env:"-" (or untagged) are
// skipped. It backs Load's _FILE pre-pass, error renaming, and DeclaredVars.
func fieldMetas() []fieldMeta {
	t := reflect.TypeOf(Config{})
	out := make([]fieldMeta, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := f.Tag.Get("env")
		if comma := strings.IndexByte(name, ','); comma >= 0 {
			name = name[:comma]
		}
		if name == "" || name == "-" {
			continue
		}
		out = append(out, fieldMeta{
			fieldName:    f.Name,
			envName:      name,
			fileFallback: f.Tag.Get("envFile") == "true",
		})
	}
	return out
}

// Load reads the environment and returns a populated Config or an error if
// any required variable is missing or any value is invalid.
func Load() (*Config, error) {
	envMap, err := buildEnvironment()
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := env.ParseWithOptions(cfg, env.Options{Environment: envMap}); err != nil {
		return nil, renameParseErrors(err)
	}
	cfg.normalize()
	if err := cfg.checkFields(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// buildEnvironment snapshots os.Environ into a map with two ggscale-specific
// adjustments the library doesn't make on its own:
//
//   - a var set to "" is dropped, so set-but-empty behaves as unset (defaults
//     apply, required checks still fire);
//   - the <NAME>_FILE convention is resolved for file-fallback fields: _FILE
//     wins over the plain var, the file content is trimmed of trailing
//     whitespace, and an empty/unreadable file is treated as unset / a hard
//     error respectively.
func buildEnvironment() (map[string]string, error) {
	m := make(map[string]string)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if key, val := kv[:eq], kv[eq+1:]; val != "" {
			m[key] = val
		}
	}
	for _, meta := range fieldMetas() {
		if !meta.fileFallback {
			continue
		}
		path := m[meta.envName+"_FILE"]
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // operator-supplied secret path is the documented contract
		if err != nil {
			return nil, fmt.Errorf("read %s_FILE %q: %w", meta.envName, path, err)
		}
		content := strings.TrimRight(string(data), " \t\r\n")
		if content == "" {
			delete(m, meta.envName)
			continue
		}
		m[meta.envName] = content
	}
	return m, nil
}

// renameParseErrors rewrites env.ParseError messages so they name the env var
// instead of the struct field. env.VarIsNotSetError already carries the env
// key, so it passes through untouched.
func renameParseErrors(err error) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}
	byField := make(map[string]string)
	for _, meta := range fieldMetas() {
		byField[meta.fieldName] = meta.envName
	}
	renamed := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var pe env.ParseError
		if errors.As(e, &pe) {
			name := byField[pe.Name]
			if name == "" {
				name = pe.Name
			}
			renamed = append(renamed, fmt.Errorf("%s: %w", name, pe.Err))
			continue
		}
		renamed = append(renamed, e)
	}
	return errors.Join(renamed...)
}

// normalize applies post-parse cleanup the struct tags can't express:
// the derived ControlPanelEnabled flag, the trimmed metrics token, and CSV
// list hygiene (trim each element, drop empties, nil when nothing remains).
func (c *Config) normalize() {
	c.MetricsAuthToken = strings.TrimSpace(c.MetricsAuthToken)
	c.ControlPanelEnabled = !c.ControlPanelDisabled
	c.CORSAllowedOrigins = normalizeCSV(c.CORSAllowedOrigins)
	c.DockerRegistryAllowlist = normalizeCSV(c.DockerRegistryAllowlist)
	c.TrustedProxyCIDRs = normalizeCSV(c.TrustedProxyCIDRs)
}

func normalizeCSV(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DeclaredVars returns the list of env-var names this package reads,
// including <name>_FILE variants for vars that support file fallback.
// Used by the drift test to compare against .env.example.
func DeclaredVars() []string {
	metas := fieldMetas()
	out := make([]string, 0, len(metas))
	for _, meta := range metas {
		out = append(out, meta.envName)
		if meta.fileFallback {
			out = append(out, meta.envName+"_FILE")
		}
	}
	return out
}
