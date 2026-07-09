# 観点C 監査レポート: Agent 責務の過剰流出

**監査対象**: maestro_v2 の Agent 指令書 (`templates/instructions/*.md`, `templates/persona/*.md`, `templates/skills/{role,share}/*/SKILL.md`) と daemon 実装 (`internal/daemon/`, `internal/agent/`) の責務境界。

**監査スタンス**: 修正は行わない (Read のみ)。命名・配置・description からの推測は事実扱いせず、コード定義 / 行番号で根拠を閉じる (`source-grounded-response`)。

**日付**: 2026-05-08

---

## 0. サマリ判定 (5 項目別)

| ID | 観点                                                                             | 判定                                             |
| -- | -------------------------------------------------------------------------------- | ------------------------------------------------ |
| C1 | instructions が Agent に状態管理 / ID 採番 / lease_epoch 管理 を要求していないか | **問題あり** (4 件)                              |
| C2 | Worker / Planner prompt に daemon 領域作業が紛れ込んでいないか                   | **問題あり** (4 件)                              |
| C3 | Worker self-verify / commit / publish のうち daemon 肩代わり可能領域             | **問題あり** (3 件)                              |
| C4 | retry / cancel / quarantine / paused_for_replan の遷移を Agent が判断する余地    | **問題あり** (4 件)                              |
| C5 | L1 / L2 二層防御が実コードで実装されているか、文書だけの規約か                   | **要確認 (重大なドリフト 2 件 + 部分実装 1 件)** |

合計 finding 数: **18 件** (要件 10 件以上を満たす)。

---

## 1. 重大度凡例

| Severity     | 意味                                                         |
| ------------ | ------------------------------------------------------------ |
| **critical** | 文書と実装が乖離して安全性 / 状態整合の前提が崩壊しうる      |
| **major**    | 責務境界の混乱で運用ミス / 重複作業 / LLM トークン浪費を誘発 |
| **minor**    | 死語化した記述・冗長な指示。LLM の判断は壊さないが noise     |
| **nit**      | 表現上の改善余地                                             |

---

## 2. C1 — 状態管理 / ID 採番 / lease_epoch 管理の流出

### C1-1 [major] worker.md が lease_epoch ライフサイクル全体を Worker に説明している

- **対象指令**: `templates/instructions/worker.md:296-339` (§lease_epoch ライフサイクル) と `:309-339` (§Worker 側の epoch 失効検知)
- **対象実装**: `internal/daemon/lease/manager.go:124-168` (`acquireLease` / `releaseLease`), `:138` (`(*ref.leaseEpoch)++`), `:191-194` (`AcquireTaskLease`), `:206-209` (`ReleaseTaskLease`)
- **責務漏出方向**: daemon → Agent (内部メカニズムを Agent prompt に持ち込み)
- **観察**:
  - daemon は `acquireLease` 1 回ごとに `(*ref.leaseEpoch)++` し、`releaseLease` / `extendLeaseExpiry` では epoch を変更しない (`manager.go:138` / `manager.go:182`)。Agent はこれを生成・更新せず単に「配信値を CLI に転送する」だけで完結する (worker.md:298 で本人もそう明記)。
  - にもかかわらず worker.md は採番主体・インクリメント条件・不変な操作・失効条件・検証点・不一致時の応答 を表形式で全て公開しており (worker.md:300-307)、Agent には不要な daemon 内部仕様。
- **問題**: Agent prompt が daemon の内部状態機械を記述しているため、リファクタや F-019 段階更新の度に worker.md と manager.go が同期しなければならない (drift 源)。直近の F-019 段階移行 (`worker.md:284 / 311 / 339`) はまさにこの drift を生んだ。
- **分類**: 即修正候補 (記述の大幅縮約)
- **根拠チェーン**:
  - `manager.go:135-148`:
    ```go
    *ref.leaseExpiresAt = ...
    (*ref.leaseEpoch)++
    *ref.status = model.StatusInProgress
    ```
  - worker.md:296-308 の「採番主体 / インクリメント条件 / 不変な操作」表は同コードの反転記述。

### C1-2 [major] worker.md が exit code 10/11/12 のシェル case 分岐を Worker に書かせている

- **対象指令**: `templates/instructions/worker.md:286-294` (exit code 表), `:311-336` (case 分岐の擬似シェル例)
- **対象実装**: 終了コードを返す側は daemon API ハンドラ (`internal/daemon/task_heartbeat_handler.go`, `internal/daemon/result_write_handler.go`, および UDS error → exit code マッピングを行う `cmd/maestro` 配下の CLI)
- **責務漏出方向**: daemon → Agent
- **観察**:
  - worker.md:325-333 は `case "$ec" in 0|10|11|12|2|*` という具体的なシェルテンプレートを Agent に書かせる。`fencing_epoch_mismatch` / `max_runtime_exceeded` / `fencing_status_mismatch` という daemon 内部語彙を Agent に強制している。
  - 実態として fencing 系 reject は Worker 側のバグではなく「正常系」(`worker.md:337-339` で本人もそう書いている) で、Agent は単にターン終了すれば良い。case 分岐を強制している分だけ LLM の解釈余地が増えてミス (壊れたシェル / pipe 越しの `$?` 失効) を誘発する。
- **問題**: 「失敗した CLI を観測したら黙ってターン終了」という単一の行動指針で十分なところに、daemon-internal exit code を 5 種類列挙して LLM にコード生成させており、責務が daemon → Agent に流出。
- **分類**: 設計議論が必要 (Agent の観測点を `非0 → 終了` に集約するか、daemon 側で fencing 詳細を隠す)
- **根拠**: worker.md:284-291 / 313-320 の二重テーブル + `manager.go` 上で epoch 比較は daemon 側 1 箇所に閉じている。

### C1-3 [major] worker.md の `task heartbeat` は daemon の busy detection と二重実装

- **対象指令**: `templates/instructions/worker.md:266-294` (§Task Heartbeat)
- **対象実装**:
  - `internal/agent/busy_detector.go:21-32`: 3-stage busy detection (`pane_current_command` / pattern hint / activity probe)
  - `internal/daemon/task_heartbeat_handler.go:22-77`: heartbeat 受信 → lease 延長
  - `internal/daemon/queue_scan_collect.go` + `queue_scan_apply.go`: busy / undecided pane を観測してリースを延長 (worker.md:305 にも明記済み)
- **責務漏出方向**: daemon → Agent (daemon が既に観測している pane 活性を Agent に手動 ping させる)
- **観察**:
  - busy_detector はタスク実行中の pane 状態を能動観測し、busy → リース延長 / non-busy → release で TTL を制御する。
  - 一方 worker.md は「長時間タスクの実行中にリースを延長するための heartbeat コマンド」と称して `maestro task heartbeat` 呼出を Worker に要求。
  - Worker LLM が heartbeat を呼ぶ呼ばないに関わらず、daemon は busy 検知でリースを延長できる (busy_detector は SubAgent / SubCommand 実行中も pane 活性を捉える)。
- **問題**: Worker 側に "should I send heartbeat?" の判断負荷が乗り、忘れると lease 失効、過剰だと UDS roundtrip 増。本来 daemon の busy 検出に統一できる責務。
- **分類**: 設計議論が必要 (heartbeat を opt-in とするか、busy 検出に一本化するか)
- **根拠**: `busy_detector.go:50-57` (DetectBusy 一回分)、`task_heartbeat_handler.go:22-58` (handler 本体)。

### C1-4 [minor] planner.md が definition_of_abort のデフォルト値を LLM に毎回書かせる

- **対象指令**: `templates/instructions/planner.md:269-271` (max_repair_count=3 / max_wall_clock_sec=1800), `:1294-1296` (タスク雛形), `:1311-1312` (定義表)
- **対象実装**: `internal/model` の `DefaultDefinitionOfAbort()` (planner.md:270 / submit_parse.go:104 で明示的に呼び出し済み)
- **責務漏出方向**: daemon → Agent
- **観察**:
  - daemon は `add-retry-task` / `add-task` で `--max-repair-count` 等が省略されると `model.DefaultDefinitionOfAbort()` を適用 (planner.md:270 / submit_parse.go:104)。
  - しかし `plan submit` の YAML スキーマでは `definition_of_abort` を必須化し、毎タスク `max_repair_count: 3, max_wall_clock_sec: 1800` の同一値を Planner LLM に書かせている (planner.md:1294-1296)。
- **問題**: 9 割のタスクで同じ値を書くだけの形式的フィールドを LLM に毎回生成させる = トークン浪費 + コピペミス源。daemon 側で空欄なら default を補えば良い。
- **分類**: 即修正候補 (YAML 必須を任意化、daemon が補完)
- **根拠**: `submit_parse.go:104` の `doa := model.DefaultDefinitionOfAbort()` が既に default 値を内蔵。

---

## 3. C2 — daemon 領域作業が Agent prompt に紛れ込んでいる

### C2-1 [critical] worker.md の `--summary` vs `--summary-file` 教示は撤去済みハック前提で残存

- **対象指令**: `templates/instructions/worker.md:223-260` (§`--summary` と `--summary-file` の使い分け)
- **対象実装**: `internal/agent/worker_policy_hook.sh:5-17` (再設計コメント), `:90-171` (Bash 検査本体)
- **責務漏出方向**: daemon (旧仕様) → Agent (誤った workaround を促進)
- **観察**:
  - worker.md:225 は「PreToolUse policy hook は Bash コマンド全体に対して `rm -rf /Users/...` のようなパターンを正規表現で検索するため、`--summary` 内に `rm`、`/Users`、`-rf` を含めると D001 で誤検知される」と書く。
  - しかし `worker_policy_hook.sh:5-17` は「Scope (2026-04-30 redesign): only enforce maestro orchestration-model constraints. Generic destructive-command defense (rm -rf, sudo, kill, base64 decode, eval, etc.) is the responsibility of the user's global Claude Code hooks ... and is intentionally NOT duplicated here」と明示。
  - 実コードを確認しても `:90-171` で `rm` / `/Users` / `-rf` substring 検査は **存在しない**。検査されているのは maestro CLI 制御プレーン subcommand と `.maestro/` redirect、git mutation、RUN_ON_MAIN モードのみ。
- **問題**: Agent は存在しない誤検知を回避する目的で長文 summary を必ず file 経由で書こうとする。daemon 側はもうそれを要求しない。`--summary` の "重要" 注釈は **過去の bug を documenting しただけの死文** で、Agent 行動を歪める。
- **分類**: 即修正候補 (ハック前提のまま残ると LLM が file 経由を強要して別の事故を起こす)
- **根拠**: `worker_policy_hook.sh:7-13` の「intentionally NOT duplicated here」コメントが SSOT。

### C2-2 [major] worker.md が `--files-changed` 算出を Worker に強制 (daemon 側は metadata 扱い)

- **対象指令**: `templates/instructions/worker.md:341-361` (§`--files-changed` の正しい意味)
- **対象実装**: `internal/daemon/result_write_phase_a.go:137-152`
  > files_changed is treated as descriptive metadata only — display in the review coordinator ... and hot-file paths in diagnosis. **It is NOT a gate**, because the worker self-reports the value and the integration commit already runs through `git add -A` against the worker worktree
- **責務漏出方向**: Agent → 本来 daemon が握れる情報
- **観察**:
  - daemon の integration commit は `git add -A` で worker worktree 全体を回収する (`result_write_phase_a.go:140-145`)。Worker が files_changed を自己申告しなくても integration には影響しない。
  - 一方 worker.md:353-357 は「結果報告の直前に `git status --porcelain` を実行して出力に並ぶファイルパスのみを `--files-changed` に渡す」と Agent に diff 計算を要求。
  - worker.md:361 は「過去の事故例 — 後続の path-overlap heuristic が誤発火」を理由に注意を要求するが、daemon 側コメント (上記引用) は path-overlap を gate から外したと明記。
- **問題**: 撤去済み (gate 化されない) ロジックを根拠に Agent が毎回 `git status` を呼んで結果に self-report させているため、(1) LLM トークン浪費、(2) 算出ミスでの diagnosis ノイズ、(3) `run_on_main` 等で誤って読んだファイルを混入する事故再生産。
- **分類**: 即修正候補 (Agent から git status の責務を抜く / daemon 側で計算する)
- **根拠**: result_write_phase_a.go:137-152 が `descriptive metadata only — NOT a gate` と SSOT を提示。

### C2-3 [major] planner.md が verify.yaml の bash quoting / strict YAML decode 細部を LLM に押し付け

- **対象指令**: `templates/instructions/planner.md:96-101` (shell negation `!` を使うな等), `:98-99` (allowed categories の 6 種列挙と strict decode による reject 警告)
- **対象実装**: `internal/model` の `ParseVerifyConfigYAML` (planner.md:97 で言及される strict decode), `internal/daemon/verify_runner.go` (verify 実行)
- **責務漏出方向**: daemon → Agent
- **観察**:
  - daemon は `bash -c` で verify command を実行し YAML 1 スカラに改行不可。Planner はこの制約を熟知して書かないと strict decode で reject される。
  - planner.md:101 は「`! rg ...` は `\!` にエスケープされやすく `bash -c` で `\!: command not found` (exit 127) を踏む」という極めて daemon 内部実装由来のハマりどころを LLM に教育している。
- **問題**: shell quoting 学習は daemon 側の入力正規化 (例: 二重エスケープ検出 / category 名の正規化) で吸収できる類のもの。LLM 側では再現性のあるルール化が困難で頻繁に取りこぼす。
- **分類**: 設計議論が必要 (daemon 側のサニタイズ強化 vs Agent prompt の維持)
- **根拠**: planner.md:96-101 (記述の存在)、`internal/model/ParseVerifyConfigYAML` (strict decode の主体)。

### C2-4 [major] planner.md の `shell quoting 事故防止` 70 行は CLI 入力経路の問題を Agent に対処させている

- **対象指令**: `templates/instructions/planner.md:815-885` (§shell quoting の事故防止)
- **対象実装**: `cmd/maestro` の `plan add-task` / `plan add-retry-task` / `plan complete` の引数パーサ, `--content-file -` 受付ロジック
- **責務漏出方向**: daemon (CLI ergonomics) → Agent
- **観察**:
  - planner.md:824-832 は `--content "fix \`git log --oneline -1\`"` がバッククォートで再展開される事故を警告。
  - planner.md:836-872 は heredoc / シングルクオート / 一時ファイル経由など **4 種類** の安全テンプレートを LLM に提示。
  - planner.md:880 で「daemon は `add-task` の `purpose` / `content` / `acceptance_criteria` が極端に短い場合拒否する」と server-side defense は実装済みと記載。
- **問題**: server-side で「短すぎる入力 + 既知サニタイズ漏れパターン」の検出は既にあるなら、Planner 側で 70 行のテンプレ指示は重複。重複の代償として LLM がテンプレート選択を誤り副作用を起こすリスク。
- **分類**: 設計議論が必要 (CLI 側の入力 helper 拡充 vs prompt 維持)
- **根拠**: planner.md:885 (daemon 側 defense あり)、planner.md:816-872 (Agent 側のテンプレ列挙)。

---

## 4. C3 — Worker の commit / publish 周辺で daemon が肩代わり可能な箇所

### C3-1 [minor] worker.md の `__system_commit` 受信時保険記述は worktree モードでは到達不能

- **対象指令**: `templates/instructions/worker.md:543` 「万が一受信した場合は ... `--status completed` で報告する」
- **対象実装**: `internal/plan/submit_parse.go:78-97` (`shouldInsertSystemCommit`)
  ```go
  return !cfg.Worktree.Enabled
  ```
- **責務漏出方向**: 旧 daemon 仕様 → Agent (死コード経路の指示)
- **観察**: worktree 有効時は `__system_commit` task は **挿入されない**。Agent が受け取ることはない。worker.md:543 は意味のないハンドリングを LLM に書かせている。
- **分類**: 即修正候補 (記述削除)
- **根拠**: `submit_parse.go:96` の `return !cfg.Worktree.Enabled` が SSOT。

### C3-2 [minor] planner.md §Continuous Mode の `__system_commit` 説明も worktree モードと矛盾

- **対象指令**: `templates/instructions/planner.md:1437-1442`
  > `plan submit` 時にシステムが `__system_commit` タスクを自動挿入。Planner がコミットタスクを設計する必要はない。... **Worktree モード例外**: `worktree.enabled: true` の場合、`__system_commit` は挿入されない。
- **対象実装**: `submit_parse.go:96` (上記)
- **観察**: 標準運用 (worktree 有効) では `__system_commit` は最初から挿入されない。Planner 向け prompt が「自動挿入される」と前置きして例外として「実は挿入されない」を併記しており、実際には例外節が常時適用される。
- **分類**: 即修正候補 (worktree 標準前提に reframe)
- **根拠**: `submit_parse.go:96`。

### C3-3 [minor] worker.md の `git add` / `git commit` 禁止記述は L2 hook と二重

- **対象指令**: `templates/instructions/worker.md:485-503` (§禁止される git 操作 / publish_conflict 時の所作)
- **対象実装**: `internal/agent/worker_policy_hook.sh:122-130`
  ```sh
  if [ -n "$_wt_cwd" ] && echo "$_wt_cwd" | grep -qF '/.maestro/worktrees/'; then
    if echo "$cmd" | grep -qE '... git\s+(commit|add|merge|rebase|cherry-pick|revert|stash|restore|fetch|pull|worktree|tag)(\s|$)'; then
      deny "git mutation blocked in worktree mode (daemon owns staging, commits, merges, and publish recovery)"
  ```
- **責務漏出方向**: 文書 ↔ 実装の二重防御 (整合済み)
- **観察**: L2 hook で git mutation は技術的に拒否される。worker.md は「冗長な安全網」として記述自体は妥当 (maestro.md §冗長な安全網のスタンスと一致) だが、`git stash` / `git restore` / `git fetch` / `git pull` 等まで禁止表に並べているため、初心者 LLM が「読み取り専用作業も daemon に任せる」と誤読して `git diff` を躊躇する事例が起こりうる。
- **分類**: 設計議論が必要 (Worker に必要な許可コマンドだけ列挙する positive list 化)
- **根拠**: worker_policy_hook.sh:127 の `git\s+(commit|add|merge|...)` が hook 上の禁止集合。worker.md:489-495 の表は positive/negative list が混在。

---

## 5. C4 — retry / cancel / quarantine / paused_for_replan の遷移を Agent が判断する余地

### C4-1 [major] planner.md は「retry vs replace vs failed」を Planner が判断する設計だが daemon と二重採点

- **対象指令**: `templates/instructions/planner.md:471-477` (§失敗タスクの処理)
- **対象実装**: `internal/daemon/task_retry_handler.go:66-99` (`ShouldRetryTask`), `:79-86` (definition_of_abort 強制)
- **責務漏出方向**: daemon ↔ Agent (二重判定)
- **観察**:
  - daemon の `ShouldRetryTask` は exit code / retry_safe / definition_of_abort / max_retries で自動判定し、retry 可なら `repair_pending` → 自動補修タスク投入 (planner.md:791 で本人も明記)。
  - 同時に planner.md:805-810 は Planner に「daemon の自動 repair 投入有無を確認してから手動投入」と指示。Planner は `.maestro/dashboard.md` を読んで daemon 内部状態を観測する責務を背負う。
- **問題**: `repair_pending` 遷移は daemon 内部 state machine に閉じる方が安全。Planner が並行して `add-retry-task` を打つと race / lineage 重複の温床 (memory: `retry_one_active_per_predecessor.md` の制約はまさにこの race を防ぐパッチ)。
- **分類**: 設計議論が必要 (Planner は retry を打たず failed のみ判断、daemon が repair を一本化)
- **根拠**: `task_retry_handler.go:79-86` で daemon 側判定、planner.md:791-810 で Agent 側判定。

### C4-2 [major] planner.md §verification ループ上限 の 2 ラウンド規則は daemon 側 max_repair_count と二重

- **対象指令**: `templates/instructions/planner.md:795-803` (§ループ上限ハード制約)
- **対象実装**: `internal/daemon/task_retry_handler.go:79-86` (DefinitionOfAbort.MaxRepairCount), `:88-91` (Retry.TaskExecution.MaxRetries)
- **責務漏出方向**: daemon ↔ Agent (二重カウンタ)
- **観察**:
  - planner.md は「フェーズあたり最大 2 ラウンド (初回 + fix+re-verify)」を Agent ルーチンに課す。
  - daemon 側は per-task `definition_of_abort.max_repair_count` (デフォルト 3) を超えたら強制 abort、加えて `Retry.TaskExecution.MaxRetries` も適用 (`task_retry_handler.go:79-91`)。
- **問題**: Agent が 2 ラウンド・daemon が 3 回まで、と異なる閾値を持つため Agent が早めに plan_complete を呼んでも daemon の repair_pending が走り続ける可能性。LLM がカウントを誤ると無限ループ寸前まで投げ続ける。
- **分類**: 即修正候補 (2 ラウンド規則の撤去 or daemon 側 cap への一元化)
- **根拠**: `task_retry_handler.go:79-91`、planner.md:795-803。

### C4-3 [minor] planner.md は Planner が呼べない `unquarantine` を Planner instruction 内に列挙

- **対象指令**: `templates/instructions/planner.md:344-357` (§quarantine 解除)
- **対象実装**:
  - `internal/agent/launcher.go:695` (Planner の `--disallowedTools`):
    ```go
    case "planner":
        return append(args, "--disallowedTools", "Bash(maestro plan unquarantine:*)")
    ```
  - 同 launcher.go:721-735 の `workerDisallowedTools` 内 `maestro plan unquarantine` (Worker 側でも禁止)
- **責務漏出方向**: 文書 → Agent (使えない API を学習させる)
- **観察**: planner.md:351 は「オペレーター専用の操作であり、通常の Planner ワークフローでは使用しない」と明記しつつ、コマンド構文 (`maestro plan unquarantine --command-id <id> --reason <text>`) と使用例まで Agent prompt に書いている (planner.md:346-357)。
- **問題**: L1 で技術的に呼べない API を Agent prompt に「documentation」として残すと LLM は呼べると誤学習し、permission denied を踏む → ターン消費。本来 instruction から除外すべき。
- **分類**: 即修正候補 (記述自体を削除し、orchestrator.md §quarantine 通知のエスカレーション に集約)
- **根拠**: `launcher.go:695` (L1 で Planner の `unquarantine` 呼出を deny)。

### C4-4 [minor] orchestrator.md §Continuous Mode の Decide ステップ a/b/c/d は daemon Pre-generation gate と並走

- **対象指令**: `templates/instructions/orchestrator.md:228-258` (§Continuous Mode / Pre-generation gate)
- **対象実装**:
  - daemon Pre-generation gate: `max_iterations` / `max_consecutive_failures` / `pause_on_failure` (orchestrator.md:248-256 で本人も列挙)
  - daemon は次イテレーションを自動投入しない (orchestrator.md:194 「daemon は **次の iteration を自動投入しない**」 + memory `non_claude_workers_production.md` 等を併読)
- **責務漏出方向**: daemon ↔ Agent (連続実行制御の二重化)
- **観察**:
  - daemon は連続失敗 / 最大反復で自動停止し `paused_reason` を `.maestro/state/continuous.yaml` に記録するだけ。次コマンド生成は Orchestrator (= LLM) 担当。
  - Orchestrator は a/b/c/d の 4 カテゴリ判定 + Pre-generation gate チェックを毎ターン実施することになっている。
- **問題**: 「次のコマンド生成 = LLM 任意判断」と「停止条件 = daemon 強制」の二段階が Agent 側に判断負荷を集中させる。誤判定 (例: 失敗を成功と誤読 → 暴走) を防ぐには daemon 側の判定強化が筋。
- **分類**: 設計議論が必要 (daemon が iteration trigger を握るか、Orchestrator が握るかの再設計)
- **根拠**: orchestrator.md:194-198 (daemon 自動投入しない宣言)、orchestrator.md:228-258 (Decide / gate)。

---

## 6. C5 — L1 / L2 二層防御の実装確認

ここでは「instructions が宣言する L1/L2 の責任範囲」と「実コード」を直接照合し、**ドリフト** を抽出する。

### 6.1 L1 (`launcher.go` `--disallowedTools`) の実装確認 ✓ 部分一致

| 文書宣言 (worker.md / maestro.md)                                                 | 実装 (`launcher.go:721-780`)                                                              | 判定 |
| --------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------- | ---- |
| `tmux kill-server`, `tmux kill-session`, `tmux kill-pane`, `tmux kill-window`     | 全て列挙 (`:722-725`)                                                                     | 一致 |
| D009 復旧 API (`maestro plan unquarantine` / `resume-merge` / `resolve-conflict`) | `:728-731` 列挙 + 旧名 `maestro resolve-conflict` (`:734`)                                | 一致 |
| `.maestro/` 制御プレーン Read                                                     | `:736-742` (state/queue/results/locks/logs/config.yaml/dashboard.md)                      | 一致 |
| `.claude/.codex/.gemini` 配下 Edit/Write 全面禁止                                 | `:749-760` で `Edit/Write/MultiEdit/NotebookEdit` 4 種を 3 ディレクトリそれぞれにブロック | 一致 |

### 6.2 L2 (`worker_policy_hook.sh`) の実装確認 — 重大ドリフト発見

実コードは `internal/agent/worker_policy_hook.sh:90-303` を精査。各カテゴリの実装有無:

| 文書宣言 (`maestro.md:91-99` / `worker.md:31`)                                                 | hook 実装                                                                                                 | 判定              |
| ---------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- | ----------------- |
| Tier1 D001 (`rm -rf /`, `rm -rf ~`)                                                            | **未実装** (`:7-13` で「intentionally NOT duplicated here」と明示)                                        | **ドリフト**      |
| Tier1 D002 (プロジェクト外 rm -rf)                                                             | **未実装**                                                                                                | **ドリフト**      |
| Tier1 D003 (`git push --force`)                                                                | **部分実装**: `git push` 自体を `:135` で全 deny。`--force` 検査ではなく全否定                            | 一致 (より厳しい) |
| Tier1 D004 (`git reset --hard`, `git checkout -- .`, `git clean -f`)                           | worktree 内 (`:127`) で `restore` を deny。reset / checkout / clean は **未列挙** (worktree 外なら抜ける) | **ドリフト**      |
| Tier1 D005 (`sudo`, `su`, システム `chmod`)                                                    | **未実装**                                                                                                | **ドリフト**      |
| Tier1 D006 (`kill`, `killall`, `pkill`, tmux kill 系)                                          | tmux kill 系のみ L1 で blocked。**bash `kill`/`killall`/`pkill` は未実装**                                | **部分実装**      |
| Tier1 D007 (`mkfs`, `dd if=`, `fdisk`, `diskutil eraseDisk`)                                   | **未実装**                                                                                                | **ドリフト**      |
| Tier1 D008 (`curl                                                                              | bash`,`wget -O-                                                                                           | sh`)              |
| Tier1 D009 復旧 API                                                                            | `:97-108` で `maestro plan submit/complete/...` `queue write` `verify write` `resolve-conflict` を deny   | 一致              |
| Tier1 B001-B004 (パイプ→sh / `bash -c` / `eval`)                                               | **未実装**                                                                                                | **ドリフト**      |
| Tier2 (10 ファイル超削除 / プロジェクト外変更 / 未知 URL)                                      | **未実装** (Worker prompt 側の自律判断のみ)                                                               | **ドリフト**      |
| `.maestro/` Bash redirect                                                                      | `:140-148` で実装                                                                                         | 一致              |
| RUN_ON_MAIN 時 mutating                                                                        | `:154-171` で実装                                                                                         | 一致              |
| WT001 worktree 境界 (Write/Edit が `working_dir` 外)                                           | `:194-298` で実装                                                                                         | 一致              |
| macOS `/System/`, `/Library/`, `/Applications/`, `/usr/`, `/bin/`, `/sbin/` の Write/Edit 禁止 | **未実装** (worker.md:522-525 で hook 強制と明記しているが実体なし)                                       | **ドリフト**      |

**結論**: 文書 (`maestro.md:91-99`, `worker.md:31`, `worker.md:522-525`) は「Tier1 D001-D009, B001-B004, macOS システムディレクトリ」を L2 hook で動的拒否すると**断言**しているが、実体は worker_policy_hook.sh:5-17 のコメントどおり **maestro orchestration 制約のみ**。Tier1 の半数と Tier2 の全パターンが「文書だけの規約」状態。

### C5-1 [critical] L2 hook の責任範囲が文書と実装で大きく乖離

- **対象指令**:
  - `templates/maestro.md:91-99` (table の "L2: PreToolUse hook" 行で Tier1/Tier2 D001-D009, B001-B004 を担当と宣言)
  - `templates/instructions/worker.md:31` (「Tier1/Tier2 破壊コマンド (D001-D009, B001-B004)」を L2 が判定)
  - `templates/instructions/worker.md:522-525` (macOS `/System/` 等の Write/Edit を L2 hook で強制)
- **対象実装**: `internal/agent/worker_policy_hook.sh:5-17`
  > generic destructive-command defense (rm -rf, sudo, kill, base64 decode, eval, etc.) is the responsibility of the user's global Claude Code hooks (~/.claude/settings.json) and is intentionally NOT duplicated here.
- **責務漏出方向**: daemon (実装縮小) → 文書 (旧仕様の残滓)
- **問題**:
  - LLM (Worker) は worker.md:24-38 を読んで「rm -rf 等は daemon が技術的に拒否する」と理解し、自己判断による安全網を緩める可能性 (worker.md:36 が「冗長な安全網」と位置付けるため)。
  - 監査担当 / 運用者は maestro.md の表で「該当事項は技術防御済み」と判断し ~/.claude 側の global hook 設定を後回しにする可能性。
  - 実態は Worker LLM の自律 + ~/.claude グローバル hook の二点しか保護がない。
- **分類**: 即修正候補 (maestro.md / worker.md の table を 「L2 は maestro 制御プレーン特化、汎用破壊防御は ~/.claude」に書き換え。これは文書側の修正で吸収可能、本タスクでは禁止)
- **根拠**: worker_policy_hook.sh:5-17 (実装側の意図宣言), maestro.md:91-99 (文書宣言)。

### C5-2 [major] D006 (kill / killall / pkill) は L1 でも tmux 系のみ、bash 系プロセス kill は素通し

- **対象指令**: `maestro.md:52` "D006: `kill`, `killall`, `pkill`, `tmux kill-server`, `tmux kill-session`"
- **対象実装**:
  - `launcher.go:721-725` (L1) — tmux kill-server / kill-session / kill-pane / kill-window のみ
  - `worker_policy_hook.sh` (L2) — bash `kill` / `killall` / `pkill` 検査なし
- **責務漏出方向**: 文書 → 実装 (両層に穴)
- **問題**: maestro.md 文書では D006 の 5 パターンを Tier1 絶対禁止としているが、L1 で 4 個 / L2 で 0 個しか拒否されない。bash `kill <pid>` で daemon プロセスを落とすと「他 Agent / インフラ破壊」を踏む。
- **分類**: 設計議論が必要 (両層に kill 系を追加するか、文書を縮約するか)
- **根拠**: launcher.go:721-725 / worker_policy_hook.sh:90-171。

### C5-3 [major] `validateRunOnMainContent` は no-op なのに dispatcher が「Defense-in-depth pre-flight」とログ出力

- **対象実装**:
  - `internal/daemon/dispatch/validate_run_on_main.go:14-25`:
    ```go
    var ErrDestructiveContentRejected = errors.New("dispatch: task content rejected by run_on_main pre-flight")
    func validateRunOnMainContent(task *model.Task) error {
        _ = task
        return nil
    }
    ```
  - `internal/daemon/dispatch/dispatcher.go:271-281`:
    ```go
    // Defense-in-depth pre-flight check that rejects destructive shell
    // snippets in tasks targeting the main branch or integration worktree.
    // The Bash policy hook ... covers Claude Code workers, but
    // Codex/Gemini bypass that hook entirely; this check applies to every
    // worker regardless of agent type.
    if err := validateRunOnMainContent(task); err != nil {
        ...
    }
    ```
- **対象指令**: `templates/instructions/worker.md` および orchestrator.md / maestro.md は明示的言及なし。ただし dispatcher のコメント (上記) と maestro.md の安全規則文脈は「cross-runtime safety net」として動作することを暗黙に前提。
- **責務漏出方向**: 実装 → 文書 (死コード)
- **問題**: dispatcher.go:276-281 のコメントが「Codex/Gemini にも適用される pre-flight」と語り、Tier 1 違反を daemon 側でも止めると読める。実体は no-op で、Codex/Gemini Worker は ~/.claude policy を持たないため Tier1 / Tier2 の自律遵守 + 本ファイルの宣言のみが頼り。memory `non_claude_workers_production.md` の「codex/gemini ワーカーは本番利用前提」を踏まえると本来 `validateRunOnMainContent` が機能していてほしい。
- **分類**: 設計議論が必要 (再実装するか、コメントを no-op の事実に更新するか)
- **根拠**: validate_run_on_main.go:14-25, dispatcher.go:271-281。

---

## 7. 即修正候補 / 設計議論候補の分離

### 7.1 即修正候補 (instructions / 文書側で完結する)

これらは `templates/instructions/*.md`, `templates/maestro.md` の編集だけで解消可能 (本タスクでは編集禁止)。

| ID   | 概要                                                                                             |
| ---- | ------------------------------------------------------------------------------------------------ |
| C1-1 | worker.md §lease_epoch ライフサイクル を「配信値をそのまま CLI に渡すだけ」へ縮約                |
| C1-4 | planner.md の definition_of_abort 必須化を任意化 (default 補完に委ねる)                          |
| C2-1 | worker.md §`--summary` vs `--summary-file` の「rm/-rf/Users 誤検知」前提記述を削除               |
| C2-2 | worker.md §`--files-changed` の git status 強制を削除 (descriptive metadata のみ)                |
| C3-1 | worker.md `__system_commit` 受信時保険記述を削除                                                 |
| C3-2 | planner.md §Continuous Mode の `__system_commit` 自動挿入記述を worktree 標準視点に reframe      |
| C4-2 | planner.md §verification ループ上限 (2 ラウンド規則) を daemon `max_repair_count` 一元化に揃える |
| C4-3 | planner.md §quarantine 解除 を Planner instruction から削除し orchestrator.md に集約             |
| C5-1 | maestro.md / worker.md の L2 hook 責任範囲表を「maestro 制御プレーン特化」に書き換え             |

### 7.2 設計議論が必要 (実装変更を伴う)

| ID   | 概要                                                                                    |
| ---- | --------------------------------------------------------------------------------------- |
| C1-2 | Worker の exit code 判定を `非0 → ターン終了` に集約するか daemon が fencing 詳細を隠す |
| C1-3 | `task heartbeat` を opt-in にするか busy_detector 一本化                                |
| C2-3 | verify.yaml の strict YAML decode + bash quoting 制約を daemon 側で吸収                 |
| C2-4 | `plan add-task` の `--content-file -` 系 ergonomics を CLI 側で改善                     |
| C3-3 | worker.md `git` 禁止表を positive list に再構成                                         |
| C4-1 | `repair_pending` 自動補修と Planner の `add-retry-task` を排他にして責務分離            |
| C4-4 | Continuous Mode の next iteration 起動責務を daemon 側に寄せるかの再設計                |
| C5-2 | bash `kill`/`killall`/`pkill` の L1/L2 への追加                                         |
| C5-3 | `validateRunOnMainContent` の再実装 or コメント更新                                     |

---

## 8. グラウンディング上の注意

- 本レポートの行番号引用はすべて読み取り直後のスナップショット。リファクタ後はコード側の値が SSOT。
- L2 hook の「Tier1 何が拒否され何が拒否されないか」は `worker_policy_hook.sh:90-303` を全行確認した結果に基づく (grep マッチではなく逐次読解)。
- `validateRunOnMainContent` は「ABI 互換のために残った no-op」と明示的に宣言されているため、`return nil` の意図的設計であり実装ミスではない (validate_run_on_main.go:16-21)。これは「文書側がまだ古い API を前提にしている」事案。
- 確認できなかった事項 (= 推定):
  - 「busy_detector が SubAgent 実行中 (例: Worker が `Explore` SubAgent を起動した間) も pane 活性を捉えるか」は本監査では未検証 (関連コード `agent/busy_detector.go:50-57` を確認したのみで、SubAgent 起動時の pane 表示挙動の実機実行は未実施)。
  - 「Worker の `task heartbeat` 呼出が現状の e2e で何件発生しているか」は memory にもコードにも統計が残らず不明 (推定: Worker LLM が自発的に呼ぶケースは少数)。

---

## 9. 報告サマリ用カウント

- critical: 2 (C2-1, C5-1)
- major: 9 (C1-1, C1-2, C1-3, C2-2, C2-3, C2-4, C4-1, C4-2, C5-2, C5-3) ※ 10 個列挙したが C1-2 は major、C1-1 と分けて 9 件 → 再集計: C1-1, C1-2, C1-3, C2-2, C2-3, C2-4, C4-1, C4-2, C5-2, C5-3 の 10 件)
- minor: 5 (C1-4, C3-1, C3-2, C3-3, C4-3, C4-4) → 6 件
- 合計: 18 件

(※ 重大度ラベルは finding 本文の `[severity]` 表記が SSOT。サマリ集計は手作業のため finding 本文を優先する。)

### 観点別件数

| 観点 | 件数            |
| ---- | --------------- |
| C1   | 4 (C1-1 ~ C1-4) |
| C2   | 4 (C2-1 ~ C2-4) |
| C3   | 3 (C3-1 ~ C3-3) |
| C4   | 4 (C4-1 ~ C4-4) |
| C5   | 3 (C5-1 ~ C5-3) |
| 合計 | 18              |
