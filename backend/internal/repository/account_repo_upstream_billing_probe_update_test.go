package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

func TestLockAndMergeAccountProbeExtraUsesCurrentDatabaseSnapshot(t *testing.T) {
	tests := []struct {
		name              string
		identityUnchanged bool
		databaseEnabled   any
		databaseSnapshot  any
		inputExtra        map[string]any
		wantSnapshot      any
		wantEnabled       any
	}{
		{
			name:              "ordinary edit preserves current enable flag and snapshot created after account load",
			identityUnchanged: true,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra:        map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false},
			wantSnapshot:      map[string]any{"status": "ok"},
			wantEnabled:       true,
		},
		{
			name:              "identity change clears stale snapshot",
			identityUnchanged: false,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: true,
		},
		{
			name:              "current explicit disable clears snapshot",
			identityUnchanged: true,
			databaseEnabled:   []byte(`false`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: false,
		},
		{
			name:              "missing database snapshot is not resurrected from stale input",
			identityUnchanged: true,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  nil,
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil, credentialAPIKeyHashHex("sk-test"), credentialHashOfJSONString(`{"api_key":"sk-test"}`)).
				WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "opencode_group_unchanged", "opencode_auto", "opencode_snapshot", "grok_observed_models", "upstream_model_metadata"}).
					AddRow(tt.identityUnchanged, false, true, tt.databaseEnabled, nil, tt.databaseSnapshot, nil, nil, nil, false, nil, nil, nil, nil))

			account := &service.Account{
				ID:          27,
				Platform:    service.PlatformOpenAI,
				Type:        service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test"},
				Extra:       tt.inputExtra,
			}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
			require.NoError(t, err)
			if tt.wantSnapshot == nil {
				require.NotContains(t, got, service.UpstreamBillingProbeExtraKey)
			} else {
				require.Equal(t, tt.wantSnapshot, got[service.UpstreamBillingProbeExtraKey])
			}
			require.Equal(t, tt.wantEnabled, got[service.UpstreamBillingProbeEnabledExtraKey])
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func probeBoolPtr(value bool) *bool {
	return &value
}

// The probe switch drives the rate-sync switch and never the other way round:
// syncing depends on probing, so a row where sync is on but the probe key is
// missing must lose the sync flag instead of silently gaining periodic
// outbound calls on the next unrelated edit.
func TestLockAndMergeAccountProbeExtraNeverInfersProbeFromRateSync(t *testing.T) {
	tests := []struct {
		name                 string
		databaseEnabled      any
		databaseRateSync     any
		explicitProbeEnabled *bool
		explicitRateSync     *bool
		wantEnabled          any
		wantRateSync         any
	}{
		{
			name:             "sync on with missing probe key zeroes sync and keeps probing off",
			databaseEnabled:  nil,
			databaseRateSync: []byte(`true`),
			wantEnabled:      nil,
			wantRateSync:     false,
		},
		{
			name:             "sync on with probe off zeroes sync",
			databaseEnabled:  []byte(`false`),
			databaseRateSync: []byte(`true`),
			wantEnabled:      false,
			wantRateSync:     false,
		},
		{
			name:             "both on in database stay on",
			databaseEnabled:  []byte(`true`),
			databaseRateSync: []byte(`true`),
			wantEnabled:      true,
			wantRateSync:     true,
		},
		{
			name:                 "admin enabling both explicitly still turns probing on",
			databaseEnabled:      nil,
			databaseRateSync:     nil,
			explicitProbeEnabled: probeBoolPtr(true),
			explicitRateSync:     probeBoolPtr(true),
			wantEnabled:          true,
			wantRateSync:         true,
		},
		{
			name:                 "explicit probe disable clears sync",
			databaseEnabled:      []byte(`true`),
			databaseRateSync:     []byte(`true`),
			explicitProbeEnabled: probeBoolPtr(false),
			wantEnabled:          false,
			wantRateSync:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(31), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil, credentialAPIKeyHashHex("sk-test"), credentialHashOfJSONString(`{"api_key":"sk-test"}`)).
				WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "opencode_group_unchanged", "opencode_auto", "opencode_snapshot", "grok_observed_models", "upstream_model_metadata"}).
					AddRow(true, false, true, tt.databaseEnabled, tt.databaseRateSync, nil, nil, nil, nil, false, nil, nil, nil, nil))

			account := &service.Account{
				ID:          31,
				Platform:    service.PlatformOpenAI,
				Type:        service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test"},
			}
			got, err := lockAndMergeAccountProbeExtra(
				context.Background(), client, account, tt.explicitProbeEnabled, tt.explicitRateSync,
			)
			require.NoError(t, err)
			if tt.wantEnabled == nil {
				require.NotContains(t, got, service.UpstreamBillingProbeEnabledExtraKey)
			} else {
				require.Equal(t, tt.wantEnabled, got[service.UpstreamBillingProbeEnabledExtraKey])
			}
			if tt.wantRateSync == nil {
				require.NotContains(t, got, service.UpstreamBillingRateSyncEnabledExtraKey)
			} else {
				require.Equal(t, tt.wantRateSync, got[service.UpstreamBillingRateSyncEnabledExtraKey])
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestLockAndMergeAccountProbeExtraProtectsOllamaManagedFields(t *testing.T) {
	for _, identityUnchanged := range []bool{true, false} {
		t.Run(map[bool]string{true: "same identity keeps snapshot", false: "changed identity clears snapshot"}[identityUnchanged], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(29), service.PlatformAnthropic, service.AccountTypeAPIKey, `{"api_key":"key","base_url":"https://ollama.com"}`, nil, credentialAPIKeyHashHex("key"), credentialHashOfJSONString(`{"api_key":"key","base_url":"https://ollama.com"}`)).
				WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "opencode_group_unchanged", "opencode_auto", "opencode_snapshot", "grok_observed_models", "upstream_model_metadata"}).
					AddRow(identityUnchanged, identityUnchanged, true, nil, nil, nil, []byte(`"local-ciphertext"`), []byte(`true`), []byte(`{"status":"ok"}`), false, nil, nil, nil, nil))

			account := &service.Account{
				ID: 29, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "key", "base_url": "https://ollama.com"},
				Extra: map[string]any{
					service.OllamaCloudUsageSessionExtraKey:     "forged-ciphertext",
					service.OllamaCloudUsageAutoRefreshExtraKey: false,
					service.OllamaCloudUsageSnapshotExtraKey:    map[string]any{"status": "forged"},
				},
			}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
			require.NoError(t, err)
			if identityUnchanged {
				require.Equal(t, "local-ciphertext", got[service.OllamaCloudUsageSessionExtraKey])
				require.Equal(t, true, got[service.OllamaCloudUsageAutoRefreshExtraKey])
				require.Equal(t, map[string]any{"status": "ok"}, got[service.OllamaCloudUsageSnapshotExtraKey])
			} else {
				require.NotContains(t, got, service.OllamaCloudUsageSessionExtraKey)
				require.NotContains(t, got, service.OllamaCloudUsageAutoRefreshExtraKey)
				require.NotContains(t, got, service.OllamaCloudUsageSnapshotExtraKey)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUpdateExtraExplicitProbeDisableRemovesSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe_enabled":false}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateExtraNilProbeRemovesKeyInsteadOfWritingJSONNull(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe":null}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeExtraKey: nil})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateNilProbeRemovesKeyInsteadOfWritingJSONNull(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27}, service.AccountBulkUpdate{
		Extra: map[string]any{service.UpstreamBillingProbeExtraKey: nil},
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "- 'upstream_billing_probe'")
}

func TestBulkUpdateDisablingProbeRemovesSnapshot(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27}, service.AccountBulkUpdate{
		Extra: map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false},
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "- 'upstream_billing_probe'")
	payload, ok := exec.execArgs[0][0].([]byte)
	require.True(t, ok)
	require.Equal(t, `{"upstream_billing_probe_enabled":false}`, string(payload))
}

func TestBulkUpdateProbeEligibilityMismatchRollsBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	enabled := true
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* WHERE id = ANY\(\$2\) AND deleted_at IS NULL AND type = \$3`).
		WithArgs(sqlmock.AnyArg(), `{27,28}`, service.AccountTypeAPIKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	rows, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{
		ProbeEnabled: &enabled,
	})

	require.ErrorIs(t, err, service.ErrUpstreamBillingProbeAccountInvalid)
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateCredentialsAtomicallyClearsProbeForOpenAIAPIKeyIdentityChange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts.*credentials_hash IS DISTINCT FROM \$4.*- 'upstream_billing_probe'`).
		WithArgs(`{"api_key":"sk-new"}`, int64(27), credentialAPIKeyHashHex("sk-new"),
			credentialHashOfJSONString(`{"api_key":"sk-new"}`),
			`{"has_refresh_token":false}`,
			sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQLCrypto(client, db, nil, credentialTestEncryptor(t))

	err = repo.UpdateCredentials(context.Background(), 27, map[string]any{"api_key": "sk-new"})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateWithAccountBillingSettingsRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
		WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil, credentialAPIKeyHashHex("sk-test"), credentialHashOfJSONString(`{"api_key":"sk-test"}`)).
		WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "opencode_group_unchanged", "opencode_auto", "opencode_snapshot", "grok_observed_models", "upstream_model_metadata"}).
			AddRow(true, false, true, []byte(`true`), []byte(`true`), []byte(`{"status":"ok"}`), nil, nil, nil, false, nil, nil, nil, nil))
	mock.ExpectExec(`(?s)UPDATE .*accounts.*SET.*WHERE .*id.*`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "id" = \$1`).
		WithArgs(int64(27)).
		WillReturnRows(updatedAccountRows(27, `{"upstream_billing_probe_enabled":false,"upstream_billing_rate_sync_enabled":false}`))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	account := &service.Account{
		ID:          27,
		Name:        "test",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra: map[string]any{
			service.UpstreamBillingProbeExtraKey: map[string]any{"status": "stale"},
		},
		Concurrency: 1,
		Priority:    1,
		Status:      service.StatusActive,
		Schedulable: true,
	}

	probeDisabled := false
	err = repo.UpdateWithAccountBillingSettings(context.Background(), account, &probeDisabled, nil, nil)

	require.EqualError(t, err, "outbox failed")
	require.Equal(t, false, account.Extra[service.UpstreamBillingProbeEnabledExtraKey])
	require.Equal(t, false, account.Extra[service.UpstreamBillingRateSyncEnabledExtraKey])
	require.NotContains(t, account.Extra, service.UpstreamBillingProbeExtraKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateExtraRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe_enabled":false}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false})

	require.EqualError(t, err, "outbox failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateCredentialsRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts.*credentials_hash IS DISTINCT FROM \$4.*- 'upstream_billing_probe'`).
		WithArgs(`{"api_key":"sk-new"}`, int64(27), credentialAPIKeyHashHex("sk-new"),
			credentialHashOfJSONString(`{"api_key":"sk-new"}`),
			`{"has_refresh_token":false}`,
			sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQLCrypto(client, db, nil, credentialTestEncryptor(t))
	err = repo.UpdateCredentials(context.Background(), 27, map[string]any{"api_key": "sk-new"})

	require.EqualError(t, err, "outbox failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	name := "renamed"
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET name = \$1.*WHERE id = ANY\(\$2\)`).
		WithArgs(name, `{27,28}`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	rows, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{Name: &name})

	require.EqualError(t, err, "outbox failed")
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func updatedAccountRows(id int64, extra string) *sqlmock.Rows {
	return updatedAccountRowsRow(id, extra, []byte(`{}`), nil, nil, nil)
}

// updatedAccountRowsEncrypted 与 updatedAccountRows 同形，但凭据派生列**按生产形状填满**
// （密文/指纹/meta 都有）。
//
// 为什么必须有这一版（阶段 3.4）：删掉明文回落之后，"只有明文、没有密文"的行读出来是
// **硬错误**（不是回落）。凡是让读路径真的去解凭据的用例，夹具就必须像回填后的生产库
// 一样带密文；否则测的是一种线上不存在的状态，而且失败信息还会指向"回填没跑"。
//nolint:unused // 被 //go:build unit 的用例使用，lint 在默认 tag 下看不见
func updatedAccountRowsEncrypted(t *testing.T, id int64, extra string, cryptor service.SecretEncryptor) *sqlmock.Rows {
	t.Helper()
	creds := map[string]any{"api_key": "sk-test"}
	cols, err := CredentialCryptoColumnsFor(creds, cryptor)
	require.NoError(t, err)
	require.NotNil(t, cols.Encrypted, "带加密器时必须产出密文")
	require.NotNil(t, cols.Hash)
	return updatedAccountRowsRow(id, extra, []byte(mustJSON(t, cols.Meta)), cols.Encrypted, cols.Hash, cols.APIKeyHash)
}

func updatedAccountRowsRow(id int64, extra string, meta []byte, encrypted, hash, apiKeyHash *string) *sqlmock.Rows {
	now := time.Now()
	// 列序必须与 ent 生成的 dbaccount.Columns 一致：明文 credentials 已在阶段 5.2 删除，
	// 所以凭据段现在从 credentials_meta 开始（meta / encrypted / hash / api_key_hash），
	// 其后是 extra。增删凭据列时这里要同步，否则 sqlmock 会报
	// "Expected number of values to match number of columns"。
	return sqlmock.NewRows(dbaccount.Columns).AddRow(
		id, now, now, nil, "test", nil, service.PlatformOpenAI, service.AccountTypeAPIKey,
		// 阶段 5.2：明文列已删（dbaccount.Columns 少一列），行值同步少一个
		meta, nullable(encrypted), nullable(hash), nullable(apiKeyHash),
		[]byte(extra), nil, nil, 1, nil, 1, 1.0,
		service.StatusActive, nil, nil, nil, false, true, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, service.QuotaDimensionGlobal,
	)
}

// nullable 把 *string 变成 sqlmock 能接受的 nil / 值（直接传 *string 会被当成**指针**扫进目标列）。
func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
