package main

import (
	"context"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/minio/minio/auth"
	"go.uber.org/zap"
)

type ContextKey string

const BearerTokenContextKey ContextKey = "bearer-token"

func attachNewUserAuth(router *mux.Router, center *auth.Center, log *zap.Logger) {
	uamw := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bearerToken, err := center.AuthenticationPassed(r)
			if err != nil {
				log.Error("failed to pass authentication", zap.Error(err))
				// TODO: Handle any auth error by rejecting request.
			}
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), BearerTokenContextKey, bearerToken)))

		})
	}
	router.Use(uamw)
}