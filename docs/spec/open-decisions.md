# 开放决策登记(Open Decisions)

> **这份文档是什么。** [`SPEC.md`](./SPEC.md)(及其分篇 model/subsystems/api-contract)只描述
> **已收敛的当前架构**;凡是**尚未拍板的选型、待细化的设计点、落地前要验证的假设**,一律从 spec 里
> 抽出登记在本文,避免把"未定"与"已定"混在同一篇里。下文出现的 `§X.Y` 是全局 §编号,按 SPEC.md
> 顶部「文档地图」定位到具体文件。
>
> **怎么组织。** 三类分开:**§1 落地前验证**(阻塞子系统的 spike)、**§2 待细化设计点**
> (架构已定、内部机制未展开)、**§3 待拍板选型**(有候选、未定论)。每条标注**归属章节 +
> 当前倾向/fallback + 谁来消费结论**。已在深挖文档里详述的条目,此处只给一行摘要 + 指针,
> **不复制正文**(保持无冗余)。

---

## 1. 落地前验证(spike,阻塞相关子系统)

> **当前无阻塞 spike。** 两个 substrate 载体已定案、结论进 spec:持久层载体(外层内核 overlayfs +
> `--overlay2=none`,§2.5 / §5.B)、transport 载体(netns 内 TCP,§5.E)。两条载体上仍待设计的机制
> (非阻塞)分别见 §2 的 §5.B / §5.E 行。

---

## 2. 待细化的设计点(架构已定,内部机制未展开)

| 归属(plan §) | 待细化内容 | 备注 / 当前倾向 |
| --- | --- | --- |
| **§2 安全模型** | 五支柱各自的 enforcement 细节;§2.3 数据面在 shared-rw Volume 等场景下的复合影响 | 支柱契约已定,逐条 enforcement 后续展开 |
| **§5.C Volume(契约边界外)** | Volume 四条契约已定(§5.C);待细化的四条边界项(独占卷抢占策略 / shared-rw ENOSPC 复合行为 / 直连变更 vs live-ro reader 准入 / snapshot 一致性标注进 §6) | **详见 [`deep-dives/volume.md`](./deep-dives/volume.md) D5**,本表不复制。多为产品层策略或落 §6 时定 |
| **§1.4 侧信道** | co-residency 策略(不共核/不共调度)在**多租户接入时**的具体物化 | 随多租户激活才做,落在 placement 层;五支柱不变 |
| **§4.3 对象回收 sweep** | 引用-回收**原子性不变量**已定(§4.3:引用 +1 与 GC 判定-删除同一次原子 store 操作,复用 §4.4-C 契约;store 权威 refcount,无独立标记清扫器 / 世代屏障)。**待细化**:refcount 物化成计数字段 vs 由"引用行是否存在"查询导出的取舍、sweep 的触发时机与批量策略、destroy 删字节的异步重试与 refcount==0 判定的具体落地 | 不变量已进 spec、不留本表;此处仅登记 sweep **实现**细节。谁消费:§4.3 reaper / §5.C 挂载行 / §5.B Template lineage GC |
| **§4.4-A 周期对账 + 连续健康分类** | 启动对账(对偶回收 + 接管)及其落地机制已在 spec 定死:provider `Adopt`/`ReclaimOrphans` 签名 + SandboxID 标注(§5.A)、两层判活(L1 + 一次 L2 `Health` 探针,§4.4-A/§5.A)、skip-and-log + 保守保护(§4.4-A)。**未定**:① orchestrator 活着时的**周期对账**——与 §4.4-B 转换锁协调(周期跑须跳过持锁沙箱,避免误收进行中 resume 的活 placement)、批量与重试策略、egress 面 nft 类资源的枚举;② **连续健康分类**——把接管期的单次就绪探针升级成 §4.3 四象限(healthy/idle/transport-down/dead-recoverable):启动对账因锁已全无,"L1 活、L2 瞬态不通"一律降 `paused` 偏保守;周期对账 + reaper 才需区分 transport-down 瞬态、避免无谓冷启换 IP。 | 谁消费:§4.3 reaper / §5.A provider(与 reaper 同批) |
| **§4.1 / §4.4-C 事件流(对外面)** | 投递算法、store 原子提交契约、后端(ADR-0004 SQLite)已定。**待细化的是对外面**:event schema、可重放窗口大小、subscriber 接入/续位约定。<br>**两处已知限制(设计使然,非缺陷)**:① outbox **只标记不删行**、随转换单调增长(部分索引 `WHERE published_at IS NULL` 令 unpublished 扫描不退化,at-rest 无界)——修剪策略(保留期 / 归档)随「可重放窗口」一并落地(窗口定 ⇒ 修剪 = 删窗口外已发布行);② `store.DedupSink` 是引擎内**参考实现**,`seen` 进程内 / 无界 / 非持久,重启不记得崩前已投递的 ID(靠 outbox 重发 + 真实 subscriber 侧持久去重兜底,§4.4-C at-least-once)——真实 subscriber 在引擎外自行持久去重,不在引擎内加。 | additive(add a subscriber);引擎不追踪 subscriber、只保证可靠写入一条可重放 stream |
| **§4.2 pause 静止点(仅剩参数级)** | 协议两层结构已定(§4.2 + deep-dive D4.4:① drain-barrier ② `SIGTSTP`+观测 `/proc` stopped+超时降 `SIGSTOP`,不引入 guest 内 ack 通道)。**残留纯调参**:grace 时长、可 handle 信号的具体选择、是否给模板一个可选"可协作 flush"钩子(默认不需要) | 非架构,落地时定;不改两层结构。见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.4 |
| **§5.D 凭证存储 seam** | 凭证"留宿主侧"的**后端**(secret-store):怎么存、注册、轮转。此处只留**at-rest 后端**选型;alias→directives 的 **resolve/缓存/失效契约**是另一条 seam,见下行 | 一类可插拔 seam(类比 runtime/FS provider);明文不进控制面/Overlay,只在宿主侧独立后端 |
| **§5.D egress 数据面 seam + 两条契约** | 引擎自有 egress 数据面(gateway,行为 seam)与两条传输无关薄契约的**精确签名**:① **resolve 契约**(数据面↔resolver:alias+目标上下文 → directives+过期+version 戳 + 失效信号)② **ingress 契约**(数据面↔引流:抽象描述符 + 双向劫持能力协商)。行为的两种强制模式(builtin 内联求值 / 外部编译下发)对接口要求不同 | **详见 [`deep-dives/network.md`](./deep-dives/network.md) D3/D7**,本表不复制。今天只建接口形状 + builtin 单实现(模式 1),registry plumbing 缓(D6);谁消费:§5.D 数据面 + 未来远端 provider |
| **§5.D 凭证注入模式** | builtin 首版支持哪些注入模式,各自约束:MITM(TLS terminate-reoriginate,每 SNI 现签叶子)/ 明文 alias 端点 / placeholder 替换 / header 注入 / SSH re-origination | **别过早钉 MITM 为唯一路径**;MITM 选中才需 guest 信任注入(CA 证书进 guest、私钥留数据面),落 §5.B 模板 / §5.E.1。见 [`deep-dives/network.md`](./deep-dives/network.md) D9 |
| **§2.4 / §5.D egress deny-set 分层** | 三条 invariant 已定并毕业进 spec(§2.3/§2.4:分层 deny-set、IP 承重墙、两跳都钉、fail-closed、三档姿态 L0 code-deny/内网 deny-safe/公网默认放行;推理 [`deep-dives/network.md`](./deep-dives/network.md) D5.3–D5.6)。**残留细粒度**:① L0 地板精确清单(orchestrator 自身端点 / sibling 网段如何在 placement 时算出;link-local/loopback 已定)② 内网 deny-safe 放行的 NetworkPolicy 语法 ③ fail-closed 是否发 §4.1 压力/审计事件(倾向发) | 谁消费:§5.A steering 物化 + §5.D 数据面 dial 逻辑 + placement;L0 清单是引擎资产、两跳共用一份 |
| **§5.A provider 接口** | runtime provider 接口的**精确签名**(策略+句柄) | 以 `local_runsc` 为唯一实现,用"被调度的远端隔离单元"作纸面参照反推"策略不泄漏宿主原语"的边界,**不实现任何远端 provider** |
| **§5.B Overlay 物化** | 载体与持久单元(留驻 upperdir)已定(§5.B);待细化:workdir/孤儿回收、pause flush 一致点、Promote 的 excluded paths、content-addressed diff 层(演进项)、multi-node 经 FS provider 跨 node 寻址(PV/对象存储) | **详见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D5**,本表不复制 |
| **§3.4 / §5.B / §5.C disk 容量帽物化** | 两轴切分(速率归 cgroup、容量归 FS 配额)与 per-sandbox upper 帽的三种手段已定(§3.4 / §5.B / D6:首版 project quota,work+upper 同配额)。**待细化**:首版 project quota 的落地(project id 分配/回收、软硬双阈取值、destroy 时清理)、Volume per-volume 帽在其后端的物化 | **详见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D6**;谁消费:§5.B/§5.C FS provider |
| **§2.3 / §4 ENOSPC 标准行为** | per-sandbox 帽把"撑满整机"降级成"撑满自己"已定(§2.3);**待细化**:打到自己帽时的标准行为——guest 见普通 `ENOSPC`(定)、是否发一条**容量压力事件**(Consumption seam,add a subscriber 做告警)、是否触发 auto-pause(倾向不强制)、软阈告警 + 硬阈拒写的双阈是否都做 | additive(add a subscriber);谁消费:§2.3 爆炸半径不变量 + §4.1 事件流 + 可选 §4.3 reaper |
| **§4 节点磁盘压力准入门** | 允许超卖(见 §3 选型)时 per-sandbox 帽不足以保证整机不满;**待细化**:mediator 在 create / resume 物化 upper 前的**节点余量准入检查**(不足则 create 失败 / resume 留 `paused`,落 §4.4-A 原子退回源态,不留脏态)+ 运行时磁盘压力监控的阈值与 reaper 反应 | 谁消费:§4 mediator create/resume 编排 + §4.3 reaper;与超卖选型(§3)耦合 |
| **§5.E transport** | 载体(netns 内 TCP)与 wire 协议(Connect-RPC/h2c、非对称三分 stream、process/fs 端点、进程组信号、`Health` 探针、扇出两条 invariant [源不阻塞 + 有界环形回放]、通道信任模型、长流禁 idle 超时)已成形(§5.E / D4);待收进 spec §6.1 的是:session adapter 划界、API/CLI 投影。**残留纯调参**:① 输出环形缓冲的字节上界取值(重绘保真 vs 内存)② 慢订阅者发送窗口大小与溢出是丢弃 or 断开 ③ `keepalive` 周期 | **详见 [`deep-dives/transport.md`](./deep-dives/transport.md) D4**,本表不复制。机制已定进 deep-dive、不留本表;此处仅登记调参 |
| **§5.E.1 resume 冷启时序** | 拓扑(§5.E.1)、探活二分 + 单飞守卫(§4.3)已定;待收进 spec 的是 resume canonical 链(引擎侧 ①–⑦ + readiness 门)与失败/重试语义 | **详见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5** |
| **§5.A / 模板** | **内置 Template 目录**:一个 bare base(uv/pip/git/build-essential + 非 root + sudo),各 agent 栈(claude-code/opencode/pi)经同一 lineage 机制叠在 bare base 之上(§3.1 分层) | guest 装包是一次性 exec、不引入 init 依赖,见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1 |
| **§6 部署形态** | 高层形态已定(§1.1:单二进制 + 配置 + 本地 store,store 后端见 ADR-0004);仅待细化打包 / 配置 schema | 对齐 §1.1 部署目标 |

---

## 3. 待拍板的选型(有候选,未定论)

| 选型 | 候选 | 当前倾向 | 归属 / 谁消费 |
| --- | --- | --- | --- |
| **checkpoint/restore 恢复 seam** | 进程态 checkpoint 恢复 / agent 原生 `--resume` 重放 | 两条都留,session 层按能力选。**注意与「无缝内存 resume」(§1.4 永久排除)不是一回事**:这里是**单进程级**冻结/恢复,不是全 VM 内存镜像(后者锁 CPU-feature、被 §4.2 否决) | [`deep-dives/transport.md`](./deep-dives/transport.md) D3 seam / D5 |
| **磁盘容量超卖策略** | ① 不超卖(sum(per-sandbox 帽) ≤ 物理盘,准入核算即硬保证整机不满) / ② 超卖 + 磁盘压力监控 + reaper 反应 | 倾向 ② 有限超卖(容量利用率高,配合准入门 + 压力监控兜底);单机简单部署可选 ① 图省心 | §4 准入门 + §4.3 reaper;与上表「节点磁盘压力准入门」耦合 |

---

> **纪律:** 任何一项拍板后,先更新对应深挖文档或 plan 的相应章节,**再从本登记移除**——保证
> "已定"进 plan、"未定"留本文,两边不重叠。
