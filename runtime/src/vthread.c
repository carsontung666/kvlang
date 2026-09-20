#include "runtime_internal.h"

#define PC_PACK(d, i) (((int64_t)(d) << 32) | (uint32_t)(i))
#define PC_D(p) ((int)((int64_t)(p) >> 32))
#define PC_I(p) ((int)(uint32_t)(p))

static void pc_key(const char *vtid, kvlangStrbuf_t *k) {
    kvlangKeytreeVthreadPc(vtid, k);
}

int kvlangVthreadWritePc(kvlangKv_t *kv, const char *vtid, int d, int irseq) {
    kvlangStrbuf_t k;
    kvlangStrbufInit(&k);
    pc_key(vtid, &k);
    int64_t pack = PC_PACK(d, irseq);
    kvlangXvalue_t v;
    kvlangXvalueZero(&v);
    kvlangXvalueNewInt64(&v, pack);
    kvlangKvPair_t pair = { k.p, v };
    char err[256];
    int rc = kvlangKvSet(kv, &pair, 1, err, sizeof err);
    kvlangXvalueFree(&v);
    kvlangStrbufFree(&k);
    return rc;
}

int kvlangVthreadReadPacked(kvlangKv_t *kv, const char *vtid, int *d, int *irseq, char **status) {
    if (status) *status = NULL;
    kvlangStrbuf_t k;
    kvlangStrbufInit(&k);
    pc_key(vtid, &k);
    kvlangXvalue_t pv;
    kvlangXvalueZero(&pv);
    kvlangKvGetOne(kv, k.p, &pv);
    int rc = -1;
    if (!kvlangXvalueNone(&pv) && kvlangXvalueKindIs(&pv, KVSPACE_KIND_INT64)) {
        int64_t pack = kvlangXvalueAsInt64(&pv);
        *d = PC_D(pack);
        *irseq = PC_I(pack);
        rc = 0;
    } else if (!kvlangXvalueNone(&pv)) {
        char *path = kvlangXvalueValueString(&pv);
        if (path && kvlangKeytreeParsePc(path, d, irseq) == 0)
            rc = 0;
        free(path);
    }
    kvlangXvalueFree(&pv);
    if (status) {
        kvlangKeytreeVthreadStatus(vtid, &k);
        kvlangXvalue_t sv;
        kvlangXvalueZero(&sv);
        kvlangKvGetOne(kv, k.p, &sv);
        if (!kvlangXvalueNone(&sv))
            *status = kvlangXvalueValueString(&sv);
        kvlangXvalueFree(&sv);
    }
    kvlangStrbufFree(&k);
    return rc;
}

void kvlangVthreadGet(kvlangKv_t *kv, const char *vtid, char **pc, char **status) {
    *pc = NULL;
    *status = NULL;
    int d = 0, irseq = 0;
    if (kvlangVthreadReadPacked(kv, vtid, &d, &irseq, status) == 0 && d >= 1 && irseq >= 1)
        *pc = kvlangKeytreePcAt(vtid, d, irseq);
}

void kvlangVthreadSet(kvlangKv_t *kv, const char *vtid, const char *pc, const char *status) {
    int d = 1, irseq = 1;
    if (pc && kvlangKeytreeParsePc(pc, &d, &irseq) == 0)
        kvlangVthreadWritePc(kv, vtid, d, irseq);
    else if (pc && pc[0]) {
        kvlangStrbuf_t k1;
        kvlangStrbufInit(&k1);
        pc_key(vtid, &k1);
        kvlangXvalue_t v1;
        kvlangXvalueZero(&v1);
        kvlangXvalueNewCharUtf8(&v1, pc);
        kvlangKvPair_t pair = { k1.p, v1 };
        char err[256];
        kvlangKvSet(kv, &pair, 1, err, sizeof err);
        kvlangXvalueFree(&v1);
        kvlangStrbufFree(&k1);
    }
    if (status) {
        kvlangStrbuf_t k2;
        kvlangStrbufInit(&k2);
        kvlangKeytreeVthreadStatus(vtid, &k2);
        kvlangXvalue_t v2;
        kvlangXvalueZero(&v2);
        kvlangXvalueNewCharUtf8(&v2, status);
        kvlangKvPair_t pair = { k2.p, v2 };
        char err[256];
        kvlangKvSet(kv, &pair, 1, err, sizeof err);
        kvlangXvalueFree(&v2);
        kvlangStrbufFree(&k2);
    }
}

void kvlangVthreadSetDone(kvlangKv_t *kv, const char *vtid, const char *ret) {
    if (ret == NULL || ret[0] == 0) ret = "ok";
    kvlangStrbuf_t k; kvlangStrbufInit(&k);
    kvlangKeytreeVthreadStatus(vtid, &k);
    kvlangXvalue_t v; kvlangXvalueZero(&v);
    kvlangXvalueNewCharUtf8(&v, ret);
    kvlangKvPair_t pair = { k.p, v };
    char err[256];
    kvlangKvSet(kv, &pair, 1, err, sizeof err);
    kvlangXvalueFree(&v); kvlangStrbufFree(&k);
}

void kvlangVthreadSetError(kvlangKv_t *kv, const char *vtid, const char *pc, const char *msg) {
    kvlangStrbuf_t msg_path, pc_key, st_key; kvlangStrbufInit(&msg_path); kvlangStrbufInit(&pc_key); kvlangStrbufInit(&st_key);
    kvlangKeytreeVthreadStatusMsg(vtid, "error", &msg_path);
    kvlangKeytreeVthreadPc(vtid, &pc_key);
    kvlangKeytreeVthreadStatus(vtid, &st_key);

    /* 确保 .error/ 父目录存在 */
    char *sep = strrchr(msg_path.p, '/');
    if (sep) {
        kvlangStrbuf_t dir; kvlangStrbufInit(&dir);
        kvlangStrbufPutn(&dir, msg_path.p, (size_t)(sep - msg_path.p) + 1);
        char err[256];
        kvlangKvMkindex(kv, dir.p, err, sizeof err);
        kvlangStrbufFree(&dir);
    }

    kvlangXvalue_t vpc, vmsg, vst; kvlangXvalueZero(&vpc); kvlangXvalueZero(&vmsg); kvlangXvalueZero(&vst);
    kvlangXvalueNewCharUtf8(&vpc, pc);
    kvlangXvalueNewCharUtf8(&vmsg, msg);
    kvlangXvalueNewCharUtf8(&vst, "error");
    kvlangKvPair_t pairs[3] = { { pc_key.p, vpc }, { msg_path.p, vmsg }, { st_key.p, vst } };
    char err[256];
    kvlangKvSet(kv, pairs, 3, err, sizeof err);
    kvlangXvalueFree(&vpc); kvlangXvalueFree(&vmsg); kvlangXvalueFree(&vst);
    kvlangStrbufFree(&msg_path); kvlangStrbufFree(&pc_key); kvlangStrbufFree(&st_key);
}
