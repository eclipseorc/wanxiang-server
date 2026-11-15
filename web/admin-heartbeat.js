(() => {
  'use strict';

  // Observe the dashboard-owned socket and send application-level com/ping commands.
  let observedSocket = null;
  let nextPingAt = 0;
  let requestSequence = 0;
  const pending = new Map();
  const status = document.getElementById('connStatus');

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
        console.info('[admin-ws] received com/pong', { requestId: key, latencyMs: latency });
      } else if (message.command === 'error') {
        console.warn('[admin-ws] command error', message.data);
      }
    });

    socket.addEventListener('open', () => {
      console.info('[admin-ws] connected');
      nextPingAt = Date.now();
    });
    socket.addEventListener('close', event => {
      pending.forEach(request => clearTimeout(request.timeout));
      pending.clear();
      if (status) status.textContent = '实时事件已断开';
      console.info('[admin-ws] disconnected', { code: event.code, reason: event.reason });
    });
    socket.addEventListener('error', () => console.error('[admin-ws] connection error'));
  }

  function sendPing(socket) {
    const sentAt = Date.now();
    const requestId = `ping-${sentAt}-${++requestSequence}`;
    const timeout = setTimeout(() => {
      pending.delete(requestId);
      if (status && socket === ws) status.textContent = '实时事件已连接 · ping 超时';
      console.warn('[admin-ws] com/ping timed out', { requestId });
    }, 10000);

    pending.set(requestId, { sentAt, timeout });
    try {
      socket.send(JSON.stringify({
        type: 'com',
        command: 'ping',
        requestId,
        data: { sentAt },
      }));
      console.info('[admin-ws] sent com/ping', { requestId });
      nextPingAt = sentAt + 25000;
    } catch (error) {
      clearTimeout(timeout);
      pending.delete(requestId);
      console.error('[admin-ws] failed to send com/ping', error);
      nextPingAt = Date.now() + 5000;
    }
  }

  setInterval(() => {
    observeSocket();
    const socket = ws;
    if (!socket || socket.readyState !== WebSocket.OPEN || Date.now() < nextPingAt) return;
    sendPing(socket);
  }, 500);
})();
