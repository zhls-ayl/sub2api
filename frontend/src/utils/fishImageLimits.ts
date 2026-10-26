// Fish 原生号的最大出图质量（存在账号 extra 里）。
// 请求高于上限时后端按上限向上游出图，响应仍回显客户请求的档位。

export const FISH_MAX_QUALITY_VALUES = ['low', 'medium', 'high', 'xhigh', 'max'] as const

// 从账号 extra 回显；缺失或无效值按不限。
export function fishMaxQualityFromExtra(extra?: Record<string, unknown> | null): string {
  const quality = typeof extra?.fish_max_quality === 'string' ? extra.fish_max_quality.trim().toLowerCase() : ''
  if ((FISH_MAX_QUALITY_VALUES as readonly string[]).includes(quality)) return quality
  return ''
}

// 把表单写回 extra。始终写出 fish_max_quality，不限时写空串，保证能清除旧上限。
export function applyFishMaxQualityToExtra(
  extra: Record<string, unknown>,
  maxQuality: string
): Record<string, unknown> {
  const quality = (maxQuality || '').trim().toLowerCase()
  return {
    ...extra,
    fish_max_quality: (FISH_MAX_QUALITY_VALUES as readonly string[]).includes(quality) ? quality : ''
  }
}
