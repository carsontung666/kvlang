#include "runtime_internal.h"

int kvlangRwirExtractAddr0(const char *coord) {
    const char *p = coord;
    while (*p == '[' || *p == ' ' || *p == '\t')
        p++;
    char *end;
    long n = strtol(p, &end, 10);
    return end == p ? 0 : (int)n;
}

int kvlangRwirNextPc(const char *pc, kvlangStrbuf_t *out) {
    kvlangStrbufClear(out);
    const char *slash = strrchr(pc, '/');
    size_t plen = slash ? (size_t)(slash - pc) + 1 : 0;
    int num = kvlangRwirExtractAddr0(slash ? slash + 1 : pc);
    kvlangStrbufPutn(out, pc, plen);
    kvlangStrbufPrintf(out, "[%d,0]", num + 1);
    return 0;
}

/* 免分配版：把 pc 末段 [n,0] 换成 [n+1,0] 写入 buf，返回长度；cap 不足返回 0。
 * 供每指令的 PC 推进热路径复用调用方缓冲，免 strbuf 的 realloc + Printf。 */
size_t kvlangRwirNextPcBuf(const char *pc, char *buf, size_t cap) {
    const char *slash = strrchr(pc, '/');
    size_t plen = slash ? (size_t)(slash - pc) + 1 : 0;
    int num = kvlangRwirExtractAddr0(slash ? slash + 1 : pc);
    char tail[32];
    int tl = snprintf(tail, sizeof tail, "[%d,0]", num + 1);
    if (plen + (size_t)tl + 1 > cap) return 0;
    memcpy(buf, pc, plen);
    memcpy(buf + plen, tail, (size_t)tl + 1);
    return plen + (size_t)tl;
}

void kvlangRwirInstFree(kvlangRwirInst_t *inst) {
    free(inst->opcode);
    for (int i = 0; i < inst->nr; i++) {
        free(inst->reads[i].name);
        free(inst->reads[i].type);
        kvlangXvalueFree(&inst->reads[i].val);
    }
    for (int i = 0; i < inst->nw; i++) {
        free(inst->writes[i].name);
        free(inst->writes[i].type);
        kvlangXvalueFree(&inst->writes[i].val);
    }
    free(inst->reads);
    free(inst->writes);
    inst->opcode = NULL;
    inst->reads = NULL;
    inst->writes = NULL;
    inst->nr = inst->nw = 0;
}

static char *operand_type(const kvlangXvalue_t *v) {
    kvspaceHead_t h;
    if (kvlangXvalueHead(v, &h) != 0)
        return NULL;
    if (!kvlangXvalueKindIs(v, KVSPACE_KIND_RWIR) &&
        !kvlangXvalueKindIs(v, KVSPACE_KIND_RWFUNC))
        return strdup((const char *)h.langtype);
    int32_t len = 0;
    const uint8_t *body = kvlangXvalueBody(v, &h, &len);
    const uint8_t *end = body && len > 5 ?
        memchr(body + 5, 0, (size_t)len - 5) : NULL;
    return end && end + 1 < body + len ?
        strndup((const char *)end + 1, (size_t)(body + len - end - 1)) : NULL;
}

static int materialize_operand(kvlangKv_t *kv, const char *link_base,
                               const char *frame_root, const char *slot,
                               kvlangXvalue_t *v, char *err, uint32_t err_cap) {
    if (kvlangXvalueIsPtr(v))
        return 0;
    kvlangXvalueMaterialize(v);
    int descriptor = kvlangXvalueKindIs(v, KVSPACE_KIND_RWIR) ||
                     kvlangXvalueKindIs(v, KVSPACE_KIND_RWFUNC);
    char *target = NULL;
    char *type = operand_type(v);
    if (descriptor) {
        char *name = kvlangXvalueSlotName(v);
        if (!name || !name[0]) {
            snprintf(err, err_cap, "Decode: empty operand %s", slot);
            free(name);
            free(type);
            return -1;
        }
        target = kvlangBuiltinResolveWriteSlot(kv, frame_root, name);
        free(name);
        if (target && (!type || strcmp(type, "any") != 0)) {
            kvspaceHead_t actual;
            if (kvlangKvGetHead(kv, target, &actual) == 0 &&
                actual.langtype_len > 0 &&
                (size_t)actual.langtype_len <= sizeof actual.langtype) {
                char *actual_type = strndup((const char *)actual.langtype,
                                            (size_t)actual.langtype_len);
                if (!actual_type) {
                    snprintf(err, err_cap, "Decode: out of memory");
                    free(target);
                    free(type);
                    return -1;
                }
                free(type);
                type = actual_type;
            }
        }
    } else {
        kvlangStrbuf_t literal;
        kvlangStrbufInit(&literal);
        kvlangStrbufPrintf(&literal, "%s%soperand%s", link_base,
                           RUNTIME_MEMBER_SEP, slot);
        target = kvlangStrbufDetach(&literal);
        kvlangKvPair_t pair = {target, *v};
        if (kvlangKvSet(kv, &pair, 1, err, err_cap) != 0) {
            free(target);
            free(type);
            return -1;
        }
    }
    if (!target || !target[0]) {
        snprintf(err, err_cap, "Decode: invalid operand %s", slot);
        free(target);
        free(type);
        return -1;
    }
    kvlangXvalue_t ptr;
    kvlangXvalueZero(&ptr);
    kvlangXvalueNewPtr(&ptr, type ? type : "any", target);
    free(type);
    free(target);
    if (kvlangXvalueNone(&ptr)) {
        snprintf(err, err_cap, "Decode: invalid pointer at %s", slot);
        return -1;
    }
    kvlangStrbuf_t physical;
    kvlangStrbufInit(&physical);
    kvlangStrbufPrintf(&physical, "%s%s", link_base, slot);
    kvlangKvPair_t pair = {physical.p, ptr};
    int rc = kvlangKvSet(kv, &pair, 1, err, err_cap);
    kvlangStrbufFree(&physical);
    if (rc != 0) {
        kvlangXvalueFree(&ptr);
        return -1;
    }
    kvlangXvalueFree(v);
    *v = ptr;
    return 0;
}

static int decode_operand(kvlangKv_t *kv, const char *link_base,
                          const char *frame_root, const char *slot,
                          kvlangParam_t *out, char *err, uint32_t err_cap) {
    memset(out, 0, sizeof *out);
    kvlangXvalue_t v;
    kvlangXvalueZero(&v);
    if (kvlangKvGetMember(kv, link_base, slot, &v) != 0) {
        snprintf(err, err_cap, "Decode: cannot read %s", slot);
        return -1;
    }
    if (kvlangXvalueNone(&v)) {
        kvlangXvalueFree(&v);
        return 0;
    }
    if (materialize_operand(kv, link_base, frame_root, slot,
                            &v, err, err_cap) != 0) {
        kvlangXvalueFree(&v);
        return -1;
    }
    kvlangXvalueMaterialize(&v);
    out->name = kvlangXvaluePtrTarget(&v);
    kvspaceHead_t h;
    if (!out->name || kvlangXvalueHead(&v, &h) != 0) {
        snprintf(err, err_cap, "Decode: invalid pointer at %s", slot);
        kvlangXvalueFree(&v);
        free(out->name);
        out->name = NULL;
        return -1;
    }
    out->type = strdup((const char *)h.langtype);
    out->address = 1;
    if (!out->type || kvlangKvGetOne(kv, out->name, &out->val) != 0) {
        snprintf(err, err_cap, "Decode: cannot read target for %s", slot);
        free(out->name);
        free(out->type);
        out->name = out->type = NULL;
        kvlangXvalueFree(&out->val);
        kvlangXvalueFree(&v);
        return -1;
    }
    kvlangXvalueMaterialize(&out->val);
    kvlangXvalueFree(&v);
    return 1;
}

int kvlangRwirDecode(kvlangKv_t *kv, const char *link_base, const char *pc,
                     kvlangRwirInst_t *out, char *err, uint32_t err_cap) {
    memset(out, 0, sizeof(*out));
    const char *last = NULL;
    for (const char *p = pc; (p = strstr(p, "/[")) != NULL; p += 2)
        last = p;
    if (!last) {
        snprintf(err, err_cap, "Decode: invalid pc (no /[coord]): %s", pc);
        return -1;
    }
    int addr0 = kvlangRwirExtractAddr0(last + 1);

    kvlangStrbuf_t key;
    kvlangStrbufInit(&key);

    out->reads = malloc(sizeof(kvlangParam_t) * MAX_PARAMS);
    out->writes = malloc(sizeof(kvlangParam_t) * MAX_PARAMS);
    if (!out->reads || !out->writes) {
        snprintf(err, err_cap, "Decode: out of memory");
        kvlangRwirInstFree(out);
        kvlangStrbufFree(&key);
        return -1;
    }
    out->nr = out->nw = 0;
    char *frame_root = kvlangKeytreeFrameRoot(pc);

    /* 指令槽是稠密数组：opcode 在 [addr0,0]，读参 [addr0,-1..]、写参 [addr0,1..] 各自从 1 连续，
     * 首个缺失槽即终止。逐槽读、遇空即停，替代每步固定读满 1+2*MAX_PARAMS 个槽——durable 后端上
     * 那些缺失槽会各触发一次祖先 ext-index 解析，放大成 syscall 风暴（prime_sieve fs/redis 超时根因）。 */
    kvlangXvalue_t v;
    char *nm;

    kvlangStrbufPrintf(&key, "[%d,0]", addr0);
    nm = kvlangStrbufDetach(&key);
    kvlangKvGetMember(kv, link_base, nm, &v);
    free(nm);
    if (!kvlangXvalueNone(&v))
        out->opcode = kvlangXvalueValueString(&v);
    kvlangXvalueFree(&v);
    out->op_id =
        out->opcode ? kvlangOpClassify(out->opcode) : OPID_notinmyrwircaps;

    for (int i = 1; i <= MAX_PARAMS; i++) {
        kvlangStrbufPrintf(&key, "[%d,-%d]", addr0, i);
        nm = kvlangStrbufDetach(&key);
        int rc = decode_operand(kv, link_base, frame_root, nm,
                                &out->reads[out->nr], err, err_cap);
        free(nm);
        if (rc < 0)
            goto fail;
        if (rc == 0)
            break;
        out->nr++;
    }
    for (int i = 1; i <= MAX_PARAMS; i++) {
        kvlangStrbufPrintf(&key, "[%d,%d]", addr0, i);
        nm = kvlangStrbufDetach(&key);
        int rc = decode_operand(kv, link_base, frame_root, nm,
                                &out->writes[out->nw], err, err_cap);
        free(nm);
        if (rc < 0)
            goto fail;
        if (rc == 0)
            break;
        out->nw++;
    }

    free(frame_root);
    kvlangStrbufFree(&key);
    return 0;

fail:
    free(frame_root);
    kvlangStrbufFree(&key);
    kvlangRwirInstFree(out);
    return -1;
}
