/* scrcpy-control.js — 远程控制模块
 * 通过 ADB 无线调试远程查看并操作设备屏幕。
 * 配对/连接走同源 HTTP（admin-server，复用全局 api()），
 * 视频/控制走独立的 WS（scrcpy-server :9004）。
 *
 * 性能与交互要点：
 *  1. 视频解码用 WebCodecs，输出走 rAF 队列 + 最新帧覆盖，避免主线程被
 *     逐帧同步 drawImage 阻塞（这是之前卡顿与「点击变长按」的主因之一）。
 *  2. 坐标换算严格按 canvas 显示尺寸等比映射到设备分辨率（canvas 用
 *     aspect-ratio 锁定比例，杜绝 CSS 拉伸导致的偏移）。
 *  3. 点击判定带 slop 阈值：按下后若位移小于阈值视为「点按」，不发送
 *     move 事件；只有超过阈值才按拖拽处理。这能避免手指/鼠标微抖产生
 *     down→move→up 序列被设备识别成长按。
 */
(() => {
  'use strict';
  const $ = id => document.getElementById(id);

  // scrcpy 的 WS 地址（独立于管理端的 WS，端口 9004）。
  const scrcpyWsBase = localStorage.getItem('scrcpyWsBase') ||
    `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.hostname}:9004`;

  let scrcpyWs = null;      // scrcpy 会话的 WebSocket
  let decoder = null;       // WebCodecs VideoDecoder
  let canvasCtx = null;
  let screenW = 0, screenH = 0;
  let pendingSpsPps = null; // 缓存的纯 SPS/PPS 帧，等 IDR 帧合并

  // —— 连接配置的记忆：按设备 deviceId 存到 localStorage，下次打开自动回填 ——
  const SCR_CFG_KEY = 'scrcpyConfigs';
  let currentDevice = null; // 当前正在控制的设备对象

  function loadSavedConfig(deviceId) {
    try {
      const all = JSON.parse(localStorage.getItem(SCR_CFG_KEY) || '{}');
      return all[deviceId] || null;
    } catch (_) { return null; }
  }

  function saveConfig(deviceId, cfg) {
    try {
      const all = JSON.parse(localStorage.getItem(SCR_CFG_KEY) || '{}');
      all[deviceId] = cfg;
      localStorage.setItem(SCR_CFG_KEY, JSON.stringify(all));
    } catch (_) {}
  }

  function currentCfg() {
    return {
      host: $('scrIp').value.trim(),
      pairPort: $('scrPairPort').value.trim(),
      connPort: $('scrConnPort').value.trim(),
    };
  }

  function persistCurrentCfg() {
    if (!currentDevice) return;
    const c = currentCfg();
    if (c.host || c.connPort) {
      saveConfig(currentDevice.id, { host: c.host, pairPort: c.pairPort, connPort: c.connPort });
    }
  }

  // —— 渲染队列：rAF 驱动，只画最新一帧，避免积压与主线程阻塞 ——
  let latestFrame = null;   // 最新待绘制的 VideoFrame
  let rafScheduled = false;
  let lastFrameTs = 0;
  const FRAME_INTERVAL = 1000 / 60; // 最多 60fps 上屏，过高也无益

  function status(msg, ok) {
    const el = $('scrStatus');
    if (!el) return;
    el.textContent = msg;
    el.classList.toggle('ok', !!ok);
    el.classList.toggle('err', ok === false);
  }

  // 配对/连接复用页面的 api()（同源 admin-server）。
  function adminApi(path, body) {
    return api(path, { method: 'POST', body: JSON.stringify(body) });
  }

  // 步骤 1：配对
  async function doPair() {
    const host = $('scrIp').value.trim();
    const pairPort = parseInt($('scrPairPort').value.trim(), 10);
    const code = $('scrPairCode').value.trim();
    if (!host || !pairPort || !code) { toast('请填写设备 IP、配对端口和配对码'); return; }
    status('正在配对…');
    try {
      await adminApi('/api/admin/scrcpy/pair', { host, pairPort, code });
      status('配对成功，请填写连接端口并连接', true);
      persistCurrentCfg();
    } catch (e) {
      status('配对失败：' + e.message, false);
    }
  }

  // 步骤 2：连接
  async function doConnect() {
    const host = $('scrIp').value.trim();
    const port = parseInt($('scrConnPort').value.trim(), 10);
    if (!host || !port) { toast('请填写设备 IP 和连接端口'); return; }
    status('正在连接…');
    try {
      await adminApi('/api/admin/scrcpy/connect', { host, port });
      status('ADB 连接成功，可开始控制', true);
      persistCurrentCfg();
    } catch (e) {
      status('连接失败：' + e.message, false);
    }
  }

  // 步骤 3：开始控制（直接连 scrcpy WS，服务端在握手时完成 prepare）
  function doStart() {
    const host = $('scrIp').value.trim();
    const port = parseInt($('scrConnPort').value.trim(), 10);
    if (!host || !port) { toast('请先完成配对与连接'); return; }
    const serial = `${host}:${port}`;
    status('正在启动远程会话…');
    persistCurrentCfg();
    connectScrcpyWs(serial);
  }

  // 建立 scrcpy 会话 WS：服务端在握手时完成 deploy/start/forward 并推视频。
  function connectScrcpyWs(serial) {
    const wsUrl = `${scrcpyWsBase}/api/scrcpy/ws?serial=${encodeURIComponent(serial)}`;
    const sock = new WebSocket(wsUrl);
    sock.binaryType = 'arraybuffer';
    scrcpyWs = sock;

    sock.onopen = () => { status('远程会话已建立，等待视频…', true); };
    sock.onmessage = async (event) => {
      if (typeof event.data === 'string') {
        try {
          const msg = JSON.parse(event.data);
          if (msg.type === 'codec') { initDecoder(msg.codec, msg.width, msg.height); }
          if (msg.type === 'error') { status('会话错误：' + msg.message, false); }
        } catch (_) {}
        return;
      }
      if (!decoder) return;
      try {
        const bytes = new Uint8Array(event.data);
        const isKey = hasIdr(bytes);
        if (isKey) {
          // IDR 关键帧：把缓存的 SPS/PPS 前置合并，确保解码器拿到参数集。
          if (pendingSpsPps && pendingSpsPps.length > 0) {
            const merged = new Uint8Array(pendingSpsPps.length + bytes.length);
            merged.set(pendingSpsPps, 0);
            merged.set(bytes, pendingSpsPps.length);
            decoder.decode(new EncodedVideoChunk({ type: 'key', timestamp: performance.now() * 1000, data: merged }));
            pendingSpsPps = null;
          } else {
            decoder.decode(new EncodedVideoChunk({ type: 'key', timestamp: performance.now() * 1000, data: event.data }));
          }
        } else if (isSpsPpsOnly(bytes)) {
          // 纯 SPS/PPS 帧（无图像数据）：缓存，等 IDR 帧前置合并。
          pendingSpsPps = bytes;
        } else {
          decoder.decode(new EncodedVideoChunk({ type: 'delta', timestamp: performance.now() * 1000, data: event.data }));
        }
      } catch (e) { console.warn('decode chunk failed', e); }
    };
    sock.onclose = () => { status('远程会话已断开'); stopRender(); };
    sock.onerror = () => { status('远程连接失败', false); };
  }

  // 初始化 WebCodecs 解码器，输出交给 rAF 队列上屏。
  function initDecoder(codec, width, height) {
    screenW = width; screenH = height;
    const canvas = $('scrCanvas');
    canvas.width = width; canvas.height = height;
    // 锁定显示比例，保证坐标换算精确（竖屏/横屏都等比）。
    canvas.style.setProperty('--scr-ratio', `${width} / ${height}`);
    canvasCtx = canvas.getContext('2d');
    // 切换到控制画面，隐藏配置区
    $('scrSetup').classList.add('hidden');
    $('scrStage').classList.remove('hidden');
    $('scrInfo').textContent = `分辨率 ${width}x${height} · ${codec}`;
    if (currentDevice) $('scrInfo').textContent = `${currentDevice.name || currentDevice.model || '设备'} · ${width}x${height} · ${codec}`;

    if (typeof VideoDecoder === 'undefined') {
      status('当前浏览器不支持 WebCodecs，无法解码视频', false);
      return;
    }
    try {
      decoder = new VideoDecoder({
        output: (frame) => {
          // 只保留最新一帧，rAF 回调时统一上屏，天然丢帧防积压。
          if (latestFrame) { try { latestFrame.close(); } catch (_) {} }
          latestFrame = frame;
          scheduleRender();
        },
        error: (e) => { console.warn('decode error:', e); status('解码错误：' + e.message, false); },
      });
      decoder.configure({
        // scrcpy 发的是 Annex-B 格式（带 00 00 00 01 起始码，SPS/PPS 在带内），
        // 因此用 avc3 前缀（in-band 参数集），而不是 avc1（需要 description）。
        codec: codec === 'h265' ? 'hev1.1.6.L93.B0' : 'avc3.640028',
        codedWidth: width,
        codedHeight: height,
      });
      status('视频已连接，可拖动画面控制设备', true);
    } catch (e) {
      status('解码器初始化失败：' + e.message, false);
    }
  }

  function scheduleRender() {
    if (rafScheduled) return;
    rafScheduled = true;
    requestAnimationFrame(() => {
      rafScheduled = false;
      if (!latestFrame || !canvasCtx) return;
      const now = performance.now();
      if (now - lastFrameTs < FRAME_INTERVAL) {
        // 还没到上屏节拍，让出主线程；但下一帧到来会再次调度。
        if (latestFrame) { scheduleRender(); }
        return;
      }
      lastFrameTs = now;
      canvasCtx.drawImage(latestFrame, 0, 0, latestFrame.displayWidth, latestFrame.displayHeight);
      latestFrame.close();
      latestFrame = null;
    });
  }

  // 判断 Annex-B H.264 帧是否含 IDR（nal_unit_type=5）。
  function hasIdr(bytes) {
    for (let i = 0; i + 4 < bytes.length; i++) {
      if (bytes[i] !== 0 || bytes[i + 1] !== 0) continue;
      let s;
      if (bytes[i + 2] === 1) s = 3;
      else if (bytes[i + 2] === 0 && bytes[i + 3] === 1) s = 4;
      else continue;
      const t = bytes[i + s] & 0x1F;
      if (t === 5) return true;
      i += s;
    }
    return false;
  }

  // 判断是否为「纯 SPS/PPS 帧」：只含参数集（type 7/8），不含图像 slice（type 1/5）。
  function isSpsPpsOnly(bytes) {
    let hasParam = false, hasImage = false;
    for (let i = 0; i + 4 < bytes.length; i++) {
      if (bytes[i] !== 0 || bytes[i + 1] !== 0) continue;
      let s;
      if (bytes[i + 2] === 1) s = 3;
      else if (bytes[i + 2] === 0 && bytes[i + 3] === 1) s = 4;
      else continue;
      const t = bytes[i + s] & 0x1F;
      if (t === 7 || t === 8) hasParam = true;
      if (t === 1 || t === 5) hasImage = true;
      i += s;
    }
    return hasParam && !hasImage;
  }

  // 发送控制事件。
  function sendControl(msg) {
    if (scrcpyWs && scrcpyWs.readyState === WebSocket.OPEN) {
      scrcpyWs.send(JSON.stringify(msg));
    }
  }

  // 把 canvas 上的坐标换算成设备真实坐标（严格等比，canvas 用 aspect-ratio 锁定比例）。
  function toDevice(e) {
    const canvas = $('scrCanvas');
    const rect = canvas.getBoundingClientRect();
    const x = (e.clientX - rect.left) / rect.width * screenW;
    const y = (e.clientY - rect.top) / rect.height * screenH;
    return {
      x: Math.round(Math.max(0, Math.min(screenW - 1, x))),
      y: Math.round(Math.max(0, Math.min(screenH - 1, y))),
    };
  }

  // 绑定 canvas 鼠标/触摸/滚轮/键盘事件。
  // 点击判定：按下后位移 < SLOP 视为点按（不发 move），避免被识别成长按。
  function bindControls() {
    const canvas = $('scrCanvas');
    const SLOP = 4; // 像素阈值（设备坐标），小于此位移不触发拖拽
    let down = false;
    let downX = 0, downY = 0;
    let dragging = false;

    canvas.addEventListener('pointerdown', (e) => {
      canvas.setPointerCapture(e.pointerId);
      const p = toDevice(e);
      down = true; dragging = false;
      downX = p.x; downY = p.y;
      sendControl({ type: 'touch', action: 'down', x: p.x, y: p.y, width: screenW, height: screenH });
      e.preventDefault();
    });
    canvas.addEventListener('pointermove', (e) => {
      if (!down) return;
      const p = toDevice(e);
      const dx = p.x - downX, dy = p.y - downY;
      if (!dragging && (dx * dx + dy * dy) < SLOP * SLOP) return; // 未超阈值：当作点按，静默
      dragging = true;
      sendControl({ type: 'touch', action: 'move', x: p.x, y: p.y, width: screenW, height: screenH });
    });
    canvas.addEventListener('pointerup', (e) => {
      if (!down) return;
      down = false;
      const p = toDevice(e);
      // 点按：up 落在原地；拖拽：up 落点即终点。
      sendControl({ type: 'touch', action: 'up', x: dragging ? p.x : downX, y: dragging ? p.y : downY, width: screenW, height: screenH });
      dragging = false;
    });
    canvas.addEventListener('pointercancel', () => {
      if (!down) return;
      down = false;
      sendControl({ type: 'touch', action: 'up', x: downX, y: downY, width: screenW, height: screenH });
      dragging = false;
    });
    canvas.addEventListener('wheel', (e) => {
      e.preventDefault();
      const p = toDevice(e);
      const vScroll = e.deltaY > 0 ? 1 : -1;
      sendControl({ type: 'scroll', x: p.x, y: p.y, width: screenW, height: screenH, hScroll: 0, vScroll: vScroll });
    }, { passive: false });
    canvas.tabIndex = 0;
    canvas.addEventListener('keydown', (e) => {
      const keymap = { Enter: 66, Escape: 4, ArrowLeft: 21, ArrowRight: 22, ArrowUp: 19, ArrowDown: 20, Home: 3, Tab: 61 };
      if (keymap[e.key] !== undefined) {
        e.preventDefault();
        sendControl({ type: 'key', action: 'down', key: keymap[e.key] });
        sendControl({ type: 'key', action: 'up', key: keymap[e.key] });
      } else if (e.key === 'Backspace') {
        e.preventDefault();
        sendControl({ type: 'key', action: 'down', key: 67 });
        sendControl({ type: 'key', action: 'up', key: 67 });
      } else if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
        sendControl({ type: 'text', text: e.key });
      }
    });
  }

  function stopRender() {
    if (latestFrame) { try { latestFrame.close(); } catch (_) {} latestFrame = null; }
    if (decoder) { try { decoder.close(); } catch (_) {} decoder = null; }
    if (scrcpyWs) { try { scrcpyWs.close(); } catch (_) {} scrcpyWs = null; }
    pendingSpsPps = null;
    $('scrStage').classList.add('hidden');
    $('scrSetup').classList.remove('hidden');
    if ($('scrStatus')) status('会话已结束，可重新连接');
  }

  // 打开远程控制抽屉，回填该设备已保存的连接配置。
  function openDrawer(device) {
    currentDevice = device || null;
    const drawer = $('scrDrawer');
    if (!drawer) return;
    // 回填配置
    const saved = device ? loadSavedConfig(device.id) : null;
    if (saved) {
      if (saved.host) $('scrIp').value = saved.host;
      if (saved.pairPort) $('scrPairPort').value = saved.pairPort;
      if (saved.connPort) $('scrConnPort').value = saved.connPort;
    }
    // 恢复到配置区初始状态
    $('scrStage').classList.add('hidden');
    $('scrSetup').classList.remove('hidden');
    if ($('scrStatus')) status(device ? `准备连接设备：${device.name || device.model || device.id.slice(0, 8)}` : '尚未连接设备');
    if (currentDevice) {
      $('scrInfo').textContent = currentDevice.name || currentDevice.model || '设备';
    }
    drawer.classList.remove('hidden');
  }

  function closeDrawer() {
    stopRender();
    const drawer = $('scrDrawer');
    if (drawer) drawer.classList.add('hidden');
  }

  function init() {
    const pairBtn = $('scrPairBtn');
    if (!pairBtn) return;
    pairBtn.onclick = doPair;
    $('scrConnBtn').onclick = doConnect;
    $('scrStartBtn').onclick = doStart;
    $('scrStopBtn').onclick = stopRender;
    if ($('scrCloseBtn')) $('scrCloseBtn').onclick = closeDrawer;
    if ($('scrDrawerBackdrop')) $('scrDrawerBackdrop').onclick = closeDrawer;
    // 暴露给主脚本：设备列表「远程控制」按钮调用
    window.openScrcpy = openDrawer;
    window.closeScrcpy = closeDrawer;
    bindControls();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
