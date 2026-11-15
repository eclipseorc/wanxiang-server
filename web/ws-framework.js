/*
 * 万象管理端 WebSocket 客户端骨架
 *
 * 职责分层：
 *   1. Transport：包装浏览器 WebSocket，管理连接状态和 JSON 收发；
 *   2. Message bus：将入站消息分发给注册的业务处理器；
 *   3. Command API：提供通用 com 命令发送入口；
 *   4. Plugin API：业务通过 registerHandler/registerCommand 接入，不改传输层。
 *
 * 本文件不处理设备列表、watch 等具体业务。它在 index.html 的内联脚本之前加载，
 * 通过代理 WebSocket 构造函数接入现有连接创建逻辑，因此不改变 ticket 和 URL 流程。
 */
(() => {
  'use strict';

  const NativeWebSocket = window.WebSocket;
  const handlers = new Map();
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

  function emitConnection(socket, state) {
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
    for (const listener of connectionListeners) {
      try { listener(event); } catch (error) { console.error('[ws-framework] connection listener failed', error); }
    }
  }

  function dispatch(message, socket) {
    if (!message || typeof message !== 'object' || Array.isArray(message)) return;

    for (const entry of handlers.values()) {
      let matches = false;
      try { matches = entry.match(message); } catch (error) {
        console.error(`[ws-framework] matcher failed (${entry.name})`, error);
      }
      if (!matches) continue;
      try { entry.handle(message, { socket, api }); } catch (error) {
        console.error(`[ws-framework] handler failed (${entry.name})`, error);
      }
    }

    if (message.type === 'com' && typeof message.command === 'string') {
      const commandHandler = commandHandlers.get(message.command);
      if (commandHandler) {
        try { commandHandler(message, { socket, api }); } catch (error) {
          console.error(`[ws-framework] command handler failed (${message.command})`, error);
        }
      }
    }
  }

  function attach(socket, url) {
    if (!isAdminSocket(url)) return;
    adminSockets.add(socket);

    // 监听器以 addEventListener 注册，不覆盖页面原有的 onmessage 业务处理。
    socket.addEventListener('open', () => {
      console.info('[ws-framework] management socket open');
      emitConnection(socket, 'open');
    });
    socket.addEventListener('message', event => {
      let message;
      try { message = JSON.parse(event.data); } catch (error) {
        console.warn('[ws-framework] ignored non-JSON message', error);
        return;
      }
      dispatch(message, socket);
    });
    socket.addEventListener('close', event => {
      adminSockets.delete(socket);
      console.info('[ws-framework] management socket closed', { code: event.code, reason: event.reason });
      emitConnection(socket, 'close');
    });
    socket.addEventListener('error', event => {
      console.error('[ws-framework] management socket error', event);
      emitConnection(socket, 'error');
    });

    // 统一记录应用主动发送的 JSON 消息；原生 send 仍负责真正传输。
    const nativeSend = socket.send.bind(socket);
    socket.send = data => {
      try {
        if (typeof data === 'string') {
          const parsed = JSON.parse(data);
          console.debug('[ws-framework] outbound', parsed);
        }
      } catch {
        // WebSocket 允许发送非 JSON 数据；框架只观察 JSON 协议，不阻止原始数据。
      }
      return nativeSend(data);
    };

    if (socket.readyState === NativeWebSocket.OPEN) emitConnection(socket, 'open');
  }

  // WebSocket Proxy 保留浏览器构造函数的静态常量和原型，只在管理端连接上挂载框架。
  const ManagedWebSocket = new Proxy(NativeWebSocket, {
    construct(target, args, newTarget) {
      const socket = Reflect.construct(target, args, newTarget);
      attach(socket, args[0]);
      return socket;
    },
  });
  window.WebSocket = ManagedWebSocket;

  const api = {
    // 注册通用消息处理器；返回取消注册函数，便于页面卸载时清理。
    registerHandler(name, match, handle) {
      if (typeof name !== 'string' || typeof match !== 'function' || typeof handle !== 'function') {
        throw new TypeError('registerHandler(name, match, handle) requires functions');
      }
      handlers.set(name, { name, match, handle });
      return () => handlers.delete(name);
    },

    // 注册 com 子命令处理器，例如 registerCommand('pong', handler)。
    registerCommand(command, handle) {
      if (typeof command !== 'string' || typeof handle !== 'function') {
        throw new TypeError('registerCommand(command, handle) requires a function');
      }
      commandHandlers.set(command, handle);
      return () => commandHandlers.delete(command);
    },

    // 监听管理端连接变化；注册时立即通知当前已打开的连接。
    onConnectionChange(listener) {
      connectionListeners.add(listener);
      for (const socket of adminSockets) {
        if (socket.readyState === NativeWebSocket.OPEN) emitConnection(socket, 'open');
      }
      return () => connectionListeners.delete(listener);
    },

    // 发一条通用协议消息；不让业务插件直接访问 socket 全局变量。
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

    // 统一封装 com 子命令信封，payload 为该命令自己的 data 内容。
    sendCommand(command, data, options = {}) {
      const requestId = options.requestId || `req-${Date.now()}-${++sequence}`;
      const sent = this.send({ type: 'com', command, requestId, data }, options.socket || null);
      return sent ? requestId : null;
    },

    getState() {
      return [...adminSockets].map(socket => ({
        url: socket.url,
        readyState: socket.readyState,
        bufferedAmount: socket.bufferedAmount,
      }));
    },
  };

  Object.defineProperty(window, 'WanxiangWS', {
    value: Object.freeze(api),
    configurable: false,
    enumerable: false,
    writable: false,
  });
})();
