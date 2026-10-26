package relaynode_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/relaynode"
)

func validSpec() relaynode.Spec {
	return relaynode.Spec{
		Region:       "us-east",
		TURNURLs:     []string{"turn:relay-1.example.com:3478?transport=udp"},
		LifecycleURL: "http://10.0.0.5:9091/lifecycle",
	}
}

func TestSpecValidate_should_accept_a_complete_spec(t *testing.T) {
	err := validSpec().Validate()

	assert.NoError(t, err)
}

func TestSpecValidate_should_reject_bad_fields(t *testing.T) {
	cases := map[string]func(*relaynode.Spec){
		"empty region":            func(s *relaynode.Spec) { s.Region = "" },
		"uppercase region":        func(s *relaynode.Spec) { s.Region = "US-East" },
		"region with space":       func(s *relaynode.Spec) { s.Region = "us east" },
		"long region":             func(s *relaynode.Spec) { s.Region = strings.Repeat("a", 65) },
		"no turn urls":            func(s *relaynode.Spec) { s.TURNURLs = nil },
		"too many turn urls":      func(s *relaynode.Spec) { s.TURNURLs = make([]string, 17) },
		"https turn url":          func(s *relaynode.Spec) { s.TURNURLs = []string{"https://relay.example.com"} },
		"turn url without host":   func(s *relaynode.Spec) { s.TURNURLs = []string{"turn:"} },
		"empty lifecycle url":     func(s *relaynode.Spec) { s.LifecycleURL = "" },
		"relative lifecycle url":  func(s *relaynode.Spec) { s.LifecycleURL = "/lifecycle" },
		"ftp lifecycle url":       func(s *relaynode.Spec) { s.LifecycleURL = "ftp://10.0.0.5/lifecycle" },
		"lifecycle url with user": func(s *relaynode.Spec) { s.LifecycleURL = "http://user:pw@10.0.0.5:9091/lifecycle" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			mutate(&spec)

			err := spec.Validate()

			assert.ErrorIs(t, err, relaynode.ErrInvalid)
		})
	}
}

func TestSpecValidate_should_accept_turns_urls(t *testing.T) {
	spec := validSpec()
	spec.TURNURLs = []string{"turns:relay-1.example.com:5349?transport=tcp"}

	err := spec.Validate()

	assert.NoError(t, err)
}
