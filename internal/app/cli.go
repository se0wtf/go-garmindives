package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	charmLog "github.com/charmbracelet/log"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	envutil "github.com/se0wtf/go-garmindives/internal/env"
	garminhttp "github.com/se0wtf/go-garmindives/internal/garminhttp"

	garmin "github.com/se0wtf/go-garminconnect"
)

func NewAppLogger(output io.Writer, configuredLevel string) (*charmLog.Logger, error) {
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

func Run(ctx context.Context, args []string, input *os.File, output io.Writer, logger *charmLog.Logger) error {
	if len(args) != 0 && args[0] == "serve" {
		return runServe(ctx, args[1:], output, logger)
	}
	if len(args) >= 2 && args[0] == "admin" && args[1] == "reset-password" {
		return resetAdminPassword(ctx, args[2:], input, output, logger)
	}
	if len(args) != 0 && args[0] == "sync" {
		args = args[1:]
	}
	config, err := appconfig.Load(args)
	if err != nil {
		return err
	}
	help := false
	for _, arg := range args {
		if arg == "-h" || arg == "-help" || arg == "--help" {
			help = true
		}
	}
	if !help && !config.File.Initialized {
		return appconfig.ErrNotConfigured
	}
	httpClient, err := garminhttp.FromEnvironment(logger)
	if err != nil {
		return err
	}
	defer httpClient.CloseIdleConnections()
	prompt := newTerminalPrompter(input, output)
	dependencies := appDependencies{
		config: &config, requireInitialization: true,
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
	config                *appconfig.Loaded
	requireInitialization bool
	personalAPIKey        string
	configDirectory       func() (string, error)
	openDestination       func(context.Context, appconfig.Output) (diveDestination, error)
	loadCached            func(string) (diveClient, error)
	loginFresh            func(context.Context, string) (diveClient, error)
	newPersonal           func(string, string) (diveClient, error)
}

func runWithDependencies(ctx context.Context, args []string, output io.Writer, logger *charmLog.Logger, dependencies appDependencies) error {
	configDirectory, err := dependencies.configDirectory()
	if err != nil {
		return fmt.Errorf("find user configuration directory: %w", err)
	}
	flags := flag.NewFlagSet("go-garmindives", flag.ContinueOnError)
	flags.SetOutput(output)
	kindDefault, dsnDefault := envutil.Default("GARMIN_OUTPUT_TYPE", appconfig.SQLite), strings.TrimSpace(os.Getenv("GARMIN_OUTPUT_DSN"))
	configPath := envutil.Default("GARMIN_CONFIG", "config.yaml")
	if dependencies.config != nil {
		kindDefault = dependencies.config.Effective.Database.Type
		dsnDefault = dependencies.config.DatabaseOutput().Endpoint
		configPath = dependencies.config.Path
	}
	flags.String("config", configPath, "YAML application configuration file")
	outputType := flags.String("output-type", kindDefault, "SQL database type: sqlite or postgres")
	outputDSN := flags.String("output-dsn", dsnDefault, "SQLite database path or PostgreSQL URI")
	activityID := flags.Int64("activity-id", 0, "refresh only this Garmin dive activity ID, preserving cached FIT and local media")
	tokenFile := "tokens.json"
	if dependencies.personalAPIKey != "" {
		tokenFile = "personal-tokens.json"
	}
	tokenPath := flags.String("token-file", filepath.Join(configDirectory, "go-garmindives", tokenFile), "path to the Garmin token cache")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(output, `go-garmindives downloads every Garmin Connect dive into SQLite or PostgreSQL.
SQLite output defaults to dives.db in the current directory.
Later runs download new dives and add FIT data to dives that do not have it yet.
Use -activity-id to refresh one dive's metadata, including edited notes.

Usage:
  go-garmindives [sync] [options]
  go-garmindives serve [options]

Commands:
  sync   Download missing Garmin dives (default)
  serve  Serve stored SQL dives over a REST API
  admin reset-password  Reset the local administrator password and revoke sessions

Options:`)
		flags.PrintDefaults()
		_, _ = fmt.Fprintln(output, `
Environment:
  GARMIN_PERSONAL_API_KEY  Garmin Personal API key; selects the official API
  GARMIN_REQUEST_INTERVAL  Minimum delay between Garmin requests (default 2s)
  GARMIN_EMAIL             Garmin account email; prompted when required and unset
  GARMIN_PASSWORD          Garmin password; securely prompted when required and unset
  GARMIN_MFA_CODE          Current one-time MFA code; prompted when Garmin requests it
  GARMIN_CONFIG            YAML application configuration (default config.yaml)
  GARMIN_DB_TYPE           SQL type; overrides database.type
  GARMIN_DB_URI            SQL URI; overrides database.uri
  GARMIN_OUTPUT_TYPE       Legacy SQL-type ENV alias: sqlite or postgres
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
	var targeted bool
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "activity-id" {
			targeted = true
		}
	})
	if targeted && *activityID <= 0 {
		return errors.New("activity ID must be positive")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments; use -activity-id to refresh one dive")
	}
	kind := strings.ToLower(strings.TrimSpace(*outputType))
	endpoint := strings.TrimSpace(*outputDSN)
	switch kind {
	case appconfig.SQLite:
		if endpoint == "" {
			endpoint = "dives.db"
		}
	}
	if err := appconfig.ValidateDatabase(appconfig.Database{Type: kind, URI: endpoint}); err != nil {
		return err
	}
	config := appconfig.Output{Type: kind, Endpoint: endpoint}
	logger.Debug("configuration resolved", "output_type", config.Type, "token_cache", *tokenPath)

	destination, err := dependencies.openDestination(ctx, config)
	if err != nil {
		return fmt.Errorf("open %s output: %w", config.Type, err)
	}
	defer destination.Close()
	if dependencies.requireInitialization {
		sql, ok := destination.(*sqlDestination)
		if !ok || dependencies.config == nil || !dependencies.config.File.Initialized {
			return appconfig.ErrNotConfigured
		}
		if err := sql.store.checkInitialization(ctx, dependencies.config.File.InstanceID); err != nil {
			return appconfig.ErrNotConfigured
		}
	}
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
	logger.Info("synchronizing Garmin dives", "output_type", config.Type, "output", config.LogDestination())
	if targeted {
		logger.Info("refreshing Garmin dive", "activity_id", *activityID)
	}
	added, err := syncWithAuthentication(ctx, &archive, loadCached, loginFresh, logger, *activityID)
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
type serveAPIFunc func(context.Context, string, *application, *charmLog.Logger) error

func runServe(ctx context.Context, args []string, output io.Writer, logger *charmLog.Logger) error {
	return runServeWithDependencies(ctx, args, output, logger, openSQLStore, func(ctx context.Context, address string, app *application, logger *charmLog.Logger) error {
		return serveAPI(ctx, address, app.Handler(), logger)
	})
}
func runServeWithDependencies(ctx context.Context, args []string, output io.Writer, logger *charmLog.Logger, openStore openSQLStoreFunc, serve serveAPIFunc) error {
	c, err := appconfig.Load(args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("go-garmindives serve", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.String("config", c.Path, "YAML application configuration file")
	kind := flags.String("output-type", c.Effective.Database.Type, "SQL database type: sqlite or postgres")
	uri := flags.String("output-dsn", c.Effective.Database.URI, "SQLite path or PostgreSQL URI")
	listen := flags.String("listen", c.Effective.HTTP.Listen, "REST API listen address")
	assets := flags.String("assets-dir", c.Effective.Media.Directory, "media assets directory")
	maxUpload := flags.String("media-max-upload-bytes", strconv.FormatInt(c.Effective.Media.MaxUploadBytes, 10), "maximum upload size")
	flags.Usage = func() {
		fmt.Fprintln(output, `Serve synchronized SQL dives with first-run setup and administrator authentication.
Usage: go-garmindives serve [options]
Configuration precedence: flags > ENV > YAML > defaults.
Environment: GARMIN_CONFIG GARMIN_DB_TYPE GARMIN_DB_URI GARMIN_OUTPUT_TYPE GARMIN_OUTPUT_DSN GARMIN_API_ADDR
GARMIN_ASSETS_DIR GARMIN_MEDIA_MAX_UPLOAD_BYTES GARMIN_COOKIE_SECURE GARMIN_PUBLIC_URL
GARMIN_PENDING_DIR GARMIN_PENDING_SCAN_INTERVAL
GARMIN_SESSION_LIFETIME GARMIN_SESSION_IDLE_TIMEOUT DEEPL_API_KEY DEEPL_TARGET_LANG DEEPL_REQUEST_INTERVAL
DEEPL_API_URL DEEPL_GLOSSARY_ID GARMIN_GEOCODING_ENABLED GARMIN_GEOCODING_URL
GARMIN_GEOCODING_INTERVAL GARMIN_GEOCODING_USER_AGENT LOG_LEVEL`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	parsedUpload, err := strconv.ParseInt(*maxUpload, 10, 64)
	if err != nil {
		return errors.New("invalid upload limit")
	}
	c.Effective.Database = appconfig.Database{Type: strings.ToLower(strings.TrimSpace(*kind)), URI: strings.TrimSpace(*uri)}
	c.Effective.HTTP.Listen = *listen
	c.Effective.Media.Directory, c.Effective.Media.MaxUploadBytes = *assets, parsedUpload
	fields := map[string]string{"output-type": "database.type", "output-dsn": "database.uri", "listen": "http.listen", "assets-dir": "media.directory", "media-max-upload-bytes": "media.max_upload_bytes"}
	flags.Visit(func(f *flag.Flag) {
		if key := fields[f.Name]; key != "" {
			c.Overrides[key] = "flag:-" + f.Name
		}
	})
	if err := appconfig.Validate(c.Effective); err != nil {
		return err
	}
	app := newApplication(ctx, c, logger, openStore)
	defer app.Close()
	return serve(ctx, c.Effective.HTTP.Listen, app, logger)
}

type clientLoader func() (diveClient, error)

func syncWithAuthentication(ctx context.Context, archive *syncState, loadCached, loginFresh clientLoader, logger *charmLog.Logger, activityID int64) (int, error) {
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
	synchronize := func(client diveClient) (int, error) {
		if activityID > 0 {
			return syncDive(ctx, client, archive, activityID, reportProgress)
		}
		return syncDives(ctx, client, archive, reportProgress)
	}
	added, err := synchronize(client)
	if !errors.Is(err, garmin.ErrSessionExpired) || loginFresh == nil {
		return added, err
	}
	logger.Info("cached Garmin session expired; requesting credentials")
	client, err = loginFresh()
	if err != nil {
		return 0, fmt.Errorf("reauthenticate with Garmin: %w", err)
	}
	return synchronize(client)
}
