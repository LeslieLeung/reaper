const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

class FakeElement {
    constructor(id = '') {
        this.id = id;
        this.dataset = {};
        this.value = '';
        this.listeners = {};
    }

    addEventListener(type, listener) {
        (this.listeners[type] ||= []).push(listener);
    }

    dispatch(type, event = {}) {
        const payload = Object.assign({
            target: this,
            currentTarget: this,
            preventDefault() {}
        }, event);
        (this.listeners[type] || []).forEach(listener => listener(payload));
    }
}

class FakeApp extends FakeElement {
    constructor() {
        super('app');
        this.elements = new Map();
        this.pageButtons = [];
        this._innerHTML = '';
    }

    set innerHTML(html) {
        this._innerHTML = html;
        this.elements.clear();
        this.pageButtons = [];

        for (const match of html.matchAll(/id="([^"]+)"/g)) {
            this.elements.set(match[1], new FakeElement(match[1]));
        }
        for (const match of html.matchAll(/<button[^>]*class="[^"]*\bpg-page\b[^"]*"[^>]*>/g)) {
            const id = /id="([^"]+)"/.exec(match[0])?.[1] || '';
            const page = /data-page="(\d+)"/.exec(match[0])?.[1] || '';
            const button = new FakeElement(id);
            button.dataset.page = page;
            this.elements.set(id, button);
            this.pageButtons.push(button);
        }
    }

    get innerHTML() {
        return this._innerHTML;
    }

    querySelector(selector) {
        return selector.startsWith('#') ? this.elements.get(selector.slice(1)) || null : null;
    }

    querySelectorAll(selector) {
        return selector === '.pg-page' ? this.pageButtons : [];
    }
}

function repositoryPage(total, repositories) {
    return {
        total,
        page: 1,
        limit: 20,
        repositories: repositories || [{
            Name: 'repo', URL: 'https://example.com/acme/repo', Type: 'repo',
            Cron: '', Storage: [], UseCache: false, AllBranches: false,
            DownloadReleases: false, DownloadIssues: false, DownloadWiki: false,
            DownloadDiscussion: false, total_runs: 0, success_runs: 0, failed_runs: 0
        }]
    };
}

function createHarness(responder, initialHash = '#/repositories') {
    const app = new FakeApp();
    const requests = [];
    let hash = initialHash;
    let hashChange = null;
    const location = {
        get hash() { return hash; },
        set hash(value) {
            hash = value;
            if (hashChange) queueMicrotask(hashChange);
        }
    };
    const document = {
        querySelector: selector => selector === '#app' ? app : app.querySelector(selector),
        querySelectorAll: selector => app.querySelectorAll(selector),
        addEventListener() {}
    };
    const context = {
        URLSearchParams,
        document,
        location,
        fetch: async url => {
            requests.push(url);
            return { ok: true, status: 200, json: async () => ({ code: 200, data: responder(url) }) };
        },
        setTimeout,
        clearTimeout,
        setInterval: () => 0,
        EventSource: function () {},
        confirm: () => true
    };
    context.globalThis = context;
    context.window = context;
    context.window.addEventListener = (type, listener) => {
        if (type === 'hashchange') hashChange = listener;
    };

    vm.createContext(context);
    vm.runInContext(fs.readFileSync('web/static/js/pagination.js', 'utf8'), context);
    vm.runInContext(fs.readFileSync('web/static/js/main.js', 'utf8'), context);
    return { app, context, requests };
}

test('repository page buttons request the selected page and search resets to page one', async () => {
    const harness = createHarness(() => repositoryPage(240));
    vm.runInContext("state.activeRoute = 'repositories'; state.routeEpoch = 1", harness.context);
    await vm.runInContext('renderRepositories()', harness.context);

    const pageTwo = harness.app.pageButtons.find(button => button.dataset.page === '2');
    assert.ok(pageTwo);
    pageTwo.dispatch('click');
    assert.equal(vm.runInContext('state.reposPage', harness.context), 2);

    await new Promise(resolve => setImmediate(resolve));
    assert.equal(new URL(harness.requests.at(-1), 'http://localhost').searchParams.get('page'), '2');
    const search = harness.app.querySelector('#repos-search');
    search.value = 'needle';
    harness.app.querySelector('#btn-search-repos').dispatch('click');

    assert.equal(vm.runInContext('state.reposPage', harness.context), 1);
    assert.equal(vm.runInContext('state.reposSearch', harness.context), 'needle');
    await new Promise(resolve => setImmediate(resolve));
    const searchRequest = new URL(harness.requests.at(-1), 'http://localhost').searchParams;
    assert.equal(searchRequest.get('page'), '1');
    assert.equal(searchRequest.get('search'), 'needle');
});

test('next run sorting survives route loading, direction changes and pagination', async () => {
    const harness = createHarness(() => repositoryPage(60), '#/repositories?sort=next_run&direction=asc');
    vm.runInContext('renderApp()', harness.context);
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(harness.app.querySelector('#repos-sort').value, 'next_run');

    harness.app.querySelector('#repos-direction').value = 'desc';
    harness.app.querySelector('#repos-direction').dispatch('change');
    await new Promise(resolve => setImmediate(resolve));
    harness.app.pageButtons.find(button => button.dataset.page === '2').dispatch('click');
    await new Promise(resolve => setImmediate(resolve));

    const params = new URL(harness.requests.at(-1), 'http://localhost').searchParams;
    assert.equal(params.get('sort'), 'next_run');
    assert.equal(params.get('direction'), 'desc');
    assert.equal(params.get('page'), '2');
});

test('an out-of-range repository page refetches the final valid page', async () => {
    const harness = createHarness(url => {
        const page = new URL(url, 'http://localhost').searchParams.get('page');
        return page === '9' ? repositoryPage(75, []) : repositoryPage(75);
    }, '#/repositories?page=9&sort=attention&direction=asc');
    vm.runInContext("state.activeRoute = 'repositories'; state.routeEpoch = 1; state.reposPage = 9", harness.context);

    await vm.runInContext('renderRepositories()', harness.context);
    await new Promise(resolve => setImmediate(resolve));

    assert.deepEqual(harness.requests.map(url => new URL(url, 'http://localhost').searchParams.get('page')), ['9', '4']);
    assert.equal(vm.runInContext('state.reposPage', harness.context), 4);
    assert.match(harness.context.location.hash, /page=4/);
});
