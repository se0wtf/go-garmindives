package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	garmin "github.com/se0wtf/go-garminconnect"
	"golang.org/x/term"
)

type credentials struct {
	Email    string
	Password string
}

type prompter interface {
	Line(string) (string, error)
	Secret(string) (string, error)
}

type terminalPrompter struct {
	input  *os.File
	reader *bufio.Reader
	output io.Writer
}

func newTerminalPrompter(input *os.File, output io.Writer) *terminalPrompter {
	return &terminalPrompter{input: input, reader: bufio.NewReader(input), output: output}
}

func (p *terminalPrompter) Line(label string) (string, error) {
	if _, err := fmt.Fprint(p.output, label); err != nil {
		return "", err
	}
	value, err := p.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (p *terminalPrompter) Secret(label string) (string, error) {
	if !term.IsTerminal(int(p.input.Fd())) {
		return "", errors.New("password prompt requires a terminal; set GARMIN_PASSWORD")
	}
	if _, err := fmt.Fprint(p.output, label); err != nil {
		return "", err
	}
	value, err := term.ReadPassword(int(p.input.Fd()))
	_, _ = fmt.Fprintln(p.output)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(value)), nil
}

func readCredentials(prompt prompter) (credentials, error) {
	email := strings.TrimSpace(os.Getenv("GARMIN_EMAIL"))
	password := os.Getenv("GARMIN_PASSWORD")
	var err error
	if email == "" {
		email, err = prompt.Line("Garmin email: ")
		if err != nil {
			return credentials{}, fmt.Errorf("read Garmin email: %w", err)
		}
	}
	if password == "" {
		password, err = prompt.Secret("Garmin password: ")
		if err != nil {
			return credentials{}, fmt.Errorf("read Garmin password: %w", err)
		}
	}
	if email == "" || password == "" {
		return credentials{}, errors.New("Garmin email and password must not be empty")
	}
	return credentials{Email: email, Password: password}, nil
}

func login(ctx context.Context, tokenFile string, prompt prompter) (*garmin.Client, error) {
	credentials, err := readCredentials(prompt)
	if err != nil {
		return nil, err
	}
	mfa := func(context.Context) (string, error) {
		if code := strings.TrimSpace(os.Getenv("GARMIN_MFA_CODE")); code != "" {
			return code, nil
		}
		return prompt.Line("Garmin MFA code: ")
	}
	return garmin.Login(ctx, credentials.Email, credentials.Password, mfa, garmin.WithTokenFile(tokenFile))
}
