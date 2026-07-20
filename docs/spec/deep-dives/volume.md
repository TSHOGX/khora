# Volume:写入 / 一致性 / 租约的推导与证伪

> **定位:[`§5.C`](../subsystems.md)(spec,导航见 [`SPEC.md`](../SPEC.md))与 [`§3.4`](../model.md)、
> [`§4.2`](../model.md)、[`§4.3`](../model.md) 的 Volume 深挖。** §5.C 已载 Volume 的模型与四条已定
> 契约(物化 / 写入 regime / 快照一致性 / 独占租约=挂载行)。**本文不复述那些契约正文**,只留深挖该
> 留的三件事:每条契约**为什么是这个形状**、**排除了哪些备选(证伪)**、**契约边界外待办(D5)**。
>
> **方法论(engine CLAUDE.md):** 从"设计该是什么样"正向推导。三个常见备选机制——FUSE 物化、直连
> 反向代理、心跳租约回收——都是**分布式 / K8s / 远端后端**环境的产物:要么解决单机引擎**根本没有的
> 问题**,要么把产品语义塞进引擎。逐条**从第一性原理重新推导,不照搬**。

---

## D0. 结论

Volume 与 Overlay(§5.B)共用同一条单机物化载体(宿主目录经 gofer 代理进 guest)。它相对 Overlay
只多三个属性,每个属性对应一条契约(正文在 §5.C)和一处证伪:

| Volume 相对 Overlay 的新属性 | 契约(详见 §5.C) | 排除的备选 |
| --- | --- | --- |
| **多写入路径**(挂载 + 直连 API) | 写入 regime 唯一 | 反向代理(D3) |
| **可被多沙箱并发挂** | 跨沙箱 rw coherence undefined | —(D1 说明为何本质如此) |
| **独立快照** | 默认 crash-consistent | 二段式 checkpoint(D2) |
| **独占** | 租约 = §4.3 挂载行 | 心跳双回收(D4) |

---

## D1. 物化:为什么单机不需要 FUSE

Volume 的单机后端就是宿主上一个目录,进 guest 的路径与 §5.B 的 merged rootfs **完全同构**——写进
runsc OCI mount spec、由 gofer 在 `--overlay2=none --file-access=shared` 下代理。exclusive /
shared-ro / shared-rw 都只是"同一宿主目录进一到 N 个 guest",无特殊机制。

**FUSE 只属于远端后端,不进核心。** 用 FUSE 的动机只在 Volume 后端是**远端**(K8s PV / 对象存储)时
成立:必须在 node 本地把远端物化成一个可挂 FS;随之而来的"内核缓存禁用 API 死锁 → 空实现 + TTL
兜底"是**远端物化固有的难**,不是 Volume 抽象的难。本引擎单机后端不经这条路;远端后端(multi-node)
才需要它,那时 FUSE / CSI 连同其缓存一致性风险,属于**那个 FS provider 的实现义务**(§3.1.1 / §5.B
multi-node 物化),封在 provider 内。核心模型只认"Volume 是 ID 寻址、经 FS provider 物化的后端",不认
它是本地目录还是 FUSE 挂点(§3.1.1「对内也 ID 寻址」)。

**跨沙箱 rw coherence 定为 undefined 是本质,不是待完成。** 跨隔离边界的共享可变 FS 一致性本质困难——
每个 guest 的 gofer/sentry 各自缓存,跨边界的 coherence 不可靠。引擎因此**明确不定义**它(而非"孵化
中、以后补"),由用户文档诚实标出即可。这与 FUSE 的取舍同源:跨边界共享可变状态是难题,引擎的回应是
**不承诺**,而不是造一个脆弱机制去假装承诺。

---

## D2. 快照:不对称的根源,与为什么不移植二段式 checkpoint

### D2.1 为什么 Volume 快照不能照搬 Overlay 的 Promote

Overlay 的 Promote 能给强一致(§5.B「只读 at-rest、只在 pause 读」),**前提是 Overlay 单 owner**——
可以 pause 那一个沙箱、flush 到一致点、再读冻结字节。Volume 打破这个前提:shared-rw 卷可能被 N 个
运行中沙箱同时写(不会为快照一个共享卷去 pause 三个不相干沙箱);独占卷虽单 owner,但"挂载中快照"的
诉求本身就意味着不想停掉它。**这个"多 owner / 不想停"就是不对称的根源**——它逼出 §5.C 的默认契约
(crash-consistent),app-consistent 只能由调用方 pause owner 后**组合**得到。

### D2.2 被排除:二段式 checkpoint

一个备选是用"阻止新操作 + 排空飞行事务 + 强制刷盘"的二段式 checkpoint 给挂载中卷拍一致快照。
**不采用**,两头不落好:

1. **对独占卷冗余**:二段式本质就是 §4.2 的 pause 静止点(drain-barrier + flush)。独占卷的
   app-consistent 已用"pause owner 再快照"覆盖(§5.C),在 Volume 子系统里再造一份会让 §5.C 反向
   依赖 §4.2 的静止机制,违反"子系统间不互调"(§4 开头)。
2. **对 shared-rw 不够**:二段式只在**单一写入协调者**下成立;N 个独立沙箱各自的 gofer 没有共同
   协调者,强造一个就是把 swarm 编排塞进引擎(§1.3 否决)。

---

## D3. 写入 regime:为什么一条不变量够了,不要反向代理

hazard 是真的:卷可能同时被运行中沙箱经 gofer rw 写、被直连 API 从宿主侧写同一后端 → 撕裂,且
gofer/sentry 页缓存与直连写不 coherent。§5.C 用一条**不变量**(有 live-rw 挂载则拒直连变更)堵掉它。

**被排除:"直连反向代理到挂载 owner"**(卷挂着时把直连写反向代理进 owner 沙箱,以允许"挂着也能从
外面写")。不采用:

- 这是**产品便利**,不是引擎正确性需求。引擎只要求"别撕裂",§5.C 的**拒绝**就够,成本远低于造一条
  反向代理数据面。
- 反向代理要引擎理解"谁是 owner、如何穿进它的 gofer 写它的 FS",把**跨对象写入编排**拉进引擎。
  §1.3 litmus:它不是 register-impl / add-join-table / add-subscriber,而是改现有结构 → 该问"是不是
  不该进引擎"。答案:要这个便利就叠一层引擎外 adapter(读挂载表 → 经 §5.E `fs` 写进 owner 沙箱),
  引擎不做。

---

## D4. 独占租约:为什么崩溃保持、且不要心跳

§5.C 把独占租约定义成 §4.3 的挂载行(refcount 一等对象)。两个非平凡判断在此展开:

**崩溃保持租约是正确行为,不是漏洞。** 沙箱崩溃 = dead-recoverable(§4.3,L1 探不到、持久态完好),
它**会 resume 回同一个卷**。若崩溃即释放独占租约、别的沙箱抢走,崩溃者 resume 就撞车——**释放才是
bug,保持才是正确**,与 §4.2「崩溃走 resume 恢复、不是特殊路径」一字一致。租约只在挂载行被拆时释放
(destroy / reaper 对 paused 超 TTL 的 evict),reaper TTL 就是"无人释放"的兜底。

**被排除:"心跳失效 + Pod 销毁"双回收。** 它是**分布式无单一权威 store、node 会凭空
消失**的产物——node 死了没人知道,只能靠心跳超时推断。本引擎单机单 orchestrator、store 即权威
(§1.1):没有"凭空消失的 node",崩溃是 L1 探活立即可判的本地事实(§4.3),租约回收退化成"reaper 在
destroy/evict 时 GC 挂载行"。心跳式回收是**未来 multi-node provider 的义务**(那时 node 死亡由其调度
层如 K8s 接管),不进核心——与"多 node 调度天然位于某个 provider 内"(§1.1 / §5.A)同构。

**唯一真残留**:长期 paused / 卡住的沙箱占着独占卷、他方现在就想要——引擎默认独占直到 destroy/evict,
不主动抢占;要不要强制 detach / 抢占是策略(大概率产品层),登记 open-decisions(D5)。

---

## D5. 契约边界外待办(登记 [`open-decisions.md`](../open-decisions.md),不在本文拍板)

- **独占卷的抢占 / 强制 detach 策略**(D4):默认独占直到 destroy/evict;要不要 admin 强制 detach、
  抢占优先级——策略,大概率产品层。
- **shared-rw 打到 per-volume 帽 / ENOSPC 的复合行为**:多沙箱共写一卷、卷写满时各 guest 见到的
  errno、是否发容量压力事件——与 rootfs-overlay.md D6.5 的 per-sandbox ENOSPC 同族。
- **直连变更 vs live-ro reader 的准入细节**(D3 边界):有 live-**ro** 挂载时允许直连写会让 ro reader
  的 gofer 缓存与后端不 coherent;要不要加告警 / 版本戳 / 拒绝,精度问题。
- **snapshot/clone 一致性标注如何进 §6 契约**(D2):API 是否显式标注"本快照 crash-consistent"、
  clone 出的卷是否继承 quota/策略——落 §6 时定。
