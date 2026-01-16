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
    checkForUpdates: () => ipcRenderer.invoke('checkForUpdates')
});
