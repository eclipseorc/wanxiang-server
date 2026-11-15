/*
 * 管理端心跳插件：只依赖通用 WanxiangWS API，不直接持有 WebSocket。
 * 以后新增业务应以插件方式注册消息/命令处理器，避免在传输框架里堆业务分支。
 */
(() => {
  'use strict';

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
    const sentRequestId = wsAPI.sendCommand('ping', { sentAt }, { socket: activeSocket, requestId });
    if (!sentRequestId) {
      clearTimeout(timeout);
      pending.delete(requestId);
      return;
    }
    console.info('[admin-heartbeat] sent com/ping', { requestId });
  }

  // pong 是心跳插件负责的命令；其他 com 命令由各自插件处理。
  wsAPI.registerCommand('pong', message => {
    const requestId = message.requestId;
    const request = pending.get(requestId);
    if (!request) return;

    clearTimeout(request.timeout);
    pending.delete(requestId);
    lastPongAt = Date.now();
    const latency = Math.max(0, lastPongAt - request.sentAt);
    if (status) status.textContent = `实时已连接 · ping ${latency} ms`;
    console.info('[admin-heartbeat] received com/pong', { requestId, latencyMs: latency });
  });

  wsAPI.registerCommand('error', message => {
    console.warn('[admin-heartbeat] server command error', message.data);
    const request = pending.get(message.requestId);
    if (request) {
      clearTimeout(request.timeout);
      pending.delete(message.requestId);
    }
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
        // 超时后仍继续发心跳，方便恢复后重新显示 RTT。
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
