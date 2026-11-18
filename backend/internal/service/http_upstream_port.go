package service

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// 有界队列快速拒哨兵（add-sub2api-fork-concurrency 2.1）。repository 层的
// 账号信号量返回这两个值（errors.Is 可比）；handler 的 transport 错误分类
// 把它们映射成 429 + Retry-After 的终止性 failover（NextAccountStop）。
var (
	ErrAccountQueueFull   = errors.New("account queue full")
	ErrAccountTrippedAIMD = errors.New("account circuit open (aimd)")
)

// HTTPUpstream 上游 HTTP 请求接口
// 用于向上游 API（Claude、OpenAI、Gemini 等）发送请求
type HTTPUpstream interface {
	// Do 执行 HTTP 请求（不启用 TLS 指纹）
	Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)

	// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
	//
	// profile 参数:
	//   - nil: 不启用 TLS 指纹，行为与 Do 方法相同
	//   - non-nil: 使用指定的 Profile 进行 TLS 指纹伪装
	//
	// Profile 由调用方通过 TLSFingerprintProfileService 解析后传入，
	// 支持按账号绑定的数据库 profile 或内置默认 profile。
	DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error)

	// RecordAIMDResult 把「响应头已到但语义失败/恢复」的结果喂给 per-account
	// 容量状态机（add-sub2api-fork-concurrency 2.2）：5xx 计连击减半、429 作
	// 容量信号收窄、其余（2xx/4xx 语义错误）恢复。transport 层失败在 Do 内部
	// 自行记账，不经此口。未建队列的账号（还没发过请求）静默忽略。
	RecordAIMDResult(accountID int64, statusCode int)
}
