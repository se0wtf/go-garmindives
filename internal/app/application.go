package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	charmLog "github.com/charmbracelet/log"

	apidocs "github.com/se0wtf/go-garmindives/docs"
	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	countrydata "github.com/se0wtf/go-garmindives/internal/country"
	geolookup "github.com/se0wtf/go-garmindives/internal/geocoding"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
	identity "github.com/se0wtf/go-garmindives/internal/identity"
	security "github.com/se0wtf/go-garmindives/internal/security"
	deeplclient "github.com/se0wtf/go-garmindives/internal/translation"
)

type application struct {
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	wake        chan struct{}
	gate        chan struct{}
	cryptoSlots chan struct{}
	mu          sync.RWMutex
	config      appconfig.Loaded
	runtime     *applicationRuntime
	state       string
	logger      *charmLog.Logger
	openStore   openSQLStoreFunc
	writeConfig func(string, appconfig.Config) error
	limiter     loginLimiter
}
type applicationRuntime struct {
	store        *sqlStore
	media        *mediaLibrary
	translations *translationService
	geocoder     *countryService
	auth         *adminAuth
	handler      http.Handler
	pending      *pendingService
}

func (r *applicationRuntime) Close() {
	if r.pending != nil {
		r.pending.Close()
	}
	r.geocoder.Close()
	r.translations.Close()
	r.media.Close()
	r.store.Close()
}

func newApplication(ctx context.Context, c appconfig.Loaded, logger *charmLog.Logger, open openSQLStoreFunc) *application {
	ctx, cancel := context.WithCancel(ctx)
	a := &application{ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), gate: make(chan struct{}, 1), cryptoSlots: make(chan struct{}, 2), config: c, state: "not_configured", logger: logger, openStore: open, writeConfig: appconfig.Write}
	if c.File.Initialized {
		a.state = "starting"
	}
	go a.supervise()
	a.signal()
	return a
}
func (a *application) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}
func (a *application) Close() { a.cancel(); <-a.done }
func (a *application) snapshot() (appconfig.Loaded, *applicationRuntime, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config, a.runtime, a.state
}
func (a *application) supervise() {
	defer close(a.done)
	defer func() {
		_, r, _ := a.snapshot()
		if r != nil {
			r.Close()
		}
	}()
	for {
		c, r, _ := a.snapshot()
		if c.File.Initialized && r == nil {
			r, state := a.activate(c)
			a.mu.Lock()
			a.runtime = r
			a.state = state
			a.mu.Unlock()
			if r == nil && a.ctx.Err() == nil {
				if a.logger != nil {
					a.logger.Warn("configured service is unavailable; retrying", "code", state)
				}
				timer := time.NewTimer(5 * time.Second)
				select {
				case <-a.ctx.Done():
					timer.Stop()
					return
				case <-a.wake:
					timer.Stop()
				case <-timer.C:
				}
				continue
			}
		}
		timer := time.NewTimer(10 * time.Minute)
		select {
		case <-a.ctx.Done():
			timer.Stop()
			return
		case <-a.wake:
			timer.Stop()
		case <-timer.C:
			if _, r, _ := a.snapshot(); r != nil {
				_, _ = r.store.db.ExecContext(a.ctx, r.store.bind(`DELETE FROM admin_sessions WHERE expires_at<=?`), time.Now().UnixMilli())
			}
		}
	}
}
func (a *application) activate(c appconfig.Loaded) (*applicationRuntime, string) {
	if err := appconfig.ValidateDatabase(c.Effective.Database); err != nil {
		return nil, "invalid_configuration"
	}
	deeplConfig, err := deeplclient.FromEnvironment()
	if err != nil {
		return nil, "invalid_configuration"
	}
	geoConfig, err := geolookup.FromEnvironment()
	if err != nil {
		return nil, "invalid_configuration"
	}
	config := c.DatabaseOutput()
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	store, err := a.openStore(ctx, config.Type, config.Endpoint)
	cancel()
	if err != nil {
		return nil, "database_unavailable"
	}
	ok := false
	defer func() {
		if !ok {
			store.Close()
		}
	}()
	if err := store.checkInitialization(a.ctx, c.File.InstanceID); err != nil {
		return nil, "database_not_initialized"
	}
	media, err := openMediaLibrary(store, c.AssetsDirectory(), c.Effective.Media.MaxUploadBytes, a.logger)
	if err != nil {
		return nil, "media_unavailable"
	}
	defer func() {
		if !ok {
			media.Close()
		}
	}()
	store.media = media
	interval, _ := time.ParseDuration(c.Effective.Media.PendingScanInterval)
	pending, err := newPendingService(media, c.PendingDirectory(), interval)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("pending photo directory unavailable", "directory", c.PendingDirectory(), "err", err)
		}
		return nil, "media_unavailable"
	}
	defer func() {
		if !ok {
			pending.Close()
		}
	}()
	store.pending = pending
	// Finish recorded assignments before the ordinary media scan can reconcile
	// files published just before an earlier process stopped.
	if _, err := pending.Recover(a.ctx); err != nil {
		return nil, "media_unavailable"
	}
	if err := store.reconcileTravel(a.ctx); err != nil {
		return nil, "database_unavailable"
	}
	if _, err := media.Scan(a.ctx, false); err != nil {
		return nil, "media_unavailable"
	}
	var translator deeplclient.Translator
	if deeplConfig.APIKey != "" {
		translator = deeplclient.NewClient(deeplConfig)
	}
	translations := newTranslationService(store, deeplConfig, translator, a.logger)
	store.translations = translations
	var lookup geolookup.Lookup
	if geoConfig.Enabled {
		lookup = geolookup.NewClient(geoConfig)
	}
	geocoder := newCountryService(store, geoConfig, lookup, a.logger)
	store.geocoder = geocoder
	auth := newAdminAuth(a, store, c.Effective)
	r := &applicationRuntime{store: store, media: media, translations: translations, geocoder: geocoder, auth: auth, pending: pending}
	r.handler = auth.Handler(newAPIHandler(store, a.logger))
	translations.Start(a.ctx)
	geocoder.Start(a.ctx)
	pending.Start(a.ctx)
	ok = true
	if a.logger != nil {
		a.logger.Info("configured service ready", "output_type", config.Type, "output", config.LogDestination())
	}
	return r, "ready"
}
func (a *application) Handler() http.Handler {
	mux := http.NewServeMux()
	apidocs.RegisterRoutes(mux)
	mux.HandleFunc("GET /api/v1/setup/status", a.setupStatus)
	mux.HandleFunc("GET /api/v1/setup/countries", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, 200, map[string]any{"items": countrydata.Catalog})
	})
	mux.HandleFunc("POST /api/v1/setup", a.setup)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready, state := a.readiness(r.Context())
		if !ready {
			httpx.WriteAPIError(w, 503, state, "service is not ready")
			return
		}
		httpx.WriteJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, runtime, state := a.snapshot()
		if !c.File.Initialized {
			httpx.WriteAPIError(w, 503, "not_configured", "complete initial setup first")
			return
		}
		if runtime == nil {
			httpx.WriteAPIError(w, 503, state, "configured service is not ready; setup remains closed")
			return
		}
		runtime.handler.ServeHTTP(w, r)
	})
	protection := http.NewCrossOriginProtection()
	c, _, _ := a.snapshot()
	if c.Effective.HTTP.PublicURL != "" {
		_ = protection.AddTrustedOrigin(strings.TrimRight(c.Effective.HTTP.PublicURL, "/"))
	}
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteAPIError(w, 403, "csrf_rejected", "cross-origin writes are not allowed")
	}))
	return apiMiddleware(protection.Handler(mux))
}

type setupStatusResponse struct {
	Configured            bool              `json:"configured"`
	Ready                 bool              `json:"ready"`
	State                 string            `json:"state"`
	DatabaseType          string            `json:"database_type"`
	DatabaseURIConfigured bool              `json:"database_uri_configured"`
	Overrides             map[string]string `json:"overrides"`
	PasswordMinLength     int               `json:"password_min_length"`
	PasswordMaxLength     int               `json:"password_max_length"`
}

func (a *application) setupStatus(w http.ResponseWriter, r *http.Request) {
	c, _, _ := a.snapshot()
	ready, state := a.readiness(r.Context())
	httpx.WriteJSON(w, 200, setupStatusResponse{Configured: c.File.Initialized, Ready: ready, State: state, DatabaseType: c.Effective.Database.Type, DatabaseURIConfigured: strings.TrimSpace(c.Effective.Database.URI) != "", Overrides: c.Overrides, PasswordMinLength: security.MinPasswordLength, PasswordMaxLength: security.MaxPasswordLength})
}

func (a *application) readiness(ctx context.Context) (bool, string) {
	_, runtime, state := a.snapshot()
	if runtime == nil {
		return false, state
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := runtime.store.db.PingContext(ctx); err != nil {
		return false, "database_unavailable"
	}
	return true, "ready"
}

type databasePatch struct {
	Type *string `json:"type"`
	URI  *string `json:"uri"`
}
type setupHomeCountry struct {
	CountryCode string  `json:"country_code"`
	From        string  `json:"from"`
	To          *string `json:"to"`
}
type setupRequest struct {
	Database      databasePatch      `json:"database"`
	AdminPassword string             `json:"admin_password"`
	HomeCountries []setupHomeCountry `json:"home_countries"`
}

func (a *application) withDatabasePatch(c appconfig.Loaded, p databasePatch) (appconfig.Loaded, error) {
	file := c.File
	if p.Type != nil {
		value := strings.ToLower(strings.TrimSpace(*p.Type))
		if source := c.Overrides["database.type"]; source != "" && value != c.Effective.Database.Type {
			return c, errors.New("database.type is managed by ENV or a command flag")
		}
		file.Database.Type = value
	}
	if p.URI != nil {
		value := strings.TrimSpace(*p.URI)
		if source := c.Overrides["database.uri"]; source != "" && value != c.Effective.Database.URI {
			return c, errors.New("database.uri is managed by ENV or a command flag")
		}
		file.Database.URI = value
	}
	resolved, err := appconfig.Resolve(c.Path, file)
	if err != nil {
		return c, err
	}
	for key, source := range c.Overrides {
		if !strings.HasPrefix(source, "flag:") {
			continue
		}
		resolved.Overrides[key] = source
		switch key {
		case "database.type":
			resolved.Effective.Database.Type = c.Effective.Database.Type
		case "database.uri":
			resolved.Effective.Database.URI = c.Effective.Database.URI
		case "http.listen":
			resolved.Effective.HTTP.Listen = c.Effective.HTTP.Listen
		case "media.directory":
			resolved.Effective.Media.Directory = c.Effective.Media.Directory
		case "media.max_upload_bytes":
			resolved.Effective.Media.MaxUploadBytes = c.Effective.Media.MaxUploadBytes
		}
	}
	if err := appconfig.ValidateDatabase(resolved.Effective.Database); err != nil {
		return c, err
	}
	return resolved, nil
}
func (a *application) setup(w http.ResponseWriter, r *http.Request) {
	c, _, _ := a.snapshot()
	if c.File.Initialized {
		httpx.WriteAPIError(w, 409, "already_configured", "initial setup has already completed")
		return
	}
	select {
	case a.gate <- struct{}{}:
		defer func() { <-a.gate }()
	default:
		httpx.WriteAPIError(w, 409, "setup_in_progress", "another setup request is in progress")
		return
	}
	c, _, _ = a.snapshot()
	if c.File.Initialized {
		httpx.WriteAPIError(w, 409, "already_configured", "initial setup has already completed")
		return
	}
	if !a.allowPasswordAttempt(w, r) {
		return
	}
	var request setupRequest
	if !decodeAuthRequest(w, r, &request) {
		return
	}
	if len(request.AdminPassword) > 512 {
		httpx.WriteAPIError(w, 400, "invalid_password", "password exceeds 128 characters")
		return
	}
	next, err := a.withDatabasePatch(c, request.Database)
	if err != nil {
		httpx.WriteAPIError(w, 400, "invalid_configuration", err.Error())
		return
	}
	if len(request.HomeCountries) > 100 {
		httpx.WriteAPIError(w, 400, "invalid_request", "at most 100 home-country periods are allowed")
		return
	}
	for i := range request.HomeCountries {
		home := &request.HomeCountries[i]
		country, err := countrydata.Resolve(home.CountryCode)
		to := ""
		if home.To != nil {
			to = *home.To
		}
		if err != nil || home.From == "" || (home.To != nil && to == "") {
			httpx.WriteAPIError(w, 400, "invalid_request", "provide a valid home country and date range")
			return
		}
		if err := validateDateRange(home.From, to); err != nil {
			httpx.WriteAPIError(w, 400, "invalid_request", err.Error())
			return
		}
		home.CountryCode = country.Code
	}
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	output := next.DatabaseOutput()
	store, err := a.openStore(ctx, output.Type, output.Endpoint)
	if err != nil {
		httpx.WriteAPIError(w, 400, "database_unavailable", "cannot connect to the selected SQL database")
		return
	}
	defer store.Close()
	credentials, readErr := store.adminCredentials(ctx)
	existing := readErr == nil
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		httpx.WriteAPIError(w, 503, "database_unavailable", "cannot read initialization state")
		return
	}
	if !a.acquireCrypto(w, r) {
		return
	}
	var hash string
	if existing {
		if !security.VerifyPassword(credentials.passwordHash, request.AdminPassword) {
			a.releaseCrypto()
			httpx.WriteAPIError(w, 401, "invalid_credentials", "existing database requires its administrator password")
			return
		}
	} else {
		hash, err = security.HashPassword(request.AdminPassword)
		if err != nil {
			a.releaseCrypto()
			httpx.WriteAPIError(w, 400, "invalid_password", err.Error())
			return
		}
	}
	a.releaseCrypto()
	tx, err := store.beginTravelTx(ctx)
	if err != nil {
		httpx.WriteAPIError(w, 503, "database_unavailable", "cannot initialize database")
		return
	}
	defer tx.Rollback()
	if existing {
		var current adminCredentials
		err = tx.QueryRowContext(ctx, `SELECT instance_id,password_hash,auth_version FROM admin_credentials WHERE id=1`).Scan(&current.instanceID, &current.passwordHash, &current.version)
		if err != nil || current.version != credentials.version || current.passwordHash != credentials.passwordHash {
			httpx.WriteAPIError(w, 409, "setup_conflict", "administrator state changed; retry setup")
			return
		}
		next.File.InstanceID = current.instanceID
	} else {
		next.File.InstanceID = identity.NewID()
		result, err := tx.ExecContext(ctx, store.bind(`INSERT INTO admin_credentials(id,instance_id,password_hash,auth_version,initialized_at) VALUES(1,?,?,1,?) ON CONFLICT(id) DO NOTHING`), next.File.InstanceID, hash, nowString())
		if err != nil {
			httpx.WriteAPIError(w, 503, "database_unavailable", "cannot initialize administrator")
			return
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			httpx.WriteAPIError(w, 409, "setup_conflict", "database was initialized by another request")
			return
		}
		if err := insertInitialHomes(ctx, store, tx, request.HomeCountries); err != nil {
			httpx.WriteAPIError(w, 400, "invalid_home_countries", err.Error())
			return
		}
	}
	next.File.Initialized = true
	next.Effective.Initialized = true
	next.Effective.InstanceID = next.File.InstanceID
	if err := a.writeConfig(next.Path, next.File); err != nil {
		httpx.WriteAPIError(w, 500, "configuration_write_failed", "cannot save configuration; database initialization was rolled back")
		return
	}
	if err := tx.Commit(); err != nil {
		restoreErr := a.writeConfig(c.Path, c.File)
		if restoreErr != nil {
			a.mu.Lock()
			a.config = next
			a.state = "initialization_failed"
			a.mu.Unlock()
		}
		httpx.WriteAPIError(w, 503, "initialization_failed", "database commit failed; check server configuration before retrying")
		return
	}
	a.mu.Lock()
	a.config = next
	a.state = "starting"
	a.mu.Unlock()
	a.signal()
	if a.logger != nil {
		a.logger.Info("initial configuration completed", "output_type", output.Type, "output", output.LogDestination())
	}
	httpx.WriteJSON(w, 201, map[string]any{"configured": true, "ready": false, "state": "starting"})
}
func insertInitialHomes(ctx context.Context, s *sqlStore, tx *sql.Tx, homes []setupHomeCountry) error {
	for _, home := range homes {
		var matches int
		err := tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM home_countries WHERE country_code=? AND from_date=? AND COALESCE(to_date,'')=COALESCE(CAST(? AS TEXT),'')`), home.CountryCode, home.From, home.To).Scan(&matches)
		if err != nil {
			return errors.New("cannot read residence history")
		}
		if matches > 0 {
			continue
		}
		var overlap int
		err = tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM home_countries WHERE (CAST(? AS TEXT) IS NULL OR from_date<=?) AND (to_date IS NULL OR to_date>=?)`), home.To, home.To, home.From).Scan(&overlap)
		if err != nil {
			return errors.New("cannot read residence history")
		}
		if overlap > 0 {
			return errResidenceOverlap
		}
		if _, err := tx.ExecContext(ctx, s.bind(`INSERT INTO home_countries(id,country_code,from_date,to_date) VALUES(?,?,?,?)`), identity.NewID(), home.CountryCode, home.From, home.To); err != nil {
			return errors.New("cannot save residence history")
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM home_countries`).Scan(&count); err != nil {
		return errors.New("cannot read residence history")
	}
	if count == 0 {
		return errors.New("provide at least one home-country period")
	}
	if err := reconcileTripsTx(ctx, s, tx); err != nil {
		return errors.New("cannot initialize dive trips")
	}
	return nil
}
