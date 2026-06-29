const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('remoteAudit', {
    getStatus: () => ipcRenderer.invoke('getStatus'),
    getHistory: () => ipcRenderer.invoke('getHistory'),
    getHistoryPaginated: (page, pageSize) => ipcRenderer.invoke('getHistoryPaginated', page, pageSize),
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
    toolsToggle: (processName, enabled) => ipcRenderer.invoke('toolsToggle', processName, enabled),
    toolsSnapshot: () => ipcRenderer.invoke('toolsSnapshot'),
    toolsDiff: () => ipcRenderer.invoke('toolsDiff'),
    toolsAddCustom: (tool) => ipcRenderer.invoke('toolsAddCustom', tool),
    toolsRemoveCustom: (processName) => ipcRenderer.invoke('toolsRemoveCustom', processName),
    toolsGetRulesRaw: () => ipcRenderer.invoke('toolsGetRulesRaw'),
    toolsSetRulesRaw: (jsonText) => ipcRenderer.invoke('toolsSetRulesRaw', jsonText),
    toolsResetBuiltin: () => ipcRenderer.invoke('toolsResetBuiltin')
});
