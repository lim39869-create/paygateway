<template>
  <div class="settings-page">
    <div class="pg-card">
      <div class="pg-card-header"><span class="section-label">系统设置</span></div>
      <div class="pg-card-body">
        <el-alert type="info" :closable="false" show-icon class="mb16"
          title="设置保存后立即生效，无需重启。" />
        <el-alert type="info" :closable="false" show-icon class="mb16"
          title="上游对接（易支付 pid/密钥/服务地址）已迁移到「上游对接」页：每个上游独立配置，AES-256-GCM 加密存于 upstreams.db。" />
        <el-divider />

        <h3 class="sec-title">支付通道</h3>
        <div class="channel-grid">
          <div v-for="ch in channels" :key="ch.name" class="channel-switch" :class="{ disabled: !ch.enabled }">
            <div class="ch-info">
              <div class="ch-name">{{ ch.name }}</div>
              <div class="ch-status">{{ ch.enabled ? '已启用' : '已禁用' }}</div>
            </div>
            <el-switch v-model="ch.enabled" @change="toggleChannel(ch.name)" />
          </div>
        </div>
        <el-divider />

        <h3 class="sec-title">系统参数</h3>
        <el-form :model="sys" label-width="180px" size="small">
          <el-form-item label="订单超时关闭（分钟）">
            <el-input-number v-model="sys.order_close_minutes" :min="0" :max="1440" />
            <div class="form-tip">0 = 不自动关闭</div>
          </el-form-item>
          <el-form-item label="扫描间隔（秒）">
            <el-input-number v-model="sys.order_close_interval" :min="10" :max="600" />
          </el-form-item>
        </el-form>

        <div class="mt16">
          <el-button type="primary" size="default" @click="saveAll" :loading="saving">保存全部设置</el-button>
          <span v-if="saved" class="saved-hint">
            <el-icon :size="14"><CircleCheck /></el-icon> 已保存
          </span>
        </div>

        <el-divider />

        <h3 class="sec-title">管理员账号</h3>
        <div class="admin-item">
          <div class="admin-info">
            <div class="admin-label">当前登录账号</div>
            <div class="admin-value font-mono">{{ adminUsername || '—' }}</div>
          </div>
          <el-button size="small" type="danger" plain @click="openReset">重置管理员</el-button>
        </div>
        <el-alert type="warning" :closable="false" show-icon class="mb12 mt12"
          title="重置管理员会清除当前登录凭据并回到首次注册流程，需重新设置新的管理员账号（旧账号立即失效）。此操作需验证当前密码。" />

        <el-divider />

        <h3 class="sec-title">安全密钥</h3>
        <el-alert type="info" :closable="false" show-icon class="mb12"
          title="账本签名私钥与套码 token 密钥已自动生成并存库（首次启动无需手动配置）。查看完整值需确认，操作会记入审计日志。" />
        <div class="key-grid">
          <div v-for="item in keyItems" :key="item.key" class="key-item">
            <div class="key-label">{{ item.label }}</div>
            <div class="key-value font-mono">{{ showKey === item.key ? (keys[item.key] || '（未生成）') : '••••••••••••••••' }}</div>
            <div class="key-actions">
              <el-button size="small" text type="primary" @click="toggleKey(item.key)">
                {{ showKey === item.key ? '隐藏' : '查看' }}
              </el-button>
              <el-button size="small" text type="primary" :disabled="showKey !== item.key" @click="copyKey(item.key)">复制</el-button>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 重置管理员对话框 -->
    <el-dialog v-model="resetVisible" title="重置管理员账号" width="440px" :close-on-click-modal="false">
      <el-alert type="warning" :closable="false" class="mb16" show-icon
        title="重置后将回到首次注册页，需要重新设置管理员。当前账号立即失效。" />
      <el-form label-width="100px" size="small">
        <el-form-item label="当前密码" required>
          <el-input v-model="resetPassword" type="password" show-password
            placeholder="验证当前管理员密码" @keyup.enter="doReset" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="resetVisible = false">取消</el-button>
        <el-button type="danger" :loading="resetLoading" @click="doReset">确认重置</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { CircleCheck } from '@element-plus/icons-vue'
import { api } from '../api'
import { bumpRefresh, refreshVersion } from '../store'

const sys = reactive({ order_close_minutes: 30, order_close_interval: 60 })
const channels = ref([])
const saving = ref(false)
const saved = ref(false)
const keys = ref({})
const showKey = ref('')
const adminUsername = ref(sessionStorage.getItem('admin_username') || '')
const resetVisible = ref(false)
const resetPassword = ref('')
const resetLoading = ref(false)

const keyItems = [
  { key: 'ledger_private_key', label: '审计账本私钥（ECDSA P-256）' },
  { key: 'qr_token_secret', label: '套码 token 签名密钥' }
]

function openReset() {
  resetPassword.value = ''
  resetVisible.value = true
}

async function doReset() {
  if (!resetPassword.value) { ElMessage.warning('请输入当前密码'); return }
  resetLoading.value = true
  try {
    await api.resetAdmin(resetPassword.value)
    ElMessage.success('管理员已重置，正在跳转到初始化页...')
    // 清除本地会话，跳回首次注册页
    sessionStorage.removeItem('admin_token')
    sessionStorage.removeItem('admin_username')
    setTimeout(() => { window.location.reload() }, 1200)
  } catch (e) {
    ElMessage.error('重置失败: ' + e.message)
  } finally {
    resetLoading.value = false
  }
}

async function load() {
  try {
    const data = await api.settings()
    const s = data.settings || {}
    if (s.order_close_minutes) sys.order_close_minutes = parseInt(s.order_close_minutes)
    if (s.order_close_interval) sys.order_close_interval = parseInt(s.order_close_interval)
    channels.value = (data.channels || []).map(name => ({ name, enabled: data.channel_enabled?.[name] !== false }))
  } catch (e) { ElMessage.error('加载设置失败: ' + e.message) }
}

async function toggleKey(key) {
  if (showKey.value === key) {
    showKey.value = ''
    return
  }
  try {
    await ElMessageBox.confirm(
      '将显示完整密钥（明文），请确认当前环境安全。查看行为会记入审计日志。',
      '查看安全密钥', { confirmButtonText: '确认查看', cancelButtonText: '取消', type: 'warning' }
    )
    const data = await api.settingsKeys()
    keys.value = data || {}
    showKey.value = key
  } catch (e) {
    if (e !== 'cancel') ElMessage.error('获取密钥失败: ' + (e.message || e))
  }
}

async function copyKey(key) {
  const v = keys.value[key]
  if (!v) { ElMessage.warning('密钥不存在'); return }
  try {
    await navigator.clipboard.writeText(v)
    ElMessage.success('已复制')
  } catch (e) { ElMessage.error('复制失败: ' + e.message) }
}

async function saveAll() {
  saving.value = true; saved.value = false
  try {
    const settings = []
    settings.push({ key: 'order_close_minutes', value: String(sys.order_close_minutes) })
    settings.push({ key: 'order_close_interval', value: String(sys.order_close_interval) })
    await api.saveSettings({ settings })
    bumpRefresh()
    saved.value = true
    setTimeout(() => { saved.value = false }, 3000)
    ElMessage.success('保存成功')
  } catch (e) { ElMessage.error('保存失败: ' + e.message) }
  finally { saving.value = false }
}

async function toggleChannel(name) {
  try {
    const r = await api.toggleChannel(name)
    const ch = channels.value.find(c => c.name === name)
    if (ch && ch.enabled !== r.enabled) ch.enabled = r.enabled
    bumpRefresh()
    ElMessage.success(`通道 ${name} 已${r.enabled ? '启用' : '禁用'}`)
  } catch (e) {
    const ch = channels.value.find(c => c.name === name)
    if (ch) ch.enabled = !ch.enabled
    ElMessage.error('切换失败: ' + e.message)
  }
}

watch(refreshVersion, load)
onMounted(load)
</script>

<style scoped>
.settings-page { max-width: 800px; margin: 0 auto; }
.section-label { font-weight: 590; font-size: 13px; color: var(--text-primary); }
.mb16 { margin-bottom: 16px; }
.mt16 { margin-top: 16px; }
.sec-title { font-size: 14px; margin: 0 0 12px; color: var(--text-primary); font-weight: 600; }
.form-tip { font-size: 11px; color: var(--text-muted); margin-top: 4px; }
.saved-hint { display: inline-flex; align-items: center; gap: 4px; margin-left: 12px; color: var(--success); font-size: 13px; font-weight: 500; }

.channel-grid { display: flex; flex-wrap: wrap; gap: 10px; margin-bottom: 16px; }
.channel-switch {
  display: flex; align-items: center; gap: 14px;
  padding: 12px 16px;
  background: var(--bg-subtle);
  border-radius: 8px;
  border: 1px solid var(--border);
  min-width: 150px;
}
.channel-switch.disabled { opacity: 0.5; }
.ch-name { font-weight: 600; font-size: 13px; color: var(--text-primary); }
.ch-status { font-size: 11px; color: var(--text-muted); margin-top: 2px; }

.mb12 { margin-bottom: 12px; }
.mt12 { margin-top: 12px; }
.key-grid { display: flex; flex-direction: column; gap: 10px; }
.key-item {
  background: var(--bg-subtle);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 12px 14px;
  display: flex; align-items: center; gap: 14px; flex-wrap: wrap;
}
.key-label { font-size: 12px; color: var(--text-muted); font-weight: 510; width: 180px; flex-shrink: 0; }
.key-value {
  flex: 1; min-width: 160px;
  font-size: 12px; color: var(--text-primary);
  word-break: break-all; font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace;
}
.key-actions { display: flex; gap: 2px; flex-shrink: 0; }

.admin-item {
  display: flex; align-items: center; justify-content: space-between; gap: 14px;
  background: var(--bg-subtle); border: 1px solid var(--border); border-radius: 8px;
  padding: 12px 14px;
}
.admin-label { font-size: 11px; color: var(--text-muted); margin-bottom: 4px; font-weight: 510; }
.admin-value { font-size: 13px; color: var(--text-primary); }
</style>