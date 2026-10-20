const { test } = require('node:test');
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const { createPrivacyPrompt } = require('./privacy-prompt');

function setup(post = async () => ({ success: true })) {
    const windows = [];
    const handlers = new Map();
    class Window extends EventEmitter {
        constructor() {
            super();
            this.dead = false;
            this.messages = [];
            this.webContents = { send: (name, value) => this.messages.push([name, value]) };
            windows.push(this);
        }
        isDestroyed() { return this.dead; }
        destroy() { this.dead = true; this.emit('closed'); }
        setAlwaysOnTop() {}
        show() { this.shown = true; }
        moveTop() {}
        focus() {}
        loadFile() {}
    }
    const prompt = createPrivacyPrompt({
        BrowserWindow: Window,
        ipcMain: { handle: (name, handler) => handlers.set(name, handler) },
        post
    });
    const status = (id = 'session-1', seconds = 10) => ({
        auto_pending: true,
        auto_request_id: id,
        auto_deadline: new Date(Date.now() + seconds * 1000).toISOString()
    });
    const invoke = (name, sender = windows.at(-1).webContents) => handlers.get(name)({ sender });
    return { prompt, windows, status, invoke };
}

test('one window per request and shown response extends the visible deadline', async () => {
    const calls = [];
    const env = setup(async (endpoint, body) => {
        calls.push([endpoint, body]);
        return { status: env.status('session-1', 12) };
    });
    const old = env.status('session-1', 8);
    env.prompt.sync(old);
    env.prompt.sync(old);
    assert.equal(env.windows.length, 1);
    env.windows[0].emit('ready-to-show');
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(env.windows[0].shown, true);
    assert.deepEqual(calls[0], ['/api/privacy/auto', { request_id: 'session-1', action: 'shown' }]);
    const extended = env.invoke('privacyAutoGetState').deadline;
    env.prompt.sync(old);
    assert.equal(env.invoke('privacyAutoGetState').deadline, extended);
    env.prompt.close();
});

test('cancel only this request; stale poll cannot reopen it and next session can prompt', async () => {
    const calls = [];
    const env = setup(async (endpoint, body) => { calls.push(body); });
    const old = env.status();
    env.prompt.sync(old);
    assert.equal((await env.invoke('privacyAutoCancel')).success, true);
    assert.deepEqual(calls, [{ request_id: 'session-1', action: 'cancel' }]);
    assert.equal(env.windows[0].dead, true);
    env.prompt.sync(old);
    assert.equal(env.windows.length, 1);
    env.prompt.sync(env.status('session-2'));
    assert.equal(env.windows.length, 2);
    env.prompt.close();
});

test('reject IPC from other windows and retain prompt on cancellation failure', async () => {
    const env = setup(async () => { throw new Error('本次倒计时已结束'); });
    env.prompt.sync(env.status());
    assert.equal((await env.invoke('privacyAutoCancel', {})).success, false);
    assert.equal(env.invoke('privacyAutoGetState', {}), null);
    const result = await env.invoke('privacyAutoCancel');
    assert.equal(result.error, '本次倒计时已结束');
    assert.equal(env.windows[0].dead, false);
    env.prompt.sync({ auto_pending: false, active: true });
    assert.equal(env.windows[0].dead, true);
});

test('closing prompt cancels while session end dismisses it without cancellation', async () => {
    const calls = [];
    const env = setup(async (endpoint, body) => { calls.push(body); });
    env.prompt.sync(env.status());
    let prevented = false;
    env.windows[0].emit('close', { preventDefault: () => { prevented = true; } });
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(prevented, true);
    assert.equal(calls[0].action, 'cancel');
    env.prompt.sync(env.status('session-2'));
    env.prompt.sync({ auto_pending: false });
    assert.equal(calls.length, 1);
    assert.equal(env.windows[1].dead, true);
});
