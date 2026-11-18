package lanes

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// 车道上游账号 CRUD（tasks 2.4 验证「admin 可增删上游账号」）：经既有
// AccountService（admin 账号管理面同链）对车道平台建/删账号；凭据 api_key
// 原样入库（SanitizeStoredCredentials 只剥 OAuth 类秘密）；平台不在
// typesafe 专属校验内=任意车道平台直收。
type laneAccountRepoStub struct {
	service.AccountRepository
	accounts map[int64]*service.Account
	nextID   int64
	deleted  []int64
}

func (r *laneAccountRepoStub) Create(_ context.Context, account *service.Account) error {
	r.nextID++
	account.ID = r.nextID
	clone := *account
	r.accounts[account.ID] = &clone
	return nil
}

func (r *laneAccountRepoStub) ExistsByID(_ context.Context, id int64) (bool, error) {
	_, ok := r.accounts[id]
	return ok, nil
}

func (r *laneAccountRepoStub) Delete(_ context.Context, id int64) error {
	r.deleted = append(r.deleted, id)
	delete(r.accounts, id)
	return nil
}

func newLaneAccountServiceForTest() (*service.AccountService, *laneAccountRepoStub) {
	repo := &laneAccountRepoStub{accounts: map[int64]*service.Account{}}
	return service.NewAccountService(repo, nil), repo
}

func TestLaneUpstreamAccountCRUD(t *testing.T) {
	svc, repo := newLaneAccountServiceForTest()

	created, err := svc.Create(context.Background(), service.CreateAccountRequest{
		Name:        "词元-搜索-serpapi-1",
		Platform:    domain.PlatformSerpAPI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-lane-serpapi-test"},
		Priority:    1,
		Concurrency: 8,
	})
	require.NoError(t, err)
	require.Equal(t, domain.PlatformSerpAPI, created.Platform)
	require.Equal(t, service.AccountTypeAPIKey, created.Type)
	require.Equal(t, "sk-lane-serpapi-test", created.Credentials["api_key"])

	stored := repo.accounts[created.ID]
	require.NotNil(t, stored)
	require.Equal(t, domain.PlatformSerpAPI, stored.Platform)
	require.Equal(t, service.StatusActive, stored.Status) // 新建即 active=池可取

	// 同链删除（admin 账号管理面同方法）
	err = svc.Delete(context.Background(), created.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{created.ID}, repo.deleted)
	require.NotContains(t, repo.accounts, created.ID)
}

// 硅基平台同样直收（audio+rerank 双车道上游）。
func TestLaneUpstreamAccountCRUDSiliconFlow(t *testing.T) {
	svc, repo := newLaneAccountServiceForTest()
	created, err := svc.Create(context.Background(), service.CreateAccountRequest{
		Name:        "词元-硅基-1",
		Platform:    domain.PlatformSiliconFlow,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-lane-sf-test"},
	})
	require.NoError(t, err)
	require.Equal(t, domain.PlatformSiliconFlow, repo.accounts[created.ID].Platform)
}
