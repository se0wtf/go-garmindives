package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	charmLog "github.com/charmbracelet/log"
	appconfig "github.com/se0wtf/go-garmindives/internal/config"
)

func TestCommonConfigurationFlagsOnEveryCommand(t *testing.T) {
	clearApplicationEnvironment(t)
	args := []string{"-config", filepath.Join(t.TempDir(), "config.yaml")}
	args = append(args, "-garmin-request-interval=5s", "-deepl-request-interval=3s", "-geocoding-enabled=false", "-log-level=warn", "-cookie-secure=false", "-session-lifetime=48h", "-session-idle-timeout=3h", "-pending-scan-interval=90s", "-help")
	for _, cmd := range [][]string{{"sync"}, {"serve"}, {"admin", "reset-password"}} {
		var output bytes.Buffer
		if err := Run(context.Background(), append(cmd, args...), nil, &output, testLogger(t)); err != nil {
			t.Fatalf("%v: %v", cmd, err)
		}
		for _, s := range appconfig.Settings() {
			if !strings.Contains(output.String(), "-"+s.Flag) {
				t.Fatalf("%v missing flag %s", cmd, s.Flag)
			}
		}
	}
}

func TestServeUsesResolvedProviderSettingsAndRedactsSecrets(t *testing.T) {
	clearApplicationEnvironment(t)
	store, _ := openTestSQLStore(t)
	args := configuredServeArgs(t, store, nil)
	loaded, err := appconfig.Load(args)
	if err != nil {
		t.Fatal(err)
	}
	file := loaded.File
	file.Garmin.PersonalAPIKey = "synthetic-yaml-garmin-key"
	file.Garmin.ResyncTokenFile = "resync.json"
	file.Garmin.RequestInterval = "7s"
	file.Translation.APIKey = "synthetic-yaml-deepl:fx"
	file.Translation.URL = "https://yaml.invalid"
	file.Translation.TargetLang = "EN-US"
	file.Translation.GlossaryID = "yaml-glossary"
	file.Translation.Interval = "4s"
	file.Geocoding.Enabled = false
	file.Geocoding.URL = "https://geo.invalid"
	file.Geocoding.Interval = "20s"
	file.Geocoding.UserAgent = "YAML application"
	file.Logging.Level = "warn"
	if err = appconfig.Write(loaded.Path, file); err != nil {
		t.Fatal(err)
	}
	// All providers must use the merged snapshot, including a flag overriding bad ENV.
	t.Setenv("DIVELOG_GARMIN_PERSONAL_API_KEY", "synthetic-env-garmin-key")
	t.Setenv("DIVELOG_DEEPL_API_KEY", "synthetic-env-deepl:fx")
	t.Setenv("DIVELOG_DEEPL_API_URL", "invalid")
	t.Setenv("DIVELOG_GEOCODING_ENABLED", "true")
	args = append(args, "-deepl-api-url=https://flag.invalid", "-geocoding-enabled=false", "-log-level=error")
	logger := testLogger(t)
	err = runServeWithDependencies(context.Background(), args, io.Discard, logger, func(context.Context, string, string) (*sqlStore, error) { return store, nil }, func(_ context.Context, _ string, a *application, _ *charmLog.Logger) error {
		runtime := waitApplicationReady(t, a)
		if runtime.translations.config.APIKey != "synthetic-env-deepl:fx" || runtime.translations.config.Endpoint != "https://flag.invalid/v2/translate" || runtime.translations.config.Locale != "EN-US" || runtime.translations.config.Glossary != "yaml-glossary" || runtime.translations.config.Interval != 4*time.Second {
			t.Fatal("translation worker bypassed resolved settings")
		}
		if runtime.geocoder.client != nil || runtime.geocoder.config.Endpoint != "https://geo.invalid/reverse" || runtime.geocoder.config.Interval != 20*time.Second || runtime.geocoder.config.UserAgent != "YAML application" {
			t.Fatal("geocoder bypassed resolved settings")
		}
		if runtime.resync.factory == nil {
			t.Fatal("Garmin resync ignored configured key")
		}
		if logger.GetLevel() != charmLog.ErrorLevel {
			t.Fatal("logging flag was ignored")
		}
		cookie := loginApplication(t, a, testAdminPassword)
		w := appRequest(t, a, "GET", "/api/v1/config", nil, cookie)
		for _, secret := range []string{"synthetic-yaml-garmin-key", "synthetic-env-garmin-key", "synthetic-yaml-deepl:fx", "synthetic-env-deepl:fx"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("config API leaked secret")
			}
		}
		response := appJSON[configurationResponse](t, a, "GET", "/api/v1/config", nil, cookie, 200)
		if response.Logging.Level != "error" || response.Translation.URL != "https://flag.invalid" || response.Geocoding.Enabled {
			t.Fatal("config API omitted effective settings")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSyncUsesResolvedPersonalKeyAndTokenPath(t *testing.T) {
	clearApplicationEnvironment(t)
	file := appconfig.Default()
	file.Garmin.PersonalAPIKey = "synthetic-yaml-key"
	file.Garmin.TokenFile = "yaml-tokens.json"
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("DIVELOG_GARMIN_PERSONAL_API_KEY", "synthetic-env-key")
	args := []string{"-garmin-personal-api-key", "synthetic-flag-key", "-token-file", "flag-tokens.json"}
	c, err := appconfig.ResolveArgs(path, file, args)
	if err != nil {
		t.Fatal(err)
	}
	destination := &fakeDestination{archive: newSyncState(), existed: true}
	called := false
	err = runWithDependencies(context.Background(), args, io.Discard, testLogger(t), appDependencies{
		config: &c, configDirectory: func() (string, error) { return "/auto", nil },
		openDestination: func(context.Context, appconfig.Output) (diveDestination, error) { return destination, nil },
		newPersonal: func(key, cache string) (diveClient, error) {
			called = true
			if key != "synthetic-flag-key" || cache != "flag-tokens.json" {
				t.Fatal("sync ignored configured personal credentials/cache")
			}
			return &fakeDiveClient{}, nil
		},
	})
	if err != nil || !called {
		t.Fatal("personal sync configuration", err)
	}
	// No environment lookup or prompt can replace explicitly configured legacy credentials.
	prompt := &fakePrompter{err: errors.New("unexpected prompt")}
	creds, err := readConfiguredCredentials(context.Background(), prompt, "yaml@example.invalid", "  YAML password  ")
	if err != nil || creds.Password != "  YAML password  " || creds.Email != "yaml@example.invalid" || prompt.lineCalls != 0 || prompt.secretCalls != 0 {
		t.Fatal("configured credentials not honored", err)
	}
}
