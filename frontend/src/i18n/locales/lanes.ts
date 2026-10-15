// 词元车道 i18n 词条（fork 缝点：i18n 词条，ADR-0024 / 台账§二）。
// 三车道入口页 + 侧边导航标签；端点/形状与后端 lanes 包契约同源。
export const lanesZh = {
  lanes: {
    navLabel: '模型车道',
    title: '模型车道',
    subtitle: '在 chat 之外，用同一把词元 API key 直接消费语音、重排与搜索能力',
    authNote: '三车道与 chat 共用同一鉴权链与账本：同一把 key、同一余额与消费窗口、同一用量面板。',
    endpointsTitle: '端点',
    shapeTitle: '请求与响应形状',
    billingTitle: '计量口径',
    errorsTitle: '错误面',
    exampleTitle: '调用示例',
    columns: {
      lane: '车道',
      endpoint: '端点',
      protocol: '协议',
      billing: '计量',
      upstream: '上游',
    },
    rows: {
      rerank: {
        lane: '重排 rerank',
        protocol: 'Cohere/Jina 式 /v1/rerank',
        billing: '按请求（分组 rerank 单价）',
      },
      websearch: {
        lane: '搜索 websearch',
        protocol: '自有统一协议（engine + 统一信封）',
        billing: '按次（分组搜索单价）',
      },
      audioStt: {
        lane: '语音转写 ASR',
        protocol: 'OpenAI /v1/audio/transcriptions',
        billing: '按音频时长（小时）',
      },
      audioTts: {
        lane: '语音合成 TTS',
        protocol: 'OpenAI /v1/audio/speech',
        billing: '按合成字符数（百万字符）',
      },
    },
    shapes: {
      rerank: "请求 {'{'}query, documents[], top_n{'}'}；响应 {'{'}results:[{'{'}index, relevance_score{'}'}]{'}'}",
      websearch: "请求 {'{'}engine, q, num{'}'}；响应 {'{'}meta:{'{'}engine, latency_ms{'}'}, results:[{'{'}title, url, snippet, position{'}'}], usage:{'{'}requests{'}'}{'}'}",
      asr: 'multipart：file + model + response_format(json|text|verbose_json)',
      tts: "请求 {'{'}input, voice, model, response_format{'}'}；响应为音频二进制流",
    },
    billingNote: '单价在管理端按分组配置；余额/消费窗口耗尽时车道请求与 chat 同口径拒绝，不会触达上游。',
    errors: {
      invalid: '400 调用者请求形状/内容问题',
      unavailable: '503 车道暂无可用上游账号',
      upstream: '502 上游故障（断连/超时/异常），均不外泄上游细节、不计费',
    },
    exampleNote: '把 {apiKey} 换成你的词元 key，{base} 换成实例地址。',
  },
}

export const lanesEn = {
  lanes: {
    navLabel: 'Model Lanes',
    title: 'Model Lanes',
    subtitle: 'Beyond chat: consume speech, rerank and search with the same ciyuan API key',
    authNote: 'All lanes share the same auth chain and ledger as chat: same key, same balance and windows, same usage panel.',
    endpointsTitle: 'Endpoints',
    shapeTitle: 'Request & response shapes',
    billingTitle: 'Metering',
    errorsTitle: 'Error surface',
    exampleTitle: 'Examples',
    columns: {
      lane: 'Lane',
      endpoint: 'Endpoint',
      protocol: 'Protocol',
      billing: 'Metering',
      upstream: 'Upstream',
    },
    rows: {
      rerank: {
        lane: 'Rerank',
        protocol: 'Cohere/Jina-style /v1/rerank',
        billing: 'Per request (group rerank price)',
      },
      websearch: {
        lane: 'Web search',
        protocol: 'Own unified protocol (engine + envelope)',
        billing: 'Per call (group search price)',
      },
      audioStt: {
        lane: 'Speech-to-text',
        protocol: 'OpenAI /v1/audio/transcriptions',
        billing: 'Per audio duration (hours)',
      },
      audioTts: {
        lane: 'Text-to-speech',
        protocol: 'OpenAI /v1/audio/speech',
        billing: 'Per characters (million chars)',
      },
    },
    shapes: {
      rerank: "Request {'{'}query, documents[], top_n{'}'}; response {'{'}results:[{'{'}index, relevance_score{'}'}]{'}'}",
      websearch: "Request {'{'}engine, q, num{'}'}; response {'{'}meta:{'{'}engine, latency_ms{'}'}, results:[{'{'}title, url, snippet, position{'}'}], usage:{'{'}requests{'}'}{'}'}",
      asr: 'multipart: file + model + response_format(json|text|verbose_json)',
      tts: "Request {'{'}input, voice, model, response_format{'}'}; response is an audio byte stream",
    },
    billingNote: 'Unit prices are configured per group in admin; exhausted balance/windows reject lane requests with the same semantics as chat, without touching upstreams.',
    errors: {
      invalid: '400 caller request shape/content problem',
      unavailable: '503 no available lane upstream account',
      upstream: '502 upstream fault (refused/timeout/abnormal), no upstream details leaked, never billed',
    },
    exampleNote: 'Replace {apiKey} with your ciyuan key and {base} with the instance address.',
  },
}
