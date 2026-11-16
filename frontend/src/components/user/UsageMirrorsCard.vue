<template>
  <div class="card">
    <div class="flex items-center justify-between border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div>
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('usage.mirrors.title') }}</h2>
        <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('usage.mirrors.description') }}</p>
      </div>
      <span class="badge badge-gray">{{ t('usage.mirrors.badge') }}</span>
    </div>

    <div class="p-6">
      <div v-if="loading" class="flex items-center justify-center py-8">
        <LoadingSpinner size="lg" />
      </div>

      <div v-else-if="failed" class="flex flex-col items-center gap-3 py-6">
        <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('usage.mirrors.loadFailed') }}</p>
        <button type="button" class="btn btn-secondary btn-sm" @click="load">
          {{ t('usage.mirrors.retry') }}
        </button>
      </div>

      <div v-else-if="rows.length === 0" class="py-4">
        <EmptyState
          :title="t('usage.mirrors.empty')"
          :description="t('usage.mirrors.emptyHint')"
        />
      </div>

      <div v-else class="space-y-3">
        <div
          v-for="row in rows"
          :key="row.id"
          class="flex items-start justify-between gap-4 rounded-xl bg-gray-50 p-4 dark:bg-dark-800/50"
        >
          <div class="flex min-w-0 items-start gap-3">
            <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-primary-100 dark:bg-primary-900/30">
              <Icon name="beaker" size="sm" class="text-primary-600 dark:text-primary-400" />
            </div>
            <div class="min-w-0">
              <p class="truncate text-sm font-medium text-gray-900 dark:text-white">{{ row.note || row.external_id }}</p>
              <p class="text-xs text-gray-500 dark:text-dark-400">{{ formatDateTime(row.recorded_at) }}</p>
            </div>
          </div>
          <span class="badge badge-primary shrink-0">
            {{ row.minutes }} {{ t('usage.mirrors.minutesUnit') }}
          </span>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Icon from '@/components/icons/Icon.vue'
import { formatDateTime } from '@/utils/format'
import { usageAPI, type UsageMirrorRow } from '@/api/usage'

type MirrorFetcher = (
  params: { limit?: number },
  config?: { signal?: AbortSignal }
) => Promise<{ items: UsageMirrorRow[]; total: number }>

// fetcher 可注入：缺省走 usageAPI.listMirrors，单测里换成桩就能在不碰网络的前提下
// 走完 loading → rows / 失败 → 重试 三条路。
const props = withDefaults(
  defineProps<{
    limit?: number
    fetcher?: MirrorFetcher
  }>(),
  { limit: 20 }
)

const { t } = useI18n()
const rows = ref<UsageMirrorRow[]>([])
const loading = ref(true)
const failed = ref(false)

async function load() {
  loading.value = true
  failed.value = false
  try {
    const fetcher = props.fetcher ?? usageAPI.listMirrors
    const res = await fetcher({ limit: props.limit })
    rows.value = Array.isArray(res?.items) ? res.items : []
  } catch {
    // 镜像只是展示面：读不到就打一行失败与重试，不影响页面上任何计费/配额读数。
    failed.value = true
    rows.value = []
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>
