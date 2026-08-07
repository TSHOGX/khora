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
> **原先登记在此的"控制面重启后活着的流是否因 ct 状态丢失而断"已实测收口**,结论与对 `Adopt` 的三条硬
> 约束见 [`deep-dives/network.md`](./deep-dives/network.md) D5 ㉒。

---

## 2. 待细化的设计点(架构已定,内部机制未展开)

| 归属(plan §) | 待细化内容 | 备注 / 当前倾向 |
| --- | --- | --- |
| **§5.C Volume(契约边界外)** | 契约在 §5.C;待细化的边界项:独占卷抢占策略 / 直连变更 vs live-ro reader 准入 / snapshot **一致性标注**进 §6 / 远端 backend 落地形态 | **详见 [`deep-dives/volume.md`](./deep-dives/volume.md) D8**,本表不复制。多为产品层策略或落 §6 时定。抢占那条另有一条**锁序**论据(同文 D3:卷维度反查持有者会闭合成环,实测死锁) |
| **§4.4-C 事件流可重放窗口大小** | 事件流的对外形状已定(pull + `seq` 游标,§4.4-C / §6.1)。**仍开放的只有一项:可重放窗口取多大**,即何时开始裁剪 outbox。**详见 §4.4-C 末段**(为何是被推迟而非被回答、连带的 at-rest KNOWN LIMITATION),本表不复制 | 谁消费:未来外部 subscriber——取值须由"承诺给它的重连时长"论证,而今天没有这样的订阅者 |
| **§5.D 声明的两处行为细节** | 边界、四字段声明、三条 invariant、L0 地板的成员清单与规则文本、**声明放行的钩子归属**都已定(§5.D / §2.4;推理见 [`deep-dives/network.md`](./deep-dives/network.md) D1–D4,实测见同文 D5)。**仍开放两项**:① **内网 deny-safe 档的成员判据与其"减去地板"的表达式**(见下面单列的那一行)② fail-closed 是否发 §4.1 审计事件(倾向发;**容量压力事件不在此列——软阈已定为不发**,见 §4.4-C)。**"内网放行的声明语法"不在此列**:实测它不是语法问题——内网目标就是一个 IP 字面量 CIDR,`egress.allow[]` 今天就收得下,待定的是**成员判据与次序**(① 那一行)。**`SteeringSpec` 的字段形状也不在本行**,它归下面「§5.A provider 接口(远端形态入参)」 | 谁消费:M8(② 不阻塞开工;① 见那一行) |
| **§5.D 内网 deny-safe 档的成员判据** | §2.4 定了这一档的行为,**开放的只有成员清单**。**三个候选**:RFC1918-only(最窄,漏掉落在 `0.0.0.0/8` 的本机解析器,D5 ⑦)· IANA 特殊用途登记表 **Globally-Reachable=False** 列的**钉死快照**(出处可审计,显著更宽)· 中间的手工集。**必须与它一起定的两件**:① 派生集里**五项 GR=True**(`192.0.0.9/32` · `192.0.0.10/32` · `192.31.196.0/24` · `192.52.193.0/24` · `192.175.48.0/24`;前两项落在成员 `192.0.0.0/24` 内)要一条排在档位 drop **之上**的放行,否则整表 drop 拦掉可全球到达的 anycast;② 清单**必须写成"档位减地板的 daddr 清单项"**(loopback / link-local / sibling CIDR;地板第四项"宿主自身的任一地址"是纯 ct-state 规则,**减不进也不必减**,§2.4),否则调用方的放行**看着抬动了地板再在线上被拒**;这个"减"只能落成两条按次序排的规则(D5 ⑱)。**v6 侧随所选 v4 集合机械派生**(`fc00::/7` + 各成员的 NAT64 映像,RFC 6052 **第 2.2 节**),各条边界条件**已定、不在本行**(network.md D5)。 | 谁消费:M8——`egress.default: allow` 能否诚实交付的前提(D5 ⑤);不阻塞开工 |
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
