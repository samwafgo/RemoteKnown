const { app, BrowserWindow, Tray, Menu, ipcMain, nativeImage, Notification, dialog } = require('electron');
const { spawn, spawnSync } = require('child_process');
const path = require('path');
const http = require('http');
const fs = require('fs');
const { createPrivacyPrompt } = require('./privacy-prompt');
let privacyPrompt = null;

// 正式版与 package.json build.appId 一致；开发版独立分组，避免复用已安装版的旧图标缓存。
const APP_USER_MODEL_ID = app.isPackaged ? 'com.remoteknown.app' : 'com.remoteknown.app.dev';
app.setAppUserModelId(APP_USER_MODEL_ID);

const API_BASE = 'http://127.0.0.1:18080';

const STARTUP_REG_KEY = 'HKCU\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Run';
const STARTUP_APP_NAME = 'RemoteKnown';

function getStartupEnabled() {
    try {
        const result = spawnSync('reg', [
            'query', STARTUP_REG_KEY, '/v', STARTUP_APP_NAME
        ], { encoding: 'utf8', windowsHide: true });
        return result.status === 0 && result.stdout.includes(STARTUP_APP_NAME);
    } catch (e) {
        return false;
    }
}

function setStartupEnabled(enabled) {
    try {
        if (enabled) {
            const exePath = app.getPath('exe');
            const result = spawnSync('reg', [
                'add', STARTUP_REG_KEY,
                '/v', STARTUP_APP_NAME,
                '/t', 'REG_SZ',
                '/d', `"${exePath}"`,
                '/f'
            ], { encoding: 'utf8', windowsHide: true });
            return result.status === 0;
        } else {
            const result = spawnSync('reg', [
                'delete', STARTUP_REG_KEY,
                '/v', STARTUP_APP_NAME,
                '/f'
            ], { encoding: 'utf8', windowsHide: true });
            return result.status === 0;
        }
    } catch (e) {
        console.error('[自启] 注册表操作失败:', e.message);
        return false;
    }
}

let mainWindow;
let tray = null;
let isQuitting = false;  // 允许窗口真正关闭（而不是隐藏到托盘）
let exitHandled = false; // 退出通知流程是否已经跑过，防止重入
let backendProcess = null;
let lastRemoteStatus = false; // 记录上一次的远程状态
let currentSessionStartTime = null; // 记录当前会话开始时间

// --- 密码保护 ---
// securityState 由 2 秒轮询刷新，托盘闸门与静默通知都读它。
let securityState = { enabled: false, has_password: false, silent_local: true, lock_tray: true };
const UNLOCK_WINDOW_WIDTH = 340;
let unlockWindow = null;
let unlockPending = null;   // { resolve } —— 等待解锁小窗给出结果
let lastUnlockToken = null; // 最近一次验证通过的令牌，供"修改安全设置"复用
let securityStateLoaded = false; // 是否已经成功读到过一次安全状态
let initialShowDone = false;
// 守护进程被杀 -> 界面拉起来之后补一条告警（每次运行只报一次，避免刷屏）
let backendStartedAt = 0;
let backendDiedUnexpectedly = false;
let backendDeathReported = false;
let backendStarting = false;   // 探活是异步的，防止并发起两个守护进程

// --- 单实例锁逻辑 ---
const gotTheLock = app.requestSingleInstanceLock();

if (!gotTheLock) {
    app.quit();
} else {
    app.on('second-instance', async (event, commandLine, workingDirectory) => {
        // 当运行第二个实例时,将焦点集中到主窗口。
        // 再次启动本程序同样等于"显示主窗口"，闸门不能从这里绕过去。
        if (!mainWindow) return;
        if (!mainWindow.isVisible() && !await requireUnlock('show')) return;
        if (mainWindow.isMinimized()) mainWindow.restore();
        mainWindow.show();
        mainWindow.focus();
    });

    // 只有获取到锁的实例才执行初始化
    app.whenReady().then(() => {
        const startupFlagFile = path.join(app.getPath('userData'), '.startup_initialized');
        if (!fs.existsSync(startupFlagFile)) {
            setStartupEnabled(true);
            fs.writeFileSync(startupFlagFile, '1');
        }

        createWindow();
        createTray();
        startBackend();
        setupIPC();
        privacyPrompt = createPrivacyPrompt({ BrowserWindow, ipcMain, post: fetchAPIPost });

        // 先问一次安全状态，决定启动时要不要显示主窗口。
        // 兜底：4 秒内后端还没答复就照常显示，免得界面永远出不来。
        refreshSecurityState();
        setTimeout(decideInitialVisibility, 4000);

        // 定时轮询状态
        setInterval(async () => {
            if (mainWindow && !mainWindow.isDestroyed()) {
                try {
                    refreshSecurityState();
                    const status = await fetchAPI('/api/status');
                    if (status) {
                        // 检测状态变化并发送通知
                        if (status.remote_active !== lastRemoteStatus) {
                            if (status.remote_active) {
                                // 远程控制开始
                                currentSessionStartTime = new Date(); // 记录开始时间

                                let startTimeStr = '刚刚';
                                if (status.start_time) {
                                    try {
                                        const date = new Date(status.start_time);
                                        // 如果后端返回了有效时间，更新 currentSessionStartTime
                                        if (!isNaN(date.getTime())) {
                                            currentSessionStartTime = date;
                                        }
                                        startTimeStr = date.toLocaleString('zh-CN', {
                                            hour12: false,
                                            year: 'numeric',
                                            month: '2-digit',
                                            day: '2-digit',
                                            hour: '2-digit',
                                            minute: '2-digit',
                                            second: '2-digit'
                                        });
                                    } catch (e) { }
                                }

                                // 获取远程工具名称
                                let tools = '未知软件';
                                if (status.signals && status.signals.length > 0) {
                                    const names = [...new Set(status.signals.map(s => s.name || s.Name))];
                                    if (names.length > 0) {
                                        tools = names.join(', ');
                                    }
                                }

                                // 静默模式下不弹本地通知，避免远程操作者看见"你被发现了"。
                                // 飞书/钉钉/邮件由守护进程独立发送，不受这里影响。
                                if (!isSilent()) {
                                    new Notification({
                                        title: '警告：正在被远程控制',
                                        body: `软件: ${tools}\n时间: ${startTimeStr}`,
                                        icon: path.join(__dirname, 'icon.png'),
                                        urgent: true
                                    }).show();
                                } else {
                                    console.log('[静默] 密码保护静默已开启，跳过本地通知');
                                }
                            } else {
                                // 远程控制结束
                                let durationStr = '未知';

                                // 优先使用后端返回的时长，如果后端返回空则前端计算
                                if (status.duration) {
                                    durationStr = status.duration;
                                } else if (currentSessionStartTime) {
                                    const now = new Date();
                                    const diffSeconds = Math.floor((now - currentSessionStartTime) / 1000);

                                    const h = Math.floor(diffSeconds / 3600);
                                    const m = Math.floor((diffSeconds % 3600) / 60);
                                    const s = diffSeconds % 60;
                                    durationStr = `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
                                }

                                currentSessionStartTime = null; // 重置

                                if (!isSilent()) {
                                    new Notification({
                                        title: '安全：远程控制已结束',
                                        body: `远程会话已结束。\n持续时长: ${durationStr}`,
                                        icon: path.join(__dirname, 'icon.png')
                                    }).show();
                                }
                            }
                            lastRemoteStatus = status.remote_active;
                        }

                        mainWindow.webContents.send('statusUpdate', status);
                        updateTray(status);
                    }

                    // 隐私屏：同一轮里顺带拿一次状态。
                    // unlock_pending 表示用户在遮罩下按了热键，等着弹密码框。
                    const pv = await fetchAPI('/api/privacy/status');
                    if (pv) {
                        mainWindow.webContents.send('privacyUpdate', pv);
                        privacyPrompt.sync(pv);
                        if (pv.unlock_pending) {
                            const cfg = await fetchAPI('/api/privacy/config');
                            handlePrivacyUnlockRequest(cfg);
                        }
                    }
                } catch (e) {
                    // ignore errors during poll
                }
            }
        }, 2000);
    });

    app.on('window-all-closed', () => {
        if (process.platform !== 'darwin') {
            app.quit();
        }
    });

    app.on('will-quit', (event) => {
        // 退出流程只跑一次。
        // 注意这里必须用独立的 exitHandled，不能用 isQuitting：
        // 托盘"退出"要先把 isQuitting 置真，窗口才关得掉（见 mainWindow 的 close 处理），
        // 否则这里会被误判成"已处理"而直接跳过退出通知。
        if (exitHandled) {
            console.log('[退出] 已经处理过退出，直接退出');
            return;
        }
        exitHandled = true;

        console.log('[退出] 开始处理退出流程');
        // 阻止默认退出行为，等待通知发送完成
        event.preventDefault();
        isQuitting = true;

        console.log('[退出] 发送退出通知请求到后端...');
        const startTime = Date.now();
        
        // 发送退出通知到后端，最多等待3秒
        Promise.race([
            fetchAPIPost('/api/notify', { type: 'app_exit' }).then((result) => {
                const elapsed = Date.now() - startTime;
                console.log(`[退出] 收到后端响应 (耗时: ${elapsed}ms):`, result);
                return result;
            }),
            new Promise((resolve) => {
                setTimeout(() => {
                    const elapsed = Date.now() - startTime;
                    console.log(`[退出] 等待超时 (${elapsed}ms)，继续退出`);
                    resolve({ timeout: true });
                }, 3000);
            })
        ]).then((result) => {
            // 通知发送完成（成功或超时），现在可以安全地退出
            console.log('[退出] 准备杀死后端进程并退出应用');
            stopBackend();
            console.log('[退出] 后端进程已终止');
            // 真正退出应用
            console.log('[退出] 调用 app.exit(0)');
            app.exit(0);
        }).catch((error) => {
            // 即使失败也要退出
            const elapsed = Date.now() - startTime;
            console.error(`[退出] 发送退出通知失败 (耗时: ${elapsed}ms):`, error);
            stopBackend();
            console.log('[退出] 后端进程已终止（错误情况下）');
            console.log('[退出] 调用 app.exit(0)（错误情况下）');
            app.exit(0);
        });
    });

    app.on('activate', () => {
        if (BrowserWindow.getAllWindows().length === 0) {
            createWindow();
        } else if (mainWindow) {
            mainWindow.show();
        }
    });
}

// --- 功能函数定义 ---

// isBackendAlive 探一下 18080 上是不是已经有守护进程在跑
function isBackendAlive() {
    return fetchAPI('/health').then(() => true).catch(() => false);
}

// stopBackend 关闭守护进程。
// 守护进程是分离启动的（detached），当前这个可能是上一次界面留下来、本次只是接管的，
// 句柄不在手里，所以按进程名兜底收一次，确保"退出"就是真的退出。
function stopBackend() {
    if (backendProcess) {
        try { backendProcess.kill(); } catch (e) { }
        backendProcess = null;
    }
    try {
        spawnSync('taskkill', ['/IM', 'RemoteKnown-daemon.exe', '/F'], { windowsHide: true });
    } catch (e) {
        console.error('[后端] 结束守护进程失败:', e.message);
    }
}

async function startBackend() {
    // backendStarting 是必需的：探活是异步的，没有它两次并发调用会各起一个守护进程
    if (backendProcess || backendStarting) return;
    backendStarting = true;
    try {
        // 守护进程分离之后，界面被杀/重启时它可能还活着。
        // 这时候不能再起一个去抢 18080，否则新进程秒退，陷入 2 秒一次的重启循环。
        if (await isBackendAlive()) {
            console.log('[后端] 已有守护进程在运行，直接接管');
            return;
        }
        spawnBackend();
    } finally {
        backendStarting = false;
    }
}

function spawnBackend() {

    let backendPath;
    if (app.isPackaged) {
        backendPath = path.join(process.resourcesPath, 'RemoteKnown-daemon.exe');
    } else {
        backendPath = path.join(__dirname, '..', 'RemoteKnown-daemon.exe');
    }

    console.log('Starting backend service:', backendPath);

    if (!fs.existsSync(backendPath)) {
        console.error('Backend executable not found at:', backendPath);
        return;
    }

    try {
        backendStartedAt = Date.now();
        backendProcess = spawn(backendPath, [], {
            detached: true, // 必须分离：否则界面被强杀时守护进程会被 job object 一起带走，就没人报警了
            stdio: 'ignore',
            windowsHide: true
        });

        // 让守护进程不再挂在界面的事件循环上，界面被杀它也能继续跑
        backendProcess.unref();

        backendProcess.on('error', (err) => {
            console.error('Backend process error:', err);
        });

        backendProcess.on('close', (code) => {
            console.log(`Backend process exited with code ${code}`);
            const lived = Date.now() - backendStartedAt;
            backendProcess = null;
            if (!isQuitting) {
                // 活过 10 秒才算"曾经正常跑着又被弄死了"。
                // 端口被占导致的秒退会一直循环重启，不能把它当成入侵告警。
                if (lived > 10000 && !backendDeathReported) {
                    backendDiedUnexpectedly = true;
                }
                // 如果非正常退出且应用仍在运行，尝试重启
                setTimeout(startBackend, 2000);
            }
        });
    } catch (err) {
        console.error('Failed to spawn backend:', err);
    }
}

function fetchAPI(endpoint) {
    return new Promise((resolve, reject) => {
        http.get(`${API_BASE}${endpoint}`, (res) => {
            let data = '';
            res.on('data', chunk => data += chunk);
            res.on('end', () => {
                // 检查HTTP状态码
                if (res.statusCode < 200 || res.statusCode >= 300) {
                    try {
                        const errorData = JSON.parse(data);
                        reject(new Error(errorData.error || `HTTP ${res.statusCode}: ${res.statusMessage || '请求失败'}`));
                    } catch (e) {
                        reject(new Error(`HTTP ${res.statusCode}: ${res.statusMessage || data || '请求失败'}`));
                    }
                    return;
                }

                try {
                    resolve(JSON.parse(data));
                } catch (e) {
                    // 如果响应体为空，返回null（兼容旧代码）
                    if (data.trim() === '') {
                        resolve(null);
                    } else {
                        reject(new Error('响应解析失败: ' + e.message));
                    }
                }
            });
        }).on('error', (err) => {
            reject(new Error('网络请求失败: ' + err.message));
        });
    });
}

function fetchAPIPost(endpoint, data, extraHeaders) {
    return new Promise((resolve, reject) => {
        const postData = JSON.stringify(data);
        const options = {
            hostname: '127.0.0.1',
            port: 18080,
            path: endpoint,
            method: 'POST',
            headers: Object.assign({
                'Content-Type': 'application/json',
                'Content-Length': Buffer.byteLength(postData)
            }, extraHeaders || {})
        };

        const req = http.request(options, (res) => {
            let responseData = '';
            res.on('data', chunk => responseData += chunk);
            res.on('end', () => {
                // 检查HTTP状态码
                if (res.statusCode < 200 || res.statusCode >= 300) {
                    try {
                        const errorData = JSON.parse(responseData);
                        reject(new Error(errorData.error || `HTTP ${res.statusCode}: ${res.statusMessage || '请求失败'}`));
                    } catch (e) {
                        reject(new Error(`HTTP ${res.statusCode}: ${res.statusMessage || responseData || '请求失败'}`));
                    }
                    return;
                }

                try {
                    const result = JSON.parse(responseData);
                    resolve(result);
                } catch (e) {
                    // 如果响应体为空或不是JSON，但状态码是成功的，仍然resolve
                    if (responseData.trim() === '') {
                        resolve({});
                    } else {
                        reject(new Error('响应解析失败: ' + e.message));
                    }
                }
            });
        });

        req.on('error', (err) => {
            reject(new Error('网络请求失败: ' + err.message));
        });
        
        req.write(postData);
        req.end();
    });
}

// ===================== 密码保护闸门 =====================

// refreshSecurityState 跟着 2 秒轮询一起刷新，失败时保持上一次的值。
async function refreshSecurityState() {
    try {
        const st = await fetchAPI('/api/security/state');
        if (st) {
            securityState = st;
            securityStateLoaded = true;
            decideInitialVisibility();
            reportBackendDeath(); // 后端已经答话了，可以补告警
        }
    } catch (e) {
        // 后端还没起来，按上一次的状态处理
    }
    return securityState;
}

// reportBackendDeath 守护进程被杀又被拉起来之后，由新的守护进程把这件事发出去。
// 必须等它重新起来才发得出去，所以放在状态轮询成功之后。
function reportBackendDeath() {
    if (!backendDiedUnexpectedly || backendDeathReported) return;
    backendDiedUnexpectedly = false;
    backendDeathReported = true;
    console.log('[心跳] 守护进程曾意外退出，已重启，正在补发告警');
    fetchAPIPost('/api/notify', { type: 'daemon_restarted' }).catch((e) => {
        console.error('[心跳] 补发告警失败:', e.message);
    });
}

// isSilent 是否静默本地 Windows 通知（只有启用密码保护后才生效）
function isSilent() {
    return !!(securityState.enabled && securityState.silent_local);
}

// isGated 托盘入口是否需要密码
function isGated() {
    return !!(securityState.enabled && securityState.lock_tray);
}

// decideInitialVisibility 启用密码保护且静默时，启动就缩在托盘里不露面。
// 主窗口以 show:false 创建，这里决定要不要放出来。
function decideInitialVisibility() {
    if (initialShowDone || !mainWindow || mainWindow.isDestroyed()) return;
    initialShowDone = true;
    if (isSilent()) {
        console.log('[静默] 已启用密码保护静默，启动后仅驻留托盘');
        return;
    }
    mainWindow.show();
}

// activateUnlockWindow 把解锁窗抢到前台。
// 隐私屏场景下前台刚被遮罩进程占着，Windows 前台锁可能拒掉第一次 focus，
// 切换 alwaysOnTop 再 focus 是最可靠的激活手段，所以连补几枪覆盖激活时序。
function activateUnlockWindow() {
    const kick = () => {
        if (!unlockWindow || unlockWindow.isDestroyed()) return;
        try {
            unlockWindow.setAlwaysOnTop(false);
            unlockWindow.setAlwaysOnTop(true, 'screen-saver');
            unlockWindow.moveTop();
            unlockWindow.focus();
            if (unlockWindow.webContents) unlockWindow.webContents.focus();
        } catch (e) { /* 窗口关闭竞态，忽略 */ }
    };
    kick();
    setTimeout(kick, 150);
    setTimeout(kick, 500);
}

// requireUnlock 弹出独立解锁小窗，等用户验证。
// 用独立窗口而不是主窗口里的弹层，是因为"显示主窗口"本身就要先验证。
async function requireUnlock(action) {
    // 启动最初几秒还没拿到安全状态，先补一次，别让人趁这个空档溜过去
    if (!securityStateLoaded) await refreshSecurityState();
    const needsPassword = action === 'privacy' ? securityState.enabled : isGated();
    if (!needsPassword) return true;

    // 已经开着一个解锁窗就聚焦它，不重复弹
    if (unlockWindow && !unlockWindow.isDestroyed()) {
        activateUnlockWindow();
        return unlockPending ? unlockPending.promise : false;
    }

    const result = new Promise((resolve) => {
        unlockPending = { resolve };

        unlockWindow = new BrowserWindow({
            width: UNLOCK_WINDOW_WIDTH,
            height: 240,
            frame: false,
            resizable: false,
            minimizable: false,
            maximizable: false,
            fullscreenable: false,
            alwaysOnTop: true,
            skipTaskbar: true,
            show: false,
            backgroundColor: '#fbfbfc',
            webPreferences: {
                nodeIntegration: false,
                contextIsolation: true,
                preload: path.join(__dirname, 'preload.js')
            }
        });

        // 密码框绝不能出现在远端画面里；隐私屏场景下还得压过全屏遮罩
        try {
            unlockWindow.setContentProtection(true);
            unlockWindow.setAlwaysOnTop(true, 'screen-saver');
        } catch (e) {
            console.error('解锁窗置顶/防抓屏设置失败:', e);
        }

        unlockWindow.loadFile('unlock.html', { query: { action } });
        unlockWindow.once('ready-to-show', () => {
            unlockWindow.show();
            activateUnlockWindow();
        });

        // 直接关掉窗口 = 取消，程序继续跑
        unlockWindow.on('closed', () => {
            unlockWindow = null;
            if (unlockPending) {
                const p = unlockPending;
                unlockPending = null;
                p.resolve(false);
            }
        });
    });
    unlockPending.promise = result;
    return result;
}

// privacyDisable 撤除遮罩。
// 后端认令牌也认密码；这里优先带上最近一次验证拿到的令牌，
// 省得同一个密码被重复校验、把失败次数算重。
async function privacyDisable(password) {
    return await fetchAPIPost('/api/privacy/disable', { password: password || '' },
        lastUnlockToken ? { 'X-Unlock-Token': lastUnlockToken } : {});
}

// handlePrivacyUnlockRequest 用户在遮罩下按了热键。
// 遮罩是全屏置顶的，所以解锁小窗必须：
//   1. alwaysOnTop 用 screen-saver 层级，压过遮罩
//   2. setContentProtection(true)，别让远端看到密码框
// 守护进程那边在 unlock_pending 期间也会暂停抢置顶，两头配合才稳。
let privacyUnlocking = false;
async function handlePrivacyUnlockRequest(cfg) {
    if (privacyUnlocking) return;
    privacyUnlocking = true;
    try {
        // 隐私屏的密码要求独立于“托盘操作需要密码”，以最新后端状态为准。
        await refreshSecurityState();
        const needPw = cfg && cfg.require_password && securityState.enabled;
        if (!needPw) {
            await privacyDisable('');
            return;
        }
        const ok = await requireUnlock('privacy');
        if (ok) {
            await privacyDisable('');
        } else {
            // 用户取消了，清掉标记，别让它每轮都重复弹
            await fetchAPIPost('/api/privacy/unlock-ack', {}).catch(() => {});
        }
    } catch (e) {
        console.error('隐私屏解锁失败:', e);
        await fetchAPIPost('/api/privacy/unlock-ack', {}).catch(() => {});
        dialog.showErrorBox('隐私屏解锁失败', `${e.message}\n请再次按解锁快捷键重试。`);
    } finally {
        privacyUnlocking = false;
    }
}

// finishUnlock 由解锁小窗的 IPC 调用，ok 为 true 表示验证通过
function finishUnlock(ok, token) {
    if (token) lastUnlockToken = token;
    const p = unlockPending;
    unlockPending = null;
    if (unlockWindow && !unlockWindow.isDestroyed()) {
        unlockWindow.destroy();
        unlockWindow = null;
    }
    if (p) p.resolve(!!ok);
}

function createWindow() {
    mainWindow = new BrowserWindow({
        width: 900,
        height: 700,
        icon: path.join(__dirname, 'icon.png'),
        resizable: false,
        frame: false,
        transparent: false,
        alwaysOnTop: true,
        // 先不显示：等拿到安全状态再决定要不要露面（静默模式下只驻留托盘）
        show: false,
        webPreferences: {
            nodeIntegration: false,
            contextIsolation: true,
            preload: path.join(__dirname, 'preload.js')
        }
    });

    if (process.platform === 'win32') {
        // 任务栏分组可能从应用标识读取旧图标，窗口 icon 并不覆盖重启图标资源。
        mainWindow.setAppDetails({
            appId: APP_USER_MODEL_ID,
            appIconPath: app.isPackaged ? process.execPath : path.join(__dirname, 'icon.ico'),
            appIconIndex: 0,
            relaunchCommand: app.isPackaged ? `"${process.execPath}"` : `"${process.execPath}" "${app.getAppPath()}"`,
            relaunchDisplayName: '远程知道了'
        });
    }

    mainWindow.loadFile('index.html');

    mainWindow.on('close', (e) => {
        if (!isQuitting) {
            e.preventDefault();
            if (mainWindow) mainWindow.hide();
        }
    });

    mainWindow.on('closed', () => {
        mainWindow = null;
    });
}

function createTray() {
    const assetsDir = path.join(__dirname, 'assets');
    const iconPath = path.join(assetsDir, 'tray-icon.png');

    try {
        if (fs.existsSync(iconPath)) {
            let image = nativeImage.createFromPath(iconPath);
            if (!image.isEmpty()) {
                image = image.resize({ width: 16, height: 16 });
                tray = new Tray(image);
            } else {
                tray = new Tray(nativeImage.createEmpty());
            }
        } else {
            tray = new Tray(nativeImage.createEmpty());
        }
    } catch (e) {
        try {
            tray = new Tray(nativeImage.createEmpty());
        } catch (e2) {
            console.log('无法创建托盘图标:', e2);
            return;
        }
    }

    tray.setToolTip('远程知道了 - 未检测到远程控制');

    const contextMenu = Menu.buildFromTemplate([
        {
            label: '显示主窗口', click: async () => {
                if (!await requireUnlock('show')) return;
                if (mainWindow) mainWindow.show();
            }
        },
        {
            label: '查看状态', click: async () => {
                if (!await requireUnlock('status')) return;
                try {
                    const status = await fetchAPI('/api/status');
                    if (status && mainWindow) {
                        const { dialog } = require('electron');
                        const msg = status.remote_active
                            ? `远程控制进行中\n开始时间: ${status.start_time}`
                            : '未检测到远程控制';
                        dialog.showMessageBox({
                            type: 'info',
                            title: '远程状态',
                            message: msg
                        });
                    }
                } catch (error) {
                    console.error('获取状态失败:', error);
                    if (mainWindow) {
                        const { dialog } = require('electron');
                        dialog.showMessageBox({
                            type: 'error',
                            title: '错误',
                            message: '无法获取远程状态: ' + error.message
                        });
                    }
                }
            }
        },
        { type: 'separator' },
        {
            label: '退出', click: async () => {
                if (!await requireUnlock('quit')) return;
                // 明说退出的后果：本机不再被监控，并且会往手机推一条提醒
                if (isGated()) {
                    const { response } = await dialog.showMessageBox({
                        type: 'warning',
                        buttons: ['取消', '确认退出'],
                        defaultId: 0,
                        cancelId: 0,
                        title: '远程知道了',
                        message: '确认退出？',
                        detail: '退出后本机将不再被监控，并会向你配置的通知渠道发送一条退出提醒。'
                    });
                    if (response !== 1) return;
                }
                isQuitting = true;
                app.quit();
            }
        }
    ]);

    tray.setContextMenu(contextMenu);
    tray.on('click', async () => {
        if (!mainWindow) return;
        if (mainWindow.isVisible()) {
            mainWindow.hide();
            return;
        }
        // 单击展开主窗口和菜单里的"显示主窗口"是同一件事，闸门也一样
        if (!await requireUnlock('show')) return;
        mainWindow.show();
    });
}

function updateTray(status) {
    if (!tray) return;

    const iconFile = status.remote_active ? 'tray-active.png' : 'tray-icon.png';
    const iconPath = path.join(__dirname, 'assets', iconFile);

    try {
        if (fs.existsSync(iconPath)) {
            let image = nativeImage.createFromPath(iconPath);
            if (!image.isEmpty()) {
                image = image.resize({ width: 16, height: 16 });
                tray.setImage(image);
            }
        }
    } catch (e) {
        console.error('更新托盘图标失败:', e);
    }

    let tooltip;
    if (status.remote_active) {
        let startTimeStr = '未知';
        if (status.start_time) {
            try {
                const date = new Date(status.start_time);
                startTimeStr = date.toLocaleString('zh-CN', {
                    year: 'numeric',
                    month: '2-digit',
                    day: '2-digit',
                    hour: '2-digit',
                    minute: '2-digit',
                    second: '2-digit',
                    hour12: false
                });
            } catch (e) {
                startTimeStr = status.start_time;
            }
        }
        tooltip = `远程控制进行中\n开始: ${startTimeStr}\n持续: ${status.duration || '-'}`;
    } else {
        tooltip = '远程知道了 - 安全';
    }
    tray.setToolTip(tooltip);
}

function setupIPC() {
    ipcMain.handle('getStatus', async () => {
        return await fetchAPI('/api/status');
    });

    ipcMain.handle('getHistory', async () => {
        return await fetchAPI('/api/history');
    });

    ipcMain.handle('getHistoryPaginated', async (event, page, pageSize, filters = {}) => {
        const params = new URLSearchParams({ page, pageSize });
        for (const key of ['start_from', 'start_to', 'min_duration', 'max_duration', 'keyword']) {
            if (filters && filters[key] !== undefined && filters[key] !== '') {
                params.set(key, String(filters[key]));
            }
        }
        return await fetchAPI(`/api/history?${params}`);
    });

    ipcMain.handle('getNotificationConfig', async () => {
        return await fetchAPI('/api/notification');
    });

    ipcMain.handle('saveNotificationConfig', async (event, config) => {
        return await fetchAPIPost('/api/notification', config);
    });

    ipcMain.handle('testNotification', async (event, config) => {
        return await fetchAPIPost('/api/notification/test', config);
    });

    ipcMain.handle('minimize', () => {
        if (mainWindow) mainWindow.hide();
    });

    ipcMain.handle('maximize', () => {
        if (mainWindow) {
            if (mainWindow.isMaximized()) {
                mainWindow.unmaximize();
            } else {
                mainWindow.maximize();
            }
        }
    });

    ipcMain.handle('close', () => {
        if (mainWindow) mainWindow.hide();
    });

    ipcMain.handle('openExternal', async (event, url) => {
        const { shell } = require('electron');
        await shell.openExternal(url);
    });

    ipcMain.handle('getStartupEnabled', () => {
        return getStartupEnabled();
    });

    ipcMain.handle('setStartupEnabled', (event, enabled) => {
        const success = setStartupEnabled(enabled);
        return { success };
    });

    ipcMain.handle('getDeviceName', async () => {
        return await fetchAPI('/api/device-name');
    });

    ipcMain.handle('saveDeviceName', async (event, name) => {
        return await fetchAPIPost('/api/device-name', { name });
    });

    // 检测规则版本管理
    ipcMain.handle('rulesGetVersion', async () => {
        return await fetchAPI('/api/rules/version');
    });

    ipcMain.handle('rulesCheck', async () => {
        return await fetchAPIPost('/api/rules/check', {});
    });

    ipcMain.handle('rulesApply', async () => {
        return await fetchAPIPost('/api/rules/apply', {});
    });

    ipcMain.handle('rulesRollback', async (event, version) => {
        return await fetchAPIPost('/api/rules/rollback', { version });
    });

    // 手工导入规则文件（面向内网/离线环境）
    ipcMain.handle('rulesUpload', async () => {
        const { canceled, filePaths } = await dialog.showOpenDialog({
            title: '选择规则文件 (rules.json)',
            filters: [{ name: 'JSON 规则文件', extensions: ['json'] }],
            properties: ['openFile']
        });
        if (canceled || !filePaths || filePaths.length === 0) {
            return { canceled: true };
        }
        let content;
        try {
            content = JSON.parse(fs.readFileSync(filePaths[0], 'utf8'));
        } catch (e) {
            throw new Error('规则文件不是有效的 JSON: ' + e.message);
        }
        return await fetchAPIPost('/api/rules/upload', content);
    });

    // 监控对象管理（图标化工具选择 + 快照录制生成规则）
    ipcMain.handle('toolsList', async () => {
        return await fetchAPI('/api/tools');
    });

    ipcMain.handle('toolsToggle', async (event, processName, toolName, enabled) => {
        return await fetchAPIPost('/api/tools/toggle', { processName, toolName, enabled });
    });

    ipcMain.handle('toolsSnapshot', async () => {
        return await fetchAPIPost('/api/tools/snapshot', {});
    });

    ipcMain.handle('toolsDiff', async () => {
        return await fetchAPIPost('/api/tools/diff', {});
    });

    ipcMain.handle('toolsAddCustom', async (event, tool) => {
        return await fetchAPIPost('/api/tools/custom', tool);
    });

    ipcMain.handle('toolsRemoveCustom', async (event, processName, toolName) => {
        return await fetchAPIPost('/api/tools/custom/remove', { processName, toolName });
    });

    ipcMain.handle('toolsGetRulesRaw', async () => {
        return await fetchAPI('/api/tools/rules');
    });

    ipcMain.handle('toolsSetRulesRaw', async (event, jsonText) => {
        return await fetchAPIPost('/api/tools/rules', { json: jsonText });
    });

    ipcMain.handle('toolsResetBuiltin', async () => {
        return await fetchAPIPost('/api/tools/rules/reset', {});
    });

    // ---- 密码保护 ----
    // ---------- 隐私屏 ----------

    ipcMain.handle('privacyGetStatus', async () => {
        return await fetchAPI('/api/privacy/status');
    });

    ipcMain.handle('privacyGetConfig', async () => {
        return await fetchAPI('/api/privacy/config');
    });

    // 这三个接口在被拒时会返回 4xx，fetchAPIPost 会因此 reject。
    // 统一转成 { success:false, error } 交给渲染进程：
    // "触发方式为关闭""请先验证密码"这类都是正常的业务拒绝，
    // 不该在主进程日志里刷成未捕获异常。
    const privacyCall = async (fn) => {
        try {
            return await fn();
        } catch (e) {
            return { success: false, error: e.message || String(e) };
        }
    };

    ipcMain.handle('privacySaveConfig', async (event, config) => {
        return await privacyCall(() => fetchAPIPost('/api/privacy/config', config,
            lastUnlockToken ? { 'X-Unlock-Token': lastUnlockToken } : {}));
    });

    ipcMain.handle('privacyEnable', async () => {
        return await privacyCall(() => fetchAPIPost('/api/privacy/enable', {}));
    });

    ipcMain.handle('privacyDisable', async (event, password) => {
        return await privacyCall(() => privacyDisable(password));
    });

    ipcMain.handle('securityGetState', async () => {
        const st = await fetchAPI('/api/security/state');
        if (st) securityState = st;
        return st;
    });

    ipcMain.handle('securitySetup', async (event, password) => {
        const r = await fetchAPIPost('/api/security/setup', { password });
        await refreshSecurityState();
        return r;
    });

    ipcMain.handle('securityVerify', async (event, password, action) => {
        const r = await fetchAPIPost('/api/security/verify', { password, action });
        if (r && r.token) lastUnlockToken = r.token;
        return r;
    });

    ipcMain.handle('securityChange', async (event, oldPassword, newPassword) => {
        return await fetchAPIPost('/api/security/change', {
            old_password: oldPassword,
            new_password: newPassword
        });
    });

    ipcMain.handle('securityDisable', async (event, password) => {
        const r = await fetchAPIPost('/api/security/disable', { password });
        await refreshSecurityState();
        return r;
    });

    // 改安全设置需要带解锁令牌；令牌过期就复用同一个解锁小窗再验一次。
    ipcMain.handle('securitySetOptions', async (event, opts) => {
        const post = () => fetchAPIPost('/api/security/options', opts,
            lastUnlockToken ? { 'X-Unlock-Token': lastUnlockToken } : {});
        try {
            const r = await post();
            await refreshSecurityState();
            return r;
        } catch (e) {
            if (!/验证密码/.test(e.message || '')) throw e;
            if (!await requireUnlock('options')) throw new Error('已取消');
            const r = await post();
            await refreshSecurityState();
            return r;
        }
    });

    ipcMain.handle('securityRegenerateRecovery', async (event, password) => {
        return await fetchAPIPost('/api/security/recovery/regenerate', { password });
    });

    ipcMain.handle('securityResetRequest', async () => {
        return await fetchAPIPost('/api/security/reset/request', {});
    });

    ipcMain.handle('securityResetConfirm', async (event, payload) => {
        return await fetchAPIPost('/api/security/reset/confirm', payload || {});
    });

    // ---- 解锁小窗专用 ----
    ipcMain.handle('unlockDone', (event, token) => {
        finishUnlock(true, token);
    });

    ipcMain.handle('unlockCancel', () => {
        finishUnlock(false, null);
    });

    ipcMain.handle('unlockResize', (event, height) => {
        if (!unlockWindow || unlockWindow.isDestroyed()) return;
        if (event.sender !== unlockWindow.webContents || !Number.isFinite(height)) return;
        const h = Math.max(160, Math.min(520, Math.round(height) + 34)); // 32px 自绘标题栏 + 2px 边框
        // Windows 下不可缩放窗口会把 min/max 锁在当前尺寸，直接 setSize 无法缩小。
        // 临时解除限制再调整内容区，随后恢复不可缩放；固定宽度避免 DPI 舍入累积。
        unlockWindow.setResizable(true);
        try {
            unlockWindow.setContentSize(UNLOCK_WINDOW_WIDTH, h);
        } finally {
            unlockWindow.setResizable(false);
        }
        unlockWindow.center();
    });

    ipcMain.handle('getAppVersion', () => {
        return app.getVersion();
    });

    ipcMain.handle('checkForUpdates', async () => {
        return new Promise((resolve) => {
            const https = require('https');
            const options = {
                hostname: 'api.github.com',
                path: '/repos/samwafgo/RemoteKnown/releases/latest',
                method: 'GET',
                headers: { 'User-Agent': 'RemoteKnown-App' }
            };

            const req = https.request(options, (res) => {
                let data = '';
                res.on('data', (chunk) => data += chunk);
                res.on('end', () => {
                    try {
                        if (res.statusCode === 200) {
                            const release = JSON.parse(data);
                            resolve({
                                tag_name: release.tag_name,
                                html_url: release.html_url,
                                body: release.body
                            });
                        } else {
                            resolve(null);
                        }
                    } catch (e) {
                        resolve(null);
                    }
                });
            });

            req.on('error', (e) => {
                console.error('Update check failed:', e);
                resolve(null);
            });
            req.end();
        });
    });
}
