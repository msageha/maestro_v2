# (1) 重点項目: コードコメント品質

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

---

## 修正対応メモ

2026-04-27 の修正で H-1〜H-7、M-1〜M-3、L-1/L-2/L-3/L-4/L-5 を局所修正済み。Agent 向け日本語エンベロープ文言は出力仕様として残し、`EnvelopeNoneLabel` に集約した。

---

## High (言語混在等)

### H-1. internal/daemon/quality_gate.go:17-18

- **ファイル:行番号**: `internal/daemon/quality_gate.go:17-18`
- **深刻度**: High
- **現状コメント抜粋**:
  ```go
  qualityGateEventBufferSize = 100                    // eventChan のバッファサイズ
  defaultEvaluationTimeout   = 100 * time.Millisecond // ゲート評価のタイムアウト
  ```
- **問題**: 同一ファイル内で godoc は英語 (`// QualityGateEvent represents an event ...` :21 ほか多数) で書かれている一方、定数の line-end コメントだけ日本語混在。読者が「英語 godoc と日本語インラインコメントのどちらが正なのか」を毎回切り替えながら読む必要があり、可読性とメンテナンス時の翻訳判断が分散する。
- **推奨対応**: 周辺 godoc が英語のため、line-end コメントも英語へ統一。例: `// buffer size for eventChan` / `// timeout for gate evaluation`。

### H-2. internal/daemon/queue_scan_helpers.go:231-233

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

### H-3. internal/daemon/queue_scan_helpers.go:535-539

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

### H-4. internal/daemon/queue_scan_phase_a_worktree_stall.go:53-57

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

### H-5. internal/lock/lock_order_enabled.go:160-182

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

### H-6. internal/model/state.go:36-70

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

### H-7. internal/envelope/envelope.go:256, 311, 325

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

---

## Medium (過剰ドキュメンテーション)

### M-1. internal/daemon/quality_gate.go の EventType / Timestamp 6 メソッド

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

### M-2. internal/model/fitness.go の IsFailed (`fitness.go:40-41`)

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

### M-3. internal/model パッケージ全般 (`state.go`, `fitness.go` を含む)

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

---

## Low (軽微・TODO 残存・UI 文字列等)

### L-1. internal/envelope/envelope.go の UI 文字列 "なし"

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

### L-2. internal/agent/launcher.go の MAINTENANCE INVARIANT

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

### L-3. internal/daemon/worktree/resolver.go のテンプレ参照

- **ファイル:行番号**: 未検証
- **深刻度**: Low
- **現状コメント抜粋**: 該当なし
- **問題**: 前回 Worker は `resolver.go` に「テンプレ参照」が残っていると指摘していたが、現時点で `internal/daemon/worktree/resolver.go:1-80` を Read した範囲では「コピー&ペーストの残骸」「テンプレート的な雛形参照」に該当する箇所は確認できなかった。`SignalStore` インターフェース :55-77 や `errSignalStoreUnavailable` :49-53 の godoc は具体的な契約を述べており、テンプレ参照には当たらない。指摘箇所の特定不能のため**未検証**として記録する。
- **推奨対応**: 前回コンテキストの「テンプレ参照」が指していた具体行を再特定するタスクを別途立てるか、本項目は「該当なし (検証時点で解消、または別ファイルが想定されていた可能性)」として close。

### L-4. internal/model/fitness.go:147 の曖昧コメント

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

### L-5. TODO/FIXME 残存箇所

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

---

## 統計サマリ

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
