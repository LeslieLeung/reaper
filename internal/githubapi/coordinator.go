package githubapi

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-github/v56/github"
	"github.com/wnarutou/gitrieve/internal/ui"
)

type Config struct {
	Concurrency           uint
	AccountConcurrency    uint
	MinRequestInterval    time.Duration
	LowRemainingThreshold int
}

type Observation struct {
	Resource               string
	Limit, Remaining, Used int
	Reset                  time.Time
	Secondary              bool
	RetryAfter             time.Duration
}

type Permit interface{ Done(Observation) }

type coordinator struct {
	cfg            Config
	mu             sync.Mutex
	active         uint
	changed        chan struct{}
	credentials    map[string]*credentialState
	selection      uint64
	secondaryPause time.Time
}

type permit struct {
	c          *coordinator
	once       sync.Once
	token      string
	credential *credentialState
	resource   string
}

type scopeContextKey struct{}

// Scope records the immutable API configuration accepted with one runtime
// generation. Acquisition always delegates to the stable process arbiter so
// old and new generations share concurrency, pacing, and quota state.
type Scope struct {
	cfg Config
}

func NewScope(cfg Config) *Scope {
	return &Scope{cfg: cfg}
}

func WithScope(ctx context.Context, scope *Scope) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, scopeContextKey{}, scope)
}

func ConfigFromContext(ctx context.Context) (Config, bool) {
	if ctx == nil {
		return Config{}, false
	}
	scope, ok := ctx.Value(scopeContextKey{}).(*Scope)
	if !ok || scope == nil {
		return Config{}, false
	}
	return scope.cfg, true
}

var current atomic.Pointer[coordinator]
var publicationMu sync.RWMutex

// publicationReadBoundaryTestHook is a package-private synchronization seam
// for tests that need to observe the exact publication read-lock boundary.
type publicationReadBoundaryTestHook struct {
	beforeLock  func()
	afterLock   func()
	afterUnlock func()
}

var publicationReadBoundaryHookForTest atomic.Pointer[publicationReadBoundaryTestHook]

func newCoordinator(cfg Config) *coordinator {
	return &coordinator{
		cfg:         normalizedConfig(cfg),
		changed:     make(chan struct{}),
		credentials: make(map[string]*credentialState),
	}
}

func normalizedConfig(cfg Config) Config {
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 1
	}
	if cfg.AccountConcurrency == 0 {
		cfg.AccountConcurrency = 2
	}
	return cfg
}

func (c *coordinator) configure(cfg Config) {
	c.mu.Lock()
	c.cfg = normalizedConfig(cfg)
	c.signalLocked()
	c.mu.Unlock()
}

func (c *coordinator) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Publish installs an application configuration snapshot and its matching
// GitHub coordinator under one reader-visible publication boundary. install
// must only publish immutable state; it runs while publicationMu is held.
func Publish(cfg Config, install func()) {
	publicationMu.Lock()
	defer publicationMu.Unlock()
	if install != nil {
		install()
	}
	c := current.Load()
	if c == nil {
		current.Store(newCoordinator(cfg))
		return
	}
	c.configure(cfg)
}

// ReadPublication runs read while no paired configuration/coordinator
// publication is in progress.
func ReadPublication(read func()) {
	readPublication(read)
}

func readPublication(read func()) {
	hook := publicationReadBoundaryHookForTest.Load()
	if hook != nil && hook.beforeLock != nil {
		hook.beforeLock()
	}
	publicationMu.RLock()
	defer func() {
		publicationMu.RUnlock()
		if hook != nil && hook.afterUnlock != nil {
			hook.afterUnlock()
		}
	}()
	if hook != nil && hook.afterLock != nil {
		hook.afterLock()
	}
	read()
}

func Configure(cfg Config) { Publish(cfg, nil) }

func Acquire(ctx context.Context, resource string) (Permit, error) {
	c := loadCoordinator()
	return c.acquire(ctx, resource)
}

func loadCoordinator() *coordinator {
	var c *coordinator
	readPublication(func() {
		c = current.Load()
	})
	if c != nil {
		return c
	}

	publicationMu.Lock()
	defer publicationMu.Unlock()
	if c = current.Load(); c == nil {
		c = newCoordinator(Config{Concurrency: 1})
		current.Store(c)
	}
	return c
}

func (c *coordinator) acquire(ctx context.Context, resource string) (Permit, error) {
	return c.acquireAccount(ctx, resource, nil, nil)
}

func (p *permit) Done(obs Observation) {
	p.once.Do(func() {
		if obs.Resource == "" && !obs.Reset.IsZero() {
			obs.Resource = p.resource
		}
		p.c.observeCredential(p.credential, obs)
		p.c.mu.Lock()
		if p.c.active > 0 {
			p.c.active--
		}
		if p.credential.active > 0 {
			p.credential.active--
		}
		p.c.signalLocked()
		p.c.mu.Unlock()
	})
}

func (c *coordinator) observeCredential(credential *credentialState, obs Observation) {
	c.mu.Lock()
	now := time.Now()
	secondaryExtended := false
	var secondaryUntil time.Time
	resourceExtended := false
	if obs.Secondary {
		d := obs.RetryAfter
		if d <= 0 {
			d = time.Minute
		}
		if until := now.Add(d); until.After(c.secondaryPause) {
			c.secondaryPause = until
			secondaryUntil = until
			secondaryExtended = true
		}
	}
	if obs.Resource != "" && !obs.Reset.IsZero() && obs.Remaining <= c.cfg.LowRemainingThreshold && obs.Reset.After(credential.resourcePause[obs.Resource]) {
		credential.resourcePause[obs.Resource] = obs.Reset
		resourceExtended = true
	}
	if secondaryExtended || resourceExtended {
		c.signalLocked()
	}
	c.mu.Unlock()
	if secondaryExtended {
		ui.Printf("GitHub API secondary-limit pause active until %s", secondaryUntil.Format(time.RFC3339))
	}
	if resourceExtended {
		ui.Printf("GitHub API %s quota low (%d remaining); paused until %s", obs.Resource, obs.Remaining, obs.Reset.Format(time.RFC3339))
	}
}

func ObserveError(err error) Observation {
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		d := time.Minute
		if abuse.RetryAfter != nil {
			d = *abuse.RetryAfter
		}
		return Observation{Secondary: true, RetryAfter: d}
	}
	var limited *github.RateLimitError
	if errors.As(err, &limited) {
		return Observation{Resource: "core", Remaining: 0, Reset: limited.Rate.Reset.Time}
	}
	return Observation{}
}

func ObserveREST(resp *github.Response, err error) Observation {
	obs := ObserveError(err)
	if resp == nil {
		return obs
	}
	obs.Limit = resp.Rate.Limit
	obs.Remaining = resp.Rate.Remaining
	obs.Reset = resp.Rate.Reset.Time
	if resp.Response != nil {
		obs.Resource = resp.Header.Get("X-RateLimit-Resource")
		obs.Used, _ = strconv.Atoi(resp.Header.Get("X-RateLimit-Used"))
	}
	return obs
}
