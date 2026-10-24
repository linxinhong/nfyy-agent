package config

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

type Config struct {
	Listen               string
	DatabaseURL          string
	ArtifactRoot         string
	IndustryRoot         string
	IndustryID           string
	BookStackURL         string
	BookStackTokenID     string
	BookStackTokenSecret string
	BookStackAllowHTTP   bool
}

// Load uses an injected lookup so configuration tests do not mutate process state.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Listen:               strings.TrimSpace(getenv("YUANPU_SERVER_LISTEN")),
		DatabaseURL:          getenv("YUANPU_DATABASE_URL"),
		ArtifactRoot:         strings.TrimSpace(getenv("YUANPU_ARTIFACT_ROOT")),
		IndustryRoot:         strings.TrimSpace(getenv("YUANPU_INDUSTRY_ROOT")),
		IndustryID:           strings.TrimSpace(getenv("YUANPU_INDUSTRY_ID")),
		BookStackURL:         strings.TrimSpace(getenv("YUANPU_BOOKSTACK_URL")),
		BookStackTokenID:     strings.TrimSpace(getenv("YUANPU_BOOKSTACK_TOKEN_ID")),
		BookStackTokenSecret: strings.TrimSpace(getenv("YUANPU_BOOKSTACK_TOKEN_SECRET")),
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8788"
	}
	if cfg.IndustryID == "" {
		cfg.IndustryID = "legal"
	}
	if value := strings.TrimSpace(getenv("YUANPU_BOOKSTACK_ALLOW_HTTP")); value != "" {
		allowed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("YUANPU_BOOKSTACK_ALLOW_HTTP must be a boolean")
		}
		cfg.BookStackAllowHTTP = allowed
	}
	if cfg.BookStackURL == "" && (cfg.BookStackTokenID != "" || cfg.BookStackTokenSecret != "") ||
		cfg.BookStackURL != "" && (cfg.BookStackTokenID == "" || cfg.BookStackTokenSecret == "") {
		return Config{}, errors.New("BookStack URL, token ID and secret must be configured together")
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
