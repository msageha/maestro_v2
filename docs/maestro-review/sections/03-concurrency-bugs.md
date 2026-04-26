# 03. 並行性・状態遷移・I/O レビュー

## サマリー

- 検出件数: **8 件** (Critical 1, High 2, Medium 4, Low 1)
- 危険度上位 3 件:
  1. **Critical**: `result_write_handler.go` の `advanceRepairPendingToPausedForReplan` → `emitPausedForReplanPlannerSignal` 経路で **state→queue の逆順ロック** を取得している (`internal/daemon/doc.go:13` のキャノニカル順序違反)。同じ瞬間に `queue:planner_signals → state:{commandID}` を取得する経路は現状無いため即時デッドロックには至らないが、将来コードが追加されれば確実にデッドロックする。
  2. **High**: `internal/plan/retry.go:22-33` の `saveStateWithContext` は ctx タイムアウト時に goroutine を野放しにし、後続の `restoreStateOrLog` リカバリと **書き込み順序が逆転して新しい状態を破棄する** リスクがある。fs ハング (NFS 等) で状態巻き戻しが発生し得る。
  3. **High**: `internal/plan/state.go:172` の `DeleteState` および `internal/daemon/reconcile/r0_planning_stuck.go:114` の `os.Remove(statePath)` は兄弟の `*.bak` を削除しない。次回同 commandID の状態 YAML が破損した時、`state_recovery.go` が古い世代の `.bak` を復元し、ORC-3 epoch クランプの効果を超えて旧状態が復活する余地が残る。
- 回避策: ほとんどの High 以下は単独パスでは現状顕在化しない (シリアライザ・scanMu 等が偶然守っている)。Critical は documentation を追加するか順序を是正すべきだが、即時クラッシュは無い。

## 調査範囲と前提

- `internal/daemon/`、`internal/plan/`、`internal/lock/`、`internal/yaml/`、`internal/model/` を主に Read。
- ロック規約は `internal/daemon/doc.go:13-72` の Canonical lock order (queue → state → result、queue 内 sub-order: planner → worker* → orchestrator → planner_signals) を正本として参照。
- 状態遷移規約は `internal/model/status.go` の `validCommandTaskQueueTransitions` / `validTaskStateTransitions` / `validPhaseTransitions` を正本として参照。
- 「推定」と明記しない箇所はコード読解で確認済み。動的検証 (実行・再現) は実施していない。

---

## a. lease / fencing 不整合

### Finding A-1

| 項目 | 値 |
|---|---|
| カテゴリ | lease/fencing |
| 重要度 | Low |
| 場所 | `internal/daemon/result_write_phase_a.go:430-434` |
| 現状の問題 | `updateQueueState` は terminal status へ遷移する際に `LeaseOwner` / `LeaseExpiresAt` をクリアするが、`LeaseEpoch` は残置する。READMEと `internal/daemon/lease/manager.go:152-168` の `releaseLease` 設計では epoch は意図的に保持する仕様だが、Phase A は `releaseLease` を経由せず直接構造体を書き換えている。フェンシングは `in_progress` 状態のみ厳密一致比較するため運用上の不整合は発生しないが、規約バイパス。 |
| 再現条件 | Worker が `result write --status completed` を投げ、Phase A 経路で queue が更新される全ケース。 |
| 推奨アクション | `lease.Manager.releaseLease` を呼び出すように統一し、フィールド更新箇所を一元化する。少なくとも `// Phase A bypasses leaseManager intentionally; epoch is preserved per H5` 等のコメントで意図を明示する。 |

### Finding A-2

| 項目 | 値 |
|---|---|
| カテゴリ | lease/fencing |
| 重要度 | Medium |
| 場所 | `internal/daemon/queue_scan_collect.go:417-421` (`checkInProgressDependencyFailuresDeferred`) |
| 現状の問題 | `in_progress` のままの依存先タスクを cancelled に書き換える際、`task.LeaseEpoch` は維持される一方、`releaseLease` が呼ばれず `LeaseManager` の内部メトリクス・H5 監査ログが更新されない。後続の Worker が同じ epoch で heartbeat / result write を投げると lease は既に queue 上は cancelled に遷移済みなので `validCommandTaskQueueTransitions` で `cancelled→cancelled` または `cancelled→completed` を弾く必要があるが、その検証が `applyTaskDispatchResult` 系には無い (Phase A は `Validate` を呼ばない)。 |
| 再現条件 | 推定: 依存先失敗による cancellation と Worker の遅延 result write が衝突するタイミング。同じ task_id に対して queue 側は cancelled・Worker 側は completed を報告する競合。 |
| 推奨アクション | `releaseLease(task)` を呼び出して epoch を保持しつつクリア手順を一本化する。さらに `result_write_phase_a.go:431` の terminal 設定の前に `model.ValidateCommandTaskQueueTransition(task.Status, resultStatus)` を入れて queue 側 cancelled を上書きしないようにする。 |

### Finding A-3

| 項目 | 値 |
|---|---|
| カテゴリ | lease/fencing |
| 重要度 | Medium |
| 場所 | `internal/daemon/task_heartbeat_handler.go:202` |
| 現状の問題 | heartbeat 受領時に `yaml.AtomicWrite(queuePath, &queue)` で queue ファイルを更新するが、`recordSelfWrite` を呼んでいない。同名のキュー書き込みは `result_write_phase_a.go:444,447`、`result_write_handler.go:562` ではすべて `recordSelfWrite` でセルフ書き込みとしてマークされており、event-bridge / fsnotify が外部編集だと誤認しない仕組みになっている。heartbeat だけがこの保護から外れている。 |
| 再現条件 | event-bridge または fsnotify ベースの再スキャンが有効化されている環境で、heartbeat 頻度が高いタスク。fsnotify が queue 変更を「外部編集」と認識して spurious な scan/replan を誘発する。 |
| 推奨アクション | `result_file_store` 経由で `SaveQueueFile` を使い `recordSelfWrite` を呼ぶ、または `task_heartbeat_handler.go:202` で `h.recordSelfWrite(queuePath, queue)` を直接呼ぶ。 |

---

## b. 状態遷移バグ

### Finding B-1

| 項目 | 値 |
|---|---|
| カテゴリ | 状態遷移 |
| 重要度 | Medium |
| 場所 | `internal/daemon/queue_scan_collect.go:417` (`checkInProgressDependencyFailuresDeferred`) |
| 現状の問題 | `task.Status = model.StatusCancelled` を直接代入しており、`validCommandTaskQueueTransitions` に基づくバリデーションを通していない。同関数は in_progress タスクのみ走査する想定だが、ループの呼び出し元 (`collectExpiredTaskBusyChecks` 周辺) が走査時点と書き換え時点でステータスを再確認していないため、レアケースで pending・cancelled・completed のタスクを cancelled に上書きする可能性が残る。 |
| 再現条件 | 推定: Phase A 走査中に同一タスクへの並行更新 (Worker の `result write completed` と queue scan の cancellation 判断) が衝突するケース。`scanMu.RLock` は heartbeat 側で取られるが、collect は `scanMu.Lock` を保持していないため別 goroutine の更新を見逃し得る。 |
| 推奨アクション | `model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled)` を呼び出し、不正遷移ならスキップ (warn ログ) する。また走査時点の status を関数内で再検査する。 |

### Finding B-2

| 項目 | 値 |
|---|---|
| カテゴリ | 状態遷移 |
| 重要度 | Low |
| 場所 | `internal/daemon/result_write_phase_a.go:429-449` |
| 現状の問題 | `updateQueueState` は terminal status の代入前に遷移検証を行わない。`completed/failed/cancelled/dead_letter` への遷移はすべて `in_progress` 起点のみ valid だが、Phase A 到達時点で queue 上のタスクが何らかの理由で `pending` 等に戻っていた場合 (R1 reconciler が干渉した直後など) でも上書きされる。 |
| 再現条件 | 推定: 長時間タスクで queue scan の repair が `in_progress→pending` に戻した直後に Worker が `result write` を発行する競合。 |
| 推奨アクション | `model.ValidateCommandTaskQueueTransition(queueTask.Status, resultStatus)` を呼び出し、無効遷移は warn ログを出して queue 書き込みをスキップ (result file は既にコミット済みなので reconciler に任せる)。 |

---

## c. YAML I/O 競合

### Finding C-1

| 項目 | 値 |
|---|---|
| カテゴリ | YAML I/O / 復旧整合 |
| 重要度 | High |
| 場所 | `internal/plan/state.go:172` (`DeleteState`) / `internal/daemon/reconcile/r0_planning_stuck.go:114` (`os.Remove(statePath)`) |
| 現状の問題 | 状態ファイル削除時に兄弟の `<state>.bak` を削除しない。`internal/yaml/atomic.go:88-157` の `AtomicWriteRaw` は書き込みごとに `.bak` を作成するため、同一 commandID で再度状態が作られた後に YAML が破損 (truncated 等) すると `internal/daemon/state_recovery.go:41-96` (`recoverStateDir`) が古い `.bak` を「正当な前世代」として復元し、ORC-3 epoch クランプ (`state_recovery.go:51,88,186-202`) でも整合できない論理状態に陥る (タスクの存在/不在が時系列で逆転する)。 |
| 再現条件 | (1) commandID X で状態を作成 → (2) `DeleteState`/R0 が削除 → (3) 同じ commandID X で別シナリオの状態を作成 → (4) (3) の YAML が短時間で破損 → (5) 起動時 recoverStateDir が (1) 時点の `.bak` を復元。CommandID は採番設計上は使い回さない想定だが、Bridge の手動リプレイや bug 修正による再投入では衝突し得る。 |
| 推奨アクション | `DeleteState` 内で `os.Remove(statePath + ".bak")` を best-effort 実施。R0 の `os.Remove` も同様に対応。テスト `internal/plan/state_test.go:100` の `TestStateManager_DeleteState` を更新して `.bak` 不在を assert する。 |

### Finding C-2

| 項目 | 値 |
|---|---|
| カテゴリ | YAML I/O / 復旧整合 |
| 重要度 | Medium |
| 場所 | `internal/daemon/state_recovery.go:41-96` (vs `.maestro/queues/`) |
| 現状の問題 | 状態 YAML には `.bak` リカバリ + ORC-3 epoch floor clamp があるが、queue YAML (`internal/daemon/file_store` 系) には対応する `recoverQueueDir` が存在しない。queue ファイルが起動時に部分書き込み (truncated) になっていた場合、daemon は空キュー扱いで起動するため、queue 上の `lease_epoch` を含む全タスク履歴が消滅する。state 側との整合監査 (R1 reconciler) はあるが、queue が空ならば repair ロジックは「何もしない」分岐になる。 |
| 再現条件 | crash-during-write で queue YAML が壊れたまま daemon を再起動。`internal/yaml/atomic.go` は temp + rename + fsync_dir で原則安全だが、ファイルシステム異常時 (full disk, NFS) では rename が失敗・部分反映する余地はある (推定)。 |
| 推奨アクション | queue ファイルにも `loadYAMLFile` の corrupted 検出 → `.bak` 復元 → epoch floor clamp (queue 側 lease_epoch は state 側に追従する設計だが、最低でも state 側 epoch を超えない範囲で復元する) を実装する。少なくとも起動時の queue 破損を fatal にして手動リカバリを強制する選択肢も検討する。 |

### Finding C-3 (該当なし根拠)

`AtomicWriteRaw` (`internal/yaml/atomic.go:88-157`) は temp file → write → validate (round-trip) → checksum cache → `.bak` copy → rename → `syncDir` の順で動作し、各 commandID の state 書き込みは `state:{commandID}` ロックで直列化される。同一 YAML への並行書き込みパスは `MutexMap` で排他されているため、状態 YAML 同一ファイルの直接競合パスは確認できなかった。queue YAML も同様に `queue:{worker}` キーで直列化されており、heartbeat と Phase A の併走は `scanMu.RLock` で保護されている。

---

## d. ファイルロック (flock / mutex)

### Finding D-1

| 項目 | 値 |
|---|---|
| カテゴリ | ファイルロック |
| 重要度 | **Critical** |
| 場所 | `internal/daemon/result_write_handler.go:570-595` (`advanceRepairPendingToPausedForReplan`) と `:597-635` (`emitPausedForReplanPlannerSignal`) |
| 現状の問題 | `advanceRepairPendingToPausedForReplan` は `state:{commandID}` ロックを取得し (`L572`)、defer で解放する。**ロック保持中に** `emitPausedForReplanPlannerSignal` を呼び (`L594`)、その内部で `queue:planner_signals` を取得する (`L612`)。これは `internal/daemon/doc.go:13` のキャノニカル順序 `queue → state → result` の **完全な逆順** であり、別経路で `queue:planner_signals → state:{commandID}` を取得する箇所が一つでも存在すれば即デッドロックする。現状そのような相互経路は確認できなかったが (signal 配信は queue:planner_signals のみ取得)、コードレビューで気付きにくい時限爆弾である。 |
| 再現条件 | 将来 PlannerSignal 処理に「commandID 単位の状態を更新する」コードが追加された時点で 100% 顕在化する。すなわち今すぐは再現しないが、潜在的にはレビュー漏れで容易に踏む。 |
| 推奨アクション | `advanceRepairPendingToPausedForReplan` の処理を 2 段階に分割し、(1) state ロック内で必要情報をスナップショット → 解放、(2) `queue:planner_signals` 取得 → 書き込み、の順にする (R1 reconciler と同じパターン: `internal/daemon/reconcile/r1_result_queue.go:107-202`)。 |

### Finding D-2

| 項目 | 値 |
|---|---|
| カテゴリ | ファイルロック |
| 重要度 | Low |
| 場所 | `internal/lock/lock.go:192-228` / `:245-250` |
| 現状の問題 | `FileLock` は flock LOCK_EX|LOCK_NB ベースで取得・解放するが、ロックファイル自体は意図的に削除しない (コメント `:245-250` で言及)。長時間運用で `.maestro/locks/` 配下のロックファイルが単調増加し inode 枯渇を招く可能性がある (推定)。 |
| 再現条件 | 数千〜数万コマンドを処理する長期運用環境。 |
| 推奨アクション | 起動時 / 定期 GC で 7 日以上アクセスのないロックファイルを削除する。flock は file descriptor 単位なので削除はロック保持中の他プロセスに影響しないが、安全のため flock NB 取得が成功した時点で削除する 2 段階処理が望ましい。 |

### Finding D-3 (該当なし根拠)

`MutexMap` (`internal/lock/lock.go:19-23`) は ref counting で空エントリを GC しており、長期運用でのメモリリークは無い。`internal/plan/queue_locks.go:73` の `lockQueueKeys` はキーをソート順序 (planner=00, worker=10, default=50, orchestrator=90, planner_signals=99) でソートしてから取得するため、queue 内の sub-ordering デッドロックは確認できなかった。`task_heartbeat_handler.go:217` の `acquireFileLock` は `scanMu.RLock` を併用し、queue 単位のキーで直列化する設計で defer 解放も完了している。

---

## e. 通知再送 / idempotency

### Finding E-1 (該当なし根拠)

PlannerSignal 配信は `internal/daemon/signal_store_yaml.go:30-50` で `signalDedupKey` (Kind/CommandID/PhaseID/Reason の合成キー) によって idempotent に追加される (`result_write_handler.go:614-625` の `index[key]` チェック)。同一トリガーが複数回発火しても重複登録は防がれる。heartbeat は別経路で lease_epoch 一致を厳密検証するため、再送に対して fencing_reject を返すことで idempotency を担保している (`task_heartbeat_handler.go` 全体)。再送・重複に起因する問題は確認できなかった。

---

## f. レース条件 (失効 / heartbeat / release / 再 dispatch)

### Finding F-1

| 項目 | 値 |
|---|---|
| カテゴリ | レース |
| 重要度 | High |
| 場所 | `internal/plan/retry.go:22-33` (`saveStateWithContext`) |
| 現状の問題 | ctx タイムアウト時、関数は早期 return するが起動済み goroutine は `saveFn`(=`sm.SaveState(state)`) を実行し続ける (チャネル `done` は buffered size 1 なので送信は成功して goroutine 自体は完走するが、syscall ハング時はそのまま残る)。タイムアウト後、呼び出し元 `writeAndCommitRetryQueue` (`L286-295`) は `restoreStateOrLog` で原状回復しようと別の `SaveState` を呼ぶ。`SaveState` 内部は `state:{commandID}` ロックで直列化されるため厳密な書き込み交錯は無いが、**(1) 復旧 SaveState** → **(2) ハング解放後の元 SaveState** の順序が成立すると **新しい状態 (=ハング前の意図) が後勝ちで永続化されてしまう**。すなわちタイムアウト → ロールバックしたつもりが、後から元の goroutine が完了してロールバックを上書きするデータ巻き戻し。 |
| 再現条件 | NFS 等で AtomicWrite が長時間ハングし、`stateSaveTimeout=30s` (`L16`) を超えた後にハング解放。 |
| 推奨アクション | `saveStateWithContext` をブロック内で `sync.Once` + 「最終勝者」フラグ (`atomic.Int32`) で保護し、タイムアウト後に goroutine が完走しても永続化しない (temp file を unlink して終了する) ようにする。あるいは `saveFn` 内で ctx.Done を確認するフックを差し込む (現状の `SaveState` は ctx を受け取らない設計なので非自明)。 |

### Finding F-2 (該当なし根拠)

`internal/daemon/lease/manager.go:125-149` の `acquireLease` は dispatch 時に `LeaseEpoch` を +1、`extendLeaseExpiry` (`:176-184`) では epoch 不変、`releaseLease` (`:152-168`) でも epoch 不変、という maestro.md の lease epoch ライフサイクル仕様と完全に一致している。`task_heartbeat_handler.go:196` の `ExtendTaskLease` 呼び出しと `result_write_phase_a.go:432` の Phase A クリアは別経路だが、両方とも task 構造体のロック (`scanMu.RLock` + `queue:{worker}`) で直列化されている。失効と heartbeat の競合は heartbeat 側で `LeaseEpoch` 一致を検証するため (内部 `LeaseManager.ValidateEpoch` 系)、stale heartbeat は fencing_reject となる。F-1 以外の lease 関連レースは確認できなかった。

---

## g. 異常終了時の整合 (crash recovery)

### Finding G-1 (Finding C-1 と複合)

| 項目 | 値 |
|---|---|
| カテゴリ | 復旧整合 |
| 重要度 | High |
| 場所 | `internal/daemon/state_recovery.go:41-96` + `internal/plan/state.go:172` |
| 現状の問題 | `recoverStateDir` は corrupted state file を `.bak` から復元 (`L74-99`) し、復元時に `LeaseEpoch` を **既存値の最大値 + 1** にクランプする (`L186-202` の ORC-3)。しかし `DeleteState` が `.bak` を残置するため、同一 commandID で再作成された状態が破損したケース (Finding C-1 の経路) では、**前世代の `.bak` が ORC-3 クランプ対象として参照され、本来到達不能な lease_epoch を再注入する**。つまり (1) 状態 X 作成 (epoch=10) → (2) DeleteState → (3) 同じ commandID で状態 Y 作成 (epoch=1) → (4) Y の YAML 破損 → (5) 復元時に `.bak` (X の epoch=10) を読み、Y の epoch を 10+1=11 に強制押し上げる。Worker が保持する epoch=1 は永久に stale となり、その時点で in_progress の Y タスクは fencing_reject ループに陥る。 |
| 再現条件 | C-1 と同条件 (commandID の意図的・偶発的再利用 + recover タイミング)。 |
| 推奨アクション | C-1 の修正 (`DeleteState` で `.bak` を消す) で根本解決する。加えて `recoverStateDir` で `.bak` の `created_at` / `command_id` を YAML 中身でも検証し、現行状態の commandID と不一致なら `.bak` を無視する。 |

### Finding G-2 (該当なし根拠)

queue 側の crash recovery については Finding C-2 で言及済み。Worker クラッシュ後の状態回復は `LeaseManager` の TTL 失効 → queue scan による release → 再 dispatch 経路で確実にカバーされる (maestro.md「lease_epoch ライフサイクル」記載通り)。`autoRecoverAfterResolution` (内部 reconcile 群) も状態と queue の整合を再構築する。Critical な recovery バグは G-1 以外確認できなかった。

---

## 補足: 確認した規約整合点

- ロック順序: `internal/daemon/doc.go:13-72` の Canonical lock order と `internal/plan/queue_locks.go:73` の queueKeyOrder は整合。
- Lease epoch ライフサイクル: `internal/daemon/lease/manager.go` 全体は maestro.md の仕様 (dispatch +1 / heartbeat 不変 / release 不変 / 失効後再 dispatch +1) と一致。
- 状態遷移ガード: `internal/model/status.go` の `validCommandTaskQueueTransitions` / `validTaskStateTransitions` / `validPhaseTransitions` を経由する書き込み (`applyTaskDispatchResult` 等) は適切に検証している。バイパス箇所は B-1 / B-2 / A-2 で列挙済み。
- 三段階 reconcile: `internal/daemon/reconcile/r1_result_queue.go:107-202` の snapshot→release→reacquire パターンは推奨ベストプラクティスであり、D-1 の修正テンプレとして再利用可能。

## レビュー時の不確実性

- F-1 の goroutine リーク → ロールバック巻き戻しは **コード読解上確実だが、動的再現は未実施**。stateSaveTimeout=30s 超過の fs ハングが発生する環境でのみ顕在化するため、生産負荷では稀。
- C-2 の queue YAML 破損経路は **推定** (実害再現は未確認)。`AtomicWriteRaw` は安全側に倒した実装だが、disk full / NFS 異常時の挙動はベンダー依存。
- D-1 はコードレビュー結果として確実 (state→queue 順を取る経路が現に存在する) だが、現時点では **デッドロック相手が存在しないため実害ゼロ**。将来の追加コードに対する防御として Critical 評価とした。
