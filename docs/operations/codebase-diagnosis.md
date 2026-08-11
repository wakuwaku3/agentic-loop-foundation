# コードベース自己診断

## 目的

自己診断は、要件と実装の双方向のずれ、ディレクトリ構成、開発用skillの追加余地、古く未使用のファイルを定期的に監査する。診断中にコードを変更せず、検証できる改善点だけを `diagnosis` Label付きの日本語GitHub Issueとして記録する。自動修正キューへは投入しない。

## 実行

`install.sh` はリポジトリごとのuser-level systemd timerを冪等に設定する。初回は起動後30分、その後は7日間隔、最大6時間のランダム遅延で実行する。CodexサブスクリプションのCodex CLIと既存のGitHub認証だけを使い、API keyや追加の有料サービスは使わない。

手動診断はリポジトリルートで実行する。

```sh
bin/agentic-loop-diagnose
```

定期実行の状態と履歴は次で確認する。

```sh
systemctl --user list-timers 'agentic-loop-diagnosis-*'
journalctl --user -u 'agentic-loop-diagnosis-*'
```

同一リポジトリの診断はlockで直列化される。Codexはread-only sandboxでコードを調査し、既存Issueを検索して重複を避ける。Issueには問題、根拠、期待状態、受け入れ条件、対象パスを記載する。対応する場合は人が内容を確認してから `agent:queued` を追加する。
