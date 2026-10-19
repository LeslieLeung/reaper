const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const pagination = require('./static/js/pagination.js');

function maintenanceHarness(responses) {
    const controls = Object.fromEntries(['#maintenance-status', '#btn-maintenance', '#btn-maintenance-refresh'].map(id => [id, {
        textContent: '', disabled: false, addEventListener() {},
    }]));
    const requests = [];
    const timers = [];
    const context = vm.createContext({
        document: { addEventListener() {}, querySelector: id => controls[id] || null, querySelectorAll: () => [] },
        window: { addEventListener() {}, GitrievePagination: pagination },
        location: { hash: '#/repositories' }, URLSearchParams,
        setTimeout(fn) { timers.push(fn); return timers.length; }, clearTimeout() {}, setInterval() {},
        fetch: async (path, opts) => {
            requests.push({path, method: opts.method || 'GET'});
            return {ok:true, json:async () => ({data: responses.shift()})};
        },
    });
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/main.js'), 'utf8'), context);
    return {context, controls, requests, timers};
}

test('maintenance readiness waits for drain and never reloads repositories', async () => {
    const h = maintenanceHarness([
        {enabled:true, ready:false, running:2, pending:1, preparing:1},
        {enabled:true, ready:true, running:0, pending:0, preparing:0},
    ]);
    await vm.runInContext('refreshMaintenance()',h.context);
    assert.equal(h.controls['#btn-maintenance'].disabled,true);
    assert.match(h.controls['#maintenance-status'].textContent,/2/);
    assert.doesNotMatch(h.controls['#maintenance-status'].textContent,/可以安全升级/);
    assert.equal(h.timers.length,1);
    await h.timers[0]();
    assert.match(h.controls['#maintenance-status'].textContent,/可以安全升级/);
    assert.equal(h.controls['#btn-maintenance'].disabled,false);
    assert.deepEqual(h.requests.map(r => r.path),['/api/maintenance','/api/maintenance']);
});

test('maintenance controls enter and leave without submitting jobs', async () => {
    const h = maintenanceHarness([
        {enabled:false, ready:false}, {enabled:true, ready:true}, {enabled:false, ready:false},
    ]);
    await vm.runInContext('refreshMaintenance()',h.context);
    await vm.runInContext('toggleMaintenance()',h.context);
    assert.match(h.controls['#btn-maintenance'].textContent,/退出维护/);
    await vm.runInContext('toggleMaintenance()',h.context);
    assert.match(h.controls['#btn-maintenance'].textContent,/进入维护/);
    assert.deepEqual(h.requests.map(r => r.method),['GET','POST','DELETE']);
    assert.ok(h.requests.every(r => r.path === '/api/maintenance'));
});

test('maintenance persistence errors do not show safe upgrade or allow exit', async () => {
    const h = maintenanceHarness([
        {enabled:true, drained:true, ready:false, error:'database write failed'},
    ]);
    await vm.runInContext('refreshMaintenance()',h.context);
    assert.match(h.controls['#maintenance-status'].textContent,/database write failed/);
    assert.doesNotMatch(h.controls['#maintenance-status'].textContent,/可以安全升级/);
    assert.equal(h.controls['#btn-maintenance'].disabled,true);
    assert.equal(h.timers.length,0);
    await vm.runInContext('toggleMaintenance()',h.context);
    assert.equal(h.requests.length,1);
});

test('server shutdown cannot be reversed from the maintenance control', async () => {
    const h = maintenanceHarness([{enabled:true, ready:true, shutting_down:true}]);
    await vm.runInContext('refreshMaintenance()',h.context);
    assert.equal(h.controls['#btn-maintenance'].disabled,true);
    await vm.runInContext('toggleMaintenance()',h.context);
    assert.equal(h.requests.length,1);
});

test('stuck repository badges retain the active execution status', () => {
    const context = vm.createContext({
        document: { addEventListener() {} },
        window: { addEventListener() {}, GitrievePagination: pagination },
    });
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/main.js'), 'utf8'), context);
    for (const status of ['running', 'pending']) {
        context.repo = { health_status: 'stuck', last_status: status, stuck: true };
        const html = vm.runInContext('repositoryHealthBadge(repo)', context);
        const labels = [...html.matchAll(/<span\b[^>]*>([^<]*)<\/span>/g)].map(match => match[1]);
        assert.deepEqual(labels, [status, 'stuck']);
    }
    for (const health of ['running', 'pending', 'healthy', 'failed', 'overdue', 'never_synced']) {
        context.repo = { health_status: health, last_status: health, stuck: false };
        const html = vm.runInContext('repositoryHealthBadge(repo)', context);
        const labels = [...html.matchAll(/<span\b[^>]*>([^<]*)<\/span>/g)].map(match => match[1]);
        assert.deepEqual(labels, [health]);
    }
});

test('renders repository actions before the repository name', async () => {
    const app = { innerHTML: '' };
    const control = () => ({
        addEventListener() {},
        value: '',
        checked: false,
    });
    const controls = {
        '#btn-add-repo': control(),
        '#btn-refresh-repos': control(),
        '#repos-search': control(),
        '#btn-search-repos': control(),
        '#repos-health': control(),
        '#repos-overdue': control(),
        '#repos-sort': control(),
        '#repos-direction': control(),
    };
    const document = {
        addEventListener() {},
        querySelector(selector) {
            if (selector === '#app') return app;
            return controls[selector] || null;
        },
        querySelectorAll() {
            return [];
        },
    };
    const context = vm.createContext({
        document,
        fetch: async () => ({
            ok: true,
            status: 200,
            json: async () => ({
                data: {
                    repositories: [{
                        Name: 'gitrieve',
                        URL: 'github.com/wnarutou/gitrieve',
                        Type: 'repo',
                        Storage: [],
                        health_status: 'healthy',
                        last_status: 'completed',
                        total_runs: 0,
                        success_runs: 0,
                        failed_runs: 0,
                    }],
                    summary: { total: 1, healthy: 1 },
                    total: 1,
                },
            }),
        }),
        location: { hash: '#/repositories' },
        setInterval() {},
        setTimeout() {},
        clearTimeout() {},
        URLSearchParams,
        window: { addEventListener() {}, GitrievePagination: pagination },
    });

    const scriptPath = path.join(__dirname, 'static', 'js', 'main.js');
    vm.runInContext(fs.readFileSync(scriptPath, 'utf8'), context);
    await vm.runInContext("state.activeRoute = 'repositories'; state.routeEpoch = 1; renderRepositories(1)", context);

    const headerCells = [...app.innerHTML.matchAll(/<th(?:\s[^>]*)?>([\s\S]*?)<\/th>/g)]
        .map((match) => match[1].trim());
    const firstRow = app.innerHTML.match(/<tbody>\s*<tr>([\s\S]*?)<\/tr>/);
    assert.ok(firstRow, 'expected a rendered repository row');
    const bodyCells = [...firstRow[1].matchAll(/<td(?:\s+class="([^"]*)")?[^>]*>([\s\S]*?)<\/td>/g)];

    const actionIndex = headerCells.indexOf('Actions');
    const nameIndex = headerCells.indexOf('Name / URL');
    assert.ok(actionIndex >= 0, 'expected an Actions column');
    assert.ok(nameIndex > actionIndex, 'expected Actions before the repository name');
    assert.equal(bodyCells[0][1], 'actions');
    assert.match(bodyCells[0][2], /btn-run-repo/);
    assert.match(bodyCells[nameIndex][2], /<strong>gitrieve<\/strong>/);
});
