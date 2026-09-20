# cnidaria-cni

小さな Kubernetes クラスター向けの CNI。ノードごとに 1 本の bridge、ノード間はホストの
ルーティングテーブルで到達（flannel `host-gw` 相当）、IPv4 / IPv6 デュアルスタック。
flannel が持たない NetworkPolicy の enforce と、ノード自身を守るポリシーを nftables で足す。

## 公開の前提（最優先）

**このリポジトリは OSS として公開する。**

- 実環境の固有名詞を書かない。ホスト名、実 IPv6 プレフィックス、実 MAC、実アドレス、
  ノードの台数や機材の型番のような環境の事実
- ホスト上の絶対パスを書かない
- 例に使ってよいアドレスは文書用に予約されたものだけ。
  IPv4 は `192.0.2.0/24` / `198.51.100.0/24` / `203.0.113.0/24`（RFC 5737）、
  IPv6 は `2001:db8::/32`（RFC 3849）、MAC は `00:00:5e:00:53:xx`（RFC 7042）。
  LAN 側の例にプライベートレンジを使うのは可
- 動機を個人の事情で書かない。**要件として一般化する**
  （「全ノードが同じ L2 セグメントにある構成を想定する」と書く）

実環境に紐づく設定・値・評価はこのリポジトリに置かない。

## 設計判断

実装に入る前に `docs/adr/`（日本語は `docs/ja/adr/`）を読むこと。一覧は `docs/adr/README.md`。
ここに書かれた決定を覆す実装をしないこと。覆す必要が生じた場合は、先に ADR を追加または
更新する。

## 前提となる決定（ADR の要約）

- **自前の CNI バイナリは置かない。** デーモンが `bridge` / `host-local` / `portmap` を並べた
  conflist をノードに書き、Pod ごとの作業はリファレンスプラグインが行う（ADR 0001）
- **`br_netfilter` を前提にする。** `bridge-nf-call-iptables` / `ip6tables` が 1 でなければ
  デーモンは起動を拒否する（ADR 0002）
- **nftables のテーブルは `inet cnidaria` の 1 つ。** `nft -f` でテーブル単位に不可分に置き換え、
  ruleset 全体を flush しない。kube-proxy のテーブルには触らない（ADR 0003）
- **NodePolicy CRD は cluster-scoped。** ノードを締め出さない安全ルールはポリシーで消せず、
  既定は permissive（drop せずログと counter で見せる）で、`Enforce` は明示的に選ぶ（ADR 0004）
- **IPAM は `host-local`。** ranges は `node.spec.podCIDRs` から書く（ADR 0005）
- **経路は family ごと。** 相手ノードの InternalIP をネクストホップにし、片方の family が
  無ければその family の経路は入れず警告する（ADR 0006）
- **API の watch は controller-runtime に統一。** core 型も CRD も Manager のキャッシュで読み、
  kubebuilder の雛形は使わない。CRD の manifest と DeepCopy は controller-gen で生成する（ADR 0007）
- **kube-proxy は置き換えない。** Service の負荷分散は kube-proxy のまま

## コードの書き方

- **簡潔に、人間が読みやすく書く。アーキテクチャも人間が理解しやすい形を優先する。**
  抽象化・汎用化・間接層は、いま必要な分だけにする。将来のために先回りして層を作らない
- 責務ごとにパッケージを分ける。構成表は下記。ドメインのパッケージ（`conflist`、`routes`、
  `netpol`、`nodepol`、`nftables`）は Kubernetes クライアントに依存せず、クラスター無しで
  テストできる形を保つ
- Go 製の開発ツール（golangci-lint、controller-gen）は `go.mod` の `tool` ディレクティブで
  管理し、`go tool <name>` で呼ぶ。`go install` でのグローバル導入やダウンロードスクリプトは使わない
- `.claude/settings.json` の PostToolUse hook が、Claude Code が Edit / Write で書いた
  `.go` ファイルに `gofmt -w` を掛ける（`.go` 以外には何もしない。`jq` が要る）

## パッケージ構成

| パス | 責務 |
|---|---|
| `cmd/cnidaria` | ノード常駐デーモンの入口。フラグ、Manager の組み立て、シグナル処理 |
| `internal/apis/v1alpha1` | `NodePolicy` CRD の Go 型。controller-gen が DeepCopy と CRD manifest を生成する |
| `internal/conflist` | このノード用の CNI conflist を描画し（純粋関数、golden テスト）、ノードに原子的に書き出す |
| `internal/routes` | 他ノードの PodCIDR への host-gw 経路を family ごとに保つ |
| `internal/netpol` | NetworkPolicy v1 の意味論を chain / set のモデルに変換する |
| `internal/nodepol` | NodePolicy を chain のモデルに変換し、mode に応じた verdict を置く（消せない安全ルールは `internal/nftables` が描画する） |
| `internal/nftables` | モデルを nft テキストに描画し、`nft -f` で適用する。netns 1 つと nft だけで済む tag `netns` のテストもここに置く |
| `internal/controller` | controller-runtime の reconciler。経路用と ruleset 用の 2 つ |
| `internal/sysctl` | 起動時の kernel 設定検査（`br_netfilter`、forwarding） |
| `test/netns` | build tag `netns` の統合テスト。netns で「ノード」を組んで外から検証する |
| `test/netns/testbed` | netns テストの共有基盤。セグメント・ノード・Pod の netns を組み、リファレンスプラグインで Pod を bridge に繋ぐ。ノードへの ruleset 適用と、Pod からの到達判定（drop と「誰も聞いていない」の区別を含む）もここ |
| `hack/netns` | netns テストを回す特権コンテナのイメージと起動スクリプト |
| `deploy` | kustomize（DaemonSet、RBAC、`crd/` に生成された CRD）。`kubectl apply -k deploy` で入る。配るイメージのタグは `kustomization.yaml` の `images:` |
| `.github/workflows` | CI（lint / fmt-check / vet / test）と、タグ push でのマルチアーキイメージ公開 |
| `docs/adr`, `docs/ja/adr` | 設計判断の記録（英語が正、日本語を併置） |

## 開発の進め方

- 原則としてテスト駆動開発（TDD）。期待される入出力に基づきテストを先に書き、
  失敗を確認してから実装する。実装中にテストを都合よく書き換えない
- `make test` は Go ツールチェインだけで通る純粋なユニットテスト。
  root / `CAP_NET_ADMIN` が要るものは build tag `netns` で分離し、`make test-netns`
  （nft と ip がある環境）または `make test-netns-docker`（特権コンテナ）で回す
- 実装が終わったら `make lint`、`make fmt-check`、`make vet`、`make test` を通すこと。
  CI もこの 4 つを回す
- 会話は日本語。**コミットメッセージとコード内のものは英語**

## ドキュメント

- **英語を正とする。** `README.md` と `docs/` の本体は英語で書く
- **日本語を併置する。** `README.ja.md`、`docs/ja/` 以下に対応するものを置く
- **片方だけ更新しない。** 英語を直したら同じコミットで日本語も直す
- **コードは英語だけ。** コメント、`Makefile` の help、テストの失敗メッセージ、
  シェルスクリプトが人に見せる文字列、コミットメッセージ。ここに日本語を混ぜない。
  ただし理由を書いたコメントを、訳を省いて短くしないこと
- この `CLAUDE.md` と `README.md` にある日本語版への案内 1 行は、日本語のままでよい
- ドキュメントにソースコードを含めないこと。構造や挙動は文章と表で説明する
