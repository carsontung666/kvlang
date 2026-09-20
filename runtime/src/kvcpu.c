#include "runtime_internal.h"

/* 找末个成员分隔符（·，多字节），对齐 strrchr('.') 的单字节旧语义。 */
static const char *rfind_sep(const char *s) {
    const char *found = NULL, *p = s;
    while ((p = strstr(p, MEMBER_SEP)) != NULL) {
        found = p;
        p += MEMBER_SEP_LEN;
    }
    return found;
}

/* goto/br 目标：layout 已把 label 解析为 int64 irseq（≥1）。非 int64 / 越界返回 -1。 */
static int irseq_of(const kvlangParam_t *p) {
    if (kvlangXvalueNone(&p->val) || !kvlangXvalueKindIs(&p->val, KVSPACE_KIND_INT64)) return -1;
    int64_t n = kvlangXvalueAsInt64(&p->val);
    if (n < 1 || n > 0x7fffffff) return -1;
    return (int)n;
}

static int jump_to(kvlangKv_t *kv, const char *vtid, const char *pc, const kvlangParam_t *target, const char *op) {
    int irseq = irseq_of(target);
    if (irseq < 0) {
        char msg[256];
        snprintf(msg, sizeof msg, "RuntimeError: %s target is not an int64 irseq: %s (kind=%s)",
                 op, target->name ? target->name : "", kvlangXvalueKind(&target->val));
        kvlangVthreadSetError(kv, vtid, pc, msg);
        return -1;
    }
    int d = 1, cur_i = 1;
    if (kvlangKeytreeParsePc(pc, &d, &cur_i) != 0)
        d = kvlangKeytreeFrameNum(pc);
    kvlangVthreadWritePc(kv, vtid, d, irseq);
    kvlangLogDebug("[%s] %s → [%d]/[%d,0]", vtid, op, d, irseq);
    return 0;
}

static bool is_literal(const char *s) {
    if (!s || !s[0]) return false;
    return s[0] == '"' || s[0] == '/' || strcmp(s, "true") == 0 || strcmp(s, "false") == 0 ||
           strcmp(s, "null") == 0 || (s[0] >= '0' && s[0] <= '9') || (s[0] == '-' && s[1]);
}

/* 派发期读参类型校验（runtime篇-07 第八节）：把每个实参的 kind 逐一匹配
 * rwir/rwfunc 定义的读参 kindexp。def_sig 为读参 kindexp 在前的 \n 分隔列表，
 * def_nr 为定义读参数。空 kindexp / any 跳过；末读参 "..." 变参吸收其后全部实参。
 * XValue 头只携带 array_len 不含多维 shape，故仅校验 kind 层。
 * 不匹配 → 置 TypeError，返回 -1；通过返回 0。 */
static int check_read_types(kvlangKv_t *kv, const char *vtid, const char *pc,
                            const char *opcode, const char *def_sig, int def_nr,
                            kvlangParam_t *args, int nargs) {
    if (def_nr <= 0 || !def_sig || !*def_sig) return 0;
    char *dup = strdup(def_sig);
    char *reads[128];
    int rn = 0;
    for (char *s = dup; rn < def_nr && rn < 128; ) {
        reads[rn++] = s;
        char *nl = strchr(s, '\n');
        if (!nl) break;
        *nl = 0; s = nl + 1;
    }
    bool var_last = rn > 0 && kvlang_rwirextKindexprVariadic(reads[rn - 1]);
    int min_args = var_last ? rn - 1 : rn;
    char *fr = kvlangKeytreeFrameRoot(pc);
    int rc = 0;
    if (nargs < min_args) {
        char msg[256];
        snprintf(msg, sizeof msg, "TypeError: %s expects %d args, got %d", opcode, min_args, nargs);
        kvlangVthreadSetError(kv, vtid, pc, msg);
        rc = -1;
    }
    for (int i = 0; rc == 0 && i < nargs; i++) {
        const char *exp = i < rn ? reads[i] : (var_last ? reads[rn - 1] : NULL);
        if (!exp) {
            char msg[256];
            snprintf(msg, sizeof msg, "TypeError: %s expects %d args, got %d", opcode, rn, nargs);
            kvlangVthreadSetError(kv, vtid, pc, msg);
            rc = -1;
            break;
        }
        if (!exp[0] || !kvlang_rwirextKindexprValid(exp)) continue;   /* 动态/非法 kindexp 跳过 */
        kvlangXvalue_t v; kvlangXvalueZero(&v);
        kvlangBuiltinResolveReadValue(kv, fr, args[i].name, &args[i].val, &v);
        const char *k = kvlangXvalueKind(&v);
        kvspaceHead_t h; kvlangXvalueHead(&v, &h);
        kvlang_kindexpr_t kx; kvlang_kindexpr_parse(h.kindexpr, &kx);
        bool ok = kvlang_rwirextKindexprMatch(exp, k, kx.ndim, kx.dims);
        char kbuf[40]; snprintf(kbuf, sizeof kbuf, "%s", k[0] ? k : "None");
        kvlangXvalueFree(&v);
        if (!ok) {
            char msg[256];
            snprintf(msg, sizeof msg, "TypeError: %s arg %d: expected %s, got %s", opcode, i + 1, exp, kbuf);
            kvlangVthreadSetError(kv, vtid, pc, msg);
            rc = -1;
        }
    }
    free(fr); free(dup);
    return rc;
}

/* 读取 rwir/rwfunc 定义体的 kindexp-list（nr/nw 前缀后的 \n 分隔串）。
 * 返回 malloc 串（调用方 free）并置 *out_nr；无定义返回 NULL。 */
static char *load_def_reads(kvlangKv_t *kv, const char *key, int *out_nr) {
    *out_nr = 0;
    kvlangXvalue_t v; kvlangXvalueZero(&v);
    kvlangKvGetOne(kv, key, &v);
    if (kvlangXvalueNone(&v)) { kvlangXvalueFree(&v); return NULL; }
    kvspaceHead_t h; kvlangXvalueHead(&v, &h);
    int32_t bl; const uint8_t *b = kvlangXvalueBody(&v, &h, &bl);
    if (bl < 4) { kvlangXvalueFree(&v); return NULL; }
    *out_nr = b[0] | (b[1] << 8);
    size_t sl = (size_t)(bl - 4);
    char *sig = malloc(sl + 1);
    memcpy(sig, b + 4, sl); sig[sl] = 0;
    kvlangXvalueFree(&v);
    return sig;
}

static char *frame_slot_key(const char *frame_root, const char *slot) {
    if (!slot || !slot[0]) return NULL;
    if (slot[0] == '/') return strdup(slot);
    if (strncmp(slot, MEMBER_SEP, MEMBER_SEP_LEN) == 0) return NULL;
    kvlangStrbuf_t b; kvlangStrbufInit(&b);
    char *stk = kvlangKeytreeStack(frame_root);
    kvlangStrbufPuts(&b, stk); free(stk);
    kvlangStrbufPuts(&b, slot);
    return kvlangStrbufDetach(&b);
}

/* 实参名 → 其存储键（写入被调帧 [0,-k]/[0,k]）。字面量返回 NULL。
 * 命名参数经 Ptr 指到本帧 [0,±k]，槽内是上层 handle_call 已 resolve 好的最终键路径；
 * 之后只追显式 Ptr（ref==1）链，勿把 char 值当路径再追——否则字符串实参的内容会被
 * 误当键（穿两层调用即变 None）。写侧 kvlangBuiltinResolveWriteSlot 同此纪律（#124）。 */
static char *resolve_read_path(kvlangKv_t *kv, const char *frame_root, const char *name) {
    if (is_literal(name)) return NULL;
    char *stk = kvlangKeytreeStack(frame_root);
    kvlangXvalue_t v; kvlangXvalueZero(&v);
    kvlangKvGetMember(kv, stk, name, &v);
    char *result = NULL;
    if (kvlangXvalueIsPtr(&v)) {
        char *target = kvlangXvaluePtrTarget(&v);
        kvlangXvalue_t nv; kvlangXvalueZero(&nv);
        kvlangKvGetMember(kv, stk, target, &nv);
        if (kvlangXvalueNone(&nv) || !kvlangXvalueIsCharKind(kvlangXvalueKind(&nv))) {
            result = frame_slot_key(frame_root, target);
        } else {
            char *path = kvlangXvalueValueString(&nv);
            for (;;) {
                kvlangXvalue_t hop; kvlangXvalueZero(&hop);
                kvlangKvGetOne(kv, path, &hop);
                if (!kvlangXvalueIsPtr(&hop)) { kvlangXvalueFree(&hop); result = path; break; }
                char *p2 = kvlangXvaluePtrTarget(&hop);
                kvlangXvalueFree(&hop);
                free(path);
                path = p2;
            }
        }
        kvlangXvalueFree(&nv);
        free(target);
    } else {
        result = frame_slot_key(frame_root, name);
    }
    kvlangXvalueFree(&v); free(stk);
    return result;
}

/* return：弹出当前帧 [d]。d==1 → 顶层结束（*out_next=NULL）；否则 *out_next=‥returnpc。
 * 返回链断裂（帧无 ‥returnpc）→ RuntimeError（#109），保留该帧供排查，返回 -1。 */
static int handle_return(kvlangKv_t *kv, const char *vtid, const char *pc, char **out_next) {
    *out_next = NULL;
    char *fr = kvlangKeytreeFrameRoot(pc);
    int d = kvlangKeytreeFrameNum(fr);
    char *next = NULL;
    if (d > 1) {
        kvlangStrbuf_t rk; kvlangStrbufInit(&rk);
        kvlangKeytreeFrameReturnpc(fr, &rk);
        kvlangXvalue_t v; kvlangXvalueZero(&v);
        kvlangKvGetOne(kv, rk.p, &v);
        if (!kvlangXvalueNone(&v)) next = kvlangXvalueValueString(&v);
        kvlangXvalueFree(&v);
        kvlangStrbufFree(&rk);
        if (!next || !next[0]) {
            char msg[512];
            snprintf(msg, sizeof msg, "RuntimeError: broken return chain: frame %s has no returnpc (pc=%s)", fr, pc);
            kvlangVthreadSetError(kv, vtid, pc, msg);
            free(next); free(fr);
            return -1;
        }
    }
    char *stk = kvlangKeytreeStack(fr);
    char err[256];
    kvlangKvDelExtIndex(kv, stk, err, sizeof err);
    kvlangKvDelTree(kv, fr, err, sizeof err);
    free(stk); free(fr);
    *out_next = next;
    return 0;
}

/* HandleCall：创建子帧。返回 EntryPC(frameRoot)，失败 NULL */
static char *handle_call(kvlangKv_t *kv, const char *pc, kvlangRwirInst_t *inst) {
    kvlangStrbuf_t vtid_b; kvlangStrbufInit(&vtid_b);
    const char *vtid = kvlangKeytreeVtidFromPc(pc, &vtid_b);
    const char *fn = inst->reads[0].name;
    char *pkg = strdup("");
    char *name = strdup(fn);
    const char *lp = "/lib/";
    if (strncmp(fn, lp, 5) == 0) {
        const char *rest = fn + 5;
        const char *dot = rfind_sep(rest);
        if (dot) { free(pkg); pkg = strndup(rest, (size_t)(dot - rest)); free(name); name = strdup(dot + MEMBER_SEP_LEN); }
        else { free(name); name = strdup(rest); }
    } else {
        const char *dot = rfind_sep(fn);
        if (dot) { free(pkg); pkg = strndup(fn, (size_t)(dot - fn)); free(name); name = strdup(dot + MEMBER_SEP_LEN); }
        else {
            /* 裸名调用（无 /lib/ 无 ·）：同 pkg 优先——当前函数所在 lib 下有同名 rwfunc 就用之
             * （lib aaa/bbb/math 内 sum(A,A) → /lib/aaa/bbb/math·sum），否则退回根 /lib/<fn>。 */
            char *ff = kvlangKeytreeFrameRoot(pc);
            if (ff) {
                /* 函数目录在帧的 ‥lib 槽（/lib/aaa/bbb/math·double/）：由此取调用者 pkg。 */
                kvlangStrbuf_t lk; kvlangStrbufInit(&lk);
                char *stk = kvlangKeytreeStack(ff);
                kvlangStrbufPuts(&lk, stk); free(stk);
                kvlangStrbufPuts(&lk, SEG_LIB);
                kvlangXvalue_t lv; kvlangXvalueZero(&lv);
                kvlangKvGetOne(kv, lk.p, &lv);
                kvlangStrbufFree(&lk);
                char *funcdir = kvlangXvalueNone(&lv) ? NULL : kvlangXvalueValueString(&lv);
                kvlangXvalueFree(&lv);
                if (funcdir) {
                    char *rel = funcdir + 5; // 剥 /lib/
                    size_t rl = strlen(rel);
                    if (rl > 0 && rel[rl - 1] == '/') rel[rl - 1] = '\0'; // 剥尾 /
                    const char *sep = rfind_sep(rel);
                    if (sep) {
                        char *cand_pkg = strndup(rel, (size_t)(sep - rel));
                        char *cand = kvlangKeytreeLibFunc(cand_pkg, fn);
                        kvlangStrbuf_t sk; kvlangStrbufInit(&sk);
                        kvlangStrbufPrintf(&sk, "%s/[0,0]", cand);
                        kvlangXvalue_t sv; kvlangXvalueZero(&sv);
                        kvlangKvGetOne(kv, sk.p, &sv);
                        bool ok = !kvlangXvalueNone(&sv) && kvlangXvalueKindIs(&sv, KVSPACE_KIND_DEF_RWFUNC);
                        kvlangXvalueFree(&sv); kvlangStrbufFree(&sk);
                        if (ok) { free(pkg); pkg = cand_pkg; }
                        else free(cand_pkg);
                        free(cand);
                    }
                    free(funcdir);
                }
                free(ff);
            }
        }
    }
    char *func_key = kvlangKeytreeLibFunc(pkg, name);
    kvlangStrbuf_t func_dir; kvlangStrbufInit(&func_dir);
    kvlangStrbufPuts(&func_dir, func_key); kvlangStrbufPutc(&func_dir, '/');

    kvlangStrbuf_t sig_key; kvlangStrbufInit(&sig_key);
    kvlangStrbufPrintf(&sig_key, "%s[0,0]", func_dir.p);
    kvlangXvalue_t sig; kvlangXvalueZero(&sig);
    kvlangKvGetOne(kv, sig_key.p, &sig);
    if (kvlangXvalueNone(&sig) || !kvlangXvalueKindIs(&sig, KVSPACE_KIND_DEF_RWFUNC)) {
        /* 按 xvalue 的 kind 精确区分缺 rwir 还是缺 rwfunc：
         * 到这里说明 opcode 已被 isothersrwir 判否（/lib/<op> 非 rwir）。 */
        char *rk = kvlangKeytreeRwir(fn);
        kvlangXvalue_t rv; kvlangXvalueZero(&rv);
        kvlangKvGetOne(kv, rk, &rv);
        char msg[256];
        if (!kvlangXvalueNone(&rv) && kvlangXvalueKindIs(&rv, KVSPACE_KIND_DEF_RWIR))
            snprintf(msg, sizeof msg, "NameError: rwir 未注册/签名不匹配: %s", fn);
        else if (!kvlangXvalueNone(&sig))
            snprintf(msg, sizeof msg, "NameError: %s 不是 rwfunc (kind=%s)", fn, kvlangXvalueKind(&sig));
        else
            snprintf(msg, sizeof msg, "NameError: rwfunc not found: %s", fn);
        kvlangXvalueFree(&rv); free(rk);
        kvlangVthreadSetError(kv, vtid, pc, msg);
        goto fail;
    }
    kvspaceHead_t h; kvlangXvalueHead(&sig, &h);
    const uint8_t *sbody = sig.data + h.body_offset;
    int nr = sbody[0] | (sbody[1] << 8);
    int nw = sbody[2] | (sbody[3] << 8);

    {   /* 读参类型校验：reads[0]=函数名，实参从 reads[1] 起 */
        size_t sl = h.body_len >= 4 ? (size_t)(h.body_len - 4) : 0;
        char *ds = malloc(sl + 1);
        memcpy(ds, sbody + 4, sl); ds[sl] = 0;
        int crc = check_read_types(kv, vtid, pc, fn, ds, nr, inst->reads + 1, inst->nr - 1);
        free(ds);
        if (crc != 0) goto fail;
    }

    char *caller_fr = kvlangKeytreeFrameRoot(pc);
    int d = kvlangKeytreeFrameNum(pc);
    char *frame_root = kvlangKeytreeFrameAt(vtid, d + 1);
    char err[256];
    kvlangKvDelTree(kv, frame_root, err, sizeof err);
    char *stack_fr = kvlangKeytreeStack(frame_root);
    kvlangKvMkindex(kv, stack_fr, err, sizeof err);
    kvlangKvExtIndex(kv, stack_fr, func_dir.p, err, sizeof err);

    /* 系统变量 */
    kvlangStrbuf_t npc; kvlangStrbufInit(&npc); kvlangRwirNextPc(pc, &npc);
    kvlangStrbuf_t retpc; kvlangStrbufInit(&retpc); kvlangKeytreeFrameReturnpc(frame_root, &retpc);
    kvlangStrbuf_t callpc; kvlangStrbufInit(&callpc); kvlangKeytreeFrameCallpc(frame_root, &callpc);
    char *ep = kvlangKeytreeEntryPc(frame_root);
    kvlangStrbuf_t seglib; kvlangStrbufInit(&seglib); kvlangStrbufPuts(&seglib, stack_fr); kvlangStrbufPuts(&seglib, SEG_LIB);
    kvlangXvalue_t v_npc, v_ep, v_fn; kvlangXvalueZero(&v_npc); kvlangXvalueZero(&v_ep); kvlangXvalueZero(&v_fn);
    kvlangXvalueNewCharUtf8(&v_npc, npc.p);
    kvlangXvalueNewCharUtf8(&v_ep, ep);
    kvlangXvalueNewCharUtf8(&v_fn, func_key);
    kvlangKvPair_t sys[3] = { { retpc.p, v_npc }, { callpc.p, v_ep }, { seglib.p, v_fn } };
    kvlangKvSet(kv, sys, 3, err, sizeof err);
    kvlangXvalueFree(&v_npc); kvlangXvalueFree(&v_ep); kvlangXvalueFree(&v_fn);

    /* 读参 + 写参 */
    kvlangKvPair_t pairs[512]; int np = 0;
    int lit_seq = 0;
    for (int i = 0; i < nr; i++) {
        kvlangStrbuf_t slot; kvlangStrbufInit(&slot);
        kvlangStrbufPrintf(&slot, "%s/[0,-%d]", frame_root, i + 1);
        if (i + 1 < inst->nr) {
            kvlangParam_t *arg = &inst->reads[i + 1];
            char *rk = resolve_read_path(kv, caller_fr, arg->name);
            bool concrete = !kvlangXvalueNone(&arg->val) && !kvlangXvalueKindIs(&arg->val, KVSPACE_KIND_RWIR) && !kvlangXvalueKindIs(&arg->val, KVSPACE_KIND_RWFUNC);
            if (concrete) {
                /* 字面量无变量槽，一律写 ._litN；勿沿用 resolve_read_path 的返回值——
                 * 否则字面量内容（如 "https://x" 里的 //）会被当路径段，二次读回即丢。 */
                if (rk) free(rk);
                kvlangStrbuf_t lk; kvlangStrbufInit(&lk);
                kvlangStrbufPrintf(&lk, "%s/._lit%d", caller_fr, lit_seq++);
                rk = kvlangStrbufDetach(&lk);
                /* 写字面量到 rk（拷贝，避免 double-free） */
                kvspaceHead_t ah;
                kvlangXvalueHead(&arg->val, &ah);
                int32_t abl; const uint8_t *ab = kvlangXvalueBody(&arg->val, &ah, &abl);
                kvlang_kindexpr_t akx; kvlang_kindexpr_parse(ah.kindexpr, &akx);
                pairs[np].key = strdup(rk);
                kvlangXvalueZero(&pairs[np].val);
                kvlangXvalueEncodeBox(kvlangXvalueKind(&arg->val), ab, (uint32_t)abl, akx.dims, akx.ndim,
                                      &pairs[np].val.data, &pairs[np].val.len);
                np++;
            }
            if (rk) {
                kvlangXvalue_t rv; kvlangXvalueNewCharUtf8(&rv, rk);
                pairs[np].key = kvlangStrbufDetach(&slot);
                pairs[np].val = rv;
                np++;
                free(rk);
            }
        }
        kvlangStrbufFree(&slot);
    }
    for (int i = 0; i < nw; i++) {
        kvlangStrbuf_t slot; kvlangStrbufInit(&slot);
        kvlangStrbufPrintf(&slot, "%s/[0,%d]", frame_root, i + 1);
        if (i < inst->nw) {
            char *wk = resolve_read_path(kv, caller_fr, inst->writes[i].name);
            if (wk) {
                kvlangXvalue_t wv; kvlangXvalueNewCharUtf8(&wv, wk);
                pairs[np].key = kvlangStrbufDetach(&slot);
                pairs[np].val = wv;
                np++;
                free(wk);
            }
        }
        kvlangStrbufFree(&slot);
    }
    if (np > 0) kvlangKvSet(kv, pairs, np, err, sizeof err);
    for (int i = 0; i < np; i++) { free(pairs[i].key); kvlangXvalueFree(&pairs[i].val); }

    free(caller_fr); free(stack_fr);
    kvlangStrbufFree(&npc); kvlangStrbufFree(&retpc); kvlangStrbufFree(&callpc); kvlangStrbufFree(&seglib);
    kvlangStrbufFree(&func_dir); kvlangStrbufFree(&sig_key); kvlangStrbufFree(&vtid_b);
    kvlangXvalueFree(&sig); free(func_key); free(pkg); free(name);
    free(frame_root);
    return ep;

fail:
    kvlangStrbufFree(&func_dir); kvlangStrbufFree(&sig_key); kvlangStrbufFree(&vtid_b);
    kvlangXvalueFree(&sig); free(func_key); free(pkg); free(name);
    return NULL;
}

static int handle_control(kvlangKv_t *kv, const char *vtid, const char *pc, kvlangRwirInst_t *inst) {
    int id = inst->op_id;
    if (id == OPID_NONE && inst->opcode)
        id = kvlangOpcodeIntern(NULL, inst->opcode);
    if (id == OPID_CALL || (id == OPID_NONE && inst->opcode && strcmp(inst->opcode, OP_CALL) == 0)) {
        char *sub = handle_call(kv, pc, inst);
        if (!sub) return -1;
        kvlangVthreadSet(kv, vtid, sub, "running");
        free(sub);
        return 0;
    }
    if (id == OPID_RETURN || (id == OPID_NONE && inst->opcode && strcmp(inst->opcode, OP_RETURN) == 0)) {
        char *parent = NULL;
        if (handle_return(kv, vtid, pc, &parent) != 0) return -1;
        if (!parent) { kvlangVthreadSetDone(kv, vtid, "ok"); return 2; }
        kvlangVthreadSet(kv, vtid, parent, "running");
        free(parent);
        return 0;
    }
    if (id == OPID_GOTO || (id == OPID_NONE && inst->opcode && strcmp(inst->opcode, OP_GOTO) == 0)) {
        if (inst->nr != 1) {
            char msg[128]; snprintf(msg, sizeof msg, "RuntimeError: goto expects 1 irseq, got %d", inst->nr);
            kvlangVthreadSetError(kv, vtid, pc, msg);
            return -1;
        }
        return jump_to(kv, vtid, pc, &inst->reads[0], OP_GOTO);
    }
    if (id == OPID_BR || (id == OPID_NONE && inst->opcode && strcmp(inst->opcode, OP_BR) == 0)) {
        if (inst->nr != 3) {
            char msg[128]; snprintf(msg, sizeof msg, "RuntimeError: br expects cond trueIrseq falseIrseq, got %d", inst->nr);
            kvlangVthreadSetError(kv, vtid, pc, msg);
            return -1;
        }
        char *fr = kvlangKeytreeFrameRoot(pc);
        kvlangXvalue_t cond; kvlangXvalueZero(&cond);
        kvlangBuiltinResolveReadValue(kv, fr, inst->reads[0].name, &inst->reads[0].val, &cond);
        free(fr);
        if (kvlangXvalueNone(&cond)) {
            kvlangVthreadSetError(kv, vtid, pc, "TypeError: None in branch condition");
            kvlangXvalueFree(&cond);
            return -1;
        }
        bool taken = kvlangXvalueAsBool(&cond);
        kvlangXvalueFree(&cond);
        return jump_to(kv, vtid, pc, &inst->reads[taken ? 1 : 2], OP_BR);
    }
    return -1;
}

/* 动态调用：以运行时得到的 funckey 在当前 vthread 造一次 OP_CALL（不新开 vid），
 * pc 落在被调入口，帧结束回到本指令 NextPc。供 native vthread·call 用。 */
int kvlangKvcpuDynCall(kvlangKv_t *kv, const char *vtid, const char *pc, const char *funckey) {
    kvlangRwirInst_t ci;
    ci.opcode = strdup(OP_CALL);
    ci.op_id = OPID_CALL;
    ci.reads = malloc(sizeof(kvlangParam_t));
    ci.reads[0].name = strdup(funckey);
    kvlangXvalueZero(&ci.reads[0].val);
    ci.nr = 1;
    ci.writes = NULL;
    ci.nw = 0;
    int rc = handle_control(kv, vtid, pc, &ci);
    free(ci.opcode); free(ci.reads[0].name); free(ci.reads);
    return rc;
}

static bool is_copy_op(const char *opcode) {
    return strcmp(opcode, "=") == 0;
}

int handoff_external_rwir(kvlangKv_t *kv, const char *vtid, const char *pc, kvlangRwirInst_t *inst) {
    /* handoff：把 pc 挂到共享队列 /lib/<opcode>/vids/<vtid>（各 rwir 的 vids 已 Ptr 统一到
     * 第一个 rwir 的 vids 下，Set 经路径穿透落到同一 strkeymap）。外部执行器认领并驱动该 vthread，
     * 完成后删除该条目。本端 watch 同一 key 直至变 None（== 认领方已完成），单键交接、无 id。 */
    char *base = kvlangKeytreeRwir(inst->opcode);
    kvlangStrbuf_t vids; kvlangStrbufInit(&vids);
    kvlangStrbufPrintf(&vids, "%s/vids/%s", base, vtid);
    kvlangXvalue_t pv; kvlangXvalueNewCharUtf8(&pv, pc);
    kvlangKvPair_t p = { vids.p, pv };
    char err[256];
    kvlangKvSet(kv, &p, 1, err, sizeof err);
    kvlangXvalueFree(&pv);

    kvlangXvalue_t none; kvlangXvalueZero(&none);   /* 目标 None：等条目被删除 */
    kvlangXvalue_t got; kvlangXvalueZero(&got);
    int rc = kvlangKvWatch(kv, vids.p, &none, 30000000000ULL, &got);
    kvlangXvalueFree(&got);
    kvlangStrbufFree(&vids); free(base);
    if (rc != 0) {
        char msg[256]; snprintf(msg, sizeof msg, "RuntimeError: external rwir %s handoff failed", inst->opcode);
        kvlangVthreadSetError(kv, vtid, pc, msg);
        return -1;
    }
    return 0;
}

char *kvlangKvcpuBootstrap(kvlangKv_t *kv, const char *vtid, const char *funcname,
                      const char *const *args, int nargs) {
    char *pkg = strdup("");
    char *name = strdup(funcname);
    const char *dot = rfind_sep(funcname);
    if (dot) { free(pkg); pkg = strndup(funcname, (size_t)(dot - funcname)); free(name); name = strdup(dot + MEMBER_SEP_LEN); }
    char *func_key = kvlangKeytreeLibFunc(pkg, name);
    kvlangStrbuf_t func_dir; kvlangStrbufInit(&func_dir);
    kvlangStrbufPuts(&func_dir, func_key); kvlangStrbufPutc(&func_dir, '/');

    kvlangStrbuf_t sig_key; kvlangStrbufInit(&sig_key);
    kvlangStrbufPrintf(&sig_key, "%s[0,0]", func_dir.p);
    kvlangXvalue_t sig; kvlangXvalueZero(&sig);
    kvlangKvGetOne(kv, sig_key.p, &sig);
    if (kvlangXvalueNone(&sig) || !kvlangXvalueKindIs(&sig, KVSPACE_KIND_DEF_RWFUNC)) {
        char msg[256]; snprintf(msg, sizeof msg, "Bootstrap: rwir/rwfunc not found: %s", funcname);
        kvlangVthreadSetError(kv, vtid, "", msg);
        kvlangXvalueFree(&sig); kvlangStrbufFree(&sig_key); kvlangStrbufFree(&func_dir);
        free(func_key); free(pkg); free(name);
        return NULL;
    }
    kvspaceHead_t h; kvlangXvalueHead(&sig, &h);
    const uint8_t *sbody = sig.data + h.body_offset;
    int nr = sbody[0] | (sbody[1] << 8);

    char *frame_root = kvlangKeytreeFrameAt(vtid, 1);
    char *stack_fr = kvlangKeytreeStack(frame_root);
    char err[256];
    kvlangKvMkindex(kv, stack_fr, err, sizeof err);
    kvlangKvExtIndex(kv, stack_fr, func_dir.p, err, sizeof err);

    char *ep = kvlangKeytreeEntryPc(frame_root);
    kvlangStrbuf_t callpc; kvlangStrbufInit(&callpc); kvlangKeytreeFrameCallpc(frame_root, &callpc);
    kvlangStrbuf_t seglib; kvlangStrbufInit(&seglib); kvlangStrbufPuts(&seglib, stack_fr); kvlangStrbufPuts(&seglib, SEG_LIB);
    kvlangXvalue_t v_ep, v_fn; kvlangXvalueZero(&v_ep); kvlangXvalueZero(&v_fn);
    kvlangXvalueNewCharUtf8(&v_ep, ep);
    kvlangXvalueNewCharUtf8(&v_fn, func_key);
    kvlangKvPair_t sys[2] = { { callpc.p, v_ep }, { seglib.p, v_fn } };
    kvlangKvSet(kv, sys, 2, err, sizeof err);
    kvlangXvalueFree(&v_ep); kvlangXvalueFree(&v_fn);

    if (nargs > 0) {
        kvlangKvPair_t pairs[128]; int np = 0;
        for (int i = 0; i < nr && i < nargs; i++) {
            kvlangStrbuf_t slot; kvlangStrbufInit(&slot);
            kvlangStrbufPrintf(&slot, "%s/[0,-%d]", frame_root, i + 1);
            kvlangXvalue_t av; kvlangXvalueZero(&av);
            kvlangBuiltinResolveReadValue(kv, "", args[i], NULL, &av);
            pairs[np].key = kvlangStrbufDetach(&slot);
            pairs[np].val = av;
            np++;
        }
        if (np > 0) kvlangKvSet(kv, pairs, np, err, sizeof err);
        for (int i = 0; i < np; i++) { free(pairs[i].key); kvlangXvalueFree(&pairs[i].val); }
    }

    kvlangXvalueFree(&sig); kvlangStrbufFree(&sig_key); kvlangStrbufFree(&func_dir);
    kvlangStrbufFree(&callpc); kvlangStrbufFree(&seglib);
    free(stack_fr); free(frame_root); free(func_key); free(pkg); free(name);
    return ep;
}

#define IC_CAP 256
typedef struct {
    char *base;
    int irseq;
    kvlangRwirInst_t inst;
    int live;
} ic_ent_t;
static ic_ent_t g_ic[IC_CAP];
static unsigned g_ic_n;
static void tight_cache_flush(void);

static void ic_flush(void) {
    for (unsigned i = 0; i < IC_CAP; i++) {
        if (g_ic[i].live) {
            kvlangRwirInstFree(&g_ic[i].inst);
            free(g_ic[i].base);
            g_ic[i].base = NULL;
            g_ic[i].live = 0;
        }
    }
    g_ic_n = 0;
    tight_cache_flush();
}

static kvlangRwirInst_t *ic_get(kvlangKv_t *kv, const char *base, int irseq, char *err, uint32_t err_cap) {
    for (unsigned i = 0; i < IC_CAP; i++) {
        if (g_ic[i].live && g_ic[i].irseq == irseq && g_ic[i].base &&
            strcmp(g_ic[i].base, base) == 0)
            return &g_ic[i].inst;
    }
    unsigned slot = g_ic_n % IC_CAP;
    g_ic_n++;
    if (g_ic[slot].live) {
        kvlangRwirInstFree(&g_ic[slot].inst);
        free(g_ic[slot].base);
        g_ic[slot].live = 0;
    }
    if (kvlangRwirDecodeAt(kv, base, irseq, &g_ic[slot].inst, err, err_cap) != 0)
        return NULL;
    g_ic[slot].base = strdup(base);
    g_ic[slot].irseq = irseq;
    g_ic[slot].inst.op_id = kvlangOpcodeIntern(kv, g_ic[slot].inst.opcode);
    g_ic[slot].live = 1;
    return &g_ic[slot].inst;
}

static const char *bare_op(const char *op) {
    if (!op || !op[0]) return "";
    const char *d = strstr(op, MEMBER_SEP);
    return d ? d + MEMBER_SEP_LEN : op;
}

static int cmp_kind_of(const char *op) {
    const char *b = bare_op(op);
    if (strcmp(b, "lt") == 0 || strcmp(b, "<") == 0) return 1;
    if (strcmp(b, "le") == 0 || strcmp(b, "<=") == 0 || strcmp(b, "≤") == 0) return 2;
    if (strcmp(b, "gt") == 0 || strcmp(b, ">") == 0) return 3;
    if (strcmp(b, "ge") == 0 || strcmp(b, ">=") == 0 || strcmp(b, "≥") == 0) return 4;
    return 0;
}

static int arith_kind_of(const char *op) {
    const char *b = bare_op(op);
    if (strcmp(b, "add") == 0 || strcmp(b, "+") == 0) return 1;
    if (strcmp(b, "sub") == 0 || strcmp(b, "-") == 0) return 2;
    if (strcmp(b, "mul") == 0 || strcmp(b, "×") == 0 || strcmp(b, "*") == 0) return 3;
    return 0;
}

static int64_t rd_i64_le(const uint8_t *p) {
    uint64_t u = (uint64_t)p[0] | ((uint64_t)p[1] << 8) | ((uint64_t)p[2] << 16) |
                 ((uint64_t)p[3] << 24) | ((uint64_t)p[4] << 32) | ((uint64_t)p[5] << 40) |
                 ((uint64_t)p[6] << 48) | ((uint64_t)p[7] << 56);
    return (int64_t)u;
}

static void wr_i64_le(uint8_t *p, int64_t n) {
    uint64_t u = (uint64_t)n;
    for (int i = 0; i < 8; i++)
        p[i] = (uint8_t)(u >> (i * 8));
}

static int64_t rd_elem(const uint8_t *p, int esz) {
    switch (esz) {
    case 1: return (int8_t)p[0];
    case 2: return (int16_t)(p[0] | ((uint16_t)p[1] << 8));
    case 4: {
        uint32_t u = (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) |
                     ((uint32_t)p[3] << 24);
        return (int32_t)u;
    }
    case 8: return rd_i64_le(p);
    default: return 0;
    }
}

static void wr_elem(uint8_t *p, int esz, int64_t n) {
    uint64_t u = (uint64_t)n;
    for (int i = 0; i < esz; i++)
        p[i] = (uint8_t)(u >> (i * 8));
}

static int xv_at_set(const char *op) {
    if (!op)
        return 0;
    if (strcmp(op, "xv" MEMBER_SEP "at") == 0)
        return 1;
    if (strcmp(op, "xv" MEMBER_SEP "set") == 0)
        return 2;
    return 0;
}

#ifndef TK_I64
#define TK_I64 0
#define TK_BOOL 1
#define TK_ARR 2
#define TK_F64 3
#define TK_MAP 4
#endif

static int param_is_const(const kvlangParam_t *p, int64_t *out, int *kind) {
    if (p->val.data && !kvlangXvalueNone(&p->val)) {
        if (kvlangXvalueKindIs(&p->val, KVSPACE_KIND_INT64)) {
            *out = kvlangXvalueAsInt64(&p->val);
            *kind = TK_I64;
            return 1;
        }
        if (kvlangXvalueKindIs(&p->val, KVSPACE_KIND_BOOL)) {
            *out = kvlangXvalueAsBool(&p->val);
            *kind = TK_BOOL;
            return 1;
        }
        if (kvlangXvalueKindIs(&p->val, KVSPACE_KIND_FLOAT64)) {
            double d = kvlangXvalueAsFloat64(&p->val);
            memcpy(out, &d, sizeof d);
            *kind = TK_F64;
            return 1;
        }
    }
    if (p->name && p->name[0]) {
        if (strcmp(p->name, "true") == 0) {
            *out = 1;
            *kind = TK_BOOL;
            return 1;
        }
        if (strcmp(p->name, "false") == 0) {
            *out = 0;
            *kind = TK_BOOL;
            return 1;
        }
        if ((p->name[0] >= '0' && p->name[0] <= '9') ||
            (p->name[0] == '-' && p->name[1] >= '0' && p->name[1] <= '9')) {
            if (strchr(p->name, '.') || strchr(p->name, 'e') || strchr(p->name, 'E')) {
                char *end = NULL;
                double d = strtod(p->name, &end);
                if (end && end != p->name && *end == 0) {
                    memcpy(out, &d, sizeof d);
                    *kind = TK_F64;
                    return 1;
                }
            }
            *out = strtoll(p->name, NULL, 10);
            *kind = TK_I64;
            return 1;
        }
    }
    return 0;
}

static int param_is_const_i64(const kvlangParam_t *p, int64_t *out) {
    int k = -1;
    return param_is_const(p, out, &k) && k == TK_I64;
}

#define TIGHT_VARS 16
#define TIGHT_OPS 48
#define TIGHT_MAP_ELEMS 64
#define TOP_DEAD 0
#define TOP_ADD 1
#define TOP_SUB 2
#define TOP_MUL 3
#define TOP_REM 4
#define TOP_EQ 5
#define TOP_NE 6
#define TOP_COPY 7
#define TOP_BR 8
#define TOP_GOTO 9
#define TOP_LT 10
#define TOP_LE 11
#define TOP_GT 12
#define TOP_GE 13
#define TOP_BR_REM_EQ 14
#define TOP_AT 15
#define TOP_SET 16
#define TOP_MAP_AT 17
#define TOP_PRINT 18
#define TIP_LATCH -1
#define TIP_EXIT -2
#define TIP_NONE -3
typedef struct {
    int op, irseq;
    int dst, s0, s1, s2, s3, nidx;
    int64_t c0, c1, c2, c3, cc;
    int tseq, fseq;
    char lit0[80];
} tight_op_t;

typedef struct {
    int live, br_irseq, ck, cond_i, exit_i, nops, nvars, cl, cr, start_ip;
    int64_t clc, crc;
    char base[256];
    char nam[TIGHT_VARS][80];
    int kinds[TIGHT_VARS];
    tight_op_t ops[TIGHT_OPS];
} tight_prog_t;
static tight_prog_t g_tp;
static tight_prog_t g_forin;
static int g_forin_k, g_forin_n, g_forin_oexit, g_forin_sw;
static int64_t g_forin_ncst;
static tight_prog_t g_fmap;
static int g_fmap_k, g_fmap_n, g_fmap_oexit, g_fmap_sw;
static int64_t g_fmap_ncst;

static void tight_cache_flush(void) {
    g_tp.live = 0;
    g_forin.live = 0;
    g_forin_sw = 0;
    g_fmap.live = 0;
    g_fmap_sw = 0;
}

static int tight_intern(const char **names, int *kinds, int *n, const char *name, int kind) {
    if (!name || !name[0] || name[0] == '/')
        return -1;
    if ((name[0] >= '0' && name[0] <= '9') ||
        (name[0] == '-' && name[1] >= '0' && name[1] <= '9'))
        return -1;
    for (int i = 0; i < *n; i++)
        if (strcmp(names[i], name) == 0) {
            if (kind >= 0) {
                if (kinds[i] < 0)
                    kinds[i] = kind;
                else if (kinds[i] != kind)
                    return -1;
            }
            return i;
        }
    if (*n >= TIGHT_VARS)
        return -1;
    names[*n] = name;
    kinds[*n] = kind;
    return (*n)++;
}

static int tight_src(const kvlangParam_t *p, const char **names, int *kinds, int *nvars,
                     int *slot, int64_t *cst, int kind) {
    int ck = -1;
    if (param_is_const(p, cst, &ck)) {
        *slot = -1;
        if (kind >= 0 && ck >= 0 && kind != ck)
            return 0;
        return 1;
    }
    *slot = tight_intern(names, kinds, nvars, p->name, kind);
    return *slot >= 0;
}

static int arith_or_rel(const char *op) {
    int a = arith_kind_of(op);
    if (a)
        return a;
    int c = cmp_kind_of(op);
    if (c)
        return TOP_LT + c - 1;
    const char *b = bare_op(op);
    if (strcmp(b, "mod") == 0 || strcmp(b, "%") == 0) return TOP_REM;
    if (strcmp(b, "eq") == 0 || strcmp(b, "==") == 0) return TOP_EQ;
    if (strcmp(b, "neq") == 0 || strcmp(b, "!=") == 0) return TOP_NE;
    return 0;
}

static int tight_find_ip(const tight_op_t *ops, int nops, int irseq) {
    for (int i = 0; i < nops; i++)
        if (ops[i].irseq == irseq)
            return i;
    return -1;
}

static int64_t tight_load(uint8_t **body, const int *kinds, int slot) {
    if (kinds[slot] == TK_BOOL)
        return body[slot][0] != 0;
    return rd_i64_le(body[slot]);
}

static void tight_store(uint8_t **body, const int *kinds, int slot, int64_t r) {
    if (kinds[slot] == TK_BOOL)
        body[slot][0] = r != 0;
    else
        wr_i64_le(body[slot], r);
}

static double f64_from_bits(int64_t c) {
    double d;
    memcpy(&d, &c, sizeof d);
    return d;
}

static int64_t f64_to_bits(double d) {
    int64_t c;
    memcpy(&c, &d, sizeof c);
    return c;
}

static double f64_load(const uint8_t *p) {
    return f64_from_bits(rd_i64_le(p));
}

static void f64_store(uint8_t *p, double d) {
    wr_i64_le(p, f64_to_bits(d));
}

static double f64_opnd(uint8_t **body, int slot, int64_t c) {
    return slot >= 0 ? f64_load(body[slot]) : f64_from_bits(c);
}

static double f64_eval(int op, double a, double b) {
    switch (op) {
    case TOP_ADD: return a + b;
    case TOP_SUB: return a - b;
    case TOP_MUL: return a * b;
    case TOP_EQ: return a == b;
    case TOP_NE: return a != b;
    case TOP_LT: return a < b;
    case TOP_LE: return a <= b;
    case TOP_GT: return a > b;
    case TOP_GE: return a >= b;
    default: return a;
    }
}

static int64_t tight_eval(int op, int64_t a, int64_t b) {
    switch (op) {
    case TOP_ADD: return a + b;
    case TOP_SUB: return a - b;
    case TOP_MUL: return a * b;
    case TOP_REM: return b == 0 ? 0 : a % b;
    case TOP_EQ: return a == b;
    case TOP_NE: return a != b;
    case TOP_LT: return a < b;
    case TOP_LE: return a <= b;
    case TOP_GT: return a > b;
    case TOP_GE: return a >= b;
    default: return a;
    }
}

static int tight_is_cmp(int op) {
    return op == TOP_EQ || op == TOP_NE || (op >= TOP_LT && op <= TOP_GE);
}

static int tight_is_brlike(const tight_op_t *o) {
    return o->op == TOP_BR || o->op == TOP_BR_REM_EQ || (tight_is_cmp(o->op) && o->dst < 0);
}

/* Follow gotos. Incomplete/cycle → TIP_NONE. Does not skip fused-away ops. */
static int tight_follow_ir(const tight_op_t *ops, int nops, int ir, int cond_i, int exit_i) {
    int guard = 0;
    while (guard++ <= nops + 2) {
        if (ir == cond_i)
            return TIP_LATCH;
        if (ir == exit_i)
            return TIP_EXIT;
        int i = tight_find_ip(ops, nops, ir);
        if (i < 0)
            return TIP_NONE;
        if (ops[i].op == TOP_DEAD) {
            ir = ops[i].irseq + 1;
            continue;
        }
        if (ops[i].op == TOP_GOTO) {
            ir = ops[i].tseq;
            continue;
        }
        return ir;
    }
    return TIP_NONE;
}

static int tight_slot_used(const tight_op_t *ops, int nops, int slot, int skip) {
    int u = 0;
    for (int i = 0; i < nops; i++) {
        if (i == skip || ops[i].op == TOP_DEAD || ops[i].op == TOP_GOTO)
            continue;
        if (ops[i].s0 == slot || ops[i].s1 == slot || ops[i].s2 == slot || ops[i].s3 == slot)
            u++;
    }
    return u;
}

/* Fuse cmp+br and rem+(eq+br); drop gotos; thread tseq/fseq as ips.
 * Incomplete edges return -1. start_ip on success. */
static int tight_compile(tight_op_t *ops, int *pnops, int cond_i, int exit_i, int body_i) {
    int nops = *pnops;
    for (int i = 0; i < nops; i++) {
        if (!tight_is_cmp(ops[i].op) || ops[i].dst < 0)
            continue;
        int nir = tight_follow_ir(ops, nops, ops[i].irseq + 1, cond_i, exit_i);
        if (nir < 1)
            continue;
        int j = tight_find_ip(ops, nops, nir);
        if (j < 0 || ops[j].op != TOP_BR || ops[j].s0 < 0 || ops[j].s0 != ops[i].dst)
            continue;
        ops[i].tseq = ops[j].tseq;
        ops[i].fseq = ops[j].fseq;
        ops[i].dst = -1;
        ops[j].op = TOP_DEAD;
    }
    for (int i = 0; i < nops; i++) {
        if (ops[i].op != TOP_REM || ops[i].dst < 0)
            continue;
        int nir = tight_follow_ir(ops, nops, ops[i].irseq + 1, cond_i, exit_i);
        if (nir < 1)
            continue;
        int j = tight_find_ip(ops, nops, nir);
        if (j < 0 || ops[j].op != TOP_EQ || ops[j].dst >= 0 || ops[j].s0 != ops[i].dst ||
            ops[j].s1 >= 0)
            continue;
        if (tight_slot_used(ops, nops, ops[i].dst, j) != 0)
            continue;
        ops[i].op = TOP_BR_REM_EQ;
        ops[i].cc = ops[j].c1;
        ops[i].tseq = ops[j].tseq;
        ops[i].fseq = ops[j].fseq;
        ops[i].dst = -1;
        ops[j].op = TOP_DEAD;
    }
    for (int i = 0; i < nops; i++) {
        if (ops[i].op == TOP_DEAD || ops[i].op == TOP_GOTO)
            continue;
        if (tight_is_brlike(&ops[i]))
            continue;
        ops[i].tseq = ops[i].irseq + 1;
        ops[i].fseq = 0;
    }
    for (int i = 0; i < nops; i++) {
        if (ops[i].op == TOP_DEAD || ops[i].op == TOP_GOTO)
            continue;
        int t = tight_follow_ir(ops, nops, ops[i].tseq, cond_i, exit_i);
        if (t == TIP_NONE)
            return -1;
        ops[i].tseq = t;
        if (tight_is_brlike(&ops[i])) {
            int f = tight_follow_ir(ops, nops, ops[i].fseq, cond_i, exit_i);
            if (f == TIP_NONE)
                return -1;
            ops[i].fseq = f;
        }
    }
    int start_ir = tight_follow_ir(ops, nops, body_i, cond_i, exit_i);
    if (start_ir < 1)
        return -1;
    tight_op_t out[TIGHT_OPS];
    int m = 0;
    for (int i = 0; i < nops; i++) {
        if (ops[i].op == TOP_DEAD || ops[i].op == TOP_GOTO)
            continue;
        out[m++] = ops[i];
    }
    if (m < 1)
        return -1;
    memcpy(ops, out, sizeof(tight_op_t) * (size_t)m);
    nops = m;
    *pnops = nops;
    for (int i = 0; i < nops; i++) {
        if (ops[i].tseq > 0) {
            int ip = tight_find_ip(ops, nops, ops[i].tseq);
            if (ip < 0)
                return -1;
            ops[i].tseq = ip;
        }
        if (tight_is_brlike(&ops[i]) && ops[i].fseq > 0) {
            int ip = tight_find_ip(ops, nops, ops[i].fseq);
            if (ip < 0)
                return -1;
            ops[i].fseq = ip;
        }
    }
    int start_ip = tight_find_ip(ops, nops, start_ir);
    return start_ip;
}

/* Inner while: fused cmp header, rem==k ? copy-const; break : inc; latch. */
static int tight_tdiv_hdr(const tight_op_t *ops, int nops, int h) {
    if (h < 0 || h >= nops)
        return 0;
    const tight_op_t *H = &ops[h];
    if (!tight_is_cmp(H->op) || H->dst >= 0)
        return 0;
    int b = H->tseq, x = H->fseq;
    if (b < 0 || b >= nops)
        return 0;
    const tight_op_t *B = &ops[b];
    if (B->op != TOP_BR_REM_EQ)
        return 0;
    int th = B->tseq, el = B->fseq;
    if (th < 0 || th >= nops || el < 0 || el >= nops)
        return 0;
    const tight_op_t *T = &ops[th];
    const tight_op_t *E = &ops[el];
    if (T->op != TOP_COPY || T->s0 >= 0 || T->dst < 0)
        return 0;
    if (T->tseq != x && T->tseq != TIP_EXIT)
        return 0;
    if (E->op != TOP_ADD || E->dst < 0 || E->s0 != E->dst)
        return 0;
    if (E->s1 >= 0 || E->c1 != 1)
        return 0;
    if (E->tseq != h)
        return 0;
    return 1;
}

/* Outer cmp+br; body may include add/sub/mul/rem/eq/ne/copy, 1-d/2-d xv.at/set,
 * and inner br/goto. Latch is goto cond. PC written only on region exit.
 * ARRAYND at/set write body[flat*elem] in borrowed shm; else region refused. */
static int try_tight_while_inc(kvlangKv_t *kv, const char *vtid, int d, int br_irseq,
                               const char *link_base, const char *fr, kvlangRwirInst_t *br) {
    if (!br || br->nr != 3 || br_irseq < 2)
        return 0;
    int body_i = irseq_of(&br->reads[1]);
    int exit_i = irseq_of(&br->reads[2]);
    if (body_i < 1 || exit_i < 1)
        return 0;
    int cond_i = br_irseq - 1;
    char err[128];
    const char *names[TIGHT_VARS];
    int kinds[TIGHT_VARS];
    tight_op_t ops[TIGHT_OPS];
    int nvars = 0, nops = 0, ck = 0, cl = -1, cr = -1, start_ip = 0;
    int64_t clc = 0, crc = 0;
    int used_cache = 0;
    if (g_tp.live && g_tp.br_irseq == br_irseq && strcmp(g_tp.base, link_base) == 0) {
        ck = g_tp.ck;
        cond_i = g_tp.cond_i;
        exit_i = g_tp.exit_i;
        nops = g_tp.nops;
        nvars = g_tp.nvars;
        cl = g_tp.cl;
        cr = g_tp.cr;
        clc = g_tp.clc;
        crc = g_tp.crc;
        start_ip = g_tp.start_ip;
        memcpy(ops, g_tp.ops, sizeof(tight_op_t) * (size_t)nops);
        memcpy(kinds, g_tp.kinds, sizeof(int) * (size_t)nvars);
        for (int i = 0; i < nvars; i++)
            names[i] = g_tp.nam[i];
        used_cache = 1;
    }
    int saw_latch = 0;
    if (!used_cache) {
    kvlangRwirInst_t *cmp = ic_get(kv, link_base, br_irseq - 1, err, sizeof err);
    if (!cmp || !cmp->opcode || cmp_kind_of(cmp->opcode) == 0 || cmp->nr < 2)
        return 0;
    ck = cmp_kind_of(cmp->opcode);
    int q[TIGHT_OPS * 2], qn = 0, qs = 0, qfull = 0;
    q[qn++] = body_i;
    while (qs < qn && nops < TIGHT_OPS) {
        int pc = q[qs++];
        if (pc == cond_i) {
            saw_latch = 1;
            continue;
        }
        if (pc == exit_i)
            continue;
        if (tight_find_ip(ops, nops, pc) >= 0)
            continue;
        kvlangRwirInst_t *in = ic_get(kv, link_base, pc, err, sizeof err);
        if (!in || !in->opcode)
            return 0;
        tight_op_t *o = &ops[nops];
        memset(o, 0, sizeof *o);
        o->irseq = pc;
        o->dst = o->s0 = o->s1 = o->s2 = o->s3 = -1;
        if (in->op_id == OPID_GOTO) {
            if (in->nr < 1)
                return 0;
            o->op = TOP_GOTO;
            o->tseq = irseq_of(&in->reads[0]);
            if (o->tseq < 1)
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = o->tseq;
            else
                qfull = 1;
            continue;
        }
        if (in->op_id == OPID_BR) {
            if (in->nr != 3)
                return 0;
            o->op = TOP_BR;
            if (!tight_src(&in->reads[0], names, kinds, &nvars, &o->s0, &o->c0, TK_BOOL))
                return 0;
            o->tseq = irseq_of(&in->reads[1]);
            o->fseq = irseq_of(&in->reads[2]);
            if (o->tseq < 1 || o->fseq < 1)
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]) - 1) {
                q[qn++] = o->tseq;
                q[qn++] = o->fseq;
            } else
                qfull = 1;
            continue;
        }
        if (in->op_id == OPID_COPY || (in->opcode && in->opcode[0] == '=' && !in->opcode[1])) {
            if (in->nr < 1 || in->nw < 1)
                return 0;
            o->op = TOP_COPY;
            int sk, ckind = -1;
            if (param_is_const(&in->reads[0], &o->c0, &ckind)) {
                sk = ckind;
                o->s0 = -1;
            } else {
                o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, -1);
                if (o->s0 < 0)
                    return 0;
                sk = kinds[o->s0];
            }
            o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, sk < 0 ? TK_I64 : sk);
            if (o->dst < 0)
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = pc + 1;
            else
                qfull = 1;
            continue;
        }
        if (in->opcode &&
            (strcmp(in->opcode, "println") == 0 || strcmp(in->opcode, "print") == 0 ||
             strcmp(in->opcode, "cerr") == 0)) {
            o->op = TOP_PRINT;
            o->c0 = in->opcode[0] == 'c' ? 2 : (strcmp(in->opcode, "print") == 0 ? 1 : 0);
            o->nidx = in->nr > 4 ? 4 : in->nr;
            o->s0 = o->s1 = o->s2 = o->s3 = -1;
            o->lit0[0] = 0;
            int *ss[4] = { &o->s0, &o->s1, &o->s2, &o->s3 };
            int64_t *cs[4] = { &o->c1, &o->c1, &o->c2, &o->c3 };
            for (int ai = 0; ai < o->nidx; ai++) {
                const kvlangParam_t *p = &in->reads[ai];
                if (p->val.data && !kvlangXvalueNone(&p->val) &&
                    kvlangXvalueIsCharKind(kvlangXvalueKind(&p->val))) {
                    char *s = kvlangXvalueValueString(&p->val);
                    if (ai == 0 && s)
                        snprintf(o->lit0, sizeof o->lit0, "%s", s);
                    *ss[ai] = -2;
                    free(s);
                    continue;
                }
                int sl = -1;
                int64_t cst = 0;
                if (!tight_src(p, names, kinds, &nvars, &sl, &cst, -1))
                    return 0;
                *ss[ai] = sl;
                if (sl < 0)
                    *cs[ai] = cst;
            }
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = pc + 1;
            else
                qfull = 1;
            continue;
        }
        int xs = xv_at_set(in->opcode);
        if (xs == 1) {
            if ((in->nr != 2 && in->nr != 3) || in->nw < 1)
                return 0;
            o->op = TOP_AT;
            o->nidx = in->nr - 1;
            o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, TK_ARR);
            if (o->s0 < 0)
                return 0;
            if (!tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_I64))
                return 0;
            if (o->nidx == 2 &&
                !tight_src(&in->reads[2], names, kinds, &nvars, &o->s2, &o->c2, TK_I64))
                return 0;
            o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, -1);
            if (o->dst < 0 || o->dst == o->s0)
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = pc + 1;
            else
                qfull = 1;
            continue;
        }
        if (xs == 2) {
            if ((in->nr != 3 && in->nr != 4) || in->nw < 1)
                return 0;
            o->op = TOP_SET;
            o->nidx = in->nr - 2;
            o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, TK_ARR);
            if (o->s0 < 0)
                return 0;
            o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, TK_ARR);
            if (o->dst != o->s0)
                return 0;
            if (!tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_I64))
                return 0;
            if (o->nidx == 1) {
                if (!tight_src(&in->reads[2], names, kinds, &nvars, &o->s2, &o->c2, TK_I64))
                    return 0;
            } else {
                if (!tight_src(&in->reads[2], names, kinds, &nvars, &o->s2, &o->c2, TK_I64))
                    return 0;
                if (!tight_src(&in->reads[3], names, kinds, &nvars, &o->s3, &o->c3, TK_I64))
                    return 0;
            }
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = pc + 1;
            else
                qfull = 1;
            continue;
        }
        int ak = arith_or_rel(in->opcode);
        if (!ak || in->nr < 2 || in->nw < 1)
            return 0;
        int dk = (ak == TOP_EQ || ak == TOP_NE || ak >= TOP_LT) ? TK_BOOL : TK_I64;
        o->op = ak;
        o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, dk);
        if (o->dst < 0)
            return 0;
        if (!tight_src(&in->reads[0], names, kinds, &nvars, &o->s0, &o->c0, TK_I64) ||
            !tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_I64))
            return 0;
        nops++;
        if (qn < (int)(sizeof q / sizeof q[0]))
            q[qn++] = pc + 1;
        else
            qfull = 1;
    }
    if (qfull || qs < qn)
        return 0;
    if (!saw_latch || nops < 1)
        return 0;
    for (int i = 0; i < nops; i++) {
        tight_op_t *o = &ops[i];
        if (o->op == TOP_AT) {
            if (o->s0 < 0 || kinds[o->s0] != TK_ARR)
                return 0;
            if (o->dst < 0 || kinds[o->dst] == TK_ARR)
                return 0;
            if (o->nidx != 1 && o->nidx != 2)
                return 0;
            if (o->s1 >= 0 && kinds[o->s1] == TK_ARR)
                return 0;
            if (o->nidx == 2 && o->s2 >= 0 && kinds[o->s2] == TK_ARR)
                return 0;
        } else if (o->op == TOP_SET) {
            if (o->s0 < 0 || kinds[o->s0] != TK_ARR || o->dst != o->s0)
                return 0;
            if (o->nidx != 1 && o->nidx != 2)
                return 0;
            if (o->s1 >= 0 && kinds[o->s1] == TK_ARR)
                return 0;
            if (o->s2 >= 0 && kinds[o->s2] == TK_ARR)
                return 0;
            if (o->nidx == 2 && o->s3 >= 0 && kinds[o->s3] == TK_ARR)
                return 0;
        } else {
            if (o->dst >= 0 && kinds[o->dst] == TK_ARR)
                return 0;
            if (o->s0 >= 0 && kinds[o->s0] == TK_ARR)
                return 0;
            if (o->s1 >= 0 && kinds[o->s1] == TK_ARR)
                return 0;
        }
    }

    if (!tight_src(&cmp->reads[0], names, kinds, &nvars, &cl, &clc, TK_I64) ||
        !tight_src(&cmp->reads[1], names, kinds, &nvars, &cr, &crc, TK_I64))
        return 0;
    start_ip = tight_compile(ops, &nops, cond_i, exit_i, body_i);
    if (start_ip < 0)
        return 0;
    memset(&g_tp, 0, sizeof g_tp);
    g_tp.live = 1;
    g_tp.br_irseq = br_irseq;
    g_tp.ck = ck;
    g_tp.cond_i = cond_i;
    g_tp.exit_i = exit_i;
    g_tp.nops = nops;
    g_tp.nvars = nvars;
    g_tp.cl = cl;
    g_tp.cr = cr;
    g_tp.clc = clc;
    g_tp.crc = crc;
    g_tp.start_ip = start_ip;
    snprintf(g_tp.base, sizeof g_tp.base, "%s", link_base);
    memcpy(g_tp.ops, ops, sizeof(tight_op_t) * (size_t)nops);
    memcpy(g_tp.kinds, kinds, sizeof(int) * (size_t)nvars);
    for (int i = 0; i < nvars; i++)
        snprintf(g_tp.nam[i], sizeof g_tp.nam[i], "%s", names[i] ? names[i] : "");
    }

    char *keys[TIGHT_VARS];
    kvlangXvalue_t hold[TIGHT_VARS];
    uint8_t *bodyp[TIGHT_VARS];
    int esz[TIGHT_VARS], alen[TIGHT_VARS], andim[TIGHT_VARS];
    int adim0[TIGHT_VARS], adim1[TIGHT_VARS];
    memset(keys, 0, sizeof keys);
    memset(hold, 0, sizeof hold);
    memset(bodyp, 0, sizeof bodyp);
    memset(esz, 0, sizeof esz);
    memset(alen, 0, sizeof alen);
    memset(andim, 0, sizeof andim);
    memset(adim0, 0, sizeof adim0);
    memset(adim1, 0, sizeof adim1);
    int pin = 1, has_arr = 0;
    for (int i = 0; i < nvars; i++) {
        if (kinds[i] < 0)
            kinds[i] = TK_I64;
        keys[i] = kvlangBuiltinResolveWriteSlot(kv, fr, names[i]);
        if (!keys[i])
            goto fail;
        kvlangXvalueZero(&hold[i]);
        if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0 || kvlangXvalueNone(&hold[i])) {
            if (kinds[i] == TK_ARR)
                goto fail;
            kvlangXvalue_t z;
            kvlangXvalueZero(&z);
            if (kinds[i] == TK_BOOL)
                kvlangXvalueNewBool(&z, false);
            else
                kvlangXvalueNewInt64(&z, 0);
            kvlangKvPair_t pair = { keys[i], z };
            char e2[128];
            kvlangKvSet(kv, &pair, 1, e2, sizeof e2);
            kvlangXvalueFree(&z);
            kvlangXvalueZero(&hold[i]);
            if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0 || kvlangXvalueNone(&hold[i]))
                goto fail;
        }
        kvspaceHead_t h;
        memset(&h, 0, sizeof h);
        if (kvlangXvalueHead(&hold[i], &h) != 0 || h.body_offset < 0)
            goto fail;
        if (kinds[i] == TK_ARR) {
            has_arr = 1;
            kvlang_kindexpr_t kx;
            kvlang_kindexpr_parse(h.kindexpr, &kx);
            int sz = kvlangXvalueElemSize(kvlangXvalueKind(&hold[i]));
            if (sz <= 0 || kx.ndim < 1 || kx.ndim > 2 || kx.dims[0] <= 0)
                goto fail;
            if (kx.ndim == 2 && kx.dims[1] <= 0)
                goto fail;
            int64_t nel = (int64_t)kx.dims[0] * (kx.ndim == 2 ? kx.dims[1] : 1);
            int64_t need = nel * sz;
            if (h.body_len < need || (uint32_t)h.body_offset + (uint32_t)need > hold[i].len)
                goto fail;
            esz[i] = sz;
            alen[i] = (int)nel;
            andim[i] = kx.ndim;
            adim0[i] = kx.dims[0];
            adim1[i] = kx.ndim == 2 ? kx.dims[1] : 1;
            bodyp[i] = hold[i].data + h.body_offset;
            if (!hold[i].borrowed)
                pin = 0;
            continue;
        }
        const char *want = kinds[i] == TK_BOOL ? KVSPACE_KIND_BOOL : KVSPACE_KIND_INT64;
        if (!kvlangXvalueKindIs(&hold[i], want))
            goto fail;
        int need = kinds[i] == TK_BOOL ? 1 : 8;
        if (h.body_len < need || (uint32_t)h.body_offset + (uint32_t)need > hold[i].len)
            goto fail;
        bodyp[i] = hold[i].data + h.body_offset;
        if (!hold[i].borrowed)
            pin = 0;
    }
    /* ARRAYND tight path is in-place shm body[flat*elem]; no malloc+Set fallback. */
    if (has_arr && !pin)
        goto fail;
    for (int i = 0; i < nops; i++) {
        if (ops[i].op != TOP_AT && ops[i].op != TOP_SET)
            continue;
        int as = ops[i].s0;
        if (as < 0 || andim[as] != ops[i].nidx)
            goto fail;
    }

    for (;;) {
        int64_t left = pin ? (cl >= 0 ? tight_load(bodyp, kinds, cl) : clc) : 0;
        int64_t right = pin ? (cr >= 0 ? tight_load(bodyp, kinds, cr) : crc) : 0;
        int64_t regs[TIGHT_VARS];
        if (!pin) {
            for (int i = 0; i < nvars; i++) {
                kvlangXvalueFree(&hold[i]);
                kvlangXvalueZero(&hold[i]);
                if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0) {
                    for (int j = 0; j < nvars; j++) {
                        kvlangXvalueFree(&hold[j]);
                        free(keys[j]);
                    }
                    return -1;
                }
                regs[i] = kinds[i] == TK_BOOL ? kvlangXvalueAsBool(&hold[i])
                                              : kvlangXvalueAsInt64(&hold[i]);
            }
            left = cl >= 0 ? regs[cl] : clc;
            right = cr >= 0 ? regs[cr] : crc;
        }
        int ok = 0;
        switch (ck) {
        case 1: ok = left < right; break;
        case 2: ok = left <= right; break;
        case 3: ok = left > right; break;
        case 4: ok = left >= right; break;
        }
        if (!ok)
            break;
        int ip = start_ip;
        int broke = 0;
        while (ip >= 0) {
            tight_op_t *o = &ops[ip];
            if (o->op == TOP_PRINT) {
                FILE *fp = o->c0 == 2 ? stderr : stdout;
                const char *sep = o->c0 == 1 ? "" : " ";
                int slots[4] = { o->s0, o->s1, o->s2, o->s3 };
                int64_t csts[4] = { o->c1, o->c1, o->c2, o->c3 };
                for (int ai = 0; ai < o->nidx; ai++) {
                    if (ai > 0 && sep[0])
                        fputs(sep, fp);
                    if (slots[ai] == -2)
                        fputs(o->lit0, fp);
                    else if (slots[ai] >= 0) {
                        int64_t v = pin ? tight_load(bodyp, kinds, slots[ai]) : regs[slots[ai]];
                        if (kinds[slots[ai]] == TK_BOOL)
                            fputs(v ? "true" : "false", fp);
                        else
                            fprintf(fp, "%lld", (long long)v);
                    } else
                        fprintf(fp, "%lld", (long long)csts[ai]);
                }
                if (o->c0 != 1)
                    fputc('\n', fp);
                ip = o->tseq;
                continue;
            }
            if (o->op == TOP_AT || o->op == TOP_SET) {
                if (!pin)
                    goto fail_run;
                int as = o->s0;
                if (as < 0 || esz[as] <= 0 || o->nidx != andim[as])
                    goto fail_run;
                int64_t i0 = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                int64_t flat;
                if (o->nidx == 1) {
                    if (i0 < 0 || i0 >= adim0[as])
                        goto fail_run;
                    flat = i0;
                } else {
                    int64_t i1 = o->s2 >= 0 ? tight_load(bodyp, kinds, o->s2) : o->c2;
                    if (i0 < 0 || i0 >= adim0[as] || i1 < 0 || i1 >= adim1[as])
                        goto fail_run;
                    flat = i0 * (int64_t)adim1[as] + i1;
                }
                if (flat < 0 || flat >= alen[as])
                    goto fail_run;
                uint8_t *ep = bodyp[as] + flat * esz[as];
                if (o->op == TOP_AT) {
                    if (o->dst < 0)
                        goto fail_run;
                    tight_store(bodyp, kinds, o->dst, rd_elem(ep, esz[as]));
                } else {
                    int64_t v;
                    if (o->nidx == 1)
                        v = o->s2 >= 0 ? tight_load(bodyp, kinds, o->s2) : o->c2;
                    else
                        v = o->s3 >= 0 ? tight_load(bodyp, kinds, o->s3) : o->c3;
                    wr_elem(ep, esz[as], v);
                }
                ip = o->tseq;
                continue;
            }
            if (pin && tight_tdiv_hdr(ops, nops, ip)) {
                const tight_op_t *H = o;
                const tight_op_t *B = &ops[H->tseq];
                const tight_op_t *T = &ops[B->tseq];
                const tight_op_t *E = &ops[B->fseq];
                for (;;) {
                    int64_t dv = H->s0 >= 0 ? tight_load(bodyp, kinds, H->s0) : H->c0;
                    int64_t nv = H->s1 >= 0 ? tight_load(bodyp, kinds, H->s1) : H->c1;
                    if (!tight_eval(H->op, dv, nv))
                        break;
                    int64_t a = B->s0 >= 0 ? tight_load(bodyp, kinds, B->s0) : B->c0;
                    int64_t b = B->s1 >= 0 ? tight_load(bodyp, kinds, B->s1) : B->c1;
                    int64_t rem = b == 0 ? 0 : a % b;
                    if (rem == B->cc) {
                        tight_store(bodyp, kinds, T->dst, T->c0);
                        break;
                    }
                    tight_store(bodyp, kinds, E->dst,
                                (E->s0 >= 0 ? tight_load(bodyp, kinds, E->s0) : E->c0) + 1);
                }
                ip = H->fseq;
                continue;
            }
            if (o->op == TOP_BR) {
                int64_t c = o->s0 >= 0 ? (pin ? tight_load(bodyp, kinds, o->s0) : regs[o->s0])
                                       : o->c0;
                ip = c ? o->tseq : o->fseq;
                continue;
            }
            if (o->op == TOP_BR_REM_EQ) {
                int64_t a = o->s0 >= 0 ? (pin ? tight_load(bodyp, kinds, o->s0) : regs[o->s0])
                                       : o->c0;
                int64_t b = o->s1 >= 0 ? (pin ? tight_load(bodyp, kinds, o->s1) : regs[o->s1])
                                       : o->c1;
                int64_t rem = b == 0 ? 0 : a % b;
                ip = (rem == o->cc) ? o->tseq : o->fseq;
                continue;
            }
            if (tight_is_cmp(o->op) && o->dst < 0) {
                int64_t a = o->s0 >= 0 ? (pin ? tight_load(bodyp, kinds, o->s0) : regs[o->s0])
                                       : o->c0;
                int64_t b = o->s1 >= 0 ? (pin ? tight_load(bodyp, kinds, o->s1) : regs[o->s1])
                                       : o->c1;
                ip = tight_eval(o->op, a, b) ? o->tseq : o->fseq;
                continue;
            }
            int64_t r;
            if (o->op == TOP_COPY) {
                r = o->s0 >= 0 ? (pin ? tight_load(bodyp, kinds, o->s0) : regs[o->s0]) : o->c0;
            } else {
                int64_t a = o->s0 >= 0 ? (pin ? tight_load(bodyp, kinds, o->s0) : regs[o->s0])
                                       : o->c0;
                int64_t b = o->s1 >= 0 ? (pin ? tight_load(bodyp, kinds, o->s1) : regs[o->s1])
                                       : o->c1;
                r = tight_eval(o->op, a, b);
            }
            if (o->dst < 0)
                goto fail_run;
            if (pin)
                tight_store(bodyp, kinds, o->dst, r);
            else {
                regs[o->dst] = r;
                kvlangXvalue_t xv;
                kvlangXvalueZero(&xv);
                if (kinds[o->dst] == TK_BOOL)
                    kvlangXvalueNewBool(&xv, r != 0);
                else
                    kvlangXvalueNewInt64(&xv, r);
                kvlangKvPair_t pair = { keys[o->dst], xv };
                char e2[128];
                kvlangKvSet(kv, &pair, 1, e2, sizeof e2);
                kvlangXvalueFree(&xv);
            }
            ip = o->tseq;
        }
        if (ip == TIP_EXIT)
            broke = 1;
        else if (ip != TIP_LATCH)
            goto fail_run;
        if (broke)
            break;
    }
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    kvlangVthreadWritePc(kv, vtid, d, exit_i);
    return 1;
fail_run:
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    return -1;
fail:
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    return 0;
}

/* float64 while: write-through borrowed shm bodies; packed PC only on region exit. */
static int try_tight_while_f64(kvlangKv_t *kv, const char *vtid, int d, int br_irseq,
                               const char *link_base, const char *fr, kvlangRwirInst_t *br) {
    if (!br || br->nr != 3 || br_irseq < 2)
        return 0;
    int body_i = irseq_of(&br->reads[1]);
    int exit_i = irseq_of(&br->reads[2]);
    if (body_i < 1 || exit_i < 1)
        return 0;
    int cond_i = br_irseq - 1;
    char err[128];
    kvlangRwirInst_t *cmp = ic_get(kv, link_base, br_irseq - 1, err, sizeof err);
    if (!cmp || !cmp->opcode || cmp_kind_of(cmp->opcode) == 0 || cmp->nr < 2)
        return 0;
    int ck = cmp_kind_of(cmp->opcode);
    const char *names[TIGHT_VARS];
    int kinds[TIGHT_VARS];
    tight_op_t ops[TIGHT_OPS];
    int nvars = 0, nops = 0, cl = -1, cr = -1, start_ip = 0;
    int64_t clc = 0, crc = 0;
    int q[TIGHT_OPS * 2], qn = 0, qs = 0, qfull = 0, saw_latch = 0;
    q[qn++] = body_i;
    while (qs < qn && nops < TIGHT_OPS) {
        int pc = q[qs++];
        if (pc == cond_i) {
            saw_latch = 1;
            continue;
        }
        if (pc == exit_i)
            continue;
        if (tight_find_ip(ops, nops, pc) >= 0)
            continue;
        kvlangRwirInst_t *in = ic_get(kv, link_base, pc, err, sizeof err);
        if (!in || !in->opcode)
            return 0;
        tight_op_t *o = &ops[nops];
        memset(o, 0, sizeof *o);
        o->irseq = pc;
        o->dst = o->s0 = o->s1 = o->s2 = o->s3 = -1;
        if (in->op_id == OPID_GOTO) {
            if (in->nr < 1)
                return 0;
            o->op = TOP_GOTO;
            o->tseq = irseq_of(&in->reads[0]);
            if (o->tseq < 1)
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = o->tseq;
            else
                qfull = 1;
            continue;
        }
        if (in->op_id == OPID_BR) {
            if (in->nr != 3)
                return 0;
            o->op = TOP_BR;
            if (!tight_src(&in->reads[0], names, kinds, &nvars, &o->s0, &o->c0, TK_BOOL))
                return 0;
            o->tseq = irseq_of(&in->reads[1]);
            o->fseq = irseq_of(&in->reads[2]);
            if (o->tseq < 1 || o->fseq < 1)
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]) - 1) {
                q[qn++] = o->tseq;
                q[qn++] = o->fseq;
            } else
                qfull = 1;
            continue;
        }
        if (in->op_id == OPID_COPY || (in->opcode && in->opcode[0] == '=' && !in->opcode[1])) {
            if (in->nr < 1 || in->nw < 1)
                return 0;
            o->op = TOP_COPY;
            int sk, ckind = -1;
            if (param_is_const(&in->reads[0], &o->c0, &ckind)) {
                sk = ckind;
                o->s0 = -1;
            } else {
                o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, -1);
                if (o->s0 < 0)
                    return 0;
                sk = kinds[o->s0];
            }
            o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name,
                                 sk < 0 ? TK_F64 : sk);
            if (o->dst < 0)
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = pc + 1;
            else
                qfull = 1;
            continue;
        }
        if (xv_at_set(in->opcode))
            return 0;
        int ak = arith_or_rel(in->opcode);
        if (!ak || ak == TOP_REM || in->nr < 2 || in->nw < 1)
            return 0;
        int dk = (ak == TOP_EQ || ak == TOP_NE || ak >= TOP_LT) ? TK_BOOL : TK_F64;
        o->op = ak;
        o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, dk);
        if (o->dst < 0)
            return 0;
        if (!tight_src(&in->reads[0], names, kinds, &nvars, &o->s0, &o->c0, TK_F64) ||
            !tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_F64))
            return 0;
        nops++;
        if (qn < (int)(sizeof q / sizeof q[0]))
            q[qn++] = pc + 1;
        else
            qfull = 1;
    }
    if (qfull || qs < qn || !saw_latch || nops < 1)
        return 0;
    for (int i = 0; i < nops; i++) {
        tight_op_t *o = &ops[i];
        if (o->op == TOP_AT || o->op == TOP_SET || o->op == TOP_REM || o->op == TOP_BR_REM_EQ)
            return 0;
        int sl[4] = { o->dst, o->s0, o->s1, o->s2 };
        for (int j = 0; j < 4; j++) {
            int s = sl[j];
            if (s < 0)
                continue;
            if (kinds[s] == TK_ARR || kinds[s] == TK_I64)
                return 0;
        }
    }
    if (!tight_src(&cmp->reads[0], names, kinds, &nvars, &cl, &clc, TK_F64) ||
        !tight_src(&cmp->reads[1], names, kinds, &nvars, &cr, &crc, TK_F64))
        return 0;
    start_ip = tight_compile(ops, &nops, cond_i, exit_i, body_i);
    if (start_ip < 0)
        return 0;

    char *keys[TIGHT_VARS];
    kvlangXvalue_t hold[TIGHT_VARS];
    uint8_t *bodyp[TIGHT_VARS];
    memset(keys, 0, sizeof keys);
    memset(hold, 0, sizeof hold);
    memset(bodyp, 0, sizeof bodyp);
    for (int i = 0; i < nvars; i++) {
        if (kinds[i] < 0)
            kinds[i] = TK_F64;
        if (kinds[i] != TK_F64 && kinds[i] != TK_BOOL)
            goto fail;
        keys[i] = kvlangBuiltinResolveWriteSlot(kv, fr, names[i]);
        if (!keys[i])
            goto fail;
        kvlangXvalueZero(&hold[i]);
        if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0 || kvlangXvalueNone(&hold[i]))
            goto fail;
        kvspaceHead_t h;
        memset(&h, 0, sizeof h);
        if (kvlangXvalueHead(&hold[i], &h) != 0 || h.body_offset < 0)
            goto fail;
        const char *want = kinds[i] == TK_BOOL ? KVSPACE_KIND_BOOL : KVSPACE_KIND_FLOAT64;
        if (!kvlangXvalueKindIs(&hold[i], want))
            goto fail;
        int need = kinds[i] == TK_BOOL ? 1 : 8;
        if (h.body_len < need || (uint32_t)h.body_offset + (uint32_t)need > hold[i].len)
            goto fail;
        bodyp[i] = hold[i].data + h.body_offset;
        if (!hold[i].borrowed)
            goto fail;
    }

    for (;;) {
        double left = f64_opnd(bodyp, cl, clc);
        double right = f64_opnd(bodyp, cr, crc);
        int ok = 0;
        switch (ck) {
        case 1: ok = left < right; break;
        case 2: ok = left <= right; break;
        case 3: ok = left > right; break;
        case 4: ok = left >= right; break;
        }
        if (!ok)
            break;
        int ip = start_ip;
        int broke = 0;
        while (ip >= 0) {
            tight_op_t *o = &ops[ip];
            if (o->op == TOP_BR) {
                int64_t c = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                ip = c ? o->tseq : o->fseq;
                continue;
            }
            if (tight_is_cmp(o->op) && o->dst < 0) {
                double a = f64_opnd(bodyp, o->s0, o->c0);
                double b = f64_opnd(bodyp, o->s1, o->c1);
                ip = f64_eval(o->op, a, b) != 0 ? o->tseq : o->fseq;
                continue;
            }
            if (o->op == TOP_AT || o->op == TOP_SET || o->op == TOP_REM ||
                o->op == TOP_BR_REM_EQ || o->op == TOP_GOTO)
                goto fail_run;
            double r;
            if (o->op == TOP_COPY) {
                if (o->dst >= 0 && kinds[o->dst] == TK_BOOL) {
                    int64_t v = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    tight_store(bodyp, kinds, o->dst, v);
                    ip = o->tseq;
                    continue;
                }
                r = f64_opnd(bodyp, o->s0, o->c0);
            } else {
                r = f64_eval(o->op, f64_opnd(bodyp, o->s0, o->c0),
                             f64_opnd(bodyp, o->s1, o->c1));
            }
            if (o->dst < 0)
                goto fail_run;
            if (kinds[o->dst] == TK_BOOL)
                tight_store(bodyp, kinds, o->dst, r != 0);
            else
                f64_store(bodyp[o->dst], r);
            ip = o->tseq;
        }
        if (ip == TIP_EXIT)
            broke = 1;
        else if (ip != TIP_LATCH)
            goto fail_run;
        if (broke)
            break;
    }
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    kvlangVthreadWritePc(kv, vtid, d, exit_i);
    return 1;
fail_run:
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    return -1;
fail:
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    return 0;
}

static int forin_is_add1(kvlangRwirInst_t *in, const char **names, int *kinds, int *nvars,
                         int *slot) {
    if (!in || !in->opcode || arith_or_rel(in->opcode) != TOP_ADD || in->nr < 2 || in->nw < 1)
        return 0;
    int dst = tight_intern(names, kinds, nvars, in->writes[0].name, TK_I64);
    if (dst < 0)
        return 0;
    int s0 = -1, s1 = -1;
    int64_t c0 = 0, c1 = 0;
    if (!tight_src(&in->reads[0], names, kinds, nvars, &s0, &c0, TK_I64) ||
        !tight_src(&in->reads[1], names, kinds, nvars, &s1, &c1, TK_I64))
        return 0;
    if (!((s0 == dst && s1 < 0 && c1 == 1) || (s1 == dst && s0 < 0 && c0 == 1)))
        return 0;
    *slot = dst;
    return 1;
}

static int forin_follow_goto(kvlangKv_t *kv, const char *base, int ir, int hops) {
    char err[128];
    for (int i = 0; i < hops; i++) {
        kvlangRwirInst_t *in = ic_get(kv, base, ir, err, sizeof err);
        if (!in || in->op_id != OPID_GOTO || in->nr < 1)
            return ir;
        int t = irseq_of(&in->reads[0]);
        if (t < 1)
            return -1;
        ir = t;
    }
    return ir;
}

/* while (k < n) { for-in; k+=1 } around this for-in: intern k,n and outer exit.
 * lenop is the init length builtin (ndarray·numel or kv·listlen). */
static int forin_match_outer_op(kvlangKv_t *kv, const char *base, int inc_i, int exit_i, int idx,
                               int len_slot, const char **names, int *kinds, int *nvars, int *kslot,
                               int *nslot, int64_t *ncst, int *oexit, const char *lenop) {
    char err[128];
    *kslot = -1;
    *nslot = -1;
    *ncst = 0;
    *oexit = -1;
    kvlangRwirInst_t *kinc = ic_get(kv, base, exit_i, err, sizeof err);
    int k = -1;
    if (!forin_is_add1(kinc, names, kinds, nvars, &k) || k == idx)
        return 0;
    kvlangRwirInst_t *gt = ic_get(kv, base, exit_i + 1, err, sizeof err);
    if (!gt || gt->op_id != OPID_GOTO || gt->nr < 1)
        return 0;
    int oc = irseq_of(&gt->reads[0]);
    if (oc < 1)
        return 0;
    kvlangRwirInst_t *ocmp = ic_get(kv, base, oc, err, sizeof err);
    if (!ocmp || !ocmp->opcode || cmp_kind_of(ocmp->opcode) != 1 || ocmp->nr < 2 || ocmp->nw < 1)
        return 0;
    int kl = -1, nr = -1;
    int64_t kc = 0, nrc = 0;
    if (!tight_src(&ocmp->reads[0], names, kinds, nvars, &kl, &kc, TK_I64) ||
        !tight_src(&ocmp->reads[1], names, kinds, nvars, &nr, &nrc, TK_I64))
        return 0;
    if (kl != k)
        return 0;
    kvlangRwirInst_t *obr = ic_get(kv, base, oc + 1, err, sizeof err);
    if (!obr || obr->op_id != OPID_BR || obr->nr != 3)
        return 0;
    if (!obr->reads[0].name || !ocmp->writes[0].name ||
        strcmp(obr->reads[0].name, ocmp->writes[0].name) != 0)
        return 0;
    int obody = irseq_of(&obr->reads[1]);
    int ox = irseq_of(&obr->reads[2]);
    if (obody < 1 || ox < 1)
        return 0;
    int init = forin_follow_goto(kv, base, obody, 4);
    if (init < 1)
        return 0;
    kvlangRwirInst_t *cp = ic_get(kv, base, init, err, sizeof err);
    if (!cp || cp->nr < 1 || cp->nw < 1)
        return 0;
    if (!(cp->op_id == OPID_COPY || (cp->opcode && cp->opcode[0] == '=' && !cp->opcode[1])))
        return 0;
    int64_t c0 = 0;
    int cknd = -1;
    if (!param_is_const(&cp->reads[0], &c0, &cknd) || c0 != -1)
        return 0;
    int idst = tight_intern(names, kinds, nvars, cp->writes[0].name, TK_I64);
    if (idst != idx)
        return 0;
    kvlangRwirInst_t *nu = ic_get(kv, base, init + 1, err, sizeof err);
    if (!nu || !nu->opcode || !lenop || strcmp(nu->opcode, lenop) != 0 || nu->nw < 1)
        return 0;
    int ldst = tight_intern(names, kinds, nvars, nu->writes[0].name, TK_I64);
    if (ldst < 0 || (len_slot >= 0 && ldst != len_slot))
        return 0;
    kvlangRwirInst_t *ig = ic_get(kv, base, init + 2, err, sizeof err);
    if (!ig || ig->op_id != OPID_GOTO || ig->nr < 1)
        return 0;
    if (irseq_of(&ig->reads[0]) != inc_i)
        return 0;
    *kslot = k;
    *nslot = nr;
    *ncst = nrc;
    *oexit = ox;
    return 1;
}

static int forin_match_outer(kvlangKv_t *kv, const char *base, int inc_i, int exit_i, int idx,
                             int len_slot, const char **names, int *kinds, int *nvars, int *kslot,
                             int *nslot, int64_t *ncst, int *oexit) {
    return forin_match_outer_op(kv, base, inc_i, exit_i, idx, len_slot, names, kinds, nvars, kslot,
                                nslot, ncst, oexit, "ndarray" MEMBER_SEP "numel");
}

/* Compact 1-d for-in: header idx+=1; idx<len at br-2,br-1. Body xv.at + int ops.
 * Optional host while (k < n) { for-in; k+=1 } is one region so pin is once.
 * Write-through borrowed shm; packed PC only on region exit; refuse if not borrowed. */
static int try_tight_forin(kvlangKv_t *kv, const char *vtid, int d, int br_irseq,
                           const char *link_base, const char *fr, kvlangRwirInst_t *br) {
    if (!br || br->nr != 3 || br_irseq < 3)
        return 0;
    int body_i = irseq_of(&br->reads[1]);
    int exit_i = irseq_of(&br->reads[2]);
    if (body_i < 1 || exit_i < 1)
        return 0;
    int inc_i = br_irseq - 2;
    char err[128];
    const char *names[TIGHT_VARS];
    int kinds[TIGHT_VARS];
    tight_op_t ops[TIGHT_OPS];
    int nvars = 0, nops = 0, ck = 1, cl = -1, cr = -1, start_ip = 0;
    int kslot = -1, nslot = -1, oexit = -1, swallow = 0;
    int64_t clc = 0, crc = 0, ncst = 0;
    int used_cache = 0;
    if (g_forin.live && g_forin.br_irseq == br_irseq && strcmp(g_forin.base, link_base) == 0) {
        ck = g_forin.ck;
        inc_i = g_forin.cond_i;
        exit_i = g_forin.exit_i;
        nops = g_forin.nops;
        nvars = g_forin.nvars;
        cl = g_forin.cl;
        cr = g_forin.cr;
        clc = g_forin.clc;
        crc = g_forin.crc;
        start_ip = g_forin.start_ip;
        kslot = g_forin_k;
        nslot = g_forin_n;
        oexit = g_forin_oexit;
        swallow = g_forin_sw;
        ncst = g_forin_ncst;
        memcpy(ops, g_forin.ops, sizeof(tight_op_t) * (size_t)nops);
        memcpy(kinds, g_forin.kinds, sizeof(int) * (size_t)nvars);
        for (int i = 0; i < nvars; i++)
            names[i] = g_forin.nam[i];
        used_cache = 1;
    }
    if (!used_cache) {
        kvlangRwirInst_t *inc = ic_get(kv, link_base, inc_i, err, sizeof err);
        kvlangRwirInst_t *cmp = ic_get(kv, link_base, br_irseq - 1, err, sizeof err);
        if (!inc || !inc->opcode || arith_or_rel(inc->opcode) != TOP_ADD || inc->nr < 2 || inc->nw < 1)
            return 0;
        if (!cmp || !cmp->opcode || cmp_kind_of(cmp->opcode) != 1 || cmp->nr < 2)
            return 0;
        int idx = tight_intern(names, kinds, &nvars, inc->writes[0].name, TK_I64);
        if (idx < 0)
            return 0;
        int as0 = -1, as1 = -1;
        int64_t ac0 = 0, ac1 = 0;
        if (!tight_src(&inc->reads[0], names, kinds, &nvars, &as0, &ac0, TK_I64) ||
            !tight_src(&inc->reads[1], names, kinds, &nvars, &as1, &ac1, TK_I64))
            return 0;
        if (!((as0 == idx && as1 < 0 && ac1 == 1) || (as1 == idx && as0 < 0 && ac0 == 1)))
            return 0;
        if (!tight_src(&cmp->reads[0], names, kinds, &nvars, &cl, &clc, TK_I64) ||
            !tight_src(&cmp->reads[1], names, kinds, &nvars, &cr, &crc, TK_I64))
            return 0;
        if (cl != idx)
            return 0;
        ck = 1;
        int q[TIGHT_OPS * 2], qn = 0, qs = 0, qfull = 0, saw_latch = 0;
        q[qn++] = body_i;
        while (qs < qn && nops < TIGHT_OPS) {
            int pc = q[qs++];
            if (pc == inc_i) {
                saw_latch = 1;
                continue;
            }
            if (pc == exit_i)
                continue;
            if (tight_find_ip(ops, nops, pc) >= 0)
                continue;
            kvlangRwirInst_t *in = ic_get(kv, link_base, pc, err, sizeof err);
            if (!in || !in->opcode)
                return 0;
            tight_op_t *o = &ops[nops];
            memset(o, 0, sizeof *o);
            o->irseq = pc;
            o->dst = o->s0 = o->s1 = o->s2 = o->s3 = -1;
            if (in->op_id == OPID_GOTO) {
                if (in->nr < 1)
                    return 0;
                o->op = TOP_GOTO;
                o->tseq = irseq_of(&in->reads[0]);
                if (o->tseq < 1)
                    return 0;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]))
                    q[qn++] = o->tseq;
                else
                    qfull = 1;
                continue;
            }
            if (in->op_id == OPID_BR) {
                if (in->nr != 3)
                    return 0;
                o->op = TOP_BR;
                if (!tight_src(&in->reads[0], names, kinds, &nvars, &o->s0, &o->c0, TK_BOOL))
                    return 0;
                o->tseq = irseq_of(&in->reads[1]);
                o->fseq = irseq_of(&in->reads[2]);
                if (o->tseq < 1 || o->fseq < 1)
                    return 0;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]) - 1) {
                    q[qn++] = o->tseq;
                    q[qn++] = o->fseq;
                } else
                    qfull = 1;
                continue;
            }
            if (in->op_id == OPID_COPY || (in->opcode && in->opcode[0] == '=' && !in->opcode[1])) {
                if (in->nr < 1 || in->nw < 1)
                    return 0;
                o->op = TOP_COPY;
                int sk, ckind = -1;
                if (param_is_const(&in->reads[0], &o->c0, &ckind)) {
                    sk = ckind;
                    o->s0 = -1;
                } else {
                    o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, -1);
                    if (o->s0 < 0)
                        return 0;
                    sk = kinds[o->s0];
                }
                o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, sk < 0 ? TK_I64 : sk);
                if (o->dst < 0)
                    return 0;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]))
                    q[qn++] = pc + 1;
                else
                    qfull = 1;
                continue;
            }
            int xs = xv_at_set(in->opcode);
            if (xs == 1) {
                if (in->nr != 2 || in->nw < 1)
                    return 0;
                o->op = TOP_AT;
                o->nidx = 1;
                o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, TK_ARR);
                if (o->s0 < 0)
                    return 0;
                if (!tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_I64))
                    return 0;
                o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, -1);
                if (o->dst < 0 || o->dst == o->s0)
                    return 0;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]))
                    q[qn++] = pc + 1;
                else
                    qfull = 1;
                continue;
            }
            if (xs == 2) {
                if (in->nr != 3 || in->nw < 1)
                    return 0;
                o->op = TOP_SET;
                o->nidx = 1;
                o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, TK_ARR);
                if (o->s0 < 0)
                    return 0;
                o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, TK_ARR);
                if (o->dst != o->s0)
                    return 0;
                if (!tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_I64) ||
                    !tight_src(&in->reads[2], names, kinds, &nvars, &o->s2, &o->c2, TK_I64))
                    return 0;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]))
                    q[qn++] = pc + 1;
                else
                    qfull = 1;
                continue;
            }
            int ak = arith_or_rel(in->opcode);
            if (!ak || in->nr < 2 || in->nw < 1)
                return 0;
            int dk = (ak == TOP_EQ || ak == TOP_NE || ak >= TOP_LT) ? TK_BOOL : TK_I64;
            o->op = ak;
            o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, dk);
            if (o->dst < 0)
                return 0;
            if (!tight_src(&in->reads[0], names, kinds, &nvars, &o->s0, &o->c0, TK_I64) ||
                !tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_I64))
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = pc + 1;
            else
                qfull = 1;
        }
        if (qfull || qs < qn)
            return 0;
        if (!saw_latch || nops < 1)
            return 0;
        int saw_at = 0;
        for (int i = 0; i < nops; i++) {
            tight_op_t *o = &ops[i];
            if (o->op == TOP_AT) {
                if (o->s0 < 0 || kinds[o->s0] != TK_ARR || o->nidx != 1)
                    return 0;
                if (o->dst < 0 || kinds[o->dst] == TK_ARR)
                    return 0;
                if (o->s1 != cl)
                    return 0;
                saw_at = 1;
            } else if (o->op == TOP_SET) {
                if (o->s0 < 0 || kinds[o->s0] != TK_ARR || o->dst != o->s0 || o->nidx != 1)
                    return 0;
            } else {
                if (o->dst >= 0 && kinds[o->dst] == TK_ARR)
                    return 0;
                if (o->s0 >= 0 && kinds[o->s0] == TK_ARR)
                    return 0;
                if (o->s1 >= 0 && kinds[o->s1] == TK_ARR)
                    return 0;
            }
        }
        if (!saw_at)
            return 0;
        start_ip = tight_compile(ops, &nops, inc_i, exit_i, body_i);
        if (start_ip < 0)
            return 0;
        swallow = forin_match_outer(kv, link_base, inc_i, exit_i, cl, cr, names, kinds, &nvars,
                                    &kslot, &nslot, &ncst, &oexit);
        memset(&g_forin, 0, sizeof g_forin);
        g_forin.live = 1;
        g_forin.br_irseq = br_irseq;
        g_forin.ck = ck;
        g_forin.cond_i = inc_i;
        g_forin.exit_i = exit_i;
        g_forin.nops = nops;
        g_forin.nvars = nvars;
        g_forin.cl = cl;
        g_forin.cr = cr;
        g_forin.clc = clc;
        g_forin.crc = crc;
        g_forin.start_ip = start_ip;
        g_forin_k = kslot;
        g_forin_n = nslot;
        g_forin_oexit = oexit;
        g_forin_sw = swallow;
        g_forin_ncst = ncst;
        snprintf(g_forin.base, sizeof g_forin.base, "%s", link_base);
        memcpy(g_forin.ops, ops, sizeof(tight_op_t) * (size_t)nops);
        memcpy(g_forin.kinds, kinds, sizeof(int) * (size_t)nvars);
        for (int i = 0; i < nvars; i++)
            snprintf(g_forin.nam[i], sizeof g_forin.nam[i], "%s", names[i] ? names[i] : "");
    }
    if (cl < 0)
        return 0;

    char *keys[TIGHT_VARS];
    kvlangXvalue_t hold[TIGHT_VARS];
    uint8_t *bodyp[TIGHT_VARS];
    int esz[TIGHT_VARS], alen[TIGHT_VARS];
    memset(keys, 0, sizeof keys);
    memset(hold, 0, sizeof hold);
    memset(bodyp, 0, sizeof bodyp);
    memset(esz, 0, sizeof esz);
    memset(alen, 0, sizeof alen);
    int has_arr = 0;
    for (int i = 0; i < nvars; i++) {
        if (kinds[i] < 0)
            kinds[i] = TK_I64;
        if (kinds[i] == TK_F64)
            goto fail;
        keys[i] = kvlangBuiltinResolveWriteSlot(kv, fr, names[i]);
        if (!keys[i])
            goto fail;
        kvlangXvalueZero(&hold[i]);
        if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0 || kvlangXvalueNone(&hold[i])) {
            if (kinds[i] == TK_ARR)
                goto fail;
            kvlangXvalue_t z;
            kvlangXvalueZero(&z);
            if (kinds[i] == TK_BOOL)
                kvlangXvalueNewBool(&z, false);
            else
                kvlangXvalueNewInt64(&z, 0);
            kvlangKvPair_t pair = { keys[i], z };
            char e2[128];
            kvlangKvSet(kv, &pair, 1, e2, sizeof e2);
            kvlangXvalueFree(&z);
            kvlangXvalueZero(&hold[i]);
            if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0 || kvlangXvalueNone(&hold[i]))
                goto fail;
        }
        kvspaceHead_t h;
        memset(&h, 0, sizeof h);
        if (kvlangXvalueHead(&hold[i], &h) != 0 || h.body_offset < 0)
            goto fail;
        if (kinds[i] == TK_ARR) {
            has_arr = 1;
            kvlang_kindexpr_t kx;
            kvlang_kindexpr_parse(h.kindexpr, &kx);
            const char *ak = kvlangXvalueKind(&hold[i]);
            if (!kvlangXvalueIsIntKind(ak) && !kvlangXvalueIsUintKind(ak))
                goto fail;
            int sz = kvlangXvalueElemSize(ak);
            if (sz <= 0 || kx.ndim != 1 || kx.dims[0] <= 0)
                goto fail;
            int64_t need = (int64_t)kx.dims[0] * sz;
            if (h.body_len < need || (uint32_t)h.body_offset + (uint32_t)need > hold[i].len)
                goto fail;
            esz[i] = sz;
            alen[i] = kx.dims[0];
            bodyp[i] = hold[i].data + h.body_offset;
            if (!hold[i].borrowed)
                goto fail;
            continue;
        }
        const char *want = kinds[i] == TK_BOOL ? KVSPACE_KIND_BOOL : KVSPACE_KIND_INT64;
        if (!kvlangXvalueKindIs(&hold[i], want))
            goto fail;
        int need = kinds[i] == TK_BOOL ? 1 : 8;
        if (h.body_len < need || (uint32_t)h.body_offset + (uint32_t)need > hold[i].len)
            goto fail;
        bodyp[i] = hold[i].data + h.body_offset;
        if (!hold[i].borrowed)
            goto fail;
    }
    if (!has_arr)
        goto fail;
    for (int i = 0; i < nops; i++) {
        if (ops[i].op != TOP_AT && ops[i].op != TOP_SET)
            continue;
        int as = ops[i].s0;
        if (as < 0 || esz[as] <= 0 || alen[as] <= 0)
            goto fail;
    }

    if (swallow && (kslot < 0 || oexit < 1))
        swallow = 0;
    int pc_out = exit_i;
    for (;;) {
        for (;;) {
            int64_t left = tight_load(bodyp, kinds, cl);
            int64_t right = cr >= 0 ? tight_load(bodyp, kinds, cr) : crc;
            int ok = 0;
            switch (ck) {
            case 1: ok = left < right; break;
            case 2: ok = left <= right; break;
            case 3: ok = left > right; break;
            case 4: ok = left >= right; break;
            }
            if (!ok)
                break;
            int ip = start_ip;
            int broke = 0;
            while (ip >= 0) {
                tight_op_t *o = &ops[ip];
                if (o->op == TOP_AT || o->op == TOP_SET) {
                    int as = o->s0;
                    if (as < 0 || esz[as] <= 0)
                        goto fail_run;
                    int64_t i0 = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                    if (i0 < 0 || i0 >= alen[as])
                        goto fail_run;
                    uint8_t *ep = bodyp[as] + i0 * esz[as];
                    if (o->op == TOP_AT) {
                        if (o->dst < 0)
                            goto fail_run;
                        tight_store(bodyp, kinds, o->dst, rd_elem(ep, esz[as]));
                    } else {
                        int64_t v = o->s2 >= 0 ? tight_load(bodyp, kinds, o->s2) : o->c2;
                        wr_elem(ep, esz[as], v);
                    }
                    ip = o->tseq;
                    continue;
                }
                if (o->op == TOP_BR) {
                    int64_t c = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    ip = c ? o->tseq : o->fseq;
                    continue;
                }
                if (o->op == TOP_BR_REM_EQ) {
                    int64_t a = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    int64_t b = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                    int64_t rem = b == 0 ? 0 : a % b;
                    ip = (rem == o->cc) ? o->tseq : o->fseq;
                    continue;
                }
                if (tight_is_cmp(o->op) && o->dst < 0) {
                    int64_t a = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    int64_t b = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                    ip = tight_eval(o->op, a, b) ? o->tseq : o->fseq;
                    continue;
                }
                int64_t r;
                if (o->op == TOP_COPY) {
                    r = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                } else {
                    int64_t a = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    int64_t b = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                    r = tight_eval(o->op, a, b);
                }
                if (o->dst < 0)
                    goto fail_run;
                tight_store(bodyp, kinds, o->dst, r);
                ip = o->tseq;
            }
            if (ip == TIP_EXIT)
                broke = 1;
            else if (ip != TIP_LATCH)
                goto fail_run;
            if (broke)
                break;
            tight_store(bodyp, kinds, cl, tight_load(bodyp, kinds, cl) + 1);
        }
        if (!swallow)
            break;
        tight_store(bodyp, kinds, kslot, tight_load(bodyp, kinds, kslot) + 1);
        int64_t kvv = tight_load(bodyp, kinds, kslot);
        int64_t nvv = nslot >= 0 ? tight_load(bodyp, kinds, nslot) : ncst;
        if (!(kvv < nvv)) {
            pc_out = oexit;
            break;
        }
        tight_store(bodyp, kinds, cl, 0);
    }
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    kvlangVthreadWritePc(kv, vtid, d, pc_out);
    return 1;
fail_run:
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    return -1;
fail:
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    return 0;
}

/* stringkeymap scatter for-in: header idx+=1; idx<len at br-2,br-1.
 * Body kv·listn+kv·get fused to MAP_AT. Pin ·[0]..·[n) from marker dims as borrowed
 * int64 (not kv.list). Write-through scalars; packed PC on region exit. */
static int try_tight_forin_map(kvlangKv_t *kv, const char *vtid, int d, int br_irseq,
                               const char *link_base, const char *fr, kvlangRwirInst_t *br) {
    if (!br || br->nr != 3 || br_irseq < 3)
        return 0;
    int body_i = irseq_of(&br->reads[1]);
    int exit_i = irseq_of(&br->reads[2]);
    if (body_i < 1 || exit_i < 1)
        return 0;
    int inc_i = br_irseq - 2;
    char err[128];
    const char *names[TIGHT_VARS];
    int kinds[TIGHT_VARS];
    tight_op_t ops[TIGHT_OPS];
    int nvars = 0, nops = 0, ck = 1, cl = -1, cr = -1, start_ip = 0;
    int kslot = -1, nslot = -1, oexit = -1, swallow = 0;
    int64_t clc = 0, crc = 0, ncst = 0;
    int used_cache = 0;
    if (g_fmap.live && g_fmap.br_irseq == br_irseq && strcmp(g_fmap.base, link_base) == 0) {
        ck = g_fmap.ck;
        inc_i = g_fmap.cond_i;
        exit_i = g_fmap.exit_i;
        nops = g_fmap.nops;
        nvars = g_fmap.nvars;
        cl = g_fmap.cl;
        cr = g_fmap.cr;
        clc = g_fmap.clc;
        crc = g_fmap.crc;
        start_ip = g_fmap.start_ip;
        kslot = g_fmap_k;
        nslot = g_fmap_n;
        oexit = g_fmap_oexit;
        swallow = g_fmap_sw;
        ncst = g_fmap_ncst;
        memcpy(ops, g_fmap.ops, sizeof(tight_op_t) * (size_t)nops);
        memcpy(kinds, g_fmap.kinds, sizeof(int) * (size_t)nvars);
        for (int i = 0; i < nvars; i++)
            names[i] = g_fmap.nam[i];
        used_cache = 1;
    }
    if (!used_cache) {
        kvlangRwirInst_t *inc = ic_get(kv, link_base, inc_i, err, sizeof err);
        kvlangRwirInst_t *cmp = ic_get(kv, link_base, br_irseq - 1, err, sizeof err);
        if (!inc || !inc->opcode || arith_or_rel(inc->opcode) != TOP_ADD || inc->nr < 2 || inc->nw < 1)
            return 0;
        if (!cmp || !cmp->opcode || cmp_kind_of(cmp->opcode) != 1 || cmp->nr < 2)
            return 0;
        int idx = tight_intern(names, kinds, &nvars, inc->writes[0].name, TK_I64);
        if (idx < 0)
            return 0;
        int as0 = -1, as1 = -1;
        int64_t ac0 = 0, ac1 = 0;
        if (!tight_src(&inc->reads[0], names, kinds, &nvars, &as0, &ac0, TK_I64) ||
            !tight_src(&inc->reads[1], names, kinds, &nvars, &as1, &ac1, TK_I64))
            return 0;
        if (!((as0 == idx && as1 < 0 && ac1 == 1) || (as1 == idx && as0 < 0 && ac0 == 1)))
            return 0;
        if (!tight_src(&cmp->reads[0], names, kinds, &nvars, &cl, &clc, TK_I64) ||
            !tight_src(&cmp->reads[1], names, kinds, &nvars, &cr, &crc, TK_I64))
            return 0;
        if (cl != idx)
            return 0;
        ck = 1;
        int q[TIGHT_OPS * 2], qn = 0, qs = 0, qfull = 0, saw_latch = 0;
        q[qn++] = body_i;
        while (qs < qn && nops < TIGHT_OPS) {
            int pc = q[qs++];
            if (pc == inc_i) {
                saw_latch = 1;
                continue;
            }
            if (pc == exit_i)
                continue;
            if (tight_find_ip(ops, nops, pc) >= 0)
                continue;
            kvlangRwirInst_t *in = ic_get(kv, link_base, pc, err, sizeof err);
            if (!in || !in->opcode)
                return 0;
            tight_op_t *o = &ops[nops];
            memset(o, 0, sizeof *o);
            o->irseq = pc;
            o->dst = o->s0 = o->s1 = o->s2 = o->s3 = -1;
            if (in->op_id == OPID_GOTO) {
                if (in->nr < 1)
                    return 0;
                o->op = TOP_GOTO;
                o->tseq = irseq_of(&in->reads[0]);
                if (o->tseq < 1)
                    return 0;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]))
                    q[qn++] = o->tseq;
                else
                    qfull = 1;
                continue;
            }
            if (in->op_id == OPID_BR) {
                if (in->nr != 3)
                    return 0;
                o->op = TOP_BR;
                if (!tight_src(&in->reads[0], names, kinds, &nvars, &o->s0, &o->c0, TK_BOOL))
                    return 0;
                o->tseq = irseq_of(&in->reads[1]);
                o->fseq = irseq_of(&in->reads[2]);
                if (o->tseq < 1 || o->fseq < 1)
                    return 0;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]) - 1) {
                    q[qn++] = o->tseq;
                    q[qn++] = o->fseq;
                } else
                    qfull = 1;
                continue;
            }
            if (in->op_id == OPID_COPY || (in->opcode && in->opcode[0] == '=' && !in->opcode[1])) {
                if (in->nr < 1 || in->nw < 1)
                    return 0;
                o->op = TOP_COPY;
                int sk, ckind = -1;
                if (param_is_const(&in->reads[0], &o->c0, &ckind)) {
                    sk = ckind;
                    o->s0 = -1;
                } else {
                    o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, -1);
                    if (o->s0 < 0)
                        return 0;
                    sk = kinds[o->s0];
                }
                o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, sk < 0 ? TK_I64 : sk);
                if (o->dst < 0)
                    return 0;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]))
                    q[qn++] = pc + 1;
                else
                    qfull = 1;
                continue;
            }
            if (in->opcode && strcmp(in->opcode, "kv" MEMBER_SEP "listn") == 0) {
                if (in->nr < 2 || in->nw < 1)
                    return 0;
                kvlangRwirInst_t *gt = ic_get(kv, link_base, pc + 1, err, sizeof err);
                if (!gt || !gt->opcode || strcmp(gt->opcode, "kv" MEMBER_SEP "get") != 0)
                    return 0;
                if (gt->nr < 2 || gt->nw < 1)
                    return 0;
                if (!in->writes[0].name || !gt->reads[1].name ||
                    strcmp(in->writes[0].name, gt->reads[1].name) != 0)
                    return 0;
                if (!in->reads[0].name || !gt->reads[0].name ||
                    strcmp(in->reads[0].name, gt->reads[0].name) != 0)
                    return 0;
                o->op = TOP_MAP_AT;
                o->s0 = tight_intern(names, kinds, &nvars, in->reads[0].name, TK_MAP);
                if (o->s0 < 0)
                    return 0;
                if (!tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_I64))
                    return 0;
                if (o->s1 != cl)
                    return 0;
                o->dst = tight_intern(names, kinds, &nvars, gt->writes[0].name, TK_I64);
                if (o->dst < 0 || o->dst == o->s0)
                    return 0;
                nops++;
                if (nops >= TIGHT_OPS)
                    return 0;
                tight_op_t *dead = &ops[nops];
                memset(dead, 0, sizeof *dead);
                dead->op = TOP_DEAD;
                dead->irseq = pc + 1;
                dead->dst = dead->s0 = dead->s1 = dead->s2 = dead->s3 = -1;
                nops++;
                if (qn < (int)(sizeof q / sizeof q[0]))
                    q[qn++] = pc + 2;
                else
                    qfull = 1;
                continue;
            }
            int ak = arith_or_rel(in->opcode);
            if (!ak || in->nr < 2 || in->nw < 1)
                return 0;
            int dk = (ak == TOP_EQ || ak == TOP_NE || ak >= TOP_LT) ? TK_BOOL : TK_I64;
            o->op = ak;
            o->dst = tight_intern(names, kinds, &nvars, in->writes[0].name, dk);
            if (o->dst < 0)
                return 0;
            if (!tight_src(&in->reads[0], names, kinds, &nvars, &o->s0, &o->c0, TK_I64) ||
                !tight_src(&in->reads[1], names, kinds, &nvars, &o->s1, &o->c1, TK_I64))
                return 0;
            nops++;
            if (qn < (int)(sizeof q / sizeof q[0]))
                q[qn++] = pc + 1;
            else
                qfull = 1;
        }
        if (qfull || qs < qn)
            return 0;
        if (!saw_latch || nops < 1)
            return 0;
        int saw_at = 0;
        for (int i = 0; i < nops; i++) {
            tight_op_t *o = &ops[i];
            if (o->op == TOP_DEAD)
                continue;
            if (o->op == TOP_MAP_AT) {
                if (o->s0 < 0 || kinds[o->s0] != TK_MAP)
                    return 0;
                if (o->dst < 0 || kinds[o->dst] == TK_MAP || kinds[o->dst] == TK_ARR)
                    return 0;
                if (o->s1 != cl)
                    return 0;
                saw_at = 1;
            } else {
                if (o->dst >= 0 && (kinds[o->dst] == TK_MAP || kinds[o->dst] == TK_ARR))
                    return 0;
                if (o->s0 >= 0 && (kinds[o->s0] == TK_MAP || kinds[o->s0] == TK_ARR))
                    return 0;
                if (o->s1 >= 0 && (kinds[o->s1] == TK_MAP || kinds[o->s1] == TK_ARR))
                    return 0;
            }
        }
        if (!saw_at)
            return 0;
        start_ip = tight_compile(ops, &nops, inc_i, exit_i, body_i);
        if (start_ip < 0)
            return 0;
        swallow = forin_match_outer_op(kv, link_base, inc_i, exit_i, cl, cr, names, kinds, &nvars,
                                       &kslot, &nslot, &ncst, &oexit, "kv" MEMBER_SEP "listlen");
        memset(&g_fmap, 0, sizeof g_fmap);
        g_fmap.live = 1;
        g_fmap.br_irseq = br_irseq;
        g_fmap.ck = ck;
        g_fmap.cond_i = inc_i;
        g_fmap.exit_i = exit_i;
        g_fmap.nops = nops;
        g_fmap.nvars = nvars;
        g_fmap.cl = cl;
        g_fmap.cr = cr;
        g_fmap.clc = clc;
        g_fmap.crc = crc;
        g_fmap.start_ip = start_ip;
        g_fmap_k = kslot;
        g_fmap_n = nslot;
        g_fmap_oexit = oexit;
        g_fmap_sw = swallow;
        g_fmap_ncst = ncst;
        snprintf(g_fmap.base, sizeof g_fmap.base, "%s", link_base);
        memcpy(g_fmap.ops, ops, sizeof(tight_op_t) * (size_t)nops);
        memcpy(g_fmap.kinds, kinds, sizeof(int) * (size_t)nvars);
        for (int i = 0; i < nvars; i++)
            snprintf(g_fmap.nam[i], sizeof g_fmap.nam[i], "%s", names[i] ? names[i] : "");
    }
    if (cl < 0)
        return 0;

    char *keys[TIGHT_VARS];
    kvlangXvalue_t hold[TIGHT_VARS];
    uint8_t *bodyp[TIGHT_VARS];
    char *ekeys[TIGHT_MAP_ELEMS];
    kvlangXvalue_t ehold[TIGHT_MAP_ELEMS];
    uint8_t *elemp[TIGHT_MAP_ELEMS];
    int n_elem = 0;
    memset(keys, 0, sizeof keys);
    memset(hold, 0, sizeof hold);
    memset(bodyp, 0, sizeof bodyp);
    memset(ekeys, 0, sizeof ekeys);
    memset(ehold, 0, sizeof ehold);
    memset(elemp, 0, sizeof elemp);
    int has_map = 0;
    for (int i = 0; i < nvars; i++) {
        if (kinds[i] < 0)
            kinds[i] = TK_I64;
        if (kinds[i] == TK_F64 || kinds[i] == TK_ARR)
            goto fail;
        keys[i] = kvlangBuiltinResolveWriteSlot(kv, fr, names[i]);
        if (!keys[i])
            goto fail;
        kvlangXvalueZero(&hold[i]);
        if (kinds[i] == TK_MAP) {
            if (has_map)
                goto fail;
            if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0 || kvlangXvalueNone(&hold[i]))
                goto fail;
            if (!kvlangXvalueKindIs(&hold[i], KVSPACE_KIND_MAP) || !hold[i].borrowed)
                goto fail;
            int cnt = kvlangXvalueArrayLen(&hold[i]);
            if (cnt < 1 || cnt > TIGHT_MAP_ELEMS)
                goto fail;
            n_elem = cnt;
            int pin_ok = 1;
            for (int j = 0; j < cnt && pin_ok; j++) {
                char nbuf[32];
                snprintf(nbuf, sizeof nbuf, "[%d]", j);
                ekeys[j] = kvlangKeytreeMember(keys[i], nbuf);
                kvlangXvalueZero(&ehold[j]);
                if (!ekeys[j] || kvlangKvGetOne(kv, ekeys[j], &ehold[j]) != 0 ||
                    kvlangXvalueNone(&ehold[j]) || !ehold[j].borrowed ||
                    !kvlangXvalueKindIs(&ehold[j], KVSPACE_KIND_INT64))
                    pin_ok = 0;
                else {
                    kvspaceHead_t eh;
                    memset(&eh, 0, sizeof eh);
                    if (kvlangXvalueHead(&ehold[j], &eh) != 0 || eh.body_offset < 0 ||
                        eh.body_len < 8 ||
                        (uint32_t)eh.body_offset + 8u > ehold[j].len)
                        pin_ok = 0;
                    else
                        elemp[j] = ehold[j].data + eh.body_offset;
                }
            }
            if (!pin_ok)
                goto fail;
            has_map = 1;
            continue;
        }
        if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0 || kvlangXvalueNone(&hold[i])) {
            kvlangXvalue_t z;
            kvlangXvalueZero(&z);
            if (kinds[i] == TK_BOOL)
                kvlangXvalueNewBool(&z, false);
            else
                kvlangXvalueNewInt64(&z, 0);
            kvlangKvPair_t pair = { keys[i], z };
            char e2[128];
            kvlangKvSet(kv, &pair, 1, e2, sizeof e2);
            kvlangXvalueFree(&z);
            kvlangXvalueZero(&hold[i]);
            if (kvlangKvGetOne(kv, keys[i], &hold[i]) != 0 || kvlangXvalueNone(&hold[i]))
                goto fail;
        }
        kvspaceHead_t h;
        memset(&h, 0, sizeof h);
        if (kvlangXvalueHead(&hold[i], &h) != 0 || h.body_offset < 0)
            goto fail;
        const char *want = kinds[i] == TK_BOOL ? KVSPACE_KIND_BOOL : KVSPACE_KIND_INT64;
        if (!kvlangXvalueKindIs(&hold[i], want))
            goto fail;
        int need = kinds[i] == TK_BOOL ? 1 : 8;
        if (h.body_len < need || (uint32_t)h.body_offset + (uint32_t)need > hold[i].len)
            goto fail;
        bodyp[i] = hold[i].data + h.body_offset;
        if (!hold[i].borrowed)
            goto fail;
    }
    if (!has_map || n_elem < 1)
        goto fail;
    if (cr >= 0)
        tight_store(bodyp, kinds, cr, n_elem);

    if (swallow && (kslot < 0 || oexit < 1))
        swallow = 0;
    int pc_out = exit_i;
    for (;;) {
        for (;;) {
            int64_t left = tight_load(bodyp, kinds, cl);
            int64_t right = cr >= 0 ? tight_load(bodyp, kinds, cr) : crc;
            int ok = 0;
            switch (ck) {
            case 1: ok = left < right; break;
            case 2: ok = left <= right; break;
            case 3: ok = left > right; break;
            case 4: ok = left >= right; break;
            }
            if (!ok)
                break;
            int ip = start_ip;
            int broke = 0;
            while (ip >= 0) {
                tight_op_t *o = &ops[ip];
                if (o->op == TOP_MAP_AT) {
                    int64_t i0 = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                    if (i0 < 0 || i0 >= n_elem || !elemp[i0] || o->dst < 0)
                        goto fail_run;
                    tight_store(bodyp, kinds, o->dst, rd_i64_le(elemp[i0]));
                    ip = o->tseq;
                    continue;
                }
                if (o->op == TOP_BR) {
                    int64_t c = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    ip = c ? o->tseq : o->fseq;
                    continue;
                }
                if (o->op == TOP_BR_REM_EQ) {
                    int64_t a = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    int64_t b = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                    int64_t rem = b == 0 ? 0 : a % b;
                    ip = (rem == o->cc) ? o->tseq : o->fseq;
                    continue;
                }
                if (tight_is_cmp(o->op) && o->dst < 0) {
                    int64_t a = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    int64_t b = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                    ip = tight_eval(o->op, a, b) ? o->tseq : o->fseq;
                    continue;
                }
                int64_t r;
                if (o->op == TOP_COPY) {
                    r = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                } else {
                    int64_t a = o->s0 >= 0 ? tight_load(bodyp, kinds, o->s0) : o->c0;
                    int64_t b = o->s1 >= 0 ? tight_load(bodyp, kinds, o->s1) : o->c1;
                    r = tight_eval(o->op, a, b);
                }
                if (o->dst < 0)
                    goto fail_run;
                tight_store(bodyp, kinds, o->dst, r);
                ip = o->tseq;
            }
            if (ip == TIP_EXIT)
                broke = 1;
            else if (ip != TIP_LATCH)
                goto fail_run;
            if (broke)
                break;
            tight_store(bodyp, kinds, cl, tight_load(bodyp, kinds, cl) + 1);
        }
        if (!swallow)
            break;
        tight_store(bodyp, kinds, kslot, tight_load(bodyp, kinds, kslot) + 1);
        int64_t kvv = tight_load(bodyp, kinds, kslot);
        int64_t nvv = nslot >= 0 ? tight_load(bodyp, kinds, nslot) : ncst;
        if (!(kvv < nvv)) {
            pc_out = oexit;
            break;
        }
        tight_store(bodyp, kinds, cl, 0);
    }
    for (int i = 0; i < n_elem; i++) {
        kvlangXvalueFree(&ehold[i]);
        free(ekeys[i]);
    }
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    kvlangVthreadWritePc(kv, vtid, d, pc_out);
    return 1;
fail_run:
    for (int i = 0; i < n_elem; i++) {
        kvlangXvalueFree(&ehold[i]);
        free(ekeys[i]);
    }
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    return -1;
fail:
    for (int i = 0; i < n_elem; i++) {
        kvlangXvalueFree(&ehold[i]);
        free(ekeys[i]);
    }
    for (int i = 0; i < nvars; i++) {
        kvlangXvalueFree(&hold[i]);
        free(keys[i]);
    }
    return 0;
}

int kvlangKvcpuExecuteMode(kvlangKv_t *kv, const char *pc, kvmode_t mode, char **out_pc) {
    if (out_pc) *out_pc = NULL;
    kvlangStrbuf_t vtid_b; kvlangStrbufInit(&vtid_b);
    const char *vtid = kvlangKeytreeVtidFromPc(pc, &vtid_b);
    if (vtid[0] == 0) { kvlangStrbufFree(&vtid_b); return -1; }

    int rc = 0;
    int d = 1, irseq = 1;
    if (kvlangKeytreeParsePc(pc, &d, &irseq) != 0) {
        if (kvlangVthreadReadPacked(kv, vtid, &d, &irseq, NULL) != 0) {
            kvlangStrbufFree(&vtid_b);
            return -1;
        }
    } else {
        kvlangVthreadWritePc(kv, vtid, d, irseq);
    }

    char *fr = kvlangKeytreeFrameAt(vtid, d);
    char *link_base = kvlangKeytreeStack(fr);
    kvlangResolveCacheReset(fr);
    int cur_d = d;

    for (;;) {
        if (kvlangVthreadReadPacked(kv, vtid, &d, &irseq, NULL) != 0)
            break;
        if (d < 1 || irseq < 1)
            break;
        if (d > MAX_STACK_DEPTH) {
            char curbuf[768];
            snprintf(curbuf, sizeof curbuf, "%s/[%d,0]", fr, irseq);
            char msg[256];
            snprintf(msg, sizeof msg, "RecursionError: stack overflow: depth=%d pc=%s", d, curbuf);
            kvlangVthreadSetError(kv, vtid, curbuf, msg);
            rc = -1;
            break;
        }
        if (d != cur_d) {
            free(fr);
            free(link_base);
            fr = kvlangKeytreeFrameAt(vtid, d);
            link_base = kvlangKeytreeStack(fr);
            kvlangResolveCacheReset(fr);
            ic_flush();
            g_tp.live = 0;
            cur_d = d;
        }

        char err[256];
        kvlangRwirInst_t *inst = ic_get(kv, link_base, irseq, err, sizeof err);
        char curbuf[768];
        snprintf(curbuf, sizeof curbuf, "%s/[%d,0]", fr, irseq);
        if (!inst) {
            char msg[256]; snprintf(msg, sizeof msg, "decode: %s", err);
            kvlangVthreadSetError(kv, vtid, curbuf, msg);
            rc = -1;
            break;
        }

        kvlangLogDebug("[%s] PC=%s OP=%s R=%d W=%d", vtid, curbuf, inst->opcode ? inst->opcode : "(empty)", inst->nr, inst->nw);

        if (!inst->opcode || !inst->opcode[0]) {
            char msg[512];
            snprintf(msg, sizeof msg, "RuntimeError: no instruction at %s", curbuf);
            kvlangVthreadSetError(kv, vtid, curbuf, msg);
            rc = -1;
            break;
        }

        int exec_err = 0;
        int oid = inst->op_id;
        switch (oid) {
        case OPID_GOTO:
            exec_err = handle_control(kv, vtid, curbuf, inst);
            break;
        case OPID_BR: {
            int t = try_tight_forin(kv, vtid, d, irseq, link_base, fr, inst);
            if (t == 0)
                t = try_tight_forin_map(kv, vtid, d, irseq, link_base, fr, inst);
            if (t == 0)
                t = try_tight_while_inc(kv, vtid, d, irseq, link_base, fr, inst);
            if (t == 0)
                t = try_tight_while_f64(kv, vtid, d, irseq, link_base, fr, inst);
            if (t == 1) {
                exec_err = 0;
                break;
            }
            if (t < 0) {
                exec_err = -1;
                break;
            }
            exec_err = handle_control(kv, vtid, curbuf, inst);
            break;
        }
        case OPID_CALL:
        case OPID_RETURN:
            exec_err = handle_control(kv, vtid, curbuf, inst);
            break;
        case OPID_COPY:
            exec_err = kvlangBuiltinExecuteCopy(kv, vtid, curbuf, inst);
            break;
        case OPID_OTHER: {
            char *rk = kvlangKeytreeRwir(inst->opcode);
            int def_nr = 0;
            char *def_sig = load_def_reads(kv, rk, &def_nr);
            free(rk);
            if (def_sig) {
                exec_err = check_read_types(kv, vtid, curbuf, inst->opcode, def_sig, def_nr, inst->reads, inst->nr);
                free(def_sig);
            }
            if (exec_err == 0 && mode == KVMODE_RETURN) {
                if (out_pc) *out_pc = strdup(curbuf);
                free(fr); free(link_base);
                kvlangStrbufFree(&vtid_b);
                return 1;
            }
            if (exec_err == 0) exec_err = handoff_external_rwir(kv, vtid, curbuf, inst);
            break;
        }
        case OPID_USER: {
            kvlangRwirInst_t ci;
            ci.opcode = strdup(OP_CALL);
            ci.op_id = OPID_CALL;
            ci.nr = inst->nr + 1;
            ci.nw = inst->nw;
            ci.reads = malloc(sizeof(kvlangParam_t) * (size_t)ci.nr);
            ci.reads[0].name = strdup(inst->opcode);
            ci.reads[0].val.data = NULL; ci.reads[0].val.len = 0;
            for (int i = 0; i < inst->nr; i++) { ci.reads[i + 1] = inst->reads[i]; }
            ci.writes = inst->writes;
            exec_err = handle_control(kv, vtid, curbuf, &ci);
            free(ci.opcode); free(ci.reads[0].name); free(ci.reads);
            break;
        }
        default: {
            char *yield = NULL;
            kvlangFrame_t f = { kv, vtid, curbuf, inst, &yield };
            exec_err = kvlangBuiltinNative(&f);
            if (exec_err == 0 && yield) {
                if (out_pc) *out_pc = yield; else free(yield);
                free(fr); free(link_base);
                kvlangStrbufFree(&vtid_b);
                return 1;
            }
            break;
        }
        }

        if (exec_err == 2) { rc = 0; break; }
        if (exec_err != 0) { rc = -1; break; }
    }

    free(fr);
    free(link_base);
    kvlangStrbufFree(&vtid_b);
    return rc;
}

int kvlangKvcpuExecute(kvlangKv_t *kv, const char *pc) {
    int rc = kvlangKvcpuExecuteMode(kv, pc, KVMODE_WATCH, NULL);
    return rc == 1 ? 0 : rc;   /* WATCH 模式不返回 1，防御性归一 */
}
