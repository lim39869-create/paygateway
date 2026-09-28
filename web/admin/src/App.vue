<template>
  <Login v-if="!loggedIn" @login-success="loggedIn = true" />
  <div v-else class="app-shell" :class="{ dark: isDark }">
    <!-- 侧边栏 -->
    <aside class="sidebar">
      <div class="brand">
        <div class="brand-logo">
          <img src="/assets/favicon.svg" alt="PayGateway" width="30" height="30" />
        </div>
        <div class="brand-text">
          <div class="brand-name">PayGateway</div>
          <div class="brand-sub">管理控制台</div>
        </div>
      </div>

      <nav class="nav">
        <button v-for="item in menus" :key="item.id"
          class="nav-item" :class="{ active: active === item.id }"
          @click="go(item.id)">
          <el-icon :size="15"><component :is="item.icon" /></el-icon>
          <span>{{ item.label }}</span>
          <span v-if="item.badge" class="nav-badge" :class="item.badgeType">{{ item.badge }}</span>
        </button>
      </nav>

      <div class="sidebar-foot">
        <div class="tunnel-mini" :class="{ ok: tunnelHealthy }">
          <span class="dot"></span>
          <span class="tunnel-text">{{ tunnelHealthy ? '隧道正常' : '未连接' }}</span>
        </div>
      </div>
    </aside>

    <!-- 主区域 -->
    <div class="main-area">
      <!-- 顶栏 -->
      <header class="topbar">
        <div class="page-title">{{ pageTitle }}</div>
        <div class="topbar-right">
          <el-tooltip content="刷新数据" placement="bottom">
            <button class="icon-btn" @click="refreshAll">
              <el-icon :size="15"><Refresh /></el-icon>
            </button>
          </el-tooltip>
          <el-tooltip :content="isDark ? '浅色模式' : '深色模式'" placement="bottom">
            <button class="icon-btn" @click="toggleTheme">
              <el-icon :size="15"><Sunny v-if="isDark" /><Moon v-else /></el-icon>
            </button>
          </el-tooltip>
          <div class="avatar">G</div>
        </div>
      </header>

      <!-- 内容 -->
      <main class="content">
        <transition name="page" mode="out-in">
          <Dashboard v-if="active === 'dashboard'" :refresh-key="refreshKey" @tunnel="onTunnel" @goto="go" />
          <Channels v-else-if="active === 'channels'" :refresh-key="refreshKey" />
          <Orders v-else-if="active === 'orders'" :refresh-key="refreshKey" />
          <NotifyTasks v-else-if="active === 'notify'" :refresh-key="refreshKey" />
          <Logs v-else-if="active === 'logs'" :refresh-key="refreshKey" />
          <Accounts v-else-if="active === 'accounts'" :refresh-key="refreshKey" />
          <Upstreams v-else-if="active === 'upstreams'" :refresh-key="refreshKey" />
          <Reconciler v-else-if="active === 'reconciler'" :refresh-key="refreshKey" />
          <Ledger v-else-if="active === 'ledger'" :refresh-key="refreshKey" />
          <PersonalDevices v-else-if="active === 'devices'" :refresh-key="refreshKey" />
          <Unmatched v-else-if="active === 'unmatched'" :refresh-key="refreshKey" />
          <Qrcodes v-else-if="active === 'qrcodes'" :refresh-key="refreshKey" />
          <Settings v-else-if="active === 'settings'" :refresh-key="refreshKey" />
        </transition>
      </main>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import Login from './views/Login.vue'
import Dashboard from './views/Dashboard.vue'
import Channels from './views/Channels.vue'
import Orders from './views/Orders.vue'
import NotifyTasks from './views/NotifyTasks.vue'
import Logs from './views/Logs.vue'
import Ledger from './views/Ledger.vue'
import Accounts from './views/Accounts.vue'
import Upstreams from './views/Upstreams.vue'
import Reconciler from './views/Reconciler.vue'
import PersonalDevices from './views/PersonalDevices.vue'
import Unmatched from './views/Unmatched.vue'
import Settings from './views/Settings.vue'
import Qrcodes from './views/Qrcodes.vue'

const menus = [
  { id: 'dashboard', label: '仪表盘', icon: 'Odometer' },
  { id: 'channels', label: '支付通道', icon: 'Connection' },
  { id: 'orders', label: '订单管理', icon: 'List' },
  { id: 'notify', label: '回调队列', icon: 'Bell' },
  { id: 'logs', label: '系统日志', icon: 'Document' },
  { id: 'accounts', label: '账户池', icon: 'Wallet' },
  { id: 'upstreams', label: '上游对接', icon: 'Share' },
  { id: 'reconciler', label: '对账中心', icon: 'WarningFilled' },
  { id: 'ledger', label: '审计账本', icon: 'Lock' },
  { id: 'devices', label: '监听端', icon: 'Iphone' },
  { id: 'unmatched', label: '无订单收款', icon: 'Coin' },
  { id: 'qrcodes', label: '收款码', icon: 'Camera' },
  { id: 'settings', label: '系统设置', icon: 'Setting' }
]

const titles = {
  dashboard: '仪表盘',
  channels: '支付通道',
  orders: '订单管理',
  notify: '回调队列',
  logs: '系统日志',
  accounts: '账户池',
  upstreams: '上游对接',
  reconciler: '对账中心',
  ledger: '审计账本',
  devices: '监听端',
  unmatched: '无订单收款',
  qrcodes: '动态收款码',
  settings: '系统设置'
}

const loggedIn = ref(false)
const active = ref('dashboard')
const isDark = ref(false)
const tunnelHealthy = ref(false)
const refreshKey = ref(0)

onMounted(() => {
  const token = sessionStorage.getItem('admin_token')
  if (token) loggedIn.value = true
  const saved = localStorage.getItem('pg-theme')
  isDark.value = saved === 'dark'
  document.documentElement.classList.toggle('dark', isDark.value)
})

const pageTitle = computed(() => titles[active.value])

function go(id) { active.value = id }
function onTunnel(t) { tunnelHealthy.value = t?.running && t?.healthy }

function toggleTheme() {
  isDark.value = !isDark.value
  document.documentElement.classList.toggle('dark', isDark.value)
  localStorage.setItem('pg-theme', isDark.value ? 'dark' : 'light')
}

function refreshAll() { refreshKey.value++ }
</script>

<style scoped>
.app-shell {
  display: flex;
  height: 100vh;
  overflow: hidden;
}

/* ===== 侧边栏 ===== */
.sidebar {
  width: var(--sidebar-width);
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  background: var(--bg-sidebar);
  border-right: 1px solid var(--border);
  transition: background 0.18s ease, border-color 0.18s ease;
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 14px 14px 12px;
  border-bottom: 1px solid var(--border);
}

.brand-logo {
  width: 30px;
  height: 30px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.brand-logo img {
  display: block;
  width: 30px;
  height: 30px;
}

.brand-name {
  font-size: 13px;
  font-weight: 590;
  color: var(--text-primary);
  letter-spacing: -0.01em;
  line-height: 1.2;
}

.brand-sub {
  font-size: 11px;
  color: var(--text-muted);
  line-height: 1.2;
  margin-top: 1px;
}

/* ===== 导航 ===== */
.nav {
  flex: 1;
  padding: 8px 8px;
  display: flex;
  flex-direction: column;
  gap: 1px;
  overflow-y: auto;
}

.nav-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 0 10px;
  height: 32px;
  border: none;
  background: transparent;
  border-radius: 6px;
  color: var(--text-muted);
  font-size: 12px;
  font-weight: 410;
  cursor: pointer;
  transition: background 0.12s ease, color 0.12s ease;
  text-align: left;
  width: 100%;
  position: relative;
  letter-spacing: 0.01em;
}

.nav-item:hover {
  background: var(--bg-hover);
  color: var(--text-body);
}

.nav-item.active {
  background: var(--accent-light);
  color: var(--accent);
  font-weight: 510;
}

.nav-item.active::before {
  content: '';
  position: absolute;
  left: 0;
  top: 6px;
  bottom: 6px;
  width: 2px;
  border-radius: 0 2px 2px 0;
  background: var(--accent);
}

.nav-badge {
  margin-left: auto;
  font-size: 10px;
  font-weight: 590;
  padding: 1px 5px;
  border-radius: 4px;
  background: var(--danger-bg);
  color: var(--danger);
  border: 1px solid var(--danger-border);
  letter-spacing: 0.02em;
}

.nav-badge.warn {
  background: var(--warning-bg);
  color: var(--warning);
  border-color: var(--warning-border);
}

/* ===== 侧边栏底部 ===== */
.sidebar-foot {
  padding: 10px 14px;
  border-top: 1px solid var(--border);
  margin-top: auto;
}

.tunnel-mini {
  display: flex;
  align-items: center;
  gap: 7px;
  font-size: 11px;
  color: var(--text-muted);
  font-weight: 410;
}

.tunnel-mini .dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--danger);
  flex-shrink: 0;
  transition: background 0.2s;
}

.tunnel-mini.ok .dot { background: var(--success); }
.tunnel-mini.ok .tunnel-text { color: var(--success); }

/* ===== 主区域 ===== */
.main-area {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
  background: var(--bg-base);
}

.topbar {
  height: var(--topbar-height);
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 20px;
  background: var(--bg-topbar);
  border-bottom: 1px solid var(--border);
  transition: background 0.18s ease, border-color 0.18s ease;
}

.page-title {
  font-size: 13px;
  font-weight: 590;
  color: var(--text-primary);
  letter-spacing: -0.01em;
}

.topbar-right {
  display: flex;
  align-items: center;
  gap: 4px;
}

.icon-btn {
  width: 30px;
  height: 30px;
  border: 1px solid transparent;
  background: transparent;
  border-radius: 6px;
  color: var(--text-muted);
  cursor: pointer;
  display: flex;
  align-items: center;
  justify-content: center;
  transition: all 0.12s ease;
}

.icon-btn:hover {
  color: var(--accent);
  border-color: var(--border);
  background: var(--bg-hover);
}

.avatar {
  width: 28px;
  height: 28px;
  border-radius: 50%;
  background: var(--accent);
  color: #fff;
  display: flex;
  align-items: center;
  justify-content: center;
  font-weight: 590;
  font-size: 12px;
  cursor: pointer;
  margin-left: 4px;
  letter-spacing: 0.02em;
}

/* ===== 内容区 ===== */
.content {
  flex: 1;
  overflow-y: auto;
  padding: 20px 24px;
}

/* 页面切换动画 */
.page-enter-active {
  transition: opacity 0.15s ease, transform 0.15s cubic-bezier(0.16, 1, 0.3, 1);
}
.page-leave-active {
  transition: opacity 0.10s ease;
}
.page-enter-from {
  opacity: 0;
  transform: translateY(4px);
}
.page-leave-to {
  opacity: 0;
}
</style>
