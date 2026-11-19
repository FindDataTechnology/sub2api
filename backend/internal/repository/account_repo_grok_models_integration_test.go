//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGrokCatalogCASPreservesSingleKeyAndRejectsStaleExpected(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQLCrypto(tx.Client(), tx, cache, credentialTestEncryptor(t))
	account := mustCreateGrokCatalogAccount(t, tx.Client(), "grok-cas-stale")
	before := readGrokCatalogRow(t, tx, account.ID)

	nextJSON := json.RawMessage(`{"fetched_at":"2026-10-25T02:00:01Z","models":["grok-4.7"]}`)
	applied, err := repo.CompareAndSwapGrokObservedModels(ctx, account.ID, nil, nextJSON)
	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, cache.setAccounts, 1)
	require.Equal(t, "keep", cache.setAccounts[0].Extra["unrelated"])

	applied, err = repo.CompareAndSwapGrokObservedModels(ctx, account.ID, nil, json.RawMessage(`{"models":["stale"]}`))
	require.NoError(t, err)
	require.False(t, applied)

	after := readGrokCatalogRow(t, tx, account.ID)
	require.Equal(t, "keep", after.unrelated)
	require.Equal(t, []string{"grok-4.7"}, after.models)
	require.Equal(t, "2026-10-25T02:00:01Z", after.fetchedAt)
	require.Equal(t, before.token, after.token)
	require.Equal(t, before.status, after.status)
	require.Equal(t, before.rateMultiplier, after.rateMultiplier)
	require.Equal(t, 0, grokCatalogOutboxCount(t, tx, account.ID))

	inTxCache := &schedulerCacheRecorder{}
	inTxRepo := newAccountRepositoryWithSQLCrypto(integrationEntClient, integrationDB, inTxCache, credentialTestEncryptor(t))
	replacement := json.RawMessage(`{"fetched_at":"2026-10-25T03:00:01Z","models":["grok-4.6"]}`)
	applied, err = inTxRepo.CompareAndSwapGrokObservedModels(dbent.NewTxContext(ctx, tx), account.ID, after.rawModels, replacement)
	require.NoError(t, err)
	require.True(t, applied, "context transaction must see the uncommitted row")
	require.Empty(t, inTxCache.setAccounts)
	replaced := readGrokCatalogRow(t, tx, account.ID)
	require.Equal(t, []string{"grok-4.6"}, replaced.models)
	require.Equal(t, "keep", replaced.unrelated)
	require.Equal(t, before.token, replaced.token)
	require.Equal(t, before.rateMultiplier, replaced.rateMultiplier)
	require.Equal(t, 0, grokCatalogOutboxCount(t, tx, account.ID))
}

func TestGrokCatalogCASTreatsNullMissingAndKeyOrderAsSameValue(t *testing.T) {
	ctx := context.Background()

	t.Run("missing key", func(t *testing.T) {
		tx := testEntTx(t)
		repo := newAccountRepositoryWithSQLCrypto(tx.Client(), tx, nil, credentialTestEncryptor(t))
		account := mustCreateGrokCatalogAccount(t, tx.Client(), "grok-cas-missing")
		applied, err := repo.CompareAndSwapGrokObservedModels(ctx, account.ID, json.RawMessage("null"), json.RawMessage(`{"models":["grok-4.5"],"fetched_at":"2026-10-25T02:00:01Z"}`))
		require.NoError(t, err)
		require.True(t, applied)
		row := readGrokCatalogRow(t, tx, account.ID)
		require.Equal(t, []string{"grok-4.5"}, row.models)
		require.Equal(t, "keep", row.unrelated)
	})

	t.Run("json null", func(t *testing.T) {
		tx := testEntTx(t)
		repo := newAccountRepositoryWithSQLCrypto(tx.Client(), tx, nil, credentialTestEncryptor(t))
		account := mustCreateGrokCatalogAccount(t, tx.Client(), "grok-cas-json-null")
		_, err := tx.ExecContext(ctx, `UPDATE accounts SET extra = jsonb_set(extra, '{grok_observed_models}', 'null'::jsonb, true) WHERE id = $1`, account.ID)
		require.NoError(t, err)
		applied, err := repo.CompareAndSwapGrokObservedModels(ctx, account.ID, nil, json.RawMessage(`{"models":["from-json-null"]}`))
		require.NoError(t, err)
		require.True(t, applied)
		row := readGrokCatalogRow(t, tx, account.ID)
		require.Equal(t, []string{"from-json-null"}, row.models)
		require.Equal(t, "keep", row.unrelated)
	})

	t.Run("sql null", func(t *testing.T) {
		tx := testEntTx(t)
		repo := newAccountRepositoryWithSQLCrypto(tx.Client(), tx, nil, credentialTestEncryptor(t))
		account := mustCreateGrokCatalogAccount(t, tx.Client(), "grok-cas-sql-null")
		// The column is NOT NULL today. Drop it only inside this rolled-back
		// transaction so the COALESCE(extra, '{}') branch is actually executed.
		_, err := tx.ExecContext(ctx, `ALTER TABLE accounts ALTER COLUMN extra DROP NOT NULL`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra = NULL WHERE id = $1`, account.ID)
		require.NoError(t, err)
		applied, err := repo.CompareAndSwapGrokObservedModels(ctx, account.ID, json.RawMessage(" \t"), json.RawMessage(`{"models":["from-sql-null"]}`))
		require.NoError(t, err)
		require.True(t, applied)
		row := readGrokCatalogRow(t, tx, account.ID)
		require.Equal(t, []string{"from-sql-null"}, row.models)
		require.Empty(t, row.unrelated)
	})

	t.Run("key order is not a conflict", func(t *testing.T) {
		tx := testEntTx(t)
		repo := newAccountRepositoryWithSQLCrypto(tx.Client(), tx, nil, credentialTestEncryptor(t))
		account := mustCreateGrokCatalogAccount(t, tx.Client(), "grok-cas-order")
		original := json.RawMessage(`{"z_source":"upstream_v1_models","models":["grok-4.5"],"fetched_at":"2026-10-25T02:00:01Z"}`)
		applied, err := repo.CompareAndSwapGrokObservedModels(ctx, account.ID, nil, original)
		require.NoError(t, err)
		require.True(t, applied)

		reordered := json.RawMessage(`{"fetched_at":"2026-10-25T02:00:01Z","models":["grok-4.5"],"z_source":"upstream_v1_models"}`)
		applied, err = repo.CompareAndSwapGrokObservedModels(ctx, account.ID, reordered, json.RawMessage(`{"models":["grok-4.7"],"fetched_at":"2026-10-25T03:00:01Z"}`))
		require.NoError(t, err)
		require.True(t, applied)
		row := readGrokCatalogRow(t, tx, account.ID)
		require.Equal(t, []string{"grok-4.7"}, row.models)
		require.Equal(t, "keep", row.unrelated)
		require.NotContains(t, string(row.rawModels), "z_source")
	})
}

func TestGrokCatalogCASDoesNotUpdateSoftDeletedOrOtherPlatform(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQLCrypto(tx.Client(), tx, nil, credentialTestEncryptor(t))
	nextJSON := json.RawMessage(`{"models":["should-not-land"]}`)

	deleted := mustCreateGrokCatalogAccount(t, tx.Client(), "grok-cas-deleted")
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET deleted_at = NOW() WHERE id = $1`, deleted.ID)
	require.NoError(t, err)
	applied, err := repo.CompareAndSwapGrokObservedModels(ctx, deleted.ID, nil, nextJSON)
	require.NoError(t, err)
	require.False(t, applied)
	row := readGrokCatalogRow(t, tx, deleted.ID)
	require.Empty(t, row.models)
	require.Equal(t, "keep", row.unrelated)
	require.True(t, row.deleted)

	other := mustCreateAccount(t, tx.Client(), &service.Account{
		Name:        "grok-cas-openai",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Extra:       map[string]any{"unrelated": "keep"},
		Credentials: map[string]any{"access_token": "token-keep"},
	})
	applied, err = repo.CompareAndSwapGrokObservedModels(ctx, other.ID, nil, nextJSON)
	require.NoError(t, err)
	require.False(t, applied)
	row = readGrokCatalogRow(t, tx, other.ID)
	require.Empty(t, row.models)
	require.Equal(t, "keep", row.unrelated)
	require.Equal(t, "token-keep", row.token)
}

func TestGrokCatalogCASConcurrentIndependentTransactionsMergeSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account := mustCreateGrokCatalogAccount(t, testEntClient(t), "grok-cas-concurrent")
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = integrationDB.ExecContext(cleanupCtx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(cleanupCtx, "DELETE FROM accounts WHERE id = $1", account.ID)
	})

	tx1, err := integrationEntClient.Tx(ctx)
	require.NoError(t, err)
	tx2, err := integrationEntClient.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = tx1.Rollback()
		_ = tx2.Rollback()
	})
	pid1 := grokCatalogBackendPID(t, tx1)
	pid2 := grokCatalogBackendPID(t, tx2)
	require.NotEqual(t, pid1, pid2)

	scope, err := service.NewGrokCatalogScope(account.ID, service.AccountTypeOAuth, "https://cli-chat-proxy.grok.com/v1/models")
	require.NoError(t, err)
	success := service.GrokCatalogObservation{
		Models: []string{"from-success"},
		Attempt: service.GrokCatalogAttempt{
			StartedAt:  "2026-10-25T02:00:00Z",
			FinishedAt: "2026-10-25T02:00:01Z",
			Status:     "success",
			Scope:      scope,
		},
	}
	failure := service.GrokCatalogObservation{Attempt: service.GrokCatalogAttempt{
		StartedAt:  "2026-10-25T04:00:00Z",
		FinishedAt: "2026-10-25T04:00:01Z",
		Status:     "failed",
		ErrorCode:  "transport",
		Scope:      scope,
	}}

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	first := make(chan bool, 2)
	repo1 := &grokCatalogGatingRepo{AccountRepository: newAccountRepositoryWithSQLCrypto(tx1.Client(), tx1, nil, credentialTestEncryptor(t)), entered: entered, release: release, first: first}
	repo2 := &grokCatalogGatingRepo{AccountRepository: newAccountRepositoryWithSQLCrypto(tx2.Client(), tx2, nil, credentialTestEncryptor(t)), entered: entered, release: release, first: first}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	run := func(tx *dbent.Tx, repo service.AccountRepository, observation service.GrokCatalogObservation) {
		defer wg.Done()
		_, persistErr := service.PersistGrokCatalogObservation(ctx, repo, account.ID, observation)
		if persistErr != nil {
			_ = tx.Rollback()
			errCh <- persistErr
			return
		}
		errCh <- tx.Commit()
	}
	go run(tx1, repo1, success)
	go run(tx2, repo2, failure)
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("timed out waiting for both catalog CAS attempts")
		}
	}
	close(release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	var applied, rejected int
	close(first)
	for value := range first {
		if value {
			applied++
		} else {
			rejected++
		}
	}
	require.Equal(t, 1, applied)
	require.Equal(t, 1, rejected)

	row := readGrokCatalogRow(t, integrationDB, account.ID)
	require.Equal(t, []string{"from-success"}, row.models)
	require.Equal(t, "2026-10-25T02:00:01Z", row.fetchedAt)
	require.Equal(t, "failed", row.lastStatus)
	require.Equal(t, "2026-10-25T04:00:00Z", row.lastStarted)
	require.Equal(t, "keep", row.unrelated)
	require.Equal(t, "token-keep", row.token)
	require.False(t, row.deleted)
	require.Equal(t, 0, grokCatalogOutboxCount(t, integrationDB, account.ID))
}

type grokCatalogSQL interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type grokCatalogRow struct {
	unrelated      string
	rawModels      json.RawMessage
	models         []string
	fetchedAt      string
	lastStatus     string
	lastStarted    string
	token          string
	status         string
	rateMultiplier string
	deleted        bool
}

// decryptGrokCatalogToken 从已取到的 credentials_encrypted 解出 access_token。
//
// 刻意**不再二次查库**：用例跑在 testEntTx 的事务里，另起一路查询（无论用全局
// integrationEntClient 还是混用 tx.Client + integrationDB）都会撞连接或读不到
// 未提交的行（实测先后报 `ent: account not found` 与 `pq: unexpected Parse response 'C'`）。
// 主查询已经把密文列取回来了，就地解最稳。
func decryptGrokCatalogToken(t *testing.T, encrypted *string) string {
	t.Helper()
	if encrypted == nil || *encrypted == "" {
		return ""
	}
	plaintext, err := credentialTestEncryptor(t).Decrypt(*encrypted)
	require.NoError(t, err)
	var creds map[string]any
	require.NoError(t, json.Unmarshal([]byte(plaintext), &creds))
	token, _ := creds["access_token"].(string)
	return token
}

func readGrokCatalogRow(t *testing.T, q grokCatalogSQL, id int64) grokCatalogRow {
	t.Helper()
	// access_token 是密钥子键，不在 credentials_meta 白名单里（明文列已在迁移 251 drop），
	// SQL 取不到它。这里先用 SQL 取非密文字段，token 再经 ent + credentialsFromEntity 解密。
	rows, err := q.QueryContext(context.Background(), `
		SELECT COALESCE(extra->>'unrelated', ''),
		       COALESCE((extra->'grok_observed_models')::text, 'null'),
		       status,
		       rate_multiplier::text,
		       deleted_at IS NOT NULL,
		       credentials_encrypted
		FROM accounts
		WHERE id = $1`, id)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var row grokCatalogRow
	var raw string
	var encrypted *string
	require.NoError(t, rows.Scan(&row.unrelated, &raw, &row.status, &row.rateMultiplier, &row.deleted, &encrypted))
	require.NoError(t, rows.Err())
	row.token = decryptGrokCatalogToken(t, encrypted)
	row.rawModels = json.RawMessage(raw)
	if raw != "null" {
		var payload struct {
			Models    []string `json:"models"`
			FetchedAt string   `json:"fetched_at"`
			Last      *struct {
				Status    string `json:"status"`
				StartedAt string `json:"started_at"`
			} `json:"last_attempt"`
		}
		require.NoError(t, json.Unmarshal(row.rawModels, &payload))
		row.models = payload.Models
		row.fetchedAt = payload.FetchedAt
		if payload.Last != nil {
			row.lastStatus = payload.Last.Status
			row.lastStarted = payload.Last.StartedAt
		}
	}
	return row
}

func grokCatalogOutboxCount(t *testing.T, q grokCatalogSQL, id int64) int {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), `SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1`, id)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var count int
	require.NoError(t, rows.Scan(&count))
	require.NoError(t, rows.Err())
	return count
}

func grokCatalogBackendPID(t *testing.T, q grokCatalogSQL) int {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), `SELECT pg_backend_pid()`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var pid int
	require.NoError(t, rows.Scan(&pid))
	require.NoError(t, rows.Err())
	return pid
}

func mustCreateGrokCatalogAccount(t *testing.T, client *dbent.Client, name string) *service.Account {
	t.Helper()
	return mustCreateAccount(t, client, &service.Account{
		Name:        name,
		Platform:    service.PlatformGrok,
		Type:        service.AccountTypeOAuth,
		Extra:       map[string]any{"unrelated": "keep"},
		Credentials: map[string]any{"access_token": "token-keep"},
	})
}

type grokCatalogGatingRepo struct {
	service.AccountRepository
	entered chan struct{}
	release <-chan struct{}
	first   chan bool
	calls   int
}

func (g *grokCatalogGatingRepo) CompareAndSwapGrokObservedModels(ctx context.Context, id int64, expected, next json.RawMessage) (bool, error) {
	g.calls++
	if g.calls == 1 {
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	applied, err := g.AccountRepository.(service.GrokCatalogSnapshotRepository).CompareAndSwapGrokObservedModels(ctx, id, expected, next)
	if g.calls == 1 && err == nil {
		g.first <- applied
	}
	return applied, err
}
