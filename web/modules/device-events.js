/* 设备快照和状态事件插件：管理设备列表状态，不处理传输连接细节。 */
(() => {
  'use strict';
  const bus = window.WanxiangWS;
  const dashboard = window.WanxiangDashboard;
  if (!bus || !dashboard) return;

  bus.registerHandler('device-state-events', message => [
    'device-list', 'device-online', 'device-offline', 'device-state', 'device-admin-state',
  ].includes(message.type), message => {
    if (message.type === 'device-list') {
      dashboard.setDevices(message.devices || []);
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
    }
    dashboard.renderDevices();
    return true;
  });
})();
