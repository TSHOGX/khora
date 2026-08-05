# M7 — Volume(把"额外要独立存活的数据"做成一等公民)

> ladder 见 [`README.md`](./README.md);spec 锚点见文末「附」。
>
> **设计已全部收敛进 spec**(§5.C / §3.4 / §4.3 / §6.1 / §6.2,推导与证伪在
> [`deep-dives/volume.md`](../spec/deep-dives/volume.md) D0–D9),本文**不复述契约**,只写落地要的六件事:
> 边界怎么切、实测约束怎么约束写法、store 侧四条 FK 路径怎么翻译、改哪些代码、怎么验收、内部怎么分门。
>
> **编号约定**:裸 `§N` 一律指 spec 全局章节;`Dx` 指 volume.md 的节;指本文自己的节时写「本文 §N」。

---

## 0. 一句话结论(先读这句)

把 Volume 从"spec 里已定、代码里一行没有"变成真交付:**一个独立子系统 + 它自己的 `volume.Backend` seam**
(local = btrfs subvolume;remote/FUSE 划走)、**一张挂载表**(§4.3 一等持久对象,承载独占租约)、
**准入门缺失的那一半**(卷帽进第一项、卷的无帽字节进第二项)、**snapshot / clone**(带 `parent` lineage,
帽必须显式写)、**直连 import/export**(卷与沙箱解耦的兑现面)、以及**两个 sweep 面**(孤儿卷字节 +
squota 泄漏的配额行)。

它是剩下最大的一级,但**不需要任何新的存储原语**:`internal/btrfs` 原样复用,直连访问器是一个
`openat2(RESOLVE_IN_ROOT)`。

---

## 1. 范围(in scope)

| # | 件 | 物化 |
| --- | --- | --- |
| **1** | `volume.Backend` seam + `internal/volume`(§5.C 子系统) | local 臂 = btrfs subvolume;**外加一个 test-only plain-dir 第二臂**(D2:seam 唯一的证伪手段) |
| **2** | 卷作为一等对象 | `volumes` 表(`size` / `backend` / `parent`),存在即状态、derived refcount、无事件(§4.3) |
| **3** | 挂载表 + 独占租约 | `mounts` 表,双向 FK RESTRICT + 原子插入判定租约;两条唯一性断言 + 路径规范化(§5.C) |
| **4** | 进 guest 的挂载 | `LaunchSpec.Mounts []MountSource` → `local_runsc` 追加 OCI bind(与 `RootfsSource` 平级) |
| **5** | 三种 AccessMode | exclusive / shared-ro / shared-rw;**ro 的强制在运行时、排他在控制面**(§5.C) |
| **6** | 准入门的 §5.C 那一半 | 第一项 += Σ(卷帽);第二项 += 卷 backend 的无帽字节(§3.4) |
| **7** | 卷帽的写点 | `PATCH /v1/volumes/{id}` `{size}`;抬帽重过准入门、**降到低于当前用量 → 400**(§5.C / D7.3) |
| **8** | snapshot / clone | 普通可写卷 + `parent` RESTRICT;**帽必须在同一次 backend 操作里显式写**(D6.1a / D7.3) |
| **9** | 两个回收面 | `SweepOrphanVolumes`(孤儿卷字节)+ **`<stale>` 配额行回收**(D7 的回收程序) |
| **10** | §6 端点 | create / list / inspect / **patch** / delete / snapshot / clone(§6.1 Volume 块,除 import/export) |
| **11** | Promote 的 excluded-paths 变成 per-Promote 入参 | rootfs-overlay D5 已定形;Volume 一落地它就是**正确性前提**(本文 §3⑤) |
| **12** | 直连 import/export + **写入 regime 不变量** + 它的 in-flight 标记(三者同门,一个单元) | seam 加 `OpenRoot`(根限定访问器,local 臂 = `openat2(RESOLVE_IN_ROOT)`)+ §6.1 两个端点 + live-map 谓词 |

第 12 件是**"卷与沙箱解耦"的兑现面**,不是附加便利:没有它,"从零播种一个卷"与"读最后一个沙箱消失之后的
卷"都无路可走,而这两件正是一等卷该能做到的事。它同时是写入 regime 不变量**唯一的**触发者,故三者必须
同门——单独实现标记就是 §1.3 禁止的无消费者契约成员。**它很便宜**:实测 `openat2(RESOLVE_IN_ROOT)` 一个
syscall 即给出与 chroot 相同的路径语义(`..` 夹在根、绝对路径按根重解释、越界 symlink 解析回根内),
无特权、无 mount namespace,0.52 µs/op——对照一个参考实现为同一保证付的 178 µs/op(每请求一个 namespace +
`pivot_root`,还要 root)。

第 11 件不是顺手带的:挂载点会把**被遮蔽的既有 upper 字节**完整拷进冻结层,并同时计进该沙箱的容量帽与
代价预报。它是第 4 件的正确性组成部分。

---

## 2. 非范围(out of scope,明确划走)

- **remote / FUSE 臂**:D2 的 handle 形状(非导出私有 handle)必须**容得下它**,但一行 FUSE 代码不写。
  seam 的 `Release` 之所以不叫 `Unmount`,正是为了它(D2)。
- **独占卷的抢占 / 强制 detach**:默认独占直到 destroy/evict。除策略未定之外另有一条独立论据——锁序:
  卷维度反查持有者会闭合成环(D3,实测死锁)。
- **`SeedBuiltinVolumes`**:D3 的钩子表里列了它("若有平台预置卷"),**本里程碑不做**——没有消费者。
- **卷的 TTL / 自动回收**:卷的生命周期与任何沙箱解耦,§4.3 只给了 refcount 判据,没有 TTL 判据。不发明。
- **guest 侧 statfs 反映帽**:显式**不承诺**(§5.C / D7.4)。能诚实投影的后端可以做得更好,那是该后端的
  义务,不是核心承诺。

---

## 3. 边界设计(本里程碑最容易做坏的地方,先钉死)

**① seam 吐一样东西:一个宿主挂载点。三个动作不能压进一个调用。**
「分配」「设帽」「放置」各自成方法(`CreateVolume` / `SetCapacity` / `Materialize`)。这条是**用第二个
backend 验出来的**(D2):合并形状在创建、容量、准入项三处各响一次。故 test-only 的 plain-dir 臂不是凑数,
**它是这个 seam 唯一的证伪手段**,必须与 btrfs 臂跑同一份共享契约用例。

**② `Usage(ctx, volumeID) → (used, limit int64)` 不是自选方法,是卷帽写点逼出来的。**
§5.C 要求 `PATCH` 拒绝"降到低于当前用量",判据是**严格不等式**,而引擎必须能读到"当前用量"才能判。
`inspect` 想报用量也走它。它符合既有形状(mediator 问、provider 答,ID 过 seam、宿主路径留内)。

> **`limit` 的返回值有一个已实测的二义性,不要试图用它做校验**:`internal/btrfs` 两个方向上都把
> `none ↔ 0` 映射,故"从未设过帽"与"故意无帽"读回来**逐字节相同**(D7.3)。`Usage` 的 `limit` 只能用于
> 诊断与 inspect,**权威永远是卷记录**。任何"事后核对帽是否设对"的想法在这条基底上不可实现。

**③ 卷身份在 seam 之上恒为 ID;宿主目录只以不透明 `SourcePath` 浮出。**
照 `Merged.MergedPath` 的先例,`seam_test.go` 的孪生守卫保留 §5.A 那份宿主原语 token 清单**原样**,另加
`reflink` / `fuse` / `loopdev` / `mountopt`,唯一豁免是 `SourcePath`(连理由一起写进豁免表)。
并守 `Capabilities` 成员**必须是 `bool`**——一个 `map[string]string` 的特性表会把机制名当作**值**带过 seam,
任何扫标识符的结构守卫都看不见它(D2)。三条守卫都要有**反向注入验证**(注入 `SubvolumePath` / `FuseSocket` /
`Features map[string]string` 各触发一次失败)。

**④ `Capabilities()` 必须在分配 ID / 动字节之前问。**
形状是 per-backend(不是 per-volume)、成员只列有消费者的能力(`Snapshot` / `Clone` / `Quota`,§1.3 禁预留)。
**时序是承重的**:一个外部实现把"后端不支持"映射在 HTTP 边界,而错误出生在深处——源卷已被 fetch、flush、
materialize 之后才 400。配合 D2 那条"新卷 ID 由 mediator 分配,因为 backend 自铸 ID 的字节无法登记进在飞
集合",迟到的 capability 检查会留下一个**副作用不可归类**的窗口。

**⑤ Promote 的 excluded-paths 成为 per-Promote 入参,`PromoteToTemplate` 与 `PromoteCostBytes` 同时改签名。**
静态包级清单无从得知 per-sandbox 的挂载 dest。作为参数传入也是架构上更干净的路——§4 mediator 本来就持挂载表、
本来就调 Promote,故 **§5.B 永远不必去问 §5.C**(不产生子系统互调)。**两个方法必须用同一个集合**,否则代价
预报会继续把不会被拷的遮蔽字节算进去(方向安全,但已知是错的)。路径按既有约定:upper 相对、去前导斜杠。
嵌套 dest **剪叶子子树、接受空的中间链**(rootfs-overlay D5 已定)。

**⑥ 挂载行的写者只有一个,这把并发面收得很窄。**
`resume` 只**读**该表,`destroy` 只删自己沙箱的行且 FK RESTRICT 覆盖次序,`pause` 不写行。**唯一的竞态写者
是挂载行的 INSERT**,兑现方式是一条 `INSERT … SELECT … WHERE NOT EXISTS`(语句在 D3),`RowsAffected()==0`
⟹ 租约冲突 → 409。**不新增 per-volume 锁类**;Memory 侧同一谓词落在其既有互斥下。

---

## 4. 实测约束怎么约束写法(这些是"代码在哪儿会绊倒",不是背景)

**帽 × 快照(D7.3)**

1. **`Snapshot` / `Clone` 里设帽是不可省的一步,且必然是 ioctl 之后的第二次调用。** 基底给快照的是一条全新
   **无帽**行,漏设是静默且事后不可检出的。故那两个方法的实现必须自己把帽写上,**不能依赖调用方随后 PATCH**。
2. **由此产生的窗口要被在飞集合覆盖,且 sweep/adopt 要把"卷记录在、配额行无帽"当修复态。** 崩溃卡在两次调用
   之间留下的是一个已登记、活着、**无帽**的卷。
3. **`btrfs subvolume snapshot -i <qgroupid>` 明确排除。** squota 下它被接受,但设的是**成员关系**不是帽:
   新单元自己的行仍无帽,而 level-1 父上的 limit 确实强制 ⇒ 它是**所有成员共享的天花板**,一个成员耗尽会让
   **兄弟卷**一起 `ENOSPC`。它在单卷测试里会像能用,在生产里炸邻居。
4. **`-r` 快照在本 progs 版本上要先 `property set ro false` 才能 `subvolume delete`。** 设计路径不用 `-r`
   (快照是普通可写卷),但**测试与清理代码**若造过 `-r` 单元就会踩到,trap 要无条件带这步。
   - 同一次实测顺带否掉一条实现路径依赖:解冻后帽**穿过 ro→rw 转换存活**,故 `Clone` 可以是
     snapshot-then-unfreeze,snapshot 与 clone 因此共用一条后端路径(在范围第 8 件里当一件写)。
   - **且范围第 5 件("ro 强制在运行时")的方向性要落进代码,不只落进理解**:那句话只对"可写单元 + ro
     挂载"成立。反向——**只读单元 + rw 挂载规格**——不是运行时 `EROFS`,而是 `runsc create` **硬失败在
     gofer 挂载**(容器没被创建、guest 从未运行)。设计路径踩不到它,但物化挂载规格那段代码不能把 ro 当
     "随便标都安全"的位:一个反了的位会把本该准入期 4xx 的错误变成冷启期一个不透明的 mount 失败(D1)。

**降帽(D7.3)**

5. **`PATCH` 的校验是严格不等式 `size > 当前用量`,不是 `>=`。** 实测 `limit == 用量` 被接受并存下,但随后
   一次写就 `ENOSPC` ⇒ 它意思是**满**。`>=` 会给调用方一个 200 和一个再也写不进一个字节的卷。
6. **不要写"降帽后再抬回去"的补偿逻辑,那条路不存在。** 超限期间配额 ioctl 自己也返 `ENOSPC`,帽既不能撤、
   也不能抬,连抬到高于当前用量都不行。唯一出路是删 guest 的字节,而引擎不能代调用方做。

**sweep / 回收(D7 / D7.1 / D7.2 / §5.C)**

7. **枚举**必须按目录逐条探测(readdir + 逐条 `IsSubvolume`),**不得**用文件系统级 subvolume 枚举——后者的
   作用域是整个 fs(实测在 volumes 目录与在兄弟 overlay 目录下返回逐字节相同的结果,且含节点上其他软件的
   单元)。**`internal/btrfs` 今天就是对的形状,不需要新代码,只需要不"为速度优化"掉它。**
8. **`<stale>` 行的 destroy 是一条新路径**:今天 `DeleteSubvolume` 的 best-effort destroy 给不了它。需要
   按**删除时记下的 id** 定位、读标签、在 `<stale>` 上 destroy;`<squota space holder>` 留着下轮;
   `<under deletion>` 再 settle 后重试;读失败 = **未知**,不得据此下结论。**轮内重试上界约 3 轮**,超界留行
   + warn,**绝不在 EBUSY 上死转**(一个 space holder 可以在活克隆的整个生命期里合法地无法 destroy)。
9. **每轮一次 `filesystem sync` 然后 `subvolume sync`**(不是每单元一次):`subvolume sync` 单独用会阻塞到
   下一次周期 commit(实测 3–16 s),前面先 `filesystem sync` 则两者合起来 14–29 ms。同样效果,从"等 commit"
   变成"有界"。
10. **配额行的批量读取在并发 churn 下会整体失败、一行都不返回**(实测约 1/150,而那正是 sweep 运行时的条件)。
    **把空输出当成"行已消失"的读取方会静默跳过一次 destroy,那是永久泄漏。** 必须查退出码。
11. **启动时那一次 sweep 排在任何存储单元创建之前**——它在启动序列里的位置是承重的,不是随手挂在周期对账上
    (否则一次重启可以在第一个 tick 之前就把继承来的帽发出去)。

**挂载 × Promote(rootfs-overlay D5)**

12. **空占位不需要排除项**(实测:`runsc create` 之后、任何 guest 代码之前,upper 里已是普通空目录、
    mode 0755、零 overlayfs xattr)。**排除项真正要干的是剪掉被挂载点遮蔽的既有字节**——那些字节 guest
    完全看不到,却会被完整拷进冻结层并计进容量帽与代价预报。
13. **Promote 不要改用 `cp --reflink` 当优化**:reflink **出去**会每次漏一行配额行,且泄漏比那个 layer 活得
    更久(注释已就地写在 `copyTree`)。

**harness(实测各制造过一次假结论,别"简化"掉)**

14. 活 guest 探针的循环时长必须长过整场运行——探针自行结束会让 PID 1 退出、容器转 `stopped`,与"这次改动
    杀死了沙箱"**不可区分**。
15. 不要用 `timeout` 包住 frozen 窗口里的快照(破坏 freeze 计数)。
16. guest 脚本必须是**单物理行**(`process.args` 里的换行会让 runsc 报字符串字面量非法);bare rootfs 无
    coreutils,只能用 dash 内建。

---

## 5. store 侧:四条 FK 路径、四个 409、以及"白拿"到底白拿了哪一半

Volume 给 store 加了**四条新的引用关系**,它们的结局必须**互相可区分**(§6.2)。今天的代码在这里有一个
具体缺口,位置是确定的:

| 引用关系 | 违反时的结局 | 今天的代码 |
| --- | --- | --- |
| `mounts.volume → volumes` RESTRICT | 删有活挂载行的卷 → 409 | 谓词可复用,**sentinel 要新的** |
| `mounts.sandbox → sandboxes` RESTRICT | 删有活挂载行的沙箱 → 409 | **翻译点不存在**(见下) |
| `volumes.parent → volumes` RESTRICT | 删有活子代卷的源 → 409 | 谓词可复用,**sentinel 要新的** |
| `mounts` 唯一违反(租约) | attach 撞独占 → 409 | **需要一个新的唯一违反谓词** |

**"白拿"要分清哪一半**(D3 实测):**检测谓词**可原样复用——既有的 FK-违反判定是**表无关、列无关**的字符串
匹配,对新增的 FK 路径全部为真、对唯一违反为假。**但沙箱侧的翻译点今天不存在**:`SQLite.DeleteSandbox` 是
裸 `t.Exec` 后直接 `return err`,故 RESTRICT 会以裸驱动错误冒到 API 的 `default` 分支变成 **500 而不是 409**。

> **那个字符串匹配是稳定的一侧,不要"优化"成码比较。** 已实测:开了扩展结果码后两个 FK 方向报**不同的码**
> ——缺父插入是 787,而删一条被 `ON DELETE RESTRICT` 指着的行是 **1811**(隔离 A/B 确认这是 RESTRICT 自己的
> 行为,不是驱动怪癖)。两者文本相同,这正是一条字符串匹配覆盖所有路径的原因;`code == 787` 的重写会**静默**
> 停止识别每一次 RESTRICT-删除。(注释已就地写在 `isFKViolation`。)

落地要点:

- **`store.ErrInUse` 的文案是 "store: template in use"**,API 侧注释也只讲 Template。被挂载行挡住的**沙箱**
  删除、被子代挡住的**卷**删除,复用它都会返回一个**说错对象**的 409。故形状是:既有 FK 谓词照用 +
  **新加一个唯一违反谓词** + **各自的 sentinel**。
- **`Memory` 后端今天完全没有引用扫描**(`DeleteSandbox` 只有 not-found):要**新写**沙箱侧、卷侧、lineage 侧
  三处扫描。`storetest` 的契约清单里**没有任何挂载形状的用例**,要一并加(两个后端跑同一套断言,§1.3)。
- **`mounts` 表要带 `CHECK (access IN (...))`**——租约偏索引的谓词是**字面串比较**,`'Exclusive'`(大写)会让
  偏索引根本不适用而两条都插入成功,是一类**静默**失效。
- **一条契约测试守住 FK 强制本身**(不是靠 DSN 字符串的存在):失去它的表现是**静默**的——关掉 `foreign_keys`
  pragma 后删被引用的行**零错误返回**(实测)。
- **删除次序是必需的,不是风格选择**:RESTRICT **立即**生效,把两条删除包进一个事务**买不到**次序无关性,
  故必须先删挂载行、再删沙箱。**`defer_foreign_keys` 实测拒绝**:它确实让次序无关,但违反在 `Commit()` 才
  浮现,而 Go 的 `Tx.Commit()` 返错时 SQLite **并未回滚**——在被钉死的单连接上事务保持打开,其后经连接池的
  查询**跑在该事务内部**、看见别的 handle 看不到的未提交状态。这是 store 级正确性风险。
- **幂等删除不得吞掉这四个 409**:它们是 RESTRICT 触发的**全部**对外信号。一个外部实现正是这样失手的
  ——卷不存在返 `200 {"deleted": true}`,快照删除把三种 not-found 全折成成功,于是"守卫失败的删除"与"成功的
  删除"不可区分。
- **无迁移顾虑**:`volumes` / `mounts` 都还不存在,`sandboxes` 也可破坏性改(CLAUDE.md:无 compat 负担)。

---

## 6. 代码面(改哪些,按子系统)

| 位置 | 改动 |
| --- | --- |
| **`internal/volume`**(新,§5.C 子系统) | `Backend` seam(`Name` / `CreateVolume` / `SetCapacity` / `Usage` / `Materialize` / `Release` / `AdoptMaterialized` / `OpenRoot` / `DestroyVolume` / `UncappedBytes` / `Snapshot` / `Clone` / `SweepOrphanVolumes` / `Capabilities`)+ `local_btrfs` 实现 + **test-only `plain_dir` 第二臂** + `volumetest` 共享契约包 + `seam_test.go` 结构守卫。`OpenRoot` 返回的 `Root` **不含宿主路径**,两臂都用 `openat2(RESOLVE_IN_ROOT)`(plain_dir 臂与 btrfs 臂在这一条上机制相同) |
| `internal/btrfs` | **原样复用,不改形状**;只加 `<stale>` 配额行回收要的两件:按 id 读行标签、按 id destroy(本文 §4⑧)。`DeleteSubvolume` 的 best-effort destroy 不动 |
| `internal/model` | `VolumeID` / `Volume{ID, Backend, CapByte, Parent, CreatedAt}` / `Mount{Sandbox, Volume, Path, Access}` / `AccessMode`。**卷帽不进 `Limits`**——它不是 Binding 三轴(§3.4) |
| `internal/store`(接口) | `CreateVolume` / `GetVolume` / `ListVolumes` / `SetVolumeCapacity` / `DeleteVolume`;`CreateMount`(原子,返租约冲突)/ `ListMounts(sandbox)` / `ListMountsByVolume` / `DeleteMounts(sandbox)`;**四个 sentinel**(本文 §5)。挂载行**不发事件**(非 §4.1 转换) |
| `internal/store/sqlite.go` | `volumes` + `mounts` 两张表(FK RESTRICT × 3、租约偏索引、`CHECK (access IN …)`、`UNIQUE(sandbox,path)`、`UNIQUE(sandbox,volume)`);**`DeleteSandbox` 加 FK 分支**(今天缺,本文 §5);新增唯一违反谓词 |
| `internal/store/memory.go` | 同上;**新写三处引用扫描**(沙箱侧 / 卷侧 / lineage 侧),同一谓词落在既有互斥下 |
| `internal/store/storetest/contract.go` | 加挂载形状的契约用例(今天一条都没有)+ **FK 强制本身**一条 + 租约原子插入的并发用例 |
| `internal/runtime`(seam) | `LaunchSpec` 加 `Mounts []MountSource{SourcePath, Dest, ReadOnly}`(与 `RootfsSource` 平级) |
| `internal/runtime/local_runsc.go` | `buildOCISpec` 把每个 `MountSource` 追加成 `ociMount{Type:"bind", Source, Destination, Options}`(ro 臂加 `ro`) |
| `internal/rootfs`(seam) | **`PromoteToTemplate` / `PromoteCostBytes` 增 excluded-mount-points 入参**(本文 §3⑤);两者必须用同一集合 |
| `internal/lifecycle` | 五处挂钩(`Create` 写挂载行、`coldStart` Materialize + 组 `LaunchSpec.Mounts`、`resumeLocked` 读回挂载表重 Materialize、`pauseLocked` Release、`destroyLocked` **先删挂载行再 `DeleteSandbox`**);`liveState` 加 `mounts`;**`unwind` 必须显式删挂载行**(今天那几处把错误写成 `_ =`,会留下一条删不掉的僵尸沙箱记录);`CreateSpec` 增 volume-binding 入参 |
| `internal/lifecycle/admission.go` | `sumCommitted` 加 Σ(卷帽);disk 项加 `Volume.UncappedBytes`;**新增 `admitVolume(capBytes)` 入口**——不复用 `admit`(那条"计量轴必须有帽"的检查是关于**沙箱**的陈述);抬帽路径持 `admitMu` 跨 {gate → commit} |
| `internal/lifecycle`(sweep) | `SweepOrphanVolumes` 镜像 `SweepOrphanOverlays`(mediator 算 live + building,backend 枚举自己)+ 配额行回收轮 |
| `internal/orchestrator/api.go` | Volume **九个**端点(七个 + `import`/`export`);四个 409 各自映射 + **写入 regime 的第五种 409**(live-rw 挂载持有 ⟹ 拒直连变更);`size` 缺失 / 降帽越界 / 绑定自相矛盾 → 400;capability 不支持 → 400。import/export 流式,不整块缓冲 |
| `internal/lifecycle`(写入 regime) | `admitDirectWrite(volumeID)`:在**持 live-map 那把锁的同一个临界区**内既裁决"无 live-rw 持有者"又发布 in-flight 标记(与既有在飞构建 keep-set 同形,**不引新锁类**)。谓词读 live-map,**不读** `sandboxes.state` |
| `cmd/orchestrator/main.go` | `--volumes-dir`(默认 `StateDir/volumes`,与 overlays 同级);**boot 序列:卷 sweep 排在任何存储单元创建之前**(本文 §4⑪) |
| `internal/arch/coupling_test.go` | 无需改——`volume` 已预列在 `subsystems`,allowlist 保持为空 |

---

## 7. 验收(e2e on `sbx`,每条 assert 内联其 property)

> **一条纪律先说**:**不要断言"沙箱存活"**。实测五种在活 guest 底下拿掉卷的手法**无一杀死沙箱**(容器仍
> running、新 exec rc=0、runtime 无错误),故它恒真、永不失败,作为回归守卫**毫无价值**。要断言的是那些
> **会静默通过**的性质。

| # | 断言 | property |
| --- | --- | --- |
| 1 | 卷先于沙箱建、晚于沙箱亡:create 卷 → create 沙箱挂它 → destroy 沙箱 → 卷仍在且内容逐字节不变 | §5.C 生命周期解耦 |
| 2 | 同一卷被两个沙箱并发 rw 挂:两侧都**真的可写**(不是静默降级),kill 掉一侧后另一侧仍能读**和写** | D1 的承重地基;拆除不是跨沙箱副作用 |
| 3 | ro 臂与两个活 rw 臂并存:guest 内写得 `EROFS`(干净 POSIX 拒绝)、且**宿主始终看不到那次写** | ro 强制在运行时,且不静默成功 |
| 4 | 独占租约:24 racer 并发 attach 同一卷,**恰好一个**成功、其余 409、零驱动错误;多卷 all-or-nothing | §5.C 唯一新不变量 + D3 原子插入 |
| 5 | 挂载拓扑跨 pause 持久:pause → resume 后按**当前挂载表**重挂(新宿主物化、同一真身) | 挂载关系是 Binding(§3.4) |
| 6 | detach(PATCH 去掉一项)→ 租约立即释放、拓扑在 next-resume 才变 | §6.1 两个生效点不同步是设计事实 |
| 7 | **有活挂载行的卷 sweep 返 0 且单元仍在,并带正向对照**(同一次调用里一个真孤儿确实被回收) | 只验"没删错"会漏掉"什么都没干" |
| 8 | 兄弟树(Overlay upper)与枚举根下的**外部**存储单元一根未动 | 枚举形状(本文 §4⑦) |
| 9 | 活 guest 的稳态在 sweep 前后**逐字节相同** | 只断言"写返回 ok"在卷已被彻底删除时照样通过 |
| 10 | 触碰 stale 行的 sweep 之后:卷的帽**未变**、卡在帽上的 guest 之后仍得 `ENOSPC` | `limit none` 是唯一破坏了数据完整性以外承诺的操作(D7.2) |
| 11 | `Materialize` 是 **create-if-absent**:源被删后 resume 干净重启(数据全失但可恢复),而非永久无法 resume | D7.2 的恢复结论依赖这一条 |
| 12 | snapshot 一个**活的、正被写**的卷:guest 写入速率不受影响、零写入错误;快照的 append-only 日志是完整前缀 | crash-consistent 且不打扰写入方 |
| 13 | **snapshot 出的卷帽确实被写上**:新卷 `Usage().limit` == 声明值,且写超声明值精确 `ENOSPC` | 漏设帽静默且事后不可检出(D7.3) |
| 14 | 删有活子代的源卷 → **409**,且**配额行零泄漏**(fs 的行数回到基线) | `parent` RESTRICT 让泄漏根本不产生(D6.1a) |
| 15 | 崩溃注入在 snapshot ioctl 与设帽之间:重启后该卷被判为**修复态**并补上帽(不是被当合法无帽卷放过) | D7.3 的承重窗口 |
| 16 | `PATCH` 抬帽 → 重过准入门(可 507);降到 == 当前用量 → **400**;降到 < 用量 → **400** | 严格不等式;`==` 意思是满(D7.3) |
| 17 | 准入门:一堆**无帽卷**不能把节点填满而一次都不拒;Σ(卷帽) 进第一项 | 漏掉卷帽的后果是当场超卖(§3.4) |
| 18 | Promote 一个挂过卷的沙箱:挂载点在层里只是**空占位**,且被遮蔽的 upper 字节**不进层**;代价预报用同一集合 | 本文 §3⑤ / §4⑫ |
| 19 | 契约用例双臂全过,含 **capability-negative**(plain-dir 臂上 snapshot → typed 拒绝**且无副作用**)与 **`UncappedBytesGrowsWithWrites`** | 后者在缺该方法的 seam 上会失败,这是它不是装饰的证明 |
| 20 | **结构断言**:`volume.Backend` 的签名 / 结果 / **结构体字段**里无宿主原语(含 `reflink`/`fuse`/`loopdev`/`mountopt`),`Capabilities` 成员全是 `bool`;三条守卫各有反向注入验证 | §5.A ❌ list / §1.3 seam(D2) |
| 21 | **零沙箱播种与读回**:一个从未有过任何沙箱的节点上 `import` → `export` 逐字节回来;再 create 沙箱挂它,guest 看见同样的字节;destroy 沙箱后 `export` 仍逐字节相同 | 这是"生命周期解耦"的**兑现**,不是记录层面的解耦(§5.C) |
| 22 | **访问器的根限定**:`import` 一条 `../../etc/passwd` 形状的路径、以及一条指向卷外的 symlink 上的写,**都落在卷内**且宿主目标文件逐字节未变;`export` 同形 | `RESOLVE_IN_ROOT` 的语义是承重的;失效是**静默**的(写会"成功") |
| 23 | **写入 regime 的两向**:有 live-rw 挂载 → `import` **409**;把该沙箱 **pause** 后同一个 `import` **成功**,且 resume 后 guest 看见新字节 | `paused` 不算 live writer 是**明写的承诺**,不是可以收紧掉的细节(§5.C) |
| 24 | 并发 `import` 与 sweep:in-flight 标记让在飞的直连写不被当孤儿回收 | 标记与裁决同临界区(本文 §6) |

断言 4 属 `internal/store`(不需要 substrate);19、20 是单元/结构测试;**22 是纯单元测试**(访问器的根限定
不需要 runsc,只需要一个目录——这也是它便宜的一部分);其余是 `*_substrate_test.go`(`substrate` tag,
`-p 1`,需 root)。断言 15 需要一个故障注入点,照既有 `FaultAfterColdStart` 的形状。

---

## 8. 分期(单里程碑,内部三道门)

1. **门 A — 卷作为一等对象,不碰挂载。** seam + 两臂 + `volumes` 表 + `volumetest` 契约 + CRUD/PATCH 端点 +
   准入门那一半 + 孤儿卷字节 sweep + 结构守卫 + **`OpenRoot` 与 import/export 端点**。
   断言 1、7、8、11、16、17、19、20、**22**。
   **它可以独立交付**:一个能建、能改帽、能删、能**装进字节也能读出字节**、能被正确记账与回收的卷,
   只是还没人挂它。**写入 regime 的门此时不存在**——本门内没有 `mounts` 表,那条不变量**没有任何可被违反
   的方式**(不是"缓一缓",是本门内不存在写入方冲突);它随挂载表一起在门 B 落地。
2. **门 B — 挂载表与租约。** `mounts` 表 + 四个 sentinel + `DeleteSandbox` 的 FK 分支 + Memory 三处扫描 +
   五处 mediator 挂钩(含 `unwind` 删行)+ `LaunchSpec.Mounts` + OCI bind + Promote 的 per-Promote 入参 +
   **写入 regime 的门与 in-flight 标记**(挂载表一出现,那条不变量的触发者就出现了)。
   断言 2、3、4、5、6、9、10、18、**21、23、24**。**风险集中在 `unwind` 与删除次序**(那是留僵尸记录的两处)。
3. **门 C — snapshot / clone 与配额行回收。** `parent` + RESTRICT + 第四个 sentinel + `Snapshot`/`Clone`
   (含显式设帽 + 在飞集合 + 修复态)+ `<stale>` 行回收轮。断言 12、13、14、15。
   **排最后的理由是机制性的**:配额行泄漏的**唯一触发者**是 snapshot/clone,故这两件必须同门——把回收放进
   前面的门会实现一条本门内不可能被触发的路径(§1.3),把 snapshot 放前面则会先引入一个已知会漏的形状。

**降级时降得干净**:门 A 与门 B 各自是可用的交付物;若要砍,从门 C 整门砍(而不是砍它的一半)。

---

## 附. spec 权威指针

契约与模型:**§5.C**(Volume 全部已定契约:seam / 三种 AccessMode / 快照对象形状 / 卷帽写点 / 挂载行与独占
租约 / 两条唯一性断言 / sweep 安全要求)· **§3.4**(节点核算式两项跨 §5.B/§5.C 求和 / 帽必填 / 降帽 caveat)·
**§4.3**(对象回收 uniform invariant + 引用-回收原子性 + Volume lineage)· **§4.4-A**(原子退回源态、反向对账)·
**§6.1 / §6.2**(端点、四个 409、400 判据)。

推导与证伪:**volume.md** D1(物化与并发地基)· D2(seam 形状 + 第二臂)· D3(挂载表 / 租约 / 锁序)·
D4(访问模式,证伪心跳双回收)· D5(写入 regime,证伪反向代理)· **D6.1a(快照对象形状)**· D6.2(为何不
fsync/freeze)· D7(容量帽 + 回收程序)· D7.1(泄漏是正确性 bug)· D7.2(误 sweep 爆炸半径)·
**D7.3(帽的 limit 侧)**· D7.4(guest statfs)· D8(边界外待办)· D9(与参考实现的分歧)。
另:**rootfs-overlay D5**(挂载点占位与 excluded-paths)· D6.2/D6.4(squota 选型、为何不做 QuotaProvider)。

载体脚本(`test/preflight/`):`spike-volume-mount.sh`(bind 与并发/ro 臂)· `spike-live-volume-snapshot.sh`
(活卷快照一致性)· `spike-volume-cap-snapshot.sh`(帽的 limit 侧、降帽不可逆)· `spike-qgroup-reclaim.sh`
(配额行回收谓词)· `spike-volume-yank.sh`(误 sweep 爆炸半径)· `spike-mount-dest-upper.sh`(挂载点 × Promote)·
`spike-ro-snapshot-mount.sh`(只读快照臂;设计路径已不用 `-r`,留作失败模式记录)。
