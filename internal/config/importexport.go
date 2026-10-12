package config

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/viper"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"gopkg.in/yaml.v3"
)

var readConfigFile = func(v *viper.Viper) error { return v.ReadInConfig() }

// ServerSection mirrors the `server:` section of config.yaml. It carries both
// yaml and mapstructure tags so it can be (de)serialized directly and read via
// viper's UnmarshalKey.
type ServerSection struct {
	Host               string `yaml:"host" mapstructure:"host"`
	Port               string `yaml:"port" mapstructure:"port"`
	AuthEnabled        bool   `yaml:"authEnabled" mapstructure:"authEnabled"`
	AuthToken          string `yaml:"authToken" mapstructure:"authToken"`
	DbPath             string `yaml:"dbPath" mapstructure:"dbPath"`
	LogRetentionMonths int    `yaml:"logRetentionMonths" mapstructure:"logRetentionMonths"`
}

const DefaultLogRetentionMonths = 2

// DurationString renders a time.Duration as its Go string form ("5s") in YAML
// and accepts either that form or integer nanoseconds on parse. This keeps
// exported config usable as config.yaml (viper parses "5s"); a bare
// time.Duration would serialize as nanoseconds.
type DurationString time.Duration

func (d DurationString) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

func (d *DurationString) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string or integer")
	}
	if node.Tag == "!!int" {
		n, err := strconv.ParseInt(node.Value, 10, 64)
		if err != nil {
			return err
		}
		*d = DurationString(n)
		return nil
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = DurationString(parsed)
	return nil
}

// ExportConfig is the full config document used for export/import: the Config
// fields plus the server section (which Config does not model).
type ExportConfig struct {
	Repository                  []typedef.Repository   `yaml:"repository"`
	Storage                     []typedef.MultiStorage `yaml:"storage"`
	GitHubToken                 string                 `yaml:"githubToken"`
	ConcurrencyNum              uint                   `yaml:"cocurrencyNum"`
	ReleaseSizeLimit            int                    `yaml:"releaseSizeLimit"`
	ReleaseNumLimit             int                    `yaml:"releaseNumLimit"`
	RetryMaxCount               int                    `yaml:"retryMaxCount"`
	RetryBaseDelay              DurationString         `yaml:"retryBaseDelay"`
	SyncOverdueGrace            DurationString         `yaml:"syncOverdueGrace"`
	SyncStuckThreshold          DurationString         `yaml:"syncStuckThreshold"`
	GitHubAPIConcurrency        uint                   `yaml:"githubApiConcurrency"`
	GitHubMinRequestInterval    DurationString         `yaml:"githubMinRequestInterval"`
	GitHubLowRemainingThreshold int                    `yaml:"githubLowRemainingThreshold"`
	GitHubScheduleJitter        DurationString         `yaml:"githubScheduleJitter"`
	Server                      ServerSection          `yaml:"server"`
}

// GetServerSection returns the `server:` section, applying defaults for unset
// fields. It reads from the viper instance that loaded the config file (see
// Init) so host/port/dbPath in config.yaml are honored; if Init has not run yet
// it falls back to the global viper singleton so callers can still override
// settings directly.
func GetServerSection() ServerSection {
	stateMu.Lock()
	defer stateMu.Unlock()
	v := vp
	if v == nil {
		v = viper.GetViper()
	}

	// Defaults — set before UnmarshalKey so missing keys fall back to them.
	v.SetDefault("server.port", "8080")
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.authEnabled", false)
	v.SetDefault("server.authToken", "")
	v.SetDefault("server.dbPath", "gitrieve.db")
	v.SetDefault("server.logRetentionMonths", DefaultLogRetentionMonths)

	// UnmarshalKey may omit nested defaults when a partial server section
	// exists in the file. Seed the target as well, so adding retention alone
	// cannot clear the listening address or database path.
	cfg := ServerSection{
		Host: "0.0.0.0", Port: "8080", DbPath: "gitrieve.db",
		LogRetentionMonths: DefaultLogRetentionMonths,
	}
	if err := v.UnmarshalKey("server", &cfg); err != nil {
		panic(err)
	}
	if cfg.LogRetentionMonths <= 0 {
		cfg.LogRetentionMonths = DefaultLogRetentionMonths
	}
	return cfg
}

// ExportFrom assembles the full config document (repository + storage +
// globals from cfg, server section from the loaded config file) and returns it
// as YAML usable directly as config.yaml.
func ExportFrom(cfg *Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("config not initialized")
	}
	doc := ExportConfig{
		Repository:                  cfg.Repository,
		Storage:                     cfg.Storage,
		GitHubToken:                 cfg.GitHubToken,
		ConcurrencyNum:              cfg.ConcurrencyNum,
		ReleaseSizeLimit:            cfg.ReleaseSizeLimit,
		ReleaseNumLimit:             cfg.ReleaseNumLimit,
		RetryMaxCount:               cfg.RetryMaxCount,
		RetryBaseDelay:              DurationString(cfg.RetryBaseDelay),
		SyncOverdueGrace:            DurationString(cfg.SyncOverdueGrace),
		SyncStuckThreshold:          DurationString(cfg.SyncStuckThreshold),
		GitHubAPIConcurrency:        cfg.GitHubAPIConcurrency,
		GitHubMinRequestInterval:    DurationString(cfg.GitHubMinRequestInterval),
		GitHubLowRemainingThreshold: cfg.GitHubLowRemainingThreshold,
		GitHubScheduleJitter:        DurationString(cfg.GitHubScheduleJitter),
		Server:                      GetServerSection(),
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to marshal config: %w", err)
	}
	return string(out), nil
}

// Export returns the config document for the package-global config instance.
func Export() (string, error) {
	return ExportFrom(GetIns())
}

// ReloadSnapshot is a validated, unpublished config-file generation. Its
// internals remain private so callers cannot mutate the config or viper state
// between the read and the unified publication boundary.
type ReloadSnapshot struct {
	config *Config
	viper  *viper.Viper
}

// Config returns a defensive copy of the unpublished disk generation.
func (s *ReloadSnapshot) Config() *Config {
	if s == nil {
		return nil
	}
	return Clone(s.config)
}

// ReadReloadSnapshot reads, unmarshals, defaults, and validates config.yaml
// without changing any package-global or runtime state.
func ReadReloadSnapshot() (*ReloadSnapshot, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	return readReloadSnapshotLocked()
}

func readReloadSnapshotLocked() (*ReloadSnapshot, error) {
	if vp == nil {
		return nil, fmt.Errorf("config not initialized")
	}
	// A fresh viper avoids inheriting override keys: Save()/SetServerField leave
	// vp.Set() overrides that ReadInConfig never clears, so Unmarshal would merge
	// the stale overrides on top of the freshly-read file.
	nv := viper.New()
	nv.SetConfigFile(Path)
	if err := readConfigFile(nv); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	var next Config
	if err := nv.Unmarshal(&next); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config file: %w", err)
	}
	seedDefaults(&next)
	if !nv.IsSet("githubScheduleJitter") {
		next.GitHubScheduleJitter = 30 * time.Second
	}
	if err := validateIdentity(&next); err != nil {
		return nil, err
	}
	return &ReloadSnapshot{config: Clone(&next), viper: nv}, nil
}

// PublishReloadSnapshot installs a previously validated disk generation
// without re-reading or writing config.yaml. Package config, GitHub policy, and
// install's immutable runtime state share the normal publication boundary.
func PublishReloadSnapshot(snapshot *ReloadSnapshot, install func(*Config)) *Config {
	if snapshot == nil {
		return nil
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	return publishSnapshotLocked(snapshot.config, snapshot.viper, install)
}

// Reload preserves the package-level behavior for non-server callers while
// holding stateMu across the complete disk-read-to-publication operation.
func Reload() error {
	stateMu.Lock()
	defer stateMu.Unlock()
	snapshot, err := readReloadSnapshotLocked()
	if err != nil {
		return err
	}
	publishSnapshotLocked(snapshot.config, snapshot.viper, nil)
	return nil
}

// seedExportDefaults fills zero-valued global options in an imported document
// with the same defaults Init applies (see seedDefaults), so an import that
// omits them behaves identically to a config file that omits them.
func seedExportDefaults(doc *ExportConfig, jitterPresent bool) {
	if doc.RetryMaxCount <= 0 {
		doc.RetryMaxCount = 3
	}
	if time.Duration(doc.RetryBaseDelay) <= 0 {
		doc.RetryBaseDelay = DurationString(5 * time.Second)
	}
	if time.Duration(doc.SyncOverdueGrace) <= 0 {
		doc.SyncOverdueGrace = DurationString(DefaultSyncOverdueGrace)
	}
	if time.Duration(doc.SyncStuckThreshold) <= 0 {
		doc.SyncStuckThreshold = DurationString(DefaultSyncStuckThreshold)
	}
	if doc.ConcurrencyNum == 0 {
		doc.ConcurrencyNum = 3
	}
	if doc.ReleaseNumLimit == 0 {
		doc.ReleaseNumLimit = 3
	}
	if doc.ReleaseSizeLimit == 0 {
		doc.ReleaseSizeLimit = 300000000
	}
	if doc.GitHubAPIConcurrency == 0 {
		doc.GitHubAPIConcurrency = 2
	}
	if time.Duration(doc.GitHubMinRequestInterval) <= 0 {
		doc.GitHubMinRequestInterval = DurationString(200 * time.Millisecond)
	}
	if doc.GitHubLowRemainingThreshold <= 0 {
		doc.GitHubLowRemainingThreshold = 100
	}
	if !jitterPresent || time.Duration(doc.GitHubScheduleJitter) < 0 {
		doc.GitHubScheduleJitter = DurationString(30 * time.Second)
	}
	// Startup settings require a restart, but an absent/partial section
	// must not force empty host/port/dbPath onto the config on apply.
	if doc.Server.Host == "" {
		doc.Server.Host = "0.0.0.0"
	}
	if doc.Server.Port == "" {
		doc.Server.Port = "8080"
	}
	if doc.Server.DbPath == "" {
		doc.Server.DbPath = "gitrieve.db"
	}
	if doc.Server.LogRetentionMonths <= 0 {
		doc.Server.LogRetentionMonths = DefaultLogRetentionMonths
	}
}

// ParseImport parses an imported YAML config document into its typed form. It
// seeds defaults for unset global options (mirroring Init), applies server
// defaults for a missing/partial server section, and synthesizes concrete URLs
// for user/org entries so their identity keys resolve. It does not validate
// identities — ValidateImport collects every violation for the caller.
func ParseImport(yamlStr string) (*ExportConfig, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(yamlStr), &root); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	var doc ExportConfig
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	for i := range doc.Repository {
		doc.Repository[i].URL = doc.Repository[i].EffectiveURL()
	}
	jitterPresent := false
	if len(root.Content) > 0 && len(root.Content[0].Content) > 0 {
		for i := 0; i+1 < len(root.Content[0].Content); i += 2 {
			if root.Content[0].Content[i].Value == "githubScheduleJitter" {
				jitterPresent = true
				break
			}
		}
	}
	seedExportDefaults(&doc, jitterPresent)
	return &doc, nil
}

// ValidateImport checks an imported document for the rules that apply before
// any import: every repository needs a usable identity (non-empty URL or
// orgName), no two imported repositories share a normalized URL, and storage
// names are non-empty and unique. Returns every violation so the caller can
// surface them all at once.
func ValidateImport(doc *ExportConfig) []string {
	var errs []string
	seen := map[string]string{}
	for _, repo := range doc.Repository {
		if repo.Key() == "" {
			errs = append(errs, fmt.Sprintf("repository %q (type %q) has an empty URL and no orgName", repo.Name, repo.GetType()))
			continue
		}
		if prev, ok := seen[repo.Key()]; ok {
			errs = append(errs, fmt.Sprintf("repository %q duplicates URL %q of repository %q", repo.Name, repo.Key(), prev))
		} else {
			seen[repo.Key()] = repo.Name
		}
	}
	seenStorage := map[string]bool{}
	for _, st := range doc.Storage {
		if st.Name == "" {
			errs = append(errs, "storage entry has an empty name")
			continue
		}
		if seenStorage[st.Name] {
			errs = append(errs, fmt.Sprintf("storage %q is duplicated", st.Name))
		}
		seenStorage[st.Name] = true
	}
	return errs
}

// SetServerField persists one `server:` field to the loaded viper instance so
// an imported server section is written to config.yaml. The running server
// re-reads retention on each cleanup pass; other fields require a restart.
// Returns an error when the config was never initialized.
func SetServerField(field string, value interface{}) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if vp == nil {
		return fmt.Errorf("config not initialized")
	}
	vp.Set("server."+field, value)
	return nil
}
