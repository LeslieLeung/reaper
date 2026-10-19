const $ = (sel, root) => (root || document).querySelector(sel);
const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));
const { normalizePage, paginationHTML } = window.GitrievePagination;

function esc(value) {
    return String(value === null || value === undefined ? '' : value).replace(/[&<>"']/g, c => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));
}

// repoKey 返回仓库身份键。后端以规范化 URL 为唯一身份，前端不做规范化。
const repoKey = (r) => r.URL || '';

function fmtTime(value) {
    if (!value) return '-';
    const d = new Date(value);
    if (isNaN(d.getTime())) return esc(value);
    return d.toLocaleString();
}

function fmtDurationSeconds(value) {
    if (value === null || value === undefined || !Number.isFinite(value)) return '-';
    let seconds = Math.max(0, Math.floor(value));
    // Match uptime: fixed 30-day months and 12-month (360-day) years.
    const units = [[31104000, 'y'], [2592000, 'mo'], [86400, 'd'], [3600, 'h'], [60, 'm'], [1, 's']];
    const parts = [];
    for (const [size, suffix] of units) {
        const count = Math.floor(seconds / size);
        if (count > 0 || parts.length > 0 || size === 1) parts.push(count + suffix);
        seconds %= size;
    }
    return parts.join('');
}

function fmtDuration(start, end) {
    if (!start) return '-';
    const s = new Date(start);
    if (isNaN(s.getTime())) return '-';
    const e = end ? new Date(end) : new Date();
    if (isNaN(e.getTime())) return '-';
    let sec = Math.max(0, Math.floor((e - s) / 1000));
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const r = sec % 60;
    if (h > 0) return h + 'h ' + m + 'm';
    if (m > 0) return m + 'm ' + r + 's';
    return r + 's';
}

async function api(path, opts) {
    opts = opts || {};
    const resp = await fetch(path, Object.assign({}, opts, {
        headers: Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {})
    }));
    let body = null;
    try { body = await resp.json(); } catch (e) { /* ignore */ }
    if (!resp.ok || (body && typeof body.code === 'number' && body.code >= 400)) {
        const err = new Error((body && body.message) || ('HTTP ' + resp.status));
        err.status = resp.status;
        err.data = body ? body.data : null;
        if (body && body.data && Array.isArray(body.data.errors)) err.errors = body.data.errors;
        throw err;
    }
    return body ? body.data : null;
}

let maintenanceState = null;
let maintenanceBusy = false;
let maintenanceTimer = null;
let maintenanceEpoch = 0;

function displayMaintenance(value) {
    maintenanceState = value;
    clearTimeout(maintenanceTimer);
    maintenanceTimer = null;
    const status = $('#maintenance-status');
    const button = $('#btn-maintenance');
    if (!status || !button) return;
    button.textContent = value.enabled ? '退出维护模式' : '进入维护模式';
    button.disabled = maintenanceBusy || value.shutting_down || (value.enabled && !value.ready);
    if (!value.enabled) {
        status.textContent = '正常运行 · 升级前可进入维护模式';
    } else if (value.error) {
        status.textContent = '维护收尾状态保存失败：' + value.error + '。请检查任务日志和数据库，尚不能确认安全升级。';
        if (!value.drained) maintenanceTimer = setTimeout(refreshMaintenance, 2000);
    } else if (value.ready) {
        status.textContent = value.shutting_down ? '任务已收尾，服务正在停止。' :
            '已进入维护，可以安全升级。重启后恢复正常调度，不重跑旧任务。';
    } else {
        status.textContent = `正在收尾：运行 ${value.running || 0}，排队收尾 ${value.pending || 0}，提交中 ${value.preparing || 0}。等待当前页或文件及归档写入完成。`;
        maintenanceTimer = setTimeout(refreshMaintenance, 2000);
    }
}

async function refreshMaintenance() {
    if (maintenanceBusy) return;
    const epoch = ++maintenanceEpoch;
    try {
        const value = await api('/api/maintenance');
        if (epoch === maintenanceEpoch) displayMaintenance(value);
    } catch (e) {
        if (epoch !== maintenanceEpoch) return;
        maintenanceState = null;
        clearTimeout(maintenanceTimer);
        const status = $('#maintenance-status');
        if (status) status.textContent = '无法确认维护状态：' + e.message + '。请刷新状态后再升级。';
        const button = $('#btn-maintenance');
        if (button) button.disabled = true;
    }
}

async function toggleMaintenance() {
    if (maintenanceBusy || !maintenanceState || maintenanceState.shutting_down ||
        (maintenanceState.enabled && !maintenanceState.ready)) return;
    maintenanceBusy = true;
    ++maintenanceEpoch;
    clearTimeout(maintenanceTimer);
    $('#btn-maintenance').disabled = true;
    try {
        const value = await api('/api/maintenance', {method: maintenanceState.enabled ? 'DELETE' : 'POST'});
        maintenanceBusy = false;
        displayMaintenance(value);
    } catch (e) {
        maintenanceBusy = false;
        toast('维护操作失败：' + e.message, true);
        await refreshMaintenance();
    }
}

function optionsCell(r) {
    const parts = [];
    if (r.UseCache) parts.push('cache');
    if (r.AllBranches) parts.push('allBranches');
    if (r.DownloadReleases) parts.push('releases');
    if (r.DownloadIssues) parts.push('issues');
    if (r.DownloadWiki) parts.push('wiki');
    if (r.DownloadDiscussion) parts.push('discussion');
    return parts.length ? esc(parts.join(' ')) : '-';
}

const state = {
    jobsPage: 1,
    jobsStatus: '',
    jobsRepo: '',
    reposPage: 1,
    reposSearch: '',
    reposHealth: '',
    reposOverdue: false,
    reposSort: 'attention',
    reposDirection: 'asc',
    reposRenderSerial: 0,
    componentDetailSerial: 0,
    activeRoute: '',
    routeEpoch: 0,
    es: null,
    logJob: null,
    logIds: {}
};

function toast(message, isError) {
    const el = $('#toast');
    el.textContent = message;
    el.className = 'toast ' + (isError ? 'error' : '');
    clearTimeout(el._t);
    el._t = setTimeout(() => { el.className = 'toast hidden'; }, 4000);
}

function setActiveNav(route) {
    $$('.nav-links a').forEach(a => a.classList.toggle('active', a.dataset.route === route));
}

function routeFromHash() {
    const hash = (location.hash || '#/jobs').replace(/^#\/?/, '');
    return (hash.split('?')[0].split('/')[0]) || 'jobs';
}

function renderApp() {
    const hash = (location.hash || '#/jobs').replace(/^#\/?/, '');
    const route = routeFromHash();
    const routeEpoch = ++state.routeEpoch;
    state.activeRoute = route;
    state.componentDetailSerial++;
    setActiveNav(route);
    if (route === 'repositories') {
        applyRepositoryRoute(hash);
        renderRepositories(routeEpoch);
    }
    else {
        state.reposRenderSerial++;
        if (route === 'storage') renderStorage();
        else if (route === 'config') renderConfig();
        else renderJobs();
    }
}

window.addEventListener('hashchange', renderApp);

const repositoryHealthValues = ['healthy', 'failed', 'overdue', 'stuck', 'never_synced', 'cancelled', 'pending', 'running', 'syncing'];
const repositorySortValues = ['attention', 'name', 'last_attempt', 'last_success', 'next_run'];

function isActiveRepositoryRoute(routeEpoch) {
    if (routeEpoch !== state.routeEpoch) return false;
    return state.activeRoute === 'repositories' && routeFromHash() === 'repositories';
}

function applyRepositoryRoute(hash) {
    const query = (hash || '').split('?')[1] || '';
    const p = new URLSearchParams(query);
    const page = parseInt(p.get('page') || '1', 10);
    const health = p.get('health') || '';
    const sort = p.get('sort') || 'attention';
    const direction = p.get('direction') || 'asc';
    state.reposPage = Number.isFinite(page) && page > 0 ? page : 1;
    state.reposSearch = p.get('search') || '';
    state.reposHealth = repositoryHealthValues.includes(health) ? health : '';
    state.reposOverdue = p.get('overdue') === 'true';
    state.reposSort = repositorySortValues.includes(sort) ? sort : 'attention';
    state.reposDirection = direction === 'desc' ? 'desc' : 'asc';
}

function repositoryParams() {
    const p = new URLSearchParams({ page: state.reposPage, limit: 20 });
    if (state.reposSearch) p.set('search', state.reposSearch);
    if (state.reposHealth) p.set('health', state.reposHealth);
    if (state.reposOverdue) p.set('overdue', 'true');
    p.set('sort', state.reposSort || 'attention');
    p.set('direction', state.reposDirection || 'asc');
    return p;
}

function setRepositoryRoute(next) {
    Object.assign(state, next);
    const p = repositoryParams();
    p.delete('limit');
    const nextHash = '#/repositories?' + p.toString();
    if (location.hash === nextHash) renderRepositories();
    else location.hash = nextHash;
}

async function refreshMetrics() {
    const dot = $('#server-status');
    try {
        const data = await api('/api/metrics');
        dot.className = 'status-dot ok';
        $('#metrics').textContent = 'uptime ' + (data.uptime || '');
    } catch (e) {
        dot.className = 'status-dot err';
        $('#metrics').textContent = 'offline';
    }
}

function statusBadge(status) {
    const cls = ['pending', 'running', 'completed', 'failed', 'cancelled'].includes(status) ? status : 'pending';
    return '<span class="badge ' + cls + '">' + esc(status) + '</span>';
}

function jobsToolbar(jobCount) {
    return `
        <div class="panel">
            <div class="toolbar">
                <div class="toolbar-group">
                    <input type="text" id="jobs-repo-filter" placeholder="Filter by repository name or URL…" value="${esc(state.jobsRepo)}">
                    <button id="btn-search-jobs" class="btn">Search</button>
                </div>
                <div class="toolbar-group">
                    <select id="jobs-status">
                        <option value="">All statuses</option>
                        <option value="pending">Pending</option>
                        <option value="running">Running</option>
                        <option value="completed">Completed</option>
                        <option value="failed">Failed</option>
                        <option value="cancelled">Cancelled</option>
                    </select>
                    <button id="btn-refresh" class="btn">Refresh</button>
                </div>
                <div class="toolbar-group toolbar-count">${jobCount} job(s)</div>
            </div>
        </div>`;
}

function jobsTable(jobs, page, limit, total) {
    if (!jobs.length) {
        return '<div class="empty">No jobs yet. Run a repository from the <strong>Repositories</strong> page, or click <strong>Refresh</strong>.</div>';
    }
    const rows = jobs.map(j => `
        <tr>
            <td>${statusBadge(j.status)}</td>
            <td><strong>${esc(j.name)}</strong></td>
            <td class="muted">${esc(j.url || '-')}</td>
            <td>${fmtTime(j.start_time)}</td>
            <td>${fmtTime(j.end_time)}</td>
            <td>${fmtDuration(j.start_time, j.end_time)}</td>
            <td class="err-cell" title="${esc(j.error_message)}">${esc(j.error_message || '-')}</td>
            <td class="actions">
                <button class="btn btn-sm btn-log" data-jobid="${esc(j.id)}" data-jobname="${esc(j.name)}">Logs</button>
                ${(j.status === 'running' || j.status === 'pending') ?
                    '<button class="btn btn-sm btn-danger btn-cancel" data-jobid="' + esc(j.id) + '">Cancel</button>' : ''}
            </td>
        </tr>`).join('');

    const pages = Math.max(1, Math.ceil(total / limit));
    return `
        <div class="table-wrap">
            <table class="table">
                <thead>
                    <tr>
                        <th>Status</th><th>Job</th><th>Repository</th>
                        <th>Started</th><th>Finished</th><th>Duration</th>
                        <th>Message</th><th></th>
                    </tr>
                </thead>
                <tbody>${rows}</tbody>
            </table>
        </div>
        ${paginationHTML(page, pages, total, 'jobs', true)}`;
}

async function renderJobs() {
    $('#app').innerHTML = '<div class="loading">Loading jobs\u2026</div>';

    const params = new URLSearchParams({ page: state.jobsPage, limit: 20 });
    if (state.jobsStatus) params.set('status', state.jobsStatus);
    if (state.jobsRepo) params.set('repository', state.jobsRepo);

    let jobs = [], total = 0;
    try {
        const data = await api('/api/jobs?' + params.toString());
        jobs = (data && data.jobs) || [];
        total = (data && data.total) || 0;
    } catch (e) {
        $('#app').innerHTML = '<div class="empty error-text">Failed to load jobs: ' + esc(e.message) + '</div>';
        return;
    }

    const pages = Math.max(1, Math.ceil(total / 20));
    const validPage = normalizePage(state.jobsPage, pages);
    if (state.jobsPage !== validPage) {
        state.jobsPage = validPage;
        return renderJobs();
    }

    $('#app').innerHTML = jobsToolbar(total) + jobsTable(jobs, state.jobsPage, 20, total);

    $('#btn-refresh').addEventListener('click', () => renderJobs());
    $('#jobs-status').value = state.jobsStatus;
    $('#jobs-status').addEventListener('change', (ev) => {
        state.jobsStatus = ev.target.value;
        state.jobsPage = 1;
        renderJobs();
    });

    // Query only on explicit action (Search button or Enter), never on every
    // keystroke, so the input keeps focus while typing.
    const applyJobFilter = () => {
        state.jobsRepo = $('#jobs-repo-filter').value.trim();
        state.jobsPage = 1;
        renderJobs();
    };
    $('#btn-search-jobs').addEventListener('click', applyJobFilter);
    $('#jobs-repo-filter').addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter') {
            ev.preventDefault();
            applyJobFilter();
        }
    });

    const prev = $('#pg-prev-jobs');
    const next = $('#pg-next-jobs');
    if (prev) prev.addEventListener('click', () => { if (state.jobsPage > 1) { state.jobsPage--; renderJobs(); } });
    if (next) next.addEventListener('click', () => { if (state.jobsPage < pages) { state.jobsPage++; renderJobs(); } });
    $$('.pg-page').forEach(button => button.addEventListener('click', () => {
        const page = Number(button.dataset.page);
        if (Number.isInteger(page) && page >= 1 && page <= pages && page !== state.jobsPage) {
            state.jobsPage = page;
            renderJobs();
        }
    }));

    $$('.btn-log').forEach(b => b.addEventListener('click', () => openLogModal(b.dataset.jobid, b.dataset.jobname)));
    $$('.btn-cancel').forEach(b => b.addEventListener('click', () => cancelJob(b.dataset.jobid)));
}

async function cancelJob(jobId) {
    if (!confirm('Cancel this job?')) return;
    try {
        await api('/api/jobs/' + encodeURIComponent(jobId), { method: 'DELETE' });
        toast('Job cancelled');
        renderJobs();
    } catch (e) {
        toast('Failed to cancel job: ' + e.message, true);
    }
}

async function runRepo(key, name, btn) {
    if (!key) return;
    const routeEpoch = state.routeEpoch;
    btn.disabled = true;
    try {
        const data = await api('/api/jobs', { method: 'POST', body: JSON.stringify({ repository_key: key }) });
        if (!isActiveRepositoryRoute(routeEpoch)) return;
        const jobIDs = (data && data.job_ids) || [];
        if (jobIDs.length === 1) {
            toast('Job started (' + jobIDs[0].slice(0, 8) + '…)');
            openLogModal(jobIDs[0], name);
        } else {
            toast('Started ' + jobIDs.length + ' jobs (org expansion)');
        }
        renderRepositories(routeEpoch);
    } catch (e) {
        if (!isActiveRepositoryRoute(routeEpoch)) return;
        toast('Failed to start job: ' + e.message, true);
        btn.disabled = false;
    }
}

function openLogModal(jobId, jobName, showComponents) {
    if (state.es) state.es.close();
    if (!showComponents) state.componentDetailSerial++;
    state.logJob = jobId;
    state.logIds = {};
    $('#log-modal-title').textContent = 'Logs: ' + jobName;
    $('#log-modal-state').textContent = '';
    const consoleEl = $('#log-console');
    consoleEl.innerHTML = '<div class="log-line muted">Waiting for logs\u2026</div>';
    if (!showComponents) {
        $('#execution-details').classList.add('hidden');
        $('#component-details').classList.add('hidden');
    }
    $('#log-modal').classList.remove('hidden');

    const es = new EventSource('/api/jobs/' + encodeURIComponent(jobId) + '/logs');
    state.es = es;

    es.addEventListener('done', (ev) => {
        if (state.es !== es) return;
        let status = '';
        try { status = (JSON.parse(ev.data) || {}).status || ''; } catch (e) { /* ignore */ }
        appendLogLine({ level: 'info', message: 'Job finished (status: ' + status + ')' });
        $('#log-modal-state').textContent = 'finished';
        es.close();
        if (state.es === es) state.es = null;
    });

    es.onmessage = (ev) => {
        if (state.es !== es) return;
        let entry = null;
        try { entry = JSON.parse(ev.data); } catch (e) { return; }
        if (entry && entry.id && state.logIds[entry.id]) return;
        if (entry && entry.id) state.logIds[entry.id] = true;
        appendLogLine(entry);
    };

    es.onerror = () => { /* EventSource auto-reconnects; dedupe + done event handle the rest */ };
}

function componentBadge(status) {
    const value = String(status || 'pending');
    const cls = ['pending', 'running', 'completed', 'failed', 'skipped', 'cancelled'].includes(value) ? value : 'pending';
    return '<span class="badge component-' + cls + '">' + esc(value) + '</span>';
}

function renderComponentDetails(components) {
    const list = $('#component-list');
    if (!components.length) {
        list.textContent = 'Component details unavailable';
        return;
    }
    list.innerHTML = components.map(component => `
        <div class="component-row">
            <strong>${esc(component.component)}</strong>
            ${componentBadge(component.status)}
            <span class="muted">${esc(fmtDuration(component.start_time, component.end_time))}</span>
            <span class="component-error">${esc(component.error_message || '')}</span>
        </div>`).join('');
}

function repositoryCanRetry(repository) {
    const status = repository.last_status || '';
    return !repository.stuck && !['pending', 'running'].includes(status) &&
        (status === 'failed' || status === 'cancelled' || repository.overdue);
}

function renderExecutionDetails(repository, detailSerial, routeEpoch) {
    $('#execution-details').classList.remove('hidden');
    $('#execution-status').textContent = repository.last_status || repository.health_status || 'unknown';
    $('#execution-attempt').textContent = fmtTime(repository.last_attempt_time);
    $('#execution-duration').textContent = fmtDurationSeconds(repository.last_duration_seconds);
    $('#execution-error').textContent = repository.last_error_message || '-';
    const retry = $('#btn-retry-execution');
    retry.onclick = null;
    retry.disabled = false;
    retry.classList.toggle('hidden', !repositoryCanRetry(repository));
    if (repositoryCanRetry(repository)) {
        retry.onclick = () => retryExecutionRepository(repository, detailSerial, routeEpoch);
    }
}

async function retryExecutionRepository(repository, detailSerial, routeEpoch) {
    if (!isActiveRepositoryRoute(routeEpoch) || detailSerial !== state.componentDetailSerial) return;
    const retry = $('#btn-retry-execution');
    retry.disabled = true;
    try {
        const data = await api('/api/jobs', {
            method: 'POST',
            body: JSON.stringify({ repository_key: repoKey(repository) })
        });
        if (!isActiveRepositoryRoute(routeEpoch) || detailSerial !== state.componentDetailSerial) return;
        const jobIDs = (data && data.job_ids) || [];
        if (jobIDs.length === 1) {
            toast('Repository retry started');
            renderRepositories(routeEpoch);
            openLogModal(jobIDs[0], repository.Name);
        } else {
            closeLogModal();
            if (jobIDs.length > 1) toast('Started ' + jobIDs.length + ' jobs (repository expansion)');
            else toast('Repository retry accepted; no execution log was returned');
            renderRepositories(routeEpoch);
        }
    } catch (e) {
        if (isActiveRepositoryRoute(routeEpoch) && detailSerial === state.componentDetailSerial) {
            toast('Repository retry failed: ' + e.message, true);
            retry.disabled = false;
        }
    }
}

async function openExecutionDetails(executionID, name, repository) {
    const routeEpoch = state.routeEpoch;
    if (!isActiveRepositoryRoute(routeEpoch)) return;
    const detailSerial = ++state.componentDetailSerial;
    const snapshot = Object.freeze(Object.assign({}, repository));
    renderExecutionDetails(snapshot, detailSerial, routeEpoch);
    $('#component-details').classList.remove('hidden');
    $('#component-list').textContent = 'Loading component details…';
    let components = [];
    try {
        const data = await api('/api/jobs/' + encodeURIComponent(executionID) + '/components');
        components = (data && data.components) || [];
    } catch (e) {
        components = null;
    }
    if (detailSerial !== state.componentDetailSerial || !isActiveRepositoryRoute(routeEpoch)) return;
    if (components === null) $('#component-list').textContent = 'Component details unavailable';
    else renderComponentDetails(components);
    openLogModal(executionID, name, true);
}

function appendLogLine(entry) {
    if (!entry) return;
    const consoleEl = $('#log-console');
    const level = ['info', 'error', 'warn', 'debug'].includes(entry.level) ? entry.level : 'info';
    const div = document.createElement('div');
    div.className = 'log-line ' + level;
    const ts = entry.timestamp ? new Date(entry.timestamp).toLocaleTimeString() : '';
    div.textContent = (ts ? '[' + ts + '] ' : '') + (entry.message || '');
    consoleEl.appendChild(div);
    while (consoleEl.childNodes.length > 1000) consoleEl.removeChild(consoleEl.firstChild);
    consoleEl.scrollTop = consoleEl.scrollHeight;
}

async function closeLogModal() {
    if (state.es) { state.es.close(); state.es = null; }
    state.componentDetailSerial++;
    $('#log-modal').classList.add('hidden');
}

function openRepoForm(repo) {
    $('#repo-modal-title').textContent = repo ? 'Edit Repository' : 'Add Repository';
    $('#repo-original-key').value = repo ? repoKey(repo) : '';
    $('#repo-name').value = repo ? repo.Name : '';
    $('#repo-name').disabled = false; // name 仅展示/查询，可重复可改名
    $('#repo-url').value = repo ? (repo.URL || '') : '';
    $('#repo-type').value = repo && repo.Type ? repo.Type : 'repo';
    $('#repo-org').value = repo ? (repo.OrgName || '') : '';
    $('#repo-cron').value = repo ? (repo.Cron || '') : '';
    $('#repo-depth').value = repo ? (repo.Depth || 0) : 0;
    $('#repo-uses').checked = !!(repo && repo.UseCache);
    $('#repo-allbranches').checked = !!(repo && repo.AllBranches);
    $('#repo-releases').checked = !!(repo && repo.DownloadReleases);
    $('#repo-issues').checked = !!(repo && repo.DownloadIssues);
    $('#repo-wiki').checked = !!(repo && repo.DownloadWiki);
    $('#repo-discussion').checked = !!(repo && repo.DownloadDiscussion);

    const selected = new Set(repo ? (repo.Storage || []) : []);
    const box = $('#repo-storage');
    api('/api/storage').then(storages => {
        const names = (storages || []).map(s => s.Name);
        if (!names.length) {
            box.innerHTML = '<span class="muted">No storage backends configured.</span>';
            return;
        }
        box.innerHTML = names.map(n => `
            <label class="checkbox">
                <input type="checkbox" value="${esc(n)}" ${selected.has(n) ? 'checked' : ''}> ${esc(n)}
            </label>`).join('');
    }).catch(() => { box.innerHTML = '<span class="muted">Failed to load storages.</span>'; });

    $('#repo-modal').classList.remove('hidden');
}

async function saveRepo(ev) {
    ev.preventDefault();
    const routeEpoch = state.routeEpoch;
    const originalKey = $('#repo-original-key').value;
    const name = $('#repo-name').value.trim();
    if (!name) { toast('Name is required', true); return; }

    const storage = $$('#repo-storage input:checked').map(i => i.value);
    const repo = {
        Name: name,
        URL: $('#repo-url').value.trim(),
        Type: $('#repo-type').value,
        OrgName: $('#repo-org').value.trim(),
        Cron: $('#repo-cron').value.trim(),
        Storage: storage,
        UseCache: $('#repo-uses').checked,
        AllBranches: $('#repo-allbranches').checked,
        Depth: parseInt($('#repo-depth').value, 10) || 0,
        DownloadReleases: $('#repo-releases').checked,
        DownloadIssues: $('#repo-issues').checked,
        DownloadWiki: $('#repo-wiki').checked,
        DownloadDiscussion: $('#repo-discussion').checked
    };

    try {
        if (originalKey) {
            await api('/api/repositories/' + encodeURIComponent(originalKey), { method: 'PUT', body: JSON.stringify(repo) });
        } else {
            await api('/api/repositories', { method: 'POST', body: JSON.stringify(repo) });
        }
        if (!isActiveRepositoryRoute(routeEpoch)) return;
        toast(originalKey ? 'Repository updated' : 'Repository added');
        $('#repo-modal').classList.add('hidden');
        renderRepositories(routeEpoch);
    } catch (e) {
        if (!isActiveRepositoryRoute(routeEpoch)) return;
        toast('Failed to save repository: ' + e.message, true);
    }
}

async function deleteRepo(key) {
    if (!confirm('Delete repository?')) return;
    const routeEpoch = state.routeEpoch;
    try {
        await api('/api/repositories/' + encodeURIComponent(key), { method: 'DELETE' });
        if (!isActiveRepositoryRoute(routeEpoch)) return;
        toast('Repository deleted');
        renderRepositories(routeEpoch);
    } catch (e) {
        if (!isActiveRepositoryRoute(routeEpoch)) return;
        toast('Failed to delete repository: ' + e.message, true);
    }
}

function repositoryHealthBadge(repo) {
    const value = String(repo.health_status || 'never_synced');
    const cls = repositoryHealthValues.includes(value) ? value : 'never_synced';
    const healthBadge = '<span class="badge health-' + cls + '">' + esc(value) + '</span>';
    if (value === 'stuck' && ['running', 'pending'].includes(repo.last_status)) {
        return '<span class="badge health-' + repo.last_status + '">' + esc(repo.last_status) + '</span> ' + healthBadge;
    }
    return healthBadge;
}

function repositorySummaryCards(summary) {
    const s = summary || {};
    const cards = [
        { label: 'All', count: s.total || 0, health: '' },
        { label: 'Healthy', count: s.healthy || 0, health: 'healthy' },
        { label: 'Failed', count: s.failed || 0, health: 'failed' },
        { label: 'Syncing', count: (s.pending || 0) + (s.running || 0), health: 'syncing' },
        { label: 'Never synced', count: s.never_synced || 0, health: 'never_synced' },
        { label: 'Overdue', count: s.overdue || 0, overdue: true }
    ];
    return cards.map(card =>
        '<button type="button" class="summary-card" data-health="' + esc(card.health || '') +
        '" data-overdue="' + (card.overdue ? 'true' : '') + '"><strong>' +
        esc(card.count) + '</strong><span>' + esc(card.label) + '</span></button>'
    ).join('');
}

function repositoryBulkSelector() {
    const selector = { search: state.reposSearch, health: state.reposHealth };
    if (state.reposOverdue) selector.overdue = true;
    return selector;
}

function selectorsEqual(left, right) {
    return left.search === right.search && left.health === right.health &&
        Boolean(left.overdue) === Boolean(right.overdue);
}

function freezeBulkRetryAttempt(selector, expectedCount, routeEpoch) {
    const frozenSelector = { search: selector.search || '', health: selector.health || '' };
    if (selector.overdue) frozenSelector.overdue = true;
    return Object.freeze({
        selector: Object.freeze(frozenSelector),
        expectedCount: Number(expectedCount) || 0,
        routeEpoch: routeEpoch
    });
}

function bulkRetryAttemptIsCurrent(attempt) {
    return isActiveRepositoryRoute(attempt.routeEpoch) &&
        selectorsEqual(attempt.selector, repositoryBulkSelector());
}

function canRetryFilteredRepositories() {
    return state.reposHealth === 'failed' || state.reposHealth === 'cancelled' ||
        state.reposHealth === 'overdue' || state.reposOverdue;
}

async function retryFilteredRepositories(attempt, retryAfterConflict) {
    if (!attempt.expectedCount) {
        toast('No eligible repositories to retry');
        return;
    }
    if (!bulkRetryAttemptIsCurrent(attempt)) {
        toast('Repository filters changed. Retry from the current view.', true);
        return;
    }
    const wording = retryAfterConflict ? 'The eligible set changed. Retry ' + attempt.expectedCount + ' repositories?' :
        'Retry ' + attempt.expectedCount + ' eligible repositories?';
    if (!confirm(wording)) return;
    if (!bulkRetryAttemptIsCurrent(attempt)) {
        toast('Repository filters changed. Retry from the current view.', true);
        return;
    }
    try {
        const data = await api('/api/jobs/bulk', {
            method: 'POST',
            body: JSON.stringify({ selector: attempt.selector, expected_count: attempt.expectedCount })
        });
        if (!bulkRetryAttemptIsCurrent(attempt)) return;
        const result = data || {};
        toast('Bulk retry: requested ' + (result.requested || 0) + ', queued ' + (result.queued || 0) +
            ', skipped active ' + (result.skipped_active || 0) + ', no longer eligible ' +
            (result.no_longer_eligible || 0) + ', failed to enqueue ' + (result.failed_to_enqueue || 0));
        renderRepositories(attempt.routeEpoch);
    } catch (e) {
        const actual = e && e.data ? e.data.actual_count : null;
        if (e.status === 409 && Number.isInteger(actual) && !retryAfterConflict) {
            if (!bulkRetryAttemptIsCurrent(attempt)) {
                toast('Repository filters changed. Retry from the current view.', true);
                return;
            }
            retryFilteredRepositories(freezeBulkRetryAttempt(attempt.selector, actual, attempt.routeEpoch), true);
            return;
        }
        if (bulkRetryAttemptIsCurrent(attempt)) toast('Bulk retry failed: ' + e.message, true);
    }
}

async function renderRepositories(expectedRouteEpoch) {
    const routeEpoch = expectedRouteEpoch === undefined ? state.routeEpoch : expectedRouteEpoch;
    if (!isActiveRepositoryRoute(routeEpoch)) return;
    const renderSerial = ++state.reposRenderSerial;
    if (!isActiveRepositoryRoute(routeEpoch)) return;
    $('#app').innerHTML = '<div class="loading">Loading repositories\u2026</div>';
    let data = null;
    try {
        data = await api('/api/repositories?' + repositoryParams().toString());
    } catch (e) {
        if (renderSerial !== state.reposRenderSerial || !isActiveRepositoryRoute(routeEpoch)) return;
        $('#app').innerHTML = '<div class="empty error-text">Failed to load repositories: ' + esc(e.message) + '</div>';
        return;
    }
    if (renderSerial !== state.reposRenderSerial || !isActiveRepositoryRoute(routeEpoch)) return;
    const repos = (data && data.repositories) || [];
    const total = (data && data.total) || 0;
    const pages = Math.max(1, Math.ceil(total / 20));
    const validPage = normalizePage(state.reposPage, pages);
    if (state.reposPage !== validPage) {
        setRepositoryRoute({ reposPage: validPage });
        return;
    }
    const summary = (data && data.summary) || {};
    const rows = repos.map(r => {
        const nextRun = r.schedule_error ? 'Schedule error: ' + r.schedule_error : fmtTime(r.next_run_time);
        const detail = r.latest_execution_id
            ? '<button class="btn btn-sm btn-execution-details" data-key="' + esc(repoKey(r)) + '">Details</button>' : '';
        return `<tr>
            <td class="actions">${detail}
                <button class="btn btn-sm btn-primary btn-run-repo" data-key="${esc(repoKey(r))}">Execute</button>
                <button class="btn btn-sm btn-edit-repo" data-key="${esc(repoKey(r))}">Edit</button>
                <button class="btn btn-sm btn-danger btn-del-repo" data-key="${esc(repoKey(r))}">Delete</button>
            </td>
            <td>${repositoryHealthBadge(r)}</td>
            <td><strong>${esc(r.Name)}</strong><br><span class="muted">${esc(r.URL || '-')}</span></td>
            <td>${fmtTime(r.last_attempt_time)}</td>
            <td>${fmtTime(r.last_success_time)}</td>
            <td>${esc(fmtDurationSeconds(r.last_duration_seconds))}</td>
            <td class="muted" title="${esc(r.schedule_error || '')}">${esc(nextRun)}</td>
            <td class="err-cell" title="${esc(r.last_error_message || '')}">${esc(r.last_error_message || '-')}</td>
        </tr>`;
    }).join('');

    $('#app').innerHTML = `
        <div class="page-header"><h2>Repositories</h2>
            <div class="toolbar-group"><button id="btn-add-repo" class="btn btn-primary">Add Repository</button>
                <button id="btn-refresh-repos" class="btn">Refresh</button></div>
        </div>
        <div class="summary-grid">${repositorySummaryCards(summary)}</div>
        <div class="panel repository-toolbar"><div class="toolbar">
            <div class="toolbar-group"><input type="text" id="repos-search" placeholder="Filter by name or URL\u2026" value="${esc(state.reposSearch)}">
                <button id="btn-search-repos" class="btn">Search</button></div>
            <div class="toolbar-group"><label>Health <select id="repos-health">
                <option value="">All health</option><option value="healthy">Healthy</option><option value="failed">Failed</option>
                <option value="cancelled">Cancelled</option><option value="overdue">Overdue</option><option value="stuck">Stuck</option>
                <option value="never_synced">Never synced</option><option value="pending">Pending</option>
                <option value="running">Running</option><option value="syncing">Syncing</option></select></label>
                <label class="checkbox"><input type="checkbox" id="repos-overdue"> Overdue only</label></div>
            <div class="toolbar-group"><label>Sort <select id="repos-sort"><option value="attention">Attention</option>
                <option value="name">Name</option><option value="last_attempt">Last attempt</option><option value="last_success">Last success</option><option value="next_run">Next run</option></select></label>
                <label>Direction <select id="repos-direction"><option value="asc">Ascending</option><option value="desc">Descending</option></select></label>
                ${canRetryFilteredRepositories() ? '<button id="btn-retry-filtered" class="btn btn-danger" ' + (total ? '' : 'disabled') + '>Retry filtered (' + esc(total) + ')</button>' : ''}
            </div>
        </div></div>
        <div class="panel repository-table">
            ${repos.length ? '<div class="table-wrap"><table class="table"><thead><tr><th>Actions</th><th>Status</th><th>Name / URL</th><th>Last attempt</th><th>Last success</th><th>Duration</th><th>Next run</th><th>Error summary</th></tr></thead><tbody>' + rows + '</tbody></table></div>' :
                '<div class="empty">No repositories match this view.</div>'}
            ${repos.length ? paginationHTML(state.reposPage, pages, total, 'repos', true) : ''}
        </div>`;

    $('#repos-health').value = state.reposHealth;
    $('#repos-overdue').checked = state.reposOverdue;
    $('#repos-sort').value = state.reposSort;
    $('#repos-direction').value = state.reposDirection;
    $('#btn-add-repo').addEventListener('click', () => openRepoForm(null));
    $('#btn-refresh-repos').addEventListener('click', () => renderRepositories());
    const applyRepositoryFilters = () => setRepositoryRoute({
        reposSearch: $('#repos-search').value.trim(), reposHealth: $('#repos-health').value,
        reposOverdue: $('#repos-overdue').checked, reposSort: $('#repos-sort').value,
        reposDirection: $('#repos-direction').value, reposPage: 1
    });
    $('#btn-search-repos').addEventListener('click', applyRepositoryFilters);
    $('#repos-search').addEventListener('keydown', ev => { if (ev.key === 'Enter') { ev.preventDefault(); applyRepositoryFilters(); } });
    $('#repos-health').addEventListener('change', applyRepositoryFilters);
    $('#repos-overdue').addEventListener('change', applyRepositoryFilters);
    $('#repos-sort').addEventListener('change', applyRepositoryFilters);
    $('#repos-direction').addEventListener('change', applyRepositoryFilters);
    $$('.summary-card').forEach(button => button.addEventListener('click', () => setRepositoryRoute({
        reposHealth: button.dataset.health || '', reposOverdue: button.dataset.overdue === 'true', reposPage: 1
    })));
    const retry = $('#btn-retry-filtered');
    if (retry) {
        const retryAttempt = freezeBulkRetryAttempt(repositoryBulkSelector(), total, routeEpoch);
        retry.addEventListener('click', () => retryFilteredRepositories(retryAttempt, false));
    }
    const prev = $('#pg-prev-repos');
    const next = $('#pg-next-repos');
    if (prev) prev.addEventListener('click', () => { if (state.reposPage > 1) setRepositoryRoute({ reposPage: state.reposPage - 1 }); });
    if (next) next.addEventListener('click', () => { if (state.reposPage < pages) setRepositoryRoute({ reposPage: state.reposPage + 1 }); });
    $$('.pg-page').forEach(button => button.addEventListener('click', () => {
        const page = Number(button.dataset.page);
        if (Number.isInteger(page) && page >= 1 && page <= pages && page !== state.reposPage) {
            setRepositoryRoute({ reposPage: page });
        }
    }));
    $$('.btn-run-repo').forEach(b => b.addEventListener('click', () => {
        const r = repos.find(x => repoKey(x) === b.dataset.key);
        runRepo(b.dataset.key, r ? r.Name : '', b);
    }));
    $$('.btn-edit-repo').forEach(b => b.addEventListener('click', () => {
        const r = repos.find(x => repoKey(x) === b.dataset.key);
        if (r) openRepoForm(r);
    }));
    $$('.btn-del-repo').forEach(b => b.addEventListener('click', () => deleteRepo(b.dataset.key)));
    $$('.btn-execution-details').forEach(b => b.addEventListener('click', () => {
        const r = repos.find(x => repoKey(x) === b.dataset.key);
        if (r) openExecutionDetails(r.latest_execution_id, r.Name, r);
    }));
}

function openStorageForm(storage) {
    $('#storage-modal-title').textContent = storage ? 'Edit Storage' : 'Add Storage';
    $('#storage-original-name').value = storage ? storage.Name : '';
    $('#storage-name').value = storage ? storage.Name : '';
    $('#storage-name').disabled = !!storage;
    $('#storage-path').value = storage ? (storage.Path || '') : '';
    $('#storage-modal').classList.remove('hidden');
}

async function saveStorage(ev) {
    ev.preventDefault();
    const original = $('#storage-original-name').value;
    const name = $('#storage-name').value.trim();
    if (!name) { toast('Name is required', true); return; }

    const storage = {
        Name: name,
        Type: 'file',
        Path: $('#storage-path').value.trim()
    };

    try {
        if (original) {
            await api('/api/storage/' + encodeURIComponent(original), { method: 'PUT', body: JSON.stringify(storage) });
            toast('Storage updated');
        } else {
            await api('/api/storage', { method: 'POST', body: JSON.stringify(storage) });
            toast('Storage added');
        }
        $('#storage-modal').classList.add('hidden');
        renderStorage();
    } catch (e) {
        toast('Failed to save storage: ' + e.message, true);
    }
}

async function deleteStorage(name) {
    if (!confirm('Delete storage "' + name + '"?')) return;
    try {
        await api('/api/storage/' + encodeURIComponent(name), { method: 'DELETE' });
        toast('Storage deleted');
        renderStorage();
    } catch (e) {
        toast('Failed to delete storage: ' + e.message, true);
    }
}

async function renderStorage() {
    $('#app').innerHTML = '<div class="loading">Loading storage\u2026</div>';
    let storages = [];
    try {
        storages = (await api('/api/storage')) || [];
    } catch (e) {
        $('#app').innerHTML = '<div class="empty error-text">Failed to load storage: ' + esc(e.message) + '</div>';
        return;
    }

    const rows = storages.map(s => `
        <tr>
            <td><strong>${esc(s.Name)}</strong></td>
            <td>${esc(s.Type)}</td>
            <td class="muted">${esc(s.Path || '-')}</td>
            <td class="actions">
                <button class="btn btn-sm btn-edit-storage" data-name="${esc(s.Name)}">Edit</button>
                <button class="btn btn-sm btn-danger btn-del-storage" data-name="${esc(s.Name)}">Delete</button>
            </td>
        </tr>`).join('');

    $('#app').innerHTML = `
        <div class="page-header">
            <h2>Storage</h2>
            <button id="btn-add-storage" class="btn btn-primary">Add Storage</button>
        </div>
        <div class="panel">
            ${storages.length
                ? '<div class="table-wrap"><table class="table"><thead><tr><th>Name</th><th>Type</th><th>Path</th><th></th></tr></thead><tbody>' + rows + '</tbody></table></div>'
                : '<div class="empty">No storage backends configured. Click <strong>Add Storage</strong>.</div>'}
        </div>`;

    $('#btn-add-storage').addEventListener('click', () => openStorageForm(null));
    $$('.btn-edit-storage').forEach(b => b.addEventListener('click', () => {
        const s = storages.find(x => x.Name === b.dataset.name);
        if (s) openStorageForm(s);
    }));
    $$('.btn-del-storage').forEach(b => b.addEventListener('click', () => deleteStorage(b.dataset.name)));
}

/* ---------------- Config: export / import / reload ---------------- */

async function renderConfig() {
    $('#app').innerHTML = `
        <div class="page-header"><h2>Configuration</h2></div>
        <div class="panel"><div class="panel-pad">
            <h3>导出配置</h3>
            <textarea id="config-export" class="config-textarea" readonly placeholder="点击“刷新”加载当前配置…"></textarea>
            <div class="toolbar-group" style="margin-top:8px;">
                <button id="btn-config-copy" class="btn btn-sm">复制</button>
                <button id="btn-config-download" class="btn btn-sm">下载 config.yaml</button>
                <button id="btn-config-export-refresh" class="btn btn-sm">刷新</button>
            </div>
        </div></div>
        <div class="panel"><div class="panel-pad">
            <h3>导入配置（合并）</h3>
            <textarea id="config-import" class="config-textarea" placeholder="粘贴 config.yaml 内容，或选择文件…"></textarea>
            <div class="toolbar-group" style="margin-top:8px;">
                <button id="btn-config-file" class="btn btn-sm">选择文件</button>
                <input type="file" id="config-file" accept=".yaml,.yml,.txt" hidden>
                <button id="btn-config-preview" class="btn btn-sm btn-primary">预览差异</button>
            </div>
            <div id="config-preview"></div>
        </div></div>
        <div class="panel"><div class="panel-pad">
            <h3>配置操作</h3>
            <button id="btn-config-reload" class="btn">刷新配置（从磁盘重载 config.yaml）</button>
            <p class="muted" style="margin-top:8px;">server 段的监听地址、认证和数据库路径改动需重启 server 生效；logRetentionMonths（日志保留月数）在下次清理时生效。daemon 排程需重启 daemon。修改 authEnabled/authToken 后若忘记令牌，可能无法访问本界面。</p>
        </div></div>`;

    loadExport();
    $('#btn-config-copy').addEventListener('click', copyExport);
    $('#btn-config-download').addEventListener('click', downloadExport);
    $('#btn-config-export-refresh').addEventListener('click', loadExport);
    $('#btn-config-file').addEventListener('click', () => $('#config-file').click());
    $('#config-file').addEventListener('change', (ev) => {
        const f = ev.target.files[0];
        if (!f) return;
        const reader = new FileReader();
        reader.onload = () => { $('#config-import').value = reader.result; };
        reader.readAsText(f);
    });
    $('#btn-config-preview').addEventListener('click', previewImport);
    $('#btn-config-reload').addEventListener('click', reloadConfig);
}

async function loadExport() {
    try {
        const data = await api('/api/config/export');
        $('#config-export').value = (data && data.yaml) || '';
    } catch (e) {
        toast('导出配置失败: ' + e.message, true);
    }
}

function copyExport() {
    const text = $('#config-export').value;
    if (!text) return;
    navigator.clipboard.writeText(text).then(
        () => toast('配置已复制'),
        () => toast('复制失败', true));
}

function downloadExport() {
    const text = $('#config-export').value;
    if (!text) return;
    const blob = new Blob([text], { type: 'text/yaml' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'config.yaml';
    a.click();
    URL.revokeObjectURL(a.href);
}

async function previewImport() {
    const config = $('#config-import').value.trim();
    if (!config) { toast('请粘贴或选择配置内容', true); return; }
    try {
        const data = await api('/api/config/import/preview', { method: 'POST', body: JSON.stringify({ config }) });
        renderPreview(data);
    } catch (e) {
        if (e.errors && e.errors.length) toast('导入配置无效：' + e.errors.join('；'), true);
        else toast(e.message, true);
        $('#config-preview').innerHTML = '';
    }
}

function fmt(v) {
    if (v === null || v === undefined) return '';
    if (typeof v === 'boolean') return v ? 'true' : 'false';
    if (Array.isArray(v)) return v.join(', ') || '(空)';
    return String(v);
}

function entryLabel(e) {
    return '<strong>' + esc(e.name) + '</strong>' +
        (e.url ? ' <span class="muted">' + esc(e.url) + '</span>' : '');
}

function changeCell(changes) {
    if (!changes || !changes.length) return '<span class="muted">—</span>';
    return changes.map(c => `<code>${esc(c.field)}</code>: ${esc(fmt(c.existing))} → ${esc(fmt(c.imported))}`).join('<br>');
}

function chip(key, label, n) {
    if (!n) return '';
    return `<button type="button" class="btn btn-sm diff-chip" data-diff="${key}">${label} ${n}</button>`;
}

function renderPreview(data) {
    const p = $('#config-preview');
    const s = data.summary || {};
    const warns = (data.warnings || []).map(w => `<div class="alert warn">${esc(w)}</div>`).join('');
    const r = s.repositories || {}, st = s.storages || {}, g = s.globals || {}, sv = s.server || {};

    p.innerHTML = warns + `
        <div class="diff-summary">
            ${chip('repos-added', '仓库 新增', r.added)}
            ${chip('repos-deleted', '仓库 删除', r.deleted)}
            ${chip('repos-modified', '仓库 修改', r.modified)}
            ${chip('storages-added', '存储 新增', st.added)}
            ${chip('storages-deleted', '存储 删除', st.deleted)}
            ${chip('storages-modified', '存储 修改', st.modified)}
            ${chip('globals', '全局项 变更', g.changed)}
            ${chip('server', 'server 段 变更', sv.changed)}
        </div>
        <div id="diff-repos-added" class="diff-section hidden"></div>
        <div id="diff-repos-deleted" class="diff-section hidden"></div>
        <div id="diff-repos-modified" class="diff-section hidden"></div>
        <div id="diff-storages-added" class="diff-section hidden"></div>
        <div id="diff-storages-deleted" class="diff-section hidden"></div>
        <div id="diff-storages-modified" class="diff-section hidden"></div>
        <div id="diff-globals" class="diff-section hidden"></div>
        <div id="diff-server" class="diff-section hidden"></div>
        <div class="toolbar-group" style="margin-top:12px;">
            <button id="btn-config-apply" class="btn btn-primary">应用导入</button>
        </div>`;

    renderAdded('#diff-repos-added', data.repositories.added);
    renderDeleted('#diff-repos-deleted', data.repositories.deleted, 'repo');
    renderModified('#diff-repos-modified', data.repositories.modified, 'repo');
    renderAdded('#diff-storages-added', data.storages.added);
    renderDeleted('#diff-storages-deleted', data.storages.deleted, 'storage');
    renderModified('#diff-storages-modified', data.storages.modified, 'storage');
    renderFieldChoices('#diff-globals', data.globals, 'global', '全局项');
    renderFieldChoices('#diff-server', data.server, 'server', 'server 段');

    $$('.diff-chip').forEach(ch => ch.addEventListener('click', () => {
        const sec = $('#diff-' + ch.dataset.diff);
        if (sec) sec.classList.toggle('hidden');
    }));
    $('#btn-config-apply').addEventListener('click', applyImport);
}

function renderAdded(sel, entries) {
    const box = $(sel);
    if (!entries || !entries.length) return;
    box.innerHTML = `<div class="diff-header"><strong>新增（${entries.length}，将采用导入）</strong></div>
        <div class="table-wrap"><table class="table"><tbody>
        ${entries.map(e => `<tr><td>${entryLabel(e)}</td></tr>`).join('')}
        </tbody></table></div>`;
    box.classList.remove('hidden');
}

function renderDeleted(sel, entries, kind) {
    const box = $(sel);
    if (!entries || !entries.length) return;
    const rows = entries.map(e => `
        <tr data-key="${esc(e.key || e.name)}">
            <td>${entryLabel(e)}</td>
            <td class="muted">导入配置中不存在</td>
            <td>
                <label class="radio"><input type="radio" name="${kind}-del-${esc(e.key || e.name)}" value="delete"> 删除</label>
                <label class="radio"><input type="radio" name="${kind}-del-${esc(e.key || e.name)}" value="keep" checked> 保留</label>
            </td>
        </tr>`).join('');
    box.innerHTML = `<div class="diff-header"><strong>删除（${entries.length}，默认保留）</strong>
        <button type="button" class="btn btn-sm" data-bulk-del="delete">全部删除</button>
        <button type="button" class="btn btn-sm" data-bulk-del="keep">全部保留</button></div>
        <div class="table-wrap"><table class="table"><thead><tr><th>条目</th><th>说明</th><th>选择</th></tr></thead><tbody>${rows}</tbody></table></div>`;
    box.classList.remove('hidden');
    $$('[data-bulk-del]', box).forEach(b => b.addEventListener('click', () => {
        const val = b.dataset.bulkDel;
        $$('input[type=radio]', box).forEach(r => { r.checked = (r.value === val); });
    }));
}

function renderModified(sel, entries, kind) {
    const box = $(sel);
    if (!entries || !entries.length) return;
    const rows = entries.map(e => `
        <tr data-key="${esc(e.key || e.name)}">
            <td>${entryLabel(e)}</td>
            <td>${changeCell(e.changes)}</td>
            <td>
                <label class="radio"><input type="radio" name="${kind}-${esc(e.key || e.name)}" value="imported" checked> 采用导入</label>
                <label class="radio"><input type="radio" name="${kind}-${esc(e.key || e.name)}" value="existing"> 保留现有</label>
            </td>
        </tr>`).join('');
    box.innerHTML = `<div class="diff-header"><strong>修改（${entries.length}，默认采用导入）</strong>
        <button type="button" class="btn btn-sm" data-bulk="imported">全部采用导入</button>
        <button type="button" class="btn btn-sm" data-bulk="existing">全部保留</button></div>
        <div class="table-wrap"><table class="table"><thead><tr><th>条目</th><th>差异</th><th>选择</th></tr></thead><tbody>${rows}</tbody></table></div>`;
    box.classList.remove('hidden');
    $$('[data-bulk]', box).forEach(b => b.addEventListener('click', () => {
        const val = b.dataset.bulk;
        $$('input[type=radio]', box).forEach(r => { r.checked = (r.value === val); });
    }));
}

function renderFieldChoices(sel, changes, kind, title) {
    const box = $(sel);
    if (!changes || !changes.length) return;
    const rows = changes.map(c => `
        <tr data-field="${esc(c.field)}">
            <td><code>${esc(c.field)}</code></td>
            <td class="muted">${esc(fmt(c.existing))}</td>
            <td class="muted">${esc(fmt(c.imported))}</td>
            <td>
                <label class="radio"><input type="radio" name="${kind}-${esc(c.field)}" value="imported" checked> 采用导入</label>
                <label class="radio"><input type="radio" name="${kind}-${esc(c.field)}" value="existing"> 保留现有</label>
            </td>
        </tr>`).join('');
    box.innerHTML = `<div class="diff-header"><strong>${title}（变更 ${changes.length}，默认采用导入）</strong></div>
        <div class="table-wrap"><table class="table"><thead><tr><th>字段</th><th>现有</th><th>导入</th><th>选择</th></tr></thead><tbody>${rows}</tbody></table></div>`;
    box.classList.remove('hidden');
}

async function applyImport() {
    const config = $('#config-import').value.trim();
    if (!config) return;
    const choices = {
        repository_deletions: [],
        repository_choices: {},
        storage_deletions: [],
        storage_choices: {},
        global_choices: {},
        server_choices: {}
    };
    const collect = (sel, map, deleteArr) => {
        $$(sel + ' tr[data-key]').forEach(tr => {
            const input = $$('input[type=radio]:checked', tr)[0];
            if (!input) return;
            const key = tr.dataset.key;
            if (deleteArr && input.value === 'delete') deleteArr.push(key);
            else if (!deleteArr) map[key] = input.value;
        });
    };
    collect('#diff-repos-modified', choices.repository_choices, null);
    collect('#diff-repos-deleted', null, choices.repository_deletions);
    collect('#diff-storages-modified', choices.storage_choices, null);
    collect('#diff-storages-deleted', null, choices.storage_deletions);
    $$('#diff-globals tr[data-field]').forEach(tr => {
        const input = $$('input[type=radio]:checked', tr)[0];
        if (input) choices.global_choices[tr.dataset.field] = input.value;
    });
    $$('#diff-server tr[data-field]').forEach(tr => {
        const input = $$('input[type=radio]:checked', tr)[0];
        if (input) choices.server_choices[tr.dataset.field] = input.value;
    });
    try {
        const data = await api('/api/config/import', {
            method: 'POST',
            body: JSON.stringify({ config, choices })
        });
        const d = data || {};
        toast('导入完成：仓库 +' + (d.repositories_added || 0) + '/改 ' + (d.repositories_updated || 0) +
              '/删 ' + (d.repositories_deleted || 0) + '；存储 +' + (d.storages_added || 0) +
              '/改 ' + (d.storages_updated || 0) + '/删 ' + (d.storages_deleted || 0));
        renderApp();
    } catch (e) {
        if (e.errors && e.errors.length) toast('导入失败：' + e.errors.join('；'), true);
        else toast('导入失败: ' + e.message, true);
    }
}

async function reloadConfig() {
    try {
        await api('/api/config/reload', { method: 'POST' });
        toast('配置已从磁盘重载');
        renderApp();
    } catch (e) {
        toast('重载失败: ' + e.message, true);
    }
}

document.addEventListener('DOMContentLoaded', () => {
    $('#btn-maintenance').addEventListener('click', toggleMaintenance);
    $('#btn-maintenance-refresh').addEventListener('click', refreshMaintenance);
    refreshMaintenance();
    $('#log-modal-close').addEventListener('click', closeLogModal);
    $('#log-modal').addEventListener('click', (ev) => {
        if (ev.target === ev.currentTarget) closeLogModal();
    });
    $('#repo-form').addEventListener('submit', saveRepo);
    $('#repo-form-cancel').addEventListener('click', () => $('#repo-modal').classList.add('hidden'));
    $('#repo-modal-close').addEventListener('click', () => $('#repo-modal').classList.add('hidden'));
    $('#repo-modal').addEventListener('click', (ev) => {
        if (ev.target === ev.currentTarget) $('#repo-modal').classList.add('hidden');
    });
    $('#storage-form').addEventListener('submit', saveStorage);
    $('#storage-form-cancel').addEventListener('click', () => $('#storage-modal').classList.add('hidden'));
    $('#storage-modal-close').addEventListener('click', () => $('#storage-modal').classList.add('hidden'));
    $('#storage-modal').addEventListener('click', (ev) => {
        if (ev.target === ev.currentTarget) $('#storage-modal').classList.add('hidden');
    });

    refreshMetrics();
    setInterval(refreshMetrics, 15000);

    renderApp();
});
