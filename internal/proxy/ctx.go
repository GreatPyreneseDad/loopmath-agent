package proxy

import "context"

func withExchange(ctx context.Context, x *exchange) context.Context {
	return context.WithValue(ctx, ctxKey{}, x)
}
