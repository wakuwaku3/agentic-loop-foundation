# Firestore 永続化境界

更新日: 2026-08-22

`internal/store/firestore` は `application.UnitOfWork` を Cloud Firestore の
`RunTransaction` に結び付ける実装である。正規状態をメモリへ複製せず、各トランザクションの
callback 内で読み取った Firestore document を根拠に optimistic version／revision を検証する。

## トランザクション規則

- callback の読み取りを先に完了できるよう、書き込みは `UnitOfWork` 内に staged し callback
  成功後に deterministic な path 順で flush する。
- callback が error を返す、または Firestore が競合 retry を行う場合、staged event、outbox、
  idempotency、aggregate はまとめて破棄される。
- authority time は application が callback 前に一度だけ取得し、UoW の `AuthorityContext` から
  event に渡す。retry のたびに clock や ID generator を呼ばない。
- 1 callback の書き込み上限は Firestore の上限より小さい 400 document とする。

## Document codec

各 document は `record_schema=v1`、`kind`、JSON `payload` の envelope を持つ。payload を
JSON string とすることで Firestore の暗黙型変換を避け、未知 schema、kind 不一致、壊れた payload は
`ErrInvalidSchema` として fail closed する。identifier は UTF-8 かつ 512 bytes 以下の URL-safe base64
単一 path component に変換する。Event、Outbox、Idempotency は create-only とし、ID衝突を上書きで
隠さない。

Requirement の本文は Requirement document の `text` field と同じ payload wrapper に co-locate する。
`SaveRequirement` と `SaveRequirementText` は staged document を merge するため、本文取得で
Requirement collection に対する N+1 read や別 collection の整合性問題を生まない。

`installations/<encoded-installation>/<collection>/<encoded-id>` を tenant 境界とし、browser から
Firestore を直接読ませない。`firebase.json` と `scripts/firestore-emulator.sh` はローカル検証専用で、
実 GCP 接続や credential をテストから行わない。

## Query と index

M2 の canonical correctness を優先し、lease query は transaction snapshot から最大 1000 documents
だけを読み、上限超過を `ErrQueryLimit` として fail closed する。domain type に decode して predicate を
適用し、未コミット staged value も同じ predicate に通すため read-your-writes が保たれる。候補量が
上限へ近づく前に、公開 projection と `firestore.indexes.json` を追加し、query read budget を計測した
上で置き換える。

emulator integration test は rollback、aggregate/event/outbox/idempotency の atomicity、codec corruption
を検証する。テストは `FIRESTORE_EMULATOR_HOST` がない環境では skip し、production endpoint へ接続しない。
