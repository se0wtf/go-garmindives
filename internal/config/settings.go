package config

import (
	"errors"
	"flag"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Setting is the single inventory for YAML leaves, ENV aliases, flags and help.
// Initialization/version fields and command actions are deliberately not settings.
type Setting struct {
	Key, Flag, Description string
	Env, Aliases           []string
	Secret                 bool
}

var settings = []Setting{
	{"database.type", "db-type", "SQL database type: sqlite or postgres", []string{"DIVELOG_OUTPUT_TYPE", "DIVELOG_DB_TYPE"}, []string{"output-type"}, false},
	{"database.uri", "db-uri", "SQLite filename or PostgreSQL URI", []string{"DIVELOG_OUTPUT_DSN", "DIVELOG_DB_URI"}, []string{"output-dsn"}, true},
	{"http.listen", "listen", "HTTP listen address", []string{"DIVELOG_API_ADDR"}, nil, false},
	{"http.cookie_secure", "cookie-secure", "Use HTTPS-only administrator session cookies", []string{"DIVELOG_COOKIE_SECURE"}, nil, false},
	{"http.public_url", "public-url", "Public HTTP(S) origin", []string{"DIVELOG_PUBLIC_URL"}, nil, false},
	{"auth.session_lifetime", "session-lifetime", "Administrator session lifetime", []string{"DIVELOG_SESSION_LIFETIME"}, nil, false},
	{"auth.idle_timeout", "session-idle-timeout", "Administrator session idle timeout", []string{"DIVELOG_SESSION_IDLE_TIMEOUT"}, nil, false},
	{"media.directory", "assets-dir", "Media assets directory", []string{"DIVELOG_ASSETS_DIR"}, nil, false},
	{"media.max_upload_bytes", "media-max-upload-bytes", "Maximum media upload bytes", []string{"DIVELOG_MEDIA_MAX_UPLOAD_BYTES"}, nil, false},
	{"media.pending_directory", "pending-dir", "Inbox directory, relative to assets unless absolute", []string{"DIVELOG_PENDING_DIR"}, nil, false},
	{"media.pending_scan_interval", "pending-scan-interval", "Pending inbox polling interval", []string{"DIVELOG_PENDING_SCAN_INTERVAL"}, nil, false},
	{"garmin.personal_api_key", "garmin-personal-api-key", "Garmin Personal API key", []string{"DIVELOG_GARMIN_PERSONAL_API_KEY"}, nil, true},
	{"garmin.email", "garmin-email", "Garmin legacy login email", []string{"DIVELOG_GARMIN_EMAIL"}, nil, true},
	{"garmin.password", "garmin-password", "Garmin legacy login password", []string{"DIVELOG_GARMIN_PASSWORD"}, nil, true},
	{"garmin.mfa_code", "garmin-mfa-code", "Garmin legacy one-time MFA code", []string{"DIVELOG_GARMIN_MFA_CODE"}, nil, true},
	{"garmin.token_file", "token-file", "CLI Garmin token cache; empty selects automatic path", []string{"DIVELOG_GARMIN_TOKEN_FILE"}, nil, false},
	{"garmin.resync_token_file", "garmin-resync-token-file", "Serve resync token cache; empty selects automatic path", []string{"DIVELOG_GARMIN_RESYNC_TOKEN_FILE"}, nil, false},
	{"garmin.request_interval", "garmin-request-interval", "Minimum interval between Garmin requests", []string{"DIVELOG_GARMIN_REQUEST_INTERVAL"}, nil, false},
	{"translation.api_key", "deepl-api-key", "DeepL API key; empty disables translation", []string{"DIVELOG_DEEPL_API_KEY"}, nil, true},
	{"translation.url", "deepl-api-url", "DeepL origin; empty selects Free/Pro from the API key", []string{"DIVELOG_DEEPL_API_URL"}, nil, false},
	{"translation.target_lang", "deepl-target-lang", "English translation locale: EN-GB or EN-US", []string{"DIVELOG_DEEPL_TARGET_LANG"}, nil, false},
	{"translation.glossary_id", "deepl-glossary-id", "Optional DeepL glossary ID", []string{"DIVELOG_DEEPL_GLOSSARY_ID"}, nil, false},
	{"translation.interval", "deepl-request-interval", "Minimum interval between translation requests", []string{"DIVELOG_DEEPL_REQUEST_INTERVAL"}, nil, false},
	{"geocoding.enabled", "geocoding-enabled", "Enable background country lookup", []string{"DIVELOG_GEOCODING_ENABLED"}, nil, false},
	{"geocoding.url", "geocoding-url", "Reverse geocoder origin", []string{"DIVELOG_GEOCODING_URL"}, nil, false},
	{"geocoding.interval", "geocoding-interval", "Minimum geocoding request interval", []string{"DIVELOG_GEOCODING_INTERVAL"}, nil, false},
	{"geocoding.user_agent", "geocoding-user-agent", "Identifying geocoder User-Agent", []string{"DIVELOG_GEOCODING_USER_AGENT"}, nil, false},
	{"logging.level", "log-level", "Log level: debug, info, warn, error or fatal", []string{"DIVELOG_LOG_LEVEL"}, nil, false},
}

func Settings() []Setting { return append([]Setting{}, settings...) }
func settingField(c *Config, key string) reflect.Value {
	value := reflect.ValueOf(c).Elem()
	for _, name := range strings.Split(key, ".") {
		found := false
		for i := 0; i < value.NumField(); i++ {
			tag := strings.Split(value.Type().Field(i).Tag.Get("yaml"), ",")[0]
			if tag == name {
				value = value.Field(i)
				found = true
				break
			}
		}
		if !found {
			panic("unknown registered setting: " + key)
		}
	}
	return value
}
func setSetting(c *Config, s Setting, value string) error {
	field := settingField(c, s.Key)
	switch field.Kind() {
	case reflect.String:
		if s.Key != "garmin.password" {
			value = strings.TrimSpace(value)
		}
		if s.Key == "database.type" || s.Key == "logging.level" {
			value = strings.ToLower(value)
		}
		if s.Key == "translation.target_lang" {
			value = strings.ToUpper(value)
		}
		field.SetString(value)
	case reflect.Bool:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%s must be true or false", s.Key)
		}
		field.SetBool(parsed)
	case reflect.Int64:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("%s must be an integer", s.Key)
		}
		field.SetInt(parsed)
	default:
		panic("unsupported setting type: " + s.Key)
	}
	return nil
}

type settingFlagValue struct {
	c *Config
	s Setting
}

func (v *settingFlagValue) String() string {
	if v.c == nil {
		return ""
	}
	if v.s.Secret {
		return ""
	}
	return fmt.Sprint(settingField(v.c, v.s.Key).Interface())
}
func (v *settingFlagValue) Set(value string) error { return setSetting(v.c, v.s, value) }
func (v *settingFlagValue) IsBoolFlag() bool {
	return v.c != nil && settingField(v.c, v.s.Key).Kind() == reflect.Bool
}

func RegisterFlags(fs *flag.FlagSet, c *Config) {
	for _, s := range settings {
		for _, name := range append([]string{s.Flag}, s.Aliases...) {
			fs.Var(&settingFlagValue{c, s}, name, s.Description+" ("+s.Key+"; "+strings.Join(s.Env, ", ")+")")
		}
	}
}

type flagOverride struct {
	setting     Setting
	name, value string
}

// Read only setting flags before validation. The command's FlagSet still owns
// action flags, unknown-argument rejection and help. Skip consumed string values
// so a password equal to '-config' can never change the selected file.
func settingOverrides(args []string) (map[string]flagOverride, string, error) {
	byFlag := map[string]Setting{}
	for _, s := range settings {
		byFlag[s.Flag] = s
		for _, alias := range s.Aliases {
			byFlag[alias] = s
		}
	}
	result := map[string]flagOverride{}
	path := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "h" || name == "help" {
			break
		}
		if name == "config" || name == "activity-id" {
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, "", fmt.Errorf("flag -%s requires a value", name)
				}
				value = args[i]
			}
			if name == "config" {
				path = value
				if strings.TrimSpace(path) == "" {
					return nil, "", errors.New("configuration path must not be empty")
				}
			}
			continue
		}
		s, ok := byFlag[name]
		if !ok {
			continue
		}
		if !hasValue {
			if settingFieldValueKind(s) == reflect.Bool {
				value = "true"
			} else {
				i++
				if i >= len(args) {
					return nil, "", fmt.Errorf("flag -%s requires a value", name)
				}
				value = args[i]
			}
		}
		result[s.Key] = flagOverride{s, name, value}
	}
	return result, path, nil
}
func settingFieldValueKind(s Setting) reflect.Kind {
	c := Default()
	return settingField(&c, s.Key).Kind()
}

// WithFile preserves every command override while re-resolving a saved YAML
// base. ENV/flag secrets remain only in Effective and never migrate into File.
func (c Loaded) WithFile(file Config) (Loaded, error) {
	overrides := map[string]flagOverride{}
	for _, s := range settings {
		if source := c.Overrides[s.Key]; strings.HasPrefix(source, "flag:") {
			overrides[s.Key] = flagOverride{s, strings.TrimPrefix(source, "flag:-"), fmt.Sprint(settingField(&c.Effective, s.Key).Interface())}
		}
	}
	return resolve(c.Path, file, overrides)
}
