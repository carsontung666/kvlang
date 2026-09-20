#include "runtime_internal.h"

/* 64-byte head, body at +64. Legacy TLV still decodes. Borrowed shm Get: do not free. */

static uint32_t rd32(const uint8_t *p) { return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24); }
static uint16_t rd16(const uint8_t *p) { return (uint16_t)(p[0] | (p[1] << 8)); }
static uint64_t rd64(const uint8_t *p) { return (uint64_t)rd32(p) | ((uint64_t)rd32(p + 4) << 32); }
static void wr16(uint8_t *d, uint16_t v) { d[0] = (uint8_t)v; d[1] = (uint8_t)(v >> 8); }
static void wr32(uint8_t *d, uint32_t v) {
    d[0] = (uint8_t)v; d[1] = (uint8_t)(v >> 8); d[2] = (uint8_t)(v >> 16); d[3] = (uint8_t)(v >> 24);
}
static void wr64(uint8_t *d, uint64_t v) { wr32(d, (uint32_t)v); wr32(d + 4, (uint32_t)(v >> 32)); }

#define H64_STORETYPE 1
#define H64_NDIM      3
#define H64_VID       4
#define H64_BODYLEN   8
#define H64_BODYCAP   16
#define H64_KIND      24
#define H64_DIMS      28
#define H64_ST_NONE   0
#define H64_ST_ATOM   1
#define H64_ST_ARRAY  2
#define H64_ST_INDEX  3
#define H64_ST_EXT    4

static const char *const h64_kind_names[] = {
    "", KVSPACE_KIND_BOOL,
    KVSPACE_KIND_INT8, KVSPACE_KIND_INT16, KVSPACE_KIND_INT32, KVSPACE_KIND_INT64,
    KVSPACE_KIND_UINT8, KVSPACE_KIND_UINT16, KVSPACE_KIND_UINT32, KVSPACE_KIND_UINT64,
    KVSPACE_KIND_FLOAT32, KVSPACE_KIND_FLOAT64,
    KVSPACE_KIND_CHAR, KVSPACE_KIND_CHAR_UTF8, KVSPACE_KIND_CHAR_ASCII,
    KVSPACE_KIND_OBJ, KVSPACE_KIND_MAP, KVSPACE_KIND_INDEX, KVSPACE_KIND_EXT_INDEX,
    KVSPACE_KIND_RWIR, KVSPACE_KIND_RWFUNC, KVSPACE_KIND_DEF_RWIR, KVSPACE_KIND_SCOPE,
    KVSPACE_KIND_TIME, KVSPACE_KIND_DURATION, KVSPACE_KIND_DEF_RWFUNC,
};

static const char *h64_kind_name(uint16_t id) {
    if (id < sizeof(h64_kind_names) / sizeof(h64_kind_names[0]))
        return h64_kind_names[id];
    return "*";
}

static uint16_t h64_kind_id(const char *name) {
    if (!name || !name[0] || strcmp(name, KVSPACE_KIND_NONE) == 0)
        return 0;
    for (uint16_t i = 1; i < sizeof(h64_kind_names) / sizeof(h64_kind_names[0]); i++)
        if (strcmp(name, h64_kind_names[i]) == 0)
            return i;
    return 255;
}

static uint8_t h64_storetype(const char *kind, int32_t ndim) {
    if (!kind || !kind[0] || strcmp(kind, KVSPACE_KIND_NONE) == 0)
        return H64_ST_NONE;
    if (strcmp(kind, KVSPACE_KIND_EXT_INDEX) == 0)
        return H64_ST_EXT;
    if (strcmp(kind, KVSPACE_KIND_INDEX) == 0 || strcmp(kind, KVSPACE_KIND_RWFUNC) == 0 ||
        strcmp(kind, KVSPACE_KIND_DEF_RWIR) == 0 || strcmp(kind, KVSPACE_KIND_DEF_RWFUNC) == 0 ||
        kind[0] == '/')
        return H64_ST_INDEX;
    if (ndim > 0)
        return H64_ST_ARRAY;
    return H64_ST_ATOM;
}

static int looks_head64(const uint8_t *d, uint32_t len) {
    if (!d || len < KVLANG_XVALUE_HEADLEN)
        return 0;
    if (d[H64_STORETYPE] > H64_ST_EXT || d[H64_NDIM] > X_MAX_NDIM)
        return 0;
    uint64_t bl = rd64(d + H64_BODYLEN), cap = rd64(d + H64_BODYCAP);
    if (bl > cap || bl > 16ull * 1024ull * 1024ull)
        return 0;
    if ((uint64_t)KVLANG_XVALUE_HEADLEN + bl > len)
        return 0;
    return 1;
}

static int decode_head64(const uint8_t *d, uint32_t len, kvspaceHead_t *h) {
    memset(h, 0, sizeof(*h));
    if (!looks_head64(d, len))
        return -1;
    int8_t ref = (int8_t)d[0];
    uint8_t st = d[H64_STORETYPE];
    uint8_t ndim = d[H64_NDIM];
    uint16_t kid = rd16(d + H64_KIND);
    const char *kn = h64_kind_name(kid);
    int o = 0;
    if (ref == 1)
        h->kindexpr[o++] = '*';
    else if (ref == 2 || ref == -1)
        h->kindexpr[o++] = '@';
    if (st == H64_ST_ARRAY && ndim > 0) {
        h->kindexpr[o++] = '[';
        for (int i = 0; i < ndim && o < 200; i++) {
            if (i)
                h->kindexpr[o++] = ',';
            o += snprintf((char *)h->kindexpr + o, sizeof h->kindexpr - (size_t)o, "%u",
                          rd32(d + H64_DIMS + i * 4));
        }
        h->kindexpr[o++] = ']';
    }
    size_t kl = strlen(kn);
    if (o + (int)kl >= (int)sizeof h->kindexpr)
        kl = sizeof h->kindexpr - (size_t)o - 1;
    memcpy(h->kindexpr + o, kn, kl);
    h->kindexpr[o + (int)kl] = 0;
    h->ro = d[2] & 1;
    h->vid = rd32(d + H64_VID);
    h->body_len = (int32_t)rd64(d + H64_BODYLEN);
    h->body_offset = KVLANG_XVALUE_HEADLEN;
    return h->kindexpr[0] ? 0 : (kid == 0 ? 0 : -1);
}

int kvlangXvalueEncodeBox(const char *kind, const uint8_t *raw, uint32_t raw_len,
                          const int32_t *dims, int32_t ndim, uint8_t **out, uint32_t *out_len) {
    if (!out || !out_len)
        return -1;
    if (ndim < 0)
        ndim = 0;
    if (ndim > X_MAX_NDIM)
        return -1;
    if (!kind)
        kind = "";
    uint32_t total = (uint32_t)KVLANG_XVALUE_HEADLEN + raw_len;
    uint8_t *buf = malloc(total ? total : 1);
    if (!buf)
        return -1;
    memset(buf, 0, KVLANG_XVALUE_HEADLEN);
    buf[0] = 0;
    buf[H64_STORETYPE] = h64_storetype(kind, ndim);
    buf[H64_NDIM] = (uint8_t)ndim;
    wr64(buf + H64_BODYLEN, raw_len);
    wr64(buf + H64_BODYCAP, raw_len);
    wr16(buf + H64_KIND, h64_kind_id(kind));
    for (int i = 0; i < ndim; i++)
        wr32(buf + H64_DIMS + i * 4, (uint32_t)(dims ? dims[i] : 0));
    if (raw_len && raw)
        memcpy(buf + KVLANG_XVALUE_HEADLEN, raw, raw_len);
    *out = buf;
    *out_len = total;
    return 0;
}

void kvlangXvalueFree(kvlangXvalue_t *v) {
    if (v->data && !v->borrowed)
        free(v->data);
    v->data = NULL;
    v->len = 0;
    v->borrowed = 0;
}

void kvlangXvalueSetBytes(kvlangXvalue_t *v, uint8_t *data, uint32_t len) {
    v->data = data;
    v->len = len;
    v->borrowed = 0;
}

void kvlang_kindexpr_parse(const uint8_t *kx, kvlang_kindexpr_t *out) {
    memset(out, 0, sizeof(*out));
    if (!kx) return;
    int32_t i = 0;
    if (kx[0] == '*') { out->ref = 1; i = 1; }
    else if (kx[0] == '@') { out->ref = 2; i = 1; }
    if (kx[i] == '[') {
        i++;
        while (kx[i] != ']' && kx[i] != 0 && out->ndim < X_MAX_NDIM) {
            int32_t d = 0;
            while (kx[i] >= '0' && kx[i] <= '9') { d = d * 10 + (kx[i] - '0'); i++; }
            out->dims[out->ndim++] = d;
            if (kx[i] == ',') i++;
        }
        if (kx[i] == ']') i++;
    }
    out->kind = (const char *)(kx + i);
    out->kind_len = (int32_t)strlen((const char *)(kx + i));
    out->array_len = 1;
    for (int d = 0; d < out->ndim; d++) out->array_len *= out->dims[d];
}

static int kvlangXvalueDecodeHeadRaw(const uint8_t *d, uint32_t len, kvspaceHead_t *h) {
    memset(h, 0, sizeof(*h));
    if (!d || len == 0) return -1;
    if (looks_head64(d, len))
        return decode_head64(d, len, h);
    kvspaceDecodeHead(d, len, h);
    return h->kindexpr[0] ? 0 : -1;
}

static int32_t al_to_dims(const char *kind, int32_t array_len, int32_t *dims) {
    if (strncmp(kind, "char/", 5) == 0) { dims[0] = array_len < 0 ? 0 : array_len; return 1; }
    if (array_len > 1) { dims[0] = array_len; return 1; }
    return 0;
}

static uint8_t *kvlangXvalueOwn(uint8_t *tmp, uint32_t tl, uint32_t *out_len) {
    if (!tmp) { *out_len = 0; return NULL; }
    uint8_t *buf = malloc(tl);
    memcpy(buf, tmp, tl);
    kvspaceBytesFree(tmp, tl);
    *out_len = tl;
    return buf;
}

static uint8_t *kvlangXvalueEncodeTlv(const char *kind, const uint8_t *raw,
                              uint32_t raw_len, int32_t array_len, uint32_t *out_len) {
    int32_t dims[1]; int32_t ndim = al_to_dims(kind, array_len, dims);
    uint8_t *buf = NULL;
    if (kvlangXvalueEncodeBox(kind, raw, raw_len, dims, ndim, &buf, out_len) != 0)
        return NULL;
    return buf;
}

int kvlangXvalueHead(const kvlangXvalue_t *v, kvspaceHead_t *h) {
    memset(h, 0, sizeof(*h));
    if (kvlangXvalueNone(v)) return -1;
    return kvlangXvalueDecodeHeadRaw(v->data, v->len, h);
}

const char *kvlangXvalueKind(const kvlangXvalue_t *v) {
    static __thread char buf[16][33];
    static __thread int idx = 0;
    char *b = buf[idx];
    idx = (idx + 1) & 15;
    if (kvlangXvalueNone(v)) { b[0] = 0; return b; }
    if (looks_head64(v->data, v->len)) {
        const char *kn = h64_kind_name(rd16(v->data + H64_KIND));
        size_t kl = strlen(kn);
        if (kl > 32) kl = 32;
        memcpy(b, kn, kl);
        b[kl] = 0;
        return b;
    }
    kvspaceHead_t h;
    if (kvlangXvalueHead(v, &h) < 0) { b[0] = 0; return b; }
    kvlang_kindexpr_t kx; kvlang_kindexpr_parse(h.kindexpr, &kx);
    int32_t kl = kx.kind_len;
    if (kl > 32) kl = 32;
    memcpy(b, kx.kind, (size_t)kl);
    b[kl] = 0;
    return b;
}

bool kvlangXvalueKindIs(const kvlangXvalue_t *v, const char *kind) {
    if (kvlangXvalueNone(v)) return kind[0] == 0;
    kvspaceHead_t h;
    if (kvlangXvalueHead(v, &h) < 0) return false;
    kvlang_kindexpr_t kx; kvlang_kindexpr_parse(h.kindexpr, &kx);
    return (size_t)kx.kind_len == strlen(kind) && memcmp(kx.kind, kind, (size_t)kx.kind_len) == 0;
}

bool kvlangXvalueIsPtr(const kvlangXvalue_t *v) {
    if (kvlangXvalueNone(v)) return false;
    kvspaceHead_t h;
    if (kvlangXvalueHead(v, &h) < 0) return false;
    kvlang_kindexpr_t kx; kvlang_kindexpr_parse(h.kindexpr, &kx);
    return kx.ref == 1;
}

int32_t kvlangXvalueArrayLen(const kvlangXvalue_t *v) {
    if (kvlangXvalueNone(v)) return 0;
    kvspaceHead_t h; kvlangXvalueDecodeHeadRaw(v->data, v->len, &h);
    kvlang_kindexpr_t kx; kvlang_kindexpr_parse(h.kindexpr, &kx);
    return kx.array_len;
}

const uint8_t *kvlangXvalueBody(const kvlangXvalue_t *v, const kvspaceHead_t *h, int32_t *out_len) {
    int32_t off = h->body_offset, len = h->body_len;
    if (off < 0 || len < 0 || off + len > (int32_t)v->len) {
        if (out_len) *out_len = 0;
        return NULL;
    }
    if (out_len) *out_len = len;
    return v->data + off;
}

bool kvlangXvalueIsCharKind(const char *kind) { return strncmp(kind, "char/", 5) == 0; }
bool kvlangXvalueIsIntKind(const char *kind) {
    return strcmp(kind, KVSPACE_KIND_INT8) == 0 || strcmp(kind, KVSPACE_KIND_INT16) == 0 ||
           strcmp(kind, KVSPACE_KIND_INT32) == 0 || strcmp(kind, KVSPACE_KIND_INT64) == 0;
}
bool kvlangXvalueIsUintKind(const char *kind) {
    return strcmp(kind, KVSPACE_KIND_UINT8) == 0 || strcmp(kind, KVSPACE_KIND_UINT16) == 0 ||
           strcmp(kind, KVSPACE_KIND_UINT32) == 0 || strcmp(kind, KVSPACE_KIND_UINT64) == 0;
}
bool kvlangXvalueIsFloatKind(const char *kind) {
    return strcmp(kind, KVSPACE_KIND_FLOAT32) == 0 || strcmp(kind, KVSPACE_KIND_FLOAT64) == 0;
}
bool kvlangXvalueIsNumKind(const char *kind) {
    return kvlangXvalueIsIntKind(kind) || kvlangXvalueIsUintKind(kind) || kvlangXvalueIsFloatKind(kind);
}

int32_t kvlangXvalueElemSize(const char *kind) {
    if (strcmp(kind, KVSPACE_KIND_INT8) == 0 || strcmp(kind, KVSPACE_KIND_UINT8) == 0 ||
        strcmp(kind, KVSPACE_KIND_CHAR_UTF8) == 0 || strcmp(kind, KVSPACE_KIND_CHAR_ASCII) == 0 ||
        strcmp(kind, KVSPACE_KIND_BOOL) == 0) return 1;
    if (strcmp(kind, KVSPACE_KIND_INT16) == 0 || strcmp(kind, KVSPACE_KIND_UINT16) == 0) return 2;
    if (strcmp(kind, KVSPACE_KIND_INT32) == 0 || strcmp(kind, KVSPACE_KIND_UINT32) == 0 ||
        strcmp(kind, KVSPACE_KIND_FLOAT32) == 0 || strcmp(kind, KVSPACE_KIND_CHAR) == 0) return 4;
    if (strcmp(kind, KVSPACE_KIND_INT64) == 0 || strcmp(kind, KVSPACE_KIND_UINT64) == 0 ||
        strcmp(kind, KVSPACE_KIND_FLOAT64) == 0 || strcmp(kind, KVSPACE_KIND_TIME) == 0 ||
        strcmp(kind, KVSPACE_KIND_DURATION) == 0) return 8;
    return 0;
}

static const uint8_t *v_body(const kvlangXvalue_t *v, kvspaceHead_t *h) {
    if (looks_head64(v->data, v->len)) {
        if (h && decode_head64(v->data, v->len, h) < 0) return NULL;
        return v->data + KVLANG_XVALUE_HEADLEN;
    }
    if (kvlangXvalueDecodeHeadRaw(v->data, v->len, h) < 0) return NULL;
    return v->data + h->body_offset;
}

int64_t kvlangXvalueAsInt64(const kvlangXvalue_t *v) {
    if (kvlangXvalueNone(v)) return 0;
    if (looks_head64(v->data, v->len) && v->len >= (uint32_t)KVLANG_XVALUE_HEADLEN + 8) {
        uint16_t id = rd16(v->data + H64_KIND);
        const uint8_t *b = v->data + KVLANG_XVALUE_HEADLEN;
        if (id == 5) return (int64_t)rd64(b); /* int64 */
        if (id == 4) return (int32_t)rd32(b);
        if (id == 3) return (int16_t)rd16(b);
        if (id == 2) return (int8_t)b[0];
        if (id == 9) return (int64_t)rd64(b);
        if (id == 8) return (int64_t)rd32(b);
        if (id == 7) return rd16(b);
        if (id == 6) return b[0];
        if (id == 1) return b[0] != 0;
    }
    kvspaceHead_t h; const uint8_t *b = v_body(v, &h);
    if (!b) return 0;
    const char *k = kvlangXvalueKind(v);
    if (strcmp(k, KVSPACE_KIND_BOOL) == 0) return b[0] != 0;
    if (strcmp(k, KVSPACE_KIND_INT8) == 0) return (int8_t)b[0];
    if (strcmp(k, KVSPACE_KIND_INT16) == 0) return (int16_t)rd16(b);
    if (strcmp(k, KVSPACE_KIND_INT32) == 0) return (int32_t)rd32(b);
    if (strcmp(k, KVSPACE_KIND_INT64) == 0) return (int64_t)rd64(b);
    if (strcmp(k, KVSPACE_KIND_UINT8) == 0) return b[0];
    if (strcmp(k, KVSPACE_KIND_UINT16) == 0) return rd16(b);
    if (strcmp(k, KVSPACE_KIND_UINT32) == 0) return rd32(b);
    if (strcmp(k, KVSPACE_KIND_UINT64) == 0) return (int64_t)rd64(b);
    if (strcmp(k, KVSPACE_KIND_FLOAT32) == 0) { float f; uint32_t u = rd32(b); memcpy(&f, &u, 4); return (int64_t)f; }
    if (strcmp(k, KVSPACE_KIND_FLOAT64) == 0) { double d; uint64_t u = rd64(b); memcpy(&d, &u, 8); return (int64_t)d; }
    if (strcmp(k, KVSPACE_KIND_TIME) == 0 || strcmp(k, KVSPACE_KIND_DURATION) == 0) return (int64_t)rd64(b);
    return 0;
}

double kvlangXvalueAsFloat64(const kvlangXvalue_t *v) {
    if (kvlangXvalueNone(v)) return 0;
    kvspaceHead_t h; const uint8_t *b = v_body(v, &h);
    if (!b) return 0;
    const char *k = kvlangXvalueKind(v);
    if (strcmp(k, KVSPACE_KIND_FLOAT32) == 0) { float f; uint32_t u = rd32(b); memcpy(&f, &u, 4); return f; }
    if (strcmp(k, KVSPACE_KIND_FLOAT64) == 0) { double d; uint64_t u = rd64(b); memcpy(&d, &u, 8); return d; }
    return (double)kvlangXvalueAsInt64(v);
}

uint64_t kvlangXvalueAsUint64(const kvlangXvalue_t *v) {
    if (kvlangXvalueNone(v)) return 0;
    kvspaceHead_t h; const uint8_t *b = v_body(v, &h);
    if (!b) return 0;
    const char *k = kvlangXvalueKind(v);
    if (strcmp(k, KVSPACE_KIND_UINT8) == 0) return b[0];
    if (strcmp(k, KVSPACE_KIND_UINT16) == 0) return rd16(b);
    if (strcmp(k, KVSPACE_KIND_UINT32) == 0) return rd32(b);
    if (strcmp(k, KVSPACE_KIND_UINT64) == 0) return rd64(b);
    return (uint64_t)kvlangXvalueAsInt64(v);
}

bool kvlangXvalueAsBool(const kvlangXvalue_t *v) {
    if (kvlangXvalueNone(v)) return false;
    kvspaceHead_t h; const uint8_t *b = v_body(v, &h);
    return b && h.body_len > 0 && b[0] != 0;
}

uint32_t kvlangXvalueChar32At(const kvlangXvalue_t *v, int32_t idx) {
    kvspaceHead_t h; const uint8_t *b = v_body(v, &h);
    if (!b) return 0;
    kvlang_kindexpr_t kx; kvlang_kindexpr_parse(h.kindexpr, &kx);
    if (idx < 0 || idx >= kx.array_len) return 0;
    return rd32(b + idx * 4);
}

/* ── UTF-8 ↔ UTF-32 ────────────────────────────────────────────────── */

static void utf8_putc(kvlangStrbuf_t *b, uint32_t cp) {
    if (cp < 0x80) kvlangStrbufPutc(b, (char)cp);
    else if (cp < 0x800) { kvlangStrbufPutc(b, (char)(0xC0 | (cp >> 6))); kvlangStrbufPutc(b, (char)(0x80 | (cp & 0x3F))); }
    else if (cp < 0x10000) {
        kvlangStrbufPutc(b, (char)(0xE0 | (cp >> 12))); kvlangStrbufPutc(b, (char)(0x80 | ((cp >> 6) & 0x3F)));
        kvlangStrbufPutc(b, (char)(0x80 | (cp & 0x3F)));
    } else {
        kvlangStrbufPutc(b, (char)(0xF0 | (cp >> 18))); kvlangStrbufPutc(b, (char)(0x80 | ((cp >> 12) & 0x3F)));
        kvlangStrbufPutc(b, (char)(0x80 | ((cp >> 6) & 0x3F))); kvlangStrbufPutc(b, (char)(0x80 | (cp & 0x3F)));
    }
}

static uint32_t utf8_next(const char *s, size_t *i, size_t len) {
    const unsigned char *p = (const unsigned char *)s;
    uint32_t cp = p[*i];
    if (cp < 0x80) { (*i)++; return cp; }
    int n = 0;
    if ((cp & 0xE0) == 0xC0) { n = 1; cp &= 0x1F; }
    else if ((cp & 0xF0) == 0xE0) { n = 2; cp &= 0x0F; }
    else if ((cp & 0xF8) == 0xF0) { n = 3; cp &= 0x07; }
    else { (*i)++; return 0xFFFD; }
    (*i)++;
    for (int j = 0; j < n && *i < len; j++, (*i)++) cp = (cp << 6) | (p[*i] & 0x3F);
    return cp;
}

static char *utf32_to_utf8(const uint8_t *body, int32_t blen) {
    kvlangStrbuf_t b; kvlangStrbufInit(&b);
    for (int32_t i = 0; i + 4 <= blen; i += 4) utf8_putc(&b, rd32(body + i));
    return kvlangStrbufDetach(&b);
}

static char *strndup2(const uint8_t *p, int32_t n) {
    char *s = malloc((size_t)n + 1);
    if (s) { memcpy(s, p, (size_t)n); s[n] = 0; }
    return s;
}

char *kvlangXvaluePtrTarget(const kvlangXvalue_t *v) {
    kvspaceHead_t h; const uint8_t *b = v_body(v, &h);
    if (!b) return strdup("");
    return strndup2(b, h.body_len);
}

static void append_num_int(kvlangStrbuf_t *b, int64_t n) { kvlangStrbufPrintf(b, "%lld", (long long)n); }
static void append_num_uint(kvlangStrbuf_t *b, uint64_t n) { kvlangStrbufPrintf(b, "%llu", (unsigned long long)n); }

char *kvlangXvalueValueString(const kvlangXvalue_t *v) {
    if (kvlangXvalueNone(v)) return strdup(KVSPACE_KIND_NONE);
    kvspaceHead_t h; const uint8_t *body = v_body(v, &h);
    if (!body) return strdup(KVSPACE_KIND_NONE);
    int32_t blen = h.body_len;
    const char *k = kvlangXvalueKind(v);
    kvlang_kindexpr_t kx; kvlang_kindexpr_parse(h.kindexpr, &kx);

    if (kx.ref == 1) {
        kvlangStrbuf_t b; kvlangStrbufInit(&b);
        kvlangStrbufPutn(&b, "\xE2\x86\x92", 3);
        kvlangStrbufPutn(&b, (const char *)body, (size_t)blen);
        return kvlangStrbufDetach(&b);
    }
    if (strcmp(k, KVSPACE_KIND_BOOL) == 0) return strdup(body[0] ? "true" : "false");
    if (strcmp(k, KVSPACE_KIND_INT8) == 0) { kvlangStrbuf_t b; kvlangStrbufInit(&b); append_num_int(&b, (int8_t)body[0]); return kvlangStrbufDetach(&b); }
    if (strcmp(k, KVSPACE_KIND_INT16) == 0) { kvlangStrbuf_t b; kvlangStrbufInit(&b); append_num_int(&b, (int16_t)rd16(body)); return kvlangStrbufDetach(&b); }
    if (strcmp(k, KVSPACE_KIND_INT32) == 0) { kvlangStrbuf_t b; kvlangStrbufInit(&b); append_num_int(&b, (int32_t)rd32(body)); return kvlangStrbufDetach(&b); }
    if (strcmp(k, KVSPACE_KIND_INT64) == 0) { kvlangStrbuf_t b; kvlangStrbufInit(&b); append_num_int(&b, (int64_t)rd64(body)); return kvlangStrbufDetach(&b); }
    if (strcmp(k, KVSPACE_KIND_UINT8) == 0) { kvlangStrbuf_t b; kvlangStrbufInit(&b); append_num_uint(&b, body[0]); return kvlangStrbufDetach(&b); }
    if (strcmp(k, KVSPACE_KIND_UINT16) == 0) { kvlangStrbuf_t b; kvlangStrbufInit(&b); append_num_uint(&b, rd16(body)); return kvlangStrbufDetach(&b); }
    if (strcmp(k, KVSPACE_KIND_UINT32) == 0) { kvlangStrbuf_t b; kvlangStrbufInit(&b); append_num_uint(&b, rd32(body)); return kvlangStrbufDetach(&b); }
    if (strcmp(k, KVSPACE_KIND_UINT64) == 0) { kvlangStrbuf_t b; kvlangStrbufInit(&b); append_num_uint(&b, rd64(body)); return kvlangStrbufDetach(&b); }
    if (strcmp(k, KVSPACE_KIND_FLOAT32) == 0 || strcmp(k, KVSPACE_KIND_FLOAT64) == 0) {
        char tmp[64]; kvlangFormatFloat(tmp, sizeof tmp, kvlangXvalueAsFloat64(v)); return strdup(tmp);
    }
    if (strcmp(k, KVSPACE_KIND_CHAR_UTF8) == 0 || strcmp(k, KVSPACE_KIND_CHAR_ASCII) == 0) return strndup2(body, blen);
    if (strcmp(k, KVSPACE_KIND_CHAR) == 0) return utf32_to_utf8(body, blen);
    if (strcmp(k, KVSPACE_KIND_RWIR) == 0 || strcmp(k, KVSPACE_KIND_RWIR_OR_RWFUNC) == 0) return strndup2(body + (blen >= 4 ? 4 : 0), blen >= 4 ? blen - 4 : 0);
    if (strcmp(k, KVSPACE_KIND_RWFUNC) == 0) {
        kvlangStrbuf_t b; kvlangStrbufInit(&b);
        kvlangStrbufPrintf(&b, "r%d/w%d", (blen >= 2 ? rd16(body) : 0), (blen >= 4 ? rd16(body + 2) : 0));
        return kvlangStrbufDetach(&b);
    }
    if (strcmp(k, KVSPACE_KIND_INDEX) == 0) {
        int n = blen >= 4 ? (int)rd32(body) : 0;
        kvlangStrbuf_t b; kvlangStrbufInit(&b); kvlangStrbufPrintf(&b, "(%d)", n); return kvlangStrbufDetach(&b);
    }
    if (strcmp(k, KVSPACE_KIND_OBJ) == 0) return strdup(KVSPACE_KIND_OBJ);
    if (strcmp(k, KVSPACE_KIND_MAP) == 0) {
        kvlangStrbuf_t b; kvlangStrbufInit(&b);
        kvlangStrbufPuts(&b, "map[");
        for (int d = 0; d < kx.ndim; d++) {
            if (d) kvlangStrbufPutc(&b, ',');
            kvlangStrbufPrintf(&b, "%d", kx.dims[d]);
        }
        kvlangStrbufPutc(&b, ']');
        return kvlangStrbufDetach(&b);
    }
    return strndup2(body, blen);
}

void kvlangXvalueNewTlv(kvlangXvalue_t *v, const char *kind, const uint8_t *raw, uint32_t raw_len, int32_t al) {
    uint32_t len;
    v->borrowed = 0;
    v->data = kvlangXvalueEncodeTlv(kind, raw, raw_len, al, &len);
    v->len = len;
}

void kvlangXvalueNewTlvDims(kvlangXvalue_t *v, const char *kind, const uint8_t *raw, uint32_t raw_len,
                            const int32_t *dims, int32_t ndim) {
    uint8_t *buf = NULL;
    uint32_t tl = 0;
    v->borrowed = 0;
    if (kvlangXvalueEncodeBox(kind, raw, raw_len, dims, ndim, &buf, &tl) != 0 || !buf) {
        v->data = NULL; v->len = 0; return;
    }
    v->data = buf; v->len = tl;
}

void kvlangXvalueNewInt64(kvlangXvalue_t *v, int64_t n) {
    uint8_t r[8];
    r[0] = n & 0xFF; r[1] = (n >> 8) & 0xFF; r[2] = (n >> 16) & 0xFF; r[3] = (n >> 24) & 0xFF;
    r[4] = (n >> 32) & 0xFF; r[5] = (n >> 40) & 0xFF; r[6] = (n >> 48) & 0xFF; r[7] = (n >> 56) & 0xFF;
    kvlangXvalueNewTlv(v, KVSPACE_KIND_INT64, r, 8, 1);
}
void kvlangXvalueNewFloat64(kvlangXvalue_t *v, double f) {
    uint64_t u; memcpy(&u, &f, 8);
    uint8_t r[8];
    for (int i = 0; i < 8; i++) r[i] = (u >> (i * 8)) & 0xFF;
    kvlangXvalueNewTlv(v, KVSPACE_KIND_FLOAT64, r, 8, 1);
}
void kvlangXvalueNewBool(kvlangXvalue_t *v, bool b) {
    uint8_t r = b ? 1 : 0;
    kvlangXvalueNewTlv(v, KVSPACE_KIND_BOOL, &r, 1, 1);
}
void kvlangXvalueNewCharUtf8(kvlangXvalue_t *v, const char *s) {
    uint32_t sl = (uint32_t)strlen(s);
    kvlangXvalueNewTlv(v, KVSPACE_KIND_CHAR_UTF8, (const uint8_t *)s, sl, (int32_t)sl);
}
void kvlangXvalueNewCharKind(kvlangXvalue_t *v, const char *kind, const char *s) {
    uint32_t sl = (uint32_t)strlen(s);
    kvlangXvalueNewTlv(v, kind, (const uint8_t *)s, sl, (int32_t)sl);
}
void kvlangXvalueNewCharUtf32(kvlangXvalue_t *v, const char *s) {
    size_t len = strlen(s);
    kvlangStrbuf_t raw; kvlangStrbufInit(&raw);
    size_t i = 0;
    while (i < len) {
        uint32_t cp = utf8_next(s, &i, len);
        uint8_t le[4] = { cp & 0xFF, (cp >> 8) & 0xFF, (cp >> 16) & 0xFF, (cp >> 24) & 0xFF };
        kvlangStrbufPutn(&raw, (const char *)le, 4);
    }
    kvlangXvalueNewTlv(v, KVSPACE_KIND_CHAR, (const uint8_t *)raw.p, (uint32_t)raw.len, (int32_t)(raw.len / 4));
    kvlangStrbufFree(&raw);
}
void kvlangXvalueNewPtr(kvlangXvalue_t *v, const char *target_kindexpr, const char *target) {
    uint8_t *tmp = NULL; uint32_t tl = 0, len = 0;
    if (kvspaceNewPtr(target_kindexpr, target, &tmp, &tl) != 0) { v->data = NULL; v->len = 0; return; }
    v->data = kvlangXvalueOwn(tmp, tl, &len);
    v->len = len;
}
void kvlangXvalueNewRwir(kvlangXvalue_t *v, int32_t nr, int32_t nw, const char *sig) {
    size_t sl = strlen(sig);
    uint8_t *raw = malloc(4 + sl);
    raw[0] = nr & 0xFF; raw[1] = (nr >> 8) & 0xFF;
    raw[2] = nw & 0xFF; raw[3] = (nw >> 8) & 0xFF;
    memcpy(raw + 4, sig, sl);
    kvlangXvalueNewTlv(v, KVSPACE_KIND_DEF_RWIR, raw, (uint32_t)(4 + sl), 1);
    free(raw);
}

void kvlangFormatFloat(char *out, size_t cap, double v) {
    char s[64];
    snprintf(s, sizeof s, "%.16g", v);
    if (strtod(s, NULL) != v) snprintf(s, sizeof s, "%.17g", v);
    snprintf(out, cap, "%s", s);
    if (strchr(out, 'e')) return;
    char *dot = strchr(out, '.');
    if (!dot) {
        size_t l = strlen(out);
        if (l + 2 < cap) { out[l] = '.'; out[l + 1] = '0'; out[l + 2] = 0; }
        return;
    }
    char *p = out + strlen(out) - 1;
    while (p > dot && *p == '0') *p-- = 0;
    if (p == dot) { p[1] = '0'; p[2] = 0; }
}
