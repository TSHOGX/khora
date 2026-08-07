# M8 — Network(可达集从"巧合"变成"被强制")

> ladder 见 [`README.md`](./README.md);spec 锚点见文末「附」。
>
> **设计已全部收敛进 spec**(§2.4 / §5.D / §6.1 / §6.2,推导与证伪在
> [`deep-dives/network.md`](../spec/deep-dives/network.md) D0–D8),本文**不复述契约**,只写落地要的六件事:
> 边界怎么切、实测约束怎么约束写法、声明怎么落库、改哪些代码、怎么验收、内部怎么分门。
>
> **编号约定**:裸 `§N` 一律指 spec 全局章节;`Dx` 指 network.md 的节;指本文自己的节时写「本文 §N」。

---

## 0. 一句话结论(先读这句)

把 §5.D 从"spec 里已定、代码一行没有"变成真交付:**一个 `internal/network` 子系统**(纯函数:
`(声明, L0 地板) → 抽象规则集`)、**声明落进沙箱记录**(四字段 Binding + 一个热加写点)、
**`SteeringSpec` 从空结构变成真载荷**、**provider 侧的渲染与安装**(每沙箱一张 nft 表,装在建 netns 之后、
拉起隔离单元之前)、**出网所需的三件宿主机制**(今天一件都没有,故 `default:allow` 今天无从物化),
以及**规则集作为崩溃幸存宿主态进入 §4.4-A 对账**。

它是 ladder 的最后一级,也是**唯一安全关键的一级**:验收的重点不是"能通",而是**那些会静默通过的性质**
(D4.1 / D5 ⑧)。

**不需要任何新 seam 类型**:`SteeringSpec` 是 §5.A 契约里既有的成员,`arch` 守卫已预先登记
`internal/network`。

---

## 1. 起点:今天的现状(spec 各处指向此处)

**§2.4 三条 invariant 今天无一成立。** spec 用现在时描述它们(§2.1 ③④ / §2.4 / §5.D),那是**义务规格**,
不是对当前代码的陈述——本节是那份陈述的唯一出处,spec 侧只留指针。

- **代码一行未落**:无 `internal/network` 包,无 nft 规则集,无 L0 地板;`CREATE TABLE sandboxes` 里
  **没有存那份声明的列**,故声明今天连落库处都没有。`local_runsc` 只建 netns + veth + 两个 `/30`。
- **今天的"不可达"是巧合,不是强制**:沙箱到不了公网 / sibling / link-local 是"没有默认路由"**且**
  "没有宿主侧 SNAT"两件事一起,而 guest root 持 `CAP_NET_ADMIN`、加得动那条路由(逐条与逃逸步数见
  [`network.md`](../spec/deep-dives/network.md) D5 ⑤;sibling 已实测握手成功并读到载荷,D5 ①)。宿主
  loopback 与 orchestrator 控制面是被 `127.0.0.1` bind + `route_localnet=0` **偶然**覆盖的,同样不是强制。
- **支柱④ 只有 substrate 那一半**:每沙箱独立 netns + 点对点 veth 已在,**谓词那半没有**——它要等规则集
  落地才有意义。
- **两个端点 404** :`GET …/network` 与 `POST …/publish` 落到 sandbox 子路由的 `default` 分支。
- **`POST /v1/sandboxes` 的 `network?` 被静默丢弃**:它是未知键,而全仓**无一处 `DisallowUnknownFields`**,
  故调用方拿到 201 会以为声明已生效。
- **IPv6 是一个默认打开的地址族,比"未建"更糟**:引擎从不碰 IPv6 而 `disable_ipv6` 默认 0,故内核给每条
  沙箱 veth 自动配宿主侧 `fe80::/64`,本机 `eth0` 又有真实 IPv6;实测**无地板时 guest 不仅可达宿主侧 veth
  的链路本地,还可达 `eth0` 的全局 IPv6**。**它不需要地板设计上的任何新东西**——一条不带 family 限定的纯
  ct-state `inet` 规则连 IPv6 一起覆盖(D5 ⑪),故这一族的敞开纯粹是"地板一行未落"的后果,与 IPv4 那一半
  同时消失。

**本节随 M8 落地而整节删除**(plan expire 时一并消失)。

---

## 2. 范围(in scope)

| # | 件 | 物化 |
| --- | --- | --- |
| **1** | `internal/network` 子系统(§5.D) | 纯函数规则生成:`(声明, L0 地板) → 抽象规则集`。**placement-free、免 root、可单测** |
| **2** | L0 地板,住一个 leaf 包 | 四项全静态(D5 ④⑧):link-local(v4+v6)/ loopback / sibling CIDR / 宿主自身(ct-state)。**两跳共用一份** |
| **3** | 声明落库 | `sandboxes` 表新增声明列(§3.4 Binding);`CREATE TABLE` 直接改,**不写迁移**(无兼容负担) |
| **4** | 声明的八类 400 + 校验在准入门之前 | §6.2 那张清单逐条,**含 `via[].endpoint` 非宿主本地 / 落在地板内**两类,**外加 `egress.allow[]` 整段落在地板 loopback / link-local 项内**(判据是整段包含、不判 `port`/`proto`、**不覆盖 sibling CIDR 与宿主本地**——前者已因 `input` 侧也发声明放行而是活规则、是 §5.D 语义 ③ 的声明路径,D4.2 / D5 ㉑)与 **v4-mapped 字面量当场拒**(D5 ⑲,它是前一类的必要前置) |
| **5** | `SteeringSpec` 变成真载荷 | 抽象规则集 + publish 端点描述符;**签名里不得出现宿主原语**(§5.A ❌ 清单,`seam_test.go` 已守) |
| **6** | provider 侧渲染 + 安装 | 每沙箱**一张 nft 表**;`iifname` / guest IP / publish 宿主端点在 placement 时绑定 |
| **7** | 安装与 placement 原子 | 装在**建 netns/veth 之后、拉起隔离单元之前**(§2.4);半建失败走 §4.4-A 回收 |
| **8** | 出网三件宿主机制 | `ip_forward` · guest 侧带 `dev` 的默认路由 · 该 `/30` 的 masquerade(D5 ⑤;**今天一件都没有**)。**那条路由必须在 `runsc run` 之前装**:sentry 在 boot 时把 L3 地址从内核侧 veth 收走,boot 后 `ip route add` 报 `Nexthop has invalid gateway`(D5 ⑬)。**反面也量过**:`rp_filter` / `proxy_arp` / `accept_local` / per-if `forwarding` **都不需要**,别加 |
| **8b** | publish 的两条伴随规则 | 一条 **`postrouting` SNAT 到本沙箱 `/30` 网关**(否则 publish 只在"guest 恰好有默认路由"时能用,D5 ⑬)· 一条 **`output` 钩子的 DNAT**(否则宿主自身拨不通,而这与"外部拨得通"是两条独立路径,D5 ⑭)。伴随 accept 挂 **`forward`**,不是 `input`(D5 ⑮) |
| **9** | 两个 §6 端点 | `GET …/network`(今天 404)· `POST …/publish`(今天 404,热加写点) |
| **10** | `network` 进 `PATCH` + create | 整份换整份、`next-resume` 生效;create 的 `network?` 今天被**静默丢弃** |
| **11** | 规则集进 §4.4-A 对账 | `Adopt` 重建、`ReclaimOrphans` 拆除、按表名前缀枚举孤儿表。**三条硬约束(D5 ㉒,违反任一条都会掐断活连接)**:重装**必须是单个原子 `nft -f` 文件**(先 `delete` 再装 = 5/5 断流)· **对账全程不许清 conntrack**(`tcp_loose=0` 下清一次就断)· `ReclaimOrphans` 的 keep 集**必须在任何删表之前算好**(否则"先 reclaim 后 adopt"逐字就是那条会断流的写法)。**反面是个许可**:满足这三条时 `Adopt` 可以对捡到的表不做任何推理、直接整份重新渲染装上,不必 diff |
| **12** | 三条渲染守卫(结构性) | 拒 `policy drop` 于每沙箱基链 · 拒 `iif`(只许 `iifname`)· 拒**未带 `iifname` 的 accept**。理由见本文 §4 ②③ 与 D5 ⑩ |

**第 8 件是"`egress.default` 有两个取值"的兑现面,不是附加便利**:今天 `allow` 与 `deny` 在行为上
**不可区分**(两者都不通),故不落它就无法验收"声明被强制"——一个恒不通的引擎在每条 deny 断言上都
假过。

**第 12 件不是防御性编程**:三者都是**一个词的编辑就破安全、且没有任何测试会响**(D5 ⑧⑩⑫ / 本文 §4),
与 `seam_test.go` 的 token 守卫同源——那条守卫存在的理由逐字适用。**`iif` 那条尤其**:`nft list` 会把冻结的
ifindex 反解回名字,故**评审读渲染输出看不出差别**,而症状只在 veth 重建后显形、且表现为"计数器为 0"
——与"还没有流量"不可区分(D5 ⑫)。**同一条实测还带出一个未量但更坏的风险**:若某个退役的 ifindex 被
**别的**沙箱的 veth 复用,一条 stale `iif` 规则会开始匹配**那个沙箱**的流量——不是漏洞而是**静默的跨沙箱
错配**。未量,只增强这条守卫的理由。

---

## 3. 边界怎么切(生成 vs 渲染,两半在哪相遇)

| | `internal/network`(§5.D) | `local_runsc`(§5.A) |
| --- | --- | --- |
| 输入 | 声明 + L0 地板 | `SteeringSpec` |
| 输出 | **抽象规则集** | nft 文本 → 装进内核 |
| 知道 | CIDR / port / proto / "哪一跳" | `iifname`、guest IP、publish 宿主端点 |
| **不**知道 | 任何 placement 产物 | 声明本身(不持权威) |
| 可测性 | **纯函数,单测全覆盖取值组合** | 要 root + 真 substrate |

切在这里由 D1 ② / D6 定:那三样宿主原语只有 provider 持有,抬上跨 provider 契约面就是 §5.A ❌ 清单里的
泄漏。**收益是可测性**:声明取值组合的全部差异都在免 root 的单测里钉住,substrate 测试只需证"装上了、
且顺序对"。

---

## 4. 实测约束怎么约束写法(九条,别重新发现)

> 全部在 `sbx` 上量过(含真 runsc guest)。证据在 D5 逐条,本文只写**它约束了什么写法**。

**① 规则集形状:每沙箱一张表,基链在 `input` 与 `forward` 两处。** 表是可原子删除的命名单元,正好落进
`enumeratePlacements` / `teardownPlacement` 既有形状。**只做 `forward` 会整个漏掉 guest→宿主**(实测)。

**② 每沙箱基链必须 `policy accept`,默认拒绝写成 `iifname` 收窄的最后一条规则。** 链 policy **无法**被
任何入接口谓词收窄(`iifname` 也不行,这是 policy 这个位置本身的性质),故一张表里的 `policy drop` 是
**跨沙箱杀伤开关**——实测它在自己那条规则一个包都没命中的情况下
杀掉了另一个沙箱的流量。⟹ **渲染守卫(第 12 件)**。

**③ 只许 `iifname`,禁 `iif`。** `iif` 在**装载时**把名字解析成 ifindex(字节码里是一个冻结整数),veth
同名重建后**静默 fail-open**——实测 `iif`-only 地板下 guest 的包**被投递**而 drop 计数器为 **0**(D5 ⑫)。
`iif` 还**不能在接口存在前装载**(整份 `-f` 原子失败),与第 7 件的次序冲突;`iifname` 两个问题都没有。
**评审看不出来**:`nft list` 平时把那个整数反解回名字,两种写法在列表里长得一样。⟹ **渲染守卫(第 12 件)**。

**④ `input` 侧不带 daddr 清单,`forward` 侧必须带、且必须有自己的 default-deny。** "非 established"在宿主
全部地址上是穷尽的,故**宿主自身的那些地址从不被算出**——地板第四项是"宿主自身的任一地址",由这一条纯
ct-state 规则静态覆盖,引擎因此不必知道自己的 bind 地址(§2.4 / D5 ⑧)。**三条附带约束**:① sibling drop
**之前**须放行"本沙箱自己的 `/30` 网关",否则连带杀掉宿主本地解析器(D5 ⑦);**该放行落 `input`**——那个
网关是宿主自己的地址,声明的放行因此**两个钩子都发**,且 `input` 侧**必须自带一条 sibling drop**(否则一条
`allow 10.0.0.0/8` 让沙箱 A 连上 sibling B 的网关;`forward` 侧那条 sibling drop 对这条路径计 0,D4.2);
② **`via[]` 的窄放行**——形状已收口成"同时钉住 pre-NAT 元组 + post-NAT 端口 + `iifname`",四条实测反例说明三样都承重,**逐条见
D5 ⑩,本文不复制**;③ **`forward` 侧不是只有 daddr 清单**:一条目标不在宿主上、又未被引流的流量根本不经
`input`,故那条 default-deny drop 要在 `forward` 侧自己有一份(D5 ⑩ 的 `input` 行读法陷阱)。

**⑤ `delete table` 对不存在的表 rc=1 且**会中止整个 `-f` 文件**。** 用 bare-`table` + `delete` + 定义
三连写在一个文件里(幂等且原子),或 `destroy table`。附带:nft 会把地址位置上的裸词**经宿主 resolver
解析**,故渲染器**只许发数字字面量**——一个畸形地址会静默变成一条匹配无关 IP 的合法规则。

**⑥ 面向 guest 的拒绝用 `reject`,不用 `drop`。** gVisor netstack **认 ICMP 差错**,故同一条"拒绝"两种
写法的 guest 体验差数个数量级(D5 ⑨)。**纯 `drop` 的地板满足 fail-closed 的安全那一半、违反它自己写下的
"guest 得到一个干净的连接失败"那一半**——两半都要。安全性不受影响(两者都不放行)。**UDP 上要用
`icmp port-unreachable`**:`admin-prohibited` 被 gVisor 忽略。**注意证据强度**:这一条是本节九条里**唯一
单来源且复现未成**的(D5 ⑨ 的 ⚠ 块),方向可信、数字待复现,故门 B 第 8b 条按量级断言并顺带补齐它。

**⑦ gVisor 不改变 conntrack 与 original-dst 的任何结论**(D5 ⑨):ct-state 放行在 gVisor 流上承重(去掉
它,被放行的流也断),端口-only 重定向对 gVisor 发起的连接同样取回真实目标。⟹ **门 A 的纯函数结论可以
直接信,不必为 gVisor 另立一套。** 一条限定:D5 ⑩⑫ 与本节 ⑧⑨ 是在**普通 netns peer** 上量的,故
**⑨ 那个跨链陷阱与 ④③ 的 `forward` default-deny 未在 runsc 下复跑**——它们由门 B 覆盖。

**⑧ IPv6 由同一条 `inet` 地板覆盖,不需要单独的 `ip6` 臂(已实测,D5 ⑪)。** 一条**不带 family 限定**的
纯 ct-state 规则同样匹配并 drop IPv6,`established,related` 一条共用臂即可承载 v6 回程;显式
`meta nfproto ipv6` 第二臂**冗余**。⟹ **生成器不为 IPv6 分叉。** 但**门 B 仍必须有 IPv6 断言**(第 10b 条):
今天引擎从不碰 IPv6 而内核照样给每条 veth 自动配宿主侧 `fe80::/64`,实测无地板时 guest **不仅**可达那个
链路本地、**还可达 `eth0` 的全局 IPv6**——即这一族今天确实敞开,只是**不需要新机制**去关。
**「关掉 veth 的 IPv6」是可选加固,不是机制**:guest root 能改回自己那半(宿主侧那半它碰不到)。

**⑨ 那条 accept 必须与地板 drop 同链——跨链会让计数器读成健康而功能是死的(实测,D5 ⑩)。** 放进同钩子、
更低 priority 的另一条基链时,它**匹配了、accept 计数涨了,而流量仍被地板 drop、连接仍失败**:基链的
`accept` 只结束**那条链**,包继续进下一条基链,**只有 `drop` 跨链终止**。⟹ **门 B 的 working 那一侧不能
只断言「accept 计数非 0」,必须同时断言可达**——与 ② 同为静默假过,但方向相反:这次是功能死了而信号看着是活的。

---

## 5. 改哪些代码

| 位置 | 改什么 |
| --- | --- |
| `internal/network/`(新) | 子系统:声明类型、L0 地板、纯函数生成器、八类 400 的校验。`arch` 守卫已预登记,不动守卫 |
| leaf 包(新,或并入既有 leaf) | sibling CIDR 常量。**今天硬编码在 `local_runsc.allocSubnet` 里**,而生成器要用同一份 ⟹ 按 `guestport` 先例放 leaf,**不开子系统边** |
| `internal/model/model.go` | `Sandbox` 加声明字段(今天刻意省略,注释明写"omitted rather than stubbed"——连同注释一起改) |
| `internal/store/` | `CREATE TABLE sandboxes` 加列 + `Binding` 加字段 + `Apply`;两个后端同步 |
| `internal/runtime/provider.go` | `SteeringSpec` 从 `struct{}` 变成真载荷(注释里的 "Filled in by M8" 兑现) |
| `internal/runtime/local_runsc.go` | 渲染 + 安装(次序:`buildNetns` 之后、`runsc run` 之前)· 出网三件机制 · `teardownPlacement` 加拆表 · `Adopt`/`ReclaimOrphans` 认表 |
| `internal/lifecycle/lifecycle.go` | `coldStart` 从记录读声明 → 生成 → 填 `SteeringSpec`(与 `limits` 同形,Binding 每次 placement 重新派生) |
| `internal/orchestrator/api.go` | 两个端点(今天落 `default` 分支 → 404)· `patchReq` 加 `network` · create 的 `network?` |
| `internal/orchestrator/api.go`(§6.2) | 八类 400 进 `statusCodeFor` |
| **`DisallowUnknownFields`** | **全仓无一处**,故 create 的 `network?` 今天被**静默丢弃、调用方拿 201 以为生效**。至少在声明相关写点开启 |

---

## 6. 怎么验收

> **纪律(D4.1 / D5 ⑧):"能通"是不足的断言。** 一条写错顺序的规则集在正常流量上完全正常。故每条 deny
> 断言都必须是 **netns 内不可达** ∧ **刻意探测后地板计数器非 0**。**计数器为 0 的地板 = 被绕过的地板,
> 两者不可区分。**

**门 A — 纯函数(免 root,`go test ./...`)**
1. 声明取值组合 → 规则集:`deny`+空 / `deny`+`allow[]` / `deny`+`via[]` / `allow`,逐一钉住。
2. 八类 400 逐条。**第七类两个方向都要有断言**:整段包含 → 400;**部分重叠的超网(`10.0.0.0/8`)→ 必须 200**(D5 ㉑);**同一 CIDR 出现在 `via[].match[]` 里 → 必须 200**(两字段判定不对称,D5 ㉑);**落在 sibling CIDR 内的 `allow[]`(含本沙箱网关那个 `/32`)→ 必须 200**(它是 §5.D 语义 ③ 的声明路径,不是死规则,D4.2)。**第八类**:`::ffff:<v4>` 字面量 → 400,且断言它在包含判定**之前**被拒(D5 ⑲)。
3. **规则次序是断言,不是风格**:`via[]` 胜 `allow[]`;多条 `via[]` first-match-wins;地板在声明之上。
    **`via[]` 那条窄放行另钉三样**(D5 ⑩,少一样都有实测反例):pre-NAT 元组 ∧ post-NAT 端口 ∧ `iifname`;
    并钉**它与地板 drop 同链且在其上**(跨链的 accept 会让计数器读成健康而功能是死的,本文 §4 ⑨)。
    **透明截获形态恰好少 `ct original ip daddr` 一个 clause**——断言"当且仅当声明钉了 cidr 时才发这个
    clause",两个方向都要有一例。
4. **三条渲染守卫**:`policy drop` 于每沙箱基链 → 拒;`iif` → 拒;**未带 `iifname` 的 accept → 拒**
    (D5 ⑩:每沙箱表都是共用钩子上的 `policy accept` 基链,故一张表里未收窄的 accept 是**所有**沙箱地板上的
    洞)。
5. L0 地板四项都在,且 **`input` 侧**在 sibling drop 之前有"本沙箱网关"放行(该网关是宿主自身地址,故这
    一对规则落 `input`、不落 `forward`,D4.2)。**声明的放行两个钩子都发**,而 `input` 侧那份必须被一条
    sibling drop 兜住:**一条 `allow 10.0.0.0/8` 之下,`input` 侧必须仍有一条盖住 sibling CIDR 的 drop,且
    排在该放行与"本沙箱网关"放行之间**——次序是断言,不是风格(线上那一面归门 B 第 11c 条)。
    **另断言生成器不为 IPv6 分叉**:不发任何
    `meta nfproto ipv6` / `ip6` 臂(D5 ⑪ 已量到那是冗余的),`inet` + 无 family 限定的 ct-state 一条即可。

**门 B — 真 substrate(`-tags substrate -p 1`,root)**
6. `default:deny` + 空:guest 到公网 / sibling / link-local / 宿主任一地址**全不可达**,且**地板计数非 0**。
    **计数要按钩子分别读**:到宿主自身地址的那几条命中 `input` 侧,而到"宿主会路由出去的东西"命中
    `forward` 侧——**一条目标不在宿主上又未被引流的流量根本不经 `input`**,故若只读 `input` 计数器,那类
    流量会读成"地板计数为 0",按本门的纪律是**无效断言**而不是一次通过(D5 ⑩)。
7. `default:allow`:公网**可达**(证第 8 件真的物化了,否则第 6 条在一个恒不通的引擎上假过)。
8. `egress.allow[]` 单条:**恰好**那个 `{cidr,port,proto}` 通,同地址的**别的端口不通**。
    **UDP 那条单独断言**(`proto: udp` 放行后 UDP 真的通),因为它是 DNS 唯一的可声明路径;但**只断言到
    规则层面**——guest 里 `curl` 能否解析取决于 `resolv.conf`,那不在 M8(本文 §8)。
8b. **面向 guest 的拒绝是 `reject` 而非 `drop`**:被拒的连接**快速失败**,不是挂到超时。断言按**量级**写
    (亚秒 vs 数十秒),**不要把具体毫秒数写进断言**——那组数字单来源且复现未成(D5 ⑨ 的 ⚠)。这条断言的是
    §2.4 fail-closed 的**后半句**("guest 侧得到一个干净的连接失败"),它与前半句是**两个可分别失败的
    性质**。**这条同时是那组数字的复现场**:M8 跑它的时候顺带把 D5 ⑨ 的效力补齐。
9. **transport 不被地板杀**:装上地板后 host→guest 的 `Health` 与一次多 KB 往返照常(D5 ⑧ 的响亮反面)。
9b. **`via[]` 真的通,且那条窄放行没有重新打开地板** —— 两面都断言,缺一面都不算过。**这条是本里程碑
    最容易静默假过的断言**,而它有**两个方向相反的假过**(D5 ⑩):只测 (a) 则一条宽到废掉整个地板的放行
    完美通过;只看计数器则一条**跨链**的 accept 计数在涨而功能是死的。
    (a) **working 那一侧要同时断言"可达" ∧ "accept 计数非 0"**:命中的流量**到达那个宿主本地 endpoint**,
    且该进程读回的是**真实原始目标**(D5 ⑥ 的端到端兑现)。
    (b) **同一份规则集下**,以下每一条都**仍被拦且地板计数增长**:guest 到宿主**其他**地址 / 其他端口;
    **直连那个 endpoint 端口**(绕过引流);**一条未声明的原始端口重定向到已声明的 listener**;
    **一条已声明的原始端口重定向到未声明的 listener**。后两条各自对应 D5 ⑩ 表里一个更简写法的实测反例,
    **它们是那三个 clause 承重的证明**,不是补充。
    (c) **`iifname` 那一维单独断言**:第二个沙箱的同样流量**不被本沙箱那条 accept 放行**(D5 ⑩ 末行——
    未加 `iifname` 的 accept 是所有沙箱地板上的洞)。
10. **支柱④**:guest root `ip addr add` 伪造宿主/sibling 地址后重试 → 仍被拦,**且地板计数增长**
    (证是 `iifname` 拦的,不是 `rp_filter`)。
10b. **IPv6 也被同一条地板覆盖**:guest 到**宿主侧 veth 的 `fe80::/64`**(内核自动配的,不是我们配的)
    以及到宿主 `eth0` 的**全局** IPv6 地址**均不可达且地板计数增长**。**这条不能省**,即使 D5 ⑪ 已经量到
    `inet` 地板覆盖 v6:那是在 netns peer 上量的规则语义,而这里要证的是**引擎渲染出来的那份规则**在 runsc
    下确实覆盖了它——今天引擎不碰 IPv6 而内核照样配地址,故一份只覆盖 IPv4 的地板会让全部 IPv4 断言通过、
    而整族敞开(本文 §4 ⑧)。**断言 `eth0` 那条要取全局地址,不要取它的链路本地**——后者从沙箱本就不可达
    (不同链路),拿它断言等于断言一个恒真的事(D5 ⑪)。
11. **跨沙箱**:两个沙箱各一张表,一个装 default-deny **不影响**另一个(②的正面);删/重建其中一张表,
    另一个的可达集不变。
11b. **veth 同名重建后地板仍然拦得住**(③ 的正面,也是 `iifname` 那条守卫的行为面):一次 pause→resume
    之后 guest 的同一条探测**仍不可达且地板计数增长**。**这条要断言在"重建之后"这个时点上**——`iif` 的
    fail-open 恰好只在此时显形,而它的症状是**计数器为 0 而流量被投递**(D5 ⑫)。
11c. **`input` 侧的声明放行没有把 sibling 的网关一起放开**:两个沙箱,A 声明 `allow 10.0.0.0/8`(整段含两个
    `/30`)→ A 到**自己**网关上那个宿主本地端点**可达**(§5.D 语义 ③ 的兑现面),而 A 到 **sibling B 的网关
    不可达**,**且增长的是 `input` 侧那条 sibling drop 的计数器**。**两面都要**:只测可达则一条漏放的规则集
    完美通过;而"连不上"本身不是有效断言——`forward` 侧那条 sibling drop 对这条路径恒计 0(实测),只读它会
    把一次真正的漏放读成"地板计数为 0"、按本门纪律是无效断言(D4.2)。
12. **原子性**:规则在隔离单元起跑前已就位(无 fail-open 窗口);launch 半途失败**不留表**。
13. **对账**:杀掉控制面 → 重启 → 活沙箱的表被认回、孤儿表被拆;**表不泄漏**(照
    `CountEngineNetns` 的形状加一个按表名前缀计数的断言)。
13b. **对账不断活流**:一条**正在传输**的 guest 出站流跨越"杀控制面 → 重启 → `Adopt` 重认"全程
    **继续传字节**——**按字节增长断言,不是按 socket 还在**(往一条对端已消失的 socket 上 `write()` 通常会
    成功一次才拿到 `EPIPE`,故"写成功"证不了投递;要有读回校验,D5 ⑰ 的量法)。**这一问不能省**:一个把每条
    活连接都悄悄掐断的 `Adopt` 会让第 13 条(表被认回、孤儿被拆、表不泄漏)**全部通过**——断流之后表计数恰好
    还是对的,故第 13 条对这个故障**完全不敏感**(D5 ㉒)。**顺带覆盖 D5 ㉒ 那条"未测"**:那里量的是 `Adopt`
    可能做的三种宿主态改动,而"真的杀掉并重启一个 orchestrator 进程"要等本里程碑才存在。
14. **publish**:`running` 沙箱热加 → 宿主 `ip:port` 可达 → 跨 resume 存活。**拆成四问,四条都实测过
    各自的反面**(D5 ⑬–⑯):
    (a) **从一个 `/30` 之外的调用方**可达(而不只是从宿主拨测)——**且断言那条 SNAT 在**:没有它,
    publish 在"guest 恰好有默认路由"的沙箱上能用、在没有的沙箱上超时(D5 ⑬)。
    (b) **宿主自身**也可达 —— 这是**另一条**规则(`output` 钩子),`prerouting` 那条对宿主发起的流量
    根本不求值。⟹ **两条路径分别断言,不许用一条代替另一条**(D5 ⑭)。
    (c) 伴随 accept 在 **`forward`** 上生效(读 per-hook 计数器证实,`input` 侧应恒 0)——入站与
    `via[]` 的钩子归属**相反**,别照搬 D5 ⑧⑩(D5 ⑮)。
    (d) 绑 `127.0.0.1` 的 guest 服务**连接被拒且快速失败**(不是挂到超时)。**只断言到这里**:
    它与"没有监听者"在调用方侧不可区分(都是 RST),故**不要断言可归因**——那半承诺已因此从 §5.D 移除
    (D5 ⑯)。
    (e) **"publish 不通"的两个签名要分别断言,不许合并成"不可达"**(D5 ⑰):少了 DNAT → 新连接
    **`refused`**(快);少了伴随 SNAT → **连上了但读超时**(`connected_no_payload`),后者极易被误读成
    guest 侧应用故障。一份半拆的规则集正是以后者的形态坏掉。
14c. **`paused` 上的 publish 只落库**:对 `paused` 沙箱 `POST …/publish` → 200 且
    `applies_at: "next-resume"`、**无 `host_endpoint` 键**;此刻宿主上**没有**对应规则(按表名前缀断言该表
    不存在);随后 resume → 该端口可达。**同一条 publish 在 `GET …/network` 上的渲染与写面响应一致**
    (`paused` 时无 `host_endpoint`,`running` 后有)——写面与读面在同一字段上分叉即契约缺陷。
    另断言这次写**不动 `UpdatedAt`**(§3.4 纪律 ②:`retention` 从那一列起算)。
14d. **重复 publish 幂等**:同一 `{guest_port, proto}` 连发两次 → 都是 200,声明里**只有一项**,宿主上
    **只有一条** DNAT 规则。**反面已量**:nft 收下两条逐字相同的规则不报错、第二条计数器恒 0,故基底不替
    引擎发现重复,去重必须在声明层(§5.D)。
14e. **撤一条 publish 只关新连接,关不掉在途流**(D5 ⑰):`running` 上删掉那两条 DNAT 后,新连接被拒
    而**已建立的流仍能读回数据**。这条不是要求引擎去修它,而是**钉住那个语义**——它正是"今天没有 un-publish
    专用端点"的机制理由(本文 §8),故断言它、以免将来有人以为撤销是干净的。
14b. **publish 的反向流没有变成一条出站路径**(D1 ④):同一份规则集下 guest 主动出站**仍被地板拦且地板
    计数增长**。**这条在本里程碑才第一次能有效断言**——spike 里那次因 guest 没有默认路由、出站在离开沙箱
    前就 `net unreachable`,地板计数 0,按门纪律是无效断言(D5 ⑯ 末的说明)。此处第 8 件已装好默认路由,
    故同一条探测会真的打到地板上。
15. **`PATCH` 的 read-modify-write 危险**:用旧读回值回写会静默丢掉期间热加的 publish —— 断言这条
    (§5.D 明写它是安全相关字段上的真实危险)。

---

## 7. 内部怎么分门

**门 A(纯函数)先行,整门可在免 root 下完成。** 它覆盖声明取值组合、八类 400、次序、三条守卫——即
**M8 全部"设计正确性"**。门 B 只证"装上了、次序对、崩溃后对得上账"。

**第 8 件(出网三件机制)必须在门 B 第 6 条之前落地**,否则第 6 条在一个恒不通的引擎上假过。这是
**分门次序上的硬约束**,不是偏好。

---

## 8. 明确不做(out of scope)

| 不做 | 为什么 |
| --- | --- |
| 域名 / SNI / TLS 终结 / 内容改写 | **不在引擎内**(D2 代价判据 / §5.D);承载它们的 seam 也不存在 |
| 凭证 / 鉴权 / 多租户 | §1.4 永久排除;`via[].endpoint` 后面是什么引擎不知道也不问 |
| DNS 代理 / 解析器概念 | 同 D2 判据。**`default:deny` 下 DNS 由调用方声明 `proto: udp`**,引擎只把这条 sharp edge 写进契约(§5.D 第 3 条用户可见语义) |
| **给 guest 写 `resolv.conf`** | **实测 guest 今天完全没有解析器配置**(`build-rootfs.sh` 不写,全仓无一处挂载),故 DNS 在 guest 里今天**根本不工作**,早于任何策略问题。但那是**模板/产品层的内容选型**(同 open-decisions「§5.A / 模板」),不是引擎语义。**M8 不做,但它是"声明 DNS"的前置条件**——故 M8 的 DNS 验收(门 B 第 8 条)只断言**规则层面放行生效**,不断言 guest 里 `curl` 能解析 |
| UDP 引流(`via[]` 收 UDP) | 机制上取不到原始目标(D3.2 / D5 ⑥);入站 `publish` 的 `proto: udp` **照收** |
| `SteeringSpec` 的远端形态入参 | 归 open-decisions「§5.A provider 接口(远端形态入参)」,不实现任何远端 provider |
| 内网 deny-safe 档的成员清单 | 仍在 open-decisions(「§5.D 内网 deny-safe 档的成员判据」一行,未收口),M8 不猜。**语法不是问题**:内网目标就是一个 IP 字面量 CIDR,`egress.allow[]` 今天就收得下 |
| fail-closed 是否发 §4.1 审计事件 | 同上(倾向发,未定) |
| un-publish 专用端点 | **推迟的理由是机制,不是工期**:实测删掉一条 publish 的两条 DNAT 后新连接被拒、**但已建立的流仍在传数据**——conntrack 持着那份 NAT 绑定,规则删除对它不求值(D5 ⑰)。故 `running` 上的 un-publish 只能承诺"不再收新连接"、不能承诺"端口已关",而两者的差别正是撤一个入站口时想要的东西。要落地先得定"撤是否断在途连接",那是一次独立的语义决定 + 一次测量。走整份 `PATCH`(`next-resume` 生效)**没有**这个歧义:placement 整个重建,旧流随之消失。**它同时是 `PATCH` 必须保留 `publish[]` 所有权的原因**——没有它,M8 就没有任何撤销路径 |
| 按源 CIDR 收窄 publish | §5.D 明写是将来的纯加法 |
| 沙箱 disk 帽降到当前用量以下的写点 | **不是网络**。仍在 open-decisions §4 且**无 owning plan**(需要一次 §5.B seam 改造) |

---

## 附:spec 锚点

| 本文的件 | spec |
| --- | --- |
| 三条 invariant / provider 安全义务 / 原子性 | §2.4 |
| 子系统边界、四字段声明、四条用户可见语义、publish 热加 | §5.D |
| provider 契约 3-tuple、`SteeringSpec`、对账机制 | §5.A |
| 声明是 Binding、`PATCH` 是写点 | §3.4 |
| 两个端点、wire schema、`applies_at` | §6.1 |
| 八类 400、次序两条、校验早于准入门 | §6.2 |
| 跨沙箱网络隔离是独立 invariant | §2.3 |
| 推导 / 证伪 / 逐条实测 | [`network.md`](../spec/deep-dives/network.md) D0–D8 |
