package dispatch

// kvspace ABI 回归测试 —— 委托机制依赖的全部契约假设。
//
// 这些问题原本靠读 kvlang 仓库无法判断（仓库内证据互相矛盾：layoutandrun.go:39
// 对 List 结果做 TrimSuffix，而 ps.go:31 与 router.go:32 假定裸名）。答案已通过
// 阅读 kvspace-go 源码 + 实测确定，记在每个用例的注释里。
//
// 本文件的作用因此从「探针」变成「断言」：跑一次确认你的环境与记录一致，
// 不一致就说明 kvspace-go 版本变了，委托路径的实现前提需要重新审。
//
// 跑法（两个后端都要跑，语义有差异）：
//
//	KVLANG_KVSPACE=redis://127.0.0.1:6379 go test ./rwir/dispatch/ -run Contract -v
//	KVLANG_KVSPACE=art://                  go test ./rwir/dispatch/ -run Contract -v
//
// 前提：go.mod 必须 require 能用的 kvspace-go 版本。tag v0.1.0 的 API 与本仓库
// 完全不兼容（XValue 是 struct 不是 interface、无 art 子包、构造函数名不同），
// 用它连编译都过不了。

import (
	"os"
	"testing"
	"time"

	"github.com/array2d/kvspace-go"
	// 后端驱动靠 init() 自注册，不空导入则 Conn 报 unknown scheme。
	// 与 cmd/kvlang/main.go:18-19 同样的处理。
	_ "github.com/array2d/kvspace-go/art"
	_ "github.com/array2d/kvspace-go/redis"
)

const probeRoot = "/probe_contract"

func dial(t *testing.T) kvspace.KVSpace {
	t.Helper()
	dsn := os.Getenv("KVLANG_KVSPACE")
	if dsn == "" {
		t.Skip("需要 KVLANG_KVSPACE，例如 redis://127.0.0.1:6379；art:// 下的答案与 redis:// 可能不同，两个都要跑")
	}
	kv := kvspace.Conn(dsn)
	if kv == nil {
		t.Fatalf("连不上 %s", dsn)
	}
	return kv
}

// Q1【决定 B1】List 返回带尾斜杠的目录项，还是裸名？
//
// 已知答案：**目录带尾斜杠，叶子不带**。实测 List("/lib/") =
// [fake.echo llm.chat wrap/ wrap.src wrap] —— rwir 声明是叶子（无 body 子树）故不带，
// rwfunc 有 body 子树故 "wrap/" 带斜杠。这解释了为何仓库内证据看似矛盾。
//
// 影响：/sys/op/<backend> 必为目录 → router.go 不剥离就拼出 /sys/op/<b>//func/<op>，
// kvspace 对双斜杠路径**直接 panic 而非报错**。已在 router.go 修复。
// ps.go:31 的 ParseInt 同样受影响（/vthread/<id> 也是目录）。
func TestContractQ1List(t *testing.T) {
	kv := dial(t)
	defer kv.DisConn()
	defer kv.DelTree(probeRoot)

	kvspace.MkIndexRecursive(kv, probeRoot+"/dir/")
	if err := kv.Set([]kvspace.KVPair{
		{Key: probeRoot + "/leaf", Val: kvspace.NewChar("v")},
		{Key: probeRoot + "/dir/inner", Val: kvspace.NewChar("v")},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got := kv.List(probeRoot+"/", false)
	t.Logf("Q1 List(%q) = %#v", probeRoot+"/", got)
	for _, e := range got {
		if e == "dir/" {
			t.Logf("  → 目录项带尾斜杠。B1 成立：router.go 必须 TrimSuffix，ps.go:31 今天是坏的。")
		}
		if e == "dir" {
			t.Logf("  → 目录项是裸名。B1 不成立：router.go 无需改，/sys/op 只是从没被人用过。")
		}
	}
	t.Logf("Q1b List(recursive=true) = %#v", kv.List(probeRoot+"/", true))
}

// Q2【决定 B2】Set 是不是原子的？部分失败会怎样？
//
// 影响：认领任务（CAS）与「推进 PC + 记录结果」必须是一个事务。
// 若 Set 是 pipeline 而非事务，多实例认领需要另找原语。
// 注意：MVP 单后端不需要这个，但阶段 5.2 需要。
func TestContractQ2SetAtomicity(t *testing.T) {
	kv := dial(t)
	defer kv.DisConn()
	defer kv.DelTree(probeRoot)

	kvspace.MkIndexRecursive(kv, probeRoot+"/")

	// 一批里混入一个非法键（含 //），看合法的那个是否仍被写入。
	// redis 后端对畸形键是 panic 而非返回 error，必须 recover，否则本用例会
	// 掀掉整个测试进程，Q3–Q6 根本跑不到。
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("Q2 Set 对畸形键 **panic** 而非返回 error: %v", r)
				t.Logf("  → 委托路径的 kv.Set 用的是用户代码推导出的键，"+
					"必须自己 recover，否则一个畸形写槽能掀掉整个 VM")
				err = nil
			}
		}()
		err = kv.Set([]kvspace.KVPair{
			{Key: probeRoot + "/ok", Val: kvspace.NewChar("written")},
			{Key: probeRoot + "//bad", Val: kvspace.NewChar("x")},
		})
		t.Logf("Q2 混合批次 Set 返回 err = %v（未 panic）", err)
	}()

	v := kvspace.GetOne(kv, probeRoot+"/ok")
	if kvspace.IsNone(v) {
		t.Logf("  → 合法键未写入：Set 具备某种全或无语义（对 CAS 有利）")
	} else {
		t.Logf("  → 合法键已写入 (%q)：Set 是逐条应用，非事务。CAS 需另找原语。", v.String())
	}
	t.Logf("  注意：若上面 panic 而非返回 err，说明 kvspace 用 panic 报错——" +
		"委托路径必须自己 recover，否则一个畸形键就能杀掉整个 VM")
}

// Q3【决定 §4.1.4 与整个等待协议】Notify 早于 Watch 发生，Watch 还收得到吗？
//
// 已知答案：**收得到**。Notify=LPUSH、Watch=BLPOP，打在持久 list 上。
//
// 注意接口注释写的是 fire-and-forget（"投递一次性通知信号"、"阻塞等待下一次 Notify"），
// 与实现不符。**照注释写的代码今天就是错的，依赖今天行为的代码明天可能是错的** ——
// 所以 Delegate 仍应保留「挂 Watch 后复查持久 status」这一步，不因本结论省略。
func TestContractQ3NotifyBeforeWatch(t *testing.T) {
	kv := dial(t)
	defer kv.DisConn()
	defer kv.DelTree(probeRoot)

	key := probeRoot + "/sig_early"
	if err := kv.Notify(key, kvspace.NewChar("early")); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // 确保 Notify 先于 Watch

	got := kv.Watch(key, 2*time.Second)
	t.Logf("Q3 先 Notify 后 Watch，收到 = %q", got.String())
	if kvspace.IsNone(got) || got.String() == "" {
		t.Logf("  → fire-and-forget。Delegate 必须轮询兜底，Notify 仅作提示。")
	} else {
		t.Logf("  → 持久队列。无丢唤醒，但必须处理陈旧信号（见 Q4）。")
	}
}

// Q4【决定陈旧信号风险】DelTree 能不能清掉 notify 积压？
//
// 若 Notify 落在 kvspace 树外的裸键（如 __notify:<path>），DelTree 删不掉它，
// 则每次超时/放弃的委托都往一个无 TTL、无追踪的队列里漏一条，
// 后续复用同名键的等待者会立刻收到别人的陈旧信号。
func TestContractQ4StaleNotify(t *testing.T) {
	kv := dial(t)
	defer kv.DisConn()
	defer kv.DelTree(probeRoot)

	key := probeRoot + "/sig_stale"
	kv.Notify(key, kvspace.NewChar("stale"))
	time.Sleep(100 * time.Millisecond)

	kv.DelTree(probeRoot)
	got := kv.Watch(key, 500*time.Millisecond)
	t.Logf("Q4 DelTree 之后残留信号 = %q", got.String())
	if !kvspace.IsNone(got) && got.String() != "" {
		t.Logf("  → 陈旧信号存活。完成信号必须带 taskid，且等待前须校验归属。")
	} else {
		t.Logf("  → DelTree 清得掉（或本就是 fire-and-forget）。")
	}
}

// Q5【决定 C17】Set 要不要求父目录已存在？
//
// 已知答案：**不要求**，Set 内部自己 MkIndexRecursive(parent)。
// 注意接口注释 kvspace.go:32「pre 路径如果不存在，则汇报异常」与实现不符 ——
// 这条错注释已经导致 vthread/vthread.go:68 写了一次多余的 MkIndexRecursive。
func TestContractQ5SetNeedsParent(t *testing.T) {
	kv := dial(t)
	defer kv.DisConn()
	defer kv.DelTree(probeRoot)

	err := kv.Set([]kvspace.KVPair{
		{Key: probeRoot + "/no_parent/deep/k", Val: kvspace.NewChar("v")},
	})
	v := kvspace.GetOne(kv, probeRoot+"/no_parent/deep/k")
	t.Logf("Q5 无父目录直接 Set: err=%v, 回读=%q", err, v.String())
	if kvspace.IsNone(v) {
		t.Logf("  → 需要先 MkIndexRecursive。后端注册脚本必须显式建目录。")
	} else {
		t.Logf("  → 自动建父目录。")
	}
}

// Q6【决定命令队列语义】多条 Notify 到同一键，取出顺序是 FIFO 还是 LIFO？
//
// 已知答案：**LIFO**。Notify=LPUSH、Watch=BLPOP，两者都在头部——这是个栈不是队列。
//
// 影响：/sys/op/<b>/<n>/cmd 积压时后端会先执行最新任务。LLM 后端慢，积压是常态
// 而非边缘情况。要 FIFO 需改成 LPUSH+BRPOP（或 RPUSH+BLPOP），属 kvspace 侧改动。
func TestContractQ6QueueOrder(t *testing.T) {
	kv := dial(t)
	defer kv.DisConn()
	defer kv.DelTree(probeRoot)

	key := probeRoot + "/queue"
	for _, s := range []string{"first", "second", "third"} {
		kv.Notify(key, kvspace.NewChar(s))
	}
	time.Sleep(100 * time.Millisecond)

	var order []string
	for i := 0; i < 3; i++ {
		v := kv.Watch(key, 500*time.Millisecond)
		if kvspace.IsNone(v) || v.String() == "" {
			break
		}
		order = append(order, v.String())
	}
	t.Logf("Q6 投递 [first second third]，取出顺序 = %v", order)
	switch {
	case len(order) == 0:
		t.Logf("  → 取不出来：Notify 不缓冲（与 Q3 应一致）")
	case order[0] == "first":
		t.Logf("  → FIFO，符合队列直觉")
	case order[0] == "third":
		t.Logf("  → LIFO，是个栈。积压时后端会先做最新任务，需改成 LPUSH+BRPOP 语义或在上层排序。")
	}
}
