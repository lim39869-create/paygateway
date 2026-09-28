<template>
  <div class="dash" v-loading="loading">
    <!-- KPI 卡片 -->
    <div class="kpi-grid">
      <div v-for="k in kpis" :key="k.label" class="kpi-card pg-card">
        <div class="kpi-label">{{ k.label }}</div>
        <div class="kpi-value font-mono">{{ k.value }}</div>
        <div class="kpi-delta">{{ k.delta }}</div>
      </div>
    </div>

    <!-- 图表行 1：订单趋势 + 通道占比 -->
    <div class="chart-row">
      <div class="pg-card chart-card wide">
        <div class="pg-card-header">
          <span class="chart-title">订单趋势</span>
          <div class="trend-controls">
            <el-radio-group v-model="days" size="small" @change="onQuickRange">
              <el-radio-button :value="7">7天</el-radio-button>
              <el-radio-button :value="14">14天</el-radio-button>
              <el-radio-button :value="30">30天</el-radio-button>
            </el-radio-group>
            <el-date-picker v-model="range" type="daterange" range-separator="至"
              start-placeholder="开始日期" end-placeholder="结束日期"
              value-format="YYYY-MM-DD" size="small" :clearable="false"
              :disabled-date="d => d.getTime() > Date.now()"
              style="width: 230px" @change="onRangePick" />
          </div>
        </div>
        <div ref="trendRef" class="chart-box"></div>
      </div>

      <div class="pg-card chart-card">
        <div class="pg-card-header">
          <span class="chart-title">通道占比</span>
        </div>
        <div ref="channelPieRef" class="chart-box"></div>
      </div>
    </div>

    <!-- 图表行 2：金额柱状 + 状态分布 -->
    <div class="chart-row">
      <div class="pg-card chart-card wide">
        <div class="pg-card-header"><span class="chart-title">交易金额</span></div>
        <div ref="amountRef" class="chart-box"></div>
      </div>

      <div class="pg-card chart-card">
        <div class="pg-card-header"><span class="chart-title">订单状态</span></div>
        <div ref="statusRef" class="chart-box"></div>
      </div>
    </div>

    <!-- 系统状态行 -->
    <div class="status-row">
      <div class="pg-card status-card">
        <div class="pg-card-header">
          <span class="chart-title">公网隧道</span>
          <div class="tunnel-badge" :class="{ ok: tunnelHealthy }">
            <span class="dot"></span>
            {{ tunnelStatusText }}
          </div>
        </div>
        <div class="pg-card-body">
          <div class="tunnel-info">
            <el-icon :size="18" class="tunnel-icon"><Connection /></el-icon>
            <div class="tunnel-details">
              <div class="tunnel-url font-mono">{{ tunnel?.url || '未连接' }}</div>
              <div class="tunnel-cb text-muted" v-if="tunnel?.callback">{{ tunnel.callback }}</div>
            </div>
          </div>
        </div>
      </div>

      <div class="pg-card status-card">
        <div class="pg-card-header">
          <span class="chart-title">回调队列</span>
        </div>
        <div class="pg-card-body">
          <div class="queue-grid">
            <div class="queue-item">
              <div class="queue-num font-mono text-success">{{ d.notify_success ?? 0 }}</div>
              <div class="queue-label">成功</div>
            </div>
            <div class="queue-item">
              <div class="queue-num font-mono text-warning">{{ d.notify_pending ?? 0 }}</div>
              <div class="queue-label">待重试</div>
            </div>
            <div class="queue-item">
              <div class="queue-num font-mono text-danger">{{ d.notify_dead ?? 0 }}</div>
              <div class="queue-label">死信</div>
            </div>
            <div class="queue-item">
              <div class="queue-num font-mono">{{ d.ledger_count ?? 0 }}</div>
              <div class="queue-label">账本条目</div>
            </div>
          </div>
          <el-progress v-if="notifyTotal > 0" :percentage="notifySuccessRate" :stroke-width="4"
            :color="rateColor" class="mt14" />
          <div class="rate-label text-muted">回调成功率 {{ notifySuccessRate }}%</div>
        </div>
      </div>
    </div>

    <!-- 账本状态 -->
    <div class="pg-card" v-if="ledgerLastHash">
      <div class="pg-card-header">
        <span class="chart-title">账本状态</span>
        <div style="display:flex;align-items:center;gap:8px">
          <el-tag :type="ledgerVerifyResult?.valid ? 'success' : 'warning'" size="small" effect="light">
            {{ ledgerVerifyResult?.valid ? '账本完整' : '未校验' }}
          </el-tag>
          <el-button size="small" @click="verifyLedger" :loading="verifyLoading" plain>
            <el-icon><CircleCheck /></el-icon>&nbsp;校验
          </el-button>
        </div>
      </div>
      <div class="pg-card-body">
        <div class="ledger-grid">
          <div class="ledger-item">
            <div class="ledger-num font-mono">{{ d.ledger_count ?? 0 }}</div>
            <div class="ledger-label">总条目</div>
          </div>
          <div class="ledger-item">
            <div class="ledger-num font-mono text-muted" style="font-size:12px">{{ ledgerLastHashShort }}</div>
            <div class="ledger-label">链尾哈希</div>
          </div>
          <div class="ledger-item">
            <div class="ledger-num text-muted" style="font-size:12px">{{ ledgerLastTime }}</div>
            <div class="ledger-label">最近记录</div>
          </div>
          <div class="ledger-item">
            <div class="ledger-num" :class="ledgerVerifyResult?.snapshot_ok ? 'text-success' : 'text-muted'">
              {{ ledgerVerifyResult?.snapshot_ok ? '匹配' : '—' }}
            </div>
            <div class="ledger-label">快照锚点</div>
          </div>
        </div>
      </div>
    </div>

    <!-- 最近订单 -->
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="chart-title">最近订单</span>
        <button class="link-btn" @click="$emit('goto', 'orders')">
          查看全部
          <el-icon :size="13"><ArrowRight /></el-icon>
        </button>
      </div>
      <el-table :data="d.recent_orders || []" size="small">
        <el-table-column prop="out_trade_no" label="商户订单号" min-width="150" show-overflow-tooltip />
        <el-table-column prop="subject" label="商品" min-width="100" show-overflow-tooltip />
        <el-table-column label="金额" width="100">
          <template #default="{ row }"><span class="font-mono">¥{{ (row.amount_minor / 100).toFixed(2) }}</span></template>
        </el-table-column>
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="statusType(row.status)" size="small" effect="light">{{ statusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="时间" width="160">
          <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
        </el-table-column>
      </el-table>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onBeforeUnmount, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { CircleCheck, ArrowRight } from '@element-plus/icons-vue'
import * as echarts from 'echarts/core'
import { LineChart, BarChart, PieChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
echarts.use([LineChart, BarChart, PieChart, GridComponent, TooltipComponent, LegendComponent, CanvasRenderer])
import { api } from '../api'

const emit = defineEmits(['tunnel', 'goto'])
const props = defineProps({ refreshKey: Number })

const d = ref({})
const loading = ref(false)
const days = ref(14)
const range = ref(null)
const channels = ref([])

const ledgerVerifyResult = ref(null)
const verifyLoading = ref(false)
const ledgerLastHash = computed(() => d.value.ledger_last_hash || '')
const ledgerLastHashShort = computed(() => ledgerLastHash.value ? ledgerLastHash.value.slice(0, 12) + '…' : '—')
const ledgerLastTime = computed(() => {
  if (!d.value.ledger_last_time) return '—'
  return d.value.ledger_last_time.slice(0, 19).replace('T', ' ')
})

async function verifyLedger() {
  verifyLoading.value = true
  try {
    const data = await api.ledgerVerify()
    ledgerVerifyResult.value = data
    if (data.valid) {
      ElMessage.success('账本校验通过，共 ' + data.total_entries + ' 条')
    } else {
      ElMessage.error('账本校验失败，' + (data.errors || []).length + ' 处错误')
    }
  } catch (e) {
    ElMessage.error('校验失败: ' + e.message)
  } finally {
    verifyLoading.value = false
  }
}

const trendRef = ref(null)
const channelPieRef = ref(null)
const amountRef = ref(null)
const statusRef = ref(null)
let charts = []

const kpis = computed(() => {
  const trend = d.value.trend || []
  const todayStr = new Date().toLocaleDateString('en-CA') // YYYY-MM-DD 本地日期
  const today = trend.find(x => x.date === todayStr) || { total_orders: 0, success_orders: 0, amount_minor: 0 }
  const rate = today.total_orders > 0 ? Math.round((today.success_orders / today.total_orders) * 100) : 0
  return [
    { label: '今日订单', value: (today.total_orders ?? 0).toLocaleString(), delta: '成功 ' + (today.success_orders ?? 0) + ' 单' },
    { label: '今日交易额', value: '¥' + ((today.amount_minor ?? 0) / 100).toFixed(2), delta: '成功订单金额' },
    { label: '成功率', value: rate + '%', delta: '今日成功率' },
    { label: '待支付', value: (d.value.wait_orders ?? 0).toLocaleString(), delta: '累计待支付' },
    { label: '累计交易额', value: '¥' + ((d.value.total_amount_minor ?? 0) / 100).toFixed(2), delta: '全部成功订单' }
  ]
})

const tunnel = computed(() => d.value.tunnel || {})
const tunnelHealthy = computed(() => tunnel.value.running && tunnel.value.healthy)
const tunnelStatusText = computed(() => {
  if (!tunnel.value.running) return '未连接'
  return tunnel.value.healthy ? '正常运行' : '异常'
})

const notifyTotal = computed(() => (d.value.notify_success ?? 0) + (d.value.notify_pending ?? 0) + (d.value.notify_dead ?? 0))
const notifySuccessRate = computed(() => notifyTotal.value > 0 ? Math.round(((d.value.notify_success ?? 0) / notifyTotal.value) * 100) : 100)
const rateColor = computed(() => {
  const isDark = document.documentElement.classList.contains('dark')
  if (notifySuccessRate.value >= 99) return isDark ? '#22C55E' : '#16A34A'
  if (notifySuccessRate.value >= 90) return isDark ? '#D97706' : '#B45309'
  return isDark ? '#EF4444' : '#DC2626'
})

// Minimal chart palette — single accent + grays + muted semantic
function chartColors() {
  const isDark = document.documentElement.classList.contains('dark')
  return {
    // Accent (single brand color, consistent across themes)
    accent: '#7170FF',
    // Neutrals
    text: isDark ? '#C4C4C8' : '#3F3F46',
    muted: isDark ? '#52525B' : '#A1A1AA',
    border: isDark ? 'rgba(255,255,255,0.07)' : 'rgba(0,0,0,0.06)',
    // Surfaces
    bg: isDark ? '#18181B' : '#F4F4F5',
    // Semantic (muted, desaturated)
    success: isDark ? '#4ADE80' : '#22A069',
    warning: isDark ? '#FCD34D' : '#C27803',
    danger: isDark ? '#F87171' : '#D92D20',
  }
}

function renderCharts() {
  const c = chartColors()
  const trend = d.value.trend || []
  const dates = trend.map(x => x.date.slice(5))
  const totals = trend.map(x => x.total_orders)
  const succs = trend.map(x => x.success_orders)
  const amounts = trend.map(x => (x.amount_minor / 100).toFixed(2))

  // === 折线图：订单趋势 ===
  // No splitLine, no area gradient, single accent line + muted dashed line
  if (trendRef.value) {
    const chart = echarts.getInstanceByDom(trendRef.value) || echarts.init(trendRef.value)
    chart.setOption({
      tooltip: {
        trigger: 'axis',
        backgroundColor: c.bg,
        borderColor: c.border,
        textStyle: { color: c.text, fontSize: 12 },
        axisPointer: { lineStyle: { color: c.border } }
      },
      legend: { data: ['订单数', '成功'], textStyle: { color: c.muted, fontSize: 11 }, top: 0 },
      grid: { left: 40, right: 16, top: 32, bottom: 10, containLabel: true },
      xAxis: {
        type: 'category', data: dates,
        axisLine: { lineStyle: { color: c.border } },
        axisTick: { show: false },
        axisLabel: { color: c.muted, fontSize: 11 }
      },
      yAxis: {
        type: 'value',
        splitLine: { show: false },
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: { color: c.muted, fontSize: 11 }
      },
      series: [
        {
          name: '订单数',
          type: 'line',
          smooth: true,
          data: totals,
          symbol: 'circle',
          symbolSize: 5,
          lineStyle: { width: 2, color: c.accent },
          itemStyle: { color: c.accent }
        },
        {
          name: '成功',
          type: 'line',
          smooth: true,
          data: succs,
          symbol: 'circle',
          symbolSize: 4,
          lineStyle: { width: 1.5, color: c.muted, type: 'dashed' },
          itemStyle: { color: c.muted }
        }
      ]
    }, true)
    charts.push(chart)
  }

  // === 饼图：通道占比 ===
  // Single accent hue palette — shades of the same color family, not multi-hue
  if (channelPieRef.value) {
    const chart = echarts.getInstanceByDom(channelPieRef.value) || echarts.init(channelPieRef.value)
    const pieData = channels.value.map(ch => ({ name: ch.name, value: ch.total_orders }))
    // Monochromatic accent palette — Linear violet at different lightness levels
    const piePalette = ['#7170FF', '#9B9BF5', '#C5C5FF', '#DEDEDFF', '#EBEBFF']
    chart.setOption({
      tooltip: {
        trigger: 'item',
        backgroundColor: c.bg,
        borderColor: c.border,
        textStyle: { color: c.text, fontSize: 12 },
        formatter: '{b}: {c} 单 ({d}%)'
      },
      legend: { bottom: 4, textStyle: { color: c.muted, fontSize: 11 } },
      series: [{
        type: 'pie',
        radius: ['44%', '66%'],
        center: ['50%', '48%'],
        avoidLabelOverlap: true,
        itemStyle: { borderRadius: 4, borderColor: c.bg, borderWidth: 2 },
        label: { show: false },
        emphasis: { label: { show: true, fontSize: 13, fontWeight: 590, color: c.text } },
        data: pieData.map((x, i) => ({ ...x, itemStyle: { color: piePalette[i % piePalette.length] } }))
      }]
    }, true)
    charts.push(chart)
  }

  // === 柱状图：交易金额 ===
  // Flat accent bar, no gradient
  if (amountRef.value) {
    const chart = echarts.getInstanceByDom(amountRef.value) || echarts.init(amountRef.value)
    chart.setOption({
      tooltip: {
        trigger: 'axis',
        backgroundColor: c.bg,
        borderColor: c.border,
        textStyle: { color: c.text, fontSize: 12 },
        formatter: p => `${p[0].axisValue}<br/>交易额: ¥${p[0].value}`
      },
      grid: { left: 56, right: 16, top: 12, bottom: 10, containLabel: true },
      xAxis: {
        type: 'category', data: dates,
        axisLine: { lineStyle: { color: c.border } },
        axisTick: { show: false },
        axisLabel: { color: c.muted, fontSize: 11 }
      },
      yAxis: {
        type: 'value',
        splitLine: { show: false },
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: { color: c.muted, fontSize: 11, formatter: v => '¥' + v }
      },
      series: [{
        type: 'bar',
        data: amounts,
        barWidth: '50%',
        itemStyle: { color: c.accent, borderRadius: [3, 3, 0, 0] }
      }]
    }, true)
    charts.push(chart)
  }

  // === 横向柱状图：订单状态 ===
  // Muted gray bars, accent for emphasis on the first bar
  if (statusRef.value) {
    const chart = echarts.getInstanceByDom(statusRef.value) || echarts.init(statusRef.value)
    const sData = [
      { name: '成功', value: d.value.success_orders ?? 0, color: c.accent },
      { name: '待支付', value: d.value.wait_orders ?? 0, color: c.muted },
      { name: '已关闭', value: d.value.closed_orders ?? 0, color: c.muted }
    ]
    chart.setOption({
      tooltip: {
        trigger: 'axis',
        backgroundColor: c.bg,
        borderColor: c.border,
        textStyle: { color: c.text, fontSize: 12 }
      },
      grid: { left: 12, right: 40, top: 8, bottom: 10, containLabel: true },
      xAxis: {
        type: 'value',
        splitLine: { show: false },
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: { color: c.muted, fontSize: 11 }
      },
      yAxis: {
        type: 'category',
        data: sData.map(x => x.name),
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: { color: c.text, fontSize: 12 }
      },
      series: [{
        type: 'bar',
        barWidth: '38%',
        data: sData.map(x => ({ value: x.value, itemStyle: { color: x.color, borderRadius: [0, 3, 3, 0] } })),
        label: { show: true, position: 'right', color: c.muted, fontSize: 11 }
      }]
    }, true)
    charts.push(chart)
  }
}

async function load() {
  loading.value = true
  try {
    let params = {}
    if (range.value && range.value.length === 2) {
      params = { start: range.value[0], end: range.value[1] }
    } else {
      params = { days: days.value }
    }
    const [dash, chs] = await Promise.all([api.dashboard(params), api.channels()])
    d.value = dash
    channels.value = chs
    emit('tunnel', dash.tunnel)
    setTimeout(renderCharts, 50)
  } catch (e) {
    ElMessage.error('加载失败: ' + e.message)
  } finally {
    loading.value = false
  }
}

// 快捷按钮：清空日历范围，按天数加载
function onQuickRange() {
  range.value = null
  reloadCharts()
}

// 日历范围选择：加载所选范围
function onRangePick(val) {
  if (!val) return
  reloadCharts()
}

function reloadCharts() {
  charts.forEach(c => c.dispose())
  charts = []
  load()
}

watch(() => props.refreshKey, load)
watch(days, onQuickRange)

function onResize() { charts.forEach(c => c.resize()) }

function statusText(s) {
  return { WAIT_BUYER_PAY: '待支付', TRADE_SUCCESS: '成功', TRADE_CLOSED: '已关闭', CONFIRMING: '确认中', FAILED: '失败' }[s] || s
}
function statusType(s) {
  return { WAIT_BUYER_PAY: 'warning', TRADE_SUCCESS: 'success', TRADE_CLOSED: 'info', CONFIRMING: 'warning', FAILED: 'danger' }[s] || 'info'
}
function fmtTime(t) {
  if (!t) return '—'
  return new Date(t).toLocaleString('zh-CN', { hour12: false })
}

onMounted(() => { load(); window.addEventListener('resize', onResize) })
onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  charts.forEach(c => c.dispose())
  charts = []
})
</script>

<style scoped>
.dash { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 20px; }

/* KPI 网格 — 克制配色：全中性文字，无彩色图标 */
.kpi-grid { display: grid; grid-template-columns: repeat(5, 1fr); gap: 14px; }

.kpi-card {
  display: flex;
  flex-direction: column;
  padding: 18px 20px;
  cursor: default;
}

/* label：小号、uppercase、字母间距 */
.kpi-label {
  font-size: 11px;
  font-weight: 510;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.06em;
  line-height: 1.2;
}

/* 值：统一用主文字色，无任何彩色背景 */
.kpi-value {
  font-size: 24px;
  font-weight: 590;
  color: var(--text-primary);
  letter-spacing: -0.02em;
  line-height: 1.2;
  margin-top: 6px;
  font-feature-settings: 'tnum';
}

/* 描述：小号、浅色 */
.kpi-delta {
  font-size: 11px;
  color: var(--text-muted);
  margin-top: 4px;
  font-weight: 400;
}

/* 图表行 */
.chart-row { display: grid; grid-template-columns: 2fr 1fr; gap: 14px; }
.chart-card { overflow: hidden; }
.chart-title { font-weight: 590; font-size: 13px; color: var(--text-primary); }
.trend-controls { display: flex; align-items: center; gap: 10px; }
.chart-box { height: 220px; padding: 8px 8px 10px; }

/* 状态行 */
.status-row { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
.status-card { overflow: hidden; }

.tunnel-badge {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  color: var(--danger);
  font-weight: 510;
}
.tunnel-badge.ok { color: var(--success); }
.tunnel-badge .dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
}

.tunnel-info { display: flex; align-items: center; gap: 12px; }
.tunnel-icon { color: var(--text-muted); flex-shrink: 0; }
.tunnel-url { font-size: 13px; color: var(--text-body); word-break: break-all; font-feature-settings: 'tnum'; }
.tunnel-cb { font-size: 11px; color: var(--text-muted); margin-top: 2px; word-break: break-all; }

/* 队列统计 */
.queue-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 8px; }
.queue-item { text-align: center; padding: 12px 6px; background: var(--bg-subtle); border-radius: 6px; }
.queue-num { font-size: 18px; font-weight: 590; color: var(--text-primary); font-feature-settings: 'tnum'; }
.queue-label { font-size: 10px; color: var(--text-muted); margin-top: 4px; font-weight: 510; text-transform: uppercase; letter-spacing: 0.05em; }

.rate-label { font-size: 11px; margin-top: 10px; text-align: center; }

/* 账本 */
.ledger-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 8px; }
.ledger-item { text-align: center; padding: 14px 6px; background: var(--bg-subtle); border-radius: 6px; }
.ledger-num { font-size: 17px; font-weight: 590; color: var(--text-primary); }
.ledger-label { font-size: 10px; color: var(--text-muted); margin-top: 4px; font-weight: 510; text-transform: uppercase; letter-spacing: 0.05em; }

/* 链接按钮 */
.link-btn {
  display: flex;
  align-items: center;
  gap: 4px;
  border: none;
  background: transparent;
  color: var(--accent);
  font-size: 12px;
  font-weight: 510;
  cursor: pointer;
  padding: 4px 8px;
  border-radius: 4px;
  transition: background 0.12s ease;
}
.link-btn:hover { background: var(--accent-light); }

/* 通用 */
.text-mono { font-family: 'JetBrains Mono', 'SF Mono', Consolas, monospace; font-feature-settings: 'tnum'; }
.text-muted { color: var(--text-muted); }
.text-success { color: var(--success); }
.text-warning { color: var(--warning); }
.text-danger { color: var(--danger); }
.font-mono { font-family: 'JetBrains Mono', 'SF Mono', Consolas, monospace; font-feature-settings: 'tnum'; }
.mt14 { margin-top: 14px; }

@media (max-width: 1200px) {
  .kpi-grid { grid-template-columns: repeat(3, 1fr); }
  .chart-row, .status-row { grid-template-columns: 1fr; }
}
</style>