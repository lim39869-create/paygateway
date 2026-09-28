<template>
  <div class="login-page">
    <div class="login-card">
      <div class="login-header">
        <div class="login-logo">
          <img src="/assets/favicon.svg" alt="PayGateway" width="44" height="44" />
        </div>
        <h1 class="login-title">PayGateway</h1>
        <p class="login-sub">{{ setupNeeded ? '首次启动 · 初始化管理员' : '支付网关管理控制台' }}</p>
      </div>

      <!-- 首次启动注册 -->
      <el-form v-if="setupNeeded" @submit.prevent="register" :model="form" size="large">
        <el-alert type="info" :closable="false" class="mb12" style="font-size: 12px"
          title="首次启动：设置管理员账号与密码。凭据经 bcrypt 哈希加密后存入数据库，登录后台后可随时管理。" />
        <el-form-item>
          <el-input v-model="form.username" placeholder="管理员用户名（3-32 位）" prefix-icon="User"
            @keyup.enter="register" size="large" class="login-input" />
        </el-form-item>
        <el-form-item>
          <el-input v-model="form.password" type="password" placeholder="管理员密码（至少 8 位）"
            prefix-icon="Lock" show-password @keyup.enter="register" size="large" class="login-input" />
        </el-form-item>
        <el-form-item style="margin-bottom: 0; margin-top: 4px">
          <el-button type="primary" :loading="loading" class="login-btn" @click="register"
            style="width:100%" size="large">
            {{ loading ? '初始化中...' : '创建管理员并登录' }}
          </el-button>
        </el-form-item>
        <div v-if="error" class="login-error">
          <el-icon :size="13"><CircleCloseFilled /></el-icon>
          {{ error }}
        </div>
      </el-form>

      <!-- 登录 -->
      <el-form v-else @submit.prevent="login" :model="form" size="large">
        <el-form-item>
          <el-input v-model="form.username" placeholder="用户名" prefix-icon="User"
            @keyup.enter="login" size="large" class="login-input" />
        </el-form-item>
        <el-form-item>
          <el-input v-model="form.password" type="password" placeholder="密码" prefix-icon="Lock"
            show-password @keyup.enter="login" size="large" class="login-input" />
        </el-form-item>
        <el-form-item style="margin-bottom: 0; margin-top: 4px">
          <el-button type="primary" :loading="loading" class="login-btn" @click="login"
            style="width:100%" size="large">
            {{ loading ? '登录中...' : '登 录' }}
          </el-button>
        </el-form-item>
        <div v-if="error" class="login-error">
          <el-icon :size="13"><CircleCloseFilled /></el-icon>
          {{ error }}
        </div>
      </el-form>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { CircleCloseFilled } from '@element-plus/icons-vue'

const emit = defineEmits(['login-success'])

const form = reactive({ username: '', password: '' })
const loading = ref(false)
const error = ref('')
const setupNeeded = ref(false)

onMounted(async () => {
  try {
    const res = await fetch('/api/auth/setup-status')
    const json = await res.json()
    setupNeeded.value = json.data?.setup_needed ?? false
  } catch (e) {
    // 状态接口失败不阻塞登录页
  }
})

async function login() {
  if (!form.username || !form.password) {
    error.value = '请输入用户名和密码'
    return
  }
  loading.value = true
  error.value = ''
  try {
    const res = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(form)
    })
    const json = await res.json()
    if (!res.ok) {
      error.value = json.error || '登录失败'
      return
    }
    sessionStorage.setItem('admin_token', json.data.token)
    sessionStorage.setItem('admin_username', form.username)
    ElMessage.success('登录成功')
    emit('login-success')
  } catch (e) {
    error.value = '网络错误: ' + e.message
  } finally {
    loading.value = false
  }
}

async function register() {
  if (!form.username || form.username.length < 3) {
    error.value = '用户名至少 3 位'
    return
  }
  if (!form.password || form.password.length < 8) {
    error.value = '密码至少 8 位'
    return
  }
  loading.value = true
  error.value = ''
  try {
    const res = await fetch('/api/auth/register', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: form.username, password: form.password })
    })
    const json = await res.json()
    if (!res.ok) {
      error.value = json.error || '注册失败'
      return
    }
    ElMessage.success('管理员创建成功')
    // 注册成功 → 直接用该凭据登录（复用登录流程，避免重复输入）
    setupNeeded.value = false
    loading.value = false
    await login()
  } catch (e) {
    error.value = '网络错误: ' + e.message
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--bg-base);
  transition: background 0.18s ease;
}

.login-card {
  background: var(--bg-surface);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 36px 32px 28px;
  width: 360px;
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.06);
  transition: background 0.18s ease, border-color 0.18s ease, box-shadow 0.18s ease;
}

.login-header {
  text-align: center;
  margin-bottom: 28px;
}

.login-logo {
  width: 44px;
  height: 44px;
  display: flex;
  align-items: center;
  justify-content: center;
  margin: 0 auto 12px;
}

.login-logo img {
  display: block;
  width: 44px;
  height: 44px;
}

.login-title {
  margin: 0 0 5px;
  font-size: 18px;
  font-weight: 590;
  color: var(--text-primary);
  letter-spacing: -0.02em;
}

.login-sub {
  margin: 0;
  font-size: 12px;
  color: var(--text-muted);
  font-weight: 410;
}

.mb12 { margin-bottom: 12px; }

/* Input overrides for login */
:deep(.login-input .el-input__wrapper) {
  background: var(--bg-subtle) !important;
  border: 1px solid var(--border) !important;
  box-shadow: none !important;
  border-radius: 6px !important;
  padding: 2px 12px !important;
  transition: all 0.12s ease !important;
}
:deep(.login-input .el-input__wrapper:hover) {
  border-color: var(--border-strong) !important;
}
:deep(.login-input .el-input__wrapper.is-focus) {
  border-color: var(--accent) !important;
  box-shadow: 0 0 0 2px var(--accent-light) !important;
  background: var(--bg-surface) !important;
}
:deep(.login-input .el-input__inner) {
  color: var(--text-primary) !important;
  font-size: 13px !important;
}
:deep(.login-input .el-input__inner::placeholder) {
  color: var(--text-placeholder) !important;
}
:deep(.login-input .el-input__prefix .el-icon) {
  color: var(--text-muted) !important;
  font-size: 14px !important;
}

/* Login button */
.login-btn {
  height: 40px !important;
  font-size: 13px !important;
  font-weight: 590 !important;
  border-radius: 6px !important;
  border: none !important;
  background: var(--accent) !important;
  color: #fff !important;
  letter-spacing: 0.02em !important;
  transition: background 0.12s ease !important;
}

.login-btn:hover {
  background: var(--accent-hover) !important;
}

.login-btn:active {
  background: var(--accent) !important;
  transform: scale(0.99);
}

/* Error */
.login-error {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  color: var(--danger);
  font-size: 12px;
  margin-top: 10px;
  padding: 8px 12px;
  background: var(--danger-bg);
  border: 1px solid var(--danger-border);
  border-radius: 6px;
}
</style>
