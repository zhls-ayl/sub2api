import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: { value: false }, copyToClipboard: vi.fn() })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import OAuthAuthorizationFlow from '../OAuthAuthorizationFlow.vue'

const mountFlow = (platform: string, excelOauth: boolean) =>
  mount(OAuthAuthorizationFlow, {
    props: {
      addMethod: 'oauth',
      platform,
      excelOauth,
      authUrl: 'https://auth.openai.com/oauth/authorize?client_id=excel',
      sessionId: 'session-1'
    },
    global: { stubs: { Icon: true } }
  })

describe('OAuthAuthorizationFlow Excel (BPS) OAuth', () => {
  it('describes the bps.openai.com callback instead of localhost', async () => {
    const wrapper = mountFlow('openai', true)
    const text = wrapper.text()

    expect(text).toContain('admin.accounts.oauth.openai.excelTitle')
    expect(wrapper.find('[data-testid="excel-oauth-login-hint"]').exists()).toBe(false)
    expect(text).toContain('admin.accounts.oauth.openai.excelCodeHint')
    expect(text).not.toContain('admin.accounts.oauth.openai.importantNotice')
    expect(text).not.toContain('admin.accounts.oauth.openai.authCodeDesc')
    const textarea = wrapper.findAll('textarea').at(-1)
    expect(textarea?.attributes('placeholder')).toBe('admin.accounts.oauth.openai.excelCodePlaceholder')

    await textarea!.setValue('https://bps.openai.com/basispoints/extension/x/auth/callback?code=test-code&state=bps.test')
    expect((wrapper.vm as unknown as { oauthState: string }).oauthState).toBe('bps.test')
  })

  it('keeps the Codex and other providers wording', () => {
    const codex = mountFlow('openai', false).text()
    expect(codex).toContain('admin.accounts.oauth.openai.importantNotice')
    expect(mountFlow('anthropic', true).text()).not.toContain('excel')
  })
})
