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
| **§3.4 第二项在配额读失败时该往哪边错** | **不是"要不要 fallback",是 fallback 的方向没被论证过**。`LocalBtrfs.UncappedBytes` 在某个单元的配额行读不出来时落到 `allocbytes.Tree`,注释论据是"漏计真实字节会让节点超发,故测量是保守方向"——但那句话把误差方向当成了单向的,实测两向都有:走行走**只数常规文件**,而配额行**另计 metadata extent**(空单元实测 16384 B vs 走行走 0),故它对稀疏/小文件多的单元**低计**(超发方向);同时它把与源**物理共享**的 extent 全额计入,而配额行只计一次,故它对快照/克隆密集的单元**高计**(误拒方向)。**哪边占优取决于该节点的构成,不取决于代码**。触发条件不是罕见:`qgroup show` 为**每一行**解析路径后才过滤,故该文件系统上**任何**位置的并发删除都会让读整体失败(D7.1 已记 ~1/150;三路 churn 下实测 157/2142 ≈ 7.3%,报错甚至指名别人的行 `cannot find the qgroup 0/21680`),而 substrate 的 cleaner 是异步的,调用方无法与之串行化。**per-volume 的那一半已收口**(`Backend.Usage` 现在把不可读的行返成 error 而非 `(walk, 0, nil)`,D7.3 的严格不等式与 `RepairVolumeCaps` 因此才真的 fail-closed);本行只问**聚合项**该怎么办——`UncappedBytes` 没有 per-unit 的错误出口,一个单元读失败要么污染整个节点的数字、要么让整次准入读失败 | 倾向:先把两向误差量到"够不够大到会改变准入判决"再定形状,不预留机制;谁消费:§3.4 准入门——取值须由"这个节点上卷的实际构成"论证 |
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

---

> **纪律:** 任何一项拍板后,先更新对应深挖文档或 plan 的相应章节,**再从本登记移除**——保证
> "已定"进 plan、"未定"留本文,两边不重叠。本文 §4 的缺口同理:owning plan 出现即迁入、从此移除。
>
> **引用本表的行按其加粗标题,不按位置**(理由与 network.md D5 顶部拒绝按位置编号的那条逐字相同)。
