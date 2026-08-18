# 0020: workerが着手する前に、変更影響とリスクをpreflightとして判定する

- 状態: 採用
- 日付: 2026-08-18

## 背景

AGENTS.mdの不変条件「破壊的・不可逆または重大なコストを伴う判断は、実行前に明示的な承認を得る」は、これまで散文の指示だけで、機械的な強制点を持たなかった。planとexecの間には、scope競合回避（scope.sh）や親子分解（`agentic-loop:decomposition`、worker.sh）のような機械可読gateが既に存在するが、「このIssueは危険か」を判定してexecへ進む前に止める仕組みは無かった。

## 判断

### plan段が10軸固定のリスクを自己申告する

`agentic-loop:preflight` fenced JSON blockを、`agentic-loop:scope`・`agentic-loop:decomposition`と同じくplan出力の一部として要求する。リスク軸は`security`・`confidentiality`・`integrity`・`availability`・`data_migration`・`external_environment`・`cost`・`compatibility`・`release_deploy`・`rollback`の10軸で**固定**し、record は毎回全軸をちょうど1回ずつ申告しなければならない（欠落・重複・未知軸名は`schema-invalid`）。これにより「都合の悪い軸を黙って書かない」ことを構造的に不可能にする。各軸の水準は`low`/`medium`/`high`/`unknown`の4値。`low`以外は200文字以内・改行/backtick禁止の`reason`を必須にし、`unknown`はさらに`missing`（判定に必要な不足情報）を必須にする。これが要件「判定不能時に根拠と不足情報を示す」の強制点であり、`unknown`を`low`と同じ扱いにする経路は存在しない。

### recordは自己申告であり、単独では信用しない

`agentic-loop:traceability`（ADR 0017）が「recordの主張とGitHubの観測を照合し、観測を優先する」のと同じ非対称性を採る。`bin/lib/agentic-loop/preflight.sh`の`preflight_signal_class`は、`.agentic-loop/capabilities.toml`の`[[protected]].change_requires = "approval"`宣言と、Issueの変更scope（`scope.sh`が既に維持しているキャッシュ、追加のGitHub API呼び出しはゼロ）だけから、追加のAPI呼び出しゼロで`signal`（`approval`/`medium`/`none`）を導出する。recordが`low`と自己申告していても、signalが`approval`（宣言scopeが`change_requires=approval`のpath、または`paths=*`に重なる）を示せば`signal-mismatch`として扱う。**recordを一切出さないことでgateを回避できない**ことがこの設計の核心である: record不在（`missing-record`）でもsignalが`approval`ならやはり`signal-mismatch`になる。

### verdictは5値、modeは3値

| verdict | 条件 |
| --- | --- |
| `autonomous` | 全軸`low`/`medium`、triggerなし、signalとも矛盾しない |
| `approval-required` | いずれかの軸が`high`、または`approval.triggers`に1件以上 |
| `undetermined` | いずれかの軸が`unknown` |
| `invalid` | record欠落以外の理由で検証できない（schema不正・サイズ超過・secret様・`approval.required=false`なのに`high`/triggerがある`claim-mismatch`） |
| `signal-mismatch` | record（または record 不在）がsignalと矛盾する |

`.agentic-loop.toml`の`queue.preflight`（`require`/`warn`/`off`、traceabilityと同型）:

| verdict | `off` | `warn` | `require` |
| --- | --- | --- | --- |
| `autonomous` | 続行 | 続行（監査commentを残す） | 続行 |
| `approval-required` / `undetermined` / `signal-mismatch` | 続行 | **gate** | **gate** |
| `missing` (record不在・signalも無害) | 続行 | 助言commentのみ・続行 | **gate** |
| `invalid` | 続行 | 助言commentのみ・続行 | **gate** |

このrepository自身は`warn`を出荷する（traceability・pause-controlと同じ理由: 導入直後に自己のqueueを自己lockさせないため）。`require`への切替は運用判断であり、[preflightの運用文書](../operations/preflight.md)に手順を書く。

### gate先は既存の`agent:needs-input`を再利用する

新しいLabelや新しい終端状態は作らない。gateは`agentic-loop:needs-input`マーカーを含むcommentを1件投稿し（`worker_state.sh`の`requeue_answered`が既存どおり拾える）、Issueを`agent:needs-input`にする。これはADR 0019のpause/abortが`agent:parked`を再利用したのと同じ判断である。

### 承認はrecordの文面ではなく「リスクのenvelope」に対して行う

再planでrecordの文言が変わっても、宣言されたリスク（`low`以外の軸+水準の集合とtrigger集合）が同じなら、既に得た承認は失効しない。`preflight_token`は、issue番号と「`low`以外の`axis:level`」「trigger」をsortしてsha256の先頭12桁を取ったものを使う。record不在でsignalだけがgateした場合は、signalの理由文字列（例: `protected:devbox.lock`）からtokenを計算する（`preflight_signal_token`）。

承認は自由記述のIssue返信では判定しない。`requeue_answered`（既存機構）は`agentic-loop:`で始まらないcommentを人間の返信として検出し、Issueを`agent:queued`へ戻すが、それ自体はrecordの承認にはならない。**承認の正本は`bin/agentic-loop preflight ISSUE --approve --token TOKEN`が投稿する`<!-- agentic-loop:preflight-approved token=... -->`markerだけ**であり、`dispose`/`resume`/`pause`/`abort`（ADR 0010、0019）と同じ`authorized_operator`（`gh api user` → collaborator permission `write|maintain|admin`）による認可を要求する。このCLIは承認markerの投稿に加えて、Issueが`agent:needs-input`であれば`agent:queued`へ戻す（承認だけでは再開せず、別途返信を待つという回りくどい経路を避けるため）。再planしたworkerは同じenvelopeなら同じtokenを再計算し、承認markerを見つければ続行する。

### 実装中のリスク増大には、mergeを機械的に止められない限界がある

execは1 turnでplanの実装からPRのmergeまでを完遂する構造（`docs/decisions/0004`）のため、「merge前に機械的に止める」ことは以下の2層に依存し、完全ではない:

1. **exec promptへの契約**: 承認済みenvelope（またはverdict）をexec promptへ埋め込み、宣言を超える新たなリスクを発見したら実装を進めず`AGENTIC_LOOP_RESULT=needs-input`で終えるよう明示する。
2. **完了確定の機械的backstop（`preflight_reevaluate_diff`）**: `trace_gate`と同じ位置（完了確定シーケンスの直前、`pr-merged`resume経路と通常exec完了経路の両方）で、実際に測定したdiff（`worker_refine_scope_from_diff`と同じ手法）からsignalを再評価する。承認されていない`approval`signalを検出したら、**cleanup・close・completed遷移を行わず**、`agent:needs-input`へ移してworktree/branch/PRを保持する。

つまり2は「merge済みの変更を取り消す」ものではなく、`trace_gate`同様「完了の確定（cleanup+close）を止める防波堤」である。この限界は意図的であり、production環境でexecがGitHubへの書き込みを自律的に行う設計そのものに起因する（悪意ある変更ではなく、宣言を超えて広がった正当な変更を人が確認するための停止点）。

## 対象外

- `.agentic-loop/capabilities.toml`のschema拡張（既存の`[[protected]]`/`[[external_environment]]`/`release.*`で足りる）。
- 新しいLabelや新しい終端状態の追加（既存の`agent:needs-input`を再利用する）。
- execの途中（provider呼び出し中）でのmergeそのものの機械的な阻止（上記の限界を参照）。

## 追加費用

`preflight_signal_class`はローカルの`.agentic-loop/capabilities.toml`と既存のscopeキャッシュだけを読み、追加のGitHub API呼び出しはゼロ。gateが発火した場合のみ、承認marker検索（REST(core) 1回、paginated）とcomment投稿（1回）が発生する。GraphQLは使わない。追加費用ゼロ。
