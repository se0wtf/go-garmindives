package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	charmLog "github.com/charmbracelet/log"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"

	garmin "github.com/se0wtf/go-garminconnect"
)

func testLogger(t *testing.T) *charmLog.Logger {
	t.Helper()
	logger, err := NewAppLogger(io.Discard, "debug")
	if err != nil {
		t.Fatal(err)
	}
	return logger
}

type fakeDestination struct {
	archive syncState
	existed bool
	loadErr error
	saveErr error
	saved   syncState
	added   int
	closed  bool
}

func (d *fakeDestination) Load(context.Context) (syncState, bool, error) {
	return d.archive, d.existed, d.loadErr
}

func (d *fakeDestination) Save(_ context.Context, archive syncState, added int) error {
	d.saved, d.added = archive, added
	return d.saveErr
}

func (d *fakeDestination) Close() error {
	d.closed = true
	return nil
}

func TestAppLoggerLevels(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewAppLogger(&output, "debug")
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("details", "dives", 2)
	logger.Info("ready")
	logger.Error("failed", "err", errors.New("test failure"))
	for _, expected := range []string{"DEBU", "details", "INFO", "ready", "ERRO", "failed"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("log output %q does not contain %q", output.String(), expected)
		}
	}
	if _, err := NewAppLogger(io.Discard, "invalid"); err == nil {
		t.Fatal("expected invalid level error")
	}
}

func TestRunHelp(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	for _, argument := range []string{"-h", "--help"} {
		var output bytes.Buffer
		if err := Run(context.Background(), []string{argument}, input, &output, testLogger(t)); err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{
			"downloads every Garmin Connect dive",
			"GARMIN_PERSONAL_API_KEY", "GARMIN_REQUEST_INTERVAL",
			"GARMIN_EMAIL", "GARMIN_PASSWORD", "GARMIN_MFA_CODE", "GARMIN_OUTPUT_TYPE", "GARMIN_OUTPUT_DSN", "GARMIN_API_ADDR", "LOG_LEVEL",
			"-output", "-output-type", "-output-dsn", "-token-file",
		} {
			if !strings.Contains(output.String(), expected) {
				t.Fatalf("help output for %s does not contain %q: %q", argument, expected, output.String())
			}
		}
	}
}

func TestRunUsesSQLConfigurationFromEnvironment(t *testing.T) {
	t.Setenv("GARMIN_OUTPUT_TYPE", appconfig.Postgres)
	const dsn = "postgres://secret:password@example.invalid:5432/dives?application_name=private-option"
	t.Setenv("GARMIN_OUTPUT_DSN", dsn)
	var config appconfig.Output
	destination := &fakeDestination{archive: newSyncState(), existed: true}
	dependencies := appDependencies{
		configDirectory: func() (string, error) { return "/config", nil },
		openDestination: func(_ context.Context, got appconfig.Output) (diveDestination, error) {
			config = got
			return destination, nil
		},
		loadCached: func(string) (diveClient, error) {
			return &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}, nil
		},
		loginFresh: func(context.Context, string) (diveClient, error) { return nil, errors.New("unexpected login") },
	}
	var logs bytes.Buffer
	logger, err := NewAppLogger(&logs, "debug")
	if err != nil {
		t.Fatal(err)
	}
	if err := runWithDependencies(context.Background(), nil, &bytes.Buffer{}, logger, dependencies); err != nil {
		t.Fatal(err)
	}
	if config != (appconfig.Output{Type: appconfig.Postgres, Endpoint: dsn}) {
		t.Fatalf("output configuration = %#v", config)
	}
	if !strings.Contains(logs.String(), "synchronizing Garmin dives output_type=postgres output=example.invalid:5432/dives") {
		t.Fatalf("sync log does not show the PostgreSQL destination: %q", logs.String())
	}
	if strings.Contains(logs.String(), dsn) || strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "password") || strings.Contains(logs.String(), "private-option") {
		t.Fatalf("database credentials leaked to logs: %q", logs.String())
	}
}

func TestRunWithSQLiteOutputIsIncremental(t *testing.T) {
	t.Setenv("GARMIN_OUTPUT_TYPE", "")
	fixture := testSQLDive(t, 42)
	activityDetails, err := json.Marshal(fixture.ActivityDetails)
	if err != nil {
		t.Fatal(err)
	}
	diveDetails, err := json.Marshal(fixture.DiveDetails)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeDiveClient{
		activities:         []garmin.Activity{{ID: 42, Name: "Lagoon", ActivityType: garmin.ActivityType{Key: "single_gas_diving"}}},
		activityDetails:    map[int64]json.RawMessage{42: activityDetails},
		diveDetails:        map[int64]json.RawMessage{42: diveDetails},
		activityDetailsErr: map[int64]error{},
		diveDetailsErr:     map[int64]error{},
	}
	dependencies := appDependencies{
		configDirectory: func() (string, error) { return "/config", nil },
		openDestination: openDestination,
		loadCached:      func(string) (diveClient, error) { return client, nil },
		loginFresh:      func(context.Context, string) (diveClient, error) { return nil, errors.New("unexpected login") },
	}
	path := filepath.Join(t.TempDir(), "dives.db")
	args := []string{"-output-dsn", path}
	for run := 0; run < 2; run++ {
		if err := runWithDependencies(context.Background(), args, &bytes.Buffer{}, testLogger(t), dependencies); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(client.activityDetailCalls, []int64{42}) || !reflect.DeepEqual(client.diveDetailCalls, []int64{42}) {
		t.Fatalf("incremental detail calls = %v / %v", client.activityDetailCalls, client.diveDetailCalls)
	}
	store, err := openSQLStore(context.Background(), appconfig.SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var dives, samples int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dives`).Scan(&dives); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dive_samples`).Scan(&samples); err != nil {
		t.Fatal(err)
	}
	if dives != 1 || samples != 2 {
		t.Fatalf("stored dives=%d samples=%d", dives, samples)
	}
}

func TestRunDefaultsToSQLite(t *testing.T) {
	t.Setenv("GARMIN_OUTPUT_TYPE", "")
	t.Setenv("GARMIN_OUTPUT_DSN", "")
	client := &fakeDiveClient{
		activities:         []garmin.Activity{{ID: 9, ActivityType: garmin.ActivityType{Key: "single_gas_diving"}}},
		activityDetails:    map[int64]json.RawMessage{9: json.RawMessage(`{"activityId":9}`)},
		diveDetails:        map[int64]json.RawMessage{9: json.RawMessage(`{"connectId":9}`)},
		activityDetailsErr: map[int64]error{},
		diveDetailsErr:     map[int64]error{},
	}
	var config appconfig.Output
	var loadedTokenPath string
	destination := &fakeDestination{archive: newSyncState()}
	dependencies := appDependencies{
		configDirectory: func() (string, error) { return "/config", nil },
		openDestination: func(_ context.Context, got appconfig.Output) (diveDestination, error) {
			config = got
			return destination, nil
		},
		loadCached: func(path string) (diveClient, error) {
			loadedTokenPath = path
			return client, nil
		},
		loginFresh: func(context.Context, string) (diveClient, error) { return nil, errors.New("unexpected login") },
	}
	var output bytes.Buffer
	var logOutput bytes.Buffer
	logger, err := NewAppLogger(&logOutput, "info")
	if err != nil {
		t.Fatal(err)
	}
	err = runWithDependencies(context.Background(), nil, &output, logger, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if config != (appconfig.Output{Type: appconfig.SQLite, Endpoint: "dives.db"}) || loadedTokenPath != "/config/go-garmindives/tokens.json" {
		t.Fatalf("configuration = %#v / %q", config, loadedTokenPath)
	}
	if len(destination.saved.Dives) != 1 || destination.added != 1 || !destination.closed {
		t.Fatalf("destination=%#v output=%q", destination, output.String())
	}
	if !strings.Contains(logOutput.String(), "synchronisation in progress... (1/1)") {
		t.Fatalf("progress log = %q", logOutput.String())
	}
	if !strings.Contains(logOutput.String(), "synchronizing Garmin dives output_type=sqlite output=dives.db") {
		t.Fatalf("sync log does not show the default SQLite destination: %q", logOutput.String())
	}
}

func TestRunWithDependenciesDoesNotRewriteUnchangedArchive(t *testing.T) {
	destination := &fakeDestination{archive: newSyncState(), existed: true}
	dependencies := appDependencies{
		configDirectory: func() (string, error) { return "/config", nil },
		openDestination: func(context.Context, appconfig.Output) (diveDestination, error) { return destination, nil },
		loadCached: func(string) (diveClient, error) {
			return &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}, nil
		},
		loginFresh: func(context.Context, string) (diveClient, error) { return nil, errors.New("unexpected login") },
	}
	if err := runWithDependencies(context.Background(), nil, &bytes.Buffer{}, testLogger(t), dependencies); err != nil {
		t.Fatal(err)
	}
	if destination.saved.Dives != nil {
		t.Fatalf("unexpected saved archive = %#v", destination.saved)
	}
}

func TestRunWithDependenciesErrors(t *testing.T) {
	wantErr := errors.New("failure")
	base := func() appDependencies {
		destination := &fakeDestination{archive: newSyncState()}
		return appDependencies{
			configDirectory: func() (string, error) { return "/config", nil },
			openDestination: func(context.Context, appconfig.Output) (diveDestination, error) { return destination, nil },
			loadCached: func(string) (diveClient, error) {
				return &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}, nil
			},
			loginFresh: func(context.Context, string) (diveClient, error) { return nil, wantErr },
		}
	}
	tests := []struct {
		name string
		args []string
		edit func(*appDependencies)
	}{
		{"config", nil, func(d *appDependencies) { d.configDirectory = func() (string, error) { return "", wantErr } }},
		{"flags", []string{"-unknown"}, func(*appDependencies) {}},
		{"open output", nil, func(d *appDependencies) {
			d.openDestination = func(context.Context, appconfig.Output) (diveDestination, error) { return nil, wantErr }
		}},
		{"load output", nil, func(d *appDependencies) {
			d.openDestination = func(context.Context, appconfig.Output) (diveDestination, error) {
				return &fakeDestination{loadErr: wantErr}, nil
			}
		}},
		{"authenticate", nil, func(d *appDependencies) { d.loadCached = func(string) (diveClient, error) { return nil, wantErr } }},
		{"save", nil, func(d *appDependencies) {
			d.openDestination = func(context.Context, appconfig.Output) (diveDestination, error) {
				return &fakeDestination{archive: newSyncState(), saveErr: wantErr}, nil
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies := base()
			test.edit(&dependencies)
			if err := runWithDependencies(context.Background(), test.args, &bytes.Buffer{}, testLogger(t), dependencies); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	dependencies := base()
	var output bytes.Buffer
	if err := runWithDependencies(context.Background(), []string{"-help"}, &output, testLogger(t), dependencies); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage:") {
		t.Fatalf("help output = %q", output.String())
	}
}

func TestSyncWithAuthenticationUsesCache(t *testing.T) {
	cached := &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}
	loginCalls := 0
	archive := newSyncState()
	added, err := syncWithAuthentication(context.Background(), &archive,
		func() (diveClient, error) { return cached, nil },
		func() (diveClient, error) { loginCalls++; return nil, nil },
		testLogger(t), 0,
	)
	if err != nil || added != 0 || loginCalls != 0 {
		t.Fatalf("added=%d loginCalls=%d err=%v", added, loginCalls, err)
	}
}

func TestSyncWithAuthenticationLogsInWithoutCache(t *testing.T) {
	fresh := &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}
	loginCalls := 0
	archive := newSyncState()
	_, err := syncWithAuthentication(context.Background(), &archive,
		func() (diveClient, error) { return nil, garmin.ErrNoTokenFile },
		func() (diveClient, error) { loginCalls++; return fresh, nil },
		testLogger(t), 0,
	)
	if err != nil || loginCalls != 1 {
		t.Fatalf("loginCalls=%d err=%v", loginCalls, err)
	}
}

func TestSyncWithAuthenticationReauthenticatesExpiredSession(t *testing.T) {
	cached := &fakeDiveClient{activitiesErr: garmin.ErrSessionExpired}
	fresh := &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}
	loginCalls := 0
	archive := newSyncState()
	_, err := syncWithAuthentication(context.Background(), &archive,
		func() (diveClient, error) { return cached, nil },
		func() (diveClient, error) { loginCalls++; return fresh, nil },
		testLogger(t), 0,
	)
	if err != nil || loginCalls != 1 {
		t.Fatalf("loginCalls=%d err=%v", loginCalls, err)
	}
}

func TestSyncWithAuthenticationDoesNotHideCacheErrors(t *testing.T) {
	wantErr := errors.New("corrupt token cache")
	loginCalls := 0
	archive := newSyncState()
	_, err := syncWithAuthentication(context.Background(), &archive,
		func() (diveClient, error) { return nil, wantErr },
		func() (diveClient, error) { loginCalls++; return nil, nil },
		testLogger(t), 0,
	)
	if !errors.Is(err, wantErr) || loginCalls != 0 {
		t.Fatalf("loginCalls=%d err=%v", loginCalls, err)
	}
}
