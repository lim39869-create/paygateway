<template>
  <div v-loading="loading">
    <el-alert v-if="error" :title="error" type="error" show-icon closable class="mb-3" />
    <div v-if="!loading && items.length === 0" class="empty">
      <el-empty description="暂无收款码数据" />
    </div>
    <div v-else class="qrcode-grid">
      <div v-for="item in items" :key="item.device_id + '-' + item.platform + '-' + (item.user_id ?? 0)" class="qrcode-card">
        <div class="card-header">
          <span class="device-name">{{ item.device_name || item.device_id }}</span>
          <el-tag v-if="item.platform === 'alipay'" type="primary" size="small">支付宝</el-tag>
          <el-tag v-else-if="item.platform === 'wechat'" type="success" size="small">微信</el-tag>
          <el-tag :type="item.status === 'active' ? 'success' : 'info'" size="small">
            {{ item.status === 'active' ? '有效' : '已过期' }}
          </el-tag>
          <el-tag v-if="(item.user_id ?? 0) === 999" type="warning" size="small" effect="plain">双开</el-tag>
          <el-tag v-else size="small" effect="plain">主空间</el-tag>
        </div>
        <div class="card-account" v-if="item.account">
          收款账户: <span class="account-name">{{ item.account }}</span>
        </div>
        <div class="card-account" v-else>收款账户: <span class="account-unknown">未知</span></div>
        <div class="card-body">
          <div class="qr-area" v-if="item.qr_content">
            <img :src="qrUrl(item.qr_content)" class="qr-img" alt="收款码" />
            <div class="qr-deadline">
              过期倒计时: <span :class="{ urgent: secondsLeft(item) < 15 }">
                {{ secondsLeftText(item) }}
              </span>
            </div>
          </div>
          <div v-else class="no-qr">暂无收款码</div>
        </div>
        <div class="card-footer">
          <el-button v-if="item.platform === 'wechat'" type="success" size="small"
            :loading="adbCapturing === item.device_id + '-' + (item.user_id ?? 0)" @click="handleAdbCapture(item.device_id, 'wechat', item.user_id ?? 0)">
            <el-icon><Connection /></el-icon> adb捕获
          </el-button>
          <el-button v-if="item.platform === 'alipay'" type="warning" size="small"
            :loading="adbCapturing === item.device_id + '-' + (item.user_id ?? 0)" @click="handleAdbCapture(item.device_id, 'alipay', item.user_id ?? 0)">
            <el-icon><Connection /></el-icon> adb捕获
          </el-button>
          <el-button type="danger" size="small" plain
            :loading="deleting === item.device_id + '-' + item.platform + '-' + (item.user_id ?? 0)"
            @click="handleDelete(item)">
            <el-icon><Delete /></el-icon> 移除
          </el-button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import { ElMessageBox } from 'element-plus'
import { Connection, Delete } from '@element-plus/icons-vue'
import { api } from '../api.js'

const props = defineProps({ refreshKey: Number })

const loading = ref(false)
const error = ref('')
const items = ref([])
const adbCapturing = ref('')
const deleting = ref('')
let timer = null

async function load() {
  loading.value = true
  error.value = ''
  try {
    const data = await api.personalQrcodes()
    items.value = Array.isArray(data) ? data : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function qrUrl(content) {
  if (!content) return ''
  return 'https://chart.googleapis.com/chart?chs=200x200&cht=qr&chl=' + encodeURIComponent(content)
}

function secondsLeft(item) {
  if (!item.expire_at) return 0
  const expire = new Date(item.expire_at).getTime()
  const now = Date.now()
  return Math.max(0, Math.floor((expire - now) / 1000))
}

function secondsLeftText(item) {
  const s = secondsLeft(item)
  if (s <= 0) return '已过期'
  const m = Math.floor(s / 60)
  const sec = s % 60
  return `${m}:${String(sec).padStart(2, '0')}`
}

// 收款码：服务器 adb 捕获（微信保存链路 / 支付宝截图链路，穿透 SECURE）
async function handleAdbCapture(deviceId, platform, userId = 0) {
  const key = deviceId + '-' + userId
  adbCapturing.value = key
  try {
    if (platform === 'wechat') {
      await api.personalCaptureWechatQrcode(deviceId, userId)
    } else {
      await api.personalCaptureAlipayQrcode(deviceId, userId)
    }
    await load()
    error.value = ''
  } catch (e) {
    error.value = 'adb捕获失败: ' + e.message
  } finally {
    adbCapturing.value = ''
  }
}

async function handleDelete(item) {
  const userId = item.user_id ?? 0
  try {
    await ElMessageBox.confirm(
      `确定移除「${item.device_name || item.device_id}」的${item.platform === 'alipay' ? '支付宝' : '微信'}收款码（${userId === 999 ? '双开' : '主空间'}）？`,
      '移除收款码',
      { type: 'warning', confirmButtonText: '移除', cancelButtonText: '取消' }
    )
  } catch {
    return
  }
  deleting.value = item.device_id + '-' + item.platform + '-' + userId
  try {
    await api.personalDeleteQrcode(item.device_id, item.platform, userId)
    await load()
  } catch (e) {
    error.value = '移除失败: ' + e.message
  } finally {
    deleting.value = ''
  }
}

// 倒计时更新（每秒刷新倒计时显示）
onMounted(() => {
  load()
  timer = setInterval(load, 15000) // 每 15s 自动刷新
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.qrcode-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
  gap: 16px;
  padding: 16px 0;
}
.qrcode-card {
  background: #fff;
  border-radius: 8px;
  border: 1px solid var(--el-border-color-lighter);
  padding: 16px;
  transition: box-shadow 0.2s;
}
.qrcode-card:hover {
  box-shadow: 0 2px 12px rgba(0,0,0,0.08);
}
:root.dark .qrcode-card {
  background: var(--el-bg-color-overlay);
}
.card-header {
  display: flex;
  gap: 8px;
  align-items: center;
  margin-bottom: 12px;
}
.device-name {
  font-weight: 600;
  font-size: 14px;
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.card-account {
  font-size: 13px;
  color: #666;
  margin-bottom: 10px;
  padding-left: 2px;
}
.card-account .account-name {
  color: var(--el-color-primary);
  font-weight: 500;
}
.card-account .account-unknown {
  color: #bbb;
}
.card-body {
  display: flex;
  justify-content: center;
  margin-bottom: 12px;
}
.qr-area {
  text-align: center;
}
.qr-img {
  width: 180px;
  height: 180px;
  display: block;
  margin: 0 auto;
  border: 1px solid #eee;
  border-radius: 4px;
}
.qr-deadline {
  margin-top: 8px;
  font-size: 13px;
  color: #999;
}
.qr-deadline .urgent {
  color: #f56c6c;
  font-weight: 600;
}
.no-qr {
  text-align: center;
  color: #ccc;
  font-size: 14px;
  padding: 40px 0;
}
.card-footer {
  display: flex;
  justify-content: center;
}
.empty {
  margin-top: 60px;
}
.mb-3 { margin-bottom: 12px; }
</style>
