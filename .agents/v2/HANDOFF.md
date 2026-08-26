# v2 引継ぎ — 別マシンで続けるために

2026-08-26。このマシンは **claude だけが認証済み**の環境で、live 観測をここで払い切ってから
残りを別マシンに渡す。**clone しても付いてこない事実**を先に書く。それが引継ぎの本体である。

---

## 0. 最初に確認すること（1分）

```
git fetch origin 'refs/live-observations/*:refs/live-observations/*'
git ls-remote origin 'refs/live-observations/*'
```

**これが空なら止まってほしい。** live 観測 commit は `v2` から到達不可で、この ref だけが
命綱である。2026-08-26 に `refs/live-observations/v2-075-provider-live-claude` →
`d9f98ae573e617aafd82d87fa1c35f9bd6e26ffb` が **push されていないことを実測して**上げた。
この commit は6ファイルが参照しており、**M3 gate（V2-076）の根拠そのもの**である。
失うと G6b の diff が永久に測れず、live 記録が判定不能になる。

`git clone` は `refs/live-observations/*` を**自動では取らない**。上の fetch を明示的に打つこと。

---

## 1. clone しても付いてこないもの

`.agents/v2` には **`/home/takushi` を含む絶対パスが 72箇所**ある。別マシンでは全部無効で、
これらは**再実行の argv ではなく、事実の記録として読むもの**である。

### ledger は repo の外にある

```
/home/takushi/.local/state/agentic-loop/v2/*-cost.json   <- git 管理外・0600
```

| ledger | 消費 | 上限 | settled `actual_usd` | 金額上限 |
|---|---|---|---|---|
| V2-017 | 11 | 16 | 0.652746 | 10.00 |
| V2-022 | 23 | 24 | 1.008563 | 20.00 |
| V2-063 | 11 | 12 | 0.505211 | 8.00 |
| V2-075 | **12** | **12** | 0.444129 | 8.00 |
| V2-080 | 10 | 48 | 0.505867 | 40.00 |

settle 1件あたり概ね **0.07〜0.11 USD**。

**止まるのは回数だけである。ただし決済が無料だからではない** — この単価では金額の壁が
はるか遠いからである。V2-075 は 12/12 で止まったが、そのとき使っていたのは 8.00 USD の
うち **0.444129**、つまり**金額の94%を残して回数で止まった**。

**見積りは金額ではなく回数で立てる。**

> この表は一度間違っていた。私は「settled 総額は4本とも 0.00 USD」と書いたが、集計 script が
> key に `cost` を含む field だけを足していて、実際の field 名は `actual_usd` だった。
> **何も足さずに0を報告していた。** V2-080 の実装者が測り直して指摘した。経緯は
> `docs/operations/v2-task-dag.md` 9.5 節にある。**0 という結果は「足して0」と
> 「何も足していない」を区別しない。**

### 禁則（例外なし）

**上限の引き上げ・ledger の再初期化・`halted` の消去・ledger file の編集は全部禁止。**
閾値到達は**成功でも失敗でもない fail-closed の停止**で、救済は常に**新しい record を起こすこと**。

別マシンは**自前の preflight record を起こす。** このマシンの record をコピーしてはいけない
（executable path も version も違う）。`claude` はここでは
`/home/takushi/.local/bin/claude` の 2.1.241。

### 認証状態（2026-08-26 実測）

| CLI | 導入 | 認証 |
|---|---|---|
| claude | 2.1.241 | **済** |
| codex | codex-cli 0.149.1 | `codex login status` → **Not logged in** |
| opencode | 1.18.22 | `opencode auth list` → **0 credentials** |

非対話経路は stdin から api key か access token を読む形しかなく、どちらも owner 本人が
持つ値なので**代行不能**。`codex login` と `opencode auth login` を1回ずつ。

---

## 2. 残タスク — required と deferred（2026-08-26の再導出後）

**gate の合格条件は task 一覧ではなく性質集合である**（G1 v2、`docs/operations/v2-task-dag.md`
§12.1）。M5 の性質 P1〜P6 と、そこに**含まれない**繰り延べ済み 7 capability は §12.8 / §12.9。

### required（M5 を閉じるもの）

| task | 中身 | packet |
|---|---|---|
| V2-084 | lifecycle edge 2本（`CompleteFraming`、Claim ready→active）| あり・**queued** |
| V2-089 | Claim 拒否 + fixture 62件の移行 | あり |
| V2-090 | owner の pause/cancel + paused の出口 | あり |
| V2-091 | **到達性の欠陥1件に統合**（reconcile pass の waiting/recovering、promotion の評価と完了、出荷 Runner の journey driver）| 要作成 |
| V2-095 | M5 自身が負う 4 product gap + dogfood 再観測 | 要作成 |
| V2-092 | **M5 再判定 gate**（`repair_of: V2-081`、依存は §12.2 により凍結済み）| 要作成 |

**V2-091・V2-092・V2-093 の3件は統合しました。** outcome が3件とも同じことを言っていた
——出荷されたコードから到達できない遷移がある。V2-093 の file は削除し、V2-092 は
M5 の再判定 gate に再 scope しました。**packet が完備している V2-084/086/089/090 は
統合対象から外しています**（済んだ設計を捨てない）。

**V2-094 は台帳から外しました**（required ではないため。内容は `DEFERRED.md` D1）。

### 人待ち（§8.3 により M5 の gate は止めない）

| 待ち | 塞いでいるもの |
|---|---|
| **codex / opencode の login 各1回** | V2-028〜036（M6・M7・M8）＋ dogfood の 3 capability |
| **GCP project** | V2-014・V2-054・V2-037〜039（M9・D1）＋ dogfood の 4 capability |

**V2-095 の依存から V2-028 を外しました。** 以前は V2-028 に依存していたため、
**認証が済むまで M5 はどうやっても閉じませんでした。**

### deferred

`.agents/v2/DEFERRED.md` に9件。`.agents/v2/DECLARATION-GAP.md` は宣言と実装の差の台帳で、
**以後の発見は新規 task ではなくこの台帳の消し込みとして扱います**（§12.6）。

## 3. 運用規約 — 破ると静かに間違う

**devbox**
- `devbox run` の **exit code は当てにならない。verdict 行で判定する。**
- `devbox run --pure` は**環境変数を消す**。`AGENTIC_LOOP_LIVE_LOCAL` や
  `AGENTIC_LOOP_LIVE_PROVIDER` は必ず `-e` で渡す。shell prefix は無効。
- `devbox run --pure -- <裸の名前>` は **devbox script 名として解釈される**。
  実コマンドは `devbox run --pure -- sh -c '...'` で包む。

**gitleaks**
- `gitleaks dir .agents --config .gitleaks.toml` を使う。
- `gitleaks dir .` は gitignore された `build/evidence/**` から **26件**拾う。
- 散文中の digest は **64 or 40 hex + 句読点1つまで**。2 revision を `..` で繋いだ形は allowlist 外。

**make check**
- **"FAILS 0" のような行は出ない。** `^FAIL` が無いこと、`no leaks found`、
  `workflow-pins: ... no disagreement` の3点で判定する。
- **実装者に `make check` を回させない。** coordinator が統合時に1回だけ回す。
  実装者は `go test -race ./...`、`go vet`、`gofmt -l .`、触った `make component-*`、
  evidence commit 後の `gitleaks dir .agents`。

**manifest / evidence key**
- `internal/ci` の manifest check は `git ls-files` を読む。**`git add` するまで新規 file は不可視**で、
  `make evidence-keys` も同じ。
- **宣言と実 import は同じ commit に載せる。** 宣言だけ先だと `VerifyNoUnjustifiedEdges`、
  import だけ先だと `VerifyDependencyCoverage` が落ちる。
- `ci/**` は `all_on_change` なので、**触ると 23 key 全部が動く。**
- test 由来の edge は `verification_dependencies`（`Affected()` は読まない）。
  `runner` と `reconciler` の間に**実在の循環**がある。
- **source file に module path の文字列 literal を書かない。** manifest check が未宣言 edge と読んで
  `make check` が赤くなる。prefix と suffix を連結する。

**live provider evidence record**
- `component` は **`provider-live-<provider名>`** で始めること。`internal/contracts/provider_preflight.go:160`
  は component が `provider-live-` で始まる index entry **だけ**を選び、選ばれたものにだけ
  「`artifact_refs` が preflight record 自身の digest を名指す」「`approved_at` < `observed_at`」
  の2つを強制する。**`runner` と書くと静かに check の射程から外れる**（V2-080 で実際に起きた）。
- `component` は分類 marker で CI component ではない。`evidence_key` の方に runner component の key を入れる。

**gate 規則**（`docs/operations/v2-task-dag.md` §4）
- **G6a**: component evidence key の一致は**来歴であって合否ではない**。
  発行する record の component だけ測る。23 全数は測らない。
- **G6b**: live 記録の鮮度は**観測 commit と、そこから判定 commit までの diff が
  exercise file set と交わらないことの実測**。**測っていない交わりは G6b を落とす。**
  「重要な変更は無い」は測定ではない。
- **G7**: **skip は pass ではない。** emulator 無しの
  `go test ./internal/store/firestore` は `ok` を出しながら `--- SKIP` を 26件出す。
  その run は何も測っていない。

**task の作法**
- **A1 の scope boundary 5行表は、source を編集する前に evidence record へ書いて commit する。**
  事後に書くと、触った範囲の否定形として何を書いても整合してしまい、境界を守った証拠にならない。
  V2-087 は内容完備・順序未達を自己申告した。V2-088 は正しい順序で、最初の commit が record である。
- Work Order の**行番号と件数は陳腐化している前提で読む。** 直近16 task が毎回1件以上の
  食い違いを見つけ、V2-087 は5件見つけた。測り直して**測った値を使い、食い違いを全部報告する**。
- **実行していない検証を passed と書かない。** 満たせない項目は弱めず、言い換えず、escalate する。

**security**
- **prompt と raw provider response は、切り詰めでも hash でも、どんな形でも記録しない。**
  provider が発行した実行識別子（session_id）は記録してよい。
- **credential 値をどこにも書かない**（test file、doc、evidence、commit message すべて）。
- 子 process の環境は承認 record の `base_names` そのもの。`granted_names` は空のまま。
  CLI は自分の credential store を読む。
- secret 形の fixture は **prefix と suffix を連結**して、pattern が file の中に literal として
  存在しないようにする。

**git**
- 失って困るのは**未 commit の作業**なので、`git reset` / `git checkout -- <path>` /
  `git stash` / `git commit --amend` を使わない。**細かく commit する。**
- task agent は push しない。coordinator が `make check` 緑のときだけ push する。
- `.agents/v2/task-state/**` は **coordinator 専用**。
- `.agents/v2/evidence/index.json` は **1行の compact JSON**。append のみ。
  他 task の entry を触らない。

---

## 4. このマシンの到達点と、M5 について私が間違えたこと

- `v2` は remote と一致。`make check` 緑。**complete 69 / 96**（blocked 20・failed 6・queued 1）。
- gate は **M0〜M4 通過。M5 は失敗**（V2-026、後継 V2-081 も **failed**）。

**live 記録は1本ではなく2本ある。ここを取り違えた。**

| gate | 必須 component | exercise |
|---|---|---|
| **M3live** | `provider-live-claude` | `./internal/runner` の `TestProviderLiveVerticalSlice` |
| **M5live** | `release-live-dogfood` | `./internal/api` の `TestFoundationPreviewLocalDogfood` |

V2-080 が再観測したのは**上の行**である。私はこれで M5 の G6b が閉じると書いたが、
**閉じていない。** M5 の根拠は下の行で、別の exercise・別の record である。
V2-080 それ自体は正しく、M3 の根拠の鮮度を回復している。

**さらに M5 は、このマシンでは原理的に閉じられない。** V2-022 の dogfood が `partial`
だった理由を capability ごとに実測すると:

| 原因 | 数 | capability |
|---|---|---|
| **codex/opencode 未認証** | 3 | `cap-autonomous-resolution`, `cap-shared-resource-allocation`, `cap-provider-operation` |
| **GCP 待ち（D1）** | 4 | `cap-preview-operation`, `cap-stable-promotion`, `cap-loop-control`, `cap-loop-self-update` |
| **M5 自身が負う product gap** | 4 | `cap-repository-registration`, `cap-backlog-visibility`, `cap-human-input-request`, `cap-user-documentation` |

**12 のうち 7 は人の作業が入るまで到達できない。** いま払っても同じ `partial` が返るので、
再観測は無駄になる。したがって **V2-095**（dogfood 再観測）は認証と project と
4つの gap 修正を待つ `blocked` として登録してある。

なお 4つの gap のうち `cap-backlog-visibility` の cursor 拒否は **V2-079 が既に直しており**、
`cap-human-input-request` の「captured から needs-input に入れない」は V2-082 が入れた
framing と V2-084 の `CompleteFraming` が触る範囲なので、**再観測すれば正当に反転する見込み**がある。

**M5 は M6 相当の作業の後に閉じる。** milestone の番号順とは逆になるが、これは
§8.1 の第3根拠が既に述べていたことである。
