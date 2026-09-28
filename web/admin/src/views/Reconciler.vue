<template>
  <div class="reconciler-page">
    <el-tabs v-model="tab">
      <el-tab-pane label="异常订单池" name="exceptions">
        <div class="filter-bar">
          <el-select v-model="filters.phase" placeholder="阶段" clearable style="width:120px" @change="load">
            <el-option label="待处理" value="pending" />
            <el-option label="处理中" value="processing" />
            <el-option label="已处理" value="resolved" />
          </el-select>
          <el-select v-model="filters.reason" placeholder="原因" clearable style="width:140px" @change="load">
            <el-option label="查单失败" value="query_failed" />
            <el-option label="金额不一致" value="amount_mismatch" />
            <el-option label="状态矛盾" value="state_conflict" />
            <el-option label="关单存疑" value="close_uncertain" />
            <el-option label="通知dead" value="notify_dead" />
            <el-option label="手动加入" value="manual" />
          </el-select>
          <el-button type="primary" @click="showAddDialog">手动加入</el-button>
          <span class="text-muted" style="margin-left:auto;font-size:12px">共 {{ total }} 条</span>
        </div>

        <el-table v-loading="loading" :data="list" size="small" class="mt10">
          <el-table-column prop="out_trade_no" label="订单号" min-width="160" show-overflow-tooltip />
          <el-table-column prop="upstream_name" label="上游" width="120" />
          <el-table-column prop="reason" label="原因" width="110">
            <template #default="{ row }">
              <el-tag :type="reasonType(row.reason)" size="small">{{ reasonText(row.reason) }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="channel_status" label="通道状态" width="110" />
          <el-table-column label="阶段" width="90">
            <template #default="{ row }">
              <el-tag :type="row.phase === 'resolved' ? 'success' : row.phase === 'processing' ? 'warning' : 'danger'" size="small" effect="light">
                {{ row.phase === 'pending' ? '待处理' : row.phase === 'processing' ? '处理中' : '已处理' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="金额" width="90">
            <template #default="{ row }"><b>¥{{ (row.amount_minor / 100).toFixed(2) }}</b></template>
          </el-table-column>
          <el-table-column prop="created_at" label="创建时间" width="160" />
          <el-table-column label="" width="140" fixed="right">
            <template #default="{ row }">
              <el-button size="small" text type="primary" @click.stop="showDetail(row)">详情</el-button>
              <el-button v-if="row.phase !== 'resolved'" size="small" text type="primary" @click.stop="showResolve(row)">处理</el-button>
            </template>
          </el-table-column>
        </el-table>
        <div class="pager" v-if="total > pageSize">
          <el-pagination background layout="prev,pager,next" :total="total" :page-size="pageSize" v-model:current-page="page" @current-change="load" />
        </div>
      </el-tab-pane>

      <el-tab-pane label="Dead 通知" name="dead">
        <el-table v-loading="deadLoading" :data="deadList" size="small" class="mt10">
          <el-table-column prop="id" label="任务ID" width="80" />
          <el-table-column prop="out_trade_no" label="商户订单号" min-width="160" show-overflow-tooltip />
          <el-table-column prop="retry_count" label="重试次数" width="90" />
          <el-table-column prop="created_at" label="创建时间" width="160" />
          <el-table-column label="" width="150">
            <template #default="{ row }">
              <el-button size="small" type="primary" @click="retryDead(row.id)">重发</el-button>
              <el-button size="small" type="danger" @click="deleteDead(row.id)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <el-tab-pane label="日终对账" name="daily">
        <div class="daily-report">
          <div class="report-head">
            <el-date-picker v-model="reportDate" type="date" value-format="YYYY-MM-DD" size="small" placeholder="选择日期" />
            <el-button size="small" type="primary" :loading="reportLoading" @click="loadReport">生成报告</el-button>
            <el-tag v-if="report" :type="report.conclusion.startsWith('通过') ? 'success' : 'warning'" size="small">
              {{ report.conclusion }}
            </el-tag>
          </div>
          <div v-if="report" v-loading="reportLoading" class="mt16">
            <div class="report-stats">
              <div class="stat-box pg-card">
                <div class="stat-val">{{ report.orders_total }}</div>
                <div class="stat-label">当日订单</div>
              </div>
              <div class="stat-box pg-card">
                <div class="stat-val text-success">{{ report.orders_success }}</div>
                <div class="stat-label">成功订单</div>
              </div>
              <div class="stat-box pg-card">
                <div class="stat-val text-warning">¥{{ (report.success_amount / 100).toFixed(2) }}</div>
                <div class="stat-label">成功金额</div>
              </div>
              <div class="stat-box pg-card">
                <div class="stat-val" :class="{ 'text-danger': report.notify_dead > 0 }">{{ report.notify_dead }}</div>
                <div class="stat-label">回调 Dead</div>
              </div>
              <div class="stat-box pg-card">
                <div class="stat-val" :class="{ 'text-danger': report.exceptions_pending > 0 }">{{ report.exceptions_pending }}</div>
                <div class="stat-label">异常待处理</div>
              </div>
            </div>
            <el-table :data="report.by_channel" stripe size="small" class="mt16">
              <el-table-column prop="channel" label="通道" min-width="160" />
              <el-table-column prop="total_orders" label="订单" width="80" />
              <el-table-column prop="success_orders" label="成功" width="80" />
              <el-table-column label="成功金额" width="110">
                <template #default="{ row }">¥{{ (row.amount_minor / 100).toFixed(2) }}</template>
              </el-table-column>
              <el-table-column prop="closed_orders" label="关闭" width="80" />
              <el-table-column label="关闭金额" width="110">
                <template #default="{ row }">¥{{ (row.amount_closed / 100).toFixed(2) }}</template>
              </el-table-column>
            </el-table>
          </div>
          <div v-else class="empty-text">选择日期生成日终对账报告</div>
        </div>
      </el-tab-pane>
    </el-tabs>

    <!-- 异常单详情弹窗 -->
    <el-dialog v-model="detailVisible" title="异常单详情" width="700px">
      <template v-if="detail">
        <el-descriptions :column="2" border size="small">
          <el-descriptions-item label="订单号" :span="2"><span class="font-mono">{{ detail.out_trade_no }}</span></el-descriptions-item>
          <el-descriptions-item label="上游">{{ detail.upstream_name || '—' }}</el-descriptions-item>
          <el-descriptions-item label="通道">{{ detail.channel }}</el-descriptions-item>
          <el-descriptions-item label="金额">¥{{ (detail.amount_minor / 100).toFixed(2) }}</el-descriptions-item>
          <el-descriptions-item label="通道状态">{{ detail.channel_status }}</el-descriptions-item>
          <el-descriptions-item label="交易号"><span class="font-mono">{{ detail.channel_tx_id || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="原因" :span="2">{{ reasonText(detail.reason) }}</el-descriptions-item>
          <el-descriptions-item label="建议" :span="2">{{ detail.suggestion || '—' }}</el-descriptions-item>
          <el-descriptions-item label="处理人">{{ detail.handler || '—' }}</el-descriptions-item>
          <el-descriptions-item label="处理动作">{{ detail.resolved_action || '—' }}</el-descriptions-item>
          <el-descriptions-item label="备注" :span="2">{{ detail.handler_note || '—' }}</el-descriptions-item>
        </el-descriptions>
        <h4 class="section-title">证据时间线</h4>
        <pre class="evidence-box">{{ detail.evidence || '（无）' }}</pre>
        <h4 class="section-title">操作日志</h4>
        <el-timeline v-if="logs && logs.length">
          <el-timeline-item v-for="l in logs" :key="l.id" :timestamp="l.created_at" placement="top">
            <p><b>{{ l.action }}</b> — {{ l.operator }}</p>
            <p v-if="l.detail" class="text-muted" style="font-size:12px">{{ l.detail }}</p>
          </el-timeline-item>
        </el-timeline>
        <p v-else class="text-muted">无操作记录</p>
      </template>
      <template #footer>
        <el-button @click="detailVisible = false">关闭</el-button>
        <el-button v-if="detail && detail.phase !== 'resolved'" type="primary" @click="detailVisible = false; showResolve(detail)">处理</el-button>
        <el-button v-if="detail && detail.phase === 'resolved'" text @click="reopen(detail.id)">重新打开</el-button>
        <el-button text @click="requery(detail?.id)">再次查单</el-button>
      </template>
    </el-dialog>

    <!-- 处理弹窗 -->
    <el-dialog v-model="resolveVisible" title="人工处理" width="500px">
      <el-form :model="resolveForm" label-width="100px">
        <el-form-item label="动作">
          <el-radio-group v-model="resolveForm.action">
            <el-radio value="settle">确认入账</el-radio>
            <el-radio value="close">确认未支付，关单</el-radio>
            <el-radio value="noop">无需处理</el-radio>
          </el-radio-group>
        </el-form-item>
        <el-form-item v-if="resolveForm.action === 'settle'" label="实际金额">
          <el-input-number v-model="resolveForm.paid_amount" :min="1" :precision="2" />
          <span class="text-muted" style="margin-left:8px">元</span>
        </el-form-item>
        <el-form-item v-if="resolveForm.action === 'settle'" label="通道交易号">
          <el-input v-model="resolveForm.channel_tx_id" placeholder="从支付宝账单核对后填入" />
        </el-form-item>
        <el-form-item v-if="resolveForm.action === 'settle'" label="付款方">
          <el-input v-model="resolveForm.payer" placeholder="可选" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="resolveForm.note" type="textarea" :rows="2" placeholder="处理说明" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="resolveVisible = false">取消</el-button>
        <el-button type="primary" @click="doResolve" :loading="resolving">确认处理</el-button>
      </template>
    </el-dialog>

    <!-- 手动加入弹窗 -->
    <el-dialog v-model="addVisible" title="手动加入异常池" width="400px">
      <el-form :model="addForm" label-width="100px">
        <el-form-item label="订单号">
          <el-input v-model="addForm.out_trade_no" placeholder="输入订单号" />
        </el-form-item>
        <el-form-item label="说明">
          <el-input v-model="addForm.note" type="textarea" :rows="2" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="addVisible = false">取消</el-button>
        <el-button type="primary" @click="doAdd">加入</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '../api'

const tab = ref('exceptions')
const loading = ref(false)
const list = ref([])
const total = ref(0)
const page = ref(1)
const pageSize = 20
const filters = ref({ phase: 'pending', reason: '' })
const detailVisible = ref(false)
const detail = ref(null)
const logs = ref([])
const resolveVisible = ref(false)
const resolveForm = ref({ action: 'settle', paid_amount: 0, channel_tx_id: '', payer: '', note: '' })
const resolving = ref(false)
const currentException = ref(null)
const addVisible = ref(false)
const addForm = ref({ out_trade_no: '', note: '' })
const deadLoading = ref(false)
const deadList = ref([])
const reportDate = ref(new Date().toISOString().slice(0, 10))
const report = ref(null)
const reportLoading = ref(false)

async function loadReport() {
  reportLoading.value = true
  try { report.value = await api.dailyReport({ date: reportDate.value }) }
  catch (e) { ElMessage.error('生成报告失败：' + e.message) }
  finally { reportLoading.value = false }
}

function reasonType(r) { return { query_failed: 'danger', amount_mismatch: 'warning', state_conflict: 'warning', close_uncertain: 'danger', notify_dead: 'info', manual: '' }[r] || '' }
function reasonText(r) { return { query_failed: '查单失败', amount_mismatch: '金额不一致', state_conflict: '状态矛盾', close_uncertain: '关单存疑', notify_dead: '通知dead', manual: '手动加入' }[r] || r }

async function load() {
  loading.value = true
  try {
    const res = await api.listExceptions({ phase: filters.value.phase, reason: filters.value.reason, page: page.value, page_size: pageSize })
    list.value = res.list || []; total.value = res.total || 0
  } catch (e) { ElMessage.error('加载失败: ' + e.message) }
  finally { loading.value = false }
}

async function showDetail(row) {
  try {
    const res = await api.getException(row.id)
    detail.value = res.exception; logs.value = res.logs || []; detailVisible.value = true
  } catch (e) { ElMessage.error('加载详情失败: ' + e.message) }
}

async function requery(id) {
  try { await api.requeryException(id); ElMessage.success('查单完成'); if (detail.value?.id === id) showDetail({ id }) }
  catch (e) { ElMessage.error('查单失败: ' + e.message) }
}

function showResolve(row) {
  currentException.value = row
  resolveForm.value = { action: 'settle', paid_amount: row.amount_minor / 100, channel_tx_id: row.channel_tx_id || '', payer: '', note: '' }
  resolveVisible.value = true
}

async function doResolve() {
  resolving.value = true
  try {
    const data = { action: resolveForm.value.action, paid_amount: Math.round(resolveForm.value.paid_amount * 100), channel_tx_id: resolveForm.value.channel_tx_id, payer: resolveForm.value.payer, note: resolveForm.value.note }
    await api.resolveException(currentException.value.id, data)
    ElMessage.success('处理成功'); resolveVisible.value = false; load()
  } catch (e) { ElMessage.error('处理失败: ' + e.message) }
  finally { resolving.value = false }
}

async function reopen(id) { try { await api.reopenException(id); ElMessage.success('已重新打开'); detailVisible.value = false; load() } catch (e) { ElMessage.error('操作失败: ' + e.message) } }
function showAddDialog() { addForm.value = { out_trade_no: '', note: '' }; addVisible.value = true }
async function doAdd() { try { await api.addException(addForm.value); ElMessage.success('已加入异常池'); addVisible.value = false; load() } catch (e) { ElMessage.error('操作失败: ' + e.message) } }
async function loadDead() {
  deadLoading.value = true
  try {
    const res = await api.listDeadNotify()
    deadList.value = (res.list || []).map(t => {
      let o = ''
      try { if (t.payload) o = JSON.parse(t.payload).out_trade_no || '' } catch {}
      return { ...t, out_trade_no: o }
    })
  } catch (e) { ElMessage.error('加载失败: ' + e.message) }
  finally { deadLoading.value = false }
}
async function retryDead(id) {
  try { await api.retryDeadNotify(id); ElMessage.success('已按最新回调地址重新入队'); loadDead() }
  catch (e) { ElMessage.error('重发失败: ' + e.message) }
}
async function deleteDead(id) {
  try {
    await ElMessageBox.confirm('彻底删除该死信通知任务？若该订单已支付但未通知到商户，删除后将无法自动补发。', '确认删除', { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' })
  } catch { return }
  try { await api.deleteDeadNotify(id); ElMessage.success('已删除'); loadDead() }
  catch (e) { ElMessage.error('删除失败: ' + e.message) }
}

onMounted(() => { load(); loadReport() })
</script>

<style scoped>
.reconciler-page { max-width: 1400px; margin: 0 auto; }
.filter-bar { display: flex; gap: 8px; flex-wrap: wrap; padding: 14px 16px 0; }
.pager { display: flex; justify-content: flex-end; padding: 0 16px 16px; }
.mt10 { margin-top: 10px; }
.mt16 { margin-top: 16px; }
.section-title { margin: 18px 0 10px; font-size: 14px; font-weight: 590; color: var(--text-primary); }
.evidence-box {
  background: var(--bg-subtle); padding: 12px; border-radius: 6px;
  font-size: 12px; white-space: pre-wrap; max-height: 200px; overflow-y: auto;
  border: 1px solid var(--border); margin: 0;
}
.report-head { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; }
.report-stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 10px; }
.stat-box { padding: 16px; text-align: center; }
.stat-val { font-size: 20px; font-weight: 590; color: var(--text-primary); font-family: 'JetBrains Mono', monospace; font-feature-settings: 'tnum'; }
.stat-label { font-size: 11px; color: var(--text-muted); margin-top: 4px; font-weight: 500; }
.empty-text { text-align: center; padding: 40px 0; color: var(--text-muted); font-size: 13px; }
.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
.text-muted { color: var(--text-muted); }
.text-success { color: var(--success); }
.text-warning { color: var(--warning); }
.text-danger { color: var(--danger); }
</style>