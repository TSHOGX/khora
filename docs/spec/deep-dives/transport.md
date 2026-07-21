# Transport 深挖:两服务地基(process / fs)+ session 语义加法

> **定位:[`§5.E`](../subsystems.md)(spec,导航见 [`SPEC.md`](../SPEC.md))的传输层深挖文档。** spec 只明确 §5.E 的边界
> (engine 只暴露 process / fs 两个无状态服务,session 语义在 engine 外);本文展开这条边界背后的
> **选型理由、ACP 会话映射、要守住的 seam**,并给出 **wire 协议(D4)** 与边界议程(D5)。
>
> **设计参照:** E2B `envd`(公开的沙箱守护进程,两服务收敛的先例)与 ACP(Zed 的 Agent Client
> Protocol,coding agent 会话协议)。二者是公开可查的形态参照,不是本仓依赖;下文只取其**设计取向**,
> 结论在此自洽陈述。

---

## D0. 一句话结论

传输用 **Connect-RPC**(HTTP-native,流式)。**engine 只实现两个服务**,它们构成自足的地基
——等价于一个 E2B 式沙箱:

| 服务 | 性质 | 在 engine 内? | 谁来用 |
| --- | --- | --- | --- |
| **`process`** | 无状态机制原语:起进程 + 可选 PTY + 双向流 + signal | ✅ engine 内 | 任何要在沙箱里跑东西的人 |
| **`fs`** | 无状态机制原语:read(offset)/write/list/stat | ✅ engine 内 | 任何要读写沙箱文件的人 |

**`session`(agent 会话语义)不在 engine 内实现**,而是**叠在 `process` 之上的一层语义加法**,
位于 engine 之外(adapter / 上层组件)。形态对齐 ACP 的 `session/*`,但它的实现、状态、审计
**全部在 engine 外**;engine 核心对"agent / turn / session"一无所知。

| 层 | 是什么 | 何时做 | 在哪 |
| --- | --- | --- | --- |
| **engine 地基** | `process` + `fs`,自足 = E2B 式沙箱 | **现在** | engine 内 |
| **session 加法** | ACP 形态的 agent 会话协议(prompt/update/load/resume) | **以后**,纯加法 | engine 外 |

**核心纪律(要守死的一条)**:session 必须能作为**纯加法**叠在 `process` 上——加它**不改 engine
核心**(对齐 `spec` §1.3「成本判据」)。本文为此预留 seam(D3),但现阶段**不为 session 在
engine 里买任何单**。

---

## D1. 为什么 engine 只做这两个,session 划到外面

设计压力来自三方:E2B(机制怎么收敛)、ACP(agent 会话怎么标准化)、渲染 vs 交互的非对称。
三者交汇的结论是:**process/fs 是 engine 的全部,session 是 engine 外的加法。**

### D1.1 `process` + `fs`:抄 E2B,因为"在沙箱里干活"本就只有两类,且自足

E2B 的 envd 守护进程只暴露两个 service:**Filesystem** 和 **Process**(PTY 并入 Process,
不单独成 service)。理由是:沙箱里能做的一切,要么动文件系统、要么跑一个进程。把进程原语
做足(可选 PTY + 双向流 + signal),则 **shell、命令执行、后台服务、jupyter kernel 全是
"起一个进程"的特例**——不必为每种形态各开接口。

据此,两个能力都定为 **Connect-RPC 服务**:

- **`process`**:`Start`(带可选 `pty.size`)/ `Connect`(重连已有进程)/ `SendInput` /
  `SendSignal` / `List`。PTY 不是独立服务,是 `Start` 带 PTY 字段的特例——resize = 更新
  size,键盘输入 = input 的 pty 分支。**进程按 pid 寻址**(guest 原生;tag 不进模型见 D4.6)。
- **`fs`**:`Read(path, offset, len)` / `Write` / `List` / `Stat(size, mtime)` /
  `MakeDir` / `Remove` / `Move`。**不提供 `watch`**,消费方用 `stat`+`read(offset)` 自行轮询拉增量
  (理由见 D4.4)。大文件走 HTTP multipart 旁路,不挤 RPC。

这两个服务**无状态**:engine 不为它们维护任何持久 SoT。"沙箱上现在跑着什么进程" =
向 sandboxd 实时 `process.List`,不存控制面记录。这是 E2B 同款——零进程注册表。

**关键:这两个就够成一个完整产品。** process + fs = 一个持久、可 root、可挂多连接、可读写
文件、可跑任意进程的隔离沙箱,即 `spec` §1.2 in-scope 的全部地基。它**不依赖 session 存在**
就能独立交付、独立使用。session 是它之上的可选语义,不是它的一部分。

### D1.2 `session`:形态抄 ACP,但它是 engine 外的加法,不是 engine 的职责

如果只有 process/fs,agent 就是"起一个进程读它的流"。这对 shell 完全够用;对 coding agent
缺三样东西——**turn 语义、会话历史与恢复、结构化事件**。ACP(Zed 的 Agent Client Protocol,
"coding agent 界的 LSP")把这三样标准化了。但**关键问题是:这三样该由谁提供?**

答案是 **engine 之外**。理由有三,层层递进:

1. **它们都能在 process/fs 之上拼出来,不需要 engine 原语。** turn 循环 = adapter 在 process
   流上识别 prompt/输出/结束;会话历史 = agent 本就往自己 FS 写(`~/.claude` 的 JSONL,落在
   Overlay 工件里,经 `fs` 可读);恢复 = adapter 调 agent 原生的 `--resume`。**engine 一个新
   概念都不用加。**
2. **它们是产品语义,塞进 engine 会污染地基(`spec` §1.3)。** "turn""session""哪个 agent"
   都是产品层概念;让 engine 认识它们,就是把产品概念泄漏进沙箱引擎——被第一性原则否决。
3. **ACP 自己就把 agent 当 engine 外的对等体。** ACP 里 agent 是 client 起的子进程,通过
   `session/*` 对话——agent 和"跑它的环境"是分离的两层。本项目把这层映射成:**engine 提供环境
   (process/fs),session adapter 在外面把具体 agent 翻译成 ACP 形态的会话协议。**

所以 `session` = **ACP `session/*` 方法族**(`new` / `prompt` / `update` 流 / `cancel` /
`load` / `resume` / `stopReason`),**由 engine 外的 adapter 实现**。`control` payload 属这一层:
它**不是 engine 的一种 payload 类型,而是 engine 外 session 层的协议**,对齐 ACP,engine 不持有它。

ACP 验证的三条判断(都指向"turn 不进 engine 控制面"):

- **turn 状态归 agent 进程**:ACP 无控制面,turn 是 agent 进程内的活动,对端只是观察者。
- **会话历史归 agent 持有**:`sessionId` 由 agent 侧生成,历史用 driver-native 机制存。
- **跨重启恢复是能力声明**:`session/load`(重放历史)vs `session/resume`(不重放、只恢复
  context),各由 capability 门控,agent 不支持就明说——**"重启行为不确定"是显式能力位,
  不是 engine 用 lease/grace 去保证的东西**。

这三条在新模型里**天然成立**,因为 session 根本不在 engine 里——engine 没有 turn 表可仲裁,
也没有会话内容可持有。session 层的状态(sessionId↔driver 映射、恢复能力、审计事件流)**全在
engine 外那层自己管**。

### D1.3 engine 侧只有 process / fs:没有 lane / payload 注册表 / turn 仲裁

两个无状态服务(D1.1)+ session 外移(D1.2)之后,engine 侧不需要任何统一传输原语、payload
类型系统或 turn 仲裁。逐条锁定它们各自的正当替代,防止移植代码时被带回:

- **不需要统一传输原语(lane)**:一个"什么都管"的传输抽象要承担的五件事——多路复用、每连接定序、
  可重启、控制面注册、checkpoint quiesce——前四件由 **Connect-RPC / HTTP-2 原生多 stream + 每
  stream 天生有序 + checkpoint 接管可重启 + process/fs 无状态**逐件吸收;只剩 checkpoint quiesce
  是真需求,由 engine 的**进程级冻结原语**(D4)单独提供。没有留给"统一 lane"的职责。
- **不需要 payload 类型系统**:jupyter = 起个 kernel 进程读流、file = fs 原语、cli = 起个进程——
  全在 process/fs 上拼得出来;`control`(agent 会话)划到 engine 外(D1.2)。故 engine 侧只有
  process / fs 两个通用机制,**新增任何交互形态 = engine 外叠一层,engine 一行不改**。
- **不做 turn 仲裁**:turn / session 整体在 engine 外,engine 控制面**不存** turn/lease/owner/
  游标/历史。engine 侧唯一与"可靠性"相关的承诺压到最低:process 双向流是尽力而为的在线流,
  中断时 in-flight 数据可能丢——裸 pty 字节流容忍丢字符(shell 无持久 SoT),agent 会话的审计幂等由
  engine 外 session 层自己保证。

> **这正是 `spec` §1.5 立足点④的精确含义。** ④"开放交互模型"不是"payload registry 可插拔",
> 而是"**engine 只两个通用原语(process/fs),开放性全在 engine 外 adapter**"——不是放弃开放性,
> 而是把它放到正确的层:engine 小而稳,任何东西都能在 process/fs 上叠。

---

## D2. 渲染 vs 交互:非对称,决定了 fs 不做 watch

"看一群 agent 在干嘛"(渲染)和"驱动一个 agent"(交互)是**两件不对称的事**,不该塞进同一原语:

| | **读 / 渲染** | **写 / 交互** |
| --- | --- | --- |
| 语义 | 旁观、回放、看状态 | 驱动一个正在跑的进程 |
| 形态 | fan-out、多对一、pull | 点对点、duplex |
| 引擎角色 | 通用 `fs`(read/stat,内容不透明) | `process`(duplex + signal) |
| 要不要连接 | **不一定**(可完全不开流,只轮询 fs) | **要——这条流就是 process 交互** |

- **渲染面**:engine 只保证"能读到字节 + 读到大小"(`fs.Read(offset)` + `fs.Stat`),
  **不提供 watch**。要不要 push、隔多久 poll,是消费方(engine 外组件)自己的策略;变更推送
  (inotify 式)是这之上的优化,不进 engine(理由见 D4.4),而轮询 `stat` 在 run-anywhere 下铁定可用。
- **交互面**:只有要**往活进程写**(给 shell 发键、给 agent 投消息)并拿实时双向回流时,
  才需要 `process` 的双向流。纯看历史/看状态,一条流都不用开。

一个推论:**engine 对内容一无所知。** agent 自带的会话日志(JSONL)天然落在 Overlay 工件里,
于是"持久化 / `--resume` / 前端读取(轮询 read+stat)"复用同一份文件,engine **不需再造**
"会话日志"机制或变更推送。还原 sidechain 树、画 swarm 拓扑——全在 engine 外/产品层。

> **PTY 保持最简,不落 scrollback 文件。** PTY 是 ephemeral 字节流,输出走 process 流即可;
> 为凑齐"读文件即渲染"的对称性而给它人造落盘 SoT,是给最简单的东西加复杂度。两条路径各自最简、
> 互不拉平:会写日志的 agent 有自带落盘会话可渲染(SoT 是它自己的文件),裸 pty shell 只有在线
> 流、无落盘 SoT。这忠实反映两种东西的语义差异,engine 不替任一方发明它本不具备的语义。

---

## D3. 要守住的 seam:保证 session 是纯加法

现在不实现 session,但要保证将来加它**不回头改 engine**。需要 engine 现在就守住三个 seam:

- **process 的双向流足够通用**:能承载任意字节/帧,使 session adapter 可在其上跑 ACP 的
  JSON-RPC(prompt/update 等),engine 不需要理解流里的内容。
- **process 支持 `Connect` 重连 + checkpoint/restore**:session 的"恢复"语义可建立在"进程
  态被 checkpoint 保住 + 重连接回"之上,或建立在 agent 原生 `--resume` + `fs` 读历史之上。
  两条路都不要求 engine 懂 session。
- **fs 能从沙箱外读到 agent 自带日志的字节与大小**(read+offset / stat):使 session 层/前端
  能渲染历史,engine 不提供也不需要"会话日志"这种概念。

只要这三个 seam 在,session 就能作为 engine 外的纯加法落地。**本文不设计 session 的内部,
只确保地基不挡它的路。**

> **三个"借 ACP 但要改"的边界**(留给将来真正做 session 时处理,此处仅登记):
> ① ACP 的 fs 是 agent **反向**调 client(方向 agent→client),本项目 fs 是**宿主→沙箱**——
> session adapter 要做方向适配;② ACP **无审计**,本项目要 durable event(在 session 层旁挂,
> 不在 engine);③ ACP 靠**重放**恢复,本项目 process 有 **checkpoint**,resume 可走进程态恢复
> 而非重放。

---

## D4. wire 协议

### D4.1 载体:netns 内 TCP(见 spec §5.E)

载体已定(spec §5.E):`sandboxd` 在 guest netns 里监听 TCP 端口,连接由 sentry 的 userspace netstack
终结,宿主经该 netns 的 veth IP 直连——unary 走 HTTP、双向流走 h2c 流式(server-stream/client-stream)+
即时 flush,不用 vsock;框架是 Connect-RPC over h2c(D4.2),非裸 WebSocket duplex。

transport 只依赖 placement 给出的结果——**netns 由谁建、veth IP 由谁分配、sentry 如何 adopt netns 都归
§5.A**,不在本层重述。对 wire 协议只有一条约束落地:重连拿的是**当前** netns veth IP(冷启即变),不缓存
固定地址(§3.3)。wire 协议建在这层载体上。

下面各条凡刻意偏离 E2B `envd` 的成熟形态,都点明理由。

**通道信任模型:为什么 transport 不带通道内 auth(明确论证,非遗漏)。** `sandboxd` 在 guest netns 内
**明文**监听、宿主经 veth IP 直连,通道本身不带 token / mTLS。这是攻击面分析的**结论**,不是漏项。逐一
过一遍谁够得到这个端口:

- **guest 自己**:够得到,但**拿不到新能力**。guest 内已是 root(§2.2),通道允许的一切(起进程、读写
  **本沙箱** FS、发信号)它作为 root 本就全能做;通道也**不持有凭证**——凭证从不进 guest(支柱④),
  `sandboxd` 只做 process/fs、对 model/provider 凭证一无所知。故"guest 可达控制通道"既不提权、也不泄
  凭证(纠正一个常见误判:它读不到凭证配置,因为凭证根本不在 guest 侧)。
- **sibling 沙箱**:**够不到**——已被跨沙箱网络隔离挡死(§2.3 / network.md D5.5:sibling IP 进 L0 地板 /
  点对点 veth,正是为拦"sibling 直连 veth IP"而设)。
- **外部**:除非**显式 publish**(可带 `auth`,§5.D)否则不可达。

**与 E2B 的关键差异**:E2B `envd` 挂在**公网可达 URL** 上,故必须带 access token;本项目 `sandboxd` 只在
netns 内、宿主经 veth 直连,攻击面小一个量级,**通道内 auth 在当前载体下是冗余的**。但这条**取决于载体
形态**:若将来某 provider(§5.A)把 transport 暴露到不可信网络,通道内 auth 就从冗余变成必需——届时按该
provider 补,属 §5.A provider 的网络义务,不改本 wire 协议。

### D4.2 框架与流式分工

- **RPC 框架 = Connect-RPC over h2c。** 一套 IDL 同时讲 Connect / gRPC / gRPC-web,unary 与 stream
  同源,可走纯 HTTP(契合 D4.1 载体)。E2B `envd` 正是这套,直接抄形态。
- **流式分工 = 非对称三分,不做全 duplex。**

  | 方向 | 形态 | RPC |
  | --- | --- | --- |
  | **输出**(stdout/stderr/pty-out + 进程事件) | **server-stream** | `Start` / `Connect` |
  | **输入**(stdin/pty-in,保序) | **client-stream** | `StreamInput` |
  | **控制**(信号/resize/列举/一次性写) | **unary** | `SendInput` / `SendSignal` / `Update` / `List` / `CloseStdin` |

  多个独立实现都收敛到这个三分——**输出是 fan-out 多订阅(server-stream
  天然),输入是保序单写(client-stream 天然),控制是点操作(unary 天然)**。硬塞进一条 duplex 流会
  把三种不同的背压 / 保序 / 扇出语义揉成一根。D3 要守的 seam(流足够通用以承载 ACP JSON-RPC)由
  `Start`/`StreamInput` 这对双向能力满足,不需要 duplex。
- **长流禁用 idle 读写超时,靠 `keepalive` 维系。** 输出 server-stream 是长驻的(空闲终端可挂几小时),
  服务端**不得**对它施加默认 idle read/write deadline——那会静默杀掉空闲 shell(已知的稳定坑)。事件
  `oneof` 里的 `keepalive`(D4.3)就是为此:服务端周期性发 `keepalive` 帧,让中间层 / 前端知道连接活着、
  不因静默被回收。这属实现级约束,登记在此、不进核心 spec。

### D4.3 process 服务面

RPC 清单(抄 E2B envd 形态;按 pid 寻址,tag 不进模型见 D4.6):

| RPC | 形态 | 语义 |
| --- | --- | --- |
| `Start{cmd,args,env,cwd,pty?}` | server-stream | 起进程,流回 `oneof{start,data,end,keepalive}` 事件 |
| `Connect{pid}` | server-stream | 重连已有进程,接上输出流(§3.2 多连接) |
| `StreamInput{pid, stream<bytes>}` | client-stream | 保序写 stdin / pty-in |
| `SendInput{pid, oneof{stdin,pty}}` | unary | 一次性写(不需保持流时) |
| `SendSignal{pid, signal}` | unary | 见下 |
| `Update{pid, pty.size}` | unary | PTY resize(不单开服务,是 process 的字段) |
| `List{}` | unary | 实时枚举(**无注册表**,§5.E) |
| `CloseStdin{pid}` | unary | 关 stdin(pty 下等价发 Ctrl-D) |

事件 `oneof{start(pid), data(stdout|stderr|pty), end(exit_code), keepalive}`——抄 E2B。**PTY 不
单独成服务**:`Start` 带 `pty.size` 即 pty 特例,resize=`Update`,输入走 input 的 pty 分支。

- **多连接扇出:输出广播,输入不仲裁。** §3.2「多路并发挂同一沙箱」落到单进程即"多个 `Connect` 挂
  同一 pid"。**输出**:每个 attach 者各自拿到一份输出流拷贝(一个源 fan-out 到 N 个订阅者,退订
  不影响其余)。**输入**:engine **不**给多写者做仲裁 /
  加锁——多个连接同时写一个 pty = 多人 `screen` 同一终端,字节交错是 Unix 本身的语义(§3.2「多人
  ssh 同一台机」);要不要协调是**产品层**的事,engine 只如实转发。这守住「engine 不为连接维护状态」
  (§5.E 无状态)。

- **扇出的两条正确性 invariant:源永不因订阅者阻塞 + 有界回放。** 「退订不影响其余」只说 disconnect;
  扇出还有两条必须显式钉死的规则,否则一个卡住的前端能冻结 guest 里的真实进程:
  1. **慢订阅者非阻塞丢弃,源 `write` 永不阻塞。** 每个 live 订阅者有独立**有界**发送窗口;窗口溢出
     即对**该订阅者**非阻塞处置,**背压绝不回传到源**(PTY/进程)。这把 §D1.3「裸
     pty lossy」从模糊表述钉成可验收规则:lossy 的是**慢订阅者那一份拷贝**,不是源。
     - **溢出处置 = 断开该订阅者**(M3 定,原「丢弃 or 断开」的余地就此拍定)。理由:窗口一旦溢出,
       这份拷贝的字节流已有空洞、后续无从无缝续上;与其留一条**残缺**的 live 流让上层误以为完整,
       不如断开,由 client 重新 `Connect`——重连会先回放有界环形缓冲的近期 tail(invariant 2),
       比一条带洞的流干净。断开是**该订阅者**的事,源与其余订阅者不受影响。这不给源加任何"保证送达"
       语义(§D1.3 lossy 边界不变);"你被断开了"是否升级成一条**可观测的连接活动信号**属 §4.3 健康态
       四分(reaper 里程碑)的活,不在本层——本层只保证源不被拖住。
  2. **`Connect` 先回放有界环形缓冲、再接 live。** 每进程维护一个 **per-process、in-guest、按字节上界**
     的输出环形缓冲(旧字节静默丢弃);`Connect{pid}` **先把缓冲内近期
     字节回放给新订阅者、再切入 live 流**。于是重连 / 刷新页面 / 多端挂载**立刻见最近状态而非空屏**——
     这正是 REPL / 交互式终端体验的刚需。
  - **这不破 §5.E「无状态」**:无状态约束的是**控制面不持久 SoT**(无进程注册表、不跨 resume 续命);
    环形缓冲是 transport 进程内的**瞬时、非权威、随进程消亡、不跨 resume/transport 重启**的加速结构,
    落在"无 **持久** SoT"的措辞内(§5.E)。缓冲丢了只损失回放近况,不损失任何权威记录。
  - **边界:engine 只保证 best-effort 字节 tail,不保证屏幕重建。** 裸字节回放可能落在 ANSI 转义序列
    中段;**完整屏幕状态重建(tmux/screen 式 reflow)是 engine 外 client/adapter 的活**,engine 不理解
    流内容(§D3 seam),不替终端发明它本不具备的语义(呼应 §D2「PTY 不落 scrollback 文件」)。
- **信号默认发进程组,可寻址单进程。** `SendSignal` 默认对**进程目标的进程组**发(先 `kill(-pgid)`、
  fallback `kill(pid)`),因为 `apt`/build/
  dev-server 都 fork 子树,只杀父会留一堆孤儿。这兑现 spec §5.E.1 的分工:**定向信号(单进程/进程
  组)是 transport 模式 `sandboxd` 的活**(它握 pid/pgid 句柄),也是 spec §4.2 协作式静止点「向
  guest 内进程发协作信号」的落点;**容器级信号扇出**(SIGTERM → 全体)不在此 RPC,归 PID1(§5.E.1)。

### D4.4 fs 服务面

RPC 清单:全 unary + 大文件 multipart 旁路,不做 watch(抄 E2B 除 watch 外)。

`Read{path,offset,len}` / `Write` / `List{path,depth}` / `Stat` / `MakeDir` / `Remove` / `Move`
全 unary;`Stat` 返回 `EntryInfo{name,type,size,mode,mtime,...}`。**大文件走 HTTP multipart 上传/
下载旁路**,不挤 RPC(E2B 同款)。

- **不提供 `watch`**:E2B envd 有 `WatchDir` server-stream + `CreateWatcher` 轮询两套,我们**两套都
  不要**——watch 要 engine 持订阅/合并/重连状态(破 §5.E 无状态),且 gVisor overlay upperdir 上
  inotify 可靠性存疑(§2.5)。消费方用 `Stat`+`Read(offset)` 自行轮询拉增量(§2 渲染 vs 交互非对称)。
- **resume 后读无 offset 续传契约**:`fs.Read` 读的是 Overlay 工件**当前**字节;resume 换新容器后
  engine **不**保证跨 resume 的 offset 连续性(它无状态、不记读游标)。消费方跨 resume 要「从头整份
  读一遍」或自持游标按 `Stat.size` 变化决定增量。与"输入不仲裁"同源:engine 不替连接记状态。

### D4.5 liveness / readiness:一个 `Health` 探针

`sandboxd` transport 模式暴露一个 unary `Health{}`→`{ok}`。它一处两用,不另造协议:transport 探活
探针(spec §4.3 两层探活的
transport 层)与 resume readiness 门(guest-runtime D5.2 要点①:首次 `Health` 成功即翻 `running`,不信
裸 TCP accept)。**用专门的 `Health` 而非 `process.List` 兜底**——`Health` 恒定开销、不列进程,探活
开销不随进程数偏移。

### D4.6 三条刻意偏离

- **不引入 tag 寻址**:E2B `ProcessSelector{pid|tag}` 有 tag,我们**只 pid**(guest 原生)。tag 是
  引擎之上的命名空间,属产品层数据、不进模型(§1.1;§3.2.1 名字是数据不是引擎原语)。
- **不引入 session/control payload**:`control`(agent 会话)是 engine 外 session 层的协议,不是 engine 的
  payload(全论证见 D1.2)。
- **不引入 payload 类型系统**:jupyter/file/cli 都是 process/fs 上拼出来的,engine 侧无 payload
  registry(D1.3)。

---

## D5. 边界与仍待

- **checkpoint / restore 原语**:engine 提供的进程级冻结/恢复(runsc),对齐 spec §4.2「不存内存、
  resume 冷启」;给 session 层留的恢复 seam(进程态 vs `--resume` 重放)仍是 open-decision
  ([`open-decisions.md`](../open-decisions.md) §3)。
- **engine 独立使用形态**:只用 process/fs 的沙箱产品边界;session adapter 将来如何在外面叠。
- **API / CLI 投影**:见 D6。spec §6.1 的 process/fs 两组端点按本节 RPC 清单细化(session 不进
  engine API);外沿协议分工(控制面 REST / 数据面 Connect)在 D6 定死。

---

## D6. 外沿投影:控制面 REST + 数据面 Connect(client↔orchestrator)

D4 定**内沿** wire(host↔`sandboxd`);本节定**外沿** wire(client〔前端/CLI/SDK〕↔`orchestrator`)。
两沿的协议分工不同,理由如下。

### D6.1 一个不可绕过的前提:client 永远不直连 `sandboxd`

E2B 的 SDK **直连** envd(经路由代理,`E2b-Sandbox-Id/Port` 头寻址)。本项目**不能照抄这条**,
原因是载体 + 纪律双重决定的:

- **载体(D4.1)**:`sandboxd` 只在 guest netns 内明文监听,只有宿主够得到那个 veth IP;client
  在引擎外,物理上到不了。
- **§4 mediator 纪律**:一切沙箱动词经 `orchestrator` 居中(它按 `sandbox_id` 解析当前端点,
  placement 永不外泄,§6.2)。client 直连 `sandboxd` 会把 placement 泄到引擎外,破 §3.3。

所以外沿数据面**必须终结在 `orchestrator`、再由它 re-proxy 到内沿 `sandboxd`**。E2B「数据面直连」
那份好处本项目拿不到(载体决定),但**typed-contract + 多语言 codegen 那份好处能拿**——这是 D6.2
的全部动机。

### D6.2 分工:控制面 REST/OpenAPI,数据面 Connect-RPC

| 面 | 协议 | 端点(§6.1) | 为什么 |
| --- | --- | --- | --- |
| **控制面**(沙箱/template/volume/network 生命周期) | **REST + JSON**(OpenAPI 事后描述) | `POST /v1/sandboxes`、`pause`/`resume`、`DELETE`、`GET`、volume/template/publish… | 低频 CRUD,手调 / curl / 文档化友好;无强类型流。E2B 同款分法 |
| **数据面**(process / fs) | **Connect-RPC over h2c**,proto 契约先行 | `engine.transport.v1.ProcessService` / `FsService`(D4.3/D4.4) | 有 server-stream + 强类型帧;proto 是**唯一契约源**,前端(TS)/ 未来多语言 SDK 全从它 `buf` 生成 client stub |

**关键收益:内沿那份 proto 契约直接投影到外沿。** `orchestrator` 对外**复用同一份**
`transport.proto`(而非另立外沿 schema),数据面外沿就是**Connect 反代 + 按 `sandbox_id` 解析端点**
——无翻译层,内外沿只有一份 wire 表示。这兑现 §6「CLI/前端是同一套 API 的平级 client」:两端共用
proto 生成的同一份 client。

### D6.3 为什么数据面外沿走 Connect,而非 REST facade

数据面外沿的另一条候选是**手写 REST facade + NDJSON chunked 流**(orchestrator 逐帧把内沿 Connect
stream 翻成 NDJSON)。它被否决,三条理由:

- **凭空造第三种流表示**:内沿 Connect server-stream、外沿 NDJSON、(将来前端可能还要的)另一套——
  三份并存靠 orchestrator 手工翻译,正是 §1.3「成本判据」要防的加法;Connect 直投只剩一份。
- **在最需要契约的边界丢类型**:数据面进程/文件帧(`oneof{start,data,end}`、`DataEvent.stream`)正是
  强类型受益最大处;NDJSON 拍平成无 schema 的 JSON,前端/SDK 得手写解析。Connect 直投让前端 `buf`
  生成、编译期对齐。
- **不换来简单**:REST facade 的手写 handler(query-param `?path=`、NDJSON encoder、flush 循环)不比
  Connect 反代少写代码,反而多一层要维护的映射。

**控制面为什么不一起上 Connect**:控制面是 CRUD,无流、无强类型帧红利,REST/OpenAPI 的手调与文档
化价值更高;强上 proto 是负收益。分界线落在「有没有 stream / 强类型帧」上——数据面有,控制面没有。

### D6.4 边界与残留

- **鉴权对齐 D4.1 的分层结论**:外沿(client↔orchestrator)是否加 auth 由 `orchestrator` 的部署
  形态决定(它可能挂公网),**与内沿 `sandboxd` 无通道内 auth 的结论正交**——别把内沿"通道内 auth
  冗余"错用到外沿。外沿 auth 属 orchestrator 的接入层义务,不在本 wire 协议。
- **session 仍不进外沿 API**:D1.2 的划界不因投影而变——外沿只投影 process/fs,session adapter 在
  engine 外自带它的协议面(ACP 形态)。
- **前端多语言 stub 是纯加法**:外沿复用同一份 `transport.proto`,前端(TS)/ 未来 SDK 从它 `buf`
  生成 client;新增一门语言的 client = 加一个 `buf` 生成目标,不改本投影形状(§6 契约随 milestone
  生长)。
- **`sandbox_id` 走带外请求头,不进 wire。** D4.6 把 `sandbox_id` 挡在 proto 外(进程只按 guest-native
  pid 寻址),D6.2 又强制外沿复用**同一份** `transport.proto`——两者合起来**逼出**一个带外载体:外沿
  Connect 请求用请求头 `Engine-Sandbox-Id` 携带 id,反代 strip 掉它、经 mediator 解析当前端点后透传到
  内沿(placement 永不外泄,§6.2 / D6.1)。头名是实现约定,不改 wire 形状。
- **实现级取值(M3 落定)**:环形缓冲上界 **64 KiB**、慢订阅者发送窗口 **256 帧**、`keepalive` 周期 **20s**。
  这些是可随经验调的**数值旋钮**,不是设计问题(形状由 D4.3/D6 定死);登记在 open-decisions §5.E 作
  "当前取值"。**溢出处置(断开)不在此列**——那是已在 D4.3 invariant 1 拍定的设计选择,不是旋钮。
