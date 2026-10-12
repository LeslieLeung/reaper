package config

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"gopkg.in/yaml.v3"
)

var readConfigFile = func(v *viper.Viper) error {
	content, err := os.ReadFile(v.ConfigFileUsed())
	if err != nil {
		return err
	}
	if _, err := parseConfigYAML(content); err != nil {
		return err
	}
	// Decode exactly the validated bytes; re-reading the file could admit a
	// different generation. Generic decoder diagnostics can contain credentials.
	if err := v.ReadConfig(bytes.NewReader(content)); err != nil {
		return fmt.Errorf("invalid configuration field structure")
	}
	return nil
}

// parseConfigYAML checks credential shapes without formatting source values.
// Aliases are supported; unexpected decoder failures are also sanitized by the
// callers, so merge keys or other YAML constructs cannot expose raw secrets.
func parseConfigYAML(content []byte) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("invalid YAML syntax")
	}
	if len(root.Content) == 0 {
		return &root, nil
	}
	document := resolveYAMLNode(root.Content[0])
	if document == nil || document.Kind != yaml.MappingNode {
		if document != nil && document.Tag == "!!null" {
			return &root, nil
		}
		return nil, fmt.Errorf("configuration must be a mapping")
	}
	for i := 0; i+1 < len(document.Content); i += 2 {
		if !strings.EqualFold(document.Content[i].Value, "githubAccounts") {
			continue
		}
		pool := resolveYAMLNode(document.Content[i+1])
		if pool != nil && pool.Tag == "!!null" {
			continue
		}
		if pool == nil || pool.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("githubAccounts must be a list (line %d)", document.Content[i+1].Line)
		}
		for index, item := range pool.Content {
			account := resolveYAMLNode(item)
			if account == nil || account.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("githubAccounts entry %d must be a mapping (line %d)", index+1, item.Line)
			}
			for field := 0; field+1 < len(account.Content); field += 2 {
				key := account.Content[field]
				if key.Value != "name" && key.Value != "token" {
					continue
				}
				value := resolveYAMLNode(account.Content[field+1])
				if value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
					return nil, fmt.Errorf("githubAccounts entry %d %s must be a string (line %d)", index+1, key.Value, account.Content[field+1].Line)
				}
			}
		}
	}
	return &root, nil
}

func resolveYAMLNode(node *yaml.Node) *yaml.Node {
	seen := map[*yaml.Node]bool{}
	for node != nil && node.Kind == yaml.AliasNode {
		if seen[node] {
			return nil
		}
		seen[node] = true
		node = node.Alias
	}
	return node
}

// ServerSection mirrors the `server:` section of config.yaml. It carries both
// yaml and mapstructure tags so it can be (de)serialized directly and read via
// viper's UnmarshalKey.
type ServerSection struct {
	Host        string `yaml:"host" mapstructure:"host"`
	Port        string `yaml:"port" mapstructure:"port"`
	AuthEnabled bool   `yaml:"authEnabled" mapstructure:"authEnabled"`
	AuthToken   string `yaml:"authToken" mapstructure:"authToken"`
	DbPath      string `yaml:"dbPath" mapstructure:"dbPath"`
}

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
	GitHubAccounts              []githubapi.Account    `yaml:"githubAccounts"`
	GitHubAccountConcurrency    uint                   `yaml:"githubAccountConcurrency"`
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

	var cfg ServerSection
	if err := v.UnmarshalKey("server", &cfg); err != nil {
		panic(err)
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
		GitHubAccounts:              cfg.GitHubAccounts,
		GitHubAccountConcurrency:    cfg.GitHubAccountConcurrency,
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
		return nil, fmt.Errorf("failed to unmarshal config file: check configuration field types")
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
	if doc.GitHubAccountConcurrency == 0 {
		doc.GitHubAccountConcurrency = 2
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
	// The server section is never hot-applied, but an absent/partial section
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
}

// ParseImport parses an imported YAML config document into its typed form. It
// seeds defaults for unset global options (mirroring Init), applies server
// defaults for a missing/partial server section, and synthesizes concrete URLs
// for user/org entries so their identity keys resolve. It does not validate
// identities — ValidateImport collects every violation for the caller.
func ParseImport(yamlStr string) (*ExportConfig, error) {
	root, err := parseConfigYAML([]byte(yamlStr))
	if err != nil {
		return nil, err
	}
	var doc ExportConfig
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		return nil, fmt.Errorf("invalid YAML field types or duplicate fields")
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
// names are non-empty and unique, and GitHub accounts have nonblank credentials
// and unique names. Returns every violation without including credential values.
func ValidateImport(doc *ExportConfig) []string {
	errs := validateGitHubAccounts(doc.GitHubAccounts)
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

func validateGitHubAccounts(accounts []githubapi.Account) []string {
	var errs []string
	seenNames := make(map[string]bool, len(accounts))
	for i, account := range accounts {
		name := strings.TrimSpace(account.Name)
		if name == "" {
			errs = append(errs, fmt.Sprintf("githubAccounts entry %d has a blank name", i+1))
		} else if seenNames[name] {
			errs = append(errs, fmt.Sprintf("githubAccounts entry %d has a duplicate name", i+1))
		}
		seenNames[name] = true
		if strings.TrimSpace(account.Token) == "" {
			errs = append(errs, fmt.Sprintf("githubAccounts entry %d has a blank token", i+1))
		}
	}
	return errs
}

// SetServerField persists one `server:` field to the loaded viper instance so
// an imported server section is written to config.yaml. The running server
// never re-reads the server section, so this only takes effect after a restart.
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
