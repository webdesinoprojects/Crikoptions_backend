package matches

import "context"

type realtimeCtxKey struct{}

// WithDeferredBallRealtime suppresses score/commentary WebSocket publishes from
// RecordBall and UpdateMatchScore until PublishBallDelivery is called.
func WithDeferredBallRealtime(ctx context.Context) context.Context {
	return context.WithValue(ctx, realtimeCtxKey{}, true)
}

func deferBallRealtime(ctx context.Context) bool {
	deferred, _ := ctx.Value(realtimeCtxKey{}).(bool)
	return deferred
}
