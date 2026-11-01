package config

import (
	"net"
	"strconv"

	"github.com/jackc/pgx/v5"
)

const (
	SQLite   = "sqlite"
	Postgres = "postgres"
)

type Output struct {
	Type     string
	Endpoint string
}

func (c Output) LogDestination() string {
	switch c.Type {
	case SQLite:
		if c.Endpoint == "" {
			return "dives.db"
		}
		if path, err := SQLiteDatabasePath(c.Endpoint); err == nil {
			if path == "" {
				return ":memory:"
			}
			return path
		}
		return "configured SQLite database"
	case Postgres:
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
