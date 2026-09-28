// API 封装：管理后台 JSON 接口
const BASE = '/api'

function getToken() {
  return sessionStorage.getItem('admin_token')
}

async function request(path, params = {}, opts = {}) {
  const qs = new URLSearchParams()
  // 只有 GET 请求（无 opts.method）才加 query string
  if (!opts.method || opts.method === 'GET') {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== null && v !== '') qs.set(k, v)
    }
  }
  const url = `${BASE}${path}${qs.toString() ? '?' + qs : ''}`

  // 添加 token 鉴权
  const headers = { 'Accept': 'application/json' }
  const token = getToken()
  if (token) headers['Authorization'] = 'Bearer ' + token

  const fetchOpts = { headers, ...opts }
  // 合并 headers（opts 里的 headers 覆盖默认 headers）
  if (opts.headers) {
    fetchOpts.headers = { ...headers, ...opts.headers }
  }
  const res = await fetch(url, fetchOpts)
  if (res.status === 401) {
    // token 过期或未登录，清除并跳转登录页
    sessionStorage.removeItem('admin_token')
    window.location.reload()
    throw new Error('请重新登录')
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error || `HTTP ${res.status}`)
  }
  const json = await res.json()
  return json.data
}

export const api = {
  // 仪表盘（days: 趋势天数）
  dashboard: (params = {}) => request('/dashboard', params),
  // 易支付对接信息
  epayInfo: () => request('/epay-info'),
  // 支付通道
  channels: () => request('/channels'),
  // 系统设置
  settings: () => request('/settings'),
  saveSettings: (data) => request('/settings', {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' } }),
  // 安全密钥（完整值，需登录；调用记审计日志）
  settingsKeys: () => request('/settings/keys'),
  // 重置管理员：验证当前密码后清空凭据，回到首次注册态
  resetAdmin: (password) => request('/auth/reset', {}, { method: 'POST', body: JSON.stringify({ password }), headers: { 'Content-Type': 'application/json' } }),
  toggleChannel: (name) => request(`/settings/channel/${name}/toggle`, {}, { method: 'POST' }),
  toggleChannelMode: (name, mode) => request(`/settings/channel/${name}/mode/${mode}/toggle`, {}, { method: 'POST' }),
  // 订单
  orders: (params) => request('/orders', params),
  orderEvents: (id, limit = 20) => request(`/orders/${id}/events`, { limit }),
  // 通知任务
  notifyTasks: (params) => request('/notify-tasks', params),
  notifyStats: () => request('/notify-stats'),
  // 日志
  logs: (params) => request('/logs', params),
  // 账本
  ledger: (params) => request('/ledger', params),
  ledgerVerify: () => request('/ledger/verify', {}, { method: 'POST' }),
  ledgerSnapshots: (params) => request('/ledger/snapshots', params),
  createSnapshot: () => request('/ledger/snapshot', {}, { method: 'POST' }),
  ledgerPublicKey: () => request('/ledger/public-key'),
  // 导出账本 CSV（下载文件，不走 JSON 封装）
  ledgerExport: async () => {
    const token = getToken()
    const res = await fetch(BASE + '/ledger/export', { headers: { 'Authorization': 'Bearer ' + token } })
    if (!res.ok) {
      const body = await res.json().catch(() => ({}))
      throw new Error(body.error || '导出失败')
    }
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'ledger_audit_' + new Date().toISOString().slice(0, 10) + '.csv'
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(url)
  },
  // 隧道
  tunnel: () => request('/tunnel'),

  // 账户池管理
  accounts: (params) => request('/accounts', params),
  getAccount: (id) => request(`/accounts/${id}`),
  // 完整密钥（明文，需登录）：管理后台「查看/复制完整密钥」用
  accountSecret: (id) => request(`/accounts/${id}/secret`),
  createAccount: (data) => request('/accounts', {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  updateAccount: (id, data) => request(`/accounts/${id}`, {}, { method: 'PUT', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  deleteAccount: (id) => request(`/accounts/${id}`, {}, { method: 'DELETE' }),
  freezeAccount: (id) => request(`/accounts/${id}/freeze`, {}, { method: 'POST' }),
  unfreezeAccount: (id) => request(`/accounts/${id}/unfreeze`, {}, { method: 'POST' }),
  enableAccount: (id) => request(`/accounts/${id}/enable`, {}, { method: 'POST' }),
  disableAccount: (id) => request(`/accounts/${id}/disable`, {}, { method: 'POST' }),
  accountLogs: (id, limit) => request(`/accounts/${id}/logs`, { limit }),

  // 上传收款码图片 → 服务端解析二维码内容（docs/MIANQIAN.md §7.6 阶段一）
  decodeQRCode: async (file) => {
    const fd = new FormData()
    fd.append('file', file)
    const token = getToken()
    const res = await fetch(BASE + '/accounts/decode-qrcode', {
      method: 'POST',
      headers: { 'Authorization': 'Bearer ' + token },
      body: fd
    })
    if (res.status === 401) {
      sessionStorage.removeItem('admin_token')
      window.location.reload()
      throw new Error('请重新登录')
    }
    const json = await res.json()
    if (!res.ok) throw new Error(json.error || `HTTP ${res.status}`)
    return json.data.content
  },

  // 上游对接方管理（多商户）
  upstreams: () => request('/upstreams'),
  getUpstream: (id) => request(`/upstreams/${id}`),
  // 上游完整密钥（明文，需登录）：对接指引「复制」真实可用密钥用
  upstreamSecret: (id) => request(`/upstreams/${id}/secret`),
  createUpstream: (data) => request('/upstreams', {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  updateUpstream: (id, data) => request(`/upstreams/${id}`, {}, { method: 'PUT', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  deleteUpstream: (id) => request(`/upstreams/${id}`, {}, { method: 'DELETE' }),
  enableUpstream: (id) => request(`/upstreams/${id}/enable`, {}, { method: 'POST' }),
  disableUpstream: (id) => request(`/upstreams/${id}/disable`, {}, { method: 'POST' }),
  upstreamLogs: (id, limit) => request(`/upstreams/${id}/logs`, { limit }),

  // 容灾对账中心（异常池 + dead 通知）
  listExceptions: (params) => request('/reconciler/exceptions', params),
  getException: (id) => request(`/reconciler/exceptions/${id}`),
  requeryException: (id) => request(`/reconciler/exceptions/${id}/requery`, {}, { method: 'POST' }),
  resolveException: (id, data) => request(`/reconciler/exceptions/${id}/resolve`, {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  reopenException: (id) => request(`/reconciler/exceptions/${id}/reopen`, {}, { method: 'POST' }),
  addException: (data) => request('/reconciler/exceptions', {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  listDeadNotify: () => request('/reconciler/notify-dead'),
  retryDeadNotify: (id) => request(`/reconciler/notify-dead/${id}/retry`, {}, { method: 'POST' }),
  deleteDeadNotify: (id) => request(`/reconciler/notify-dead/${id}/delete`, {}, { method: 'POST' }),
  // 日终对账报告（RESILIENCE P2-1）
  dailyReport: (params) => request('/reconciler/daily-report', params),

  // 个人免签：监听端设备 + 推送流水 + 无订单收款（新设备统一走 USB 引导向导的 wizard-start）
  personalDevices: () => request('/personal/devices'),
  deletePersonalDevice: (id) => request(`/personal/devices/${id}`, {}, { method: 'DELETE' }),
  freezePersonalDevice: (id) => request(`/personal/devices/${id}/freeze`, {}, { method: 'POST' }),
  unfreezePersonalDevice: (id) => request(`/personal/devices/${id}/unfreeze`, {}, { method: 'POST' }),
  restartPersonalDevice: (id) => request(`/personal/devices/${id}/restart`, {}, { method: 'POST' }),
  personalPushes: (params) => request('/personal/pushes', params),
  personalUnmatched: (params) => request('/personal/unmatched', params),
  resolveUnmatched: (id, data) => request(`/personal/unmatched/${id}/resolve`, {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  // 监听端配置二维码（docs/MIANQIAN.md §7.6）：返回 PNG blob URL
  personalConfigQR: async (id) => {
    const token = getToken()
    const res = await fetch(BASE + `/personal/devices/${id}/config-qr`, { headers: { 'Authorization': 'Bearer ' + token } })
    if (res.status === 401) {
      sessionStorage.removeItem('admin_token')
      window.location.reload()
      throw new Error('请重新登录')
    }
    const body = await res.json().catch(() => ({}))
    if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`)
    const blob = await res.blob()
    return URL.createObjectURL(blob)
  },
  // 设备控制台（web 可视化操作 USB 真机 / adb over WiFi 设备，docs/MIANQIAN.md §7.0 增强）
  personalScreenshot: async (id) => {
    const token = getToken()
    const res = await fetch(BASE + `/personal/devices/${id}/screenshot`, { headers: { 'Authorization': 'Bearer ' + token } })
    if (res.status === 401) {
      sessionStorage.removeItem('admin_token')
      window.location.reload()
      throw new Error('请重新登录')
    }
    const ct = res.headers.get('Content-Type') || ''
    if (!res.ok || !ct.includes('image')) {
      const body = await res.json().catch(() => ({}))
      throw new Error(body.error || `HTTP ${res.status}`)
    }
    const blob = await res.blob()
    return URL.createObjectURL(blob)
  },
  personalDeviceAction: (id, data) => request(`/personal/devices/${id}/action`, {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  personalSetAdb: (id, data) => request(`/personal/devices/${id}/adb`, {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  // 设备 ↔ 账户 多对多绑定（应用双开多账户）
  personalDeviceAccounts: (id) => request(`/personal/devices/${id}/accounts`),
  personalAddDeviceAccount: (id, data) => request(`/personal/devices/${id}/accounts`, {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  personalRemoveDeviceAccount: (id, accountId) => request(`/personal/devices/${id}/accounts/${accountId}`, {}, { method: 'DELETE' }),
  // 批量运维（ids 为空 + all=true 作用于全部设备）
  personalBatchRestart: (data) => request('/personal/devices/batch/restart', {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  personalBatchStatus: (data) => request('/personal/devices/batch/status', {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  // USB 真机引导向导
  personalUsbDetect: () => request('/personal/usb-detect'),
  personalWizardStart: (data) => request('/personal/devices/wizard-start', {}, { method: 'POST', body: JSON.stringify(data), headers: { 'Content-Type': 'application/json' } }),
  personalWizardWifi: (id) => request(`/personal/devices/${id}/wizard-wifi`, {}, { method: 'POST' }),
  personalWizardVerify: (id) => request(`/personal/devices/${id}/wizard-verify`, {}, { method: 'POST' }),
  personalWifiReconnect: (id) => request(`/personal/devices/${id}/wifi-reconnect`, {}, { method: 'POST' }),
  // 动态收款码
  personalQrcodes: () => request('/personal/qrcodes'),
  personalDeleteQrcode: (deviceId, platform, user = 0) => request(`/personal/qrcodes/${deviceId}/${platform}/${user}`, {}, { method: 'DELETE' }),
  // 微信收款码：服务器 adb 驱动捕获（微信 SECURE 需服务器 adb 通道）
  personalCaptureWechatQrcode: (deviceId, user = 0) => request(`/personal/qrcodes/${deviceId}/wechat/capture?user=${user}`, {}, { method: 'POST' }),
  personalCaptureAlipayQrcode: (deviceId, user = 999) => request(`/personal/qrcodes/${deviceId}/alipay/capture?user=${user}`, {}, { method: 'POST' }),
}
