package main

import (
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// Responses are appended into pooled buffers: no reflection and next to no allocations.

type out struct{ b []byte }

var (
	outPool  = sync.Pool{New: func() any { return &out{b: make([]byte, 0, 16<<10)} }}
	jsonType = []string{"application/json"}
)

func newOut() *out {
	o := outPool.Get().(*out)
	o.b = o.b[:0]
	return o
}

// send writes the buffer with an explicit Content-Length (so net/http never falls back to chunked
// encoding) and recycles it. An unsent out (error paths) is simply left to the GC.
func (o *out) send(w http.ResponseWriter, status int) {
	h := w.Header()
	h["Content-Type"] = jsonType
	h["Content-Length"] = []string{strconv.Itoa(len(o.b))}
	w.WriteHeader(status)
	w.Write(o.b)
	if cap(o.b) <= 1<<20 {
		outPool.Put(o)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	o := newOut()
	o.b = append(o.b, `{"errors":{"body":[`...)
	o.b = appendString(o.b, msg)
	o.b = append(o.b, "]}}"...)
	o.send(w, status)
}

func internalError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

const hexDigits = "0123456789abcdef"

// appendString appends s as a JSON string. Like encoding/json it turns invalid UTF-8 into U+FFFD;
// unlike it, it leaves <, >, & and U+2028/U+2029 unescaped, which is still valid JSON.
func appendString[S ~string | ~[]byte](b []byte, s S) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRune([]byte(s[i:min(i+utf8.UTFMax, len(s))]))
			if r == utf8.RuneError && size == 1 {
				b = append(b, s[start:i]...)
				b = append(b, string(utf8.RuneError)...)
				start = i + 1
			}
			i += size
			continue
		}
		if c >= 0x20 && c != '"' && c != '\\' {
			i++
			continue
		}
		b = append(b, s[start:i]...)
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		i++
		start = i
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

func appendNullString(b []byte, s *string) []byte {
	if s == nil {
		return append(b, "null"...)
	}
	return appendString(b, *s)
}

// appendDigits appends v zero-padded to n digits.
func appendDigits(b []byte, v, n int) []byte {
	for d := n - 1; d >= 0; d-- {
		b = append(b, '0'+byte(v/pow10[d]%10))
	}
	return b
}

var pow10 = [...]int{1, 10, 100, 1000}

// appendTime appends t as a JSON string "2006-01-02T15:04:05.000Z" (UTC, milliseconds truncated).
func appendTime(b []byte, t time.Time) []byte {
	t = t.UTC()
	year, month, day := t.Date()
	hour, minute, sec := t.Clock()
	b = append(b, '"')
	b = appendDigits(b, year, 4)
	b = append(b, '-')
	b = appendDigits(b, int(month), 2)
	b = append(b, '-')
	b = appendDigits(b, day, 2)
	b = append(b, 'T')
	b = appendDigits(b, hour, 2)
	b = append(b, ':')
	b = appendDigits(b, minute, 2)
	b = append(b, ':')
	b = appendDigits(b, sec, 2)
	b = append(b, '.')
	b = appendDigits(b, t.Nanosecond()/1e6, 3)
	return append(b, 'Z', '"')
}

func appendProfile(b []byte, p *profileJSON) []byte {
	b = append(b, `{"username":`...)
	b = appendString(b, p.Username)
	b = append(b, `,"bio":`...)
	b = appendNullString(b, p.Bio)
	b = append(b, `,"image":`...)
	b = appendNullString(b, p.Image)
	b = append(b, `,"following":`...)
	b = strconv.AppendBool(b, p.Following)
	return append(b, '}')
}

func appendUser(b []byte, u *userJSON) []byte {
	b = append(b, `{"email":`...)
	b = appendString(b, u.Email)
	b = append(b, `,"token":`...)
	b = appendString(b, u.Token)
	b = append(b, `,"username":`...)
	b = appendString(b, u.Username)
	b = append(b, `,"bio":`...)
	b = appendNullString(b, u.Bio)
	b = append(b, `,"image":`...)
	b = appendNullString(b, u.Image)
	return append(b, '}')
}

func appendArticle(b []byte, x *articleJSON) []byte {
	b = append(b, `{"slug":`...)
	b = appendString(b, x.Slug)
	b = append(b, `,"title":`...)
	b = appendString(b, x.Title)
	b = append(b, `,"description":`...)
	b = appendString(b, x.Description)
	if x.Body != nil {
		b = append(b, `,"body":`...)
		b = appendString(b, *x.Body)
	}
	b = append(b, `,"tagList":[`...)
	for i, t := range x.TagList {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, t)
	}
	b = append(b, `],"createdAt":`...)
	b = appendTime(b, x.CreatedAt)
	b = append(b, `,"updatedAt":`...)
	b = appendTime(b, x.UpdatedAt)
	b = append(b, `,"favorited":`...)
	b = strconv.AppendBool(b, x.Favorited)
	b = append(b, `,"favoritesCount":`...)
	b = strconv.AppendInt(b, x.FavoritesCount, 10)
	b = append(b, `,"author":`...)
	b = appendProfile(b, &x.Author)
	return append(b, '}')
}

func appendComment(b []byte, c *commentJSON) []byte {
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, c.ID, 10)
	b = append(b, `,"createdAt":`...)
	b = appendTime(b, c.CreatedAt)
	b = append(b, `,"updatedAt":`...)
	b = appendTime(b, c.UpdatedAt)
	b = append(b, `,"body":`...)
	b = appendString(b, c.Body)
	b = append(b, `,"author":`...)
	b = appendProfile(b, &c.Author)
	return append(b, '}')
}
