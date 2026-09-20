/* Frontend v0.2.5 dlopen-requires kvspaceCp; installed durable does not export it.
 * This shim loads libkvspace_durable.impl.so and adds Cp as Get+Set. */
#define _GNU_SOURCE
#include "kvspace/kvspace.h"
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void *impl;

static void load_impl(void) {
    if (impl)
        return;
    Dl_info info;
    if (dladdr((void *)load_impl, &info) && info.dli_fname) {
        char path[1024];
        snprintf(path, sizeof path, "%s", info.dli_fname);
        char *slash = strrchr(path, '/');
        if (slash)
            snprintf(slash + 1, sizeof path - (size_t)(slash + 1 - path),
                     "libkvspace_durable.impl.so");
        impl = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    }
    if (!impl)
        impl = dlopen("libkvspace_durable.impl.so", RTLD_NOW | RTLD_LOCAL);
    if (!impl)
        fprintf(stderr, "durable_cp_shim: %s\n", dlerror());
}

static void *sym(const char *n) {
    load_impl();
    return impl ? dlsym(impl, n) : NULL;
}

void *kvspaceConnect(const char *dsn) {
    void *(*fn)(const char *) = sym("kvspaceConnect");
    return fn ? fn(dsn) : NULL;
}
void kvspaceClose(void *h) {
    void (*fn)(void *) = sym("kvspaceClose");
    if (fn)
        fn(h);
}
void kvspaceBytesFree(uint8_t *p, uint32_t len) {
    void (*fn)(uint8_t *, uint32_t) = sym("kvspaceBytesFree");
    if (fn)
        fn(p, len);
}
int kvspaceDisconnect(void *h, char *err, uint32_t err_cap) {
    int (*fn)(void *, char *, uint32_t) = sym("kvspaceDisconnect");
    return fn ? fn(h, err, err_cap) : 1;
}
int kvspaceSet(void *h, const char *const *keys, const uint8_t *vals,
               const uint32_t *lens, uint32_t n, char *err, uint32_t err_cap) {
    int (*fn)(void *, const char *const *, const uint8_t *, const uint32_t *,
              uint32_t, char *, uint32_t) = sym("kvspaceSet");
    return fn ? fn(h, keys, vals, lens, n, err, err_cap) : 1;
}
int kvspaceGet(void *h, const char *key, uint8_t **out, uint32_t *out_len) {
    int (*fn)(void *, const char *, uint8_t **, uint32_t *) = sym("kvspaceGet");
    return fn ? fn(h, key, out, out_len) : 1;
}
int kvspaceGetBatch(void *h, const char *prefix, const char *const *names,
                    uint32_t nnames, uint8_t **out, uint32_t *out_len) {
    int (*fn)(void *, const char *, const char *const *, uint32_t, uint8_t **,
              uint32_t *) = sym("kvspaceGetBatch");
    return fn ? fn(h, prefix, names, nnames, out, out_len) : 1;
}
int kvspaceList(void *h, const char *prefix, int expand_ext, int resolve,
                uint8_t **out, uint32_t *out_len) {
    int (*fn)(void *, const char *, int, int, uint8_t **, uint32_t *) =
        sym("kvspaceList");
    return fn ? fn(h, prefix, expand_ext, resolve, out, out_len) : 1;
}
int kvspaceDel(void *h, const char *const *keys, uint32_t nkeys, char *err,
               uint32_t err_cap) {
    int (*fn)(void *, const char *const *, uint32_t, char *, uint32_t) =
        sym("kvspaceDel");
    return fn ? fn(h, keys, nkeys, err, err_cap) : 1;
}
int kvspaceDelTree(void *h, const char *prefix, char *err, uint32_t err_cap) {
    int (*fn)(void *, const char *, char *, uint32_t) = sym("kvspaceDelTree");
    return fn ? fn(h, prefix, err, err_cap) : 1;
}
int kvspaceMkindex(void *h, const char *path, char *err, uint32_t err_cap) {
    int (*fn)(void *, const char *, char *, uint32_t) = sym("kvspaceMkindex");
    return fn ? fn(h, path, err, err_cap) : 1;
}
int kvspaceMkindexExt(void *h, const char *path, const char *ext_path, char *err,
                      uint32_t err_cap) {
    int (*fn)(void *, const char *, const char *, char *, uint32_t) =
        sym("kvspaceMkindexExt");
    return fn ? fn(h, path, ext_path, err, err_cap) : 1;
}
int kvspaceRmindexExt(void *h, const char *path, char *err, uint32_t err_cap) {
    int (*fn)(void *, const char *, char *, uint32_t) = sym("kvspaceRmindexExt");
    return fn ? fn(h, path, err, err_cap) : 1;
}
int kvspaceClear(void *h, char *err, uint32_t err_cap) {
    int (*fn)(void *, char *, uint32_t) = sym("kvspaceClear");
    return fn ? fn(h, err, err_cap) : 1;
}
int kvspaceWatch(void *h, const char *key, const uint8_t *target,
                 uint32_t target_len, uint64_t tick_ns, uint8_t **out,
                 uint32_t *out_len) {
    int (*fn)(void *, const char *, const uint8_t *, uint32_t, uint64_t,
              uint8_t **, uint32_t *) = sym("kvspaceWatch");
    return fn ? fn(h, key, target, target_len, tick_ns, out, out_len) : 1;
}
int kvspaceTlvEncode(const char *kind, const uint8_t *raw, uint32_t raw_len,
                     const int32_t *dims, int32_t ndim, uint8_t **out,
                     uint32_t *out_len) {
    int (*fn)(const char *, const uint8_t *, uint32_t, const int32_t *, int32_t,
              uint8_t **, uint32_t *) = sym("kvspaceTlvEncode");
    return fn ? fn(kind, raw, raw_len, dims, ndim, out, out_len) : 1;
}
int kvspaceTlvEncodePtr(const char *kind, const uint8_t *raw, uint32_t raw_len,
                        const int32_t *dims, int32_t ndim, uint8_t **out,
                        uint32_t *out_len) {
    int (*fn)(const char *, const uint8_t *, uint32_t, const int32_t *, int32_t,
              uint8_t **, uint32_t *) = sym("kvspaceTlvEncodePtr");
    return fn ? fn(kind, raw, raw_len, dims, ndim, out, out_len) : 1;
}
int kvspaceTlvEncodeMode(const char *kind, const uint8_t *raw, uint32_t raw_len,
                         const int32_t *dims, int32_t ndim, int32_t ref,
                         uint8_t ro, uint32_t vid, uint8_t **out,
                         uint32_t *out_len) {
    int (*fn)(const char *, const uint8_t *, uint32_t, const int32_t *, int32_t,
              int32_t, uint8_t, uint32_t, uint8_t **, uint32_t *) =
        sym("kvspaceTlvEncodeMode");
    return fn ? fn(kind, raw, raw_len, dims, ndim, ref, ro, vid, out, out_len)
              : 1;
}
int kvspaceDecodeHead(const uint8_t *data, uint32_t data_len, kvspaceHead_t *out) {
    int (*fn)(const uint8_t *, uint32_t, kvspaceHead_t *) = sym("kvspaceDecodeHead");
    return fn ? fn(data, data_len, out) : 1;
}
int kvspaceNewPtr(const char *kind, const char *target, int32_t array_len,
                  uint8_t **out, uint32_t *out_len) {
    int (*fn)(const char *, const char *, int32_t, uint8_t **, uint32_t *) =
        sym("kvspaceNewPtr");
    return fn ? fn(kind, target, array_len, out, out_len) : 1;
}
int kvspaceNewChar(const uint8_t *bytes, uint32_t len, uint8_t **out,
                   uint32_t *out_len) {
    int (*fn)(const uint8_t *, uint32_t, uint8_t **, uint32_t *) =
        sym("kvspaceNewChar");
    return fn ? fn(bytes, len, out, out_len) : 1;
}
int kvspaceNewBool(uint8_t v, uint8_t **out, uint32_t *out_len) {
    int (*fn)(uint8_t, uint8_t **, uint32_t *) = sym("kvspaceNewBool");
    return fn ? fn(v, out, out_len) : 1;
}
int kvspaceNewInt64(int64_t v, uint8_t **out, uint32_t *out_len) {
    int (*fn)(int64_t, uint8_t **, uint32_t *) = sym("kvspaceNewInt64");
    return fn ? fn(v, out, out_len) : 1;
}
int kvspaceNewFloat64(double v, uint8_t **out, uint32_t *out_len) {
    int (*fn)(double, uint8_t **, uint32_t *) = sym("kvspaceNewFloat64");
    return fn ? fn(v, out, out_len) : 1;
}

int kvspaceCp(void *h, const char *src, const char *dst, char *err,
              uint32_t err_cap) {
    int (*get)(void *, const char *, uint8_t **, uint32_t *) = sym("kvspaceGet");
    int (*set)(void *, const char *const *, const uint8_t *, const uint32_t *,
               uint32_t, char *, uint32_t) = sym("kvspaceSet");
    void (*bfree)(uint8_t *, uint32_t) = (void (*)(uint8_t *, uint32_t))sym("kvspaceBytesFree");
    if (!get || !set || !bfree)
        return 1;
    uint8_t *d = NULL;
    uint32_t len = 0;
    if (get(h, src, &d, &len) != 0)
        return 1;
    int rc = set(h, &dst, d, &len, 1, err, err_cap);
    bfree(d, len);
    return rc;
}

int kvspaceCpTree(void *h, const char *src, const char *dst, char *err,
                  uint32_t err_cap) {
    if (kvspaceCp(h, src, dst, err, err_cap) != 0)
        return 1;
    int (*lst)(void *, const char *, int, int, uint8_t **, uint32_t *) =
        sym("kvspaceList");
    void (*bfree)(uint8_t *, uint32_t) = (void (*)(uint8_t *, uint32_t))sym("kvspaceBytesFree");
    if (!lst || !bfree)
        return 0;
    uint8_t *listed = NULL;
    uint32_t llen = 0;
    char pfx[2048];
    snprintf(pfx, sizeof pfx, "%s%s", src, src[strlen(src) - 1] == '/' ? "" : "/");
    if (lst(h, pfx, 1, 1, &listed, &llen) != 0)
        return 0;
    if (!listed || llen == 0)
        return 0;
    char *buf = malloc((size_t)llen + 1);
    memcpy(buf, listed, llen);
    buf[llen] = 0;
    bfree(listed, llen);
    char *save = NULL;
    for (char *tok = strtok_r(buf, "\n", &save); tok; tok = strtok_r(NULL, "\n", &save)) {
        char cs[2048], cd[2048];
        snprintf(cs, sizeof cs, "%s%s", pfx, tok);
        snprintf(cd, sizeof cd, "%s%s%s", dst,
                 dst[strlen(dst) - 1] == '/' ? "" : "/", tok);
        if (kvspaceCpTree(h, cs, cd, err, err_cap) != 0) {
            free(buf);
            return 1;
        }
    }
    free(buf);
    return 0;
}
