import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import UsageMirrorsCard from '../UsageMirrorsCard.vue'
import type { UsageMirrorRow } from '@/api/usage'

const messages: Record<string, string> = {
  'usage.mirrors.title': 'Plan Usage',
  'usage.mirrors.description': 'Settled plan consumption mirrored from the billing side',
  'usage.mirrors.badge': 'Settled plan',
  'usage.mirrors.empty': 'No plan usage yet',
  'usage.mirrors.emptyHint': 'When a plan settlement is mirrored it shows up here.',
  'usage.mirrors.loadFailed': 'Could not load plan usage',
  'usage.mirrors.retry': 'Retry',
  'usage.mirrors.minutesUnit': 'min',
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => messages[key] ?? key,
    }),
  }
})

const row: UsageMirrorRow = {
  id: 7,
  external_id: 'wu_abc123',
  user_id: 42,
  minutes: 3,
  note: 'wanxing plan wp_1 (agent packs-p1-heal)',
  recorded_at: '2026-11-17T10:00:00Z',
}

describe('UsageMirrorsCard', () => {
  it('renders one mirrored settlement with its minutes and note', async () => {
    const fetcher = vi.fn().mockResolvedValue({ items: [row], total: 1 })
    const wrapper = mount(UsageMirrorsCard, { props: { fetcher } })

    // 挂载那一刻还在取数（loading 档），不为空列表——空列表会说「暂无套餐用量」，
    // 与「还没读到」是两件事。
    expect(wrapper.text()).not.toContain('No plan usage yet')
    await flushPromises()

    expect(fetcher).toHaveBeenCalledTimes(1)
    expect(fetcher.mock.calls[0][0]).toEqual({ limit: 20 })
    expect(wrapper.text()).toContain(row.note)
    expect(wrapper.text()).toContain('3 min')
    expect(wrapper.text()).not.toContain('No plan usage yet')
  })

  it('shows the empty state when the user has no mirrors', async () => {
    const fetcher = vi.fn().mockResolvedValue({ items: [], total: 0 })
    const wrapper = mount(UsageMirrorsCard, { props: { fetcher } })
    await flushPromises()

    expect(wrapper.text()).toContain('No plan usage yet')
  })

  // 镜像只是展示面：读失败要自己出「重试」，而不是让整页白掉。
  it('surfaces a retry when the read fails, and recovers on retry', async () => {
    const fetcher = vi
      .fn()
      .mockRejectedValueOnce(new Error('boom'))
      .mockResolvedValueOnce({ items: [row], total: 1 })
    const wrapper = mount(UsageMirrorsCard, { props: { fetcher } })
    await flushPromises()

    expect(wrapper.text()).toContain('Could not load plan usage')
    const retry = wrapper.findAll('button').find((b) => b.text() === 'Retry')
    expect(retry).toBeTruthy()

    await retry!.trigger('click')
    await flushPromises()

    expect(fetcher).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain(row.note)
    expect(wrapper.text()).not.toContain('Could not load plan usage')
  })

  // 形状防御：后端若回一个缺 items 的对象，卡片退化成空档而不是崩。
  it('treats a malformed payload as empty instead of throwing', async () => {
    const fetcher = vi.fn().mockResolvedValue({ total: 0 })
    const wrapper = mount(UsageMirrorsCard, { props: { fetcher } })
    await flushPromises()

    expect(wrapper.text()).toContain('No plan usage yet')
  })
})
