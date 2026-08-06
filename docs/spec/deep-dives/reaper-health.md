# Reaper 深挖:健康态怎么判、reaper 怎么据态处置、周期对账怎么与转换锁共存

> **定位:[`§4.3`](../model.md)(健康与回收模型)、[`§4.4-A`](../model.md)(崩溃变体 / 周期对账)的
> 深挖文档,并涉及 [`§5.E`](../subsystems.md)(活动信号载体)、[`§4.4-B`](../model.md)(转换锁)。**
> spec §4.3 定死了「两层探活 + 连接活动 → 健康态 → 处置互不相同」的形状、§4.4-A 定死了「反向对账 =
> 一次本地扫描 diff 一张本地表 + 对偶回收/接管」;本文展开那三件 spec 里点名「待细化」的机制:**①连接
> 活动信号从哪来、是什么;②健康分类器与 resume single-flight / 转换锁怎么不打架;③启动对账升级成
> 周期对账时,与转换锁的协调、批量与失败策略。**
>
> **编号约定:** 本文自己的节用 `D` 前缀;`§` 一律指 spec 全局章节,两套不混。

---

## D0. 一句话结论

reaper **不是独立回收器**,是**健康模型的消费者**(§4.3):先对每个沙箱判当前健康态,再据态调 §4
mediator 已有的动词(`pause`/`destroy` 的 reaper 变体)。它引入的唯一新机械是**周期后台循环**——引擎
第一个不靠外部 API 调用、自主推进状态机的 actor。三件待细化的机制各有一个正确的落点:

| 待细化 | 结论 | 落在 |
| --- | --- | --- |
| 连接活动信号 | **guest 侧、沙箱全局的「上次 I/O」时戳**,`Health` 探针带回 idle 时长;L2 不通时读不到、绝不误判成 idle | D2 / §5.E |
| 分类器 vs 并发动词 | 分类器只读、不持转换锁;对**持锁**沙箱 `TryLock` 跳过(在建/在转,不判) | D3 / §4.4-B |
| 分类→处置的活动竞态 | CAS 只比对 lifecycle-state、接不住"窗口内刚变活跃";故 `auto-pause` **锁内再重读一次活动**,仍 idle 才冻结 | D3 / §4.4-B |
| transport-down 收敛 | 瞬态等重启,但**连续超重启预算 → 升级 dead-recoverable 走 resume**,防崩溃循环卡死 | D4b / §4.3 |
| 死因怎么从分类传到处置 | 分类器只读且**丢弃** placement,故死因必须**随分类结果一起带出**、不能让处置再问一次 | D4c / §4.3 |
| 周期对账 | 与启动对账**同一份反向对账**,只多两条:跳持锁沙箱、保护 live-map 活 placement | D4 / §4.4-A |

---

## D1. 为什么活动信号必须在 guest 侧量,不能在 host 侧量

§4.3 `idle` 健康态的判据是「**所有**连接都 idle——无进程活动、pty 无 I/O」。天然的诱惑是在 host 侧量:
orchestrator 本就代理整条数据面(`processProxy`/`fsProxy`,transport D6),看着字节流过顺手打个时戳即可,
**零 guest 改动、transport 层不沾进程语义**。

**这条是错的,会误判成 idle 把干活的 agent 冷冻掉。** 根因在 §3.2 的分离式进程模型:`sandboxd` 的
`proc.run` **持续**把进程输出泵进每进程的环形缓冲,**无论有没有客户端连着**——这正是"进程比 Start 流
活得久、稍后 `Connect` 重连"能成立的前提。于是一个 agent 可以**跑几分钟、狂产输出、但零字节流过
orchestrator**(没人 attach)。host 侧时戳会把它读成 idle,`auto-pause` 掉一个正在工作的沙箱。这是
idle 定义层面的正确性 bug,不是调参能救的。而同一份活动对 guest 侧恰恰可见——每一帧输出都过
`proc.broadcast()`、每次输入都过 `writeInput()`,在**已经存在**的路径上极廉价地打戳。故信号在 guest 侧量。

**为什么是 I/O 而不是 CPU。** spec §4.3 的判据措辞是 I/O 活动(`process.List 有进程活动 / pty 有 I/O`),
不是「有 CPU 占用」。纯 CPU-bound、零 I/O 的进程(极少见于 agent 工作负载)不计入活动——这是 §4.3
idle 定义本身的选择;若将来证明需要,升级成「CPU+I/O 采样」是 §5.E 的 additive 加法
(healthServer 多读一份 `/proc/*/stat` jiffies),不改本文其余结论。

---

## D2. 信号的形状:沙箱全局单时戳,`Health` 探针带回

- **粒度 = 沙箱全局,不是每 pid。** reaper 的处置是**每沙箱**的(auto-pause 整个沙箱),不是每连接的;
  故只需一个「这个沙箱最近一次任意 I/O 的时刻」,取所有进程 + pty 的 max。一个进程内共享的 unix-毫秒
  原子时戳,`broadcast()` 与 `writeInput()` 各写一次。无锁、O(1)、不随进程数偏移。
- **搭 `Health`,不另造协议。** 探针本身(一处三用:L2 探活 / resume readiness 门 / 活动探针)定形在
  transport D4.5,本文不复制。这里只钉活动那一用的形状:`CheckResponse` 带 `idle_millis`
  (= `now - lastIO`),**与 L2「通不通」同一次 RPC 拿全**——分两个 RPC 是无谓往返。host 侧
  `Client.Health` 的域类型相应带回 idle 时长;冷启 ⑦ 门 / 接管 L2 探针只看 err==nil,忽略它即可。
- **仍是无状态(§5.E)。** 这个时戳是 **transient、进程内、随 transport 进程消亡**的——和环形缓冲同一类
  (transport D4.3「无**持久** SoT」),不跨 resume、不进控制面、不是连接注册表。它只回答「此刻这个 guest 多久
  没 I/O」,不记「谁连过、连了多久」。
- **`idle_millis` 只在 L2 通时有意义——L2 不通绝不可读成 idle。** 活动时戳搭在 `Health` 响应里,只有探针
  **成功返回**才拿得到 `idle_millis`;L2 不通(transport-down / 冷启中 / 死亡)时根本没有可信的活动读数。
  分类器据此严守次序:**先看 L1×L2 定健康态,只有落入「L1 活 + L2 通」才继续用 `idle_millis` 分 healthy vs
  idle**——`idle` 健康态的判据本就含「L2 通」(§4.3 表)。故"探不到活动"永远归 transport-down / dead-recoverable
  (按 L1 分),**绝不因读不到时戳而误判成 idle 去 auto-pause**。

---

## D3. 分类器与并发:只读探针,不承重正确性,持锁沙箱跳过

**健康分类器只读、不改态、不持转换锁**(spec §4.3「resume in-flight 期挂起分类」那段的直接落地)。它对一个
沙箱做的是:L1(`Adopt` 或对 live-map 已有句柄的端点直探)× L2+idle(`Health`)→ 健康态。判完若要处置,才由
**对应动词**去拿转换锁 + CAS——处置动作本身走 §4.4-B,分类不走。

由此两条守则:

- **对持锁沙箱 `TryLock` 跳过。** in-flight resume **全程持转换锁**(§4.4-B):guest 尚未 listening,L2 必不
  通,分类器照跑会误判成 `dead-recoverable`、空发一次注定失败的 resume 尝试并误报(spec §4.3 明列此坑)。
  故分类器对每沙箱先 `TryLock`:拿不到 = 有动词在推进它(在建/在转)= **跳过本轮**,不判不处置。这就是
  spec 的 single-flight 守卫,落成「非阻塞试锁 + 跳过」,**不新增 §4.1 状态**(仍四态)。
- **正确性由处置动词在锁内兜底,但分类→处置窗口有两条竞态,CAS 只接住一条。** sweep 判完 idle、轮到处置
  之间(单轮全量扫时可长达数秒)沙箱状态可能变。逐条看:
  1. **并发动词改了 lifecycle-state**(用户 `pause`/`resume`/`destroy` 插进来):`auto-pause` 拿锁后重做
     **CAS 前置校验**(要求源态 `running`),态已变则 CAS 失败 / 被串行化,不误处置。这条 §4.4-B 的锁 + CAS
     直接兜住。
  2. **沙箱刚变活跃**(窗口内用户发了输入 / 进程恢复 I/O):**CAS 接不住这条**——活动度**不是** lifecycle-state,
     态仍是 `running`,auto-pause 的 CAS(源态 `running`?)照样通过,会把一个刚开始干活的沙箱冻掉(反例:
     spec §4.4-B 的 CAS 只比对 lifecycle-state)。故 `auto-pause` 拿锁后**除 CAS 外须再重读一次活动信号**——
     取当前 `idle_millis`(一次 `Health` 探针;锁本就要持整条 pause,多一个 RPC 可忽略),仍 `≥ idle_timeout`
     才冻结,否则**放弃本次 auto-pause、下轮再判**。于是处置永远建立在**锁内最新的**「态 + 活动」双重快照上,
     而非 sweep 起点的陈旧分类。
  - 残留的亚微秒窗口(锁内重读之后、runsc 冻结之前用户恰好发输入)无害且不可消除:它与"用户在 pause 刚起步的
    瞬间发输入"无从区分,由 §4.2 pause 静止点语义正常处理,不算误暂停。§4.4-B「锁承重正确性、single-flight 只
    承重效率」在此精确为:分类器读到的是快照、可"看走眼"一瞬,而处置在锁内对**态 + 活动**同时重判才落定。

**为什么不让分类器持锁跑。** 若分类持锁,一次全量 sweep 会与用户动词长时间争锁(冷启动词持锁数秒),把
reaper 变成延迟源;且"只读探活"本不需要独占。分开是对的:sweep 廉价并发探,处置才短暂持锁。

---

## D4. 周期对账 = 启动对账同一份逻辑,只多两条

spec §4.4-A 已把**启动对账**的反向对账形状定死,并点出「持锁辨识只在 orchestrator 活着的**周期对账**下才
需要」。本节把那半句落地。

**同一份 diff,不是新算法。** 周期对账仍是「一次本地扫描 diff 一张本地表」,与启动对账共用 `Reconcile` 的
主体。区别只在两条,都因"orchestrator 此刻活着、有 live-map、有正在跑的转换":

1. **跳持锁沙箱**(同 D3):启动时进程刚崩、锁全无,一切 in-flight resume 都已死,故半建 placement 无需持锁
   辨识;周期跑时锁还在,**持锁 = 正在建的活 placement**,绝不能当孤儿回收(会误拆正 resume 脚下的资源)。
2. **保护 live-map 活 placement**:启动时 live-map 空,存活 `running` 靠 `Adopt` 重认;周期跑时 live-map 里
   已有本进程亲手建的活句柄,这些沙箱的 placement **本就不是孤儿**,直接进 keep,不必也不该对它们跑 `Adopt`
   重认(那是崩溃后才需要的补接管)。仅对 store 记 `running`、但 live-map **没有**句柄者才走接管/降级路径——
   那要么是本进程从没建过(不该发生)、要么是崩溃重启后首轮(此时其实就是启动对账语义)。

**周期对账 vs reaper 循环:两个消费者,一份健康判据,可同一个 tick。** reaper 处置 `idle`/`paused`-TTL 是
「态对但该推进」;周期对账处置的是「态与现实不符」(store 说 `running`、单元已死 → `crash-paused`;孤儿
placement → 回收)。二者都建立在**同一次健康分类**之上(§4.3:回收是健康模型的消费者),故实现上**同一个周期
tick 里先分类、再分派**:分类结果既喂 reaper 的 TTL 处置,也喂对账的 crash-pause / 孤儿回收,不跑两遍探活。

**失败策略沿用 §4.4-A:skip-and-log,对不确定的 `running` 存活者取保守**(保护其 placement 不回收,漏拆可逆、
误拆不可逆),对账幂等,跳过项下轮重来。**批量策略**:逐沙箱串行判、单轮全量扫(单机、沙箱数有限,足够);
若将来规模变大,分批 + 限流是 additive 调参,不改本形状。**枚举本机资源**当前只有 runsc 容器 + netns + overlay
mount(egress 面 nft/gateway 随 §5.D 数据面落地时并入枚举,这里预留位置、不提前建)。

---

## D4b. transport-down 的重启预算:瞬态与死亡之间的收敛边界

§4.3 表把 transport-down 定为**瞬态**(L1 活、L2 不通 = transport 子进程崩了、PID1 正原地重启,§5.E.1),处置
是**等重启**、不走 resume。但「等」不能无界:一个**崩溃循环**的 transport 子进程(bind 端口即 panic、re-exec
又崩)永远翻不回 L2 通,若无限等,该沙箱既回不到 healthy、又永远不被 auto-pause / crash-pause,成一个**卡死
的黑洞**。故 transport-down **必须有一条收敛边界**——这正是 §4.3「超重启预算才升级」那半句要落地的机制。

**判据 = 有界重启预算,升级方向 = dead-recoverable。** 一个沙箱**连续**分类为 transport-down 的时长(判据取
时长而非 PID1 重启计数:不需 PID1 向控制面上报计数)超过
**重启预算**后,reaper 把它从 transport-down **升级**为 `dead-recoverable` 处置:走 resume(§4.2)换新 IP 整链
冷启。理由:transport 原地重启本是"成本远低于 resume"的快路径(§4.3),但**快路径不收敛时,唯一仍能恢复该沙箱
的手段就是整链冷启**——升级到 resume 是从"进程内可恢复"退回到"substrate 级恢复",与 dead-recoverable 同一处置,
不新造状态、不新造动词。

**这条不与 single-flight / 转换锁打架。** 升级后的 resume 照常走 §4.4-B 全程持锁 + §4.3 单飞守卫(持锁期分类
挂起),与 dead-recoverable 触发的 resume 完全同路。reaper 在此只是**改判处置健康态**(transport-down→dead-recoverable),
处置动作本身不特殊。

**重启预算取值是纯调参**(同 idle_timeout / retention):保守默认(给 PID1 足够多次重启窗口,避免把一次慢重启
误升级成冷启),配置点标注,可调,不影响本收敛边界的形状。

---

## D4c. 死因必须随分类结果带出:信号易失,处置不能"再问一次"

§4.3 把 `resource-terminated` 与 `dead-recoverable` 定为**并列**处置(前者不 resume),二者只能靠**死因**区分。
这给分类器加了一条本文其余机制都没有的性质:**死因信号是易失的**——它活在 provider 的 placement-local 资源里,
`Stop`/`Destroy`/`ReclaimOrphans` 任一条拆除 placement 即消失,也不跨宿主重启。于是三条约束连锁成立:

- **读点唯一,在 L1 探活那一刻。** 分类器判 L1 死靠 `Adopt`(D3),而那正是 placement 尚完整的最后一刻。故
  `Adopt` 的"已死"结论必须**顺带带出死因**,而不是让处置动词事后再查——事后 placement 可能已被拆掉。
- **带出而非重取。** 分类器是**只读**的、判完就**丢弃**重建的 placement 句柄(D3)。所以死因必须随分类结果
  一起返回给 reaper,由 reaper 交给处置动词;分类器读了却不带出,等于读了就扔。
- **落库与降级 CAS 同一次提交。** 处置动词拿到死因后,写入必须与 `crash-paused` 的 CAS 原子(§4.4-C 同一形状)
  ——CAS 之后 teardown 随时可跑,证据不可恢复,补写窗口不存在。

**两条实测事实把设计空间压成唯一解:**

- **runsc 的盘上状态里没有死因,所以"事后再问"这个世界不存在。** `--root=<dir>` 的持久 `.state` 虽扛宿主
  重启,但只有身份 + 创建期快照:四例对照(OOM / 正常退 0 / 外部 SIGKILL / 退 42)的持久字段**逐字段相同**
  ——无 exit code、无 signal、无 OOM 标志。更要小心的是持久 `status` 对已死容器**仍写着 `running`**(创建时
  写、死时不更新),`runsc state` 报的 `stopped` 是**读时**探 pid 活性算出来的。**别信盘上那个字段**,重启后
  那个 pid 也没有意义。
- **唯一精确的 `ResourceExceeded` 信号是 memcg 叶子的 `oom_group_kill`,而它只活到 `runsc delete`。** 整组
  击杀后实测 `oom_group_kill 1`、`memory.peak` 恰好顶到帽;容器已 `stopped` 但未 `delete` 时仍可读。
  **用 `oom_group_kill`、不用 `oom_kill`**:后者是组内单 task 计数,同一次整组击杀实测为 9,无法回答
  "这个沙箱是不是因为自己的 RAM 帽被杀"。

于是读点唯一落在 `Adopt` 的 `!alive` 分支返回前:`teardownPlacement` 的 `kill`→`delete` 两行会同时销毁
memcg 叶子与 runsc 自己在 `--root` 下的容器状态,而三条死亡发现路径(启动对账 / 周期对账 /
reaper crash-pause)全都经过 `Adopt`。
映射:`oom_group_kill > 0` → `ResourceExceeded`,否则 `Gone`,读不到 → `Unknown`。

> **还有第四条拆除路径,它不是发现路径:用户 `pause` 一个已经死掉的沙箱。** `pause` 不探活,CAS 因记录仍是
> `running` 而成功,随后 `Stop` → teardown 把 memcg 连同死因一起销毁,而 `pause` 传的 cause 是 `nil`(它不是
> 死亡观测,不该编造死因)。于是一个**确实撞了帽**的沙箱可能留下"从未死过"的记录。这不破 §4.3——`Unknown`
> 的结构性自我纠正正为此(§4.3),但它是一处**已登记的窗口**,不是被忽略的疏漏。

**`Unknown` 的处置与结构性自我纠正已毕业进 §4.3,本文不复述**;此处只补它的备选为何不成立。

**落库式重试计数没有可推导的重置点(这是它真正的死因)。** 每一次**完成**的 `Resume` 都是成功的
(`resumeLocked` 只在 ⑦ 就绪门之后翻 `running`),死亡是**之后**异步到来的。故"尝试次数"只能在
crash-pause 时计,它实质是一条**连续死亡 streak**,且必须跨过中间那次成功的 resume——而那次成功正是
引擎拿到的唯一正向信号,代码在那里**刻意清掉死因**,好让陈旧状态不累积。于是只剩三个重置点,都不成立:
成功即重置 ⇒ 计数永不超过 1、什么也没度量;永不重置 ⇒ 半年里合法崩溃五次的沙箱被永久砖化;按时间窗
重置 ⇒ 那是**限流**,永不永久拒绝,既给不出所声称的上界、也不贡献收敛。此外,把它落进沙箱记录里的
per-sandbox 单调计数,**就是换了名字的世代计数**(§1.1 明令禁止,理由正是"防世代计数从别处渗回")。

**并且:按次数拒绝一个显式动词,与按事实拒绝是两回事。** §4.3 已经批准了一种拒绝,其正当性很具体:
`resource-terminated` 拒的依据是**关于这个沙箱的一条已落库事实**,有因果解释力、原则上可补救(抬帽或
销毁)。而按次数拒绝不依赖关于沙箱的任何事实,它说的是"你已经试够多次了"——一个关于**调用方执着程度**
的判断,属产品层语义,且严格劣于把决定交还调用方(调用方可能刚修好负载,引擎无从知道)。

---

## D5. 边界

- **对象回收 refcount/GC sweep(§4.3 uniform-object invariant)不在本文。** 那是 Template lineage /
  Volume 挂载行的引用-回收原子性(§4.3 + §4.4-C),另属对象回收 sweep。reaper 的 `evict` 走 `Destroy` 的
  **同步删 Overlay**,**不按 refcount 异步删字节**——reaper 在此只推**生命周期态**;失败后的字节由同一 tick
  的孤儿单元 sweep 兜。二者的接缝(evict 标 destroyed 与异步删字节解耦)spec §4.4 已定。
- **TTL 取值**(default `idle_timeout` / `retention`)与**重启预算取值**(D4b)都是数值旋钮,不是待办:
  按 [`transport.md`](./transport.md) D6.4 的同一条纪律,取保守默认、在配置点标注、留可调——落点在代码,
  不登记为开放决策。
- **egress 面资源枚举**并入周期对账,随 §5.D 数据面落地;本文预留位。
- **CPU-bound 活动**若将来证明需计入,healthServer 加一份 `/proc` jiffies 采样(D1 末),additive。
