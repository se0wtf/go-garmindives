package app

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	identity "github.com/se0wtf/go-garmindives/internal/identity"
	security "github.com/se0wtf/go-garmindives/internal/security"
)

const testAdminPassword = "Coral gardens shine at dawn!"

var testPasswordHash = sync.OnceValue(func() string {
	hash, err := security.HashPassword(testAdminPassword)
	if err != nil {
		panic(err)
	}
	return hash
})

func configuredServeArgs(t *testing.T, s *sqlStore, args []string) []string {
	t.Helper()
	c := appconfig.Default()
	c.Initialized = true
	c.InstanceID = identity.NewID()
	c.Database.URI = s.path
	c.Media.Directory = t.TempDir()
	c.HTTP.CookieSecure = false
	if _, err := s.db.Exec(`INSERT INTO admin_credentials(id,instance_id,password_hash,auth_version,initialized_at) VALUES(1,?,?,1,?) ON CONFLICT(id) DO UPDATE SET instance_id=excluded.instance_id,password_hash=excluded.password_hash`, c.InstanceID, testPasswordHash(), nowString()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := appconfig.Write(path, c); err != nil {
		t.Fatal(err)
	}
	return append([]string{"-config", path}, args...)
}
func waitApplicationReady(t *testing.T, a *application) *applicationRuntime {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	state := ""
	for time.Now().Before(deadline) {
		_, r, s := a.snapshot()
		state = s
		if r != nil {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("application did not become ready: %s", state)
	return nil
}
