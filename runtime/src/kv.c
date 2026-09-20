#include "runtime_internal.h"
#include <dlfcn.h>

/* kvspace* C ABI. */

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

static void ref_drop(kvlangKv_t *k, const char *key) {
    if (!k || !key)
        return;
    int w = 0;
    for (int i = 0; i < k->nref; i++) {
        if (k->ref[i].key && strcmp(k->ref[i].key, key) == 0) {
            free(k->ref[i].key);
            continue;
        }
        if (w != i)
            k->ref[w] = k->ref[i];
        w++;
    }
    k->nref = w;
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
typedef int (*kvlang_shm_resolve_fn)(void *, const char *, uint32_t *, uint32_t *);
typedef uint8_t *(*kvlang_shm_get_by_ref_fn)(void *, uint32_t *, uint32_t *,
                                            const char *, int32_t *);
typedef int (*kvlang_shm_set_part_fn)(void *, uint32_t *, uint32_t *, const char *,
                                     uint32_t, const uint8_t *, uint32_t);
typedef int (*kvlang_shm_set_fn)(void *, const char *, const uint8_t *, int32_t);
typedef int (*kvlang_shm_cptree_fn)(void *, const char *, const char *);

static kvlang_shm_get_fn shm_get_fn;
static kvlang_shm_resolve_fn shm_resolve_fn;
static kvlang_shm_get_by_ref_fn shm_get_by_ref_fn;
static kvlang_shm_set_part_fn shm_set_part_fn;
static kvlang_shm_set_fn shm_set_fn;
static kvlang_shm_cptree_fn shm_cptree_fn;
static int shm_bind_tried;

static void shm_bind(void) {
    if (shm_bind_tried)
        return;
    shm_bind_tried = 1;
    void *so = dlopen("libkvspace-c.so.1", RTLD_NOLOAD | RTLD_LAZY);
    if (!so)
        so = dlopen("libkvspace-c.so", RTLD_NOLOAD | RTLD_LAZY);
    if (!so)
        return;
    shm_get_fn = (kvlang_shm_get_fn)dlsym(so, "kvspaceShmGet");
    shm_resolve_fn = (kvlang_shm_resolve_fn)dlsym(so, "kvspaceShmResolveRef");
    shm_get_by_ref_fn = (kvlang_shm_get_by_ref_fn)dlsym(so, "kvspaceShmGetByRef");
    shm_set_part_fn = (kvlang_shm_set_part_fn)dlsym(so, "kvspaceShmSetPartByRef");
    shm_set_fn = (kvlang_shm_set_fn)dlsym(so, "kvspaceShmSet");
    shm_cptree_fn = (kvlang_shm_cptree_fn)dlsym(so, "kvspaceShmCptree");
}

static void *shm_backend(void *h) {
    return h ? *(void **)((char *)h + 16) : NULL;
}

static void ref_cache_key(kvlangKv_t *k, const char *key) {
    if (!k || !k->ref_on || !key || !key[0] || ref_find(k, key))
        return;
    shm_bind();
    void *bkv = shm_backend(k->h);
    if (bkv && shm_resolve_fn) {
        uint32_t bid = 0, gen = 0;
        if (shm_resolve_fn(bkv, key, &bid, &gen) == 0) {
            kvspaceRef_t r = { bid, gen };
            ref_put(k, key, &r);
        }
        return;
    }
    if (kvspaceResolveRef) {
        kvspaceRef_t r;
        if (kvspaceResolveRef(k->h, key, &r) == 0)
            ref_put(k, key, &r);
    }
}

static void take_owned(uint8_t *d, uint32_t len, kvlangXvalue_t *out) {
    kvlangXvalueCopyMalloc(out, d, len);
    if (d)
        kvspaceBytesFree(d, len);
}

static int take_shm_view(kvlangKv_t *k, const char *key, kvlangXvalue_t *out) {
    if (!k || !key)
        return 0;
    shm_bind();
    void *bkv = shm_backend(k->h);
    if (!bkv)
        return 0;
    int32_t n = 0;
    uint8_t *d = NULL;
    kvlangRefEnt_t *e = ref_find(k, key);
    if (e && shm_get_by_ref_fn) {
        uint32_t bid = e->block_id, gen = e->gen;
        d = shm_get_by_ref_fn(bkv, &bid, &gen, key, &n);
        if (d && n > 0) {
            e->block_id = bid;
            e->gen = gen;
        } else {
            d = NULL;
            n = 0;
            ref_drop(k, key);
        }
    }
    if (!d && shm_get_fn) {
        d = shm_get_fn(bkv, key, 0, &n);
        if (!d || n <= 0) {
            d = NULL;
            n = 0;
        } else {
            ref_cache_key(k, key);
        }
    }
    if (!d)
        return 0;
    k->borrow_get = 1;
    out->data = d;
    out->len = (uint32_t)n;
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
    if (ref_ok(k) && key) {
        kvlangRefEnt_t *e = ref_find(k, key);
        if (e) {
            kvspaceRef_t r = { e->block_id, e->gen };
            uint8_t *d;
            uint32_t len;
            if (kvspaceGetByRef(k->h, &r, key, &d, &len) == 0 && d && len) {
                e->block_id = r.block_id;
                e->gen = r.gen;
                take_owned(d, len, out);
                return 0;
            }
            ref_drop(k, key);
        }
    }
    uint8_t *d = NULL;
    uint32_t len = 0;
    if (kvspaceGet(k->h, key, &d, &len) != 0) return -1;
    take_owned(d, len, out);
    ref_cache_key(k, key);
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
    if (key[0] && take_shm_view(k, key, out))
        return 0;
    if (ref_ok(k) && key[0]) {
        kvlangRefEnt_t *e = ref_find(k, key);
        if (e) {
            kvspaceRef_t r = { e->block_id, e->gen };
            uint8_t *d; uint32_t len;
            if (kvspaceGetByRef(k->h, &r, key, &d, &len) == 0 && d && len) {
                e->block_id = r.block_id;
                e->gen = r.gen;
                take_owned(d, len, out);
                return 0;
            }
            ref_drop(k, key);
        }
    }
    if (key[0]) {
        uint8_t *d;
        uint32_t len;
        if (kvspaceGet(k->h, key, &d, &len) == 0 && d && len) {
            take_owned(d, len, out);
            ref_cache_key(k, key);
            return 0;
        }
    }
    char *nm = (char *)name;
    int rc = kvlangKvGetBatch(k, dir, &nm, 1, out);
    if (rc == 0 && !kvlangXvalueNone(out) && key[0])
        ref_cache_key(k, key);
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

static int key_has_own_node(kvlangKv_t *k, const char *key) {
    if (!k || !key || !key[0])
        return 0;
    if (ref_find(k, key))
        return 1;
    ref_cache_key(k, key);
    return ref_find(k, key) != NULL;
}

int kvlangKvSet(kvlangKv_t *k, const kvlangKvPair_t *pairs, int n, char *err, uint32_t err_cap) {
    if (n <= 0) return 0;
    if (n == 1 && pairs[0].key && (pairs[0].val.data == NULL || pairs[0].val.len == 0))
        return kvlangKvDel(k, pairs[0].key, err, err_cap);
    if (n == 1 && pairs[0].key && pairs[0].val.data) {
        const char *key = pairs[0].key;
        kvspaceHead_t hd;
        int32_t blen = 0;
        const uint8_t *src = NULL;
        if (kvlangXvalueHead(&pairs[0].val, &hd) == 0)
            src = kvlangXvalueBody(&pairs[0].val, &hd, &blen);
        shm_bind();
        void *bkv = shm_backend(k->h);
        kvlangRefEnt_t *e = ref_find(k, key);
        if (!e && key_has_own_node(k, key))
            e = ref_find(k, key);
        if (e && bkv && shm_set_part_fn) {
            uint32_t bid = e->block_id, gen = e->gen;
            if (shm_set_part_fn(bkv, &bid, &gen, key, 0, pairs[0].val.data,
                                pairs[0].val.len) == 0) {
                e->block_id = bid;
                e->gen = gen;
                return 0;
            }
            ref_drop(k, key);
            e = NULL;
        }
        if (e && kvspaceSetPartByRef) {
            kvspaceRef_t r = { e->block_id, e->gen };
            if (kvspaceSetPartByRef(k->h, &r, key, 0, pairs[0].val.data,
                                    pairs[0].val.len, err, err_cap) == 0) {
                e->block_id = r.block_id;
                e->gen = r.gen;
                return 0;
            }
            ref_drop(k, key);
            e = NULL;
        }
        if (k->borrow_get > 0 && e) {
            kvlangXvalue_t cur;
            kvlangXvalueZero(&cur);
            if (kvlangKvGetOne(k, key, &cur) == 0 && cur.borrowed && cur.data) {
                kvspaceHead_t hd_cur;
                int32_t oldb = 0;
                uint8_t *dst = NULL;
                if (kvlangXvalueHead(&cur, &hd_cur) == 0)
                    dst = (uint8_t *)kvlangXvalueBody(&cur, &hd_cur, &oldb);
                if (dst && src && oldb == blen) {
                    memcpy(dst, src, (size_t)blen);
                    kvlangXvalueFree(&cur);
                    return 0;
                }
            }
            kvlangXvalueFree(&cur);
        }
    }
    int rc;
    if (n == 1) {
        const char *key = pairs[0].key;
        uint32_t len = pairs[0].val.len;
        shm_bind();
        void *bkv = shm_backend(k->h);
        if (bkv && shm_set_fn && key && pairs[0].val.data &&
            shm_set_fn(bkv, key, pairs[0].val.data, (int32_t)len) == 0)
            rc = 0;
        else
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
    if (rc == 0 && n == 1 && pairs[0].key)
        ref_cache_key(k, pairs[0].key);
    return rc;
}

int kvlangKvDel(kvlangKv_t *k, const char *key, char *err, uint32_t err_cap) {
    ref_drop(k, key);
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
    if (rc == 0)
        ref_cache_key(k, dst);
    return rc;
}

int kvlangKvCpTree(kvlangKv_t *k, const char *src, const char *dst, char *err, uint32_t err_cap) {
    if (!src || !dst)
        return -1;
    shm_bind();
    void *bkv = shm_backend(k->h);
    if (bkv && shm_cptree_fn) {
        int rc = shm_cptree_fn(bkv, src, dst);
        if (rc == 0)
            kvlangKvInvalidateFrame(k, dst);
        return rc;
    }
    if (kvspaceCpTree) {
        int rc = kvspaceCpTree(k->h, src, dst, err, err_cap);
        if (rc == 0)
            kvlangKvInvalidateFrame(k, dst);
        return rc;
    }
    return kvlangKvCp(k, src, dst, err, err_cap);
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
