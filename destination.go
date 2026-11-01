package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	outputSQLite   = "sqlite"
	outputPostgres = "postgres"
)

type outputConfig struct {
	Type     string
	Endpoint string
}

func (c outputConfig) logDestination() string {
	switch c.Type {
	case outputSQLite:
		if c.Endpoint == "" {
			return "dives.db"
		}
		if path, err := sqliteDatabasePath(c.Endpoint); err == nil {
			if path == "" {
				return ":memory:"
			}
			return path
		}
		return "configured SQLite database"
	case outputPostgres:
		// Parse the actual destination, including keyword DSNs and URL options,
		// without logging credentials, connection options, or parsing errors.
		config, err := pgx.ParseConfig(c.Endpoint)
		if err != nil {
			return "configured PostgreSQL database"
		}
		return net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))) + "/" + config.Database
	default:
		return "configured output"
	}
}

type diveDestination interface {
	Load(context.Context) (syncState, bool, error)
	Save(context.Context, syncState, int) error
	Close() error
}

func openDestination(ctx context.Context, config outputConfig) (diveDestination, error) {
	switch strings.ToLower(strings.TrimSpace(config.Type)) {
	case outputSQLite, outputPostgres:
		store, err := openSQLStore(ctx, config.Type, config.Endpoint)
		if err != nil {
			return nil, err
		}
		return &sqlDestination{store: store}, nil
	default:
		return nil, fmt.Errorf("unsupported output type %q: use sqlite or postgres", config.Type)
	}
}

type sqlDestination struct {
	store *sqlStore
}

func (d *sqlDestination) Load(ctx context.Context) (syncState, bool, error) {
	dives, err := d.store.DiveSyncState(ctx)
	if err != nil {
		return syncState{}, true, err
	}
	archive := newSyncState()
	if dives != nil {
		archive.Dives = dives
	}
	return archive, true, nil
}

func (d *sqlDestination) Save(ctx context.Context, archive syncState, added int) error {
	if added < 0 || added > len(archive.Dives) {
		return fmt.Errorf("invalid number of new dives %d", added)
	}
	return d.store.SaveDives(ctx, archive.Dives[len(archive.Dives)-added:])
}

func (d *sqlDestination) Close() error { return d.store.Close() }
