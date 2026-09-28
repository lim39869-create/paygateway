<template>
  <div class="upstreams-page">
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="section-label">上游对接方管理（多商户）</span>
        <div class="header-actions">
          <span class="text-muted" style="font-size: 12px">共 {{ list.length }} 个上游</span>
          <el-button type="primary" size="small" @click="openCreate">
            <el-icon><Plus /></el-icon> 新增上游
          </el-button>
        </div>
      </div>
      <div class="pg-card-body">
        <el-alert type="info" :closable="false" class="mb16"
          title="每个上游拥有独立的易支付 pid + 密钥：下单验签、回调签名均按 pid 路由，互不伪造。" />

        <div v-loading="loading" class="upstream-grid">
          <div v-for="up in list" :key="up.id" class="upstream-card pg-card">
            <div class="upstream-head">
              <div class="upstream-logo">{{ up.name.charAt(0).toUpperCase() }}</div>
              <div class="upstream-info">
                <div class="upstream-name">{{ up.name }}</div>
                <div class="upstream-pid font-mono text-muted">pid: {{ up.pid }}</div>
              </div>
              <el-tag :type="up.status === 'enabled' ? 'success' : 'info'" size="small" effect="light" round>
                {{ up.status === 'enabled' ? '启用' : '禁用' }}
              </el-tag>
            </div>

            <el-descriptions :column="1" size="small" class="mt10" border>
              <el-descriptions-item label="服务地址">
                <span class="font-mono" style="font-size: 12px">{{ up.base_url || '—' }}</span>
              </el-descriptions-item>
              <el-descriptions-item label="商户密钥">
                <span class="font-mono text-muted" style="font-size: 12px">{{ up.key || '—' }}</span>
              </el-descriptions-item>
              <el-descriptions-item label="回调域名白名单">
                <span class="font-mono text-muted" style="font-size: 12px">{{ up.notify_domains || '不限' }}</span>
              </el-descriptions-item>
              <el-descriptions-item label="备注">
                <span style="font-size: 12px">{{ up.remark || '—' }}</span>
              </el-descriptions-item>
            </el-descriptions>

            <div class="upstream-actions">
              <el-button size="small" text type="primary" @click="openEdit(up)">编辑</el-button>
              <el-button v-if="up.status === 'disabled'" size="small" text type="success" @click="enable(up.id)">启用</el-button>
              <el-button v-if="up.status === 'enabled'" size="small" text type="warning" @click="disable(up.id)">禁用</el-button>
              <el-button size="small" text type="danger" @click="remove(up)">删除</el-button>
              <el-button size="small" text @click="showGuide(up)">对接指引</el-button>
            </div>
          </div>

          <div v-if="!list.length && !loading" class="empty-card pg-card">
            <el-empty description="暂无上游配置，系统已自动创建 default" :image-size="80" />
            <el-steps :active="1" align-center class="setup-steps">
              <el-step title="新增上游" description="为每个对接项目创建独立条目（如 new-api）" />
              <el-step title="分配 pid + 密钥" description="保存后立即生效，无需重启；密钥加密入库" />
              <el-step title="告知上游" description="把对接信息配置到上游项目的易支付设置" />
            </el-steps>
          </div>
        </div>
      </div>
    </div>

    <!-- 新增/编辑对话框 -->
    <el-dialog v-model="dialogVisible" :title="isEdit ? '编辑上游' : '新增上游'" width="560px" :close-on-click-modal="false">
      <el-form :model="form" label-width="120px" size="small">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="例：new-api" />
        </el-form-item>
        <el-form-item label="商户ID (pid)" required>
          <el-input v-model="form.pid" placeholder="易支付商户ID，唯一" />
        </el-form-item>
        <el-form-item label="易支付密钥">
          <el-input v-model="form.key" type="textarea" :rows="2"
            :placeholder="isEdit ? '留空表示不修改' : '粘贴易支付密钥（AES-256-GCM 加密存储）'" show-password />
        </el-form-item>
        <el-form-item label="服务地址">
          <el-input v-model="form.base_url" placeholder="https://new-api.example.com" />
        </el-form-item>
        <el-form-item label-width="0" style="margin-top:-10px">
          <el-alert type="warning" :closable="false" show-icon
            title="该地址必须是这个上游商户自己的公网域名！" />
          <div class="help-text" style="margin-top:6px">
            填<b>该商户站点</b>（如 new-api）用户实际打开的公网地址——支付完成后浏览器会
            <b>跳回这里</b>（网关在商户传来的 return_url 是内网地址时用它兜底重写）。
            例：new-api 部署在 <code>https://new-api.example.com</code>，就填它。
            不要填网关自己的域名（paygateway 的），也不要填 <code>localhost</code>/内网。
          </div>
        </el-form-item>
        <el-form-item label="回调域名白名单">
          <el-input v-model="form.notify_domains" placeholder="可选，逗号分隔：api.example.com,example.org" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="form.remark" type="textarea" :rows="2" placeholder="例：New API 充值+订阅" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">{{ isEdit ? '保存' : '创建' }}</el-button>
      </template>
    </el-dialog>

    <!-- 对接指引对话框 -->
    <el-dialog v-model="guideVisible" title="对接指引" width="620px">
      <div v-if="current" class="guide-box" v-loading="guideLoading">
        <el-alert type="info" :closable="false" class="mb16" show-icon
          title="对接逻辑：pid 和密钥都由「上游项目」配置，paygateway 这里只是登记相同的值用于验签；只有回调端点是上游需要填的。" />

        <div class="guide-steps mb16">
          <div class="guide-step">
            <div class="guide-step-num">①</div>
            <div class="guide-step-body">
              <div class="guide-step-title">在上游项目（如 New API）配置易支付</div>
              <div class="guide-step-desc">
                <div>· pid、密钥：<b>由上游项目自己配置/生成</b>，把这两个值记下来</div>
                <div>· 网关地址 / 下单接口：填 <code>{{ (current.base_url || 'http://<host>:8080') + '/submit.php' }}</code></div>
                <div>· 查询接口：填 <code>{{ (current.base_url || 'http://<host>:8080') + '/api.php' }}</code></div>
                <div class="guide-note">
                  ⚠️ 上述网关地址必须用<b>公网域名</b>（如 <code>https://paygateway.de5.net</code>），
                  不能填 <code>localhost</code>/<code>127.0.0.1</code>/内网 IP——否则支付界面在其他机器上打不开。
                </div>
              </div>
            </div>
          </div>
          <div class="guide-step">
            <div class="guide-step-num">②</div>
            <div class="guide-step-body">
              <div class="guide-step-title">把上游的 pid 和密钥登记到这里（本页上游配置）</div>
              <div class="guide-step-desc">
                编辑本上游，把上一步记下的 <b>pid</b> 和 <b>密钥</b> 填进来。两边的值必须<b>完全一致</b>，paygateway 才能验签通过。
              </div>
            </div>
          </div>
          <div class="guide-step">
            <div class="guide-step-num">③</div>
            <div class="guide-step-body">
              <div class="guide-step-title">上游项目填回调端点 notify_url</div>
              <div class="guide-step-desc">
                <div>这是<b>上游需要配置</b>的唯一回调项，指向 paygateway 的回调地址：</div>
                <div class="guide-callback font-mono">{{ guideNotify || '（启动后自动生成，见「支付通道」页）' }}</div>
              </div>
            </div>
          </div>
        </div>

        <el-divider>当前登记的 pid / 密钥</el-divider>
        <el-alert v-if="!guideLoading" type="warning" :closable="false" class="mb16"
          title="密钥为明文展示，复制后请勿泄露。" />
        <el-descriptions :column="1" size="small" border>
          <el-descriptions-item label="商户ID (pid)"><span class="font-mono">{{ current.pid }}</span></el-descriptions-item>
          <el-descriptions-item label="商户密钥">
            <span class="font-mono" style="word-break: break-all">{{ guideKey || current.key }}</span>
            <el-button size="small" text type="primary" @click="copyGuide">复制</el-button>
          </el-descriptions-item>
          <el-descriptions-item label="网关地址">
            <span class="font-mono" style="font-size: 12px">{{ current.base_url || '（未配置）' }}</span>
          </el-descriptions-item>
        </el-descriptions>
      </div>
    </el-dialog>

    <!-- 操作日志对话框 -->
    <el-dialog v-model="logVisible" title="上游操作日志" width="600px">
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
import { ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { api } from '../api'

const props = defineProps({ refreshKey: Number })

const list = ref([])
const loading = ref(false)
const dialogVisible = ref(false)
const isEdit = ref(false)
const saving = ref(false)
const form = ref(createEmptyForm())
const guideVisible = ref(false)
const guideLoading = ref(false)
const guideKey = ref('')
const guideNotify = ref('')
const current = ref(null)
const logVisible = ref(false)
const logLoading = ref(false)
const logs = ref([])

watch(() => props.refreshKey, load)

function createEmptyForm() {
  return { id: 0, name: '', pid: '', key: '', base_url: '', notify_domains: '', remark: '' }
}

async function load() {
  loading.value = true
  try { list.value = await api.upstreams() }
  catch (e) { ElMessage.error('加载失败: ' + e.message) }
  finally { loading.value = false }
}

function logActionTag(a) { return { create: 'success', update: 'primary', delete: 'danger', enable: 'success', disable: 'info' }[a] || 'info' }

function openCreate() { isEdit.value = false; form.value = createEmptyForm(); dialogVisible.value = true }
function openEdit(up) {
  isEdit.value = true
  form.value = { id: up.id, name: up.name, pid: up.pid, key: '', base_url: up.base_url, notify_domains: up.notify_domains, remark: up.remark }
  dialogVisible.value = true
}

async function save() {
  if (!form.value.name || !form.value.pid) { ElMessage.warning('请填写名称和商户ID (pid)'); return }
  saving.value = true
  try {
    const data = { name: form.value.name, pid: form.value.pid, key: form.value.key, base_url: form.value.base_url, notify_domains: form.value.notify_domains, remark: form.value.remark }
    if (isEdit.value) { await api.updateUpstream(form.value.id, data); ElMessage.success('上游已更新') }
    else { await api.createUpstream(data); ElMessage.success('上游创建成功') }
    dialogVisible.value = false; load()
  } catch (e) { ElMessage.error('保存失败: ' + e.message) }
  finally { saving.value = false }
}

async function enable(id) { try { await api.enableUpstream(id); ElMessage.success('已启用'); load() } catch (e) { ElMessage.error(e.message) } }
async function disable(id) { try { await api.disableUpstream(id); ElMessage.success('已禁用'); load() } catch (e) { ElMessage.error(e.message) } }
async function remove(up) {
  try {
    await ElMessageBox.confirm(`确定删除上游「${up.name}」？`, '确认删除', { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' })
    await api.deleteUpstream(up.id); ElMessage.success('已删除'); load()
  } catch (e) { if (e !== 'cancel') ElMessage.error(e.message) }
}

function showGuide(up) {
  ElMessageBox.confirm(
    `将显示上游「${up.name}」的完整明文密钥，请确认当前环境安全。`,
    '查看完整密钥', { confirmButtonText: '确认查看', cancelButtonText: '取消', type: 'warning' }
  ).then(async () => {
    current.value = up
    guideKey.value = ''
    guideNotify.value = ''
    guideVisible.value = true
    guideLoading.value = true
    try {
      const sec = await api.upstreamSecret(up.id)
      guideKey.value = sec.key || ''
      const ei = await api.epayInfo()
      guideNotify.value = (ei?.submit_url || '').replace('/submit.php', '/alipay/notify')
    } catch (e) { ElMessage.error('获取完整密钥失败: ' + e.message) }
    finally { guideLoading.value = false }
  }).catch(() => {})
}

async function copyGuide() {
  if (!current.value) return
  const key = guideKey.value || current.value.key
  const text = `商户ID (pid): ${current.value.pid}\n商户密钥: ${key}\n网关地址: ${current.value.base_url || ''}\n下单接口: ${(current.value.base_url || '') + '/submit.php'}\n查询接口: ${(current.value.base_url || '') + '/api.php'}\n回调端点 notify_url: ${guideNotify.value || ''}`
  try { await navigator.clipboard.writeText(text); ElMessage.success('已复制') }
  catch (e) { ElMessage.error('复制失败: ' + e.message) }
}

async function showLogs(up) {
  logVisible.value = true; logLoading.value = true
  try { logs.value = await api.upstreamLogs(up.id, 30) }
  catch (e) { ElMessage.error(e.message) }
  finally { logLoading.value = false }
}

load()
</script>

<style scoped>
.upstreams-page { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 16px; }
.section-label { font-weight: 590; font-size: 13px; color: var(--text-primary); }
.mb16 { margin-bottom: 16px; }
.mt10 { margin-top: 10px; }

.upstream-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 12px; }
.upstream-card { padding: 16px; cursor: default; }
.upstream-card:hover { border-color: var(--border-strong); }

.upstream-head { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }
.upstream-logo {
  width: 36px; height: 36px; border-radius: 9px;
  background: var(--accent-gradient);
  color: #fff; font-weight: 700; font-size: 16px;
  display: flex; align-items: center; justify-content: center;
  flex-shrink: 0;
}
.upstream-info { flex: 1; min-width: 0; }
.upstream-name { font-weight: 600; font-size: 14px; color: var(--text-primary); }
.upstream-pid { font-size: 11px; margin-top: 2px; }

.upstream-actions {
  display: flex; gap: 2px; margin-top: 12px;
  flex-wrap: wrap; border-top: 1px solid var(--border); padding-top: 10px;
}

.log-item { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.log-detail { font-size: 13px; }
.log-operator { font-size: 11px; }
.text-muted { color: var(--text-muted); }
.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
.header-actions { display: flex; align-items: center; gap: 10px; }
.setup-steps { margin-top: 16px; }

/* 对接指引步骤 */
.guide-steps { display: flex; flex-direction: column; gap: 14px; }
.guide-step { display: flex; gap: 12px; }
.guide-step-num {
  width: 24px; height: 24px; border-radius: 50%;
  background: var(--accent); color: #fff;
  display: flex; align-items: center; justify-content: center;
  font-size: 12px; font-weight: 590; flex-shrink: 0; margin-top: 1px;
}
.guide-step-body { flex: 1; min-width: 0; }
.guide-step-title { font-weight: 590; font-size: 13px; color: var(--text-primary); margin-bottom: 4px; }
.guide-step-desc { font-size: 12px; color: var(--text-body); line-height: 1.7; }
.guide-step-desc code {
  background: var(--bg-subtle); padding: 1px 6px; border-radius: 4px;
  font-family: 'JetBrains Mono', Consolas, monospace; font-size: 11px;
}
.guide-callback {
  margin-top: 6px; padding: 8px 10px; background: var(--bg-subtle);
  border: 1px solid var(--border); border-radius: 6px;
  font-size: 12px; color: var(--text-primary); word-break: break-all;
}
.guide-note {
  margin-top: 8px; padding: 8px 10px; background: var(--warning-bg, #fff7e6);
  border: 1px solid var(--warning, #e6a23c); border-radius: 6px;
  font-size: 12px; color: #b88230; line-height: 1.7;
}
.guide-note code { background: rgba(0,0,0,0.06); padding: 1px 5px; border-radius: 4px; }
.help-text { font-size: 12px; color: var(--text-muted); line-height: 1.7; }
.help-text code { background: var(--bg-subtle); padding: 1px 5px; border-radius: 4px; }
</style>