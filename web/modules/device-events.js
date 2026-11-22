/* 设备快照和状态事件插件：管理设备列表状态，不处理传输连接细节。 */
(() => {
  'use strict';
  const bus = window.WanxiangWS;
  const dashboard = window.WanxiangDashboard;
  if (!bus || !dashboard) return;

  bus.registerHandler('device-state-events', message => [
    'device-list', 'device-online', 'device-offline', 'device-state', 'device-admin-state', 'device-adb-state',
  ].includes(message.type), message => {
    if (message.type === 'device-list') {
      const list = (message.devices || []).map(d => ({
        ...d,
        adbPort: (d && d.adbPort > 0) ? d.adbPort : 5555,
      }));
      dashboard.setDevices(list);
      dashboard.renderDevices();
      return true;
    }

    const list = dashboard.devices;
    const device = list.find(item => item.id === message.deviceId);
    if (!device) {
      dashboard.loadAll();
      return true;
    }

    switch (message.type) {
      case 'device-online':
        device.online = true;
        break;
      case 'device-offline':
        device.online = false;
        device.adbOnline = false;
        break;
      case 'device-admin-state':
        if (message.data?.state === 'disconnect') {
          device.enabled = false;
          device.online = false;
        }
        if (message.data?.state === 'enable') device.enabled = true;
        break;
      case 'device-state':
        if (message.data) {
          device.battery = message.data.battery ?? device.battery;
          device.rttMs = message.data.rttMs ?? device.rttMs;
          device.status = message.data.status ?? device.status;
          device.lastSeen = message.data.lastSeen ?? device.lastSeen;
        }
        break;
      case 'device-adb-state':
        if (message.data) {
          device.adbOnline = message.data.adbOnline ?? true;
          if (message.data.ip) device.adbIp = message.data.ip;
          if (message.data.port > 0) device.adbPort = message.data.port;
          else if (!device.adbPort) device.adbPort = 5555;
          if (message.data.adbMode) device.adbMode = message.data.adbMode;
        }
        break;
    }
    dashboard.renderDevices();
    return true;
  });
})();
