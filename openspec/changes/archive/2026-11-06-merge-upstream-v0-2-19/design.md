## Context

fork main = 上游 v0.2.16 + 45 自有提交（三车道/车道池/AIMD/embedding 面板/OIDC 多宿主），生产镜像 sha-11ebaa6 与 fork HEAD 同步。上游 v0.2.17–v0.2.19 共 164 commits。三方演练（`git merge-tree --write-tree`）证实真文本冲突仅 3 文件，60 个重叠文件其余全部自动合并；`git merge-tree` 合成树已验证 fork 的 `openai_capabilities` 投影白名单（scheduler_cache.go L1261）与注释完好存活。认证快照版本双侧同升 v25、迁移编号 246 双占，是两处文本合并成功但行为会坏的语义坑。出片链与回滚窗口受 images 仓 GHA（Go 钉版 1.27.0，inflight 内存上界测试敏感于分配器行为）与 ArgoCD（sub2api 应用 selfHeal=off）约束。

## Goals / Non-Goals

Goals: 上游 v0.2.19 合流且 fork 不变量零回退；两处语义坑规则化处理；出片上产一次滚成。
Non-Goals: 不 rebase 重写 fork 历史；不动 per-image billing 开关（挂账评估）；不接 AIMD 观测面进 dashboards（独立项）；不消化上游 unreleased 的 bound-gateway-resources。

## Decisions

- **D1 合并提交而非 rebase**：45 个提交 rebase 将重写全部 fork SHA，快照仓提交信息、部署清单注释、运营手册的外部引用全断；merge 仅 3 处冲突且历史可审计。替代方案（rebase/重建叠加）仅得线性历史 cosmetic 收益，否决。
- **D2 快照版本 v25→v26 双注释**：版本常数是「快照形状」的判据，双侧同号异义时合并体形状=双方字段并集，必须再升一版；部署后旧 v25 快照按版本不符拒绝并重建。注释同时写明两侧来源，防下次合并再撞。
- **D3 迁移 246 共存不重命名**：fork 246 已在线上应用，版本键=完整文件名（TEXT PRIMARY KEY），重命名会换键导致重复执行 ALTER 报错；上游 246 按字典序（e<g）先跑，两文件改不同列，顺序无敏感。checksum 校验与迁移测试套作守门。
- **D4 Go 钉版对齐 1.27.3**：go.mod 合流后为 1.27.3，旧钉版 1.27.0 靠 GOTOOLCHAIN=auto 透明切换，注释前提已失效；显式对齐并实测 inflight 边界，必要时同步数值。
- **D5 运营口径项走配置面不进代码**：GPT-6.1 白名单、gpt-5.5/Gemini 2.x 映射核对、scoped key 发放、grok 调价知会均为后台操作，列入 tasks 第 4 组逐项核销。

## Risks / Trade-offs

- [调度 memo 路径回归] 上游进程内快照缓存是新机制，投影白名单虽然文本存活，行为需单测钉死 → 任务 1.7 补回归断言
- [AIMD 喂点漂移] 上游改 openai_gateway_service 错误路径，类型化 Reason 分流点可能位移 → 任务 1.8 全量单测 + metrics 端点契约断言
- [inflight 内存界漂移] Go 1.27.3 分配器行为变化 → 任务 2.1 本地实测，2.2 同步 CI
- [首滚迁移失败] 247 建 scoped_admin_api_keys 表 / 248 加列在启动自动执行 → 任务 3.4 核对执行结果后再核健康探针
- [快照误判窗口] 若 v26 漏升，按张计费字段静默读 nil → spec 场景 + 任务 3.5 部署后验证兜底

## Migration Plan

出片链一次滚：合流分支全绿 → main 合并提交 → `ciyuan-v0.2.19-1` tag + ciyuan-tag 文件 → images 仓快照 → GHA unit+build → hkccr→relay→ccr → fd-infra-deploy 改 tag+注释 → ArgoCD 手动 sync（selfHeal=off）→ 迁移/健康/快照重建三核。
**回滚**：fd-infra-deploy 镜像 tag 退回 `sha-11ebaa6`，ArgoCD 手动 sync 即回旧版（旧镜像仍在 ccr）；迁移 246_enable/248 为加列/建表幂等无害，247 新表不影响旧版运行，无需迁移回滚。

## Open Questions

无。
