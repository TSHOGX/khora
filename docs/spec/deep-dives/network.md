# Network 深挖:egress 数据面怎么分层——策略权威 / 数据面行为 / 落位引流

> **定位:[`§5.D`](../subsystems.md)(spec,导航见 [`SPEC.md`](../SPEC.md))的网络深挖文档,并涉及
> [`§2.1`/`§2.4`](../SPEC.md) 安全支柱、[`§5.A`](../subsystems.md) provider 契约、[`§4`](../model.md)
> mediator 归属。** spec §5.D 明确边界(Network 持策略权威、enforcement 归 egress 数据面、引流归 placement、
> 凭证三层分解);本文展开那个真正查策略、注凭证、按需 MITM 的 **egress 数据面(gateway)如何分层**——
> 解成三个独立可换点 + 两条薄契约,并给出要守的 invariant 与仍待细化的议程。
>
> **编号约定:** 本文自己的节用 `D` 前缀(D1、D3.2…);`§` 一律指 spec 的全局章节(§5.D 等),两套不混。

---

## D0. 一句话结论

那个"查策略 + 注凭证 + 按需 MITM"的 **egress 数据面(下称 gateway),不是 runtime provider 物化出来的
东西,也不是引擎里写死的一段固定代码**。它内部还叠着**两根正交的可换轴**,必须现在就分开:

| 轴 | 是什么 | provider 相关? | 首个实现 | 未来加法 = |
| --- | --- | --- | --- | --- |
| **行为**(gateway *做什么*) | classify→decide→inject→按需 MITM | **无关** | `builtin`(自研基础版) | 换一个数据面实现(Envoy / 商用网关…) |
| **落位与引流**(gateway *跑在哪、流量怎么进来*) | 宿主进程+TPROXY / sidecar / per-node daemon | **特异** | 宿主进程 + netns/nft/TPROXY | 换一个 runtime provider(K8s…) |

连同**策略权威**(§5.D)与**凭证 at-rest 后端**(§5.D secret-store seam),一共**四块各自独立、各自只做
一个基础实现**:

| 块 | 归属 | 首个实现 | 加法轴(§1.3) |
| --- | --- | --- | --- |
| 策略权威(allow-deny / publish / 凭证路由) | §5.D | 控制面记录 | add a join table(Data) |
| **egress 数据面(行为)** | §5.D 的 behavior seam | `builtin` | register an implementation |
| **落位与引流** | §5.A runtime provider 的网络义务 | 宿主进程 + netns/TPROXY | register an implementation |
| 凭证 at-rest 后端 | §5.D secret-store seam | 宿主侧独立后端(选型未定,见 open-decisions) | register an implementation |

四块只经**两条传输无关的薄契约**相遇(D3):**resolve**(数据面↔凭证 resolver)与 **ingress**(数据面↔
引流)。两条在 single-node 下都是进程内调用,但**接口形状留成"走网络零改动"**。

这套设计**不发明任何新 seam 类型**——它把三类既有 seam 摆对位置,并补上 resolve / ingress 两条契约;
未来加法(Envoy / K8s / Vault / 新上游凭证)逐条落回 §1.3 三种加法之一,见 D8 litmus。

---

## D1. egress 关卡本质是三件事,不是一件

一个 egress 关卡要做的事,拆到底是**三件本质不同、各沿不同轴演化的东西**:

1. **引流**:把隔离单元的出站流量透明送进关卡(netns/iptables/TPROXY + `SO_ORIGINAL_DST` 取原目标)。
   netns/iptables 机制,**placement-local、provider 特异**。
2. **数据面行为**:classify(peek 字节认协议)→ decide(有序规则求值)→ 转发 → 按需 TLS-terminate-
   reoriginate MITM(每 SNI 现签叶子)/ SSH re-origination / header 注入 / placeholder 替换。
   **纯逻辑 + 承载凭证材料的进程**。
3. **策略与凭证权威**:谁持 `NetworkPolicy`、谁存凭证 at-rest、谁下发、谁广播轮转失效。**控制面的事**。

三件各走各的轴:引流随 provider,行为随数据面实现,权威在控制面(凭证 at-rest 后端是 D0 表列的第四块
secret-store 接缝,与这三件正交,本节不展开)。**只要把它们分开,"关卡跑在 K8s 还是
单机"就只是第 1 件(引流)的一种取值,不牵动另两件**——同一台 local_runsc 上就能把 builtin 换成 Envoy
(只动第 2 件),换 provider 也不动数据面逻辑(只动第 1 件)。

> **为什么现在就分。** 把这三件焊进一个 per-node daemon 是**分布式部署的产物**,不是它们本该在一起:
> 随之而来的一整套形态(节点级 daemon / annotation 下发 / informer / applied-hash 握手 /
> resolve-over-HTTP / NOTIFY / operator)**全是分布式传输 plumbing**,单机换成本地 channel / 进程内事件即可。
> 真正的机制内核只有四样:TPROXY 引流、classify→decide→转发核、凭证 alias/projection/cache/watermark 失效、
> guest CA 注入。

---

## D2. 两根正交轴:为什么必须现在就分开

D1 第 2 件(数据面行为)与第 1 件(落位与引流)是 D0 表列的两根**正交**轴,不能合成"gateway 归 provider"
一件。两轴正交的证据:**{builtin, Envoy} × {宿主进程, K8s sidecar} 每一格都是可独立注册的配置**——换行为不动
引流、换引流不动行为。一旦把 gateway 塞进 runtime provider 接口当"物化的一部分",两根轴被焊成一根,
四格退化成对角线两格(只剩"单机+builtin""K8s+Envoy")。

**但正交是接口层的可分离,不是能力无关**:每一格**能做到的行为上限**受该引流暴露的劫持能力约束
(见 D3.2)。"能独立注册"与"注册进来就什么都能做"是两回事——接口把两轴解开,让它们能各自替换
并**显式协商**能力档位,而不是假装行为不依赖引流。焊死时连协商的机会都没有。

**为什么不能拖到有第二个实现再拆**(§1.3 成本判据自查):

- **数据面本身就握 MITM CA 私钥、就要够到 secret-store 去 resolve model key**。若把 gateway 当作
  provider 物化的一部分,凭证 resolve 会顺势泄漏进 §5.A provider 契约,§2.4"支柱④ 与 provider 无关"
  当场破产。这是**接口设计的正确性问题**,不是为假想未来预留。
- 落在判据"该建"侧:seam / invariant 是为 single-node 实现本就该做的好设计,顺便成为未来落点。
  **接口边界的形状**承重,具体的多实现注册机制不承重(D6)。

---

## D3. 两条薄契约:四块只在这两处相遇

四块解耦的关键不是"各写各的",而是**它们之间只暴露两条窄接口**,且两条都**传输无关**——single-node 下是
进程内调用,接口形状留成"数据面被摆成远端 sidecar、走网络调用零改动"。这与
[`transport.md`](./transport.md) D3"明确将来加它不回头改 engine 的接缝"是同一种纪律。

### D3.1 resolve 契约(数据面 ↔ 凭证 resolver)

数据面注凭证时**不直连 secret-store**,而是经一个 resolver handle 拿注入材料:

- **形状**:`(alias, 目标上下文{host/port/protocol}) → (注入 directives, 过期时刻 expiresAt, version 戳)`
  + **失效信号**(某 credential source 轮转 → 通知数据面按 version watermark evict 缓存)。
- **数据面侧**:持 TTL 缓存 + source-version watermark(读到旧于 watermark 的响应触发 re-resolve;
  TTL 是 NOTIFY 失效失灵时的兜底一致性界)。
- **为什么是契约不是直读**:single-node 里 resolver 可以是进程内调用,但 **K8s 里 sidecar 数据面必须
  回调控制面**。若接口写成"数据面直读本地 secret-store 内存句柄",远端形态一来就得改引擎。契约让
  resolver 的**落位**(进程内 / 远端 HTTP)成为实现细节。
- **与 secret-store seam 的关系**:secret-store(§5.D 已登记 seam)管**凭证怎么存**(AES-GCM / Vault);
  resolve 契约管 **alias→directives 怎么变出来 + 缓存/失效**。resolver 站在 secret-store 前面:它读
  secret-store,数据面读它。两个 seam,别合并。

### D3.2 ingress 契约(数据面 ↔ 引流)

引流(provider)把 egress 送进数据面时,不能直接知道"数据面在宿主哪个路径/端口"——那会把落位轴焊回
provider。这条契约是**双向能力协商**,不是单向的地址下发:

- **形状(双向)**:
  - 数据面 → 引流:一个**抽象 ingress 描述符**(不是宿主绝对路径)+ **要求的劫持能力**(需不需要原目标
    恢复、透明重定向、DNS 拦截、UDP)。
  - 引流 → 数据面:它**能提供的劫持能力**(透明 TPROXY+`SO_ORIGINAL_DST` / 仅 NAT REDIRECT / 仅显式
    proxy 端点)。
  - 两边取交集,定出"这次数据面能做到哪档"(如引流只给 NAT 无原目标 → 数据面降级到 SNI 路由、做不了
    需要原 IP:port 的精确策略;引流不给透明重定向 → MITM 不可用)。**能力不匹配是 placement 失败
    (D4 半建回收),不是静默降级到不安全。**
- 引流按描述符把隔离单元的 egress **全量**导入(exhaustive,见 D5.1)。
- **随 placement 重绑**:每次 resume,ingress 描述符随新 placement 重新分配,兑现 §3.3"身份不编码
  placement"——和 transport 的 netns IP 换新同源(§5.E)。
- **退化允许、抽象不许省**:single-node 里描述符可以退化成"本 netns 内一个约定端口",但**接口层必须是
  描述符**,不能让 provider 代码里出现"gateway 的宿主路径"。这是 §6.2"对外不暴露宿主路径"贯彻到引擎
  内部(同 §3.1.1 对 Overlay 的要求)。

---

## D4. 编排:mediator 穿针,子系统不互调(守 §4 归属)

两条契约意味着 resume 时有个**次序**,且必须由 §4 mediator 编排、不能让 Network 去调 provider(否则
违反 §4"子系统间永不互调"):

```
resume(mediator 编排):
  1. §5.D 经数据面 seam 建/选 gateway(喂:策略 + resolver handle)→ 拿回 ingress 描述符
  2. mediator 把 ingress 描述符交给 §5.A provider
  3. §5.A provider 建 netns/IP + 引流(把 egress 全量强制导入该描述符)+ 拉起 runsc
  4. (§5.B 重挂 Overlay、§5.C 重挂 Volume,见 §4 / guest-runtime.md D5 的 canonical 链)
```

- **handle 由 mediator 穿针**:Network(D)产出 ingress 描述符,mediator 转交 provider(A);A 与 D
  不互相持有引用、不互调。这正是 §4"动词归 §4、机制归 §5"的落点——mediator 持"何时建、拿谁的产物喂
  谁",子系统只出机制。
- **失败语义复用 §4.4-A**:gateway 建了、引流没建成 = 半建 placement,失败**回收半建 placement + 退回
  源态**,无新中间态。gateway 是 placement-local 资源(和 netns/nft/overlay-mount 同类),不是持久态,
  不损坏 Overlay/Volume。
- **落位是 provider 的业务、经契约 B 汇合**:数据面"跑在哪"本身也随 provider 变(宿主进程 / sidecar /
  per-node daemon)。解法不是再切一个 seam,而是一条原则——**引擎交给 provider 一个 ingress 要求
  ("egress 必须落到一个说 ingress 契约的数据面、从隔离单元可达、不可绕过"),provider 按自己的世界满足
  它**。引擎不假设数据面是本地子进程。

> **精确边界:provider 不进 resolve 路径**(不是"永不见凭证字节")。区别要害:K8s 里凭证密文可能**物理
> 流经** provider 调度的基础设施(sidecar 所在 pod、它的网络);但 provider **逻辑上不解析 alias、不调
> resolver、不持 alias→directives 的求值权**——resolve 路径(D3.1)完全在引擎数据面 + resolver 之间。
> 这才是可验收的义务:不是"密文碰不到 provider 的地盘"(远端形态下做不到也不必),而是"provider 不是
> resolve 的参与方"。同理 provider **不持策略权威**,但会收到**由策略派生的引流要求**(default-deny +
> steering directives)——收指令 ≠ 持权威。

---

## D5. 要守的 invariant(规范陈述在 §2.3 / §2.4 / §5.D)

### D5.1 引流不可绕过(steering 必须 exhaustive)

provider 的引流义务**不是**"能到 gateway",而是**默认拒绝 + 仅放行到 gateway**(sandbox nft 只放行到
gateway + 显式端点)。兜底强制的分层 deny-set(挡 metadata/内网/SSRF 的确切分层与 IP-承重墙)见 D5.3——
本节只钉「引流穷尽、不可绕过」这一面。

**不守这条,策略权威形同虚设**:guest 里一条 `--noproxy` 直连、或走一个没被引流覆盖的协议/端口,就
绕过了整个 §5.D 策略面。故它是 provider 的**可验收安全义务**(spec §2.4):

> 把隔离单元的**每一条 egress** exhaustive 地强制导入引擎指定的 gateway ingress,默认拒绝其余出站。
> **逃逸即失败**,不是降级。

与支柱④"provider 无关"分工清晰:**provider 保证流量不漏**(把每条 egress 强制导入关卡)、**凭证注入发生在
引擎数据面内**——前者是引流义务,后者是数据面行为,分属两轴;于是换 provider 只换引流,resolve 路径分毫
不动(精确边界见 D4)。

### D5.3 分层 deny-set:IP 是承重墙,域名是便利层

egress 强制的**承重墙是 dial 那一跳对「钉死的 IP」跑的 deny-set,不是域名白名单**。域名控制只是叠在
IP 层之上的**便利 + 审计层,不是安全边界**:guest 用 DNS-over-HTTPS(443 到公网 resolver)把查询藏进
TLS,gateway 看不到真实名字;即便上 MITM,guest 也能直连 **IP 字面量**完全跳过 DNS。域名策略只在
「gateway 是唯一 resolver **且** :53/:853/443-DoH 出站被 deny」时才 best-effort 成立,**永远不是兜底**。

**dial 时钉 IP 一次、禁止连接期再解析**,一举覆盖三种域名层拦不住的逃逸:① 重绑定 TOCTOU(鉴权时
解析到公网、dial 时重解析到内网)· ② IP 直连(guest 根本不做 DNS)· ③ Host/SNI 与目的 IP 背离。

**三档姿态的清单(L0 地板 / 内网私有段 deny-safe / 公网默认放行)的规范陈述在 §2.4,本文不复制**;此处只留
它们各自的判据与那一层不属 IP 的便利层:

- **L0 · 不可覆盖地板**的**成员判据**:进 L0 当且仅当 (a) 到达它**击穿某条引擎 invariant**(宿主隔离 /
  控制面权威 / 跨沙箱隔离)且 (b) 引擎**不靠部署配置就能命名它**(IETF 保留段 / 自引用 / placement 自知)。
  由此**云 metadata 不单列**:AWS/GCP/Azure 的 metadata 都在 `169.254.169.254`(link-local)、被 L0 天然
  覆盖;`metadata.google.internal` 这类主机名也不进地板——它解析出的 IP 落 link-local,dial 时照样拦。
  **引擎因此不认识任何云厂商**,守 §1.1「不假设部署环境」。
- **内网私有段的 deny-safe 是配置默认、不是 code**:业务可整体改姿态,与 L0 的 code 强制截然不同,
  兑现"引擎不独裁、不侵入业务网络,出厂安全但决定权在业务"。
- **L2 域名 allow-list · 便利 + 审计层(best-effort)**:叠在 IP 层之上给人可读的放行/审计,DoH/IP 直连
  可绕,**不承担安全边界**。

### D5.4 deny-set 必须钉在两跳上(只钉一跳拦不住重绑定)

egress 有两跳,deny-set 少钉一跳就漏:

1. **guest → gateway(steering nft,provider 侧)**:default-deny + 只放行到 gateway ingress。这是
   D5.1 的引流义务,拦的是 guest **绕过 gateway 的直连**(含直连 sibling、直连 metadata)。
2. **gateway → 上游(onward dial,数据面侧)**:gateway 收下"发往可信域名"的流量后,**自己 dial
   解析出的 IP**——**重绑定恰恰发生在这一跳**。故 dial 前必须对钉死的 IP **再跑一遍 deny-set**(L0 地板
   + 内网私有段)。

只钉 ①,重绑定照样成立(guest→gateway 合法、gateway→内网无人拦);只钉 ②,guest 一条 `--noproxy`
直连就绕过整个数据面。**两跳同一套 deny-set、缺一不可。**

> 归属:① 是 §5.A provider 的 steering 物化(placement-local);② 是 §5.D 数据面 behavior 的 dial
> 逻辑。deny-set 的 **L0 清单是引擎资产**(像 D7 的 classify/decide reference),两跳都引用同一份,
> 不各自维护——否则两跳语义漂移,地板被从缝里穿过。

### D5.5 跨沙箱网络隔离:为什么支柱①管不到,靠什么守

跨沙箱**网络**隔离是一条**独立的 control-plane invariant**,不搭支柱① 的车(结论在 §2.3,推理在此)。
**支柱①(syscall 拦截)守不住它**:它虚拟化 syscall,不拦网络包——guest 合法用网络,包本就要出去。存储
跨沙箱隔离(其他沙箱的 Overlay/Volume)能靠支柱①,网络可达性不能。single-node 若所有沙箱 netns 挂同一
L2 网桥同网段,sandbox A 能直连 sandbox B 的 veth IP(§5.E transport 正是用 veth IP 直连,sibling 在网桥
上有可路由 IP),完全绕开 gateway。

D5.1「默认拒绝其余出站」字面能涵盖,但"出站"通常指南北向,**东西向(sibling IP)是否在内有歧义**——故
§2.3 把它显式钉成 invariant(可达集恰好 = {gateway ingress,策略显式放行的端点},sibling 经 gateway 中转
而非 L2 直连)。本文只补它的**物化**(placement/provider 细节):

**(i) steering nft 把 sibling 网段纳入 default-deny(sibling IP 进 L0 地板)是这条 invariant 唯一的物化方,
不是兜底。** 点对点 veth 替代不了它:**消除二层邻接并不消除可达性**——宿主在两个 `/30` 之间**路由**
(`ip_forward=1`),东西向根本不需要 L2 邻接。实测:两个真 runsc 沙箱各自一对 `/30`、无共享网桥(即
`local_runsc` 今天的形态),sandbox A **完成了到 sibling B guest IP 的 TCP 握手并读到其载荷**。

**(ii) 每沙箱点对点 veth、不共享 L2 网桥**因此是**纵深防御的一层**(它关掉"同网段直连"这一条路径,且是
今天的形态),**不是那条 invariant 的物化方**。

**两条连带的物化约束(同一次实测)**:① 规则**不能只写在沙箱 netns 内部**——gVisor netstack 经 L2
(`fdbased`/`SOCK_RAW`)注入,guest 报文**不遍历该 netns 的 L3 钩子**(实测 5 个包在 netns `inet
output`/`postrouting` 计数为 **+0**,在 `netdev egress` 为 +5,在**宿主侧** `prerouting` 全见);故
enforcement 挂**宿主端 veth**或 `netdev`/`egress` 钩子(§5.A 的"nft 打在 per-sandbox netns 上"要按此
读:是宿主侧针对该 netns 的 veth,不是 netns 内的 filter 表)。这同时意味着**放在沙箱 netns 内的 TPROXY
不成立**。② 谓词必须**按入接口(`iif`)**而非按源地址:guest root 能给自己网卡加地址(实测 `ip addr add`
rc=0),**`saddr` 谓词被绕过**(3 个包命中"已绕过"计数器),`iif` 谓词在同一攻击下全数拦住。

> **⚠ 今天 (i) 尚未落地,故 §2.3 的 sibling-不可达不成立**——缺口、连带影响与 owning plan 见
> [`open-decisions.md`](../open-decisions.md) §4。

### D5.6 egress 全程 fail-closed(两处失败同源)

resolve 失败的默认行为是**顶层安全基调,不是 resolve 契约的 schema 细节**,现在就能拍:**fail-closed**。
理由比"上游会拒"更硬——若 fail-open 原样转发,在 placeholder/header 注入模式下会把**带 alias 占位符的
原始请求**发给真上游,等于泄漏别名机制、或发出 guest 可观测/复用的未鉴权请求,当场破支柱④"guest 永不
见真凭证、gateway 是唯一注入方"。

作用域**per-connection**:只拒那条"需注入却 resolve 不到"的连接,不牵连其余已放行 egress。它与 D5.1
拼成一个自洽基调——**egress 全程 fail-closed**:

| 失败点 | 机制 | 结果 |
| --- | --- | --- |
| steering 失败(gateway 不可达,D5.1) | provider nft default-deny | guest 直连被断 |
| injection 失败(resolve 不到,本节) | 数据面拒该连接 | 该连接 fail-closed,余者不受累 |

两处都是"逃逸/降级即失败,不静默降级到不安全"。

---

## D6. 接口形状 vs registry plumbing:哪一半现在就该存在

对 §1.3 判据最诚实的一刀——**"四块独立可换"不等于"四个 open registry"**,后者会踩 reservation。要建的
是**接口边界的形状**:两条契约(D3)+ 数据面模块不碰宿主原语、不假设"我是本地子进程";具体的槽
(registry plumbing)随第二个实现出现才建。这正是 §1.3「微妙时序」:seam 形状现在建,槽将来建。四块
各只有一个实现时,注册机制本身就是 reservation。

---

## D7. classify / decide 归属:策略权威留 §5.D,数据面两种强制模式

gateway seam 的接口有个边界抉择:**协议分类(classify)+ 策略求值(decide)算数据面 impl 的义务,还是
引擎给所有数据面复用的公共裁决?**

定论:**引擎持策略 schema + 一份 reference 求值语义(§5.D 权威);数据面 impl "按这语义强制 + 注入"。**
但"按语义强制"有**两种模式**,不能退化成一句"数据面只执行"——它们对接口的要求不同:

- **模式 1 · 内联求值(builtin 走这条)**:数据面**逐连接调用引擎的 classify/decide 函数**拿裁决,自己
  只做转发+注入。策略语义 100% 由引擎那份 reference 实现产生,零偏移。纯逻辑、无宿主/K8s 耦合。
  **接口要求**:引擎把 classify/decide 暴露成数据面可调用的裁决函数。
- **模式 2 · 编译下发(Envoy / 商用网关走这条)**:引擎把策略 schema **编译**成实现的原生配置
  (Envoy RBAC/filter、nft 集…),裁决在实现内部用它自己的引擎跑。**语义等价靠"编译器正确"担保,而非
  逐连接回调**。**接口要求**:引擎提供 policy→native-config 的编译产物 + 一个一致性校验点(编译后语义
  必须等价于 reference,否则拒绝上线——例如编译后回写一个 applied-hash 供控制面校验已生效配置)。

**两种模式策略权威都在 §5.D**:模式 1 靠内联同一份实现,模式 2 靠"编译等价 + 校验"。关键是**编译器是引擎
的资产、不是各实现自研**——否则策略语义随实现偏移,破 §5.D 权威。接口取向随之定:gateway seam 不是"给你
流量你全权处置",而是**要么"给你流量+我的裁决函数"(模式 1)、要么"给你我编译好的原生策略+校验门"(模式
2)**。builtin 走模式 1;模式 2 的编译器接口是 seam 形状,随第二个实现出现才搭(D6)。

---

## D8. litmus 自查(§1.3):全部落回三种加法之一

| 未来需求 | 归到哪类加法 | 需要新 seam 类型? |
| --- | --- | --- |
| 换数据面实现(Envoy / 商用网关) | register an implementation(behavior) | 否 |
| 扩到 K8s / 别的调度后端 | register a runtime provider(behavior) | 否 |
| 换凭证 at-rest 后端(Vault) | register an implementation(secret-store) | 否 |
| 新上游凭证 / 新出站路由 | add policy rows(Data join table) | 否 |
| egress 审计喂计费/observability | add a subscriber(Consumption,§4.1 event stream) | 否 |

**全 additive,不发明新 seam 类型**——这正是 §1.3 要的形状。

---

## D9. 仍待细化的议程(登记 open-decisions)

- **注入模式**:MITM(TLS terminate-reoriginate,每 SNI 现签叶子)/ 明文 alias 端点 / placeholder 替换 /
  header 注入 / SSH re-origination——**别过早锁定 MITM 为唯一路径**。哪些模式进 builtin、各自的约束
  (如 body 替换要求已知 Content-Length、非压缩、≤1MiB)待定。
- **guest 信任注入**(仅当"TLS-terminate 注入"模式被选中才需要):CA **证书**(非私钥)投进 guest、
  私钥留数据面、guest 导出 `SSL_CERT_FILE`/`NODE_EXTRA_CA_CERTS`/`REQUESTS_CA_BUNDLE`/`CURL_CA_BUNDLE`
  等一串环境变量指向它。这是 placement-local 的 guest-provisioning 叶子,落 §5.B
  模板默认 / §5.E.1 `sandboxd` 启动。
- **resolve 契约的精确 schema**:directives 的具体形态、目标上下文字段、失效信号的投递(NOTIFY 类
  推送 vs 数据面轮询)。
- **ingress 描述符的精确形态**:single-node 退化端点 与 抽象描述符的最小公共接口。
- **数据面 behavior seam 的接口精确签名**:与 §5.A provider 接口同批次定,确保两轴在签名层就分开、
  且签名里不出现"凭证/宿主原语"字样。
- **D5.3–D5.6 的残留细粒度**:L0 地板精确清单 / 内网放行的 NetworkPolicy 语法 / fail-closed 是否发
  §4.1 审计事件。

> 以上逐条登记在 [`open-decisions.md`](../open-decisions.md)。D0–D8 的规范陈述在 §2.1/§2.3/§2.4/§5.D;
> 本文只展开推理与要守的 invariant,不复制 spec 正文。

