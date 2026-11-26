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

  // 最小化 dock：左下角存放已最小化窗口的缩略条。懒创建，首个最小化时初始化。
  let minDock = null;
  function ensureMinDock() {
    if (minDock) return minDock;
    minDock = document.createElement('div');
    minDock.className = 'scr-min-dock';
    document.body.appendChild(minDock);
    return minDock;
  }

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

  // pickCodecConfig 为给定的编码格式与分辨率，动态探测浏览器 + GPU 实际支持的
  // 最优 WebCodecs 解码配置，优先命中硬件加速（hardwareAcceleration: 'prefer-hardware'），
  // 避免写死一个不兼容的 codec 字符串导致退回软解。
  //
  // 候选按「硬件加速优先 + 从高到低」排列，取第一个 isConfigSupported 返回 supported 的。
  // 均不支持时回退到一个保守的 H.264 软解配置，保证至少能出画面。
  async function pickCodecConfig(codec, width, height) {
    const base = { codedWidth: width, codedHeight: height };
    // H.264 候选：avc3（Annex-B 字节流，scrcpy 的实际封装）+ avc1（AVCC），
    // 覆盖主流 profile/level，优先命中硬解。
    const h264Candidates = [
      'avc3.640028', // High 4:2:0, level 4.0（Annex-B）
      'avc1.640028', // High 4:2:0, level 4.0（AVCC）
      'avc3.4D401F', // Main, level 3.1
      'avc1.4D401F',
      'avc3.42E01E', // Baseline, level 3.0
      'avc1.42E01E',
    ];
    // H.265 候选：hev1（Annex-B），当前后端固定 h264，保留以备用。
    const h265Candidates = ['hev1.1.6.L93.B0', 'hev1.1.6.L120.B0'];

    const candidates = (codec === 'h265' ? h265Candidates : h264Candidates);

    // 优先尝试硬件加速。
    for (const c of candidates) {
      if (await supported(c, 'prefer-hardware')) return { codec: c, ...base, hardwareAcceleration: 'prefer-hardware' };
    }
    // 再尝试自动（浏览器自选，可能软解）。
    for (const c of candidates) {
      if (await supported(c, 'no-preference')) return { codec: c, ...base, hardwareAcceleration: 'no-preference' };
    }
    // 兜底：H.264 软解（avc3 = Annex-B 字节流，与 scrcpy 的实际封装一致）。
    return { codec: 'avc3.640028', ...base, hardwareAcceleration: 'no-preference' };

    async function supported(codecStr, hw) {
      try {
        const cfg = { codec: codecStr, ...base };
        if (hw) cfg.hardwareAcceleration = hw;
        const r = await VideoDecoder.isConfigSupported(cfg);
        return !!(r && r.supported);
      } catch (_) { return false; }
    }
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
          <div class="acc-step" data-step="1">
            <button type="button" class="acc-head" data-acc-toggle>
              <span class="acc-no">1</span><span class="acc-title">切换为无线</span>
              <span class="acc-badge" data-acc-badge></span>
              <svg class="ico ico-sm acc-chevron"><use href="#i-chevron-down"/></svg>
            </button>
            <div class="acc-body">
              <div class="acc-notice" data-acc-notice>适用于没有「无线调试」菜单的设备。先用 USB 连接并开启 USB 调试，填序列号切换为无线，再拔线用 IP+端口连接。</div>
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
            </div>
          </div>
          <div class="acc-step" data-step="2">
            <button type="button" class="acc-head" data-acc-toggle>
              <span class="acc-no">2</span><span class="acc-title">连接设备</span>
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
      </div>`;
  }

  class ScrcpySession {
    constructor(device, opts) {
      this.device = device || null;
      this.opts = opts || {};
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
      this.minimized = false;      // 窗口是否已最小化
      this.minTab = null;          // 最小化后的缩略条 DOM
      this.frameCount = 0;         // 本秒内解码出的帧数（用于 FPS 统计）
      this.fpsTimer = null;        // 每秒刷新一次的 FPS 定时器

      this.buildWindow();
      this.bindWindow();
      this.bindControls();
      // 初始化步骤状态：把第一步标为 active 并展开，否则 acc-body 默认
      // display:none，新打开窗口时第一步折叠、看不到输入框（restore 仅在
      // 有保存配置时才改变步骤状态）。
      this.resetAccordion();
      this.initSession(this.opts);
    }

    // —— DOM 构建 ——
    buildWindow() {
      const name = this.device ? (this.device.name || this.device.model || '设备') : '设备';
      const root = document.createElement('div');
      root.className = 'scr-window';
      root.innerHTML = `
        <div class="scr-window-bar" data-drag>
          <span class="scr-win-status${(this.device && (this.device.online || this.device.adbOnline)) ? ' on' : ''}" data-status-dot></span>
          <span class="scr-win-title" title="${name}">${name}</span>
          <button type="button" class="scr-win-min" data-toggle-setup title="手动连接设置">⚙</button>
          <button type="button" class="scr-win-min" data-min title="最小化"><svg class="ico ico-sm" viewBox="0 0 24 24"><path d="M5 12h14" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" fill="none"/></svg></button>
          <button type="button" class="scr-win-close" data-close title="关闭">✕</button>
        </div>
        <div class="scr-window-body">
          ${setupTemplate(this.device)}
          <div class="screen-frame hidden" data-stage><canvas data-canvas></canvas></div>
          <textarea class="scr-kb-input" autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false" data-kb></textarea>
        </div>
        <div class="scr-win-status" data-status>
          <span class="scr-win-status-text" data-status-text>准备连接…</span>
          <span class="scr-win-fps" data-fps>-- fps</span>
        </div>
        <div class="scr-win-resize" data-resize></div>`;
      document.body.appendChild(root);
      this.el = root;
      this.setup = root.querySelector('.scr-win-setup');
      this.stage = root.querySelector('[data-stage]');
      this.canvas = root.querySelector('[data-canvas]');
      this.kb = root.querySelector('[data-kb]');
      this.statusEl = root.querySelector('[data-status]');
      this.statusTextEl = root.querySelector('[data-status-text]');
      this.statusDot = root.querySelector('[data-status-dot]');
      this.titleEl = root.querySelector('.scr-win-title');
      this.fpsEl = root.querySelector('[data-fps]');

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
      if (this.statusTextEl) this.statusTextEl.textContent = msg;
      else this.statusEl.textContent = msg;
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
        this.setStepNotice(1, '先用 USB 连接并开启 USB 调试，填序列号切换为无线。', 'info');
        this.setStepState(2, 'locked');
        this.setStepNotice(2, '切换成功后，用设备 IP 与端口建立 ADB 连接。', 'info');
        this.setStepState(3, 'locked');
        this.setStepNotice(3, '连接就绪后，点击开始进入远程画面。', 'info');
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

      // 最小化：隐藏窗口，缩略条收纳到左下角 dock。
      const minBtn = this.el.querySelector('[data-min]');
      if (minBtn) minBtn.addEventListener('click', () => this.minimize());

      // 切换手动连接设置面板（兜底用）
      const setupBtn = this.el.querySelector('[data-toggle-setup]');
      if (setupBtn) {
        setupBtn.addEventListener('click', () => {
          const showingSetup = !this.setup.classList.contains('hidden');
          if (showingSetup && this.alive) {
            this.setup.classList.add('hidden');
            this.stage.classList.remove('hidden');
          } else {
            this.setup.classList.remove('hidden');
            this.stage.classList.add('hidden');
          }
        });
      }

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
          const prev = Number(n) - 1;
          toast(prev >= 1 ? `请先完成第 ${prev} 步` : '请先完成上一步操作');
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
        const r = await adminApi('/api/admin/scrcpy/connect', { host, port });
        // 后端返回设备真实在线的端口（无线调试端口动态分配，不能假设 5555），
        // 用它回填输入框并持久化，保证下次打开能按真实端口恢复，无需重新配对。
        const realPort = (r && r.port) ? r.port : port;
        this.field('connPort').value = String(realPort);
        this.setStepState(2, 'done');
        this.setStepNotice(2, 'ADB 连接成功', 'ok');
        this.setStepState(3, 'active');
        this.setStepNotice(3, '连接就绪，点击「开始控制」进入远程画面。', 'info');
        this.status('ADB 连接成功，可开始控制', true);
        this.persistCfg();
        // 连接成功后把 IP、真实端口与无线调试模式（wireless）落库，供设备清单显示状态。
        await this.persistAdbOnline(host, realPort, 'wireless');
      } catch (e) {
        this.setStepState(2, 'error');
        this.setStepNotice(2, '连接失败：' + e.message, 'err');
        this.status('连接失败：' + e.message, false);
      }
    }

    // persistAdbOnline 在任意一条「ADB 已建立连接」的路径上统一落库设备 IP、
    // 端口与在线状态（/api/admin/device/connect），并同步更新前端本地 devices
    // 数组，让「设备分组」「读取文件」等依赖 adbIp/adbPort 的逻辑立即生效、
    // 无需刷新页面。失败不阻断控制流程（下次连接或刷新时仍可由心跳 IP / ADB
    // 扫描兜底）。
    async persistAdbOnline(host, port, mode = 'manual') {
      if (!this.device || !this.device.id || !host) return;
      const body = { deviceId: this.device.id, host, mode };
      if (port) body.port = port;
      try {
        const r = await adminApi('/api/admin/device/connect', body);
        const realPort = (r && r.port) ? r.port : (port || 0);
        const realMode = (r && r.adbMode) ? r.adbMode : mode;
        if (this.device) {
          this.device.adbIp = host;
          this.device.adbPort = realPort;
          this.device.adbOnline = true;
          this.device.adbMode = realMode;
        }
        if (typeof devices !== 'undefined' && Array.isArray(devices)) {
          const dev = devices.find((d) => d && d.id === this.device.id);
          if (dev) {
            dev.adbIp = host;
            dev.adbPort = realPort;
            dev.adbOnline = true;
            dev.adbMode = realMode;
          }
        }
        if (typeof renderDevices === 'function') renderDevices();
        toast('已记录设备连接状态');
      } catch (e) {
        toast('记录连接状态失败：' + (e && e.message ? e.message : e));
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
        const port = parseInt(this.field('connPort').value.trim(), 10);
        if (!host || !port) { toast('请先完成配对与连接'); return; }
        serial = `${host}:${port}`; // 使用无线调试的真实连接端口
        this.persistCfg();
      }
      this.setup.classList.add('hidden');
      this.stage.classList.remove('hidden');
      // 若当前窗口已经在正常播放该 serial 的视频流（decoder 已就绪），直接切回画面即可，不要重复发起第二条 WS 连接
      if (this.alive && this.decoder && this.serial === serial && this.ws && this.ws.readyState === WebSocket.OPEN) {
        this.status('远程画面进行中', true);
        return;
      }
      this.status('正在启动远程会话…');
      this.connectWs(serial);
    }

    // 从服务端获取当前 USB 连接的设备序列号（含授权状态）及已在线的 5555 无线设备。
    // 若检测到该设备已处于 5555 无线模式，自动标记第 1 步完成并展开第 2 步，避免重复对未授权 USB 序列号切 tcpip。
    async loadUsbSerials() {
      const sel = this.field('usbSerial');
      if (!sel) return;
      sel.innerHTML = '<option value="">正在检测 USB 设备…</option>';
      sel.disabled = true;
      try {
        const r = await api('/api/admin/scrcpy/usb-devices', { method: 'GET' });
        const usbDevs = (r && Array.isArray(r.devices) && r.devices.length)
          ? r.devices
          : ((r && r.serials) || []).map((s) => ({ serial: s, state: 'device' }));
        const tcpipOnline = (r && Array.isArray(r.tcpipOnline)) ? r.tcpipOnline : [];

        // 检查当前设备 IP 是否已在 5555 无线端口在线（传统模式切过一次 5555 后无需再切）
        const curIp = (this.field('tcpipIp') && this.field('tcpipIp').value.trim()) || (this.device && this.device.adbIp) || '';
        const curPort = parseInt((this.field('tcpipConnPort') && this.field('tcpipConnPort').value.trim()) || '5555', 10) || 5555;
        const matchedTcpip = curIp
          ? tcpipOnline.find((s) => s === `${curIp}:${curPort}` || s.startsWith(`${curIp}:`))
          : (tcpipOnline.length === 1 && !usbDevs.some((u) => u.state === 'device') ? tcpipOnline[0] : '');

        if (!usbDevs.length) {
          sel.innerHTML = '<option value="">未检测到 USB 设备（若已切过 5555 请直接走第 2 步连接）</option>';
        } else {
          sel.innerHTML = '<option value="">请选择设备…</option>' +
            usbDevs.map((u) => {
              const label = u.state === 'unauthorized'
                ? `${u.serial}（未授权 · 若已切 5555 请直接走第 2 步）`
                : `${u.serial}（已授权 · 可切 5555）`;
              return `<option value="${u.serial}">${label}</option>`;
            }).join('');
          const firstAuth = usbDevs.find((u) => u.state === 'device');
          if (firstAuth) sel.value = firstAuth.serial;
          else if (usbDevs.length === 1) sel.value = usbDevs[0].serial;
        }

        // 若该设备已处于 5555 无线在线（或所有 USB 设备均已变成 unauthorized 且存在已保存 IP），自动放行第 2 步！
        if (matchedTcpip) {
          const [mIp, mPort] = matchedTcpip.split(':');
          if (mIp && this.field('tcpipIp') && !this.field('tcpipIp').value.trim()) {
            this.field('tcpipIp').value = mIp;
          }
          if (mPort && this.field('tcpipConnPort')) {
            this.field('tcpipConnPort').value = mPort;
          }
          this.setStepState(1, 'done');
          this.setStepNotice(1, `检测到设备已处于无线模式（${matchedTcpip}），无需重复执行 adb tcpip，可直接执行第 2 步。`, 'ok');
          this.setStepState(2, 'active');
          this.setStepNotice(2, `设备 ${matchedTcpip} 已在线，点击下方「连接」即可直接控制。`, 'info');
        } else if (curIp && !usbDevs.some((u) => u.state === 'device')) {
          // 未发现已授权 USB（常见于已切 5555 后拔线或 USB 显示未授权），允许用户直接点开第 2 步尝试 IP:5555 连接
          this.setStepState(2, 'active');
          this.setStepNotice(2, `若手机未重启且之前已切过 ${curPort}，可直接点击下方「连接」跳过第 1 步。`, 'info');
        }
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
      const curIp = (this.field('tcpipIp') && this.field('tcpipIp').value.trim()) || '';
      if (!serial) { toast('请填写设备序列号（先 USB 连接并开启 USB 调试）'); return; }
      this.setStepNotice(1, '正在切换为无线监听模式…', 'info');
      this.status('正在切换 adb tcpip…');
      try {
        await adminApi('/api/admin/scrcpy/tcpip', { serial, port });
        this.setStepState(1, 'done');
        this.setStepNotice(1, `切换成功（监听端口 ${port}），可拔线。`, 'ok');
        this.setStepState(2, 'active');
        this.setStepNotice(2, `切换成功，请填写设备 IP 与端口连接。`, 'info');
        this.field('tcpipConnPort').value = String(port);
        toast('已切换到无线监听，可拔掉 USB 线');
      } catch (e) {
        // 传统模式二次连接常见场景：之前已切过 5555，USB 序列号变为 unauthorized，但 IP:5555 本身已可达。
        // 若已填写设备 IP，自动探测一次 IP:port 连接；连通则直接跳过第 1 步并完成连接！
        if (curIp) {
          this.setStepNotice(1, `USB 序列号切换失败，正在检测 ${curIp}:${port} 是否已处于无线模式…`, 'info');
          try {
            await adminApi('/api/admin/scrcpy/connect', { host: curIp, port });
            this.setStepState(1, 'done');
            this.setStepNotice(1, `设备已处于 ${port} 无线模式，已自动跳过 USB 切换。`, 'ok');
            this.setStepState(2, 'done');
            this.setStepNotice(2, `已自动连接 ${curIp}:${port}`, 'ok');
            this.setStepState(3, 'active');
            this.setStepNotice(3, '连接就绪，点击「开始控制」进入远程画面。', 'info');
            this.status('ADB 连接成功，可开始控制', true);
            this.field('tcpipConnPort').value = String(port);
            this.persistTcpipCfg();
            await this.persistAdbOnline(curIp, port, 'tcpip');
            toast(`设备已在 ${port} 端口就绪，已自动跳过 USB 切换并完成连接`);
            return;
          } catch (_) {
            // IP:5555 也不可达，说明手机确实重启或未授权，展示明确指引
          }
        }
        this.setStepState(1, 'error');
        const msg = (e && e.message) ? e.message : String(e);
        this.setStepNotice(1, '切换失败：' + msg + '（若之前已切过 5555 且未重启，请直接点击第 2 步「连接」；若手机已重启，请重插 USB 并在手机点允许调试）', 'err');
        this.status('切换失败：' + msg, false);
      }
    }

    async doTcpipConnect() {
      const host = this.field('tcpipIp').value.trim();
      const port = parseInt(this.field('tcpipConnPort').value.trim(), 10);
      if (!host || !port) { toast('请填写设备 IP 和连接端口'); return; }
      this.setStepNotice(2, '正在建立 ADB 连接…', 'info');
      this.status('正在连接…');
      try {
        await adminApi('/api/admin/scrcpy/connect', { host, port });
        this.setStepState(2, 'done');
        this.setStepNotice(2, 'ADB 连接成功', 'ok');
        this.setStepState(3, 'active');
        this.setStepNotice(3, '连接就绪，点击「开始控制」进入远程画面。', 'info');
        this.status('ADB 连接成功，可开始控制', true);
        this.persistTcpipCfg();
        await this.persistAdbOnline(host, port, 'tcpip');
      } catch (e) {
        this.setStepState(2, 'error');
        this.setStepNotice(2, '连接失败：' + e.message, 'err');
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

    // —— 初始化：区分「ADB 已在线直开屏幕」「自动模式重连/降级」「手动模式（无线调试/传统模式）」 ——
    async initSession(opts) {
      if (!this.device) return;
      const saved = loadSavedConfig(this.device.id) || {};
      const host = (this.device.adbIp && String(this.device.adbIp).trim())
        || saved.host
        || saved.tcpipIp
        || '';
      const port = (this.device.adbPort && this.device.adbPort > 0)
        ? this.device.adbPort
        : (parseInt(saved.connPort || saved.tcpipPort, 10) || 5555);

      if (host) {
        this.field('ip').value = host;
        this.field('tcpipIp').value = host;
      }
      if (saved.pairPort) this.field('pairPort').value = saved.pairPort;
      if (port) {
        this.field('connPort').value = String(port);
        this.field('tcpipConnPort').value = String(port);
      }

      if (opts && opts.manualSetup) {
        this.showManualSetup('请使用「无线调试模式」或「传统模式」手动连接 ADB');
        return;
      }

      // 1. 只要 ADB 在线（无论是无线调试还是传统模式），直接打开投屏画面（不篡改已有 adbMode）！
      if (this.device.adbOnline === true && host && port) {
        this.startDirectStream(host, port);
        return;
      }

      // 2. ADB 不在线时：若有已上报/保存的 IP，尝试直连；否则展开手动连接面板。
      if (host && this.device.online) {
        await this.tryAutoConnectOrFallback(host, port || 5555);
      } else {
        this.showManualSetup('当前设备 ADB 未在线，请通过「无线调试模式」或「传统模式」连接');
      }
    }

    setDeviceAdbMode(mode, online) {
      if (this.device) {
        this.device.adbMode = mode;
        if (typeof online === 'boolean') this.device.adbOnline = online;
      }
      if (typeof devices !== 'undefined' && Array.isArray(devices) && this.device) {
        const dev = devices.find((d) => d && d.id === this.device.id);
        if (dev) {
          dev.adbMode = mode;
          if (typeof online === 'boolean') dev.adbOnline = online;
        }
      }
      if (typeof renderDevices === 'function') renderDevices();
    }

    startDirectStream(host, port) {
      const realPort = Number(port) || 5555;
      this.setup.classList.add('hidden');
      this.stage.classList.remove('hidden');
      this.markReady();
      this.status(`ADB 在线 (${host}:${realPort})，正在打开远程画面…`, true);
      this.connectWs(`${host}:${realPort}`);
    }

    showManualSetup(reasonMsg) {
      this.stage.classList.add('hidden');
      this.setup.classList.remove('hidden');
      // 若该设备之前通过「传统模式」连接过（保存了 tcpipIp 且未用过无线调试配对码），
      // 自动切到传统模式并检测 5555 是否已在线，不影响无线调试模式设备。
      if (this.device) {
        const saved = loadSavedConfig(this.device.id) || {};
        if (saved.tcpipIp && !saved.pairPort) {
          this.setMode('tcpip');
        }
      }
      if (reasonMsg) {
        this.status(reasonMsg, false);
      }
    }

    async tryAutoConnectOrFallback(host, port) {
      let realPort = Number(port) || 5555;
      const keepMode = (this.device && (this.device.adbMode === 'tcpip' || this.device.adbMode === 'wireless'))
        ? this.device.adbMode
        : 'wireless';
      this.setup.classList.add('hidden');
      this.stage.classList.remove('hidden');
      this.status(`正在尝试直连 (${host}:${realPort})…`);

      try {
        const r = await adminApi('/api/admin/device/connect', {
          deviceId: this.device.id,
          host,
          port: realPort,
          mode: keepMode,
        });
        if (r && r.port) realPort = r.port;
        const finalMode = (r && r.adbMode) ? r.adbMode : keepMode;
        if (this.device) {
          this.device.adbIp = host;
          this.device.adbPort = realPort;
          this.device.adbOnline = true;
          this.device.adbMode = finalMode;
        }
        if (typeof devices !== 'undefined' && Array.isArray(devices)) {
          const dev = devices.find((d) => d && d.id === this.device.id);
          if (dev) {
            dev.adbIp = host;
            dev.adbPort = realPort;
            dev.adbOnline = true;
            dev.adbMode = finalMode;
          }
        }
        if (typeof renderDevices === 'function') renderDevices();
        this.field('connPort').value = String(realPort);
        this.field('tcpipConnPort').value = String(realPort);
        saveConfig(this.device.id, {
          host,
          connPort: String(realPort),
          tcpipIp: host,
          tcpipPort: String(realPort),
        });
        this.startDirectStream(host, realPort);
      } catch (e) {
        // 直连失败 → 自动降级到手动连接面板（保持原模式记录不被误改）
        this.showManualSetup(`直连 ${host}:${realPort} 失败（${e.message}），请通过「无线调试模式」或「传统模式」连接`);
        this.setStepState(1, 'active');
        this.setStepNotice(1, `直连失败（${e.message}），请使用无线调试模式或传统模式连接。`, 'err');
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
    connectWs(serial, opts) {
      if (this.ws) {
        try {
          this.ws.onopen = null;
          this.ws.onmessage = null;
          this.ws.onclose = null;
          this.ws.onerror = null;
          this.ws.close();
        } catch (_) {}
        this.ws = null;
      }
      if (this.decoder) { try { this.decoder.close(); } catch (_) {} this.decoder = null; }
      if (this.latestFrame) { try { this.latestFrame.close(); } catch (_) {} this.latestFrame = null; }
      this.pendingSpsPps = null;
      this.ctx = null;

      // 可选的编码参数覆盖（群控降配用）：maxSize/bitrate/fps。
      const q = new URLSearchParams({ serial });
      if (opts) {
        if (opts.maxSize) q.set('maxSize', opts.maxSize);
        if (opts.bitrate) q.set('bitrate', opts.bitrate);
        if (opts.fps) q.set('fps', opts.fps);
      }
      const wsUrl = `${scrcpyWsBase}/api/scrcpy/ws?${q.toString()}`;
      const sock = new WebSocket(wsUrl);
      sock.binaryType = 'arraybuffer';
      this.ws = sock;
      this.serial = serial;
      this.alive = true;
      this.lastError = null;

      sock.onopen = () => {
        if (this.ws !== sock) return;
        this.status('远程会话已建立，等待视频…', true);
        this.statusDot.classList.add('on');
      };
      sock.onmessage = async (event) => {
        if (this.ws !== sock) return;
        if (typeof event.data === 'string') {
          try {
            const msg = JSON.parse(event.data);
            if (msg.type === 'codec') { await this.initDecoder(msg.codec, msg.width, msg.height); }
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
        if (this.ws !== sock) return;
        const err = this.lastError;
        this.lastError = null;
        this.alive = false;
        this.serial = null;
        this.statusDot.classList.remove('on');
        this.stopFpsCounter();
        if (err) {
          if (!this.decoder) {
            this.showManualSetup('启动失败：' + err + '（已自动降级为手动连接模式）');
          } else {
            this.status('启动失败：' + err, false);
          }
        }
        else { this.status('远程会话已断开'); }
      };
      sock.onerror = () => {
        if (this.ws !== sock) return;
        this.status('远程连接失败', false);
      };
    }

    async initDecoder(codec, width, height) {
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
            this.frameCount++;
            if (this.latestFrame) { try { this.latestFrame.close(); } catch (_) {} }
            this.latestFrame = frame;
            this.scheduleRender();
          },
          error: (e) => { console.warn('decode error:', e); this.status('解码错误：' + e.message, false); },
        });
        // 动态选择浏览器/GPU 实际支持的最优解码配置，优先命中硬件加速，
        // 避免写死一个不兼容的 codec 字符串导致退回软解（多路群控时 CPU 爆）。
        const cfg = await pickCodecConfig(codec, width, height);
        this.decoder.configure(cfg);
        this.status(`已连接 · ${width}x${height} · ${cfg.hardwareAcceleration || 'auto'}`, true);
        this.startFpsCounter();
      } catch (e) {
        this.status('解码器初始化失败：' + e.message, false);
      }
    }

    // startFpsCounter 每秒把本秒解码出的帧数刷新到标题栏的 FPS 显示。
    startFpsCounter() {
      if (this.fpsTimer) return;
      this.fpsTimer = setInterval(() => {
        if (this.fpsEl) this.fpsEl.textContent = `${this.frameCount} fps`;
        this.frameCount = 0;
      }, 1000);
    }

    // stopFpsCounter 停止 FPS 统计并复位显示（会话结束/窗口关闭时调用）。
    stopFpsCounter() {
      if (this.fpsTimer) { clearInterval(this.fpsTimer); this.fpsTimer = null; }
      this.frameCount = 0;
      if (this.fpsEl) this.fpsEl.textContent = '-- fps';
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

    // —— 最小化 / 恢复 ——
    minimize() {
      if (this.closed || this.minimized) return;
      this.minimized = true;
      this.el.classList.add('is-minimized');

      const name = this.device ? (this.device.name || this.device.model || '设备') : '设备';
      const dock = ensureMinDock();
      const tab = document.createElement('div');
      tab.className = 'scr-min-tab';
      tab.innerHTML = `
        <span class="dot${this.serial ? '' : ' off'}"></span>
        <span class="name">${name}</span>
        <button type="button" class="restore" title="恢复窗口"><svg class="ico ico-sm" viewBox="0 0 24 24"><path d="M7 17L17 7M9 7h8v8" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" fill="none"/></svg></button>`;
      tab.addEventListener('click', (e) => {
        if (e.target.closest('.restore')) return; // 恢复按钮单独处理
        this.restore();
      });
      tab.querySelector('.restore').addEventListener('click', () => this.restore());
      dock.appendChild(tab);
      this.minTab = tab;
    }

    restore() {
      if (this.closed || !this.minimized) return;
      this.minimized = false;
      this.el.classList.remove('is-minimized');
      this.el.style.zIndex = ++topZ;
      if (this.minTab) { this.minTab.remove(); this.minTab = null; }
      if (minDock && !minDock.children.length) {
        minDock.remove();
        minDock = null;
      }
    }

    // —— 关闭 ——
    close() {
      if (this.closed) return;
      this.closed = true;
      this.stopFpsCounter();
      if (this.latestFrame) { try { this.latestFrame.close(); } catch (_) {} }
      if (this.decoder) { try { this.decoder.close(); } catch (_) {} }
      if (this.ws) { try { this.ws.close(); } catch (_) {} }
      if (this.minTab) { this.minTab.remove(); this.minTab = null; }
      if (minDock && !minDock.children.length) {
        minDock.remove();
        minDock = null;
      }
      this.el.remove();
      if (this.device) sessions.delete(this.device.id);
    }
  }

  // ============ 取点模式（供自动化引擎复用） ============
  // 取点状态：同一时刻只允许一个取点会话，存 { session, onPick, cleanup }。
  let pickState = null;

  // 进入取点模式：在指定设备窗口上，用户点击一次即回调「真实设备坐标」。
  // onPick(x, y) 拿到的是等比换算回设备分辨率的坐标（与 tap 动作一致）。
  function enterPickMode(device, onPick) {
    if (!device || typeof onPick !== 'function') return;
    // 已有取点则先清理，避免串台。
    if (pickState) exitPickMode();

    let reused = false;
    let session;
    if (sessions.has(device.id)) {
      session = sessions.get(device.id);
      if (session.minimized) session.restore();
      reused = true;
    } else {
      session = new ScrcpySession(device);
      sessions.set(device.id, session);
    }
    topZ = Math.max(topZ + 1, 2200);
    session.el.style.zIndex = topZ;

    // 画面提示层：告诉用户当前处于取点模式，并提供一个「取消」退出入口。
    // 放在 body 顶部（标题栏下方），不遮挡窗口自带的关闭按钮。
    const banner = document.createElement('div');
    banner.className = 'scr-pick-banner';
    banner.innerHTML = `<span class="scr-pick-dot"></span><span class="scr-pick-text">点击屏幕上要定位的位置</span><button type="button" class="scr-pick-cancel">取消</button>`;

    // 取点逻辑：监听画布点击，命中后换算坐标并回调。
    const onDown = (e) => {
      if (session.screenW <= 0 || session.screenH <= 0) {
        // 尚未拿到视频分辨率，提示稍后再点。
        toast && toast('画面尚未就绪，请稍候再点');
        return;
      }
      e.stopPropagation();
      e.preventDefault();
      const rect = session.canvas.getBoundingClientRect();
      const x = Math.round(Math.max(0, Math.min(session.screenW - 1, (e.clientX - rect.left) / rect.width * session.screenW)));
      const y = Math.round(Math.max(0, Math.min(session.screenH - 1, (e.clientY - rect.top) / rect.height * session.screenH)));
      onPick(x, y);
      exitPickMode();
    };
    session.canvas.addEventListener('pointerdown', onDown, true);
    session.el.classList.add('is-picking');

    // 取消按钮：主动退出取点模式。
    const cancelBtn = banner.querySelector('.scr-pick-cancel');
    cancelBtn.addEventListener('click', () => exitPickMode());

    // 插入到 body 顶部，作为取点模式的固定提示条。
    const body = session.el.querySelector('.scr-window-body');
    if (body) body.insertBefore(banner, body.firstChild);

    pickState = {
      session,
      onPick,
      cleanup: () => {
        session.canvas.removeEventListener('pointerdown', onDown, true);
        session.el.classList.remove('is-picking');
        banner.remove();
        if (!reused) session.close();
      },
    };
  }

  // 进入「控件悬停高亮拾取器（UI Inspector）」模式（类似影刀捕获元素）：
  // 自动抓取手机当前 UI 控件树，鼠标在画面上悬停时高亮对应控件矩形并展示 text/resId/desc，
  // 点击即可将控件选择器（by + value + centerX/Y）回填给流程步骤。
  function enterInspectMode(device, onPickElement) {
    if (!device || typeof onPickElement !== 'function') return;
    if (pickState) exitPickMode();

    let reused = false;
    let session;
    if (sessions.has(device.id)) {
      session = sessions.get(device.id);
      if (session.minimized) session.restore();
      reused = true;
    } else {
      session = new ScrcpySession(device);
      sessions.set(device.id, session);
    }
    topZ = Math.max(topZ + 1, 2200);
    session.el.style.zIndex = topZ;

    const banner = document.createElement('div');
    banner.className = 'scr-pick-banner';
    banner.innerHTML = `
      <span class="scr-pick-dot"></span>
      <span class="scr-pick-text" data-inspect-status>正在抓取手机控件树…</span>
      <button type="button" class="scr-pick-cancel" data-inspect-refresh style="margin-right:4px">刷新控件</button>
      <button type="button" class="scr-pick-cancel" data-inspect-cancel>取消</button>`;

    const statusText = banner.querySelector('[data-inspect-status]');
    const refreshBtn = banner.querySelector('[data-inspect-refresh]');
    const cancelBtn = banner.querySelector('[data-inspect-cancel]');

    // 悬停高亮框与浮层标签
    const highlight = document.createElement('div');
    highlight.style.cssText = 'position:fixed;pointer-events:none;border:2px solid #2e6cf6;background:rgba(46,108,246,0.18);border-radius:4px;z-index:2500;display:none;transition:all 40ms ease-out;';
    const tip = document.createElement('div');
    tip.style.cssText = 'position:absolute;left:0;top:-24px;background:#101828;color:#fff;font-size:11.5px;padding:2px 7px;border-radius:4px;white-space:nowrap;max-width:280px;overflow:hidden;text-overflow:ellipsis;box-shadow:0 2px 6px rgba(0,0,0,.25);';
    highlight.appendChild(tip);
    document.body.appendChild(highlight);

    let uiNodes = [];
    let hoveredNode = null;

    const fetchTree = async () => {
      statusText.textContent = '正在抓取屏幕控件树…';
      try {
        const res = await api(`/api/admin/automation/ui-tree?deviceId=${encodeURIComponent(device.id)}`, { method: 'GET' });
        uiNodes = (res && Array.isArray(res.nodes)) ? res.nodes : [];
        statusText.textContent = uiNodes.length
          ? `已捕获 ${uiNodes.length} 个控件，移动鼠标悬停高亮并点击拾取`
          : '未抓到控件，可直接点击画面按坐标拾取';
      } catch (e) {
        uiNodes = [];
        statusText.textContent = '控件树抓取失败，点击画面将回退为坐标拾取';
      }
    };
    fetchTree();
    refreshBtn.addEventListener('click', (e) => { e.stopPropagation(); fetchTree(); });
    cancelBtn.addEventListener('click', (e) => { e.stopPropagation(); exitPickMode(); });

    const findSmallestHitNode = (devX, devY) => {
      let best = null;
      let bestArea = Infinity;
      for (const n of uiNodes) {
        const b = n.bounds;
        if (!b || b.length !== 4) continue;
        if (devX < b[0] || devX > b[2] || devY < b[1] || devY > b[3]) continue;
        const hasInfo = !!(n.text || n.desc || n.resId || n.clickable);
        const area = Math.max(1, (b[2] - b[0]) * (b[3] - b[1])) * (hasInfo ? 1 : 4);
        if (area <= bestArea) {
          bestArea = area;
          best = n;
        }
      }
      return best;
    };

    const onMove = (e) => {
      if (session.screenW <= 0 || session.screenH <= 0 || !uiNodes.length) {
        highlight.style.display = 'none';
        return;
      }
      const rect = session.canvas.getBoundingClientRect();
      if (e.clientX < rect.left || e.clientX > rect.right || e.clientY < rect.top || e.clientY > rect.bottom) {
        highlight.style.display = 'none';
        hoveredNode = null;
        return;
      }
      const devX = Math.round((e.clientX - rect.left) / rect.width * session.screenW);
      const devY = Math.round((e.clientY - rect.top) / rect.height * session.screenH);
      const hit = findSmallestHitNode(devX, devY);
      hoveredNode = hit;
      if (!hit) {
        highlight.style.display = 'none';
        return;
      }
      const [l, t, r, b] = hit.bounds;
      const scaleX = rect.width / session.screenW;
      const scaleY = rect.height / session.screenH;
      highlight.style.left = (rect.left + l * scaleX) + 'px';
      highlight.style.top = (rect.top + t * scaleY) + 'px';
      highlight.style.width = Math.max(8, (r - l) * scaleX) + 'px';
      highlight.style.height = Math.max(8, (b - t) * scaleY) + 'px';
      const label = hit.text
        ? `文本: "${hit.text}"`
        : (hit.desc ? `描述: "${hit.desc}"` : (hit.resId ? `ID: ${hit.resId}` : hit.className));
      tip.textContent = `${label} (${hit.centerX}, ${hit.centerY})`;
      highlight.style.display = 'block';
    };

    const onDown = (e) => {
      if (session.screenW <= 0 || session.screenH <= 0) {
        toast && toast('画面尚未就绪，请稍候再点');
        return;
      }
      e.stopPropagation();
      e.preventDefault();
      const rect = session.canvas.getBoundingClientRect();
      const devX = Math.round(Math.max(0, Math.min(session.screenW - 1, (e.clientX - rect.left) / rect.width * session.screenW)));
      const devY = Math.round(Math.max(0, Math.min(session.screenH - 1, (e.clientY - rect.top) / rect.height * session.screenH)));
      const hit = hoveredNode || findSmallestHitNode(devX, devY);
      let picked;
      if (hit && (hit.text || hit.resId || hit.desc)) {
        const by = hit.text ? 'text' : (hit.resId ? 'id' : 'desc');
        const value = hit.text || hit.resId || hit.desc;
        picked = {
          by,
          value,
          text: hit.text || '',
          resId: hit.resId || '',
          desc: hit.desc || '',
          className: hit.className || '',
          x: hit.centerX || devX,
          y: hit.centerY || devY,
        };
      } else {
        picked = { by: 'any', value: '', x: devX, y: devY };
      }
      onPickElement(picked);
      exitPickMode();
    };

    session.canvas.addEventListener('pointermove', onMove, true);
    session.canvas.addEventListener('pointerdown', onDown, true);
    session.el.classList.add('is-picking');

    const body = session.el.querySelector('.scr-window-body');
    if (body) body.insertBefore(banner, body.firstChild);

    pickState = {
      session,
      onPick: onPickElement,
      cleanup: () => {
        session.canvas.removeEventListener('pointermove', onMove, true);
        session.canvas.removeEventListener('pointerdown', onDown, true);
        session.el.classList.remove('is-picking');
        banner.remove();
        highlight.remove();
        if (!reused) session.close();
      },
    };
  }

  function exitPickMode() {
    if (!pickState) return;
    const { cleanup } = pickState;
    pickState = null;
    try { cleanup(); } catch (_) {}
  }

  // 对外暴露：pickPoint(device, onPick) 与 pickElement(device, onPickElement)。
  window.pickPoint = (device, onPick) => enterPickMode(device, onPick);
  window.pickElement = (device, onPickElement) => enterInspectMode(device, onPickElement);

  // 打开某台设备的独立手机窗口。
  function openScrcpy(device, opts) {
    if (!device) return;
    // 同设备去重：已有窗口则恢复并置顶（含最小化状态）。
    if (sessions.has(device.id)) {
      const existing = sessions.get(device.id);
      existing.device = device;
      if (existing.minimized) existing.restore();
      existing.el.style.zIndex = ++topZ;
      if (!existing.alive || (opts && opts.manualSetup)) existing.initSession(opts);
      return;
    }
    const session = new ScrcpySession(device, opts);
    sessions.set(device.id, session);
    session.el.style.zIndex = ++topZ;
  }

  let topZ = 90;
  window.openScrcpy = openScrcpy;
  window.closeScrcpy = (deviceId) => {
    if (deviceId) {
      const s = sessions.get(deviceId);
      if (s) s.close();
      return;
    }
    for (const s of [...sessions.values()]) s.close();
  };

  // ============ 群控（镜像操作）面板 ============
  // 形态 A：输入同步。把多台「已连接」的设备编成一个群控组，指定主控后，
  // 在主控窗口上点/滑/打字，后端按各自分辨率归一化后同步到组内所有设备。
  // 后端逻辑见 internal/scrcpy/sync.go（/api/scrcpy/sync WS 端点）。
  let syncPanel = null;       // 群控面板 DOM
  let syncWs = null;          // 群控管理 WS 连接
  let syncGroupId = null;     // 当前群控组 id（服务端生成或前端指定）
  let mirrorGroupSeq = 1;     // 固定镜像组命名序号（内存计数）

  // openSyncPanel 打开（或聚焦）群控面板，列出所有已连接的远程控制窗口。
  window.openSyncPanel = function () {
    if (syncPanel && syncPanel.isConnected) {
      syncPanel.style.zIndex = ++topZ;
      renderSyncMembers();
      return;
    }
    buildSyncPanel();
    renderSyncMembers();
  };

  function buildSyncPanel() {
    const root = document.createElement('div');
    root.className = 'sync-panel';
    root.innerHTML = `
      <div class="sync-panel-bar" data-sync-drag>
        <span class="sync-panel-title">群控（镜像操作）</span>
        <button type="button" class="sync-panel-close" data-sync-close>✕</button>
      </div>
      <div class="sync-panel-body">
        <p class="sync-panel-hint">把多台「已连接」的远程控制窗口编成一组，指定主控后，在主控上操作会同步到组内所有设备。</p>
        <div class="sync-members" data-sync-members></div>
        <div class="sync-actions">
          <button type="button" class="btn-primary btn-block" data-sync-create>创建群控组</button>
          <button type="button" class="btn-danger btn-block hidden" data-sync-destroy>解散群控组</button>
        </div>
        <div class="sync-status" data-sync-status></div>
        <div class="mirror-groups">
          <div class="mirror-groups-head">
            <span>固定镜像组</span>
            <button type="button" class="btn-ghost btn-sm mirror-save-btn" data-save-group>保存当前为固定组</button>
          </div>
          <div class="mirror-groups-list" data-mirror-groups>加载中…</div>
        </div>
      </div>`;
    document.body.appendChild(root);
    syncPanel = root;

    // 拖拽
    const bar = root.querySelector('[data-sync-drag]');
    bar.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      const sx = e.clientX, sy = e.clientY;
      const ol = root.offsetLeft, ot = root.offsetTop;
      const move = (ev) => { root.style.left = (ol + ev.clientX - sx) + 'px'; root.style.top = (ot + ev.clientY - sy) + 'px'; };
      const up = () => { document.removeEventListener('pointermove', move); document.removeEventListener('pointerup', up); };
      document.addEventListener('pointermove', move);
      document.addEventListener('pointerup', up);
    });

    // 关闭
    root.querySelector('[data-sync-close]').addEventListener('click', () => {
      closeSyncWs();
      root.remove();
      syncPanel = null;
      syncGroupId = null;
    });

    root.querySelector('[data-sync-create]').addEventListener('click', () => createSyncGroup());
    root.querySelector('[data-sync-destroy]').addEventListener('click', () => destroySyncGroup());
    root.querySelector('[data-save-group]').addEventListener('click', () => saveCurrentAsGroup());

    // 初始位置靠右上
    root.style.left = 'auto';
    root.style.right = '24px';
    root.style.top = '80px';
    root.style.zIndex = ++topZ;

    renderMirrorGroups();
  }

  // renderSyncMembers 列出所有已连接的窗口会话，供勾选编组。
  function renderSyncMembers() {
    const box = syncPanel && syncPanel.querySelector('[data-sync-members]');
    if (!box) return;
    const entries = [];
    sessions.forEach((s, deviceId) => {
      if (s.alive && s.serial) {
        entries.push({ deviceId, session: s, name: s.device ? (s.device.name || s.device.model || '设备') : '设备', serial: s.serial });
      }
    });
    if (!entries.length) {
      box.innerHTML = '<p class="sync-empty">暂无可群控的设备。请先打开要群控的手机的「远程控制」窗口并连接成功。</p>';
      return;
    }
    box.innerHTML = entries.map((e, i) => `
      <label class="sync-member">
        <input type="checkbox" data-sync-check value="${e.serial}" data-name="${e.name}" data-device="${e.deviceId}">
        <span class="sync-member-name">${e.name}</span>
        <span class="sync-member-serial">${e.serial}</span>
      </label>`).join('');
  }

  // getCheckedMembers 返回当前勾选的设备（serial + 显示名 + deviceId）。
  function getCheckedMembers() {
    if (!syncPanel) return [];
    const boxes = syncPanel.querySelectorAll('[data-sync-check]:checked');
    return Array.from(boxes).map((b) => ({ serial: b.value, name: b.dataset.name, deviceId: b.dataset.device }));
  }

  // ===== 固定镜像组管理 =====
  // 把「主控 + 成员」用 deviceId 持久化，一键镜像时复用；与临时勾选建组并存。

  // renderMirrorGroups 拉取并渲染固定镜像组列表到群控面板。
  async function renderMirrorGroups() {
    const box = syncPanel && syncPanel.querySelector('[data-mirror-groups]');
    if (!box) return;
    let groups = [];
    try { groups = await loadSyncGroups(); } catch (e) { box.innerHTML = '<p class="sync-empty">加载固定组失败</p>'; return; }
    if (!groups.length) { box.innerHTML = '<p class="sync-empty">暂无固定镜像组。勾选设备后点「保存当前为固定组」。</p>'; return; }
    box.innerHTML = groups.map((g) => {
      const names = (g.deviceIds || []).map((id) => deviceName(id)).join('、');
      return `
        <div class="mirror-group-item" data-gid="${g.id}">
          <div class="mirror-group-info">
            <span class="mirror-group-name">${escapeHtml(g.name)}</span>
            <span class="mirror-group-members">${escapeHtml(names)}</span>
          </div>
          <div class="mirror-group-actions">
            <button type="button" class="btn-primary btn-sm" data-launch>一键镜像</button>
            <button type="button" class="btn-ghost btn-sm mirror-save-btn" data-edit title="编辑">编辑</button>
            <button type="button" class="btn-ghost btn-sm" data-del title="删除">✕</button>
          </div>
        </div>`;
    }).join('');
    box.querySelectorAll('[data-launch]').forEach((btn) => {
      btn.addEventListener('click', () => launchMirrorGroup(btn.closest('[data-gid]').dataset.gid));
    });
    box.querySelectorAll('[data-edit]').forEach((btn) => {
      btn.addEventListener('click', () => openEditSyncGroup(btn.closest('[data-gid]').dataset.gid));
    });
    box.querySelectorAll('[data-del]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const gid = btn.closest('[data-gid]').dataset.gid;
        if (!confirm('删除这个固定镜像组？')) return;
        try { await api('/api/admin/sync-groups/' + encodeURIComponent(gid), { method: 'DELETE' }); toast('已删除'); renderMirrorGroups(); }
        catch (e) { toast('删除失败：' + e.message); }
      });
    });
  }

  // saveCurrentAsGroup 把当前勾选设备存为固定镜像组（主控取第一个勾选）。
  async function saveCurrentAsGroup() {
    const members = getCheckedMembers();
    if (members.length < 2) { syncStatus('请先勾选至少 2 台设备，再保存为固定组', true); return; }
    const name = prompt('固定镜像组名称：', '镜像组 ' + (mirrorGroupSeq++));
    if (name == null) return;
    const trimmed = (name || '').trim();
    if (!trimmed) { syncStatus('名称不能为空', true); return; }
    const deviceIds = members.map((m) => m.deviceId);
    const masterId = members[0].deviceId;
    try {
      await api('/api/admin/sync-groups', { method: 'POST', body: JSON.stringify({ name: trimmed, masterId, deviceIds }) });
      toast('固定镜像组已保存');
      renderMirrorGroups();
    } catch (e) {
      syncStatus('保存失败：' + e.message, true);
    }
  }

  // ===== 固定镜像组编辑面板 =====
  let editPanel = null;    // 编辑面板 DOM
  let editingGroup = null; // 正在编辑的固定组对象

  // adbConnectedDevices 返回所有已连接 adb（有 adbIp）的设备，作为成员候选。
  function adbConnectedDevices() {
    if (typeof devices === 'undefined' || !Array.isArray(devices)) return [];
    return devices.filter((d) => d && d.adbIp);
  }

  // openEditSyncGroup 打开某固定组的编辑面板（改名 / 改主控 / 增删成员）。
  async function openEditSyncGroup(groupId) {
    let grp = null;
    try {
      const groups = await loadSyncGroups();
      grp = groups.find((g) => g.id === groupId);
    } catch (e) {
      toast('加载镜像组失败：' + e.message);
      return;
    }
    if (!grp) { toast('镜像组不存在'); return; }
    editingGroup = grp;
    buildEditPanel(grp, false);
  }

  // openDeviceGroupAdd 打开「新建镜像组」面板（isNew 模式，保存走 POST）。
  async function openDeviceGroupAdd() {
    editingGroup = { id: '', name: '', masterId: '', deviceIds: [] };
    buildEditPanel(editingGroup, true);
  }

  function buildEditPanel(grp, isNew) {
    // 只移除旧面板 DOM，保留 editingGroup（由 openEditSyncGroup / openDeviceGroupAdd 负责赋值）。
    if (editPanel) { editPanel.remove(); editPanel = null; }
    const root = document.createElement('div');
    root.className = 'mirror-edit-panel';
    root.dataset.isNew = isNew ? '1' : '0';
    const candidates = adbConnectedDevices();
    // 成员勾选行：已连接的设备全部列出，当前组成员预勾选。
    const memberRows = candidates.map((d) => {
      const checked = (grp.deviceIds || []).includes(d.id);
      return `
        <label class="mirror-edit-member" data-device="${escapeHtml(d.id)}">
          <input type="checkbox" class="mirror-edit-check" value="${escapeHtml(d.id)}" ${checked ? 'checked' : ''}>
          <span class="mirror-edit-member-name">${escapeHtml(deviceName(d.id))}</span>
          <span class="mirror-edit-member-serial">${escapeHtml(d.adbIp)}</span>
        </label>`;
    }).join('');
    if (!candidates.length) {
      // 无已连接设备时，仍展示当前组成员（不可增删，仅能改名/改主控）。
      const current = (grp.deviceIds || []).map((id) => `
        <label class="mirror-edit-member" data-device="${escapeHtml(id)}">
          <input type="checkbox" class="mirror-edit-check" value="${escapeHtml(id)}" checked>
          <span class="mirror-edit-member-name">${escapeHtml(deviceName(id))}</span>
        </label>`).join('');
      root.innerHTML = `
        <div class="mirror-edit-bar" data-drag>
          <span class="mirror-edit-title">${isNew ? '新建镜像组' : '编辑固定镜像组'}</span>
          <button type="button" class="mirror-edit-close" data-close>✕</button>
        </div>
        <div class="mirror-edit-body">
          <label class="mirror-edit-field">名称<input data-name value="${escapeHtml(grp.name)}"></label>
          <div class="mirror-edit-label">成员（当前无已连接 ADB 设备，无法增减）</div>
          <div class="mirror-edit-members" data-members>${current}</div>
          <div class="mirror-edit-hint">提示：请先在「设备清单」中点击设备的「连接设备」并确认连接成功，再回到这里新建镜像组。</div>
          <div class="mirror-edit-label">主控</div>
          <select class="mirror-edit-master" data-master></select>
          <div class="mirror-edit-actions">
            <button type="button" class="btn-primary" data-save>保存</button>
            <button type="button" class="btn-ghost" data-cancel>取消</button>
          </div>
        </div>`;
    } else {
      root.innerHTML = `
        <div class="mirror-edit-bar" data-drag>
          <span class="mirror-edit-title">${isNew ? '新建镜像组' : '编辑固定镜像组'}</span>
          <button type="button" class="mirror-edit-close" data-close>✕</button>
        </div>
        <div class="mirror-edit-body">
          <label class="mirror-edit-field">名称<input data-name value="${escapeHtml(grp.name)}"></label>
          <div class="mirror-edit-label">成员（勾选要加入的设备，至少 2 台）</div>
          <div class="mirror-edit-members" data-members>${memberRows}</div>
          <div class="mirror-edit-label">主控</div>
          <select class="mirror-edit-master" data-master></select>
          <div class="mirror-edit-actions">
            <button type="button" class="btn-primary" data-save>保存</button>
            <button type="button" class="btn-ghost" data-cancel>取消</button>
          </div>
        </div>`;
    }
    document.body.appendChild(root);
    editPanel = root;
    root.style.zIndex = ++topZ;

    // 主控下拉：由勾选成员动态刷新。
    const masterSelect = root.querySelector('[data-master]');
    const membersBox = root.querySelector('[data-members]');
    const refreshMaster = () => {
      const checkedIds = Array.from(membersBox.querySelectorAll('.mirror-edit-check:checked')).map((c) => c.value);
      const prev = masterSelect.value;
      masterSelect.replaceChildren();
      for (const id of checkedIds) {
        const o = document.createElement('option');
        o.value = id;
        o.textContent = deviceName(id);
        masterSelect.append(o);
      }
      // 尽量保留原主控；否则默认第一个。
      if ([...masterSelect.options].some((o) => o.value === prev)) masterSelect.value = prev;
      else if (grp.masterId && [...masterSelect.options].some((o) => o.value === grp.masterId)) masterSelect.value = grp.masterId;
      else if (masterSelect.options.length) masterSelect.value = masterSelect.options[0].value;
    };
    membersBox.addEventListener('change', refreshMaster);
    refreshMaster();

    // 拖拽
    const bar = root.querySelector('[data-drag]');
    bar.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      const sx = e.clientX, sy = e.clientY;
      const ol = root.offsetLeft, ot = root.offsetTop;
      const move = (ev) => { root.style.left = (ol + ev.clientX - sx) + 'px'; root.style.top = (ot + ev.clientY - sy) + 'px'; };
      const up = () => { document.removeEventListener('pointermove', move); document.removeEventListener('pointerup', up); };
      document.addEventListener('pointermove', move);
      document.addEventListener('pointerup', up);
    });

    root.querySelector('[data-close]').addEventListener('click', closeEditPanel);
    root.querySelector('[data-cancel]').addEventListener('click', closeEditPanel);
    root.querySelector('[data-save]').addEventListener('click', () => saveEditSyncGroup(root));
  }

  // saveEditSyncGroup 收集面板数据，PUT（编辑）或 POST（新建）保存固定组。
  async function saveEditSyncGroup(root) {
    const isNew = root.dataset.isNew === '1';
    const name = (root.querySelector('[data-name]').value || '').trim();
    if (!name) { toast('名称不能为空'); return; }
    const deviceIds = Array.from(root.querySelectorAll('.mirror-edit-check:checked')).map((c) => c.value);
    if (deviceIds.length < 2) { toast('成员至少需要 2 台设备'); return; }
    const masterId = root.querySelector('[data-master]').value;
    try {
      if (isNew) {
        await api('/api/admin/sync-groups', { method: 'POST', body: JSON.stringify({ name, masterId, deviceIds }) });
        toast('镜像组已创建');
      } else {
        await api('/api/admin/sync-groups/' + encodeURIComponent(editingGroup.id), {
          method: 'PUT',
          body: JSON.stringify({ name, masterId, deviceIds }),
        });
        toast('固定镜像组已更新');
      }
      closeEditPanel();
      renderMirrorGroups();
      if (typeof renderDeviceGroupsPage === 'function') renderDeviceGroupsPage();
    } catch (e) {
      toast(isNew ? '创建失败：' + e.message : '更新失败：' + e.message);
    }
  }

  function closeEditPanel() {
    if (editPanel) { editPanel.remove(); editPanel = null; }
    editingGroup = null;
  }

  function syncStatus(text, isErr) {
    if (!syncPanel) return;
    const el = syncPanel.querySelector('[data-sync-status]');
    el.textContent = text || '';
    el.classList.toggle('err', !!isErr);
  }

  function openSyncWs() {
    if (syncWs && syncWs.readyState === WebSocket.OPEN) return syncWs;
    closeSyncWs();
    const ws = new WebSocket(`${scrcpyWsBase}/api/scrcpy/sync`);
    ws.onclose = () => { syncWs = null; };
    ws.onerror = () => { syncStatus('群控连接失败，请确认 scrcpy-server(9004) 已启动', true); };
    syncWs = ws;
    return ws;
  }

  function closeSyncWs() {
    if (syncWs) { try { syncWs.close(); } catch (_) {} syncWs = null; }
  }

  // sendSync 发送一条群控管理消息，等待服务端响应。
  function sendSync(msg) {
    return new Promise((resolve, reject) => {
      const ws = openSyncWs();
      if (ws.readyState === WebSocket.OPEN) {
        doSend(ws, msg, resolve, reject);
      } else {
        ws.onopen = () => doSend(ws, msg, resolve, reject);
      }
      // 兜底超时
      setTimeout(() => reject(new Error('群控请求超时')), 8000);
    });
  }

  function doSend(ws, msg, resolve, reject) {
    const onMsg = (e) => {
      try {
        const r = JSON.parse(e.data);
        if (r.type === 'sync-error') { ws.removeEventListener('message', onMsg); reject(new Error(r.message)); }
        else if (r.type === 'sync-created' || r.type === 'sync-master-set' || r.type === 'sync-destroyed') { ws.removeEventListener('message', onMsg); resolve(r); }
      } catch (_) {}
    };
    ws.addEventListener('message', onMsg);
    ws.send(JSON.stringify(msg));
  }

  // 群控低配参数：多路视频同时解码时降低每路帧率/码率/分辨率，避免浏览器
  // 与设备端资源被多路高清流占满导致卡顿。清晰度足够看清画面与操作对位。
  const SYNC_LOW_OPTS = { maxSize: 1280, bitrate: 1500000, fps: 15 };

  async function createSyncGroup() {
    const members = getCheckedMembers();
    if (members.length < 2) { syncStatus('请至少勾选 2 台已连接的设备', true); return; }
    const groupId = 'sync-' + Date.now();
    const serials = members.map((m) => m.serial);

    // 建组前把组内所有设备用低配参数重连，降低多路视频的资源占用。
    syncStatus('正在切换到群控低配模式（15fps）并重建会话…');
    await reconnectMembersLow(members);

    syncStatus('正在创建群控组…');
    try {
      const r = await sendSync({ type: 'sync-create', groupId, serials, master: serials[0] });
      syncGroupId = r.groupId;
      syncStatus(`群控组已创建，主控：${members[0].name}（共 ${serials.length} 台）。在主控窗口操作即可同步。`, false);
      toast(`群控组已创建，主控：${members[0].name}`);
      syncPanel.querySelector('[data-sync-destroy]').classList.remove('hidden');
    } catch (e) {
      syncStatus('创建失败：' + e.message, true);
    }
  }

  // reconnectMembersLow 用低配参数重连所有群控成员，并等待会话重新就绪。
  // 群控要求每台设备都有活跃会话（后端 registerSession），重连后需等 codec
  // 头到达、解码器重建完成才能建组。
  async function reconnectMembersLow(members) {
    const sessions_ = members
      .map((m) => ({ ...m, session: sessions.get(m.deviceId) }))
      .filter((m) => m.session);

    // 逐个用低配参数重连（不等待，串行发起避免瞬间并发压垮 adb）。
    for (const m of sessions_) {
      m.session.connectWs(m.serial, SYNC_LOW_OPTS);
    }

    // 等待所有成员解码器重建完成（decoder 就绪即 codec 头已到、会话已登记）。
    const deadline = Date.now() + 15000;
    while (Date.now() < deadline) {
      const allReady = sessions_.every((m) => m.session.decoder && m.session.ctx);
      if (allReady) return;
      await new Promise((r) => setTimeout(r, 300));
    }
    // 超时：不阻塞建组，交由后端在建组时校验会话是否活跃。
  }

  async function destroySyncGroup() {
    if (!syncGroupId) return;
    syncStatus('正在解散群控组…');
    try {
      await sendSync({ type: 'sync-destroy', groupId: syncGroupId });
      syncGroupId = null;
      syncStatus('群控组已解散', false);
      syncPanel.querySelector('[data-sync-destroy]').classList.add('hidden');
      toast('群控组已解散');
    } catch (e) {
      syncStatus('解散失败：' + e.message, true);
    }
  }

  // ============ 文件浏览面板 ============
  // 读取设备内部存储的文件：列目录、显示图片缩略图、多选下载。
  // 接口走同源 admin-server（复用 token 鉴权）：/api/admin/files/{list,download,thumb}。
  let filePanel = null;          // 文件面板 DOM
  let fileSerial = null;         // 当前浏览的设备 serial（ip:port）
  let filePath = '/sdcard/';     // 当前浏览的目录
  let fileEntries = [];          // 当前目录下的条目（用于勾选下载）

  // authFetch 带管理员 token 请求 admin-server，返回 { ok, blob, json, text }。
  // 下载/缩略图需要二进制流，不能走全局 api()（它会把响应当 JSON 解析）。
  async function authFetch(path) {
    const res = await fetch(apiBase + path, {
      headers: { Authorization: `Bearer ${token}` },
    });
    const text = await res.text();
    let json = null;
    try { json = JSON.parse(text); } catch (_) {}
    return { ok: res.ok, json, text };
  }

  // openFilePanel 打开某台设备的文件浏览窗口。serial 是 ip:port，name 用于标题。
  window.openFilePanel = function (serial, name) {
    if (filePanel) {
      filePanel.style.zIndex = ++topZ;
      if (serial && serial !== fileSerial) {
        // 切换设备时重置路径并刷新。
        fileSerial = serial;
        filePath = '/sdcard/';
        renderFileList();
      }
      return;
    }
    fileSerial = serial;
    filePath = '/sdcard/';
    buildFilePanel(name);
    renderFileList();
  };

  function buildFilePanel(name) {
    const root = document.createElement('div');
    root.className = 'file-panel';
    root.innerHTML = `
      <div class="file-panel-bar" data-file-drag>
        <svg class="ico ico-sm"><use href="#i-folder"/></svg>
        <span class="file-panel-title">文件管理 · ${escapeHtml(name || '设备')}</span>
        <button type="button" class="file-panel-close" data-file-close>✕</button>
      </div>
      <div class="file-panel-body">
        <div class="file-pathbar">
          <button type="button" class="file-up" data-file-up title="返回上级"><svg class="ico ico-sm"><use href="#i-back"/></svg></button>
          <input class="file-path-input" data-file-path value="${escapeHtml(filePath)}" spellcheck="false">
          <button type="button" class="file-go" data-file-go>跳转</button>
        </div>
        <div class="file-list" data-file-list></div>
        <div class="file-actions">
          <button type="button" class="btn-primary" data-file-download><svg class="ico ico-sm"><use href="#i-download"/></svg>下载选中</button>
          <button type="button" class="btn-ghost hidden" data-file-clear>取消全选</button>
        </div>
        <div class="file-status" data-file-status></div>
      </div>`;
    document.body.appendChild(root);
    filePanel = root;

    // 拖拽
    const bar = root.querySelector('[data-file-drag]');
    bar.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      const sx = e.clientX, sy = e.clientY;
      const ol = root.offsetLeft, ot = root.offsetTop;
      const move = (ev) => { root.style.left = (ol + ev.clientX - sx) + 'px'; root.style.top = (ot + ev.clientY - sy) + 'px'; };
      const up = () => { document.removeEventListener('pointermove', move); document.removeEventListener('pointerup', up); };
      document.addEventListener('pointermove', move);
      document.addEventListener('pointerup', up);
    });

    root.querySelector('[data-file-close]').addEventListener('click', () => {
      root.remove();
      filePanel = null;
      fileEntries = [];
    });
    root.querySelector('[data-file-up]').addEventListener('click', () => {
      const parent = parentPath(filePath);
      if (parent === filePath) return;
      filePath = parent;
      renderFileList();
    });
    root.querySelector('[data-file-go]').addEventListener('click', () => {
      const v = root.querySelector('[data-file-path]').value.trim();
      if (v) { filePath = ensureSlash(v); renderFileList(); }
    });
    root.querySelector('[data-file-path]').addEventListener('keydown', (e) => {
      if (e.key === 'Enter') root.querySelector('[data-file-go]').click();
    });
    root.querySelector('[data-file-download]').addEventListener('click', downloadSelected);
    root.querySelector('[data-file-clear]').addEventListener('click', () => renderFileList());

    root.style.left = '160px';
    root.style.top = '100px';
    root.style.zIndex = ++topZ;
  }

  function fileStatus(msg, isErr) {
    if (!filePanel) return;
    const el = filePanel.querySelector('[data-file-status]');
    el.textContent = msg || '';
    el.classList.toggle('err', !!isErr);
  }

  // parentPath 返回上一级目录（保持以 / 结尾，根目录返回自身）。
  function parentPath(p) {
    const trimmed = p.replace(/\/+$/, '');
    if (trimmed === '') return '/';
    const idx = trimmed.lastIndexOf('/');
    if (idx <= 0) return '/';
    return trimmed.slice(0, idx + 1);
  }

  // ensureSlash 给目录补结尾斜杠。
  function ensureSlash(p) {
    return p.endsWith('/') ? p : p + '/';
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));
  }

  function formatSize(n) {
    if (n == null || n <= 0) return '—';
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(1) + ' GB';
  }

  // renderFileList 拉取当前目录并渲染。缩略图懒加载：图片项先显示占位，
  // 进入视口后再请求 thumb 端点。
  async function renderFileList() {
    if (!filePanel || !fileSerial) return;
    const list = filePanel.querySelector('[data-file-list]');
    const pathInput = filePanel.querySelector('[data-file-path]');
    pathInput.value = filePath;
    list.innerHTML = '<div class="file-empty">加载中…</div>';
    fileStatus('');
    try {
      const r = await authFetch(`/api/admin/files/list?serial=${encodeURIComponent(fileSerial)}&path=${encodeURIComponent(filePath)}`);
      if (!r.ok) { fileStatus(r.json?.error || '列出目录失败', true); list.innerHTML = '<div class="file-empty">加载失败</div>'; return; }
      fileEntries = r.json.files || [];
      // 目录在前，文件在后，各自按名称排序。
      fileEntries.sort((a, b) => (a.IsDir === b.IsDir ? a.Name.localeCompare(b.Name) : a.IsDir ? -1 : 1));
      if (!fileEntries.length) { list.innerHTML = '<div class="file-empty">空目录</div>'; return; }

      list.innerHTML = fileEntries.map((f, i) => {
        const icon = f.IsDir
          ? '<svg class="ico ico-sm file-ico-dir"><use href="#i-folder"/></svg>'
          : (f.IsImage
            ? `<div class="file-thumb" data-thumb="${i}"><svg class="ico ico-sm"><use href="#i-image"/></svg></div>`
            : '<svg class="ico ico-sm file-ico-file"><use href="#i-file"/></svg>');
        return `
          <div class="file-row ${f.IsDir ? 'is-dir' : ''}" data-row="${i}" data-path="${escapeHtml(joinPath(filePath, f.Name))}">
            <input type="checkbox" class="file-check" data-check="${i}">
            ${icon}
            <span class="file-name">${escapeHtml(f.Name)}</span>
            <span class="file-size">${f.IsDir ? '' : formatSize(f.Size)}</span>
          </div>`;
      }).join('');

      // 目录点击进入；文件点击切换勾选。
      list.querySelectorAll('[data-row]').forEach((row) => {
        const i = Number(row.dataset.row);
        const f = fileEntries[i];
        const check = row.querySelector('[data-check]');
        row.addEventListener('click', (e) => {
          if (e.target === check) return;
          if (f.IsDir) { filePath = ensureSlash(row.dataset.path); renderFileList(); }
          else { check.checked = !check.checked; updateClearBtn(); }
        });
        check.addEventListener('change', updateClearBtn);
      });

      // 缩略图懒加载：IntersectionObserver 进入视口后请求。
      const obs = ('IntersectionObserver' in window) ? new IntersectionObserver((entries) => {
        entries.forEach((en) => {
          if (!en.isIntersecting) return;
          obs.unobserve(en.target);
          loadThumb(en.target);
        });
      }, { root: list, rootMargin: '80px' }) : null;
      list.querySelectorAll('[data-thumb]').forEach((el) => {
        if (obs) obs.observe(el); else loadThumb(el);
      });

      updateClearBtn();
    } catch (e) {
      fileStatus('列出目录失败：' + e.message, true);
      list.innerHTML = '<div class="file-empty">加载失败</div>';
    }
  }

  function joinPath(dir, name) {
    return (dir.endsWith('/') ? dir : dir + '/') + name;
  }

  function updateClearBtn() {
    if (!filePanel) return;
    const any = filePanel.querySelector('[data-check]:checked');
    const btn = filePanel.querySelector('[data-file-clear]');
    btn.classList.toggle('hidden', !any);
  }

  // loadThumb 请求缩略图并填充占位容器。
  async function loadThumb(el) {
    const i = Number(el.dataset.thumb);
    const f = fileEntries[i];
    if (!f) return;
    const url = `/api/admin/files/thumb?serial=${encodeURIComponent(fileSerial)}&path=${encodeURIComponent(joinPath(filePath, f.Name))}`;
    try {
      const res = await fetch(apiBase + url, { headers: { Authorization: `Bearer ${token}` } });
      if (!res.ok) { el.classList.add('file-thumb-none'); return; }
      const blob = await res.blob();
      const objUrl = URL.createObjectURL(blob);
      const img = document.createElement('img');
      img.onload = () => { el.innerHTML = ''; el.appendChild(img); };
      img.onerror = () => { el.classList.add('file-thumb-none'); URL.revokeObjectURL(objUrl); };
      img.src = objUrl;
    } catch (_) {
      el.classList.add('file-thumb-none');
    }
  }

  // downloadSelected 逐一下载勾选的文件，触发浏览器「另存为」。
  // 仅统计普通文件（目录被跳过）；失败时保留具体原因，不被汇总文案覆盖。
  //
  // 用浏览器原生下载（<a download> 直接导航）而非 fetch + blob 内存缓冲：
  // 大文件经 Wi-Fi 拉取耗时十几秒，fetch 缓冲整个响应体在部分环境下会被
  // 浏览器/中间层重置连接（报 "Failed to fetch"）；原生下载走浏览器下载引擎，
  // 有进度条且更稳定。token 通过 query 传递（后端 bearer 已支持 query fallback）。
  async function downloadSelected() {
    if (!filePanel || !fileSerial) return;
    const checks = Array.from(filePanel.querySelectorAll('[data-check]:checked'))
      .map((c) => fileEntries[Number(c.dataset.check)])
      .filter((f) => f && !f.IsDir);
    if (!checks.length) { fileStatus('请先勾选要下载的文件（目录不支持整目录下载）', true); return; }
    const total = checks.length;
    let done = 0;
    const failures = [];
    for (const f of checks) {
      fileStatus(`正在下载 ${f.Name}（${done + 1}/${total}）…`);
      const url = apiBase + `/api/admin/files/download?serial=${encodeURIComponent(fileSerial)}&path=${encodeURIComponent(joinPath(filePath, f.Name))}&token=${encodeURIComponent(token || '')}`;
      try {
        const ok = await nativeDownload(url, f.Name);
        if (!ok) { failures.push(`${f.Name}：下载被取消或失败`); continue; }
        done++;
      } catch (e) {
        failures.push(`${f.Name}：${e.message}`);
      }
    }
    if (failures.length) {
      fileStatus(`下载失败 ${failures.length}/${total}：\n${failures.join('\n')}`, true);
    } else if (done === total) {
      fileStatus(`已下载 ${done} 个文件（保存到浏览器默认下载目录）`);
    } else {
      fileStatus(`完成，成功 ${done}/${total}`, true);
    }
  }

  // nativeDownload 用隐藏 <a download> 触发浏览器原生下载。原生下载由浏览器
  // 下载引擎处理（带进度条、稳定、可断点续传），绕开 fetch 缓冲大文件的坑。
  // 无法可靠拿到「是否真正完成」，因此仅返回「是否已触发」。
  function nativeDownload(url, name) {
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    a.style.display = 'none';
    document.body.appendChild(a);
    a.click();
    setTimeout(() => a.remove(), 1000);
    return Promise.resolve(true);
  }

  // ============ 镜像工作台（固定镜像组 + 主机大屏 + 从机自适应网格） ============
  // 与上方的「临时群控面板」并行：临时群控仍需先手动开多窗口勾选建组；
  // 镜像工作台则基于「固定镜像组」一键拉起，复用底层的 /api/scrcpy/sync 群控引擎。
  //
  // 布局形态（对应需求「主机单独拎出 + 从机自适应网格」）：
  //   - 左侧：主机大屏，可交互（点/滑/打字），控制事件经后端归一化广播到从机。
  //   - 右侧：从机自适应网格（repeat(auto-fill, minmax(...))），纯监视，点击换主控。
  //
  // 关键约束：后端每台设备同一时刻只允许一个观看会话（tryAcquire）。
  // 因此每台成员各连一条 /api/scrcpy/ws 视频流（群控低配参数），主机流额外负责发控制。
  // 「换主控」= 后端 sync-set-master 换主控 serial + 前端把「发控制」职责切到新主机流。

  let mirrorWs = null;          // 镜像工作台用的群控管理 WS（/api/scrcpy/sync）
  let mirrorGroupId = null;     // 当前工作台对应的临时群控组 id
  let mirrorMasterSerial = null;// 当前主控 serial（ip:真实端口）
  let mirrorTiles = new Map();  // serial -> MirrorTile
  let mirrorRoot = null;        // 工作台 DOM 根

  // MirrorTile：工作台内一台设备的一个「视频瓦片」。独立于 ScrcpySession，
  // 不复用浮动窗口，只复用 pickCodecConfig / hasIdr / isSpsPpsOnly 等纯函数，
  // 自己管理 WS 视频流 + WebCodecs 解码 + canvas 上屏 +（仅主控）控制发送。
  class MirrorTile {
    constructor(serial, name, deviceId) {
      this.serial = serial;
      this.name = name || serial;
      this.deviceId = deviceId;
      this.ws = null;
      this.decoder = null;
      this.ctx = null;
      this.latestFrame = null;
      this.rafScheduled = false;
      this.lastFrameTs = 0;
      this.pendingSpsPps = null;
      this.screenW = 0;
      this.screenH = 0;
      this.alive = false;
      this.lastError = null;
      this.isMaster = false;
      this.onReady = null;   // 解码器就绪（会话登记完成）回调，供一键镜像等待全员就绪
      this.onPick = null;
      this.frameCount = 0;   // 本秒解码帧数（诊断用）
      this.fpsText = '';     // 最近一次 FPS 快照（显示在状态栏）
      this.fpsTimer = null;

      this.el = document.createElement('div');
      this.el.className = 'mirror-tile';
      this.el.innerHTML = `
        <div class="mirror-tile-head">
          <span class="mirror-tile-badge" data-master-badge>主控</span>
          <span class="mirror-tile-name">${escapeHtml(this.name)}</span>
          <span class="mirror-tile-state" data-state>连接中…</span>
        </div>
        <div class="mirror-tile-body"><canvas data-canvas></canvas></div>`;
      this.canvas = this.el.querySelector('[data-canvas]');
      this.stateEl = this.el.querySelector('[data-state]');
      this.badgeEl = this.el.querySelector('[data-master-badge]');
      this.el.addEventListener('click', () => this.onPick && this.onPick(this));
    }

    setState(msg, err) {
      // 追加帧率信息，便于诊断「黑屏」：0 fps 说明帧没到/没解码，>0 说明是渲染问题。
      const fps = this.fpsText ? ` · ${this.fpsText}` : '';
      this.stateEl.textContent = (msg || '') + fps;
      this.stateEl.classList.toggle('err', !!err);
    }

    setMaster(isMaster) {
      this.isMaster = isMaster;
      this.badgeEl.classList.toggle('hidden', !isMaster);
      this.el.classList.toggle('is-master', isMaster);
    }

    // connect 建立视频流（群控低配参数）。opts 透传 maxSize/bitrate/fps。
    connect(opts) {
      // 重连前只清理旧资源（ws/decoder/帧），**不移除 DOM**。el 已在
      // buildMirrorWorkbench 里挂到主控槽/网格槽，若这里调 close() 会把 el 从
      // DOM 摘掉，导致 canvas 不可见（黑屏）。仅当真正关闭工作台时才 remove。
      this.reset();
      const q = new URLSearchParams({ serial: this.serial });
      if (opts) {
        if (opts.maxSize) q.set('maxSize', opts.maxSize);
        if (opts.bitrate) q.set('bitrate', opts.bitrate);
        if (opts.fps) q.set('fps', opts.fps);
      }
      const sock = new WebSocket(`${scrcpyWsBase}/api/scrcpy/ws?${q.toString()}`);
      sock.binaryType = 'arraybuffer';
      this.ws = sock;
      this.alive = true;
      this.lastError = null;

      sock.onopen = () => this.setState('等待视频…');
      sock.onmessage = async (event) => {
        if (typeof event.data === 'string') {
          try {
            const msg = JSON.parse(event.data);
            if (msg.type === 'codec') await this.initDecoder(msg.codec, msg.width, msg.height);
            else if (msg.type === 'error') {
              this.lastError = msg.message || '未知错误';
              // 撞上「设备仍被占用」（浮动窗口刚关闭、后端 release 尚未完成）时
              // 自动重试一次，而非直接判失败。
              if (/already being controlled|being controlled/i.test(this.lastError) && !this._retriedOccupied) {
                this._retriedOccupied = true;
                this.setState('等待释放…');
                // 主动关闭 WS，让后端本次会话立刻结束、释放观看锁（release 依赖
                // 请求 context 结束）；否则后端 goroutine 可能卡住不释放锁，导致
                // 重试永远撞上 "device already being controlled"。
                try { sock.close(); } catch (_) {}
                setTimeout(() => this.connect(opts), 600);
                return;
              }
              this.setState('错误：' + this.lastError, true);
              // 错误后主动关闭 WS：避免后端 handler 因前端未断开而卡在等待
              // 视频分辨率上，造成观看锁泄漏（后续同 serial 永久无法连接）。
              try { sock.close(); } catch (_) {}
            }
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
        } catch (e) { console.warn('mirror decode chunk failed', e); }
      };
      sock.onclose = () => {
        if (this.ws !== sock) return;
        const err = this.lastError;
        this.lastError = null;
        this.alive = false;
        if (err) this.setState('失败：' + err, true);
        else this.setState('已断开');
      };
      sock.onerror = () => this.setState('连接失败', true);
    }

    async initDecoder(codec, width, height) {
      this.screenW = width;
      this.screenH = height;
      this.canvas.width = width;
      this.canvas.height = height;
      this.canvas.style.setProperty('--mirror-ratio', `${width} / ${height}`);
      this.ctx = this.canvas.getContext('2d');
      if (typeof VideoDecoder === 'undefined') { this.setState('浏览器不支持 WebCodecs', true); return; }
      try {
        this.decoder = new VideoDecoder({
          output: (frame) => {
            this.frameCount++;
            if (this.latestFrame) { try { this.latestFrame.close(); } catch (_) {} }
            this.latestFrame = frame;
            this.scheduleRender();
          },
          error: (e) => { console.warn('mirror decode error:', e); this.setState('解码错误', true); },
        });
        const cfg = await pickCodecConfig(codec, width, height);
        this.decoder.configure(cfg);
        this.setState(`${width}x${height}`);
        this.startFpsCounter();
        // 解码器就绪 = 后端已完成 registerSession（codec 头之后登记会话），
        // 此刻可安全建群控组。触发 onReady 供 launchMirrorGroup 等待全员就绪。
        if (this.onReady) { try { this.onReady(this); } catch (_) {} }
      } catch (e) {
        this.setState('解码初始化失败', true);
      }
    }

    // startFpsCounter 每秒把本秒解码出的帧数快照到状态栏（诊断黑屏用）。
    startFpsCounter() {
      if (this.fpsTimer) return;
      this.fpsTimer = setInterval(() => {
        this.fpsText = `${this.frameCount} fps`;
        this.frameCount = 0;
        // 状态栏已含 fps 后缀，仅在非错误态刷新显示。
        if (!this.stateEl.classList.contains('err')) {
          const base = this.stateEl.textContent.split(' · ')[0];
          this.stateEl.textContent = base + ' · ' + this.fpsText;
        }
      }, 1000);
    }

    stopFpsCounter() {
      if (this.fpsTimer) { clearInterval(this.fpsTimer); this.fpsTimer = null; }
      this.fpsText = '';
      this.frameCount = 0;
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

    // sendControl 仅主控瓦片使用：把控制事件发给后端，由后端归一化广播。
    // isMaster 守卫：换主控后旧主控虽残留监听器，但不再发送控制事件。
    sendControl(msg) {
      if (!this.isMaster) return;
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

    // bindMasterControl 给主控瓦片的 canvas 绑定交互（点/滑/滚/键盘）。幂等。
    bindMasterControl() {
      if (this._masterBound) return;
      this._masterBound = true;
      const canvas = this.canvas;
      const SLOP = 4;
      let down = false, downX = 0, downY = 0, dragging = false;
      const kb = this._ensureKbInput();

      canvas.addEventListener('pointerdown', (e) => {
        canvas.setPointerCapture(e.pointerId);
        kb.focus();
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
    }

    // _ensureKbInput 在主机容器里放一个隐藏 textarea 接收键盘（含中文 IME）。
    _ensureKbInput() {
      if (this._kb) return this._kb;
      const kb = document.createElement('textarea');
      kb.className = 'mirror-kb-input';
      kb.autocomplete = 'off';
      kb.autocorrect = 'off';
      kb.autocapitalize = 'off';
      kb.spellcheck = false;
      this.el.appendChild(kb);
      this._kb = kb;
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
      return kb;
    }

    // reset 清理会话资源（ws/decoder/帧/FPS），保留 el 在 DOM。供 connect 重连时
    // 复用，避免误移除已挂载的 DOM 元素。
    reset() {
      this.alive = false;
      this.stopFpsCounter();
      if (this.latestFrame) { try { this.latestFrame.close(); } catch (_) {} this.latestFrame = null; }
      if (this.decoder) { try { this.decoder.close(); } catch (_) {} this.decoder = null; }
      if (this.ws) { try { this.ws.close(); } catch (_) {} this.ws = null; }
      this.pendingSpsPps = null;
      this._retriedOccupied = false;
    }

    close() {
      this.reset();
      this.el.remove();
    }
  }

  // 固定镜像组结构：{ id, name, masterId, deviceIds }。
  // deviceIds 是 device.id；渲染时需按 device.id 找到当前 serial（adbIp:adbPort）。

  // loadSyncGroups 拉取固定镜像组列表（供管理 UI）。
  async function loadSyncGroups() {
    const r = await api('/api/admin/sync-groups');
    return r || [];
  }

  // resolveSerial 由 deviceId 找到 adb serial。依赖全局 devices（index.html 里加载）。
  // 无线调试端口动态分配，优先用落库的 adbPort；缺失（旧数据）时兜底 5555。
  function resolveSerial(deviceId) {
    const dev = (typeof devices !== 'undefined' && Array.isArray(devices)) ? devices.find((d) => d.id === deviceId) : null;
    if (!dev || !dev.adbIp) return null;
    const port = dev.adbPort || 5555;
    return dev.adbIp + ':' + port;
  }

  function deviceName(deviceId) {
    const dev = (typeof devices !== 'undefined' && Array.isArray(devices)) ? devices.find((d) => d.id === deviceId) : null;
    return dev ? (dev.name || dev.model || deviceId) : deviceId;
  }

  // launchMirrorGroup 一键镜像：读固定镜像组成员，自动拉起每台设备的屏幕控制，
  // 建立临时群控组并打开工作台。无需设备预先处于控屏状态——只要 ADB 已配对/
  // 连接（adb devices 中 device 状态），工作台会逐台自动部署并启动 scrcpy。
  window.launchMirrorGroup = async function (groupId) {
    // 关闭旧工作台（若在重复镜像，先释放旧会话）。
    closeMirrorWorkbench();

    let grp = null;
    try {
      const groups = await loadSyncGroups();
      grp = groups.find((g) => g.id === groupId);
    } catch (e) {
      toast('加载镜像组失败：' + e.message);
      return;
    }
    if (!grp || !grp.deviceIds || grp.deviceIds.length < 2) {
      toast('镜像组无效：至少需要 2 台设备');
      return;
    }

    // 拉取 ADB 当前在线（已配对+连接）的设备 serial 集合，作为「可被开启控屏」
    // 的判定依据。serial 形如 ip:port，取其 IP 用于与设备 adbIp 匹配（端口可能变化）。
    let onlineIPs = new Set();
    try {
      const r = await api('/api/admin/scrcpy/online-devices');
      for (const s of (r && r.serials) || []) {
        const ip = String(s).split(':')[0];
        if (ip) onlineIPs.add(ip);
      }
    } catch (e) {
      toast('查询 ADB 在线设备失败：' + e.message);
      return;
    }

    // 解析每个成员的 serial：用落库/心跳的 adbIp 拼 serial，并用 onlineIPs 校验
    // 该 IP 当前确实 ADB 在线（防止 adbIp 过期）。缺 adbIp 或已掉线则中止提示。
    const members = [];
    for (const did of grp.deviceIds) {
      const dev = (typeof devices !== 'undefined' && Array.isArray(devices))
        ? devices.find((d) => d && d.id === did) : null;
      const ip = dev && dev.adbIp ? dev.adbIp : '';
      const name = deviceName(did);
      if (!ip) {
        toast(`设备「${name}」缺少连接 IP，请先在设备清单「连接设备」后重试`);
        return;
      }
      // 有在线列表时，校验 IP 是否仍在线（端口无关，比较 IP 即可）。
      if (onlineIPs.size > 0 && !onlineIPs.has(ip)) {
        toast(`设备「${name}」ADB 已掉线（${ip}），请重新连接后再镜像`);
        return;
      }
      // 端口用落库的真实端口（无线调试动态分配），缺失时兜底 5555。
      const port = (dev && dev.adbPort) ? dev.adbPort : 5555;
      members.push({ deviceId: did, serial: ip + ':' + port, name });
    }

    // 主控：优先 grp.masterId，否则取第一个。
    let masterSerial = null;
    if (grp.masterId) {
      const m = members.find((x) => x.deviceId === grp.masterId);
      masterSerial = m ? m.serial : members[0].serial;
    } else {
      masterSerial = members[0].serial;
    }

    // 一键镜像接管：关闭组内所有已打开的独立浮动窗口，释放后端的观看锁
    // （tryAcquire），避免镜像工作台的视频流与浮动窗口抢同一 serial 失败。
    let closedAny = false;
    if (typeof sessions !== 'undefined' && sessions) {
      for (const m of members) {
        const existing = sessions.get(m.deviceId);
        if (existing) { try { existing.close(); closedAny = true; } catch (_) {} }
      }
    }
    // 浮动窗口的 WS 关闭是异步的，后端 release 需要一点时间；稍等再拉起，
    // 避免镜像视频流立刻撞上尚未释放的观看锁。
    if (closedAny) await new Promise((r) => setTimeout(r, 400));

    // 先打开工作台并逐台连接视频流（MirrorTile.connect 会触发后端 prepare 自动
    // 部署+启动 scrcpy-server，即「自动拉起屏幕控制」），等全员会话就绪后再建群控组。
    const tempGroupId = 'mirror-' + Date.now();
    const tiles = buildMirrorWorkbench(grp.name, members, masterSerial);

    // 等待所有成员解码器就绪（= 后端 registerSession 完成）。
    try {
      await waitTilesReady(tiles);
    } catch (e) {
      toast('拉起镜像失败：' + e.message);
      closeMirrorWorkbench();
      return;
    }

    // 建临时群控组（复用底层引擎）。此时所有成员均已登记会话。
    // 前端 onReady 可能略早于后端 registerSession 完成（resolution 通道竞态），
    // 故 sync-create 失败时稍作等待重试一次，规避 "device has no active session"。
    const serials = members.map((m) => m.serial);
    try {
      await sendSync({ type: 'sync-create', groupId: tempGroupId, serials, master: masterSerial });
    } catch (e) {
      await new Promise((r) => setTimeout(r, 600));
      try {
        await sendSync({ type: 'sync-create', groupId: tempGroupId, serials, master: masterSerial });
      } catch (e2) {
        toast('创建群控组失败：' + e2.message);
        closeMirrorWorkbench();
        return;
      }
    }
    mirrorGroupId = tempGroupId;
    mirrorMasterSerial = masterSerial;
    toast(`已拉起 ${members.length} 台设备，主控：${deviceName(grp.masterId || members[0].deviceId)}`);
  };

  // waitTilesReady 等待所有镜像瓦片解码器就绪（会话登记完成），任一失败即报错。
  function waitTilesReady(tiles) {
    return new Promise((resolve, reject) => {
      const arr = Array.from(tiles.values());
      let ready = 0;
      let failed = 0;
      let settled = false;
      let timer = null;
      const settle = (fn) => { if (settled) return; settled = true; if (timer) clearInterval(timer); fn(); };
      const check = () => {
        if (failed > 0) { settle(() => reject(new Error(`有 ${failed} 台设备连接失败`))); return; }
        if (ready >= arr.length) { settle(() => resolve()); }
      };
      // 每 500ms 轮询兜底：若某瓦片进入「错误/断开/失败」状态则计为失败。
      timer = setInterval(() => {
        for (const t of arr) {
          if (!t._settled) {
            const s = t.stateEl.textContent || '';
            if (t.lastError || s.startsWith('失败') || s.startsWith('错误') || s.startsWith('连接失败') || s === '已断开') {
              t._settled = true; failed++;
            }
          }
        }
        check();
      }, 500);
      for (const t of arr) {
        t._settled = false;
        t.onReady = () => { if (!t._settled) { t._settled = true; ready++; check(); } };
      }
      // 超时兜底：12 秒未全员就绪则报错。
      setTimeout(() => settle(() => reject(new Error('连接超时，请检查设备网络'))), 12000);
    });
  }

  // buildMirrorWorkbench 构建工作台 DOM 并启动所有成员的视频流。
  // 返回 mirrorTiles（serial -> MirrorTile），供调用方等待全员会话就绪。
  function buildMirrorWorkbench(name, members, masterSerial) {
    const root = document.createElement('div');
    root.className = 'mirror-workbench';
    root.innerHTML = `
      <div class="mirror-bar">
        <span class="mirror-title">镜像工作台 · ${escapeHtml(name)}</span>
        <span class="mirror-meta">${members.length} 台设备 · 正在拉起…</span>
        <button type="button" class="mirror-close" data-close>✕</button>
      </div>
      <div class="mirror-layout">
        <div class="mirror-master-zone" data-master-zone>
          <div class="mirror-zone-label">主控</div>
          <div class="mirror-master-slot" data-master-slot></div>
        </div>
        <div class="mirror-grid-zone">
          <div class="mirror-zone-label">从机</div>
          <div class="mirror-grid" data-grid></div>
        </div>
      </div>`;
    document.body.appendChild(root);
    mirrorRoot = root;
    root.style.zIndex = ++topZ;

    root.querySelector('[data-close]').addEventListener('click', closeMirrorWorkbench);

    const masterSlot = root.querySelector('[data-master-slot]');
    const grid = root.querySelector('[data-grid]');
    mirrorTiles.clear();

    for (const m of members) {
      const tile = new MirrorTile(m.serial, m.name, m.deviceId);
      tile.onPick = (t) => changeMirrorMaster(t);
      mirrorTiles.set(m.serial, tile);
    }

    // 主控放入主控槽，从机放入网格槽；主控单独在大屏，从机进网格。
    const master = mirrorTiles.get(masterSerial);
    const slaves = members.filter((m) => m.serial !== masterSerial);

    master.setMaster(true);
    masterSlot.appendChild(master.el);
    master.bindMasterControl();

    for (const m of slaves) {
      const tile = mirrorTiles.get(m.serial);
      tile.setMaster(false);
      grid.appendChild(tile.el);
    }

    // 逐个连接视频流（群控低配参数）。connect 会触发后端 prepare 自动部署+
    // 启动 scrcpy-server，即「自动拉起屏幕控制」，无需设备预先处于控屏状态。
    for (const m of members) {
      mirrorTiles.get(m.serial).connect(SYNC_LOW_OPTS);
    }

    return mirrorTiles;
  }

  // changeMirrorMaster 点击从机换主控：后端 sync-set-master + 前端换槽位与交互。
  async function changeMirrorMaster(tile) {
    if (tile.serial === mirrorMasterSerial) return;
    try {
      await sendSync({ type: 'sync-set-master', groupId: mirrorGroupId, master: tile.serial });
    } catch (e) {
      toast('切换主控失败：' + e.message);
      return;
    }
    const oldMaster = mirrorTiles.get(mirrorMasterSerial);
    mirrorMasterSerial = tile.serial;

    const masterSlot = mirrorRoot.querySelector('[data-master-slot]');
    const grid = mirrorRoot.querySelector('[data-grid]');

    // 旧主控退回网格（解除主控样式，sendControl 的 isMaster 守卫会使其不再发控制）。
    oldMaster.setMaster(false);
    grid.appendChild(oldMaster.el);

    tile.setMaster(true);
    masterSlot.appendChild(tile.el);
    tile.bindMasterControl();

    toast(`主控已切换为「${tile.name}」`);
  }

  // closeMirrorWorkbench 关闭工作台：解散临时群控组并释放所有瓦片。
  function closeMirrorWorkbench() {
    if (mirrorGroupId) {
      try { sendSync({ type: 'sync-destroy', groupId: mirrorGroupId }); } catch (_) {}
      mirrorGroupId = null;
    }
    mirrorMasterSerial = null;
    for (const tile of mirrorTiles.values()) tile.close();
    mirrorTiles.clear();
    if (mirrorRoot) { mirrorRoot.remove(); mirrorRoot = null; }
  }
  window.closeMirrorWorkbench = closeMirrorWorkbench;

  // ===== 设备分组独立页面（sec-device-groups）=====
  // 把固定镜像组渲染到独立页面的表格 #syncGroupRows；与群控面板数据同源。

  // renderDeviceGroupsPage 渲染设备分组页面表格。
  async function renderDeviceGroupsPage() {
    const body = document.getElementById('syncGroupRows');
    if (!body) return;
    let groups = [];
    try { groups = await loadSyncGroups(); } catch (e) {
      body.innerHTML = '<tr><td colspan="5" class="table-empty">加载镜像组失败</td></tr>';
      return;
    }
    if (!groups.length) {
      body.innerHTML = '<tr><td colspan="5" class="table-empty">暂无镜像组，点右上角「添加」新建</td></tr>';
      updateDeviceGroupDelState();
      return;
    }
    body.innerHTML = groups.map((g) => {
      const ids = g.deviceIds || [];
      const names = ids.map((id) => deviceName(id));
      const masterName = g.masterId ? deviceName(g.masterId) : '—';
      const memberText = names.join('、') || '—';
      return `
        <tr data-gid="${escapeHtml(g.id)}">
          <td><input type="checkbox" class="row-check" data-key="${escapeHtml(g.id)}"></td>
          <td class="name-cell">${escapeHtml(g.name)}</td>
          <td>${escapeHtml(memberText)}</td>
          <td>${escapeHtml(masterName)}</td>
          <td class="action-cell">
            <div class="actions" style="flex-wrap:nowrap">
              <button type="button" class="primary" data-launch>一键镜像</button>
              <button type="button" class="secondary" data-edit>编辑</button>
              <button type="button" class="danger" data-del>删除</button>
            </div>
          </td>
        </tr>`;
    }).join('');
    body.querySelectorAll('[data-launch]').forEach((btn) => {
      btn.addEventListener('click', () => launchMirrorGroup(btn.closest('[data-gid]').dataset.gid));
    });
    body.querySelectorAll('[data-edit]').forEach((btn) => {
      btn.addEventListener('click', () => openEditSyncGroup(btn.closest('[data-gid]').dataset.gid));
    });
    body.querySelectorAll('[data-del]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const gid = btn.closest('[data-gid]').dataset.gid;
        if (!confirm('删除这个镜像组？')) return;
        try { await api('/api/admin/sync-groups/' + encodeURIComponent(gid), { method: 'DELETE' }); toast('已删除'); renderDeviceGroupsPage(); renderMirrorGroups(); }
        catch (e) { toast('删除失败：' + e.message); }
      });
    });
    updateDeviceGroupDelState();
  }

  // updateDeviceGroupDelState 根据勾选更新「删除」按钮禁用态。
  function updateDeviceGroupDelState() {
    const btn = document.querySelector('[data-syncgroup-del]');
    const body = document.getElementById('syncGroupRows');
    if (!btn || !body) return;
    const any = body.querySelectorAll('.row-check:checked').length > 0;
    btn.disabled = !any;
  }

  // deleteSelectedDeviceGroups 批量删除勾选的镜像组。
  async function deleteSelectedDeviceGroups() {
    const body = document.getElementById('syncGroupRows');
    if (!body) return;
    const cbs = [...body.querySelectorAll('.row-check:checked')];
    if (!cbs.length) return;
    if (!confirm(`确定删除选中的 ${cbs.length} 个镜像组？`)) return;
    let ok = 0, fail = 0;
    for (const cb of cbs) {
      try { await api('/api/admin/sync-groups/' + encodeURIComponent(cb.dataset.key), { method: 'DELETE' }); ok++; }
      catch (e) { fail++; }
    }
    toast(fail ? `删除完成：成功 ${ok}，失败 ${fail}` : `已删除 ${ok} 个镜像组`);
    renderDeviceGroupsPage();
    renderMirrorGroups();
  }

  window.renderDeviceGroupsPage = renderDeviceGroupsPage;
  window.openDeviceGroupAdd = openDeviceGroupAdd;
  window.deleteSelectedDeviceGroups = deleteSelectedDeviceGroups;
})();
