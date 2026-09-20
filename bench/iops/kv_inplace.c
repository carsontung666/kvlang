/* IOPS: 64-byte head, WriteInPlace into body at +64. Same a=a+1 loop. */
#define _GNU_SOURCE
#include "kvspace_shm.h"
#include <inttypes.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#define DATA_SIZE (8UL * 64 * 64 * 64)

static uint64_t now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

int main(void) {
    int64_t n = 1000000;
    const char *ns = getenv("IOPS_N");
    if (ns && ns[0])
        n = strtoll(ns, NULL, 10);

    char dir[] = "/tmp/kvlang-iops-inplace-XXXXXX";
    if (!mkdtemp(dir))
        return 1;
    char path[256];
    snprintf(path, sizeof path, "%s/kv", dir);
    kvspace_t *kv = kvspaceShmOpen(path, DATA_SIZE);
    if (!kv) {
        fprintf(stderr, "kvspaceShmOpen failed\n");
        return 1;
    }

    int64_t z = 0;
    uint8_t *box = NULL;
    int32_t blen = kvspaceXvalueNewInt64(&z, 1, &box);
    if (blen < KVSPACE_HEADLEN + 8 || !box) {
        fprintf(stderr, "NewInt64 failed\n");
        return 1;
    }
    if (kvspaceShmSet(kv, "/a", box, blen) != 0) {
        fprintf(stderr, "seed set failed\n");
        return 1;
    }
    free(box);

    uint64_t t0 = now_ns();
    int64_t a = 0;
    for (;;) {
        uint8_t *body = NULL;
        if (kvspaceShmWriteInPlace(kv, "/a", 0, 8, &body) != 0 || !body) {
            fprintf(stderr, "WriteInPlace failed\n");
            return 1;
        }
        a = kvspaceXvalueRawInt64(body);
        if (a >= n)
            break;
        a += 1;
        memcpy(body, &a, 8);
    }
    uint64_t elapsed = now_ns() - t0;
    kvspaceShmClose(kv);
    if (a != n) {
        fprintf(stderr, "inplace: a=%" PRId64 " want %" PRId64 "\n", a, n);
        return 1;
    }
    printf("kvspace-c-inplace n=%" PRId64 " ns=%" PRIu64 " ns/iter=%.3f a=%" PRId64 "\n",
           n, elapsed, (double)elapsed / (double)n, a);
    return 0;
}
