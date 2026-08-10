# Guest 运行时边界:跑什么、谁当 PID 1、resume 怎么冷启

> **定位:[`§5.E.1`](../subsystems.md)(guest 启动拓扑)的深挖文档,并涉及 [`§4.2`](../model.md)(pause
> 静止点)、[`§4.3`](../model.md)(两层探活);导航见 [`SPEC.md`](../SPEC.md)。** 展开两块 spec 只留结论
> 的推理:① **作用域**——引擎只支持"按需 exec 出来的进程",不以系统守护服务为目标(D1、D3);
> ② **guest 启动拓扑 + pause 静止点 + resume 冷启时序**(D4、D5)。规范结论在 §5.E.1 / §4.2 / §4.3。
>
> **编号约定:** 本文自己的节用 `D` 前缀(D1、D4.4…);`§` 一律指 spec 的全局章节(§4.2 等),两套不混。
> **节号是稳定寻址锚,不是目录序号**:一经被引用就不再重排,故序列里的空号(D2 已并入 D1)是刻意保留、
> 不复用的——与 network.md D5 顶部同一条纪律。

---

## D0. 一句话结论

引擎只跑一类东西——**经 `process` 按需 exec 出来的进程**,不做系统守护服务(D1、D3),故 guest 只需一个
**轻量 reaper**。它靠"一个二进制、两个模式"实现:`sandboxd` 以 **init 模式**当 PID 1(自研最小 init,tini
源码作正确性 checklist),re-exec 出 **transport 模式**子进程当 process/fs 终点(D4)。这条分进程拓扑换来
崩溃隔离,以及**两层探活**区分 transport-down 与 substrate 死亡(D5)。

---

## D1. 作用域:引擎只跑一类"东西"

引擎要支持的"跑东西"只有一类:**经 `sandboxd` 的 `process` 能力按需拉起的进程**
(monitor / 构建 / `apt|npm|pip` 安装 / dev server / artifact HTTP server)。**不**以"开机即起的
系统守护服务"(`service`/`systemctl` 那套)为目标。判据干净:**是否需要 init 把它"开机拉起并常驻"**
——需要 → 移出范围;不需要、能被 exec 出来 → `process` + `publish` 两个现有原语即覆盖。

判据落在两类东西上:

| | 系统守护服务(不做) | 按需 exec 的进程(全部模型) |
| --- | --- | --- |
| 典型 | `service postgresql start` / `systemctl` | monitor、`apt install`、dev server、artifact server |
| 怎么起 | init 在启动时拉起、作为基础设施常驻 | 经 `sandboxd` 的 `process` 能力拉起 |

---

## D3. 两个典型需求都落在"按需 exec"这一侧

### D3.1 `apt install`:一次性命令,不碰 init

安装工具链(Python / npm / Node / Rust 等)与部署长驻 DB 是两回事。`apt`/`npm`/`pip`/cargo 都是
**一次性命令**——exec、跑完就退,不是 `service xxx start` 的守护进程,**根本不碰 init**。默认进
sandbox 就是 root(spec §2.2:引擎默认 guest root、**不建模 guest 用户**,「以某用户运行」是 guest 原生
的 `su`/`sudo`),安装依赖的权限天然满足;而 guest 内 root 被 sentry 在用户态拦截、逃不到宿主,所以
"放心让需要 root 的安装在里面跑"正是 gVisor 的姿势。唯一前置是网络:
`apt`/`npm` 出网靠 §5.D 声明里的出站放行(`egress.default: allow`,或一条覆盖上游的 `allow[]`)。

> postgres 之所以麻烦,不是 gVisor 的 syscall 兼容性(postgres-on-gVisor 能跑),而是"容器没有
> init 系统" + locale / 切用户等精简镜像通病。不以常驻 DB 服务为目标,这串坑整个不趟。

### D3.2 serve HTML / artifact:不需要新原语

artifact server 就是两步,全落在现有能力上:

```
process.Start("python -m http.server 8000")     ← spec §5.E process 能力
POST /v1/sandboxes/{id}/publish {guest_port:8000, proto:"tcp"}   ← spec §5.D 入站 publish(引擎只交付 host ip:port,声明里无鉴权字段)
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

init 与 `sandboxd` **不合并成同一个进程**,两条工程理由:

1. **崩溃爆炸半径 / 可重启性(最硬)**。`sandboxd` 是全系统最复杂、最会长、最会出 bug 的部分
   (wire 协议、流多路复用、pty、fs ops)。若它就是 PID 1,一崩**整个 guest 陪葬**——用户在跑的
   `apt` / dev server / build 全没。独立的 ~300 行 init 几乎永不变,能**原地重启 `sandboxd`** 而不
   导致沙箱崩溃,把 `sandboxd` 崩溃降级成**进程内可恢复**(对齐 spec §4.2「崩溃 = 非自愿的 pause」),
   而非升格成一次 substrate 死亡。**这道重启只是崩溃恢复、不是换版本的通路**:`/proc/self/exe` 是
   PID 1 已打开的那个 inode 的句柄、不在 exec 时重解析路径,故监督循环每次拉起的都是**同一份字节**
   ——换版本只有"新 Template + 新 placement"这一条路。
2. **PID 1 信号语义对复杂 server 有敌意**(D4.1)。Go 运行时(多线程 + 自带 signal handling)叠在
   PID 1 特殊规则上要格外小心;让极小的 init 当 PID 1、`sandboxd` 当普通子进程,这问题整个绕开。

"reaper 要知道 `sandboxd` 拉起的进程,所以得放一起"这个合并动机被内核特性化解:
**`prctl(PR_SET_CHILD_SUBREAPER)`** 让 `sandboxd` 声明为 subreaper 后,它派生的进程树里的孤儿
reparent 到 `sandboxd` 而非 PID 1——不当 PID 1 也能收割自己关心的后代(它有 context)。PID 1 于是
只当**纯兜底**,收割真正的全局孤儿(在"只有按需进程"模型下这批极小)。

**落地形态:一个二进制两个模式**(init 模式 PID 1 经 `/proc/self/exe` re-exec 出 transport 模式子进程),
完整拓扑图见 spec §5.E.1。

### D4.3 transport 模式要落的机制、信号目标的分工,与两个要拒的形状

收割语义无处可抄——它必须靠 D4.1 自研 + tini checklist。但 transport 模式要落的另外三样机制是成熟的
进程管理套路,形状如下:

- **每进程监督**:per-process 一个 monitor goroutine,退出即广播 `end` 帧(退出码 `128+signo` on
  signal)。若将来要向调用方暴露更细的进程状态(running/stopped/killed/crashed),那是这个 monitor
  之上的 additive 加法,不改本拓扑。
  - **但它的状态不能来自 `cmd.Wait()`——那与 subreaper 职责不可共存,二者只能有一个赢。**
    transport 模式按 §5.E.1 声明 `PR_SET_CHILD_SUBREAPER` **并因此必须自己跑 `Wait4(-1)`**,而
    `Wait4(-1)` **无法排除某个 pid**:通配 waiter 与 per-child `cmd.Wait()` 竞争同一个状态,谁先收到谁
    拿走。**实测:通配 waiter 先到时 `cmd.Wait()` 10/10 次返回 `waitid: no child processes`**——于是每一
    个 `end` 帧的退出码都会是**编造**的,而退出码送达是 §5.E.1 明写的承重契约。故形状是:**单一通配
    收割器 + 一张交接表**(pid → 等待者),monitor 从表里领自己那份状态,与 PID 1 的收割器**同一份机制**
    (`reapTable`,两个模式共用,§5.E.1;它正是为"通配 waiter 不得偷走已注册子进程的状态"而设计的)。(『声明』与
    『装上通配 drain』不可分开落地,§5.E.1。)
  - **连带一条不显然的义务**:跳过 `cmd.Wait()` 也就跳过了它对 `StdoutPipe`/`StderrPipe` 父端的关闭,
    故长命 daemon 里每个进程会漏 2 个 fd——输出泵必须在 EOF 时自己关。(PTY master 例外:它同时是输入
    端,由进程终结路径统一关。)
- **每进程 pgid 纪律**:非 PTY 进程 `Setpgid` 自成组、PTY 进程交给 pty 的 session——这条 tini 覆盖
  不到的 PTY vs 非 PTY 非对称,完整推理见 D4.5 C1。
- **drain-barrier**:pause 前用一道 barrier 挡住 in-flight mutating 写、等其排空——D4.4 那一层 host 侧
  drain-barrier 要的原语(D5.2 的单飞守卫在 host 侧、不用它)。

**要拒的是"独立世代计数"identity 模型**(常见于分布式 init/supervisor:给每次运行挂一个
`epoch`/`generation` 标签辨识陈旧实例)。引擎不引入它,用 spec §3.3/§4.3 的 placement/IP-as-epoch
替代(spec §「引擎词汇边界」点名 `generation` 是最易被带回的概念)。禁词表拦得住词、拦不住概念
——drain-barrier 的**排空逻辑**要、任何往它上面挂 **generation 标签**的做法不要。

**信号目标天然分裂**:定向信号(单进程 / 进程组,spec §4.2 静止点、§5.E 信号通道)归
**transport 模式**(它握 pid/pgid 句柄);容器级信号扇出归 **PID 1**——它 `kill(-1, sig)` 广播给
**本 pid namespace 内除自己以外的全部进程**,故模板自带的长跑 sibling 与 transport 的整棵子树一并覆盖,
不只是它自己 spawn 的那一个子进程。这条判据是"不指定任何 pid/pgid":正因为不定向,它才落在 PID 1 侧
而非 transport 侧——句柄留给定向那条路。

**这条判据在 `SendSignal` 入口是可执行的,不只是分工描述:`pid <= 0` 判 `invalid_argument`。**
POSIX 里 `pid <= 0` **就是**扇出形式(`kill(0)` 打整个进程组、`kill(-1)` 打整个 pid namespace),
故它不是"危险取值",是**投错了子系统**——那条路归 PID 1,transport 侧没有它的入口。校验落在
**guest 侧**(`cmd/sandboxd`),因为 §5.E.1 的前提是 guest 能写自己的 fs、必须自证;orchestrator 侧
同形一道只作 fail-fast(少一次 RPC),不是权威。类型保持 `int32`:`uint32` 会让 wire 上的负值静默变成
大正数,把一个显式 400 换成一条更难查的行为。

它**只校验 pid > 0**,不查进程注册表——`Connect`/`SendInput`/`Update`/`CloseStdin` 必须查是因为要摸
per-process 状态,而发信号只需要一个合法 pid;要求注册表命中会打断 §6.1 明写的
「`List` 拿 pid → `SendSignal` 打它」配对(`List` 实时扫 `/proc`,返回的 pid 本就从未进过那张表)。

**校验拦住的是什么——三个取值,已在 gVisor pid namespace 内实测(`SIGTERM`,绕过校验直发)。**
`killProcessGroup` 的两级回退(先 `kill(-pid)`、失败再 `kill(pid)`)在 `pid <= 0` 上两臂都转为敌对:

| 入参 | 实际发生 | 后果 |
| --- | --- | --- |
| `pid=-1` | `kill(-(-1))` = **定向** `kill(1)`,命中 PID 1 自己的 TERM handler | 沙箱被拆(RPC 收 `unexpected EOF`、Health 再不回来) |
| `pid=0` | `kill(-0)` = `kill(0)`,打 sandboxd 自己的进程组——而 transport 的 `pgrp` 与 PID 1 同为 **1**(它由 PID 1 fork、不 `Setpgid`),故 PID 1 在组内 | 同上,沙箱被拆 |
| `pid=1` | `kill(-1)` 命名空间广播。Linux `kill(-1)` **排除 PID 1 与调用者自身**,故沙箱存活,但**每个工作负载进程被连带杀死**(实测两个无关进程同时 `exit 143`);guest 内一个工作负载都没有时该发返 `ESRCH`,于是落到回退臂 `kill(1)`,变成 `pid=-1` 那一行——沙箱被拆 | 越权扇出;空 guest 下退化为拆沙箱 |

§6.1 假定调用方已被信任、引擎不做授权,故要补的是**输入校验**,不是授权。

**而 `pid=1` 那一行的扇出被有意放行:校验只拦 `pid <= 0`。** 上表把机制与后果都写全了(命名空间广播打掉
每个工作负载进程;空 guest 下 `ESRCH` 落到回退臂、退化成拆沙箱)。两条理由:① **`pid > 1` 会打断 §6.1 明写的 `List` → `SendSignal` 配对** —— `List` 实时扫 `/proc`,必然返回
PID 1,于是 API 会吐出一个自己拒绝接受的 pid,那是比扇出更难解释的契约缺陷(同上一段"不查注册表"那条判据的
延伸:那里拒的是注册表命中要求,这里拒的是把同一个配对从取值一侧掐断)。② 放行的依据是 **PID 1 是 guest
原生可寻址对象**——上面三行里 `pid=-1` / `pid=0` 是**投错子系统**的扇出形式(POSIX 语义,transport 侧没有
它们的入口),而 `1` 是一个合法的定向 pid,它的扇出是 Linux `kill(-1)` 语义的后果、由**调用方自担**。

**不做沙箱级宽限期——已否决。** PID 1 收 TERM 后广播、**紧接 `os.Exit(0)`**,而 PID 1 退出即拆
pid namespace,故那一发 TERM 事实上不可被处理、与直接 KILL 无差别。**要拒的不是"没实现宽限",而是这个
形状本身对不上:** ① 需要宽限的是**工作负载进程**——它才有要 flush 的东西,而进程级定向信号已经存在
(就是上面那条 `SendSignal`),调用方要优雅关闭什么,它自己知道该打哪个 pid;② 沙箱级 TERM 想"优雅"
就得让引擎判断"guest 里哪些进程值得等、等多久",那是**产品层策略**,§1.3 litmus 要挡在引擎外面
(engine 不认识 agent,spec §3.2.1)。故 §4 的动词表保持 pause/destroy 两个,destroy 即刻生效。
外部证据方向一致:e2b 的 `DELETE /sandboxes/{id}` summary 字面是 "Kill sandbox"、无宽限字段;
sandbox0 沙箱 PodSpec 根本没设 `TerminationGracePeriodSeconds`(吃 kubelet 隐式 30s,一个没人为它论证过
的值),而它**真正做过决定**的宽限期全在进程/会话粒度、宿主侧一个都没有。

### D4.4 pause 静止点协议(spec §4.2 的机制)

形状与两个前提在 §4.2:协议**只有一层 host 侧 drain-barrier**——停收新的 mutating fs 写、等 in-flight
排空,再交 §5.B flush(复用 D4.3 那个原语)。本节只补 §4.2 没有的三样:一个实测、一张不进 barrier 的
清单、以及协作式冻结为何被否决。

**已实测**:让一次 64 MiB 的 `fs.Write` 与 `Pause` 对撞、并等到写确实进入 guest 后才发 pause,关掉
barrier 时出现被截断的文件(10 轮里有一轮只落了 36.7 MB),开启后 10 轮全部完整落盘。

**闸门只挡"写",不挡"读"**:`Read`/`Stat`/`List`、`process.List`、`Connect` 一律不进 barrier——它们
截断不了任何东西,而在 pause 进行中挡住它们,恰好是在运维最想看一眼的时刻把观测面停掉。`process.Start`
/ `SendInput` / `SendSignal` 同样**不挡**:它们改的是 guest 状态,不经由"引擎发出的、带 truncate 窗口的
写";且 `Start` 是长流,在它上面等排空会让 barrier 自锁——排空在等一条要等排空才结束的流。

**不做 guest 内的协作式冻结(`SIGTSTP` + 观测 `/proc` 到 `T` + 超 grace 降 `SIGSTOP`)——已否决。**
两条理由:① 它要求负载**自愿**在信号里停到安全点,而 engine 既不能要求、也无法验证(不认识 agent,
spec §3.2.1);② 两个成熟实现都选了**不可屏蔽**的冻结(一个 `SIGSTOP` 进程组、一个 cgroup freezer),
无一提供应用层安全点协议——证据方向一致。**反向的通道(进程主动上报"我到安全点了")同样否决**,理由同
①:那要求长跑进程为 pause 改自己的代码(违反 spec §3.2.1)。故应用层安全点**不进引擎**、engine 不引入
应用层握手:它落在 §4.4「协作只是优化、不是正确性前提」那一侧,超时/强杀的语义与崩溃一致,而崩溃本就是
§4.2 接受的底线。

**超时即抢占,不是让 pause 失败。** §4.4 把这一层放在"优化"一侧,故每条路径都必须落到合法的
`paused`:一条卡住的 RPC 不能让沙箱变成不可 pause。超时只记下还没排空的操作(**按名字**,"还有 3 条"
对读的人毫无用处)然后照常冻结——那个落点正是崩溃的落点,而崩溃是 §4.2 已接受的底线。(一个成熟实现
在超时时**放弃 pause**;在这里那个取舍是错的,理由同上。)

**crash-pause 不排空**:容器已经死了,没有 in-flight 可等、也没有东西还能被截断,等只会推迟记录一件
已经观测到的死亡。voluntary 的三条(用户 `pause`、`auto-pause`、Promote 的静止)才排空。

参数级残留(纯调参,不改这一层的结构):drain 的 grace 时长——它等的是单条 fs RPC,健康的毫秒级完成,
还挂着的是卡住而非慢。

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
  故**退出码(`end` 事件)仅在 transport 单次存活期内保证**,跨一次重启退化为
  「`process.List`(扫 `/proc`)仍可见、退出状态可能丢」。这与裸 pty 字节流的 lossy 同源:
  engine 无状态、可重启性不由 engine 承担,"不丢"担保由上层(session adapter)自理。**零新增机制**,
  不加 PID 1↔transport 的 exit-status 上报缝。

  **同一存活期内那条边界(pid 退出即不可寻址,§5.E.1)的落法只有一个**:要在退出后仍可寻址,前提是换成
  永不复用的不透明 ID——那会连带要求一套分配器 + 回收策略,且 handle 的语义从"这个进程"变成"这个可被重启
  的位置"(故还需一个 attempt/世代号),即一个 restart supervisor。§5.E 不做这件事,故要"不丢"就在
  engine 外接住 `end` 帧。
  > **留的 seam(若 session 层证明必须"不丢"才做,不回头改 engine)**:PID 1 收全局孤儿退出
  > 状态时旁路记一份供新 transport 查询(一条极窄的内部上报缝)。列出它是为锁定"加它不动 transport /
  > engine 无状态契约",与 [`transport.md`](./transport.md) D3「守 seam」同纪律。

- **C3 — 收割权在 PID 1 与 transport 间的划分,含重启瞬态。** 正常态 transport 经
  `PR_SET_CHILD_SUBREAPER` 收自己后代、PID 1 只兜底全局孤儿;**瞬态**(transport 已死、新 transport
  未接手)划分翻转——后代短暂 reparent 到 PID 1、由 PID 1 收(正是 C2 的成因)。故"transport 永远在收
  自己后代"不成立。

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
⓪ §4 写入 regime ： 读挂载表；若本次会给某卷 rw 挂载而该卷正有直连变更在飞 → 拒（§5.C；退回源态。
                   门在最前，故一次拒绝什么都没建）
① §5.B Rootfs   ： 挂 merged overlay（lower=Template / upper=Overlay 工件 / work）
                   〔create：先建空 upper/work；resume：重挂同一 Overlay 工件〕
② §5.C Volume   ： 按挂载表 Binding 逐个 Materialize，句柄组进 LaunchSpec.Mounts
                   （runsc 启动时从 OCI spec 取，故必须在 ④ 之前）
③ §5.D Network  ： 按声明 Binding 生成规则集（纯函数：放行集 + L0 地板 + publish 端点 → 规则文本）
      └─ ①②③ 的产出由 §4 汇总交给 ④——子系统间不互调
④ §5.A Runtime  ： 重建 placement（netns/IP/cgroup 按资源上限 Binding；建 netns/veth 后、拉起前
                   装 ③ 的规则集，谓词绑宿主端 veth 的 `iifname`（按名字；`iif` 按 ifindex，
                   veth 同名重建后静默 fail-open，§5.A）——§2.4 原子性义务）
                   → 以 sandboxd（init 模式）为 entrypoint 拉起 runsc
⑤ guest 内启动  ： PID1（init 模式）装 reaper+信号+监督 → 派生 transport 模式 sandboxd
                   → 子进程 PR_SET_CHILD_SUBREAPER → bind netns TCP → listening
⑥ §5.A placement： 把 transport 端点定为 guest netns IP:port —— 不是一次 boot 后探测：launch
                   路径直接用引擎自己那次分配；Adopt 路径读宿主端 veth 的 /30 再 +1（guest 侧
                   地址已被 sentry netstack 收走、读不回来，§5.A）
⑦ §4 探活门     ： 向 IP:port 发 Health RPC，轮询至首次成功 or 超时
      ├─ 首次成功 → state=running，发 resumed/created 事件（与状态提交单次原子，spec §4.4-C）→ API 返回
      └─ 超时/拉起失败 → 回收半建 placement 后：resume→留 paused 可重试；create→删记录/→destroyed
⑧ （engine 外） ： client 重新解析 placement、对当前 IP 重连 process/fs；session adapter
                   调 driver --resume（engine 不参与，spec §5.E）
```

**顺序约束**:⓪ 必须在 ① 之前(一次被拒的冷启不该已经挂上 overlay);①②③ 必须在 ④ 之前物化(挂载在
runsc 启动时从 OCI spec 取,运行中不可热改);
⑥ 需 netns 已存在(④ 之后);⑦ 需 IP 已知(⑥)**且**端口在听(⑤)——**⑦ 是"已拉起"与
"running"之间唯一的屏障**。**并发约束**:§4.4-B 转换锁**全程持有**这条链——①前拿锁、⑦翻 `running`
(或超时 unwind)才放,故整个 ①–⑦ 期间并发 `destroy`/`evict` 拿不到锁,不会去删正被挂载/冷启的
Overlay 工件。

### D5.2 设计要点

- **要点① — readiness = 首次 `Health` RPC 成功,复用 §4.3 探活探针,不另造 ready 协议**(裸 TCP accept
  可能早于 RPC handler 接好、抢跑;readiness 与健康探测本是同一件事)。
- **要点② — in-flight resume 期不做健康分类(单飞守卫,§4.3)。** 落地形状(「非阻塞试锁 + 跳过」、它挡
  什么、为何不承重并发正确性)见 [`reaper-health.md`](./reaper-health.md) D3。
- **要点③ — resume API 在 running 时返回 = 调用方"可以连了"的信号。** engine 不向 client 推送就绪
  (无连接注册表、无回调)。持旧 IP 句柄的 client 连接失败即**重新解析当前 placement**(旧 IP 冷启
  即作废)——时序上只要求 client 不缓存旧 IP。
- **要点④ — 冷启失败原子退回源态(⑦ 的失败臂),不加 failed 态、不引入补偿事务**(spec §4.4-A):失败只是
  "这次 placement 没建成",持久态完好在后端。
- **要点⑤ — PID1 内部就绪被 transport 端口就绪吸收**:PID1 装完 handler 才可能派生 transport 子进程(⑤),
  故 ⑦ 那道门已门控唯一对外可观测的 readiness,PID1 内部次序不必暴露握手。

### D5.3 探活失败分两类(监督拓扑的后果)

PID1 会**原地监督并重启** transport 子进程,于是 **transport 崩溃不等于 substrate 死亡**——两层探针的
定义、健康态分类与处置全在 spec §4.3。这条区分正是 D4.2 分进程拓扑换来的附加能力:少了独立 PID 1,
"端口不通"就只剩一种解释。

---

## D6. 边界

- **pause 静止点**:结构已定(单层 drain-barrier,D4.4);**应用层协作安全点已否决**,不是待办。grace
  时长是数值旋钮,家在代码里(`internal/lifecycle` 的 `defaultDrainGrace`),按 transport D5.4 的同一条
  纪律**不登记为开放决策**——一个旋钮有两个家就会有两个值。
- **模板是否预置常驻进程**:不需要;若出现,平级托管,不进 `sandboxd`。**内置 Template 的内容清单**是
  产品/模板层选型,仍开放,登记在 [`open-decisions.md`](../open-decisions.md)(该行指回 D3.1)。
