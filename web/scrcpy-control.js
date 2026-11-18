/* scrcpy-control.js — 远程控制模块（多实例）
 * 每台设备一个独立的 ScrcpySession 实例，渲染成「仿手机」的独立浮动窗口，
 * 可拖拽、可缩放、可多开。配对/连接走同源 HTTP（admin-server，复用全局 api()），
 * 视频/控制走独立的 WS（scrcpy-server :9004）。
 *
 * 设计要点：
 *  1. 会话完全实例化：每个窗口拥有自己的 DOM、WS、VideoDecoder、canvas、状态，
 *     互不干扰，从而支持同时控制多台设备。
 *  2. 视频解码用 WebCodecs，输出走 rAF 队列 + 最新帧覆盖，避免主线程阻塞。
 *  3. 坐标换算严格按 canvas 显示尺寸等比映射到设备分辨率。
 *  4. 点击判定带 slop 阈值，避免微抖被识别成长按。
 *  5. 键盘输入走隐藏 textarea，支持中文 IME。
 */
(() => {
  'use strict';
  const $ = id => document.getElementById(id);

  // scrcpy 的 WS 地址（独立于管理端的 WS，端口 9004）。
  const scrcpyWsBase = localStorage.getItem('scrcpyWsBase') ||
    `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.hostname}:9004`;

  // 连接配置的记忆：按设备 deviceId 存到 localStorage，下次打开自动回填。
  const SCR_CFG_KEY = 'scrcpyConfigs';
  const FRAME_INTERVAL = 1000 / 60; // 最多 60fps 上屏

  // 正在打开的窗口实例，按 deviceId 去重（同设备只开一个窗口）。
  const sessions = new Map();

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

  // 配对/连接复用页面的 api()（同源 admin-server）。
  function adminApi(path, body) {
    return api(path, { method: 'POST', body: JSON.stringify(body) });
  }

  // —— Annex-B H.264 帧类型判断 ——
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

  // 手机窗口的配对/配置表单模板（每窗口独立 DOM，用 data-* 定位，不依赖全局 id）。
  function setupTemplate(device) {
    const name = device ? (device.name || device.model || '设备') : '设备';
    return `
      <div class="scr-win-setup">
        <p class="scr-setup-tip">通过 ADB 无线调试连接设备。设备需在「开发者选项 → 无线调试」中开启并显示配对信息。</p>
        <div class="scr-mode-toggle" data-mode-toggle>
          <button type="button" class="scr-mode-btn is-active" data-mode="wireless">无线调试模式</button>
          <button type="button" class="scr-mode-btn" data-mode="tcpip">传统模式</button>
        </div>
        <div class="scr-accordion" data-acc="wireless">
          <div class="acc-step" data-step="1">
            <button type="button" class="acc-head" data-acc-toggle>
              <span class="acc-no">1</span><span class="acc-title">配对设备</span>
              <span class="acc-badge" data-acc-badge></span>
              <svg class="ico ico-sm acc-chevron"><use href="#i-chevron-down"/></svg>
            </button>
            <div class="acc-body">
              <div class="acc-notice" data-acc-notice>填写无线调试页的配对端口与 6 位配对码。</div>
              <div class="scrcpy-form">
                <label>设备 IP<input data-f="ip" placeholder="如 192.168.1.100"></label>
                <label>配对端口<input data-f="pairPort" placeholder="如 35711"></label>
                <label>配对码<input data-f="pairCode" placeholder="6 位配对码"></label>
                <button type="button" data-act="pair"><svg class="ico ico-sm"><use href="#i-key"/></svg>配对</button>
              </div>
            </div>
          </div>
          <div class="acc-step" data-step="2">
            <button type="button" class="acc-head" data-acc-toggle>
              <span class="acc-no">2</span><span class="acc-title">建立连接</span>
              <span class="acc-badge" data-acc-badge></span>
              <svg class="ico ico-sm acc-chevron"><use href="#i-chevron-down"/></svg>
            </button>
            <div class="acc-body">
              <div class="acc-notice" data-acc-notice>配对成功后，用连接端口建立 ADB 连接。</div>
              <div class="scrcpy-form">
                <label>连接端口<input data-f="connPort" placeholder="如 35583"></label>
                <button type="button" data-act="connect"><svg class="ico ico-sm"><use href="#i-bolt"/></svg>连接</button>
              </div>
            </div>
          </div>
          <div class="acc-step" data-step="3">
            <button type="button" class="acc-head" data-acc-toggle>
              <span class="acc-no">3</span><span class="acc-title">开始控制</span>
              <span class="acc-badge" data-acc-badge></span>
              <svg class="ico ico-sm acc-chevron"><use href="#i-chevron-down"/></svg>
            </button>
            <div class="acc-body">
              <div class="acc-notice" data-acc-notice>连接就绪后，点击开始进入远程画面。</div>
              <button type="button" data-act="start" class="btn-primary btn-block"><svg class="ico ico-sm"><use href="#i-monitor"/></svg>开始控制</button>
            </div>
          </div>
        </div>
        <div class="scr-accordion hidden" data-acc="tcpip">
          <div class="scr-tcpip-form">
            <p class="scr-tcpip-hint">适用于没有「无线调试」菜单的设备。先用 USB 连接并开启 USB 调试，填序列号切换为无线，再拔线用 IP+端口连接。</p>
            <div class="scrcpy-form">
              <label>设备序列号（USB）
                <div class="scr-usb-serial">
                  <select data-f="usbSerial"><option value="">正在检测 USB 设备…</option></select>
                  <button type="button" class="scr-usb-refresh" data-act="usbRefresh" title="重新检测 USB 设备"><svg class="ico ico-sm"><use href="#i-refresh"/></svg></button>
                </div>
              </label>
              <label>端口<input data-f="tcpipPort" value="5555"></label>
              <button type="button" data-act="tcpipSwitch"><svg class="ico ico-sm"><use href="#i-bolt"/></svg>切换为无线</button>
            </div>
            <div class="acc-notice" data-tcpip-notice>切换成功后，用下方「设备 IP + 端口」建立连接。</div>
          </div>
          <div class="acc-step" data-step="1">
            <button type="button" class="acc-head" data-acc-toggle>
              <span class="acc-no">1</span><span class="acc-title">连接设备</span>
              <span class="acc-badge" data-acc-badge></span>
              <svg class="ico ico-sm acc-chevron"><use href="#i-chevron-down"/></svg>
            </button>
            <div class="acc-body">
              <div class="acc-notice" data-acc-notice>用手机 IP 与端口建立 ADB 连接（无需配对码）。</div>
              <div class="scrcpy-form">
                <label>设备 IP<input data-f="tcpipIp" placeholder="如 192.168.1.100"></label>
                <label>连接端口<input data-f="tcpipConnPort" value="5555"></label>
                <button type="button" data-act="tcpipConnect"><svg class="ico ico-sm"><use href="#i-bolt"/></svg>连接</button>
              </div>
            </div>
          </div>
          <div class="acc-step" data-step="2">
            <button type="button" class="acc-head" data-acc-toggle>
              <span class="acc-no">2</span><span class="acc-title">开始控制</span>
              <span class="acc-badge" data-acc-badge></span>
              <svg class="ico ico-sm acc-chevron"><use href="#i-chevron-down"/></svg>
            </button>
            <div class="acc-body">
              <div class="acc-notice" data-acc-notice>连接就绪后，点击开始进入远程画面。</div>
              <button type="button" data-act="start" class="btn-primary btn-block"><svg class="ico ico-sm"><use href="#i-monitor"/></svg>开始控制</button>
            </div>
          </div>
        </div>
      </div>`;
  }

  class ScrcpySession {
    constructor(device) {
      this.device = device || null;
      this.mode = 'wireless';      // 'wireless' | 'tcpip'
      this.ws = null;              // scrcpy 会话 WebSocket
      this.decoder = null;         // WebCodecs VideoDecoder
      this.ctx = null;             // canvas 2d context
      this.screenW = 0; this.screenH = 0;
      this.pendingSpsPps = null;
      this.latestFrame = null;
      this.rafScheduled = false;
      this.lastFrameTs = 0;
      this.lastError = null;
      this.alive = false;          // 会话是否存活
      this.serial = null;
      this.closed = false;         // 窗口是否已关闭

      this.buildWindow();
      this.bindWindow();
      this.bindControls();
      this.restore();
    }

    // —— DOM 构建 ——
    buildWindow() {
      const name = this.device ? (this.device.name || this.device.model || '设备') : '设备';
      const root = document.createElement('div');
      root.className = 'scr-window';
      root.innerHTML = `
        <div class="scr-window-bar" data-drag>
          <span class="scr-win-status" data-status-dot></span>
          <span class="scr-win-title">${name}</span>
          <button type="button" class="scr-win-close" data-close title="关闭">✕</button>
        </div>
        <div class="scr-window-body">
          ${setupTemplate(this.device)}
          <div class="screen-frame hidden" data-stage><canvas data-canvas></canvas></div>
          <textarea class="scr-kb-input" autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false" data-kb></textarea>
        </div>
        <div class="scr-win-status" data-status>准备连接…</div>
        <div class="scr-win-resize" data-resize></div>`;
      document.body.appendChild(root);
      this.el = root;
      this.setup = root.querySelector('.scr-win-setup');
      this.stage = root.querySelector('[data-stage]');
      this.canvas = root.querySelector('[data-canvas]');
      this.kb = root.querySelector('[data-kb]');
      this.statusEl = root.querySelector('[data-status]');
      this.statusDot = root.querySelector('[data-status-dot]');
      this.titleEl = root.querySelector('.scr-win-title');

      // 初始位置：错开摆放，避免多窗口完全重叠。
      const n = sessions.size;
      this.el.style.left = (80 + n * 40) + 'px';
      this.el.style.top = (60 + n * 36) + 'px';
    }

    // —— 字段/步骤辅助 ——
    field(name) { return this.el.querySelector(`[data-f="${name}"]`); }
    stepEl(n) {
      const acc = this.el.querySelector(`[data-acc="${this.mode}"]`);
      return acc ? acc.querySelector(`.acc-step[data-step="${n}"]`) : null;
    }

    setStepState(n, state) {
      const el = this.stepEl(n);
      if (!el) return;
      el.classList.remove('is-active', 'is-done', 'is-error', 'is-open');
      const badge = el.querySelector('[data-acc-badge]');
      badge.className = 'acc-badge';
      badge.innerHTML = '';
      if (state === 'done') {
        el.classList.add('is-done');
        badge.classList.add('badge-done');
        badge.innerHTML = '<svg class="ico ico-sm"><use href="#i-check"/></svg>已完成';
      } else if (state === 'error') {
        el.classList.add('is-error', 'is-open');
        badge.classList.add('badge-error');
        badge.innerHTML = '<svg class="ico ico-sm"><use href="#i-alert"/></svg>失败';
      } else if (state === 'locked') {
        badge.classList.add('badge-lock');
        badge.innerHTML = '<svg class="ico ico-sm"><use href="#i-lock"/></svg>待解锁';
      } else if (state === 'active') {
        el.classList.add('is-active', 'is-open');
        badge.classList.add('badge-lock');
        badge.textContent = '进行中';
      }
    }

    setStepNotice(n, text, type) {
      const el = this.stepEl(n);
      if (!el) return;
      const notice = el.querySelector('[data-acc-notice]');
      if (!notice) return;
      notice.classList.remove('is-ok', 'is-err');
      let icon = 'i-info';
      if (type === 'ok') { notice.classList.add('is-ok'); icon = 'i-check'; }
      else if (type === 'err') { notice.classList.add('is-err'); icon = 'i-alert'; }
      notice.innerHTML = `<svg class="ico ico-sm"><use href="#${icon}"/></svg><span>${text}</span>`;
    }

    status(msg, ok) {
      this.statusEl.textContent = msg;
      this.statusEl.classList.toggle('ok', !!ok);
      this.statusEl.classList.toggle('err', ok === false);
    }

    setMode(mode) {
      this.mode = mode;
      this.el.querySelectorAll('.scr-mode-btn').forEach((b) => {
        b.classList.toggle('is-active', b.dataset.mode === mode);
      });
      const w = this.el.querySelector('[data-acc="wireless"]');
      const t = this.el.querySelector('[data-acc="tcpip"]');
      if (w) w.classList.toggle('hidden', mode !== 'wireless');
      if (t) t.classList.toggle('hidden', mode !== 'tcpip');
      this.resetAccordion();
      // 切到传统模式时自动检测 USB 设备，免去用户手填序列号。
      if (mode === 'tcpip') this.loadUsbSerials();
    }

    resetAccordion() {
      if (this.mode === 'tcpip') {
        this.setStepState(1, 'active');
        this.setStepNotice(1, '用手机 IP 与端口建立 ADB 连接（无需配对码）。', 'info');
        this.setStepState(2, 'locked');
        this.setStepNotice(2, '连接就绪后，点击开始进入远程画面。', 'info');
        return;
      }
      this.setStepState(1, 'active');
      this.setStepNotice(1, '填写无线调试页的配对端口与 6 位配对码。', 'info');
      this.setStepState(2, 'locked');
      this.setStepNotice(2, '配对成功后，用连接端口建立 ADB 连接。', 'info');
      this.setStepState(3, 'locked');
      this.setStepNotice(3, '连接就绪后，点击开始进入远程画面。', 'info');
    }

    // —— 窗口交互：拖拽 + 缩放 + 关闭 ——
    bindWindow() {
      const bar = this.el.querySelector('[data-drag]');
      const resize = this.el.querySelector('[data-resize]');
      const close = this.el.querySelector('[data-close]');

      // 拖拽
      bar.addEventListener('pointerdown', (e) => {
        e.preventDefault();
        const startX = e.clientX, startY = e.clientY;
        const origL = this.el.offsetLeft, origT = this.el.offsetTop;
        const move = (ev) => {
          this.el.style.left = (origL + ev.clientX - startX) + 'px';
          this.el.style.top = (origT + ev.clientY - startY) + 'px';
        };
        const up = () => {
          document.removeEventListener('pointermove', move);
          document.removeEventListener('pointerup', up);
        };
        document.addEventListener('pointermove', move);
        document.addEventListener('pointerup', up);
      });

      // 缩放
      resize.addEventListener('pointerdown', (e) => {
        e.preventDefault();
        e.stopPropagation();
        const startX = e.clientX, startY = e.clientY;
        const origW = this.el.offsetWidth;
        const move = (ev) => {
          const w = Math.max(220, Math.min(720, origW + ev.clientX - startX));
          this.el.style.width = w + 'px';
        };
        const up = () => {
          document.removeEventListener('pointermove', move);
          document.removeEventListener('pointerup', up);
        };
        document.addEventListener('pointermove', move);
        document.addEventListener('pointerup', up);
      });

      close.addEventListener('click', () => this.close());

      // 模式切换按钮
      this.el.querySelectorAll('.scr-mode-btn').forEach((b) => {
        b.addEventListener('click', () => this.setMode(b.dataset.mode));
      });

      // 手风琴折叠/展开
      this.el.querySelectorAll('[data-acc-toggle]').forEach((head) => {
        head.addEventListener('click', () => {
          const step = head.closest('.acc-step');
          if (!step) return;
          if (step.classList.contains('is-done') || step.classList.contains('is-error') || step.classList.contains('is-active')) {
            step.classList.toggle('is-open');
            return;
          }
          const n = step.dataset.step;
          toast(`请先完成第 ${Number(n) - 1} 步`);
        });
      });

      // 动作按钮
      this.el.querySelectorAll('[data-act]').forEach((btn) => {
        const act = btn.dataset.act;
        btn.addEventListener('click', () => {
          if (act === 'pair') this.doPair();
          else if (act === 'connect') this.doConnect();
          else if (act === 'start') this.doStart();
          else if (act === 'tcpipSwitch') this.doTcpipSwitch();
          else if (act === 'tcpipConnect') this.doTcpipConnect();
          else if (act === 'usbRefresh') this.loadUsbSerials();
        });
      });
    }

    // —— 配对 / 连接 / 开始 ——
    async doPair() {
      const host = this.field('ip').value.trim();
      const pairPort = parseInt(this.field('pairPort').value.trim(), 10);
      const code = this.field('pairCode').value.trim();
      if (!host || !pairPort || !code) { toast('请填写设备 IP、配对端口和配对码'); return; }
      this.setStepNotice(1, '正在配对设备…', 'info');
      this.status('正在配对…');
      try {
        await adminApi('/api/admin/scrcpy/pair', { host, pairPort, code });
        this.setStepState(1, 'done');
        this.setStepNotice(1, '配对成功', 'ok');
        this.setStepState(2, 'active');
        this.setStepNotice(2, '请填写连接端口以建立 ADB 连接。', 'info');
        this.status('配对成功，请填写连接端口并连接', true);
        this.persistCfg();
      } catch (e) {
        this.setStepState(1, 'error');
        this.setStepNotice(1, '配对失败：' + e.message, 'err');
        this.status('配对失败：' + e.message, false);
      }
    }

    async doConnect() {
      const host = this.field('ip').value.trim();
      const port = parseInt(this.field('connPort').value.trim(), 10);
      if (!host || !port) { toast('请填写设备 IP 和连接端口'); return; }
      this.setStepNotice(2, '正在建立 ADB 连接…', 'info');
      this.status('正在连接…');
      try {
        await adminApi('/api/admin/scrcpy/connect', { host, port });
        this.field('connPort').value = '5555'; // 后端已切固定端口
        this.setStepState(2, 'done');
        this.setStepNotice(2, 'ADB 连接成功，已切换固定端口 5555', 'ok');
        this.setStepState(3, 'active');
        this.setStepNotice(3, '连接就绪，点击「开始控制」进入远程画面。', 'info');
        this.status('ADB 连接成功，可开始控制', true);
        this.persistCfg();
      } catch (e) {
        this.setStepState(2, 'error');
        this.setStepNotice(2, '连接失败：' + e.message, 'err');
        this.status('连接失败：' + e.message, false);
      }
    }

    doStart() {
      let serial;
      if (this.mode === 'tcpip') {
        const host = this.field('tcpipIp').value.trim();
        const port = parseInt(this.field('tcpipConnPort').value.trim(), 10);
        if (!host || !port) { toast('请先完成连接'); return; }
        serial = `${host}:${port}`;
      } else {
        const host = this.field('ip').value.trim();
        if (!host) { toast('请先完成配对与连接'); return; }
        serial = `${host}:5555`; // 后端已切固定端口
        this.persistCfg();
      }
      this.status('正在启动远程会话…');
      this.connectWs(serial);
    }

    // 从服务端获取当前 USB 连接的设备序列号，填充下拉框（免手填）。
    async loadUsbSerials() {
      const sel = this.field('usbSerial');
      if (!sel) return;
      sel.innerHTML = '<option value="">正在检测 USB 设备…</option>';
      sel.disabled = true;
      try {
        const r = await api('/api/admin/scrcpy/usb-devices', { method: 'GET' });
        const serials = (r && r.serials) || [];
        if (!serials.length) {
          sel.innerHTML = '<option value="">未检测到 USB 设备（请插线并开启 USB 调试）</option>';
          return;
        }
        sel.innerHTML = '<option value="">请选择设备…</option>' +
          serials.map((s) => `<option value="${s}">${s}</option>`).join('');
        if (serials.length === 1) sel.value = serials[0]; // 仅一台时自动选中
      } catch (e) {
        sel.innerHTML = '<option value="">检测失败，请手动填写序列号</option>';
        // 检测失败时退回可手填的文本框，避免用户卡住
        const input = document.createElement('input');
        input.setAttribute('data-f', 'usbSerial');
        input.placeholder = '如 8DBY0X2K';
        sel.replaceWith(input);
      } finally {
        if (sel.disabled) sel.disabled = false;
      }
    }

    async doTcpipSwitch() {
      const serial = this.field('usbSerial').value.trim();
      const port = parseInt(this.field('tcpipPort').value.trim(), 10) || 5555;
      if (!serial) { toast('请填写设备序列号（先 USB 连接并开启 USB 调试）'); return; }
      const notice = this.el.querySelector('[data-tcpip-notice]');
      if (notice) notice.textContent = '正在切换为无线监听模式…';
      this.status('正在切换 adb tcpip…');
      try {
        await adminApi('/api/admin/scrcpy/tcpip', { serial, port });
        if (notice) notice.textContent = '切换成功，已可拔线。请填写设备 IP 与端口后点「连接」。';
        this.setStepNotice(1, `切换成功（监听端口 ${port}），请填写设备 IP 与端口连接。`, 'ok');
        toast('已切换到无线监听，可拔掉 USB 线');
        this.field('tcpipConnPort').value = String(port);
      } catch (e) {
        if (notice) notice.textContent = '切换失败：' + e.message;
        this.setStepNotice(1, '切换失败：' + e.message, 'err');
        this.status('切换失败：' + e.message, false);
      }
    }

    async doTcpipConnect() {
      const host = this.field('tcpipIp').value.trim();
      const port = parseInt(this.field('tcpipConnPort').value.trim(), 10);
      if (!host || !port) { toast('请填写设备 IP 和连接端口'); return; }
      this.setStepNotice(1, '正在建立 ADB 连接…', 'info');
      this.status('正在连接…');
      try {
        await adminApi('/api/admin/scrcpy/connect', { host, port });
        this.setStepState(1, 'done');
        this.setStepNotice(1, 'ADB 连接成功', 'ok');
        this.setStepState(2, 'active');
        this.setStepNotice(2, '连接就绪，点击「开始控制」进入远程画面。', 'info');
        this.status('ADB 连接成功，可开始控制', true);
        this.persistTcpipCfg();
      } catch (e) {
        this.setStepState(1, 'error');
        this.setStepNotice(1, '连接失败：' + e.message, 'err');
        this.status('连接失败：' + e.message, false);
      }
    }

    persistCfg() {
      if (!this.device) return;
      const host = this.field('ip').value.trim();
      const pairPort = this.field('pairPort').value.trim();
      const connPort = this.field('connPort').value.trim();
      if (host || connPort) {
        saveConfig(this.device.id, { host, pairPort, connPort });
      }
    }

    persistTcpipCfg() {
      if (!this.device) return;
      const ip = this.field('tcpipIp').value.trim();
      const port = this.field('tcpipConnPort').value.trim();
      if (ip || port) saveConfig(this.device.id, { tcpipIp: ip, tcpipPort: port });
    }

    // —— 恢复已有配置 ——
    restore() {
      if (!this.device) return;
      const saved = loadSavedConfig(this.device.id);
      if (!saved) return;
      if (saved.host) this.field('ip').value = saved.host;
      if (saved.pairPort) this.field('pairPort').value = saved.pairPort;
      if (saved.connPort) this.field('connPort').value = saved.connPort;
      if (saved.tcpipIp) this.field('tcpipIp').value = saved.tcpipIp;
      if (saved.tcpipPort) this.field('tcpipConnPort').value = saved.tcpipPort;
      // 有保存的配置时，查后端真实状态，在线则直接标记就绪。
      if (saved.host && saved.connPort) {
        this.restoreFromBackend(saved.host, saved.connPort);
      } else if (saved.tcpipIp && saved.tcpipPort) {
        this.setMode('tcpip');
        this.restoreFromBackend(saved.tcpipIp, saved.tcpipPort);
      }
    }

    async restoreFromBackend(host, port) {
      this.setStepNotice(1, '正在检查设备连接状态…', 'info');
      this.status('正在检查设备连接状态…');
      try {
        const r = await api(`/api/admin/scrcpy/status?host=${encodeURIComponent(host)}&port=${encodeURIComponent(port)}`, { method: 'GET' });
        if (r && r.online) { this.markReady(); return; }
        this.status('设备不在线，正在自动重连…');
        try {
          await adminApi('/api/admin/scrcpy/connect', { host, port: parseInt(port, 10) });
          this.markReady();
        } catch (e2) {
          this.setStepState(1, 'error');
          this.setStepNotice(1, '设备未连接（adb 无此设备）', 'err');
          this.status('设备未连接：' + e2.message, false);
        }
      } catch (e) {
        this.setStepState(1, 'error');
        this.setStepNotice(1, '无法确认设备状态', 'err');
        this.status('无法确认设备状态：' + e.message, false);
      }
    }

    markReady() {
      if (this.mode === 'tcpip') {
        this.setStepState(1, 'done');
        this.setStepNotice(1, 'ADB 已连接', 'ok');
        this.setStepState(2, 'active');
        this.setStepNotice(2, '设备就绪，点击「开始控制」进入远程画面。', 'info');
      } else {
        this.setStepState(1, 'done');
        this.setStepNotice(1, '设备已配对', 'ok');
        this.setStepState(2, 'done');
        this.setStepNotice(2, 'ADB 已连接', 'ok');
        this.setStepState(3, 'active');
        this.setStepNotice(3, '设备就绪，点击「开始控制」进入远程画面。', 'info');
      }
      this.status('设备已连接，可直接开始控制', true);
    }

    // —— scrcpy WS 会话 ——
    connectWs(serial) {
      if (this.ws && this.alive) { try { this.ws.close(); } catch (_) {} }
      if (this.decoder) { try { this.decoder.close(); } catch (_) {} this.decoder = null; }
      if (this.latestFrame) { try { this.latestFrame.close(); } catch (_) {} this.latestFrame = null; }

      const wsUrl = `${scrcpyWsBase}/api/scrcpy/ws?serial=${encodeURIComponent(serial)}`;
      const sock = new WebSocket(wsUrl);
      sock.binaryType = 'arraybuffer';
      this.ws = sock;
      this.serial = serial;
      this.alive = true;
      this.lastError = null;

      sock.onopen = () => { this.status('远程会话已建立，等待视频…', true); this.statusDot.classList.add('on'); };
      sock.onmessage = async (event) => {
        if (typeof event.data === 'string') {
          try {
            const msg = JSON.parse(event.data);
            if (msg.type === 'codec') { this.initDecoder(msg.codec, msg.width, msg.height); }
            if (msg.type === 'error') { this.lastError = msg.message || '未知错误'; this.status('会话错误：' + this.lastError, false); }
          } catch (_) {}
          return;
        }
        if (!this.decoder) return;
        try {
          const bytes = new Uint8Array(event.data);
          const isKey = hasIdr(bytes);
          if (isKey) {
            if (this.pendingSpsPps && this.pendingSpsPps.length > 0) {
              const merged = new Uint8Array(this.pendingSpsPps.length + bytes.length);
              merged.set(this.pendingSpsPps, 0);
              merged.set(bytes, this.pendingSpsPps.length);
              this.decoder.decode(new EncodedVideoChunk({ type: 'key', timestamp: performance.now() * 1000, data: merged }));
              this.pendingSpsPps = null;
            } else {
              this.decoder.decode(new EncodedVideoChunk({ type: 'key', timestamp: performance.now() * 1000, data: event.data }));
            }
          } else if (isSpsPpsOnly(bytes)) {
            this.pendingSpsPps = bytes;
          } else {
            this.decoder.decode(new EncodedVideoChunk({ type: 'delta', timestamp: performance.now() * 1000, data: event.data }));
          }
        } catch (e) { console.warn('decode chunk failed', e); }
      };
      sock.onclose = () => {
        const err = this.lastError;
        this.lastError = null;
        this.alive = false;
        this.serial = null;
        this.statusDot.classList.remove('on');
        if (err) { this.status('启动失败：' + err, false); }
        else { this.status('远程会话已断开'); }
      };
      sock.onerror = () => { this.status('远程连接失败', false); };
    }

    initDecoder(codec, width, height) {
      this.screenW = width; this.screenH = height;
      this.canvas.width = width; this.canvas.height = height;
      this.canvas.style.setProperty('--scr-ratio', `${width} / ${height}`);
      this.ctx = this.canvas.getContext('2d');
      this.setup.classList.add('hidden');
      this.stage.classList.remove('hidden');
      this.kb.focus();

      if (typeof VideoDecoder === 'undefined') {
        this.status('当前浏览器不支持 WebCodecs，无法解码视频', false);
        return;
      }
      try {
        this.decoder = new VideoDecoder({
          output: (frame) => {
            if (this.latestFrame) { try { this.latestFrame.close(); } catch (_) {} }
            this.latestFrame = frame;
            this.scheduleRender();
          },
          error: (e) => { console.warn('decode error:', e); this.status('解码错误：' + e.message, false); },
        });
        this.decoder.configure({
          codec: codec === 'h265' ? 'hev1.1.6.L93.B0' : 'avc3.640028',
          codedWidth: width,
          codedHeight: height,
        });
        this.status(`已连接 · ${width}x${height}`, true);
      } catch (e) {
        this.status('解码器初始化失败：' + e.message, false);
      }
    }

    scheduleRender() {
      if (this.rafScheduled) return;
      this.rafScheduled = true;
      requestAnimationFrame(() => {
        this.rafScheduled = false;
        if (!this.latestFrame || !this.ctx) return;
        const now = performance.now();
        if (now - this.lastFrameTs < FRAME_INTERVAL) {
          if (this.latestFrame) this.scheduleRender();
          return;
        }
        this.lastFrameTs = now;
        this.ctx.drawImage(this.latestFrame, 0, 0, this.latestFrame.displayWidth, this.latestFrame.displayHeight);
        this.latestFrame.close();
        this.latestFrame = null;
      });
    }

    // —— 控制事件 ——
    sendControl(msg) {
      if (this.ws && this.ws.readyState === WebSocket.OPEN) {
        this.ws.send(JSON.stringify(msg));
      }
    }

    toDevice(e) {
      const rect = this.canvas.getBoundingClientRect();
      const x = (e.clientX - rect.left) / rect.width * this.screenW;
      const y = (e.clientY - rect.top) / rect.height * this.screenH;
      return {
        x: Math.round(Math.max(0, Math.min(this.screenW - 1, x))),
        y: Math.round(Math.max(0, Math.min(this.screenH - 1, y))),
      };
    }

    bindControls() {
      const canvas = this.canvas;
      const SLOP = 4;
      let down = false, downX = 0, downY = 0, dragging = false;

      canvas.addEventListener('pointerdown', (e) => {
        canvas.setPointerCapture(e.pointerId);
        this.kb.focus();
        const p = this.toDevice(e);
        down = true; dragging = false;
        downX = p.x; downY = p.y;
        this.sendControl({ type: 'touch', action: 'down', x: p.x, y: p.y, width: this.screenW, height: this.screenH });
        e.preventDefault();
      });
      canvas.addEventListener('pointermove', (e) => {
        if (!down) return;
        const p = this.toDevice(e);
        const dx = p.x - downX, dy = p.y - downY;
        if (!dragging && (dx * dx + dy * dy) < SLOP * SLOP) return;
        dragging = true;
        this.sendControl({ type: 'touch', action: 'move', x: p.x, y: p.y, width: this.screenW, height: this.screenH });
      });
      canvas.addEventListener('pointerup', (e) => {
        if (!down) return;
        down = false;
        const p = this.toDevice(e);
        this.sendControl({ type: 'touch', action: 'up', x: dragging ? p.x : downX, y: dragging ? p.y : downY, width: this.screenW, height: this.screenH });
        dragging = false;
      });
      canvas.addEventListener('pointercancel', () => {
        if (!down) return;
        down = false;
        this.sendControl({ type: 'touch', action: 'up', x: downX, y: downY, width: this.screenW, height: this.screenH });
        dragging = false;
      });
      canvas.addEventListener('wheel', (e) => {
        e.preventDefault();
        const p = this.toDevice(e);
        const vScroll = e.deltaY > 0 ? 1 : -1;
        this.sendControl({ type: 'scroll', x: p.x, y: p.y, width: this.screenW, height: this.screenH, hScroll: 0, vScroll: vScroll });
      }, { passive: false });

      // 键盘：隐藏 textarea 接收（含中文 IME）
      const kb = this.kb;
      kb.addEventListener('keydown', (e) => {
        const keymap = { Enter: 66, Escape: 4, ArrowLeft: 21, ArrowRight: 22, ArrowUp: 19, ArrowDown: 20, Home: 3, Tab: 61 };
        if (keymap[e.key] !== undefined) {
          e.preventDefault();
          this.sendControl({ type: 'key', action: 'down', key: keymap[e.key] });
          this.sendControl({ type: 'key', action: 'up', key: keymap[e.key] });
          return;
        }
        if (e.key === 'Backspace') {
          e.preventDefault();
          this.sendControl({ type: 'key', action: 'down', key: 67 });
          this.sendControl({ type: 'key', action: 'up', key: 67 });
        }
      });
      kb.addEventListener('beforeinput', (e) => {
        if ((e.inputType === 'insertText' || e.inputType === 'insertCompositionText') && e.data) {
          this.sendControl({ type: 'text', text: e.data });
        } else if (e.inputType === 'deleteContentBackward') {
          this.sendControl({ type: 'key', action: 'down', key: 67 });
          this.sendControl({ type: 'key', action: 'up', key: 67 });
        }
        e.preventDefault();
        kb.value = '';
      });
      let lastComposed = '';
      kb.addEventListener('compositionend', (e) => {
        if (e.data && e.data !== lastComposed) {
          this.sendControl({ type: 'text', text: e.data });
          lastComposed = e.data;
        }
        kb.value = '';
      });
    }

    // —— 关闭 ——
    close() {
      if (this.closed) return;
      this.closed = true;
      if (this.latestFrame) { try { this.latestFrame.close(); } catch (_) {} }
      if (this.decoder) { try { this.decoder.close(); } catch (_) {} }
      if (this.ws) { try { this.ws.close(); } catch (_) {} }
      this.el.remove();
      if (this.device) sessions.delete(this.device.id);
    }
  }

  // 打开某台设备的独立手机窗口。
  function openScrcpy(device) {
    if (!device) return;
    // 同设备去重：已有窗口则置顶。
    if (sessions.has(device.id)) {
      const existing = sessions.get(device.id);
      existing.el.style.zIndex = ++topZ;
      return;
    }
    const session = new ScrcpySession(device);
    sessions.set(device.id, session);
    session.el.style.zIndex = ++topZ;
  }

  let topZ = 90;
  window.openScrcpy = openScrcpy;
  window.closeScrcpy = () => {}; // 兼容旧调用，无操作
})();
