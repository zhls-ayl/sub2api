// Leonardo Cookie 账号的出图上限（存在账号 extra 里）。超过上限时后端按上限出图，
// 尺寸再在网关本地等比超分回客户请求的尺寸；计费按交付尺寸。

export const LEONARDO_MAX_QUALITY_VALUES = ['low', 'medium', 'high', 'xhigh'] as const

export const LEONARDO_DEFAULT_MAX_UPSCALE_FACTOR = 2

export interface LeonardoImageLimitsForm {
  // '' 表示不限
  maxQuality: string
  // 短边上限（像素），0 表示不限
  maxShortEdge: number
  // 最大放大倍数，默认 2
  maxUpscaleFactor: number
}

export function defaultLeonardoImageLimits(): LeonardoImageLimitsForm {
  return { maxQuality: '', maxShortEdge: 0, maxUpscaleFactor: LEONARDO_DEFAULT_MAX_UPSCALE_FACTOR }
}

function toFiniteNumber(value: unknown): number | null {
  const parsed = typeof value === 'number' ? value : typeof value === 'string' && value.trim() !== '' ? Number(value) : NaN
  return Number.isFinite(parsed) ? parsed : null
}

// 从账号 extra 回显；缺失或无效值按默认（不限 / 2 倍）。
export function leonardoImageLimitsFromExtra(extra?: Record<string, unknown> | null): LeonardoImageLimitsForm {
  const form = defaultLeonardoImageLimits()
  const quality = typeof extra?.leonardo_max_quality === 'string' ? extra.leonardo_max_quality.trim().toLowerCase() : ''
  if ((LEONARDO_MAX_QUALITY_VALUES as readonly string[]).includes(quality) || quality === 'max') {
    form.maxQuality = quality
  }
  const shortEdge = toFiniteNumber(extra?.leonardo_max_short_edge)
  if (shortEdge !== null && shortEdge > 0) form.maxShortEdge = Math.round(shortEdge)
  const factor = toFiniteNumber(extra?.leonardo_max_upscale_factor)
  if (factor !== null && factor > 0) form.maxUpscaleFactor = factor
  return form
}

// 把表单写回 extra（三个 key 始终写出，不限时写中性值，保证能清除旧上限）。
export function applyLeonardoImageLimitsToExtra(
  extra: Record<string, unknown>,
  form: LeonardoImageLimitsForm
): Record<string, unknown> {
  const shortEdge = toFiniteNumber(form.maxShortEdge)
  const factor = toFiniteNumber(form.maxUpscaleFactor)
  return {
    ...extra,
    leonardo_max_quality: (form.maxQuality || '').trim().toLowerCase(),
    leonardo_max_short_edge: shortEdge !== null && shortEdge > 0 ? Math.round(shortEdge) : 0,
    leonardo_max_upscale_factor: factor !== null && factor > 0 ? factor : LEONARDO_DEFAULT_MAX_UPSCALE_FACTOR
  }
}
