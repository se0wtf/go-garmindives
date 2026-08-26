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
	prompt := newTerminalPrompter(input, output)
	dependencies := appDependencies{
		configDirectory: os.UserConfigDir,
		loadArchive:     loadArchive,
		saveArchive:     saveArchive,
		loadCached: func(path string) (diveClient, error) {
			return garmin.NewClientFromTokenFile(path)
		},
		loginFresh: func(ctx context.Context, path string) (diveClient, error) {
			return login(ctx, path, prompt)
		},
	}
	return runWithDependencies(ctx, args, output, logger, dependencies)
}

type appDependencies struct {
	configDirectory func() (string, error)
	loadArchive     func(string) (Archive, bool, error)
	saveArchive     func(string, Archive) error
	loadCached      func(string) (diveClient, error)
	loginFresh      func(context.Context, string) (diveClient, error)
}

func runWithDependencies(ctx context.Context, args []string, output io.Writer, logger *charmLog.Logger, dependencies appDependencies) error {
	configDirectory, err := dependencies.configDirectory()
	if err != nil {
		return fmt.Errorf("find user configuration directory: %w", err)
	}
	flags := flag.NewFlagSet("go-garmindives", flag.ContinueOnError)
	flags.SetOutput(output)
	archivePath := flags.String("output", "dives.yaml", "path to the YAML dive archive")
	tokenPath := flags.String("token-file", filepath.Join(configDirectory, "go-garmindives", "tokens.json"), "path to the Garmin token cache")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(output, `go-garmindives downloads every Garmin Connect dive into one YAML archive.
On later runs it reuses cached tokens and fetches only activity IDs that are
not already present in the archive.

Usage:
  go-garmindives [options]

Options:`)
		flags.PrintDefaults()
		_, _ = fmt.Fprintln(output, `
Environment:
  GARMIN_EMAIL      Garmin account email; prompted when required and unset
  GARMIN_PASSWORD   Garmin password; securely prompted when required and unset
  GARMIN_MFA_CODE   Current one-time MFA code; prompted when Garmin requests it
  LOG_LEVEL         Console verbosity: debug, info, warn, error, or fatal

Credentials are only needed when no reusable token cache exists. Passwords and
MFA codes are never written to disk.`)
	}
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	logger.Debug("configuration resolved", "archive", *archivePath, "token_cache", *tokenPath)

	archive, existed, err := dependencies.loadArchive(*archivePath)
	if err != nil {
		return err
	}
	loadCached := func() (diveClient, error) {
		return dependencies.loadCached(*tokenPath)
	}
	loginFresh := func() (diveClient, error) {
		return dependencies.loginFresh(ctx, *tokenPath)
	}
	logger.Debug("archive loaded", "exists", existed, "dives", len(archive.Dives))
	logger.Info("synchronizing Garmin dives")
	added, err := syncWithAuthentication(ctx, &archive, loadCached, loginFresh, logger)
	if err != nil {
		return err
	}
	if added > 0 || !existed {
		if err := dependencies.saveArchive(*archivePath, archive); err != nil {
			return err
		}
		logger.Debug("archive saved", "path", *archivePath)
	}
	logger.Info("synchronization complete", "new_dives", added, "total_dives", len(archive.Dives))
	return nil
}

type clientLoader func() (diveClient, error)

func syncWithAuthentication(ctx context.Context, archive *Archive, loadCached, loginFresh clientLoader, logger *charmLog.Logger) (int, error) {
	client, err := loadCached()
	if errors.Is(err, garmin.ErrNoTokenFile) {
		logger.Debug("token cache not found; requesting Garmin credentials")
		client, err = loginFresh()
	} else if err == nil {
		logger.Debug("using cached Garmin session")
	}
	if err != nil {
		return 0, fmt.Errorf("authenticate with Garmin: %w", err)
	}
	added, err := syncDives(ctx, client, archive)
	if !errors.Is(err, garmin.ErrSessionExpired) {
		return added, err
	}
	logger.Info("cached Garmin session expired; requesting credentials")
	client, err = loginFresh()
	if err != nil {
		return 0, fmt.Errorf("reauthenticate with Garmin: %w", err)
	}
	return syncDives(ctx, client, archive)
}
