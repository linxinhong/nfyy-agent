package config

import "testing"

func TestLoad(t *testing.T) {
	cfg, err := Load(func(string) string { return "" })
	if err != nil || cfg.Listen != "127.0.0.1:8788" || cfg.DatabaseURL != "" {
		t.Fatalf("unexpected defaults: %+v, %v", cfg, err)
	}
	for _, address := range []string{"localhost", "localhost:nope", "localhost:65536", "localhost:-1"} {
		t.Run(address, func(t *testing.T) {
			_, err := Load(func(key string) string {
				if key == "YUANPU_SERVER_LISTEN" {
					return address
				}
				return ""
			})
			if err == nil {
				t.Fatal("invalid listen address accepted")
			}
		})
	}
	cfg, err = Load(func(key string) string {
		switch key {
		case "YUANPU_SERVER_LISTEN":
			return "[::1]:0"
		case "YUANPU_DATABASE_URL":
			return "postgres://example"
		}
		return ""
	})
	if err != nil || cfg.Listen != "[::1]:0" || cfg.DatabaseURL != "postgres://example" {
		t.Fatalf("explicit configuration was not preserved: %+v, %v", cfg, err)
	}
}
