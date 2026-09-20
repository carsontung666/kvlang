#include "runtime_internal.h"

int kvlangRwirExtractAddr0(const char *coord) {
    const char *p = coord;
    while (*p == '[' || *p == ' ' || *p == '\t') p++;
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

void kvlangRwirInstFree(kvlangRwirInst_t *inst) {
    free(inst->opcode);
    for (int i = 0; i < inst->nr; i++) { free(inst->reads[i].name); kvlangXvalueFree(&inst->reads[i].val); }
    for (int i = 0; i < inst->nw; i++) { free(inst->writes[i].name); kvlangXvalueFree(&inst->writes[i].val); }
    free(inst->reads);
    free(inst->writes);
    inst->opcode = NULL; inst->reads = NULL; inst->writes = NULL; inst->nr = inst->nw = 0;
}

int kvlangRwirDecodeAt(kvlangKv_t *kv, const char *link_base, int addr0, kvlangRwirInst_t *out,
                       char *err, uint32_t err_cap) {
    memset(out, 0, sizeof(*out));
    if (addr0 < 1) { snprintf(err, err_cap, "Decode: irseq %d < 1", addr0); return -1; }

    out->reads = malloc(sizeof(kvlangParam_t) * MAX_PARAMS);
    out->writes = malloc(sizeof(kvlangParam_t) * MAX_PARAMS);
    out->nr = out->nw = 0;

    enum { CHUNK = 8 };
    int have_op = 0;
    for (int base = 1; base <= MAX_PARAMS; base += CHUNK) {
        int n = MAX_PARAMS - base + 1;
        if (n > CHUNK)
            n = CHUNK;
        int nslots = have_op ? 2 * n : 1 + 2 * n;
        char *names[1 + 2 * CHUNK];
        char bufs[1 + 2 * CHUNK][32];
        kvlangXvalue_t vals[1 + 2 * CHUNK];
        int ni = 0;
        if (!have_op) {
            snprintf(bufs[ni], 32, "[%d,0]", addr0);
            names[ni] = bufs[ni];
            ni++;
        }
        for (int i = 0; i < n; i++) {
            int slot = base + i;
            snprintf(bufs[ni], 32, "[%d,-%d]", addr0, slot);
            names[ni] = bufs[ni];
            ni++;
            snprintf(bufs[ni], 32, "[%d,%d]", addr0, slot);
            names[ni] = bufs[ni];
            ni++;
        }
        if (kvlangKvGetBatch(kv, link_base, names, nslots, vals) != 0) {
            snprintf(err, err_cap, "Decode: GetBatch failed irseq=%d", addr0);
            kvlangRwirInstFree(out);
            return -1;
        }
        int vi = 0;
        if (!have_op) {
            if (!kvlangXvalueNone(&vals[0]))
                out->opcode = kvlangXvalueValueString(&vals[0]);
            kvlangXvalueFree(&vals[0]);
            vi = 1;
            have_op = 1;
        }
        int more = 0;
        for (int i = 0; i < n; i++) {
            kvlangXvalue_t *rv = &vals[vi++];
            if (!kvlangXvalueNone(rv) && out->nr < MAX_PARAMS) {
                out->reads[out->nr].name = kvlangXvalueValueString(rv);
                out->reads[out->nr].val = *rv;
                rv->data = NULL;
                rv->len = 0;
                out->nr++;
                more = 1;
            } else {
                kvlangXvalueFree(rv);
            }
            kvlangXvalue_t *wv = &vals[vi++];
            if (!kvlangXvalueNone(wv) && out->nw < MAX_PARAMS) {
                out->writes[out->nw].name = kvlangXvalueValueString(wv);
                out->writes[out->nw].val = *wv;
                wv->data = NULL;
                wv->len = 0;
                out->nw++;
                more = 1;
            } else {
                kvlangXvalueFree(wv);
            }
        }
        if (!more)
            break;
    }
    return 0;
}

int kvlangRwirDecode(kvlangKv_t *kv, const char *link_base, const char *pc, kvlangRwirInst_t *out,
                char *err, uint32_t err_cap) {
    const char *last = NULL;
    for (const char *p = pc; (p = strstr(p, "/[")) != NULL; p += 2) last = p;
    if (!last) { snprintf(err, err_cap, "Decode: invalid pc (no /[coord]): %s", pc); return -1; }
    int addr0 = kvlangRwirExtractAddr0(last + 1);
    return kvlangRwirDecodeAt(kv, link_base, addr0, out, err, err_cap);
}
