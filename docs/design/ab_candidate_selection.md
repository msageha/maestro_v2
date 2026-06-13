# 設計方針書: クロスランタイム A/B 候補選抜 (1-B)

- Status: v2.6 — PR1〜PR4 実装済み (全 PR 完了)。codex レビュー承認済み
- Date: 2026-06-12
- 関連: ロードマップ 1-B、運用方針 (性能最優先・トークン費用度外視・自律完結)

## 0.5 PR1 実装後の確定差分 (2026-06-12 全体監査 → 修正で確定)

実装と本文が乖離する場合、本節が優先する。

| # | 本文 (v2.2) | 実装 (確定) | 理由 |
|---|------------|------------|------|
| 1 | R-AB は独立 reconciler | **スキャンステップ + state 駆動発見**。`collectABGroupWork` が `CandidateGroups` (SSOT) から未解決 group を全列挙し、queue 行が消えていても (dead-letter / archive / 未書込) state の TaskStates へフォールバックして収束させる。group なしのタグ付き孤児行はタグ剥がし + 成果 intake で修復 | dead-letter は pending 行を物理削除するため queue 駆動だけでは永久 racing になる |
| 2 | fan-out は queue → state の順に書込 | **state (group) を先に書く**。クラッシュ残骸は常に「durable group + 未タグ/欠落行」になり、state 駆動復旧で一元修復 (untagged canonical → degraded(fanout_incomplete)、shadow 行欠落 → state-only cancel) | 逆順の残骸 (タグ付き行 + group なし) は state から不可視で、成果が候補ブランチに取り残される |
| 3 | タイムアウトで遅い候補を破壊的に終了 | **pending (未 dispatch) / 行喪失の候補のみ CAS 取消**。実行中の敗者は自身の DOA/lease に委ね、全 terminal 後に通常選抜。timeout_sec はグループ作成起点のレース全体予算 | 実行中 worker の worktree を消すのは破壊的すぎる |
| 4 | walkover (単独完走) は即 intake | **単独候補でも Stage 0 + 候補スイートを実行**。健全な検証器で fail した唯一の完走者は intake せず repair 縮退 (`SoleCandidateFailed`) | 検証済みの不良成果を出荷しないため (品質最優先) |
| 5 | 候補 worktree/branch は解決時に削除 | **command cleanup まで全保持** (監査可能性 + 解決前後のクラッシュ窓を構造的に排除)。削除は `cleanupCommandCore` に一元化 | 勝者確定〜resolve 間のクラッシュで再 commit 不能になる wedge を排除 |
| 6 | 勝者決定は選抜のたびに再計算 | **`PendingWinnerTaskID` を intake 前に durable 記録**。クラッシュ後の再入は同じ勝者を再 finalize (flake による別勝者 intake を防止) | 選抜は at-least-once、決定は exactly-once にする |
| 7 | §8: bandit 報酬のみ skip | **全学習経路 (fingerprint / bandit / search tree / novelty / evolution) を skip**。同一論理タスクの二重計上を防ぐ。PR4 で win/lose 信号を追加 | 学習汚染の防止 |
| 8 | retry との相互作用は未規定 | **A/B 候補は daemon auto-retry / R1 再生成 / Planner add_retry_task の対象外** (未解決中)。解決後は勝者/残存 canonical のみ retry 可。`ABGroupID` は構造体コピーで継承させない (retry コンストラクタで必ずクリア) | retry 行が孤児候補 worktree に振られ成果消失 + ExpectedTaskCount 不変条件破壊を防ぐ |
| 9 | barrier は ForCompletion レンズのみ | **CanComplete / HasNonTerminalTaskState (publish・cleanup ゲート) / IsTaskBlocked / CheckDependencyFailure も未解決 group を考慮** | phaseless コマンドで barrier が素通りし候補成果が cleanup で全損するのを防ぐ |
| 10 | path-overlap 言及なし | **同一 ABGroupID 同士は path-overlap 判定から除外** (候補は専用 worktree で構造隔離済み) | overlap ゲートがレースを直列化し walkover 化するのを防ぐ |
| 12 | (PR2) Stage 2/flake の仕様詳細は未確定 | **flake ガード**: 両候補 merge 成功かつ「片方 all pass・片方 fail」のときのみ fail 側を 1 回再実行 (conflict / 両 fail は対象外)。回復 → 同点として Stage 2 へ (evidence に flake_rerun=recovered)。**Stage 2**: healthy Stage 1 の同点時のみ。辞書式 expected_paths 逸脱 (完全一致のみ tie) → diff 行数 → 変更ファイル数 (後 2 者は (larger-smaller)/larger <= 0.10 を tie とする相対マージン、larger=0 は tie)。binary diff は Files に数え Lines は不算入。expected_paths は生存している方の queue 行から両候補へ共通適用 (verifier broken / no verifier は従来どおり default intake で Stage 2 に進まない) | 機械シグナルの決定論性とノイズ耐性の両立 |
| 13 | (PR3) クロステスト行列の仕様詳細は未確定 | 抽出 = `git diff --name-status -z <BaseSHA>..<branch>` の A/M 行を basename glob (既定 + `ab_test.cross_test_patterns`) で判定。両候補が変更した同一パスは除外。overlay = 相手候補のテストファイル**のみ** (`git show`) を merge 中のツリーへ配置し verifyCmds を再実行。スコアは辞書式 (suite, cross)。**cross 中立 (= 満点)**: 相手にテスト変更なし / 単独候補 / どちらかが merge conflict (統合不能候補のテストで生存候補を罰しない)。flake ガードは suite のみ対象 (cross fail は意味的差異)。Stage 2 は (suite, cross) 完全同点時のみ | 「相手のテスト期待を自候補が満たせるか」という対称な実シグナルを決定論に組み込む |
| 14 | (PR4) Stage 3 / 学習接続の仕様詳細は未確定 | **Stage 3**: stage2 が tie_canonical_first の完全同点時のみ、judge 2 体 (`ab_test.judge_models`、既定固定ペア claude-sonnet-4-6 + codex、空リストで無効、1 件は validation 拒否) が両候補の diff (48KB cap、Stage 1 merge 中に採取) + purpose/acceptance を見て A/B 投票。一致 → 採用、不一致 → Stage 2 のマージン無視 lexicographic の僅差側 (同値 → canonical)、judge 失敗/タイムアウト (child ctx 3 分) → canonical。親 ctx cancel は retryable。**bandit**: resolved 遷移の適用時のみ (state lock 解放後) 各候補へ win=1.0/lose=0.0 を記録。degraded は記録しない。walkover は記録する。**verifier_weak**: stage0=no_verifier/verifier_broken/degraded_selection で resolve したら Planner signal `ab_verifier_weak` を group 単位 dedup で送出。**dashboard**: A/B Races セクション (未解決 + 直近解決) | 自己選好回避のクロス judge と、勝敗ラベルの学習接続を加点的に実現 |
| 11 | shutdown 時の選抜は縮退 | **ctx cancel は retryable** (次スキャンで再開)。選抜不能の最終縮退は selecting タイムアウト (selecting マークは commit より先) + repair 縮退エスケープ | shutdown が恒久的な canonical walkover に化けるのを防ぐ |

## 0. v1 からの主な変更 (codex 指摘対応)

| # | v1 | v2 |
|---|----|----|
| 1 | shadow タスクは CandidateGroups のみで追跡 | **両候補とも TaskStates / TaskDependencies に登録** (result_write の登録必須要件・notify gate に適合)。CandidateGroup を第一級 state とし、racing 中の候補を phantom 検査・reconcile・dashboard が明示的に除外 |
| 2 | 敗者 worker branch に revert を積む | **候補専用 branch + 候補専用 worktree で実行**し、勝者のみ canonical worker branch へ取り込む。敗者隔離が構造的に不要になり revert/reset は廃止 |
| 3 | integration worktree を外側から借用 | **`WorktreeManager.RunCandidateSelection` (仮称) を Manager 内 API** とし、`wm.mu` + `integration_status=selecting` で merge/publish と排他 |
| 4 | freeze 言及なし | **選抜完了まで候補 worker 2 体への新規 dispatch を停止** (取り込み・cleanup と worker コミットのレースを遮断) |
| 5 | bandit skip は設計言及のみ | **queue Task に `ab_group_id` を永続化**し result learning 側で除外。選抜後の直接記録用に CandidateGroup へ `{task_id, worker_id, model, bloom_level}` を保存 (taskBloom の in-memory consume に依存しない) |
| 6 | PR1 に Stage 0 なし | **PR1 に Stage 0 (ベースライン健全性) と verifier failure 時の canonical fallback を含める** |

## 1. 目的

同一タスクを異なるランタイム (claude-code / codex) の Worker に**並列発注**し、
客観的な選抜パイプラインで勝者を 1 つ選んで既存の merge / publish パイプラインに
進める。狙い:

1. **性能**: 並列サンプリング + 外部検証器による品質向上 (外部検証器を伴う場合
   のみ best-of-N が実利を生むという 2026 年の知見に基づく)
2. **学習データ**: A/B の勝敗は「bloom バケット × ランタイム」のペア比較ラベル
   として contextual bandit (C-2) と将来の学習ルーターに直接フィードできる

A/B は常に**加点的 (additive) な機能**とする: A/B 機構のどこが失敗しても、
canonical 候補 1 本の従来パイプラインに縮退し、コマンド進行を止めない。

## 2. スコープ

- **対象**: 通常の worktree 実行タスクのみ。`run_on_integration` / `run_on_main`
  と repair / retry タスクは対象外 — A/B は「初回試行の選抜」に限定
- **候補数**: 2 (claude-code 系 × 1、codex 系 × 1)。選抜パイプラインは N 候補
  対応の形で設計するが、N>2 は将来拡張
- **発火条件**: `ab_test.enabled: true` かつ `bloom_level >= ab_test.min_bloom_level`
  (デフォルト 4)。両ランタイムの worker が構成されていなければスキップして通常
  dispatch (警告ログ)
- **非スコープ**: コスト制御、judge モデルの追加構成 (既存 reviewer invoker 流用)

## 3. 実行モデル: 候補専用 branch + 候補専用 worktree

**v2 の核心的変更。** 候補は worker の永続 worktree / worker branch を使わず、
タスク単位の隔離環境で実行する:

```
maestro/{commandID}/candidate/{taskID}   ← 候補専用 branch (integration HEAD 起点)
.maestro-worktrees/{commandID}/candidate-{taskID}/  ← 候補専用 worktree
```

- dispatch 時、daemon が候補専用 worktree を作成し、envelope の `working_dir` に
  そのパスを渡す (**Worker 側は無変更** — working_dir は従来からシステム設定値)
- Worker の完了報告後、daemon が候補 worktree の変更を候補 branch にコミット
  (`CommitCandidateChanges` — 後述の候補専用 API。worker 用 API は流用しない)
- **勝者確定後**: 勝者の候補 branch を **canonical worker の branch に merge で
  取り込む**。これにより worker-branch 単位の既存 merge 機構
  (`MergeToIntegration(commandID, workerIDs)`) は**無変更**で済む
- **敗者**: 候補 branch / worktree を削除するだけ。worker branch は最初から
  無傷であり、revert / reset / 混入リスクが**構造的に存在しない**
- 候補 worktree / branch の cleanup・孤児 GC は既存の worktree GC 機構を拡張

**候補専用 API (PR1 の実装対象として明示)**: 既存の worker 用 API
(`CommitWorkerChanges` は `findWorker(state, workerID)` と worker status 遷移に
結合しており、path 引数化での流用は不可) とは別系統で、WorktreeManager に
候補専用メソッドを新設する:

- `EnsureCandidateWorktree(commandID, taskID) (path, branch, error)`
- `CommitCandidateChanges(commandID, taskID) error`
- `ComputeCandidateDiff(commandID, taskID) (diff, changedFiles, error)`
- candidate branch / path は worktree state ファイルに durable に記録し、
  startup Reconcile と GC が孤児を回収できるようにする

dispatch 側は `resolveTaskWorkingDir` に「`ab_group_id` 付きタスクは候補
worktree パスを返す」分岐を追加する (worker state の偽装はしない)。

**取り込みのレース対策 (freeze)**: A/B 候補の dispatch から選抜確定・取り込み・
cleanup 完了まで、**候補を実行した worker 2 体への新規タスク dispatch を停止**
する (dispatch 収集ループに CandidateGroup 参照のゲートを追加)。canonical
worker branch への取り込みは worker がアイドルな間に行われるため安全。
freeze はタイムアウト (§7) で必ず解除され、解除漏れは reconciler (R 系に
1 パターン追加) が回収する。

> 取り込み merge が conflict した場合の扱いは §6 (PR1 では degraded 縮退)。

## 4. 状態モデル: 対称 CandidateGroup (第一級エンティティ)

```yaml
# CommandState 拡張 (state/commands/{id}.yaml)
candidate_groups:
  grp_<id>:
    status: racing | selecting | resolved | degraded
    canonical_task_id: task_A    # phase/required に属する側 (発注時に確定)
    winner_task_id: ""           # resolved 後に設定
    candidates:
      - task_id: task_A
        worker_id: worker1
        model: opus              # bandit 記録用に dispatch 時点で永続化
        bloom_level: 5
        branch: maestro/cmd_x/candidate/task_A
      - task_id: task_B
        worker_id: worker3
        model: codex
        bloom_level: 5
        branch: maestro/cmd_x/candidate/task_B
    selection_evidence: {}       # §5 各 Stage の結果 (監査・学習用)
```

- **両候補とも `TaskStates` / `TaskDependencies` に登録する** (result_write の
  validation と notify gate の要件を充足)。queue Task には `ab_group_id` を
  持たせ、daemon の各機構が「A/B 候補である」ことを機械判定できるようにする
- **phase.TaskIDs / RequiredTaskIDs / OptionalTaskIDs には canonical のみ**を
  入れる。phase 進行・completion・publish ゲートは canonical 1 本を参照する。
  ただし **CandidateGroup が未解決 (`racing | selecting`) の間は §4.5 の barrier
  により phase 進行の対象外**であり、「既存ロジック無変更」は参照構造についてのみ
  成り立つ (barrier 分の変更は §4.5 が定義する)
- **非 canonical 候補のライフサイクル**は CandidateGroup が SSOT:
  - phantom 検査・R 系 reconciler・dead-letter・dashboard は、`ab_group_id` を
    持つタスクについて CandidateGroup の status を参照し、`racing | selecting`
    中は介入しない (明示的な except 分類。散在させず「ab 候補か?」の判定
    ヘルパーを 1 箇所に置く)
  - `resolved` 後: 敗者は `cancelled` / reason=`superseded_by_ab_loser` で終端
    (notify は既存の superseded 黙認パターンを流用)
- **B (非 canonical) 勝利時**: 既存 retry-supersede と同じ手順で state を置換 —
  `replaceInRequiredOrOptional(A→B)` + `rewriteDependencies(A→B)` +
  phase.TaskIDs 置換 + lineage 記録 (`ab_winner:task_B`) + A を
  `cancelled`/`superseded_by_ab_winner` で終端。以降 B が canonical

## 4.5 選抜前 barrier (PR1 の不変条件)

`ab_group_id` 付きタスクは、所属 CandidateGroup が `resolved | degraded` に
なるまで、completed 報告後も以下の**通常後続処理の対象にしない**:

| 通常処理 | A/B 候補での扱い |
|---|---|
| Planner への成功通知 (notify gate) | **保留**。選抜確定後、勝者のみ既存通知へ流す。敗者は superseded 黙認パターンで silent ack |
| phase completion / phase merge 収集 | **対象外**。canonical が completed でも group 未解決なら phase は進めない (notify 保留により Planner 側も完了を認知しない) |
| worker branch への auto-commit / merge 収集 | **対象外**。候補の変更は候補 branch にのみコミットされ、worker branch は選抜確定の取り込みまで無変更 |
| advisory review (PrecaptureDiff + review dispatch) | **skip**。選抜前のレビューは重複かつ worker branch 差分前提のため誤差分になる。必要なら勝者取り込み後に勝者差分のみレビュー |
| bandit 報酬 (recordBanditReward) | **skip** (§8)。選抜確定時に直接記録 |

barrier の判定は「ab 候補か? group は未解決か?」を返すヘルパー 1 箇所に集約し、
notify gate / phase 収集 / review / 報酬の各所はそれを参照する。

## 5. 選抜パイプライン

前提の物理制約: worker/候補 worktree は deps キャッシュを持たず検証コマンドを
実行できない。検証は deps 整備済みの **integration worktree** で行う。

### 5.0 排他: `WorktreeManager.RunCandidateSelection` (Manager 内 API)

選抜の git 操作全体 (base 検証 → 候補マージ → 検証 → クロステスト → 復元) を
WorktreeManager のメソッドとして実装し、以下を**一括管理**する:

- `wm.mu` による merge / publish / 他コマンドとの排他
- 開始条件: integration がアイドル (MERGE_HEAD なし、publish 進行中でない、
  quarantine でない)。busy なら defer して次スキャンで再試行
- `integration_status = selecting` を worktree state に記録し、選抜中の
  merge / publish / forward-merge をすべて defer させる
- 開始時に `preSelectionSHA` を記録し、各候補の評価後・選抜完了後に
  `reset --hard preSelectionSHA` + `clean -fd` で完全復元 (`prepareIntegrationForMerge`
  と同じ後始末規律)。daemon crash 時は startup Reconcile が `selecting` 状態を
  検出して復元する (publish-sync marker と同様の durable marker)

### Stage 0: ベースライン健全性 (検証器自体の検証)

候補の変更を含まない base 状態 (integration HEAD) で verify.yaml の全コマンド
を実行する。

- **base で fail** → 検証器か環境が壊れている (候補のせいではない)。機械検証は
  順位付けに使わず Stage 2/3 で選抜し、`verifier_broken` を planner_signals に
  送出。selection_evidence に記録
- **base で pass** → Stage 1 へ。base での所要時間を記録し、候補評価の
  タイムアウト基準にする

### Stage 1: 検証行列 (候補スイート + クロステスト)

各候補について (integration worktree 上で逐次):

1. **候補スイート**: 候補 branch を一時マージし verify.yaml 全コマンドを実行
2. **クロステスト**: 相手候補が追加・変更したテストファイル
   (base..candidate の diff をテスト命名パターンで機械抽出: `*_test.go` /
   `*.test.*` / `test_*.py` / `*.spec.*` 等、config で拡張可) を overlay して
   実行。両候補が同一テストファイルを変更している場合は当該ファイルを除外

スコアは辞書式 `(候補スイート pass 数, クロステスト pass 数)`。片候補のみ
全 pass → 勝者確定。

### Flake ガード

片候補のみ fail した場合、確定前に fail 側を 1 回だけ再実行。再実行で pass に
転じたら「両 pass」として Stage 2 へ (不安定さは selection_evidence に記録)。

### Stage 2: 機械メトリクス (決定論 tiebreak)

辞書式 (マージン付き): expected_paths 逸脱数 → diff 行数 → 変更ファイル数。

### Stage 3: クロス LLM 判定 (最後の同点決勝)

既存 reviewer invoker を流用。判定基準は task の `content` /
`acceptance_criteria` への適合。claude 系 judge が候補 B を、codex 系 judge が
候補 A を採点 (自己選好の構造的回避)。一致 → 採用。不一致 → Stage 2 の僅差側。
完全同点 → canonical 既定勝利 (自律完結、人間エスカレートなし)。

### 縮退の不変条件

どの Stage / git 操作が失敗しても: integration を preSelectionSHA に復元 →
**canonical 不戦勝**として resolve → `ab_selection_degraded` を
selection_evidence とログに記録。A/B は常に「canonical 単独実行に + α」であり、
失敗してもコマンド進行を悪化させない。

## 6. 勝者の取り込み

- **A (canonical) 勝利**: 勝者候補 branch を canonical worker branch に merge
  (worker は freeze 中なので安全)。以降は従来どおり worker branch 単位の
  phase merge へ
- **B 勝利**: §4 の supersede で state を B に置換した上で、B の候補 branch を
  **B を実行した worker の branch** に merge (B 側 worker も freeze 中)
- **取り込み merge が conflict した場合**: 候補 branch は integration HEAD 起点
  なので、worker branch が先行 phase の未マージコミットを持つ場合にのみ起こる。
  **PR1 ではこのケースは `degraded` 縮退とする** (取り込みを中止し、canonical を
  従来の単一候補として再実行扱い = 通常 retry 経路へ。選抜結果は evidence として
  記録するが採用しない)。「勝者候補 branch を MergeToIntegration の merge 対象に
  直接渡す」案は、MergeToIntegration が workerIDs→WorktreeState 解決・worker
  status・conflict signal に深く結合しているため小改修では済まない (中〜大工数)。
  実施するなら `MergeSource{workerID, branch, kind}` の明示モデル導入として
  将来 PR で設計する

## 7. 同期・タイムアウト・失敗モード

| 状況 | 挙動 |
|---|---|
| 両候補 terminal | 選抜開始 (§5) |
| 片候補が `ab_test.timeout_sec` 超過 / dead-letter | 残存候補の不戦勝 (Stage 0 + 候補スイートで健全性のみ確認) |
| 両候補失敗 | canonical の既存 repair 経路に一本化。グループは `degraded` で終端、A/B は繰り返さない |
| integration busy が続き選抜を開始できない | `ab_test.selection_timeout_sec` 経過で canonical 不戦勝 (検証なし、`ab_selection_degraded`) |
| freeze 解除漏れ / グループ宙吊り | reconciler (新パターン R-AB) が CandidateGroup の経過時間で検出し、canonical 不戦勝で強制 resolve + freeze 解除 |
| daemon crash | CandidateGroup と integration の `selecting` marker は state ファイル / git ref として durable。startup Reconcile が「selecting → 復元 + racing へ巻き戻し or 強制 resolve」を行う |

## 8. 学習への接続

- **bandit (C-2) の二重計上防止**: queue Task の `ab_group_id` を TaskResult 経由
  ではなく **dispatch 時に CandidateGroup へ永続化した `{model, bloom_level}`**
  で扱う。`recordBanditReward` は `ab_group_id` 付きタスクをスキップ (queue Task
  読み出しは dispatch 時に記録済みの ab フラグを taskBloom と同様の経路で参照)。
  選抜確定時に勝者 `RecordResult(model, bloom, 1.0)` / 敗者 `0.0` を直接記録
- **判別力の計測**: 「両候補 base スイート全 pass」率を command 単位で記録し、
  閾値超過で `verifier_weak` を planner_signals へ → Planner が verify.yaml を
  強化する改善ループ
- **selection_evidence**: 決着 Stage・スコア・flake 有無を保存 (学習ルーター
  2-b の訓練データ)

## 9. 設定 (config.yaml)

```yaml
ab_test:
  enabled: false              # デフォルト off (実験的機能)
  min_bloom_level: 4
  timeout_sec: 0              # 0 = タスクの max_wall_clock_sec に従う
  selection_timeout_sec: 1800 # integration busy 等で選抜を開始できない場合の上限
  flake_retry: 1
  cross_test_patterns: []     # 内蔵デフォルトに追加するパターン
```

## 10. 実装フェーズ分割

| PR | 内容 | 完結性 |
|---|---|---|
| PR1 | CandidateGroup state + `ab_group_id` + 候補専用 API (`EnsureCandidateWorktree` / `CommitCandidateChanges` / `ComputeCandidateDiff` + durable 記録) + **選抜前 barrier (§4.5: notify 保留 / phase 対象外 / review skip / 報酬 skip)** + worker freeze + `RunCandidateSelection` (Stage 0 + 候補スイートのみ) + 勝者取り込み (conflict 時 degraded 縮退) + 全縮退経路 + R-AB reconciler | **単体で安全に機能完結** (検証器健全性チェック付き簡易 A/B) |
| PR2 | Stage 2 (メトリクス tiebreak) + flake ガード + crash 復元の E2E | |
| PR3 | クロステスト行列 (Stage 1 完成) | |
| PR4 | Stage 3 (クロス LLM judge) + bandit 勝敗記録 + verifier_weak シグナル + dashboard 表示 | |

各 PR ごとに E2E (実 tmux + 実 git) で検証する。

## 11. 残懸念 (ユーザー / レビュアー判断材料)

1. **工数**: v2 (候補専用 worktree + 対称 CandidateGroup) は v1 (shadow + revert)
   より構造的に安全だが、worktree manager / dispatch / reconcile への変更面が
   広い。PR1 だけで中規模 (目安: B2/B1 級の修正 5〜8 個分)
2. **worker freeze による並列度低下**: A/B 1 グループの実行中、worker 2 体が
   そのタスクに占有される。性能最優先方針では許容のはずだが、worker 数 4 構成
   では同時 A/B は実質 1〜2 グループが上限
3. **選抜の直列性**: integration worktree の逐次借用のため、候補評価は直列
   (候補 2 × verify.yaml 所要時間 + α)。deps を複製した評価専用 worktree の
   並列化は将来最適化として保留
4. **judge の相関エラー**: クロス判定でも両 judge が同方向に誤る可能性は残る。
   最終防衛線は publish 後の人間レビュー (既存ゲート、変更なし)
5. **canonical の事前固定**: どちらのランタイムを canonical にするかは発注時の
   既定 (例: bandit の現推奨) に従う。同点時に canonical が勝つ規則と合わせて、
   わずかに canonical 有利のバイアスがある (決定論性とのトレードオフ)
