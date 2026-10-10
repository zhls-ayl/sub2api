import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import UsageCleanupDialog from '../UsageCleanupDialog.vue'
import Pagination from '@/components/common/Pagination.vue'

const { listCleanupTasks, createCleanupTask, showError } = vi.hoisted(() => ({
  listCleanupTasks: vi.fn(),
  createCleanupTask: vi.fn(),
  showError: vi.fn(),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
  }),
}))

vi.mock('@/api/admin/usage', () => ({
  default: {
    listCleanupTasks,
    createCleanupTask,
    cancelCleanupTask: vi.fn(),
  },
  adminUsageAPI: {
    listCleanupTasks,
    createCleanupTask,
    cancelCleanupTask: vi.fn(),
  },
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    usage: {
      searchUsers: vi.fn().mockResolvedValue([]),
      searchApiKeys: vi.fn().mockResolvedValue([]),
    },
    groups: {
      list: vi.fn().mockResolvedValue({ items: [] }),
    },
    dashboard: {
      getModelStats: vi.fn().mockResolvedValue({ models: [] }),
    },
    accounts: {
      list: vi.fn().mockResolvedValue({ items: [] }),
    },
  },
}))

enableAutoUnmount(afterEach)

beforeEach(() => {
  vi.clearAllMocks()
  vi.spyOn(console, 'error').mockImplementation(() => {})
  listCleanupTasks.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 5 })
  createCleanupTask.mockResolvedValue({})
})

afterEach(() => {
  vi.restoreAllMocks()
})

const page = (number: number) => ({ items: [{ id: number, status: 'succeeded', deleted_rows: number, filters: {} }], total: 15, page: number, page_size: 5 })

async function openDialog() {
  listCleanupTasks.mockResolvedValueOnce(page(1))
  const wrapper = mount(UsageCleanupDialog, {
    props: { show: false, filters: {}, startDate: '2026-09-01', endDate: '2026-09-02' },
    global: { stubs: { BaseDialog: { template: '<div><slot/><slot name="footer"/></div>' }, UsageFilters: true, ConfirmDialog: true, Pagination: true } },
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

describe('UsageCleanupDialog', () => {
  it('把外部模型选项传给清理筛选器', async () => {
    const wrapper = mount(UsageCleanupDialog, {
      props: {
        show: true,
        filters: {},
        startDate: '2026-07-01',
        endDate: '2026-07-01',
        modelOptions: ['claude-opus-4-8', 'gpt-5.4'],
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show'],
            template: '<section v-if="show"><slot /><slot name="footer" /></section>',
          },
          ConfirmDialog: true,
          Pagination: true,
          UsageFilters: {
            props: ['modelOptions'],
            template: '<div data-test="model-options">{{ modelOptions.join(",") }}</div>',
          },
        },
      },
    })

    await flushPromises()

    expect(wrapper.get('[data-test="model-options"]').text()).toBe('claude-opus-4-8,gpt-5.4')
  })

  it('提交弹窗中选择的所有用量明细筛选条件', async () => {
    const wrapper = mount(UsageCleanupDialog, {
      props: {
        show: false,
        filters: {},
        startDate: '2026-07-01',
        endDate: '2026-07-01',
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show'],
            template: '<section v-if="show"><slot /><slot name="footer" /></section>',
          },
          ConfirmDialog: {
            props: ['show'],
            template: '<button v-if="show" data-test="confirm" @click="$emit(\'confirm\')">confirm</button>',
          },
          Pagination: true,
          UsageFilters: {
            props: ['modelValue'],
            template: '<button data-test="select-filters" @click="$emit(\'update:modelValue\', { ...modelValue, native_compaction_v2: true, billing_mode: \'image\', upstream_model_mismatch: false })">select</button>',
          },
        },
      },
    })

    await wrapper.setProps({ show: true })
    await wrapper.get('[data-test="select-filters"]').trigger('click')
    const submit = wrapper.findAll('button').find((button) => button.text() === 'admin.usage.cleanup.submit')
    expect(submit).toBeTruthy()
    await submit!.trigger('click')
    await wrapper.get('[data-test="confirm"]').trigger('click')
    await flushPromises()

    expect(createCleanupTask).toHaveBeenCalledWith(expect.objectContaining({
      start_date: '2026-07-01',
      end_date: '2026-07-01',
      native_compaction_v2: true,
      billing_mode: 'image',
      upstream_model_mismatch: false,
    }))
    wrapper.unmount()
  })
})

describe('cleanup task pagination', () => {
  it.each(['success', 'failure'])('ignores an older page request that completes with %s', async (outcome) => {
    const wrapper = await openDialog()
    let resolve!: (value: object) => void
    let reject!: (error: Error) => void
    listCleanupTasks.mockImplementationOnce(() => new Promise((res, rej) => { resolve = res; reject = rej }))
    wrapper.findComponent(Pagination).vm.$emit('update:page', 2)
    await flushPromises()
    listCleanupTasks.mockResolvedValueOnce(page(3))
    wrapper.findComponent(Pagination).vm.$emit('update:page', 3)
    await flushPromises()
    if (outcome === 'success') resolve(page(2))
    else reject(new Error('old failure'))
    await flushPromises()
    expect(wrapper.findComponent(Pagination).props('page')).toBe(3)
    expect(wrapper.text()).toContain('#3')
    expect(showError).not.toHaveBeenCalled()
  })

  it('ignores a pending result after the dialog is closed and reopened', async () => {
    const wrapper = await openDialog()
    let resolve!: (value: object) => void
    listCleanupTasks.mockImplementationOnce(() => new Promise(res => { resolve = res }))
    wrapper.findComponent(Pagination).vm.$emit('update:page', 2)
    await flushPromises()
    await wrapper.setProps({ show: false })
    listCleanupTasks.mockResolvedValueOnce(page(1))
    await wrapper.setProps({ show: true })
    await flushPromises()
    resolve(page(2))
    await flushPromises()
    expect(wrapper.findComponent(Pagination).props('page')).toBe(1)
    expect(wrapper.text()).toContain('#1')
  })
})
