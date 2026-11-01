package translation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	envutil "github.com/se0wtf/go-garmindives/internal/env"
	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

const (
	DefaultTranslationInterval = 2 * time.Second
	MaxTranslationRequestBytes = 120 << 10
)

type Config struct {
	APIKey, Endpoint, Locale, Glossary string
	Interval                           time.Duration
}

func FromEnvironment() (Config, error) {
	config := Config{
		APIKey:   strings.TrimSpace(os.Getenv("DEEPL_API_KEY")),
		Locale:   strings.ToUpper(envutil.Default("DEEPL_TARGET_LANG", "EN-GB")),
		Glossary: strings.TrimSpace(os.Getenv("DEEPL_GLOSSARY_ID")),
	}
	if config.Locale != "EN-GB" && config.Locale != "EN-US" {
		return config, errors.New("DEEPL_TARGET_LANG must be EN-GB or EN-US")
	}
	interval, err := time.ParseDuration(envutil.Default("DEEPL_REQUEST_INTERVAL", "2s"))
	if err != nil || interval <= 0 {
		return config, errors.New("DEEPL_REQUEST_INTERVAL must be a positive duration")
	}
	config.Interval = interval
	base := "https://api.deepl.com"
	if strings.HasSuffix(config.APIKey, ":fx") {
		base = "https://api-free.deepl.com"
	}
	base = envutil.Default("DEEPL_API_URL", base)
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return config, errors.New("DEEPL_API_URL must be an HTTPS origin without credentials, query, or path")
	}
	loopback := strings.EqualFold(parsed.Hostname(), "localhost")
	if ip := net.ParseIP(parsed.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return config, errors.New("DEEPL_API_URL requires HTTPS; HTTP is allowed only for a loopback test/proxy")
	}
	config.Endpoint = strings.TrimRight(base, "/") + "/v2/translate"
	return config, nil
}

type Translator interface {
	Translate(context.Context, []string, string) ([]string, error)
}

type Failure struct {
	Code       string
	Retryable  bool
	Blocked    bool
	RetryAfter time.Duration
}

func (e *Failure) Error() string { return e.Code }

type Client struct {
	Config Config
	http   *http.Client
}

func NewClient(config Config) *Client {
	return &Client{Config: config, http: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) RequestBody(texts []string, surrounding string) ([]byte, error) {
	return json.Marshal(struct {
		Text               []string `json:"text"`
		Source             string   `json:"source_lang"`
		Target             string   `json:"target_lang"`
		Context            string   `json:"context"`
		PreserveFormatting bool     `json:"preserve_formatting"`
		Glossary           string   `json:"glossary_id,omitempty"`
	}{texts, "FR", c.Config.Locale, surrounding, true, c.Config.Glossary})
}

func (c *Client) Translate(ctx context.Context, texts []string, surrounding string) ([]string, error) {
	if len(texts) == 0 || len(texts) > 50 {
		return nil, &Failure{Code: "invalid_translation_batch"}
	}
	body, err := c.RequestBody(texts, surrounding)
	if err != nil || len(body) > MaxTranslationRequestBytes {
		return nil, &Failure{Code: "translation_text_too_large"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &Failure{Code: "invalid_translation_configuration", Blocked: true}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "DeepL-Auth-Key "+c.Config.APIKey)
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Do not expose upstream error strings, response bodies, or credentials.
		return nil, &Failure{Code: "translation_network_error", Retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		failure := &Failure{Code: "translation_request_rejected"}
		switch response.StatusCode {
		case 400, 404:
			failure.Code, failure.Blocked = "translation_configuration_error", true
		case 401, 403:
			failure.Code, failure.Blocked = "translation_authentication_failed", true
		case 456:
			failure.Code, failure.Blocked = "translation_quota_exceeded", true
		case 429:
			failure.Code, failure.Retryable = "translation_rate_limited", true
			failure.RetryAfter = httpx.RetryAfter(response.Header.Get("Retry-After"), time.Now())
		case 413:
			failure.Code = "translation_text_too_large"
		default:
			if response.StatusCode >= 500 {
				failure.Code, failure.Retryable = "translation_provider_unavailable", true
				failure.RetryAfter = httpx.RetryAfter(response.Header.Get("Retry-After"), time.Now())
			}
		}
		return nil, failure
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, &Failure{Code: "invalid_translation_response", Retryable: true}
	}
	var decoded struct {
		Translations []struct {
			Text *string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Translations) != len(texts) {
		return nil, &Failure{Code: "invalid_translation_response", Retryable: true}
	}
	result := make([]string, len(texts))
	for i, translated := range decoded.Translations {
		if translated.Text == nil || (strings.TrimSpace(texts[i]) != "" && strings.TrimSpace(*translated.Text) == "") {
			return nil, &Failure{Code: "invalid_translation_response", Retryable: true}
		}
		result[i] = *translated.Text
	}
	return result, nil
}
