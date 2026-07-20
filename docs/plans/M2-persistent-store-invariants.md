# M2 — 持久 store + §4.4 三不变量的深度兑现

> engine 的 **M2 前瞻计划**。只答四件事:切到哪(scope)、开工前备好什么(DoR)、按什么次序开工
> (risk-first)、怎么算通过(acceptance)。架构规格在 `docs/spec/`——§4.4 三不变量的**契约**已在
> `model.md §4.4` 定死,本文**不复述**,只写 M2 要**造什么、怎么验**。与 spec 冲突不就地调和,按
> `CLAUDE.md`「Relationship rule」停下分析(§7)。本文随里程碑推进而失效。

---

## 1. 目标:持久化,以及它逼出来的对账

M1 把 §4.4 三不变量的**形状**立对了(CAS、转换锁、失败 unwind),但用的是进程内 `store.Memory`——
一把 mutex 站位真事务。M2 只换**一样东西**:`store.Memory` → 嵌入式持久后端。转换锁 map 与 placement
live-map **不换**——它们本就该是进程内的:placement 派生自 store 权威态、每次 resume 重绑(§3.3),锁
在单进程单机(§1.1)下无需跨重启存活。

**关键洞察:让记录持久,正是"对账"这个问题的来源。** `store.Memory` 崩溃时记录一起丢,没什么可对;
换成持久后端后,**崩溃后 store 记得 `running`/`paused`,但它蕴含的 placement 已随进程蒸发**——持久记录
第一次可能比它的 placement 活得久。于是持久化与 §4.4-A 崩溃对账是**同一个里程碑的一体两面**。

M2 因此把 M1 立对形状的三条不变量各压到"真并发 / 真崩溃"下成立:

| 不变量 | 契约(spec) | M2 造什么 |
| --- | --- | --- |
| **§4.4-C** 状态+事件单次原子提交 | `model.md §4.4` | store 接口扩 outbox;一份 at-least-once 投递算法;subscriber 幂等去重参考实现 |
| **§4.4-B** 转换锁 + CAS,锁全程持有 | `model.md §4.4` | 持久 store 下的**真并发压测**(`-race`),证明 destroy 打不穿 in-flight resume |
| **§4.4-A** 失败原子退源态 + 重启对账(回收孤儿 + 接管存活) | `model.md §4.4` | orchestrator 启动对账:孤儿回收 + 存活 running 重新接管;provider 扩 Adopt/ReclaimOrphans |

**两处已定选型(开工即成立,归属见 §6):** 后端 = **SQLite**(纯 Go,ADR-0004);对账 = **仅启动**
(周期对账归 M3)。

---

## 2. 边界:显式不做

| 不做 | 属于 | 为什么能安全后置 |
| --- | --- | --- |
| **周期对账** + §4.4-B 持锁辨识(跳过 in-flight resume 的活 placement) | M3 | 启动对账(M2)锁全没了,无需与转换锁协调;活着的 orchestrator 周期对账要辨识"正在建的活 placement"、须持锁辨识,与 reaper 同属 M3 |
| reaper + 健康态四分 + single-flight 分类器 | M3 | M2 对账仅启动触发,不引运行时健康分类 |
| Volume / egress / Promote / 凭证 / 容量帽 | M4–M7 | 与 §4.4 三不变量正交 |
| 对外 event schema / 重放窗口 / subscriber 接入续位 | 随后续增量 | §4.4-C:引擎只保证"可靠写入一条可重放 stream";schema 是 subscriber 侧 / 落 §6 时定 |

> `store.Memory` **不删**——它是 §1.3 Behavior seam 的第二实现,L1 单测继续用(快、不落盘)。
> M2 的 SQLite 实现与它**并存于同一接口**,装配谁由 orchestrator 决定(register-an-implementation)。

---

## 3. 接缝:M2 唯一要扩的接口

M1 已把 `store.Store` 的 CAS 形状立对,接口注释预留了 §4.4-C 的原子承诺。M2 的活是**给转换方法带上
事件、让两后端兑现同一条原子契约**——这是 M2 唯一的接口形状变动:

- **转换方法带事件(§4.4-C)**——目标形状:每个状态转换方法在**同一次提交**里落"状态变更 + outbox
  行"。倾向让 `CreateSandbox` / `CompareAndSetState` / `DeleteSandbox` 各多收一个 `Event`(由 mediator
  按转换语义构造,带**转换唯一 ID**),而非新增可被单独调用、破坏原子性的 `AppendEvent`。**铁律
  (§4.4-C):方法本身承诺原子,后端兑现;绝不设计成"后端返回是否支持事务、算法据此分支"。** SQLite 走
  `BEGIN/COMMIT`,`store.Memory` 走"同一把 mutex 下改 map + append slice",对上层**同一份**算法、零分支。
- **发布器读写**——`UnpublishedEvents` / `MarkPublished`,异步读未发出行、发出、标记。不进原子提交契约
  (不与状态转换同事务),两后端同样实现。
- **不动的部分**:CAS 精确语义(expected≠current→`ErrConflict`)、四动词合法源态、`SandboxID` 不编码
  placement(§3.3)。M1 已立对,换后端不动。

**`runtime.Provider` 也要扩(§4.4-A 对账)**——§5.A 禁止 mediator 直接碰宿主原语,故对账必经 provider。
加两个方法:`Adopt(ctx, sandboxID) → (placement, alive, err)`(按 SandboxID 重认自己的资源、重建句柄、
重发现端点;死/不存在 → `alive=false`)与 `ReclaimOrphans(ctx, keep []sandboxID)`(拆掉不在 keep 集里的
全部 engine placement)。两者左边收 engine 词汇(SandboxID)+ store 派生集,右边出**不透明**句柄——过
§5.A ❌ 清单(不泄漏 cgroup 路径 / netns fd / 宿主绝对路径)。这要求 **placement-local 资源携带 SandboxID
标签**(`local_runsc` 今天按 subnet-seq 命名、忽略 `LaunchSpec.Sandbox`);这是 §3.3「身份不编码 placement」
的反向,允许(顺带 closes `local_runsc` 的 ipSeq 跨重启 TODO)。

**一条铁律(§4 开头 / `CLAUDE.md`):动词归 mediator,机制归子系统,子系统间永不互调。** 对账是
mediator 启动路径**调** provider 的枚举/回收 + 调 store 的按态对账;store 权威态由 mediator 读出、
喂给 provider,不是让 runtime 去调 store。

---

## 4. 启动对账:崩溃后怎么收拾(§4.4-A 的核心)

进程刚崩 ⟹ live-map 空、转换锁全无 ⟹ 无"正在建的活 placement"要靠持锁辨识保护(那是周期对账才需的,
M3)。但**不能一律回收**:§4.4-A 说 `running` 沙箱的**活** placement 本就不是孤儿。对账因此是**对偶**的
——回收孤儿 + 重新接管存活者。逐条按 store 态处理(枚举本机 engine placement 后,与 store 记录 diff):

- `creating` → **删记录 → destroyed**,其半建 placement 归孤儿回收(create 半途崩,从未 running,无可退静止态)。
- `paused` → **留记录**;半建 placement 若有(崩在 resume 中途)归孤儿回收,upper 完好、可 resume。
- `running` → **探活其隔离单元**:**活** → `Adopt` 重建句柄 + 重发现端点,填回 live-map,**留 `running`**
  (同一 placement、同一 IP,**不是 resume、不换 epoch**);**死** → CAS 降 `paused`(§4.2 非自愿 pause),
  半建 placement 归孤儿回收。
- 不支撑任一上述记录的 placement = **孤儿** → 拆除:mediator 先力拆(`MNT_DETACH`)孤儿 merged
  overlay(**按 target 路径 scope,绝不按 `-t overlay` 类型**),再由 `ReclaimOrphans` 做 runsc
  kill/delete → `ip netns del`(级联对端 veth)。启动对账无进程内句柄,overlay-umount 早于 runsc-kill
  安全(力拆只摘 mount、内核延迟回收、upper 不碰;详见 §4.4-A「回收次序」)。持久态(upper 目录)**分毫不动**。

接管是正确性、不是优化(理由在 spec §4.4-A「另一半」):不接管则存活 `running` 沙箱**失联**——引擎握不到
句柄,此后 destroy 漏拆活容器(泄漏)、transport 代理因"无活 placement"失败。

对账全程无阶段日志、不引世代计数,是"一次本地扫描 diff 一张本地表"。**`running`→`paused`(判死时)是
M2 唯一新造的对账态转换**,它也走 §4.4-C 原子提交发事件——把 §4.4-A 与 §4.4-C 在一条路径上打通。

---

## 5. Risk-first 任务分解

风险不在 substrate 集成(M1 已 retire),在**并发正确性**与**崩溃对账**——编译绿证明不了任何一条。
故先落后端 + 契约(两实现交叉验证),再压并发,最后打对账(最险、最需故障注入)。

| 序 | 任务 | 落点 | 压的不变量 | done-when |
| --- | --- | --- | --- | --- |
| **A** | store 接口扩 outbox 契约;`store.Memory` 先兑现;写**契约级共享单测**(两后端跑同一套断言) | `internal/store` | §4.4-C 契约面 | 接口过 §1.3 litmus;Memory 兑现;共享单测就位 |
| **B** | **SQLite 后端**(纯 Go,`CGO_ENABLED=0`,ADR-0004):全 `store.Store` + outbox,状态+事件进同一 `BEGIN/COMMIT`;跑通 A 共享单测,**验证 §4.4-C 契约在 SQLite 上可兑现** | `internal/store`(新实现) | §4.4-C 后端兑现 | 两后端过**同一份** A 单测;commit 中途崩不留半写 |
| **C** | **outbox 发布器 + 投递算法**:异步读未发出行 → 投递到进程内可重放 sink → 标记;事件带转换唯一 ID;写 subscriber 幂等去重参考实现证 at-least-once | `internal/store` 侧发布器 + `internal/lifecycle` 接线 | §4.4-C 投递 | 注入"投递后、标记前崩" → 重发同一 ID → 去重后不重复记账 |
| **D** | mediator 接持久 store + **真并发压测**:N goroutine 抢同一沙箱四动词,`-race`;证锁全程持有——destroy 在 resume 持锁期拿不到锁、删不掉 in-flight overlay | `internal/lifecycle` | §4.4-B 深度 | `-race` 无竞争;并发 destroy 打不穿 in-flight resume;CAS 拒非法源态 |
| **E** | **`runtime.Provider` 扩对账能力**:placement-local 资源携带 SandboxID 标签(`local_runsc` 改按 SandboxID 命名,去掉 ipSeq 跨重启 TODO)+ `Adopt(sandboxID)→(placement,alive)` + `ReclaimOrphans(keep)`;签名过 §5.A ❌ 清单 | `internal/runtime` | §4.4-A 机制面 | 真 runsc:Adopt 对活容器重建句柄 + 重发现同一 IP;ReclaimOrphans 拆净不在 keep 集的资源 |
| **F** | **启动对账编排**(§4.4-A,§4 的对偶):orchestrator 启动即 diff 本机 placement × store 记录——孤儿回收 / running 探活后 Adopt-or-降 paused;故障注入 kill orchestrator 于转换中途 | `cmd/orchestrator` + `internal/lifecycle` | §4.4-A 崩溃变体 | 见 §6 B5–B7;持久态完好、无 failed 态 |
| **G** | orchestrator 装配 SQLite + 启动跑对账;§6.1 API 不变;M1 A1–A10 **回归全绿** + M2 B1–B7 全绿 | `cmd/orchestrator` + `test/` | DoD 封口 | A1–A10 回归 + B1–B7 新断言全绿 |

**A 与 E(provider 对账能力)无强依赖,可并行起步**;C 可与 B 后段并行;D 依赖 B;F 依赖 B + E。

---

## 6. 验收(可执行 DoD)

本机真跑 runsc(ADR-0002 `sbx`),复用 M1 的 R1–R3 preflight。**全绿 = 三不变量深度成立**。

### 6.1 回归:换后端不破竖切

| # | 断言 |
| --- | --- |
| A1–A10 | M1 全部验收在 **SQLite store** 上回归全绿——证明 §1.3 seam:register-an-implementation 换后端,竖切与失败 shape 分毫不动 |

### 6.2 M2 新增:三不变量深度

| # | 断言 | 层 | 证 |
| --- | --- | --- | --- |
| B1 | **原子提交**:两后端跑同一套断言,状态转换与 outbox 追加同成同败;提交点注入崩溃 → 重开后要么都在要么都不在 | L1 + L2 | §4.4-C |
| B2 | **at-least-once + 去重**:注入"投递后、标记前崩" → 重启重发同一转换 ID → 去重后只记一次;宁重发不漏发 | L1 + L4 | §4.4-C |
| B3 | **真并发锁互斥**:N goroutine 抢同一 `paused` 沙箱 resume/destroy/pause,`-race` 无竞争;恰一动词推进,余者对已变态 CAS 被拒或阻塞后被拒 | L1 | §4.4-B |
| B4 | **锁全程持有闭合数据竞态**:resume 持锁冷启期并发 destroy **拿不到锁**,删不掉 in-flight resume 正挂载的 Overlay;resume 落定或 unwind 后 destroy 才推进 | L1 + L3 | §4.4-B |
| B5 | **孤儿回收**:kill orchestrator 于 resume 中途(placement 半建、store 仍 `paused`)→ 重启 → 半建 placement 被回收,**upper 完好**,store 停 `paused`;另注入 `creating`(→删记录)一例,其半建 placement 亦被回收 | L4(真 runsc) | §4.4-A |
| B6 | **存活 running 重新接管**:一个 `running` 沙箱 kill orchestrator(容器**存活**)→ 重启 → 探活判活 → `Adopt` 重建句柄,store 仍 `running`、**IP 不变**(非 resume、不换 epoch);接管后 destroy 能**干净拆掉活容器**(不泄漏)、transport 代理可达。对照:容器**已死**的 `running` → CAS 降 `paused` | L4(真 runsc) | §4.4-A |
| B7 | **对账无孤儿残留 & 幂等**:重启后 `runsc list` + netns 枚举**无 engine 残留**;再跑一次对账无副作用;无 `failed` 中间态、无世代计数 | L4 | §4.4-A |

任何一条红在评审门停下,按 `CLAUDE.md`「失败两次就 root-cause」做系统性分析。验证前置(`go build` /
`go vet` + 真跑 runsc + `-race`)见 ADR-0002。

---

## 7. 开工前置与决策归属

- **R4 SQLite 依赖入库**:纯 Go 驱动,保持 `CGO_ENABLED=0`(与 sandboxd 静态 ELF 同源约束);沿用本机
  `go env`(`GOFLAGS=-mod=mod GOSUMDB=off GOTOOLCHAIN=local`)。
- **R5 崩溃注入手柄**:orchestrator 要有测试可控的"在转换 X 阶段后强制退出"注入点(build-tag / 测试钩子),
  供 B5(制造"placement 半建、store 停源态"的孤儿)与 B6(制造"容器存活、句柄丢失"的接管场景);**不进生产语义**。
- **决策归属**:后端选型 = **ADR-0004**(SQLite);§4.4-A 对账两半(回收 + 接管)已在 spec,落地机制
  (provider 签名 / 探活判据 / SQLite 事务边界 + outbox schema)在 open-decisions,Task B/E/F 验证后回写。
  撞到 spec 欠定按 `CLAUDE.md` Relationship rule 停下:已定回写 spec/ADR、未定登记 open-decisions,两边不重叠。
