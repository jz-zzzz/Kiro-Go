package proxy

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
)

// kamImportConcurrency bounds how many refresh-token exchanges run in parallel
// during a bulk import. Each candidate needs one upstream AWS/Kiro round-trip to
// turn its refreshToken into a usable accessToken; too much parallelism risks the
// upstream rate-limiting (or IP-banning) the whole batch. Earlier runs at 10
// concurrent saw a large fraction of rows fail with transient 429s that a single
// non-retried exchange could not survive, so we keep the fan-out modest.
const kamImportConcurrency = 5

// kamExchangeMaxAttempts is how many times a single row's token exchange is
// retried before giving up. Only transient failures (429 / 5xx / network) are
// retried; a hard credential rejection (invalid_grant — a dead refresh token)
// fails immediately since no amount of retrying will revive it.
const kamExchangeMaxAttempts = 4

// kamExchangeBaseBackoff is the first retry delay; subsequent attempts back off
// exponentially with jitter to spread load off the auth endpoint.
const kamExchangeBaseBackoff = 700 * time.Millisecond

// kamExchangeJitterMax is the upper bound of the small random pre-exchange delay
// added to every attempt so the bounded worker pool does not fire all of its
// requests at the exact same instant.
const kamExchangeJitterMax = 250 * time.Millisecond

// kamJobRetention is how long a finished job is kept queryable before the next
// job-creation sweep evicts it, so the polling frontend can still fetch the
// final summary after completion without leaking memory indefinitely.
const kamJobRetention = 15 * time.Minute

// kamMaxErrorsTracked caps the per-job error list so a batch where every row
// fails cannot grow the job snapshot without bound.
const kamMaxErrorsTracked = 100

type kamImportStatus string

const (
	kamStatusRunning  kamImportStatus = "running"
	kamStatusComplete kamImportStatus = "complete"
	kamStatusError    kamImportStatus = "error"
)

// kamImportItem is one entry in the pasted JSON array. Only refreshToken is
// required; everything else is optional and inferred when missing. The format
// seen in the wild is: {"refreshToken","provider","email","pwd"} — note there
// is no accessToken, so each row must be exchanged upstream before it is usable.
type kamImportItem struct {
	RefreshToken string `json:"refreshToken"`
	AccessToken  string `json:"accessToken"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Provider     string `json:"provider"`
	Email        string `json:"email"`
	Region       string `json:"region"`
	AuthMethod   string `json:"authMethod"`
}

// kamImportJob holds the live state of one bulk import. All mutable fields are
// guarded by mu because the worker goroutines update counters concurrently and
// the HTTP status handler reads them.
type kamImportJob struct {
	mu         sync.Mutex
	id         string
	status     kamImportStatus
	total      int
	processed  int
	added      int
	skippedDup int
	failed     int
	// Failure breakdown so the operator can tell "dead credentials" apart from
	// "we hammered the auth endpoint too hard". failed == failRateLimited +
	// failDeadToken + failOther.
	failRateLimited int
	failDeadToken   int
	failOther       int
	errors          []string
	startedAt       int64
	finishedAt      int64
}

func (j *kamImportJob) incProcessed() {
	j.mu.Lock()
	j.processed++
	j.mu.Unlock()
}

func (j *kamImportJob) addError(msg string) {
	j.mu.Lock()
	if len(j.errors) < kamMaxErrorsTracked {
		j.errors = append(j.errors, msg)
	}
	j.mu.Unlock()
}

// kamFailKind classifies why a single token exchange failed, so the final
// summary can separate "dead credentials" (nothing we can do) from "rate
// limited" (worth re-importing) and everything else.
type kamFailKind int

const (
	kamFailKindOther kamFailKind = iota
	kamFailKindRateLimited
	kamFailKindDeadToken
)

// classifyExchangeError maps an exchange error onto a failure bucket. The auth
// endpoint returns "refresh failed: <status> <body>"; we key off the HTTP
// status text and the well-known invalid_grant marker for dead refresh tokens.
func classifyExchangeError(err error) kamFailKind {
	if err == nil {
		return kamFailKindOther
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "400") ||
		strings.Contains(lower, "401") ||
		strings.Contains(lower, "403"):
		return kamFailKindDeadToken
	case strings.Contains(lower, "429") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "throttl") ||
		strings.Contains(lower, "rate"):
		return kamFailKindRateLimited
	default:
		return kamFailKindOther
	}
}

// recordFailure bumps the total and the matching per-kind counter under the
// lock so concurrent workers stay consistent.
func (j *kamImportJob) recordFailure(kind kamFailKind) {
	j.mu.Lock()
	j.failed++
	switch kind {
	case kamFailKindRateLimited:
		j.failRateLimited++
	case kamFailKindDeadToken:
		j.failDeadToken++
	default:
		j.failOther++
	}
	j.mu.Unlock()
}

// kamJobSnapshot is the JSON-serializable view returned to the polling frontend.
type kamJobSnapshot struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	Total      int      `json:"total"`
	Processed  int      `json:"processed"`
	Added      int      `json:"added"`
	SkippedDup int      `json:"skippedDup"`
	Failed     int      `json:"failed"`
	// Failure breakdown surfaced to the frontend summary.
	FailRateLimited int      `json:"failRateLimited"`
	FailDeadToken   int      `json:"failDeadToken"`
	FailOther       int      `json:"failOther"`
	Errors          []string `json:"errors,omitempty"`
	StartedAt       int64    `json:"startedAt"`
	FinishedAt      int64    `json:"finishedAt,omitempty"`
}

func (j *kamImportJob) snapshot() kamJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	errs := make([]string, len(j.errors))
	copy(errs, j.errors)
	return kamJobSnapshot{
		ID:              j.id,
		Status:          string(j.status),
		Total:           j.total,
		Processed:       j.processed,
		Added:           j.added,
		SkippedDup:      j.skippedDup,
		Failed:          j.failed,
		FailRateLimited: j.failRateLimited,
		FailDeadToken:   j.failDeadToken,
		FailOther:       j.failOther,
		Errors:          errs,
		StartedAt:       j.startedAt,
		FinishedAt:      j.finishedAt,
	}
}

// kamImportManager tracks in-flight and recently-finished import jobs.
type kamImportManager struct {
	mu   sync.Mutex
	jobs map[string]*kamImportJob
}

func newKamImportManager() *kamImportManager {
	return &kamImportManager{jobs: make(map[string]*kamImportJob)}
}

// newJob registers a job and opportunistically evicts finished jobs whose
// retention window has elapsed.
func (m *kamImportManager) newJob(total int) *kamImportJob {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for id, j := range m.jobs {
		j.mu.Lock()
		expired := j.finishedAt > 0 && now.Unix()-j.finishedAt > int64(kamJobRetention/time.Second)
		j.mu.Unlock()
		if expired {
			delete(m.jobs, id)
		}
	}

	job := &kamImportJob{
		id:        auth.GenerateAccountID(),
		status:    kamStatusRunning,
		total:     total,
		startedAt: now.Unix(),
	}
	m.jobs[job.id] = job
	return job
}

func (m *kamImportManager) get(id string) *kamImportJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// parseKamInput accepts either a bare JSON array of items or an object wrapping
// them under "accounts". It returns the de-duplicated candidate list (dups within
// the paste collapsed) plus how many in-paste duplicates / empty rows were dropped.
func parseKamInput(raw string) ([]kamImportItem, int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, 0, fmt.Errorf("empty input")
	}

	var items []kamImportItem
	// Try a bare array first, then the {"accounts":[...]} envelope.
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		var wrapper struct {
			Accounts []kamImportItem `json:"accounts"`
		}
		if werr := json.Unmarshal([]byte(raw), &wrapper); werr != nil {
			return nil, 0, fmt.Errorf("invalid JSON: %v", err)
		}
		items = wrapper.Accounts
	}

	deduped := make([]kamImportItem, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	skipped := 0
	for _, it := range items {
		it.RefreshToken = strings.TrimSpace(it.RefreshToken)
		if it.RefreshToken == "" {
			skipped++
			continue
		}
		if _, dup := seen[it.RefreshToken]; dup {
			skipped++
			continue
		}
		seen[it.RefreshToken] = struct{}{}
		deduped = append(deduped, it)
	}
	return deduped, skipped, nil
}

// normalizeKamItem resolves authMethod/provider/region defaults the same way the
// single-account credentials importer does, so bulk and single paths agree.
func normalizeKamItem(it *kamImportItem) {
	if it.Region == "" {
		it.Region = "us-east-1"
	}
	switch strings.ToLower(it.AuthMethod) {
	case "idc", "builderid", "enterprise":
		it.AuthMethod = "idc"
	case "social", "google", "github":
		it.AuthMethod = "social"
	default:
		if it.ClientID != "" && it.ClientSecret != "" {
			it.AuthMethod = "idc"
		} else {
			it.AuthMethod = "social"
		}
	}
	if it.Provider == "" {
		if it.AuthMethod == "idc" {
			it.Provider = "BuilderId"
		} else {
			it.Provider = "Google"
		}
	}
}

// startKamImport launches the background worker pool for a bulk import and returns
// the job immediately so the caller can hand the id back to the polling client.
func (h *Handler) startKamImport(items []kamImportItem, preSkipped int) *kamImportJob {
	job := h.kamImports.newJob(len(items))
	job.mu.Lock()
	job.skippedDup += preSkipped
	job.mu.Unlock()

	go h.runKamImport(job, items)
	return job
}

func (h *Handler) runKamImport(job *kamImportJob, items []kamImportItem) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("[KamImport] job %s panicked: %v", job.id, r)
			job.mu.Lock()
			job.status = kamStatusError
			job.finishedAt = time.Now().Unix()
			job.mu.Unlock()
		}
	}()

	var (
		wg        sync.WaitGroup
		sem       = make(chan struct{}, kamImportConcurrency)
		collectMu sync.Mutex
		ready     = make([]config.Account, 0, len(items))
	)

	for i := range items {
		it := items[i]

		// Cheap pre-dedup against already-persisted accounts: skip the upstream
		// token exchange entirely when the refresh token is already known.
		if config.RefreshTokenExists(it.RefreshToken) {
			job.mu.Lock()
			job.skippedDup++
			job.mu.Unlock()
			job.incProcessed()
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			acc, err := h.exchangeKamItem(it)
			if err != nil {
				job.recordFailure(classifyExchangeError(err))
				label := it.Email
				if label == "" {
					label = maskToken(it.RefreshToken)
				}
				job.addError(fmt.Sprintf("%s: %v", label, err))
				job.incProcessed()
				return
			}
			collectMu.Lock()
			ready = append(ready, acc)
			collectMu.Unlock()
			job.incProcessed()
		}()
	}

	wg.Wait()

	// Single locked write of everything that exchanged successfully. AddAccounts
	// dedups again (covers in-flight collisions) and Saves exactly once.
	added, skippedDup, err := config.AddAccounts(ready)
	job.mu.Lock()
	if err != nil {
		job.status = kamStatusError
		job.addError(fmt.Sprintf("persist failed: %v", err))
	} else {
		job.added = added
		job.skippedDup += skippedDup
		job.status = kamStatusComplete
	}
	job.finishedAt = time.Now().Unix()
	job.mu.Unlock()

	if err == nil && added > 0 {
		h.pool.Reload()
	}
	// Always log the final tally (even when nothing was added) with the failure
	// breakdown, so the operator can tell at a glance whether the failures were
	// dead credentials (re-importing won't help) or rate limiting (re-import to
	// recover the rows that a transient 429 killed).
	snap := job.snapshot()
	logger.Infof("[KamImport] job %s done: added=%d skippedDup=%d failed=%d (rateLimited=%d deadToken=%d other=%d)",
		job.id, snap.Added, snap.SkippedDup, snap.Failed, snap.FailRateLimited, snap.FailDeadToken, snap.FailOther)
}

// exchangeKamItem turns one candidate into a ready-to-store Account by exchanging
// its refresh token for a live access token. Email is taken from the paste when
// present (avoids an extra round-trip); otherwise we ask upstream. The background
// refresh loop later fills/corrects email, subscription and usage.
//
// The exchange is retried with exponential backoff + jitter on transient
// failures (429 / 5xx / network). A dead refresh token (invalid_grant) fails
// immediately — retrying it only wastes time and adds load. This is the core
// fix for the earlier run where ~750 rows failed: most were "rate limited"
// rows that a single non-retried attempt could not survive.
func (h *Handler) exchangeKamItem(it kamImportItem) (config.Account, error) {
	normalizeKamItem(&it)

	tmp := &config.Account{
		RefreshToken: it.RefreshToken,
		AccessToken:  it.AccessToken,
		ClientID:     it.ClientID,
		ClientSecret: it.ClientSecret,
		AuthMethod:   it.AuthMethod,
		Region:       it.Region,
	}

	var (
		accessToken, refreshToken, profileArn string
		expiresAt                             int64
		lastErr                               error
	)
	for attempt := 0; attempt < kamExchangeMaxAttempts; attempt++ {
		// A small jitter before every attempt keeps the bounded pool from
		// firing all of its in-flight requests at the exact same instant.
		time.Sleep(time.Duration(rand.Int63n(int64(kamExchangeJitterMax))))

		accessToken, refreshToken, expiresAt, profileArn, lastErr = auth.RefreshToken(tmp)
		if lastErr == nil {
			break
		}
		// Dead credentials never recover — stop immediately.
		if classifyExchangeError(lastErr) == kamFailKindDeadToken {
			return config.Account{}, fmt.Errorf("token exchange failed: %w", lastErr)
		}
		// Transient: back off (exponentially, with jitter) and retry, unless
		// this was the final attempt.
		if attempt < kamExchangeMaxAttempts-1 {
			backoff := kamExchangeBaseBackoff * time.Duration(1<<attempt)
			backoff += time.Duration(rand.Int63n(int64(kamExchangeJitterMax)))
			time.Sleep(backoff)
		}
	}
	if lastErr != nil {
		return config.Account{}, fmt.Errorf("token exchange failed: %w", lastErr)
	}
	if refreshToken == "" {
		refreshToken = it.RefreshToken
	}

	email := it.Email
	if email == "" {
		if e, _, uerr := auth.GetUserInfo(accessToken); uerr == nil {
			email = e
		}
	}

	return config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     it.ClientID,
		ClientSecret: it.ClientSecret,
		AuthMethod:   it.AuthMethod,
		Provider:     it.Provider,
		Region:       it.Region,
		ExpiresAt:    expiresAt,
		ProfileArn:   profileArn,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}, nil
}

// maskToken renders a short, non-sensitive label for a refresh token so error
// lines can identify a failed row without leaking the full credential.
func maskToken(tok string) string {
	if len(tok) <= 12 {
		return "token:****"
	}
	return "token:" + tok[:6] + "…" + tok[len(tok)-4:]
}

// apiKamImportStart parses the pasted batch, kicks off the background worker
// pool and returns the job id immediately so the client can poll for progress.
// The whole exchange (1000+ rows, each an upstream round-trip) takes minutes, so
// it must never run inline within the request.
func (h *Handler) apiKamImportStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	items, preSkipped, err := parseKamInput(req.Data)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(items) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":      "no importable rows (all empty or duplicate)",
			"skippedDup": preSkipped,
		})
		return
	}

	job := h.startKamImport(items, preSkipped)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"jobId":   job.id,
		"total":   len(items),
	})
}

// apiKamImportStatus returns the live snapshot for a job id so the frontend can
// render a progress bar and the final summary.
func (h *Handler) apiKamImportStatus(w http.ResponseWriter, r *http.Request, id string) {
	job := h.kamImports.get(id)
	if job == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "import job not found"})
		return
	}
	json.NewEncoder(w).Encode(job.snapshot())
}
