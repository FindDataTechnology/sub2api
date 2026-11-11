// credential-backfill 补齐 accounts / api_keys 的凭据派生列（落盘加密阶段二的前置）。
//
// 为什么必须在阶段二**之前**跑完：
// 阶段二会把 SQL 谓词从 `credentials IS DISTINCT FROM $1::jsonb` 换成
// `credentials_hash IS DISTINCT FROM $1`。而 `NULL IS DISTINCT FROM x` 恒为真，
// 所以只要还有一行的 hash 是 NULL，它就会被当成"整份凭据已变化"，触发
// 上游探测 / 用量快照的清理分支，把没坏的受管状态清掉。
// 也就是说这不是"最好先做"，而是"不先做就会误清数据"。
//
// 用法：
//
//	# 1) 先演练（默认就是演练，零写入）
//	go run ./cmd/credential-backfill -dsn "$DSN"
//
//	# 2) 小批量试跑（比如 50 行）
//	go run ./cmd/credential-backfill -dsn "$DSN" -apply -limit 50
//
//	# 3) 全量
//	go run ./cmd/credential-backfill -dsn "$DSN" -apply
//
// 密钥来源与线上一致：env TOTP_ENCRYPTION_KEY（64 位 hex）。不传 -key 时读该环境变量。
// 用错密钥会让新旧密文无法互解，所以**换钥前必须先跑完回填**，换钥＝全量重加密，须单独立项。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
)

func main() {
	var (
		dsn       = flag.String("dsn", "", "PostgreSQL DSN（必填），例如 postgres://user:pass@host:5432/sub2api?sslmode=disable")
		keyHex    = flag.String("key", "", "AES-256 密钥（64 位 hex）。留空则读 env TOTP_ENCRYPTION_KEY")
		batchSize = flag.Int("batch", 200, "每批行数")
		limit     = flag.Int("limit", 0, "**每张表**本次最多处理多少行（0=不限）；建议生产先跑 50。accounts 与 api_keys 各算各的，-limit 50 最多写 100 行")
		apply     = flag.Bool("apply", false, "真正写库。不加此参数为演练模式（零写入）")
	)
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 -dsn")
		os.Exit(2)
	}
	key := *keyHex
	if key == "" {
		key = os.Getenv("TOTP_ENCRYPTION_KEY")
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "缺少密钥：请传 -key 或设置 env TOTP_ENCRYPTION_KEY")
		os.Exit(2)
	}

	cryptor, err := repository.NewAESEncryptor(&config.Config{
		Totp: config.TotpConfig{EncryptionKey: key},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "构造加密器失败：%v\n", err)
		os.Exit(2)
	}

	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接数据库失败：%v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mode := "DRY-RUN（零写入）"
	if *apply {
		mode = "APPLY（写库）"
	}
	fmt.Printf("[credential-backfill] mode=%s batch=%d limit=%d\n", mode, *batchSize, *limit)
	if !*apply {
		fmt.Println("[credential-backfill] 这是演练。确认数字合理后加 -apply 才会真正写入。")
	}

	started := time.Now()
	result, err := repository.BackfillCredentialColumns(ctx, db, cryptor, repository.CredentialBackfillOptions{
		BatchSize: *batchSize,
		Limit:     *limit,
		DryRun:    !*apply,
		Logger: func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "回填失败：%v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[credential-backfill] 完成，耗时 %s\n", time.Since(started).Round(time.Millisecond))
	// accounts 没有 skipped 一说：判据只选出"派生列还缺"的行，空凭据也必须被派生
	// （否则 credentials_encrypted 永远是 NULL、闸门永远过不去）。
	fmt.Printf("  accounts: scanned=%d updated=%d failed=%d\n",
		result.AccountsScanned, result.AccountsUpdated, result.AccountsFailed)
	fmt.Printf("  api_keys: scanned=%d updated=%d skipped=%d failed=%d\n",
		result.APIKeysScanned, result.APIKeysUpdated, result.APIKeysSkipped, result.APIKeysFailed)
	if result.APIKeysSkipped > 0 {
		fmt.Printf("[credential-backfill] 注意：%d 个 api_key 是空的（无 key 可派生），已跳过、非失败。\n",
			result.APIKeysSkipped)
	}

	if result.AccountsFailed > 0 || result.APIKeysFailed > 0 {
		fmt.Println("[credential-backfill] 有失败行，见上面的逐行日志；这些行在阶段二切换后会被当成'凭据已变化'。")
		os.Exit(1)
	}

	// 收尾自检：回填完必须一行都不缺，否则阶段二不能上。
	pendingAccounts, pendingKeys, err := repository.CountCredentialBackfillPending(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "自检查询失败：%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  剩余待回填：accounts=%d api_keys=%d\n", pendingAccounts, pendingKeys)
	if !*apply && (pendingAccounts > 0 || pendingKeys > 0) {
		fmt.Println("[credential-backfill] 演练结束（未写入）。")
		return
	}
	if pendingAccounts > 0 || pendingKeys > 0 {
		// exit 1 刻意保留：迁移确实没完成，脚本调用方必须被挡住（"选可验证更严的一侧"）。
		// 但**原因要说对**：清单建议生产先跑 `-limit 50`，那是分片试跑，剩余非零是
		// 预期结果；若和"没加 -limit 却仍有剩余"（说明有行反复失败）共用一句
		// "⚠️ 仍有未回填行"，运维会把正常的试跑读成故障，然后学会忽略这句话。
		if *limit > 0 {
			fmt.Fprintf(os.Stderr,
				"[credential-backfill] ⏸ 分片运行未完成（预期）：-limit=%d 是每张表的上限，剩余 accounts=%d api_keys=%d。\n",
				*limit, pendingAccounts, pendingKeys)
			fmt.Fprintln(os.Stderr,
				"[credential-backfill]   这不是故障；但迁移仍未完成，阶段二 MUST NOT 上线。继续跑（再跑一次或去掉 -limit）直到剩余为 0。")
		} else {
			fmt.Fprintln(os.Stderr,
				"[credential-backfill] ⚠️ 未加 -limit 却仍有剩余：说明这些行反复失败（上面应有逐行日志），阶段二 MUST NOT 上线")
		}
		os.Exit(1)
	}
	fmt.Println("[credential-backfill] 全部回填完成，阶段二可以开始。")
}
