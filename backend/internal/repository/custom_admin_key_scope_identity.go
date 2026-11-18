package repository

import (
	"strconv"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// scoped-admin Codex 身份匹配的 **Go 侧实现**（design.md A4 选项 C）。
//
// 为什么要有它：原实现靠 `"accounts"."credentials"->'key'` 拼 SQL 谓词
// （custom_admin_key_scope.go 的 field()），而 drop 明文列之后那一族谓词全部失效。
// 三个候选方案里选 C（Go 侧解密后过滤）：不新增列、不把 PII 搬进明文列、
// 且**只改代码不改表**——revert 即完全回到现状（回滚点见 design.md A4）。
//
// ⚠️ 与 SQL 侧**必须逐字等价**，包括顺序。原实现是"按序逐条谓词、先命中者胜"，
// 所以这里的 matchers 也按同一顺序生成，调用方按同一顺序取第一个命中。
// 等价性由 TestScopedCodexIdentityPredicates_SQLAndGoAgree 逐条断言（行 × 身份 × 谓词序），
// 那份测试与本文件互为守门：任何一侧改了语义，另一边立刻红。

// scopedCodexIdentityFields 是参与匹配的四个子键，值已按 SQL 侧 field() 的语义归一化：
//   - 字符串：去首尾空白（btrim，空白集合与 unicode.IsSpace 一致）
//   - 数字：按 float64 展开成十进制文本并去掉尾随零（对应
//     trim_scale(((x)::double precision)::text::numeric)::text）
//   - 其它类型（bool/对象/数组/null/缺失）：空串（对应 CASE ... ELSE ”）
type scopedCodexIdentityFields struct {
	AccountID string
	UserID    string
	Email     string // 已小写：SQL 侧比较时套了 lower()
	Access    string
}

// scopedIdentityNormalize 复刻 SQL 侧 field() 的归一化。
func scopedIdentityNormalize(raw any) string {
	switch value := raw.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case float64:
		// 与 SQL 一致：先转文本再按 numeric 去掉尾随零。'f' 不带指数，
		// 正好对应 "1e+20"::numeric::text 的展开结果。
		return trimNumericText(strconv.FormatFloat(value, 'f', -1, 64))
	case float32:
		return trimNumericText(strconv.FormatFloat(float64(value), 'f', -1, 64))
	case int:
		return strconv.FormatInt(int64(value), 10)
	case int64:
		return strconv.FormatInt(value, 10)
	case int32:
		return strconv.FormatInt(int64(value), 10)
	default:
		// bool / map / slice 等：SQL 的 CASE 落到 ELSE ''，这里必须一致，
		// 否则"存了个对象"的行会在两边给出不同结论。
		return ""
	}
}

// trimNumericText 复刻 numeric 的 trim_scale：去掉小数部分末尾的 0，以及孤立的小数点。
func trimNumericText(s string) string {
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s
	}
	trimmed := strings.TrimRight(s, "0")
	trimmed = strings.TrimSuffix(trimmed, ".")
	if trimmed == "" || trimmed == "-" {
		return "0"
	}
	return trimmed
}

// scopedCodexIdentityFieldsFrom 从凭据里取出四个匹配字段。
func scopedCodexIdentityFieldsFrom(credentials map[string]any) scopedCodexIdentityFields {
	if credentials == nil {
		return scopedCodexIdentityFields{}
	}
	return scopedCodexIdentityFields{
		AccountID: scopedIdentityNormalize(credentials["chatgpt_account_id"]),
		UserID:    scopedIdentityNormalize(credentials["chatgpt_user_id"]),
		Email:     strings.ToLower(scopedIdentityNormalize(credentials["email"])),
		Access:    scopedIdentityNormalize(credentials["access_token"]),
	}
}

// scopedCodexIdentityMatchers 按**与原 SQL 构造函数完全相同的顺序**生成匹配器。
//
// 顺序即语义：调用方取第一个命中者，所以"只给 email 但还能靠 account id 兜底"
// 这类行为取决于谁排在前面。改动这里必须同时让等价性测试保持绿。
func scopedCodexIdentityMatchers(identity service.ScopedCodexIdentity) []func(scopedCodexIdentityFields) bool {
	// 与原实现一致：account/user 只 trim，email 既 trim 又 lower，token 只 trim
	accountID := strings.TrimSpace(identity.AccountID)
	userID := strings.TrimSpace(identity.UserID)
	email := strings.ToLower(strings.TrimSpace(identity.Email))
	token := strings.TrimSpace(identity.AccessToken)

	var matchers []func(scopedCodexIdentityFields) bool

	if !identity.AgentIdentity && !identity.AccessTokenOnly {
		switch {
		case accountID != "" && userID != "":
			matchers = append(matchers, func(f scopedCodexIdentityFields) bool {
				return f.AccountID == accountID && f.UserID == userID
			})
		case userID != "":
			matchers = append(matchers, func(f scopedCodexIdentityFields) bool {
				return f.AccountID == "" && f.UserID == userID
			})
		case accountID == "" && email != "":
			matchers = append(matchers, func(f scopedCodexIdentityFields) bool {
				return f.AccountID == "" && f.UserID == "" && f.Email == email
			})
		}
	}

	if !identity.AgentIdentity && token != "" {
		matchers = append(matchers, func(f scopedCodexIdentityFields) bool {
			return f.Access == token
		})
	}

	if accountID != "" && (identity.AgentIdentity || !identity.AccessTokenOnly) {
		if userID == "" {
			matchers = append(matchers, func(f scopedCodexIdentityFields) bool {
				return f.AccountID == accountID
			})
		} else {
			matchers = append(matchers, func(f scopedCodexIdentityFields) bool {
				return f.AccountID == accountID && (f.UserID == "" || f.UserID == userID)
			})
		}
	}

	return matchers
}

// scopedCodexIdentityMatchVerdicts 给出"这一份凭据对每一条谓词是否命中"。
// 仅供等价性测试按序逐条比对用（生产路径只关心"第一个命中的账号"）。
func scopedCodexIdentityMatchVerdicts(credentials map[string]any, identity service.ScopedCodexIdentity) []bool {
	fields := scopedCodexIdentityFieldsFrom(credentials)
	matchers := scopedCodexIdentityMatchers(identity)
	out := make([]bool, len(matchers))
	for i, m := range matchers {
		out[i] = m(fields)
	}
	return out
}

// scopedCodexIdentityPick 复刻调用方的选择规则：**按序**取第一个命中的谓词，
// 在该谓词的命中集合里取 id 最大的一行（原实现是 ORDER BY id DESC LIMIT 1）。
//
// rows 必须按 id 降序传入。返回 nil 表示没有任何谓词命中。
func scopedCodexIdentityPick(rows []*dbent.Account, credentialsOf func(*dbent.Account) map[string]any,
	identity service.ScopedCodexIdentity) *dbent.Account {
	matchers := scopedCodexIdentityMatchers(identity)
	fields := make([]scopedCodexIdentityFields, len(rows))
	for i, row := range rows {
		fields[i] = scopedCodexIdentityFieldsFrom(credentialsOf(row))
	}
	for _, match := range matchers {
		for i := range rows {
			if match(fields[i]) {
				return rows[i]
			}
		}
	}
	return nil
}
