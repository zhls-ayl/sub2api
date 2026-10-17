import { describe, expect, it } from 'vitest'
import {
  applyLeonardoImageLimitsToExtra,
  defaultLeonardoImageLimits,
  leonardoImageLimitsFromExtra
} from '../leonardoImageLimits'

describe('leonardoImageLimits', () => {
  it('reads limits from extra and falls back to defaults', () => {
    expect(leonardoImageLimitsFromExtra(undefined)).toEqual(defaultLeonardoImageLimits())
    expect(
      leonardoImageLimitsFromExtra({
        leonardo_max_quality: ' MEDIUM ',
        leonardo_max_short_edge: '1024',
        leonardo_max_upscale_factor: 1.5
      })
    ).toEqual({ maxQuality: 'medium', maxShortEdge: 1024, maxUpscaleFactor: 1.5 })
    expect(
      leonardoImageLimitsFromExtra({ leonardo_max_quality: 'ultra', leonardo_max_short_edge: -1, leonardo_max_upscale_factor: 'x' })
    ).toEqual(defaultLeonardoImageLimits())
  })

  it('always writes all three keys so old limits can be cleared', () => {
    expect(applyLeonardoImageLimitsToExtra({ keep: true }, { maxQuality: '', maxShortEdge: NaN, maxUpscaleFactor: 0 })).toEqual({
      keep: true,
      leonardo_max_quality: '',
      leonardo_max_short_edge: 0,
      leonardo_max_upscale_factor: 2
    })
    expect(applyLeonardoImageLimitsToExtra({}, { maxQuality: 'High', maxShortEdge: 1023.6, maxUpscaleFactor: 3 })).toEqual({
      leonardo_max_quality: 'high',
      leonardo_max_short_edge: 1024,
      leonardo_max_upscale_factor: 3
    })
  })
})
