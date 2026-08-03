# 开放决策登记(Open Decisions)

> spec 只描述已收敛的架构;未拍板的选型、待细化的设计点、落地前要验证的假设登记在本文。`§X.Y` 是全局
> §编号,按 SPEC.md 顶部「文档地图」定位。
>
> **§1** 阻塞子系统的 spike · **§2** 架构已定、内部机制未展开 · **§3** 有候选未定论 · **§4** 实现缺口
> (设计已定、代码未兑现;有 owning plan 后迁入该 plan)。每条标注归属 + 当前倾向 + 谁消费;已在深挖
> 文档详述的只给一行摘要 + 指针,**不复制正文**。

---

## 1. 落地前验证(spike,阻塞相关子系统)

> 无。

---

## 2. 待细化的设计点(架构已定,内部机制未展开)

| 归属(plan §) | 待细化内容 | 备注 / 当前倾向 |
| --- | --- | --- |
| **§5.C Volume(契约边界外)** | 契约在 §5.C;待细化的边界项:独占卷抢占策略 / shared-rw ENOSPC 复合行为(连带 §2.3 爆炸半径在多写入方下的复合表述)/ 直连变更 vs live-ro reader 准入 / snapshot 一致性标注进 §6 / 远端 backend 落地形态 | **详见 [`deep-dives/volume.md`](./deep-dives/volume.md) D8**,本表不复制。多为产品层策略或落 §6 时定 |
| **§4.3 对象回收 sweep(§5.C 挂载行消费者)** | 待细化的是 §5.C 挂载行这一类消费者:独占租约 = 挂载行 refcount,其 sweep/回收形状随 Volume 子系统定 | 谁消费:§5.C 挂载行;reaper 复用 §4.4-A 反向对账形状 |
| **§4.1 / §4.4-C 事件流(对外面)** | 待细化的是对外面:event schema、可重放窗口大小、subscriber 接入/续位约定。两处设计使然的限制(outbox 只标记不删行;`DedupSink` 参考实现非持久)随「可重放窗口」一并收口 | additive(add a subscriber);引擎不追踪 subscriber、只保证可靠写入一条可重放 stream |
| **§4.2 pause drain-barrier 的 grace 时长** | barrier 已建(单层 host 侧,§4.2 / D4.4;guest 内协作式冻结已否决)。**待定的只是排空 grace 取值**:当前 5s,可随经验再调 | 纯数值,不改结构。见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.4 |
| **§5.D 凭证存储 seam** | 凭证"留宿主侧"的**后端**(secret-store):怎么存、注册、轮转。此处只留**at-rest 后端**选型;alias→directives 的 **resolve/缓存/失效契约**是另一条 seam,见下行 | 一类可插拔 seam(类比 runtime / rootfs provider);明文不进控制面/Overlay,只在宿主侧独立后端 |
| **§5.D egress 数据面 seam + 两条契约** | 引擎自有 egress 数据面(gateway,行为 seam)与两条传输无关薄契约的**精确签名**:① **resolve 契约**(数据面↔resolver:alias+目标上下文 → directives+过期+version 戳 + 失效信号)② **ingress 契约**(数据面↔引流:抽象描述符 + 双向劫持能力协商)。行为的两种强制模式(builtin 内联求值 / 外部编译下发)对接口要求不同 | **详见 [`deep-dives/network.md`](./deep-dives/network.md) D3/D7**,本表不复制。今天只建接口形状 + builtin 单实现(模式 1),registry plumbing 缓(D6);谁消费:§5.D 数据面 + 未来远端 provider |
| **§5.D 凭证注入模式** | builtin 首版支持哪些注入模式、各自约束 | **别过早钉 MITM 为唯一路径**;MITM 选中才需 guest 信任注入(CA 证书进 guest、私钥留数据面),落 §5.B 模板 / §5.E.1。**详见 [`deep-dives/network.md`](./deep-dives/network.md) D9**,本表不复制 |
| **§2.4 / §5.D egress deny-set 分层** | 三条 invariant 在 §2.3/§2.4(推理见 [`deep-dives/network.md`](./deep-dives/network.md) D5.3–D5.6)。**待细化的细粒度**:① L0 地板精确清单(orchestrator 自身端点 / sibling 网段如何在 placement 时算出;link-local/loopback 已定)② 内网 deny-safe 放行的 NetworkPolicy 语法 ③ fail-closed 是否发 §4.1 压力/审计事件(倾向发) | 谁消费:§5.A steering 物化 + §5.D 数据面 dial 逻辑 + placement;L0 清单是引擎资产、两跳共用一份 |
| **§5.A provider 接口(远端形态入参)** | **待细化的是远端形态的入参形状**:`RootfsSource`(本地 `MergedPath` vs 远端句柄)、`SteeringSpec`(egress 引流描述符)、`ResourceLimits` 在非 cgroup 调度层的等价物——用"被调度的远端隔离单元"作纸面参照反推,**不实现任何远端 provider** | 谁消费:未来远端 provider |
| **§5.B Overlay 物化** | 待细化:pause flush 一致点的**应用层协作安全点**、**Promote excluded-paths 中的 Volume 挂载点占位**、content-addressed diff 层、multi-node 经 rootfs provider 跨 node 寻址 | **详见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D5**,本表不复制 |
| **§5.C per-volume 容量帽在 remote 后端的物化** | local 臂是 subvolume + squota(§5.B/§5.C 共享原语)。**待细化的是 remote 后端**:一个对象存储 / 网络 FS backend 用什么手段兑现"这个卷最多多大"(后端配额 API / 客户端侧核算),以及它与 `Capabilities()` 的关系 | 封在该 backend 内(volume.md D1/D7),不进核心;谁消费:未来 remote volume backend |
| **§2.3 / §4 容量压力事件(软阈)** | 硬阈是既定行为:guest 见普通 `ENOSPC`、沙箱存活、只做硬阈拒写(§3.4 / rootfs-overlay D6.5)。**待细化的是软阈**:是否发一条**容量压力事件**作告警(Consumption seam,add a subscriber)、阈值取值 | 刻意**随事件流「可重放窗口」那轮统一收口**——现在单开一个 event kind,到时要改两遍;谁消费:§4.1 事件流 |
| **§5.A / 模板(内容目录)** | **内置 Template 的内容清单**:一个 bare base(uv/pip/git/build-essential;可选 sudo,仅作 guest 内便利、非引擎语义),各 agent 栈(claude-code/opencode/pi)叠在 bare base 之上。**这是产品/模板层的内容选型**,仍开放(Template 的一等行化 / boot seed / multi-lowerdir 叠层机制是 §4.3 / §5.B 的既定架构,不在本行) | guest 默认 root(§2.2),装包是一次性 exec、不引入 init 依赖,见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1 |

---

## 3. 待拍板的选型(有候选,未定论)

| 选型 | 候选 | 当前倾向 | 归属 / 谁消费 |
| --- | --- | --- | --- |
| **disk I/O 速率帽的整盘 MAJ:MIN 来源** | 宿主拓扑配置项 / 由 provider 自行推导 | 速率轴只做 CPU,io 轴待有真实消费者时同形加入(接口加一个字段 + OCI 多一段,不动形状;§1.3 成本判据:缺的是消费者,不是机制)。**待定的是**:`io.max` 需整块盘的 MAJ:MIN(给分区号则 launch 直接失败),而 per-sandbox Binding 里没有宿主拓扑 | 谁消费:§5.A 资源上限物化 |

---

## 4. 实现缺口(设计已定,代码未落地 / 待收口)

| 缺口 | 现状 vs 目标 | 落地归属 |
| --- | --- | --- |
| **① outbox publisher 头阻塞**(已留注,非正确性破坏) | 一条投递失败挡住其后全部(保序 / at-least-once 正确) | 随事件流可重放窗口(后续增量)择机收口 |
| **② PID1 退出码竞争**(已留注,非正确性破坏) | `init.go` 盲 `Wait4(-1)` reaper 与监督循环 `cmd.Wait()` 争 transport 子进程退出码,输者拿 `ECHILD`;自愈重启不受影响,只退出码/telemetry 可能丢 | 择机收口 |
| **③ PID1 容器级信号扇出单子进程**(已留注,非正确性破坏) | 当前只转发给唯一的 transport 子进程(transport 是唯一子进程时正确) | 模板自带长跑 sibling 落地后升级成进程组扇出(§5.E.1) |
| **④ 转换锁 map 只增不删**(已留注,非正确性破坏) | `lifecycle.go` 每见一个新 SandboxID 加一把锁、destroy 后不回收(高 churn 下慢泄漏);destroy 时删锁会与阻塞在同锁上的动词竞争(令阻塞动词与新动词序列化到不同锁上),需谨慎处理 | 择机收口 |
| **⑤ Promote 不过节点容量准入门** | `Promote` 固化新层会**增加"无帽覆盖的字节"**(核算式第二项,rootfs-overlay D6.5),但它不经准入门,故可把 Σ 推过节点预算。原则已定(§3.4:任何增加无帽字节的控制面动作都应过同一道不等式),只是代码未兑现 | 落地归属:下一个里程碑 |
| **⑥ `death_cause` 不在 §6.1 读面上** | 死因已落库(§4.3),但 `GET /v1/sandboxes/{id}` 不返回它,`resource-terminated` 只以 409 错误串现身。既然恢复决定归调用方(§4.3 narrowing),调用方就需要看到它所依据的那条事实 | 随事件流「可重放窗口 / event schema」那轮统一定读面(§2 的 §4.1 行) |

---

> **纪律:** 任何一项拍板后,先更新对应深挖文档或 plan 的相应章节,**再从本登记移除**——保证
> "已定"进 plan、"未定"留本文,两边不重叠。§4 的缺口同理:owning plan 出现即迁入、从此移除。
