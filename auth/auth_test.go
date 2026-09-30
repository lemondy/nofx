package auth

import (
	"github.com/golang-jwt/jwt/v5"
	"testing"
	"time"
)

func TestJWTUniqueVersionIssuerAndExpiry(t *testing.T) {
	JWTSecret = []byte("unit-test-secret-at-least-32-characters")
	first, err := GenerateJWTWithVersion("u", "u@test.com", 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateJWTWithVersion("u", "u@test.com", 3)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("rapid logout/login can return the same revoked token")
	}
	claims, err := ValidateJWT(first)
	if err != nil || claims.TokenVersion != 3 || claims.ID == "" {
		t.Fatalf("%+v %v", claims, err)
	}
	for _, bad := range []Claims{
		{UserID: "u", RegisteredClaims: jwt.RegisteredClaims{Issuer: "foreign", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}},
		{UserID: "u", RegisteredClaims: jwt.RegisteredClaims{Issuer: "nofxAI"}},
		{UserID: "u", RegisteredClaims: jwt.RegisteredClaims{Issuer: "nofxAI", ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour))}},
	} {
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, bad).SignedString(JWTSecret)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateJWT(token); err == nil {
			t.Fatal("invalid issuer/expiry accepted")
		}
	}
}
