<template>
  <div class="channels-page">
    <!-- 易支付对接信息 -->
    <div class="pg-card epay-info-card">
      <div class="pg-card-header">
        <span class="section-label">易支付对接信息</span>
        <el-tag type="success" size="small" effect="light">复制给 New API 等支持易支付的项目</el-tag>
      </div>
      <div class="pg-card-body">
        <el-row :gutter="12">
          <el-col :span="8">
            <div class="cfg-item">
              <div class="cfg-label">易支付网关地址（submit）</div>
              <div class="cfg-value font-mono"><code>{{ epay.submit_url }}</code></div>
              <el-button size="small" text type="primary" @click="copy(epay.submit_url)">复制</el-button>
            </div>
          </el-col>
          <el-col :span="8">
            <div class="cfg-item">
              <div class="cfg-label">商户ID (pid)</div>
              <div class="cfg-value text-muted" style="font-size: 12px">每个上游独立配置</div>
              <div class="cfg-value text-muted" style="font-size: 12px">→ 见「上游对接」页</div>
            </div>
          </el-col>
          <el-col :span="8">
            <div class="cfg-item">
              <div class="cfg-label">商户密钥 (key)</div>
              <div class="cfg-value text-muted" style="font-size: 12px">每个上游独立密钥</div>
              <div class="cfg-value text-muted" style="font-size: 12px">→ 见「上游对接」页</div>
            </div>
          </el-col>
        </el-row>
        <el-row :gutter="12" style="margin-top: 12px">
          <el-col :span="12">
            <div class="cfg-item">
              <div class="cfg-label">notify_url（填商户/上游自己的回调，不是这里）</div>
              <div class="cfg-value text-muted">例：http://127.0.0.1:3000/api/payment/notify（仅同机部署；公网对接填商户自己的公网回调域名）</div>
            </div>
          </el-col>
          <el-col :span="12">
            <div class="cfg-item">
              <div class="cfg-label">订单查询接口</div>
              <div class="cfg-value font-mono"><code>{{ epay.query_url }}</code></div>
            </div>
          </el-col>
        </el-row>
        <el-alert type="success" :closable="false" class="mt12"
          title="对接地址优先展示公网固定域名（与支付页/兜底回跳一致）；本机部署新 API 可用 127.0.0.1:8080（网关固定监听，与 CF 隧道无关）。公网对接请用本页展示的公网地址。" />
      </div>
    </div>

    <!-- 支付通道列表 -->
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="section-label">支付通道</span>
        <span class="text-muted" style="font-size: 12px">已接入 {{ list.length }} 个通道</span>
      </div>
      <div class="pg-card-body">
        <el-alert type="info" :closable="false" class="mb16"
          title="通道采用可插拔架构：新增支付方式 = 实现 PaymentChannel 接口 + 注册 + 配置。以下为当前已接入的通道。" />
        <div v-loading="loading" class="channel-grid">
          <div v-for="ch in list" :key="ch.name" class="channel-card pg-card">
            <div class="channel-head">
              <div class="channel-logo" :class="ch.name">
                <el-icon :size="18"><component :is="channelIconComponent(ch.name)" /></el-icon>
              </div>
              <div class="channel-name-wrap">
                <div class="channel-name">{{ channelName(ch.name) }}</div>
                <div class="channel-id font-mono text-muted">{{ ch.name }}</div>
              </div>
              <el-tag :type="ch.enabled ? 'success' : 'danger'" size="small" effect="light">
                {{ ch.enabled ? '已启用' : '已停用' }}
              </el-tag>
            </div>

            <el-descriptions :column="1" size="small" class="mt10">
              <el-descriptions-item label="环境">
                <el-tag :type="ch.is_prod ? 'danger' : 'warning'" size="small">{{ ch.is_prod ? '生产' : '沙箱测试' }}</el-tag>
              </el-descriptions-item>
              <el-descriptions-item label="APPID">
                <span class="font-mono" style="font-size: 12px">{{ ch.app_id || '—' }}</span>
              </el-descriptions-item>
              <el-descriptions-item label="商户号" v-if="ch.mch_id">
                <span class="font-mono" style="font-size: 12px">{{ ch.mch_id }}</span>
              </el-descriptions-item>
              <el-descriptions-item label="支持模式">
                <div class="mode-switches">
                  <div v-for="m in allModes" :key="m" class="mode-switch-item">
                    <el-tag :type="ch.modes_enabled && ch.modes_enabled[m] !== false ? 'success' : 'info'" size="small" effect="plain" style="margin-right: 4px">
                      {{ modeText(m) }}
                    </el-tag>
                    <el-switch v-if="ch.modes_enabled" :model-value="ch.modes_enabled[m] !== false"
                      size="small" @change="toggleMode(ch.name, m)" />
                  </div>
                </div>
              </el-descriptions-item>
              <el-descriptions-item label="回调地址">
                <span class="font-mono text-muted" style="font-size: 11px; word-break: break-all">{{ ch.notify_url || '—' }}</span>
              </el-descriptions-item>
            </el-descriptions>

            <el-alert v-if="ch.enabled && !ch.configured" type="warning" :closable="false" class="mt10"
              title="通道已启用但未配置账户" description="请前往「账户池管理」点击「新增账户」配置密钥，保存后即刻生效，无需重启。" />

            <div class="channel-stats">
              <div class="stat">
                <div class="stat-num font-mono">{{ ch.today_orders ?? 0 }}</div>
                <div class="stat-label">今日订单</div>
              </div>
              <div class="stat">
                <div class="stat-num font-mono">¥{{ ((ch.today_amount ?? 0) / 100).toFixed(2) }}</div>
                <div class="stat-label">今日交易额</div>
              </div>
              <div class="stat">
                <div class="stat-num font-mono">{{ ch.total_orders ?? 0 }}</div>
                <div class="stat-label">累计订单</div>
              </div>
              <div class="stat">
                <div class="stat-num font-mono">¥{{ ((ch.total_amount ?? 0) / 100).toFixed(2) }}</div>
                <div class="stat-label">累计交易额</div>
              </div>
            </div>

            <div class="channel-foot">
              <span class="text-muted" style="font-size: 11px">类型: {{ ch.kind === 'callback' ? '回调驱动' : '监听驱动' }}</span>
              <el-tag v-if="ch.enabled" type="success" size="small">已启用</el-tag>
              <el-tag v-else type="info" size="small">已禁用</el-tag>
            </div>
          </div>

          <div v-if="!list.length && !loading" class="empty-card pg-card">
            <el-empty description="暂无已接入的通道" :image-size="80" />
          </div>
        </div>
      </div>
    </div>

    <!-- 接入新通道指引 -->
    <div class="pg-card">
      <div class="pg-card-header"><span class="section-label">接入新通道</span></div>
      <div class="pg-card-body">
        <el-row :gutter="16">
          <el-col :span="6" v-for="(s, i) in steps" :key="i">
            <div class="step">
              <div class="step-num">{{ i + 1 }}</div>
              <div class="step-title">{{ s.title }}</div>
              <div class="step-desc">{{ s.desc }}</div>
            </div>
          </el-col>
        </el-row>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, watch, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '../api'

const props = defineProps({ refreshKey: Number })

const list = ref([])
const loading = ref(false)
const epay = ref({})

watch(() => props.refreshKey, load)

function copy(text) {
  navigator.clipboard?.writeText(text)
  ElMessage.success('已复制')
}

const steps = [
  { title: '进入账户池', desc: '打开「账户池管理」页面，点击「新增账户」' },
  { title: '填写密钥', desc: '选择通道，粘贴 AppID / 私钥 / 公钥（AES-256-GCM 加密存储）' },
  { title: '保存生效', desc: '保存后账户自动进入内存账户池参与调度，无需重启' },
  { title: '开启支付模式', desc: '在通道卡片上切换 page / wap / qrcode 模式（扫码兜底）' }
]

async function load() {
  loading.value = true
  try {
    const [chs, ei] = await Promise.all([api.channels(), api.epayInfo()])
    list.value = chs
    epay.value = ei || {}
  } catch (e) {
    ElMessage.error('加载失败: ' + e.message)
  } finally {
    loading.value = false
  }
}

function channelName(name) {
  return { alipay: '支付宝', wechat: '微信支付', paypal: 'PayPal', crypto: '加密货币' }[name] || name
}

function channelIconComponent(name) {
  return { alipay: 'CircleCheck', wechat: 'CircleCheck', paypal: 'CircleCheck', crypto: 'CircleCheck' }[name] || 'CircleCheck'
}

function modeText(m) {
  return { page: '电脑网页', wap: '手机WAP', qrcode: '扫码', address: '链上地址' }[m] || m
}

const allModes = ['page', 'wap', 'qrcode']

async function toggleMode(channel, mode) {
  try {
    const data = await api.toggleChannelMode(channel, mode)
    ElMessage.success(`支付模式 ${modeText(mode)} 已${data.enabled ? '开启' : '关闭'}`)
    load()
  } catch (e) {
    ElMessage.error('切换失败: ' + e.message)
  }
}

onMounted(load)
</script>

<style scoped>
.channels-page { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 16px; }

.section-label { font-weight: 590; font-size: 13px; color: var(--text-primary); }

.channel-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(340px, 1fr)); gap: 12px; }
.channel-card { padding: 16px; cursor: default; }
.channel-card:hover { border-color: var(--border-strong); }

.channel-head { display: flex; align-items: center; gap: 10px; }
.channel-logo {
  width: 38px; height: 38px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  font-size: 16px; font-weight: 600; color: #fff;
  background: var(--accent);
  flex-shrink: 0;
}
.channel-logo.wechat { background: #22A069; }
.channel-name-wrap { flex: 1; min-width: 0; }
.channel-name { font-size: 13px; font-weight: 590; color: var(--text-primary); }
.channel-id { font-size: 11px; font-feature-settings: 'tnum'; }

.channel-stats { display: grid; grid-template-columns: repeat(4, 1fr); gap: 8px; margin-top: 14px; }
.stat { text-align: center; padding: 10px 4px; background: var(--bg-subtle); border-radius: 6px; }
.stat-num { font-size: 14px; font-weight: 590; color: var(--text-primary); font-feature-settings: 'tnum'; }
.stat-label { font-size: 10px; color: var(--text-muted); margin-top: 2px; font-weight: 510; text-transform: uppercase; letter-spacing: 0.04em; }

.channel-foot {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid var(--border-subtle);
}

.step { padding: 16px; border-radius: 8px; background: var(--bg-subtle); }
.step-num {
  width: 22px; height: 22px; border-radius: 50%;
  background: var(--accent);
  color: #fff;
  display: flex; align-items: center; justify-content: center;
  font-size: 11px; font-weight: 590; margin-bottom: 10px;
}
.step-title { font-size: 13px; font-weight: 590; margin-bottom: 4px; color: var(--text-primary); }
.step-desc { font-size: 11px; color: var(--text-muted); line-height: 1.5; }

.mode-switches { display: flex; flex-wrap: wrap; gap: 8px; }
.mode-switch-item { display: flex; align-items: center; gap: 4px; }

.mt10 { margin-top: 10px; }
.mt12 { margin-top: 12px; }
.mb16 { margin-bottom: 16px; }

/* 易支付对接卡片 */
.epay-info-card { border: 1px solid var(--success-border); }
.cfg-item {
  background: var(--bg-subtle);
  border-radius: 6px;
  padding: 12px 14px;
  margin-bottom: 8px;
}
.cfg-label { font-size: 11px; color: var(--text-muted); margin-bottom: 6px; font-weight: 510; text-transform: uppercase; letter-spacing: 0.04em; }
.cfg-value { font-size: 13px; word-break: break-all; display: flex; align-items: center; gap: 8px; }
.cfg-value code {
  background: var(--bg-surface);
  padding: 2px 8px;
  border-radius: 4px;
  font-size: 12px;
  border: 1px solid var(--border);
  font-family: 'JetBrains Mono', monospace;
}

.font-mono { font-family: 'JetBrains Mono', 'SF Mono', Consolas, monospace; font-feature-settings: 'tnum'; }
.text-muted { color: var(--text-muted); }
</style>