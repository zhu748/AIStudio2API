<script setup lang="ts">
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { api } from '@/api'
import { useI18n, type TranslationKey } from '@/i18n'
import { maskProxyCredentials, proxySchemeError } from '@/proxy'
import type { Account, AccountDraft, AccountState, ChromeImportProfile } from '@/types'
import UiIcon from './UiIcon.vue'

const props = defineProps<{
  accounts: Account[]
  loading: boolean
  error: string
  globalProxy: string
}>()

const emit = defineEmits<{
  refresh: []
  notice: [message: string, tone: 'success' | 'error']
}>()

const { t } = useI18n()
const defaultAccountLocale = navigator.language || 'en-US'
const defaultAccountTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
const showEditor = ref(false)
const showBrowserLogin = ref(false)
const showChromeImport = ref(false)
const editingAccountID = ref('')
const pendingAction = ref('')
const chromeProfiles = ref<ChromeImportProfile[]>([])
const selectedChromeProfiles = ref<string[]>([])
// 两份独立环境状态:浏览器登录与 Chrome 导入互不串值,
// 避免在登录弹窗填写后被导入弹窗意外沿用
const loginEnvironment = reactive({
  proxy: '',
  locale: defaultAccountLocale,
  timezone: defaultAccountTimezone,
})
const importEnvironment = reactive({
  proxy: '',
  locale: defaultAccountLocale,
  timezone: defaultAccountTimezone,
})
const draft = reactive<AccountDraft>({
  label: '',
  enabled: true,
  proxy: '',
  locale: defaultAccountLocale,
  timezone: defaultAccountTimezone,
})

const stateKeys: Record<AccountState, TranslationKey> = {
  ready: 'state.ready',
  busy: 'state.busy',
  cooldown: 'state.cooldown',
  auth_required: 'state.auth_required',
  unavailable: 'state.unavailable',
  disabled: 'state.disabled',
}

// 代理客户端校验:三个入口(编辑/登录/导入)共享同一套规则与提示
const draftProxyError = computed(() => proxyErrorOf(draft.proxy))
const loginProxyError = computed(() => proxyErrorOf(loginEnvironment.proxy))
const importProxyError = computed(() => proxyErrorOf(importEnvironment.proxy))

function proxyErrorOf(value: string): string {
  const code = proxySchemeError(value)
  if (code === '') return ''
  if (code === 'scheme') return t('common.proxyInvalidScheme')
  return t('common.proxyInvalid')
}

// 全局操作(浏览器登录/Chrome 发现与导入)会阻塞所有账户按钮;
// 账户级操作只禁用目标账户自身的按钮,不再误伤其它账户(C9)
function isGlobalPending(): boolean {
  return (
    pendingAction.value === 'browser-login' ||
    pendingAction.value === 'chrome-discover' ||
    pendingAction.value === 'chrome-import'
  )
}

function actionPending(action: string, accountID: string): boolean {
  return isGlobalPending() || pendingAction.value === `${action}:${accountID}`
}

// accountProxyText 展示账号代理:掩码凭据后截断;空值区分
// “跟随全局代理”与“真直连”,消除歧义(C3)
function accountProxyText(account: Account): string {
  if (account.proxy !== '') return maskProxyCredentials(account.proxy)
  return props.globalProxy !== '' ? t('accounts.followGlobal') : t('accounts.direct')
}

function accountProxyTitle(account: Account): string {
  if (account.proxy !== '') return account.proxy
  return props.globalProxy !== ''
    ? `${t('accounts.followGlobal')}: ${props.globalProxy}`
    : t('accounts.direct')
}

// actionError 将账户写操作错误发送到全局通知
function actionError(error: unknown): void {
  emit('notice', error instanceof Error ? error.message : t('common.error'), 'error')
}

// ===================== 模态焦点管理 =====================
// 三个对话框此前无焦点圈禁:打开时焦点仍留在触发按钮(导致
// Esc 键无效——事件绑在 form 上而焦点不在内部),Tab 可越过
// 对话框进入背景页面。这里补齐:打开时移入对话框、关闭时
// 归还触发按钮、Tab 在对话框内循环。
const editorForm = ref<HTMLFormElement>()
const loginForm = ref<HTMLFormElement>()
const importForm = ref<HTMLFormElement>()
let modalTriggerElement: HTMLElement | null = null

const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function rememberModalTrigger(): void {
  modalTriggerElement =
    document.activeElement instanceof HTMLElement ? document.activeElement : null
}

function focusModalForm(form: HTMLFormElement | undefined): void {
  nextTick(() => form?.focus())
}

function releaseModalFocus(): void {
  modalTriggerElement?.focus()
  modalTriggerElement = null
}

// trapModalFocus 将 Tab 循环限制在对话框内部
function trapModalFocus(event: KeyboardEvent, form: HTMLElement | undefined): void {
  if (form === undefined || event.defaultPrevented) return
  const focusable = Array.from(form.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
    (element) => element.offsetParent !== null,
  )
  if (focusable.length === 0) return
  const first = focusable[0]
  const last = focusable[focusable.length - 1]
  if (first === undefined || last === undefined) return
  const active = document.activeElement
  if (event.shiftKey) {
    if (active === first || active === form) {
      event.preventDefault()
      last.focus()
    }
  } else if (active === last || active === form) {
    event.preventDefault()
    first.focus()
  }
}

watch(showEditor, (open) => {
  if (open) focusModalForm(editorForm.value)
  else releaseModalFocus()
})
watch(showBrowserLogin, (open) => {
  if (open) focusModalForm(loginForm.value)
  else releaseModalFocus()
})
watch(showChromeImport, (open) => {
  if (open) focusModalForm(importForm.value)
  else releaseModalFocus()
})

function beginEdit(account: Account): void {
  rememberModalTrigger()
  editingAccountID.value = account.id
  draft.label = account.label
  draft.enabled = account.enabled
  draft.proxy = account.proxy
  draft.locale = account.locale
  draft.timezone = account.timezone
  showEditor.value = true
}

function closeEditor(): void {
  showEditor.value = false
  editingAccountID.value = ''
}

function openBrowserLogin(): void {
  rememberModalTrigger()
  showBrowserLogin.value = true
}

function closeBrowserLogin(): void {
  if (pendingAction.value === 'browser-login') return
  showBrowserLogin.value = false
}

// beginBrowserLogin 启动浏览器登录并使用返回身份创建账户
async function beginBrowserLogin(): Promise<void> {
  if (loginProxyError.value !== '') return
  pendingAction.value = 'browser-login'
  try {
    await api.createAccount({ ...loginEnvironment })
    showBrowserLogin.value = false
    emit('refresh')
    emit('notice', t('accounts.loginComplete'), 'success')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// openChromeImport 读取本机可导入的 Chrome 账户
async function openChromeImport(): Promise<void> {
  pendingAction.value = 'chrome-discover'
  try {
    chromeProfiles.value = await api.chromeImportProfiles()
    selectedChromeProfiles.value = chromeProfiles.value.map((profile) => profile.profile)
    rememberModalTrigger()
    showChromeImport.value = true
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

function closeChromeImport(): void {
  if (pendingAction.value === 'chrome-import') return
  showChromeImport.value = false
  chromeProfiles.value = []
  selectedChromeProfiles.value = []
}

// importChromeAccounts 导入用户选中的 Chrome 账户
async function importChromeAccounts(): Promise<void> {
  if (importProxyError.value !== '') return
  pendingAction.value = 'chrome-import'
  try {
    const result = await api.importChromeAccounts({
      profiles: [...selectedChromeProfiles.value],
      ...importEnvironment,
    })
    showChromeImport.value = false
    chromeProfiles.value = []
    selectedChromeProfiles.value = []
    emit('refresh')
    emit(
      'notice',
      t('accounts.importComplete').replace('{count}', String(result.accounts.length)),
      'success',
    )
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// saveAccount 保存已有账户配置并刷新产品数据
async function saveAccount(): Promise<void> {
  if (editingAccountID.value === '' || draftProxyError.value !== '') return
  pendingAction.value = `edit:${editingAccountID.value}`
  try {
    await api.updateAccount(editingAccountID.value, { ...draft })
    closeEditor()
    emit('refresh')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// toggleAccount 切换账户是否参与请求
async function toggleAccount(account: Account): Promise<void> {
  pendingAction.value = `toggle:${account.id}`
  try {
    await api.updateAccount(account.id, {
      label: account.label,
      enabled: !account.enabled,
      proxy: account.proxy,
      locale: account.locale,
      timezone: account.timezone,
    })
    emit('refresh')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// runAccountAction 执行登录或会话验证
async function runAccountAction(account: Account, action: 'login' | 'verify'): Promise<void> {
  pendingAction.value = `${action}:${account.id}`
  try {
    if (action === 'login') {
      await api.loginAccount(account.id)
    } else {
      await api.verifyAccount(account.id)
    }
    emit('refresh')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}

// removeAccount 删除用户确认的账户
async function removeAccount(account: Account): Promise<void> {
  if (!window.confirm(t('accounts.deleteConfirm'))) return
  pendingAction.value = `delete:${account.id}`
  try {
    await api.deleteAccount(account.id)
    emit('refresh')
  } catch (error) {
    actionError(error)
  } finally {
    pendingAction.value = ''
  }
}
</script>

<template>
  <section class="mx-auto w-full max-w-4xl flex-1 overflow-auto p-4 md:p-8">
    <div
      class="mb-6 flex flex-wrap items-center justify-between gap-3 border-b border-[#30363d] pb-2"
    >
      <h2 class="text-2xl font-bold text-white">{{ t('section.accounts.title') }}</h2>
      <div class="flex flex-wrap justify-end gap-2">
        <button
          class="flex items-center gap-2 rounded border border-[#30363d] bg-[#21262d] px-4 py-2 text-sm font-medium text-gray-200 transition hover:bg-[#30363d] disabled:opacity-50"
          type="button"
          :disabled="isGlobalPending()"
          @click="openChromeImport"
        >
          <UiIcon name="accounts" :size="15" />
          {{ t('accounts.importChrome') }}
        </button>
        <button
          class="flex items-center gap-2 rounded bg-blue-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-blue-500 disabled:opacity-50"
          type="button"
          :disabled="isGlobalPending()"
          @click="openBrowserLogin"
        >
          <UiIcon name="login" :size="15" />
          {{ t('accounts.browserLogin') }}
        </button>
      </div>
    </div>

    <div v-if="error" class="rounded border border-red-500/40 bg-red-500/10 p-4 text-red-300">
      {{ error }}
    </div>
    <div v-else-if="loading" class="py-12 text-center text-gray-400">
      {{ t('common.loading') }}
    </div>
    <div
      v-else-if="accounts.length === 0"
      class="rounded border border-[#30363d] bg-[#161b22] py-10 text-center text-gray-400"
    >
      <p class="mb-2">{{ t('accounts.empty') }}</p>
      <p class="text-xs">{{ t('accounts.emptyHint') }}</p>
    </div>
    <div v-else class="space-y-3">
      <article
        v-for="account in accounts"
        :key="account.id"
        class="rounded-lg border border-[#30363d] bg-[#161b22] p-4"
        :class="{ 'opacity-60': !account.enabled }"
      >
        <div class="mb-2 flex items-center justify-between gap-4">
          <div class="flex min-w-0 items-center gap-3">
            <div
              class="h-3 w-3 shrink-0 rounded-full"
              :class="{
                'bg-green-500': account.state === 'ready',
                'bg-blue-500': account.state === 'busy',
                'bg-yellow-500': account.state === 'cooldown',
                'bg-red-500': account.state === 'auth_required' || account.state === 'unavailable',
                'bg-gray-500': account.state === 'disabled',
              }"
            ></div>
            <div class="min-w-0">
              <strong class="block truncate font-mono text-white">{{ account.label }}</strong>
            </div>
          </div>
          <div class="shrink-0 text-xs text-gray-400">
            {{ t('accounts.models') }}:
            {{ account.models.length === 0 ? '—' : account.models.length }}
          </div>
        </div>

        <div
          class="grid grid-cols-1 gap-2 border-t border-[#30363d] pt-3 text-xs sm:grid-cols-2 md:grid-cols-4"
        >
          <div>
            <span class="text-gray-400">{{ t('accounts.proxy') }}</span>
            <div class="truncate text-gray-300" :title="accountProxyTitle(account)">
              {{ accountProxyText(account) }}
            </div>
          </div>
          <div>
            <span class="text-gray-400">{{ t('accounts.locale') }}</span>
            <div class="text-gray-300">{{ account.locale }}</div>
          </div>
          <div>
            <span class="text-gray-400">{{ t('accounts.timezone') }}</span>
            <div class="text-gray-300">{{ account.timezone }}</div>
          </div>
          <div>
            <span class="text-gray-400">{{ t('accounts.benefitTier') }}</span>
            <div class="text-gray-300">{{ account.benefit_tier }}</div>
          </div>
        </div>

        <p v-if="account.message" class="mt-3 break-words text-xs text-red-400">
          {{ account.message }}
        </p>

        <div class="mt-3 flex items-center justify-between gap-3 border-t border-[#30363d] pt-3">
          <span
            class="text-xs font-medium uppercase"
            :class="{
              'text-green-400': account.state === 'ready',
              'text-blue-400': account.state === 'busy',
              'text-yellow-400': account.state === 'cooldown',
              'text-red-400': account.state === 'auth_required' || account.state === 'unavailable',
              'text-gray-400': account.state === 'disabled',
            }"
          >
            {{ t(stateKeys[account.state]) }}
          </span>
          <div class="flex flex-wrap justify-end gap-2">
            <button
              class="rounded border border-[#30363d] bg-[#21262d] px-3 py-1 text-xs text-gray-300 transition hover:bg-[#30363d] disabled:opacity-50"
              type="button"
              :disabled="actionPending('edit', account.id)"
              @click="beginEdit(account)"
            >
              {{ t('common.edit') }}
            </button>
            <button
              class="rounded border border-[#30363d] bg-[#21262d] px-3 py-1 text-xs text-gray-300 transition hover:bg-[#30363d] disabled:opacity-50"
              type="button"
              :disabled="actionPending('toggle', account.id)"
              @click="toggleAccount(account)"
            >
              {{ t(account.enabled ? 'common.disable' : 'common.enable') }}
            </button>
            <button
              v-if="account.state === 'auth_required'"
              class="rounded bg-green-600 px-3 py-1 text-xs text-white transition hover:bg-green-500 disabled:opacity-50"
              type="button"
              :disabled="actionPending('login', account.id) || !account.enabled"
              @click="runAccountAction(account, 'login')"
            >
              {{ t('common.relogin') }}
            </button>
            <button
              class="rounded border border-[#30363d] bg-[#21262d] px-3 py-1 text-xs text-gray-300 transition hover:bg-[#30363d] disabled:opacity-50"
              type="button"
              :disabled="actionPending('verify', account.id) || !account.enabled"
              @click="runAccountAction(account, 'verify')"
            >
              {{ t('common.verify') }}
            </button>
            <button
              class="rounded border border-red-900/50 bg-red-900/30 px-3 py-1 text-xs text-red-400 transition hover:bg-red-900/50 disabled:opacity-50"
              type="button"
              :disabled="actionPending('delete', account.id)"
              @click="removeAccount(account)"
            >
              {{ t('common.delete') }}
            </button>
          </div>
        </div>
      </article>
    </div>

    <Teleport to="body">
      <div v-if="showEditor" class="modal-overlay" @click.self="closeEditor">
        <form
          ref="editorForm"
          class="mx-4 max-h-[90vh] w-full max-w-md overflow-y-auto rounded-lg border border-[#30363d] bg-[#161b22] p-6 shadow-xl"
          role="dialog"
          aria-modal="true"
          aria-labelledby="account-editor-title"
          tabindex="-1"
          @submit.prevent="saveAccount"
          @keydown.esc="closeEditor"
          @keydown.tab="trapModalFocus($event, editorForm)"
        >
          <div class="mb-4 flex items-center justify-between">
            <h3 id="account-editor-title" class="text-lg font-bold text-white">{{
              t('accounts.editTitle')
            }}</h3>
            <button
              class="rounded p-1 text-gray-400 hover:bg-[#30363d] hover:text-white"
              type="button"
              :aria-label="t('common.close')"
              @click="closeEditor"
            >
              <UiIcon name="close" :size="16" />
            </button>
          </div>
          <p class="mb-4 truncate font-mono text-sm text-gray-300">{{ draft.label }}</p>
          <div class="space-y-4">
            <label class="flex items-center justify-between">
              <span class="text-sm font-medium text-gray-400">{{ t('common.enable') }}</span>
              <input v-model="draft.enabled" class="h-4 w-4 accent-blue-600" type="checkbox" />
            </label>
            <label class="block">
              <span class="mb-1 block text-sm font-medium text-gray-400">{{
                t('accounts.proxy')
              }}</span>
              <input
                v-model.trim="draft.proxy"
                class="w-full rounded border px-3 py-2 text-white transition focus:outline-none"
                :class="
                  draftProxyError !== ''
                    ? 'border-red-500/60 bg-[#0d1117] focus:border-red-500'
                    : 'border-[#30363d] bg-[#0d1117] focus:border-blue-500'
                "
                placeholder="http://127.0.0.1:7890"
                autocomplete="off"
                spellcheck="false"
              />
              <span v-if="draftProxyError !== ''" class="mt-1 block text-xs text-red-400">
                {{ draftProxyError }}
              </span>
              <span v-else class="mt-1 block text-xs text-gray-400">
                {{ t('accounts.proxyHint') }}
              </span>
            </label>
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.locale')
                }}</span>
                <input
                  v-model.trim="draft.locale"
                  class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                  required
                  autocomplete="off"
                />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.timezone')
                }}</span>
                <input
                  v-model.trim="draft.timezone"
                  class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                  required
                  autocomplete="off"
                />
              </label>
            </div>
          </div>
          <div class="mt-6 flex gap-2">
            <button
              class="flex-1 rounded bg-[#21262d] py-2 text-sm text-gray-300 transition hover:bg-[#30363d]"
              type="button"
              @click="closeEditor"
            >
              {{ t('common.cancel') }}
            </button>
            <button
              class="flex-1 rounded bg-blue-600 py-2 text-sm text-white transition hover:bg-blue-500 disabled:opacity-50"
              type="submit"
              :disabled="pendingAction !== '' || draftProxyError !== ''"
            >
              {{ t('common.save') }}
            </button>
          </div>
        </form>
      </div>

      <div v-if="showBrowserLogin" class="modal-overlay" @click.self="closeBrowserLogin">
        <form
          ref="loginForm"
          class="mx-4 max-h-[90vh] w-full max-w-md overflow-y-auto rounded-lg border border-[#30363d] bg-[#161b22] p-6 shadow-xl"
          role="dialog"
          aria-modal="true"
          aria-labelledby="browser-login-title"
          tabindex="-1"
          @submit.prevent="beginBrowserLogin"
          @keydown.esc="closeBrowserLogin"
          @keydown.tab="trapModalFocus($event, loginForm)"
        >
          <div class="mb-4 flex items-center justify-between">
            <h3 id="browser-login-title" class="text-lg font-bold text-white">{{
              t('accounts.browserLogin')
            }}</h3>
            <button
              class="rounded p-1 text-gray-400 hover:bg-[#30363d] hover:text-white disabled:opacity-50"
              type="button"
              :disabled="pendingAction === 'browser-login'"
              :aria-label="t('common.close')"
              @click="closeBrowserLogin"
            >
              <UiIcon name="close" :size="16" />
            </button>
          </div>
          <div class="space-y-4">
            <label class="block">
              <span class="mb-1 block text-sm font-medium text-gray-400">{{
                t('accounts.proxy')
              }}</span>
              <input
                v-model.trim="loginEnvironment.proxy"
                class="w-full rounded border px-3 py-2 text-white transition focus:outline-none"
                :class="
                  loginProxyError !== ''
                    ? 'border-red-500/60 bg-[#0d1117] focus:border-red-500'
                    : 'border-[#30363d] bg-[#0d1117] focus:border-blue-500'
                "
                placeholder="http://127.0.0.1:7890"
                autocomplete="off"
                spellcheck="false"
              />
              <span v-if="loginProxyError !== ''" class="mt-1 block text-xs text-red-400">
                {{ loginProxyError }}
              </span>
              <span v-else class="mt-1 block text-xs text-gray-400">
                {{ t('accounts.proxyHint') }}
              </span>
            </label>
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.locale')
                }}</span>
                <input
                  v-model.trim="loginEnvironment.locale"
                  class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                  required
                  autocomplete="off"
                />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.timezone')
                }}</span>
                <input
                  v-model.trim="loginEnvironment.timezone"
                  class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                  required
                  autocomplete="off"
                />
              </label>
            </div>
          </div>
          <div class="mt-6 flex gap-2">
            <button
              class="flex-1 rounded bg-[#21262d] py-2 text-sm text-gray-300 transition hover:bg-[#30363d] disabled:opacity-50"
              type="button"
              :disabled="pendingAction === 'browser-login'"
              @click="closeBrowserLogin"
            >
              {{ t('common.cancel') }}
            </button>
            <button
              class="flex-1 rounded bg-blue-600 py-2 text-sm text-white transition hover:bg-blue-500 disabled:opacity-50"
              type="submit"
              :disabled="pendingAction !== '' || loginProxyError !== ''"
            >
              {{ t('accounts.browserLogin') }}
            </button>
          </div>
        </form>
      </div>

      <div v-if="showChromeImport" class="modal-overlay" @click.self="closeChromeImport">
        <form
          ref="importForm"
          class="mx-4 max-h-[90vh] w-full max-w-lg overflow-y-auto rounded-lg border border-[#30363d] bg-[#161b22] p-6 shadow-xl"
          role="dialog"
          aria-modal="true"
          aria-labelledby="chrome-import-title"
          tabindex="-1"
          @submit.prevent="importChromeAccounts"
          @keydown.esc="closeChromeImport"
          @keydown.tab="trapModalFocus($event, importForm)"
        >
          <div class="mb-4 flex items-center justify-between">
            <h3 id="chrome-import-title" class="text-lg font-bold text-white">{{
              t('accounts.chromeTitle')
            }}</h3>
            <button
              class="rounded p-1 text-gray-400 hover:bg-[#30363d] hover:text-white disabled:opacity-50"
              type="button"
              :disabled="pendingAction === 'chrome-import'"
              :aria-label="t('common.close')"
              @click="closeChromeImport"
            >
              <UiIcon name="close" :size="16" />
            </button>
          </div>
          <div class="mb-4 space-y-4">
            <label class="block">
              <span class="mb-1 block text-sm font-medium text-gray-400">{{
                t('accounts.proxy')
              }}</span>
              <input
                v-model.trim="importEnvironment.proxy"
                class="w-full rounded border px-3 py-2 text-white transition focus:outline-none"
                :class="
                  importProxyError !== ''
                    ? 'border-red-500/60 bg-[#0d1117] focus:border-red-500'
                    : 'border-[#30363d] bg-[#0d1117] focus:border-blue-500'
                "
                placeholder="http://127.0.0.1:7890"
                autocomplete="off"
                spellcheck="false"
              />
              <span v-if="importProxyError !== ''" class="mt-1 block text-xs text-red-400">
                {{ importProxyError }}
              </span>
              <span v-else class="mt-1 block text-xs text-gray-400">
                {{ t('accounts.proxyHint') }}
              </span>
            </label>
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.locale')
                }}</span>
                <input
                  v-model.trim="importEnvironment.locale"
                  class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                  required
                  autocomplete="off"
                />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm font-medium text-gray-400">{{
                  t('accounts.timezone')
                }}</span>
                <input
                  v-model.trim="importEnvironment.timezone"
                  class="w-full rounded border border-[#30363d] bg-[#0d1117] px-3 py-2 text-white transition focus:border-blue-500 focus:outline-none"
                  required
                  autocomplete="off"
                />
              </label>
            </div>
          </div>
          <div v-if="chromeProfiles.length === 0" class="py-8 text-center text-sm text-gray-400">
            {{ t('accounts.chromeEmpty') }}
          </div>
          <div v-else class="max-h-[50vh] space-y-2 overflow-auto">
            <label
              v-for="profile in chromeProfiles"
              :key="profile.profile"
              class="flex cursor-pointer items-start gap-3 rounded border border-[#30363d] bg-[#0d1117] p-3 transition hover:border-[#4b5563]"
            >
              <input
                v-model="selectedChromeProfiles"
                class="mt-1 h-4 w-4 shrink-0 accent-blue-600"
                type="checkbox"
                :value="profile.profile"
              />
              <span class="min-w-0 flex-1">
                <strong class="block truncate text-sm text-white">{{ profile.email }}</strong>
                <span class="block truncate text-xs text-gray-400">{{ profile.display_name }}</span>
                <span class="block truncate font-mono text-xs text-gray-400">{{
                  profile.profile
                }}</span>
              </span>
            </label>
          </div>
          <div class="mt-6 flex gap-2">
            <button
              class="flex-1 rounded bg-[#21262d] py-2 text-sm text-gray-300 transition hover:bg-[#30363d] disabled:opacity-50"
              type="button"
              :disabled="pendingAction === 'chrome-import'"
              @click="closeChromeImport"
            >
              {{ t('common.cancel') }}
            </button>
            <button
              class="flex-1 rounded bg-blue-600 py-2 text-sm text-white transition hover:bg-blue-500 disabled:opacity-50"
              type="submit"
              :disabled="pendingAction !== '' || selectedChromeProfiles.length === 0 || importProxyError !== ''"
            >
              {{ t('accounts.importSelected') }}
            </button>
          </div>
        </form>
      </div>
    </Teleport>
  </section>
</template>
