#include "runtime_internal.h"

/* kv 访问统一走 kvspace-durable 兼容 C ABI（kvspace*）。
 * 后端由链接的 kvspace 库决定（kvspace-durable / kvspace-c 均导出同一 ABI）。 */

static void kvlangXvalueCopyMalloc(kvlangXvalue_t *out, const uint8_t *d, uint32_t len) {
    if (len > 0) {
        out->data = malloc(len);
        memcpy(out->data, d, len);
        out->len = len;
    }
}

static kvlangRefEnt_t *ref_find(kvlangKv_t *k, const char *key) {
    for (int i = 0; i < k->nref; i++)
        if (k->ref[i].key && strcmp(k->ref[i].key, key) == 0) return &k->ref[i];
    return NULL;
}

static void ref_put(kvlangKv_t *k, const char *key, const kvspaceRef_t *r) {
    kvlangRefEnt_t *e = ref_find(k, key);
    if (!e) {
        if (k->nref < KVLANG_REF_CAP) e = &k->ref[k->nref++];
        else { e = &k->ref[0]; free(e->key); }
        e->key = strdup(key);
    }
    e->block_id = r->block_id;
    e->gen = r->gen;
}

static int ref_ok(kvlangKv_t *k) {
    return k->ref_on && kvspaceResolveRef && kvspaceGetByRef;
}

void kvlangKvInvalidateFrame(kvlangKv_t *k, const char *fr) {
    if (!k || !k->ref_on || !fr || !fr[0]) return;
    size_t n = strlen(fr);
    int w = 0;
    for (int i = 0; i < k->nref; i++) {
        char *key = k->ref[i].key;
        if (key && strncmp(key, fr, n) == 0 && (key[n] == 0 || key[n] == '/')) {
            free(key);
            continue;
        }
        if (w != i) k->ref[w] = k->ref[i];
        w++;
    }
    k->nref = w;
}

kvlangKv_t *kvlangKvConnect(const char *dsn) {
    kvlangKv_t *k = calloc(1, sizeof(*k));
    k->h = kvspaceConnect(dsn);
    if (!k->h) { free(k); return NULL; }
    k->ref_on = 1;
    return k;
}

void kvlangKvDisconnect(kvlangKv_t *k) {
    if (!k) return;
    for (int i = 0; i < k->nref; i++) free(k->ref[i].key);
    if (k->h) kvspaceClose(k->h);
    free(k);
}

int kvlangKvGetOne(kvlangKv_t *k, const char *key, kvlangXvalue_t *out) {
    kvlangXvalueZero(out);
    uint8_t *d; uint32_t len;
    if (kvspaceGet(k->h, key, &d, &len) != 0) return -1;
    kvlangXvalueCopyMalloc(out, d, len);
    kvspaceBytesFree(d, len);
    return 0;
}

/* Frame member: GetBatch(dir, name). Full-path Get does not ext-fallback on [d] frames. */
int kvlangKvGetMember(kvlangKv_t *k, const char *dir, const char *name, kvlangXvalue_t *out) {
    kvlangXvalueZero(out);
    if (!name || !name[0]) return 0;
    char key[2048];
    if (ref_ok(k) && dir) {
        snprintf(key, sizeof key, "%s%s", dir, name);
        kvlangRefEnt_t *e = ref_find(k, key);
        if (e) {
            kvspaceRef_t r = { e->block_id, e->gen };
            uint8_t *d; uint32_t len;
            if (kvspaceGetByRef(k->h, &r, key, &d, &len) == 0) {
                e->block_id = r.block_id;
                e->gen = r.gen;
                kvlangXvalueCopyMalloc(out, d, len);
                kvspaceBytesFree(d, len);
                return 0;
            }
        }
    }
    char *nm = (char *)name;
    int rc = kvlangKvGetBatch(k, dir, &nm, 1, out);
    if (rc == 0 && !kvlangXvalueNone(out) && ref_ok(k) && dir) {
        kvspaceRef_t r;
        if (kvspaceResolveRef(k->h, key, &r) == 0) ref_put(k, key, &r);
    }
    return rc;
}

int kvlangKvGetBatch(kvlangKv_t *k, const char *prefix, char **names, int n, kvlangXvalue_t *out) {
    for (int i = 0; i < n; i++) kvlangXvalueZero(&out[i]);
    const char **ns = malloc(sizeof(char *) * (size_t)n);
    for (int i = 0; i < n; i++) ns[i] = names[i];
    uint8_t *d; uint32_t len;
    int rc = kvspaceGetBatch(k->h, prefix, ns, (uint32_t)n, &d, &len);
    free(ns);
    if (rc != 0) return rc;
    uint32_t off = 0;
    for (int i = 0; i < n; i++) {
        if (off + 4 > len) break;
        uint32_t vl = (uint32_t)d[off] | ((uint32_t)d[off + 1] << 8) | ((uint32_t)d[off + 2] << 16) | ((uint32_t)d[off + 3] << 24);
        off += 4;
        if (vl > 0 && off + vl <= len) kvlangXvalueCopyMalloc(&out[i], d + off, vl);
        off += vl;
    }
    kvspaceBytesFree(d, len);
    return 0;
}

int kvlangKvSet(kvlangKv_t *k, const kvlangKvPair_t *pairs, int n, char *err, uint32_t err_cap) {
    if (n <= 0) return 0;
    if (n == 1 && ref_ok(k) && kvspaceSetPartByRef && pairs[0].key) {
        kvlangRefEnt_t *e = ref_find(k, pairs[0].key);
        if (e) {
            kvspaceRef_t r = { e->block_id, e->gen };
            if (kvspaceSetPartByRef(k->h, &r, pairs[0].key, 0, pairs[0].val.data,
                                    pairs[0].val.len, err, err_cap) == 0) {
                e->block_id = r.block_id;
                e->gen = r.gen;
                return 0;
            }
        }
    }
    const char **keys = malloc(sizeof(char *) * (size_t)n);
    uint32_t *lens = malloc(sizeof(uint32_t) * (size_t)n);
    size_t total = 0;
    for (int i = 0; i < n; i++) {
        keys[i] = pairs[i].key;
        lens[i] = pairs[i].val.len;
        total += pairs[i].val.len;
    }
    uint8_t *vals = malloc(total ? total : 1);
    size_t off = 0;
    for (int i = 0; i < n; i++) {
        if (pairs[i].val.len) memcpy(vals + off, pairs[i].val.data, pairs[i].val.len);
        off += pairs[i].val.len;
    }
    int rc = kvspaceSet(k->h, keys, vals, lens, (uint32_t)n, err, err_cap);
    free(keys); free(lens); free(vals);
    if (rc == 0 && n == 1 && ref_ok(k) && pairs[0].key) {
        kvspaceRef_t r;
        if (kvspaceResolveRef(k->h, pairs[0].key, &r) == 0) ref_put(k, pairs[0].key, &r);
    }
    return rc;
}

int kvlangKvDel(kvlangKv_t *k, const char *key, char *err, uint32_t err_cap) {
    const char *keys[1] = { key };
    return kvspaceDel(k->h, keys, 1, err, err_cap);
}

int kvlangKvDelTree(kvlangKv_t *k, const char *prefix, char *err, uint32_t err_cap) {
    kvlangKvInvalidateFrame(k, prefix);
    return kvspaceDelTree(k->h, prefix, err, err_cap);
}

int kvlangKvCp(kvlangKv_t *k, const char *src, const char *dst, char *err, uint32_t err_cap) {
    if (!src || !dst)
        return -1;
    if (kvspaceCp)
        return kvspaceCp(k->h, src, dst, err, err_cap);
    kvlangXvalue_t v;
    kvlangXvalueZero(&v);
    if (kvlangKvGetOne(k, src, &v) != 0)
        return -1;
    kvlangKvPair_t p = { (char *)dst, v };
    int rc = kvlangKvSet(k, &p, 1, err, err_cap);
    kvlangXvalueFree(&v);
    return rc;
}

int kvlangKvMkindex(kvlangKv_t *k, const char *path, char *err, uint32_t err_cap) {
    return kvspaceMkindex(k->h, path, err, err_cap);
}

int kvlangKvExtIndex(kvlangKv_t *k, const char *path, const char *ext, char *err, uint32_t err_cap) {
    return kvspaceMkindexExt(k->h, path, ext, err, err_cap);
}

int kvlangKvDelExtIndex(kvlangKv_t *k, const char *path, char *err, uint32_t err_cap) {
    return kvspaceRmindexExt(k->h, path, err, err_cap);
}

int kvlangKvList(kvlangKv_t *k, const char *prefix, bool expand_ext, bool resolve,
            char ***out_names, int *out_count) {
    *out_names = NULL; *out_count = 0;
    uint8_t *d; uint32_t len;
    if (kvspaceList(k->h, prefix, expand_ext ? 1 : 0, resolve ? 1 : 0, &d, &len) != 0) return -1;
    if (len == 0) { if (d) kvspaceBytesFree(d, len); return 0; }
    char *s = malloc((size_t)len + 1);
    memcpy(s, d, len); s[len] = 0;
    kvspaceBytesFree(d, len);
    int cnt = 1;
    for (uint32_t i = 0; i < len; i++) if (s[i] == '\n') cnt++;
    char **names = malloc(sizeof(char *) * (size_t)cnt);
    int idx = 0;
    char *save = NULL;
    for (char *tok = strtok_r(s, "\n", &save); tok; tok = strtok_r(NULL, "\n", &save))
        names[idx++] = strdup(tok);
    free(s);
    *out_names = names; *out_count = idx;
    return 0;
}

int kvlangKvWatch(kvlangKv_t *k, const char *key, const kvlangXvalue_t *target, uint64_t tick_ns, kvlangXvalue_t *out) {
    kvlangXvalueZero(out);
    const uint8_t *t = target->data ? target->data : (const uint8_t *)"";
    uint32_t tl = target->len;
    uint8_t *d; uint32_t len;
    if (kvspaceWatch(k->h, key, t, tl, tick_ns, &d, &len) != 0) return -1;
    kvlangXvalueCopyMalloc(out, d, len);
    kvspaceBytesFree(d, len);
    return 0;
}
