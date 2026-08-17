# ADR 0013: bin/agentic-loop の機能単位モジュール分割

## 決定

公開CLI・hook・状態形式（`.git/agentic-loop`）・install/upgrade配布経路を変えずに、`bin/agentic-loop`（単一bash約5,000行・260関数超）の実装を機能単位のモジュールへ分割する。

- 公開入口は引き続き**`bin/agentic-loop` のみ**。shebang / `set -euo pipefail` / runtime PATH復元、共通定数と既定値、固定順の `source`、`usage`/`main` のcase dispatchを持つ薄いラッパへ縮退する。
- 実装本体は **`bin/lib/agentic-loop/*.sh`** に置く。各モジュールは**関数定義と変数初期化のみ**を持ち、source時点でI/O・network・lock・プロセス生成を一切行わない。副作用は `main` から呼ばれるコマンド実行時だけに発生する。
- `source` 順は依存の一方向に固定し、入口に明示的に列挙する（glob展開に依存しない）。
- モジュール境界は機能単位（config/api/agent/setup/service/status/doctor/upgrade/metrics/project/dispose/worker_state/dependency/scope/supervisor/worker）で、**機械的な移動＋source配線のみ**を行う。アルゴリズム・状態schema・CLI契約・provider呼び出し意味論は変更しない。

## 理由

単一5,000行ファイルでは、ほぼ全ての変更が同じ1ファイルに触れるため並列worker間のscope競合（`paths=bin/agentic-loop`）が常に直列化を強制する。またworker・diagnoseプロンプトが不要な全関心の関数群を読み込むためコンテキスト消費が大きい。機能単位に分離すれば、変更宣言を `paths=bin/lib/agentic-loop/status.sh` のようにモジュール単位で行え、独立した変更が同一ファイル集中で直列化されずに済む。

言語書き換えや設定schema変更はしない。bashのままで分割するのは、(1) 既存のE2E（fake ghを含む）がそのまま振る舞いの回帰検証として使える、(2) install/upgrade・systemd・`setsid "$0" _worker` の自己再exec契約が入口path不変のまま保てる、(3) 静的検証（`bash -n`/shellcheck）を全モジュールへ適用できるため。

## 安全性

- 入口は公開path・権限・systemd unit名を維持し、`$0` 経由の自己再exec（`nohup "$0" _service` / `setsid "$0" _worker`）は入口を指し続ける。
- モジュール間はグローバル変数で結合する。入口のpreambleに共通定数・既定値が集中し、モジュールはそれらを読む。分割によりshellcheckのSC2034/SC2153（クロスモジュール変数）とSC1091（source追跡）が新たに発生するため、必要なファイルに限定してdisableする。
- 配布はmanifestのfile単位hash方式に合わせ、`SHARED_FILES` へ全モジュールpathを明示列挙する。upgradeは既存のpath単位atomic writeがそのまま各モジュールへ適用される。rollbackは旧単一ファイル構成へ戻せる。
- 検証は既存の共通入口 `devbox run --pure check` で行う。`scripts/lint.sh` のシンボル不変条件は入口＋全モジュールを対象に走査するよう拡張する。

## 帰結

- 運用コマンド・install/upgrade・`.agentic-loop/diagnose-codebase.sh`（`_pick-tier`）の呼び出し契約は不変。
- 以降の実装変更は、触るモジュールのpathをscope宣言に書くことでモジュール単位の並列化が可能になる。
- lintは全モジュールの存在・source配線・`bash -n`・shellcheckを検証する。
- 本ADRとモジュール群はFoundationのSHARED_FILESとして配布される。
