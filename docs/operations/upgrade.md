# 導入済みFoundationのupgrade（[ADR 0009](../decisions/0009-foundation-upgrade.md)）

`bin/agentic-loop upgrade`は、既に導入済みのAgentic Loop Foundationを、利用者の変更を失わず、互換性と差分を確認しながら更新するcommandである。既定は書き込みを一切行わないdry-runで、`--apply`を指定した場合だけ実際に変更する。

```sh
bin/agentic-loop upgrade [--revision SHA | --source PATH] [--apply] [--approve] [--skip-verify] [--overwrite PATH]... [--format json]
bin/agentic-loop upgrade --rollback
```

- 既定(引数なし、または`--apply`なし): 何が変わるかを日本語で表示するだけ。GitHubへの書き込み・作業treeの変更・`.git/agentic-loop/`への書き込みは一切行わない。
- `--revision SHA`: 40桁のcommit SHAでpin対象を明示する。branch名・tag名は受け付けない(`main`への暗黙追従を防ぐため)。省略時は`.agentic-loop.toml`の`[foundation].revision`を使う。両方が空なら終了code 2で停止する。
- `--source PATH`: ネットワーク取得の代わりにlocal pathをFoundation sourceとして使う(offlineや固定revisionでの再現、テスト用途)。
- `--apply`: 実際に適用する。事前条件(後述)を満たさない場合は何も変更せず失敗する。
- `--approve`: `breaking`/`irreversible`/`cost`/`permission`のいずれかの`risk`を持つ項目がある場合に、それらを承認して適用する。
- `--overwrite PATH`: 利用者編集との競合(後述)を、明示したpathに限り新内容で上書きする。繰り返し指定できる。
- `--skip-verify`: 適用後の完全検証(`[foundation].verify_command`)を省略する。`doctor`は省略しない。
- `--rollback`: 直前の`--apply`が検証未完了のまま残した状態を、Gitの`HEAD`を使って元に戻す。他の引数と併用できない。
- `--format json`: `schema_version: 1`の1行JSON。

```sh
bin/agentic-loop upgrade --format json
```

## 責務分担: install / upgrade / setup

| command | 対象 | 役割 |
| --- | --- | --- |
| `install.sh` | 未導入のrepository | Foundationを初めて配置し、`bin/agentic-loop setup`/`start`まで実行する。 |
| `bin/agentic-loop upgrade` | 導入済みのrepository | 配布元の新しいrevisionとの差分を確認し、Foundation管理部分だけを安全に更新する。 |
| `bin/agentic-loop setup` | 導入済みのrepository | GitHub Label/Projectの作成・整合。upgradeの一部ではなく、必要なら独立して再実行できる。 |

## 導入版数の記録: `.agentic-loop/manifest.json`

install/upgradeの度に、target repositoryへ`.agentic-loop/manifest.json`を書き込む(git管理下に置き、通常のcommit対象とする)。

```json
{
  "schema_version": 1,
  "source": {"repository": "wakuwaku3/agentic-loop-foundation", "revision": "<40桁SHA>", "revision_ref": "main"},
  "installed_at": 1786600000,
  "mode": "install",
  "migration_level": 1,
  "files": [{"path": "bin/agentic-loop", "class": "shared", "sha256": "..."}],
  "history": [{"at": 1786600000, "from_revision": "none", "to_revision": "<SHA>", "from_level": 0, "to_level": 1, "steps": [], "result": "installed"}]
}
```

- `class: shared`(`scripts/lib/foundation-files.sh`の`SHARED_FILES`): Foundationが将来も更新しうるfile。upgradeの更新対象。
- `class: init`(同`INIT_FILES`、`mode: init`で導入されたrepositoryのみ): 新規repository向けの一度きりの種。**upgradeは絶対に書き込まない**。上流に変更があっても通知するだけ。
- `manifest.json`が無い(#50より前の導入、または削除された)場合、upgradeは全fileの内容を新revisionと直接比較し、一致しないものはすべて競合として扱う(無断上書きしない)。

### manifestは機械生成の未commit変更として残る

install/upgradeは適用したrevisionを`manifest.json`へ書き込むが、この変更は**自動ではcommitしない**。利用者が置いた未commit変更を巻き込まないためである。そのためinstall/upgrade直後のworktreeは、`manifest.json`だけがdirtyになる(利用者の変更やFoundation管理fileに差分が無い場合)。

`.agentic-loop/update-main.sh sync`はこの**manifest単独の生成差分を許容**する。`git status --porcelain`の差分が` M .agentic-loop/manifest.json`だけなら、fast-forwardを中止せず、ローカルのmanifest内容(適用済みrevisionの記録)を保持したまま`origin/main`へ進める。manifestを破棄・上書きせず、`git merge --ff-only`が差分を温存する性質を利用している。

逆に、manifest以外の未commit変更(利用者の編集、Foundation管理fileの変更など)が1つでもある場合、および`origin/main`が`HEAD`のancestorでない場合(ahead・分岐)は、従来どおり`refusing to update a dirty main worktree`／`refusing to update a main branch that is ahead of or diverged from origin/main`で同期を拒否し、対象worktreeとbranchを変更しない。加えて、ローカルのmanifestがdirtyな状態で`origin/main`側も`manifest.json`を書き換えている場合は、どちらのrevisionを正とするか自動では決められないため明示的に拒否する。

同期が拒否された場合の復旧手順:

```sh
# 自分が置いた変更を残したい場合: commitする
git add <自分の変更>
git commit -m "..."    # .agentic-loop/manifest.jsonは巻き込まない
# 機械生成のmanifest差分だけを破棄してよい場合
git checkout -- .agentic-loop/manifest.json
# その後、同期を再実行する
.agentic-loop/update-main.sh sync .
```

## upgradeの判定(file単位)

各`SHARED_FILES`のpathを、旧manifestのhash・targetの現在のhash・新revisionのhashの3値で分類する(`.agentic-loop.toml`は対象外。後述の設定migrationで扱う)。

| targetの状態 | 判定 | `--apply`の動作 |
| --- | --- | --- |
| target無し、新revisionに有り | 追加 | 追加する |
| targetが旧manifestと同じ内容(未編集)で、新revisionと異なる | 更新 | 上書きする |
| targetが旧manifestと異なる内容(利用者編集済み) | **競合** | 上書きしない。新内容を`<path>.agentic-loop-new`として保存する。`--overwrite <path>`を指定した場合のみ上書きする |
| targetの内容が新revisionと既に一致 | 変更なし | 何もしない |
| 新revisionに無い(上流で削除)、targetの内容が旧manifestと一致 | 削除 | 削除する |
| 新revisionに無い、targetの内容が旧manifestと異なる(利用者編集済み) | 削除候補 | 削除しない。手動判断を促す |
| `class: init`のfileに上流の変更がある | 情報のみ | 何もしない |

いずれの上書き・削除も、同じdirectory内に一時fileを書いてから`mv`でatomicに置換する。現在実行中の`bin/agentic-loop`が自分自身を安全に置き換えられるのはこのためである([ADR 0009](../decisions/0009-foundation-upgrade.md)参照)。

## 設定のmigration

`.agentic-loop.toml`は利用者が値を編集する前提のfileのため、file単位の上書き判定には載せない。`scripts/upgrade/migrations/NNNN-slug.sh <target> check|apply`という番号付きscriptが、TOMLの特定keyだけを追記・変更する。

- `check`: 既に適用済みなら終了code 0、未適用(適用が必要)なら終了code 1。
- `apply`: 適用に成功(既に適用済みの再実行を含む)したら終了code 0、失敗したら0以外。
- 各scriptの先頭commentに`# id:` `# risk: safe|breaking|irreversible|cost|permission` `# reversible: yes|no` `# approval: required|no` `# summary:` `# recovery:`を日本語で記す。
- 契約上**冪等**でなければならない。これにより、途中のmigrationが失敗しても、原因を解消して`--apply`を再実行するだけで良く、専用の`--resume` flagは存在しない。

現在の唯一のmigrationは`0001-foundation-config-section`で、`.agentic-loop.toml`に`[foundation]`section(`repository`/`revision`/`verify_command`)を追加する。

## 承認gate

migrationの`risk`が`breaking`/`irreversible`/`cost`/`permission`のいずれかの場合、`--apply`は**何も変更せず**終了code 3で停止し、対象・影響・復旧手順を表示する。`--approve`を付けた再実行だけが適用へ進む。file単位の`競合`はここでいう承認対象ではない(`--overwrite`という明示操作自体が承認になる)。

## 適用前後の検証

`--apply`は次を順に確認し、いずれか一つでも満たさなければ**何も変更せず**終了する。

1. 作業treeがclean(`git status --porcelain`が空)。
2. Supervisorが稼働していない(`bin/agentic-loop stop`で停止してから実行する)。
3. `bin/agentic-loop doctor`に失敗が無い。
4. revisionが明示されている(`--revision`/`--source`/`[foundation].revision`のいずれか)。

適用後は`doctor`を再実行し、続けて`[foundation].verify_command`(既定`devbox run --pure check`)を実行する。いずれかが失敗した場合、**適用済みの状態を保持したまま**終了code 1で停止し、`bin/agentic-loop upgrade --rollback`または再実行(`--apply`をもう一度)の案内を表示する。自動rollbackは行わない([継続的デリバリーポリシー](../policies/continuous-delivery.md)と同じく、自動復旧が被害を広げうる場面では停止して判断を仰ぐ)。

## rollback

`--apply`は、事前条件として作業treeのcleanさを要求するため、触れるすべてのfileは適用直前の内容が必ず`HEAD`にcommit済みである。`--rollback`はこの性質を使い、専用のbackupを持たずに元へ戻す。

1. この回の適用で新規追加したpathを削除する。
2. この回の適用で変更・削除したpathを`git checkout HEAD --`で復元する。
3. 適用に伴って書き換えた`.agentic-loop/manifest.json`も`HEAD`から復元し、適用前のrevision記録・`history`を維持する。
4. 適用記録(`.git/agentic-loop/upgrade-last-apply.json`、非commit)を削除する。

この記録が残っている(＝適用後検証が完了していない)間、`doctor`は失敗として報告する。

## bootstrap(#50より前に導入されたrepository)

`bin/agentic-loop upgrade`自体が存在しない導入先では、`install.sh`にcommandを渡して新revisionのupgrade適用器を直接起動する。

```sh
curl -fsSL https://github.com/wakuwaku3/agentic-loop-foundation/archive/<SHA>.tar.gz | ... # install.shと同様にrevisionを解決した後
AGENTIC_LOOP_TARGET=/path/to/target bash install.sh upgrade --revision <SHA>
```

manifestが無い場合の分類は前述の表の通りで、内容が一致するfileだけが静かに記録され、一致しないものはすべて競合として報告される(無断上書きしない)。

## 互換性方針

`compatibility.level`(`--format json`)は次のいずれかを返す。

| level | 意味 |
| --- | --- |
| `compatible` | 保留中のmigrationも競合も無い。 |
| `migration-required` | 保留中のmigrationがあるが、競合や高risk項目は無い。 |
| `breaking` | 競合、または`breaking`/`irreversible`のmigrationがある。 |

## release手順

配布物のtag/releaseは持たない。`main`へのmergeが唯一の配布起点であり([継続的デリバリーポリシー](../policies/continuous-delivery.md))、`--revision <SHA>`による明示pinと`migration_level`・manifest schemaが互換性の判定単位になる。将来SemVer tagging/GitHub Releaseへ移行する場合は、`[foundation].revision`にtag名を許可する拡張として本docsとADR 0009を更新する。

## 費用・秘密

追加のGitHub API呼び出しは`doctor`が読むものだけで、`agentic-loop upgrade`自体が新たなAPI呼び出しを追加することはない。追加費用ゼロ。`manifest.json`・upgradeの出力(`--format json`含む)に秘密・token・Issue本文は一切含まれない。
