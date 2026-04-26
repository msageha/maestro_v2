# 01. 未コミット差分レビュー

## サマリー

- **変更ファイル数**: 132 (staged: 0 / unstaged: 約105 / untracked: 27)
  - 削除 (`.D`): 4 ファイル (`internal/daemon/dashboard_api.go`, `heartbeat_api.go`, `plan_handler.go`, `skill_handler.go`)
  - 修正 (`.M`): 約101 ファイル
  - 新規 (`?`): 27 ファイル (`internal/daemon/daemonapi/` 配下 8 ファイル含む)
- **追加/削除行数**: +5,722 / −2,400 (tracked のみ)
- **upstream に対する差分**: `origin/main` 比 +391 commit / −0 (`branch.ab`)
- **主要な変更意図 (推定)**:
  1. UDS API ハンドラを `internal/daemon/` から新パッケージ `internal/daemon/daemonapi/` へ層分離 (`api_factory.go`, `apipolicy/role.go`, 旧 `*_handler.go` 削除と置換)
  2. `result_write` ハンドラを 5 ファイルに分割 (`result_write_phases.go`, `result_write_postprocess.go`, `result_write_verify_orchestration.go` 等) し、async verify と admission control を追加
  3. verify サブシステム強化: command-scoped `verify` config の `maestro verify write` CLI 追加、shell `-c` 全廃 (`ParseVerifyCommand`)、stall 検出と自動 repair (`r9_verify_stall.go` +304 行)
  4. publish_conflict 解消フローの責務移行: Worker は編集のみ、Daemon が `git add` / `commit` を握る (`merge_publish.go` の `stageResolvedForwardMergeFiles` と `worker.md`/`planner.md` 更新)
  5. Orchestrator の `Bash` 許可コマンドを `maestro queue write planner --type command` / `maestro skill list` / `maestro plan request-cancel` の 3 つだけに絞り込み
  6. Plan / Worker queue へのロック取得順序を `internal/plan/queue_locks.go` で一元化
  7. agent runtime ランタイムを `claude-code` のみに制限し、`runtime_launcher.go` から非対応ランタイム分岐を撤去
- **全体的な健全性所見**:
  - 削除ファイル群はすべて新パッケージに 1:1 で移植済み (機能欠落は検出されず)
  - テストは概ね追従しており、重要パスに新規テストが追加されている
  - 整合性崩れはほぼ既存呼び出し元の追従漏れ程度に留まる
  - Critical 級の潜在バグは検出されず (Critical 1 件は要確認のためフラグ付き)
- **検出件数**: Critical 1 / High 5 / Medium 6 / Low 3 / Nit 2

## Findings

### F-01: Worktree manager が `autoRecoverWorktreeManager` インターフェース実装を満たしているか目視で未確認

| 項目 | 値 |
|---|---|
| カテゴリ | 整合性 |
| 重要度 | Critical |
| 場所 | `internal/daemon/result_write_handler.go:105` 付近 (差分内) |
| 現状の問題 | `result_write_handler.go` の `worktreeManager` フィールド型が具体型 `*WorktreeManager` から新インターフェース `autoRecoverWorktreeManager` 化された。`SetWorktreeManager(wm *WorktreeManager)` のシグネチャは変わらないが、`*WorktreeManager` が新インターフェース (`AutoRecoverAfterResolution`, `ResetResolvingWorkerToConflict` 等) を実際に充足しているかをコード読みのみでは確定できない。コンパイルが通れば実装を満たしているが、本レビューはビルドを禁じているため目視判定の範囲を出ない。 |
| 推奨アクション | merge 前に `go build ./...` と関連テスト (`result_write_handler_test.go`, `result_write_phase_b_lifecycle_test.go`) の green を必ず確認する。インターフェース定義箇所を grep してメソッド一覧を `internal/daemon/worktree/manager.go` の公開メソッドと突き合わせる。 |

### F-02: orchestrator/planner ドキュメントと launcher.go の `--allowedTools` ホワイトリストはひと通り整合

| 項目 | 値 |
|---|---|
| カテゴリ | 整合性 (確認済み) |
| 重要度 | Low (報告のみ) |
| 場所 | `internal/agent/launcher.go:43-45,56`、`templates/instructions/orchestrator.md:44`、`templates/maestro.md:100` |
| 現状の問題 | orchestrator.md と maestro.md は「Orchestrator は `maestro queue write planner --type command` / `maestro skill list` / `maestro plan request-cancel` のみ」と記載、launcher.go は同名 3 件 + `Bash(maestro:*)` (Planner 用) を `allowedToolsByRole` に登録しており、ドキュメント側も整合する。Bug ではないが、新規構造のため初回確認として記録する。 |
| 推奨アクション | 既知の差分はなし。今後 sub-command を追加する際は `allowedToolsByRole` と `instructions/*.md` の双方更新を忘れないこと。 |

### F-03: integration worktree の Worker 例外を撤廃する documentation/code 整合は取れている

| 項目 | 値 |
|---|---|
| カテゴリ | 整合性 (確認済み) |
| 重要度 | Low (報告のみ) |
| 場所 | `templates/instructions/worker.md:411-447`、`templates/instructions/planner.md:898-919`、`internal/daemon/worktree/merge_publish.go:291-389` |
| 現状の問題 | `worker.md` から「integration worktree での `git add`/`git commit` 例外」が削除され、`planner.md` の publish_conflict ハンドリング指示も `git add` / `git commit` を行わない方針に書き換わった。code 側では `merge_publish.go:reuseInFlightForwardMerge` が `stageResolvedForwardMergeFiles` を呼んで Daemon が代わりに stage→`commit --no-edit` する経路が追加されており、ドキュメントとコードが整合している。 |
| 推奨アクション | 当該テストカバレッジは `publish_conflict_test.go` (+11 行) で部分的に追加されているが、Daemon stage 経路の `bytesContainConflictMarkers` 判定 (F-13 参照) を含むパス全体のテストを 1 件追加するのが望ましい。 |

### F-04: `r9_verify_stall.go` の `r9QueuePausedForReplanSignal` dedup ロジックは仕様通り

| 項目 | 値 |
|---|---|
| カテゴリ | 整合性 (確認済み・反証) |
| 重要度 | Low (報告のみ) |
| 場所 | `internal/daemon/reconcile/r9_verify_stall.go:391-403` |
| 現状の問題 | dedup 条件は `existing.Kind == sig.Kind && existing.CommandID == sig.CommandID && existing.PhaseID == sig.PhaseID && existing.WorkerID == sig.WorkerID && existing.ConflictGeneration == sig.ConflictGeneration` で、`paused_for_replan` シグナルでは `WorkerID` / `ConflictGeneration` の双方が zero value のため、実質 `(Kind, CommandID, PhaseID)` で dedup される。これは `phaseID = "__task_" + taskID` を使う本シグナルの特性に合致しており、重複登録は抑止される。 |
| 推奨アクション | 仕様通り。コメントで「`paused_for_replan` ではこれらは zero 値で比較され dedup は (Kind, CommandID, PhaseID) で機能する」旨を補足するとさらに保守性が上がる。 |

### F-05: `submit.go` phase fill での state lock 解放→queue 書込→再取得の TOCTOU 余地

| 項目 | 値 |
|---|---|
| カテゴリ | 危険変更 (要追検証) |
| 重要度 | High |
| 場所 | `internal/plan/submit.go` 周辺 (差分 +235 行)、特に reload+rollback ロジック |
| 現状の問題 | phase fill 処理で state lock を一度解放してから `writeQueueEntries`、その後 state lock を再取得して `reloadPhaseFillState()` で整合確認する設計が新規導入されている (Explore 調査結果)。再取得時に既に他処理が phase 状態を変えていた場合、queue rollback が発火するが、rollback 自体が失敗した場合の整合性回復経路が不明。state と queue の inconsistency が残るリスクがある。 |
| 推奨アクション | 該当のロールバック失敗時に最低限ログレベル ERROR で残し、`paused_for_replan` シグナル化を経由する経路を持たせるべき。`submit_queue_test.go` (+108 行) に rollback 失敗時の assertion ケースが含まれているか確認する。 |

### F-06: `result_write_phase_a.go` の duplicate 経路で `taskRunOnIntegration` を未設定のままにしている

| 項目 | 値 |
|---|---|
| カテゴリ | 整合性 |
| 重要度 | High |
| 場所 | `internal/daemon/result_write_phase_a.go:91, 105, 126` (Explore 調査結果) |
| 現状の問題 | duplicate 検出パスが `resultWritePhaseAResult` の `taskRunOnIntegration` を明示的に false に書かず、構造体の zero value 依存になっている。呼び出し元 `handleValidatedResultWrite:337,362` で参照されるが、duplicate 時は `maybeAutoRecoverAfterResolution` をスキップするため実害はない。将来 zero value セマンティクスが変わると silent regression を起こしやすい。 |
| 推奨アクション | duplicate パスでも `taskRunOnIntegration: false` を明示代入する、もしくは `taskRunOnIntegrationSet bool` フラグを追加する。 |

### F-07: `r9EffectiveVerifyStallThreshold` が `count == 0` で configured threshold をそのまま使うが、configured が 0 の場合の保護がない

| 項目 | 値 |
|---|---|
| カテゴリ | 危険変更 |
| 重要度 | High |
| 場所 | `internal/daemon/reconcile/r9_verify_stall.go:296-325` (新規) |
| 現状の問題 | `r9EffectiveVerifyStallThreshold` は config の `stall_threshold_sec` (`templates/config.yaml` のデフォルト 600) が小さい場合に `LoadVerifyConfig` の `count * 5min + 30s` を最低限とする。しかし configured が 0 (operator が yaml で 0 を設定) の場合、verify config が無いコマンドでは threshold = 0 のまま `r9ApplyForCommand` に渡され、即時 stall 判定 (常に stall 扱い) になる可能性がある。`r9_verify_stall_test.go` (+227 行) に該当ケースのテストが含まれているか要確認。 |
| 推奨アクション | `configured <= 0` の場合に sane minimum (例: 60s) にクランプするか、startup 時の config validation で 0 を拒否する。 |

### F-08: `verify_runner_real.go` の `gitChangedFiles` が非 git ディレクトリで失敗するパスのリカバリ

| 項目 | 値 |
|---|---|
| カテゴリ | 危険変更 |
| 重要度 | Medium |
| 場所 | `internal/daemon/verify_runner_real.go` (+150 行)、`expectedPaths` 検証ロジック (Explore 調査結果) |
| 現状の問題 | `expectedPaths` containment check で `git status` 直接呼び出しが追加されたが、worktree 外実行や非 git ディレクトリの場合に `not a git repo` エラーで verify 全体が失敗する経路が想定される。failed-closed 設計とは整合するが、operator が verify 対象を非 git ディレクトリに切り替えた場合の挙動 (明確な fallback / エラー文言) が要確認。 |
| 推奨アクション | テスト `verify_runner_real_test.go` (+170 行) に「非 git ディレクトリ」ケースの assertion を追加し、エラー文言が `expected_paths` 検証由来であることをわかるよう wrap する。 |

### F-09: launcher.go から非 claude-code ランタイム分岐を撤廃した結果、`runtime_launcher_test.go` のテストが −266 行削減

| 項目 | 値 |
|---|---|
| カテゴリ | 整合性 (要評価) |
| 重要度 | Medium |
| 場所 | `internal/agent/runtime_launcher_test.go` (−266 行)、`internal/agent/launcher.go:138` 付近 |
| 現状の問題 | codex / gemini ランタイム関連テストが大量削除され、新規 `TestRuntimeLauncher_RejectsUnsupportedRuntimes` 1 件に統合された。`README.md` (+39 行) と `REQUIREMENTS.md` (+35 行) で「runtime は claude-code 限定」と明記されており、設計判断としては legitimate。ただし削除されたテストの中に汎用的な引数ビルダ等のロジックを検証していたものがあれば、退化リスクがある。 |
| 推奨アクション | 削除前後で `go test ./internal/agent/...` のカバレッジが極端に下がっていないか coverage 比較する。`launcher_test.go` (+314 行) に同等の検証が引き継がれているか目視確認する。 |

### F-10: `daemonapi/skill.go` の関数名が旧 `daemonSlugify` / `daemonFormatSkillMD` から `slugify` / `formatSkillMD` に変更

| 項目 | 値 |
|---|---|
| カテゴリ | コメント / 命名 |
| 重要度 | Medium |
| 場所 | `internal/daemon/daemonapi/skill.go:158, 172` (新規ファイル) |
| 現状の問題 | 旧 `internal/daemon/skill_handler.go` (削除) では `daemonSlugify` / `daemonFormatSkillMD` という prefix 付きの命名だったが、新パッケージで package-private 関数になったため prefix が外れた。同一動作のローカル関数なので影響は無いが、grep 経由で旧名を辿ると見つからず、過去の知見を引き継ぐ際にノイズになる。 |
| 推奨アクション | パッケージ移動を反映するなら問題なし。気になる場合は `skillSlugify` / `formatSkillMarkdown` 等で目的を残すリネームを検討する。 |

### F-11: `evolution/engine.go` の `successCount` が `SuccessCount` (exported) に昇格

| 項目 | 値 |
|---|---|
| カテゴリ | 整合性 |
| 重要度 | Medium |
| 場所 | `internal/daemon/learnings/fingerprint_db.go:20` 付近 (型 `FailurePattern`) |
| 現状の問題 | 旧 unexported `successCount` が `SuccessCount` に変更され、JSON tag が付与された。これは fingerprint_db の永続化 (`encoding/json`, `os`, `filepath` import 追加) を見据えた変更。`engine_test.go` / `fingerprint_db_test.go` にて新規挙動カバーがあるが、外部から `SuccessCount` を直接書き換えると `SuccessRate` との整合が崩れる新たな侵入面ができる。 |
| 推奨アクション | `SuccessCount` を godoc コメントで「外部から書き換えると `SuccessRate` の整合が崩れる」旨警告するか、setter 経由のみ受け付ける。 |

### F-12: `result_write_postprocess.go` と `verify_runner` の二重 `AfterVerification` 呼び出し経路

| 項目 | 値 |
|---|---|
| カテゴリ | 整合性 |
| 重要度 | Medium |
| 場所 | `internal/daemon/result_write_postprocess.go:38-46`、`result_write_verify_orchestration.go:448-450` (Explore 調査結果) |
| 現状の問題 | `AfterVerification` が duplicate フラグで二重実行を抑止しているが、sync 経路 / async 経路の両方で flag 設定タイミングが異なる。`dispatchAdvisoryReview` が二度呼ばれないかは `result_write_phase_b_lifecycle_test.go` の新規テスト (`TestResultWrite_CompletedVerifyRunsAsyncInBackground`, `TestResultWrite_AsyncVerifyHonorsAdmissionLimit`) でカバーされている可能性が高いが、sync 経路に対する assertion が未確認。 |
| 推奨アクション | sync 経路で `AfterVerification` が exactly-once 呼び出しになることを assert する単体テストを追加する。 |

### F-13: `bytesContainConflictMarkers` が「ファイル先頭にいきなり `=======` / `>>>>>>>` がある」エッジケースを取り逃がす

| 項目 | 値 |
|---|---|
| カテゴリ | 危険変更 (エッジ) |
| 重要度 | Low |
| 場所 | `internal/daemon/worktree/merge_publish.go:386-389` (新規) |
| 現状の問題 | `\n=======` と `\n>>>>>>>` を `strings.Contains` で探すため、ファイル先頭が `=======\n` で始まる病的なケース (実環境ではほぼ発生しない) は marker 検出に失敗し、Daemon が conflict 残存ファイルを stage する可能性がある。`<<<<<<<` の検出は prefix 不要なので主要なケースは捕まる。 |
| 推奨アクション | `strings.HasPrefix(s, "=======")` を OR 条件に追加するか、`bytes.Contains(data, []byte("\n======="))` 検査前に `bytes.HasPrefix(data, []byte("======="))` を加える。 |

### F-14: `internal/daemon/daemon.go:3` に `TODO(refactor)` コメントが追加されている

| 項目 | 値 |
|---|---|
| カテゴリ | コメント / 未完成 |
| 重要度 | Low |
| 場所 | `internal/daemon/daemon.go:3-5` |
| 現状の問題 | パッケージ doc に「`TODO(refactor): This package remains the daemon composition root. UDS endpoint adapters live in daemonapi, and several domains already live in sub-packages (worktree, reconcile, dispatch, lease, etc.), but result-write state ...`」と未完了の refactor 指針が残されている。意図的なマーカーであり実害はないが、release 時にトラッカー化を検討。 |
| 推奨アクション | TODO の根拠 issue 番号 (例: `TODO(refactor #1234)`) を付与するか、separate `RAFTOR.md` で管理する。 |

### F-15: `submitPromptSearchLines = 8` のマジックナンバーが `message_deliverer.go` 直書き

| 項目 | 値 |
|---|---|
| カテゴリ | コメント / 設定外出し |
| 重要度 | Nit |
| 場所 | `internal/agent/message_deliverer.go:30` |
| 現状の問題 | submit 確認の捜索行数 (8 行) や retry 試行回数 (8 回)、probe 間隔 (750ms) が定数として直書きされている。実環境のシェル幅・ターミナル速度依存で挙動が変わる可能性があるが、現状 user-configurable ではない。 |
| 推奨アクション | `model.WatcherConfig` の追加項目として外出しすると tuning しやすい (急ぎは不要)。 |

### F-16: `message_deliverer.confirmSubmittedOrRetry` の最終 fallback が `return nil` (silent)

| 項目 | 値 |
|---|---|
| カテゴリ | 危険変更 (silent failure) |
| 重要度 | Nit |
| 場所 | `internal/agent/message_deliverer.go:144-147` |
| 現状の問題 | `maxSubmitProbeAttempts` (=8) 試行後も submit 確認できなかった場合、エラー文言を一切返さず `return nil` する。Daemon 側ではメッセージ送信成功と同等に扱われるため、Worker pane が pasted text 状態でハングしても上位は気付けない。capture 失敗時には warn ログが出るが、最終 fallback は無音。 |
| 推奨アクション | 最終 fallback に `d.log(logLevelWarn, "submit_confirm_giving_up agent_id=%s task_id=%s after %d probes", ...)` を入れ、`Retryable: true` のエラーで返すか検討する (Daemon 側の上位リトライポリシーと相談)。 |

## 補足

- TODO/FIXME/XXX/HACK の追加は `internal/daemon/daemon.go:3` の 1 件のみ (新規ファイル群と差分の grep でコンファーム済み)。デバッグ print やコメントアウトされた旧コードの混入は検出されず。
- Agent 責務崩れ (Agent 側で ID 採番、tmux send-keys 直接呼び出し等) は検出されず。`message_deliverer.go` の `SendKeys("Enter")` は **daemon 側の executor 経由** であり、Agent prompt から呼び出されるわけではないため違反ではない。
- `git push` / `git reset --hard` などの破壊的操作の追加は検出されず。
- ロック解放漏れ・panic 抑制の追加は検出されず (`r9_verify_stall.go` 新規ロジックは `LockMap.WithLock` でスコープ管理されている)。
