<script setup lang="ts">
import { computed, markRaw, onMounted, onUnmounted, reactive, ref, watch, type Component } from 'vue'
import { api, openAdminEvents, type EventConnection } from '@/api'
import { useI18n, type TranslationKey } from '@/i18n'
import type {
  Account,
  AdminEvent,
  LogEntry,
  Model,
  Cooldown,
  RequestSummary,
  ServiceConfig,
  ServiceStatus,
  TabID,
} from '@/types'
import AccountsPanel from '@/components/AccountsPanel.vue'
import LogsPanel from '@/components/LogsPanel.vue'
import ModelsTable from '@/components/ModelsTable.vue'
import PlaygroundPanel from '@/components/PlaygroundPanel.vue'
import RequestsPanel from '@/components/RequestsPanel.vue'
import SettingsPanel from '@/components/SettingsPanel.vue'
import UiIcon, { type IconName } from '@/components/UiIcon.vue'

const { availableLocales, locale, setLocale, t } = useI18n()
const currentTab = ref<TabID>('logs')
const status = ref<ServiceStatus | null>(null)
const logs = ref<LogEntry[]>([])
const accounts = ref<Account[]>([])
const models = ref<Model[]>([])
const cooldowns = ref<Cooldown[]>([])
const requests = ref<RequestSummary[]>([])
const config = ref<ServiceConfig | null>(null)
const startPending = ref(false)
const stopPending = ref(false)
const launchCancellationRequested = ref(false)
const eventsConnected = ref(true)
const languageMenuOpen = ref(false)
const notice = reactive({ message: '', tone: 'success' as 'success' | 'error' })
const loading = reactive({
  accounts: true,
  models: true,
  requests: true,
  cooldowns: true,
  config: true,
})
const errors = reactive({ accounts: '', models: '', requests: '', cooldowns: '', config: '' })
let eventConnection: EventConnection | undefined
let noticeTimer: number | undefined

// requests 列表上限:与日志一致截断历史,防止长时间挂机时
// 内存与 DOM 无限增长(SSE 每请求至少推 2 次)
const maxRequests = 500
// 日志条目单调递增 id:重放与实时共用同一计数器,
// 保证 v-for key 稳定(2000 条上限裁剪头部时幸存行不再整列重建)
let logSeq = 0

const navigation: { id: TabID; label: TranslationKey; icon: IconName }[] = [
  { id: 'logs', label: 'nav.logs', icon: 'dashboard' },
  { id: 'accounts', label: 'nav.accounts', icon: 'key' },
  { id: 'models', label: 'nav.models', icon: 'collection' },
  { id: 'requests', label: 'nav.requests', icon: 'info' },
  { id: 'settings', label: 'nav.settings', icon: 'settings' },
  { id: 'playground', label: 'nav.playground', icon: 'chat' },
]

const serviceState = computed(() => {
  if (status.value === null) return 'unavailable'
  if (status.value.state === 'RUNNING') return 'running'
  if (status.value.state === 'LAUNCHING') return 'launching'
  if (startPending.value && !launchCancellationRequested.value) return 'launching'
  return 'stopped'
})
// 语言菜单触发器显示当前语言的自称(而非 locale 代码)
const currentLocaleLabel = computed(
  () => availableLocales.find((item) => item.code === locale.value)?.label ?? locale.value,
)
const statusColor = computed(() => {
  if (serviceState.value === 'running') return 'bg-green-500 shadow-[0_0_10px_rgba(34,197,94,0.5)]'
  if (serviceState.value === 'launching') return 'bg-cyan-400 animate-pulse'
  return 'bg-gray-600'
})
const statusTextColor = computed(() => {
  if (serviceState.value === 'running') return 'text-green-400'
  if (serviceState.value === 'launching') return 'text-cyan-300'
  return 'text-gray-400'
})

// messageOf 统一呈现服务端错误内容
function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : t('common.error')
}

// showNotice 显示一次操作结果;错误消息通常较长,
// 给予更长阅读窗口(错误 6s / 成功 3.2s),可手动关闭
function showNotice(message: string, tone: 'success' | 'error'): void {
  notice.message = message
  notice.tone = tone
  if (noticeTimer !== undefined) window.clearTimeout(noticeTimer)
  noticeTimer = window.setTimeout(() => {
    notice.message = ''
  }, tone === 'error' ? 6000 : 3200)
}

// dismissNotice 手动关闭通知
function dismissNotice(): void {
  notice.message = ''
  if (noticeTimer !== undefined) window.clearTimeout(noticeTimer)
}

async function loadStatus(): Promise<void> {
  try {
    status.value = await api.status()
  } catch {
    status.value = null
  }
}

async function loadAccounts(): Promise<void> {
  loading.accounts = accounts.value.length === 0
  errors.accounts = ''
  try {
    accounts.value = await api.accounts()
  } catch (error) {
    errors.accounts = messageOf(error)
  } finally {
    loading.accounts = false
  }
}

async function loadAccountData(): Promise<void> {
  await Promise.all([loadAccounts(), loadModels(), loadStatus()])
}

async function loadModels(): Promise<void> {
  loading.models = models.value.length === 0
  errors.models = ''
  try {
    models.value = await api.models()
  } catch (error) {
    errors.models = messageOf(error)
  } finally {
    loading.models = false
  }
}

async function loadCooldowns(): Promise<void> {
  loading.cooldowns = cooldowns.value.length === 0
  errors.cooldowns = ''
  try {
    cooldowns.value = await api.cooldowns()
  } catch (error) {
    errors.cooldowns = messageOf(error)
  } finally {
    loading.cooldowns = false
  }
}

async function loadRequests(): Promise<void> {
  loading.requests = requests.value.length === 0
  errors.requests = ''
  try {
    // REST 按开始时间升序返回;反转为新在前,与 SSE
    // replaceByID 头插的实时顺序保持一致,避免刷新后列表
    // “旧在前”、直播时又“新在前”的错位。
    requests.value = (await api.requests()).slice().reverse()
  } catch (error) {
    errors.requests = messageOf(error)
  } finally {
    loading.requests = false
  }
}

async function loadRequestData(): Promise<void> {
  await Promise.all([loadRequests(), loadCooldowns()])
}

async function loadConfig(): Promise<void> {
  loading.config = config.value === null
  errors.config = ''
  try {
    config.value = await api.config()
  } catch (error) {
    errors.config = messageOf(error)
  } finally {
    loading.config = false
  }
}

async function refreshAll(): Promise<void> {
  await Promise.all([
    loadStatus(),
    loadAccounts(),
    loadModels(),
    loadCooldowns(),
    loadRequests(),
    loadConfig(),
  ])
}

async function startService(): Promise<void> {
  startPending.value = true
  launchCancellationRequested.value = false
  try {
    status.value = await api.startService()
    if (launchCancellationRequested.value || status.value.state !== 'RUNNING') return
    showNotice(t('app.start'), 'success')
    await Promise.all([loadAccounts(), loadModels(), loadCooldowns()])
  } catch (error) {
    if (!launchCancellationRequested.value) showNotice(messageOf(error), 'error')
    await loadStatus()
  } finally {
    startPending.value = false
  }
}

async function stopService(): Promise<void> {
  launchCancellationRequested.value = serviceState.value === 'launching'
  stopPending.value = true
  try {
    status.value = await api.stopService()
    showNotice(t('app.stop'), 'success')
    await Promise.all([loadAccounts(), loadModels(), loadCooldowns(), loadRequests()])
  } catch (error) {
    showNotice(messageOf(error), 'error')
    await loadStatus()
  } finally {
    stopPending.value = false
  }
}

async function clearLogs(): Promise<void> {
  try {
    await api.clearLogs()
    logs.value = []
  } catch (error) {
    showNotice(messageOf(error), 'error')
  }
}

function replaceByID<T extends { id: string }>(items: T[], incoming: T): void {
  const index = items.findIndex((item) => item.id === incoming.id)
  if (index === -1) {
    items.unshift(incoming)
    if (items.length > maxRequests) items.splice(maxRequests)
  } else {
    items[index] = incoming
  }
}

function handleAdminEvent(event: AdminEvent): void {
  if (event.type === 'status') {
    status.value = event.data
    return
  }
  if (event.type === 'log') {
    logs.value.push({ ...event.data, id: logSeq++ })
    if (logs.value.length > 2000) logs.value.splice(0, logs.value.length - 2000)
    return
  }
  if (event.type === 'accounts') {
    accounts.value = event.data.accounts
    return
  }
  if (event.type === 'models') {
    models.value = event.data.models
    return
  }
  if (event.type === 'cooldowns') {
    cooldowns.value = event.data
    return
  }
  replaceByID(requests.value, event.data)
}

onMounted(async () => {
  document.title = t('app.title')
  await refreshAll()
  eventConnection = openAdminEvents(
    handleAdminEvent,
    () => {
      // 重连成功:服务端每次连接都会重放完整快照
      // (status/models/accounts/cooldowns + 全部日志历史 +
      // 全部活动请求)。清空两个增量累积的集合,否则重放会
      // 与已有数据交错重复,且断线期间完成的请求会永久
      // 滞留“运行中”(重放只含活动请求)。
      eventsConnected.value = true
      logs.value = []
      requests.value = []
    },
    () => {
      eventsConnected.value = false
    },
  )
  // Esc 关闭语言菜单
  window.addEventListener('keydown', handleGlobalKeydown)
})

function handleGlobalKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') languageMenuOpen.value = false
}

function toggleLanguageMenu(): void {
  languageMenuOpen.value = !languageMenuOpen.value
}

// handleMenuFocusout 焦点移出菜单时关闭(键盘 Tab 导航友好)
function handleMenuFocusout(event: FocusEvent): void {
  const container = event.currentTarget
  if (!(container instanceof HTMLElement)) return
  if (!container.contains(event.relatedTarget as Node | null)) languageMenuOpen.value = false
}

function selectLocale(code: string): void {
  languageMenuOpen.value = false
  setLocale(code as Parameters<typeof setLocale>[0])
}

watch(locale, () => {
  document.title = t('app.title')
})

// 面板注册表:KeepAlive + 动态组件保留懒挂载(首次切入才实例化),
// 同时缓存已挂载面板,切页不销毁本地状态——Playground 正在流式
// 输出的对话、日志过滤条件、模型搜索词在切页后得以保留。
// 注意组件对象需 markRaw,避免被 reactivity 代理触发警告。
const panels: Record<
  TabID,
  { component: Component; props: () => Record<string, unknown>; events: Record<string, (...args: never[]) => void> }
> = {
  logs: {
    component: markRaw(LogsPanel),
    props: () => ({ logs: logs.value }),
    events: { clear: clearLogs },
  },
  accounts: {
    component: markRaw(AccountsPanel),
    props: () => ({
      accounts: accounts.value,
      loading: loading.accounts,
      error: errors.accounts,
      globalProxy: config.value?.proxy ?? '',
    }),
    events: { refresh: loadAccountData, notice: showNotice },
  },
  models: {
    component: markRaw(ModelsTable),
    props: () => ({ models: models.value, loading: loading.models, error: errors.models }),
    events: {},
  },
  requests: {
    component: markRaw(RequestsPanel),
    props: () => ({
      accounts: accounts.value,
      cooldowns: cooldowns.value,
      requests: requests.value,
      loading: loading.requests || loading.cooldowns,
      cooldownError: errors.cooldowns,
      requestError: errors.requests,
    }),
    events: { refresh: loadRequestData, notice: showNotice },
  },
  settings: {
    component: markRaw(SettingsPanel),
    props: () => ({ config: config.value, loading: loading.config, error: errors.config }),
    events: {
      saved: (value: ServiceConfig) => {
        config.value = value
      },
      notice: showNotice,
    },
  },
  playground: {
    component: markRaw(PlaygroundPanel),
    props: () => ({ models: models.value, apiKey: config.value?.proxy_api_key ?? '' }),
    events: {},
  },
}
const activePanel = computed(() => panels[currentTab.value])

onUnmounted(() => {
  eventConnection?.close()
  window.removeEventListener('keydown', handleGlobalKeydown)
  if (noticeTimer !== undefined) window.clearTimeout(noticeTimer)
})
</script>

<template>
  <div class="flex h-full w-full flex-col md:flex-row">
    <aside
      class="flex min-w-0 w-full shrink-0 flex-col border-b border-[#30363d] bg-[#161b22] md:w-64 md:border-r md:border-b-0"
    >
      <div class="flex h-14 items-center justify-between gap-2 border-b border-[#30363d] px-4">
        <div class="flex min-w-0 items-center gap-2">
          <div class="h-3 w-3 rounded-full" :class="statusColor"></div>
          <h1 class="whitespace-nowrap text-lg font-bold text-white">AI Studio Proxy</h1>
        </div>
        <div class="relative">
          <button
            class="flex shrink-0 items-center gap-1 whitespace-nowrap rounded border border-gray-700 px-1.5 py-0.5 font-mono text-xs text-gray-400 transition hover:text-white"
            type="button"
            :aria-expanded="languageMenuOpen"
            aria-haspopup="menu"
            :aria-label="currentLocaleLabel"
            @click="toggleLanguageMenu"
          >
            {{ currentLocaleLabel }}
            <UiIcon name="chevronDown" :size="12" />
          </button>
          <div
            v-if="languageMenuOpen"
            class="absolute top-full right-0 z-50 pt-1"
            role="menu"
            @focusout="handleMenuFocusout"
          >
            <div class="overflow-hidden rounded border border-[#30363d] bg-[#161b22] shadow-xl">
              <button
                v-for="item in availableLocales"
                :key="item.code"
                class="block w-full px-4 py-2 text-left text-xs whitespace-nowrap text-gray-300 hover:bg-blue-600 hover:text-white"
                type="button"
                role="menuitem"
                @click="selectLocale(item.code)"
              >
                {{ item.label }}
              </button>
            </div>
          </div>
        </div>
      </div>

      <nav
        class="flex w-full min-w-0 flex-none gap-1 overflow-x-auto p-2 md:flex-1 md:flex-col"
        aria-label="Main"
      >
        <button
          v-for="item in navigation"
          :key="item.id"
          :class="[
            'flex w-auto shrink-0 items-center gap-2 rounded-md px-3 py-2 text-left whitespace-nowrap transition md:w-full',
            currentTab === item.id
              ? 'bg-blue-600 text-white'
              : 'text-gray-400 hover:bg-[#21262d] hover:text-white',
          ]"
          type="button"
          :aria-current="currentTab === item.id ? 'page' : undefined"
          @click="currentTab = item.id"
        >
          <UiIcon :name="item.icon" :size="16" />
          {{ t(item.label) }}
        </button>
      </nav>

      <div
        class="relative min-w-0 overflow-hidden border-t border-[#30363d] p-2 md:p-4"
        :class="serviceState === 'launching' ? 'launching-shell' : ''"
      >
        <div v-if="serviceState === 'launching'" class="launching-scan" aria-hidden="true"></div>
        <div class="mb-2 text-xs text-gray-400">{{ t('app.status') }}</div>
        <div class="mb-4 flex items-center justify-between">
          <span class="font-mono font-bold" :class="statusTextColor">
            {{ serviceState.toUpperCase() }}
          </span>
          <span
            v-if="!eventsConnected"
            class="flex items-center gap-1 rounded border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-xs text-amber-300"
            role="status"
          >
            <UiIcon name="spinner" :size="10" />
            {{ t('app.reconnecting') }}
          </span>
          <span
            v-else
            class="max-w-[65%] truncate font-mono text-xs text-gray-400"
            :title="status?.version || ''"
          >
            v{{ status?.version || '—' }}
          </span>
        </div>
        <button
          v-if="serviceState === 'stopped'"
          class="flex w-full items-center justify-center gap-2 rounded bg-green-600 py-2 font-bold text-white shadow transition hover:bg-green-500 disabled:opacity-50"
          type="button"
          :disabled="startPending || stopPending"
          @click="startService"
        >
          <UiIcon :name="startPending ? 'spinner' : 'play'" :size="16" />
          {{ t('app.start') }}
        </button>
        <button
          v-else-if="serviceState === 'launching' || serviceState === 'running'"
          class="flex w-full items-center justify-center gap-2 rounded bg-red-600 py-2 font-bold text-white shadow transition hover:bg-red-500 disabled:opacity-50"
          type="button"
          :disabled="stopPending"
          @click="stopService"
        >
          <UiIcon :name="stopPending ? 'spinner' : 'stop'" :size="16" />
          {{ t('app.stop') }}
        </button>
      </div>
    </aside>

    <main class="flex min-w-0 flex-1 flex-col bg-[#0d1117]">
      <KeepAlive>
        <component :is="activePanel.component" v-bind="activePanel.props()" v-on="activePanel.events" />
      </KeepAlive>
    </main>

    <Transition name="notice">
      <div
        v-if="notice.message"
        class="fixed bottom-20 right-4 z-50 flex max-w-md items-start gap-3 rounded border bg-[#161b22] px-4 py-3 text-sm shadow-xl md:bottom-5 md:right-5"
        :class="
          notice.tone === 'error'
            ? 'border-red-500/50 text-red-300'
            : 'border-green-500/50 text-green-300'
        "
        :role="notice.tone === 'error' ? 'alert' : 'status'"
      >
        <span class="min-w-0 break-words">{{ notice.message }}</span>
        <button
          class="shrink-0 rounded p-0.5 text-gray-400 transition hover:text-white"
          type="button"
          :aria-label="t('common.close')"
          @click="dismissNotice"
        >
          <UiIcon name="close" :size="12" />
        </button>
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.launching-shell {
  background: radial-gradient(circle at 12% 0%, rgb(34 211 238 / 14%), transparent 55%), #161b22;
}

.launching-scan {
  position: absolute;
  top: 0;
  left: -45%;
  width: 45%;
  height: 1px;
  background: linear-gradient(90deg, transparent, rgb(103 232 249), transparent);
  animation: launching-scan 1.25s ease-in-out infinite;
}

@keyframes launching-scan {
  to {
    transform: translateX(320%);
  }
}
</style>
