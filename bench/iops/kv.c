/* #204 floor: shm Get+decode / +1 / NewInt64+Set. Same loop as kvlang a=a+1. */
#include "kvspace/kvspace.h"
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

static int64_t rd64(const uint8_t *p) {
    uint64_t u = (uint64_t)p[0] | ((uint64_t)p[1] << 8) | ((uint64_t)p[2] << 16) |
                 ((uint64_t)p[3] << 24) | ((uint64_t)p[4] << 32) |
                 ((uint64_t)p[5] << 40) | ((uint64_t)p[6] << 48) |
                 ((uint64_t)p[7] << 56);
    return (int64_t)u;
}

static uint64_t now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

int main(void) {
    const char *dsn = getenv("KVSPACE");
    if (!dsn || !dsn[0])
        dsn = "shm:///tmp/kvlang_iops.shm";
    int64_t n = 1000000;
    const char *ns = getenv("IOPS_N");
    if (ns && ns[0])
        n = strtoll(ns, NULL, 10);

    void *h = kvspaceConnect(dsn);
    if (!h) {
        fprintf(stderr, "kvspaceConnect failed: %s\n", dsn);
        return 1;
    }

    uint8_t *tlv = NULL;
    uint32_t tlen = 0;
    if (kvspaceNewInt64(0, &tlv, &tlen) != 0 || !tlv) {
        fprintf(stderr, "NewInt64 0 failed\n");
        return 1;
    }
    const char *key = "/a";
    char err[128] = {0};
    if (kvspaceSet(h, &key, tlv, &tlen, 1, err, sizeof err) != 0) {
        fprintf(stderr, "seed set: %s\n", err);
        return 1;
    }
    kvspaceBytesFree(tlv, tlen);

    uint64_t t0 = now_ns();
    int64_t a = 0;
    for (;;) {
        uint8_t *d = NULL;
        uint32_t len = 0;
        if (kvspaceGet(h, key, &d, &len) != 0 || !d) {
            fprintf(stderr, "get failed\n");
            return 1;
        }
        kvspaceHead_t hd;
        memset(&hd, 0, sizeof hd);
        if (kvspaceDecodeHead(d, len, &hd) != 0 || hd.body_len < 8 ||
            hd.body_offset + 8 > (int32_t)len) {
            fprintf(stderr, "decode failed len=%u\n", len);
            kvspaceBytesFree(d, len);
            return 1;
        }
        a = rd64(d + hd.body_offset);
        kvspaceBytesFree(d, len);
        if (a >= n)
            break;
        a += 1;
        tlv = NULL;
        tlen = 0;
        if (kvspaceNewInt64(a, &tlv, &tlen) != 0) {
            fprintf(stderr, "NewInt64 failed\n");
            return 1;
        }
        if (kvspaceSet(h, &key, tlv, &tlen, 1, err, sizeof err) != 0) {
            fprintf(stderr, "set: %s\n", err);
            kvspaceBytesFree(tlv, tlen);
            return 1;
        }
        kvspaceBytesFree(tlv, tlen);
    }
    uint64_t elapsed = now_ns() - t0;
    kvspaceClose(h);
    if (a != n) {
        fprintf(stderr, "kv: a=%" PRId64 " want %" PRId64 "\n", a, n);
        return 1;
    }
    printf("kvspace-c n=%" PRId64 " ns=%" PRIu64 " ns/iter=%.3f a=%" PRId64 "\n",
           n, elapsed, (double)elapsed / (double)n, a);
    return 0;
}
