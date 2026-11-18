package lanes

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 音频可达化：部分上游（豆包录音文件识别）只接受**公网可访问的音频 URL**，
// 不接受请求体内的字节。调用者按 OpenAI 形状上传的是字节，故适配层必须自己
// 把字节落成上游可取到的 URL。
//
// 实现取向（v1）：落到车道进程可读写的临时目录，并以**账号凭据里的
// audio_public_base_url** 拼出对外可达地址（由部署侧提供反代/对象存储前缀）。
// 未配置该前缀时无法满足上游要求——返回明确错误而非静默降级（避免「看起来
// 成功但转写的是空音频」）。
//
// 生命周期：文件在发布后按 TTL 清理（后台惰性清扫 + 每次发布顺带清理），
// 时间窗取上游任务完成上限（3 小时）与磁盘占用的折中。
const (
	audioSpoolDirEnv   = "LANE_AUDIO_SPOOL_DIR"
	audioSpoolTTL      = 4 * time.Hour // 覆盖上游「任务 3 小时内完成」
	audioSpoolMaxBytes = 512 << 20     // 与豆包单文件上限一致
)

var (
	audioSpoolMu   sync.Mutex
	audioSpoolLast time.Time
)

// publishAudioForUpstream 把上传字节发布为上游可取的 URL。
//
// 前缀来源优先级：账号凭据 audio_public_base_url > 环境变量
// LANE_AUDIO_PUBLIC_BASE_URL。前者支持多部署/多租户各自不同的对外域；
// 两者皆空时**明确报错**——上游要求公网可达 URL 无法满足，静默降级会导致
// 「看起来成功、实际转写空音频」这类最坏缺陷。
func publishAudioForUpstream(form *asrFormData, accountPublicBase string) (string, error) {
	if form == nil || len(form.fileBytes) == 0 {
		return "", fmt.Errorf("no audio bytes to publish")
	}
	if len(form.fileBytes) > audioSpoolMaxBytes {
		return "", fmt.Errorf("audio exceeds upstream limit (%d bytes)", audioSpoolMaxBytes)
	}
	base := strings.TrimSpace(accountPublicBase)
	if base == "" {
		base = strings.TrimSpace(os.Getenv("LANE_AUDIO_PUBLIC_BASE_URL"))
	}
	if base == "" {
		return "", fmt.Errorf("audio public base URL is not configured (set account credential audio_public_base_url or env LANE_AUDIO_PUBLIC_BASE_URL); upstream requires a publicly reachable audio URL")
	}
	dir := strings.TrimSpace(os.Getenv(audioSpoolDirEnv))
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "lane-audio-spool")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create audio spool dir: %w", err)
	}

	// 文件名不受用户输入影响：randomSpoolName 生成随机名，用户只贡献扩展名，
	// 而扩展名走 audioFileExt 的**白名单**（8 个固定值，见该函数）。
	// gosec 的污点分析看不穿这两层，故此处显式豁免。
	name := randomSpoolName(audioFileExt(form.fileName))
	path := filepath.Join(dir, name) //nolint:gosec // 见上：随机名 + 扩展名白名单
	if err := os.WriteFile(path, form.fileBytes, 0o600); err != nil {
		return "", fmt.Errorf("write audio spool file: %w", err)
	}
	sweepAudioSpool(dir)
	return strings.TrimRight(base, "/") + "/" + name, nil
}

// randomSpoolName 生成不可猜测的文件名（URL 即凭据，不可枚举）。
func randomSpoolName(ext string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	}
	return hex.EncodeToString(b) + ext
}

// audioFileExt 取上传文件的安全扩展名白名单（防路径穿越）。
func audioFileExt(fileName string) string {
	lower := strings.ToLower(fileName)
	if i := strings.LastIndex(lower, "."); i >= 0 {
		switch ext := lower[i:]; ext {
		case ".wav", ".mp3", ".ogg", ".spx", ".amr", ".aac", ".m4a", ".pcm":
			return ext
		}
	}
	return ".wav"
}

// sweepAudioSpool 惰性清理过期文件（节流：每小时最多一次）。
func sweepAudioSpool(dir string) {
	audioSpoolMu.Lock()
	if time.Since(audioSpoolLast) < time.Hour {
		audioSpoolMu.Unlock()
		return
	}
	audioSpoolLast = time.Now()
	audioSpoolMu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-audioSpoolTTL)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		// e.Name() 来自 os.ReadDir 的目录条目，不是用户输入。
		_ = os.Remove(filepath.Join(dir, e.Name())) //nolint:gosec // 名字来自 ReadDir
	}
}

// 编译期确认 http 仍被本文件所在包使用（分派层引用）。
var _ = http.StatusOK
