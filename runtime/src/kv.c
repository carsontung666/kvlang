#include "runtime_internal.h"
#include <dlfcn.h>

/* kv 访问统一走 kvspace-durable 兼容 C ABI（kvspace*）。
 * 后端由链接的 kvspace 库决定（kvspace-durable / kvspace-c 均导出同一 ABI）。 */

static void kvlangXvalueCopyMalloc(kvlangXvalue_t *out, const uint8_t *d, uint32_t len) {
    out->borrowed = 0;
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

typedef uint8_t *(*kvlang_shm_get_fn)(void *, const char *, int, int32_t *);

static kvlang_shm_get_fn shm_get_fn;
static int shm_get_tried;

/* Live shm mapping (not malloc). NULL if this backend has no kvspaceShmGet. */
static uint8_t *shm_borrow_get(void *h, const char *key, uint32_t *out_len) {
    if (!h || !key || !out_len)
        return NULL;
    if (!shm_get_tried) {
        shm_get_tried = 1;
        void *so = dlopen("libkvspace-c.so.1", RTLD_NOLOAD | RTLD_LAZY);
        if (so)
            shm_get_fn = (kvlang_shm_get_fn)dlsym(so, "kvspaceShmGet");
    }
    if (!shm_get_fn)
        return NULL;
    void *bkv = *(void **)((char *)h + 16);
    if (!bkv)
        return NULL;
    int32_t n = 0;
    uint8_t *d = shm_get_fn(bkv, key, 0, &n);
    if (!d || n <= 0)
        return NULL;
    *out_len = (uint32_t)n;
    return d;
}

/* kvspaceGet / GetByRef buffers are process-owned malloc; never borrowed. */
static void take_owned(uint8_t *d, uint32_t len, kvlangXvalue_t *out) {
    kvlangXvalueCopyMalloc(out, d, len);
    if (d)
        kvspaceBytesFree(d, len);
}

static int take_shm_view(kvlangKv_t *k, const char *key, kvlangXvalue_t *out) {
    uint32_t len = 0;
    uint8_t *d = shm_borrow_get(k->h, key, &len);
    if (!d)
        return 0;
    k->borrow_get = 1;
    out->data = d;
    out->len = len;
    out->borrowed = 1;
    return 1;
}

kvlangKv_t *kvlangKvConnect(const char *dsn) {
    kvlangKv_t *k = calloc(1, sizeof(*k));
    k->h = kvspaceConnect(dsn);
    if (!k->h) { free(k); return NULL; }
    k->ref_on = 1;
    k->borrow_get = -1; /* unknown until first Get */
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
    if (take_shm_view(k, key, out))
        return 0;
    uint8_t *d = NULL;
    uint32_t len = 0;
    if (kvspaceGet(k->h, key, &d, &len) != 0) return -1;
    take_owned(d, len, out);
    return 0;
}

/* Frame member: GetBatch(dir, name). Full-path Get does not ext-fallback on [d] frames. */
int kvlangKvGetMember(kvlangKv_t *k, const char *dir, const char *name, kvlangXvalue_t *out) {
    kvlangXvalueZero(out);
    if (!name || !name[0]) return 0;
    char key[2048];
    key[0] = 0;
    if (dir)
        snprintf(key, sizeof key, "%s%s", dir, name);
    if (key[0] && take_shm_view(k, key, out)) {
        if (ref_ok(k)) {
            kvspaceRef_t r;
            if (kvspaceResolveRef(k->h, key, &r) == 0)
                ref_put(k, key, &r);
        }
        return 0;
    }
    if (ref_ok(k) && key[0]) {
        kvlangRefEnt_t *e = ref_find(k, key);
        if (e) {
            kvspaceRef_t r = { e->block_id, e->gen };
            uint8_t *d; uint32_t len;
            if (kvspaceGetByRef(k->h, &r, key, &d, &len) == 0) {
                e->block_id = r.block_id;
                e->gen = r.gen;
                take_owned(d, len, out);
                return 0;
            }
        }
    }
    if (key[0]) {
        uint8_t *d;
        uint32_t len;
        if (kvspaceGet(k->h, key, &d, &len) == 0 && d && len) {
            take_owned(d, len, out);
            if (ref_ok(k)) {
                kvspaceRef_t r;
                if (kvspaceResolveRef(k->h, key, &r) == 0)
                    ref_put(k, key, &r);
            }
            return 0;
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

/* True if key is an ART leaf (not an extindex fallback to another tree). */
static int key_has_own_node(kvlangKv_t *k, const char *key) {
    if (!k || !key || !key[0])
        return 0;
    const char *slash = strrchr(key, '/');
    if (!slash)
        return 0;
    char parent[2048];
    size_t n = (size_t)(slash - key + 1);
    if (n >= sizeof parent)
        return 0;
    memcpy(parent, key, n);
    parent[n] = 0;
    const char *name = slash + 1;
    if (!name[0])
        return 0;
    char **names = NULL;
    int count = 0;
    if (kvlangKvList(k, parent, false, false, &names, &count) != 0)
        return 0;
    int found = 0;
    for (int i = 0; i < count; i++) {
        if (!found && names[i] && strcmp(names[i], name) == 0)
            found = 1;
        free(names[i]);
    }
    free(names);
    return found;
}

int kvlangKvSet(kvlangKv_t *k, const kvlangKvPair_t *pairs, int n, char *err, uint32_t err_cap) {
    if (n <= 0) return 0;
    if (n == 1 && pairs[0].key && pairs[0].val.data) {
        kvspaceHead_t hd;
        int32_t blen = 0;
        const uint8_t *src = NULL;
        if (kvlangXvalueHead(&pairs[0].val, &hd) == 0)
            src = kvlangXvalueBody(&pairs[0].val, &hd, &blen);
        if (src && blen > 0) {
            if (kvspaceWriteInPlace) {
                uint8_t *body = NULL;
                if (kvspaceWriteInPlace(k->h, pairs[0].key, 0, (uint32_t)blen, &body,
                                        err, err_cap) == 0 &&
                    body) {
                    memcpy(body, src, (size_t)blen);
                    return 0;
                }
            }
            /* Borrowed Get follows extindex; memcpy would punch through into
             * shared /lib IR. Only in-place when this key already has its own node. */
            if (k->borrow_get > 0 && key_has_own_node(k, pairs[0].key)) {
                kvlangXvalue_t cur;
                kvlangXvalueZero(&cur);
                if (kvlangKvGetOne(k, pairs[0].key, &cur) == 0 && cur.borrowed &&
                    cur.data) {
                    kvspaceHead_t hd_cur;
                    int32_t oldb = 0;
                    uint8_t *dst = NULL;
                    if (kvlangXvalueHead(&cur, &hd_cur) == 0)
                        dst = (uint8_t *)kvlangXvalueBody(&cur, &hd_cur, &oldb);
                    if (dst && oldb == blen) {
                        memcpy(dst, src, (size_t)blen);
                        kvlangXvalueFree(&cur);
                        return 0;
                    }
                }
                kvlangXvalueFree(&cur);
            }
        }
    }
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
    int rc;
    if (n == 1) {
        const char *key = pairs[0].key;
        uint32_t len = pairs[0].val.len;
        rc = kvspaceSet(k->h, &key, pairs[0].val.data, &len, 1, err, err_cap);
    } else {
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
            if (pairs[i].val.len)
                memcpy(vals + off, pairs[i].val.data, pairs[i].val.len);
            off += pairs[i].val.len;
        }
        rc = kvspaceSet(k->h, keys, vals, lens, (uint32_t)n, err, err_cap);
        free(keys);
        free(lens);
        free(vals);
    }
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
    int rc;
    if (!src || !dst)
        return -1;
    if (kvspaceCp)
        rc = kvspaceCp(k->h, src, dst, err, err_cap);
    else {
        kvlangXvalue_t v;
        kvlangXvalueZero(&v);
        if (kvlangKvGetOne(k, src, &v) != 0)
            return -1;
        kvlangKvPair_t p = { (char *)dst, v };
        rc = kvlangKvSet(k, &p, 1, err, err_cap);
        kvlangXvalueFree(&v);
        return rc;
    }
    if (rc == 0 && ref_ok(k)) {
        kvspaceRef_t r;
        if (kvspaceResolveRef(k->h, dst, &r) == 0)
            ref_put(k, dst, &r);
    }
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
