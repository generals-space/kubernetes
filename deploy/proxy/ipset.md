
参考文章

1. [Advanced Firewall Configurations with ipset](https://www.linuxjournal.com/content/advanced-firewall-configurations-ipset)
1. [Stateful matching of multicast responses in iptables](https://serverfault.com/questions/250797/stateful-matching-of-multicast-responses-in-iptables/911286)
3. [iptables/ipset match-set not working](https://www.linuxquestions.org/questions/linux-networking-3/iptables-ipset-match-set-not-working-4175514709/)

> `man iptables-extensions`可以查看`--match-set`的使用方法(`man iptables`中没有).

在阅读proxy源码时, 发现在创建iptables规则时使用了类似如下的命令.

```
iptables -A INPUT -p tcp -m set --match-set myset dst,dst,src -j DROP
```

`--match-set myset`如果不指定后面的`dst/src`时, 默认为src, 即请求的来源地址与myset集合中声明的地址匹配.

如果写成`--match-set myset src,dst`, 就要求myset的类型是ip,ip的形式了, 因为要分别匹配.

比如上面的`dst,dst,src`, 则需要myset设置type为`ip,port,ip`

```
ipset create myset hash:ip,port,ip
```

这样, 在添加时

```
ipset add foo 192.168.1.1,80,10.0.0.1
```

只有在请求的目标IP为`192.168.1.1`, 目标端口为`80`, 并且来源IP为`10.0.0.1`, 才算匹配成功...

