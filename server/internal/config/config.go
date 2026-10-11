package config

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

type Config struct {
	Listen      string
	DatabaseURL string
}

// Load uses an injected lookup so configuration tests do not mutate process state.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Listen:      strings.TrimSpace(getenv("YUANPU_SERVER_LISTEN")),
		DatabaseURL: getenv("YUANPU_DATABASE_URL"),
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8788"
	}
	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return Config{}, errors.New("YUANPU_SERVER_LISTEN must be a host:port address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return Config{}, errors.New("YUANPU_SERVER_LISTEN port must be between 0 and 65535")
	}
	return cfg, nil
}
