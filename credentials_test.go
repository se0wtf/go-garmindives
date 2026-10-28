package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	garmin "github.com/se0wtf/go-garminconnect"
)

func TestTerminalPrompterLine(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := writer.WriteString("value\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	var output bytes.Buffer
	prompt := newTerminalPrompter(reader, &output)
	value, err := prompt.Line(context.Background(), "Label: ")
	if err != nil || value != "value" || output.String() != "Label: " {
		t.Fatalf("value=%q output=%q err=%v", value, output.String(), err)
	}
	if _, err := prompt.Secret(context.Background(), "Secret: "); err == nil {
		t.Fatal("expected non-terminal secret error")
	}
}

type fakePrompter struct {
	lines       []string
	secret      string
	lineCalls   int
	secretCalls int
	err         error
}

func (p *fakePrompter) Line(context.Context, string) (string, error) {
	p.lineCalls++
	if p.err != nil {
		return "", p.err
	}
	value := ""
	if len(p.lines) > 0 {
		value, p.lines = p.lines[0], p.lines[1:]
	}
	return value, nil
}

func (p *fakePrompter) Secret(context.Context, string) (string, error) {
	p.secretCalls++
	if p.err != nil {
		return "", p.err
	}
	return p.secret, nil
}

func TestReadCredentialsFromEnvironment(t *testing.T) {
	t.Setenv("GARMIN_EMAIL", "runner@example.com")
	t.Setenv("GARMIN_PASSWORD", "secret")
	prompt := &fakePrompter{}
	got, err := readCredentials(context.Background(), prompt)
	if err != nil || got.Email != "runner@example.com" || got.Password != "secret" {
		t.Fatalf("credentials=%#v err=%v", got, err)
	}
	if prompt.lineCalls != 0 || prompt.secretCalls != 0 {
		t.Fatalf("unexpected prompt calls: line=%d secret=%d", prompt.lineCalls, prompt.secretCalls)
	}
}

func TestReadCredentialsPromptsForMissingValues(t *testing.T) {
	t.Setenv("GARMIN_EMAIL", "")
	t.Setenv("GARMIN_PASSWORD", "")
	prompt := &fakePrompter{lines: []string{"runner@example.com"}, secret: "secret"}
	got, err := readCredentials(context.Background(), prompt)
	if err != nil || got.Email != "runner@example.com" || got.Password != "secret" {
		t.Fatalf("credentials=%#v err=%v", got, err)
	}
	if prompt.lineCalls != 1 || prompt.secretCalls != 1 {
		t.Fatalf("prompt calls: line=%d secret=%d", prompt.lineCalls, prompt.secretCalls)
	}
}

func TestReadCredentialsErrors(t *testing.T) {
	t.Setenv("GARMIN_EMAIL", "")
	t.Setenv("GARMIN_PASSWORD", "")
	wantErr := errors.New("prompt failed")
	if _, err := readCredentials(context.Background(), &fakePrompter{err: wantErr}); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}
	if _, err := readCredentials(context.Background(), &fakePrompter{}); err == nil {
		t.Fatal("expected empty credentials error")
	}
}

func TestAwaitPromptCancellation(t *testing.T) {
	ctx, cancelContext := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var cancelCalls atomic.Int32
	result := make(chan error, 1)
	go func() {
		_, err := awaitPrompt(ctx, func() (string, error) {
			close(started)
			<-release
			return "late value", nil
		}, func() { cancelCalls.Add(1) })
		result <- err
	}()
	<-started
	cancelContext()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if cancelCalls.Load() != 1 {
		t.Fatalf("cancel calls = %d", cancelCalls.Load())
	}
	close(release)
}

func TestReadCredentialsCancellation(t *testing.T) {
	t.Setenv("GARMIN_EMAIL", "")
	t.Setenv("GARMIN_PASSWORD", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prompt := &contextPrompter{}
	_, err := readCredentials(ctx, prompt)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestLoginDoesNotUseLegacyCredentialsForPersonalCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := garmin.SaveTokens(path, garmin.Tokens{PersonalAPI: true, AccessToken: "expired-personal-token"}); err != nil {
		t.Fatal(err)
	}
	prompt := &fakePrompter{}
	_, err := login(context.Background(), path, prompt)
	if err == nil || !strings.Contains(err.Error(), "GARMIN_PERSONAL_API_KEY") || prompt.lineCalls != 0 || prompt.secretCalls != 0 {
		t.Fatalf("Personal API cache should require a key without prompting: %v", err)
	}
}

type contextPrompter struct{}

func (*contextPrompter) Line(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (*contextPrompter) Secret(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
