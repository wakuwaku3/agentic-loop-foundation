# Provider adapter・pool・usage window・circuit breaker・handoff（V2-027）

この文書は `internal/provider` が**実際にやっていること**だけを書く。実 Provider CLI を1回も起動していない範囲の話であり、実物での確認は V2-028（`provider-live-multi`）が担う。

---

## 1. 3 adapter の argv surface と、その測り方

`--help` と `--version` の読み取りのみで測った。**subcommand は1つも実行していない**（help の読み取りは Provider usage を消費せず認証も要らない。`codex exec` や `opencode run` を実際に走らせることは、flag の動作確認目的であっても禁止）。

測定環境: hostname `D18061`、`uname -sr` = `Linux 6.18.33.2-microsoft-standard-WSL2`、`devbox run --pure` 内の Go は `go1.25.0 linux/amd64`。

| Provider | `--version` の実測 | 解決した絶対 path | argv |
| --- | --- | --- | --- |
| codex | `codex-cli 0.149.1` | `/home/takushi/.nvm/versions/node/v24.18.0/bin/codex` | `codex exec --json --ephemeral -C <workspace>` |
| claude | `2.1.241 (Claude Code)` | `/home/takushi/.local/bin/claude` | `claude --print --output-format json --no-session-persistence`（Work Packet は stdin） |
| opencode | `1.18.22` | `/home/takushi/.nvm/versions/node/v24.18.0/bin/opencode` | `opencode run --pure --format json --dir <workspace>` |

### help が宣言している行（逐語）

`codex exec --help`:

```
  -C, --cd <DIR>
          Tell the agent to use the specified directory as its working root
      --ephemeral
          Run without persisting session files to disk
      --json
          Print events to stdout as JSONL
```

`opencode run --help`:

```
      --pure         run without external plugins                                          [boolean]
      --format       format: default (formatted) or json (raw JSON events)
                                          [string] [choices: "default", "json"] [default: "default"]
      --dir          directory to run in, path on remote server if attaching                [string]
```

**この測定の結論として、V2-027 は3 adapter の flag を1つも増やしていないし1つも削っていない。** 設計 packet（dp-v2-027 d3/d4）は `--pure`・`--dir`・`--json`・`--ephemeral`・`-C` を「無根拠」と書いていたが、上の help はそのすべてを宣言している。help が宣言しない flag を足すのは捏造であり、help が宣言する flag を消すのは regression である。どちらもしていない。

`--pure`（外部 plugin を読まない）と `--ephemeral`（session file を disk に残さない、claude の `--no-session-persistence` と同型）はいずれも意図的な硬化なので残す。

### `--dir` / `-C` は `Invocation.WorkingDirectory` の二重表現か（実測した）

**二重表現ではない。** 設計は「`Invocation.WorkingDirectory` が既に workspace を持ち、`ProcessSupervisor` が Dir を設定するので flag は弱い2枚目の写しである」という前提だったが、この tree ではその前提が成り立たない。

- `internal/runner/supervisor.go` の `ProcessSupervisor.Run` は `(ctx, argv)` しか取らず、子 process に working directory を**一切設定しない**（`cmd.Dir` への代入が存在しない）。
- `internal/runner/provider.go` の `SupervisedInvocationRunner.Run` も `Invocation.WorkingDirectory` を**読まない**（読むのは `Argv`・`Stdin`・`Environment` だけ）。

したがって production 経路では `Invocation.WorkingDirectory` は現在**何にも消費されていない**。codex と opencode については、`-C` / `--dir` が workspace を子 process に伝える**唯一の表現**である。これを削ると子は runner がたまたま居た directory で動く。これは簡素化ではなく regression なので、削らない。`internal/runner` を編集して `WorkingDirectory` を消費させるのは V2-027 の scope 外である（`internal/runner` は不可触）。

境界を実際に保持しているのは kernel である: `internal/runner.NamespaceConfinement` が rootless user+mount namespace で書き込み可能 mount を workspace に固定し、namespace を提供できない環境では子を**起動しない**（fail closed）。`ProcessSupervisor` は子を独立した process group で走らせ、group ごと TERM→KILL する。adapter の仕事は「出て行こうと**要求できない**こと」だけであり、それが `build()` の以下の拒否である。

- workspace が `..` segment・NUL・CR・LF を含むなら `Build` は拒否する（`ErrInvalidRequest`）。
- argv の全要素について同じ拒否をかける。
- argv[0] は絶対 path ではなく素の実行名である。絶対 path への解決は `SupervisedInvocationRunner` が承認済み `provider-preflight` record の `executable_path` に対して行い、`CostLedger.Reserve` が adapter の argv[0] と record の一致を検査して食い違えば拒否する。adapter 側に executable path を複製しないのは、複製が黙って乖離するからである。

argv 中の絶対 path は codex と opencode で**1個だけ**（workspace 引数）、claude では**0個**。workspace 引数は必ず測定済みの directory flag の直後にしか現れない。

---

## 2. contract fixture（`internal/provider/testdata/**`）

**1 byte も実 Provider 応答ではない。** 全 entry が手書きの projected shape であり、`manifest.json` の `shape_source` が entry ごとに `projected-shape-hand-authored` と宣言している。

なぜ捕獲物ではないか。3 CLI のうち2つ（codex・opencode）はこの machine で認証済み session を持たず、応答を1つも生成できない。claude だけ捕獲すると、「3行が同じであること」だけが目的の table において claude 行だけ構造的に別物になる。だから3つとも宣言された shape にした。V2-028 が実応答を得たとき、manifest の diff が「宣言された shape が実測された shape に置き換わった」ことをそのまま示す。

`manifest.json` の entry の field の意味:

| field | 意味 |
| --- | --- |
| `file` | この entry が指す fixture file 名（file ↔ entry は全単射で、directory を walk して検証する） |
| `provider` | 3 adapter 名のいずれか |
| `cli_version_declared` | この shape を**どの version に対して書いたか**。応答を捕獲した version ではない（捕獲していない）。実装者が `--version` で再実測した値と一致することを test が検査する |
| `shape_source` | 出所。V2-027 の全 entry は `projected-shape-hand-authored` |
| `observed_at` | authoring 時刻（UTC） |
| `reason` | この entry を書いた理由。task 名（V2-027）を必ず含む |
| `corresponding_live_exercise` | 実 CLI での確認を担う exercise。全 entry `provider-live-multi`（V2-028） |

fixture が secret を持たないことは、宣言ではなく**測定**で保証している。directory を walk して（file 0件なら Fatal）、file ごとに4つを検査する。

1. `provider.MaxFixtureBytes` より小さい。
2. `output` / `result` / `summary` の値は空文字列か `sha256:` + 小文字 hex 64桁のみ。
3. package 自身の forbidden pattern・secret pattern のどちらにも一致しない（test 側の複製ではなく package が実際に適用している述語 `ForbiddenPatternMatches` / `SecretPatternMatches` を呼ぶ）。
4. credential deny list に一致する JSON key を持たない。意図的に壊した fixture は decoder が読めないので、key の走査は正規表現で text として行う。

4つの matcher にはいずれも positive control があり、matcher が壊れて何にも一致しなくなった場合に test が通ってしまうことはない。

### 9ケースを単一 table で3 adapter に流す

`docs/architecture/validation.md` の「Provider adapter contract tests」の9行を逐語で採り、2行だけ分割した（`empty／malformed structured output` は区別可能な2つの事実なので2 file、`usageが取得可能／不可能` も2 file）。`timeout／cancel` の行は**file を持たない**: timeout と cancel は出力の shape ではなく invocation の条件なので、`ClassifyError` と `NormalizeError` を通して検証する（`testdata` に timeout/cancel を含む名前の file が存在しないことも assert する）。

結果として file は 10ケース × 3 provider = 30個。table は**1つだけ**で、`fixtureMatrix` を range する文が package 全体で **1つ**であることを AST で assert する。3つの near-identical な copy は全 assertion を満たしながら table の目的（`build` / `parseFixture` / `normalize` への変更が3 adapter 全部に届くこと）を無効化するので、driver の個数そのものを assert している。

`usage` が「報告された（全部0）」と「報告されなかった」を区別する表現は、`Result.UsageReported bool`（`json:"usage_reported"`）1つを足す形にした。`fixture.Usage` を pointer にして、object があれば `true`。`Result.Usage` についての既存の assertion は意味が変わらない（`Usage` は fixture が宣言した count をそのまま持ち、object が無ければ zero 値のまま）。usage window は「報告されなかった」を **unknown** として扱い、**0消費として扱わない**。

---

## 3. usage window は `internal/quota` でも cost ledger でもない

3つは**別の資源**であり、混同は category error である。

| | 単位 | 権限 | 失敗の意味 |
| --- | --- | --- | --- |
| `internal/quota` | Firestore の read / write / delete | 日次無料枠の予約 | 「今日この Loop は無料枠を超える」。V2-027 は**これを編集しない** |
| `internal/runner.CostLedger` | runner local の承認済み record に束縛された暴走検知器 | **exec の前に process を起動してよいかを決める唯一の権限** | 上限到達は「調査のための停止」。成功でも失敗でも課金 event でもない |
| `internal/provider.UsageWindow` | pool を通して試行した invocation 数と、Provider が報告した token 数 | breaker と pool 選択への**入力だけ** | 呼び手が渡した ceiling のどちら側かを言うだけ |

`UsageWindow` は process を起動してよいかを**決めない**（その権限は `CostLedger` 単独のもの）。subscription の残量を主張**しない**。数えている母集団は「この Loop が invoke した分」だけであり、それは実際の使用量の真部分集合だから、残量の主張はそもそも成立しない（`docs/operations/provider-live-claude.md` の「台帳が守る範囲の境界」を参照）。

金銭語彙は機械的に禁止している。`usd` / `cost` / `price` / `billing` / `spend` / `budget` / `credit` を、**non-test file の全 identifier**（AST の `*ast.Ident` 走査）と、**package が emit する全 type の marshal 後の JSON key** に対して大文字小文字を無視した部分文字列として検査する。上限到達は課金 event ではないという裁定の、強制可能な唯一の形がこれである。

state は closed set で `within-window` / `exhausted` / `unknown` の3つ。

- exhausted は ceiling の**ちょうど1単位超**で到達し、それ以前には到達しない（invocation ceiling 3 なら3回目は within、4回目で exhausted。token ceiling 100 ならちょうど100は within、101で exhausted）。
- window の roll over は**呼び手が渡す時刻だけ**が駆動する。stored clock も timer も無い。1周期分の直前ではまだ同じ generation で、周期をまたぐと generation が1つ進む。4周期分を一度に渡せば4つ進む。
- 確立した invocation 数の exhausted は、token 側が unknown でも exhausted のまま（確立した exhaustion を無関係な unknown で格下げしない）。

---

## 4. pool は3 slot・1 Provider = 1 slot

standing authorization が codex・claude・opencode を1つの enum 集合として authorize しており、subscription は CLI ごとに1 identity なので、pool は「Provider を選ぶもの」であって「1 Provider の複数 account を選ぶもの」ではない。したがって **per-Provider の同時実行上限は構造として1** である。`Acquire` は package 中で `Lease` を返す**唯一の関数**であり（AST で assert）、in-use の slot に対しては queue せず**拒否**する。同一 subscription 上の2 invocation は同じ usage window を共有するので、2つ handle を返す pool は存在しない隔離を偽ることになる。

slot が持つ field は次の8つ**だけ**（`reflect` で field 集合そのものを assert しているので、9個目が足されれば test が落ちる）。

| 持つ | 型 |
| --- | --- |
| `Name` | 3 adapter 名 |
| `Authorized` | owner の standing authorization が覆っているか |
| `VerifiedByLoopInvocation` | Loop がこの slot 経由で invocation を完了したことがあるか |
| `PreflightRecordID` | 承認済み `provider-preflight` record の **id だけ** |
| `CooldownDeadline` | 呼び手が渡した deadline |
| `ConsecutiveFailures` | 連続失敗数 |
| `LastFailureClass` | 直近の failure class |
| `State` | slot state（closed set） |

**持たない**: credential 値、環境変数の値、認証 token、session id、owner identity、executable path、しきい値の数値。数値 field は `ConsecutiveFailures` の1つだけであることも assert する（preflight record から limit を写す先が存在しない）。credential の家は runner の Secret Broker だけであり、executable path の正本は digest 束縛された preflight record（`CostLedger.Reserve` が argv[0] の一致を検査する）である。2つ目の家を作れば黙って乖離する。

slot state の closed set: `available` / `in-use` / `cooling-down` / `unauthenticated` / `quarantined` / `stopped-for-inspection`。

`unauthenticated` は「standing authorization が覆っていない」と「この machine に session が無い」の両方が休む場所である。どちらも Loop 側に打てる手が無く owner の行動を要する点で同じであり、どちらなのかは `Authorized` field が記録する。standing authorization が覆っていない Provider は `Clear` でも解除できない。

`quarantined`（secret 疑い）と `stopped-for-inspection`（上限到達）は、**deadline も probe も動かせない**。動かせるのは明示的な外部 clearance だけである。

clock は読まない。時刻に意味が依存する遷移（`Acquire`・`Release`・`StartCooldown`・`EndCooldown`）は呼び手の時刻を引数に取り、依存しない遷移はあえて取らない（無視される引数を「記録された timestamp」と読み違えられないようにするため）。

---

## 5. circuit breaker

### 5.1 開く条件（provider-local の9 class）

breaker は `internal/provider` 自身の `FailureClass` で switch する。`internal/domain` は import しない（provider component を ci の DAG で leaf に保つ。edge を変えられるのは V2-045 だけ）。

| provider-local class | 挙動 | 理由 |
| --- | --- | --- |
| `provider-quota` | 初回で即 open | 明示的な枯渇 signal に rate は要らない。次も失敗すると既に言っている |
| `contract-incompatible` | 初回で即 open | adapter が parse できない shape は retry でも parse できない |
| `provider-transport` | 窓付き count のしきい値で open | 通常の Provider 側障害。bounded retry の後 |
| `timeout` | transport と同じ count に加算し、生じた open を **ambiguous** と印す | timeout は失敗ではなく「結果が不明」 |
| `unknown` | transport より**厳密に大きい** count で open。初回では絶対に open しない | 素性の分からない失敗を transport と同じ速さで扱わない |
| `provider-model` | Provider 全体ではなく **Provider+model の狭い circuit だけ** open | 指示された対処は「同じ pool 内の互換 model 候補を評価する」であり、Provider 全体が open ではそれが不可能 |
| `provider-unauthenticated` | open せず **slot を `unauthenticated` へ**動かす | retry は session を作れない。認証は owner の identity を使うので agent には打てない |
| `cancelled` | 数えない・開かない | 止めたのは我々である。Provider についての観測ではない |
| `invalid-input` | 数えない・開かない | 我々の request についての話である |

網羅性は**構造で**保証している。`FailureClass` 定数の集合を package の AST から読み出し、`FailureClasses()` の列挙と opening table の key 集合の両方と一致することを assert する。10個目の class を足して行を書き忘れれば test が落ちる。table に行の無い class は default に落ちるのではなく `ErrUnmappedFailureClass` で**拒否**する。

**verdict: provider-local 9 class に対して table は網羅的であり、行が無い class は拒否される。**

### 5.2 Loop-level taxonomy（17値）との対応 — 宣言 table のみ

`internal/domain.FailureClass` の17値との対応は、test と本書の**宣言 table** であって import ではない。

| Loop-level class | 挙動 | 理由 |
| --- | --- | --- |
| `provider-quota` | 即 open | 明示的な枯渇 signal |
| `contract-incompatible` | 即 open | parse 不能は retry で変わらない |
| `provider-transport` | 窓付き count | 失敗率。bounded retry の後 |
| `unknown` | transport より厳密に大きい count、初回では開かない | 同上 |
| `provider-model` | Provider+model の狭い circuit だけ | 同じ pool 内の互換 model 候補を評価する |
| `secret-suspected` | open せず slot を `quarantined` へ | 対処は stop / redact / revoke。解除は人の行為であって probe ではない |
| `budget-exceeded` | open せず slot を `stopped-for-inspection` へ | 上限到達は成功でも失敗でもない停止。ここで open すると Loop 側の停止を Provider の障害として報告することになる |
| `invalid-input` | 数えない・開かない | 我々の request |
| `policy-denied` | 数えない・開かない | 我々の request |
| `capacity-unavailable` | 数えない・開かない | 我々の capacity |
| `execution-lost` | 数えない・開かない | Runner か lease の話 |
| `progress-stalled` | 数えない・開かない | **誘惑的なのでここに理由を書く。** 指示された対処は checkpoint→TERM/KILL→新しい Execution であり、「process は確実に消えた」と「結果を確立できない」を区別することが要求されている。Provider を名指さない曖昧さで circuit を開けば、別の何かについての証拠で稼働中の Provider を止めることになる |
| `verification-failed` | 数えない・開かない | Provider は動き、変更が間違っていた |
| `external-ambiguous` | 数えない・開かない | invocation が終わった後の話 |
| `integration-conflict` | 数えない・開かない | 同上 |
| `preview-regression` | 数えない・開かない | 同上 |
| `promotion-partial` | 数えない・開かない | 同上 |

`provider-unauthenticated` はこの17値に**含まれない**。V2-067 がこの class を `internal/domain` ではなく `internal/provider` に足したためである（決定が起きるのはこの leaf package であり、taxonomy に定数を足すのは以前 gate が閉包を証明した package に値を足すことになる）。行は 5.1 の table にある。

`secret-suspected` と `budget-exceeded` は adapter の分類結果ではなく Loop 側の条件なので、`FailureClass` 経由ではなく `Pool.Quarantine` / `Pool.StopForInspection` という明示的な呼び出しで届く。どちらの場合も **circuit は `sending` のまま**であることを test が assert する（Provider は何も悪いことをしていない）。

### 5.3 閉じるのは時間だけでは絶対に起きない

**verdict: 経過時間だけで `sending` に戻る経路は存在しない。** open のどの原因についても、時間を任意に進めて state を問い直しても `sending` にはならないことを assert している。

`open` → `half-open` に必要なもの（すべて呼び手が渡す時刻で駆動する）:

1. cooldown deadline が呼び手の時刻で経過している。
2. その open が probe 適格である。`contract-incompatible` 起因の open は**永久に probe 不適格**（待っても双方の宣言 version は変わらない）。1年進めても `ErrProbeNotEligible`。
3. **枯渇 class 起因の open では、usage window が roll over していること。** deadline は経過したが window は roll していない場合、probe は発行されない（`ErrWindowNotRolledOver`）。経過時間だけでは枯渇した窓は新しくならず、枯渇した窓に対する時間駆動の probe は「2回目の枯渇」を確実に引き当てて実 invocation を1つ捨てるだけである。
4. pool が probe を発行できる。**probe は pool 全体で同時に1つだけ**。1つ outstanding の間は、同じ Provider に対しても他の Provider に対しても発行されない。

`half-open` → `closed` は**成功した invocation の報告のみ**。probe を発行しただけでは閉じない（発行後の state は `probing` であり `sending` ではない）。

probe が失敗したときは cooldown を宣言された ceiling まで乗算し（例: 10分 → 30分 → 90分 = ceiling、以後 90分で止まる）、**counter を reset しない**。失敗した probe は失敗の連続が続いている証拠である。stale な probe は結果を報告できない（`ErrProbeMismatch`）。

`contract-incompatible` の open を閉じるのは、**宣言 CLI version の変化を明示的に観測すること**のみ。同じ version を観測しても閉じない。

`quarantined` と `stopped-for-inspection` の slot には probe も deadline も無い。

固定 sleep・wall-clock timer・goroutine は package 内（test file も含む）に1つも無い。`time.Now` / `Sleep` / `After` / `Tick` / `NewTimer` / `NewTicker` / `AfterFunc` を non-test でも test でも参照しないこと、`go` 文が1つも無いことを AST で assert する。

### 5.4 open は Provider が壊れているという主張ではない

公開 state は `sending` / `not-sending` / `probing` の3つだけで、いずれも**この Loop の行動**を指す。`healthy` / `unhealthy` / `broken` / `down` / `up` / `alive` / `dead` / `ok` という identifier が non-test file に1つも無いことを AST で assert する（完全一致。`openingTable` が `up` を含むのは偶然なので部分一致では禁止しない。部分一致なら正しい code を禁止することになり、その check は最初に踏んだ人に無効化される）。

`not-sending` と `probing` の値は必ず、それを生んだ failure class の closed set・count を取った窓の名前・`observation_scope` を伴う。`sending` の値はそれらを持たない（下していない決定の根拠は持たない）。

`observation_scope` は次を**言葉で**述べる: 数えたのは「この Loop 自身の実行経路を通った invocation だけ」であり、人や別 process が同じ CLI を直接起動した分はこの母集団に入らず、ここからは見えない。

この文言の測定された先例: `docs/operations/provider-live-claude.md` の「台帳が守る範囲の境界」は、runner の台帳が `SupervisedInvocationRunner` を通った invocation しか記録しないことを述べ、実測例として **V2-017 実装時に実装者が shell から手で実行した claude invocation 6回**（HOME/PATH 最小構成の確認2回、到達不能 URL への挙動確認4回）が台帳に一切現れていないことを記録している。同じ母集団の上に建てた breaker は「この Loop 自身の呼び出しがどうだったか」を知っているだけで、Provider の状態については何も知らない。だから `healthy` という field は、唯一重要な場合（人が問題なく使えている Provider に対して、この Loop の直近の試行だけが失敗している場合）に false になる。

---

## 6. provider-neutral handoff

`PrepareHandoff` の既存の意味は変えていない（同じ引数から同じ `Packet`・`Usage`・`OutputDigest`・`Failure` を持つ `Handoff` が出る）。足したのは4つ。

1. `Handoff.Validate`。
2. **carried facts の content digest**。`IncrementID`・`Constraints`・`Decisions`・`Artifacts`・`Verification`・`Unresolved` を固定 field 順で（map ではなく slice で）marshal した bytes の sha256。この6つが handoff の約束であり、要約文の言い換えは digest を変えない。
3. `RequestFromHandoff`。target の Request に戻す。
4. closed enum だけの bounded な試行 history。

digest は「誰も検査していない性質」を **fail closed** に変える。digest が入る前は、Artifact を1つ落とした handoff は完全に valid な `Handoff` を作った。入った後は**拒否**になる。`IncrementID`・`Constraints`（順序の入れ替えを含む）・`Decisions`（detail 1文字を含む）・`Artifacts`（削除・name・digest）・`Verification`（削除・status・evidence digest）・`Unresolved` のどれを変えても `ErrHandoffContentChanged` になり、`Request` は作られない。digest を空にしても拒否する（検査対象を消して検査を飛ばせない）。

`RequestFromHandoff` が拒否するもの:

- target が3名の外（`ErrUnknownProvider`）。`PrepareHandoff` 側でも拒否する。
- 再計算した content digest が不一致（`ErrHandoffContentChanged`）。
- **target の breaker が `sending` でない**（`ErrHandoffTargetNotSending`）。`probing` も `sending` ではない: 1 invocation が既に「確かめる」ことに約束されており、handoff はその invocation ではない。target が `sending` に戻れば同じ handoff が通るので、この拒否は circuit についてのものであって handoff についてのものではない。
- 同じ Increment を、既にその Increment で試した Provider に戻すこと（`ErrHandoffRevisit`）。

**6つの順序対すべて**（codex→claude、codex→opencode、claude→codex、claude→opencode、opencode→codex、opencode→claude）について、target の Request の packet が元の packet と field ごとに一致すること（全 Artifact の name と digest、全 Verification entry を含む）を assert する。同時に、target 自身の `Result` は **target 自身の Provider 名と target 自身の output digest** を持ち、handoff が運んできた digest ではないことも assert する。

history は `(from, to, class)` の3 field だけで、`from`/`to` は3名の closed set、`class` は package が宣言する `FailureClass` のみ（宣言外の文字列は拒否される）。2 hop の chain は**最初の failure class を保持する**。同じ Increment を過去の参加者に戻す chain は3方向すべて拒否する（起点へ戻す、中間へ戻す、3 hop 目）。Increment が違えば別の問いなので history は再出発する。

宣言された bound は `MaxHandoffHistory = 2`。恣意的な数ではない: Provider 3名と「同じ Increment を過去の参加者に戻さない」規則の下では、chain は最大2 hop（1 hop 目で参加者2名、2 hop 目で3名、3 hop 目は必ず参加者へ戻る）。長さ check は revisit 規則と冗長だが、bound が論証だけでなく明示された数でも守られるように残している。

`Handoff` 自身が宣言する string 型の field は5つだけで、allow list で列挙している: `Version`（契約 version）・`FromProvider`・`ToProvider`（3名）・`OutputDigest`・`ContentDigest`（sha256）。prompt・response・conversation・transcript・session・text・body・message のいずれかを名前に含む field は存在してはならない。

**測定された例外を記録する。** `Handoff.Failure` は `Failure` であり、`Failure.Message` は string である。上の閉包は `Handoff` が直接宣言する field についてのものであり、`Failure.Message` はその1つの named field 経由でしか到達しない。`Message` は実質的に自由文ではない: `safeMessage` が256 byte で切り、package の secret pattern に一致する場合は文字列ごと差し替える。V2-027 の code が作る failure は全て固定 literal を入れる。この境界は暗黙にせず test で明示している（4096文字を渡しても256 byte 以下、`Bearer` 形の text は handoff まで到達しない）。

---

## 7. M6 の完了条件のうち V2-027 が満たすもの — **1つも無い**

| M6 完了条件（`docs/architecture/roadmap.md` §M6 より逐語） | V2-027 が真にすること | しないこと | 残りの持ち主 |
| --- | --- | --- | --- |
| 各Providerのsuccess／error／quota／cancelを実物で確認する | **無し** | 全部。fixture は実物ではない | **完全に V2-028** |
| 共通adapter変更を3 Providerすべてで実証する | contract fixture の層。9ケースを単一 table で3 adapter 値に流し、driver が1つであることを assert する | 条件が求める「実証」。`validation.md` はこれを実 Preview の capability 演習の下に置いている | 実証は V2-028 |
| Provider変更後もIncrement、Artifact、Evidenceを失わない | fixture 水準の保存と、失った場合の fail-closed な拒否（content digest） | 実 journal・lease・Increment・Evidence record を通した実 switch での保存 | 継続性の半分は V2-028 |
| opencodeも共通sandbox境界を越えない | `OpenCodeAdapter.Build` が**出て行こうと要求できない**こと（WorkingDirectory は request workspace、argv に injection と traversal が無い） | 条件そのもの。これは request の性質であって process の性質ではない | **完全に V2-028** |

条件1と条件4が完全に V2-028 のものである理由は gate 規則である。**G3.1** は実 Provider CLI を名指す条件に、CLI version と machine 識別子を持つ live evidence を要求する。**G3.3** は unit / fake / stub / contract-test の evidence がどの grade の real-environment 条件も満たさないと述べる。V2-027 が作るのは contract-fixture evidence なので、この2条件は原理的に満たせない。

`validation.md` §3 の「capability 演習」の言い回しは実 Preview revision と実 Provider についてのものなので、fixture matrix に流用してはならない。その語を V2-027 の evidence・code comment・本書のいずれにも書かないことを、package 全 file の walk で assert している（走査 code 自身が自分を検出しないよう、語は2片の連結で組み立てている）。

**本書と V2-027 の evidence は、いかなる release gate も通ったと主張しない。**

---

## 8. V2-075 live exercise 集合との交差（G6b、測定のみ・修復しない）

`internal/provider` を編集すると `ev-v2-075-provider-live-claude-rebind` の G6b 交差が非空になる。V2-027 はこれを隠さないし修復も試みない。

`.agents/v2/evidence/artifacts/V2-075-live-exercise-files.json`（76 path）の `reproduction.measure_the_intersection` が宣言する argv で測った結果:

- **V2-027 自身の diff（base `6a984e38b21d2b684dc56a65c2d889f633a1f52b` → 作業 tree）との交差は3件**: `internal/provider/adapters.go`、`internal/provider/handoff.go`、`internal/provider/provider.go`。
- 観測 commit `d9f98ae573e617aafd82d87fa1c35f9bd6e26ffb` から判定 tree までの diff との交差は9件。上の3件に加えて `internal/application/ports.go`、`internal/application/readmodels.go`、`internal/application/repository.go`、`internal/application/service.go`、`internal/domain/repository.go`、`internal/store/memory/store.go`。後者6件は V2-067 と V2-069 が着地させたもので、V2-027 のものではない。

したがって `ev-v2-075-provider-live-claude-rebind` は G6b の下で**再観測を要する**。再観測の持ち主は **V2-028** である。V2-075 の台帳は 12/12 まで使い切られているので、しきい値を上げてはならない。owner の承認のもとで新しい record と新しい `ledger_path` を発行する必要がある（`docs/operations/provider-live-claude.md`）。V2-028 は3つの実 CLI を claude 含めて再観測するので、この再観測はもともと V2-028 が行う予定のものである。

V2-027 は `.agents/v2/provider-preflight/**` を触らず、live suite を1つも走らせず、しきい値を1つも調整していない。

---

## 9. component evidence key の移動（G6a）

`internal/provider/**` への変更で動いた key は6つ（`devbox run --pure -- make evidence-keys` で実測。すべての touch file を git add した状態で読んだ）。

`provider`（roots 自身）、`runner`（provider を dependency として宣言）、`reconciler`（verification dependency）、`api`・`control-plane`・`store-firestore`（いずれも runner 経由で到達）。runbook と台帳の編集で `docs` と `task-ledger` も動く。

発行する evidence record は **`provider` の1件だけ**である。残りの key 移動は G6a の下で測定の note として記録し、V2-027 が検証していない component の record は作らない。
