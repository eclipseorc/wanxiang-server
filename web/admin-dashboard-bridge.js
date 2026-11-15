/*
 * 把现有页面的词法状态包装成小型业务 API，并将入站消息交给可插拔模块。
 * 模块消费的消息不再进入旧的大型 handleServerMessage；其他消息仍走旧逻辑，
 * 因而可以逐类迁移并保持未迁移功能继续工作。
 */
(() => {
  'use strict';

  const legacyHandler = handleServerMessage;
  window.WanxiangDashboard = Object.freeze({
    get devices() { return devices; },
    setDevices(value) { devices = Array.isArray(value) ? value : []; },
    renderDevices() { renderDevices(); },
    loadAll() { return loadAll(); },
    toast(message) { toast(message); },
    element(id) { return $(id); },
    get currentWatch() { return currentWatch; },
    set currentWatch(value) { currentWatch = value; },
    acceptOffer(message) { return acceptOffer(message); },
    addCandidate(message) { return addCandidate(message); },
    cleanupWatch(hide) { cleanupWatch(hide); },
  });

  handleServerMessage = async function (message) {
    if (window.WanxiangWS.dispatchMessage(message, ws)) return;
    return legacyHandler(message);
  };
})();
