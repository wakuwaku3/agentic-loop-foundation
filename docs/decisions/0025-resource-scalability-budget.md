# 0025: 有限資源を前提とした処理量設計とスケーラビリティ検証を開発ループへ組み込む

- 状態: 採用
- 日付: 2026-08-19

## 背景

Supervisor/workerの実装は、rate limit reserve（`graphql_reserve`/`core_reserve`）やretry上限、worker timeoutといった**安全弁**を既に備える（ADR 0003、ADR 0006）。しかし安全弁は「壊れて止まる前に検知する」ための仕組みであり、「通常経路がそもそも入力規模に対して効率的か」を検証する仕組みではない。実際に棚卸しした結果、`preflight.sh`の承認token検索は1 Issueの全comment（本Issue自身で175件超）を毎回`--paginate`走査し、`flaky.sh`の修復Issue重複検出はrepository全体の全open/全closed Issueを毎回走査していた（それぞれIssue #197、#198として追跡）。これらは安全弁の範囲内（rate limitやtimeoutに達していない）で動作し続けるため、既存の検証だけでは検出できない。

## 判断

### plan段が処理量モデルを自己申告する（`agentic-loop:workload`）

`agentic-loop:preflight`（ADR 0020）・`agentic-loop:traceability`（ADR 0017）と同じ型を踏襲し、plan段が外部I/O・pagination・探索・retry・並列処理を追加/変更する場合に限り、処理量モデル（1操作あたりの呼び出し回数、入力規模Nに対する増加率、停止条件、再取得回避の可否、idle/障害/複数host時の非増幅、検証方法）を`agentic-loop:workload` fenced JSON blockで宣言する。無関係な変更（外部I/Oを一切追加・変更しない）は`external_io: "none"`の1行で済み、負担をかけない。

`agentic-loop:preflight`のrecordへ相乗りしなかった理由は、preflightが**承認**gate（人間の判断を要求する）だからである。効率の問題を無条件に承認要求へ昇格させると、Issue #157/#158で問題になった「過剰に人間を待たせるgate」を再現する。`workload`は既定で承認を要求せず、`require`でもmissing/invalidを`agent:needs-input`へ落とすだけで、`preflight`のようなtoken承認機構は持たない（再planでの再宣言だけで解除できる）。

### 静的検査は`scripts/lint.sh`ではなく`bin/lib/agentic-loop/workload.sh`に置く

`scripts/`はINIT_FILES（一度きりseed）であり、`agentic-loop upgrade`で配布先へ更新されない。検出ロジックをSHARED配布のqueue CLIモジュール（`bin/lib/agentic-loop/workload.sh`、`bin/agentic-loop workload`）として実装すれば、Foundationを導入した既存repositoryにも`doctor`経由・`workload`コマンド経由で同じ規律が届く。`scripts/lint.sh`はこのrepository自身の検証入口として、単に`bin/agentic-loop workload`を呼ぶだけにする。

### 固定の数値上限を置かない

「1操作あたりの呼び出し回数はN回まで」のような一律の数値上限は置かない。有限資源の性質は操作ごとに異なり（Issue単位・repository単位・comment単位など）、意味のある基準は操作ごとの根拠と増加率でしか判定できない。数値は各testが操作の実測値として個別に固定し、退行時に落ちるようにする。

### 静的検査（`workload_scan`）の3種類

1. **W1（共通境界の迂回検出）**: `bin/lib/agentic-loop/api.sh`以外で`gh api`を直接呼ぶ行は、`# workload-boundary: <理由>`注釈が無ければ違反とする。既存の迂回のうちREST repositoryエンドポイントに対するもの（`dispose.sh`のcollaborator権限確認、`doctor.sh`のrepository権限確認）は`repo_api`経由へ寄せ、GraphQL・非GitHub host（opencode usage API）・`gh api user`は注釈で宣言する。
2. **W2（無境界paginationの検出）**: `--paginate`を含む行が、同一行に境界filter（`-f labels=`/`-f since=`/`-f sha=`/`-f head=`）を持たない場合、直前行の`# workload-unbounded: <理由> bound=<上限の根拠> [track=#N]`注釈を要求する。既存の無境界走査のうち、per-Issueまたはrepository全体の累積量に比例して増加する2箇所（`preflight_approved`、`flaky.sh`の重複検出）は`track=#N`付きで注釈し、最適化そのものは追跡Issue（#197、#198）へ切り出す。それ以外（label一覧取得、reinstall時の全Issue走査など）は設計上の根拠を注釈として明記する。
3. **W3（集約の退行検出）**: `snapshot_state_rows`を経由せずloop本体の内側でIssue一覧を再取得する行を検出する。現状該当箇所は無いが、将来の退行（N件の入力に対して同じ一覧をN回取得する）を共通入口で機械的に検出するための予防線とする。

## 対象外

- 既存の無境界I/O（W2で検出される2箇所）そのものの最適化。検出機構と追跡導線の提供までを本Issueのscopeとし、最適化はIssue #197・#198へ切り出す。
- rate limit reserve・retry上限・worker timeoutなど既存の安全弁の変更（このADRは安全弁を代替しない設計判断であり、安全弁自体はそのまま維持する）。
- 固定の数値上限を伴うAPI呼び出し回数の一律ポリシー（各操作の増加率testが個別に実測値を検証する）。

## 追加費用

`workload_gate`はplan出力ファイル（ローカル）からrecordを読むだけで、追加のGitHub API呼び出しはゼロ。gate発火時のみ既存の`comment_post`経由で1件のcommentを投稿する。`workload_scan`はローカルのソースファイルを読むだけで、GitHub API呼び出しを一切行わない。追加費用ゼロ。
