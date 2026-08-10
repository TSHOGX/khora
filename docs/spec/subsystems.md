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
- **5.D Network** — 每个沙箱可声明的网络可达集,以及兑现它的规则集(沙箱"能到哪、谁能到它")。
- **5.E Transport** — 宿主↔沙箱的通道:两个通用能力服务(process / fs),外加一个引擎自用的 `Health` 探针。

> Rootfs 与 Volume 分两个子系统:**Overlay 工件**生命周期**随沙箱**、引用某个 Template、就是
> pause/resume 的那份冻结层;**Volume** 生命周期**独立**、先于/晚于沙箱存活、自带管理 API 与后端。
> 二者都 ID 寻址,但**不共用一个物化 seam**——§5.B `rootfs.Provider` 物化分层合成(multi-lowerdir +
> upper/work + Promote)、§5.C `volume.Backend` 物化自足存储单元(直接 bind、无合成),只共享底层存储
> substrate 原语(btrfs subvolume / squota / reflink);回收也不同:Overlay 引用 Template 且随沙箱回收,
> Volume 谁都不引用、独立回收。

### 5.A Runtime(执行基底)

- **目标**:让单个沙箱持久、可变、可拿 root、可冷启 from Template。
- 提供 **runsc 进程级 start/stop 机制**(供 §4 的 create/pause/resume/destroy 动词驱动),
  物化三态模型(§3.1)。动词归 §4,本子系统只给机制(§4 开头)。
- guest 默认 **root**;引擎不建模 guest 用户,「以某用户运行」是 guest 原生的事(`su`/`sudo`),
  不是引擎原语(§2.2 / §3.2.1)。
- **资源上限物化(速率轴 + RAM)**:沙箱的速率上限(CPU;disk I/O 同轴、待有消费者时同形加入)与 **RAM**
  上限是 Binding(§3.4),由本子系统在每次 placement 时物化——single-node provider 落成 cgroup(CPU 的
  **上限与份额都写**:`cpu.max` + `cpu.weight`,二者同由 `cpu_millis` 派生,理由见 §3.4;RAM 落 memcg);
  别的 provider 落成其调度层的等价资源约束(份额对应其调度权重)。**disk 容量上限不在此**——它是存储层
  配额,per-sandbox 的归 §5.B(Overlay upper+work)、per-volume 的归 §5.C。
  - **disk I/O 速率轴落地时,整盘 MAJ:MIN 由 provider 自行推导**——与 RAM 的 overhead 同一形状:契约里
    只有速率意图,设备号是 provider 内部细节。**落地时** `local_runsc` 将从自己的 state dir 反查 backing
    整盘(`io.max` 要整块盘,给分区号则 launch 直接失败),该值全程不出本地 provider;远端 provider 用其
    调度层的等价物表达速率帽。**否决"宿主拓扑配置项"**:无论落进 Binding 还是落进 provider 入参,都把一个宿主原语抬上了
    跨 provider 契约面(下文 ❌ 清单)——设备号是彻底的宿主局部事实,远端 provider 根本产不出整盘设备号。
    该禁令由 `internal/runtime/seam_test.go` 的 token 守卫结构性钉住(`major`/`minor`/`majmin`/`devno`)。
- **RAM 帽的摊分是 provider 内部细节**:接口只说 `MemoryByte = N`
  (§3.4 Binding);provider 自己决定怎么把"N 字节 RAM"物化。`local_runsc` 内部把 guest 可见 RAM 设成
  N、宿主天花板设成 `N + overhead`(overhead 是**它的实现常数**,非契约成员——远端 provider 的开销
  构成完全不同:可能是 VM balloon、可能是调度层的 memory request)。**不这么切就是 spec bug**:两者
  都设 N 时 guest 被告知 N 却在低于 N 处被终止。它因此也**不进节点容量核算**(§3.4:核算式只计引擎能
  命名的字节)。终止的**确定性**(整组、非内核挑 victim)同属 provider 义务,见 §2.4。
  - **两条硬约束**(不守则帽形同虚设):① **limit 必须配 swap**——OCI `swap` 与 limit 相等
    (⇒ v2 `memory.swap.max=0`)才是硬帽,把宿主天花板抬到 `N + overhead` 后**必须再把 swap 钉回 0**,
    否则那段 headroom 又变成 swap-backed;② **不用 `memory.high`**——`swap.max=0` 加不可回收 anon 会把
    "节流"变成挂起,只用硬 limit + `oom.group=1`。
- **启动**:从 Template 冷启(秒级),可叠加现成 restore / 启动优化。容器 entrypoint 是
  `sandboxd`(init 模式当 PID 1,§5.E.1)——本子系统只负责"把它作为 PID 1 拉起",guest 内的
  init/reaper/re-exec 拓扑归 §5.E.1,不在此展开。
- **runtime provider 是本子系统的核心 seam**:"在哪儿、用什么跑一个隔离的进程上下文" 经一个
  可插拔 provider 接口表达。接口必须用**策略 + 句柄**而非**宿主原语**表达——否则把接口绑死在
  "宿主本地"假设上,将来任何非本地形态的 provider 都无法承接。**这条是全部存储/执行 seam 的通则,
  权威表述在此**(§5.C `volume.Backend` 同守,清单原样继承):
  - ❌ 泄漏宿主原语:"给我一个 cgroup 路径 / 一个 netns fd / Overlay 的**内部**宿主绝对路径
    (upper/work)"——一个被调度到远端 node 的隔离单元根本给不出这些。
  - ✅ 契约式:`(rootfs 来源, 可达性要求, 资源上限) → (placement 句柄, transport
    端点, 生命周期控制 stop/start)`。**`rootfs 来源`是一个不透明句柄**:`local_runsc` 下它恰好物化成
    一个已 merged 好的 rootfs 路径,远端形态下会是另一种句柄——被禁的只是上一条那个**内部** upper/work
    路径,已 merged 的 rootfs 句柄是合法入参。**可达性要求(契约成员名 `SteeringSpec`)**是 §5.D 从声明
    派生出来的**抽象规则集**:放行集 + L0 地板 + publish 端点描述符 + default-deny,**不是声明本身**
    ——provider 收派生结果,不持声明权威。**渲染成 nft 文本、以及绑定 `iifname` / guest IP / publish 宿主
    端点,都发生在 provider 内、在 placement 时**:那三样是 placement 的产物,也只有 provider 持有的
    宿主原语,抬上跨 provider 契约面就是上一条 ❌ 清单里的泄漏(精确划分见 §5.D)。
  唯一实现 `local_runsc` 把可达性要求渲染并物化成 nft + netns、把 §3.4 的资源上限经 **OCI `linux.resources`**
  下发给 runsc 落成 cgroup(cgroupfs driver)。provider 是**规则集与资源上限的物化方**,不持声明权威
  (§5.D)。
  - **强制机制的安装必须与 placement 原子,这是 provider 的可验收义务**(§2.4):规则装在**建 netns/veth
    之后、拉起隔离单元之前**;半建失败走 §4.4-A 回收。同一条义务延伸到 `Adopt` / `ReclaimOrphans`
    (见下文对账机制)——规则集是崩溃幸存的宿主态,接管一个活 placement 必须连它一起重建。
  - **谓词必须绑 guest 改不动的事实**(支柱④):`local_runsc` 落成**宿主端 veth 的入接口谓词**。
    按源地址匹配是**实测可被 guest 伪造**的(network.md D5 ③),故这是安全断言而非风格选择;放在沙箱
    netns 内部同样不成立(同处实测:guest 报文不遍历那些钩子)。**那个谓词必须写成 `iifname`(按名字),
    不是 `iif`(按 ifindex)**:`iif` 在装载时把名字冻结成一个整数,veth 同名重建后**静默 fail-open**
    ——实测包被投递而 drop 计数器为 0(D5 ⑫)。两者在 `nft list` 里长得一样,故这条靠渲染守卫而非评审。
  - **网络 placement 机制**:`--network=sandbox` 的 runsc **只 adopt 一个已存在的 netns、不自建**。故
    `local_runsc` 先建 netns + veth、**给 guest 端分配 IP**,再经 OCI `linux.namespaces[type=network].path`
    把 netns 交给 runsc;sentry 加入后在 boot 时把内核侧 L3 地址**收进自己的 netstack**(boot 后从内核侧读
    不到该地址,veth 链路仍在)。由此 guest-runtime D5.1 冷启链的 ⑥ 对 local_runsc **不是一次 boot 后探测**:
    launch 路径直接用引擎自己那次分配;`Adopt` 路径读**宿主端** veth 仍带的 /30 再 +1(分配把 guest 钉在
    host+1),故崩溃重启后端点可重建而无需问 guest。resume 每次**新建 netns**(§3.3 换新 IP),故无「复用
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
      (§4.4-A 对不确定取保守)。右侧仍是**策略 + 句柄**,过上面的 ❌ 宿主原语清单——死因是枚举,不是
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
    直透宿主内核,**违反 §2 substrate ①**(故否决,非"另一可选模式");且它是 §5.D 强制(per-sandbox
    netns + 宿主端 veth 上的 nft)与 §5.E transport(netstack-terminated,宿主经 veth IP 拨通)的前提。**代价:它拒绝
    `--rootless`**,故 orchestrator/launcher 须 root(或 CAP_NET_ADMIN+CAP_SYS_ADMIN)物化 placement——
    **sentry 自身仍非特权,支柱② 不破**;这是部署 caveat(§1.1「可跑在 CI/容器内」= 该环境须授予这些
    cap),不是隔离退让。
  - `--overlay2=none --file-access=shared` —— §5.B/§5.C 的持久层由 engine 在 runsc 外自管,gofer 只代理
    宿主目录。
  - `--net-raw=false` —— runsc 的默认本就掐掉 raw/AF_PACKET socket(OCI 里请求 `CAP_NET_RAW` 也拿不到,
    实测 guest `CapEff` 里该位是清的),故"guest 拿不到 raw socket"若不钉死就**只是一个未被声明的
    默认值**:一旦跨版本漂移,支柱④ 就要面对一个能伪造二层帧的 guest——入接口谓词赖以成立的正是
    "guest 改不动自己出现在哪个接口上",而没有任何测试会响。`CAP_NET_RAW` 因此也**不在请求列表里**——请求一个必然拿不到的 cap 是个假不变量,它让
    读代码的人以为那是刻意授予的。
  - `--systemd-cgroup=false` —— 承上条 cgroupfs driver。
  精确 argv 属 provider 实现细节,不进本节;本节只固定"哪些键承重、各对哪条 §保证"。
- **观测 CPU 用量要对 `cgroup.procs` 全体求和**:systrap 把 guest 线程跑成**独立的宿主 thread group**,
  只采 sentry 的 `/proc/<pid>/stat` 会严重低估。这约束的是量法,不是 enforcement(节流由 `cpu.max` 兑现)。
- **不含**:rootfs 构成与固化(→ B)、Volume 挂载语义(→ C)、网络声明本身(→ D)、
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
  的 Volume **也不在这棵树里**(它在各自后端,§5.C)——要守的性质是**"Volume 内容不进层、被挂载点遮蔽的
  既有字节也不进层"**,而不是"层里一定有个空目录":挂载点(如 `/home/workspace`)在固化结果里**或是一个
  空目录占位、或根本不存在**,两者都正确,别把"空占位"读成对两种情形的承诺(哪种情形给哪个结果,同处
  D5)。冷启不受影响——`runsc create` 会在 dest 缺失时自己把它物化出来,故 §5.B
  不需要补一个"造空目录"的步骤。
  - **挂载点集合必须作为 per-Promote 入参传进来,不能是静态包级清单**:Volume dest 是 per-sandbox 的
    挂载行,包级清单无从得知。这也让 **§5.B 永不必去问 §5.C**(不产生子系统互调)——§4 mediator 本来
    就持挂载表、本来就调 Promote。**代价预报走同一个集合**。
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
  它同守 §5.A 那条 **策略 + 句柄、不泄漏宿主原语**的 seam 通则(❌ 清单原样继承,理由不复述;结构守卫
  与豁免表见 [`deep-dives/volume.md`](./deep-dives/volume.md) D2)。
- **共享/独占策略**:`AccessMode ∈ {Exclusive, SharedReadOnly, SharedReadWrite}`(独占 /
  只读扇出 / 共享读写;后者只保证同沙箱内多连接(process)的 POSIX 语义,**跨沙箱 rw coherence 引擎不
  定义(undefined)**——gofer/sentry 各自缓存让跨隔离边界的共享可变一致性本质不可靠,引擎不承诺、
  用户文档诚实标出)。
  - **AccessMode 是挂载行的属性,不是卷的属性**(基数是 per-挂载,不是 per-卷):实测同一个卷可以
    **同时**对沙箱 A 是 shared-rw、对沙箱 C 是 shared-ro,故一个 per-卷字段无法诚实携带它。卷记录上
    只有 `cap_byte`(帽,wire 上叫 `size`)/ `backend` / `parent`(lineage,见下文快照);`access` 只出现
    在挂载行上(§4.3),`Materialize` 也不收它——**ro 的强制在运行时**(ro bind ⇒ guest 的写得 `EROFS`,
    一个干净的 POSIX 拒绝而非静默成功),**排他性在控制面**(挂载行),两者都在调用发生前已裁决,
    故都不需要 backend 知情(seam 侧的独立论据见
    [`deep-dives/volume.md`](./deep-dives/volume.md) D2)。
    - **"强制在运行时"只对"可写单元 + ro 挂载"这一向成立。** 反向——**不可变单元 + rw 挂载**——不是运行时
      `EROFS`,而是 `runsc create` **硬失败于 gofer 挂载**(容器根本没被创建,guest 从未运行)。故这条 ro
      规则的方向性必须显式:引擎绝不能让一个只读后端单元配上 rw 挂载规格,否则一个本该是准入期 4xx 的
      错误会退化成冷启期一个不透明的 mount 失败。设计上这条不会被踩到(快照产出的是**普通可写卷**,见下文),
      但它是关于基底的事实,不是关于我们代码的假设(实测见
      [`deep-dives/volume.md`](./deep-dives/volume.md) D1)。
  - 三种模式都**无特殊机制**、并发情形亦然(实测清单见
    [`deep-dives/volume.md`](./deep-dives/volume.md) D1)。
- **backend 两条臂,gofer 侧零差异**:**local(默认)**= 宿主上一个 **btrfs subvolume**(自带配额帽 +
  廉价原子快照),gofer 代理普通目录、POSIX 由宿主内核白拿;**remote**(S3 / NFS / …)= backend 维护的
  **宿主侧 FUSE 挂载点**,gofer 同样代理进 guest。**FUSE 只在宿主侧、绝不在 guest 内**(gVisor 内 FUSE
  支持有限),且封在该 remote backend 内、不进核心模型(为何不"全走 FUSE"、FUSE 定位见
  [`deep-dives/volume.md`](./deep-dives/volume.md) D1)。
- **未挂载也能操作**:create/delete/list/inspect/snapshot/clone + 直接 import/export
  内容。对外**一律 Volume ID 寻址,不暴露宿主路径**。
  - **兑现方式是 seam 上一个根限定的内容访问器**(`OpenRoot`),**不经 §5.E 的 `fs`**:路径相对卷根、
    越界不可达,local 臂 = `openat2(RESOLVE_IN_ROOT)`。为何不经 guest、以及机制的实测明细见
    [`deep-dives/volume.md`](./deep-dives/volume.md) D2 / D5.1。
  - **这条承诺兼答两个否则无解的场景,故它不是可选项**:**从零播种一个卷**(还没有任何沙箱时)与
    **所有沙箱都没了之后读一个卷**。二者都在"卷与沙箱生命周期解耦"(本节开篇)的直接后果集里——一个
    生命周期独立的对象,如果内容只能经沙箱进出,那它的独立性只是记录层面的。
- **写入 regime 唯一(横切不变量,防双写撕裂)**:Volume 有挂载与直连 API 两条写入路径,引擎守一条
  不变量——**只要有 live-rw 挂载正持有该卷,直连 API 的变更操作(`import`)一律拒绝**;
  无 live-rw 挂载时直连写才放行。门开在"**活的**写入方",不是"在挂载表里":**paused 沙箱不算 live
  writer**(容器已停、无活 gofer),此时直连写安全,且 resume 后自然见到新内容。不引入反向代理(证伪见
  [`deep-dives/volume.md`](./deep-dives/volume.md) D5)。
  - **这条不变量是双向对称的,两侧必须都设门。** 上面那句是从直连写这一侧陈述的,但它要防的危险
    (两个写入方同时写同一份后端字节,而 gofer/sentry 的缓存与宿主侧写入不一致)**与谁先到无关**。
    故反向同样成立:**只要有直连变更在飞,一次会给该卷 rw 挂载的冷启(create / resume)同样被拒**,
    退回源态(§4.4-A:失败原子退回,resume→`paused`)。只守一侧就等于没守——一个 T0 被放行的
    import 加一个 T1 完成的 resume,不经过任何 `paused` 态就造出了那两个并发写入方。
  - **两侧在同一个临界区里裁决**:live-rw 持有者集与直连写的在飞标记是**同一张按 volume 键入的互斥表
    的两半**,不是两个各自维护的检查(否则两个检查要靠人去保持一致)。与既有在飞构建 keep-set 同形,
    **不引新锁类**——尤其不引入按卷键入的锁:D3 实测那会闭合成环并死锁。
  - **冷启侧是拒绝,不是等待。** 直连写的标记按契约持**整条流**,时长随载荷无界;而冷启全程持有该沙箱
    的转换锁(§4.4-B),等待会把 destroy / pause / evict 一起堵在一个无界的上传后面。拒绝是可重试的、
    可解释的(remedy:等这次 import 结束),且落在 §4.4-A 已有的"退回源态"路径上,不新增失败语义。
  - **"live" 的谓词来源是 gofer 存活(placement live-map),不是 `sandboxes.state`。** 实测该状态列
    **双向**都不是忠实代理:冷启在健康门与 CAS-to-`running` **之前**就已记下活 placement(故记录说
    `creating`/`paused` 而 gofer 已活,窗口最长 `HealthTimeout`),pause 则先 CAS 成 `paused`、**之后**
    才停容器(故记录说 `paused` 而容器还在跑)。任何锁或 SQL 语句都修不了它——**被测的那个事实不住在
    store 里**。故裁决读的是 mediator 的 live-map,不是那个状态列(它与直连写的在飞标记同处上一条的互斥表)。
- **快照 / 克隆 = per-backend capability,默认 crash-consistent**:local 后端用 `btrfs subvolume
  snapshot`(整树、原子、瞬时)兑现,是否支持由 backend 的 capability 查询决定(纯目录 / 某些 remote
  后端可不支持,调用即 400、不静默)。快照是后端对当前盘上字节的原子快照、**不静默写入方**,一致性等价
  一次崩溃(§4.2 已接受的底线)。要 app-consistent 就先 pause 持有沙箱再快照——现有原语的**组合**,
  引擎不加机制;**shared-rw 不提供** app-consistent(协调 N 个沙箱是产品语义,§1.3 否决)。与 Overlay
  Promote 为何不对称,推导见 [`deep-dives/volume.md`](./deep-dives/volume.md) D6。
  - **挂载中的活卷可以直接快照,不降级、不打扰 guest**(实测:持续写入下 guest 写入速率不受影响、零写入
    错误,每个捕获点都落在宿主观测的活窗口内,append-only 日志在每次捕获里都是完整前缀;偶尔捕到一个刚被
    `O_TRUNC` 清空、还没写回的文件,正是 crash-consistency 该给的东西)。**不需要 `fsync`/freeze 前置**
    ——sync 无可测差别,而 freeze 与快照根本无法组合(D6.2)。
  - **快照不破坏 per-volume 帽语义:写入方付账,两个方向都是**(实测)。覆写源只记源账、快照那行停在
    元数据成本上不动;覆写快照只记快照账、源不动。三条后果:**一个没人写的快照卷不会自己涨到撞帽**、
    两个卷的帽互相隔离(源钉在帽上时快照仍可写)、删源后快照的计费不回溯继承被释放的 extent。
  - **快照 / 克隆产出的是一个普通的、可写的 Volume**,没有只读标记:可写性是 per-挂载的裁决(与 `access`
    同一条基数论据),要不可变就用 `shared-ro` 挂载行表达。两者因此是**同一种对象、同一条 backend 路径**,
    差别只在语义标注(推导与证伪见 [`deep-dives/volume.md`](./deep-dives/volume.md) D6.1a)。
  - **新卷的帽必须由引擎在同一次 backend 操作里显式写入,`size` 必填、不继承**:实测基底给快照的是一条
    **全新的无帽**配额行,没有可继承之物;而漏写是**静默且事后不可检出**的(`Usage()` 读回的"无帽"与故意
    无帽逐字节相同)。故 `size` 在 disk 被计量时对这两个端点同样必填(§3.4 / §6.2 同一条规则),缺失即 400。
    - 帽必然是快照 ioctl 之后的**第二次**调用,故"卷已在、帽未设"是一个真实中间态:它落在在飞集合里,
      且 **sweep / adopt 必须把"卷记录存在、配额行无帽"当作修复态,不是合法态**(D7.3)。
- **卷的容量帽在未挂载时也可编辑,且挂载中同样可编辑**(写点是 §6.1 `PATCH /v1/volumes/{id}`)。这与沙箱
  资源上限只在 `paused` 可改**正好相反**,理由是机制性的:沙箱帽在 placement 时物化,而**卷没有自己的
  placement**——"下次 placement 重物化"对一个未挂载的卷等于"永不",故卷帽直接落在其存储单元上,没有可推迟
  之物,也就没有"先 pause"的 409。三条附带纪律:
  - **改帽不受写入 regime 管**:它动的是分配上界、不是字节,不存在撕裂。(区别于**改 `access`**——那一条在
    活挂载下被禁,见下文独占租约。)
  - **抬帽须重过准入门**(§3.4:卷帽落在 disk 轴,对它的写入当场改变那条 Σ——门只在 **disk / memory**
    两轴上求值,CPU 不进任何节点不等式),可 507;降帽不需要。
  - **但降到低于当前用量必须拒(400,判据是严格不等式)**:实测这会把该单元卡死且**不可逆**——其后连
    `mkdir` 与"把已有文件 truncate 成 0"都 `ENOSPC`,而配额 ioctl 自己也 `ENOSPC`,帽既不能撤也不能抬,
    唯一出路是删 guest 的字节。引擎不能代调用方做那件事,故不接受这种请求(细节见 D7.3)。
- **挂载关系是 Binding(§3.4)**:"挂了哪些、挂到哪、什么策略"记在控制面挂载表,resume 时
  按当前挂载表**重新挂**回去,而非从冻结字节恢复——Volume 永远是那一个真身,不分叉副本。
- **整份绑定的替换是一次原子提交,由 store 契约承诺**(`ReplaceMounts`):§6.2 说一个沙箱**任何时刻**持
  一份完整绑定、`PATCH` 只整份换整份,那这条就不能实现成"先删后插"两次独立提交。**删必须在插之前**
  (一份重新提交的相同绑定否则会与它正在替换的那些行自撞:租约谓词与 `UNIQUE(sandbox,path)`),故解法
  不是换次序,而是把两步**收进同一次提交**。三条后果,每条都是两次独立提交守不住的性质:
  ① 失败时沙箱留在**原绑定**上,不会出现"新绑定被拒 + 旧绑定也没了"的空绑定;② 独占租约在替换过程中
  **不被短暂释放**,故并发 create 抢不走一个本次 PATCH 打算留住的卷;③ 写入 regime 的谓词
  (`ListMountsByVolume`)读不到"零挂载行"这个中间态,故并发直连写不会被误放行。
  - **这不是在 seam 上开事务**,而是 §4.4-C 那条纪律的第二次应用:**接口方法自己承诺原子性,后端各自
    兑现**(SQLite 走本地事务,Memory 在其既有互斥下做整体替换),算法只有一份、不按后端能力分支。
    契约测试两条:失去租约的替换保留**旧**绑定且可读;并发 `ListMountsByVolume` **永不**观察到零行。
- **独占租约 = 那条 Exclusive 挂载行的存在本身**:租约不是独立对象,而是 §4.3 挂载行(一等持久对象、
  derived refcount)的派生状态,故无 `Lease` 类型、无归属字段。**不变量的精确陈述**:一个卷上**至多一条
  `exclusive` 挂载行,且该行不与该卷的任何其他挂载行并存**。
  - **兑现在一条 store 语句里,判定与插入不可分开**:`INSERT … SELECT … WHERE NOT EXISTS`,
    `RowsAffected()==0` ⟹ 租约冲突 → 409。**mediator 的前置校验 + 插入不行**——那是一个跨沙箱不被任何锁
    序列化的真 TOCTOU(每次 create 新铸 SandboxID,两个并发 create 持两把不相交的转换锁),与准入门为何
    需要一把独立互斥是同一条理由。这不是新机制:同一个权威 store 串行仲裁**判定-动作**,与 §4.3
    引用-回收同形。语句、实测复现与锁序见 [`deep-dives/volume.md`](./deep-dives/volume.md) D3。
  - **`UNIQUE(volume) WHERE access='exclusive'` 偏索引 + `CHECK (access IN (...))` 是 schema 级断言,不是
    不变量的兑现方**:偏索引只禁"两条 exclusive"、**不禁 exclusive 与 shared 并存**(只覆盖一半);
    而它的谓词是**字面串比较**,故 `CHECK` 是必需的——大小写打错会让偏索引根本不适用,是一类静默失效。
    **谓词必须是行内不可变的属性**(此处 `access`),**不得建在可变的生命周期字段上**(理由与外部反面
    教材见 D3)。
- **挂载拓扑的两条唯一性断言,判 400 而非 409**:`UNIQUE(sandbox, path)` 与 `UNIQUE(sandbox, volume)`。
  - **同一沙箱内两个卷挂到同一 dest 必须禁**:挂载物化成一条 OCI bind,同一 dest 两条 bind 意味着后者
    **遮蔽**前者——于是一个卷在 guest 里不可见,却仍计进 §3.4、仍持着它的租约。"guest 看不见的卷上有一个
    独占租约"正是 schema 断言要抓的那类静默失效。
  - **同一个卷在一个沙箱内挂两次也禁**:独占租约的谓词(D3)已经在任一行是 exclusive 时拒掉同卷第二行,
    故不禁的话规则会随 access mode 分叉;统一禁掉让规则唯一。它没有已知消费者,而禁是一条校验——以后放开
    不破坏调用方,反之不然。(§5.C 上文"可挂到多个沙箱的不同路径"说的是多个**沙箱**,与此无关。)
  - **判 400**:一个沙箱任何时刻持一份完整绑定,`PATCH` 只是整份换整份(§6.2),不存在"往已有集合里加一条
    挂载"的操作。故每次重复都是**单次提交的绑定内部自相矛盾**——不读别的行即可判定,且没有任何重试或状态
    变化能让它变合法,这正是 §6.2 的 400 判据。嵌套(见挂载拓扑规则)同类,也是 400。**§6.2 的四个 409
    结局类别不增加**(类别 ≠ sentinel 个数:② 有卷侧与沙箱侧两条 FK 路径、两个 sentinel,仍计一类,见 §6.2)。
  - **路径必须在插入前规范化(绝对、cleaned、非 `/`),否则 UNIQUE 静默不适用**:`/data`、`/data/`、
    `/data/../data` 是一个挂载点三个字符串——与 `CHECK (access IN (...))` 必须存在于偏索引之侧是同一个
    陷阱(唯一性断言里的字面串比较**静默**失效)。**且 `UNIQUE(sandbox, path)` 抓不到嵌套**(`/data` 与
    `/data/sub` 是不同字符串),嵌套要靠校验里的前缀扫描,做不成 UNIQUE。两者都保留:校验先命中,断言是
    校验被绕过或写错时的那道网。
  - **`access` 没有独立写点,这是刻意的**:改 access 只能经整份绑定替换(`ReplaceMounts`),故它必然重过
    租约谓词。**不要为它开一个 per-mount 写点**——在活挂载下把 exclusive 翻成 shared,会让额外写入方进入
    一个"原持有者从不知道自己失去了排他"的卷,正是绕过上述断言的路径;真要开,须同时带上活挂载检查。
  - **崩溃保持租约是正确行为**——崩溃是 dead-recoverable(§4.3),沙箱会 resume 回同一卷,即时释放会让
    resume 撞车(§4.2)。租约**只在挂载行被拆时释放**(destroy,含 reaper 对 paused 超 TTL 的 evict),
    **reaper TTL 就是"无人释放"的兜底**,不引心跳;心跳式回收是 multi-node provider 的义务、不进核心
    (证伪见 [`deep-dives/volume.md`](./deep-dives/volume.md) D4)。
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
  - **打满时的行为与 §5.B 的 per-sandbox 帽一致**(guest 见普通 `ENOSPC`、沙箱照常存活、控制面不介入,
    §3.4),**并且对写入方的数量不敏感**:多个沙箱并发写同一个满卷时,每个写入方各自见到普通 `ENOSPC`、
    每个沙箱都存活、帽仍精确——但**字节在写入方之间的划分是任意的**,谁多写不确定,且一个写入方可以在
    对端已 `ENOSPC` 时继续成功。故两个方向都不承诺。
  - **三条用户可见语义(调用方会撞上,不是内部记账细节)**:
    1. **guest 无法发现自己的卷帽:`statfs` 报的是承载文件系统,不是帽**(实测 32 MiB 帽的卷上报 476 GiB
       可用)。故 guest 侧任何空间预检(装包器、包管理器、基于 `df` 的守卫)都会在 `write()` 失败前一刻
       报告几百 GiB 可用而做错决定。**引擎不承诺 guest 侧 statfs 反映帽**;一个能诚实投影的后端可以做得
       更好,那是该后端的义务、不是核心承诺(成因与外部旁证见
       [`deep-dives/volume.md`](./deep-dives/volume.md) D7.4)。
    2. **squota 下的帽是"终身写入预算",不是"活跃体积上限"。** 它**不退还被覆写的** extent(同一个
       32 MiB 文件原地重写两次记 64 MiB),但**退还被删除的**(unlink 后立即结算、退还恰好是文件大小)。
       故一个被反复原地重写的长命卷会在远小于其"当前体积"处撞帽,**收口手段是删除、不是覆写**。
    3. **`ENOSPC` 不是一道黏性的墙。** 实测多次:撞帽之后的**下一次**小写有时仍会成功,随后才稳定失败
       (机制与残余余量 + 释放失败写的超额预留一致,只测了行为、未测成因)。共享帽下对端的活动还会移动
       这条线。故"拿到 ENOSPC 就停"与"拿到 ENOSPC 后一次小写仍可能落地"是两份不同的契约,引擎承诺的是
       后者。
- **回收与 sweep 的安全要求(local 臂,squota 特有)**:残留有三类,对应**三个互相独立的入口**,只是在
  同一个启动位置与同一次 reaper tick 上依次调用——孤儿字节(`SweepOrphanVolumes`)· D7.3 的修复态
  (`RepairVolumeCaps`)· squota 泄漏的配额行(`ReclaimQuotaRows`);**别读成一个 sweep 的三个阶段**,
  三者的谓词、失败处置与可达路径都不同。三类都**误伤即静默**,故安全要求是契约的一部分。
  **职责切分是两层的**:**判定与调度归 mediator**——它才持有 live 集,且把判定推给 backend(让它收 live 集
  去删差集)实测**会删掉已提交卷的字节**,推导与两组实测数字见
  [`deep-dives/volume.md`](./deep-dives/volume.md) D2;**residue 的记账与枚举归 backend**——只有它知道自己
  究竟有没有配额行,`Backend.ListUnitIDs` 枚举它持有的存储单元、`Backend.DeletedRowIDs` 报出它在删除时
  记下的行 id——一个根本没有配额行的 backend 返回空,这条路径对它零成本(§1.3:调用方不长能力分支)。
  故下面每条安全要求约束的是 **mediator 侧的判定**,其依据的记账事实由 backend 供给:
  - **泄漏的配额行是正确性问题,不是不整洁**:一行泄漏的行不为自己的存储单元 id 占位,故一次节点重启
    可以把那个 id **连着它的帽**发给一个全新的卷,而引擎内部无法察觉这次错配。故 sweep 必须真的回收这些
    行,且**在启动时、任何存储单元被创建之前先跑一次**——否则一次重启可以在第一个 sweep tick 之前就把
    继承来的帽发出去。泄漏谓词、轮询谓词与回收程序见 [`deep-dives/volume.md`](./deep-dives/volume.md) D7。
  - **枚举必须按目录逐条探测,不得用文件系统级的 subvolume 枚举**:后者的作用域是**整个文件系统 / 所在
    存储单元**而不是给定目录(实测:在 volumes 目录与在兄弟的 overlay 目录下运行返回逐字节相同的结果,
    且含节点上其他软件的存储单元)。逐条探测无前置条件地安全,且是引擎既有 `internal/btrfs` 的形状。
  - **必须有一道独立于 FK 的活挂载检查**:跳过任何 id 出现在**任何**挂载行里的单元(不论是否 live);
    **枚举或读取出错时跳过,绝不把空结果当成"无拥有者"**(实测:并发 churn 下配额行的批量读取会整体
    失败并返回零行)。配额行**按删除时记下的 id 定位,绝不扫行**。
  - **一次误 sweep 没有任何检测路径,这是上面每条要求都是硬要求的原因。** 它是纯数据面事件——沙箱照常
    存活、**guest 的写仍然返回成功**、控制面什么都看不到(五种手法的实测分布与后果见
    [`deep-dives/volume.md`](./deep-dives/volume.md) D7.2)。连带一条**验收纪律**:"沙箱是否存活"在这里
    恒真,故它作为断言永远不会失败、毫无守卫价值;要断言的是那些**会静默通过**的性质。
  - **`Backend.Materialize` 必须是 create-if-absent**:若它是 open-only,一个被误 sweep 的沙箱将**永久
    无法 resume**;create-if-absent 则干净重启(数据全失但可恢复)。这也是它承担"卷的字节按需存在"的地方。
- **不含**:沙箱自身的 rootfs / Overlay 工件(→ B)。

### 5.D Network(可达性声明与其强制)

- **目标**:让每个沙箱的网络可达集**可声明、且被强制**。本子系统是那份声明的家,并生成兑现它的
  **抽象规则集**(渲染与安装归 §5.A,见下)。

> 本节的设计推导、逐条实测与被证伪的选项见
> [`deep-dives/network.md`](./deep-dives/network.md) D5,**本节不复制**。

- **本子系统只有两件事:持有声明,生成规则。** 它**不搬字节、不终结连接、不贡献任何进程**。判据是
  §1.1 第一性原则的直接应用——引擎该做的是那件"物理上只有它能做"的事(§2.4:强制机制必须与 placement
  原子、谓词必须绑 guest 改不动的事实),而"终结一条连接、按应用层信息决定它去哪"**不需要**创建
  namespace 的那一侧来做,故它不在引擎内。
  - **规则生成是纯函数**:`(声明, L0 地板) → 抽象规则集`——**placement-free、免 root**,故**声明取值
    组合的全部差异都能在这一层被单测钉住**,不必等 substrate。那份抽象规则集正是 §5.A 契约里
    `SteeringSpec` 承载的东西。**入参只有声明一个**:L0 地板是 `ruleset` 叶子持的静态事实,不作参数传入,
    故没有任何调用方能换一份地板进来。**输出是一张有序清单,每条规则自带钩子归属**——跨钩子的次序本身就是
    安全性质(`via[]` 的窄放行必须与地板 drop **同链**,D5 ⑩),故不能先按钩子切开。按钩子分组只发生在
    下游(渲染器与 `RuleSet.ByHook`),而单测钉的正是"每个钩子上各出了什么":同一条声明会在多个钩子上
    各产出规则(下一条),把两侧压成一张不带归属的清单,钩子差异就恰好从被钉住的那部分里漏掉。
  - **声明的放行在 `input` 与 `forward` 两侧都发,且 `input` 侧必须自带一条 sibling drop。** 理由是本节
    语义 ③ 自己的承诺:调用方要能声明自己的解析器,而唯一稳定可用的宿主本地解析器地址是**本沙箱自己的
    `/30` 网关**,它落 `input`——只发 `forward` 的放行**到不了它**,那条承诺就没有兑现路径;而 `input` 侧
    缺了那条 sibling drop,一条 `allow 10.0.0.0/8` 就会让本沙箱连上 sibling 的**网关**。**这与"地板在
    `input` 侧不需要 daddr 清单"不冲突**:地板在那一侧是一条 `iifname` 收窄的纯 ct-state 规则(`iifname`
    是它成为每沙箱规则的全部理由;它对宿主自身的任一地址已
    穷尽、从不枚举),sibling drop 是**另一条**规则、谓词是 sibling CIDR——两条规则,不是往地板上挂一份
    daddr 清单(推导与实测见 [`deep-dives/network.md`](./deep-dives/network.md) D4.2)。
  - **渲染并安装归 §5.A**:nft 文本的渲染,以及 `iifname` / guest IP / publish 宿主端点的绑定,都在
    provider 内、在 placement 时发生——那三样是 placement 的产物、是只有它持有的宿主原语,放进本子系统
    的签名里既跑不起来(placement 尚不存在)、又把宿主原语抬上了跨 provider 契约面(§5.A ❌ 清单)。
    规则集是 placement-local 资源,由 provider 在建 netns 与拉起隔离单元**之间**装上(§2.4 的原子性
    义务)。次序不是风格——见 §5.A `SteeringSpec`。

- **声明的形状:四个字段,全部只用 IP / 端口 / 协议词汇**(wire schema 见 §6.1):

  ```text
  egress.default    : deny | allow          # allow = 公网(减去 L0 地板与内网 deny-safe = RFC1918,§2.4)
  egress.allow[]    : {cidr, port, proto}   # 只收 IP 字面量,不收域名
  egress.via[]      : {match[], endpoint}   # match[] 同为 {cidr, port, proto};命中者送进 endpoint
  ingress.publish[] : {guest_port, proto}   # → host endpoint
  ```

  - **`{cidr, port, proto}` 是一个元组,不是三个可选项。** `proto` 恒显式,因为它是**格式要求**而非
    可选维度:承载它的内核机制里,端口匹配本身就依赖协议匹配(理由见
    [`deep-dives/network.md`](./deep-dives/network.md) D3.1)。故 `{cidr, port}` 不是良构的。
    - **`cidr` 与 `port` 各有一个通配取值,且都由"省略"表达;`proto` 没有。** `cidr` 省略 = **任意目的**
      (即 `via[].match[]` 的透明截获形态,D3.3),`port` 省略 = **任意端口**。两者在规则上是同一个机制:
      渲染器**当且仅当**该字段被指定时才发那条子句,故通配比指定**少一条子句**。两个方向都担着分量:给
      通配元组补上子句会**拒掉声明本就授权的流量**,而给已指定的元组漏掉子句会**授权该目的上其余全部
      端口**(逐条反例见 network.md D5 ⑩)。**`proto` 之所以没有通配,不是取舍而是机制**:上一条那个依赖
      使得少了它的元组根本渲染不出来。
    - **通配必须"写得出来",不能只是零值的副作用。** 判据就是下面 `egress.default` 必写那一条(空值分辨
      不出是谁选的),这里只是把它用在通配上。故
      **通配在两个面上都拼作"省略"**——读面渲染通配时**不发**该字段(而不是发 `"port": 0`),从而读面的
      输出仍是一份合法的写体(§6.1 的 read-modify-write)。**写面上 `"port": 0` 与省略都合法、且落库成
      同一个值**:两种拼法在**解码处就合一**,不会各自落进记录——§6.2 第八类(v4-mapped)拒的是**活到
      记录里、随后在每次 read-modify-write 上比较不等**的第二种拼法,这里没有那样的东西。
    - **`ingress.publish[]` 的 `guest_port` 是反面,零非法。** 它是 **DNAT 的目标**而不是匹配项,零没有
      可改写的去处;而元组的 `port` 是**匹配项**,省略即匹配全部。两者形状相反,故 `guest_port: 0` 落
      §6.2 的**良构性**那一类。
  - **没有"姿态枚举"。** 姿态不是引擎概念,它是这几个字段的**取值组合**:`default:deny` + 无
    `allow[]` + 无 `via[]` 就是"无出站";`default:deny` + 有 `via[]` 就是"只经某个汇合点"。少一个枚举,
    就少一处引擎与产品语义的耦合点(§1.1)。
  - **`egress.default` 必写,缺省非法**(与 `idle_timeout` / `retention` 同形):若"未指定"被解释成某个
    默认值,记录里的空值就分辨不出"调用方选了它"还是"这条记录建于本字段存在之前"。
  - **但"整份 `network` 省略"与"给了 `network` 而漏 `egress.default`"是两件事,前者合法。** create 上
    `network?` 可省(§6.1),省略即 **`{default: deny, allow: [], via: [], publish: []}`**,而引擎**在写点
    就把这份值显式落库**——记录里因此永远有一个被选定的取值,上一条要防的"空值分辨不出来"**构造上不存在**。
    这不是给上一条开例外:上一条治的是**记录里的空**,而这里落进记录的是 `deny` 这个确定值。**取 `deny` 而非
    `allow`**:它是 fail-closed 的那一侧,且它恰好是"没有网络"这个最小声明(§5.D 没有姿态枚举,姿态就是取值
    组合)。**为何不干脆让 `network` 必填**:每次 placement 都要按记录生成一份规则集,故这个字段没有"未绑定"
    态可言;而必填会让每个不关心网络的调用方**逐字写出**那份最小声明,换不到任何机制上的东西——绑定的完整性
    由落库时的归一化保证,不必由 wire 上的冗余保证。
  - **`via[].endpoint` 后面是什么,引擎不知道也不问。** 它是一个地址;引擎装一条把命中流量送去那里的
    规则,仅此而已。**故那个地址不是引擎常量、也不是引擎配置项**——它是声明里的一个值,由调用方给出。
  - **域名不收,只收 IP 字面量。** 理由是**代价而非不可行**——那条路要求引擎内长出一个 DNS 代理和一份会
    churn 的产品清单,破 §1.1(完整推理、以及"有人已经走通了"的旁证见 network.md D2)。要"只准某个域名"
    这种收窄,用 `via[]` 把流量交给引擎外的进程按应用层信息判。

- **`publish` 是入站的显式例外,且是唯一可热加的一项。** 它把沙箱内一个端口暴露成一个宿主 `ip:port`,
  是**纯宿主侧规则**、纯加法、不触及隔离单元,故可对运行中沙箱热加;而**出站三字段的变更走 resume 边界**
  (同 §5.C 改挂载表),是设计取舍而非技术限制。
  - **一条 publish 是一个规则集,不是一条规则**:`prerouting` 的 DNAT、一条把回程源地址改成**本沙箱自己
    `/30` 网关**的 SNAT、`output` 钩子上**另一条**DNAT、以及挂在**钩子正确那条链**上的伴随 accept
    (转换后的目标是 guest IP、要被路由出去,故落 `forward`,不落 `input`)。**四项缺一都不是"少一点功能"
    而是一个坏掉的 publish**:缺 SNAT 则只在恰好有默认路由的沙箱上能用,缺 `output` 侧 DNAT 则宿主自身
    拨不通(反之亦然,故"宿主上拨通了"不是 publish 生效的证据)。逐条实测见
    [`deep-dives/network.md`](./deep-dives/network.md) D5 ⑬⑭⑮,**本节不复制数字**。义务在此:**生成器把
    四项作为一个单元产出,provider 原子地装上**——中间不存在"只装了 DNAT"这种可观测态。**规则集变宽不
    改变上面那三条性质**:仍是纯宿主侧、纯加法、不触及隔离单元,而正是那三条性质在给热加背书。
  - **引擎交付的就是那个 `host ip:port`,前面摆什么都是引擎外的事**:TLS 终结、公网 hostname、证书、
    任何形式的调用方校验。**声明里因此没有"鉴权"字段**——校验一个入站调用方必然要终结并理解应用层协议,
    正是上面"引擎不贡献进程"排除掉的东西。(**按源 CIDR 收窄是个例外,因为它只是一条规则谓词、不需要
    进程**;它不在今天的四字段里,将来加是纯加法。)
  - **它因此是 Binding 写点唯一性的一条精确例外(§3.4:`PATCH` 是 create 之外唯一写点、provider 消费的
    字段只在 `paused` 可改)**:`POST …/publish` 只写 `ingress.publish[]` 这**一个**字段,且可写在
    `running` 沙箱上;出站三字段不在此列。理由就是上一条——纯宿主侧加法,不触及隔离单元,故不存在
    "记录说一套、活着的隔离单元做一套"。
  - **例外的是"可写在 `running` 上",不是"只写在 `running` 上"**:`paused` 同样合法,两态只差生效点。
    §3.4 那条状态门是**对 `running` 的收紧**(不接受一个要等未来某次 resume 才生效的编辑),`paused` 一侧
    对每个 Binding 字段都是开的;一个拒 `paused` 的 publish 端点会成为**唯一一个在 `paused` 比在 `running`
    更难写的字段**,把 §3.4 的结构倒过来而换不到任何机制上的东西。**`paused` 上"记录描述了一个不存在的
    入站口"不是新的假不变量**——placement 随 pause 释放,故 `paused` 沙箱的**整份**声明(出站三字段一并)
    本就一条规则都没装;记录描述得**比现存的多**是 `paused` 的定义,反向才是 §2.4 要拦的。**代价更实**:
    拒了 `paused`,给一个暂停沙箱加端口就只剩"先 resume"(一次冷启 + 一道准入门,可能 507)或整份 `PATCH`
    两条路,而后者正是下面点名危险的那个写点——收紧反而把调用方赶上更不安全的路。
  - **热加的项照样落进声明记录,它必须跨 resume 存活**:publish 不是一次一过的运行时旁路,下一次
    placement 仍按当前记录重新生成规则集。
  - **原子次序:先落库,再装规则;装不上就删回那行并返错。** 要守的不变量是**"不存在记录未描述的入站口"**,
    它是单向的,故两个半成品只有一个可接受:**"记录有、规则未装"= 功能缺口,fail-closed 且下次 placement
    自动补上**;**"规则装了、记录没有"= 一个引擎自己不知道的开放端口**,活到下次 placement。故次序不是风格。
    **全程持该沙箱的转换锁**(§4.4-B),故与并发 resume/pause 不交错;
    **引擎内那次写必须在锁内 read-append-commit**,否则两个并发 publish 会丢掉一条——那是引擎的 bug,
    不是下面那条调用方侧的 sharp edge 能覆盖的。
  - **基底的原子性只覆盖"装"这一侧,回删那一侧没有对等保护。** 装是原子的(实测:一批规则里有一条不合法
    则整批回滚),故上一条规则集里"只装上了一部分"不是个要处置的态。但失败路径是**装不上就删回那行**,
    那次回删跑在**拆**的一侧:它同样必须把四项**作为一批**发出,而若这批回删自己也失败,残留就是一份
    **半拆的规则集**——它的坏法是"连得上、读超时"这一类,即最难诊断的那种(D5 ⑰)。**处置是承认它并收窄
    承诺,不新增机制**:它落回的正是上面那个"规则装了、记录没有"的态,但是其中**最轻的一种**——残留只可能
    是引擎刚为一个**调用方明确请求过的** `{guest_port, proto}` 装上的那一份的**子集**,而规则集全是加法,
    故子集的可达性**严格窄于**全集:它开不出请求之外的任何东西,也活不过下次 placement(整份重建即清除)。
    **剩下的真实代价只在可诊断性**:引擎必须把这次失败的回删连同涉及的 `{guest_port, proto}` 记进日志,
    否则运维者面对的是一个"连上却没数据"的宿主端点,而控制面记录里没有任何对应项。
  - **重复 publish 同一 `{guest_port, proto}` 是幂等 no-op**(返既有那条,不追加第二项)。**去重必须落在
    声明层,基底不会替引擎发现它**:实测 nft 收下两条逐字相同的 DNAT 规则不报错、第二条恒不命中。而两条
    相同项之下,经 `PATCH` 删掉一条仍留一条,即"撤掉了 publish 而端口还开着"——安全相关字段上的静默失败。
  - **这次写不动 `UpdatedAt`**:`retention` 从那一列起算(§3.4 纪律 ②),否则给一个 `paused` 沙箱加端口会
    顺带延长它的驱逐期限。**这条义务只因允许 `paused` 才存在。**
  - **由此对整份 `network` 的 `PATCH` 有一条硬要求:必须 read-modify-write**——`PATCH` 只整份换整份,
    故一份用旧读回值拼出来的绑定会**静默丢掉**期间经 `publish` 加进来的项。这是**安全相关字段上的真实
    危险**(丢一条 publish 是可察觉的功能缺失,而反过来把一条本已删除的 publish 写回来则是重新开一个
    入站口),不是风格提醒;wire 侧的规则由 §6.1 / §6.2 承载。
  - **移除今天没有专用端点**:去掉一条 publish 走整份绑定 `PATCH`,故它与出站三字段同样**在下一次
    resume 生效**。**这不是对称性的疏漏,而是"撤"与"加"在机制上不对偶**:加是纯加法、当场对所有流生效;
    而**撤在 `running` 上撤不干净**——实测删掉那两条 DNAT 后新连接被拒,**但已建立的流仍在传数据**
    (conntrack 持着那份 NAT 绑定,规则删除对它不求值;逐条见 network.md D5 ⑰)。故一个 `running` 上的
    un-publish 只能承诺"不再收新连接"、不能承诺"这个端口关了",而两者的差别正是调用方撤一个入站口时想要的
    东西。走 `next-resume` 的整份 `PATCH` **没有**这个歧义(placement 整个重建,旧流随之消失)。专用端点要
    落地必须先答"撤的语义是否也要断开在途连接",那是一次独立的语义决定 + 一次测量,故不在今天。
  - **调用方侧的 read-modify-write 有一条次序要求:那次 GET 必须排在 pause 之后。** 两个写点在 `paused` 上
    **重叠**,故这个 lost update **不需要任何状态转换来穿插**——先 GET 再 pause 会把整个 pause 窗口纳入
    危险区。安全序列是 `pause → GET → PATCH → resume`。**契约就此接受 last-writer-wins 并写明,不引入
    乐观并发戳**:一个记录级版本号正是 §3.3 拒掉的世代计数(`generation` 在 §1.1 词汇边界上被直接禁)。
    这一类冲突有一条**纯加法的根治路径**——给 `ingress.publish[]` 一个删的写点,`PATCH` 从此不必承载这个
    字段,冲突**类别消失**而不是被检测;今天没有那个端点(见上条),故今天这是一条写明的 sharp edge。

- **四条用户可见语义,契约必须明写**(调用方会撞上,不是内部细节):
  1. **绑 `127.0.0.1` 的服务 publish 不到。** 沙箱有独立 netns 与独立 loopback,宿主侧的转发落到 guest
     netstack,只听 loopback 的进程收不到投递。这是标准 TCP/IP 语义、不是基底怪癖,但几乎所有 dev server
     默认绑 loopback,所以它会**看起来像 publish 坏了**。契约:要求 `0.0.0.0`,绑 loopback 时**连接被拒
     且快速失败**(不是挂到超时)。**契约到此为止,不承诺"错误可归因"**:实测这一情形与"guest 里根本
     没有监听者"在调用方侧**逐字相同**(两者都是 gVisor netstack 回的 RST,即 `connection refused`,
     皆亚毫秒级),故一个外部调用方**无法**区分二者(network.md D5 ⑯)。要归因只能在引擎侧——引擎知道
     自己给这个端口装了哪几条规则、并能读它们的计数器,而调用方只有一个 RST。
  2. **未放行的 UDP 被拒(不是被丢),故 QUIC 在 `default:deny` 下不通而非降级。** 拒法与 TCP 同源:
     面向 guest 的拒绝一律 `reject`,而 UDP 上只有 `icmpx port-unreachable` 被 gVisor 认
     (`admin-prohibited` 被忽略、仍挂数秒),故渲染取前者——回落因此**快速**发生而不是等超时。
     `via[]` 的匹配对 UDP 不成立
     (机制上取不到原始目标,见 network.md D3.2)。多数客户端回落 TCP,**但不保证**。这个失败方向比
     "未匹配就放行"更安全,代价是它表现为"某些站点莫名不通"。**这条排除只在出站 / `via[]` 方向**——入站
     `publish` 照收 `proto: udp`,DNAT 到一个固定 guest 端口不需要取回原始目标,那条机制反对到不了它
     (wire 与状态码见 §6.1 / §6.2)。
  3. **`default:deny` 下 DNS 要能用,调用方必须自己声明它——引擎刻意不代劳。** DNS 几乎全是 UDP,故上一条
     的代价首先落在它身上:只放行 TCP/53 时解析器**不会回落 TCP**,实测症状是先挂 10–20 s 再报一个
     **名字解析失败**(而非策略拒绝,D4.3 诊断错位的实例;数字见 network.md D5 ⑦)。故用法是把解析器地址
     连同 `proto: udp` 写进 `egress.allow[]`;**唯一稳定可用的宿主本地解析器地址是本沙箱自己的 `/30`
     网关**,而它的兑现路径是 `input` 侧的那条声明放行(上面规则生成义务里的那条,理由见 network.md D4.2)。
     **为何不代劳**:代劳就要引擎认识"解析器"这个概念并持有一份
     部署特异的地址,破 §1.1——与"域名不进引擎"同一条判据(network.md D2)。**哪些解析器地址实际可声明是
     部署环境的性质**:绑 loopback 的那类对 gVisor guest 构造上不可达,落在地板 daddr 项内的那类被地板拦下
     (实测逐条见 network.md D5 ⑦)。
  4. **`default:allow` + publish 组合起来是一条外部 → 沙箱 → 公网的双向通路**(即开放代理)。单看两个
     功能都合规,组合的语义要写明;**禁不禁是产品策略,不由引擎裁决**。

- **声明是 Binding(§3.4)**,记在沙箱自己的控制面记录上,每次 placement 按**当前记录**重新生成规则集、
  由 §5.A 渲染并装上。故它跨 resume 稳定、崩溃后由 §4.4-A 对账重建——规则和 netns 一样是崩溃幸存的宿主态。
- **"此处不需要授权主体"**:声明按**不透明的沙箱 ID** 键入,故本子系统**不为多租户预留任何东西**
  (§1.3 成本判据:那会是 reservation)。多租户仍是非目标,接入路径见 §1.4。
- **不含**:宿主↔guest 控制通道(→ E);任何应用层行为、任何凭证概念(§1.4 永久排除)。
  **别把本节读成"引擎里有个精简版网关"**——连接终结、协议识别、内容改写这类东西引擎侧不存在,承载
  它们的 seam 也不存在。

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
     `move`,外加**大文件流式旁路**(宿主侧 `OpenRead` / `WriteStream`,与 RPC 共用 guest 同一监听端口,
     阈值由调用方定,D4.4);**不做 watch**(消费方用 `read`+`stat` 自行轮询拉增量)。
  精确 RPC 清单以 proto 为权威契约(§6.1)。
  这个 daemon 是"CLI/TUI/WebUI 都是同一套 API 的平级 client"(§6)成真的具体载体。它**必须保持
  通用**:不内嵌任何 agent/turn/session 语义,那些是 engine 外 adapter 的事(见末条)。
- **载体:netns 内 TCP,不用 vsock**:`sandboxd` 在 guest netns 里监听一个 TCP 端口,连接由 gVisor
  userspace netstack(sentry)终结,宿主经该 netns 的 veth IP 直连——unary 走 HTTP、流式走 h2c
  (server-stream/client-stream + 即时 flush),保序、亚毫秒即时、大 payload 无死锁。冷启换新 netns IP
  (旧 IP 作废)零改动重连,**直接兑现 §3.3「载体不编码 placement」**。那个 veth IP 由 placement(§5.A)
  分配、非 runsc 产物;宿主如何拿到 IP 归 §5.A,不进本子系统。细节见
  [`deep-dives/transport.md`](./deep-dives/transport.md) D4。
  **端点的两半分属两个子系统:IP 归 §5.A(它分配),端口归 §5.E(`sandboxd` 监听它)**。§5.A 要拼出
  `IP:port` 就得知道端口,但**这不许成为 §5.A → §5.E 的依赖**(§4:子系统互不调用)——端口是一个共享
  常量,故落在一个**无依赖的 leaf** 里,两端各自依赖该 leaf、彼此不依赖。端口可跨沙箱恒定,因为每个沙箱
  自带 netns,消歧靠 veth IP 而非端口。
- **无状态**:约束的是**控制面持久 SoT**——engine 不为连接维护持久记录,"沙箱上现在跑着什么进程" =
  实时 `process.List`、不落控制面注册表。**它不等于"拨号无进程级资源"**:宿主侧客户端按端点构造、可随手
  丢弃(它没有可关的东西),但其下的 **HTTP/2 连接池是进程级共享的**——按 `IP:port` 键入,故换 IP 的
  resume 自然拿到新连接、退役端点的连接按 idle 超时回收。**共享一份池是正确性要求,不是效率优化**
  ——per-dial 建池的泄漏推理与实测见 [`deep-dives/transport.md`](./deep-dives/transport.md) D4.1。
  可重启性不由 engine 承担:裸 pty 字节流明说 lossy(shell 本无持久 SoT),任何"不丢"
  担保由上层自理。transport 进程内瞬时、非权威的输出环形缓冲(供 `Connect` 回放 + 慢订阅者非阻塞丢弃)
  不在此约束内,机制见 [`deep-dives/transport.md`](./deep-dives/transport.md) D4.3。
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
- **不含**:agent / 会话 / turn 语义(→ engine 外 adapter)、网络声明与其强制(→ D)。

#### 5.E.1 guest 启动拓扑:`sandboxd` 的两个进程角色

`sandboxd` 是 guest 内核心组件,承担**两个正交职责**,靠**同一个二进制、两个进程**切开——不合并
(合并会让最复杂的 transport 崩溃时导致整个 guest 崩溃),不引第二个二进制(守 §1.1
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
  - **"声明"与"收割"是同一个动作,不可分开落地**:声明了 subreaper 却不跑 `Wait4(-1)`,比根本不声明
    **更糟**——不声明时孤儿 reparent 到 PID 1 并被其 drain 收掉,声明了却无 waiter 则孤儿全部堆在
    transport 名下成为**永久僵尸**。且通配收割与 per-child 等待**不可共存**(实测细节与交接表形状见
    [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.3)。
- **transport 模式(子进程)**:就是本节的 process/fs 终点,**保持纯通用**、对 agent/turn/session
  一无所知。**定向信号**(单进程/进程组)归它(它握 pid/pgid 句柄);**容器级信号扇出**归 PID 1。
- **代价:退出码的送达有两条边界。** ① 跨 transport 重启 lossy:退出码保证**仅在 transport 单次存活期内**
  成立,跨一次重启退化为「`process.List` 仍可见、退出码可能丢」;② 存活期**内**,进程一退出其 pid 立即
  不可寻址——`end` 帧只发给退出那一刻在线的订阅者,此后 `Connect` 该 pid 是 not-found,**不留退出状态等人
  来取**(pid 由内核回收,留一条已退出记录在 pid 复用时就变成在讲另一个进程)。两条都与裸 pty 字节流的
  lossy 同源:engine 无状态,"不丢"担保由上层自理,不为它加内部状态。机制与 seam 见
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.5(C2 / C3)。
- **guest 侧故障注入不进生产二进制。** 测试要制造"transport 起不来"的崩溃循环,靠的是 guest 内一个哨兵
  文件;它**只在 `testguest` build tag 下编译**,生产 `sandboxd` 里根本不存在这段代码。理由是 §1.1 的
  guest 可拿 root、可写自己的 FS:一条编进生产的故障路径,等于让沙箱能靠 touch 一个文件停掉自己的
  transport。宿主侧的故障钩子(mediator 的 cold-start 注入)不受此限——那由引擎自己控制何时触发。
- **常驻服务(若模板自带)与 transport 模式 `sandboxd` 平级**被 PID 1 托管,不塞进 `sandboxd`
  (§3.2.1 引擎一视同仁看待每个进程)。
- **resume 冷启时序**(init→re-exec→监听→重连的精确次序、探活在哪一步判 healthy)见
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5。

> **细节另见 [`deep-dives/transport.md`](./deep-dives/transport.md)**:Connect-RPC 选型、process/fs wire
> 协议、session 划界与 seam、外沿投影(控制面 REST + 数据面 Connect,D5);**guest 启动拓扑的完整推理见
> [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4**。
