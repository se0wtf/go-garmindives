package app

import (
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/scs/v2"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
	security "github.com/se0wtf/go-garmindives/internal/security"
)

type loginBucket struct {
	count int
	start time.Time
}
type loginLimiter struct {
	mu      sync.Mutex
	clients map[string]loginBucket
	global  loginBucket
}

func (l *loginLimiter) allow(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clients == nil {
		l.clients = map[string]loginBucket{}
	}
	if now.Sub(l.global.start) >= time.Minute {
		l.global = loginBucket{start: now}
		for key, b := range l.clients {
			if now.Sub(b.start) >= time.Minute {
				delete(l.clients, key)
			}
		}
	}
	b := l.clients[client]
	if now.Sub(b.start) >= time.Minute {
		b = loginBucket{start: now}
	}
	if l.global.count >= 20 || b.count >= 10 || len(l.clients) >= 4096 {
		return false
	}
	l.global.count++
	b.count++
	l.clients[client] = b
	return true
}
func (a *application) allowPasswordAttempt(w http.ResponseWriter, r *http.Request) bool {
	// RemoteAddr is authoritative. Forwarded-IP headers are deliberately ignored;
	// an untrusted caller must not be able to rotate its rate-limit identity.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !a.limiter.allow(ip, time.Now()) {
		w.Header().Set("Retry-After", "60")
		httpx.WriteAPIError(w, 429, "too_many_attempts", "too many password attempts; retry later")
		return false
	}
	return true
}
func (a *application) acquireCrypto(w http.ResponseWriter, r *http.Request) bool {
	select {
	case a.cryptoSlots <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false
	case <-a.ctx.Done():
		return false
	}
}
func (a *application) releaseCrypto() { <-a.cryptoSlots }
func decodeAuthRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		httpx.WriteAPIError(w, 415, "unsupported_media", "use application/json")
		return false
	}
	return decodeRequest(w, r, value)
}

type adminAuth struct {
	app      *application
	store    *sqlStore
	sessions *scs.SessionManager
}

func newAdminAuth(a *application, s *sqlStore, c appconfig.Config) *adminAuth {
	m := scs.New()
	m.Store = &sqlSessionStore{s: s}
	m.Lifetime, _ = time.ParseDuration(c.Auth.SessionLifetime)
	m.IdleTimeout, _ = time.ParseDuration(c.Auth.IdleTimeout)
	m.Cookie.Name = "garmindives_session"
	if c.HTTP.CookieSecure {
		m.Cookie.Name = "__Host-garmindives_session"
	}
	m.Cookie.Secure = c.HTTP.CookieSecure
	m.Cookie.HttpOnly = true
	m.Cookie.SameSite = http.SameSiteStrictMode
	m.Cookie.Path = "/"
	m.Cookie.Persist = true
	m.ErrorFunc = func(w http.ResponseWriter, r *http.Request, err error) {
		if a.logger != nil {
			a.logger.Error("session storage unavailable")
		}
		httpx.WriteAPIError(w, 503, "database_unavailable", "session storage is unavailable")
	}
	return &adminAuth{app: a, store: s, sessions: m}
}
func (a *adminAuth) authenticated(r *http.Request) (bool, error) {
	if !a.sessions.GetBool(r.Context(), "admin") {
		return false, nil
	}
	var version int64
	err := a.store.db.QueryRowContext(r.Context(), `SELECT auth_version FROM admin_credentials WHERE id=1`).Scan(&version)
	if err != nil {
		return false, err
	}
	return version == a.sessions.GetInt64(r.Context(), "auth_version"), nil
}
func (a *adminAuth) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := a.authenticated(r)
		if err != nil {
			httpx.WriteAPIError(w, 503, "database_unavailable", "cannot verify administrator session")
			return
		}
		if !ok {
			httpx.WriteAPIError(w, 401, "authentication_required", "administrator authentication is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *adminAuth) Handler(base http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/auth/session", a.session)
	mux.HandleFunc("POST /api/v1/auth/login", a.login)
	mux.Handle("POST /api/v1/auth/logout", a.requireAdmin(http.HandlerFunc(a.logout)))
	mux.Handle("POST /api/v1/auth/password", a.requireAdmin(http.HandlerFunc(a.changePassword)))
	mux.Handle("GET /api/v1/config", a.requireAdmin(http.HandlerFunc(a.getConfig)))
	mux.Handle("PATCH /api/v1/config", a.requireAdmin(http.HandlerFunc(a.patchConfig)))
	private := a.sessions.LoadAndSave(mux)
	writes := a.sessions.LoadAndSave(a.requireAdmin(base))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") || r.URL.Path == "/api/v1/config" {
			private.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			base.ServeHTTP(w, r)
		default:
			writes.ServeHTTP(w, r)
		}
	})
}

type authSessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	ExpiresAt     string `json:"expires_at,omitempty"`
}

func (a *adminAuth) session(w http.ResponseWriter, r *http.Request) {
	ok, err := a.authenticated(r)
	if err != nil {
		httpx.WriteAPIError(w, 503, "database_unavailable", "cannot verify session")
		return
	}
	response := authSessionResponse{Authenticated: ok}
	if ok {
		response.ExpiresAt = a.sessions.Deadline(r.Context()).UTC().Format(time.RFC3339)
	}
	httpx.WriteJSON(w, 200, response)
}
func (a *adminAuth) login(w http.ResponseWriter, r *http.Request) {
	if !a.app.allowPasswordAttempt(w, r) {
		return
	}
	var request struct {
		Password string `json:"password"`
	}
	if !decodeAuthRequest(w, r, &request) {
		return
	}
	credentials, err := a.store.adminCredentials(r.Context())
	if err != nil {
		httpx.WriteAPIError(w, 503, "database_unavailable", "cannot read administrator credentials")
		return
	}
	if !a.app.acquireCrypto(w, r) {
		return
	}
	ok := security.VerifyPassword(credentials.passwordHash, request.Password)
	a.app.releaseCrypto()
	if !ok {
		httpx.WriteAPIError(w, 401, "invalid_credentials", "invalid administrator password")
		return
	}
	if err := a.sessions.RenewToken(r.Context()); err != nil {
		httpx.WriteAPIError(w, 503, "database_unavailable", "cannot start session")
		return
	}
	a.sessions.Put(r.Context(), "admin", true)
	a.sessions.Put(r.Context(), "auth_version", credentials.version)
	httpx.WriteJSON(w, 200, authSessionResponse{Authenticated: true, ExpiresAt: a.sessions.Deadline(r.Context()).UTC().Format(time.RFC3339)})
}
func (a *adminAuth) logout(w http.ResponseWriter, r *http.Request) {
	if err := a.sessions.Destroy(r.Context()); err != nil {
		httpx.WriteAPIError(w, 503, "database_unavailable", "cannot end session")
		return
	}
	w.WriteHeader(204)
}
func (a *adminAuth) changePassword(w http.ResponseWriter, r *http.Request) {
	if !a.app.allowPasswordAttempt(w, r) {
		return
	}
	var request struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if !decodeAuthRequest(w, r, &request) {
		return
	}
	if err := security.ValidatePassword(request.Next); err != nil {
		httpx.WriteAPIError(w, 400, "invalid_password", err.Error())
		return
	}
	credentials, err := a.store.adminCredentials(r.Context())
	if err != nil {
		httpx.WriteAPIError(w, 503, "database_unavailable", "cannot read administrator credentials")
		return
	}
	if !a.app.acquireCrypto(w, r) {
		return
	}
	if !security.VerifyPassword(credentials.passwordHash, request.Current) {
		a.app.releaseCrypto()
		httpx.WriteAPIError(w, 401, "invalid_credentials", "invalid current password")
		return
	}
	hash, err := security.HashPassword(request.Next)
	a.app.releaseCrypto()
	if err != nil {
		httpx.WriteAPIError(w, 400, "invalid_password", err.Error())
		return
	}
	if err := a.store.setAdminPassword(r.Context(), credentials.version, hash); err != nil {
		httpx.WriteAPIError(w, 409, "credentials_changed", "administrator credentials changed; sign in again")
		return
	}
	if err := a.sessions.Destroy(r.Context()); err != nil {
		httpx.WriteAPIError(w, 503, "database_unavailable", "password changed; sign in again")
		return
	}
	httpx.WriteJSON(w, 200, authSessionResponse{Authenticated: false})
}

type databaseDisplay struct {
	Type          string `json:"type"`
	URIDisplay    string `json:"uri_display"`
	URIConfigured bool   `json:"uri_configured"`
}
type configurationResponse struct {
	Database        databaseDisplay   `json:"database"`
	PendingDatabase databaseDisplay   `json:"pending_database"`
	HTTP            appconfig.HTTP    `json:"http"`
	Auth            appconfig.Auth    `json:"auth"`
	Media           appconfig.Media   `json:"media"`
	Overrides       map[string]string `json:"overrides"`
	RestartRequired bool              `json:"restart_required"`
}

func displayDatabase(c appconfig.Database) databaseDisplay {
	return databaseDisplay{Type: c.Type, URIDisplay: (appconfig.Output{Type: c.Type, Endpoint: c.URI}).LogDestination(), URIConfigured: c.URI != ""}
}
func (a *adminAuth) configResponse() configurationResponse {
	c, _, _ := a.app.snapshot()
	pending, err := a.app.withDatabasePatch(c, databasePatch{})
	if err != nil {
		pending = c
	}
	return configurationResponse{Database: displayDatabase(c.Effective.Database), PendingDatabase: displayDatabase(pending.Effective.Database), HTTP: c.Effective.HTTP, Auth: c.Effective.Auth, Media: c.Effective.Media, Overrides: c.Overrides, RestartRequired: pending.Effective.Database != c.Effective.Database}
}
func (a *adminAuth) getConfig(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, 200, a.configResponse())
}
func (a *adminAuth) patchConfig(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Database databasePatch `json:"database"`
	}
	if !decodeAuthRequest(w, r, &request) {
		return
	}
	if request.Database.Type == nil && request.Database.URI == nil {
		httpx.WriteAPIError(w, 400, "invalid_request", "provide database.type or database.uri")
		return
	}
	select {
	case a.app.gate <- struct{}{}:
		defer func() { <-a.app.gate }()
	default:
		httpx.WriteAPIError(w, 409, "configuration_busy", "another configuration request is in progress")
		return
	}
	c, _, _ := a.app.snapshot()
	next, err := a.app.withDatabasePatch(c, request.Database)
	if err != nil {
		httpx.WriteAPIError(w, 400, "invalid_configuration", err.Error())
		return
	}
	if err := a.app.writeConfig(c.Path, next.File); err != nil {
		httpx.WriteAPIError(w, 500, "configuration_write_failed", "cannot save configuration")
		return
	}
	a.app.mu.Lock()
	a.app.config.File = next.File
	a.app.mu.Unlock()
	httpx.WriteJSON(w, 200, a.configResponse())
}
