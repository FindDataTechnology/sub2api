<template>
  <AppLayout>
    <div class="mx-auto max-w-5xl px-4 py-8 sm:px-6 lg:py-10">
      <header class="mb-8">
        <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">{{ t('lanes.title') }}</h1>
        <p class="mt-2 text-sm leading-6 text-gray-600 dark:text-gray-300">{{ t('lanes.subtitle') }}</p>
        <p class="mt-3 rounded-lg border border-gray-200 bg-gray-50 px-4 py-3 text-sm text-gray-600 dark:border-dark-700 dark:bg-dark-800 dark:text-gray-300">
          {{ t('lanes.authNote') }}
        </p>
      </header>

      <section class="mb-8">
        <h2 class="mb-3 text-base font-semibold text-gray-900 dark:text-white">{{ t('lanes.endpointsTitle') }}</h2>
        <div class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 dark:bg-dark-800">
              <tr>
                <th class="px-4 py-3 text-left font-medium text-gray-600 dark:text-gray-300">{{ t('lanes.columns.lane') }}</th>
                <th class="px-4 py-3 text-left font-medium text-gray-600 dark:text-gray-300">{{ t('lanes.columns.endpoint') }}</th>
                <th class="px-4 py-3 text-left font-medium text-gray-600 dark:text-gray-300">{{ t('lanes.columns.protocol') }}</th>
                <th class="px-4 py-3 text-left font-medium text-gray-600 dark:text-gray-300">{{ t('lanes.columns.billing') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 bg-white dark:divide-dark-700 dark:bg-dark-900">
              <tr v-for="row in rows" :key="row.endpoint">
                <td class="whitespace-nowrap px-4 py-3 font-medium text-gray-900 dark:text-white">{{ t(row.laneKey) }}</td>
                <td class="whitespace-nowrap px-4 py-3 font-mono text-xs text-primary-600 dark:text-primary-400">{{ row.endpoint }}</td>
                <td class="px-4 py-3 text-gray-600 dark:text-gray-300">{{ t(row.protocolKey) }}</td>
                <td class="px-4 py-3 text-gray-600 dark:text-gray-300">{{ t(row.billingKey) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section class="mb-8">
        <h2 class="mb-3 text-base font-semibold text-gray-900 dark:text-white">{{ t('lanes.shapeTitle') }}</h2>
        <ul class="space-y-2">
          <li v-for="row in rows" :key="row.endpoint" class="rounded-lg border border-gray-200 bg-white px-4 py-3 dark:border-dark-700 dark:bg-dark-900">
            <div class="mb-1 font-mono text-xs text-primary-600 dark:text-primary-400">{{ row.endpoint }}</div>
            <div class="text-sm text-gray-600 dark:text-gray-300">{{ t(row.shapeKey) }}</div>
          </li>
        </ul>
      </section>

      <section class="mb-8">
        <h2 class="mb-3 text-base font-semibold text-gray-900 dark:text-white">{{ t('lanes.billingTitle') }}</h2>
        <p class="rounded-lg border border-gray-200 bg-white px-4 py-3 text-sm text-gray-600 dark:border-dark-700 dark:bg-dark-900 dark:text-gray-300">
          {{ t('lanes.billingNote') }}
        </p>
      </section>

      <section class="mb-8">
        <h2 class="mb-3 text-base font-semibold text-gray-900 dark:text-white">{{ t('lanes.errorsTitle') }}</h2>
        <ul class="space-y-2 text-sm text-gray-600 dark:text-gray-300">
          <li class="rounded-lg border border-gray-200 bg-white px-4 py-3 dark:border-dark-700 dark:bg-dark-900">{{ t('lanes.errors.invalid') }}</li>
          <li class="rounded-lg border border-gray-200 bg-white px-4 py-3 dark:border-dark-700 dark:bg-dark-900">{{ t('lanes.errors.unavailable') }}</li>
          <li class="rounded-lg border border-gray-200 bg-white px-4 py-3 dark:border-dark-700 dark:bg-dark-900">{{ t('lanes.errors.upstream') }}</li>
        </ul>
      </section>

      <section>
        <h2 class="mb-3 text-base font-semibold text-gray-900 dark:text-white">{{ t('lanes.exampleTitle') }}</h2>
        <p class="mb-3 text-sm text-gray-600 dark:text-gray-300">{{ t('lanes.exampleNote', { apiKey: 'ciyuan-...', base: exampleBase }) }}</p>
        <pre class="overflow-x-auto rounded-lg bg-gray-900 px-4 py-4 text-xs leading-6 text-gray-100 dark:bg-dark-950">{{ examples }}</pre>
      </section>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'

const { t } = useI18n()

const rows = [
  { laneKey: 'lanes.rows.rerank.lane', endpoint: 'POST /v1/rerank', protocolKey: 'lanes.rows.rerank.protocol', billingKey: 'lanes.rows.rerank.billing', shapeKey: 'lanes.shapes.rerank' },
  { laneKey: 'lanes.rows.websearch.lane', endpoint: 'POST /v1/search', protocolKey: 'lanes.rows.websearch.protocol', billingKey: 'lanes.rows.websearch.billing', shapeKey: 'lanes.shapes.websearch' },
  { laneKey: 'lanes.rows.audioStt.lane', endpoint: 'POST /v1/audio/transcriptions', protocolKey: 'lanes.rows.audioStt.protocol', billingKey: 'lanes.rows.audioStt.billing', shapeKey: 'lanes.shapes.asr' },
  { laneKey: 'lanes.rows.audioTts.lane', endpoint: 'POST /v1/audio/speech', protocolKey: 'lanes.rows.audioTts.protocol', billingKey: 'lanes.rows.audioTts.billing', shapeKey: 'lanes.shapes.tts' },
]

// 示例里的 base 取当前站点 origin，不写死任何部署域名（公开仓不得含内部域名）。
const exampleBase = typeof window !== 'undefined' ? window.location.origin : 'https://your-gateway.example.com'

const examples = computed(() =>
  [
    `# rerank`,
    `curl -X POST {base}/v1/rerank \\`,
    `  -H "Authorization: Bearer {apiKey}" -H "Content-Type: application/json" \\`,
    `  -d '{"query":"如何重置密码","documents":["文档A","文档B"],"top_n":2}'`,
    ``,
    `# websearch（统一信封）`,
    `curl -X POST {base}/v1/search \\`,
    `  -H "Authorization: Bearer {apiKey}" -H "Content-Type: application/json" \\`,
    `  -d '{"engine":"serpapi","q":"壹座平台","num":5}'`,
    ``,
    `# ASR 转写`,
    `curl -X POST {base}/v1/audio/transcriptions \\`,
    `  -H "Authorization: Bearer {apiKey}" \\`,
    `  -F file=@clip.mp3 -F model=FunAudioLLM/SenseVoiceSmall`,
    ``,
    `# TTS 合成`,
    `curl -X POST {base}/v1/audio/speech \\`,
    `  -H "Authorization: Bearer {apiKey}" -H "Content-Type: application/json" \\`,
    `  -d '{"input":"你好，壹座","voice":"FunAudioLLM/CosyVoice2-0.5B:alex"}' --output out.mp3`,
  ].join('\n').split('{base}').join(exampleBase).split('{apiKey}').join('ciyuan-...')
)
</script>
