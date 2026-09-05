// 代理输入的客户端校验与展示辅助。
// 协议白名单与后端 config.ValidateProxy 保持一致:
// 标准 http/https/socks5(无认证、无路径/参数)+ 分享链接
// vless/vmess/trojan/ss/hysteria/hysteria2/hy2/tuic/anytls。

const standardSchemes = new Set(['http', 'https', 'socks5'])
const shareSchemes = new Set([
  'vless',
  'vmess',
  'trojan',
  'ss',
  'hysteria',
  'hysteria2',
  'hy2',
  'tuic',
  'anytls',
])

// proxySchemeError 返回格式错误原因;空字符串表示通过。
// 仅做轻量语法检查,深度校验(分享链接参数)由服务端完成。
export function proxySchemeError(value: string): string {
  const trimmed = value.trim()
  if (trimmed === '') return ''
  const schemeMatch = /^([a-zA-Z][a-zA-Z0-9+.-]*):\/\//.exec(trimmed)
  if (schemeMatch === null) return 'scheme'
  const scheme = (schemeMatch[1] ?? '').toLowerCase()
  if (shareSchemes.has(scheme)) return ''
  if (!standardSchemes.has(scheme)) return 'scheme'
  // 标准代理:URL 形态须为 host:port,无认证与路径
  let url: URL
  try {
    url = new URL(trimmed)
  } catch {
    return 'url'
  }
  if (url.hostname === '') return 'url'
  if (url.username !== '' || url.password !== '') return 'auth'
  if (url.pathname !== '' && url.pathname !== '/' ) return 'path'
  if (url.search !== '' || url.hash !== '') return 'path'
  return ''
}

// maskProxyCredentials 展示代理值时隐藏 userinfo 密码,
// 例如 socks5://user:pass@host:1080 → socks5://user:•••@host:1080
export function maskProxyCredentials(value: string): string {
  const trimmed = value.trim()
  if (trimmed === '') return ''
  const schemeMatch = /^([a-zA-Z][a-zA-Z0-9+.-]*):\/\//.exec(trimmed)
  if (schemeMatch === null) return trimmed
  const scheme = (schemeMatch[1] ?? '').toLowerCase()
  if (!shareSchemes.has(scheme) && !standardSchemes.has(scheme)) return trimmed
  const rest = trimmed.slice(schemeMatch[0].length)
  const at = rest.lastIndexOf('@')
  if (at === -1) return trimmed
  const userinfo = rest.slice(0, at)
  const host = rest.slice(at + 1)
  const colon = userinfo.indexOf(':')
  if (colon === -1) return `${schemeMatch[0]}${userinfo}@${host}`
  const user = userinfo.slice(0, colon)
  if (user === '') return trimmed
  return `${schemeMatch[0]}${user}:•••@${host}`
}
