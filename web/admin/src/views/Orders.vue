<template>
  <div class="orders-page">
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="section-label">订单管理</span>
        <span class="text-muted" style="font-size: 12px">共 {{ total }} 笔</span>
      </div>

      <!-- 筛选栏 -->
      <div class="filter-bar">
        <el-input v-model="filters.keyword" placeholder="搜索订单号 / 网关单号 / 交易号 / 商品" clearable
          style="width: 260px" @keyup.enter="reload" @clear="reload">
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-select v-model="filters.status" placeholder="全部状态" clearable style="width: 130px" @change="reload">
          <el-option label="待支付" value="WAIT_BUYER_PAY" />
          <el-option label="成功" value="TRADE_SUCCESS" />
          <el-option label="已关闭" value="TRADE_CLOSED" />
          <el-option label="失败" value="FAILED" />
        </el-select>
        <el-select v-model="filters.channel" placeholder="全部通道" clearable style="width: 120px" @change="reload">
          <el-option label="支付宝" value="alipay" />
        </el-select>
        <el-date-picker v-model="dateRange" type="daterange" range-separator="至"
          start-placeholder="开始日期" end-placeholder="结束日期" style="width: 260px"
          value-format="YYYY-MM-DD" @change="reload" />
        <el-button type="primary" @click="reload">
          <el-icon><Search /></el-icon>&nbsp;查询
        </el-button>
        <el-button @click="reset">重置</el-button>
      </div>

      <!-- 表格 -->
      <el-table v-loading="loading" :data="list" size="small" @row-click="showDetail" class="mt10">
        <el-table-column prop="out_trade_no" label="商户订单号" min-width="150" show-overflow-tooltip />
        <el-table-column prop="gateway_no" label="网关单号" min-width="150" show-overflow-tooltip />
        <el-table-column label="上游" width="130">
          <template #default="{ row }">
            <el-tag v-if="row.upstream_name" size="small" type="primary" effect="light">{{ row.upstream_name }}</el-tag>
            <span v-else class="text-muted">—</span>
          </template>
        </el-table-column>
        <el-table-column prop="channel" label="通道" width="80" />
        <el-table-column label="账户" width="110">
          <template #default="{ row }">
            <span v-if="row.account_name">{{ row.account_name }}</span>
            <span v-else-if="row.account_id" class="text-muted">#{{ row.account_id }}</span>
            <span v-else class="text-muted">—</span>
          </template>
        </el-table-column>
        <el-table-column prop="subject" label="商品" min-width="100" show-overflow-tooltip />
        <el-table-column label="金额" width="95">
          <template #default="{ row }"><b>¥{{ (row.amount_minor / 100).toFixed(2) }}</b></template>
        </el-table-column>
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag :type="statusType(row.status)" size="small" effect="light" round>{{ statusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="channel_tx_id" label="通道交易号" min-width="150" show-overflow-tooltip />
        <el-table-column label="支付时间" width="160">
          <template #default="{ row }">{{ fmtTime(row.paid_at) }}</template>
        </el-table-column>
        <el-table-column label="创建时间" width="160">
          <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
        </el-table-column>
        <el-table-column label="" width="70" fixed="right">
          <template #default="{ row }">
            <el-button size="small" text type="primary" @click.stop="showDetail(row)">详情</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div class="pager mt16">
        <el-pagination background layout="total, prev, pager, next, jumper" :total="total"
          :page-size="filters.limit" :current-page="page" @current-change="onPage" />
      </div>
    </div>

    <!-- 详情抽屉 -->
    <el-drawer v-model="drawer" :title="'订单详情'" size="500px" class="order-drawer">
      <template v-if="current">
        <div class="detail-head">
          <div class="detail-amount">¥{{ (current.amount_minor / 100).toFixed(2) }}</div>
          <el-tag :type="statusType(current.status)" size="small" effect="dark">{{ statusText(current.status) }}</el-tag>
        </div>
        <el-descriptions :column="1" border size="small" class="mt16">
          <el-descriptions-item label="商户订单号"><span class="font-mono">{{ current.out_trade_no }}</span></el-descriptions-item>
          <el-descriptions-item label="网关单号"><span class="font-mono">{{ current.gateway_no }}</span></el-descriptions-item>
          <el-descriptions-item label="通道">{{ current.channel }}</el-descriptions-item>
          <el-descriptions-item label="处理账户">
            <span v-if="current.account_name">{{ current.account_name }}</span>
            <span v-else-if="current.account_id" class="text-muted">#{{ current.account_id }}</span>
            <span v-else class="text-muted">—</span>
          </el-descriptions-item>
          <el-descriptions-item label="通道交易号"><span class="font-mono">{{ current.channel_tx_id || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="上游">
            <span>{{ current.upstream_name || '—' }}</span>
            <span v-if="current.pid" class="text-muted" style="font-size: 11px; margin-left: 6px">(pid: {{ current.pid }})</span>
          </el-descriptions-item>
          <el-descriptions-item label="上游请求ID">
            <span class="font-mono" style="font-size: 12px">{{ current.attach || '—' }}</span>
            <el-button v-if="current.attach" size="small" text type="primary" @click="copyAttach(current.attach)">复制</el-button>
          </el-descriptions-item>
          <el-descriptions-item label="商品">{{ current.subject }}</el-descriptions-item>
          <el-descriptions-item label="付款方">{{ current.payer || '—' }}</el-descriptions-item>
          <el-descriptions-item label="回调地址"><span class="font-mono" style="font-size: 11px">{{ current.notify_url || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="通知状态">{{ current.notified ? `已通知 ${current.notify_count} 次` : '未通知' }}</el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ fmtTime(current.created_at) }}</el-descriptions-item>
          <el-descriptions-item label="支付时间">{{ fmtTime(current.paid_at) }}</el-descriptions-item>
        </el-descriptions>

        <h4 class="section-title">资金流水</h4>
        <div v-loading="eventsLoading">
          <el-timeline v-if="events.length">
            <el-timeline-item v-for="e in events" :key="e.id" :timestamp="fmtTime(e.created_at)"
              :type="eventType(e.event_type)" placement="top">
              <b>{{ eventText(e.event_type) }}</b>
              <div class="font-mono text-muted" style="font-size: 11px">{{ e.channel_tx_id || '' }}</div>
              <div class="text-muted" style="font-size: 12px">金额 ¥{{ (e.amount_minor / 100).toFixed(2) }}</div>
            </el-timeline-item>
          </el-timeline>
          <div v-else-if="!eventsLoading" class="empty-tip">
            <el-icon :size="28" color="var(--text-muted)"><Document /></el-icon>
            <div class="empty-text">暂无资金流水</div>
            <div class="empty-sub">该订单可能为历史数据，或支付流程未产生事件记录</div>
          </div>
        </div>
      </template>
    </el-drawer>
  </div>
</template>

<script setup>
import { ref, reactive, watch, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '../api'

const props = defineProps({ refreshKey: Number })

const list = ref([])
const total = ref(0)
const loading = ref(false)
const page = ref(1)
const dateRange = ref(null)
const filters = reactive({ status: '', channel: '', keyword: '', limit: 20, offset: 0 })
const drawer = ref(false)
const current = ref(null)
const events = ref([])
const eventsLoading = ref(false)

async function load() {
  loading.value = true
  try {
    const [from, to] = dateRange.value || [null, null]
    const data = await api.orders({
      status: filters.status, channel: filters.channel, keyword: filters.keyword,
      from, to, limit: filters.limit, offset: filters.offset
    })
    list.value = data.list
    total.value = data.total
  } catch (e) {
    ElMessage.error('加载失败: ' + e.message)
  } finally {
    loading.value = false
  }
}

function reload() { page.value = 1; filters.offset = 0; load() }
function reset() {
  filters.status = ''; filters.channel = ''; filters.keyword = ''
  dateRange.value = null; reload()
}
function onPage(p) { page.value = p; filters.offset = (p - 1) * filters.limit; load() }
watch(() => props.refreshKey, load)

async function showDetail(row) {
  current.value = row; drawer.value = true; events.value = []; eventsLoading.value = true
  try {
    const data = await api.orderEvents(row.id, 20)
    events.value = data || []
  } catch (e) {
    ElMessage.warning('流水加载失败: ' + e.message)
    events.value = []
  } finally {
    eventsLoading.value = false
  }
}

function statusText(s) {
  return { WAIT_BUYER_PAY: '待支付', TRADE_SUCCESS: '成功', TRADE_CLOSED: '已关闭', CONFIRMING: '确认中', FAILED: '失败' }[s] || s
}
function statusType(s) {
  return { WAIT_BUYER_PAY: 'warning', TRADE_SUCCESS: 'success', TRADE_CLOSED: 'info', CONFIRMING: 'warning', FAILED: 'danger' }[s] || 'info'
}
function eventText(e) {
  return { created: '订单创建', paid: '支付成功', closed: '订单关闭', notified: '通知商户', failed: '失败' }[e] || e
}
function eventType(e) {
  return { created: 'info', paid: 'success', closed: 'info', notified: 'primary', failed: 'danger' }[e] || 'info'
}
function fmtTime(t) { if (!t) return '—'; return new Date(t).toLocaleString('zh-CN', { hour12: false }) }

onMounted(() => {
  const hash = window.location.hash
  if (hash.includes('keyword=')) {
    const m = hash.match(/keyword=([^&]*)/)
    if (m && m[1]) filters.keyword = decodeURIComponent(m[1])
  }
  load()
})

async function copyAttach(attach) {
  try { await navigator.clipboard.writeText(attach); ElMessage.success('已复制') }
  catch (e) { ElMessage.error('复制失败: ' + e.message) }
}
</script>

<style scoped>
.orders-page { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 16px; }

.section-label { font-weight: 590; font-size: 13px; color: var(--text-primary); }

.filter-bar { display: flex; gap: 8px; flex-wrap: wrap; padding: 14px 16px 0; }
.pager { display: flex; justify-content: flex-end; padding: 0 20px 16px; }
.mt10 { margin-top: 10px; }
.mt16 { margin-top: 16px; }

.detail-head { display: flex; align-items: center; gap: 12px; margin-bottom: 20px; }
.detail-amount { font-size: 26px; font-weight: 700; color: var(--text-primary); }
.section-title { margin: 20px 0 12px; font-size: 14px; font-weight: 600; color: var(--text-primary); }

.empty-tip {
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  padding: 32px 0; gap: 6px;
}
.empty-text { font-size: 13px; color: var(--text-muted); }
.empty-sub { font-size: 12px; color: var(--text-muted); }

.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
.text-muted { color: var(--text-muted); }
</style>