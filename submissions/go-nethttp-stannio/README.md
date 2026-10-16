# Go: `net/http` + pgx

**Front: direct.** The server holds every user's keep-alive connection itself on `0.0.0.0:80`, saving the CPU
Nginx would use.

Started from [`examples/go`](../../examples/go) and optimized from profiles of the full seed under load.

| Piece | Version |
|---|---|
| Go | 1.27.2 (`install.sh`, official tarball) |
| HTTP | stdlib `net/http`, Go 1.22+ pattern routing |
| Postgres | `github.com/jackc/pgx/v5` v5.11.0, `pgxpool` |
| bcrypt | `golang.org/x/crypto/bcrypt`, cost 10 |
| JWT | stdlib `crypto/hmac` + `crypto/sha256` (HS256) |

## Optimizations so far

Postgres (measured on the full seed with `pg_stat_statements`; every rewrite checked row for row against
SPEC's SQL):

- **`plan_cache_mode=force_generic_plan` + `jit=off`** per connection (pgx `RuntimeParams`, no extra round trip).
  The global and tag lists were re-planned on every call (LIMIT/OFFSET parameters make the generic plan look
  expensive), which was ~20% of Postgres time. `jit=off` is required with it: the generic global-list plan would
  otherwise be JIT-compiled on every call.
- **tag_list by primary key**: `ARRAY(SELECT (SELECT t.name FROM tags t WHERE t.id = at.tag_id) ... ORDER BY 1)`
  instead of a hash join over all 200 tags for every article row (~2.5x cheaper per row).
- **Global list picks its page first**, so skipped OFFSET rows don't run the per-row subqueries.
- **One round trip per request**: lists send page + count as one pgx batch; comments run SPEC's slug lookup and
  comments query as one statement (no row = 404, one NULL row = no comments). The feed returns
  `following = true` (every feed row is by a followee).

Go:

- **Responses are appended into pooled byte buffers** instead of `encoding/json` reflection, with an explicit
  `Content-Length` (no chunked encoding) and a hand-written timestamp formatter. Lists and comments stream rows
  straight into the buffer; `GET /api/tags` copies each name from pgx's raw row bytes.
- **HS256 JWTs with the standard library**, checked like golang-jwt with `WithValidMethods(HS256)` and
  `WithExpirationRequired`. Every request still verifies its token (rule 7).
- **GC**: `GOGC=off`, `GOMEMLIMIT=3GiB`. The live heap is small, so collecting every time it doubles only
  burns CPU.
- **No TCP keep-alive probes** on client connections, and a 120 s idle timeout (users think up to 70 s).

`json_test.go` checks the JSON escaping and timestamps against `encoding/json`/`time`, and the token rules.
