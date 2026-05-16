package webutil

import (
	"os"
	"regexp"
	"strings"
)

// envAllowlist is the set of host environment variable names safe to pass
// to a child process. Everything else (DB credentials, signing keys, SMTP
// passwords) stays in the parent.
var envAllowlist = map[string]bool{
	"PATH": true,
	"HOME": true,
	"USER": true,
	"LANG": true,
	"TZ":   true,
}

// envSecretRE catches anything that smells like a credential even if the
// caller passed it explicitly via extra (defence in depth).
var envSecretRE = regexp.MustCompile(`(?i)(secret|token|password|key|credential)`)

// ScrubEnv returns a child-process environment containing only the safe
// host variables plus the explicit extras. Variables whose name matches
// envSecretRE are dropped from both sets.
//
// The intent is to prevent a fleet plugin (or any other child subprocess)
// from inheriting JWT_SIGNING_KEY, DATABASE_URL, RELAY_SHARED_SECRET, or
// SMTP credentials by accident.
func ScrubEnv(extra []string) []string {
	out := make([]string, 0, len(envAllowlist)+len(extra))
	for _, kv := range os.Environ() {
		name := envName(kv)
		if !envAllowlist[name] {
			continue
		}
		if envSecretRE.MatchString(name) {
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range extra {
		name := envName(kv)
		if name == "" {
			continue
		}
		if envSecretRE.MatchString(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func envName(kv string) string {
	if i := strings.IndexByte(kv, '='); i > 0 {
		return kv[:i]
	}
	return ""
}
