package muse

import (
	"strings"
	"testing"
)

func TestConfigRejectsRemovedCredentialPolicy(t *testing.T) {
	for _, raw := range []string{
		"api-keys: [{api-key: old-secret}]",
		"api-keys: []",
		"host-credentials: false",
		"meta-limits: {max-concurrent: 2}",
	} {
		t.Run(strings.SplitN(raw, ":", 2)[0], func(t *testing.T) {
			cfg, err := ParseConfig([]byte(raw))
			if err == nil || cfg != nil {
				t.Fatalf("obsolete policy silently accepted: cfg=%+v error=%v", cfg, err)
			}
			if strings.Contains(err.Error(), "old-secret") {
				t.Fatalf("config error disclosed credential: %v", err)
			}
		})
	}
}
