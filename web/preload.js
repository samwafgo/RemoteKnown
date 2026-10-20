const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('remoteAudit', {
    getStatus: () => ipcRenderer.invoke('getStatus'),
    getHistory: () => ipcRenderer.invoke('getHistory'),
    getHistoryPaginated: (page, pageSize, filters) => ipcRenderer.invoke('getHistoryPaginated', page, pageSize, filters),
    getNotificationConfig: () => ipcRenderer.invoke('getNotificationConfig'),
    saveNotificationConfig: (config) => ipcRenderer.invoke('saveNotificationConfig', config),
    testNotification: (config) => ipcRenderer.invoke('testNotification', config),
    onStatusUpdate: (callback) => {
        ipcRenderer.on('statusUpdate', (event, data) => callback(data));
    },
    minimize: () => ipcRenderer.invoke('minimize'),
    maximize: () => ipcRenderer.invoke('maximize'),
    close: () => ipcRenderer.invoke('close'),
    openExternal: (url) => ipcRenderer.invoke('openExternal', url),
    getAppVersion: () => ipcRenderer.invoke('getAppVersion'),
    checkForUpdates: () => ipcRenderer.invoke('checkForUpdates'),
    getStartupEnabled: () => ipcRenderer.invoke('getStartupEnabled'),
    setStartupEnabled: (enabled) => ipcRenderer.invoke('setStartupEnabled', enabled),
    getDeviceName: () => ipcRenderer.invoke('getDeviceName'),
    saveDeviceName: (name) => ipcRenderer.invoke('saveDeviceName', name),
    rulesGetVersion: () => ipcRenderer.invoke('rulesGetVersion'),
    rulesCheck: () => ipcRenderer.invoke('rulesCheck'),
    rulesApply: () => ipcRenderer.invoke('rulesApply'),
    rulesRollback: (version) => ipcRenderer.invoke('rulesRollback', version),
    rulesUpload: () => ipcRenderer.invoke('rulesUpload'),
    // 监控对象管理
    toolsList: () => ipcRenderer.invoke('toolsList'),
    toolsToggle: (processName, toolName, enabled) => ipcRenderer.invoke('toolsToggle', processName, toolName, enabled),
    toolsSnapshot: () => ipcRenderer.invoke('toolsSnapshot'),
    toolsDiff: () => ipcRenderer.invoke('toolsDiff'),
    toolsAddCustom: (tool) => ipcRenderer.invoke('toolsAddCustom', tool),
    toolsRemoveCustom: (processName, toolName) => ipcRenderer.invoke('toolsRemoveCustom', processName, toolName),
    toolsGetRulesRaw: () => ipcRenderer.invoke('toolsGetRulesRaw'),
    toolsSetRulesRaw: (jsonText) => ipcRenderer.invoke('toolsSetRulesRaw', jsonText),
    toolsResetBuiltin: () => ipcRenderer.invoke('toolsResetBuiltin'),
    // 密码保护
    securityGetState: () => ipcRenderer.invoke('securityGetState'),
    securitySetup: (password) => ipcRenderer.invoke('securitySetup', password),
    securityVerify: (password, action) => ipcRenderer.invoke('securityVerify', password, action),
    securityChange: (oldPassword, newPassword) => ipcRenderer.invoke('securityChange', oldPassword, newPassword),
    securityDisable: (password) => ipcRenderer.invoke('securityDisable', password),
    securitySetOptions: (opts) => ipcRenderer.invoke('securitySetOptions', opts),
    securityRegenerateRecovery: (password) => ipcRenderer.invoke('securityRegenerateRecovery', password),
    securityResetRequest: () => ipcRenderer.invoke('securityResetRequest'),
    securityResetConfirm: (payload) => ipcRenderer.invoke('securityResetConfirm', payload),
    // 隐私屏
    privacyGetStatus: () => ipcRenderer.invoke('privacyGetStatus'),
    privacyGetConfig: () => ipcRenderer.invoke('privacyGetConfig'),
    privacySaveConfig: (config) => ipcRenderer.invoke('privacySaveConfig', config),
    privacyEnable: () => ipcRenderer.invoke('privacyEnable'),
    privacyAutoGetState: () => ipcRenderer.invoke('privacyAutoGetState'),
    privacyAutoCancel: () => ipcRenderer.invoke('privacyAutoCancel'),
    onPrivacyAutoUpdate: (callback) => {
        ipcRenderer.on('privacyAutoUpdate', (event, data) => callback(data));
    },
    onPrivacyAutoError: (callback) => {
        ipcRenderer.on('privacyAutoError', (event, error) => callback(error));
    },
    privacyDisable: (password) => ipcRenderer.invoke('privacyDisable', password),
    onPrivacyUpdate: (callback) => {
        ipcRenderer.on('privacyUpdate', (event, data) => callback(data));
    },
    // 解锁小窗
    unlockDone: (token) => ipcRenderer.invoke('unlockDone', token),
    unlockCancel: () => ipcRenderer.invoke('unlockCancel'),
    unlockResize: (height) => ipcRenderer.invoke('unlockResize', height)
});
