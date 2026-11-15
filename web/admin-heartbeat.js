(() => {
  'use strict';

  // The dashboard owns the WebSocket lifecycle; this script observes each socket
  // and sends an application-level ping independently of WebSocket control frames.
  let observedSocket = null;
  let nextPingAt = 0;
  const status = document.getElementById('connStatus');

  function observeSocket() {
    const socket = ws;
    if (!socket || socket === observedSocket) return;

    observedSocket = socket;
    nextPingAt = Date.now() + 25000;
    socket.addEventListener('message', event => {
      let message;
      try { message = JSON.parse(event.data); } catch { return; }
      if (message.type !== 'pong') return;

      const sentAt = Number(message.sentAt);
      const latency = Number.isFinite(sentAt) ? Math.max(0, Date.now() - sentAt) : null;
      const detail = latency === null ? 'pong' : `pong ${latency} ms`;
      if (status) status.textContent = `实时事件已连接 · ${detail}`;
      console.info('[admin-ws] received pong', { latencyMs: latency });
    });
    socket.addEventListener('open', () => {
      console.info('[admin-ws] connected');
      nextPingAt = Date.now();
    });
    socket.addEventListener('close', event => {
      console.info('[admin-ws] disconnected', { code: event.code, reason: event.reason });
    });
    socket.addEventListener('error', () => console.error('[admin-ws] connection error'));
  }

  setInterval(() => {
    observeSocket();
    if (!ws || ws.readyState !== WebSocket.OPEN || Date.now() < nextPingAt) return;

    const sentAt = Date.now();
    try {
      ws.send(JSON.stringify({ type: 'ping', sentAt }));
      console.info('[admin-ws] sent ping');
      nextPingAt = sentAt + 25000;
    } catch (error) {
      console.error('[admin-ws] failed to send ping', error);
      nextPingAt = Date.now() + 5000;
    }
  }, 500);
})();
