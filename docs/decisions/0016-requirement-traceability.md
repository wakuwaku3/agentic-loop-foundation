# 0016: 要求・変更・検証のトレーサビリティをPR本文の自己申告＋GitHub再観測で記録する

- 状態: 採用
- 日付: 2026-08-18

## 背景

Issueの受け入れ条件が、実際にどの変更・どの検証で満たされたかは、mergeされた瞬間から追跡不能になっていた。squash mergeで個々のcommitは失われ、PRやIssueのfree-formなコメントは検索・検証の単位として弱く、後から「この要求は本当に実装され、検証されたのか」「この変更はどの要求のためか」を確認する手段がない。

## 決定

### 記録の正本はPR本文の`agentic-loop:traceability` fenced JSON blockにする

workerはPR本文へ、受け入れ条件ごとの状態・検証方法・対象変更・対象checkを対応付けるJSON blockを書く。commit本文やIssueコメントを正本にしない。理由:

- PR本文はGitHub上でPR自体のライフサイクル（`pulls/N`）に紐づき、squash mergeでもmerge後も1つのAPI呼び出しで確実に取得できる。commitメッセージはsquash mergeで1つに畳まれ、個々の対応関係が失われる。
- コメントは複数投稿されうるため「どれが最新か」の曖昧さがある。PR本文は1PRにつき1つで、上書き編集される前提が既に他のGitHubワークフロー（review, checks）と一致する。

### criterionの識別子は、行番号でもcommit SHAでもなく、正規化テキストのsha256先頭8桁にする

`ac-<8桁hex>`という識別子は、Issue本文の受け入れ条件の該当行を`trace_normalize_criterion`で正規化（箇条書き記号・チェックボックス・連番・前後空白を除去し空白を1つに圧縮）した文字列のsha256から導く。

- **行番号にしない理由**: Issue本文は編集され、箇条書きの順序も変わる。行番号は安定しない。
- **commit SHAにしない理由**: squash mergeで個々のcommit SHAは失われ、resumeやrebaseでも変わる。PRのhead SHAやmerge commit SHAはcriterion粒度の対応を表現できない。
- 文言そのものを正規化してハッシュ化することで、**箇条書きの再配置やチェックボックスの追加では識別子が変わらず、文言の実質的な変更だけが新しい識別子を生む**。これが「条件変更」の検出原理そのものであり、`trace_validate_coverage`は現在Issue本文から導出できる全識別子がrecordに（`status`を問わず）含まれていることだけを要求する。文言変更で生まれた新識別子をrecordが含まなければ`criteria-missing`で失敗する。

### recordの自己申告を一切信用せず、GitHubの観測結果と照合する

`trace_evaluate`は、recordが自ら主張する`checks[].result`や`changes[].path`を鵜呑みにしない。

- `checks[]`は、PRのhead commitに実際に付いた`check-runs`の最終`conclusion`と厳密一致するときだけ認める（名前`check`＝`devbox run --pure check`全体は例外的に自己申告を許すが、これは対応するcheck-runが存在しない共有entry pointを指すためであり、個別のcheck名は必ず観測と一致させる）。
- `changes[].path`は、PR自身の`pulls/N/files`（変更ファイル一覧）に実在するpathだけを認める。

これにより、providerが「CIを実行した」「このファイルを変更した」と書くだけでは完了条件を満たせず、GitHubに実際に記録された事実だけが証拠になる。この非対称性（recordは主張のみ、判定は観測のみ）が本機能の安全性の中心である。

### squash mergeではPRのhead SHAとmerge commit SHAを区別する

`check-runs`はPRのhead commit（feature branchの最終commit）に付き、squash mergeでmainに乗るmerge commitには付かない。`trace_gate`はcheck-runsの参照に必ずhead SHAを使い、marker表示用の`merge_commit=`欄にはmerge commit SHA（squashなら別のSHA、それ以外はhead SHAと同一）を別に保持する。両者を混同すると、squash mergeを使う運用では常に`evidence-mismatch`になる。

### 3つのmode（`require` / `warn` / `off`）と既定値

`.agentic-loop.toml`の`queue.traceability`で選択する。

| mode | 動作 |
| --- | --- |
| `require` | record不在・検証失敗時、Issueを`agent:failed`にし**closeせずworktree/branchを保持**する。最も厳格。 |
| `warn` | 同じ検証を行うが、失敗しても完了処理は継続する。失敗時はIssueへ助言commentを1件投稿する。 |
| `off` | 評価を一切行わない。追加のREST呼び出しは発生しない。 |

新規導入のFoundation配布物（`.agentic-loop.toml`、`scripts/upgrade/migrations/0002-traceability-config.sh`）は`warn`を書き込む。`bin/agentic-loop`自体のcode側default（設定keyが存在しない場合のfallback）は`off`のままにする。これは、既にqueueが稼働している既存の導入先が本Foundation revisionへ更新した瞬間、PR本文の規約をまだ知らないworker実行が無警告で`agent:failed`になったり、突然advisory commentが増えたりしないようにするためである。`warn`はqueueを詰まらせない（completionをblockしない）ため、既定の出荷値として選び、記録が定着したら`require`へ運用者が明示的に切り替える。

`require`はopt-inであり、切り替えた場合、record不正時にIssueをcloseしない・worktree/branchを保持するという安全側の失敗にする（誤って要求の証跡なしに完了扱いされることを防ぐが、進行中の作業を破棄しない）。

## 秘密の扱い

recordは`.agentic-loop/guard-secrets.sh --text`でスキャンし、`SECRET_PATTERN`に一致すれば`secret-like`で拒否する。guardスクリプト自体が実行できない場合は`guard-unavailable`で同様に拒否する（fail-closed。「スキャンできなかったので通す」を選ばない）。サイズは8KiB上限、`reason`は300文字以内で改行・backtickを禁止し、log全文やプロンプトそのものを記録できない形状に制限する。

## 追加費用

既定の`off`では追加費用ゼロ（GitHub API呼び出しが1本も増えない）。`warn`/`require`では、Issue完了1件あたり最大4回のREST(core)呼び出し（PR本文取得・check-runs取得・変更ファイル一覧取得・verdict comment upsertのGET/POST/PATCH）が増える。GraphQLは一切使わない。1時間あたりのREST(core)予算(5000)に対し無視できる増加量である。

## 対象外

- recordの自動生成・自動修正（providerが自分で書く前提。schema検証に失敗したら`warn`はcompletionを継続しつつ助言、`require`は失敗させるのみで、Foundation側からPR本文を書き換えることはしない）。
- Issueの受け入れ条件を持たない（見出しが無い）レガシーIssueへの遡及的な適用強制。この場合`trace_derive_criteria`は0件を返し、`trace_validate_coverage`はrecordの存在だけを要求する（緩和されたfallback）。
