const { app, BrowserWindow, Tray, Menu, ipcMain, nativeImage, Notification } = require('electron');
const { spawn } = require('child_process');
const path = require('path');
const http = require('http');
const fs = require('fs');

// 设置 AppUserModelId，修复 Windows 通知标题和图标问题
// 必须与 package.json build.appId 一致
app.setAppUserModelId('com.remoteknown.app');

const API_BASE = 'http://127.0.0.1:18080';

let mainWindow;
let tray = null;
let isQuitting = false;
let backendProcess = null;
let lastRemoteStatus = false; // 记录上一次的远程状态
let currentSessionStartTime = null; // 记录当前会话开始时间

// --- 单实例锁逻辑 ---
const gotTheLock = app.requestSingleInstanceLock();

if (!gotTheLock) {
    app.quit();
} else {
    app.on('second-instance', (event, commandLine, workingDirectory) => {
        // 当运行第二个实例时,将焦点集中到主窗口
        if (mainWindow) {
            if (mainWindow.isMinimized()) mainWindow.restore();
            mainWindow.show();
            mainWindow.focus();
        }
    });

    // 只有获取到锁的实例才执行初始化
    app.whenReady().then(() => {
        createWindow();
        createTray();
        startBackend();
        setupIPC();

        // 定时轮询状态
        setInterval(async () => {
            if (mainWindow && !mainWindow.isDestroyed()) {
                try {
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

                                new Notification({
                                    title: '警告：正在被远程控制',
                                    body: `软件: ${tools}\n时间: ${startTimeStr}`,
                                    icon: path.join(__dirname, 'assets', 'icon.png'),
                                    urgent: true
                                }).show();
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

                                new Notification({
                                    title: '安全：远程控制已结束',
                                    body: `远程会话已结束。\n持续时长: ${durationStr}`,
                                    icon: path.join(__dirname, 'assets', 'icon.png')
                                }).show();
                            }
                            lastRemoteStatus = status.remote_active;
                        }

                        mainWindow.webContents.send('statusUpdate', status);
                        updateTray(status);
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
        // 如果已经处理过退出，直接退出
        if (isQuitting) {
            console.log('[退出] 已经处理过退出，直接退出');
            return;
        }
        
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
            if (backendProcess) {
                backendProcess.kill();
                backendProcess = null;
                console.log('[退出] 后端进程已终止');
            }
            // 真正退出应用
            console.log('[退出] 调用 app.exit(0)');
            app.exit(0);
        }).catch((error) => {
            // 即使失败也要退出
            const elapsed = Date.now() - startTime;
            console.error(`[退出] 发送退出通知失败 (耗时: ${elapsed}ms):`, error);
            if (backendProcess) {
                backendProcess.kill();
                backendProcess = null;
                console.log('[退出] 后端进程已终止（错误情况下）');
            }
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

function startBackend() {
    if (backendProcess) return;

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
        backendProcess = spawn(backendPath, [], {
            detached: false, // 不分离，这样父进程退出子进程也会收到信号（虽然我们手动处理了 will-quit）
            stdio: 'ignore',
            windowsHide: true
        });

        backendProcess.on('error', (err) => {
            console.error('Backend process error:', err);
        });

        backendProcess.on('close', (code) => {
            console.log(`Backend process exited with code ${code}`);
            backendProcess = null;
            if (!isQuitting) {
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

function fetchAPIPost(endpoint, data) {
    return new Promise((resolve, reject) => {
        const postData = JSON.stringify(data);
        const options = {
            hostname: '127.0.0.1',
            port: 18080,
            path: endpoint,
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Content-Length': Buffer.byteLength(postData)
            }
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

function createWindow() {
    mainWindow = new BrowserWindow({
        width: 900,
        height: 700,
        resizable: false,
        frame: false,
        transparent: false,
        alwaysOnTop: true,
        webPreferences: {
            nodeIntegration: false,
            contextIsolation: true,
            preload: path.join(__dirname, 'preload.js')
        }
    });

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
        { label: '显示主窗口', click: () => mainWindow && mainWindow.show() },
        {
            label: '查看状态', click: async () => {
                try {
                    const status = await fetchAPI('/api/status');
                    if (status && mainWindow) {
                        const { dialog } = require('electron');
                        const msg = status.remote_active
                            ? `远程控制进行中\n开始时间: ${status.start_time}`
                            : '未检测到远程控制';
                        dialog.showMessageBox(mainWindow, {
                            type: 'info',
                            title: '远程状态',
                            message: msg
                        });
                    }
                } catch (error) {
                    console.error('获取状态失败:', error);
                    if (mainWindow) {
                        const { dialog } = require('electron');
                        dialog.showMessageBox(mainWindow, {
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
            label: '退出', click: () => {
                isQuitting = true;
                app.quit();
            }
        }
    ]);

    tray.setContextMenu(contextMenu);
    tray.on('click', () => {
        if (mainWindow) {
            if (mainWindow.isVisible()) {
                mainWindow.hide();
            } else {
                mainWindow.show();
            }
        }
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

    ipcMain.handle('getHistoryPaginated', async (event, page, pageSize) => {
        return await fetchAPI(`/api/history?page=${page}&pageSize=${pageSize}`);
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
