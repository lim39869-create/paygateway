<template>
  <div class="device-stream">
    <div class="stream-wrap">
      <div class="stream-stage">
        <canvas ref="canvasEl" class="stream-canvas"
          @pointerdown="onPointerDown" @pointermove="onPointerMove"
          @pointerup="onPointerUp" @pointercancel="onPointerUp"
          @contextmenu.prevent></canvas>
        <div v-if="!connected" class="stream-overlay">
          <el-icon v-if="connecting" class="is-loading" :size="40"><Loading /></el-icon>
          <el-icon v-else :size="40"><WarningFilled /></el-icon>
          <div class="stream-msg">{{ statusText }}</div>
          <el-button v-if="!connecting && errorMsg" size="small" type="primary" @click="connect">重连</el-button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, watch, onMounted, onBeforeUnmount } from 'vue'
import { Loading, WarningFilled } from '@element-plus/icons-vue'
import { api } from '../api'

const props = defineProps({
  // 候选 adb serial 列表（USB 主通道优先，WiFi 备用次之）。连接失败/断开自动尝试下一个。
  serials: { type: Array, default: () => [] },
  deviceId: { type: Number, default: 0 }
})

// 本地 serial 副本：全部候选失败后自动重建 WiFi 链路并把可用 serial 追加进来重试
const serials = ref([...props.serials])
watch(() => props.serials, (v) => { serials.value = [...v] })

const canvasEl = ref(null)
const connecting = ref(false)
const connected = ref(false)
const statusText = ref('连接中…')
const errorMsg = ref('')

let ws = null
let decoder = null
let ctx2d = null
let meta = { width: 0, height: 0 }
let pendingTouch = null
let dpr = window.devicePixelRatio || 1
// 当前尝试的 serial 下标；USB 断开后自动切到 WiFi 备用（第 2 个候选）
let serialIdx = 0
// 手动关闭标志：用户主动断开时不自动重连
let manualClosed = false
// WiFi 通道重建尝试次数（连续失败 3 次后停止，避免无限重试）
let reconnectAttempts = 0

function wsURL() {
  const serial = serials.value[serialIdx] || ''
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  const token = sessionStorage.getItem('admin_token') || ''
  return `${proto}://${location.host}/api/personal/devices/stream?serial=${encodeURIComponent(serial)}&token=${encodeURIComponent(token)}`
}

function connect() {
  if (connecting.value) return
  if (!serials.value.length) { fail('设备未配置 adb 通道'); return }
  connecting.value = true
  manualClosed = false
  errorMsg.value = ''
  statusText.value = `连接中…（${serialIdx + 1}/${serials.value.length}）`
  try {
    ws = new WebSocket(wsURL())
  } catch (e) {
    fail('WebSocket 创建失败: ' + e.message)
    return
  }
  ws.binaryType = 'arraybuffer'
  ws.onopen = () => { connected.value = true; statusText.value = '已连接'; reconnectAttempts = 0 }
  ws.onmessage = onWsMessage
  ws.onclose = () => {
    connected.value = false
    connecting.value = false
    destroyDecoder()
    if (manualClosed) return
    // 自动切换下一个候选 serial（USB → WiFi 备用）
    if (serialIdx < serials.value.length - 1) {
      serialIdx++
      errorMsg.value = ''
      setTimeout(connect, 500)
    } else {
      // 全部候选失败：断网/断 USB 后 adb 连接会失效，重建 WiFi 链路再重试
      rebuildAndRetry()
    }
  }
}

// 设备心跳走公网隧道（status 可能仍 online），但 scrcpy 画面流的 adb 连接在断网时失效，
// 需调用后端重新 adb connect（wifi-reconnect）恢复链路。最多重试 3 次。
async function rebuildAndRetry() {
  if (!props.deviceId) { fail('连接已断开（设备未关联）'); return }
  if (reconnectAttempts >= 3) { fail('连接已断开（多次重建失败，请检查设备网络/USB）'); return }
  reconnectAttempts++
  connecting.value = true
  statusText.value = `adb 通道离线，正在重建连接（${reconnectAttempts}/3）…`
  try {
    const data = await api.personalWifiReconnect(props.deviceId)
    const serial = data && data.serial
    if (!serial) throw new Error('无可用通道')
    if (!serials.value.includes(serial)) serials.value.unshift(serial)
    serialIdx = 0
    errorMsg.value = ''
    setTimeout(connect, 300)
  } catch (e) {
    statusText.value = '连接已断开（等待重试）'
    errorMsg.value = e.message || '连接已断开'
    setTimeout(rebuildAndRetry, 3000)
  }
}

function fail(msg) {
  connecting.value = false
  connected.value = false
  errorMsg.value = msg
  statusText.value = msg
}

function onWsMessage(ev) {
  if (typeof ev.data === 'string') {
    try {
      const m = JSON.parse(ev.data)
      if (m.type === 'meta') { meta = m; resizeCanvas() }
    } catch (e) { /* ignore */ }
    return
  }
  const buf = new Uint8Array(ev.data)
  if (buf.length < 1) return
  const tag = buf[0]
  const payload = buf.slice(1)
  if (!decoder) ensureDecoder()
  if (!decoder) return
  try {
    // 服务端帧类型前缀：0x01=config(SPS/PPS) 0x02=IDR关键帧 0x03=P帧
    // 关键帧必须标 type:'key'，否则 WebCodecs 拒绝解码
    const chunk = new EncodedVideoChunk({
      type: tag === 0x01 || tag === 0x02 ? 'key' : 'delta',
      timestamp: 0,
      data: payload
    })
    decoder.decode(chunk)
  } catch (e) { /* 解码器未就绪时忽略 */ }
}

function ensureDecoder() {
  if (decoder) return
  if (typeof VideoDecoder === 'undefined') {
    errorMsg.value = '浏览器不支持 WebCodecs（请使用 Chrome/Edge 94+）'
    statusText.value = errorMsg.value
    return
  }
  decoder = new VideoDecoder({
    output: (frame) => { drawFrame(frame); frame.close() },
    error: () => { decoder = null }
  })
  try {
    // Annex-B 格式：scrcpy 数据为 start code 分隔，SPS/PPS 内嵌在 config 帧中
    decoder.configure({ codec: 'avc1.42E01E', avc: { format: 'annexb' }, optimizeForLatency: true })
    statusText.value = '解码就绪'
  } catch (e) {
    errorMsg.value = '解码器配置失败: ' + e.message
    statusText.value = errorMsg.value
    decoder = null
  }
}

function drawFrame(frame) {
  const canvas = canvasEl.value
  if (!canvas) return
  ctx2d = ctx2d || canvas.getContext('2d')
  // 按显示区域等比缩放居中绘制（对象覆盖）
  const cw = canvas.clientWidth, ch = canvas.clientHeight
  const scale = Math.min(cw / frame.displayWidth, ch / frame.displayHeight)
  const dw = frame.displayWidth * scale, dh = frame.displayHeight * scale
  const dx = (cw - dw) / 2, dy = (ch - dh) / 2
  ctx2d.clearRect(0, 0, canvas.width, canvas.height)
  ctx2d.drawImage(frame, dx, dy, dw, dh)
}

function destroyDecoder() {
  if (decoder) { try { decoder.close() } catch (e) {} decoder = null }
  ctx2d = null
}

function resizeCanvas() {
  const canvas = canvasEl.value
  if (!canvas || !meta.width) return
  canvas.width = Math.round(canvas.clientWidth * dpr)
  canvas.height = Math.round(canvas.clientHeight * dpr)
  ctx2d = canvas.getContext('2d')
  ctx2d.setTransform(dpr, 0, 0, dpr, 0, 0)
}

function sendControl(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj))
}

// 指针坐标 → 设备坐标：与 drawFrame 用完全相同的 contain 几何
// （视频等比缩放居中显示，黑边区域忽略，避免点击错位）
function pointerToDevice(ev) {
  const canvas = canvasEl.value
  if (!canvas || !meta.width || !meta.height) return null
  const rect = canvas.getBoundingClientRect()
  const scale = Math.min(rect.width / meta.width, rect.height / meta.height)
  const dw = meta.width * scale
  const dh = meta.height * scale
  const dx = (rect.width - dw) / 2
  const dy = (rect.height - dh) / 2
  const px = ev.clientX - rect.left
  const py = ev.clientY - rect.top
  if (px < dx || px > dx + dw || py < dy || py > dy + dh) return null
  return { x: Math.round((px - dx) / scale), y: Math.round((py - dy) / scale) }
}

function onPointerDown(ev) {
  if (!meta.width) return
  const p = pointerToDevice(ev)
  if (!p) return
  pendingTouch = p
  canvasEl.value.setPointerCapture(ev.pointerId)
  sendControl({ type: 'touch', action: 0, x: p.x, y: p.y })
}

function onPointerMove(ev) {
  if (!pendingTouch || !meta.width) return
  const p = pointerToDevice(ev)
  if (!p) return
  sendControl({ type: 'touch', action: 2, x: p.x, y: p.y })
}

function onPointerUp() {
  if (!pendingTouch) return
  sendControl({ type: 'touch', action: 1, x: pendingTouch.x, y: pendingTouch.y })
  pendingTouch = null
}

onMounted(() => { connect(); window.addEventListener('resize', resizeCanvas) })
onBeforeUnmount(() => {
  window.removeEventListener('resize', resizeCanvas)
  manualClosed = true // 组件卸载时禁止自动重连
  if (ws) ws.close()
  destroyDecoder()
})
</script>

<style scoped>
.device-stream { display: flex; flex-direction: column; gap: 10px; height: 100%; }
/* 手机竖屏容器：自适应高度，宽度按 9/16 比例，居中 */
.stream-wrap { flex: 1; display: flex; align-items: center; justify-content: center; min-height: 0; overflow: hidden; }
.stream-stage {
  position: relative; background: #000; border-radius: 10px; overflow: hidden;
  height: 100%; aspect-ratio: 9 / 16; max-width: 100%;
}
.stream-canvas { width: 100%; height: 100%; display: block; cursor: crosshair; touch-action: none; }
.stream-overlay {
  position: absolute; inset: 0; display: flex; flex-direction: column; gap: 12px;
  align-items: center; justify-content: center; color: #999; background: rgba(0,0,0,.3);
}
.stream-msg { font-size: 13px; color: #bbb; }
</style>
