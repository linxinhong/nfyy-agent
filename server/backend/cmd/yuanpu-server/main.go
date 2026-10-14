package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"yuanpu.dev/server/internal/app"
	"yuanpu.dev/server/internal/config"
	"yuanpu.dev/server/internal/database"
	"yuanpu.dev/server/internal/identity"
)

var version = "0.1.0"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if len(os.Args) > 2 || (command != "serve" && command != "migrate" && command != "version" && command != "probe" && command != "bootstrap-admin") {
		return errors.New("usage: yuanpu-server [serve|migrate|version|probe|bootstrap-admin]")
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if command == "probe" {
		_, port, _ := net.SplitHostPort(cfg.Listen)
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://127.0.0.1:" + port + "/v1/ready")
		if err != nil {
			return errors.New("server probe failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("server is not ready")
		}
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if command == "migrate" {
		migrationCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		latest, err := db.Migrate(migrationCtx)
		if err != nil {
			return err
		}
		logger.Info("migrations complete", "schemaVersion", latest)
		return nil
	}
	if command == "bootstrap-admin" {
		if _, err := db.Ready(ctx); err != nil {
			return errors.New("migrate the database before bootstrapping an administrator")
		}
		var input struct {
			Email    string `json:"email"`
			Name     string `json:"name"`
			Password string `json:"password"`
		}
		decoder := json.NewDecoder(os.Stdin)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return errors.New("bootstrap-admin expects JSON on standard input")
		}
		user, err := identity.New(db.Pool()).BootstrapAdmin(ctx, input.Email, input.Name, input.Password)
		if err != nil {
			return err
		}
		fmt.Printf("administrator created: %s (%s)\n", user.Name, user.Email)
		return nil
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	defer listener.Close()
	handler, err := app.NewHandler(version, db, cfg.ArtifactRoot)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	completed := make(chan error, 1)
	go func() { completed <- server.Serve(listener) }()
	logger.Info("listening", "address", listener.Addr().String(), "version", version)
	select {
	case err := <-completed:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.New("graceful shutdown timed out")
		}
		if err := <-completed; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		logger.Info("shutdown complete")
		return nil
	}
}
