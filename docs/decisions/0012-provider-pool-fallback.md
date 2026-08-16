# ADR 0012: プール・モデルの優先順位付きフォールバックと枠枯渇・回復検知

## 決定

`plan` / `exec` / `diagnose` の各phaseは、`.agentic-loop.toml` で `[[agent.<phase>.tiers]]` により**プール（=サブスクリプション、quota境界）**と**プール内モデル（優先順位）**の2階層を宣言できる。実行時（`agent_pick_tier <phase>`）は、枯渇していない最上位プールの、使用率閾値（`max_usage_percent`）を超えていない最上位モデルを選ぶ。

- 枠の枯渇・回復は**プール単位**で記録する。`STATE_ROOT/pools/<pool>/exhausted`（Git管理外、key/token非保存）に `resume_epoch` を書き、usage実測または固定cooldown（`EXHAUSTION_PAUSE_SECONDS`）経過で削除する。
- グローバルpause（`agent-exhausted`）は**全候補プールが同時に利用不可のときだけ**発動する。部分枯渇では他プールでclaim・実行を継続する。
- プール使用率の実測は、codexが既存のセッションログ（`secondary.used_percent`）、opencode goが `GET https://opencode.ai/zen/go/v1/usage`（`~/.local/share/opencode/auth.json` の `opencode-go` keyで認証）を用いる。読めない場合はfail-open（使用率判定をスキップ）し、回復判定はcooldownにフォールバックする。
- 失敗分類を分割する。quota / 429 / usage limit / `insufficient_quota` / credit balance（および後方互換のため「空result + 非0 exit」）はプール枯渇、`overloaded` やmodel解決失敗は**モデル固有失敗**として同プール内の次モデルへ移す。モデル固有失敗をプール枯渇扱いにしない。
- **同一stage内フォールバック**: プール枯渇を検知したらそのプールをmarkerし、**同じstageのループで次の候補へ`continue`する**（Issueを一度requeueして次claimを待つ必要はない）。全候補が枯渇したときだけ `STAGE_RC=1` でrequeueし、候補が最初から無いときだけ `STAGE_RC=2` でグローバルpauseする。plan段もexec段と同じ `run_stage_candidates` を使う。
- **provider exit codeの保持**: `agent_run_stage` はusage抽出の成否に関わらずproviderのexit statusを返す。Codexのようにusage limitをstderrのみに出し `--output-last-message` を空にするCLIでは、resultが空のときstderrをresultへ折りたたみ、分類matcherが文言を読めるようにする。
- `budget.weekly_reserve_percent`（緊急枠のclaim全体pause）は残す。`max_usage_percent` はプール内モデル降格用で役割を分離する。
- scalar `provider` / `model` / `reasoning_effort` は「暗黙pool=provider名、models 1件、max_usageなし」の1 tierに正規化し、後方互換を保つ。既定のcommitted設定はscalarのまま。

## 理由

利用者契約は openai plus（codex CLI、単一モデル）と opencode go（opencode CLI、複数の同等モデル）で、両者はプール単位でquotaを共有する。従来はphaseごとに固定1組のprovider/modelしか持てず、枠を使い切るとIssue全体がpause・再キューを繰り返していた。opencode goのモデルは消費が桁違い（例: gpt-5.6-luna 約2,050回/5h vs deepseek-v4-flash 約31,650回/5h）なため、プール使用率が閾値を超えたら同じ枠内で安いモデルへ順次降格し、残枠を延命しながら回復で優先候補へ復帰できる構造が要る。

同一プール内モデルは同じ枠を共有するため、枯渇をモデル単位で記録せずプール単位で記録する。`overloaded` を枯渇扱いにすると（従来の `agent_result_is_exhausted` は含んでいた）plus全体が停止するため、モデル固有失敗と分離する。

## 安全性

- usage APIのkey値はログ・Issue・PR・stateファイルへ一切出力しない。doctorはkeyの**存在**だけを検査し、キャッシュは `epoch<TAB>percent` のみ保持する。
- 外部環境の操作は読み取り専用GETのみで、追加課金APIや新規有料serviceは導入しない。
- 使用率が読めない場合のデフォルトはfail-open（claim継続・モデル降格なし）であり、誤ってclaimを止めない。usage APIへの呼び出しは `USAGE_CACHE_SECONDS`（300秒）で間引きし、失敗も同期限で記憶する。
- プールmarkerの回復は「usage実測で回復を確認」→「cooldown経過」の順で行い、誤った早期復帰による枯渇ループは実測が「依然exhausted」を返す限り抑制される。

## 帰結

- 設定は `.agentic-loop.toml`（またはlocal override）でtiersを宣言できる。tiers未設定時は従来と同一動作。
- statusはプール別のpause理由と「次に選ばれるplan/exec候補」を表示する（usage実測はせず、markerと設定からの推論に限定してコストを抑える）。
- doctorはtiersスキーマ（未知provider・空models・不正percent）とopencode go認証keyの有無を検査する。
- `agent_used_providers` は全phase・全tierのproviderを返すため、install/preflight/systemd PATHがフォールバック先CLIも検査・設定できる。
- `.agentic-loop/diagnose-codebase.sh` は `bin/agentic-loop _pick-tier diagnose` で同じ選択契約を利用する。
- 分解PRは出さない。state機械・設定・run_stage・観測が同一の選択契約に密結合しており、独立mergeすると途中状態で「設定だけ新・実行は旧」になり危険なため。
