# ADR 0011: installが所有する永続runtime profile

## 決定

`scripts/install-target.sh` はinstallのたびに、target自身のgit-common-dir配下の `agentic-loop/runtime`（Git管理外のrepository local state）に、Foundation自身の `devbox.json` / `devbox.lock` で固定したDevbox仮想環境を構築する（`devbox run --config <state>/runtime -- true`）。生成された `.devbox/nix/profile/default/bin` の実在（特に `yq`）を明示的に検証し、満たさなければinstallを失敗させる。`runtime.path` にはこのディレクトリを先頭で記録する。

このディレクトリはinstall/uninstallを跨いで残り続けるため、そこに生成されるDevbox profile symlink（indirect nix GC root）も同様に永続する。これは `install.sh` がyq bootstrapに使う `--config` 先（ホストに`yq`が無い場合の一時ツリーであり得る）とは独立であり、bootstrap後にその一時ツリーが削除されても影響を受けない。

`git`・`gh`・`devbox`・`systemctl`・`systemd-escape`・provider CLIは、それぞれ現在の論理pathで記録する（nix store実体へは解決しない）。ただし解決結果がinstall-target.sh自身の実行元ディレクトリ配下の `.devbox/`（bootstrap用の一時Devbox環境）に限られる場合は記録を拒否し、上記の永続runtimeがそのツールを供給できないときはinstallを失敗させる。

`bin/agentic-loop` は起動時に `runtime.path` を復元した後、`yq` が解決できない場合に限り、この永続runtimeの再実体化（`devbox run --config <state>/runtime -- true`）を一度だけ試み、成功すればそのままPATHへ追加して継続する。失敗時は原因と復旧コマンドを含むエラーで終了する。`doctor` は記録済みディレクトリの実在、永続profileの生存（dangling symlinkでないこと）、`nix-store` が利用可能な環境ではGC root保護の実証（`nix-store --query --roots`）を読み取り専用で検査する。

## 理由

record_runtime_pathが記録するツールの出自は一様ではない。`yq`・`git` はFoundationが `devbox.json` で明示的にpinしている一方、`gh`・`devbox`・provider CLIはホスト側の任意のインストール経路（パッケージマネージャ、手動配置）に依存する。後者をnix store実体へ解決してしまうと、利用者がそのツールを更新した瞬間に記録済みpathが古い実体を指したまま残り、次のGCで消える。前者（Foundationがpinするツール）は逆に、GC耐性のある永続的な供給元を明示的に用意しない限り、一時的なbootstrap環境のGCに巻き込まれる。出自ごとに記録方法を分けることで両方を成立させる。

自己修復を「yqが解決できない失敗経路」に限定するのは、通常経路のオーバーヘッドをゼロに保つためであり、再試行を1回に固定するのは、ネットワークやstore再取得を伴い得る操作を無限にループさせないためである。

## 安全性

永続runtimeの構築・自己修復はtarget配下のrepository local stateにのみ書き込み、共有nix storeへの破壊的操作（`nix-collect-garbage` の実行など）は一切行わない。実測で、この構成のprofileはnix indirect gcrootとして登録され、`nix-collect-garbage --dry-run` の削除候補に対象store pathが含まれないことを確認している。既存の `runtime.path` 復元・bootstrap経路の回帰テストは変更しない。

## 帰結

target 1件あたり永続gc rootが1個増え、そのprofileぶんのnix storeがGCで回収されなくなる。これは意図した保持であり、追加のAPI呼び出しや課金は発生しない。`scripts/upgrade-target.sh` は本ADRの対象外とする。Foundationの `devbox.json` / `devbox.lock` が更新されても、既存の永続virtenvはinstall時のpinのまま追随しないため、upgrade時にpinを更新する仕組みは別Issueで検討する。
