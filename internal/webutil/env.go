package webutil

import (
	"os"
	"regexp"
	"strings"
)

// envAllowlist is the set of host environment variable names safe to pass
// to a child process. Everything else (DB credentials, signing keys, SMTP
// passwords) stays in the parent.
var envAllowlist = []string{"PATH", "HOME", "USER", "LANG", "TZ"}

// envSecretRE catches anything that smells like a credential even if the
// caller passed it explicitly via extra (defence in depth).
var envSecretRE = regexp.MustCompile(`(?i)(secret|token|password|key|credential)`)

// ScrubEnv returns a child-process environment containing only the safe
// host variables plus the explicit extras. Extras whose name matches
// envSecretRE are dropped.
//
// The intent is to prevent a fleet plugin (or any other child subprocess)
// from inheriting JWT_SIGNING_KEY, DATABASE_URL, RELAY_SHARED_SECRET, or
// SMTP credentials by accident.
func ScrubEnv(extra []string) []string {
	out := make([]string, 0, len(envAllowlist)+len(extra))
	for _, name := range envAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	for _, kv := range extra {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || envSecretRE.MatchString(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
