# claude CLI 実 Provider vertical slice（V2-017）

このドキュメントは、実 `claude` CLI を経由する live test suite の運用手順を記す。対象は `internal/runner/provider_live_test.go` の `TestProviderLiveVerticalSlice` である。

## live suite の実行方法

`AGENTIC_LOOP_LIVE_PROVIDER=1` を設定し、かつ `devbox run --pure` が環境変数を落とすため `-e` で明示的に渡す。`go` / `gcc` を解決できる devbox 由来の PATH に `claude` の実体（`/home/takushi/.local/bin`）を加えて渡すこと。

```sh
LIVE_PATH="$(devbox run --pure -- env | grep '^PATH=' | cut -d= -f2-):/home/takushi/.local/bin:/usr/bin:/bin"
devbox run --pure \
  -e AGENTIC_LOOP_LIVE_PROVIDER=1 \
  -e HOME=/home/takushi \
  -e PATH="$LIVE_PATH" \
  -- go test -count=1 -v -timeout 600s -run TestProviderLiveVerticalSlice ./internal/runner
```

live test は次の3条件をすべて満たさない限り必ず skip する（`t.Logf` でどの条件が失敗したかを記録する）:

1. `AGENTIC_LOOP_LIVE_PROVIDER=1`
2. `.agents/v2/provider-preflight/V2-017-provider-live-claude.json` が `contracts/schemas/provider-preflight.json` に対して valid、かつ `internal/contracts.CheckProviderPreflightLedger` のリポジトリ全体整合性チェックを通過する
3. 承認済み record の `limits.ledger_path` が書き込み可能である

`devbox run --pure -- go test ./...`（＝ `make check` の一部）はこの3条件のいずれも満たさないため、live invocation は常に 0 件である。

## ledger の場所と読み方

ledger は承認済み record の `limits.ledger_path` にあり、本タスクでは絶対パス:

```
/home/takushi/.local/state/agentic-loop/v2/V2-017-provider-cost.json
```

リポジトリの外、ワークスペースの外にあり、0600 のファイルが 0700 のディレクトリに置かれる。中身は `{schema_version, provider, task_id, preflight_digest, limits, halted, entries[]}` で、各 `entries[]` は `{sequence, purpose, state("reserved"|"settled"), reserved_usd, actual_usd, session_id, input_count, output_count, cache_read_count, cache_creation_count, duration_api_ms, duration_ms, num_turns, started_at, finished_at}` を持つ。`cat` で直接読める（`jq` があれば整形できる）。prompt・応答本文は一切含まれない。

## halted フラグの意味と解除できる者

`halted:true` は「単発の invocation の実測 `actual_usd` が 2.00 USD を超えた」ことを示す暴走検知シグナルである。billing 上の失敗ではなく、context 無限成長などの異常を疑うべき停止信号。

- `halted:true` になった後は、`internal/runner.CostLedger.Reserve` がどんな残余バジェットがあっても必ず失敗する（fail-closed）。
- **解除できるのはリポジトリの owner のみ**であり、ledger ファイルを手で編集して `halted` を書き換えることは禁止する（それは異常の証拠を消すことになる）。owner は、新しい調査を経てから、新しい `provider-preflight` record を発行し、必要なら新しい `ledger_path` を割り当てて再開させる。

## capacity exhaustion（上限到達）時の対応

`max_invocations`（16）または `max_total_cost_usd`（10.00 USD）に到達した場合も fail-closed で停止する。これは「暴走検知のための点検停止」であり、成功でも失敗でもない。

- 上限をその場で引き上げてはならない（record の `limits` を書き換えない）。
- 同じ ledger を初期化し直して回数をリセットしてはならない。
- 対応は owner に新しい `provider-preflight` record（新しい `id`、必要なら新しい `ledger_path`）の発行を依頼することである。

## 再承認の手順（work order 編集時）

`approval.subject_path`/`subject_digest` は `.agents/v2/packets/V2-017-work-order.json` の sha256 に固定的束縛されている。Work Order を1バイトでも編集すると、その時点で `subject_digest` は一致しなくなり、`internal/contracts.CheckProviderPreflightLedger` も `internal/runner.LoadPreflightRecord` も失敗するようになる。**これは意図的な設計である。**

- 対応: **`subject_digest` を新しい値に書き換えて辻褄を合わせてはならない。**
- 正しい対応: owner に新しい work order の sha256 に対する承認を依頼し、新しい `approval` block を持つ record を（必要なら新しい `id` で）発行する。

## 台帳が守る範囲の境界（記録されるのは Loop 自身の実行経路だけ）

台帳（`internal/runner.CostLedger`）が entries を残せるのは、invocation が `SupervisedInvocationRunner`（＝ Loop 自身の実行経路）を通り、`CostLedger.Reserve` を経由した場合に限る。この境界の外で何が起きても、台帳はそれを一切観測しない。

- **operator や agent が shell から直接 `claude` を手で実行した分は、台帳には一切現れない。** 台帳の `entries[]` の合計が表しているのは「Loop がこの ledger を通じて使った量」であり、「この machine で `claude` を使った総量」ではない。両者を同一視してはならない。
- したがって、subscription の枠の実際の残量を知りたい場合、この台帳を見ても分からない。台帳はそのための代替にはならず、Provider 側の使用量表示を直接確認する必要がある。
- `halted` フラグや `max_invocations` / `max_total_cost_usd` といった暴走検知のしきい値も、同じ境界の内側でしか機能しない。Loop の実行経路を通らずに動くループ（人や別プロセスが `claude` を直接繰り返し呼ぶ場合など）を、この台帳は検知することも止めることもできない。
- 具体例: V2-017 の実装時、ハーネス配線前の検証として bash 経由で `claude` CLI に対し2回（HOME/PATH 最小構成の確認、各 約$0.08相当）、加えて到達不能 URL への挙動確認を4回（いずれも API 到達前に hang したため $0）、計6回の invocation が実行されたことが実装者本人により開示されている。これらは `CostLedger.Reserve` を経由していないため、**台帳には一切記録されていない**。台帳に残っているのは、ハーネス配線後に `CostLedger` 経由で実行された8 invocation・11 entry のみである。

## 再観測（V2-063）と、いま有効な record

V2-045 が evidence key の算法を「依存先 Roots の推移閉包」へ変えたため、`ev-v2-017-provider-live-claude` は自身の宣言算法（evidence_key は runner component の key）によって stale になった。V2-063 は同じ9 subtest を実 `claude` CLI に対して再実行し、`ev-v2-063-provider-live-claude-refresh` として記録した。V2-017 の record・evidence の bytes は書き換えていない。

このため、上の「live suite の実行方法」の条件2で名指している record と、「ledger の場所と読み方」の絶対パスは、**現在は次の値である**（上の記述は V2-017 実施時点の記録として残している）。

- 承認済み record: `.agents/v2/provider-preflight/V2-063-provider-live-claude-refresh.json`（`internal/runner/provider_live_test.go` の `liveRecordRelPath`／`liveTaskID` が指す先）
- ledger: `/home/takushi/.local/state/agentic-loop/v2/V2-063-provider-live-claude-refresh-cost.json`（V2-017 の台帳とは別ファイル。再観測は旧台帳の残枠を借りない）
- 暴走検知しきい値: `max_invocations` 12・`max_total_cost_usd` 8.00 USD（V2-017 は 16・10.00 USD）。再観測は既存範囲を超えないため、必要な8 invocation と名前付き retry 数回分に合わせて小さくした
- `approval.subject_path`: `.agents/v2/packets/provider-standing-authorization.json`。したがって「再承認の手順」節の work order 束縛は V2-017 の record に固有の話であり、現行 record は standing authorization packet の bytes に束縛されている。この packet を1バイト編集すれば同じ理由で `subject_digest` が一致しなくなる

record を新しく発行するときの注意（V2-063 で実測した順序）:

1. record を先に置き、`go test -run TestProviderPreflightLedger ./internal/contracts` で schema と束縛を通す（この時点では invocation 0）
2. `-run 'TestProviderLiveVerticalSlice/<存在しない subtest 名>'` で gate だけを踏む。gate が通れば skip log が出ず `--- PASS` になり、台帳ファイルはまだ生成されない。ここで gate の誤りを invocation 0 で検出できる
3. phase ごとに別 process で走らせる。`caseB_reuses_I1_journal_zero_invocations` は I1 の in-process の journal を再利用するので、単独で走らせると `t.Skip` になる（skip は pass に数えない）。I1 と同じ process で走らせること
4. 失敗した試行の台帳 entry も消さない。V2-063 では I1..I4 と同一 process で走らせた I5/I6 の第1試行が I6 で失敗し（`actual_usd` 0.00・`session_id` 無し＝API 未到達の transport 失敗）、seq 5/6 を消費した。source を変えずに単独 process で再実行して PASS したが、失敗試行の2 entry は台帳に残したままである

## 鮮度を交差の実測で判定する（V2-075、G6b）

V2-063 は同じ9 subtest を再実行したが、鮮度の主張を `evidence_key`（runner component key）の一致に置いていた。V2-045 が key を推移閉包へ変えた結果 runner の閉包は11 component に広がっており、この主張は「観測の後にリポジトリのどこにも何も着地しない」場合しか満たせない。実際 M3 gate は、観測後に runner roots 内の doc comment が1つ直っただけで落ちた。`docs/operations/v2-task-dag.md` §4 の G6b はこれを受けて鮮度の判定を置き換えた。live evidence の鮮度は次の2つの**実測**で満たす。

1. evidence が**観測 commit** を記録していること
2. 観測 commit から判定 commit までの diff と、**その exercise が実際に compile し実行する file 集合**との**交差が空**であること

「comment だけだから無害」という論証は G6b を満たさない。許すのは測定だけである。交差が空でなければ理由に関係なく再観測、交差が空なら key がどれだけ動いていても鮮度は満たされる。

### file 集合はどこにあるか

`.agents/v2/evidence/artifacts/V2-075-live-exercise-files.json` が tracked file として集合そのものを持つ。判定側が再計算できなければ G6b は判定できないため、この file は集合の全 path（`paths`、V2-075 の観測時点で76件）と、それを機械的に取り直す argv、交差を測る argv、除外した runtime 入力とその理由を全部含む。

集合の内訳（`path_groups`）:

- **compiled（67件）**: `go list -deps -test ./internal/runner` が返す自 module の package から `GoFiles`・`CgoFiles`・`EmbedFiles` のみを集める。**package ごとに `TestGoFiles` を足してはならない。** go list は test 込みの variant `internal/runner [internal/runner.test]` を別 entry として返し、その variant の `GoFiles` に in-package test file 18件が既に畳み込まれている。したがって `GoFiles` だけを取れば「test binary が実際に compile するもの」と完全に一致し、依存 package 自身の test file（binary は compile しない）を誤って含めることもない。go list が返す唯一の非リポジトリ path（go build cache 内の生成 `_testmain.go`）は `git ls-files` との交差で落ちる。git diff は tracked path しか名指せないので、この交差で失うものはない
- **runtime_read（5件）**: compile はされないが実行中に開かれる file。`LoadPreflightRecord` が読む `contracts/schemas/provider-preflight.json`、record 自身、`approval.subject_path` が指す packet、そして `CheckProviderPreflightLedger` が読む `.agents/v2/provider-preflight/` 配下の全 record
- **toolchain_pins（4件）**: `go.mod`・`go.sum`（module graph と Go version。`go.mod` は suite の `mustRepoRoot` が実行時に stat もする）、`devbox.json`・`devbox.lock`（toolchain 実体と shell）。`Makefile` は集合に入らない——この exercise は `go test` を直接叩き、make target は一切関与しない

集合に入れられない外部実体は `out_of_tree_executables` に別枠で記録する。provider CLI（`/home/takushi/.local/bin/claude`）はリポジトリ外なので git diff には原理的に現れず、交差では守れない。代わりに record の `provider.executable_path`／`provider.version` が identity を固定し、`CostLedger.Reserve` が invocation ごとに argv[0] の解決先の一致を確認する。

### 除外した runtime 入力（G6a に落ちるもの）

`LoadPreflightRecord` は毎回 `CheckProviderPreflightLedger` を呼び、これが `.agents/v2/evidence/index.json` と、index が `provider-live-` component として名指す evidence file を読む。これらは **judged set から除外している**。理由は自己参照である: この observation が書く record と index entry は観測より後に着地するので、含めれば G6b の条件はこの record に限らず**あらゆる** live record にとって原理的に充足不能になる。

除外は穴ではない。evidence 台帳の整合性は判定 commit で**再実行**される: `make check` の `go test ./internal/contracts` が `TestProviderPreflightLedgerPassesOnTheRealTree`（`CheckProviderPreflightLedger` を実 tree に対して再実行）と `TestActiveEvidenceIndex`（index と file の全単射、および全 `evidence_hash` の再計算）を回す。これは再実行による鮮度＝**G6a** であり、だからこそこの入力群に G6b の交差は不要である。

### 交差の測り方

```sh
# 1) 公開された集合を読み出す
python3 -c "import json;print(chr(10).join(json.load(open('.agents/v2/evidence/artifacts/V2-075-live-exercise-files.json'))['paths']))" \
  | LC_ALL=C sort -u > /tmp/v2-075-set.txt

# 2) 観測 commit から判定 commit までの diff を取る（2つの rev は別引数で渡す）
git diff --name-only <observed_commit> <judging_commit> | LC_ALL=C sort -u > /tmp/v2-075-diff.txt

# 3) 交差
comm -12 /tmp/v2-075-set.txt /tmp/v2-075-diff.txt
```

出力が1行も無ければ鮮度を満たす。1行でも出れば、その変更が何であれ再観測を要する。完全な argv は集合 file の `reproduction.measure_the_intersection` にあり、そのまま実行できる。

2つの rev を `A..B` ではなく別引数で渡しているのは意図的である。`.gitleaks.toml` の allowlist は `.agents/v2/` 配下の 40桁／64桁 hex に**末尾の句読点1文字まで**しか許さないので、`<sha>..<sha>` と書くと点が2つ続き secret 検出で赤くなる。

### 交差が機能していることの確認（positive control）

集合と架空の diff list で計算するだけでよく、tracked file を編集する必要はない（編集は完結規約違反になる）。V2-075 で実測した3件はいずれも交差1件を返した: exercise 自身の test file（`internal/runner/provider_live_test.go`）、依存 package の source（`internal/domain/model.go`）、toolchain pin（`devbox.lock`）。対照として docs と evidence 台帳と task-state だけの diff list は交差0件を返した。

### live task の完結規約

**exercise の key と交差を測った後に、その exercise が compile する file を同一 task で1つも編集しないこと。** V2-063 はこれを守ったが、coordinator が統合時に破って M3 gate を落とした。`liveRecordRelPath`／`liveTaskID` の切り替えは runner の key 閉包の内側なので、再観測 task が触ってよい唯一の箱であり、**観測より前に**着地させる。以後の commit は `.agents/**` と `docs/**` だけに限る。

### いま有効な record（V2-075）

- 承認済み record: `.agents/v2/provider-preflight/V2-075-provider-live-claude-rebind.json`
- ledger: `/home/takushi/.local/state/agentic-loop/v2/V2-075-provider-live-claude-rebind-cost.json`（V2-017・V2-063 の台帳とは別 file。残枠を借りない）
- 暴走検知しきい値: `max_invocations` 12・`max_total_cost_usd` 8.00 USD（V2-063 と同値）
- **注意: V2-075 の観測はこの台帳を 12/12 まで使い切った。** provider 側の transient 失敗が3 invocation を消費したためで（`halted` は false、Reserve が拒否した invocation は1件も無い）、この record にはもう残枠が無い。次に live suite を回す者は**しきい値を上げてはならない**。owner の承認のもとで新しい record と新しい ledger path を発行すること
