# 02. 設計妥当性と Agent 責務レビュー

## サマリー

- 検出件数: **11 件** (Finding F-01〜F-11)
- 観点別内訳: 観点1=2 件, 観点2=2 件, 観点3=2 件, 観点8=2 件, 観点10=2 件, 観点11=1 件
- 重要度別内訳: Critical=0, High=4, Medium=5, Low=2, Nit=0
- 主要懸念:
  1. **閉ループ穴**: lease 失効通知が「stderr 文字列」に依存し、Worker は `current_epoch` を取得する API を持たない (F-03)。`task_heartbeat_handler` は `recordSelfWrite` を呼ばないため、queue 書き込みが自分の fsnotify イベントと衝突する潜在競合がある (F-04, learnings 参照)
  2. **手動介入残**: `unquarantine` / `resume-merge` / `retry-publish` は Planner にも Orchestrator にも CLI 経路を開いておらず、人間オペレータの介入が前提 (F-05, F-06)
  3. **instructions 乖離**: planner.md の plan submit YAML 包括例には `expected_paths` / `definition_of_abort` が **必須付記つきで掲載済み** だが、衝突解決例や shell quoting 例など **抜粋ブロックには未記載** で誤読しやすい (F-07)
  4. **Daemon 吸収すべき**: 600 行超の PreToolUse 安全ポリシーが bash 正規表現で worker サイドに常駐 (F-09)、`@run_on_main` フラグも tmux user-variable 経由でフックに伝播するため tmux 障害耐性が低い (F-10)
  5. **Agent 過剰責務**: lease_epoch 不一致時の「黙ってターン終了」「`--no-retry-safe` を付けない」セマンティクスを Worker 自身が判定する (F-11)

設計上の総評: 「Daemon が Single Source of Truth」「Agent はビジネスロジック専念」という maestro.md の原則は概ね守られているが、**例外フロー (quarantine, fencing, publish 競合) で Agent / 人間オペレータ依存が残る**。下記 Findings 群で具体化する。

---

## Findings

### 観点 1: 自然言語要求 → plan submit / phase / blocked_by / persona / skill 機構の実現性

#### F-01 (設計穴・High)

| 項目 | 値 |
|---|---|
| カテゴリ | 設計穴 |
| 重要度 | High |
| 場所 | `internal/agent/launcher.go:41-63` (`allowedToolsByRole`)、`internal/agent/launcher.go:280-283` (planner disallow)、`templates/instructions/orchestrator.md:1-256` (Orchestrator 5 アクション) |
| 現状の問題 | Orchestrator は `Bash(maestro:*)` ホワイトリストで CLI を走らせるが、launcher の `allowedToolsByRole` で配信される CLI サブコマンドは `command submit / status / list / cancel` のみで、`plan unquarantine` / `plan resume-merge` / `plan retry-publish` を呼ぶ経路がない。Planner も `--disallowedTools "Bash(maestro plan unquarantine:*)"` で明示的にブロックされている。結果として「ユーザーから Orchestrator への自然言語指示」だけでは quarantine / publish 競合からの自動復旧が不可能で、必ず人間オペレータが別経路で CLI を叩く必要がある。 |
| 推奨アクション | (a) `plan unquarantine` を Planner にも開放し、quarantine の根本原因が解消したことを Planner が宣言できるフローを導入する、または (b) Orchestrator に `command resume-quarantine <command_id>` 相当の高位 CLI を追加して自然言語指示でも復旧経路に乗るようにする。 |
| 根拠 | `templates/instructions/orchestrator.md` (Orchestrator の 5 actions に unquarantine 系なし)、`templates/instructions/planner.md:240` (「`unquarantine` はオペレータ専用」明記)、launcher の `--disallowedTools` 配線。 |

#### F-02 (設計穴・Medium)

| 項目 | 値 |
|---|---|
| カテゴリ | 設計穴 |
| 重要度 | Medium |
| 場所 | `templates/instructions/planner.md:1018-1052` (plan submit YAML 包括例)、`internal/plan/validate.go:307-320` (`validateTaskFieldsCore`)、`internal/plan/retry.go:125-126,317` (`validateInjectedSchemaFields`) |
| 現状の問題 | `expected_paths` / `definition_of_abort` は validate.go で必須化されているが、`persona_hint` / `skill_refs` は依然として **任意** フィールドであり、Planner が省略すると Worker は persona / skill 注入なしで動く。一方 planner.md は「ペルソナ前提でタスク設計せよ」と要求しており、必須/任意の境界が文章とスキーマで一致していない。SMART criteria の検証可能性も persona に強く依存する。 |
| 推奨アクション | (a) `persona_hint` を validate.go で必須化する、または (b) persona_hint 省略時に Planner 自身が `implementer` をデフォルト設定する旨を planner.md に明記し、`tools_hint` / `skill_refs` の必須/任意境界を表で再整理する。 |
| 根拠 | `templates/instructions/planner.md` (ペルソナ前提のタスク設計を要求)、`internal/plan/validate.go:307-320` (expected_paths / definition_of_abort は必須だが persona は未検証)。 |

---

### 観点 2: 自律 LLM Orchestration の閉ループ整合性

#### F-03 (閉ループ穴・High)

| 項目 | 値 |
|---|---|
| カテゴリ | 閉ループ穴 |
| 重要度 | High |
| 場所 | `templates/instructions/worker.md` の lease_epoch ライフサイクル節 (Worker 側 epoch 失効検知)、`internal/daemon/lease/manager.go:138,176-184,152-168` (`acquireLease`/`extendLeaseExpiry`/`releaseLease`) |
| 現状の問題 | 「epoch 失効を能動照会する API は提供されていない (...必要性が出れば fencing エラー側に `current_epoch` を載せる拡張が推奨方針として残されている)」と worker.md 自身が認めている。Worker は heartbeat / result write の **stderr 文字列** をパースして黙ってターン終了する設計で、(a) stderr メッセージのフォーマットがコードのまま (`task <id> epoch mismatch: queue=<N>, request=<M>` と `task <id> lease_epoch mismatch: queue=<N>, request=<M>`) で異なり、Worker のテキストマッチが脆い、(b) 失効後に何が起きたか (再 dispatch されたか / quarantine に入ったか) を Worker が知る術がない。 |
| 推奨アクション | fencing エラー応答に `current_epoch` と `current_status` (例: `RECLAIMED_BY_OTHER` / `MAX_RUNTIME_EXCEEDED` / `QUARANTINED`) を構造化 JSON で同梱し、CLI が機械可読な exit code と stderr を吐き分けるようにする。Worker 側の判定を「文字列 grep」から「exit code 比較」に置き換える。 |
| 根拠 | `templates/instructions/worker.md` (epoch ライフサイクル節)、`internal/daemon/lease/manager.go:138-184`。 |

#### F-04 (閉ループ穴・High)

| 項目 | 値 |
|---|---|
| カテゴリ | 閉ループ穴 |
| 重要度 | High |
| 場所 | `internal/daemon/result_write_phase_a.go:444,447` / `internal/daemon/result_write_handler.go:562` / `internal/daemon/task_heartbeat_handler.go:202` (learnings 参照) |
| 現状の問題 | `recordSelfWrite` は phase_a と result_write_handler では呼ばれるが、`task_heartbeat_handler.go:202` の queue 書き込み経路では呼ばれない。fsnotify ベースの自己書込トラッカは SHA-256 で外部編集を検出する設計のため、heartbeat による queue 更新を Daemon 自身の event-bridge が「外部編集」と誤認しスキャンを誘発する競合が潜在する。閉ループ (heartbeat→延長→再スキャン) のうち heartbeat→延長の部分が自己イベント増幅で過剰トリガーされうる。 |
| 推奨アクション | `task_heartbeat_handler.go:202` の書き込み直前に `recordSelfWrite(<computed sha>)` を追加するか、heartbeat 専用の write path に self-write tracker のフックを共通化する `applyQueueWrite(...)` ヘルパーを追加してリーク経路をなくす。 |
| 根拠 | learnings (Worker1 過去報告): `result_write_phase_a.go:444,447` と `result_write_handler.go:562` は `recordSelfWrite` を呼ぶが `task_heartbeat_handler.go:202` は呼ばない。 |

---

### 観点 3: ユーザー手動介入が必要な箇所

#### F-05 (手動介入残・High)

| 項目 | 値 |
|---|---|
| カテゴリ | 手動介入残 |
| 重要度 | High |
| 場所 | `templates/instructions/planner.md:240,857` (unquarantine はオペレータ専用)、`internal/agent/launcher.go:280-283` (planner --disallowedTools 強制) |
| 現状の問題 | quarantine 解除 (`maestro plan unquarantine`)、publish 競合解決 (`maestro plan resume-merge`/`retry-publish`)、commit_failed エスカレーションは Orchestrator にも Planner にも CLI 経路がなく、人間オペレータが手動で叩く必要がある。Orchestrator が自然言語で「再開して」と指示しても自律復旧できない。 |
| 推奨アクション | quarantine の根本原因 (verify config drift / publish race / fencing reject loop) ごとに「Planner が安全に解除できる条件」を定義し、(a) 自動解除は Daemon の scan_orchestrator が担う、(b) 残る人間判断は Orchestrator 用 CLI として表面化する、の二段で手動介入を縮退させる。 |
| 根拠 | `templates/instructions/planner.md:240,857`、`internal/agent/launcher.go:280-283`。 |

#### F-06 (手動介入残・Medium)

| 項目 | 値 |
|---|---|
| カテゴリ | 手動介入残 |
| 重要度 | Medium |
| 場所 | `templates/instructions/planner.md` Phase 0 verify 節 (lines 64-76)、`internal/plan/submit.go:78-83,98-116` (`validateRequiredVerifySnapshot`) |
| 現状の問題 | Phase 0 で verify config snapshot を `maestro verify write` 相当で **先に書き込んでから** plan submit する手順が要求される。submit.go は snapshot がない場合 `Verify.EffectiveEnabled() && RequireVerifySnapshot` のときに reject するが、Phase 0 の前置タスクは Planner プロンプトでしか強制されておらず、Daemon が自動的に「verify 設定なしで submit したら verify ステップを差し込む」フォローはしない。実質、Planner の手順遵守に依存する。 |
| 推奨アクション | submit.go 側で snapshot 不在時は (a) `verify-bootstrap` 専用タスクを自動挿入する、または (b) reject エラーで「`maestro verify write` を先に投入してください」と CLI ヒントを返し、Planner プロンプトでも再掲する。 |
| 根拠 | `templates/instructions/planner.md` Phase 0 節、`internal/plan/submit.go:78-116`。 |

---

### 観点 8: instructions/docs と実装の乖離

#### F-07 (instructions 乖離・Medium)

| 項目 | 値 |
|---|---|
| カテゴリ | instructions 乖離 |
| 重要度 | Medium |
| 場所 | `templates/instructions/planner.md:1018-1052` (包括 YAML、必須記載あり)、`templates/instructions/planner.md` の衝突解決例 / shell quoting 例 (抜粋ブロック)、`internal/plan/validate.go:307-320` (validateTaskFieldsCore) |
| 現状の問題 | `expected_paths` / `definition_of_abort` の **必須化** はコード側で完結しているが、planner.md の YAML スニペット (衝突解決時の例、シェルクォート例など包括例以外の抜粋ブロック) には省略表記が混在する。Planner LLM が抜粋ブロックを参考に YAML を生成すると validate.go で reject されるため、planner.md 内の **すべての** YAML 例にこれら必須フィールドを併記するか、「省略している場合は包括例 (lines 1018-1052) を参照」と注記すべき。 |
| 推奨アクション | planner.md 内のすべての YAML 抜粋に対して: (a) `expected_paths` / `definition_of_abort` を必須として明示する、または (b) 省略している抜粋には「※ expected_paths / definition_of_abort は省略。必須項目は §包括例参照」と明示する。 |
| 根拠 | `templates/instructions/planner.md:1018-1052` (必須記載あり)、`internal/plan/validate.go:307-320`。 |

#### F-08 (instructions 乖離・Low)

| 項目 | 値 |
|---|---|
| カテゴリ | instructions 乖離 |
| 重要度 | Low |
| 場所 | `templates/instructions/worker.md` の 「`--force-with-lease` は Worker 適用外」記述、`templates/maestro.md` Tier3 「`git push --force` の代替: `--force-with-lease`」、`internal/agent/policy_checker.go:354-356` (B003 で `git push --force-with-lease` も拒否) |
| 現状の問題 | maestro.md (共通) Tier3 では `--force-with-lease` を安全代替として記載しているが、Worker には適用外と worker.md 側で打ち消しており、policy_checker の B003 も `--force-with-lease` を含む `git push` 全面拒否を実装している。共通プロンプト → Worker 専用プロンプトの "上書き" 関係が読みづらく、Planner が Worker タスクの content に「git push --force-with-lease を実行してください」と書くと拒否される。 |
| 推奨アクション | maestro.md Tier3 の代替表に「※ Worker には適用外。Worker 用は『push 自体禁止』を参照」と注記し、policy_checker の B003 拒否文言にも明示する。 |
| 根拠 | `templates/maestro.md` Tier3 表、`templates/instructions/worker.md` (Worker 固有規則)、`internal/agent/policy_checker.go:354-356`。 |

---

### 観点 10: Daemon が吸収すべきロジックを Agent プロンプトに押し付けていないか

#### F-09 (Daemon 吸収すべき・Medium)

| 項目 | 値 |
|---|---|
| カテゴリ | Daemon 吸収すべき |
| 重要度 | Medium |
| 場所 | `internal/agent/policy_checker.go:121-690` (PreToolUse hook 全体)、`internal/agent/policy_checker.go:72-244` (`WriteHookScript` / `HookSettings`) |
| 現状の問題 | 600 行超の安全ポリシー (D001-D012, B001-B004, M-AGT1, RUN_ON_MAIN, WT-GIT, WT001) が **bash 正規表現** として Worker サイドに毎回書き出される。Tier1/Tier2 判定ロジックがシェルスクリプト中に分散しており、(a) Go 側に同等の `internal/validate` を持つにもかかわらず重複保守が必要、(b) 正規表現の取りこぼしや shell escaping のバグがそのまま安全網の穴になる、(c) 将来 Claude Code が hook プロトコルを変更すると一括破綻する。 |
| 推奨アクション | (a) Tier1/Tier2 判定を Go の `internal/validate/policy.go` に集約し、PreToolUse hook はその Go バイナリを呼び出すラッパに痩せさせる、(b) 検査結果を構造化 JSON で記録し scan_orchestrator が後追いで監査できるようにする。 |
| 根拠 | `internal/agent/policy_checker.go:72-244,121-690`、`templates/maestro.md` 「Worker Bash / ツール制約の全体像」(L1/L2 二層設計)。 |

#### F-10 (Daemon 吸収すべき・Medium)

| 項目 | 値 |
|---|---|
| カテゴリ | Daemon 吸収すべき |
| 重要度 | Medium |
| 場所 | `internal/agent/policy_checker.go:159-176` (RUN_ON_MAIN フラグの tmux user-variable 経由読み取り) |
| 現状の問題 | Worker が main ブランチ直接編集を許されているか否かは tmux user-variable `@run_on_main` で hook に通知される。tmux 障害 / セッション切替 / pane 再生成のレースが起きると hook がフラグを取得できず fail-closed (=デフォルト拒否) になり、Worker が全くファイル編集できなくなる。安全側に倒れているが、Worker からは「なぜ拒否されたか」が tmux 状態に依存し再現が困難。 |
| 推奨アクション | 配信タスクの引数 (環境変数 / プロンプト先頭の structured field) として `MAESTRO_RUN_ON_MAIN` を Daemon が直接渡し、tmux user-variable には依存しない。tmux は表示用途に限定する。 |
| 根拠 | `internal/agent/policy_checker.go:159-176`。 |

---

### 観点 11: Agent への過剰責務

#### F-11 (Agent 過剰責務・High)

| 項目 | 値 |
|---|---|
| カテゴリ | Agent 過剰責務 |
| 重要度 | High |
| 場所 | `templates/instructions/worker.md` の lease_epoch ライフサイクル節 / 「Worker 側の epoch 失効検知」、`templates/maestro.md` 「Agent の責務原則」(ID 自己生成禁止 / lease_epoch 自己更新禁止) |
| 現状の問題 | maestro.md は「Agent はビジネスロジックのみ」と原則を掲げる一方、worker.md は Worker に対して **(a) heartbeat 応答の `FENCING_REJECT` を機械的にハンドル、(b) result write の stderr に出る "lease_epoch mismatch" 文字列を判定して `--no-retry-safe --partial-changes` を付けずに黙ってターン終了する、(c) `MAX_RUNTIME_EXCEEDED` を heartbeat で受領したら作業を中断する**、という三種のセマンティクスを Worker LLM の判断に委ねている。これは「ID 採番禁止」「lease_epoch 自己更新禁止」と整合する一方、復旧条件の **判定ロジック** は Agent 側に残されており、stderr フォーマット変更や LLM のハルシネーションでクラッシュリカバリが破綻しやすい。 |
| 推奨アクション | Daemon 側で「Worker は失効後に何をすべきか」を構造化 status (例: `TERMINATE_SILENTLY` / `TERMINATE_WITH_PARTIAL_REPORT` / `RETRY_WITH_NEW_EPOCH`) として返し、Worker は status を見るだけで判断できる形に統一する。worker.md の冗長判定説明は status マッピング表に置換する。F-03 と一体で対処することが望ましい。 |
| 根拠 | `templates/instructions/worker.md` (Worker 側の epoch 失効検知節)、`templates/maestro.md` (Agent 責務原則)。 |

---

## 観点別カバレッジ

| 観点 | Findings |
|---|---|
| 1. 自然言語要求の実現性 | F-01, F-02 |
| 2. 自律 Orchestration 閉ループ整合性 | F-03, F-04 |
| 3. 手動介入残 | F-05, F-06 |
| 8. instructions/docs ↔ 実装乖離 | F-07, F-08 |
| 10. Daemon 吸収すべきロジック | F-09, F-10 |
| 11. Agent 過剰責務 | F-11 |

すべての重点観点について 1 件以上の Finding が記載されている。

## 補足 (確実性レベル)

- **確認済み** (コード読解済み): F-01, F-02, F-03, F-05, F-06, F-07, F-08, F-09, F-10, F-11
- **参照済み (learnings 経由)**: F-04 (`task_heartbeat_handler.go:202` は本セッションでは未直接読込。learnings の Worker1 過去報告に基づく。`recordSelfWrite` の有無の最終確認は次回タスクで実コード再読込推奨)
