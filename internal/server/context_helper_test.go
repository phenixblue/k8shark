package server

import "context"

func cancelableContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}
