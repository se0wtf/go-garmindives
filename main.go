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
	"syscall"

	garmin "github.com/se0wtf/go-garminconnect"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "go-garmindives:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input *os.File, output io.Writer) error {
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
	return runWithDependencies(ctx, args, output, dependencies)
}

type appDependencies struct {
	configDirectory func() (string, error)
	loadArchive     func(string) (Archive, bool, error)
	saveArchive     func(string, Archive) error
	loadCached      func(string) (diveClient, error)
	loginFresh      func(context.Context, string) (diveClient, error)
}

func runWithDependencies(ctx context.Context, args []string, output io.Writer, dependencies appDependencies) error {
	configDirectory, err := dependencies.configDirectory()
	if err != nil {
		return fmt.Errorf("find user configuration directory: %w", err)
	}
	flags := flag.NewFlagSet("go-garmindives", flag.ContinueOnError)
	flags.SetOutput(output)
	archivePath := flags.String("output", "dives.yaml", "path to the YAML dive archive")
	tokenPath := flags.String("token-file", filepath.Join(configDirectory, "go-garmindives", "tokens.json"), "path to the Garmin token cache")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}

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
	added, err := syncWithAuthentication(ctx, &archive, loadCached, loginFresh)
	if err != nil {
		return err
	}
	if added > 0 || !existed {
		if err := dependencies.saveArchive(*archivePath, archive); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(output, "%d new dive(s); %d total\n", added, len(archive.Dives))
	return err
}

type clientLoader func() (diveClient, error)

func syncWithAuthentication(ctx context.Context, archive *Archive, loadCached, loginFresh clientLoader) (int, error) {
	client, err := loadCached()
	if errors.Is(err, garmin.ErrNoTokenFile) {
		client, err = loginFresh()
	}
	if err != nil {
		return 0, fmt.Errorf("authenticate with Garmin: %w", err)
	}
	added, err := syncDives(ctx, client, archive)
	if !errors.Is(err, garmin.ErrSessionExpired) {
		return added, err
	}
	client, err = loginFresh()
	if err != nil {
		return 0, fmt.Errorf("reauthenticate with Garmin: %w", err)
	}
	return syncDives(ctx, client, archive)
}
