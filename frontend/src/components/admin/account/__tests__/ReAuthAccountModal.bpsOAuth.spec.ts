import { defineComponent, ref } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { generateAuthUrlMock } = vi.hoisted(() => ({ generateAuthUrlMock: vi.fn() }))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: { accounts: { applyOAuthCredentials: vi.fn() } }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

vi.mock('@/composables/useOpenAIOAuth', async () => {
  const actual = await vi.importActual<typeof import('@/composables/useOpenAIOAuth')>('@/composables/useOpenAIOAuth')
  return {
    ...actual,
    useOpenAIOAuth: () => ({
      authUrl: ref(''),
      sessionId: ref(''),
      oauthState: ref(''),
      loading: ref(false),
      error: ref(''),
      resetState: vi.fn(),
      generateAuthUrl: generateAuthUrlMock,
      exchangeAuthCode: vi.fn(),
      validateRefreshToken: vi.fn(),
      buildCredentials: vi.fn(() => ({})),
      buildExtraInfo: vi.fn(() => undefined)
    })
  }
})

import ReAuthAccountModal from '../ReAuthAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  props: { excelOauth: Boolean },
  emits: ['generate-url'],
  template: '<button data-testid="reauth-generate-url" @click="$emit(\'generate-url\')">generate</button>'
})

const mountModal = (type: string) =>
  mount(ReAuthAccountModal, {
    props: {
      show: false,
      account: { id: 31, name: 'OpenAI', platform: 'openai', type, credentials: {}, extra: {}, proxy_id: 7 } as any
    },
    global: {
      stubs: { BaseDialog: BaseDialogStub, Icon: true, Select: true, OAuthAuthorizationFlow: OAuthAuthorizationFlowStub }
    }
  })

describe('ReAuthAccountModal BPS OAuth', () => {
  beforeEach(() => {
    generateAuthUrlMock.mockReset().mockResolvedValue(true)
  })

  it('re-authorizes BPS OAuth accounts through the Excel client', async () => {
    const wrapper = mountModal('bps-oauth')
    await wrapper.setProps({ show: true })
    await flushPromises()
    await wrapper.get('[data-testid="reauth-generate-url"]').trigger('click')

    expect(generateAuthUrlMock).toHaveBeenCalledWith(7, undefined, 'excel')
    // The flow explains the bps.openai.com callback, not a localhost one.
    expect(wrapper.findComponent({ name: 'OAuthAuthorizationFlow' }).props('excelOauth')).toBe(true)
  })

  it('keeps Codex OAuth re-authorization on the Codex client', async () => {
    const wrapper = mountModal('oauth')
    await wrapper.setProps({ show: true })
    await flushPromises()
    await wrapper.get('[data-testid="reauth-generate-url"]').trigger('click')

    expect(generateAuthUrlMock).toHaveBeenCalledWith(7, undefined, 'codex')
  })
})
