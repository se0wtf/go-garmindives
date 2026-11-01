package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

const configVersion = 1

var errNotConfigured = errors.New("not configured; complete setup through the REST API first")

type databaseConfig struct {
	Type string `yaml:"type" json:"type"`
	URI  string `yaml:"uri" json:"uri"`
}
type httpConfig struct {
	Listen       string `yaml:"listen" json:"listen"`
	CookieSecure bool   `yaml:"cookie_secure" json:"cookie_secure"`
	PublicURL    string `yaml:"public_url,omitempty" json:"public_url,omitempty"`
}
type authConfig struct {
	SessionLifetime string `yaml:"session_lifetime" json:"session_lifetime"`
	IdleTimeout     string `yaml:"idle_timeout" json:"idle_timeout"`
}
type mediaConfig struct {
	Directory      string `yaml:"directory" json:"directory"`
	MaxUploadBytes int64  `yaml:"max_upload_bytes" json:"max_upload_bytes"`
}
type applicationConfig struct {
	Version     int            `yaml:"version" json:"version"`
	Initialized bool           `yaml:"initialized" json:"initialized"`
	InstanceID  string         `yaml:"instance_id,omitempty" json:"-"`
	Database    databaseConfig `yaml:"database" json:"database"`
	HTTP        httpConfig     `yaml:"http" json:"http"`
	Auth        authConfig     `yaml:"auth" json:"auth"`
	Media       mediaConfig    `yaml:"media" json:"media"`
}
type loadedConfig struct {
	Path            string
	File, Effective applicationConfig
	Overrides       map[string]string
}

func defaultApplicationConfig() applicationConfig {
	return applicationConfig{Version: configVersion, Database: databaseConfig{Type: outputSQLite, URI: "dives.db"}, HTTP: httpConfig{Listen: "127.0.0.1:8080", CookieSecure: true}, Auth: authConfig{SessionLifetime: "24h", IdleTimeout: "2h"}, Media: mediaConfig{Directory: "assets", MaxUploadBytes: defaultMediaMaxUpload}}
}

func configPathFromArgs(args []string) (string, error) {
	path := envDefault("GARMIN_CONFIG", "config.yaml")
	for i := 0; i < len(args); i++ {
		arg := strings.TrimLeft(args[i], "-")
		if arg == "config" && strings.HasPrefix(args[i], "-") {
			i++
			if i == len(args) {
				return "", errors.New("-config requires a path")
			}
			path = args[i]
		} else if strings.HasPrefix(arg, "config=") && strings.HasPrefix(args[i], "-") {
			path = strings.TrimPrefix(arg, "config=")
		}
	}
	if strings.TrimSpace(path) == "" {
		return "", errors.New("configuration path must not be empty")
	}
	return filepath.Abs(path)
}
func loadApplicationConfig(args []string) (loadedConfig, error) {
	path, err := configPathFromArgs(args)
	if err != nil {
		return loadedConfig{}, err
	}
	c := defaultApplicationConfig()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return loadedConfig{}, errors.New("cannot read configuration file")
	}
	if err == nil {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&c); err != nil {
			return loadedConfig{}, errors.New("invalid YAML configuration")
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return loadedConfig{}, errors.New("configuration must contain one YAML document")
		}
	}
	return resolveApplicationConfig(path, c)
}
func resolveApplicationConfig(path string, file applicationConfig) (loadedConfig, error) {
	result := loadedConfig{Path: path, File: file, Effective: file, Overrides: map[string]string{}}
	c := &result.Effective
	for _, field := range []struct {
		key    string
		names  []string
		target *string
	}{
		{"database.type", []string{"GARMIN_OUTPUT_TYPE", "GARMIN_DB_TYPE"}, &c.Database.Type}, {"database.uri", []string{"GARMIN_OUTPUT_DSN", "GARMIN_DB_URI"}, &c.Database.URI},
		{"http.listen", []string{"GARMIN_API_ADDR"}, &c.HTTP.Listen}, {"http.public_url", []string{"GARMIN_PUBLIC_URL"}, &c.HTTP.PublicURL},
		{"auth.session_lifetime", []string{"GARMIN_SESSION_LIFETIME"}, &c.Auth.SessionLifetime}, {"auth.idle_timeout", []string{"GARMIN_SESSION_IDLE_TIMEOUT"}, &c.Auth.IdleTimeout},
		{"media.directory", []string{"GARMIN_ASSETS_DIR"}, &c.Media.Directory},
	} {
		for _, name := range field.names {
			if value, ok := os.LookupEnv(name); ok {
				*field.target = strings.TrimSpace(value)
				result.Overrides[field.key] = name
			}
		}
	}
	if value, ok := os.LookupEnv("GARMIN_COOKIE_SECURE"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return result, errors.New("GARMIN_COOKIE_SECURE must be true or false")
		}
		c.HTTP.CookieSecure = parsed
		result.Overrides["http.cookie_secure"] = "GARMIN_COOKIE_SECURE"
	}
	if value, ok := os.LookupEnv("GARMIN_MEDIA_MAX_UPLOAD_BYTES"); ok {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return result, errors.New("invalid GARMIN_MEDIA_MAX_UPLOAD_BYTES")
		}
		c.Media.MaxUploadBytes = parsed
		result.Overrides["media.max_upload_bytes"] = "GARMIN_MEDIA_MAX_UPLOAD_BYTES"
	}
	c.Database.Type = strings.ToLower(strings.TrimSpace(c.Database.Type))
	if err := validateApplicationConfig(*c); err != nil {
		return result, err
	}
	return result, nil
}
func validateDatabaseConfig(c databaseConfig) error {
	if len(c.URI) > 8192 {
		return errors.New("database.uri exceeds 8192 bytes")
	}
	switch c.Type {
	case outputSQLite:
		if strings.HasPrefix(c.URI, "file:") {
			u, err := url.Parse(c.URI)
			if err != nil || strings.EqualFold(u.Query().Get("mode"), "memory") || (u.Host != "" && u.Host != "localhost") {
				return errors.New("persistent local SQLite file URI is required")
			}
		} else if strings.Contains(c.URI, "://") {
			return errors.New("SQLite URI must be a filename or file: URI")
		}
		if strings.TrimSpace(c.URI) == "" {
			return errors.New("database.uri is required")
		}
		path, err := sqliteDatabasePath(c.URI)
		if err != nil {
			return errors.New("invalid SQLite URI")
		}
		if path == "" {
			return errors.New("persistent SQLite storage is required; memory databases cannot hold initialization and authentication")
		}
	case outputPostgres:
		if strings.TrimSpace(c.URI) == "" {
			return errors.New("database.uri is required")
		}
		if _, err := pgx.ParseConfig(c.URI); err != nil {
			return errors.New("invalid PostgreSQL URI")
		}
	default:
		return errors.New("database.type must be sqlite or postgres")
	}
	return nil
}
func validateApplicationConfig(c applicationConfig) error {
	if c.Version != configVersion {
		return fmt.Errorf("unsupported configuration version %d", c.Version)
	}
	if c.Initialized && !validTripID(c.InstanceID) {
		return errors.New("initialized configuration requires a valid instance_id")
	}
	if !c.Initialized && c.InstanceID != "" {
		return errors.New("instance_id requires initialized configuration")
	}
	if c.Database.Type != outputSQLite && c.Database.Type != outputPostgres {
		return errors.New("database.type must be sqlite or postgres")
	}
	if strings.TrimSpace(c.HTTP.Listen) == "" {
		return errors.New("http.listen must not be empty")
	}
	if c.HTTP.PublicURL != "" {
		u, err := url.Parse(c.HTTP.PublicURL)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("http.public_url must be an HTTP/HTTPS origin")
		}
	}
	lifetime, err := time.ParseDuration(c.Auth.SessionLifetime)
	if err != nil || lifetime < time.Minute || lifetime > 30*24*time.Hour {
		return errors.New("auth.session_lifetime must be between 1m and 720h")
	}
	idle, err := time.ParseDuration(c.Auth.IdleTimeout)
	if err != nil || idle < time.Minute || idle > lifetime {
		return errors.New("auth.idle_timeout must be between 1m and session_lifetime")
	}
	if c.Media.Directory == "" || c.Media.MaxUploadBytes <= 0 || c.Media.MaxUploadBytes > 1<<60 {
		return errors.New("invalid media directory or upload limit")
	}
	return nil
}
func (c loadedConfig) databaseOutput() outputConfig {
	uri := c.Effective.Database.URI
	if c.Effective.Database.Type == outputSQLite && strings.HasPrefix(uri, "file:") {
		if parsed, err := url.Parse(uri); err == nil {
			path := parsed.Path
			if path == "" {
				path = parsed.Opaque
			}
			if path != "" && !filepath.IsAbs(path) {
				parsed.Path = filepath.Join(filepath.Dir(c.Path), path)
				parsed.Opaque = ""
				uri = parsed.String()
			}
		}
	}
	if c.Effective.Database.Type == outputSQLite && uri != "" && !filepath.IsAbs(uri) && !strings.HasPrefix(uri, "file:") {
		uri = filepath.Join(filepath.Dir(c.Path), uri)
	}
	return outputConfig{Type: c.Effective.Database.Type, Endpoint: uri}
}
func (c loadedConfig) assetsDirectory() string {
	path := c.Effective.Media.Directory
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(c.Path), path)
	}
	return path
}

func writeApplicationConfig(path string, c applicationConfig) error {
	if err := validateApplicationConfig(c); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return errors.New("cannot encode configuration")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.New("cannot create configuration directory")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return errors.New("cannot create configuration file")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("cannot secure configuration file")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("cannot write configuration file")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("cannot sync configuration file")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot close configuration file")
	}
	if err := os.Rename(name, path); err != nil {
		return errors.New("cannot replace configuration file")
	}
	return nil
}
