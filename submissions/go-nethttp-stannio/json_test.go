package main

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestAppendString(t *testing.T) {
	for _, s := range []string{"", "plain", `q"uote`, `back\slash`, "nl\nnl\r\t", "\x00\x01\x1f\x7f", "<>&", "é ü 日本 🎉",
		"bad\xffutf8\xc3", "\xe2\x80\xa8\xe2\x80\xa9", strings.Repeat("x\"", 100)} {
		var got string
		if err := json.Unmarshal(appendString(nil, s), &got); err != nil {
			t.Fatalf("%q: invalid JSON %s: %v", s, appendString(nil, s), err)
		}
		want, _ := json.Marshal(s) // encoding/json's view of the same string (invalid UTF-8 -> U+FFFD)
		var w string
		json.Unmarshal(want, &w)
		if got != w {
			t.Errorf("%q: decoded %q, want %q", s, got, w)
		}
		if b := appendString(nil, []byte(s)); string(b) != string(appendString(nil, s)) {
			t.Errorf("%q: []byte and string variants differ", s)
		}
	}
}

func TestAppendTime(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	loc := time.FixedZone("x", 5*3600+1800)
	for i := 0; i < 10000; i++ {
		tm := time.Unix(r.Int64N(4102444800), r.Int64N(1e9)).In(loc)
		want := `"` + tm.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z") + `"`
		if got := string(appendTime(nil, tm)); got != want {
			t.Fatalf("%v: got %s, want %s", tm, got, want)
		}
	}
}

func TestJWT(t *testing.T) {
	s := newSigner([]byte("secret"))
	if id := s.verify(s.issue(42)); id != 42 {
		t.Fatalf("issued token verifies to %d", id)
	}
	now := time.Now().Unix()
	mint := func(header, payload string, signer *signer) string {
		t := b64.EncodeToString([]byte(header)) + "." + b64.EncodeToString([]byte(payload))
		return t + "." + b64.EncodeToString(signer.sum(nil, []byte(t)))
	}
	hs := `{"alg":"HS256","typ":"JWT"}`
	ok := func(p string) string { return mint(hs, p, s) }
	cases := []struct {
		name string
		tok  string
		want int64
	}{
		{"valid", ok(`{"sub":"7","iat":1,"exp":` + itoa(now+60) + `}`), 7},
		{"header key order", mint(`{"typ":"JWT","alg":"HS256"}`, `{"sub":"7","exp":`+itoa(now+60)+`}`, s), 7},
		{"float exp", ok(`{"sub":"7","exp":` + itoa(now+60) + `.5}`), 7},
		{"nbf past", ok(`{"sub":"7","nbf":` + itoa(now-5) + `,"exp":` + itoa(now+60) + `}`), 7},
		{"nbf future", ok(`{"sub":"7","nbf":` + itoa(now+50) + `,"exp":` + itoa(now+60) + `}`), 0},
		{"expired", ok(`{"sub":"7","exp":` + itoa(now-1) + `}`), 0},
		{"exp now", ok(`{"sub":"7","exp":` + itoa(now) + `}`), 0},
		{"no exp", ok(`{"sub":"7","iat":1}`), 0},
		{"null exp", ok(`{"sub":"7","exp":null}`), 0},
		{"numeric sub", ok(`{"sub":7,"exp":` + itoa(now+60) + `}`), 0},
		{"non-numeric sub", ok(`{"sub":"abc","exp":` + itoa(now+60) + `}`), 0},
		{"zero sub", ok(`{"sub":"0","exp":` + itoa(now+60) + `}`), 0},
		{"string iat", ok(`{"sub":"7","iat":"x","exp":` + itoa(now+60) + `}`), 0},
		{"alg none", mint(`{"alg":"none","typ":"JWT"}`, `{"sub":"7","exp":`+itoa(now+60)+`}`, s), 0},
		{"alg HS512", mint(`{"alg":"HS512","typ":"JWT"}`, `{"sub":"7","exp":`+itoa(now+60)+`}`, s), 0},
		{"wrong secret", mint(hs, `{"sub":"7","exp":`+itoa(now+60)+`}`, newSigner([]byte("other"))), 0},
		{"alg key case", mint(`{"Alg":"HS256"}`, `{"sub":"7","exp":`+itoa(now+60)+`}`, s), 0},
		{"alg plus ALG", mint(`{"alg":"HS256","ALG":"none"}`, `{"sub":"7","exp":`+itoa(now+60)+`}`, s), 7},
		{"aud string", ok(`{"sub":"7","aud":"x","exp":` + itoa(now+60) + `}`), 7},
		{"aud array", ok(`{"sub":"7","aud":["x","y"],"exp":` + itoa(now+60) + `}`), 7},
		{"aud number", ok(`{"sub":"7","aud":1,"exp":` + itoa(now+60) + `}`), 0},
		{"aud mixed array", ok(`{"sub":"7","aud":["x",1],"exp":` + itoa(now+60) + `}`), 0},
		{"iss number", ok(`{"sub":"7","iss":1,"exp":` + itoa(now+60) + `}`), 0},
		{"jti number", ok(`{"sub":"7","jti":1,"exp":` + itoa(now+60) + `}`), 0},
		{"garbage", "not.a.jwt", 0},
		{"two parts", "a.b", 0},
		{"four parts", ok(`{"sub":"7","exp":`+itoa(now+60)+`}`) + ".x", 0},
	}
	for _, c := range cases {
		if got := s.verify(c.tok); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
