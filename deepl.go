package main

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
	"strconv"
	"strings"
	"time"
)

const (
	defaultTranslationInterval = 2 * time.Second
	maxTranslationRequestBytes = 120 << 10
)

type translationConfig struct {
	key, endpoint, locale, glossary string
	interval                        time.Duration
}

func translationConfigFromEnvironment() (translationConfig, error) {
	config := translationConfig{
		key:      strings.TrimSpace(os.Getenv("DEEPL_API_KEY")),
		locale:   strings.ToUpper(envDefault("DEEPL_TARGET_LANG", "EN-GB")),
		glossary: strings.TrimSpace(os.Getenv("DEEPL_GLOSSARY_ID")),
	}
	if config.locale != "EN-GB" && config.locale != "EN-US" {
		return config, errors.New("DEEPL_TARGET_LANG must be EN-GB or EN-US")
	}
	interval, err := time.ParseDuration(envDefault("DEEPL_REQUEST_INTERVAL", "2s"))
	if err != nil || interval <= 0 {
		return config, errors.New("DEEPL_REQUEST_INTERVAL must be a positive duration")
	}
	config.interval = interval
	base := "https://api.deepl.com"
	if strings.HasSuffix(config.key, ":fx") {
		base = "https://api-free.deepl.com"
	}
	base = envDefault("DEEPL_API_URL", base)
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
	config.endpoint = strings.TrimRight(base, "/") + "/v2/translate"
	return config, nil
}

type textTranslator interface {
	Translate(context.Context, []string, string) ([]string, error)
}

type translationFailure struct {
	code       string
	retryable  bool
	blocked    bool
	retryAfter time.Duration
}

func (e *translationFailure) Error() string { return e.code }

type deeplClient struct {
	config translationConfig
	http   *http.Client
}

func newDeepLClient(config translationConfig) *deeplClient {
	return &deeplClient{config: config, http: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *deeplClient) requestBody(texts []string, surrounding string) ([]byte, error) {
	return json.Marshal(struct {
		Text               []string `json:"text"`
		Source             string   `json:"source_lang"`
		Target             string   `json:"target_lang"`
		Context            string   `json:"context"`
		PreserveFormatting bool     `json:"preserve_formatting"`
		Glossary           string   `json:"glossary_id,omitempty"`
	}{texts, "FR", c.config.locale, surrounding, true, c.config.glossary})
}

func (c *deeplClient) Translate(ctx context.Context, texts []string, surrounding string) ([]string, error) {
	if len(texts) == 0 || len(texts) > 50 {
		return nil, &translationFailure{code: "invalid_translation_batch"}
	}
	body, err := c.requestBody(texts, surrounding)
	if err != nil || len(body) > maxTranslationRequestBytes {
		return nil, &translationFailure{code: "translation_text_too_large"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &translationFailure{code: "invalid_translation_configuration", blocked: true}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "DeepL-Auth-Key "+c.config.key)
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Do not expose upstream error strings, response bodies, or credentials.
		return nil, &translationFailure{code: "translation_network_error", retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		failure := &translationFailure{code: "translation_request_rejected"}
		switch response.StatusCode {
		case 400, 404:
			failure.code, failure.blocked = "translation_configuration_error", true
		case 401, 403:
			failure.code, failure.blocked = "translation_authentication_failed", true
		case 456:
			failure.code, failure.blocked = "translation_quota_exceeded", true
		case 429:
			failure.code, failure.retryable = "translation_rate_limited", true
			failure.retryAfter = translationRetryAfter(response.Header.Get("Retry-After"), time.Now())
		case 413:
			failure.code = "translation_text_too_large"
		default:
			if response.StatusCode >= 500 {
				failure.code, failure.retryable = "translation_provider_unavailable", true
				failure.retryAfter = translationRetryAfter(response.Header.Get("Retry-After"), time.Now())
			}
		}
		return nil, failure
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, &translationFailure{code: "invalid_translation_response", retryable: true}
	}
	var decoded struct {
		Translations []struct {
			Text *string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Translations) != len(texts) {
		return nil, &translationFailure{code: "invalid_translation_response", retryable: true}
	}
	result := make([]string, len(texts))
	for i, translated := range decoded.Translations {
		if translated.Text == nil || (strings.TrimSpace(texts[i]) != "" && strings.TrimSpace(*translated.Text) == "") {
			return nil, &translationFailure{code: "invalid_translation_response", retryable: true}
		}
		result[i] = *translated.Text
	}
	return result, nil
}

func translationRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64((1<<63-1)/int64(time.Second)) {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}
