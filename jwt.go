package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// A tiny dependency-free HS256 JWT, just enough for session cookies. We control
// both the issuer and the verifier, so there is no need for a general JWT
// library or algorithm negotiation — the header is fixed to HS256.

var jwtHeader = base64URL([]byte(`{"alg":"HS256","typ":"JWT"}`))

// sessionClaims is the payload carried in the session cookie.
type sessionClaims struct {
	Sub    string `json:"sub"`    // user ID
	Name   string `json:"name"`   // display name
	Avatar string `json:"avatar"` // avatar URL
	Exp    int64  `json:"exp"`    // unix expiry
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signToken(secret []byte, c sessionClaims) string {
	payload, _ := json.Marshal(c)
	signing := jwtHeader + "." + base64URL(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	return signing + "." + base64URL(mac.Sum(nil))
}

func verifyToken(secret []byte, token string) (*sessionClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	expected := base64URL(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[2])) != 1 {
		return nil, errors.New("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("bad payload encoding")
	}
	var c sessionClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("bad payload json")
	}
	if c.Exp != 0 && time.Now().Unix() > c.Exp {
		return nil, errors.New("token expired")
	}
	return &c, nil
}
