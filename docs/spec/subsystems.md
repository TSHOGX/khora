# Sandbox Engine — 子系统(§5 目标定义)

> 属 [SPEC.md](./SPEC.md);全局 §编号跨文件连续,导航见 SPEC.md 顶部「文档地图」。
> 五个子系统平级、只依赖 §3 共用模型,彼此不依赖——故合为一份。

---

## 5. 子系统(目标定义)

五个子系统**平级**,只依赖 §3 的共用模型(沙箱身份 + store),**不互相依赖**(防成环)。
本节定目标与边界;各子系统待细化的内部机制登记在 [`open-decisions.md`](./open-decisions.md)。

- **5.A Runtime** — runsc 进程生命周期、三态物化(沙箱"怎么跑")。
- **5.B Rootfs** — Template(只读底层)+ 可写 Overlay 工件;pause/resume 与 Promote 都挂在
  这里(沙箱"是什么")。
- **5.C Volume** — 解耦、ID 寻址、独立生命周期的存储(额外"挂了哪些独立数据")。
- **5.D Network** — 每个沙箱可定义的网络可达性策略。
- **5.E Transport** — 宿主↔沙箱的通道:两个通用 RPC 服务(process / fs)。

> Rootfs 与 Volume 分两个子系统:**Overlay 工件**生命周期**随沙箱**、引用某个 Template、就是
> pause/resume 的那份冻结层;**Volume** 生命周期**独立**、先于/晚于沙箱存活、自带管理 API 与后端。
> 二者都 ID 寻址,但**不共用一个物化 seam**——§5.B `rootfs.Provider` 物化分层合成(multi-lowerdir +
> upper/work + Promote)、§5.C `volume.Backend` 物化自足存储单元(直接 bind、无合成),只共享底层存储
> substrate 原语(btrfs subvolume / squota / reflink);回收也不同:Overlay 引用 Template 且随沙箱回收,
> Volume 谁都不引用、独立回收。

### 5.A Runtime(执行基底)

> **Runtime** = "用什么、在哪儿把沙箱跑起来"(§1.1 术语);核心 seam 是 runtime provider。

- **目标**:让单个沙箱持久、可变、可拿 root、可冷启 from Template。
- 提供 **runsc 进程级 start/stop 机制**(供 §4 的 create/pause/resume/destroy 动词驱动),
  物化三态模型(§3.1)。动词归 §4,本子系统只给机制(§4 开头"动词归 §4,机制归 §5")。
- guest 默认 **root**;引擎不建模 guest 用户,「以某用户运行」是 guest 原生的事(`su`/`sudo`),
  不是引擎原语(§2.2 / §3.2.1)。
- **资源上限物化(速率轴 + RAM)**:沙箱的速率上限(CPU;disk I/O 同轴、待有消费者时同形加入)与 **RAM**
  上限是 Binding(§3.4),由本子系统在每次 placement 时物化——single-node provider 落成 cgroup(CPU 的
  **上限与份额都写**:`cpu.max` + `cpu.weight`,二者同由 `cpu_millis` 派生,理由见 §3.4;RAM 落 memcg);
  别的 provider 落成其调度层的等价资源约束(份额对应其调度权重)。**disk 容量上限不在此**——它是 FS 层
  配额,per-sandbox 的归 §5.B(Overlay upper+work)、per-volume 的归 §5.C。
- **RAM 是安全限,其"一个旋钮/两个值"的摊分是 provider 内部细节**:接口只说 `MemoryByte = N`
  (§3.4 Binding);provider 自己决定怎么把"N 字节 RAM"物化。`local_runsc` 内部把 guest 可见 RAM 设成
  N、宿主天花板设成 `N + overhead`(overhead 是**它的实现常数**,非契约成员——远端 provider 的开销
  构成完全不同:可能是 VM balloon、可能是调度层的 memory request)。**不这么切就是 spec bug**:两者
  都设 N 时 guest 被告知 N 却在低于 N 处被终止。它因此也**不进节点容量核算**(§3.4:核算式只计引擎能
  命名的字节)。终止的**确定性**(整组、非内核挑 victim)同属 provider 义务,见 §2.4。
- **启动**:从 Template 冷启(秒级),可叠加现成 restore / 启动优化。容器 entrypoint 是
  `sandboxd`(init 模式当 PID 1,§5.E.1)——本子系统只负责"把它作为 PID 1 拉起",guest 内的
  init/reaper/re-exec 拓扑归 §5.E.1,不在此展开。
- **runtime provider 是本子系统的核心 seam**:"在哪儿、用什么跑一个隔离的进程上下文" 经一个
  可插拔 provider 接口表达。接口必须用**策略 + 句柄**而非**宿主原语**表达——否则把接口绑死在
  "宿主本地"假设上,将来任何非本地形态的 provider 都无法承接:
  - ❌ 泄漏宿主原语:"给我一个 cgroup 路径 / 一个 netns fd / Overlay 的**内部**宿主绝对路径
    (upper/work)"——一个被调度到远端 node 的隔离单元根本给不出这些。
  - ✅ 契约式:`(rootfs 来源, 引流要求, 资源上限) → (placement 句柄, transport
    端点, 生命周期控制 stop/start)`。**`rootfs 来源`是一个不透明句柄**:`local_runsc` 下它恰好物化成
    一个已 merged 好的 rootfs 路径,远端形态下会是另一种句柄——被禁的只是上一条那个**内部** upper/work
    路径,已 merged 的 rootfs 句柄是合法入参。**引流要求**是 §5.D 给的 ingress 描述符 + default-deny(把
    egress 全量引导进引擎数据面,§5.D),不是完整网络策略——策略 enforcement 与凭证注入在引擎数据面里,
    不在 provider。
  唯一实现 `local_runsc` 把引流要求物化成 nft+netns、把 §3.4 的资源上限经 **OCI `linux.resources`**
  下发给 runsc 落成 cgroup(cgroupfs driver)。provider 是**引流与资源上限的物化方**,不碰策略权威、
  不进 resolve 路径(§5.D)。
  - **网络 placement 机制**:`--network=sandbox` 的 runsc **只 adopt 一个已存在的 netns、不自建**。故
    `local_runsc` 先建 netns + veth、**给 guest 端分配 IP**,再经 OCI `linux.namespaces[type=network].path`
    把 netns 交给 runsc;sentry 加入后在 boot 时把内核侧 L3 地址**收进自己的 netstack**(boot 后从内核侧读
    不到该地址,veth 链路仍在)。由此 guest-runtime D5.1 冷启链的 ⑥「发现 IP」对 local_runsc 退化成
    **读回引擎自己的分配**——不做 boot 后探测。resume 每次**新建 netns**(§3.3 换新 IP),故无「复用
    netns 须重灌地址」问题。
  - **cgroup 归属:引擎建父层、叶子归 runsc**——runsc 只创建 `cgroupsPath` 的最后一段且要求父层已存在,
    **并且只对自己创建的叶子写限额**(引擎若预建完整路径,限额会**静默不生效而 launch 照常成功**);故引擎
    `mkdir` 父层 + 开其 `subtree_control`,叶子留给 runsc 建、`delete` 时由它回收,launch 后**断言限额确已
    生效**。父层是一个**扁平命名容器**、不是 kubelet-式 cgroup driver 层级(不代建每沙箱中间层,那才是反
    §1.1 重造那一层)。故一个**孤儿 CID-cgroup 就是孤儿 placement**的可扫信号,§4.4-A 对账兜底。
  - **对账机制(§4.4-A 崩溃变体的 provider 面)**:除 `Launch`/`Stop`/`Destroy` 外,provider 再出两个
    枚举/接管/回收机制,供 §4 mediator 的**启动与周期对账**驱动(同一份机制,动词仍归 §4):
    - `Adopt(SandboxID) → (placement 句柄, alive, 死因, err)`——按 SandboxID **重认**该沙箱自己的
      placement-local 资源、重建**不透明**句柄、重发现 transport 端点(**同一端点、不换 epoch**,隔离单元
      从未重启,§3.3)。三种结局互斥:**活且端点已恢复** → 交出句柄;**确已死/不存在** → `alive=false`,
      并**顺带交出死因**——这是 placement 尚完整的最后一刻,信号为何易失、判不出为何只能报 `Unknown`
      见 §4.3;**L1 活但端点恢复不了 → 报错,不是死亡**——
      provider 不得把"不确定"塌缩成 `alive=false`,否则 mediator 会去降级并拆掉一个证明活着的隔离单元
      (§4.4-A 对不确定取保守)。右侧仍是**策略+句柄**,过上面的 ❌ 宿主原语清单——死因是枚举,不是
      errno / cgroup 文件。
    - `ReclaimOrphans(keep []SandboxID)`——拆掉不在 keep 集里的全部 engine placement(反向依赖序 runsc
      kill/delete → cgroup 叶子 → veth → netns;overlay umount 归 mediator 侧,子系统不互调)。keep 由
      mediator 读**权威 store** 算出喂进来(provider 不反向调 store,§4 开头);持久态(Overlay upper)
      **绝不碰**(§4.4-A)。

    据此 **placement-local 资源须携带 SandboxID 标签**(`local_runsc`:netns=`sbxns_<id>` 可逆、
    cid=`sbxc_<id>`、veth=`vh`+SandboxID 的 sha256[:12] 单向派生),provider 才能枚举自己的资源(扫 netns
    前缀)并按 SandboxID 反查——这是 §3.3「身份不编码 placement」的**反向**(标注 placement 服务哪个
    SandboxID 允许,用 placement 反推 identity 被禁)。**判活两层**:provider 出 L1(`local_runsc` =
    `runsc state` ∈ {running,created})+ 端点,mediator 再补一次 L2 `Health` 探针才认作可接管(对齐冷启 ⑦
    就绪门,§4.3),探不通即降 `paused`。SandboxID 命名也让接管存活者时把其 IP 标进分配器 inUse,重启后的
    分配器不会把该 IP 再发给新沙箱。
- **`local_runsc` 钉死其依赖的整套 runsc flag、不继承默认**——每个键对一条既定保证,`runsc` 的默认值
  跨版本会漂移,而 §5.B 的**跨 runsc 版本可移性**不能建立在"默认不变"上:
  - `--root=<钉死的目录>` —— 所有 runsc 状态的唯一寻址根。钉错则 `state`/`list` 静默为空,§4.4-A 的反向
    对账会把活 placement 全读成不存在;故它是承重键,不是路径偏好。
  - `--platform=systrap` —— 兑现 §1.1 no-hypervisor;platform 本身可插拔(别的 provider 可选 KVM,§1.1)。
  - `--network=sandbox`(userspace netstack)—— `host`/hostinet 让 sentry 直用宿主 socket、syscall
    直透宿主内核,**违反 §2 substrate ①**(故否决,非"另一可选模式");且它是 §5.D 引流(per-sandbox
    netns+nft/TPROXY)与 §5.E transport(netstack-terminated,宿主经 veth IP 拨通)的前提。**代价:它拒绝
    `--rootless`**,故 orchestrator/launcher 须 root(或 CAP_NET_ADMIN+CAP_SYS_ADMIN)物化 placement——
    **sentry 自身仍非特权,支柱② 不破**;这是部署 caveat(§1.1「可跑在 CI/容器内」= 该环境须授予这些
    cap),不是隔离退让。
  - `--overlay2=none --file-access=shared` —— §5.B/§5.C 的持久层由 engine 在 runsc 外自管,gofer 只代理
    宿主目录。
  - `--systemd-cgroup=false` —— 承上条 cgroupfs driver。
  精确 argv 属 provider 实现细节,不进本节;本节只固定"哪些键承重、各对哪条 §保证"。
- **RAM 帽的两条硬约束**(不守则帽形同虚设):① **limit 必须配 swap**——OCI `swap` 与 limit 相等
  (⇒ v2 `memory.swap.max=0`)才是硬帽,把宿主天花板抬到 `N + overhead` 后**必须再把 swap 钉回 0**,否则
  那段 headroom 又变成 swap-backed;② **不用 `memory.high`**——`swap.max=0` 加不可回收 anon 会把"节流"
  变成挂起,只用硬 limit + `oom.group=1`。
- **观测 CPU 用量要对 `cgroup.procs` 全体求和**:systrap 把 guest 线程跑成**独立的宿主 thread group**,
  只采 sentry 的 `/proc/<pid>/stat` 会严重低估。这约束的是量法,不是 enforcement(节流由 `cpu.max` 兑现)。
- **不含**:rootfs 构成与固化(→ B)、Volume 挂载语义(→ C)、网络策略本身(→ D)、
  宿主↔guest I/O 通道(→ E)。

### 5.B Rootfs(沙箱由什么构成)

- **目标**:让"沙箱是什么"可持久、可冻结、可固化成可复用底座,且持久层**与 placement 解耦**。
- **结构**:rootfs = Template(只读底层)+ 本沙箱可写 **Overlay 工件**(§3.1.1),merged 作为
  可写 rootfs 绑进容器。`apt`/`pip` 与 `$HOME` 全写进 Overlay。
- **一条关键支撑事实**:**不存内存(§4.2)⟹ 持久态就是宿主上的纯文件层**——由此白拿跨 host / CPU /
  runsc 版本可移(§1.5#3)、可审计可 diff(宿主侧 `tar`/`diff` 即读)、以及下面的 Promote。
- **读 Overlay 内容只在 at-rest**:**只在 pause / stop**(容器已停、runsc 已 flush)读 Overlay 的**文件
  内容**(Promote 固化走的正是这条路),**绝不**在 guest 运行中读内容。因此"运行中经 gofer 缓存读到不一致
  状态"这类 live 一致性问题不在路径上。干净停掉后 upper 是宿主普通目录树,冷启挂回字节一致(§2.5)。
  **这条约束的是内容,不是容量计量**:准入门要在别的沙箱正在跑时求节点用量(§3.4),那读的是后端的
  **分配元数据**(已分配块数),不打开任何文件、与 gofer 缓存无关,故不受本条限制。两种读区分开:
  内容要 at-rest,计量随时可问后端。
- **载体:外层内核 overlayfs,runsc 不掺和写层**(§2.5;推导见
  [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md)):engine 在 runsc 外拼
  lower(Template)/upper/work,merged 交给 `--overlay2=none --file-access=shared` 的 runsc。**merged 的
  组装/卸载归本子系统**(runsc 只收一个已 merged 的 rootfs 路径,即 §5.A provider 的 `rootfs 来源` 入参,
  它不关心 rootfs 怎么拼出来);upper/work 须落在同一个支持 overlayfs 的 FS。
- **Overlay 经 rootfs provider 寻址,不假设宿主本地路径**:
  - single-node 物化:Overlay 落成宿主 overlayfs 的 upperdir、Template 落成 lowerdir,同机,
    pause 零捕获、resume 重挂同一 upperdir 冷启(§4.2)。持久单元是留驻 upperdir;见
    [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D3。
  - multi-node 物化:Overlay 落成可跨 node 寻址的层(PV / 对象存储),resume 在任意 node 挂回
    同一工件。模型层面 Overlay 始终是"引用某 Template 的、ID 寻址的冻结层",宿主路径只是
    provider 实现细节(§3.1.1)。
- **Promote**:把当前 Overlay 固化成新 Template 的底层——可写层降为只读底层,Template 因此
  天然分层(像分层镜像)。Promote 需要的仅是"at-rest 读到上层字节固化成一个新只读层",外层 overlayfs 的
  upper 是宿主普通目录树,直接读即可(§2.5)。**读的是干净停后的 at-rest upper**(§4.2 静止点:先 host 侧
  drain-barrier、超时降抢占式;协作式应用层安全点非正确性前提,substrate flush 本就保证)。
- **分层 Template = multi-lowerdir**:一摞只读层(像 OCI image),Promote 往上叠一层——新 Template 的
  lower 栈 = `[新层, ...父 Template 的 lower 栈]`,沿 lineage(`parent` 引用)展开。层怎么存/引用由
  **rootfs provider** 决定,位于 runsc overlay 机制**之上**;抽象不因存储形态而变(可选形态见
  rootfs-overlay D3)。
- **不可变工件字节先行(bytes-first)**:理由见 §4.3「存在即状态」——Template 无 state 列、行存在即
  ready,故其字节(新只读层目录)必须**先在后端完全物化、再插控制面行**。create-from-Template =
  引用既有层(字节早在);Promote = 先写层字节、后插 Template 行——crash 在中间只留**孤儿层字节**
  (可回收),绝不出现"行指向不存在字节"的破损 ready 态。**两个同形的孤儿 sweep**,都复用 §4.4-A 反向
  对账(不靠 FK 删字节)、都由一个 **in-flight 构建 keep-set** 保护在建者不被误拆(placement live-map 的
  同形):**孤儿层** = 无对应活 Template 行的层目录;**孤儿 Overlay 单元** = 无对应活沙箱行的存储单元——
  它接的是 §4.4「删字节失败不阻塞状态转换、由 sweep 异步重试」留下的那一半,记录已删而字节仍在,只能
  靠 sweep 回收。
- **Promote 捕获只含 Overlay 树,且排除引擎注入物**:固化沙箱 FS = 固化 Overlay 这棵树,但**引擎自己
  注入 guest 的运行时文件不进层**(`sandboxd` 的 portal / 哨兵文件,清单见 rootfs-overlay D5)。挂载进来
  的 Volume **也不在这棵树里**——挂载点(如 `/home/workspace`)在固化结果里只是一个**空目录占位**,
  Volume 内容不进去(它在各自后端,§5.C)。这跟 `tar` 一个挂了 NFS 的目录只 tar 到挂载点空壳是同一回事。
- **底层存储附带能力(single-node)**:snapshot/clone 落在 CoW FS(reflink)上廉价。这是 single-node
  provider 的附加能力,不是模型对宿主路径的依赖。
- **容量配额是本子系统的物化职责(disk 容量轴,§3.4)**:per-sandbox 的 disk 容量上限(Binding)由
  rootfs provider 在 placement 时施加到 **Overlay upper 子树 + work**(**两者落在同一个存储单元 ⇒ 同一份
  配额,无绕过路径**)。local 物化 = **btrfs subvolume + squota limit**;超限时 guest 拿到普通
  `ENOSPC`、**沙箱照常存活**(§3.4)。**帽兑现不了就拒绝这次 placement,绝不静默降级成"不设帽跑"**——
  否则 §2.3 的每一条断言都在相信一个不存在的帽;取值 0(无上限)则要把单元上残留的帽**清掉**,记录
  始终是唯一权威(§3.4)。为何是 squota 而非完整 qgroup、以及它带来的"Template 字节不受帽覆盖"代价,
  见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D6。
  **Template 只读、不被沙箱写,不设 per-sandbox 帽**——其增长由 Promote + refcount GC(§4.3)控制,
  只作节点容量核算的输入,是全局核算问题而非隔离问题。
- **provider 报"无帽覆盖的字节"(节点核算第二项,§3.4)**:seam 报出本子系统持有、且**无任何已声明帽
  覆盖**的字节总数——有帽的单元计 0(其帽 mediator 已计),物理共享的字节至多计一次。数字必须来自**后端
  自己的分配陈述**,**永不来自 guest**(sentry 伪造 `st_blocks`,guest 对稀疏文件报出按大小推算的块数而
  宿主真实值可为 0),也不用 apparent size(稀疏文件会无界高估,在准入门里等于**拒掉本可容纳的 create**)。
  契约用策略语言表述、不提 subvolume / qgroup / 目录遍历,故一个对象存储或网络 FS 后端的 rootfs provider
  同样可以兑现它(各自问自己的后端)。§5.C 同构——per-volume 帽进第一项、其无帽字节进第二项,
  **不新增第三项**。
- **不含**:Volume 的生命周期与挂载策略(→ C)、Overlay 跑在哪个进程上下文(→ A)。

### 5.C Volume(解耦的独立存储)

- **目标**:把"额外要独立存活的数据"做成一等公民,生命周期与任何沙箱解耦。
- **定义**:ID 寻址的持久存储单元,先于沙箱建、晚于沙箱亡、未挂载也在;可挂到多个沙箱的
  不同路径,**不进沙箱持久态 / 不进 Promote 捕获**,各自后端。
- **独立子系统、独立 seam `volume.Backend`**(不与 §5.B 共用物化 seam,见 §5 前言):它只吐**一样
  东西:一个宿主挂载点**(runsc 写进 OCI mount spec、gofer 代理进 guest,与 merged rootfs 同构)。
- **共享/独占策略**:`AccessMode ∈ {Exclusive, SharedReadOnly, SharedReadWrite}`(独占挂载唯一 /
  只读扇出 / 共享读写;后者只保证同沙箱内多连接(process)的 POSIX 语义,**跨沙箱 rw coherence 引擎不
  定义(undefined)**——gofer/sentry 各自缓存让跨隔离边界的共享可变一致性本质不可靠,引擎不承诺、
  用户文档诚实标出)。
- **backend 两条臂,gofer 侧零差异**:**local(默认)**= 宿主上一个 **btrfs subvolume**(自带配额帽 +
  廉价原子快照),gofer 代理普通目录、POSIX 由宿主内核白拿;**remote**(S3 / NFS / …)= backend 维护的
  **宿主侧 FUSE 挂载点**,gofer 同样代理进 guest。**FUSE 只在宿主侧、绝不在 guest 内**(gVisor 内 FUSE
  支持有限),且封在该 remote backend 内、不进核心模型(为何不"全走 FUSE"、FUSE 定位见
  [`deep-dives/volume.md`](./deep-dives/volume.md) D1)。
- **未挂载也能操作**:create/delete/list/inspect/snapshot/clone + 直接 import/export
  内容。对外**一律 Volume ID 寻址,不暴露宿主路径**。
- **写入 regime 唯一(横切不变量,防双写撕裂)**:Volume 有挂载与直连 API 两条写入路径,引擎守一条
  不变量——**只要有 live-rw 挂载正持有该卷(某沙箱 `running` 且 rw),直连 API 的变更操作
  (import / restore-into)一律拒绝**;无 live-rw 挂载时直连写才放行。门开在"**活的**写入方",不是
  "在挂载表里":**paused 沙箱不算 live writer**(容器已停、无活 gofer),此时直连写安全,且 resume
  后自然见到新内容。裁决在控制面(读 store),不引入反向代理(证伪见
  [`deep-dives/volume.md`](./deep-dives/volume.md) D5)。
- **快照 / 克隆 = per-backend capability,默认 crash-consistent**:local 后端用 `btrfs subvolume
  snapshot`(整树、原子、瞬时)兑现,是否支持由 backend 的 capability 查询决定(纯目录 / 某些 remote
  后端可不支持,调用即 400、不静默)。快照是后端对当前盘上字节的原子快照、**不静默写入方**,一致性等价
  一次崩溃(§4.2 已接受的底线)。要 app-consistent 就先 pause 持有沙箱再快照——现有原语的**组合**,
  引擎不加机制;**shared-rw 不提供** app-consistent(协调 N 个沙箱是产品语义,§1.3 否决)。与 Overlay
  Promote 为何不对称,推导见 [`deep-dives/volume.md`](./deep-dives/volume.md) D6。
- **挂载关系是 Binding(§3.4)**:"挂了哪些、挂到哪、什么策略"记在控制面挂载表,resume 时
  按当前挂载表**重新挂**回去,而非从冻结字节恢复——Volume 永远是那一个真身,不分叉副本。
- **独占租约 = 那条 Exclusive 挂载行的存在本身**:租约不是独立对象,而是 §4.3 挂载行(一等持久对象、
  derived refcount)的派生状态,故无 `Lease` 类型、无归属字段。建 exclusive 挂载行前控制面校验无冲突行,
  有则拒;store 侧靠一条 `UNIQUE(volume) WHERE access='exclusive'` 偏索引强制唯一。**崩溃保持租约是正确
  行为**——崩溃是 dead-recoverable(§4.3),沙箱会 resume 回同一卷,即时释放会让 resume 撞车(§4.2)。
  租约**只在挂载行被拆时释放**(destroy,含 reaper 对 paused 超 TTL 的 evict),**reaper TTL 就是"无人
  释放"的兜底**,不引心跳;心跳式回收是 multi-node provider 的义务、不进核心(证伪见
  [`deep-dives/volume.md`](./deep-dives/volume.md) D4)。
- **挂载拓扑变更发生在 resume 边界,不是热挂**:runsc 从 OCI spec 在**启动时**取挂载,运行中
  **不支持**热加/热卸(仅有 EROFS debug 旁路,不用)。所以"给运行中沙箱挂个新 Volume" = 改挂载表
  Binding → 冷 resume 带新表,与 §4.2 的 FS-first pause/resume 复用同一条路径。
- **挂载拓扑规则**:挂载点宿主目录归 Overlay 工件(沙箱 rootfs);**Volume 之间不嵌套挂载**
  (不把一个 Volume 挂进另一个 Volume 的子树),否则 Promote 捕获里挂载点占位的归属会含糊。
- **容量配额是 per-volume 属性,不是 per-sandbox(disk 容量轴,§3.4)**:一个 Volume 的容量上限
  记在它自己的对象记录上,由 §5.C backend 在其后端物化;它**不能归给某个沙箱**——shared-rw Volume
  被多个沙箱共写,占用无法拆到单沙箱头上。这与 §5.B 的 Overlay upper 帽(per-sandbox)是两个独立
  的容量帽,各自 enforce,但**共享同一个底层 substrate 原语**(btrfs subvolume + squota limit)——该原语
  跨 §5.B/§5.C 复用,见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D6
  与 [`deep-dives/volume.md`](./deep-dives/volume.md) D7。
- **不含**:沙箱自身的 rootfs / Overlay 工件(→ B)。

### 5.D Network(可达性策略)

- **目标**:做"自由"的网络使能者,同时守住安全。每个沙箱**拉起时可定义**网络策略。
- **职责切分(策略 / 数据面 / 引流,三块)**:本子系统**持有策略**(出站 allow-deny、入站 publish
  规则、model 路由 + 凭证路由);策略的**强制与凭证注入由一个引擎自有的 egress 数据面(gateway)承担**
  ——它是本子系统的一个**行为 seam**(可换 builtin / Envoy / 商用网关,与 provider 无关);**引流**
  (把 egress 引导进该数据面)是 **placement-local 资源**,由 §5.A provider 用 nft/netns/TPROXY(或
  CNI/sidecar)物化,与"资源上限是 Binding、cgroup 由 provider 物化"同构。所以 resume 时:**gateway
  由本子系统建/选,引流由 provider 建**;两者经 mediator 编排的两条薄契约(resolve / ingress)相遇、
  不互调(§4;详见 [`deep-dives/network.md`](./deep-dives/network.md))。
- **出站**:三档姿态与两跳 deny-set 由 §2.4 定死(承重墙是 dial 时对钉死 IP 求值的 deny-set,域名
  allow-list 只作 best-effort)。本子系统持这些规则的**策略权威**,provider 侧 nft 只放行到数据面 +
  显式端点。数据面的**进程基数**(per-sandbox / per-node)
  是物化选择、**非模型承诺**:`local_runsc` 落成 per-sandbox,别的物化可 per-node 复用一个数据面而仍按
  源 IP 强制 per-sandbox 策略。
- **入站**:默认关闭;**显式 publish** 把沙箱内端口暴露成外部入口,可带鉴权。publish 是**宿主侧
  规则**(nft DNAT)、纯加法、不触及容器,故可对运行中沙箱**热加**;创建期绑定的网络策略变更走 resume
  边界(同 §5.C 改挂载表),是设计取舍而非技术限制。
- model 访问 = gateway 上一条带 provider 凭证的出站路由,鉴权挂稳定沙箱身份(§3.3),
  凭证留宿主侧。
- **凭证生命周期(三层分解收口)**:凭证走的是和网络策略**完全同构**的三层分解,本子系统是它
  的家、不散落无主——
  - **不变量**(支柱④,§2.1):provider/model 凭证**从不进 guest**,guest 只见代理别名。这是
    宿主隔离的一根支柱,与 provider 形态无关。
  - **策略权威**(本子系统):"哪条出站路由用哪份凭证、注入到哪个上游"由本子系统持有;和
    "出站 allow-deny / 入站 publish 规则"同属 §5.D 的策略权威。
  - **注入执行**(引擎自有 egress 数据面):凭证在出站时**由引擎数据面(gateway)注入**,经一条传输
    无关的 **resolve 契约**(alias → 注入 directives + 过期 + 失效信号)拿注入材料,数据面持 TTL 缓存。
    **provider 不进 resolve 路径**(§2.1 义):即便凭证密文物理流经它调度的基础设施(远端形态),它也
    只是引流方、不是 resolve 参与方。resolve 契约的精确 schema 见
    [`deep-dives/network.md`](./deep-dives/network.md) D3.1 / [`open-decisions.md`](./open-decisions.md)。
    **resolve 失败 fail-closed,作用域 per-connection**(理由见 §2.4 /
    [`deep-dives/network.md`](./deep-dives/network.md) D5.6)。
  - 凭证明文**既不进控制面 Binding 记录、也不进 Overlay 工件**(§3.4 无第三类"可撤销外部授权";
    §2.3 凭证在"不可触及面")——它只存在于宿主侧一个独立后端。**这个后端(凭证怎么存/注册/轮转)
    是一类可插拔 seam,尚未定,登记在 [`open-decisions.md`](./open-decisions.md)。**
- **此处"鉴权"挂的是沙箱身份,不是用户/租户 principal**(后者仍是非目标,§1.4):publish 的
  `auth` 校验入站调用方、model 路由按沙箱身份注入凭证,都不需要引入授权主体概念。
- **不含**:宿主↔guest 控制通道(→ E)。

### 5.E Transport(宿主↔沙箱通道)

- **目标**:让宿主可靠地在沙箱里跑进程、读写文件,支持多连接并发。**engine 只暴露两个通用、无状态的
  能力服务(process / fs),外加一个引擎自用的 `Health` 探针**(§4.3 L2 + 就绪门 + 活动信号,不是对外
  能力面);两个服务自足 = 一个 E2B 式沙箱,agent 会话语义不在 engine 内(见末条)。
- **宿主侧客户端按这三块切开,不是一个大接口**:`Health` 探针 / process / fs 各自成一个接口,具体客户端
  同时兑现三者。判据是**各处只依赖自己真正用到的那块**:§4 mediator 全程只用 `Health`(就绪门 + §4.3
  分类器),数据面才是 process/fs 的消费方。判据的代价是可度量的:合成一个大接口,每个 L1 fake 就得
  实现十几个自己从不调用的方法,新增一条 fs RPC 于是牵动与 fs 无关的测试文件。
  (`Deps.Dial` 仍返回合并后的完整客户端——因为 mediator 要把它转交数据面,从而**端点解析留在 §4 内**、
  调用方只按 SandboxID 寻址、placement 不外泄(§6.2)。这一处宽度换的是那条不变量,值得。)
- **in-guest daemon(`sandboxd`,核心组件)**:连接在 guest 内的终点是一个**payload 无关的
  daemon**,只暴露两个**通用**能力,且**对 agent / turn / session 一无所知**:
  1. **process** —— 起进程 / 重连 / 输入 / 信号 / resize / 列进程;PTY 是带 pty 字段的特例(shell 就是
     带 PTY 的 `exec /bin/bash`),进程按 pid 寻址(guest 原生)。
  2. **fs** —— 读写沙箱文件:`read`(可带 offset)/ `write` / `stat` / `list` / `mkdir` / `remove` /
     `move`,外加**大文件流式旁路**(与 RPC 共用 guest 同一监听端口,阈值由调用方定,D4.4);
     **不做 watch**(消费方用 `read`+`stat` 自行轮询拉增量)。
  精确 RPC 清单以 proto 为权威契约(§6.1)。
  这个 daemon 是"CLI/TUI/WebUI 都是同一套 API 的平级 client"(§6)成真的具体载体。它**必须保持
  通用**:不内嵌任何 agent/turn/session 语义,那些是 engine 外 adapter 的事(见末条)。
- **载体:netns 内 TCP,不用 vsock**:`sandboxd` 在 guest netns 里监听一个 TCP 端口,连接由 gVisor
  userspace netstack(sentry)终结,宿主经该 netns 的 veth IP 直连——unary 走 HTTP、流式走 h2c
  (server-stream/client-stream + 即时 flush),保序、亚毫秒即时、大 payload 无死锁。冷启换新 netns IP(旧 IP 作废)零改动重连,
  **直接兑现 §3.3「载体不编码 placement」**。那个 veth IP 由 placement(§5.A)分配、非 runsc 产物;宿主
  如何拿到 IP 归 §5.A,不进本子系统。细节见 [`deep-dives/transport.md`](./deep-dives/transport.md) D4。
  **端点的两半分属两个子系统:IP 归 §5.A(它分配),端口归 §5.E(`sandboxd` 监听它)**。§5.A 要拼出
  `IP:port` 就得知道端口,但**这不许成为 §5.A → §5.E 的依赖**(§4:子系统互不调用)——端口是一个共享
  常量,故落在一个**无依赖的 leaf** 里,两端各自依赖该 leaf、彼此不依赖。端口可跨沙箱恒定,因为每个沙箱
  自带 netns,消歧靠 veth IP 而非端口。
- **无状态**:约束的是**控制面持久 SoT**——engine 不为连接维护持久记录,"沙箱上现在跑着什么进程" =
  实时 `process.List`、不落控制面注册表。可重启性不由 engine 承担:裸 pty 字节流明说 lossy(shell 本无
  持久 SoT),任何"不丢"担保由上层自理。transport 进程内瞬时、非权威的输出环形缓冲(供 `Connect` 回放 +
  慢订阅者非阻塞丢弃)不在此约束内,机制见 [`deep-dives/transport.md`](./deep-dives/transport.md) D4.3。
  同类瞬时信号:`Health` 探针带回的**沙箱全局「上次 I/O」时戳**(§4.3 idle 健康态的连接活动信号),进程内、
  随 transport 消亡、不跨 resume、不进控制面——量的是"此刻多久没 I/O",不是连接注册表。为何在 guest 侧量见
  [`deep-dives/reaper-health.md`](./deep-dives/reaper-health.md) D1。
- **session 是 engine 外的纯加法**:agent 会话语义(turn / 历史 / 恢复)**不在 engine 内实现**,而是
  engine 外 adapter 叠在 `process`/`fs` 之上:turn 循环在进程流上识别、历史用 driver-native 落盘(在
  Overlay 工件里,经 `fs` 可读)、恢复调 agent 原生 `--resume`。engine 一个新概念都不加——**开放性在
  adapter 层,engine 小而稳**(§1.3 / §1.5#4;ACP 形态映射与要守的 seam 见
  [`deep-dives/transport.md`](./deep-dives/transport.md) D1.2 / D3)。
- **引擎支持的"跑东西"只有一类:经 exec 拉起的进程**(monitor / 构建 / `apt`|`pip` 安装 /
  dev server / artifact server),**不以系统守护服务(`service`/`systemctl` 那套)为目标**。这条把
  "需要完整 init 机制"的整类需求移出范围(详见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md))。
- **不含**:agent / 会话 / turn 语义(→ engine 外 adapter)、网络策略(→ D)。

#### 5.E.1 guest 启动拓扑:`sandboxd` 的两个进程角色

`sandboxd` 是 guest 内核心组件,承担**两个正交职责**,靠**同一个二进制、两个进程**切开——不合并
(合并会让最复杂的 transport 崩溃时导致整个 guest 崩溃,也堵死热升级),不引第二个二进制(守 §1.1
单二进制部署)。runsc 以 `sandboxd` 为容器 entrypoint,**init 模式**启动当 PID 1,再经
`/proc/self/exe` **re-exec 自己进 transport 模式**当子进程:

```
PID 1 = sandboxd（init 模式：reaper + 信号转发 + subreaper 兜底 + 监督/重启）
  └── sandboxd（transport 模式：本节的 process/fs 终点，对 agent/turn/session 一无所知）
        ├── /bin/bash                 (process + pty)
        ├── agent 进程                 (process)
        └── apt / http.server / build  (exec 出来的子进程)
```

- **init 模式(PID 1)**:自研最小 init 语义(收割孤儿/僵尸、容器级信号扇出、监督并原地重启
  transport 模式),以 tini 源码为正确性 checklist,**不打包 tini 二进制**。它只当**兜底** reaper:
  transport 模式经 `PR_SET_CHILD_SUBREAPER` 声明自己为 subreaper,自己拉起的进程树的孤儿
  reparent 到它、由它收割,PID 1 只收真正的全局孤儿。
- **transport 模式(子进程)**:就是本节的 process/fs 终点,**保持纯通用**、对 agent/turn/session
  一无所知。**定向信号**(单进程/进程组)归它(它握 pid/pgid 句柄);**容器级信号扇出**归 PID 1。
- **代价:退出码的送达有两条边界。** ① 跨 transport 重启 lossy:退出码保证**仅在 transport 单次存活期内**
  成立,跨一次重启退化为「`process.List` 仍可见、退出码可能丢」;② 存活期**内**,进程一退出其 pid 立即
  不可寻址——`end` 帧只发给退出那一刻在线的订阅者,此后 `Connect` 该 pid 是 not-found,**不留退出状态等人
  来取**(pid 由内核回收,留一条已退出记录在 pid 复用时就变成在讲另一个进程)。两条都与裸 pty 字节流的
  lossy 同源:engine 无状态,"不丢"担保由上层自理,不为它加内部状态。机制与 seam 见
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.5 / D4.3 C2。
- **guest 侧故障注入不进生产二进制。** 测试要制造"transport 起不来"的崩溃循环,靠的是 guest 内一个哨兵
  文件;它**只在 `testguest` build tag 下编译**,生产 `sandboxd` 里根本不存在这段代码。理由是 §1.1 的
  guest 可拿 root、可写自己的 FS:一条编进生产的故障路径,等于让沙箱能靠 touch 一个文件停掉自己的
  transport。宿主侧的故障钩子(mediator 的 cold-start 注入)不受此限——那由引擎自己控制何时触发。
- **常驻服务(若模板自带)与 transport 模式 `sandboxd` 平级**被 PID 1 托管,不塞进 `sandboxd`
  (§3.2.1 引擎一视同仁看待每个进程)。
- **resume 冷启时序**(init→re-exec→监听→重连的精确次序、探活在哪一步判 healthy)见
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5。

> **细节另见 [`deep-dives/transport.md`](./deep-dives/transport.md)**:Connect-RPC 选型、process/fs wire
> 协议、session 划界与 seam、外沿投影(控制面 REST + 数据面 Connect,D6);**guest 启动拓扑的完整推理见
> [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4**。本节只明确边界,不展开细节(渐进式披露)。
