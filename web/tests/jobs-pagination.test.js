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

function jobsPage(total, jobs) {
    return {
        total,
        page: 1,
        limit: 20,
        jobs: jobs || [{
            id: 'job-1',
            name: 'archive',
            url: 'https://example.com/acme/repo',
            status: 'completed',
            start_time: '2026-09-12T01:00:00Z',
            end_time: '2026-09-12T01:01:00Z',
            error_message: ''
        }]
    };
}

function createHarness(responder) {
    const app = new FakeApp();
    const requests = [];
    const document = {
        querySelector: selector => selector === '#app' ? app : app.querySelector(selector),
        querySelectorAll: selector => app.querySelectorAll(selector),
        addEventListener() {}
    };
    const context = {
        URLSearchParams,
        document,
        location: { hash: '#/jobs' },
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
    context.window.addEventListener = () => {};

    vm.createContext(context);
    vm.runInContext(fs.readFileSync('web/static/js/pagination.js', 'utf8'), context);
    vm.runInContext(fs.readFileSync('web/static/js/main.js', 'utf8'), context);
    return { app, context, requests };
}

test('job page buttons request the selected page', async () => {
    const harness = createHarness(() => jobsPage(240));
    await vm.runInContext('renderJobs()', harness.context);

    const pageTwo = harness.app.pageButtons.find(button => button.dataset.page === '2');
    assert.ok(pageTwo);
    pageTwo.dispatch('click');

    await new Promise(resolve => setImmediate(resolve));
    assert.equal(vm.runInContext('state.jobsPage', harness.context), 2);
    assert.equal(new URL(harness.requests.at(-1), 'http://localhost').searchParams.get('page'), '2');
});

test('an out-of-range job page refetches the final valid page', async () => {
    const harness = createHarness(url => {
        const page = new URL(url, 'http://localhost').searchParams.get('page');
        return page === '9' ? jobsPage(75, []) : jobsPage(75);
    });
    vm.runInContext('state.jobsPage = 9', harness.context);

    await vm.runInContext('renderJobs()', harness.context);

    assert.deepEqual(harness.requests.map(url => new URL(url, 'http://localhost').searchParams.get('page')), ['9', '4']);
    assert.equal(vm.runInContext('state.jobsPage', harness.context), 4);
});

test('job pagination renders first, last, current, and neighboring pages with totals', async () => {
    const harness = createHarness(() => jobsPage(240));
    vm.runInContext('state.jobsPage = 6', harness.context);

    await vm.runInContext('renderJobs()', harness.context);

    assert.match(harness.app.innerHTML, /Page 6 of 12 \(240 total\)/);
    assert.match(harness.app.innerHTML, /class="pg-ellipsis"/);
    assert.deepEqual(
        harness.app.pageButtons.map(button => Number(button.dataset.page)),
        [1, 5, 6, 7, 12]
    );
});

test('job pagination does not request a page past the final page', async () => {
    const harness = createHarness(() => jobsPage(240));
    vm.runInContext('state.jobsPage = 12', harness.context);
    await vm.runInContext('renderJobs()', harness.context);
    const requestCount = harness.requests.length;

    harness.app.querySelector('#pg-next-jobs').dispatch('click');
    await new Promise(resolve => setImmediate(resolve));

    assert.equal(vm.runInContext('state.jobsPage', harness.context), 12);
    assert.equal(harness.requests.length, requestCount);
});
