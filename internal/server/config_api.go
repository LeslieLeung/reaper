package server

import (
	"net/http"
	"reflect"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

// secretFields are config values never echoed in plaintext in a diff.
var secretFields = map[string]bool{
	"githubToken":     true,
	"authToken":       true,
	"secretAccessKey": true,
}

// maskSecret returns "***" for a non-empty secret value, the value otherwise.
func maskSecret(field string, v interface{}) interface{} {
	if s, ok := v.(string); ok && secretFields[field] && s != "" {
		return "***"
	}
	return v
}

var repoFields = []struct {
	name string
	get  func(typedef.Repository) interface{}
}{
	{"name", func(r typedef.Repository) interface{} { return r.Name }},
	{"url", func(r typedef.Repository) interface{} { return r.EffectiveURL() }},
	{"cron", func(r typedef.Repository) interface{} { return r.Cron }},
	{"storage", func(r typedef.Repository) interface{} { return r.Storage }},
	{"useCache", func(r typedef.Repository) interface{} { return r.UseCache }},
	{"type", func(r typedef.Repository) interface{} { return r.GetType() }},
	{"orgName", func(r typedef.Repository) interface{} { return r.OrgName }},
	{"allBranches", func(r typedef.Repository) interface{} { return r.AllBranches }},
	{"depth", func(r typedef.Repository) interface{} { return r.Depth }},
	{"downloadReleases", func(r typedef.Repository) interface{} { return r.DownloadReleases }},
	{"downloadIssues", func(r typedef.Repository) interface{} { return r.DownloadIssues }},
	{"downloadWiki", func(r typedef.Repository) interface{} { return r.DownloadWiki }},
	{"downloadDiscussion", func(r typedef.Repository) interface{} { return r.DownloadDiscussion }},
}

var storageFields = []struct {
	name string
	get  func(typedef.MultiStorage) interface{}
}{
	{"name", func(s typedef.MultiStorage) interface{} { return s.Name }},
	{"type", func(s typedef.MultiStorage) interface{} { return s.Type }},
	{"path", func(s typedef.MultiStorage) interface{} { return s.Path }},
	{"endpoint", func(s typedef.MultiStorage) interface{} { return s.Endpoint }},
	{"bucket", func(s typedef.MultiStorage) interface{} { return s.Bucket }},
	{"region", func(s typedef.MultiStorage) interface{} { return s.Region }},
	{"accessKeyID", func(s typedef.MultiStorage) interface{} { return s.AccessKeyID }},
	{"secretAccessKey", func(s typedef.MultiStorage) interface{} { return s.SecretAccessKey }},
}

// stringSetEq compares two string slices as unordered sets (used for the
// storage-backend references, where order is not meaningful).
func stringSetEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]bool, len(a))
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		if !m[s] {
			return false
		}
	}
	return true
}

func valuesEqual(a, b interface{}) bool {
	if as, ok := a.([]string); ok {
		bs, ok2 := b.([]string)
		if !ok2 {
			return false
		}
		return stringSetEq(as, bs)
	}
	return reflect.DeepEqual(a, b)
}

func repoFieldChanges(existing, imported typedef.Repository) []FieldChange {
	var out []FieldChange
	for _, f := range repoFields {
		a, b := f.get(existing), f.get(imported)
		if valuesEqual(a, b) {
			continue
		}
		out = append(out, FieldChange{Field: f.name, Existing: a, Imported: b})
	}
	return out
}

func storageFieldChanges(existing, imported typedef.MultiStorage) []FieldChange {
	var out []FieldChange
	for _, f := range storageFields {
		a, b := f.get(existing), f.get(imported)
		if valuesEqual(a, b) {
			continue
		}
		out = append(out, FieldChange{Field: f.name, Existing: maskSecret(f.name, a), Imported: maskSecret(f.name, b)})
	}
	return out
}

// diffRepositories classifies every imported repository as added / modified /
// absent (identical ones are omitted) and every current repository as deleted.
// Only a subset of a repository's fields drives "modified"; the classification
// is by identity key (normalized URL).
func diffRepositories(current, imported []typedef.Repository) RepoDiff {
	curByKey := map[string]typedef.Repository{}
	for _, r := range current {
		if r.Key() != "" {
			curByKey[r.Key()] = r
		}
	}
	impByKey := map[string]typedef.Repository{}
	for _, r := range imported {
		impByKey[r.Key()] = r
	}

	var diff RepoDiff
	for _, imp := range imported {
		entry := RepoEntry{Key: imp.Key(), Name: imp.Name, URL: imp.EffectiveURL()}
		cur, exists := curByKey[imp.Key()]
		if !exists {
			diff.Added = append(diff.Added, entry)
			continue
		}
		if changes := repoFieldChanges(cur, imp); len(changes) > 0 {
			entry.Changes = changes
			diff.Modified = append(diff.Modified, entry)
		}
	}
	keys := make([]string, 0, len(curByKey))
	for k := range curByKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := impByKey[k]; !ok {
			r := curByKey[k]
			diff.Deleted = append(diff.Deleted, RepoEntry{Key: r.Key(), Name: r.Name, URL: r.EffectiveURL()})
		}
	}
	return diff
}

func diffStorages(current, imported []typedef.MultiStorage) StorageDiff {
	curByName := map[string]typedef.MultiStorage{}
	for _, s := range current {
		curByName[s.Name] = s
	}
	impByName := map[string]typedef.MultiStorage{}
	for _, s := range imported {
		impByName[s.Name] = s
	}

	var diff StorageDiff
	for _, imp := range imported {
		entry := StorageEntry{Name: imp.Name, Type: imp.Type}
		cur, exists := curByName[imp.Name]
		if !exists {
			diff.Added = append(diff.Added, entry)
			continue
		}
		if changes := storageFieldChanges(cur, imp); len(changes) > 0 {
			entry.Changes = changes
			diff.Modified = append(diff.Modified, entry)
		}
	}
	names := make([]string, 0, len(curByName))
	for n := range curByName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, ok := impByName[n]; !ok {
			s := curByName[n]
			diff.Deleted = append(diff.Deleted, StorageEntry{Name: s.Name, Type: s.Type})
		}
	}
	return diff
}

// diffGlobals compares the current config's global options with the imported
// document's, reporting only changed fields.
func diffGlobals(cur *config.Config, imp *config.ExportConfig) []FieldChange {
	pairs := []struct {
		field string
		cur   interface{}
		imp   interface{}
	}{
		{"githubToken", cur.GitHubToken, imp.GitHubToken},
		{"cocurrencyNum", cur.ConcurrencyNum, imp.ConcurrencyNum},
		{"releaseSizeLimit", cur.ReleaseSizeLimit, imp.ReleaseSizeLimit},
		{"releaseNumLimit", cur.ReleaseNumLimit, imp.ReleaseNumLimit},
		{"retryMaxCount", cur.RetryMaxCount, imp.RetryMaxCount},
		{"retryBaseDelay", cur.RetryBaseDelay.String(), time.Duration(imp.RetryBaseDelay).String()},
		{"syncOverdueGrace", cur.SyncOverdueGrace.String(), time.Duration(imp.SyncOverdueGrace).String()},
		{"syncStuckThreshold", cur.SyncStuckThreshold.String(), time.Duration(imp.SyncStuckThreshold).String()},
		{"githubApiConcurrency", cur.GitHubAPIConcurrency, imp.GitHubAPIConcurrency},
		{"githubMinRequestInterval", cur.GitHubMinRequestInterval.String(), time.Duration(imp.GitHubMinRequestInterval).String()},
		{"githubLowRemainingThreshold", cur.GitHubLowRemainingThreshold, imp.GitHubLowRemainingThreshold},
		{"githubScheduleJitter", cur.GitHubScheduleJitter.String(), time.Duration(imp.GitHubScheduleJitter).String()},
	}
	var out []FieldChange
	for _, p := range pairs {
		if reflect.DeepEqual(p.cur, p.imp) {
			continue
		}
		out = append(out, FieldChange{Field: p.field, Existing: maskSecret(p.field, p.cur), Imported: maskSecret(p.field, p.imp)})
	}
	return out
}

func diffServer(cur, imp config.ServerSection) []FieldChange {
	pairs := []struct {
		field string
		cur   interface{}
		imp   interface{}
	}{
		{"host", cur.Host, imp.Host},
		{"port", cur.Port, imp.Port},
		{"authEnabled", cur.AuthEnabled, imp.AuthEnabled},
		{"authToken", cur.AuthToken, imp.AuthToken},
		{"dbPath", cur.DbPath, imp.DbPath},
		{"logRetentionMonths", cur.LogRetentionMonths, imp.LogRetentionMonths},
	}
	var out []FieldChange
	for _, p := range pairs {
		if reflect.DeepEqual(p.cur, p.imp) {
			continue
		}
		out = append(out, FieldChange{Field: p.field, Existing: maskSecret(p.field, p.cur), Imported: maskSecret(p.field, p.imp)})
	}
	return out
}

// buildWarnings explains the operational consequences of the changes being
// previewed. Startup settings require a restart, and changing
// authEnabled/authToken can lock the operator out of the web UI.
func buildWarnings(serverChanges []FieldChange) []string {
	if len(serverChanges) == 0 {
		return nil
	}
	for _, c := range serverChanges {
		if c.Field == "authEnabled" || c.Field == "authToken" {
			return []string{"server.authEnabled / server.authToken 已变更：该改动不会热生效，需重启 server；若忘记令牌，重启后可能无法访问 Web UI。"}
		}
	}
	for _, c := range serverChanges {
		if c.Field != "logRetentionMonths" {
			return []string{"server 段已变更：监听地址、认证和数据库路径改动需重启 server 后生效；日志保留月数在下次清理时生效。"}
		}
	}
	return []string{"日志保留月数已变更：下次自动清理时生效。"}
}

// ExportConfig returns the full current config as YAML, usable directly as
// config.yaml (secrets in plaintext — the export is the source of truth).
func (a *API) ExportConfig(c *gin.Context) {
	yamlStr, err := config.ExportFrom(a.configSnapshot())
	if err != nil {
		c.JSON(http.StatusInternalServerError, Response{Code: 500, Message: "导出配置失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, Response{Code: 200, Data: gin.H{"yaml": yamlStr}})
}

// PreviewImport parses and validates the submitted config and computes a diff
// against the current config. It never mutates state.
func (a *API) PreviewImport(c *gin.Context) {
	var req ImportPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: "Invalid request: " + err.Error()})
		return
	}
	doc, err := config.ParseImport(req.Config)
	if err != nil {
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: err.Error()})
		return
	}
	if errs := config.ValidateImport(doc); len(errs) > 0 {
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: "导入配置无效", Data: ImportErrorData{Errors: errs}})
		return
	}

	curServer := config.GetServerSection()
	current := a.configSnapshot()
	repos := diffRepositories(current.Repository, doc.Repository)
	storages := diffStorages(current.Storage, doc.Storage)
	globals := diffGlobals(current, doc)
	serverChanges := diffServer(curServer, doc.Server)

	data := ImportPreviewData{
		Summary: ImportSummary{
			Repositories: CountSummary{Added: len(repos.Added), Deleted: len(repos.Deleted), Modified: len(repos.Modified)},
			Storages:     CountSummary{Added: len(storages.Added), Deleted: len(storages.Deleted), Modified: len(storages.Modified)},
			Globals:      ChangedCount{Changed: len(globals)},
			Server:       ChangedCount{Changed: len(serverChanges)},
		},
		Repositories: repos,
		Storages:     storages,
		Globals:      globals,
		Server:       serverChanges,
		Warnings:     buildWarnings(serverChanges),
	}
	c.JSON(http.StatusOK, Response{Code: 200, Data: data})
}

// ApplyImport applies a previously-previewed import. Choices select, per entry,
// whether the imported or the existing value wins; entries without a choice use
// the documented defaults (added/modified/globals/server -> imported, deleted ->
// keep). Builds a complete replacement config, then publishes, persists, and
// refreshes schedules as one serialized generation. The server section is
// written to viper; log retention takes effect on the next cleanup pass,
// while listening, authentication and database settings require a restart.
func (a *API) ApplyImport(c *gin.Context) {
	var req ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: "Invalid request: " + err.Error()})
		return
	}
	doc, err := config.ParseImport(req.Config)
	if err != nil {
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: err.Error()})
		return
	}
	if errs := config.ValidateImport(doc); len(errs) > 0 {
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: "导入配置无效", Data: ImportErrorData{Errors: errs}})
		return
	}

	a.configMu.Lock()
	result, next := a.applyImportLocked(doc, &req)
	_, msg := a.publishPersistAndRefreshConfigLocked(next, "配置已应用（内存）但未能持久化: ")
	a.configMu.Unlock()
	c.JSON(http.StatusOK, Response{Code: 200, Data: result, Message: msg})
}

func (a *API) applyImportLocked(doc *config.ExportConfig, req *ImportRequest) (ImportResult, *config.Config) {
	var result ImportResult
	// Build a NEW config instance instead of mutating a.config in place: job
	// goroutines read a.config / ins / e.cfg.Load() concurrently, and atomic
	// pointer swaps only protect the pointer, not the contents. A shallow copy
	// suffices — the merge below reassigns whole slice headers into freshly
	// built kept/keptSt arrays and writes scalar globals, never mutating shared
	// backing arrays or nested objects. The new instance is published once it is
	// fully built (see the publish block before return).
	current := config.Clone(a.config)
	next := *current
	choice := func(m map[string]string, key string, def string) string {
		if v, ok := m[key]; ok {
			return v
		}
		return def
	}

	// Recompute the classification so choices apply only to the classes the
	// user actually saw in the preview (modified / deleted). This reads the
	// current config before any merge.
	repoDiff := diffRepositories(current.Repository, doc.Repository)
	modifiedRepos := map[string]bool{}
	deletedRepos := map[string]bool{}
	for _, e := range repoDiff.Modified {
		modifiedRepos[e.Key] = true
	}
	for _, e := range repoDiff.Deleted {
		deletedRepos[e.Key] = true
	}
	stDiff := diffStorages(current.Storage, doc.Storage)
	modifiedStorages := map[string]bool{}
	deletedStorages := map[string]bool{}
	for _, e := range stDiff.Modified {
		modifiedStorages[e.Name] = true
	}
	for _, e := range stDiff.Deleted {
		deletedStorages[e.Name] = true
	}

	deleteRepo := map[string]bool{}
	for _, k := range req.Choices.RepositoryDeletions {
		deleteRepo[k] = true
	}
	deleteStorage := map[string]bool{}
	for _, n := range req.Choices.StorageDeletions {
		deleteStorage[n] = true
	}

	// ---- repositories: drop deleted, then merge imported ----
	kept := make([]typedef.Repository, 0, len(next.Repository))
	for _, r := range next.Repository {
		if deletedRepos[r.Key()] && deleteRepo[r.Key()] {
			result.RepositoriesDeleted++
			continue
		}
		kept = append(kept, r)
	}
	idxByKey := map[string]int{}
	for i, r := range kept {
		if r.Key() != "" {
			idxByKey[r.Key()] = i
		}
	}
	for _, imp := range doc.Repository {
		imp.URL = imp.EffectiveURL() // 用户/组织条目以合成 URL 落库，保证身份
		i, ok := idxByKey[imp.Key()]
		if !ok {
			kept = append(kept, imp)
			idxByKey[imp.Key()] = len(kept) - 1
			result.RepositoriesAdded++
			continue
		}
		if !modifiedRepos[imp.Key()] {
			continue // unchanged: existing wins silently
		}
		if choice(req.Choices.RepositoryChoices, imp.Key(), "imported") == "imported" {
			kept[i] = imp
			result.RepositoriesUpdated++
		}
	}
	next.Repository = kept

	// ---- storages: same pattern by name ----
	keptSt := make([]typedef.MultiStorage, 0, len(next.Storage))
	for _, st := range next.Storage {
		if deletedStorages[st.Name] && deleteStorage[st.Name] {
			result.StoragesDeleted++
			continue
		}
		keptSt = append(keptSt, st)
	}
	idxByName := map[string]int{}
	for i, st := range keptSt {
		idxByName[st.Name] = i
	}
	for _, imp := range doc.Storage {
		i, ok := idxByName[imp.Name]
		if !ok {
			keptSt = append(keptSt, imp)
			idxByName[imp.Name] = len(keptSt) - 1
			result.StoragesAdded++
			continue
		}
		if !modifiedStorages[imp.Name] {
			continue
		}
		if choice(req.Choices.StorageChoices, imp.Name, "imported") == "imported" {
			keptSt[i] = imp
			result.StoragesUpdated++
		}
	}
	next.Storage = keptSt

	// ---- globals ----
	if choice(req.Choices.GlobalChoices, "githubToken", "imported") == "imported" && next.GitHubToken != doc.GitHubToken {
		next.GitHubToken = doc.GitHubToken
		result.GlobalsUpdated++
	}
	if choice(req.Choices.GlobalChoices, "cocurrencyNum", "imported") == "imported" && next.ConcurrencyNum != doc.ConcurrencyNum {
		next.ConcurrencyNum = doc.ConcurrencyNum
		result.GlobalsUpdated++
	}
	if choice(req.Choices.GlobalChoices, "releaseSizeLimit", "imported") == "imported" && next.ReleaseSizeLimit != doc.ReleaseSizeLimit {
		next.ReleaseSizeLimit = doc.ReleaseSizeLimit
		result.GlobalsUpdated++
	}
	if choice(req.Choices.GlobalChoices, "releaseNumLimit", "imported") == "imported" && next.ReleaseNumLimit != doc.ReleaseNumLimit {
		next.ReleaseNumLimit = doc.ReleaseNumLimit
		result.GlobalsUpdated++
	}
	if choice(req.Choices.GlobalChoices, "retryMaxCount", "imported") == "imported" && next.RetryMaxCount != doc.RetryMaxCount {
		next.RetryMaxCount = doc.RetryMaxCount
		result.GlobalsUpdated++
	}
	if choice(req.Choices.GlobalChoices, "retryBaseDelay", "imported") == "imported" {
		d := time.Duration(doc.RetryBaseDelay)
		if next.RetryBaseDelay != d {
			next.RetryBaseDelay = d
			result.GlobalsUpdated++
		}
	}
	if choice(req.Choices.GlobalChoices, "syncOverdueGrace", "imported") == "imported" {
		d := time.Duration(doc.SyncOverdueGrace)
		if next.SyncOverdueGrace != d {
			next.SyncOverdueGrace = d
			result.GlobalsUpdated++
		}
	}
	if choice(req.Choices.GlobalChoices, "syncStuckThreshold", "imported") == "imported" {
		d := time.Duration(doc.SyncStuckThreshold)
		if next.SyncStuckThreshold != d {
			next.SyncStuckThreshold = d
			result.GlobalsUpdated++
		}
	}
	if choice(req.Choices.GlobalChoices, "githubApiConcurrency", "imported") == "imported" && next.GitHubAPIConcurrency != doc.GitHubAPIConcurrency {
		next.GitHubAPIConcurrency = doc.GitHubAPIConcurrency
		result.GlobalsUpdated++
	}
	if choice(req.Choices.GlobalChoices, "githubMinRequestInterval", "imported") == "imported" {
		d := time.Duration(doc.GitHubMinRequestInterval)
		if next.GitHubMinRequestInterval != d {
			next.GitHubMinRequestInterval = d
			result.GlobalsUpdated++
		}
	}
	if choice(req.Choices.GlobalChoices, "githubLowRemainingThreshold", "imported") == "imported" && next.GitHubLowRemainingThreshold != doc.GitHubLowRemainingThreshold {
		next.GitHubLowRemainingThreshold = doc.GitHubLowRemainingThreshold
		result.GlobalsUpdated++
	}
	if choice(req.Choices.GlobalChoices, "githubScheduleJitter", "imported") == "imported" {
		d := time.Duration(doc.GitHubScheduleJitter)
		if next.GitHubScheduleJitter != d {
			next.GitHubScheduleJitter = d
			result.GlobalsUpdated++
		}
	}

	// ---- server section: persist chosen fields ----
	curServer := config.GetServerSection()
	if choice(req.Choices.ServerChoices, "host", "imported") == "imported" && curServer.Host != doc.Server.Host {
		_ = config.SetServerField("host", doc.Server.Host)
		result.ServerUpdated++
	}
	if choice(req.Choices.ServerChoices, "port", "imported") == "imported" && curServer.Port != doc.Server.Port {
		_ = config.SetServerField("port", doc.Server.Port)
		result.ServerUpdated++
	}
	if choice(req.Choices.ServerChoices, "authEnabled", "imported") == "imported" && curServer.AuthEnabled != doc.Server.AuthEnabled {
		_ = config.SetServerField("authEnabled", doc.Server.AuthEnabled)
		result.ServerUpdated++
	}
	if choice(req.Choices.ServerChoices, "authToken", "imported") == "imported" && curServer.AuthToken != doc.Server.AuthToken {
		_ = config.SetServerField("authToken", doc.Server.AuthToken)
		result.ServerUpdated++
	}
	if choice(req.Choices.ServerChoices, "dbPath", "imported") == "imported" && curServer.DbPath != doc.Server.DbPath {
		_ = config.SetServerField("dbPath", doc.Server.DbPath)
		result.ServerUpdated++
	}
	if choice(req.Choices.ServerChoices, "logRetentionMonths", "imported") == "imported" && curServer.LogRetentionMonths != doc.Server.LogRetentionMonths {
		_ = config.SetServerField("logRetentionMonths", doc.Server.LogRetentionMonths)
		result.ServerUpdated++
	}

	// The caller publishes this fully-built replacement only after every
	// selected section has been applied.
	return result, &next
}

// ReloadConfig re-reads config.yaml from disk, repoints the API and executor at
// the fresh config instance, and replaces the running server's cron schedules.
// Startup server settings require a restart; retention is read on each cleanup pass.
func (a *API) ReloadConfig(c *gin.Context) {
	a.configMu.Lock()
	snapshot, err := a.readReloadSnapshot()
	if err != nil {
		a.configMu.Unlock()
		c.JSON(http.StatusBadRequest, Response{Code: 400, Message: "重载配置失败: " + err.Error()})
		return
	}
	published := a.publishReloadConfigLocked(snapshot)
	message := a.refreshSchedules(published)
	a.configMu.Unlock()
	c.JSON(http.StatusOK, Response{Code: 200, Data: gin.H{}, Message: message})
}
