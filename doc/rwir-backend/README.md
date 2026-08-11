# rwir 委托机制 —— 需求与实施方案

> 状态：阶段 1 已实现并验收（分支 `feat/delegation-rwir`），阶段 2/3 待做。
> 设计权威是 `deepx-design/doc/rwir-backend/`（01 注册发现 / 02 委托协议 /
> 03 三类场景 / 04 kvcpu 集成）。本文只讲**怎么落地**，以及**实现与设计的差异**。

## 一、需求

让 rwir 指令**不必全部由 kvlang runtime 解释执行**：有一张运行时可变的注册表，
决定哪些自己执行、哪些交给外部模块。载体是 kvspace —— 双方看的是同一个地址空间，
交换的是路径，不建 TCP/gRPC 直连。

这句话拆成两半：

- **共享状态提供可见性**：kvlang 的变量、调用栈、PC 全在 kvspace 里，外部进程
  连上同一个 store 就能看见。这一半 kvspace 已经给了。
- **委托提供调用语义**：看得见不等于能协作。外部进程还需要知道**算什么、拿什么
  算、结果写哪、怎么说算完**。委托就是加在共享存储上的这套调用约定。

一旦有了这套约定，三类差异极大的后端变成同一种东西 —— 都只是"连着这个空间、
按约定干活的进程"：

| 类别 | 例子 | 延迟 | 传参 | 特殊需求 |
|---|---|---|---|---|
| 计算委托 | `tensor.matmul(a,b) -> c` | µs–ms | by-ref（显存指针） | 融合编译、设备亲和 |
| API 封装 | `llm.chat(sid) -> resp` | s–10s | by-value | 重试、限流 |
| Agent 封装 | `agent.search(q) -> r` | min | 递归（spawn 子 vthread） | 可再委托 |

调用点三者写法完全一致，写代码的人不需要知道区别。

## 二、已实现（阶段 1）

分支 `feat/delegation-rwir`，四个 commit：

```
c0bcf82  feat: 委托判据改为后端注册表 —— 哪些 rwir 交出去由运行时的表决定
129101c  feat: 写落点校验 —— 程序不再能写引擎自己的状态
04265c3  feat: 委托 rwir —— 把只有签名的算子交给外部进程求值
468236a  chore: kvspace-go 依赖回到含 art 子包的版本
```

**判据**（`rwir/dispatch/router.go`）：有任一**在岗**后端在
`/sys/rwir-backend/<b>/op/<opcode>` 注册了该算子即委托。在岗 = status 为
`ready` 或 `busy`；`offline` 或缺失都不算。判据在运行时、由后端自报 ——
起一个后端该算子当场可委托，源码一个字不用改；后端全部下线则回落本地实现。

**调度链**（`kvcpu/execute.go`）：控制流 → 内建 → copy → **委托** → 用户函数。

**协议**：任务推 `/sys/rwir-backend/<b>/cmd`，完成信号 `/done/rwir/<request_id>`，
`request_id` 格式 `rwir:<backend>:<vtid>:<seq>`。执行器必须**先写 outputs、
再置 status、最后才发信号**。

> ⚠ **线上 JSON 里这个字段现在叫 `id`，不叫 `request_id`**（值的格式已符合设计）。
> 写后端解析的按 `id` 来。重命名列在阶段 2。

**写落点校验**（`keytree/writekey.go`）：程序只能写自己 vthread 子树内的槽位与
用户全局键；`/lib`（代码区）、`/sys`（注册表与任务对象）、`/dev`（任意文件写与
SSRF 的 sink）、`/done`、别的 vthread、任何 `‥` 打头的引擎保留键，一律拒绝。
这不是附加功能，是"代码与数据同域"的必然推论 —— `/lib` 就在同一个空间里，
agent 能写代码进去。

**验收**（阶段 1 全部实跑通过）：`go build` exit 0；`go vet` 55 条 unkeyed
（基线不变，无其他类别）；`go test -shuffle=on` 连跑 5 次全绿、`-race` 干净；
tutorial `PASS:128 FAIL:1`（唯一失败是既有的 `01-basics/time.kv`，与本改动无关）；
`examples/delegate/run.sh`（真 redis + 独立 python 执行器）**连跑 3 次，3 次
均 PASS** —— 这是"跑了 3 遍"的意思，脚本本身只输出一行 PASS，不是它报的比分。
这一条依赖 `redis-server` 与 kvspace CLI 都在 PATH 上；两者缺任一就跑不了，
复核时若没装请直接标"未跑"，别拿别的东西凑。

## 三、与设计文档的差异

### 3.1 刻意偏离（三处，请复核）

**A. copy 排在委托之前。** 文档 04 的调度链是委托第 3、copy 第 5。按那个顺序，
一个注册了 opcode `=` 的后端能接管程序里**每一次赋值** —— 它拿到被赋的值和写槽
绝对路径，写什么变量就是什么。赋值是 VM 原语，和控制流、内建同类，不该被注册表
夺走。副带好处：纯赋值指令不再需要扫注册表。

> 实测：把委托分支挪回文档顺序，`TestVMPrimitivesBeatRegistry/赋值_=` 当场变红，
> 注册 `=` 的后端收到
> `{"opcode":"=","inputs":[{"value":"7"}],"outputs":[{"key":"/vthread/…/x"}]}`。
> 该用例经变异检验，不是自欺。

**B. 保留持久 `/sys/task/<id>.status`。** 文档 02 的成功判据是 done 信号的 JSON
载荷。实现里信号只负责唤醒，判成功靠这个持久键 + 输出槽非空 —— 信号丢失不会被
误判成功。文档没有这一层。

**C. 字面量留在 `inputs[].value`，不挪进 `params`。** 文档 `resolveParams` 的伪码
说字面量放 params，但它给的**所有** JSON 例子里 params 装的都是配置
（`transpose_a` / `max_tokens` / `retry`）。挪进 map 会丢位置信息 ——
`matmul(a, 2.0)` 的后端无从知道 `2.0` 是第几个参数，参数个数校验随之失效。

### 3.2 文档自身的问题（建议改文档）

**D. 注册时序建议反过来。** 文档 01 让 status 第 3 步写、op 子键第 5 步写；
实现是反的（op 先写、status 最后写，见 `fake_backend.py`）。

要说清楚的是：**两种顺序都有"注册未完成"的窗口**（见 R1），反序并没有消掉它。
反序买到的是另一件事 —— status 是**最后**一个落地的键，于是它可以当就绪信号用：
`run.sh` 就是靠轮询 `status=ready` 来等后端进入 Watch 循环的。按文档顺序的话
status 先亮，任务会被投给一个还没开始 Watch 的进程，白等一个超时周期。

**E. 幂等方案不成立。** 文档 04 一边说完成信号"kvcpu 消费即消失"，一边又让后端
崩溃恢复时"检查 `/done/rwir/<id>` 是否存在"来去重 —— 已被消费的一次性信号永远
不存在，这条去重永远失效。**幂等目前是缺的**，需要定方案：成功后保留 status 键
并加 TTL？还是让完成信号落成持久键？

**F. `tensor.* → Compute` 无条件兜底会误伤。** 文档 04 Phase 1 想保留一条
`strings.HasPrefix(opcode,"tensor.")` 的兜底分支。那会连用户自己起名叫 `tensor`
的库一起劫持（仓库测试 `TestTensorLibIsNotDelegated` 正是钉这个）。要恢复兜底
必须先判本地有没有定义。

### 3.3 尚未实现（阶段 2/3）

| 项 | 文档 | 现状 | 影响 |
|---|---|---|---|
| `inputs[].key` | 02 | 只发 `value` | 后端拿不到路径，无法自己 Get |
| `dtype` / `shape` | 02 | 缺 | GPU 后端无类型信息 |
| `address{node,device,ptr,byte_size}` | 02 | 缺 | **计算委托整条路断着** |
| `params` | 02 | 缺 | 配置项无处安放 |
| 心跳（5s 更新 / 15s 超时回写 offline） | 01 | 缺 | 死后端不会被自动标 offline |
| 按 category 分档超时（30/120/300s） | 02 | 单一 30s | agent 类必然超时 |
| 幂等 | 02/04 | 缺 | 见 E |
| `delegationCache` | 04 | 缺 | 每条**非原语**指令扫一遍注册表（控制流/内建/copy 在委托前短路） |
| 设备亲和 `SelectWithAffinity` | 03 | 缺 | GPU 多卡路由 |

## 四、下一步方案

### 阶段 2：补齐线协议（优先，其余都依赖它）

目标是让**计算委托**这一类真的能跑 —— 三类后端里差异最大的一档，缺了它
"统一异构委托"就只剩 API 和 agent 两类，撑不起这个说法。

1. `ParamRef` 恢复 `Key` / `Dtype` / `Shape` / `Address`（这些字段 master 上
   本来就有，是阶段 1 重写 `Compute → Delegate` 时被我删掉的）
2. 读参从"解引用后的值"改为"绝对路径 + 元信息"；字面量仍留 `value`（见 C）
3. `OpTask` 加 `Params`；JSON 字段名 `id` → `request_id`
4. 同步改 `fake_backend.py`，并加一个最小的按 key 读值的后端路径

**动手前先看这两处，否则会卡住**：

- **`TestOpTaskWireFormat`（`rwir/dispatch/delegate_test.go`）把字段集双向钉死**
  （`{id,vtid,pc,opcode,inputs,outputs,done_key}`），多一个字段就报"线协议多出
  未声明字段"。加 `params`、改 `id → request_id` 必然让它变红 —— 那是设计如此，
  改协议时同步改这条用例，别绕开它。
- **`done_key` 的去留要一并决定**。它是实现自加的字段，设计文档 02 的 RwirTask
  没有（后端应自行由 request_id 推出 `/done/rwir/<id>`）。留着的理由写在
  `fake_backend.py`：键布局只该有一个来源。做重命名时顺带定掉。

**门禁**：阶段 1 的全套 + 跨进程样例改成"后端按 key 去 Get 输入"仍 PASS。

### 阶段 3：可用性

心跳与自动下线 → 按 category 分档超时 → 幂等（等 E 定方案）→ `delegationCache`
（注意失效：文档说 Watch 注册表变更，但后端下线只改 status 不删键，缓存必须
连 status 一起失效，否则下线后判据永远为真）。

### 阶段 4：三类场景各一个真后端

计算（GPU）/ API（LLM）/ Agent（子 vthread）。这一步才真正验证"一套协议跨三档"。

## 五、已知风险

**R1（设计级，需拍板）**：后端注册中途的窗口里，若 `/lib` 有同名 rwfunc，程序会
**静默跑本地实现且 exit 0**。这是"注册表判据 + 后端盖过本地"的固有竞态 ——
无论 op 先写还是 status 先写都存在窗口。要么接受，要么规定同名冲突直接报错。

**R1b**：失败的任务状态键**永不回收**。`Delegate` 只在成功路径 `Del` 掉
`/sys/task/<id>.status`，失败的留着给人排查 —— 长跑下来会一直堆积。这与 §3.1 B
打算让这个键承担幂等（成功后保留并加 TTL）是同一处设计，两个方向要一起想。

**R2**：`taskID` 的唯一性只在单进程内成立。`vthread.AllocVtid` 底层是
Get-then-Set 非原子，两个 kvlang 进程连同一个 redis 并发启动会拿到同一个 vtid。
这是既有问题，不是委托引入的，但委托的唯一性论证建立在它之上。

**R3（合流阻断，最要紧的一条）**：**上游把 `art` 后端删了，而本分支 5 处依赖它。**

kvspace-go 三个版本的子包互不重合：

| 版本 | 子包 | 谁在用 |
|---|---|---|
| `v0.1.0`（唯一的 tag） | `cmd` `redis` | master 的 go.mod 钉的 |
| `v0.1.1-0.20260803065547` | **`art`** `cmd` `redis` `tutorial` | 本分支钉的 |
| `v0.1.1-0.20260811111044` | `cmd` **`goheap`** `redis` **`shm`** `tutorial` | master 的 import 需要的 |

**没有任何一版同时有 `art` 和 `goheap`/`shm`。** 上游在
`refactor: 移除 art backend (迁至 shm)` 里删掉了 art，而本分支
`cmd/kvlang/main.go:19` 加 4 个测试文件都 `import _ ".../art"`，
全部测试也都跑在 `art://` 上。这才是真正的阻断。

次要分歧：master 的 `kv.List` 是三参数（`List(path, false, true)`），本分支是
两参数 —— 两边调用不可能同时编译通过。

**对位替代是 `goheap`，不是 shm**：`goheap` 是纯 Go 的进程内后端
（`kvspace.Register("goheap", Conn)`，无 cgo），与 art 定位相同；`shm` 要
`#include "kvspace/kvspace.h"`，得先构建 kvspace-c。

**顺带纠正一条容易走弯路的判断**：master 编译不过**不是**因为那两个子包"没发布"。
`go get github.com/array2d/kvspace-go@master` 一条命令就能解决 missing-package
（实跑：`upgraded v0.1.0 => v0.1.1-0.20260811111044`，exit 0）。用伪版本本来就是
常规做法 —— 本分支自己钉的也是伪版本。别去要 owner "发一个带 goheap/shm 的 tag"，
东西早就有了。

依赖升上去之后，master 自身还有两处编译不过（实跑）：

```
shm/shm.go:9:10: fatal error: kvspace/kvspace.h: No such file or directory
layout/layout.go:325:66: too many arguments in call to kvspace.NewRwfunc
    have (number, number, number, string)
```

即 master 的 `layout.go` 也没跟上新库的 `NewRwfunc` 签名。

本分支 `feat/delegation-rwir` 自身 `go build ./...` exit 0（钉在旧伪版本上）。

**结论：现在无法 rebase。** 顺序应该是——
① owner 确认 art 的替代就是 goheap；② 本分支把 5 处 `art` 换成 `goheap`、
`art://` 换成 `goheap://`、`kv.List` 补第三个参数；③ master 那边修
`NewRwfunc` 与 shm 的 cgo 依赖；④ 两边都钉同一个伪版本，再谈合流。
在那之前本分支继续基于 `04db8b8` 推进。

## 六、给复核者：验证方式

阶段 1 的每条结论都可复跑。注意两个坑，缺一都会得出反向结论：

- **默认 DSN 是 `redis://` 而非 `art://`**（`cmd/kvlang/util.go`）。art 后端对
  非规范路径是 **panic 而非报错**（`mustValidDirPath`），而全仓零处 `recover`；
  redis 后端只在出现 `//` 时 panic（`redis/kvspace.go` 的 Set 路径），Get 路径
  不校验。同一段程序在两个后端下行为不同 —— 跨进程样例必须 redis（art 是进程内
  的，外部进程看不见 `/sys`），而路径类的坑只在 art 下现形。
  **注意**：art 已被上游删除（见 R3），迁到 `goheap` 之后这条要重新评估 ——
  goheap 是否同样 panic、是否同样校验，都得重验，不能沿用本文的结论。
- **`.gitignore` 里有裸的一行 `kvlang`**，它同时匹配了 `cmd/kvlang/` 整个目录。
  任何尊重 .gitignore 的检索工具（ripgrep 默认、编辑器搜索、包装成 ugrep 的
  shell grep）都会静默丢掉整个 CLI 包。全仓搜索一律用 `command grep`。

parser 有既有死循环 bug，两种形态写测试语料时要避开：
`for (x in [7,2,9,4])` 和 `d = { "ab" = 1 }`。命令一律加 timeout。
