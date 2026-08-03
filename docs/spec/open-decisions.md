# 开放决策登记(Open Decisions)

> **这份文档是什么。** [`SPEC.md`](./SPEC.md)(及其分篇 model/subsystems/api-contract)只描述
> **已收敛的当前架构**;凡是**尚未拍板的选型、待细化的设计点、落地前要验证的假设**,一律从 spec 里
> 抽出登记在本文,避免把"未定"与"已定"混在同一篇里。下文出现的 `§X.Y` 是全局 §编号,按 SPEC.md
> 顶部「文档地图」定位到具体文件。
>
> **怎么组织。** 前三类是**未定**项:**§1 落地前验证**(阻塞子系统的 spike)、**§2 待细化设计点**
> (架构已定、内部机制未展开)、**§3 待拍板选型**(有候选、未定论)。**§4 是实现缺口登记**——设计
> **已定**、代码**尚未落地或待收口**的项(不是"未定决策",而是"已定未建",按 CLAUDE.md 本属 plan
> territory,但当前无 owning plan 时集中于此备查)。每条标注**归属 + 当前倾向/fallback + 谁消费**。
> 已在深挖文档详述的条目,此处只给一行摘要 + 指针,**不复制正文**(保持无冗余)。

---

## 1. 落地前验证(spike,阻塞相关子系统)

> **当前无阻塞 spike。** 两个 substrate 载体在 spec 内:持久层载体(外层内核 overlayfs +
> `--overlay2=none`,§2.5 / §5.B)、transport 载体(netns 内 TCP,§5.E)。两条载体上仍待设计的机制
> (非阻塞)分别见 §2 的 §5.B / §5.E 行。

---

## 2. 待细化的设计点(架构已定,内部机制未展开)

| 归属(plan §) | 待细化内容 | 备注 / 当前倾向 |
| --- | --- | --- |
| **§2 安全模型** | 五支柱各自的 enforcement 细节;§2.3 数据面在 shared-rw Volume 等场景下的复合影响 | 支柱契约已定,逐条 enforcement 后续展开 |
| **§5.C Volume(契约边界外)** | Volume 契约与设计已定(§5.C:独立 `volume.Backend` seam、local=btrfs subvolume / remote=host-FUSE 两臂、AccessMode、独占租约=挂载行、写入 regime、btrfs 快照);待细化的边界项(独占卷抢占策略 / shared-rw ENOSPC 复合行为 / 直连变更 vs live-ro reader 准入 / snapshot 一致性标注进 §6 / 远端 backend 落地形态) | **详见 [`deep-dives/volume.md`](./deep-dives/volume.md) D8**,本表不复制。多为产品层策略或落 §6 时定 |
| **§1.4 侧信道** | co-residency 策略(不共核/不共调度)在**多租户接入时**的具体物化 | 随多租户激活才做,落在 placement 层;五支柱不变 |
| **§4.3 对象回收 sweep(§5.C 挂载行消费者)** | 引用-回收原子性不变量已定(§4.3:derived refcount + 外键);Template lineage 已随 M5 shipped(§5.B)。**待细化的只剩 §5.C 挂载行消费者**:独占租约 = 挂载行 refcount,其 sweep/回收随 Volume 子系统落地定 | 谁消费:§5.C 挂载行(Volume 里程碑);reaper 复用 §4.4-A 反向对账形状 |
| **§4.1 / §4.4-C 事件流(对外面)** | 投递算法、store 原子提交契约、后端(ADR-0004 SQLite)已定。**待细化的是对外面**:event schema、可重放窗口大小、subscriber 接入/续位约定。两处设计使然的已知限制(outbox 只标记不删行、随转换单调增长;`store.DedupSink` 参考实现进程内/非持久)均已在对应代码点留注,随「可重放窗口」一并收口。 | additive(add a subscriber);引擎不追踪 subscriber、只保证可靠写入一条可重放 stream |
| **§4.2 pause 静止点(仅剩参数级)** | 协议两层结构已定(§4.2 + deep-dive D4.4:① drain-barrier ② `SIGTSTP`+观测 `/proc` stopped+超时降 `SIGSTOP`,不引入 guest 内 ack 通道)。**残留纯调参**:grace 时长、可 handle 信号的具体选择、是否给模板一个可选"可协作 flush"钩子(默认不需要) | 非架构,落地时定;不改两层结构。见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.4 |
| **§5.D 凭证存储 seam** | 凭证"留宿主侧"的**后端**(secret-store):怎么存、注册、轮转。此处只留**at-rest 后端**选型;alias→directives 的 **resolve/缓存/失效契约**是另一条 seam,见下行 | 一类可插拔 seam(类比 runtime/FS provider);明文不进控制面/Overlay,只在宿主侧独立后端 |
| **§5.D egress 数据面 seam + 两条契约** | 引擎自有 egress 数据面(gateway,行为 seam)与两条传输无关薄契约的**精确签名**:① **resolve 契约**(数据面↔resolver:alias+目标上下文 → directives+过期+version 戳 + 失效信号)② **ingress 契约**(数据面↔引流:抽象描述符 + 双向劫持能力协商)。行为的两种强制模式(builtin 内联求值 / 外部编译下发)对接口要求不同 | **详见 [`deep-dives/network.md`](./deep-dives/network.md) D3/D7**,本表不复制。今天只建接口形状 + builtin 单实现(模式 1),registry plumbing 缓(D6);谁消费:§5.D 数据面 + 未来远端 provider |
| **§5.D 凭证注入模式** | builtin 首版支持哪些注入模式,各自约束:MITM(TLS terminate-reoriginate,每 SNI 现签叶子)/ 明文 alias 端点 / placeholder 替换 / header 注入 / SSH re-origination | **别过早钉 MITM 为唯一路径**;MITM 选中才需 guest 信任注入(CA 证书进 guest、私钥留数据面),落 §5.B 模板 / §5.E.1。见 [`deep-dives/network.md`](./deep-dives/network.md) D9 |
| **§2.4 / §5.D egress deny-set 分层** | 三条 invariant 已定并毕业进 spec(§2.3/§2.4;推理 [`deep-dives/network.md`](./deep-dives/network.md) D5.3–D5.6)。**残留细粒度**:① L0 地板精确清单(orchestrator 自身端点 / sibling 网段如何在 placement 时算出;link-local/loopback 已定)② 内网 deny-safe 放行的 NetworkPolicy 语法 ③ fail-closed 是否发 §4.1 压力/审计事件(倾向发) | 谁消费:§5.A steering 物化 + §5.D 数据面 dial 逻辑 + placement;L0 清单是引擎资产、两跳共用一份 |
| **§5.A provider 接口(远端形态入参)** | 核心 `Provider` 签名(策略+句柄)已定且经 `local_runsc` 兑现(§5.A)。**待细化的仅剩远端形态的入参形状**:`RootfsSource`(本地 `MergedPath` vs 远端句柄)、`SteeringSpec`(egress 引流描述符,网络里程碑)、`ResourceLimits` 在非 cgroup 调度层的等价物——用"被调度的远端隔离单元"作纸面参照反推,**不实现任何远端 provider** | 谁消费:未来远端 provider;`local_runsc` 已是唯一实现 |
| **§5.B Overlay 物化** | 载体与持久单元(留驻 upperdir)已定(§5.B);upper/work 的 workdir 新建与孤儿回收**已随资源治理里程碑 shipped**(upper+work 同处一个 subvolume,孤儿 subvolume 由对账回收)。待细化:pause flush 一致点的**应用层协作安全点**(substrate 半已定)、**Promote excluded-paths 完整清单**(最小集——sandboxd portal/工作目录——已随 M5 shipped;Volume 挂载点占位待 Volume 里程碑)、content-addressed diff 层(演进项)、multi-node 经 rootfs provider 跨 node 寻址(PV/对象存储) | **详见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D5**,本表不复制 |
| **§5.C per-volume 容量帽在 remote 后端的物化** | 单机 local 臂**已随资源治理里程碑 shipped**(subvolume + squota,§5.B Overlay upper 是首个消费者)。**待细化的只剩 remote 后端**:一个对象存储 / 网络 FS backend 用什么手段兑现"这个卷最多多大"(后端配额 API / 客户端侧核算),以及它与 `Capabilities()` 的关系 | 封在该 backend 内(volume.md D1/D7),不进核心;谁消费:未来 remote volume backend |
| **§2.3 / §4 容量压力事件(软阈那一半)** | 硬阈行为**已 shipped**:guest 见普通 `ENOSPC`、沙箱存活、**只做硬阈拒写**(rootfs-overlay D6.5)。**待细化的只剩软阈**:是否发一条**容量压力事件**作告警(Consumption seam,add a subscriber)、阈值取值 | 刻意**随事件流「可重放窗口」那轮统一收口**——现在单开一个 event kind,到时要改两遍;谁消费:§4.1 事件流 |
| **§5.E transport(残留调参)** | wire 机制(内沿 Connect,D4)与外沿投影(D6)、溢出处置(断开订阅者,D4.3 invariant 1)均已定进 spec/deep-dive。此处仅留**可调数值旋钮**,当前值可随经验再调:① 输出环形缓冲字节上界 = **64 KiB**(重绘保真 vs 内存)② 慢订阅者发送窗口 = **256 帧** ③ `keepalive` 周期 = **20s** | 纯数值,不影响 D4/D6 形状;谁消费:§5.E transport 实现 |
| **§5.A / 模板(内容目录)** | **内置 Template 的内容清单**:一个 bare base(uv/pip/git/build-essential;可选 sudo,仅作 guest 内便利、非引擎语义),各 agent 栈(claude-code/opencode/pi)叠在 bare base 之上。**这是产品/模板层的内容选型**,仍开放;其一等行化 + boot seed + 叠层机制(multi-lowerdir)**已随 M5 shipped**(§4.3/§5.B) | guest 默认 root(§2.2),装包是一次性 exec、不引入 init 依赖,见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1 |
| **§6 部署形态** | 高层形态已定(§1.1:单二进制 + 配置 + 本地 store,store 后端见 ADR-0004);仅待细化打包 / 配置 schema | 对齐 §1.1 部署目标 |

---

## 3. 待拍板的选型(有候选,未定论)

| 选型 | 候选 | 当前倾向 | 归属 / 谁消费 |
| --- | --- | --- | --- |
| **checkpoint/restore 恢复 seam** | 进程态 checkpoint 恢复 / agent 原生 `--resume` 重放 | 两条都留,session 层按能力选。**注意与「无缝内存 resume」(§1.4 永久排除)不是一回事**:这里是**单进程级**冻结/恢复,不是全 VM 内存镜像(后者锁 CPU-feature、被 §4.2 否决) | [`deep-dives/transport.md`](./deep-dives/transport.md) D3 seam / D5 |

> **已拍板并移出本表:disk I/O 速率帽 = 只做 CPU。** io 轴推迟到有真实消费者时再 additive 加(接口加
> 一个字段 + OCI 多一段,不动形状)。依据是 §1.3 成本判据——**缺的是消费者,不是机制**:§6.1 的 create
> 字段、`model.Sandbox` 的 Binding、schema 四列、10 条验收断言全都只列 CPU,没有一条读 io。机制侧已实测
> 可行(runsc 会把 OCI `blockIO` 写进它那个叶子的 `io.max`),**不是被 substrate 堵住**;真正的连带待决项
> 是"整块盘的 MAJ:MIN 从哪来"(必须给整盘,给分区号 launch 直接失败`no such device`)——那是宿主拓扑,
> per-sandbox Binding 里没有它,等有消费者时一并定。§3.4 / §5.A 的速率轴措辞含 io,故这是"io 那半留在
> settled-not-yet-built",**不是把它划出 spec**;§2.3 的爆炸半径不依赖 io 帽(容量轴由 squota 守、CPU 由
> `cpu.max` 守),推迟**不留安全缺口**。`ResourceLimits` 的注释已按此措辞。

---

## 4. 实现缺口(设计已定,代码未落地 / 待收口)

> 这些**不是未定决策**——设计已在 spec/deep-dive 定死,只是代码尚未兑现或有待收口的小项。按
> CLAUDE.md「settled + not-yet-built 属 plan」,owning milestone 的 plan 出现后应迁入其中并从此移除。

| 缺口 | 现状 vs 目标 | 落地归属 |
| --- | --- | --- |
| **① outbox publisher 头阻塞**(已留注,非正确性破坏) | 一条投递失败挡住其后全部(保序 / at-least-once 正确) | 随事件流可重放窗口(后续增量)择机收口 |
| **② PID1 退出码竞争**(已留注,非正确性破坏) | `init.go` 盲 `Wait4(-1)` reaper 与监督循环 `cmd.Wait()` 争 transport 子进程退出码,输者拿 `ECHILD`;自愈重启不受影响,只退出码/telemetry 可能丢(Go 1.23+ pidfd Wait 或已收窄) | 择机收口 |
| **③ PID1 容器级信号扇出单子进程**(已留注,非正确性破坏) | 当前只转发给唯一的 transport 子进程(transport 是唯一子进程时正确) | 模板自带长跑 sibling 落地后升级成进程组扇出(§5.E.1) |
| **④ 转换锁 map 只增不删**(已留注,非正确性破坏) | `lifecycle.go` 每见一个新 SandboxID 加一把锁、destroy 后不回收(高 churn 下慢泄漏);destroy 时删锁会与阻塞在同锁上的动词竞争(令阻塞动词与新动词序列化到不同锁上),需谨慎处理 | 择机收口 |
| **⑤ Promote 不过节点容量准入门** | `Promote` 固化新层会**增加"无帽覆盖的字节"**(核算式第二项,rootfs-overlay D6.5),但它不经准入门,故可把 Σ 推过节点预算。原则已定(§3.4:任何增加无帽字节的控制面动作都应过同一道不等式),只是代码未兑现 | 落地归属待定的下一个里程碑;**不追加进已冻结的资源治理里程碑** |
| **⑥ `death_cause` 不在 §6.1 读面上** | 死因已落库(§4.3),但 `GET /v1/sandboxes/{id}` 不返回它,`resource-terminated` 只以 409 错误串现身。既然恢复决定归调用方(§4.3 narrowing),调用方就需要看到它所依据的那条事实 | 随事件流「可重放窗口 / event schema」那轮统一定读面(§2 的 §4.1 行) |

---

> **纪律:** 任何一项拍板后,先更新对应深挖文档或 plan 的相应章节,**再从本登记移除**——保证
> "已定"进 plan、"未定"留本文,两边不重叠。§4 的缺口同理:owning plan 出现即迁入、从此移除。
