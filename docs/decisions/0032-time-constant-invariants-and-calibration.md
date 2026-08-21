# 0032: 時間定数の不変条件を単一の表にし、実測分布との整合を検査する

- 状態: 採用
- 日付: 2026-08-21

## 背景

Agentic Loopの挙動は10個前後の時間定数（`stall_seconds`/`provider_stall_seconds`/`worker_timeout_seconds`/`lease_seconds`/`pause_grace_seconds`/`poll_seconds`/`poll_max_seconds`/`worker_orphan_grace_seconds`、および`bin/agentic-loop`の`EXHAUSTION_PAUSE_SECONDS`/`EXHAUSTION_BACKOFF_MAX_SECONDS`）の相対関係で決まる。[0031](0031-stage-stall-threshold-and-provider-progress-signal.md)導入時点では、このうち3つの関係だけが`doctor.sh`のインライン`if`文としてwarningになっていた（`provider_stall_seconds`が`worker_timeout_seconds`以上、bandの逆転、`worker_timeout_seconds`が安全下限未満）。しかし:

- これらは`doctor`実行時にしか検査されず、`.agentic-loop.toml`をこれらの関係に反する値へ編集しても`devbox run --pure check`は通っていた。既定値そのものが破壊されても検出できない。
- `bin/agentic-loop`のbash fallback定数（toml未記載時に使われる既定値）と`.agentic-loop.toml`の既定値が独立に編集されて乖離する経路が未検査だった。
- lease/heartbeat（`worker.sh`の`hb_interval=lease_seconds/3+1`）、`pause_grace_seconds`、pollの単調性、枯渇backoffの上下限、`worker_orphan_grace_seconds`の関係は一切検査されていなかった。
- 閾値が実測の所要時間分布とどれだけ整合しているかを機械的に確認する経路が存在しなかった（[0031](0031-stage-stall-threshold-and-provider-progress-signal.md)自体もIssueコメントの実測を人手で読んで既定値を決めている）。

## 判断

### 不変条件を単一の純粋関数にする

`bin/lib/agentic-loop/config.sh`の`timing_invariant_violations()`が、時間定数の値だけを引数に取り、違反を`name<TAB>detail<TAB>remedy`のTSVで返す。GitHub呼び出しも状態書き込みも行わない。検査する関係は9件: `stall_seconds`/`provider_stall_seconds`がそれぞれ`worker_timeout_seconds`未満であること、両者のband順序、`worker_timeout_seconds`の安全下限、heartbeatを1回落としてもleaseが切れない余裕、`pause_grace_seconds`が`worker_timeout_seconds`未満であること、pollのbackoffが単調であること、枯渇backoffの上限がinitial以上であること、`worker_orphan_grace_seconds`が`worker_timeout_seconds`未満であること。

### 3層で使う

1. **静的gate（`_timing-check --committed`、hard fail）**: `bin/agentic-loop`の内部subcommand。`.agentic-loop.toml`だけを読み（`.agentic-loop.local.toml`は無視するので、開発者のlocal overrideが他の開発者の`make lint`を落とさない）、`timing_invariant_violations`に加えて、bash fallback定数と committed tomlの同名キーの一致も検査する（`timing_fallback_mismatches`）。`scripts/lint.sh`の末尾からこれを呼び、違反があれば`make lint`（=`devbox run --pure check`のlint段）が落ちる。
2. **runtime診断（`doctor`、warning）**: 実効設定（`.agentic-loop.local.toml`を含む）に対して同じ関数を評価し、warningとして報告する。doctorはこれらをfailureにはしない（すべて運用者が意図的に選べるtunableな値であり、Supervisorの起動自体を妨げるべきではないため）。既存の2つの警告文言（`設定値: WORKER_TIMEOUT_SECONDS`、`設定値: PROVIDER_STALL_SECONDS`）はテストが文字列一致でgrepしているため、ラベル文字列を保った。
3. **test**: 出荷既定値に対する正常系と、各条件を個別に破る否定系。

### 実測分布との突き合わせ

新しい外部I/Oは追加しない。

- **`doctor`（host-local）**: `events.log`は既に`progress`行を蓄積している（[0005](0005-status-observability.md)、[0031](0031-stage-stall-threshold-and-provider-progress-signal.md)）。`worker_state.sh`の`events_stage_duration_samples()`が、同一subjectの連続する`progress`行の間隔を「departing stageの実所要時間」として復元する（最新`EVENTS_CALIBRATION_MAX_LINES`行のみ、有界）。`worker_timeout_seconds`を超える間隔はhost/timeout起因として除外する。band毎にサンプル数が`CALIBRATION_MIN_SAMPLES`未満なら判定を保留し（成功として報告、警告ではない）、十分なら実測p90が現在の閾値以上のとき警告する。
- **`metrics`（GitHub由来、追加API呼び出しなし）**: 既に算出済みの`DIST_PLANSEC`/`DIST_EXECSEC`から`metrics_calibration_verdict()`が同じ判定を行い、`--format json`に`stall_calibration`ブロック（`stall_seconds`/`provider_stall_seconds`/`plan_p90`/`exec_p90`/`verdict`）を追加する。undersizedなら既存の`warnings`配列に警告文字列を追加する（text/json両方に自動で載る、既存の`label_marker_mismatch`と同じ仕組み）。

## 却下した案

- **`load_config`を書き換えて数値キーmapをtiming用の共有tableに統合する**: `CONFIG_NUMERIC_KEYS`（var↔key）は`load_config`のcase文の内容をミラーしているが、実績のある1行ずつのcase解析を変えるリスクの方が、小さな重複のコストより大きいと判断し、`load_config`自体は変更しなかった。
- **不変条件違反をdoctorのfailureにする**: 既存test（`失敗=0`のassert）が示す方針どおり、時間定数はすべて運用者が意図的にtuneできる値であり、Supervisor起動を妨げるべきではない。lintのhard gateは「committedなdefaultが壊れている」ことだけを止め、運用者のlocal override判断は妨げない。
- **stall実測calibrationを新しいstate fileや新しいAPI呼び出しで実現する**: `events.log`（doctor）と既存の`usage` marker集計（metrics）で十分に導出できるため、新規の永続stateや呼び出しは追加しなかった。

## 帰結

- `_timing-check --committed`が`scripts/lint.sh`のhard gateとして実行され、時間定数間の関係が壊れた状態のcommitは`make lint`（=`devbox run --pure check`）で止まる。
- `doctor`は時間定数間の不変条件と、実測分布とのcalibrationを、いずれもwarningとして報告する。failure数は変わらない。
- `metrics --format json`に`stall_calibration`が追加される（additive、`schema_version`は1のまま）。metricsのREST呼び出し回数は変わらない。
- 配布先repositoryが既に`queue.*`をこれらの不変条件に反する値へ手編集している場合、upgrade後の`make lint`が新たに落ちる可能性がある。是正手順: `./bin/agentic-loop _timing-check --committed`の出力に従って`.agentic-loop.toml`の該当キーを見直す。
