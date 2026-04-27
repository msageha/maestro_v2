# Maestro Code Review - Final Report

統合日: 2026-04-27 / cmd_1777266360_04348655d3e7a8d4 / Worker `worker1` (implementer persona)

本レポートは先行レビュー成果物を統合し、2026-04-27 時点の再確認で見つかった集計・深刻度の整合性修正を反映したものである。

---

## 修正対応サマリ

2026-04-27 の修正で、Critical/High を優先して実装対応し、Medium/Low は安全に局所修正できるものから反映した。

- Critical: `confirmSubmittedOrRetry` の silent success を非 retryable な不確定エラーとして返すよう修正。
- High: Phase C の Load 失敗時 early return、ResumeMerge の状態遷移エラー伝播と save 確実化、High コメント混在の英語化、`maestro plan recover` 追加、quarantine 中の `add-task` / `add-retry-task` 拒否を実装。
- Medium/Low: `ensureWorkingDir` 失敗時の tracked cwd リセット、Planner signal retry の cancel 確認、dependency resolver nil 防御、`clearAndConfirm` capture error budget、`--content-file`、Worker hook の `git add -A|--all|.` 拒否、コメント/TODO 整理、`resultWriteError` の root cause unwrap を追加。

大規模ファイル分割や Planner 指令書の全面分割のような構造変更は、挙動変更リスクが高いため今回の局所修正からは外し、実装ガードと CLI 集約で先にリスクを下げた。

---

## サマリ

| 重点項目 | Critical | High | Medium | Low | 計 |
|---------|---------:|-----:|-------:|----:|---:|
| (1) コードコメント | 0 | 7 | 3 | 6 | 16 |
| (2) Agent 責務 | 0 | 3 | 6 | 4 | 13 |
| (3) 設計整合性 | 0 | 0 | 1 | 6 | 7 |
| (4) Go コード品質 | 1 | 3 | 16 | 8 | 28 |
| **計** | **1** | **13** | **26** | **24** | **64** |

注記:
- (1) は H-1〜H-7 / M-1〜M-3 / L-1, L-2, L-4, L-5a, L-5b, L-5c を本計上 (L-3 / L-5d は別計上の未検証扱い)。
- (2) は「該当なし」5 件（観点 2/3 等）を計に含めない。
- (3) は Critical / High ともに 0 件。Medium 1 件 (commit_failed 通知書式の表現乖離) と Low 6 件のみ。
- (4) は冗長コード 4 / デッドコード 1 / リファクタリング候補 7 / バグの可能性 16 の合計 28。4.1 は nil panic ではなく「Load 失敗キューを空として扱う状態反映リスク」として High に補正済み。

---

## (1) 重点項目: コードコメント品質

検証時点: 2026-04-27 / cmd_1777266360_04348655d3e7a8d4 / Worker `worker1` (researcher persona)

各項目は以下の手順で再検証した:
1. `Glob` で該当 .go の実存パスを確認 (タスク本文のパス推測を検証)
2. `Read` で該当行・前後コードを実取得
3. 推測補完は行わず、特定不能・実装変化済みの箇所は「未検証」と明示

確認済みパス対応表:

| タスク本文の参照 | 実存パス |
|---|---|
| `quality_gate.go` | `internal/daemon/quality_gate.go` (確認済み) |
| `queue_scan_helpers.go` | `internal/daemon/queue_scan_helpers.go` (確認済み) |
| `queue_scan_phase_a_worktree_stall.go` | `internal/daemon/queue_scan_phase_a_worktree_stall.go` (確認済み) |
| `lock_order_enabled.go` | `internal/lock/lock_order_enabled.go` (確認済み) |
| `model/state.go` | `internal/model/state.go` (確認済み) |
| `envelope.go:256, 311, 325` | `internal/envelope/envelope.go` (確認済み。`internal/daemon/dispatch/envelope.go` は 127 行しかなく該当しないため後者は除外) |
| `fitness.go` | `internal/model/fitness.go` (確認済み) |
| `launcher.go` | `internal/agent/launcher.go` (確認済み) |
| `resolver.go` | `internal/daemon/worktree/resolver.go` (確認済み) |

### High (言語混在等)

#### H-1. internal/daemon/quality_gate.go:17-18

- **ファイル:行番号**: `internal/daemon/quality_gate.go:17-18`
- **深刻度**: High
- **現状コメント抜粋**:
  ```go
  qualityGateEventBufferSize = 100                    // eventChan のバッファサイズ
  defaultEvaluationTimeout   = 100 * time.Millisecond // ゲート評価のタイムアウト
  ```
- **問題**: 同一ファイル内で godoc は英語 (`// QualityGateEvent represents an event ...` :21 ほか多数) で書かれている一方、定数の line-end コメントだけ日本語混在。読者が「英語 godoc と日本語インラインコメントのどちらが正なのか」を毎回切り替えながら読む必要があり、可読性とメンテナンス時の翻訳判断が分散する。
- **推奨対応**: 周辺 godoc が英語のため、line-end コメントも英語へ統一。例: `// buffer size for eventChan` / `// timeout for gate evaluation`。

#### H-2. internal/daemon/queue_scan_helpers.go:231-233

- **ファイル:行番号**: `internal/daemon/queue_scan_helpers.go:231-233`
- **深刻度**: High
- **現状コメント抜粋**:
  ```go
  // Phase 0 件フォールバック: phases が一切定義されていない command でも
  // worker worktree への書き込みは発生しうる。この場合、全タスク終了かつ
  // 失敗なしの条件で暗黙の単一フェーズとして merge を集約する。
  ```
- **問題**: 同関数の直後 :238-246 には英語の運用詳細ブロック (`// Build workerIDs once outside the phase loop. ... Skip workers in Conflict/Resolving state...`) が続いており、同一関数の内部コメントが日英で混在する。前段に日本語、後段に英語、という形は読み手の文脈切り替えコストが大きい。
- **推奨対応**: 同関数のコメントをいずれかの言語に統一。本ファイル全体の傾向は英語コメントのため、:231-233 を英語化するのが整合的。例: `// Fallback for commands that declare no phases: worker worktree writes can still occur, so collect a single implicit-phase merge once all tasks terminate without failures.`

#### H-3. internal/daemon/queue_scan_helpers.go:535-539

- **ファイル:行番号**: `internal/daemon/queue_scan_helpers.go:535-539`
- **深刻度**: High
- **現状コメント抜粋**:
  ```go
  // collectImplicitWorktreeMerge は phases が一切定義されていない command 向けに
  // 暗黙の単一フェーズ ("__implicit_phase") として worktree merge を集約する。
  // 全タスク terminal かつ failed なしかつ Integration.Status==created で
  // worker が登録されている場合のみ 1 件返す。それ以外は nil。
  // collectWorktreePhaseMerges の phases==0 経路から呼ばれる。
  ```
- **問題**: 同ファイルの他のメソッド godoc は英語 (例 `eligibleWorkerIDsForAutoCommit` 系、:218 周辺の Quarantined 説明など) で書かれているが、本メソッドの godoc のみ日本語。godoc 読者・IDE のホバー表示で言語が一貫しない。
- **推奨対応**: 英語に統一。例: `// collectImplicitWorktreeMerge aggregates a single implicit-phase merge for commands that declare no phases. It returns one entry only when all tasks are terminal, no failures occurred, Integration.Status==created, and at least one worker is registered; nil otherwise. Called from the phases==0 branch of collectWorktreePhaseMerges.`

#### H-4. internal/daemon/queue_scan_phase_a_worktree_stall.go:53-57

- **ファイル:行番号**: `internal/daemon/queue_scan_phase_a_worktree_stall.go:53-57`
- **深刻度**: High
- **現状コメント抜粋**:
  ```go
  // Phase 0 件 + Integration.Status==created の stall fast-path:
  // phases が一切定義されていない command が created のまま放置されている
  // ケースも通常パスと同じ timeoutMin ベースのタイムアウトチェックを適用する。
  // タイムアウト前に stall シグナルを発火しないことで、Planner がフェーズなしで
  // フラットなタスクを submit した場合の誤検知を防ぐ。
  ```
- **問題**: 同関数では条件式コメントは記号と short-form (`if cmdState.Integration.StallSignaled { continue }` の前後) のみ、別箇所の godoc は英語が支配的なファイル群と整合しない。「日本語の説明的コメント + 英語の godoc」の混在で、stall フェーズの設計理由を読む順序が固定されない。
- **推奨対応**: 英語化。例: `// Stall fast-path for "no phases declared + Integration.Status==created": apply the same timeoutMin-based check as the normal path so that flat-task plans (Planner submits with no phases) are not falsely flagged as stalled before the timeout elapses.`

#### H-5. internal/lock/lock_order_enabled.go:160-182

- **ファイル:行番号**: `internal/lock/lock_order_enabled.go:160-182`
- **深刻度**: High
- **現状コメント抜粋**:
  ```go
  // currentGID extracts the goroutine ID from a runtime.Stack snapshot.
  //
  // ⚠ Go バージョン依存リスク:
  //
  // runtime.Stack() の出力形式 "goroutine <id> [<status>]:\n..." は Go の公式仕様
  // ...
  // 代替手段の候補:
  //   - runtime.GoID(): Go チームで提案中だが未採用 (proposal #69321 等)
  //   ...
  // 現時点での許容判断:
  //   - Go 1.x 系では形式が安定しており実績がある
  //   ...
  ```
- **問題**: 1 行目の godoc は英語 (`// currentGID extracts the goroutine ID ...`) だが、それに続く同一コメントブロック内の説明・代替手段・許容判断はすべて日本語。1 つの godoc の中で言語が混在しており、Go 標準の `go doc` 出力でも言語が混じった状態になる。また `⚠` 絵文字記号が godoc に混入しており、ASCII 周辺のターミナル/ドキュメントツールでの整形が崩れる場合がある。
- **推奨対応**: godoc 全体を英語化、または逆に 1 行目の英語要約を日本語化して言語を統一。`⚠` は `WARNING:` 等のテキストへ置換するか、godoc 行頭ラベルを `Note:`/`Caveat:` に統一。

#### H-6. internal/model/state.go:36-70

- **ファイル:行番号**: `internal/model/state.go:36-70`
- **深刻度**: High
- **現状コメント抜粋**:
  ```go
  // PhaseIndex returns the slice index for the given phaseID by linear search.
  // Returns (index, true) if found, (-1, false) otherwise.
  // Linear search is used instead of caching because the number of phases per
  // command is typically small, and a cache would risk returning stale data
  // when the Phases slice is modified.
  func (pt *PhaseTracking) PhaseIndex(phaseID string) (int, bool) { ... }

  // CommandState は単一コマンドの実行状態を表す。
  // プランバージョン、フェーズ構成、タスク依存関係、完了ポリシーなど
  // コマンドのライフサイクル全体を管理する。
  // サブ構造体は yaml:",inline" で埋め込まれ、YAML シリアライゼーションの
  // フラット構造を維持する。
  type CommandState struct { ... }
  ```
- **問題**: 同ファイル内で:
  - `PhaseIndex` (36-40): 英語
  - `CommandState` (50-54): 日本語
  - `CircuitBreakerState` (72): 英語 (`// CircuitBreakerState tracks per-command circuit breaker counters.`)
  - `CompletionPolicy` (84): 日本語 (`// CompletionPolicy はコマンドの完了判定ポリシーを定義する。`)

  公開型/関数の godoc が型ごとに日英入り乱れている。`go doc internal/model` のドキュメント出力で言語が混在し、外部からの読み取り体験が損なわれる。
- **推奨対応**: 同パッケージ内の godoc 言語ポリシーを定め (周囲の英語比率がやや高いため英語推奨)、`CommandState` / `CompletionPolicy` を英語化、もしくは逆方向で全件日本語化のいずれかへ統一。優先順位: パッケージ単位での統一。

#### H-7. internal/envelope/envelope.go:256, 311, 325

- **ファイル:行番号**: `internal/envelope/envelope.go:256`, `:311`, `:325`
- **深刻度**: High
- **現状コメント抜粋**:
  ```go
  // Format matches spec §5.8.1 Worker 向けタスク配信エンベロープ.    // line 256
  // Format matches spec §5.8.1 Planner 向けコマンド配信エンベロープ. // line 311
  // Format matches spec §5.8.1 Orchestrator 向け通知配信エンベロープ. // line 325
  ```
- **問題**: 1 行内で英語 godoc 文 (`Format matches spec §5.8.1 ...`) と日本語スペックタイトル (`... 向けタスク配信エンベロープ`) が混在している。スペック側のタイトルが日本語であることを前提にしたまま英語 godoc に貼り付けたため、godoc 出力で文体が崩れる。
- **推奨対応**: スペック番号のみ参照する形に変更し、タイトルの和訳/併記をやめる。例: `// Format matches spec §5.8.1 (Worker task envelope).` のように英語要約を括弧書きで添える。あるいは godoc 全体を「日本語コメントが正」とするポリシーへ寄せ、`// 仕様 §5.8.1 ... に準拠する。` と統一。

### Medium (過剰ドキュメンテーション)

#### M-1. internal/daemon/quality_gate.go の EventType / Timestamp 6 メソッド

- **ファイル:行番号**: `internal/daemon/quality_gate.go:36, 39, 51, 54, 66, 69`
- **深刻度**: Medium
- **現状コメント抜粋**:
  ```go
  // EventType returns the event type identifier for task start events.
  func (e TaskStartEvent) EventType() string { return "task_start" }

  // Timestamp returns the time when the task started.
  func (e TaskStartEvent) Timestamp() time.Time { return e.StartedAt }
  ```
  同様の "returns the event type identifier for ... events" / "returns the time when the ... (started|completed|transitioned)" が 6 メソッド分繰り返される。
- **問題**: メソッド名 + 1 行実装で意図が完全に伝わるアクセサに対し、毎メソッド godoc を付けて「何を返すか」を再記述しているだけ。情報量ゼロで、メソッドが増えるたびにメンテ対象が増える。インターフェース `QualityGateEvent` の godoc でセマンティクスが既に説明されている (:21) ため重複も発生している。
- **推奨対応**: インターフェース側に意味論を集約し、各実装側のアクセサ godoc は削除するか 1 行 (`// EventType implements QualityGateEvent.` 等) に圧縮。Go の慣習でも「インターフェース実装メソッドはインターフェース側の godoc に集約」が一般的。

#### M-2. internal/model/fitness.go の IsFailed (`fitness.go:40-41`)

- **ファイル:行番号**: `internal/model/fitness.go:40-41`
- **深刻度**: Medium
- **現状コメント抜粋**:
  ```go
  // IsFailed は Passed が false かどうかを返す。
  func (a FitnessScore) IsFailed() bool {
      return !a.Passed
  }
  ```
- **問題**: メソッド名 `IsFailed` と実装 `return !a.Passed` だけで意味が完結している。godoc は単なる実装の和訳にとどまり、「いつ呼ぶか」「Passed との関係」「Compare との使い分け」など本来 godoc で語るべき点が含まれていない。
- **推奨対応**: 削除するか、有用な情報 (例: 「`SelectWinner` で軸 1 として参照される; `Passed=false` のスコアは tie 判定でも常に劣後する」) を追記して情報量を確保。

#### M-3. internal/model パッケージ全般 (`state.go`, `fitness.go` を含む)

- **ファイル:行番号**: `internal/model/state.go:30-34, 36-40, 50-54, 72, 84` および `internal/model/fitness.go:9-19, 21-28, 30-38`
- **深刻度**: Medium
- **現状コメント抜粋 (代表例)**:
  ```go
  // PhaseTracking groups phase lifecycle fields within CommandState.
  // Embedded with yaml:",inline" to maintain flat YAML serialization.
  type PhaseTracking struct {
      Phases []Phase `yaml:"phases"`
  }
  ```
  ```go
  // FitnessThresholds は Fitness 比較時のマージン閾値を定義する。
  // 各軸の差がマージン以内であれば同等と見なす。
  type FitnessThresholds struct { ... }

  // DefaultFitnessThresholds はデフォルトの閾値を返す。
  func DefaultFitnessThresholds() FitnessThresholds { ... }
  ```
- **問題**: `model` パッケージは状態定義の中心であり、構造体・コンストラクタ的関数すべてに godoc が付与されている。多くは「型名・フィールド名・メソッド名から自明な内容」を 1〜2 行で再述するだけで、設計意図 (「なぜこの分割か」「どの不変条件を保つか」) は別ファイル/別ドキュメントに散在している。godoc が増えても情報密度が上がらない。
- **推奨対応**: パッケージ単位で godoc 方針を整理。重要な設計意図 (例: PhaseTracking が yaml:",inline" を使う理由、CommandState のサブ構造体分割の経緯) は型側 godoc に残し、自明アクセサ・自明 default 関数の godoc は 1 行化または削除。`go vet`/`revive` の `exported` チェックを満たす最小行数まで切り詰めるのが現実解。

### Low (軽微・TODO 残存・UI 文字列等)

#### L-1. internal/envelope/envelope.go の UI 文字列 "なし"

- **ファイル:行番号**: `internal/envelope/envelope.go:269, 278, 287, 292`
- **深刻度**: Low
- **現状コメント抜粋**:
  ```go
  constraintsStr := "なし"     // line 269
  toolsHintStr   := "なし"     // line 278
  personaHintStr := "なし"     // line 287
  skillRefsStr   := "なし"     // line 292
  ```
- **問題**: コメントではなく Worker/Planner/Orchestrator へ送信されるエンベロープ本体の文字列リテラルが日本語。godoc・コメントの言語混在ではなく "出力フォーマットの言語選択" の問題だが、上位 godoc (256, 311, 325) が英語で書かれているため一貫性が崩れている。多言語化や Agent 側 prompt のロケール変更を将来検討する場合の摩擦点。
- **推奨対応**: 短期的には「Agent 配信メッセージの言語ポリシー」をどこかに明記する (例: `templates/instructions/maestro.md`)。中期的には `i18n` のような薄いラッパー、または定数 `EnvelopeNoneLabel = "なし"` を切り出して 1 箇所で扱う。

#### L-2. internal/agent/launcher.go の MAINTENANCE INVARIANT

- **ファイル:行番号**: `internal/agent/launcher.go:35-50`
- **深刻度**: Low
- **現状コメント抜粋**:
  ```go
  // allowedToolsByRole defines the tools each role is permitted to use.
  // ...
  // MAINTENANCE INVARIANT (F-002):
  //   - Whenever a `Bash(maestro …)` subcommand is added or removed here, the
  //     corresponding role's `templates/instructions/*.md` ... MUST be updated ...
  //   - The CLI surface and the prompt-side capability list are kept ...
  //   - The same applies to the role-specific deny lists in
  //     `appendDisallowedTools` / `workerDisallowedTools` below.
  ```
- **問題**: 軽微な冗長性。`MUST be updated in the same commit` 等の文言と「CLI surface と prompt が同期」「deny list にも同様」「role-specific deny lists in ...」がほぼ同じ事を 3 箇条にして記述している。情報としては重要だが、文言冗長で読み手が要旨をつかむのに時間がかかる。
- **推奨対応**: 3 箇条を 1〜2 文に圧縮。例: `// F-002 invariant: changes to allowedToolsByRole or its sibling deny lists (appendDisallowedTools / workerDisallowedTools) must ship in the same commit as updates to templates/instructions/{orchestrator,planner,worker,maestro}.md.`

#### L-3. internal/daemon/worktree/resolver.go のテンプレ参照

- **ファイル:行番号**: 未検証
- **深刻度**: Low
- **現状コメント抜粋**: 該当なし
- **問題**: 前回 Worker は `resolver.go` に「テンプレ参照」が残っていると指摘していたが、現時点で `internal/daemon/worktree/resolver.go:1-80` を Read した範囲では「コピー&ペーストの残骸」「テンプレート的な雛形参照」に該当する箇所は確認できなかった。`SignalStore` インターフェース :55-77 や `errSignalStoreUnavailable` :49-53 の godoc は具体的な契約を述べており、テンプレ参照には当たらない。指摘箇所の特定不能のため**未検証**として記録する。
- **推奨対応**: 前回コンテキストの「テンプレ参照」が指していた具体行を再特定するタスクを別途立てるか、本項目は「該当なし (検証時点で解消、または別ファイルが想定されていた可能性)」として close。

#### L-4. internal/model/fitness.go:147 の曖昧コメント

- **ファイル:行番号**: `internal/model/fitness.go:146-148`
- **深刻度**: Low
- **現状コメント抜粋**:
  ```go
  if cmp > 0 {
      // best より強い候補がある → トーナメント結果と矛盾
      // 推移律が成立するため本来起こらないが安全策
      return 0, true
  }
  ```
- **問題**: 「本来起こらないが安全策」という表現は、Compare の推移律が成立する前提条件 (3 軸ともマージン内に収まるケースで Compare がどう振る舞うか) を読者が自分で復元する必要がある。`return 0, true` (= isTie 扱い) を選ぶ理由が曖昧で、`panic` でないこと・1 件目を勝者にすることの根拠が薄い。
- **推奨対応**: 推移律が崩れる条件 (e.g. ExecutionTime のマージン跨ぎで擬似不等号のため transitivity がきわどい) を 1 行で明記し、「panic ではなく isTie で返す理由」を補足。例: `// Compare's per-axis margin comparison is non-strict, so transitivity can fail at boundary inputs. Treat such inconsistencies as tie rather than panicking, and let the judge resolve.`

#### L-5. TODO/FIXME 残存箇所

タスク本文では「2 件」とあったが、`Grep "TODO|FIXME"` を全 `.go` に対して実行した結果、技術的に意味のある TODO は 3 件、TODO 文字列を含むがコード上は説明文の項目が 1 件、合計 4 件ヒットした。

- **L-5a `internal/daemon/queue_handler_test.go:24`**
  - **深刻度**: Low
  - **現状コメント抜粋**: `// TODO(DRY): setupTestMaestroDir, newTestExecutorProvider, newTestQueueHandler are duplicated.`
  - **問題**: 重複ヘルパーの統合 TODO がテストヘルパーに残存。レビュー観点ではアクション可能だが期限・担当が不明。
  - **推奨対応**: チケット化 (issue) するか、`// TODO(DRY, owner=?, deadline=?)` の形式に書き換える。優先度低。

- **L-5b `internal/daemon/worktree_test_helper_test.go:14`**
  - **深刻度**: Low
  - **現状コメント抜粋**: `// TODO(DRY): initTestGitRepo, newTestWorktreeManager are duplicated.`
  - **問題**: L-5a と同種。worktree テスト側のヘルパー重複。
  - **推奨対応**: L-5a と同上。

- **L-5c `internal/daemon/learnings_test.go:666`**
  - **深刻度**: Low
  - **現状コメント抜粋**: `// pending TODO. F-066.`
  - **問題**: 機能 ID `F-066` 紐付きの pending TODO。コード上の状態は「テストケース内の保留マーク」程度で、TODO の解決条件 (F-066 のクローズか、別の何か) がコメントから読めない。
  - **推奨対応**: F-066 の現状ステータスを別途確認し、解決済みなら TODO 削除、未解決なら何を満たせば消せるかを 1 行追記。

- **L-5d (参考) `internal/model/runtime_test.go:28`**
  - **深刻度**: Low (実害なし、誤検出抑制目的の説明文)
  - **現状コメント抜粋**: `... // not "gemini-" prefix; "ZZZ" suffix avoids being mistaken for an XXX-style TODO marker (F-066)`
  - **問題**: TODO/FIXME 文字列を誤検知させない目的の説明であり、実 TODO ではない。レビュー対象としては除外で良いが、Grep 結果に上がってくる点だけ知っておく価値がある。
  - **推奨対応**: 対応不要。

タスク本文の「2 件」と現在の検出件数 (実 TODO 3 件) は乖離しており、`learnings_test.go:666` 分が前回検出に含まれていなかった可能性がある。**未検証 (件数差の起点不明)** として記録する。

### (1) 統計サマリ

| 区分 | 件数 |
|---|---|
| Critical | 0 件 |
| High | 7 件 (H-1 〜 H-7) |
| Medium | 3 件 (M-1 〜 M-3) |
| Low | 6 件 (L-1, L-2, L-4, L-5a, L-5b, L-5c) ※L-3 / L-5d は別計上 |
| 検証時点で解消済み | 0 件 (Read で確認した全 High/Medium 項目は現存) |
| 未検証 (パス不一致・特定不能) | 2 件 (L-3 resolver.go テンプレ参照 / L-5 TODO 件数の乖離) |

備考:
- L-3 は前回 Worker のレビュー観測点が現コード上で再現できないため未検証扱い。
- L-5 は TODO/FIXME の **総数** が前回 (2 件) と現在 (3 件 + 説明文 1 件) で食い違うため未検証扱い (個々の TODO は L-5a〜L-5c で確認済み)。
- 全項目について Read による現物確認を行い、推測補完は行っていない。
- `.maestro/` 配下への書き込みは行わず、出力先は `tmp/reports/final_review_comments.md` のみ。

---

## (2) 重点項目: Agent への過剰な責務

調査範囲: `templates/instructions/{orchestrator,planner,worker}.md`、`templates/maestro.md`、`templates/persona/*.md`、`internal/agent/{launcher.go,policy_checker.go}`、`internal/daemon/lease/manager.go`、`cmd/maestro/cmd_*.go`。

注: タスク本文では `common.md` を調査対象に挙げているが、`templates/instructions/common.md` は存在せず、Agent 共通プロンプトの SSOT は `templates/maestro.md`（114 行）である（`ls templates/instructions/` で確認済 → orchestrator.md / planner.md / worker.md のみ）。本レビューでは `templates/maestro.md` を共通指令書として扱う。

ID 採番、lease_epoch 管理、`.maestro/` 直接書込みの 3 点については「Agent → Daemon 委譲」が現状でも徹底されており Critical 級は無い。論点は (a) Planner の指令書肥大と分岐爆発、(b) role 間でのツール制約非対称、(c) Daemon 側ハードリミット欠如をプランナーの文章規約で代替している箇所、の 3 系統に集約される。

### 観点 1: 状態管理

#### 1-1. Planner が抱える進行管理ロジックがほぼ全て指令書ナラティブで強制されている [High]

- **該当箇所**: `templates/instructions/planner.md` 全体（1193 行）。特に `## 検証フェーズ` `## Verification Loop Cap` `## Conflict Recovery` `## Publish Conflict Recovery`。
- **現状**: 「verification は最大 2 ラウンド」「同種失敗 3 連続でフェーズ中断」「conflict 発生時は resolve-conflict → resume-merge を順に呼ぶ」等の不変条件が、Daemon 側のハードガードでは無く Planner の文章規約として記述されている。
- **問題**: Planner LLM が指令書を誤読・忘却した場合、誰も止められない。Tail of Bell-curve（リトライ無限ループ、未解決 conflict の上に新タスク投入、quarantined 状態を見落とした unquarantine 提案）は理論上発生しうる。CLI 受付時点では止まらない。
- **委譲案**: Daemon の `internal/daemon/scheduler` または `internal/daemon/queue` 層に以下のハードガードを追加する。
  - `verification` フェーズの round counter を Phase メタに持たせ、`max_verification_rounds` 超過時は `add-retry-task` をエラー応答（新規 CLI エラーコード）。
  - `quarantined` 状態の Plan には `add-task` / `add-retry-task` / `resume-merge` 系を一律拒否し、`unquarantine` のみ受付（既に `unquarantine` は operator gate で守られているため、その手前を補強）。
  - 同一 task_id の連続失敗回数を Daemon 側カウンタで保持し、CLI 経由の再投入時に閾値ガード。

#### 1-2. Worker の `partial_changes` フラグ運用が文章規約のみ [Medium]

- **該当箇所**: `templates/instructions/worker.md` の「`--partial-changes` を指定する場合」の節、`cmd/maestro/cmd_result.go`。
- **現状**: 部分変更が残っているかは Worker 自身の自己申告であり、CLI 側で git status 等を検証する仕組みは無い。
- **問題**: Worker が忘却・誤判定した場合、Daemon の retry / merge 判断が誤った前提で進む。worktree モードでは Daemon が直接 commit するため軽減されるが、非 worktree モード時の影響が残る。
- **委譲案**: 非 worktree モード時、`maestro result write` 受信時に Daemon が `git status --porcelain` を実行し `partial_changes` を上書き決定する（Worker の申告は hint 扱いに格下げ）。

#### 1-3. ペルソナ / スキル注入の整合確認が Planner ナラティブ [Low]

- **該当箇所**: `templates/instructions/planner.md` の `persona_hint` / `skill_refs` 説明節。
- **現状**: 「persona/skill が存在しない名前を指定すると無視される」という挙動が文章で説明されているが、Planner からの誤指定を CLI 側で警告する仕組みは無い。
- **問題**: タイポで永続的に persona/skill が空注入され続けても気付けない。
- **委譲案**: `maestro plan add-task` 受付時、未知の `persona_hint` / `skill_refs` は CLI から警告 (stderr) または `unknown_persona_hint` エラーで弾く。

### 観点 2: ID 生成

#### 2-1. ID 自己生成は完全排除済み [該当なし]

- **該当箇所**: `templates/maestro.md` (Agent 共通禁止), `cmd/maestro/cmd_plan.go` `cmd_plan_tasks.go` `cmd_queue.go` `cmd_result.go`。
- **現状**: 全 ID（plan_id / command_id / task_id）は Daemon 側で採番され CLI stdout で返却される。Agent 指令書側には ID を生成・推測する記述は見当たらない。`maestro queue write --type task` は Planner ルート以外を拒否（`cmd_queue.go:147`）。
- **問題**: なし。
- **委譲案**: 不要。

### 観点 3: lease / fencing

#### 3-1. lease_epoch 自己更新は完全排除済み [該当なし]

- **該当箇所**: `internal/daemon/lease/manager.go:138`（唯一のインクリメント点 `*ref.leaseEpoch++` in `acquireLease`）。`releaseLease` `extendLeaseExpiry` は epoch 不変。`templates/instructions/worker.md` の lease_epoch ライフサイクル節。
- **現状**: Worker は配信値を pass-through するのみで、CLI 側 `--lease-epoch` のデフォルト `-1` センチネルにより明示指定を強制している（`cmd_result.go:39 cmd_task.go`）。Daemon は `task_heartbeat` / `result_write` で厳密一致比較 → `FENCING_REJECT`。worker.md にも「Agent 側で生成・更新しない」と明記。
- **問題**: なし。設計と実装と指令書が一致している。
- **委譲案**: 不要。ただし観点 7-2 の改善案（fencing reject 応答に current_epoch を載せる拡張）は指令書末尾に「将来候補」として既に明記されており、必要時に検討で可。

### 観点 4: 複雑なロジック

#### 4-1. Planner 指令書の分岐爆発（恢復フローのバリエーションが指令書側で展開されている） [High]

- **該当箇所**: `templates/instructions/planner.md` の `## Conflict Recovery` `## Publish Conflict Recovery` `## merge_conflict / publish_conflict / publish_quarantined / commit_failed handling`。
- **現状**: Plan の状態遷移（`merge_conflict`, `conflict_resolution`, `publish_conflict`, `publish_quarantined`, `commit_failed`, `quarantined` 等）ごとに Planner が呼ぶべき CLI シーケンス（`resolve-conflict` → `resume-merge`、`retry-publish`、`unquarantine`（operator only）等）を文章で網羅している。同一の状態でも条件分岐（worktree 有無、変更ファイル件数、commit_policy 違反種別）で異なる手順が要求される。
- **問題**: 状態 × 条件のマトリクスが Planner の自然言語推論にすべて任されている。新しい状態が増えるたびに指令書が膨張し、忘却・誤適用のリスクが線形に増える。
- **委譲案**: Daemon に `maestro plan recover --plan-id <id>` 系の単一エントリを実装し、内部で状態判定 → 必要 CLI 呼び出しを連鎖実行する（=「recovery を CLI 1 個に集約」）。Planner は「recover を呼ぶ」だけになり、指令書から状態遷移マトリクスを削れる。
  - 短期的妥協案として、planner.md の該当節を `cmd/maestro/cmd_plan_ops.go` 側コメントの参照リンクに置換するだけでも肥大は抑えられる。

#### 4-2. Verification phase の重複定義禁止規則が指令書ローカル [Medium]

- **該当箇所**: `templates/instructions/planner.md` の verification phase 章（「concrete に verification 命令を含めてはならない」「`__system_verify` は Daemon 自動投入」相当の記述）。
- **現状**: フェーズ分離規則（concrete / deferred の禁止コマンド、`__system_*` タスクの自動投入）は Planner ナラティブで規律化されている。CLI 側は受付時に該当フェーズの content を全文舐める検証を行っていない。
- **問題**: Planner が誤って concrete に `go test ./...` を入れたとき、初手で気付けない。verification 二重実行や成功条件のすり替えが起きうる。
- **委譲案**: `maestro plan add-task --phase concrete` 受付時、`content` に `go test` / `npm test` / `pytest` 等のキーワード（または config.yaml のフルチェックコマンド）が含まれていれば warning または reject。

#### 4-3. Five Questions Analysis / Bloom's Taxonomy 等のメタ分析手順 [Low]

- **該当箇所**: `templates/instructions/planner.md` の Five Questions Analysis、Bloom's Taxonomy、phase 分割判定節。
- **現状**: 「タスク分解前にこの 5 観点を分析する」「認知レベルを Bloom 分類で判定する」等の手続きが規定されている。
- **問題**: これらは思考の質を高めるためのフレームワークであり Daemon に委譲すべき性質では無いが、文章量として大きく Planner が読み飛ばす可能性がある。
- **委譲案**: Daemon 委譲は不要。指令書側で「分析フレームワーク」セクションとして独立ファイル化し、`planner.md` 本体からは要約のみリンク参照に変更する選択肢のみ示す（責務移譲ではなく構造改善）。

### 観点 5: CLI ラップの抜け（.maestro/ 直接編集の指令書ヒント）

#### 5-1. .maestro/ 直接編集の指令は無し [該当なし、ただし注意点 1 件]

- **該当箇所**: `templates/maestro.md` § Agent の責務原則、`templates/instructions/worker.md` § `.maestro/` アクセス制御、L1/L2 enforcement 節。
- **現状**: 全 role の指令書で「`.maestro/` 以下の YAML 直接書き込み禁止」「全状態変更は CLI 経由」が冒頭で宣言されている。Worker は L1（disallowedTools の `Read(.maestro/{state,queue,results,locks,logs}/**)` `Read(.maestro/{config.yaml,dashboard.md})`）と L2（Bash/Write/Edit hook）の二重で技術的にもブロック済み。
- **問題**: なし。
- **注意点**: `templates/instructions/worker.md` で `.maestro/worktrees/` 配下のソースコードは「読み書き可能（管理ファイルの直接操作は禁止）」と書かれているが、`.git/` config 等の管理ファイルが Bash 経由で書かれた場合 L2 hook がカバーしているかは追加検証推奨（後述 7-3 と関連）。

#### 5-2. `git add -A` / `git add .` 禁止が文章規約 [Low]

- **該当箇所**: `templates/instructions/worker.md` § Commit Task Protocol。
- **現状**: 「`git add -A` / `git add .` は使用禁止」「`.maestro/` 以下はステージングしない」という規律は文章のみ。L2 hook が `git add -A` を直接ブロックしているか不明（hook script 本体未確認）。
- **問題**: 規律違反が技術的に検出されない可能性。
- **委譲案**: L2 worker policy hook に `git add -A|--all|\.` 検出と reject を追加する（既にカバーされていれば不要）。worktree モード時は `__system_commit` が配信されないため影響は限定。

### 観点 6: 指令書の肥大化・重複・古さ

#### 6-1. `templates/instructions/planner.md` が 1193 行と突出して大きい [High]

- **該当箇所**: ファイル全体（行数: 1193）。比較対象 — orchestrator.md: 269 行、worker.md: 515 行、maestro.md: 114 行。
- **現状**: Plan ライフサイクル、CLI コマンド全種類のサンプル、persona/skill 注入規則、verification phase 規律、conflict recovery、publish 各種 recovery、Five Questions Analysis、Bloom's Taxonomy、shell quoting 注意、過去インシデント参照（`cmd_1775542302` `cmd_1775548269` 等の固有 ID 言及）、phase 分離規則 — がすべて 1 ファイルに同居。
- **問題**:
  - 過去インシデント由来の but-now-historical な防止策が削除されず堆積している（特定 cmd_id の言及はナラティブ価値が低い）。
  - LLM が指令書を「読み飛ばす」リスクが文書サイズと相関する。
  - SSOT が複数走る恐れ（同じ規律が複数節で言及されている疑い）。
- **委譲案**:
  - 過去インシデント言及（特定 cmd_id 含む）を別ファイル `templates/instructions/planner_lessons.md` に切り出し、本体は規律のみに整理。
  - Conflict / Publish recovery 節は CLI ヘルプおよび `cmd_plan_ops.go` のコメントに正本を置き、planner.md からは要約 + 参照リンクのみとする。
  - Five Questions Analysis / Bloom's Taxonomy は別 skill 化（`skills/planning_framework.md`）して `skill_refs` で必要時注入する設計に寄せる。

#### 6-2. shell quoting / heredoc の注意書きが Planner に蓄積 [Medium]

- **該当箇所**: `templates/instructions/planner.md` の shell quoting 章および `--content` 渡し時の注意。
- **現状**: 「`--content` には heredoc を使う」「シングルクォート内のシングルクォートは `'\''` でエスケープ」等、CLI 起因のハマり所が指令書に列挙されている。これは CLI 設計の仕様（`--content` を string 引数で受ける）の弱さを Planner ナラティブで補っている。
- **問題**: Planner の認知負荷が高い。Daemon/CLI 改善で根治できる種類の負債である。
- **委譲案**: `maestro plan add-task` に `--content-file <path>` または `--content-stdin`（`-` 値）を追加し、Planner には「長文は --content-file に書き出して渡す」とだけ指示する。shell quoting 節は大幅に簡略化できる。

#### 6-3. Tier1/Tier2 SSOT 化の取り扱いは適切 [該当なし]

- **該当箇所**: `templates/maestro.md` § 破壊的操作の安全規則（D001-D008）。
- **現状**: maestro.md が SSOT、worker.md は Worker 固有規則と参照のみ。重複していない。
- **問題**: なし。
- **委譲案**: 不要。

#### 6-4. Worker `.maestro/` 制御プレーン表が maestro.md と worker.md の双方に存在 [Low]

- **該当箇所**: `templates/maestro.md` Agent 責務節と `templates/instructions/worker.md` `.maestro/` アクセス制御節。
- **現状**: Worker からのアクセス可否が両方で表化されており、ほぼ同内容を 2 度記述している。
- **問題**: 軽微な不整合（worker.md 側のみ `worktrees/**` が「読み書き可能だが管理ファイル直接操作禁止」と詳細記述）が生じうる。
- **委譲案**: maestro.md の表は role 共通の上位規則のみに残し、Worker 固有の `worktrees/**` 規律は worker.md に集約。表の重複を解消する。

### 観点 7: ツール制約・ホスト依存性の対称性

#### 7-1. role 間のツール制約は技術的に強制されている [該当なし、ただし注意点あり]

- **該当箇所**: `internal/agent/launcher.go` の `allowedToolsByRole` (lines 51-73), `workerDisallowedTools` (lines 334-356), `appendDisallowedTools` (lines 320-329)、`internal/agent/policy_checker.go:178-199` `Bash|Write|Edit` matcher。
- **現状**:
  - Orchestrator: `Bash(maestro plan submit:*)` `Bash(maestro plan complete:*)` `Bash(maestro skill list:*)` のみ許可、Read 不要。
  - Planner: `Bash(maestro:*)` + `Read(.maestro/**)` 許可、加えて `Bash(maestro plan unquarantine:*)` を `appendDisallowedTools` で拒否。
  - Worker: allow-list 空（unrestricted）、`workerDisallowedTools` で tmux kill, `.maestro/` 制御プレーン Read, plan ops 系をブロック。L2 hook で動的判定。
- **問題**: なし。設計通り。
- **注意点**: 観点 7-2/7-3 を参照。

#### 7-2. Planner と Worker の制約レイヤーが非対称 [Medium]

- **該当箇所**: `internal/agent/launcher.go` `allowedToolsByRole`、`policy_checker.go` `HookSettings`。
- **現状**: Worker は L1（`--disallowedTools`）+ L2（PreToolUse hook）の二層。Planner / Orchestrator は L1 (`--allowedTools` allow-list) のみで L2 hook は配線されていない。
- **問題**:
  - Planner の `Bash(maestro:*)` 配下に将来追加された破壊的サブコマンド（仮に `maestro daemon shutdown` のような）が自動的に Planner に届いてしまう。allow-list の粒度が `maestro:*` で粗いため、新規 CLI 追加時の安全性は CLI 側設計者の規律に依存している。
  - Planner の `Read(.maestro/**)` は `state/`, `queues/`, `results/`, `locks/` 等の制御プレーンを含む全領域に及ぶ。worker.md 観点では制御プレーン Read は禁止であり、role 間で原則が非対称。
- **委譲案**:
  - Planner にも L2 hook（軽量版）を配線し、新規 `maestro` サブコマンド追加時のホワイトリスト管理を hook 側に集約。または `--allowedTools` をサブコマンド粒度に分解（`Bash(maestro plan add-task:*)` 等）。
  - Planner の `Read(.maestro/**)` を実利用範囲（おそらく `state/plans/<id>.yaml` `queues/plan_queue.yaml` 等）に絞る。少なくとも `locks/` と `logs/raw/` は除外可能。

#### 7-3. macOS 固有保護が Worker 文章規約のみ [Medium]

- **該当箇所**: `templates/instructions/worker.md` § Worker 固有規則 § macOS 固有保護。
- **現状**: 「`/System/`, `/Library/`, `/Applications/`, `/Users/`（プロジェクトツリー外）の削除・再帰変更を禁止」「`rm` 実行前に `realpath` 検証」が文章で要求される。L2 hook（`internal/agent/worker_policy_hook.sh` 想定）が実装しているかは hook script 本体未確認。
- **問題**: ホスト依存規律（macOS パス）が hook で実装されていない場合、Linux ホストでは無効、Worker LLM が誤って rm したときに止められない。
- **委譲案**: L2 hook 内で OS 判定（`uname` または env）し、macOS 時のみ `/System|/Library|/Applications|/Users/` の write/delete をブロック。Linux 用にも `/etc|/usr|/var` 系の同等規律を追加する。

#### 7-4. Worker の `git push` 全面禁止は文章規約のみ [Medium]

- **該当箇所**: `templates/instructions/worker.md` § Worker 固有規則「`git push` 全面禁止」。
- **現状**: maestro.md Tier3 の `--force-with-lease` 代替も Worker には適用外と明記。だが L2 hook で `git push` を全パターン reject しているかは未確認。
- **問題**: Worker LLM が忘却した場合、リモートに到達するリスク。
- **委譲案**: L2 hook で Worker の `git push` を Bash パターンマッチで一律 reject する（既に実装されていれば確認のみ）。

### (2) 統計サマリ

| 観点 | Critical | High | Medium | Low | 該当なし |
|------|---------:|-----:|-------:|----:|---------:|
| 1. 状態管理 | 0 | 1 | 1 | 1 | - |
| 2. ID 生成 | 0 | 0 | 0 | 0 | 1 |
| 3. lease / fencing | 0 | 0 | 0 | 0 | 1 |
| 4. 複雑なロジック | 0 | 1 | 1 | 1 | - |
| 5. CLI ラップ抜け | 0 | 0 | 0 | 1 | 1 |
| 6. 指令書肥大・重複 | 0 | 1 | 1 | 1 | 1 |
| 7. ツール制約対称性 | 0 | 0 | 3 | 0 | 1 |
| **合計** | **0** | **3** | **6** | **4** | **5** |

### 総評

- **Critical 級は 0 件**。ID / lease_epoch / 制御プレーン書込みという設計の根幹については、Daemon 委譲が文書・実装の両面で徹底されている。観点 2/3 は「該当なし」。
- **High 3 件は全て Planner / 状態管理に集中**。1-1（Plan 進行不変条件が文章規約止まり）、4-1（recovery 分岐爆発）、6-1（planner.md 1193 行）はいずれも「Planner ナラティブが Daemon ハードガードを代替している」共通構造であり、`maestro plan recover` 系の集約 CLI 新設と planner.md の構造分割でまとめて改善可能。
- **Medium 6 件のうち 3 件はツール制約の非対称性**（7-2/7-3/7-4）。Worker の二層防御モデルを Planner と Linux ホストにも展開し、L2 hook を SSOT 化することで一括解決できる。残る 3 件（1-2 partial_changes 自己申告、4-2 verification 二重定義禁止、6-2 shell quoting）は CLI 側の小改修（`--content-file` 追加、フェーズ別 keyword reject、git status 自動照合）で個別対応。
- **Low 4 件**は構造改善・文書整理レベルであり緊急度は低いが、6-1 の planner.md 分割と合わせて取り組むのが効率的。

---

## (3) 設計整合性 (Design Integrity)

# 設計整合性レビュー: ツール制限・破壊的操作・lease/fencing・シグナル経路

調査日: 2026-04-27
調査者: worker1 (researcher persona)
対象: Maestro Multi-Agent System (`/Users/mzk/Works/src/github.com/msageha/maestro_v2`)

調査範囲:
1. ツール制限の技術的強制 (L1: `--allowedTools`/`--disallowedTools`, L2: PreToolUse hook)
2. 破壊的操作 (D001-D008, B001-B004) の hook ブロック
3. `.maestro/` 制御プレーンへのアクセス制御
4. `lease_epoch` / fencing token による並行制御
5. シグナル経路整合 (merge_conflict / commit_failed / circuit_breaker_tripped / quarantine 等)

各項目で「指令書記述 ↔ 実装の対応」を 1 対 1 で目視照合した結果を記載する。実ファイル参照のみに基づく報告であり、推測や未検証情報は含めない。

### V1: ツール制限の技術的強制

#### 設計意図

`templates/maestro.md` および各 role 指令書 (`templates/instructions/*.md`):
- Orchestrator / Planner: `--allowedTools` ホワイトリスト方式で `Bash(maestro:*)` 系のみ許可
- Worker: `--disallowedTools` (L1) + PreToolUse hook (L2 / `worker-policy.sh`) の二層防御
- Worker は `Bash | Write | Edit` matcher で配線され、動的判定で破壊的操作と `.maestro/` 制御プレーンを拒否

#### 実装状況

##### Orchestrator / Planner ホワイトリスト

`internal/agent/launcher.go` `allowedToolsByRole` (該当箇所 L51-73):

```
"orchestrator": {
  Bash(maestro queue write planner --type command:*),    // L53
  Bash(maestro skill list:*),                            // L54
  Bash(maestro plan request-cancel:*),                   // L55
  Read(.maestro/dashboard.md),                           // L56
  Read(.maestro/results/planner.yaml),                   // L57
  Read(.maestro/config.yaml),                            // L58
  Read(.maestro/state/continuous.yaml),                  // L63
}
"planner": {
  Bash(maestro:*),           // L66
  Read(.maestro/**),         // L67
  Read(.maestro/dashboard.md), Read(.maestro/config.yaml), Read(.maestro/results/*)
}
"worker": {} // L72 未登録 = 全ツール許可
```

##### Worker L1 disallowedTools

`internal/agent/launcher.go` `workerDisallowedTools` (L334-356):

```
Bash(tmux kill-server:*) / Bash(tmux kill-session:*) / Bash(tmux kill-pane:*) / Bash(tmux kill-window:*)
Bash(maestro plan unquarantine:*) / Bash(maestro plan resume-merge:*) / Bash(maestro plan resolve-conflict:*)
Bash(maestro resolve-conflict:*)  // legacy
Read(.maestro/state/**) / Read(.maestro/queue/**) / Read(.maestro/results/**)
Read(.maestro/locks/**) / Read(.maestro/logs/**)
Read(.maestro/config.yaml) / Read(.maestro/dashboard.md)
```

##### Worker L2 hook 配線

`internal/agent/policy_checker.go` の `WriteHookScript` / `HookSettings` で `Bash | Write | Edit` matcher に PreToolUse hook (`worker-policy.sh`) を配線。Hook 本体: `internal/agent/worker_policy_hook.sh`。

#### ギャップ・該当箇所・推奨対応

該当なし。

L1 ホワイトリスト/ブラックリストは指令書 (`maestro.md §Worker Bash / ツール制約の全体像`、`worker.md §.maestro/ アクセス制御`) の表と完全に一致。Worker の二層防御は launcher 静的拒否と PreToolUse hook 動的判定の補完関係になっており、設計意図通り。

設計上のリスクとして「`launcher.go` で Worker の `--disallowedTools` に列挙された Read 対象パス」と「`worker-policy.sh` で Bash 経由でブロックされる `.maestro/` パス」のリストが二箇所で管理されているため、新パス追加時に同期漏れが発生する余地がある (Low)。`policy_parity_test.go` がこの整合性をテストしているが、新パス追加時の手順をコメントで明示するとよい。

| 深刻度 | 該当箇所 | 設計意図 | 実装状況 | ギャップ | 推奨対応 |
|--------|---------|----------|----------|---------|----------|
| Low | `launcher.go:349-355` ↔ `worker_policy_hook.sh:515-567` | `.maestro/` 制御プレーン Read/Write/Bash 全面拒否 | 二箇所で同義のリストを保持 | 新パス追加時の同期漏れリスク | コメントで管理点を相互参照、または定数を共有 |

### V2: 破壊的操作 (D001-D008, B001-B004) の hook ブロック

#### 設計意図

`templates/maestro.md §破壊的操作の安全規則 Tier 1` の D001-D008、`templates/instructions/worker.md` の Worker 固有規則 (Worker は `git push` 全面禁止)、および bypass 系 B001-B004 を hook 正規表現で動的にブロック。

#### 実装状況

`internal/agent/worker_policy_hook.sh`:

| ID | 設計意図 | 実装行 | 主な正規表現 |
|----|---------|--------|-------------|
| D001 | OS / ホーム破壊 | L202-221 | `rm\s+-[a-zA-Z]*[rR][a-zA-Z]*f...\s+(/$|/\s|~|/Users|/home|/root|/opt)` 等 3 形態 |
| D002 | プロジェクトツリー外 `rm -rf` | L223-277 | `realpath -P` で絶対化し `project_root` 配下確認 |
| D003 | Worker `git push` 全面禁止 | L279-291 | `git\s+push(\s|$)` 単一パターンで `--force-with-lease` 含めて全拒否 |
| D004 | 未コミット作業破壊 | L324-341 | `git reset --hard` / `git checkout -- .` / `git checkout .` / `git clean -f` (`-n` 併用は許可) |
| D005 | 権限昇格 | L343-354 | `(^|;|\||&&)\s*sudo\s` / `su\s` / `chmod -R` on `/(System|Library|Applications|usr|bin|sbin|etc)` |
| D006 | プロセス破壊 | L357-362 | `kill(all)?\s` / `pkill\s` (シェル境界つき) |
| D007 | ディスク破壊 | L365-373 | `mkfs|fdisk` / `dd\s+if=` / `diskutil\s+eraseDisk` |
| D008 | リモートコード実行 | L375-378 | `(curl|wget)\s.*\|\s*(ba)?sh` |
| B001 | pipe to shell | L381-384 | `\|\s*(/usr)?/bin/(ba)?sh\b` 等 3 パターン |
| B002 | `sh -c` / `bash -c` | L388 | `\b(bash|sh)\s+-[a-zA-Z]*c\b` |
| B003 | `eval` | L393 | `(^|;|\||&&)\s*eval\s+` |
| B004 | 絶対パス shell 起動 | L432 | `(^|[;|&(])\s*(/usr)?/bin/(ba)?sh\b` |

#### 検証: false positive / negative

- **D001 引数順序**: `-rf`, `-fr`, `--recursive --force`, 分離 (`-r ... -f`) の 3 ブランチで網羅 ✅
- **D003 `git push --force-with-lease`**: maestro.md Tier3 の代替として他 role には許可されるが、Worker には L289 で `git push` 自体を全面禁止。指令書 (`worker.md §Worker 固有規則`, `maestro.md §role 別の補足`) が「Worker には `--force-with-lease` 代替適用外」と明記しており実装と整合 ✅
- **D004 `git checkout`**: `git checkout -- .` (セパレータ付き) と `git checkout .` (セパレータなし) を別パターンでブロック。`git checkout .github/workflows/` 等のファイル指定は `\.(\s|$)` の単独 `.` トークンマッチで誤拒否されない ✅
- **D004 `git clean -n`**: dry-run は `! grep -qE '-[a-zA-Z]*n'` で許可される ✅
- **D005 `sudo`**: 行頭 / シェルセパレータ直後のみマッチするため `mysudo` のような prefix 衝突なし ✅
- **D008 `curl | bash`**: 中間パイプを許可するか不明。実装 `(curl|wget)\s.*\|\s*(ba)?sh` の `.*` で間に他コマンドが挟まれても OK だが、その分 `curl xxx | tee /tmp/log | bash` のような複合形態も正しく検出される ✅

#### ギャップ・該当箇所・推奨対応

該当なし (Critical/High はゼロ)。

潜在的な弱点として、以下のシナリオは現在の正規表現では捕捉できない可能性がある (Low):

| 深刻度 | パターン | 該当箇所 | 設計意図 | ギャップ | 推奨対応 |
|--------|---------|----------|----------|---------|----------|
| Low | `D008` 経路: `bash <(curl ...)` (process substitution) | `worker_policy_hook.sh:375-378` | リモートコード実行禁止 | `\|` のみ検査するため process substitution `<(...)` は通る可能性 | 別パターン `(<\s*\(\s*(curl|wget))` の追加検討 |
| Low | `D006` 経路: SIGKILL 同等の `kill -KILL`, `kill -9` の正規表現は `kill(all)?\s` に含まれる | `worker_policy_hook.sh:357-358` | プロセス破壊禁止 | 既に網羅済み | - |
| Low | `D004` 経路: `git restore --staged` での誤拒否は無いが `git restore .` (`-- .` なし) は通る | `worker_policy_hook.sh:328-336` | 未コミット作業破壊禁止 | `git restore` パターンが未追加 | `git restore` の hard 形態を検討 (ただし通常 worktree 操作ではリスク低) |

主要 D001-D008, B001-B004 はすべて確実にブロックされており、Critical/High の漏れは無い。

### V3: `.maestro/` 制御プレーンへのアクセス制御

#### 設計意図

`templates/instructions/worker.md §.maestro/ アクセス制御`:
- 制御プレーン (state, queues, results, locks, logs, config.yaml, dashboard.md) は L1+L2 の両方で読み書き拒否
- 参照ファイル (instructions, persona, skills, hooks, maestro.md, worktrees) はブロック対象外（ただし能動参照不要）

#### 実装状況

##### L1 `--disallowedTools` (Read 全面拒否)

`internal/agent/launcher.go:349-355`:

```
Read(.maestro/state/**) / Read(.maestro/queue/**) / Read(.maestro/results/**)
Read(.maestro/locks/**) / Read(.maestro/logs/**)
Read(.maestro/config.yaml) / Read(.maestro/dashboard.md)
```

注: 指令書では `queues/**` (複数形) と表記されるが launcher は `queue/**` (単数形) でブロック。これは指令書側の誤記の可能性あり（実装ディレクトリは単数）。

##### L2 hook (Bash / Write / Edit)

`internal/agent/worker_policy_hook.sh:515-539` (Bash 経由):

```
(cat|head|tail|less|more|vim|nano|sed|awk)\s+.*\.maestro/(state|queue|results|locks|logs|config|dashboard|verify\.yaml)
(ls|find|grep|rg)\s+.*\.maestro/(state|queue|results|locks|logs|config|dashboard|verify\.yaml)
(cp|mv|rsync|ln|install|tar|zip)\s+.*\.maestro/(state|queue|results|locks|logs|config|dashboard|verify\.yaml)
(cp|mv|rsync|install)\s+.+\s+[^ ]*\.maestro/   // 書き込み拒否
ln\s+(-[a-zA-Z]*\s+)*[^ ]+\s+[^ ]*\.maestro/   // symlink 拒否
(echo|printf|tee)\s.*>\s*\.maestro/             // redirect 拒否
>\s*\.maestro/                                  // 任意 redirect 拒否
```

`internal/agent/worker_policy_hook.sh:560-567` (Write/Edit 経由 / `tool_input.file_path` を `case` で分岐):

```
*/.maestro/state/* | */.maestro/queue/* | */.maestro/results/* | */.maestro/locks/* |
*/.maestro/logs/* | */.maestro/hooks/* | */.maestro/config.yaml | */.maestro/dashboard.md |
*/.maestro/verify.yaml
+ 同等の相対パス系
```

#### ギャップ・該当箇所・推奨対応

| 深刻度 | 該当箇所 | 設計意図 | 実装状況 | ギャップ | 推奨対応 |
|--------|---------|----------|----------|---------|----------|
| Low | `launcher.go:350` の `Read(.maestro/queue/**)` | 制御プレーン全面ブロック | 単数形 `queue` で実装、指令書 `worker.md §.maestro/ アクセス制御` は `queues/**` (複数形) 表記 | 単純な綴りの不一致 (実ディレクトリは単数 `queue`) | `worker.md` を `queue/**` に修正 |
| Low | `worker_policy_hook.sh:515-539` で `\.maestro/` の `.` がリテラル | 通常ファイルでの `.maestro` matching | `grep -qiE` で `.` は正規表現のメタ文字だが、実用上 `.` を任意一文字としても false positive の差は無視できる | 厳密にはエスケープ漏れだが影響は理論上のみ | `\.maestro/` への置換 (微小) |
| Low | `hooks/**` の取扱 | 参照ファイルだが Worker は編集不可とすべき | L2 で書き込み拒否、L1 では明記なし (Read は許可) | `hooks/**` の Read 許可は意図的か曖昧 | `worker.md` の参照ファイル表に「Read 可、Write/Edit 不可」と注記 |

`.maestro/results/**` への Worker 書き込みは L2 hook で確実に拒否される (実装 L560-567)。これは Worker が `maestro result write` CLI 経由でのみ結果を渡せるという設計と整合する。本タスクの acceptance_criteria が `.maestro/results/review_part_design_integrity.md` への新規作成を要求しているが、この経路は技術的に閉じられているため Worker 単独では達成不可能（後述「実行上の制約」参照）。

### V4: `lease_epoch` / fencing token の並行制御

#### 設計意図

`templates/maestro.md §lease_epoch ライフサイクル` および `templates/instructions/worker.md §lease_epoch ライフサイクル`:

- `LeaseManager` のみが採番。Worker は配信値をそのまま CLI に渡す
- `AcquireTaskLease` (pending → in_progress) で `+1`
- `ExtendTaskLease` (heartbeat) / `ReleaseTaskLease` (pending 戻し) では epoch 不変
- 失効後の再 dispatch でさらに `+1`、旧 epoch は永久に stale
- `task_heartbeat` および `result write` で queue epoch とリクエスト epoch を厳密一致比較
- 不一致時は `FENCING_REJECT` エラーコードで stderr 通知:
  - heartbeat: `task <id> epoch mismatch: queue=<N>, request=<M>`
  - result write: `task <id> lease_epoch mismatch: queue=<N>, request=<M>` （`lease_epoch` の語が入る点が heartbeat と異なる）

#### 実装状況

##### F1: 採番ロジック

| 操作 | 実装箇所 | epoch への影響 |
|------|---------|----------------|
| AcquireTaskLease | `internal/daemon/lease/manager.go:138` `(*ref.leaseEpoch)++` | +1 |
| ExtendTaskLease | `internal/daemon/lease/manager.go:227-233` | 不変 (expiry のみ更新) |
| ReleaseTaskLease | `internal/daemon/lease/manager.go:207-209` `releaseLease` | 不変 (status を pending に戻すのみ) |
| extendLeaseExpiry | `internal/daemon/lease/manager.go:176-183` | 不変 |

呼び出し元は `internal/daemon/queue_scan_collect.go:161` の `collectPendingTaskDispatches` 内の単一箇所 (for-break ループ)。同一スキャンサイクルで同タスクに複数 Acquire される経路は無い。

##### F2: 失効後再 dispatch で +1

1. `manager.go:323-334` `ExpireTasks` が `leaseExpiresAt` 超過タスクを検出
2. `queue_scan_collect.go:227-262` `collectExpiredTaskBusyChecks` が busy 判定経路で `ReleaseTaskLease` (L255) を呼ぶ
3. 次スキャンの `collectPendingTaskDispatches` で再 `AcquireTaskLease` → epoch `+1`
4. 旧 Worker が保有する epoch は queue 上で更新済みのため永久に stale

##### F3: fencing reject 検出経路と stderr 通知

| エンドポイント | 検出箇所 | エラーコード | stderr 文言 |
|----------------|---------|-------------|------------|
| heartbeat | `internal/daemon/task_heartbeat_handler.go:165-193` `leaseInvalidReason` | `uds.ErrCodeFencingRejectEpoch` (epoch) / `uds.ErrCodeFencingRejectStatus` (status) | `task %s epoch mismatch: queue=%d, request=%d` (L183) |
| result write | `internal/daemon/result_write_phase_a.go:298-310` `validateFencing` | `uds.ErrCodeFencingReject` | `task %s lease_epoch mismatch: queue=%d, request=%d` (L300) |

両者とも `uds.FencingDetails{Kind:"fencing_epoch_mismatch", ...}` を構造化エラーフィールドに添付し、`cmd/maestro/fencing_error.go:84-102` `emitFencingStderrEnvelope` で JSON 1 行を stderr に出力。Worker は `jq -r .details.kind` で検出可能。

終了コードマッピング (F-019):

| 状況 | exit code | 仕様要求 |
|------|-----------|----------|
| epoch mismatch | 10 (`ExitCodeFencingEpoch`) | 10 ✅ |
| max_runtime_exceeded | 11 (`ExitCodeMaxRuntimeExceeded`) | 11 ✅ |
| status mismatch | 12 (`ExitCodeFencingStatus`) | 12 ✅ |

##### F4: 古い epoch の拒否

`internal/daemon/queue_scan_helpers.go:15-23` `leaseInvalidReason`:

```go
if status != model.StatusInProgress { return "status" }
if leaseEpoch != expectedEpoch     { return "epoch" }
return ""
```

`!=` の厳密一致比較。M < N (古い)、M > N (未来) の双方を拒否。レース対策として heartbeat は `scanMu.RLock()` + queue ファイルロックの順で取得。`ExtendTaskLease` は epoch 不変なので並行 `AcquireTaskLease` との競合は無い。

##### F5: 仕様文書と実装メッセージ整合

| エンドポイント | worker.md / maestro.md 記載 | 実装文字列 (`fmt.Sprintf` 形式) | 整合 |
|----------------|------------------------------|--------------------------------|------|
| heartbeat | `task <id> epoch mismatch: queue=<N>, request=<M>` | `task %s epoch mismatch: queue=%d, request=%d` (`task_heartbeat_handler.go:183`) | ✅ |
| result write | `task <id> lease_epoch mismatch: queue=<N>, request=<M>` | `task %s lease_epoch mismatch: queue=%d, request=%d` (`result_write_phase_a.go:300`) | ✅ |

文言差 (`epoch mismatch` vs `lease_epoch mismatch`) は意図的でフェーズ別の責務反映。

#### ギャップ・該当箇所・推奨対応

該当なし。Critical/High/Medium/Low いずれも検出されず。

設計意図と実装は完全整合。AcquireTaskLease のみが epoch を採番、Extend/Release では不変、失効→Release→次 dispatch で `+1`、heartbeat と result write の両方で厳密一致比較、stderr 文言が仕様と一致、exit code が仕様 (10/11/12) と一致。古い epoch リクエストは `!=` で確実に拒否され、レース条件も `scanMu` + ファイルロックで防御されている。

### V5: シグナル経路整合 (merge_conflict / commit_failed / circuit_breaker_tripped / quarantine 等)

#### 設計意図

`templates/instructions/planner.md` 記載のシグナル種別と書式、復旧経路コマンド (`maestro plan resume-merge` / `unquarantine` / `request-cancel` / `retry-publish`) が、Daemon 実装の通知文字列リテラルおよび CLI ハンドラと 1 対 1 で整合していること。

#### 実装状況: シグナル書式 (S1)

| シグナル | planner.md 記載 | 実装箇所 | 整合 |
|---------|-----------------|---------|------|
| merge_conflict | `[maestro] kind:merge_conflict command_id:cmd_xxx phase:implementation worker:worker1 base:<sha> ours:<sha> theirs:<sha>\nconflict_files: ...` (planner.md L790-792) | `internal/daemon/queue_scan_phase_c.go:207-210` `[maestro] kind:merge_conflict command_id:%s phase:%s worker:%s base:%s ours:%s theirs:%s\nconflict_files: %s` | ✅ |
| commit_failed | planner.md L1004-1010 で構造化フィールド (kind/command_id/phase_id/worker_id/reason_code/error) を YAML 風に提示 | `queue_scan_phase_c.go:185-186` で CSV 風 1 行: `[maestro] kind:commit_failed command_id:%s phase:%s worker:%s reason:%s\nerror: %v` | ⚠️ Medium 形式の表現乖離 |
| circuit_breaker_tripped | `[maestro] kind:circuit_breaker_tripped command_id:cmd_xxx\nreason: consecutive_failures=N reached threshold=M` (planner.md L323-324) | `queue_scan_phase_a.go:118-119, 141-142` `[maestro] kind:circuit_breaker_tripped command_id:%s\nreason: %s`, reason 値は `circuitbreaker.go:122-124` で `consecutive_failures=%d reached threshold=%d` | ✅ |
| conflict_escalation | planner.md L862 で言及のみ、形式未記載 | `internal/reconcile/engine.go:220-221` `[maestro] kind:conflict_escalation command_id:%s worker_id:%s\nconflict resolution attempts exhausted — escalating to planner` | ⚠️ Low 文書補強 |
| publish_conflict | `[maestro] kind:publish_conflict command_id:cmd_xxx\nForward-merge of base branch into integration failed due to content conflicts.\nconflict_files: ...\nThe Planner should dispatch a worker...` (planner.md L873-878) | `queue_scan_phase_c.go:442-448` 同等メッセージ + auto-recover/手動 retry-publish の説明 | ✅ |
| publish_completed | `[maestro] kind:publish_completed command_id:cmd_xxx` (planner.md L956) | `queue_scan_phase_c.go:387-393` 同形式 + 補足説明 | ✅ |
| publish_quarantined | planner.md L949 で言及 | `internal/reconcile/engine.go:239-240` `[maestro] kind:publish_quarantined command_id:%s\npublish failures reached quarantine threshold — operator intervention required: %s` | ✅ |

#### 実装状況: 復旧経路コマンド (S2)

| コマンド | planner.md 定義 | CLI 実装 | Daemon ハンドラ | 整合 |
|---------|-----------------|---------|----------------|------|
| `maestro plan resume-merge --command-id <id>` | planner.md L250-260 | `cmd/maestro/cmd_plan_ops.go:159-183` `runPlanResumeMerge` operation=`resume_merge` | `internal/daemonapi/plan.go:87, 100` → `worktreeManager.ResumeMerge(ctx, commandID)` | ✅ |
| `maestro plan unquarantine --command-id <id> [--reason <text>]` | planner.md L234-247 | `cmd_plan_ops.go:117-155` operation=`unquarantine`、CLI 層で role=Planner なら拒否 | `daemonapi/plan.go:96-99` Daemon 側でも Planner 拒否 (多層防御) | ✅ |
| `maestro plan retry-publish --command-id <id>` | planner.md L263-272 | `cmd_plan_ops.go:187-211` operation=`retry_publish` | `daemonapi/plan.go:87, 100` → `worktreeManager.RetryPublish(commandID)` | ✅ |
| `maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]` | planner.md L219 | `cmd_plan_ops.go:13-59` target=planner, type=cancel-request | (cancel handler 経路) | ✅ |

#### 実装状況: シグナル → 復旧コマンドのフロー (S3)

| シグナル | フィールド | 復旧コマンド引数 | 整合 |
|---------|-----------|----------------|------|
| merge_conflict (command_id, phase, worker, base/ours/theirs, conflict_files) | conflict_files → 競合解決タスクの `--expected-paths`、command_id → resume-merge `--command-id` | ✅ |
| publish_conflict (command_id, conflict_files) | conflict_files → integration worktree 上の解決タスク (`--run-on-integration`)、command_id → retry-publish `--command-id` | ✅ |
| circuit_breaker_tripped (command_id, reason) | 明示的復旧コマンド不要、Planner が `plan complete` で報告 | ✅ |
| conflict_escalation (command_id, worker_id) | Planner 判断委譲 (resume-merge 再実行 or キャンセル) | ✅ |

#### 実装状況: 送出箇所の網羅性 (S4)

planner.md 記載シグナル 7 種すべてが実装側で送出される (上表参照)。逆に実装で送出されるが planner.md に記載されないシグナルは検出されず。

`merge_quarantine` (Merge 側の隔離信号) は planner.md にも実装にも存在しない。Merge 側は状態として保持されるが信号は送出されず、隔離信号は publish 側のみ (`publish_quarantined`)。

#### ギャップ・該当箇所・推奨対応

| 深刻度 | 該当箇所 | 設計意図 | 実装状況 | ギャップ | 推奨対応 |
|--------|---------|----------|----------|---------|----------|
| Medium | `templates/instructions/planner.md:1004-1010` ↔ `internal/daemon/queue_scan_phase_c.go:185-186` | commit_failed の通知書式の文書整合 | 実装は `kind:commit_failed command_id:%s phase:%s worker:%s reason:%s\nerror: %v` の CSV 風 1 行。文書側は構造化 YAML 風に列挙 | 表記の乖離 (フィールド数や reason_code 名は対応するが表現形式が異なる) | planner.md L1004-1010 を実装の CSV 風表記に修正、または実装を YAML 風統一 |
| Low | `templates/instructions/planner.md:862` | conflict_escalation の具体書式 | 実装側は `[maestro] kind:conflict_escalation command_id:%s worker_id:%s\n...` で固定 | 文書に書式例なし | planner.md L862 直下に実装文言の例を追加 |
| Low | `templates/instructions/planner.md` 全体 | merge 側 quarantine と publish 側 quarantined の区別 | merge 側は信号不送、publish 側のみ `publish_quarantined` | 両者の違いが文書で明示されていない | quarantine セクションに「Merge 側は状態のみ、信号は publish 側のみ」と明記 |

Critical / High はゼロ。merge_conflict / publish_conflict / circuit_breaker_tripped / publish_completed / publish_quarantined / conflict_escalation の 6 種は文書と実装で完全整合。commit_failed のみ表現形式の乖離あり (Medium)。

### 総合評価

#### 深刻度別サマリー

| 深刻度 | 件数 | 内訳 |
|--------|------|------|
| Critical | 0 | なし |
| High | 0 | なし |
| Medium | 1 | commit_failed メッセージ書式の表現乖離 (planner.md ↔ queue_scan_phase_c.go) |
| Low | 6 | (V1) 制御プレーンパス管理点の二箇所同期、(V2) `bash <(curl ...)` / `git restore .` の正規表現不足、(V3) `queue` vs `queues` 表記、`hooks/**` Read 取扱の文書化、(V5) conflict_escalation 書式・merge 側 quarantine の文書化 |

#### 設計意図と実装の整合度

- **V1 ツール制限**: 完全整合 (二層防御は launcher.go 静的拒否 + worker_policy_hook.sh 動的判定で正しく機能)
- **V2 破壊的操作**: 主要 D001-D008, B001-B004 はすべてブロック。Worker `git push` 全面禁止も `git\s+push(\s|$)` 単一パターンで包括
- **V3 `.maestro/` 制御プレーン**: L1+L2 多層防御で読み書き共に拒否。参照ファイル経路は誤拒否なし
- **V4 lease_epoch/fencing**: AcquireTaskLease のみが採番、Extend/Release で不変、失効→再 dispatch で `+1`、`!=` 厳密比較で古い epoch を確実に拒否、stderr 文言と exit code (10/11/12) が仕様一致
- **V5 シグナル経路**: 7 種シグナルと 4 種復旧コマンドが文書と実装で対応。commit_failed のみ表現乖離 (Medium)、conflict_escalation 等は書式の文書化推奨 (Low)

全体として Critical/High の漏れは無く、設計意図は実装で技術的に強制されている。改善余地は文書整備 (Low 6 件) と commit_failed 通知書式の文書統一 (Medium 1 件) のみ。

### 実行上の制約 (本タスクの acceptance_criteria 達成不能について)

`acceptance_criteria` は `.maestro/results/review_part_design_integrity.md` への新規作成を要求しているが、本検証で確認した通り `internal/agent/launcher.go:351` (`Read(.maestro/results/**)`) および `internal/agent/worker_policy_hook.sh:560-567` (`*/.maestro/results/*` への Write 拒否) により Worker 経路では技術的に書き込み不可能。これは設計意図そのものであり、本レポートで検証対象とした制御プレーン保護が正しく機能している証左でもある。

そのため本レポート本文は `/tmp/claude/review_part_design_integrity.md` に出力し、`maestro result write --status failed` で報告する。Planner 側で必要であれば、本ファイル内容を `.maestro/results/` 配下へ再配置 (Daemon 経由) するか、出力先を `/tmp/results/` 等に変更したタスク再投入を検討されたい (過去の Worker 学習知見でも同様の運用が示唆されている)。

---

## (4) Go コード品質 (internal/ + cmd/maestro/)

# Go コード品質レビュー (Part: code_quality)

調査範囲: `internal/**/*.go` および `cmd/maestro/**/*.go` (テスト `*_test.go` 除く)
調査手法: Explore SubAgent 3 並列 (agent+daemon / queue系 daemon サブセット / 残り全体) による Read/Grep 走査
エビデンスレベル: 全項目「参照済み」(コード読解で確認、実行検証は未実施)

### 1. 冗長コード

#### 1.1 worktree manager のエラーハンドリングパターン重複
- **ファイル**: `internal/daemon/worktree/recover_resume.go:129-193, 227-229, 346-349, 370-372, 391-393`
- **関数/型**: `finalizeResumeMergeIntegrationStatus()`, `tryMergeWorker()`, `resetWorkersToActive()`
- **現状の概要**: 「state 遷移呼び出し → エラー時 Warn ログ + return/continue」というパターンが 6 箇所以上で同一構造で繰り返される
- **問題**: コピー&ペーストによる保守負荷。ログフォーマット文字列が個別に定義されており、揺れが入りやすい
- **推奨対応**: `logAndReturnOnStatusError(state, status, fmtStr, args...)` 等のヘルパー抽出
- **深刻度**: Medium

#### 1.2 cancelMark / busy check の条件チェック→ログ→continue 繰り返し
- **ファイル**: `internal/daemon/queue_scan_phase_c.go:95-114`
- **関数**: `applyCancelDispatchAndBusyChecks()`
- **現状の概要**: cancelMarks のループ内で「条件 → ログ → continue」が 3 回連続
- **問題**: スコープが狭く影響は限定的だが、共通サブルーチン抽出余地あり
- **推奨対応**: `findTaskInQueue(tq, taskID)` ヘルパー + cancel 処理サブ関数化
- **深刻度**: Low

#### 1.3 execDeliver の orchestrator/planner 重複ロジック
- **ファイル**: `internal/agent/executor_core.go:572-629`
- **関数**: `execDeliver`
- **現状の概要**: orchestrator 分岐と planner/other 分岐で `ensureClaudeRunning → detectBusyWithRetry → sendAndConfirm` の同一構造を別個に記述
- **問題**: DRY 違反。busy check 詳細度が違うだけで全体構造は同一
- **推奨対応**: `deliverWithBusyCheck()` 共通メソッド抽出 + orchestrator 固有ログを分岐
- **深刻度**: Medium

#### 1.4 isPromptReady と isAgentReady の prompt スキャン重複
- **ファイル**: `internal/agent/tmux_pane_io.go:130-157`, `internal/agent/process_manager.go:211-227`
- **関数**: `isPromptReady`, `isAgentReady`
- **現状の概要**: `maxPromptSearchLines` 行を逆向きスキャンし trimmed prompt マーカー (❯/>) を探すロジックが 2 箇所に分散
- **問題**: 軽微な重複だが prompt 仕様変更時に修正漏れリスク
- **推奨対応**: `isPromptReady()` を `isAgentReady()` の claude-code 分岐から再利用
- **深刻度**: Low

### 2. デッドコード

#### 2.1 Manager.AutoCommit / AutoMerge の未呼び出し
- **ファイル**: `internal/daemon/worktree/manager.go:877-880`
- **関数**: `(*Manager) AutoCommit() bool`, `(*Manager) AutoMerge() bool`
- **現状の概要**: いずれも `wm.config.AutoCommit/AutoMerge` を返すだけのアクセサ
- **問題**: Grep で呼び出し元 0 件 (推定: 直接呼び出しのみ調査)。`worktreeManager` インターフェース (`internal/daemon/queue_handler_deps.go:147-148`) には宣言されているが実利用なし
- **推奨対応**: 削除またはインターフェースから外す
- **深刻度**: Low

#### 2.2 (備考) 第 1 / 第 2 Explore は agent / daemon 主要部分のデッドコードを「該当なし」と報告
- 広範囲のシンボル逆引きは時間制約で全数実施できておらず、追加調査の余地あり (リフレクション/インターフェース呼び出し含む網羅は未実施)

### 3. リファクタリング候補

#### 3.1 800 行超の大型ファイル群
- **ファイル**:
  - `internal/daemon/worktree/manager.go` (約 880 行)
  - `internal/tmux/session.go` (約 864 行)
  - `internal/agent/launcher.go` (約 832 行)
  - `internal/daemon/worktree/merge_publish.go` (約 783 行)
- **現状の概要**: 単一ファイルで複数の責務 (lifecycle, status, IO, merge, publish 等) を内包
- **問題**: 可読性とテスト性低下、変更影響範囲の特定が困難
- **推奨対応**: 機能単位でサブファイル分割 (`manager_lifecycle.go`, `manager_status.go` 等)
- **深刻度**: Medium

#### 3.2 Launch 関数の 4 段階責務集約
- **ファイル**: `internal/agent/launcher.go:82-145`
- **関数**: `Launch`
- **現状の概要**: 64 行内に「pane 変数読み込み → system prompt 構築 → CLI args 構築 → runtime 起動」が直列配置
- **問題**: エラーハンドリングが各段階で異なり (return / log+fallback)、ユニットテストが困難
- **推奨対応**: `loadAgentConfig()`, `buildPrompt()`, `buildAndLaunchArgs()` 等への分割
- **深刻度**: Medium

#### 3.3 buildLaunchArgs の固定 6 段階チェーン
- **ファイル**: `internal/agent/launcher.go:270-279`
- **関数**: `buildLaunchArgs`
- **現状の概要**: `launchArgsCore → appendAllowedTools → appendDisallowedTools → appendNotificationSettings → appendWorkspaceReadAllowances → appendResolvedMaestroBashAllowances` の固定順チェーン
- **問題**: 新規追加が末尾固定。順序依存が暗黙的でテスト時の組み立て自由度が低い
- **推奨対応**: `LaunchArgsBuilder` 構造体 + method chain 化、または options 構造体導入
- **深刻度**: Medium

#### 3.4 stepPlannerSignalsDeferred の 4 レベルネスト
- **ファイル**: `internal/daemon/queue_dispatch.go:22-139`
- **関数**: `stepPlannerSignalsDeferred`
- **現状の概要**: Phase-level / Command-level の判定が if-if-if-else の 4 段ネスト (37-113 行)
- **問題**: Phase/Command 判定ロジックが条件式に隠蔽され、フローが追いにくい
- **推奨対応**: Phase-level と Command-level 処理を独立メソッドに分割
- **深刻度**: Medium

#### 3.5 applyCancelDispatchAndBusyChecks の 4 レベルネスト
- **ファイル**: `internal/daemon/queue_scan_phase_c.go:90-121`
- **関数**: `applyCancelDispatchAndBusyChecks`
- **現状の概要**: `if cancelMarks > 0 → for marks → for tasks → if id 一致` の 4 段ネスト
- **問題**: 内部ループは「task ID で task ポインタを引く」だけで抽出可能
- **推奨対応**: `findTaskInQueue(tq, taskID) *Task` ヘルパー抽出
- **深刻度**: Medium

#### 3.6 applyMergeResultSignals の複雑分岐
- **ファイル**: `internal/daemon/queue_scan_phase_c.go:231-301`
- **関数**: `applyMergeResultSignals`
- **現状の概要**: `MarkPhaseMerged` 判定が 7 つのコメント段落で説明される複雑条件
- **問題**: 条件意図が自明でなく、コメントなしでは可読性が極めて低い
- **推奨対応**: `isStatusSafeForMarkMerged(status) bool` 関数抽出 + ユニットテスト
- **深刻度**: Medium

#### 3.7 maybeAutoRecoverAfterResolution の switch 分岐
- **ファイル**: `internal/daemon/result_write_handler.go:181-232`
- **関数**: `maybeAutoRecoverAfterResolution`
- **現状の概要**: 復帰アクションの switch が複数段階の条件判定を内包
- **問題**: ケース内処理が肥大
- **推奨対応**: ケース毎にヘルパーメソッド抽出
- **深刻度**: Low

### 4. バグの可能性

#### 4.1 Phase C scan: Load エラー時に zero 値で後続処理続行
- **ファイル**: `internal/daemon/queue_scan_phase_c.go:22-50`
- **関数**: `executeScanPhaseCBody`
- **現状の概要**:
  ```go
  commandQueue, commandPath, err := qh.queueStore.LoadCommandQueue()
  if err != nil { qh.log(...); /* err は捨てて続行 */ }
  taskQueues, err := qh.queueStore.LoadAllTaskQueues()
  if err != nil { qh.log(...); /* 同上 */ }
  notificationQueue, notificationPath, err := qh.queueStore.LoadNotificationQueue()
  if err != nil { qh.log(...); /* 同上 */ }
  signalQueue, signalPath, err := qh.queueStore.LoadPlannerSignalQueue()
  if err != nil { qh.log(...); /* 同上 */ }
  ...
  signalIndex := buildSignalIndex(signalQueue.Signals) // Load 失敗時は空の signalQueue で処理継続
  qh.applyCancelDispatchAndBusyChecks(..., commandQueue, commandPath, taskQueues, notificationQueue, notificationPath)
  qh.syncIdleAfterPhaseC(commandQueue, taskQueues, notificationQueue)
  ```
- **問題**:
  - Load エラー時、戻り値の zero 値 (struct / nil map / nil slice) のまま applyCancelDispatch/sync/buildSignalIndex を呼ぶ。Go の nil slice/map range は安全なため `signalQueue.Signals` 自体は panic しないが、キュー内容が「空」として扱われ、dispatch / busy / signal / metrics / dashboard の反映が欠落または誤反映される可能性がある。
  - エラーログのみで `err` を上位に返さない設計のため、呼び出し側が Phase C を失敗扱いにできない。
- **推奨対応**: 各 Load エラーで early return し、その scan cycle の apply / metrics / dashboard 更新を中止する。部分継続する場合でも、ロード成功したキューだけを処理対象にする明示的な success flag を持たせる。
- **深刻度**: High (panic ではなく、失敗キューを空として扱う状態反映リスク)

#### 4.2 confirmSubmittedOrRetry のサイレント失敗
- **ファイル**: `internal/agent/message_deliverer.go:147-182`
- **関数**: `confirmSubmittedOrRetry`
- **現状の概要**: `CapturePaneJoined` のキャプチャエラー時に `return nil` (line 159)。プローブ予算 exhausted 時も `return nil` (line 181)
- **問題**: 双方とも caller に「成功」シグナルを返すため、配信失敗が隠蔽される。コメント (F-016) によれば double plan_submit 防止の意図的設計だが、silent failure リスクが残る
- **推奨対応**: 中間結果型 (`SubmitProbeResult{ProbeAttempted/NoProbeNeeded/ProbeExhausted/ProbeError}`) を返し、caller に retry 判断を委ねる
- **深刻度**: Critical (intentional だが配信欠落の根本原因)

#### 4.3 ensureWorkingDir の partial failure による state 不整合
- **ファイル**: `internal/agent/process_manager.go:84-143`
- **関数**: `(*processManager) ensureWorkingDir`
- **現状の概要**: `RespawnPane` → `waitForShell` → `ResetClearReady` → Claude relaunch → `waitReadyStrict` → `SetCWD` の段階実行。途中 error 時はその場で return する。
- **問題**: `RespawnPane` 後かつ `SetCWD` 前に失敗すると、実 pane は新しい workingDir 側へ移動している一方で paneState の `cwd` は旧値のまま残る。次回配送で不要な respawn が再実行される可能性がある。ただし `cwd` が旧値のため「一致と誤判定して配送する」経路ではない。
- **推奨対応**: step 完了状態を track し、失敗時に paneState を明示的に unknown/dirty 扱いへ更新する。`SetCWD` 失敗は warning のみで握りつぶさず、再同期可能な状態にする。
- **深刻度**: Medium (state corruption というより再同期不全 / 不要 respawn リスク)

#### 4.4 deliverPlannerSignal の retry ループでの cancel/context 取り扱い
- **ファイル**: `internal/daemon/queue_dispatch.go:237-274`
- **関数**: `deliverPlannerSignal`
- **現状の概要**:
  ```go
  for attempt := 0; attempt <= maxRetries; attempt++ {
    if attempt > 0 {
      select { case <-ctx.Done(): return ...; case <-time.After(retryDelay): }
    }
    attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
    err := qh.deliverPlannerSignalOnce(attemptCtx, ...)
    cancel()
    ...
  }
  ```
- **問題**:
  - `cancel()` の即時呼び出し自体は timer 解放として妥当だが、panic 時には実行されない。
  - ループ先頭で `ctx.Err()` 確認がないため、外側 ctx キャンセル直後の attempt 0 でも `deliverPlannerSignalOnce` まで進む。
- **推奨対応**: ループ先頭で `ctx.Err()` を確認する。panic-safe にするなら attempt 本体を小さなクロージャに閉じ込めて `defer cancel()` するが、通常経路では現在の即時 `cancel()` を遅らせない。
- **深刻度**: Medium

#### 4.5 dependencyResolver.GetStateManager() の nil 戻り値リスク
- **ファイル**: `internal/daemon/queue_scan_collect.go:362-365` 付近 (`checkInProgressDependencyFailuresDeferred`)
- **現状の概要**: `if HasStateReader() { GetStateManager().UpdateTaskState(...) }` の構造
- **問題**: `HasStateReader()` が true でも `GetStateManager()` が nil を返す実装可能性。インターフェース契約が緩く、nil チェックなしで .UpdateTaskState を呼ぶ
- **推奨対応**: 戻り値 nil チェック or インターフェース契約を `GetStateManager()` 単独で nil 返さないと明文化
- **深刻度**: Medium

#### 4.6 リカバリ用 setIntegrationStatus のエラー無視
- **ファイル**: `internal/daemon/worktree/recover_resume.go:129-193`
- **関数**: `finalizeResumeMergeIntegrationStatus`
- **現状の概要**:
  ```go
  if tErr := wm.setIntegrationStatus(state, IntegrationStatusMerged, now); tErr != nil {
    wm.Log(Warn, "...", tErr)
    _ = wm.setIntegrationStatus(state, IntegrationStatusFailed, now) // ← err 無視
  }
  ```
- **問題**: リカバリ自体が失敗した場合、状態が未確定のまま続行。ログにもリカバリ失敗が残らない
- **推奨対応**: リカバリ err は必ず Log + 上位伝播 (Phase A 再試行のため)
- **深刻度**: High

#### 4.7 ResumeMerge: 部分状態変更後の saveState 漏れ
- **ファイル**: `internal/daemon/worktree/recover_resume.go:116-143`
- **関数**: `ResumeMerge`
- **現状の概要**: 行 116-140 で複数の状態遷移を実行後、行 141-143 で `saveState` 呼び出し。途中の遷移で error が返ると saveState 前に return
- **問題**: メモリ上の `state` には partial 変更が残ったまま。次回呼び出し時に古い state から復帰される設計上の脆弱性
- **推奨対応**: トランザクション的アプローチ (snapshot+rollback)、または saveState を `defer` で確実化
- **深刻度**: High

#### 4.8 clearAndConfirm の連続キャプチャエラーで timeout まで待ち続ける
- **ファイル**: `internal/agent/message_deliverer.go:225-300, 365-399`
- **関数**: `clearAndConfirm`, `clearConfirmationPoller.poll`
- **現状の概要**: poll 内のキャプチャエラーは `p.reset()` のみで stableCount を 0 にリセットして続行
- **問題**: 同じキャプチャエラーが連続発生する状況では、無限ループではないが timeout まで成功不能な poll を続ける。`ClearMaxAttempts` 分だけ遅延が積み上がる可能性がある。
- **推奨対応**: 連続 N 回のキャプチャエラーで early abort (max error budget)
- **深刻度**: Medium

#### 4.9 daemon_shutdown.go の defer 順序と goroutine 連携
- **ファイル**: `internal/daemon/daemon_shutdown.go:44-60`
- **関数**: `waitSignals`
- **現状の概要**:
  ```go
  shutdownDone := make(chan struct{})
  var closeShutdownDone sync.Once
  defer closeShutdownDone.Do(func(){ close(shutdownDone) })
  go func(){ select { case <-sigCh: ...; case <-shutdownDone: return } }()
  ```
- **問題**: 通常フローでは問題ないが、panic 時に defer 実行と goroutine 終了の順序が不確定
- **推奨対応**: 明示的 close + context cancel に統一して goroutine ライフサイクルを単純化
- **深刻度**: Medium

#### 4.10 context.Background() のロガー直接利用
- **ファイル**: `internal/daemon/quality_gate.go:126`, `internal/daemon/core/core.go:100, 108` 他
- **現状の概要**: `dl.slogger.Log(context.Background(), ...)` の形でログ呼び出し
- **問題**: Daemon shutdown ctx が利用可能なケースでも捨てているため、シャットダウン中もログ出力が継続。slog の trace 連携にも影響
- **推奨対応**: 利用可能な ctx (d.ctx) を引き回す
- **深刻度**: Medium

#### 4.11 PeriodicScan / debounce で context.Background() 生成
- **ファイル**: `internal/daemon/scan_orchestrator.go:39-43`, `internal/daemon/debounce_controller.go:100+`
- **現状の概要**: shutdownCtx が nil の場合に `context.Background()` を生成
- **問題**: 非テストコードでも nil フォールバック経路が存在し、shutdown 伝播が遮断される可能性
- **推奨対応**: 初期化時に shutdownCtx を必須化、テスト用フォールバックは build tag で隔離
- **深刻度**: Low (現状はテスト互換のための意図的設計)

#### 4.12 busyDetector.softRetryUndecided の cancel 原因区別なし
- **ファイル**: `internal/agent/busy_detector.go:243-272`
- **関数**: `softRetryUndecided`
- **現状の概要**: `sleepCtx` cancel で即 `VerdictBusy` 返却 (intentional)
- **問題**: cancel 原因が shutdown か内部 timeout か区別できない。timeout 中の shutdown は本来 Undecided 維持で十分なケースも Busy 昇格される
- **推奨対応**: `context.Cause()` (Go 1.20+) で原因判定。または明示コメントで設計意図を残す
- **深刻度**: Medium

#### 4.13 clearConfirmationPoller hash stability 判定の race
- **ファイル**: `internal/agent/message_deliverer.go:365-399`
- **関数**: `clearConfirmationPoller.poll`, `isConfirmed`
- **現状の概要**: `hashChanged` は一度 true になると不可逆。stableCount は同 hash 連続でインクリメント。`hashChanged && stableCount >= 2` で confirmed 判定
- **問題**: pane content が deterministic cycle (hash 変化後に元値へ戻る) の場合 false positive で confirmed 判定。実運用では `clearTextVisible()` 主条件のため発火稀
- **推奨対応**: clearTextVisible() を主条件に明文化、hash check は補助で OK
- **深刻度**: Medium (実運用では Low)

#### 4.14 isAgentBusy の undecidedTracker nil 経路
- **ファイル**: `internal/daemon/queue_dispatch.go:327-380`
- **関数**: `isAgentBusy`
- **現状の概要**: `undecidedTracker == nil` の場合 count = 0 のまま閾値比較で常に false
- **問題**: 警告ログが出ない経路。undecidedTracker 未初期化時のテスト時に逆に「正常扱い」される
- **推奨対応**: nil-safe ヘルパー or 初期化保証
- **深刻度**: Low

#### 4.15 events.bus の Subscribe 即時 cancel 時のイベント欠落
- **ファイル**: `internal/events/bus.go:182, 241`
- **関数**: `Subscribe`, `SubscribeCoalesced`
- **現状の概要**: ctx.Done() 時 `for range sub.ch {}` で drain
- **問題**: 設計上 at-most-once。重要 event は registration 直後 cancel で失われる
- **推奨対応**: Critical event に at-least-once 経路を別途設ける
- **深刻度**: Low

#### 4.16 resultWritePhaseA のエラーラップ抜け
- **ファイル**: `internal/daemon/result_write_phase_a.go:83-86`
- **関数**: `resultWritePhaseA`
- **現状の概要**: `&resultWriteError{ErrCodeInternal, err.Error()}` で string 化
- **問題**: errors.Is/As チェーンが切れ、原因解析時に root cause 追跡不可
- **推奨対応**: `fmt.Errorf("...: %w", err)` か独自 error type に Unwrap 実装
- **深刻度**: Low

### (4) 統計サマリ

| 観点 | 件数 |
|------|------|
| 1. 冗長コード | 4 |
| 2. デッドコード | 1 (+情報 1) |
| 3. リファクタリング候補 | 7 |
| 4. バグの可能性 | 16 |
| **合計** | **28** |

#### 深刻度別

| 深刻度 | 件数 | 主な対象 |
|--------|------|---------|
| **Critical** | 1 | 4.2 confirmSubmittedOrRetry silent failure |
| **High** | 3 | 4.1 Phase C Load error 継続 / 4.6 リカバリ err 無視 / 4.7 ResumeMerge partial state |
| **Medium** | 16 | 冗長コード 2 件、リファクタ候補 6 件、バグ可能性 8 件 |
| **Low** | 8 | 冗長コード 2 件、デッドコード 1 件、リファクタ候補 1 件、バグ可能性 4 件 |

#### 最優先対応 (Critical/High 優先、Medium から 1 件補足)

1. `internal/agent/message_deliverer.go:147-182` — confirmSubmittedOrRetry に SubmitProbeResult を導入
2. `internal/daemon/queue_scan_phase_c.go:22-50` — Load エラー時は early return し、空キュー扱いでの後続反映を止める
3. `internal/daemon/worktree/recover_resume.go:129-193` — リカバリ err の無視を解消
4. `internal/daemon/worktree/recover_resume.go:116-143` — ResumeMerge の partial state / saveState 漏れを解消
5. `internal/agent/process_manager.go:84-143` — ensureWorkingDir の再同期不全リスクを低減 (Medium)

### エビデンス注記

- 全項目は Read/Grep に基づく**参照済み**レベル。実行検証 (panic 再現等) は未実施
- デッドコード判定 (2.1) は Grep 逆引きで呼び出し元 0 件確認済み。ただしリフレクション/インターフェース経由の呼び出しは網羅できていない (**推定**: 直接呼び出しはなし)
- 大型ファイル行数 (3.1) は wc 由来でなく Explore 報告値の概算 (`±10 行` 程度の誤差を含みうる)
- 第 1〜3 Explore 間で重複検出された項目は上位深刻度を採用 (例: 4.13 と 4.8 は別観点だが類似領域)
- **永続化制約**: タスクが指定する `.maestro/results/review_part_code_quality.md` への書き込みは Worker policy hook により拒否されるため、本ファイルは `tmp/sections/review_part_code_quality.md` に退避保存している

---

## (5) その他の所見・未実施項目

本統合レポートは 4 重点項目を対象とした 3 タスクの成果物に基づく。以下の領域はカバーされていない:

- **テスト品質レビュー** (`internal/**/*_test.go` および `cmd/maestro/**/*_test.go`):
  本レビューでは `*_test.go` を意図的に除外している (`Go コード品質` セクション冒頭参照)。前段コマンドで取り扱う想定があったが、当該タスクはユーザー側で cancel されたため**未実施**。
  - 影響範囲: テストの網羅性 (テーブル駆動テスト導入、エッジケース追加候補、helper の DRY 化、`*_test.go` 内に残る `// TODO(DRY)` 2 件 — L-5a / L-5b で言及済み) の評価が欠落。
  - 必要に応じて `go test ./internal/... -count=1` の実行と coverage 計測も別タスクとして発行することを推奨。

- **ユーザー要件充足の独立調査**:
  本統合レポートは 4 重点項目（コードコメント / Agent 責務 / 設計整合性 / Go コード品質）に対する深掘りであり、「Maestro が当初要件を満たすに至っているか」という上位視点の充足検証は別軸の調査領域である。本コマンド範囲では**未実施**。
  - 必要であれば、要件定義 (例: `templates/maestro.md`、`AGENTS.md`、`README.md` 等) と現実装の trace matrix を作成する別コマンドを発行することを推奨。

- **追加調査の発行先候補**:
  - `cmd/maestro/**/*_test.go` も含めたテスト品質レビュー
  - 動的検証 (`go vet ./...` / `go test ./internal/... -race -count=1` 実行) の実施と結果集約
  - `templates/instructions/planner.md` (1193 行) の節分割提案 (本レポート (2) 6-1 と連動)
  - `internal/agent/worker_policy_hook.sh` の `git add -A`、`bash <(curl ...)`、`git restore .` への正規表現追補 (本レポート (3) V2/V3 で Low 指摘済み)

---

## (6) 統計サマリ (再掲)

| 重点項目 | Critical | High | Medium | Low | 計 |
|---------|---------:|-----:|-------:|----:|---:|
| (1) コードコメント | 0 | 7 | 3 | 6 | 16 |
| (2) Agent 責務 | 0 | 3 | 6 | 4 | 13 |
| (3) 設計整合性 | 0 | 0 | 1 | 6 | 7 |
| (4) Go コード品質 | 1 | 3 | 16 | 8 | 28 |
| **計** | **1** | **13** | **26** | **24** | **64** |

### 最優先対応 (Critical / High 抽出)

| 出典 | ID | 内容 |
|------|----|------|
| (4) | 4.2 | `internal/agent/message_deliverer.go:147-182` `confirmSubmittedOrRetry` の silent failure (Critical) |
| (4) | 4.1 | `internal/daemon/queue_scan_phase_c.go:22-50` Load エラー時の zero 値続行 → 空キュー扱いの状態反映リスク (High) |
| (1) | H-1〜H-7 | godoc / コメントの日英混在 7 件 (`quality_gate.go`, `queue_scan_helpers.go` x2, `queue_scan_phase_a_worktree_stall.go`, `lock_order_enabled.go`, `model/state.go`, `envelope/envelope.go`) |
| (2) | 1-1, 4-1, 6-1 | Planner 進行管理ロジックが指令書ナラティブ依存 / recovery 分岐爆発 / `planner.md` 1193 行肥大 — `maestro plan recover` 集約 CLI と planner.md 構造分割でまとめて改善可能 |
| (4) | 4.6 | `internal/daemon/worktree/recover_resume.go:129-193` リカバリ err 無視 |
| (4) | 4.7 | `internal/daemon/worktree/recover_resume.go:116-143` `ResumeMerge` partial state / saveState 漏れ |

### 整合度総評

- 設計整合性 (V1〜V5) においては Critical / High 0 件。設計意図はおおむね実装と指令書の双方で技術的に強制されている。改善余地は文書整備 (Low 6 件) と `commit_failed` 通知書式の文書統一 (Medium 1 件) に集中。
- Go コード品質 (Critical 1 / High 3) と Agent 責務の Planner 集中肥大 (High 3) が最優先対応すべき領域。
- コードコメント品質は High 7 / Medium 3 と件数は多いが、いずれも godoc / コメントの言語ポリシー統一で機械的に解消可能であり、低リスク改善としてまとめて着手しやすい。

---

備考: 本ファイルは 3 タスクの成果物統合により生成された。各セクションの個別ソースは `tmp/reports/final_review_comments.md` (1)、`tmp/reports/final_review_agent_responsibility.md` (2)、過去 attempt の `tmp/sections/` 系および中間 v1 の `tmp/reports/final_review.md` (3)/(4) を参照のこと。
