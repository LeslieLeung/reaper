const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const pagination = require('./static/js/pagination.js');

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
