package app

import

// syncState contains the in-memory selection used by a synchronization run.
// Durable dive data is stored exclusively in SQLite or PostgreSQL.
fitdata "github.com/se0wtf/go-garmindives/internal/fit"

type syncState struct{ Dives []DiveRecord }
type DiveRecord struct {
	ID              int64
	Activity        any
	ActivityDetails any
	DiveDetails     any
	FIT             *fitdata.Document
	fitComplete     bool
	refreshMetadata bool
}

func newSyncState() syncState { return syncState{Dives: []DiveRecord{}} }
