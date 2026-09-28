<template>
  <div class="ledger-page">
    <!-- 操作栏 -->
    <div class="action-bar pg-card">
      <el-button type="primary" :loading="verifyLoading" @click="verifyLedger" plain>
        <el-icon><CircleCheck /></el-icon>&nbsp;校验账本
      </el-button>
      <el-button :loading="snapshotLoading" @click="createSnapshot" plain>
        <el-icon><Camera /></el-icon>&nbsp;创建快照
      </el-button>
      <el-button @click="showPublicKey" plain>
        <el-icon><Key /></el-icon>&nbsp;账本公钥
      </el-button>
      <el-button :loading="exportLoading" @click="exportLedger" plain>
        <el-icon><Download /></el-icon>&nbsp;导出CSV
      </el-button>
      <el-tag v-if="lastVerify" :type="lastVerify.valid ? 'success' : 'danger'" size="small" effect="plain" class="verify-tag">
        {{ lastVerify.valid ? '上次校验通过' : '上次校验失败' }}
        <span class="tag-muted">({{ lastVerify.checked_at ? fmtTime(lastVerify.checked_at) : '—' }})</span>
      </el-tag>
    </div>

    <!-- 标签页 -->
    <el-tabs v-model="activeTab" class="mt12">
      <el-tab-pane label="账本条目" name="entries">
        <div class="pg-card">
          <div class="pg-card-header">
            <span class="section-label">审计账本 · 哈希链</span>
            <div style="display: flex; align-items: center; gap: 10px">
              <el-tag type="success" size="small" effect="light">共 {{ total }} 条</el-tag>
              <el-button size="small" @click="loadEntries"><el-icon><Refresh /></el-icon>&nbsp;刷新</el-button>
            </div>
          </div>
          <div class="pg-card-body">
            <el-alert type="info" :closable="false" class="mb16"
              title="账本为哈希链结构：每条记录包含上一记录哈希 + ECDSA 签名。任何历史修改都会导致链条断裂。" />

            <div class="chain" v-loading="loading">
              <template v-for="(row, i) in list" :key="row.id">
                <div class="chain-node" :class="{ head: i === 0 }">
                  <div class="chain-badge">#{{ row.id }}</div>
                  <div class="chain-body">
                    <div class="chain-row"><span class="chain-label">事件ID</span>
                      <span class="font-mono event-link" @click="goToOrder(row.event_id)" v-if="row.event_id">{{ row.event_id }}</span>
                      <span class="font-muted" v-else>—</span>
                    </div>
                    <div class="chain-row"><span class="chain-label">条目哈希</span>
                      <span class="font-mono hash muted">{{ row.entry_hash }}</span>
                    </div>
                    <div class="chain-row"><span class="chain-label">前置哈希</span>
                      <span class="font-mono hash muted">{{ row.prev_hash || '—（创世）' }}</span>
                    </div>
                    <div class="chain-row"><span class="chain-label">时间</span><span class="font-muted">{{ fmtTime(row.created_at) }}</span></div>
                  </div>
                  <el-button size="small" text type="primary" @click="showDetail(row)">完整</el-button>
                </div>
                <div v-if="i < list.length - 1" class="chain-link">
                  <el-icon color="var(--success)"><Link /></el-icon>
                </div>
              </template>
              <el-empty v-if="!list.length && !loading" description="账本为空" :image-size="60" />
            </div>
          </div>
        </div>
      </el-tab-pane>

      <el-tab-pane label="快照管理" name="snapshots">
        <div class="pg-card">
          <div class="pg-card-header">
            <span class="section-label">每日快照</span>
            <div style="display: flex; align-items: center; gap: 10px">
              <el-tag size="small" effect="light">共 {{ snapshots.length }} 条</el-tag>
              <el-button size="small" @click="loadSnapshots"><el-icon><Refresh /></el-icon>&nbsp;刷新</el-button>
            </div>
          </div>
          <div class="pg-card-body">
            <el-alert type="info" :closable="false" class="mb16"
              title="快照是对账本链尾哈希的签名记录，用于锚定截至某日的账本状态。" />

            <div class="snapshot-grid" v-loading="snapLoading">
              <div v-for="sn in snapshots" :key="sn.id" class="snapshot-card" @click="showSnapshotDetail(sn)" style="cursor:pointer">
                <div class="snapshot-date">{{ sn.snapshot_date }}</div>
                <div class="snapshot-body">
                  <div class="sn-row"><span class="sn-label">链尾哈希</span><span class="font-mono hash muted">{{ sn.last_hash }}</span></div>
                  <div class="sn-row"><span class="sn-label">签名</span><span class="font-mono hash muted">{{ sn.signature }}</span></div>
                  <div class="sn-row"><span class="sn-label">时间</span><span class="font-muted">{{ fmtTime(sn.created_at) }}</span></div>
                </div>
                <el-icon class="detail-icon" color="var(--accent)"><View /></el-icon>
              </div>
              <el-empty v-if="!snapshots.length && !snapLoading" description="暂无快照" :image-size="60" />
            </div>
          </div>
        </div>
      </el-tab-pane>
    </el-tabs>

    <!-- 校验结果弹窗 -->
    <el-dialog v-model="verifyDialog" title="账本校验结果" width="600px" :close-on-click-modal="false">
      <div v-if="verifyResult" class="verify-result">
        <div class="verify-status" :class="verifyResult.valid ? 'pass' : 'fail'">
          <el-icon :size="48"><CircleCheck v-if="verifyResult.valid" /><WarningFilled v-else /></el-icon>
          <div class="verify-status-text">{{ verifyResult.valid ? '账本完整' : '账本校验失败' }}</div>
        </div>
        <el-descriptions :column="2" border size="small" class="mt16">
          <el-descriptions-item label="总条目数">{{ verifyResult.total_entries }}</el-descriptions-item>
          <el-descriptions-item label="快照锚点">
            <el-tag :type="verifyResult.snapshot_ok ? 'success' : 'danger'" size="small">
              {{ verifyResult.snapshot_ok ? '匹配' : '不匹配' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="耗时">{{ verifyResult.duration || '—' }}</el-descriptions-item>
          <el-descriptions-item label="检查时间">{{ fmtTime(verifyResult.checked_at) }}</el-descriptions-item>
        </el-descriptions>
        <div v-if="verifyResult.errors && verifyResult.errors.length" class="verify-errors mt16">
          <div class="verify-errors-title">错误详情（{{ verifyResult.errors.length }} 处）</div>
          <div v-for="(err, i) in verifyResult.errors" :key="i" class="verify-error-item">
            <el-icon color="var(--danger)"><CircleCloseFilled /></el-icon>
            <span>{{ err }}</span>
          </div>
        </div>
      </div>
      <div v-else class="verify-loading" v-loading="verifyLoading">
        <span>正在校验账本...</span>
      </div>
    </el-dialog>

    <!-- 快照详情弹窗 -->
    <el-dialog v-model="snapDetailDialog" title="快照详情" width="600px">
      <div v-if="snapDetail" class="snap-detail">
        <el-descriptions :column="1" border size="small">
          <el-descriptions-item label="快照日期">{{ snapDetail.snapshot_date }}</el-descriptions-item>
          <el-descriptions-item label="链尾哈希">
            <span class="font-mono" style="font-size:12px;word-break:break-all">{{ snapDetail.last_hash }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="签名">
            <span class="font-mono" style="font-size:12px;word-break:break-all;color:var(--text-muted)">{{ snapDetail.signature }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ fmtTime(snapDetail.created_at) }}</el-descriptions-item>
        </el-descriptions>
        <div class="mt16" style="display:flex;gap:12px;justify-content:center">
          <el-button @click="verifySnapshotAnchor(snapDetail)" :loading="anchorLoading" type="primary">
            <el-icon><CircleCheck /></el-icon>&nbsp;核对锚点
          </el-button>
          <el-button @click="copyToClipboard(JSON.stringify(snapDetail, null, 2))">复制快照信息</el-button>
        </div>
        <div v-if="anchorResult !== null" class="mt16" :class="anchorResult ? 'anchor-match' : 'anchor-mismatch'">
          <el-icon :size="20"><CircleCheck v-if="anchorResult" /><WarningFilled v-else /></el-icon>
          <span>{{ anchorResult ? '锚点匹配：链尾哈希与快照一致' : '锚点不匹配：数据可能已被篡改' }}</span>
        </div>
      </div>
    </el-dialog>

    <!-- 公钥弹窗 -->
    <el-dialog v-model="pubKeyDialog" title="账本公钥" width="700px">
      <div class="pubkey-container">
        <p style="color:var(--text-muted);margin-bottom:12px;font-size:13px">
          使用此公钥可验证账本条目的 ECDSA P256 签名，确保数据未被篡改。
        </p>
        <pre class="pubkey-block">{{ publicKey || '加载中...' }}</pre>
        <el-button @click="copyToClipboard(publicKey)" size="small" class="mt16">复制公钥</el-button>
      </div>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, watch, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { CircleCheck, WarningFilled, CircleCloseFilled, Camera, Key, Download, Link, Refresh, View } from '@element-plus/icons-vue'
import { api } from '../api'

const props = defineProps({ refreshKey: Number })

const list = ref([])
const total = ref(0)
const loading = ref(false)
const dialog = ref(false)
const current = ref(null)
const snapshots = ref([])
const snapLoading = ref(false)
const verifyLoading = ref(false)
const verifyDialog = ref(false)
const verifyResult = ref(null)
const lastVerify = ref(null)
const activeTab = ref('entries')
const snapDetailDialog = ref(false)
const snapDetail = ref(null)
const anchorLoading = ref(false)
const anchorResult = ref(null)
const pubKeyDialog = ref(false)
const publicKey = ref('')
const exportLoading = ref(false)
const snapshotLoading = ref(false)

async function loadEntries() {
  loading.value = true
  try { const data = await api.ledger({ limit: 30 }); list.value = data.list; total.value = data.total }
  catch (e) { ElMessage.error('加载失败: ' + e.message) }
  finally { loading.value = false }
}

async function loadSnapshots() {
  snapLoading.value = true
  try { const data = await api.ledgerSnapshots({ limit: 30 }); snapshots.value = data || [] }
  catch (e) { ElMessage.error('加载快照失败: ' + e.message) }
  finally { snapLoading.value = false }
}

async function verifyLedger() {
  verifyLoading.value = true; verifyDialog.value = true; verifyResult.value = null
  try {
    const data = await api.ledgerVerify(); verifyResult.value = data; lastVerify.value = data
  } catch (e) {
    verifyResult.value = { valid: false, total_entries: 0, errors: ['请求失败: ' + e.message], snapshot_ok: false, checked_at: new Date().toISOString() }
    ElMessage.error('校验失败: ' + e.message)
  } finally { verifyLoading.value = false }
}

function showSnapshotDetail(sn) { snapDetail.value = sn; snapDetailDialog.value = true; anchorResult.value = null }

async function verifySnapshotAnchor(sn) {
  anchorLoading.value = true
  try {
    const data = await api.ledger({ limit: 1 })
    if (data.list && data.list.length > 0) { anchorResult.value = data.list[0].entry_hash === sn.last_hash }
    else { anchorResult.value = false }
  } catch (e) { ElMessage.error('核对失败: ' + e.message); anchorResult.value = false }
  finally { anchorLoading.value = false }
}

async function showPublicKey() {
  pubKeyDialog.value = true
  try { const data = await api.ledgerPublicKey(); publicKey.value = data.public_key || '未获取到公钥' }
  catch (e) { publicKey.value = '获取公钥失败: ' + e.message }
}

async function exportLedger() {
  exportLoading.value = true
  try { await api.ledgerExport(); ElMessage.success('账本已导出') }
  catch (e) { ElMessage.error('导出失败: ' + e.message) }
  finally { exportLoading.value = false }
}

function goToOrder(eventId) {
  window.location.hash = '#/orders?keyword=' + eventId; window.location.reload()
}

function copyToClipboard(text) { navigator.clipboard.writeText(text).then(() => ElMessage.success('已复制')).catch(() => ElMessage.error('复制失败')) }

async function createSnapshot() {
  snapshotLoading.value = true
  try { await api.createSnapshot(); ElMessage.success('快照创建成功'); activeTab.value = 'snapshots'; await loadSnapshots() }
  catch (e) { ElMessage.error('创建快照失败: ' + e.message) }
  finally { snapshotLoading.value = false }
}

function showDetail(row) { current.value = row; dialog.value = true }
watch(() => props.refreshKey, () => { loadEntries(); loadSnapshots() })
function fmtTime(t) { if (!t) return '—'; return new Date(t).toLocaleString('zh-CN', { hour12: false }) }
onMounted(() => { loadEntries(); loadSnapshots() })
</script>

<style scoped>
.ledger-page { max-width: 1000px; margin: 0 auto; }
.mb16 { margin-bottom: 16px; }
.mt12 { margin-top: 12px; }
.mt16 { margin-top: 16px; }
.section-label { font-weight: 600; font-size: 14px; color: var(--text-primary); }

/* 操作栏 */
.action-bar {
  display: flex; align-items: center; gap: 10px;
  padding: 14px 18px; flex-wrap: wrap;
}
.verify-tag { margin-left: auto; }
.verify-tag .tag-muted { opacity: 0.6; margin-left: 4px; font-size: 11px; }

/* 链条 */
.chain { display: flex; flex-direction: column; align-items: center; padding: 8px 0; }
.chain-node {
  width: 100%; display: flex; align-items: center; gap: 14px;
  padding: 12px 16px; background: var(--bg-subtle);
  border-radius: 8px; border: 1px solid var(--border);
}
.chain-node.head {
  border-color: var(--success);
  box-shadow: 0 0 0 1px var(--success), 0 4px 12px -4px rgba(0, 135, 90, 0.2);
}
.chain-badge {
  width: 36px; height: 36px; border-radius: 50%;
  background: var(--accent-gradient); color: #fff;
  display: flex; align-items: center; justify-content: center;
  font-weight: 700; font-size: 12px; flex-shrink: 0;
}
.chain-node.head .chain-badge { background: var(--success); }
.chain-body { flex: 1; min-width: 0; }
.chain-row { display: flex; gap: 10px; font-size: 12px; padding: 2px 0; align-items: center; }
.chain-label { color: var(--text-muted); width: 66px; flex-shrink: 0; font-weight: 500; }
.chain-link { color: var(--success); padding: 3px 0; display: flex; justify-content: center; }

/* 快照 */
.snapshot-grid { display: grid; gap: 10px; }
.snapshot-card {
  display: flex; align-items: flex-start; gap: 14px;
  padding: 14px; background: var(--bg-subtle);
  border-radius: 8px; border: 1px solid var(--border);
  position: relative;
}
.snapshot-card:hover { border-color: var(--accent); }
.snapshot-date {
  width: 72px; height: 56px; border-radius: 8px;
  background: var(--accent-gradient); color: #fff;
  display: flex; align-items: center; justify-content: center;
  font-weight: 700; font-size: 13px; flex-shrink: 0;
  text-align: center; line-height: 1.3;
}
.snapshot-body { flex: 1; min-width: 0; }
.sn-row { display: flex; gap: 10px; font-size: 12px; padding: 2px 0; align-items: center; }
.sn-label { color: var(--text-muted); width: 66px; flex-shrink: 0; font-weight: 500; }
.detail-icon { position: absolute; right: 14px; top: 50%; transform: translateY(-50%); opacity: 0.5; }

/* 校验 */
.verify-result { text-align: center; }
.verify-status {
  display: flex; flex-direction: column; align-items: center; gap: 8px;
  padding: 20px; border-radius: 10px;
}
.verify-status.pass { background: var(--success-bg); color: var(--success); }
.verify-status.fail { background: var(--danger-bg); color: var(--danger); }
.verify-status-text { font-size: 18px; font-weight: 590; }
.verify-errors {
  text-align: left;
  background: var(--danger-bg);
  border: 1px solid var(--danger-border);
  border-radius: 8px; padding: 14px;
}
.verify-errors-title { font-weight: 590; font-size: 13px; margin-bottom: 8px; color: var(--danger); }
.verify-error-item { display: flex; align-items: center; gap: 8px; font-size: 12px; padding: 4px 0; color: var(--danger); }
.verify-loading { min-height: 80px; display: flex; align-items: center; justify-content: center; }

/* 公钥 */
.pubkey-container { padding: 8px 0; }
.pubkey-block {
  background: #1a1e2e; color: #c9d1d9; padding: 14px; border-radius: 8px;
  font-family: 'JetBrains Mono', Consolas, monospace; font-size: 12px; line-height: 1.6;
  white-space: pre-wrap; word-break: break-all; max-height: 280px; overflow-y: auto;
}

/* 事件链接 */
.event-link { color: var(--accent); cursor: pointer; text-decoration: underline; font-weight: 600; }
.event-link:hover { color: var(--accent-hover); }
.snap-detail { padding: 8px 0; }
.anchor-match { display: flex; align-items: center; gap: 8px; padding: 12px 14px; background: var(--success-bg); border-radius: 8px; color: var(--success); font-weight: 600; font-size: 13px; }
.anchor-mismatch { display: flex; align-items: center; gap: 8px; padding: 12px 14px; background: var(--danger-bg); border-radius: 8px; color: var(--danger); font-weight: 600; font-size: 13px; }

.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
.hash { font-size: 11px; word-break: break-all; }
.muted { color: var(--text-muted); }
.font-muted { color: var(--text-muted); font-size: 12px; }
</style>