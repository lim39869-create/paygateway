<template>
  <div class="accounts-page">
    <!-- 顶部操作栏 -->
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="section-label">账户池管理</span>
        <div class="header-actions">
          <span class="text-muted" style="font-size: 12px">共 {{ list.length }} 个账户</span>
          <el-button type="primary" size="small" @click="openCreate">
            <el-icon><Plus /></el-icon> 新增账户
          </el-button>
        </div>
      </div>
      <div class="pg-card-body">
        <el-alert type="warning" :closable="false" class="mb16"
          title="密钥加密存储：敏感字段（私钥/API密钥）使用 AES-256-GCM 加密后存入独立 accounts.db，仅管理后台可读写。" />

        <div class="filter-bar mb16">
          <el-select v-model="filterChannel" placeholder="全部通道" clearable size="small" style="width: 180px" @change="load">
            <el-option label="全部通道" value="" />
            <el-option-group label="商家通道（官方 API）">
              <el-option label="支付宝（商家）" value="alipay" />
              <el-option label="微信支付（商家）" value="wechat" />
            </el-option-group>
            <el-option-group label="个人免签（收款码监听）">
              <el-option label="个人·支付宝" value="personal.alipay" />
              <el-option label="个人·微信" value="personal.wechat" />
            </el-option-group>
          </el-select>
          <el-tag size="small" type="info" class="ml8" v-if="filterChannel">{{ channelName(filterChannel) }}</el-tag>
        </div>

        <div v-loading="loading" class="account-grid">
          <div v-for="acct in filteredList" :key="acct.id" class="account-card pg-card">
            <div class="account-head">
              <div class="account-logo" :class="acct.channel">
                <el-icon :size="18"><component :is="acct.channel.includes('wechat') ? 'CircleCheck' : 'CircleCheck'" /></el-icon>
              </div>
              <div class="account-info">
                <div class="account-name">{{ acct.name }}</div>
                <div class="account-channel text-muted">{{ channelName(acct.channel) }}</div>
              </div>
              <el-tag :type="statusTag(acct.status)" size="small" effect="light" round>
                {{ statusText(acct.status) }}
              </el-tag>
            </div>

            <el-descriptions :column="2" size="small" class="mt10" border>
              <template v-if="isPersonal(acct.channel)">
                <el-descriptions-item label="收款码内容" :span="2">
                  <span class="font-mono" style="font-size: 11px" :title="acct.qr_content">{{ acct.qr_content || '—' }}</span>
                </el-descriptions-item>
                <el-descriptions-item label="绑定设备" :span="2">
                  <span class="text-muted" style="font-size: 12px">{{ acct.remark || '未绑定' }}</span>
                </el-descriptions-item>
              </template>
              <template v-else>
                <el-descriptions-item label="AppID" :span="2">
                  <span class="font-mono" style="font-size: 12px">{{ acct.app_id || '—' }}</span>
                </el-descriptions-item>
                <el-descriptions-item label="商户号" v-if="acct.mch_id">
                  <span class="font-mono" style="font-size: 12px">{{ acct.mch_id }}</span>
                </el-descriptions-item>
                <el-descriptions-item label="应用私钥">
                  <span class="font-mono text-muted" style="font-size: 11px">{{ acct.private_key || '—' }}</span>
                </el-descriptions-item>
                <el-descriptions-item label="公钥/API密钥">
                  <span class="font-mono text-muted" style="font-size: 11px">{{ acct.public_key || acct.api_key || '—' }}</span>
                </el-descriptions-item>
              </template>
              <el-descriptions-item label="权重"><el-tag size="small">{{ acct.weight }}</el-tag></el-descriptions-item>
              <el-descriptions-item label="优先级"><el-tag size="small" type="warning">{{ acct.priority }}</el-tag></el-descriptions-item>
              <el-descriptions-item label="日限额">
                <span :class="{ 'text-danger': acct.daily_limit > 0 && (acct.daily_used ?? 0) >= acct.daily_limit }">
                  ¥{{ ((acct.daily_used ?? 0) / 100).toFixed(2) }} / ¥{{ ((acct.daily_limit ?? 0) / 100).toFixed(2) }}
                </span>
              </el-descriptions-item>
              <el-descriptions-item label="自动禁用">
                <el-tag :type="acct.auto_ban ? 'danger' : 'info'" size="small">{{ acct.auto_ban ? '开启' : '关闭' }}</el-tag>
              </el-descriptions-item>
              <el-descriptions-item label="连续失败">
                <span :class="{ 'text-danger': (acct.fail_count ?? 0) >= 3 }">{{ acct.fail_count ?? 0 }} 次</span>
              </el-descriptions-item>
              <el-descriptions-item label="分组"><el-tag size="small" type="info">{{ acct.group }}</el-tag></el-descriptions-item>
              <el-descriptions-item label="环境">
                <el-tag :type="acct.is_prod ? 'danger' : 'warning'" size="small">{{ acct.is_prod ? '生产' : '沙箱' }}</el-tag>
              </el-descriptions-item>
            </el-descriptions>

            <div class="account-actions">
              <el-button size="small" text type="primary" @click="openEdit(acct)">编辑</el-button>
              <el-button size="small" text type="primary" @click="openSecret(acct)">查看/复制完整密钥</el-button>
              <el-button v-if="acct.status === 'normal'" size="small" text type="warning" @click="freeze(acct.id)">冻结</el-button>
              <el-button v-if="acct.status === 'frozen'" size="small" text type="success" @click="unfreeze(acct.id)">解冻</el-button>
              <el-button v-if="acct.status === 'disabled'" size="small" text type="success" @click="enable(acct.id)">启用</el-button>
              <el-button v-if="acct.status === 'normal'" size="small" text type="danger" @click="disable(acct.id)">禁用</el-button>
              <el-button size="small" text type="danger" @click="remove(acct)">删除</el-button>
              <el-button size="small" text @click="showLogs(acct)">日志</el-button>
            </div>
          </div>

          <div v-if="!filteredList.length && !loading" class="empty-card pg-card">
            <el-empty description="账户池为空，请按以下步骤配置第一个支付账户" :image-size="80" />
            <el-steps :active="1" align-center class="setup-steps">
              <el-step title="准备密钥" description="在支付宝开放平台/微信商户平台获取 AppID、私钥、公钥" />
              <el-step title="点击「新增账户」" description="选择通道并粘贴密钥，保存后 AES-256-GCM 加密入库" />
              <el-step title="通道即刻生效" description="无需重启，账户自动进入账户池参与下单调度" />
            </el-steps>
            <el-alert type="info" :closable="false" class="mt12"
              title="已移除 .env 密钥迁移：支付密钥只通过本界面配置，加密存储于独立 accounts.db，不再依赖环境变量。" />
          </div>
        </div>
      </div>
    </div>

    <!-- 新增/编辑对话框 -->
    <el-dialog v-model="dialogVisible" :title="isEdit ? '编辑账户' : '新增账户'" width="600px" :close-on-click-modal="false">
      <el-form :model="form" label-width="100px" size="small">
        <el-form-item label="通道" required>
          <el-select v-model="form.channel" placeholder="选择通道" style="width: 100%">
            <el-option label="支付宝（商家）" value="alipay" />
            <el-option label="微信支付（商家）" value="wechat" />
            <el-option label="个人免签·支付宝" value="personal.alipay" />
            <el-option label="个人免签·微信" value="personal.wechat" />
          </el-select>
        </el-form-item>
        <el-form-item label="账户名称" required>
          <el-input v-model="form.name" placeholder="例：支付宝-主账户 / 个人码-支付宝-测试1" />
        </el-form-item>
        <template v-if="!isPersonal(form.channel)">
          <el-row :gutter="12">
            <el-col :span="12">
              <el-form-item label="AppID" required>
                <el-input v-model="form.app_id" placeholder="支付宝 APPID / 微信 AppID" />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="商户号">
                <el-input v-model="form.mch_id" placeholder="微信商户号" />
              </el-form-item>
            </el-col>
          </el-row>
          <el-form-item label="应用私钥" required>
            <el-input v-model="form.private_key" type="textarea" :rows="3"
              :placeholder="isEdit && form.private_key_configured ? '已配置（留空保持不变）' : '粘贴应用私钥（AES-256-GCM 加密存储）'" />
            <el-tag v-if="isEdit && form.private_key_configured" size="small" type="success">已配置</el-tag>
          </el-form-item>
          <el-form-item label="公钥/API密钥">
            <el-input v-model="form.public_key" type="textarea" :rows="2"
              :placeholder="isEdit && (form.public_key_configured || form.api_key_configured) ? '已配置（留空保持不变）' : '支付宝公钥 或 微信 API 密钥（加密存储）'" />
            <el-tag v-if="isEdit && (form.public_key_configured || form.api_key_configured)" size="small" type="success">已配置</el-tag>
          </el-form-item>
        </template>
        <el-form-item v-if="isPersonal(form.channel)" label="收款码内容" :required="!isEdit">
          <el-input v-model="form.qr_content" type="textarea" :rows="2"
            :placeholder="isEdit && form.qr_content_configured ? '已配置（留空保持不变）' : '收款码内容，如 https://qr.alipay.com/xxx'" />
          <el-tag v-if="isEdit && form.qr_content_configured" size="small" type="success">已配置</el-tag>
          <div class="qr-upload-row">
            <el-upload :show-file-list="false" :before-upload="handleQRUpload" accept="image/png,image/jpeg,image/gif" :disabled="decoding">
              <el-button type="primary" plain size="small" :loading="decoding">
                <el-icon><Picture /></el-icon> 上传收款码图片自动解析
              </el-button>
            </el-upload>
            <span class="text-muted" style="font-size: 12px">从支付宝/微信 App「保存收款码」导出图片，上传后自动识别</span>
          </div>
        </el-form-item>
        <el-row :gutter="12">
          <el-col :span="8">
            <el-form-item label="权重">
              <el-input-number v-model="form.weight" :min="1" :max="999" style="width: 100%" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="优先级">
              <el-input-number v-model="form.priority" :min="0" :max="99" style="width: 100%" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="日限额(元)">
              <el-input-number v-model="form.dailyLimitYuan" :min="0" :step="100" style="width: 100%" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="12">
          <el-col :span="8">
            <el-form-item label="自动禁用">
              <el-switch v-model="form.auto_ban" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="生产环境">
              <el-switch v-model="form.is_prod" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="分组">
              <el-input v-model="form.group" placeholder="default" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="备注">
          <el-input v-model="form.remark" type="textarea" :rows="2" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">{{ isEdit ? '保存' : '创建' }}</el-button>
      </template>
    </el-dialog>

    <!-- 完整密钥对话框 -->
    <el-dialog v-model="secretVisible" title="完整密钥" width="640px">
      <el-alert type="warning" :closable="false" class="mb16"
        title="以下为明文密钥，请勿泄露或截图分享。复制后请及时关闭。" />
      <div v-loading="secretLoading" class="secret-box">
        <template v-if="secretData">
          <div v-if="isPersonal(secretData.channel)" class="secret-item">
            <div class="secret-label">收款码内容</div>
            <div class="secret-value">
              <span class="font-mono" style="word-break: break-all">{{ secretData.qr_content || '—' }}</span>
              <el-button v-if="secretData.qr_content" size="small" text type="primary" @click="copySecret(secretData.qr_content)">复制</el-button>
            </div>
          </div>
          <template v-else>
            <div class="secret-item">
              <div class="secret-label">AppID</div>
              <div class="secret-value">
                <span class="font-mono">{{ secretData.app_id || '—' }}</span>
                <el-button v-if="secretData.app_id" size="small" text type="primary" @click="copySecret(secretData.app_id)">复制</el-button>
              </div>
            </div>
            <div v-if="secretData.mch_id" class="secret-item">
              <div class="secret-label">商户号</div>
              <div class="secret-value">
                <span class="font-mono">{{ secretData.mch_id }}</span>
                <el-button size="small" text type="primary" @click="copySecret(secretData.mch_id)">复制</el-button>
              </div>
            </div>
            <div class="secret-item">
              <div class="secret-label">应用私钥</div>
              <div class="secret-value">
                <span class="font-mono text-muted">{{ secretData.private_key || '（未配置）' }}</span>
                <el-button v-if="secretData.private_key" size="small" text type="primary" @click="copySecret(secretData.private_key)">复制</el-button>
              </div>
            </div>
            <div class="secret-item">
              <div class="secret-label">公钥/API密钥</div>
              <div class="secret-value">
                <span class="font-mono text-muted">{{ secretData.public_key || secretData.api_key || '（未配置）' }}</span>
                <el-button v-if="secretData.public_key || secretData.api_key" size="small" text type="primary"
                  @click="copySecret(secretData.public_key || secretData.api_key)">复制</el-button>
              </div>
            </div>
          </template>
        </template>
      </div>
    </el-dialog>

    <!-- 操作日志对话框 -->
    <el-dialog v-model="logVisible" title="账户操作日志" width="600px">
      <div v-loading="logLoading">
        <el-timeline v-if="logs.length">
          <el-timeline-item v-for="log in logs" :key="log.id" :timestamp="log.created_at" placement="top">
            <div class="log-item">
              <el-tag :type="logActionTag(log.action)" size="small">{{ log.action }}</el-tag>
              <span class="log-detail">{{ log.detail }}</span>
              <span class="log-operator text-muted">— {{ log.operator || '系统' }}</span>
            </div>
          </el-timeline-item>
        </el-timeline>
        <el-empty v-else description="暂无操作日志" :image-size="60" />
      </div>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Picture } from '@element-plus/icons-vue'
import { api } from '../api'

const props = defineProps({ refreshKey: Number })

const list = ref([])
const loading = ref(false)
const filterChannel = ref('')
const dialogVisible = ref(false)
const isEdit = ref(false)
const saving = ref(false)
const form = ref(createEmptyForm())
const logVisible = ref(false)
const logLoading = ref(false)
const logs = ref([])
const secretVisible = ref(false)
const secretLoading = ref(false)
const secretData = ref(null)

watch(() => props.refreshKey, load)

function createEmptyForm() {
  return {
    id: 0, channel: 'alipay', name: '', app_id: '', mch_id: '',
    private_key: '', public_key: '', api_key: '', qr_content: '',
    private_key_configured: false, public_key_configured: false, api_key_configured: false, qr_content_configured: false,
    weight: 50, priority: 0, auto_ban: true, dailyLimitYuan: 1000,
    group: 'default', is_prod: false, remark: ''
  }
}

const filteredList = computed(() => {
  if (!filterChannel.value) return list.value
  return list.value.filter(a => a.channel === filterChannel.value)
})

async function load() {
  loading.value = true
  try { list.value = await api.accounts({ channel: filterChannel.value || undefined }) }
  catch (e) { ElMessage.error('加载失败: ' + e.message) }
  finally { loading.value = false }
}

function channelName(name) {
  return { alipay: '支付宝（商家）', wechat: '微信支付（商家）', 'personal.alipay': '个人免签·支付宝', 'personal.wechat': '个人免签·微信', 'personal.qq': '个人免签·QQ' }[name] || name
}

function statusTag(s) { return { normal: 'success', cooling: 'warning', frozen: 'danger', disabled: 'info' }[s] || 'info' }
function statusText(s) { return { normal: '正常', cooling: '冷却中', frozen: '已冻结', disabled: '已禁用' }[s] || s }
function logActionTag(a) { return { create: 'success', update: 'primary', delete: 'danger', freeze: 'warning', unfreeze: 'success', enable: 'success', disable: 'info' }[a] || 'info' }
function isPersonal(channel) { return String(channel).startsWith('personal.') }

function openCreate() { isEdit.value = false; form.value = createEmptyForm(); dialogVisible.value = true }

function openEdit(acct) {
  isEdit.value = true
  form.value = {
    id: acct.id, channel: acct.channel, name: acct.name, app_id: acct.app_id, mch_id: acct.mch_id,
    private_key: '', public_key: '', api_key: '',
    private_key_configured: !!acct.private_key, public_key_configured: !!acct.public_key, api_key_configured: !!acct.api_key, qr_content_configured: !!acct.qr_content,
    weight: acct.weight, priority: acct.priority, auto_ban: acct.auto_ban,
    dailyLimitYuan: acct.daily_limit / 100, group: acct.group, is_prod: acct.is_prod,
    remark: acct.remark, qr_content: ''
  }
  dialogVisible.value = true
}

async function openSecret(acct) {
  try {
    await ElMessageBox.confirm(
      `将显示账户「${acct.name}」的完整明文密钥（含私钥），请确认当前环境安全。`,
      '查看完整密钥', { confirmButtonText: '确认查看', cancelButtonText: '取消', type: 'warning' }
    )
  } catch (e) { return }
  secretVisible.value = true
  secretLoading.value = true
  secretData.value = null
  try { secretData.value = await api.accountSecret(acct.id) }
  catch (e) { secretVisible.value = false; ElMessage.error('获取完整密钥失败: ' + e.message) }
  finally { secretLoading.value = false }
}

async function copySecret(text) {
  try { await navigator.clipboard.writeText(text); ElMessage.success('已复制完整密钥') }
  catch (e) { ElMessage.error('复制失败: ' + e.message) }
}

const decoding = ref(false)
async function handleQRUpload(file) {
  decoding.value = true
  try {
    const content = await api.decodeQRCode(file)
    if (content) { form.value.qr_content = content; ElMessage.success('收款码解析成功') }
    else { ElMessage.warning('未识别到二维码内容') }
  } catch (e) { ElMessage.error('解析失败：' + e.message) }
  finally { decoding.value = false }
  return false
}

async function save() {
  if (!form.value.channel || !form.value.name) { ElMessage.warning('请填写通道和账户名称'); return }
  if (isPersonal(form.value.channel) && !isEdit.value && !form.value.qr_content.trim()) { ElMessage.warning('请填写收款码内容，或上传收款码图片自动解析'); return }
  saving.value = true
  try {
    const data = {
      channel: form.value.channel, name: form.value.name, app_id: form.value.app_id, mch_id: form.value.mch_id,
      private_key: form.value.private_key, public_key: form.value.public_key, api_key: form.value.api_key,
      qr_content: form.value.qr_content, weight: form.value.weight, priority: form.value.priority,
      auto_ban: form.value.auto_ban, daily_limit: Math.round(form.value.dailyLimitYuan * 100),
      group: form.value.group || 'default', is_prod: form.value.is_prod, remark: form.value.remark
    }
    if (isEdit.value) { await api.updateAccount(form.value.id, data); ElMessage.success('账户已更新') }
    else { await api.createAccount(data); ElMessage.success('账户创建成功') }
    dialogVisible.value = false; load()
  } catch (e) { ElMessage.error('保存失败: ' + e.message) }
  finally { saving.value = false }
}

async function freeze(id) { try { await api.freezeAccount(id); ElMessage.success('已冻结'); load() } catch (e) { ElMessage.error(e.message) } }
async function unfreeze(id) { try { await api.unfreezeAccount(id); ElMessage.success('已解冻'); load() } catch (e) { ElMessage.error(e.message) } }
async function enable(id) { try { await api.enableAccount(id); ElMessage.success('已启用'); load() } catch (e) { ElMessage.error(e.message) } }
async function disable(id) { try { await api.disableAccount(id); ElMessage.success('已禁用'); load() } catch (e) { ElMessage.error(e.message) } }
async function remove(acct) {
  try {
    await ElMessageBox.confirm(`确定删除账户「${acct.name}」？此操作不可恢复。`, '确认删除', { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' })
    await api.deleteAccount(acct.id); ElMessage.success('已删除'); load()
  } catch (e) { if (e !== 'cancel') ElMessage.error(e.message) }
}

async function showLogs(acct) {
  logVisible.value = true; logLoading.value = true
  try { logs.value = await api.accountLogs(acct.id, 30) }
  catch (e) { ElMessage.error('加载日志失败: ' + e.message) }
  finally { logLoading.value = false }
}

onMounted(load)
</script>

<style scoped>
.accounts-page { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 16px; }
.section-label { font-weight: 590; font-size: 13px; color: var(--text-primary); }
.account-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(380px, 1fr)); gap: 12px; }
.account-card { padding: 18px; cursor: default; }
.account-card:hover { border-color: var(--border-strong); }

.account-head { display: flex; align-items: center; gap: 10px; }
.account-logo {
  width: 40px; height: 40px; border-radius: 10px;
  display: flex; align-items: center; justify-content: center;
  font-size: 18px; font-weight: 700; color: #fff;
  background: linear-gradient(135deg, #5c6ac4, #7c7fe4);
  flex-shrink: 0;
}
.account-logo.wechat { background: linear-gradient(135deg, #07c160, #3ddc84); }
.account-info { flex: 1; min-width: 0; }
.account-name { font-size: 14px; font-weight: 600; color: var(--text-primary); }
.account-channel { font-size: 11px; }

.account-actions {
  display: flex; gap: 2px; flex-wrap: wrap;
  margin-top: 14px; padding-top: 12px;
  border-top: 1px solid var(--border);
}

.filter-bar { display: flex; gap: 10px; align-items: center; }
.header-actions { display: flex; align-items: center; gap: 10px; }
.mt10 { margin-top: 10px; }
.mt12 { margin-top: 12px; }
.ml8 { margin-left: 8px; }
.mb16 { margin-bottom: 16px; }

.log-item { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.log-detail { font-size: 13px; }
.log-operator { font-size: 11px; }
.text-muted { color: var(--text-muted); }
.text-danger { color: var(--danger); }
.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
.qr-upload-row { display: flex; align-items: center; gap: 10px; margin-top: 8px; flex-wrap: wrap; }
.secret-box { display: flex; flex-direction: column; gap: 10px; }
.secret-item {
  background: var(--bg-subtle);
  border-radius: 6px;
  padding: 10px 12px;
}
.secret-label { font-size: 11px; color: var(--text-muted); margin-bottom: 6px; font-weight: 510; text-transform: uppercase; letter-spacing: 0.04em; }
.secret-value { font-size: 13px; display: flex; align-items: center; gap: 10px; }
.secret-value .font-mono { flex: 1; min-width: 0; font-size: 12px; }
</style>