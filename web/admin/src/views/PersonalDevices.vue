<template>
  <div class="personal-devices-page">
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="section-label">监听端设备</span>
        <div class="header-actions">
          <span v-if="!batchMode" class="text-muted" style="font-size: 12px">共 {{ devices.length }} 台 · 在线 {{ onlineCount }} 台</span>
          <span v-else class="text-muted" style="font-size: 12px; color: var(--el-color-warning)">已选 {{ selectedCount }} 台</span>
          <el-button v-if="!batchMode" type="warning" size="small" @click="toggleBatchMode">
            批量操作
          </el-button>
          <el-button v-else size="small" @click="toggleBatchMode">退出批量</el-button>
          <el-button type="success" size="small" @click="openWizard">
            <el-icon><Plus /></el-icon> USB 引导接入
          </el-button>
        </div>
      </div>
      <div class="pg-card-body">
        <el-alert type="info" :closable="false" class="mb16"
          title="ADB 真机绑定个人收款账户：设备上线由 adb 连通性判定（USB 直连 / WiFi 备用）。" />
        <el-collapse class="integration-guide">
          <el-collapse-item name="guide">
            <template #title>
              <span class="guide-title">集成流程（USB 真机）</span>
              <el-tag type="info" size="small" effect="plain" round class="guide-tag">点击展开</el-tag>
            </template>
            <div class="guide-content">
              <el-tabs>
                <el-tab-pane label="真机 USB 直连">
                  <ol class="step-list">
                    <li><b>接入引导</b>：点击右上「USB 引导接入」，向导自动检测已连接的 USB 真机（手机插 USB + 开启开发者选项 + USB 调试）。</li>
                    <li><b>授权调试</b>：手机会弹出「允许 USB 调试」，点击允许，向导自动进入下一步。</li>
                    <li><b>绑定设备</b>：填设备名称、选择收款账户，向导自动创建设备记录。</li>
                    <li><b>ADB 连通即上线</b>：USB 直连后设备自动上线，WiFi 备用通道自动配置（断线自动切换保活）。</li>
                  </ol>
                  <el-alert type="info" :closable="false" class="mt8"
                    title="历史变更（2026-08-17）：redroid 容器载体方案已废弃；个人免签监听端统一走 USB 引导接入自动检测，手填「新增设备」表单已移除。" />
                </el-tab-pane>
              </el-tabs>
            </div>
          </el-collapse-item>
        </el-collapse>
      </div>
    </div>

    <div class="pg-card">
      <div class="pg-card-body" v-loading="loading">
        <div class="device-grid">
          <div v-for="d in devices" :key="d.id" class="phone-card" :class="{ 'phone-card-selected': selectedIds.has(d.id) }" @click="batchMode ? toggleSelect(d.id) : openConsole(d)">
            <div v-if="batchMode" class="phone-checkbox">
              <el-checkbox :model-value="selectedIds.has(d.id)" @click.stop @change="toggleSelect(d.id)" />
            </div>
            <!-- 手机壳 -->
            <div class="phone-frame">
              <div class="phone-notch"></div>
              <div class="phone-screen">
                <img v-if="d._cachedSnapshot" :src="d._cachedSnapshot" class="phone-screenshot" />
                <div v-else class="phone-placeholder">
                  <div class="phone-placeholder-icon">📱</div>
                  <div class="phone-placeholder-name">{{ d.name || '未命名' }}</div>
                </div>
              </div>
              <div class="phone-home" @click.stop="openConsole(d)"></div>
            </div>
            <!-- 设备信息 -->
            <div class="phone-info">
              <div class="phone-name">{{ d.name || '未命名' }}</div>
              <div class="phone-meta">
                <el-tag :type="statusTag(d.status)" size="small" effect="light" round>{{ statusText(d.status) }}</el-tag>
                <el-tag :type="d.provider === 'alipay' ? 'primary' : 'success'" size="small" effect="plain" round>{{ d.provider === 'alipay' ? '支付宝' : '微信' }}</el-tag>
                <el-tag v-if="d._accountCount > 1" type="primary" size="small" effect="plain" round>+{{ d._accountCount - 1 }} 账户</el-tag>
                <el-tag v-if="d.adb_host" type="info" size="small" effect="plain" round>USB</el-tag>
                <el-tag v-else-if="d.adb_wifi_host" type="info" size="small" effect="plain" round>WiFi</el-tag>
                <el-tag v-if="d.adb_host && d.adb_wifi_host" type="warning" size="small" effect="plain" round>双通道</el-tag>
                <!-- P2P 控制面状态徽标（docs/P2P-ADB-RECOVERY.md Phase 1） -->
                <el-tag v-if="controlStateBadge(d)" :type="controlStateBadge(d).type" size="small" effect="plain" round>
                  {{ controlStateBadge(d).label }}
                </el-tag>
                <el-tag v-if="d.ts_ip" type="info" size="small" effect="plain" round class="font-mono">{{ d.ts_ip }}</el-tag>
                <span v-if="d._cachedSnapshot" class="snapshot-tag">快照</span>
                <el-tag v-if="setupProgressBadge(d)" type="warning" size="small" effect="plain" round>配置中 {{ setupProgressCount(d) }}/5</el-tag>
              </div>
            </div>
            <!-- 操作 -->
            <div class="phone-actions" @click.stop>
              <el-button size="small" type="primary" text @click="openConsole(d)">控制台</el-button>
              <el-button v-if="d.status !== 'frozen'" size="small" type="danger" text @click="freeze(d)">冻结</el-button>
              <el-button v-else size="small" type="success" text @click="unfreeze(d)">解冻</el-button>
              <el-button size="small" type="danger" text @click="remove(d)">删除</el-button>
              <el-button size="small" type="warning" text :loading="restartLoading[d.id]" @click="restartDevice(d)">重启</el-button>
              <el-button size="small" type="success" text :loading="warmupLoading[d.id]" @click="warmupDevice(d)">预热</el-button>
            </div>
          </div>
          <div v-if="!loading && devices.length === 0" class="empty-state">
            <div style="font-size: 40px; margin-bottom: 12px">📱</div>
            <div class="text-muted">暂无监听端设备</div>
            <el-button type="success" plain style="margin-top: 16px" @click="openWizard">USB 引导接入</el-button>
          </div>
        </div>
        <!-- 批量操作栏 -->
        <div v-if="batchMode" class="batch-actions-bar">
          <div class="batch-actions-left">
            <el-checkbox :model-value="isAllSelected" :indeterminate="selectedCount > 0 && !isAllSelected" @change="selectAll">{{ isAllSelected ? '取消全选' : '全选' }}</el-checkbox>
            <el-button size="small" text @click="clearSelection" :disabled="selectedCount === 0">清空</el-button>
          </div>
          <div class="batch-actions-right">
            <el-button type="warning" size="small" :disabled="selectedCount === 0" :loading="batchLoading === 'restart'" @click="batchRestart">
              批量重启
            </el-button>
            <el-button type="danger" size="small" :disabled="selectedCount === 0" :loading="batchLoading === 'freeze'" @click="batchFreeze">
              批量冻结
            </el-button>
            <el-button type="success" size="small" :disabled="selectedCount === 0" :loading="batchLoading === 'unfreeze'" @click="batchUnfreeze">
              批量解冻
            </el-button>
          </div>
        </div>
      </div>
    </div>

    <el-dialog v-model="consoleVisible" title="设备控制台" width="1100px" top="4vh" destroy-on-close @closed="onConsoleClosed">
      <div v-if="consoleDevice" class="console-wrap">
        <div class="console-header">
          <div class="console-device-info">
            <div class="device-name">{{ consoleDevice.name || '未命名' }}</div>
            <div class="font-mono text-muted" style="font-size: 11px">{{ consoleDevice.device_id }}</div>
            <div class="font-mono text-muted" style="font-size: 11px">USB: {{ consoleDevice.adb_host || '—' }}{{ consoleDevice.adb_port ? ':' + consoleDevice.adb_port : '' }} · WiFi: {{ consoleDevice.adb_wifi_host ? consoleDevice.adb_wifi_host + ':' + (consoleDevice.adb_wifi_port || '—') : '未配置' }}</div>
            <div style="margin-top:4px">
              <el-tag :type="statusTag(consoleDevice.status)" size="small" effect="light" round>{{ statusText(consoleDevice.status) }}</el-tag>
              <el-tag v-if="controlStateBadge(consoleDevice)" :type="controlStateBadge(consoleDevice).type" size="small" effect="plain" round style="margin-left:6px">
                {{ controlStateBadge(consoleDevice).label }}
              </el-tag>
              <el-tag v-if="consoleDevice.ts_ip" type="info" size="small" effect="plain" round class="font-mono" style="margin-left:6px">{{ consoleDevice.ts_ip }}</el-tag>
              <template v-for="a in consoleAccounts" :key="a.account_id">
                <el-tag :type="a.is_primary ? 'primary' : 'info'" size="small" effect="plain" round style="margin-left:6px">
                  {{ a.name }}<template v-if="a.is_primary">（主）</template>
                </el-tag>
              </template>
            </div>
          </div>
          <div class="console-header-actions">
            <el-button size="small" plain @click="openEditAdb">编辑 ADB</el-button>
            <el-button size="small" type="success" plain @click="openAddAccount">绑定账户</el-button>
            <el-button size="small" type="warning" plain @click="openCaptureQr">捕获收款码</el-button>
          </div>
        </div>
        <div v-if="consoleSerials.length" class="console-stream">
          <DeviceStream :serials="consoleSerials" :device-id="consoleDevice.id" />
        </div>
        <el-alert v-else type="warning" :closable="false" title="该设备未配置 ADB 地址，无法实时操作。请编辑设备填写 adb_host/adb_port。" />
      </div>
    </el-dialog>

    <el-dialog v-model="editAdbVisible" title="编辑 ADB 通道" width="480px">
      <el-form label-width="90px">
        <el-form-item label="USB 直连">
          <el-input v-model="editAdbForm.adb_host" placeholder="真机 USB serial，如 4f2bdc30" />
          <div class="text-muted" style="font-size: 11px; width: 100%">真机 USB 直连填 adb serial；adb over WiFi（可选）填 host:port</div>
        </el-form-item>
        <el-form-item label="TCP 端口">
          <el-input v-model.number="editAdbForm.adb_port" placeholder="如 5555（USB 直连留空）" />
        </el-form-item>
        <el-form-item label="WiFi 备用">
          <el-input v-model="editAdbForm.adb_wifi_host" placeholder="同局域网无线调试 IP（可选）" style="width: 45%" />
          <span style="margin: 0 6px; color: var(--text-muted)">:</span>
          <el-input v-model.number="editAdbForm.adb_wifi_port" placeholder="5555" style="width: 45%" />
          <div class="text-muted" style="font-size: 11px; width: 100%">可选：USB 断线后自动切到 WiFi 通道保活</div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="editAdbVisible = false">取消</el-button>
        <el-button type="primary" :loading="editAdbSaving" @click="saveAdb">保存</el-button>
      </template>
    </el-dialog>

    <!-- 绑定更多账户（应用双开多账户：一台设备服务多个收款账户） -->
    <el-dialog v-model="addAccountVisible" title="绑定更多账户（应用双开）" width="520px">
      <el-alert type="info" :closable="false" style="margin-bottom:12px"
        title="一台设备（双开微信/支付宝）可服务多个收款账户。已绑定账户在设备恢复心跳/接单时会同时生效。" />
      <div v-if="consoleAccounts.length" style="margin-bottom:12px">
        <div style="font-size:12px;color:var(--text-muted);margin-bottom:6px">已绑定账户：</div>
        <el-tag v-for="a in consoleAccounts" :key="a.account_id" :type="a.is_primary ? 'primary' : 'info'"
          size="small" effect="plain" round style="margin:0 6px 6px 0" closable
          :closable="!a.is_primary" @close="removeAccount(a)">
          {{ a.name }}{{ a.is_primary ? '（主）' : '' }}
        </el-tag>
      </div>
      <el-form label-width="90px">
        <el-form-item label="添加账户" required>
          <el-select v-model="addAccountForm.account_id" placeholder="选择同通道的个人收款账户" style="width:100%">
            <el-option v-for="a in bindableAccounts" :key="a.id"
              :label="`#${a.id} ${a.name}（${channelName(a.channel)}）`" :value="a.id" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="addAccountVisible = false">取消</el-button>
        <el-button type="primary" :loading="addAccountSaving" :disabled="!addAccountForm.account_id" @click="submitAddAccount">
          绑定
        </el-button>
      </template>
    </el-dialog>

    <!-- 收款码捕获：无码设备也能一键触发服务端 adb 自动化捕获（无需先有收款码） -->
    <el-dialog v-model="captureQrVisible" title="捕获收款码" width="420px">
      <el-alert type="info" :closable="false" class="mb12"
        title="即使设备尚未捕获过收款码也可直接捕获。服务端 adb 会自动打开微信/支付宝并导航到收款码页，请确保设备 adb 在线。" />
      <el-form label-width="90px">
        <el-form-item label="平台" required>
          <el-radio-group v-model="captureForm.platform">
            <el-radio value="alipay">支付宝</el-radio>
            <el-radio value="wechat">微信</el-radio>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="应用空间" required>
          <el-radio-group v-model="captureForm.user">
            <el-radio :value="0">主空间</el-radio>
            <el-radio :value="999">双开</el-radio>
          </el-radio-group>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="captureQrVisible = false">取消</el-button>
        <el-button type="warning" :loading="captureQrLoading" @click="doCaptureQr">
          开始捕获（约 30~90 秒）
        </el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="wizardVisible" title="USB 真机引导接入" width="720px" destroy-on-close @closed="onWizardClosed">
      <el-steps :active="wizardStep" simple style="margin-bottom: 20px">
        <el-step title="连接 USB" />
        <el-step title="授权调试" />
        <el-step title="绑定设备" />
        <el-step title="下发配置" />
      </el-steps>

      <div class="wizard-body">
        <!-- 第 0 步：连接 USB，自动检测设备 -->
        <div v-if="wizardStep === 0">
          <div v-if="usbDetecting" style="text-align:center;padding:16px 0">
            <el-icon class="is-loading" style="font-size:24px"><Loading /></el-icon>
            <div style="margin-top:8px;color:var(--text-muted);font-size:13px">正在检测 USB 设备...</div>
          </div>
          <div v-else-if="usbDevices.length === 0" style="text-align:center;padding:32px 0">
            <div style="font-size:48px;margin-bottom:12px">🔌</div>
            <div style="color:var(--text-muted)">未检测到 USB 真机，请用数据线连接手机并开启开发者选项中的 USB 调试</div>
          </div>
          <div v-else>
            <div style="margin-bottom:12px;color:var(--text-muted);font-size:13px">请选择要接入的设备：</div>
            <div class="usb-device-list">
              <div v-for="dev in usbDevices" :key="dev.serial"
                class="usb-device-item"
                :class="{ 'usb-device-item-selected': selectedUsbSerial === dev.serial }"
                @click="selectUsbDevice(dev)">
                <div class="usb-device-info">
                  <div class="usb-device-serial font-mono">{{ dev.serial }}</div>
                  <div class="usb-device-model text-muted" style="font-size:12px">
                    <el-tag v-if="dev.kind === 'wifi'" type="info" size="small" effect="plain" style="margin-right:4px">WiFi</el-tag>
                    <el-tag v-else type="success" size="small" effect="plain" style="margin-right:4px">USB</el-tag>
                    {{ dev.model || '未知型号' }}
                  </div>
                </div>
                <div class="usb-device-right">
                  <el-tag v-if="dev.state === 'device'" type="success" size="small" effect="plain">可下一步</el-tag>
                  <el-tag v-else-if="dev.state === 'unauthorized'" type="warning" size="small" effect="plain">待授权</el-tag>
                  <el-tag v-else type="info" size="small" effect="plain">连接异常</el-tag>
                  <el-icon v-if="selectedUsbSerial === dev.serial" style="margin-left:6px;color:var(--el-color-success)"><Check /></el-icon>
                </div>
              </div>
            </div>
            <el-alert v-if="selectedUsbSerial && selectedUsbDevice && selectedUsbDevice.state === 'unauthorized'"
              type="warning" :closable="false" style="margin-top:12px"
              title="请在手机弹出的对话框中点击「允许 USB 调试」" />
          </div>
        </div>

        <!-- 第 1 步：等待授权 -->
        <div v-if="wizardStep === 1">
          <div style="text-align:center;padding:24px 0">
            <el-icon class="is-loading" style="font-size:32px"><Loading /></el-icon>
            <div style="margin-top:12px;font-size:15px;font-weight:500">等待手机端授权...</div>
            <div style="margin-top:8px;color:var(--text-muted);font-size:13px">
              请在手机屏幕上点击「允许 USB 调试」
            </div>
            <div v-if="selectedUsbDevice" style="margin-top:16px">
              <el-tag type="info" size="small" effect="plain" class="font-mono">{{ selectedUsbSerial }}</el-tag>
              <el-tag type="info" size="small" effect="plain" style="margin-left:6px">{{ selectedUsbDevice.model || '未知型号' }}</el-tag>
            </div>
            <div v-if="selectedUsbDevice && selectedUsbDevice.state === 'device'" style="margin-top:16px">
              <el-icon style="color:var(--el-color-success);font-size:28px"><CircleCheck /></el-icon>
              <div style="color:var(--el-color-success);margin-top:6px;font-weight:500">授权成功，点击下方「下一步」继续</div>
            </div>
            <div v-else style="margin-top:16px;color:var(--text-muted);font-size:13px">
              若手机未弹出授权框，请断开 USB 重插，或检查「开发者选项 → USB 调试」是否开启
            </div>
          </div>
        </div>

        <!-- 第 2 步：绑定设备 -->
        <div v-if="wizardStep === 2">
          <el-form label-width="90px">
            <el-form-item label="设备名称" required>
              <el-input v-model="wizardForm.name" placeholder="如：卧室安卓机-1" maxlength="30" />
            </el-form-item>
            <el-form-item label="收款账户" required>
              <el-select v-model="wizardForm.account_id" placeholder="选择个人通道收款账户" style="width:100%">
                <el-option v-for="a in personalAccounts" :key="a.id"
                  :label="`#${a.id} ${a.name}`" :value="a.id" />
              </el-select>
            </el-form-item>
          </el-form>
          <el-alert v-if="wizardDeviceId" type="info" :closable="false" style="margin-top:8px"
            title="该设备已绑定，将继续未完成的配置流程" />
        </div>

        <!-- 第 3 步：下发配置 -->
        <div v-if="wizardStep === 3">
          <div v-if="!verifyDone">
            <div style="text-align:center;padding:24px 0">
              <el-button type="primary" size="large" :loading="verifyLoading" @click="doVerify">
                下发并启动
              </el-button>
              <el-alert v-if="verifyError" type="danger" :closable="false" style="margin-top:16px"
                :title="verifyError" />
            </div>
          </div>
          <div v-else style="text-align:center;padding:16px 0">
            <el-icon style="color:var(--el-color-success);font-size:40px"><CircleCheck /></el-icon>
            <div style="margin-top:10px;font-size:15px;font-weight:500">配置下发成功！</div>
            <el-alert v-if="verifyHint" type="info" :closable="false" style="margin-top:12px;text-align:left" :title="verifyHint" />
            <el-alert v-if="verifyWarning" type="warning" :closable="false" style="margin-top:8px;text-align:left" :title="verifyWarning" />
            <el-alert v-if="keepaliveHint" type="warning" :closable="false" show-icon style="margin-top:8px;text-align:left"
              :title="keepaliveHint" />
            <div style="margin-top:20px">
              <el-button type="primary" @click="finishWizard">完成</el-button>
            </div>
          </div>
        </div>
      </div>

      <template #footer>
        <el-button @click="closeWizard">关闭</el-button>
        <el-button v-if="wizardStep > 0 && wizardStep < 3" @click="goBackStep">上一步</el-button>
        <el-button v-if="wizardStep === 0" type="primary" :disabled="!selectedUsbSerial" @click="goStep1">
          下一步
        </el-button>
        <el-button v-if="wizardStep === 1" type="primary" :disabled="!(selectedUsbDevice && selectedUsbDevice.state === 'device')" @click="goStep2">
          下一步
        </el-button>
        <el-button v-if="wizardStep === 2" type="primary" :loading="wizardStartLoading" @click="doWizardStart">
          创建并继续
        </el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Loading, Check, CircleCheck } from '@element-plus/icons-vue'
import { api } from '../api'
import { bumpRefresh } from '../store'
import DeviceStream from '../components/DeviceStream.vue'

const props = defineProps({ refreshKey: { type: Number, default: 0 } })

const devices = ref([])
const loading = ref(false)
const personalAccounts = ref([])

// 设备控制台状态（实时流模式）
const consoleVisible = ref(false)
const consoleDevice = ref(null)
// adb serial 候选列表（按优先级：USB 主通道 → WiFi 备用通道），供 DeviceStream 依次尝试连接。
// USB 断开后 WS 失败会自动切到 WiFi 备用通道恢复画面，无需用户重开控制台。
const consoleSerials = computed(() => {
  const d = consoleDevice.value
  if (!d) return []
  const list = []
  if (d.adb_host) {
    list.push(d.adb_port ? `${d.adb_host}:${d.adb_port}` : d.adb_host) // USB serial 或 host:port
  }
  if (d.adb_wifi_host && d.adb_wifi_port) {
    list.push(`${d.adb_wifi_host}:${d.adb_wifi_port}`)
  }
  return list
})

// 批量模式状态
const batchMode = ref(false)
const selectedIds = ref(new Set())
const isAllSelected = computed(() => devices.value.length > 0 && selectedIds.value.size === devices.value.length)
const selectedCount = computed(() => selectedIds.value.size)

const onlineCount = computed(() => devices.value.filter(d => d.status === 'online').length)

function toggleBatchMode() {
  batchMode.value = !batchMode.value
  if (!batchMode.value) {
    selectedIds.value.clear()
  }
}

function toggleSelect(id) {
  if (selectedIds.value.has(id)) {
    selectedIds.value.delete(id)
  } else {
    selectedIds.value.add(id)
  }
  selectedIds.value = new Set(selectedIds.value) // 触发响应式更新
}

function selectAll() {
  if (isAllSelected.value) {
    selectedIds.value.clear()
  } else {
    selectedIds.value = new Set(devices.value.map(d => d.id))
  }
  selectedIds.value = new Set(selectedIds.value)
}

function clearSelection() {
  selectedIds.value.clear()
  selectedIds.value = new Set(selectedIds.value)
}

function statusTag(s) { return { online: 'success', offline: 'info', frozen: 'danger' }[s] || 'info' }
function statusText(s) { return { online: '在线', offline: '离线', frozen: '已冻结' }[s] || s }
function channelName(ch) { if (!ch) return ''; return ch.includes('wechat') ? '微信' : ch.includes('alipay') ? '支付宝' : ch }
function fmtTime(t) { if (!t) return '—'; return String(t).replace('T', ' ').slice(0, 19) }

// controlStateBadge 把服务端 control_state 映射为 Element Plus 徽标样式。
// 空值/未上线不显示（避免与 status='offline' 重复）。
function controlStateBadge(d) {
  const s = d && d.control_state
  if (!s) return null
  if (s === 'usb') return { type: 'success', label: '控制面:USB' }
  if (s === 'relay') return { type: 'primary', label: '控制面:反代' }
  if (s === 'tunnel_only') return { type: 'info', label: '控制面:隧道' }
  return { type: 'info', label: '控制面:' + s }
}

async function load() {
  loading.value = true
  try {
    const data = await api.personalDevices()
    const oldMap = new Map(devices.value.map(d => [d.id, d]))
    const list = data.devices || []
    // 保留旧的缓存快照（API 不返回截图，用之前缓存过的）
    for (const d of list) {
      const old = oldMap.get(d.id)
      if (old && old._cachedSnapshot) d._cachedSnapshot = old._cachedSnapshot
    }
    devices.value = list
  }
  catch (e) { ElMessage.error('加载设备失败：' + e.message) }
  finally { loading.value = false }
}

async function loadAccounts() {
  try { const data = await api.accounts({}); personalAccounts.value = (data || []).filter(a => String(a.channel).startsWith('personal.')) }
  catch (e) { /* ignore */ }
}

async function freeze(row) {
  await ElMessageBox.confirm(`确认冻结设备「${row.name || row.device_id}」？`, '冻结设备', { type: 'warning' })
  try { await api.freezePersonalDevice(row.id); ElMessage.success('已冻结'); load() }
  catch (e) { ElMessage.error('冻结失败：' + e.message) }
}

async function unfreeze(row) {
  try { await api.unfreezePersonalDevice(row.id); ElMessage.success('已解冻'); load() }
  catch (e) { ElMessage.error('解冻失败：' + e.message) }
}

const restartLoading = ref({})
async function restartDevice(row) {
  await ElMessageBox.confirm(`确认重启设备「${row.name || row.device_id}」？重启可能需要 30-60 秒。`, '重启设备', { type: 'warning' })
  restartLoading.value = { ...restartLoading.value, [row.id]: true }
  try {
    const r = await api.restartPersonalDevice(row.id)
    const stages = (r && r.stages) || []
    const stageText = stages.map(s => `${s.stage}:${s.ok === true ? '成功' : '失败'}`).join(' / ')
    const ok = !!r && r.ok === true
    if (ok) ElMessage.success(`重启完成${stageText ? '（' + stageText + '）' : ''}`)
    else ElMessage.error(`重启失败${stageText ? '（' + stageText + '）' : ''}`)
    setTimeout(() => load(), 8000)
  } catch (e) {
    ElMessage.error('重启失败：' + e.message)
  } finally {
    restartLoading.value = { ...restartLoading.value, [row.id]: false }
  }
}

const warmupLoading = ref({})
async function warmupDevice(row) {
  await ElMessageBox.confirm(`确认预热设备「${row.name || row.device_id}」？将执行冷启动导航到支付宝账本页（约 18 秒）。`, '手动预热', { type: 'info' })
  warmupLoading.value = { ...warmupLoading.value, [row.id]: true }
  try {
    const r = await api.personalDeviceAction(row.id, { action: 'warmup' })
    if (r && r.data && r.data.ok) ElMessage.success('预热完成：账本页已就绪，任务卡片已锁定')
    else ElMessage.error('预热失败')
  } catch (e) {
    ElMessage.error('预热失败：' + e.message)
  } finally {
    warmupLoading.value = { ...warmupLoading.value, [row.id]: false }
  }
}

async function remove(row) {
  await ElMessageBox.confirm(`确认删除设备「${row.name || row.device_id}」？`, '删除设备', { type: 'error' })
  try { await api.deletePersonalDevice(row.id); ElMessage.success('已删除'); load() }
  catch (e) { ElMessage.error('删除失败：' + e.message) }
}

// ===== 设备控制台（实时流模式）=====

function openConsole(dev) {
  consoleDevice.value = dev
  consoleVisible.value = true
  loadConsoleAccounts(dev.id)
}

function onConsoleClosed() {
  consoleDevice.value = null
  consoleAccounts.value = []
}

// ---- 设备 ↔ 账户 多对多绑定（应用双开多账户）----
const consoleAccounts = ref([])
const addAccountVisible = ref(false)
const addAccountSaving = ref(false)
const addAccountForm = ref({ account_id: null })

async function loadConsoleAccounts(deviceId) {
  try {
    const data = await api.personalDeviceAccounts(deviceId)
    consoleAccounts.value = (data && data.accounts) || []
    // 回写卡片账户计数徽章
    const dev = consoleDevice.value
    if (dev && data) dev._accountCount = (data.accounts || []).length
  } catch (e) { /* 忽略 */ }
}

// bindableAccounts 可选绑定的账户：个人通道 + 未绑定（支持跨通道：一台设备可服务微信+支付宝多账户）
const bindableAccounts = computed(() => {
  const bound = new Set(consoleAccounts.value.map(a => a.account_id))
  return personalAccounts.value.filter(a => {
    if (bound.has(a.id)) return false
    return String(a.channel).startsWith('personal.')
  })
})

function openAddAccount() {
  addAccountForm.value = { account_id: null }
  addAccountVisible.value = true
  loadAccounts()
}

async function submitAddAccount() {
  const dev = consoleDevice.value
  if (!dev || !addAccountForm.value.account_id) return
  addAccountSaving.value = true
  try {
    await api.personalAddDeviceAccount(dev.id, { account_id: addAccountForm.value.account_id })
    ElMessage.success('账户已绑定（下次心跳后生效）')
    addAccountVisible.value = false
    await loadConsoleAccounts(dev.id)
    load()
  } catch (e) { ElMessage.error('绑定失败：' + e.message) }
  finally { addAccountSaving.value = false }
}

async function removeAccount(a) {
  const dev = consoleDevice.value
  if (!dev) return
  await ElMessageBox.confirm(`确认解绑账户「${a.name}」？该账户将不再由本设备服务。`, '解绑账户', { type: 'warning' })
  try {
    await api.personalRemoveDeviceAccount(dev.id, a.account_id)
    ElMessage.success('已解绑')
    await loadConsoleAccounts(dev.id)
    load()
  } catch (e) { ElMessage.error('解绑失败：' + e.message) }
}

// ---- 收款码捕获（服务端 adb 自动化，无码设备也能一键触发；复用 /personal/qrcodes/:id/.../capture）----
const captureQrVisible = ref(false)
const captureQrLoading = ref(false)
const captureForm = ref({ platform: 'alipay', user: 0 })

function openCaptureQr() {
  const dev = consoleDevice.value
  if (!dev) return
  captureForm.value = { platform: 'alipay', user: 0 }
  captureQrVisible.value = true
}

async function doCaptureQr() {
  const dev = consoleDevice.value
  if (!dev || !dev.device_id) return
  captureQrLoading.value = true
  try {
    if (captureForm.value.platform === 'wechat') {
      await api.personalCaptureWechatQrcode(dev.device_id, captureForm.value.user)
    } else {
      await api.personalCaptureAlipayQrcode(dev.device_id, captureForm.value.user)
    }
    ElMessage.success('收款码捕获成功')
    captureQrVisible.value = false
    bumpRefresh()
    load()
  } catch (e) {
    ElMessage.error('捕获失败：' + e.message)
  } finally {
    captureQrLoading.value = false
  }
}

// ---- 编辑 ADB 通道（控制台内联修改 adb_host/adb_port/wifi）----
const editAdbVisible = ref(false)
const editAdbSaving = ref(false)
const editAdbForm = ref({ adb_host: '', adb_port: null, adb_wifi_host: '', adb_wifi_port: null })

function openEditAdb() {
  const dev = consoleDevice.value
  if (!dev) return
  editAdbForm.value = {
    adb_host: dev.adb_host || '',
    adb_port: dev.adb_port || null,
    adb_wifi_host: dev.adb_wifi_host || '',
    adb_wifi_port: dev.adb_wifi_port || null,
  }
  editAdbVisible.value = true
}

async function saveAdb() {
  const dev = consoleDevice.value
  if (!dev) return
  editAdbSaving.value = true
  try {
    await api.personalSetAdb(dev.id, editAdbForm.value)
    ElMessage.success('ADB 通道已更新，下次操作生效')
    Object.assign(dev, editAdbForm.value)
    editAdbVisible.value = false
    load()
  } catch (e) {
    ElMessage.error('保存失败：' + e.message)
  } finally {
    editAdbSaving.value = false
  }
}

// ===== USB 引导向导 =====

const wizardVisible = ref(false)
const wizardStep = ref(0)
const usbDetecting = ref(false)
const usbDevices = ref([])
const selectedUsbSerial = ref('')
const selectedUsbDevice = ref(null)
const wizardDeviceId = ref(null)
const wizardForm = ref({ name: '', account_id: null })
const wizardStartLoading = ref(false)
// WiFi 备用通道自动配置状态（绑定后自动触发，无需用户输入）
const wifiBackupState = ref('') // ''=未配置 / running=配置中 / done=已配置 / skipped=跳过(非USB/失败)
const wifiBackupInfo = ref('')
const verifyLoading = ref(false)
const verifyError = ref('')
const verifyDone = ref(false)
const verifyHint = ref('')
const verifyWarning = ref('')
const keepaliveHint = ref('')
let usbPollTimer = null

function setupProgressBadge(d) {
  const sp = d.setup_progress
  if (!sp || !sp.length) return false
  return sp.includes('usb_connected')
}

function setupProgressCount(d) {
  const sp = d.setup_progress
  if (!sp) return 0
  return [...new Set(sp)].length
}

function openWizard() {
  wizardVisible.value = true
  wizardStep.value = 0
  usbDevices.value = []
  selectedUsbSerial.value = ''
  selectedUsbDevice.value = null
  wizardDeviceId.value = null
  wizardForm.value = { name: '', account_id: null }
  wizardStartLoading.value = false
  wifiBackupState.value = ''
  wifiBackupInfo.value = ''
  verifyLoading.value = false
  verifyError.value = ''
  verifyDone.value = false
  verifyHint.value = ''
  verifyWarning.value = ''
  keepaliveHint.value = ''
  loadAccounts()
}

function onWizardClosed() {
  stopUsbPoll()
  wizardDeviceId.value = null
}

function closeWizard() {
  wizardVisible.value = false
}

function stopUsbPoll() {
  if (usbPollTimer !== null) {
    clearInterval(usbPollTimer)
    usbPollTimer = null
  }
}

async function pollUsbDetect() {
  if (!wizardVisible.value) return
  usbDetecting.value = true
  try {
    const data = await api.personalUsbDetect()
    const devs = (data && data.devices) || []
    // 只展示未绑定的新设备
    usbDevices.value = devs.filter(d => !d.bound)
    // 更新选中设备的实时状态（授权状态变化由第 1 步展示 + 手动下一步，避免自动跳步干扰返回导航）
    if (selectedUsbSerial.value) {
      const updated = usbDevices.value.find(d => d.serial === selectedUsbSerial.value)
        || devs.find(d => d.serial === selectedUsbSerial.value)
      if (updated) {
        selectedUsbDevice.value = updated
      }
    }
  } catch {
    // ignore poll errors
  } finally {
    usbDetecting.value = false
  }
}

function selectUsbDevice(dev) {
  selectedUsbSerial.value = dev.serial
  selectedUsbDevice.value = dev
}

function goStep1() {
  if (!selectedUsbSerial.value) return
  wizardStep.value = 1
}

function goStep2() {
  wizardStep.value = 2
}

function goBackStep() {
  stopUsbPoll()
  wizardStep.value -= 1
  // 回到第 0/1 步时恢复轮询（设备列表/授权状态需实时刷新）
  if (wizardStep.value <= 1) {
    pollUsbDetect()
    if (usbPollTimer === null) {
      usbPollTimer = setInterval(pollUsbDetect, 2500)
    }
  }
}

function doWizardStart() {
  if (!wizardForm.value.name.trim()) return ElMessage.warning('请填写设备名称')
  if (!wizardForm.value.account_id) return ElMessage.warning('请选择收款账户')
  wizardStartLoading.value = true
  const payload = {
    serial: selectedUsbSerial.value,
    name: wizardForm.value.name,
    account_id: wizardForm.value.account_id,
  }
  api.personalWizardStart(payload).then(data => {
    wizardDeviceId.value = data.id || data.device_id
    stopUsbPoll() // 绑定完成不再需要设备检测轮询
    // 自动配置 WiFi 备用通道（USB 直连设备全自动，无需用户输入；失败不阻塞向导）
    setupWifiBackup()
    // 直接进入「下发配置」步骤（步骤 3）
    wizardStep.value = 3
  }).catch(e => {
    ElMessage.error('绑定设备失败：' + e.message)
  }).finally(() => {
    wizardStartLoading.value = false
  })
}

// setupWifiBackup 绑定后自动配置 WiFi 备用通道：USB 开启无线 adb + 自动获取设备 IP。
// 用户无感知，第 3 步 UI 展示配置结果（成功显示 ip:port，失败提示不影响 USB 使用）。
function setupWifiBackup() {
  const id = wizardDeviceId.value
  if (!id) return
  wifiBackupState.value = 'running'
  wifiBackupInfo.value = ''
  api.personalWizardWifi(id).then(data => {
    if (data && data.ok) {
      wifiBackupState.value = 'done'
      wifiBackupInfo.value = `${data.wifi_host}:${data.wifi_port}`
    } else {
      wifiBackupState.value = 'skipped'
      wifiBackupInfo.value = (data && data.warning) || '配置失败（USB 直连不受影响）'
    }
  }).catch(() => {
    wifiBackupState.value = 'skipped'
    wifiBackupInfo.value = '配置失败（USB 直连不受影响）'
  })
}

function doVerify() {
  if (!wizardDeviceId.value) return
  verifyLoading.value = true
  verifyError.value = ''
  api.personalWizardVerify(wizardDeviceId.value).then(data => {
    verifyHint.value = data.hint || ''
    verifyWarning.value = data.warning || ''
    keepaliveHint.value = data.keepalive_hint || ''
    verifyDone.value = true
  }).catch(e => {
    verifyError.value = '下发配置失败：' + e.message
  }).finally(() => {
    verifyLoading.value = false
  })
}

function finishWizard() {
  wizardVisible.value = false
  load()
}

// ===== 批量运维 =====
const batchLoading = ref(null)

function buildBatchParams() {
  const ids = Array.from(selectedIds.value)
  if (ids.length === devices.value.length) {
    return { all: true }
  }
  return { ids }
}function summarizeBatchResult(r, actionName) {
  const results = r.results || []
  const total = r.total || results.length
  const missing = r.missing_ids || []
  const okCount = results.filter(x => x.ok).length
  const failCount = results.filter(x => !x.ok).length
  const throttled = results.filter(x => x.throttled)

  let msg = `${actionName}完成：共 ${total} 台\n成功 ${okCount} 台`
  if (failCount > 0) {
    msg += `\n失败 ${failCount} 台：`
    const fails = results.filter(x => !x.ok).slice(0, 5)
    msg += fails.map(x => `${x.device_id}（${x.error || '未知错误'}）`).join('；')
    if (results.filter(x => !x.ok).length > 5) msg += '...'
  }
  if (throttled.length > 0) {
    msg += `\n距上次重启不足 5 分钟已跳过 ${throttled.length} 台`
  }
  if (missing.length > 0) {
    msg += `\n不存在或其他原因跳过 ${missing.length} 台（id: ${missing.join(',')}）`
  }
  return msg
}

async function batchRestart() {
  const count = selectedCount.value
  const isAll = isAllSelected.value
  await ElMessageBox.confirm(
    isAll ? `确认重启全部 ${devices.value.length} 台设备？` : `确认重启选中的 ${count} 台设备？`,
    '批量重启', { type: 'warning' }
  )
  batchLoading.value = 'restart'
  try {
    const params = buildBatchParams()
    const r = await api.personalBatchRestart(params)
    const msg = summarizeBatchResult(r, '重启')
    const hasError = (r.results || []).some(x => !x.ok) || (r.missing_ids || []).length > 0
    if (hasError) ElMessage.warning(msg)
    else ElMessage.success(msg)
    load()
    clearSelection()
    batchMode.value = false
  } catch (e) {
    ElMessage.error('批量重启失败：' + e.message)
  } finally {
    batchLoading.value = null
  }
}

async function batchFreeze() {
  const count = selectedCount.value
  const isAll = isAllSelected.value
  await ElMessageBox.confirm(
    isAll ? `确认冻结全部 ${devices.value.length} 台设备？` : `确认冻结选中的 ${count} 台设备？`,
    '批量冻结', { type: 'warning' }
  )
  batchLoading.value = 'freeze'
  try {
    const params = buildBatchParams()
    const r = await api.personalBatchStatus({ ...params, status: 'frozen' })
    const msg = summarizeBatchResult(r, '冻结')
    const hasError = (r.results || []).some(x => !x.ok) || (r.missing_ids || []).length > 0
    if (hasError) ElMessage.warning(msg)
    else ElMessage.success(msg)
    load()
    clearSelection()
    batchMode.value = false
  } catch (e) {
    ElMessage.error('批量冻结失败：' + e.message)
  } finally {
    batchLoading.value = null
  }
}

async function batchUnfreeze() {
  const count = selectedCount.value
  const isAll = isAllSelected.value
  await ElMessageBox.confirm(
    isAll ? `确认解冻全部 ${devices.value.length} 台设备？` : `确认解冻选中的 ${count} 台设备？`,
    '批量解冻', { type: 'warning' }
  )
  batchLoading.value = 'unfreeze'
  try {
    const params = buildBatchParams()
    const r = await api.personalBatchStatus({ ...params, status: 'offline' })
    const msg = summarizeBatchResult(r, '解冻')
    const hasError = (r.results || []).some(x => !x.ok) || (r.missing_ids || []).length > 0
    if (hasError) ElMessage.warning(msg)
    else ElMessage.success(msg)
    load()
    clearSelection()
    batchMode.value = false
  } catch (e) {
    ElMessage.error('批量解冻失败：' + e.message)
  } finally {
    batchLoading.value = null
  }
}

onMounted(load)
watch(() => props.refreshKey, load)
watch(() => wizardVisible.value, (val) => {
  if (val) {
    pollUsbDetect()
    usbPollTimer = setInterval(pollUsbDetect, 2500)
  } else {
    stopUsbPoll()
  }
})
onUnmounted(stopUsbPoll)
</script>

<style scoped>
.personal-devices-page { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 16px; }
.section-label { font-weight: 600; font-size: 14px; color: var(--text-primary); }
.mb16 { margin-bottom: 16px; }
.header-actions { display: flex; align-items: center; gap: 10px; }
.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
.text-muted { color: var(--text-muted); }

/* 设备卡片网格 */
.device-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 20px; }
.phone-card {
  background: var(--bg-surface); border-radius: 14px; border: 1px solid var(--border);
  padding: 16px 16px 12px; cursor: pointer; transition: box-shadow .2s, transform .2s;
  text-align: center; position: relative;
}
.phone-card:hover { box-shadow: 0 4px 16px rgba(0,0,0,.1); transform: translateY(-2px); }

/* 手机壳 */
.phone-frame {
  width: 100%; max-width: 180px; margin: 0 auto;
  background: #1a1a1a; border-radius: 20px; padding: 6px; position: relative;
  box-shadow: 0 2px 8px rgba(0,0,0,.25);
}
.phone-notch { width: 80px; height: 16px; background: #1a1a1a; border-radius: 0 0 10px 10px; margin: -6px auto 0; }
.phone-screen {
  aspect-ratio: 9 / 16; background: #111; border-radius: 12px; overflow: hidden;
  display: flex; align-items: center; justify-content: center;
  margin: 0; position: relative;
}
.phone-screenshot { width: 100%; height: 100%; object-fit: cover; display: block; }
.phone-placeholder { text-align: center; padding: 24px 8px; color: #555; }
.phone-placeholder-icon { font-size: 32px; margin-bottom: 4px; }
.phone-placeholder-name { font-size: 11px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.phone-home { width: 30px; height: 30px; border: 1.5px solid #333; border-radius: 50%; margin: 4px auto 0; cursor: pointer; }

/* 设备信息 */
.phone-info { margin-top: 10px; }
.phone-name { font-weight: 600; font-size: 13px; color: var(--text-primary); margin-bottom: 4px; }
.phone-meta { display: flex; align-items: center; justify-content: center; gap: 4px; flex-wrap: wrap; }
.snapshot-tag { font-size: 10px; color: var(--text-muted); }

/* 操作按钮 */
.phone-actions { display: flex; justify-content: center; gap: 2px; margin-top: 6px; flex-wrap: wrap; }

/* 空状态 */
.empty-state { grid-column: 1 / -1; text-align: center; padding: 60px 0; }

/* 设备控制台（实时流模式） */
.console-wrap { display: flex; flex-direction: column; gap: 12px; height: 640px; }
.console-header { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
.console-device-info { font-size: 13px; }
.console-stream { flex: 1; min-height: 0; }

/* 集成流程折叠面板 */
.integration-guide { margin-top: 4px; border-top: 1px dashed var(--border); padding-top: 8px; }
.integration-guide :deep(.el-collapse-item__header) { font-weight: 500; }
.integration-guide :deep(.el-collapse-item__wrap) { padding-bottom: 20px; border-bottom: none; }
.integration-guide :deep(.el-collapse-item__content) { padding-bottom: 0; }
.guide-title { margin-right: 8px; color: var(--text-primary); }
.guide-tag { margin-left: auto; }
.guide-content { padding: 12px 0 4px 8px; }
.step-list { margin: 4px 0 0; padding-left: 20px; line-height: 1.8; color: var(--text-primary); }
.step-list li { margin-bottom: 4px; }
.step-list code { background: var(--bg-surface); padding: 1px 6px; border-radius: 4px; font-family: 'JetBrains Mono', Consolas, monospace; font-size: 12px; }
.mt8 { margin-top: 8px; margin-bottom: 16px; }

/* 批量模式 */
.phone-checkbox { position: absolute; top: 8px; left: 8px; z-index: 10; }
.phone-card-selected { border-color: var(--el-color-warning); box-shadow: 0 0 0 2px var(--el-color-warning-light-7); }
.batch-actions-bar {
  display: flex; align-items: center; justify-content: space-between;
  padding: 12px 16px; margin-top: 16px; background: var(--bg-surface);
  border-radius: 8px; border: 1px solid var(--border);
}
.batch-actions-left { display: flex; align-items: center; gap: 12px; }
.batch-actions-right { display: flex; align-items: center; gap: 8px; }

/* USB 引导向导 */
.wizard-body { min-height: 200px; }
.usb-device-list { display: flex; flex-direction: column; gap: 8px; }
.usb-device-item {
  display: flex; align-items: center; justify-content: space-between;
  padding: 12px 16px; border: 1px solid var(--border);
  border-radius: 8px; cursor: pointer;
  transition: border-color .2s, background .2s;
}
.usb-device-item:hover { border-color: var(--el-color-primary); background: var(--el-fill-color-light); }
.usb-device-item-selected { border-color: var(--el-color-success); background: var(--el-color-success-light-9); }
.usb-device-info { display: flex; flex-direction: column; gap: 2px; }
.usb-device-serial { font-size: 13px; color: var(--text-primary); }
.usb-device-right { display: flex; align-items: center; }
</style>