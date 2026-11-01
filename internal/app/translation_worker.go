package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	charmLog "github.com/charmbracelet/log"

	countrydata "github.com/se0wtf/go-garmindives/internal/country"
	deeplclient "github.com/se0wtf/go-garmindives/internal/translation"
)

type englishTranslation struct {
	Status          string   `json:"status"`
	Name            string   `json:"name,omitempty"`
	Subtitle        string   `json:"subtitle,omitempty"`
	OperatorComment string   `json:"operator_comment,omitempty"`
	Price           string   `json:"price,omitempty"`
	Comment         string   `json:"comment,omitempty"`
	Highlights      []string `json:"highlights,omitempty"`
	Caption         string   `json:"caption,omitempty"`
	Description     string   `json:"description,omitempty"`
	Error           string   `json:"error,omitempty"`
}

type translations struct {
	English englishTranslation `json:"en"`
}

// A cache entry contains one complete note or media description. Its hash also
// includes the provider/settings version, target locale, and glossary. It never
// includes an API key, entity ID, or filename, allowing identical text to reuse it.
type translationSource struct {
	Kind            string   `json:"kind"`
	Name            string   `json:"name,omitempty"`
	Subtitle        string   `json:"subtitle,omitempty"`
	OperatorComment string   `json:"operator_comment,omitempty"`
	Price           string   `json:"price,omitempty"`
	Comment         string   `json:"comment,omitempty"`
	Highlights      []string `json:"highlights,omitempty"`
	Caption         string   `json:"caption,omitempty"`
	Description     string   `json:"description,omitempty"`
}

func (source translationSource) texts() []string {
	if source.Kind == "dive" {
		return append([]string{source.Comment}, source.Highlights...)
	}
	if source.Kind == "trip" {
		return []string{source.Name, source.Subtitle, source.Description, source.OperatorComment, source.Price}
	}
	return []string{source.Caption, source.Description}
}

func (source translationSource) context() string {
	text := source.Comment
	if source.Kind == "media" || source.Kind == "trip" {
		text = source.Description
	}
	// Supply surrounding prose and domain context, not translation instructions.
	text = "Compte rendu de plongée sous-marine et observations de la vie marine.\n" + text
	runes := []rune(text)
	if len(runes) > 4000 {
		text = string(runes[:4000])
	}
	return text
}

func (source translationSource) empty() bool {
	for _, text := range source.texts() {
		if strings.TrimSpace(text) != "" {
			return false
		}
	}
	return true
}

func (source translationSource) translated(values []*string) (englishTranslation, error) {
	if len(values) != len(source.texts()) {
		return englishTranslation{}, errors.New("invalid cached translation length")
	}
	for _, value := range values {
		if value == nil {
			return englishTranslation{}, errors.New("incomplete cached translation")
		}
	}
	result := englishTranslation{Status: "ready"}
	if source.Kind == "dive" {
		result.Comment = *values[0]
		for _, text := range values[1:] {
			result.Highlights = append(result.Highlights, *text)
		}
	} else if source.Kind == "trip" {
		result.Name, result.Subtitle, result.Description, result.OperatorComment = *values[0], *values[1], *values[2], *values[3]
		result.Price = *values[4]
	} else {
		result.Caption, result.Description = *values[0], *values[1]
	}
	return result, nil
}

type translationService struct {
	store          *sqlStore
	config         deeplclient.Config
	client         deeplclient.Translator
	profile, owner string
	logger         *charmLog.Logger
	wake           chan struct{}
	done           chan struct{}
	cancel         context.CancelFunc
	now            func() time.Time
	mu             sync.Mutex
	blocked        string
	dirty          atomic.Bool
}

func newTranslationService(store *sqlStore, config deeplclient.Config, client deeplclient.Translator, logger *charmLog.Logger) *translationService {
	profile := sha256.Sum256([]byte("deepl/fr/v1/" + config.Locale + "/" + config.Glossary))
	return &translationService{store: store, config: config, client: client, logger: logger,
		profile: hex.EncodeToString(profile[:]), owner: rand.Text(), now: time.Now,
		wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (s *sqlStore) translationService() *translationService {
	if s.translations != nil {
		return s.translations
	}
	// Store-only callers/tests read the default English cache without enabling
	// network access or discovering credentials from the process environment.
	return newTranslationService(s, deeplclient.Config{Locale: "EN-GB", Interval: deeplclient.DefaultTranslationInterval}, nil, nil)
}

func (s *translationService) key(source translationSource) (string, string) {
	data, _ := json.Marshal(source) // Only strings/slices of strings are encoded.
	hash := sha256.Sum256(append([]byte(s.profile+":"), data...))
	return hex.EncodeToString(hash[:]), string(data)
}

func (s *translationService) signal() {
	s.dirty.Store(true)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *sqlStore) translationsChanged() {
	if s.translations != nil {
		s.translations.signal()
	}
}

func (s *translationService) blockedReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}
func (s *translationService) block(code string) { s.mu.Lock(); defer s.mu.Unlock(); s.blocked = code }

func (s *translationService) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	if s.logger != nil {
		s.logger.Info("English translation worker configured", "enabled", s.client != nil,
			"provider", "deepl", "source", "FR", "target", s.config.Locale, "request_interval", s.config.Interval)
	}
	go s.run(ctx)
}

func (s *translationService) Close() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
}

func (s *translationService) pause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	case <-timer.C:
		return true
	}
}

func (s *translationService) run(ctx context.Context) {
	defer close(s.done)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = s.store.db.ExecContext(cleanup, s.store.bind(`UPDATE translation_cache SET lease_owner = '', lease_until = 0 WHERE lease_owner = ?`), s.owner)
	}()
	if s.client != nil {
		if err := s.Retry(ctx); err != nil && ctx.Err() == nil && s.logger != nil {
			s.logger.Warn("could not resume failed translations", "err", err)
		}
	}
	var nextRequest, nextScan time.Time
	for ctx.Err() == nil {
		if s.dirty.Swap(false) || !s.now().Before(nextScan) {
			if err := s.reconcile(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				if s.logger != nil {
					s.logger.Warn("could not reconcile translation cache", "err", err)
				}
				if !s.pause(ctx, 30*time.Second) {
					return
				}
				continue
			}
			nextScan = s.now().Add(30 * time.Second)
		}
		if s.client == nil || s.blockedReason() != "" {
			if !s.pause(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if delay := nextRequest.Sub(s.now()); delay > 0 {
			if !s.pause(ctx, min(delay, 30*time.Second)) {
				return
			}
			continue
		}
		worked, cooldown, err := s.workOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if s.logger != nil {
				s.logger.Warn("translation cache operation failed", "err", err)
			}
			if !s.pause(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if worked {
			nextRequest = s.now().Add(max(s.config.Interval, cooldown))
			continue
		}
		if !s.pause(ctx, 30*time.Second) {
			return
		}
	}
}

// Reconcile snapshots just translatable text, not FIT payloads or original files.
// Inactive source revisions remain cached but are never sent for translation.
func (s *translationService) reconcile(ctx context.Context) error {
	rows, err := s.store.db.QueryContext(ctx, `SELECT note_comment, highlights_json FROM dives ORDER BY id`)
	if err != nil {
		return err
	}
	var sources []translationSource
	for rows.Next() {
		source := translationSource{Kind: "dive"}
		var raw string
		if err := rows.Scan(&source.Comment, &raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &source.Highlights); err != nil {
			rows.Close()
			return errors.New("invalid stored highlights for translation")
		}
		if !source.empty() {
			sources = append(sources, source)
		}
	}
	readErr, closeErr := rows.Err(), rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	rows, err = s.store.db.QueryContext(ctx, `SELECT caption, description FROM dive_media UNION ALL SELECT caption,description FROM trip_media`)
	if err != nil {
		return err
	}
	for rows.Next() {
		source := translationSource{Kind: "media"}
		if err := rows.Scan(&source.Caption, &source.Description); err != nil {
			rows.Close()
			return err
		}
		if !source.empty() {
			sources = append(sources, source)
		}
	}
	readErr, closeErr = rows.Err(), rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	rows, err = s.store.db.QueryContext(ctx, `SELECT name,name_en,name_override,subtitle,description,operator_comment,price FROM dive_trips WHERE deleted=0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t diveTrip
		if err := rows.Scan(&t.Name, &t.NameEN, &t.NameOverridden, &t.Subtitle, &t.Description, &t.OperatorComment, &t.Price); err != nil {
			rows.Close()
			return err
		}
		source := t.translationSource()
		if !source.empty() {
			sources = append(sources, source)
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, s.store.bind(`UPDATE translation_cache SET active = 0 WHERE profile = ? AND active <> 0`), s.profile); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, source := range sources {
		key, raw := s.key(source)
		if seen[key] {
			continue
		}
		seen[key] = true
		now := s.now().UnixMilli()
		if _, err := tx.ExecContext(ctx, s.store.bind(`INSERT INTO translation_cache
			(source_hash, profile, source_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (source_hash) DO UPDATE SET active = 1`), key, s.profile, raw, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type translationJob struct {
	key, sourceJSON, resultJSON string
	attempts                    int
}

func (s *translationService) claim(ctx context.Context) (translationJob, error) {
	now := s.now()
	var job translationJob
	err := s.store.db.QueryRowContext(ctx, s.store.bind(`UPDATE translation_cache SET lease_owner = ?, lease_until = ?
		WHERE source_hash = (SELECT source_hash FROM translation_cache
		WHERE profile = ? AND active = 1 AND status = 'pending' AND retry_at <= ? AND lease_until <= ?
		ORDER BY retry_at, created_at, source_hash LIMIT 1) AND lease_until <= ?
		RETURNING source_hash, source_json, result_json, attempts`),
		s.owner, now.Add(2*time.Minute).UnixMilli(), s.profile, now.UnixMilli(), now.UnixMilli(), now.UnixMilli()).Scan(&job.key, &job.sourceJSON, &job.resultJSON, &job.attempts)
	return job, err
}

func (s *translationService) saveJob(ctx context.Context, job translationJob, result []*string, status, code string, attempts int, delay time.Duration) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	now := s.now()
	tx, err := s.store.beginTravelTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, s.store.bind(`UPDATE translation_cache SET result_json = ?, status = ?,
		last_error = ?, attempts = ?, retry_at = ?, lease_owner = '', lease_until = 0, updated_at = ?
		WHERE source_hash = ? AND lease_owner = ?`), string(raw), status, code, attempts, now.Add(delay).UnixMilli(), now.UnixMilli(), job.key, s.owner)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 && status == "ready" {
		var source translationSource
		if json.Unmarshal([]byte(job.sourceJSON), &source) == nil && source.Kind == "trip" && source.Name != "" {
			if english, err := source.translated(result); err == nil {
				_, err = tx.ExecContext(ctx, s.store.bind(`UPDATE dive_trips SET search_name=? WHERE name_override=1 AND name_en='' AND name=? AND subtitle=? AND description=? AND operator_comment=? AND price=? AND deleted=0`), countrydata.NormalizeSearch(source.Name+" "+english.Name), source.Name, source.Subtitle, source.Description, source.OperatorComment, source.Price)
				if err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

// Process at most one provider batch. Successful batches are saved immediately,
// so retrying a later failed batch does not charge again for completed text.
func (s *translationService) workOnce(ctx context.Context) (bool, time.Duration, error) {
	if s.client == nil || s.blockedReason() != "" {
		return false, 0, nil
	}
	job, err := s.claim(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	var source translationSource
	var result []*string
	if json.Unmarshal([]byte(job.sourceJSON), &source) != nil || json.Unmarshal([]byte(job.resultJSON), &result) != nil {
		return true, 0, s.saveJob(ctx, job, nil, "error", "invalid_cached_translation", job.attempts, 0)
	}
	texts := source.texts()
	if len(result) == 0 {
		result = make([]*string, len(texts))
	}
	if len(result) != len(texts) {
		return true, 0, s.saveJob(ctx, job, nil, "error", "invalid_cached_translation", job.attempts, 0)
	}
	var batch []string
	var groups [][]int
	indexes := map[string]int{}
	completed := map[string]*string{}
	for i, value := range result {
		if value != nil {
			completed[texts[i]] = value
		}
	}
	probe := &deeplclient.Client{Config: s.config}
	for i, text := range texts {
		if result[i] != nil {
			continue
		}
		if strings.TrimSpace(text) == "" {
			value := text
			result[i] = &value
			continue
		}
		if value, ok := completed[text]; ok {
			result[i] = value
			continue
		}
		if index, exists := indexes[text]; exists {
			groups[index] = append(groups[index], i)
			continue
		}
		candidate := append(append([]string{}, batch...), text)
		body, _ := probe.RequestBody(candidate, source.context())
		if len(candidate) > 50 || len(body) > deeplclient.MaxTranslationRequestBytes {
			if len(batch) != 0 {
				break
			}
			return true, 0, s.saveJob(ctx, job, result, "error", "translation_text_too_large", job.attempts, 0)
		}
		indexes[text] = len(batch)
		batch, groups = candidate, append(groups, []int{i})
	}
	if len(batch) == 0 {
		return true, 0, s.saveJob(ctx, job, result, "ready", "", 0, 0)
	}
	translated, err := s.client.Translate(ctx, batch, source.context())
	if ctx.Err() != nil {
		return true, 0, ctx.Err()
	}
	if err != nil {
		failure := &deeplclient.Failure{Code: "translation_network_error", Retryable: true}
		var known *deeplclient.Failure
		if errors.As(err, &known) {
			failure = known
		}
		attempts := job.attempts + 1
		delay := 30 * time.Second * time.Duration(1<<min(attempts-1, 5))
		delay = max(delay, failure.RetryAfter)
		status := "error"
		if failure.Retryable && attempts < 5 {
			status = "pending"
		}
		if failure.Blocked {
			s.block(failure.Code)
		}
		if s.logger != nil {
			s.logger.Warn("English translation deferred", "code", failure.Code, "attempt", attempts, "blocked", failure.Blocked)
		}
		return true, delay, s.saveJob(ctx, job, result, status, failure.Code, attempts, delay)
	}
	if len(translated) != len(batch) {
		return true, 0, s.saveJob(ctx, job, result, "error", "invalid_translation_response", job.attempts, 0)
	}
	for i, text := range translated {
		for _, index := range groups[i] {
			value := text
			result[index] = &value
		}
	}
	status := "ready"
	for _, value := range result {
		if value == nil {
			status = "pending"
			break
		}
	}
	if s.logger != nil {
		s.logger.Info("English translation saved", "status", status, "texts", len(batch))
	}
	return true, 0, s.saveJob(ctx, job, result, status, "", 0, 0)
}

type translationTarget struct {
	source      translationSource
	destination *englishTranslation
	cover       bool
}

// Response assembly does batched SQLite/PostgreSQL lookups only. Missing rows
// wake the worker; no request handler waits for DeepL.
func (s *translationService) populate(ctx context.Context, targets []translationTarget) error {
	type cached struct{ status, result, code string }
	cache := map[string]cached{}
	keys := []string{}
	seen := map[string]bool{}
	for _, target := range targets {
		if target.source.empty() {
			continue
		}
		key, _ := s.key(target.source)
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	for start := 0; start < len(keys); start += 200 {
		end := min(start+200, len(keys))
		args := make([]any, 0, end-start)
		for _, key := range keys[start:end] {
			args = append(args, key)
		}
		query := `SELECT source_hash, status, result_json, last_error FROM translation_cache WHERE source_hash IN (` + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)`
		rows, err := s.store.db.QueryContext(ctx, s.store.bind(query), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key string
			var entry cached
			if err := rows.Scan(&key, &entry.status, &entry.result, &entry.code); err != nil {
				rows.Close()
				return err
			}
			cache[key] = entry
		}
		readErr, closeErr := rows.Err(), rows.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, target := range targets {
		result := englishTranslation{Status: "pending"}
		key, _ := s.key(target.source)
		entry, found := cache[key]
		switch {
		case target.source.empty():
			result.Status = "ready"
		case found && entry.status == "ready":
			var values []*string
			var err error
			if json.Unmarshal([]byte(entry.result), &values) == nil {
				result, err = target.source.translated(values)
			} else {
				err = errors.New("invalid cached JSON")
			}
			if err != nil {
				result = englishTranslation{Status: "error", Error: "invalid_cached_translation"}
			}
		case s.client == nil:
			result.Status = "disabled"
		case s.blockedReason() != "":
			result.Status, result.Error = "error", s.blockedReason()
		case found && entry.status == "error":
			result.Status, result.Error = "error", entry.code
		default:
			s.signal()
		}
		if target.cover {
			result.Caption = ""
		}
		*target.destination = result
	}
	return nil
}

func (s *translationService) Retry(ctx context.Context) error {
	_, err := s.store.db.ExecContext(ctx, s.store.bind(`UPDATE translation_cache SET status = 'pending',
		attempts = 0, retry_at = 0, last_error = '' WHERE profile = ? AND status = 'error'`), s.profile)
	if err == nil {
		s.block("")
		s.signal()
	}
	return err
}
