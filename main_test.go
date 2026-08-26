package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	garmin "github.com/se0wtf/go-garminconnect"
)

func TestRunHelp(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var output bytes.Buffer
	if err := run(context.Background(), []string{"-help"}, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage of go-garmindives") {
		t.Fatalf("help output = %q", output.String())
	}
}

func TestRunWithDependencies(t *testing.T) {
	client := &fakeDiveClient{
		activities:         []garmin.Activity{{ID: 9, ActivityType: garmin.ActivityType{Key: "diving"}}},
		activityDetails:    map[int64]json.RawMessage{9: json.RawMessage(`{"activityId":9}`)},
		diveDetails:        map[int64]json.RawMessage{9: json.RawMessage(`{"connectId":9}`)},
		activityDetailsErr: map[int64]error{},
		diveDetailsErr:     map[int64]error{},
	}
	var savedPath, loadedTokenPath string
	var saved Archive
	dependencies := appDependencies{
		configDirectory: func() (string, error) { return "/config", nil },
		loadArchive:     func(string) (Archive, bool, error) { return newArchive(), false, nil },
		saveArchive: func(path string, archive Archive) error {
			savedPath, saved = path, archive
			return nil
		},
		loadCached: func(path string) (diveClient, error) {
			loadedTokenPath = path
			return client, nil
		},
		loginFresh: func(context.Context, string) (diveClient, error) { return nil, errors.New("unexpected login") },
	}
	var output bytes.Buffer
	err := runWithDependencies(context.Background(), []string{"-output", "export.yaml"}, &output, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if savedPath != "export.yaml" || loadedTokenPath != "/config/go-garmindives/tokens.json" {
		t.Fatalf("paths = %q / %q", savedPath, loadedTokenPath)
	}
	if len(saved.Dives) != 1 || !strings.Contains(output.String(), "1 new dive(s); 1 total") {
		t.Fatalf("saved=%#v output=%q", saved, output.String())
	}
}

func TestRunWithDependenciesDoesNotRewriteUnchangedArchive(t *testing.T) {
	saveCalls := 0
	dependencies := appDependencies{
		configDirectory: func() (string, error) { return "/config", nil },
		loadArchive:     func(string) (Archive, bool, error) { return newArchive(), true, nil },
		saveArchive:     func(string, Archive) error { saveCalls++; return nil },
		loadCached: func(string) (diveClient, error) {
			return &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}, nil
		},
		loginFresh: func(context.Context, string) (diveClient, error) { return nil, errors.New("unexpected login") },
	}
	if err := runWithDependencies(context.Background(), nil, &bytes.Buffer{}, dependencies); err != nil {
		t.Fatal(err)
	}
	if saveCalls != 0 {
		t.Fatalf("save calls = %d", saveCalls)
	}
}

func TestRunWithDependenciesErrors(t *testing.T) {
	wantErr := errors.New("failure")
	base := func() appDependencies {
		return appDependencies{
			configDirectory: func() (string, error) { return "/config", nil },
			loadArchive:     func(string) (Archive, bool, error) { return newArchive(), false, nil },
			saveArchive:     func(string, Archive) error { return nil },
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
		{"load archive", nil, func(d *appDependencies) {
			d.loadArchive = func(string) (Archive, bool, error) { return Archive{}, false, wantErr }
		}},
		{"authenticate", nil, func(d *appDependencies) { d.loadCached = func(string) (diveClient, error) { return nil, wantErr } }},
		{"save", nil, func(d *appDependencies) { d.saveArchive = func(string, Archive) error { return wantErr } }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies := base()
			test.edit(&dependencies)
			if err := runWithDependencies(context.Background(), test.args, &bytes.Buffer{}, dependencies); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	dependencies := base()
	var output bytes.Buffer
	if err := runWithDependencies(context.Background(), []string{"-help"}, &output, dependencies); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage of go-garmindives") {
		t.Fatalf("help output = %q", output.String())
	}
}

func TestSyncWithAuthenticationUsesCache(t *testing.T) {
	cached := &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}
	loginCalls := 0
	archive := newArchive()
	added, err := syncWithAuthentication(context.Background(), &archive,
		func() (diveClient, error) { return cached, nil },
		func() (diveClient, error) { loginCalls++; return nil, nil },
	)
	if err != nil || added != 0 || loginCalls != 0 {
		t.Fatalf("added=%d loginCalls=%d err=%v", added, loginCalls, err)
	}
}

func TestSyncWithAuthenticationLogsInWithoutCache(t *testing.T) {
	fresh := &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}
	loginCalls := 0
	archive := newArchive()
	_, err := syncWithAuthentication(context.Background(), &archive,
		func() (diveClient, error) { return nil, garmin.ErrNoTokenFile },
		func() (diveClient, error) { loginCalls++; return fresh, nil },
	)
	if err != nil || loginCalls != 1 {
		t.Fatalf("loginCalls=%d err=%v", loginCalls, err)
	}
}

func TestSyncWithAuthenticationReauthenticatesExpiredSession(t *testing.T) {
	cached := &fakeDiveClient{activitiesErr: garmin.ErrSessionExpired}
	fresh := &fakeDiveClient{activityDetailsErr: map[int64]error{}, diveDetailsErr: map[int64]error{}}
	loginCalls := 0
	archive := newArchive()
	_, err := syncWithAuthentication(context.Background(), &archive,
		func() (diveClient, error) { return cached, nil },
		func() (diveClient, error) { loginCalls++; return fresh, nil },
	)
	if err != nil || loginCalls != 1 {
		t.Fatalf("loginCalls=%d err=%v", loginCalls, err)
	}
}

func TestSyncWithAuthenticationDoesNotHideCacheErrors(t *testing.T) {
	wantErr := errors.New("corrupt token cache")
	loginCalls := 0
	archive := newArchive()
	_, err := syncWithAuthentication(context.Background(), &archive,
		func() (diveClient, error) { return nil, wantErr },
		func() (diveClient, error) { loginCalls++; return nil, nil },
	)
	if !errors.Is(err, wantErr) || loginCalls != 0 {
		t.Fatalf("loginCalls=%d err=%v", loginCalls, err)
	}
}
