package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

func SQLiteDatabasePath(endpoint string) (string, error) {
	if endpoint == ":memory:" || strings.HasPrefix(endpoint, "file::memory:") {
		return "", nil
	}
	if !strings.HasPrefix(endpoint, "file:") {
		return endpoint, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse SQLite DSN: %w", err)
	}
	path := parsed.Path
	if path == "" {
		path = parsed.Opaque
	}
	if path == "" {
		return "", errors.New("SQLite file DSN has no database path")
	}
	return path, nil
}
