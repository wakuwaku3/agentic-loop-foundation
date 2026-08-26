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
/home/takushi/.local/state/agentic-loop/v2/*-cost.json   ← git 管理外・0600
```

| ledger | 消費 | 上限 | settled 総額 |
|---|---|---|---|
| V2-017 | 11 | 16 | **0.00 USD** |
| V2-022 | 23 | 24 | **0.00 USD** |
| V2-063 | 11 | 12 | **0.00 USD** |
| V2-075 | **12** | **12** | **0.00 USD** |
| V2-080 | （このマシンで進行中） | 48 | — |

**定額契約なので金額上限は一度も効いたことがない。効くのは回数だけである。**
V2-075 は 12/12 で止まった。見積りは金額ではなく回数で立てること。

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

## 2. 残タスク — required と deferred

### required（v2 の完了条件）

| task | 中身 | packet |
|---|---|---|
| V2-085 | Plan/Prepare の permit 欠落。**emergency-stop 下で canonical state が動く** | あり |
| V2-086 | scheduler role の到達性 | あり |
| V2-084 | lifecycle edge 2本（`CompleteFraming`、Claim ready→active）| あり |
| V2-089 | Claim 拒否 + fixture 62件の移行 | あり |
| V2-090 | owner の pause/cancel + paused の出口 | あり |
| V2-091 | reconcile tick の waiting/recovering | **要作成** |
| V2-092 | promotion 配線と完了 | **要作成** |
| V2-093 | production journey driver | **要作成** |
| V2-081 | M5 gate 再判定（`repair_of: V2-026`）| **要作成** |
| V2-028〜036 | M6・M7・M8 — **codex/opencode 認証が必要** | 要作成 |
| V2-014, V2-054, V2-037〜039 | M9 — **GCP project が必要** | V2-014 のみあり |

**packet を書くときの雛形は `V2-089` と `V2-090` を読むこと。** この2つが最も強い。
V2-089 は「機構は既に1状態について存在していたので、これは問いを追加する task ではなく
既に発している問いを広げる task」と見抜き、admitting set を `internal/domain` から**導出**して
`internal/domain/**` を prohibited にし、**set の二重宣言を build failure に変えた**。
さらに直感的な誤答（`{ready, active}` だけ）が**test suite 全体から不可視**であることまで測っている。

### deferred（gate を塞がない）

`.agents/v2/DEFERRED.md` に9件。**V2-094 は台帳に登録済みだが required ではない。**

---

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

## 4. このマシンに残した価値

- `v2` は remote と一致。`make check` 緑。
- **complete 68 / 95。** gate は M0〜M4 通過、M5 は V2-026 で失敗し V2-081 が後継。
- V2-080（pooled live 観測）をここで払う。判定 commit での交わりを空にして
  **V2-081 が M5 を再判定できる状態**にするのが、このマシンの最後の仕事である。
- M5 が失敗した2つの根拠のうち片方（G6a の赤い `make check`）は既に解消済み。
  もう片方が G6b の交わりで、それが V2-080 の対象である。
