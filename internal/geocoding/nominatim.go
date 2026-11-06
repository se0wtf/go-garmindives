package geocoding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	countrydata "github.com/se0wtf/go-garmindives/internal/country"
	envutil "github.com/se0wtf/go-garmindives/internal/env"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

const Attribution = "© OpenStreetMap contributors, ODbL 1.0 — https://www.openstreetmap.org/copyright"

type Config struct {
	Enabled             bool
	Endpoint, UserAgent string
	Interval            time.Duration
}

type Settings struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	URL       string `yaml:"url" json:"url"`
	UserAgent string `yaml:"user_agent" json:"user_agent"`
	Interval  string `yaml:"interval" json:"interval"`
}

func DefaultSettings() Settings {
	return Settings{Enabled: true, URL: "https://nominatim.openstreetmap.org", UserAgent: "go-garmindives/1.0 (https://github.com/se0wtf/go-garmindives)", Interval: "15s"}
}

func FromEnvironment() (Config, error) {
	s := DefaultSettings()
	s.URL = envutil.Default("DIVELOG_GEOCODING_URL", s.URL)
	s.UserAgent = envutil.Default("DIVELOG_GEOCODING_USER_AGENT", s.UserAgent)
	s.Interval = envutil.Default("DIVELOG_GEOCODING_INTERVAL", s.Interval)
	enabled, err := strconv.ParseBool(envutil.Default("DIVELOG_GEOCODING_ENABLED", "true"))
	if err != nil {
		return Config{}, errors.New("geocoding.enabled must be true or false")
	}
	s.Enabled = enabled
	return ParseSettings(s)
}

func ParseSettings(s Settings) (Config, error) {
	c := Config{Enabled: s.Enabled, Endpoint: s.URL, UserAgent: s.UserAgent}
	var err error
	c.Interval, err = time.ParseDuration(s.Interval)
	if err != nil || c.Interval <= 0 {
		return c, errors.New("geocoding.interval must be positive")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, errors.New("geocoding.url must be an origin without credentials, path, or query")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return c, errors.New("geocoding requires HTTPS except for loopback test/proxy servers")
	}
	if strings.EqualFold(u.Hostname(), "nominatim.openstreetmap.org") && c.Interval < 15*time.Second {
		return c, errors.New("public Nominatim background requests require geocoding.interval of at least 15s")
	}
	if len(c.UserAgent) < 8 || strings.ContainsAny(c.UserAgent, "\r\n") {
		return c, errors.New("geocoding.user_agent must identify your application")
	}
	c.Endpoint = strings.TrimRight(c.Endpoint, "/") + "/reverse"
	return c, nil
}

type Lookup interface {
	Lookup(context.Context, float64, float64) (string, error)
}
type Failure struct {
	Code      string
	Retryable bool
	Delay     time.Duration
}

func (e *Failure) Error() string { return e.Code }

type nominatimClient struct {
	config Config
	http   *http.Client
}

func NewClient(config Config) *nominatimClient {
	return &nominatimClient{config: config, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *nominatimClient) Lookup(ctx context.Context, lat, lon float64) (string, error) {
	u, err := url.Parse(c.config.Endpoint)
	if err != nil {
		return "", &Failure{Code: "geocoding_configuration_error"}
	}
	q := u.Query()
	q.Set("format", "jsonv2")
	// Regional boundaries retain overseas territory identifiers (a country-only
	// response labels French Polynesia and New Caledonia as France).
	q.Set("zoom", "5")
	q.Set("addressdetails", "1")
	q.Set("accept-language", "en")
	q.Set("lat", strconv.FormatFloat(lat, 'f', 6, 64))
	q.Set("lon", strconv.FormatFloat(lon, 'f', 6, 64))
	u.RawQuery = q.Encode()
	r, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return "", &Failure{Code: "geocoding_configuration_error"}
	}
	r.Header.Set("User-Agent", c.config.UserAgent)
	r.Header.Set("Accept", "application/json")
	response, err := c.http.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &Failure{Code: "geocoding_network_error", Retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return "", nil
	}
	if response.StatusCode != 200 {
		return "", &Failure{Code: fmt.Sprintf("geocoding_http_%d", response.StatusCode), Retryable: response.StatusCode == 429 || response.StatusCode >= 500, Delay: httpx.RetryAfter(response.Header.Get("Retry-After"), time.Now())}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return "", &Failure{Code: "invalid_geocoding_response", Retryable: true}
	}
	var result struct {
		Address map[string]string `json:"address"`
		Error   string            `json:"error"`
	}
	if json.Unmarshal(body, &result) != nil {
		return "", &Failure{Code: "invalid_geocoding_response", Retryable: true}
	}
	if result.Address["country_code"] == "" {
		if result.Error != "" && result.Error != "Unable to geocode" {
			return "", &Failure{Code: "invalid_geocoding_response", Retryable: true}
		}
		return "", nil
	}
	code := strings.ToUpper(result.Address["country_code"])
	if code == "FR" {
		territories := map[string]string{"FR-PF": "PF", "FR-NC": "NC", "FR-WF": "WF", "FR-PM": "PM", "FR-TF": "TF", "FR-GP": "GP", "FR-MQ": "MQ", "FR-GF": "GF", "FR-RE": "RE", "FR-YT": "YT", "FR-BL": "BL", "FR-MF": "MF"}
		for key, value := range result.Address {
			if strings.HasPrefix(key, "ISO3166-2-") {
				if territory, ok := territories[value]; ok {
					code = territory
					break
				}
			}
		}
	}
	if countrydata.ByCode(code) == nil {
		return "", &Failure{Code: "unknown_geocoding_country"}
	}
	return code, nil
}
