package server

import (
	"context"
	"net/url"
	"time"
)

type contextLike = context.Context

func ctxWith(ctx contextLike, key, val any) contextLike {
	return context.WithValue(ctx, key, val)
}

func contextWithTimeout(parent contextLike, d time.Duration) (contextLike, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

func queryEscape(s string) string { return url.QueryEscape(s) }
