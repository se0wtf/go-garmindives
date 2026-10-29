package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	charmLog "github.com/charmbracelet/log"
	garmin "github.com/se0wtf/go-garminconnect"
)

func main() {
	logger, err := newAppLogger(os.Stderr, os.Getenv("LOG_LEVEL"))
	if err != nil {
		charmLog.Error("invalid logger configuration", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, logger); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("interrupted")
			return
		}
		logger.Error("application failed", "err", err)
		os.Exit(1)
	}
}

func newAppLogger(output io.Writer, configuredLevel string) (*charmLog.Logger, error) {
	level := charmLog.InfoLevel
	if configuredLevel = strings.TrimSpace(configuredLevel); configuredLevel != "" {
		parsed, err := charmLog.ParseLevel(configuredLevel)
		if err != nil {
			return nil, fmt.Errorf("parse LOG_LEVEL: %w", err)
		}
		level = parsed
	}
	return charmLog.NewWithOptions(output, charmLog.Options{
		Level:           level,
		Prefix:          "go-garmindives",
		ReportTimestamp: true,
		TimeFormat:      time.TimeOnly,
	}), nil
}

func run(ctx context.Context, args []string, input *os.File, output io.Writer, logger *charmLog.Logger) error {
	if len(args) != 0 && args[0] == "serve" {
		return runServe(ctx, args[1:], output, logger)
	}
	if len(args) != 0 && args[0] == "sync" {
		args = args[1:]
	}
	httpClient, err := newGarminHTTPClientFromEnvironment(logger)
	if err != nil {
		return err
	}
	defer httpClient.CloseIdleConnections()
	prompt := newTerminalPrompter(input, output)
	dependencies := appDependencies{
		personalAPIKey:  strings.TrimSpace(os.Getenv("GARMIN_PERSONAL_API_KEY")),
		configDirectory: os.UserConfigDir,
		openDestination: openDestination,
		loadCached: func(path string) (diveClient, error) {
			return garmin.NewClientFromTokenFile(path, garmin.WithHTTPClient(httpClient))
		},
		loginFresh: func(ctx context.Context, path string) (diveClient, error) {
			return login(ctx, path, prompt, garmin.WithHTTPClient(httpClient))
		},
		newPersonal: func(key, path string) (diveClient, error) {
			return garmin.NewPersonalClient(key, garmin.WithTokenFile(path), garmin.WithHTTPClient(httpClient))
		},
	}
	return runWithDependencies(ctx, args, output, logger, dependencies)
}

type appDependencies struct {
	personalAPIKey  string
	configDirectory func() (string, error)
	openDestination func(context.Context, outputConfig) (diveDestination, error)
	loadCached      func(string) (diveClient, error)
	loginFresh      func(context.Context, string) (diveClient, error)
	newPersonal     func(string, string) (diveClient, error)
}

func runWithDependencies(ctx context.Context, args []string, output io.Writer, logger *charmLog.Logger, dependencies appDependencies) error {
	configDirectory, err := dependencies.configDirectory()
	if err != nil {
		return fmt.Errorf("find user configuration directory: %w", err)
	}
	flags := flag.NewFlagSet("go-garmindives", flag.ContinueOnError)
	flags.SetOutput(output)
	archivePath := flags.String("output", "dives.yaml", "path to the YAML dive archive (requires -output-type yaml)")
	outputType := flags.String("output-type", envDefault("GARMIN_OUTPUT_TYPE", outputSQLite), "output type: sqlite, yaml, or postgres")
	outputDSN := flags.String("output-dsn", strings.TrimSpace(os.Getenv("GARMIN_OUTPUT_DSN")), "SQL database path or PostgreSQL connection URL")
	tokenFile := "tokens.json"
	if dependencies.personalAPIKey != "" {
		tokenFile = "personal-tokens.json"
	}
	tokenPath := flags.String("token-file", filepath.Join(configDirectory, "go-garmindives", tokenFile), "path to the Garmin token cache")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(output, `go-garmindives downloads every Garmin Connect dive into YAML or a SQL database.
SQLite output defaults to dives.db in the current directory.
Later runs download new dives and add FIT data to dives that do not have it yet.

Usage:
  go-garmindives [sync] [options]
  go-garmindives serve [options]

Commands:
  sync   Download missing Garmin dives (default)
  serve  Serve stored SQL dives over a REST API

Options:`)
		flags.PrintDefaults()
		_, _ = fmt.Fprintln(output, `
Environment:
  GARMIN_PERSONAL_API_KEY  Garmin Personal API key; selects the official API
  GARMIN_REQUEST_INTERVAL  Minimum delay between Garmin requests (default 2s)
  GARMIN_EMAIL             Garmin account email; prompted when required and unset
  GARMIN_PASSWORD          Garmin password; securely prompted when required and unset
  GARMIN_MFA_CODE          Current one-time MFA code; prompted when Garmin requests it
  GARMIN_OUTPUT_TYPE       Output type: sqlite (default), yaml, or postgres
  GARMIN_OUTPUT_DSN        SQLite path (default dives.db) or PostgreSQL connection URL
  GARMIN_API_ADDR          REST API listen address (default 127.0.0.1:8080)
  LOG_LEVEL                Console verbosity: debug, info, warn, error, or fatal

The Personal API key is exchanged for a token each run and is never stored.
Without a key, a cached session or the legacy email/password login is used.
Passwords and MFA codes are never written to disk.`)
	}
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	kind := strings.ToLower(strings.TrimSpace(*outputType))
	endpoint := strings.TrimSpace(*outputDSN)
	switch kind {
	case outputYAML:
		endpoint = *archivePath
	case outputSQLite:
		if endpoint == "" {
			endpoint = "dives.db"
		}
	}
	config := outputConfig{Type: kind, Endpoint: endpoint}
	logger.Debug("configuration resolved", "output_type", config.Type, "token_cache", *tokenPath)

	destination, err := dependencies.openDestination(ctx, config)
	if err != nil {
		return fmt.Errorf("open %s output: %w", config.Type, err)
	}
	defer destination.Close()
	archive, existed, err := destination.Load(ctx)
	if err != nil {
		return fmt.Errorf("load %s output: %w", config.Type, err)
	}
	var loadCached clientLoader = func() (diveClient, error) {
		return dependencies.loadCached(*tokenPath)
	}
	var loginFresh clientLoader = func() (diveClient, error) {
		return dependencies.loginFresh(ctx, *tokenPath)
	}
	if dependencies.personalAPIKey != "" {
		logger.Debug("using Garmin Personal API")
		loadCached = func() (diveClient, error) {
			return dependencies.newPersonal(dependencies.personalAPIKey, *tokenPath)
		}
		// The library renews tokens using the key. A rejected key must not
		// trigger another exchange or a legacy credential prompt.
		loginFresh = nil
	}
	logger.Debug("stored dives loaded", "exists", existed, "dives", len(archive.Dives))
	logger.Info("synchronizing Garmin dives", "output_type", config.Type, "output", config.logDestination())
	added, err := syncWithAuthentication(ctx, &archive, loadCached, loginFresh, logger)
	if err != nil {
		return err
	}
	if added > 0 || !existed {
		if err := destination.Save(ctx, archive, added); err != nil {
			return fmt.Errorf("save %s output: %w", config.Type, err)
		}
		logger.Debug("dive output saved", "output_type", config.Type)
	}
	logger.Info("synchronization complete", "downloaded_dives", added, "total_dives", len(archive.Dives))
	return nil
}

type openSQLStoreFunc func(context.Context, string, string) (*sqlStore, error)
type serveAPIFunc func(context.Context, string, *sqlStore, *charmLog.Logger) error

func runServe(ctx context.Context, args []string, output io.Writer, logger *charmLog.Logger) error {
	return runServeWithDependencies(ctx, args, output, logger, openSQLStore, serveAPI)
}

func runServeWithDependencies(ctx context.Context, args []string, output io.Writer, logger *charmLog.Logger, openStore openSQLStoreFunc, serve serveAPIFunc) error {
	flags := flag.NewFlagSet("go-garmindives serve", flag.ContinueOnError)
	flags.SetOutput(output)
	outputType := flags.String("output-type", envDefault("GARMIN_OUTPUT_TYPE", outputSQLite), "SQL output type: sqlite or postgres")
	outputDSN := flags.String("output-dsn", strings.TrimSpace(os.Getenv("GARMIN_OUTPUT_DSN")), "SQLite database path or PostgreSQL connection URL")
	address := flags.String("listen", envDefault("GARMIN_API_ADDR", "127.0.0.1:8080"), "REST API listen address")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(output, `Serve synchronized dives from SQLite or PostgreSQL.

Usage:
  go-garmindives serve [options]

Options:`)
		flags.PrintDefaults()
		_, _ = fmt.Fprintln(output, `
Environment:
  GARMIN_OUTPUT_TYPE  SQL output type: sqlite (default) or postgres
  GARMIN_OUTPUT_DSN   SQLite database path or PostgreSQL connection URL
  GARMIN_API_ADDR     Listen address (default 127.0.0.1:8080)
  LOG_LEVEL           Console verbosity: debug, info, warn, error, or fatal`)
	}
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	kind := strings.ToLower(strings.TrimSpace(*outputType))
	if kind != outputSQLite && kind != outputPostgres {
		return fmt.Errorf("REST API requires sqlite or postgres output, got %q", kind)
	}
	endpoint := strings.TrimSpace(*outputDSN)
	if kind == outputSQLite && endpoint == "" {
		endpoint = "dives.db"
	}
	store, err := openStore(ctx, kind, endpoint)
	if err != nil {
		return err
	}
	defer store.Close()
	return serve(ctx, strings.TrimSpace(*address), store, logger)
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

type clientLoader func() (diveClient, error)

func syncWithAuthentication(ctx context.Context, archive *Archive, loadCached, loginFresh clientLoader, logger *charmLog.Logger) (int, error) {
	reportProgress := func(completed, total int) {
		logger.Info(fmt.Sprintf("synchronisation in progress... (%d/%d)", completed, total))
	}
	client, err := loadCached()
	if errors.Is(err, garmin.ErrNoTokenFile) && loginFresh != nil {
		logger.Debug("token cache not found; requesting Garmin credentials")
		client, err = loginFresh()
	} else if err == nil {
		logger.Debug("Garmin client ready")
	}
	if err != nil {
		return 0, fmt.Errorf("authenticate with Garmin: %w", err)
	}
	added, err := syncDives(ctx, client, archive, reportProgress)
	if !errors.Is(err, garmin.ErrSessionExpired) || loginFresh == nil {
		return added, err
	}
	logger.Info("cached Garmin session expired; requesting credentials")
	client, err = loginFresh()
	if err != nil {
		return 0, fmt.Errorf("reauthenticate with Garmin: %w", err)
	}
	return syncDives(ctx, client, archive, reportProgress)
}
