# (2) 重点項目: Agent への過剰な責務

調査範囲: `templates/instructions/{orchestrator,planner,worker}.md`、`templates/maestro.md`、`templates/persona/*.md`、`internal/agent/{launcher.go,policy_checker.go}`、`internal/daemon/lease/manager.go`、`cmd/maestro/cmd_*.go`。

注: タスク本文では `common.md` を調査対象に挙げているが、`templates/instructions/common.md` は存在せず、Agent 共通プロンプトの SSOT は `templates/maestro.md`（114 行）である（`ls templates/instructions/` で確認済 → orchestrator.md / planner.md / worker.md のみ）。本レビューでは `templates/maestro.md` を共通指令書として扱う。

ID 採番、lease_epoch 管理、`.maestro/` 直接書込みの 3 点については「Agent → Daemon 委譲」が現状でも徹底されており Critical 級は無い。論点は (a) Planner の指令書肥大と分岐爆発、(b) role 間でのツール制約非対称、(c) Daemon 側ハードリミット欠如をプランナーの文章規約で代替している箇所、の 3 系統に集約される。

---

## 観点 1: 状態管理

### 1-1. Planner が抱える進行管理ロジックがほぼ全て指令書ナラティブで強制されている [High]

- **該当箇所**: `templates/instructions/planner.md` 全体（1193 行）。特に `## 検証フェーズ` `## Verification Loop Cap` `## Conflict Recovery` `## Publish Conflict Recovery`。
- **現状**: 「verification は最大 2 ラウンド」「同種失敗 3 連続でフェーズ中断」「conflict 発生時は resolve-conflict → resume-merge を順に呼ぶ」等の不変条件が、Daemon 側のハードガードでは無く Planner の文章規約として記述されている。
- **問題**: Planner LLM が指令書を誤読・忘却した場合、誰も止められない。Tail of Bell-curve（リトライ無限ループ、未解決 conflict の上に新タスク投入、quarantined 状態を見落とした unquarantine 提案）は理論上発生しうる。CLI 受付時点では止まらない。
- **委譲案**: Daemon の `internal/daemon/scheduler` または `internal/daemon/queue` 層に以下のハードガードを追加する。
  - `verification` フェーズの round counter を Phase メタに持たせ、`max_verification_rounds` 超過時は `add-retry-task` をエラー応答（新規 CLI エラーコード）。
  - `quarantined` 状態の Plan には `add-task` / `add-retry-task` / `resume-merge` 系を一律拒否し、`unquarantine` のみ受付（既に `unquarantine` は operator gate で守られているため、その手前を補強）。
  - 同一 task_id の連続失敗回数を Daemon 側カウンタで保持し、CLI 経由の再投入時に閾値ガード。

### 1-2. Worker の `partial_changes` フラグ運用が文章規約のみ [Medium]

- **該当箇所**: `templates/instructions/worker.md` の「`--partial-changes` を指定する場合」の節、`cmd/maestro/cmd_result.go`。
- **現状**: 部分変更が残っているかは Worker 自身の自己申告であり、CLI 側で git status 等を検証する仕組みは無い。
- **問題**: Worker が忘却・誤判定した場合、Daemon の retry / merge 判断が誤った前提で進む。worktree モードでは Daemon が直接 commit するため軽減されるが、非 worktree モード時の影響が残る。
- **委譲案**: 非 worktree モード時、`maestro result write` 受信時に Daemon が `git status --porcelain` を実行し `partial_changes` を上書き決定する（Worker の申告は hint 扱いに格下げ）。

### 1-3. ペルソナ / スキル注入の整合確認が Planner ナラティブ [Low]

- **該当箇所**: `templates/instructions/planner.md` の `persona_hint` / `skill_refs` 説明節。
- **現状**: 「persona/skill が存在しない名前を指定すると無視される」という挙動が文章で説明されているが、Planner からの誤指定を CLI 側で警告する仕組みは無い。
- **問題**: タイポで永続的に persona/skill が空注入され続けても気付けない。
- **委譲案**: `maestro plan add-task` 受付時、未知の `persona_hint` / `skill_refs` は CLI から警告 (stderr) または `unknown_persona_hint` エラーで弾く。

---

## 観点 2: ID 生成

### 2-1. ID 自己生成は完全排除済み [該当なし]

- **該当箇所**: `templates/maestro.md` (Agent 共通禁止), `cmd/maestro/cmd_plan.go` `cmd_plan_tasks.go` `cmd_queue.go` `cmd_result.go`。
- **現状**: 全 ID（plan_id / command_id / task_id）は Daemon 側で採番され CLI stdout で返却される。Agent 指令書側には ID を生成・推測する記述は見当たらない。`maestro queue write --type task` は Planner ルート以外を拒否（`cmd_queue.go:147`）。
- **問題**: なし。
- **委譲案**: 不要。

---

## 観点 3: lease / fencing

### 3-1. lease_epoch 自己更新は完全排除済み [該当なし]

- **該当箇所**: `internal/daemon/lease/manager.go:138`（唯一のインクリメント点 `*ref.leaseEpoch++` in `acquireLease`）。`releaseLease` `extendLeaseExpiry` は epoch 不変。`templates/instructions/worker.md` の lease_epoch ライフサイクル節。
- **現状**: Worker は配信値を pass-through するのみで、CLI 側 `--lease-epoch` のデフォルト `-1` センチネルにより明示指定を強制している（`cmd_result.go:39 cmd_task.go`）。Daemon は `task_heartbeat` / `result_write` で厳密一致比較 → `FENCING_REJECT`。worker.md にも「Agent 側で生成・更新しない」と明記。
- **問題**: なし。設計と実装と指令書が一致している。
- **委譲案**: 不要。ただし観点 7-2 の改善案（fencing reject 応答に current_epoch を載せる拡張）は指令書末尾に「将来候補」として既に明記されており、必要時に検討で可。

---

## 観点 4: 複雑なロジック

### 4-1. Planner 指令書の分岐爆発（恢復フローのバリエーションが指令書側で展開されている） [High]

- **該当箇所**: `templates/instructions/planner.md` の `## Conflict Recovery` `## Publish Conflict Recovery` `## merge_conflict / publish_conflict / publish_quarantined / commit_failed handling`。
- **現状**: Plan の状態遷移（`merge_conflict`, `conflict_resolution`, `publish_conflict`, `publish_quarantined`, `commit_failed`, `quarantined` 等）ごとに Planner が呼ぶべき CLI シーケンス（`resolve-conflict` → `resume-merge`、`retry-publish`、`unquarantine`（operator only）等）を文章で網羅している。同一の状態でも条件分岐（worktree 有無、変更ファイル件数、commit_policy 違反種別）で異なる手順が要求される。
- **問題**: 状態 × 条件のマトリクスが Planner の自然言語推論にすべて任されている。新しい状態が増えるたびに指令書が膨張し、忘却・誤適用のリスクが線形に増える。
- **委譲案**: Daemon に `maestro plan recover --plan-id <id>` 系の単一エントリを実装し、内部で状態判定 → 必要 CLI 呼び出しを連鎖実行する（=「recovery を CLI 1 個に集約」）。Planner は「recover を呼ぶ」だけになり、指令書から状態遷移マトリクスを削れる。
  - 短期的妥協案として、planner.md の該当節を `cmd/maestro/cmd_plan_ops.go` 側コメントの参照リンクに置換するだけでも肥大は抑えられる。

### 4-2. Verification phase の重複定義禁止規則が指令書ローカル [Medium]

- **該当箇所**: `templates/instructions/planner.md` の verification phase 章（「concrete に verification 命令を含めてはならない」「`__system_verify` は Daemon 自動投入」相当の記述）。
- **現状**: フェーズ分離規則（concrete / deferred の禁止コマンド、`__system_*` タスクの自動投入）は Planner ナラティブで規律化されている。CLI 側は受付時に該当フェーズの content を全文舐める検証を行っていない。
- **問題**: Planner が誤って concrete に `go test ./...` を入れたとき、初手で気付けない。verification 二重実行や成功条件のすり替えが起きうる。
- **委譲案**: `maestro plan add-task --phase concrete` 受付時、`content` に `go test` / `npm test` / `pytest` 等のキーワード（または config.yaml のフルチェックコマンド）が含まれていれば warning または reject。

### 4-3. Five Questions Analysis / Bloom's Taxonomy 等のメタ分析手順 [Low]

- **該当箇所**: `templates/instructions/planner.md` の Five Questions Analysis、Bloom's Taxonomy、phase 分割判定節。
- **現状**: 「タスク分解前にこの 5 観点を分析する」「認知レベルを Bloom 分類で判定する」等の手続きが規定されている。
- **問題**: これらは思考の質を高めるためのフレームワークであり Daemon に委譲すべき性質では無いが、文章量として大きく Planner が読み飛ばす可能性がある。
- **委譲案**: Daemon 委譲は不要。指令書側で「分析フレームワーク」セクションとして独立ファイル化し、`planner.md` 本体からは要約のみリンク参照に変更する選択肢のみ示す（責務移譲ではなく構造改善）。

---

## 観点 5: CLI ラップの抜け（.maestro/ 直接編集の指令書ヒント）

### 5-1. .maestro/ 直接編集の指令は無し [該当なし、ただし注意点 1 件]

- **該当箇所**: `templates/maestro.md` § Agent の責務原則、`templates/instructions/worker.md` § `.maestro/` アクセス制御、L1/L2 enforcement 節。
- **現状**: 全 role の指令書で「`.maestro/` 以下の YAML 直接書き込み禁止」「全状態変更は CLI 経由」が冒頭で宣言されている。Worker は L1（disallowedTools の `Read(.maestro/{state,queue,results,locks,logs}/**)` `Read(.maestro/{config.yaml,dashboard.md})`）と L2（Bash/Write/Edit hook）の二重で技術的にもブロック済み。
- **問題**: なし。
- **注意点**: `templates/instructions/worker.md` で `.maestro/worktrees/` 配下のソースコードは「読み書き可能（管理ファイルの直接操作は禁止）」と書かれているが、`.git/` config 等の管理ファイルが Bash 経由で書かれた場合 L2 hook がカバーしているかは追加検証推奨（後述 7-3 と関連）。

### 5-2. `git add -A` / `git add .` 禁止が文章規約 [Low]

- **該当箇所**: `templates/instructions/worker.md` § Commit Task Protocol。
- **現状**: 「`git add -A` / `git add .` は使用禁止」「`.maestro/` 以下はステージングしない」という規律は文章のみ。L2 hook が `git add -A` を直接ブロックしているか不明（hook script 本体未確認）。
- **問題**: 規律違反が技術的に検出されない可能性。
- **委譲案**: L2 worker policy hook に `git add -A|--all|\.` 検出と reject を追加する（既にカバーされていれば不要）。worktree モード時は `__system_commit` が配信されないため影響は限定。

---

## 観点 6: 指令書の肥大化・重複・古さ

### 6-1. `templates/instructions/planner.md` が 1193 行と突出して大きい [High]

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

### 6-2. shell quoting / heredoc の注意書きが Planner に蓄積 [Medium]

- **該当箇所**: `templates/instructions/planner.md` の shell quoting 章および `--content` 渡し時の注意。
- **現状**: 「`--content` には heredoc を使う」「シングルクォート内のシングルクォートは `'\''` でエスケープ」等、CLI 起因のハマり所が指令書に列挙されている。これは CLI 設計の仕様（`--content` を string 引数で受ける）の弱さを Planner ナラティブで補っている。
- **問題**: Planner の認知負荷が高い。Daemon/CLI 改善で根治できる種類の負債である。
- **委譲案**: `maestro plan add-task` に `--content-file <path>` または `--content-stdin`（`-` 値）を追加し、Planner には「長文は --content-file に書き出して渡す」とだけ指示する。shell quoting 節は大幅に簡略化できる。

### 6-3. Tier1/Tier2 SSOT 化の取り扱いは適切 [該当なし]

- **該当箇所**: `templates/maestro.md` § 破壊的操作の安全規則（D001-D008）。
- **現状**: maestro.md が SSOT、worker.md は Worker 固有規則と参照のみ。重複していない。
- **問題**: なし。
- **委譲案**: 不要。

### 6-4. Worker `.maestro/` 制御プレーン表が maestro.md と worker.md の双方に存在 [Low]

- **該当箇所**: `templates/maestro.md` Agent 責務節と `templates/instructions/worker.md` `.maestro/` アクセス制御節。
- **現状**: Worker からのアクセス可否が両方で表化されており、ほぼ同内容を 2 度記述している。
- **問題**: 軽微な不整合（worker.md 側のみ `worktrees/**` が「読み書き可能だが管理ファイル直接操作禁止」と詳細記述）が生じうる。
- **委譲案**: maestro.md の表は role 共通の上位規則のみに残し、Worker 固有の `worktrees/**` 規律は worker.md に集約。表の重複を解消する。

---

## 観点 7: ツール制約・ホスト依存性の対称性

### 7-1. role 間のツール制約は技術的に強制されている [該当なし、ただし注意点あり]

- **該当箇所**: `internal/agent/launcher.go` の `allowedToolsByRole` (lines 51-73), `workerDisallowedTools` (lines 334-356), `appendDisallowedTools` (lines 320-329)、`internal/agent/policy_checker.go:178-199` `Bash|Write|Edit` matcher。
- **現状**:
  - Orchestrator: `Bash(maestro plan submit:*)` `Bash(maestro plan complete:*)` `Bash(maestro skill list:*)` のみ許可、Read 不要。
  - Planner: `Bash(maestro:*)` + `Read(.maestro/**)` 許可、加えて `Bash(maestro plan unquarantine:*)` を `appendDisallowedTools` で拒否。
  - Worker: allow-list 空（unrestricted）、`workerDisallowedTools` で tmux kill, `.maestro/` 制御プレーン Read, plan ops 系をブロック。L2 hook で動的判定。
- **問題**: なし。設計通り。
- **注意点**: 観点 7-2/7-3 を参照。

### 7-2. Planner と Worker の制約レイヤーが非対称 [Medium]

- **該当箇所**: `internal/agent/launcher.go` `allowedToolsByRole`、`policy_checker.go` `HookSettings`。
- **現状**: Worker は L1（`--disallowedTools`）+ L2（PreToolUse hook）の二層。Planner / Orchestrator は L1 (`--allowedTools` allow-list) のみで L2 hook は配線されていない。
- **問題**:
  - Planner の `Bash(maestro:*)` 配下に将来追加された破壊的サブコマンド（仮に `maestro daemon shutdown` のような）が自動的に Planner に届いてしまう。allow-list の粒度が `maestro:*` で粗いため、新規 CLI 追加時の安全性は CLI 側設計者の規律に依存している。
  - Planner の `Read(.maestro/**)` は `state/`, `queues/`, `results/`, `locks/` 等の制御プレーンを含む全領域に及ぶ。worker.md 観点では制御プレーン Read は禁止であり、role 間で原則が非対称。
- **委譲案**:
  - Planner にも L2 hook（軽量版）を配線し、新規 `maestro` サブコマンド追加時のホワイトリスト管理を hook 側に集約。または `--allowedTools` をサブコマンド粒度に分解（`Bash(maestro plan add-task:*)` 等）。
  - Planner の `Read(.maestro/**)` を実利用範囲（おそらく `state/plans/<id>.yaml` `queues/plan_queue.yaml` 等）に絞る。少なくとも `locks/` と `logs/raw/` は除外可能。

### 7-3. macOS 固有保護が Worker 文章規約のみ [Medium]

- **該当箇所**: `templates/instructions/worker.md` § Worker 固有規則 § macOS 固有保護。
- **現状**: 「`/System/`, `/Library/`, `/Applications/`, `/Users/`（プロジェクトツリー外）の削除・再帰変更を禁止」「`rm` 実行前に `realpath` 検証」が文章で要求される。L2 hook（`internal/agent/worker_policy_hook.sh` 想定）が実装しているかは hook script 本体未確認。
- **問題**: ホスト依存規律（macOS パス）が hook で実装されていない場合、Linux ホストでは無効、Worker LLM が誤って rm したときに止められない。
- **委譲案**: L2 hook 内で OS 判定（`uname` または env）し、macOS 時のみ `/System|/Library|/Applications|/Users/` の write/delete をブロック。Linux 用にも `/etc|/usr|/var` 系の同等規律を追加する。

### 7-4. Worker の `git push` 全面禁止は文章規約のみ [Medium]

- **該当箇所**: `templates/instructions/worker.md` § Worker 固有規則「`git push` 全面禁止」。
- **現状**: maestro.md Tier3 の `--force-with-lease` 代替も Worker には適用外と明記。だが L2 hook で `git push` を全パターン reject しているかは未確認。
- **問題**: Worker LLM が忘却した場合、リモートに到達するリスク。
- **委譲案**: L2 hook で Worker の `git push` を Bash パターンマッチで一律 reject する（既に実装されていれば確認のみ）。

---

## 統計サマリ

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
