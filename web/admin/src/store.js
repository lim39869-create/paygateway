// 全局共享状态，解决页面间数据同步
import { ref } from 'vue'

// 每次通道启停/设置修改时递增，所有页面 watch 此值自动刷新
export const refreshVersion = ref(0)

export function bumpRefresh() {
  refreshVersion.value++
}