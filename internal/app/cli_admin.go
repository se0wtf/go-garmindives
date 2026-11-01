package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	charmLog "github.com/charmbracelet/log"
	"golang.org/x/term"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	security "github.com/se0wtf/go-garmindives/internal/security"
)

func resetAdminPassword(ctx context.Context, args []string, input *os.File, output io.Writer, logger *charmLog.Logger) error {
	return resetAdminPasswordWithPrompt(ctx, args, func(ctx context.Context, label string) (string, error) {
		if !term.IsTerminal(int(input.Fd())) {
			return "", errors.New("administrator password recovery requires an interactive terminal")
		}
		if _, err := fmt.Fprint(output, label+": "); err != nil {
			return "", err
		}
		state, err := term.GetState(int(input.Fd()))
		if err != nil {
			return "", err
		}
		value, err := awaitPrompt(ctx, func() (string, error) { data, err := term.ReadPassword(int(input.Fd())); return string(data), err }, func() { _ = term.Restore(int(input.Fd()), state) })
		_, _ = fmt.Fprintln(output)
		return value, err
	}, output, logger)
}
func resetAdminPasswordWithPrompt(ctx context.Context, args []string, secret func(context.Context, string) (string, error), output io.Writer, logger *charmLog.Logger) error {
	c, err := appconfig.Load(args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("admin reset-password", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.String("config", c.Path, "application YAML configuration")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if !c.File.Initialized {
		return appconfig.ErrNotConfigured
	}
	if err := appconfig.ValidateDatabase(c.Effective.Database); err != nil {
		return err
	}
	db := c.DatabaseOutput()
	store, err := openSQLStore(ctx, db.Type, db.Endpoint)
	if err != nil {
		return errors.New("SQL database is unavailable")
	}
	defer store.Close()
	if err := store.checkInitialization(ctx, c.File.InstanceID); err != nil {
		return appconfig.ErrNotConfigured
	}
	password, err := secret(ctx, "New administrator password")
	if err != nil {
		return err
	}
	confirmation, err := secret(ctx, "Confirm administrator password")
	if err != nil {
		return err
	}
	if password != confirmation {
		return errors.New("passwords do not match")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	credentials, err := store.adminCredentials(ctx)
	if err != nil {
		return errors.New("cannot read administrator credentials")
	}
	if err := store.setAdminPassword(ctx, credentials.version, hash); err != nil {
		return err
	}
	logger.Info("administrator password reset; all sessions revoked")
	return nil
}
