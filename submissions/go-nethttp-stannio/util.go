package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type app struct {
	pool *pgxpool.Pool
	jwt  *signer
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---------- query parameters ----------

// parseUint accepts only ASCII digits (no sign, no spaces) that fit in an int64.
func parseUint(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// limitOffset reads limit (default 20, 1..100) and offset (default 0, >= 0).
// A parameter that is present (even empty) must be valid.
func limitOffset(r *http.Request) (limit, offset int64, ok bool) {
	q := r.URL.Query()
	limit, offset = 20, 0
	if vs, present := q["limit"]; present {
		n, valid := parseUint(vs[0])
		if !valid || n < 1 || n > 100 {
			return 0, 0, false
		}
		limit = n
	}
	if vs, present := q["offset"]; present {
		n, valid := parseUint(vs[0])
		if !valid {
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// ---------- request bodies ----------

var errBadBody = errors.New("invalid body")

// readWrapper parses the body as a JSON object and returns the object under key as raw fields.
// Matching is case-sensitive; unknown fields are ignored.
func readWrapper(r *http.Request, key string) (map[string]json.RawMessage, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, errBadBody
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, errBadBody
	}
	raw, ok := top[key]
	if !ok {
		return nil, errBadBody
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw, &inner); err != nil || inner == nil {
		return nil, errBadBody
	}
	return inner, nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// optString: absent or null gives nil; a string gives a pointer to it; anything else is an error.
// With nonEmpty, an empty string is an error too.
func optString(m map[string]json.RawMessage, k string, nonEmpty bool) (*string, error) {
	raw, ok := m[k]
	if !ok || isNull(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errBadBody
	}
	if nonEmpty && s == "" {
		return nil, errBadBody
	}
	return &s, nil
}

// reqString: must be present and a non-empty string.
func reqString(m map[string]json.RawMessage, k string) (string, error) {
	p, err := optString(m, k, true)
	if err != nil || p == nil {
		return "", errBadBody
	}
	return *p, nil
}

// ---------- auth ----------

// auth inspects the Authorization header. present reports whether the header was sent;
// id > 0 means a valid token for that user id.
func (a *app) auth(r *http.Request) (id int64, token string, present bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return 0, "", false
	}
	token, ok := strings.CutPrefix(h, "Token ")
	if !ok {
		return 0, "", true
	}
	if id = a.jwt.verify(token); id == 0 {
		return 0, "", true
	}
	return id, token, true
}

type authed func(w http.ResponseWriter, r *http.Request, viewer int64, token string)

// required: a missing or invalid token gives 401.
func (a *app) required(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, token, _ := a.auth(r)
		if id == 0 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r, id, token)
	}
}

// optional: no header means viewer 0; a header that is present but invalid gives 401.
func (a *app) optional(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, token, present := a.auth(r)
		if present && id == 0 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r, id, token)
	}
}
