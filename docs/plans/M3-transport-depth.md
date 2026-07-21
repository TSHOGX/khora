# M3 — transport 深度:process/fs 数据面兑现到"多连接并发"

> **✅ SHIPPED — 历史记录,非活动计划。** 本里程碑已交付并过评审门;A1–A10 + B1–B7 回归 + C1–C7 在真机
> runsc 上 `-race` 全绿,D4.3 慢订阅者处置(断开)与外沿投影(D6)已回写 spec/deep-dive。引擎"现在是
> 什么"以 `docs/spec/` 为准,本文只留作前瞻记录(`docs/plans/README.md` 的 expire-as-shipped 纪律)。
> 后续里程碑不改本文。

> engine 的 **M3 前瞻计划**。只答四件事:切到哪(scope)、开工前备好什么(DoR)、按什么次序开工
> (risk-first)、怎么算通过(acceptance)。架构规格在 `docs/spec/`——§5.E 的**边界**已在
> `subsystems.md §5.E` 定死,wire **机制**已在 [`deep-dives/transport.md`](../spec/deep-dives/transport.md)
> D4(内沿)/ D6(外沿)**完整设计**,本文**不复述**,只写 M3 要**造什么、怎么验**。与 spec 冲突不
> 就地调和,按 `CLAUDE.md`「Relationship rule」停下分析(§7)。本文随里程碑推进而失效。

---

## 1. 目标:把 §5.E 从骨架兑现成产品原子

M1 把传输**载体**立对了(netns 内 TCP、Connect-RPC/h2c、`Health` 探针、`Start` server-stream 单流、
`SendInput`/`Read`/`Write`、PID1 原地重启子进程),M2 完全没碰它。但 §5.E 的**产品面仍是骨架**——
今天的沙箱**不能被多个连接并发挂载**,而这恰是 §1.2 / §3.2 的头条能力(「一个沙箱可并发挂多条连接,
共享同一 FS 与网络」)。具体缺口(设计已在 D4,代码未兑现):

- **PTY 被显式拒绝**(`cmd/sandboxd/process.go`:`"pty not in M1 scope"`)——shell 是 §3.2 的一等连接类型,没 PTY 就没 shell。
- **无 `Connect` 重连**——`Start` 起的进程,连接一断就再也接不回;§3.2「可 `Connect` 重连」不成立。
- **输出无扇出**——一次 `Start` 一条流,做不到「多个连接挂同一 pid」(§3.2 多路并发)。
- **无有界输出环形缓冲 + 回放**(D4.3 invariant 2)——重连 / 刷新只能见空屏,不是近期状态。
- **慢订阅者会阻塞源**(D4.3 invariant 1 未兑现)——一个卡住的前端能反压冻结 guest 里的真实进程。
- **外沿数据面当前是临时 REST/NDJSON**(`api.go` handleStart/handleFsRead/Write)——M3 换成数据面 Connect 反代(D6)。

M3 只做**数据面的正确性承重核心**,不做 fs 广度(见 §2)。核心的风险**不在 substrate 集成**(M1 已 retire),
**在扇出并发正确性**——两条 D4.3 invariant 是这一层的 §4.4(编译绿证明不了)。

**本里程碑不新增 spec**:D4/D6 已把机制与外沿投影设计完;M3 是**实现 settled 设计**。撞到 D4 欠定
(如环形缓冲字节上界)按 open-decisions §5.E 的"残留纯调参"取值,不回改 deep-dive 形状。

---

## 2. 边界:显式不做(core-first)

| 不做 | 属于 | 为什么能安全后置 |
| --- | --- | --- |
| **fs 广度**:`List` / `Stat` / `MakeDir` / `Remove` / `Move` + 大文件 multipart 旁路 | M3 后的小增量 | 低风险机械 CRUD,无扇出并发红利;塞进本里程碑会稀释 fault-injection 焦点(决策:core-first) |
| **reaper + 健康态四分 + 连接活动信号 + 周期对账** | M4 | M3 产出的正是 M4 要消费的 activity/liveness 信号;顺序见 `README.md`「Why this order」 |
| **`buf` TS / 多语言 client gen** | 随 §6 增量 | 外沿投影形状(D6)本里程碑定死并兑现 Go 反代;前端 stub 生成是 additive,不阻塞数据面正确性 |
| **checkpoint/restore 进程态恢复 seam** | open-decisions §3 | session 恢复的两条路(进程态 vs `--resume`)仍未拍板;M3 只需守住 D3 的三个 seam,不实现恢复 |
| **session / turn / ACP 语义** | engine 外 adapter | §5.E / D1.2:engine 只暴露 process/fs,session 是引擎外加法,永不进 engine |
| **Volume / egress / Promote / 凭证** | M5–M8 | 与传输数据面正交 |

> **`Update`(pty resize)/ `CloseStdin` 属核心**:它们是 PTY 交互的组成部分(resize 是 shell 刚需),
> 一并做;它们是 unary 点操作,不是广度负担。

---

## 3. 接缝:M3 要扩的接口(内沿 wire + 外沿投影)

M3 的接口变动有两处,都**沿 D4/D6 已定形状**,不发明新抽象:

### 3.1 内沿 wire(`transport.proto` + `transport.Client` 域接口)

D4.3 的 RPC 清单大部分**已在 proto 里**(`Start`/`SendInput`/`SendSignal`/`List` + `PtySize` 字段),
M3 补齐 server 实现与缺失的 RPC:

- **proto 补**:`Connect{pid}`(server-stream)、`StreamInput{pid, stream<bytes>}`(client-stream)、
  `Update{pid, pty.size}`(unary)、`CloseStdin{pid}`(unary);`ProcessEvent.oneof` 补 `keepalive` 帧。
  **proto 是唯一契约源**(D6.2),改 proto → `buf generate` 重生成 stub,不手改 gen 代码。
- **`transport.Client` 域接口补**:对应 `Connect` / `StreamInput` / `Update` / `CloseStdin` 的域方法,
  译 domain↔proto(§5.E seam:mediator 永不见 generated 类型)。`StartSpec.Pty` 已存在,打通即可。
- **不动的部分**:`Health` 探针语义、pid 寻址(不引 tag,D4.6)、无进程注册表(`List` 实时扫 `/proc`)、
  按 pgid 发信号——M1 已立对,不动。

### 3.2 外沿投影(orchestrator 数据面 = Connect 反代,D6)

- **清掉 NDJSON facade**:`api.go` 的 `handleStart`/`handleFsRead`/`handleFsWrite` 三个手写 REST 数据面
  handler 换成 **Connect 反代**——orchestrator 对外挂 `ProcessService`/`FsService` 的 Connect handler,
  内部按 `sandbox_id` 经 `Mediator.Client(id)` 解析当前端点、把外沿 stream **透传**到内沿 `sandboxd`
  (placement 永不外泄,§6.2 / D6.1)。控制面 REST(生命周期/volume/template)**不动**(D6.2 分界)。
- **一份 wire**:外沿复用**同一份** `transport.proto`,不另立 schema(D6.2)。翻译逻辑归零。

**铁律(§4 开头 / `CLAUDE.md`):动词归 mediator,机制归子系统,子系统间永不互调。** 数据面反代是
orchestrator **调** transport client(经 mediator 解析端点),不是让 transport 去调别的子系统。扇出
的环形缓冲 / 慢订阅者窗口是 **transport 进程内**机制(在 `sandboxd` 侧),不进控制面 SoT(§5.E 无状态,
D4.3 已论证环形缓冲落在"无**持久** SoT"措辞内)。

---

## 4. 两条 D4.3 invariant:本里程碑的正确性核心

M3 的风险全在这两条(D4.3「扇出的两条正确性 invariant」)。它们是并发正确性,**必须 fault-inject 验证**:

- **invariant 1 —— 源永不因订阅者阻塞。** 每个 live 订阅者有独立**有界**发送窗口;窗口溢出即对**该
  订阅者**非阻塞丢弃(可直接断开它),**背压绝不回传到源**(PTY/进程)。这把 §5.E「裸 pty lossy」从
  模糊表述钉成可验收规则:lossy 的是**慢订阅者那一份拷贝**,不是源。
- **invariant 2 —— `Connect` 先回放有界环形缓冲、再接 live。** 每进程维护 per-process、in-guest、按
  字节上界的输出环形缓冲(旧字节静默丢弃);`Connect{pid}` **先回放缓冲内近期字节、再切 live 流**。
  重连 / 刷新 / 多端挂载**立刻见近期状态而非空屏**。

两条都**不破 §5.E 无状态**(D4.3 已论证:环形缓冲是瞬时、非权威、随进程消亡、不跨 resume 的加速结构)。
**边界**:engine 只保证 best-effort 字节 tail,不保证屏幕重建(tmux 式 reflow 是引擎外 client 的活,
engine 不理解流内容,D3 seam)。

**残留纯调参**(open-decisions §5.E,落地时取值,不影响形状):环形缓冲字节上界、慢订阅者窗口大小与
溢出处置(丢弃 or 断开)、`keepalive` 周期。

---

## 5. Risk-first 任务分解

先打通 wire + codegen(A),再落最险的扇出核心(B/C,fault-injection 重灾区),再补 PTY 交互(D)、
外沿投影(E),最后回归封口(F)。

| 序 | 任务 | 落点 | 压的不变量 | done-when |
| --- | --- | --- | --- | --- |
| **A** | **proto 扩 + codegen + 域接口**:proto 补 `Connect`/`StreamInput`/`Update`/`CloseStdin` + `keepalive`;`buf generate` 重生成;`transport.Client` 补对应域方法(译 domain↔proto) | `proto/` + `internal/transport` | §5.E seam(mediator 不见 generated 类型) | `buf generate` 干净;`go build`/`go vet` 绿;域接口过 §1.3 litmus |
| **B** | **输出扇出 + 有界环形缓冲**:`sandboxd` 侧把单进程输出做成 fan-out 多订阅 + per-process 有界环形缓冲;`Connect{pid}` 先回放缓冲再接 live | `cmd/sandboxd` | **D4.3 invariant 2** | 起进程产出若干输出后 `Connect` → 先收到近期字节再收 live;多个 `Connect` 各拿独立拷贝、退订不影响其余 |
| **C** | **慢订阅者非阻塞丢弃**:每订阅者独立有界发送窗口,溢出对该订阅者丢弃/断开,**背压不回源**;fault-inject 一个卡死订阅者 | `cmd/sandboxd` | **D4.3 invariant 1** | 注入"永不读的订阅者" → 源进程/PTY `write` 不阻塞、不冻结,其余订阅者与进程正常;慢者被丢弃或断开 |
| **D** | **PTY + 交互闭环**:去掉 `"pty not in M1 scope"`,纯 Go 分配 PTY(no-cgo,sandboxd 是静态 ELF)、`Start{pty}` 走 pty 分支、`Update` resize、`StreamInput` 保序写 pty-in、`CloseStdin` | `cmd/sandboxd` + `internal/transport` | §3.2 shell 连接、pty 保序输入 | 真 runsc:`exec /bin/bash` 带 pty,`StreamInput` 键入 → 回显流回;`Update` resize 生效;`SendSignal` 发进程组 |
| **E** | **外沿 Connect 反代**:orchestrator 挂 `ProcessService`/`FsService` Connect handler,按 `sandbox_id` 经 `Mediator.Client` 解析端点透传到 `sandboxd`;**清掉 NDJSON facade**;控制面 REST 不动 | `internal/orchestrator` | D6.1(placement 不外泄)/ D6.2(一份 wire) | 外沿 Connect client 起进程/重连/读写,端到端经 orchestrator 到 guest;placement 从不出现在外沿;NDJSON handler 删净 |
| **F** | **回归封口**:M1 A1–A10 + M2 B1–B7 全绿;新增 C1–C7(见 §6);`go build`/`go vet`/`-race` + 真跑 runsc | `test/` | DoD 封口 | 全部绿;无回归 |

**A 是其余全部的前置**;B→C 顺序(C 依赖 B 的扇出结构);D 依赖 A(pty wire);E 依赖 A(外沿复用同一 proto);
B/C/D 可在 A 后并行起步。

---

## 6. 验收(可执行 DoD)

本机真跑 runsc(ADR-0002 `sbx`),复用 M1 的 preflight。**全绿 = 数据面正确性 + 多连接并发成立**。

### 6.1 回归:数据面加深不破已有竖切

| # | 断言 |
| --- | --- |
| A1–A10 | M1 全部验收回归全绿(生命周期 + 单流 transport 竖切不变) |
| B1–B7 | M2 全部验收回归全绿(持久 + 三不变量 + 崩溃对账不变) |

### 6.2 M3 新增:transport 深度

| # | 断言 | 层 | 证 |
| --- | --- | --- | --- |
| C1 | **PTY 交互闭环**:真 runsc 起 `/bin/bash` 带 pty,`StreamInput` 键入命令 → 输出流回;`Update` resize 后 `stty size` 反映新尺寸;`CloseStdin` 等价 Ctrl-D | L4(真 runsc) | §3.2 / D4.3 |
| C2 | **多连接扇出**:同一 pid 两个 `Connect` 各收到**完整独立**输出拷贝;断开其一,另一及源进程不受影响(§3.2 多路并发) | L2 + L4 | D4.3 |
| C3 | **`Connect` 回放**:进程先产出输出、订阅者后 `Connect` → **先收到环形缓冲内近期字节、再接 live**,不是空屏;缓冲超上界后旧字节被静默丢弃(见到 tail 而非全量) | L2 + L4 | D4.3 invariant 2 |
| C4 | **慢订阅者不冻结源**(核心):注入一个**永不读**的订阅者 → 源进程 / PTY 的 `write` **不阻塞**、进程照常推进,其余订阅者正常收流;慢者按调参被丢弃或断开。`-race` 无竞争 | L1 + L4(fault-inject) | D4.3 invariant 1 |
| C5 | **信号发进程组**:`SendSignal` 对 fork 子树的父进程 → 整组收到(无孤儿残留);pid 寻址、无 tag | L4 | D4.3 |
| C6 | **外沿经 orchestrator、placement 不外泄**:外沿 Connect client 全程只用 `sandbox_id`,起进程/重连/读写端到端可用;抓外沿响应**无 netns IP / 容器 id / 宿主路径**;NDJSON facade 已删净 | L4 | D6.1 / §6.2 |
| C7 | **长流不被 idle 超时误杀 + keepalive**:一条挂着 idle shell 的输出流保持数十秒不被服务端 idle deadline 静默关闭;`keepalive` 帧周期性到达 | L2 + L4 | D4.2 |

任何一条红在评审门停下,按 `CLAUDE.md`「失败两次就 root-cause」做系统性分析。验证前置(`go build` /
`go vet` + 真跑 runsc + `-race`)见 ADR-0002。

---

## 7. 开工前置与决策归属

- **R1 PTY 无 cgo**:`sandboxd` 是静态 CGO-free guest ELF(与 M1 build-rootfs 同源约束)。PTY 必须用
  **纯 Go**(`syscall` 开 `/dev/ptmx` + `TIOCGPTN`/`TIOCSPTLCK` ioctl,或纯 Go pty 库)——**不引 cgo
  `openpty`**。落 Task D 前先在 `sbx` 上验一次纯 Go pty 在 runsc guest 里能开。
- **R2 `buf` 工具链**:codegen 已配置(`proto/buf.gen.yaml`:`protoc-gen-go` + `protoc-gen-connect-go`,
  Go-only)。Task A 前确认 `buf` + 两个 local 插件在 PATH;TS/多语言 gen 本里程碑不做(§2)。
- **R3 慢订阅者 fault-inject 手柄**:需要一个测试可控的"永不读/极慢读"订阅者注入点(测试钩子,不进
  生产语义),供 C4 制造背压场景。类比 M2 的 `FaultAfterColdStart`——build-tag / Deps 钩子,不污染
  生产路径。
- **决策归属**:外沿投影(控制面 REST / 数据面 Connect)= **已定,D6**(本里程碑兑现,不再议);wire
  机制 = **已定,D4**;**残留纯调参**(环形缓冲上界 / 慢订阅者窗口 / keepalive 周期)在 open-decisions
  §5.E,落地时取值、不回改形状。撞到 D4/D6 欠定按 `CLAUDE.md` Relationship rule 停下:已定回写
  spec/deep-dive、未定登记 open-decisions,两边不重叠。
