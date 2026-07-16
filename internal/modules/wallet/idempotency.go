package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type operationKeyContextKey struct{}

// WithOperationKey scopes one wallet mutation to a stable, caller-owned key.
// Reusing the key with identical inputs returns the original ledger result;
// reusing it with different inputs fails closed.
func WithOperationKey(ctx context.Context, key string) context.Context {
	key = normalizeOperationKey(key)
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, operationKeyContextKey{}, key)
}

// OperationKey returns the wallet operation key carried by ctx.
func OperationKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(operationKeyContextKey{}).(string)
	return normalizeOperationKey(key)
}

func normalizeOperationKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 512 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}
