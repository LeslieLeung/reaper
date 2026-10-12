package config

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/retry"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"github.com/wnarutou/gitrieve/internal/ui"
)

const (
	DefaultSyncOverdueGrace   = 30 * time.Minute
	DefaultSyncStuckThreshold = 24 * time.Hour
)

type Config struct {
	Repository                  []typedef.Repository   `yaml:"repository"`
	Storage                     []typedef.MultiStorage `yaml:"storage"`
	GitHubToken                 string                 `yaml:"githubToken"`
	ConcurrencyNum              uint                   `yaml:"cocurrencyNum" mapstructure:"cocurrencyNum"`
	ReleaseSizeLimit            int                    `yaml:"releaseSizeLimit"`
	ReleaseNumLimit             int                    `yaml:"releaseNumLimit"`
	RetryMaxCount               int                    `yaml:"retryMaxCount"`
	RetryBaseDelay              time.Duration          `yaml:"retryBaseDelay"`
	SyncOverdueGrace            time.Duration          `yaml:"syncOverdueGrace" mapstructure:"syncOverdueGrace"`
	SyncStuckThreshold          time.Duration          `yaml:"syncStuckThreshold" mapstructure:"syncStuckThreshold"`
	GitHubAPIConcurrency        uint                   `yaml:"githubApiConcurrency" mapstructure:"githubApiConcurrency"`
	GitHubMinRequestInterval    time.Duration          `yaml:"githubMinRequestInterval" mapstructure:"githubMinRequestInterval"`
	GitHubLowRemainingThreshold int                    `yaml:"githubLowRemainingThreshold" mapstructure:"githubLowRemainingThreshold"`
	GitHubScheduleJitter        time.Duration          `yaml:"githubScheduleJitter" mapstructure:"githubScheduleJitter"`
}

var Path string

var vp *viper.Viper
var stateMu sync.Mutex
var ins atomic.Pointer[Config]
var publishGitHubAPI = githubapi.Publish

type executionConfigContextKey struct{}

// ExecutionSnapshot is an immutable configuration generation carried by one
// admitted executor job. Its contents are never republished process-wide.
type ExecutionSnapshot struct {
	config *Config
}

func NewExecutionSnapshot(cfg *Config) *ExecutionSnapshot {
	return &ExecutionSnapshot{config: Clone(cfg)}
}

// WithExecutionSnapshot binds a frozen configuration generation to ctx.
func WithExecutionSnapshot(ctx context.Context, snapshot *ExecutionSnapshot) context.Context {
	if snapshot == nil {
		return ctx
	}
	return context.WithValue(ctx, executionConfigContextKey{}, snapshot)
}

// GetExecutionConfig returns the job-bound configuration when present, and
// preserves the package-global behavior for CLI/daemon callers otherwise.
func GetExecutionConfig(ctx context.Context) *Config {
	if ctx != nil {
		if snapshot, ok := ctx.Value(executionConfigContextKey{}).(*ExecutionSnapshot); ok && snapshot != nil {
			return Clone(snapshot.config)
		}
	}
	return GetIns()
}

// configPublicationReadTestHook is a package-private synchronization seam for
// tests that need to substitute the publication read gate itself.
type configPublicationReadTestHook struct {
	read func(func())
}

var configPublicationReadHookForTest atomic.Pointer[configPublicationReadTestHook]

func Init() {
	nextViper := viper.New()
	nextViper.SetConfigFile(Path)
	err := nextViper.ReadInConfig()
	if err != nil {
		ui.ErrorfExit("Error reading config file, %s", err)
	}
	var next Config
	err = nextViper.Unmarshal(&next)
	if err != nil {
		ui.ErrorfExit("Error unmarshalling config file, %s", err)
	}
	seedDefaults(&next)
	if !nextViper.IsSet("githubScheduleJitter") {
		next.GitHubScheduleJitter = 30 * time.Second
	}
	// Every repository entry must have an identity before publication.
	if err := validateIdentity(&next); err != nil {
		ui.ErrorfExit("Invalid configuration: %s", err)
	}
	if err := validateStorages(next.Storage); err != nil {
		ui.ErrorfExit("Invalid configuration: %s", err)
	}
	snapshot := Clone(&next)
	stateMu.Lock()
	vp = nextViper
	setInsLocked(snapshot)
	stateMu.Unlock()
}

func validateStorages(storages []typedef.MultiStorage) error {
	for _, storage := range storages {
		if err := storage.ValidateType(); err != nil {
			return err
		}
	}
	return nil
}

// seedDefaults fills zero-valued global options with their defaults. It runs in
// Init and Reload (both single-threaded) so the in-memory config always has the
// same interpretation as the getters; lazy mutation in getters would race under
// the daemon's concurrent workers.
func seedDefaults(cfg *Config) {
	if cfg.RetryMaxCount <= 0 {
		cfg.RetryMaxCount = 3
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = 5 * time.Second
	}
	if cfg.SyncOverdueGrace <= 0 {
		cfg.SyncOverdueGrace = DefaultSyncOverdueGrace
	}
	if cfg.SyncStuckThreshold <= 0 {
		cfg.SyncStuckThreshold = DefaultSyncStuckThreshold
	}
	if cfg.ConcurrencyNum == 0 {
		cfg.ConcurrencyNum = 3
	}
	if cfg.ReleaseNumLimit == 0 {
		cfg.ReleaseNumLimit = 3
	}
	if cfg.ReleaseSizeLimit == 0 {
		cfg.ReleaseSizeLimit = 300000000
	}
	if cfg.GitHubAPIConcurrency == 0 {
		cfg.GitHubAPIConcurrency = 2
	}
	if cfg.GitHubMinRequestInterval <= 0 {
		cfg.GitHubMinRequestInterval = 200 * time.Millisecond
	}
	if cfg.GitHubLowRemainingThreshold <= 0 {
		cfg.GitHubLowRemainingThreshold = 100
	}
	if cfg.GitHubScheduleJitter < 0 {
		cfg.GitHubScheduleJitter = 30 * time.Second
	}
}

// Clone returns a deep copy suitable for copy-on-write configuration updates.
func Clone(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	cloned.Repository = make([]typedef.Repository, len(cfg.Repository))
	for index, repository := range cfg.Repository {
		repository.Storage = append([]string(nil), repository.Storage...)
		cloned.Repository[index] = repository
	}
	cloned.Storage = append([]typedef.MultiStorage(nil), cfg.Storage...)
	return &cloned
}

func GetIns() *Config {
	return Clone(loadPublishedConfig())
}

// SetIns replaces the package-global config instance. Used by apply/import to
// publish a fully-built replacement so concurrent readers (job goroutines)
// observe a complete old or complete new instance, never a torn one.
func SetIns(cfg *Config) {
	PublishSnapshot(cfg, nil)
}

func setInsLocked(snapshot *Config) {
	publishSnapshotLocked(snapshot, nil, nil)
}

// PublishSnapshot installs package config, the stable GitHub arbiter policy,
// and any caller-owned immutable runtime state through one reader-observed
// boundary. install must not perform network or persistence work.
func PublishSnapshot(cfg *Config, install func(*Config)) *Config {
	stateMu.Lock()
	defer stateMu.Unlock()
	return publishSnapshotLocked(cfg, nil, install)
}

func publishSnapshotLocked(cfg *Config, nextViper *viper.Viper, install func(*Config)) *Config {
	published := Clone(cfg)
	publishGitHubAPI(gitHubAPIConfig(published), func() {
		if nextViper != nil {
			vp = nextViper
		}
		ins.Store(published)
		if install != nil {
			install(Clone(published))
		}
	})
	return Clone(published)
}

func currentConfig() *Config {
	cfg := loadPublishedConfig()
	if cfg == nil {
		return &Config{}
	}
	return cfg
}

func loadPublishedConfig() *Config {
	var cfg *Config
	readPublication := githubapi.ReadPublication
	if hook := configPublicationReadHookForTest.Load(); hook != nil {
		readPublication = hook.read
	}
	readPublication(func() {
		cfg = ins.Load()
	})
	return cfg
}

// GetViper returns a detached copy of the viper instance that loaded the
// config file. It is nil until Init has run (registered via cobra.OnInitialize,
// so it runs before any command executes). Mutating the returned instance
// cannot bypass the package's configuration publication lock.
func GetViper() *viper.Viper {
	stateMu.Lock()
	defer stateMu.Unlock()
	if vp == nil {
		return nil
	}
	detached := viper.New()
	_ = detached.MergeConfigMap(cloneViperSettings(vp.AllSettings()))
	return detached
}

func cloneViperSettings(settings map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(settings))
	for key, value := range settings {
		if value == nil {
			cloned[key] = nil
			continue
		}
		cloned[key] = cloneViperValue(reflect.ValueOf(value)).Interface()
	}
	return cloned
}

func cloneViperValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return reflect.ValueOf(nil)
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneViperValue(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(cloneViperValue(value.Elem()))
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(iterator.Key(), cloneViperValue(iterator.Value()))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneViperValue(value.Index(index)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneViperValue(value.Index(index)))
		}
		return result
	case reflect.Struct:
		result := reflect.New(value.Type()).Elem()
		result.Set(value)
		for index := 0; index < value.NumField(); index++ {
			if result.Field(index).CanSet() && value.Field(index).CanInterface() {
				result.Field(index).Set(cloneViperValue(value.Field(index)))
			}
		}
		return result
	default:
		return value
	}
}

func GetStorageMap() map[string]typedef.MultiStorage {
	storageMap := make(map[string]typedef.MultiStorage)
	for _, storage := range currentConfig().Storage {
		storageMap[storage.Name] = storage
	}
	return storageMap
}

// GetReleaseNumLimit returns the max number of releases to keep. Init seeds it
// to 3 when the config value is zero; a negative value means "no limit". It is
// read-only (no lazy mutation) so it is safe under concurrent workers.
func GetReleaseNumLimit() int {
	return currentConfig().ReleaseNumLimit
}

func GetReleaseNumLimitContext(ctx context.Context) int {
	return executionConfig(ctx).ReleaseNumLimit
}

// GetReleaseSizeLimit returns the max total release size to keep. Init seeds it
// to 300000000 when the config value is zero; a negative value means "no
// limit". It is read-only so it is safe under concurrent workers.
func GetReleaseSizeLimit() int {
	return currentConfig().ReleaseSizeLimit
}

func GetReleaseSizeLimitContext(ctx context.Context) int {
	return executionConfig(ctx).ReleaseSizeLimit
}

// GetConcurrencyNum returns the max number of concurrent scheduler jobs. Init
// seeds it to 3 when the config value is zero. It is read-only so it is safe
// under concurrent workers.
func GetConcurrencyNum() uint {
	return currentConfig().ConcurrencyNum
}

// GetRetryMaxCount returns the configured max retries per API call. Init seeds
// it to 3 when the config value is zero or negative, so this getter is
// read-only.
func GetRetryMaxCount() int {
	return currentConfig().RetryMaxCount
}

// GetRetryBaseDelay returns the exponential-backoff base delay. Init seeds it
// to 5 seconds when the config value is zero or negative, so this getter is
// read-only.
func GetRetryBaseDelay() time.Duration {
	return currentConfig().RetryBaseDelay
}

func GetSyncOverdueGrace() time.Duration { return currentConfig().SyncOverdueGrace }

func GetSyncStuckThreshold() time.Duration { return currentConfig().SyncStuckThreshold }

func GetGitHubAPIConcurrency() uint { return currentConfig().GitHubAPIConcurrency }

func GetGitHubMinRequestInterval() time.Duration { return currentConfig().GitHubMinRequestInterval }

func GetGitHubLowRemainingThreshold() int { return currentConfig().GitHubLowRemainingThreshold }

func GetGitHubScheduleJitter() time.Duration { return currentConfig().GitHubScheduleJitter }

func GetGitHubAPIConfig() githubapi.Config {
	return gitHubAPIConfig(currentConfig())
}

// GitHubAPIConfigFrom returns the API-coordination inputs belonging to cfg.
func GitHubAPIConfigFrom(cfg *Config) githubapi.Config {
	return gitHubAPIConfig(cfg)
}

func gitHubAPIConfig(cfg *Config) githubapi.Config {
	if cfg == nil {
		return githubapi.Config{}
	}
	return githubapi.Config{Concurrency: cfg.GitHubAPIConcurrency, MinRequestInterval: cfg.GitHubMinRequestInterval, LowRemainingThreshold: cfg.GitHubLowRemainingThreshold}
}

// GetRetryConfig assembles the retry configuration used by every GitHub API
// call site in the issue/discussion/release syncs.
func GetRetryConfig() retry.Config {
	return retry.Config{
		MaxRetries: GetRetryMaxCount(),
		BaseDelay:  GetRetryBaseDelay(),
	}
}

func GetRetryConfigContext(ctx context.Context) retry.Config {
	cfg := executionConfig(ctx)
	return retry.Config{MaxRetries: cfg.RetryMaxCount, BaseDelay: cfg.RetryBaseDelay}
}

func executionConfig(ctx context.Context) *Config {
	if ctx != nil {
		if snapshot, ok := ctx.Value(executionConfigContextKey{}).(*ExecutionSnapshot); ok && snapshot != nil && snapshot.config != nil {
			return snapshot.config
		}
	}
	return currentConfig()
}

// validateIdentity ensures every repository entry has a usable identity (a
// non-empty URL, or orgName for user/org types). The repository identity is the
// normalized URL; an entry without one can never be matched or executed.
// Returns an error rather than exiting so it is unit-testable; Init surfaces
// it via ui.ErrorfExit.
func validateIdentity(cfg *Config) error {
	for _, repo := range cfg.Repository {
		if repo.Key() == "" {
			return fmt.Errorf("repository %q (type %q) has an empty URL and no orgName; every repository needs a URL identity",
				repo.Name, repo.GetType())
		}
	}
	return nil
}

// Save persists the current in-memory config back to the config file via viper.
func Save() error {
	stateMu.Lock()
	defer stateMu.Unlock()
	return saveSnapshotLocked(currentConfig())
}

// SaveSnapshot persists cfg through the current viper instance without
// republishing it. Callers can therefore save the exact generation they
// committed even if package-global configuration changes concurrently.
func SaveSnapshot(cfg *Config) error {
	snapshot := Clone(cfg)
	stateMu.Lock()
	defer stateMu.Unlock()
	return saveSnapshotLocked(snapshot)
}

func saveSnapshotLocked(cfg *Config) error {
	if vp == nil {
		return fmt.Errorf("config not initialized")
	}
	if cfg == nil {
		cfg = &Config{}
	}
	// Update the viper config with current ins values
	vp.Set("repository", cfg.Repository)
	vp.Set("storage", cfg.Storage)
	vp.Set("githubToken", cfg.GitHubToken)
	vp.Set("cocurrencyNum", cfg.ConcurrencyNum)
	vp.Set("releaseSizeLimit", cfg.ReleaseSizeLimit)
	vp.Set("releaseNumLimit", cfg.ReleaseNumLimit)
	vp.Set("retryMaxCount", cfg.RetryMaxCount)
	vp.Set("retryBaseDelay", cfg.RetryBaseDelay)
	vp.Set("syncOverdueGrace", cfg.SyncOverdueGrace)
	vp.Set("syncStuckThreshold", cfg.SyncStuckThreshold)
	vp.Set("githubApiConcurrency", cfg.GitHubAPIConcurrency)
	vp.Set("githubMinRequestInterval", cfg.GitHubMinRequestInterval)
	vp.Set("githubLowRemainingThreshold", cfg.GitHubLowRemainingThreshold)
	vp.Set("githubScheduleJitter", cfg.GitHubScheduleJitter)
	return vp.WriteConfig()
}
