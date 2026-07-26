package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// googleCertsURL serves Google's public RSA keys (JWKS) used to sign ID tokens.
const googleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

var (
	errGoogleNotConfigured = errors.New("google sign-in is not configured")
	errGoogleInvalidToken  = errors.New("invalid Google credential")
)

// googleClaims is the subset of the Google ID token payload we rely on.
type googleClaims struct {
	Iss           string `json:"iss"`
	Aud           string `json:"aud"`
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Exp           int64  `json:"exp"`
}

// googleVerifier validates Google ID tokens (the "credential" returned by
// Google Identity Services) against Google's rotating public keys. Keys are
// cached in memory and refreshed on demand when an unknown key id is seen.
type googleVerifier struct {
	clientID string
	client   *http.Client

	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func newGoogleVerifier(clientID string) *googleVerifier {
	return &googleVerifier{
		clientID: strings.TrimSpace(clientID),
		client:   &http.Client{Timeout: 10 * time.Second},
		keys:     make(map[string]*rsa.PublicKey),
	}
}

// Verify checks the signature and standard claims of a Google ID token and
// returns the validated claims on success.
func (v *googleVerifier) Verify(ctx context.Context, idToken string) (googleClaims, error) {
	if v == nil || v.clientID == "" {
		return googleClaims{}, errGoogleNotConfigured
	}

	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) != 3 {
		return googleClaims{}, errGoogleInvalidToken
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil {
		return googleClaims{}, errGoogleInvalidToken
	}
	if header.Alg != "RS256" || header.Kid == "" {
		return googleClaims{}, errGoogleInvalidToken
	}

	key, err := v.keyByID(ctx, header.Kid)
	if err != nil {
		return googleClaims{}, err
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return googleClaims{}, errGoogleInvalidToken
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return googleClaims{}, errGoogleInvalidToken
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return googleClaims{}, errGoogleInvalidToken
	}
	var claims googleClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return googleClaims{}, errGoogleInvalidToken
	}

	if claims.Iss != "accounts.google.com" && claims.Iss != "https://accounts.google.com" {
		return googleClaims{}, errGoogleInvalidToken
	}
	if claims.Aud != v.clientID {
		return googleClaims{}, errGoogleInvalidToken
	}
	if claims.Exp <= 0 || time.Now().UTC().Unix() > claims.Exp {
		return googleClaims{}, errGoogleInvalidToken
	}
	if strings.TrimSpace(claims.Email) == "" || !claims.EmailVerified {
		return googleClaims{}, errGoogleInvalidToken
	}

	return claims, nil
}

// keyByID returns the cached RSA key for kid, refreshing the JWKS cache once if
// the key is unknown (Google rotates keys periodically).
func (v *googleVerifier) keyByID(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key := v.keys[kid]
	v.mu.RUnlock()
	if key != nil {
		return key, nil
	}

	if err := v.refresh(ctx); err != nil {
		return nil, err
	}

	v.mu.RLock()
	key = v.keys[kid]
	v.mu.RUnlock()
	if key == nil {
		return nil, errGoogleInvalidToken
	}
	return key, nil
}

func (v *googleVerifier) refresh(ctx context.Context) error {
	// Coalesce bursts: skip if another goroutine just refreshed.
	v.mu.RLock()
	recent := time.Since(v.fetched) < time.Minute && len(v.keys) > 0
	v.mu.RUnlock()
	if recent {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleCertsURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errGoogleInvalidToken
	}

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return err
	}

	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		pub, err := parseRSAPublicKey(k.N, k.E)
		if err != nil || k.Kid == "" {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errGoogleInvalidToken
	}

	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.mu.Unlock()
	return nil
}

// parseRSAPublicKey builds an rsa.PublicKey from the base64url-encoded modulus
// (n) and exponent (e) fields of a JWK.
func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, err
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, errGoogleInvalidToken
	}

	// Left-pad the exponent to 8 bytes so it fits a uint64.
	ePadded := make([]byte, 8)
	copy(ePadded[8-len(eBytes):], eBytes)

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(binary.BigEndian.Uint64(ePadded)),
	}, nil
}
