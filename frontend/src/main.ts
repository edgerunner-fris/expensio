import { createApp } from 'vue'
import { createPinia } from 'pinia'
import {
  ElAlert,
  ElAside,
  ElAvatar,
  ElBadge,
  ElButton,
  ElCol,
  ElConfigProvider,
  ElContainer,
  ElDatePicker,
  ElDescriptions,
  ElDescriptionsItem,
  ElDialog,
  ElDropdown,
  ElDropdownItem,
  ElDropdownMenu,
  ElEmpty,
  ElForm,
  ElFormItem,
  ElHeader,
  ElIcon,
  ElInput,
  ElInputNumber,
  ElLoading,
  ElMain,
  ElMenu,
  ElMenuItem,
  ElOption,
  ElPagination,
  ElProgress,
  ElRow,
  ElSelect,
  ElSwitch,
  ElTable,
  ElTableColumn,
  ElTag,
} from 'element-plus'
import 'element-plus/dist/index.css'

import App from './App.vue'
import router from './router'
import './styles.css'

// 只注册模板里真正用到的组件。全量 app.use(ElementPlus) 会把整个组件库
// 打进首屏 chunk；按需注册后未使用的组件与指令会被 tree-shaking 去掉。
// 新增 <el-*> 时必须同步这里，否则运行时会得到 "Failed to resolve component"。
const components = {
  ElAlert,
  ElAside,
  ElAvatar,
  ElBadge,
  ElButton,
  ElCol,
  ElConfigProvider,
  ElContainer,
  ElDatePicker,
  ElDescriptions,
  ElDescriptionsItem,
  ElDialog,
  ElDropdown,
  ElDropdownItem,
  ElDropdownMenu,
  ElEmpty,
  ElForm,
  ElFormItem,
  ElHeader,
  ElIcon,
  ElInput,
  ElInputNumber,
  ElMain,
  ElMenu,
  ElMenuItem,
  ElOption,
  ElPagination,
  ElProgress,
  ElRow,
  ElSelect,
  ElSwitch,
  ElTable,
  ElTableColumn,
  ElTag,
}

const app = createApp(App)
app.use(createPinia())
app.use(router)
for (const [name, component] of Object.entries(components)) {
  app.component(name, component)
}
app.use(ElLoading)
app.mount('#app')
