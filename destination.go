package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	outputYAML     = "yaml"
	outputSQLite   = "sqlite"
	outputPostgres = "postgres"
)

type outputConfig struct {
	Type     string
	Endpoint string
}

type diveDestination interface {
	Load(context.Context) (Archive, bool, error)
	Save(context.Context, Archive, int) error
	Close() error
}

func openDestination(ctx context.Context, config outputConfig) (diveDestination, error) {
	switch strings.ToLower(strings.TrimSpace(config.Type)) {
	case outputYAML:
		if strings.TrimSpace(config.Endpoint) == "" {
			return nil, errors.New("YAML output path must not be empty")
		}
		return yamlDestination{path: config.Endpoint}, nil
	case outputSQLite, outputPostgres:
		store, err := openSQLStore(ctx, config.Type, config.Endpoint)
		if err != nil {
			return nil, err
		}
		return &sqlDestination{store: store}, nil
	default:
		return nil, fmt.Errorf("unsupported output type %q: use yaml, sqlite, or postgres", config.Type)
	}
}

type yamlDestination struct {
	path string
}

func (d yamlDestination) Load(context.Context) (Archive, bool, error) {
	return loadArchive(d.path)
}

func (d yamlDestination) Save(_ context.Context, archive Archive, _ int) error {
	return saveArchive(d.path, archive)
}

func (yamlDestination) Close() error { return nil }

type sqlDestination struct {
	store *sqlStore
}

func (d *sqlDestination) Load(ctx context.Context) (Archive, bool, error) {
	dives, err := d.store.DiveSyncState(ctx)
	if err != nil {
		return Archive{}, true, err
	}
	archive := newArchive()
	if dives != nil {
		archive.Dives = dives
	}
	return archive, true, nil
}

func (d *sqlDestination) Save(ctx context.Context, archive Archive, added int) error {
	if added < 0 || added > len(archive.Dives) {
		return fmt.Errorf("invalid number of new dives %d", added)
	}
	return d.store.SaveDives(ctx, archive.Dives[len(archive.Dives)-added:])
}

func (d *sqlDestination) Close() error { return d.store.Close() }
