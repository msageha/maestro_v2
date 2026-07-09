# Maestro オーケストレーション・パターン Reference

Maestro の 3 層オーケストレーション (Orchestrator → Planner → Worker) の設計根拠を、公開カタログとして最も厳密な部類である [addyosmani/agent-skills](https://github.com/addyosmani/agent-skills) の `references/orchestration-patterns.md` (endorsed patterns + anti-patterns + decision flow) と対比して明文化する。

addy のカタログの統治原則は「ユーザ (または slash command) が orchestrator であり、persona は他の persona を呼ばず、orchestration の深さは最大 1」。Maestro は**意図的に深さ 3 の自動オーケストレータ**であり、この原則から逸脱している。本書はカタログを規範として輸入するのではなく、**どの逸脱がどの構造的緩和策で正当化されるか**を記述する。逸脱の緩和が構造 (ツール制限・daemon 仲介・state ファイル) で担保されていることが Maestro の設計の核であり、prose の注意書きで担保しているのではない。

本 reference は既存構造の**記述**であり、新たなフロー強制・gate・チェックポイントを導入しない。

## 1. 3 層構造と各層の責務

| 層               | 責務                                                         | 技術的強制 (prose ではなく構造)                                                                                                                                                                                  |
| ---------------- | ------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Orchestrator** | ユーザ意図をコマンドとして構造化し Planner に委譲。結果報告  | 許可コマンド 5 種のみ (`queue write planner` / `skill list` / `request-cancel` / `.maestro` status Read / 報告)。コード読取・SubAgent・Edit/Write は L1/L2 でブロック (`templates/instructions/orchestrator.md`) |
| **Planner**      | コマンドをタスク・phase に分解し Worker へ委譲。裁定・再計画 | `maestro` CLI + `.maestro` status Read のみ。コード読取禁止 — 調査は Worker タスクとして発注 (`templates/instructions/planner.md`)                                                                               |
| **Worker**       | タスク実行。worktree 隔離。結果を `result write` で報告      | worktree 境界 (WT001)・git mutation 禁止・`.maestro/` 制御プレーン遮断 (`templates/instructions/worker.md`)                                                                                                      |
| **Daemon**       | dispatch・lease/fencing・auto_commit/merge・verify・recovery | LLM ではない決定的コード。調整ロジックの大半はここにあり、LLM 層は「判断」だけを担う                                                                                                                             |

統治原則は addy と対をなす形で言える: **層間の情報伝達を LLM の paraphrase に載せない。**伝達は構造化ファイル (queue / results / state) と daemon 仲介にのみ載せ、LLM が別の LLM の出力を要約して次工程へ渡す経路を設計上持たない。

## 2. Decision flow — いつ並列、いつ直列、いつ隔離か

Planner がタスク分解時に使う判断フレーム (既存の `dependency-worktree-planning` / `breakdown-plan` skill の要約であり、新規則ではない):

```
要求を受けた
├─ タスク間に成果物依存が無く、expected_paths が重ならない
│    → 同一 phase で並列 fan-out (worktree 隔離が conflict を封じる)
├─ 成果物依存がある / 同一ファイルに触れる / 検証ゲートを挟みたい
│    → phase を分割し depends_on_phases で直列化。
│      統合検証は最終 phase の run_on_integration タスク
└─ 大量のコード読み込みが必要だが、必要なのは digest だけ
     → research 隔離: researcher persona / Explore SubAgent に委譲し
       要約のみを本体 context へ戻す
```

- **並列の前提は worktree 隔離 + Worker の scope 規律**である (worker.md §最重要原則)。隔離が conflict を機械的に防ぎ、scope 規律が隔離を意味あるものに保つ。
- **research 隔離は addy Pattern 5 (research isolation) をほぼそのまま採用した数少ない部分。**大量読み込みの生出力を coordinator の context に流し込まず、digest だけ返す。Maestro では Worker→Explore SubAgent (worker.md §SubAgent 活用ガイド: 探索的 Read/Grep が 3 回を超える調査は委譲既定) と researcher persona がこれを担う。

## 3. addy anti-patterns との対比

### Anti-pattern A: router persona (persona がルーティングだけを行う)

Maestro の Orchestrator は形式上 router である — これは事実であり隠さない。addy の批判の実体は「prose で定義された persona に自由裁量のルーティングをさせると、誤配・スキップ・介入不能が起きる」こと。Maestro の緩和は裁量の除去にある: ルーティング先は Planner ただ 1 つに固定され、実行可能な操作は 5 種に L1/L2 で技術制限される。「どこに送るか」の判断が存在しないため、router の失敗モード (誤配) が構造的に成立しない。

### Anti-pattern B: persona-calls-persona (persona 間の直接呼び出し)

持ち込んでいない。agent 間の直接通信 (tmux send-keys 等) は全 role で禁止され、全ての層間通信は daemon 仲介の queue / results を経由する。Worker の SubAgent は 1 段のみで、daemon API (`plan` / `queue` / UDS) に触れられず結果報告の窓口にもなれない (worker.md §SubAgent への境界条件)。つまり「呼び出しの連鎖」はどの層からも延ばせない。

### Anti-pattern C: paraphrase する sequential orchestrator — 最大の対立点

addy は「LLM の lifecycle orchestrator は (a) ステップ間 paraphrase で nuance を失い (b) human checkpoint を失い (c) token を 2 倍にするから自動化するな」と明言する。Maestro はまさにその自動化 (Orchestrator→Planner→Worker の自律委譲) を設計目標にしており、ここは正面から対立する。逸脱を正当化する緩和策が 3 つの失敗要因それぞれに対応する:

| addy の指摘する損失           | Maestro の構造的緩和                                                                                                                                                                                                                                                                                                                                                 |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| (a) paraphrase で nuance 喪失 | Worker の成果物は daemon が **verbatim に auto_commit** し、coordinator が成果物を書き換える経路が存在しない (worker.md §Commit Task Protocol)。summary は成果物の運搬手段ではなくメタデータ。Planner が消失しても daemon が **state ファイルから synthetic planner result を機械導出**する (`internal/daemon/queue_scan_helpers.go`) — 「要約の再要約」で復元しない |
| (b) human checkpoint 喪失     | checkpoint を「ステップ間の逐次承認」から「**観測 + 随時介入**」に置き換える。Orchestrator はユーザ窓口として残り、`dashboard.md` が全体状態を常時観測可能にし、`maestro plan request-cancel` でいつでも中断でき、escalation は通知として届く                                                                                                                        |
| (c) token 2 倍                | Orchestrator / Planner は**ソースコードを読まない** (両 instruction の F002)。コード context は Worker 層にのみ存在し、上位層は制御メタデータ (YAML / dashboard) だけを扱うため、コード context の複製が起きない                                                                                                                                                     |

残存する trade-off も記録しておく: 自動化した分だけ「ステップごとのユーザ判断」は確かに減る。これは長時間自律運用という目的のための**意図した**交換であり、addy の「ユーザが orchestrator であれ」はその目的と両立しない。緩和は損失をゼロにするのではなく、損失が起きる箇所を paraphrase (検出不能) から構造 (検出・介入可能) へ移すものである。

### Anti-pattern D: deep persona tree (深い動的な委譲木)

深さは 3 で**静的に固定**されており、動的に深くならない。Planner が別の Planner を生む経路も、Worker がタスクを発注する経路も存在しない (Worker は `plan submit` を L2 hook で拒否される)。SubAgent は Worker の道具であって第 4 の委譲層ではない — 結果は Worker が統合して単一窓口として報告する。addy の「深さ 1」には従わないが、「深さが動的に成長する木」という D の実体は共有して排除している。

## 4. カタログへの追加基準 (addy から採用)

addy はカタログの陳腐化を防ぐため「実運用で 2 回使った / repo 内に具体例がある / 既存パターンでダメな理由を言える」まで足すなと規律化している。本 reference も同じ基準を採用する。新パターン・新対比を追加してよいのは、次の 3 条件を全て満たすときのみ:

1. 実運用 (実際の formation 実行) でそのパターンを 2 回以上使った
2. リポジトリ内の具体例 (instruction・skill・daemon コードのファイル参照) を示せる
3. 既存パターンでは不十分だった理由を 1 文で言える

満たさない案は issue として起票し、本書には足さない。

## 5. 本 reference が変えないこと

load balancing・lease/fencing (lease_epoch)・phase merge・retry lineage・publish recovery は実装・検証済みの構造であり、本書はそれらの記述・整理に留まる。adversarial 検証 (doubt-driven) の規律は別 issue の管轄であり、本書は「いつ並列・いつ直列・いつ隔離か」の判断フレームと anti-pattern 対比のみを扱う。
