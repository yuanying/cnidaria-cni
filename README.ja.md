# cnidaria-cni

ノードが同じ L2 セグメントにある小さな Kubernetes クラスター向けの CNI。ノードごとに
1 本の bridge、ノード間はホストのルーティング（flannel `host-gw` 相当）、IPv4 / IPv6
デュアルスタック。そして flannel が持たないもの、NetworkPolicy の enforce とノード自身を
守るポリシーを、どちらも nftables で行う。

名前は刺胞動物（クラゲやイソギンチャク）の門から。触れるまでは無害に見える。

English version: [README.md](README.md)

> **状態: alpha。** API グループは `v1alpha1` で、フィールドはまだ動きうる。データプレーンと
> ポリシーの意味論は、netns でノードを組んで Pod の中から到達性を確かめるテストで覆われて
> いるが、実際に動き続けているクラスターの上で動かした実績はまだない。

## すること

- ノードごとに、リファレンスプラグイン `bridge` / `host-local` / `portmap` を並べた CNI
  conflist を書く。アドレスの範囲はそのノードの `podCIDRs`。Pod はノードが CIDR を持つ
  family ごとにアドレスを 1 つ受け取る（デュアルスタックなら 2 つ、シングルスタックなら
  1 つ）。hairpin と hostPort はこれまでどおり効く
- 他のすべてのノードの PodCIDR への経路を family ごとに保つ。ネクストホップはそのノードの
  InternalIP
- NetworkPolicy v1 を 1 つの nftables テーブル `inet cnidaria` に描画する
- cluster-scoped の `NodeNetworkPolicy` を受け取り、ノード自身の `input` / `output` に適用する。
  ポリシーで消せない安全ルールを持つ。既定は permissive で、drop されるはずのものをログに出して
  数え、`Enforce` を選ぶまで落とさない
- クラスターの PodCIDR の外へ出る Pod のトラフィックを送信元 NAT する
- コンテナエンジンやホストのファイアウォールが iptables の `FORWARD` の policy を `DROP` に
  していても Pod のトラフィックを通す。クラスターの PodCIDR から来るものと PodCIDR へ行く
  ものを accept する自分のチェインを持ち、`FORWARD` の先頭からそこへ jump する

## しないこと

- **kube-proxy の置き換え。** Service の負荷分散は、kube-proxy がどちらのモードで動いて
  いようとそのまま。cnidaria のテーブルは kube-proxy のテーブルの隣に置かれ、そこには何も
  触らず、ruleset 全体を flush することもない。自分のテーブルの外に書くのは、iptables の
  `FORWARD` に置く jump とその先の自分のチェインだけで、`FORWARD` の policy と他のルールは
  そのままにする（ADR 0003）
- **オーバーレイ。** ノード同士は 1 つのセグメントで直接届く必要がある。VXLAN もトンネルも
  BGP も無い
- **自前の CNI バイナリや IPAM。** Pod ごとの作業はリファレンスプラグインが行う。cnidaria は
  ノード常駐のデーモンと conflist であり、Pod の起動時に cnidaria のコードは 1 行も動かない
- **カーネルモジュールの読み込み。** `br_netfilter` が無い、あるいはその sysctl が 1 で
  なければ、デーモンはそれを有効にするのではなく起動を拒否する。デーモンが自分で有効に
  する kernel 設定は IP forwarding だけで、Pod のためにルーティングする CNI が一般に
  そうするのと同じである

## 前提

| 前提 | 理由 |
|---|---|
| `br_netfilter` が読み込まれ、`net.bridge.bridge-nf-call-iptables` と `bridge-nf-call-ip6tables` が 1 | 同一ノードに留まる Pod 間のトラフィックは bridge で L2 スイッチされ、これが無いと IP の filter hook を通らない。デーモンは起動時に確認し、無ければ起動を拒否する（ADR 0002） |
| IP forwarding については不要。デーモンが `net.ipv4.ip_forward` と `net.ipv6.conf.all.forwarding` を 1 にする | ノードが自分の bridge とセグメントの間をルーティングする。デーモンは起動時に forwarding を有効にしてログに残し、設定を書けない場合にだけ起動を拒否する（ADR 0002） |
| IPv6 の default 経路をルーター広告から得るインターフェースに `accept_ra=2`、または静的な default 経路 | forwarding が有効だと、`accept_ra=1` はルーター広告を無視し、default 経路が失効する。cnidaria は `accept_ra` に触らない（ADR 0002） |
| kube-controller-manager に `--allocate-node-cidrs` とクラスター CIDR | ノードの範囲の出どころは `node.spec.podCIDRs` だけ。デュアルスタックなら family ごとに 1 つ |
| 各ノードが、使う family それぞれの InternalIP を持つ | 経路のネクストホップは同じ family の相手ノードの InternalIP。片方の family が無ければその family の経路は入らず、警告が出る（ADR 0006） |
| 全ノードが同じ L2 セグメント上にある | ネクストホップは on-link でなければならない |
| kube-proxy（モードは問わない） | Service は kube-proxy の仕事であって cnidaria の仕事ではない。ここにあるものは kube-proxy のテーブル名やチェイン名に依存せず、DNAT が forward hook より前で起きることにだけ依存する。これは iptables モードでも nftables モードでも変わらない（ADR 0003） |
| iptables の `FORWARD` の policy については不要。`DROP` でもよい | デーモンが自分のチェインで Pod のトラフィックを accept する。iptables の backend（nft か legacy）は kube-proxy の `KUBE-` チェインを持っているほうを使う。それが無い family はもう一方に合わせ、どこにも無ければ nft を使う。選択と理由は起動時にログに出る（ADR 0003） |

ノードに事前に入れておくものは無い。イメージがデーモンと `nft` と `iptables` と
リファレンスプラグインを運ぶ。

## インストール

```sh
kubectl apply -k deploy
```

入るのは 5 つ。`NodeNetworkPolicy` の CRD、ClusterRole、ClusterRoleBinding（この 3 つは
cluster-scoped）、そして `kube-system` の ServiceAccount と DaemonSet。
配るイメージのタグは `deploy/kustomization.yaml` の `images:` で、overlay から上書きできる。
イメージは `ghcr.io/yuanying/cnidaria-cni` に置かれる。

DaemonSet は `hostNetwork` と `CAP_NET_ADMIN` だけで動く（privileged ではない）。デーモンが
forwarding を有効にできるよう、ホストの `/proc/sys/net` を書き込み可能でマウントする。
`/proc/sys` のそれ以外は読み取り専用のまま。init container が `bridge` / `host-local` /
`portmap` を `/opt/cni/bin` にコピーし、ノードにある他のプラグインには手を触れない。

ノードが動いている状態とは、conflist が `/etc/cni/net.d/10-cnidaria.conflist` にあり、
`ip route show proto 200` に他ノードの PodCIDR が並び、テーブル `inet cnidaria` が
存在すること。

## NodeNetworkPolicy: ノード自身のトラフィック

NetworkPolicy は Pod にしか効かない。`NodeNetworkPolicy` は cluster-scoped で、ラベルでノードを
選び、そのノードの `input` / `output` チェインに描画される（ADR 0004）。

ノードを締め出さないための仕掛けが 2 つある。

**安全ルール。** `input` と `output` のチェインは、どのポリシーでも消せないルールから始まる。
established / related、loopback、ホストが path MTU discovery と近隣探索に必要とする ICMP /
ICMPv6 のタイプ。これに input では SSH・kubelet・API server・etcd・NodePort レンジが、
output では API server・etcd・kubelet・DNS と自ノードの PodCIDR が加わる。最後のものが、
kubelet が自ノードの Pod に probe を打ち `exec` できる状態を保つ。ポート番号は慣例のもので、
一覧は [ADR 0004](docs/ja/adr/0004-node-policy-crd-and-lockout-prevention.md) にある。

**既定は permissive。** `spec.mode` の無いポリシーは完全に描画されるが何も落とさない。
`Enforce` なら落としていたものを、代わりにログに出して数える。したがってポリシーを実運用に
入れる手順はこうなる。

1. そのまま適用する。`mode` の無い `NodeNetworkPolicy` は `Permissive` である
2. 何が落ちるはずだったかを、そのノードのトラフィックの周期に見合うだけ眺める。夜間に
   バックアップが走るノードなら丸 1 日

   ```sh
   journalctl -k | grep cnidaria-nodenetworkpolicy   # 送信元・宛先・プロトコル・ポート
   nft list chain inet cnidaria input         # counter。こちらは rate limit されない
   kubectl get nodenetworkpolicies                   # 各ポリシーが要求している mode
   kubectl get nodenetworkpolicy NAME -o yaml        # status.nodes[]: 各ノードで効いた mode
   ```

   `kubectl get` が出す列は `spec.mode`、つまりポリシーが要求している mode である。
   各ノードで実際に効いた mode は `status.nodes[]` の各エントリにあり、同じ方向を共有する
   別の permissive なポリシーがある間、この 2 つは食い違う。

3. ログが示す足りない項目を足し、`spec.mode: Enforce` にする

2 つの mode の間で変わるのは最後の verdict だけで、ruleset の他は何も変わらない。
`Permissive` で見たものが、そのまま `Enforce` の挙動になる。

ポリシーが相手を指すのはアドレスだけで、Pod や Namespace のセレクターは使えない。ノードは
クラスターではなくネットワークから宛てられるものだからである。Pod も他と同じく CIDR で指す。

```yaml
apiVersion: cnidaria.unstable.cloud/v1alpha1
kind: NodeNetworkPolicy
metadata:
  name: workers
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
  ingress:
    - from:
        - ipBlock:
            cidr: 192.0.2.0/24
            except: [192.0.2.128/25]
      ports:
        - port: 9100          # protocol の既定は NetworkPolicy と同じく TCP
```

ある方向のポリシーに 1 つも選ばれていないノードは、その方向は開いている。選ばれたノードは
ルールに並んだものだけを通す。1 つのノードを選ぶ複数のポリシーは和で効き、その方向を閉じる
ポリシーが**すべて** `Enforce` のときだけノードはその方向を enforce する。permissive な
ポリシーが 1 つあれば、ノードは観測を続ける。

## NetworkPolicy: どこまで enforce するか

NetworkPolicy v1 は丸ごと描画する。`podSelector`、`namespaceSelector`、`except` 付きの
`ipBlock`、番号と名前の両方のポート、`endPort` のレンジ、TCP / UDP / SCTP、`policyTypes`、
そして「ある方向のポリシーに 1 つでも選ばれた Pod は、その方向で隔離される」。IPv4 と IPv6 は
対称に扱う。ノードは自分が抱える Pod のポリシーを enforce するので、パケットは出ていくノードで
送信元 Pod の egress に、着いたノードで宛先 Pod の ingress に当たる。

範囲の外にあるもの:

| 対象外 | 理由 |
|---|---|
| `hostNetwork` の Pod | ノードのアドレスを持つ。これを指すポリシーはノードを指すことになる。このトラフィックを司るのは `NodeNetworkPolicy` |
| ノードから自ノードの Pod への通信 | ノード自身の `output` から出ていき forward を通らないので、Pod の ingress ポリシーはこれを見ない。kubelet の probe と `exec` がこれに依存しており、上の安全ルールの 1 つが開けたままにする |
| 終了した Pod | Succeeded / Failed の Pod は、誰かに消されるまで API 上でアドレスを持ち続け、そのアドレスは既に動いている別の Pod のものかもしれない。どの選択からも外す |
| NetworkPolicy の観測モード | `Permissive` は `NodeNetworkPolicy` だけのもの。Pod のポリシーは、他のどの実装とも同じく適用した瞬間から落とす |

## 設定

デーモンに渡すのは自分のノード名だけで、他はノードに合った既定値を持つ。

| フラグ | 既定 | 用途 |
|---|---|---|
| `--node-name` | `$NODE_NAME` | このコピーが受け持つノード。DaemonSet が downward API から埋める |
| `--conflist` | `/etc/cni/net.d/10-cnidaria.conflist` | conflist の書き出し先 |
| `--network-name` | `cnidaria` | conflist のネットワーク名。`host-local` はリースを `/var/lib/cni/networks/<名前>` に置くので、別の CNI が既に配ったアドレスの割り当て状態を引き継ぐときに、その CNI のネットワーク名を指定する（ADR 0009） |
| `--mtu` | `0` | bridge と Pod のインターフェースの MTU。0 ならノードの InternalIP を持つインターフェースから読む |
| `--health-addr` | `127.0.0.1:19080` | `/healthz` と `/readyz`。loopback にしか bind しないので、DaemonSet の probe は `127.0.0.1` を指定している |
| `--metrics-addr` | `0` | Prometheus メトリクス。既定は off |

controller-runtime とそのロガーが登録するフラグ（`--kubeconfig`、`--zap-*` など）は表から
省いている。

## 構成

| パス | 内容 |
|---|---|
| `cmd/cnidaria` | ノード常駐デーモン |
| `internal/` | 責務ごとに 1 パッケージ。各パッケージの package comment が何を持つかを書く |
| `deploy/` | kustomize マニフェスト: CRD、RBAC、DaemonSet |
| `docs/adr/` | 設計判断。日本語は `docs/ja/adr/` |
| `test/netns/` | netns 上の統合テスト（build tag `netns`） |

## 開発

`make help` でターゲットの一覧が出る。`make test` は Go ツールチェインだけで通り、
`make test-netns-docker` は netns でノードを組んで統合テストを特権コンテナで回す。
`make image` は `docker buildx` で `linux/amd64` と `linux/arm64` のイメージを作る。
CI は `lint`、`fmt-check`、`vet`、`test` を回す。

何かを変える前に [docs/ja/adr](docs/ja/adr/README.md) を読むこと。そこにある決定を
前提にコードの形が決まっており、覆すなら新しい記録を書くところから始める。
