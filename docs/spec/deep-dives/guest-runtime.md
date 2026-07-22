# Guest 运行时边界:跑什么、谁当 PID 1、resume 怎么冷启

> **定位:[`SPEC.md`](../SPEC.md) 的 guest 侧深挖文档。** 展开两块 spec 只留结论的推理:
> ① **作用域**——引擎只支持"按需 exec 出来的进程",不以系统守护服务为目标(D1–D3);
> ② **guest 启动拓扑 + 协作式静止点 + resume 冷启时序**(D4、D5)。规范结论在 spec §5.E.1 / §4.2 / §4.3。
>
> **编号约定:** 本文自己的节用 `D` 前缀(D1、D4.4…);`§` 一律指 spec 的全局章节(§4.2 等),两套不混。

---

## D0. 一句话结论

引擎只跑一类东西——**经 `process` 按需 exec 出来的进程**,不做系统守护服务(D1–D3),故 guest 只需一个
**轻量 reaper**。它靠"一个二进制、两个模式"实现:`sandboxd` 以 **init 模式**当 PID 1(自研最小 init,tini
源码作正确性 checklist),re-exec 出 **transport 模式**子进程当 process/fs 终点(D4)。这条分进程拓扑换来
崩溃隔离、热升级、以及**两层探活**区分 transport-down 与 substrate 死亡(D5,规范结论在 §5.E.1 / §4.2 / §4.3)。

---

## D1. 作用域:引擎只跑一类"东西"

引擎要支持的"跑东西"只有一类:**经 `sandboxd` 的 `process` 能力按需拉起的进程**
(monitor / 构建 / `apt|npm|pip` 安装 / dev server / artifact HTTP server)。**不**以"开机即起的
系统守护服务"(`service`/`systemctl` 那套)为目标。判据干净:**是否需要 init 把它"开机拉起并常驻"**
——需要 → 移出范围;不需要、能被 exec 出来 → `process` + `publish` 两个现有原语即覆盖。

这条收缩把"需要完整 init 系统"的整类需求移出范围,guest 启动拓扑因此只需一个**轻量 reaper**
(D4)。

---

## D2. 两类进程的分界

| | 系统守护服务(不做) | 按需 exec 的进程(全部模型) |
| --- | --- | --- |
| 典型 | `service postgresql start` / `systemctl` | monitor、`apt install`、dev server、artifact server |
| 怎么起 | init 在启动时拉起、作为基础设施常驻 | 经 `sandboxd` 的 `process` 能力拉起 |
| 依赖 | SysV init 脚本 / systemd / dbus | 无;直接 fork 一个前台进程 |
| 引擎视角 | 需要 init 系统 + 启动编排 | 一个进程 +(可选)一个被 publish 的端口 |

---

## D3. 两个典型需求都落在"按需 exec"这一侧

### D3.1 `apt install`:一次性命令,不碰 init

安装工具链(Python / npm / Node / Rust 等)与部署长驻 DB 是两回事。`apt`/`npm`/`pip`/cargo 都是
**一次性命令**——exec、跑完就退,不是 `service xxx start` 的守护进程,**根本不碰 init**。默认进
sandbox 就是 root(引擎默认 guest root,§2.2),安装依赖的权限天然满足;guest 内 root 被 sentry 在用户态
拦截、逃不到宿主(spec §2.2),所以"放心让需要 root 的安装在里面跑"正是 gVisor 的姿势。唯一前置是网络:
`apt`/`npm` 出网走 spec §5.D 的出站默认放行(经 egress gateway)。

> postgres 之所以麻烦,不是 gVisor 的 syscall 兼容性(postgres-on-gVisor 能跑),而是"容器没有
> init 系统" + locale / 切用户等精简镜像通病。不以常驻 DB 服务为目标,这串坑整个不趟。

### D3.2 serve HTML / artifact:不需要新原语

artifact server 就是两步,全落在现有能力上:

```
process.Start("python -m http.server 8000")     ← spec §5.E process 能力
POST /v1/sandboxes/{id}/publish {port:8000}      ← spec §5.D 入站 publish(可带鉴权)
```

listen/accept + 读文件发响应都在 gVisor netstack 支持范围(吞吐不如 host 网络,serve artifact
够用)。引擎不知道"这是个 artifact server",只看见"一个进程 + 一个被 publish 的端口";"是
artifact"是产品层语义,不进引擎(spec §3.2.1)。

一个细节:artifact server **命可以长**(查看期间一直在),这与"按需"不冲突——"按需"指的是
**谁拉起、怎么拉起**(经 `process` 显式、可观测),不是寿命短。publish 生命周期本就独立于连接。

---

## D4. guest 启动拓扑:PID 1 用谁

两个正交问题:**Q1** init 语义谁实现(自研 vs 打包 tini)、**Q2** init 要不要和 `sandboxd` 同进程。

### D4.1 Q1:自研 init 语义,拿 tini 源码当正确性 checklist

**自研,不打包 tini 二进制;tini 源码当可执行规格逐条抄语义。** tini 是"最小 init 一堆坑"的权威
答案,checklist:

- `waitpid(-1, …, WNOHANG)` 收割循环 + `ECHILD` 处理;
- 信号转发(默认转给单个子进程,`-g` 转给整个进程组);
- **PID 1 无默认信号处置**——内核不给 PID 1 装默认 handler,不显式处理的信号(除
  `SIGKILL`/`SIGSTOP`)被静默丢弃,必须逐个显式装;
- 退出码 `128+signo` 传播、`SA_RESTART` 语义、subreaper 交互。

自研而非打包:对 spec §1.1「单二进制 + 配置 + 本地 store」的部署形态,**不往每个 base rootfs 塞一个
tini 二进制**是实打实的收益——少一个要版本化 / 追 CVE / 打进模板的活动件。代价是 PID 1 正确性的
失败模式脏(僵尸堆积、信号丢投、关机挂死),但这是**有界**问题(~300 行)、有 tini 兜底 checklist。

### D4.2 Q2:init 与 `sandboxd` 分进程,用"一个二进制两个模式"

init 与 `sandboxd` **不合并成同一个进程**,三条工程理由:

1. **崩溃爆炸半径 / 可重启性(最硬)**。`sandboxd` 是全系统最复杂、最会长、最会出 bug 的部分
   (wire 协议、流多路复用、pty、fs ops)。若它就是 PID 1,一崩**整个 guest 陪葬**——用户在跑的
   `apt` / dev server / build 全没。独立的 ~300 行 init 几乎永不变,能**原地重启 `sandboxd`** 而不
   导致沙箱崩溃,把 `sandboxd` 崩溃降级成**进程内可恢复**(对齐 spec §4.2「崩溃 = 非自愿的 pause」),
   而非升格成一次 substrate 死亡。
2. **热升级 `sandboxd`**。沙箱长命持久(idle→pause→resume 反复,spec §1.1),这寿命里会想换引擎
   版本、重启 transport 层而**不打扰用户长跑进程**。`sandboxd` == PID 1 做不到;作为 init 的子进程
   就能(init re-exec 新 `sandboxd`,老子进程 reparent 到 init 存活)。
3. **PID 1 信号语义对复杂 server 有敌意**(D4.1)。Go 运行时(多线程 + 自带 signal handling)叠在
   PID 1 特殊规则上要格外小心;让极小的 init 当 PID 1、`sandboxd` 当普通子进程,这问题整个绕开。

"reaper 要知道 `sandboxd` 拉起的进程,所以得放一起"这个合并动机被内核特性化解:
**`prctl(PR_SET_CHILD_SUBREAPER)`** 让 `sandboxd` 声明为 subreaper 后,它派生的进程树里的孤儿
reparent 到 `sandboxd` 而非 PID 1——不当 PID 1 也能收割自己关心的后代(它有 context)。PID 1 于是
只当**纯兜底**,收割真正的全局孤儿(在"只有按需进程"模型下这批极小)。

**落地形态:一个二进制两个模式**(init 模式 PID 1 经 `/proc/self/exe` re-exec 出 transport 模式子进程),
完整拓扑图见 spec §5.E.1。

### D4.3 transport 模式要落的三样机制,和一个要拒的模型

收割语义无处可抄——它必须靠 D4.1 自研 + tini checklist。但 transport 模式要落的另外三样机制是成熟的
进程管理套路,现在就锁定形状:

- **每进程监督状态机**:per-process 一个 `cmd.Wait()` 的 monitor goroutine + 状态枚举
  (running/stopped/killed/crashed)+ exit/start handler。
- **每进程 pgid 纪律**:非 PTY 进程 `Setpgid` 自成组、PTY 进程交给 pty 的 session——这条 tini 覆盖
  不到的 PTY vs 非 PTY 非对称,完整推理见 D4.5 C1。
- **drain-barrier**:pause/resume 前用一道 barrier 挡住 in-flight mutating RPC、等其排空——正是 D4.4
  协作式静止点、D5.2 单飞守卫要的原语。

**要拒的是"独立世代计数"identity 模型**(常见于分布式 init/supervisor:给每次运行挂一个
`epoch`/`generation` 标签辨识陈旧实例)。引擎不引入它,用 spec §3.3/§4.3 的 placement/IP-as-epoch
替代(spec §「引擎词汇边界」点名 `generation` 是最易被带回的概念)。禁词表拦得住词、拦不住概念
——drain-barrier 的**排空逻辑**要、任何往它上面挂 **generation 标签**的做法不要。

**信号目标天然分裂**:定向信号(单进程 / 进程组,spec §4.2 协作式静止点、§5.E 信号通道)归
**transport 模式**(它握 pid/pgid 句柄);容器级信号扇出(runsc 收 `SIGTERM` → 转发子进程)归 **PID 1**。

### D4.4 协作式静止点协议(spec §4.2 长跑进程原语的机制)

spec §4.2 把静止点按进程行为分两类原语。裸 pty 走抢占式(直接冻结,无待办);本节只展开**协作式**。
两个前提把协议收窄到很小:

- **协作式静止点只是"尽量不截断 in-flight 写"的优化,不是正确性前提**(spec §4.4):超时 → 降抢占 →
  最坏强杀,任一路径都落到合法 `paused`(与崩溃语义一致)。不值得为它引入重协议。
- **一致性分两层,只有一层要"协作"**。substrate 层的 flush 一致点(spec §5.B:runsc/gofer 把已
  提交内核的 in-flight 写落到 Overlay upperdir)本就保证,不依赖进程配合。协作式唯一多要的是**应用层
  安全点**——进程正写一个逻辑单元写到一半(如 agent 写 JSONL 写到半行),冻结会留半条。而应用层
  安全点通用地无法由 engine 判定:engine 不认识 agent(spec §3.2.1),不能要求进程为 pause 实现回调。

**协议因此两层,一层确定、一层尽力,都不引入 guest 内 ack 通道:**

- **① host 发起的 in-flight 写:drain-barrier(确定性)。** pause 编排先令 transport 模式进入 barrier
  ——停收新的 mutating process/fs RPC、等 in-flight 的排空,再交 §5.B flush。这层不需要 guest 配合,因为
  host 完全掌握自己发出的 RPC(复用 D4.3 的 drain-barrier 原语,只留 in-flight 计数 + 排空等待)。
- **② guest 内长跑进程:信号 + 观测 stopped + 超时降级(尽力)。** 发一个**可被 handle 的** stop
  请求(`SIGTSTP` 族,经 SendSignal 定向到进程组)——实现了 handler 的进程(如 agent 收到会 flush
  历史再停)受益,没实现的走默认 stop。engine **不等进程主动 ack**,而是**观测 `/proc` 进程状态
  到达 `T`(stopped)**——这是 engine 侧可观测的代理信号,不是 guest 进程回调,不违反"engine 不
  认识 agent"。到达 stopped 或超 grace → §5.B flush → runsc 冻结;超时则降 `SIGSTOP` 抢占。

**关键的"不做":不给长跑进程一个"我到安全点了"的主动上报通道。** 那要求进程为 pause 改代码
(违反 spec §3.2.1),且它非正确性前提,不值得。"到没到安全点"要么由进程自己在 `SIGTSTP` handler
里体现(它停了 = 它认为到了),要么由 engine 观测 `/proc` 状态代理——engine 不引入应用层握手。

参数级仍待(纯调参,不改两层结构,登记 open-decisions):grace 时长、可 handle 信号的具体选择、
是否给模板一个可选"可协作 flush"钩子(默认不需要)。

### D4.5 tini checklist 覆盖不到的三条缝

tini 的语义权威,前提是**它只有一个子进程、自己永不被重启**。而 transport 模式 `sandboxd` 是
**N 个子进程 + PTY + 会被 PID 1 原地重启**(D5.3)。三条 tini 覆盖不到、须本引擎自己锁定,都位于
"transport 模式握 pid/pgid 句柄"这一侧:

- **C1 — per-process pgid / session,PTY 非对称。** tini `-g` 只把信号转给它那唯一子进程的进程组;
  `sandboxd` 要的是**每次** `process.Start` 给进程单独 `setpgid` 成独立组,好让 `process.signal(pid)`
  定向打那一棵进程树。**但 PTY 特例反过来**:pty 已 `setsid` + 设控制终端做 job control,再 setpgid
  会与 pty 的 session 管理打架。故契约分叉——**非 PTY 进程自成 pgid,PTY 进程交给 pty 的 session**,
  定向信号对两者走不同路径。归 transport 模式,PID 1 不碰。

- **C2 — Wait / 退出码语义跨一次 transport 重启是 lossy。** transport 崩、PID 1 原地重启后:它作为
  subreaper 收养的子进程在它死的瞬间 reparent 到 PID 1(真 init),新 transport 丢了内存里的 pid/pgid
  句柄表、且那些进程现在挂在 PID 1 名下——新 transport `waitpid` 不到它们,退出状态被 PID 1 收走。
  故 `process.Wait` / exit-code **仅在 transport 单次存活期内保证**,跨一次重启退化为
  「`process.List`(扫 `/proc`)仍可见、Wait/exit-status 可能丢」。这与裸 pty 字节流的 lossy 同源:
  engine 无状态、可重启性不由 engine 承担,"不丢"担保由上层(session adapter)自理。**零新增机制**,
  不加 PID 1↔transport 的 exit-status 上报缝。
  > **留的 seam(将来若 session 层证明必须"不丢"再做,不回头改 engine)**:PID 1 收全局孤儿退出
  > 状态时旁路记一份供新 transport 查询(一条极窄的内部上报缝)。今天不做——列在这里是为锁定"加它
  > 不动 transport / engine 无状态契约",与 [`transport.md`](./transport.md) D3「守 seam」同纪律。

- **C3 — 收割权在 PID 1 与 transport 间的划分,含重启瞬态。** 正常态 transport 经
  `PR_SET_CHILD_SUBREAPER` 收自己后代、PID 1 只兜底全局孤儿;**瞬态**(transport 已死、新 transport
  未接手)划分翻转——后代短暂 reparent 到 PID 1、由 PID 1 收(正是 C2 的成因)。这条瞬态契约要显式
  写清,别默认"transport 永远在收自己后代"。

> C1(pgid 非对称)是成熟套路;C2/C3 只在**"独立 PID 1 + 监督重启"**拓扑下才出现(一个不当 PID 1、
> 不被监督重启的普通 HTTP 子进程遇不到它们),是本引擎这套拓扑的新增议程。

---

## D5. resume 冷启时序

载体(netns TCP,§5.E)、探活模型(§4.3)、transport 无状态、placement 发现 IP(§5.A)之上的编排。

> **create 与 resume 是同一条冷启链,只差前缀与失败出口。** ④⑤⑥⑦(拉起 runsc + readiness 门)对
> 两者同构;不同的只有:(a) 前缀——resume 重挂**已有** Overlay 工件,create 先建**空** Overlay +
> 首次绑定 Volume/Network 记录;(b) 失败出口——resume 有可退的静止态(→ `paused`,要点④),create 没有,
> 失败要清理半建资源(空 Overlay + placement)后删记录 / 进 `destroyed`(spec §4.4 不变量 A)。
> create 的就绪窗口 / 半死处置复用 ⑦ 的 Health RPC 门,超时 / 拉起失败落到自己的失败出口。下面以
> resume 为主线,create 差异随文标注。

### D5.1 canonical 冷启链(create / resume 共享;动词归 §4,机制归 §5,子系统不互调)

`resume` / `create`(生命周期动词,mediator)按序调用各子系统机制,拉起 → `running`:

```
① §5.B Rootfs   ： 挂 merged overlay（lower=Template / upper=Overlay 工件 / work）
                   〔create：先建空 upper/work；resume：重挂同一 Overlay 工件〕
② §5.C Volume   ： 按挂载表 Binding 备好 Volume（runsc 启动时从 OCI spec 取）
③ §5.D Network  ： 按策略 Binding 建/选 egress 数据面（gateway）→ 产出 ingress 描述符
                   （数据面行为归引擎、落位后置到 ④ 的引流；见 network.md D3.2/D4）
      └─ ①②③ 的产出由 §4 汇总交给 ④——子系统间不互调
④ §5.A Runtime  ： 重建 placement（netns/IP/cgroup 按资源上限 Binding + 按 ingress 描述符建 nft
                   引流，把 egress 全量逼进数据面）→ 以 sandboxd（init 模式）为 entrypoint 拉起 runsc
⑤ guest 内启动  ： PID1（init 模式）装 reaper+信号+监督 → 派生 transport 模式 sandboxd
                   → 子进程 PR_SET_CHILD_SUBREAPER → bind netns TCP → listening
⑥ §5.A placement： 发现容器当前 netns IP（veth/CNI result/inspect）
⑦ §4 探活门     ： 向 IP:port 发 Health RPC，轮询至首次成功 or 超时
      ├─ 首次成功 → state=running，发 resumed/created 事件（与状态提交单次原子，spec §4.4-C）→ API 返回
      └─ 超时/拉起失败 → 回收半建 placement 后：resume→留 paused 可重试；create→删记录/→destroyed
⑧ （engine 外） ： client 重新解析 placement、对当前 IP 重连 process/fs；session adapter
                   调 driver --resume（engine 不参与，spec §5.E）
```

**顺序约束**:①②③ 必须在 ④ 之前物化(挂载在 runsc 启动时从 OCI spec 取,运行中不可热改);
⑥ 需 netns 已存在(④ 之后);⑦ 需 IP 已知(⑥)**且**端口在听(⑤)——**⑦ 是"已拉起"与
"running"之间唯一的屏障**。**并发约束**:§4.4-B 转换锁**全程持有**这条链——①前拿锁、⑦翻 `running`
(或超时 unwind)才放,故整个 ①–⑦ 期间并发 `destroy`/`evict` 拿不到锁,不会去删正被挂载/冷启的
Overlay 工件。

### D5.2 设计要点

- **要点① — readiness = 首次 Health RPC 成功,复用 §4.3 探活探针,不另造 ready 协议。** 就绪信号不用
  guest→host 的 ready 推送、不用裸 TCP accept(accept 可能早于 RPC handler 接好、抢跑),而是
  engine 侧探针发一个应用层 `Health` RPC,首次成功即 `running`。readiness 与健康探测本是同一件事。
- **要点② — in-flight resume 期不做健康分类(单飞守卫)。** resume 是**每沙箱单飞**的 in-flight 操作,
  持有期间对该沙箱的健康分类挂起,直到 ⑦ 首次探活成功翻成 `running`(理由见 §4.3)。这是内存态操作守卫,
  不新增 §4.1 状态。它只挡"不走锁的分类器在就绪门前空跑/误报",**不承重并发正确性**——并发动词
  (destroy/evict)由 §4.4-B 全程持有的转换锁兜底(①–⑦ 期间它们拿不到锁)。
- **要点③ — resume API 在 running 时返回 = 调用方"可以连了"的信号。** engine 不向 client 推送就绪
  (无连接注册表、无回调)。持旧 IP 句柄的 client 连接失败即**重新解析当前 placement**(旧 IP 冷启
  即作废)——时序上只要求 client 不缓存旧 IP。
- **要点④ — 冷启失败原子退回源态,不加 failed 态(spec §4.4-A)。** 拉起失败 / 超时先回收半建 placement
  (netns/gateway/nft/overlay mount),持久态(Overlay 工件)完好在后端。出口按动词分:**resume** 退回
  `paused`(仍可恢复);**create** 无可退静止态,清理半建资源(含空 Overlay)后删记录 / 进 `destroyed`。
  两者都不引入补偿事务、不新增状态——失败只是"这次 placement 没建成",不是持久态损坏。
- **要点⑤ — PID1 内部就绪被 transport 端口就绪吸收。** 外部唯一可观测的 readiness 就是 transport 端口
  在听;PID1 装完 handler 才可能派生 transport 子进程(⑤),故探针门控的正是唯一要紧的事,PID1
  内部次序不必对外暴露握手。

### D5.3 探活失败分两类(监督拓扑的后果)

PID1 会**原地监督并重启** transport 子进程,于是 **transport 崩溃不再等于 substrate 死亡**:runsc
活而端口不通,是 transport 子进程崩了、PID1 正在同 IP 原地重启(瞬态,不走 resume,成本远低于整链
冷启);runsc 整个探不到才是 substrate 死亡(走 resume 换新 IP)。因此探活必须 **substrate +
transport 两层都探**才能区分二者——完整的两层探针定义、健康态分类与处置见 spec §4.3。这条区分正是
D4.2 分进程拓扑换来的附加能力。

---

## D6. 边界与仍待

- **协作式静止点(D4.4)** 仅剩参数级(grace 时长 / 信号选择 / 模板可选钩子),不改两层结构。
- **模板是否预置常驻进程**:目前不需要;若未来出现,平级托管,不进 `sandboxd`。
