import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import * as Icons from '@element-plus/icons-vue'
import App from './App.vue'
import './theme.css'

const app = createApp(App)
app.use(ElementPlus, { locale: zhCn })

// 全局注册图标
for (const [name, comp] of Object.entries(Icons)) {
  app.component(name, comp)
}

app.mount('#app')
