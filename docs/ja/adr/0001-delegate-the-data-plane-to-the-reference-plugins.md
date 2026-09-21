# ADR 0001: データプレーンはリファレンス CNI プラグインに委ね、自前のプラグインバイナリは置かない

- 状態: 決定（2026-09-19）

## 背景

cnidaria は、全ノードが 1 つの L2 セグメント上にあるクラスターで flannel の `host-gw`
バックエンドを置き換える。ノードごとに 1 本の Linux bridge、Pod のアドレスはそのノードの
`node.spec.podCIDRs`（IPv4 と IPv6）から、ホストのルーティングテーブルには相手ノードごとに
1 本の経路、という形である。flannel がやらず cnidaria が足すのは、NetworkPolicy と
ノード自身に対するポリシーの enforce である。

問題は、Pod ごとの作業 —— veth ペアの作成、bridge への接続、アドレスの付与、hairpin と
hostPort の設定 —— をどこまで cnidaria 自身が書くかである。3 つの形を比較した。

**A. CNI プラグインを書く。** `/opt/cni/bin` に自前のバイナリを置き、ADD / DEL / CHECK を
実装する。完全な制御と引き換えに、`bridge` が既にやっていることの完全な複製を抱える。
リンクの作成、netns の出入り、ゲートウェイアドレス、IPv6 DAD、hairpin、CHECK 動詞、
CNI 1.1 の GC である。

**B. flannel と同じ薄いラッパー。** 自前のバイナリが、デーモンの書いたファイルからノード
ごとのサブネット情報を読み、ADD 時に `bridge` + `host-local` の設定を組み立てて委譲する。
これは flannel の形であり、flannel にはそうする理由があるが、ここには当てはまらない。
flannel の conflist は ConfigMap から来る静的ファイルで全ノードで同一なので、実行時に
ノードのサブネットを差し込む何かが必要になる。ラッパーがその何かである。

**C. バイナリを置かず、デーモンが conflist 全体を書く。** ノードのデーモンは自分の
`podCIDRs` を既に知っているので、`bridge`、`host-local`、`portmap` を直接名指しし、
ranges を埋めたノードごとの conflist を書く。コンテナランタイムはリファレンス
プラグインを呼び出し、Pod 作成時に我々のものは何も動かない。

## 決定

**C を採る。** cnidaria は CNI バイナリを出荷しない。デーモンがリファレンスプラグインを
名指しした conflist をレンダリングして `/etc/cni/net.d` に書き、Pod ごとの作業はすべて
リファレンスプラグインが行う。

conflist は以下のもので、ranges にはノードの CIDR が入る。

| プラグイン | 設定 | 理由 |
|---|---|---|
| `bridge` | `bridge: cni0`、`isDefaultGateway: true`、`hairpinMode: true`、`mtu` はノードの上流リンクから | flannel が使っていた delegate 設定と同じ。Pod から見えるネットワークがこれまでと変わらない |
| `host-local`（`bridge` の IPAM として） | `node.spec.podCIDRs` からアドレスファミリごとに 1 つの range set | ADR 0005 |
| `portmap` | `capabilities.portMappings: true` | hostPort が flannel 時代と同じく動き続ける |

リストは CNI `1.0.0` を宣言する。使用中のコンテナランタイムもリファレンスプラグインも
このバージョンを話す。

### A を採らない理由

A が書くものはすべて `bridge` の再実装であり、regied の
[ADR 0008](https://github.com/yuanying/regied/blob/main/docs/adr/0008-delegate-to-existing-implementations.md)
が言う「他人が既に持っている層を持つ」ことに当たる。bridge プラグインは保守されており、
すべての CNI バージョンに対してテストされており、新規実装が 1 年は間違え続ける隅々
（IPv6 DAD、hairpin と promiscuous mode の関係、CHECK、GC）を扱っている。cnidaria の価値は
この層のどこにもない。

### B を採らない理由

B は、ADD 時にいくつかの数値をデーモンからプラグインへ運ぶためだけにバイナリを残す。
どのみちデーモンがノードごとに conflist を書くのだから、数値は conflist に入れられ、
バイナリは消える。残るのは、2 つのアーキテクチャ向けにビルドするものが 1 つ減り、Pod
作成の経路上のプロセスが 1 つ減り、我々のバージョンと彼らのバージョンを合わせなければ
ならない場所が 1 つ減る、ということである。

B が再び正しい形になるのは、Pod が動いている間にノードごとの入力が変わる場合だけである。
`node.spec.podCIDRs` は controller manager が一度設定し、Node オブジェクトの寿命の間
変わらないので、そうはならない。

### この決定で cnidaria が持つことになる層

| 層 | 持ち主 |
|---|---|
| veth、bridge ポート、アドレス、hairpin、hostPort | リファレンスプラグイン |
| アドレス割り当てとその状態 | `host-local`（ADR 0005） |
| ノードがどの conflist を受け取るか | cnidaria デーモン |
| 相手ノードの Pod CIDR への経路 | cnidaria デーモン（ADR 0006） |
| クラスターネットワークの外へ出る Pod トラフィックの source NAT | cnidaria デーモン（ADR 0003） |
| NetworkPolicy と NodeNetworkPolicy の enforce | cnidaria デーモン（ADR 0003、0004） |
| Service のロードバランシング | kube-proxy。触らない |

masquerade には注記が要る。flannel では `--ip-masq` フラグが、クラスターネットワークの外へ
向かう Pod トラフィックをノードのアドレスに変換するルールを入れていた。それが無いと、Pod
CIDR の外にあるものへ向かう Pod のパケットは、周囲のネットワークが返せない Pod の送信元
アドレスのまま出ていく。`bridge` プラグインには `ipMasq` オプションがあるが、これは
プラグインの中から Pod ごとに iptables ルールを書くもので、kube-proxy の隣に iptables-nft
状態の第 2 の書き手を置き、プラグインをホスト上の iptables バイナリに縛ることになる。
よって masquerade ルールは cnidaria のものとし、自分のテーブルの中に、クラスターの Pod
CIDR をキーにして置く（ADR 0003）。

## 帰結

- タスクの当初のゴールには「CNI プラグインバイナリ」が挙げられていた。その項目は無くなり、
  成果物は conflist とデーモンである。`make build` はバイナリを 1 つ作る。
- リファレンスプラグインがすべてのノードに無ければならない。デーモンのイメージがそれらを
  同梱し、DaemonSet の init コンテナが `/opt/cni/bin` へコピーするので、ノードにはコンテナ
  ランタイム以外に事前導入するものが無い。
- Pod の作成時に cnidaria のコードは一切動かない。デーモンが落ちていても Pod は起動し
  アドレスを得る。維持されなくなるのは経路とポリシーであり、まだレンダリングされていない
  ポリシーとは、デーモンが追いつくまで新しく起動した Pod へのトラフィックがフィルタ
  されないことを意味する。これはコントローラ方式の enforcer すべてが持つ同じ失敗窓である。
- netns テスト（ADR 0008）は、モックではなく本物の conflist を通した本物のプラグインを
  動かす。
