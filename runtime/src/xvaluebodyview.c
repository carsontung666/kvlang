/* P1b4-b1a：shm xvalue body view 的 runtime 薄封装（晚链接 TU，控制热 TU 布局）。 */

#include "xvaluebodyview.h"
#include "func_code.h"

__attribute__((visibility("hidden"))) int
kvlangXvalueBodyViewBind(kvspace_t *kv, kvspaceKeyOff *ko,
                       kvspaceXvalueBodyView_t *view) {
    /* ref 未解析（槽未创建）时不取 view：后端零 ref 会落到错误叶子。 */
    if (!kv || !ko || !view || !ko->key || !ko->resolved)
        return -1;
    return kvspaceXvalueBodyView(kv->handle, &ko->off, ko->key, view);
}

__attribute__((visibility("hidden"))) bool
kvlangStatusXvalueBodyViewRead(const kvspaceXvalueBodyView_t *view, char *out,
                          size_t cap) {
    if (!view || !view->head || !out || cap == 0)
        return false;
    /* 规范 status：v2 SLACK inline 固定头；任何不自洽都不暴露。 */
    if (view->headlen < KVSPACE_XVALUE_HEAD_LEN ||
        xh_storetype(view->head) != KVSPACE_XVALUE_ST_SLACK ||
        xh_ptr(view->head) != KVSPACE_XVALUE_PTR_INLINE)
        return false;
    uint64_t n = xh_len(view->head);
    if (n > view->body_cap || n + 1 > cap)
        return false;
    memcpy(out, view->body, (size_t)n);
    out[n] = 0;
    return true;
}

/* ── b1b：PC owned view ─────────────────────────────────────────────── */

#define PC_FIXED_CAP KV_KEY_MAX /* 1024 */

__attribute__((visibility("hidden"))) void
kvlangPcViewInit(kvlangPcViewState *st) {
    memset(st, 0, sizeof *st);
}

__attribute__((visibility("hidden"))) int
kvlangPcViewWrite(kvlangFrame_t *f, kvspaceKeyOff *ko, const char *pc,
                      uint32_t len) {
    (void)f;
    (void)ko;
    (void)pc;
    (void)len;
    /* General-path PC updates go through pc_write_whole so length/prefix
     * changes swap via a fresh SBO box without mid-memcpy tearing. */
    return 0;
}

/* ── T-120 A：lazy operand 只读 view ────────────────────────────────── */

/* kind/width 由 row 装载期固化（见 kvlangCodeRow_t.view_pack）。
 * pin cell 允许：pin 只在 handler 前把读值 copy 一份，view 在慢写后由既有
 * Invalidate* 失效；iops 的 `a = a + 1` 读参正是 pin。 */

/* 提升：一次 calloc + capture。失败解注册并释放，调用方清零计数重来。 */
#ifdef KV_COUNT
unsigned long kvCReadViewProm, kvCReadViewDemote, kvCReadViewHit;
#endif

static bool rv_promote(kvspace_t *kv, kvlangBoundCell_t *cell) {
    kvlangReadViewSlot *rs = calloc(1, sizeof *rs);
    if (!rs)
        return false;
    kvlangBodySlot *slot = &rs->slot;
    slot->key = cell->key;
    slot->kind = kvlangLangtypeKind(cell->view.kind);
    slot->kind_len = slot->kind ? (uint8_t)strlen(slot->kind) : 0;
    slot->width = cell->view.width;
    slot->off = &cell->off;
    /* 读 cell 的 key 是帧内绝对路径；显式取 /vthread/<vid> 前缀交给
     * capture（global/绝对 key 取不到前缀 → 拒绝提升，不隐式借用 key）。 */
    char root[KV_KEY_MAX];
    if (!slot->kind || !slot->kind[0] || !slot->width ||
        !kvlangVthreadRootOfKey(cell->key, root, sizeof root) ||
        !kvlangBodyCapture(kv, slot, root, &rs->token)) {
        kvlangReadViewSlotFree(rs);
        return false;
    }
    slot->ref = cell->off.off;
    cell->off.fast = slot;
#ifdef KV_COUNT
    kvCReadViewProm++;
#endif
    cell->view.hits = KV_READ_VIEW_RECHECK;
    cell->view_state = 2;
    return true;
}

static void rv_demote(kvlangBoundCell_t *cell) {
#ifdef KV_COUNT
    kvCReadViewDemote++;
#endif
    kvlangReadViewSlotFree((kvlangReadViewSlot *)cell->off.fast);
    cell->off.fast = NULL;
    cell->view.hits = 0;
    cell->view_state = 1;
}

/* 每 RECHECK 次命中重验一次：版本化 Check（gen/epoch/has_value/box）；
 * STALE → Refresh 原地重解析并重新校验形状。 */
static bool rv_recheck(kvspace_t *kv, kvlangBoundCell_t *cell) {
    kvlangReadViewSlot *rs = (kvlangReadViewSlot *)cell->off.fast;
    int rc = kvspaceViewCheck(kv->handle, &rs->token);
    if (rc == 0)
        return true;
    if (rc != 1)
        return false;                  /* -2/-1：回退 */
    if (kvspaceViewRefresh(kv->handle, &rs->token, cell->key) != 0)
        return false;
    uint32_t tcap =
        rs->token.cap > UINT32_MAX ? UINT32_MAX : (uint32_t)rs->token.cap;
    if (!kvlangBodyAdopt(&rs->slot, rs->token.head, tcap))
        return false;
    return true;
}

/* 立即 ack（bracket 退出时用：token 已在进入时作废，无需再失效）。 */
__attribute__((visibility("hidden"))) void
kvlangViewAckNow(kvspace_t *kv) {
    static uint32_t ack_epoch;
    if (!kv || !kv->shm || !kv->handle)
        return;
    for (;;) {
        uint32_t ep = ack_epoch;
        int rc = kvspaceViewAck(kv->handle, &ep);
        if (rc == 0) {
            ack_epoch = ep;
            return;
        }
        if (rc != 1)
            return;
        ack_epoch = ep;
    }
}

/* Q1：op 边界 ack。epoch 变了先作废全部 body slot，两步重试后再 ack。 */
__attribute__((visibility("hidden"))) void
kvlangViewQuiesce(kvspace_t *kv) {
    static uint32_t view_epoch;
    if (!kv || !kv->shm || !kv->handle)
        return;
    for (;;) {
        uint32_t ep = view_epoch;
        int rc = kvspaceViewAck(kv->handle, &ep);
        if (rc == 0) {
            view_epoch = ep;
            return;
        }
        if (rc != 1)
            return;                    /* -2：后端无 view，不 ack */
        kvlangBodyInvalidateAll();
        view_epoch = ep;
    }
}

__attribute__((visibility("hidden"))) int
kvlangReadViewTry(kvspace_t *kv, struct kvlangBoundCell *cell, uint32_t pack,
                  kvlangXvalue_t *out) {
    if (!kv || !kv->shm)
        return 0;
    switch (cell->view_state) {
    case 3:
        return 0;
    case 0:
        cell->view.kind = (uint8_t)(pack & 0xffu);
        cell->view.width = (uint16_t)(pack >> 8);
        cell->view_state = 1;
        cell->view.hits = 0;
        /* fall through */
    case 1:
        if (++cell->view.hits < KV_READ_VIEW_N)
            return 0;
        if (!rv_promote(kv, cell)) {
            cell->view.hits = 0;
            return 0;
        }
        break;
    case 2:
        break;
    default:
        cell->view_state = 3;
        return 0;
    }
    kvlangBodySlot *s = (kvlangBodySlot *)cell->off.fast;
    if (!s || !s->ok || !s->head || !s->headlen) {
        if (s)
            rv_demote(cell);
        else
            cell->view_state = 1;
        return 0;
    }
    if (--cell->view.hits == 0) {
        cell->view.hits = KV_READ_VIEW_RECHECK;
        if (!rv_recheck(kv, cell)) {
            rv_demote(cell);
            return 0;
        }
    }
#ifdef KV_COUNT
    kvCReadViewHit++;
#endif
#ifdef KV_DEBUG_TYPES
    {
        kvlangReadViewSlot *rs = (kvlangReadViewSlot *)s;
        uint32_t cap =
            rs->token.cap > UINT32_MAX ? UINT32_MAX : (uint32_t)rs->token.cap;
        kvlangXvalueSetWire(out, s->head, cap);
    }
#else
    out->bytes = s->head;
    out->length = s->headlen + s->width;
#endif
    return 1;
}
