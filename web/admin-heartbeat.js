/*
 * 管理端心跳插件：通过 WanxiangWS 发送和接收命令，不直接操作 socket。
 * 这与设备业务命令分离；未来可以独立替换为监控、状态查询等插件。
 */
(() => {
  'use strict';

  if (!window.WanxiangWS) {
    console.error('[admin-heartbeat] WebSocket framework unavailable');
    return;
  }
  const wsAPI = window.WanxiangWS;
  const status = document.getElementById('connStatus');
  const heartbeatInterval = 25000;
  const responseTimeout = 10000;
  const pending = new Map();
  let timer = null;
  let sequence = 0;
  let activeSocket = null;
  let lastPongAt = 0;

  function clearPending() {
    for (const item of pending.values()) clearTimeout(item.timeout);
    pending.clear();
  }

  function sendPing() {
    if (!activeSocket || activeSocket.readyState !== WebSocket.OPEN) return;
    const sentAt = Date.now();
    const requestId = `ping-${sentAt}-${++sequence}`;
    const timeout = setTimeout(() => {
      pending.delete(requestId);
      if (status && activeSocket) status.textContent = '实时已连接 · ping 超时';
      console.warn('[admin-heartbeat] ping timeout', { requestId });
    }, responseTimeout);

    pending.set(requestId, { sentAt, timeout });
    if (!wsAPI.sendCommand('ping', { sentAt }, { socket: activeSocket, requestId })) {
      clearTimeout(timeout);
      pending.delete(requestId);
      return;
    }
    console.info('[admin-heartbeat] sent com/ping', { requestId });
  }

  wsAPI.registerCommand('pong', message => {
    const request = pending.get(message.requestId);
    if (!request) return true;
    clearTimeout(request.timeout);
    pending.delete(message.requestId);
    lastPongAt = Date.now();
    const latency = Math.max(0, lastPongAt - request.sentAt);
    if (status) status.textContent = `实时已连接 · ping ${latency} ms`;
    console.info('[admin-heartbeat] received com/pong', { requestId: message.requestId, latencyMs: latency });
    return true;
  });

  wsAPI.registerCommand('error', message => {
    const request = pending.get(message.requestId);
    if (request) {
      clearTimeout(request.timeout);
      pending.delete(message.requestId);
    }
    const detail = message.data?.message || '命令执行失败';
    console.warn('[admin-heartbeat] command error', message.data);
    // 命令错误不会进入 legacy 消息处理器，必须由插件自己给出可见提示。
    if (window.WanxiangDashboard) window.WanxiangDashboard.toast(detail);
    return true;
  });

  wsAPI.onConnectionChange(({ socket, state }) => {
    if (state === 'open') {
      activeSocket = socket;
      lastPongAt = 0;
      if (timer) clearInterval(timer);
      if (status) status.textContent = '实时已连接 · 正在检测';
      sendPing();
      timer = setInterval(() => {
        if (!activeSocket || activeSocket.readyState !== WebSocket.OPEN) return;
        const hasPending = [...pending.values()].some(item => Date.now() - item.sentAt < responseTimeout);
        if (!hasPending && Date.now() - (lastPongAt || 0) >= heartbeatInterval) sendPing();
      }, 1000);
      return;
    }
    if (socket === activeSocket && (state === 'close' || state === 'error')) {
      activeSocket = null;
      if (timer) clearInterval(timer);
      timer = null;
      clearPending();
      if (status) status.textContent = state === 'close' ? '实时连接已断开' : '实时连接异常';
    }
  });
})();
