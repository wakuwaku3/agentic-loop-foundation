# 0027: provider/GitHub枯渇時のclaim停止・attempts保護・回復根拠の表示を、既存のpool markerへ薄く積み増しする

- 状態: 採用
- 日付: 2026-08-19

## 背景

Issue #158は、2026-08-13に17分で9件のIssueが連続closeしたカスケード（枯渇イベント下でretry予算を焼き尽くした事象）を受け、[ADR 0012](0012-provider-pool-fallback.md)（Issue #155）が実装した pool 単位の枯渇検知・フォールバックを前提に、残る4つの穴を塞ぐことを求めた。調査の結果、ADR 0012の実装は次を既に満たしていた（テスト`tests/test-agentic-loop.sh`の"Issue #158/#130"regressionが実証）。

- 枯渇の事後検知（`agent_result_is_pool_exhausted`）とpool markerによるclaim停止（`exhaustion_note_pause`）は、provider種別を判定条件に含まない。claude/opencode/codexいずれの枯渇でも同じ経路で該当poolがmarkされ、全pool枯渇時はclaim全体を止める。
- worker内で枯渇を検知したstage失敗は`clear_attempts`でattemptsを消費しない。

残っていたのは次の3点で、本ADRが扱う。

## 決定

### 1. 使用率を実測できないproviderの回復は、固定cooldownの無限反復ではなく指数backoffにする

`agent_provider_usage_percent`（`agent.sh`）にcaseがないprovider（現状claude）のpoolは、resume_epoch経過後の再exhaustedを実測できず、常に「読めないので1回だけ再試行を許す」経路（`agent_pool_exhausted`の最終分岐）に落ちる。この再試行が再び枯渇を報告すると、`agent_mark_pool_exhausted`が毎回同じ`EXHAUSTION_PAUSE_SECONDS`（1800秒）でmarkし直し、「30分ごとに再開→即失敗」を無期限に繰り返す。

これを`STATE_ROOT/pools/<pool>/streak`（連続re-exhausted回数、Git管理外・数値のみ）で数え、providerが具体的なreset時刻を示さない再mark時は`EXHAUSTION_PAUSE_SECONDS * 2^(streak-1)`（`EXHAUSTION_BACKOFF_MAX_SECONDS`=6時間で上限）へ延ばす。streakは次の3つの「本物の信号」でのみリセットする: providerが具体的なreset時刻を示した（`agent_parse_reset_epoch`が成功、根拠=reset）、使用率実測で回復を確認した（根拠=probe）、そのpoolでのstageが実際に成功した（`run_stage_candidates`の成功return）。実測が使える場合の「まだ枯渇」判定（`agent_pool_usage_still_exhausted`）はそれ自体が実測に基づく本物の再確認なので、そちらは従来どおり固定`EXHAUSTION_PAUSE_SECONDS`のまま据え置く（実測がある限り、backoffで確認間隔を伸ばす理由がない）。

### 2. crash/hang timeoutが、枯渇と相関するときはattemptsを消費しない

`enforce_worker_timeout`（ハングkill）と`recover_expired`（lease期限切れ／急死）は、原因がprovider枯渇による無応答/繰り返しエラーであっても、要求の実装が難しいために無限ループしたのであっても区別せず、同じ`agent:failed`エスカレーション経路でattempts予算を消費していた。真の原因切り分けは（プロセスが強制終了される時点では多くの場合結果ファイルが未確定なため）確実には行えないが、キル/回収の瞬間にいずれかのpoolが枯渇marker中であるという事実は、無応答/繰り返し失敗がprovider側の枯渇と相関している強い代理指標になる。

そこで、`agent_any_pool_marker_active`（新設、`agent_all_pools`の各poolに対し既存の`agent_pool_marker_active`を走らせるだけの読み取り専用チェック）が真のときに限り、両経路は`clear_attempts`を呼び、`agent:failed`を経由せず直接`agent:queued`へ戻す（worker内の枯渇検知パスと同じ着地点）。枯渇markerがなければ、これまでどおりattemptsを消費してMAX_ATTEMPTS到達でparkする経路を変えない——本物のハング（要求側の問題）まで無制限に再試行させると、park（人間トリアージ）に到達しなくなるため。

### 3. statusに回復予定時刻と根拠を表示する

`agent_pool_basis_file`（`reset`/`probe`/`backoff`のいずれかを保存、`agent_mark_pool_exhausted`・`agent_pool_exhausted`の各分岐が書く）を新設し、`status`のテキスト出力（`pool=<pool> 枯渇（回復予定=…, 根拠=…）`）と`--format json`の新設`pools`配列（`pool`/`exhausted`/`resume_at`/`basis`）の両方で表示する。marker自体の1行フォーマット（`resume_epoch`のみ）は既存テストの`cat`前提を壊さないよう変更しない。

## 安全性

- 新設state（streak、basis）はGit管理外・数値または列挙値のみで、key/tokenや自由文を保存しない。
- `agent_any_pool_marker_active`はmarker fileの読み取りのみで、追加のGitHub/usage API呼び出しを発生させない。
- 指数backoffの上限（6時間）は「安全側（長めのbackoff）に倒す」という要件を満たしつつ、無期限に再試行を諦めることはない。
- crash/timeoutの`clear_attempts`はpool markerが実際に存在するときだけ発動するため、本物のタスク側ハングに対する既存の保護（MAX_ATTEMPTS→park）は変わらない。

## 帰結

- `bin/agentic-loop:55`近傍に`EXHAUSTION_BACKOFF_MAX_SECONDS`（readonly、6時間）を追加。
- `agent.sh`: `agent_pool_basis_file/get/set`、`agent_pool_streak_file/get/clear/bump`、`agent_any_pool_marker_active`を新設し、`agent_pool_exhausted`・`agent_mark_pool_exhausted`・`run_stage_candidates`の該当分岐から呼ぶ。
- `worker_state.sh`: `enforce_worker_timeout`・`recover_expired`に`agent_any_pool_marker_active`分岐を追加。
- `status.sh`: `status_pool_basis_label`・`status_pool_pause_detail`を新設し、テキスト/JSON双方に反映。
- 分解PRは出さない。marker/streak/basisのstate機械と`status`表示は同じ小さな契約に密結合しており、独立mergeすると「新しいbasis表示だけが先に出て中身のmarkerがまだ書かれない」といった不整合な中間状態を生むため。
