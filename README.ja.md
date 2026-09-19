# cnidaria-cni

ノードが同じ L2 セグメントにある小さな Kubernetes クラスター向けの CNI。ノードごとに
1 本の bridge、ノード間はホストのルーティング（flannel `host-gw` 相当）、IPv4 / IPv6
デュアルスタック。そして flannel が持たないもの、NetworkPolicy の enforce とノード自身を
守るポリシーを、どちらも nftables で行う。

名前は刺胞動物（クラゲやイソギンチャク）の門から。触れるまでは無害に見える。

English version: [README.md](README.md)

> **状態: 設計を記録済み、実装は進行中。** 決定は [docs/ja/adr](docs/ja/adr/README.md)
> に、パッケージは骨格として存在する。

## すること

- ノードごとに、リファレンスプラグイン `bridge` / `host-local` / `portmap` を並べた CNI
  conflist を書く。アドレスの範囲はそのノードの `podCIDRs`。Pod は IPv4 と IPv6 の
  アドレスを 1 つずつ持ち、hairpin と hostPort はこれまでどおり効く
- 他のすべてのノードの PodCIDR への経路を family ごとに保つ。ネクストホップはそのノードの
  InternalIP
- NetworkPolicy v1（podSelector、namespaceSelector、ipBlock、ports、endPort、policyTypes、
  「選択されたら隔離」）を 1 つの nftables テーブル `inet cnidaria` に描画する
- cluster-scoped の `NodePolicy` を受け取り、ノード自身の `input` / `output` に適用する。
  ポリシーで消せない安全ルールを持つ。既定は permissive で、drop されるはずのものをログに出して
  数え、`Enforce` を選ぶまで落とさない
- クラスターネットワークの外へ出る Pod のトラフィックを送信元 NAT する

## しないこと

- **kube-proxy の置き換え。** Service の負荷分散はそのまま。cnidaria のテーブルは kube-proxy
  のテーブルの隣に置かれ、そこには何も触らない
- **オーバーレイ。** ノード同士は 1 つのセグメントで直接届く必要がある。VXLAN もトンネルも
  BGP も無い
- **自前の CNI バイナリや IPAM。** Pod ごとの作業はリファレンスプラグインが行う。cnidaria は
  ノード常駐のデーモンと conflist である

## 構成

| パス | 内容 |
|---|---|
| `cmd/cnidaria` | ノード常駐デーモン |
| `internal/` | 責務ごとに 1 パッケージ。各 `doc.go` が何を持つかを書く |
| `deploy/` | kustomize マニフェスト: DaemonSet、RBAC、CRD |
| `docs/adr/` | 設計判断。日本語は `docs/ja/adr/` |
| `test/netns/` | netns 上の統合テスト（build tag `netns`） |

## 開発

`make help` でターゲットの一覧が出る。`make test` は Go ツールチェインだけで通り、
`make test-netns-docker` は netns のテストを特権コンテナで回す。CI は `lint`、`fmt-check`、
`vet`、`test` を回す。
