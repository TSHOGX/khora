# 开放决策登记(Open Decisions)

> spec 只描述已收敛的架构;未拍板的选型、待细化的设计点、落地前要验证的假设登记在本文。
>
> **编号约定**:裸 `§X.Y` 一律指 spec 全局章节,按 SPEC.md 顶部「文档地图」定位;`Dx` 指相应深挖文档的
> 节;指本文自己的节时写「本文 §N」。
>
> **本文 §1** 阻塞子系统的 spike · **本文 §2** 架构已定、内部机制未展开 · **本文 §3** 有候选未定论 ·
> **本文 §4** 实现缺口(设计已定、代码未兑现;有 owning plan 后迁入该 plan)。每条标注归属 + 当前倾向 +
> 谁消费;已在深挖文档详述的只给一行摘要 + 指针,**不复制正文**。

---

## 1. 落地前验证(spike,阻塞相关子系统)

> 无。**没有任何测量阻塞当前里程碑开工。**
>
> 一条"已定的结论缺一次复现"不属于本节(它不是未定的设计),由 owning plan 的验收覆盖:`drop` vs
> `reject` 的 guest 体验量级差(单来源、数字待复现)归 [`M8-network.md`](../plans/M8-network.md) 门 B。
> 同理"控制面重启后活着的流是否因 ct 状态丢失而断"已实测收口:结论与它对 `Adopt` 施加的三条硬约束见
> [`deep-dives/network.md`](./deep-dives/network.md) D5 ㉒。

---

## 2. 待细化的设计点(架构已定,内部机制未展开)

| 归属(plan §) | 待细化内容 | 备注 / 当前倾向 |
| --- | --- | --- |
| **§5.C Volume(契约边界外)** | 契约在 §5.C;待细化的边界项:独占卷抢占策略 / 直连变更 vs live-ro reader 准入 / snapshot **一致性标注**进 §6 / 远端 backend 落地形态 | **详见 [`deep-dives/volume.md`](./deep-dives/volume.md) D8**,本表不复制。多为产品层策略或落 §6 时定。抢占那条另有一条**锁序**论据(同文 D3:卷维度反查持有者会闭合成环,实测死锁) |
| **§4.4-C 事件流可重放窗口大小** | 事件流的对外形状已定(pull + `seq` 游标,§4.4-C / §6.1)。**仍开放的只有一项:可重放窗口取多大**,即何时开始裁剪 outbox。**详见 §4.4-C 末段**(为何是被推迟而非被回答、连带的 at-rest KNOWN LIMITATION),本表不复制 | 谁消费:未来外部 subscriber——取值须由"承诺给它的重连时长"论证,而今天没有这样的订阅者 |
| **§5.D 声明的一处行为细节** | 边界、四字段声明、三条 invariant、L0 地板的成员清单与规则文本、**声明放行的钩子归属**、**内网 deny-safe 档的成员**(RFC1918,§2.4)、**第二跳的钩子与谓词**(`output` + `skuid`,D5 ㉔)都已定。**本行仍开放的只有一项**:fail-closed 是否发 §4.1 审计事件(倾向发;**容量压力事件不在此列——软阈已定为不发**,见 §4.4-C)。**"内网放行的声明语法"不在此列**:内网目标就是一个 IP 字面量 CIDR,`egress.allow[]` 今天就收得下。**`SteeringSpec` 的字段形状也不在本行**,它归下面「§5.A provider 接口(远端形态入参)」 | 谁消费:M8(不阻塞开工) |
| **§5.D 内网 deny-safe 档是否放宽到 RFC1918 之外** | **成员清单已收口进 §2.4(RFC1918 三段 + v6 机械派生),本行不再问"取哪个集合",只问将来是否放宽**——落在其外的私有用途段(`0.0.0.0/8` 里的本机解析器、CGNAT `100.64/10`、`192.0.0.0/24` 等)今天按公网处置,即 `default:deny` 下要显式声明、`default:allow` 下可达。**放宽的代价已量,故这不是一次调研**:更宽的候选是 IANA 特殊用途登记表 Globally-Reachable=False 列的钉死快照,选它就要带上五项 GR=True 的反向放行(`192.0.0.9/32` · `192.0.0.10/32` · `192.31.196.0/24` · `192.52.193.0/24` · `192.175.48.0/24`;前两项落在成员 `192.0.0.0/24` 内),否则整表 drop 拦掉可全球到达的 anycast;并且它含嵌套成员,撞上 D5 ⑱(interval set 拒嵌套 / `auto-merge` 静默并子网)。**形状上是纯加法**(给一份静态清单加行,D7 litmus),故不为它预留任何机制 | 谁消费:将来的部署方——要由"该部署的内网里有什么"论证,而不是由清单的完备性论证 |
| **§5.A provider 接口(远端形态入参)** | **待细化的是远端形态的入参形状**:`RootfsSource`(本地 `MergedPath` vs 远端句柄)、`SteeringSpec` 的**精确字段**(§5.D 派生出的可达性要求;签名里不得出现宿主原语,§5.A ❌ 清单)、`ResourceLimits` 在非 cgroup 调度层的等价物——用"被调度的远端隔离单元"作纸面参照反推,**不实现任何远端 provider** | 谁消费:未来远端 provider |
| **§5.B Overlay 物化** | 待细化:content-addressed diff 层、multi-node 经 rootfs provider 跨 node 寻址(**应用层协作安全点不在此列——已否决**,见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.4) | **详见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D5**,本表不复制 |
| **§5.C per-volume 容量帽在 remote 后端的物化** | local 臂是 subvolume + squota(§5.B/§5.C 共享原语)。**待细化的是 remote 后端**:一个对象存储 / 网络 FS backend 用什么手段兑现"这个卷最多多大"(后端配额 API / 客户端侧核算),以及它与 `Capabilities()` 的关系 | 封在该 backend 内(volume.md D1/D7),不进核心;谁消费:未来 remote volume backend |
| **§5.A / 模板(内容目录)** | **内置 Template 的内容清单**:一个 bare base(uv/pip/git/build-essential;可选 sudo,仅作 guest 内便利、非引擎语义),各 agent 栈(claude-code/opencode/pi)叠在 bare base 之上。**这是产品/模板层的内容选型**,仍开放(Template 的一等行化 / boot seed / multi-lowerdir 叠层机制是 §4.3 / §5.B 的既定架构,不在本行) | guest 默认 root(§2.2),装包是一次性 exec、不引入 init 依赖,见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1 |

---

## 3. 待拍板的选型(有候选,未定论)

> 无。

---

## 4. 实现缺口(设计已定,代码未落地 / 待收口)

| 缺口 | 现状 | owning plan |
| --- | --- | --- |
| **§6.2 的第九类拒绝:`via[]` 声明遇上未给第二跳 uid 的部署** | **§6.2 那张清单写成"八类",而 M8 的落地需要第九条拒绝,它不是声明文本的性质、是部署的性质。** 缺口在两处措辞:① 清单的基数是"八类";② §6.2 另有一句"资源上限字段按轴条件必填**是 §6.1 唯一取决于部署配置的字段语义**",而这一条同样取决于部署配置。**为什么必须拒**:第二跳靠一张按部署期 uid 收窄的 `output` 表压住(§2.4 / D5 ㉔),没有 uid 就没有表、也就**没有 §2.4 第二条 invariant 的强制** —— 收下这份声明等于落库一份引擎兑现不了自己 invariant 的声明,宿主上留着一个未被压住的 confused deputy,而**第一跳的每条断言对它完全不敏感**(实测:第二跳在 `forward` 上计 0,第一跳那份地板对它一个包都不评估)。M8 plan §5 已经把这条行为定死("给了才允许 `via[]`"),故落地无歧义;开放的只是 spec 侧怎么写。**候选**:(a) 把清单改成"八类声明 400 + 一类部署 400",并列出后者 —— **倾向这条**,它同时修掉②那句的绝对化(现在有两条取决于部署配置的语义了),且保持"调用方要分辨的语义种类"这个既有判据(remedy 不同:这一条的 remedy 是运维的,不是调用方的);(b) 把它算进第五类(`via[].endpoint` 非宿主本地)—— 拒,两者判据不同:第五类判的是 endpoint 地址,这一条即使 endpoint 完全合法也要拒;(c) 不拒、装一份没有第二跳强制的规则集 —— 拒,它正是 §2.4 点名要拒的 confused deputy。**代码已按 (a) 的语义落地**(`network.ClassViaWithoutSecondHopUID`,自带 sentinel、映射 400),注释里指回本行 —— 取倾向而不是让 spec 被代码静默改写 | **M8**(评审门上拍板,不阻塞落地) |
| **沙箱 disk 帽降到当前用量以下没有写点**(§3.4 严格不等式的 §5.B 一侧) | **设计已定([model.md](./model.md) §3.4 的"降帽不是安全方向"块,卡死后的实测在那里,本表不复制),代码只落了卷侧那一半**:`lifecycle.SetVolumeCapacity` 用严格不等式把卷帽的 below-usage 降帽拒成 400(`ErrVolumeCapBelowUsage`),而 `lifecycle.UpdateBinding` 在 disk 轴上**只校验 `0 → N`**(`ErrUncappedDiskNotCappable`),`N → M` 无校验通过。故一次 `M ≤ 当前用量` 的 `PATCH` 返 200 并**卡死该存储单元**,与卷侧逐字节同形。**不是缺一条 `if`,是一次 seam 改造**:`rootfs.Provider` 只暴露 `PromoteCostBytes` 与 `UncappedBytes`,而 `UncappedBytes` 对**有帽单元刻意计 0**(那是它的契约),中介层因此读不到严格不等式所需的 per-sandbox 用量。收口需要 §5.B 新增一个沙箱级用量查询,形状照 `volume.Backend.Usage(ctx, id) (used, limit, err)` | **无**——M8 是网络,不含此项;收口里程碑待定 |

> **§5.D 可达性强制(支柱③④)不在本表**:它是最大的未建面、又是安全关键面,但它已有 owning plan,
> 故按本文末尾那条纪律迁入 [`M8-network.md`](../plans/M8-network.md) 该文 §1(现状节)——现状陈述以那里为唯一出处,
> spec 侧(§2.1 ③④ / §2.4 / §5.D / §6.1)只留指针。M8 必须继承的那些实测在
> [`deep-dives/network.md`](./deep-dives/network.md) D5。

---

> **纪律:** 任何一项拍板后,先更新对应深挖文档或 plan 的相应章节,**再从本登记移除**——保证
> "已定"进 plan、"未定"留本文,两边不重叠。本文 §4 的缺口同理:owning plan 出现即迁入、从此移除。
>
> **引用本表的行按其加粗标题,不按位置**(理由与 network.md D5 顶部拒绝按位置编号的那条逐字相同)。
