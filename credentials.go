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
	Line(context.Context, string) (string, error)
	Secret(context.Context, string) (string, error)
}

type terminalPrompter struct {
	input  *os.File
	reader *bufio.Reader
	output io.Writer
}

func newTerminalPrompter(input *os.File, output io.Writer) *terminalPrompter {
	return &terminalPrompter{input: input, reader: bufio.NewReader(input), output: output}
}

func (p *terminalPrompter) Line(ctx context.Context, label string) (string, error) {
	if _, err := fmt.Fprint(p.output, label); err != nil {
		return "", err
	}
	value, err := awaitPrompt(ctx, func() (string, error) {
		return p.reader.ReadString('\n')
	}, nil)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (p *terminalPrompter) Secret(ctx context.Context, label string) (string, error) {
	if !term.IsTerminal(int(p.input.Fd())) {
		return "", errors.New("password prompt requires a terminal; set GARMIN_PASSWORD")
	}
	if _, err := fmt.Fprint(p.output, label); err != nil {
		return "", err
	}
	state, err := term.GetState(int(p.input.Fd()))
	if err != nil {
		return "", err
	}
	value, err := awaitPrompt(ctx, func() (string, error) {
		password, readErr := term.ReadPassword(int(p.input.Fd()))
		return string(password), readErr
	}, func() {
		_ = term.Restore(int(p.input.Fd()), state)
	})
	_, _ = fmt.Fprintln(p.output)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

type promptResult struct {
	value string
	err   error
}

func awaitPrompt(ctx context.Context, read func() (string, error), cancel func()) (string, error) {
	result := make(chan promptResult, 1)
	go func() {
		value, err := read()
		result <- promptResult{value: value, err: err}
	}()
	select {
	case result := <-result:
		return result.value, result.err
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		return "", ctx.Err()
	}
}

func readCredentials(ctx context.Context, prompt prompter) (credentials, error) {
	email := strings.TrimSpace(os.Getenv("GARMIN_EMAIL"))
	password := os.Getenv("GARMIN_PASSWORD")
	var err error
	if email == "" {
		email, err = prompt.Line(ctx, "Garmin email: ")
		if err != nil {
			return credentials{}, fmt.Errorf("read Garmin email: %w", err)
		}
	}
	if password == "" {
		password, err = prompt.Secret(ctx, "Garmin password: ")
		if err != nil {
			return credentials{}, fmt.Errorf("read Garmin password: %w", err)
		}
	}
	if email == "" || password == "" {
		return credentials{}, errors.New("Garmin email and password must not be empty")
	}
	return credentials{Email: email, Password: password}, nil
}

func login(ctx context.Context, tokenFile string, prompt prompter, options ...garmin.Option) (*garmin.Client, error) {
	if tokens, err := garmin.LoadTokens(tokenFile); err == nil && tokens.PersonalAPI {
		return nil, errors.New("set GARMIN_PERSONAL_API_KEY to renew the cached Personal API session")
	}
	credentials, err := readCredentials(ctx, prompt)
	if err != nil {
		return nil, err
	}
	mfa := func(providerCtx context.Context) (string, error) {
		if code := strings.TrimSpace(os.Getenv("GARMIN_MFA_CODE")); code != "" {
			return code, nil
		}
		return prompt.Line(providerCtx, "Garmin MFA code: ")
	}
	options = append(options, garmin.WithTokenFile(tokenFile))
	return garmin.Login(ctx, credentials.Email, credentials.Password, mfa, options...)
}
