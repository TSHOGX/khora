# M6 — 资源治理(把 §3.4 的资源上限从"结构在、值恒为零"变成真 enforce)

> **状态**:forward plan(未开工)。ladder 见 [`README.md`](./README.md);spec 锚点见文末「附」。
>
> **三维度的机制与语义已定进 spec**(§3.4 / §2.3 / §4.3 / §5.A / §5.B),本文**不复述**,只写落地
> 要的五件事:边界怎么切、实测约束怎么约束写法、死因为何只有一个读点、改哪些代码、怎么验收与分期。
>
> **编号约定**:裸 `§N` 一律指 spec 全局章节;指本文自己的节时写「本文 §N」。

---

## 0. 一句话结论(先读这句)

把 §3.4 的资源上限从"结构在、值恒为零"变成真 enforce:**三个维度各用自己该用的机制物化**
——CPU/IO 走 cgroup 速率、磁盘容量走 btrfs squota、RAM 走宿主 memcg。再加两件让它成立的事:
**准入门**(把"每沙箱不撑破自己"抬到"整机撑不满")与**终止原因分类**(让 RAM 终止不被读成崩溃)。

---

## 1. 范围(in scope)

| # | 维度 / 件 | 物化 |
| --- | --- | --- |
| **1** | CPU/IO 速率 | OCI `linux.resources` → cgroup 叶子(引擎建父层、叶子归 runsc) |
| **2** | disk 容量 | btrfs subvolume + **squota** limit,打在 Overlay upper+work |
| **3** | RAM | 宿主 memcg,**对外一个旋钮 / 内部两个值** + `memory.oom.group=1` |
| **4** | 准入门 | `Σ(已承诺帽) + 实测 layer 字节 ≤ 配置的节点容量`;不足则 create 失败 / resume 留 `paused` |
| **5** | 终止原因分类 | `resource-terminated` 处置 + 死因随降级 CAS 落库(§4.3) |

第 5 件不是可选项:第 3 件的后果是一条**生命周期事件**,控制面必须能把它与崩溃分开。否则
"撞内存帽"被读成崩溃 → 去 resume → 同一负载再触发 → 崩溃循环。**它是第 3 件的正确性组成部分。**

---

## 2. 非范围(out of scope,明确划走)

- **per-Volume 容量帽** → M7。本里程碑把 `internal/btrfs` 建成叶子包,**Overlay upper 是首个
  消费者**;Volume 落地时复用同一个包,不重造、也不改它的形状。
- **超卖 + 磁盘压力监控**(选项②)→ 已定选①(不超卖),②作后续增量。
- **容量压力事件 / 软阈告警** → 随事件流「可重放窗口」那轮统一收口(现在单开一个 event kind,
  到时要改两遍)。
- **`pids` 上限** → **永久划走,不是缓做**:spike 实测它限的是宿主 task(guest 每进程 ≈2.3 个
  宿主 task),设低了 sentry 直接 panic(`failed to get clone`)、沙箱消失。它不是资源帽而是
  可用性地雷。§3.4 的资源上限本就只列 CPU/RAM/disk,不顺手加。
- **带外内存监控作 enforcement** → 拒。轮询、有竞态、无硬顶,且是往引擎塞一个新机制(过不了
  §1.3 litmus)。留作以后容量/内存压力**可观测性**输入可以,当帽不行。
- **网络 / egress / 凭证** → M8 / M9 不变(ladder 因本里程碑插入而顺延一位)。

---

## 3. 边界设计(本里程碑最容易做坏的地方,先钉死)

目标是**可切换具体实现**:每一轴都要能换后端而不改上层。四条边界:

**① `ResourceLimits` 是纯策略,cgroup 路径永不进接口(§5.A ❌ list)。**
`LaunchSpec.Limits` 只带**数值意图**,`local_runsc` 在 `buildOCISpec` 里译成 OCI `linux.resources`
字段。**`cgroupsPath` 必须显式给**(省略则叶子建到 cgroup v2 根、污染命名空间),但它是 `local_runsc`
从 cid **内部合成的名字**,与既有 `placementHandle.cid` / `netns` 同类——不是跨 seam 递进来的宿主原语。

> **叶子所有权是一条实测硬约束,搞反了帽会静默失效。** runsc 只创建 `cgroupsPath` 的**最后一段**,
> 且要求**父层已存在**(全不预建则 launch 直接失败:`open .../memory.swap.max: no such file`)。而它
> **只对自己创建的叶子写 `memory.max`**:若引擎预建了**完整**路径,runsc 会写 `memory.swap.max=0` 却
> **不写 `memory.max`**,于是 **RAM 帽静默不生效、launch 照常成功**(实测两臂对照:预建完整叶子
> → `memory.max=max`;只预建父层 → `memory.max=134217728`)。所以:**引擎 mkdir 父层 + 开其
> `subtree_control`,叶子交给 runsc**,并在 launch 后**断言 `memory.max` 等于帽**(门 A 的验收项)。
> 所有权同样决定回收:runsc 只删自己建的叶子,预建的叶子须引擎自己 `rmdir`。

**② RAM 的"两个值"是 provider 内部细节,不上升到接口。**
对外 `MemoryByte = N`(§6.1 create 一个字段)。`local_runsc` 内部把 guest 可见 RAM 设成 N、宿主天花板
设成 `N + overhead`;**overhead 是它的实现常数,不是契约成员**——远端 provider 的开销构成完全不同
(VM balloon / 调度层 memory request),由它自己决定怎么把"N 字节 RAM"物化。**接口只说 N,不说怎么摊。**

**③ 容量帽归各自的 provider,不新开横切 seam。**
`rootfs.Provider` 负责 Overlay upper 的帽,`volume.Backend`(M7)负责 per-volume 帽。**不做一个
`QuotaProvider` 横切接口**——一个远端 FS provider 会用完全不同的手段兑现容量帽(S3 quota / 配额
API),横切接口只会把 btrfs 假设焊进所有实现。共享的是**代码**不是**seam**:

> `internal/btrfs` —— 叶子包,只包 `subvolume create/delete/snapshot` + 配额 limit 三件事,
> **不认识 Overlay / Volume / SandboxID**。§5.B 与 §5.C 各自 import 它,如同各自 import `os`。
> 这不违反"子系统间不互调":两者都不调**对方**,只是共用一个 substrate 工具。

当前 import 图是零跨子系统依赖(只有 `lifecycle → model,transport`),新包必须保持这个性质。

**④ 死因信号跨 seam 时是抽象枚举,不是 errno / cgroup 文件。**
`runtime` 包定义 `DeathCause ∈ {Unknown, Gone, ResourceExceeded}`。`local_runsc` 从自己的 memcg 读
`oom_group_kill` 得出结论,远端 provider 从其调度层的终止原因得出同一结论;**mediator 只看枚举**,
永不知道 memcg 存在。这条让第 5 件事跨 provider 成立,而不是 local 专属 hack。

---

## 4. 实测约束:实现时一定会撞的九条(逐条都是必守项)

机制与语义在 spec(§3.4 / §5.A / §5.B);这里只列**约束实现写法**的实测事实。每条都能让"看起来
配好了"的帽实际不生效,所以逐条都是必须遵守项,不是背景知识。

**速率轴**

1. **CPU quota 精确**:要 0.5 CPU 实测 0.499,每调度周期都 throttle。
2. **别用 `pids`**:它限**宿主 task**(guest 每进程 ≈2.3 个),设低了 sentry 直接 panic
   (`failed to get clone`)、沙箱消失。永久划走(本文 §2)。
3. **测 CPU 用量要对 `cgroup.procs` 全体求和**:systrap 把 guest 线程跑成**独立宿主 thread group**,
   只采 sentry 的 `/proc/<pid>/stat` 会看到 ~0.01 CPU 的假象。验收断言 1 的量法。

**RAM 轴**

4. **`memory.limit` 必须配 `swap`**:只设 limit 时 cgroup 一路换页进 13 GiB zram(192 MiB 帽下分配掉
   1 GiB、`oom` 计数 0),帽形同虚设。OCI `swap` 设成与 limit 相等(⇒ v2 `memory.swap.max=0`)才成硬帽。
5. **`memory.high` 不能用**:`swap.max=0` + 不可回收 anon 会把节流变成挂起(实测 1282 次 throttle、
   guest 停 >45 s)。
6. **两值必须分开摊**(本文 §3②):都设 N 时 guest 被告知 N 却在 0.88N 被终止;overhead 实测
   sentry+gofer 空载 ≈14 MiB。

**容量轴**

7. **squota 下 reflink 免费,故帽对 copy-up 不是硬顶**:同 fs 上 overlayfs copy-up 是 reflink,不受帽
   约束地被接纳。这是选 squota 的理由之一(完整 qgroup 下 `referenced` 会冲过帽再回落,付代价仍无硬顶),
   但**核算时要知道帽只管新写字节**。
8. **copy-up 撞帽不损坏 merged 视图**:跨 fs 真拷贝时 `open(O_WRONLY)` 干净返 errno 28,文件在 merged 里
   尺寸正确、与 lower 逐字节一致,upper/work 无残留——overlayfs 拷进 workdir 临时文件、成功才 link。
   **无需为此写任何防御代码。**
9. **sentry 伪造 `st_blocks`**:guest 对 512 MiB 稀疏文件报 `536870912`,宿主真实值 `0`。
   **容量核算只信配额,永不信 guest 侧数字**(§5.B 已定为契约)。

**连带的核算式**(第 4 件):squota 不覆盖 Template / layer 字节,故它们必须单列——

```
Σ(每沙箱 disk 帽) + Σ(实测 layer 字节) ≤ 配置的节点容量
```

这也正是"不超卖"可算的前提。**`internal/rootfs` 因此要能报出自己持有的 layer 总字节**
(本文 §6 的 `MeasuredBytes`)。

---

## 5. 死因怎么进 store(形状已定;语义在 §4.3 / reaper-health D4c)

规则在 spec:死因随 `Adopt` 的"已死"结论一起交出、与降级 CAS 同一次提交落库、`Unknown` 合法且由
有界重试自我纠正。**这里只记它为什么只能这么做**——两条实测事实把设计空间压到了唯一解:

**① runsc 的磁盘状态里没有死因,所以不存在"事后再问"的世界。** `--root=<dir>` 虽扛宿主重启,但只有
身份 + 创建期快照。四例对照(OOM / 正常退 0 / 外部 SIGKILL / 退 42)的持久 `.state` **逐字段相同**:
无 exit code、无 signal、无 OOM 标志。更要小心:持久 `status` 字段对已死容器**仍写着 `running`**
(创建时写、死时不更新),`runsc state` 报的 `stopped` 是**读时**探记录 pid 活性算出来的——**别信盘上
那个字段**,且重启后该 pid 无意义。

**② 唯一精确的 `ResourceExceeded` 信号是 memcg 叶子的 `oom_group_kill`**,而它只活到 `runsc delete`。
整组击杀后实测 `oom_group_kill 1`、`memory.peak` 恰好顶到帽;容器 `stopped` 但未 `delete` 时仍可读。
**用 `oom_group_kill`、不用 `oom_kill`**(后者是组内单 task 计数,同一次整组击杀实测为 9)。

**于是读点唯一**:`teardownPlacement` 的 `kill`→`delete` 两行同时销毁 cgroup 叶子与 `.state`,而三条
死亡发现路径(启动对账 / 周期对账 / reaper crash-pause)全都经过 `Adopt`——故读必须在 `Adopt` 的
`!alive` 分支返回前。映射:`oom_group_kill > 0` → `ResourceExceeded`,否则 `Gone`,读不到 → `Unknown`。

---

## 6. 代码面(改哪些,按子系统)

| 位置 | 改动 |
| --- | --- |
| **`internal/btrfs`**(新,叶子包) | `CreateSubvolume` / `DeleteSubvolume` / `SetQuotaLimit` / `UsedBytes`。不认识 Overlay/Volume/SandboxID(本文 §3③)。M7 的 Volume 复用同一包 |
| `internal/model` | `Sandbox` 加 `Limits`(CPUMillis / MemoryByte / DiskByte)+ `DeathCause`——前者是 Binding、create 时物化进记录(§3.4);后者随降级落库(本文 §5) |
| `internal/store`(接口) | 无新方法;字段随 `CreateSandbox` / `GetSandbox` 走。**死因须与 `crash-paused` 的 CAS 同一次提交**,故走既有 `CompareAndSetState` 的 Event 通路,不新开写入点 |
| `internal/store/sqlite.go` | `sandboxes` 加 `cpu_millis` / `memory_byte` / `disk_byte` / `death_cause` 四列(**破坏性改 `CREATE TABLE`、删旧 store.db 重建,无 migration**) |
| `internal/store/memory.go` | 同字段,`storetest/contract.go` 覆盖往返 |
| `internal/runtime`(seam) | `ResourceLimits` **不加** `DiskByte`(容量轴归 FS 配额,§3.4);加 `DeathCause{Unknown,Gone,ResourceExceeded}` + `Adopt` 多返回死因(§5) |
| `internal/runtime/local_runsc.go` | `buildOCISpec` 写 `linux.resources`(`cpu.quota/period`、`memory.limit`+**`swap`**、**不写 `pids`**)+ 显式 `cgroupsPath`(**只 mkdir 父层 + 开 `subtree_control`,叶子留给 runsc**,本文 §3①)+ 叶子置 `memory.oom.group=1` + launch 后**断言 `memory.max` == 帽**;RAM 两值摊分(N / N+overhead,本文 §3②);`Adopt` 的 `!alive` 分支在 `teardownPlacement` **之前**读 `oom_group_kill`(本文 §5) |
| `internal/rootfs`(seam) | `CreateOverlay` 增容量帽入参(纯字节数);新增 `MeasuredBytes(ctx) (int64, error)` 报 layer+upper 总字节(供准入门;与 `SweepOrphanLayers` 同族:mediator 问、provider 答) |
| `internal/rootfs/overlayfs.go` | upper+work 落进**一个 btrfs subvolume**(⇒ 同一配额单元,无绕过路径);`DestroyOverlay` 连带删 subvolume + 配额 |
| `internal/lifecycle` | create/resume 前的**准入门**;`coldStart` 填 `LaunchSpec.Limits`;reaper 把死因从 `classify` **带到** `pauseLocked`(否则读了就扔,本文 §5) |
| `internal/lifecycle/classify.go` | `HealthResourceTerminated`(= spec `resource-terminated`)与 `HealthDeadRecoverable` **并列**(不是子类):前者不 resume |
| `internal/orchestrator/api.go` | `POST /v1/sandboxes` 收 `cpu_millis?` / `memory_byte?` / `disk_byte?`;准入失败 → 507 或 409(落地时定文案) |

---

## 7. 验收(e2e on `sbx`,每条 assert 内联其 property)

| # | 断言 | property |
| --- | --- | --- |
| 1 | 帽 0.5 CPU,guest 忙循环,宿主侧对 `cgroup.procs` **全体求和**得 ≈0.5 且 `nr_throttled`>0;**同机另一沙箱不受影响** | 速率轴真节流 + 跨沙箱不干扰 |
| 2 | launch 后 `memory.max` **确等于帽**(而非 `max`) | 叶子所有权搞反则帽静默失效(本文 §3①) |
| 3 | 帽跨 pause → resume 按记录**重新物化**(不是读回冻结值) | 资源上限是 Binding(§3.4) |
| 4 | guest 写到 disk 帽 → `write()` 得 errno 28;**容器仍 `running`、可再 exec** | 容量超限是数据面内事件,不进健康分类 |
| 5 | guest 吃超 RAM → **整组确定性终止**(非抽签);`memory.events` 见 `oom_group_kill`;**宿主日志增量有界** | RAM 安全限 + 根除无限重试(§4.4) |
| 6 | 承 5:死因随 `crash-paused` **同一次提交**落库,判为 `resource-terminated`、**不 resume** | §4.3 处置分类 + 本文 §5 落库原子性 |
| 7 | 死因读不到 → 判 `Unknown` → 走 resume;第二次终止被观测并落库,**不无限循环** | §4.3 有界自我纠正 |
| 8 | `Σ帽 + layer 字节` 超节点容量 → create 失败(无半建残留);paused 沙箱 resume 失败且**留在 `paused`** | 准入门 + §4.4-A 原子退回源态 |
| 9 | destroy → subvolume + 配额全清;崩溃后 sweep 回收孤儿 subvolume | 回收纪律(§4.4-A) |
| 10 | **结构断言**:`runtime.Provider` / `rootfs.Provider` 签名里无 cgroup 路径、无宿主路径、无 memcg 概念 | §5.A ❌ list / §1.3 seam |

第 10 条用一条读接口定义的测试守;其余是 `test/*_substrate_test.go`。断言 5 须带**日志增量上界**
断言——那是唯一真正击穿 §2.3 的失败模式,只验"沙箱死了"会漏掉它。

---

## 8. 分期(单里程碑,内部三道门)

1. **门 A — 速率轴**:CPU/IO 走通,断言 1、2、3、10。最干净、风险最低,先立住 `Limits` 的
   Binding 通路(schema + create + resume 重物化)与**叶子所有权**,后两门复用。
2. **门 B — 容量轴**:`internal/btrfs` + upper 进 subvolume + squota,断言 4、9。
3. **门 C — RAM 轴 + 终止原因分类 + 落库 + 准入门**:断言 5、6、7、8。**风险集中在这门**
   (唯一改 `Adopt` 签名 + 加持久列的一门),故排最后:前两门已可独立交付,**降级时降得干净**。

---

## 附. spec 权威指针

三维度的机制与语义:**§3.4**(物化落点 + RAM 是安全限)· **§2.3**(爆炸半径,含内核日志那条绕过)·
**§2.4**(RAM 的两条 provider 义务)· **§4.3**(`resource-terminated` 处置 + 死因落库 + `Unknown` 兜底)·
**§4.4-A**(原子退回源态)· **§5.A**(资源上限物化 + cgroup 归属 + `Adopt` 契约)· **§5.B**(容量配额)。
推导与证伪:**rootfs-overlay D6**(squota 选型与代价)· **reaper-health D4c**(死因怎么从分类传到处置)。
载体脚本:`test/preflight/spike-qgroup-enospc.sh`(容量轴)· `spike-death-cause.sh`(死因窗口)。

