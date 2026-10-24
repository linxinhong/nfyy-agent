package stream

import (
	"testing"
	"time"
)

func TestConfigZeroValueResolvesToFiniteLimits(t *testing.T) {
	resolved := (Config{}).Resolve()
	if resolved.BufferItems <= 0 || resolved.MaxOutputBytes <= 0 || resolved.DeliveryTimeout <= 0 {
		t.Fatalf("zero-value stream config did not resolve to finite limits: %+v", resolved)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("zero-value stream config: %v", err)
	}
}

func TestConfigRejectsUnsafeLimits(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  Config
	}{
		{name: "negative", cfg: Config{DeliveryTimeout: -time.Second}},
		{name: "no terminal slot", cfg: Config{BufferItems: 1}},
		{name: "terminal cannot fit byte budget", cfg: Config{MaxOutputBytes: 128}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); err == nil {
				t.Fatalf("Validate(%+v) succeeded", test.cfg)
			}
		})
	}
}
