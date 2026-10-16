package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// SPEC.md's SQL with three result-preserving changes, each measured on the full seed and checked row for row
// against the reference:
//   - tag_list looks each tag up by primary key instead of hash-joining all 200 tags for every article
//     (identical rows: article_tags.tag_id is a foreign key to tags, and ORDER BY 1 is ORDER BY t.name);
//   - the global list picks its page of ids first, so skipped OFFSET rows don't run the per-row subqueries
//     (identical: author_id is NOT NULL and a foreign key, so the users join never drops or adds rows);
//   - the feed returns following = true: every feed row is by an author the viewer follows, in the same snapshot.
const (
	articleRow  = `a.id, a.slug, a.title, a.description, ` + articleRest
	articleRest = `a.created_at, a.updated_at,
       ARRAY(SELECT (SELECT t.name FROM tags t WHERE t.id = at.tag_id) FROM article_tags at
              WHERE at.article_id = a.id ORDER BY 1) AS tag_list,
       (SELECT count(*) FROM favorites f WHERE f.article_id = a.id) AS favorites_count,
       EXISTS (SELECT 1 FROM favorites f WHERE f.article_id = a.id AND f.user_id = $1) AS favorited,
       u.username, u.bio, u.image,`
	following = `
       EXISTS (SELECT 1 FROM follows fo WHERE fo.follower_id = $1 AND fo.followee_id = a.author_id) AS following`
	fromArticles = `
  FROM articles a
  JOIN users u ON u.id = a.author_id`

	articleListSelect = `SELECT ` + articleRow + following + fromArticles

	articleFullSelect = `SELECT a.id, a.slug, a.title, a.description, a.body, ` + articleRest + following + fromArticles

	orderPage = ` ORDER BY a.id DESC LIMIT $2 OFFSET $3`

	listAll = `SELECT ` + articleRow + following + `
  FROM (SELECT id, slug, title, description, created_at, updated_at, author_id
          FROM articles ORDER BY id DESC LIMIT $2 OFFSET $3) a
  JOIN users u ON u.id = a.author_id
 ORDER BY a.id DESC`
	countAll        = `SELECT count(*) FROM (SELECT 1 FROM articles a LIMIT 1000) s`
	listByTag       = articleListSelect + ` WHERE a.id IN (SELECT at.article_id FROM article_tags at WHERE at.tag_id = (SELECT t.id FROM tags t WHERE t.name = $4) ORDER BY at.article_id DESC LIMIT $2 OFFSET $3)` + ` ORDER BY a.id DESC`
	countByTag      = `SELECT count(*) FROM (SELECT 1 FROM article_tags at WHERE at.tag_id = (SELECT t.id FROM tags t WHERE t.name = $1) LIMIT 1000) s`
	listByAuthor    = articleListSelect + ` WHERE a.author_id = (SELECT au.id FROM users au WHERE au.username = $4)` + orderPage
	countByAuthor   = `SELECT count(*) FROM (SELECT 1 FROM articles a WHERE a.author_id = (SELECT u.id FROM users u WHERE u.username = $1) LIMIT 1000) s`
	listByFavorite  = articleListSelect + ` WHERE a.id IN (SELECT f.article_id FROM favorites f WHERE f.user_id = (SELECT fu.id FROM users fu WHERE fu.username = $4) ORDER BY f.article_id DESC LIMIT $2 OFFSET $3)` + ` ORDER BY a.id DESC`
	countByFavorite = `SELECT count(*) FROM (SELECT 1 FROM favorites f WHERE f.user_id = (SELECT fu.id FROM users fu WHERE fu.username = $1) LIMIT 1000) s`
	listFeed        = `SELECT ` + articleRow + ` true AS following` + fromArticles + ` WHERE a.id IN (SELECT x.id FROM follows fo CROSS JOIN LATERAL (SELECT a2.id FROM articles a2 WHERE a2.author_id = fo.followee_id ORDER BY a2.id DESC LIMIT ($2::int + $3::int)) x WHERE fo.follower_id = $1 ORDER BY x.id DESC LIMIT $2 OFFSET $3)` + ` ORDER BY a.id DESC`
	countFeed       = `SELECT count(*) FROM (SELECT 1 FROM articles a WHERE a.author_id IN (SELECT fo.followee_id FROM follows fo WHERE fo.follower_id = $1) LIMIT 1000) s`

	articleBySlug = articleFullSelect + ` WHERE a.slug = $2`
	articleByID   = articleFullSelect + ` WHERE a.id = $2`

	insertArticle = `INSERT INTO articles (id, slug, author_id, title, description, body)
SELECT n.id, trim(both '-' from lower(regexp_replace($2, '[^A-Za-z0-9]+', '-', 'g'))) || '-' || n.id, $1, $2, $3, $4
  FROM (SELECT nextval(pg_get_serial_sequence('articles', 'id')) AS id) n
RETURNING id`
	insertTags        = `INSERT INTO tags (name) SELECT DISTINCT unnest($1::text[]) ON CONFLICT (name) DO NOTHING`
	insertArticleTags = `INSERT INTO article_tags (article_id, tag_id) SELECT $1, t.id FROM tags t WHERE t.name = ANY($2::text[])`

	updateArticle = `UPDATE articles SET title = COALESCE($2, title), description = COALESCE($3, description), body = COALESCE($4, body),
       slug = CASE WHEN $2::text IS NULL THEN slug ELSE trim(both '-' from lower(regexp_replace($2, '[^A-Za-z0-9]+', '-', 'g'))) || '-' || id END,
       updated_at = now()
 WHERE id = $1`
)

type articleJSON struct {
	Slug, Title, Description string
	Body                     *string // nil in lists
	TagList                  []string
	CreatedAt, UpdatedAt     time.Time
	Favorited                bool
	FavoritesCount           int64
	Author                   profileJSON
}

func scanListArticle(row pgx.CollectableRow) (articleJSON, error) {
	var x articleJSON
	var id int64
	err := row.Scan(&id, &x.Slug, &x.Title, &x.Description, &x.CreatedAt, &x.UpdatedAt, &x.TagList, &x.FavoritesCount,
		&x.Favorited, &x.Author.Username, &x.Author.Bio, &x.Author.Image, &x.Author.Following)
	return x, err
}

func scanFullArticle(row pgx.CollectableRow) (articleJSON, error) {
	var x articleJSON
	var id int64
	var body string
	err := row.Scan(&id, &x.Slug, &x.Title, &x.Description, &body, &x.CreatedAt, &x.UpdatedAt, &x.TagList, &x.FavoritesCount,
		&x.Favorited, &x.Author.Username, &x.Author.Bio, &x.Author.Image, &x.Author.Following)
	x.Body = &body
	return x, err
}

// writeFullArticle runs ARTICLE_FULL_SELECT with the given WHERE query and writes it, or 404.
func (a *app) writeFullArticle(w http.ResponseWriter, r *http.Request, status int, query string, viewer int64, key any) {
	rows, _ := a.pool.Query(r.Context(), query, viewer, key)
	x, err := pgx.CollectExactlyOneRow(rows, scanFullArticle)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "article not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	o := newOut()
	o.b = append(o.b, `{"article":`...)
	o.b = appendArticle(o.b, &x)
	o.b = append(o.b, '}')
	o.send(w, status)
}

// writeList sends the page and the capped count in one round trip (a pgx batch: two statements, each with its own
// snapshot, as when sent separately), streams the page's articles into the response, then appends the count.
func (a *app) writeList(w http.ResponseWriter, r *http.Request, list, count string, viewer, limit, offset int64, filter any) {
	b := &pgx.Batch{}
	switch {
	case filter != nil:
		b.Queue(list, viewer, limit, offset, filter)
		b.Queue(count, filter)
	case count == countFeed:
		b.Queue(list, viewer, limit, offset)
		b.Queue(count, viewer)
	default:
		b.Queue(list, viewer, limit, offset)
		b.Queue(count)
	}
	br := a.pool.SendBatch(r.Context(), b)
	defer br.Close()
	rows, _ := br.Query()
	defer rows.Close()
	o := newOut()
	o.b = append(o.b, `{"articles":[`...)
	for n := 0; rows.Next(); n++ {
		x, err := scanListArticle(rows)
		if err != nil {
			internalError(w, err)
			return
		}
		if n > 0 {
			o.b = append(o.b, ',')
		}
		o.b = appendArticle(o.b, &x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}
	var n int64
	if err := br.QueryRow().Scan(&n); err != nil {
		internalError(w, err)
		return
	}
	if err := br.Close(); err != nil {
		internalError(w, err)
		return
	}
	o.b = append(o.b, `],"articlesCount":`...)
	o.b = strconv.AppendInt(o.b, n, 10)
	o.b = append(o.b, '}')
	o.send(w, http.StatusOK)
}

// GET /api/articles
func (a *app) listArticles(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	limit, offset, ok := limitOffset(r)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "limit must be 1-100 and offset >= 0")
		return
	}
	q := r.URL.Query()
	// The first non-empty filter in the order tag, author, favorited applies.
	switch {
	case q.Get("tag") != "":
		a.writeList(w, r, listByTag, countByTag, viewer, limit, offset, q.Get("tag"))
	case q.Get("author") != "":
		a.writeList(w, r, listByAuthor, countByAuthor, viewer, limit, offset, q.Get("author"))
	case q.Get("favorited") != "":
		a.writeList(w, r, listByFavorite, countByFavorite, viewer, limit, offset, q.Get("favorited"))
	default:
		a.writeList(w, r, listAll, countAll, viewer, limit, offset, nil)
	}
}

// GET /api/articles/feed
func (a *app) feed(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	limit, offset, ok := limitOffset(r)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "limit must be 1-100 and offset >= 0")
		return
	}
	a.writeList(w, r, listFeed, countFeed, viewer, limit, offset, nil)
}

// GET /api/articles/{slug}
func (a *app) getArticle(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	a.writeFullArticle(w, r, http.StatusOK, articleBySlug, viewer, r.PathValue("slug"))
}

// POST /api/articles
func (a *app) createArticle(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	m, err := readWrapper(r, "article")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid body")
		return
	}
	title, err1 := reqString(m, "title")
	description, err2 := reqString(m, "description")
	body, err3 := reqString(m, "body")
	if err1 != nil || err2 != nil || err3 != nil {
		writeError(w, http.StatusUnprocessableEntity, "title, description and body are required")
		return
	}
	var tagList []string
	if raw, ok := m["tagList"]; ok && !isNull(raw) {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "tagList must be an array of non-empty strings")
			return
		}
		for _, it := range items {
			var s string
			if isNull(it) || json.Unmarshal(it, &s) != nil || s == "" {
				writeError(w, http.StatusUnprocessableEntity, "tagList must be an array of non-empty strings")
				return
			}
			tagList = append(tagList, s)
		}
	}

	ctx := r.Context()
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var id int64
	if err := tx.QueryRow(ctx, insertArticle, viewer, title, description, body).Scan(&id); err != nil {
		internalError(w, err)
		return
	}
	if len(tagList) > 0 {
		if _, err := tx.Exec(ctx, insertTags, tagList); err != nil {
			internalError(w, err)
			return
		}
		if _, err := tx.Exec(ctx, insertArticleTags, id, tagList); err != nil {
			internalError(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, err)
		return
	}
	a.writeFullArticle(w, r, http.StatusCreated, articleByID, viewer, id)
}

// ownArticle looks up the article by slug: 404 if missing, 403 if the viewer isn't its author.
func (a *app) ownArticle(w http.ResponseWriter, r *http.Request, viewer int64) (int64, bool) {
	var id, authorID int64
	err := a.pool.QueryRow(r.Context(), `SELECT id, author_id FROM articles WHERE slug = $1`, r.PathValue("slug")).Scan(&id, &authorID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "article not found")
		return 0, false
	}
	if err != nil {
		internalError(w, err)
		return 0, false
	}
	if authorID != viewer {
		writeError(w, http.StatusForbidden, "not the author")
		return 0, false
	}
	return id, true
}

// PUT /api/articles/{slug}
func (a *app) updateArticle(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	m, err := readWrapper(r, "article")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid body")
		return
	}
	title, err1 := optString(m, "title", true)
	description, err2 := optString(m, "description", true)
	body, err3 := optString(m, "body", true)
	if err1 != nil || err2 != nil || err3 != nil {
		writeError(w, http.StatusUnprocessableEntity, "title, description and body must be non-empty strings")
		return
	}
	id, ok := a.ownArticle(w, r, viewer)
	if !ok {
		return
	}
	if _, err := a.pool.Exec(r.Context(), updateArticle, id, title, description, body); err != nil {
		internalError(w, err)
		return
	}
	a.writeFullArticle(w, r, http.StatusOK, articleByID, viewer, id)
}

// DELETE /api/articles/{slug}
func (a *app) deleteArticle(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	id, ok := a.ownArticle(w, r, viewer)
	if !ok {
		return
	}
	if _, err := a.pool.Exec(r.Context(), `DELETE FROM articles WHERE id = $1`, id); err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// articleID: SELECT id FROM articles WHERE slug = $1 → 404.
func (a *app) articleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var id int64
	err := a.pool.QueryRow(r.Context(), `SELECT id FROM articles WHERE slug = $1`, r.PathValue("slug")).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "article not found")
		return 0, false
	}
	if err != nil {
		internalError(w, err)
		return 0, false
	}
	return id, true
}

// POST and DELETE /api/articles/{slug}/favorite
func (a *app) favorite(fav bool) authed {
	return func(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
		id, ok := a.articleID(w, r)
		if !ok {
			return
		}
		var err error
		if fav {
			_, err = a.pool.Exec(r.Context(), `INSERT INTO favorites (user_id, article_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, viewer, id)
		} else {
			_, err = a.pool.Exec(r.Context(), `DELETE FROM favorites WHERE user_id = $1 AND article_id = $2`, viewer, id)
		}
		if err != nil {
			internalError(w, err)
			return
		}
		a.writeFullArticle(w, r, http.StatusOK, articleByID, viewer, id)
	}
}
