# 観点 A & H 監査レポート

- 監査範囲: maestro_v2 全体（`templates/instructions/*.md`、`templates/persona/*.md`、`templates/maestro.md`、`templates/dashboard.md`、`templates/config.yaml`、`README.md`、`internal/`、`cmd/maestro/`、`MEMORY.md`）
- 監査手段: Read / Grep / 既存 audit/REPORT_C/D/E.md の参照のみ
- 監査者: worker1 (`researcher` ペルソナ + `source-grounded-response` skill)
- 報告日: 2026-05-08
- task_id: task_1778225393_b398a853b367f9f8

> 重要: 本レポートはコード変更を一切伴わない。各 finding は「**事実 (確認済み)**」「**推定**」「**不確実**」を区別する `source-grounded-response` 規約に従う。命名・description からの推測を事実扱いせず、根拠チェーンの終端は必ずファイルパス:行番号で閉じる。

---

## 0. サマリ判定

| ID | 観点 | 判定 |
|----|------|------|
| A1 | Orchestrator に最小指示で自律 LLM Orchestration が回るか | **問題なし** (1 件 nit) |
| A2 | Orchestrator / Planner / Worker の責務分離が設計通りか | **問題あり** (2 件 — 既存 REPORT_C のフォロー) |
| A3 | daemon 側で処理すべきことが Agent に流れていないか (責務漏出) | **問題あり** (3 件) |
| A4 | Agent に過剰な責務が押し付けられていないか | **問題あり** (2 件) |
| A5 | ユーザー介入が必要な箇所が想定範囲に収まっているか | **問題なし** (1 件 nit) |
| H1 | instructions と実コード (CLI / daemon) の挙動乖離 | **問題あり** (4 件) |
| H2 | config.yaml 全キーと参照箇所の突合 (未参照キー / 未定義参照) | **問題あり** (1 件 critical: doc default 値が code default と矛盾) |
| H3 | README / dashboard.md template と現行挙動の差分 | **問題あり** (1 件 critical: verify shell 構文の説明が逆転) |
| H4 | MEMORY.md の「撤去済み」項目の実コード残骸チェック | **要確認** (1 件 — `fallback.Manager` 構造体は残存だが配線は無効) |

合計 finding 数: **AH-1 〜 AH-15 (15 件)**。

### 重大度内訳

- **critical**: 2 (AH-9, AH-12)
- **major**: 6 (AH-3, AH-4, AH-5, AH-6, AH-7, AH-13)
- **minor**: 5 (AH-2, AH-8, AH-10, AH-11, AH-14)
- **nit**: 2 (AH-1, AH-15)

### 重大度凡例

| Severity | 意味 |
|----------|------|
| **critical** | 文書の指示通り行動すると壊れる、または安全前提が崩壊する |
| **major** | 責務境界の混乱で運用ミス / 重複作業 / LLM トークン浪費を誘発 |
| **minor** | 死語化した記述・冗長な指示。LLM の判断は壊さないが noise |
| **nit** | 表現上の改善余地のみ |

---

## 1. 観点 A — 設計・意図整合性

### A1. Orchestrator に最小指示で自律 LLM Orchestration が回るか — **問題なし**

#### AH-1 [nit] Orchestrator instructions の許可ツール記述が冗長

- **対象**: `templates/instructions/orchestrator.md:42-49` / `:212-218` / `:22-29`
- **観察 (確認済み)**: `Bash(maestro queue write planner --type command:*)` / `Bash(maestro skill list:*)` / `Bash(maestro plan request-cancel:*)` の 3 つだけ — `internal/agent/launcher.go:46-59` の `allowedToolsByRole["orchestrator"]` 定義と完全一致。技術的にはこれだけで「ユーザー → Planner 委譲 → 通知受信 → 報告」のループが閉じることを確認した。
- **問題**: 「最小指示で自律的に回る」という設計意図は実現できているが、orchestrator.md 内で「禁止事項リスト」「許可された行動」「Compaction Recovery 時の再確認事項」と同じ内容が 3 回書かれている (`:13-29` / `:51-58` / `:212-218`)。LLM トークン的には無駄だが正しさには影響しない。
- **分類**: 設計議論不要 / 即修正は優先度低 / nit

### A2. Orchestrator / Planner / Worker の責務分離が設計通りか — **問題あり**

#### AH-2 [minor] maestro.md と planner.md / orchestrator.md の許可ツール記述が一貫しない

- **対象指令**: `templates/maestro.md:7-13`, `templates/instructions/orchestrator.md:42-49`, `templates/instructions/planner.md:15-21`
- **対象実装**: `internal/agent/launcher.go:46-86` (allowedToolsByRole) / `:882-918` (appendResolvedMaestroBashAllowances)
- **観察 (確認済み)**:
  - `maestro.md:9-10` は「Orchestrator: `Bash`（`maestro` コマンドのみ）」「Planner: `Bash`（`maestro` コマンドのみ）」と書く。
  - しかし launcher.go ではどちらも narrow per-subcommand 列挙 (Orchestrator は 3 サブコマンド、Planner は `plan:* / skill:* / queue read:* / verify:* / status:* / version:* / --version:* / --help:*`)。
  - planner.md:19 も「`maestro` で始まるコマンドのみ。他コマンドはブロック」とだけ書き、サブコマンド列挙の事実を伝えない。
- **問題**: maestro.md の記述に従うと、Planner LLM は「`maestro queue write planner --type command` も実行できる」と誤解しうる。実際には `Bash(maestro queue:*)` は L1 で許可されておらず、`Bash(maestro queue read:*)` のみ列挙されている (Report 2026-05-05 P0-B regression — launcher.go:60-71 参照)。LLM が試行 → CLI 拒否 → 補修ループという無駄な往復を生む。
- **分類**: 即修正候補 (maestro.md / planner.md / orchestrator.md の許可ツール表を launcher.go の actual allowlist に同期)
- **根拠チェーン**:
  - `launcher.go:60-71` (Planner allowed Bash subcommands を列挙する根拠コメント): 「`Bash(maestro:*)` allowed `maestro queue write planner` ... claude-code restarted the agent ... Daemon-side queue_write already rejects caller_role=planner」
  - `launcher.go:73-79`: actual narrow allowlist
  - maestro.md:7-13: drift 元の「`maestro` コマンドのみ」記述

#### AH-3 [major] planner instructions に「Bash(maestro queue read:*)」が許可されているが、`maestro queue read` サブコマンドが実装されていない

- **対象指令**: `templates/instructions/planner.md:32-34` (「読み取り可能な `.maestro/` ファイル」), 暗黙的に許可ツール経由
- **対象実装**:
  - 許可: `internal/agent/launcher.go:74` (`"Bash(maestro queue read:*)",`)
  - 同上: `launcher.go:909` (resolved-path 版でも `queue read` を列挙)
  - サブコマンドルータ: `cmd/maestro/cmd_queue.go:14-24` — `case "write":` のみ。`read` は default 分岐で `unknown subcommand` エラーになる。
- **責務漏出方向**: 文書 → 実装 (許可ツールが実装に存在しないコマンドを指している)
- **観察 (確認済み)**: `cmd_queue.go:14-24` の switch 文に `read` が無いことをコード読解で確認。Planner LLM が `maestro queue read` を打つと exit 1 で `unknown subcommand` を返す。
- **問題**: L1 allowlist に列挙されている → Planner LLM が「読み取り CLI が用意されている」と誤解して試す可能性がある。CLI が常に失敗するため、補修ループに入って LLM トークンを浪費する経路が成立する。
- **推定**: `Bash(maestro queue read:*)` は廃止された CLI の名残か、または将来用の placeholder。コミット履歴を見ていないため確定できない。
- **分類**: 即修正候補 (`launcher.go:74` および `:909` の `Bash(maestro queue read:*)` 行を削除するか、`maestro queue read` を実装するかの選択)

### A3. daemon 側で処理すべきことが Agent に流れていないか — **問題あり**

> **既存 REPORT_C との重複を避ける**: 観点 C は「責務漏出全般」、観点 A3 は「ユーザー意図に背いて Agent に押し付けられている領域」を見る。以下は観点 C で未挙だった点に絞る。

#### AH-4 [major] worker.md の `--summary-file` レシピは Worker に sandbox-aware tmpdir 知識を要求する

- **対象指令**: `templates/instructions/worker.md` の §「`--summary` と `--summary-file` の使い分け（重要）」 (現行: `:179-228` 周辺)
- **対象実装**:
  - `internal/agent/worker_policy_hook.sh:244-298` — `WT001` がスクラッチ領域 (`/tmp`, `/private/tmp`, `/var/tmp`, `/private/var/tmp`, `/var/folders/*`, `$TMPDIR`) を例外化している
  - `MEMORY.md` の `agent_tmpdir_force_override.md` (記録済み)
- **責務漏出方向**: daemon → Agent
- **観察 (確認済み)**: worker.md は「`mktemp "$TMPDIR/..."` で sandbox に対応せよ」と Worker に指示している。一方 `worker_policy_hook.sh:244-274` は `/tmp`, `/var/folders/*` 等を含むスクラッチ領域を hook 側でホワイトリスト化済み。daemon は agent 起動時に `$TMPDIR` を `<MAESTRO_DIR>/cache/tmp` に強制上書きする (`MEMORY.md` `agent_tmpdir_force_override.md`)。
- **問題**: 二段階防御自体は妥当だが、Worker prompt に「macOS sandbox 環境で素の `mktemp` が失敗するケース」を解説 (`worker.md` 該当箇所) しているのは責務漏出に近い。daemon が `$TMPDIR` を保証する以上、worker 側は単に `mktemp` を呼べば十分。daemon-internal の sandbox 知識を LLM prompt が肩代わりしている。
- **推定**: worker.md の冗長な解説は過去の事故 (Reports of 2026-05-03) に対する防御的記述だが、原因 (TMPDIR 未設定) はすでに daemon 側で恒久対処されている。
- **分類**: 設計議論が必要 (daemon の TMPDIR 上書きが恒常化した今、worker.md の解説を「`$TMPDIR` を使え」の 1 行に縮約できるか)

#### AH-5 [major] planner.md の `add-task` shell quoting 詳説が Planner に shell 知識を要求している

- **対象指令**: `templates/instructions/planner.md:814-892` (「shell quoting の事故防止」「stdin / heredoc / mktemp の選び方」)
- **対象実装**:
  - daemon 側: `add-task` の `purpose` / `content` / `acceptance_criteria` が短すぎる場合の reject — `cmd/maestro/cmd_plan_tasks.go` 経由 daemon API (planner.md:885 で「サーバ側防御」として明示)
  - L1 / L2: Planner には worker_policy_hook.sh は適用されない (launcher.go:701 — `appendDisallowedTools` の `worker` 分岐のみ)
- **責務漏出方向**: daemon → Agent (shell 構文の落とし穴を Planner LLM の責務にしている)
- **観察 (確認済み)**: planner.md は「バッククォート展開を避けるために stdin / heredoc / シングルクオートを使え」と 80 行近く解説している。一方 daemon は短すぎる `content` を reject するだけで、`content` 内のシェル展開事故そのものは検知できない。
- **問題**: Planner に「Bash heredoc の引用規則」「stdin が 1 度しか読めない」「`--content-file` と `--acceptance-criteria-file` の同時 stdin 利用が壊れる」など、daemon 側で構造化入力 (例: protobuf / JSON over UDS) にすれば回避できる shell 固有の罠を LLM に押し付けている。
- **推定**: 現状は CLI が stdin / file flag に依存しているため LLM が shell 知識を持つ必要がある。設計を構造化入力 (UDS 直接 JSON) に置き換えれば、planner.md の §「shell quoting の事故防止」は不要になる。
- **分類**: 設計議論が必要 (Agent → daemon の伝送路が `bash → CLI → UDS` の 3 ホップで shell 解釈が混入する根本問題)

#### AH-6 [major] worker.md の `--files-changed` 規律は Worker に git 状態の自己報告を要求する

- **対象指令**: `templates/instructions/worker.md` §「`--files-changed` の正しい意味」 (現行 `:243-260` 付近)
- **対象実装**: daemon 側の reflog / path-overlap 検査 (`internal/daemon/path_overlap.go` 経由)
- **責務漏出方向**: daemon → Agent
- **観察 (確認済み)**: worker.md は「結果報告の直前に `git status --porcelain` を実行せよ。出力を `--files-changed` に渡せ。空なら指定しない」と Worker に手順を強制している。
- **問題**: daemon は worktree のコミット差分を Phase B/C で自前計算できる (`auto_commit` が走る前後の git ls-files で diff を取得可能)。worker が `--files-changed` を誤って渡すと path-overlap heuristic が誤発火する事故が `MEMORY.md` の `worker_context_budget_fast_fail.md` 等に蓄積されている。
- **推定**: `--files-changed` を Worker 自己申告制にしている設計上の理由が見えない (調査範囲では見つからなかった)。現状は事故防止のために worker.md に長い手順を書いている。
- **分類**: 設計議論が必要 (`--files-changed` を deprecated にし、daemon 側で自動算出する方向の検討)

### A4. Agent に過剰な責務が押し付けられていないか — **問題あり**

#### AH-7 [major] planner.md が verification ループを再帰的に管理している

- **対象指令**: `templates/instructions/planner.md:728-870` (Verification フェーズパターン / 修正ループ / shell quoting / round 上限)
- **対象実装**:
  - `internal/daemon/verify_runner.go` / `verify_runner_real.go`
  - `internal/daemon/result_write_verify_orchestration.go:1-67`
  - 自動 repair: `internal/daemon/result_write_handler.go` 周辺
- **責務漏出方向**: daemon → Agent
- **観察 (確認済み)**: planner.md は「verification round 2 までに収束しなければ `plan complete --status failed`」「3 ラウンド目を投入してはならない」とハード制約を Planner LLM に書かせている。一方 daemon の `repair_pending` 自動投入は `retry.task_execution.enabled: true` 時に動作する (`config.yaml:240`)。
- **問題**: Planner LLM が verification round カウンタを LLM 内部で管理するのは fragile。Compaction recovery 後にカウンタが reset される可能性が高く、上限超過の判定が anti-deterministic になる。daemon 側でカウンタを保持し、上限到達時に `paused_for_replan` を発火させる経路 (`MEMORY.md` `awaiting_fill_stall_escalate.md` で escalate 済み) が既にあるなら、Planner LLM は単に通知を待てばよい。
- **推定**: 現在の Planner LLM が round カウンタを正しく追跡できるかは検証していない (実機 trace 未確認)。
- **分類**: 設計議論が必要 (Planner LLM の認知負荷を round カウンタから外し、daemon 通知ベースに集約)

#### AH-8 [minor] persona/researcher.md の grounding 規則が Worker prompt にも skill 経由で重複注入される

- **対象**:
  - `templates/persona/researcher.md:7-10` (Grounding 規則)
  - `templates/skills/share/source-grounded-response/SKILL.md` (本タスクで自動注入された skill の内容と重複)
- **観察 (確認済み)**: 本タスク自体に注入された persona section (researcher.md) と skill section (`source-grounded-response`) は内容がほぼ同一 (循環検証禁止 / authoritative source 優先 / 推定・不確実の明示)。
- **問題**: トークン重複。Worker LLM prompt size が増える。
- **分類**: 設計議論 / 即修正は優先度低 / minor (persona と skill の境界整理)

### A5. ユーザー介入が必要な箇所が想定範囲に収まっているか — **問題なし**

#### AH-15 [nit] orchestrator.md の quarantine 対応が「ユーザに状況を報告してオペレーターに委譲」で閉じている

- **対象**: `templates/instructions/orchestrator.md:167-177`
- **観察 (確認済み)**: `quarantined`、`publish_quarantined`、`commit_failed` の 3 種類について、Orchestrator が自動実行せず人間に報告する設計 (D009 で復旧 API は operator 専用)。`maestro plan unquarantine` / `resume-merge` / `resolve-conflict` は Worker / Planner からは L1 / L2 / daemon role check の 3 重で拒否される (maestro.md:55, launcher.go:721-731, worker_policy_hook.sh:97-102)。
- **判定**: ユーザー介入箇所は「quarantine 系の最終ゲート」「continuous_paused」「continuous_stopped」の 3 つに収束しており、想定範囲内。設計通り動作する。
- **分類**: nit (備考のみ)

---

## 2. 観点 H — ドキュメント整合性

### H1. instructions と実コード (CLI / daemon) の挙動乖離 — **問題あり**

#### AH-9 [critical] README.md の verify 構文制約が現行コードと逆転している

- **対象**:
  - 文書: `README.md:144-147`
    > `直接 exec のみ。;`、`&&`、`||`、バッククオート、`$(`、`${`、パイプ、リダイレクト、改行は禁止（`sh -c` / `bash -c` も拒否）。必要ならスクリプトファイルを呼ぶ形にする。
  - 実装: `internal/model/verify.go:41-51` / `:105-135`
    - `var unsupportedCommandChars = []string{"\n", "\r"}` のみ
    - コメント:「Shell metacharacters (`;`, `&&`, `||`, “ ` “, `$(`, `${`, `|`, `<`, `>`) used to be rejected ... but verify commands now run under `bash -c`」
    - `ParseVerifyCommand` は `Args: []string{"bash", "-c", trimmed}` を返す
- **問題**: README は「`bash -c` を拒否」と明記しているが、現行コードは **`bash -c` で実行する** (2026-05-06 P0-1 / P1-1 redesign 参照: `verify.go:54-56`)。README の指示に従ってスクリプトに切り出すのは現状不要。Planner LLM が README を真に受けると、すでに動作する `verify.yaml` の shell 構文を冗長なスクリプトに書き換える誤回帰を起こす。
- **重大度**: critical (ユーザ向けドキュメントが実装と逆転している)
- **分類**: 即修正 (README.md:144-147 を planner.md:96-101 と同じ「`bash -c` 経由で `&&`, `;`, `||`, `|`, リダイレクトを使ってよい」に書き換え)
- **根拠**:
  - `verify.go:51`: `unsupportedCommandChars = []string{"\n", "\r"}`
  - `verify.go:105-119`: `Validate()` は newline / CR のみ reject
  - `verify.go:127-135`: `ParseVerifyCommand` は `bash -c <cmd>` を返す
  - `templates/instructions/planner.md:95-101`: 正しい記述「各コマンドは `bash -c` 経由で実行されるため、… 通常の shell 構文をそのまま書いてよい」

#### AH-10 [minor] planner.md の「`Read(.maestro/**)` 不可」記述と launcher.go の実 allowlist が乖離

- **対象**:
  - 文書: `templates/instructions/planner.md:32-34`「読み取り可能な `.maestro/` ファイル: `config.yaml`、`dashboard.md`、`results/worker{N}.yaml`」
  - 実装: `internal/agent/launcher.go:80-83`
    ```
    "Read(.maestro/**)",
    "Read(.maestro/dashboard.md)",
    "Read(.maestro/config.yaml)",
    "Read(.maestro/results/*)",
    ```
- **問題**: 実装は `.maestro/**` 全体を Read 可能にしている (`Read(.maestro/**)` が最広で他は冗長)。docs はこれを 3 ファイルに限定している。
- **影響**: docs を真に受けると Planner LLM は実機で読める他のファイル (例: state、queue) を読まないが、これは **意図された絞込** ではなく **docs の遅れ**。Planner が `awaiting_fill` debug の際 `state/*.yaml` を読む正規ルートが docs から消えている。
- **分類**: 即修正候補 (docs を実 allowlist に同期するか、launcher.go の `Read(.maestro/**)` を絞るかを選ぶ)
- **根拠**: `launcher.go:80-83` を直接確認

#### AH-11 [minor] planner.md の persona injection 説明と実コードの動作が矛盾しないが、注入失敗時の挙動が不明示

- **対象**:
  - 文書: `templates/instructions/planner.md:530-537`「`persona_hint` の検証は識別子安全性のみで、対応ファイルが無くても `persona_hint` は配信エンベロープに残る（注入がスキップされるだけ）。未知値は Worker 側で未指定扱いとなる」
  - 実装: `internal/daemon/persona/persona.go:27-53` (`FormatPersonaSection`) — ファイル読めない場合は空文字を返す
- **観察 (確認済み)**: docs と実装は一致するが、`config.yaml` の `skills.missing_ref_policy` は `warn` / `error` を選べる (`config.yaml:393-394`) のに対し、persona には対応する policy が無い (silent skip 固定)。docs はこの非対称性に言及しない。
- **分類**: nit / 即修正は優先度低

#### AH-12 [critical] maestro.md の Worker hook 説明が、実 hook script の実装と完全に矛盾する

- **対象**:
  - 文書: `templates/maestro.md:91-94` (L2 PreToolUse hook の担当範囲)
    > `Tier1/Tier2 破壊コマンド (D001-D009, B001-B004)、.maestro/ への Bash 経由読み書き、macOS システムディレクトリ書き込み等を **動的** に判定して permissionDecision: deny を返す`
  - 文書: `templates/instructions/worker.md` の §「ツール使用と安全制約」(同様の記述)
  - 実装: `internal/agent/worker_policy_hook.sh:1-44`
    > `Scope (2026-04-30 redesign): only enforce maestro orchestration-model constraints. Generic destructive-command defense (rm -rf, sudo, kill, base64 decode, eval, etc.) is the responsibility of the user's global Claude Code hooks (~/.claude/settings.json) and is intentionally NOT duplicated here.`
- **問題**: 現行の `worker_policy_hook.sh` は Tier 1 (D001-D009 / B001-B004) を **明示的に enforce していない**。実 hook が enforce しているのは:
  - daemon-owned maestro CLI 経路 (planner-only / operator-only サブコマンド)
  - `.maestro/` 制御プレーンへの Bash redirect
  - worktree 内での git mutation
  - `git push` の常時拒否
  - RUN_ON_MAIN モード時の mutation 拒否
  - WT001 (worktree 境界)

  **D001 (rm -rf /)、D004 (git reset --hard)、D005 (sudo)、D007 (mkfs)、D008 (curl|bash)、B001-B004 (pipe to bash, eval) は意図的に L2 hook では検査されず、`~/.claude` 側の global hook に委譲されている** (worker_policy_hook.sh:5-17)。
- **重大度**: critical
  - Worker LLM は maestro.md / worker.md の表通り「Tier 1 はフックがブロックする」と信じて行動するが、実際には `~/.claude` 設定がない環境では D001 / D004 / D005 / D008 / B001-B004 はそのまま実行される。
  - 安全前提の根本崩壊。同時に worker.md は「フックがカバーしないパスでも自律的に守るための冗長な安全網」と位置付けているが、実際には冗長安全網ではなく **唯一の安全網** が文章規約のみで実装が無い。
- **分類**: 即修正 (maestro.md / worker.md の L2 hook 担当範囲表を `worker_policy_hook.sh:5-17` の実態に書き換え、Tier 1 の **どれが hook 内 enforce / どれが ~/.claude 委譲** を明示する)
- **根拠チェーン**:
  - `worker_policy_hook.sh:5-17` (scope を制限する明示的コメント)
  - `worker_policy_hook.sh:90-172` (Bash 分岐の grep 列挙) — D001-D009 / B001-B004 のパターンが含まれていないことを行単位で確認
  - `templates/maestro.md:91-94` (drift 元の表)
  - `templates/instructions/worker.md:101-112` (ツール使用と安全制約: 同じ drift 表)

### H2. config.yaml 全キーと参照箇所の突合 — **問題あり**

> 全キー突合は範囲が広いため、本レポートでは drift / 未参照 / 矛盾の検出に絞って報告する。**確認済み事実**: 主要キー (continuous, watcher, retry, queue, limits, worktree, learnings, skills, verify, admission_control, review, evolution, bandit, complexity, feature_profiles) は内部コードに対応する `Effective*()` メソッドまたは Default 定数を持つことを `internal/model/config_defaults.go:47-119` および config 利用箇所の grep で確認した。

#### AH-13 [major] `awaiting_fill_stall_notify_minutes` の docs default (5) と code default (30) が矛盾

- **対象**:
  - 文書: `templates/config.yaml:50-58` (「default: 5」とコメント、値も `5`)
  - 文書: `templates/config.yaml:54-57` の解説:「Watchdog は… 0 を設定すると watchdog を無効化（R6 のみで運用）」
  - 実装: `internal/model/config_defaults.go:71` `DefaultAwaitingFillStallNotifyMinutes = 30`
  - 実装: `internal/model/config_defaults.go:57-71` のコメント:「A 5-minute threshold proved trigger-happy ... 30 minutes is wide enough」
- **問題**: config_defaults.go のコメントは「5 分は trigger-happy だったので 30 分に変えた」と明記している。しかし `templates/config.yaml` は依然として `5` を書き出しており、setup でこの template を展開すると **デフォルトと異なる値** がプロジェクトに配置される。`MaestroConfig.AwaitingFillStallNotifyMinutes` は `*int` なので、`5` が unmarshal された場合は値 5 が使われ、code default 30 は適用されない (`config.go:106` の `omitempty` ポインタ意味論)。
- **影響**: setup 直後のプロジェクトは「config.yaml に 5 が書いてあるので watchdog が 5 分で発火」となり、Planner の正常な思考時間 (LLM 推論に数分かかるケース) で stall 通知が誤発火する。これは `config_defaults.go:62-69` のコメントが解決済みと主張する事故 (Reports of e2e false-positive) を **再導入** している。
- **重大度**: major (docs 通りに setup すると LLM 推論で false stall を踏む)
- **分類**: 即修正 (`templates/config.yaml:57` を `awaiting_fill_stall_notify_minutes: 30` に変更、コメントの `default: 5` も `default: 30` に同期。あるいは行を削除して `EffectiveAwaitingFillStallNotifyMinutes()` の fallback に委ねる)
- **根拠**:
  - `templates/config.yaml:50-57` を直接確認
  - `internal/model/config_defaults.go:57-71` を直接確認
  - `internal/model/config.go:97-112` で `*int` ポインタの `Effective*()` 動作を確認

### H3. README / dashboard.md template と現行挙動の差分 — **問題あり**

#### AH-9 (再掲) [critical] README.md の verify 構文制約が現行コードと逆転 → §H1 参照

#### AH-14 [minor] dashboard.md template が「Dashboard 不存在時の placeholder」として 18 行のスタブを置いている

- **対象**: `templates/dashboard.md:1-18`
- **実装**: `internal/daemon/dashboard_formatter.go:222` 以下 (`renderDashboard`) / `:476` (`UpdateDashboardFile`) — daemon 起動後に都度 atomic write で **全置換** する。
- **観察 (確認済み)**: daemon が起動して 1 回でも scan が回ると、template の 18 行は完全に上書きされる (`UpdateDashboardFile` が `os.WriteFile` で書き直す)。setup 直後 daemon 起動前の表示は template のまま。
- **問題**: 表示ズレは無いが、`templates/dashboard.md:5-7` の「Daemon: unknown」「Pending / In Progress 列」は実際の dashboard formatter の出力 (queue depth / formation / recent activity / verify status / admission slots ...) とフォーマットが一致しない。Orchestrator が daemon 起動前に dashboard を Read すると古い stub を見ることになる。
- **分類**: nit / 即修正は優先度低 (dashboard を最新フォーマットに揃えるか、空に近い state 表示にする)

### H4. MEMORY.md の「撤去済み」項目の実コード残骸チェック — **要確認**

> MEMORY.md の以下を実コードで個別検証した。

| MEMORY 項目 | 文書主張 | 実コード | 判定 |
|------|------|------|------|
| `policy_hook_bash_only.md` | F-025 Go 移植は撤回済み。Go ラッパや shadow を再導入しない | `internal/agent/worker_policy_hook.sh` 単体 (内蔵 bash) のみ。`policy_checker.go` は script を `WriteHookScript` で書き出すだけ。Go ラッパなし | **整合** |
| `commit_policy_removed.md` | sensitive-file/expected_paths/max_files/message_pattern などの gate は撤去済み | `internal/model/config_types.go:224` (`(Removed) CommitPolicyConfig`)、`config_load.go:147-157` で stale block surfacing、`templates/config.yaml:365-368` で「Removed」コメント。残骸の grep 結果はコメント・テスト・diagnostic のみ | **整合** |
| `fallback_degraded_mode_removed.md` | worker をブラックリスト化する機能は再導入しない | `internal/daemon/fallback/manager.go` (構造体 + Logic は **完全に残存**)、`internal/daemon/handler_registry.go:57-62` (`SetFallbackManager`) も残存。**しかし `SetFallbackManager` は production の wiring 経路で呼ばれない** (grep `SetFallbackManager` で test / 定義以外 0 件)。`d.fallbackMgr` は常に nil なので `result_write_handler.go:337` の `h.fallbackMgr()` も nil を返す | **要確認** (下記 AH-15 / 構造は dead だが残存) |
| `verify_disabled_is_normal_mode.md` | env 必須ガードを再導入しない | `templates/config.yaml:432-438` で `verify.enabled: false` がデフォルト、`internal/daemon/dashboard_data.go:132` で `verifyStatusDisabled = "disabled (verify.enabled=false in config.yaml)"`。env ガード残骸なし | **整合** |
| `setup_repair_template_overwrite.md` | instructions/skills/persona/maestro.md/dashboard.md は binary に紐付く | `internal/setup/init.go:100` のコメント参照 | **整合 (確認は限定的)** |
| `tmux_per_instance_socket_isolation.md` | tmux server を maestro instance ごとに socket 分離 | `internal/tmux/` の実装を確認 (詳細レビュー範囲外) | **未確認** (探索範囲超え) |
| `agent_tmpdir_force_override.md` | agent 起動時の TMPDIR は強制上書き | `internal/agent/launcher.go:145-148` (`buildLaunchEnvForAgent`) で `MAESTRO_AGENT_TMPDIR_OVERRIDE` 経由 | **整合** (`templates/config.yaml:19-22` でも env override 存在を文書化) |

#### AH-15 [minor] `internal/daemon/fallback/manager.go` 一式は production からは到達不能だが構造体・wiring が残存

- **対象**:
  - 残存: `internal/daemon/fallback/manager.go` 全体 (Manager / Mode / workerState / 公開 API)
  - 残存: `internal/daemon/daemon.go:75` (`fallbackMgr *fallback.Manager`)
  - 残存: `internal/daemon/daemon.go:200-205` (`fallbackAccessor`)
  - 残存: `internal/daemon/handler_registry.go:57-62` (`SetFallbackManager`)
  - 残存: `internal/daemon/queue_handler.go:69` (struct field)
  - 残存: `internal/daemon/result_write_handler.go:83` / `:337` (accessor + 呼び出し)
- **観察 (確認済み)**: `grep -rn 'SetFallbackManager\|d\.fallbackMgr =' internal/ cmd/` の結果、`SetFallbackManager` を呼ぶ箇所は test 以外存在しない (`launcher.go` / `daemon_startup.go` / `cmd/maestro` から呼ばれない)。production パスで `d.fallbackMgr` は常に nil。`fallbackAccessor` は nil を返し、`result_write_handler.go:337` の `h.fallbackMgr()` 経由でも nil チェックで空 short-circuit する。
- **問題**: dead code が `internal/daemon/fallback/` 配下に 1 ファイル + tests として残っており、MEMORY.md の「再導入しない」に対する **構造的な防御線が弱い**。新規 PR が `SetFallbackManager` を 1 行追加するだけで再有効化できる。
- **重大度**: minor (現行は dead code、副作用は無い)
- **分類**: 設計議論 (パッケージごと削除するか、`// Deprecated: do not use` コメントで明示するか)
- **推定**: production 配線が存在しないことの根拠は grep 結果のみ。daemon_startup などで `SetFallbackManager` が動的に呼ばれていないかは追加で `grep -rn` で行っており trace は閉じている。

---

## 3. 「すぐ修正」と「設計議論」の分離

### 即修正候補 (docs を真に受けると壊れる / 誤回帰を起こす)

| ID | 概要 | 重大度 |
|----|------|--------|
| AH-9 | `README.md:144-147` の verify 構文制約が逆転 (`bash -c` 拒否 → 実装は `bash -c` で実行) | critical |
| AH-12 | `templates/maestro.md:91-94` と `templates/instructions/worker.md` の L2 hook 担当範囲表が `worker_policy_hook.sh:5-17` の実装と矛盾 | critical |
| AH-13 | `templates/config.yaml:57` の `awaiting_fill_stall_notify_minutes: 5` が code default 30 と矛盾 (false stall 再導入) | major |
| AH-2 | `templates/maestro.md:7-13` / `planner.md:15-21` の許可ツール記述が launcher.go のサブコマンド列挙より過大 | minor |
| AH-3 | `launcher.go:74` / `:909` の `Bash(maestro queue read:*)` が CLI 未実装 (`cmd_queue.go:14-24`) | major |
| AH-10 | `planner.md:32-34` の Read 可能ファイル一覧が `launcher.go:80-83` の `Read(.maestro/**)` より狭い | minor |

### 設計議論が必要 (即修正は影響範囲が広い)

| ID | 概要 | 重大度 |
|----|------|--------|
| AH-4 | worker.md の `--summary-file` レシピが Worker に sandbox-aware tmpdir 知識を要求 | major |
| AH-5 | planner.md の shell quoting 詳説が Planner に shell 知識を要求 (CLI 構造化入力化の検討) | major |
| AH-6 | worker.md の `--files-changed` 自己申告制を daemon 自動算出に置き換える検討 | major |
| AH-7 | planner.md の verification round カウンタを Planner LLM 内部で持たせず daemon 通知ベースに | major |
| AH-8 | persona/researcher.md と skill/source-grounded-response/SKILL.md のトークン重複 | minor |
| AH-15 | `internal/daemon/fallback/` パッケージごと削除するか deprecated 明示するか | minor |
| AH-1 | orchestrator.md の許可ツール記述が 3 重複している (圧縮検討) | nit |
| AH-11 | persona injection の missing-ref policy 設計 (silent skip 固定 vs warn/error) | nit |
| AH-14 | dashboard.md template と daemon 生成出力のフォーマット差 | nit |

---

## 4. 修正コードを含まないことの確認

本レポートは Read / Grep のみを使用し、`audit/REPORT_AH.md` 1 ファイル以外の新規生成・既存ファイル変更を一切行わない。`git status --porcelain` 出力には audit/REPORT_AH.md 1 件のみが現れる想定 (本タスク完了報告時に Worker が確認する)。

---

## 5. 未完了 / 調査範囲外

- **完全な config.yaml キー突合**: 主要キーは確認済みだが、`feature_profiles.{simple,standard,complex,critical}.{cross_agent_review, exploratory_optimization, evolutionary_quality, adaptive_model_selection, self_improvement, adaptive_depth}` の各 6 フラグについて daemon 側の参照箇所までは追っていない (`internal/daemon/featuregate/` の評価ロジックは確認済みだが、profile 名と実機能の対応は深掘り未了)。
- **persona ファイル全数監査**: `templates/persona/{architect,implementer,quality-assurance}.md` 3 件は内容を未読。`researcher.md` のみ確認した。命名・description との対応関係は推定にとどまる。
- **MEMORY.md 全 50 件超の項目検証**: H4 では「撤去済み」明示の 6 項目に絞った。それ以外の項目 (verify shell-passthrough、blocked pane policy など) は実コード残骸チェックを未実施 (探索範囲超過)。
- **CLI subcommand 全数の cobra 定義との突合**: `cmd/maestro/cmd_*.go` の grep で主要 plan / queue / persona / status を確認したが、`maestro setup` / `maestro up` / `maestro down` / `maestro agent launch` 等の operator-only CLI と instructions の整合は未検証。
- **R0-R10 reconcile rule の docs 言及**: R0 / R6 / R7 / R8 / R9 / R10 の言及が instructions / MEMORY.md にあるが、各 rule の実装ファイル名 (`internal/daemon/reconcile/*.go`) との対応は確認していない。
- **本レポートの未確認事項は H4 の「未確認」セルおよび本節に明示済み**。
