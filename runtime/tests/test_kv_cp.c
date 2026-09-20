/* Drive shipped kvlangKvGet/Set/Cp: cp independence + borrowed Get view. */
#include "runtime_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int failures;

#define CHECK(cond, ...)                                                       \
    do {                                                                       \
        if (!(cond)) {                                                         \
            failures++;                                                        \
            fprintf(stderr, "FAIL %s:%d: ", __FILE__, __LINE__);               \
            fprintf(stderr, __VA_ARGS__);                                      \
            fprintf(stderr, "\n");                                             \
        }                                                                      \
    } while (0)

static int set_i64(kvlangKv_t *kv, const char *key, int64_t n) {
    kvlangXvalue_t v;
    kvlangXvalueZero(&v);
    kvlangXvalueNewInt64(&v, n);
    kvlangKvPair_t p = {(char *)key, v};
    char err[128] = {0};
    int rc = kvlangKvSet(kv, &p, 1, err, sizeof err);
    CHECK(rc == 0, "set %s=%lld (%s)", key, (long long)n, err);
    kvlangXvalueFree(&v);
    return rc;
}

static int64_t get_i64(kvlangKv_t *kv, const char *key) {
    kvlangXvalue_t v;
    kvlangXvalueZero(&v);
    if (kvlangKvGetOne(kv, key, &v) != 0 || kvlangXvalueNone(&v))
        return -999999;
    int64_t n = kvlangXvalueAsInt64(&v);
    kvlangXvalueFree(&v);
    return n;
}

int main(void) {
    if (!kvspaceCp) {
        fprintf(stderr, "FAIL: kvspaceCp symbol missing (not a get+set fallback)\n");
        return 1;
    }

    const char *td = getenv("TMPDIR");
    if (!td || !td[0])
        td = "/tmp";
    char dir[512];
    snprintf(dir, sizeof dir, "%s/kvlang-test-330-XXXXXX", td);
    if (!mkdtemp(dir)) {
        perror("mkdtemp");
        return 1;
    }
    char dsn[512];
    snprintf(dsn, sizeof dsn, "shm://%s/kv.shm", dir);
    kvlangKv_t *kv = kvlangKvConnect(dsn);
    CHECK(kv != NULL, "connect %s", dsn);
    if (!kv)
        return 1;

    {
        kvlangXvalue_t z;
        kvlangXvalueZero(&z);
        kvlangXvalueNewInt64(&z, 10);
        CHECK(z.len == (uint32_t)KVLANG_XVALUE_HEADLEN + 8, "int64 box is 64+8, got %u", z.len);
        CHECK(z.data && z.data[1] == 1, "ATOM storetype at byte 1");
        CHECK(kvlangXvalueAsInt64(&z) == 10, "AsInt64 reads body at +64");
        CHECK(z.data[KVLANG_XVALUE_HEADLEN] == 10, "payload first byte");
        kvlangXvalueFree(&z);
        if (failures == 0)
            fprintf(stdout, "ok head64_encode\n");
    }

    CHECK(set_i64(kv, "/src", 10) == 0, "seed /src");
    char err[128] = {0};
    CHECK(kvlangKvCp(kv, "/src", "/dst", err, sizeof err) == 0, "cp /src /dst (%s)",
          err);
    CHECK(get_i64(kv, "/dst") == 10, "dst after cp");
    CHECK(set_i64(kv, "/src", 20) == 0, "overwrite src");
    CHECK(get_i64(kv, "/src") == 20, "src after overwrite");
    CHECK(get_i64(kv, "/dst") == 10, "dst unchanged after src overwrite");
    if (failures == 0)
        fprintf(stdout, "ok cp_independent\n");

    CHECK(set_i64(kv, "/k", 1) == 0, "seed /k");
    kvlangXvalue_t view;
    kvlangXvalueZero(&view);
    CHECK(kvlangKvGetOne(kv, "/k", &view) == 0, "get /k");
    CHECK(view.borrowed, "get view borrowed");
    CHECK(view.data != NULL, "get view data");
    uint8_t *held = view.data;
    uint32_t held_len = view.len;
    CHECK(kvlangXvalueAsInt64(&view) == 1, "view before in-place");
    CHECK(set_i64(kv, "/k", 2) == 0, "same-length overwrite /k");
    CHECK(view.data == held && view.len == held_len, "held view pointer stable");
    CHECK(view.borrowed, "held view still borrowed");
    CHECK(kvlangXvalueAsInt64(&view) == 2, "held view sees in-place body write");
    CHECK(get_i64(kv, "/k") == 2, "get after in-place");
    CHECK(view.len == (uint32_t)KVLANG_XVALUE_HEADLEN + 8, "borrowed box 72");
    kvlangXvalueFree(&view);
    if (failures == 0)
        fprintf(stdout, "ok borrow_inplace\n");

    CHECK(set_i64(kv, "/acc", 0) == 0, "seed /acc");
    for (int i = 0; i < 10000; i++) {
        int64_t cur = get_i64(kv, "/acc");
        if (set_i64(kv, "/acc", cur + 1) != 0)
            break;
    }
    CHECK(get_i64(kv, "/acc") == 10000, "10000 in-place increments via Set");
    if (failures == 0)
        fprintf(stdout, "ok hot_inc_10000\n");

    kvlangKvDisconnect(kv);
    fprintf(stdout, "%d FAIL\n", failures);
    return failures ? 1 : 0;
}
