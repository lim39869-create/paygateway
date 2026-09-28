<template>
  <div class="logs-page">
    <!-- 统计行 -->
    <div class="stat-grid">
      <div class="stat-card pg-card">
        <div class="stat-icon danger"><el-icon :size="18"><CircleClose /></el-icon></div>
        <div><div class="stat-num">{{ logStats.error }}</div><div class="stat-label">错误</div></div>
      </div>
      <div class="stat-card pg-card">
        <div class="stat-icon warn"><el-icon :size="18"><Warning /></el-icon></div>
        <div><div class="stat-num">{{ logStats.warn }}</div><div class="stat-label">警告</div></div>
      </div>
      <div class="stat-card pg-card">
        <div class="stat-icon info"><el-icon :size="18"><InfoFilled /></el-icon></div>
        <div><div class="stat-num">{{ logStats.info }}</div><div class="stat-label">信息</div></div>
      </div>
      <div class="stat-card pg-card">
        <div class="stat-icon primary"><el-icon :size="18"><Document /></el-icon></div>
        <div><div class="stat-num">{{ total }}</div><div class="stat-label">日志总数</div></div>
      </div>
    </div>

    <!-- 筛选 + 日志流 -->
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="section-label">日志流</span>
        <div style="display: flex; gap: 8px; flex-wrap: wrap">
          <el-select v-model="level" placeholder="全部级别" clearable size="small" style="width: 110px" @change="reload">
            <el-option label="错误" value="ERROR" />
            <el-option label="警告" value="WARN" />
            <el-option label="信息" value="INFO" />
            <el-option label="调试" value="DEBUG" />
          </el-select>
          <el-select v-model="scope" placeholder="全部模块" clearable size="small" style="width: 120px" @change="reload">
            <el-option v-for="s in scopes" :key="s" :label="s" :value="s" />
          </el-select>
          <el-button size="small" type="primary" @click="reload">
            <el-icon><Refresh /></el-icon>&nbsp;刷新
          </el-button>
        </div>
      </div>

      <div class="log-stream" v-loading="loading">
        <template v-if="list.length">
          <div v-for="l in list" :key="l.id" class="log-line" :class="'lv-' + l.level.toLowerCase()">
            <span class="log-time font-mono">{{ fmtTime(l.created_at) }}</span>
            <span class="log-level" :class="'lv-' + l.level.toLowerCase()">{{ l.level }}</span>
            <span class="log-scope">{{ l.scope }}</span>
            <span class="log-msg">{{ l.message }}</span>
            <el-button v-if="l.fields || l.trace_id" size="small" text type="primary" @click="showDetail(l)">详情</el-button>
          </div>
        </template>
        <div v-else-if="!loading" class="empty-tip">
          <el-icon :size="32" color="var(--text-muted)"><Document /></el-icon>
          <div class="empty-text">没有找到日志</div>
          <div class="empty-sub">试试清除级别/模块筛选，或等待新日志产生</div>
        </div>
      </div>

      <div class="pager mt16">
        <el-pagination background layout="total, prev, pager, next" :total="total"
          :page-size="50" :current-page="page" @current-change="onPage" />
      </div>
    </div>

    <el-dialog v-model="detailDialog" title="日志详情" width="580px">
      <el-descriptions v-if="current" :column="1" border size="small">
        <el-descriptions-item label="ID">{{ current.id }}</el-descriptions-item>
        <el-descriptions-item label="级别">{{ current.level }}</el-descriptions-item>
        <el-descriptions-item label="模块">{{ current.scope }}</el-descriptions-item>
        <el-descriptions-item label="消息">{{ current.message }}</el-descriptions-item>
        <el-descriptions-item label="TraceID"><span class="font-mono">{{ current.trace_id || '—' }}</span></el-descriptions-item>
        <el-descriptions-item label="时间">{{ fmtTime(current.created_at) }}</el-descriptions-item>
        <el-descriptions-item v-if="current.fields" label="字段">
          <pre class="fields">{{ current.fields }}</pre>
        </el-descriptions-item>
      </el-descriptions>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, watch, onMounted } from 'vue'
import { api } from '../api'

const props = defineProps({ refreshKey: Number })

const list = ref([])
const total = ref(0)
const loading = ref(false)
const level = ref('')
const scope = ref('')
const page = ref(1)
const detailDialog = ref(false)
const current = ref(null)
const logStats = ref({ error: 0, warn: 0, info: 0 })

const scopes = ['main', 'epay', 'order', 'alipay', 'notify', 'ledger', 'tunnel', 'admin', 'logger']

async function load() {
  loading.value = true
  try {
    const data = await api.logs({ level: level.value, scope: scope.value, limit: 50, offset: (page.value - 1) * 50 })
    list.value = (data && data.list) || []
    total.value = (data && data.total) || 0
  } catch (e) { ElMessage.error('加载失败: ' + e.message); list.value = []; total.value = 0 }
  finally { loading.value = false }

  try {
    const [err, warn, info] = await Promise.allSettled([
      api.logs({ level: 'ERROR', limit: 1 }).then(r => r.total),
      api.logs({ level: 'WARN', limit: 1 }).then(r => r.total),
      api.logs({ level: 'INFO', limit: 1 }).then(r => r.total)
    ])
    logStats.value = {
      error: err.status === 'fulfilled' ? err.value : 0,
      warn: warn.status === 'fulfilled' ? warn.value : 0,
      info: info.status === 'fulfilled' ? info.value : 0
    }
  } catch (e) { /* ignore */ }
}

function reload() { page.value = 1; load() }
function onPage(p) { page.value = p; load() }
watch(() => props.refreshKey, load)

function showDetail(l) { current.value = l; detailDialog.value = true }
function fmtTime(t) { if (!t) return '—'; return new Date(t).toLocaleString('zh-CN', { hour12: false }) }

onMounted(load)
</script>

<style scoped>
.logs-page { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 12px; }
.section-label { font-weight: 590; font-size: 13px; color: var(--text-primary); }

.stat-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; }
.stat-card { display: flex; align-items: center; gap: 12px; padding: 16px 18px; }
.stat-icon {
  width: 34px; height: 34px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center; flex-shrink: 0;
}
.stat-icon.danger { background: var(--danger-bg); color: var(--danger); }
.stat-icon.warn { background: var(--warning-bg); color: var(--warning); }
.stat-icon.info { background: var(--info-bg); color: var(--info); }
.stat-icon.primary { background: var(--accent-light); color: var(--accent); }
.stat-num { font-size: 20px; font-weight: 590; color: var(--text-primary); font-family: 'JetBrains Mono', monospace; font-feature-settings: 'tnum'; }
.stat-label { font-size: 12px; color: var(--text-muted); margin-top: 2px; font-weight: 500; }

.log-stream {
  padding: 6px 12px;
  max-height: 520px;
  overflow-y: auto;
  font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace;
  font-size: 12px;
}
.log-line {
  display: flex; align-items: center; gap: 10px;
  padding: 5px 8px;
  border-radius: 5px;
  transition: background 0.1s;
}
.log-line:hover { background: var(--bg-hover); }
.log-time { color: var(--text-muted); flex-shrink: 0; font-size: 11px; }
.log-level {
  flex-shrink: 0; width: 46px; text-align: center;
  padding: 1px 5px; border-radius: 4px; font-size: 10px; font-weight: 700;
}
.log-level.lv-error, .log-line.lv-error .log-time { color: var(--danger); }
.log-level.lv-error { background: var(--danger-bg); }
.log-level.lv-warn, .log-line.lv-warn .log-time { color: var(--warning); }
.log-level.lv-warn { background: var(--warning-bg); }
.log-level.lv-info, .log-line.lv-info .log-time { color: var(--info); }
.log-level.lv-info { background: var(--info-bg); }
.log-level.lv-debug, .log-line.lv-debug .log-time { color: var(--text-muted); }
.log-level.lv-debug { background: var(--bg-subtle); }
.log-scope { color: var(--text-muted); flex-shrink: 0; width: 52px; font-size: 11px; }
.log-msg { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.pager { display: flex; justify-content: flex-end; padding: 0 16px 16px; }
.mt16 { margin-top: 16px; }
.empty-tip {
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  padding: 48px 0; gap: 8px;
}
.empty-text { font-size: 13px; color: var(--text-muted); }
.empty-sub { font-size: 12px; color: var(--text-muted); }
.fields {
  background: var(--bg-subtle);
  padding: 10px; border-radius: 6px;
  font-family: 'JetBrains Mono', Consolas, monospace; font-size: 12px;
  margin: 0; white-space: pre-wrap; word-break: break-all;
  border: 1px solid var(--border);
}
.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
</style>