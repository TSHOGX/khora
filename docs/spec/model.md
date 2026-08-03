# Sandbox Engine — 模型层(§3 沙箱模型 · §4 生命周期)

> 属 [SPEC.md](./SPEC.md);全局 §编号跨文件连续,导航见 SPEC.md 顶部「文档地图」。

---

## 3. 沙箱模型

### 3.1 三态:可变性按层切开

```text
Template   不可变、内容寻址的只读底座(可被多沙箱共享/fork)
  └─ Live   = Template(只读底层) + 可写 Overlay + 运行内存          ← 临时、可变
       └─ Paused = Overlay 落盘静止,容器停掉,释放 RAM/CPU          ← 只占存储
            └─(可选)Promote = 把当前 Overlay 固化成新 Template
```

- **Template**:不可变、共享、可 fork,**opaque ID 作身份**。平台预置(纯净 base、带某 agent
  的栈…),或用户 Promote 自建。不可变性由**只读底层 + refcount 保护**兑现,不靠内容 hash——hash
  只能*检测*篡改、不能*阻止*(阻止靠只读机制);**内容摘要是可选的去重/完整性演进锚点**(§5.B /
  rootfs-overlay D3),身份与去重/缓存是干净分开的两件事。
- **Overlay**:本沙箱相对 Template 的**可写增量层**(apt/pip 与 `$HOME` 都写进它),叠在
  Template 之上构成 Live 的可写 rootfs。**Overlay 是一等、ID 寻址、与 Template 解耦的持久工件**
  (见 3.1.1),不是 Template 的附属物。
- **Live**:`Template(只读底层) + Overlay(可写层) + 运行内存`。运行内存是临时的(pause 即弃,
  §4.2);Overlay 持续在存储后端上。
- **Paused**:静止后停掉 runsc 容器,Overlay 留在后端,释放 RAM/CPU。**不做内存快照**(§4.2)。
- **Promote**:把当前 Overlay 固化成命名 Template 的新底层(像分层镜像),供以后 fork。**对一个
  逻辑上运行中的沙箱发起,但读的是 at-rest 字节**——引擎内部先把它静止/flush 到一致点(等价一次
  pause),再读冻结的 Overlay 去固化;绝不在容器运行中读(§5.B"只读 at-rest")。

#### 3.1.1 Overlay 是解耦工件,不与 Template / 宿主路径绑定

这一条是 multi-node 能否成立的前提:Overlay 的物化位置不得退化成"upperdir 一直在本地盘上"。

- **逻辑模型**:Overlay 是一份**经 rootfs provider 寻址的冻结层**(自带 ID 与属性),它**引用**一个
  Template 作底层,但**存在何处与 Template 无关**——可以与 Template 同盘,也可以在另一块网络存储、
  对象存储,其属性(大小、引用的 Template、refcount、生命周期态)甚至可单独建表记录。
- **物化是 rootfs provider 的事,模型不假设宿主本地路径**:single-node 落成本机 overlayfs upperdir、
  multi-node 落成可跨 node 寻址的层——具体两种物化见 §5.B。关键是模型层 Overlay 始终是"引用某
  Template 的、ID 寻址的冻结层";**一旦假设宿主本地路径,任何非本地形态的 provider 一旦接入即失效**,
  这正是 §3.3"身份不编码 placement"与 §4.2 pause/resume 在 multi-node 下成立的前提。
- **对内也 ID 寻址**:§6.2 "对外不暴露宿主路径" 的要求**同样贯彻到引擎内部的 Rootfs 子系统**——
  Overlay 在控制面里以 ID 与属性存在,宿主路径只是某个 provider 的实现细节,不进模型。

### 3.2 沙箱 = 持久单元,连接 = 多路并发的 process

```text
Sandbox(持久单元:Overlay 工件、挂载的 Volume、网络策略、资源上限)  ← 稳定、不透明 ID
  ├── shell        (process + PTY)
  ├── agent        (process,如 Claude)
  └── agent        (process,如 Pi)     ← 多路并发,共享同一沙箱的 FS / 网络
```

shell 是"每个沙箱上随时可用的一类连接",不是一种产品 mode。多个连接并发写同一 FS = 多人
ssh 同一台 Linux,是 Unix 模型本身。**每条连接 = `process` 服务的一次 `Start`**(shell 即
`exec /bin/bash` 带 PTY),按 pid 寻址、可 `Connect` 重连;engine 不为连接维护任何注册表(§5.E)。

#### 3.2.1 swarm / 多 agent:engine 不认识"agent",更不寻址它

一个沙箱里可能要跑多个 agent、不同种 agent,甚至 **agent 自己再派生 agent**(swarm)。
**engine 只提供 process/fs 两个通用机制,从不认识"agent"这个概念**。三层切开,各归其位:

- **Engine 层**:沙箱就是一台机器,只暴露 `process`(起/连/控进程)和 `fs`(读写
  文件)。agent 派生 agent = 一台 Linux 上的进程经 localhost 互相说话,engine **不追踪、也不该
  追踪**(符合解耦前提)。engine 侧**没有**命名端点、tag、会话注册表这类原语。
- **寻址用 guest 原生机制,不靠 engine 造命名空间**:要跟某个(含被派生的)agent 说话,用它的
  pid / unix socket / 端口,经 `process` 能力连过去——**Linux 已有进程命名空间,engine 不再发明
  一个**。若产品要人类可读的名字,那是**数据**(agent 把"我是 planner"写进自己日志),产品层读
  日志即得映射,engine 永不解析名字。
- **产品层(在 engine 之上,不在其内)**:"session"、swarm 拓扑(谁派生谁、DAG)、哪个 agent 归
  哪个用户/租户——全位于这里,建在 engine 的 ID 寻址原子(sandbox / process / fs)之上(§1.4)。
  agent 会话语义本身也是 engine 外的 adapter 加法(§5.E)。

### 3.3 Identity:指代,不是归属

这里的 **Identity 是"指代哪一个"**——即"这个沙箱""这条连接"指的是谁、它变了还算不算
同一个。**不是"谁拥有它"**:用户身份 / 操作权限不在此讨论(也不在引擎内,见 §1.4)。下面
三层里,前两层是**稳定指代**(跨变化保持同一),Placement 是**每次重绑的物理资源**——它列在表内
正因为最容易被误当成身份:

| 层 | 是什么 | 跨 pause/resume | 说明 |
| --- | --- | --- | --- |
| **Sandbox** | 这个持久沙箱(Overlay 工件 / 挂载的 Volume) | **稳定、不透明** | resume 后仍是同一个沙箱 |
| **Process** | 一条连接(shell / 各 agent) | 临时、可并发 | pid 寻址,连接建立时由 guest 分配,可 `Connect` 重连 |
| *(非身份)* **Placement** | netns/IP / 容器 / cgroup / 宿主侧进程 uid / 落在哪个 node | **每次重新绑定** | resume 时从头分配 |

铁律:**Sandbox 身份不编码 placement**。沙箱 resume 后 IP / 容器 / cgroup / node 都可能变,但
Overlay 工件没变(会话历史等也只是 Overlay 里的字节,不另算身份),仍是同一个沙箱。这是 §4
pause/resume 与 multi-node provider(§5.A)同时成立的前提。

### 3.4 Source of Truth:两类

按 **resume 行为**切开,只有两类:

- **Frozen(读字节)**:SoT 是后端上的字节——沙箱 **Overlay 工件**(经 rootfs provider 寻址)、
  data Volume 内容(各自后端)。resume 直接读回。
- **Binding(按控制面记录重建)**:SoT 是沙箱自己的控制面记录——引用哪个 Template、挂了哪些
  Volume、**资源上限(CPU/RAM/disk)**、**生命周期 TTL(`idle_timeout`/`retention`,§4.3)**、placement、
  网络策略。resume 时按**当前记录**重建,不读冻结字节。资源上限与 TTL 都是 Binding 而非 placement:取值跨
  resume 稳定保持。**但物化点不同**:资源上限在每次 placement 时物化(§5.A/§5.B/§5.C);**TTL 没有任何
  placement-local 物化**,它的消费者是 reaper,每轮 sweep 直接读记录。连接(process)**不是 Binding**:engine
  不存连接记录,resume 后由 client 重新发起(§5.E)。

> **"资源上限"是三个维度,各用自己该用的机制物化,超限后果也就各自不同。** 三者都是 Binding、
> 都在 placement 时物化:
>
> | 维度 | 机制 | 超限后果 | 定位 |
> | --- | --- | --- | --- |
> | **CPU**(速率;disk I/O 同轴,待有消费者时同形加入) | §5.A 落 cgroup(`cpu` 节流) | guest 变慢,无错误 | 资源上限:公平分配 |
> | **disk 容量**(at-rest 字节) | 文件系统层配额,§5.B(Overlay upper+work)/ §5.C(每 Volume)经二者共享的存储 substrate 原语落 | `write()` 返 `ENOSPC`,**沙箱照常存活** | 资源上限:公平分配 |
> | **RAM** | 宿主 memcg(+ `oom.group=1`) | **沙箱终止** | **安全限:约束爆炸半径** |
>
> **disk 容量与 disk I/O 是两条正交的轴**,别混:cgroup 管不了 at-rest 占多少字节,配额管不了读写
> 速率。容量轴是 §2.3 爆炸半径对磁盘维度的 enforce 点。
>
> **三个维度就是全部,`pids` 永久排除。** 实测:cgroup 的 `pids` 帽限的是**宿主 task**(systrap 下每个
> guest 进程约 2.3 个),设低了 sentry 直接 panic(`failed to get clone`)、整个沙箱消失——它是**可用性
> 地雷而非资源帽**,与本节三个维度"超限后果各自明确"的性质根本不同(超 pids 帽的后果不是变慢、不是
> `ENOSPC`、也不是可解释的终止,而是沙箱凭空不见)。故它不进 `ResourceLimits`、不进 §6.1。

> **节点级不超卖:两条独立预算,各自按"引擎承诺了什么"计量。** 每沙箱的帽只把"撑满整机"降级成
> "撑满自己";帽的**总和**能否被兑现是另一件事,由 create / resume 前的**准入门**核算(§4)。两条预算
> 互不通兑(内存与磁盘不可替换),**只有超限后果不优雅的轴才设预算**——磁盘容量(无辜沙箱吃 `ENOSPC`)
> 与 RAM(沙箱被终止);CPU 超卖只是"guest 变慢,无错误",故无 CPU 预算。
>
> - `Σ(已声明的 disk 帽) + Σ(无帽覆盖的实测字节) ≤ 配置的磁盘预算`(推导见 rootfs-overlay D6.5)
> - `Σ(已声明的 RAM 帽) ≤ 配置的 RAM 预算`
>
> **两项互斥,同一个物理字节只计一次。** 一项是**已声明的帽**(对未来增长的上界),一项是**无帽覆盖的
> 已占字节**(唯一可得的真陈述):有帽的 Overlay upper 在实测项里**计 0**——把它的当前用量加上去等于
> 在已有承诺之上再收一次,且那是预测而非已占。判据统一为**对每个对象取最强的可得陈述**。
>
> **两条预算都只计"引擎能命名的字节"。** layer 字节单列,是因为 **Template 是引擎的一等对象**
> (§4.3 refcount / §5.B),它占的字节没有任何沙箱帽覆盖,引擎必须认账。而**基底自身的每沙箱开销
> ——磁盘上的 runsc state、RAM 里的 sentry/gofer 足迹——在预算之外**:那是 provider 的实现常数
> (§5.A),算进核算式就是把它焊进 mediator 的账本。故预算的含义是**引擎可以向 guest 承诺的字节**,
> 不是宿主的物理余量;operator 把预算配在物理值之下,那道差额就是留给基底自身开销的。
>
> > **一条诚实的残留**:未计入的 RAM 开销与沙箱数成正比,其占比随帽变小而放大,故"按物理 RAM 的某个
> > 百分比配预算"不对所有帽尺寸成立。若它咬人,收口手段是配一个**最小 RAM 帽下限**(约束比例上界,
> > 不需引擎命名 overhead、不动 §5.A seam),不是把 overhead 变成契约成员。
>
> **一旦某轴被计量,该轴上的每个沙箱都必须有帽。** 无帽沙箱(取值 0 = 无上限)对核算式左侧贡献 0、
> 却可占用右侧全部——它不是"少算一点",而是让整个不等式**不再描述那块盘**。故计量轴上的无帽 create /
> resume 在准入处被**拒绝**(400,与"节点已满"507 是两件事),而**不是**按默认值静默补齐:拒绝可见,
> 默认不可见——§2.3 之所以能说 `ENOSPC` 是"数据面内的**预期**结果",前提是那道边界被**显式声明**过。
> `0 = 无上限` 仅在该轴**未被计量**时合法。**RAM 轴尤甚**:无帽意味着超限不再被约束在本沙箱内,且
> 没有 memcg 可读、死因只能落 `Unknown`(§4.3)。CPU 不在核算内,`cpu_millis: 0` 恒合法。
>
> **抬高上限须重过准入门。** 准入门**从 store 求和**(含 `paused` 沙箱),所以任何对帽列的写入**当场**
> 改变 Σ,不是等到 resume;抬帽因此必须在提交前重跑准入、**可能被拒**,降帽不需要。同理,任何**增加
> 无帽字节**的控制面动作(如 Promote 固化新层)都应过同一道不等式。
>
> **`paused` 沙箱仍占两条预算**,尽管它物理上已释放 RAM/CPU(§4.2)。理由不是记账方便,而是 `paused`
> 必须是**可逆的门**(§1.1):不计它,则新 create 会吃掉它 resume 所需的资源,resume 撞准入门留在
> `paused`(§4.4-A),而 `UpdatedAt` 不前进 ⇒ `retention` 照走 ⇒ 沙箱被 evict、**Overlay 字节被销毁**
> (§4.3)。准入门存在的理由正是"不让一个沙箱因**别的**沙箱的用量而失败";因别人的用量而被**销毁**
> 比因它而写失败更严重。**代价明说**:一个大多数沙箱处于 `paused` 的节点会对着物理空闲的 RAM 拒绝
> create,而 auto-pause(§4.3)正是引擎自己制造这个局面的那一手。这是"pause 保证可恢复"的价钱,接受。

> **Binding 可写,写点唯一,生效点按消费者分两类。** Binding 的 SoT 是控制面记录,所以"改 Binding"
> 就是改那条记录——§6.1 `PATCH /v1/sandboxes/{id}` 是 create 之外**唯一**的写点,与其他动词共用同一把
> 转换锁 + 条件写提交(§4.4-B / §4.4-C)。生效点取决于**谁消费这个字段**:
>
> - **provider / rootfs provider 消费的(资源上限三轴)**:在**下一次 placement** 物化,即下一次 resume。
>   故这类字段**只在 `paused` 可改**——对 `running` 沙箱接受一个要等未来某次 resume 才生效的编辑,会让
>   记录与活着的隔离单元长期不一致,是"记录说一套、substrate 做一套"的假不变量。操作路径
>   `pause → PATCH → resume`,与 §5.C 改挂载表、§5.D 改网络策略同一条路。
> - **reaper 消费的(`idle_timeout` / `retention`)**:reaper 每轮 sweep 直接重读记录、**不经 placement**,
>   故**立即生效**,`running` / `paused` 皆可改。
>
> **写入的三条附带纪律。** ① `PATCH` 只换整份绑定:未给的字段不动,且不存在"待定/未解析"值(§6.2)。
> ② 它**不动 `UpdatedAt`**——`retention` 从那一列起算(§4.3),顺手刷新它等于把驱逐期限当作改帽的副作用
> 悄悄延长。③ 它**不是 §4.1 转换**,故不发事件(与 Promote 同理);"改绑定要不要发一条非转换事件"随
> event schema 那轮定(open-decisions §2)。
>
> **provider 不被要求就地改帽**:没有"把这个单元的帽设成 N,现在"这种调用——那是宿主原语形状的请求,
> 被调度的远端隔离单元一般兑现不了(§5.A ❌ list)。改帽走的是"改记录 + 下次 placement 重物化"。
>
> **RAM 上限不是资源上限,它是安全限。** 它约束爆炸半径,不提供公平分配。超限的后果是一条
> **生命周期事件(沙箱终止)**,不是一次操作失败——因为 substrate 上不存在"guest 内进程拿到
> `ENOMEM` 而沙箱存活"这条路:gVisor 无 guest 内 OOM killer,guest 内 `MemTotal` 只是开机读一次的
> 装饰值,着力点只有宿主 memcg。故 RAM 超限是**控制面必须知道的状态转换**(§4.3 有其处置分类),
> 而 CPU/disk 超限是数据面内的事件(guest 自己处理一个慢速或一个 errno),控制面不介入。

> 注意 Overlay 工件**横跨两类**:它的**字节**是 Frozen(直接读回),但它**引用哪个 Template**
> 是 Binding(控制面记录)。resume = 读回 Overlay 字节 + 按记录把它叠回当前 Template 之上,二者
> 缺一不可。

> 没有第三类(可撤销的外部授权)。引擎里没有授权主体,也就没有"外部可撤销 authority",
> 所有策略的权威都在沙箱自己的控制面记录里,归 Binding。

---

## 4. 生命周期模型

生命周期是**凌驾于五个子系统(§5)之上的协调者(mediator)**,不是第六个平级子系统,也不是某个
子系统的内部功能。理由:`resume` 这**一个动作就横跨 A–D 四个子系统**——停/起进程(A)、重挂
Overlay(B)、按挂载表重挂 Volume(C)、按网络策略重建 egress(D);E(传输)无状态,引擎不参与,
由 client 自行重连。

由此定下职责归属,防止成环:

> **动词归 §4,机制归 §5。** 生命周期(§4)持有状态机,按状态机**调用**每个子系统暴露的能力;
> 子系统只提供机制(FS 提供"挂/卸 Overlay",Network 提供"建/拆 gateway"…),**子系统之间永不
> 互相调用**。"FS 在 resume 时去戳 Network" 这类跨子系统调用是被禁止的——那正是反向依赖成环的
> 起点。

### 4.1 状态机

四个生命周期状态:`creating` / `running` / `paused` / `destroyed`。(与 §3.1 三态的关系:
`running`↔Live、`paused`↔Paused;Template 不是沙箱状态,`creating`/`destroyed` 无对应 FS 态。
§3.1 三态讲 rootfs 可变性,本节四态讲生命周期机。)

```text
creating ──▶ running ──pause / auto-pause / crash──▶ paused ──▶ destroyed
                │   ▲                                 │   │         ▲
                │   └──────────── resume ─────────────┘   │         │
                └──────── destroy ────────────────────────┴─────────┘
        (destroy 可从 running 或 paused 发起;evict = reaper 对 paused 的自动 destroy)
```

- **显式转换**(API 动词):`create` / `pause` / `resume` / `destroy`;`destroy` 从 `running` 或
  `paused` 都可发起,终态 `destroyed`(删 Overlay 工件)。
- **自动转换**(reaper 守护):`running` 空闲超阈值 → `auto-pause`;`paused` 超保留期 →
  `evict`。**`evict` 就是 reaper 对 `paused` 沙箱触发的 `destroy`**——同一个 `→ destroyed`
  终态,只是触发方是 reaper 而非用户。**两条自动转换都是"释放"方向**;引擎自主的
  `paused → running` **只有一条**:transport-down 连续超重启预算后的升级(§4.3 / reaper-health D4b),
  **dead-recoverable 不在其中**(它由显式动词恢复,§4.3)。
- **非自愿转换**(探活驱动):对账或 reaper sweep 发现一个 `running` 沙箱的隔离单元已死时,CAS 把它降到
  `paused`(§4.2 崩溃变体 / §4.3 / §4.4-A)——`running → paused` 的第三条触发,与用户 `pause`、reaper
  `auto-pause` 并列,事件 Kind 为 `crash-paused`(见下条)。**死因随这次 CAS 同一提交落库**(§4.3):
  它决定该沙箱之后走 resume 还是不走,而信号在提交后即不可恢复。
- **转换对外发布为事件**:每次状态机转换(created / paused / auto-paused / crash-paused / resumed /
  destroyed …)发布到一条 **event stream**;webhook、metrics、计费等 subscriber 在**引擎之外**消费,
  **引擎不知道也不依赖谁在订阅**。这是 observability / 计费的接法——单向、additive(§1.3:add a
  subscriber),不是引擎反过来回调外部系统。**三种 pause 类事件的态都是 `paused`,但 Kind 分承其因**:
  `paused` = 用户暂停、`auto-paused` = reaper 空闲暂停、`crash-paused` = 对账发现单元已死的**非自愿**
  降级(§4.2 崩溃变体);态相同,Kind 让 subscriber 分辨"为何暂停"。**状态提交与事件发布如何做到不丢
  不错(计费级),见 §4.4。**

### 4.2 pause / resume:FS-first,无内存快照

这是本引擎与"重型快照系统"的根本分野:

- **pause** = 把沙箱静止到一致点 → 干净停掉 runsc 容器。Overlay 工件**本来就在后端上**,
  pause 时它**什么都不用做**;停容器即释放 RAM/CPU。
- **resume**(引擎侧)= 用同一个 Overlay 工件 **冷启一个新容器** + 按 Binding 重挂 Volume / 重建
  egress。引擎的 resume **到此为止**——它只恢复沙箱基底,不碰会话。
- **会话续接不在引擎 resume 内**:agent 要"接着上次聊"是 client/adapter 在重连后自己调 driver
  原生 `--resume`(读 Overlay 里的落盘历史),引擎不感知、也不参与(§5.E)。
- **"FS 的 pause/resume" 不是独立机制**——Overlay 工件持续在后端,pause 这侧零捕获;难的内存那侧
  直接不做。

**静止点归 pause(§4),不是某个子系统的一等属性。** "把沙箱静止到一致点"是 pause 动词编排的
一个动作,横跨三个子系统的机制、和 resume 同构:**§5.A 出冻结机制**(停/冻 runsc 容器)、
**§5.E 出信号通道**(向 guest 内进程发协作信号)、**§5.B 出 flush 一致点**(容器停前把 in-flight 写落到
Overlay upper,保证 at-rest 字节一致,§5.B"只读 at-rest")。生命周期持有"何时静止、静止到哪",
子系统只提供机制——不把静止点做进 §5.E process(那会让无状态、payload 无关的传输层引入进程
语义)。静止点**按进程行为分两类原语,不按 agent/turn 语义分**:

- **长跑进程(可能在 I/O 中途)**:走**协作式静止点**——给一个到安全点的机会,再 flush、再冻结,
  避免截断 in-flight 写。识别"哪种进程要协作"是**进程行为**层面的事,不需要 engine 认识 agent/turn;
  且协作只管**应用层安全点**,substrate flush(§5.B)本就保证,故协作非正确性前提(§4.4)。协议两层
  (host 侧 drain-barrier + guest 侧信号观测)、不引入 guest 内 ack 通道,完整机制与参数级待办见
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.4。
- **裸 pty(shell)**:走**抢占式**——pty 字节流本就明说 lossy(§5.E),直接 runsc 冻结,不需要
  协作握手。

为什么这样选是对的:

1. **auto-pause 的触发点恰是内存最没用的时刻**。idle = 所有连接都 idle(agent 无 in-flight 活动、
   pty 无 I/O),一个 idle 沙箱没有有价值的 in-flight 内存态可保。
2. **正确性本就不该依赖内存态**——会话续接靠落盘的 driver/home 状态(由 adapter 读),冷启
   重建不是退化,是契约。
3. **代价**:resume 是冷启秒级,不是亚秒。resume 发生在 idle 之后、不在热路径,可接受。

副产品:沙箱持久态是**纯文件 Overlay 工件**,天然跨 host / CPU / runsc 版本可移(§1.5#3 / §5.B)。

**崩溃 = 非自愿的 pause,走同一条 resume 路径恢复。** FS-first 模型自然覆盖崩溃恢复,无需新
状态:持久态全在后端的 Overlay 工件里,substrate 意外死掉(sentry 崩溃、node 掉电)⟺ RAM/CPU
被释放 ⟺ 与一次 pause 的**盘上效果完全等价**。因此恢复一个崩溃的沙箱 = 一次 resume,不是特殊
路径。这给 reaper 提了个要求——须把"substrate 已死但可恢复"与"健康且空闲"分开,崩溃的沙箱要走
resume 而非 auto-pause 的静止(健康态分类与判据见 §4.3)。

> **注意范围:这里的"崩溃"专指 substrate 死亡(sentry/容器整死)**——即 §4.3 探活的 L1 失败。
> transport 模式 `sandboxd` 单独崩溃**不属于**这条路径:它由 PID1 原地重启、同 IP 恢复(§5.E.1),
> 是**进程内可恢复**的 transport-down 瞬态(§4.3),不走 resume。两者靠两层探活区分。
> **控制面(orchestrator)自身崩溃是第三根轴**,不在本节:它不损坏持久态、只可能留半建 placement,
> 由 §4.4-A 的崩溃变体(重启对账)接,不走 resume。

### 4.3 健康与回收模型

回收不是独立机制,而是**健康模型的消费者**:reaper 先判沙箱当前健康态,再据态处置。引擎不引入独立
世代计数(§3.3 用 Placement 表达"在哪"),陈旧性检测因此由本节的健康模型 + placement/IP-as-epoch
显式接住——语义落在模型里,不靠一个世代计数字段。

**探活是两层,不是一次。** PID1(init 模式 `sandboxd`)会**原地监督并重启** transport 模式子进程
(§5.E.1),因此"substrate 活着"与"transport 端口在听"是**两个独立事实**,控制面必须**两个都探**才能
正确分类:

- **L1 — substrate 探活**:runsc/sentry 容器是否活(§5.A)。
- **L2 — transport 探活**:向 netns IP:port 发一个 `Health` RPC 是否成功(§5.E,见 transport.md D4.5)。

**健康态分类(先两层探活,再判 idle——兑现 §4.2 的要求)。** 处置**互不相同**:

| 健康态 | 判据(L1 substrate / L2 transport) | 处置 |
| --- | --- | --- |
| **healthy** | L1 活 + L2 通,且有连接活跃(近期有 I/O:进程输出 / pty 读写) | 保持 `running`,不动 |
| **idle** | L1 活 + L2 通,但**所有**连接都 idle(近期无任何 I/O) | 超阈值 → **auto-pause** 静止到一致点(§4.2;flush 由 substrate 保证,不依赖协作握手) |
| **transport-down(瞬态)** | **L1 活、L2 不通**——transport 子进程崩了,PID1 正在原地重启(§5.E.1) | **等重启**;端口在**同一 IP** 上恢复,**不是** dead-recoverable、**不**走 resume;**连续 transport-down 超重启预算 → 升级为 dead-recoverable 走 resume**(防崩溃循环卡死,机制见 reaper-health D4b) |
| **dead-recoverable** | **L1 探不到**(容器整死),持久态(Overlay 工件)在后端完好,**且死因不是撞资源帽** | 走 **resume**(§4.2),换新 IP 冷启,**不是** auto-pause |
| **resource-terminated** | **L1 探不到,且死因是撞 RAM 帽**(§3.4 安全限:沙箱被整组终止) | **不 resume**、不 auto-pause——留 `paused`。resume 会让同一负载再次触发终止,即崩溃循环。**转机只有一条:改帽**——经 §6.1 `PATCH /v1/sandboxes/{id}` 抬高 `memory_byte`(须重过准入门,节点无容量则被拒),**该次提交同时清死因**,之后 resume 才放行。**它不豁免驱逐**(见下) |

> **resource-terminated 与 dead-recoverable 并列,不是它的子类。** 两者 L1 都探不到、持久态都完好,
> 表现完全相同;区分只能靠**死因信号**,而处置**相反**(一个必须 resume,一个必须不 resume)。死因跨
> §5.A seam 时是**抽象枚举**(`{Unknown, Gone, ResourceExceeded}`),不是 errno / cgroup 文件:
> `local_runsc` 从自己的 memcg 得出结论,远端 provider 从其调度层的终止原因得出同一结论,**mediator
> 只看枚举**。这让该分类跨 provider 成立,而非 host-local hack。
>
> **死因必须在观测到的那一刻落库,且与降级 CAS 同一次提交。** 死因信号是**易失的**(local 形态下它
> 在 provider 的 placement-local 资源里,随 placement 拆除而消失,也不跨宿主重启),所以"事后再问一次"
> 这条路不存在:降级提交之后证据已不可恢复。故 `crash-paused` 的 CAS 与死因写入是**一次原子提交**
> (§4.4-C 同一形状)。
>
> **`Unknown` 是一等合法值,不是错误。** 读不到死因时按 `dead-recoverable` 处置(去 resume)。
> **自我纠正是结构性的,不靠计数器**:若确是撞帽,下一次 resume 后同一负载会再次触发终止,而这一次
> 发生在控制面眼前、placement 完整,于是被观测、随降级 CAS 落库,**此后 resume 由那条已落库的事实
> 拒绝**。**没有 resume 次数上界,也不需要有**:引擎从不自己 resume 一个 dead-recoverable 沙箱
> (§4.1 的自动转换只有 auto-pause 与 evict,外加 reaper-health D4b 那一条 transport-down 升级),
> 故重试是调用方逐次显式付出的选择,不是引擎的循环;按次数拒绝一个显式动词与按**已落库事实**拒绝
> (`resource-terminated`)性质不同。想要"崩了自动拉起"是消费 `crash-paused` 事件的**引擎外 subscriber**
> (§1.3 Consumption)。完整证伪(为何计数无可推导的重置点、为何它等价于被禁的世代计数)见
> [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D4c。
>
> **一条诚实的残留**:结构性自我纠正只对**有帽且可复现**的耗尽成立。有帽沙箱若每次死亡时 placement
> 已被拆除(与 teardown 竞争、或跨宿主重启),死因可反复读作 `Unknown`,**收敛到 `resource-terminated`
> 无保证**。挡住无限循环的不是任何引擎侧上界,而是:每次重试都要调用方显式付一次动词、§2.4 的整组
> 确定性终止已根除"无可杀 victim → 无限重试"那条会烧宿主的路径、以及容量/RAM 帽本身。

> **连接活动信号在 guest 侧量,不在 host 侧。** `sandboxd` 在输出/输入路径打一个**沙箱全局「上次 I/O」
> 时戳**,`Health` 探针带回 idle 时长(一处三用,§5.E / transport D4.5)。**不在 host 侧数据面代理处量**:
> 分离式进程模型下(§3.2)进程无客户端 attach 时仍持续产出到环形缓冲,host 侧看不到这些字节,会把干活的
> 沙箱误判成 idle。判据是 **I/O 活动**而非 CPU 占用(纯 CPU-bound 零 I/O 不计,是 idle 定义本身)。
> 完整推理与分类器/单飞/周期对账的落地见 [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md)。

三个关键区分,都靠"两层探活 + 连接活动"三者交叉判,缺一会误判:

- **dead-recoverable 与 idle 表现相同**(都"无活跃连接"),必须靠 L1 而非连接活动来分——崩溃的
  沙箱绝不能被当成 idle 去走 auto-pause 的静止(它假定 substrate 活着、能停到一致点),它要走 resume。
- **transport-down 与 dead-recoverable 表现相同**(L2 都不通),必须靠 L1 来分——transport 崩溃是
  **进程内可恢复**(PID1 同 IP 原地重启,成本远低于 resume),错判成 dead-recoverable 会无谓地触发
  一次整链冷启换 IP。transport 崩 ≠ 容器崩,是 §5.E.1「init 与 transport 分进程」的直接结果。
- 进程活着本身不等于活跃;两层探活都通本身不等于 healthy(还要看连接活动)。

**resume in-flight 期挂起分类(single-flight 守卫)。** resume 进行中 guest 尚未 listening,L2 必然不通;
健康分类器**不走转换锁**(只读探针、不改态),照跑会误判成 dead-recoverable、空跑一次注定失败的 resume
尝试并发出误报。故 resume 是**每沙箱单飞**的 in-flight 操作,持有期间对该沙箱的健康分类挂起,直到首次
L2 探活成功翻成 `running`(时序见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5)。
**single-flight 只承重效率与观测洁净(不空跑、不误报);并发动词打架的正确性由 §4.4-B 全程持有的转换锁
兜底,不是这里。** 这是内存态操作守卫,**不新增 §4.1 状态**(仍四态)。

**陈旧性检测:placement/IP 即 epoch。** 沙箱冷启换新 netns IP、旧 IP 作废(§5.E),因此 **placement/IP
变更本身就是那个 epoch 信号**——拿着旧 IP/旧 placement 句柄去说话即判定陈旧,重新解析当前 placement
再连(§3.3"身份不编码 placement")。不新增独立计数字段,placement 的每次重绑天然携带"这是新实例"的语义。

**TTL / 驱逐**:每个沙箱带生命周期状态(§4.1 的 `creating`/`running`/`paused`/`destroyed`)
与 TTL;reaper 按 TTL 推进 `running`→(auto-pause)→`paused`→(evict)→`destroyed`。**TTL 是 per-sandbox
Binding**(§3.4):两个阈值——`idle_timeout`(`running` idle 超它 →
auto-pause)与 `retention`(`paused` 超它 → evict)——是沙箱控制面记录的字段,create 时可给、缺省落引擎级
默认(§1.1 单二进制配置),故记录里始终是 reaper 直接可读的具体时长。**取 0 = 该阈值不设期限**
(`idle_timeout: 0` 永不 auto-pause、`retention: 0` 永不 evict)。默认取值为纯调参(见
[`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D5)。
> **「等人工介入」有期限,不是无限期挂起。** `resource-terminated` **不是**回收的例外:`retention` 的
> 时钟从**死亡那一刻**(降级 CAS 落的 `UpdatedAt`)起算,超期照常 `evict`,连 Overlay 工件一并删。
> 三条理由:① 回收判据只有 refcount 与 TTL(见下条),把**死因**——一个描述**过去**事件的诊断字段
> ——加进驱逐谓词,是为一个既有旋钮已覆盖的场景加宽 reaper 契约;② 在不超卖的准入门(§3.4)下,豁免
> 会造出一类"永久占着 Overlay 字节**与准入门额度**、却永远不会再跑"的沙箱,这比泄漏更糟——门把它当
> 一份**活的承诺**;③ 需要无限期保留的场景不需要引擎加例外,`retention = 0`(无期限)这个既有旋钮就是
> 它,而 `PATCH`(§3.4)让它**事后仍可设**。故处置是:留 `paused`,给人一个 `retention` 长的窗口去
> 改帽 / Promote / destroy;窗口内无人介入即 evict。

**auto-pause 的锁内活动重读(竞态兜底)**:活动度不是 lifecycle-state,§4.4-B 的 CAS 比对不到它,故 sweep 的
「判 idle → 处置」窗口里沙箱刚变活跃时,auto-pause 的 CAS 仍会通过。为不误冻正在干活的沙箱,`auto-pause` 拿
转换锁后除 CAS 外**再重读一次活动信号**(当前 `idle_millis`),仍超阈才冻结、否则放弃本轮。处置由此建立在锁内
最新的「态 + 活动」双重快照上(推理见 [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D3)。
- **对象回收(uniform object model invariant)**:每个一等持久对象(sandbox / Overlay 工件 /
  Volume / Template / 挂载行)带**可判定的回收语义**(不是 reaper 的私有实现)。切成**普适**与
  **仅可变对象**两半:
  - **refcount 普适、且 derived**:回收看「有没有引用它的活行」,**不物化计数字段**(由引用行是否
    存在查询导出——单机权威 store 下与物化计数等价,见下条与 §4.4-C)。无 owner 列——只看
    refcount/TTL,不看"谁拥有"(§1.4)。连接(process)不是持久对象,不参与回收。
  - **显式生命周期状态机只属于会真正转换的可变对象(sandbox,§4.1 四态)**。**不可变工件
    (Template,及后续挂载行)用「存在即状态」**:行存在 = ready、删行 = destroyed,无中间态,
    故不设 state 列。**代价与硬约束**:存在即状态没有 not-ready 逃生舱,故**强制字节先行物化**
    (bytes-first,见 §5.B);有 state 列的对象(sandbox 的 `creating`)才可行先行、由 reconcile 兜底。
    **在途/可失败的构建是独立 build 实体**(将来 additive 一张 build 表,承载 building/ready/failed),
    不塞进不可变工件的存在性。
  Template lineage 最尖:被某沙箱引用作只读底层(create-from-Template / Promote 叠层 / fork 多实例
  都是这一种引用)时,refcount > 0,**绝不可删**。**挂载行的 refcount 直接承载 §5.C 的 Volume
  独占租约**(机制归 §5.C)。
- **引用-回收原子性(一条动态不变量)**:上面"refcount > 0 绝不可删"只是静态判据,还差一条把
  **reclaim-vs-reference 竞态**闭合的不变量——否则有经典 TOCTOU 窗口:GC 读到某 Template 层 refcount==0
  正要回收,同一刻一个新沙箱引用该层(每个 create-from-Template 都引用一个 Template 层,fork 只是被引用层
  恰是 Promote 出来的)。不变量:**「创建引用」(create/fork/Promote 引用 Template 层、建挂载行引用 Volume)
  与 GC「无引用 → 回收」的判定-删除,必须由同一个权威 store 串行仲裁**——直接复用 §4.4-C 的 store 原子
  提交契约,不新造机制。derived refcount 下这退化成**外键约束**:引用是一条指向目标的行,回收是对目标的
  一次 `DELETE`。于是无中间窗口:引用行先落则 `DELETE` 撞「被引用」拒删(转引一个活层)、`DELETE` 先提交
  则引用方插入撞「目标不存在」失败。**权威单机 store(§1.1)是前提**:引用与回收都是其上的行操作,单写者
  串行令二者不交错,**无需显式 CAS**——竞态从 schema 掉出来。且 Template lineage **无环**,故统一用 store
  权威的存在性判定,**不引入独立标记清扫器、也不需世代序号式 GC 屏障**(后者是无事务 store 的补偿逻辑,
  原子提交契约已闭合竞态)。**「被引用即拒删」是 store 接口的契约承诺**(seam,§1.3):SQLite 用 FK
  RESTRICT 兑现、Memory 用 Go 检查兑现,与 §4.4-C 原子提交同构——后端差异不漏进正确性。

### 4.4 转换的并发与失败语义

§4.1 的状态机是**命令式**的,命令式状态机有三道经典裂缝:「转换到一半失败留脏态」「两个动词并发
打架」「状态变了但事件没发出去(或反之)」。本引擎用单机嵌入式 store(§1.1)、以 placement/IP 作 epoch
(§3.3),这一节用三条不变量分别接住这三道裂缝——不引入 `failed` 中间态、不引入世代计数。

**四态(§4.1)不加失败态,这是模型的结论而非省略。** 逐个转换看,没有一个会卡在"跨子系统的中间脏态":

- **resume / create 冷启失败**:canonical 冷启链(§5.E.1,见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5)
  先物化 Rootfs/Volume/Network(①②③)、再拉起 runsc(④)——**挂载在 runsc 启动时从 OCI spec 取,
  运行中不可热改**(§2.5),所以 Volume 不是"起进程后再挂"而是拉起动作的组成部分,**不存在"进程起了、
  Volume 没挂上"的中间态**。失败只落在两处:①②③ 物化失败(还没碰 runsc,持久态分毫未动)、④/⑦ 拉起或
  探活失败。两者都**退回源态**(resume→`paused`、create→删记录),不留 `failed`。
- **pause 静止失败**:协作式静止点只是"尽量不截断 in-flight 写"的优化,非正确性前提——协作超时 → 降
  抢占式冻结 → 最坏强杀,任一路径都落到**合法的 `paused`**(与 §4.2 崩溃语义一致)。故 pause 无中间脏态。
- **destroy 删字节失败**:状态转换(标 `destroyed`)与字节回收(删 Overlay/Volume)**解耦**——转换是
  控制面动作,删字节由 refcount sweep(§4.3)异步重试。删字节失败不阻塞、不回退状态转换,destroy 天然幂等。

由此定三条不变量,分接上面三道裂缝(A 接失败留脏态、B 接并发打架、C 接状态与事件不一致):

**不变量 A —— 转换失败原子退回源态,回收半建 placement,不引入补偿事务。** 持久态只在 Overlay 工件
与 data Volume(§4.2),**任何转换失败都不损坏它**。失败时唯一要清理的是**本次转换半建的 placement**
(netns/IP、egress gateway、nft、overlay mount)——而这本就是 §3.3「placement 每次 resume 重绑、旧的
作废」的一部分,不是新增的补偿逻辑。所以失败处置是**回收半建 placement + 控制面态回到源态**,不是
跨子系统的 saga/回滚。这也解释了为何不需要 `failed` 态:没有"损坏到需要人工介入"的持久态,只有"这次
placement 没建成、下次重来"。

**A 的崩溃变体 —— orchestrator 自身崩在转换中途,由重启对账补做同一次回收,不落 intent/phase 日志。**
上面的回收是进程内 defer 式,默认 orchestrator 活着能跑完;它自己崩在半途(如 resume 已建
netns/IP/nft/overlay-mount、store 还停在 `paused`)时 defer 跑不了,留下**孤儿 placement**。注意此时
**store 态并不含糊**——§4.4-C 的原子提交保证它只停在干净的源态或目标态、不撕裂;漏的只是宿主上那批
半建 placement-local 资源(非持久对象,§4.3 refcount GC 与 §4.4-C outbox 都不扫它)。接法**不是向前记
「意图+阶段」日志再重放**(那正是本节开头拒掉的持久分阶段事务),**而是反向对账**:placement
派生自 store 权威态、可弃,orchestrator 启动即枚举本机 placement 资源,凡**不支撑任一按 store 应持有
placement 的沙箱**(store 非 `running`、且未处于某次进行中 resume——后者靠 §4.4-B 转换锁辨识:in-flight
resume **全程持有转换锁**,持锁即在建,否则会误收正在建的活 placement)者即孤儿,按本不变量回收。
(**启动对账**下更简单:进程刚崩、锁全没了,一切进行中 resume 都已死,故半建 placement 的辨识无需持锁——
持锁辨识只在 orchestrator 活着的**周期对账**下才需要。存活 `running` 的活 placement 不是孤儿,另半段接管
见下。)单机权威 store(§1.1)令这次对账退化成**一次本地扫描 diff 一张本地表**,无阶段记录、与
placement/IP-as-epoch(§4.3)正交,**不重新引入世代计数**。半建的 workdir/upper 孤儿是其一个特例
(rootfs-overlay D5)。**周期对账**(orchestrator 活着时跑)= 启动对账**同一份反向对账**,只多两条:
① 对**持锁**沙箱 `TryLock` 跳过(持锁 = 在建的活 placement,§4.4-B),② 保护 live-map 里本进程亲手建的活
placement 不当孤儿。触发时机、锁协调、本机资源枚举与批量策略见
[`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D4。

**回收次序:活路径严格反向依赖序,启动对账力拆在前。** 活着的 `destroy`/`pause` 握着进程内 placement
句柄,按严格反向依赖序拆干净;**启动对账没有进程内句柄**、只有一张本地表,故对孤儿 mount 走力拆
(不阻塞在忙 mount 上),且**upper 分毫不动**(§4.4-A 持久态不碰)。两条路径次序刻意不同——活路径求
干净、启动对账求"一次扫描拆净";精确拆解次序(力拆/`ReclaimOrphans` 的 runsc→veth→netns)见
[`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D4。

**A 崩溃变体的另一半 —— 对存活的 `running` placement 重新接管句柄,不是回收。** 上面的反向对账只讲了
"回收孤儿";但一个 `running` 沙箱的**活** placement **本就不是孤儿**(它支撑一个 store 应持有 placement
的沙箱),不该被回收。崩溃后真正缺的是:orchestrator 的进程内 placement 句柄(端点、provider 私有 handle)
随进程蒸发了,而底层隔离单元还活着。**若只做回收、不补接管,后果不是脏态而是失联**:store 记 `running`、
容器还在跑,但引擎握不到句柄——此后对它的 destroy 会漏拆活容器(泄漏)、transport 代理会因"无活 placement"
失败。故对账是**对偶**的两半:**非 `running` 的半建 placement → 回收**(上一段);**`running` 的存活
placement → 重新接管**(provider 按 SandboxID 重认自己的资源、重建句柄、重发现端点)。**接管不是 resume**:
同一 placement、同一 IP,不换 epoch(§3.3/§4.3),因为隔离单元从未重启。仅当探活判定该 `running` 的隔离
单元**已死**,才按 §4.2「崩溃 = 非自愿 pause」CAS 降到 `paused`(其半建 placement 若有,归上一段回收)。
重认存活者要求 provider 能按 SandboxID 枚举自己的 placement-local 资源——其机制(`Adopt`/`ReclaimOrphans`
签名、SandboxID 标注是 §3.3「身份不编码 placement」的允许反向、两层判活 L1+一次 L2 `Health`)落在 §5.A,
本节不复述。编排语义是:**判活与冷启 ⑦ 就绪门对齐**——L1 活但 L2 不应答(如 `created`-未监听)不是可接管的
活,一律降 `paused`(可经下次 resume 冷启恢复),不把死句柄填进 live-map。**失败处置是 skip-and-log +
对不确定的 `running` 存活者取保守**(保护其 placement 不回收——漏拆活容器不可逆、泄漏 placement 下次
启动再收;对账幂等,跳过项下次重来)。接管期单次就绪探针与 orchestrator 活着时的**连续健康分类**(见 §4.3,
区分 transport-down 瞬态)、**周期对账**(与 §4.4-B 锁协调)的落地见
[`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D3/D4。

**不变量 B —— 每沙箱一把转换锁,动词串行 + CAS 前置校验。** 单机嵌入式 store 下仍有并发:用户
`pause` 与 `resume` 同时到、用户 `resume` 与 reaper `evict` 竞争、两个 `pause` 叠加。仲裁机制:

- **每沙箱一把转换锁**,所有生命周期动词(`create`/`pause`/`resume`/`destroy` + reaper 的
  `auto-pause`/`evict`)对**同一沙箱**串行获取,一次只有一个动词在推进它。
- **锁内先做状态前置校验(CAS)**:每个动词声明它的合法源态(如 `resume` 要求 `paused`、`pause` 要求
  `running`),当前 `lifecycle-state` ≠ 期望源态则**拒绝**,不执行。于是"pause 与 resume 同时来"= 后拿到
  锁者面对已变的态 CAS 失败;"resume 与 evict 竞争"= 谁先拿锁谁赢,另一个**阻塞等锁**,等赢家整条转换
  落定后再拿到锁、对**已变的态**重做 CAS(resume 赢则态已 `running`,evict 要求 `paused` 被拒)。
- **锁的持有区间 = 整条转换,不是只包 CAS 那一瞬。** 对 resume/create 这种冷启长动词,锁从 CAS 一直持到
  ⑦翻成 `running`(或超时 unwind 回源态)才释放——**不在 guest listening 前提前放手**。于是"谁在物化
  placement / 挂 Overlay"与"谁能改这个沙箱的态"落在同一把锁下:resume 正物化时,并发 `destroy`/`evict`
  拿不到锁,不可能去删一个正被挂载/冷启的 Overlay 工件。这条闭合了否则会敞开的数据竞态(destroy 从
  `paused` 合法、CAS 会通过,若锁已释放就会删掉 in-flight resume 脚下的字节——refcount 兜不住,因为
  in-flight 挂载不是持久引用行)。
- **归属**:锁与 CAS 是 §4 mediator 的编排义务,store 只提供 `lifecycle-state` 的**条件写(compare-and-set)**
  机制——不违反"子系统间不互调"(§4 开头),因为串行化发生在 mediator 层,不是让某子系统去调另一个。
- **与 resume single-flight(§4.3)的关系:锁承重正确性,single-flight 只承重效率。** 锁全程持有已独占
  整条 resume,**并发动词打架的正确性由锁兜底**;single-flight 是另一层——它挂起 in-flight resume 期的
  健康分类以免空转/误报(机制见 §4.3),**不承重并发正确性**。

**不变量 C —— 状态提交与事件发布单次原子,算法一份、后端差异收口在 store 契约后。** §4.1 每次转换要
发事件到 event stream,计费类 subscriber 靠它记账,**漏一条就漏账**。危险的做法是让"后端支不支持事务"
这个判断漏进发布算法、分裂成 N 套正确性。守法是 §1.3 seam/invariant 对偶的标准应用:

- **算法只写一份**:transactional outbox + at-least-once 投递 + subscriber 幂等去重。状态转换与"待发事件"
  记录**在同一次提交里落地**,发布器异步读 outbox 发出、发出后标记;事件带**转换唯一 ID**,subscriber 据此
  去重。宁可重发(at-least-once)不可漏发。
- **store 接口承诺,后端兑现**:store 是 Behavior seam(§1.3),它把算法需要的最小保证**收口成一条契约**——
  「`lifecycle-state` 变更 + outbox 记录追加 = 单次原子提交」。**接口方法本身承诺这条原子性,后端必须兑现**:
  支持事务的后端走本地事务,不支持多键原子的走「状态与事件写进同一文档/单键」降级。**绝不能设计成"后端返回
  是否支持事务、算法据此分支"**——那会把后端差异漏进算法逻辑、分裂正确性。换后端不许动这一份算法,这正是
  seam 的意义(register an implementation,不改现有结构)。
- **引擎不追踪 subscriber**:引擎只保证"事件被可靠写入一条可重放的 stream";断线补发、消费位点是 subscriber
  自己的事(§1.3 add a subscriber)。event stream 的对外 schema、重放窗口、subscriber 接入约定仍在细化,
  见 [`open-decisions.md`](./open-decisions.md) §2 的 §4.1 行。
