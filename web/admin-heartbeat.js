(() => {
  'use strict';

  // 观察管理页面创建的 WebSocket，并发送应用层 com/ping 命令。
  let observedSocket = null;
  let nextPingAt = 0;
  let requestSequence = 0;
  const pending = new Map();
  const status = document.getElementById('connStatus');

  // 给当前连接注册消息、断开和错误监听；页面重连后会重新绑定。
  function observeSocket() {
    const socket = ws;
    if (!socket || socket === observedSocket) return;

    observedSocket = socket;
    pending.clear();
    nextPingAt = Date.now() + 25000;

    socket.addEventListener('message', event => {
      let message;
      try { message = JSON.parse(event.data); } catch { return; }
      if (message.type !== 'com') return;

      if (message.command === 'pong') {
        const sentAt = Number(message.data?.sentAt);
        const key = message.requestId;
        const request = pending.get(key);
        if (!request) return;
        clearTimeout(request.timeout);
        pending.delete(key);
        const latency = Number.isFinite(sentAt) ? Math.max(0, Date.now() - sentAt) : Date.now() - request.sentAt;
        if (status) status.textContent = `实时事件已连接 · ping ${latency} ms`;
        console.info('[admin-ws] 收到 com/pong', { requestId: key, latencyMs: latency });
      } else if (message.command === 'error') {
        console.warn('[admin-ws] 命令错误', message.data);
      }
    });

    socket.addEventListener('open', () => {
      console.info('[admin-ws] 已连接');
      nextPingAt = Date.now();
    });
    socket.addEventListener('close', event => {
      pending.forEach(request => clearTimeout(request.timeout));
      pending.clear();
      if (status) status.textContent = '实时事件已断开';
      console.info('[admin-ws] 连接已关闭', { code: event.code, reason: event.reason });
    });
    socket.addEventListener('error', () => console.error('[admin-ws] 连接错误'));
  }

  // 发送一次 ping，并设置超时；requestId 用于匹配服务端的 pong。
  function sendPing(socket) {
    const sentAt = Date.now();
    const requestId = `ping-${sentAt}-${++requestSequence}`;
    const timeout = setTimeout(() => {
      pending.delete(requestId);
      if (status && socket === ws) status.textContent = '实时事件已连接 · ping 超时';
      console.warn('[admin-ws] com/ping 超时', { requestId });
    }, 10000);

    pending.set(requestId, { sentAt, timeout });
    try {
      socket.send(JSON.stringify({
        type: 'com',
        command: 'ping',
        requestId,
        data: { sentAt },
      }));
      console.info('[admin-ws] 已发送 com/ping', { requestId });
      nextPingAt = sentAt + 25000;
    } catch (error) {
      clearTimeout(timeout);
      pending.delete(requestId);
      console.error('[admin-ws] 发送 com/ping 失败', error);
      nextPingAt = Date.now() + 5000;
    }
  }

  // 每半秒检查连接状态，但仅在到达心跳间隔时才真正发送 ping。
  setInterval(() => {
    observeSocket();
    const socket = ws;
    if (!socket || socket.readyState !== WebSocket.OPEN || Date.now() < nextPingAt) return;
    sendPing(socket);
  }, 500);
})();
