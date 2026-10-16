package main

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 10

type userJSON struct {
	Email, Token, Username string
	Bio, Image             *string
}

type profileJSON struct {
	Username   string
	Bio, Image *string
	Following  bool
}

func writeUser(w http.ResponseWriter, status int, u *userJSON) {
	o := newOut()
	o.b = append(o.b, `{"user":`...)
	o.b = appendUser(o.b, u)
	o.b = append(o.b, '}')
	o.send(w, status)
}

func writeProfile(w http.ResponseWriter, p *profileJSON) {
	o := newOut()
	o.b = append(o.b, `{"profile":`...)
	o.b = appendProfile(o.b, p)
	o.b = append(o.b, '}')
	o.send(w, http.StatusOK)
}

// hashPassword returns false for passwords bcrypt cannot hash (over 72 bytes).
func hashPassword(pw string) (string, bool) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", false
	}
	return string(h), true
}

// POST /api/users
func (a *app) register(w http.ResponseWriter, r *http.Request) {
	m, err := readWrapper(r, "user")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid body")
		return
	}
	username, err1 := reqString(m, "username")
	email, err2 := reqString(m, "email")
	password, err3 := reqString(m, "password")
	if err1 != nil || err2 != nil || err3 != nil {
		writeError(w, http.StatusUnprocessableEntity, "username, email and password are required")
		return
	}
	hash, ok := hashPassword(password)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "password is too long")
		return
	}
	var id int64
	var u userJSON
	err = a.pool.QueryRow(r.Context(), `INSERT INTO users (username, email, password_hash) VALUES ($1, $2, $3)
RETURNING id, username, email, bio, image`, username, email, hash).Scan(&id, &u.Username, &u.Email, &u.Bio, &u.Image)
	if isUniqueViolation(err) {
		writeError(w, http.StatusUnprocessableEntity, "username or email is already taken")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	u.Token = a.jwt.issue(id)
	writeUser(w, http.StatusCreated, &u)
}

// POST /api/users/login
func (a *app) login(w http.ResponseWriter, r *http.Request) {
	m, err := readWrapper(r, "user")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid body")
		return
	}
	email, err1 := reqString(m, "email")
	password, err2 := reqString(m, "password")
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusUnprocessableEntity, "email and password are required")
		return
	}
	var id int64
	var u userJSON
	var hash string
	err = a.pool.QueryRow(r.Context(), `SELECT id, username, email, bio, image, password_hash FROM users WHERE email = $1`,
		email).Scan(&id, &u.Username, &u.Email, &u.Bio, &u.Image, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	u.Token = a.jwt.issue(id)
	writeUser(w, http.StatusOK, &u)
}

// GET /api/user
func (a *app) currentUser(w http.ResponseWriter, r *http.Request, viewer int64, token string) {
	var id int64
	u := userJSON{Token: token}
	err := a.pool.QueryRow(r.Context(), `SELECT id, username, email, bio, image FROM users WHERE id = $1`,
		viewer).Scan(&id, &u.Username, &u.Email, &u.Bio, &u.Image)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	writeUser(w, http.StatusOK, &u)
}

// PUT /api/user
func (a *app) updateUser(w http.ResponseWriter, r *http.Request, viewer int64, token string) {
	m, err := readWrapper(r, "user")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid body")
		return
	}
	email, err1 := optString(m, "email", true)
	username, err2 := optString(m, "username", true)
	password, err3 := optString(m, "password", true)
	bio, err4 := optString(m, "bio", false)
	image, err5 := optString(m, "image", false)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid user fields")
		return
	}
	var hash *string
	if password != nil {
		h, ok := hashPassword(*password)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, "password is too long")
			return
		}
		hash = &h
	}
	var id int64
	u := userJSON{Token: token}
	err = a.pool.QueryRow(r.Context(), `UPDATE users SET email = COALESCE($2, email), username = COALESCE($3, username),
                 password_hash = COALESCE($4, password_hash), bio = COALESCE($5, bio), image = COALESCE($6, image)
 WHERE id = $1
RETURNING id, username, email, bio, image`, viewer, email, username, hash, bio, image).Scan(&id, &u.Username, &u.Email, &u.Bio, &u.Image)
	if isUniqueViolation(err) {
		writeError(w, http.StatusUnprocessableEntity, "username or email is already taken")
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	writeUser(w, http.StatusOK, &u)
}

// GET /api/profiles/{username}
func (a *app) getProfile(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
	var id int64
	var p profileJSON
	err := a.pool.QueryRow(r.Context(), `SELECT u.id, u.username, u.bio, u.image,
       EXISTS (SELECT 1 FROM follows fo WHERE fo.follower_id = $1 AND fo.followee_id = u.id) AS following
  FROM users u WHERE u.username = $2`, viewer, r.PathValue("username")).Scan(&id, &p.Username, &p.Bio, &p.Image, &p.Following)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "profile not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	writeProfile(w, &p)
}

// POST and DELETE /api/profiles/{username}/follow
func (a *app) follow(follow bool) authed {
	return func(w http.ResponseWriter, r *http.Request, viewer int64, _ string) {
		var id int64
		var p profileJSON
		err := a.pool.QueryRow(r.Context(), `SELECT id, username, bio, image FROM users WHERE username = $1`,
			r.PathValue("username")).Scan(&id, &p.Username, &p.Bio, &p.Image)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "profile not found")
			return
		}
		if err != nil {
			internalError(w, err)
			return
		}
		if follow {
			_, err = a.pool.Exec(r.Context(), `INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, viewer, id)
		} else {
			_, err = a.pool.Exec(r.Context(), `DELETE FROM follows WHERE follower_id = $1 AND followee_id = $2`, viewer, id)
		}
		if err != nil {
			internalError(w, err)
			return
		}
		p.Following = follow
		writeProfile(w, &p)
	}
}

// GET /api/tags: each name is copied from the row's raw bytes into the response.
func (a *app) tags(w http.ResponseWriter, r *http.Request) {
	rows, _ := a.pool.Query(r.Context(), `SELECT name FROM tags ORDER BY name`)
	defer rows.Close()
	o := newOut()
	o.b = append(o.b, `{"tags":[`...)
	for n := 0; rows.Next(); n++ {
		if n > 0 {
			o.b = append(o.b, ',')
		}
		o.b = appendString(o.b, rows.RawValues()[0])
	}
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}
	o.b = append(o.b, "]}"...)
	o.send(w, http.StatusOK)
}
