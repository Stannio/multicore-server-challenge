package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type commentJSON struct {
	ID                   int64
	CreatedAt, UpdatedAt time.Time
	Body                 string
	Author               profileJSON
}

// GET /api/articles/{slug}/comments: SPEC's slug lookup and comments query in one statement (one snapshot).
// No row means no such article (404); a single row of NULLs is an article without comments.
func (a *app) listComments(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	rows, _ := a.pool.Query(r.Context(), `SELECT c.id, c.body, c.created_at, c.updated_at, c.username, c.bio, c.image, c.following
  FROM articles a
  LEFT JOIN LATERAL (
    SELECT c.id, c.body, c.created_at, c.updated_at, u.username, u.bio, u.image,
           EXISTS (SELECT 1 FROM follows fo WHERE fo.follower_id = $1 AND fo.followee_id = c.author_id) AS following
      FROM comments c JOIN users u ON u.id = c.author_id
     WHERE c.article_id = a.id
     ORDER BY c.created_at DESC, c.id DESC
     LIMIT 100) c ON true
 WHERE a.slug = $2
 ORDER BY c.created_at DESC, c.id DESC`, viewer, r.PathValue("slug"))
	defer rows.Close()
	o := newOut()
	o.b = append(o.b, `{"comments":[`...)
	found, n := false, 0
	for rows.Next() {
		found = true
		if rows.RawValues()[0] == nil {
			continue
		}
		var c commentJSON
		if err := rows.Scan(&c.ID, &c.Body, &c.CreatedAt, &c.UpdatedAt, &c.Author.Username, &c.Author.Bio, &c.Author.Image, &c.Author.Following); err != nil {
			internalError(w, err)
			return
		}
		if n > 0 {
			o.b = append(o.b, ',')
		}
		n++
		o.b = appendComment(o.b, &c)
	}
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "article not found")
		return
	}
	o.b = append(o.b, "]}"...)
	o.send(w, http.StatusOK)
}

// POST /api/articles/{slug}/comments
func (a *app) createComment(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	m, err := readWrapper(r, "comment")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid body")
		return
	}
	body, err := reqString(m, "body")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "body is required")
		return
	}
	// SPEC's slug lookup and insert as one statement: no row means no such article (404), and nothing was written.
	var c commentJSON
	err = a.pool.QueryRow(r.Context(), `WITH c AS (INSERT INTO comments (article_id, author_id, body)
           SELECT a.id, $2::bigint, $3::text FROM articles a WHERE a.slug = $1
           RETURNING id, body, created_at, updated_at, author_id)
SELECT c.id, c.body, c.created_at, c.updated_at, u.username, u.bio, u.image
  FROM c JOIN users u ON u.id = c.author_id`, r.PathValue("slug"), viewer, body).Scan(&c.ID, &c.Body, &c.CreatedAt, &c.UpdatedAt, &c.Author.Username, &c.Author.Bio, &c.Author.Image)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "article not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	o := newOut()
	o.b = append(o.b, `{"comment":`...)
	o.b = appendComment(o.b, &c)
	o.b = append(o.b, '}')
	o.send(w, http.StatusCreated)
}

// DELETE /api/articles/{slug}/comments/{id}
func (a *app) deleteComment(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	id, ok := parseUint(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}
	var authorID int64
	err := a.pool.QueryRow(r.Context(), `SELECT c.author_id FROM comments c JOIN articles a ON a.id = c.article_id
 WHERE c.id = $1 AND a.slug = $2`, id, r.PathValue("slug")).Scan(&authorID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if authorID != viewer {
		writeError(w, http.StatusForbidden, "not the author")
		return
	}
	if _, err := a.pool.Exec(r.Context(), `DELETE FROM comments WHERE id = $1`, id); err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
