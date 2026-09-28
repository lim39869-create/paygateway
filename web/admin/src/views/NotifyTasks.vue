<template>
  <div class="notify-page">
    <!-- 队列统计 -->
    <div class="stat-grid">
      <div class="stat-card pg-card">
        <div class="stat-icon success"><el-icon :size="20"><CircleCheck /></el-icon></div>
        <div><div class="stat-num">{{ stats.success ?? 0 }}</div><div class="stat-label">成功投递</div></div>
      </div>
      <div class="stat-card pg-card">
        <div class="stat-icon warn"><el-icon :size="20"><Clock /></el-icon></div>
        <div><div class="stat-num">{{ stats.pending ?? 0 }}</div><div class="stat-label">待重试</div></div>
      </div>
      <div class="stat-card pg-card">
        <div class="stat-icon danger"><el-icon :size="20"><Warning /></el-icon></div>
        <div><div class="stat-num">{{ stats.dead ?? 0 }}</div><div class="stat-label">死信</div></div>
      </div>
      <div class="stat-card pg-card">
        <div class="stat-icon info"><el-icon :size="20"><List /></el-icon></div>
        <div><div class="stat-num">{{ stats.total ?? 0 }}</div><div class="stat-label">任务总数</div></div>
      </div>
    </div>

    <el-alert v-if="(stats.dead ?? 0) > 0" type="error" :closable="false" class="mt12"
      title="检测到死信任务！商户回调多次失败，请检查商户服务器 notify_url 可用性。" />

    <!-- 任务列表 -->
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="section-label">回调任务队列</span>
        <div style="display: flex; gap: 8px">
          <el-select v-model="status" placeholder="全部状态" clearable size="small" style="width: 120px" @change="load">
            <el-option label="待处理" value="pending" />
            <el-option label="成功" value="success" />
            <el-option label="死信" value="dead" />
          </el-select>
          <el-button size="small" type="primary" @click="load">
            <el-icon><Refresh /></el-icon>&nbsp;刷新
          </el-button>
        </div>
      </div>

      <el-table v-loading="loading" :data="list" size="small" empty-text="没有找到回调任务，试试清除状态筛选">
        <el-table-column prop="id" label="ID" width="70" />
        <el-table-column prop="out_trade_no" label="商户订单号" min-width="150" show-overflow-tooltip />
        <el-table-column prop="subject" label="商品" min-width="90" show-overflow-tooltip />
        <el-table-column label="金额" width="90">
          <template #default="{ row }"><b>¥{{ (row.amount_minor / 100).toFixed(2) }}</b></template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="statusType(row.status)" size="small" effect="light" round>{{ statusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="retry_count" label="重试次数" width="90">
          <template #default="{ row }">
            <el-tag v-if="row.retry_count > 0" type="warning" size="small">{{ row.retry_count }}</el-tag>
            <span v-else class="text-muted">0</span>
          </template>
        </el-table-column>
        <el-table-column label="下次重试" width="170">
          <template #default="{ row }">{{ fmtTime(row.next_retry_at) }}</template>
        </el-table-column>
        <el-table-column label="创建时间" width="170">
          <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
        </el-table-column>
        <el-table-column label="载荷" width="80">
          <template #default="{ row }">
            <el-button size="small" text type="primary" @click="showPayload(row)">查看</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 载荷弹窗 -->
    <el-dialog v-model="payloadDialog" title="回调载荷" width="620px">
      <div class="payload-head">
        <span class="font-mono">{{ current?.out_trade_no }}</span>
        <el-tag :type="statusType(current?.status)" size="small" effect="light">{{ statusText(current?.status) }}</el-tag>
      </div>
      <pre class="payload">{{ payloadText }}</pre>
      <template #footer>
        <el-button @click="payloadDialog = false">关闭</el-button>
        <el-button type="primary" :disabled="current?.status === 'success'" @click="manualRetry">手动重试</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, watch, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '../api'

const props = defineProps({ refreshKey: Number })

const list = ref([])
const stats = ref({})
const status = ref('')
const loading = ref(false)
const payloadDialog = ref(false)
const payloadText = ref('')
const current = ref(null)

async function load() {
  loading.value = true
  try {
    const [tasks, st] = await Promise.all([
      api.notifyTasks({ status: status.value, limit: 50 }),
      api.notifyStats()
    ])
    list.value = tasks; stats.value = st
  } catch (e) { ElMessage.error('加载失败: ' + e.message) }
  finally { loading.value = false }
}

function showPayload(row) {
  current.value = row
  try { payloadText.value = JSON.stringify(JSON.parse(row.payload), null, 2) }
  catch { payloadText.value = row.payload }
  payloadDialog.value = true
}

async function manualRetry() {
  if (!current.value?.id) { ElMessage.warning('未选中任务'); return }
  try {
    await api.retryDeadNotify(current.value.id)
    ElMessage.success('已按最新回调地址重新入队')
  } catch (e) { ElMessage.error('重试失败: ' + e.message) }
  finally { payloadDialog.value = false; load() }
}

watch(() => props.refreshKey, load)

function statusText(s) { return { pending: '待处理', success: '成功', dead: '死信' }[s] || s }
function statusType(s) { return { pending: 'warning', success: 'success', dead: 'danger' }[s] || 'info' }
function fmtTime(t) { if (!t) return '—'; return new Date(t).toLocaleString('zh-CN', { hour12: false }) }

onMounted(load)
</script>

<style scoped>
.notify-page { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 12px; }
.section-label { font-weight: 590; font-size: 13px; color: var(--text-primary); }

.stat-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; }
.stat-card { display: flex; align-items: center; gap: 14px; padding: 16px 18px; }
.stat-icon {
  width: 36px; height: 36px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center; flex-shrink: 0;
}
.stat-icon.success { background: var(--success-bg); color: var(--success); }
.stat-icon.warn { background: var(--warning-bg); color: var(--warning); }
.stat-icon.danger { background: var(--danger-bg); color: var(--danger); }
.stat-icon.info { background: var(--info-bg); color: var(--info); }
.stat-num { font-size: 20px; font-weight: 590; color: var(--text-primary); font-family: 'JetBrains Mono', monospace; font-feature-settings: 'tnum'; }
.stat-label { font-size: 12px; color: var(--text-muted); margin-top: 2px; font-weight: 500; }

.mt12 { margin-top: 12px; }
.payload-head { display: flex; gap: 10px; align-items: center; margin-bottom: 12px; }
.payload {
  background: var(--bg-subtle);
  padding: 14px;
  border-radius: 8px;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
  max-height: 400px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 0;
  color: var(--text-body);
  border: 1px solid var(--border);
}
.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
.text-muted { color: var(--text-muted); }
</style>