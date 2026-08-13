# 0008: 導入済みFoundationを、利用者の変更を失わずに安全にupgradeできるようにする

- 状態: 採用
- 日付: 2026-08-13

## 背景

`install.sh`/`scripts/install-target.sh`は「空repository（`init`）」と「既存repository」を区別し、`SHARED_FILES`/`INIT_FILES`を配布するが、再実行は「1byteでも差分があれば`refusing to overwrite`で停止する」冪等操作にしかならず、更新経路が存在しない（#50）。導入versionや配布元revisionは記録されず、`.agentic-loop.toml`のschemaが版を重ねても、既存導入先へ新しいkeyを届ける手段がない。

## 判断

### 導入状態を`.agentic-loop/manifest.json`としてtarget repositoryにcommitし、`class: shared`/`class: init`でFoundation管理部分と利用者所有部分を区別する

`scripts/lib/foundation-files.sh`が唯一の情報源として`SHARED_FILES`（Foundationが将来も更新しうる）と`INIT_FILES`(新規repository向けの一度きりの種、以後は利用者所有)を定義し、`scripts/install-target.sh`と`scripts/upgrade-target.sh`の双方がそれを`source`する。`manifest.json`は`schema_version`・配布元`repository`・適用済み`revision`（40桁SHA）・`migration_level`・`files[]`（`path`/`class`/`sha256`）・`history[]`(適用履歴)を記録する。`class: init`のfileはupgradeが**絶対に書き込まない**（差分があれば通知するのみ）ことで、「無断上書きしない」という要求を運用ルールではなくdata構造として保証する。

### 適用器は常に新revisionのtree側に置き、現在実行中のscriptが自分自身を書き換えることを避ける

`bin/agentic-loop`自身がFoundation管理fileの一つである以上、upgradeを実行中のprocessが自分のfileを直接上書きすると、読み取り中の内容が壊れる恐れがある。`bin/agentic-loop upgrade`は薄いfront-endで、対象revisionを解決してsource treeを取得したら、そのtreeに含まれる`scripts/upgrade-target.sh`を**子processとして**呼び出す。実際にfileを書き換えるのは常に新revision側のscriptであり、front-end自身の実体（現在実行中のfile）はそのprocessの生存中に一切書き換えられない。加えて、target側へのすべての書き込みは「同じディレクトリに一時fileを書き、`mv`でatomicに置換する」方式を取る。これによりexecによるprocess置換や複雑なprotocol versioningは不要になり、通常の子processのままで安全性を確保できる。

### `--rollback`はGitのcleanな作業treeを前提に、専用のbackupや staging directoryなしで実装する

`agentic-loop upgrade --apply`の事前条件は「作業treeがclean」であることを要求する（AGENTS.mdの不変条件、および他の運用scriptと同じ姿勢）。これはつまり、upgradeが触れるどのfileも、適用直前の内容が必ず`HEAD`にcommit済みであることを意味する。したがって、rollbackは`git checkout HEAD -- <path>`（変更・削除されたfile）と`rm -f <path>`（新規追加されたfile）だけで実現でき、独自のbackup directoryやsnapshotの仕組みは不要である。`bin/agentic-loop upgrade --apply`は、適用したpathの一覧を`.git/agentic-loop/upgrade-last-apply.json`（非commit、target固有のGit管理領域）に記録し、post-apply検証が成功したら削除する。この fileが残っている状態は「検証未完了の適用」を意味し、`doctor`が検出して`--rollback`または再実行を促す。

### migrationは冪等な`check`/`apply`の2段階scriptとし、途中失敗からの再開は「そのまま再実行するだけ」に単純化する

`.agentic-loop.toml`はSHARED_FILESの一員だが、利用者が`max_workers`のような値を編集するのが前提のfileであり、file単位の3値比較（旧manifest hash・現在のtarget hash・新revision hash）に載せると、ほぼ必ず「利用者編集による競合」と判定されて新しいkeyが届かない。そこで`.agentic-loop.toml`はfile単位の上書き対象から除外し、`scripts/upgrade/migrations/NNNN-slug.sh <target> check|apply`という専用の仕組みで、TOMLの特定key（例: `[foundation]`section）だけを追記・変更する。各migrationは冪等（適用済みなら`apply`も`check`も安全に0で終わる）ことを契約とするため、「途中失敗から再実行できる」という要求は、専用の`--resume` flagやjournal directoryを持たずとも、`--apply`を単純にもう一度実行するだけで満たされる。`yq`によるTOMLのround-trip書き換えは、コメントと未知keyを失うため使わない。migrationは`sed`/`cat >>`による行指向の追記に限定し、利用者のcommentと値を保持する。

### 承認gateは`risk`ラベル(`safe`/`review`/`breaking`/`irreversible`/`cost`/`permission`)で判定し、`--approve`は全体承認の1段階にする

file単位の`conflict`(利用者編集との衝突)は`risk: review`とし、承認gateの対象にしない。衝突は`<path>.agentic-loop-new`として保存されるだけで安全かつ可逆であり、`--overwrite <path>`という明示操作自体が利用者の承認になる。migrationの`risk`が`breaking`/`irreversible`/`cost`/`permission`のいずれかを宣言した場合のみ、`--apply`は**何も変更せず**終了code 3で停止し、`--approve`を伴う再実行だけが先へ進む。個別idごとの部分承認は当初案から意図的に外した — 現時点でこの粒度を要求する具体的なmigrationが存在せず、実装と検証を複雑にするだけであるため。将来必要になれば`--approve <id>`への拡張点として`docs/operations/upgrade.md`に明記する。

### `[foundation].revision`が空、またはbranch名/tag名の場合は`upgrade`を拒否し、mainへの暗黙追従を行わない

`agentic-loop upgrade`は`--revision`(40桁SHA限定)・`--source <path>`・`.agentic-loop.toml`の`[foundation].revision`のいずれかで**明示的に**revisionが与えられない限り動作しない。`install.sh`（`AGENTIC_LOOP_REVISION`が既定`main`のまま使われる新規導入の経路）は、branch/tag名を`git ls-remote`または`--source`のrepositoryの`HEAD`で40桁SHAへ解決してから`manifest.json`と`.agentic-loop.toml`の`[foundation].revision`に書き込む。以後の`upgrade`は常にこの明示されたSHAとの比較になり、`main`の先端を暗黙に追従することはない。

## 却下した案

- **3-way自動merge**: `.agentic-loop.toml`のような自由編集fileに対して自動mergeを行うと、意図しない値の混入や無言の設定変更を招く。file単位では「安全に上書きするか、競合として保存するだけ」に限定し、schema変更はmigrationに閉じ込める方が予測可能である。
- **専用のbackup/staging directoryとjournalによる`--resume`**: 「作業treeがclean」という事前条件があれば、Git自身が唯一必要なbackupになる。専用の仕組みを重ねるとfailure modeが増え、検証すべき組み合わせも増える。
- **`bin/agentic-loop`自身によるexecでの自己置換**: 実行中のfileが自分自身を書き換えないという性質は、書き込みをatomic rename化するだけで得られる。exec置換は`trap`によるcleanupが実行されない等の別の落とし穴を持ち込むため採用しない。
- **個別id単位の`--approve`**: 現時点で要求しうる具体的な粒度が無く、実装・検証コストに見合わない。

## 影響

- `scripts/lib/foundation-files.sh`: `SHARED_FILES`/`INIT_FILES`の単一の情報源、manifest生成、JSON escapeの共有library。
- `scripts/install-target.sh`: manifest生成、revisionのSHA記録、`.agentic-loop.toml`の`[foundation].revision`自動pin。
- `install.sh`: revisionのSHA解決、`upgrade`引数のpassthrough(bootstrap経路)。
- `scripts/upgrade-target.sh`: 計画(dry-run)・適用・migration実行・承認gate・rollback記録・post-apply検証。
- `scripts/upgrade/migrations/0001-foundation-config-section.sh`: 初回migration(`[foundation]`section追加)。
- `bin/agentic-loop`: `upgrade`front-end(`cmd_upgrade`)・`--rollback`のローカル実装(`upgrade_rollback`)・`doctor`への3件の点検追加。
- `docs/operations/upgrade.md`: 責務分担・manifest schema・分類表・migration作成手順・承認・rollback・release手順。
- `README.md`/`docs/operations/issue-queue.md`: CLI一覧・責務分担表の更新。
- `scripts/lint.sh`: 新規fileの配布・distribution・不変条件のgrep検証。
- `tests/test-agentic-loop.sh`: pristine・通常更新・利用者編集による競合・利用者所有fileの不変・複数段migration・承認gate・検証失敗とrollback・revision未指定拒否・実行中拒否のE2Eシナリオ。

秘密情報の追加保存はなく、追加の有料基盤・APIも必要としない。upgradeが行うGitHub API呼び出しは、既存の`doctor`が読むものだけであり、`agentic-loop upgrade`自体は新たなAPI呼び出しを追加しない。**追加費用ゼロ**。

## 対象外

- timer/cronによる無人自動upgrade。
- 複数repositoryの一括upgrade。
- upgradeをIssue worker(queue)に実行させること。運用者が明示実行するcommandであり、Supervisor稼働中は拒否する。
- SemVer taggingやGitHub Releaseに基づくrelease pipeline(既存の[継続的デリバリーポリシー](../policies/continuous-delivery.md)がmainへのmergeを起点とする経路を既に定義している)。互換性はrevision pinと`migration_level`とmanifest schemaで判定する。
