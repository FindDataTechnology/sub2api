// Package service — embedding 模型目录（fork 补丁，ciyuan-v0.2.16-16）。
//
// 问题（2026-10-28 生产实测）：聚合渠道（OpenRouter）的 /v1/models 目录
// **不列 embedding 模型**（实测 421 条零个 embedding），而 admin 后台的
// 「账号可用模型」与「分组模型白名单候选」两处都只认
//
//	上游 /v1/models  ∪  openai.DefaultModelIDs()
//
// 于是 embedding 账号在管理台里模型列表恒为空：看不见、也就无法在后台管理。
// 手写 credentials.model_mapping 也救不了——projectAccountModelsBody 投影时
// 要求目标模型存在于上游目录（byID），不存在即丢弃。
//
// 本文件只做一件事：给上面两处补一份**静态 embedding 目录**，且仅在
// 「上游目录/默认集里没有」时并入，既有任何行为逐字节不变。
// 「只增不改」：不碰路由语义、不碰转发路径、不碰准入判定
// （GroupModelAllowlist.Allows 与 account.ResolveMappedModel 均不受影响）；
// 请求真正能否成功仍由上游决定，本目录只影响**后台可见与可配置性**。
package service

import (
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// embeddingModelIDs 是本部署已知的 embedding 模型目录（后台展示用）。
//
// 只列**已在本系统实际承载流量**的模型，不做「猜上游有什么」：加错一条会让
// 管理台展示一个实际不可用的模型，比不显示更糟。新增模型时在此追加一行，
// 并同步 embeddings_provider_pin 的提供方说明。
var embeddingModelIDs = []string{
	"Qwen/Qwen3-Embedding-8B", // Nebius（经 OpenRouter 聚合，见 v-12 provider 钉扎）
	"text-embedding-3-large",  // OpenAI（openai-next 车道）
	"text-embedding-3-small",
	"text-embedding-ada-002",
}

// IsEmbeddingModelID 报告 id 是否为本部署登记的 embedding 模型。
// 判定为大小写不敏感的精确匹配：不做前缀/模糊匹配，避免把 chat 模型误并入。
func IsEmbeddingModelID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for _, m := range embeddingModelIDs {
		if strings.EqualFold(m, id) {
			return true
		}
	}
	return false
}

// EmbeddingModelIDs 返回 embedding 目录的副本（调用方可自由排序/拼接）。
func EmbeddingModelIDs() []string {
	out := make([]string, len(embeddingModelIDs))
	copy(out, embeddingModelIDs)
	return out
}

// withEmbeddingModels 把 embedding 目录并入 ids，**已存在的不重复**。
//
// 调用点纪律（与「只增不改」一致）：只在上游目录/默认集的结果上补齐，
// 绝不删改原有条目，因此既有账号的模型列表与本补丁前完全一致。
func withEmbeddingModels(ids []string) []string {
	seen := make(map[string]struct{}, len(ids)+len(embeddingModelIDs))
	for _, id := range ids {
		seen[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	out := make([]string, 0, len(ids)+len(embeddingModelIDs))
	out = append(out, ids...)
	for _, m := range embeddingModelIDs {
		if _, ok := seen[strings.ToLower(m)]; ok {
			continue
		}
		seen[strings.ToLower(m)] = struct{}{}
		out = append(out, m)
	}
	return out
}

// WithEmbeddingModelsFromMapping 把账号 model_mapping 里**被投影丢掉**的
// embedding 条目补回 picker 列表。
//
// 为什么投影会丢（2026-10-29 生产实测，账号 47 nebius-embedding-bulk）：
// GetAvailableModels 先走 FetchOpenAIAccountModels —— 那条路是
// 「上游 /v1/models → projectAccountModelsBody 按 mapping 投影」，投影要求
// 目标模型存在于上游目录（byID），不存在即 `continue` 丢弃。OpenRouter 的
// /v1/models 一条 embedding 都没有，于是 Qwen3-Embedding-8B 被丢光，接口
// **成功返回空数组**（err==nil），handler 里 `fetchErr == nil` 直接短路返回，
// 连 WithEmbeddingModelsForPicker 的回落都走不到。
//
// 判据取 mapping 而非全局目录：mapping 是运维对该账号「我能供这个模型」的显式
// 声明，比静态目录更准；再叠 IsEmbeddingModelID 把范围锁死在 embedding，
// chat 模型仍遵循「上游没列就不展示」的既有安全语义，既有账号零变化。
// 公开名优先（mapping 的 key），无 key 时退到目标名。
func WithEmbeddingModelsFromMapping(models []openai.Model, account *Account) []openai.Model {
	if account == nil {
		return models
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return models
	}
	seen := make(map[string]struct{}, len(models)+len(mapping))
	for _, m := range models {
		seen[strings.ToLower(strings.TrimSpace(m.ID))] = struct{}{}
	}
	// 稳定输出：mapping 是 map，遍历顺序随机，先收集再排序。
	extra := make([]string, 0, len(mapping))
	for publicID, target := range mapping {
		if strings.Contains(publicID, "*") {
			continue
		}
		if !IsEmbeddingModelID(publicID) && !IsEmbeddingModelID(target) {
			continue
		}
		id := strings.TrimSpace(publicID)
		if id == "" {
			id = strings.TrimSpace(target)
		}
		if id == "" {
			continue
		}
		if _, ok := seen[strings.ToLower(id)]; ok {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		extra = append(extra, id)
	}
	if len(extra) == 0 {
		return models
	}
	sort.Strings(extra)
	out := make([]openai.Model, 0, len(models)+len(extra))
	out = append(out, models...)
	for _, id := range extra {
		out = append(out, openai.Model{ID: id, Object: "model", Type: "model", DisplayName: id})
	}
	return out
}

// WithEmbeddingModelsForPicker 是 withEmbeddingModels 的 openai.Model 版本：
// admin「账号可用模型」列表回给前端的是 []openai.Model（含 object/type/
// display_name 三个 picker 字段），纯 embedding 账号的默认集里一条都没有，
// 列表因此恒空。已存在的条目原样保留（含顺序），只追加缺的 embedding 行。
func WithEmbeddingModelsForPicker(models []openai.Model) []openai.Model {
	seen := make(map[string]struct{}, len(models)+len(embeddingModelIDs))
	for _, m := range models {
		seen[strings.ToLower(strings.TrimSpace(m.ID))] = struct{}{}
	}
	out := make([]openai.Model, 0, len(models)+len(embeddingModelIDs))
	out = append(out, models...)
	for _, id := range embeddingModelIDs {
		if _, ok := seen[strings.ToLower(id)]; ok {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		out = append(out, openai.Model{ID: id, Object: "model", Type: "model", DisplayName: id})
	}
	return out
}
