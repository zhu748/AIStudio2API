// 时间格式化的实例缓存:每行日志/请求直接复用同一 Intl.DateTimeFormat,
// 避免高吞吐列表渲染时逐行构造格式化器(每实例约数 KB 分配)。

const timeFormatters = new Map<string, Intl.DateTimeFormat>()
const dateTimeFormatters = new Map<string, Intl.DateTimeFormat>()

// formatClockTime 输出 HH:MM:SS(24 小时制,语言感知)。
export function formatClockTime(value: string, locale: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  let formatter = timeFormatters.get(locale)
  if (formatter === undefined) {
    formatter = new Intl.DateTimeFormat(locale, {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    })
    timeFormatters.set(locale, formatter)
  }
  return formatter.format(date)
}

// formatDateTime 输出 MM-DD HH:MM:SS(语言感知)。
export function formatDateTime(value: string, locale: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  let formatter = dateTimeFormatters.get(locale)
  if (formatter === undefined) {
    formatter = new Intl.DateTimeFormat(locale, {
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    })
    dateTimeFormatters.set(locale, formatter)
  }
  return formatter.format(date)
}
