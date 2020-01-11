package ipvs

import (
	"bytes"

	"k8s.io/klog"
	utilipset "k8s.io/kubernetes/pkg/util/ipset"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"

)

// createAndLinkeKubeChain 创建ipvs所需的iptables规则, 在调用前, 主调函数已经将存储区reset过了.
// caller: syncProxyRules()
// createAndLinkeKubeChain create all kube chains that ipvs proxier need and write basic link.
func (proxier *Proxier) createAndLinkeKubeChain() {
	// 以下existingFilterChains与existingNATChains为filter/nat表下的各链名称及内容的map
	existingFilterChains := proxier.getExistingChains(proxier.filterChainsData, utiliptables.TableFilter)
	// md为什么是iptablesData而没有natChainsData???
	existingNATChains := proxier.getExistingChains(proxier.iptablesData, utiliptables.TableNAT)

	// Make sure we keep stats for the top-level chains
	// 首先是顶层链, 就是与INPUT/OUTPUT, 及FORWARD/POSTROUTING这些平级的链.
	for _, ch := range iptablesChains {
		if _, err := proxier.iptables.EnsureChain(ch.table, ch.chain); err != nil {
			klog.Errorf("Failed to ensure that %s chain %s exists: %v", ch.table, ch.chain, err)
			return
		}
		// chain为ch链的内容. 如果iptablesChains中存在当前系统的nat/filter表所没有的链, 
		// 就分别用kubePostroutingChain/KubeForwardChain替代...不过原因是为什么???
		if ch.table == utiliptables.TableNAT {
			if chain, ok := existingNATChains[ch.chain]; ok {
				writeBytesLine(proxier.natChains, chain)
			} else {
				writeLine(proxier.natChains, utiliptables.MakeChainLine(kubePostroutingChain))
			}
		} else {
			if chain, ok := existingFilterChains[KubeForwardChain]; ok {
				writeBytesLine(proxier.filterChains, chain)
			} else {
				writeLine(proxier.filterChains, utiliptables.MakeChainLine(KubeForwardChain))
			}
		}
	}
	// 创建iptablesJumpChain中的链.
	for _, jc := range iptablesJumpChain {
		args := []string{"-m", "comment", "--comment", jc.comment, "-j", string(jc.to)}
		_, err := proxier.iptables.EnsureRule(utiliptables.Prepend, jc.table, jc.from, args...)
		if err != nil {
			klog.Errorf("Failed to ensure that %s chain %s jumps to %s: %v", jc.table, jc.from, jc.to, err)
		}
	}

	// Install the kubernetes-specific postrouting rules. 
	// We use a whole chain for this so that it is easier to flush and change, 
	// for example if the mark value should ever change.
	// NB: THIS MUST MATCH the corresponding code in the kubelet
	masqRule := []string{
		"-A", string(kubePostroutingChain),
		"-m", "comment", "--comment", `"kubernetes service traffic requiring SNAT"`,
		"-m", "mark", "--mark", proxier.masqueradeMark,
		"-j", "MASQUERADE",
	}
	if proxier.iptables.HasRandomFully() {
		masqRule = append(masqRule, "--random-fully")
		klog.V(3).Info("Using `--random-fully` in the MASQUERADE rule for iptables")
	} else {
		klog.V(2).Info("Not using `--random-fully` in the MASQUERADE rule for iptables because the local version of iptables does not support it")
	}
	writeLine(proxier.natRules, masqRule...)

	// Install the kubernetes-specific masquerade mark rule. 
	// We use a whole chain for this so that it is easier to flush and change, 
	// for example if the mark value should ever change.
	writeLine(proxier.natRules, []string{
		"-A", string(KubeMarkMasqChain),
		"-j", "MARK", "--set-xmark", proxier.masqueradeMark,
	}...)
}

// getExistingChains get iptables-save output so we can check for existing chains and rules.
// This will be a map of chain name to chain with rules as stored in iptables-save/iptables-restore
// Result may SHARE memory with contents of buffer.
func (proxier *Proxier) getExistingChains(buffer *bytes.Buffer, table utiliptables.Table) map[utiliptables.Chain][]byte {
	buffer.Reset()
	err := proxier.iptables.SaveInto(table, buffer)
	if err != nil { // if we failed to get any rules
		klog.Errorf("Failed to execute iptables-save, syncing all rules: %v", err)
	} else { // otherwise parse the output
		return utiliptables.GetChainLines(table, buffer.Bytes())
	}
	return nil
}

// writeIptablesRules 写入ipset各集合要挂载的iptables规则, 写入proxier.natRules和FilterRules.
// caller: proxier.syncProxyRules()
// @note: 由于proxy运行在pod中, 所以该函数中写入的规则都在pod内部, 宿主机上的是不一样的.
// writeIptablesRules write all iptables rules to proxier.natRules or
// proxier.FilterRules that ipvs proxier needed
// according to proxier.ipsetList information and
// the ipset match relationship that `ipsetWithIptablesChain` specified.
// some ipset(kubeClusterIPSet for example) have particular match rules and
// iptables jump relation should be sync separately.
func (proxier *Proxier) writeIptablesRules() {
	// We are creating those slices ones here to avoid memory reallocations
	// in every loop. Note that reuse the memory, instead of doing:
	//   slice = <some new slice>
	// 使用定长数组创建切片, 重用内存, 减少gc.
	// you should always do one of the below:
	//   slice = slice[:0] // and then append to it
	//   slice = append(slice[:0], ...)
	// To avoid growing this slice, we arbitrarily set its size to 64,
	// there is never more than that many arguments for a single line.
	// Note that even if we go over 64, it will still be correct - it
	// is just for efficiency, not correctness.
	// 长度64的数组应该够用了, 但就算超出了64也不过出错.
	args := make([]string, 64)

	// 我对比了一下 ipsetWithIptablesChain 与 ipsetList 中的集合, 
	// 前者要比后者多出两个, 所以遍历前者, 然后在循环内部对后者做判断并不会有遗漏.
	for _, set := range ipsetWithIptablesChain {
		_, find := proxier.ipsetList[set.name]
		if find && !proxier.ipsetList[set.name].isEmpty() {
			args = append(args[:0], "-A", set.from)
			if set.protocolMatch != "" {
				args = append(args, "-p", set.protocolMatch)
			}
			args = append(args,
				"-m", "comment", "--comment", proxier.ipsetList[set.name].getComment(),
				"-m", "set", "--match-set", proxier.ipsetList[set.name].Name,
				set.matchType,
			)
			// iptables -A KUBE-XXX -p tcp -m set --match-set setXXX dst -j RETURN;
			writeLine(proxier.natRules, append(args, "-j", set.to)...)
		}
	}

	// 如果kubeClusterIPSet列表中有值, 那么需要为此ipset指定匹配规则, 否则就不需要.
	// 该规则将挂载到 nat 的 KUBE-SERVICES 链下.
	// 
	// kubeClusterIPSet 集合并未在 ipsetWithIptablesChain 数组中, 这里单独创建ta的规则.
	// 这个set中存储的是service对象的IP:port.
	if !proxier.ipsetList[kubeClusterIPSet].isEmpty() {
		args = append(
			args[:0],
			"-A", string(kubeServicesChain),
			"-m", "comment", "--comment", proxier.ipsetList[kubeClusterIPSet].getComment(),
			"-m", "set", "--match-set", proxier.ipsetList[kubeClusterIPSet].Name,
		)
		if proxier.masqueradeAll {
			writeLine(proxier.natRules, append(
				args, "dst,dst",
				"-j", string(KubeMarkMasqChain))...,
			)
		} else if len(proxier.clusterCIDR) > 0 {
			// This masquerades off-cluster traffic to a service VIP.
			// The idea is that you can establish a static route for your Service range,
			// routing to any node, and that node will bridge into the Service for you.
			// Since that might bounce off-node, we masquerade here.
			// If/when we support "Local" policy for VIPs, we should update this.
			// 将来自集群外, 目标却是集群内部serviceIP的请求, 重定向到mark-masq链.
			// ...这一般是从宿主机到serviceIP的过程吧? 毕竟集群外是无法访问到service的.
			writeLine(proxier.natRules, append(
				args, "dst,dst", "! -s", proxier.clusterCIDR,
				"-j", string(KubeMarkMasqChain))...,
			)
		} else {
			// Masquerade all OUTPUT traffic coming from a service ip.
			// The kube dummy interface has all service VIPs assigned which
			// results in the service VIP being picked as the source IP to reach a VIP.
			// This leads to a connection from VIP:<random port> to
			// VIP:<service port>.
			// Always masquerading OUTPUT (node-originating) traffic with a VIP
			// source ip and service port destination fixes the outgoing connections.
			writeLine(proxier.natRules, append(
				args, "src,dst",
				"-j", string(KubeMarkMasqChain))...,
			)
		}
	}

	if !proxier.ipsetList[kubeExternalIPSet].isEmpty() {
		// Build masquerade rules for packets to external IPs.
		args = append(args[:0],
			"-A", string(kubeServicesChain),
			"-m", "comment", "--comment", proxier.ipsetList[kubeExternalIPSet].getComment(),
			"-m", "set", "--match-set", proxier.ipsetList[kubeExternalIPSet].Name,
			"dst,dst",
		)
		writeLine(proxier.natRules, append(args, "-j", string(KubeMarkMasqChain))...)
		// Allow traffic for external IPs that does not come from a bridge (i.e. not from a container)
		// nor from a local process to be forwarded to the service.
		// This rule roughly translates to "all traffic from off-machine".
		// This is imperfect in the face of network plugins that might not use a bridge, but we can revisit that later.
		externalTrafficOnlyArgs := append(args,
			"-m", "physdev", "!", "--physdev-is-in",
			"-m", "addrtype", "!", "--src-type", "LOCAL")
		writeLine(proxier.natRules, append(externalTrafficOnlyArgs, "-j", "ACCEPT")...)
		dstLocalOnlyArgs := append(args, "-m", "addrtype", "--dst-type", "LOCAL")
		// Allow traffic bound for external IPs that happen to be recognized as local IPs to stay local.
		// This covers cases like GCE load-balancers which get added to the local routing table.
		writeLine(proxier.natRules, append(dstLocalOnlyArgs, "-j", "ACCEPT")...)
	}

	// 把目标地址为本机的请求重定向到kube-node-port链中(不过master和worker节点上都并未发现此链.).
	// -A KUBE-SERVICES  -m addrtype  --dst-type LOCAL -j KUBE-NODE-PORT
	args = append(
		args[:0],
		"-A", string(kubeServicesChain),
		"-m", "addrtype", "--dst-type", "LOCAL",
	)
	writeLine(proxier.natRules, append(args, "-j", string(KubeNodePortChain))...)

	// 下面两条貌似只是单纯的重定向, mark-masq和mark-drop是自定义链, 没有默认策略,
	// proxy组件也没有在这两条链下写入规则, 而是由kubelet组件写的, 可以查看ta俩的注释.
	// 主要问题在于为什么要将load-balancer链的规则标记为drop, 
	// 以及firewall链下究竟都是些什么规则???
	// mark drop for KUBE-LOAD-BALANCER
	writeLine(proxier.natRules, []string{
		"-A", string(KubeLoadBalancerChain),
		"-j", string(KubeMarkMasqChain),
	}...)
	// mark drop for KUBE-FIRE-WALL
	writeLine(proxier.natRules, []string{
		"-A", string(KubeFireWallChain),
		"-j", string(KubeMarkDropChain),
	}...)

	// Accept all traffic with destination of ipvs virtual service, 
	//in case other iptables rules block the traffic, 
	// that may result in ipvs rules invalid.
	// Those rules must be in the end of KUBE-SERVICE chain
	proxier.acceptIPVSTraffic()

	// 下面这条以及后面if语句中的两条属于同一条链, 即 nat 表的 KUBE-FORWARD.
	// If the masqueradeMark has been added then we want to forward that same
	// traffic, this allows NodePort traffic to be forwarded even if the default
	// FORWARD policy is not accept.
	writeLine(proxier.filterRules,
		"-A", string(KubeForwardChain),
		"-m", "comment", "--comment", `"kubernetes forwarding rules"`,
		"-m", "mark", "--mark", proxier.masqueradeMark,
		"-j", "ACCEPT",
	)

	// The following rules can only be set if clusterCIDR has been defined.
	if len(proxier.clusterCIDR) != 0 {
		// The following two rules ensure the traffic after the initial packet
		// accepted by the "kubernetes forwarding rules" rule above will be
		// accepted, to be as specific as possible the traffic must be sourced
		// or destined to the clusterCIDR (to/from a pod).
		writeLine(proxier.filterRules,
			"-A", string(KubeForwardChain),
			"-s", proxier.clusterCIDR,
			"-m", "comment", "--comment", `"kubernetes forwarding conntrack pod source rule"`,
			"-m", "conntrack",
			"--ctstate", "RELATED,ESTABLISHED",
			"-j", "ACCEPT",
		)
		writeLine(proxier.filterRules,
			"-A", string(KubeForwardChain),
			"-m", "comment", "--comment", `"kubernetes forwarding conntrack pod destination rule"`,
			"-d", proxier.clusterCIDR,
			"-m", "conntrack",
			"--ctstate", "RELATED,ESTABLISHED",
			"-j", "ACCEPT",
		)
	}

	// Write the end-of-table markers.
	writeLine(proxier.filterRules, "COMMIT")
	writeLine(proxier.natRules, "COMMIT")
}

func (proxier *Proxier) acceptIPVSTraffic() {
	sets := []string{kubeClusterIPSet, kubeLoadBalancerSet}
	for _, set := range sets {
		var matchType string
		if !proxier.ipsetList[set].isEmpty() {
			switch proxier.ipsetList[set].SetType {
			case utilipset.BitmapPort:
				matchType = "dst"
			default:
				matchType = "dst,dst"
			}
			writeLine(proxier.natRules, []string{
				"-A", string(kubeServicesChain),
				"-m", "set", "--match-set", proxier.ipsetList[set].Name, matchType,
				"-j", "ACCEPT",
			}...)
		}
	}
}
