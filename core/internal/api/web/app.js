// wild-work3 Web UI —— 对接 gateway（/core/* 业务、/gw/* 管理）
const $$ = (sel) => document.querySelector(sel);
const $$all = (sel) => Array.from(document.querySelectorAll(sel));

// 统一 fetch 封装；401 统一拉起管理面板登录（gateway 管理鉴权）
async function api(path, opts = {}) {
  const r = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    ...opts,
  });
  if (r.status === 401) { showAdminLogin(); throw new Error('需要管理口令'); }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) { throw new Error(data.error || path + ' 请求失败'); }
  return data;
}

// safe 包住后台加载：失败只弹 toast，不让 Promise rejection 变成页面级报错。
function safe(p) { if (p && typeof p.catch === 'function') p.catch((e) => toast(e.message, 'err')); return p; }

// ---------- 轻提示 / 确认 / 信息框（替代原生 alert/confirm） ----------
function toast(msg, kind = '') {
  const wrap = $$('#toast');
  if (!wrap) return;
  const el = document.createElement('div');
  el.className = 'toast ' + kind;
  el.textContent = msg;
  wrap.appendChild(el);
  setTimeout(() => { el.style.transition = 'opacity .3s'; el.style.opacity = '0'; setTimeout(() => el.remove(), 320); }, 2600);
}

// confirmBox 返回 Promise<boolean>
function confirmBox(title, msg) {
  return new Promise((resolve) => {
    const ov = $$('#confirmOverlay'), ok = $$('#confirmOk'), cancel = $$('#confirmCancel');
    $$('#confirmTitle').textContent = title || '确认';
    $$('#confirmBody').innerHTML = msg || '';
    cancel.classList.remove('hidden');
    ok.classList.remove('hidden');
    ov.classList.remove('hidden');
    const done = (v) => {
      ov.classList.add('hidden');
      ok.removeEventListener('click', onOk); cancel.removeEventListener('click', onCancel);
      resolve(v);
    };
    const onOk = () => done(true), onCancel = () => done(false);
    ok.addEventListener('click', onOk); cancel.addEventListener('click', onCancel);
  });
}

// infoBox 仅展示信息（只有「确定」）
function infoBox(title, html) {
  return new Promise((resolve) => {
    const ov = $$('#confirmOverlay'), ok = $$('#confirmOk'), cancel = $$('#confirmCancel');
    $$('#confirmTitle').textContent = title || '信息';
    $$('#confirmBody').innerHTML = html || '';
    cancel.classList.add('hidden');
    ov.classList.remove('hidden');
    const onOk = () => { ov.classList.add('hidden'); ok.removeEventListener('click', onOk); resolve(); };
    ok.addEventListener('click', onOk);
  });
}

// 格式化
const fmt = {
  num: (n) => (n == null ? '—' : Number(n).toLocaleString('zh-CN')),
  int: (n) => (n == null ? '—' : Math.round(n).toLocaleString('zh-CN')),
  yuan: (n) => (n == null ? '—' : '¥ ' + Number(n).toFixed(1)),
};

// esc 转义 HTML 文本（插件/上游下发的内容一律经它才拼进 innerHTML）。
function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

const CH_COLORS = {
  wb: '#FF7A45', wbai: '#5B8FF9', qoder: '#5B8FF9', qodercn: '#5B8FF9', qodercom: '#5B8FF9',
  trae: '#00B5A5', traecode: '#00B5A5', minimax: '#9254DE',
  custom: '#52C41A', hook: '#FAAD14', lm: '#FAAD14',
};
const CH_CLASS = { workbuddy: 'wb', workbuddyai: 'wbai', traework: 'trae', 'custom-api': 'custom' };
function chClass(id) {
  if (id === 'billion' || id.includes('hook')) return 'hook';
  const key = id.split('-')[0];
  return CH_CLASS[id] || CH_CLASS[key] || (key in CH_COLORS ? key : 'custom');
}

// 自定义模型（multi_channels 插件）的实例配色：按实例序号黄金角取色，
// 多个 Provider 不会全共用一个绿（与旧版 customChannelColor 同口径，W3-44）。
const GOLDEN_ANGLE = 137.508;
const _chColorCache = {};
function hslToHex(h, s, l) {
  const f = (n) => {
    const k = (n + h / 30) % 12;
    const a = (s / 100) * Math.min(l / 100, 1 - l / 100);
    const c = l / 100 - a * Math.max(-1, Math.min(k - 3, Math.min(9 - k, 1)));
    return Math.round(c * 255).toString(16).padStart(2, '0');
  };
  return `#${f(0)}${f(8)}${f(4)}`;
}
function relLuminance(hex) {
  const v = [1, 3, 5].map((i) => parseInt(hex.substr(i, 2), 16) / 255)
    .map((c) => (c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4)));
  return 0.2126 * v[0] + 0.7152 * v[1] + 0.0722 * v[2];
}
function providerColor(id) {
  const arr = state.providerIds;
  const idx = arr.indexOf(id) < 0 ? arr.length : arr.indexOf(id);
  const ck = `${id}#${idx}`;
  if (_chColorCache[ck]) return _chColorCache[ck];
  const hue = (idx * GOLDEN_ANGLE) % 360;
  // 明度从 42% 逐级下压，直到白字对比 ≥ 4.5（黄/绿最亮，要压到 28% 左右）
  let hex = hslToHex(hue, 78, 42);
  for (let l = 42; l >= 22; l--) {
    hex = hslToHex(hue, 78, l);
    if (1.05 / (relLuminance(hex) + 0.05) >= 4.5) break;
  }
  return (_chColorCache[ck] = hex);
}
// 渠道展示名 / 自定义模型实例集 / 插件 config_schema，都在 loadChannelButtons() 里刷新。
let state = { selectedChannel: 'all', days: 7, channelNames: {}, providerIds: [], pluginSchema: {} };

// isProvider：该渠道实例属于某个「实例由用户自建」的插件（实例 ID 就是模型命名空间前缀）。
function isProvider(id) { return state.providerIds.includes(id); }
function chColor(id) { return isProvider(id) ? providerColor(id) : (CH_COLORS[chClass(id)] || CH_COLORS.custom); }

function avatar(channelId, name) {
  return `<span class="ch-avatar" style="background:${chColor(channelId)}">${(name || '?').slice(0, 1)}</span>`;
}
function chName(id) { return state.channelNames[id] || id; }

// ---------- 顶栏 ----------
// 监听地址/API Key/自启属 gateway（安全与生命周期）；渠道信息属 core。
async function loadTopbar() {
  const g = await api('/gw/state');
  const v = await api('/gw/version');
  state.gw = g;
  $$('#ver').textContent = 'v' + v.gateway;
  const coreVer = v.core ? ` · core v${v.core}` : '';
  $$('#serverLine').textContent = `本地服务${coreVer} · IPC v${v.protocol}`;
  $$('#apiAddr .val').textContent = `http://${g.listen || '127.0.0.1:7863'}/v1`;
  $$('#apiKeyDisplay .val').textContent = g.api_key || '(未设置)';
}

// ---------- tabs ----------
function switchTab(name) {
  $$all('.mtab').forEach((b) => b.classList.toggle('active', b.dataset.mtab === name));
  $$all('.mtab-body').forEach((b) => b.classList.toggle('hidden', b.id !== 'mtab' + name[0].toUpperCase() + name.slice(1)));
  if (name === 'accounts') safe(loadChannelButtons());
  if (name === 'usage') safe(loadUsage());
  if (name === 'stats') safe(loadStats());
  if (name === 'fees') safe(loadFees());
  if (name === 'plugins') safe(loadStore());
}
$$all('.mtab').forEach((b) => b.addEventListener('click', () => switchTab(b.dataset.mtab)));

// ---------- Tab: 账号管理 ----------
let accounts = [];
// 「显示不可用账号」开关（旧版 localStorage['acct-show-na']）：默认隐藏
// 冷却中 / 已停用 / 无额度的账号，避免一堆死号占满面板。
const SHOW_NA_KEY = 'ww3.acct-show-na';
let showNa = localStorage.getItem(SHOW_NA_KEY) === '1';
// 与旧版一致：渠道按展示名字节序排序，自定义模型（Provider 实例）始终放最后。
function chOrder(a, b) {
  const x = chName(a), y = chName(b);
  // 沿用旧版 R142：UTF-8 字节顺序（ASCII 名先于中文），自定义模型固定最后。
  return Number(isProvider(a)) - Number(isProvider(b)) || (x < y ? -1 : x > y ? 1 : 0);
}

function renderAccounts() {
  const list = $$('#acctList');
  // 按渠道分组（与旧版一致）
  const byCh = {};
  accounts.forEach((a) => (byCh[a.channel] = byCh[a.channel] || []).push(a));
  // 顶部信用汇总（始终统计全部账号，不受「显示不可用」影响）
  const cs = $$('#creditSummary');
  const entries = Object.entries(byCh).sort((a, b) => chOrder(a[0], b[0]));
  if (entries.length === 0) { cs.classList.add('hidden'); }
  else {
    cs.classList.remove('hidden');
    cs.innerHTML = '';
    entries.forEach(([ch, accts]) => {
      const creditSum = accts.reduce((s, a) => s + (a.credits || 0), 0);
      const tokenSum = accts.filter((a) => a.unit === 'token').length;
      cs.insertAdjacentHTML('beforeend',
        `<span class="cs-item"><b style="color:${chColor(ch)}">${esc(chName(ch))}</b><span class="cs-v">${fmt.yuan(creditSum)}</span></span>`);
      if (tokenSum > 0) cs.insertAdjacentHTML('beforeend',
        `<span class="cs-item">token 渠道 ${tokenSum} 个</span>`);
    });
  }
  // 「不可用」判定：冷却中 / 已停用 / 额度耗尽（与后端 unusable 语义一致）
  const unusable = (a) => !!(a.cooling || a.disabled || a.unusable);
  const hidden = showNa ? 0 : accounts.filter(unusable).length;
  const vis = accounts.filter((a) => showNa || !unusable(a));
  if (vis.length === 0) {
    $$('#acctEmpty').classList.remove('hidden');
    $$('#acctEmpty').textContent = hidden > 0
      ? `已隐藏 ${hidden} 个不可用账号（勾选「显示不可用账号」查看）`
      : '还没有账号，点击上方渠道按钮添加';
    list.innerHTML = '';
    return;
  }
  $$('#acctEmpty').classList.add('hidden');
  const visByCh = {};
  vis.forEach((a) => (visByCh[a.channel] = visByCh[a.channel] || []).push(a));
  list.innerHTML = Object.entries(visByCh)
    .sort((a, b) => chOrder(a[0], b[0]))
    .map(([ch, accts]) => `<div class="ch-group"><b>${esc(chName(ch))}</b><span>${accts.length} 个账号</span></div>` +
      accts.map(accountCard).join('')).join('');
}

function accountCard(a) {
  const badges = [];
  if (a.stale) badges.push('<span class="tooltip-badge warn">登录态</span>');
  if (a.cooling) badges.push(`<span class="tooltip-badge danger">冷却至 ${a.until || ''}</span>`);
  if (a.disabled) badges.push('<span class="tooltip-badge">已停用</span>');
  if (a.expiring > 0) badges.push(`<span class="tooltip-badge">将到期</span>`);
  const unit = a.unit === 'token' ? 'token' : '¥';
  const creditCls = a.unit === 'token' ? 'token-unit' : '';
  return `
    <div class="acct-card">
      <div class="acct-head">${avatar(a.channel, a.nickname)}
        <span class="nick" title="点击修改显示名" data-rename="${a.uid}" data-name="${(a.nickname || '').replace(/"/g, '&quot;')}">${a.nickname}</span>
        <span class="tooltip-badge">${a.channel}</span>
      </div>
      <div class="credits ${creditCls}">${unit} ${fmt.int(a.credits)}</div>
      <div class="acct-sub">
        <span>到期额度 ${fmt.yuan(a.expiring)}</span>
        ${badges.join('')}
      </div>
      <div class="acct-actions">
        <button class="btn small" data-acc="${a.uid}" data-act="refresh">刷新</button>
        <button class="btn small" data-acc="${a.uid}" data-act="checkin">签到</button>
        <button class="btn small" data-acc="${a.uid}" data-act="detail">明细</button>
        <button class="btn small ${a.disabled ? '' : 'danger'}" data-acc="${a.uid}" data-act="disable">${a.disabled ? '启用' : '停用'}</button>
      </div>
    </div>`;
}
async function loadAccounts() {
  accounts = await api('/core/state').then((s) => s.accounts);
  await loadChannelButtons();
  renderAccounts();
}
$$('#acctList').addEventListener('click', async (e) => {
  const btn = e.target.closest('[data-act]');
  if (!btn) return;
  const { acc, act } = btn.dataset;
  const map = {
    refresh: '/core/accounts/' + acc + '/refresh',
    checkin: '/core/accounts/' + acc + '/checkin',
    disable: '/core/accounts/' + acc + '/disable',
    detail: '/core/accounts/' + acc + '/resource_detail',
  };
  try {
    if (act === 'detail') {
      const items = await api(map.detail);
      showDetail(items);
      return;
    }
    if (act === 'disable' && !await confirmBox('停用账号', `确定要切换该账号的启用状态吗？<br><span class="ov-note">${btn.textContent.trim()}</span>`)) return;
    const r = await api(map[act]);
    toast(r.message || '成功', 'ok');
    loadAccounts();
  } catch (err) { toast('失败：' + err.message, 'err'); }
});
$$('#btnRefreshAll').addEventListener('click', async () => {
  // 全部账号刷新交给 core 后台串行执行（一次一个插件进程 + 间隔），前端不再自己循环发请求：
  // 前端 for 循环会在浏览器侧瞬间并发几十个请求，core 侧同时也是几十个进程（W3-20 要避免的）。
  try {
    const r = await api('/core/accounts/refresh_all', { method: 'POST' });
    toast(r.message || '已开始刷新全部账号', 'ok');
    setTimeout(loadAccounts, 1500);
  } catch (err) { toast('失败：' + err.message, 'err'); }
});
$$('#btnCheckinAll').addEventListener('click', async () => {
  if (!await confirmBox('全部签到', '确定要触发所有支持签到的账号签到吗？')) return;
  try {
    const r = await api('/core/accounts/checkin_all', { method: 'POST' });
    toast(r.message || '全部签到完成', 'ok'); loadAccounts();
  } catch (err) { toast('失败：' + err.message, 'err'); }
});
// 导入本机既有凭据（旧版 import_local）：core 遍历启用渠道，从插件私有目录读取凭据事实入库。
$$('#btnImportLocal').addEventListener('click', async () => {
  try {
    const r = await api('/core/accounts/sync', { method: 'POST' });
    toast(`已导入 ${r.imported || 0} 个账号`, 'ok'); loadAccounts();
  } catch (err) { toast('失败：' + err.message, 'err'); }
});
// 「显示不可用账号」开关（旧版 localStorage['acct-show-na']）：只影响渲染，不重新拉数据
const showNaBox = $$('#acctShowNa');
if (showNaBox) {
  showNaBox.checked = showNa;
  showNaBox.addEventListener('change', () => {
    showNa = showNaBox.checked;
    localStorage.setItem(SHOW_NA_KEY, showNa ? '1' : '0');
    renderAccounts();
  });
}
// 点账号昵称改名（旧版 #renameOverlay）：显示名是 core 本地字段，走 POST /core/accounts/{uid}/nickname。
let renameUid = null;
function openRename(uid, currentName) {
  renameUid = uid;
  $$('#rnUid').textContent = uid;
  $$('#rnInput').value = currentName || '';
  $$('#renameOverlay').classList.remove('hidden');
  $$('#rnInput').focus();
}
function closeRename() {
  $$('#renameOverlay').classList.add('hidden');
  renameUid = null;
}
$$('#acctList').addEventListener('click', (e) => {
  const el = e.target.closest('[data-rename]');
  if (el) openRename(el.dataset.rename, el.dataset.name);
});
$$('#btnRenameCancel').addEventListener('click', closeRename);
$$('#renameOverlay').addEventListener('click', (e) => { if (e.target.id === 'renameOverlay') closeRename(); });
$$('#rnInput').addEventListener('keydown', (e) => { if (e.key === 'Enter') $$('#btnRenameSave').click(); });
$$('#btnRenameSave').addEventListener('click', async () => {
  if (!renameUid) return;
  const nickname = $$('#rnInput').value.trim();
  if (!nickname) { toast('显示名不能为空', 'err'); return; }
  try {
    await api('/core/accounts/' + encodeURIComponent(renameUid) + '/nickname', { method: 'POST', body: JSON.stringify({ nickname }) });
    toast('显示名已更新', 'ok');
    closeRename();
    loadAccounts();
  } catch (err) { toast('失败：' + err.message, 'err'); }
});

function showDetail(items) {
  const rows = items.map((i) => {
    const remain = i.unit === 'token' ? fmt.int(i.remain) + ' token' : fmt.yuan(i.remain);
    return `<tr><td>${i.name}</td><td>${remain}</td><td>${i.info_only ? '仅展示' : '可用'}</td><td>${i.expire_at || '—'}</td></tr>`;
  }).join('');
  const box = $$('#detailOverlay') || createDetailOverlay();
  box.querySelector('.ov-body').innerHTML = `<table class="fee-table"><tr><th>名称</th><th>剩余</th><th>类型</th><th>到期</th></tr>${rows}</table>`;
  box.classList.remove('hidden');
}
function createDetailOverlay() {
  const d = document.createElement('div');
  d.id = 'detailOverlay';
  d.className = 'overlay hidden';
  d.innerHTML = `<div class="overlay-box"><h3>额度明细</h3><div class="ov-body"></div><div class="ov-actions"><button class="btn" id="detailClose">关闭</button></div></div>`;
  document.body.appendChild(d);
  d.querySelector('#detailClose').addEventListener('click', () => d.classList.add('hidden'));
  return d;
}

// 渠道入口按钮：普通渠道一个按钮；声明 multi_channels 的插件（自定义模型）一个「实例列表」入口。
// 渠道实例由插件自动声明/用户自建，core 不保留独立的渠道管理页（W3-43/W3-44）。
let channelButtons = [];      // 普通渠道实例（已启用）
let providerPlugins = [];     // 实例由用户自建的插件
async function loadChannelButtons() {
  const [chans, plugs] = await Promise.all([api('/core/channels'), api('/core/plugins')]);
  providerPlugins = (plugs || []).filter((p) => p.installed && p.multi_channels && p.kind !== 'hook');
  const multi = new Set(providerPlugins.map((p) => p.plugin_id));
  // providerIds 含所有实例（含停用），按 ID 定序，保证配色/排序不随启用状态跳变
  state.providerIds = chans.filter((c) => multi.has(c.plugin_id)).map((c) => c.channel_id).sort();
  state.channelNames = Object.fromEntries(chans.map((c) => [c.channel_id, c.display_name || c.channel_id]));
  state.pluginSchema = Object.fromEntries((plugs || []).map((p) => [p.plugin_id, p.config_schema || null]));
  channelButtons = chans.filter((c) => !multi.has(c.plugin_id) && c.enabled);
  const btns = channelButtons.slice().sort((a, b) => chOrder(a.channel_id, b.channel_id)).map((c) =>
    `<button class="btn primary alt ${chClass(c.channel_id)}" data-add-channel="${esc(c.channel_id)}" title="${esc(c.display_name || c.channel_id)}">＋ ${esc(c.display_name || c.channel_id)}</button>`);
  // 旧版把「自定义模型」钉在最末：一个插件一个入口，点开是 Provider 列表
  btns.push(...providerPlugins.map((p) =>
    `<button class="btn primary alt custom" data-provider-panel="${esc(p.plugin_id)}" title="${esc(p.description || p.plugin_id)}">＋ ${esc(p.name || p.plugin_id)}</button>`));
  $$('#accountAddButtons').innerHTML = btns.join('') || '<span class="ov-note">尚未安装渠道插件，请前往「插件与更新」</span>';
}
$$('#accountAddButtons').addEventListener('click', async (e) => {
  const entry = e.target.closest('[data-provider-panel]');
  if (entry) { safe(openCustomModels(entry.dataset.providerPanel)); return; }
  const btn = e.target.closest('[data-add-channel]');
  if (!btn) return;
  const id = btn.dataset.addChannel;
  const ch = channelButtons.find((c) => c.channel_id === id);
  if ((ch?.capabilities || []).includes('auth.begin')) safe(openLoginWizard(id));
  else if (state.pluginSchema?.[ch?.plugin_id]) safe(openChannelWizard(id));   // 无登录但要密钥/基址的渠道走配置表单
  else toast('该渠道无需登录，可直接使用', 'ok');
});

// ---------- Provider 列表（旧版 #customModelsOverlay 的等价物：一插件 N 实例，ID 即命名空间前缀） ----------
const cmOverlay = $$('#customModelsOverlay');
const peOverlay = $$('#providerEditor');
let cmPlugin = null;        // 当前管理的插件（name/config_schema）
let cmRows = [];            // 其实例详情（含脱敏 config 与 models）
let peEditing = null;       // 编辑中的渠道 ID；null=新增
let peModels = [];          // 「测试连接」带回来的模型清单，保存时一并下发

async function openCustomModels(pluginID) {
  const plugs = await api('/core/plugins');
  cmPlugin = (plugs || []).find((p) => p.plugin_id === pluginID);
  if (!cmPlugin) throw new Error('插件未安装：' + pluginID);
  $$('#cmTitle').textContent = cmPlugin.name || pluginID;
  await reloadProviderRows();
  cmOverlay.classList.remove('hidden');
}

async function reloadProviderRows() {
  const chans = await api('/core/channels');
  const own = (chans || []).filter((c) => c.plugin_id === cmPlugin.plugin_id);
  // 列表要显示基址与模型数，得逐实例取详情（实例数个位，且 GET 详情不会 spawn 插件进程）
  cmRows = await Promise.all(own.map((c) => api('/core/channels/' + encodeURIComponent(c.channel_id))
    .then((d) => Object.assign({}, c, d)).catch(() => ({ ...c, config: {}, models: [] }))));
  cmRows.sort((a, b) => (a.channel_id < b.channel_id ? -1 : a.channel_id > b.channel_id ? 1 : 0));
  renderProviderRows();
}

function renderProviderRows() {
  const list = $$('#cmList');
  if (!cmRows.length) { list.innerHTML = '<div class="cm-empty">还没有 Provider，点「＋ 新增 Provider」接入任意兼容端点。</div>'; return; }
  list.innerHTML = cmRows.map((r) => {
    const cfg = r.config || {}, models = r.models || [];
    return `<div class="cm-row">
      <span class="cm-dot" style="background:${chColor(r.channel_id)}"></span>
      <div class="cm-main"><b>${esc(r.display_name || r.channel_id)}</b><span class="cm-id">${esc(r.channel_id)}/</span></div>
      <span class="cm-url" title="${esc(cfg.base_url || '')}">${esc(cfg.base_url || '未配置基址')}</span>
      <span class="cm-meta">${models.length} 模型 · ${r.enabled ? '启用' : '停用'}</span>
      <span class="cm-ops">
        <button class="btn small" data-pe-edit="${esc(r.channel_id)}">编辑</button>
        <button class="btn small danger" data-pe-del="${esc(r.channel_id)}">删除</button>
      </span>
    </div>`;
  }).join('');
}

function peMsg(text, kind) {
  const el = $$('#peMsg');
  el.textContent = text;
  el.className = 'ov-note' + (kind ? ' ' + kind : '');
}

// 密钥明文只在用户**显式点击**时向后端取（W3-45：ww 也当凭据管理器用）。
// 明文不缓存、不回填 input.value 之外的地方，日志里也不留（core 侧审计只记字段名）。
function revealSecret(btn) {
  const key = btn.dataset.reveal;
  safe((async () => {
    const res = await api('/core/channels/' + encodeURIComponent(peEditing) + '/secret?key=' + encodeURIComponent(key));
    const input = $$all('#peSchema [data-key]').find((el) => el.dataset.key === key);
    if (!input) throw new Error('表单里没有字段 ' + key);
    input.value = res.value;
    input.dataset.revealed = '1';   // 标记为明文，复制/关闭前不重新脱敏
    btn.textContent = '已显示';
    btn.disabled = true;
    peMsg(`已显示 ${key} 明文（仅本页内存，不会写入日志）`, 'ok');
  })());
}

function openProviderEditor(channelID) {
  peEditing = channelID || null;
  peModels = [];
  const row = channelID ? cmRows.find((r) => r.channel_id === channelID) : null;
  $$('#peTitle').textContent = channelID ? '编辑 Provider：' + channelID : '新增 Provider';
  $$('#peID').value = channelID || '';
  // 标识即模型前缀；改名会迁移账号/池/账本/规则（W3-45），允许但不是零成本，故显式提示
  $$('#peID').disabled = false;
  $$('#peID').title = channelID ? '改名会同步迁移该渠道的账号、冷却状态、用量账本与渠道级规则' : '';
  $$('#peName').value = row ? row.display_name || '' : '';
  $$('#peSchema').innerHTML = cmPlugin?.config_schema
    ? schemaForm(cmPlugin.config_schema, (row && row.config) || {}, 'pe_', channelID ? cmPlugin.plugin_id : '')
    : '<div class="ov-tip">该插件未提供 config.schema</div>';
  peMsg('建议先「测试连接」：连通后会自动导入上游模型清单。');
  peOverlay.classList.remove('hidden');
  $$('#peID').focus();
}

async function deleteProvider(id) {
  if (!(await confirmBox('删除 Provider', `删除 <b>${esc(id)}</b> 及其账号？凭据与用量流水保留，随时可重建同名 Provider。`))) return;
  try {
    await api('/core/channels/' + encodeURIComponent(id), { method: 'DELETE' });
    toast('已删除 ' + id, 'ok');
    await Promise.all([reloadProviderRows(), loadAccounts()]);
  } catch (err) { toast('删除失败：' + err.message, 'err'); }
}

$$('#peSchema').addEventListener('click', (e) => {
  const btn = e.target.closest('[data-reveal]');
  if (btn) { safe(revealSecret(btn)); return; }
});

$$('#cmAdd').addEventListener('click', () => safe(openProviderEditor(null)));
$$('#cmClose').addEventListener('click', () => cmOverlay.classList.add('hidden'));
$$('#peCancel').addEventListener('click', () => peOverlay.classList.add('hidden'));
$$('#cmList').addEventListener('click', (e) => {
  const ed = e.target.closest('[data-pe-edit]');
  if (ed) { safe(openProviderEditor(ed.dataset.peEdit)); return; }
  const del = e.target.closest('[data-pe-del]');
  if (del) safe(deleteProvider(del.dataset.peDel));
});

$$('#peTest').addEventListener('click', async () => {
  const cfg = collectForm('peSchema');
  $$('#peTest').disabled = true;
  peMsg('测试中：用这份配置拉上游模型清单…');
  try {
    const res = await api('/core/channels/test', { method: 'POST', body: JSON.stringify(
      { plugin_id: cmPlugin.plugin_id, channel_id: peEditing || '', config: cfg }) });
    peModels = (res.models || []).map((m) => m.id || m).filter(Boolean);
    peMsg(`${res.message} · 模型 ${peModels.length} 个${res.ok ? '，保存后生效' : ''}`, res.ok ? 'ok' : 'err');
  } catch (err) { peMsg('测试失败：' + err.message, 'err'); }
  finally { $$('#peTest').disabled = false; }
});

$$('#peSave').addEventListener('click', async () => {
  const id = $$('#peID').value.trim(), name = $$('#peName').value.trim() || id;
  const cfg = collectForm('peSchema');
  if (!id) { peMsg('请填写渠道标识', 'err'); return; }
  for (const k of cmPlugin?.config_schema?.required || []) {
    if (cfg[k] == null || String(cfg[k]).trim() === '') { peMsg('请填写 ' + (cmPlugin.config_schema.properties?.[k]?.title || k), 'err'); return; }
  }
  $$('#peSave').disabled = true;
  const isEdit = !!peEditing;   // 先记下：下面会把 peEditing 更新成新 ID，之后就不能再拿它判新增/编辑
  try {
    if (isEdit) {
      // 带 channel_id = 改名请求（服务端会迁移账号/池/账本/规则，W3-45）
      await api('/core/channels/' + encodeURIComponent(peEditing), { method: 'PUT', body: JSON.stringify(
        { channel_id: id, display_name: name, config: cfg, ...(peModels.length ? { models: peModels } : {}) }) });
      peEditing = id;   // 后续同一编辑器内的「显示密钥」请求要用新 ID
    } else {
      await api('/core/channels', { method: 'POST', body: JSON.stringify(
        { channel_id: id, plugin_id: cmPlugin.plugin_id, display_name: name, config: cfg, models: peModels }) });
    }
    peOverlay.classList.add('hidden');
    toast(isEdit ? 'Provider 已保存' : 'Provider 已添加', 'ok');
    await Promise.all([reloadProviderRows(), loadAccounts()]);
  } catch (err) { peMsg('保存失败：' + err.message, 'err'); }
  finally { $$('#peSave').disabled = false; }
});

// 渠道配置表单仍用于需要 base_url/API Key 的单实例渠道（非 multi_channels 插件），不提供独立渠道管理页。
async function loadChannels() { return loadChannelButtons(); }

// ---------- 单实例渠道配置表单（由已安装插件的 config.schema 生成） ----------
const wizard = $$('#channelWizard');
let wizardCtx = { editing: null, pluginID: null, pluginName: '', schema: null, formVals: {} };

async function openChannelWizard(channelId) {
  const chans = await api('/core/channels');
  const ch = chans.find((c) => c.channel_id === channelId);
  if (!ch) throw new Error('渠道不存在或插件未安装：' + channelId);
  const detail = await api('/core/channels/' + encodeURIComponent(channelId));
  wizardCtx = { editing: channelId, pluginID: ch.plugin_id, pluginName: ch.display_name,
    schema: detail.config_schema, formVals: detail.config || {} };
  $$('#cwTitle').textContent = '配置 ' + (ch.display_name || ch.channel_id);
  wizard.classList.remove('hidden');
  renderWizard();
}

// 由 JSON Schema 动态生成表单（支持 string/integer/number/boolean/array/object）
//
// revealFor 非空时，password 字段旁额外给一个「显示密钥」按钮：明文只能由用户显式点击
// 向后端单独索取（W3-45），列表/详情接口永远只给脱敏值。
function schemaForm(schema, vals, idPrefix, revealFor) {
  const props = (schema && schema.properties) || {};
  const req = (schema && schema.required) || [];
  const html = (v) => esc(v);
  return Object.entries(props).map(([k, d]) => {
    const v = vals[k] == null ? (d.default == null ? '' : d.default) : vals[k];
    const label = (d.title || k) + (req.includes(k) ? ' *' : '');
    const id = idPrefix + k;
    if (Array.isArray(d.enum) && d.enum.length) {
      const names = Array.isArray(d.enumNames) ? d.enumNames : d.enum;
      return `<label class="frow">${html(label)}<select class="ipt" id="${html(id)}" data-key="${html(k)}" data-type="string">${d.enum.map((o, i) => `<option value="${html(o)}" ${String(v) === String(o) ? 'selected' : ''}>${html(names[i] || o)}</option>`).join('')}</select></label>`;
    }
    if (d.type === 'boolean') {
      return `<label class="frow"><input type="checkbox" id="${html(id)}" data-key="${html(k)}" data-type="boolean" ${v ? 'checked' : ''}> ${html(label)}</label>`;
    }
    if (d.type === 'array') {
      return `<label class="frow">${html(label)}<input class="ipt" id="${html(id)}" data-key="${html(k)}" data-type="array" value="${html(Array.isArray(v) ? v.join(', ') : v)}" placeholder="逗号分隔"></label>`;
    }
    if (d.type === 'object') {
      return `<label class="frow">${html(label)}<input class="ipt" id="${html(id)}" data-key="${html(k)}" data-type="object" value="${html(typeof v === 'object' ? JSON.stringify(v) : v)}" placeholder='{"Header":"值"}'></label>`;
    }
    const t = d.format === 'password' ? 'password' : (d.type === 'integer' || d.type === 'number' ? 'number' : 'text');
    const ph = d.description || d.default || '';
    // 密钥字段：给一个显式「显示」开关（只在编辑已有实例、且当前值是脱敏值时才有意义）
    const reveal = (revealFor && d.format === 'password')
      ? `<button type="button" class="btn small" data-reveal="${html(k)}" title="从本机 core 取回明文（不经日志）">显示密钥</button>`
      : '';
    return `<label class="frow">${html(label)}<span class="ipt-row"><input class="ipt" id="${html(id)}" data-key="${html(k)}" data-type="${html(d.type || 'string')}" placeholder="${html(ph)}" value="${html(v)}">${reveal}</span></label>`;
  }).join('');
}

function collectForm(idPrefix) {
  const out = {};
  $$all('#' + idPrefix + ' [data-key]').forEach((el) => {
    const k = el.dataset.key, t = el.dataset.type;
    if (t === 'boolean') out[k] = el.checked;
    else if (t === 'array') out[k] = el.value.split(',').map((x) => x.trim()).filter(Boolean);
    else if (t === 'object') { try { out[k] = JSON.parse(el.value); } catch (_) { out[k] = {}; } }
    else if (t === 'integer' || t === 'number') out[k] = el.value === '' ? null : Number(el.value);
    else out[k] = el.value;
  });
  return out;
}

function renderWizard() {
  $$('#cwNext').textContent = '保存';
  $$('#cwBody').innerHTML = `
    <div class="ov-note">配置 <b>${esc(wizardCtx.pluginName || wizardCtx.pluginID)}</b>（由插件 config.schema 生成）</div>
    <div id="fw">${wizardCtx.schema ? schemaForm(wizardCtx.schema, wizardCtx.formVals, 'sc_') : '<div class="ov-tip">该插件未提供 config.schema</div>'}</div>`;
}

$$('#cwCancel').addEventListener('click', () => { wizard.classList.add('hidden'); });
$$('#cwNext').addEventListener('click', async () => {
  const cfg = collectForm('cwBody');
  for (const k of wizardCtx.schema?.required || []) {
    if (cfg[k] == null || String(cfg[k]).trim() === '') { toast('请填写 ' + (wizardCtx.schema.properties?.[k]?.title || k), 'warn'); return; }
  }
  $$('#cwNext').disabled = true;
  try {
    await api('/core/channels/' + encodeURIComponent(wizardCtx.editing), { method: 'PUT', body: JSON.stringify({ config: cfg }) });
    wizard.classList.add('hidden');
    toast('渠道配置已保存', 'ok');
    safe(loadChannelButtons());
  } catch (err) { toast('保存失败：' + err.message, 'err'); }
  finally { $$('#cwNext').disabled = false; }
});

// ---------- Tab: 用量 ----------
async function loadUsage() {
  const u = await api('/core/usage?days=' + state.days);
  $$('#ucTokens').textContent = fmt.int(u.tokens);
  $$('#ucReqs').textContent = fmt.int(u.requests);
  $$('#ucSpend').textContent = fmt.yuan(u.spend);
  $$('#ucEarn').textContent = fmt.yuan(u.earn);
  $$('#ucCache').textContent = u.cache_ratio == null ? '—' : (u.cache_ratio * 100).toFixed(0) + '%';
  // 渠道分布条形
  const chart = $$('#usageChart');
  const max = Math.max(...u.by_channel.map((x) => x.tokens));
  chart.innerHTML = `<div class="bar-chart">${u.by_channel.map((c) => `
      <div class="bar-col"><div class="bar" style="height:${(c.tokens / max * 100).toFixed(1)}%"></div><div class="bar-label">${c.name}</div><div class="bar-label">${fmt.int(c.tokens)}</div></div>`).join('')}</div>`;
  const list = $$('#usageList');
  list.innerHTML = `<div class="fee-card"><table class="fee-table"><tr><th>渠道</th><th>Token</th><th>消耗</th></tr>
    ${u.by_channel.map((c) => `<tr><td>${c.name}</td><td>${fmt.int(c.tokens)}</td><td>${fmt.yuan(c.spend)}</td></tr>`).join('')}</table></div>`;
  // 模型榜（后端 /core/usage 的 by_model，按 token 降序前 20）
  const bm = u.by_model || [];
  $$('#usageModels').innerHTML = bm.length ? `<div class="fee-card"><h4>模型用量排行</h4>
    <table class="fee-table"><tr><th>模型</th><th>渠道</th><th>请求</th><th>Token</th></tr>
    ${bm.map((m) => `<tr><td>${m.model}</td><td>${chName(m.channel)}</td><td>${fmt.int(m.requests)}</td><td>${fmt.int(m.tokens)}</td></tr>`).join('')}</table></div>` : '';
  loadCredits();
}

// 积分流水（后端 /core/credits，已倒序：新的在前）
const CREDIT_KIND = { earn: ['入账', 'ok'], spend: ['消耗', 'warn'], expire: ['过期', 'danger'] };
async function loadCredits() {
  const rows = await api('/core/credits?days=' + state.days);
  const box = $$('#subCredits');
  if (!rows.length) { box.innerHTML = '<div class="fee-card"><div class="ucard-label">暂无积分流水</div></div>'; return; }
  box.innerHTML = `<div class="fee-card"><table class="fee-table"><tr><th>时间</th><th>账号</th><th>渠道</th><th>类型</th><th>变动</th><th>余额</th><th>备注</th></tr>
    ${rows.map((c) => {
    const [label, cls] = CREDIT_KIND[c.kind] || [c.kind || '—', ''];
    const sign = c.amount > 0 ? '+' : '';
    return `<tr><td>${(c.ts || '').replace('T', ' ').slice(0, 16)}</td><td>${c.uid}</td><td>${chName(c.channel)}</td>` +
      `<td><span class="chip ${cls}">${label}</span></td><td>${sign}${Number(c.amount).toFixed(1)}</td>` +
      `<td>${c.after == null ? '—' : Number(c.after).toFixed(1)}</td><td>${c.note || ''}</td></tr>`;
  }).join('')}</table></div>`;
}
$$('#usageRange').addEventListener('click', (e) => {
  if (!e.target.classList.contains('seg-btn')) return;
  $$all('#usageRange .seg-btn').forEach((b) => b.classList.toggle('active', b === e.target));
  state.days = +e.target.dataset.days;
  loadUsage();
});
$$('#btnRefreshUsage').addEventListener('click', loadUsage);

// ---------- Tab: 统计 ----------
async function loadStats() {
  const s = await api('/core/stats');
  const cards = [
    { l: '账号总数', v: fmt.int(s.accounts_total) },
    { l: '今日到期', v: fmt.int(s.accounts_expiring_today), c: 'warn' },
    { l: '登录态异常', v: fmt.int(s.accounts_stale), c: 'danger' },
    { l: '冷却中', v: fmt.int(s.accounts_cooling), c: 'warn' },
    { l: '今日消耗', v: fmt.yuan(s.today_spend), c: 'warn' },
    { l: '今日入账', v: fmt.yuan(s.today_earn), c: 'ok' },
    { l: '今日轮换', v: fmt.int(s.rotations_today) },
  ];
  $$('#statsCards').innerHTML = cards.map((c) => `<div class="ucard"><div class="ucard-label">${c.l}</div><div class="ucard-value ${c.c || ''}">${c.v}</div></div>`).join('');
  const max = Math.max(...s.daily.map((d) => d.tokens));
  $$('#statsChart').innerHTML = `<div class="bar-chart">${s.daily.map((d) => `
      <div class="bar-col"><div class="bar" style="height:${(d.tokens / max * 100).toFixed(1)}%"></div><div class="bar-label">${d.day}</div></div>`).join('')}</div>`;
  // 渠道维度（今日）与异常账号清单（后端 /core/stats 的 channels / abnormal）
  const chs = s.channels || [];
  $$('#statsChannels').innerHTML = chs.length ? `<div class="fee-card"><h4>今日渠道分布</h4>
    <table class="fee-table"><tr><th>渠道</th><th>请求</th><th>Token</th><th>错误</th><th>轮换</th></tr>
    ${chs.map((c) => `<tr><td>${c.name}</td><td>${fmt.int(c.requests)}</td><td>${fmt.int(c.tokens)}</td><td>${fmt.int(c.errors)}</td><td>${fmt.int(c.rotations)}</td></tr>`).join('')}</table></div>` : '';
  // 降级记录（旧版 stats.Fallbacks；空则隐藏整块）
  const fbs = s.fallbacks || [];
  $$('#statsFallbacks').innerHTML = fbs.length ? `<div class="fee-card"><h4>降级记录</h4>
    <table class="fee-table"><tr><th>时刻</th><th>原模型</th><th>实际模型</th><th>渠道</th></tr>
    ${fbs.map((f) => `<tr><td>${f.ts}</td><td>${esc(f.from)}</td><td>${esc(f.to)}</td><td>${esc(f.channel)}</td></tr>`).join('')}</table></div>` : '';
  const ab = s.abnormal || [];
  $$('#statsAbnormal').innerHTML = ab.length ? `<div class="warnings"><div class="warn-title">⚠ 异常账号</div><ul>
    ${ab.map((a) => `<li>${a.nickname}（${a.channel}）：${a.reason}</li>`).join('')}</ul></div>` : '';
  // 今日模型消耗榜（旧版 stats.Models；只统计今日、按 token 降序）。
  // 数据来自 /core/stats 的 by_model（后端已按今日聚合），不额外发请求。
  const bm = (s.by_model || []).slice(0, 20);
  $$('#statsModels').innerHTML = bm.length ? `<div class="fee-card"><h4>今日模型消耗榜</h4>
    <table class="fee-table"><tr><th>模型</th><th>渠道</th><th>请求</th><th>Token</th></tr>
    ${bm.map((m) => `<tr><td>${esc(m.model)}</td><td>${esc(m.channel)}</td><td>${fmt.int(m.requests)}</td><td>${fmt.int(m.tokens)}</td></tr>`).join('')}</table></div>` : '';
  // 结构化请求日志（旧版 stats 的「请求日志」区块，最近 50 条，最新在前）。
  // 独立端点 /core/logs/requests：只记事实（模型/渠道/账号/ok/errkind/token/耗时），不记正文。
  const rq = await api('/core/logs/requests?limit=50').catch(() => []);
  $$('#statsReqs').innerHTML = (rq || []).length ? `<div class="fee-card"><h4>最近请求</h4>
    <table class="fee-table"><tr><th>时刻</th><th>模型</th><th>渠道</th><th>状态</th><th>Token</th><th>耗时</th><th>首字</th></tr>
    ${rq.map((x) => `<tr><td>${x.ts}</td><td>${esc(x.model)}</td><td>${esc(x.channel || '—')}</td><td>${x.ok ? '✓' : `<span style="color:var(--danger)">${esc(x.err_kind || '错误')}</span>`}</td><td>${fmt.int(x.total_tokens)}</td><td>${x.time_ms ? x.time_ms + 'ms' : '—'}</td><td>${x.first_token_ms ? x.first_token_ms + 'ms' : '—'}</td></tr>`).join('')}</table></div>` : '';
  // 失败汇总（W3-46）：来源是磁盘上的 fail/YYYYMMDD.jsonl，重启后仍在。
  // 包含无法归渠道的失败（channel 为空 = 未路由成功），这是内存环形记不到的那一半。
  const fl = await api('/core/logs/fails?days=1&limit=50').catch(() => []);
  $$('#statsFails').innerHTML = (fl || []).length ? `<div class="fee-card"><h4>今日失败 <span class="chip warn">${fl.length}</span></h4>
    <table class="fee-table"><tr><th>时刻</th><th>模型</th><th>渠道</th><th>错误</th><th>摘要</th><th>耗时</th></tr>
    ${fl.map((x) => `<tr><td>${esc((x.ts || '').slice(11, 19))}</td><td>${esc(x.model)}</td><td>${esc(x.channel || '未路由')}</td><td><span style="color:var(--danger)">${esc(x.err_kind || '错误')}</span></td><td title="${esc(x.message || '')}">${esc((x.message || '').slice(0, 60))}</td><td>${x.time_ms ? x.time_ms + 'ms' : '—'}</td></tr>`).join('')}</table></div>` : '';
  $$('#warnings').innerHTML = `<div class="warn-title">⚠ 提醒</div><ul>${s.warnings.map((w) => `<li>${w}</li>`).join('')}</ul>`;
  const logs = await api('/core/logs');
  $$('#logView').textContent = logs.join('\n');
}
$$('#btnRefreshStats').addEventListener('click', () => safe(loadStats()));

// ---------- Tab: 费率 ----------
// effortCell 渲染思考档位（W3-32）。
// 只在上游目录**显式声明**档位时才展示（efforts 非空），不自行编造；
// can_disable_thinking=false 额外标注「不可关」（假关：关掉照样思考照样计费）。
function effortCell(m) {
  const list = m.efforts || [];
  if (!list.length) return '<span style="color:var(--muted)">—</span>';
  const def = m.default_effort ? `（默认 ${esc(m.default_effort)}）` : '';
  const noDis = m.can_disable_thinking === false
    ? ' <span class="chip warn" title="上游关闭思考是假关：关掉照样思考照样计费">不可关</span>' : '';
  return `<span class="chip" title="思考档位（上游目录声明）：${esc(list.join('·'))}${def}">${esc(list.join('·'))}</span>${noDis}`;
}

async function loadFees() {
  const fees = await api('/core/fees');
  $$('#feeList').innerHTML = fees.map((c) => `
    <div class="fee-card" data-cc="${chClass(c.channel)}">
      <h4>${avatar(c.channel, c.name)} ${c.name} <button class="btn small" data-fr="${c.channel}" data-fact="refresh">刷新</button></h4>
      <table class="fee-table"><tr><th>模型</th><th>输入(¥/k)</th><th>输出(¥/k)</th><th>上下文</th><th>思考档位</th></tr>
      ${c.models.map((m) => `<tr><td>${m.id}</td><td>${fmt.yuan(m.input)}</td><td>${fmt.yuan(m.output)}</td><td>${fmt.int(m.context)}</td><td>${effortCell(m)}</td></tr>`).join('')}
    </table></div>`).join('');
}
$$('#feeList').addEventListener('click', async (e) => {
  const b = e.target.closest('[data-fact]');
  if (!b) return;
  const ch = b.dataset.fr;
  try { const r = await api('/core/channels/' + ch + '/models/refresh', { method: 'POST' }); toast(r.message || '模型列表已刷新', 'ok'); loadFees(); }
  catch (err) { toast('失败：' + err.message, 'err'); }
});
$$('#btnRefreshFees').addEventListener('click', async () => {
  const chans = await api('/core/channels');
  let n = 0;
  for (const c of chans) {
    try { await api('/core/channels/' + c.channel_id + '/models/refresh', { method: 'POST' }); n++; } catch (_) { /* 单个渠道失败不影响其余 */ }
  }
  toast(`已刷新 ${n} 个渠道的模型列表`, 'ok');
  loadFees();
});

// ---------- 模型查找弹层（旧版 #modelSearchOverlay）----------
// 纯前端本地过滤 `/core/fees`（渠道 → models 已含 id/价格/上下文/档位），不发新端点。
let msFees = null;
async function openModelSearch() {
  $$('#modelSearchOverlay').classList.remove('hidden');
  $$('#msInput').value = '';
  $$('#msResult').innerHTML = '<div class="ov-note">加载中…</div>';
  try {
    msFees = msFees || await api('/core/fees');
    renderModelSearch('');
    $$('#msInput').focus();
  } catch (err) {
    $$('#msResult').innerHTML = `<div class="ov-tip danger">加载失败：${esc(err.message)}</div>`;
  }
}
function renderModelSearch(q) {
  const kw = (q || '').trim().toLowerCase();
  const rows = [];
  (msFees || []).forEach((c) => (c.models || []).forEach((m) => {
    if (!kw || (m.id || '').toLowerCase().includes(kw) || (c.name || '').toLowerCase().includes(kw)
      || (c.channel || '').toLowerCase().includes(kw)) {
      rows.push({ ch: c.channel, name: c.name, m });
    }
  }));
  if (!rows.length) { $$('#msResult').innerHTML = '<div class="ov-note">没有匹配的模型</div>'; return; }
  $$('#msResult').innerHTML = `<table class="fee-table"><tr><th>模型</th><th>渠道</th><th>输入(¥/k)</th><th>输出(¥/k)</th><th>上下文</th></tr>${rows.slice(0, 300).map((r) => `<tr data-model="${esc(r.m.id)}"><td>${esc(r.m.id)}</td><td><span class="chip" style="color:${chColor(r.ch)}">${esc(r.ch)}</span></td><td>${fmt.yuan(r.m.input)}</td><td>${fmt.yuan(r.m.output)}</td><td>${fmt.int(r.m.context)}</td></tr>`).join('')}</table>`
    + (rows.length > 300 ? `<div class="ov-note">共 ${rows.length} 条，仅显示前 300 条（请输入更具体的关键字）</div>` : '');
}
$$('#btnModelSearch').addEventListener('click', () => safe(openModelSearch()));
$$('#modelSearchClose').addEventListener('click', () => $$('#modelSearchOverlay').classList.add('hidden'));
$$('#modelSearchOverlay').addEventListener('click', (e) => { if (e.target.id === 'modelSearchOverlay') $$('#modelSearchOverlay').classList.add('hidden'); });
$$('#msInput').addEventListener('input', (e) => renderModelSearch(e.target.value));
// 点行 → 复制模型 ID 到剪贴板（旧版是填入请求示例，这里给最直接的动作）
$$('#msResult').addEventListener('click', async (e) => {
  const tr = e.target.closest('tr[data-model]');
  if (!tr) return;
  try { await navigator.clipboard.writeText(tr.dataset.model); toast('已复制 ' + tr.dataset.model, 'ok'); }
  catch (_) { toast(tr.dataset.model); }
});

// ---------- 插件与更新 ----------
function switchSub(sub) {
  $$all('.subtab').forEach((b) => b.classList.toggle('active', b.dataset.sub === sub));
  $$('#subStore').classList.toggle('hidden', sub !== 'store');
  $$('#subInstalled').classList.toggle('hidden', sub !== 'installed');
  $$('#subUpdates').classList.toggle('hidden', sub !== 'updates');
  if (sub === 'store') safe(loadStore());
  if (sub === 'installed') safe(loadInstalled());
  if (sub === 'updates') safe(loadUpdates());
}
$$all('.subtab').forEach((b) => b.addEventListener('click', () => switchSub(b.dataset.sub)));

async function loadStore() {
  // v2（W3-48）：注册表是平铺多文件，一次 tarball 拉全，每个插件自带全部 versions[]，
  // 所以没有「指针详情/按需展开」这回事——首屏就是完整数据。
  const plugins = await api('/gw/plugins/store');
  const kindLabel = { channel: '渠道', hook: 'Hook', main: '主程序' };
  const cat = plugins.length ? plugins[0] : null;
  let errs = [];
  try { errs = await api('/gw/plugins/store/errors'); } catch (_) { errs = []; }
  $$('#subStore').innerHTML = `
    <div class="pa-group"><input class="ipt" id="storeSearch" placeholder="搜索插件…" style="width:220px">
      <button class="btn small" data-op="refresh-catalog">重新拉清单</button>
      ${cat ? `<span class="bar-label">来源 ${cat.catalog_source || '—'} · ${fmtCatTime(cat.catalog_updated_at)}</span>` : ''}
      ${cat && cat.catalog_error ? `<span class="tooltip-badge warn" title="${cat.catalog_error}">清单拉取失败，显示上次快照</span>` : ''}
      ${errs.length ? `<span class="tooltip-badge danger" id="storeErrs" style="cursor:pointer">${errs.length} 个注册条目有问题</span>` : ''}</div>
    ${errs.length ? `<div class="exit" id="storeErrBox" style="display:none">${errs.map((e) => `<div class="bar-label">${e.file} · ${e.reason}</div>`).join('')}</div>` : ''}
    <div class="exit" id="storeGrid">${plugins.map((p) => `
      <div class="plugin-card">
        <div class="pl-title">${avatar(p.plugin_id, p.name)} ${p.name}
          <span class="pl-kind ${p.kind}">${kindLabel[p.kind] || p.kind}</span>
          ${p.status && p.status !== 'verified' ? `<span class="tooltip-badge warn">${p.status === 'official' ? '官方' : '未验证'}</span>` : ''}
          ${p.installable === false ? '<span class="tooltip-badge danger">无可安装版本</span>' : ''}
          ${p.conformance && p.conformance.mode ? `<span class="tooltip-badge ${p.conformance.fail ? 'danger' : 'ok'}">准入 ${p.conformance.pass}/${p.conformance.pass + p.conformance.warn + p.conformance.fail}</span>` : ''}</div>
        <div class="pl-desc">${p.description}</div>
        <div class="pl-meta">${p.plugin_id} · ${p.version || '未安装'}${p.latest ? ' → ' + p.latest : ''}<br>${p.repo || p.source} · 作者 ${p.author}</div>
        <div class="pl-actions">
          ${p.installed
            ? (p.latest && p.version !== p.latest ? `<button class="btn small primary" data-op="upgrade" data-pid="${p.plugin_id}">升级到 ${p.latest}</button>` : '<button class="btn small" disabled>已安装</button>')
            : (p.installable === false ? '<button class="btn small" disabled>暂不可安装</button>'
              : `<button class="btn small primary" data-op="install" data-pid="${p.plugin_id}">安装${p.latest ? ' ' + p.latest : ''}</button>`)}
          ${(p.versions || []).length > 1 ? `<select class="ipt small" data-ver="${p.plugin_id}">${p.versions.map((v) => `<option value="${v.version}" ${v.version === p.latest ? 'selected' : ''}>${v.version}${v.os ? ' · ' + v.os + '/' + v.arch : ''}</option>`).join('')}</select>` : ''}
        </div>
      </div>`).join('')}</div>`;
  const eb = $$('#storeErrs');
  if (eb) eb.addEventListener('click', () => { const b = $$('#storeErrBox'); b.style.display = b.style.display === 'none' ? '' : 'none'; });
  $$('#storeSearch').addEventListener('input', (e) => {
    const q = e.target.value.toLowerCase();
    $$all('#storeGrid .plugin-card').forEach((c) => {
      c.style.display = c.textContent.toLowerCase().includes(q) ? '' : 'none';
    });
  });
}
// 清单拉取时间的人话化（只显示日期，够判断「多旧」）
function fmtCatTime(iso) {
  if (!iso) return '清单时间未声明';
  const d = new Date(iso);
  return isNaN(d) ? iso : '清单 ' + d.toLocaleDateString();
}
// 插件商店与已安装列表共用同一套动作处理（install/upgrade/rollback/uninstall）。
// 注：`#subStore` 与 `#subInstalled` 是两个独立容器，旧代码只给前者挂了监听，
// 导致「已安装」里的升级/回滚/卸载按钮是死的。
async function pluginAction(e) {
  const b = e.target.closest('[data-op]');
  if (!b) return;
  if (b.dataset.op === 'refresh-catalog') {
    try { const r = await api('/gw/registry/refresh', { method: 'POST' }); toast(`${r.message}（${r.plugins} 个插件${r.errors ? ' · ' + r.errors + ' 个条目有问题' : ''}）`, 'ok'); loadStore(); } catch (err) { toast('失败：' + err.message, 'err'); }
    return;
  }
  const { op, pid } = b.dataset;
  if (op === 'uninstall' && !(await confirmBox('卸载插件', `确定卸载 ${pid}？<br><span class="ov-note">已登录的凭据与历史统计会保留，重装后可直接用。</span>`))) return;
  // 版本下拉（若该卡片有）决定装哪个版本；没选就走清单最新
  const sel = $$all(`[data-ver="${pid}"]`)[0];
  const version = sel ? sel.value : '';
  try {
    const r = await api('/gw/plugins/' + op, { method: 'POST', body: JSON.stringify({ plugin_id: pid, version }) });
    toast(r.message || '操作成功', 'ok');
    await loadStore();
    // 安装/卸载后 core 会重扫插件；短暂等待重启就绪，再刷新账号入口。
    setTimeout(() => safe(loadAccounts()), 1200);
  } catch (err) { toast('失败：' + err.message, 'err'); }
}
$$('#subStore').addEventListener('click', pluginAction);
$$('#subInstalled').addEventListener('click', pluginAction);
async function loadInstalled() {
  const plugins = await api('/gw/plugins');
  $$('#subInstalled').innerHTML = plugins.length === 0 ? '<div class="empty">尚未安装插件</div>' : `
    <div class="exit">${plugins.map((p) => `
      <div class="plugin-card">
        <div class="pl-title">${p.name} <span class="pl-kind ${p.kind}">${p.kind === 'channel' ? '渠道' : 'Hook'}</span></div>
        <div class="pl-meta">${p.plugin_id} · v${p.version}</div>
        <div class="pl-actions">
          ${p.version !== p.latest ? `<button class="btn small primary" data-op="upgrade" data-pid="${p.plugin_id}">升级</button>` : ''}
          <button class="btn small" data-op="rollback" data-pid="${p.plugin_id}">回滚</button>
          <button class="btn small danger" data-op="uninstall" data-pid="${p.plugin_id}">卸载</button>
        </div>
      </div>`).join('')}</div>`;
}
async function loadUpdates() {
  const u = await api('/gw/updates');
  const gw = u.gateway || {}, core = u.core || {}, job = u.job || {}, cat = u.catalog || {};
  const st = job.state || 'idle';
  const pct = Math.max(0, Math.min(100, job.progress || 0));
  $$('#subUpdates').innerHTML = `
    <div class="exit">
      <div class="fee-card"><h4>ww3-gateway（wild-work3.exe）</h4>
        <div class="pl-meta">当前 v${gw.current} · <span class="tooltip-badge warn">不参与自动升级</span></div>
        <div class="ov-note" style="margin-top:6px">${gw.note || '请整包替换 gateway 可执行文件。'}</div>
        ${gw.release ? `<div class="pl-actions"><a class="btn small" href="${gw.release}" target="_blank" rel="noreferrer">打开 release 页</a></div>` : ''}</div>
      <div class="fee-card"><h4>ww3-core（ww3-core.exe）</h4>
        <div class="pl-meta">当前 v${core.current || '未知'} · 最新 v${core.latest || '未知'}
          ${core.update ? '<span class="tooltip-badge ok">可升级</span>' : ''}
          ${core.version_gate && (core.version_gate.min || core.version_gate.max) ? `<span class="tooltip-badge">需 gateway ${core.version_gate.min || '—'} ~ ${core.version_gate.max || '—'}</span>` : ''}</div>
        ${core.error ? `<div class="ov-note" style="margin-top:6px;color:var(--danger)">清单：${core.error}</div>` : ''}
        ${core.changelog ? `<div class="ov-note" style="margin-top:6px">${core.changelog}</div>` : ''}
        ${st === 'downloading' ? `<div class="bar-chart" style="height:22px;align-items:center;margin:8px 0 0">
            <div class="bar-col" style="height:12px;align-items:stretch"><div class="bar" style="width:${pct}%;height:12px;border-radius:6px"></div></div>
          </div><div class="bar-label">${pct}% · ${fmtBytes(job.downloaded_bytes)}/${fmtBytes(job.total_bytes)} · ${fmtRate(job.rate_bps)} · 源 ${job.source || '—'}</div>` : ''}
        ${st !== 'idle' ? `<div class="bar-label" style="margin-top:4px">${job.message || ''}</div>` : ''}
        <div class="pl-actions">
          ${core.update && st !== 'downloading' ? `<button class="btn small primary" data-tgt="core" data-step="download">下载 ${core.latest}</button>` : ''}
          ${st === 'ready' ? `<button class="btn small primary" data-tgt="core" data-step="apply">应用</button>` : ''}
          ${core.rollback_available || st === 'applied' ? `<button class="btn small" data-tgt="core" data-step="rollback">回滚</button>` : ''}
          <button class="btn small" data-tgt="refresh">重新检查</button>
        </div></div>
      ${cat.source ? `<div class="bar-label">插件清单源 ${cat.source}${cat.fetched_at ? ' · ' + new Date(cat.fetched_at).toLocaleString() : ''}${cat.error ? ' · ' + cat.error : ''}</div>` : ''}
    </div>`;
  // 下载中自动续轮（不自动轮的话面板得手工刷）
  if (st === 'downloading') { clearTimeout(window.__updT); window.__updT = setTimeout(() => safe(loadUpdates), 1200); }
}
function fmtBytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB']; let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i ? v.toFixed(1) : Math.round(v)) + ' ' + u[i];
}
function fmtRate(x) { return x ? fmtBytes(x) + '/s' : '—'; }
$$('#subUpdates').addEventListener('click', async (e) => {
  const b = e.target.closest('[data-tgt]');
  if (!b) return;
  try {
    if (b.dataset.tgt === 'refresh') { await api('/gw/updates?refresh=1'); safe(loadUpdates); toast('已重新拉清单', 'ok'); return; }
    const step = b.dataset.step || 'download';
    const r = await api('/gw/updates/' + step, { method: 'POST' });
    toast(r.message || '完成', 'ok');
    loadUpdates();
  } catch (err) { toast('失败：' + err.message, 'err'); }
});

// ---------- 设置（按职责分两段：gateway 接口/安全 + core 业务策略） ----------
// 纯文本 KV 编辑器：`a = b`，`#` 起注释；用于 model_map / reasoning_efforts。
function parseKV(text) {
  const out = {};
  for (const raw of (text || '').split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const i = line.indexOf('=');
    if (i <= 0) continue;
    const k = line.slice(0, i).trim(), v = line.slice(i + 1).trim();
    if (k && v) out[k] = v;
  }
  return out;
}
function fmtKV(obj) {
  return Object.entries(obj || {}).map(([k, v]) => `${k} = ${v}`).join('\n');
}
// 降级链：`from = to1,to2`（逗号/顿号分隔多备胎，按顺序尝试）
function parseFallback(text) {
  const out = {};
  for (const raw of (text || '').split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const i = line.indexOf('=');
    if (i <= 0) continue;
    const k = line.slice(0, i).trim();
    const v = line.slice(i + 1).split(/[,，]/).map((s) => s.trim()).filter(Boolean);
    if (k && v.length) out[k] = v;
  }
  return out;
}
function fmtFallback(obj) {
  return Object.entries(obj || {}).map(([k, v]) => `${k} = ${(v || []).join(', ')}`).join('\n');
}
function splitListen(s) {
  const v = (s || '').trim(), i = v.lastIndexOf(':');
  if (i <= 0) return { host: '127.0.0.1', port: 7863 };
  return { host: v.slice(0, i), port: Number(v.slice(i + 1)) || 7863 };
}

let checkinTimes = [];
function renderCheckins() {
  $$('#checkinList').innerHTML = checkinTimes.length
    ? checkinTimes.map((t, i) => `<span class="time-chip">${t}<span class="x" data-i="${i}">✕</span></span>`).join('')
    : '<span class="hint">（无）</span>';
}
function updateListenRisk(adminSet) {
  const host = $$('#setHost').value === '__custom__' ? $$('#setHostCustom').value.trim() : $$('#setHost').value;
  const loopback = ['127.0.0.1', 'localhost', '::1'].includes(host);
  $$('#listenRiskTip').hidden = loopback || adminSet || !!$$('#setAdminPass').value.trim();
}

$$('#btnSettings').addEventListener('click', async () => {
  const g = await api('/gw/state');
  const s = await api('/core/state');
  const c = await api('/core/config');
  // ① 接口与鉴权（gateway）
  const li = splitListen(g.listen_config || g.listen || '127.0.0.1:7863');
  if (['127.0.0.1', '0.0.0.0'].includes(li.host)) { $$('#setHost').value = li.host; $$('#setHostCustom').value = ''; }
  else { $$('#setHost').value = '__custom__'; $$('#setHostCustom').value = li.host; }
  $$('#setHostCustomRow').hidden = $$('#setHost').value !== '__custom__';
  $$('#setPort').value = li.port;
  $$('#setApiKey').value = g.api_key || '';
  $$('#setAdminPass').value = '';
  $$('#setAdminPass').placeholder = g.admin_password_set ? '已设置（留空则不修改）' : '未设置（面板免登录）';
  $$('#setAutostart').checked = !!g.autostart;
  updateListenRisk(!!g.admin_password_set);
  // ② 模型路由
  const def = $$('#setDefaultChannel');
  def.innerHTML = (s.channels || []).map((ch) => `<option value="${ch.channel_id}" ${ch.channel_id === s.default_channel ? 'selected' : ''}>${ch.display_name}</option>`).join('') || '<option value="">（无渠道）</option>';
  $$('#setMaxTok').value = (c.compat && c.compat.max_tokens_cap) || 0;
  $$('#setModelMap').value = fmtKV((c.compat && c.compat.model_map) || {});
  $$('#mapSummary').textContent = Object.keys((c.compat && c.compat.model_map) || {}).length ? `已配置 ${Object.keys(c.compat.model_map).length} 条映射` : '未配置映射（客户端可直接用 渠道ID/模型名）';
  // ③ 签到与开关
  checkinTimes = (c.checkin_times || []).slice(); renderCheckins();
  $$('#setLoopGuard').checked = !!c.loop_guard;
  $$('#setBodyLog').checked = !!c.body_log;
  // ④ 备用通道 / ⑤ 降级链 / ⑥ 思考强度 / ⑦ 规则覆盖
  $$('#setProxy').value = c.proxy || '';
  $$('#setFallback').value = fmtFallback(c.model_fallback || {});
  $$('#setEfforts').value = fmtKV(c.reasoning_efforts || {});
  loadRulesEditor();
  $$('#settingsOverlay').classList.remove('hidden');
});

// ---------- 规则段编辑（W3-35） ----------
// 只展示「用户覆盖段」（config.json 里 rules 段本身），不展开默认集：
// 默认规则会随版本演进，把生效结果回写会让旧副本永久钉住当前默认。
let rulesUserText = '';
// canon 稳定序列化：递归按键名排序。
// 必须排序——defaults 来自内嵌 JSON 文件（键序即书写顺序），effective 是 Go 对 map 序列化的结果
// （字母序），直接 JSON.stringify 对比会把**每一条**都当成「被用户改过」。
function canon(v) {
  if (Array.isArray(v)) return v.map(canon);
  if (v && typeof v === 'object') {
    const o = {};
    for (const k of Object.keys(v).sort()) o[k] = canon(v[k]);
    return o;
  }
  return v;
}
// canonRule 补全零值再比对：Go 侧序列化带 omitempty，空串字段（典型是 replacement:""）
// 在 effective 里直接缺席，而 defaults 的 JSON 里写的是显式空串——两者语义相同，不能算「被改过」。
function canonRule(r) {
  return JSON.stringify(canon({
    kind: r.kind || '', jsonpath: r.jsonpath || '', regex: r.regex || '',
    replacement: r.replacement || '', anchor: r.anchor || '', order: r.order || 0,
    scope: { channels: (r.scope || {}).channels || [], roles: (r.scope || {}).roles || [] },
  }));
}
async function loadRulesEditor() {
  try {
    const r = await api('/core/config/rules');
    const eff = (r.effective && r.effective.rules) || {};
    const defs = (r.defaults && r.defaults.rules) || {};
    // effective 与 defaults 逐条对比，差异项视为用户覆盖（仅用于回显提示，不当作权威）
    const over = {};
    for (const [id, rule] of Object.entries(eff)) {
      if (!defs[id] || canonRule(defs[id]) !== canonRule(rule)) over[id] = rule;
    }
    for (const id of Object.keys(defs)) if (!eff[id]) over[id] = {}; // 被删的默认规则
    rulesUserText = Object.keys(over).length ? JSON.stringify({ rules: over }, null, 2) : '';
    $$('#setRules').value = rulesUserText;
    $$('#rulesSummary').textContent = `生效 ${Object.keys(eff).length} 条 / 默认 ${Object.keys(defs).length} 条 / 覆盖 ${Object.keys(over).length} 项`;
  } catch (err) {
    $$('#rulesSummary').textContent = '规则段读取失败：' + err.message;
  }
}
$$('#btnRulesReload').addEventListener('click', loadRulesEditor);
$$('#setHost').addEventListener('change', () => {
  $$('#setHostCustomRow').hidden = $$('#setHost').value !== '__custom__';
  updateListenRisk(false);
});
$$('#setHostCustom').addEventListener('input', () => updateListenRisk(false));
$$('#setAdminPass').addEventListener('input', () => updateListenRisk(false));
$$('#btnAddCheckin').addEventListener('click', () => {
  const t = $$('#checkinNew').value;
  if (t && !checkinTimes.includes(t)) { checkinTimes.push(t); checkinTimes.sort(); renderCheckins(); }
});
$$('#checkinList').addEventListener('click', (e) => {
  const i = e.target && e.target.dataset && e.target.dataset.i;
  if (i === undefined) return;
  checkinTimes.splice(Number(i), 1); renderCheckins();
});
$$('#setCancel').addEventListener('click', () => $$('#settingsOverlay').classList.add('hidden'));
$$('#btnClearAdminPass').addEventListener('click', async () => {
  if (!(await confirmBox('清除管理口令', '清除后面板将不再要求登录（仅限监听 127.0.0.1 时安全）。继续？'))) return;
  try { await api('/gw/config', { method: 'PUT', body: JSON.stringify({ admin_password: '' }) }); toast('管理口令已清除', 'ok'); $$('#adminLoginOverlay').classList.add('hidden'); loadTopbar(); }
  catch (err) { toast('清除失败：' + err.message, 'err'); }
});
$$('#btnAdminLogout').addEventListener('click', async () => {
  try { await api('/gw/logout', { method: 'POST' }); location.reload(); } catch (err) { toast(err.message, 'err'); }
});
$$('#setSave').addEventListener('click', async () => {
  let host = $$('#setHost').value;
  if (host === '__custom__') host = $$('#setHostCustom').value.trim();
  if (!host) { toast('请填写监听主机', 'err'); return; }
  const gwBody = { listen: `${host}:${$$('#setPort').value || 7863}`, api_key: $$('#setApiKey').value, autostart: $$('#setAutostart').checked };
  const ap = $$('#setAdminPass').value;
  if (ap) gwBody.admin_password = ap; // 空=不修改
  const coreBody = {
    default_channel: $$('#setDefaultChannel').value,
    compat: { max_tokens_cap: Number($$('#setMaxTok').value || 0), model_map: parseKV($$('#setModelMap').value) },
    checkin_times: checkinTimes,
    loop_guard: $$('#setLoopGuard').checked,
    body_log: $$('#setBodyLog').checked,
    proxy: $$('#setProxy').value.trim(),
    model_fallback: parseFallback($$('#setFallback').value),
    reasoning_efforts: parseKV($$('#setEfforts').value),
  };
  try {
    await api('/gw/config', { method: 'PUT', body: JSON.stringify(gwBody) });
    await api('/core/config', { method: 'PUT', body: JSON.stringify(coreBody) });
    // ⑦ 规则段：与上面分开提交（它是 config.json 的独立段，失败不应让业务配置回滚）
    const rulesText = $$('#setRules').value.trim();
    if (rulesText !== rulesUserText.trim()) {
      let parsed;
      try { parsed = rulesText ? JSON.parse(rulesText) : null; }
      catch (e) { toast('规则段不是合法 JSON：' + e.message, 'err'); return; }
      await api('/core/config/rules', { method: 'PUT', body: JSON.stringify(parsed) });
      // Pass A 在 gateway 进程内，需要单独重载
      await api('/gw/rules/reload', { method: 'POST' });
    }
    $$('#settingsOverlay').classList.add('hidden');
    toast('设置已保存（监听地址变更需重启生效）', 'ok');
    loadTopbar();
  } catch (err) { toast('保存失败：' + err.message, 'err'); }
});

// ---------- 使用说明与风险声明 ----------
const HELP_HTML = `
  <div class="help-content">
    <h4>基本用法</h4>
    <ol>
      <li>在「插件与更新」安装渠道插件；账号管理页会自动出现该插件声明的渠道按钮。需要 API Key 的渠道点击按钮填写配置，其他渠道点击按钮开始登录。</li>
      <li>添加账号在账号页点击对应渠道按钮，按插件 <code>auth.methods</code> 返回的范式登录（浏览器、设备码、回调粘贴或凭据导入）；自定义模型填写配置即可。</li>
      <li>把客户端的 <b>Base URL</b> 设为顶部显示的 OpenAI 接口地址，<b>API Key</b> 用顶部显示的值。</li>
      <li>可在「设置」中修改监听地址、API-Key、管理口令与开机自启；管理口令启用后本面板需要登录。</li>
    </ol>
    <h4 class="risk">风险声明</h4>
    <ul>
      <li>本工具默认仅在本机 <code>127.0.0.1</code> 提供聚合服务；账户凭据保存在各插件私有目录，管理口令与 API Key 属敏感信息，请勿泄露。</li>
      <li>各渠道账号由用户自行获取，使用须遵守对应服务条款；因滥用导致的封号/限流风险由使用者自负。</li>
      <li>请勿对上游服务做批量/穷举探测，避免触发风控。</li>
    </ul>
  </div>`;
$$('#btnHelp').addEventListener('click', () => { $$('#helpBody').innerHTML = HELP_HTML; $$('#helpOverlay').classList.remove('hidden'); });
$$('#helpClose').addEventListener('click', () => $$('#helpOverlay').classList.add('hidden'));

// ---------- 管理面板登录（gateway 管理口令） ----------
function showAdminLogin() {
  const ov = $$('#adminLoginOverlay');
  if (ov.classList.contains('hidden')) {
    $$('#adminErr').classList.add('hidden');
    $$('#adminPass').value = '';
    ov.classList.remove('hidden');
    setTimeout(() => $$('#adminPass').focus(), 60);
  }
}
async function adminLogin() {
  try {
    await api('/gw/login', { method: 'POST', body: JSON.stringify({ password: $$('#adminPass').value }) });
    $$('#adminLoginOverlay').classList.add('hidden');
    await startApp();
  } catch (err) { $$('#adminErr').textContent = err.message; $$('#adminErr').classList.remove('hidden'); }
}
$$('#adminLoginBtn').addEventListener('click', adminLogin);
$$('#adminPass').addEventListener('keydown', (e) => { if (e.key === 'Enter') adminLogin(); });
$$('#btnAbout').addEventListener('click', () => {
  api('/gw/version').then((v) => {
    $$('#aboutBody').innerHTML = `<div class="ov-info">wild-work3 本地网关<br>gateway v${v.gateway} · core v${v.core} · IPC 协议 v${v.protocol}</div>`;
    $$('#aboutOverlay').classList.remove('hidden');
  });
});
$$('#aboutClose').addEventListener('click', () => $$('#aboutOverlay').classList.add('hidden'));

// ---------- 登录授权（authflow 六范式驱动，见 protocol/docs/AUTH_FLOW.md） ----------
// core 不认渠道，只按 method.type 决定 UI：device_poll/loopback_callback/cdp_capture 需要轮询，
// 只有 loopback_callback 才有「粘贴回调地址」兜底（跨机场景）。
const loginOv = $$('#loginOverlay');
let lc = { channel: null, flow: null, timer: null, method: null, terminal: false };

function loginReset() {
  if (lc.timer) { clearInterval(lc.timer); lc.timer = null; }
  lc = { channel: lc.channel, flow: null, timer: null, method: null, terminal: false };
  $$('#loBody').innerHTML = '';
  $$('#loSubmit').hidden = true;
  $$('#loStart').hidden = false;
  $$('#loStart').textContent = '开始登录';
  $$('#loStart').disabled = false;
}

async function openLoginWizard(channelId) {
  lc.channel = channelId;
  loginReset();
  $$('#loTitle').textContent = '登录账号 — ' + channelId;
  loginOv.classList.remove('hidden');
  let res;
  try { res = await api('/core/auth/methods?channel=' + encodeURIComponent(channelId)); }
  catch (err) { $$('#loBody').innerHTML = `<div class="ov-tip">${err.message}</div>`; return; }
  const methods = res.methods || [];
  if (methods.length === 0) {
    $$('#loBody').innerHTML = '<div class="ov-tip">该渠道未声明登录方式，请直接在渠道配置里填写密钥。</div>';
    $$('#loStart').hidden = true;
    return;
  }
  lc.methods = methods;
  lc.method = methods.find((m) => m.default) || methods[0];
  renderLoginMethods(res);
}

// renderLoginMethods 渲染方式选择 + 该方式的表单（Fields 是 JSON Schema 子集，复用 schemaForm）
function renderLoginMethods(res) {
  const ms = lc.methods;
  const sel = lc.method;
  const formHtml = sel && sel.fields ? schemaForm({ properties: sel.fields }, {}, 'lf_') : '';
  $$('#loBody').innerHTML = `
    ${res.note ? `<div class="ov-note">${res.note}</div>` : ''}
    ${ms.length > 1 ? `<div class="exit" id="lmGrid">${ms.map((m) => `
      <div class="plugin-card" data-mid="${m.id}" style="cursor:pointer;${m.id === sel.id ? 'border-color:var(--primary)' : ''}">
        <div class="pl-title">${m.label || m.id}</div>
        <div class="pl-desc">${m.description || ''}</div>
        <div class="pl-caps">${m.type}${m.region ? ' · ' + m.region : ''}</div>
      </div>`).join('')}</div>` : `<div class="ov-note">方式：<b>${sel.label || sel.id}</b>（${sel.type}）${sel.description ? ' — ' + sel.description : ''}</div>`}
    <div id="lf">${formHtml}</div>
    <div id="loStatus" class="ov-tip"></div>
    <div id="loActions"></div>`;
  const grid = $$('#lmGrid');
  if (grid) grid.querySelectorAll('[data-mid]').forEach((c) => c.addEventListener('click', () => {
    lc.method = ms.find((m) => m.id === c.dataset.mid);
    renderLoginMethods(res);
  }));
}

function loginStatus(html) { const el = $$('#loStatus'); if (el) el.innerHTML = html; }

// doBegin 发起登录；按范式渲染后续交互
async function loginBegin() {
  const m = lc.method;
  const values = m && m.fields ? collectForm('lf') : {};
  $$('#loStart').disabled = true;
  try {
    const br = await api('/core/auth/begin', {
      method: 'POST',
      body: JSON.stringify({ channel_id: lc.channel, method: m.id, region: m.region || '', values }),
    });
    lc.flow = br.flow_id;
    lc.terminal = false;
    renderLoginFlow(br);
    schedulePoll(br.next_poll_ms || 2000);
  } catch (err) { loginStatus('发起失败：' + err.message); }
  finally { $$('#loStart').disabled = false; }
}

function renderLoginFlow(br) {
  lc.type = br.type;
  const parts = [];
  if (br.auth_url) {
    parts.push(`<div class="frow">登录地址：<a href="${br.auth_url}" target="_blank" rel="noreferrer">${br.auth_url}</a>
      <button class="btn small" id="loOpen">打开</button></div>`);
  }
  if (br.user_code) parts.push(`<div class="frow">用户码：<b>${br.user_code}</b>${br.verification_uri ? ` （在 ${br.verification_uri} 输入）` : ''}</div>`);
  if (br.debug_port) parts.push(`<div class="frow">浏览器调试端口：${br.debug_port}（仅本机 127.0.0.1）</div>`);
  if (br.prompt) parts.push(`<div class="ov-note">${br.prompt}</div>`);
  // 只有 loopback_callback 才有回调地址与粘贴兜底
  if (br.callback_url) parts.push(`<div class="frow">本地回调：${br.callback_url}</div>`);
  if (br.can_paste_callback) {
    parts.push(`<label class="frow">跨机登录：把浏览器跳转后的完整地址粘贴到这里
      <input class="ipt" id="loCallback" placeholder="http://127.0.0.1:xxxxx/callback?code=..."></label>`);
    $$('#loSubmit').hidden = false;
    $$('#loSubmit').textContent = '提交回调';
  } else {
    $$('#loSubmit').hidden = true;
  }
  $$('#loStart').hidden = true;
  loginStatus(parts.join('') + `<div id="loStateLine"></div>`);
  const open = $$('#loOpen');
  if (open) open.addEventListener('click', () => window.open(br.auth_url, '_blank'));
}

function schedulePoll(ms) {
  if (lc.timer) clearInterval(lc.timer);
  lc.timer = setInterval(() => loginPoll().catch(() => {}), Math.max(1000, ms));
}

async function loginPoll() {
  if (!lc.flow || lc.terminal) return;
  const st = await api('/core/auth/status', { method: 'POST', body: JSON.stringify({ flow_id: lc.flow }) });
  const line = $$('#loStateLine');
  const acct = st.account ? ` · ${st.account.nickname || st.account.uid}` : '';
  if (line) line.innerHTML = `<div class="ov-tip">状态：${st.state}${st.hint ? ' — ' + st.hint : ''}${acct}</div>`;
  if (['success', 'failed', 'expired', 'cancelled'].includes(st.state)) {
    lc.terminal = true;
    if (lc.timer) { clearInterval(lc.timer); lc.timer = null; }
    $$('#loSubmit').hidden = true;
    $$('#loStart').hidden = st.state === 'success';
    if (st.state === 'success') { loadAccounts(); loadChannels(); }
    if (st.error) loginStatus(`<div class="ov-tip danger">${st.error.message || st.error.kind}</div>`);
  }
}

async function loginFinish() {
  const el = $$('#loCallback');
  if (!el || !el.value.trim()) { toast('请粘贴回调地址', 'warn'); return; }
  try {
    const st = await api('/core/auth/finish', { method: 'POST', body: JSON.stringify({ flow_id: lc.flow, callback_url: el.value.trim() }) });
    if (st.state === 'success') { lc.terminal = true; if (lc.timer) { clearInterval(lc.timer); lc.timer = null; } loadAccounts(); loadChannels(); }
    loginStatus(`<div class="ov-tip">状态：${st.state}${st.hint ? ' — ' + st.hint : ''}</div>`);
  } catch (err) { toast('提交失败：' + err.message, 'err'); }
}

$$('#loStart').addEventListener('click', loginBegin);
$$('#loSubmit').addEventListener('click', loginFinish);
$$('#loCancel').addEventListener('click', async () => {
  if (lc.flow && !lc.terminal) { try { await api('/core/auth/cancel', { method: 'POST', body: JSON.stringify({ flow_id: lc.flow }) }); } catch (_) {} }
  if (lc.timer) { clearInterval(lc.timer); lc.timer = null; }
  loginOv.classList.add('hidden');
});

// ---------- 主题（模式 × 强调色，localStorage 持久化，head 内联脚本已先应用避免闪烁） ----------
const THEME_KEY = 'ww3.theme';
const THEME = {
  mode: 'auto', // dark | light | auto
  accent: 'blue',
  accents: [
    { id: 'blue', name: '湛蓝', color: '#5B8FF9' },
    { id: 'violet', name: '暮紫', color: '#9254DE' },
    { id: 'green', name: '翠绿', color: '#2FB344' },
    { id: 'cyan', name: '青碧', color: '#13A8C2' },
    { id: 'orange', name: '琥珀', color: '#F5822B' },
    { id: 'rose', name: '樱粉', color: '#EB2F96' },
    { id: 'slate', name: '石墨', color: '#7C8798' },
  ],
};
function loadThemePref() {
  try {
    const t = JSON.parse(localStorage.getItem(THEME_KEY) || '{}');
    THEME.mode = t.mode || 'auto';
    THEME.accent = t.accent || 'blue';
  } catch (_) { /* 忽略损坏的本地配置，回退默认 */ }
}
function saveThemePref() {
  try { localStorage.setItem(THEME_KEY, JSON.stringify({ mode: THEME.mode, accent: THEME.accent })); } catch (_) {}
}
// auto 模式解析：系统偏好亮色则用亮色，否则深色
function themeIsDark() {
  return THEME.mode === 'dark' || (THEME.mode === 'auto' && !window.matchMedia('(prefers-color-scheme: light)').matches);
}
function applyTheme() {
  const el = document.documentElement;
  el.setAttribute('data-mode', themeIsDark() ? 'dark' : 'light');
  el.setAttribute('data-accent', THEME.accent);
}
function setTheme(patch) {
  Object.assign(THEME, patch);
  applyTheme(); saveThemePref(); renderThemeOverlay();
}
function renderThemeOverlay() {
  const seg = $$('#themeMode');
  if (!seg) return;
  $$all('#themeMode .seg-btn').forEach((b) => b.classList.toggle('active', b.dataset.mode === THEME.mode));
  $$('#themeAccents').innerHTML = THEME.accents.map((a) => `
    <div class="theme-swatch ${a.id === THEME.accent ? 'active' : ''}" data-accent="${a.id}" title="${a.name}">
      <span class="dot" style="background:${a.color}"></span><span>${a.name}</span>
    </div>`).join('');
}
$$('#themeMode').addEventListener('click', (e) => {
  const b = e.target.closest('.seg-btn');
  if (b) setTheme({ mode: b.dataset.mode });
});
$$('#themeAccents').addEventListener('click', (e) => {
  const s = e.target.closest('.theme-swatch');
  if (s) setTheme({ accent: s.dataset.accent });
});
$$('#btnTheme').addEventListener('click', () => { renderThemeOverlay(); $$('#themeOverlay').classList.remove('hidden'); });
$$('#themeClose').addEventListener('click', () => $$('#themeOverlay').classList.add('hidden'));
// auto 模式下跟随系统深浅色变化
window.matchMedia('(prefers-color-scheme: light)').addEventListener('change', () => { if (THEME.mode === 'auto') applyTheme(); });

// ---------- 启动 ----------
// 除主题外，先查 gateway 会话：启用了管理口令且未登录时先显示登录框。
async function startApp() {
  await loadTopbar();
  await loadAccounts();
  switchTab('accounts');
}
(async function init() {
  loadThemePref();
  applyTheme();
  try {
    const s = await api('/gw/session');
    if (s.required && !s.authenticated) { showAdminLogin(); return; }
  } catch (e) {
    // 兜底：正常经 gateway 进来时 /gw/session 一定存在。失败说明当前 origin 不是 gateway
    // （最常见＝浏览器直接开了 core 的端口，面板能渲染但 /gw/*、/core/* 全 404）。
    // core 侧已在入口返回提示页，这里只给已缓存旧页面的情况留一句可读提示，然后照旧继续。
    if (!/需要管理口令/.test(e.message)) {
      toast('后端接口不可达（' + e.message + '）：请确认访问的是 gateway 端口，而不是 core 端口', 'err');
    }
  }
  await startApp();
})();