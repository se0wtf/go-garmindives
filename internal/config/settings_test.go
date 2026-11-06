package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func clearSettingsEnvironment(t *testing.T) {
	t.Helper()
	names := []string{"DIVELOG_CONFIG"}
	for _, s := range Settings() {
		names = append(names, s.Env...)
	}
	for _, name := range names {
		old, exists := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(name, old)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

// Every operator-controlled YAML leaf must have a default, ENV and flag entry.
func TestSettingsInventoryCoversEveryYAMLLeaf(t *testing.T) {
	leaves := map[string]bool{}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, prefix string) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			key := prefix + name
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, key+".")
			} else {
				leaves[key] = true
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	for _, state := range []string{"version", "initialized", "instance_id"} {
		delete(leaves, state)
	}
	names := map[string]bool{}
	for _, s := range Settings() {
		if !leaves[s.Key] {
			t.Errorf("unknown or duplicate YAML key %s", s.Key)
		}
		delete(leaves, s.Key)
		if len(s.Env) == 0 || s.Flag == "" {
			t.Errorf("missing channel for %s", s.Key)
		}
		for _, name := range append(append([]string{s.Flag}, s.Aliases...), s.Env...) {
			if names[name] {
				t.Errorf("duplicate flag/ENV %s", name)
			}
			names[name] = true
		}
	}
	if len(leaves) > 0 {
		t.Fatalf("settings missing registry entries: %v", leaves)
	}
}

func TestEverySettingPrecedenceAndFlagRegistration(t *testing.T) {
	samples := map[string][3]string{
		"database.type":               {"postgres", "sqlite", "postgres"},
		"database.uri":                {"file.db", "environment.db", "flag.db"},
		"http.listen":                 {"127.0.0.1:8100", "127.0.0.1:8200", "127.0.0.1:8300"},
		"http.cookie_secure":          {"false", "true", "false"},
		"http.public_url":             {"https://yaml.example", "https://env.example", "https://flag.example"},
		"auth.session_lifetime":       {"90h", "100h", "120h"},
		"auth.idle_timeout":           {"3h", "4h", "5h"},
		"media.directory":             {"yaml-assets", "env-assets", "flag-assets"},
		"media.max_upload_bytes":      {"100000", "200000", "300000"},
		"media.pending_directory":     {"yaml-inbox", "env-inbox", "flag-inbox"},
		"media.pending_scan_interval": {"10s", "20s", "30s"},
		"garmin.personal_api_key":     {"synthetic-yaml-key", "synthetic-env-key", "synthetic-flag-key"},
		"garmin.email":                {"yaml@example.invalid", "env@example.invalid", "flag@example.invalid"},
		"garmin.password":             {"  yaml password  ", "  env password  ", "  flag password  "},
		"garmin.mfa_code":             {"111111", "222222", "333333"},
		"garmin.token_file":           {"yaml-tokens.json", "env-tokens.json", "flag-tokens.json"},
		"garmin.resync_token_file":    {"yaml-resync.json", "env-resync.json", "flag-resync.json"},
		"garmin.request_interval":     {"3s", "4s", "5s"},
		"translation.api_key":         {"synthetic-yaml:fx", "synthetic-env:fx", "synthetic-flag:fx"},
		"translation.url":             {"https://yaml.example", "https://env.example", "https://flag.example"},
		"translation.target_lang":     {"EN-US", "EN-GB", "EN-US"},
		"translation.glossary_id":     {"yaml-glossary", "env-glossary", "flag-glossary"},
		"translation.interval":        {"3s", "4s", "5s"},
		"geocoding.enabled":           {"false", "true", "false"},
		"geocoding.url":               {"https://yaml.example", "https://env.example", "https://flag.example"},
		"geocoding.interval":          {"16s", "17s", "18s"},
		"geocoding.user_agent":        {"yaml application", "env application", "flag application"},
		"logging.level":               {"debug", "warn", "error"},
	}
	if len(samples) != len(Settings()) {
		t.Fatal("new setting needs source/precedence test values")
	}
	for _, s := range Settings() {
		t.Run(s.Key, func(t *testing.T) {
			clearSettingsEnvironment(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			base, err := Load([]string{"-config", path})
			if err != nil {
				t.Fatal(err)
			}
			defaults := Default()
			if !reflect.DeepEqual(base.Effective, defaults) || len(base.Overrides) != 0 {
				t.Fatal("missing file must use defaults")
			}
			values := samples[s.Key]
			// Build YAML independently of the registry's setter to exercise actual decoding.
			var scalar any = values[0]
			switch settingField(&defaults, s.Key).Kind() {
			case reflect.Bool:
				scalar = values[0] == "true"
			case reflect.Int64:
				scalar = int64(100000)
			}
			parts := strings.Split(s.Key, ".")
			data, err := yaml.Marshal(map[string]any{parts[0]: map[string]any{parts[1]: scalar}})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			check := func(args []string, want, source string) Loaded {
				t.Helper()
				c, err := Load(append([]string{"-config", path}, args...))
				if err != nil {
					t.Fatal(err)
				}
				if got := fmt.Sprint(settingField(&c.Effective, s.Key).Interface()); got != want {
					t.Fatalf("%s: got %q want %q", source, got, want)
				}
				if c.Overrides[s.Key] != source {
					t.Fatalf("source=%q want %q", c.Overrides[s.Key], source)
				}
				if got := fmt.Sprint(settingField(&c.File, s.Key).Interface()); got != values[0] {
					t.Fatal("override modified file settings")
				}
				return c
			}
			check(nil, values[0], "")
			env := s.Env[len(s.Env)-1]
			t.Setenv(env, values[1])
			check(nil, values[1], env)
			args := []string{"--" + s.Flag + "=" + values[2]}
			c := check(args, values[2], "flag:-"+s.Flag)
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			effective := defaults
			RegisterFlags(fs, &effective)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(settingField(&effective, s.Key).Interface()); got != values[2] {
				t.Fatal("registered flag did not update effective setting")
			}
			updated, err := c.WithFile(c.File)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(updated.Effective, c.Effective) || !reflect.DeepEqual(updated.Overrides, c.Overrides) {
				t.Fatal("saved config lost flag overrides")
			}
		})
	}
}

func TestFlagsOverrideInvalidEnvironmentAndAliases(t *testing.T) {
	clearSettingsEnvironment(t)
	for _, tc := range []struct{ env, flag, value string }{
		{"DIVELOG_COOKIE_SECURE", "cookie-secure", "false"},
		{"DIVELOG_MEDIA_MAX_UPLOAD_BYTES", "media-max-upload-bytes", "100000"},
		{"DIVELOG_GARMIN_REQUEST_INTERVAL", "garmin-request-interval", "5s"},
		{"DIVELOG_DEEPL_REQUEST_INTERVAL", "deepl-request-interval", "3s"},
		{"DIVELOG_DEEPL_API_URL", "deepl-api-url", "https://example.invalid"},
		{"DIVELOG_LOG_LEVEL", "log-level", "warn"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, "invalid")
			if _, err := Resolve("config.yaml", Default()); err == nil {
				t.Fatal("invalid ENV accepted")
			}
			c, err := ResolveArgs("config.yaml", Default(), []string{"-" + tc.flag, tc.value})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.WithFile(c.File); err != nil {
				t.Fatal("override lost during setup/save", err)
			}
		})
	}
	t.Setenv("DIVELOG_OUTPUT_TYPE", "invalid-legacy")
	t.Setenv("DIVELOG_DB_TYPE", "postgres")
	// The canonical ENV value must win without validating the legacy value first.
	c, err := Resolve("config.yaml", Default())
	if err != nil || c.Effective.Database.Type != "postgres" {
		t.Fatal("canonical ENV alias precedence", err)
	}
	c, err = ResolveArgs("config.yaml", Default(), []string{"-db-type", "postgres", "-output-type=sqlite"})
	if err != nil || c.Effective.Database.Type != "sqlite" || c.Overrides["database.type"] != "flag:-output-type" {
		t.Fatal("last flag alias must win", err)
	}
}

func TestFlagSelectorConsumesValuesAndHonorsCommandArguments(t *testing.T) {
	clearSettingsEnvironment(t)
	args := []string{"-garmin-password", "-config", "-activity-id", "42", "-cookie-secure=false", "--log-level=debug"}
	c, err := ResolveArgs("base.yaml", Default(), args)
	if err != nil || c.Effective.Garmin.Password != "-config" || c.Effective.HTTP.CookieSecure || c.Effective.Logging.Level != "debug" {
		t.Fatal("consumed values confused flag selector", err)
	}
	overrides, path, err := settingOverrides(args)
	if err != nil || path != "" || len(overrides) != 3 {
		t.Fatal("password was interpreted as config path")
	}
	for _, args := range [][]string{{"--", "-log-level=debug"}, {"positional", "-log-level=debug"}} {
		c, err := ResolveArgs("config.yaml", Default(), args)
		if err != nil || c.Effective.Logging.Level != "info" {
			t.Fatal("parsed flags past positional/--", err)
		}
	}
	for _, args := range [][]string{{"-config"}, {"-config="}, {"-garmin-password"}} {
		if _, err := Load(args); err == nil {
			t.Fatalf("missing value accepted: %v", args)
		}
	}
}

func TestConfigurationSecretsRedactedAndNeverCopiedFromOverrides(t *testing.T) {
	clearSettingsEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := Default()
	secrets := map[string]string{}
	for _, s := range Settings() {
		if s.Secret {
			value := "synthetic-private-" + strings.ReplaceAll(s.Key, ".", "-")
			if s.Key == "database.uri" {
				value = "postgres://admin:synthetic-uri-secret@db.invalid/dives"
			}
			secrets[s.Key] = value
			if err := setSetting(&c, s, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	var help bytes.Buffer
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&help)
	RegisterFlags(fs, &c)
	fs.PrintDefaults()
	// Config.Database is a storage model; API uses DatabaseDisplay instead.
	safe := c
	safe.Database = Database{}
	data, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(help.String(), secret) || bytes.Contains(data, []byte(secret)) {
			t.Fatal("secret exposed in help/JSON")
		}
	}
	// Explicit YAML secrets are supported, with private file permissions.
	if err := Write(path, c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("config must be mode 0600", err)
	}
	loaded, err := Load([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Effective.Garmin.Password != c.Garmin.Password {
		t.Fatal("explicit YAML credential lost")
	}
	// Start with a clean file and overlay synthetic secrets through both channels.
	if err := Write(path, Default()); err != nil {
		t.Fatal(err)
	}
	args := []string{"-config", path}
	for i, s := range Settings() {
		if s.Secret {
			if i%2 == 0 {
				t.Setenv(s.Env[len(s.Env)-1], secrets[s.Key])
			} else {
				args = append(args, "-"+s.Flag, secrets[s.Key])
			}
		}
	}
	loaded, err = Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if err = Write(path, loaded.File); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("ENV/flag secret copied to YAML")
		}
	}
}

func TestTokenPathsAndBootstrapConfigSelection(t *testing.T) {
	clearSettingsEnvironment(t)
	path := filepath.Join(t.TempDir(), "settings.yaml")
	t.Setenv("DIVELOG_CONFIG", path)
	c, err := Load(nil)
	if err != nil || c.Path != path {
		t.Fatal("ENV config selector", err)
	}
	c.File.Garmin.TokenFile = "private/token.json"
	c, err = c.WithFile(c.File)
	if err != nil || c.TokenFile(false, "auto") != filepath.Join(filepath.Dir(path), "private/token.json") {
		t.Fatal("YAML-relative token path", err)
	}
	c, err = ResolveArgs(path, c.File, []string{"-token-file", "cwd-token.json"})
	if err != nil || c.TokenFile(false, "auto") != "cwd-token.json" {
		t.Fatal("explicit CLI token path", err)
	}
	c, err = Resolve(path, Default())
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenFile(false, "auto") != filepath.Join("auto", "go-garmindives", "tokens.json") || c.TokenFile(true, "auto") != filepath.Join("auto", "go-garmindives", "resync-personal-tokens.json") {
		t.Fatal("automatic caches")
	}
	c.Effective.Garmin.PersonalAPIKey = "synthetic"
	if c.TokenFile(false, "auto") != filepath.Join("auto", "go-garmindives", "personal-tokens.json") {
		t.Fatal("personal cache selection")
	}
}

func TestExampleAndSettingsReferenceCoverDefaults(t *testing.T) {
	clearSettingsEnvironment(t)
	example := filepath.Join("..", "..", "config.example.yaml")
	c, err := Load([]string{"-config", example})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.File, Default()) {
		t.Fatal("example must show the actual built-in defaults")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "specs", "settings.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	for _, s := range Settings() {
		if !strings.Contains(doc, "| `"+s.Key+"` |") || !strings.Contains(doc, "`-"+s.Flag+"`") {
			t.Fatalf("missing YAML/flag documentation for %s", s.Key)
		}
		for _, name := range append(append([]string{}, s.Env...), s.Aliases...) {
			if !strings.Contains(doc, "`"+name+"`") && !strings.Contains(doc, "`-"+name+"`") {
				t.Fatalf("missing ENV/alias documentation for %s", name)
			}
		}
	}
}

func TestEnvironmentUsesDivelogNamespace(t *testing.T) {
	clearSettingsEnvironment(t)
	for _, s := range Settings() {
		for _, name := range s.Env {
			if !strings.HasPrefix(name, "DIVELOG_") {
				t.Errorf("%s uses an unnamespaced variable: %s", s.Key, name)
			}
			old := strings.TrimPrefix(name, "DIVELOG_")
			if !strings.HasPrefix(s.Key, "garmin.") && !strings.HasPrefix(s.Key, "translation.") && s.Key != "logging.level" {
				old = "GARMIN_" + old
			}
			t.Setenv(old, "obsolete-variable-must-be-ignored")
		}
	}
	c, err := Resolve("config.yaml", Default())
	if err != nil || !reflect.DeepEqual(c.Effective, Default()) || len(c.Overrides) != 0 {
		t.Fatal("obsolete ENV names affected configuration", err)
	}
	t.Chdir(t.TempDir())
	oldConfig := filepath.Join(t.TempDir(), "obsolete.yaml")
	if err := os.WriteFile(oldConfig, []byte("unknown_setting: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARMIN_CONFIG", oldConfig)
	c, err = Load(nil)
	if err != nil || filepath.Base(c.Path) != "config.yaml" {
		t.Fatal("obsolete config selector is still active", err)
	}
	t.Setenv("DIVELOG_GEOCODING_ENABLED", "false")
	t.Setenv("DIVELOG_GARMIN_REQUEST_INTERVAL", "7s")
	t.Setenv("DIVELOG_DEEPL_API_KEY", "synthetic-namespaced-key:fx")
	t.Setenv("DIVELOG_LOG_LEVEL", "warn")
	c, err = Load(nil)
	if err != nil || c.Effective.Geocoding.Enabled || c.Effective.Garmin.RequestInterval != "7s" || c.Effective.Translation.APIKey != "synthetic-namespaced-key:fx" || c.Effective.Logging.Level != "warn" {
		t.Fatal("DIVELOG namespace not applied", err)
	}
}
