# 0026: ポストモーテムの検出・重複抑止・action item閉ループを既存機構の再利用で実装する

- 状態: 採用
- 日付: 2026-08-19

## 背景

Issue #132は、rate limit枯渇のような事故・near missを個別修正だけで閉じると、別機能・別資源・別経路で同じ構造的問題を繰り返すという問題意識から、Agentic Loop自身が学ぶポストモーテムの仕組みを要求した。要件は広く、自動起動、重複抑止、事実/推測の分離、action itemの閉ループ管理、一般化した知見のpolicy/テストへの反映まで及ぶ。

## 判断

### action itemの閉ループはdecomposition/dependencyの既存機構をそのまま再利用する

ポストモーテムのaction itemは、[ADR 0002](0002-github-issue-queue.md)のdecomposition機構と同じ native sub-issue・native dependency（`blocked_by`）を使う。`bin/agentic-loop postmortem link ISSUE ACTION_ISSUE...`は`decomposition_materialize`が親Issueに対して行う操作（sub_issues POST、dependencies/blocked_by POST、lease解放、`agent:blocked`への遷移）と同じ手順を、action item専用に切り出したものである。これにより:

- action itemの完了・未完了判定は`dependency.sh`の`dependency_satisfied`/`requeue_dependency_ready`をそのまま使う。専用のstatus追跡機構を新設しない。
- action itemがすべて閉じると、既存の`requeue_dependency_ready`が追加コードなしでポストモーテムIssueを`agent:queued`へ戻す。そこから先は通常のworker claimと完了処理（`AGENTIC_LOOP_RESULT=completed`）で閉じる。

decomposition（`mode: children`、2〜6件の新規子、共通の統合受け入れ条件）とは意図的に区別する: action itemは既存Issueの再利用を許し、件数上限も異なるため、`agentic-loop:decomposition`のschemaを拡張せず、`postmortem.sh`内の独立した小さい関数として実装した。`worker.sh`のplan/exec制御フロー自体は変更しない（既存Issueの処理経路への影響をゼロにする）。

### 自動起動は、既に構造化されている2つの観測点だけに限定する

起動基準は要件が7項目挙げるが、確実に構造化されたコストゼロの観測点は次の2つだけである。

1. `park_issue`（`worker_state.sh`）: リトライ予算（`queue.max_attempts`）を使い切った時点。既存の閾値をそのまま再利用し、専用の閾値を新設しない。
2. `exhaustion_note_pause`（`agent.sh`）: 全provider poolが利用不可になった最初の遷移（既存の`all-pools-paused`マーカーの生成タイミングに相乗り）。

「長時間停止」「安全装置作動だが回避可能だった」「小規模成功・規模増加で破綻」は、判定に追加の観測・計測基盤（doctor/statusの拡張、負荷試験）を要し、誤検出のコストも高い。本Issueでは自動起動をこの2点に絞り、残りは`bin/agentic-loop postmortem create`による明示要求の対象として[postmortem policy](../policies/postmortem.md)に文書化し、判定基準の設計は将来Issueへ委ねる。これは要件の「少なくとも次の事象を候補として検出・起票できる基準を定義する」を、自動化2点+明示要求5点として満たす選択である。

### 重複抑止はfingerprint+open検索、時間窓は使わない

`postmortem_fingerprint`は`kind:subject`のsha256先頭8桁。作成前に`label:postmortem`かつ本文に一致するfingerprintマーカーを含む**open**のIssueを検索し、あれば新規作成せず既存Issueを再利用する。closedな同一fingerprintは再発とみなし新規作成を許す。時間窓（dedup_window）ではなく状態（open/closed）で判定するため、「同じ問題が直っていない間は1件だけ」「直った後の再発は新しいIssue」が自然に成り立ち、窓の長さを設定する閾値を1つ減らせる。

### 有限資源ポリシーへの適合

`[postmortem].max_auto_created_per_day`（1〜20、既定5）が、自動作成の総量を境界付ける唯一の新設閾値である。この上限は`postmortem_consider_trigger`（自動起動2経路）だけに適用し、明示要求（`postmortem create`）は妨げない。理由: 上限は「気づかれない反復・枯渇が無制限にIssueを生む」ことを防ぐためのものであり、利用者やworkerが意図して行う明示要求まで拒否すると、要件の「利用者またはworkerが明示的にポストモーテムを要求できる」を満たせなくなる。dedup検索は`label:postmortem`かつ`state=open`のIssue一覧1回のREST呼び出しに留め、無制限な履歴取得は行わない。`postmortem link`のREST呼び出し回数はaction item数に線形（decomposition_materializeと同様）。

### worker.shの終端処理に、postmortem専用の分岐を1箇所追加する

当初「worker.sh本体の変更はゼロ」を狙ったが、実測により成立しないことが分かった。`worker.sh`の完了判定（`AGENTIC_LOOP_RESULT=completed`）は、対応するbranchのmerge済みPRの存在を前提にする。`postmortem link`はIssueを`agent:blocked`へ移すだけでPRを作らないturnであり、`postmortem complete`（後述）が完了させるturnもaction itemの検証確認だけでPRを作らない。両方とも、既存の「merge済みPRが見つからなければfailed」という既定に落ちると、`agent:blocked`を`failed`で上書きしたり（閉ループの前半が壊れる）、action item完了後の自動再キューが繰り返し`failed`→`agent:parked`に陥ったりする（ポストモーテム機構自身が自分自身のポストモーテムを誘発する自己破壊ループになる）。

これを避けるため、`bin/lib/agentic-loop/postmortem.sh`は`STATE_ROOT`配下に1turn分の意図を記すマーカー（`link`/`complete`）を書き、`worker.sh`の終端処理（providerループ直後・`lease_release`の後、および完了判定の内部）はこのマーカーを読む分岐を1箇所ずつ追加する。`STATE_ROOT`は`--git-common-dir`基準のため、専用worktree内から実行された`bin/agentic-loop postmortem link/complete`（providerのexecターン内）と、Supervisor自身のworker()プロセスは同じ絶対pathを共有する。`link`マーカーは既に行われた状態遷移（`agent:blocked`）をそのまま尊重して即returnし、`complete`マーカーは「branchがdefault branchより先に進んでいない」ことを`cleanup_completed_worker`の既存oid一致チェックで確認したうえでのみ完了・close処理を行う。分岐で先行commitを検出した場合は、そのターンのマーカーを消費したうえで既存の merge済みPR前提の完了経路へフォールバックする（未mergeの作業を黙って捨てない）。この2箇所以外のplan/exec/replan/decomposition/preflight/traceability制御フローには一切触れていない。

### `postmortem complete`のgateが、完了条件の機械的強制点になる

要件「ポストモーテム本文だけをcloseしてaction itemを未追跡にしない」を文書の努力目標にとどめず、`bin/agentic-loop postmortem complete ISSUE`が次をすべて満たす場合だけ完了マーカーを書く: (1) 紐付いた全action item（native + body `Blocked by:`の合併、`dependency_satisfied`で判定）が完了・検証済み、(2) 本文に雛形のプレースホルダ（`（記入してください）`）が残っていない、(3) 残余リスク節が空でない。いずれか不成立なら非ゼロで終了し、日本語で理由を返し、Issueをcloseしない。これは新しいgate専用の仕組みではなく、既存の`dependency_satisfied`と単純な文字列検査を組み合わせただけであり、新規の外部呼び出しは増やさない。

### 秘密情報の扱い

`postmortem_secret_scan_clean`は`trace_secret_scan_clean`と同じ`.agentic-loop/guard-secrets.sh --text`をfail-closedで再利用する。scan不能または秘密様の内容を含むevidenceは、Issue本文に一切転記せず「evidenceは秘密走査により省略」の注記だけを残す（作成そのものは止めない）。

## 対象外・残作業

- 「長時間停止」「安全装置作動だが回避可能」「小規模成功・規模増加で破綻」の自動検出。
- ポストモーテムからpolicy/テスト/観測性への反映を強制する機械的gate（現状はskillの手順とlintの個別チェックのみ）。
- resource-exhaustion以外の有限資源（disk等）の自動分類。
- `postmortem complete`と同種の「PRを持たない検証のみの完了経路」を、decomposition親Issue（`decomposition_materialize`が作る親）へも一般化すること。親Issueも全子Issue完了後は統合検証だけを行いPRを作らない場合があり、構造的には同じ穴を抱えているが、本Issueのscopeでは`postmortem.sh`に閉じた変更のみを行い、`worker.sh`の分岐もpostmortem専用のマーカーだけを見る形にとどめた。汎化は別Issueで行う。

これらは followup Issue で追跡する（本PRのポストモーテムIssue本文に記載）。

## 帰結

- 新規の待機・claim機構を増やさない。`agent:blocked`の意味を変えず、既存の依存関係pollに相乗りする。
- 追加費用ゼロ。追加のREST呼び出しは、自動検出2箇所それぞれ最大1回のdedup検索と、`postmortem link`のaction item数に比例した呼び出しのみ。
- `worker.sh`の変更は終端処理への2箇所のマーカー分岐に限定される（plan/exec/replan/decomposition/preflight/traceability制御フローには触れない）。マーカーはpostmortem経路でしか書かれないため、既存Issueの処理はこの分岐を素通りし、挙動は変わらない。
