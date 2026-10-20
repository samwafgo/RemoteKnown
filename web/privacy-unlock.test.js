const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { EventEmitter } = require('node:events');

function unlockContext() {
    const source = fs.readFileSync(require.resolve('./main.js'), 'utf8');
    const requireUnlock = source.slice(source.indexOf('async function requireUnlock(action)'), source.indexOf('// privacyDisable'));
    const handleRequest = source.slice(source.indexOf('let privacyUnlocking = false;'), source.indexOf('// finishUnlock'));
    const windows = [];
    const calls = [];
    class Window extends EventEmitter {
        constructor() { super(); windows.push(this); }
        isDestroyed() { return false; }
        setContentProtection() {}
        setAlwaysOnTop() {}
        loadFile() {}
    }
    const context = vm.createContext({
        BrowserWindow: Window,
        UNLOCK_WINDOW_WIDTH: 340,
        path: require('node:path'),
        __dirname,
        securityState: { enabled: true, lock_tray: false },
        securityStateLoaded: true,
        unlockWindow: null,
        unlockPending: null,
        isGated: () => false,
        refreshSecurityState: async () => {},
        activateUnlockWindow: () => {},
        privacyDisable: async () => { calls.push('disable'); },
        fetchAPIPost: async endpoint => { calls.push(endpoint); },
        dialog: { showErrorBox: (...args) => calls.push(args) },
        console
    });
    vm.runInContext(requireUnlock + '\n' + handleRequest, context);
    return { context, windows, calls };
}

test('privacy password is required even when tray password gating is disabled', async () => {
    const { context, windows } = unlockContext();
    const pending = context.requireUnlock('privacy');
    assert.equal(windows.length, 1);
    context.unlockPending.resolve(true);
    assert.equal(await pending, true);
});

test('existing unlock dialog is awaited and does not acknowledge cancellation early', async () => {
    const { context, windows, calls } = unlockContext();
    const original = context.requireUnlock('privacy');
    const request = context.handlePrivacyUnlockRequest({ require_password: true });
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(windows.length, 1);
    assert.deepEqual(calls, []);
    context.unlockPending.resolve(true);
    await original;
    await request;
    assert.deepEqual(calls, ['disable']);
});

test('canceling unlock dialog acknowledges pending request to restore input blocking', async () => {
    const { context, calls } = unlockContext();
    const request = context.handlePrivacyUnlockRequest({ require_password: true });
    await new Promise(resolve => setImmediate(resolve));
    context.unlockPending.resolve(false);
    await request;
    assert.deepEqual(calls, ['/api/privacy/unlock-ack']);
});
