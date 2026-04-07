# Repository Audit Report

調査日: 2026-04-07
統合元: worker1 (設計/責務/意図乖離), worker2 (機能実現性/バグ), worker3 (デッドコード/冗長性)
注: 全件コード読解ベース。実機再現・race detector 走行は未実施。

## Executive Summary

| 重大度 | 件数 |
|--------|------|
| Critical | 5 |
| High | 10 |
| Medium | 6 |
| Low | 2 |
| 合計 | 23 |

主要テーマ:
1. **タスク投入経路の二重化と所有権不明瞭** (Planner vs Daemon)
2. **result_write の冪等/race/順序問題** (CircuitBreaker, errgroup, lease epoch)
3. **cancel/complete 経路の競合と状態整合性破綻**
4. **worktree デフォルト有効化に伴う前提崩壊** (Planner doc 未追従)
5. **ドキュメント不整合** (persona 定義場所、破壊的操作規則の分散)

---

## Critical Issues

### C1. result_write での重複検出順序逆転 → CircuitBreaker 誤発火
- 根拠: `internal/daemon/result_write_handler.go:430-442`
- 出典: worker2
- 内容: CircuitBreaker counter 更新 (L433) が `AppliedResultIDs` 登録 (L442) より先。冪等弾く前に failure counter が増えブレーカが誤発火する。
- 修正方針: 冪等チェック (AppliedResultIDs 照合) を counter 更新の前段に移し、重複 result では counter を増分しない。

### C2. pending タスクの遅延 result で task_states / plan_status が矛盾
- 根拠: `internal/plan/complete.go:113-118`
- 出典: worker2
- 内容: plan_status=failed/cancelled 確定後に pending タスクの result が到着すると `CanComplete` 検証を skip し、task_states と plan_status の整合が崩れる。
- 修正方針: terminal 状態到達後の result 受信時に「結果のみ記録、state 遷移はガード」する明示分岐を追加。または complete 時に未着 result を待たず drop するポリシーを文書化する。

### C3. Daemon が queue write 経由で task ID を採番できる (Planner バイパス経路)
- 根拠: `internal/daemon/queue_write_handler.go:231`, `internal/plan/submit.go:380`
- 出典: worker1
- 内容: Planner も Daemon も task ID 採番が可能で、Worker 指令書「配信されたタスク」前提と矛盾する投入経路が併存。
- 修正方針: 採番権を Planner 単一に集約し、Daemon の queue write は Planner 経由に限定する。または現状を「Daemon 自動採番」として明文化し Worker doc を更新する。

### C4. `__system_commit` の責務が Planner と Daemon に二重化
- 根拠: `internal/plan/submit.go:375-395`, `internal/daemon/queue_scan_collect.go` の特殊処理
- 出典: worker1
- 内容: Planner が __system_commit タスクを生成する一方 Daemon が dispatch を握り、所有権が不明瞭。さらに `worker.md:287-310` は worktree 有効時に不要としているが `submit.go:369-395` は continuous.enabled のみで挿入し worktree 条件を見ない (意図乖離 C4-b)。
- 修正方針: worktree 有効時は Planner 側で生成自体を抑止する分岐を追加。責務を Planner = 生成、Daemon = 実行 と一本化する。

### C5. lease_epoch のライフサイクル不可視性
- 根拠: `internal/daemon/task_heartbeat_handler.go`
- 出典: worker1
- 内容: lease_epoch のインクリメント主体・タイミングが doc 化されていない。Worker は stderr エラーでしか revoke を検知できず、能動検知 API がない (worker.md:173-175 と矛盾)。
- 修正方針: `maestro lease status` 等の問い合わせ CLI を追加するか、heartbeat レスポンスに現行 epoch を返す。あわせてドキュメント化。

---

## High Issues

### H1. result_write Phase C errgroup race
- 根拠: `internal/daemon/result_write_handler.go:156-172`
- 出典: worker2
- `if d.eg != nil` チェックと goroutine 投入の間で shutdown が走ると `d.eg.Wait()` に拾われず goroutine 漏洩・潜在 deadlock。
- 修正方針: errgroup の参照取得と Go() を mutex 内で原子化、または shutdown 後の Go() を no-op にする atomic flag を導入。

### H2. result コミット後の queue 書込失敗時に in_progress が残留
- 根拠: `internal/daemon/result_write_handler.go:394-404`
- 出典: worker2
- 二度目の queue 書込失敗がログのみで、R1 reconciler 起動前は queue が in_progress のまま。
- 修正方針: queue 書込失敗を sticky error として state に記録し reconciler が拾える pending-fix キューに積む。

### H3. cancel と complete の競合で「結果は completed・state は cancelled」が残る
- 根拠: `internal/plan/complete.go:209-222`
- 出典: worker2
- terminal 化済みなら state 更新を skip するが、result/queue は更新済みで矛盾。
- 修正方針: 競合検出時に逆方向 (result 側) のロールバック or 整合補正を行うか、両ストアを単一トランザクションで更新。

### H4. lease epoch チェックが advisory 扱い: revoke 後の learnings/skill_candidates が静かに破棄
- 根拠: `internal/daemon/result_write_handler.go:452-472`
- 出典: worker2
- 修正方針: revoke 検出時はエラー応答 + result を `rejected_due_to_lease` として永続化し、ユーザに通知。

### H5. state:{commandID} と state:learnings のロック順序未定義
- 根拠: `internal/daemon/result_write_handler.go:479`
- 出典: worker2
- 別経路の逆順取得で deadlock 余地。
- 修正方針: ロック獲得順序をパッケージ doc.go に明文化し linter で検証。

### H6. plan rebuild が AppliedResultIDs と TaskStates の整合検証なし
- 根拠: 該当箇所 (`internal/plan/...rebuild` 周辺)
- 出典: worker2
- 削除済みタスク向け stale ID が残り idempotency 判定が誤動作。
- 修正方針: rebuild 時に AppliedResultIDs を TaskStates 存在チェックでフィルタ。

### H7. cancel 経路の二重化
- 根拠: `templates/instructions/orchestrator.md:89` (`queue write planner --type cancel-request`), `planner.md:85` (`plan request-cancel`), `internal/daemon/queue_write_handler.go: handleQueueWriteCancelRequest` が Planner を介さず直接 state を変更
- 出典: worker1
- 修正方針: `plan request-cancel` を正規経路に統一し、queue write 直結ルートを deprecate / 削除。両論併記: 緊急 cancel のために直接ルートを残す案もあるが、所有権分離の観点から Planner 経由に統一する判断を推奨。

### H8. worktree デフォルト有効 vs Planner doc の前提崩壊
- 根拠: `internal/formation/up_test.go` (worktree default enabled), `templates/instructions/planner.md:376-428` (逐次 + blocked_by 前提)
- 出典: worker1
- 並列同一ファイル編集の merge リスクが doc で扱われていない。
- 修正方針: planner.md に「並列ワーカ × worktree」運用節を追加、同一ファイル衝突回避のための blocked_by ルール強化。

### H9. ドキュメント不整合 — persona 定義場所
- 根拠: `templates/instructions/planner.md:190-201` が「config.yaml の personas セクションで定義」と記述するが、`templates/config.yaml` および `internal/model/config.go` に `personas` キーは存在せず、実体は `templates/persona/*.md` (`init.go:77` で配置)
- 出典: worker3
- 修正方針: planner.md §190 を「templates/persona/{name}.md で定義」に修正。

### H10. worktree merge_publish の二重失敗で IntegrationStatus=failed のまま無限ループ余地
- 根拠: `internal/worktree/merge_publish.go:142-150`
- 出典: worker2 (medium と判定だが、無限ループ性を踏まえ High に格上げ)
- 修正方針: 失敗回数閾値で worktree を自動 quarantine し、operator 介入なしには再試行しない。

---

## Medium Issues

### M1. queue_scan_phase_a での nil receiver panic 余地
- 根拠: `internal/daemon/queue_scan_phase_a.go:117` で先に `StateReader().TripCircuitBreaker()` を呼び、L124 で nil チェック
- 出典: worker2
- 修正方針: nil チェックを L117 の前に移動。

### M2. RetryTask の state 登録 → queue enqueue が非原子
- 根拠: `internal/daemon/result_write_handler.go:93-117`
- 出典: worker2
- 中間失敗時 orphan task が残り R1 reconciler 依存。
- 修正方針: 2-phase 化または compensating delete を実装。

### M3. Worker 制約 (Bash 非 maestro コマンド遮断) の文書化不足
- 根拠: `internal/agent/policy_checker.go:72-90` は PreToolUse hook のみで Bash 制約は別経路
- 出典: worker1
- 修正方針: 制約全体像を `templates/maestro.md` に集約 doc 化。

### M4. Orchestrator continuous モードの自動再生成停止条件が暗黙
- 根拠: `templates/instructions/orchestrator.md:189-206`, `planner.md:495-501`
- 出典: worker1
- 修正方針: 明示的 pre-generation gate (例: 連続失敗 N 回で停止) を doc + 実装に追加。

### M5. 「破壊的操作の安全規則」のドキュメント分散
- 根拠: `templates/maestro.md §破壊的操作`, `templates/instructions/worker.md:321` (Tier1-3), `planner.md:274` (1行版)
- 出典: worker3
- 修正方針: 詳細を maestro.md に一本化し worker/planner は参照リンクのみとする。

### M6. ペルソナ/SubAgent 説明が複数ファイルに分散
- 根拠: `worker.md §78-149`, `planner.md §190-201, §262, §447, §462`
- 出典: worker3
- 修正方針: persona の正本を `templates/persona/*.md` と明記し、instruction 側は責務サマリと参照のみに整理。

---

## Low Issues

### L1. デッドコード (production)
- `internal/lock/lock.go:107 (*MutexMap).remove` — deadcode + staticcheck U1000 一致、ライブラリ内部で非参照。
- 出典: worker3
- 修正方針: 削除。

### L2. デッドコード (test helper)
- `internal/agent/orchestrator_integration_test.go:31 projectRoot`
- `internal/daemon/integration_test.go:353 writeTaskWithDeps`
- `internal/daemon/phase_integration_test.go:27 maestroDir` フィールド (書き込みのみ参照なし)
- 出典: worker3
- 修正方針: 削除または利用箇所を補完。

---

## カテゴリ別詳細

### 設計 / 役割責務
- **タスク採番権の重複** (C3) と **__system_commit 二重所有** (C4) は同根の問題。Planner を「計画と採番の単一源」、Daemon を「実行と整合維持」に分離する設計憲章を文書化することで一括解消可能。
- **cancel 経路の二重化** (H7) も同テーマ。`plan request-cancel` を正規化、`queue write` 直結を deprecate。
- **lease epoch ライフサイクル** (C5) は責務というより「Daemon 内部状態の Worker 可観測性」問題。heartbeat レスポンスへの epoch 同梱が最小コストの解。

### 機能実現性 / バグ
- result_write_handler.go に問題が集中 (C1, H1, H2, H4, H5, M2)。Phase C のロック/順序/エラー伝播を統一 review して再設計する価値が高い。
- complete.go の状態整合 (C2, H3) は CanComplete のガード強化と「terminal 状態後の result は記録のみ」ポリシーで対処。
- queue_scan_phase_a.go (M1), worktree/merge_publish.go (H10) は局所修正で済む。

### 冗長性 / デッドコード
- production デッドコードは MutexMap.remove 1 件のみ (L1)。テスト helper 3 件 (L2)。
- 設定 yaml と Config 構造体の整合は良好 (worker3 確認済み)。
- ドキュメントの重複 (M5, M6) と不整合 (H9) が冗長性領域の主課題。

### 意図乖離
- worker.md ↔ submit.go: worktree 有効時の __system_commit (C4-b)
- planner.md ↔ daemon/queue_write_handler.go: cancel 経路 (H7)
- planner.md ↔ formation/up_test.go: 並列前提 (H8)
- worker.md ↔ task_heartbeat_handler.go: lease epoch 検知性 (C5)
- orchestrator.md ↔ cmd/maestro/cmd_queue.go: cancel 直接操作 (H7)
- planner.md ↔ templates/persona/: persona 定義場所 (H9)

これら全ては「実装が doc を追い越した」パターン。doc を実装に追従させる定期 audit が必要。

---

## 推奨アクション (優先度順)

1. **[Critical/即時]** result_write_handler.go の冪等チェック順序を修正 (C1)。テストで重複 result → CircuitBreaker 不発火を確認。
2. **[Critical/即時]** complete.go の terminal 状態後 result 受信ガードを追加 (C2)。
3. **[Critical]** タスク採番権を Planner に集約、Daemon の queue write 直結を制限 (C3)。並行して `__system_commit` 生成を worktree 条件で抑止 (C4)。
4. **[Critical]** lease epoch を heartbeat レスポンスに同梱、Worker doc を更新 (C5)。
5. **[High]** result_write_handler.go の Phase C errgroup race / queue 書込失敗時 reconciler 連携 / lease advisory / ロック順序 を一括 review (H1, H2, H4, H5)。
6. **[High]** cancel 経路を `plan request-cancel` に統一、queue write 直結 cancel を deprecate (H7)。
7. **[High]** planner.md に worktree 並列運用節を追加、同一ファイル衝突防止ルール強化 (H8)。
8. **[High]** plan rebuild の AppliedResultIDs / TaskStates 整合検証 (H6)。
9. **[High]** worktree merge_publish 二重失敗時の自動 quarantine (H10)。
10. **[High/Doc]** planner.md §190 の persona 定義場所修正 (H9)。
11. **[Medium]** queue_scan_phase_a nil チェック順序修正 (M1)、RetryTask 2-phase 化 (M2)。
12. **[Medium/Doc]** 破壊的操作規則と persona/SubAgent 説明の集約・参照化 (M3, M5, M6)。Orchestrator 自動再生成停止条件の明示 (M4)。
13. **[Low]** デッドコード削除 (L1, L2)。

### 重複・矛盾の処理ノート
- worker1 の cancel 二重化指摘と worker2 の cancel/complete 競合 (H3) は別問題なので分離記載。
- worker2 が H10 を Medium としていたが、無限ループ性を踏まえ本レポートでは High に格上げ (両論併記: ループ実害が未検証なら Medium 据え置きも妥当)。
- worker1/worker3 ともに doc 重複 / persona 不整合を指摘しており統合 (M5, M6, H9)。
- worker1 の medium 「Worker 制約の文書化不足」と worker3 の「破壊的操作規則の分散」は別件として M3, M5 に分離。
