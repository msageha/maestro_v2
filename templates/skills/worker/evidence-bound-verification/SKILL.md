---
name: evidence-bound-verification
description: self-verify の観測 (実行したコマンド原文 + 出力の要点 + before/after) を耐久証跡ファイル audit/evidence/<task_id>.md として成果物と一緒に残す規律。「動くはず」の主張ではなく観測を記録する
version: "1.0.0"
tags: [worker, verification, evidence, audit, self-verify, observability]
priority: 10
---

# Evidence-Bound Verification

self-verify を「観測 + 耐久証跡」に接地させる規律。証跡ファイルが無い検証は、レビュー・監査の視点では **行われなかったのと同じ** として扱う。

`verify.enabled: false`（通常運用モード）では daemon は検証コマンドを一切実行しない。品質担保は Worker の self-verify だけであり、その self-verify が本当に行われたか・何を観測したかは、耐久ファイルとして残さない限り merge 後に誰も確認できない。

**本規律は daemon の機械 gate ではない。** daemon は証跡ファイルの存在を検査しないし、してはならない。daemon が実機実行する唯一の Strong Signal は `verify.yaml` であり、本証跡はそれを置き換えるものではなく、`verify.enabled: false` 時の self-verify を監査可能にする補完層である。

## 1. 原則: 観測 > 主張

|          | 主張 (assert) — 不十分   | 観測 (observe) — 要求される                            |
| -------- | ------------------------ | ------------------------------------------------------ |
| 実装     | 「実装したので動くはず」 | 変更後にコマンドを実行し、新挙動の出力を得た           |
| バグ修正 | 「原因箇所を直した」     | 変更前に再現させた失敗が、変更後に同じコマンドで消えた |
| テスト   | 「テストを書いた」       | テストを実行し、PASS 件数と exit code 0 を確認した     |
| 回帰     | 「影響ないはず」         | 既存テスト / lint を実行し、変更前と同じ結果を得た     |

記録するのは常に **(1) 実行したコマンド原文（省略なし）、(2) exit code、(3) 判定に効く出力行、(4) before/after の対比** の 4 点である。

## 2. 適用範囲（tier）

| タスク種別                                                    | 証跡形式                                                                                                                          |
| ------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| コード変更を伴うタスク（ソース・設定・スキーマを Edit/Write） | **完全形式**: `audit/evidence/<task_id>.md`（§3-4）                                                                               |
| docs のみの変更                                               | 軽量: 証跡ファイル推奨だが、ビルド影響が無ければ summary への inline 記載で可                                                     |
| pure research / 調査（files-changed 0 件）                    | 軽量: summary に観測を inline 記載。証跡ファイルは**作らない**（read-only タスクに新規ファイルを混ぜない）                        |
| `run_on_main: true`                                           | **inline のみ**（Write/Edit が L2 hook で技術的に拒否されるため、ファイルは作れない）                                             |
| `run_on_integration: true`                                    | **inline のみ**（integration worktree は次の merge 前に `reset --hard` + `clean -fd` で auto-clean され、タスク生成物が残らない） |

inline 記載とは、`--summary` の `[注意事項]` に §4 と同じ要素（コマンド原文 + exit code + 判定に効く出力行）を圧縮して書くことを指す。

## 3. 置き場所と result への紐付け

1. 証跡は `working_dir` 相対の `audit/evidence/<task_id>.md` に置く（`mkdir -p audit/evidence` してから書く）。プロジェクト直下の `audit/` はタスク横断の監査成果物の規約ディレクトリであり、worker worktree 内に書けば daemon の verbatim commit（`git add -A`）→ integration merge → publish の**成果物と同じ経路で main に耐久化**される
2. **gitignore を確認する**: daemon の auto_commit は gitignore を尊重するため、対象プロジェクトが `audit/` を ignore していると証跡は commit されず worktree GC で消える。書き込み前に `git check-ignore -q audit/evidence/<task_id>.md`（読み取り専用コマンド）で確認し、**ignored ならファイルを作らず inline 形式に切り替え、`[注意事項]` に「audit/ が gitignore されているため証跡は summary inline」と明記する**
3. result への紐付け: 証跡ファイルは実変更なので `git status --porcelain` に現れる。`--files-changed audit/evidence/<task_id>.md` に含め、`--summary` の `[注意事項]` に `証跡: audit/evidence/<task_id>.md` の 1 行を入れる。これでレビュー・監査は result entry → 証跡ファイルを辿れる
4. 同一 `task_id` の再配信（attempt > 1）では同じファイルを上書きする。retry タスク（新 task_id）は新しいファイルを作る。他タスクの証跡ファイルには触れない（スコープ外変更）

## 4. 証跡ファイル形式

```markdown
# Evidence: <task_id>

- command: <command_id> / attempt: <n> / date: <YYYY-MM-DD>

## Before（変更前の観測）

<バグ修正なら失敗の再現コマンドと失敗出力の要点。
機能追加なら「その挙動が存在しない」ことの観測。省略する場合は理由を書く>

## Change

<git status --porcelain の出力（= --files-changed と一致）と、変更の 1 行要約>

## After（変更後の観測）

$ <実行したコマンド原文（省略・言い換え禁止）>
exit: 0
<判定に効く出力行のみ。例: "ok pkg/foo 0.41s" / "3 passed, 0 failed">
（全 N 行中 M 行を抜粋）

## Acceptance criteria との対応

- <criterion 1> → After の <どの観測> が充足を示す
- <criterion 2> → ...
```

Before が観測できないケース（新規ファイルのみの追加等）は「Before: N/A（新規追加。<理由>）」と明示する。空欄のまま残さない。

## 5. サイズ・保持方針

- **1 タスク 1 ファイル、目安 100 行 / 8 KiB 以内**。コマンド出力は判定に効く行だけを `grep` / `tail` で抜粋し、生ログの全文貼り付けを禁止する。トリムした場合は「全 N 行中 M 行を抜粋」と明記する。トリムして良いのは出力だけであり、**コマンド原文と exit code は絶対に省略しない**
- 上限を超える観測（長大なテストマトリクス等）は、判定サマリ（件数・FAIL 一覧）に圧縮して収める
- 保持の正本は git 履歴である。merge 後の `audit/evidence/` の蓄積は操作員が定期的に prune してよい（削除しても履歴から辿れる）。Worker 側に保持義務はなく、prune は Worker のタスクではない

## 6. よくある言い訳（Common Rationalizations）

| 言い訳                                                    | なぜ間違いか                                                                                                                            |
| --------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| 「テストは通したので、ファイルに書き写すのは二度手間」    | コンテキストは配信ごとにリセットされる。ファイルに残らない観測は merge 後に検証不能で、レビューは「未検証」として扱うしかない           |
| 「summary に書いたから十分」                              | 完全形式の対象（コード変更）では summary は要約であって証跡ではない。コマンド原文と exit code が無い報告は再現も監査もできない          |
| 「出力を全部貼れば確実」                                  | 逆。生ログの丸写しはサイズ上限を破り、判定に効く行を埋もれさせる。抜粋 + 抜粋率の明記が規律である                                       |
| 「before は取り忘れたが、after が通っているから問題ない」 | before の無い bugfix 証跡は「元から通っていた」と区別できない。取り忘れたなら revert で再現するか、その旨を証跡に明記する               |
| 「daemon が verify してくれるはず」                       | `verify.enabled: false` が通常運用モードであり、daemon は何も実行しない。self-verify とその証跡が唯一の担保である                       |
| 「run_on_integration だがファイルに書いておこう」         | integration worktree の生成物は merge 前 auto-clean で消える。書いても残らず、「証跡を残した」という虚偽の安心だけが残る。inline に書く |

## 7. 危険信号（Red Flags）

- 証跡の After 節に書いたコマンドを、実際には実行していない（書きながら「実行したことにする」）
- コマンド原文を「テストを実行」のような自然言語の言い換えで書いている
- exit code を書いていない / 出力を確認せず「成功したはず」で PASS と書いた
- 証跡ファイルを書いたのに `--files-changed` にも `[注意事項]` にも載せていない（辿れない証跡は無いのと同じ）
- `git check-ignore` を確認せずに書き、ignored なパスに証跡を「残した」ことにしている
- acceptance_criteria の項目のうち、どの観測にも対応しないものが残っている

## 8. 完了チェックリスト（Verification）

- [ ] 適用 tier を判定した（コード変更 = 完全形式 / research・docs = 軽量 / run_on_main・run_on_integration = inline）
- [ ] 完全形式の場合: `git check-ignore` で追跡可能性を確認し、`audit/evidence/<task_id>.md` を §4 の形式で作成した
- [ ] 証跡内の全コマンドは実際に実行し、exit code と出力を観測した（1 つも「実行したことにした」ものがない）
- [ ] before/after が対になっている（欠ける場合は理由を明記した）
- [ ] 100 行 / 8 KiB 目安に収まり、トリムには抜粋率を明記した
- [ ] `--files-changed` に証跡ファイルを含め、`--summary` の `[注意事項]` に `証跡: <path>` を記載した（inline の場合は観測本体を記載した）
- [ ] acceptance_criteria の各項目が、証跡内のいずれかの観測に対応している
