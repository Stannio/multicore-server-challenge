package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HS256 JWTs with the standard library, checked like golang-jwt/jwt/v5 ParseWithClaims into RegisteredClaims with
// WithValidMethods(HS256) and WithExpirationRequired: alg HS256, a valid signature, exp present and in the future,
// nbf (if present) not in the future, and the registered claims' JSON types. One known difference: when a time
// claim appears twice, only the last occurrence is type-checked (it is still the one enforced).
// Every request verifies its token; nothing is remembered.

const tokenTTL = 30 * 24 * 3600

// hs256Header is base64url(`{"alg":"HS256","typ":"JWT"}`): what this server and golang-jwt emit.
const hs256Header = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"

var b64 = base64.RawURLEncoding

type signer struct{ macs sync.Pool }

func newSigner(secret []byte) *signer {
	s := &signer{}
	s.macs.New = func() any { return hmac.New(sha256.New, secret) }
	return s
}

func (s *signer) sum(dst, msg []byte) []byte {
	h := s.macs.Get().(hash.Hash)
	h.Reset()
	h.Write(msg)
	dst = h.Sum(dst)
	s.macs.Put(h)
	return dst
}

// issue returns a token with claims exactly {sub, iat, exp = iat + 30 days}.
func (s *signer) issue(id int64) string {
	now := time.Now().Unix()
	p := make([]byte, 0, 64)
	p = append(p, `{"sub":"`...)
	p = strconv.AppendInt(p, id, 10)
	p = append(p, `","iat":`...)
	p = strconv.AppendInt(p, now, 10)
	p = append(p, `,"exp":`...)
	p = strconv.AppendInt(p, now+tokenTTL, 10)
	p = append(p, '}')

	t := make([]byte, 0, 192)
	t = append(t, hs256Header+"."...)
	t = b64.AppendEncode(t, p)
	var mac [sha256.Size]byte
	sig := s.sum(mac[:0], t)
	t = append(t, '.')
	return string(b64.AppendEncode(t, sig))
}

// verify returns the user id (> 0) of a valid token, or 0.
func (s *signer) verify(tok string) int64 {
	head, rest, ok1 := strings.Cut(tok, ".")
	payload, sig, ok2 := strings.Cut(rest, ".")
	if !ok1 || !ok2 || strings.IndexByte(sig, '.') >= 0 {
		return 0
	}
	if head != hs256Header && !headerIsHS256(head) {
		return 0
	}
	var got, want [sha256.Size]byte
	if b64.DecodedLen(len(sig)) != sha256.Size {
		return 0
	}
	if n, err := b64.Decode(got[:], []byte(sig)); err != nil || n != sha256.Size {
		return 0
	}
	if !hmac.Equal(got[:], s.sum(want[:0], []byte(tok[:len(head)+1+len(payload)]))) {
		return 0
	}
	raw, err := b64.DecodeString(payload)
	if err != nil {
		return 0
	}
	// Field types as in golang-jwt's RegisteredClaims (json.Number decodes like its NumericDate).
	var c struct {
		Sub string       `json:"sub"`
		Iss string       `json:"iss"`
		Jti string       `json:"jti"`
		Aud audience     `json:"aud"`
		Exp *json.Number `json:"exp"`
		Nbf *json.Number `json:"nbf"`
		Iat *json.Number `json:"iat"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp == nil {
		return 0
	}
	now := time.Now()
	exp, err := c.Exp.Float64()
	if err != nil || !now.Before(time.Unix(int64(exp), 0)) {
		return 0
	}
	if c.Nbf != nil {
		nbf, err := c.Nbf.Float64()
		if err != nil || now.Before(time.Unix(int64(nbf), 0)) {
			return 0
		}
	}
	if c.Iat != nil {
		if _, err := c.Iat.Float64(); err != nil {
			return 0
		}
	}
	id, ok := parseUint(c.Sub)
	if !ok || id <= 0 {
		return 0
	}
	return id
}

// headerIsHS256 is the slow path for a header that isn't byte-identical to ours. Like golang-jwt it decodes a
// map and reads the exact key "alg".
func headerIsHS256(seg string) bool {
	raw, err := b64.DecodeString(seg)
	if err != nil {
		return false
	}
	var h map[string]any
	return json.Unmarshal(raw, &h) == nil && h["alg"] == "HS256"
}

// audience accepts what golang-jwt's ClaimStrings accepts: a string, an array of strings, or null.
type audience struct{}

func (*audience) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch v := v.(type) {
	case nil, string:
		return nil
	case []any:
		for _, x := range v {
			if _, ok := x.(string); !ok {
				return errBadAudience
			}
		}
		return nil
	}
	return errBadAudience
}

var errBadAudience = errors.New("aud must be a string or an array of strings")
