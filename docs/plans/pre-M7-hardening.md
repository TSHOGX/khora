# pre-M7 硬化批 —— open-decisions 剩余项收口

> **无 rung**,照 post-M6 硬化的先例:不占 M 编号,按 lane 提交,每收一项**先**更新
> 深挖/spec、**再**删 open-decisions 对应行(按标题,不按位置)。M7-volume 仍是下一个 rung。
>
> 每个 lane 一个 worktree,并行开,按序 review。lane 之间代码文件不重叠;唯一必然冲突点是
> `open-decisions.md` 的删行 —— 按标题 keying 已使其可解,解法是取并集(每个 lane 只删自己那行)。

## Lane 总览

| Lane | 收的登记项 | 结论 | 状态 |
| --- | --- | --- | --- |
| **A** | §2「§4.1/§4.4-C 事件流(对外面)」+ §4「outbox publisher 头阻塞」+ §4「`death_cause` 不在 §6.1 读面上」+ §2「§2.3/§4 容量压力事件(软阈)」 | 只做 pull;不裁剪(**推迟,非回答** —— 故登记表留一行) | 已定 |
| **B** | §4「PID1 容器级 TERM 无宽限期」 | 沙箱级宽限期**拒绝**;`SendSignal` 拒 `pid<=0` | 已定 |
| **C** | §4「barrier map 只增不删」 | 登记表**陈述错误**,非代码缺陷 | 已定 |
| **D** | §3「disk I/O 速率帽的整盘 MAJ:MIN 来源」 | provider 自行推导 | 已定 |

未纳入本批(归属未来 rung,不前拉):§2 的 Volume / egress / 凭证 / Overlay 物化 / 远端 provider
入参 / 模板内容目录各行。

---

## Lane B —— 定向信号的子系统边界

**依据(不是新规则,是执行一条已写明的边界)。** `deep-dives/guest-runtime.md:134` 的判据原话:
「这条判据是『不指定任何 pid/pgid』:正因为不定向,它才落在 PID 1 侧而非 transport 侧」。
`deep-dives/transport.md:270` 同一分裂:「定向信号归 transport、容器级扇出归 PID1」。
`pid<=0` 在 POSIX 里**就是**扇出语义,故它不是「危险输入」,是**投错了子系统**。

**登记表那条的「今天不可达」不成立。** `SendSignal` 是六个 pid 寻址入口中**唯一不校验**的一个
(其余五个都 `s.get(...)` → `errNoSuchPid`;它们必须查表是因为要摸 per-process 状态,
`SendSignal` 只需一个合法 pid,故不能照抄那五个的修法 —— 会打断
「`List` 拿 pid → `SendSignal` 打它」这条 §6.1 明写的配对)。经
`killProcessGroup`(`cmd/sandboxd/transport.go:101-106`)的两级回退:

| 入参 | 第一发 `Kill(-pid)` | 实际语义 |
| --- | --- | --- |
| `pid=1` | `Kill(-1, sig)` | 全 namespace 广播;成功 → 不走回退 |
| `pid=-1` | `Kill(1, sig)` | **命中 sandboxd 自身的 TERM handler → `os.Exit(0)` → 拆 pid namespace** |
| `pid=0` | `Kill(0, sig)` | 广播到 sandboxd 自己的进程组 |

即**整个沙箱可被数据面一条 unary RPC 拆掉**。引擎自身确实从不发 TERM(销毁只发
`runsc kill KILL`,`internal/runtime/local_runsc.go:462`),但它把该能力经数据面开放给了调用方,
而 §6.1 假定调用方已被信任、引擎不做授权 —— 故这是输入校验缺口,不是授权问题。

**交付物**
1. `SendSignal` 校验 `pid > 0`,否则 `InvalidArgument`。校验点在 **guest 侧**
   (`cmd/sandboxd/process.go`),因为 §5.E.1 的前提是 guest 能写自己的 fs、必须自证;
   orchestrator 侧同形加一道(fail-fast,少一次 RPC)。
2. substrate 测试:`pid=-1, SIGTERM` 被拒且沙箱存活。**必须**是 substrate 测试 ——
   宿主上跑 `Kill(-1)` 会打到开发者会话,爆炸半径只有 runsc 的 pid namespace 关得住。
3. `proto/engine/transport/v1/transport.proto` 的 `SendSignalRequest.pid` 注释写明
   「必须 > 0;非定向信号归 PID 1」。**不改类型**(`int32` → `uint32` 会让 wire 上的
   负值静默变成大正数,把一个显式 400 换成一个更难查的行为)。
4. **拒绝态入档**:guest-runtime D4.3 写入「沙箱级宽限期已拒绝」+ 理由。理由是形状对不上:
   需要宽限的是工作负载进程(它才有要 flush 的东西),而进程级信号已存在;沙箱级 TERM 想优雅
   就得让引擎判断「guest 里哪些进程值得等、等多久」,那是产品层策略,§1.3 litmus 要挡在外面。
   外部证据:e2b 的 `DELETE /sandboxes/{id}` summary 字面是 "Kill sandbox"、无宽限字段;
   sandbox0 沙箱 PodSpec 根本没设 `TerminationGracePeriodSeconds`,吃的是 kubelet 隐式默认
   30s —— 一个没人为它论证过的值;而它**真正做过决定**的宽限期全在进程/会话粒度
   (`process/stop.go` 5s/2s、会话 `StopGracePeriodSeconds` 10s),宿主侧一个都没有。
5. 删登记行「PID1 容器级 TERM 无宽限期」。

---

## Lane C —— barrier 回收:改错的是登记表,不是代码

**登记行断言的泄漏不存在。** 五条路径全查:

1. `inFlight` 条目在 release 归零时 `delete`(`internal/lifecycle/barrier.go:96-98`)
2. `draining` 条目在 `undrain` 里 `delete`(`barrier.go:146`)
3. `pauseLocked` 在 CAS **之前**注册 `defer undrain`(`internal/lifecycle/lifecycle.go:545`),
   失败路径亦回收 —— `failpath_test.go:230` 已钉住
4. 五个 `Enter` 调用点全部 `defer release()`
   (`internal/orchestrator/dataplane.go:238,306,319,331` + `files.go:121`)
5. `mutatingClientFor` 内 `Enter` 是最后一步、其后无可失败操作,故无「条目已建但 release
   丢失」的窗口(`dataplane.go:78-94`)

实测:1000 个不同 SandboxID 各走一遍 enter→release→drain→undrain,收尾
`inFlight=0 draining=0`。

登记行那句「条目由 `cond` 持有、没有对称的释放点」在代码里没有对应物 —— `cond` 是**整个
barrier 一个**(`barrier.go:47`),不是 per-sandbox 的,故不存在「被 cond 持有的条目」。
该描述像是针对一个 per-sandbox cond 的设计写的,而代码自 `19a6fc2` 起就不是那样。

**交付物**
1. 改 `barrier.go:35-43` 的注释:从「声称泄漏」改成**陈述回收不变量**(条目在最后一个
   in-flight 操作 release 时消失;`draining` 在 undrain 时消失)。
2. 回归测试钉住该不变量。**这才是本 lane 的真实价值**:今天没有任何东西会在有人加第六个
   `Enter` 调用点却忘了 `defer release()` 时失败。
3. 删登记行「barrier map 只增不删」。

---

## Lane D —— disk MAJ:MIN 来源

**两个候选不对等。** MAJ:MIN 是彻底的宿主局部事实 —— 远端 provider 产不出整盘设备号 ——
故「宿主拓扑配置项」若落进 Binding 或 provider 入参,就把宿主原语抬进了跨 provider 契约面,
违反 §5.A「policy + handles,NEVER host primitives」。

**定为 provider 自行推导**:`local_runsc` 从自己的 state dir 反查 backing 整盘,MAJ:MIN
全程留在本地 provider 内部、不过 seam;远端 provider 用其调度层的等价物表达速率帽。

外部证据不提供反向先例:sandbox0 **一个 cgroup 文件都不写**(只读 `cpu.stat` 的 throttle
计数),限流只有网络那一路;e2b 的 `DiskSizeMB` 是容量不是带宽。两边都不从任何途径推导整盘
MAJ:MIN。故 `internal/model/model.go:99-104` 的「待有消费者时同形加入」保持不动。

**交付物**
1. `internal/model/model.go:104` 那句「What is still open is only where the whole-disk
   MAJ:MIN comes from」改为陈述已定来源。
2. `internal/runtime/seam_test.go` 的 `bannedTokens` 加 `major`/`minor`/`majmin`/`devno`。
   **理由**:现列表按名字子串匹配(`cgroup`/`hostpath`/`subvolume`…),一个叫 `DiskDevice`
   或 `MajMin` 的字段**不会**被现在的列表拦下。不加这一条,本决定的半衰期就是下一个人加字段那天。
3. 删登记行「disk I/O 速率帽的整盘 MAJ:MIN 来源」。

---

## Lane A —— 事件流对外面(最大,含一条实测出的硬约束)

### A0. 已实测的硬约束:序号必须 AUTOINCREMENT,不能是裸 rowid

ref-repo 审计发现 sandbox0 的 journal 有一个**序号跨非优雅重启回退**的缺陷:`nextSeq` 由
「已持久化 cursor 与盘上最大 Seq 取大 +1」算出,而最高频的 output 事件路径**从不持久化
cursor**;age-trim + 压实把文件清空后重启,序号回退并把同一批号发给不同事件。对计费即错账。

**同一缺陷类会经另一扇门在我们这里重演。** 今天 outbox **没有单调整数序号**:排序靠
`occurred_at`(unix 纳秒,`internal/store/sqlite.go:85`),主键是 TEXT 事件 ID。于是
① `ORDER BY occurred_at` **不是全序**(同纳秒无 tie-break,且墙钟可被 NTP 步进);
② 没有可供 subscriber 续位的游标。因为不是 `WITHOUT ROWID` 表,一个持久单调序号其实已物理
存在(隐式 `rowid`),只是未暴露未使用。

实测(SQLite,本机):

- 裁剪**旧**行 → rowid 继续前进(删 1,2 后新行为 4)
- 裁剪**全部**行 → **rowid 重启回 1**,序号回退
- 改 `INTEGER PRIMARY KEY AUTOINCREMENT` → 清空后仍发 4,高水位存于 `sqlite_sequence`

**故:一旦 Lane A 引入保留裁剪,游标就必须建在 `AUTOINCREMENT` 序号上。** 触发条件很平常
(引擎空闲 + 按年龄裁剪,窗口短暂空掉),而失败是静默的。

### A0b. 序号可见性顺序必须进 store 契约,不能是 SQLite 的偶然性质

第二份审计在 sandbox0 的计费 outbox 上发现一条**比头阻塞更要命**的缺陷:`sequence BIGSERIAL`
在 INSERT 时分配、行在 **COMMIT 时**才可见。tx1 取 5、tx2 取 6、tx2 先提交 → drain 见队头为
6、投递并标记完成,之后 5 才出现在**一个已投递的更晚事件之后**。`FOR UPDATE` + advisory lock
只序列化**读取方**,管不到**生产方**,故那条「`ORDER BY sequence ASC` 保序」的承诺不成立。

**我们的后端上这个交错不可能发生 —— 但原因是后端性质,不是形状。** 实测(引擎自身的
`modernc.org/sqlite`,WAL):tx1 持未提交写事务时,tx2 的 `INSERT` 直接被 `SQLITE_BUSY` 拒绝。
SQLite **序列化写事务**,故「取到低序号者后提交」无从发生;Postgres 允许并发写事务,才有那个洞。

这落在不变量 C 的要害上:`store.Store` 是 seam(另有 Memory 后端,未来可能有 Postgres),而
不变量 C 明写「**接口方法本身承诺这条原子性,后端必须兑现**」且「绝不能设计成后端返回是否支持、
算法据此分支」。故:

- **游标的「无可见性缺口」写成 store 契约的一条**,与既有的「状态变更 + outbox 追加 = 单次原子
  提交」并列 —— 不是 SQLite 的偶然性质。
- **`storetest/contract.go` 加一条对抗性用例**:并发两个提交,断言「已投递的最高序号之前不再出现
  新行」。SQLite 靠序列化写事务通过;一个允许并发写的后端若不额外做水位闸,**会在这条上失败**,
  而这正是我们要的 —— 让后端差异在契约测试里断掉,而不是在计费对账时断掉。

### A1. 从审计反推的其余四条约束

**更正:先前这张表写「三条因不裁剪而自动失效」,那个归因是错的。** 三条确实不适用,但原因**不是**
「我们不裁剪」,而是**它们是 sandbox0 实现手段的产物,不是裁剪这件事本身的产物** —— 手写 JSONL +
原子重写、且在 read 路径上触发裁剪。换句话说:**即使我们将来真的裁剪,这三条依然不适用**,因为
SQLite 的 `DELETE` 不经过压实、不重写文件、不在读路径上改状态。这个区别很重要 —— 按错误的归因,
将来加裁剪时会以为要重新面对三个风险,于是可能因此不敢加。

| 审计发现(sandbox0) | 对我们的约束 | 本批状态 |
| --- | --- | --- |
| `Subscribe` 的 backlog 分配**无界且由调用方控制**(`Read` 上限 10k,`Subscribe` **无对等上限**;`?after=0` 在**持锁期间**物化整个 64MiB backlog + 同长 channel) | 重放**分页 + 硬上限**,且**两条路径共用同一上限** —— 缺陷正出在两条路径上限不一致 | **适用**,且与裁剪与否无关。`GET /v1/events` 的 `limit` 必须有服务端硬上限,不只是默认值。**不裁剪反而加重它**:窗口 = 全部历史,故 `after=0` 要翻的行只增不减 |
| 压实失败留下**已关闭但非 nil** 的 file handle → 之后每次 append 永久失败;且压实报错时在 broadcast **之前**返回,一条已落盘事件永不送达 live tailer | 裁剪不在 append 路径上;投递不与保留操作耦合 | 不适用 —— **因为无压实这一实现手段**,非因不裁剪 |
| `Read` **会改状态**(一个 GET 能在锁内触发裁剪并重写整个文件) | 读路径不得触发保留动作 | 不适用 —— 我们的裁剪(若有)不会挂在读路径上;这是**实现选择**,非裁剪的固有属性 |
| 慢消费者被踢与正常 EOF **不可区分**(SSE 正常结束 body、无 error 帧,且 SSE 从不广播 `latest`,客户端连丢没丢都测不出) | 若做 push,沿用自家 `cmd/sandboxd/process.go:86-89` 的 `overflow atomic.Bool`(它正是为区分「因慢被踢」与「干净结束」而设),不抄这一处 | 不适用 —— **只做 pull,无慢消费者概念**。这条才是真正因本批决定而失效的 |

**注意第一行的方向**:不裁剪不是「消掉了 backlog 约束」,而是**放大了它** —— 这与先前那张表暗示的
方向相反,也是为什么 `limit` 硬上限在本批里是必做项而非可选项。

**唯一被判为 sound 且值得照搬的一处**:trim/cursor 竞态**不存在** —— `Subscribe` 全程持
`j.mu` 跨越 trim→校验→backlog 快照→订阅者注册,`Append` 持同一把锁跨越 write→trim→broadcast,
故快照与注册之间插不进 append/trim,无重复无缺口。**这条锁的覆盖范围是要抄的东西。**

### A2. 头阻塞:倾向保留,不改成跳过

`internal/store/publisher.go:58-60` 一条投递失败挡住其后全部。不变量 C 明写计费 subscriber
「漏一条就漏账」,即**顺序承载语义**,故头阻塞是正确的一侧。待补的是可运维性而非语义:
per-record 带上限的指数退避、以及**让 stall 可观测**。

**审计给出的三条具体约束**(同一 codebase 对头阻塞做了两个相反选择:webhook 走
retry-with-skip、计费 outbox 故意保持头阻塞;分界线是顺序是否承载语义 —— 支持我们保留头阻塞):

1. **必须有毒信封的出口。** 他们的计费 outbox **无 max-attempts、无 dead-letter、无跳过**
   (`projector.go:207`):一个 payload 解不开的队头每 60s 重试到永远,身后全部计费停摆。
   **stall 是刻意的取舍,而「没有任何上界或硬告警」不是。** 我们保留头阻塞,就必须同时给出
   「这条投不出去」的终局:一个尝试次数上界 + 一条必须响的告警。否则我们复制的是他们的缺陷,
   不是他们的取舍。
2. **可观测性不能与被观测的循环同 goroutine。** 两个 gauge 确实存在,但 `observeStats` 与
   `ProjectOnce` **在同一 goroutine**(`projector.go:106`),sink 阻塞(且 `apply` 不设 deadline)
   时永不再采样,gauge 保持旧值被 Prometheus 抓走、无 staleness 信号。净结果:**「卡住但还活着的
   队头」可见,「worker 本身卡死」不可见**。故我们的指标要独立 goroutine + 一个 worker 心跳。
3. **退避要有上限、要有 jitter。** 他们两条路各缺一样:webhook 的
   `Base * 2^(attempt-1) + jitter` **无上限**且 `MaxRetries` 在耐久路径上从未被读
   (`dispatcher.go:540-548`),约 20 次后 `NextAttemptAt` 排到几天后、记录既不投递也不进
   dead-letter 也不告警;计费那条 `1s * 2^min(attempt-1,6)` 上限 60s 但**无 jitter**,多个
   projector 以 2s 轮询时在 `available_at` 上同步,形成对同一队头行的轻度惊群。
   → 我们要**同时**有上限和 jitter。

**一条不能照抄的理由(反向证据)。** 他们能承受重排,是因为 sink 由构造即幂等且按**逻辑时间**
版本化(ClickHouse `ReplacingMergeTree(version)`,version 取自 payload 自己的
`ObservedAt`,而非到达时间),故迟到的重复在 merge 时落败、不会复活旧状态。**我们的计费流上
没有这个缓冲垫**,所以我们比他们更需要严格保序 —— 这条独立支持保留头阻塞。

### A3. 已拍板(A3.1 / A3.2)与仍开放项

1. **对外形状与放置** —— 第三份审计给出明确建议:**只做 pull,放控制面**,并把 §6.1
   「无流」那句改写成它实际要表达的意思。形状:`GET /v1/events?after=<seq>&limit=`,返回事件 +
   `{earliest, latest}`,`after` 早于保留窗口则 **410**。

   **决定性证据:那个 push 面本身就是 pull 实现的。** sandbox0 gateway 的
   `observability/events?watch=true` 返回 NDJSON,但实现是**对同一条 pull 查询做 1s 服务端轮询**
   (`pkg/gateway/http/handlers/sandbox_observability.go:418-457`),游标是同一个。故 pull 是承重
   原语、push 是表现层适配器 —— 先做 pull、日后加 `watch=true` 共用同一 seq,**结构上零代价**。

   **它按构造满足「引擎不追踪 subscriber」**:无 subscriber map、无驱逐策略、无清理路径。
   sandbox0 一旦加了真扇出就立刻需要这三样(`journal.go:169-176`),那正是 §1.3 禁止引擎持有的
   per-subscriber 状态。**「push 作为适配器」不持状态,「push 作为真扇出」持** —— 这是分界线。
   它同时绕开 §6.1 的两难:cursor 分页 GET 就是 CRUD,而引擎级事件流既非 per-sandbox 也非
   per-pid,在数据面本就无处安放。

   **突发不靠 `limit` 节流吞吐**:批次取满时跳过等待立即再取(`:445-446`,`writeWatchEvents`
   在 `len(events) >= limit` 时返回真,`:540`),故 `limit` 限的是单次响应字节数、不是吞吐。

   **反向证据(该建议自身最强的对手)**:pull 把占空比外包给每个调用方,而两个 repo 都显示调用方
   **写错了这个循环** —— e2b 自己的 CLI 有一个差一毫秒的缺口、旁边挂着承认它的 TODO
   (`packages/cli/src/commands/sandbox/logs.ts:150-153`),其 metrics 循环用 `<=` 去重会丢掉
   同时间戳的不同条目。**我们的 A0 序号游标恰好消掉这一类 bug**(e2b 那是时间戳游标),但
   「这是 subscriber 自己的事」只在 subscriber 真的解决了它时才成立。
2. **【已定】不裁剪,行只标记不删(保持现状)。**

   **⚠️ 本项曾用一条错误理由支撑,已更正。** 先前写的是「不裁剪一次性免疫四个审计缺陷」——
   **夸大了**。那四条里三条(压实留下已关闭的 file handle、压实报错跳过 broadcast、
   `Read` 会改状态)是 sandbox0 **手写 JSONL + 原子重写**、且**选择在 read 路径上触发裁剪**的
   产物。在 SQLite 上裁剪就是一条 `DELETE`,事务性由后端保证,这三条**根本不成立**。真正会传导的
   只有序号回退那一条,而它已被 `AUTOINCREMENT` 一行解决(A0 实测)。
   **故「在 SQLite 上裁剪很危险」站不住,不得作为本项依据。**

   **本项成立的真实理由(按强弱):**
   - **今天的 sink 使「已投递」几乎恒真。** `orchestrator.go:130` 的 sink 只是打日志的
     `DedupSink`,`Deliver` 从不失败。若按投递状态裁剪,窗口下界会紧跟队头前进,**事件会在从未被
     任何外部订阅者消费过的情况下被删掉** —— 那正是要防的漏账。裁剪窗口必须由「承诺给外部订阅者的
     重连时长」定,而**今天没有外部订阅者,故没有可论证的窗口取值**。凭空取一个数,是 §1.3 拒绝的
     投机性预留。
   - **改动最小**,且 at-rest 增长今天已经如此 —— 本批不改变现状。
   - 少实现一条 410 路径。**这条最弱**,不足以单独支撑本项。

   **必须诚实标注的代价(不假装已解决):**
   - **at-rest 无界增长依然存在、依然未解决。** `sqlite.go:74-79` 的「KNOWN LIMITATION」
     **保持为 limitation**,措辞不改 —— 它没有因本决定而变成优点。
   - **这是被推迟、不是被回答。** 真正的答案要等第一个外部订阅者出现、能论证窗口取值时才能给。
     故 open-decisions 里**保留一行**记这件事(见下「登记表怎么动」),不能四行全删。

   **连带后果:**
   - **`earliest` 恒为最小序号 → 410 今天不可达。** 窗口 = 全部历史。响应**仍带**
     `{earliest, latest}`(成本为零,且为将来真要裁剪时留好形状);410 **不写实现**,但
     **`api-contract.md` 要写明它是保留语义** —— 让未来加裁剪时,调用方契约无需变更。
   - **`AUTOINCREMENT` 是纵深防御,不是必需**:不删行 → `max(rowid)` 单调不减 → 裸 rowid 本就
     安全。仍要上,理由是让不变量**显式**、不依赖一条活在注释里的「我们承诺永不删行」政策 ——
     而这条政策恰恰是**将来一定会被打破**的那种。代价近零(改 `CREATE TABLE` 就地重建,
     `CLAUDE.md` 无兼容包袱)。

   **一条先前没说清的耦合(fork 指出):** 选 pull 端点就**强制**要有可翻的行,有保留就**强制**
   要面对增长边界。先前把「做 pull 端点」与「永不删行」当两个独立决定分别拍,实际**前者是后者的
   原因**。故 at-rest 增长不是遗留项,是本组合选择的直接代价 —— 上面已按此标注。
3. **`death_cause` 上读面的字段形状**:两参考实现**都无先例**(e2b 的 `SandboxState` 只有
   `running|paused`、连终态都没有,死亡即下次调用撞 404/409;sandbox0 有 `failed` 态但
   **无 cause 字段**,`TerminationReason|OOMKilled|death_cause` 全 codebase 零命中)。
   净新增,需自行论证。
4. **容量压力事件(软阈)**:两边都只有 Prometheus gauge / 采样,无人发压力事件。
   支持登记表现有倾向 —— 随本轮统一收口,不单开一个 event kind。

### A4. Lane A 内部次序

A0/A0b(序号 + store 契约)→ A2(退避上限 + jitter + 独立 goroutine 指标 + 毒信封出口)
→ `GET /v1/events`(pull,含 `limit` 硬上限)→ `death_cause` 读面 → 软阈事件。

`death_cause` 读面与 `/v1/events` 同动 `internal/orchestrator/api.go`,故**同 lane 串行、
不再拆 worktree**。

### A5. 登记表怎么动(本 lane 特有:不是四行全删)

其余三个 lane 都是「收一项、删一行」。Lane A 不是 —— **裁剪/保留窗口是被推迟,不是被回答**,
故必须留下痕迹:

| 登记行 | 动作 |
| --- | --- |
| §4「outbox publisher 头阻塞」 | **删**。已回答:保留头阻塞(顺序承载语义),补上限退避 + jitter + 毒信封出口 + 独立指标 |
| §4「`death_cause` 不在 §6.1 读面上」 | **删**。已回答:上读面 |
| §2「§2.3/§4 容量压力事件(软阈)」 | **删**。已回答 |
| §2「§4.1/§4.4-C 事件流(对外面)」 | **改写,不删**。event schema / subscriber 续位约定已定(pull + seq 游标),但**「可重放窗口大小」仍开** —— 今天无外部订阅者,故无可论证的窗口取值。该行收缩为只剩这一个问题 + 一句「410 已作为保留语义写进契约,加裁剪时调用方契约无需变更」 |

**为何这样切:** 那一行原本捆了四个问题(schema、窗口、续位约定、两处限制)。三个已答,一个未答。
按 `CLAUDE.md` 的「已定进 spec、未定留登记表、两边不重叠」,正确做法是**把已答的三个写进 spec、
把未答的那个留在登记表**,而不是整行删掉(那会让窗口问题无声消失)、也不是整行留着(那会让三个
已答问题继续假装开放)。

---

## Worktree 与 review 次序

```
git worktree add .claude/worktrees/lane-a  -b lane-a-event-stream
git worktree add .claude/worktrees/lane-b  -b lane-b-signal-boundary
git worktree add .claude/worktrees/lane-c  -b lane-c-barrier-invariant
git worktree add .claude/worktrees/lane-d  -b lane-d-disk-majmin
```

(`/.claude/` 已在 `.gitignore` 里,故 worktree 不会被 `git add -A` 当 gitlink 提交。
本批开始前先 `git worktree prune` —— 现有三个 `agent-*` worktree 的提交都已并入 main。)

**文件不重叠(可真并行)**

| Lane | 触及 |
| --- | --- |
| A | `internal/store/*`、`internal/orchestrator/{api,orchestrator}.go`、`docs/spec/{api-contract,model}.md` |
| B | `cmd/sandboxd/{process,transport}.go`、`internal/orchestrator/dataplane.go`、`proto/`、`test/`、`docs/spec/deep-dives/{guest-runtime,transport}.md` |
| C | `internal/lifecycle/barrier*.go` |
| D | `internal/model/model.go`、`internal/runtime/seam_test.go` |

**唯一必然冲突点**:四个 lane 都要删 `open-decisions.md` 的行。按标题 keying 后可解 ——
每个 lane 只删自己那行,解法取并集。**A 与 B 都碰 `internal/orchestrator/`** 但不同文件
(`api.go` vs `dataplane.go`);B 若在 orchestrator 侧加 fail-fast 校验会碰 `dataplane.go`,
A 不碰该文件,故仍不重叠。

**建议 review 次序**:C → D → B → A(按体量与风险递增;C/D 几乎纯文档 + 一个测试,
B 有一个真实的安全缺口修复,A 最大)。

**每个 lane 的完成定义**
1. `go build ./...` + `go vet ./...` 通过
2. `go test ./...`(常规门)通过
3. 触及 substrate 行为的 lane(B)另跑 `sudo env "PATH=$PATH" "HOME=$HOME" go test -tags substrate -count=1 -p 1 ./...`
4. **先**更新 spec/深挖,**再**删 open-decisions 对应行(同一提交内)
5. Conventional Commits,scope 用 §5 短名(`fix(sandboxd):`、`docs(spec):`…)
