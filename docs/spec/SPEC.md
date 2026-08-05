# Sandbox Engine — SPEC

> 一个独立、可自托管、guest 内可自由配置的 Agent 沙箱引擎的**架构规格**(spec,非路线图)。
> 描述引擎**应当是什么样、边界在哪、契约如何**;落地排期(roadmap)不在本 spec 范围。

## 文档地图(全局 §编号跨文件连续,按此定位)

本 spec 按「变化速度 × 读者」拆成多篇,`§` 编号全局唯一、跨文件连续,是稳定寻址锚
(引用只写 `§3.3` 即可,不必关心它落在哪个文件)。

| 文件 | 覆盖 | 定位 |
| --- | --- | --- |
| **SPEC.md**(本文) | §1 定位与边界 · §2 安全模型 | 纲领,最稳,少改 |
| [`model.md`](./model.md) | §3 沙箱模型 · §4 生命周期 | 核心模型,中速演进 |
| [`subsystems.md`](./subsystems.md) | §5 子系统(A–E 目标定义) | 子系统边界 |
| [`api-contract.md`](./api-contract.md) | §6 API 与 CLI 契约 | 改动最频繁 |
| [`open-decisions.md`](./open-decisions.md) | 未定项登记 | 尚未拍板的选型 / 待验证假设 |
| [`deep-dives/`](./deep-dives/) | 挂某 § 的单点深挖(proposal) | 结论稳定后回写对应 § |

纪律见 [`CLAUDE.md`](../../CLAUDE.md):已定进本 spec,未定留 `open-decisions.md`,两边不重叠。

---

## 1. 定位与边界

### 1.1 定位

一个**持久、可变、可拿 root 的 agent 沙箱引擎**。隔离基底是可插拔的 runtime provider(§5.A),
今天唯一实现是 gVisor(runsc)。一个沙箱从
Template 拉起后,就是一个隔离的环境:运行时自由地 `apt`/`pip` 安装依赖、改文件系统,变更
**持久留存**;可随时暂停(释放 RAM/CPU、只占磁盘)再恢复;可同时挂多个连接(shell、各类
agent),共享同一 FS 与网络。

核心取向是**轻量、no-hypervisor**:不依赖 KVM、不依赖嵌套虚拟化——sentry 只是宿主上一个
普通的非特权进程,因此**能跑在那些拿不到硬件虚拟化的地方**(普通云 ECS、容器内、CI runner)。
目标部署形态是**单个 orchestrator 二进制 + 一份配置 + 本地 store**,无重型外部依赖。

> **gVisor 术语**(全文复用):**sentry** = gVisor 的用户态内核,拦截并虚拟化 guest 的 syscall;
> **systrap** = sentry 默认的拦截模式,用 seccomp 陷阱捕获 syscall(无需 KVM);**gofer** =
> gVisor 中代理 guest 文件访问的宿主侧进程。
>
> **本系统术语**:**engine** = 本 spec 描述的沙箱抽象本身;**orchestrator** = 承载 engine 的那个
> 单二进制;**控制面** = engine 内持有权威记录的那部分;**store** = 控制面记录(Binding、各对象
> 的 lifecycle-state/refcount)的持久后端,经接口抽象、可换实现;**可插拔实现 seam** 有三类,分别物化
> "用什么跑隔离进程"(**runtime provider**,§5.A)、"沙箱 rootfs 由哪些层合成、落在哪"(**rootfs
> provider**,§5.B)、"独立 Volume 落在哪个后端"(**volume backend**,§5.C);后两者是两个不同形状、
> 各自注册的接口,不是同一个 seam(为何分开、共享哪些 substrate 原语见 §5 前言);**driver** = 某个
> 具体 agent 的 CLI 二进制(claude-code / opencode / pi 等);**Overlay 工件** = 本沙箱相对 Template
> 的可写增量层(§3.1)。
>
> **五子系统 canonical 短名**(§5,代码包名 / 日志 / 评审一律用这些,不用章节号指代):
> **Runtime**(§5.A,执行基底)· **Rootfs**(§5.B,rootfs/Overlay)· **Volume**(§5.C)·
> **Network**(§5.D)· **Transport**(§5.E)。四处 `fs`/`FS`/`Rootfs` 刻意区分,勿混:
> **Rootfs** = 子系统 5.B;**`fs`** = §5.E Transport 的一个 wire 服务(`read/write/list/stat/mkdir/remove/move`);
> **rootfs provider** / **volume backend** = 上面那两个不同形状的存储物化 seam。同理 **Runtime**(子系统 5.A)
> vs **Sandbox**(§3.2 顶层持久单元)分指 "用什么跑" vs "哪一个沙箱"。

**引擎词汇边界(engine vocabulary boundary)。** 第一性原则(见 engine 根 CLAUDE.md)要求产品语义不进
引擎;命名阶段就拦住,比实现期再移除更省。下表限定**引擎内实体命名**(类型 / 包 / 字段 / API 端点),
不约束"描述引擎外的东西"时的普通行文——engine 讲清楚自己**不做**什么时,自然要提到这些词。

| 禁用作引擎内实体名 | 为什么 / 用什么替代 | 可作普通行文? |
| --- | --- | --- |
| `generation` | 引擎不引入独立世代计数;它想解决的四件事各有归属:**在哪** → Placement(§3.3)· **陈旧性** → placement/IP 作 epoch(§4.3)· **并发仲裁** → 每沙箱转换锁 + CAS(§4.4-B)· **失败恢复** → 原子退回源态 + 重启对账回收孤儿 placement(§4.4-A,故也无 `failed` 态) | 否(硬拦,防世代计数从别处渗回) |
| `principal` / `tenant` | 引擎内无授权主体 / 租户(§1.4 非目标);多租户是引擎外 join table 的加法 | 否 |
| `owner`(归属/授权义) | 沙箱无 owner 列,回收只看 refcount/TTL(§4.3 / §1.4) | 仅限**职责归属**义(如"idle 判定的 owner 是控制面")——那是"谁负责",非"谁拥有" |
| `session` / `turn` | agent 会话语义是引擎外 adapter 的加法(§5.E),引擎只有 process/fs | 可,且必然出现在"这些在引擎外"的论述里 |
| `agent` | 引擎不认识、不寻址 agent(§3.2.1);只有 sandbox/process/fs 原子 | 可(描述引擎外 / 产品层时) |

> **前提:部署在能运行 gVisor 的 Linux 宿主上**(能安装 seccomp 过滤器、能处理信号、内核版本
> 够新)。这是 systrap 的固有门槛,凡能跑 gVisor 的平台即满足,engine 当作既定前提,不再赘述。

> **两条底线:no-hypervisor + 可移植性,彼此独立。** 任何"用可移植性换性能"的决定一律**拒绝,或藏到
> provider 后面做成可插拔**(§5.A)——platform 即一例:`local_runsc` 把它 pin 成 `--platform=systrap`、
> 不吃 runsc 默认(默认跨版本会漂移),别的 provider 可另选。no-hypervisor 管"不要 KVM/嵌套虚拟化";
> **不存内存(§4.2)是可移植性的结果**,不是 no-hypervisor 的结果——gVisor 在 systrap 下本可 checkpoint,
> 我们不做,因为内存态锁 CPU-feature(§2.5),持久态便不能跨 host/CPU 搬(§1.5#3)。故 resume 是冷启秒级。

"在哪儿、用什么跑一个隔离的进程上下文"因此被抽象成可插拔的 runtime provider(§5.A)。这个接口存在的
意义是**保持抽象边界干净**,而非支持别的后端:唯一实现是 single-node 的宿主 runsc 进程,"被调度的远端
隔离单元"(K8s Pod / GKE Sandbox)仅作校准接口边界的纸面参照,不进落地路径(§5.A)。

### 1.2 聚焦(in scope)

- 持久、可变、可 root 的单沙箱;运行时自由安装依赖,变更落盘留存。
- 沙箱生命周期:创建 / 暂停 / 恢复 / 销毁 + 空闲自动暂停 + 到期回收(§4)。
- 自定义 Template:平台预置 + 用户从沙箱固化(Promote,引擎内部静止后读 at-rest 字节,§3.1)。
- Volume:与沙箱解耦的、ID 寻址的独立存储,可独立创建 / 操作 / 共享。
- 可定义网络:每个沙箱拉起时可指定网络可达性策略。
- 多连接传输:一个沙箱可并发挂多条连接(shell / agent / 工具),共享同一 FS 与网络。
- 一套干净的、sandbox-centric 的 API / CLI 契约(§6)。

### 1.3 扩展原则:对偶,指导当前怎么设计

这是一套**通用方法论**(任何系统都适用),§1.4 再把每条非目标当成它的一次应用。底线是
**单向依赖**:核心永不依赖扩展,扩展只单向依赖核心,不成环。其上是一条**对偶**:

- **约束面**:扩展只做加法(additive),不改现有结构。
- **使能面**:今天必须在**正确的关节**留好 seams(可加东西的接缝)与 invariants(横切保护);
  否则加法无处可落,扩展者被迫去改,约束面就破产。

**两类设计动作:**

1. **留 seams**——系统沿三条轴生长,每轴一个 additive-only 原型:

   | 轴 | seam 原型 | "加法" = |
   | --- | --- | --- |
   | **Behavior**(怎么做) | open registry(接口 + 可注册实现) | register an implementation |
   | **Data**(存什么) | external join table(ID 寻址) | add a join table |
   | **Consumption**(向外吐什么) | event stream(广播 + 订阅) | add a subscriber |

   这三条都是**引擎内**的 seam。还有一条**正交的引擎外轴**:有些扩展根本不进引擎,而是在引擎
   暴露的原语之上**叠一层 adapter**,或在 placement 层加策略。引擎外加法天然零侵入——它**够不到**
   引擎内部,无需引擎留任何 seam。所以下面的判据只约束**引擎内**扩展。

2. **守 invariants**——invariant 不"让你加东西",而"保证某核心属性不被污染",从而让 seam 真能
   容纳未来实现。(本系统具体要守哪几条 invariant、各自落在哪,见 §3.3 / §4.3 / §6.2。)

**成本判据(最易做错):**

> seam / invariant 是你为**当前实现**本就该做的好设计,顺便成为未来加法的落点;reservation
> (预留)是只为**假想未来**而加、当下一文不值的无用结构。**前者建,后者拒。**

微妙时序:**seam / invariant 现在建**(present 刚需),**join table 将来才建**(被 invariant
担保零侵入)。即:不留具体的"槽"(列 / 钩子),但要有正确的 seam 形状。

**Litmus test(每次建表 / 定接口前跑一遍):**

> 任何要**进引擎**的未来需求,都应能归到 **register an implementation / add a join table / add a
> subscriber** 三者之一。若三者都装不下、非改现有结构不可,先问一句:**它是不是根本不该进引擎**
> ——能在引擎外叠 adapter 就该叠在外面(引擎外加法不受此判据约束)。若它确实属于引擎内、却三者
> 都装不下,**才说明当前缺一类 seam 或某 invariant 没守住**,这是现在就该补的设计缺陷。

### 1.4 非目标:不做什么,以及靠哪个 seam / invariant 保持可加

明确划在边界外。先区分两类:**永久排除**(被某条底线否决,不会再加)和**今天不做、未来可零侵入
接入**(延期项)。

- **永久排除——内存态冻结/恢复(整族)**:pause/resume 不做内存快照。这不是延期,是被可移植性
  约束否决的(内存态锁 CPU-feature,§2.5)。**范围是整族,不只是"整个沙箱的内存镜像"**:单进程级
  checkpoint 受同一条约束(锁的是同一批 CPU-feature),故也永久排除——差别只在镜像大小,不在可移植性
  代价。我们提供的是**落盘 + 冷启逻辑续接**(§4.2),会话续接由 engine 外 adapter 在 process/fs 之上
  做(`--resume` + 读落盘历史)——它**不是这条非目标的"未来接入路径"**,而是替代方案本身。

下面是**延期项**:每条标出今天不做什么,以及未来靠 §1.3 的哪个 seam / invariant 零侵入接入
——这就是"不堵死"的精确含义:指向一个已存在的 seam 或一条今天就守住的 invariant。

| 非目标(今天不做) | 未来接入路径 | 担保它的 seam / invariant |
| --- | --- | --- |
| **warm pool**:不为 ms 级感知启动维持完全就绪的沙箱 | 复用"网络 / Volume 绑定经控制面在创建期注入"的流程形状,改为晚绑定(§6.2) | 注入流程形状(§6.2) |
| **授权主体 / 多租户**:不引入 owner / principal 概念 | 加一张 `sandbox↔principal↔capability` join table | Data join table + ID 寻址 invariant |
| ↳ 连带推迟 **微架构侧信道**(Spectre 类跨租户) | 多租户接入时,placement 层加 co-residency 策略(不共核 / 不共调度) | 引擎外 placement 策略(随多租户激活,§2 五支柱不变) |
| **多 node / 多 orchestrator HA / 跨宿主迁移**:聚焦单 node、单 orchestrator | 注册一个面向"被调度的远端隔离单元"的 provider(由其调度层承担跨 node) | Behavior registry(runtime provider,§5.A)+ 身份不编码 placement |
| **成本上限(max-lifetime)**:只按 idle-TTL + refcount 回收,不设与活跃度无关的硬上限 | 上层 quota 加一张表 / 一个 subscriber 消费生命周期事件 | Data join table / Consumption(§4.1) |

> 共性:这些"接入路径"**今天一行代码都不写**,只确保对应的 seam 形状在、invariant 守住
> (§1.3 成本判据)。

### 1.5 差异化定位(轴与 E2B 不同)

E2B 是**托管云**,卖一个跑在自有 fabric + KVM 上的高速专有 microVM;我们是**可嵌入、
no-hypervisor、可自托管、状态透明**的引擎。五条立足点:

1. **no-hypervisor**——跑在 E2B 物理上跑不了的地方(gVisor systrap、非特权进程、无 KVM)。
2. **自托管 / 数据主权**——单二进制 + 本地 store,代码/数据永不出自己的基础设施。
3. **状态透明可移植**——持久态是纯文件 Overlay 工件(§5.B),跨 host / CPU / runsc 版本可移、
   可审计;不是锁 CPU-feature 的 RAM 镜像。
4. **开放交互模型**——engine 只两个通用原语(process/fs),agent / 会话语义是 engine 外 adapter
   的加法(§5.E),开放性在 adapter 层而非锁进引擎。
5. **agent-runtime 作用域**——多连接并发 + swarm 拓扑在产品层(§3.2.1)+ 凭证留宿主(§5.D),
   正是 E2B 整个交给用户那一层。

详细竞品比对不在本 spec 展开,这里只钉**我们自己的边界**。

---

## 2. 安全模型

隔离边界由**宿主侧支柱**承担,与 guest 内部的自由度正交。放松的全部发生在 guest 内,
**宿主隔离完全不改动**。

### 2.1 五支柱:按"谁保证"切成两组

每条支柱写成 **抽象契约 + 今天的 single-node 物化**;并标注**enforcement 归属**——这一步直接划清了
"换 provider 会触及什么"。两组:**substrate-borne**(隔离基底本身保证,换 provider 会触及)与
**control-plane-borne**(权威在引擎控制面,与 provider 正交)。

> 下面会先用到几个后面才详述的词:**placement** = 每次 resume 重新绑定的物理资源(netns/IP /
> 容器 / cgroup / node,§3.3);**Binding** = 记在控制面、resume 时按记录重建的那类状态(§3.4);
> **runtime provider** = "用什么跑隔离进程"的可插拔实现(§5.A)。

| # | 支柱 | 抽象契约 | single-node 物化 | enforcement 归属 |
| --- | --- | --- | --- | --- |
| ① | **syscall 拦截** | guest 内任何 uid(含 0)的 syscall 都被一个用户态内核虚拟化拦截,不直透宿主内核 | gVisor sentry(systrap) | **substrate** |
| ② | **基底宿主非特权 + seccomp** | 跑隔离基底的那个宿主进程本身非特权,且被 seccomp 收窄;宿主看到的永远是一个受限进程 | sentry 以非特权 + seccomp 运行 | **substrate** |
| ③ | **网络策略** | 出站经受控 egress、入站显式 publish;策略权威在控制面 | 引擎自有 egress 数据面(gateway,行为 seam)强制策略 + provider 侧 nft 引流(§5.D);进程基数是物化细节 | control-plane(策略权威)· 引流物化归 placement |
| ④ | **凭证留宿主侧** | provider / model 凭证从不进 guest,guest 只见代理别名 | 凭证经引擎数据面在出站时注入,provider 不进 resolve 路径(§5.D) | control-plane(provider 不进 resolve 路径) |
| ⑤ | **受控的挂载面** | 挂进 guest 的东西经控制面裁决,guest 不能自己扩大挂载面 | 挂载表为 Binding,resume 按表重挂(§5.C) | control-plane(物化归 placement) |

**这张表就是"provider 安全义务"的全部来源**(收口成一句可验收的义务见 §2.4):换 provider 只可能触及
①② 与 ③⑤ 的物化层(对 ③ 即引流),**绝不触及 ④、③⑤ 的策略层、以及 ③④ 的数据面 enforcement**。

> **③ 的 enforcement 不是一整块落 placement**(见 [`deep-dives/network.md`](./deep-dives/network.md)):
> 策略强制 + 凭证注入归**引擎数据面**(§5.D 的行为 seam,provider 无关)、只有**引流**随 provider 变。
> 于是 ④ 的"provider 无关"精确为 **provider 不进 resolve 路径**——即便凭证密文物理流经它调度的基础设施,
> 它也不解析别名、不调 resolver、不持求值权。

### 2.2 guest 身份不是 security invariant

**guest 内 root 不是宿主隔离边界。** gVisor 里的 uid 0 是 guest 内的 root,被 sentry 虚拟化
(支柱①);sentry 自身在宿主非特权(支柱②)。所以 guest 是不是 root、有没有 capability,**只是
guest 内部卫生,不是宿主隔离支柱**——五条支柱无一以"guest 非 root"为前提。这也正是"自由安装依赖"
需要的(`apt` 要 root)。

由此:**默认就是 guest root**,不引入非 root 默认身份、也不预置切用户机制。既然它在隔离轴上一文不值,
root 默认就是 §1.1「可拿 root 的 agent 沙箱」最直接的表达,装依赖的权限也天然满足(§5.A;
[`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1)。**引擎不建模 guest 用户**:「以某用户运行」是 guest
原生的事(`su`/`sudo`,产品/模板层自理),不是引擎原语(§3.2.1);引擎既无 create 期"默认身份"字段,
也无 per-`Start` run-as 参数。放开 root 的代价落在 **guest 内数据面**(§2.3),这是接受的产品取舍。
将来若真有消费者要引擎级 run-as,再按 §1.3 additive 加回,不预留。

### 2.3 数据面取舍:精确列举可触及面 vs 不可触及面

放开 guest 身份(root + 可写 Overlay 工件)放大的是**且仅是"本沙箱数据面"这一个维度**。把边界列死:

**能触及(已接受的影响范围,落在本沙箱数据面内):**

- 本沙箱 **Overlay 工件的全部内容**——`/usr`、`$HOME`、运行时装的任何包(§3.1.1)。**含把自己
  的 disk 容量帽写满**:guest `dd` 打到 per-sandbox 容量帽(§3.4 容量轴)只让**自己**的 `write()`
  拿 `ENOSPC`,是数据面内的预期结果,不外溢。
- 按挂载策略挂进来的 **Volume**:各自遵 exclusive / shared-ro / shared-rw 语义(§5.C);root
  能触及的是"已挂给本沙箱的那些",不是 Volume 全集。
- egress gateway **放行的出站端点** + publish 暴露的**入站端口**(§5.D)——即网络策略本就允许的
  那一面。

**不可触及(宿主侧 / 跨沙箱 invariant,放开 guest 身份也不动):**

- 宿主 FS、宿主进程、**其他沙箱的 Overlay 与 Volume**——被支柱① syscall 拦截隔离在 guest 外。
- **其他沙箱的网络终点**——一个沙箱的网络可达集恰好是 {egress gateway,策略显式放行的端点},
  **sibling 沙箱不可达**(除非经显式策略且经 gateway 中转,非 L2 直连)。**这条不靠支柱① 守**:①
  拦 syscall、不拦网络包(guest 合法用网络),故跨沙箱网络隔离是**独立的 control-plane invariant**,
  由 §5.D 策略 + provider steering 物化,与"存储跨沙箱隔离靠支柱①"并列(见 network.md D5.5)。
  **⚠ 这一条今天不成立**:实测两个真沙箱之间 TCP 握手成功并读到载荷——点对点 `/30` veth 消除了二层邻接
  但**宿主仍在两段之间路由**,而强制它的 nft 地板尚未落地(open-decisions §4 / network.md D5.5 的修正)。
- **provider / model 凭证**(支柱④)——只在宿主侧,guest 永不见真 key。
- **控制面记录本身**(Binding 的权威,§3.4)——挂载面、网络策略、资源上限的取值由控制面裁决,
  guest 内 root 改不动(支柱⑤)。
- sentry 之外的**宿主内核攻击面**——被支柱① 限制在用户态内核内。
- **宿主盘的物理容量**——guest 写只吃自己 Overlay upper 的 per-sandbox 容量帽(§3.4 容量轴),吃不动
  整块盘,故撑不满整机、撑不垮其余沙箱的写。**但容量帽只管 guest 自己写的字节,不管内核代它写的**:
  故"确定且整组的终止"是本不变量的组成部分,不是调参——**它绕开容量帽**的那条路径见 §2.4。

一句话:**放大只发生在"本沙箱数据面"内;宿主隔离(①②④)与跨沙箱隔离不受 guest 身份影响**。注意
**资源耗尽维度不靠 syscall 拦截(支柱①)守**——①虚拟化的是 syscall、不是资源用量:CPU/RAM/IO 速率
靠 cgroup(§5.A)、磁盘容量靠 FS 配额(§5.B/§5.C),缺了容量帽,`dd` 就是一条合法 `write()` 撑满
共享盘、击穿本不变量。

### 2.4 provider 的安全义务(抽象,不绑定任何具体 provider)

把 §2.1 两组切分收口成一句可验收的义务,供将来评估任何 provider 用:

> **一个合格的 runtime provider,必须维持 substrate-borne 的 ①②(用户态内核拦截 syscall + 基底
> 在宿主非特权);对 ③ 它只负责一件事——把隔离单元的每一条 egress exhaustive 地引流进引擎指定的
> gateway、默认拒绝其余出站(逃逸即失败,不是降级);⑤ 它只按控制面下发的策略做 enforcement,不持策略
> 权威;④ 它**不进 resolve 路径**(§2.1 义)。策略强制与凭证注入本身都发生在引擎自有的 egress 数据面
> 里,不是 provider 的活。**

**RAM 上限的物化附带两条可验收义务**(§3.4 RAM 是安全限,超限后果是沙箱终止):

- **终止必须确定且整组**,不是由底层挑一个 victim。挑 victim 的形态既不确定(同配置多次运行结果不同),
  又可能落进"无可杀 victim → 无限重试"的状态,而重试路径**绕开容量帽**、可写满宿主盘(§2.3)。
  `local_runsc` 靠 `memory.oom.group=1` 兑现;远端 provider 靠其调度层的等价整组终止语义。
- **终止原因必须可判**,且跨 seam 时是**抽象枚举**而非 errno / cgroup 文件——否则控制面无法把它与
  崩溃分开,会去 resume 一个必然再次终止的沙箱(§4.3 `resource-terminated` 与 `dead-recoverable` 并列)。
  **provider 须在拆除自己的 placement 之前把死因交出来**,判不出即报 `Unknown`、**不许猜**(信号为何
  易失、为何必须当场落库、未知怎么兜底,全在 §4.3)。

**egress 强制的三条 invariant(承重墙是 IP 层,不是域名)。** ③ 的策略求值在数据面,但兜底强制靠一条对
**dial 时钉死的 IP** 求值的 **deny-set**——与凭证注入正交(推理、成员判据与逃逸分析见
[`deep-dives/network.md`](./deep-dives/network.md) D5.3–D5.6):

- **三档姿态**:**L0 地板恒 deny(code 强制,NetworkPolicy 抬不动)** = link-local(含 IPv6,天然覆盖
  所有云 metadata)/ loopback / orchestrator 自身控制面 / sibling 沙箱地址 · **内网私有段(RFC1918)默认
  deny-safe(config,业务可显式放行)** · **公网随 §5.D「默认放行」**。deny-safe 只收内网私有段,「默认
  放行」只对公网成立——引擎既不认识任何云厂商、也不侵入业务内网(§1.1 / §1.4)。
- **两跳都钉**:deny-set 同时施加于 guest→gateway steering(provider nft)**与** gateway→上游 onward
  dial(数据面);dial 时钉死解析 IP、禁连接期再解析。缺任一跳分别漏掉直连绕过 / DNS 重绑定。
- **egress 全程 fail-closed(per-connection)**:steering 失败断直连、injection 失败拒该连接;逃逸/降级
  即失败,只作用于失败的那条连接。

这条义务**不依赖任何具体 provider 形态**——无论"宿主本地进程"还是"被调度到远端 node 的隔离
单元",义务清单一字不变(后者只是把 ①② 的物化换到它自己的隔离机制上,把 ③ 的引流换成 CNI/sidecar)。
今天只兑现 `local_runsc`,但义务是按"任何 provider 都要满足"写的。**引流不可绕过(steering 必须
exhaustive)是一条可验收义务**:不写死它,guest 一条直连就绕过整个 §5.D 策略面(见
[`deep-dives/network.md`](./deep-dives/network.md) D5.1)。

> **⚠ 落地状态:本节的 ③ 与上面三条 egress invariant 是"已定、未建"。** ①②④⑤ 已被代码兑现,**③ 的
> enforcement 一行未落**——没有 `internal/network`、没有 nft/TPROXY、没有 deny-set。今天沙箱到不了外网、
> 兄弟与 link-local,**靠的是"没有默认路由"这一个事实,不是靠强制**;而 guest root 持 `CAP_NET_ADMIN`、
> 宿主 `ip_forward=1`,故 guest 自己加一条默认路由即可达宿主 LAN 与兄弟沙箱(实测)。故本节读作**义务
> 规格**(评估任何 provider 用),不是对当前代码的陈述。缺口、连带影响与 owning plan 登记在
> [`open-decisions.md`](./open-decisions.md) §4;**L0 地板怎么挣到、deny-set 住哪**是两项待决设计。

### 2.5 substrate 事实核查

这些是 single-node `local_runsc` 物化所依赖的 runsc 事实,直接影响上面几节的判断。持久化模型的
关键支撑事实(不存内存 ⟹ 持久态 = 纯文件层)及其下游附加能力在 §5.B 展开,这里只列 runsc 侧事实。

| 事实 | 对 engine 的影响 |
| --- | --- |
| systrap(默认)用 `SECCOMP_RET_TRAP` 拦 syscall,**不需要 KVM / 嵌套虚拟化** | 坐实 no-hypervisor(§1.1)的技术前提 |
| 内存 checkpoint/restore **要求跨宿主 CPU-feature 匹配** | 我们不存内存(§4.2),**此约束直接消失** |
| 在 guest 内的 iptables/nftables **仅部分支持** | 坐实"egress 用**宿主侧** nft 打在 per-sandbox netns 上"(§5.D),别下沉到 guest 内防火墙 |
| 挂载在**启动时**从 OCI spec 取,运行中不可热改 | 挂载拓扑变更 = resume 边界操作(§5.C) |
| runsc `--overlay2` 全族(`memory`/`self`/`dir=`)**均不跨容器生命周期持久**——`dir=` 启动即 `unlink` filestore,干净停 = 数据蒸发 | 持久层**不用 runsc 自管 overlay**;改由 engine 在 runsc 外用宿主内核 overlayfs 组装,runsc 收 `--overlay2=none --file-access=shared`(§5.B) |

> 载体的完整物化、CoW/一致性证据与替代路径归 §5.B(展开见
> [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md)),此处只锚定它所依赖的 runsc 事实。
