package main

import (
	"bytes"
	"errors"
	"os"
	"testing"
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
	value, err := prompt.Line("Label: ")
	if err != nil || value != "value" || output.String() != "Label: " {
		t.Fatalf("value=%q output=%q err=%v", value, output.String(), err)
	}
	if _, err := prompt.Secret("Secret: "); err == nil {
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

func (p *fakePrompter) Line(string) (string, error) {
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

func (p *fakePrompter) Secret(string) (string, error) {
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
	got, err := readCredentials(prompt)
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
	got, err := readCredentials(prompt)
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
	if _, err := readCredentials(&fakePrompter{err: wantErr}); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}
	if _, err := readCredentials(&fakePrompter{}); err == nil {
		t.Fatal("expected empty credentials error")
	}
}
