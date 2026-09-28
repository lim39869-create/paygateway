<template>
  <div class="unmatched-page">
    <div class="pg-card">
      <div class="pg-card-header">
        <span class="section-label">无订单收款</span>
        <div class="header-actions">
          <el-select v-model="filterStatus" size="small" style="width: 130px" @change="load">
            <el-option label="全部" value="" />
            <el-option label="待处理" value="pending" />
            <el-option label="已确认" value="resolved" />
            <el-option label="已退款" value="refunded" />
          </el-select>
          <span class="text-muted" style="font-size: 12px">共 {{ list.length }} 条</span>
        </div>
      </div>
      <div class="pg-card-body">
        <el-alert type="warning" :closable="false" class="mb16"
          title="有人向收款码转账但没有匹配到订单。请人工核对后处理：已确认（资金已收到）或已退款（原路退回）。" />
        <el-alert type="error" :closable="false" class="mb16"
          title="司法提示：系统按行为模式自动标记疑似风险收款（大额/同源高频）。标记金额疑为黑钱/赃款的，请立即核实付款人身份与资金来源，确认异常的请保全本页记录并联系司法部门处理。" />
      </div>
    </div>

    <div class="pg-card">
      <div class="pg-card-body" v-loading="loading">
        <el-table :data="list" stripe style="width: 100%">
          <el-table-column label="ID" width="70">
            <template #default="{ row }"><span class="font-mono">#{{ row.id }}</span></template>
          </el-table-column>
          <el-table-column label="通道" width="100">
            <template #default="{ row }">
              <el-tag :type="row.provider === 'alipay' ? 'primary' : 'success'" size="small">
                {{ row.provider === 'alipay' ? '支付宝' : '微信' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="金额" width="120">
            <template #default="{ row }">
              <span class="text-danger" style="font-weight: 600">¥{{ (row.amount_minor / 100).toFixed(2) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="付款人" width="120">
            <template #default="{ row }">
              <span v-if="row.payer" class="font-mono">{{ row.payer }}</span>
              <span v-else class="text-muted">—</span>
            </template>
          </el-table-column>
          <el-table-column label="风险标记" width="180">
            <template #default="{ row }">
              <template v-if="row.risk_level > 0">
                <el-tag type="danger" size="small" effect="dark" style="margin-right:4px">
                  {{ row.risk_level === 2 ? '高风险' : '建议核实' }}
                </el-tag>
                <el-tag v-for="t in String(row.risk_tags || '').split(',').filter(Boolean)"
                        :key="t" type="danger" size="small" effect="plain" style="margin-right:2px">
                  {{ riskTagText(t) }}
                </el-tag>
              </template>
              <span v-else class="text-muted">—</span>
            </template>
          </el-table-column>
          <el-table-column label="备注" width="110">
            <template #default="{ row }">
              <el-tag v-if="row.note" size="small" type="warning">{{ row.note }}</el-tag>
              <span v-else class="text-muted">无</span>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag :type="statusTag(row.status)" size="small" effect="light" round>{{ statusText(row.status) }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="处理人" width="110">
            <template #default="{ row }"><span class="text-muted">{{ row.handler || '—' }}</span></template>
          </el-table-column>
          <el-table-column label="时间" width="170">
            <template #default="{ row }"><span class="font-mono text-muted" style="font-size: 12px">{{ fmtTime(row.created_at) }}</span></template>
          </el-table-column>
          <el-table-column label="通知原文" min-width="200">
            <template #default="{ row }">
              <div class="raw-cell" :title="row.raw">{{ row.raw || '—' }}</div>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="100" fixed="right">
            <template #default="{ row }">
              <el-button v-if="row.status === 'pending'" size="small" type="primary" text @click="openResolve(row)">处理</el-button>
            </template>
          </el-table-column>
        </el-table>
        <div v-if="!loading && list.length === 0" class="text-muted" style="text-align:center; padding: 40px 0">
          暂无无订单收款记录
        </div>
      </div>
    </div>

    <el-dialog v-model="resolveVisible" title="处理无订单收款" width="500px">
      <el-descriptions :column="2" size="small" border class="mb16">
        <el-descriptions-item label="金额">
          <span class="text-danger" style="font-weight: 600">¥{{ (current.amount_minor / 100).toFixed(2) }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="通道">{{ current.provider === 'alipay' ? '支付宝' : '微信' }}</el-descriptions-item>
        <el-descriptions-item label="备注码">{{ current.note || '—' }}</el-descriptions-item>
        <el-descriptions-item label="ID">#{{ current.id }}</el-descriptions-item>
      </el-descriptions>
      <el-form label-width="90px">
        <el-form-item label="处理结果" required>
          <el-radio-group v-model="resolveForm.status">
            <el-radio value="resolved">已确认收款</el-radio>
            <el-radio value="refunded">已原路退款</el-radio>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="处理备注">
          <el-input v-model="resolveForm.note" type="textarea" :rows="3" placeholder="填写处理说明：核对依据、退款凭证等（留痕审计）" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="resolveVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="submitResolve">提交</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, watch, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '../api'

const props = defineProps({ refreshKey: { type: Number, default: 0 } })

const list = ref([])
const loading = ref(false)
const filterStatus = ref('pending')
const resolveVisible = ref(false)
const saving = ref(false)
const current = ref({})
const resolveForm = ref({ status: 'resolved', note: '' })

function statusTag(s) { return { pending: 'warning', resolved: 'success', refunded: 'info' }[s] || 'info' }
function statusText(s) { return { pending: '待处理', resolved: '已确认', refunded: '已退款' }[s] || s }
function riskTagText(t) {
  return { big_amount: '大额', burst: '高频', repeat_payer: '同源重复' }[t] || t
}
function fmtTime(t) { if (!t) return '—'; return String(t).replace('T', ' ').slice(0, 19) }

async function load() {
  loading.value = true
  try { const data = await api.personalUnmatched({ status: filterStatus.value, limit: 100 }); list.value = data.unmatched || [] }
  catch (e) { ElMessage.error('加载失败：' + e.message) }
  finally { loading.value = false }
}

function openResolve(row) { current.value = row; resolveForm.value = { status: 'resolved', note: '' }; resolveVisible.value = true }

async function submitResolve() {
  saving.value = true
  try {
    await api.resolveUnmatched(current.value.id, { status: resolveForm.value.status, note: resolveForm.value.note })
    ElMessage.success('已处理'); resolveVisible.value = false; load()
  } catch (e) { ElMessage.error('处理失败：' + e.message) }
  finally { saving.value = false }
}

onMounted(load)
watch(() => props.refreshKey, load)
</script>

<style scoped>
.unmatched-page { max-width: 1400px; margin: 0 auto; display: flex; flex-direction: column; gap: 16px; }
.section-label { font-weight: 600; font-size: 14px; color: var(--text-primary); }
.mb16 { margin-bottom: 16px; }
.header-actions { display: flex; align-items: center; gap: 10px; }
.raw-cell { max-width: 300px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; font-size: 12px; color: var(--text-muted); }
.font-mono { font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace; }
.text-muted { color: var(--text-muted); }
.text-danger { color: var(--danger); }
</style>