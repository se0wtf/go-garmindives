package app

import (
	"context"
	"fmt"
	"strings"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
)

type diveDestination interface {
	Load(context.Context) (syncState, bool, error)
	Save(context.Context, syncState, int) error
	Close() error
}

func openDestination(ctx context.Context, config appconfig.Output) (diveDestination, error) {
	switch strings.ToLower(strings.TrimSpace(config.Type)) {
	case appconfig.SQLite, appconfig.Postgres:
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
