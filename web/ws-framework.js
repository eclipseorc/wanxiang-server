/*
 * 万象管理端 WebSocket 客户端骨架。
 *
 * 本层只处理通用能力：连接观察、JSON 收发、消息路由和命令注册。
 * 页面业务由外部插件注册；设备管理和 watch 等领域逻辑不写入这里。
 */
(() => {
  'use strict';

  const NativeWebSocket = window.WebSocket;
  const messageHandlers = new Map();
  const commandHandlers = new Map();
  const connectionListeners = new Set();
  const adminSockets = new Set();
  let sequence = 0;

  function isAdminSocket(url) {
    try {
      return new URL(url, window.location.href).pathname === '/ws/admin';
    } catch {
      return false;
    }
  }

  function notifyConnection(socket, state, listener = null) {
    const event = {
      socket,
      state,
      send(message) {
        if (socket.readyState !== NativeWebSocket.OPEN) return false;
        try {
          socket.send(JSON.stringify(message));
          return true;
        } catch (error) {
          console.error('[ws-framework] send failed', error);
          return false;
        }
      },
    };
    const listeners = listener ? [listener] : [...connectionListeners];
    for (const callback of listeners) {
      try { callback(event); } catch (error) {
        console.error('[ws-framework] connection listener failed', error);
      }
    }
  }

  function dispatchMessage(message, socket = null) {
    if (!message || typeof message !== 'object' || Array.isArray(message)) return false;
    let handled = false;

    for (const entry of messageHandlers.values()) {
      let matches = false;
      try { matches = entry.match(message); } catch (error) {
        console.error(`[ws-framework] matcher failed (${entry.name})`, error);
      }
      if (!matches) continue;
      try {
        if (entry.handle(message, { socket, api }) === true) handled = true;
      } catch (error) {
        console.error(`[ws-framework] handler failed (${entry.name})`, error);
      }
    }

    if (message.type === 'com' && typeof message.command === 'string') {
      const callback = commandHandlers.get(message.command);
      if (callback) {
        try {
          if (callback(message, { socket, api }) === true) handled = true;
        } catch (error) {
          console.error(`[ws-framework] command handler failed (${message.command})`, error);
        }
      }
    }
    return handled;
  }

  function attach(socket, url) {
    if (!isAdminSocket(url)) return;
    adminSockets.add(socket);

    socket.addEventListener('open', () => {
      console.info('[ws-framework] management socket open');
      notifyConnection(socket, 'open');
    });
    socket.addEventListener('message', event => {
      // 这里只做协议可见性日志；路由由页面 bridge 调用 dispatchMessage，
      // 这样 legacy 页面处理器与插件不会对同一条响应各执行一遍。
      try { console.debug('[ws-framework] inbound', JSON.parse(event.data)); } catch {
        console.warn('[ws-framework] received non-JSON message');
      }
    });
    socket.addEventListener('close', event => {
      adminSockets.delete(socket);
      console.info('[ws-framework] management socket closed', { code: event.code, reason: event.reason });
      notifyConnection(socket, 'close');
    });
    socket.addEventListener('error', event => {
      console.error('[ws-framework] management socket error', event);
      notifyConnection(socket, 'error');
    });

    const nativeSend = socket.send.bind(socket);
    socket.send = data => {
      try {
        if (typeof data === 'string') console.debug('[ws-framework] outbound', JSON.parse(data));
      } catch {
        // 允许发送非 JSON 的 WebSocket 数据；此框架仅观察 JSON 协议。
      }
      return nativeSend(data);
    };

    if (socket.readyState === NativeWebSocket.OPEN) notifyConnection(socket, 'open');
  }

  const ManagedWebSocket = new Proxy(NativeWebSocket, {
    construct(target, args, newTarget) {
      const socket = Reflect.construct(target, args, newTarget);
      attach(socket, args[0]);
      return socket;
    },
  });
  window.WebSocket = ManagedWebSocket;

  const api = {
    registerHandler(name, match, handle) {
      if (typeof name !== 'string' || typeof match !== 'function' || typeof handle !== 'function') {
        throw new TypeError('registerHandler(name, match, handle) requires functions');
      }
      messageHandlers.set(name, { name, match, handle });
      return () => messageHandlers.delete(name);
    },

    registerCommand(command, handle) {
      if (typeof command !== 'string' || typeof handle !== 'function') {
        throw new TypeError('registerCommand(command, handle) requires a function');
      }
      commandHandlers.set(command, handle);
      return () => commandHandlers.delete(command);
    },

    onConnectionChange(listener) {
      connectionListeners.add(listener);
      for (const socket of adminSockets) {
        if (socket.readyState === NativeWebSocket.OPEN) notifyConnection(socket, 'open', listener);
      }
      return () => connectionListeners.delete(listener);
    },

    send(message, socket = null) {
      const candidates = socket ? [socket] : [...adminSockets];
      const active = candidates.find(item => item.readyState === NativeWebSocket.OPEN);
      if (!active) return false;
      try {
        active.send(JSON.stringify(message));
        return true;
      } catch (error) {
        console.error('[ws-framework] send failed', error);
        return false;
      }
    },

    sendCommand(command, data, options = {}) {
      const requestId = options.requestId || `req-${Date.now()}-${++sequence}`;
      const sent = this.send({ type: 'com', command, requestId, data }, options.socket || null);
      return sent ? requestId : null;
    },

    dispatchMessage,

    getState() {
      return [...adminSockets].map(socket => ({
        url: socket.url,
        readyState: socket.readyState,
        bufferedAmount: socket.bufferedAmount,
      }));
    },
  };

  Object.defineProperty(window, 'WanxiangWS', {
    value: Object.freeze(api), configurable: false, enumerable: false, writable: false,
  });
})();
