const path = require('path');

// 倒计时由守护进程负责；窗口只负责提示和取消当前这一次自动开启。
function createPrivacyPrompt({ BrowserWindow, ipcMain, post }) {
    let promptWindow = null;
    let current = null;
    let dismissedRequestID = null;

    function close() {
        current = null;
        const win = promptWindow;
        promptWindow = null;
        if (win && !win.isDestroyed()) win.destroy();
    }

    function publish() {
        if (promptWindow && !promptWindow.isDestroyed()) {
            promptWindow.webContents.send('privacyAutoUpdate', current);
        }
    }

    async function cancel() {
        const request = current;
        if (!request) return { success: false, error: '本次倒计时已结束' };
        try {
            await post('/api/privacy/auto', { request_id: request.request_id, action: 'cancel' });
            dismissedRequestID = request.request_id;
            if (current === request) close();
            return { success: true };
        } catch (e) {
            return { success: false, error: e.message };
        }
    }

    const isPrompt = event => promptWindow && !promptWindow.isDestroyed() &&
        event.sender === promptWindow.webContents;
    ipcMain.handle('privacyAutoGetState', event => isPrompt(event) ? current : null);
    ipcMain.handle('privacyAutoCancel', event => {
        if (!isPrompt(event)) return { success: false, error: '请求来源无效' };
        return cancel();
    });

    function sync(status) {
        if (!status.auto_pending || !status.auto_request_id) {
            close();
            return;
        }
        if (status.auto_request_id === dismissedRequestID) return;
        const request = {
            request_id: status.auto_request_id,
            deadline: status.auto_deadline
        };
        if (current && current.request_id === request.request_id) {
            // 一轮旧的状态请求可能晚于 shown 响应到达，不把倒计时拨回去。
            if (Date.parse(request.deadline) > Date.parse(current.deadline)) {
                current.deadline = request.deadline;
                publish();
            }
            return;
        }
        close();
        current = request;
        const win = new BrowserWindow({
            width: 440,
            height: 350,
            frame: false,
            resizable: false,
            minimizable: false,
            maximizable: false,
            fullscreenable: false,
            alwaysOnTop: true,
            skipTaskbar: false,
            show: false,
            backgroundColor: '#fbfbfc',
            webPreferences: {
                nodeIntegration: false,
                contextIsolation: true,
                preload: path.join(__dirname, 'preload.js')
            }
        });
        promptWindow = win;
        win.setAlwaysOnTop(true, 'screen-saver');
        win.once('ready-to-show', async () => {
            if (promptWindow !== win || !current) return;
            win.show();
            win.moveTop();
            win.focus();
            publish();
            try {
                // 从实际展示时给用户完整的十秒，只能由后端延长一次。
                const result = await post('/api/privacy/auto', {
                    request_id: request.request_id, action: 'shown'
                });
                if (promptWindow === win && result.status) sync(result.status);
            } catch (e) {
                // 可能已到期或会话已结束，交给下一轮状态同步关闭窗口。
                console.error('隐私屏倒计时展示确认失败:', e.message);
            }
        });
        win.on('close', event => {
            event.preventDefault();
            cancel().then(result => {
                if (!result.success && promptWindow === win) {
                    win.webContents.send('privacyAutoError', result.error);
                }
            });
        });
        win.on('closed', () => {
            if (promptWindow === win) {
                promptWindow = null;
                current = null;
            }
        });
        win.loadFile(path.join(__dirname, 'privacy-prompt.html'));
    }

    return { sync, close };
}

module.exports = { createPrivacyPrompt };
