import { describe, expect, it } from 'vitest'
import { applyFishMaxQualityToExtra, fishMaxQualityFromExtra } from '../fishImageLimits'

describe('fishImageLimits', () => {
  it('reads a known quality and ignores invalid values', () => {
    expect(fishMaxQualityFromExtra({ fish_max_quality: ' HIGH ' })).toBe('high')
    expect(fishMaxQualityFromExtra({ fish_max_quality: ' MAX ' })).toBe('max')
    expect(fishMaxQualityFromExtra({ fish_max_quality: 'xhigh' })).toBe('xhigh')
    expect(fishMaxQualityFromExtra({ fish_max_quality: 'ultra' })).toBe('')
    expect(fishMaxQualityFromExtra({ fish_max_quality: 3 })).toBe('')
    expect(fishMaxQualityFromExtra(null)).toBe('')
  })

  it('always writes the key so an old cap can be cleared', () => {
    expect(applyFishMaxQualityToExtra({ keep: true }, 'Medium')).toEqual({
      keep: true,
      fish_max_quality: 'medium'
    })
    expect(applyFishMaxQualityToExtra({ fish_max_quality: 'high' }, '')).toEqual({
      fish_max_quality: ''
    })
    expect(applyFishMaxQualityToExtra({}, 'ultra')).toEqual({ fish_max_quality: '' })
  })
})
