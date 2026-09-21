# 設計判断の記録（ADR）

決定は、それに依存するコードより先に記録する。決定された記録は書き換えて別のことを
言わせない。考えが変わったら、それを覆す新しい記録を足すか、何がいつ変わったかを書いた
補記を付ける。英語版が正で、[docs/adr](../../adr/README.md) にある。

| # | 題 | 決めたこと |
|---|---|---|
| [0001](0001-delegate-the-data-plane-to-the-reference-plugins.md) | データプレーンはリファレンス CNI プラグインに委譲する | 自前の CNI バイナリは置かない。デーモンが `bridge` / `host-local` / `portmap` を並べた conflist を書く。masquerade は自分で持つ |
| [0002](0002-same-node-pod-traffic-passes-through-br-netfilter.md) | 同一ノード内の Pod 間トラフィックは br_netfilter を通す | br_netfilter の sysctl を起動時に検査し、満たさなければ起動を拒否する。IP forwarding はデーモンが自分で有効にする |
| [0003](0003-one-nftables-table-and-the-order-of-chains.md) | nftables のテーブルは 1 つ、チェーンの走る順序 | `inet cnidaria`、base chain 5 本、egress と ingress は別の base chain、`nft -f` で不可分に置換。唯一の例外として、iptables の `FORWARD` から Pod のトラフィックを accept する自分のチェインへ jump する |
| [0004](0004-node-policy-crd-and-lockout-prevention.md) | NodeNetworkPolicy CRD と締め出し防止 | NetworkPolicy に似せた cluster-scoped CRD、ポリシーで消せない安全ルール、既定は permissive で `Enforce` は明示的に選ぶ |
| [0005](0005-ipam-is-host-local.md) | IPAM は host-local | ranges は `node.spec.podCIDRs` から、状態は host-local のファイルストア |
| [0006](0006-next-hop-per-address-family.md) | ネクストホップは family ごと | 同じ family の InternalIP。欠けた family は迂回せず警告する。経路に所有者の印を付ける |
| [0007](0007-controller-runtime-for-everything-the-daemon-watches.md) | デーモンが watch するものはすべて controller-runtime | Manager 1 つ、reconciler 2 つ、CRD は controller-gen、kubebuilder の雛形は使わない |
| [0008](0008-tests-split-by-privilege.md) | テストは必要な権限で分ける | ユニットテストはツールチェインだけで通る。netns テストは build tag の裏に置き特権コンテナで回す |
| [0009](0009-migration-from-flannel-without-restarting-pods.md) | flannel から Pod を再起動せずに移行する | 同じ `cni0`、経路は宛先ごとに置換、ipam のストアはネットワーク名で共有（`--network-name`）、cnidaria のデプロイ前に前の CNI の conflist を退かす、他人のファイルは消さない |
