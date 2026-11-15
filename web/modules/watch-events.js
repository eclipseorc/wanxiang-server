/* Watch / WebRTC 消息插件：watch 会话由现有页面提供媒体与 DOM 操作接口。 */
(() => {
  'use strict';
  const bus = window.WanxiangWS;
  const dashboard = window.WanxiangDashboard;
  if (!bus || !dashboard) return;

  const watchMessages = new Set([
    'watch-started', 'watch-ready', 'watch-denied', 'offer', 'candidate',
    'watch-ended', 'watch-stopped', 'error',
  ]);

  bus.registerHandler('watch-events', message => watchMessages.has(message.type), message => {
    const element = id => dashboard.element(id);
    switch (message.type) {
      case 'watch-started':
        dashboard.currentWatch = message.sessionId;
        element('watchCard').classList.remove('hidden');
        element('watchInfo').textContent = `会话 ${message.sessionId} · 设备 ${message.deviceId}`;
        element('watchNotice').textContent = '已请求设备端开始共享；等待设备使用者确认系统授权…';
        return true;
      case 'watch-ready':
        element('watchNotice').textContent = '设备已授权，正在建立视频连接…';
        return true;
      case 'watch-denied':
        element('watchNotice').textContent = '设备使用者拒绝了屏幕共享请求。';
        dashboard.toast('设备拒绝共享');
        return true;
      case 'offer':
        void dashboard.acceptOffer(message);
        return true;
      case 'candidate':
        void dashboard.addCandidate(message);
        return true;
      case 'watch-ended':
      case 'watch-stopped':
        dashboard.cleanupWatch(false);
        dashboard.toast('屏幕查看已结束');
        return true;
      case 'error':
        dashboard.toast(message.message || '服务器返回错误');
        return true;
      default:
        return false;
    }
  });
})();
