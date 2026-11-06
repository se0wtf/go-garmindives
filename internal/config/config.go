package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	charmLog "github.com/charmbracelet/log"
	"github.com/jackc/pgx/v5"
	geolookup "github.com/se0wtf/go-garmindives/internal/geocoding"
	translation "github.com/se0wtf/go-garmindives/internal/translation"
	"go.yaml.in/yaml/v3"

	envutil "github.com/se0wtf/go-garmindives/internal/env"
	identity "github.com/se0wtf/go-garmindives/internal/identity"
)

const configVersion = 1

var ErrNotConfigured = errors.New("not configured; complete setup through the REST API first")

type Database struct {
	Type string `yaml:"type" json:"type"`
	URI  string `yaml:"uri" json:"uri"`
}
type HTTP struct {
	Listen       string `yaml:"listen" json:"listen"`
	CookieSecure bool   `yaml:"cookie_secure" json:"cookie_secure"`
	PublicURL    string `yaml:"public_url,omitempty" json:"public_url,omitempty"`
}
type Auth struct {
	SessionLifetime string `yaml:"session_lifetime" json:"session_lifetime"`
	IdleTimeout     string `yaml:"idle_timeout" json:"idle_timeout"`
}
type Media struct {
	Directory           string `yaml:"directory" json:"directory"`
	MaxUploadBytes      int64  `yaml:"max_upload_bytes" json:"max_upload_bytes"`
	PendingDirectory    string `yaml:"pending_directory" json:"pending_directory"`
	PendingScanInterval string `yaml:"pending_scan_interval" json:"pending_scan_interval"`
}
type Garmin struct {
	PersonalAPIKey  string `yaml:"personal_api_key,omitempty" json:"-"`
	Email           string `yaml:"email,omitempty" json:"-"`
	Password        string `yaml:"password,omitempty" json:"-"`
	MFACode         string `yaml:"mfa_code,omitempty" json:"-"`
	TokenFile       string `yaml:"token_file" json:"token_file"`
	ResyncTokenFile string `yaml:"resync_token_file" json:"resync_token_file"`
	RequestInterval string `yaml:"request_interval" json:"request_interval"`
}
type Logging struct {
	Level string `yaml:"level" json:"level"`
}
type Translation = translation.Settings
type Geocoding = geolookup.Settings
type Config struct {
	Version     int                  `yaml:"version" json:"version"`
	Initialized bool                 `yaml:"initialized" json:"initialized"`
	InstanceID  string               `yaml:"instance_id,omitempty" json:"-"`
	Database    Database             `yaml:"database" json:"database"`
	HTTP        HTTP                 `yaml:"http" json:"http"`
	Auth        Auth                 `yaml:"auth" json:"auth"`
	Media       Media                `yaml:"media" json:"media"`
	Garmin      Garmin               `yaml:"garmin" json:"garmin"`
	Translation translation.Settings `yaml:"translation" json:"translation"`
	Geocoding   geolookup.Settings   `yaml:"geocoding" json:"geocoding"`
	Logging     Logging              `yaml:"logging" json:"logging"`
}
type Loaded struct {
	Path            string
	File, Effective Config
	Overrides       map[string]string
}

func Default() Config {
	return Config{Version: configVersion, Database: Database{Type: SQLite, URI: "dives.db"}, HTTP: HTTP{Listen: "127.0.0.1:8080", CookieSecure: true}, Auth: Auth{SessionLifetime: "24h", IdleTimeout: "2h"}, Media: Media{Directory: "assets", MaxUploadBytes: DefaultMediaMaxUpload, PendingDirectory: "pending", PendingScanInterval: "60s"}, Garmin: Garmin{RequestInterval: "2s"}, Translation: translation.DefaultSettings(), Geocoding: geolookup.DefaultSettings(), Logging: Logging{Level: "info"}}
}

func Load(args []string) (Loaded, error) {
	overrides, path, err := settingOverrides(args)
	if path == "" {
		path = envutil.Default("DIVELOG_CONFIG", "config.yaml")
	}
	if err == nil {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		return Loaded{}, err
	}
	c := Default()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Loaded{}, errors.New("cannot read configuration file")
	}
	if err == nil {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&c); err != nil {
			return Loaded{}, errors.New("invalid YAML configuration")
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return Loaded{}, errors.New("configuration must contain one YAML document")
		}
	}
	return resolve(path, c, overrides)
}
func Resolve(path string, file Config) (Loaded, error) { return resolve(path, file, nil) }
func ResolveArgs(path string, file Config, args []string) (Loaded, error) {
	overrides, _, err := settingOverrides(args)
	if err != nil {
		return Loaded{}, err
	}
	return resolve(path, file, overrides)
}
func resolve(path string, file Config, flags map[string]flagOverride) (Loaded, error) {
	result := Loaded{Path: path, File: file, Effective: file, Overrides: map[string]string{}}
	for _, s := range settings {
		if _, overridden := flags[s.Key]; overridden {
			continue
		}
		for _, name := range s.Env {
			if value, ok := os.LookupEnv(name); ok {
				if err := setSetting(&result.Effective, s, value); err != nil {
					return result, err
				}
				result.Overrides[s.Key] = name
			}
		}
	}
	for key, override := range flags {
		if err := setSetting(&result.Effective, override.setting, override.value); err != nil {
			return result, err
		}
		result.Overrides[key] = "flag:-" + override.name
	}
	result.Effective.Database.Type = strings.ToLower(strings.TrimSpace(result.Effective.Database.Type))
	result.Effective.Translation.TargetLang = strings.ToUpper(strings.TrimSpace(result.Effective.Translation.TargetLang))
	result.Effective.Logging.Level = strings.ToLower(strings.TrimSpace(result.Effective.Logging.Level))
	if err := Validate(result.Effective); err != nil {
		return result, err
	}
	return result, nil
}
func ValidateDatabase(c Database) error {
	if len(c.URI) > 8192 {
		return errors.New("database.uri exceeds 8192 bytes")
	}
	switch c.Type {
	case SQLite:
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
		path, err := SQLiteDatabasePath(c.URI)
		if err != nil {
			return errors.New("invalid SQLite URI")
		}
		if path == "" {
			return errors.New("persistent SQLite storage is required; memory databases cannot hold initialization and authentication")
		}
	case Postgres:
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
func Validate(c Config) error {
	if c.Version != configVersion {
		return fmt.Errorf("unsupported configuration version %d", c.Version)
	}
	if c.Initialized && !identity.ValidID(c.InstanceID) {
		return errors.New("initialized configuration requires a valid instance_id")
	}
	if !c.Initialized && c.InstanceID != "" {
		return errors.New("instance_id requires initialized configuration")
	}
	if c.Database.Type != SQLite && c.Database.Type != Postgres {
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
	if strings.TrimSpace(c.Media.PendingDirectory) == "" {
		return errors.New("media.pending_directory must not be empty")
	}
	interval, err := time.ParseDuration(c.Media.PendingScanInterval)
	if err != nil || interval < time.Second || interval > 24*time.Hour {
		return errors.New("media.pending_scan_interval must be between 1s and 24h")
	}
	interval, err = time.ParseDuration(c.Garmin.RequestInterval)
	if err != nil || interval <= 0 {
		return errors.New("garmin.request_interval must be a positive duration")
	}
	if strings.ContainsAny(c.Garmin.PersonalAPIKey, "\r\n") {
		return errors.New("garmin.personal_api_key must not contain line breaks")
	}
	if _, err := translation.ParseSettings(c.Translation); err != nil {
		return err
	}
	if _, err := geolookup.ParseSettings(c.Geocoding); err != nil {
		return err
	}
	if _, err := charmLog.ParseLevel(c.Logging.Level); err != nil {
		return errors.New("logging.level is invalid")
	}
	return nil
}
func (c Loaded) DatabaseOutput() Output {
	uri := c.Effective.Database.URI
	if c.Effective.Database.Type == SQLite && strings.HasPrefix(uri, "file:") {
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
	if c.Effective.Database.Type == SQLite && uri != "" && !filepath.IsAbs(uri) && !strings.HasPrefix(uri, "file:") {
		uri = filepath.Join(filepath.Dir(c.Path), uri)
	}
	return Output{Type: c.Effective.Database.Type, Endpoint: uri}
}
func (c Loaded) AssetsDirectory() string {
	path := c.Effective.Media.Directory
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(c.Path), path)
	}
	return path
}

func (c Loaded) PendingDirectory() string {
	path := c.Effective.Media.PendingDirectory
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.AssetsDirectory(), path)
	}
	return filepath.Clean(path)
}

func (c Loaded) TokenFile(resync bool, configDirectory string) string {
	value := c.Effective.Garmin.TokenFile
	key := "garmin.token_file"
	if resync {
		value = c.Effective.Garmin.ResyncTokenFile
		key = "garmin.resync_token_file"
	}
	if value != "" {
		if filepath.IsAbs(value) || strings.HasPrefix(c.Overrides[key], "flag:") {
			return value
		}
		return filepath.Join(filepath.Dir(c.Path), value)
	}
	name := "tokens.json"
	if resync {
		name = "resync-personal-tokens.json"
	} else if c.Effective.Garmin.PersonalAPIKey != "" {
		name = "personal-tokens.json"
	}
	return filepath.Join(configDirectory, "go-garmindives", name)
}

func Write(path string, c Config) error {
	if err := Validate(c); err != nil {
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
